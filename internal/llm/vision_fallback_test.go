package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubVision struct {
	out   string
	err   error
	calls int
}

func (s *stubVision) ExtractFromImage(context.Context, []byte, string, string) (string, error) {
	s.calls++
	return s.out, s.err
}

func TestFallbackVisionUsesPrimaryWhenItWorks(t *testing.T) {
	p, f := &stubVision{out: "primary"}, &stubVision{out: "fallback"}
	got, err := FallbackVision{Primary: p, Fallback: f}.ExtractFromImage(context.Background(), nil, "image/png", "")
	if err != nil || got != "primary" || f.calls != 0 {
		t.Fatalf("got %q, %v, fallback calls %d; want primary and no fallback call", got, err, f.calls)
	}
}

func TestFallbackVisionFallsBackOnError(t *testing.T) {
	p := &stubVision{err: errors.New("gemini http 402: {\n \"error\": \"credits depleted\"\n}")}
	f := &stubVision{out: "fallback"}
	got, err := FallbackVision{Primary: p, Fallback: f}.ExtractFromImage(context.Background(), nil, "image/png", "")
	if err != nil || got != "fallback" {
		t.Fatalf("got %q, %v; want the fallback's answer", got, err)
	}
}

func TestFallbackVisionReportsBothErrors(t *testing.T) {
	p := &stubVision{err: errors.New("primary down")}
	f := &stubVision{err: errors.New("fallback down")}
	_, err := FallbackVision{Primary: p, Fallback: f}.ExtractFromImage(context.Background(), nil, "image/png", "")
	if err == nil || !strings.Contains(err.Error(), "primary down") || !strings.Contains(err.Error(), "fallback down") {
		t.Fatalf("err = %v; want both causes", err)
	}
}

func TestFallbackVisionWithoutFallbackReturnsPrimaryError(t *testing.T) {
	p := &stubVision{err: errors.New("primary down")}
	_, err := FallbackVision{Primary: p}.ExtractFromImage(context.Background(), nil, "image/png", "")
	if err == nil || err.Error() != "primary down" {
		t.Fatalf("err = %v; want the primary error unchanged", err)
	}
}
