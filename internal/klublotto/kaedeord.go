package klublotto

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

// Kædeord ("chain words"; immerspiele calls the game "fallout") is a Boggle-style
// grid: trace a word through touching tiles and it is removed, the tiles above
// it fall, and a column that empties closes up. The rules below are ported from
// the game's own bundle (themes/builds/34, read 2026-11-18), so the simulated
// board matches the live one move for move:
//
//   - adjacency: the 8 neighbours, plus wrap between column 0 and the last
//     column, and between the topmost occupied row and the bottom row
//   - removal: tiles above a removed tile fall; an emptied column is dropped and
//     the columns to its right shift left
//   - points: length² × the product of the word's letter multipliers
//   - words: at least 3 letters, in the game's own packed word list
//
// Clearing the board adds a bonus, and two lifelines (delete one tile, swap two)
// cost nothing. 50 points earns the daily lod.

// OpenKaedeord opens the parent Danske Spil page that embeds the game.
func OpenKaedeord(ctx context.Context, br *browser.Client) error {
	return openParentGame(ctx, br, KaedeordURL)
}

// KaedeordTile is one letter tile. ID is its index in the day's board string and
// never changes; X and Y are its current cell in the full Rows-high grid.
type KaedeordTile struct {
	ID     int
	X, Y   int
	Letter rune
	Mult   int
}

// KaedeordBoard is a board position. Rows is the full grid height. Width is the
// number of columns still holding tiles and Height the rows from the topmost
// tile to the bottom row: the two sizes the game wraps across.
type KaedeordBoard struct {
	Tiles  []KaedeordTile
	Rows   int
	Width  int
	Height int
}

// kaedeMaxTiles bounds tile IDs so a set of them fits in a kaedeSet.
const kaedeMaxTiles = 256

// ParseKaedeordBoard reads the game's board string: one letter per tile, row by
// row, each optionally followed by a digit multiplier ("W4" is a W worth ×4).
func ParseKaedeordBoard(board string, width int) (KaedeordBoard, error) {
	if width <= 0 {
		return KaedeordBoard{}, fmt.Errorf("kædeord board width %d", width)
	}
	var tiles []KaedeordTile
	rs := []rune(board)
	for i := 0; i < len(rs); i++ {
		n := len(tiles)
		t := KaedeordTile{ID: n, X: n % width, Y: n / width, Letter: rs[i], Mult: 1}
		if i+1 < len(rs) && rs[i+1] >= '0' && rs[i+1] <= '9' {
			if m := int(rs[i+1] - '0'); m > 0 {
				t.Mult = m
			}
			i++
		}
		tiles = append(tiles, t)
	}
	rows := (len(tiles) + width - 1) / width
	return NewKaedeordBoard(tiles, rows)
}

// NewKaedeordBoard builds a board from tiles at known cells, e.g. as read from
// the live DOM, deriving the current width and height the way the game does.
func NewKaedeordBoard(tiles []KaedeordTile, rows int) (KaedeordBoard, error) {
	b := KaedeordBoard{Tiles: append([]KaedeordTile(nil), tiles...), Rows: rows}
	seen := map[[2]int]bool{}
	for i, t := range b.Tiles {
		if t.ID < 0 || t.ID >= kaedeMaxTiles {
			return KaedeordBoard{}, fmt.Errorf("kædeord tile id %d out of range", t.ID)
		}
		if t.Y < 0 || t.Y >= rows || t.X < 0 {
			return KaedeordBoard{}, fmt.Errorf("kædeord tile %d at (%d,%d) outside a %d-row grid", t.ID, t.X, t.Y, rows)
		}
		if seen[[2]int{t.X, t.Y}] {
			return KaedeordBoard{}, fmt.Errorf("two kædeord tiles at (%d,%d)", t.X, t.Y)
		}
		seen[[2]int{t.X, t.Y}] = true
		if t.Mult <= 0 {
			b.Tiles[i].Mult = 1
		}
	}
	b.sortTiles()
	b.resize()
	return b, nil
}

func (b *KaedeordBoard) sortTiles() {
	sort.Slice(b.Tiles, func(i, j int) bool {
		if b.Tiles[i].Y != b.Tiles[j].Y {
			return b.Tiles[i].Y < b.Tiles[j].Y
		}
		return b.Tiles[i].X < b.Tiles[j].X
	})
}

// resize sets Width and Height from the tiles. Emptied columns are always
// dropped and tiles always fall, so the columns in use are 0..Width-1 and every
// column reaches the bottom row.
func (b *KaedeordBoard) resize() {
	b.Width, b.Height = 0, 0
	top := b.Rows
	for _, t := range b.Tiles {
		if t.X+1 > b.Width {
			b.Width = t.X + 1
		}
		if t.Y < top {
			top = t.Y
		}
	}
	if len(b.Tiles) > 0 {
		b.Height = b.Rows - top
	}
}

// String renders the board as rows of letters, "·" for an empty cell and a
// trailing digit for a multiplier.
func (b KaedeordBoard) String() string {
	at := map[[2]int]KaedeordTile{}
	for _, t := range b.Tiles {
		at[[2]int{t.X, t.Y}] = t
	}
	var sb strings.Builder
	for y := b.Rows - b.Height; y < b.Rows; y++ {
		for x := 0; x < b.Width; x++ {
			t, ok := at[[2]int{x, y}]
			switch {
			case !ok:
				sb.WriteString(" · ")
			case t.Mult > 1:
				sb.WriteString(" " + string(t.Letter) + strconv.Itoa(t.Mult))
			default:
				sb.WriteString(" " + string(t.Letter) + " ")
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Key identifies a position: equal keys mean equal boards.
func (b KaedeordBoard) Key() string {
	var sb strings.Builder
	for _, t := range b.Tiles {
		fmt.Fprintf(&sb, "%d.%d.%d,", t.ID, t.X, t.Y)
	}
	return sb.String()
}

func kaedeStep(dx, dy int) bool {
	return (dx == 1 && dy == 0) || (dx == 0 && dy == 1) || (dx == 1 && dy == 1)
}

func kaedeAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Adjacent reports whether two tiles may follow each other in a word.
func (b KaedeordBoard) Adjacent(p, q KaedeordTile) bool {
	dx, dy := kaedeAbs(p.X-q.X), kaedeAbs(p.Y-q.Y)
	if kaedeStep(dx, dy) {
		return true
	}
	last, top, bottom := b.Width-1, b.Rows-b.Height, b.Rows-1
	if b.Width > 1 && ((p.X == 0 && q.X == last) || (q.X == 0 && p.X == last)) {
		dx = 1
	}
	if b.Height > 1 && ((p.Y == top && q.Y == bottom) || (q.Y == top && p.Y == bottom)) {
		dy = 1
	}
	return kaedeStep(dx, dy)
}

// Remove returns the board after the given tiles are taken out: the tiles above
// each gap fall, and emptied columns are dropped.
func (b KaedeordBoard) Remove(ids ...int) KaedeordBoard {
	gone := map[int]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	cols := make([][]KaedeordTile, b.Width)
	removed := make([]int, b.Width)
	for _, t := range b.Tiles { // top to bottom
		if t.X >= b.Width {
			continue
		}
		if gone[t.ID] {
			removed[t.X]++
			continue
		}
		cols[t.X] = append(cols[t.X], t)
	}
	out := KaedeordBoard{Rows: b.Rows}
	shift := 0
	for x := 0; x < b.Width; x++ {
		if len(cols[x]) == 0 {
			shift++
			continue
		}
		// Each tile falls by the number of removed tiles below it. The column is
		// already packed down to the bottom row, so it ends packed again.
		for i, t := range cols[x] {
			t.X = x - shift
			t.Y = b.Rows - len(cols[x]) + i
			out.Tiles = append(out.Tiles, t)
		}
	}
	out.sortTiles()
	out.resize()
	return out
}

// Swap returns the board with two tiles' cells exchanged.
func (b KaedeordBoard) Swap(a, c int) KaedeordBoard {
	out := KaedeordBoard{Tiles: append([]KaedeordTile(nil), b.Tiles...), Rows: b.Rows}
	ia, ic := -1, -1
	for i, t := range out.Tiles {
		switch t.ID {
		case a:
			ia = i
		case c:
			ic = i
		}
	}
	if ia >= 0 && ic >= 0 {
		out.Tiles[ia].X, out.Tiles[ic].X = out.Tiles[ic].X, out.Tiles[ia].X
		out.Tiles[ia].Y, out.Tiles[ic].Y = out.Tiles[ic].Y, out.Tiles[ia].Y
	}
	out.sortTiles()
	out.resize()
	return out
}

// KaedeordPoints scores a word: length² times every letter's multiplier.
func KaedeordPoints(length int, mults []int) int {
	if length < 3 {
		return 0
	}
	p := length * length
	for _, m := range mults {
		if m > 1 {
			p *= m
		}
	}
	return p
}

// ParseKaedeordWordList unpacks the game's word list. It is front-coded: each
// entry is a base-36 count (digits and a-z) of letters shared with the previous
// word, then the new uppercase letters. As in the game, an entry is only kept
// once the next count starts, so the very last one is never added.
func ParseKaedeordWordList(packed string) []string {
	var words []string
	var prev, suffix []rune
	var num strings.Builder
	inNum, started := false, false
	flush := func() {
		n, err := strconv.ParseInt(num.String(), 36, 64)
		if err != nil || int(n) > len(prev) {
			n = int64(len(prev))
		}
		w := append(append([]rune(nil), prev[:n]...), suffix...)
		words = append(words, string(w))
		prev = w
	}
	for _, r := range packed {
		isNum := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z')
		switch {
		case !started:
			started, inNum = true, isNum
			if isNum {
				num.WriteRune(r)
			} else {
				suffix = append(suffix, r)
			}
		case isNum == inNum:
			if isNum {
				num.WriteRune(r)
			} else {
				suffix = append(suffix, r)
			}
		case isNum: // letters → count: the entry is complete
			flush()
			num.Reset()
			num.WriteRune(r)
			suffix = suffix[:0]
			inNum = true
		default:
			suffix = append(suffix, r)
			inNum = false
		}
	}
	return words
}

// KaedeordDict is the word list as a trie over the 29-letter Danish alphabet,
// for prefix-pruned path search.
type KaedeordDict struct {
	nodes []kaedeNode
	Words int
}

type kaedeNode struct {
	first, next int32 // first child and next sibling; 0 is none (node 0 is the root)
	letter      uint8
	word        bool
}

func kaedeLetter(r rune) (uint8, bool) {
	switch {
	case r >= 'A' && r <= 'Z':
		return uint8(r - 'A'), true
	case r == 'Æ':
		return 26, true
	case r == 'Ø':
		return 27, true
	case r == 'Å':
		return 28, true
	}
	return 0, false
}

// NewKaedeordDict builds the trie. Words of under 3 letters or with letters
// outside the alphabet can never be played and are skipped.
func NewKaedeordDict(words []string) *KaedeordDict {
	d := &KaedeordDict{nodes: make([]kaedeNode, 1, 1<<20)}
	for _, w := range words {
		rs := []rune(w)
		if len(rs) < 3 {
			continue
		}
		ls := make([]uint8, len(rs))
		ok := true
		for i, r := range rs {
			if ls[i], ok = kaedeLetter(r); !ok {
				break
			}
		}
		if !ok {
			continue
		}
		n := int32(0)
		for _, l := range ls {
			c := d.child(n, l)
			if c == 0 {
				c = int32(len(d.nodes))
				d.nodes = append(d.nodes, kaedeNode{letter: l, next: d.nodes[n].first})
				d.nodes[n].first = c
			}
			n = c
		}
		if !d.nodes[n].word {
			d.nodes[n].word = true
			d.Words++
		}
	}
	return d
}

func (d *KaedeordDict) child(n int32, l uint8) int32 {
	for c := d.nodes[n].first; c != 0; c = d.nodes[c].next {
		if d.nodes[c].letter == l {
			return c
		}
	}
	return 0
}

// Has reports whether the game accepts word.
func (d *KaedeordDict) Has(word string) bool {
	n := int32(0)
	for _, r := range word {
		l, ok := kaedeLetter(r)
		if !ok {
			return false
		}
		if n = d.child(n, l); n == 0 {
			return false
		}
	}
	return n != 0 && d.nodes[n].word
}

// KaedeordMove is one word to trace: tile IDs in path order.
type KaedeordMove struct {
	Word   string
	IDs    []int
	Points int
}

type kaedeSet [kaedeMaxTiles / 64]uint64

// Words lists every playable word on the board. Paths over the same tiles
// leave the same board and score the same, so each tile set appears once.
func (b KaedeordBoard) Words(d *KaedeordDict) []KaedeordMove {
	n := len(b.Tiles)
	letters := make([]uint8, n)
	valid := make([]bool, n)
	for i, t := range b.Tiles {
		letters[i], valid[i] = kaedeLetter(t.Letter)
	}
	nbr := make([][]int, n)
	for i := range b.Tiles {
		for j := range b.Tiles {
			if i != j && valid[j] && b.Adjacent(b.Tiles[i], b.Tiles[j]) {
				nbr[i] = append(nbr[i], j)
			}
		}
	}
	var moves []KaedeordMove
	seen := map[kaedeSet]bool{}
	used := make([]bool, n)
	path := make([]int, 0, 32)
	var dfs func(i int, node int32)
	dfs = func(i int, node int32) {
		if d.nodes[node].word && len(path) >= 3 {
			var key kaedeSet
			for _, k := range path {
				id := b.Tiles[k].ID
				key[id/64] |= 1 << (id % 64)
			}
			if !seen[key] {
				seen[key] = true
				m := KaedeordMove{IDs: make([]int, len(path))}
				word := make([]rune, len(path))
				mults := make([]int, len(path))
				for p, k := range path {
					m.IDs[p] = b.Tiles[k].ID
					word[p] = b.Tiles[k].Letter
					mults[p] = b.Tiles[k].Mult
				}
				m.Word = string(word)
				m.Points = KaedeordPoints(len(path), mults)
				moves = append(moves, m)
			}
		}
		for _, j := range nbr[i] {
			if used[j] {
				continue
			}
			c := d.child(node, letters[j])
			if c == 0 {
				continue
			}
			used[j] = true
			path = append(path, j)
			dfs(j, c)
			path = path[:len(path)-1]
			used[j] = false
		}
	}
	for i := range b.Tiles {
		if !valid[i] {
			continue
		}
		c := d.child(0, letters[i])
		if c == 0 {
			continue
		}
		used[i] = true
		path = append(path[:0], i)
		dfs(i, c)
		used[i] = false
	}
	sort.SliceStable(moves, func(i, j int) bool {
		if moves[i].Points != moves[j].Points {
			return moves[i].Points > moves[j].Points
		}
		return moves[i].Word < moves[j].Word
	})
	return moves
}

// KaedeordStepKind says what a plan step does.
type KaedeordStepKind int

const (
	KaedeordWord   KaedeordStepKind = iota // trace Move
	KaedeordDelete                         // delete Tiles[0]
	KaedeordSwap                           // swap Tiles[0] and Tiles[1]
)

// KaedeordStep is one action of a plan.
type KaedeordStep struct {
	Kind  KaedeordStepKind
	Move  KaedeordMove
	Tiles []int
}

func (s KaedeordStep) String() string {
	switch s.Kind {
	case KaedeordDelete:
		return fmt.Sprintf("delete tile %d", s.Tiles[0])
	case KaedeordSwap:
		return fmt.Sprintf("swap tiles %d and %d", s.Tiles[0], s.Tiles[1])
	}
	return fmt.Sprintf("%s (+%d)", s.Move.Word, s.Move.Points)
}

// Apply returns the board after the step.
func (b KaedeordBoard) Apply(s KaedeordStep) KaedeordBoard {
	switch s.Kind {
	case KaedeordDelete:
		return b.Remove(s.Tiles[0])
	case KaedeordSwap:
		return b.Swap(s.Tiles[0], s.Tiles[1])
	}
	return b.Remove(s.Move.IDs...)
}

// KaedeordPlan is a sequence of steps and what it scores.
type KaedeordPlan struct {
	Steps   []KaedeordStep
	Points  int  // word points
	Cleared bool // the steps empty the board
	Score   int  // Points plus the clear bonus when Cleared
}

// KaedeordOptions tunes SolveKaedeord.
type KaedeordOptions struct {
	Beam       int             // positions kept per depth (default 300)
	ClearBonus int             // points for emptying the board (the game's is 100)
	Deletes    int             // delete lifelines left
	Swaps      int             // swap lifelines left
	Deadline   time.Time       // stop deepening after this (zero = none)
	Avoid      map[string]bool // words the game turned down
}

type kaedeState struct {
	board   KaedeordBoard
	points  int
	steps   []KaedeordStep
	deletes int
	swaps   int
	rank    float64
}

// kaedeTileWeight is how many points the beam credits per tile removed by a
// word, on top of the word's own points. It steers toward boards that can still
// be cleared without letting cheap short words crowd out long ones.
const kaedeTileWeight = 2.0

// SolveKaedeord searches for the highest-scoring sequence of words from board,
// using lifelines where they let play continue. It is a beam search: every
// depth expands each kept position by all its words, then keeps the best
// Beam positions by points plus a small credit per tile removed. A position
// with no words left may spend a delete or a swap. The best plan seen at any
// depth is returned, finished with deletes when they can clear the rest.
func SolveKaedeord(board KaedeordBoard, d *KaedeordDict, opt KaedeordOptions) KaedeordPlan {
	if opt.Beam <= 0 {
		opt.Beam = 300
	}
	start := len(board.Tiles)
	finish := func(s *kaedeState) KaedeordPlan {
		p := KaedeordPlan{Steps: append([]KaedeordStep(nil), s.steps...), Points: s.points, Score: s.points}
		if n := len(s.board.Tiles); n == 0 || n <= s.deletes {
			for _, t := range s.board.Tiles {
				p.Steps = append(p.Steps, KaedeordStep{Kind: KaedeordDelete, Tiles: []int{t.ID}})
			}
			p.Cleared = true
			p.Score += opt.ClearBonus
		}
		return p
	}
	rank := func(s *kaedeState) float64 {
		r := float64(s.points) + kaedeTileWeight*float64(start-len(s.board.Tiles))
		if n := len(s.board.Tiles); n == 0 || n <= s.deletes {
			r += float64(opt.ClearBonus)
		}
		return r
	}

	root := &kaedeState{board: board, deletes: opt.Deletes, swaps: opt.Swaps}
	best := finish(root)
	beam := []*kaedeState{root}
	for len(beam) > 0 {
		if !opt.Deadline.IsZero() && time.Now().After(opt.Deadline) {
			break
		}
		next := map[string]*kaedeState{}
		add := func(parent *kaedeState, step KaedeordStep, points int) {
			child := &kaedeState{
				board:   parent.board.Apply(step),
				points:  parent.points + points,
				steps:   append(append([]KaedeordStep(nil), parent.steps...), step),
				deletes: parent.deletes,
				swaps:   parent.swaps,
			}
			switch step.Kind {
			case KaedeordDelete:
				child.deletes--
			case KaedeordSwap:
				child.swaps--
			}
			child.rank = rank(child)
			key := fmt.Sprintf("%s|%d|%d", child.board.Key(), child.deletes, child.swaps)
			if old, ok := next[key]; !ok || child.rank > old.rank {
				next[key] = child
			}
			if p := finish(child); p.Score > best.Score || (p.Score == best.Score && len(p.Steps) < len(best.Steps)) {
				best = p
			}
		}
		for _, s := range beam {
			moves := s.board.Words(d)
			if len(opt.Avoid) > 0 {
				kept := moves[:0]
				for _, m := range moves {
					if !opt.Avoid[m.Word] {
						kept = append(kept, m)
					}
				}
				moves = kept
			}
			for _, m := range moves {
				add(s, KaedeordStep{Kind: KaedeordWord, Move: m}, m.Points)
			}
			if len(moves) > 0 || len(s.board.Tiles) == 0 {
				continue
			}
			// Stuck: a lifeline is the only way on. Keep only those that open
			// up a word.
			if s.deletes > 0 {
				for _, t := range s.board.Tiles {
					step := KaedeordStep{Kind: KaedeordDelete, Tiles: []int{t.ID}}
					if len(s.board.Apply(step).Words(d)) > 0 {
						add(s, step, 0)
					}
				}
			}
			if s.swaps > 0 {
				for i, a := range s.board.Tiles {
					for _, c := range s.board.Tiles[i+1:] {
						if a.Letter == c.Letter {
							continue
						}
						step := KaedeordStep{Kind: KaedeordSwap, Tiles: []int{a.ID, c.ID}}
						if len(s.board.Apply(step).Words(d)) > 0 {
							add(s, step, 0)
						}
					}
				}
			}
		}
		beam = beam[:0]
		for _, s := range next {
			beam = append(beam, s)
		}
		sort.Slice(beam, func(i, j int) bool {
			if beam[i].rank != beam[j].rank {
				return beam[i].rank > beam[j].rank
			}
			return beam[i].board.Key() < beam[j].board.Key()
		})
		if len(beam) > opt.Beam {
			beam = beam[:opt.Beam]
		}
	}
	return best
}
