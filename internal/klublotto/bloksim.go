package klublotto

import "math/rand"

// Blok for Blok offline simulator. We only get ONE live game per day, so to tune
// the solver (BlokPlan/blokQuality) we play thousands of simulated games here and
// measure the score distribution. The engine implements the real scoring rules:
// +1 point per cell placed, and a combo chain — a line-clear starts the chain,
// each further clear within the window extends it, and the k-th clearing
// placement pays 10×(k−2) (the first two pay 0; multi-line clears pay nothing
// extra) — see BlokChain.Advance, fitted from live traces 2026-07-05. Game over
// when none of the remaining tray pieces fits.
//
// The PIECE SET is a model of the game's bag, approximated from the observed
// screenshots (dominoes, trominoes, all tetrominoes, the 3x3 L-pentomino, and the
// 2x3/3x2/3x3 rectangles) — refine blokSimBaseShapes if the real distribution is
// learned. Relative heuristic comparisons are robust to the exact mix as long as
// the big pieces (which cause the boxed-in game-overs) appear.

// blokSimBaseShapes are the distinct piece shapes before rotation. blokSimPieces
// expands these to all unique rotations at init.
var blokSimBaseShapes = [][][]int{
	// domino
	{{1, 1}},
	// trominoes: line + L-corner
	{{1, 1, 1}},
	{{1, 1}, {1, 0}},
	// tetrominoes: I, O, T, L, J, S, Z
	{{1, 1, 1, 1}},
	{{1, 1}, {1, 1}},
	{{1, 1, 1}, {0, 1, 0}},
	{{1, 1, 1}, {1, 0, 0}},
	{{1, 1, 1}, {0, 0, 1}},
	{{0, 1, 1}, {1, 1, 0}},
	{{1, 1, 0}, {0, 1, 1}},
	// pentomino: 3x3 L (five cells)
	{{1, 1, 1}, {1, 0, 0}, {1, 0, 0}},
	// rectangles: 2x3 / 3x3
	{{1, 1, 1}, {1, 1, 1}},
	{{1, 1, 1}, {1, 1, 1}, {1, 1, 1}},
}

var blokSimPieces = buildBlokSimPieces()

func buildBlokSimPieces() [][][]int {
	var out [][][]int
	seen := map[string]bool{}
	for _, base := range blokSimBaseShapes {
		s := base
		for r := 0; r < 4; r++ {
			key := shapeKey(s)
			if !seen[key] {
				seen[key] = true
				out = append(out, cloneShape(s))
			}
			s = rotateShape(s)
		}
	}
	return out
}

func rotateShape(s [][]int) [][]int {
	h, w := len(s), len(s[0])
	r := make([][]int, w)
	for i := 0; i < w; i++ {
		r[i] = make([]int, h)
		for j := 0; j < h; j++ {
			r[i][j] = s[h-1-j][i]
		}
	}
	return r
}

func cloneShape(s [][]int) [][]int {
	c := make([][]int, len(s))
	for i := range s {
		c[i] = append([]int(nil), s[i]...)
	}
	return c
}

func shapeKey(s [][]int) string {
	b := make([]byte, 0, len(s)*len(s[0])+len(s))
	for _, row := range s {
		for _, v := range row {
			b = append(b, byte('0'+v))
		}
		b = append(b, '|')
	}
	return string(b)
}

func shapeCells(s [][]int) int {
	n := 0
	for _, row := range s {
		for _, v := range row {
			n += v
		}
	}
	return n
}

// BlokSimResult is the outcome of one simulated game.
type BlokSimResult struct {
	Score     int // real game score (cells placed + combo bonuses)
	Cells     int // total cells placed
	Trios     int // full 3-piece sets consumed
	MaxClears int // most lines cleared by a single placement
}

// BlokChooser ranks moves for a board + tray + live chain state (BlokPlan's
// signature), so the simulator can drive any solver variant.
type BlokChooser func(board [8][8]int, shapes [][][]int, chain BlokChain) []BlokScoredMove

// SimulateBlokGame plays one full game with the given chooser and RNG, returning
// the final score. It mirrors the live driver: draw a trio of 3 random pieces,
// then repeatedly re-plan (chooser) and place the top-ranked move until the trio
// is empty; refill; stop when no remaining piece fits anywhere. Chain payout and
// state live in BlokChain.Advance — the same transition the planner searches
// with, so the sim scores exactly what the planner optimises.
func SimulateBlokGame(rng *rand.Rand, choose BlokChooser) BlokSimResult {
	var board [8][8]int
	res := BlokSimResult{}
	var chain BlokChain

	place := func(s [][]int, r, c int) {
		res.Cells += shapeCells(s)
		res.Score += shapeCells(s) // +1 per cell placed
		nb, lines := BlokApply(board, s, r, c)
		board = nb
		if lines > res.MaxClears {
			res.MaxClears = lines
		}
		var bonus int
		chain, bonus = chain.Advance(lines > 0)
		res.Score += bonus
	}

	for {
		trio := make([][][]int, 3)
		for i := range trio {
			trio[i] = blokSimPieces[rng.Intn(len(blokSimPieces))]
		}
		remaining := trio
		for len(remaining) > 0 {
			// Any remaining piece placeable?
			anyFits := false
			for _, s := range remaining {
				if len(blokValid(board, s)) > 0 {
					anyFits = true
					break
				}
			}
			if !anyFits {
				return res // game over
			}
			moves := choose(board, remaining, chain)
			if len(moves) == 0 {
				return res
			}
			mv := moves[0]
			place(remaining[mv.Pi], mv.R, mv.C)
			remaining = append(remaining[:mv.Pi], remaining[mv.Pi+1:]...)
		}
		res.Trios++
	}
}
