package klublotto

import "testing"

func TestColourCodeOrdKloeverLetters(t *testing.T) {
	// Reveal "SILKEBORG"; probe S(hit), X(miss), I(hit), S again (de-duped).
	got := ColourCodeOrdKloeverLetters([]string{"S", "X", "I", "s"}, "SILKEBORG")
	if got != "S🟩 X🟥 I🟩" {
		t.Fatalf("colour-coded = %q", got)
	}
}

func TestOrdKloeverNotes(t *testing.T) {
	got := OrdKloeverNotes("9+3", "SILKEBORG BAD", []string{"S", "X"}, "Løst")
	want := "Bogstavgæt: S🟩 X🟥 · Mønster: 9+3 · Løst"
	if got != want {
		t.Fatalf("notes = %q\nwant   %q", got, want)
	}
	// Empty inputs collapse cleanly.
	if OrdKloeverNotes("", "", nil, "") != "" {
		t.Fatal("all-empty notes should be empty")
	}
}

func TestOrdKloeverPrompt(t *testing.T) {
	st := OrdKloeverState{Category: "Person", Hint: "meteorolog", Shape: "6+8"}
	got := OrdKloeverPrompt(st)
	want := "Category: `Person`; hint: `meteorolog`; answer pattern `6+8`"
	if got != want {
		t.Fatalf("prompt = %q\nwant   %q", got, want)
	}
}
