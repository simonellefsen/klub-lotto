//go:build ignore

package klublotto

import (
	"reflect"
	"testing"
)

func TestOrdknudeFallbackMatchesObservedSalenState(t *testing.T) {
	history := []OrdknudeGuess{{
		Word:  "SALEN",
		Marks: []string{"correct", "correct", "correct", "absent", "absent"},
	}}

	got := fallbackOrdknudeGuess(history, nil)
	if got != "SALAT" {
		t.Fatalf("fallbackOrdknudeGuess() = %q, want SALAT", got)
	}
}

func TestOrdknudeFallbackAfterPersistedRows(t *testing.T) {
	history := []OrdknudeGuess{
		{Word: "SALEN", Marks: []string{"correct", "correct", "correct", "absent", "absent"}},
		{Word: "SALAT", Marks: []string{"correct", "correct", "correct", "absent", "absent"}},
		{Word: "SALDO", Marks: []string{"correct", "correct", "correct", "absent", "absent"}},
		{Word: "SALIG", Marks: []string{"correct", "correct", "correct", "absent", "absent"}},
	}

	got := fallbackOrdknudeGuess(history, nil)
	if got != "SALÆR" {
		t.Fatalf("fallbackOrdknudeGuess() = %q, want SALÆR", got)
	}
}

func TestOrdknudeFallbackHasOpeningGuess(t *testing.T) {
	if got := fallbackOrdknudeGuess(nil, nil); got == "" {
		t.Fatal("fallbackOrdknudeGuess() returned empty opening guess")
	}
}

func TestScoreOrdknudeGuessUsesDuplicateAwareWordleRules(t *testing.T) {
	got := scoreOrdknudeGuess("SALAT", "SALSA")
	want := []string{"correct", "correct", "correct", "absent", "present"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scoreOrdknudeGuess() = %#v, want %#v", got, want)
	}
}

func TestClassifyOrdknudeTileByBackground(t *testing.T) {
	tests := []struct {
		name string
		tile OrdknudeTile
		want string
	}{
		{name: "green", tile: OrdknudeTile{Letter: "S", Background: "rgb(1, 158, 1)"}, want: "correct"},
		{name: "red", tile: OrdknudeTile{Letter: "E", Background: "rgb(136, 0, 3)"}, want: "absent"},
		{name: "orange", tile: OrdknudeTile{Letter: "A", Background: "rgb(251, 176, 42)"}, want: "present"},
		{name: "white", tile: OrdknudeTile{Background: "rgb(255, 255, 255)"}, want: "pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyOrdknudeTile(tt.tile); got != tt.want {
				t.Fatalf("classifyOrdknudeTile() = %q, want %q", got, tt.want)
			}
		})
	}
}
