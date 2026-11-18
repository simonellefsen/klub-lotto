package klublotto

import (
	"os"
	"reflect"
	"testing"
)

func TestParseKaedeordWordList(t *testing.T) {
	// Front-coded: a base-36 count of letters shared with the previous word,
	// then the new letters. "aZ" keeps 10 letters. The game never adds the
	// last entry ("0X"), so neither do we.
	got := ParseKaedeordWordList("0KAT2R1ÆDE4ORD0ABCDEFGHIJKaZ0X")
	want := []string{"KAT", "KAR", "KÆDE", "KÆDEORD", "ABCDEFGHIJK", "ABCDEFGHIJZ"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestKaedeordDict(t *testing.T) {
	d := NewKaedeordDict([]string{"KAT", "KÆDE", "OK", "ÅL", "CAFÉ"})
	if d.Words != 2 {
		t.Fatalf("Words = %d, want 2 (2-letter words and É can never be played)", d.Words)
	}
	for w, want := range map[string]bool{"KAT": true, "KÆDE": true, "KA": false, "KATTE": false, "OK": false} {
		if d.Has(w) != want {
			t.Errorf("Has(%s) = %v, want %v", w, !want, want)
		}
	}
}

func kaedeTile(b KaedeordBoard, id int) KaedeordTile {
	for _, t := range b.Tiles {
		if t.ID == id {
			return t
		}
	}
	panic("no tile")
}

func TestKaedeordParseBoard(t *testing.T) {
	b, err := ParseKaedeordBoard("ABCD2EFGHI", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Tiles) != 9 || b.Width != 3 || b.Height != 3 || b.Rows != 3 {
		t.Fatalf("got %d tiles, %dx%d of %d rows", len(b.Tiles), b.Width, b.Height, b.Rows)
	}
	if d := kaedeTile(b, 3); d.Letter != 'D' || d.Mult != 2 || d.X != 0 || d.Y != 1 {
		t.Fatalf("tile 3 = %+v, want D×2 at (0,1)", d)
	}
	if e := kaedeTile(b, 4); e.Letter != 'E' || e.Mult != 1 {
		t.Fatalf("tile 4 = %+v, want a plain E", e)
	}
}

func TestKaedeordAdjacentWraps(t *testing.T) {
	b, _ := ParseKaedeordBoard("ABCDEFGHIJKLMNOP", 4) // 4×4: A=0 … P=15
	for _, c := range []struct {
		p, q int
		want bool
	}{
		{0, 1, true},   // A–B side by side
		{0, 5, true},   // A–F diagonal
		{0, 2, false},  // A–C two apart
		{0, 10, false}, // A–K two apart diagonally
		{0, 3, true},   // A–D across the left/right edge
		{0, 12, true},  // A–M across the top/bottom edge
		{0, 15, true},  // A–P across both edges
		{4, 11, true},  // E–L across the edge, one row down
		{4, 15, false}, // E–P across the edge but two rows down
	} {
		if got := b.Adjacent(kaedeTile(b, c.p), kaedeTile(b, c.q)); got != c.want {
			t.Errorf("Adjacent(%d,%d) = %v, want %v", c.p, c.q, got, c.want)
		}
	}
}

func TestKaedeordRemove(t *testing.T) {
	b, _ := ParseKaedeordBoard("ABCDEFGHI", 3)

	// E's removal drops B into its cell.
	r := b.Remove(4)
	if bt := kaedeTile(r, 1); bt.X != 1 || bt.Y != 1 {
		t.Fatalf("B at (%d,%d), want (1,1)", bt.X, bt.Y)
	}
	if r.Height != 3 {
		t.Fatalf("height %d, want 3: A and C still fill the top row", r.Height)
	}

	// Emptying the middle column shifts the right column left.
	r = b.Remove(1, 4, 7)
	if r.Width != 2 {
		t.Fatalf("width %d, want 2", r.Width)
	}
	if ct := kaedeTile(r, 2); ct.X != 1 || ct.Y != 0 {
		t.Fatalf("C at (%d,%d), want (1,0)", ct.X, ct.Y)
	}

	if b.Remove(0, 1, 2, 3, 4, 5, 6, 7, 8).Tiles != nil {
		t.Fatal("removing every tile should leave an empty board")
	}

	// The top/bottom wrap follows the topmost occupied row: once the top row
	// is gone, D (0,1) is on top and wraps to J (0,3) at the bottom.
	tall, _ := ParseKaedeordBoard("ABCDEFGHIJKL", 3) // A B C / D E F / G H I / J K L
	if tall.Adjacent(kaedeTile(tall, 3), kaedeTile(tall, 9)) {
		t.Fatal("D and J touch on the full board")
	}
	r = tall.Remove(0, 1, 2)
	if r.Height != 3 || r.Width != 3 {
		t.Fatalf("got %dx%d, want 3x3", r.Width, r.Height)
	}
	if !r.Adjacent(kaedeTile(r, 3), kaedeTile(r, 9)) {
		t.Fatal("D (now the top row) should wrap to J on the bottom row")
	}
}

func TestKaedeordSwap(t *testing.T) {
	b, _ := ParseKaedeordBoard("ABCD", 2)
	s := b.Swap(0, 3)
	if a, d := kaedeTile(s, 0), kaedeTile(s, 3); a.X != 1 || a.Y != 1 || d.X != 0 || d.Y != 0 {
		t.Fatalf("A at (%d,%d), D at (%d,%d) after swapping", a.X, a.Y, d.X, d.Y)
	}
}

func TestKaedeordPoints(t *testing.T) {
	for _, c := range []struct {
		n     int
		mults []int
		want  int
	}{
		{2, nil, 0},
		{3, []int{1, 1, 1}, 9},
		{4, []int{1, 1, 1, 1}, 16},
		{7, []int{4, 1, 1, 1, 1, 1, 1}, 196},
		{3, []int{2, 3, 1}, 54},
	} {
		if got := KaedeordPoints(c.n, c.mults); got != c.want {
			t.Errorf("KaedeordPoints(%d, %v) = %d, want %d", c.n, c.mults, got, c.want)
		}
	}
}

func TestKaedeordWordsAndSolve(t *testing.T) {
	d := NewKaedeordDict([]string{"KAT", "TAK", "HUS", "SKAT"})
	b, _ := ParseKaedeordBoard("KA2THUS", 3) // K A×2 T / H U S
	words := map[string]int{}
	for _, m := range b.Words(d) {
		words[m.Word] = m.Points
	}
	// KAT and TAK use the same three tiles, so only one of them is listed.
	if words["HUS"] != 9 || words["KAT"]+words["TAK"] != 18 {
		t.Fatalf("words = %v", words)
	}
	p := SolveKaedeord(b, d, KaedeordOptions{ClearBonus: 100})
	if !p.Cleared || p.Score != 18+9+100 {
		t.Fatalf("plan = %+v, want both words and the board cleared", p)
	}

	// A leftover tile is only cleared with a delete to spare.
	b, _ = ParseKaedeordBoard("KATX", 4)
	if p := SolveKaedeord(b, d, KaedeordOptions{ClearBonus: 100}); p.Cleared || p.Score != 9 {
		t.Fatalf("no deletes: plan = %+v", p)
	}
	p = SolveKaedeord(b, d, KaedeordOptions{ClearBonus: 100, Deletes: 1})
	if !p.Cleared || p.Score != 109 || p.Steps[len(p.Steps)-1].Kind != KaedeordDelete {
		t.Fatalf("one delete: plan = %+v", p)
	}

	// A word the game turned down is never planned again.
	p = SolveKaedeord(b, d, KaedeordOptions{Avoid: map[string]bool{"KAT": true, "TAK": true}})
	if len(p.Steps) != 0 {
		t.Fatalf("avoided words still planned: %+v", p)
	}
}

// TestKaedeordSolvesRecordedBoard runs the solver on the 2026-11-18 board with
// the real word list. Point KAEDEORD_WORDS at a downloaded words-*.txt to run it.
func TestKaedeordSolvesRecordedBoard(t *testing.T) {
	path := os.Getenv("KAEDEORD_WORDS")
	if path == "" {
		t.Skip("set KAEDEORD_WORDS to the game's word list")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := NewKaedeordDict(ParseKaedeordWordList(string(raw)))
	b, _ := ParseKaedeordBoard("ARKPSFIGNJETUMW4EVØDLHCBSLBVORÅIJEFTKNMCPDEAGÆEHYK", 7)
	p := SolveKaedeord(b, d, KaedeordOptions{Beam: 20, ClearBonus: 100, Deletes: 3, Swaps: 1})
	t.Logf("score %d: %v", p.Score, p.Steps)
	if p.Score < 500 || !p.Cleared {
		t.Fatalf("plan scores %d (cleared %v), want 500+ and cleared", p.Score, p.Cleared)
	}
	// Replaying the plan must empty the board exactly.
	for _, s := range p.Steps {
		if s.Kind == KaedeordWord && !d.Has(s.Move.Word) {
			t.Fatalf("planned %s, which is not in the list", s.Move.Word)
		}
		b = b.Apply(s)
	}
	if len(b.Tiles) != 0 {
		t.Fatalf("%d tiles left after the plan", len(b.Tiles))
	}
}
