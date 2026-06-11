package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Gemini implements Provider against Google's Generative Language API.
// We use generateContent with response_mime_type=application/json so the
// model returns clean JSON we can parse with parseChoiceJSON.
type Gemini struct {
	APIKey string
	Model  string
	HTTP   *http.Client
	URL    string // template w/o key+model substitution
}

func NewGemini(apiKey, model string) *Gemini {
	if model == "" {
		model = "gemini-2.5-flash"
	}
	return &Gemini{
		APIKey: apiKey,
		Model:  model,
		HTTP:   &http.Client{Timeout: 180 * time.Second},
		URL:    "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
	}
}

func (g *Gemini) Name() string { return "gemini:" + g.Model }

type geminiRequest struct {
	Contents         []geminiContent `json:"contents"`
	GenerationConfig geminiGenCfg    `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64-encoded
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
	// Thought is set to true on "thinking" parts returned by Gemini 2.5 Pro in
	// thinking mode. We skip these when extracting the final answer.
	Thought bool `json:"thought,omitempty"`
}

type geminiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type geminiGenCfg struct {
	Temperature      float64               `json:"temperature"`
	ResponseMimeType string                `json:"response_mime_type,omitempty"`
	ThinkingConfig   *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (g *Gemini) ChooseOne(ctx context.Context, q Question) (Answer, error) {
	raw, err := g.GenerateJSON(ctx, "You answer Danish multiple-choice quiz questions. Reply with JSON only.\n\n"+formatPrompt(q), 0)
	if err != nil {
		return Answer{}, err
	}
	return parseChoiceJSON(raw, len(q.Options))
}

func (g *Gemini) GenerateJSON(ctx context.Context, prompt string, temperature float64) (string, error) {
	url := fmt.Sprintf(g.URL, g.Model, g.APIKey)

	cfg := geminiGenCfg{
		Temperature:      temperature,
		ResponseMimeType: "application/json",
	}
	// gemini-2.5-flash supports thinkingBudget=0 (disables extended thinking,
	// keeps latency low). gemini-2.5-pro only works in thinking mode — budget=0
	// is rejected. For pro we omit ThinkingConfig entirely and let the model
	// use its default (thinking on, budget auto).
	if !strings.Contains(g.Model, "pro") {
		cfg.ThinkingConfig = &geminiThinkingConfig{ThinkingBudget: 0}
	}

	body := geminiRequest{
		Contents: []geminiContent{{
			Role: "user",
			Parts: []geminiPart{{
				Text: prompt,
			}},
		}},
		GenerationConfig: cfg,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini: request failed: %s", sanitizeGeminiError(err.Error(), g.APIKey))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini: http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var parsed geminiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("gemini: parse response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("gemini: api error: %s", parsed.Error.Message)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini: empty candidates")
	}
	return firstNonThoughtText(parsed.Candidates[0].Content.Parts)
}

// ExtractFromImage sends an image (as raw bytes) plus a text prompt to Gemini
// and returns the model's plain-text response. This satisfies llm.VisionProvider.
// mediaType should be "image/png" or "image/jpeg".
// Thinking is disabled (thinkingBudget=0) for latency; the task is purely
// perceptual (reading colours from a screenshot) and does not benefit from
// extended reasoning.
func (g *Gemini) ExtractFromImage(ctx context.Context, imageBytes []byte, mediaType, prompt string) (string, error) {
	url := fmt.Sprintf(g.URL, g.Model, g.APIKey)
	body := geminiRequest{
		Contents: []geminiContent{{
			Role: "user",
			Parts: []geminiPart{
				{InlineData: &geminiInlineData{
					MimeType: mediaType,
					Data:     base64.StdEncoding.EncodeToString(imageBytes),
				}},
				{Text: prompt},
			},
		}},
		// No ThinkingConfig here: gemini-2.5-pro requires thinking mode (budget>0
		// or omitted), while gemini-2.5-flash works with budget=0. Omitting it
		// lets the API use the model's default — flash defaults to no thinking,
		// pro defaults to thinking with a reasonable budget.
		GenerationConfig: geminiGenCfg{
			Temperature: 0,
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini vision: request failed: %s", sanitizeGeminiError(err.Error(), g.APIKey))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini vision: http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var parsed geminiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("gemini vision: parse response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("gemini vision: api error: %s", parsed.Error.Message)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini vision: empty candidates")
	}
	return firstNonThoughtText(parsed.Candidates[0].Content.Parts)
}

// firstNonThoughtText returns the text of the first part that is NOT a
// thinking trace. Gemini 2.5 Pro in thinking mode prepends one or more parts
// with Thought=true before the final answer part.
func firstNonThoughtText(parts []geminiPart) (string, error) {
	for _, p := range parts {
		if !p.Thought && p.Text != "" {
			return p.Text, nil
		}
	}
	// Fallback: return the last part's text regardless.
	if len(parts) > 0 {
		return parts[len(parts)-1].Text, nil
	}
	return "", fmt.Errorf("gemini: no text part in response")
}

func sanitizeGeminiError(s, apiKey string) string {
	if apiKey != "" {
		s = strings.ReplaceAll(s, apiKey, "REDACTED")
	}
	return s
}

// gemini.HTTP exists so tests can swap the client; not used in production.
var _ = (&Gemini{}).HTTP

// keep the time import busy when models are wired in by callers
var _ = time.Second
