package klublotto

import (
	"encoding/json"
	"strings"
)

// UnwrapString is the exported form of unwrapAgentBrowserString for use in cmd packages.
func UnwrapString(s string) string { return unwrapAgentBrowserString(s) }

func unwrapAgentBrowserString(s string) string {
	s = strings.TrimSpace(s)
	for depth := 0; depth < 4; depth++ {
		if s == "" || !strings.HasPrefix(s, "{") {
			return s
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return s
		}
		next := ""
		for _, key := range []string{"result", "value", "url", "text"} {
			if v, ok := obj[key].(string); ok {
				next = strings.TrimSpace(v)
				break
			}
		}
		if next == "" || next == s {
			return s
		}
		s = next
	}
	return s
}
