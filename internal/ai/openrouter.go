package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
)

// requestSessionID reads the chi request ID already attached to ctx by
// internal/server/middleware, so OpenRouter's sticky-session routing works
// with zero new plumbing through qa/router/rag/chat. Empty outside an HTTP
// request (direct Go calls in tests, the eval harness) — session_id is then
// simply omitted, which OpenRouter treats the same as never setting it.
func requestSessionID(ctx context.Context) string {
	return middleware.GetReqID(ctx)
}

const (
	// OpenRouterDefaultModel is OpenRouter's auto-router across whatever free
	// model is currently available in its shared pool — the only option this
	// key's account can reach without purchased credits (2026-08-22: paid
	// models like openai/gpt-4o return HTTP 402, "insufficient credits").
	// Overridable via WithOpenRouterModel or OPENROUTER_MODEL once the account
	// is funded for a specific, comparable-quality paid model.
	OpenRouterDefaultModel = "openrouter/free"

	defaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
)

// OpenRouterLLM implements LLM against OpenRouter's OpenAI-compatible chat
// completions endpoint.
//
// It exists as a fallback, not a first-class provider: this project's
// evaluation baseline is measured against Gemini, and OpenRouter's free tier
// routes to whichever small model is available in a shared pool, which is not
// a like-for-like substitute (see FallbackLLM's doc comment). Speaks HTTP
// directly, matching OpenAIEmbedder's reasoning — one endpoint, one request
// shape, not worth a vendor SDK.
type OpenRouterLLM struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// OpenRouterOption configures the client.
type OpenRouterOption func(*OpenRouterLLM)

// WithOpenRouterModel overrides the default model.
func WithOpenRouterModel(model string) OpenRouterOption {
	return func(o *OpenRouterLLM) {
		if model != "" {
			o.model = model
		}
	}
}

// WithOpenRouterBaseURL points the client at a different endpoint — a proxy,
// or a test server.
func WithOpenRouterBaseURL(url string) OpenRouterOption {
	return func(o *OpenRouterLLM) { o.baseURL = url }
}

// WithOpenRouterHTTPClient supplies the HTTP client.
func WithOpenRouterHTTPClient(c *http.Client) OpenRouterOption {
	return func(o *OpenRouterLLM) { o.client = c }
}

// NewOpenRouterLLM builds the client. It fails on an empty key rather than
// deferring the problem to the first request.
func NewOpenRouterLLM(apiKey string, opts ...OpenRouterOption) (*OpenRouterLLM, error) {
	if apiKey == "" {
		return nil, errors.New("openrouter: API key is required")
	}
	o := &OpenRouterLLM{
		apiKey:  apiKey,
		model:   OpenRouterDefaultModel,
		baseURL: defaultOpenRouterBaseURL,
		// No overall timeout, matching GeminiLLM: a streaming answer is
		// long-lived by design, and cancellation comes from the request context.
		client: &http.Client{},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o, nil
}

func (o *OpenRouterLLM) Model() string { return o.model }

type openRouterMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openRouterResponseFormat asks for OpenAI-compatible structured output.
//
// Best-effort: not every model OpenRouter's free pool routes to honours
// strict JSON schema, and this provider is only ever reached as a fallback.
// A response that ignores the shape is not a special case here — the same
// callers (router.parseDecision) that validate Gemini's structured output
// already treat an unusable response as "fail towards the safe default"
// rather than trusting the shape blindly.
//
// Strict is always false, not a stray default: OpenAI's strict mode requires
// every schema object to carry "additionalProperties": false and every
// property to be listed as required, neither of which ai.Schema (shared with
// Gemini's translation, which needs neither) expresses. Found live,
// 2026-08-28: with strict true, a real request 400'd — "'additionalProperties'
// is required to be supplied and to be false" — which silently broke both the
// router (fell back to its safe default, as designed) and the reranker (fell
// back to unranked fusion order) every time this fallback was used. Verified
// directly against the API that strict:false accepts the exact same schema
// cleanly and the model still follows it, including an optional field like
// "reason" that isn't in Required.
type openRouterResponseFormat struct {
	Type       string                `json:"type"`
	JSONSchema *openRouterJSONSchema `json:"json_schema"`
}

type openRouterJSONSchema struct {
	Name   string  `json:"name"`
	Strict bool    `json:"strict"`
	Schema *Schema `json:"schema"`
}

type openRouterRequest struct {
	Model          string                    `json:"model"`
	Messages       []openRouterMessage       `json:"messages"`
	MaxTokens      int                       `json:"max_tokens,omitempty"`
	Stream         bool                      `json:"stream,omitempty"`
	ResponseFormat *openRouterResponseFormat `json:"response_format,omitempty"`

	// SessionID asks OpenRouter to route every request in the session to the
	// same underlying provider, so repeated calls with the same system prompt
	// hit that provider's prompt cache instead of a fresh one each time. Set
	// from the chi request ID already in ctx (see requestSessionID) — this
	// only ever groups the handful of LLM calls within one HTTP request (the
	// router's classification plus the legal engine's generation); it does
	// not persist across conversation turns, since the request ID is new on
	// every request and this backend keeps no server-side conversation state.
	SessionID string `json:"session_id,omitempty"`
}

// buildRequest is shared by Stream and Complete, so the two cannot drift in
// how they translate a Request.
func (o *OpenRouterLLM) buildRequest(ctx context.Context, req Request, stream bool) (openRouterRequest, error) {
	body := openRouterRequest{
		Model:     o.model,
		MaxTokens: req.MaxTokens,
		Stream:    stream,
		SessionID: requestSessionID(ctx),
	}

	if req.System != "" {
		body.Messages = append(body.Messages, openRouterMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		role := "user"
		if m.Role == RoleAssistant {
			role = "assistant"
		}
		body.Messages = append(body.Messages, openRouterMessage{Role: role, Content: m.Content})
	}
	if len(body.Messages) == 0 {
		return openRouterRequest{}, errors.New("openrouter: request has no messages")
	}

	if req.JSONSchema != nil {
		body.ResponseFormat = &openRouterResponseFormat{
			Type: "json_schema",
			JSONSchema: &openRouterJSONSchema{
				Name:   "response",
				Strict: false,
				Schema: req.JSONSchema,
			},
		}
	}

	return body, nil
}

func (o *OpenRouterLLM) post(ctx context.Context, body openRouterRequest) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openrouter: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("openrouter: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
		snippet := readSnippet(resp.Body)
		return nil, &HTTPError{
			Provider:   "openrouter",
			Status:     resp.StatusCode,
			Body:       snippet,
			RetryAfter: parseRetryAfter(resp.Header, snippet),
		}
	}
	return resp, nil
}

type openRouterChoice struct {
	Message      openRouterMessage `json:"message"`
	Delta        openRouterMessage `json:"delta"`
	FinishReason string            `json:"finish_reason"`
}

type openRouterResponse struct {
	Choices []openRouterChoice `json:"choices"`
	Usage   *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete returns the whole response in one call.
func (o *OpenRouterLLM) Complete(ctx context.Context, req Request) (string, *Usage, error) {
	body, err := o.buildRequest(ctx, req, false)
	if err != nil {
		return "", nil, err
	}

	resp, err := o.post(ctx, body)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded openRouterResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", nil, fmt.Errorf("openrouter: decode response: %w", err)
	}
	if decoded.Error != nil {
		return "", nil, fmt.Errorf("openrouter: %s", decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return "", nil, errors.New("openrouter: response has no choices")
	}

	var usage *Usage
	if decoded.Usage != nil {
		usage = &Usage{InputTokens: decoded.Usage.PromptTokens, OutputTokens: decoded.Usage.CompletionTokens}
	}
	return decoded.Choices[0].Message.Content, usage, nil
}

// Stream generates an answer, streaming text as it arrives.
func (o *OpenRouterLLM) Stream(ctx context.Context, req Request) (Stream, error) {
	body, err := o.buildRequest(ctx, req, true)
	if err != nil {
		return nil, err
	}

	resp, err := o.post(ctx, body)
	if err != nil {
		return nil, err
	}
	return &openRouterStream{resp: resp, scanner: bufio.NewScanner(resp.Body)}, nil
}

type openRouterStream struct {
	resp    *http.Response
	scanner *bufio.Scanner
	current Event
	err     error
	usage   *Usage
	stop    StopReason
	done    bool
}

func (s *openRouterStream) Next() bool {
	if s.err != nil || s.done {
		return false
	}

	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" {
			continue
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // comments and event lines carry nothing we need
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			break
		}

		var chunk openRouterResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			s.err = fmt.Errorf("openrouter: decode stream chunk: %w", err)
			return false
		}
		if chunk.Error != nil {
			s.err = fmt.Errorf("openrouter: %s", chunk.Error.Message)
			return false
		}
		if chunk.Usage != nil {
			s.usage = &Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			s.stop = ParseStopReason(choice.FinishReason)
		}
		if choice.Delta.Content == "" {
			continue // a chunk carrying only a finish reason or usage
		}

		s.current = Event{Type: EventText, Text: choice.Delta.Content}
		return true
	}

	if err := s.scanner.Err(); err != nil {
		s.err = fmt.Errorf("openrouter: read stream: %w", err)
		return false
	}

	s.done = true
	s.current = Event{Type: EventDone, Usage: s.usage, Stop: s.stop}
	return true
}

func (s *openRouterStream) Event() Event { return s.current }
func (s *openRouterStream) Err() error   { return s.err }

func (s *openRouterStream) Close() error {
	_, _ = io.Copy(io.Discard, s.resp.Body)
	return s.resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Fallback
// ---------------------------------------------------------------------------

// FallbackLLM tries a primary provider and falls back to a secondary one if
// the primary's call fails outright.
//
// Deliberately not a mid-stream failover: Stream's error surfaces either at
// the initial call (nothing sent yet — safe to retry elsewhere) or later, from
// Err() after Next() returns false (partial text may already be on the user's
// screen — retrying there would duplicate or contradict it, which is worse
// than the failure). Only the first case is covered, because it is the one a
// retry can fix safely, and it is also the shape an exhausted daily quota or
// an expired key actually fails in: an error returned before any bytes flow.
type FallbackLLM struct {
	primary   LLM
	secondary LLM
	label     string
	log       *slog.Logger
}

// NewFallbackLLM builds the wrapper. label is what Model() reports —
// composed once, not tracked per request, so a concurrent request never
// misattributes which provider it actually used; the honest thing a shared
// label can say is that a fallback is configured, not which call took it.
// Falling back is always logged, the same way rag's reranker fallback is: a
// silent one hides a real production problem ("why has Gemini been down for
// three days") behind an answer that still worked.
func NewFallbackLLM(primary, secondary LLM, log *slog.Logger) *FallbackLLM {
	return &FallbackLLM{
		primary:   primary,
		secondary: secondary,
		label:     fmt.Sprintf("%s (fallback: %s)", primary.Model(), secondary.Model()),
		log:       log,
	}
}

func (f *FallbackLLM) Model() string { return f.label }

func (f *FallbackLLM) Stream(ctx context.Context, req Request) (Stream, error) {
	s, err := f.primary.Stream(ctx, req)
	if err == nil {
		return s, nil
	}
	f.log.WarnContext(ctx, "primary LLM failed, falling back",
		slog.String("primary", f.primary.Model()),
		slog.String("fallback", f.secondary.Model()),
		slog.Any("error", err))
	return f.secondary.Stream(ctx, req)
}

func (f *FallbackLLM) Complete(ctx context.Context, req Request) (string, *Usage, error) {
	text, usage, err := f.primary.Complete(ctx, req)
	if err == nil {
		return text, usage, nil
	}
	f.log.WarnContext(ctx, "primary LLM failed, falling back",
		slog.String("primary", f.primary.Model()),
		slog.String("fallback", f.secondary.Model()),
		slog.Any("error", err))
	return f.secondary.Complete(ctx, req)
}
