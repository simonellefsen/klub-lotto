package klublotto

import (
	"math/rand"
	"testing"
)

func TestSimulateBlokGameDeterministicAndScores(t *testing.T) {
	// A game is deterministic for a given seed and produces a real, positive score
	// (places pieces, reaches the daily lod on a mostly-easy seed). Guards the
	// engine + piece set against silent breakage.
	r1 := SimulateBlokGame(rand.New(rand.NewSource(1)), BlokPlan)
	r2 := SimulateBlokGame(rand.New(rand.NewSource(1)), BlokPlan)
	if r1 != r2 {
		t.Fatalf("non-deterministic for the same seed: %+v vs %+v", r1, r2)
	}
	if r1.Score <= 0 || r1.Cells <= 0 || r1.Trios <= 0 {
		t.Fatalf("degenerate game: %+v", r1)
	}
	// Score is cells placed plus combo bonuses, so it must be at least the cells.
	if r1.Score < r1.Cells {
		t.Fatalf("score %d < cells %d (combo bonus can't be negative)", r1.Score, r1.Cells)
	}
	// The piece set expands to a sane number of oriented shapes (no 1x1).
	if len(blokSimPieces) < 20 {
		t.Fatalf("piece set too small: %d", len(blokSimPieces))
	}
	for _, s := range blokSimPieces {
		if shapeCells(s) < 2 {
			t.Fatalf("piece with <2 cells in set: %v", s)
		}
	}
}
