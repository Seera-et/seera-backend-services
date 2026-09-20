package server

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
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Seera-et/seera-backend-services/internal/auth"
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

// Both paths, because /healthz alone is not reachable in production: Google
// Front End answers that exact path itself on *.run.app and never forwards it,
// so the frontend's connection indicator polls /readyz. If this test loses the
// alias, that indicator reads Offline against a perfectly healthy API.
func TestHealthReportsOKWhenDatabaseReachable(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestServer(t, stubDB{}).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

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
		})
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

// Traces is registered under /admin on its own — it must not require
// ingestion to also be wired, since a deployment can want trace lookup
// without an admin write path into the corpus.
func TestTracesRouteRegistersWithoutIngestion(t *testing.T) {
	srv := New(config.Config{
		Env: "dev", Addr: ":0", MaxRequestBytes: 1 << 20,
		RateLimitRPS: 100, RateLimitBurst: 100,
		AdminToken: "secret",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), stubDB{}, Handlers{
		Traces: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(chi.URLParam(r, "*")))
		}),
	})

	// middleware.RequestID's own format contains a literal slash
	// ("<host>/<short-id>-<counter>") — the route must capture all of it, not
	// silently truncate at the first segment boundary.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/traces/host123/short-000001", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != "host123/short-000001" {
		t.Errorf("body = %q, want the full request_id (with its slash) to have reached the handler", rec.Body.String())
	}
}

// The admin route must stay behind the token guard even when ingestion is
// the handler that's missing and only Traces is wired.
func TestTracesRouteRequiresAdminToken(t *testing.T) {
	srv := New(config.Config{
		Env: "dev", Addr: ":0", MaxRequestBytes: 1 << 20,
		RateLimitRPS: 100, RateLimitBurst: 100,
		AdminToken: "secret",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), stubDB{}, Handlers{
		Traces: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	})

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/traces/req-123", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d without a token", rec.Code, http.StatusUnauthorized)
	}
}

// No admin token configured at all must disable the route entirely, same as
// it already does for ingestion — an unauthenticated admin endpoint must not
// exist even by accident.
func TestTracesRouteDisabledWithoutAdminToken(t *testing.T) {
	srv := New(config.Config{
		Env: "dev", Addr: ":0", MaxRequestBytes: 1 << 20,
		RateLimitRPS: 100, RateLimitBurst: 100,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), stubDB{}, Handlers{
		Traces: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	})

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/traces/req-123", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d — the route must not exist without an admin token", rec.Code, http.StatusNotFound)
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

// --- user authentication -----------------------------------------------------

// stubVerifier accepts exactly one token, so a test can tell "rejected" apart
// from "route not registered" without importing the real JWT machinery.
type stubVerifier struct{}

func (stubVerifier) Verify(_ context.Context, raw string) (auth.User, error) {
	if raw != "good-token" {
		return auth.User{}, auth.ErrUnauthenticated
	}
	return auth.User{ID: "user-1", Email: "abebe@example.com"}, nil
}

func newAuthTestServer(t *testing.T, handlers Handlers) *Server {
	t.Helper()
	handlers.Verifier = stubVerifier{}
	return New(config.Config{
		Env: "dev", Addr: ":0", MaxRequestBytes: 1 << 20,
		CORSAllowedOrigins: []string{"https://seera.site"},
		RateLimitRPS:       100, RateLimitBurst: 100,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), stubDB{}, handlers)
}

func userEchoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.UserFrom(r.Context())
		_, _ = w.Write([]byte(user.ID))
	}
}

// The frontend route guard is presentation. This is the control: /qa/query
// spends a Gemini call per request and is one curl away from anyone.
func TestQARouteRequiresAUser(t *testing.T) {
	srv := newAuthTestServer(t, Handlers{QA: userEchoHandler()})

	for _, tc := range []struct {
		name, header string
		want         int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"not a bearer token", "good-token", http.StatusUnauthorized},
		{"rejected token", "Bearer nonsense", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"valid token", "Bearer good-token", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/qa/query", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The authenticated handler must be able to see who is calling.
func TestAuthenticatedHandlerReceivesTheUser(t *testing.T) {
	srv := newAuthTestServer(t, Handlers{QA: userEchoHandler()})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/qa/query", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Body.String() != "user-1" {
		t.Errorf("handler saw user %q, want %q", rec.Body.String(), "user-1")
	}
}

// Same posture as the admin token: no verifier means the route does not exist,
// rather than existing and serving everyone.
func TestAuthenticatedRoutesDisabledWithoutAVerifier(t *testing.T) {
	srv := New(config.Config{
		Env: "dev", Addr: ":0", MaxRequestBytes: 1 << 20,
		RateLimitRPS: 100, RateLimitBurst: 100,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), stubDB{}, Handlers{
		QA: userEchoHandler(),
		Me: userEchoHandler(),
	})

	for _, path := range []string{"/api/v1/qa/query", "/api/v1/me"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 — an unauthenticatable route must not exist",
				path, rec.Code)
		}
	}
}

// Browsing published law needs no account. A visitor has to be able to see the
// corpus before deciding whether to sign up for it.
func TestCorpusRoutesStayPublic(t *testing.T) {
	srv := newAuthTestServer(t, Handlers{
		Documents:   chiSubrouter(),
		CorpusStats: userEchoHandler(),
	})

	for _, path := range []string{"/api/v1/documents", "/api/v1/corpus/stats"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d without a token, want 200", path, rec.Code)
		}
	}
}

// A stale token in a forgotten tab must degrade a public page to anonymous,
// not break it.
func TestPublicRoutesIgnoreAnInvalidToken(t *testing.T) {
	srv := newAuthTestServer(t, Handlers{CorpusStats: userEchoHandler()})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/corpus/stats", nil)
	req.Header.Set("Authorization", "Bearer expired-nonsense")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — an invalid token on a public route "+
			"should be ignored, not rejected", rec.Code)
	}
}

// Invisible locally (the Vite proxy makes dev same-origin, so no preflight
// happens) and fatal in production, where seera.site calls api.seera.site.
func TestCORSAllowsTheAuthorizationHeader(t *testing.T) {
	srv := newAuthTestServer(t, Handlers{QA: userEchoHandler()})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/qa/query", nil)
	req.Header.Set("Origin", "https://seera.site")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	allowed := rec.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(strings.ToLower(allowed), "authorization") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to include Authorization "+
			"or every authenticated cross-origin request fails preflight", allowed)
	}
}
