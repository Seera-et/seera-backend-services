// Package config loads runtime configuration from the environment.
//
// Everything the process needs is read once, at startup, into an immutable
// Config. Nothing else in the codebase calls os.Getenv.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Secret is a string that must never reach a log line.
//
// It implements fmt.Stringer, so a stray %v, %s, or slog attribute prints
// "[redacted]" rather than an API key. Call Value at the point of use — which
// should be a provider constructor, and nowhere else.
type Secret string

func (s Secret) String() string { return "[redacted]" }

// Value returns the underlying secret.
func (s Secret) Value() string { return string(s) }

// Empty reports whether the secret was not configured.
func (s Secret) Empty() bool { return s == "" }

// Config is the fully resolved configuration.
type Config struct {
	Env      string // "dev" | "prod"
	Addr     string // listen address, e.g. ":8080"
	LogLevel string // "debug" | "info" | "warn" | "error"

	DatabaseURL Secret

	// AI provider credentials. These are deliberately not required at startup:
	// no provider is constructed until the phase that needs it, and each
	// adapter refuses to build with an empty key. Startup logs which are
	// missing, so the gap is visible without blocking a database-only run.
	GeminiAPIKey    Secret
	OpenAIAPIKey    Secret
	AnthropicAPIKey Secret
	CohereAPIKey    Secret

	// OpenRouterAPIKey enables a generation fallback, not a first-class
	// provider: when set, the LLM used for routing/chat/answer generation
	// tries Gemini first and falls back to OpenRouter only if that call fails
	// outright (an exhausted quota, an expired key) — never mid-stream, and
	// never for embeddings, since the corpus is embedded in Gemini's vector
	// space specifically. Absent, behavior is unchanged from before this
	// existed. Deliberately not in MissingAIKeys(): it is optional backup, not
	// a documented provider option someone should be nudged to configure.
	OpenRouterAPIKey Secret

	// OpenRouterModel overrides the fallback model. Empty uses the package
	// default.
	OpenRouterModel string

	// MultiHop enables the answer path's second retrieval pass: after the first
	// results come back, the model is asked what is still missing and that is
	// retrieved too.
	//
	// Off by default. It buys recall on questions that span several provisions,
	// and costs a model call plus a retrieval on the answers that use it — which
	// is charged directly to time-to-first-token. Turn it on when the evaluation
	// says the trade is worth it, not before.
	MultiHop bool

	// CitationEntailment enables a post-generation check: for each citation,
	// does the cited source's text actually support the claim it is attached
	// to, not just "was it retrieved and in context" (which MarkerFilter
	// already guarantees unconditionally).
	//
	// Off by default, same posture as MultiHop: it costs one more model call
	// after the stream ends (so it never delays the visible answer, only the
	// final citations/done event) and its false-positive rate is unmeasured
	// until the eval harness says otherwise. Turn it on when that evidence
	// says the trade is worth it, not before.
	CitationEntailment bool

	// GeminiModel overrides the generation model. Empty uses the package
	// default, which favours latency because time-to-first-token is a product
	// requirement.
	GeminiModel string

	// AdminToken guards the ingestion endpoint. When empty the endpoint is not
	// registered at all — an unauthenticated write path into the legal corpus
	// is worse than no ingestion endpoint.
	AdminToken Secret

	// SupabaseProjectURL is the origin of the Supabase project that issues user
	// tokens, e.g. https://<ref>.supabase.co. It is not a secret: it identifies
	// the issuer and locates the public JWKS the API verifies signatures
	// against. Empty leaves every authenticated route unregistered, the same
	// posture AdminToken takes — a route that cannot authenticate its caller
	// should not exist rather than fail open.
	SupabaseProjectURL string

	// SupabaseJWTAudience is the `aud` claim required on a user token. Supabase
	// issues "authenticated" for signed-in users and "anon" for the public key's
	// own token; accepting the latter would treat any visitor as a user, so this
	// is checked rather than assumed.
	SupabaseJWTAudience string

	CORSAllowedOrigins []string

	// HTTP server tuning. There is deliberately no write timeout — see
	// server.New, where SSE makes one actively harmful.
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxRequestBytes   int64

	// MaxIngestBytes is the separate, much larger cap for admin ingestion. A
	// parsed legal code runs to several megabytes of JSON, so the public limit
	// would reject every real document.
	MaxIngestBytes int64

	// EmbedTextsPerMinute paces ingestion to stay inside the embedding
	// provider's quota. Gemini's free tier allows 100 texts per minute, so 90
	// leaves room for the query embeddings that user traffic generates in the
	// same window. Zero disables pacing, which is right on a paid tier.
	EmbedTextsPerMinute int

	// Per-IP rate limit for the AI query endpoints: RateLimitRPS sustained,
	// RateLimitBurst instantaneous.
	RateLimitRPS   float64
	RateLimitBurst int
}

// Load reads configuration from the environment.
//
// It fails on anything malformed or missing that the process genuinely cannot
// start without, and returns every problem at once rather than one per restart.
func Load() (Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	cfg := Config{
		Env:      envString("ENV", "dev"),
		LogLevel: envString("LOG_LEVEL", "info"),

		DatabaseURL:        Secret(os.Getenv("DATABASE_URL")),
		GeminiAPIKey:       Secret(os.Getenv("GEMINI_API_KEY")),
		GeminiModel:        envString("GEMINI_MODEL", ""),
		MultiHop:           envBool("ANSWER_MULTI_HOP", false),
		CitationEntailment: envBool("ANSWER_CITATION_ENTAILMENT", false),
		OpenAIAPIKey:       Secret(os.Getenv("OPENAI_API_KEY")),
		AnthropicAPIKey:    Secret(os.Getenv("ANTHROPIC_API_KEY")),
		CohereAPIKey:       Secret(os.Getenv("COHERE_API_KEY")),
		AdminToken:         Secret(os.Getenv("ADMIN_API_TOKEN")),

		OpenRouterAPIKey: Secret(os.Getenv("OPENROUTER_API_KEY")),
		OpenRouterModel:  envString("OPENROUTER_MODEL", ""),

		SupabaseProjectURL:  strings.TrimRight(envString("SUPABASE_PROJECT_URL", ""), "/"),
		SupabaseJWTAudience: envString("SUPABASE_JWT_AUDIENCE", "authenticated"),

		CORSAllowedOrigins: envStrings("CORS_ALLOWED_ORIGINS", []string{"http://localhost:5173"}),
	}

	if cfg.Env != "dev" && cfg.Env != "prod" {
		fail("ENV must be \"dev\" or \"prod\", got %q", cfg.Env)
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("LOG_LEVEL must be one of debug|info|warn|error, got %q", cfg.LogLevel)
	}
	if cfg.DatabaseURL.Empty() {
		fail("DATABASE_URL is required")
	}
	if err := validateModelName("GEMINI_MODEL", cfg.GeminiModel); err != nil {
		fail("%w", err)
	}
	// Checked at boot rather than on the first sign-in: this value is compared
	// against the `iss` claim of every user token, so a typo here rejects every
	// user with an error that looks like a token problem.
	if cfg.SupabaseProjectURL != "" {
		u, err := url.Parse(cfg.SupabaseProjectURL)
		switch {
		case err != nil:
			fail("SUPABASE_PROJECT_URL is not a valid URL: %w", err)
		case u.Scheme != "https" && u.Host != "localhost" && !strings.HasPrefix(u.Host, "localhost:"):
			// http is allowed only against a local Supabase stack; a token
			// fetched over plaintext anywhere else is not a credential.
			fail("SUPABASE_PROJECT_URL must be https, got %q", cfg.SupabaseProjectURL)
		case u.Host == "":
			fail("SUPABASE_PROJECT_URL has no host: %q", cfg.SupabaseProjectURL)
		}
	}
	if cfg.SupabaseJWTAudience == "" {
		fail("SUPABASE_JWT_AUDIENCE must not be empty")
	}

	port, err := envInt("PORT", 8080)
	if err != nil {
		fail("%w", err)
	} else if port < 1 || port > 65535 {
		fail("PORT must be between 1 and 65535, got %d", port)
	}
	cfg.Addr = ":" + strconv.Itoa(port)

	for _, d := range []struct {
		key    string
		def    time.Duration
		target *time.Duration
	}{
		{"READ_HEADER_TIMEOUT", 10 * time.Second, &cfg.ReadHeaderTimeout},
		{"IDLE_TIMEOUT", 120 * time.Second, &cfg.IdleTimeout},
		{"SHUTDOWN_TIMEOUT", 20 * time.Second, &cfg.ShutdownTimeout},
	} {
		v, err := envDuration(d.key, d.def)
		if err != nil {
			fail("%w", err)
			continue
		}
		*d.target = v
	}

	maxBytes, err := envInt("MAX_REQUEST_BYTES", 1<<20) // 1 MiB
	if err != nil {
		fail("%w", err)
	}
	cfg.MaxRequestBytes = int64(maxBytes)

	maxIngestBytes, err := envInt("MAX_INGEST_BYTES", 32<<20) // 32 MiB
	if err != nil {
		fail("%w", err)
	}
	cfg.MaxIngestBytes = int64(maxIngestBytes)

	perMinute, err := envInt("EMBED_TEXTS_PER_MINUTE", 90)
	if err != nil {
		fail("%w", err)
	} else if perMinute < 0 {
		fail("EMBED_TEXTS_PER_MINUTE must not be negative, got %d", perMinute)
	}
	cfg.EmbedTextsPerMinute = perMinute

	rps, err := envFloat("RATE_LIMIT_RPS", 1)
	if err != nil {
		fail("%w", err)
	}
	cfg.RateLimitRPS = rps

	burst, err := envInt("RATE_LIMIT_BURST", 5)
	if err != nil {
		fail("%w", err)
	}
	cfg.RateLimitBurst = burst

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// IsDev reports whether the process is running in development mode.
func (c Config) IsDev() bool { return c.Env == "dev" }

// MissingAIKeys names the AI credentials that are not configured. Startup logs
// these as a warning: the features that need them will fail at construction,
// and it is better to see that at boot than at the first user question.
func (c Config) MissingAIKeys() []string {
	var missing []string
	for _, k := range []struct {
		name string
		val  Secret
	}{
		{"GEMINI_API_KEY", c.GeminiAPIKey},
		{"OPENAI_API_KEY", c.OpenAIAPIKey},
		{"ANTHROPIC_API_KEY", c.AnthropicAPIKey},
		{"COHERE_API_KEY", c.CohereAPIKey},
	} {
		if k.val.Empty() {
			missing = append(missing, k.name)
		}
	}
	return missing
}

// modelNamePattern is the shape of a provider model name: lowercase words joined
// by dots and dashes, as in "gemini-3.5-flash" or "gemini-embedding-001".
var modelNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[.\-][a-z0-9]+)*$`)

// validateModelName rejects a model override that is not a model name.
//
// This exists because of a failure that is expensive to diagnose: a credential
// landing in a model variable — from a shell export that outlives the .env file,
// or a value pasted one line off. The service then boots happily, logs the
// credential as the model name, and returns HTTP 400 from the provider on every
// single answer, which reads like a bug in the answer path rather than a
// configuration mistake.
//
// The offending value is never echoed. It may be a secret, and this is the exact
// case where it is one.
func validateModelName(key, value string) error {
	if value == "" || modelNamePattern.MatchString(value) {
		return nil
	}
	return fmt.Errorf(
		"%s is not a model name (%d characters starting %q). Models are named like "+
			`"gemini-3.5-flash"; leave it empty to use the default. If this is a credential, `+
			"it belongs in GEMINI_API_KEY — and note that a value exported in your shell "+
			"overrides the one in .env",
		key, len(value), firstRunes(value, 4))
}

// firstRunes returns a short prefix, enough to recognise a value without
// disclosing it.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// envBool reads a boolean flag. Anything other than a recognised true value is
// false, because a misspelt flag should leave a feature off rather than on.
func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envStrings(key string, def []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func envInt(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	return v, nil
}

func envFloat(key string, def float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", key, raw)
	}
	return v, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as \"30s\", got %q", key, raw)
	}
	return v, nil
}
