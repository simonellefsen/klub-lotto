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
	ranked := BlokPlan(b, shapes)
	if len(ranked) == 0 {
		t.Fatal("no moves planned")
	}
	top := ranked[0]
	if top.Pi != 0 || top.R != 0 || top.C != 6 {
		t.Fatalf("top move = piece%d@(%d,%d), want piece0@(0,6)", top.Pi, top.R, top.C)
	}
}

func TestBlokPlanRewardsCombo(t *testing.T) {
	// Rows 0 and 7 each need their last two cells; two 1x2 pieces each complete one
	// row → two SEPARATE clearing placements = a combo. Both rows then clear, so the
	// board ends empty. Expected top score: 2 lines × 120 (survival proxy) + 10 combo
	// bonus for the 2nd clearing placement + quality(empty board)=64.
	var b [8][8]int
	for c := 0; c < 6; c++ {
		b[0][c] = 1
		b[7][c] = 1
	}
	shapes := [][][]int{{{1, 1}}, {{1, 1}}}
	ranked := BlokPlan(b, shapes)
	if len(ranked) == 0 {
		t.Fatal("no moves planned")
	}
	want := 2*120 + 10 + 64
	if ranked[0].Score != want {
		t.Fatalf("top score = %d, want %d (double clear + combo bonus)", ranked[0].Score, want)
	}
}
