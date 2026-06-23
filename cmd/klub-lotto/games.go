package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/config"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
	"github.com/simonellefsen/klub-lotto/internal/llm"
	"github.com/simonellefsen/klub-lotto/internal/store"
	"github.com/simonellefsen/klub-lotto/internal/wiki"
)

func runOrdKloever(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ordkloever", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "extract and ask for candidates, but do not submit (real play with --answer commits guesses permanently; no do-overs)")
	extractOnly := fs.Bool("extract-only", false, "extract state only; do not call a provider or submit")
	submitFlag := fs.Bool("submit", false, "submit the explicit --answer through the parent page (real play; guesses are permanent with no do-overs)")
	answerFlag := fs.String("answer", "", "exact answer to submit")
	providerFlag := fs.String("provider", "", "word provider: gemini|openai|xai|anthropic|openrouter")
	finalProviderFlag := fs.String("final-provider", "", "word provider for the last-attempt guess (11/12); falls back to ORDKLOEVER_FINAL_PROVIDER or --provider")
	probeLetters := fs.Bool("probe-letters", false, "spend letter guesses to reveal Ordkløver board state")
	letterRounds := fs.Int("letter-rounds", 3, "maximum Ordkløver letter-probing rounds")
	autoAnswer := fs.Bool("auto-answer", false, "allow submitting a high-confidence provider answer after probing")
	headlessFlag := fs.Bool("headless", false, "force headless browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// Resolve the three model identities up front so we can fail fast on a bad
	// slug before opening a browser and burning attempts.
	//   word  = --provider | WORD_PROVIDER       (loop word/decision LLM)
	//   final = --final-provider | env | word     (last-attempt crunch LLM)
	//   vision = OPENROUTER_VISION_MODEL          (board reader, OpenRouter only)
	wordModel := strings.TrimSpace(*providerFlag)
	if wordModel == "" {
		wordModel = strings.TrimSpace(cfg.WordProvider)
	}
	finalModel := strings.TrimSpace(*finalProviderFlag)
	if finalModel == "" {
		finalModel = strings.TrimSpace(cfg.OrdKloeverFinalProvider)
	}
	if finalModel == "" {
		finalModel = wordModel
	}
	// Preflight: validate every model that will hit OpenRouter (slugs containing
	// "/", plus the OpenRouter vision model). A leading "~" or typo otherwise only
	// surfaces as an http 400 on every call mid-game.
	type modelCheck struct{ label, slug string }
	var checks []modelCheck
	if cfg.OpenRouterKey != "" && cfg.OpenRouterVisionModel != "" {
		checks = append(checks, modelCheck{"vision", cfg.OpenRouterVisionModel})
	}
	if strings.Contains(wordModel, "/") {
		checks = append(checks, modelCheck{"word", wordModel})
	}
	if strings.Contains(finalModel, "/") && finalModel != wordModel {
		checks = append(checks, modelCheck{"final", finalModel})
	}
	if len(checks) > 0 {
		fmt.Println("[preflight] validating OpenRouter model IDs...")
		vctx, vcancel := context.WithTimeout(ctx, 25*time.Second)
		for _, c := range checks {
			if verr := llm.ValidateOpenRouterModel(vctx, cfg.OpenRouterKey, c.slug); verr != nil {
				vcancel()
				return fmt.Errorf("[preflight] %s model invalid: %w", c.label, verr)
			}
			fmt.Printf("   [preflight] %s model OK: %s\n", c.label, llm.SanitizeModelSlug(c.slug))
		}
		vcancel()
	}

	// Vision provider priority:
	//   1. OPENROUTER_VISION_MODEL + OPENROUTER_API_KEY → primary (OpenRouter model)
	//   2. GEMINI_API_KEY only → primary Gemini 2.5 Pro
	//   3. ANTHROPIC_API_KEY only → primary Anthropic
	// Secondary cross-check is disabled — it failed too often and added latency.
	var ac llm.VisionProvider
	switch {
	case cfg.OpenRouterKey != "" && cfg.OpenRouterVisionModel != "":
		ac = llm.NewOpenRouterVision(cfg.OpenRouterKey, cfg.OpenRouterVisionModel)
		fmt.Printf("   [vision] primary: openrouter:%s\n", cfg.OpenRouterVisionModel)
	case cfg.GeminiKey != "":
		ac = llm.NewGemini(cfg.GeminiKey, "gemini-2.5-pro")
		fmt.Println("   [vision] primary: gemini:gemini-2.5-pro")
	case cfg.AnthropicKey != "":
		ac = llm.NewAnthropic(cfg.AnthropicKey, "claude-haiku-4-5-20251001")
		fmt.Println("   [vision] primary: anthropic:claude-haiku-4-5-20251001")
	}
	// On-error fallback (used only if the primary read fails), preferring a
	// direct Gemini 2.5 Pro that sidesteps the flaky OpenRouter alias.
	visionFallback := ordKloeverFallbackVision(cfg, ac)
	if visionFallback != nil {
		if n, ok := visionFallback.(interface{ Name() string }); ok {
			fmt.Printf("   [vision] fallback: %s\n", n.Name())
		}
	}
	br := gameBrowser(cfg, *headlessFlag)
	restartHeadedSession(ctx, br)

	fmt.Println("[1/4] opening Dagens Ordkløver...")
	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	err = openGameWithLogin(openCtx, br, cfg, klublotto.OpenOrdKloever)
	curURL, _ := br.URL(openCtx)
	cancel()
	if err != nil {
		return err
	}
	fmt.Println("       at:", curURL)

	fmt.Println("[2/4] extracting state...")
	extractCtx, cancel := context.WithTimeout(ctx, ordKloeverExtractTimeout)
	st, err := klublotto.ExtractOrdKloeverState(extractCtx, br, ac, visionFallback)
	cancel()
	if err != nil {
		return err
	}
	_ = saveDebug(cfg.DataDir, "ordkloever-state.txt", st.Raw)
	// Prefer latest vision-raw json for populating st fields (reliable source of CATEGORY/SHAPE/BOARD even if body/Raw is menu or launcher text).
	{
		gp := filepath.Join(cfg.DataDir, "ordkloever-vision-raw-*.txt")
		if matches, _ := filepath.Glob(gp); len(matches) > 0 {
			latest := matches[len(matches)-1]
			for _, m := range matches {
				if fi1, _ := os.Stat(m); fi1 != nil {
					if fi2, _ := os.Stat(latest); fi2 != nil && fi1.ModTime().After(fi2.ModTime()) {
						latest = m
					}
				}
			}
			if b, err := os.ReadFile(latest); err == nil {
				s := string(b)
				if strings.Contains(s, `"CATEGORY"`) || strings.Contains(s, `"SHAPE"`) || strings.Contains(s, `"BOARD"`) {
					var j map[string]interface{}
					if json.Unmarshal(b, &j) == nil {
						// strip ```json fences or extract inner {..}
						clean := string(b)
						if i := strings.Index(clean, "```"); i >= 0 {
							if j := strings.Index(clean[i+3:], "```"); j >= 0 {
								clean = clean[i+3 : i+3+j]
							}
						}
						clean = strings.TrimSpace(clean)
						if si := strings.IndexByte(clean, '{'); si >= 0 {
							if ei := strings.LastIndexByte(clean, '}'); ei > si {
								clean = clean[si : ei+1]
							}
						}
						var jj map[string]interface{}
						if json.Unmarshal([]byte(clean), &jj) == nil {
							j = jj
							if v, ok := j["CATEGORY"].(string); ok && v != "" && !strings.EqualFold(v, "Not visible") {
								st.Category = v
							}
							if v, ok := j["HINT"].(string); ok && v != "" && !strings.EqualFold(v, "Not visible") {
								st.Hint = v
							}
							if v, ok := j["SHAPE"].(string); ok && v != "" && !strings.EqualFold(v, "Unknown") {
								st.Shape = v
								if st.VisualShape == "" {
									st.VisualShape = v
								}
							}
							if v, ok := j["BOARD"].(string); ok && v != "" {
								st.Board = v
								if st.VisualBoard == "" {
									st.VisualBoard = v
								}
							}
							if v, ok := j["GUESSED"].(string); ok && v != "" {
								st.GuessedLetters = klublotto.CleanGuessedLetters(v)
							}
							if v, ok := j["ATTEMPTS"].(string); ok && st.Attempts == 0 {
								if idx := strings.Index(v, "/"); idx > 0 {
									if n, _ := strconv.Atoi(strings.TrimSpace(v[:idx])); n > 0 {
										st.Attempts = n
									}
								}
							}
						} else {
							fmt.Printf("   [dbg json] unmarshal failed for %s even after strip\n", latest)
						}
					}
				}
			}
		}
	}
	st.GuessedLetters = klublotto.CleanGuessedLetters(st.GuessedLetters)
	fmt.Printf("Category: %s\nHint: %s\nAnswer pattern: %s\n", st.Category, st.Hint, st.Shape)
	if st.VisualShape != "" && st.VisualShape != st.Shape {
		fmt.Printf("Visual layout: %s\n", st.VisualShape)
	}
	fmt.Printf("Board: %s\n", st.Board)
	if st.VisualBoard != "" && st.VisualBoard != st.Board {
		fmt.Printf("Visual board: %s\n", st.VisualBoard)
	}
	g := st.GuessedLetters
	if g == "" {
		g = "(none)"
	}
	fmt.Printf("Guessed letters: %s\nAttempts: %d/12\n", g, st.Attempts)
	if *extractOnly {
		fmt.Println("[3/4] extract only — not asking provider.")
		fmt.Println("[4/4] not submitting (extract-only).")
		return nil
	}

	// Graceful handling for already-besvaret / finished (like Ordknuden).
	lowRaw := strings.ToLower(st.Raw)
	if st.Attempts >= 12 || st.Solved || strings.Contains(lowRaw, "besvaret") || strings.Contains(lowRaw, "allerede besvaret") || strings.Contains(lowRaw, "du har allerede besvaret") {
		shot := filepath.Join(cfg.DataDir, "ordkloever-result-"+time.Now().UTC().Format("20060102-150405")+".png")
		_ = br.Screenshot(ctx, shot)
		return upsertDailyGame(ctx, cfg, "Ordkløver", klublotto.OrdKloeverPrompt(st), "SOLVED", true, true, "Already besvaret / finished on page. Screenshot: `"+shot+"`.")
	}

	// Final-attempt provider (11/12 mode) — resolved + validated up front.
	finalProvider := finalModel

	answer := klublotto.NormalizeDanishPhrase(*answerFlag)
	if st.Solved {
		if answer == "" {
			answer = "SOLVED"
		}
		shot := filepath.Join(cfg.DataDir, "ordkloever-result-"+time.Now().UTC().Format("20060102-150405")+".png")
		_ = br.Screenshot(ctx, shot)
		return upsertDailyGame(ctx, cfg, "Ordkløver", klublotto.OrdKloeverPrompt(st), answer, true, true, "Already solved on page. Screenshot: `"+shot+"`.")
	}
	if answer == "" {
		if *probeLetters {
			// Auto-play path: skip initial candidate generation, go straight to the probe loop.
			// The probe loop calls askOrdKloeverDecision each round (combined "guess or probe" decision).
			fmt.Println("[3/4] entering probe loop (auto-play — skipping initial candidate step)...")
			return runOrdKloeverProbe(ctx, cfg, br, st, ac, *providerFlag, finalProvider, nil, *dryRun || !*submitFlag, *letterRounds, *autoAnswer)
		}
		// Dry-run / listing path: ask for candidates and print them.
		fmt.Println("[3/4] asking provider for ranked Danish candidates...")
		remaining := 12 - st.Attempts
		maxProbe := remaining - 1
		if maxProbe > 2 {
			maxProbe = 2
		}
		cands, err := ordKloeverCandidates(ctx, cfg, *providerFlag, st, maxProbe)
		if err != nil {
			if *dryRun {
				fmt.Println("Provider unavailable:", err)
				fmt.Println("[4/4] extracted state only (dry-run); not submitting.")
				return nil
			}
			fmt.Printf("   initial candidates provider failed (%v)\n", err)
		}
		if len(cands) > 0 {
			printCandidates(cands)
		}
		fmt.Println("[4/4] not submitting without explicit --answer (to play for real: --answer '...' --submit; guesses/submissions are permanent, no do-overs).")
		_ = dryRun
		return nil
	}
	if effShape := klublotto.EffectiveShapeForMatching(st.Board, st.Shape); !klublotto.PhraseMatchesLengthPattern(answer, effShape) {
		return fmt.Errorf("answer %q does not match Ordkløver answer pattern %s", answer, effShape)
	}

	fmt.Println("[3/4] using explicit answer:", answer)
	if *dryRun {
		fmt.Println("[4/4] dry-run — not submitting.")
		return nil
	}
	if !*submitFlag {
		fmt.Println("[4/4] --submit not given — not submitting (for real play with no do-overs, pass --submit when supplying --answer).")
		return nil
	}

	fmt.Println("[4/4] submitting through parent page (real play, permanent, no do-overs)...")
	submitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = submitOrdKloever(submitCtx, br, answer)
	cancel()
	if err != nil {
		return err
	}
	shot := filepath.Join(cfg.DataDir, "ordkloever-result-"+time.Now().UTC().Format("20060102-150405")+".png")
	_ = br.Screenshot(ctx, shot)
	return upsertDailyGame(ctx, cfg, "Ordkløver", klublotto.OrdKloeverPrompt(st), answer, true, true, "Submitted through parent page. Screenshot: `"+shot+"`.")
}

// ordKloeverExtractTimeout bounds a single Ordkløver state extraction (browser
// crop + vision board read). It must be generous enough for a slow reasoning
// vision model: a 45s budget produced "openrouter-vision: read response:
// context deadline exceeded" on the ~google/gemini-pro-latest board read
// mid-game (our ctx expiring during the response read, not the HTTP client,
// which is already 540s). The on-error fallback gets its own extra budget on a
// detached context.
const ordKloeverExtractTimeout = 120 * time.Second

// ordKloeverFallbackVision builds an on-error fallback vision provider that is
// distinct from the primary, preferring Gemini 2.5 Pro. The primary Ordkløver
// reader is usually the OpenRouter floating alias (OPENROUTER_VISION_MODEL),
// which intermittently times out on the board read; a direct Gemini 2.5 Pro
// call avoids that routing entirely. Returns nil if no distinct provider is
// available (the caller then just has no fallback).
func ordKloeverFallbackVision(cfg *config.Config, primary llm.VisionProvider) llm.VisionProvider {
	name := func(vp llm.VisionProvider) string {
		if vp == nil {
			return ""
		}
		type namer interface{ Name() string }
		if n, ok := vp.(namer); ok {
			return n.Name()
		}
		return fmt.Sprintf("%T", vp)
	}
	pn := name(primary)
	// Direct Gemini 2.5 Pro is the most reliable fallback (skips OpenRouter).
	if cfg.GeminiKey != "" && !strings.Contains(pn, "gemini-2.5-pro") {
		return llm.NewGemini(cfg.GeminiKey, "gemini-2.5-pro")
	}
	// Otherwise Gemini 2.5 Pro pinned via OpenRouter (not the floating alias).
	if cfg.OpenRouterKey != "" && !strings.Contains(pn, "google/gemini-2.5-pro") {
		return llm.NewOpenRouterVision(cfg.OpenRouterKey, "google/gemini-2.5-pro")
	}
	// Last resort: a different model family.
	if cfg.AnthropicKey != "" && !strings.Contains(pn, "claude") {
		return llm.NewAnthropic(cfg.AnthropicKey, "claude-haiku-4-5-20251001")
	}
	return nil
}

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

func wordProvider(cfg *config.Config, override string) (llm.JSONGenerator, error) {
	name := strings.TrimSpace(override)
	if name == "" {
		name = strings.TrimSpace(cfg.WordProvider)
	}
	if name == "" {
		name = "gemini"
	}

	// Z.AI (Zhipu GLM) — OpenAI-compatible, cheaper than OpenRouter's fused models.
	// Accept "zai" (default model), "zai:<model>"/"zai/<model>", or a bare "glm-…"
	// slug. Checked before the '/' OpenRouter routing so "zai/glm-5.2" doesn't leak
	// to OpenRouter.
	if low := strings.ToLower(name); low == "zai" || low == "glm" || low == "zhipu" ||
		strings.HasPrefix(low, "zai:") || strings.HasPrefix(low, "zai/") || strings.HasPrefix(low, "glm-") {
		if cfg.ZAIKey == "" {
			return nil, fmt.Errorf("ZAI_API_KEY is required for Z.AI provider %q", name)
		}
		model := cfg.ZAIModel
		if i := strings.IndexAny(name, ":/"); i >= 0 { // zai:glm-5.2 / zai/glm-5.2
			model = strings.TrimSpace(name[i+1:])
		} else if strings.HasPrefix(low, "glm-") { // bare "glm-5.2"
			model = name
		}
		return llm.NewZAI(cfg.ZAIKey, model), nil
	}

	// If the name contains a '/' it is an OpenRouter model slug
	// (e.g. "google/gemini-3.1-pro-preview", "meta-llama/llama-3.3-70b-instruct").
	// Route it directly to OpenRouter without requiring the keyword "openrouter".
	if strings.Contains(name, "/") {
		if cfg.OpenRouterKey == "" {
			return nil, fmt.Errorf("OPENROUTER_API_KEY is required for OpenRouter model %q", name)
		}
		return llm.NewOpenRouter(cfg.OpenRouterKey, name), nil
	}

	switch strings.ToLower(name) {
	case "gemini":
		if cfg.GeminiKey == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY is required for word provider gemini")
		}
		return llm.NewGemini(cfg.GeminiKey, "gemini-2.5-pro"), nil
	case "openai":
		if cfg.OpenAIKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY is required for word provider openai")
		}
		return llm.NewOpenAI(cfg.OpenAIKey, cfg.OpenAIModel), nil
	case "xai", "grok":
		if cfg.XAIKey == "" {
			return nil, fmt.Errorf("XAI_API_KEY is required for word provider xai")
		}
		return llm.NewXAI(cfg.XAIKey, ""), nil
	case "anthropic", "claude":
		if cfg.AnthropicKey == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is required for word provider anthropic")
		}
		return llm.NewAnthropic(cfg.AnthropicKey, ""), nil
	case "openrouter":
		if cfg.OpenRouterKey == "" {
			return nil, fmt.Errorf("OPENROUTER_API_KEY is required for word provider openrouter")
		}
		return llm.NewOpenRouter(cfg.OpenRouterKey, cfg.OpenRouterModel), nil
	default:
		return nil, fmt.Errorf("unknown word provider %q — use a keyword (gemini|openai|xai|anthropic|openrouter|zai) or a model slug (zai:glm-5.2, or a full OpenRouter slug e.g. google/gemini-3.1-pro-preview)", name)
	}
}

func ordKloeverCandidates(ctx context.Context, cfg *config.Config, provider string, st klublotto.OrdKloeverState, maxProbe int) ([]klublotto.WordCandidate, error) {
	cands, err := wordCandidates(ctx, cfg, provider, klublotto.BuildOrdKloeverPrompt(st, maxProbe))
	if err != nil {
		return nil, err
	}
	// When the board carries a structural dash, the displayed shape (e.g. "9+8")
	// counts it, but a correct answer like TRYGHEDS-NARKOMAN normalizes to 8+8
	// typed letters. Validate against the dash-excluded shape so we don't drop it.
	effShape := klublotto.EffectiveShapeForMatching(st.Board, st.Shape)
	var dropped int
	cands, dropped = klublotto.FilterCandidatesByLengthPattern(cands, effShape)
	if dropped > 0 {
		fmt.Printf("Dropped %d candidate(s) that did not match answer pattern %s.\n", dropped, effShape)
	}
	cands, dropped = klublotto.FilterCandidatesByMask(cands, st.Board)
	if dropped > 0 {
		fmt.Printf("Dropped %d candidate(s) that did not match board mask %s.\n", dropped, st.Board)
	}
	if len(cands) == 0 {
		fmt.Println("   no cands matched shape/mask after first call; re-asking LLM with stricter instructions for shape and strategy...")
		strictPrompt := klublotto.BuildOrdKloeverPrompt(st, maxProbe) + "\n\nDe tidligere forslag matchede ikke det krævede svarmønster '" + st.Shape + "'. Giv KUN kandidater med ordlængder der præcist matcher tallene i mønsteret (f.eks. for '3+2+3' præcis tre ord på 3, 2 og 3 bogstaver). De skal være rigtige danske 'Udtryk' for kategorien, og matche eventuelle kendte bogstaver i BOARD."
		cands2, err2 := wordCandidates(ctx, cfg, provider, strictPrompt)
		if err2 == nil {
			cands2, _ = klublotto.FilterCandidatesByLengthPattern(cands2, effShape)
			cands2, _ = klublotto.FilterCandidatesByMask(cands2, st.Board)
			if len(cands2) > 0 {
				cands = cands2
			}
		}
	}
	// LAST RESORT: if shape filter dropped everything, try board-mask-only (shape extraction is unreliable).
	// This gives the LLM a chance to guess even when the extracted shape pattern is wrong.
	if len(cands) == 0 && st.Board != "" {
		fmt.Println("   shape filter dropped all candidates; retrying with board-mask-only (shape extraction may be wrong)...")
		relaxedPrompt := klublotto.BuildOrdKloeverPrompt(st, maxProbe) + "\n\nDet udtrukne svarmønster er muligvis forkert. Ignorér svarmønsteret og foreslå i stedet udtryk der KUN matcher de kendte bogstaver i BOARD. Prioritér at matche de afslørede bogstaver korrekt."
		cands3, err3 := wordCandidates(ctx, cfg, provider, relaxedPrompt)
		if err3 == nil {
			// Only apply board mask, NOT shape filter.
			cands3, _ = klublotto.FilterCandidatesByMask(cands3, st.Board)
			if len(cands3) > 0 {
				fmt.Printf("   relaxed filter kept %d candidate(s) (shape ignored)\n", len(cands3))
				cands = cands3
			}
		}
	}
	return cands, nil
}

// ordKloeverCategoryHint returns an extra Danish hint line for the LLM
// based on the puzzle category, to prevent common reasoning mistakes.
func ordKloeverCategoryHint(category string) string {
	cat := strings.ToUpper(strings.TrimSpace(category))
	switch {
	case strings.Contains(cat, "DANMARKSKORTET") || strings.Contains(cat, "DANMARK"):
		return "VIGTIGT: Svaret er ÉT stednavn på Danmarkskortet — fx 'KRONBORG SLOT' eller 'SILKEBORG BAD'. Det er ét sammenhængende stednavn (evt. to ord), IKKE to separate steder."
	case strings.Contains(cat, "FILM") || strings.Contains(cat, "MOVIE"):
		return "VIGTIGT: Svaret er én filmtitel (evt. to-tre ord)."
	case strings.Contains(cat, "SANG") || strings.Contains(cat, "MUSIK"):
		return "VIGTIGT: Svaret er én sangtitel."
	case strings.Contains(cat, "PERSON") || strings.Contains(cat, "NAVN"):
		return "VIGTIGT: Svaret er ét personnavn (fornavn + efternavn)."
	default:
		return ""
	}
}

// OrdKloeverDecision is returned by askOrdKloeverDecision. The LLM chooses
// whether to guess the full phrase or probe 2 more letters.
type OrdKloeverDecision struct {
	Action     string   `json:"action"`     // "guess" or "probe"
	Phrase     string   `json:"phrase"`     // non-empty when action="guess"
	Letters    []string `json:"letters"`    // 2 letters when action="probe"
	Confidence string   `json:"confidence"` // "high"|"medium"|"low" for guess
	Rationale  string   `json:"rationale"`
}

// askOrdKloeverProbeLetters asks the LLM to suggest n letters most likely to
// appear in today's Ordkløver answer, given what is already known.
// alreadyTried is the space-separated set of ALL letters tried so far
// (both correct and wrong) — used only to avoid suggesting them again.
func askOrdKloeverProbeLetters(ctx context.Context, cfg *config.Config, provider string, st klublotto.OrdKloeverState, n int, alreadyTried string) ([]string, error) {
	p, err := wordProvider(cfg, provider)
	if err != nil {
		return nil, err
	}
	remaining := 12 - st.Attempts
	// Derive shape from board in case the extracted shape field is wrong.
	boardShape := klublotto.BoardShapeFromString(st.Board)
	effectiveShape := st.Shape
	if boardShape != "" && boardShape != st.Shape {
		effectiveShape = boardShape + " (board-count; extracted shape: " + st.Shape + ")"
	}
	// Wrong letters = tried but NOT revealed on board.
	wrongLetters := klublotto.BoardWrongLetters(st.Board, alreadyTried)
	categoryHint := ordKloeverCategoryHint(st.Category)

	prompt := fmt.Sprintf(`Du løser et dansk "Ordkløver" puzzle (Wheel of Fortune-stil) på danskespil.dk.

Kategori: %s
%s
Svarform: %s (antal bogstaver per ord)
BOARD — kendte bogstaver (_ = ukendt position): %s

Wheel of Fortune-regler:
• Bogstaver vist på BOARD er bekræftede og sidder nøjagtigt der
• Forkerte bogstaver forekommer SLET IKKE noget sted i svaret
• Et bogstav der kun ses i ét ord er IKKE i det andet ord

Forkerte bogstaver (forsøgt, ikke i svaret): %s
Forsøg brugt: %d/12 (%d tilbage)

Foreslå %d bogstaver der sandsynligvis indgår i svaret.
Undgå: %s

Svar med præcis dette JSON (ingen anden tekst):
{"letters":["A","B"],"rationale":"kort begrundelse"}`,
		st.Category, categoryHint, effectiveShape, st.Board,
		wrongLetters, st.Attempts, remaining, n, alreadyTried,
	)

	modelCtx, cancel := context.WithTimeout(ctx, 540*time.Second)
	raw, err := p.GenerateJSON(modelCtx, prompt, 0.3)
	cancel()
	if err != nil {
		return nil, err
	}
	clean := klublotto.ExtractJSONObject(raw)
	var result struct {
		Letters []string `json:"letters"`
	}
	if err := json.Unmarshal([]byte(clean), &result); err != nil {
		return nil, fmt.Errorf("parse probe-letters response: %w (raw=%s)", err, raw)
	}
	var out []string
	triedSet := map[rune]bool{}
	for _, r := range []rune(klublotto.NormalizeDanishLetters(alreadyTried)) {
		triedSet[r] = true
	}
	for _, l := range result.Letters {
		l = klublotto.NormalizeDanishLetters(l)
		if l == "" {
			continue
		}
		r := []rune(l)[0]
		if !triedSet[r] {
			out = append(out, string(r))
			triedSet[r] = true
		}
	}
	return out, nil
}

// endgameInstruction returns the bullet-point strategy line for the decision prompt,
// escalating urgency as attempts run out. maxProbe = min(2, remaining-1).
func endgameInstruction(remaining int) string {
	maxProbe := remaining - 1
	if maxProbe > 2 {
		maxProbe = 2
	}
	switch {
	case maxProbe <= 0:
		return "• SIDSTE FORSØG — du SKAL gætte frasen nu. Probe er IKKE tilladt."
	case maxProbe == 1:
		return fmt.Sprintf("• Kun %d forsøg tilbage — du kan probe ét bogstav, men gæt frasen hvis du er nogenlunde sikker.", remaining)
	default:
		return "• Gæt kun frasen hvis du er rimelig sikker — ellers probe op til 2 bogstaver."
	}
}

// endgameActionBlock returns the action-choice section of the decision prompt.
// When only 1 attempt remains, option B (probe) is removed entirely.
// When 2 remain, probe is limited to 1 letter.
func endgameActionBlock(remaining int) string {
	maxProbe := remaining - 1
	if maxProbe > 2 {
		maxProbe = 2
	}
	switch {
	case maxProbe <= 0:
		return `DETTE ER DIT SIDSTE FORSØG. Du SKAL gætte hele frasen:
   {"action":"guess","phrase":"SVAR HER","confidence":"high|medium|low","rationale":"..."}`
	case maxProbe == 1:
		return `A) Gæt hele frasen:
   {"action":"guess","phrase":"SVAR HER","confidence":"high|medium|low","rationale":"..."}

B) Probe præcis 1 nyt bogstav:
   {"action":"probe","letters":["X"],"rationale":"..."}`
	default:
		return `A) Gæt hele frasen:
   {"action":"guess","phrase":"SVAR HER","confidence":"high|medium|low","rationale":"..."}

B) Probe op til 2 nye bogstaver:
   {"action":"probe","letters":["X","Y"],"rationale":"..."}`
	}
}

// askOrdKloeverDecision asks the LLM to choose between guessing the full phrase
// or probing 2 more letters, given the current board state.
// alreadyTried is ALL letters tried (both correct and wrong) — used to prevent
// re-suggesting them. We derive wrong-only letters from board for the prompt.
func askOrdKloeverDecision(ctx context.Context, cfg *config.Config, provider string, st klublotto.OrdKloeverState, alreadyTried string, cands []klublotto.WordCandidate) (OrdKloeverDecision, error) {
	p, err := wordProvider(cfg, provider)
	if err != nil {
		return OrdKloeverDecision{}, err
	}
	fmt.Printf("   [decision] model: %s\n", p.Name())
	remaining := 12 - st.Attempts

	// Derive shape from board token-count (more reliable than OCR-extracted shape).
	boardShape := klublotto.BoardShapeFromString(st.Board)
	shapeInfo := st.Shape
	if boardShape != "" && boardShape != st.Shape {
		shapeInfo = fmt.Sprintf("%s (board-count: %s, extracted: %s)", boardShape, boardShape, st.Shape)
	}

	// Only truly wrong letters (tried but NOT on board) go in the "wrong" list.
	wrongLetters := klublotto.BoardWrongLetters(st.Board, alreadyTried)

	categoryHint := ordKloeverCategoryHint(st.Category)

	// When the board shows a structural dash, explain it so the model includes
	// the dash and counts it as a position (not a missing letter). "/" separates
	// the display word-groups.
	boardDisplay := st.Board
	if strings.Contains(st.Board, "-") {
		boardDisplay = st.Board + "\n  (BEMÆRK: '-' er en fast bindestreg der hører til svaret — fx et bindestregsord som TRYGHEDS-NARKOMAN. Den tæller som én position men skrives ikke som bogstav. '/' adskiller ord. Medtag bindestregen i din frase.)"
	}

	// Build candidate block — the LLM MUST choose from this list when guessing.
	var candBlock strings.Builder
	if len(cands) > 0 {
		candBlock.WriteString("\n=== KANDIDATLISTE (allerede beregnet) ===\n")
		candBlock.WriteString("Disse kandidater er sorteret efter sandsynlighed og matcher boardet og svarformen.\n")
		if remaining <= 1 {
			candBlock.WriteString("Du HAR KUN ÉT FORSØG TILBAGE. Vælg den bedste kandidat — probe er ikke muligt.\n\n")
		} else {
			candBlock.WriteString("Når du vælger action=\"guess\", SKAL \"phrase\" være én af disse kandidater.\n")
			candBlock.WriteString("Opfind IKKE nye ord — vælg den bedste match fra listen nedenfor.\n")
			candBlock.WriteString("Hvis ingen kandidat er korrekt, vælg action=\"probe\" i stedet.\n\n")
		}
		for i, c := range cands {
			conf := c.Confidence
			if conf == "" {
				conf = "?"
			}
			candBlock.WriteString(fmt.Sprintf("%d. %s (%s) — %s\n", i+1, c.Answer, conf, c.Rationale))
		}
	}

	// Build the top-level ask line matching the probe budget.
	maxProbeForPrompt := remaining - 1
	if maxProbeForPrompt > 2 {
		maxProbeForPrompt = 2
	}
	var askLine string
	switch {
	case maxProbeForPrompt <= 0:
		askLine = "Giv kun 1 svar — dette er SIDSTE FORSØG."
	case maxProbeForPrompt == 1:
		askLine = "Giv eksakt 1 svar på løsningen, eller alternativt foreslå ét nyt bogstav vi kan gætte."
	default:
		askLine = "Giv eksakt 1 svar på løsningen, eller alternativt foreslå op til to nye bogstaver vi kan gætte."
	}

	prompt := fmt.Sprintf(`Du løser et dansk "Ordkløver" puzzle (som Wheel of Fortune) på danskespil.dk.
%s

Kategori: %s
%s

Svarform: %s
(Svarformen angiver antal bogstaver per ord — fx "8+4" = 8 bogstaver, mellemrum, 4 bogstaver = 12 bogstaver i alt)

BOARD — kendte bogstaver i nøjagtigt de rigtige positioner (_ = ukendt):
  %s

=== WHEEL OF FORTUNE REGLER (MEGET VIGTIGT) ===
1. Bogstaver på BOARD er 100%% bekræftede — de sidder præcis der.
2. Forkerte bogstaver er ABSOLUT FRAVÆRENDE fra hele svaret — de forekommer ikke ét eneste sted.
3. Hvis et bogstav er bekræftet i ord 1 men IKKE i ord 2 → ord 2 indeholder IKKE det bogstav.
4. Svaret er ét sammenhængende udtryk, der matcher svarformen bogstav for bogstav.
5. SIMULTANAVSLØRINGSREGEL (KRITISK): Når man prøver et bogstav, afsløres ALLE forekomster
   af det bogstav på én gang — ligesom i Lykkehjulet/Wheel of Fortune.
   Det betyder: hvis et prøvet bogstav X kun er synligt N gange på BOARD, er der præcis N
   forekomster af X i hele svaret — hverken flere eller færre.
   EKSEMPEL: O er kun synlig 1 gang på boardet → svaret har præcis 1 O i alt.
   Foreslå ALDRIG et udtryk, der indeholder et prøvet bogstav på FLERE positioner end det
   antal gange det vises på boardet. Hvis du gør det er svaret garanteret forkert.

Forkerte bogstaver — IKKE i svaret overhovedet: %s
Alle forsøgte bogstaver (undgå disse ved probe): %s
Forsøg brugt: %d/12 (%d tilbage)
%s
Instrukser:
• Analyser hvad BOARD og svarformen fortæller dig om svaret.
• Tæl nøje: hvis bogstavet O er vist 1 gang på boardet, kan svaret ikke indeholde 2 O'er.
• Bogstaver i ord 1 men ikke i ord 2 på BOARD = ord 2 har dem IKKE.
%s
Vælg ÉN handling og returner præcis ét JSON objekt:

%s
Regler for dit svar:
- "phrase" skal matche svarformen %s — bogstav for bogstav, inklusive mellemrum
- Alle kendte bogstaver fra BOARD skal være på de rigtige pladser i "phrase"
- Hvert prøvet bogstav på BOARD optræder præcis så mange gange i "phrase" som på boardet
- "probe"-bogstaver må IKKE være i listen over forsøgte bogstaver
- Du har maksimalt 480 sekunder til at svare — prioritér hurtigt svar
- Returner KUN JSON — ingen tekst udenfor JSON-objektet`,
		askLine,
		st.Category, categoryHint,
		shapeInfo,
		boardDisplay,
		wrongLetters, alreadyTried,
		st.Attempts, remaining,
		candBlock.String(),
		endgameInstruction(remaining),
		endgameActionBlock(remaining),
		shapeInfo,
	)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return OrdKloeverDecision{}, ctx.Err()
			case <-time.After(3 * time.Second):
			}
			fmt.Printf("   [decision retry %d/3]: %v\n", attempt+1, lastErr)
		}
		modelCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
		raw, callErr := p.GenerateJSON(modelCtx, prompt, 0.2)
		cancel()
		if callErr != nil {
			lastErr = callErr
			if ctx.Err() != nil {
				return OrdKloeverDecision{}, ctx.Err()
			}
			continue
		}
		clean := klublotto.ExtractJSONObject(raw)
		var d OrdKloeverDecision
		if err := json.Unmarshal([]byte(clean), &d); err != nil {
			lastErr = fmt.Errorf("parse decision: %w (raw=%s)", err, raw)
			continue
		}
		d.Action = strings.ToLower(strings.TrimSpace(d.Action))
		if d.Action != "guess" && d.Action != "probe" {
			lastErr = fmt.Errorf("unknown action %q in decision", d.Action)
			continue
		}
		if d.Action == "guess" {
			d.Phrase = klublotto.NormalizeDanishPhrase(d.Phrase)
		}
		return d, nil
	}
	return OrdKloeverDecision{}, fmt.Errorf("all decision attempts failed: %w", lastErr)
}

// ordKloeverReasoningAttempts is the attempts-used threshold (out of 12) at
// which the Ordkløver loop switches from the fast word model to the heavier
// reasoning model. Below it we favour speed; at/after it we favour accuracy
// because every remaining guess is precious.
const ordKloeverReasoningAttempts = 7

func runOrdKloeverProbe(ctx context.Context, cfg *config.Config, br *browser.Client, st klublotto.OrdKloeverState, ac llm.VisionProvider, provider, finalProvider string, cands []klublotto.WordCandidate, dry bool, _ int, _ bool) error {
	// Attempt-tiered word/decision model: the fast `provider` while we still have
	// plenty of attempts, switching to the heavier reasoning `finalProvider` once
	// we've used ordKloeverReasoningAttempts (7/12) or more. activeProvider reads
	// st.Attempts at call time, so it tracks the live attempt count each round.
	lastTier := ""
	activeProvider := func() string {
		p := provider
		tier := "fast"
		if finalProvider != "" && st.Attempts >= ordKloeverReasoningAttempts {
			p = finalProvider
			tier = "reasoning"
		}
		if tier != lastTier {
			fmt.Printf("   [model] %s tier @ %d/12 → %s\n", tier, st.Attempts, p)
			lastTier = tier
		}
		return p
	}

	// Ensure we're on the Ordkløver parent page.
	curURL, _ := br.URL(ctx)
	if !strings.Contains(curURL, "danskespil.dk") || !strings.Contains(curURL, "ordkloever") {
		fmt.Println("       navigating to Ordkløver parent page for probe start...")
		for i := 0; i < 3; i++ {
			if err := br.Open(ctx, klublotto.OrdKloeverURL); err == nil {
				br.WaitSettled(ctx)
				time.Sleep(500 * time.Millisecond)
				break
			}
			if i < 2 {
				time.Sleep(700 * time.Millisecond)
			}
		}
	}
	_ = startGameIfNeeded(ctx, br, "SPIL ORDKLØVER", "SPIL ORDKLOEVER")
	_ = focusIntoKloeverGame(ctx, br)

	// reExtract re-reads game state — defined early so it can also bootstrap an
	// empty initial state (extraction happens before the game is entered, so the
	// first pass may see only the welcome screen and return all-empty fields).
	visionFallback := ordKloeverFallbackVision(cfg, ac)
	reExtract := func() (klublotto.OrdKloeverState, error) {
		// If danskespil's generic crash page ("Der skete en fejl. Prøv igen.")
		// replaced the game (it sometimes does right after a submit), reading the
		// board off it yields a blank screen that mis-reads as a finished/solved
		// game. Detect it FIRST and recover by reopening the game so we extract the
		// real, server-remembered state.
		if body, _ := br.Eval(ctx, `document.body ? document.body.innerText : ""`); klublotto.IsDanskeSpilErrorScreen(body) {
			fmt.Println(`   [recover] danskespil error screen detected ("Der skete en fejl") — reopening Ordkløver to recover real state...`)
			recoverFromOrdKloeverError(ctx, br)
		}
		_ = ensureKloeverActive(ctx, br)
		extractCtx, cancel := context.WithTimeout(ctx, ordKloeverExtractTimeout)
		next, err := klublotto.ExtractOrdKloeverState(extractCtx, br, ac, visionFallback)
		cancel()
		return next, err
	}

	// If the state passed in is empty (vision saw the launcher before game was
	// entered), re-read now that startGameIfNeeded has dismissed the welcome screen.
	if st.Board == "" && st.Category == "" && st.Shape == "" {
		fmt.Println("   [probe] initial state empty — re-extracting from live game board...")
		if fresh, ferr := reExtract(); ferr == nil && (fresh.Board != "" || fresh.Category != "" || fresh.Shape != "") {
			st = fresh
			fmt.Printf("   [probe] re-extracted: Category=%q Shape=%q Board=%q Attempts=%d\n",
				st.Category, st.Shape, st.Board, st.Attempts)
		} else if ferr != nil {
			fmt.Printf("   [probe] re-extract error: %v; continuing with empty state\n", ferr)
		} else {
			fmt.Println("   [probe] re-extract still empty; continuing (first probe will reveal board)")
		}
	}

	// Compute total answer length from shape (e.g. "9+3" → 12).
	totalLen := 0
	for _, n := range klublotto.LengthPattern(st.Shape) {
		totalLen += n
	}

	// Decide initial probe count from total length (user requirement).
	var firstCount int
	switch {
	case totalLen < 10:
		firstCount = 3
	case totalLen < 15:
		firstCount = 4
	default:
		firstCount = 5
	}
	if totalLen == 0 {
		firstCount = 3 // shape unknown — safe default
	}

	fmt.Printf("   FORSØG: %d/12 (%d remaining) | Shape: %s (%d letters) | Initial probe count: %d\n",
		st.Attempts, 12-st.Attempts, st.Shape, totalLen, firstCount)
	fmt.Printf("   Board:   %s\n", st.Board)
	fmt.Printf("   Guessed: %s\n", func() string {
		if st.GuessedLetters == "" {
			return "(none)"
		}
		return st.GuessedLetters
	}())

	if dry {
		effGuessed := st.GuessedLetters + " " + st.Board
		letters := klublotto.SuggestOrdKloeverLetters(cands, effGuessed, firstCount)
		fmt.Printf("[4/4] dry run — would probe letters: %s\n", strings.Join(letters, ", "))
		return nil
	}

	var probedThisRun []string // letters probed in this session (client-side tracking)
	// Seed from the initial vision-reported GUESSED so that letters probed in a
	// previous session (or earlier in this game) are never forgotten even when a
	// later vision re-extraction underreports them (e.g. drops dimmed letters like T).
	for _, r := range []rune(klublotto.NormalizeDanishLetters(st.GuessedLetters)) {
		if r != ' ' && r != '_' {
			probedThisRun = append(probedThisRun, string(r))
		}
	}

	// triedPhrases tracks every full-phrase guess we've already submitted this
	// run (normalized) so the decision loop never wastes an attempt re-submitting
	// an identical wrong guess.
	triedPhrases := map[string]bool{}

	// submitAndCheck submits a full-phrase guess, re-extracts state, and reports
	// whether the puzzle is solved.  Returns (solved, error).
	submitAndCheck := func(phrase, label string) (bool, error) {
		// Save board + attempts BEFORE the guess — wrong phrase guesses reveal no
		// new letters and the game reverts the tiles back to only the probed
		// letters. If we re-extract too quickly, the animation is still running and
		// we read the typed (wrong) phrase letters as if they were board letters.
		// Also snapshot category/shape/hint: the post-win re-extract often returns a
		// blank screen, which would otherwise wipe these from the ledger row.
		prePhraseBoard := st.Board
		preAttempts := st.Attempts
		preCategory := st.Category
		preShape := st.Shape
		preHint := st.Hint

		// Record this phrase so the loop never re-submits an identical wrong guess.
		triedPhrases[klublotto.NormalizeDanishPhrase(phrase)] = true

		// Shared timestamp so the before/after screenshot pair is easy to correlate.
		stamp := time.Now().UTC().Format("20060102-150405")
		// "Before" screenshot — the board state as it looked right before we submit.
		shotBefore := filepath.Join(cfg.DataDir, "ordkloever-guess-"+stamp+"-before.png")
		_ = br.Screenshot(ctx, shotBefore)

		fmt.Printf("[%s] submitting %q (before: %s)\n", label, phrase, shotBefore)
		// submitOrdKloever returns nil ONLY when it saw a win banner on the page
		// right after submitting; any other outcome (incl. a normal wrong guess)
		// returns a non-nil error. So a nil here is a strong "solved" signal that
		// we must not lose if the follow-up vision re-extract catches a blank
		// transition screen.
		submitErr := submitOrdKloever(ctx, br, phrase)
		wonAtSubmit := submitErr == nil
		if submitErr != nil {
			fmt.Printf("   submission note: %v\n", submitErr)
		}
		// Give the game enough time to either show the win animation or revert the
		// wrong-guess letters back to blanks before we read the board state.
		time.Sleep(3000 * time.Millisecond)

		// "After" screenshot of the post-guess screen for later inspection (win
		// banner detection, debugging mis-reads). Saved into the data dir
		// (.klublotto) alongside the vision crops.
		shot := filepath.Join(cfg.DataDir, "ordkloever-guess-"+stamp+"-after.png")
		_ = br.Screenshot(ctx, shot)

		next, err := reExtract()
		if err != nil {
			return false, fmt.Errorf("extract after submit: %w", err)
		}
		st = next
		// Bolster win detection: the submit step or the post-guess raw page text may
		// carry a success banner ("Flot præstation", "Super imponerende", …) even
		// when the vision board read came back blank.
		if wonAtSubmit || klublotto.IsOrdKloeverWinText(st.Raw) {
			st.Solved = true
		}
		// Defensive: the win banner ("Flot præstation! Du løste ordkløver med
		// stil!") renders in the Danske Spil PARENT body, but extraction can
		// overwrite st.Raw with the vision JSON (empty board on a win screen).
		// Read the parent body directly as the authoritative solved signal.
		if !st.Solved {
			klublotto.LeaveFrame(br) // ensure top frame
			if body, _ := br.Eval(ctx, `document.body ? document.body.innerText : ""`); klublotto.IsOrdKloeverWinText(body) {
				fmt.Println("   win banner detected in parent page text — marking Ordkløver solved")
				st.Solved = true
			}
		}
		// After a wrong phrase guess the board MUST be identical to before — Wheel of
		// Fortune only reveals new positions via letter probes, never via wrong phrase
		// guesses. Restore the pre-guess board if the puzzle is not solved yet, to
		// prevent the animation's intermediate tile-state from corrupting later rounds.
		if !st.Solved && prePhraseBoard != "" {
			st.Board = prePhraseBoard
		}
		// SAFEGUARD: a blank / transition-screen re-extraction reports attempts=0.
		// Never let the counter run backwards — a wrong full-phrase guess always
		// costs exactly one attempt. Without this the loop resets 5/12 → 0/12,
		// believes it has a fresh board, and re-submits the same wrong answer
		// forever.
		if !st.Solved && st.Attempts < preAttempts {
			st.Attempts = preAttempts + 1
			fmt.Printf("   [safeguard] re-extract reported attempts=%d (< %d before guess); restoring to %d\n",
				next.Attempts, preAttempts, st.Attempts)
		}
		// The post-win re-extract usually returns a blank screen — restore the
		// puzzle metadata so the ledger row keeps its category/shape/hint.
		if st.Category == "" {
			st.Category = preCategory
		}
		if st.Shape == "" {
			st.Shape = preShape
		}
		if st.Hint == "" {
			st.Hint = preHint
		}
		fmt.Printf("   After guess: Board=%s | Attempts=%d/12 | Solved=%v | Screenshot: %s\n",
			st.Board, st.Attempts, st.Solved, shot)
		if st.Solved || st.Attempts >= 12 || !strings.Contains(st.Board, "_") {
			// Board empty + attempts exhausted = win animation replaced the board; treat as solved.
			solved := st.Solved || (st.Board == "" && st.Attempts >= 12)
			// Notes: colour-coded letter-probe sequence + shape + how it finished.
			// Colour hits against the answer when solved, else against the board.
			revealSrc := prePhraseBoard
			if solved {
				revealSrc = phrase
			}
			shape := st.Shape
			if shape == "" {
				shape = preShape
			}
			notes := klublotto.OrdKloeverNotes(shape, revealSrc, probedThisRun, label)
			modelLabel := wordModelLabel(cfg, provider)
			if finalProvider != "" {
				if fl := wordModelLabel(cfg, finalProvider); fl != modelLabel {
					modelLabel = fmt.Sprintf("%s → %s (from %d/12)", modelLabel, fl, ordKloeverReasoningAttempts)
				}
			}
			notes = appendModelNote(notes, modelLabel)
			_ = upsertDailyGame(ctx, cfg, "Ordkløver", klublotto.OrdKloeverPrompt(st), phrase, true, solved, notes)
			return true, nil
		}
		fmt.Println("   Guess was wrong; continuing...")
		return false, nil
	}

	// allUsed returns all letters ever tried (site-tracked + client-side this run)
	// as a deduplicated space-separated uppercase string.
	// We do our own dedup/normalize here instead of going through CleanGuessedLetters
	// because that helper has a len>40 guard designed for raw vision output (not our
	// constructed string which can legally exceed 40 chars on a long board).
	allUsed := func() string {
		combined := st.GuessedLetters + " " + strings.Join(probedThisRun, " ") + " " + st.Board
		seen := map[rune]bool{}
		var out strings.Builder
		for _, r := range []rune(klublotto.NormalizeDanishLetters(combined)) {
			if !seen[r] {
				if out.Len() > 0 {
					out.WriteRune(' ')
				}
				out.WriteRune(r)
				seen[r] = true
			}
		}
		return out.String()
	}

	// ── Phase 1: Initial letter probing ─────────────────────────────────────────
	// Skip if the board already has some revealed letters (game was partially played).
	boardHasLetters := strings.ContainsAny(strings.ToUpper(st.Board), "ABCDEFGHIJKLMNOPQRSTUVWXYZÆØÅ")
	if !boardHasLetters && st.Attempts < 12 {
		// Pick first-round letters from pre-computed candidates or fallback to
		// SuggestOrdKloeverLetters (frequency-based).
		effGuessed := allUsed()
		letters := klublotto.SuggestOrdKloeverLetters(cands, effGuessed, firstCount)
		if len(letters) == 0 {
			// Candidates empty — ask LLM directly.
			if ll, err := askOrdKloeverProbeLetters(ctx, cfg, activeProvider(), st, firstCount, effGuessed); err == nil && len(ll) > 0 {
				letters = ll
			}
		}
		if len(letters) > 0 && 12-st.Attempts >= len(letters) {
			fmt.Printf("[phase-1] probing initial %d letters: %s\n", len(letters), strings.Join(letters, ", "))
			if err := submitOrdKloeverLetters(ctx, br, letters); err != nil {
				return fmt.Errorf("probe initial letters: %w", err)
			}
			probedThisRun = append(probedThisRun, letters...)
			next, err := reExtract()
			if err != nil {
				return fmt.Errorf("extract after initial probe: %w", err)
			}
			st = next
			st.GuessedLetters = allUsed()
			fmt.Printf("   Board: %s | Guessed: %s | Attempts: %d/12\n",
				st.Board, st.GuessedLetters, st.Attempts)

			// If no letters hit, keep probing 2 at a time until at least 1 reveals.
			const maxMissRounds = 4
			for missRound := 1; missRound <= maxMissRounds && !klublotto.BoardHasHit(st.Board, probedThisRun) && st.Attempts < 12; missRound++ {
				fmt.Printf("[phase-1 miss %d/%d] no letters revealed yet; asking LLM for 2 more probe letters...\n", missRound, maxMissRounds)
				used := allUsed()
				more, err := askOrdKloeverProbeLetters(ctx, cfg, activeProvider(), st, 2, used)
				if err != nil || len(more) == 0 {
					fmt.Printf("   letter suggestion failed: %v; moving to phase 2\n", err)
					break
				}
				fmt.Printf("   probing: %s\n", strings.Join(more, ", "))
				if err := submitOrdKloeverLetters(ctx, br, more); err != nil {
					return fmt.Errorf("probe miss-round letters: %w", err)
				}
				probedThisRun = append(probedThisRun, more...)
				next, err := reExtract()
				if err != nil {
					return fmt.Errorf("extract after miss-round probe: %w", err)
				}
				st = next
				st.GuessedLetters = allUsed()
				fmt.Printf("   Board: %s | Guessed: %s | Attempts: %d/12\n",
					st.Board, st.GuessedLetters, st.Attempts)
			}
		}
	}

	// ── Phase 2: Solve-or-probe decision loop ───────────────────────────────────
	// Endgame probe budget: always keep 1 attempt for the final guess.
	//   remaining=1  → maxProbe=0 → break immediately, guess now
	//   remaining=2  → maxProbe=1 → probe at most 1 letter, then guess   (9/12 → 10/12 → guess)
	//   remaining=3  → maxProbe=2 → probe at most 2 letters, then guess  (wait, this is 10/12)
	// Rule summary: 11/12 → guess; 10/12 → 1 letter then guess; 9/12 → 2 letters then guess.
	const maxDecisionRounds = 10
	for round := 1; round <= maxDecisionRounds && st.Attempts < 12; round++ {
		remaining := 12 - st.Attempts
		maxProbe := remaining - 1 // letters we can still probe before final guess
		if maxProbe > 2 {
			maxProbe = 2
		}
		fmt.Printf("[phase-2 round %d] FORSØG: %d/12 (%d remaining, max probe: %d) | Board: %s\n",
			round, st.Attempts, remaining, maxProbe, st.Board)

		// No probe budget left — must guess now.
		if maxProbe <= 0 {
			fmt.Printf("   %d attempt(s) left — must guess now, no probing allowed\n", remaining)
			break
		}

		used := allUsed()
		decision, err := askOrdKloeverDecision(ctx, cfg, activeProvider(), st, used, cands)
		if err != nil {
			fmt.Printf("   decision LLM failed (%v); falling back to top candidate\n", err)
			break
		}

		fmt.Printf("   LLM decision: action=%q phrase=%q letters=%v conf=%s\n   rationale: %s\n",
			decision.Action, decision.Phrase, decision.Letters, decision.Confidence, decision.Rationale)

		if decision.Action == "guess" {
			if decision.Phrase == "" {
				fmt.Println("   guess action but empty phrase; treating as probe")
			} else {
				// Validate the phrase against the known board constraints before submitting:
				// any letter in allUsed() that does NOT appear on the board is a "not-found"
				// letter and must not appear in the phrase.
				usedNow := allUsed()
				boardUpper := strings.ToUpper(st.Board)
				phraseUpper := strings.ToUpper(decision.Phrase)
				var forbidden []string
				usedSet := map[rune]bool{}
				for _, r := range []rune(klublotto.NormalizeDanishLetters(usedNow)) {
					if r != ' ' {
						usedSet[r] = true
					}
				}
				for r := range usedSet {
					// Letter is "not found" if it's in used but NOT visible on the current board.
					if !strings.ContainsRune(boardUpper, r) && strings.ContainsRune(phraseUpper, r) {
						forbidden = append(forbidden, string(r))
					}
				}
				if len(forbidden) > 0 {
					fmt.Printf("   [validate] rejecting %q — contains forbidden (tried+not-found) letters: %s\n",
						decision.Phrase, strings.Join(forbidden, " "))
					decision.Action = "probe"
					decision.Letters = nil
				}

				// Skip phrases we've already submitted this run — re-guessing an
				// identical wrong answer just burns an attempt (and was the cause of
				// the earlier infinite loop).
				if decision.Action == "guess" && triedPhrases[klublotto.NormalizeDanishPhrase(decision.Phrase)] {
					fmt.Printf("   [dedup] %q already guessed this run — probing instead of re-submitting\n", decision.Phrase)
					decision.Action = "probe"
					decision.Letters = nil
				}

				if decision.Action == "guess" {
					// Fire-and-forget ordnet.dk lookup — informational only, non-blocking.
					// Only for single words; multi-word phrases are not indexed as units in DDO.
					if !strings.Contains(strings.TrimSpace(decision.Phrase), " ") {
						phrase := decision.Phrase
						go func() {
							ordCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							found, ordErr := klublotto.CheckOrdnet(ordCtx, phrase)
							if ordErr != nil {
								fmt.Printf("   [ordnet] %q — lookup error: %v\n", phrase, ordErr)
							} else if found {
								fmt.Printf("   [ordnet] %q ✓ found in DDO\n", phrase)
							} else {
								fmt.Printf("   [ordnet] %q ✗ NOT found in DDO\n", phrase)
							}
						}()
					}
					solved, err := submitAndCheck(decision.Phrase, fmt.Sprintf("phase-2 round %d guess", round))
					if err != nil {
						return err
					}
					if solved {
						return nil
					}
					// Wrong guess — continue to next round (state updated by submitAndCheck).
					continue
				}
			}
		}

		// Probe up to maxProbe letters.
		letters := decision.Letters
		if len(letters) == 0 {
			fmt.Printf("   probe action but no letters returned; asking again (max %d)...\n", maxProbe)
			if ll, err2 := askOrdKloeverProbeLetters(ctx, cfg, activeProvider(), st, maxProbe, used); err2 == nil {
				letters = ll
			}
		}
		// Trim to budget.
		if len(letters) > maxProbe {
			letters = letters[:maxProbe]
		}
		if len(letters) == 0 {
			fmt.Println("   no useful probe letters; forcing guess now")
			break
		}
		// Filter out already-used letters.
		usedSet := map[rune]bool{}
		for _, r := range klublotto.NormalizeDanishLetters(used) {
			usedSet[r] = true
		}
		var clean []string
		for _, l := range letters {
			l = klublotto.NormalizeDanishLetters(l)
			if l == "" || usedSet[[]rune(l)[0]] {
				continue
			}
			clean = append(clean, l)
			usedSet[[]rune(l)[0]] = true
		}
		if len(clean) == 0 {
			fmt.Println("   all suggested letters already used; forcing guess now")
			break
		}
		fmt.Printf("   probing: %s\n", strings.Join(clean, ", "))
		if err := submitOrdKloeverLetters(ctx, br, clean); err != nil {
			return fmt.Errorf("probe phase-2 letters: %w", err)
		}
		probedThisRun = append(probedThisRun, clean...)
		next, err := reExtract()
		if err != nil {
			return fmt.Errorf("extract after phase-2 probe: %w", err)
		}
		st = next
		st.GuessedLetters = allUsed()
		fmt.Printf("   Board: %s | Guessed: %s | Attempts: %d/12\n",
			st.Board, st.GuessedLetters, st.Attempts)
	}

	// ── Final fallback: submit best available candidate ──────────────────────────
	if st.Attempts >= 12 {
		fmt.Println("[probe] out of attempts")
		return nil
	}
	// Re-fetch with the final-crunch LLM and a "Giv kun 1 svar" prompt (maxProbe=0).
	if finalProvider != "" && finalProvider != provider {
		fmt.Printf("[final] switching to final-crunch LLM: %s\n", finalProvider)
	}
	if finalCands, err := ordKloeverCandidates(ctx, cfg, finalProvider, st, 0); err == nil && len(finalCands) > 0 {
		cands = finalCands
		printCandidates(cands)
	}
	top := ""
	if len(cands) > 0 {
		top = cands[0].Answer
	}
	if top == "" {
		// No candidate survived filters — ask the final-crunch LLM directly.
		fmt.Println("[final] no filtered candidate — asking final-crunch LLM for last-attempt guess...")
		used := allUsed()
		dec, decErr := askOrdKloeverDecision(ctx, cfg, finalProvider, st, used, nil)
		if decErr == nil && dec.Action == "guess" && dec.Phrase != "" {
			top = dec.Phrase
			fmt.Printf("[final] final-crunch LLM suggests: %q\n", top)
		}
	}
	if top == "" {
		fmt.Println("[final] no candidate available; skipping final submit")
		return nil
	}
	solved, err := submitAndCheck(top, "final-fallback")
	if err != nil {
		return err
	}
	_ = solved
	return nil
}

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

func submitOrdKloeverLetter(ctx context.Context, br *browser.Client, letter string) error {
	letter = klublotto.NormalizeDanishLetters(letter)
	if letter == "" {
		return fmt.Errorf("empty letter")
	}
	// Only navigate if not already on the Ordkløver parent page — avoids iframe reload
	// which would lose the current game state and trigger the welcome screen.
	curURL, _ := br.URL(ctx)
	onParent := strings.Contains(curURL, "danskespil.dk") && strings.Contains(curURL, "ordkloever")
	if !onParent {
		fmt.Println("       navigating to Danske Spil Ordkløver parent page...")
		var openErr error
		for i := 0; i < 3; i++ {
			if openErr = br.Open(ctx, klublotto.OrdKloeverURL); openErr == nil {
				br.WaitSettled(ctx)
				time.Sleep(600 * time.Millisecond)
				break
			}
			if i < 2 {
				time.Sleep(800 * time.Millisecond)
			}
		}
		if openErr != nil {
			_ = br.Screenshot(ctx, filepath.Join(".klublotto", "ordkloever-open-fail-"+time.Now().UTC().Format("20060102-150405")+".png"))
			return fmt.Errorf("open parent for known-state letter submit: %w", openErr)
		}
		_ = focusIntoKloeverGame(ctx, br)
	}

	// Use frame context to reliably click the on-screen virtual keyboard buttons.
	fmt.Println("       switching to ordkloever iframe frame context for letter input...")
	frameErr := klublotto.EnterGameFrame(ctx, br)
	if frameErr != nil {
		frameErr = br.Frame(ctx, "iframe[src*='ordkloever']")
	}
	if frameErr != nil {
		frameErr = br.Frame(ctx, "iframe[src*='clover']")
	}
	if frameErr == nil {
		defer func() {
			klublotto.LeaveFrame(br)
			// Scroll the game iframe back into view after frame reset to stop page jumping.
			_, _ = br.Eval(ctx, `(() => {
  const ifr = document.querySelector('iframe.kl-game__iframe, iframe[src*="ordkloever"], iframe[src*="clover"]');
  if (ifr) ifr.scrollIntoView({block:'center', behavior:'instant'});
})()`)
		}()

		// Wait for keyboard to be ready, clicking through the welcome screen if needed.
		// The iframe may show "SPIL ORDKLØVER" (welcome) instead of the game board when
		// the parent page was navigated or the iframe reloaded. We must click through before
		// looking for letter keys — otherwise clicks land on the wrong content and are ignored.
		var iSnap string
		for attempt := 0; attempt < 8; attempt++ {
			iSnap, _ = br.SnapshotInteractive(ctx)
			if ref := klublotto.FindRefByName(iSnap, []string{"SPIL ORDKLØVER", "Spil Ordkløver", "SPIL ORDKLOEVER"}); ref != "" {
				fmt.Println("       welcome screen detected in frame, clicking start...")
				_ = br.Click(ctx, ref)
				time.Sleep(1500 * time.Millisecond)
				continue
			}
			// Keyboard ready when a letter button is visible.
			if klublotto.FindRefByName(iSnap, []string{"q", "Q", "a", "A", "e", "E"}) != "" {
				break
			}
			time.Sleep(400 * time.Millisecond)
		}

		// Ensure "Gæt bogstav" (letter-probe) mode is active.
		if ref := klublotto.FindRefByName(iSnap, []string{"Gæt bogstav"}); ref != "" {
			_ = br.Click(ctx, ref)
			time.Sleep(250 * time.Millisecond)
			iSnap, _ = br.SnapshotInteractive(ctx)
		}

		// Click the letter key by ref.
		if ref := klublotto.FindRefByName(iSnap, []string{letter, strings.ToUpper(letter), strings.ToLower(letter)}); ref != "" {
			_ = br.Click(ctx, ref)
		} else {
			// fallback to parent-page interactive click
			_ = clickInteractiveByName(ctx, br, letter)
		}
		time.Sleep(250 * time.Millisecond)
		iSnap, _ = br.SnapshotInteractive(ctx)
		if ref := klublotto.FindRefByName(iSnap, []string{"GÆT", "Gæt"}); ref != "" {
			_ = br.Click(ctx, ref)
		} else {
			_ = br.Press(ctx, "Enter")
		}
		time.Sleep(500 * time.Millisecond)
	} else {
		fmt.Printf("       frame for letter failed (%v); falling back...\n", frameErr)
		if err := clickInteractiveByName(ctx, br, "Gæt bogstav", "Gæt bogstav"); err != nil {
			return fmt.Errorf("select letter mode: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
		if err := clickInteractiveByName(ctx, br, letter); err != nil {
			return fmt.Errorf("click on-screen key: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
		if err := clickInteractiveByName(ctx, br, "GÆT", "Gæt"); err != nil {
			return fmt.Errorf("submit letter: %w", err)
		}
	}
	return nil
}

// submitOrdKloeverLetters probes all given letters by switching into the
// Ordkløver iframe frame context.  The iframe is hosted by Immer Spiele
// (cross-origin), so parent-page snapshots never expose its buttons.
// Switching into the frame via br.Frame gives us direct access to the
// interactive elements (keyboard keys, "SPIL ORDKLØVER", "Gæt bogstav",
// "GÆT") just like the extraction code does.
func submitOrdKloeverLetters(ctx context.Context, br *browser.Client, letters []string) error {
	if len(letters) == 0 {
		return nil
	}
	// Ensure we're on the parent page (don't reload if already there).
	curURL, _ := br.URL(ctx)
	if !strings.Contains(curURL, "danskespil.dk") || !strings.Contains(curURL, "ordkloever") {
		fmt.Println("       navigating to Ordkløver parent page...")
		for i := 0; i < 3; i++ {
			if err := br.Open(ctx, klublotto.OrdKloeverURL); err == nil {
				br.WaitSettled(ctx)
				time.Sleep(600 * time.Millisecond)
				break
			}
			if i < 2 {
				time.Sleep(800 * time.Millisecond)
			}
		}
	}

	// Switch into the iframe frame context so its buttons are accessible.
	// The Ordkløver game is embedded from Immer Spiele (cross-origin), so
	// SnapshotInteractiveWithFrames on the parent does not expose its buttons.
	inFrame := false
	if ferr := klublotto.EnterGameFrame(ctx, br); ferr == nil {
		inFrame = true
		defer klublotto.LeaveFrame(br)
	} else {
		fmt.Printf("       [warn] could not switch to kl-game__iframe: %v; falling back to parent context\n", ferr)
	}

	var snap string
	if inFrame {
		snap, _ = br.SnapshotInteractive(ctx)
	} else {
		snap, _ = br.SnapshotInteractiveWithFrames(ctx)
	}

	// Click through welcome screen if shown.
	if ref := klublotto.FindRefByName(snap, []string{"SPIL ORDKLØVER", "Spil Ordkløver", "SPIL ORDKLOEVER", "spil ordkloever"}); ref != "" {
		fmt.Println("       welcome screen detected, clicking SPIL ORDKLØVER...")
		_ = br.Click(ctx, ref)
		time.Sleep(1500 * time.Millisecond)
		if inFrame {
			snap, _ = br.SnapshotInteractive(ctx)
		} else {
			snap, _ = br.SnapshotInteractiveWithFrames(ctx)
		}
	}

	// Ensure letter-probe mode ("Gæt bogstav").
	if ref := klublotto.FindRefByName(snap, []string{"Gæt bogstav", "GÆT BOGSTAV"}); ref != "" {
		_ = br.Click(ctx, ref)
		time.Sleep(200 * time.Millisecond)
		if inFrame {
			snap, _ = br.SnapshotInteractive(ctx)
		} else {
			snap, _ = br.SnapshotInteractiveWithFrames(ctx)
		}
	}

	snapFn := func() string {
		if inFrame {
			s, _ := br.SnapshotInteractive(ctx)
			return s
		}
		s, _ := br.SnapshotInteractiveWithFrames(ctx)
		return s
	}

	// Click each letter and GÆT.
	for i, letter := range letters {
		letter = klublotto.NormalizeDanishLetters(letter)
		if letter == "" {
			continue
		}
		fmt.Printf("       letter %d/%d: %s\n", i+1, len(letters), letter)
		ref := klublotto.FindRefByName(snap, []string{letter, strings.ToUpper(letter), strings.ToLower(letter)})
		if ref != "" {
			_ = br.Click(ctx, ref)
		} else {
			fmt.Printf("       key %q not found in snapshot (frame=%v)\n", letter, inFrame)
		}
		time.Sleep(200 * time.Millisecond)
		snap = snapFn()
		if ref := klublotto.FindRefByName(snap, []string{"GÆT", "Gæt"}); ref != "" {
			_ = br.Click(ctx, ref)
		} else {
			_ = br.Press(ctx, "Enter")
		}
		time.Sleep(900 * time.Millisecond)
		snap = snapFn()

		// Check for welcome-screen re-appearance between letters.
		if ref := klublotto.FindRefByName(snap, []string{"SPIL ORDKLØVER", "Spil Ordkløver", "SPIL ORDKLOEVER", "spil ordkloever"}); ref != "" {
			fmt.Println("       welcome screen re-appeared; clicking SPIL ORDKLØVER again...")
			_ = br.Click(ctx, ref)
			time.Sleep(1500 * time.Millisecond)
			snap = snapFn()
			// Re-ensure letter mode.
			if ref2 := klublotto.FindRefByName(snap, []string{"Gæt bogstav", "GÆT BOGSTAV"}); ref2 != "" {
				_ = br.Click(ctx, ref2)
				time.Sleep(200 * time.Millisecond)
				snap = snapFn()
			}
		}
	}
	return nil
}

func submitOrdKloever(ctx context.Context, br *browser.Client, answer string) error {
	// Only navigate if not already on the Ordkløver parent page.
	curURL, _ := br.URL(ctx)
	onParent := strings.Contains(curURL, "danskespil.dk") && strings.Contains(curURL, "ordkloever")
	if !onParent {
		fmt.Println("       navigating to Danske Spil Ordkløver parent page...")
		var openErr error
		for i := 0; i < 3; i++ {
			if openErr = br.Open(ctx, klublotto.OrdKloeverURL); openErr == nil {
				br.WaitSettled(ctx)
				time.Sleep(600 * time.Millisecond)
				break
			}
			if i < 2 {
				time.Sleep(800 * time.Millisecond)
			}
		}
		if openErr != nil {
			_ = br.Screenshot(ctx, filepath.Join(".klublotto", "ordkloever-open-fail-"+time.Now().UTC().Format("20060102-150405")+".png"))
			return fmt.Errorf("open parent for known-state full submit: %w", openErr)
		}
		_ = startGameIfNeeded(ctx, br, "SPIL ORDKLØVER", "SPIL ORDKLOEVER")
		_ = focusIntoKloeverGame(ctx, br)
	}

	// Switch into the iframe frame context (cross-origin Immer Spiele embed).
	inFrame := false
	if ferr := klublotto.EnterGameFrame(ctx, br); ferr == nil {
		inFrame = true
		defer klublotto.LeaveFrame(br)
	}

	snapFn := func() string {
		if inFrame {
			s, _ := br.SnapshotInteractive(ctx)
			return s
		}
		s, _ := br.SnapshotInteractiveWithFrames(ctx)
		return s
	}
	snap := snapFn()

	// Click through welcome screen if showing.
	if ref := klublotto.FindRefByName(snap, []string{"SPIL ORDKLØVER", "Spil Ordkløver", "SPIL ORDKLOEVER", "spil ordkloever"}); ref != "" {
		fmt.Println("       welcome screen, clicking SPIL ORDKLØVER...")
		_ = br.Click(ctx, ref)
		time.Sleep(1500 * time.Millisecond)
		snap = snapFn()
	}

	// Switch to "Gæt gåde" (full-phrase) mode — this is the critical mode switch.
	if ref := klublotto.FindRefByName(snap, []string{"Gæt gåde", "GÆT GÅDE", "Gæt gade"}); ref != "" {
		fmt.Println("       switching to Gæt gåde mode...")
		_ = br.Click(ctx, ref)
		time.Sleep(400 * time.Millisecond)
		snap = snapFn()
	}

	// Clear any pending input then type the answer letter by letter on the keyboard.
	// Use NormalizeDanishPhrase so spaces are kept: "SILKEBORG BAD" stays two words.
	// The game knows the word-group boundaries from the shape, so typing all letters
	// continuously (no space key) is correct — it auto-advances between groups.
	clearOrdKloeverPending(ctx, br, answer)
	norm := klublotto.NormalizeDanishPhrase(answer)
	fmt.Printf("       typing answer: %s\n", norm)
	for _, r := range []rune(norm) {
		ch := string(r)
		if ch == " " {
			continue // skip space; game advances between word groups automatically
		}
		ref := klublotto.FindRefByName(snap, []string{ch, strings.ToUpper(ch), strings.ToLower(ch)})
		if ref != "" {
			_ = br.Click(ctx, ref)
		}
		time.Sleep(80 * time.Millisecond)
		snap = snapFn()
	}
	// Submit the full phrase.
	if ref := klublotto.FindRefByName(snap, []string{"GÆT", "Gæt"}); ref != "" {
		_ = br.Click(ctx, ref)
	} else {
		_ = br.Press(ctx, "Enter")
	}
	time.Sleep(2 * time.Second)
	// Switch back to parent before reading result.
	if inFrame {
		klublotto.LeaveFrame(br)
		inFrame = false
	}
	resultSnap, _ := br.Snapshot(ctx)
	if klublotto.IsOrdKloeverWinText(resultSnap) {
		return nil
	}
	raw, _ := br.Eval(ctx, `document.body ? document.body.innerText : ""`)
	if klublotto.IsOrdKloeverWinText(raw) {
		return nil
	}
	// danskespil sometimes replaces the game with its generic crash page after a
	// submit. That is neither a win nor a confirmed wrong guess — flag it
	// distinctly so the caller recovers (reopen + re-extract) instead of treating
	// a nil return as a win or recording a blank/false result off the error page.
	if klublotto.IsDanskeSpilErrorScreen(resultSnap) || klublotto.IsDanskeSpilErrorScreen(raw) {
		return fmt.Errorf("ordkloever: danskespil returned an error screen after submit (\"Der skete en fejl\") — guess not confirmed")
	}
	return fmt.Errorf("ordkloever: guess did not produce a win screen (answer may be wrong)")
}

// recoverFromOrdKloeverError handles danskespil's generic crash page ("Der
// skete en fejl. Prøv igen.") that can replace the Ordkløver game after a
// submit. It returns to the top frame and reopens the parent page + re-enters
// the game, so a follow-up extraction reads the real, server-remembered state
// (which correctly reflects a win or the true remaining attempts) instead of a
// blank board scraped off the error screen.
func recoverFromOrdKloeverError(ctx context.Context, br *browser.Client) {
	klublotto.LeaveFrame(br) // leave any (now-stale) game iframe
	for i := 0; i < 3; i++ {
		if err := br.Open(ctx, klublotto.OrdKloeverURL); err == nil {
			br.WaitSettled(ctx)
			time.Sleep(800 * time.Millisecond)
			break
		}
		if i < 2 {
			time.Sleep(700 * time.Millisecond)
		}
	}
	_ = startGameIfNeeded(ctx, br, "SPIL ORDKLØVER", "SPIL ORDKLOEVER")
	_ = focusIntoKloeverGame(ctx, br)
}

func clearOrdKloeverPending(ctx context.Context, br *browser.Client, answer string) {
	n := len([]rune(klublotto.NormalizeDanishLetters(answer)))
	if n < 20 {
		n = 20
	}
	for i := 0; i < n; i++ {
		_ = br.Press(ctx, "Backspace")
		time.Sleep(30 * time.Millisecond)
	}
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

// focusIntoKloeverGame clicks inside the kl-game iframe rect on the parent page to
// ensure the embedded game has focus (known state) before we switch into its frame
// for kb input. This is resilient if another user has been clicking around or the
// page is in a menu/launcher/partial state.
func focusIntoKloeverGame(ctx context.Context, br *browser.Client) error {
	js := `(() => {
	  const ifr = document.querySelector('iframe.kl-game__iframe, iframe[src*="clover"], iframe[src*="ordkloever"]');
	  if (!ifr) return JSON.stringify({ok:false, error:'no kloever iframe'});
	  const r = ifr.getBoundingClientRect();
	  if (!r || r.width < 50 || r.height < 50) return JSON.stringify({ok:false, error:'tiny iframe'});
	  // Click upper-middle of the game area (typically the rebus tiles or active input zone)
	  // to bring focus into the cross-origin content before Frame().
	  const x = Math.round(r.left + r.width * 0.5);
	  const y = Math.round(r.top + r.height * 0.25);
	  return JSON.stringify({ok:true, x:x, y:y});
	})()`
	raw, err := br.Eval(ctx, js)
	if err != nil {
		return err
	}
	var res struct {
		Ok    bool   `json:"ok"`
		X     int    `json:"x"`
		Y     int    `json:"y"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return err
	}
	if !res.Ok {
		// best effort, do not fail the submit
		return nil
	}
	_ = br.MouseClick(ctx, res.X, res.Y)
	time.Sleep(200 * time.Millisecond)
	return nil
}

// ensureKloeverActive forces the parent page + starts the game if launcher is showing (via -F snap),
// focuses the iframe area, and clicks "Gæt bogstav" to put the UI in the letter-probe starting mode.
// Call this before vision extracts in the probe loop and before key actions so that the persisted
// state (GUESSED from gray kb, BOARD tiles, attempts bar) is visible and stable in the screenshot/crop.
// This counters launcher flicker, other-user clicks, or post-input UI resets that previously caused
// re-extracts to see "NONE / 0/12 / all blank" even when the site had advanced state.
func ensureKloeverActive(ctx context.Context, br *browser.Client) error {
	// Only navigate if we're not already on the Ordkløver parent page.
	// Unnecessary reloads reset the iframe to the welcome screen, causing
	// the next letter probe to land on the wrong content and be silently ignored.
	curURL, _ := br.URL(ctx)
	if !strings.Contains(curURL, "danskespil.dk") || !strings.Contains(curURL, "ordkloever") {
		for i := 0; i < 2; i++ {
			if err := br.Open(ctx, klublotto.OrdKloeverURL); err == nil {
				br.WaitSettled(ctx)
				time.Sleep(400 * time.Millisecond)
				break
			}
			time.Sleep(600 * time.Millisecond)
		}
	}
	_ = startGameIfNeeded(ctx, br, "SPIL ORDKLØVER", "SPIL ORDKLOEVER")
	_ = focusIntoKloeverGame(ctx, br)
	time.Sleep(300 * time.Millisecond)
	return nil
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
