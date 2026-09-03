package qa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/rag"
)

type stubAnswerer struct {
	events []rag.Event
	err    error
	got    rag.Question
}

func (s *stubAnswerer) Answer(_ context.Context, q rag.Question, emit func(rag.Event) error) error {
	s.got = q
	for _, e := range s.events {
		if err := emit(e); err != nil {
			return err
		}
	}
	return s.err
}

func newTestHandler(a Answerer) *Handler {
	return NewHandler(a, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/qa/query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// parseSSE turns a recorded stream into ordered (event, data) pairs.
func parseSSE(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name != "" {
			out = append(out, [2]string{name, data})
		}
	}
	return out
}

func TestQueryStreamsSourcesThenTokensThenDone(t *testing.T) {
	answerer := &stubAnswerer{events: []rag.Event{
		{Type: rag.EventSources, Citations: []rag.Citation{{Marker: "S1", DocumentTitle: "Commercial Code"}}},
		{Type: rag.EventToken, Text: "A company "},
		{Type: rag.EventToken, Text: "may be formed [S1]."},
		{Type: rag.EventDone, Citations: []rag.Citation{{Marker: "S1"}}, Result: &rag.Result{Grounded: true, Model: "m"}},
	}}

	rec := post(t, newTestHandler(answerer), `{"question":"how do I form a company?"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Error("X-Accel-Buffering is unset; a proxy would buffer the whole stream")
	}

	events := parseSSE(t, rec.Body.String())
	var names []string
	for _, e := range events {
		names = append(names, e[0])
	}
	if want := "sources,token,token,done"; strings.Join(names, ",") != want {
		t.Errorf("event order = %v, want %s", names, want)
	}
}

// Answer text contains newlines, and SSE is newline-delimited. Sending token
// text bare would split one paragraph into several events.
func TestTokenTextWithNewlinesStaysOneEvent(t *testing.T) {
	answerer := &stubAnswerer{events: []rag.Event{
		{Type: rag.EventToken, Text: "First line.\n\nSecond line.\n"},
	}}

	rec := post(t, newTestHandler(answerer), `{"question":"q"}`)

	events := parseSSE(t, rec.Body.String())
	var tokens []string
	for _, e := range events {
		if e[0] != "token" {
			continue
		}
		var payload tokenPayload
		if err := json.Unmarshal([]byte(e[1]), &payload); err != nil {
			t.Fatalf("token payload is not valid JSON: %q", e[1])
		}
		tokens = append(tokens, payload.Text)
	}

	if len(tokens) != 1 {
		t.Fatalf("got %d token events, want 1", len(tokens))
	}
	if tokens[0] != "First line.\n\nSecond line.\n" {
		t.Errorf("newlines were lost: %q", tokens[0])
	}
}

func TestQueryValidatesBeforeStreaming(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty question", `{"question":"   "}`},
		{"missing question", `{}`},
		{"bad language", `{"question":"q","language":"fr"}`},
		{"bad date", `{"question":"q","as_of":"June 2020"}`},
		{"unknown field", `{"question":"q","temperature":0.7}`},
		{"malformed json", `{"question":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, newTestHandler(&stubAnswerer{}), tc.body)

			// A validation failure must arrive as a real status code, which is
			// only possible before the stream starts.
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", ct)
			}
		})
	}
}

// Once the stream has started the status is already 200 and cannot be taken
// back, so a later failure has to arrive as an error event.
func TestFailureMidStreamBecomesAnErrorEvent(t *testing.T) {
	answerer := &stubAnswerer{
		events: []rag.Event{{Type: rag.EventToken, Text: "partial "}},
		err:    errors.New("gemini exploded"),
	}

	rec := post(t, newTestHandler(answerer), `{"question":"q"}`)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; the stream had already started", rec.Code)
	}

	events := parseSSE(t, rec.Body.String())
	last := events[len(events)-1]
	if last[0] != "error" {
		t.Fatalf("last event = %s, want error", last[0])
	}

	var payload errorPayload
	if err := json.Unmarshal([]byte(last[1]), &payload); err != nil {
		t.Fatalf("error payload is not valid JSON: %q", last[1])
	}
	// The internal cause belongs in the log, keyed by request id — not in the
	// user's browser.
	if strings.Contains(payload.Message, "gemini exploded") {
		t.Errorf("internal error leaked to the client: %q", payload.Message)
	}
	if payload.Code == "" {
		t.Error("error event carries no code")
	}
}

func TestClientDisconnectIsNotReportedAsAnError(t *testing.T) {
	answerer := &stubAnswerer{
		events: []rag.Event{{Type: rag.EventToken, Text: "partial "}},
		err:    ErrClientGone,
	}

	rec := post(t, newTestHandler(answerer), `{"question":"q"}`)

	for _, e := range parseSSE(t, rec.Body.String()) {
		if e[0] == "error" {
			t.Error("a client disconnect was reported as an error event")
		}
	}
}

func TestRequestFiltersReachTheAnswerer(t *testing.T) {
	answerer := &stubAnswerer{events: []rag.Event{{Type: rag.EventDone, Result: &rag.Result{}}}}

	post(t, newTestHandler(answerer), `{"question":"ጥያቄ","language":"am","as_of":"2021-04-01"}`)

	if answerer.got.Text != "ጥያቄ" {
		t.Errorf("question = %q", answerer.got.Text)
	}
	if string(answerer.got.Language) != "am" {
		t.Errorf("language = %q, want am", answerer.got.Language)
	}
	if answerer.got.AsOf.Format("2006-01-02") != "2021-04-01" {
		t.Errorf("as_of = %v", answerer.got.AsOf)
	}
}

func TestDoneEventAlwaysCarriesACitationsArray(t *testing.T) {
	// nil would serialise as null and break a frontend that maps over it.
	answerer := &stubAnswerer{events: []rag.Event{
		{Type: rag.EventDone, Citations: nil, Result: &rag.Result{Abstained: true}},
	}}

	rec := post(t, newTestHandler(answerer), `{"question":"q"}`)

	events := parseSSE(t, rec.Body.String())
	var payload donePayload
	if err := json.Unmarshal([]byte(events[len(events)-1][1]), &payload); err != nil {
		t.Fatalf("done payload is not valid JSON: %v", err)
	}
	if payload.Citations == nil {
		t.Error("citations serialised as null, not []")
	}
	if !payload.Abstained {
		t.Error("abstained flag was not carried through")
	}
}

// Found live, 2026-09-03: rag.Result.Unverified was added (citation
// mechanical/entailment checks) but never mapped into donePayload, so the
// signal was silently dropped between the answer path and the wire — every
// live check looked like "nothing flagged" whether or not anything actually
// was. This pins the mapping so the same class of gap (a Result field added
// but never carried into the HTTP response) cannot recur silently.
func TestDoneEventCarriesUnverifiedCitations(t *testing.T) {
	answerer := &stubAnswerer{events: []rag.Event{
		{Type: rag.EventDone, Citations: []rag.Citation{{Marker: "S1"}}, Result: &rag.Result{
			Grounded:   true,
			Unverified: []string{"S1"},
		}},
	}}

	rec := post(t, newTestHandler(answerer), `{"question":"q"}`)

	events := parseSSE(t, rec.Body.String())
	var payload donePayload
	if err := json.Unmarshal([]byte(events[len(events)-1][1]), &payload); err != nil {
		t.Fatalf("done payload is not valid JSON: %v", err)
	}
	if len(payload.Unverified) != 1 || payload.Unverified[0] != "S1" {
		t.Errorf("Unverified = %v, want [S1]", payload.Unverified)
	}
}

// An exhausted provider quota is not a fault in this service, and saying so is
// both true and actionable. The embedding quota takes down retrieval and
// therefore every answer, so this is the likeliest operational failure there is.
func TestQuotaExhaustionIsReportedAsItself(t *testing.T) {
	answerer := &stubAnswerer{
		events: []rag.Event{{Type: rag.EventSources, Citations: nil}},
		err:    fmt.Errorf("retrieve: %w: http 429", rag.ErrProviderQuota),
	}

	rec := post(t, newTestHandler(answerer), `{"question":"q"}`)

	events := parseSSE(t, rec.Body.String())
	last := events[len(events)-1]
	if last[0] != "error" {
		t.Fatalf("last event = %s, want error", last[0])
	}

	var payload errorPayload
	if err := json.Unmarshal([]byte(last[1]), &payload); err != nil {
		t.Fatalf("error payload is not valid JSON: %q", last[1])
	}
	if payload.Code != "quota_exhausted" {
		t.Errorf("code = %q, want quota_exhausted", payload.Code)
	}
	if !strings.Contains(payload.Message, "quota") {
		t.Errorf("message does not explain the cause: %q", payload.Message)
	}
}
