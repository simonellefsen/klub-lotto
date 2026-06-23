package llm

import (
	"strings"
	"testing"
)

func fullKeys() Keys {
	return Keys{
		Gemini:          "g",
		OpenAI:          "o",
		OpenAIModel:     "gpt-x",
		XAI:             "x",
		Anthropic:       "a",
		OpenRouter:      "or",
		OpenRouterModel: "or/default",
		ZAI:             "z",
		ZAIModel:        "glm-5.2",
	}
}

func TestResolveRouting(t *testing.T) {
	k := fullKeys()
	cases := []struct {
		name       string
		wantName   string // exact Name(), or "" to only check prefix
		wantPrefix string
	}{
		{"", "gemini:gemini-2.5-pro", ""},          // default
		{"gemini", "gemini:gemini-2.5-pro", ""},
		{"openai", "openai:gpt-x", ""},             // uses OpenAIModel
		{"xai", "xai:grok-4-fast", ""},
		{"grok", "xai:grok-4-fast", ""},
		{"openrouter", "openrouter:or/default", ""}, // uses OpenRouterModel
		{"anthropic", "", "anthropic:"},
		{"claude", "", "anthropic:"},
		{"zai", "zai:glm-5.2", ""},                 // default ZAIModel
		{"zai:glm-9", "zai:glm-9", ""},             // slug override
		{"zai/glm-9", "zai:glm-9", ""},
		{"glm-6", "zai:glm-6", ""},                 // bare glm- slug
		{"zhipu", "zai:glm-5.2", ""},
		{"google/gemini-3.1-pro", "openrouter:google/gemini-3.1-pro", ""}, // '/' slug → openrouter
	}
	for _, c := range cases {
		p, err := Resolve(c.name, k)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", c.name, err)
			continue
		}
		got := p.Name()
		if c.wantName != "" && got != c.wantName {
			t.Errorf("Resolve(%q).Name() = %q, want %q", c.name, got, c.wantName)
		}
		if c.wantPrefix != "" && !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("Resolve(%q).Name() = %q, want prefix %q", c.name, got, c.wantPrefix)
		}
	}
}

func TestResolveMissingKeys(t *testing.T) {
	cases := []struct {
		name    string
		keys    Keys
		wantSub string
	}{
		{"gemini", Keys{}, "GEMINI_API_KEY"},
		{"openai", Keys{}, "OPENAI_API_KEY"},
		{"xai", Keys{}, "XAI_API_KEY"},
		{"anthropic", Keys{}, "ANTHROPIC_API_KEY"},
		{"openrouter", Keys{}, "OPENROUTER_API_KEY"},
		{"zai", Keys{}, "ZAI_API_KEY"},
		{"google/x", Keys{}, "OPENROUTER_API_KEY"}, // slug needs OpenRouter key
		{"glm-6", Keys{}, "ZAI_API_KEY"},
	}
	for _, c := range cases {
		_, err := Resolve(c.name, c.keys)
		if err == nil || !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("Resolve(%q, emptykeys) error = %v, want containing %q", c.name, err, c.wantSub)
		}
	}
}

func TestResolveUnknown(t *testing.T) {
	_, err := Resolve("totally-bogus", fullKeys())
	if err == nil || !strings.Contains(err.Error(), "unknown word provider") {
		t.Fatalf("Resolve(bogus) error = %v, want 'unknown word provider'", err)
	}
}
