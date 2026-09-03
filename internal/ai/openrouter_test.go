package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestOpenRouterStreamParsesSSE(t *testing.T) {
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`data: {"choices":[{"delta":{"content":"A company "}}]}`,
			`data: {"choices":[{"delta":{"content":"may be formed [S1]."}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":9}}`,
			`data: [DONE]`,
		} {
			_, _ = w.Write([]byte(line + "\n\n"))
		}
	}))
	defer server.Close()

	llm, err := NewOpenRouterLLM("test-key", WithOpenRouterBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewOpenRouterLLM() = %v", err)
	}

	stream, err := llm.Stream(context.Background(), Request{
		System:    "you cite sources",
		Messages:  []Message{{Role: RoleUser, Content: "how do I form a company?"}},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	defer func() { _ = stream.Close() }()

	var text strings.Builder
	var usage *Usage
	var stop StopReason
	for stream.Next() {
		event := stream.Event()
		switch event.Type {
		case EventText:
			text.WriteString(event.Text)
		case EventDone:
			usage = event.Usage
			stop = event.Stop
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if got := text.String(); got != "A company may be formed [S1]." {
		t.Errorf("text = %q", got)
	}
	if usage == nil || usage.OutputTokens != 9 || usage.InputTokens != 120 {
		t.Errorf("usage = %+v, want the reported counts", usage)
	}
	if stop != StopComplete {
		t.Errorf("stop = %q, want %q", stop, StopComplete)
	}

	// The system prompt travels as a "system" role message, not systemInstruction
	// — this is the OpenAI-compatible shape, not Gemini's.
	messages, _ := gotBody["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("no messages sent")
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "you cite sources" {
		t.Errorf("first message = %+v, want the system prompt as a system-role message", first)
	}
	if gotBody["stream"] != true {
		t.Error("stream=true was not sent for a Stream() call")
	}
}

func TestOpenRouterCompleteReturnsTheWholeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if body["stream"] == true {
			t.Error("Complete() must not ask for a stream")
		}

		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "the decision"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 40, "completion_tokens": 5}
		}`))
	}))
	defer server.Close()

	llm, _ := NewOpenRouterLLM("test-key", WithOpenRouterBaseURL(server.URL))
	text, usage, err := llm.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "route this"}},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if text != "the decision" {
		t.Errorf("text = %q", text)
	}
	if usage == nil || usage.InputTokens != 40 || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestOpenRouterCompleteSendsJSONSchemaWhenSet(t *testing.T) {
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer server.Close()

	llm, _ := NewOpenRouterLLM("test-key", WithOpenRouterBaseURL(server.URL))
	_, _, err := llm.Complete(context.Background(), Request{
		Messages:   []Message{{Role: RoleUser, Content: "route this"}},
		JSONSchema: &Schema{Type: "object", Properties: map[string]*Schema{"intent": {Type: "string"}}},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}

	format, ok := gotBody["response_format"].(map[string]any)
	if !ok {
		t.Fatal("response_format was not sent when JSONSchema was set")
	}
	if format["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", format["type"])
	}

	jsonSchema, ok := format["json_schema"].(map[string]any)
	if !ok {
		t.Fatal("response_format.json_schema was not an object")
	}
	// Regression guard for the 2026-08-28 bug: strict:true without
	// additionalProperties:false on every schema object 400s against real
	// OpenAI-compatible models (e.g. openai/gpt-4o-mini). ai.Schema doesn't
	// express additionalProperties, so this must stay false.
	if strict, _ := jsonSchema["strict"].(bool); strict {
		t.Error("json_schema.strict = true, want false — OpenAI strict mode requires additionalProperties:false on every schema object, which ai.Schema doesn't set")
	}
}

func TestOpenRouterSurfacesAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer server.Close()

	llm, _ := NewOpenRouterLLM("test-key", WithOpenRouterBaseURL(server.URL))
	_, err := llm.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "q"}},
	})

	if err == nil {
		t.Fatal("Stream() ignored a 429")
	}
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not an *HTTPError: %v", err)
	}
	if !httpErr.Retryable() || !httpErr.RateLimited() {
		t.Error("a 429 should be retryable and rate-limited")
	}
}

func TestOpenRouterRejectsAnEmptyRequest(t *testing.T) {
	llm, _ := NewOpenRouterLLM("test-key")
	if _, err := llm.Stream(context.Background(), Request{}); err == nil {
		t.Error("Stream() accepted a request with no messages")
	}
	if _, _, err := llm.Complete(context.Background(), Request{}); err == nil {
		t.Error("Complete() accepted a request with no messages")
	}
}

func TestOpenRouterRequiresAPIKey(t *testing.T) {
	if _, err := NewOpenRouterLLM(""); err == nil {
		t.Error("NewOpenRouterLLM accepted an empty key")
	}
}

// Sticky routing only works when session_id is actually sent, and there is no
// chi request in a direct unit test — this only proves the field is wired to
// requestSessionID, not that it reads a real request ID (that needs a live
// HTTP request, which internal/qa's handler tests exercise indirectly).
func TestOpenRouterOmitsSessionIDOutsideAnHTTPRequest(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	llm, _ := NewOpenRouterLLM("test-key", WithOpenRouterBaseURL(server.URL))
	if _, _, err := llm.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete() = %v", err)
	}

	if _, ok := gotBody["session_id"]; ok {
		t.Error("session_id was sent even though ctx carries no chi request id")
	}
}

func TestOpenRouterDefaultModel(t *testing.T) {
	llm, _ := NewOpenRouterLLM("test-key")
	if llm.Model() != OpenRouterDefaultModel {
		t.Errorf("Model() = %q, want the default %q", llm.Model(), OpenRouterDefaultModel)
	}

	overridden, _ := NewOpenRouterLLM("test-key", WithOpenRouterModel("openai/gpt-4o"))
	if overridden.Model() != "openai/gpt-4o" {
		t.Errorf("Model() = %q, want the override", overridden.Model())
	}
}

// ---------------------------------------------------------------------------
// FallbackLLM
// ---------------------------------------------------------------------------

// stubFallbackLLM is a minimal LLM for exercising FallbackLLM without a real
// provider on either side.
type stubFallbackLLM struct {
	model       string
	streamErr   error
	completeErr error
	calls       int
}

func (s *stubFallbackLLM) Model() string { return s.model }

func (s *stubFallbackLLM) Stream(context.Context, Request) (Stream, error) {
	s.calls++
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	return noopStream{}, nil // never read in these tests; only the error path is
}

// noopStream satisfies Stream without producing anything — these tests only
// check whether Stream() itself errors, never what a successful stream yields.
type noopStream struct{}

func (noopStream) Next() bool   { return false }
func (noopStream) Event() Event { return Event{} }
func (noopStream) Err() error   { return nil }
func (noopStream) Close() error { return nil }

func (s *stubFallbackLLM) Complete(context.Context, Request) (string, *Usage, error) {
	s.calls++
	if s.completeErr != nil {
		return "", nil, s.completeErr
	}
	return s.model + " answer", nil, nil
}

func TestFallbackLLMUsesPrimaryWhenItSucceeds(t *testing.T) {
	primary := &stubFallbackLLM{model: "primary"}
	secondary := &stubFallbackLLM{model: "secondary"}

	llm := NewFallbackLLM(primary, secondary, testLogger())
	text, _, err := llm.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if text != "primary answer" {
		t.Errorf("text = %q, want the primary's answer", text)
	}
	if secondary.calls != 0 {
		t.Error("secondary was called even though the primary succeeded")
	}
}

func TestFallbackLLMFallsBackWhenPrimaryFailsOutright(t *testing.T) {
	primary := &stubFallbackLLM{model: "primary", completeErr: errors.New("quota exhausted")}
	secondary := &stubFallbackLLM{model: "secondary"}

	llm := NewFallbackLLM(primary, secondary, testLogger())
	text, _, err := llm.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Complete() = %v, want the fallback to have covered the failure", err)
	}
	if text != "secondary answer" {
		t.Errorf("text = %q, want the secondary's answer", text)
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Errorf("primary calls = %d, secondary calls = %d, want 1 and 1", primary.calls, secondary.calls)
	}
}

func TestFallbackLLMStreamFallsBackOnAnUpfrontError(t *testing.T) {
	primary := &stubFallbackLLM{model: "primary", streamErr: errors.New("quota exhausted")}
	secondary := &stubFallbackLLM{model: "secondary"}

	llm := NewFallbackLLM(primary, secondary, testLogger())
	if _, err := llm.Stream(context.Background(), Request{}); err != nil {
		t.Fatalf("Stream() = %v, want the fallback to have covered the failure", err)
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Errorf("primary calls = %d, secondary calls = %d, want 1 and 1", primary.calls, secondary.calls)
	}
}

func TestFallbackLLMReportsBothModelsInItsLabel(t *testing.T) {
	llm := NewFallbackLLM(&stubFallbackLLM{model: "gemini-3.5-flash"}, &stubFallbackLLM{model: "openrouter/free"}, testLogger())
	label := llm.Model()
	if !strings.Contains(label, "gemini-3.5-flash") || !strings.Contains(label, "openrouter/free") {
		t.Errorf("label = %q, want both model names", label)
	}
}
