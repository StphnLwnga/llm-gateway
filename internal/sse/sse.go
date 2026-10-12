// Package sse writes Server-Sent Events, with a write deadline on every event.
package sse

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrClientGone marks a failed write or flush: the client closed the
// connection or stopped reading.
var ErrClientGone = errors.New("client disconnected")

// Writer sends Server-Sent Events. Each write gets its own deadline, so a
// client that stops reading cannot hold the handler forever.
type Writer struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

// NewWriter returns a Writer that gives each event timeout to be written.
func NewWriter(w http.ResponseWriter, timeout time.Duration) *Writer {
	return &Writer{w: w, rc: http.NewResponseController(w), timeout: timeout}
}

// Event writes v as one JSON data event and flushes it. JSON has no raw
// newlines, so the payload cannot break the event framing.
func (s *Writer) Event(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("sse: encode event: %w", err)
	}
	return s.write("data: " + string(b) + "\n\n")
}

// Done writes the OpenAI end-of-stream event.
func (s *Writer) Done() error {
	return s.write("data: [DONE]\n\n")
}

// Comment writes a comment line, which clients ignore; it keeps idle
// connections and proxies from timing out.
func (s *Writer) Comment(text string) error {
	return s.write(": " + text + "\n\n")
}

func (s *Writer) write(p string) error {
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	if _, err := io.WriteString(s.w, p); err != nil {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	if err := s.rc.Flush(); err != nil { // unlike http.Flusher, this reports a failed flush
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	// A deadline left in place would expire while the stream waits upstream,
	// and then break the final write when the handler returns.
	if err := s.rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	return nil
}
