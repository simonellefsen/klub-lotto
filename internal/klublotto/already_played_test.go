package klublotto

import "testing"

func TestAlreadyPlayedMessage(t *testing.T) {
	// The <main> text of each game's page after the day's round, as served on
	// 2026-11-01.
	played := []struct{ game, text, want string }{
		{"quiz", "Quiz gennemført! Du har allerede besvaret quizzen - mission fuldført! TILBAGE TIL SPIL & QUIZ ;",
			"Du har allerede besvaret quizzen - mission fuldført"},
		{"sudoku", "Godt klaret! Du har allerede løst denne sudoku. Håber du nød udfordringen og ser frem til næste gang! TILBAGE TIL SPIL & QUIZ",
			"Du har allerede løst denne sudoku"},
		{"ordkloever", "Prøv igen i morgen! Du har allerede besvaret Ordkløver i dag. Har du prøvet de andre spil? TILBAGE TIL SPIL & QUIZ",
			"Du har allerede besvaret Ordkløver i dag"},
		{"ordknuden", "Ordknuden besvaret! Du har allerede besvaret dagens runde. Håber du havde det sjovt! Har du prøvet de andre spil? TILBAGE TIL SPIL & QUIZ",
			"Du har allerede besvaret dagens runde"},
		{"krydsord", "Intet nyt her! Du har allerede besvaret dagens krydsord – men der kommer en ny igen i morgen. TILBAGE TIL SPIL & QUIZ",
			"Du har allerede besvaret dagens krydsord"},
		{"blok", "Intet nyt her!\nDu har allerede spillet Blok for Blok – men der kommer en ny igen i morgen. TILBAGE TIL SPIL & QUIZ",
			"Du har allerede spillet Blok for Blok"},
	}
	for _, c := range played {
		got, ok := alreadyPlayedMessage(c.text)
		if !ok || got != c.want {
			t.Errorf("%s: got %q, %v; want %q, true", c.game, got, ok, c.want)
		}
	}

	notPlayed := map[string]string{
		"quiz win screen":   "Et sandt geni!\nFantastisk! Det er quiz'me godt gået!\n(Du har allerede optjent dagens lod)",
		"ordkløver welcome": "Ordkløver\nVelkommen\n\nKan du gætte dagens gåde?\n\nSPIL ORDKLØVER\nSÅDAN SPILLER DU",
		"site footer":       "Har du styr på, hvor meget du har vundet eller tabt? Find dit spiloverblik her.",
		"loading":           "Indlæser\n\nOrdkløver",
	}
	for name, text := range notPlayed {
		if msg, ok := alreadyPlayedMessage(text); ok {
			t.Errorf("%s: matched %q; want no match", name, msg)
		}
	}
}
