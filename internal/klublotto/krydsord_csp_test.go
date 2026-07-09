package klublotto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// smallGraph: A1 across at (1,1) len 3, D1 down at (1,2) len 2 — they cross at
// r1c2 (A1 position 2 / D1 position 1).
func smallGraph() KrydsordGraph {
	return KrydsordGraph{
		Across: []KrydsordGraphClue{{Clue: "across", Direction: "across", Start: KrydsordStart{Row: 1, Col: 1}, Length: 3}},
		Down:   []KrydsordGraphClue{{Clue: "down", Direction: "down", Start: KrydsordStart{Row: 1, Col: 2}, Length: 2}},
	}
}

func TestBuildKrydsordCSPAndCrossings(t *testing.T) {
	csp := BuildKrydsordCSP(smallGraph())

	if got := csp.Entries["A1"].Cells; !reflect.DeepEqual(got, []string{"r1c1", "r1c2", "r1c3"}) {
		t.Fatalf("A1 cells = %v", got)
	}
	if got := csp.Entries["D1"].Cells; !reflect.DeepEqual(got, []string{"r1c2", "r2c2"}) {
		t.Fatalf("D1 cells = %v", got)
	}
	// r1c2 is the only shared cell.
	if got := csp.Cells["r1c2"]; len(got) != 2 {
		t.Fatalf("r1c2 members = %v, want 2", got)
	}
	if n := csp.CrossingCount(); n != 1 {
		t.Fatalf("CrossingCount() = %d, want 1", n)
	}
}

func TestValidateKrydsordSolution(t *testing.T) {
	csp := BuildKrydsordCSP(smallGraph())

	// Consistent: A1="KAT", D1="AB" — both share 'A' at r1c2.
	if issues := ValidateKrydsordSolution(csp, map[string]string{"A1": "KAT", "D1": "AB"}); len(issues) != 0 {
		t.Fatalf("expected no issues for a consistent solution, got %v", issues)
	}
	// Crossing conflict: A1 wants 'A' at r1c2, D1 wants 'X'.
	issues := ValidateKrydsordSolution(csp, map[string]string{"A1": "KAT", "D1": "XB"})
	if len(issues) == 0 || !strings.Contains(strings.Join(issues, "\n"), "krydsningskonflikt r1c2") {
		t.Fatalf("expected a crossing conflict at r1c2, got %v", issues)
	}
	// Missing + wrong-length answers are reported.
	issues = ValidateKrydsordSolution(csp, map[string]string{"A1": "", "D1": "ABC"})
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "A1") || !strings.Contains(joined, "intet svar") {
		t.Fatalf("expected 'intet svar' for A1, got %v", issues)
	}
	if !strings.Contains(joined, "D1") || !strings.Contains(joined, "forventet 2") {
		t.Fatalf("expected wrong-length report for D1, got %v", issues)
	}
}

func TestParseKrydsordAnswers(t *testing.T) {
	clean := `{"answers":[{"id":"A1","clue":"x","answer":"KAT"},{"id":"D1","answer":"AB"}]}`
	got := ParseKrydsordAnswers(clean)
	if len(got) != 2 || got[0].ID != "A1" || got[0].Answer != "KAT" || got[1].ID != "D1" {
		t.Fatalf("parsed = %+v", got)
	}
	// A truncated array (reasoning model cut off) must salvage the complete objects.
	trunc := `{"answers":[{"id":"A1","answer":"KAT"},{"id":"D1","answ`
	got = ParseKrydsordAnswers(trunc)
	if len(got) != 1 || got[0].ID != "A1" {
		t.Fatalf("truncated parse = %+v, want 1 salvaged (A1)", got)
	}
}

func TestKrydsordMatchesPattern(t *testing.T) {
	cases := []struct {
		word, pat string
		want      bool
	}{
		{"KAT", "K.T", true},
		{"KAT", "...", true},
		{"KAT", "K.X", false},
		{"KAT", "KA", false}, // length mismatch
	}
	for _, c := range cases {
		if got := KrydsordMatchesPattern(c.word, c.pat); got != c.want {
			t.Errorf("KrydsordMatchesPattern(%q,%q) = %v, want %v", c.word, c.pat, got, c.want)
		}
	}
}

func TestBuildKrydsordGridFromAnswers(t *testing.T) {
	csp := BuildKrydsordCSP(smallGraph())
	grid := BuildKrydsordGridFromAnswers(csp, map[string]string{"A1": "KAT", "D1": "AB"}, 3, 2)
	want := []string{"KAT", ".B."}
	if !reflect.DeepEqual(grid, want) {
		t.Fatalf("grid = %v, want %v", grid, want)
	}
}

func TestKrydsordStartUnmarshalBothForms(t *testing.T) {
	var obj KrydsordStart
	if err := json.Unmarshal([]byte(`{"row":2,"column":3}`), &obj); err != nil || obj.Row != 2 || obj.Col != 3 {
		t.Fatalf("object form: %+v err=%v", obj, err)
	}
	var arr KrydsordStart
	if err := json.Unmarshal([]byte(`[2,3]`), &arr); err != nil || arr.Row != 2 || arr.Col != 3 {
		t.Fatalf("legacy array form: %+v err=%v", arr, err)
	}
}

func TestKrydsordConflictSlotsExcludesDisputedLetters(t *testing.T) {
	// A1 (r2c2-c4, "ABC") crosses D1 (r2c3-r3c3, "XY") — conflict at r2c3 (B vs X)
	// — and D2 (r2c4-r3c4, "CD"), which AGREES at r2c4 (C) and is thus trusted.
	// The repair pattern for A1 must keep the trusted 'C' but must NOT bake in the
	// disputed D1 letter: telling A1 to preserve the very letter under dispute is
	// how MATEMATIK got "repaired" into the non-word MATEMDTIK on 2026-07-09.
	mask := strings.Join([]string{
		".....",
		".###.",
		"..##.",
	}, "\n")
	data := KrydsordData{
		SolutionSecret: secretFromKrydsordMask(mask),
		CellCountX:     5,
		CellCountY:     3,
	}
	slots := BuildKrydsordSlots(data)
	idAt := func(dir string, row, col int) string {
		for _, s := range slots {
			if s.Direction == dir && s.Row == row && s.Col == col {
				return s.ID
			}
		}
		t.Fatalf("no %s slot at r%dc%d in %+v", dir, row, col, slots)
		return ""
	}
	a1 := idAt("across", 2, 2)
	d1 := idAt("down", 2, 3)
	d2 := idAt("down", 2, 4)

	answers := map[string]string{a1: "ABC", d1: "XY", d2: "CD"}
	involved, patterns := KrydsordConflictSlots(slots, answers)
	if len(involved) != 2 || involved[0] != a1 && involved[1] != a1 {
		t.Fatalf("involved = %v, want exactly [%s %s]", involved, a1, d1)
	}
	if got := patterns[a1]; got != "..C" {
		t.Fatalf("pattern[%s] = %q, want \"..C\" (trusted D2 letter kept, disputed D1 letter excluded)", a1, got)
	}
	if got := patterns[d1]; got != ".." {
		t.Fatalf("pattern[%s] = %q, want \"..\" (disputed A1 letter excluded)", d1, got)
	}
}
