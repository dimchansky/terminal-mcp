package viewer

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// ErrStreamStopped means the stream was stopped; the handler must not write
// more than a final event and must return.
var ErrStreamStopped = errors.New("viewer: stream stopped")

// Stream guards one long-lived response so that stopping it is race-free.
// The handler sets every write deadline through Extend (a bounded deadline
// per write; the zero time means none and is for tests only); Stop (a
// Resource) sets a deadline the handler can no longer extend, which wakes a
// write blocked on a client that stopped reading. When the stream is
// attached to a session, the attachment's release already guarantees that
// no Stop runs afterwards; Release is for streams used without a session.
// Either way it must happen before the handler returns, so a keep-alive
// connection's next request is never given this stream's deadline.
type Stream struct {
	mu       sync.Mutex
	rc       *http.ResponseController
	stopped  bool
	released bool
	cause    error
	deadline time.Time
}

// NewStream wraps the handler's response writer.
func NewStream(w http.ResponseWriter) *Stream {
	return &Stream{rc: http.NewResponseController(w)}
}

// Extend sets the write deadline for the next write. After Stop it returns
// ErrStreamStopped and keeps the stop deadline.
func (s *Stream) Extend(deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.released {
		return ErrStreamStopped
	}
	return s.rc.SetWriteDeadline(deadline)
}

// Stop implements Resource.
func (s *Stream) Stop(cause error, deadline time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.released {
		return
	}
	s.stopped, s.cause, s.deadline = true, cause, deadline
	_ = s.rc.SetWriteDeadline(deadline)
}

// Stopped reports whether the stream was stopped, by when a final event must
// have been written, and why.
func (s *Stream) Stopped() (stopped bool, deadline time.Time, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped, s.deadline, s.cause
}

// Release ends the stream's control over the connection. It waits for an
// in-flight Stop and makes later Stop calls no-ops.
func (s *Stream) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = true
}
