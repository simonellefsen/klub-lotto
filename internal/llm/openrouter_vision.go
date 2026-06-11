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

// OpenRouterVision implements VisionProvider using OpenRouter's OpenAI-compatible
// chat completions API with vision (image_url content type, base64 data URI).
// Use for cross-checking board extraction against the primary Gemini provider.
type OpenRouterVision struct {
	APIKey string
	Model  string
	HTTP   *http.Client
}

// NewOpenRouterVision returns a VisionProvider backed by OpenRouter. If model is
// empty it defaults to meta-llama/llama-3.2-11b-vision-instruct:free, a free
// vision model on OpenRouter. Other choices:
//   - nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free  (omni/multimodal, may support vision)
//   - google/gemini-flash-1.5                              (paid but cheap and reliable)
func NewOpenRouterVision(apiKey, model string) *OpenRouterVision {
	model = SanitizeModelSlug(model)
	if model == "" {
		model = "google/gemini-3.5-flash"
	}
	return &OpenRouterVision{
		APIKey: apiKey,
		Model:  model,
		HTTP:   &http.Client{Timeout: 540 * time.Second},
	}
}

// Name satisfies a loose interface convention used for logging.
func (o *OpenRouterVision) Name() string { return "openrouter-vision:" + o.Model }

// openRouterVisionRequest uses an array content field to carry both the image
// and the text prompt. This is distinct from openAIRequest which uses a plain
// string content field (text-only). OpenRouter follows OpenAI's multimodal spec.
type openRouterVisionRequest struct {
	Model       string                     `json:"model"`
	Messages    []openRouterVisionMessage  `json:"messages"`
	Temperature float64                    `json:"temperature"`
}

type openRouterVisionMessage struct {
	Role    string                   `json:"role"`
	Content []openRouterVisionPart   `json:"content"`
}

type openRouterVisionPart struct {
	Type     string                     `json:"type"`
	Text     string                     `json:"text,omitempty"`
	ImageURL *openRouterVisionImageURL  `json:"image_url,omitempty"`
}

type openRouterVisionImageURL struct {
	URL string `json:"url"`
}

// ExtractFromImage sends the image as a base64 data URI alongside the prompt to
// OpenRouter and returns the model's plain-text response. Implements VisionProvider.
func (o *OpenRouterVision) ExtractFromImage(ctx context.Context, imageBytes []byte, mediaType, prompt string) (string, error) {
	dataURI := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(imageBytes)

	body := openRouterVisionRequest{
		Model: o.Model,
		Messages: []openRouterVisionMessage{
			{
				Role: "user",
				Content: []openRouterVisionPart{
					{
						Type:     "image_url",
						ImageURL: &openRouterVisionImageURL{URL: dataURI},
					},
					{
						Type: "text",
						Text: prompt,
					},
				},
			},
		},
		Temperature: 0,
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter-vision: request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("openrouter-vision: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("openrouter-vision: http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var parsed openAIResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("openrouter-vision: parse response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("openrouter-vision: api error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("openrouter-vision: empty choices")
	}
	return parsed.Choices[0].Message.Content, nil
}
