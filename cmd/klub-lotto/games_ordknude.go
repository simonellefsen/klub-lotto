package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
	"github.com/simonellefsen/klub-lotto/internal/llm"
)

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
		lastSubmittedAnswer := ""  // tracks the most recent word submitted (for end-of-game recording)
		triedThisRun := []string{} // words submitted in this run (guards against re-suggest when re-extract fails)
		// pool holds DDO-valid candidates from the most recent LLM call that we
		// haven't tried yet. We reuse it across wrong guesses — picking another at
		// random and re-querying the LLM only when the pool is empty — so a tightly
		// constrained board (e.g. only one letter unknown) doesn't pay for a fresh
		// LLM round on every attempt.
		var pool []klublotto.WordCandidate
		// noConsistentRounds counts consecutive provider batches that produced no
		// word respecting the known green pattern. We re-ask rather than submit a
		// pattern-violating guess; after a few rounds we fall back to a local word.
		noConsistentRounds := 0
		// lastGoodHistory keeps the most recent non-empty board history: the win/
		// loss overlay re-extract returns "0 guesses", wiping st.History, so we
		// snapshot it here to reconstruct the guess sequence for the ledger.
		var lastGoodHistory []klublotto.OrdknudeGuess
		// prunePool drops tried/rejected/invalid words and any no longer consistent
		// with the observed marks (a wrong guess tightens the constraints).
		prunePool := func(in []klublotto.WordCandidate) []klublotto.WordCandidate {
			var out []klublotto.WordCandidate
			for _, c := range klublotto.FilterOrdknudeCandidates(in, st, rejected) {
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
					beforeDDO := klublotto.FilterOrdknudeCandidates(cands, st, rejected)
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
						// Keep only fully-constraint-consistent words as the reusable pool.
						pool = prunePool(validated)
						if len(pool) == 0 {
							// Full consistency over-pruned (often a mis-read yellow). Keep at
							// least the words that respect the confirmed GREEN letters — we
							// must never submit a word that contradicts a known green (e.g.
							// GRUBE when the pattern is G R _ D E).
							for _, c := range validated {
								if klublotto.ConsistentWithOrdknudeGreens(c.Answer, st.History) {
									pool = append(pool, c)
								}
							}
						}
						if len(pool) == 0 {
							// Every provider candidate violates a known green letter. Reject
							// the whole batch and ask again rather than wasting a real guess.
							for _, c := range validated {
								w := klublotto.NormalizeDanishLetters(c.Answer)
								if !containsWord(rejected, w) {
									rejected = append(rejected, w)
								}
							}
							noConsistentRounds++
							if noConsistentRounds <= 3 {
								fmt.Printf("   all %d candidate(s) violate the known green pattern — asking provider for different words...\n", len(validated))
								continue
							}
							// Repeated failures: fall back to a local green-consistent word
							// instead of looping forever.
							if fb := klublotto.FallbackOrdknudeGuess(st.History, rejected); fb != "" {
								fmt.Printf("   no consistent provider word after %d rounds; local fallback %s\n", noConsistentRounds, fb)
								currentAnswer = fb
							} else {
								return fmt.Errorf("no green-consistent candidate after %d provider rounds — stopping to avoid wasting a guess", noConsistentRounds)
							}
						} else {
							noConsistentRounds = 0
							printCandidates(pool)
							idx := rand.Intn(len(pool))
							pick := pool[idx]
							pool = append(pool[:idx], pool[idx+1:]...)
							currentAnswer = klublotto.NormalizeDanishLetters(pick.Answer)
							fmt.Printf("   -> trying %s (%s) — %s\n", currentAnswer, pick.Confidence, pick.Rationale)
						}
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
			if klublotto.AlreadyTriedOrdknude(currentAnswer, st.History) {
				fmt.Printf("   %s already tried in this game (persisted state), asking again...\n", currentAnswer)
				continue
			}
			if containsWord(triedThisRun, currentAnswer) {
				fmt.Printf("   %s already submitted this run (re-extract may have missed it), asking again...\n", currentAnswer)
				continue
			}
			// Final hard guard: never submit a word that contradicts a confirmed green
			// letter, regardless of which path produced it. Reject it and ask again.
			if !klublotto.ConsistentWithOrdknudeGreens(currentAnswer, st.History) {
				fmt.Printf("   %s violates the known green pattern — rejecting and asking for another word...\n", currentAnswer)
				if w := klublotto.NormalizeDanishLetters(currentAnswer); !containsWord(rejected, w) {
					rejected = append(rejected, w)
				}
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

			// Win detection: the banner ("Super imponerende! … ord-haj!") lands in
			// the Danske Spil PARENT body text, which the re-extract captured in
			// st.Raw. Check both the submit outcome AND st.Raw — the parent
			// accessibility snapshot can come back empty during the win transition
			// (seen as a 0-byte ordknude-submit-snap.txt), so outcome alone is not
			// reliable; st.Raw is where the banner actually shows up.
			if klublotto.IsOrdknudeWinText(outcome) || klublotto.IsOrdknudeWinText(st.Raw) {
				st.Solved = true
			}
			// The winning guess flips the board to the win overlay INSIDE the
			// iframe, which the re-extract reads as an empty board ("0 guesses").
			// Check the iframe body text directly before we force-reload below.
			if !st.Solved && klublotto.OrdknudeSolvedViaIframe(ctx, br) {
				fmt.Println("   win screen detected inside game iframe — marking as solved")
				st.Solved = true
			}

			// Extra guarantee we are on the parent (embedded) before the next LLM call or submit.
			// The extract above tries to restore, but we force it here too to avoid flicker-related
			// or restore-failure issues leaving us on the raw immerspiele URL.
			if u, _ := br.URL(ctx); isImmerspieleURL(u) || !strings.Contains(u, "danskespil.dk") {
				fmt.Println("       post-extract force to parent page for next action...")
				if err := br.Open(ctx, klublotto.OrdknudeURL); err == nil {
					br.WaitSettled(ctx)
					time.Sleep(800 * time.Millisecond)
				}
			}

			shot := filepath.Join(cfg.DataDir, "ordknude-attempt-"+time.Now().UTC().Format("20060102-150405")+".png")
			_ = br.Screenshot(ctx, shot)

			if st.Solved {
				finalShot := filepath.Join(cfg.DataDir, "ordknude-result-"+time.Now().UTC().Format("20060102-150405")+".png")
				_ = br.Screenshot(ctx, finalShot)
				fmt.Printf("\n🎉 SOLVED! Ordknuden answer: %s (attempt %d/6)\n\n", currentAnswer, currentAttempt)
				notes := klublotto.OrdknudeGuessNotes(klublotto.MergeGuessWords(lastGoodHistory, triedThisRun), lastGoodHistory, currentAnswer)
				if notes == "" {
					notes = "Auto-solved by repeated real LLM-guided guesses on parent page."
				}
				notes = appendModelNote(notes, wordModelLabel(cfg, *providerFlag))
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
		if !st.Solved && klublotto.OrdknudeSolvedViaIframe(ctx, br) {
			fmt.Println("   win screen detected inside game iframe (post-loop) — marking as solved")
			st.Solved = true
			if st.Answer == "" {
				st.Answer = lastSubmittedAnswer // the answer that triggered the win
			}
		}
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
		if seq := klublotto.OrdknudeGuessNotes(klublotto.MergeGuessWords(lastGoodHistory, triedThisRun), lastGoodHistory, recordedAnswer); seq != "" {
			if st.Solved {
				notes = seq
			} else if correctAnswer != "" {
				notes = seq + " · Ikke løst — korrekt svar: " + correctAnswer
			} else {
				notes = seq + " · Ikke løst"
			}
		}
		notes = appendModelNote(notes, wordModelLabel(cfg, *providerFlag))
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
				br.WaitSettled(ctx)
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
		Ok                       bool
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
		Ok                       bool
		Left, Top, Width, Height float64
	}
	if json.Unmarshal([]byte(raw), &rect) == nil && rect.Ok {
		// Keyboard starts at ~0.75 of iframe height; 3 rows each ~0.08 tall.
		// Bottom-row (RETUR) centre is at kbStart + 2*rowH + rowH/2 ≈ 0.91.
		kbY := rect.Top + rect.Height*0.75
		rowH := rect.Height * 0.24 / 3
		x := rect.Left + rect.Width*0.82
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

// extractOrdknudeAnswerFromSnap parses the agent-browser accessibility snapshot
// of the Ordknude result screen and returns the correct answer.
//
// The result screen snapshot contains:
//
//   - paragraph: "Det rigtige svar var:"
//   - paragraph: gummi
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
