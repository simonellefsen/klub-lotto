package klublotto

import (
	"reflect"
	"strings"
	"testing"
)

func TestOrdknudeWordList(t *testing.T) {
	if n := len(ordknudeAllWords()); n != 8506 {
		t.Fatalf("%d words, want the 8506 five-letter words of the 2026-11-18 list", n)
	}
	// Answers and accepted guesses from the ledger are in it, lowercase too.
	for _, w := range []string{"GRISK", "FRISK", "KOGLE", "SENAT", "BORDE", "DRAMA", "krans", "ØRKEN"} {
		if !IsOrdknudeWord(w) {
			t.Errorf("%s should be a word", w)
		}
	}
	// FOMLE was turned down on 2026-11-20 with "Ordet findes ikke i vores ordbog".
	for _, w := range []string{"FOMLE", "BRURS", "HÆKEL", "KRAN", "KRANSE"} {
		if IsOrdknudeWord(w) {
			t.Errorf("%s should not be a word", w)
		}
	}
	got := FilterOrdknudeWords([]WordCandidate{{Answer: "FOMLE"}, {Answer: "kogle"}})
	if len(got) != 1 || got[0].Answer != "kogle" {
		t.Fatalf("FilterOrdknudeWords kept %v", got)
	}
}

func TestFallbackOrdknudeGuessUsesWordList(t *testing.T) {
	// The 2026-11-20 board when FOMLE was tried (the answer was KOGLE).
	history := []OrdknudeGuess{
		{Word: "SENAT", Marks: []string{"absent", "present", "absent", "absent", "absent"}},
		{Word: "BORDE", Marks: []string{"absent", "correct", "absent", "absent", "correct"}},
	}
	got := FallbackOrdknudeGuess(history, []string{"FOMLE"})
	if !IsOrdknudeWord(got) || !ConsistentWithOrdknudeHistory(got, history) {
		t.Fatalf("fallback %q is not a word that fits the board", got)
	}
	if !strings.HasSuffix(got, "E") || []rune(got)[1] != 'O' {
		t.Fatalf("fallback %q ignores the greens _O__E", got)
	}
}

func TestDropTypedOrdknudeRow(t *testing.T) {
	typed := []string{"pending", "pending", "pending", "pending", "pending"}
	words := []string{"SENAT", "BORDE", "FOMLE"}
	marks := [][]string{
		{"absent", "present", "absent", "absent", "absent"},
		{"absent", "correct", "absent", "absent", "correct"},
		typed,
	}
	w, m := dropTypedOrdknudeRow(words, marks)
	if !reflect.DeepEqual(w, words[:2]) || len(m) != 2 {
		t.Fatalf("got %v %v, want FOMLE dropped", w, m)
	}
	// Only an all-uncoloured last row is a typed word.
	marks[2] = []string{"absent", "correct", "absent", "absent", "pending"}
	if w, _ := dropTypedOrdknudeRow(words, marks); len(w) != 3 {
		t.Fatalf("a partly coloured row was dropped: %v", w)
	}
}
