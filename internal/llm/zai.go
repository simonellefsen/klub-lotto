package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ZAI talks to Z.AI's (Zhipu) OpenAI-compatible Chat Completions API with a GLM
// model (default glm-5.2). It's a cheaper alternative to OpenRouter's pricier
// fused models for the word games. The request/response shape matches OpenAI, so
// it reuses openAIRequest/openAIResponse/postJSON.
type ZAI struct {
	APIKey string
	Model  string
	HTTP   *http.Client
	URL    string
	// MaxTokens caps the completion length. 0 = provider default.
	MaxTokens int
}

// NewZAI returns a Z.AI provider. If model is empty, "glm-5.2" is used.
func NewZAI(apiKey, model string) *ZAI {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "glm-5.2"
	}
	return &ZAI{
		APIKey: apiKey,
		Model:  model,
		HTTP:   &http.Client{Timeout: 120 * time.Second},
		URL:    "https://api.z.ai/api/paas/v4/chat/completions",
	}
}

func (z *ZAI) Name() string { return "zai:" + z.Model }

// ChooseOne lets a GLM model participate in quiz majority voting; reuses the
// shared multiple-choice prompt + JSON parser.
func (z *ZAI) ChooseOne(ctx context.Context, q Question) (Answer, error) {
	raw, err := z.GenerateJSON(ctx, formatPrompt(q), 0)
	if err != nil {
		return Answer{}, err
	}
	return parseChoiceJSON(raw, len(q.Options))
}

func (z *ZAI) GenerateJSON(ctx context.Context, prompt string, temperature float64) (string, error) {
	body := openAIRequest{
		Model: z.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: "Reply with JSON only. No markdown, no extra text."},
			{Role: "user", Content: prompt},
		},
		Temperature: temperature,
		MaxTokens:   z.MaxTokens,
	}
	raw, err := postJSON(ctx, z.HTTP, z.URL, z.APIKey, body)
	if err != nil {
		return "", err
	}
	var resp openAIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("zai: parse response: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("zai: api error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("zai: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}
