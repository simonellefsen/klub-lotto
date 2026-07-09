package klublotto

import (
	"context"
	"strings"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

// End-of-game / interstitial screen detection for the Klub Lotto games. The
// daily games replace the board with a banner (win), a result line (loss), or —
// occasionally — danskespil's generic crash page. These detectors centralise the
// (Danish, multi-phrasing) text matching so the run loops can decide what to do.

// IsOrdKloeverWinText reports whether a page's text contains one of Danske
// Spil's Ordkløver success banners. The win screen reads "Flot præstation! Du
// løste ordkløver med stil!", but several phrasings exist, so we match all known
// variants.
//
// Deliberately EXCLUDES broad phrases that also appear in the permanent page
// chrome and would false-positive on a fresh board:
//   - "du har vundet" / "du vandt" — the account promo "Har du styr på, hvor
//     meget du har vundet eller tabt?" is always present.
//   - "dagens lod" / "dagens første lod" — awarded/advertised for ANY game.
//   - "belønning" — generic marketing copy.
//
// (This is the same trap IsOrdknudeWinText was hardened against.)
func IsOrdKloeverWinText(s string) bool {
	low := strings.ToLower(s)
	for _, banner := range []string{
		"flot præstation", "flot praestation",
		"løste ordkløver", "loeste ordkloever", // "Du løste ordkløver med stil!"
		"super imponerende",
		"knækket koden", "knaekket koden",
		"godt klaret", "godt gået", "godt gaet",
		"du klarede det",
		"tillykke",
	} {
		if strings.Contains(low, banner) {
			return true
		}
	}
	return false
}

// IsOrdknudeWinText reports whether the page text carries the Ordknuden win
// banner ("Super imponerende! Du fandt frem til dagens ord. Du er en sand
// ord-haj!"). These phrases appear in the Danske Spil *parent* page body
// (document.body.innerText) once the puzzle is solved, while the board itself
// is wiped to a 0-guess overlay — so this banner, not the empty board, is the
// reliable solved signal.
//
// Deliberately excludes bare "vundet" (the account nav permanently reads
// "vundet eller tabt") and "dagens første lod" (awarded after ANY game earns
// the lod), both of which would false-positive on every page load.
func IsOrdknudeWinText(s string) bool {
	low := strings.ToLower(s)
	for _, banner := range []string{
		"super imponerende",
		"fandt frem til dagens ord",
		"du fandt frem til",
		"ord-haj",
		"tillykke",
		"du vandt",
	} {
		if strings.Contains(low, banner) {
			return true
		}
	}
	return false
}

// IsOrdknudeAlreadyAnswered reports whether the page shows the round-COMPLETE
// screen ("Ordknuden besvaret! Du har allerede besvaret dagens runde. …"). This
// is distinct from the win banner: you land here after finishing the round AND
// whenever you navigate to / reopen a round you've already completed — e.g. a
// human clicking "Tilbage til Spil & Quiz" on the win screen before the automation
// has read the win banner. It is a definitive "the round is over, stop guessing"
// signal (win OR loss); the caller decides which from the guess count and whether
// a loss answer ("Det rigtige svar var …") is shown.
func IsOrdknudeAlreadyAnswered(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "ordknuden besvaret") ||
		strings.Contains(low, "allerede besvaret") ||
		strings.Contains(low, "besvaret dagens runde")
}

// IsDanskeSpilErrorScreen reports whether the page text is danskespil's generic
// crash/error screen ("Der skete en fejl. Prøv igen. Hvis fejlen fortsætter
// bedes du kontakte vores Kundecenter ..."), which sometimes replaces a game
// after a submit. It is NOT a win and NOT a normal blank board — detecting it
// lets the caller reopen the game and recover the real, server-remembered state
// instead of mis-reading the blank error page (e.g. recording a false solve).
func IsDanskeSpilErrorScreen(s string) bool {
	low := strings.ToLower(s)
	if strings.Contains(low, "der skete en fejl") {
		return true
	}
	// Be conservative otherwise: require the Kundecenter sentence alongside
	// "prøv igen" so a stray "prøv igen" in normal game copy never trips this.
	return strings.Contains(low, "prøv igen") && strings.Contains(low, "kundecenter")
}

// OrdknudeSolvedViaIframe checks for the win screen INSIDE the game iframe
// ("Super imponerende!", "Du fandt frem til dagens ord", "ord-haj", or the
// "optjent dagens lod" already-earned line). The win overlay replaces the board
// and renders inside the cross-origin iframe, so a parent-page snapshot can't
// see it; we read the iframe body text via a frame() switch (works now that
// agent-browser can eval inside OOPIFs). This is a fallback to the parent-body
// IsOrdknudeWinText check, for the case where the banner only shows in-frame.
func OrdknudeSolvedViaIframe(ctx context.Context, br *browser.Client) bool {
	entered := false
	for _, sel := range []string{GameIframe, "iframe[src*='ordknuden']", "iframe[src*='ordknude']", "iframe"} {
		if br.Frame(ctx, sel) == nil {
			entered = true
			break
		}
	}
	if !entered {
		return false
	}
	defer LeaveFrame(br)
	txt, _ := br.Eval(ctx, `String(document.body ? (document.body.innerText || document.body.textContent || '') : '')`)
	low := strings.ToLower(txt)
	return strings.Contains(low, "imponerende") ||
		strings.Contains(low, "fandt frem til") ||
		strings.Contains(low, "ord-haj") ||
		strings.Contains(low, "optjent dagens lod")
}
