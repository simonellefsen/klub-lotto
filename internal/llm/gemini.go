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
		HTTP:   &http.Client{Timeout: 60 * time.Second},
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

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenCfg struct {
	Temperature      float64 `json:"temperature"`
	ResponseMimeType string  `json:"response_mime_type,omitempty"`
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
	url := fmt.Sprintf(g.URL, g.Model, g.APIKey)
	body := geminiRequest{
		Contents: []geminiContent{{
			Role: "user",
			Parts: []geminiPart{{
				Text: "You answer Danish multiple-choice quiz questions. Reply with JSON only.\n\n" + formatPrompt(q),
			}},
		}},
		GenerationConfig: geminiGenCfg{
			Temperature:      0,
			ResponseMimeType: "application/json",
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return Answer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Answer{}, err
	}
	if resp.StatusCode >= 400 {
		return Answer{}, fmt.Errorf("gemini: http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var parsed geminiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Answer{}, fmt.Errorf("gemini: parse response: %w", err)
	}
	if parsed.Error != nil {
		return Answer{}, fmt.Errorf("gemini: api error: %s", parsed.Error.Message)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return Answer{}, fmt.Errorf("gemini: empty candidates")
	}
	return parseChoiceJSON(parsed.Candidates[0].Content.Parts[0].Text, len(q.Options))
}

// gemini.HTTP exists so tests can swap the client; not used in production.
var _ = (&Gemini{}).HTTP

// keep the time import busy when models are wired in by callers
var _ = time.Second
