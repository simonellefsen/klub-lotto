package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/config"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
	"github.com/simonellefsen/klub-lotto/internal/llm"
	"github.com/simonellefsen/klub-lotto/internal/store"
	"github.com/simonellefsen/klub-lotto/internal/wiki"
)

func runSudoku(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sudoku", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "extract and solve, but do not submit")
	submitFlag := fs.Bool("submit", false, "submit the solved grid through the parent page")
	headlessFlag := fs.Bool("headless", false, "force headless browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	br := gameBrowser(cfg, *headlessFlag)
	restartHeadedSession(ctx, br)

	fmt.Println("[1/4] opening Dagens Sudoku...")
	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	err = openGameWithLogin(openCtx, br, cfg, klublotto.OpenSudoku)
	curURL, _ := br.URL(openCtx)
	cancel()
	if err != nil {
		return err
	}
	fmt.Println("       at:", curURL)

	fmt.Println("[2/4] extracting givens...")
	extractCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	givens, _, err := klublotto.ExtractSudokuGivens(extractCtx, br)
	cancel()
	if err != nil {
		return err
	}
	_ = saveDebug(cfg.DataDir, "sudoku-givens.txt", klublotto.FormatSudokuGrid(givens)+"\n")

	fmt.Println("[3/4] solving locally...")
	solved, ok := klublotto.SolveSudoku(givens)
	if !ok {
		return fmt.Errorf("sudoku has no solution")
	}
	_ = saveDebug(cfg.DataDir, "sudoku-solution.txt", klublotto.FormatSudokuGrid(solved)+"\n")
	fmt.Println()
	fmt.Println("== Givens ==")
	fmt.Println(klublotto.FormatSudokuGrid(givens))
	fmt.Println()
	fmt.Println("== Solution ==")
	fmt.Println(klublotto.FormatSudokuGrid(solved))

	submit := *submitFlag && !*dryRun
	if !submit {
		fmt.Println("[4/4] dry run — not submitting.")
		return nil
	}

	fmt.Println("[4/4] submitting through parent page...")
	submitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	err = submitSudoku(submitCtx, br, givens, solved)
	cancel()
	if err != nil {
		return err
	}
	shot := filepath.Join(cfg.DataDir, "sudoku-result-"+time.Now().UTC().Format("20060102-150405")+".png")
	_ = br.Screenshot(ctx, shot)
	return upsertDailyGame(ctx, cfg, "Sudoku", "9x9 Sudoku", gridOneLine(solved), true, true, "Solved with deterministic local compute. Screenshot: `"+shot+"`.")
}

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
							if v, ok := j["CATEGORY"].(string); ok && v != "" && !strings.EqualFold(v, "Not visible") { st.Category = v }
							if v, ok := j["HINT"].(string); ok && v != "" && !strings.EqualFold(v, "Not visible") { st.Hint = v }
							if v, ok := j["SHAPE"].(string); ok && v != "" && !strings.EqualFold(v, "Unknown") { st.Shape = v; if st.VisualShape == "" { st.VisualShape = v } }
							if v, ok := j["BOARD"].(string); ok && v != "" { st.Board = v; if st.VisualBoard == "" { st.VisualBoard = v } }
							if v, ok := j["GUESSED"].(string); ok && v != "" { st.GuessedLetters = klublotto.CleanGuessedLetters(v) }
							if v, ok := j["ATTEMPTS"].(string); ok && st.Attempts == 0 {
								if idx := strings.Index(v, "/"); idx > 0 {
									if n, _ := strconv.Atoi(strings.TrimSpace(v[:idx])); n > 0 { st.Attempts = n }
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
		return upsertDailyGame(ctx, cfg, "Ordkløver", ordKloeverPrompt(st), "SOLVED", true, true, "Already besvaret / finished on page. Screenshot: `"+shot+"`.")
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
		return upsertDailyGame(ctx, cfg, "Ordkløver", ordKloeverPrompt(st), answer, true, true, "Already solved on page. Screenshot: `"+shot+"`.")
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
	return upsertDailyGame(ctx, cfg, "Ordkløver", ordKloeverPrompt(st), answer, true, true, "Submitted through parent page. Screenshot: `"+shot+"`.")
}

func runOrdknude(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ordknude", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "extract and ask for candidates, but do not submit (real play with --answer commits guesses permanently; no do-overs)")
	extractOnly := fs.Bool("extract-only", false, "extract state only; do not call a provider or submit")
	submitFlag := fs.Bool("submit", false, "submit the explicit --answer through the parent page (real play; guesses are permanent with no do-overs)")
	answerFlag := fs.String("answer", "", "exact five-letter Danish word to submit")
	providerFlag := fs.String("provider", "", "word provider: gemini|openai|xai|anthropic|openrouter")
	headlessFlag := fs.Bool("headless", false, "force headless browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Vision provider for reading board tile colours from screenshots.
	// Prefer Gemini 2.5 Pro for Ordknude colour reading — it has better
	// colour discrimination for the dark-maroon/orange/green tile shades.
	// Fall back to Claude Haiku if only an Anthropic key is available.
	var ac llm.VisionProvider
	if cfg.GeminiKey != "" {
		ac = llm.NewGemini(cfg.GeminiKey, "gemini-2.5-pro")
		fmt.Println("       vision: gemini-2.5-pro")
	} else if cfg.AnthropicKey != "" {
		ac = llm.NewAnthropic(cfg.AnthropicKey, "claude-haiku-4-5-20251001")
		fmt.Println("       vision: claude-haiku-4-5-20251001")
	} else {
		fmt.Println("       WARNING: no vision API key (GEMINI_API_KEY or ANTHROPIC_API_KEY); colour marks will be absent.")
	}
	br := gameBrowser(cfg, *headlessFlag)
	restartHeadedSession(ctx, br)

	fmt.Println("[1/4] opening Dagens Ordknuden...")
	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	err = openGameWithLogin(openCtx, br, cfg, klublotto.OpenOrdknude)
	curURL, _ := br.URL(openCtx)
	cancel()
	if err != nil {
		return err
	}
	fmt.Println("       at:", curURL)

	fmt.Println("[2/4] extracting state...")
	extractCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	st, err := klublotto.ExtractOrdknudeState(extractCtx, br, ac)
	cancel()
	if err != nil {
		return err
	}
	_ = saveDebug(cfg.DataDir, "ordknude-state.txt", st.Raw)
	printOrdknudeState(st)
	if *extractOnly {
		fmt.Println("[3/4] extract only — not asking provider.")
		fmt.Println("[4/4] not submitting (extract-only).")
		return nil
	}
	if st.Solved {
		answer := klublotto.NormalizeDanishLetters(*answerFlag)
		if answer == "" && st.Answer != "RIGTI" {
			answer = klublotto.NormalizeDanishLetters(st.Answer)
		}
		if answer == "" {
			answer = "SOLVED"
		}
		shot := filepath.Join(cfg.DataDir, "ordknude-result-"+time.Now().UTC().Format("20060102-150405")+".png")
		_ = br.Screenshot(ctx, shot)
		return upsertDailyGame(ctx, cfg, "Ordknuden", "5-letter Danish word puzzle", answer, true, true, "Already solved on page. Screenshot: `"+shot+"`.")
	}
	rejected := klublotto.LoadRejectedWords(cfg.DataDir)

	answer := klublotto.NormalizeDanishLetters(*answerFlag)
	if answer == "" && !*dryRun && !*extractOnly {
		// AUTO-PLAY MODE (bare `make ordknude` or no --answer): we start from a
		// blank sheet (or whatever history the board shows) and keep guessing
		// real 5-letter Danish words (submitting via the parent page so the
		// checkmark + lod registers) until we get it right or run out of attempts.
		// Danske Spil remembers the state across runs/sessions for the day, so we
		// continue from persisted history (e.g. if SPROG was already tried).
		// This is "for real" — no dry-runs, no do-overs, guesses are permanent.
		fmt.Println("[3/4] auto-playing Ordknuden (LLM proposes next guess from current feedback; real submits on parent, no do-overs)...")
		if len(st.History) > 0 || st.Remaining < 6 {
			fmt.Printf("   (continuing from persisted Danske Spil state: %d guesses already made, %d remaining)\n", len(st.History), st.Remaining)
		}
		attemptsThisRun := 0
		lastSubmittedAnswer := "" // tracks the most recent word submitted (for end-of-game recording)
		triedThisRun := []string{} // words submitted in this run (guards against re-suggest when re-extract fails)
		// pool holds DDO-valid candidates from the most recent LLM call that we
		// haven't tried yet. We reuse it across wrong guesses — picking another at
		// random and re-querying the LLM only when the pool is empty — so a tightly
		// constrained board (e.g. only one letter unknown) doesn't pay for a fresh
		// LLM round on every attempt.
		var pool []klublotto.WordCandidate
		// lastGoodHistory keeps the most recent non-empty board history: the win/
		// loss overlay re-extract returns "0 guesses", wiping st.History, so we
		// snapshot it here to reconstruct the guess sequence for the ledger.
		var lastGoodHistory []klublotto.OrdknudeGuess
		// prunePool drops tried/rejected/invalid words and any no longer consistent
		// with the observed marks (a wrong guess tightens the constraints).
		prunePool := func(in []klublotto.WordCandidate) []klublotto.WordCandidate {
			var out []klublotto.WordCandidate
			for _, c := range filterOrdknudeCandidates(in, st, rejected) {
				if klublotto.ConsistentWithOrdknudeHistory(c.Answer, st.History) {
					out = append(out, c)
				}
			}
			return out
		}
		maxForDay := st.Remaining
		if maxForDay <= 0 || maxForDay > 6 {
			maxForDay = 6
		}
		for {
			if len(st.History) > 0 {
				lastGoodHistory = st.History // snapshot before a win/loss overlay wipes it
			}
			if st.Solved || st.Remaining <= 0 || attemptsThisRun >= maxForDay || attemptsThisRun >= 6 {
				break
			}
			// Honour Ctrl-C / SIGTERM at the top of every iteration.
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			// Use attemptsThisRun+1 as display attempt (st.History may be stale if re-extract failed).
			currentAttempt := attemptsThisRun + 1
			if histLen := len(st.History) + 1; histLen > currentAttempt {
				currentAttempt = histLen // trust history when it's longer
			}
			// Prune any leftover pool against the now-current constraints before
			// deciding whether we still need to ask the LLM.
			pool = prunePool(pool)
			currentAnswer := ""
			if len(pool) > 0 {
				// Reuse the existing pool — no new LLM call needed. Pick at random.
				idx := rand.Intn(len(pool))
				pick := pool[idx]
				pool = append(pool[:idx], pool[idx+1:]...)
				currentAnswer = klublotto.NormalizeDanishLetters(pick.Answer)
				fmt.Printf("   -> trying %s (from pool; %d candidate(s) left, no LLM call)\n", currentAnswer, len(pool))
			} else {
				fmt.Printf("[3/4] asking provider for candidates (attempt %d/6)...\n", currentAttempt)
				cands, err := wordCandidates(ctx, cfg, *providerFlag, klublotto.BuildOrdknudePrompt(st, rejected))
				if err != nil {
					if fb := klublotto.FallbackOrdknudeGuess(st.History, rejected); fb != "" {
						fmt.Printf("   provider failed (%v), falling back to local guess: %s\n", err, fb)
						currentAnswer = fb
						fmt.Printf("   -> trying fallback %s\n", currentAnswer)
					} else {
						return fmt.Errorf("get next guess (attempt %d): %w", currentAttempt, err)
					}
				} else {
					// Filter: remove non-5-letter, already-tried, rejected, and duplicates within the batch.
					beforeDDO := filterOrdknudeCandidates(cands, st, rejected)
					// Validate remaining candidates against ordnet.dk/ddo (drop non-Danish words).
					validated := beforeDDO
					if len(validated) > 0 {
						ddoCtx, ddoCancel := context.WithTimeout(ctx, 30*time.Second)
						validated = klublotto.FilterDDOWords(ddoCtx, validated)
						ddoCancel()
					}
					if len(validated) == 0 {
						// Add DDO-dropped words to rejected so the LLM doesn't re-suggest them.
						for _, c := range beforeDDO {
							w := klublotto.NormalizeDanishLetters(c.Answer)
							if !containsWord(rejected, w) {
								rejected = append(rejected, w)
							}
						}
						if fb := klublotto.FallbackOrdknudeGuess(st.History, rejected); fb != "" {
							fmt.Printf("   provider gave no valid candidates, falling back to local guess: %s\n", fb)
							currentAnswer = fb
							fmt.Printf("   -> trying fallback %s\n", currentAnswer)
						} else {
							fmt.Printf("   all candidates dropped by DDO — retrying LLM for new suggestions...\n")
							continue
						}
					} else {
						// Keep only constraint-consistent words as the reusable pool.
						// If that over-prunes (e.g. a mark was mis-read), fall back to
						// the full DDO-valid set rather than discarding everything.
						pool = prunePool(validated)
						if len(pool) == 0 {
							pool = validated
						}
						printCandidates(pool)
						idx := rand.Intn(len(pool))
						pick := pool[idx]
						pool = append(pool[:idx], pool[idx+1:]...)
						currentAnswer = klublotto.NormalizeDanishLetters(pick.Answer)
						fmt.Printf("   -> trying %s (%s) — %s\n", currentAnswer, pick.Confidence, pick.Rationale)
					}
				}
			}

			if !klublotto.IsDanishFiveLetterWord(currentAnswer) {
				fmt.Println("   invalid 5-letter Danish word from provider, asking again...")
				continue
			}
			if containsWord(rejected, currentAnswer) {
				fmt.Printf("   %s previously rejected, asking provider for different guess...\n", currentAnswer)
				continue
			}
			if alreadyTried(currentAnswer, st.History) {
				fmt.Printf("   %s already tried in this game (persisted state), asking again...\n", currentAnswer)
				continue
			}
			if containsWord(triedThisRun, currentAnswer) {
				fmt.Printf("   %s already submitted this run (re-extract may have missed it), asking again...\n", currentAnswer)
				continue
			}

			fmt.Printf("[4/4] submitting %s (real play, permanent, attempt %d/6, no do-overs)...\n", currentAnswer, currentAttempt)
			preShot := filepath.Join(cfg.DataDir, "ordknude-pre-"+currentAnswer+"-"+time.Now().UTC().Format("20060102-150405")+".png")
			_ = br.Screenshot(ctx, preShot)
			submitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			// needStart = true only if neither st.History nor triedThisRun has any entries
			// (i.e. no guess has ever been made in this session). Avoids clicking "SPIL ORDKNUDEN"
			// on attempts 2+ even when re-extract mistakenly returns 0 history.
			needStart := (len(st.History) == 0 && len(triedThisRun) == 0)
			outcome, err := submitOrdknude(submitCtx, br, currentAnswer, needStart)
			cancel()
			lastSubmittedAnswer = currentAnswer
			if strings.Contains(strings.ToLower(outcome), "ordet findes ikke") {
				_ = klublotto.RecordRejectedWord(cfg.DataDir, currentAnswer)
				rejected = append(rejected, currentAnswer)
				// re-extract (the bad guess usually isn't recorded in history)
				extractCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if newSt, e := klublotto.ExtractOrdknudeState(extractCtx, br, ac); e == nil {
					st = newSt
					_ = saveDebug(cfg.DataDir, "ordknude-state.txt", st.Raw)
					printOrdknudeState(st)
				}
				cancel()
				continue
			}
			if err != nil {
				if strings.Contains(err.Error(), "open parent to ensure") || strings.Contains(err.Error(), "ERR_ABORTED") || strings.Contains(err.Error(), "net::") || strings.Contains(err.Error(), "context deadline") {
					fmt.Printf("   transient browser/network error during submit (%v); re-extracting and will retry a guess...\n", err)
					time.Sleep(2000 * time.Millisecond)
					extractCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					if newSt, e := klublotto.ExtractOrdknudeState(extractCtx, br, ac); e == nil {
						st = newSt
						_ = saveDebug(cfg.DataDir, "ordknude-state.txt", st.Raw)
						printOrdknudeState(st)
					}
					cancel()
					continue
				}
				return err
			}
			attemptsThisRun++
			triedThisRun = append(triedThisRun, currentAnswer)

			time.Sleep(2500 * time.Millisecond) // let tile flip animation fully complete before scraping marks

			// Re-extract so the next LLM prompt sees the actual marks (green/yellow/gray)
			// for the guess we just made, and to detect if we won.
			extractCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if newSt, e := klublotto.ExtractOrdknudeState(extractCtx, br, ac); e == nil {
				st = newSt
				_ = saveDebug(cfg.DataDir, "ordknude-state.txt", st.Raw)
				printOrdknudeState(st)
			} else {
				fmt.Printf("warning: re-extract after guess failed: %v\n", e)
			}
			cancel()

			outLow := strings.ToLower(outcome)
			if strings.Contains(outLow, "tillykke") ||
				strings.Contains(outLow, "super imponerende") || // win screen: "Super imponerende!"
				strings.Contains(outLow, "du fandt frem til") || // win screen: "Du fandt frem til dagens ord"
				strings.Contains(outLow, "ord-haj") { // win screen: "Du er en sand ord-haj!"
				// NOTE: bare "vundet" intentionally omitted — page nav permanently contains "vundet eller tabt".
				// NOTE: "dagens første lod" intentionally omitted — appears after ANY game earns the lod.
				st.Solved = true
			}

			// Extra guarantee we are on the parent (embedded) before the next LLM call or submit.
			// The extract above tries to restore, but we force it here too to avoid flicker-related
			// or restore-failure issues leaving us on the raw immerspiele URL.
			if u, _ := br.URL(ctx); isImmerspieleURL(u) || !strings.Contains(u, "danskespil.dk") {
				fmt.Println("       post-extract force to parent page for next action...")
				if err := br.Open(ctx, klublotto.OrdknudeURL); err == nil {
					_ = br.WaitForLoad(ctx, "networkidle")
					time.Sleep(800 * time.Millisecond)
				}
			}

			shot := filepath.Join(cfg.DataDir, "ordknude-attempt-"+time.Now().UTC().Format("20060102-150405")+".png")
			_ = br.Screenshot(ctx, shot)

			if st.Solved {
				finalShot := filepath.Join(cfg.DataDir, "ordknude-result-"+time.Now().UTC().Format("20060102-150405")+".png")
				_ = br.Screenshot(ctx, finalShot)
				fmt.Printf("\n🎉 SOLVED! Ordknuden answer: %s (attempt %d/6)\n\n", currentAnswer, currentAttempt)
				notes := ordknudeGuessNotes(mergeGuessWords(lastGoodHistory, triedThisRun), lastGoodHistory, currentAnswer)
				if notes == "" {
					notes = "Auto-solved by repeated real LLM-guided guesses on parent page."
				}
				return upsertDailyGame(ctx, cfg, "Ordknuden", "5-letter Danish word puzzle", currentAnswer, true, true, notes)
			}
			// loop: next iteration will ask LLM again with the updated history+marks in the prompt
		}

		// finished (solved or out of attempts) — read result screen for correct answer
		shot := filepath.Join(cfg.DataDir, "ordknude-result-"+time.Now().UTC().Format("20060102-150405")+".png")
		_ = br.Screenshot(ctx, shot)

		// Post-loop win-screen check: when the win overlay replaces the game board the
		// re-extract returns "0 guesses, 0 remaining" (empty state) so st.Solved is never
		// set inside the loop.  Take a fresh snapshot and look for the overlay text.
		if !st.Solved {
			if pageSnap, snapErr := br.Snapshot(ctx); snapErr == nil {
				pageLow := strings.ToLower(pageSnap)
				if strings.Contains(pageLow, "super imponerende") ||
					strings.Contains(pageLow, "du fandt frem til") ||
					strings.Contains(pageLow, "ord-haj") ||
					strings.Contains(pageLow, "tillykke") {
					fmt.Println("   win overlay detected on post-loop snapshot — marking as solved")
					st.Solved = true
					if st.Answer == "" {
						st.Answer = lastSubmittedAnswer // the answer that triggered the win
					}
				}
			}
		}

		correctAnswer := st.Answer // may be set if solved
		if correctAnswer == "" {
			// Try to read "Det rigtige svar var: <answer>" from the result page.
			if resultSnap, snapErr := br.Snapshot(ctx); snapErr == nil {
				correctAnswer = extractOrdknudeAnswerFromSnap(resultSnap)
				if correctAnswer != "" {
					fmt.Printf("   correct answer was: %s\n", correctAnswer)
				}
			}
		}

		msg := "Auto-play finished"
		if st.Solved {
			msg = fmt.Sprintf("Solved! Answer: %s", correctAnswer)
			fmt.Printf("\n🎉 SOLVED! Ordknuden answer: %s\n\n", correctAnswer)
		} else {
			guessCount := attemptsThisRun
			if guessCount == 0 {
				guessCount = len(st.History)
			}
			if correctAnswer != "" {
				msg = fmt.Sprintf("Not solved after %d guesses. Correct answer was: %s", guessCount, correctAnswer)
				fmt.Printf("\n❌ Not solved after %d guesses. Correct answer was: %s\n\n", guessCount, correctAnswer)
			} else {
				msg = fmt.Sprintf("Not solved after %d guesses (out of 6 attempts)", guessCount)
				fmt.Printf("\n❌ Not solved after %d guesses (out of 6 attempts)\n\n", guessCount)
			}
		}
		recordedAnswer := correctAnswer
		if recordedAnswer == "" {
			recordedAnswer = lastSubmittedAnswer // last guess attempted
		}
		// Notes: the colour-coded guess sequence (scored against the real answer),
		// plus a loss tag when we didn't solve it. Falls back to the plain message.
		notes := msg + ". Screenshot: `" + shot + "`."
		if seq := ordknudeGuessNotes(mergeGuessWords(lastGoodHistory, triedThisRun), lastGoodHistory, recordedAnswer); seq != "" {
			if st.Solved {
				notes = seq
			} else if correctAnswer != "" {
				notes = seq + " · Ikke løst — korrekt svar: " + correctAnswer
			} else {
				notes = seq + " · Ikke løst"
			}
		}
		return upsertDailyGame(ctx, cfg, "Ordknuden", "5-letter Danish word puzzle", recordedAnswer, true, st.Solved, notes)
	}

	// Explicit --answer path (or --dry-run / --extract-only with no answer):
	// show candidates once for dry, or do the single real submit.
	if answer == "" {
		fmt.Println("[3/4] asking provider for ranked Danish candidates...")
		cands, err := wordCandidates(ctx, cfg, *providerFlag, klublotto.BuildOrdknudePrompt(st, rejected))
		if err != nil {
			if *dryRun {
				fmt.Println("Provider unavailable:", err)
				fmt.Println("[4/4] extracted state only (dry-run); not submitting.")
				return nil
			}
			return err
		}
		printCandidates(cands)
		fmt.Println("[4/4] not submitting (dry-run or no explicit --answer for real play; submissions are permanent with no do-overs).")
		return nil
	}
	if !klublotto.IsDanishFiveLetterWord(answer) {
		return fmt.Errorf("--answer must be exactly five Danish letters A-ZÆØÅ, got %q", answer)
	}
	if containsWord(rejected, answer) {
		return fmt.Errorf("%s was previously rejected by the game database; refusing to retry", answer)
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

	fmt.Println("[4/4] submitting through parent page (real play, permanent guess, no do-overs)...")
	submitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	outcome, err := submitOrdknude(submitCtx, br, answer, true)
	cancel()
	if strings.Contains(strings.ToLower(outcome), "ordet findes ikke") {
		_ = klublotto.RecordRejectedWord(cfg.DataDir, answer)
	}
	if err != nil {
		return err
	}
	shot := filepath.Join(cfg.DataDir, "ordknude-result-"+time.Now().UTC().Format("20060102-150405")+".png")
	_ = br.Screenshot(ctx, shot)
	return upsertDailyGame(ctx, cfg, "Ordknuden", "5-letter Danish word puzzle", answer, true, true, "Submitted through parent page. Screenshot: `"+shot+"`.")
}

func runKrydsord(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("krydsord", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "extract board/API artifacts + solve, but do not submit (note: bare 'krydsord' does real submit+credit by design; use this to guard; differs from sudoku/ord* siblings where bare is always dry)")
	submitFlag := fs.Bool("submit", false, "submit the solved grid (via API save + Tjek løsning on parent)")
	gridPath := fs.String("grid", "", "validate a proposed Krydsord grid from JSON or text")
	partialGrid := fs.Bool("partial", false, "allow _/space unknowns when validating --grid")
	graphOnly := fs.Bool("graph", false, "stage 1 only: ask the vision LLM to deconstruct the crossword into a clue graph (JSON) and exit — does not solve or submit")
	verifyGraph := fs.Bool("verify", true, "with --graph: run a second vision pass that re-checks each clue's length and direction against the image and corrects them")
	solveOnly := fs.Bool("solve", false, "stage 2: deconstruct (or load --graph-file) then solve every clue via the reasoning model using computed crossings; prints answers, does not submit")
	graphFile := fs.String("graph-file", "", "path to a stage-1 clue-graph JSON to solve (with --solve); if empty, --solve deconstructs fresh via vision")
	solutionFile := fs.String("solution-file", "", "with --solve: load answers from this saved solution JSON instead of calling the LLM (e.g. to re-submit a trusted solve)")
	learnFlag := fs.Bool("learn", false, "with --solve: merge this run's clue→answer pairs into the learned dictionary (wiki/concepts/krydsord-clues.json)")
	providerFlag := fs.String("provider", "", "word provider for clue candidates: gemini|openai|xai|anthropic|openrouter")
	headlessFlag := fs.Bool("headless", false, "force headless browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// ── Stage 2/3: solve from a validated graph; optionally submit ────────────────
	// Solve is offline (no browser). --submit fills the solved grid onto
	// danskespil.dk, so we open a browser only in that case.
	if *solveOnly {
		var sbr *browser.Client
		if *submitFlag {
			sbr = gameBrowser(cfg, *headlessFlag)
			restartHeadedSession(ctx, sbr)
		}
		return solveKrydsord(ctx, cfg, sbr, *graphFile, *solutionFile, *providerFlag, *learnFlag, *dryRun, *submitFlag)
	}

	br := gameBrowser(cfg, *headlessFlag)
	restartHeadedSession(ctx, br)

	fmt.Println("[1/4] opening Dagens Krydsord...")
	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	err = openGameWithLogin(openCtx, br, cfg, klublotto.OpenKrydsord)
	curURL, _ := br.URL(openCtx)
	cancel()
	if err != nil {
		return err
	}
	fmt.Println("       at:", curURL)

	// ── Stage 1: deconstruct the crossword into a clue graph (no solving) ─────────
	// Pure vision on the board as rendered on the danskespil.dk PARENT page — we do
	// NOT call the krydsord.dk iframe API (that's only needed for the structural
	// mask/solve), so stage 1 never leaves danskespil.dk.
	if *graphOnly {
		return deconstructKrydsord(ctx, cfg, br, *verifyGraph)
	}

	fmt.Println("[2/4] extracting iframe API data...")
	extractCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	data, err := klublotto.ExtractKrydsordData(extractCtx, br)
	cancel()
	if err != nil {
		return err
	}

	fmt.Println("[3/4] building grid slots...")
	slots := klublotto.BuildKrydsordSlots(data)
	art, err := klublotto.SaveKrydsordArtifacts(cfg.DataDir, data, slots)
	if err != nil {
		return err
	}
	fmt.Printf("Title: %s\n", data.Title)
	fmt.Printf("Dimensions: %dx%d\n", data.CellCountX, data.CellCountY)
	fmt.Printf("Puzzle ID: %s; crossword ID: %s\n", data.PuzzleID, data.CrosswordID)
	fmt.Println()
	fmt.Println("== Mask ==")
	fmt.Println(klublotto.FormatKrydsordMask(data))
	fmt.Println()
	fmt.Println("== Current User Grid ==")
	fmt.Println(klublotto.FormatKrydsordUserGrid(data))
	fmt.Println()
	fmt.Printf("Slots: %d contiguous answer runs (%d across, %d down)\n", len(slots), countKrydsordSlots(slots, "across"), countKrydsordSlots(slots, "down"))
	fmt.Println("Artifacts:")
	fmt.Println("- API:", art.APIPath)
	fmt.Println("- board image:", art.ImagePath)
	fmt.Println("- mask:", art.MaskPath)
	fmt.Println("- slots:", art.SlotsPath)

	// Load board image bytes (for vision OCR of clues, and debug).
	imgBytes, _ := os.ReadFile(art.ImagePath)
	if len(imgBytes) == 0 && data.Image != "" {
		img := data.Image
		if i := strings.Index(img, ","); strings.HasPrefix(img, "data:") && i >= 0 {
			img = img[i+1:]
		}
		if dec, decErr := base64.StdEncoding.DecodeString(img); decErr == nil {
			imgBytes = dec
		}
	}

	solvedGrid := []string{}
	if strings.TrimSpace(*gridPath) != "" {
		fmt.Println()
		fmt.Println("[grid] validating proposed grid:", *gridPath)
		raw, err := os.ReadFile(*gridPath)
		if err != nil {
			return fmt.Errorf("read Krydsord grid %s: %w", *gridPath, err)
		}
		grid, err := klublotto.ParseKrydsordGrid(string(raw))
		if err != nil {
			return err
		}
		check := klublotto.ValidateKrydsordAnswerGrid(data, grid)
		if *partialGrid {
			check = klublotto.ValidateKrydsordPartialGrid(data, grid)
		}
		fmt.Printf("Grid check: ok=%v filled=%d/%d answer-cells=%d\n", check.OK, check.FilledN, check.AnswerN, check.AnswerN)
		for _, err := range check.Errors {
			fmt.Println("-", err)
		}
		if !check.OK {
			return fmt.Errorf("Krydsord grid validation failed")
		}
		fmt.Println()
		fmt.Println("== Proposed Grid ==")
		for _, row := range grid {
			fmt.Println(row)
		}
		_ = saveDebug(cfg.DataDir, "krydsord-proposed-grid.txt", strings.Join(grid, "\n")+"\n")
		solvedGrid = grid
	} else {
		// Real solve path (no dry-run simulation): vision OCR clues from board image + mask/slots,
		// per-slot word candidates via configured word provider, then LLM assembly of full consistent grid.
		fmt.Println("[3.5/4] extracting clues from board image via vision...")
		var ac llm.VisionProvider
		if cfg.GeminiKey != "" {
			ac = llm.NewGemini(cfg.GeminiKey, "gemini-2.5-pro")
		} else if cfg.AnthropicKey != "" {
			// Use haiku for vision/OCR step (fast, cheap, sufficient for reading clue text in grid image).
			ac = llm.NewAnthropic(cfg.AnthropicKey, "claude-haiku-4-5-20251001")
		}
		if n, ok := ac.(interface{ Name() string }); ok {
			fmt.Printf("       vision model: %s\n", n.Name())
		}
		if ac == nil {
			fmt.Println("       WARNING: no vision API key (GEMINI_API_KEY or ANTHROPIC_API_KEY); vision-based clue OCR unavailable.")
			if !*dryRun {
				return fmt.Errorf("GEMINI_API_KEY or ANTHROPIC_API_KEY is required for real `krydsord` solve+submit (use --dry-run to extract only, or --grid <file> to validate/supply a grid)")
			}
			fmt.Println("       (dry-run: skipping auto-solve; full grid solve would require ANTHROPIC + word provider keys)")
			// solvedGrid stays [], later code will print dry note. --grid branch above already handled debug case.
		} else {
			var clues []klublotto.KrydsordClue
			var verr error
			clues, verr = klublotto.ExtractKrydsordClues(ctx, data, imgBytes, ac)
			if verr != nil {
				// Do not hard-fail the whole run on vision problems (common with haiku or truncated responses on complex boards).
				// Log warning, ensure the raw is saved for post-mortem, and continue with whatever partial/empty clues we got.
				// The mask is always authoritative for the assembler.
				fmt.Printf("       WARNING: vision clue extraction had issues (%v). Raw saved to krydsord-vision-raw.txt. Will continue with %d clues (assembler uses mask + crossings primarily).\n", verr, len(clues))
			}
			// Copy the /tmp debug raw (written by Extract) into the normal artifacts dir for this run
			// so it sits next to krydsord-board-*.jpg etc. Easy to retrieve even in k8s.
			if b, rerr := os.ReadFile(filepath.Join(os.TempDir(), "krydsord-vision-raw.txt")); rerr == nil && len(b) > 0 {
				_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-vision-raw.txt"), b, 0o644)
			}
			fmt.Printf("       %d clues extracted\n", len(clues))
			for _, cl := range clues {
				tag := ""
				if cl.IsImage {
					tag = " [image]"
				}
				fmt.Printf("       %s %s (%d): %s%s\n", cl.SlotID, cl.Direction, cl.Length, cl.Clue, tag)
			}

			if len(clues) == 0 && !*dryRun {
				return fmt.Errorf("vision extracted 0 clues (see krydsord-vision-raw.txt for the model response). Cannot reliably solve without clues; use --grid <file> with a correct filled grid (validated against the live mask), or --dry-run")
			}

			fmt.Println("[3.6/4] asking word provider for Danish candidates per clue...")
			if sp, perr := wordProvider(cfg, *providerFlag); perr == nil {
				if n, ok := sp.(interface{ Name() string }); ok {
					fmt.Printf("       solve/word model: %s (used for candidates + grid assembly)\n", n.Name())
				}
			}
			slotCands := map[string][]klublotto.WordCandidate{}

			// Batch every clue into ONE provider call instead of one call per
			// clue (was ~39 tiny requests). Falls back to per-clue below for any
			// slot the batch didn't return.
			batchClues := []klublotto.KrydsordBatchClue{}
			want := map[string]int{}
			for _, cl := range clues {
				if cl.Clue == "" {
					continue // no usable clue text; assembler relies on mask + crossings + allClueTexts
				}
				batchClues = append(batchClues, klublotto.KrydsordBatchClue{SlotID: cl.SlotID, Clue: cl.Clue, Length: cl.Length, IsImage: cl.IsImage})
				want[cl.SlotID] = cl.Length
			}
			if len(batchClues) > 0 {
				bp := klublotto.BuildKrydsordBatchPrompt(batchClues)
				if raw, berr := wordCandidatesRawJSON(ctx, cfg, *providerFlag, bp); berr != nil {
					fmt.Printf("       batch candidate call failed (%v) — falling back to per-clue\n", berr)
				} else if m, perr := klublotto.ParseKrydsordBatchCandidates(raw, want); perr != nil {
					fmt.Printf("       batch candidate parse failed (%v) — falling back to per-clue\n", perr)
				} else {
					slotCands = m
					fmt.Printf("       batch: candidates for %d/%d clues in a single call\n", len(slotCands), len(batchClues))
				}
			}

			// Per-clue fill for any slot the batch didn't cover (also the full
			// fallback path when the batch call/parse failed entirely).
			for _, cl := range clues {
				if cl.Clue == "" || len(slotCands[cl.SlotID]) > 0 {
					continue
				}
				prompt := fmt.Sprintf("Danish crossword clue: `%s`; length exactly %d letters; Danish word, no spaces or punctuation in the answer. Return JSON candidates.", cl.Clue, cl.Length)
				cands, err := wordCandidates(ctx, cfg, *providerFlag, prompt)
				if err != nil {
					fmt.Printf("       %s: provider err: %v\n", cl.SlotID, err)
					continue
				}
				var good []klublotto.WordCandidate
				for _, c := range cands {
					if len([]rune(klublotto.NormalizeDanishLetters(c.Answer))) == cl.Length {
						good = append(good, c)
					}
				}
				if len(good) > 0 {
					slotCands[cl.SlotID] = good
					printCandidates(good)
				}
			}

			fmt.Println("[3.7/4] asking provider to assemble full consistent grid...")
			// Collect all unique clue texts (even if mapping to slots was imperfect) so the assembler LLM
			// has the full set of visible clues and can re-assign based on the mask + crossings.
			allClueTexts := []string{}
			seen := map[string]bool{}
			for _, cl := range clues {
				if cl.Clue != "" && !seen[cl.Clue] {
					seen[cl.Clue] = true
					allClueTexts = append(allClueTexts, cl.Clue)
				}
			}
			grid, err := assembleKrydsordSolutionGrid(ctx, cfg, *providerFlag, data, clues, slotCands, allClueTexts)
			if err != nil {
				return fmt.Errorf("solve krydsord: %w", err)
			}
			solvedGrid = grid
			_ = saveDebug(cfg.DataDir, "krydsord-solution.txt", strings.Join(solvedGrid, "\n")+"\n")
			fmt.Println()
			fmt.Println("== Solved Grid ==")
			for _, row := range solvedGrid {
				fmt.Println(row)
			}
		}
	}

	// submit guard: per task "do not try to do a dry-run" + "real solve + submission", bare `krydsord` (no flags) performs real submit after full grid (vision+cands+assemble).
	// This diverges from sudoku (`*submitFlag && !*dryRun`) and ord* (similar) where bare command is always a dry-run unless --submit is explicit.
	// --dry-run still safely guards the final submit click (and Makefile plain target now passes --submit explicitly, like sudoku:).
	// --submit can force even with --dry-run; --grid bypasses auto-solve for debug.
	submit := *submitFlag || (!*dryRun)
	if len(solvedGrid) == 0 {
		fmt.Println("[4/4] no full answer grid; nothing to submit.")
		return nil
	}
	if !submit {
		fmt.Println("[4/4] dry run — not submitting.")
		return nil
	}

	fmt.Println("[4/4] submitting through parent page...")
	submitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	err = submitKrydsord(submitCtx, br, data, solvedGrid)
	cancel()
	if err != nil {
		snap, _ := br.Snapshot(ctx)
		_ = saveDebug(cfg.DataDir, "krydsord-submit-fail.txt", snap)
		return err
	}
	shot := filepath.Join(cfg.DataDir, "krydsord-result-"+time.Now().UTC().Format("20060102-150405")+".png")
	_ = br.Screenshot(ctx, shot)

	// Best-effort auto-attach of the result screenshot (full page is acceptable start;
	// tight crop to just the grid like the 05-31 manual example in .klublotto/ is ideal
	// for the UI detail view but not required here). Only if the ledger row already
	// exists in Postgres (e.g. from prior wiki import or web UI run that did import).
	// NOTE: when run via web UI job, the CLI attach here runs *before* the post-job
	// ImportWikiDaily (which creates the row from the upsert md); thus first-of-day
	// UI-triggered krydsord may not attach on that run (row appears after). Subsequent
	// runs or direct CLI with pre-existing row (or manual ledger import) will attach.
	// This is acceptable for smallest change + matches original upsert+attach pattern.
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		if st, stErr := store.New(ctx, dsn); stErr == nil {
			defer st.Close()
			loc, locErr := time.LoadLocation("Europe/Copenhagen")
			if locErr != nil {
				loc = time.Local
			}
			dt := time.Now().In(loc).Format("2006-01-02")
			var id int64
			_ = st.Pool.QueryRow(ctx, `SELECT id FROM daily_ledger WHERE date = $1 AND game_slug = $2`, dt, "krydsord").Scan(&id)
			if id != 0 {
				if img, rdErr := os.ReadFile(shot); rdErr == nil && len(img) > 0 {
					_ = st.SetResultImage(ctx, id, img)
					fmt.Println("       attached result screenshot to Postgres daily_ledger.result_image")
				}
			}
		}
	}

	return upsertDailyGame(ctx, cfg, "Krydsord", "Danish clues-in-squares crossword", gridOneLineKrydsord(solvedGrid), true, true, "Solved via clues OCR + LLM candidates + consistent grid. Saved via vendor API + Tjek løsning. Screenshot: `"+shot+"`.")
}

// krydsordDeconstructPrompt asks a vision model to read a Scandinavian
// clue-square crossword and emit the full clue graph (clue text/image, direction,
// start coordinate, answer length) as JSON — WITHOUT solving. Stage 1 of the
// two-stage solver: get a correct structural graph first, solve later.
const krydsordDeconstructPrompt = `Do NOT solve anything yet.

This is a Scandinavian clue-square crossword.

Rules:
- Text in the left border gives horizontal clues.
- Text in the top border gives vertical clues.
- In split clue cells:
  - upper clue = horizontal answer
  - lower clue = vertical answer
- Images follow the same rules:
  - image in left border = horizontal clue
  - image in top border = vertical clue
  - image in a clue cell behaves exactly like text clues
- The top-left logo is NOT a clue.

First create a complete list of all clues.

For every clue report:
- clue text (or a short image description, prefixed "IMG: ")
- direction (Across = horizontal, Down = vertical)
- starting coordinate of the FIRST answer cell as {"row": R, "column": C} (1-indexed, row 1 = top, column 1 = left)
- answer length = the number of consecutive EMPTY WHITE cells the answer fills,
  counted starting at the cell IMMEDIATELY to the RIGHT of the clue cell (Across)
  or IMMEDIATELY BELOW the clue cell (Down). DO NOT count the clue cell itself.
  Stop counting at the next clue cell, image cell, or the edge of the grid.
  Count CELLS on the board — NOT the number of letters in the clue word.
  (Example: if there are 3 empty white cells to the right of "SKIBSDEL", the
  length is 3 — not 8 from the clue text, and not 4 by including the clue cell.)

Do not attempt solving.

Return ONLY a JSON object, no prose, in exactly this shape:
{
  "Across": [ {"clue": "REDSKAB", "direction": "Across", "start": {"row": 2, "column": 2}, "length": 9} ],
  "Down":   [ {"clue": "FARTØJ",  "direction": "Down",   "start": {"row": 2, "column": 2}, "length": 10} ]
}`

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

// krydsordVisionProvider picks the vision model for the graph step. Override
// with OPENROUTER_VISION_MODEL (e.g. openai/gpt-5.4) to try a different LLM.
func krydsordVisionProvider(cfg *config.Config) (llm.VisionProvider, error) {
	switch {
	case cfg.OpenRouterKey != "" && cfg.OpenRouterVisionModel != "":
		fmt.Printf("   [graph] vision: openrouter:%s\n", cfg.OpenRouterVisionModel)
		return llm.NewOpenRouterVision(cfg.OpenRouterKey, cfg.OpenRouterVisionModel), nil
	case cfg.GeminiKey != "":
		fmt.Println("   [graph] vision: gemini:gemini-2.5-pro")
		return llm.NewGemini(cfg.GeminiKey, "gemini-2.5-pro"), nil
	case cfg.AnthropicKey != "":
		fmt.Println("   [graph] vision: anthropic:claude-haiku-4-5-20251001")
		return llm.NewAnthropic(cfg.AnthropicKey, "claude-haiku-4-5-20251001"), nil
	}
	return nil, fmt.Errorf("need OPENROUTER_API_KEY+OPENROUTER_VISION_MODEL, GEMINI_API_KEY, or ANTHROPIC_API_KEY")
}

// krydsordGraphJSON runs the stage-1 vision deconstruction: screenshot the
// crossword on the danskespil.dk parent page and return the clue-graph JSON
// (cleaned to the {…} object) plus the board image bytes (for a verify pass).
// Stays on the parent page — no iframe API call.
func krydsordGraphJSON(ctx context.Context, cfg *config.Config, br *browser.Client) (string, []byte, error) {
	ac, err := krydsordVisionProvider(cfg)
	if err != nil {
		return "", nil, err
	}
	// Give the embedded board a moment to finish rendering, then screenshot the
	// parent page (the crossword the player sees) — no iframe navigation.
	_ = br.WaitForLoad(ctx, "networkidle")
	time.Sleep(1500 * time.Millisecond)
	stamp := time.Now().UTC().Format("20060102-150405")
	inputPath := filepath.Join(cfg.DataDir, "krydsord-graph-input-"+stamp+".png")
	if err := br.Screenshot(ctx, inputPath); err != nil {
		return "", nil, fmt.Errorf("screenshot parent board: %w", err)
	}
	imgBytes, _ := os.ReadFile(inputPath)
	if len(imgBytes) == 0 {
		return "", nil, fmt.Errorf("parent screenshot was empty (%s)", inputPath)
	}
	fmt.Printf("   [graph] input image: %s\n", inputPath)
	visionCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	raw, err := ac.ExtractFromImage(visionCtx, imgBytes, "image/png", krydsordDeconstructPrompt)
	cancel()
	if err != nil {
		return "", nil, fmt.Errorf("vision call failed: %w", err)
	}
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-graph-raw.txt"), []byte(raw), 0o644)
	return klublotto.ExtractJSONObject(strings.TrimSpace(raw)), imgBytes, nil
}

// krydsordVerifyPrompt asks the model to re-check a graph against the image,
// focusing on the two things vision gets wrong most: answer length and direction.
const krydsordVerifyPrompt = `Du har tidligere lavet nedenstående clue-graph for det vedhæftede skandinaviske clue-square krydsord. VERIFICÉR den mod billedet og RET fejl.

Fokusér især på (det er her fejlene plejer at være):
1. LÆNGDE = antal sammenhængende TOMME HVIDE felter svaret fylder, talt FRA feltet lige til HØJRE for ledetråds-feltet (Across) eller lige UNDER ledetråds-feltet (Down).
   - Medregn ALDRIG selve ledetråds-feltet (det med teksten/billedet).
   - Stop ved næste ledetråds-felt, billed-felt eller brættets kant.
   - Tæl FELTER på billedet — IKKE bogstaver i ledetråds-ordet.
   - Gå hver post igennem og TÆL felterne på billedet igen.
   - Eksempel: er der 3 tomme hvide felter til højre for "SKIBSDEL", så er length = 3 (ikke 8 fra teksten, ikke 4 ved at tælle ledetråds-feltet med). "SMALL" med 1 tomt felt til højre = length 1.
2. RETNING: Across = vandret (svaret fylder felter til HØJRE for ledetråden), Down = lodret (svaret fylder felter NEDAD under ledetråden). Ledetråd i topkanten = Down. Ledetråd i venstrekant = Across. I et delt ledetråds-felt: øverste tekst = Across, nederste tekst = Down.

Bevar ledetråds-teksterne og startkoordinaterne. Ret kun length/direction (og flyt en post mellem Across/Down hvis retningen var forkert).

Returner KUN det rettede JSON i NØJAGTIG samme format: Across/Down lister med "clue", "direction", "start" som {"row": R, "column": C}, og "length". Ingen anden tekst.

Graph der skal verificeres:
`

// verifyKrydsordGraph sends the board image + a produced graph back to the vision
// model and asks it to correct lengths/directions. Returns the corrected graph
// JSON, or an error (callers fall back to the unverified graph).
func verifyKrydsordGraph(ctx context.Context, cfg *config.Config, imgBytes []byte, graphJSON string) (string, error) {
	ac, err := krydsordVisionProvider(cfg)
	if err != nil {
		return "", err
	}
	visionCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	raw, err := ac.ExtractFromImage(visionCtx, imgBytes, "image/png", krydsordVerifyPrompt+graphJSON)
	cancel()
	if err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-graph-verify-raw.txt"), []byte(raw), 0o644)
	out := klublotto.ExtractJSONObject(strings.TrimSpace(raw))
	if !strings.Contains(out, "Across") && !strings.Contains(out, "Down") {
		return "", fmt.Errorf("verify response had no usable graph")
	}
	return out, nil
}

// deconstructKrydsord runs stage 1 only: produce + (optionally) verify + print +
// save the clue graph.
func deconstructKrydsord(ctx context.Context, cfg *config.Config, br *browser.Client, verify bool) error {
	fmt.Printf("[graph] deconstructing crossword into a clue graph (no solving)...\n")
	graph, imgBytes, err := krydsordGraphJSON(ctx, cfg, br)
	if err != nil {
		return fmt.Errorf("krydsord --graph: %w", err)
	}
	if verify {
		fmt.Println("[graph] verifying graph against the image (length + direction)...")
		if corrected, verr := verifyKrydsordGraph(ctx, cfg, imgBytes, graph); verr != nil {
			fmt.Printf("   [graph] verify pass failed (%v) — keeping the initial graph\n", verr)
		} else {
			graph = corrected
			fmt.Println("   [graph] applied verified/corrected graph")
		}
	}
	out := graph
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(graph), "", "  ") == nil {
		out = pretty.String()
	}
	graphPath := filepath.Join(cfg.DataDir, "krydsord-graph-"+time.Now().UTC().Format("20060102-150405")+".json")
	_ = os.WriteFile(graphPath, []byte(out+"\n"), 0o644)
	fmt.Println("\n== Clue graph (stage 1) ==")
	fmt.Println(out)
	fmt.Printf("\nSaved: %s\n", graphPath)
	return nil
}

// krydsordStart is the 1-indexed start cell of an answer. It unmarshals from the
// explicit object form {"row":2,"column":2} AND the legacy array form [2,2], so
// previously-saved graphs still load.
type krydsordStart struct {
	Row int `json:"row"`
	Col int `json:"column"`
}

func (s *krydsordStart) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) > 0 && t[0] == '[' { // legacy [row, col]
		var arr []int
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		if len(arr) == 2 {
			s.Row, s.Col = arr[0], arr[1]
		}
		return nil
	}
	type alias krydsordStart // object {"row","column"}
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*s = krydsordStart(a)
	return nil
}

func (s krydsordStart) valid() bool { return s.Row >= 1 && s.Col >= 1 }

// krydsordGraphClue / krydsordGraph mirror the stage-1 graph JSON.
type krydsordGraphClue struct {
	Clue      string        `json:"clue"`
	Direction string        `json:"direction"`
	Start     krydsordStart `json:"start"` // {"row":R,"column":C}, 1-indexed
	Length    int           `json:"length"`
}

type krydsordGraph struct {
	Across []krydsordGraphClue `json:"Across"`
	Down   []krydsordGraphClue `json:"Down"`
}

// krydsordCSP is the flattened constraint-satisfaction view of the crossword:
// every entry's exact cells plus, per cell, which (entry:position) pairs share
// it. Shared cells (>1 member) ARE the crossings — letters there must match.
// Handing this to the solver removes all geometry inference; it can focus on
// language + crossings, where LLMs are strongest.
type krydsordCSP struct {
	Language string                      `json:"language"`
	Entries  map[string]krydsordCSPEntry `json:"entries"`
	Cells    map[string][]string         `json:"cells"`
}

type krydsordCSPEntry struct {
	Clue   string   `json:"clue"`
	Length int      `json:"length"`
	Cells  []string `json:"cells"`
}

// buildKrydsordCSP flattens the clue graph into the CSP structure. Cell ids are
// "r<row>c<col>" (1-indexed); membership entries are "<EntryID>:<position>" with
// position 1-indexed (letter 1 = first cell of the answer).
func buildKrydsordCSP(g krydsordGraph) krydsordCSP {
	csp := krydsordCSP{Language: "da", Entries: map[string]krydsordCSPEntry{}, Cells: map[string][]string{}}
	add := func(id, clue string, length, r, c int, down bool) {
		e := krydsordCSPEntry{Clue: clue, Length: length}
		for k := 0; k < length; k++ {
			rr, cc := r, c+k
			if down {
				rr, cc = r+k, c
			}
			cid := fmt.Sprintf("r%dc%d", rr, cc)
			e.Cells = append(e.Cells, cid)
			csp.Cells[cid] = append(csp.Cells[cid], fmt.Sprintf("%s:%d", id, k+1))
		}
		csp.Entries[id] = e
	}
	for i, a := range g.Across {
		if a.Start.valid() && a.Length > 0 {
			add(fmt.Sprintf("A%d", i+1), a.Clue, a.Length, a.Start.Row, a.Start.Col, false)
		}
	}
	for i, d := range g.Down {
		if d.Start.valid() && d.Length > 0 {
			add(fmt.Sprintf("D%d", i+1), d.Clue, d.Length, d.Start.Row, d.Start.Col, true)
		}
	}
	return csp
}

// renderKrydsordBoard draws a compact ASCII grid of the puzzle from the CSP so
// the LLM gets the spatial layout, not just the cell lists: "·" = an answer cell
// in one entry, "+" = a crossing cell (shared by an across and a down entry),
// blank = not an answer cell. Row/column headers map back to the cell ids.
func renderKrydsordBoard(csp krydsordCSP) string {
	type rc struct{ r, c int }
	count := map[rc]int{}
	maxR, maxC := 0, 0
	for cid, members := range csp.Cells {
		var r, c int
		if _, err := fmt.Sscanf(cid, "r%dc%d", &r, &c); err != nil {
			continue
		}
		count[rc{r, c}] = len(members)
		if r > maxR {
			maxR = r
		}
		if c > maxC {
			maxC = c
		}
	}
	var b strings.Builder
	b.WriteString("    ")
	for c := 1; c <= maxC; c++ {
		fmt.Fprintf(&b, "%2d ", c)
	}
	b.WriteString("\n")
	for r := 1; r <= maxR; r++ {
		fmt.Fprintf(&b, "%3d ", r)
		for c := 1; c <= maxC; c++ {
			switch n := count[rc{r, c}]; {
			case n >= 2:
				b.WriteString(" + ")
			case n == 1:
				b.WriteString(" · ")
			default:
				b.WriteString("   ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// buildKrydsordGridFromAnswers places each solved answer's letters into its CSP
// cells, producing a w×h grid of rows (answer cells = uppercase letter, every
// other cell = "."). The cell ids ("r<row>c<col>", 1-indexed) line up with the
// live API mask, so the result feeds straight into ValidateKrydsordAnswerGrid /
// BuildKrydsordUserSolution.
func buildKrydsordGridFromAnswers(csp krydsordCSP, answersByID map[string]string, w, h int) []string {
	grid := make([][]rune, h)
	for r := range grid {
		grid[r] = make([]rune, w)
		for c := range grid[r] {
			grid[r][c] = '.'
		}
	}
	for id, e := range csp.Entries {
		a := []rune(answersByID[id])
		if len(a) != e.Length {
			continue
		}
		for k, cid := range e.Cells {
			var r, c int
			if _, err := fmt.Sscanf(cid, "r%dc%d", &r, &c); err != nil {
				continue
			}
			if r >= 1 && r <= h && c >= 1 && c <= w {
				grid[r-1][c-1] = a[k]
			}
		}
	}
	out := make([]string, h)
	for r := range grid {
		out[r] = string(grid[r])
	}
	return out
}

// buildKrydsordGridFromSlotAnswers places per-slot answers (keyed by slot ID)
// deterministically into a w×h grid using each slot's known cells — so the grid
// dimensions are always correct (the LLM only picks words, never emits the
// grid, which is what caused "row N has 11 columns"). It returns the grid plus
// any crossing conflicts: cells two slots disagree on, fed back to the LLM.
func buildKrydsordGridFromSlotAnswers(data klublotto.KrydsordData, slots []klublotto.KrydsordSlot, answersByID map[string]string) (grid []string, conflicts []string) {
	w, h := data.CellCountX, data.CellCountY
	cells := make([][]rune, h)
	for r := range cells {
		cells[r] = make([]rune, w)
		for c := range cells[r] {
			cells[r][c] = '.'
		}
	}
	// Track which slot last wrote each cell so we can report disagreements.
	owner := map[[2]int]string{}
	for _, s := range slots {
		a := []rune(klublotto.NormalizeDanishLetters(answersByID[s.ID]))
		if len(a) != s.Length {
			continue
		}
		for k, cell := range s.Cells {
			if cell.Row < 1 || cell.Row > h || cell.Col < 1 || cell.Col > w {
				continue
			}
			cur := cells[cell.Row-1][cell.Col-1]
			if cur != '.' && cur != a[k] {
				conflicts = append(conflicts, fmt.Sprintf("R%dC%d: %s wants %c but %s set %c",
					cell.Row, cell.Col, s.ID, a[k], owner[[2]int{cell.Row, cell.Col}], cur))
				continue
			}
			cells[cell.Row-1][cell.Col-1] = a[k]
			owner[[2]int{cell.Row, cell.Col}] = s.ID
		}
	}
	grid = make([]string, h)
	for r := range cells {
		grid[r] = string(cells[r])
	}
	return grid, conflicts
}

// crossingCount returns the number of cells shared by two or more entries.
func (csp krydsordCSP) crossingCount() int {
	n := 0
	for _, members := range csp.Cells {
		if len(members) >= 2 {
			n++
		}
	}
	return n
}

// latestKrydsordGraph returns the most recently saved stage-1 clue graph in dir
// (krydsord-graph-*.json, written by `make krydsord-graph`), or an error telling
// the user to produce one first.
func latestKrydsordGraph(dir string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "krydsord-graph-*.json"))
	var newest string
	var newestT time.Time
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if fi.ModTime().After(newestT) {
			newestT, newest = fi.ModTime(), m
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no saved clue graph in %s — run `make krydsord-graph` first, verify the printed graph is correct, then run `make krydsord-solve` (or pass GRAPH_FILE=path to a known-good graph)", dir)
	}
	return newest, nil
}

// solveKrydsord runs stage 2 from a PREVIOUSLY-VALIDATED clue graph. Vision
// deconstruction (stage 1) is non-deterministic, so we never re-roll it here:
// solve loads the graph from --graph-file, or the most recent one saved by
// `make krydsord-graph`, builds the CSP deterministically (no AI), and asks the
// reasoning model to fill it in. Prints the answers + a CSP validation report;
// does not submit.
func solveKrydsord(ctx context.Context, cfg *config.Config, br *browser.Client, graphFile, solutionFile, provider string, learn, dry, submit bool) error {
	if strings.TrimSpace(graphFile) == "" {
		latest, err := latestKrydsordGraph(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("krydsord --solve: %w", err)
		}
		graphFile = latest
	}
	b, err := os.ReadFile(graphFile)
	if err != nil {
		return fmt.Errorf("read graph %s: %w", graphFile, err)
	}
	graphJSON := klublotto.ExtractJSONObject(string(b))
	fmt.Printf("[solve] using validated graph: %s\n", graphFile)

	var g krydsordGraph
	if err := json.Unmarshal([]byte(graphJSON), &g); err != nil {
		return fmt.Errorf("krydsord --solve: parse graph JSON: %w", err)
	}
	if len(g.Across)+len(g.Down) == 0 {
		return fmt.Errorf("krydsord --solve: graph has no clues")
	}
	csp := buildKrydsordCSP(g)
	cspJSON, _ := json.MarshalIndent(csp, "", "  ")
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-csp.json"), append(cspJSON, '\n'), 0o644)
	board := renderKrydsordBoard(csp)
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-board.txt"), []byte(board), 0o644)
	fmt.Printf("[solve] %d across, %d down, %d crossings (CSP)\n", len(g.Across), len(g.Down), csp.crossingCount())
	fmt.Println("\n== Board ==")
	fmt.Println(board)

	// Our own learned clue dictionary: feed any entries whose clue is in today's
	// puzzle into the prompt as preferred answers.
	dictPath := filepath.Join(wikiRoot(), "concepts", "krydsord-clues.json")
	dict := klublotto.LoadKrydsordDict(dictPath)
	var clueTexts []string
	for _, c := range g.Across {
		clueTexts = append(clueTexts, c.Clue)
	}
	for _, c := range g.Down {
		clueTexts = append(clueTexts, c.Clue)
	}
	dictLines := dict.MatchingLines(clueTexts)
	if len(dictLines) > 0 {
		fmt.Printf("[solve] %d clue(s) matched the learned dictionary\n", len(dictLines))
	}

	// Obtain the solution JSON: either from a saved --solution-file (skip the LLM,
	// e.g. to re-submit a solve you already trust) or by prompting the model.
	var clean, solveSource string
	if strings.TrimSpace(solutionFile) != "" {
		b, rerr := os.ReadFile(solutionFile)
		if rerr != nil {
			return fmt.Errorf("krydsord --solve: read --solution-file %s: %w", solutionFile, rerr)
		}
		clean = klublotto.ExtractJSONObject(strings.TrimSpace(string(b)))
		solveSource = "saved solution " + filepath.Base(solutionFile)
		fmt.Printf("[solve] using saved solution: %s (skipping the LLM)\n", solutionFile)
	} else {
		// The board + CSP + prompt are built deterministically (no AI) — do that
		// first so --dry-run can inspect them without an LLM key/call.
		prompt := buildKrydsordSolvePrompt(string(cspJSON), board, dictLines)
		promptPath := filepath.Join(cfg.DataDir, "krydsord-solve-prompt.txt")
		_ = os.WriteFile(promptPath, []byte(prompt), 0o644)
		fmt.Printf("   [solve] board (CSP): %s\n   [solve] prompt:      %s\n", filepath.Join(cfg.DataDir, "krydsord-csp.json"), promptPath)
		if dry {
			fmt.Printf("\n[solve] --dry-run: generated board + CSP prompt, not calling the LLM.\nPrompt saved: %s\n", promptPath)
			return nil
		}
		p, perr := wordProvider(cfg, provider)
		if perr != nil {
			return perr
		}
		// Reasoning models spend much of their output budget on reasoning; a large
		// cap keeps the full answer JSON from being truncated after the reasoning.
		if or, ok := p.(*llm.OpenRouter); ok {
			or.MaxTokens = 40000
		}
		solveSource = p.Name()
		fmt.Printf("   [solve] model: %s\n", p.Name())
		// Retry: the empty-content failure mode is intermittent.
		var raw string
		for attempt := 1; attempt <= 3; attempt++ {
			solveCtx, cancel := context.WithTimeout(ctx, 540*time.Second)
			r, callErr := p.GenerateJSON(solveCtx, prompt, 0.2)
			cancel()
			if callErr != nil {
				return fmt.Errorf("krydsord --solve: provider failed: %w", callErr)
			}
			raw = r
			if strings.Contains(r, "{") && strings.Contains(r, "}") {
				break
			}
			fmt.Printf("   [solve] attempt %d returned no JSON (reasoning model emptied its output) — retrying...\n", attempt)
		}
		_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-solve-raw.txt"), []byte(raw), 0o644)
		if !strings.Contains(raw, "{") {
			return fmt.Errorf("krydsord --solve: model returned no JSON after 3 attempts (saved krydsord-solve-raw.txt) — try --provider openai/gpt-5.5 or another non-reasoning model")
		}
		clean = klublotto.ExtractJSONObject(strings.TrimSpace(raw))
	}
	out := clean
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(clean), "", "  ") == nil {
		out = pretty.String()
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	solPath := filepath.Join(cfg.DataDir, "krydsord-solution-"+stamp+".json")
	_ = os.WriteFile(solPath, []byte(out+"\n"), 0o644)
	fmt.Println("\n== Solution (stage 2) ==")
	fmt.Println(out)
	fmt.Printf("\nSaved: %s\n", solPath)

	// Parse the answers tolerantly: reasoning models often truncate the JSON
	// array, which a strict Unmarshal rejects wholesale. Salvage every complete
	// {…} object so we still get the answers that did come through.
	answers := parseKrydsordAnswers(clean)
	answersByID := map[string]string{}
	for _, a := range answers {
		if a.ID != "" {
			answersByID[a.ID] = klublotto.NormalizeDanishLetters(a.Answer)
		}
	}
	if n := len(answersByID); n < len(csp.Entries) {
		fmt.Printf("\n[solve] parsed %d of %d answers — the response was likely truncated (reasoning models do this).\n           Try a non-reasoning model: make krydsord-solve SOLVE_MODEL=openai/gpt-5.4 GRAPH_FILE=...\n", n, len(csp.Entries))
	}

	// Validate against the CSP: lengths, missing entries, and crossing conflicts.
	// This surfaces exactly the kind of errors the model makes (wrong-length
	// answers, dropped entries, letters that disagree at a shared cell).
	issues := validateKrydsordSolution(csp, answersByID)
	fmt.Println("\n== Validering (mod CSP) ==")
	if len(issues) == 0 {
		fmt.Printf("Alle %d poster besvaret, længder og krydsninger passer ✓\n", len(csp.Entries))
	} else {
		fmt.Printf("%d problem(er) — løsningen er ikke konsistent endnu:\n", len(issues))
		for _, is := range issues {
			fmt.Println("-", is)
		}
		fmt.Println("(disse fejl er typisk forkert længde, manglende poster, eller krydsende bogstaver der ikke stemmer)")
	}

	// --learn: merge this run's clue→answer pairs into the learned dictionary.
	// Opt-in (the answers are not yet board-verified), so the user only commits a
	// solve they trust. Verified auto-learning will come with stage 3 (submit).
	if learn {
		added := 0
		for _, a := range answers {
			if dict.Add(a.Clue, a.Answer) {
				added++
			}
		}
		if err := dict.Save(dictPath); err != nil {
			fmt.Printf("   [learn] failed to save dictionary: %v\n", err)
		} else {
			fmt.Printf("   [learn] added %d new clue→answer entries to %s\n", added, dictPath)
		}
	}

	// Stage 3: fill the board on danskespil.dk and submit. Only when the solution
	// is fully consistent against the CSP — we never submit a grid with wrong
	// lengths, missing entries, or crossing conflicts.
	if submit {
		if len(issues) > 0 {
			return fmt.Errorf("krydsord --solve --submit: solution not consistent (%d issue(s)) — not submitting; re-solve until validation is clean", len(issues))
		}
		fmt.Println("\n[submit] solution is consistent — opening danskespil.dk and filling the board...")
		openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := openGameWithLogin(openCtx, br, cfg, klublotto.OpenKrydsord)
		cancel()
		if err != nil {
			return fmt.Errorf("submit: open krydsord: %w", err)
		}
		extractCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		data, derr := klublotto.ExtractKrydsordData(extractCtx, br)
		cancel()
		if derr != nil {
			return fmt.Errorf("submit: extract krydsord API data: %w", derr)
		}
		grid := buildKrydsordGridFromAnswers(csp, answersByID, data.CellCountX, data.CellCountY)
		// Safety net: the built grid must match the live mask exactly. If the graph
		// coordinates didn't line up with the API grid, this fails and we DON'T submit.
		if chk := klublotto.ValidateKrydsordAnswerGrid(data, grid); !chk.OK {
			for _, e := range chk.Errors {
				fmt.Println("   [submit] grid mismatch:", e)
			}
			return fmt.Errorf("submit: built grid does not match the live mask (%d errors) — not submitting", len(chk.Errors))
		} else {
			fmt.Printf("   [submit] grid validates against the live mask (%d answer cells) — submitting...\n", chk.AnswerN)
		}
		submitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		serr := submitKrydsord(submitCtx, br, data, grid)
		cancel()
		shot := filepath.Join(cfg.DataDir, "krydsord-result-"+time.Now().UTC().Format("20060102-150405")+".png")
		_ = br.Screenshot(ctx, shot)
		if serr != nil {
			return fmt.Errorf("submit: %w", serr)
		}
		fmt.Println("\n🎉 Krydsord submitted and confirmed correct!")
		// Verified learning: the solution is confirmed, so record every clue→answer.
		added := 0
		for _, a := range answers {
			if dict.Add(a.Clue, a.Answer) {
				added++
			}
		}
		if err := dict.Save(dictPath); err == nil && added > 0 {
			fmt.Printf("   [learn] recorded %d verified clue→answer entries to %s\n", added, dictPath)
		}
		return upsertDailyGame(ctx, cfg, "Krydsord", "Danish clues-in-squares crossword", gridOneLineKrydsord(grid), true, true,
			fmt.Sprintf("Solved via two-stage graph→CSP→LLM (%s). Screenshot: `%s`.", solveSource, shot))
	}
	return nil
}

// krydsordAnswer is one solved entry from the model's JSON.
type krydsordAnswer struct {
	ID     string `json:"id"`
	Clue   string `json:"clue"`
	Answer string `json:"answer"`
}

// parseKrydsordAnswers extracts answer objects from the model's JSON, tolerating
// a truncated array: it scans the "answers" array and unmarshals each balanced
// {…} object individually, so a cut-off tail loses only the missing entries
// rather than the whole response. (Answers are uppercase letters, so braces only
// ever appear as object delimiters here.)
func parseKrydsordAnswers(clean string) []krydsordAnswer {
	i := strings.Index(clean, `"answers"`)
	if i < 0 {
		return nil
	}
	s := clean[i:]
	if j := strings.IndexByte(s, '['); j >= 0 {
		s = s[j+1:]
	}
	var out []krydsordAnswer
	depth, start := 0, -1
	for k := 0; k < len(s); k++ {
		switch s[k] {
		case '{':
			if depth == 0 {
				start = k
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					var a krydsordAnswer
					if json.Unmarshal([]byte(s[start:k+1]), &a) == nil && a.ID != "" {
						out = append(out, a)
					}
					start = -1
				}
			}
		case ']':
			if depth == 0 {
				return out // array closed cleanly
			}
		}
	}
	return out // truncated mid-array — return what we salvaged
}

// validateKrydsordSolution checks a solution against the CSP and returns a list
// of problems: entries with no answer, answers of the wrong length, and shared
// cells whose letters disagree across the entries that cross there.
func validateKrydsordSolution(csp krydsordCSP, answers map[string]string) []string {
	var issues []string
	var ids []string
	for id := range csp.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := csp.Entries[id]
		a := answers[id]
		if a == "" {
			issues = append(issues, fmt.Sprintf("%s (%q): intet svar", id, e.Clue))
			continue
		}
		if n := len([]rune(a)); n != e.Length {
			issues = append(issues, fmt.Sprintf("%s (%q): %q har %d bogstaver, forventet %d", id, e.Clue, a, n, e.Length))
		}
	}
	// Crossing consistency: place each correct-length answer into its cells and
	// flag any cell that ends up with more than one distinct letter.
	cellLetters := map[string]map[rune][]string{}
	for id, e := range csp.Entries {
		a := []rune(answers[id])
		if len(a) != e.Length {
			continue // skip wrong-length answers (already reported)
		}
		for k, cid := range e.Cells {
			if cellLetters[cid] == nil {
				cellLetters[cid] = map[rune][]string{}
			}
			cellLetters[cid][a[k]] = append(cellLetters[cid][a[k]], fmt.Sprintf("%s:%d", id, k+1))
		}
	}
	var conflictCells []string
	for cid, byLetter := range cellLetters {
		if len(byLetter) > 1 {
			conflictCells = append(conflictCells, cid)
		}
	}
	sort.Strings(conflictCells)
	for _, cid := range conflictCells {
		var parts []string
		for r, members := range cellLetters[cid] {
			parts = append(parts, fmt.Sprintf("%c(%s)", r, strings.Join(members, ",")))
		}
		sort.Strings(parts)
		issues = append(issues, fmt.Sprintf("krydsningskonflikt %s: %s", cid, strings.Join(parts, " ≠ ")))
	}
	return issues
}

// krydsordClueHints lists common Scandinavian-crossword conventions the solver
// should consider. These are recurring "tricks" (Roman numerals for numbers,
// record formats, solfège notes, short function words, chemical symbols, …) that
// a general model often misses on Danish boards. Extend this list as we learn
// more board-specific conventions.
const krydsordClueHints = `TYPISKE KRYDSORD-TRICKS (skandinavisk krydsord — brug hvor det passer med længde og krydsninger):
- Tal skrives ofte som ROMERTAL: I=1, V=5, X=10, L=50, C=100, D=500, M=1000. Fx "1500"=MD, "1100"=MC, "2000"=MM, "51"=LI, "9"=IX, "4"=IV.
- "PLADE"/grammofonplade-format: LP, EP (evt. CD, SINGLE).
- Solmisation → node: DO=C, RE=D, MI=E, FA=F, SOL/SO=G, LA=A, TI=B (på dansk H). Fx ledetråd "MI" → E.
- Sportsstævne/mesterskab: VM (verdensmesterskab), OL (olympiske lege), DM (danmarksmesterskab), NM (nordisk mesterskab), EM (europamesterskab).
- "I DAG"/"IDAG" → DD (dags dato).
- Verdenshjørne/retning: N, S, Ø, V (nord/syd/øst/vest), samt NØ, NV, SØ, SV.
- Personligt stedord: JEG, DU, HAN, HUN, DEN, DET, VI, I, DE, MIG, DIG, SIG, OS, JER, DEM.
- Forholdsord: PÅ, I, AF, TIL, VED, OM, FOR, MED, UD, OP, AD.
- Bindeord: OG, MEN, ELLER, FOR, SÅ, AT.
- Kemisk tegn: ILT=O, BRINT=H, KULSTOF=C, JERN=FE, GULD=AU, SØLV=AG, KOBBER=CU, NATRIUM=NA.
- Udråb: AH, OH, AV, HØ, FY, NÅ, ØV.
- Et LAND som ledetråd → landekode (2 bogstaver, ISO): TYRKIET=TR, DANMARK=DK, NORGE=NO, SVERIGE=SE, TYSKLAND=DE, ITALIEN=IT, SPANIEN=ES, FRANKRIG=FR, USA=US, ØSTRIG=AT, SCHWEIZ=CH, POLEN=PL.
- Engelske ledetråde kan forekomme (fx SMALL, LARGE); oversæt til det danske svar (SMALL→LILLE/S, LARGE→STOR/L) medmindre svaret tydeligvis er en forkortelse.

KRITISK — KUN RIGTIGE DANSKE ORD:
- Svaret er ALTID dansk. Brug ALDRIG svenske/norske/engelske former (fx lyn = "LYN" på dansk, IKKE det svenske "ELD").
- Opfind ALDRIG ord for at få længden til at passe (fx "TRAWLERSL" er ikke et ord).
- Forkort ALDRIG et rigtigt ord til en ikke-eksisterende form (fx "MANEGE" må ikke afkortes til "MANEG"). Vælg et andet ord der har den rigtige længde OG findes i Den Danske Ordbog.`

// buildKrydsordSolvePrompt assembles the stage-2 solving prompt: convention
// hints, learned-dictionary answers, and the flattened CSP structure (entries +
// shared cells). The CSP gives the model exact geometry and crossings so it can
// focus purely on language — framed as a Danish crossword using Den Danske Ordbog.
func buildKrydsordSolvePrompt(cspJSON, board string, dictLines []string) string {
	var b strings.Builder
	b.WriteString("Du løser et DANSK skandinavisk krydsord (clue-square crossword).\n")
	b.WriteString("Alle svar er danske ord/udtryk og SKAL findes i Den Danske Ordbog (ordnet.dk/ddo). Ingen svenske/norske/engelske former.\n\n")
	b.WriteString(krydsordClueHints + "\n\n")
	if strings.TrimSpace(board) != "" {
		b.WriteString("BRÆT-LAYOUT (· = svar-felt i én post, + = krydsning mellem en vandret og en lodret post, blank = ikke et svar-felt). Række/kolonne-tal matcher celle-id'erne \"r<række>c<kolonne>\":\n")
		b.WriteString(board)
		b.WriteString("\n")
	}
	if len(dictLines) > 0 {
		b.WriteString("KENDTE SVAR FRA EGEN ORDBOG (set i tidligere krydsord — foretræk disse hvis længde + krydsninger passer):\n")
		for _, l := range dictLines {
			b.WriteString("- " + l + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(`Krydsordet er fladet ud som en CSP-struktur (JSON nedenfor):
- "entries": hver post (A* = vandret, D* = lodret) har "clue", "length" og "cells" — de celler posten fylder i rækkefølge. Celle nr. k i listen svarer til bogstav nr. k i svaret.
- "cells": for hver celle, listen af "POST:position" der deler cellen. Når en celle deles af flere poster, SKAL bogstavet være IDENTISK på de positioner (det er en krydsning).

CSP:
`)
	b.WriteString(cspJSON)
	b.WriteString(`

Fremgangsmåde (følg denne rækkefølge):
1. Løs FØRST de sikre, korte poster: tal→romertal (1500=MD), noder (MI=E), billeder (IMG: is-vaffel→IS), forkortelser, stedord/forholdsord. Disse er ankre.
2. Skriv ankrenes bogstaver ind i deres celler. Brug "cells"-kortet til at se, hvilke bogstaver de dermed LÅSER i de krydsende poster.
3. Løs de lange poster, så de matcher de allerede låste bogstaver (fx mønster "_ _ _ M E _ _ I _"). FORKAST et gæt der ikke passer mønsteret — også selvom ordet ellers passer ledetråden godt.
4. Gentag indtil alle poster er udfyldt og alle krydsninger stemmer.

KRAV (overhold dem nøje):
- Besvar HVER eneste post i "entries" — både alle A* (vandrette) og alle D* (lodrette). Udelad ingen.
- Tæl bogstaverne: hvert svar SKAL have præcis "length" bogstaver, hverken flere eller færre.
- Alle delte celler SKAL ende med samme bogstav i de poster der krydser der.

Returner KUN JSON i dette format (ingen anden tekst):
{"answers":[{"id":"A1","clue":"...","answer":"SVAR","confidence":"high|medium|low"}]}
- "answer" skal være med STORE bogstaver, kun danske bogstaver (A-Z, Æ, Ø, Å), og have præcis "length" bogstaver.
`)
	return b.String()
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
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
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
				return "", ctx.Err()
			}
			continue
		}
		return raw, nil
	}
	return "", fmt.Errorf("all 3 LLM attempts failed: %w", lastErr)
}

func wordProvider(cfg *config.Config, override string) (llm.JSONGenerator, error) {
	name := strings.TrimSpace(override)
	if name == "" {
		name = strings.TrimSpace(cfg.WordProvider)
	}
	if name == "" {
		name = "gemini"
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
		return nil, fmt.Errorf("unknown word provider %q — use a keyword (gemini|openai|xai|anthropic|openrouter) or a full OpenRouter model slug (e.g. google/gemini-3.1-pro-preview)", name)
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
				_ = br.WaitForLoad(ctx, "networkidle")
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
			notes := ordKloeverNotes(shape, revealSrc, probedThisRun, label)
			_ = upsertDailyGame(ctx, cfg, "Ordkløver", ordKloeverPrompt(st), phrase, true, solved, notes)
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

// parseIframeCellRefs returns @eXX refs for all clickable generic elements
// within the iframe section of a SnapshotInteractiveWithFrames snapshot, in
// DOM (row-major) order. Refs in excludeRefs are skipped.
//
// Both sudoku and krydsord grids expose their cells as:
//
//	generic [ref=eXX] clickable [cursor:pointer]          (empty / answer cell)
//	generic "N" [ref=eXX] clickable [cursor:pointer]      (sudoku given, value inline)
//
// Because cells are generic elements and control buttons are button elements,
// the generic prefix filter is sufficient to isolate cells for krydsord.
// For sudoku, number buttons (also generic) must be excluded via excludeRefs.
func parseIframeCellRefs(snap string, excludeRefs map[string]bool) []string {
	iframeSection := snap
	if idx := strings.Index(snap, "- Iframe [ref="); idx >= 0 {
		iframeSection = snap[idx:]
	} else if idx = strings.Index(snap, "Iframe [ref="); idx >= 0 {
		iframeSection = snap[idx:]
	}
	var refs []string
	for _, line := range strings.Split(iframeSection, "\n") {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "- ") // accessibility-tree list marker
		if !strings.Contains(trimmed, "[cursor:pointer]") || !strings.Contains(trimmed, "[ref=e") {
			continue
		}
		if !strings.HasPrefix(trimmed, "generic ") {
			continue
		}
		refStart := strings.Index(trimmed, "[ref=e")
		if refStart < 0 {
			continue
		}
		refEnd := strings.Index(trimmed[refStart:], "]")
		if refEnd < 0 {
			continue
		}
		// Extract "eXX" from "[ref=eXX]": skip the 5 bytes of "[ref="
		ref := "@" + trimmed[refStart+len("[ref="):refStart+refEnd]
		if !excludeRefs[ref] {
			refs = append(refs, ref)
		}
	}
	return refs
}

// parseIframeCellValues returns the current letter content of each cell in the
// same DOM order as parseIframeCellRefs. The format for a filled cell is:
//
//	generic [ref=eXX] clickable [cursor:pointer]
//	  - StaticText "E"
//
// Empty cells have rune(0). Used to verify API-save completion without extra
// browser calls: if all answer cells are filled, the save was applied.
func parseIframeCellValues(snap string) []rune {
	iframeSection := snap
	if idx := strings.Index(snap, "- Iframe [ref="); idx >= 0 {
		iframeSection = snap[idx:]
	} else if idx = strings.Index(snap, "Iframe [ref="); idx >= 0 {
		iframeSection = snap[idx:]
	}
	lines := strings.Split(iframeSection, "\n")
	var values []rune
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "- ") // accessibility-tree list marker
		if !strings.Contains(trimmed, "[cursor:pointer]") || !strings.Contains(trimmed, "[ref=e") {
			continue
		}
		if !strings.HasPrefix(trimmed, "generic ") {
			continue
		}
		// Cell ref found — peek at next non-empty line for a StaticText child.
		var ch rune
		for j := i + 1; j < len(lines) && j <= i+2; j++ {
			next := strings.TrimSpace(lines[j])
			next = strings.TrimPrefix(next, "- ")
			if next == "" {
				continue
			}
			// Pattern: StaticText "Æ" — a single Danish letter.
			if strings.HasPrefix(next, `StaticText "`) && strings.HasSuffix(next, `"`) {
				inner := []rune(next[len(`StaticText "`):len(next)-1])
				if len(inner) == 1 && isKrydsordAnswerLetter(inner[0]) {
					ch = inner[0]
				}
			}
			break // only look at the first non-empty child line
		}
		values = append(values, ch)
	}
	return values
}

// krydsordAnswerCellIndex returns the 0-based DOM-order index of the answer
// cell at (targetRow, targetCol) — both 0-indexed — within the solved grid.
// Answer cells are positions where the character is an uppercase Danish letter.
// The DOM order of cells in the snapshot matches row-major iteration of answer
// cells in solvedGrid. Returns -1 if not found.
func krydsordAnswerCellIndex(solvedGrid []string, targetRow, targetCol int) int {
	idx := 0
	for r, rowStr := range solvedGrid {
		for c, ch := range []rune(rowStr) {
			if isKrydsordAnswerLetter(ch) {
				if r == targetRow && c == targetCol {
					return idx
				}
				idx++
			}
		}
	}
	return -1
}

func isKrydsordAnswerLetter(ch rune) bool {
	return (ch >= 'A' && ch <= 'Z') || ch == 'Æ' || ch == 'Ø' || ch == 'Å'
}

// parseSudokuNumberRefs maps number-pad digits 1–9 to their @refs from a
// snapshot taken at the game iframe URL, where they render as
// `generic "N" [ref=eX]`. The grid cells are plain `.cell-<r>-<c>` divs (no
// cursor:pointer), so only the number buttons carry single-digit inline names.
func parseSudokuNumberRefs(snap string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(snap, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "- ")
		if !strings.HasPrefix(trimmed, "generic ") || !strings.Contains(trimmed, "[ref=e") {
			continue
		}
		q1 := strings.IndexByte(trimmed, '"')
		if q1 < 0 {
			continue
		}
		q2 := strings.IndexByte(trimmed[q1+1:], '"')
		if q2 < 0 {
			continue
		}
		name := trimmed[q1+1 : q1+1+q2]
		if len(name) != 1 || name[0] < '1' || name[0] > '9' {
			continue
		}
		refStart := strings.Index(trimmed, "[ref=")
		refEnd := strings.Index(trimmed[refStart:], "]")
		if refEnd < 0 {
			continue
		}
		ref := "@" + trimmed[refStart+len("[ref="):refStart+refEnd]
		if _, ok := out[name]; !ok {
			out[name] = ref
		}
	}
	return out
}

func submitSudoku(ctx context.Context, br *browser.Client, givens, solved klublotto.SudokuGrid) error {
	// The grid is a cross-origin OOPIF (sudoku.…mgame.nu) embedded in the parent.
	// The patched agent-browser can eval/click/snapshot inside OOPIFs via a
	// frame() switch, so we fill the EMBEDDED game (staying on danskespil.dk, so
	// the win registers) rather than navigating to the standalone game URL (which
	// redirects away). Inside the frame the grid is <div class="cell-<r>-<c>">
	// cells + a div.number ×9 pad.

	// Ensure the embedded game iframe is present. Extraction already loaded the
	// parent with it, so normally we DON'T re-open (a fresh re-open reloads the
	// parent and the iframe is re-added lazily, which timed out). Only re-open if
	// the iframe is somehow gone.
	hasIframe := func() bool {
		has, _ := br.Eval(ctx, `(() => !!Array.from(document.querySelectorAll('iframe')).find(f=>/sudoku/i.test(f.src)))()`)
		return strings.TrimSpace(has) == "true"
	}
	if !hasIframe() {
		if err := br.Open(ctx, klublotto.SudokuURL); err != nil {
			return fmt.Errorf("open sudoku parent: %w", err)
		}
		_ = br.WaitForLoad(ctx, "networkidle")
		deadline := time.Now().Add(30 * time.Second)
		for !hasIframe() {
			if time.Now().After(deadline) {
				return fmt.Errorf("sudoku game iframe did not appear on the parent page")
			}
			time.Sleep(1 * time.Second)
		}
	}

	// Enter the game iframe (OOPIF) and keep all subsequent eval/click inside it.
	entered := false
	for _, sel := range []string{"iframe.kl-game__iframe", "iframe[src*='sudoku']"} {
		if err := br.Frame(ctx, sel); err == nil {
			entered = true
			fmt.Printf("       entered game iframe via %q\n", sel)
			break
		}
	}
	if !entered {
		return fmt.Errorf("could not enter the sudoku game iframe")
	}
	defer func() { _ = br.Frame(context.Background(), "") }()

	// Wait for the 81 grid cells inside the frame.
	deadline := time.Now().Add(30 * time.Second)
	for {
		n, _ := br.Eval(ctx, `document.querySelectorAll('.cell').length`)
		if strings.TrimSpace(n) == "81" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sudoku grid (.cell ×81) did not render inside the iframe")
		}
		time.Sleep(1 * time.Second)
	}

	// Map number-pad buttons 1–9 to refs from the in-frame snapshot.
	snap, _ := br.SnapshotInteractiveCursor(ctx)
	numRefs := parseSudokuNumberRefs(snap)
	if len(numRefs) < 9 {
		return fmt.Errorf("only found %d/9 number-button refs inside the iframe: %v", len(numRefs), numRefs)
	}

	// Fill: click each empty cell by its unique class, then its number button.
	fmt.Println("       filling sudoku grid inside the embedded iframe (.cell-<r>-<c> + number pad)...")
	filled := 0
	for r := 0; r < 9; r++ {
		for c := 0; c < 9; c++ {
			if givens[r][c] != 0 {
				continue // skip pre-filled givens
			}
			n := strconv.Itoa(solved[r][c])
			numRef, hasRef := numRefs[n]
			if !hasRef {
				return fmt.Errorf("no ref for number %s (r%d c%d)", n, r+1, c+1)
			}
			cellSel := fmt.Sprintf(".cell-%d-%d", r, c)
			if err := br.Click(ctx, cellSel); err != nil {
				return fmt.Errorf("click cell %s: %w", cellSel, err)
			}
			time.Sleep(50 * time.Millisecond)
			if err := br.Click(ctx, numRef); err != nil {
				return fmt.Errorf("click number %s (%s) at r%d c%d: %w", n, numRef, r+1, c+1, err)
			}
			time.Sleep(70 * time.Millisecond)
			filled++
		}
	}
	fmt.Printf("       filled %d cells\n", filled)

	// Back to the parent and check for the success banner.
	_ = br.Frame(ctx, "")
	time.Sleep(1200 * time.Millisecond)
	if ok, detail := waitForSudokuSuccess(ctx, br); ok {
		fmt.Println("       success:", detail)
		return nil
	}
	return fmt.Errorf("filled %d cells but did not detect a success confirmation", filled)
}

func waitForSudokuSuccess(ctx context.Context, br *browser.Client) (bool, string) {
	successMarkers := []string{
		"vundet",
		"tillykke",
		"godt klaret",
		"dagens første lod",
		"du klarede",
		"rigtigt",
		"korrekt",
		"løst",
		"flot",
		"du løste",
		"besvaret",
		"great",
		"solved",
		"congratulations",
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := br.Eval(ctx, `(() => {
  const text = String(document.body ? (document.body.innerText || document.body.textContent || '') : '');
  return JSON.stringify({text});
})()`)
		if err == nil {
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(raw), &payload) == nil {
				low := strings.ToLower(payload.Text)
				for _, marker := range successMarkers {
					if strings.Contains(low, marker) {
						return true, marker
					}
				}
			}
		}
		time.Sleep(750 * time.Millisecond)
	}
	return false, ""
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
				_ = br.WaitForLoad(ctx, "networkidle")
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
	frameErr := br.Frame(ctx, "iframe.kl-game__iframe")
	if frameErr != nil {
		frameErr = br.Frame(ctx, "iframe[src*='ordkloever']")
	}
	if frameErr != nil {
		frameErr = br.Frame(ctx, "iframe[src*='clover']")
	}
	if frameErr == nil {
		defer func() {
			_ = br.Frame(context.Background(), "")
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
				_ = br.WaitForLoad(ctx, "networkidle")
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
	if ferr := br.Frame(ctx, "iframe.kl-game__iframe"); ferr == nil {
		inFrame = true
		defer func() { _ = br.Frame(context.Background(), "") }()
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
				_ = br.WaitForLoad(ctx, "networkidle")
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
	if ferr := br.Frame(ctx, "iframe.kl-game__iframe"); ferr == nil {
		inFrame = true
		defer func() { _ = br.Frame(context.Background(), "") }()
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
		_ = br.Frame(context.Background(), "")
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
	return fmt.Errorf("ordkloever: guess did not produce a win screen (answer may be wrong)")
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

func submitOrdknude(ctx context.Context, br *browser.Client, answer string, startIfNeeded bool) (string, error) {
	// Ensure we are on the Danske Spil *parent* page so that the game's lifecycle
	// events (completion, daily lod, checkmark on Spil & Quiz) fire correctly.
	// If we are already on the right page we skip the navigation entirely — this
	// avoids the iframe reload that was causing the "Velkommen" welcome screen to
	// appear on every guess and forcing an extra start-button click each time.
	curURL, _ := br.URL(ctx)
	onParent := strings.Contains(curURL, "danskespil.dk") && strings.Contains(curURL, "ordknude")
	if !onParent {
		fmt.Println("       navigating to Danske Spil Ordknude parent page...")
		var openErr error
		for i := 0; i < 3; i++ {
			if openErr = br.Open(ctx, klublotto.OrdknudeURL); openErr == nil {
				_ = br.WaitForLoad(ctx, "networkidle")
				time.Sleep(800 * time.Millisecond)
				break
			}
			if i < 2 {
				time.Sleep(1200 * time.Millisecond)
			}
		}
		if openErr != nil {
			_ = br.Screenshot(ctx, filepath.Join(".klublotto", "ordknude-open-fail-"+time.Now().UTC().Format("20060102-150405")+".png"))
			return "", fmt.Errorf("open parent to ensure embedded submit context: %w", openErr)
		}
	}

	normAnswer := klublotto.NormalizeDanishLetters(answer)
	fmt.Println("       typing answer via frames-inclusive parent snapshot...")

	// Use SnapshotInteractiveWithFrames — this exposes ALL iframe buttons (keyboard keys,
	// "SPIL ORDKNUDEN", "Retur") as clickable refs from the parent page, without needing
	// br.Frame() which fails with "Frame not found" for the cross-origin ordknude iframe.
	// This is the same approach that works reliably for Ordkløver letter/phrase input.
	snap, _ := br.SnapshotInteractiveWithFrames(ctx)

	// Click through welcome screen if showing (first guess of each run).
	if startIfNeeded {
		if ref := klublotto.FindRefByName(snap, []string{"SPIL ORDKNUDEN", "Spil Ordknuden", "spil ordknuden"}); ref != "" {
			fmt.Println("       clicking start button (welcome screen)...")
			_ = br.Click(ctx, ref)
			time.Sleep(1200 * time.Millisecond)
			snap, _ = br.SnapshotInteractiveWithFrames(ctx)
		}
		// Dismiss "Sådan spiller du" how-to modal if it appears.
		if ref := klublotto.FindRefByName(snap, []string{"Luk", "Close"}); ref != "" {
			_ = br.Click(ctx, ref)
			time.Sleep(500 * time.Millisecond)
			snap, _ = br.SnapshotInteractiveWithFrames(ctx)
		}
	}

	// Clear any stale partial input from a previous failed attempt.
	for i := 0; i < 5; i++ {
		_ = br.Press(ctx, "Backspace")
		time.Sleep(60 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	// Wait until keyboard is visible in the snapshot (retry up to ~4s).
	for attempt := 0; attempt < 8; attempt++ {
		if klublotto.FindRefByName(snap, []string{"q", "Q", "a", "A", "s", "S"}) != "" {
			break
		}
		fmt.Printf("       frames snapshot attempt %d: keyboard not ready yet, waiting...\n", attempt+1)
		time.Sleep(500 * time.Millisecond)
		snap, _ = br.SnapshotInteractiveWithFrames(ctx)
	}

	// Pre-build letter → ref map from the current snapshot.
	// The keyboard refs are stable within a single guess (the same key is always
	// the same ref), so we only need ONE snapshot for all 5 letter clicks.
	// This reduces typing from ~7 SnapshotInteractiveWithFrames calls to 1.
	allLetters := "QWERTYUIOPÅASDFGHJKLÆØZXCVBNM"
	letterRefs := make(map[string]string, len([]rune(allLetters)))
	for _, r := range []rune(allLetters) {
		ch := string(r)
		ref := klublotto.FindRefByName(snap, []string{ch, strings.ToLower(ch)})
		if ref != "" {
			letterRefs[ch] = ref
		}
	}
	returRef := klublotto.FindRefByName(snap, []string{"Retur", "RETUR", "retur"})

	// Type each letter by clicking its pre-looked-up keyboard button ref.
	for _, r := range []rune(normAnswer) {
		ch := strings.ToUpper(string(r))
		if ref, ok := letterRefs[ch]; ok {
			_ = br.Click(ctx, ref)
		} else {
			// Fallback: try a fresh snapshot (shouldn't be needed for A-Z Æ Ø Å).
			freshSnap, _ := br.SnapshotInteractiveWithFrames(ctx)
			if ref2 := klublotto.FindRefByName(freshSnap, []string{ch, strings.ToLower(ch)}); ref2 != "" {
				_ = br.Click(ctx, ref2)
			} else {
				fmt.Printf("       key %q not found in frames snapshot\n", ch)
			}
		}
		time.Sleep(120 * time.Millisecond)
	}

	time.Sleep(400 * time.Millisecond)
	_ = br.Screenshot(ctx, filepath.Join(".klublotto", "ordknude-mid-type.png"))

	// Submit with the Retur (Enter) key.
	if returRef == "" {
		// Refresh snapshot to find Retur if it wasn't in the initial snapshot.
		freshSnap, _ := br.SnapshotInteractiveWithFrames(ctx)
		returRef = klublotto.FindRefByName(freshSnap, []string{"Retur", "RETUR", "retur"})
	}
	if returRef != "" {
		_ = br.Click(ctx, returRef)
	} else {
		_ = br.Press(ctx, "Enter")
	}
	time.Sleep(800 * time.Millisecond)

	// Debug post-enter (pre poll) — last one as fixed name for easy inspection.
	_ = br.Screenshot(ctx, filepath.Join(".klublotto", "ordknude-mid-enter.png"))

	var resultSnap string
	for i := 0; i < 12; i++ {
		time.Sleep(500 * time.Millisecond)
		resultSnap, _ = br.Snapshot(ctx)
		low := strings.ToLower(resultSnap)
		if strings.Contains(low, "ordet findes ikke") ||
			strings.Contains(low, "tillykke") ||
			strings.Contains(low, "super imponerende") || // win: "Super imponerende!"
			strings.Contains(low, "du fandt frem til") || // win: "Du fandt frem til dagens ord"
			strings.Contains(low, "ord-haj") || // win: "Du er en sand ord-haj!"
			// NOTE: bare "vundet" intentionally omitted — the page nav permanently contains
			// "vundet eller tabt" (account overview link), so it fires on every page load.
			// NOTE: "dagens første lod" intentionally omitted — appears on the parent page
			// after ANY game earns the daily lod (e.g. krydsord solved earlier today).
			strings.Contains(low, "det rigtige svar var") || // loss: "The correct answer was:"
			strings.Contains(low, "lige ved og næsten") { // loss: "So close"
			break
		}
	}
	low := strings.ToLower(resultSnap)
	_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-submit-snap.txt"), []byte(resultSnap), 0o644)
	if strings.Contains(low, "ordet findes ikke") {
		return resultSnap, fmt.Errorf("Ordknuden rejected %s: Ordet findes ikke i vores database", answer)
	}
	return resultSnap, nil
}

func clearOrdknudePending(ctx context.Context, br *browser.Client) {
	// Send Backspace both via agent press (may work) and via explicit dispatch into the
	// iframe contentWindow. This clears any partial letters in the current row before we type.
	for i := 0; i < 5; i++ {
		_ = br.Press(ctx, "Backspace")
		_, _ = br.Eval(ctx, `(() => {
		  const opts = {key:'Backspace', code:'Backspace', bubbles:true, cancelable:true};
		  try { document.dispatchEvent(new KeyboardEvent('keydown', opts)); } catch(e){}
		  const ifr = document.querySelector('iframe[src*="ordknuden"], iframe[src*="ordknude"], .kl-game__iframe');
		  if (ifr && ifr.contentWindow) { try { ifr.contentWindow.dispatchEvent(new KeyboardEvent('keydown', opts)); } catch(e){} }
		})()`)
		time.Sleep(60 * time.Millisecond)
	}
}

func isImmerspieleURL(u string) bool {
	lu := strings.ToLower(u)
	return strings.Contains(lu, "immerspiele") || strings.Contains(lu, "klub-lotto.immerspiele.com")
}

func ordknudeGameFocusPoint(ctx context.Context, br *browser.Client) (pagePoint, error) {
	// Locate the embedded immerspiele iframe on the Danske Spil parent and return
	// a click point in the lower half (typically the on-screen keyboard or active
	// board area). Clicking here focuses the cross-origin game so that:
	//   1. Keyboard input reaches it.
	//   2. Its postMessage(gameCompleted, ...) goes to the real parent window,
	//      which is what registers the daily lod / checkmark on Spil & Quiz.
	js := `(() => {
	  const ifr = document.querySelector('iframe[src*="ordknuden"], iframe[src*="ordknude"], .kl-game__iframe');
	  if (!ifr) return JSON.stringify({ok:false, error:'no ordknude iframe'});
	  const r = ifr.getBoundingClientRect();
	  if (!r || r.width < 50 || r.height < 50) return JSON.stringify({ok:false, error:'tiny iframe'});
	  // Leftish upper tiles area (first cell of current/empty guess row). Clicking here reliably
	  // activates input for the row so that subsequent keyboard type delivers letters into the game.
	  // Tuned from live experiments on the embedded board (y~0.18, x~0.22 puts us on row 2 left tile).
	  const x = Math.round(r.left + r.width * 0.22);
	  const y = Math.round(r.top + r.height * 0.18);
	  return JSON.stringify({ok:true, x:x, y:y});
	})()`
	raw, err := br.Eval(ctx, js)
	if err != nil {
		return pagePoint{}, err
	}
	var res struct {
		Ok    bool   `json:"ok"`
		X     int    `json:"x"`
		Y     int    `json:"y"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return pagePoint{}, err
	}
	if !res.Ok {
		return pagePoint{}, fmt.Errorf("locate ordknude iframe focus point: %s", res.Error)
	}
	return pagePoint{X: res.X, Y: res.Y}, nil
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
				_ = br.WaitForLoad(ctx, "networkidle")
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

// clickOrdknudeVirtualKey clicks the on-screen virtual keyboard key for the given char
// using the iframe's bounding rect (from parent; cross-origin prevents reading contentDocument
// for exact key elems) + observed 3-row Danish layout proportions to compute center of each key.
// We also dispatch the unicode key event as a second channel (some games listen to one or the other).
// A prior focus click on the board area helps activate the input row.
func clickOrdknudeVirtualKey(ctx context.Context, br *browser.Client, ch rune) error {
	upper := unicode.ToUpper(ch)
	// Get iframe rect in page coords (CSS px; mouse uses same units; dpr not needed here).
	js := `(() => {
	  const ifr = document.querySelector('iframe[src*="ordknuden"], iframe[src*="ordknude"], .kl-game__iframe');
	  if (!ifr) {
	    const w = window.innerWidth || 1200;
	    const h = window.innerHeight || 800;
	    return JSON.stringify({ok:true, left:0, top:0, width:w, height:h});
	  }
	  const r = ifr.getBoundingClientRect();
	  if (r.width < 50 || r.height < 50) {
	    const w = window.innerWidth || 1200;
	    const h = window.innerHeight || 800;
	    return JSON.stringify({ok:true, left:0, top:0, width:w, height:h});
	  }
	  return JSON.stringify({ok:true, left:r.left, top:r.top, width:r.width, height:r.height});
	})()`
	raw, err := br.Eval(ctx, js)
	if err != nil {
		// dispatch only
		dispatchKey(ctx, br, string(ch))
		return nil
	}
	var rect struct {
		Ok             bool
		Left, Top, Width, Height float64
	}
	if json.Unmarshal([]byte(raw), &rect) != nil || !rect.Ok {
		dispatchKey(ctx, br, string(ch))
		return nil
	}
	// Proportions measured from live screenshots: the 6-tile board occupies the upper ~72%
	// of the iframe, and the 3-row on-screen keyboard occupies the lower ~24% (kbStartY~0.75).
	// The old values (0.62 / 0.37) put clicks inside the tile board, not on the keys.
	kbStartY := 0.75
	kbH := 0.24
	rowH := kbH / 3.0
	kbY := rect.Top + rect.Height*kbStartY
	var row int
	var keys string
	idx := -1
	switch {
	case strings.ContainsRune("QWERTYUIOPÅ", upper):
		row = 0
		keys = "QWERTYUIOPÅ"
		idx = strings.IndexRune(keys, upper)
	case strings.ContainsRune("ASDFGHJKLÆØ", upper):
		row = 1
		keys = "ASDFGHJKLÆØ"
		idx = strings.IndexRune(keys, upper)
	case strings.ContainsRune("ZXCVBNM", upper):
		row = 2
		keys = "ZXCVBNM"
		idx = strings.IndexRune(keys, upper)
	default:
		dispatchKey(ctx, br, string(ch))
		return nil
	}
	if idx < 0 {
		dispatchKey(ctx, br, string(ch))
		return nil
	}
	n := float64(len(keys))
	keyW := rect.Width / (n + 0.5)
	x := rect.Left + float64(idx)*keyW + keyW*0.5
	if row == 2 {
		x = rect.Left + rect.Width*0.05 + float64(idx)*(rect.Width*0.09)
	}
	y := kbY + float64(row)*rowH + rowH*0.5
	// Mouse click the computed key position (to hit the visual kb element).
	br.MouseClick(ctx, int(x), int(y))
	time.Sleep(50 * time.Millisecond)
	// Also dispatch (covers games that advance on key events after focus).
	dispatchKey(ctx, br, string(ch))
	time.Sleep(40 * time.Millisecond)
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

// clickOrdknudeEnter clicks the RETUR (enter) key on the virtual kb (right side of bottom row).
func clickOrdknudeEnter(ctx context.Context, br *browser.Client) error {
	// Compute RETUR position from the iframe rect (parent) and click it (to hit the visual
	// RETUR key), plus dispatch Enter. Use a y frac ~0.85 to be on the bottom kb row, not
	// the help button below it. This (with the letter virtual clicks) should commit without
	// causing the welcome/menu flicker.
	js := `(() => {
	  const ifr = document.querySelector('iframe[src*="ordknuden"], iframe[src*="ordknude"], .kl-game__iframe');
	  if (!ifr) return JSON.stringify({ok:false});
	  const r = ifr.getBoundingClientRect();
	  if (r.width < 50 || r.height < 50) return JSON.stringify({ok:false});
	  return JSON.stringify({ok:true, left:r.left, top:r.top, width:r.width, height:r.height});
	})()`
	raw, _ := br.Eval(ctx, js)
	var rect struct {
		Ok bool
		Left, Top, Width, Height float64
	}
	if json.Unmarshal([]byte(raw), &rect) == nil && rect.Ok {
		// Keyboard starts at ~0.75 of iframe height; 3 rows each ~0.08 tall.
		// Bottom-row (RETUR) centre is at kbStart + 2*rowH + rowH/2 ≈ 0.91.
		kbY := rect.Top + rect.Height*0.75
		rowH := rect.Height * 0.24 / 3
		x := rect.Left + rect.Width * 0.82
		y := kbY + 2*rowH + rowH*0.5
		br.MouseClick(ctx, int(x), int(y))
		time.Sleep(50 * time.Millisecond)
	}
	// Always dispatch too.
	_, _ = br.Eval(ctx, `(() => {
	  const opts = {key: 'Enter', code: 'Enter', bubbles:true, cancelable:true};
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
	})()`)
	time.Sleep(60 * time.Millisecond)
	return nil
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
	if ferr := br.Frame(ctx, "iframe.kl-game__iframe"); ferr == nil {
		defer func() { _ = br.Frame(context.Background(), "") }()
		isnap, _ := br.SnapshotInteractive(ctx)
		if ref := klublotto.FindRefByName(isnap, names); ref != "" {
			_ = br.Click(ctx, ref)
			time.Sleep(1200 * time.Millisecond)
		}
	} else {
		_ = br.Frame(context.Background(), "")
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

func sudokuCellSelectors(r, c int) []string {
	oneR, oneC := r+1, c+1
	return []string{
		fmt.Sprintf("iframe >> [data-row='%d'][data-col='%d']", r, c),
		fmt.Sprintf("iframe >> [data-row='%d'][data-col='%d']", oneR, oneC),
		fmt.Sprintf("iframe >> [aria-rowindex='%d'][aria-colindex='%d']", oneR, oneC),
		fmt.Sprintf("iframe >> .cell-%d-%d", r, c),
		fmt.Sprintf("iframe >> .cell-%d-%d", oneR, oneC),
		fmt.Sprintf("[data-row='%d'][data-col='%d']", r, c),
		fmt.Sprintf("[data-row='%d'][data-col='%d']", oneR, oneC),
		fmt.Sprintf(".cell-%d-%d", r, c),
		fmt.Sprintf(".cell-%d-%d", oneR, oneC),
	}
}

func sudokuNumberSelectors(n string) []string {
	return []string{
		"iframe >> button:has-text('" + n + "')",
		"iframe >> [role='button']:has-text('" + n + "')",
		"button:has-text('" + n + "')",
		"[role='button']:has-text('" + n + "')",
	}
}

func printCandidates(cands []klublotto.WordCandidate) {
	fmt.Println()
	fmt.Println("== Candidates ==")
	for i, c := range cands {
		fmt.Printf("%d. %s (%s) — %s\n", i+1, c.Answer, c.Confidence, c.Rationale)
	}
}

func printOrdknudeState(st klublotto.OrdknudeState) {
	fmt.Println("┌─────────────────────────────────────┐")
	fmt.Printf("│  Ordknuden — %d guesses, %d remaining  │\n", len(st.History), st.Remaining)
	fmt.Println("├─────────────────────────────────────┤")
	if len(st.History) == 0 {
		fmt.Println("│  (no guesses yet)                   │")
	} else {
		for i, h := range st.History {
			emojis := make([]string, len(h.Marks))
			absent := []string{}
			for j, m := range h.Marks {
				switch m {
				case "correct":
					emojis[j] = "🟩"
				case "present":
					emojis[j] = "🟨"
				default:
					emojis[j] = "⬛"
					runes := []rune(h.Word)
					if j < len(runes) {
						absent = append(absent, string(runes[j]))
					}
				}
			}
			fmt.Printf("│  %d. %s  %s\n", i+1, h.Word, strings.Join(emojis, ""))
		}
		// Summarise what we know
		fmt.Println("├─────────────────────────────────────┤")
		knownCorrect := map[int]rune{}
		mustHave := map[rune]bool{}
		mustNot := map[rune]bool{}
		for _, h := range st.History {
			for i, m := range h.Marks {
				runes := []rune(h.Word)
				if i >= len(runes) {
					continue
				}
				ch := runes[i]
				switch m {
				case "correct":
					knownCorrect[i+1] = ch
					mustHave[ch] = true
				case "present":
					mustHave[ch] = true
				case "absent":
					mustNot[ch] = true
				}
			}
		}
		// letters that are also correct/present are not truly absent
		for ch := range mustHave {
			delete(mustNot, ch)
		}
		if len(knownCorrect) > 0 {
			positions := []int{1, 2, 3, 4, 5}
			pattern := []rune{'_', '_', '_', '_', '_'}
			for _, p := range positions {
				if ch, ok := knownCorrect[p]; ok {
					pattern[p-1] = ch
				}
			}
			fmt.Printf("│  Pattern : %s\n", string(pattern))
		}
		if len(mustNot) > 0 {
			banned := []rune{}
			for ch := range mustNot {
				banned = append(banned, ch)
			}
			sort.Slice(banned, func(i, j int) bool { return banned[i] < banned[j] })
			fmt.Printf("│  ⬛ Banned: %s\n", string(banned))
		}
		if len(mustHave) > 0 {
			have := []rune{}
			for ch := range mustHave {
				if _, ok := knownCorrect[0]; !ok { // only non-fixed
					have = append(have, ch)
				}
			}
			if len(have) > 0 {
				sort.Slice(have, func(i, j int) bool { return have[i] < have[j] })
				fmt.Printf("│  🟨 Must  : %s\n", string(have))
			}
		}
	}
	fmt.Println("└─────────────────────────────────────┘")
	if st.Solved {
		fmt.Println("✅ SOLVED:", st.Answer)
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

func alreadyTried(word string, history []klublotto.OrdknudeGuess) bool {
	w := klublotto.NormalizeDanishLetters(word)
	for _, h := range history {
		if klublotto.NormalizeDanishLetters(h.Word) == w {
			return true
		}
	}
	return false
}

// filterOrdknudeCandidates removes candidates that are:
//   - not exactly 5 Danish letters
//   - already in the game history
//   - in the rejected-words list
//   - duplicates within the batch (keeps first occurrence)
func filterOrdknudeCandidates(cands []klublotto.WordCandidate, st klublotto.OrdknudeState, rejected []string) []klublotto.WordCandidate {
	seen := map[string]bool{}
	out := make([]klublotto.WordCandidate, 0, len(cands))
	for _, c := range cands {
		word := klublotto.NormalizeDanishLetters(c.Answer)
		if word == "" || seen[word] {
			continue
		}
		if !klublotto.IsDanishFiveLetterWord(word) {
			continue
		}
		if alreadyTried(word, st.History) {
			continue
		}
		if containsWord(rejected, word) {
			continue
		}
		seen[word] = true
		c.Answer = word
		out = append(out, c)
	}
	return out
}

// extractOrdknudeAnswerFromSnap parses the agent-browser accessibility snapshot
// of the Ordknude result screen and returns the correct answer.
//
// The result screen snapshot contains:
//
//	- paragraph: "Det rigtige svar var:"
//	- paragraph: gummi
//
// For a win it contains "Tillykke" and the answer word in the board.
func extractOrdknudeAnswerFromSnap(snap string) string {
	lines := strings.Split(snap, "\n")
	for i, line := range lines {
		if !strings.Contains(strings.ToLower(line), "det rigtige svar var") {
			continue
		}
		// The answer is the next TEXT node after the marker. In the accessibility
		// snapshot the role label and its text are on separate lines:
		//     - paragraph
		//       - StaticText "binde"
		// so we must skip role-only lines ("- paragraph", "- generic") and read
		// the next StaticText/text content — grabbing a role line is exactly how we
		// previously reported "PARAGRAPH" instead of "binde".
		for j := i + 1; j < len(lines) && j < i+10; j++ {
			if word := ordknudeWordFromSnapLine(lines[j]); word != "" {
				return word
			}
		}
	}
	return ""
}

// ordknudeWordFromSnapLine returns the normalized word carried by a snapshot
// text node line (e.g. `- StaticText "binde"` → "BINDE"), or "" if the line is a
// role-only node ("- paragraph", "- generic", …) or has no usable word.
func ordknudeWordFromSnapLine(line string) string {
	line = strings.TrimSpace(line)
	var val string
	switch {
	case strings.Contains(line, "StaticText"):
		if q1 := strings.Index(line, `"`); q1 >= 0 {
			if q2 := strings.LastIndex(line, `"`); q2 > q1 {
				val = line[q1+1 : q2]
			}
		}
	case strings.HasPrefix(line, "- text:"):
		val = strings.Trim(strings.TrimPrefix(line, "- text:"), ` "`)
	default:
		return "" // role-only line — not the answer text
	}
	word := klublotto.NormalizeDanishLetters(val)
	if len([]rune(word)) >= 3 {
		return word
	}
	return ""
}

func gridOneLine(g klublotto.SudokuGrid) string {
	return strings.ReplaceAll(klublotto.FormatSudokuGrid(g), "\n", " / ")
}

// ordKloeverNotes builds the daily-ledger "Notes" cell for a finished Ordkløver
// round: the colour-coded letter-probe sequence (🟩 = letter is in the answer,
// 🟥 = miss), the answer shape, and how the round was finished. revealSrc is the
// string we colour letters against — the solved answer when solved, otherwise the
// revealed board.
func ordKloeverNotes(shape, revealSrc string, probed []string, label string) string {
	var parts []string
	if seq := colourCodeOrdKloeverLetters(probed, revealSrc); seq != "" {
		parts = append(parts, "Bogstavgæt: "+seq)
	}
	if shape != "" {
		parts = append(parts, "Mønster: "+shape)
	}
	if label != "" {
		parts = append(parts, label)
	}
	return strings.Join(parts, " · ")
}

// colourCodeOrdKloeverLetters returns the probed letters in order, de-duplicated,
// each tagged 🟩 if it appears in revealSrc (a hit) or 🟥 if not (a miss).
func colourCodeOrdKloeverLetters(probed []string, revealSrc string) string {
	hit := map[rune]bool{}
	for _, r := range []rune(klublotto.NormalizeDanishLetters(revealSrc)) {
		hit[r] = true
	}
	seen := map[rune]bool{}
	var out []string
	for _, l := range probed {
		l = klublotto.NormalizeDanishLetters(l)
		if l == "" {
			continue
		}
		r := []rune(l)[0]
		if seen[r] {
			continue
		}
		seen[r] = true
		mark := "🟥"
		if hit[r] {
			mark = "🟩"
		}
		out = append(out, string(r)+mark)
	}
	return strings.Join(out, " ")
}

// ordknudeGuessNotes builds the daily-ledger "Notes" cell for a finished
// Ordknuden round: the ordered guess sequence with each tile colour-coded
// (🟩 correct, 🟨 present, 🟥 absent). answer is the actual solution (the winning
// word on a win, the revealed correct word on a loss); marks not present in the
// extracted history are reconstructed by scoring each guess against it.
func ordknudeGuessNotes(tried []string, history []klublotto.OrdknudeGuess, answer string) string {
	marksByWord := map[string][]string{}
	for _, h := range history {
		marksByWord[klublotto.NormalizeDanishLetters(h.Word)] = h.Marks
	}
	answer = klublotto.NormalizeDanishLetters(answer)
	var parts []string
	for i, w := range tried {
		w = klublotto.NormalizeDanishLetters(w)
		marks := marksByWord[w]
		if len(marks) != 5 {
			marks = scoreOrdknudeGuess(w, answer)
		}
		sq := ordknudeMarkSquares(marks)
		if sq == "" && answer != "" && w == answer {
			sq = "🟩🟩🟩🟩🟩" // winning guess — re-extract may not have caught it
		}
		parts = append(parts, fmt.Sprintf("%d. %s %s", i+1, w, sq))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Gæt: " + strings.Join(parts, " · ")
}

// scoreOrdknudeGuess marks a 5-letter guess against the known answer using
// Wordle rules: exact matches are "correct", remaining letters that exist
// elsewhere (each answer letter consumed once) are "present", the rest "absent".
func scoreOrdknudeGuess(guess, answer string) []string {
	g := []rune(klublotto.NormalizeDanishLetters(guess))
	a := []rune(klublotto.NormalizeDanishLetters(answer))
	if len(g) != 5 || len(a) != 5 {
		return nil
	}
	marks := make([]string, 5)
	used := make([]bool, 5)
	for i := 0; i < 5; i++ {
		if g[i] == a[i] {
			marks[i] = "correct"
			used[i] = true
		}
	}
	for i := 0; i < 5; i++ {
		if marks[i] != "" {
			continue
		}
		marks[i] = "absent"
		for j := 0; j < 5; j++ {
			if !used[j] && g[i] == a[j] {
				marks[i] = "present"
				used[j] = true
				break
			}
		}
	}
	return marks
}

// mergeGuessWords returns the ordered, de-duplicated guess list: the board
// history first (oldest→newest), then any words submitted this run that the
// win/loss overlay wiped from the extracted history (e.g. the final guess).
func mergeGuessWords(history []klublotto.OrdknudeGuess, tried []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(w string) {
		w = klublotto.NormalizeDanishLetters(w)
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, h := range history {
		add(h.Word)
	}
	for _, w := range tried {
		add(w)
	}
	return out
}

func ordknudeMarkSquares(marks []string) string {
	var b strings.Builder
	for _, m := range marks {
		switch m {
		case "correct":
			b.WriteString("🟩")
		case "present":
			b.WriteString("🟨")
		case "absent":
			b.WriteString("🟥")
		}
	}
	return b.String()
}

func ordKloeverPrompt(st klublotto.OrdKloeverState) string {
	parts := []string{}
	if st.Category != "" {
		parts = append(parts, "Category: `"+st.Category+"`")
	}
	if st.Hint != "" {
		parts = append(parts, "hint: `"+st.Hint+"`")
	}
	if st.Shape != "" {
		parts = append(parts, "answer pattern `"+st.Shape+"`")
	}
	if st.VisualShape != "" && st.VisualShape != st.Shape {
		parts = append(parts, "visual layout `"+st.VisualShape+"`")
	}
	return strings.Join(parts, "; ")
}

func countKrydsordSlots(slots []klublotto.KrydsordSlot, direction string) int {
	n := 0
	for _, slot := range slots {
		if slot.Direction == direction {
			n++
		}
	}
	return n
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

func submitKrydsord(ctx context.Context, br *browser.Client, data klublotto.KrydsordData, solvedGrid []string) error {
	if len(solvedGrid) == 0 {
		return fmt.Errorf("no solved grid to submit")
	}
	userSol := klublotto.BuildKrydsordUserSolution(data, solvedGrid)
	fmt.Printf("       saving solution via API (len=%d, crossword_id=%s)...\n", len(userSol), data.CrosswordID)
	if err := klublotto.SetKrydsordUserSolutionViaAPI(ctx, br, data.IframeURL, data.CrosswordID, userSol); err != nil {
		return err
	}

	// Re-open the Danske Spil *parent* so the iframe is embedded; the parent receives
	// gameCompleted etc and awards the daily lod. (Direct iframe would bypass credit.)
	fmt.Println("       opening parent page (iframe embedded for registration)...")
	if err := br.Open(ctx, klublotto.KrydsordURL); err != nil {
		return fmt.Errorf("open parent for krydsord submit: %w", err)
	}
	_ = br.WaitForLoad(ctx, "networkidle")
	time.Sleep(1200 * time.Millisecond)

	// Snapshot from the parent page to find cell and button refs exposed by the
	// new agent-browser. Both grid cells and the "TJEK LØSNING" / "GEM" buttons
	// are visible as clickable refs from the parent page (cross-frame refs).
	snap, _ := br.SnapshotInteractiveWithFrames(ctx)
	cellRefs := parseIframeCellRefs(snap, nil)
	tjekRef := klublotto.FindRefByName(snap, []string{"TJEK LØSNING", "TJEK LOSNING"})
	gemRef := klublotto.FindRefByName(snap, []string{"GEM"})

	// Verify API save: count how many cells are already filled in the snapshot.
	// After a successful API save the iframe reloads with all letters visible
	// as StaticText children of each cell ref.
	cellValues := parseIframeCellValues(snap)
	filled := 0
	for _, ch := range cellValues {
		if ch != 0 {
			filled++
		}
	}
	fmt.Printf("       cell refs: %d  filled: %d/%d  tjekRef: %q  gemRef: %q\n",
		len(cellRefs), filled, len(cellValues), tjekRef, gemRef)

	// The API save does NOT reliably populate the board (it can leave it empty),
	// so fill every answer cell directly via its ref — click the cell and type its
	// letter — exactly like the sudoku submit. cellRefs are the answer cells in
	// row-major order, matching the order we walk the solved grid; KeyboardType
	// handles Danish letters (Æ/Ø/Å) that Press can't.
	if len(cellRefs) == 0 {
		return fmt.Errorf("no krydsord cell refs found — cannot fill the board")
	}
	fmt.Printf("       filling %d answer cells via refs (API save left %d/%d)...\n", len(cellRefs), filled, len(cellRefs))
	k := 0
	for _, rowstr := range solvedGrid {
		for _, ch := range []rune(rowstr) {
			if !isKrydsordAnswerLetter(ch) {
				continue
			}
			if k < len(cellRefs) {
				if err := br.Click(ctx, cellRefs[k]); err == nil {
					time.Sleep(150 * time.Millisecond) // let the click focus the cell
					// The game captures document-level keydowns, so Press works for
					// every letter — including Æ/Ø/Å (KeyboardType's insertText needs
					// a focused input the cells don't have, so it silently dropped them).
					_ = br.Press(ctx, string(ch))
					time.Sleep(80 * time.Millisecond)
				}
			}
			k++
		}
	}
	time.Sleep(1500 * time.Millisecond) // let the grid commit the typed letters

	// Best-effort read of how many cells show a value. NOTE: the cell StaticText
	// values often haven't committed in the DOM immediately after typing (they
	// render on blur/commit), so a low count here is NOT reliable — we proceed to
	// Tjek løsning regardless and let the result banner be the source of truth.
	snap2, _ := br.SnapshotInteractiveWithFrames(ctx)
	filled = 0
	for _, ch := range parseIframeCellValues(snap2) {
		if ch != 0 {
			filled++
		}
	}
	fmt.Printf("       typed %d cells (DOM shows %d committed — read is best-effort mid-edit)\n", k, filled)
	if r := klublotto.FindRefByName(snap2, []string{"TJEK LØSNING", "TJEK LOSNING"}); r != "" {
		tjekRef = r
	}
	if r := klublotto.FindRefByName(snap2, []string{"GEM"}); r != "" {
		gemRef = r
	}

	// Click "TJEK LØSNING" via ref (preferred) or fall back to name search.
	fmt.Println("       clicking Tjek løsning (on parent)...")
	var checkErr error
	switch {
	case tjekRef != "":
		checkErr = br.Click(ctx, tjekRef)
	case gemRef != "":
		checkErr = br.Click(ctx, gemRef)
	default:
		checkErr = clickInteractiveByName(ctx, br, "Tjek løsning", "Tjek løsning", "GEM", "Tjek")
	}
	if checkErr != nil {
		return fmt.Errorf("click Tjek løsning: %w", checkErr)
	}
	time.Sleep(1500 * time.Millisecond)

	if ok, detail := waitForKrydsordSuccess(ctx, br); ok {
		fmt.Println("       success detected:", detail)
		return nil
	} else {
		return fmt.Errorf("Krydsord not confirmed solved: %s", detail)
	}
}

func waitForKrydsordSuccess(ctx context.Context, br *browser.Client) (bool, string) {
	// Krydsord-SPECIFIC success banners only. NOTE: do NOT match "vundet" — the
	// page's permanent footer/nav contains "vundet eller tabt", which previously
	// produced a FALSE success on an unsolved board. "tillykke"/"dagens lod" are
	// likewise too generic.
	success := []string{"hvor er du vild", "ordmester", "løste dagens krydsord"}
	// Explicit failure overlay the game shows on a wrong/incomplete solution.
	failure := []string{"ikke løst korrekt", "prøv igen", "opgaven er ikke løst"}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := br.Eval(ctx, `(() => { const t = String(document.body ? (document.body.innerText || document.body.textContent || '') : ''); return JSON.stringify({text:t}); })()`)
		if err == nil {
			var p struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(raw), &p) == nil {
				low := strings.ToLower(p.Text)
				for _, m := range failure {
					if strings.Contains(low, m) {
						return false, "ikke løst korrekt (" + m + ")"
					}
				}
				for _, m := range success {
					if strings.Contains(low, m) {
						return true, m
					}
				}
			}
		}
		time.Sleep(600 * time.Millisecond)
	}
	return false, "no success banner (board likely not filled or solution wrong)"
}

func pickASCIIFixCell(data klublotto.KrydsordData, grid []string) (klublotto.KrydsordCell, rune) {
	prefer := "ERTASILNØ"
	for _, pref := range prefer {
		for r, rowstr := range grid {
			for c, ch := range []rune(rowstr) {
				if ch == pref {
					if (ch >= 'A' && ch <= 'Z') || ch == 'Æ' || ch == 'Ø' || ch == 'Å' {
						return klublotto.KrydsordCell{Row: r + 1, Col: c + 1}, ch
					}
				}
			}
		}
	}
	for r, rowstr := range grid {
		for c, ch := range []rune(rowstr) {
			if (ch >= 'A' && ch <= 'Z') || ch == 'Æ' || ch == 'Ø' || ch == 'Å' {
				return klublotto.KrydsordCell{Row: r + 1, Col: c + 1}, ch
			}
		}
	}
	return klublotto.KrydsordCell{}, 0
}

func assembleKrydsordSolutionGrid(ctx context.Context, cfg *config.Config, provider string, data klublotto.KrydsordData, clues []klublotto.KrydsordClue, perSlot map[string][]klublotto.WordCandidate, allClueTexts []string) ([]string, error) {
	p, err := wordProvider(cfg, provider)
	if err != nil {
		return nil, err
	}
	// Ask the model for ONE answer per slot id, NOT the whole grid. Go then
	// places the letters deterministically from each slot's known cells, so the
	// grid dimensions are always correct. Emitting the grid directly produced
	// persistent "row N has 11 columns" / blank-cell errors no retry could fix.
	slots := klublotto.BuildKrydsordSlots(data)
	cluesByID := map[string]klublotto.KrydsordClue{}
	for _, cl := range clues {
		cluesByID[cl.SlotID] = cl
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Løs dette danske krydsord (clues-in-squares). VÆLG for HVER slot ét dansk svar med PRÆCIS den angivne længde, så bogstaverne passer ved ALLE krydsninger (celler delt mellem to slots skal have samme bogstav).\n")
	fmt.Fprintf(&b, "Billedledetråde står som engelske beskrivelser (fx \"grill\", \"t-shirt\", \"turnip\", \"desk lamp\") — svar med det danske ord for tingen (GRILL, TSHIRT, ROE, LAMPE).\n")
	if len(allClueTexts) > 0 {
		fmt.Fprintf(&b, "Alle synlige ledetråde (OCR-tildelingen pr. slot kan være unøjagtig): %q\n", allClueTexts)
	}
	fmt.Fprintf(&b, "\nSlots (id, retning, længde, ledetråd, celler r<row>c<col>, kandidater):\n")
	for _, s := range slots {
		cl := cluesByID[s.ID]
		var candList []string
		for _, c := range perSlot[s.ID] {
			a := klublotto.NormalizeDanishLetters(c.Answer)
			if len([]rune(a)) == s.Length {
				candList = append(candList, a)
			}
		}
		cellIDs := make([]string, 0, len(s.Cells))
		for _, cell := range s.Cells {
			cellIDs = append(cellIDs, fmt.Sprintf("r%dc%d", cell.Row, cell.Col))
		}
		kind := ""
		if cl.IsImage {
			kind = " BILLEDE"
		}
		fmt.Fprintf(&b, "- %s %s len=%d clue=%q%s cells=%s cands=%v\n", s.ID, s.Direction, s.Length, cl.Clue, kind, strings.Join(cellIDs, ","), candList)
	}
	b.WriteString("\nReturnér KUN JSON: {\"answers\":{\"A1\":\"ORD\",\"D1\":\"ORD\", ...}} med ét svar pr. slot-id ovenfor. Kun bogstaver (ÆØÅ tilladt), ingen mellemrum/tegn. INGEN markdown.\n")
	basePrompt := b.String()

	// Retry, feeding back which slots are missing or which crossings conflict.
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		prompt := basePrompt
		if lastErr != nil {
			prompt += fmt.Sprintf("\nForrige forsøg var forkert: %v\nRet svarene: hvert slot skal have et svar med korrekt længde, og delte celler skal have samme bogstav.\n", lastErr)
		}
		modelCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
		raw, genErr := p.GenerateJSON(modelCtx, prompt, 0.05)
		cancel()
		if genErr != nil {
			lastErr = genErr
			fmt.Printf("       [assemble] attempt %d/%d: model error: %v\n", attempt, maxAttempts, genErr)
			continue
		}
		answers, parseErr := klublotto.ParseKrydsordAnswerMap(raw)
		if parseErr != nil || len(answers) == 0 {
			lastErr = fmt.Errorf("parse per-slot answers: %v", parseErr)
			fmt.Printf("       [assemble] attempt %d/%d: %v\n", attempt, maxAttempts, lastErr)
			continue
		}
		grid, conflicts := buildKrydsordGridFromSlotAnswers(data, slots, answers)
		check := klublotto.ValidateKrydsordAnswerGrid(data, grid)
		if check.OK && check.FilledN == check.AnswerN && len(conflicts) == 0 {
			if attempt > 1 {
				fmt.Printf("       [assemble] consistent solution on attempt %d/%d\n", attempt, maxAttempts)
			}
			return grid, nil
		}
		errs := append([]string{}, check.Errors...)
		errs = append(errs, conflicts...)
		if len(errs) > 8 {
			errs = append(errs[:8], "…")
		}
		lastErr = fmt.Errorf("filled %d/%d answer cells, %d crossing conflicts: %v", check.FilledN, check.AnswerN, len(conflicts), errs)
		fmt.Printf("       [assemble] attempt %d/%d invalid: %v\n", attempt, maxAttempts, lastErr)
	}
	return nil, fmt.Errorf("krydsord assembly failed after %d attempts: %w", maxAttempts, lastErr)
}

func gridOneLineKrydsord(g []string) string {
	s := strings.Join(g, " / ")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
