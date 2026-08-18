package ai

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiStreamParsesSSE(t *testing.T) {
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("alt"); got != "sse" {
			t.Errorf("alt = %q, want sse — without it the endpoint returns one JSON "+
				"array after generation finishes, which is not streaming", got)
		}
		if got := r.URL.Query().Get("key"); got != "test-key" {
			t.Errorf("key = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`data: {"candidates":[{"content":{"parts":[{"text":"A company "}]}}]}`,
			`data: {"candidates":[{"content":{"parts":[{"text":"may be formed [S1]."}]}}]}`,
			`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":9}}`,
		} {
			_, _ = w.Write([]byte(line + "\n\n"))
		}
	}))
	defer server.Close()

	llm, err := NewGeminiLLM("test-key", WithGeminiBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewGeminiLLM() = %v", err)
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
	for stream.Next() {
		event := stream.Event()
		switch event.Type {
		case EventText:
			text.WriteString(event.Text)
		case EventDone:
			usage = event.Usage
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

	// The system prompt must travel as systemInstruction, not as another turn:
	// Gemini weights the two differently.
	if _, ok := gotBody["systemInstruction"]; !ok {
		t.Error("system prompt was not sent as systemInstruction")
	}
}

func TestGeminiStreamSurfacesAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer server.Close()

	llm, _ := NewGeminiLLM("test-key", WithGeminiBaseURL(server.URL))
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
	if !httpErr.Retryable() {
		t.Error("a 429 should be retryable")
	}
}

func asHTTPError(err error, target **HTTPError) bool {
	if e, ok := err.(*HTTPError); ok {
		*target = e
		return true
	}
	return false
}

func TestGeminiStreamRejectsAnEmptyRequest(t *testing.T) {
	llm, _ := NewGeminiLLM("test-key")
	if _, err := llm.Stream(context.Background(), Request{}); err == nil {
		t.Error("Stream() accepted a request with no messages")
	}
}

func TestGeminiEmbedderUsesTaskTypes(t *testing.T) {
	var requests []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Requests []map[string]any `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests = body.Requests

		values := make([]float32, GeminiEmbeddingDimensions)
		for i := range values {
			values[i] = 0.5
		}
		out := map[string]any{"embeddings": []map[string]any{}}
		list := make([]map[string]any, len(body.Requests))
		for i := range body.Requests {
			list[i] = map[string]any{"values": values}
		}
		out["embeddings"] = list
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()

	embedder, err := NewGeminiEmbedder("test-key", WithGeminiBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewGeminiEmbedder() = %v", err)
	}

	if _, err := embedder.Embed(context.Background(), []string{"a document"}); err != nil {
		t.Fatalf("Embed() = %v", err)
	}
	if got := requests[0]["taskType"]; got != geminiTaskDocument {
		t.Errorf("document taskType = %v, want %s", got, geminiTaskDocument)
	}
	if got := requests[0]["outputDimensionality"]; got != float64(GeminiEmbeddingDimensions) {
		t.Errorf("outputDimensionality = %v, want %d", got, GeminiEmbeddingDimensions)
	}

	// The distinction that costs retrieval accuracy when it is wrong: Gemini
	// embeds a question and a passage into different spaces.
	if _, err := embedder.EmbedQuery(context.Background(), "a question"); err != nil {
		t.Fatalf("EmbedQuery() = %v", err)
	}
	if got := requests[0]["taskType"]; got != geminiTaskQuery {
		t.Errorf("query taskType = %v, want %s", got, geminiTaskQuery)
	}
}

func TestGeminiEmbedderNormalizesVectors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		values := make([]float32, GeminiEmbeddingDimensions)
		for i := range values {
			values[i] = 3 // deliberately not unit length
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": []map[string]any{{"values": values}},
		})
	}))
	defer server.Close()

	embedder, _ := NewGeminiEmbedder("test-key", WithGeminiBaseURL(server.URL))
	vectors, err := embedder.Embed(context.Background(), []string{"text"})
	if err != nil {
		t.Fatalf("Embed() = %v", err)
	}

	var sum float64
	for _, f := range vectors[0] {
		sum += float64(f) * float64(f)
	}
	if math.Abs(math.Sqrt(sum)-1) > 1e-5 {
		t.Errorf("vector length = %f, want 1 — truncated Gemini embeddings are not "+
			"normalised by the model", math.Sqrt(sum))
	}
}

func TestGeminiEmbedderRejectsWrongDimensions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": []map[string]any{{"values": []float32{0.1, 0.2, 0.3}}},
		})
	}))
	defer server.Close()

	embedder, _ := NewGeminiEmbedder("test-key", WithGeminiBaseURL(server.URL))
	if _, err := embedder.Embed(context.Background(), []string{"text"}); err == nil {
		t.Error("Embed() accepted a vector of the wrong width, which would fail " +
			"far later at INSERT time")
	}
}

func TestGeminiRequiresAPIKey(t *testing.T) {
	if _, err := NewGeminiLLM(""); err == nil {
		t.Error("NewGeminiLLM() accepted an empty key")
	}
	if _, err := NewGeminiEmbedder(""); err == nil {
		t.Error("NewGeminiEmbedder() accepted an empty key")
	}
}

// The key travels as a query parameter, so any error built from the request URL
// would leak it into the logs.
func TestGeminiErrorsDoNotLeakTheAPIKey(t *testing.T) {
	llm, _ := NewGeminiLLM("super-secret-key", WithGeminiBaseURL("http://127.0.0.1:1"))

	_, err := llm.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "q"}},
	})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Errorf("API key leaked into the error: %v", err)
	}
}

var _ Embedder = (*GeminiEmbedder)(nil)
var _ LLM = (*GeminiLLM)(nil)
