package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Anthropic implements Provider against Claude's Messages API.
//
// Wire format differs from OpenAI: separate top-level `system` field,
// `content` is an array of typed parts, no `response_format` flag (we ask
// for JSON in the prompt and parse defensively). Default model is
// claude-sonnet-4-6 — strong on Danish trivia, the right tier for
// per-question latency. Override the model via NewAnthropic's second arg.
type Anthropic struct {
	APIKey  string
	Model   string
	Version string // anthropic-version header
	HTTP    *http.Client
	URL     string
}

// NewAnthropic returns a Provider. If model is empty, "claude-sonnet-4-6"
// is used. Versions older than 2023-06-01 are not supported.
func NewAnthropic(apiKey, model string) *Anthropic {
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	return &Anthropic{
		APIKey:  apiKey,
		Model:   model,
		Version: "2023-06-01",
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		URL:     "https://api.anthropic.com/v1/messages",
	}
}

func (a *Anthropic) Name() string { return "anthropic:" + a.Model }

// anthropicCacheMinChars is the prompt size above which GenerateJSON marks the
// prompt block with an ephemeral cache_control breakpoint. Prompt caching needs
// ≥1024 tokens (≥2048 on Haiku) to activate; ~4k chars ≈ 1.2k tokens. Repeated
// long prompts with a stable prefix (e.g. the Ordkløver decision rounds, the
// krydsord assembler retries) then pay ~10% for the cached part on re-reads.
// Below the threshold (or above it but never repeated) the marker is harmless.
const anthropicCacheMinChars = 4096

func (a *Anthropic) GenerateJSON(ctx context.Context, prompt string, temperature float64) (string, error) {
	var content interface{} = prompt
	if len(prompt) >= anthropicCacheMinChars {
		content = []anthropicTextBlock{{
			Type:         "text",
			Text:         prompt,
			CacheControl: &anthropicCacheControl{Type: "ephemeral"},
		}}
	}
	body := anthropicRequest{
		Model:       a.Model,
		MaxTokens:   512,
		System:      "Reply with JSON only. No markdown, no extra text.",
		Temperature: temperature,
		Messages: []anthropicMessage{
			{Role: "user", Content: content},
		},
	}
	text, err := a.post(ctx, body)
	if err != nil {
		return "", err
	}
	return text, nil
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	// Temperature is omitted (default 1.0). For deterministic answers we
	// set this to 0.
	Temperature float64 `json:"temperature"`
}

// anthropicMessage content is either a plain string or []anthropicTextBlock
// (needed to attach cache_control to long prompts).
type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type anthropicTextBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (a *Anthropic) ChooseOne(ctx context.Context, q Question) (Answer, error) {
	body := anthropicRequest{
		Model:       a.Model,
		MaxTokens:   512,
		System:      "You answer Danish multiple-choice quiz questions. Reply with JSON only.",
		Temperature: 0,
		Messages: []anthropicMessage{
			{Role: "user", Content: formatPrompt(q)},
		},
	}
	text, err := a.post(ctx, body)
	if err != nil {
		return Answer{}, err
	}
	return parseChoiceJSON(text, len(q.Options))
}

func (a *Anthropic) post(ctx context.Context, body anthropicRequest) (string, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", a.Version)

	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("anthropic: http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("anthropic: parse response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic: api error %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	// Concatenate text blocks (current API returns one for plain prompts,
	// but the schema is a slice).
	var text string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if text == "" {
		return "", fmt.Errorf("anthropic: empty content")
	}
	return text, nil
}

// ExtractFromImage sends an image plus a text prompt to Claude and returns
// the plain-text response. The image is passed as a base64-encoded content
// block. mediaType should be "image/png" or "image/jpeg".
func (a *Anthropic) ExtractFromImage(ctx context.Context, imageBytes []byte, mediaType, prompt string) (string, error) {
	type imageSrc struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	type contentBlock struct {
		Type   string    `json:"type"`
		Source *imageSrc `json:"source,omitempty"`
		Text   string    `json:"text,omitempty"`
	}
	type visionMsg struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type visionReq struct {
		Model       string      `json:"model"`
		MaxTokens   int         `json:"max_tokens"`
		System      string      `json:"system,omitempty"`
		Temperature float64     `json:"temperature"`
		Messages    []visionMsg `json:"messages"`
	}

	body := visionReq{
		Model:       a.Model,
		MaxTokens:   512,
		System:      "Reply with JSON only. No markdown, no extra text.",
		Temperature: 0,
		Messages: []visionMsg{{
			Role: "user",
			Content: []contentBlock{
				{
					Type: "image",
					Source: &imageSrc{
						Type:      "base64",
						MediaType: mediaType,
						Data:      base64.StdEncoding.EncodeToString(imageBytes),
					},
				},
				{Type: "text", Text: prompt},
			},
		}},
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", a.Version)

	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("anthropic vision: http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("anthropic vision: parse: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic vision: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	var text string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if text == "" {
		return "", fmt.Errorf("anthropic vision: empty content")
	}
	return text, nil
}
