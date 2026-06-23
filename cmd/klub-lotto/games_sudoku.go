package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
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

func submitSudoku(ctx context.Context, br *browser.Client, givens, solved klublotto.SudokuGrid) error {
	// The grid is a cross-origin OOPIF (sudoku.…mgame.nu) embedded in the parent.
	// The patched agent-browser can eval/click inside OOPIFs via a frame()
	// switch, so we fill the EMBEDDED game (staying on danskespil.dk so the win
	// registers) rather than the standalone game URL (which redirects away).
	// EnterSudokuGameContext handles the lazy iframe/grid load and switches in.
	inFrame, err := klublotto.EnterSudokuGameContext(ctx, br)
	if err != nil {
		return err
	}
	if inFrame {
		defer klublotto.LeaveFrame(br)
	}

	// Tag each number-pad button with a stable per-digit attribute. Filled cells
	// share the digit text and are also cursor:pointer, so ref/snapshot-based
	// digit matching is unreliable — a unique attribute selector is not.
	if _, err := br.Eval(ctx, `(() => { let n=0; document.querySelectorAll('.number').forEach(el=>{const d=(el.textContent||'').trim(); if(/^[1-9]$/.test(d)){el.setAttribute('data-ab-num',d); n++;}}); return String(n); })()`); err != nil {
		return fmt.Errorf("tag sudoku number buttons: %w", err)
	}

	// The game's decorative .shadow overlays (top/right/bottom/left/center) sit
	// over the grid edges and intercept clicks, so agent-browser refuses to click
	// a covered cell ("covered by <div.shadow.right>"). Disable their pointer
	// events so every cell click lands on the cell.
	if _, err := br.Eval(ctx, `(() => { document.querySelectorAll('.shadow').forEach(s=>{s.style.pointerEvents='none';}); return 'ok'; })()`); err != nil {
		return fmt.Errorf("neutralize sudoku shadow overlays: %w", err)
	}

	// Fill: click each empty cell by its unique class, then its number button.
	fmt.Println("       filling sudoku grid (.cell-<r>-<c> + number pad)...")
	filled := 0
	for r := 0; r < 9; r++ {
		for c := 0; c < 9; c++ {
			if givens[r][c] != 0 {
				continue // skip pre-filled givens
			}
			n := solved[r][c]
			cellSel := fmt.Sprintf(".cell-%d-%d", r, c)
			if err := br.Click(ctx, cellSel); err != nil {
				return fmt.Errorf("click cell %s: %w", cellSel, err)
			}
			time.Sleep(50 * time.Millisecond)
			numSel := fmt.Sprintf(`[data-ab-num="%d"]`, n)
			if err := br.Click(ctx, numSel); err != nil {
				return fmt.Errorf("click number %d (%s) at r%d c%d: %w", n, numSel, r+1, c+1, err)
			}
			time.Sleep(70 * time.Millisecond)
			filled++
		}
	}
	fmt.Printf("       filled %d cells\n", filled)

	// Back to the parent and check for the success banner.
	if inFrame {
		_ = br.Frame(ctx, "")
	}
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

func gridOneLine(g klublotto.SudokuGrid) string {
	return strings.ReplaceAll(klublotto.FormatSudokuGrid(g), "\n", " / ")
}
