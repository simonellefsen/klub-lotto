package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	if model == "" {
		model = "google/gemini-2.5-flash"
	}
	return &OpenRouter{
		APIKey: apiKey,
		Model:  model,
		HTTP:   &http.Client{Timeout: 90 * time.Second},
		URL:    "https://openrouter.ai/api/v1/chat/completions",
	}
}

func (o *OpenRouter) Name() string { return "openrouter:" + o.Model }

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
