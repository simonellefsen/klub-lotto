package klublotto

import (
	"fmt"
	"strings"
)

// Ordknuden ledger-notes helpers: pure formatting of the guess sequence and
// Wordle-style colour marks for the daily ledger. No browser/LLM dependency.

// ScoreOrdknudeGuess marks a 5-letter guess against the known answer using
// Wordle rules: exact matches are "correct", remaining letters that exist
// elsewhere (each answer letter consumed once) are "present", the rest "absent".
// Returns nil unless both guess and answer are exactly 5 letters.
func ScoreOrdknudeGuess(guess, answer string) []string {
	g := []rune(NormalizeDanishLetters(guess))
	a := []rune(NormalizeDanishLetters(answer))
	if len(g) != 5 || len(a) != 5 {
		return nil
	}
	marks := make([]string, 5)
	used := make([]bool, 5)
	for i := 0; i < 5; i++ {
		if g[i] == a[i] {
			marks[i] = "correct"
			used[i] = true
		}
	}
	for i := 0; i < 5; i++ {
		if marks[i] != "" {
			continue
		}
		marks[i] = "absent"
		for j := 0; j < 5; j++ {
			if !used[j] && g[i] == a[j] {
				marks[i] = "present"
				used[j] = true
				break
			}
		}
	}
	return marks
}

// OrdknudeMarkSquares renders a marks slice ("correct"/"present"/"absent") as
// the green/yellow/red square emoji string used in the ledger.
func OrdknudeMarkSquares(marks []string) string {
	var b strings.Builder
	for _, m := range marks {
		switch m {
		case "correct":
			b.WriteString("🟩")
		case "present":
			b.WriteString("🟨")
		case "absent":
			b.WriteString("🟥")
		}
	}
	return b.String()
}

// MergeGuessWords returns the ordered, de-duplicated guess list: the board
// history first (oldest→newest), then any words submitted this run that the
// win/loss overlay wiped from the extracted history (e.g. the final guess).
func MergeGuessWords(history []OrdknudeGuess, tried []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(w string) {
		w = NormalizeDanishLetters(w)
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, h := range history {
		add(h.Word)
	}
	for _, w := range tried {
		add(w)
	}
	return out
}

// OrdknudeGuessNotes builds the daily-ledger note: the colour-coded guess
// sequence ("Gæt: 1. SPROG 🟥🟨🟥🟥🟩 · …"). It prefers each guess's real board
// marks (from history) and falls back to scoring against the known answer.
func OrdknudeGuessNotes(tried []string, history []OrdknudeGuess, answer string) string {
	marksByWord := map[string][]string{}
	for _, h := range history {
		marksByWord[NormalizeDanishLetters(h.Word)] = h.Marks
	}
	answer = NormalizeDanishLetters(answer)
	var parts []string
	for i, w := range tried {
		w = NormalizeDanishLetters(w)
		marks := marksByWord[w]
		if len(marks) != 5 {
			marks = ScoreOrdknudeGuess(w, answer)
		}
		sq := OrdknudeMarkSquares(marks)
		if sq == "" && answer != "" && w == answer {
			sq = "🟩🟩🟩🟩🟩" // winning guess — re-extract may not have caught it
		}
		parts = append(parts, fmt.Sprintf("%d. %s %s", i+1, w, sq))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Gæt: " + strings.Join(parts, " · ")
}
