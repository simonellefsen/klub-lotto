package klublotto

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// KrydsordDict is our own growable lookup of crossword clue → known answers,
// learned over time from solved puzzles. Keys are normalized clues (see
// NormKrydsordClue); values are uppercase Danish answers, most-likely first.
// It complements the static convention hints in the solve prompt: those are
// rules (Roman numerals, solfège, …), this is accumulated lexical knowledge.
type KrydsordDict map[string][]string

// NormKrydsordClue normalizes a clue for dictionary keying/lookup: uppercase,
// keep only Danish letters and digits (so "1500" and "I DAG"→"IDAG" both work),
// drop everything else. Image-description clues ("IMG: …") collapse to letters,
// which simply won't match — that's fine.
func NormKrydsordClue(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == 'Æ', r == 'Ø', r == 'Å':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LoadKrydsordDict reads the dictionary JSON; a missing/invalid file yields an
// empty (usable) dictionary so callers never need to nil-check.
func LoadKrydsordDict(path string) KrydsordDict {
	d := KrydsordDict{}
	b, err := os.ReadFile(path)
	if err != nil {
		return d
	}
	_ = json.Unmarshal(b, &d)
	return d
}

// Save writes the dictionary as stable, pretty-printed JSON (keys sorted).
func (d KrydsordDict) Save(path string) error {
	// json.Marshal sorts map keys already; MarshalIndent keeps it readable/diffable.
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Add records a clue→answer mapping. Returns true if it was new. The answer is
// appended (existing answers keep priority); duplicates are ignored.
func (d KrydsordDict) Add(clue, answer string) bool {
	c := NormKrydsordClue(clue)
	a := NormalizeDanishLetters(answer)
	if c == "" || a == "" {
		return false
	}
	for _, x := range d[c] {
		if x == a {
			return false
		}
	}
	d[c] = append(d[c], a)
	return true
}

// Lookup returns the known answers for a clue (normalized internally), or nil.
func (d KrydsordDict) Lookup(clue string) []string {
	return d[NormKrydsordClue(clue)]
}

// MatchingLines returns, for each clue in clues that the dictionary knows, a
// human-readable "CLUE: A, B" line — sorted and de-duplicated by clue — suitable
// for embedding in the solve prompt. Only clues present in the puzzle are
// included to keep the prompt focused.
func (d KrydsordDict) MatchingLines(clues []string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, c := range clues {
		k := NormKrydsordClue(c)
		if k == "" || seen[k] || len(d[k]) == 0 {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		lines = append(lines, k+": "+strings.Join(d[k], ", "))
	}
	return lines
}
