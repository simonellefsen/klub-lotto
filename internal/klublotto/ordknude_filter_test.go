package klublotto

import (
	"reflect"
	"testing"
)

func TestAlreadyTriedOrdknude(t *testing.T) {
	hist := []OrdknudeGuess{{Word: "SNUDE"}, {Word: "sprog"}}
	if !AlreadyTriedOrdknude("snude", hist) { // case-insensitive
		t.Fatal("SNUDE should be reported as already tried")
	}
	if !AlreadyTriedOrdknude("SPROG", hist) {
		t.Fatal("SPROG should be reported as already tried")
	}
	if AlreadyTriedOrdknude("SNYDE", hist) {
		t.Fatal("SNYDE has not been tried")
	}
}

func TestFilterOrdknudeCandidates(t *testing.T) {
	st := OrdknudeState{History: []OrdknudeGuess{{Word: "SNUDE"}}}
	rejected := []string{"REJEK"}
	cands := []WordCandidate{
		{Answer: "SPROG"}, // valid
		{Answer: "sprog"}, // duplicate of SPROG (normalised)
		{Answer: "ABC"},   // not 5 letters
		{Answer: "SNUDE"}, // already tried
		{Answer: "REJEK"}, // rejected
		{Answer: "SNYDE"}, // valid
	}
	got := FilterOrdknudeCandidates(cands, st, rejected)
	var words []string
	for _, c := range got {
		words = append(words, c.Answer)
	}
	if !reflect.DeepEqual(words, []string{"SPROG", "SNYDE"}) {
		t.Fatalf("filtered = %v, want [SPROG SNYDE]", words)
	}
}
