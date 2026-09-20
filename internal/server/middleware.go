package server

import (
	"context"
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

	"github.com/Seera-et/seera-backend-services/internal/auth"
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
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				// Authorization carries the user's token. Without it named here
				// the browser's preflight fails and every authenticated request
				// dies before it is sent — and it fails only in production,
				// because the Vite dev proxy makes local requests same-origin so
				// no preflight ever happens. seera.site calling api.seera.site
				// is cross-origin and does preflight.
				h.Set("Access-Control-Allow-Headers", "Content-Type, Accept, Authorization")
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

// userVerifier turns a bearer token into a user. Declared here, by the consumer,
// and satisfied by *auth.Verifier — so the server package depends on a method,
// not on how tokens happen to be signed this year.
type userVerifier interface {
	Verify(ctx context.Context, raw string) (auth.User, error)
}

// requireUser rejects a request that does not carry a valid user token.
//
// This is the real access control. The frontend's route guard decides what to
// render; it cannot decide what the API will serve, and anyone can call these
// endpoints directly. In particular /qa/query spends a Gemini call per request,
// so "the UI hides the page" is not a control, it is a suggestion.
//
// The reason a token was rejected is logged and never returned. "Expired",
// "wrong issuer" and "bad signature" each tell a caller which part of their
// guess was right; the client only needs to know to sign in again.
func requireUser(v userVerifier, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				unauthorized(w, r)
				return
			}

			user, err := v.Verify(r.Context(), token)
			if err != nil {
				log.InfoContext(r.Context(), "rejected user token",
					slog.String("path", r.URL.Path),
					slog.Any("error", err))
				unauthorized(w, r)
				return
			}

			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
		})
	}
}

// optionalUser attaches the user when a valid token is present and otherwise
// does nothing. It is for endpoints that are public but behave better when they
// know who is calling — the rate limiter keys on the user id when it has one.
//
// An invalid token is ignored rather than rejected: these routes serve anonymous
// callers by definition, and a stale token in a forgotten tab should degrade to
// anonymous, not to an error page on a public document.
func optionalUser(v userVerifier, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			user, err := v.Verify(r.Context(), token)
			if err != nil {
				log.DebugContext(r.Context(), "ignoring invalid token on public route",
					slog.String("path", r.URL.Path),
					slog.Any("error", err))
				next.ServeHTTP(w, r)
				return
			}

			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", false
	}
	raw = strings.TrimSpace(raw)
	return raw, raw != ""
}

func unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
	writeError(w, r, http.StatusUnauthorized, "unauthorized",
		"Sign in to use this endpoint.")
}

// ipRateLimiter is a per-client token bucket for the endpoints that cost real
// money per call.
//
// The key is the user id when the request carries one and the client IP
// otherwise. Keying on IP alone punishes everyone behind one NAT for each
// other's usage — which in Ethiopia, where mobile carriers NAT aggressively,
// means a shared office can exhaust the limit between them — and lets a
// determined caller reset their bucket by changing address.
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
		if !l.allow(clientKey(r)) {
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusTooManyRequests, "rate_limited",
				"Too many requests. Please wait a moment and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientKey identifies who to charge a request to: the authenticated user if
// there is one, otherwise the client address. The "user:" prefix keeps the two
// namespaces from ever colliding.
func clientKey(r *http.Request) string {
	if user, ok := auth.UserFrom(r.Context()); ok {
		return "user:" + user.ID
	}
	return "ip:" + clientIP(r)
}

// clientIP prefers chi's RealIP result and falls back to the socket address.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
