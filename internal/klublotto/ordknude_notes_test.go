package klublotto

import (
	"reflect"
	"testing"
)

func TestScoreOrdknudeGuess(t *testing.T) {
	// Straightforward: one exact, one present, three absent.
	// answer SPROG, guess SPILD: S,P exact; (I,L,D absent); no duplicates.
	if got := ScoreOrdknudeGuess("SPILD", "SPROG"); !reflect.DeepEqual(got,
		[]string{"correct", "correct", "absent", "absent", "absent"}) {
		t.Fatalf("SPILD vs SPROG = %v", got)
	}
	// Duplicate-letter handling: answer has one L. Guess LLAMA-style "LLAMA"
	// isn't 5 Danish letters here; use answer "KLODS", guess "LLLLL": only the
	// real L position (index 1) is present-or-correct ONCE, the rest absent.
	// answer KLODS has L at index 1. guess "LILLE": L(0) present (consumes the
	// one L), I absent, L(2) absent (no L left), L(3) absent, E absent.
	got := ScoreOrdknudeGuess("LILLE", "KLODS")
	want := []string{"present", "absent", "absent", "absent", "absent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LILLE vs KLODS = %v, want %v (one L consumed only)", got, want)
	}
	// Exact win.
	if got := ScoreOrdknudeGuess("SPROG", "SPROG"); !reflect.DeepEqual(got,
		[]string{"correct", "correct", "correct", "correct", "correct"}) {
		t.Fatalf("exact = %v", got)
	}
	// Wrong length → nil.
	if ScoreOrdknudeGuess("ABC", "SPROG") != nil {
		t.Fatal("non-5-letter guess should score nil")
	}
}

func TestOrdknudeMarkSquares(t *testing.T) {
	got := OrdknudeMarkSquares([]string{"correct", "present", "absent", "correct", "absent"})
	if got != "🟩🟨🟥🟩🟥" {
		t.Fatalf("squares = %q", got)
	}
}

func TestMergeGuessWords(t *testing.T) {
	hist := []OrdknudeGuess{{Word: "sprog"}, {Word: "snude"}}
	// "SNUDE" already in history (case-insensitive); "SNYDE" is new this run.
	got := MergeGuessWords(hist, []string{"snude", "SNYDE"})
	if !reflect.DeepEqual(got, []string{"SPROG", "SNUDE", "SNYDE"}) {
		t.Fatalf("merged = %v", got)
	}
}

func TestOrdknudeGuessNotes(t *testing.T) {
	hist := []OrdknudeGuess{{Word: "SPROG", Marks: []string{"correct", "correct", "absent", "absent", "absent"}}}
	// SPROG from history (real marks), SNYDE scored against the answer SNYDE (win).
	notes := OrdknudeGuessNotes([]string{"SPROG", "SNYDE"}, hist, "SNYDE")
	want := "Gæt: 1. SPROG 🟩🟩🟥🟥🟥 · 2. SNYDE 🟩🟩🟩🟩🟩"
	if notes != want {
		t.Fatalf("notes = %q\nwant   %q", notes, want)
	}
	if OrdknudeGuessNotes(nil, nil, "") != "" {
		t.Fatal("empty tried should yield empty notes")
	}
}
