package klublotto

import (
	"strings"
	"testing"
)

// boardString renders an 8x8 board the same way blok.py does (# filled, . empty)
// so the parity assertion reads like the Python tool's stdout.
func boardString(g [8][8]int) string {
	var b strings.Builder
	for r := 0; r < 8; r++ {
		for c := 0; c < 8; c++ {
			if g[r][c] != 0 {
				b.WriteByte('#')
			} else {
				b.WriteByte('.')
			}
			if c == 7 {
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func shapeString(s [][]int) string {
	var b strings.Builder
	for _, row := range s {
		for _, v := range row {
			if v != 0 {
				b.WriteByte('#')
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TestBlokPerceptionMatchesFixture pins the Go perception to the exact output the
// Python tool produced on a saved mid-game screenshot (52/200, 2026-06-18):
//
//	board:                    piece0 2x3 cells=4 pick=(599,688)  ".##" / "##."
//	  ..###...                piece1 3x2 cells=4 pick=(734,688)  ".#" / "##" / "#."
//	  ..###...
//	  ######..
//	  ...###..
//	  .##.##..
//	  .#######
//	  .#...##.
//	  ........
//
// If this drifts, the port no longer matches the tuned Python perception.
func TestBlokPerceptionMatchesFixture(t *testing.T) {
	im, err := loadBlokImage("testdata/blok_state.png")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	g, geom, ok := readBoard(im)
	if !ok {
		t.Fatal("readBoard reported no board on the fixture")
	}

	wantBoard := strings.Join([]string{
		"..###...",
		"..###...",
		"######..",
		"...###..",
		".##.##..",
		".#######",
		".#...##.",
		"........",
	}, "\n") + "\n"
	if got := boardString(g); got != wantBoard {
		t.Fatalf("board mismatch:\n got:\n%s\nwant:\n%s", got, wantBoard)
	}

	pieces := readPieces(im, geom)
	if len(pieces) != 2 {
		t.Fatalf("got %d pieces, want 2", len(pieces))
	}

	type want struct {
		h, w, cells, px, py int
		shape               string
	}
	wants := []want{
		{2, 3, 4, 599, 688, ".##\n##.\n"},
		{3, 2, 4, 734, 688, ".#\n##\n#.\n"},
	}
	for i, w := range wants {
		p := pieces[i]
		if p.H != w.h || p.W != w.w || p.Cells != w.cells {
			t.Errorf("piece%d dims = %dx%d cells=%d, want %dx%d cells=%d",
				i, p.H, p.W, p.Cells, w.h, w.w, w.cells)
		}
		if p.PickX != w.px || p.PickY != w.py {
			t.Errorf("piece%d pick = (%d,%d), want (%d,%d)", i, p.PickX, p.PickY, w.px, w.py)
		}
		if got := shapeString(p.Shape); got != w.shape {
			t.Errorf("piece%d shape:\n got:\n%swant:\n%s", i, got, w.shape)
		}
	}
}

func TestBlokDetectBoardNoBoard(t *testing.T) {
	// A blank (all-black) frame — the win/game-over screen has no maroon board.
	// detectBoard must report ok=false rather than indexing off the image.
	im := &blokImage{w: 200, h: 200, pix: make([]uint8, 3*200*200)}
	if _, _, _, ok := detectBoard(im); ok {
		t.Fatal("detectBoard reported a board on a blank frame")
	}
}

func TestBlokApplyClearsLines(t *testing.T) {
	// A board with row 0 missing only its last cell; dropping a 1x1-ish piece
	// there clears the row.
	var b [8][8]int
	for c := 0; c < 7; c++ {
		b[0][c] = 1
	}
	g, cleared := BlokApply(b, [][]int{{1}}, 0, 7)
	if cleared != 1 {
		t.Fatalf("cleared = %d, want 1", cleared)
	}
	for c := 0; c < 8; c++ {
		if g[0][c] != 0 {
			t.Fatalf("row 0 not cleared: col %d = %d", c, g[0][c])
		}
	}
}

func TestBlokValidRespectsCollisions(t *testing.T) {
	var b [8][8]int
	b[0][0] = 1
	// A 1x1 piece can go anywhere except (0,0).
	got := blokValid(b, [][]int{{1}})
	if len(got) != 63 {
		t.Fatalf("valid placements = %d, want 63", len(got))
	}
	for _, rc := range got {
		if rc[0] == 0 && rc[1] == 0 {
			t.Fatal("blokValid returned the occupied cell (0,0)")
		}
	}
}

func TestBlokPlanPrefersLineClear(t *testing.T) {
	// Row 0 filled except the last two cells; a 1x2 piece completes it. The
	// line-clearing placement (0,6) must rank first (120*1 + quality beats any
	// non-clearing placement's pure quality).
	var b [8][8]int
	for c := 0; c < 6; c++ {
		b[0][c] = 1
	}
	shapes := [][][]int{{{1, 1}}}
	ranked := BlokPlan(b, shapes, BlokChain{})
	if len(ranked) == 0 {
		t.Fatal("no moves planned")
	}
	top := ranked[0]
	if top.Pi != 0 || top.R != 0 || top.C != 6 {
		t.Fatalf("top move = piece%d@(%d,%d), want piece0@(0,6)", top.Pi, top.R, top.C)
	}
}

func TestBlokChainAdvance(t *testing.T) {
	// Payout schedule confirmed from the 2026-07-07 continuous-chain live trace
	// (validated to chain 41/+400 live 2026-07-11): the k-th clearing placement
	// pays 10×(k−1) — only the FIRST clear is free. Gap rule corrected from the
	// 2026-07-11 193-move game: the chain survives gaps of ≤2 non-clearing
	// placements and dies on the 3rd (89/89 continuations at gaps 0-2, 3/3
	// +0 restarts at gap 3 — the game's only mispredicted moves that day).
	var ch BlokChain
	wantPay := []int{0, 10, 20, 30, 40}
	for k, want := range wantPay {
		var pay int
		ch, pay = ch.Advance(true)
		if pay != want || ch.Len != k+1 {
			t.Fatalf("clear #%d: pay=%d len=%d, want pay=%d len=%d", k+1, pay, ch.Len, want, k+1)
		}
	}
	// Two non-clears keep the chain alive; the next clear extends it.
	for i := 0; i < 2; i++ {
		ch, _ = ch.Advance(false)
	}
	if ch.Len != 5 {
		t.Fatalf("chain died too early: len=%d after 2 non-clears", ch.Len)
	}
	ch, pay := ch.Advance(true)
	if pay != 50 || ch.Len != 6 {
		t.Fatalf("clear #6 after gap of 2: pay=%d len=%d, want 50/6", pay, ch.Len)
	}
	// Three non-clears kill it; the next clear restarts at len 1, pay 0.
	for i := 0; i < 3; i++ {
		ch, _ = ch.Advance(false)
	}
	if ch.Len != 0 {
		t.Fatalf("chain should be dead after 3 non-clears, len=%d", ch.Len)
	}
	ch, pay = ch.Advance(true)
	if pay != 0 || ch.Len != 1 {
		t.Fatalf("restart clear: pay=%d len=%d, want 0/1", pay, ch.Len)
	}
}

func TestBlokPlanRewardsChainedClears(t *testing.T) {
	// Rows 0 and 7 each need their last two cells; two 1x2 pieces each complete
	// one row → two SEPARATE clearing placements. From a cold chain the 1st clear
	// pays 0 and the 2nd pays 10, and the branch ends with a LIVE chain of 2.
	// Expected top score: survival + 2 lines × 120 + BlokWChain×10 bonus +
	// 2×BlokWChainState + quality(empty board)=64.
	var b [8][8]int
	for c := 0; c < 6; c++ {
		b[0][c] = 1
		b[7][c] = 1
	}
	shapes := [][][]int{{{1, 1}}, {{1, 1}}}
	ranked := BlokPlan(b, shapes, BlokChain{})
	if len(ranked) == 0 {
		t.Fatal("no moves planned")
	}
	want := 2*10000 + 2*120 + BlokWChain*10 + 2*BlokWChainState + 64
	if ranked[0].Score != want {
		t.Fatalf("top score = %d, want %d (survival + clears + chain bonus + live-chain state)", ranked[0].Score, want)
	}
}

func TestBlokPlanSequencesClearsOverDoubleClear(t *testing.T) {
	// Rows 0 and 1 are both one 2-cell gap from clearing at cols 6-7. Tray: a 2x2
	// (fills both gaps at once → ONE clearing placement, one chain step) and a
	// 1x2 (row 0 alone, then the 2x2 finishes row 1 → TWO clearing placements).
	// The game pays NOTHING extra for a multi-line clear, so with a live chain of
	// 3 the real payout makes sequencing strictly better:
	//   double clear: pay 10×(4−1)=30, chain ends at 4
	//   sequenced:    pays 30 + 40 = 70, chain ends at 5
	// The top-ranked FIRST move must be the 1x2 at (0,6), not the 2x2 double clear.
	var b [8][8]int
	for c := 0; c < 6; c++ {
		b[0][c] = 1
		b[1][c] = 1
	}
	shapes := [][][]int{
		{{1, 1}, {1, 1}}, // piece0: 2x2
		{{1, 1}},         // piece1: 1x2
	}
	ranked := BlokPlan(b, shapes, BlokChain{Len: 3, SinceClear: 0})
	if len(ranked) == 0 {
		t.Fatal("no moves planned")
	}
	top := ranked[0]
	if top.Pi != 1 || top.R != 0 || top.C != 6 {
		t.Fatalf("top move = piece%d@(%d,%d), want piece1 (1x2) @(0,6) — sequenced clears must beat the double clear", top.Pi, top.R, top.C)
	}
}
