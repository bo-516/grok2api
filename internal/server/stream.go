package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// sse writes Server-Sent Events. Headers stay unwritten until begin, so a failure
// that happens before grok message_start can still use a normal HTTP status.
// Keepalive comments go out when no event has been written for the interval.
type sse struct {
	w        http.ResponseWriter
	mu       sync.Mutex
	headered bool
	stop     chan struct{}
	reset    chan struct{}
	every    time.Duration
}

// newSSE wraps w. every is the keepalive interval. Zero disables keepalive.
func newSSE(w http.ResponseWriter, every time.Duration) *sse {
	return &sse{w: w, every: every}
}

// begin writes status 200 and the SSE headers, then the first event.
// setHeader may add X-Agent-Mock-* headers. begin is safe to call once;
// a second call returns an error so a handler cannot restart a stream.
func (s *sse) begin(setHeader func(http.Header)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headered {
		return fmt.Errorf("sse already started")
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	if setHeader != nil {
		setHeader(h)
	}
	s.w.WriteHeader(http.StatusOK)
	s.headered = true
	s.flush()
	s.armKeepalive()
	return nil
}

// Started reports whether begin has succeeded. The chat handler uses it to
// decide between an HTTP error and an in-band SSE error.
func (s *sse) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headered
}

// Send writes one data event and flushes it. It resets the keepalive timer.
// Calling Send before begin writes a body without SSE headers, so callers begin first.
func (s *sse) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.flush()
	s.nudge()
	return nil
}

// Comment writes an SSE comment. The OpenAI SDK ignores lines whose field name is empty.
func (s *sse) Comment(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.headered {
		return
	}
	_, _ = fmt.Fprintf(s.w, ": %s\n\n", text)
	s.flush()
}

// Done writes the terminal data: [DONE] event.
func (s *sse) Done() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flush()
}

// Close stops the keepalive goroutine. It does not close the HTTP body.
func (s *sse) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
}

// armKeepalive starts the idle comment loop. The caller holds s.mu.
func (s *sse) armKeepalive() {
	if s.every <= 0 || s.stop != nil {
		return
	}
	s.stop = make(chan struct{})
	s.reset = make(chan struct{}, 1)
	stop := s.stop
	reset := s.reset
	every := s.every
	go func() {
		timer := time.NewTimer(every)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-reset:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(every)
			case <-timer.C:
				s.Comment("keepalive")
				timer.Reset(every)
			}
		}
	}()
}

// nudge asks the keepalive loop to restart its interval. The caller holds s.mu.
func (s *sse) nudge() {
	if s.reset == nil {
		return
	}
	select {
	case s.reset <- struct{}{}:
	default:
	}
}

// flush pushes buffered bytes to the client. Missing Flusher support means the
// bytes still sit until the handler returns, so tests use a real HTTP server.
func (s *sse) flush() {
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
		return
	}
	_ = http.NewResponseController(s.w).Flush()
}
