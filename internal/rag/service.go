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
		MaxOutputTokens:    2048,
	}
}

// Service answers legal questions from the corpus.
type Service struct {
	retriever Retriever
	reranker  Reranker
	llm       ai.LLM
	cfg       Config
	log       *slog.Logger
}

func NewService(retriever Retriever, reranker Reranker, llm ai.LLM, cfg Config, log *slog.Logger) *Service {
	if cfg.TopN <= 0 {
		cfg.TopN = DefaultTopN
	}
	if cfg.Candidates <= 0 {
		cfg.Candidates = 50
	}
	return &Service{retriever: retriever, reranker: reranker, llm: llm, cfg: cfg, log: log}
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
}

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
)

// Event is emitted to the caller as the answer is produced.
type Event struct {
	Type      EventType
	Text      string
	Citations []Citation
	Result    *Result
}

// AnswerKind is what sort of reply this was, so the client can present the three
// cases honestly rather than inferring them from flags.
type AnswerKind string

const (
	// KindLegal is an answer generated from retrieved provisions.
	KindLegal AnswerKind = "legal"

	// KindAbstention is a refusal: retrieval found nothing usable.
	KindAbstention AnswerKind = "abstention"

	// KindConversation is a reply to a message that was not a legal question at
	// all — a greeting, thanks, or a question about what Seera does. It carries
	// no citations because it makes no claim about the law, and it must never be
	// shown with the trappings of a grounded legal answer.
	KindConversation AnswerKind = "conversation"
)

// Result summarises a completed answer.
type Result struct {
	Kind      AnswerKind `json:"kind"`
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
	Condense     int64 `json:"condense"`
	Retrieval    int64 `json:"retrieval"`
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

	// A greeting is not a retrieval problem. Answering it from a written reply
	// costs nothing, returns immediately, and cannot state anything about the
	// law; sending it through retrieval produced "I could not find this in the
	// corpus", which is true and useless.
	if intent := Classify(q.Text); intent != IntentLegal {
		return s.converse(ctx, intent, timings, start, emit)
	}

	q.History = TrimHistory(q.History)

	// What retrieval searches for. Identical to the question unless this is a
	// follow-up, in which case it is the standalone rewrite — "what about for a
	// PLC?" carries none of the terms that would find the provision.
	searchText, condenseMS := s.retrievalQuery(ctx, q)
	timings.Condense = condenseMS

	retrieveStart := time.Now()
	candidates, err := s.retriever.Search(ctx, search.Query{
		Text:     searchText,
		Language: q.Language,
		AsOf:     q.AsOf,
		Limit:    s.cfg.Candidates,
	})
	if err != nil {
		if rateLimited(err) {
			return fmt.Errorf("retrieve: %w: %w", ErrProviderQuota, err)
		}
		return fmt.Errorf("retrieve: %w", err)
	}
	timings.Retrieval = time.Since(retrieveStart).Milliseconds()
	timings.CandidateN = len(candidates)

	rerankStart := time.Now()
	top := candidates
	if len(candidates) > 0 {
		// Reranked against the same text retrieval used, not the raw follow-up:
		// scoring a pronoun against a provision is how a good candidate set gets
		// thrown away at the last step.
		top, err = s.reranker.Rerank(ctx, searchText, candidates, s.cfg.TopN)
		if err != nil {
			// A reranker outage must not take the whole answer path down with
			// it; fused order is a worse ordering, not a wrong one. The
			// degradation is logged and reported in the result.
			s.log.WarnContext(ctx, "reranker failed, falling back to fusion order",
				slog.String("reranker", s.reranker.Name()),
				slog.Any("error", err))
			top = candidates
			if len(top) > s.cfg.TopN {
				top = top[:s.cfg.TopN]
			}
		}
	}
	timings.Rerank = time.Since(rerankStart).Milliseconds()

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
	var (
		answer     strings.Builder
		firstToken time.Time
	)

	for stream.Next() {
		event := stream.Event()
		if event.Type != ai.EventText || event.Text == "" {
			continue
		}
		if firstToken.IsZero() {
			firstToken = time.Now()
			timings.FirstToken = firstToken.Sub(start).Milliseconds()
		}

		if safe := filter.Write(event.Text); safe != "" {
			answer.WriteString(safe)
			if err := emit(Event{Type: EventToken, Text: safe}); err != nil {
				return err
			}
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	if tail := filter.Close(); tail != "" {
		answer.WriteString(tail)
		if err := emit(Event{Type: EventToken, Text: tail}); err != nil {
			return err
		}
	}

	used := filter.Used()
	grounded := len(used) > 0 || !looksSubstantive(answer.String())

	// Report only the citations the answer actually made. Returning all
	// retrieved sources would let an ungrounded answer arrive wearing a full set
	// of plausible references.
	cited := filterCitations(citations, used)

	timings.Total = time.Since(start).Milliseconds()
	result := Result{
		Kind:      KindLegal,
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

// converse answers a message that was not a legal question.
//
// It emits the same event sequence as any other answer — sources, then text,
// then done — so the client has one code path. The sources list is empty and
// the kind says why: nothing here rests on a provision, and the UI must not
// dress it as though it does.
func (s *Service) converse(
	ctx context.Context,
	intent Intent,
	timings Timings,
	start time.Time,
	emit func(Event) error,
) error {
	timings.Total = time.Since(start).Milliseconds()

	s.log.InfoContext(ctx, "answered conversationally",
		slog.String("intent", string(intent)))

	if err := emit(Event{Type: EventSources, Citations: []Citation{}}); err != nil {
		return err
	}
	if err := emit(Event{Type: EventToken, Text: ConversationalReply(intent)}); err != nil {
		return err
	}
	return emit(Event{Type: EventDone, Citations: []Citation{}, Result: &Result{
		Kind: KindConversation,
		// Neither flag applies: nothing was searched, and nothing was claimed.
		// They are false so that no client reading only the booleans can present
		// this as a grounded legal answer.
		Abstained: false,
		Grounded:  false,
		Citations: []Citation{},
		Timings:   timings,
	}})
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
