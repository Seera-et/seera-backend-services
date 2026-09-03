package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// UserAgent identifies this crawler to the Ministry's servers.
//
// Named, with a contact route, because an anonymous crawler hitting a
// government site is the kind of traffic that gets an IP blocked — and because
// whoever runs that site deserves to know who is asking.
const UserAgent = "SeeraLegalBot/1.0 (+https://github.com/Seera-et; Ethiopian legal corpus indexing)"

// ClientConfig tunes crawl politeness and resilience.
//
// The defaults are deliberately slow. This crawler reads a public government
// site that owes it nothing, and the whole corpus is a one-off acquisition
// measured in hours, not seconds. Nothing here should be raised without a
// reason better than impatience.
type ClientConfig struct {
	// RequestsPerSecond is the sustained ceiling across the whole crawl.
	RequestsPerSecond float64

	// Burst allows a short cluster of requests — a law page plus its PDF —
	// without waiting between each.
	Burst int

	// Timeout bounds one request, including body transfer. Generous: some
	// Ministry PDFs are tens of megabytes.
	Timeout time.Duration

	// MaxRetries is how many times a retryable failure is retried, beyond the
	// first attempt.
	MaxRetries int

	// BaseBackoff is the first retry delay; it doubles each attempt.
	BaseBackoff time.Duration

	// MaxBackoff caps the doubling.
	MaxBackoff time.Duration
}

// DefaultClientConfig is the conservative starting point.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		RequestsPerSecond: 0.5, // one request every two seconds
		Burst:             2,
		Timeout:           2 * time.Minute,
		MaxRetries:        4,
		BaseBackoff:       2 * time.Second,
		MaxBackoff:        60 * time.Second,
	}
}

// Client is a polite, retrying HTTP client for one host.
//
// It wraps http.Client rather than replacing it: redirects, connection reuse
// and transparent gzip are the standard library's, already correct, and not
// worth reimplementing.
type Client struct {
	http *http.Client
	lim  *rate.Limiter
	cfg  ClientConfig
	log  *slog.Logger
}

func NewClient(cfg ClientConfig, log *slog.Logger) *Client {
	if cfg.RequestsPerSecond <= 0 {
		cfg.RequestsPerSecond = DefaultClientConfig().RequestsPerSecond
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 1
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultClientConfig().Timeout
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = DefaultClientConfig().BaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultClientConfig().MaxBackoff
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return &Client{
		http: &http.Client{
			Timeout: cfg.Timeout,
			// The default transport already pools connections and negotiates
			// gzip. Only the idle-connection ceiling is worth raising, and only
			// because every request goes to one host.
			Transport: &http.Transport{
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		lim: rate.NewLimiter(rate.Limit(cfg.RequestsPerSecond), cfg.Burst),
		cfg: cfg,
		log: log,
	}
}

// ErrNotRetryable marks a response the crawler should stop asking for: the
// server answered clearly, and asking again will produce the same answer.
var ErrNotRetryable = errors.New("not retryable")

// Get fetches a URL, honouring the rate limit and retrying transient failures.
//
// The caller owns the returned body and must close it. On error the body has
// already been drained and closed.
func (c *Client) Get(ctx context.Context, rawURL string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := c.backoff(attempt, lastErr)
			c.log.DebugContext(ctx, "retrying",
				slog.String("url", rawURL),
				slog.Int("attempt", attempt),
				slog.Duration("after", delay),
				slog.Any("error", lastErr))
			if err := sleep(ctx, delay); err != nil {
				return nil, err
			}
		}

		// Waits for the limiter before every attempt, retries included: a retry
		// is another request to the same server, and backing off in wall-clock
		// terms while ignoring the rate budget would defeat the point.
		if err := c.lim.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter: %w", err)
		}

		resp, err := c.do(ctx, rawURL)
		switch {
		case err == nil:
			return resp, nil
		case errors.Is(err, ErrNotRetryable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, err
		default:
			lastErr = err
		}
	}

	return nil, fmt.Errorf("get %s: gave up after %d attempts: %w", rawURL, c.cfg.MaxRetries+1, lastErr)
}

func (c *Client) do(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		// A URL this client cannot even form a request for will not become
		// valid on the next attempt.
		return nil, fmt.Errorf("build request for %s: %w: %w", rawURL, ErrNotRetryable, err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,*/*")
	req.Header.Set("Accept-Language", "en,am;q=0.9")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", rawURL, err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		// Retryable, and the delay is taken from Retry-After when the server
		// bothered to send one — see backoff.
		return nil, drainf(resp, "%s: rate limited (HTTP 429)", rawURL)

	case resp.StatusCode >= 500:
		// Retryable. This is also the JetElements endpoint's habitual answer,
		// which is why a 500 here is a reason to try the next candidate rather
		// than to fail the document — see Resolver.Resolve.
		return nil, drainf(resp, "%s: server error (HTTP %d)", rawURL, resp.StatusCode)

	default:
		// 4xx other than 429: the server understood and declined.
		return nil, drainf(resp, "%s: HTTP %d: %w", rawURL, resp.StatusCode, ErrNotRetryable)
	}
}

// backoff is exponential, and defers to Retry-After when the server sent one.
func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	if d, ok := retryAfter(lastErr); ok {
		if d > c.cfg.MaxBackoff {
			return c.cfg.MaxBackoff
		}
		return d
	}

	delay := c.cfg.BaseBackoff
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= c.cfg.MaxBackoff {
			return c.cfg.MaxBackoff
		}
	}
	return delay
}

// retryAfterError carries a server's Retry-After hint through the retry loop.
type retryAfterError struct {
	error
	after time.Duration
}

func (e retryAfterError) Unwrap() error { return e.error }

func retryAfter(err error) (time.Duration, bool) {
	var r retryAfterError
	if errors.As(err, &r) && r.after > 0 {
		return r.after, true
	}
	return 0, false
}

// drainf reads and closes a response body, then builds an error describing it.
//
// Draining matters: an unread body holds the connection out of the pool, and
// this client makes every request to one host.
func drainf(resp *http.Response, format string, args ...any) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()

	err := fmt.Errorf(format, args...)
	if resp.StatusCode == http.StatusTooManyRequests {
		if after, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			return retryAfterError{error: err, after: after}
		}
	}
	return err
}

// parseRetryAfter reads the header in its delay-seconds form. The HTTP-date
// form is legal too and is not handled: it does not appear here, and guessing
// at clock skew to support it would be worse than falling back to exponential
// backoff.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// sleep waits, or returns early if the context ends first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
