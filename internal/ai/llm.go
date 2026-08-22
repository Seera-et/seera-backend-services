// Package ai is the provider boundary. It defines the LLM and Embedder
// interfaces the rest of the system programs against, plus the vendor adapters
// that implement them.
//
// Vendor SDK types never leave this package. Swapping a provider is a wiring
// change in cmd/api, not a change to rag, search, or ingestion.
package ai

import (
	"context"
	"strings"
)

// Role of a message in a conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn.
type Message struct {
	Role    Role
	Content string
}

// Request is a generation request.
//
// The System prompt carries instructions and the retrieved sources block. It
// never carries legal text of its own: every provision the model sees comes
// from retrieval, or it does not exist.
type Request struct {
	System    string
	Messages  []Message
	MaxTokens int

	// JSONSchema constrains the response to a shape, for calls whose output is
	// read by code rather than by a person — routing decisions, retrieval plans.
	//
	// It is a provider-agnostic subset of JSON Schema: object, string, boolean,
	// integer, array, and enum. Adapters translate it. Nil means free text.
	//
	// Asking for a shape is not the same as trusting it: a caller still validates
	// what comes back, because a provider that ignores the schema fails loudly
	// rather than silently.
	JSONSchema *Schema
}

// Schema is the subset of JSON Schema the providers agree on.
type Schema struct {
	Type        string             `json:"type"`
	Description string             `json:"description,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Required    []string           `json:"required,omitempty"`
}

// EventType discriminates the stream events.
type EventType string

const (
	// EventText carries an incremental piece of the answer.
	EventText EventType = "text"
	// EventDone is emitted once, last, and carries final usage.
	EventDone EventType = "done"
)

// Event is one item from a generation stream.
type Event struct {
	Type  EventType
	Text  string // set when Type is EventText
	Usage *Usage // set when Type is EventDone, if the provider reports it

	// Stop says why generation ended, on EventDone. Empty when the provider did
	// not say.
	Stop StopReason
}

// StopReason is why a generation ended.
//
// It matters because the alternative is silence: an answer cut off at the token
// limit reads as a finished one, and on a legal answer a sentence that stops
// halfway through a condition is worse than no sentence.
type StopReason string

const (
	// StopComplete — the model finished what it was saying.
	StopComplete StopReason = "complete"

	// StopMaxTokens — the output limit was reached. The answer is incomplete.
	StopMaxTokens StopReason = "max_tokens"

	// StopBlocked — the provider stopped for its own safety or policy reasons.
	StopBlocked StopReason = "blocked"

	// StopOther — the provider gave a reason this code does not know.
	StopOther StopReason = "other"
)

// ParseStopReason maps a provider's finish reason onto ours.
func ParseStopReason(raw string) StopReason {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", "STOP", "FINISH_REASON_STOP":
		return StopComplete
	case "MAX_TOKENS", "FINISH_REASON_MAX_TOKENS", "LENGTH":
		return StopMaxTokens
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return StopBlocked
	default:
		return StopOther
	}
}

// Usage is per-request token accounting, for the latency and cost logs.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Stream is a pull-based iterator over generation events.
//
// Usage is the standard scanner shape:
//
//	s, err := llm.Stream(ctx, req)
//	if err != nil { return err }
//	defer s.Close()
//	for s.Next() {
//	    ev := s.Event()
//	    ...
//	}
//	return s.Err()
//
// Next returns false on both normal completion and failure; Err distinguishes
// them and must be checked. Close is always safe to call and releases the
// underlying connection.
type Stream interface {
	Next() bool
	Event() Event
	Err() error
	Close() error
}

// LLM generates text.
//
// Stream is the answer path: time-to-first-token is a product requirement, so
// the user-facing answer is never assembled before it starts arriving.
//
// Complete is for the calls whose output is consumed by code before anything
// reaches the user — the routing decision, and any reply that has to be checked
// before it is shown. Streaming those would mean emitting text the caller might
// have to retract.
type LLM interface {
	Stream(ctx context.Context, req Request) (Stream, error)

	// Complete returns the whole response. Usage may be nil if the provider does
	// not report it.
	Complete(ctx context.Context, req Request) (string, *Usage, error)

	// Model identifies the model actually in use, for logs and eval reports.
	Model() string
}
