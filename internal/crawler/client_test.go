package crawler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testClientConfig is the real configuration with the waiting taken out. The
// retry *counts* are what these tests are about; the delays are the Ministry's
// business, not the test suite's.
func testClientConfig() ClientConfig {
	cfg := DefaultClientConfig()
	cfg.RequestsPerSecond = 1000
	cfg.Burst = 100
	cfg.BaseBackoff = time.Millisecond
	cfg.MaxBackoff = 5 * time.Millisecond
	cfg.Timeout = 5 * time.Second
	return cfg
}

func TestClientRetriesServerErrorsThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "upstream exploded", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	resp, err := NewClient(testClientConfig(), nil).Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := calls.Load(); got != 3 {
		t.Errorf("made %d requests, want 3 (two failures then a success)", got)
	}
}

func TestClientGivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "still broken", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testClientConfig()
	cfg.MaxRetries = 2

	if _, err := NewClient(cfg, nil).Get(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if got, want := calls.Load(), int32(3); got != want {
		t.Errorf("made %d requests, want %d (one attempt plus two retries)", got, want)
	}
}

// A 429 is the Ministry asking to be left alone. Honouring Retry-After is the
// difference between backing off and being blocked.
func TestClientHonoursRetryAfterOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := testClientConfig()
	cfg.MaxBackoff = time.Second // do not clamp the server's own request away

	start := time.Now()
	resp, err := NewClient(cfg, nil).Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("retried after %s, want at least the 1s the server asked for", elapsed)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("made %d requests, want 2", got)
	}
}

// A 404 is a clear answer. Asking four more times is just noise on someone
// else's server.
func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := NewClient(testClientConfig(), nil).Get(context.Background(), srv.URL)
	if !errors.Is(err, ErrNotRetryable) {
		t.Errorf("error = %v, want it to wrap ErrNotRetryable", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("made %d requests for a 404, want 1", got)
	}
}

func TestClientRateLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := testClientConfig()
	cfg.RequestsPerSecond = 20 // 50ms apart
	cfg.Burst = 1
	client := NewClient(cfg, nil)

	start := time.Now()
	for range 3 {
		resp, err := client.Get(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// Three requests at 20/s: the first is free from the burst, the next two
	// wait ~50ms each.
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("three requests took %s, want the rate limiter to have spaced them", elapsed)
	}
}

func TestClientCancellationStopsImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testClientConfig()
	cfg.BaseBackoff = time.Hour // any retry would hang without cancellation

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := NewClient(cfg, nil).Get(ctx, srv.URL); err == nil {
		t.Fatal("expected an error when the context expires mid-backoff")
	}
}
