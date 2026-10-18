package ingestion

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

const (
	// DefaultBatchSize is how many texts go in one embedding request. OpenAI
	// accepts more, but larger batches make a single transient failure
	// expensive to retry.
	DefaultBatchSize = 96

	// maxAttempts is the number of tries per batch, including the first.
	//
	// Sized for a quota window, not a network blip. Gemini's free embedding tier
	// allows 100 texts per minute, so a large document spends most of its
	// ingestion waiting for the window to roll over — and a run that gives up
	// after a few seconds simply cannot ingest a legal code at all.
	maxAttempts = 8

	// baseBackoff is the first retry delay when the provider does not say how
	// long to wait; it doubles per attempt.
	baseBackoff = time.Second

	// maxBackoff caps the guessed delay. A provider-specified delay is honoured
	// in full and is not subject to this.
	maxBackoff = 30 * time.Second

	// quotaWindow is the floor for a rate-limit rejection that does not say how
	// long to wait.
	//
	// Providers state the delay most of the time but not always, and a 429 is
	// window-based by nature — Gemini's embedding quota is per minute. Backing
	// off from a second in that situation burns every attempt inside a single
	// window and fails an ingest that only needed to wait.
	quotaWindow = time.Minute
)

// errQuotaPacing marks a wait that is deliberate pacing rather than a rejection,
// so the log can say which.
var errQuotaPacing = errors.New("staying inside the embedding quota")

// retryable is implemented by provider errors that know whether retrying could
// help.
type retryable interface {
	Retryable() bool
}

// delayed is implemented by provider errors that state how long to wait.
type delayed interface {
	// RetryDelay returns the provider's requested wait, or zero if it gave none.
	RetryDelay() time.Duration
}

// limited is implemented by provider errors that distinguish a rate limit from a
// server fault.
type limited interface {
	RateLimited() bool
}

// BatchEmbedder adds batching and retries around an ai.Embedder.
//
// Ingestion embeds a whole document at once — hundreds of chunks — and a single
// rate-limit response part-way through would otherwise abandon the run.
type BatchEmbedder struct {
	embedder  ai.Embedder
	batchSize int
	sleep     func(context.Context, time.Duration) error

	// onWait is called before each retry sleep. Ingestion sets it so a run that
	// is merely waiting on quota looks different in the log from one that is
	// stuck.
	onWait func(attempt int, wait time.Duration, cause error)

	// perMinute is the client-side pacing budget in texts per minute; zero
	// disables it. sent records when each text was submitted, forming the
	// sliding window the budget is measured over.
	//
	// mu guards sent. One BatchEmbedder is shared by every caller of the
	// embedding provider — cmd/api hands the same instance to the ingestion
	// service that serves concurrent requests — and the budget is only a
	// budget if every reservation is counted against the same window.
	mu        sync.Mutex
	perMinute int
	sent      []time.Time
	now       func() time.Time
}

// reserve blocks until sending n more texts stays inside the pacing budget.
func (b *BatchEmbedder) reserve(ctx context.Context, n int) error {
	if b.perMinute <= 0 {
		return nil
	}

	for {
		// The lock covers the read-modify-write of the window and is released
		// before sleeping: holding it across the wait would serialise every
		// other caller behind this one for a full quota window.
		b.mu.Lock()

		now := b.now()
		cutoff := now.Add(-time.Minute)

		// Drop everything that has aged out of the window.
		kept := b.sent[:0]
		for _, t := range b.sent {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		b.sent = kept

		if len(b.sent)+n <= b.perMinute {
			for i := 0; i < n; i++ {
				b.sent = append(b.sent, now)
			}
			b.mu.Unlock()
			return nil
		}

		// Wait until the oldest entry leaves the window, which is the earliest
		// moment the budget can possibly allow this batch.
		wait := b.sent[0].Add(time.Minute).Sub(now) + 100*time.Millisecond
		b.mu.Unlock()

		if b.onWait != nil {
			b.onWait(0, wait, errQuotaPacing)
		}
		if err := b.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// OnWait registers a callback invoked before each retry sleep.
func (b *BatchEmbedder) OnWait(f func(attempt int, wait time.Duration, cause error)) *BatchEmbedder {
	b.onWait = f
	return b
}

// NewBatchEmbedder wraps an embedder.
func NewBatchEmbedder(e ai.Embedder) *BatchEmbedder {
	return &BatchEmbedder{
		embedder:  e,
		batchSize: DefaultBatchSize,
		sleep:     sleepCtx,
		now:       time.Now,
	}
}

// PerMinute paces requests to stay under a provider quota, in texts per minute.
// Zero disables pacing.
//
// Retrying after a 429 is not sufficient on a tight quota, and the arithmetic
// says why: Gemini's free embedding tier allows 100 texts per minute, and a
// default batch is 96. One query embedding from a user in the same window pushes
// the batch over, the retry resubmits all 96 into a window that is already
// short, and a document can fail after minutes of waiting — which is exactly
// what happened to the Amharic commercial code. Staying under the limit on
// purpose turns a fragile retry loop into a predictable slow ingest.
//
// The batch size is also clamped to the budget, since a batch larger than the
// per-minute allowance can never succeed.
func (b *BatchEmbedder) PerMinute(texts int) *BatchEmbedder {
	b.perMinute = texts
	if texts > 0 && texts < b.batchSize {
		b.batchSize = texts
	}
	return b
}

// Model, Version and Dimensions pass through, so callers can record which
// embedding space a vector belongs to without unwrapping.
func (b *BatchEmbedder) Model() string   { return b.embedder.Model() }
func (b *BatchEmbedder) Version() string { return b.embedder.Version() }
func (b *BatchEmbedder) Dimensions() int { return b.embedder.Dimensions() }

// EmbedAll embeds every text, in order.
//
// It is all-or-nothing. A partial result would leave some chunks unindexed and
// therefore permanently unretrievable, with nothing downstream to notice.
func (b *BatchEmbedder) EmbedAll(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += b.batchSize {
		end := min(start+b.batchSize, len(texts))

		if err := b.reserve(ctx, end-start); err != nil {
			return nil, err
		}

		vectors, err := b.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embedding texts %d-%d of %d: %w", start, end-1, len(texts), err)
		}
		if len(vectors) != end-start {
			return nil, fmt.Errorf("embedding texts %d-%d: got %d vectors for %d inputs",
				start, end-1, len(vectors), end-start)
		}
		out = append(out, vectors...)
	}
	return out, nil
}

func (b *BatchEmbedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		vectors, err := b.embedder.Embed(ctx, texts)
		if err == nil {
			return vectors, nil
		}
		lastErr = err

		// A cancelled context is the caller giving up, not a transient fault.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var r retryable
		if errors.As(err, &r) && !r.Retryable() {
			return nil, err
		}
		if attempt == maxAttempts {
			break
		}

		// Prefer the provider's own number. A quota window resets on its clock,
		// and guessing shorter just burns an attempt.
		wait := min(baseBackoff<<(attempt-1), maxBackoff)

		var d delayed
		if errors.As(err, &d) && d.RetryDelay() > 0 {
			wait = d.RetryDelay() + time.Second // a beat past the boundary
		} else {
			var l limited
			if errors.As(err, &l) && l.RateLimited() {
				wait = max(wait, quotaWindow)
			}
		}
		if b.onWait != nil {
			b.onWait(attempt, wait, err)
		}
		if err := b.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
