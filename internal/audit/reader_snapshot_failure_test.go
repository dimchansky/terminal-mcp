package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A filesystem rename may succeed yet the wrapper can report a failure.
// The writer has not published a sealed segment or advanced NextArchive, so
// its old Active identity must still be readable at the future archive name.
func TestFrozenSnapshotRenameExecutedButReportedFailed(t *testing.T) {
	writer, reader, path := snapshotTestReader(t)
	nearlyFillObservedLog(t, writer)
	before, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	s := writer.sink.(*fileSink)
	failure := errors.New("rename reported failure")
	s.rename = func(source, archive string) error {
		if err := os.Rename(source, archive); err != nil {
			return err
		}
		return failure
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); !errors.Is(err, failure) {
		t.Fatalf("real rename did not report injected failure: %v", err)
	}
	unchangedWake(t, before.observation.Changed)
	after, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !after.observation.HasActive || after.observation.NextArchive != before.observation.NextArchive ||
		len(after.observation.Sealed) != 0 {
		t.Fatalf("reported rename failure falsely certified a new segment: %+v", after.observation)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original active name unexpectedly still exists: %v", err)
	}
	expected := path + ".1"
	if _, err := os.Stat(expected); err != nil {
		t.Fatal(err)
	}
	archived, err := reader.openFrozen(context.Background(), before, 1)
	if err != nil {
		t.Fatalf("frozen original active was lost after physical rename: %v", err)
	}
	t.Cleanup(func() { _ = archived.close() })
	if archived.size != before.observation.Active.Size ||
		archived.baseline != before.observation.Active.Baseline {
		t.Fatalf("previously certified bytes changed after unacknowledged rotation: %+v", archived)
	}
	// This archive was not sealed by the writer, so a fresh capture still
	// recognizes the original active identity at its one possible new name.
	post, err := reader.openCapturedActive(context.Background(), after)
	if err != nil {
		t.Fatalf("captured active could not follow a rename with failed return: %v", err)
	}
	if post.size != after.observation.Active.Size {
		t.Fatalf("captured active size moved after failed return: %d", post.size)
	}
	_ = post.close()
}

func TestFrozenSnapshotExistingArchiveCannotMasqueradeAsAnotherSuffix(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	legacy := []byte("{\"legacy\":true}\n")
	if err := os.WriteFile(path+".1", legacy, 0600); err != nil {
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
	before, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if before.observation.NextArchive != 2 {
		t.Fatal("fixture must have a preexisting archive")
	}
	// A hard link with identical file identity but a future archive suffix
	// must not be accepted as a new or certified historical segment.
	if err := os.Link(path+".1", path+".2"); err != nil {
		t.Skipf("filesystem cannot create hard-link test fixture: %v", err)
	}
	if _, err := reader.openFrozen(context.Background(), before, 2); !errors.Is(err, errOutsideSnapshot) {
		t.Fatalf("hard link at post-snapshot suffix was accepted: %v", err)
	}
}

// Replacing the expected archived inode with an unrelated file must never
// make openCapturedActive accept another generation at the same suffix.
func TestFrozenSnapshotRejectsForeignReplacementAtExpectedArchiveName(t *testing.T) {
	writer, reader, path := snapshotTestReader(t)
	nearlyFillObservedLog(t, writer)
	snap, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	archive := path + ".1"
	if err := os.Rename(archive, archive+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("{\"unrelated\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := reader.openCapturedActive(context.Background(), snap); !errors.Is(err, errOutsideSnapshot) {
		if opened != nil {
			_ = opened.close()
		}
		t.Fatalf("foreign archived inode accepted as captured active: %v", err)
	}
}
