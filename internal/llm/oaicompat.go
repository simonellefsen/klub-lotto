package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatible implements the JSONGenerator + multiple-choice Provider
// interfaces against any OpenAI-style /chat/completions endpoint. OpenAI, xAI
// (Grok) and Z.AI (Zhipu GLM) all speak this wire format, so they share one
// implementation, differing only in base URL, default model, label, timeout,
// and whether the endpoint honours response_format=json_object. Construct via
// NewOpenAI / NewXAI / NewZAI. (OpenRouter is kept separate — it adds model-id
// validation and slug handling.)
type OpenAICompatible struct {
	label  string // "openai" | "xai" | "zai" — used in Name() and error messages
	APIKey string
	Model  string
	URL    string
	HTTP   *http.Client
	// MaxTokens caps the completion length; 0 = provider default.
	MaxTokens int
	// jsonResponseFormat requests response_format=json_object (OpenAI supports
	// it; xAI / Z.AI don't always, so they instruct in-prompt and parse defensively).
	jsonResponseFormat bool
}

func (o *OpenAICompatible) Name() string { return o.label + ":" + o.Model }

func (o *OpenAICompatible) ChooseOne(ctx context.Context, q Question) (Answer, error) {
	raw, err := o.GenerateJSON(ctx, formatPrompt(q), 0)
	if err != nil {
		return Answer{}, err
	}
	return parseChoiceJSON(raw, len(q.Options))
}

func (o *OpenAICompatible) GenerateJSON(ctx context.Context, prompt string, temperature float64) (string, error) {
	body := openAIRequest{
		Model: o.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: "Reply with JSON only. No markdown, no extra text."},
			{Role: "user", Content: prompt},
		},
		Temperature: temperature,
		MaxTokens:   o.MaxTokens,
	}
	if o.jsonResponseFormat {
		body.ResponseFormat = &openAIRespFormat{Type: "json_object"}
	}
	raw, err := postJSON(ctx, o.HTTP, o.URL, o.APIKey, body)
	if err != nil {
		return "", err
	}
	var resp openAIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("%s: parse response: %w", o.label, err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("%s: api error: %s", o.label, resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("%s: empty choices", o.label)
	}
	return resp.Choices[0].Message.Content, nil
}

// NewOpenAI returns an OpenAI Chat Completions provider. Empty model → "gpt-5.4"
// (good Danish, predictable JSON). OpenAI honours response_format=json_object.
func NewOpenAI(apiKey, model string) *OpenAICompatible {
	if model == "" {
		model = "gpt-5.4"
	}
	return &OpenAICompatible{
		label:              "openai",
		APIKey:             apiKey,
		Model:              model,
		URL:                "https://api.openai.com/v1/chat/completions",
		HTTP:               &http.Client{Timeout: 60 * time.Second},
		jsonResponseFormat: true,
	}
}

// NewXAI returns an xAI (Grok) provider. Empty model → "grok-4-fast" (cheap,
// fast, decent on Danish). response_format isn't on every Grok model, so we
// instruct in-prompt instead.
func NewXAI(apiKey, model string) *OpenAICompatible {
	if model == "" {
		model = "grok-4-fast"
	}
	return &OpenAICompatible{
		label:  "xai",
		APIKey: apiKey,
		Model:  model,
		URL:    "https://api.x.ai/v1/chat/completions",
		HTTP:   &http.Client{Timeout: 60 * time.Second},
	}
}

// NewZAI returns a Z.AI (Zhipu) GLM provider. Empty model → "glm-5.2" — a
// cheaper alternative to OpenRouter's fused models for the word games. Uses a
// longer timeout (GLM can be slow) and instructs JSON in-prompt.
func NewZAI(apiKey, model string) *OpenAICompatible {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "glm-5.2"
	}
	return &OpenAICompatible{
		label:  "zai",
		APIKey: apiKey,
		Model:  model,
		URL:    "https://api.z.ai/api/paas/v4/chat/completions",
		HTTP:   &http.Client{Timeout: 120 * time.Second},
	}
}

// --- shared OpenAI wire types + HTTP helper (used here and by OpenRouter) -----

type openAIRequest struct {
	Model          string            `json:"model"`
	Messages       []openAIMessage   `json:"messages"`
	ResponseFormat *openAIRespFormat `json:"response_format,omitempty"`
	Temperature    float64           `json:"temperature"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
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
