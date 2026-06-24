package klublotto

import "testing"

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
