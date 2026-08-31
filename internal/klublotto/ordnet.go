package klublotto

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ordnetReadLimit bounds how much of a DDO page we read looking for the entry
// marker. It must be generous: the marker is NOT near the top of the document.
// Measured 2026-08-31 across real entries, `class="modern-match"` consistently
// lands around 64–67 KB in (flittiglise 64098, cykel 64819, år 64524, bil 66716,
// søn 66722, hus 67235) because DDO emits its nav/search chrome first. Whole
// pages run 73 KB (the "no results" page) to ~275 KB.
//
// This limit was 32 KB until 2026-08-31, which sat BELOW every real marker
// offset — so CheckOrdnet reported "not found" for every word ever passed to it,
// real or invented. 512 KB clears the largest observed page with room to spare.
const ordnetReadLimit = 512 * 1024

// CheckOrdnet returns true if the word has a direct entry in Den Danske Ordbog
// (DDO) at ordnet.dk. Non-existent words still get a 200 response but the page
// body contains no `class="modern-match"` element — that marker is the reliable
// distinguisher between a real entry and an empty-results page.
//
// Errors are non-fatal for the caller: if the lookup fails (timeout, network,
// etc.) log it and proceed rather than blocking auto-play.
func CheckOrdnet(ctx context.Context, word string) (found bool, err error) {
	clean := strings.ToLower(strings.TrimSpace(word))
	clean = strings.ReplaceAll(clean, " ", "") // compound words have no spaces in DDO URLs
	if clean == "" {
		return false, nil
	}

	targetURL := "https://ordnet.dk/ddo/ordbog/" + clean
	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; klub-lotto/1.0)")
	req.Header.Set("Accept", "text/html")

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("ordnet: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return false, nil
	}
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, fmt.Errorf("ordnet: http %d for %q", resp.StatusCode, clean)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, ordnetReadLimit))
	if err != nil {
		return false, fmt.Errorf("ordnet: read body: %w", err)
	}
	return ordnetHasEntry(body), nil
}

// ordnetHasEntry reports whether a DDO page body is a real dictionary entry.
// `class="modern-match"` is present exactly once when the word has an entry and
// absent entirely on the empty-results page an unknown word returns (that page
// is served with HTTP 200, so the status code cannot be used to tell them apart).
func ordnetHasEntry(body []byte) bool {
	return strings.Contains(string(body), `class="modern-match"`)
}
