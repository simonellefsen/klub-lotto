package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/config"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
	"github.com/simonellefsen/klub-lotto/internal/llm"
	"github.com/simonellefsen/klub-lotto/internal/store"
	"github.com/simonellefsen/klub-lotto/internal/wiki"
)

// ordKloeverExtractTimeout bounds a single Ordkløver state extraction (browser
// crop + vision board read). It must be generous enough for a slow reasoning
// vision model: a 45s budget produced "openrouter-vision: read response:
// context deadline exceeded" on the ~google/gemini-pro-latest board read
// mid-game (our ctx expiring during the response read, not the HTTP client,
// which is already 540s). The on-error fallback gets its own extra budget on a
// detached context.
const ordKloeverExtractTimeout = 120 * time.Second

func gameBrowser(cfg *config.Config, headlessFlag bool) *browser.Client {
	headless := headlessFlag
	if v := os.Getenv("KLUBLOTTO_HEADED"); v != "" {
		headless = strings.EqualFold(v, "false")
	}
	return browser.New(cfg.BrowserSessionName, !headless)
}

func openGameWithLogin(ctx context.Context, br *browser.Client, cfg *config.Config, open func(context.Context, *browser.Client) error) error {
	if err := open(ctx, br); err != nil {
		return err
	}
	curURL, _ := br.URL(ctx)
	if !klublotto.IsLoginFlowURL(curURL) {
		// Session reuse: already on game page (e.g. from prior MitID login job in same headed "klublotto" session).
		// No extra login flows per design (openGameWithLogin early-returns; web UI VNC shows the live one).
		if os.Getenv("KLUBLOTTO_DEBUG") != "" {
			fmt.Println("       (reusing existing logged-in session; no login redirect)")
		}
		return nil
	}
	if cfg.DanskespilUsername == "" || cfg.DanskespilPassword == "" {
		return fmt.Errorf("login required before game can run (landed at %s; no configured Rød Konto username/password)", curURL)
	}
	fmt.Println("       login redirect detected; trying automatic Rød Konto login...")
	ok, needsMitID, err := tryAutomaticRedKontoLogin(ctx, br, cfg.DanskespilUsername, cfg.DanskespilPassword)
	if err != nil {
		return fmt.Errorf("automatic Rød Konto login before game: %w", err)
	}
	if needsMitID {
		return fmt.Errorf("MitID interaction required before game can run (landed at %s)", curURL)
	}
	if !ok {
		return fmt.Errorf("automatic Rød Konto login before game did not complete")
	}
	if err := open(ctx, br); err != nil {
		return fmt.Errorf("reopen game after login: %w", err)
	}
	return nil
}

func wordCandidates(ctx context.Context, cfg *config.Config, providerName, prompt string) ([]klublotto.WordCandidate, error) {
	p, err := wordProvider(cfg, providerName)
	if err != nil {
		return nil, err
	}
	fmt.Printf("   [llm] provider: %s  prompt (%d chars):\n", p.Name(), len(prompt))
	for _, line := range strings.Split(prompt, "\n") {
		fmt.Printf("      | %s\n", line)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// Brief pause before retry; don't retry if the parent context is done.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
			fmt.Printf("   [llm retry %d/3] previous attempt failed (%v), retrying...\n", attempt+1, lastErr)
		}
		modelCtx, cancel := context.WithTimeout(ctx, 540*time.Second)
		raw, callErr := p.GenerateJSON(modelCtx, prompt, 0.2)
		cancel()
		if callErr != nil {
			lastErr = callErr
			if ctx.Err() != nil {
				return nil, ctx.Err() // parent canceled (Ctrl-C), stop immediately
			}
			continue // transient error — retry
		}
		cands, parseErr := klublotto.ParseCandidateJSON(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s candidates: %w (raw=%s)", p.Name(), parseErr, raw)
		}
		return cands, nil
	}
	return nil, fmt.Errorf("all 3 LLM attempts failed: %w", lastErr)
}

// wordCandidatesRawJSON resolves the word provider and returns the raw JSON
// response (with retry), for callers that parse a custom shape — e.g. the
// batched krydsord candidate request keyed by slot id.
func wordCandidatesRawJSON(ctx context.Context, cfg *config.Config, providerName, prompt string) (string, error) {
	p, err := wordProvider(cfg, providerName)
	if err != nil {
		return "", err
	}
	fmt.Printf("   [llm] provider: %s  batch prompt (%d chars, %d clues)\n", p.Name(), len(prompt), strings.Count(prompt, "\n- id="))
	var lastErr error
	// Candidate lists are short; a slow/stuck model shouldn't hold the whole solve
	// hostage. Cap each attempt at 150s (×2) so we fail fast and let the assembler
	// proceed from clue texts + dictionary patterns rather than hanging for minutes.
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(3 * time.Second):
			}
			fmt.Printf("   [llm retry %d/2] previous attempt failed (%v), retrying...\n", attempt+1, lastErr)
		}
		modelCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
		raw, callErr := p.GenerateJSON(modelCtx, prompt, 0.2)
		cancel()
		if callErr != nil {
			lastErr = callErr
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			continue
		}
		return raw, nil
	}
	return "", fmt.Errorf("all LLM attempts failed: %w", lastErr)
}

// wordModelLabel returns a stable identifier for the word/JSON model that
// wordProvider would resolve for `override` (falling back to the configured
// default). It is recorded in the daily ledger so we can see which model solved
// or failed each word puzzle. On a resolution error it returns the raw override
// (or configured default) so the ledger still shows what was attempted.
func wordModelLabel(cfg *config.Config, override string) string {
	if p, err := wordProvider(cfg, override); err == nil {
		if s := strings.TrimSpace(p.Name()); s != "" {
			return s
		}
	}
	name := strings.TrimSpace(override)
	if name == "" {
		name = strings.TrimSpace(cfg.WordProvider)
	}
	if name == "" {
		return "unknown"
	}
	return name
}

// appendModelNote appends a "Word model: `…`." sentence to a ledger Notes cell,
// recording which model solved (or failed) the puzzle. It is a no-op for an
// empty label so already-solved / explicit-answer rows (no model used) stay clean.
func appendModelNote(notes, label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return notes
	}
	suffix := "Word model: `" + label + "`."
	if strings.TrimSpace(notes) == "" {
		return suffix
	}
	return strings.TrimRight(notes, " ") + " " + suffix
}

// wordProvider resolves the configured/overridden word-model name to a provider.
// The routing itself lives in (and is tested in) internal/llm; this wrapper just
// supplies the default from config and maps config fields to llm.Keys.
func wordProvider(cfg *config.Config, override string) (llm.JSONGenerator, error) {
	name := strings.TrimSpace(override)
	if name == "" {
		name = strings.TrimSpace(cfg.WordProvider)
	}
	return llm.Resolve(name, providerKeys(cfg))
}

// providerKeys maps the config's API keys + default models into llm.Keys.
func providerKeys(cfg *config.Config) llm.Keys {
	return llm.Keys{
		Gemini:          cfg.GeminiKey,
		OpenAI:          cfg.OpenAIKey,
		OpenAIModel:     cfg.OpenAIModel,
		XAI:             cfg.XAIKey,
		Anthropic:       cfg.AnthropicKey,
		OpenRouter:      cfg.OpenRouterKey,
		OpenRouterModel: cfg.OpenRouterModel,
		ZAI:             cfg.ZAIKey,
		ZAIModel:        cfg.ZAIModel,
	}
}

// ordKloeverReasoningAttempts is the attempts-used threshold (out of 12) at
// which the Ordkløver loop switches from the fast word model to the heavier
// reasoning model. Below it we favour speed; at/after it we favour accuracy
// because every remaining guess is precious.
const ordKloeverReasoningAttempts = 7

type pagePoint struct {
	OK    bool   `json:"ok"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	Error string `json:"error"`
}

func evalPoint(ctx context.Context, br *browser.Client, js string) (pagePoint, error) {
	raw, err := br.Eval(ctx, js)
	if err != nil {
		return pagePoint{}, err
	}
	var pt pagePoint
	if err := json.Unmarshal([]byte(raw), &pt); err != nil {
		return pagePoint{}, fmt.Errorf("parse point: %w (raw=%s)", err, raw)
	}
	if !pt.OK {
		if pt.Error == "" {
			pt.Error = "point lookup failed"
		}
		return pagePoint{}, fmt.Errorf("%s", pt.Error)
	}
	return pt, nil
}

func clickInteractiveByName(ctx context.Context, br *browser.Client, names ...string) error {
	var last string
	for attempt := 0; attempt < 4; attempt++ {
		snap, err := br.SnapshotInteractive(ctx)
		if err != nil {
			return err
		}
		last = snap
		if ref := klublotto.FindRefByName(snap, names); ref != "" {
			return br.Click(ctx, ref)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("could not find interactive element named %s in snapshot: %s", strings.Join(names, ", "), last)
}

func isImmerspieleURL(u string) bool {
	lu := strings.ToLower(u)
	return strings.Contains(lu, "immerspiele") || strings.Contains(lu, "klub-lotto.immerspiele.com")
}

func dispatchKey(ctx context.Context, br *browser.Client, k string) {
	_, _ = br.Eval(ctx, fmt.Sprintf(`(() => {
	  const k = %q;
	  const opts = {key: k, bubbles:true, cancelable:true};
	  try {
	    document.dispatchEvent(new KeyboardEvent('keydown', opts));
	    document.dispatchEvent(new KeyboardEvent('keypress', opts));
	    document.dispatchEvent(new KeyboardEvent('keyup', opts));
	  } catch(e){}
	  const ifr = document.querySelector('iframe[src*="ordknuden"], iframe[src*="ordknude"], .kl-game__iframe');
	  if (ifr && ifr.contentWindow) {
	    try {
	      const w = ifr.contentWindow;
	      w.dispatchEvent(new KeyboardEvent('keydown', opts));
	      w.dispatchEvent(new KeyboardEvent('keypress', opts));
	      w.dispatchEvent(new KeyboardEvent('keyup', opts));
	    } catch(e){}
	  }
	  return '';
	})()`, k))
}

func startGameIfNeeded(ctx context.Context, br *browser.Client, names ...string) error {
	// First try parent-context snapshot (works for same-origin iframes like Ordknude).
	snap, err := br.SnapshotInteractiveWithFrames(ctx)
	if err != nil {
		snap, err = br.SnapshotInteractive(ctx)
		if err != nil {
			return err
		}
	}
	if ref := klublotto.FindRefByName(snap, names); ref != "" {
		if err := br.Click(ctx, ref); err != nil {
			return err
		}
		time.Sleep(1200 * time.Millisecond)
		return nil
	}

	// Fallback: try frame-based access (required for cross-origin iframes such as
	// the Ordkløver/Immer Spiele embed where parent snapshots never expose buttons).
	if ferr := klublotto.EnterGameFrame(ctx, br); ferr == nil {
		defer klublotto.LeaveFrame(br)
		isnap, _ := br.SnapshotInteractive(ctx)
		if ref := klublotto.FindRefByName(isnap, names); ref != "" {
			_ = br.Click(ctx, ref)
			time.Sleep(1200 * time.Millisecond)
		}
	} else {
		klublotto.LeaveFrame(br)
	}
	return nil
}

func dismissHowTo(ctx context.Context, br *browser.Client) error {
	snap, err := br.SnapshotInteractive(ctx)
	if err != nil {
		return err
	}
	if ref := klublotto.FindRefByName(snap, []string{"Luk", "Close"}); ref != "" {
		_ = br.Click(ctx, ref)
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func clickFirst(ctx context.Context, br *browser.Client, selectors ...string) error {
	var last error
	for _, sel := range selectors {
		if strings.TrimSpace(sel) == "" {
			continue
		}
		if err := br.Click(ctx, sel); err == nil {
			return nil
		} else {
			last = err
		}
	}
	if last == nil {
		last = fmt.Errorf("no selectors supplied")
	}
	return last
}

func fillFirst(ctx context.Context, br *browser.Client, value string, selectors ...string) error {
	var last error
	for _, sel := range selectors {
		if err := br.Fill(ctx, sel, value); err == nil {
			return nil
		} else {
			last = err
		}
	}
	if last == nil {
		last = fmt.Errorf("no selectors supplied")
	}
	return last
}

func printCandidates(cands []klublotto.WordCandidate) {
	fmt.Println()
	fmt.Println("== Candidates ==")
	for i, c := range cands {
		fmt.Printf("%d. %s (%s) — %s\n", i+1, c.Answer, c.Confidence, c.Rationale)
	}
}

func containsWord(words []string, want string) bool {
	for _, w := range words {
		if w == want {
			return true
		}
	}
	return false
}

func upsertDailyGame(ctx context.Context, cfg *config.Config, game, prompt, answer string, submitted, registered bool, notes string) error {
	loc, err := time.LoadLocation("Europe/Copenhagen")
	if err != nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	wikiDir := wikiRoot()
	if err := os.MkdirAll(filepath.Join(wikiDir, "daily"), 0o755); err != nil {
		return err
	}
	path := filepath.Join(wikiDir, "daily", now.Format("2006-01-02")+".md")
	body := ""
	if raw, err := os.ReadFile(path); err == nil {
		body = string(raw)
	}
	row := fmt.Sprintf("| %s | %s | %s | %s | %s | %s |\n",
		mdCell(game), mdCell(prompt), mdCell(answer), yesNo(submitted), yesNo(registered), mdCell(notes))
	if body == "" || !strings.Contains(body, "| Game |") {
		body = fmt.Sprintf("---\nkind: daily-ledger\ndate: %s\ntags: [klublotto, daily-ledger, answers]\nupdated: %s\n---\n\n# Klub Lotto Daily Ledger — %s\n\n## Answers\n\n| Game | Prompt / clue | Answer | Submitted through parent page | Registered on overview | Notes |\n|---|---|---|---:|---:|---|\n%s",
			now.Format("2006-01-02"), now.UTC().Format(time.RFC3339), now.Format("2006-01-02"), row)
	} else {
		lines := strings.Split(body, "\n")
		replaced := false
		prefix := "| " + game + " |"
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), prefix) {
				lines[i] = strings.TrimRight(row, "\n")
				replaced = true
			}
		}
		body = strings.Join(lines, "\n")
		if !replaced {
			lines = strings.Split(body, "\n")
			for i, line := range lines {
				if strings.HasPrefix(strings.TrimSpace(line), "|---") {
					lines = append(lines[:i+1], append([]string{strings.TrimRight(row, "\n")}, lines[i+1:]...)...)
					break
				}
			}
			body = strings.Join(lines, "\n")
		}
		body = regexpReplace(body, `(?m)^updated:\s*.*$`, "updated: "+now.UTC().Format(time.RFC3339))
	}
	_ = cfg
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	// Best-effort append to log.md so that scripts/sync.sh (run by `make sudoku`,
	// `make krydsord` etc.) will use a fresh entry for the commit subject
	// instead of a stale one from a previous day's quiz/ingest.
	kind := strings.ToLower(game)
	_ = wiki.AppendIngestLog(wikiDir, now.UTC(), kind, prompt, "submitted")

	// Also write to Postgres daily_ledger when DATABASE_URL is configured.
	// Best-effort: never fail the overall command if postgres is unavailable.
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		if pg, pgErr := store.New(ctx, dsn); pgErr == nil {
			defer pg.Close()
			slug := dailyGameSlug(game)
			entry := store.LedgerEntry{
				Date:       now,
				GameSlug:   slug,
				Prompt:     prompt,
				Answer:     answer,
				Submitted:  submitted,
				Registered: registered,
				Notes:      notes,
				SourcePath: "wiki/daily/" + now.Format("2006-01-02") + ".md",
				PageURL:    dailyGamePageURL(slug),
			}
			if _, pgErr2 := pg.UpsertLedger(ctx, entry, nil); pgErr2 != nil {
				fmt.Printf("   [postgres] UpsertLedger %s: %v\n", game, pgErr2)
			} else {
				fmt.Printf("   [postgres] UpsertLedger %s OK\n", game)
			}
		}
	}
	return nil
}

// dailyGameSlug returns the postgres game_slug for a given game display name.
func dailyGameSlug(game string) string {
	switch strings.ToLower(strings.TrimSpace(game)) {
	case "ordknuden", "ordknude":
		return "ordknuden"
	case "ordkløver", "ordkloever", "ordklover":
		return "ordkloever"
	case "krydsord":
		return "krydsord"
	case "quiz":
		return "quiz"
	case "sudoku":
		return "sudoku"
	default:
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(game), " ", "-"))
	}
}

// dailyGamePageURL returns the parent page URL for a given game_slug.
func dailyGamePageURL(slug string) string {
	switch slug {
	case "ordknuden":
		return klublotto.OrdknudeURL
	case "ordkloever":
		return klublotto.OrdKloeverURL
	case "krydsord":
		return klublotto.KrydsordURL
	default:
		return ""
	}
}

// --- Krydsord submit and solve helpers (modeled exactly on sudoku/ord* patterns) ---
