package klublotto

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsDDOWordRejectsBlockedUserAgent pins the live 2026-07-15 incident:
// ordnet.dk 401s any request whose User-Agent contains "bot", and the old
// honest "klub-lotto-bot/1.0" UA was silently turning every one of those
// blocks into a false "not a Danish word" — dropping real answers like
// KAHYT, HABIT, KAPUT. The fixed UA must not contain "bot", and a non-200
// response must surface as an ERROR (so the caller's fail-open path keeps
// the candidate), never as a confirmed miss.
func TestIsDDOWordRejectsBlockedUserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(strings.ToLower(r.UserAgent()), "bot") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body class="modern-match">ok</body></html>`))
	}))
	defer srv.Close()
	orig := ddoBaseURL
	ddoBaseURL = srv.URL + "/"
	defer func() { ddoBaseURL = orig }()

	ok, err := IsDDOWord(context.Background(), "kahyt")
	if err != nil {
		t.Fatalf("IsDDOWord() with a non-bot UA should succeed, got err: %v", err)
	}
	if !ok {
		t.Fatal("IsDDOWord() = false, want true (hit marker present)")
	}
}

// TestIsDDOWordNon200IsErrorNotMiss ensures a block/rate-limit/outage (any
// non-200) is reported as an error, not a confirmed miss — the bug that
// silently dropped valid words when ordnet.dk returned 401.
func TestIsDDOWordNon200IsErrorNotMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	orig := ddoBaseURL
	ddoBaseURL = srv.URL + "/"
	defer func() { ddoBaseURL = orig }()

	ok, err := IsDDOWord(context.Background(), "kaput")
	if err == nil {
		t.Fatal("IsDDOWord() on HTTP 401 should return an error, got nil")
	}
	if ok {
		t.Fatal("IsDDOWord() on HTTP 401 = true, want false")
	}
}

// TestIsDDOWordMissMarker confirms a genuine "no such word" page still
// reports a clean (false, nil) miss.
func TestIsDDOWordMissMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body>"xyzzy" matcher ingen opslag i ordbogen</body></html>`))
	}))
	defer srv.Close()
	orig := ddoBaseURL
	ddoBaseURL = srv.URL + "/"
	defer func() { ddoBaseURL = orig }()

	ok, err := IsDDOWord(context.Background(), "xyzzy")
	if err != nil {
		t.Fatalf("IsDDOWord() on a miss page should not error, got: %v", err)
	}
	if ok {
		t.Fatal("IsDDOWord() = true for a confirmed miss page, want false")
	}
}

// TestFilterDDOWordsKeepsCandidateOnLookupFailure pins the fail-open
// contract at the FilterDDOWords level: a lookup error must never drop a
// candidate.
func TestFilterDDOWordsKeepsCandidateOnLookupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	orig := ddoBaseURL
	ddoBaseURL = srv.URL + "/"
	defer func() { ddoBaseURL = orig }()

	cands := []WordCandidate{{Answer: "kaput"}}
	got := FilterDDOWords(context.Background(), cands)
	if len(got) != 1 {
		t.Fatalf("FilterDDOWords() dropped a candidate on lookup failure: got %#v", got)
	}
}
