package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// OpenAIEmbeddingModel is the embedding model the corpus is indexed with.
	OpenAIEmbeddingModel = "text-embedding-3-large"

	// OpenAIEmbeddingDimensions is the requested output size.
	//
	// The model's native width is 3072. It is reduced to 1536 because pgvector
	// cannot build an HNSW index on a `vector` wider than 2000, and an
	// unindexed vector column means sequential scans over the whole corpus.
	// OpenAI's dimension reduction is designed for this and the quality cost is
	// small; the cost of no index is not.
	OpenAIEmbeddingDimensions = 1536

	defaultOpenAIBaseURL = "https://api.openai.com/v1"
)

// OpenAIEmbedder implements Embedder against OpenAI's embeddings endpoint.
//
// It speaks HTTP directly rather than through a vendor SDK: this is one
// endpoint with one request shape, and the dependency would buy nothing.
type OpenAIEmbedder struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// OpenAIOption configures the embedder.
type OpenAIOption func(*OpenAIEmbedder)

// WithOpenAIBaseURL points the client at a different endpoint — a proxy, or a
// test server.
func WithOpenAIBaseURL(url string) OpenAIOption {
	return func(e *OpenAIEmbedder) { e.baseURL = url }
}

// WithOpenAIHTTPClient supplies the HTTP client.
func WithOpenAIHTTPClient(c *http.Client) OpenAIOption {
	return func(e *OpenAIEmbedder) { e.client = c }
}

// NewOpenAIEmbedder builds the embedder. It fails on an empty key rather than
// deferring the problem to the first ingestion run.
func NewOpenAIEmbedder(apiKey string, opts ...OpenAIOption) (*OpenAIEmbedder, error) {
	if apiKey == "" {
		return nil, errors.New("openai: API key is required")
	}
	e := &OpenAIEmbedder{
		apiKey:  apiKey,
		baseURL: defaultOpenAIBaseURL,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

func (e *OpenAIEmbedder) Model() string   { return OpenAIEmbeddingModel }
func (e *OpenAIEmbedder) Version() string { return fmt.Sprintf("d%d", OpenAIEmbeddingDimensions) }
func (e *OpenAIEmbedder) Dimensions() int { return OpenAIEmbeddingDimensions }

type openAIEmbeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions"`
	EncodingFormat string   `json:"encoding_format"`
}

type openAIEmbeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// EmbedQuery embeds a search query.
//
// OpenAI's embedding models make no document/query distinction, so this is Embed
// of a single text. The interface still carries it because Gemini's does.
func (e *OpenAIEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// Embed returns one vector per input, in input order.
//
// It is all-or-nothing: a partial result would silently leave chunks
// unretrievable, so any failure fails the whole call and the caller retries.
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	for i, t := range texts {
		if t == "" {
			return nil, fmt.Errorf("openai: input %d is empty", i)
		}
	}

	body, err := json.Marshal(openAIEmbeddingRequest{
		Model:          OpenAIEmbeddingModel,
		Input:          texts,
		Dimensions:     OpenAIEmbeddingDimensions,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body := readSnippet(resp.Body)
		return nil, &HTTPError{
			Provider:   "openai",
			Status:     resp.StatusCode,
			Body:       body,
			RetryAfter: parseRetryAfter(resp.Header, body),
		}
	}

	var parsed openAIEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("openai: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("openai: asked for %d embeddings, got %d", len(texts), len(parsed.Data))
	}

	// The response is documented as ordered, but it carries an explicit index
	// and the cost of trusting the order wrongly is every chunk in the batch
	// being attached to the wrong text — a corpus-wide corruption that no test
	// downstream would catch.
	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("openai: response index %d out of range", d.Index)
		}
		if out[d.Index] != nil {
			return nil, fmt.Errorf("openai: response index %d repeated", d.Index)
		}
		if len(d.Embedding) != OpenAIEmbeddingDimensions {
			return nil, fmt.Errorf("openai: embedding %d has %d dimensions, want %d",
				d.Index, len(d.Embedding), OpenAIEmbeddingDimensions)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("openai: no embedding returned for input %d", i)
		}
	}
	return out, nil
}

// HTTPError is a non-2xx response from a provider. It carries the status so
// callers can decide what is worth retrying.
type HTTPError struct {
	Provider string
	Status   int
	Body     string

	// RetryAfter is how long the provider asked us to wait, when it said so.
	//
	// Worth honouring rather than guessing: a quota-based 429 resets on the
	// provider's clock, not ours, and exponential backoff from a few hundred
	// milliseconds will exhaust its attempts long before a per-minute window
	// rolls over. Gemini's free embedding tier allows 100 texts per minute and
	// reports the exact delay; ignoring it turns a slow ingest into a failed one.
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: http %d: %s", e.Provider, e.Status, e.Body)
}

// Retryable reports whether retrying the same request could plausibly succeed:
// rate limits and server-side faults, not malformed requests or bad keys.
func (e *HTTPError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// RetryDelay returns how long the provider asked us to wait, or zero.
func (e *HTTPError) RetryDelay() time.Duration { return e.RetryAfter }

// RateLimited reports whether this was a quota or rate-limit rejection, as
// opposed to a server fault. The distinction matters to callers choosing how
// long to wait: a rate limit resets on a fixed window, a 500 does not.
func (e *HTTPError) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// readSnippet reads a bounded prefix of an error body. Provider errors can be
// long, and this text ends up in logs.
func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 4096))
	return string(bytes.TrimSpace(b))
}

// retryDelayPattern matches the delay Google states in a quota error, e.g.
// "Please retry in 20.024757244s." It is also carried in a structured RetryInfo
// detail, but the sentence is present in every form of the response.
var retryDelayPattern = regexp.MustCompile(`retry in ([0-9]+(?:\.[0-9]+)?)s`)

// parseRetryAfter reads the provider's requested wait from the response headers
// or the error body, returning zero when it did not say.
func parseRetryAfter(header http.Header, body string) time.Duration {
	if v := header.Get("Retry-After"); v != "" {
		// Seconds form; the HTTP-date form is not used by these providers.
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	if m := retryDelayPattern.FindStringSubmatch(body); m != nil {
		if secs, err := strconv.ParseFloat(m[1], 64); err == nil && secs >= 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return 0
}
