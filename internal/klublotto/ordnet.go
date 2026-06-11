package klublotto

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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

	// Read the first 32 KB — enough to find the match marker without downloading
	// the whole page (DDO pages are ~200 KB each).
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return false, fmt.Errorf("ordnet: read body: %w", err)
	}

	// `class="modern-match"` is present exactly once when the word has a DDO entry.
	// It is absent (zero occurrences) when ordnet.dk returns an empty-results page
	// for an unknown word.
	return strings.Contains(string(body), `class="modern-match"`), nil
}
