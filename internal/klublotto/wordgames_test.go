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

func TestIsOrdKloeverWinText(t *testing.T) {
	// The exact win banner from the parent body (see the live "Flot præstation!"
	// screen). Must read as a win.
	win := "Ordkløver\nFlot præstation!\nDu løste ordkløver med stil!\n(Du har allerede optjent dagens lod)"
	if !IsOrdKloeverWinText(win) {
		t.Fatal("IsOrdKloeverWinText() = false for the real Ordkløver win banner")
	}
	// A fresh launcher / welcome screen is NOT a win.
	if IsOrdKloeverWinText("Velkommen\nKan du gætte dagens gåde?\nSPIL ORDKLØVER") {
		t.Fatal("IsOrdKloeverWinText() = true for the welcome/launcher screen")
	}
}

func TestIsOrdknudeWinText(t *testing.T) {
	// The exact win banner from the parent body innerText (ordknude-state.txt).
	win := "Ordknuden\nSuper imponerende!\nDu fandt frem til dagens ord. Du er en sand ord-haj!"
	if !IsOrdknudeWinText(win) {
		t.Fatal("IsOrdknudeWinText() = false for the real Ordknuden win banner")
	}
	if !IsOrdknudeWinText("Tillykke! Du vandt.") {
		t.Fatal("IsOrdknudeWinText() = false for the tillykke variant")
	}
	// Loss screen must NOT read as a win.
	if IsOrdknudeWinText("Lige ved og næsten! Det rigtige svar var: GUMMI") {
		t.Fatal("IsOrdknudeWinText() = true for a loss screen")
	}
	// Account-nav copy that permanently lives on the page must not false-positive.
	if IsOrdknudeWinText("Se om du har vundet eller tabt — Dagens første lod kan være din.") {
		t.Fatal("IsOrdknudeWinText() = true for permanent account-nav / lod copy")
	}
}

func TestIsDanskeSpilErrorScreen(t *testing.T) {
	// The exact crash page danskespil showed after an Ordkløver submit.
	errPage := "Ordkløver\nDer skete en fejl. Prøv igen. Hvis fejlen fortsætter bedes " +
		"du kontakte vores Kundecenter på tlf. 3672 8080.\nPRØV IGEN\nFORSIDE"
	if !IsDanskeSpilErrorScreen(errPage) {
		t.Fatal("IsDanskeSpilErrorScreen() = false for the danskespil crash page")
	}
	// "Prøv igen" + "Kundecenter" without the headline still counts.
	if !IsDanskeSpilErrorScreen("noget gik galt — prøv igen, ellers kontakt vores Kundecenter") {
		t.Fatal("IsDanskeSpilErrorScreen() = false for the prøv-igen + Kundecenter variant")
	}
	// A normal win screen must NOT be mistaken for an error.
	if IsDanskeSpilErrorScreen("Flot præstation! Du har knækket koden. Dagens lod er din.") {
		t.Fatal("IsDanskeSpilErrorScreen() = true for a win screen")
	}
	// A stray "prøv igen" in normal copy (no Kundecenter, no headline) must not trip it.
	if IsDanskeSpilErrorScreen("Forkert gæt — prøv igen med et nyt ord.") {
		t.Fatal("IsDanskeSpilErrorScreen() = true for a benign 'prøv igen' hint")
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
