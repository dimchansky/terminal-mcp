package viewer

import (
	"context"
	"sync"
	"time"
)

// Resource is something a viewer request holds on behalf of a session: a
// projection window, a live stream, a full-record read. Stop asks it to end
// by deadline; it is called outside registry locks, at most once, and must
// only signal and wake the owning handler (for example by setting a write
// deadline and canceling), never tear down state the handler may be using.
//
// Rules that keep shutdown free of deadlocks: Stop must not block (it runs
// synchronously before HTTP shutdown), must not call its own release, and
// must not take a lock that the handler holds while calling release.
type Resource interface {
	Stop(cause error, deadline time.Time)
}

// Attacher is the session interface the dashboard adapter (#7) and stream
// (#8) code against; viewertest provides a fake.
type Attacher interface {
	// Context is canceled with the session's end cause: ErrLoggedOut,
	// ErrExpired, ErrRevoked, ErrReplaced or *StopError.
	Context() context.Context
	// Attach registers a resource of a kind within the configured limits
	// (ErrCapacity) while the session is open (ErrSessionClosed). The
	// returned release is idempotent; after it returns, Stop is not running
	// and will not be called. Call it before the handler returns, and not
	// while holding a lock Stop may take. For a stream:
	//
	//	st := viewer.NewStream(w)
	//	release, err := sess.Attach(viewer.KindStream, st)
	//	...
	//	defer release()
	//
	// Any viewer response that can exceed a few KiB (history pages, full
	// records, streams) must be written through an attached Stream, so a
	// client that stopped reading cannot hold up server shutdown.
	Attach(kind Kind, res Resource) (release func(), err error)
}

// Session is one authenticated viewer (one browser tab).
type Session struct {
	registry *Registry
	id       uint64
	hash     digest
	ctx      context.Context
	cancel   context.CancelCauseFunc

	// Guarded by registry.mu.
	timer     Timer
	lastUsed  time.Time
	idle      time.Time
	absolute  time.Time
	closed    bool
	resources map[*attachment]bool
	counts    map[Kind]int
}

var _ Attacher = (*Session)(nil)

type attachment struct {
	kind Kind
	res  Resource
	mu   sync.Mutex // Held while Stop runs; release waits for it.
	done bool       // Released or stopped.
}

// ID is a small ordinal for logs; it is not a credential.
func (s *Session) ID() uint64 { return s.id }

// Context implements Attacher.
func (s *Session) Context() context.Context { return s.ctx }

// Expiry returns the idle and absolute expiry times.
func (s *Session) Expiry() (idle, absolute time.Time) {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	return s.idle, s.absolute
}

// Attach implements Attacher.
func (s *Session) Attach(kind Kind, res Resource) (func(), error) {
	r := s.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.closed {
		return nil, ErrSessionClosed
	}
	limit, ok := r.cfg.Limits[kind]
	if !ok || s.counts[kind] >= limit.PerSession || r.resources[kind] >= limit.Total {
		return nil, ErrCapacity
	}
	a := &attachment{kind: kind, res: res}
	s.resources[a] = true
	s.counts[kind]++
	r.resources[kind]++
	var once sync.Once
	return func() { once.Do(func() { s.release(a) }) }, nil
}

func (s *Session) release(a *attachment) {
	a.mu.Lock() // Waits for an in-flight Stop.
	a.done = true
	a.mu.Unlock()
	r := s.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.resources[a] {
		delete(s.resources, a)
		s.counts[a.kind]--
		r.resources[a.kind]--
	}
}

// finish ends a session detached from the registry: it cancels the context
// and stops every resource still attached, outside registry locks.
func (s *Session) finish(cause error, deadline time.Time) {
	r := s.registry
	r.mu.Lock()
	if s.closed {
		r.mu.Unlock()
		return
	}
	s.closed = true
	attached := make([]*attachment, 0, len(s.resources))
	for a := range s.resources {
		attached = append(attached, a)
		delete(s.resources, a)
		s.counts[a.kind]--
		r.resources[a.kind]--
	}
	r.mu.Unlock()
	s.cancel(cause)
	for _, a := range attached {
		a.mu.Lock()
		if !a.done {
			a.done = true
			a.res.Stop(cause, deadline)
		}
		a.mu.Unlock()
	}
}
