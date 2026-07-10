package klublotto

import (
	"fmt"
	"testing"
)

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
	// The permanent account-overview promo (always in the page body when the
	// account menu is open) must NOT read as a win — it contains "du har vundet".
	if IsOrdKloeverWinText("Har du styr på, hvor meget du har vundet eller tabt? Find dit spiloverblik her.") {
		t.Fatal("IsOrdKloeverWinText() = true for the 'vundet eller tabt' account promo")
	}
	// "Dagens lod" / "belønning" marketing copy must not trip it either.
	if IsOrdKloeverWinText("Optjen dagens lod og få en belønning!") {
		t.Fatal("IsOrdKloeverWinText() = true for generic lod/belønning marketing copy")
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

func TestIsOrdknudeAlreadyAnswered(t *testing.T) {
	// The exact round-complete splash a human lands on after clicking "Tilbage til
	// Spil & Quiz" post-win (the screen that broke a live run 2026-07-09).
	done := "Ordknuden besvaret!\nDu har allerede besvaret dagens runde. Håber du havde det sjovt! Har du prøvet de andre spil?\nTILBAGE TIL SPIL & QUIZ"
	if !IsOrdknudeAlreadyAnswered(done) {
		t.Fatal("IsOrdknudeAlreadyAnswered() = false for the 'besvaret / allerede besvaret' completion screen")
	}
	// The win banner and a fresh board are NOT the already-answered screen.
	if IsOrdknudeAlreadyAnswered("Super imponerende! Du fandt frem til dagens ord.") {
		t.Fatal("IsOrdknudeAlreadyAnswered() = true for the win banner")
	}
	if IsOrdknudeAlreadyAnswered("Ordknuden\nGæt dagens ord.") {
		t.Fatal("IsOrdknudeAlreadyAnswered() = true for a fresh playable board")
	}
}

func TestIsFrameTornDownError(t *testing.T) {
	// The exact error agent-browser returned when the last sudoku cell's click
	// completed the puzzle and the win screen replaced the game iframe.
	live := fmt.Errorf(`click [data-ab-num="3"]: exit status 1 ` +
		`(stdout={"success":false,"data":null,"error":"CDP error (DOM.getFrameOwner): ` +
		`Frame with the given id was not found."} stderr=)`)
	if !IsFrameTornDownError(live) {
		t.Fatal("IsFrameTornDownError() = false for the live DOM.getFrameOwner teardown error")
	}
	for _, msg := range []string{
		"Execution context was destroyed",
		"could not find frame for selector",
		"Target closed",
		"frame was detached",
	} {
		if !IsFrameTornDownError(fmt.Errorf("%s", msg)) {
			t.Errorf("IsFrameTornDownError(%q) = false, want true", msg)
		}
	}
	// A real fault must still fail loudly — never swallowed as "we won".
	for _, msg := range []string{
		"element not found: .cell-8-6",
		"element is covered by <div.shadow.right>",
		"timeout waiting for selector",
		"context deadline exceeded",
	} {
		if IsFrameTornDownError(fmt.Errorf("%s", msg)) {
			t.Errorf("IsFrameTornDownError(%q) = true, want false (real error must not be swallowed)", msg)
		}
	}
	if IsFrameTornDownError(nil) {
		t.Fatal("IsFrameTornDownError(nil) = true")
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
