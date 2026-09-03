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

	// CitationEntailment enables the post-generation verification pass (see
	// Verifier). Mirrors config.Config's field of the same name, threaded
	// through separately so rag package tests can exercise it without
	// depending on internal/config.
	CitationEntailment bool
}

// DefaultConfig is the tuned starting point.
func DefaultConfig() Config {
	return Config{
		TopN:               DefaultTopN,
		Candidates:         50,
		ContextTokenBudget: DefaultContextTokenBudget,
		// Raised from 2048, then from 4096 (2026-08-22, after a genuine
		// comprehensive-coverage question — "what are the different types of
		// business organizations and how is each one formed" — legitimately
		// needed to cover five-plus distinct entity types and got cut off at
		// 4096). The prompt asks for the provision *and* an explanation of it,
		// a multi-article answer in Amharic costs about a token per character,
		// and rule 13 now asks for richer, more structured answers on top of
		// that — all of which raise the ceiling a genuinely broad question needs.
		MaxOutputTokens: 6144,
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
	verifier  Verifier
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

// WithVerifier supplies the citation-entailment checker. Only consulted when
// Config.CitationEntailment is also on — see that field's doc comment; a
// verifier configured without the flag set simply never runs.
func WithVerifier(v Verifier) Option {
	return func(s *Service) { s.verifier = v }
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

	// SkipRetrieval asks the answer path to rely solely on CarriedChunks,
	// without a fresh search — set only when the router judged the sources
	// already cited are enough. False is always safe: it degrades to a normal
	// retrieval, never to a wrong answer. Ignored (as if false) when carried
	// sources resolve to nothing, so a stale or empty carry still gets a real
	// search rather than a spurious abstention.
	SkipRetrieval bool

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

	// Confidence summarises how much evidence backs this answer. A pointer so
	// it is simply absent on results that never reached generation
	// (abstention, conversation) rather than a misleading zero value.
	Confidence *Confidence `json:"confidence,omitempty"`

	// Unverified lists the markers of citations checkCitations judged
	// unsupported by their source's actual text — distinct from Grounded,
	// which only says every claim carries *some* citation. Populated by two
	// independent checks: a free mechanical one (lexicallySupported, always
	// runs) and an experimental semantic one (Verifier, only when
	// Config.CitationEntailment is on) — see checkCitations. Nil means
	// neither check flagged anything, not that nothing was checked; the
	// mechanical half always runs.
	Unverified []string `json:"unverified,omitempty"`
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

	var (
		top []search.Result
		// topScore is the best score this request's rerank produced, kept for
		// the abstention log: "retrieval found nothing" and "retrieval found
		// six provisions the threshold rejected" are different bugs and used
		// to look identical.
		topScore float64
	)
	if q.SkipRetrieval && len(carried) > 0 {
		// The router judged the sources already cited are enough — no fresh
		// search, no rerank (there is nothing new to rank against), and no
		// multi-hop (paying for a "what's missing" call directly contradicts
		// the premise that nothing is missing).
		top = carried
		timings.CarriedN = len(carried)
		s.log.DebugContext(ctx, "answering from carried sources only", slog.Int("carried", len(carried)))
	} else {
		candidates, retrievalMS, err := s.retrieve(ctx, q, searchText)
		if err != nil {
			return err
		}
		timings.Retrieval = retrievalMS
		timings.CandidateN = len(candidates)

		var reranked bool
		rerankStart := time.Now()
		top, reranked = s.rerankTop(ctx, searchText, candidates, topN)
		timings.Rerank = time.Since(rerankStart).Milliseconds()

		// A second retrieval pass for what the first one missed. Off unless enabled.
		if s.cfg.MultiHop && len(top) > 0 {
			var expandMS int64
			top, reranked, expandMS = s.expand(ctx, q, searchText, top, reranked, topN)
			timings.Expand = expandMS
		}

		// The threshold is applied here, to this request's reranked retrieval,
		// before carried sources join it. A carried source carries no score on
		// the reranker's scale — nothing scored it for this question — and
		// mergeSources deliberately puts it first, so asking this after the
		// merge read a zero no reranker ever produced and abstained on every
		// follow-up that carried anything at all.
		topScore, _ = bestRerankedScore(top)
		if belowThreshold(top, reranked, s.cfg.MinRerankScore) {
			// Nothing retrieval found is about this question. Carried sources
			// still may be, and they become all the evidence this answer gets —
			// the same shape the SkipRetrieval branch above produces. Keeping
			// the rejected chunks as filler would only invite a citation to a
			// provision already judged irrelevant.
			top = nil
		}

		// Carried sources lead: they are what a follow-up is about, and putting
		// them first means the context budget spends itself on them before
		// anything else.
		top = mergeSources(carried, top)
		timings.CarriedN = len(carried)
	}

	if len(top) == 0 {
		return s.abstain(ctx, q, timings, topScore, start, emit)
	}

	built := BuildContext(top, s.cfg.ContextTokenBudget)
	timings.ContextChunk = len(built.Sources)
	if len(built.Sources) == 0 {
		return s.abstain(ctx, q, timings, topScore, start, emit)
	}
	confidence := confidenceFrom(top, len(built.Sources), s.cfg.MinRerankScore)

	citations := make([]Citation, 0, len(built.Sources))
	for _, ref := range built.Sources {
		citations = append(citations, citationFrom(ref))
	}
	if err := emit(Event{Type: EventSources, Citations: citations}); err != nil {
		return err
	}

	stream, err := s.llm.Stream(ctx, ai.Request{
		System:    SystemPrompt,
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: userPrompt(q, built.Block, confidence)}},
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

	// The mechanical half of this always runs (see checkCitations); the
	// semantic half only when Config.CitationEntailment is on. Runs after
	// every token has already streamed to the reader — this can only delay
	// the final citations/done event, never time-to-first-token or the
	// visible answer.
	unverified := s.checkCitations(ctx, answer.String(), cited)

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
		Kind:       kind,
		Depth:      q.Depth,
		Truncated:  truncated,
		Related:    relatedQuestions(cited),
		Grounded:   grounded,
		Citations:  cited,
		Timings:    timings,
		Model:      s.llm.Model(),
		Reranker:   s.reranker.Name(),
		Confidence: &confidence,
		Unverified: unverified,
	}

	s.logAnswer(ctx, q, result, filter.Invalid(), built.Dropped)

	return emit(Event{Type: EventDone, Citations: cited, Result: &result})
}

// checkCitations flags citations that fail either of two independent checks
// and returns their markers, in the order the claims first appear.
//
// The mechanical check (lexicallySupported) is free and always runs. The
// semantic check (Verifier) is experimental and only runs when
// Config.CitationEntailment and a Verifier are both set — see that field's
// doc comment. A Verifier failure (provider error, malformed response) is
// logged and treated as "nothing more to report," the same posture
// rerankTop already takes toward a reranker failure: neither check may ever
// take away a signal the other found, or fail the answer they describe.
func (s *Service) checkCitations(ctx context.Context, answer string, cited []Citation) []string {
	claims := extractClaims(answer, cited)
	if len(claims) == 0 {
		return nil
	}

	flagged := make(map[string]bool, len(claims))
	for _, c := range claims {
		if !lexicallySupported(c.Text, c.Source) {
			flagged[c.Marker] = true
		}
	}

	if s.cfg.CitationEntailment && s.verifier != nil {
		verdicts, err := s.verifier.Verify(ctx, claims)
		if err != nil {
			s.log.WarnContext(ctx, "citation entailment check failed, skipping", slog.Any("error", err))
		} else {
			for i, ok := range verdicts {
				if !ok {
					flagged[claims[i].Marker] = true
				}
			}
		}
	}

	if len(flagged) == 0 {
		return nil
	}
	unverified := make([]string, 0, len(flagged))
	for _, c := range claims {
		if flagged[c.Marker] {
			unverified = append(unverified, c.Marker)
		}
	}
	return unverified
}

// belowThreshold reports whether this request's reranked retrieval failed to
// clear the abstention bar.
//
// It asks only about retrieval. Whether there is enough evidence to answer at
// all is a separate question, settled by the caller once carried sources have
// been merged in — conflating the two is what made a follow-up abstain while
// holding the provision it was asking about.
//
// reranked is false when rerankTop fell back to raw fusion order — those scores
// are on an RRF scale, not the reranker's, and comparing them against
// MinRerankScore would reject nearly everything. Skip the threshold entirely in
// that case.
func belowThreshold(top []search.Result, reranked bool, minScore float64) bool {
	if !reranked || minScore <= 0 {
		return false
	}
	best, scored := bestRerankedScore(top)
	return scored && best < minScore
}

// abstain reports that there was not enough evidence to answer.
//
// topScore is the best score this request's rerank produced, or zero when
// nothing was scored. It is logged rather than used: without it, a threshold
// rejecting good retrieval and retrieval genuinely finding nothing produce the
// same line, and telling them apart meant reading the code.
func (s *Service) abstain(
	ctx context.Context,
	q Question,
	timings Timings,
	topScore float64,
	start time.Time,
	emit func(Event) error,
) error {
	timings.Total = time.Since(start).Milliseconds()

	s.log.InfoContext(ctx, "abstained",
		slog.Int("candidates", timings.CandidateN),
		slog.Int("carried", timings.CarriedN),
		slog.String("language", string(q.Language)),
		slog.String("reranker", s.reranker.Name()),
		slog.Float64("min_rerank_score", s.cfg.MinRerankScore),
		slog.Float64("top_score", topScore),
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
func userPrompt(q Question, sources string, confidence Confidence) string {
	var b strings.Builder
	b.WriteString("Sources:\n\n")
	b.WriteString(sources)

	if len(q.History) > 0 {
		b.WriteString("Conversation so far (context only — not a source):\n\n")
		writeHistory(&b, q.History)
	}

	b.WriteString("Question: ")
	b.WriteString(q.Text)
	b.WriteString(depthGuidance(q.Depth))
	if confidence.Level != ConfidenceHigh {
		b.WriteString(confidenceGuidance)
	}
	return b.String()
}

// depthGuidance reinforces what topNFor already scales: how much the answer
// should do with what it finds. DepthExplain, the default, gets nothing extra
// — today's behavior is the baseline this leaves unchanged.
func depthGuidance(depth Depth) string {
	switch depth {
	case DepthQuote:
		return "\n\nThe reader wants the provision itself. Lead with it; keep the explanation short."
	case DepthAnalyse:
		return "\n\nThe reader is applying several provisions to a situation. Work through how they fit together, note any condition that changes the outcome, and say what follows practically from applying them together."
	default:
		return ""
	}
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
