package server

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/time/rate"
)

// requestLogger logs one line per request, after it completes.
//
// It records latency and status but never the request body or any response
// content: this service handles legal document text, and application logs are
// not the place for it.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				// Health checks run constantly and say nothing useful.
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			log.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
		})
	}
}

// recoverer turns a panic into a 500 and a log line, keeping the process alive.
//
// A panic mid-stream cannot become a JSON error — the status and part of the
// body are already on the wire — so in that case it just closes the response.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					panic(rec) // the server's own signal; do not swallow it
				}

				log.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
					slog.String("request_id", middleware.GetReqID(r.Context())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)

				if ww, ok := w.(middleware.WrapResponseWriter); ok && ww.Status() != 0 {
					return // response already started; nothing safe to write
				}
				writeError(w, r, http.StatusInternalServerError, "internal_error",
					"Something went wrong on our side.")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// cors answers preflights and tags responses for the browser.
//
// The allow-list is explicit — no wildcard reflection of arbitrary Origins.
func cors(allowed []string) func(http.Handler) http.Handler {
	allowSet := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		allowSet[strings.TrimRight(strings.TrimSpace(o), "/")] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")
			if origin != "" && allowSet[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, Accept")
				h.Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// limitBody caps request bodies. Reading past the limit fails the read rather
// than buffering an unbounded amount of memory.
func limitBody(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireAdminToken gates the ingestion endpoint on a shared bearer token.
//
// Authentication proper is out of MVP scope, but an unauthenticated write path
// into the legal corpus is not an acceptable interim state: anything that can
// write here can put words in the law's mouth. The comparison is
// constant-time so the token cannot be recovered by timing.
func requireAdminToken(token string) func(http.Handler) http.Handler {
	want := []byte(token)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(presented), want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
				writeError(w, r, http.StatusUnauthorized, "unauthorized",
					"A valid admin token is required.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ipRateLimiter is a per-client token bucket, for the endpoints that cost real
// money per call. Unauthenticated by design (auth is out of MVP scope), so the
// client key is its IP.
type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rps     rate.Limit
	burst   int
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPRateLimiter(rps float64, burst int) *ipRateLimiter {
	return &ipRateLimiter{
		buckets: make(map[string]*bucket),
		rps:     rate.Limit(rps),
		burst:   burst,
	}
}

func (l *ipRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[ip] = b
	}
	b.lastSeen = time.Now()
	return b.limiter.Allow()
}

// evict drops buckets that have been idle, so the map cannot grow without bound
// on a public endpoint.
func (l *ipRateLimiter) evict(idleFor time.Duration) {
	cutoff := time.Now().Add(-idleFor)
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
}

// middleware enforces the limit, replying 429 with Retry-After.
func (l *ipRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusTooManyRequests, "rate_limited",
				"Too many requests. Please wait a moment and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP prefers chi's RealIP result and falls back to the socket address.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
