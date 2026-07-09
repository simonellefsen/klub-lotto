package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/config"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
	"github.com/simonellefsen/klub-lotto/internal/llm"
	"github.com/simonellefsen/klub-lotto/internal/store"
)

// krydsordDictFixMaxLen is the longest learned-dictionary answer we trust enough
// to PRE-FIX on the grid as a hard crossing constraint. At or below it (1-3
// letters) a single learned answer is reliably unambiguous; above it, a single
// learned answer is treated as a candidate only — the same clue can map to a
// different long word, and fixing a wrong one poisons every crossing.
const krydsordDictFixMaxLen = 3

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
	// solveClues carries the vision-extracted clues (slot id + clue text) out of the
	// solve branch so we can auto-learn clue→answer mappings after a confirmed submit.
	var solveClues []klublotto.KrydsordClue
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
		// Same provider selection as the graph step, so OPENROUTER_VISION_MODEL
		// overrides the default (e.g. OPENROUTER_VISION_MODEL=~google/gemini-pro-latest).
		ac, _ := krydsordVisionProvider(cfg)
		// Reuse a previous run's vision clues for this exact puzzle (keyed by
		// crossword id) so a restart — e.g. after the assembler times out — skips the
		// slow OCR and jumps straight to [3.6/4]. Delete the cache file to force a
		// fresh read.
		cachedClues, cluesCached := klublotto.LoadKrydsordClueCache(cfg.DataDir, data.CrosswordID)
		if cluesCached {
			if !promptYesNo(fmt.Sprintf("       Found cached vision clues for crossword %s (%d clues). Use the cache and skip vision OCR?", data.CrosswordID, len(cachedClues)), true) {
				fmt.Println("       re-reading clues via vision (the cache will be overwritten)...")
				cluesCached, cachedClues = false, nil
			}
		}
		if ac == nil && !cluesCached {
			fmt.Println("       WARNING: no vision API key (GEMINI_API_KEY or ANTHROPIC_API_KEY); vision-based clue OCR unavailable.")
			if !*dryRun {
				return fmt.Errorf("GEMINI_API_KEY or ANTHROPIC_API_KEY is required for real `krydsord` solve+submit (use --dry-run to extract only, or --grid <file> to validate/supply a grid)")
			}
			fmt.Println("       (dry-run: skipping auto-solve; full grid solve would require ANTHROPIC + word provider keys)")
			// solvedGrid stays [], later code will print dry note. --grid branch above already handled debug case.
		} else {
			var clues []klublotto.KrydsordClue
			if cluesCached {
				clues = cachedClues
				fmt.Printf("       reusing %d cached clues for crossword %s — skipping vision OCR (delete %s to force a fresh read)\n",
					len(clues), data.CrosswordID, klublotto.KrydsordClueCachePath(cfg.DataDir, data.CrosswordID))
			} else {
				if n, ok := ac.(interface{ Name() string }); ok {
					fmt.Printf("       vision model: %s\n", n.Name())
				}
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
				// Cache the clues so a restart on this same puzzle reuses them —
				// but ONLY when coverage is complete. Caching a partial read would
				// pin the missing clues (e.g. a dropped BEGÆRET) for the rest of the
				// day; a re-run then re-reads and can recover them.
				covered, total := klublotto.KrydsordClueCoverage(data, clues)
				if covered < total {
					fmt.Printf("       clue coverage %d/%d — NOT caching (a re-run will re-read the missed cells)\n", covered, total)
				} else if err := klublotto.SaveKrydsordClueCache(cfg.DataDir, data.CrosswordID, clues); err == nil && len(clues) > 0 {
					fmt.Printf("       cached %d clues (coverage %d/%d) to %s for reuse on restart\n", len(clues), covered, total, klublotto.KrydsordClueCachePath(cfg.DataDir, data.CrosswordID))
				}
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

			// 1) Seed answers from our learned dictionary FIRST. A clue with a SINGLE
			// valid answer of the right length AND that length ≤ krydsordDictFixMaxLen
			// is treated as FIXED — pre-placed on the grid as a crossing constraint
			// (e.g. URAN=U → A1 reads __U______, ILT=O → D1 _______O__). We only fix
			// SHORT words because they're reliably unambiguous: 1-3 letter answers are
			// almost always abbreviations / function words / fixed forms (ILT→O, ASA,
			// SUT, SMS). A LONGER single match is NOT fixed — the same Danish clue can
			// have a different long answer than the one we happened to learn (REDSKAB →
			// LOMMEKNIV vs SAV vs …), and fixing a wrong long word poisons every
			// crossing. Longer matches (and clues with several valid answers, e.g.
			// STÆVNE → OL/EM/NM/VM) are seeded as candidates only; the crossings + LLM
			// decide which fits. These curated candidates also skip the LLM per slot.
			dictPath := filepath.Join(wikiRoot(), "concepts", "krydsord-clues.json")
			dict := klublotto.LoadKrydsordDict(dictPath)
			knownAnswers := map[string]string{}
			seeded := 0
			for _, cl := range clues {
				if cl.Clue == "" {
					continue
				}
				var matching []string
				for _, ans := range dict.Lookup(klublotto.WithImageMarker(cl.Clue, cl.IsImage)) {
					ans = klublotto.NormalizeDanishLetters(ans)
					if len([]rune(ans)) == cl.Length {
						matching = append(matching, ans)
					}
				}
				if len(matching) == 0 {
					continue
				}
				for _, ans := range matching {
					slotCands[cl.SlotID] = append(slotCands[cl.SlotID], klublotto.WordCandidate{Answer: ans, Confidence: "high", Rationale: "learned dictionary"})
					seeded++
				}
				if len(matching) == 1 && len([]rune(matching[0])) <= krydsordDictFixMaxLen {
					knownAnswers[cl.SlotID] = matching[0] // short + unambiguous → fix as a crossing constraint
				}
			}
			if seeded > 0 {
				fmt.Printf("       seeded %d candidate(s) from the learned dictionary (%s); %d slot(s) fixed as crossing constraints\n", seeded, dictPath, len(knownAnswers))
			}

			// 2) Ask the word provider — in ONE batch call — for candidates for every
			// clue we DON'T already know from the dictionary. Image clues are phrased
			// as "an image of a X" so the model treats them as picture clues (see
			// BuildKrydsordBatchPrompt). On failure we do NOT fan out to ~45 per-clue
			// calls (that fan-out is what made this step take many minutes) — the
			// assembler still receives every clue text + the dictionary patterns and
			// can solve without per-slot candidates.
			batchClues := []klublotto.KrydsordBatchClue{}
			want := map[string]int{}
			for _, cl := range clues {
				if cl.Clue == "" || len(slotCands[cl.SlotID]) > 0 {
					continue // no clue text, or already covered by dictionary candidates
				}
				batchClues = append(batchClues, klublotto.KrydsordBatchClue{SlotID: cl.SlotID, Clue: cl.Clue, Length: cl.Length, IsImage: cl.IsImage})
				want[cl.SlotID] = cl.Length
			}
			if len(batchClues) > 0 {
				bp := klublotto.BuildKrydsordBatchPrompt(batchClues)
				if raw, berr := wordCandidatesRawJSON(ctx, cfg, *providerFlag, bp); berr != nil {
					fmt.Printf("       batch candidate call failed (%v) — assembling from clue texts + dictionary patterns only\n", berr)
				} else if m, perr := klublotto.ParseKrydsordBatchCandidates(raw, want); perr != nil {
					fmt.Printf("       batch candidate parse failed (%v) — assembling from clue texts + dictionary patterns only\n", perr)
				} else {
					for id, cs := range m {
						slotCands[id] = append(slotCands[id], cs...) // dict answer (if any) stays first
					}
					fmt.Printf("       batch: candidates for %d/%d remaining clues in a single call\n", len(m), len(batchClues))
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
			grid, err := assembleKrydsordSolutionGrid(ctx, cfg, *providerFlag, data, clues, slotCands, allClueTexts, knownAnswers)
			if err != nil {
				return fmt.Errorf("solve krydsord: %w", err)
			}
			solveClues = clues // remember for post-submit auto-learn
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
		if errors.Is(err, errKrydsordNotSolved) {
			// Wrong solution rejected by the vendor — record the loss and exit cleanly
			// (like an Ordknuden loss) instead of a hard error.
			return recordKrydsordFailure(ctx, cfg, solvedGrid, wordModelLabel(cfg, *providerFlag), "Tofaset clues OCR + LLM-kandidater + konsistent gitter.")
		}
		return err
	}
	shot := filepath.Join(cfg.DataDir, "krydsord-result-"+time.Now().UTC().Format("20060102-150405")+".png")
	_ = br.Screenshot(ctx, shot)

	// Auto-learn: the submit confirmed the solution is correct (submitKrydsord only
	// returns nil on a success banner), so record every clue→answer mapping into the
	// learned dictionary for future puzzles. Each slot's verified answer is read
	// straight from the solved grid; dict.Add de-dupes and accumulates alternatives
	// (so STÆVNE gains EM alongside OL, BLOMSTRE gains today's answer, etc.).
	//
	// The --grid path doesn't vision-extract clues (solveClues is empty), so fall
	// back to this puzzle's cached clues so a supplied-grid submit still learns.
	if len(solveClues) == 0 {
		if cached, ok := klublotto.LoadKrydsordClueCache(cfg.DataDir, data.CrosswordID); ok {
			solveClues = cached
			fmt.Printf("       [learn] using %d cached clues for crossword %s\n", len(cached), data.CrosswordID)
		}
	}
	if len(solveClues) > 0 {
		dictPath := filepath.Join(wikiRoot(), "concepts", "krydsord-clues.json")
		dict := klublotto.LoadKrydsordDict(dictPath)
		slots := klublotto.BuildKrydsordSlots(data)
		slotByID := map[string]klublotto.KrydsordSlot{}
		for _, s := range slots {
			slotByID[s.ID] = s
		}
		added := 0
		for _, cl := range solveClues {
			if strings.TrimSpace(cl.Clue) == "" {
				continue
			}
			s, ok := slotByID[cl.SlotID]
			if !ok {
				continue
			}
			var ans []rune
			for _, cell := range s.Cells {
				if cell.Row-1 < 0 || cell.Row-1 >= len(solvedGrid) {
					continue
				}
				row := []rune(solvedGrid[cell.Row-1])
				if cell.Col-1 >= 0 && cell.Col-1 < len(row) {
					ans = append(ans, row[cell.Col-1])
				}
			}
			if len(ans) != s.Length {
				continue
			}
			if dict.Add(klublotto.WithImageMarker(cl.Clue, cl.IsImage), string(ans)) {
				added++
			}
		}
		if added > 0 {
			if serr := dict.Save(dictPath); serr != nil {
				fmt.Printf("       [learn] failed to save dictionary: %v\n", serr)
			} else {
				fmt.Printf("       [learn] recorded %d new clue→answer entries to %s\n", added, dictPath)
			}
		}
	}

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

	krydsordModel := wordModelLabel(cfg, *providerFlag)
	if vp, verr := krydsordVisionProvider(cfg); verr == nil {
		if n, ok := vp.(interface{ Name() string }); ok {
			if vn := strings.TrimSpace(n.Name()); vn != "" {
				krydsordModel = krydsordModel + " (vision: " + vn + ")"
			}
		}
	}
	notes := appendModelNote("Solved via clues OCR + LLM candidates + consistent grid. Saved via vendor API + Tjek løsning.", krydsordModel)
	return upsertDailyGame(ctx, cfg, "Krydsord", "Danish clues-in-squares crossword", krydsordAnswerBoard(solvedGrid), true, true, notes)
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

// krydsordVisionProvider picks the vision model for the krydsord graph AND
// clue-extraction steps. Override the default with OPENROUTER_VISION_MODEL
// (e.g. OPENROUTER_VISION_MODEL=~google/gemini-pro-latest or openai/gpt-5.4).
// The caller logs the chosen model.
func krydsordVisionProvider(cfg *config.Config) (llm.VisionProvider, error) {
	// An explicit vision model (VISION_MODEL / OPENROUTER_VISION_MODEL) is routed by
	// its slug syntax: "gemini[:model]" → native Gemini, "anthropic[:model]" → native
	// Anthropic, and an "author/model" slug → OpenRouter. This is what lets
	// VISION_MODEL=gemini:gemini-pro-latest hit Google directly instead of being
	// shoved into OpenRouter as an invalid model id.
	if m := strings.TrimSpace(cfg.OpenRouterVisionModel); m != "" {
		return llm.ResolveVision(m, providerKeys(cfg))
	}
	switch {
	case cfg.GeminiKey != "":
		return llm.NewGemini(cfg.GeminiKey, "gemini-2.5-pro"), nil
	case cfg.AnthropicKey != "":
		// Haiku for vision/OCR (fast, cheap, sufficient for reading clue text).
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
	if n, ok := ac.(interface{ Name() string }); ok {
		fmt.Printf("   [graph] vision model: %s\n", n.Name())
	}
	// Give the embedded board a moment to finish rendering, then screenshot —
	// CROPPED to the game iframe: the board is all the model needs, and the
	// parent-page chrome would only add image tokens (falls back to the full
	// page when the rect can't be found). No iframe navigation.
	br.WaitSettled(ctx)
	time.Sleep(1500 * time.Millisecond)
	stamp := time.Now().UTC().Format("20060102-150405")
	inputPath := filepath.Join(cfg.DataDir, "krydsord-graph-input-"+stamp+".png")
	imgBytes, err := klublotto.CropToGameIframe(ctx, br, "iframe[src*='krydsord']")
	if err != nil {
		return "", nil, fmt.Errorf("screenshot parent board: %w", err)
	}
	if len(imgBytes) == 0 {
		return "", nil, fmt.Errorf("parent screenshot was empty")
	}
	_ = os.WriteFile(inputPath, imgBytes, 0o644)
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

	var g klublotto.KrydsordGraph
	if err := json.Unmarshal([]byte(graphJSON), &g); err != nil {
		return fmt.Errorf("krydsord --solve: parse graph JSON: %w", err)
	}
	if len(g.Across)+len(g.Down) == 0 {
		return fmt.Errorf("krydsord --solve: graph has no clues")
	}
	csp := klublotto.BuildKrydsordCSP(g)
	cspJSON, _ := json.MarshalIndent(csp, "", "  ")
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-csp.json"), append(cspJSON, '\n'), 0o644)
	board := klublotto.RenderKrydsordBoard(csp)
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-board.txt"), []byte(board), 0o644)
	fmt.Printf("[solve] %d across, %d down, %d crossings (CSP)\n", len(g.Across), len(g.Down), csp.CrossingCount())
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
	answers := klublotto.ParseKrydsordAnswers(clean)
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
	issues := klublotto.ValidateKrydsordSolution(csp, answersByID)
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
		grid := klublotto.BuildKrydsordGridFromAnswers(csp, answersByID, data.CellCountX, data.CellCountY)
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
			if errors.Is(serr, errKrydsordNotSolved) {
				return recordKrydsordFailure(ctx, cfg, grid, wordModelLabel(cfg, provider), "Tofaset graph→CSP→LLM ("+solveSource+").")
			}
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
		return upsertDailyGame(ctx, cfg, "Krydsord", "Danish clues-in-squares crossword", krydsordAnswerBoard(grid), true, true,
			fmt.Sprintf("Solved via two-stage graph→CSP→LLM (%s).", solveSource))
	}
	return nil
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

func countKrydsordSlots(slots []klublotto.KrydsordSlot, direction string) int {
	n := 0
	for _, slot := range slots {
		if slot.Direction == direction {
			n++
		}
	}
	return n
}

// enterKrydsordGameFrame switches the browser into the krydsord game iframe
// (a cross-origin OOPIF) and confirms the board has actually rendered inside it.
// Two things race: the board iframe attaches lazily after the parent loads, and
// the OOPIF CDP session attaches slightly after br.Frame() returns — so we
// re-enter and re-check until the `.cell` divs appear (a frame switch that
// "succeeds" but lands on the main session evaluates against the parent doc,
// which has no .cell). On success the caller must switch back with br.Frame("").
func enterKrydsordGameFrame(ctx context.Context, br *browser.Client) error {
	selectors := []string{
		klublotto.GameIframe,
		"iframe[src*='krydsord']",
		"iframe[src*='kryds']",
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range selectors {
			if br.Frame(ctx, sel) == nil {
				// Verify we're inside the game frame and the grid has rendered.
				n, _ := br.Eval(ctx, `String(document.querySelectorAll('.cell').length)`)
				if cnt, _ := strconv.Atoi(strings.TrimSpace(n)); cnt > 0 {
					return nil
				}
			}
		}
		time.Sleep(800 * time.Millisecond)
	}
	return fmt.Errorf("krydsord board cells never rendered inside the game iframe")
}

// errKrydsordNotSolved is returned by submitKrydsord when the grid was filled and
// submitted but Danske Spil rejected it ("Opgaven er ikke løst korrekt") — i.e. a
// genuine wrong-solution failure, as opposed to a technical/timeout error. Callers
// use errors.Is to record a not-solved ledger row and exit cleanly instead of
// hard-failing.
var errKrydsordNotSolved = errors.New("krydsord submitted but not solved correctly")

// recordKrydsordFailure logs a not-solved Krydsord attempt to the daily ledger
// (registered=no) and prints a clear failure line. The Danish failure screen
// reveals no correct solution, so we record the grid we submitted plus a loss tag.
func recordKrydsordFailure(ctx context.Context, cfg *config.Config, grid []string, model, source string) error {
	fmt.Println("\n❌ Krydsord not solved — Danske Spil rejected the submitted grid (ikke løst korrekt).")
	note := "Ikke løst — indsendt gitter blev afvist (ikke løst korrekt)."
	if strings.TrimSpace(source) != "" {
		note += " " + source
	}
	note = appendModelNote(note, model)
	return upsertDailyGame(ctx, cfg, "Krydsord", "Danish clues-in-squares crossword", krydsordAnswerBoard(grid), true, false, note)
}

func submitKrydsord(ctx context.Context, br *browser.Client, data klublotto.KrydsordData, solvedGrid []string) error {
	if len(solvedGrid) == 0 {
		return fmt.Errorf("no solved grid to submit")
	}
	// Letters of the solved grid in row-major order. The board's .cell divs, sorted
	// by their (top,left) pixel position, come out in the same row-major order, so
	// the i-th cell gets the i-th letter.
	var letters []string
	for _, rowstr := range solvedGrid {
		for _, ch := range []rune(rowstr) {
			if isKrydsordAnswerLetter(ch) {
				letters = append(letters, string(ch))
			}
		}
	}

	// Open the Danske Spil *parent* so the game iframe is embedded; the parent
	// receives gameCompleted etc and awards the daily lod. We deliberately do NOT
	// navigate to the standalone iframe URL — that URL carries a single-use launcher
	// token and, loaded directly, hangs forever on the red loading spinner.
	fmt.Println("       opening parent page (iframe embedded for registration)...")
	if err := br.Open(ctx, klublotto.KrydsordURL); err != nil {
		return fmt.Errorf("open parent for krydsord submit: %w", err)
	}
	br.WaitSettled(ctx)

	// The board is a cross-origin OOPIF whose answer cells are bare clickable divs
	// with no accessibility role — they never appear in the snapshot (interactive
	// OR cursor), so there are no refs to click. But they DO take real input: a
	// genuine mouse click at the cell's screen position selects it, after which the
	// game's document-level keydown handler accepts the typed letter. So we fill by
	// coordinate. Read the iframe's viewport offset from the PARENT first (a
	// cross-origin iframe's rect is only visible from the parent).
	//
	// On a slow danskespil day the iframe isn't attached/laid out within a fixed
	// delay — reading its rect then returned {x:-1} and aborted the submit. Poll for
	// a valid, laid-out rect (mirrors the extraction-side iframe readiness).
	var ifr struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	rectDeadline := time.Now().Add(30 * time.Second)
	for {
		ifrRaw, _ := br.Eval(ctx, `JSON.stringify((()=>{const f=document.querySelector("iframe.kl-game__iframe");if(!f)return{x:-1,y:-1,w:0,h:0};const r=f.getBoundingClientRect();return{x:r.x,y:r.y,w:r.width,h:r.height};})())`)
		var probe struct {
			X, Y, W, H float64
		}
		if json.Unmarshal([]byte(ifrRaw), &probe) == nil && probe.X >= 0 && probe.W > 0 && probe.H > 0 {
			ifr.X, ifr.Y = probe.X, probe.Y
			break
		}
		if time.Now().After(rectDeadline) || ctx.Err() != nil {
			return fmt.Errorf("krydsord game iframe not found on parent page within budget (last raw=%s)", ifrRaw)
		}
		time.Sleep(800 * time.Millisecond)
	}

	// Switch into the OOPIF and confirm the grid rendered.
	if err := enterKrydsordGameFrame(ctx, br); err != nil {
		return fmt.Errorf("enter krydsord game iframe: %w", err)
	}
	defer klublotto.LeaveFrame(br)

	// Collect every answer cell's center (frame-viewport coords), sorted row-major.
	cellsRaw, _ := br.Eval(ctx, `JSON.stringify((()=>{const cs=Array.from(document.querySelectorAll(".cell"));const p=cs.map(c=>{const r=c.getBoundingClientRect();return {x:r.x+r.width/2,y:r.y+r.height/2,t:parseFloat(c.style.top)||r.y,l:parseFloat(c.style.left)||r.x};});p.sort((a,b)=>Math.abs(a.t-b.t)>5?a.t-b.t:a.l-b.l);return p.map(o=>({x:Math.round(o.x),y:Math.round(o.y)}));})())`)
	var cells []struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	if err := json.Unmarshal([]byte(cellsRaw), &cells); err != nil {
		return fmt.Errorf("read krydsord cell positions: %w (raw=%s)", err, cellsRaw)
	}
	if len(cells) != len(letters) {
		fmt.Printf("       [warn] cell count %d != answer letters %d — filling min(...)\n", len(cells), len(letters))
	}
	n := len(cells)
	if len(letters) < n {
		n = len(letters)
	}
	fmt.Printf("       filling %d answer cells by coordinate (iframe at %.0f,%.0f)...\n", n, ifr.X, ifr.Y)
	for i := 0; i < n; i++ {
		absX := int(ifr.X) + cells[i].X
		absY := int(ifr.Y) + cells[i].Y
		if err := br.MouseClick(ctx, absX, absY); err != nil {
			continue
		}
		time.Sleep(60 * time.Millisecond) // let the click select the cell
		// The game captures document-level keydowns, so Press works for every
		// letter — including Æ/Ø/Å.
		_ = br.Press(ctx, letters[i])
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond) // let the grid commit the typed letters

	// Find the "TJEK LØSNING" button — the buttons DO carry roles, so they appear
	// in the in-frame interactive snapshot as refs.
	snap, _ := br.SnapshotInteractive(ctx)
	tjekRef := klublotto.FindRefByName(snap, []string{"TJEK LØSNING", "TJEK LOSNING"})
	gemRef := klublotto.FindRefByName(snap, []string{"GEM"})

	// Click "TJEK LØSNING" via ref (preferred) or fall back to name search.
	fmt.Println("       clicking Tjek løsning...")
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

	// Check for the success banner. We're still inside the game frame, so the eval
	// reads the game iframe's body — where "hvor er du vild"/"ordmester" render.
	if ok, detail := waitForKrydsordSuccess(ctx, br); ok {
		fmt.Println("       success detected (in game frame):", detail)
		return nil
	} else if strings.Contains(detail, "ikke løst korrekt") {
		// The game's own rejection overlay — definitively a wrong solution.
		return fmt.Errorf("%w: %s", errKrydsordNotSolved, detail)
	}
	// Fallback: some confirmations ("løste dagens krydsord") surface on the parent
	// page, so switch back to main and check there before declaring failure.
	klublotto.LeaveFrame(br)
	if ok, detail := waitForKrydsordSuccess(ctx, br); ok {
		fmt.Println("       success detected (on parent page):", detail)
		return nil
	} else if strings.Contains(detail, "ikke løst korrekt") {
		return fmt.Errorf("%w: %s", errKrydsordNotSolved, detail)
	} else {
		return fmt.Errorf("Krydsord not confirmed solved: %s", detail)
	}
}

func waitForKrydsordSuccess(ctx context.Context, br *browser.Client) (bool, string) {
	// Krydsord-SPECIFIC success banners only. NOTE: do NOT match "vundet" — the
	// page's permanent footer/nav contains "vundet eller tabt", which previously
	// produced a FALSE success on an unsolved board. "tillykke"/"dagens lod" are
	// likewise too generic.
	// Observed win banners: "Hvor er du vild…", "…Ordmester…", and
	// "Rigtig godt arbejde! Du klarede krydsordet med bravur! Det var virkelig
	// imponerende, og du får et lod." (2026-06-19).
	success := []string{"hvor er du vild", "ordmester", "løste dagens krydsord",
		"rigtig godt arbejde", "klarede krydsordet", "med bravur"}
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

func assembleKrydsordSolutionGrid(ctx context.Context, cfg *config.Config, provider string, data klublotto.KrydsordData, clues []klublotto.KrydsordClue, perSlot map[string][]klublotto.WordCandidate, allClueTexts []string, knownAnswers map[string]string) ([]string, error) {
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
	slotByID := map[string]klublotto.KrydsordSlot{}
	for _, s := range slots {
		slotByID[s.ID] = s
	}

	// Pre-place the deterministic known answers onto the grid as FIXED cells, so
	// every crossing slot inherits the constraint as a letter pattern. cellKey is
	// "r<row>c<col>"; the value is the fixed rune at that cell. REBUILDABLE:
	// when a conflicting dict answer is dropped mid-loop, the fixed cells (and
	// thus every mønster= in the next prompt) must be recomputed — a stale
	// pattern kept pushing the model to preserve the dropped letter (seen live:
	// MATEMATIK "corrected" to the non-word MATEMDTIK to match an untrusted D).
	cellKey := func(r, c int) string { return fmt.Sprintf("r%dc%d", r, c) }
	fixed := map[string]rune{}
	rebuildFixed := func() {
		fixed = map[string]rune{}
		for id, ans := range knownAnswers {
			s, ok := slotByID[id]
			if !ok {
				continue
			}
			rs := []rune(ans)
			if len(rs) != len(s.Cells) {
				continue
			}
			for i, cell := range s.Cells {
				k := cellKey(cell.Row, cell.Col)
				if ex, seen := fixed[k]; seen && ex != rs[i] {
					continue // two known answers disagree (mapping/curation error) — keep first
				}
				fixed[k] = rs[i]
			}
		}
	}
	rebuildFixed()
	// slotPattern returns the crossing pattern for a slot ("." = unknown), plus
	// the count of fixed letters in it. matchesPattern tests a candidate rune-wise.
	slotPattern := func(s klublotto.KrydsordSlot) (string, int) {
		var pb strings.Builder
		known := 0
		for _, cell := range s.Cells {
			if r, ok := fixed[cellKey(cell.Row, cell.Col)]; ok {
				pb.WriteRune(r)
				known++
			} else {
				pb.WriteRune('.')
			}
		}
		return pb.String(), known
	}
	matchesPattern := func(word, pat string) bool {
		wr, pr := []rune(word), []rune(pat)
		if len(wr) != len(pr) {
			return false
		}
		for i := range pr {
			if pr[i] != '.' && pr[i] != wr[i] {
				return false
			}
		}
		return true
	}

	buildPrompt := func() string {
		var b strings.Builder
		fmt.Fprintf(&b, "Løs dette danske krydsord (clues-in-squares). VÆLG for HVER slot ét dansk svar med PRÆCIS den angivne længde, så bogstaverne passer ved ALLE krydsninger (celler delt mellem to slots skal have samme bogstav).\n")
		fmt.Fprintf(&b, "Billedledetråde står som engelske beskrivelser (fx \"grill\", \"t-shirt\", \"turnip\", \"desk lamp\") — svar med det danske ord for tingen (GRILL, TSHIRT, ROE, LAMPE).\n")
		if len(allClueTexts) > 0 {
			fmt.Fprintf(&b, "Alle synlige ledetråde (OCR-tildelingen pr. slot kan være unøjagtig): %q\n", allClueTexts)
		}
		fmt.Fprintf(&b, "\nSlots (id, retning, længde, ledetråd, celler r<row>c<col>, kandidater):\n")
		for _, s := range slots {
			cl := cluesByID[s.ID]
			// Skip speculative 1-letter slots that got no clue from vision (or whose
			// untrusted clue was dropped mid-loop) — their single cell is already
			// fixed by the crossing word, and asking the LLM to "answer" them only
			// invites a wrong letter that conflicts.
			if s.Length == 1 && strings.TrimSpace(cl.Clue) == "" {
				continue
			}
			pat, known := slotPattern(s)
			var candList []string
			for _, c := range perSlot[s.ID] {
				a := klublotto.NormalizeDanishLetters(c.Answer)
				if len([]rune(a)) == s.Length {
					candList = append(candList, a)
				}
			}
			// If crossings fix some letters, prefer candidates that match the pattern.
			// Only narrow when at least one survives — never starve a slot to zero.
			if known > 0 {
				var keep []string
				for _, a := range candList {
					if matchesPattern(a, pat) {
						keep = append(keep, a)
					}
				}
				if len(keep) > 0 {
					candList = keep
				}
			}
			cellIDs := make([]string, 0, len(s.Cells))
			for _, cell := range s.Cells {
				cellIDs = append(cellIDs, cellKey(cell.Row, cell.Col))
			}
			clueText := cl.Clue
			kind := ""
			if cl.IsImage {
				// Spell out that this clue is a picture so the model translates the
				// depicted object instead of treating the description as a literal word.
				if clueText != "" {
					clueText = "an image of a " + clueText
				}
				kind = " BILLEDE"
			}
			patHint := ""
			if known > 0 {
				// Show the fixed-letter pattern so the model picks a crossing-consistent word.
				patHint = fmt.Sprintf(" mønster=%s", pat)
			}
			fmt.Fprintf(&b, "- %s %s len=%d clue=%q%s%s cells=%s cands=%v\n", s.ID, s.Direction, s.Length, clueText, kind, patHint, strings.Join(cellIDs, ","), candList)
		}
		if len(fixed) > 0 {
			fmt.Fprintf(&b, "\nVIGTIGT: 'mønster' viser bogstaver der ALLEREDE er fastlagt af krydsende ord (. = ukendt). Dit svar for sådan et slot SKAL matche mønsteret nøjagtigt på de kendte positioner.\n")
		}
		b.WriteString("\nReturnér KUN JSON: {\"answers\":{\"A1\":\"ORD\",\"D1\":\"ORD\", ...}} med ét svar pr. slot-id ovenfor. Kun bogstaver (ÆØÅ tilladt), ingen mellemrum/tegn. INGEN markdown.\n")
		return b.String()
	}
	basePrompt := buildPrompt()

	// Surface the exact assembler prompt — including the per-slot `mønster=` patterns
	// derived from the fixed 1-letter clues (e.g. ILT=O making D1 BILLEDE `_______O__`)
	// — so it can be inspected. Saved to a file and echoed to the console.
	promptPath := filepath.Join(cfg.DataDir, "krydsord-assemble-prompt.txt")
	_ = os.WriteFile(promptPath, []byte(basePrompt), 0o644)
	fmt.Printf("       [assemble] prompt (%d chars) saved: %s\n", len(basePrompt), promptPath)
	if os.Getenv("KLUBLOTTO_DEBUG") != "" {
		fmt.Println("       ---- assembler prompt ----")
		for _, ln := range strings.Split(strings.TrimRight(basePrompt, "\n"), "\n") {
			fmt.Println("       | " + ln)
		}
		fmt.Println("       ---- end prompt ----")
	}

	// Retry, feeding back which slots are missing or which crossings conflict.
	// A reasoning model at medium effort can think past 3 minutes on a 40+ slot
	// grid (seen live: attempt 1 "context deadline exceeded"). Give each attempt
	// 300s, and after a TIMEOUT drop to reasoning effort=low for the remaining
	// attempts so they answer within budget — the dictionary already pre-fixes
	// several slots and seeds candidates, so low effort is usually enough.
	const maxAttempts = 3
	const attemptTimeout = 300 * time.Second
	var lastErr error
	// Slots whose untrusted 1-letter dict answer was dropped mid-loop: the model
	// must never re-answer them (the crossing word owns the cell), so they are
	// scrubbed from every parsed/repaired answer set.
	droppedSlots := map[string]bool{}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Rebuild from the CURRENT knownAnswers/clues — a stale prompt carries
		// mønster= patterns for dict answers that were dropped as untrusted.
		rebuildFixed()
		prompt := buildPrompt()
		if lastErr != nil {
			prompt += fmt.Sprintf("\nForrige forsøg var forkert: %v\nRet svarene: hvert slot skal have et svar med korrekt længde, og delte celler skal have samme bogstav.\n", lastErr)
		}
		modelCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		raw, genErr := p.GenerateJSON(modelCtx, prompt, 0.05)
		cancel()
		if genErr != nil {
			lastErr = genErr
			fmt.Printf("       [assemble] attempt %d/%d: model error: %v\n", attempt, maxAttempts, genErr)
			if or, ok := p.(*llm.OpenRouter); ok && or.ReasoningEffort != "low" &&
				(errors.Is(genErr, context.DeadlineExceeded) || strings.Contains(genErr.Error(), "context deadline")) {
				cp := *or
				cp.ReasoningEffort = "low"
				p = &cp
				fmt.Println("       [assemble] timeout — dropping to reasoning effort=low for the retry")
			}
			continue
		}
		_ = os.WriteFile(filepath.Join(cfg.DataDir, "krydsord-assemble-raw.txt"), []byte(raw), 0o644)
		answers, parseErr := klublotto.ParseKrydsordAnswerMap(raw)
		if parseErr != nil || len(answers) == 0 {
			lastErr = fmt.Errorf("parse per-slot answers: %v", parseErr)
			fmt.Printf("       [assemble] attempt %d/%d: %v\n", attempt, maxAttempts, lastErr)
			continue
		}
		// Force the deterministic known answers: the dictionary is trusted over the
		// model, so a 1-letter abbreviation (or other curated answer) is never lost
		// to a model hallucination. Any resulting crossing conflict is a real signal
		// the model's crossing word is wrong, which the retry then corrects.
		for id, ans := range knownAnswers {
			answers[id] = ans
		}
		// Scrub slots whose untrusted answer was dropped — their cell belongs to
		// the crossing word now; the model re-answering them (e.g. from its own
		// clue knowledge: VITAMIN → D) would just recreate the conflict.
		for id := range droppedSlots {
			delete(answers, id)
		}
		// Echo the answers used this attempt (model's + forced dictionary answers).
		ids := make([]string, 0, len(answers))
		for id := range answers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Printf("       [assemble] attempt %d answers:\n", attempt)
		for _, id := range ids {
			tag := ""
			if _, ok := knownAnswers[id]; ok {
				tag = " (dict)"
			}
			fmt.Printf("       | %-4s %s%s\n", id, answers[id], tag)
		}
		grid, conflicts := klublotto.BuildKrydsordGridFromSlotAnswers(data, slots, answers)
		check := klublotto.ValidateKrydsordAnswerGrid(data, grid)
		if check.OK && check.FilledN == check.AnswerN && len(conflicts) == 0 {
			if attempt > 1 {
				fmt.Printf("       [assemble] consistent solution on attempt %d/%d\n", attempt, maxAttempts)
			}
			return grid, nil
		}

		// Near-miss repair: the grid is fully filled but a few crossings disagree
		// (e.g. A10=OL where D1/D3 demand E,M → A10 should be EM). Re-prompting all
		// 75 cells is slow and the model tends to regress; instead ask it to fix ONLY
		// the conflicting slots, telling it the exact letters their crossings demand.
		if check.FilledN == check.AnswerN && len(conflicts) > 0 && len(conflicts) <= 8 {
			// A dict answer that participates in a crossing conflict is likely WRONG
			// for this puzzle — e.g. an ambiguous short clue (FUGL→ØRN, or VITAMIN
			// where A and D are both valid answers) whose only learned answer isn't
			// today's. Stop trusting it EVERYWHERE: drop it from knownAnswers, purge
			// it from the slot's candidate list, and for a 1-letter slot cede the
			// cell to the crossing word entirely (blank the clue so no later prompt
			// re-asks it, and remove its answer). Half-dropping it (knownAnswers
			// only) left the stale letter in the retry prompt's mønster= patterns
			// and in cands=[…], which pushed the model to "fix" the crossing WORD
			// instead — turning MATEMATIK into the non-word MATEMDTIK.
			if involved, _ := klublotto.KrydsordConflictSlots(slots, answers); len(involved) > 0 {
				droppedNow := false
				for _, id := range involved {
					ans, ok := knownAnswers[id]
					if !ok {
						continue
					}
					fmt.Printf("       [assemble] dropping dict answer %s=%s — it conflicts with the crossings (untrusted for this puzzle)\n", id, ans)
					delete(knownAnswers, id)
					// Purge the untrusted answer from the slot's candidates.
					var keep []klublotto.WordCandidate
					for _, c := range perSlot[id] {
						if klublotto.NormalizeDanishLetters(c.Answer) != ans {
							keep = append(keep, c)
						}
					}
					perSlot[id] = keep
					if s, okS := slotByID[id]; okS && s.Length == 1 {
						cl := cluesByID[id]
						cl.Clue, cl.IsImage = "", false
						cluesByID[id] = cl
						droppedSlots[id] = true
						delete(answers, id)
						fmt.Printf("       [assemble] %s is a 1-letter slot — ceding its cell to the crossing word\n", id)
					}
					droppedNow = true
				}
				// The drop alone may fully resolve the grid (the crossing word was
				// right all along) — re-validate before burning a repair call.
				if droppedNow {
					g2, c2 := klublotto.BuildKrydsordGridFromSlotAnswers(data, slots, answers)
					chk2 := klublotto.ValidateKrydsordAnswerGrid(data, g2)
					if chk2.OK && chk2.FilledN == chk2.AnswerN && len(c2) == 0 {
						fmt.Printf("       [assemble] resolved by dropping the untrusted dict answer(s) (after attempt %d)\n", attempt)
						return g2, nil
					}
					grid, conflicts, check = g2, c2, chk2 // carry the improved state forward
				}
			}
			if repaired, ok := repairKrydsordConflictsLLM(ctx, p, slots, cluesByID, perSlot, answers); ok {
				for id, ans := range knownAnswers {
					repaired[id] = ans // remaining trusted dict answers stay authoritative
				}
				for id := range droppedSlots {
					delete(repaired, id) // ceded cells stay with the crossing word
				}
				g2, c2 := klublotto.BuildKrydsordGridFromSlotAnswers(data, slots, repaired)
				chk2 := klublotto.ValidateKrydsordAnswerGrid(data, g2)
				if chk2.OK && chk2.FilledN == chk2.AnswerN && len(c2) == 0 {
					fmt.Printf("       [assemble] resolved by targeted conflict repair (after attempt %d)\n", attempt)
					return g2, nil
				}
				if len(c2) < len(conflicts) {
					// Improved but not perfect — carry it into the next whole-grid retry.
					answers, grid, conflicts = repaired, g2, c2
					fmt.Printf("       [assemble] targeted repair reduced conflicts to %d\n", len(c2))
				}
			}
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

// krydsordAnswerBoard renders the solved grid as a compact, monospaced board for
// the daily ledger: a header of column numbers, a separator, then each row
// labelled A, B, C… The all-blocked outer frame (the vendor's 1-indexed border
// row/column) is cropped away by clipping to the bounding box of letter cells, so
// the meaningless leading "."-only row and column drop out; interior blocked
// cells stay as ".". Each line is wrapped in backticks and joined with <br> so it
// renders as aligned monospace inside a Markdown table cell. The column separator
// is "│" (U+2502, box drawing) — NOT an ASCII "|", which would split the cell.
func krydsordAnswerBoard(g []string) string {
	rows := make([][]rune, len(g))
	width := 0
	for i, r := range g {
		rows[i] = []rune(r)
		if len(rows[i]) > width {
			width = len(rows[i])
		}
	}
	cellAt := func(r, c int) rune {
		if r < 0 || r >= len(rows) || c < 0 || c >= len(rows[r]) {
			return '.'
		}
		return rows[r][c]
	}
	isLetter := func(ch rune) bool { return ch != '.' && ch != ' ' && ch != 0 }

	// Clip to the bounding box of letter cells (drops fully-blocked edges).
	minR, maxR, minC, maxC := len(rows), -1, width, -1
	for r := range rows {
		for c := 0; c < width; c++ {
			if isLetter(cellAt(r, c)) {
				if r < minR {
					minR = r
				}
				if r > maxR {
					maxR = r
				}
				if c < minC {
					minC = c
				}
				if c > maxC {
					maxC = c
				}
			}
		}
	}
	if maxR < 0 { // no letters at all — fall back to a plain join
		return strings.Join(g, " / ")
	}
	cropW := maxC - minC + 1

	var lines []string
	var hdr strings.Builder
	hdr.WriteString("* │ ")
	for c := 0; c < cropW; c++ {
		hdr.WriteByte(byte('0' + (c+1)%10))
	}
	lines = append(lines, hdr.String())
	lines = append(lines, strings.Repeat("-", 4+cropW))
	for r := minR; r <= maxR; r++ {
		var sb strings.Builder
		sb.WriteString(krydsordRowLabel(r - minR))
		sb.WriteString(" │ ")
		for c := minC; c <= maxC; c++ {
			ch := cellAt(r, c)
			if !isLetter(ch) {
				ch = '.'
			}
			sb.WriteRune(ch)
		}
		lines = append(lines, sb.String())
	}
	for i, ln := range lines {
		lines[i] = "`" + ln + "`"
	}
	return strings.Join(lines, "<br>")
}

// krydsordRowLabel maps a 0-based row index to A, B, C… (puzzles are far smaller
// than 26 rows; beyond that it falls back to the 1-based number).
func krydsordRowLabel(i int) string {
	if i >= 0 && i < 26 {
		return string(rune('A' + i))
	}
	return fmt.Sprintf("%d", i+1)
}

// repairKrydsordConflictsLLM asks the model to correct ONLY the slots involved in
// crossing conflicts, given the exact letters each one's crossings demand. This is
// a small, fast prompt (a handful of slots) instead of re-emitting the whole grid.
// Returns a full answers map (a copy with the corrected slots overwritten) and
// whether the call produced any change.
func repairKrydsordConflictsLLM(ctx context.Context, p llm.JSONGenerator, slots []klublotto.KrydsordSlot, cluesByID map[string]klublotto.KrydsordClue, perSlot map[string][]klublotto.WordCandidate, answers map[string]string) (map[string]string, bool) {
	involved, patternByID := klublotto.KrydsordConflictSlots(slots, answers)
	if len(involved) == 0 {
		return nil, false
	}
	// The repair is a tiny constrained pick — cap reasoning effort. p is SHARED
	// with the assembler's retries, so clone before mutating.
	if or, ok := p.(*llm.OpenRouter); ok {
		cp := *or
		cp.ReasoningEffort = "low"
		p = &cp
	}
	byID := map[string]klublotto.KrydsordSlot{}
	for _, s := range slots {
		byID[s.ID] = s
	}
	var b strings.Builder
	b.WriteString("Dette danske krydsord er NÆSTEN løst, men nogle krydsende ord er uenige om bogstaver i delte celler. RET KUN nedenstående slots, så hvert svar matcher 'mønster' (bogstaver fastlagt af de KRYDSENDE ord; . = frit). Vælg om nødvendigt et andet dansk ord der både passer ledetråden OG mønsteret. Behold alle andre svar uændret.\n\n")
	for _, id := range involved {
		s := byID[id]
		cl := cluesByID[id]
		clueText := cl.Clue
		if cl.IsImage && clueText != "" {
			clueText = "an image of a " + clueText
		}
		var cands []string
		for _, c := range perSlot[id] {
			cand := klublotto.NormalizeDanishLetters(c.Answer)
			if len([]rune(cand)) == s.Length && klublotto.KrydsordMatchesPattern(cand, patternByID[id]) {
				cands = append(cands, cand)
			}
		}
		fmt.Fprintf(&b, "- %s len=%d clue=%q nu=%s mønster=%s mulige=%v\n", id, s.Length, clueText, answers[id], patternByID[id], cands)
	}
	b.WriteString("\nReturnér KUN JSON: {\"answers\":{\"A1\":\"ORD\", ...}} med rettede svar for KUN ovenstående slots. Kun bogstaver (ÆØÅ), ingen mellemrum/tegn. INGEN markdown.\n")

	fmt.Printf("       [repair] asking model to fix %d conflicting slot(s): %v\n", len(involved), involved)
	modelCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	raw, err := p.GenerateJSON(modelCtx, b.String(), 0.05)
	cancel()
	if err != nil {
		fmt.Printf("       [repair] model error: %v\n", err)
		return nil, false
	}
	fixed, perr := klublotto.ParseKrydsordAnswerMap(raw)
	if perr != nil || len(fixed) == 0 {
		return nil, false
	}
	out := map[string]string{}
	for id, a := range answers {
		out[id] = a
	}
	changed := false
	for id, a := range fixed {
		a = klublotto.NormalizeDanishLetters(a)
		if a != "" && out[id] != a {
			out[id] = a
			changed = true
		}
	}
	return out, changed
}
