package klublotto

import "math/rand"

// Blok for Blok offline simulator. We only get ONE live game per day, so to tune
// the solver (BlokPlan/blokQuality) we play thousands of simulated games here and
// measure the score distribution. The engine implements the real scoring rules:
// +1 point per cell placed, and a combo chain — the 1st line-clear starts the
// chain, and each further clear WITHIN 3 placements pays an escalating bonus
// (2nd +10, 3rd +20, …). Game over when none of the remaining tray pieces fits.
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

// BlokChooser ranks moves for a board + tray (BlokPlan's signature), so the
// simulator can drive any solver variant.
type BlokChooser func(board [8][8]int, shapes [][][]int) []BlokScoredMove

// SimulateBlokGame plays one full game with the given chooser and RNG, returning
// the final score. It mirrors the live driver: draw a trio of 3 random pieces,
// then repeatedly re-plan (chooser) and place the top-ranked move until the trio
// is empty; refill; stop when no remaining piece fits anywhere.
func SimulateBlokGame(rng *rand.Rand, choose BlokChooser) BlokSimResult {
	var board [8][8]int
	res := BlokSimResult{}
	comboLen := 0   // active chain length (0 = no chain)
	sinceClear := 4 // placements since last clear (>3 ⇒ chain expired)

	place := func(s [][]int, r, c int) {
		res.Cells += shapeCells(s)
		res.Score += shapeCells(s) // +1 per cell placed
		nb, lines := BlokApply(board, s, r, c)
		board = nb
		if lines > 0 {
			if lines > res.MaxClears {
				res.MaxClears = lines
			}
			if comboLen > 0 && sinceClear <= 3 {
				comboLen++
			} else {
				comboLen = 1 // start (or restart) the chain
			}
			res.Score += 10 * (comboLen - 1) // 1st clear +0, 2nd +10, 3rd +20…
			sinceClear = 0
		} else {
			sinceClear++
			if sinceClear > 3 {
				comboLen = 0
			}
		}
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
			moves := choose(board, remaining)
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
