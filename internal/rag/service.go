package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// ErrProviderQuota marks a failure caused by an exhausted provider quota rather
// than a fault in this service.
//
// Worth distinguishing all the way to the user: "we are over our quota, try
// later" is true, actionable and not alarming, where "something went wrong"
// invites a retry that cannot succeed and hides the real cause from whoever has
// to fix it. The embedding quota takes down retrieval and therefore every answer,
// so this is the single most likely operational failure of the whole feature.
var ErrProviderQuota = errors.New("provider quota exhausted")

// rateLimited reports whether a provider error in the chain was a rate limit.
func rateLimited(err error) bool {
	var l interface{ RateLimited() bool }
	return errors.As(err, &l) && l.RateLimited()
}

// Retriever is the retrieval this service needs.
type Retriever interface {
	Search(ctx context.Context, q search.Query) ([]search.Result, error)
}

// Config tunes the answer path.
type Config struct {
	// TopN is how many reranked chunks reach the generator.
	TopN int

	// Candidates is how many chunks retrieval returns for reranking.
	Candidates int

	// ContextTokenBudget caps the sources block.
	ContextTokenBudget int

	// MaxOutputTokens caps the generated answer.
	MaxOutputTokens int

	// MultiHop enables a second retrieval pass for what the first one missed.
	// Off by default: it buys recall on questions that span several provisions
	// and costs a model call plus a retrieval on every answer that uses it.
	MultiHop bool

	// MinRerankScore is the abstention threshold, compared against the top
	// reranked score. It is meaningful only for a reranker that produces
	// calibrated relevance scores: with the passthrough reranker the score is an
	// RRF value on an unrelated scale, so wiring must leave this at zero and let
	// "no candidates at all" be the only abstention trigger.
	MinRerankScore float64
}

// DefaultConfig is the tuned starting point.
func DefaultConfig() Config {
	return Config{
		TopN:               DefaultTopN,
		Candidates:         50,
		ContextTokenBudget: DefaultContextTokenBudget,
		// Raised from 2048 when answers began stopping mid-sentence: the prompt
		// now asks for the provision *and* an explanation of it, and a
		// multi-article answer in Amharic — where a character costs about a
		// token — does not fit in the old budget.
		MaxOutputTokens: 4096,
	}
}

// topNFor scales the number of provisions the generator sees to what the reader
// asked for. Quoting one article needs few; applying several to a situation needs
// more, and the context budget still has the final say.
func (c Config) topNFor(depth Depth) int {
	switch depth {
	case DepthQuote:
		if c.TopN > 4 {
			return 4
		}
	case DepthAnalyse:
		if c.TopN < maxAnalyseTopN {
			return maxAnalyseTopN
		}
	}
	return c.TopN
}

// maxAnalyseTopN is the ceiling for an analysis question. Above this the
// relevant provision starts competing with everything else in the window, which
// is the failure the context budget exists to prevent.
const maxAnalyseTopN = 10

// Service answers legal questions from the corpus.
type Service struct {
	retriever Retriever
	reranker  Reranker
	llm       ai.LLM
	sources   SourceResolver
	articles  ArticleFinder
	cfg       Config
	log       *slog.Logger
}

// Option configures optional collaborators.
//
// Optional because the service answers without them: without a SourceResolver a
// follow-up re-retrieves instead of carrying its sources, and without an
// ArticleFinder the second retrieval pass is simply not available. Both
// degrade the answer; neither breaks it.
type Option func(*Service)

// WithSources supplies the resolver that re-reads chunks cited earlier in a
// conversation.
func WithSources(resolver SourceResolver) Option {
	return func(s *Service) { s.sources = resolver }
}

// WithArticles supplies exact-article lookup, used by the second retrieval pass.
func WithArticles(finder ArticleFinder) Option {
	return func(s *Service) { s.articles = finder }
}

func NewService(
	retriever Retriever,
	reranker Reranker,
	llm ai.LLM,
	cfg Config,
	log *slog.Logger,
	opts ...Option,
) *Service {
	if cfg.TopN <= 0 {
		cfg.TopN = DefaultTopN
	}
	if cfg.Candidates <= 0 {
		cfg.Candidates = 50
	}

	s := &Service{retriever: retriever, reranker: reranker, llm: llm, cfg: cfg, log: log}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Question is one request for an answer.
type Question struct {
	Text     string
	Language legal.Language
	AsOf     time.Time

	// History is the conversation this question continues, oldest first, and is
	// empty for the first question in a thread. The server keeps no conversation
	// state: the client sends what it wants considered, and TrimHistory bounds it.
	//
	// It affects two things and nothing else — the query retrieval runs, and the
	// context the generator reads. It is never evidence: a legal claim still has
	// to cite a source retrieved for this request.
	History []Turn

	// SearchText is what retrieval should search for, as decided by the router:
	// the question itself, or a rewrite that resolves the references in it.
	// Empty means "use Text", which is what a caller without a router gets.
	SearchText string

	// Depth is how much the answer should do with what it finds. Empty behaves
	// as DepthExplain.
	Depth Depth

	// CarriedChunks are chunks already cited in this conversation, which the
	// reader may be asking about. They are identifiers only: the text is read
	// from the database, never taken from the client.
	CarriedChunks []legal.ChunkID

	// Candidates lets a caller supply retrieval results it has already fetched —
	// the speculative retrieval the dialogue layer starts while the router is
	// still deciding. Nil means this service retrieves for itself.
	Candidates []search.Result
}

// Depth is how much the answer should explain. Set by the router.
type Depth string

const (
	// DepthQuote — the reader wants the provision itself.
	DepthQuote Depth = "quote"
	// DepthExplain — the reader wants to understand it. The default.
	DepthExplain Depth = "explain"
	// DepthAnalyse — the reader wants several provisions applied together.
	DepthAnalyse Depth = "analyse"
)

// Event is one thing that happens while answering.
type EventType string

const (
	// EventSources carries the citations, emitted after retrieval and before the
	// first token so the UI can render source cards while the answer streams.
	EventSources EventType = "sources"

	// EventToken carries a piece of the answer.
	EventToken EventType = "token"

	// EventDone carries the final citations and timings.
	EventDone EventType = "done"

	// EventRoute carries the routing decision, emitted before anything else so
	// the client can say what is actually happening — searching the corpus for
	// a rewritten question, or answering directly.
	EventRoute EventType = "route"
)

// Route is what the router decided, as the client sees it.
type Route struct {
	Intent string `json:"intent"`
	Depth  Depth  `json:"depth,omitempty"`

	// SearchText is the question retrieval was given. Shown to the reader when
	// it differs from what they typed, because a rewrite they cannot see is a
	// rewrite they cannot correct.
	SearchText string `json:"search_text,omitempty"`

	// CarriedSources is how many provisions came from earlier in the conversation.
	CarriedSources int `json:"carried_sources,omitempty"`
}

// Event is emitted to the caller as the answer is produced.
type Event struct {
	Type      EventType
	Text      string
	Citations []Citation
	Result    *Result
	Route     *Route
}

// AnswerKind is what sort of reply this was, so the client can present the three
// cases honestly rather than inferring them from flags.
type AnswerKind string

const (
	// KindLegal is an answer generated from retrieved provisions.
	KindLegal AnswerKind = "legal"

	// KindAbstention is a refusal: retrieval found nothing usable.
	KindAbstention AnswerKind = "abstention"

	// KindInsufficient is an answer that read the retrieved provisions and
	// reported that they do not settle the question. Sources were found and are
	// worth showing; the question was not answered.
	KindInsufficient AnswerKind = "insufficient"

	// KindConversation is a reply to a message that was not a legal question at
	// all — a greeting, thanks, or a question about what Seera does. It carries
	// no citations because it makes no claim about the law, and it must never be
	// shown with the trappings of a grounded legal answer.
	KindConversation AnswerKind = "conversation"
)

// Result summarises a completed answer.
type Result struct {
	Kind AnswerKind `json:"kind"`

	// Intent and Depth record what the router decided, so a surprising answer
	// can be traced to the decision that shaped it.
	Intent string `json:"intent,omitempty"`
	Depth  Depth  `json:"depth,omitempty"`

	// Related are follow-up questions built from the citations this answer used.
	// Derived from stored metadata, never written by the model.
	Related []Suggestion `json:"related,omitempty"`

	// Truncated is set when generation hit the output limit. The answer stops
	// mid-sentence, and the client must say so rather than showing it as
	// finished — an unfinished legal statement is worse than none.
	Truncated bool `json:"truncated,omitempty"`

	Abstained bool       `json:"abstained"`
	Grounded  bool       `json:"grounded"`
	Citations []Citation `json:"citations"`
	Timings   Timings    `json:"timings_ms"`
	Model     string     `json:"model"`
	Reranker  string     `json:"reranker"`
}

// Timings is the per-stage latency breakdown the SRS asks to be logged.
type Timings struct {
	// Condense is the follow-up rewrite, and is zero for the first question in a
	// conversation. Reported separately because it sits in front of retrieval and
	// is therefore charged directly to time-to-first-token.
	Condense  int64 `json:"condense"`
	Retrieval int64 `json:"retrieval"`
	// Expand is the second retrieval pass, and is zero unless multi-hop is on.
	Expand int64 `json:"expand"`
	// CarriedN is how many sources came from earlier in the conversation.
	CarriedN     int   `json:"carried_sources"`
	Rerank       int64 `json:"rerank"`
	FirstToken   int64 `json:"first_token"`
	Total        int64 `json:"total"`
	CandidateN   int   `json:"candidates"`
	ContextChunk int   `json:"context_chunks"`
}

// Answer streams an answer to a question.
//
// The pipeline is: retrieve wide → rerank to a few → decide whether there is
// enough evidence → build a context of the survivors → generate with a
// citation-required prompt → validate every marker against that context as it
// streams. Retrieval filters (published, in force) are SQL predicates applied
// inside the retriever, never instructions to the model.
//
// emit is called for each event in order and must not be called concurrently. It
// returning an error aborts the answer — that is how a disconnected client stops
// work rather than paying for tokens nobody will read.
func (s *Service) Answer(ctx context.Context, q Question, emit func(Event) error) error {
	start := time.Now()
	var timings Timings

	if strings.TrimSpace(q.Text) == "" {
		return fmt.Errorf("question is empty")
	}

	q.History = TrimHistory(q.History)

	// What retrieval searches for. Identical to the question unless the router
	// rewrote it — "what about for a PLC?" carries none of the terms that would
	// find the provision.
	searchText := strings.TrimSpace(q.SearchText)
	if searchText == "" {
		searchText = q.Text
	}
	topN := s.cfg.topNFor(q.Depth)

	// Sources the reader is already looking at, re-read from the database. The
	// client sends identifiers; the text always comes from here.
	carried := s.carriedSources(ctx, q.CarriedChunks)

	candidates, retrievalMS, err := s.retrieve(ctx, q, searchText)
	if err != nil {
		return err
	}
	timings.Retrieval = retrievalMS
	timings.CandidateN = len(candidates)

	rerankStart := time.Now()
	top := s.rerankTop(ctx, searchText, candidates, topN)
	timings.Rerank = time.Since(rerankStart).Milliseconds()

	// A second retrieval pass for what the first one missed. Off unless enabled.
	if s.cfg.MultiHop && len(top) > 0 {
		var expandMS int64
		top, expandMS = s.expand(ctx, q, searchText, top, topN)
		timings.Expand = expandMS
	}

	// Carried sources lead: they are what a follow-up is about, and putting them
	// first means the context budget spends itself on them before anything else.
	top = mergeSources(carried, top)
	timings.CarriedN = len(carried)

	if s.shouldAbstain(top) {
		return s.abstain(ctx, q, timings, start, emit)
	}

	built := BuildContext(top, s.cfg.ContextTokenBudget)
	timings.ContextChunk = len(built.Sources)
	if len(built.Sources) == 0 {
		return s.abstain(ctx, q, timings, start, emit)
	}

	citations := make([]Citation, 0, len(built.Sources))
	for _, ref := range built.Sources {
		citations = append(citations, citationFrom(ref))
	}
	if err := emit(Event{Type: EventSources, Citations: citations}); err != nil {
		return err
	}

	stream, err := s.llm.Stream(ctx, ai.Request{
		System:    SystemPrompt,
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: userPrompt(q, built.Block)}},
		MaxTokens: s.cfg.MaxOutputTokens,
	})
	if err != nil {
		if rateLimited(err) {
			return fmt.Errorf("generate: %w: %w", ErrProviderQuota, err)
		}
		return fmt.Errorf("generate: %w", err)
	}
	defer func() { _ = stream.Close() }()

	filter := NewMarkerFilter(built.Sources)
	// Strips the model's "these sources do not answer this" signal before any of
	// it reaches the reader, and remembers that it was there.
	gate := newPrefixGate(InsufficientSourcesSentinel)

	var (
		answer     strings.Builder
		firstToken time.Time
		stop       = ai.StopComplete
	)

	for stream.Next() {
		event := stream.Event()
		if event.Type == ai.EventDone && event.Stop != "" {
			stop = event.Stop
		}
		if event.Type != ai.EventText || event.Text == "" {
			continue
		}
		if firstToken.IsZero() {
			firstToken = time.Now()
			timings.FirstToken = firstToken.Sub(start).Milliseconds()
		}

		if safe := filter.Write(gate.Write(event.Text)); safe != "" {
			answer.WriteString(safe)
			if err := emit(Event{Type: EventToken, Text: safe}); err != nil {
				return err
			}
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	if held := filter.Write(gate.Close()); held != "" {
		answer.WriteString(held)
		if err := emit(Event{Type: EventToken, Text: held}); err != nil {
			return err
		}
	}
	if tail := filter.Close(); tail != "" {
		answer.WriteString(tail)
		if err := emit(Event{Type: EventToken, Text: tail}); err != nil {
			return err
		}
	}

	used := filter.Used()
	grounded := len(used) > 0 || !looksSubstantive(answer.String())

	// An answer that hit the output limit stops mid-sentence, and on a legal
	// answer the half that is missing is often the condition that changes the
	// meaning. It is reported rather than presented as complete.
	truncated := stop == ai.StopMaxTokens
	if stop == ai.StopBlocked {
		s.log.WarnContext(ctx, "generation stopped by the provider",
			slog.String("stop", string(stop)))
	}

	// Report only the citations the answer actually made. Returning all
	// retrieved sources would let an ungrounded answer arrive wearing a full set
	// of plausible references.
	cited := filterCitations(citations, used)

	timings.Total = time.Since(start).Milliseconds()
	// The model said the provisions do not settle the question. That is a
	// different outcome from an answer, and a different one again from finding
	// nothing at all: the sources were found, read, and were not enough. Saying
	// "Grounded" over the top of it would be the most misleading badge in the
	// product.
	kind := KindLegal
	if gate.Matched() {
		kind = KindInsufficient
	}

	result := Result{
		Kind:      kind,
		Depth:     q.Depth,
		Truncated: truncated,
		Related:   relatedQuestions(cited),
		Grounded:  grounded,
		Citations: cited,
		Timings:   timings,
		Model:     s.llm.Model(),
		Reranker:  s.reranker.Name(),
	}

	s.logAnswer(ctx, q, result, filter.Invalid(), built.Dropped)

	return emit(Event{Type: EventDone, Citations: cited, Result: &result})
}

// shouldAbstain decides whether there is enough evidence to answer at all.
func (s *Service) shouldAbstain(top []search.Result) bool {
	if len(top) == 0 {
		return true
	}
	if s.cfg.MinRerankScore > 0 && top[0].Score < s.cfg.MinRerankScore {
		return true
	}
	return false
}

func (s *Service) abstain(ctx context.Context, q Question, timings Timings, start time.Time, emit func(Event) error) error {
	timings.Total = time.Since(start).Milliseconds()

	s.log.InfoContext(ctx, "abstained",
		slog.Int("candidates", timings.CandidateN),
		slog.String("language", string(q.Language)),
		slog.Int64("retrieval_ms", timings.Retrieval))

	if err := emit(Event{Type: EventSources, Citations: []Citation{}}); err != nil {
		return err
	}
	if err := emit(Event{Type: EventToken, Text: AbstainMessage}); err != nil {
		return err
	}
	return emit(Event{Type: EventDone, Citations: []Citation{}, Result: &Result{
		Kind:      KindAbstention,
		Abstained: true,
		Grounded:  true, // an admission of ignorance is grounded by construction
		Citations: []Citation{},
		Timings:   timings,
		Model:     s.llm.Model(),
		Reranker:  s.reranker.Name(),
	}})
}

func (s *Service) logAnswer(ctx context.Context, q Question, r Result, invalidMarkers, dropped int) {
	attrs := []any{
		slog.Int("candidates", r.Timings.CandidateN),
		slog.Int("context_chunks", r.Timings.ContextChunk),
		slog.Int("citations", len(r.Citations)),
		slog.Bool("grounded", r.Grounded),
		slog.String("language", string(q.Language)),
		slog.String("model", r.Model),
		slog.Int64("retrieval_ms", r.Timings.Retrieval),
		slog.Int64("rerank_ms", r.Timings.Rerank),
		slog.Int64("first_token_ms", r.Timings.FirstToken),
		slog.Int64("total_ms", r.Timings.Total),
	}
	if dropped > 0 {
		attrs = append(attrs, slog.Int("context_dropped", dropped))
	}

	// Both of these are bugs rather than user errors, and both are invisible
	// without a log line: the answer still reads perfectly well.
	switch {
	case invalidMarkers > 0:
		attrs = append(attrs, slog.Int("invalid_markers", invalidMarkers))
		s.log.WarnContext(ctx, "answer cited sources that were not in its context", attrs...)
	case !r.Grounded:
		s.log.WarnContext(ctx, "answer made legal claims with no citations", attrs...)
	default:
		s.log.InfoContext(ctx, "answered", attrs...)
	}
}

// userPrompt assembles the turn: sources first, conversation, question last.
//
// Order matters for two reasons. The model reads the evidence before the task,
// which is the shape the prompt's rules assume. And the stable, cacheable part of
// the request comes first — the question, which differs every time, goes at the
// end.
//
// The conversation block is context for *what is being asked*, never evidence
// for what the law says. Rule 9 of the system prompt says so, and the marker
// filter enforces it regardless: a claim can only cite a source retrieved for
// this request.
func userPrompt(q Question, sources string) string {
	var b strings.Builder
	b.WriteString("Sources:\n\n")
	b.WriteString(sources)

	if len(q.History) > 0 {
		b.WriteString("Conversation so far (context only — not a source):\n\n")
		writeHistory(&b, q.History)
	}

	b.WriteString("Question: ")
	b.WriteString(q.Text)
	return b.String()
}

func filterCitations(all []Citation, used []string) []Citation {
	if len(used) == 0 {
		return []Citation{}
	}
	byMarker := make(map[string]Citation, len(all))
	for _, c := range all {
		byMarker[c.Marker] = c
	}

	out := make([]Citation, 0, len(used))
	for _, marker := range used {
		if c, ok := byMarker[marker]; ok {
			out = append(out, c)
		}
	}
	return out
}
