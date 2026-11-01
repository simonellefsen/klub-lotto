package llm

import (
	"context"
	"fmt"
	"strings"
)

// FallbackVision is a VisionProvider that tries Primary and, when that call
// fails, Fallback. Without it a vision key that runs dry blinds every board
// read for the day: on 2026-11-01 the Gemini prepay credits ran out, Ordknuden's
// only vision provider returned 402 on every call, and the run fell through to
// a text heuristic that recorded an unplayed board as solved.
type FallbackVision struct {
	Primary, Fallback VisionProvider
}

// ExtractFromImage implements VisionProvider.
func (f FallbackVision) ExtractFromImage(ctx context.Context, imageBytes []byte, mediaType, prompt string) (string, error) {
	out, err := f.Primary.ExtractFromImage(ctx, imageBytes, mediaType, prompt)
	if err == nil || f.Fallback == nil || ctx.Err() != nil {
		return out, err
	}
	fmt.Printf("   [vision] %s failed (%s); trying %s\n", visionName(f.Primary), firstLine(err.Error(), 120), visionName(f.Fallback))
	out, ferr := f.Fallback.ExtractFromImage(ctx, imageBytes, mediaType, prompt)
	if ferr != nil {
		return "", fmt.Errorf("%w; fallback %s: %v", err, visionName(f.Fallback), ferr)
	}
	return out, nil
}

// Name reports both providers, primary first.
func (f FallbackVision) Name() string {
	return visionName(f.Primary) + " → " + visionName(f.Fallback)
}

func visionName(vp VisionProvider) string {
	if n, ok := vp.(interface{ Name() string }); ok {
		return n.Name()
	}
	return fmt.Sprintf("%T", vp)
}

// firstLine returns s up to its first newline, cut to at most max runes —
// provider errors often carry a whole JSON body.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
