package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatibleNames(t *testing.T) {
	if got := NewOpenAI("k", "").Name(); got != "openai:gpt-5.4" {
		t.Errorf("OpenAI default Name = %q", got)
	}
	if got := NewXAI("k", "").Name(); got != "xai:grok-4-fast" {
		t.Errorf("XAI default Name = %q", got)
	}
	if got := NewZAI("k", "  ").Name(); got != "zai:glm-5.2" {
		t.Errorf("ZAI default Name (blank trimmed) = %q", got)
	}
	if got := NewZAI("k", "glm-9").Name(); got != "zai:glm-9" {
		t.Errorf("ZAI explicit Name = %q", got)
	}
}

// stubServer returns the given JSON body and records the last request body seen.
func stubServer(t *testing.T, respBody string, status int, capture *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, capture)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
}

func TestOpenAICompatibleGenerateJSON(t *testing.T) {
	var req map[string]any
	srv := stubServer(t, `{"choices":[{"message":{"content":"{\"answer\":\"KAT\"}"}}]}`, 200, &req)
	defer srv.Close()

	p := NewOpenAI("key", "gpt-test")
	p.URL = srv.URL
	got, err := p.GenerateJSON(context.Background(), "prompt", 0.2)
	if err != nil {
		t.Fatalf("GenerateJSON error: %v", err)
	}
	if got != `{"answer":"KAT"}` {
		t.Fatalf("content = %q", got)
	}
	// OpenAI requests response_format=json_object.
	if _, ok := req["response_format"]; !ok {
		t.Error("OpenAI request missing response_format")
	}
}

func TestXAIOmitsResponseFormat(t *testing.T) {
	var req map[string]any
	srv := stubServer(t, `{"choices":[{"message":{"content":"ok"}}]}`, 200, &req)
	defer srv.Close()

	p := NewXAI("key", "grok-test")
	p.URL = srv.URL
	if _, err := p.GenerateJSON(context.Background(), "prompt", 0); err != nil {
		t.Fatalf("GenerateJSON error: %v", err)
	}
	if _, ok := req["response_format"]; ok {
		t.Error("xAI request should NOT set response_format")
	}
}

func TestOpenAICompatibleAPIError(t *testing.T) {
	srv := stubServer(t, `{"error":{"message":"insufficient balance"}}`, 200, nil)
	defer srv.Close()

	p := NewZAI("key", "glm-test")
	p.URL = srv.URL
	_, err := p.GenerateJSON(context.Background(), "prompt", 0)
	if err == nil || !strings.Contains(err.Error(), "insufficient balance") || !strings.Contains(err.Error(), "zai") {
		t.Fatalf("expected labelled api error, got %v", err)
	}
}

func TestOpenAICompatibleHTTPError(t *testing.T) {
	srv := stubServer(t, `nope`, 500, nil)
	defer srv.Close()

	p := NewOpenAI("key", "gpt-test")
	p.URL = srv.URL
	_, err := p.GenerateJSON(context.Background(), "prompt", 0)
	if err == nil || !strings.Contains(err.Error(), "http 500") {
		t.Fatalf("expected http 500 error, got %v", err)
	}
}

func TestChatCompletionsURL(t *testing.T) {
	const fallback = "https://www.gdprchat.eu/api/v1"
	cases := []struct{ base, want string }{
		{"https://www.gdprchat.eu/api/v1", "https://www.gdprchat.eu/api/v1/chat/completions"},
		{"https://www.gdprchat.eu/api/v1/", "https://www.gdprchat.eu/api/v1/chat/completions"},
		{"  https://www.gdprchat.eu/api/v1  ", "https://www.gdprchat.eu/api/v1/chat/completions"},
		// A base that already names the endpoint must not get it appended twice.
		{"https://www.gdprchat.eu/api/v1/chat/completions", "https://www.gdprchat.eu/api/v1/chat/completions"},
		// Empty base falls back rather than producing "/chat/completions".
		{"", fallback + "/chat/completions"},
		{"https://self-hosted.example/v1", "https://self-hosted.example/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := chatCompletionsURL(c.base, fallback); got != c.want {
			t.Errorf("chatCompletionsURL(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestNewGDPRChatDefaults(t *testing.T) {
	p := NewGDPRChat("key", "", "")
	if p.Model != "mistral-large-latest" {
		t.Errorf("default model = %q, want mistral-large-latest", p.Model)
	}
	if p.Name() != "gdprchat:mistral-large-latest" {
		t.Errorf("Name() = %q", p.Name())
	}
	// An explicit eu_direct id must survive untouched.
	if p := NewGDPRChat("key", "https://x/v1", "qwen3.5-397b-a17b"); p.Model != "qwen3.5-397b-a17b" {
		t.Errorf("explicit model = %q", p.Model)
	}
}

func TestDescribeEmptyCompletion(t *testing.T) {
	// The 2026-09-01 case: OpenRouter dresses an upstream network failure as a
	// successful "stop", so the error must name the host and its own reason.
	var r openAIResponse
	r.Provider = "Z.AI"
	r.Choices = append(r.Choices, struct {
		NativeFinishReason string `json:"native_finish_reason,omitempty"`
		Message            struct {
			Content string `json:"content"`
		} `json:"message"`
	}{NativeFinishReason: "network_error"})
	if got, want := describeEmptyCompletion(&r), "Z.AI: network_error"; got != want {
		t.Errorf("describeEmptyCompletion = %q, want %q", got, want)
	}

	// Degrade gracefully when the envelope carries neither field.
	var bare openAIResponse
	if got, want := describeEmptyCompletion(&bare), "unknown provider"; got != want {
		t.Errorf("describeEmptyCompletion(bare) = %q, want %q", got, want)
	}
}
