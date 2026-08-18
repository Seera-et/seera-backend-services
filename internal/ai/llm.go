// Package ai is the provider boundary. It defines the LLM and Embedder
// interfaces the rest of the system programs against, plus the vendor adapters
// that implement them.
//
// Vendor SDK types never leave this package. Swapping a provider is a wiring
// change in cmd/api, not a change to rag, search, or ingestion.
package ai

import "context"

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

// LLM generates grounded answers. Only streaming is exposed: time-to-first-token
// is a product requirement, so there is no non-streaming path to accidentally
// take.
type LLM interface {
	Stream(ctx context.Context, req Request) (Stream, error)

	// Model identifies the model actually in use, for logs and eval reports.
	Model() string
}
