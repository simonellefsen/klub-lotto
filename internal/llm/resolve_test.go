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
		{"", "gemini:gemini-2.5-pro", ""}, // default
		{"gemini", "gemini:gemini-2.5-pro", ""},
		{"gemini:gemini-pro-latest", "gemini:gemini-pro-latest", ""}, // native account, model override
		{"gemini:gemini-3-pro-preview", "gemini:gemini-3-pro-preview", ""},
		{"openai", "openai:gpt-x", ""},                                // uses OpenAIModel
		{"openai:gpt-5.6-luna", "openai:gpt-5.6-luna", ""},            // slug override — native OpenAI, not OpenRouter
		{"openai:gpt-5.6-terra", "openai:gpt-5.6-terra", ""},          // slug override
		{"openai/gpt-5.6-luna", "openrouter:openai/gpt-5.6-luna", ""}, // '/' → OpenRouter's catalog, distinct provider
		{"xai", "xai:grok-4-fast", ""},
		{"grok", "xai:grok-4-fast", ""},
		{"openrouter", "openrouter:or/default", ""}, // uses OpenRouterModel
		{"anthropic", "", "anthropic:"},
		{"claude", "", "anthropic:"},
		{"zai", "zai:glm-5.2", ""},     // default ZAIModel
		{"zai:glm-9", "zai:glm-9", ""}, // slug override
		{"zai/glm-9", "zai:glm-9", ""},
		{"glm-6", "zai:glm-6", ""}, // bare glm- slug
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

// TestResolveGeminiModelDefault checks that the "gemini" keyword honours a
// configured default model (GEMINI_MODEL) while "gemini:<model>" still overrides it.
func TestResolveGeminiModelDefault(t *testing.T) {
	k := fullKeys()
	k.GeminiModel = "gemini-pro-latest"
	p, err := Resolve("gemini", k)
	if err != nil {
		t.Fatalf("Resolve(gemini): %v", err)
	}
	if got := p.Name(); got != "gemini:gemini-pro-latest" {
		t.Errorf("Resolve(gemini) with GeminiModel set = %q, want gemini:gemini-pro-latest", got)
	}
	// Explicit slug overrides the configured default.
	p2, _ := Resolve("gemini:gemini-2.5-flash", k)
	if got := p2.Name(); got != "gemini:gemini-2.5-flash" {
		t.Errorf("Resolve(gemini:gemini-2.5-flash) = %q, want gemini:gemini-2.5-flash", got)
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
		{"openai:gpt-5.6-luna", Keys{}, "OPENAI_API_KEY"},
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

func TestResolveVisionRouting(t *testing.T) {
	k := fullKeys()

	// "gemini:<model>" → native Gemini vision (the bug: previously routed to
	// OpenRouter and rejected as an invalid model id).
	vp, err := ResolveVision("gemini:gemini-pro-latest", k)
	if err != nil {
		t.Fatalf("ResolveVision(gemini:...): %v", err)
	}
	g, ok := vp.(*Gemini)
	if !ok {
		t.Fatalf("gemini slug → %T, want *Gemini", vp)
	}
	if g.Model != "gemini-pro-latest" {
		t.Errorf("Gemini.Model = %q, want gemini-pro-latest", g.Model)
	}

	// An "author/model" slug still routes to OpenRouter vision.
	if vp, err := ResolveVision("openai/gpt-5.5", k); err != nil {
		t.Fatalf("ResolveVision(slug): %v", err)
	} else if _, ok := vp.(*OpenRouterVision); !ok {
		t.Fatalf("slug → %T, want *OpenRouterVision", vp)
	}

	// "claude" → native Anthropic vision.
	if vp, err := ResolveVision("claude", k); err != nil {
		t.Fatalf("ResolveVision(claude): %v", err)
	} else if _, ok := vp.(*Anthropic); !ok {
		t.Fatalf("claude → %T, want *Anthropic", vp)
	}

	// Missing key for the routed provider is a clear error, not a silent fallback.
	if _, err := ResolveVision("gemini:x", Keys{}); err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("ResolveVision(gemini, no key) err = %v, want GEMINI_API_KEY", err)
	}
}

func TestResolveUnknown(t *testing.T) {
	_, err := Resolve("totally-bogus", fullKeys())
	if err == nil || !strings.Contains(err.Error(), "unknown word provider") {
		t.Fatalf("Resolve(bogus) error = %v, want 'unknown word provider'", err)
	}
}

// TestResolveOpenRouterReasoning verifies the reasoning-effort bound is applied
// to both OpenRouter routing paths (the "openrouter" keyword and a "/" slug),
// and left empty when not configured. The effort caps a thinking model's
// internal reasoning so it can't run past the per-call timeout.
func TestResolveOpenRouterReasoning(t *testing.T) {
	k := fullKeys()
	k.OpenRouterReasoning = "medium"
	for _, name := range []string{"openrouter", "google/gemini-3.1-pro"} {
		p, err := Resolve(name, k)
		if err != nil {
			t.Fatalf("Resolve(%q) error: %v", name, err)
		}
		or, ok := p.(*OpenRouter)
		if !ok {
			t.Fatalf("Resolve(%q) = %T, want *OpenRouter", name, p)
		}
		if or.ReasoningEffort != "medium" {
			t.Errorf("Resolve(%q).ReasoningEffort = %q, want %q", name, or.ReasoningEffort, "medium")
		}
	}

	// Unset → no reasoning bound (provider default).
	p, _ := Resolve("openrouter", fullKeys())
	if or := p.(*OpenRouter); or.ReasoningEffort != "" {
		t.Errorf("ReasoningEffort = %q, want empty when unconfigured", or.ReasoningEffort)
	}
}
