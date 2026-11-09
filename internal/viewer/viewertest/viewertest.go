// Package viewertest provides a fake viewer session for code that attaches
// resources to sessions (the dashboard adapter and stream).
package viewertest

import (
	"context"
	"sync"
	"time"

	"terminalmcp/internal/viewer"
)

// Session is a fake viewer.Attacher with the same limits and stop contract
// as a real session; End ends it like logout, expiry or a server stop.
type Session struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu        sync.Mutex
	limits    map[viewer.Kind]viewer.Limit
	attachErr error
	closed    bool
	resources map[*attached]bool
	counts    map[viewer.Kind]int
}

type attached struct {
	kind viewer.Kind
	res  viewer.Resource
	mu   sync.Mutex
	done bool
}

var _ viewer.Attacher = (*Session)(nil)

// NewSession returns an open fake session; nil limits select the defaults.
// Per-session limits apply (there is no global total in a fake).
func NewSession(limits map[viewer.Kind]viewer.Limit) *Session {
	if limits == nil {
		limits = viewer.DefaultLimits()
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Session{ctx: ctx, cancel: cancel, limits: limits,
		resources: make(map[*attached]bool), counts: make(map[viewer.Kind]int)}
}

// Context implements viewer.Attacher.
func (s *Session) Context() context.Context { return s.ctx }

// Attach implements viewer.Attacher.
func (s *Session) Attach(kind viewer.Kind, res viewer.Resource) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, viewer.ErrSessionClosed
	}
	if err := s.attachErr; err != nil {
		s.attachErr = nil
		return nil, err
	}
	limit, ok := s.limits[kind]
	if !ok || s.counts[kind] >= limit.PerSession {
		return nil, viewer.ErrCapacity
	}
	a := &attached{kind: kind, res: res}
	s.resources[a] = true
	s.counts[kind]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.done = true
			a.mu.Unlock()
			s.mu.Lock()
			if s.resources[a] {
				delete(s.resources, a)
				s.counts[a.kind]--
			}
			s.mu.Unlock()
		})
	}, nil
}

// FailNextAttach makes the next Attach return err, for example
// viewer.ErrCapacity when other sessions hold the global total.
func (s *Session) FailNextAttach(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachErr = err
}

// Attached reports how many resources of kind are attached.
func (s *Session) Attached(kind viewer.Kind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[kind]
}

// End ends the session with cause, stopping attached resources by deadline.
func (s *Session) End(cause error, deadline time.Time) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	var list []*attached
	for a := range s.resources {
		list = append(list, a)
		delete(s.resources, a)
		s.counts[a.kind]--
	}
	s.mu.Unlock()
	s.cancel(cause)
	for _, a := range list {
		a.mu.Lock()
		if !a.done {
			a.done = true
			a.res.Stop(cause, deadline)
		}
		a.mu.Unlock()
	}
}
