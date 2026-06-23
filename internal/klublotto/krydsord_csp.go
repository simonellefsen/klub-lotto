package klublotto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Krydsord constraint-satisfaction model + solution validation. This is the pure
// (no browser, no LLM, no I/O) core of the two-stage crossword solver: it
// flattens the stage-1 clue graph into a cell-level CSP, lays answers onto the
// grid, and validates crossings. Deterministic and unit-tested
// (krydsord_csp_test.go); the CLI driver in cmd/klub-lotto handles vision,
// prompting, and submission.

// KrydsordStart is a clue's 1-indexed start cell. It tolerates both the legacy
// [row, col] array form and the {"row","column"} object form in graph JSON.
type KrydsordStart struct {
	Row int `json:"row"`
	Col int `json:"column"`
}

func (s *KrydsordStart) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) > 0 && t[0] == '[' { // legacy [row, col]
		var arr []int
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		if len(arr) == 2 {
			s.Row, s.Col = arr[0], arr[1]
		}
		return nil
	}
	type alias KrydsordStart // object {"row","column"}
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*s = KrydsordStart(a)
	return nil
}

func (s KrydsordStart) valid() bool { return s.Row >= 1 && s.Col >= 1 }

// KrydsordGraphClue / KrydsordGraph mirror the stage-1 graph JSON.
type KrydsordGraphClue struct {
	Clue      string        `json:"clue"`
	Direction string        `json:"direction"`
	Start     KrydsordStart `json:"start"` // {"row":R,"column":C}, 1-indexed
	Length    int           `json:"length"`
}

type KrydsordGraph struct {
	Across []KrydsordGraphClue `json:"Across"`
	Down   []KrydsordGraphClue `json:"Down"`
}

// KrydsordCSP is the flattened constraint-satisfaction view of the crossword:
// every entry's exact cells plus, per cell, which (entry:position) pairs share
// it. Shared cells (>1 member) ARE the crossings — letters there must match.
// Handing this to the solver removes all geometry inference; it can focus on
// language + crossings, where LLMs are strongest.
type KrydsordCSP struct {
	Language string                      `json:"language"`
	Entries  map[string]KrydsordCSPEntry `json:"entries"`
	Cells    map[string][]string         `json:"cells"`
}

type KrydsordCSPEntry struct {
	Clue   string   `json:"clue"`
	Length int      `json:"length"`
	Cells  []string `json:"cells"`
}

// BuildKrydsordCSP flattens the clue graph into the CSP structure. Cell ids are
// "r<row>c<col>" (1-indexed); membership entries are "<EntryID>:<position>" with
// position 1-indexed (letter 1 = first cell of the answer).
func BuildKrydsordCSP(g KrydsordGraph) KrydsordCSP {
	csp := KrydsordCSP{Language: "da", Entries: map[string]KrydsordCSPEntry{}, Cells: map[string][]string{}}
	add := func(id, clue string, length, r, c int, down bool) {
		e := KrydsordCSPEntry{Clue: clue, Length: length}
		for k := 0; k < length; k++ {
			rr, cc := r, c+k
			if down {
				rr, cc = r+k, c
			}
			cid := fmt.Sprintf("r%dc%d", rr, cc)
			e.Cells = append(e.Cells, cid)
			csp.Cells[cid] = append(csp.Cells[cid], fmt.Sprintf("%s:%d", id, k+1))
		}
		csp.Entries[id] = e
	}
	for i, a := range g.Across {
		if a.Start.valid() && a.Length > 0 {
			add(fmt.Sprintf("A%d", i+1), a.Clue, a.Length, a.Start.Row, a.Start.Col, false)
		}
	}
	for i, d := range g.Down {
		if d.Start.valid() && d.Length > 0 {
			add(fmt.Sprintf("D%d", i+1), d.Clue, d.Length, d.Start.Row, d.Start.Col, true)
		}
	}
	return csp
}

// RenderKrydsordBoard draws a compact ASCII grid of the puzzle from the CSP so
// the LLM gets the spatial layout, not just the cell lists: "·" = an answer cell
// in one entry, "+" = a crossing cell (shared by an across and a down entry),
// blank = not an answer cell. Row/column headers map back to the cell ids.
func RenderKrydsordBoard(csp KrydsordCSP) string {
	type rc struct{ r, c int }
	count := map[rc]int{}
	maxR, maxC := 0, 0
	for cid, members := range csp.Cells {
		var r, c int
		if _, err := fmt.Sscanf(cid, "r%dc%d", &r, &c); err != nil {
			continue
		}
		count[rc{r, c}] = len(members)
		if r > maxR {
			maxR = r
		}
		if c > maxC {
			maxC = c
		}
	}
	var b strings.Builder
	b.WriteString("    ")
	for c := 1; c <= maxC; c++ {
		fmt.Fprintf(&b, "%2d ", c)
	}
	b.WriteString("\n")
	for r := 1; r <= maxR; r++ {
		fmt.Fprintf(&b, "%3d ", r)
		for c := 1; c <= maxC; c++ {
			switch n := count[rc{r, c}]; {
			case n >= 2:
				b.WriteString(" + ")
			case n == 1:
				b.WriteString(" · ")
			default:
				b.WriteString("   ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// BuildKrydsordGridFromAnswers places each solved answer's letters into its CSP
// cells, producing a w×h grid of rows (answer cells = uppercase letter, every
// other cell = "."). The cell ids ("r<row>c<col>", 1-indexed) line up with the
// live API mask, so the result feeds straight into ValidateKrydsordAnswerGrid /
// BuildKrydsordUserSolution.
func BuildKrydsordGridFromAnswers(csp KrydsordCSP, answersByID map[string]string, w, h int) []string {
	grid := make([][]rune, h)
	for r := range grid {
		grid[r] = make([]rune, w)
		for c := range grid[r] {
			grid[r][c] = '.'
		}
	}
	for id, e := range csp.Entries {
		a := []rune(answersByID[id])
		if len(a) != e.Length {
			continue
		}
		for k, cid := range e.Cells {
			var r, c int
			if _, err := fmt.Sscanf(cid, "r%dc%d", &r, &c); err != nil {
				continue
			}
			if r >= 1 && r <= h && c >= 1 && c <= w {
				grid[r-1][c-1] = a[k]
			}
		}
	}
	out := make([]string, h)
	for r := range grid {
		out[r] = string(grid[r])
	}
	return out
}

// BuildKrydsordGridFromSlotAnswers places per-slot answers (keyed by slot ID)
// deterministically into a w×h grid using each slot's known cells — so the grid
// dimensions are always correct (the LLM only picks words, never emits the
// grid, which is what caused "row N has 11 columns"). It returns the grid plus
// any crossing conflicts: cells two slots disagree on, fed back to the LLM.
func BuildKrydsordGridFromSlotAnswers(data KrydsordData, slots []KrydsordSlot, answersByID map[string]string) (grid []string, conflicts []string) {
	w, h := data.CellCountX, data.CellCountY
	cells := make([][]rune, h)
	for r := range cells {
		cells[r] = make([]rune, w)
		for c := range cells[r] {
			cells[r][c] = '.'
		}
	}
	// Track which slot last wrote each cell so we can report disagreements.
	owner := map[[2]int]string{}
	for _, s := range slots {
		a := []rune(NormalizeDanishLetters(answersByID[s.ID]))
		if len(a) != s.Length {
			continue
		}
		for k, cell := range s.Cells {
			if cell.Row < 1 || cell.Row > h || cell.Col < 1 || cell.Col > w {
				continue
			}
			cur := cells[cell.Row-1][cell.Col-1]
			if cur != '.' && cur != a[k] {
				conflicts = append(conflicts, fmt.Sprintf("R%dC%d: %s wants %c but %s set %c",
					cell.Row, cell.Col, s.ID, a[k], owner[[2]int{cell.Row, cell.Col}], cur))
				continue
			}
			cells[cell.Row-1][cell.Col-1] = a[k]
			owner[[2]int{cell.Row, cell.Col}] = s.ID
		}
	}
	grid = make([]string, h)
	for r := range cells {
		grid[r] = string(cells[r])
	}
	return grid, conflicts
}

// CrossingCount returns the number of cells shared by two or more entries.
func (csp KrydsordCSP) CrossingCount() int {
	n := 0
	for _, members := range csp.Cells {
		if len(members) >= 2 {
			n++
		}
	}
	return n
}

// KrydsordAnswer is one solved entry from the model's JSON.
type KrydsordAnswer struct {
	ID     string `json:"id"`
	Clue   string `json:"clue"`
	Answer string `json:"answer"`
}

// ParseKrydsordAnswers extracts answer objects from the model's JSON, tolerating
// a truncated array: it scans the "answers" array and unmarshals each balanced
// {…} object individually, so a cut-off tail loses only the missing entries
// rather than the whole response. (Answers are uppercase letters, so braces only
// ever appear as object delimiters here.)
func ParseKrydsordAnswers(clean string) []KrydsordAnswer {
	i := strings.Index(clean, `"answers"`)
	if i < 0 {
		return nil
	}
	s := clean[i:]
	if j := strings.IndexByte(s, '['); j >= 0 {
		s = s[j+1:]
	}
	var out []KrydsordAnswer
	depth, start := 0, -1
	for k := 0; k < len(s); k++ {
		switch s[k] {
		case '{':
			if depth == 0 {
				start = k
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					var a KrydsordAnswer
					if json.Unmarshal([]byte(s[start:k+1]), &a) == nil && a.ID != "" {
						out = append(out, a)
					}
					start = -1
				}
			}
		case ']':
			if depth == 0 {
				return out // array closed cleanly
			}
		}
	}
	return out // truncated mid-array — return what we salvaged
}

// ValidateKrydsordSolution checks a solution against the CSP and returns a list
// of problems: entries with no answer, answers of the wrong length, and shared
// cells whose letters disagree across the entries that cross there.
func ValidateKrydsordSolution(csp KrydsordCSP, answers map[string]string) []string {
	var issues []string
	var ids []string
	for id := range csp.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := csp.Entries[id]
		a := answers[id]
		if a == "" {
			issues = append(issues, fmt.Sprintf("%s (%q): intet svar", id, e.Clue))
			continue
		}
		if n := len([]rune(a)); n != e.Length {
			issues = append(issues, fmt.Sprintf("%s (%q): %q har %d bogstaver, forventet %d", id, e.Clue, a, n, e.Length))
		}
	}
	// Crossing consistency: place each correct-length answer into its cells and
	// flag any cell that ends up with more than one distinct letter.
	cellLetters := map[string]map[rune][]string{}
	for id, e := range csp.Entries {
		a := []rune(answers[id])
		if len(a) != e.Length {
			continue // skip wrong-length answers (already reported)
		}
		for k, cid := range e.Cells {
			if cellLetters[cid] == nil {
				cellLetters[cid] = map[rune][]string{}
			}
			cellLetters[cid][a[k]] = append(cellLetters[cid][a[k]], fmt.Sprintf("%s:%d", id, k+1))
		}
	}
	var conflictCells []string
	for cid, byLetter := range cellLetters {
		if len(byLetter) > 1 {
			conflictCells = append(conflictCells, cid)
		}
	}
	sort.Strings(conflictCells)
	for _, cid := range conflictCells {
		var parts []string
		for r, members := range cellLetters[cid] {
			parts = append(parts, fmt.Sprintf("%c(%s)", r, strings.Join(members, ",")))
		}
		sort.Strings(parts)
		issues = append(issues, fmt.Sprintf("krydsningskonflikt %s: %s", cid, strings.Join(parts, " ≠ ")))
	}
	return issues
}

// KrydsordMatchesPattern reports whether word matches pat, where '.' in pat is a
// wildcard. Lengths must be equal.
func KrydsordMatchesPattern(word, pat string) bool {
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

// KrydsordConflictSlots returns the slots involved in any crossing disagreement
// (for a fully-filled answer set) and, for each, the pattern its CROSSINGS demand
// ('.' where unconstrained or where the crossings themselves disagree). A slot that
// disagrees with several crossings (like an outvoted 2-letter across) shows up with
// a mostly- or fully-determined pattern — exactly the hint needed to refit it.
func KrydsordConflictSlots(slots []KrydsordSlot, answers map[string]string) (involved []string, patternByID map[string]string) {
	type ref struct {
		id  string
		pos int
	}
	cellRefs := map[[2]int][]ref{}
	byID := map[string]KrydsordSlot{}
	for _, s := range slots {
		byID[s.ID] = s
		for k, cell := range s.Cells {
			key := [2]int{cell.Row, cell.Col}
			cellRefs[key] = append(cellRefs[key], ref{s.ID, k})
		}
	}
	letterAt := func(id string, pos int) (rune, bool) {
		a := []rune(NormalizeDanishLetters(answers[id]))
		if len(a) != byID[id].Length || pos < 0 || pos >= len(a) {
			return 0, false
		}
		return a[pos], true
	}
	inv := map[string]bool{}
	for _, refs := range cellRefs {
		letters := map[string]rune{}
		for _, r := range refs {
			if ch, ok := letterAt(r.id, r.pos); ok {
				letters[r.id] = ch
			}
		}
		distinct := map[rune]bool{}
		for _, ch := range letters {
			distinct[ch] = true
		}
		if len(distinct) > 1 {
			for id := range letters {
				inv[id] = true
			}
		}
	}
	patternByID = map[string]string{}
	for id := range inv {
		s := byID[id]
		pat := make([]rune, s.Length)
		for k, cell := range s.Cells {
			pat[k] = '.'
			var want rune
			ok, bad := false, false
			for _, r := range cellRefs[[2]int{cell.Row, cell.Col}] {
				if r.id == id {
					continue
				}
				if ch, has := letterAt(r.id, r.pos); has {
					if !ok {
						want, ok = ch, true
					} else if want != ch {
						bad = true
					}
				}
			}
			if ok && !bad {
				pat[k] = want
			}
		}
		patternByID[id] = string(pat)
		involved = append(involved, id)
	}
	sort.Strings(involved)
	return involved, patternByID
}
