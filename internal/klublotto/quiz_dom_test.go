package klublotto

import (
	"reflect"
	"testing"
)

// The 2026-11-23 snapshot: a 106-character question, cut by agent-browser at
// 100 characters before its "?".
const quizSnapLongQuestion = `- link "Danske Spil" [ref=e89]
- radio "Hvilket oldtidsfolk var ophavsmænd til det alfabet, som det græske og det latinske alfabet nedstamme" [checked=false, ref=e1] clickable [onclick]
  - LabelText "Sumererne" [ref=e57] clickable [cursor:pointer]
  - LabelText "Fønikerne" [ref=e58] clickable [cursor:pointer]
  - LabelText "Grækerne" [ref=e59] clickable [cursor:pointer]
  - button "AFGIV SVAR" [ref=e52]
- generic [ref=e18] clickable [onclick]
- link "Driftsstatus" [ref=e19]`

func TestExtractRoundCutOffQuestion(t *testing.T) {
	round, err := ExtractRound(quizSnapLongQuestion)
	if err != nil {
		t.Fatalf("ExtractRound() error = %v", err)
	}
	cut := "Hvilket oldtidsfolk var ophavsmænd til det alfabet, som det græske og det latinske alfabet nedstamme"
	if round.Prompt != cut {
		t.Fatalf("Prompt = %q, want %q", round.Prompt, cut)
	}
	if want := []string{"Sumererne", "Fønikerne", "Grækerne"}; !reflect.DeepEqual(round.Options, want) {
		t.Fatalf("Options = %q, want %q", round.Options, want)
	}
	if want := []string{"@e57", "@e58", "@e59"}; !reflect.DeepEqual(round.OptionRefs, want) {
		t.Fatalf("OptionRefs = %q, want %q", round.OptionRefs, want)
	}

	full := "Hvilket oldtidsfolk var ophavsmænd til det alfabet, som det græske og det latinske alfabet nedstammer fra?"
	if got := completeQuizPrompt(round.Prompt, full); got != full {
		t.Fatalf("completeQuizPrompt() = %q, want the full question", got)
	}
}

func TestExtractRoundIgnoresRadioWithoutOptions(t *testing.T) {
	snap := `- radio "Modtag nyhedsbrev" [checked=false, ref=e3]
- link "Driftsstatus" [ref=e19]`
	if _, err := ExtractRound(snap); err == nil {
		t.Fatal("ExtractRound() took a lone radio for the quiz")
	}
}

func TestCompleteQuizPrompt(t *testing.T) {
	full := "Hvilket år blev Danmark medlem af EF?"
	for _, tc := range []struct{ prompt, want string }{
		{"Hvilket år blev Danmark", full}, // cut off → completed
		{full, full},                      // already whole
		{"Hvilken farve har himlen?", "Hvilken farve har himlen?"}, // a different question stays
		{"", ""},
	} {
		if got := completeQuizPrompt(tc.prompt, full); got != tc.want {
			t.Errorf("completeQuizPrompt(%q) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
}

func TestQuizOptionIndex(t *testing.T) {
	opts := []string{"Sumererne", "Fønikerne", "Grækerne"}
	for _, tc := range []struct {
		want string
		idx  int
	}{
		{"Fønikerne", 1},
		{"fønikerne", 1},
		{"B) Fønikerne", 1},
		{"Fenikerne", 1}, // the 2026-11-23 screenshot misread
		{"Grækeme", 2},
		{"Romerne", -1},
		{"", -1},
	} {
		if got := quizOptionIndex(opts, tc.want); got != tc.idx {
			t.Errorf("quizOptionIndex(%q) = %d, want %d", tc.want, got, tc.idx)
		}
	}
	// Options a letter apart: a misread one is not guessed between.
	years := []string{"1920'erne", "1930'erne", "1950'erne"}
	if got := quizOptionIndex(years, "1940'erne"); got != -1 {
		t.Errorf("near-twins: quizOptionIndex = %d, want -1", got)
	}
	if got := quizOptionIndex(years, "1930'erne"); got != 1 {
		t.Errorf("exact year: quizOptionIndex = %d, want 1", got)
	}
}
