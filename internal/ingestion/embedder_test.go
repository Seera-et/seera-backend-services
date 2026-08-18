package ingestion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

type scriptedEmbedder struct {
	errs  []error // one per call; nil means succeed
	calls int
	sizes []int
}

func (s *scriptedEmbedder) Model() string   { return "test-model" }
func (s *scriptedEmbedder) Version() string { return "v1" }
func (s *scriptedEmbedder) Dimensions() int { return 4 }

func (s *scriptedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	v, err := s.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func (s *scriptedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	i := s.calls
	s.calls++
	s.sizes = append(s.sizes, len(texts))
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	out := make([][]float32, len(texts))
	for j := range out {
		out[j] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// newTestBatcher replaces the sleep with a recorder, so retry timing is asserted
// rather than waited out.
func newTestBatcher(e ai.Embedder, batchSize int) (*BatchEmbedder, *[]time.Duration) {
	var slept []time.Duration
	b := NewBatchEmbedder(e)
	b.batchSize = batchSize
	b.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return b, &slept
}

// The behaviour that decides whether a legal code can be ingested at all on a
// free tier: the provider states how long its quota window has left, and that
// number must be used instead of a guess.
func TestEmbedAllHonoursProviderRetryDelay(t *testing.T) {
	quota := &ai.HTTPError{
		Provider:   "gemini",
		Status:     429,
		Body:       "Quota exceeded. Please retry in 20.02s.",
		RetryAfter: 20020 * time.Millisecond,
	}
	e := &scriptedEmbedder{errs: []error{quota}}
	b, slept := newTestBatcher(e, 96)

	vectors, err := b.EmbedAll(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedAll() = %v", err)
	}
	if len(vectors) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vectors))
	}

	if len(*slept) != 1 {
		t.Fatalf("slept %d times, want 1", len(*slept))
	}
	// The stated delay plus a beat past the window boundary, not exponential
	// backoff from a second.
	if got := (*slept)[0]; got < 20*time.Second || got > 22*time.Second {
		t.Errorf("waited %v, want the provider's ~20s", got)
	}
}

func TestEmbedAllFallsBackToExponentialBackoff(t *testing.T) {
	// A 500 with no stated delay.
	e := &scriptedEmbedder{errs: []error{
		&ai.HTTPError{Provider: "gemini", Status: 500, Body: "internal"},
		&ai.HTTPError{Provider: "gemini", Status: 500, Body: "internal"},
	}}
	b, slept := newTestBatcher(e, 96)

	if _, err := b.EmbedAll(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("EmbedAll() = %v", err)
	}
	if len(*slept) != 2 {
		t.Fatalf("slept %d times, want 2", len(*slept))
	}
	if (*slept)[1] <= (*slept)[0] {
		t.Errorf("backoff did not grow: %v", *slept)
	}
	if (*slept)[0] > maxBackoff {
		t.Errorf("first backoff %v exceeds the cap", (*slept)[0])
	}
}

func TestEmbedAllStopsOnNonRetryableError(t *testing.T) {
	e := &scriptedEmbedder{errs: []error{
		&ai.HTTPError{Provider: "gemini", Status: 400, Body: "bad request"},
	}}
	b, slept := newTestBatcher(e, 96)

	if _, err := b.EmbedAll(context.Background(), []string{"a"}); err == nil {
		t.Fatal("EmbedAll() retried a 400")
	}
	if len(*slept) != 0 {
		t.Errorf("slept before giving up on a permanent error: %v", *slept)
	}
	if e.calls != 1 {
		t.Errorf("called the provider %d times for a 400, want 1", e.calls)
	}
}

func TestEmbedAllReportsWaits(t *testing.T) {
	e := &scriptedEmbedder{errs: []error{
		&ai.HTTPError{Provider: "gemini", Status: 429, RetryAfter: 5 * time.Second},
	}}
	b, _ := newTestBatcher(e, 96)

	var attempts []int
	b.OnWait(func(attempt int, _ time.Duration, _ error) { attempts = append(attempts, attempt) })

	if _, err := b.EmbedAll(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("EmbedAll() = %v", err)
	}
	if len(attempts) != 1 || attempts[0] != 1 {
		t.Errorf("OnWait attempts = %v, want [1]", attempts)
	}
}

func TestEmbedAllBatchesAndPreservesOrder(t *testing.T) {
	e := &scriptedEmbedder{}
	b, _ := newTestBatcher(e, 2)

	vectors, err := b.EmbedAll(context.Background(), []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatalf("EmbedAll() = %v", err)
	}
	if len(vectors) != 5 {
		t.Fatalf("got %d vectors, want 5", len(vectors))
	}
	if want := []int{2, 2, 1}; len(e.sizes) != 3 ||
		e.sizes[0] != want[0] || e.sizes[1] != want[1] || e.sizes[2] != want[2] {
		t.Errorf("batch sizes = %v, want %v", e.sizes, want)
	}
}

func TestEmbedAllGivesUpAfterMaxAttempts(t *testing.T) {
	errs := make([]error, maxAttempts)
	for i := range errs {
		errs[i] = &ai.HTTPError{Provider: "gemini", Status: 429, RetryAfter: time.Second}
	}
	e := &scriptedEmbedder{errs: errs}
	b, _ := newTestBatcher(e, 96)

	_, err := b.EmbedAll(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("EmbedAll() succeeded despite every attempt failing")
	}
	if !errors.As(err, new(*ai.HTTPError)) {
		t.Errorf("error lost the provider cause: %v", err)
	}
	if e.calls != maxAttempts {
		t.Errorf("made %d attempts, want %d", e.calls, maxAttempts)
	}
}

// A rate limit with no stated delay must be treated as a quota window, not a
// network blip. Backing off from a second burns every attempt inside one window
// and fails an ingest that only needed to wait — which is exactly how the
// Amharic commercial code failed to ingest before this.
func TestEmbedAllAssumesAQuotaWindowWhenTheProviderIsSilent(t *testing.T) {
	e := &scriptedEmbedder{errs: []error{
		&ai.HTTPError{Provider: "gemini", Status: 429, Body: "quota exceeded"}, // no delay stated
	}}
	b, slept := newTestBatcher(e, 96)

	if _, err := b.EmbedAll(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("EmbedAll() = %v", err)
	}
	if len(*slept) != 1 {
		t.Fatalf("slept %d times, want 1", len(*slept))
	}
	if got := (*slept)[0]; got < quotaWindow {
		t.Errorf("waited %v for a rate limit, want at least the %v quota window", got, quotaWindow)
	}
}

// A server fault is not a quota window and should not wait a minute for one.
func TestEmbedAllDoesNotWaitAQuotaWindowForServerFaults(t *testing.T) {
	e := &scriptedEmbedder{errs: []error{
		&ai.HTTPError{Provider: "gemini", Status: 503, Body: "unavailable"},
	}}
	b, slept := newTestBatcher(e, 96)

	if _, err := b.EmbedAll(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("EmbedAll() = %v", err)
	}
	if got := (*slept)[0]; got >= quotaWindow {
		t.Errorf("waited %v for a 503, want the short backoff", got)
	}
}

// Pacing is what makes ingestion possible on a tight quota: 96-text batches
// against a 100/minute limit collide with any other traffic in the window, and
// retrying resubmits the whole batch into a window that is already short.
func TestPerMinutePacesInsteadOfColliding(t *testing.T) {
	e := &scriptedEmbedder{}
	b, slept := newTestBatcher(e, 96)
	b.PerMinute(90)

	// A fake clock, so the sliding window is exercised without waiting.
	base := time.Now()
	elapsed := time.Duration(0)
	b.now = func() time.Time { return base.Add(elapsed) }
	b.sleep = func(_ context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		elapsed += d // the wait actually advances the clock
		return nil
	}

	texts := make([]string, 180)
	for i := range texts {
		texts[i] = "chunk"
	}

	vectors, err := b.EmbedAll(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedAll() = %v", err)
	}
	if len(vectors) != 180 {
		t.Fatalf("got %d vectors, want 180", len(vectors))
	}

	// The batch size is clamped to the budget, so no request can exceed it.
	for i, n := range e.sizes {
		if n > 90 {
			t.Errorf("batch %d had %d texts, over the 90/minute budget", i, n)
		}
	}
	// 180 texts cannot fit in one window, so it must have waited.
	if len(*slept) == 0 {
		t.Error("no pacing wait for 180 texts against a 90/minute budget")
	}
}

func TestPerMinuteZeroDisablesPacing(t *testing.T) {
	e := &scriptedEmbedder{}
	b, slept := newTestBatcher(e, 96)
	b.PerMinute(0)

	texts := make([]string, 300)
	for i := range texts {
		texts[i] = "chunk"
	}
	if _, err := b.EmbedAll(context.Background(), texts); err != nil {
		t.Fatalf("EmbedAll() = %v", err)
	}
	if len(*slept) != 0 {
		t.Errorf("paced despite being disabled: %v", *slept)
	}
}
