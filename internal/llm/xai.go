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
	// xAI accepts response_format but it's not on every model; we instruct
	// in the prompt and parse defensively.
	body := openAIRequest{
		Model: x.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: "You answer Danish multiple-choice quiz questions. Reply with JSON only."},
			{Role: "user", Content: formatPrompt(q)},
		},
		Temperature: 0,
	}
	raw, err := postJSON(ctx, x.HTTP, x.URL, x.APIKey, body)
	if err != nil {
		return Answer{}, err
	}
	var resp openAIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Answer{}, fmt.Errorf("xai: parse response: %w", err)
	}
	if resp.Error != nil {
		return Answer{}, fmt.Errorf("xai: api error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return Answer{}, fmt.Errorf("xai: empty choices")
	}
	return parseChoiceJSON(resp.Choices[0].Message.Content, len(q.Options))
}
