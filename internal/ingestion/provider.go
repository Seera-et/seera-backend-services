package ingestion

import (
	"log/slog"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// NewEmbedder picks the embedding provider.
//
// It lives here rather than in a main package because there are now two
// binaries that must embed into the *same* vector space — the API and the
// corpus ingest command — and a second copy of this choice is a second chance
// to pick a different provider. A document embedded with a different model than
// the one retrieval queries with is not degraded, it is unretrievable, and
// nothing downstream would report it.
//
// Gemini is preferred when its key is present: the corpus is half Amharic and
// Gemini's multilingual coverage is materially better than
// text-embedding-3-large's. Each provider writes to its own table, so both can
// be populated and compared on the language-split evaluation.
//
// Returns (nil, nil) when no key is configured. That is not an error — a
// deployment without embedding credentials leaves ingestion unwired rather than
// failing on every call — so callers must check for nil.
func NewEmbedder(geminiKey, openAIKey string, log *slog.Logger) (ai.Embedder, error) {
	switch {
	case geminiKey != "":
		return ai.NewGeminiEmbedder(geminiKey)
	case openAIKey != "":
		log.Info("using OpenAI embeddings; set GEMINI_API_KEY for better Amharic coverage")
		return ai.NewOpenAIEmbedder(openAIKey)
	default:
		return nil, nil
	}
}

// Paced wraps a provider for bulk ingestion: batching, retries, and a
// client-side quota budget.
//
// Only the ingestion path takes this wrapper. A user's query goes to the raw
// provider, because the budget here is a *blocking* one — a query that arrived
// while a legal code was being indexed would otherwise wait out the remainder
// of the quota window before the search could even start.
func Paced(e ai.Embedder, perMinute int, log *slog.Logger) *BatchEmbedder {
	return NewBatchEmbedder(e).
		PerMinute(perMinute).
		OnWait(func(attempt int, wait time.Duration, cause error) {
			// Distinguishes "waiting on the provider's quota window" from
			// "stuck": a large legal code on a free tier spends most of its
			// ingestion here, and silence looks identical to a hang.
			if attempt == 0 {
				// Deliberate pacing, not a rejection.
				log.Info("pacing embeddings to stay inside quota",
					slog.Duration("wait", wait.Round(time.Second)))
				return
			}
			log.Warn("embedding rate limited, waiting as instructed",
				slog.Int("attempt", attempt),
				slog.Duration("wait", wait),
				slog.String("cause", CompactError(cause.Error())))
		})
}

// CompactError flattens a provider's multi-line error into one log field,
// keeping the part an operator can act on.
//
// Quota errors bury the useful sentence — which metric, what limit, how long to
// wait — behind boilerplate about billing and documentation URLs. Truncating
// from the front threw exactly that away, and the difference matters: a
// per-minute limit means wait, a daily limit means stop until tomorrow or enable
// billing.
func CompactError(s string) string {
	s = strings.Join(strings.Fields(s), " ")

	if i := strings.Index(s, "Quota exceeded for metric"); i >= 0 {
		s = s[i:]
	}
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}
