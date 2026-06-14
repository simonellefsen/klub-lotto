package klublotto

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

type SudokuGrid [9][9]int

func ParseSudokuGrid(s string) (SudokuGrid, error) {
	var g SudokuGrid
	var vals []int
	for _, r := range s {
		switch {
		case r >= '1' && r <= '9':
			vals = append(vals, int(r-'0'))
		case r == '0' || r == '.':
			vals = append(vals, 0)
		}
	}
	if len(vals) != 81 {
		return g, fmt.Errorf("expected 81 sudoku cells, got %d", len(vals))
	}
	for i, v := range vals {
		g[i/9][i%9] = v
	}
	return g, nil
}

func FormatSudokuGrid(g SudokuGrid) string {
	var b strings.Builder
	for r := 0; r < 9; r++ {
		if r > 0 {
			b.WriteByte('\n')
		}
		for c := 0; c < 9; c++ {
			b.WriteByte(byte('0' + g[r][c]))
		}
	}
	return b.String()
}

func SolveSudoku(g SudokuGrid) (SudokuGrid, bool) {
	r, c, ok := nextSudokuCell(g)
	if !ok {
		return g, true
	}
	for n := 1; n <= 9; n++ {
		if sudokuAllowed(g, r, c, n) {
			g[r][c] = n
			if solved, ok := SolveSudoku(g); ok {
				return solved, true
			}
			g[r][c] = 0
		}
	}
	return g, false
}

func nextSudokuCell(g SudokuGrid) (int, int, bool) {
	bestR, bestC, bestCount := -1, -1, 10
	for r := 0; r < 9; r++ {
		for c := 0; c < 9; c++ {
			if g[r][c] != 0 {
				continue
			}
			count := 0
			for n := 1; n <= 9; n++ {
				if sudokuAllowed(g, r, c, n) {
					count++
				}
			}
			if count < bestCount {
				bestR, bestC, bestCount = r, c, count
			}
		}
	}
	return bestR, bestC, bestR >= 0
}

func sudokuAllowed(g SudokuGrid, row, col, n int) bool {
	for i := 0; i < 9; i++ {
		if g[row][i] == n || g[i][col] == n {
			return false
		}
	}
	br, bc := (row/3)*3, (col/3)*3
	for r := br; r < br+3; r++ {
		for c := bc; c < bc+3; c++ {
			if g[r][c] == n {
				return false
			}
		}
	}
	return true
}

func OpenSudoku(ctx context.Context, br *browser.Client) error {
	if err := br.Open(ctx, SudokuURL); err != nil {
		return err
	}
	_ = br.WaitForLoad(ctx, "networkidle")
	time.Sleep(1200 * time.Millisecond)
	return nil
}

func ExtractSudokuGivens(ctx context.Context, br *browser.Client) (SudokuGrid, string, error) {
	parentURL, _ := br.URL(ctx)
	// The game iframe is a cross-origin OOPIF that agent-browser can neither
	// enter (frame) nor expand (-F snapshot) from the parent, so we must
	// navigate the top-level tab to the iframe src to read the grid. This brief
	// switch to mgame.nu is unavoidable with the current agent-browser.
	src, _ := br.Eval(ctx, `(() => Array.from(document.querySelectorAll('iframe')).map(f => f.src).find(s => /sudoku/i.test(s)) || '')()`)
	src = unwrapAgentBrowserString(src)
	if strings.TrimSpace(src) != "" {
		if err := br.Open(ctx, strings.TrimSpace(src)); err != nil {
			return SudokuGrid{}, parentURL, fmt.Errorf("open sudoku iframe: %w", err)
		}
		_ = br.WaitForLoad(ctx, "networkidle")
		time.Sleep(800 * time.Millisecond)
	}
	raw, err := br.Eval(ctx, sudokuExtractJS)
	if err != nil {
		return SudokuGrid{}, parentURL, fmt.Errorf("extract sudoku cells: %w", err)
	}
	var cells []int
	if err := json.Unmarshal([]byte(raw), &cells); err != nil {
		return SudokuGrid{}, parentURL, fmt.Errorf("parse sudoku cells: %w (raw=%s)", err, raw)
	}
	if len(cells) != 81 {
		return SudokuGrid{}, parentURL, fmt.Errorf("expected 81 sudoku cells, got %d", len(cells))
	}
	var g SudokuGrid
	for i, v := range cells {
		g[i/9][i%9] = v
	}
	return g, parentURL, nil
}

const sudokuExtractJS = `(() => {
  const out = Array(81).fill(0);
  const candidates = Array.from(document.querySelectorAll('[class*="cell"], input, button, [role="gridcell"]'));
  for (const el of candidates) {
    const cls = String(el.className || '');
    const m = cls.match(/cell[-_](\d)[-_](\d)/) || cls.match(/cell-(\d+)-(\d+)/);
    const dataR = el.getAttribute('data-row') || el.getAttribute('aria-rowindex');
    const dataC = el.getAttribute('data-col') || el.getAttribute('aria-colindex');
    let r = m ? Number(m[1]) : (dataR ? Number(dataR) - 1 : NaN);
    let c = m ? Number(m[2]) : (dataC ? Number(dataC) - 1 : NaN);
    if (!Number.isFinite(r) || !Number.isFinite(c)) continue;
    if (r > 8) r--; if (c > 8) c--;
    if (r < 0 || r > 8 || c < 0 || c > 8) continue;
    const text = (el.value || el.innerText || el.textContent || el.getAttribute('aria-label') || '').trim();
    const d = text.match(/[1-9]/);
    if (d) out[r * 9 + c] = Number(d[0]);
  }
  return JSON.stringify(out);
})()`
