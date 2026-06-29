package llm

import (
	"fmt"
	"strings"
)

// Keys holds the API keys and default-model overrides that Resolve needs. The
// caller populates it from config, so the llm package stays free of any config
// dependency and Resolve is unit-testable without it.
type Keys struct {
	Gemini              string
	GeminiModel         string // default model for the "gemini" keyword ("" → gemini-2.5-pro)
	OpenAI              string
	OpenAIModel         string // default model for the "openai" keyword ("" → NewOpenAI default)
	XAI                 string
	Anthropic           string
	OpenRouter          string
	OpenRouterModel     string // default model for the "openrouter" keyword
	OpenRouterReasoning string // reasoning effort for OpenRouter thinking models (high|medium|low)
	ZAI                 string
	ZAIModel            string // default model for the "zai" keyword ("" → NewZAI default)
}

// Resolve maps a word-provider name to a JSONGenerator, pulling the matching key
// from keys. Accepted names:
//
//   - a keyword: gemini | openai | xai (grok) | anthropic (claude) | openrouter
//   - a native Gemini slug: "gemini" or "gemini:<model>" (e.g. "gemini:gemini-pro-latest")
//     — uses GEMINI_API_KEY directly (your own Google account), NOT OpenRouter
//   - a Z.AI slug: "zai", "zai:<model>", "zai/<model>", "glm" or a bare "glm-…"
//   - a full OpenRouter slug containing "/" (e.g. "google/gemini-3.1-pro-preview")
//
// An empty name resolves to gemini. Returns an error naming the missing key when
// the selected provider has no key configured, or for an unknown name.
func Resolve(name string, keys Keys) (JSONGenerator, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "gemini"
	}

	// Z.AI (Zhipu GLM) — OpenAI-compatible, cheaper than OpenRouter's fused
	// models. Checked before the '/' OpenRouter routing so "zai/glm-5.2" doesn't
	// leak to OpenRouter.
	if low := strings.ToLower(name); low == "zai" || low == "glm" || low == "zhipu" ||
		strings.HasPrefix(low, "zai:") || strings.HasPrefix(low, "zai/") || strings.HasPrefix(low, "glm-") {
		if keys.ZAI == "" {
			return nil, fmt.Errorf("ZAI_API_KEY is required for Z.AI provider %q", name)
		}
		model := keys.ZAIModel
		if i := strings.IndexAny(name, ":/"); i >= 0 { // zai:glm-5.2 / zai/glm-5.2
			model = strings.TrimSpace(name[i+1:])
		} else if strings.HasPrefix(low, "glm-") { // bare "glm-5.2"
			model = name
		}
		return NewZAI(keys.ZAI, model), nil
	}

	// Native Gemini (Google Generative Language API) via your own GEMINI_API_KEY.
	// "gemini" → the default/configured model; "gemini:<model>" picks one, e.g.
	// "gemini:gemini-pro-latest" for the rolling latest Pro on your account. Checked
	// before the '/' OpenRouter routing so it never leaks to OpenRouter.
	if low := strings.ToLower(name); low == "gemini" || strings.HasPrefix(low, "gemini:") {
		if keys.Gemini == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY is required for word provider gemini")
		}
		model := keys.GeminiModel
		if i := strings.IndexByte(name, ':'); i >= 0 { // gemini:gemini-pro-latest
			model = strings.TrimSpace(name[i+1:])
		}
		if model == "" {
			model = "gemini-2.5-pro"
		}
		return NewGemini(keys.Gemini, model), nil
	}

	// A '/' means an OpenRouter model slug (e.g. "meta-llama/llama-3.3-70b-instruct").
	// Route it straight to OpenRouter without requiring the keyword "openrouter".
	if strings.Contains(name, "/") {
		if keys.OpenRouter == "" {
			return nil, fmt.Errorf("OPENROUTER_API_KEY is required for OpenRouter model %q", name)
		}
		return newOpenRouterWithReasoning(keys.OpenRouter, name, keys.OpenRouterReasoning), nil
	}

	switch strings.ToLower(name) {
	case "openai":
		if keys.OpenAI == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY is required for word provider openai")
		}
		return NewOpenAI(keys.OpenAI, keys.OpenAIModel), nil
	case "xai", "grok":
		if keys.XAI == "" {
			return nil, fmt.Errorf("XAI_API_KEY is required for word provider xai")
		}
		return NewXAI(keys.XAI, ""), nil
	case "anthropic", "claude":
		if keys.Anthropic == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is required for word provider anthropic")
		}
		return NewAnthropic(keys.Anthropic, ""), nil
	case "openrouter":
		if keys.OpenRouter == "" {
			return nil, fmt.Errorf("OPENROUTER_API_KEY is required for word provider openrouter")
		}
		return newOpenRouterWithReasoning(keys.OpenRouter, keys.OpenRouterModel, keys.OpenRouterReasoning), nil
	default:
		return nil, fmt.Errorf("unknown word provider %q — use a keyword (gemini|openai|xai|anthropic|openrouter|zai) or a model slug (zai:glm-5.2, or a full OpenRouter slug e.g. google/gemini-3.1-pro-preview)", name)
	}
}

// ResolveVision maps a vision-model name to a VisionProvider, mirroring Resolve's
// routing on the image path so the same slug syntax works for VISION models:
//
//   - "gemini" or "gemini:<model>"        → native Gemini vision (GEMINI_API_KEY)
//   - "anthropic"/"claude"[:<model>]      → native Anthropic vision
//   - anything containing "/" (e.g. "openai/gpt-5.5", "~google/gemini-pro-latest",
//     "google/gemini-2.5-pro")            → OpenRouter vision
//
// Without this, a "gemini:<model>" vision slug was passed straight to OpenRouter
// and rejected ("gemini:gemini-3.5-pro is not a valid model ID").
func ResolveVision(name string, keys Keys) (VisionProvider, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("empty vision model")
	}
	low := strings.ToLower(name)
	switch {
	case low == "gemini" || strings.HasPrefix(low, "gemini:"):
		if keys.Gemini == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY is required for vision model %q", name)
		}
		model := keys.GeminiModel
		if i := strings.IndexByte(name, ':'); i >= 0 {
			model = strings.TrimSpace(name[i+1:])
		}
		if model == "" {
			model = "gemini-2.5-pro"
		}
		return NewGemini(keys.Gemini, model), nil
	case low == "anthropic" || low == "claude" || strings.HasPrefix(low, "anthropic:") || strings.HasPrefix(low, "claude:"):
		if keys.Anthropic == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is required for vision model %q", name)
		}
		model := ""
		if i := strings.IndexByte(name, ':'); i >= 0 {
			model = strings.TrimSpace(name[i+1:])
		}
		return NewAnthropic(keys.Anthropic, model), nil
	case strings.Contains(name, "/"):
		if keys.OpenRouter == "" {
			return nil, fmt.Errorf("OPENROUTER_API_KEY is required for OpenRouter vision model %q", name)
		}
		return NewOpenRouterVision(keys.OpenRouter, name), nil
	default:
		return nil, fmt.Errorf("unknown vision model %q — use gemini[:model], anthropic[:model], or an OpenRouter slug (e.g. openai/gpt-5.5)", name)
	}
}

// newOpenRouterWithReasoning builds an OpenRouter provider with an optional
// reasoning-effort bound so thinking models don't reason past the call timeout.
func newOpenRouterWithReasoning(apiKey, model, reasoning string) *OpenRouter {
	o := NewOpenRouter(apiKey, model)
	o.ReasoningEffort = strings.TrimSpace(reasoning)
	return o
}
