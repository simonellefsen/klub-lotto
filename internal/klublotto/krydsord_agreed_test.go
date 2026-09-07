package klublotto

import "testing"

// slot builds a horizontal or vertical slot of the given length.
func mkSlot(id string, row, col, length int, down bool) KrydsordSlot {
	s := KrydsordSlot{ID: id, Length: length}
	for i := 0; i < length; i++ {
		r, c := row, col+i
		if down {
			r, c = row+i, col
		}
		s.Cells = append(s.Cells, KrydsordCell{Row: r, Col: c})
	}
	return s
}

// The 2026-09-06 shape: a long across whose first letters are each confirmed by a
// different down, and two wrong downs that contradict it further along. The
// confirmed prefix must survive; only the disputed cells go free.
func TestKrydsordAgreedPatternsKeepsCorroboratedPrefix(t *testing.T) {
	slots := []KrydsordSlot{
		mkSlot("A1", 0, 0, 5, false),
		mkSlot("D1", 0, 0, 2, true), // shares A1[0]
		mkSlot("D2", 0, 1, 2, true), // shares A1[1]
		mkSlot("D8", 0, 3, 2, true), // shares A1[3] — disagrees
	}
	answers := map[string]string{
		"A1": "ØVELS",
		"D1": "ØX",
		"D2": "VY",
		"D8": "QZ", // wants Q at A1[3], A1 says L
	}
	pats := KrydsordAgreedPatterns(slots, answers)

	if got, want := pats["A1"], "ØV.."+"."; got != want {
		t.Errorf("A1 pattern = %q, want %q (Ø and V confirmed, disputed cell free, uncrossed cell free)", got, want)
	}
	// The confirmed prefix must rule out a wholesale replacement.
	if KrydsordMatchesPattern("SPILL", pats["A1"]) {
		t.Error("a replacement contradicting the confirmed Ø/V must not match the pattern")
	}
	if !KrydsordMatchesPattern("ØVERS", pats["A1"]) {
		t.Error("an alternative agreeing with the confirmed letters must still match")
	}
}

func TestKrydsordAgreedPatternsMarksDisputedCellsFree(t *testing.T) {
	slots := []KrydsordSlot{
		mkSlot("A1", 0, 0, 2, false),
		mkSlot("D1", 0, 0, 2, true),
	}
	// The two disagree on the shared cell — neither side may assert it.
	answers := map[string]string{"A1": "AB", "D1": "XY"}
	pats := KrydsordAgreedPatterns(slots, answers)
	if pats["A1"] != ".." {
		t.Errorf("A1 = %q, want %q — a disputed cell is not confirmed", pats["A1"], "..")
	}
	if pats["D1"] != ".." {
		t.Errorf("D1 = %q, want %q", pats["D1"], "..")
	}
}

func TestKrydsordAgreedPatternsSkipsWrongLengthAndScores(t *testing.T) {
	slots := []KrydsordSlot{mkSlot("A1", 0, 0, 4, false), mkSlot("D1", 0, 0, 2, true)}
	// A1's answer is the wrong length — it contributes nothing and gets no pattern.
	pats := KrydsordAgreedPatterns(slots, map[string]string{"A1": "AB", "D1": "AZ"})
	if _, ok := pats["A1"]; ok {
		t.Error("a wrong-length answer must not produce a pattern")
	}
	if got := KrydsordAgreementScore("ØV..."); got != 2 {
		t.Errorf("KrydsordAgreementScore = %d, want 2", got)
	}
	if got := KrydsordAgreementScore("....."); got != 0 {
		t.Errorf("KrydsordAgreementScore(all free) = %d, want 0", got)
	}
}
