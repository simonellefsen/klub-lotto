package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// XAI implements Provider against xAI's Grok API. The wire format is a
// near-clone of OpenAI's chat completions, so we reuse the same request
// shape. Default model: grok-4-fast — cheap, fast, decent on Danish.
type XAI struct {
	APIKey string
	Model  string
	HTTP   *http.Client
	URL    string
}

func NewXAI(apiKey, model string) *XAI {
	if model == "" {
		model = "grok-4-fast"
	}
	return &XAI{
		APIKey: apiKey,
		Model:  model,
		HTTP:   &http.Client{Timeout: 60 * time.Second},
		URL:    "https://api.x.ai/v1/chat/completions",
	}
}

func (x *XAI) Name() string { return "xai:" + x.Model }

func (x *XAI) ChooseOne(ctx context.Context, q Question) (Answer, error) {
	raw, err := x.GenerateJSON(ctx, formatPrompt(q), 0)
	if err != nil {
		return Answer{}, err
	}
	return parseChoiceJSON(raw, len(q.Options))
}

func (x *XAI) GenerateJSON(ctx context.Context, prompt string, temperature float64) (string, error) {
	// xAI accepts response_format but it's not on every model; we instruct
	// in the prompt and parse defensively.
	body := openAIRequest{
		Model: x.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: "Reply with JSON only. No markdown, no extra text."},
			{Role: "user", Content: prompt},
		},
		Temperature: temperature,
	}
	raw, err := postJSON(ctx, x.HTTP, x.URL, x.APIKey, body)
	if err != nil {
		return "", err
	}
	var resp openAIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("xai: parse response: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("xai: api error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("xai: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}
