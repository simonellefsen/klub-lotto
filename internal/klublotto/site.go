// Package klublotto holds site-specific flows: login and per-game solvers.
//
// Selectors here are best-effort — the Danske Spil DOM changes occasionally
// and we don't have a stable test environment. We lean on agent-browser's
// accessibility-tree snapshot (which surfaces ARIA roles + names) and on
// semantic locators (find role / find text) instead of hard-coded CSS where
// possible.
package klublotto

const (
	// BaseURL is the public entry point.
	BaseURL = "https://danskespil.dk"

	// KlubLottoURL is the marketing/landing page. Logged-out users see the
	// "Log ind" CTA here.
	KlubLottoURL = "https://danskespil.dk/klublotto"

	// LoginURL is the central Danske Spil login. Klub Lotto redirects
	// here when an unauthenticated user clicks "Log ind".
	LoginURL = "https://danskespil.dk/log-ind"

	// QuizURL is best-guess for the Klub Lotto Quiz / "Tænkespil" game.
	// The site reshuffles paths occasionally so the solver also tries a
	// fallback discovery via the "Vælg spil" menu.
	QuizURL = "https://danskespil.dk/klublotto/dagens-quiz"

	// SudokuURL is the parent Danske Spil page for the daily Sudoku.
	SudokuURL = "https://danskespil.dk/klublotto/dagens-sudoku"

	// OrdKloeverURL is the parent Danske Spil page for Ordkløver.
	OrdKloeverURL = "https://danskespil.dk/klublotto/dagens-ordkloever"

	// OrdknudeURL is the parent Danske Spil page for Ordknuden.
	OrdknudeURL = "https://danskespil.dk/klublotto/dagens-ordknuden"

	// KrydsordURL is the parent Danske Spil page for the daily crossword.
	KrydsordURL = "https://danskespil.dk/klublotto/dagens-krydsord"

	// BlokURL is the parent Danske Spil page for Blok for Blok.
	BlokURL = "https://danskespil.dk/klublotto/dagens-blok-for-blok"
)
