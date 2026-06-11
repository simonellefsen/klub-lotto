package klublotto

// ddo.go — validation of Danish words against Den Danske Ordbog (ordnet.dk/ddo).
//
// We do a plain HTTP GET to ordnet.dk/ddo/ordbog/<word> and look for tell-tale
// markers in the HTML to decide whether the word is in the dictionary.
// No API key or scraping library is required.
//
// From testing:
//   Valid word  (e.g. "drøne"): page title "drøne | Den Danske Ordbog", JSON-LD
//                                description is a real definition, no miss markers.
//   Invalid word (e.g. "børne"): page contains '"børne" matcher ingen opslag i ordbogen'
//                                and JSON-LD description is "Ordet findes ikke i ordbogen."

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var ddoHTTP = &http.Client{Timeout: 10 * time.Second}

// IsDDOWord reports whether word appears as a headword in Den Danske Ordbog.
// Returns (true, nil) on a confirmed hit, (false, nil) on a confirmed miss,
// and (false, err) when the lookup itself failed (network, rate-limit, etc).
func IsDDOWord(ctx context.Context, word string) (bool, error) {
	word = strings.ToUpper(strings.TrimSpace(word))
	if word == "" {
		return false, fmt.Errorf("empty word")
	}
	// Use the direct URL format /ddo/ordbog/<word> (lowercase) for canonical lookup.
	lower := strings.ToLower(word)
	url := "https://ordnet.dk/ddo/ordbog/" + lower
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; klub-lotto-bot/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := ddoHTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("ddo lookup %s: %w", word, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return false, fmt.Errorf("ddo read body %s: %w", word, err)
	}
	html := string(body)

	// Definitive MISS markers — ordnet.dk uses these exact strings.
	missMarkers := []string{
		"matcher ingen opslag i ordbogen",
		"optræder ikke i ordbogen",
		"Ordet findes ikke i ordbogen",
		"ingen artikler matcher",
	}
	for _, m := range missMarkers {
		if strings.Contains(html, m) {
			return false, nil
		}
	}

	// Definitive HIT markers.
	hitMarkers := []string{
		`class="modern-match"`,   // the article heading for a matched headword
		`class="artikel modern-article"`, // article body present
		`"@type": "DefinedTerm"`, // JSON-LD for a real dictionary entry with definition
	}
	for _, m := range hitMarkers {
		if strings.Contains(html, m) {
			return true, nil
		}
	}

	// If none of the above matched, the page is ambiguous (could be a redirect or
	// suggestions page). Treat as a miss to avoid submitting questionable words.
	return false, nil
}

// FilterDDOWords removes candidates whose answer is not found in ordnet.dk/ddo.
// Words that fail the network check (timeout, etc.) are kept to avoid over-filtering.
func FilterDDOWords(ctx context.Context, cands []WordCandidate) []WordCandidate {
	out := make([]WordCandidate, 0, len(cands))
	for _, c := range cands {
		word := NormalizeDanishLetters(c.Answer)
		if word == "" {
			continue
		}
		lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ok, err := IsDDOWord(lookupCtx, word)
		cancel()
		if err != nil {
			// Network/timeout: keep the candidate (fail-open)
			fmt.Printf("   [ddo] %s — lookup failed (%v), keeping\n", word, err)
			out = append(out, c)
			continue
		}
		if ok {
			out = append(out, c)
		} else {
			fmt.Printf("   [ddo] dropping %s — not found in ordnet.dk/ddo\n", word)
		}
	}
	return out
}
