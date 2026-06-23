package klublotto

import "strings"

// Ordkløver ledger-notes / prompt helpers: pure formatting of the probed-letter
// hit/miss sequence, the round note, and the puzzle prompt. No browser/LLM.

// OrdKloeverNotes builds the daily-ledger note for a finished Ordkløver round:
// the colour-coded letter probes, the answer shape, and a final status label.
func OrdKloeverNotes(shape, revealSrc string, probed []string, label string) string {
	var parts []string
	if seq := ColourCodeOrdKloeverLetters(probed, revealSrc); seq != "" {
		parts = append(parts, "Bogstavgæt: "+seq)
	}
	if shape != "" {
		parts = append(parts, "Mønster: "+shape)
	}
	if label != "" {
		parts = append(parts, label)
	}
	return strings.Join(parts, " · ")
}

// ColourCodeOrdKloeverLetters returns the probed letters in order, de-duplicated,
// each tagged 🟩 if it appears in revealSrc (a hit) or 🟥 if not (a miss).
func ColourCodeOrdKloeverLetters(probed []string, revealSrc string) string {
	hit := map[rune]bool{}
	for _, r := range []rune(NormalizeDanishLetters(revealSrc)) {
		hit[r] = true
	}
	seen := map[rune]bool{}
	var out []string
	for _, l := range probed {
		l = NormalizeDanishLetters(l)
		if l == "" {
			continue
		}
		r := []rune(l)[0]
		if seen[r] {
			continue
		}
		seen[r] = true
		mark := "🟥"
		if hit[r] {
			mark = "🟩"
		}
		out = append(out, string(r)+mark)
	}
	return strings.Join(out, " ")
}

// OrdKloeverPrompt renders the puzzle's category/hint/shape into the one-line
// "prompt / clue" cell of the daily ledger.
func OrdKloeverPrompt(st OrdKloeverState) string {
	parts := []string{}
	if st.Category != "" {
		parts = append(parts, "Category: `"+st.Category+"`")
	}
	if st.Hint != "" {
		parts = append(parts, "hint: `"+st.Hint+"`")
	}
	if st.Shape != "" {
		parts = append(parts, "answer pattern `"+st.Shape+"`")
	}
	if st.VisualShape != "" && st.VisualShape != st.Shape {
		parts = append(parts, "visual layout `"+st.VisualShape+"`")
	}
	return strings.Join(parts, "; ")
}
