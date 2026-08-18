// Package rag orchestrates the answer path: retrieve, filter, rerank, build
// context, generate, validate grounding, stream.
//
// It never talks to the database directly — it goes through internal/search and
// internal/citations.
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/search"
)

// DefaultTopN is how many chunks survive reranking and reach the generator.
//
// Retrieve wide, rerank, pass few. Handing fifty chunks to the model degrades
// grounding — the relevant provision competes with forty-odd near-misses — and
// costs tokens for the privilege.
const DefaultTopN = 6

// Reranker reorders retrieved candidates by relevance to the question.
type Reranker interface {
	// Rerank returns the topN most relevant results, best first. It must not
	// invent, merge, or alter results — only select and reorder them.
	Rerank(ctx context.Context, query string, results []search.Result, topN int) ([]search.Result, error)

	// Name identifies the reranker in logs and evaluation reports.
	Name() string
}

// PassthroughReranker keeps fusion order and truncates.
//
// It exists so the evaluation harness can measure retrieval with and without a
// reranker and put a number on what the reranker is worth. A dependency that
// costs money per query should have to justify itself.
type PassthroughReranker struct{}

func (PassthroughReranker) Name() string { return "passthrough" }

func (PassthroughReranker) Rerank(_ context.Context, _ string, results []search.Result, topN int) ([]search.Result, error) {
	if topN > 0 && len(results) > topN {
		results = results[:topN]
	}
	return results, nil
}

// CohereRerankModel is the cross-encoder used for reranking.
//
// Multilingual matters more than raw English quality here: half the corpus is
// Amharic. Verify the model is current against Cohere's model list before
// changing it — a wrong name fails at request time, not at startup.
const CohereRerankModel = "rerank-v3.5"

const defaultCohereBaseURL = "https://api.cohere.com/v2"

// CohereReranker reranks with Cohere's hosted cross-encoder.
type CohereReranker struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// CohereOption configures the reranker.
type CohereOption func(*CohereReranker)

// WithCohereBaseURL points the client at a different endpoint — a proxy, or a
// test server.
func WithCohereBaseURL(url string) CohereOption {
	return func(r *CohereReranker) { r.baseURL = url }
}

// WithCohereModel overrides the model.
func WithCohereModel(model string) CohereOption {
	return func(r *CohereReranker) { r.model = model }
}

// WithCohereHTTPClient supplies the HTTP client.
func WithCohereHTTPClient(c *http.Client) CohereOption {
	return func(r *CohereReranker) { r.client = c }
}

// NewCohereReranker builds the reranker. It fails on an empty key rather than
// deferring the problem to the first question a user asks.
func NewCohereReranker(apiKey string, opts ...CohereOption) (*CohereReranker, error) {
	if apiKey == "" {
		return nil, errors.New("cohere: API key is required")
	}
	r := &CohereReranker{
		apiKey:  apiKey,
		model:   CohereRerankModel,
		baseURL: defaultCohereBaseURL,
		// Reranking sits between retrieval and the first token, inside a
		// time-to-first-token budget measured in seconds.
		client: &http.Client{Timeout: 10 * time.Second},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

func (r *CohereReranker) Name() string { return "cohere:" + r.model }

type cohereRerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

type cohereRerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
	Message string `json:"message"`
}

// Rerank scores each candidate against the query and returns the best topN.
func (r *CohereReranker) Rerank(ctx context.Context, query string, results []search.Result, topN int) ([]search.Result, error) {
	if len(results) == 0 {
		return nil, nil
	}
	if topN <= 0 || topN > len(results) {
		topN = len(results)
	}

	// The reranker scores the contextualised text — the same form that was
	// embedded and indexed. A bare span without its document and article header
	// is as ambiguous to a cross-encoder as it is to an embedding model.
	documents := make([]string, len(results))
	for i, res := range results {
		documents[i] = rerankDocument(res)
	}

	body, err := json.Marshal(cohereRerankRequest{
		Model:     r.model,
		Query:     query,
		Documents: documents,
		TopN:      topN,
	})
	if err != nil {
		return nil, fmt.Errorf("cohere: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cohere: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cohere: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("cohere: http %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var parsed cohereRerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("cohere: decode response: %w", err)
	}

	out := make([]search.Result, 0, len(parsed.Results))
	for _, scored := range parsed.Results {
		// An out-of-range index would silently attach a relevance score to the
		// wrong provision, so it fails the request instead.
		if scored.Index < 0 || scored.Index >= len(results) {
			return nil, fmt.Errorf("cohere: result index %d out of range for %d candidates",
				scored.Index, len(results))
		}
		result := results[scored.Index]
		result.Score = scored.RelevanceScore
		out = append(out, result)
	}
	return out, nil
}

// rerankDocument renders a result for the cross-encoder.
func rerankDocument(r search.Result) string {
	var b bytes.Buffer
	b.WriteString(r.DocumentTitle)
	b.WriteString("\nArticle ")
	b.WriteString(r.ArticleNo)
	if r.ArticleTitle != "" {
		b.WriteString(" — ")
		b.WriteString(r.ArticleTitle)
	}
	b.WriteString("\n\n")
	b.WriteString(r.Text)
	return b.String()
}
