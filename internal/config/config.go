// Package config loads runtime configuration. Secrets come from .env.local
// (kept out of git). The plan is to swap this for a Vault/SOPS-aware loader
// once we move into k8s.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds everything the CLI needs at startup. Add fields here rather
// than reading os.Getenv from random packages.
type Config struct {
	DanskespilUsername string
	DanskespilPassword string

	OpenAIKey string
	XAIKey    string
	GeminiKey string

	// Extended provider keys (used by ordknude and future commands)
	AnthropicKey     string
	OpenRouterKey    string
	ZAIKey           string // Z.AI (Zhipu) — OpenAI-compatible GLM models; cheaper than OpenRouter fused models
	ZAIModel         string // Z.AI model slug, default glm-5.2
	OpenAIModel      string
	OpenRouterModel  string
	OpenRouterVisionModel string // optional second vision model for cross-check (e.g. google/gemini-flash-1.5 via OpenRouter)
	WordProvider              string
	OrdknudeProvider          string
	OrdKloeverFinalProvider   string // LLM used only for the very last attempt (11/12); typically a smarter model

	// Browser preferences — exposed so commands can override per-run.
	BrowserSessionName string // --session passed to agent-browser
	Headed             bool   // show the window (debugging)

	// Where to write logs, screenshots, transcripts.
	DataDir string
}

// Load reads .env.local from the given dir (typically the repo root) and
// returns a populated Config. Missing values are reported via Validate so the
// caller can decide which subset of secrets it actually needs.
func Load(repoRoot string) (*Config, error) {
	envPath := filepath.Join(repoRoot, ".env.local")
	kv, err := parseDotEnv(envPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", envPath, err)
	}

	get := func(key string) string {
		if v, ok := kv[key]; ok && v != "" {
			return cleanEnvValue(v)
		}
		return cleanEnvValue(os.Getenv(key))
	}

	dataDir := get("KLUBLOTTO_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Join(repoRoot, ".klublotto")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}

	session := get("AGENT_BROWSER_SESSION")
	if session == "" {
		session = get("AGENT_BROWSER_SESSION_NAME")
	}
	if session == "" {
		session = "klublotto"
	}

	wordProvider := get("WORD_PROVIDER")
	if wordProvider == "" {
		wordProvider = get("ORDKNUDE_PROVIDER")
	}

	zaiModel := get("ZAI_MODEL")
	if zaiModel == "" {
		zaiModel = "glm-5.2"
	}

	return &Config{
		DanskespilUsername: get("DANSKESPIL_USERNAME"),
		DanskespilPassword: get("DANSKESPIL_PASSWORD"),
		OpenAIKey:          get("OPENAI_API_KEY"),
		XAIKey:             get("XAI_API_KEY"),
		GeminiKey:          get("GEMINI_API_KEY"),
		AnthropicKey:       get("ANTHROPIC_API_KEY"),
		OpenRouterKey:      get("OPENROUTER_API_KEY"),
		ZAIKey:             get("ZAI_API_KEY"),
		ZAIModel:           zaiModel,
		OpenAIModel:        get("OPENAI_MODEL"),
		OpenRouterModel:    get("OPENROUTER_MODEL"),
		OpenRouterVisionModel: get("OPENROUTER_VISION_MODEL"),
		WordProvider:            wordProvider,
		OrdknudeProvider:        get("ORDKNUDE_PROVIDER"),
		OrdKloeverFinalProvider: get("ORDKLOEVER_FINAL_PROVIDER"),
		BrowserSessionName: session,
		Headed:             strings.EqualFold(get("KLUBLOTTO_HEADED"), "true"),
		DataDir:            dataDir,
	}, nil
}

// Validate returns a non-nil error listing every required field that is
// empty, scoped to the named action.
func (c *Config) Validate(action string) error {
	var missing []string
	require := func(name, val string) {
		if val == "" {
			missing = append(missing, name)
		}
	}
	switch action {
	case "login", "quiz":
		require("DANSKESPIL_USERNAME", c.DanskespilUsername)
		require("DANSKESPIL_PASSWORD", c.DanskespilPassword)
	}
	if action == "quiz" {
		// at least one LLM key must be present
		if c.OpenAIKey == "" && c.XAIKey == "" && c.GeminiKey == "" {
			missing = append(missing, "one of OPENAI_API_KEY/XAI_API_KEY/GEMINI_API_KEY")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing config for %q: %s", action, strings.Join(missing, ", "))
	}
	return nil
}

// parseDotEnv is a tiny, dependency-free .env parser. It tolerates blank
// lines, comments (#), and optional `export ` prefixes. Values may be
// surrounded by single or double quotes. We deliberately don't expand $VAR
// references — the values we deal with are credentials, not shell snippets.
func parseDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		out[key] = cleanEnvValue(val)
	}
	return out, sc.Err()
}

func cleanEnvValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
