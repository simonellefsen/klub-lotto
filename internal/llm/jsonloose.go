package llm

import (
	"encoding/json"
	"strings"
)

// jsonUnmarshalLoose tolerates a few common model quirks before delegating
// to encoding/json:
//
//   - Trailing commas in objects/arrays (some models love them).
//   - Smart quotes (U+201C/D) where ASCII quotes belong.
//   - Single quotes around keys/strings (Gemini sometimes does this).
//
// We intentionally keep this small. If a model produces malformed JSON the
// caller surfaces the raw output and we improve the prompt rather than
// growing a JSON repair engine.
func jsonUnmarshalLoose(s string, v any) error {
	s = strings.ReplaceAll(s, "“", "\"")
	s = strings.ReplaceAll(s, "”", "\"")
	s = strings.ReplaceAll(s, "‘", "'")
	s = strings.ReplaceAll(s, "’", "'")

	// Try as-is first; only repair on failure.
	if err := json.Unmarshal([]byte(s), v); err == nil {
		return nil
	}

	// Remove trailing commas before } or ].
	cleaned := removeTrailingCommas(s)
	if err := json.Unmarshal([]byte(cleaned), v); err == nil {
		return nil
	}

	// Last resort: convert single quotes to double quotes. This is brittle
	// (strings containing apostrophes will break), so we only do it as a
	// fallback.
	swapped := strings.ReplaceAll(cleaned, "'", "\"")
	return json.Unmarshal([]byte(swapped), v)
}

func removeTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ',' {
			// Look ahead past whitespace.
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // skip the comma
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}
