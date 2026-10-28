package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mustObserve(t *testing.T, l *Log) Observation {
	t.Helper()
	o := l.Observe()
	if o.Disabled {
		t.Fatal("journal observation unexpectedly disabled")
	}
	return o
}

func unchangedWake(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("audit observer woke without acknowledged new data")
	default:
	}
}

func woke(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatal("audit observer missed successful commit or close")
	}
}

func TestObservationInitialHistoryAndWake(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	history := []byte("{\"old\":true}\n")
	if err := os.WriteFile(path, history, 0600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	original := mustObserve(t, l)
	if !original.HasActive || original.Active.Size != int64(len(history)) ||
		original.Active.Baseline != int64(len(history)) || len(original.Sealed) != 0 {
		t.Fatalf("initial existing bytes must be historical: %+v", original)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	_ = f.Close()
	if err != nil || !original.Active.SameFile(info) {
		t.Fatalf("recorded identity was not taken from the open journal: %v", err)
	}
	unchangedWake(t, original.Changed)
	if err := l.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	woke(t, original.Changed)
	next := mustObserve(t, l)
	if next.Active.Baseline != int64(len(history)) || next.Active.Size <= original.Active.Size ||
		!next.Active.SameFile(info) {
		t.Fatalf("successful append was not published: old=%+v new=%+v", original, next)
	}
	unchangedWake(t, next.Changed)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	woke(t, next.Changed)
	done := mustObserve(t, l)
	if !done.Closed {
		t.Fatal("Close did not publish the terminal state")
	}
	woke(t, done.Changed)
}

func TestObservationRefusesFailedCompleteAppendAndFailedRepair(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	s := l.sink.(*fileSink)
	old := mustObserve(t, l)
	underlying := s.f
	injected := errors.New("complete write reported failed")
	s.f = &faultFile{
		auditFile: underlying,
		writeFn: func(data []byte) (int, error) {
			n, err := underlying.Write(data)
			if err != nil {
				return n, err
			}
			return n, injected
		},
		truncateFn: func(int64) error {
			return errors.New("injected truncate failure")
		},
	}
	if err := l.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": "unacknowledged private value"}})); !errors.Is(err, injected) {
		t.Fatalf("expected failed complete write, got %v", err)
	}
	after := mustObserve(t, l)
	if after.Active.Size != old.Active.Size || after.Active.Baseline != old.Active.Baseline {
		t.Fatalf("failed complete line advanced frontier: %+v", after)
	}
	unchangedWake(t, old.Changed)
	physical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(physical, []byte("unacknowledged private value")) ||
		physical[len(physical)-1] != '\n' {
		t.Fatal("test must exercise a physically complete, unacknowledged line")
	}
	s.f = underlying
	if err := l.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal("repair and next append must recover", err)
	}
	woke(t, old.Changed)
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(saved, []byte("unacknowledged private value")) ||
		int64(len(saved)) != mustObserve(t, l).Active.Size {
		t.Fatal("recovery exposed or retained a rejected full line")
	}
}

func TestObservationRotationRelocationAfterFailedNewAppend(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	// Fill the file to within two bytes of the 4 MiB boundary, rather
	// than guessing the current event metadata length.
	large := NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": ""}})
	empty, err := json.Marshal(large)
	if err != nil {
		t.Fatal(err)
	}
	large.Args = map[string]any{"content": strings.Repeat("a", int(maxFileBytes)-len(empty)-2)}
	if err := l.Start(large); err != nil {
		t.Fatal(err)
	}
	old := mustObserve(t, l)
	if old.Active.Size < maxFileBytes-1024 {
		t.Fatalf("test event did not nearly fill the file: %d", old.Active.Size)
	}
	s := l.sink.(*fileSink)
	s.rename = func(before, after string) error {
		if err := os.Rename(before, after); err != nil {
			return err
		}
		// Rotation has succeeded, but opening a new active file must fail.
		return os.Mkdir(before, 0700)
	}
	newEvent := NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})
	if err := l.Start(newEvent); err == nil {
		t.Fatal("expected failure after successful archival rename")
	}
	wake := mustObserve(t, l)
	if wake.HasActive || len(wake.Sealed) != 1 ||
		wake.Sealed[0].Archive != 1 ||
		wake.Sealed[0].Size != old.Active.Size {
		// SameFile tested separately below; avoid relying on path-based stat.
		t.Fatalf("sealed segment not published after failed new append: %+v", wake)
	}
	unchangedWake(t, old.Changed)
	archive, err := os.Open(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	stat, err := archive.Stat()
	_ = archive.Close()
	if err != nil || !wake.Sealed[0].SameFile(stat) {
		t.Fatalf("archive identity changed across rename: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.rename = os.Rename
	if err := l.Start(newEvent); err != nil {
		t.Fatal("could not append after rotation open failure", err)
	}
	woke(t, old.Changed)
	recovered := mustObserve(t, l)
	if !recovered.HasActive || len(recovered.Sealed) != 1 ||
		recovered.Active.Size == 0 || recovered.Active.Baseline != 0 {
		t.Fatalf("recovered append/snapshot incorrect: %+v", recovered)
	}
	// Modifying a caller-owned snapshot cannot change the published slice.
	recovered.Sealed[0].Size = 0
	if mustObserve(t, l).Sealed[0].Size == 0 {
		t.Fatal("Observe returned a mutable shared sealed-slice")
	}
}

func TestObservationConcurrentReadersWritersAndClose(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const producers = 8
	var wg sync.WaitGroup
	for range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 90 {
				if err := l.Write(NewEvent(Event{Tool: "file_read", Phase: "request_finish",
					RequestID: fmt.Sprintf("%d", i)})); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3000 {
				ob := l.Observe()
				if ob.Disabled || ob.Active.Size < ob.Active.Baseline {
					t.Errorf("torn audit observer snapshot: %+v", ob)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if !l.Observe().Closed || !(&Log{}).Observe().Disabled || !(*Log)(nil).Observe().Disabled {
		t.Fatal("invalid closed or disabled observation")
	}
}

func BenchmarkAuditObservationAppend(b *testing.B) {
	for _, size := range []int{0, 1 << 20} {
		b.Run(fmt.Sprintf("args_bytes_%d", size), func(b *testing.B) {
			dir := filepath.Join(b.TempDir(), "private-audit")
			if err := os.Mkdir(dir, 0700); err != nil {
				b.Fatal(err)
			}
			path := filepath.Join(dir, "audit.jsonl")
			l, err := OpenWithLimit(path, 0) // Explicitly unlimited for benchmark runs.
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			event := NewEvent(Event{Tool: "file_write", Phase: "request_start",
				Args: map[string]any{"content": strings.Repeat("a", size)}})
			encoded, err := json.Marshal(event)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(encoded) + 1))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := l.Start(event); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
