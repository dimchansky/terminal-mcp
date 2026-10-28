package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nearlyFillObservedLog(t *testing.T, l *Log) {
	t.Helper()
	large := NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": ""}})
	empty, err := json.Marshal(large)
	if err != nil {
		t.Fatal(err)
	}
	large.Args = map[string]any{"content": strings.Repeat("x", int(maxFileBytes)-len(empty)-2)}
	if err := l.Start(large); err != nil {
		t.Fatal(err)
	}
	if got := l.Observe().Active.Size; got != maxFileBytes-1 {
		t.Fatalf("file was not filled to the intended boundary: %d", got)
	}
}

func TestObservationFailedPartialAndZeroWrites(t *testing.T) {
	for _, tc := range []struct {
		name         string
		writePrefix  int
		failTruncate bool
	}{
		{"short-repaired", 11, false},
		{"short-failed-truncate", 11, true},
		{"zero", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(privateAuditDir(t), "audit.jsonl")
			l, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			s := l.sink.(*fileSink)
			real := s.f
			failure := errors.New("partial write failed")
			s.f = &faultFile{
				auditFile: real,
				writeFn: func(data []byte) (int, error) {
					n := min(tc.writePrefix, len(data))
					if n == 0 {
						return 0, failure
					}
					got, err := real.Write(data[:n])
					if err != nil {
						return got, err
					}
					return got, failure
				},
				truncateFn: func(n int64) error {
					if tc.failTruncate {
						return errors.New("failed truncate")
					}
					return real.Truncate(n)
				},
			}
			old := mustObserve(t, l)
			if err := l.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start",
				Args: map[string]any{"content": "do not admit"}})); !errors.Is(err, failure) {
				t.Fatalf("expected injected failure, got %v", err)
			}
			unchangedWake(t, old.Changed)
			if got := l.Observe().Active.Size; got != old.Active.Size {
				t.Fatalf("denied append advanced frontier: %d", got)
			}
			s.f = real
			good := NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})
			if err := l.Start(good); err != nil {
				t.Fatal("append should recover after repairing tail", err)
			}
			woke(t, old.Changed)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(good)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, append(want, '\n')) ||
				int64(len(data)) != l.Observe().Active.Size {
				t.Fatal("failed write leaked into the acknowledged journal")
			}
		})
	}
}

func TestObservationRenameAndCloseRotationFailure(t *testing.T) {
	for _, fail := range []string{"rename", "close"} {
		t.Run(fail, func(t *testing.T) {
			path := filepath.Join(privateAuditDir(t), "audit.jsonl")
			l, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			nearlyFillObservedLog(t, l)
			s := l.sink.(*fileSink)
			old := mustObserve(t, l)
			originalFile := s.f
			switch fail {
			case "rename":
				s.rename = func(string, string) error { return errors.New("injected rename failure") }
			case "close":
				s.f = &faultFile{auditFile: originalFile, closeFn: func() error {
					if err := originalFile.Close(); err != nil {
						return err
					}
					return errors.New("injected close failure")
				}}
			}
			next := NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})
			if err := l.Start(next); err == nil {
				t.Fatal("rotation must fail")
			}
			failed := mustObserve(t, l)
			if !failed.HasActive || len(failed.Sealed) != 0 ||
				failed.Active.Size != old.Active.Size || s.next != 1 {
				t.Fatalf("failed rotation moved or lost confirmed segment: %+v", failed)
			}
			unchangedWake(t, old.Changed)
			original, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			stat, err := original.Stat()
			_ = original.Close()
			if err != nil || !failed.Active.SameFile(stat) {
				t.Fatalf("original active identity lost: %v", err)
			}
			s.rename = os.Rename
			if err := l.Start(next); err != nil {
				t.Fatal("subsequent append should recover", err)
			}
			woke(t, old.Changed)
			recovered := mustObserve(t, l)
			if len(recovered.Sealed) != 1 ||
				recovered.Sealed[0].Baseline != old.Active.Size ||
				recovered.Sealed[0].Size != old.Active.Size {
				t.Fatalf("reopened original should be conservatively historical: %+v", recovered)
			}
		})
	}
}

func TestObservationDeniedBeforeAppendAndFakeSink(t *testing.T) {
	l := newLog(&testSink{write: func([]byte) error { return nil }})
	if o := l.Observe(); !o.Disabled {
		t.Fatalf("fake sink exposes journal observation: %+v", o)
	}
	woke(t, l.Observe().Changed) // Disabled observers must never wait forever.
	if err := l.Write(NewEvent(Event{Tool: "terminal_list", Phase: "request_finish"})); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if !l.Observe().Disabled {
		t.Fatal("fake sink became observable")
	}

	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	full, err := OpenWithLimit(path, 512)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = full.Close() }()
	original := mustObserve(t, full)
	tooLarge := NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": strings.Repeat("z", int(maxFileBytes))}})
	if err := full.Start(tooLarge); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("oversize pre-effect start was admitted: %v", err)
	}
	unchangedWake(t, original.Changed)
	if err := full.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": strings.Repeat("q", 2048)}})); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("hard audit cap not enforced: %v", err)
	}
	unchangedWake(t, original.Changed)
	if full.Observe().Active.Size != 0 {
		t.Fatal("pre-write rejection advanced the frontier")
	}
}

func TestObservationNoDoubleWakeAfterRepeatClose(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	original := mustObserve(t, l)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	woke(t, original.Changed)
	finished := mustObserve(t, l)
	if err := l.Close(); err != nil {
		t.Fatal("repeated close must be idempotent", err)
	}
	if newer := l.Observe(); newer.Changed != finished.Changed || !newer.Closed {
		t.Fatalf("second close published or reclosed terminal signal: %+v", newer)
	}
}
