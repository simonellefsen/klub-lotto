package klublotto

// Ordknuden candidate filtering: pure predicates that prune LLM word candidates
// against the game state (history, rejected words) and Danish-word validity.

func containsWord(words []string, want string) bool {
	for _, w := range words {
		if w == want {
			return true
		}
	}
	return false
}

// AlreadyTriedOrdknude reports whether word (normalised) already appears in the
// board history.
func AlreadyTriedOrdknude(word string, history []OrdknudeGuess) bool {
	w := NormalizeDanishLetters(word)
	for _, h := range history {
		if NormalizeDanishLetters(h.Word) == w {
			return true
		}
	}
	return false
}

// FilterOrdknudeCandidates removes candidates that are:
//   - not exactly 5 Danish letters
//   - already in the game history
//   - in the rejected-words list
//   - duplicates within the batch (keeps first occurrence)
//
// Surviving candidates have their Answer normalised to upper-case Danish letters.
func FilterOrdknudeCandidates(cands []WordCandidate, st OrdknudeState, rejected []string) []WordCandidate {
	seen := map[string]bool{}
	out := make([]WordCandidate, 0, len(cands))
	for _, c := range cands {
		word := NormalizeDanishLetters(c.Answer)
		if word == "" || seen[word] {
			continue
		}
		if !IsDanishFiveLetterWord(word) {
			continue
		}
		if AlreadyTriedOrdknude(word, st.History) {
			continue
		}
		if containsWord(rejected, word) {
			continue
		}
		seen[word] = true
		c.Answer = word
		out = append(out, c)
	}
	return out
}
