// Package llm is a thin multi-provider abstraction for the Klub Lotto Quiz.
//
// The Quiz prompt is small (a single question + a handful of multiple-choice
// options, in Danish), so we don't need streaming, function calling, or any
// of the heavier capabilities. What we DO need is:
//
//   - A "pick one of N" answer that's trivial to parse.
//   - Cheap to swap providers (OpenAI, xAI, Gemini today; Claude later).
//   - Cheap to run multiple in parallel for an A/B comparison.
//
// So the interface is intentionally narrow: ChooseOne returns the index of
// the selected option plus a short rationale. Implementations enforce strict
// JSON output where the API supports it, and fall back to regex parsing when
// it doesn't.
package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Question is one quiz round: a Danish prompt and 2–6 textual options.
// Index 0 corresponds to Options[0], which is the answer the UI labels as
// the first choice — this matches the order we'll click later.
type Question struct {
	Prompt  string   // the question text in Danish, verbatim
	Options []string // the answer choices in display order
	Context string   // optional surrounding text scraped from the page
}

// Answer is what a Provider returns.
type Answer struct {
	Index      int    // 0-based index into Question.Options
	Confidence string // "high" | "medium" | "low" (free-form, model-dependent)
	Rationale  string // 1–2 sentences explaining the choice (Danish or English)
	Raw        string // verbatim model output, for debugging
}

// Provider is what every backend implements.
type Provider interface {
	Name() string
	ChooseOne(ctx context.Context, q Question) (Answer, error)
}

// Vote is one provider's answer plus timing — what compare command emits.
type Vote struct {
	Provider string
	Answer   Answer
	Err      error
	Latency  time.Duration
}

// CompareAll asks every provider concurrently and returns one Vote per
// provider in the same order. Errored providers still get a Vote with Err
// set, so callers can show a full transcript.
func CompareAll(ctx context.Context, providers []Provider, q Question) []Vote {
	votes := make([]Vote, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			start := time.Now()
			ans, err := p.ChooseOne(ctx, q)
			votes[i] = Vote{
				Provider: p.Name(),
				Answer:   ans,
				Err:      err,
				Latency:  time.Since(start),
			}
		}(i, p)
	}
	wg.Wait()
	return votes
}

// Majority returns the index chosen by the most providers. Ties are broken
// by preferring the first provider in the slice (so put the strongest one
// first). Returns -1 if no provider returned a valid index.
func Majority(votes []Vote) int {
	counts := map[int]int{}
	for _, v := range votes {
		if v.Err != nil {
			continue
		}
		counts[v.Answer.Index]++
	}
	if len(counts) == 0 {
		return -1
	}
	best, bestCount := -1, 0
	for _, v := range votes {
		if v.Err != nil {
			continue
		}
		c := counts[v.Answer.Index]
		if c > bestCount {
			best, bestCount = v.Answer.Index, c
		}
	}
	return best
}

// formatPrompt builds the user-facing instruction sent to each model. Kept
// here (and identical across providers) so we're really comparing models,
// not prompt engineering.
func formatPrompt(q Question) string {
	var b strings.Builder
	b.WriteString("Du er en dansk quizassistent. Besvar spørgsmålet ved at vælge det rigtige svar fra listen.\n\n")
	if q.Context != "" {
		b.WriteString("Kontekst:\n")
		b.WriteString(q.Context)
		b.WriteString("\n\n")
	}
	b.WriteString("Spørgsmål: ")
	b.WriteString(q.Prompt)
	b.WriteString("\n\nSvarmuligheder:\n")
	for i, o := range q.Options {
		fmt.Fprintf(&b, "  %d. %s\n", i, o)
	}
	b.WriteString("\nSvar med JSON i præcis dette format: ")
	b.WriteString(`{"index": <0-baseret>, "confidence": "high|medium|low", "rationale": "kort begrundelse"}`)
	b.WriteString("\nIngen markdown, ingen ekstra tekst — kun JSON.")
	return b.String()
}

// parseChoiceJSON extracts an Answer from a model response that should be a
// JSON object with index/confidence/rationale. We're permissive: strip
// ```json fences, find the first {...}, validate the index is in range.
func parseChoiceJSON(raw string, optionCount int) (Answer, error) {
	a := Answer{Raw: raw}
	clean := strings.TrimSpace(raw)
	// Drop markdown code fences if the model added them.
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)
	// Find the first JSON object.
	start := strings.Index(clean, "{")
	end := strings.LastIndex(clean, "}")
	if start < 0 || end <= start {
		return a, fmt.Errorf("no JSON object in response: %s", truncate(raw, 200))
	}
	obj := clean[start : end+1]

	var parsed struct {
		Index      *int   `json:"index"`
		Confidence string `json:"confidence"`
		Rationale  string `json:"rationale"`
		// Some models return option text instead of index — fall back to that.
		Answer string `json:"answer"`
	}
	if err := jsonUnmarshalLoose(obj, &parsed); err != nil {
		return a, fmt.Errorf("parse JSON %q: %w", obj, err)
	}
	if parsed.Index != nil {
		a.Index = *parsed.Index
	} else {
		// No index — try to match the textual answer.
		return a, errors.New("response missing 'index' field")
	}
	if a.Index < 0 || a.Index >= optionCount {
		return a, fmt.Errorf("index %d out of range [0,%d)", a.Index, optionCount)
	}
	a.Confidence = parsed.Confidence
	a.Rationale = parsed.Rationale
	return a, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
