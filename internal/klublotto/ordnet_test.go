package klublotto

import "testing"

func TestOrdnetHasEntry(t *testing.T) {
	if !ordnetHasEntry([]byte(`<div class="modern-match">flittiglise</div>`)) {
		t.Error("a body containing the match marker should count as a DDO entry")
	}
	if ordnetHasEntry([]byte(`<div class="searchResultBox">Der blev ikke fundet noget</div>`)) {
		t.Error("the empty-results page must not count as a DDO entry")
	}
}

// TestOrdnetReadLimitClearsMarkerOffset guards the bug fixed on 2026-08-31: the
// read limit was 32 KB while DDO emits `class="modern-match"` around 64–67 KB
// into the page, so every lookup — real word or invented — reported "not found".
// Keep a wide margin over the highest offset observed (hus, 67235).
func TestOrdnetReadLimitClearsMarkerOffset(t *testing.T) {
	const observedMaxMarkerOffset = 67235
	if ordnetReadLimit <= observedMaxMarkerOffset {
		t.Fatalf("ordnetReadLimit = %d, must exceed the observed marker offset %d "+
			"or CheckOrdnet silently false-negatives on every word",
			ordnetReadLimit, observedMaxMarkerOffset)
	}
}
