package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// setMinimalEnv clears every variable Load reads and sets only what is
// required, so the developer's own environment cannot change the result.
func setMinimalEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ENV", "PORT", "LOG_LEVEL", "GEMINI_API_KEY", "GEMINI_MODEL",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY",
		"COHERE_API_KEY", "CORS_ALLOWED_ORIGINS", "READ_HEADER_TIMEOUT",
		"IDLE_TIMEOUT", "SHUTDOWN_TIMEOUT", "MAX_REQUEST_BYTES",
		"RATE_LIMIT_RPS", "RATE_LIMIT_BURST",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_URL", "postgres://seera:seera@localhost:5432/seera")
}

func TestLoadAppliesDefaults(t *testing.T) {
	setMinimalEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, ":8080")
	}
	if cfg.Env != "dev" || !cfg.IsDev() {
		t.Errorf("Env = %q, want dev", cfg.Env)
	}
	if len(cfg.CORSAllowedOrigins) != 1 || cfg.CORSAllowedOrigins[0] != "http://localhost:5173" {
		t.Errorf("CORSAllowedOrigins = %v, want the Vite dev origin", cfg.CORSAllowedOrigins)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded without DATABASE_URL")
	} else if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error = %q, want it to name DATABASE_URL", err)
	}
}

// Every problem should surface on one restart, not one per restart.
func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PORT", "not-a-number")
	t.Setenv("LOG_LEVEL", "chatty")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with three invalid values")
	}
	for _, want := range []string{"DATABASE_URL", "PORT", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestMissingAIKeysNamesUnsetCredentials(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("GEMINI_API_KEY", "test-gemini-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	got := strings.Join(cfg.MissingAIKeys(), ",")
	if got != "OPENAI_API_KEY,ANTHROPIC_API_KEY,COHERE_API_KEY" {
		t.Errorf("MissingAIKeys() = %q, want the unset keys", got)
	}
}

// A credential that reaches a log line is a leaked credential. Secret must
// redact through every formatting path a caller might reasonably take.
func TestSecretNeverFormatsItsValue(t *testing.T) {
	const value = "sk-ant-super-secret"
	s := Secret(value)

	for _, got := range []string{
		s.String(),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprint(s),
	} {
		if strings.Contains(got, value) {
			t.Errorf("formatted secret leaked the value: %q", got)
		}
	}

	if s.Value() != value {
		t.Error("Value() must return the real secret")
	}
	if !Secret("").Empty() || Secret("x").Empty() {
		t.Error("Empty() is wrong")
	}
}

func TestSecretRedactsInLogAttributes(t *testing.T) {
	const value = "sk-ant-super-secret"
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))

	log.Info("startup", slog.Any("key", Secret(value)))

	if strings.Contains(buf.String(), value) {
		t.Errorf("secret leaked through slog: %s", buf.String())
	}
}

func TestGeminiModelMustLookLikeAModelName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty uses the default", "", false},
		{"a real model", "gemini-3.5-flash", false},
		{"an embedding model", "gemini-embedding-001", false},
		// Shapes only — never a real credential, not even a prefix of one, in a
		// file that is committed.
		{"a credential pasted into the wrong variable", "AQ.Example0000_not-a-real-token", true},
		{"an AI Studio key", "AIzaSyD-EXAMPLE-KEY-VALUE-000000000000", true},
		{"a path-prefixed name the client would double up", "models/gemini-3.5-flash", true},
		{"whitespace", "gemini 3.5 flash", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv("GEMINI_MODEL", tt.value)

			_, err := Load()
			if tt.wantErr && err == nil {
				t.Fatalf("Load() with GEMINI_MODEL=%q succeeded, want a startup failure", tt.value)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Load() with GEMINI_MODEL=%q: %v", tt.value, err)
			}
			if !tt.wantErr {
				return
			}
			// The value may be a credential. It must not appear in the error,
			// which is printed to stderr and shipped to the log.
			if strings.Contains(err.Error(), tt.value) {
				t.Errorf("error message discloses the value: %v", err)
			}
			if !strings.Contains(err.Error(), "GEMINI_MODEL") {
				t.Errorf("error message does not name the variable: %v", err)
			}
		})
	}
}
