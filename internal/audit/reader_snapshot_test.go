package audit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func snapshotTestReader(t *testing.T) (*Log, *Reader, string) {
	t.Helper()
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	return writer, reader, path
}

func TestFrozenSnapshotExcludesAppendAfterCapture(t *testing.T) {
	writer, reader, _ := snapshotTestReader(t)
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "before"})); err != nil {
		t.Fatal(err)
	}
	snap, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	originalSize := snap.observation.Active.Size
	if snap.observation.NextArchive != 1 || len(snap.observation.Sealed) != 0 {
		t.Fatalf("initial archive sequence incorrect: %+v", snap.observation)
	}
	// Append after capture, before the file is opened. This is deliberately
	// deterministic; the concurrent rotation stress test runs separately.
	if err := writer.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start",
		RequestID: "after", Args: map[string]any{"content": "not in snapshot"}})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-snap.observation.Changed: // The SAME frozen channel wakes live consumers.
	default:
		t.Fatal("append did not signal the captured snapshot channel")
	}
	opened, err := reader.openFrozen(context.Background(), snap, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.close() }()
	if opened.size != originalSize || opened.baseline != 0 {
		t.Fatalf("later append advanced frozen boundary: size=%d original=%d baseline=%d",
			opened.size, originalSize, opened.baseline)
	}
	info, err := opened.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= opened.size {
		t.Fatal("test did not append actual physical bytes after the observation")
	}
	if _, _, err := readRecordForward(context.Background(), opened.file, opened.size, opened.size); !errors.Is(err, io.EOF) {
		t.Fatalf("reader crossed frozen frontier: %v", err)
	}
	latest, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if latest.observation.Active.Size <= opened.size {
		t.Fatal("new observation did not advance")
	}
}

func TestFrozenSnapshotRecognizesOnlyCapturedActiveAfterDoubleRotation(t *testing.T) {
	writer, reader, _ := snapshotTestReader(t)
	nearlyFillObservedLog(t, writer) // Active A, exactly one byte below 4 MiB.
	first, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if first.observation.NextArchive != 1 || len(first.observation.Sealed) != 0 {
		t.Fatal("invalid first-generation archive snapshot")
	}
	// First rotation: A -> .1; active pathname now holds a new B.
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "in-B"})); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.openFrozen(context.Background(), first, 0); !errors.Is(err, errOutsideSnapshot) {
		t.Fatalf("new active B was included in snapshot of A: %v", err)
	}
	archivedA, err := reader.openFrozen(context.Background(), first, 1)
	if err != nil {
		t.Fatalf("old active A was not reachable at .NextArchive: %v", err)
	}
	if archivedA.size != first.observation.Active.Size || archivedA.baseline != first.observation.Active.Baseline {
		t.Fatalf("renamed A did not retain frozen boundary: %+v", archivedA)
	}
	_ = archivedA.close()
	capturedA, err := reader.openCapturedActive(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if capturedA.archive != first.observation.NextArchive || capturedA.size != first.observation.Active.Size {
		t.Fatalf("snapshot A did not follow its successful rename: %+v", capturedA)
	}
	_ = capturedA.close()
	second, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if second.observation.NextArchive != 2 || len(second.observation.Sealed) != 1 ||
		second.observation.Sealed[0].Archive != 1 {
		t.Fatalf("unexpected second-generation snapshot: %+v", second.observation)
	}
	bSize := second.observation.Active.Size
	// Second rotation: near-full event rotates small B into .2, and opens C.
	nearlyFillObservedLog(t, writer)
	if _, err := reader.openFrozen(context.Background(), first, 2); !errors.Is(err, errOutsideSnapshot) {
		t.Fatalf("post-snapshot archive .2 leaked into snapshot of A: %v", err)
	}
	archivedB, err := reader.openFrozen(context.Background(), second, 2)
	if err != nil {
		t.Fatalf("snapshot B could not follow later rename to .2: %v", err)
	}
	if archivedB.size != bSize || archivedB.baseline != second.observation.Active.Baseline {
		t.Fatalf("snapshot B read later bytes: %+v", archivedB)
	}
	_ = archivedB.close()
	capturedB, err := reader.openCapturedActive(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if capturedB.archive != second.observation.NextArchive || capturedB.size != bSize {
		t.Fatalf("snapshot B did not follow the second rename: %+v", capturedB)
	}
	_ = capturedB.close()
	if _, err := reader.openFrozen(context.Background(), second, 0); !errors.Is(err, errOutsideSnapshot) {
		t.Fatalf("snapshot B admitted newer C: %v", err)
	}
}

func TestFrozenSnapshotSeparatesHistoricalAndWriterOwnedArchives(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	legacy := []byte("{\"history\":\"unverified\"}\n")
	if err := os.WriteFile(path+".3", legacy, 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	old, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if old.observation.NextArchive != 4 || len(old.observation.Sealed) != 0 {
		t.Fatalf("first suffix must follow existing historical archives: %+v", old.observation)
	}
	archived, err := reader.openFrozen(context.Background(), old, 3)
	if err != nil {
		t.Fatal(err)
	}
	if archived.size != int64(len(legacy)) || archived.baseline != archived.size {
		t.Fatalf("older archive was falsely certified: %+v", archived)
	}
	_ = archived.close()
	// A new historical-looking archive at the next writer-owned suffix is
	// outside the old snapshot, even if its JSONL looks perfectly valid.
	if err := os.WriteFile(path+".4", legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.openFrozen(context.Background(), old, 4); !errors.Is(err, errOutsideSnapshot) {
		t.Fatalf("new archive accepted as old history: %v", err)
	}
	if err := os.Remove(path + ".4"); err != nil {
		t.Fatal(err)
	}
	nearlyFillObservedLog(t, writer)
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	frozen, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if frozen.observation.NextArchive != 5 || len(frozen.observation.Sealed) != 1 ||
		frozen.observation.Sealed[0].Archive != 4 {
		t.Fatalf("not a contiguous in-process suffix range: %+v", frozen.observation)
	}
	sealed, err := reader.openFrozen(context.Background(), frozen, 4)
	if err != nil {
		t.Fatal(err)
	}
	_ = sealed.close()
	// Rename original to keep its inode allocated and replace .4 with an
	// identical *byte sequence* but a different inode: identity must win.
	contents, err := os.ReadFile(path + ".4")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".4", path+".4.held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".4", contents, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.openFrozen(context.Background(), frozen, 4); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("writer-owned archive with replaced inode accepted: %v", err)
	}
}

func TestFrozenSnapshotWithoutActiveAfterFailedReopen(t *testing.T) {
	writer, reader, _ := snapshotTestReader(t)
	nearlyFillObservedLog(t, writer)
	s := writer.sink.(*fileSink)
	s.rename = func(before, after string) error {
		if err := os.Rename(before, after); err != nil {
			return err
		}
		return os.Mkdir(before, 0700) // Real rename succeeded; reopen now fails.
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err == nil {
		t.Fatal("expected fault-injected open failure")
	}
	snap, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.observation.HasActive || len(snap.observation.Sealed) != 1 || snap.observation.NextArchive != 2 {
		t.Fatalf("failed reopen snapshot did not seal old confirmed bytes: %+v", snap.observation)
	}
	if _, err := reader.openFrozen(context.Background(), snap, 0); !errors.Is(err, errOutsideSnapshot) {
		t.Fatalf("absent active opened directory/new generation: %v", err)
	}
	archived, err := reader.openFrozen(context.Background(), snap, 1)
	if err != nil || archived.size != snap.observation.Sealed[0].Size {
		t.Fatalf("recovery made previously acknowledged history unavailable: %+v err %v", archived, err)
	}
	_ = archived.close()
}

func TestFrozenSnapshotAfterRenameFailureKeepsOriginalActive(t *testing.T) {
	writer, reader, _ := snapshotTestReader(t)
	nearlyFillObservedLog(t, writer)
	s := writer.sink.(*fileSink)
	s.rename = func(string, string) error { return errors.New("injected rename failure") }
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err == nil {
		t.Fatal("rename failure was not injected")
	}
	snap, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.observation.HasActive || snap.observation.NextArchive != 1 || len(snap.observation.Sealed) != 0 {
		t.Fatalf("failed rename falsely advanced archive generation: %+v", snap.observation)
	}
	opened, err := reader.openFrozen(context.Background(), snap, 0)
	if err != nil || opened.size != snap.observation.Active.Size {
		t.Fatalf("failed rename lost active committed boundary: %+v err %v", opened, err)
	}
	_ = opened.close()
}

func TestFrozenSnapshotConcurrentPublicationsRemainAtomic(t *testing.T) {
	writer, reader, _ := snapshotTestReader(t)
	nearlyFillObservedLog(t, writer)
	const writers, entries = 4, 75
	payload := strings.Repeat("a", 16<<10) // Enforce at least two real rotations.
	var group sync.WaitGroup
	for i := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := range entries {
				err := writer.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start",
					RequestID: fmt.Sprintf("t-%d-%d", i, j),
					Args:      map[string]any{"content": payload}}))
				if err != nil {
					t.Errorf("append failed: %v", err)
					return
				}
			}
		}()
	}
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			var previous Observation
			for range 120 {
				snap, err := reader.captureSnapshot()
				if err != nil {
					t.Errorf("torn snapshot: %v", err)
					return
				}
				s := snap.observation
				if s.NextArchive < uint64(len(s.Sealed))+1 || s.NextArchive-uint64(len(s.Sealed)) != 1 ||
					s.Changed == nil || s.NextArchive < previous.NextArchive ||
					len(s.Sealed) < len(previous.Sealed) {
					t.Errorf("writer suffix/segments/channel regressed or were published torn: %+v", s)
					return
				}
				if s.HasActive {
					opened, err := reader.openCapturedActive(context.Background(), snap)
					if err != nil || opened == nil || opened.size != s.Active.Size || opened.baseline != s.Active.Baseline {
						t.Errorf("captured active boundary changed under concurrent rotation: %+v err %v", opened, err)
						return
					}
					_ = opened.close()
				}
				for _, sealed := range s.Sealed {
					opened, err := reader.openFrozen(context.Background(), snap, sealed.Archive)
					if err != nil || opened == nil || opened.size != sealed.Size || opened.baseline != sealed.Baseline {
						t.Errorf("sealed writer boundary changed: %+v err %v", opened, err)
						return
					}
					_ = opened.close()
				}
				previous = s
			}
		}()
	}
	group.Wait()
	final, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if final.observation.NextArchive < 3 || len(final.observation.Sealed) < 2 {
		t.Fatalf("stress test must exercise at least two real rotations: %+v", final.observation)
	}
}
