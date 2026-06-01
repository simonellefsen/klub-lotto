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
