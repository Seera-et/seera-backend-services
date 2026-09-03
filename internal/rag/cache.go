package rag

import (
	"container/list"
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// Answerer is whatever actually answers a legal question. *Service satisfies
// it; Cache wraps one of these, not a concrete *Service, so it can wrap a
// test double too.
type Answerer interface {
	Answer(ctx context.Context, q Question, emit func(Event) error) error
}

// CacheConfig tunes Cache's size and staleness bound.
type CacheConfig struct {
	// Capacity is how many answers the in-process LRU holds at once.
	Capacity int

	// TTL is a belt-and-suspenders bound on staleness, not the primary
	// correctness mechanism — see Cache's doc comment for why the chunk-ID
	// check does the real work. This just caps how long a corner case (an
	// unrelated ingestion edit that happens not to change this question's
	// retrieved chunks) could theoretically persist.
	TTL time.Duration
}

// DefaultCacheConfig is the tuned starting point: generous enough to catch a
// repeated question or a page refresh within a session, short enough that a
// stale entry cannot survive long even in the corner case the TTL guards
// against.
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{Capacity: 200, TTL: 12 * time.Minute}
}

// Cache wraps an Answerer with an in-process, self-invalidating answer cache.
//
// It exists because every question — repeated, refreshed, or asked twice by
// two readers — re-runs retrieval, rerank, and generation from scratch today.
// That is expensive (two LLM calls per legal answer, after Gap 1's reranker)
// and, under Gemini's free-tier daily quota, was observed live to be the
// difference between an eval run completing and running out of quota
// partway through.
//
// Correctness, not just speed, is why this is a decorator over rag.Service
// rather than a change inside it: a cached answer served after a provision is
// amended or repealed would be silently wrong, and internal/legal's version
// model exists precisely to prevent that. So retrieval always runs first — it
// is comparatively cheap (one DB round trip plus one embedding call) — and its
// fresh set of chunk IDs is compared against what the cached entry was built
// from. Identical → the cached answer is still correct, because the sources it
// would generate from have not changed; skip rerank and generation entirely.
// Different → treat it as any other miss. This means a hit never skips the
// step that would notice a newly published version or an amendment: if
// ingestion changes what retrieval returns for a question, the very next
// request for it recomputes and overwrites the stale entry, with no explicit
// invalidation hook needed anywhere in internal/ingestion.
//
// Deliberately narrow about what it caches. A follow-up that carries earlier
// sources, relies on conversation history, or skipped retrieval outright is
// never cached — those turns are about *this* conversation, and reusing a
// different conversation's cached answer for the same question text would be
// actively wrong. Only a question asked the way a fresh, standalone question
// is asked gets cached.
type Cache struct {
	inner      Answerer
	retriever  Retriever
	candidates int

	store *lru
	log   *slog.Logger
}

// NewCache builds the decorator. retriever runs the same hybrid search
// rag.Service itself would, so the freshness check reflects the same
// retrieval a miss would produce — not a cheaper approximation of it.
func NewCache(inner Answerer, retriever Retriever, candidates int, cfg CacheConfig, log *slog.Logger) *Cache {
	if cfg.Capacity <= 0 {
		cfg.Capacity = DefaultCacheConfig().Capacity
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultCacheConfig().TTL
	}
	if candidates <= 0 {
		candidates = 50
	}
	return &Cache{
		inner:      inner,
		retriever:  retriever,
		candidates: candidates,
		store:      newLRU(cfg.Capacity, cfg.TTL),
		log:        log,
	}
}

// Answer implements Answerer.
func (c *Cache) Answer(ctx context.Context, q Question, emit func(Event) error) error {
	if !cacheable(q) {
		return c.inner.Answer(ctx, q, emit)
	}

	start := time.Now()
	searchText := strings.TrimSpace(q.SearchText)
	if searchText == "" {
		searchText = q.Text
	}

	candidates, err := c.currentCandidates(ctx, q, searchText)
	if err != nil {
		// Cannot check freshness without retrieval. Answering normally is
		// always safe; the cache is an optimization, not a dependency the
		// answer path should fail over.
		return c.inner.Answer(ctx, q, emit)
	}
	ids := sortedChunkIDs(candidates)

	key := cacheKeyFor(q)
	if hit, ok := c.store.get(key); ok && equalChunkIDs(hit.chunkIDs, ids) {
		c.log.DebugContext(ctx, "answered from cache", slog.Int("candidates", len(candidates)))
		return replay(hit, start, emit)
	}

	captured := &capturedAnswer{}
	if err := c.inner.Answer(ctx, q, capturingEmit(emit, captured)); err != nil {
		return err
	}
	if result := captured.result; result != nil && cacheableKind(result.Kind) {
		c.store.set(key, cacheEntry{
			chunkIDs:   ids,
			citations:  captured.citations,
			answerText: captured.answer.String(),
			result:     *result,
		})
	}
	return nil
}

// currentCandidates reuses speculative retrieval when the dialogue layer
// already fetched it — the common case for a first-turn question, which is
// exactly the case this cache targets — rather than paying for a second
// retrieval the miss path would also run.
func (c *Cache) currentCandidates(ctx context.Context, q Question, searchText string) ([]search.Result, error) {
	if q.Candidates != nil {
		return q.Candidates, nil
	}
	return c.retriever.Search(ctx, search.Query{
		Text:     searchText,
		Language: q.Language,
		AsOf:     q.AsOf,
		Limit:    c.candidates,
	})
}

// cacheable reports whether q is even eligible for caching, before spending a
// retrieval call to check.
//
// CarriedChunks/SkipRetrieval are excluded per Cache's own doc comment: those
// turns are about a specific conversation's earlier sources, not a standalone
// question. History is excluded too, beyond what the plan this shipped from
// specified: userPrompt writes the conversation verbatim into the generation
// prompt, so two requests with the same question text but different history
// are not the same request, and a cache keyed only on question text would
// serve one conversation's answer into another's.
func cacheable(q Question) bool {
	return !q.SkipRetrieval && len(q.CarriedChunks) == 0 && len(q.History) == 0
}

// cacheableKind reports whether a result is worth caching at all. Only
// results that reached generation are: KindConversation never reaches this
// decorator (dialogue only calls the legal engine for legal/discussion
// intents), and KindAbstention already skipped generation on the miss that
// produced it, so caching it would save nothing worth the entry.
func cacheableKind(k AnswerKind) bool {
	return k == KindLegal || k == KindInsufficient
}

// replay serves a cache hit: the same events a fresh answer would emit, built
// from what was captured when this entry was written.
//
// Timings are rebuilt rather than replayed verbatim — reusing the original
// miss's rerank/generation timings on a request that did neither would
// misreport what this request actually cost, which is retrieval (already
// paid for by the freshness check) and nothing else.
func replay(hit cacheEntry, start time.Time, emit func(Event) error) error {
	if err := emit(Event{Type: EventSources, Citations: hit.citations}); err != nil {
		return err
	}
	if hit.answerText != "" {
		if err := emit(Event{Type: EventToken, Text: hit.answerText}); err != nil {
			return err
		}
	}

	result := hit.result
	elapsed := time.Since(start).Milliseconds()
	result.Timings = Timings{
		CandidateN:   hit.result.Timings.CandidateN,
		ContextChunk: hit.result.Timings.ContextChunk,
		CarriedN:     hit.result.Timings.CarriedN,
		FirstToken:   elapsed,
		Total:        elapsed,
	}
	return emit(Event{Type: EventDone, Citations: hit.citations, Result: &result})
}

// capturedAnswer accumulates what a miss's Answer call emits, so it can be
// written to the cache after the fact without changing how Service.Answer
// itself streams.
type capturedAnswer struct {
	citations []Citation
	answer    strings.Builder
	result    *Result
}

// capturingEmit forwards every event to the real caller unchanged — a cache
// miss must stream exactly as it would without the cache in front of it —
// while also recording what a later hit would need to replay.
func capturingEmit(real func(Event) error, c *capturedAnswer) func(Event) error {
	return func(e Event) error {
		switch e.Type {
		case EventSources:
			c.citations = e.Citations
		case EventToken:
			c.answer.WriteString(e.Text)
		case EventDone:
			c.result = e.Result
		}
		return real(e)
	}
}

// cacheEntry is what one cache slot holds.
type cacheEntry struct {
	// chunkIDs is the sorted candidate set retrieval returned when this entry
	// was written — the self-invalidation signal a hit is checked against.
	chunkIDs   []legal.ChunkID
	citations  []Citation
	answerText string
	result     Result
}

// cacheKey identifies "the same question" for caching purposes.
//
// Deliberately not just normalized text: Depth changes both how many sources
// reach the generator (Config.topNFor) and the instructions it is given
// (depthGuidance), so "explain Article 45" and "quote Article 45" must not
// share an entry even though chunk retrieval for them may be identical.
type cacheKey struct {
	text     string
	language string
	asOf     string
	depth    Depth
}

// cacheKeyFor normalizes a question into a key. AsOf's zero value ("today")
// collapses to one key rather than one per call's exact timestamp — the TTL
// is what bounds how long "today" can be served for, not this normalization.
func cacheKeyFor(q Question) cacheKey {
	asOf := ""
	if !q.AsOf.IsZero() {
		asOf = q.AsOf.UTC().Format("2006-01-02")
	}
	return cacheKey{
		text:     normalizeForCache(q.Text),
		language: string(q.Language),
		asOf:     asOf,
		depth:    q.Depth,
	}
}

func normalizeForCache(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func sortedChunkIDs(results []search.Result) []legal.ChunkID {
	ids := make([]legal.ChunkID, len(results))
	for i, r := range results {
		ids[i] = r.ChunkID
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func equalChunkIDs(a, b []legal.ChunkID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// lru is a minimal thread-safe, TTL-bounded LRU.
//
// Hand-rolled rather than a dependency: this is container/list plus a map and
// a mutex, the same size of problem this codebase already solves itself
// elsewhere (OpenAIEmbedder speaks HTTP directly rather than importing a
// vendor SDK; NormalizeNumber reimplements Ge'ez numeral parsing rather than
// pulling one in) rather than importing one more package for it.
// ---------------------------------------------------------------------------

type lru struct {
	mu       sync.Mutex
	ll       *list.List
	items    map[cacheKey]*list.Element
	capacity int
	ttl      time.Duration
}

type lruEntry struct {
	key       cacheKey
	value     cacheEntry
	expiresAt time.Time
}

func newLRU(capacity int, ttl time.Duration) *lru {
	return &lru{
		ll:       list.New(),
		items:    make(map[cacheKey]*list.Element),
		capacity: capacity,
		ttl:      ttl,
	}
}

func (c *lru) get(key cacheKey) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return cacheEntry{}, false
	}
	entry := el.Value.(*lruEntry)
	if time.Now().After(entry.expiresAt) {
		c.ll.Remove(el)
		delete(c.items, key)
		return cacheEntry{}, false
	}
	c.ll.MoveToFront(el)
	return entry.value, true
}

func (c *lru) set(key cacheKey, value cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	expiresAt := time.Now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*lruEntry)
		entry.value = value
		entry.expiresAt = expiresAt
		c.ll.MoveToFront(el)
		return
	}

	el := c.ll.PushFront(&lruEntry{key: key, value: value, expiresAt: expiresAt})
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*lruEntry).key)
		}
	}
}
