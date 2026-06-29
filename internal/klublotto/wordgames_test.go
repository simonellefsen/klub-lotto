package klublotto

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNormalizeDanishPhraseKeepsDanishLetters(t *testing.T) {
	got := NormalizeDanishPhrase("  salær / blå-øl \n æøå  ")
	want := "SALÆR BLÅ ØL ÆØÅ"
	if got != want {
		t.Fatalf("NormalizeDanishPhrase() = %q, want %q", got, want)
	}
}

func TestFindLossScreenAnswer(t *testing.T) {
	// Loss screen reveals the answer after the explicit marker.
	loss := "Lige ved og næsten! Det rigtige svar var: GUMMI. Prøv igen i morgen."
	if got := findLossScreenAnswer(loss); got != "GUMMI" {
		t.Fatalf("findLossScreenAnswer(loss) = %q, want GUMMI", got)
	}
	// Win banner never carries the answer — must return "" (not a junk 5-letter
	// run scraped from chrome text like "...imponerende...").
	win := "Super imponerende! Du fandt frem til dagens ord. Du er en sand ord-haj!"
	if got := findLossScreenAnswer(win); got != "" {
		t.Fatalf("findLossScreenAnswer(win) = %q, want empty", got)
	}

	// Real loss screen body text (æ word), via the exported wrapper. The answer
	// is on the line after the marker and must normalise to upper-case Danish.
	body := "Du kæmpede bravt\nDet rigtige svar var:\nænder\nOp med humøret, det var et hæderligt forsøg."
	if got := OrdknudeLossAnswer(body); got != "ÆNDER" {
		t.Fatalf("OrdknudeLossAnswer(loss body) = %q, want ÆNDER", got)
	}
}

func TestIsDanishFiveLetterWord(t *testing.T) {
	for _, word := range []string{"SALÆR", "BLÅÅL", "ABCDE"} {
		if !IsDanishFiveLetterWord(word) {
			t.Fatalf("IsDanishFiveLetterWord(%q) = false", word)
		}
	}
	for _, word := range []string{"SALÆRS", "SA LÆ"} {
		if IsDanishFiveLetterWord(word) {
			t.Fatalf("IsDanishFiveLetterWord(%q) = true", word)
		}
	}
}

func TestParseCandidateJSONNormalizesAnswers(t *testing.T) {
	cands, err := ParseCandidateJSON(`{"candidates":[{"answer":"roterende-fis/i kasketten","confidence":"high","rationale":"kendt udtryk"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Answer != "ROTERENDE FIS I KASKETTEN" {
		t.Fatalf("candidates = %#v", cands)
	}
}

// Some models (e.g. gpt-5.5) return "candidates" as an array of plain strings
// with confidence/rationale at the top level. ParseCandidateJSON must accept
// that shape rather than failing to unmarshal string-into-struct.
func TestParseCandidateJSONStringArray(t *testing.T) {
	raw := `{"candidates":["BINDE","FINDE","HINDE","KINDE","MINDE","PINDE"],"confidence":"high","rationale":"matcher _INDE"}`
	cands, err := ParseCandidateJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 6 || cands[0].Answer != "BINDE" || cands[0].Confidence != "high" {
		t.Fatalf("candidates = %#v", cands)
	}
	// Bare string array, too.
	bare, err := ParseCandidateJSON(`["BINDE","FINDE"]`)
	if err != nil || len(bare) != 2 || bare[1].Answer != "FINDE" {
		t.Fatalf("bare string array: %#v err=%v", bare, err)
	}
}

func TestConsistentWithOrdknudeHistory(t *testing.T) {
	// Board after TRANE (..present,correct) and VINDE (absent,correct×4): _INDE
	// with I,N,D,E locked and A,R,T,V gray.
	history := []OrdknudeGuess{
		{Word: "TRANE", Marks: []string{"absent", "absent", "absent", "present", "correct"}},
		{Word: "VINDE", Marks: []string{"absent", "correct", "correct", "correct", "correct"}},
	}
	for _, w := range []string{"BINDE", "FINDE", "HINDE", "KINDE", "MINDE", "PINDE", "LINDE"} {
		if !ConsistentWithOrdknudeHistory(w, history) {
			t.Errorf("%s should be consistent with _INDE constraints", w)
		}
	}
	for _, w := range []string{"TINDE", "RINDE", "VINDE"} { // contain a gray letter (or V proven absent)
		if ConsistentWithOrdknudeHistory(w, history) {
			t.Errorf("%s should be rejected by the constraints", w)
		}
	}
}

func TestPhraseMatchesLengthPattern(t *testing.T) {
	if !PhraseMatchesLengthPattern("DET ER EN SANGBOG", "3 / 2 / 2 / 7") {
		t.Fatal("expected phrase to match Danish word-length pattern")
	}
	if PhraseMatchesLengthPattern("TONE OG EN AKKORD", "4 / 2 / 2 / 7") {
		t.Fatal("expected six-letter AKKORD to be rejected for final seven-letter slot")
	}
}

func TestFilterCandidatesByLengthPattern(t *testing.T) {
	cands := []WordCandidate{
		{Answer: "TONE OG EN AKKORD"},
		{Answer: "TONE OG EN SANGBOG"},
	}
	got, dropped := FilterCandidatesByLengthPattern(cands, "4 / 2 / 2 / 7")
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(got) != 1 || got[0].Answer != "TONE OG EN SANGBOG" {
		t.Fatalf("filtered candidates = %#v", got)
	}
}

func TestPhraseMatchesMask(t *testing.T) {
	if !PhraseMatchesMask("TONE OM EN KONCERT", "T___ / __ / __ / K______") {
		t.Fatal("expected phrase to match revealed Ordkløver mask")
	}
	if PhraseMatchesMask("TONE OM EN SANGBOG", "T___ / __ / __ / K______") {
		t.Fatal("expected final word to be rejected by revealed K")
	}
}

func TestFilterCandidatesByMask(t *testing.T) {
	cands := []WordCandidate{
		{Answer: "TONE OM EN KONCERT"},
		{Answer: "TONE OM EN SANGBOG"},
	}
	got, dropped := FilterCandidatesByMask(cands, "T___ / __ / __ / K______")
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(got) != 1 || got[0].Answer != "TONE OM EN KONCERT" {
		t.Fatalf("filtered candidates = %#v", got)
	}
}

func TestSuggestOrdKloeverLetters(t *testing.T) {
	cands := []WordCandidate{
		{Answer: "TONE OM EN KONCERT", Confidence: "high"},
		{Answer: "TONE OM EN KAPELLE", Confidence: "medium"},
	}
	got := SuggestOrdKloeverLetters(cands, "TONE", 3)
	if len(got) != 3 {
		t.Fatalf("got %d letters, want 3: %#v", len(got), got)
	}
	for _, letter := range got {
		if letter == "T" || letter == "O" || letter == "N" || letter == "E" {
			t.Fatalf("suggested already-known letter %s in %#v", letter, got)
		}
	}
}

func TestRejectedWordsAreDeduplicated(t *testing.T) {
	dir := t.TempDir()
	if err := RecordRejectedWord(dir, "salær"); err != nil {
		t.Fatal(err)
	}
	if err := RecordRejectedWord(dir, "SALÆR"); err != nil {
		t.Fatal(err)
	}
	got := LoadRejectedWords(dir)
	want := []string{"SALÆR"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadRejectedWords() = %#v, want %#v", got, want)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ordknude-rejected.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "SALÆR\n" {
		t.Fatalf("rejected file = %q", raw)
	}
}

func TestClassifyOrdknudeTile(t *testing.T) {
	tests := []struct {
		name string
		tile OrdknudeTile
		want string
	}{
		{name: "green class", tile: OrdknudeTile{ClassName: "tile correct"}, want: "correct"},
		{name: "yellow class", tile: OrdknudeTile{ClassName: "tile present"}, want: "present"},
		{name: "red rgb", tile: OrdknudeTile{Background: "rgb(130, 20, 10)"}, want: "absent"},
		// Real board colours (CSS-module hashed classes, so only RGB matters).
		// Regression for the green-D-in-GRØDE misread: rgb(1,158,1) must be correct.
		{name: "board green rgb", tile: OrdknudeTile{ClassName: "_tile_x", Background: "rgb(1, 158, 1)"}, want: "correct"},
		{name: "board absent rgb", tile: OrdknudeTile{ClassName: "_tile_x", Background: "rgb(136, 0, 3)"}, want: "absent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyOrdknudeTile(tt.tile); got != tt.want {
				t.Fatalf("classifyOrdknudeTile() = %q, want %q", got, tt.want)
			}
		})
	}
}
