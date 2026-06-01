// Command klub-lotto is the CLI entrypoint.
//
// Subcommands:
//
//	klub-lotto doctor                       sanity-check config, agent-browser, providers
//	klub-lotto login [--check]              open browser, log in, save session state. With --check: just report VALID/INVALID

//	klub-lotto quiz [--dry-run] [--headless] solve today's Quiz
//	klub-lotto ordknude [--headless]        solve today's Ordknuden
//	klub-lotto wiki ingest --file <path>    ingest an arbitrary markdown source
//	klub-lotto wiki import-db --dsn ...     import wiki/daily/*.md into Postgres (DB becomes source of truth)
//	klub-lotto wiki query "<question>"      ask the wiki (shells out to qmd if present)
//	klub-lotto wiki lint                    summarise stale/missing pages
//
// The POC defaults to HEADED mode so you can watch the browser. Pass
// --headless to flip it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/config"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
	"github.com/simonellefsen/klub-lotto/internal/llm"
	"github.com/simonellefsen/klub-lotto/internal/store"
	"github.com/simonellefsen/klub-lotto/internal/wiki"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "doctor":
		err = runDoctor(ctx, args)
	case "login":
		err = runLogin(ctx, args)
	case "quiz":
		err = runQuiz(ctx, args)
	case "ordknude":
		// Temporarily disabled — see internal/klublotto/ordknude.go (build ignore tag)
		fmt.Fprintln(os.Stderr, "ordknude command is temporarily disabled (API drift in solver; see ordknude.go)")
		err = nil
	case "wiki":
		err = runWiki(ctx, args)
	case "ledger":
		err = runLedger(ctx, args)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `klub-lotto — automated Klub Lotto player (PoC)

Usage:
  klub-lotto doctor
  klub-lotto login     [--headless] [--web] [--check]
  klub-lotto quiz      [--headless] [--dry-run] [--submit]
  klub-lotto ordknude  [--headless] [--max-guesses 6] [--provider gemini]
  klub-lotto wiki ingest --file <path>
  klub-lotto wiki import-db --dsn <postgres-url> [--wiki <dir>]
  klub-lotto wiki query "<question>"
  klub-lotto wiki lint
  klub-lotto ledger attach-image --dsn <dsn> --date YYYY-MM-DD --game <slug> --image <path.png|jpg>
                                        attach cropped result screenshot (e.g. filled Krydsord) to ledger row (Postgres truth)

Run klub-lotto <command> -h for command-specific flags.

Default is HEADED mode so you can watch the browser. Pass --headless when
you're confident the flow works and want to schedule it.
`)
}

// ---------------------------------------------------------------------------
// ordknude (temporarily stubbed — see ordknude.go build tag)
// ---------------------------------------------------------------------------

func runOrdknude(ctx context.Context, args []string) error {
	fmt.Fprintln(os.Stderr, "ordknude command is temporarily disabled due to API drift in the solver (internal/klublotto/ordknude.go has //go:build ignore).")
	fmt.Fprintln(os.Stderr, "Use the old binary in ./bin if you need it, or restore the file after fixing the llm/browser calls.")
	return nil
}

// (remaining ordknude body from previous edit removed — ordknude command is now a no-op stub)

// (ordknude helper functions removed — they referenced the old llm/browser
// APIs that no longer exist. The command is stubbed above.)

// ---------------------------------------------------------------------------
// quiz
// ---------------------------------------------------------------------------

func runQuiz(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quiz", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "extract and vote, but do not click the answer")
	submitFlag := fs.Bool("submit", false, "submit the chosen answer (default unless --dry-run)")
	headlessFlag := fs.Bool("headless", false, "force headless browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	headless := *headlessFlag
	if v := os.Getenv("KLUBLOTTO_HEADED"); v != "" {
		headless = strings.EqualFold(v, "false")
	}
	br := browser.New(cfg.BrowserSessionName, !headless)
	restartHeadedSession(ctx, br)

	fmt.Println("[1/6] opening Dagens Quiz...")
	openCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := klublotto.OpenQuiz(openCtx, br); err != nil {
		return fmt.Errorf("open quiz: %w", err)
	}
	curURL, _ := br.URL(openCtx)
	fmt.Println("       at:", curURL)
	if klublotto.IsLoginFlowURL(curURL) {
		if cfg.DanskespilUsername == "" || cfg.DanskespilPassword == "" {
			return fmt.Errorf("login required before quiz can run (landed at %s; no configured Rød Konto username/password)", curURL)
		}
		fmt.Println("       login redirect detected; trying automatic Rød Konto login...")
		ok, needsMitID, err := tryAutomaticRedKontoLogin(openCtx, br, cfg.DanskespilUsername, cfg.DanskespilPassword)
		if err != nil {
			return fmt.Errorf("automatic Rød Konto login before quiz: %w", err)
		}
		if needsMitID {
			return fmt.Errorf("MitID interaction required before quiz can run (landed at %s)", curURL)
		}
		if !ok {
			return fmt.Errorf("automatic Rød Konto login before quiz did not complete")
		}
		if err := klublotto.OpenQuiz(openCtx, br); err != nil {
			return fmt.Errorf("reopen quiz after login: %w", err)
		}
		curURL, _ = br.URL(openCtx)
		fmt.Println("       after login:", curURL)
	}

	fmt.Println("[2/6] snapshotting page...")
	snap, err := br.SnapshotInteractive(openCtx)
	if err != nil {
		return fmt.Errorf("snapshot quiz: %w", err)
	}
	_ = saveDebug(cfg.DataDir, "quiz-snapshot.txt", snap)
	if klublotto.IsLoginRequired(curURL, snap) {
		return fmt.Errorf("login required before quiz can run (landed at %s; snapshot saved to %s)", curURL, filepath.Join(cfg.DataDir, "quiz-snapshot.txt"))
	}

	fmt.Println("[3/6] extracting question and options...")
	round, err := klublotto.ExtractRound(snap)
	if err != nil {
		return fmt.Errorf("extract round: %w", err)
	}
	fmt.Println()
	fmt.Println("== Question ==")
	fmt.Println(round.Prompt)
	fmt.Println()
	fmt.Println("== Options ==")
	for i, opt := range round.Options {
		fmt.Printf("  %d. %s\n", i, opt)
	}

	ps := providers(cfg)
	if len(ps) == 0 {
		return fmt.Errorf("no LLM providers configured")
	}
	fmt.Printf("[4/6] asking %d providers in parallel...\n", len(ps))
	votes := llm.CompareAll(ctx, ps, llm.Question{
		Prompt:  round.Prompt,
		Options: round.Options,
		Context: round.Raw,
	})
	fmt.Println()
	fmt.Println("== Votes ==")
	for _, v := range votes {
		if v.Err != nil {
			fmt.Printf("- %s: ERROR (%s) — %v\n", v.Provider, v.Latency.Round(time.Millisecond), v.Err)
			continue
		}
		option := ""
		if v.Answer.Index >= 0 && v.Answer.Index < len(round.Options) {
			option = round.Options[v.Answer.Index]
		}
		fmt.Printf("- %s: %d %s (%s, %s) — %s\n",
			v.Provider, v.Answer.Index, option, v.Answer.Confidence, v.Latency.Round(time.Millisecond), v.Answer.Rationale)
	}

	fmt.Println("[5/6] deciding...")
	chosen := llm.Majority(votes)
	if chosen < 0 || chosen >= len(round.Options) || chosen >= len(round.OptionRefs) {
		_, _ = writeQuizSource(cfg, round, votes, -1, false, "error: no majority", curURL)
		return fmt.Errorf("no provider returned a usable answer")
	}
	chosenText := round.Options[chosen]
	fmt.Printf("\nMajority choice: %d. %s\n", chosen, chosenText)

	submit := !*dryRun || *submitFlag
	if *dryRun {
		submit = false
	}
	if submit {
		fmt.Println("[6/6] submitting answer...")
		if err := klublotto.Submit(ctx, br, round.OptionRefs[chosen]); err != nil {
			_, _ = writeQuizSource(cfg, round, votes, chosen, false, "error: submit failed", curURL)
			return fmt.Errorf("submit choice: %w", err)
		}
		time.Sleep(1500 * time.Millisecond)
	} else {
		fmt.Println("[6/6] dry run — not clicking.")
	}

	outcome := "submitted"
	if !submit {
		outcome = "dry-run"
	}
	source, err := writeQuizSource(cfg, round, votes, chosen, submit, outcome, curURL)
	if err != nil {
		return err
	}
	fmt.Println("Wiki source page written:", source)
	if err := upsertDailyQuiz(cfg, round.Prompt, chosenText, submit, submit, source); err != nil {
		return err
	}
	return nil
}

func writeQuizSource(cfg *config.Config, round klublotto.QuizRound, votes []llm.Vote, chosen int, submitted bool, outcome, pageURL string) (string, error) {
	now := time.Now().UTC()
	wikiDir := wikiRoot()
	if err := os.MkdirAll(filepath.Join(wikiDir, "sources"), 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("quiz-%s.md", now.Format("20060102-150405"))
	rel := filepath.Join("wiki", "sources", name)
	path := filepath.Join(wikiDir, "sources", name)

	var b strings.Builder
	fmt.Fprintf(&b, "---\nkind: quiz-round\ndate: %s\noutcome: %s\nsubmitted: %v\ntags: [klublotto, quiz, source]\n---\n\n", now.Format(time.RFC3339), outcome, submitted)
	fmt.Fprintf(&b, "# Quiz round — %s\n\n", now.Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "## Question\n\n> %s\n\n", mdCell(round.Prompt))
	fmt.Fprintf(&b, "## Options\n\n")
	for i, opt := range round.Options {
		prefix := "  "
		if i == chosen {
			prefix = "->"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", prefix, i, opt)
	}
	fmt.Fprintf(&b, "\n## Model votes\n\n")
	fmt.Fprintf(&b, "| provider | index | option | confidence | latency | rationale |\n|---|---|---|---|---|---|\n")
	for _, v := range votes {
		if v.Err != nil {
			fmt.Fprintf(&b, "| %s |  |  |  | %s | ERROR: %s |\n", mdCell(v.Provider), v.Latency.Round(time.Millisecond), mdCell(v.Err.Error()))
			continue
		}
		option := ""
		if v.Answer.Index >= 0 && v.Answer.Index < len(round.Options) {
			option = round.Options[v.Answer.Index]
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s |\n",
			mdCell(v.Provider), v.Answer.Index, mdCell(option), mdCell(v.Answer.Confidence), v.Latency.Round(time.Millisecond), mdCell(v.Answer.Rationale))
	}
	chosenText := ""
	if chosen >= 0 && chosen < len(round.Options) {
		chosenText = round.Options[chosen]
	}
	fmt.Fprintf(&b, "\n## Submission\n\n")
	fmt.Fprintf(&b, "- chosen index: %d\n- chosen text: %s\n- submitted: %v\n- outcome: %s\n- page: %s\n\n", chosen, chosenText, submitted, outcome, pageURL)
	fmt.Fprintf(&b, "## See also\n\n- [Quiz game page](../games/quiz.md)\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	_ = cfg
	return rel, nil
}

func upsertDailyQuiz(cfg *config.Config, prompt, answer string, submitted, registered bool, sourceRel string) error {
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
	row := fmt.Sprintf("| Quiz | %s | %s | %s | %s | Source: [%s](../sources/%s). |\n",
		mdCell(prompt), mdCell(answer), yesNo(submitted), yesNo(registered), filepath.Base(sourceRel), filepath.Base(sourceRel))
	if body == "" || !strings.Contains(body, "| Game |") {
		body = fmt.Sprintf("---\nkind: daily-ledger\ndate: %s\ntags: [klublotto, daily-ledger, answers]\nupdated: %s\n---\n\n# Klub Lotto Daily Ledger — %s\n\n## Answers\n\n| Game | Prompt / clue | Answer | Submitted through parent page | Registered on overview | Notes |\n|---|---|---|---:|---:|---|\n%s",
			now.Format("2006-01-02"), now.UTC().Format(time.RFC3339), now.Format("2006-01-02"), row)
	} else {
		lines := strings.Split(body, "\n")
		replaced := false
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "| Quiz |") {
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
	return os.WriteFile(path, []byte(body), 0o644)
}

func wikiRoot() string {
	if v := os.Getenv("KLUBLOTTO_WIKI_DIR"); v != "" {
		return v
	}
	return filepath.Join(repoRoot(), "wiki")
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.TrimSpace(s)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func regexpReplace(s, expr, repl string) string {
	re := regexp.MustCompile(expr)
	if re.MatchString(s) {
		return re.ReplaceAllString(s, repl)
	}
	return s
}

// repoRoot finds the directory holding go.mod by walking up from the
// binary's working directory. Falls back to cwd if not found, which is the
// expected layout when you `go run ./cmd/klub-lotto`.
func repoRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
		}
		dir = parent
	}
}

func loadConfig() (*config.Config, error) {
	return config.Load(repoRoot())
}

// providers returns every LLM provider the user has a key for. Order
// matters: the first provider wins ties in majority voting. We put Claude
// first because in our (limited so far) experience it's the strongest on
// Danish trivia; reorder once we have benchmark data in wiki/concepts/.
func providers(cfg *config.Config) []llm.Provider {
	var out []llm.Provider
	if cfg.AnthropicKey != "" {
		out = append(out, llm.NewAnthropic(cfg.AnthropicKey, ""))
	}
	if cfg.OpenAIKey != "" {
		out = append(out, llm.NewOpenAI(cfg.OpenAIKey, cfg.OpenAIModel))
	}
	if cfg.GeminiKey != "" {
		out = append(out, llm.NewGemini(cfg.GeminiKey, ""))
	}
	if cfg.XAIKey != "" {
		out = append(out, llm.NewXAI(cfg.XAIKey, ""))
	}
	return out
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

func runDoctor(ctx context.Context, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	fmt.Println("== Config ==")
	maskedReport(cfg)

	fmt.Println("\n== agent-browser ==")
	if path, err := exec.LookPath("agent-browser"); err == nil {
		fmt.Println("found:", path)
		out, _ := exec.CommandContext(ctx, "agent-browser", "--version").CombinedOutput()
		fmt.Println("version:", strings.TrimSpace(string(out)))
	} else {
		fmt.Println("NOT FOUND on PATH. Install with: npm i -g agent-browser && agent-browser install")
	}

	fmt.Println("\n== qmd ==")
	if path, err := exec.LookPath("qmd"); err == nil {
		fmt.Println("found:", path)
		out, _ := exec.CommandContext(ctx, "qmd", "status").CombinedOutput()
		fmt.Println(strings.TrimSpace(string(out)))
	} else {
		fmt.Println("not installed (optional). See github.com/simonellefsen/qmd-rust")
	}

	fmt.Println("\n== providers ==")
	ps := providers(cfg)
	if len(ps) == 0 {
		fmt.Println("none configured")
	} else {
		for _, p := range ps {
			fmt.Println("-", p.Name())
		}
	}
	return nil
}

func maskedReport(cfg *config.Config) {
	mask := func(name, v string) {
		if v == "" {
			fmt.Printf("- %s: (empty)\n", name)
			return
		}
		if len(v) < 6 {
			fmt.Printf("- %s: ***\n", name)
			return
		}
		fmt.Printf("- %s: %s***%s (%d chars)\n", name, v[:2], v[len(v)-2:], len(v))
	}
	mask("DANSKESPIL_USERNAME", cfg.DanskespilUsername)
	mask("DANSKESPIL_PASSWORD", cfg.DanskespilPassword)
	mask("OPENAI_API_KEY", cfg.OpenAIKey)
	if cfg.OpenAIModel == "" {
		fmt.Println("- OPENAI_MODEL: (default gpt-5.4)")
	} else {
		fmt.Println("- OPENAI_MODEL:", cfg.OpenAIModel)
	}
	mask("XAI_API_KEY", cfg.XAIKey)
	mask("GEMINI_API_KEY", cfg.GeminiKey)
	mask("ANTHROPIC_API_KEY", cfg.AnthropicKey)
	mask("OPENROUTER_API_KEY", cfg.OpenRouterKey)
	if cfg.OpenRouterModel == "" {
		fmt.Println("- OPENROUTER_MODEL: (default google/gemini-2.5-flash)")
	} else {
		fmt.Println("- OPENROUTER_MODEL:", cfg.OpenRouterModel)
	}
	if cfg.OrdknudeProvider == "" {
		fmt.Println("- ORDKNUDE_PROVIDER: (default gemini)")
	} else {
		fmt.Println("- ORDKNUDE_PROVIDER:", cfg.OrdknudeProvider)
	}
	fmt.Println("- BrowserSessionName:", cfg.BrowserSessionName)
	fmt.Println("- DataDir:", cfg.DataDir)
}

// ---------------------------------------------------------------------------
// login (MitID-assisted for k8s web UI; password flow kept for completeness)
// ---------------------------------------------------------------------------

func runLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	check := fs.Bool("check", false, "machine-readable one-shot probe; prints VALID or INVALID")
	web := fs.Bool("web", false, "web UI mode: no stdin prompts, poll until MitID succeeds in the visible browser")
	headlessFlag := fs.Bool("headless", false, "force headless (probes set this)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// Decide headedness: explicit flag wins, then KLUBLOTTO_HEADED env (web UI sets it per-job),
	// then config default.
	headless := *headlessFlag
	if v := os.Getenv("KLUBLOTTO_HEADED"); v != "" {
		headless = strings.EqualFold(v, "false")
	}

	br := browser.New(cfg.BrowserSessionName, !headless)

	if *check {
		// Fast probe used by web UI auth badge and startup bootstrap.
		// We still do a lightweight navigation + snapshot so we get an accurate
		// live answer even if cookies are stale. Do not close the agent-browser
		// session here: the k8s UI may have a visible VNC login browser open in
		// the same session.
		openCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		if err := br.Open(openCtx, klublotto.KlubLottoURL); err != nil {
			fmt.Println("INVALID")
			return fmt.Errorf("open for check: %w", err)
		}
		_ = br.WaitForLoad(openCtx, "domcontentloaded")
		ok, _ := klublotto.IsLoggedIn(openCtx, br)
		if ok {
			fmt.Println("VALID")
			fmt.Println("already logged in")
			return nil
		}
		fmt.Println("INVALID")
		return fmt.Errorf("session not valid")
	}

	// Normal login flow or --web (the one the "Trigger MitID login" button uses).
	fmt.Fprintf(os.Stderr, "login: starting browser (headed=%v, web=%v) session=%s\n", !headless, *web, cfg.BrowserSessionName)
	restartHeadedSession(ctx, br)

	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := br.Open(openCtx, klublotto.KlubLottoURL); err != nil {
		return fmt.Errorf("open klublotto: %w", err)
	}
	_ = br.WaitForLoad(openCtx, "networkidle")

	if ok, _ := klublotto.IsLoggedIn(openCtx, br); ok {
		fmt.Println("already logged in")
		fmt.Println("VALID")
		fmt.Println("authenticated")
		return nil
	}

	if cfg.DanskespilUsername != "" && cfg.DanskespilPassword != "" {
		fmt.Println("Trying automatic Rød Konto login with configured credentials...")
		autoCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		ok, needsMitID, err := tryAutomaticRedKontoLogin(autoCtx, br, cfg.DanskespilUsername, cfg.DanskespilPassword)
		if ok {
			fmt.Println("VALID")
			fmt.Println("authenticated")
			return nil
		}
		if err != nil {
			fmt.Println("Automatic Rød Konto login did not complete:", err)
		}
		if needsMitID {
			fmt.Println("Automatic Rød Konto login reached MitID/NemLog-in; user interaction is required.")
		}
	} else {
		fmt.Println("No configured Rød Konto username/password; automatic login is unavailable.")
	}

	if !*web {
		// Interactive / legacy password-assisted path (rarely used now that MitID is primary).
		// If creds are present we could call the old klublotto.Login, but for the k8s
		// MitID use case we always go through the web button which passes --web.
		fmt.Println("Not currently logged in.")
		fmt.Println("For MitID login with live VNC, click 'Trigger MitID login' in the web UI (it passes --web).")
		if cfg.DanskespilUsername != "" && cfg.DanskespilPassword != "" {
			fmt.Println("(Password credentials are present in config but MitID is the supported path.)")
		}
		return fmt.Errorf("not logged in; use --web via UI for MitID")
	}

	// --web mode: if the automatic Rød Konto path reached MitID/NemLog-in,
	// the browser is now visible on the pod's Xvfb :99 (and thus in noVNC).
	// The human completes only that MitID handoff. Once MitID returns to the
	// Rød Konto username/password form, the green UI button can submit the
	// saved credentials and verify the final Klub Lotto session.
	fmt.Println("MitID login mode active (--web).")
	fmt.Println("Headed browser is now visible in the VNC.")
	fmt.Println("If MitID is shown, complete it. If the Rød Konto form or account drawer is shown, click the green verify button in the UI.")

	// Manual-only mode. We trust the button. Keep the desktop alive.
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			_ = exec.Command("xsetroot", "-solid", "#1e1e2e").Run()
			time.Sleep(30 * time.Second)
		}
	}

	fmt.Println("MitID login timed out waiting for manual confirmation.")
	return fmt.Errorf("MitID login timed out (30 min)")
}

func tryAutomaticRedKontoLogin(ctx context.Context, br *browser.Client, username, password string) (ok bool, needsMitID bool, err error) {
	if err := br.Open(ctx, klublotto.KlubLottoURL); err != nil {
		return false, false, fmt.Errorf("open klublotto: %w", err)
	}
	_ = br.WaitForLoad(ctx, "domcontentloaded")

	clickedLogin := false
	submittedRedKonto := false
	var submittedRedKontoAt time.Time
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		cur, _ := br.URL(ctx)
		if klublotto.IsMitIDHandoffURL(cur) {
			return false, true, nil
		}
		if visible, err := klublotto.IsRedKontoLoginPage(ctx, br); err != nil {
			return false, false, err
		} else if visible && submittedRedKonto {
			if time.Since(submittedRedKontoAt) > 20*time.Second {
				return false, false, fmt.Errorf("Rød Konto login form is still visible after one automatic submission; refusing to retry")
			}
			time.Sleep(1 * time.Second)
			continue
		}
		if visible, err := klublotto.CompleteRedKontoIfVisible(ctx, br, username, password); err != nil {
			return false, false, err
		} else if visible {
			submittedRedKonto = true
			submittedRedKontoAt = time.Now()
			fmt.Println("Submitted Rød Konto username/password; waiting for Klub Lotto session...")
			time.Sleep(2 * time.Second)
			continue
		}
		if ok, _ := klublotto.IsLoggedIn(ctx, br); ok {
			return true, false, nil
		}
		if !clickedLogin {
			clicked, err := klublotto.ClickLoginEntryIfVisible(ctx, br)
			if err != nil {
				return false, false, fmt.Errorf("click login entry: %w", err)
			}
			if clicked {
				clickedLogin = true
				_ = br.WaitForLoad(ctx, "domcontentloaded")
				time.Sleep(2 * time.Second)
				continue
			}
		}
		time.Sleep(1 * time.Second)
	}
	return false, false, fmt.Errorf("timed out waiting for Rød Konto login to complete")
}

func restartHeadedSession(ctx context.Context, br *browser.Client) {
	if !br.Headed {
		return
	}
	closeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = br.Close(closeCtx)
	time.Sleep(1500 * time.Millisecond)
}

func saveDebug(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

// ---------------------------------------------------------------------------
// wiki
// ---------------------------------------------------------------------------

func runWiki(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("wiki: missing subcommand (ingest|query|lint)")
	}
	root := filepath.Join(repoRoot(), "wiki")
	w, err := wiki.New(root)
	if err != nil {
		return err
	}

	switch args[0] {
	case "ingest":
		fs := flag.NewFlagSet("wiki ingest", flag.ExitOnError)
		file := fs.String("file", "", "path to a markdown file to copy into wiki/sources/")
		_ = fs.Parse(args[1:])
		if *file == "" {
			return fmt.Errorf("wiki ingest: --file required")
		}
		body, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, "sources", filepath.Base(*file))
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return err
		}
		_ = w.LogQuery("manual ingest "+filepath.Base(*file), "filed to sources/")
		fmt.Println("ingested:", dst)
		return nil

	case "import-db":
		// One-time (or periodic) bridge from the wiki/daily markdown files
		// into Postgres. After this the DB (daily_ledger + runs) is the
		// source of truth; the wiki files become derived/historical output.
		fs := flag.NewFlagSet("wiki import-db", flag.ExitOnError)
		dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "Postgres DSN (or set DATABASE_URL)")
		wikiDir := fs.String("wiki", root, "root of the wiki tree containing daily/ and sources/")
		_ = fs.Parse(args[1:])
		if *dsn == "" {
			return fmt.Errorf("wiki import-db: --dsn (or DATABASE_URL) is required")
		}
		st, err := store.New(ctx, *dsn)
		if err != nil {
			return fmt.Errorf("connect to postgres: %w", err)
		}
		defer st.Close()

		warnings, err := st.ImportWikiDaily(ctx, *wikiDir)
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}

		fmt.Printf("klub-lotto wiki import-db: target = %s\n", *wikiDir)

		if len(warnings) > 0 {
			fmt.Println("\nNotes / warnings from import:")
			for _, w := range warnings {
				fmt.Println("  ", w)
			}
		}

		// Give the user a quick way to verify
		fmt.Println("\nTo verify what is now in Postgres, look at the Daily ledger section in the web UI,")
		fmt.Println("or run: kubectl -n klub-lotto exec deploy/klub-lotto -- psql -U klublotto -c 'SELECT date, game_slug, answer FROM daily_ledger ORDER BY date DESC;'")
		return nil

	case "query":
		if len(args) < 2 {
			return fmt.Errorf("wiki query: missing question")
		}
		q := strings.Join(args[1:], " ")
		// Prefer qmd if installed; otherwise fall back to substring grep.
		if _, err := exec.LookPath("qmd"); err == nil {
			cmd := exec.CommandContext(ctx, "qmd", "search", q)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			_ = cmd.Run()
		} else {
			fmt.Println("(qmd not installed; using naive grep)")
			cmd := exec.CommandContext(ctx, "grep", "-rni", "--", q, root)
			cmd.Stdout = os.Stdout
			_ = cmd.Run()
		}
		_ = w.LogQuery(q, "see stdout")
		return nil

	case "lint":
		// Naive lint: warn on orphan source pages (no incoming reference
		// in any other md), surface most recent log entries.
		fmt.Println("wiki lint — not yet implemented; see RUN.md for the planned checks")
		_ = w.LogLint("manual lint invocation")
		return nil

	default:
		return fmt.Errorf("wiki: unknown subcommand %q", args[0])
	}
}

// ---------------------------------------------------------------------------
// ledger (attach-image etc. for visual results like cropped Krydsord grids)
// ---------------------------------------------------------------------------

func runLedger(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("ledger: subcommand required (attach-image)")
	}
	switch args[0] {
	case "attach-image":
		return runLedgerAttachImage(ctx, args[1:])
	default:
		return fmt.Errorf("ledger: unknown subcommand %q", args[0])
	}
}

func runLedgerAttachImage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ledger attach-image", flag.ExitOnError)
	dsn := fs.String("dsn", "", "postgres dsn (required; or set DATABASE_URL)")
	dateStr := fs.String("date", "", "YYYY-MM-DD of the daily ledger row (required)")
	game := fs.String("game", "", "game slug e.g. krydsord (required)")
	imgPath := fs.String("image", "", "path to cropped PNG/JPEG of the completed board (required)")
	_ = fs.Parse(args)

	if *dsn == "" {
		*dsn = os.Getenv("DATABASE_URL")
	}
	if *dsn == "" {
		return fmt.Errorf("--dsn or DATABASE_URL required")
	}
	if *dateStr == "" || *game == "" || *imgPath == "" {
		return fmt.Errorf("--date, --game and --image are required")
	}
	d, err := time.Parse("2006-01-02", *dateStr)
	if err != nil {
		return fmt.Errorf("bad --date: %w", err)
	}
	data, err := os.ReadFile(*imgPath)
	if err != nil {
		return fmt.Errorf("read image: %w", err)
	}

	st, err := store.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	// Find the ledger id for (date, game)
	var id int64
	err = st.Pool.QueryRow(ctx, `SELECT id FROM daily_ledger WHERE date = $1 AND game_slug = $2`, d, *game).Scan(&id)
	if err != nil {
		return fmt.Errorf("find ledger for %s/%s: %w (did you run the game or import first?)", *dateStr, *game, err)
	}

	if err := st.SetResultImage(ctx, id, data); err != nil {
		return err
	}
	fmt.Printf("attached %d bytes as result_image for ledger id=%d (%s %s)\n", len(data), id, *dateStr, *game)
	return nil
}
