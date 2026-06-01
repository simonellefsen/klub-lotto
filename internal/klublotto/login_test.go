package klublotto

import "testing"

func TestLooksLoggedInRejectsMitIDRødKontoText(t *testing.T) {
	if looksLoggedIn(
		"https://nemlog-in.mitid.dk/login/mitid",
		`- textbox "USER ID" [ref=e1]`,
		"Log-on at Danske Spil Rød Konto.",
	) {
		t.Fatal("MitID page must not be treated as a logged-in Danske Spil session")
	}
}

func TestLooksLoggedInRejectsKlubLottoRestrictionLoginPage(t *testing.T) {
	if looksLoggedIn(
		"https://danskespil.dk/klublotto/log-ind?returnUrl=https%3a%2f%2fdanskespil.dk%2fklublotto%2fdagens-quiz&source=klubLottoRestriction",
		`- button "Log ind" [ref=e1]`,
		"Danske Spil Rød Konto",
	) {
		t.Fatal("Klub Lotto login restriction page must not be treated as authenticated")
	}
}

func TestLooksLoggedInRejectsRedKontoCredentialPage(t *testing.T) {
	if looksLoggedIn(
		"https://id-dlo.danskespil.dk/webflow/login",
		`- textbox "Brugernavn" [ref=e1]`,
		"Log på Rød Konto",
	) {
		t.Fatal("Rød Konto credential page must not be treated as authenticated")
	}
}

func TestLooksLoggedInAcceptsAccountDrawerSignals(t *testing.T) {
	if !looksLoggedIn(
		"https://danskespil.dk/klublotto",
		`- button "Log ud" [ref=e1]`,
		"Saldo 1,00 kr. Indbetaling Udbetaling Kontohistorik",
	) {
		t.Fatal("account drawer signals should be treated as authenticated")
	}
}
