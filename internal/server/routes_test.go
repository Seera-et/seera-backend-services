package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Seera-et/seera-backend-services/internal/config"
)

type stubDB struct{ err error }

func (s stubDB) Health(context.Context) error { return s.err }

func newTestServer(t *testing.T, db healthChecker) *Server {
	t.Helper()
	return New(config.Config{
		Env:                "dev",
		Addr:               ":0",
		CORSAllowedOrigins: []string{"http://localhost:5173"},
		MaxRequestBytes:    1 << 20,
		RateLimitRPS:       100,
		RateLimitBurst:     100,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), db, Handlers{})
}

func TestHealthReportsOKWhenDatabaseReachable(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t, stubDB{}).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if got.Status != "ok" || got.DB != "up" {
		t.Errorf("body = %+v, want {ok up}", got)
	}
}

// A process that cannot reach Postgres cannot answer legal questions, so it
// must fail its probe rather than report itself healthy.
func TestHealthReportsDegradedWhenDatabaseDown(t *testing.T) {
	rec := httptest.NewRecorder()
	srv := newTestServer(t, stubDB{err: errors.New("connection refused")})
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var got healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if got.Status != "degraded" || got.DB != "down" {
		t.Errorf("body = %+v, want {degraded down}", got)
	}
}

// The frontend parses exactly one error shape.
func TestUnknownRouteReturnsStructuredError(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t, stubDB{}).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var got errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if got.Error.Code != "not_found" {
		t.Errorf("error code = %q, want %q", got.Error.Code, "not_found")
	}
	if got.Error.RequestID == "" {
		t.Error("error carries no request_id; failures would be untraceable in logs")
	}
}

func TestCORSOnlyEchoesAllowedOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, origin, want string
	}{
		{"allowed", "http://localhost:5173", "http://localhost:5173"},
		{"not allowed", "https://evil.example", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			newTestServer(t, stubDB{}).routes().ServeHTTP(rec, req)

			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.want {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRateLimiterAllowsBurstThenRejects(t *testing.T) {
	l := newIPRateLimiter(0.0001, 2) // effectively no refill during the test

	if !l.allow("1.2.3.4") || !l.allow("1.2.3.4") {
		t.Fatal("burst of 2 should be allowed")
	}
	if l.allow("1.2.3.4") {
		t.Error("third request within the burst window should be rejected")
	}
	if !l.allow("5.6.7.8") {
		t.Error("a different client must not be throttled by another client's usage")
	}
}

// The bucket map is keyed by client IP on a public endpoint, so it has to be
// bounded or it is a slow memory leak.
func TestRateLimiterEvictsIdleClients(t *testing.T) {
	l := newIPRateLimiter(1, 1)
	l.allow("1.2.3.4")

	l.evict(time.Hour)
	if len(l.buckets) != 1 {
		t.Errorf("recently seen client was evicted: %d buckets remain", len(l.buckets))
	}

	l.evict(-time.Second) // treat everything as idle
	if len(l.buckets) != 0 {
		t.Errorf("idle client was not evicted: %d buckets remain", len(l.buckets))
	}
}

// newTestServerWith mounts specific handlers, for the registration tests below.
func newTestServerWith(t *testing.T, handlers Handlers) *Server {
	t.Helper()
	return New(config.Config{
		Env:                "dev",
		Addr:               ":0",
		CORSAllowedOrigins: []string{"http://localhost:5173"},
		MaxRequestBytes:    1 << 20,
		RateLimitRPS:       100,
		RateLimitBurst:     100,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), stubDB{}, handlers)
}

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
}

// A catalogue that is not wired must 404 rather than answer emptily: "no
// documents" and "browsing is not enabled" are different facts.
func TestCatalogueRoutesAreAbsentUntilWired(t *testing.T) {
	srv := newTestServer(t, stubDB{})

	for _, path := range []string{
		"/api/v1/documents",
		"/api/v1/documents/abc",
		"/api/v1/corpus/stats",
	} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 while the handler is nil", path, rec.Code)
		}
	}
}

func TestCatalogueRoutesMountWhenWired(t *testing.T) {
	docs := chiSubrouter()
	srv := newTestServerWith(t, Handlers{
		Documents:   docs,
		CorpusStats: okHandler(`{"documents":1}`),
	})

	cases := []struct {
		path string
		want string
	}{
		{"/api/v1/documents", "list"},
		{"/api/v1/documents/abc", "detail"},
		{"/api/v1/documents/abc/articles", "articles"},
		{"/api/v1/corpus/stats", `{"documents":1}`},
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", tc.path, rec.Code)
		}
		if got := rec.Body.String(); got != tc.want {
			t.Errorf("GET %s served %q, want %q", tc.path, got, tc.want)
		}
	}
}

// chiSubrouter mirrors the shape documents.Handler.Routes() returns, so the
// mount point is tested without depending on that package.
func chiSubrouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("list")) })
	r.Get("/{id}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("detail")) })
	r.Get("/{id}/articles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("articles"))
	})
	return r
}
