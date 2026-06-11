package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenRouter is OpenAI-compatible for the JSON prompt path. Official docs
// use POST https://openrouter.ai/api/v1/chat/completions with Bearer auth.
type OpenRouter struct {
	APIKey string
	Model  string
	HTTP   *http.Client
	URL    string
}

func NewOpenRouter(apiKey, model string) *OpenRouter {
	model = SanitizeModelSlug(model)
	if model == "" {
		model = "google/gemini-2.5-flash"
	}
	return &OpenRouter{
		APIKey: apiKey,
		Model:  model,
		HTTP:   &http.Client{Timeout: 540 * time.Second},
		URL:    "https://openrouter.ai/api/v1/chat/completions",
	}
}

// SanitizeModelSlug trims surrounding whitespace from a model slug.
//
// IMPORTANT: it does NOT strip a leading "~". On OpenRouter "~author/model-latest"
// is a real *floating-alias* syntax (e.g. "~google/gemini-pro-latest" resolves to
// the current concrete Gemini Pro preview). Stripping the "~" turns a valid alias
// into an invalid concrete slug ("google/gemini-pro-latest" → 400). When a model
// genuinely doesn't exist (with or without "~"), ValidateOpenRouterModel catches
// it via a real probe call instead.
func SanitizeModelSlug(s string) string {
	return strings.TrimSpace(s)
}

// ValidateOpenRouterModel confirms OpenRouter recognises a model slug by sending
// a minimal 1-token probe completion. We deliberately do NOT consult the public
// /models list — it omits valid aliases (e.g. "google/gemini-pro-latest"), which
// caused false negatives. Only OpenRouter's own "<id> is not a valid model ID"
// rejection counts as "does not exist"; any other outcome (success, rate limit,
// insufficient credits, transient network error) means the model is real and the
// run may proceed.
func ValidateOpenRouterModel(ctx context.Context, apiKey, model string) error {
	model = SanitizeModelSlug(model)
	if model == "" {
		return fmt.Errorf("empty OpenRouter model slug")
	}
	if apiKey == "" {
		return fmt.Errorf("OPENROUTER_API_KEY required to validate model %q", model)
	}

	probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	body := openAIRequest{
		Model:       model,
		Messages:    []openAIMessage{{Role: "user", Content: "ping"}},
		Temperature: 0,
		MaxTokens:   16, // some providers reject <16 (e.g. Azure-hosted GPT)
	}
	client := &http.Client{Timeout: 25 * time.Second}
	raw, err := postJSON(probeCtx, client, "https://openrouter.ai/api/v1/chat/completions", apiKey, body)
	if err != nil {
		if isUnknownModelErr(err.Error()) {
			return fmt.Errorf("model %q not recognised by OpenRouter — check the slug (a concrete model wants no '~'; a floating alias wants '~author/model-latest')", model)
		}
		// Some other failure (credits, rate limit, network) — that is NOT proof the
		// model is missing, so don't block the run on it.
		fmt.Printf("   [preflight] could not fully verify %q (%v) — proceeding anyway\n", model, err)
		return nil
	}
	// 2xx may still carry an inline error object.
	var resp openAIResponse
	if json.Unmarshal(raw, &resp) == nil && resp.Error != nil && isUnknownModelErr(resp.Error.Message) {
		return fmt.Errorf("model %q not recognised by OpenRouter: %s", model, resp.Error.Message)
	}
	return nil
}

// isUnknownModelErr reports whether an OpenRouter error string indicates the
// model ID itself is unrecognised (as opposed to credits/rate-limit/auth).
func isUnknownModelErr(s string) bool {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "not a valid model"):
		return true
	case strings.Contains(s, "no endpoints found"):
		return true
	case strings.Contains(s, "no allowed providers"):
		return true
	case strings.Contains(s, "model") && strings.Contains(s, "not found"):
		return true
	default:
		return false
	}
}

func (o *OpenRouter) Name() string { return "openrouter:" + o.Model }

// ChooseOne lets an OpenRouter model participate in quiz majority voting. It
// reuses the shared multiple-choice prompt + JSON parser (same path as OpenAI),
// since OpenRouter is OpenAI-compatible.
func (o *OpenRouter) ChooseOne(ctx context.Context, q Question) (Answer, error) {
	raw, err := o.GenerateJSON(ctx, formatPrompt(q), 0)
	if err != nil {
		return Answer{}, err
	}
	return parseChoiceJSON(raw, len(q.Options))
}

func (o *OpenRouter) GenerateJSON(ctx context.Context, prompt string, temperature float64) (string, error) {
	body := openAIRequest{
		Model: o.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: "Reply with JSON only. No markdown, no extra text."},
			{Role: "user", Content: prompt},
		},
		Temperature: temperature,
	}
	raw, err := postJSON(ctx, o.HTTP, o.URL, o.APIKey, body)
	if err != nil {
		return "", err
	}
	var resp openAIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("openrouter: parse response: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("openrouter: api error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openrouter: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}
