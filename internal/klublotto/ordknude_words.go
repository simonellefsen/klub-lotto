package klublotto

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"
)

// ordknudeWordList holds every 5-letter word in Retskrivningsordbogen as the
// games' vendor ships it: the 5-letter entries of Kædeord's own word list
// (klub-lotto.immerspiele.com/public/assets/fallout/words-2820fe4d.txt, read
// 2026-11-18 with ParseKaedeordWordList). All 373 guesses Ordknuden accepted up
// to 2026-11-20 are in it; FOMLE, which it turned down that day, is not.
//
// It replaced the ordnet.dk/ddo lookups. DDO started answering 401/410, and the
// lookup kept a word whenever it failed, so FOMLE reached the board.
//
//go:embed ordknude_words.txt
var ordknudeWordList string

var ordknudeWordSet = sync.OnceValue(func() map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(ordknudeWordList) {
		set[w] = true
	}
	return set
})

// IsOrdknudeWord reports whether word is a 5-letter Retskrivningsordbogen word,
// the only kind Ordknuden accepts.
func IsOrdknudeWord(word string) bool {
	return ordknudeWordSet()[NormalizeDanishLetters(word)]
}

// FilterOrdknudeWords drops the candidates Ordknuden would turn down.
func FilterOrdknudeWords(cands []WordCandidate) []WordCandidate {
	out := make([]WordCandidate, 0, len(cands))
	for _, c := range cands {
		word := NormalizeDanishLetters(c.Answer)
		if IsOrdknudeWord(word) {
			out = append(out, c)
			continue
		}
		fmt.Printf("   [ordliste] dropping %s — not in Retskrivningsordbogen\n", word)
	}
	return out
}

// ordknudeAllWords lists the word list in order.
func ordknudeAllWords() []string {
	return strings.Fields(ordknudeWordList)
}
