package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gemini is used for both generation and embeddings, so one key covers the whole
// answer path.
const (
	// GeminiDefaultModel is chosen on measured time-to-first-token, which is a
	// product requirement (< 3s). Measured against this corpus on 2026-08-17,
	// same prompt and sources each time:
	//
	//	gemini-3.5-flash        2.1s   ← chosen
	//	gemini-3.1-flash-lite   3.3s
	//	gemini-3.6-flash       52.9s
	//	gemini-3.7-flash       503 Service Unavailable
	//
	// The newest model is not the best model for this path. Generation here is
	// retrieval-bound, not reasoning-bound: the law is already in the context, and
	// the job is to summarise it faithfully and cite it. The heavier thinking
	// models spend tens of seconds on reasoning that buys nothing here.
	//
	// Setting thinkingConfig.thinkingLevel to "low" made 3.5-flash *slower*
	// (3.0s), so it is deliberately left unset.
	//
	// Pinned to an explicit version rather than the gemini-flash-latest alias:
	// Phase 4 measures grounding against a baseline, and a moving alias would
	// change the model underneath a measured number. Re-run the measurement when
	// bumping this — and note the 2.5 family 404s for keys created after its
	// retirement.
	GeminiDefaultModel = "gemini-3.5-flash"

	GeminiEmbeddingModel = "gemini-embedding-001"

	// GeminiEmbeddingDimensions keeps vectors under pgvector's 2000-dimension
	// ceiling for HNSW indexes. The model's native width is 3072.
	GeminiEmbeddingDimensions = 1536

	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"
)

// GeminiOption configures either Gemini client.
type GeminiOption func(*geminiClient)

// WithGeminiBaseURL points the client at a different endpoint — a proxy, or a
// test server.
func WithGeminiBaseURL(u string) GeminiOption {
	return func(c *geminiClient) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithGeminiHTTPClient supplies the HTTP client.
func WithGeminiHTTPClient(h *http.Client) GeminiOption {
	return func(c *geminiClient) { c.http = h }
}

// WithGeminiModel overrides the model.
func WithGeminiModel(model string) GeminiOption {
	return func(c *geminiClient) { c.model = model }
}

// geminiClient holds what both the LLM and the embedder need.
type geminiClient struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// url builds an endpoint URL with the key as a query parameter.
//
// The key goes in the query string because that is the only auth this API
// accepts, which means it must never be logged: every error below is built from
// the response, never from the request URL.
func (c *geminiClient) url(method string, query map[string]string) string {
	u := fmt.Sprintf("%s/models/%s:%s", c.baseURL, c.model, method)
	values := url.Values{"key": {c.apiKey}}
	for k, v := range query {
		values.Set(k, v)
	}
	return u + "?" + values.Encode()
}

func (c *geminiClient) post(ctx context.Context, endpoint string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gemini: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("gemini: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Deliberately not wrapping err's URL, which carries the API key.
		return nil, fmt.Errorf("gemini: request failed: %w", scrubURLError(err))
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		body := readSnippet(resp.Body)
		return nil, &HTTPError{
			Provider:   "gemini",
			Status:     resp.StatusCode,
			Body:       body,
			RetryAfter: parseRetryAfter(resp.Header, body),
		}
	}
	return resp, nil
}

// scrubURLError removes the request URL from transport errors, because it
// contains the API key as a query parameter.
func scrubURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

// ---------------------------------------------------------------------------
// Generation
// ---------------------------------------------------------------------------

// GeminiLLM implements LLM against Gemini's streaming generate endpoint.
type GeminiLLM struct {
	client *geminiClient
}

// NewGeminiLLM builds the generation client.
func NewGeminiLLM(apiKey string, opts ...GeminiOption) (*GeminiLLM, error) {
	if apiKey == "" {
		return nil, errors.New("gemini: API key is required")
	}
	c := &geminiClient{
		apiKey:  apiKey,
		model:   GeminiDefaultModel,
		baseURL: defaultGeminiBaseURL,
		// No overall timeout: a streaming answer is long-lived by design, and an
		// absolute deadline would cut it mid-sentence. Cancellation comes from
		// the request context.
		http: &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return &GeminiLLM{client: c}, nil
}

func (g *GeminiLLM) Model() string { return g.client.model }

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiGenerateRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`

	// ResponseMIMEType and ResponseSchema are Gemini's structured-output
	// controls. Set together: the schema is ignored unless the MIME type asks
	// for JSON.
	ResponseMIMEType string  `json:"responseMimeType,omitempty"`
	ResponseSchema   *Schema `json:"responseSchema,omitempty"`
}

// buildRequest is shared by Stream and Complete, so the two cannot drift in how
// they translate a Request.
func buildRequest(req Request) (geminiGenerateRequest, error) {
	body := geminiGenerateRequest{}
	body.GenerationConfig.MaxOutputTokens = req.MaxTokens

	if req.JSONSchema != nil {
		body.GenerationConfig.ResponseMIMEType = "application/json"
		body.GenerationConfig.ResponseSchema = req.JSONSchema
	}

	if req.System != "" {
		body.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: req.System}}}
	}
	for _, m := range req.Messages {
		role := "user"
		if m.Role == RoleAssistant {
			role = "model"
		}
		body.Contents = append(body.Contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: m.Content}},
		})
	}
	if len(body.Contents) == 0 {
		return geminiGenerateRequest{}, errors.New("gemini: request has no messages")
	}
	return body, nil
}

// geminiResponse is the non-streaming shape.
type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// Complete returns the whole response in one call.
//
// Used where the output is read by code before anything reaches the user: a
// routing decision, or a reply that has to pass a check before it is shown.
func (g *GeminiLLM) Complete(ctx context.Context, req Request) (string, *Usage, error) {
	body, err := buildRequest(req)
	if err != nil {
		return "", nil, err
	}

	resp, err := g.client.post(ctx, g.client.url("generateContent", nil), body)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", nil, fmt.Errorf("gemini: decode response: %w", err)
	}

	if decoded.PromptFeedback != nil && decoded.PromptFeedback.BlockReason != "" {
		return "", nil, fmt.Errorf("gemini: prompt blocked (%s)", decoded.PromptFeedback.BlockReason)
	}
	if len(decoded.Candidates) == 0 {
		return "", nil, errors.New("gemini: response has no candidates")
	}

	var text strings.Builder
	for _, part := range decoded.Candidates[0].Content.Parts {
		text.WriteString(part.Text)
	}

	usage := &Usage{
		InputTokens:  decoded.UsageMetadata.PromptTokenCount,
		OutputTokens: decoded.UsageMetadata.CandidatesTokenCount,
	}
	return text.String(), usage, nil
}

type geminiStreamChunk struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Stream generates an answer, streaming text as it arrives.
func (g *GeminiLLM) Stream(ctx context.Context, req Request) (Stream, error) {
	body, err := buildRequest(req)
	if err != nil {
		return nil, err
	}

	// alt=sse asks for Server-Sent Events; without it the endpoint returns a
	// single JSON array that only arrives once generation is complete, which
	// defeats the point of streaming.
	endpoint := g.client.url("streamGenerateContent", map[string]string{"alt": "sse"})

	resp, err := g.client.post(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}

	return &geminiStream{resp: resp, scanner: bufio.NewScanner(resp.Body)}, nil
}

type geminiStream struct {
	resp    *http.Response
	scanner *bufio.Scanner
	current Event
	err     error
	usage   *Usage
	stop    StopReason
	done    bool
}

func (s *geminiStream) Next() bool {
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

		var chunk geminiStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			s.err = fmt.Errorf("gemini: decode stream chunk: %w", err)
			return false
		}
		if chunk.Error != nil {
			s.err = fmt.Errorf("gemini: %s", chunk.Error.Message)
			return false
		}

		for _, candidate := range chunk.Candidates {
			if candidate.FinishReason != "" {
				s.stop = ParseStopReason(candidate.FinishReason)
			}
		}

		if chunk.UsageMetadata.CandidatesTokenCount > 0 {
			s.usage = &Usage{
				InputTokens:  chunk.UsageMetadata.PromptTokenCount,
				OutputTokens: chunk.UsageMetadata.CandidatesTokenCount,
			}
		}

		var text strings.Builder
		for _, candidate := range chunk.Candidates {
			for _, part := range candidate.Content.Parts {
				text.WriteString(part.Text)
			}
		}
		if text.Len() == 0 {
			continue // a chunk carrying only usage or a finish reason
		}

		s.current = Event{Type: EventText, Text: text.String()}
		return true
	}

	if err := s.scanner.Err(); err != nil {
		s.err = fmt.Errorf("gemini: read stream: %w", err)
		return false
	}

	s.done = true
	s.current = Event{Type: EventDone, Usage: s.usage, Stop: s.stop}
	return true
}

func (s *geminiStream) Event() Event { return s.current }
func (s *geminiStream) Err() error   { return s.err }

func (s *geminiStream) Close() error {
	_, _ = io.Copy(io.Discard, s.resp.Body)
	return s.resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Embeddings
// ---------------------------------------------------------------------------

// GeminiEmbedder implements Embedder against Gemini's embedding endpoint.
type GeminiEmbedder struct {
	client *geminiClient
}

// NewGeminiEmbedder builds the embedding client.
func NewGeminiEmbedder(apiKey string, opts ...GeminiOption) (*GeminiEmbedder, error) {
	if apiKey == "" {
		return nil, errors.New("gemini: API key is required")
	}
	c := &geminiClient{
		apiKey:  apiKey,
		model:   GeminiEmbeddingModel,
		baseURL: defaultGeminiBaseURL,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return &GeminiEmbedder{client: c}, nil
}

func (e *GeminiEmbedder) Model() string   { return e.client.model }
func (e *GeminiEmbedder) Dimensions() int { return GeminiEmbeddingDimensions }

// Version distinguishes vectors that are not comparable with each other. The
// output width is part of that identity: a 1536-dimension vector from this model
// is not interchangeable with a 3072-dimension one.
func (e *GeminiEmbedder) Version() string {
	return fmt.Sprintf("d%d", GeminiEmbeddingDimensions)
}

// Task types tell the model what the text is for. Gemini embeds a document and
// a question about that document into deliberately different spaces, and using
// the wrong one measurably costs retrieval accuracy — this is free quality that
// only requires saying which is which.
const (
	geminiTaskDocument = "RETRIEVAL_DOCUMENT"
	geminiTaskQuery    = "RETRIEVAL_QUERY"
)

type geminiEmbedRequest struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	TaskType             string        `json:"taskType"`
	OutputDimensionality int           `json:"outputDimensionality"`
}

type geminiBatchEmbedRequest struct {
	Requests []geminiEmbedRequest `json:"requests"`
}

type geminiBatchEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed embeds texts as documents, in input order.
func (e *GeminiEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, texts, geminiTaskDocument)
}

// EmbedQuery embeds a search query.
func (e *GeminiEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.embed(ctx, []string{text}, geminiTaskQuery)
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (e *GeminiEmbedder) embed(ctx context.Context, texts []string, taskType string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	requests := make([]geminiEmbedRequest, 0, len(texts))
	for i, text := range texts {
		if text == "" {
			return nil, fmt.Errorf("gemini: input %d is empty", i)
		}
		requests = append(requests, geminiEmbedRequest{
			Model:                "models/" + e.client.model,
			Content:              geminiContent{Parts: []geminiPart{{Text: text}}},
			TaskType:             taskType,
			OutputDimensionality: GeminiEmbeddingDimensions,
		})
	}

	resp, err := e.client.post(ctx, e.client.url("batchEmbedContents", nil),
		geminiBatchEmbedRequest{Requests: requests})
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var parsed geminiBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("gemini: %s", parsed.Error.Message)
	}
	if len(parsed.Embeddings) != len(texts) {
		return nil, fmt.Errorf("gemini: asked for %d embeddings, got %d",
			len(texts), len(parsed.Embeddings))
	}

	out := make([][]float32, len(parsed.Embeddings))
	for i, embedding := range parsed.Embeddings {
		if len(embedding.Values) != GeminiEmbeddingDimensions {
			return nil, fmt.Errorf("gemini: embedding %d has %d dimensions, want %d",
				i, len(embedding.Values), GeminiEmbeddingDimensions)
		}
		out[i] = normalize(embedding.Values)
	}
	return out, nil
}

// normalize scales a vector to unit length.
//
// Gemini's embeddings are only normalised at their native 3072 dimensions; a
// truncated output is not, and Google's guidance is to normalise it yourself.
// Cosine distance is scale-invariant so ranking is unaffected either way, but
// unit vectors make L2 and cosine agree, which keeps the door open to an index
// that uses either.
func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return v
	}
	norm := float32(math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = f / norm
	}
	return out
}
