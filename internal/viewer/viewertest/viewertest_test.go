package viewertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"terminalmcp/internal/viewer"
)

type stopRecorder struct {
	cause    error
	deadline time.Time
	calls    int
}

func (s *stopRecorder) Stop(cause error, deadline time.Time) {
	s.cause, s.deadline = cause, deadline
	s.calls++
}

func TestFakeSessionFollowsTheAttachContract(t *testing.T) {
	s := NewSession(map[viewer.Kind]viewer.Limit{viewer.KindWindow: {PerSession: 1, Total: 1}})
	a, b := &stopRecorder{}, &stopRecorder{}
	relA, err := s.Attach(viewer.KindWindow, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach(viewer.KindWindow, b); !errors.Is(err, viewer.ErrCapacity) {
		t.Fatalf("limit: %v", err)
	}
	s.FailNextAttach(viewer.ErrCapacity)
	relA()
	relA()
	if _, err := s.Attach(viewer.KindWindow, b); !errors.Is(err, viewer.ErrCapacity) {
		t.Fatalf("injected global capacity: %v", err)
	}
	if _, err := s.Attach(viewer.KindWindow, b); err != nil || s.Attached(viewer.KindWindow) != 1 {
		t.Fatalf("after release: %v", err)
	}
	deadline := time.Now()
	s.End(viewer.ErrExpired, deadline)
	s.End(viewer.ErrRevoked, deadline)
	if a.calls != 0 || b.calls != 1 || !errors.Is(b.cause, viewer.ErrExpired) || !b.deadline.Equal(deadline) {
		t.Fatalf("stops a=%+v b=%+v", a, b)
	}
	if !errors.Is(context.Cause(s.Context()), viewer.ErrExpired) {
		t.Fatalf("cause %v", context.Cause(s.Context()))
	}
	if _, err := s.Attach(viewer.KindWindow, a); !errors.Is(err, viewer.ErrSessionClosed) {
		t.Fatalf("attach after end: %v", err)
	}
}
