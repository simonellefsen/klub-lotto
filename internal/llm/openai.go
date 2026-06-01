package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAI implements Provider against the OpenAI Chat Completions API.
// Uses response_format=json_object to keep parsing trivial. Defaults to
// gpt-4.1 (good Danish, predictable JSON). Override via NewOpenAI's model
// arg.
type OpenAI struct {
	APIKey string
	Model  string
	HTTP   *http.Client
	URL    string // override for tests / Azure
}

// NewOpenAI returns a Provider. If model is empty, "gpt-4.1" is used.
func NewOpenAI(apiKey, model string) *OpenAI {
	if model == "" {
		model = "gpt-4.1"
	}
	return &OpenAI{
		APIKey: apiKey,
		Model:  model,
		HTTP:   &http.Client{Timeout: 60 * time.Second},
		URL:    "https://api.openai.com/v1/chat/completions",
	}
}

func (o *OpenAI) Name() string { return "openai:" + o.Model }

type openAIRequest struct {
	Model          string            `json:"model"`
	Messages       []openAIMessage   `json:"messages"`
	ResponseFormat *openAIRespFormat `json:"response_format,omitempty"`
	Temperature    float64           `json:"temperature"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRespFormat struct {
	Type string `json:"type"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (o *OpenAI) ChooseOne(ctx context.Context, q Question) (Answer, error) {
	body := openAIRequest{
		Model: o.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: "You answer Danish multiple-choice quiz questions. Reply with JSON only."},
			{Role: "user", Content: formatPrompt(q)},
		},
		ResponseFormat: &openAIRespFormat{Type: "json_object"},
		Temperature:    0,
	}
	raw, err := postJSON(ctx, o.HTTP, o.URL, o.APIKey, body)
	if err != nil {
		return Answer{}, err
	}
	var resp openAIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Answer{}, fmt.Errorf("openai: parse response: %w", err)
	}
	if resp.Error != nil {
		return Answer{}, fmt.Errorf("openai: api error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return Answer{}, fmt.Errorf("openai: empty choices")
	}
	return parseChoiceJSON(resp.Choices[0].Message.Content, len(q.Options))
}

// postJSON is the shared HTTP helper. Bearer-token auth, JSON in/out, ctx
// cancellation respected.
func postJSON(ctx context.Context, hc *http.Client, url, token string, body any) ([]byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(out), 400))
	}
	return out, nil
}
