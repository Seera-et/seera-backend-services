package qa

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrClientGone is returned when the caller has disconnected. It stops the
// answer rather than paying for tokens nobody will read.
var ErrClientGone = errors.New("client disconnected")

// sseWriter writes Server-Sent Events.
//
// It lives here rather than in internal/server because internal/server imports
// this package, and the reverse edge would be a cycle. When a second feature
// needs SSE it moves to pkg/ — one consumer is not yet a shared concern.
type sseWriter struct {
	w       http.ResponseWriter
	buf     *bufio.Writer
	flusher http.Flusher
}

// newSSEWriter prepares the response for streaming.
//
// It fails if the ResponseWriter cannot flush, because without flushing every
// event would sit in a buffer until the answer finished — which looks exactly
// like a server that streams nothing.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("response writer does not support flushing")
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Tells nginx and friends not to buffer the response; without it a proxy
	// happily undoes all of this.
	h.Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)

	s := &sseWriter{w: w, buf: bufio.NewWriter(w), flusher: flusher}

	// An initial comment opens the stream immediately, so the client's connection
	// is confirmed before retrieval starts rather than after it.
	if err := s.raw(": stream open\n\n"); err != nil {
		return nil, err
	}
	return s, nil
}

// send writes one named event with a JSON payload.
func (s *sseWriter) send(event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", event, err)
	}
	// The payload is JSON, which never contains a raw newline, so a single
	// data: line is always valid — this is why token text is wrapped in an
	// object rather than sent bare.
	return s.raw("event: " + event + "\ndata: " + string(body) + "\n\n")
}

func (s *sseWriter) raw(text string) error {
	if _, err := s.buf.WriteString(text); err != nil {
		return fmt.Errorf("%w: %v", ErrClientGone, err)
	}
	if err := s.buf.Flush(); err != nil {
		return fmt.Errorf("%w: %v", ErrClientGone, err)
	}
	s.flusher.Flush()
	return nil
}
