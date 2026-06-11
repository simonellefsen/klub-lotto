package config

import "testing"

func TestLoadStripsQuotedEnvironmentValues(t *testing.T) {
	t.Setenv("DANSKESPIL_USERNAME", `"lindau"`)
	t.Setenv("DANSKESPIL_PASSWORD", `'secret'`)
	t.Setenv("GEMINI_API_KEY", `"gemini-key"`)

	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if cfg.DanskespilUsername != "lindau" {
		t.Fatalf("username = %q", cfg.DanskespilUsername)
	}
	if cfg.DanskespilPassword != "secret" {
		t.Fatalf("password = %q", cfg.DanskespilPassword)
	}
	if cfg.GeminiKey != "gemini-key" {
		t.Fatalf("gemini key = %q", cfg.GeminiKey)
	}
}

func TestCleanEnvValueKeepsMismatchedQuotes(t *testing.T) {
	if got := cleanEnvValue(`"lindau'`); got != `"lindau'` {
		t.Fatalf("cleanEnvValue kept mismatched quotes as %q", got)
	}
}

func TestLoadPrefersAgentBrowserSession(t *testing.T) {
	t.Setenv("AGENT_BROWSER_SESSION", "new-session")
	t.Setenv("AGENT_BROWSER_SESSION_NAME", "old-session")

	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrowserSessionName != "new-session" {
		t.Fatalf("BrowserSessionName = %q", cfg.BrowserSessionName)
	}
}

func TestLoadWordProviderPrecedence(t *testing.T) {
	t.Setenv("WORD_PROVIDER", "openrouter")
	t.Setenv("ORDKNUDE_PROVIDER", "gemini")

	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WordProvider != "openrouter" {
		t.Fatalf("WordProvider = %q", cfg.WordProvider)
	}
	if cfg.OrdknudeProvider != "gemini" {
		t.Fatalf("OrdknudeProvider = %q", cfg.OrdknudeProvider)
	}
}

func TestLoadWordProviderFallsBackToOrdknudeProvider(t *testing.T) {
	t.Setenv("ORDKNUDE_PROVIDER", "xai")

	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WordProvider != "xai" {
		t.Fatalf("WordProvider = %q", cfg.WordProvider)
	}
}
