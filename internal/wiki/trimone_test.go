package wiki

import (
	"testing"
	"unicode/utf8"
)

// The 2026-10-18 failure: a Danish quiz subject whose 60-byte cut lands inside
// the two-byte "ø" of "rører". Byte slicing left a dangling 0xC3, which made
// wiki/log.md invalid UTF-8 and killed scripts/sync.sh with
// "sed: RE error: illegal byte sequence".
func TestTrimOneNeverSplitsAMultiByteCharacter(t *testing.T) {
	subject := "Hvilken plante er kendt for at lukke sine blade, når man rører ved den?"
	got := trimOne(subject, 60)
	if !utf8.ValidString(got) {
		t.Fatalf("trimOne produced invalid UTF-8: %q (% x)", got, got)
	}
	if n := utf8.RuneCountInString(got); n > 61 { // 60 + the ellipsis
		t.Errorf("trimOne returned %d runes, want <= 61", n)
	}
}

// Every prefix length must stay valid, including each cut that falls inside a
// multi-byte character.
func TestTrimOneValidAtEveryCutPoint(t *testing.T) {
	s := "ø æ å ø æ å ø æ å ø æ å — rører, blødgør, småkager"
	for n := 0; n <= utf8.RuneCountInString(s)+2; n++ {
		got := trimOne(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("n=%d produced invalid UTF-8: % x", n, got)
		}
	}
}

func TestTrimOneCountsRunesNotBytes(t *testing.T) {
	// 5 Danish characters = 10 bytes. A rune budget of 5 must keep all of them.
	if got, want := trimOne("øæåøæ", 5), "øæåøæ"; got != want {
		t.Errorf("trimOne = %q, want %q (budget is characters, not bytes)", got, want)
	}
	if got, want := trimOne("øæåøæ", 3), "øæå…"; got != want {
		t.Errorf("trimOne = %q, want %q", got, want)
	}
	// Newlines collapse; short strings pass through unchanged.
	if got, want := trimOne("a\nb", 10), "a b"; got != want {
		t.Errorf("trimOne = %q, want %q", got, want)
	}
}

// Invalid input must not be passed through into the log.
func TestTrimOneSanitisesInvalidInput(t *testing.T) {
	got := trimOne("ok\xc3", 10) // dangling lead byte
	if !utf8.ValidString(got) {
		t.Errorf("trimOne passed invalid UTF-8 through: % x", got)
	}
}
