package audit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReaderActiveSnapshotDoesNotReadNewFileUnderOldFrontier(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	nearlyFillObservedLog(t, writer)
	r, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	old := writer.Observe()
	opened, err := r.openObservedActive(context.Background(), func() {
		// Rotates A into .1 and opens B at the same active pathname between
		// reading the immutable A snapshot and opening that pathname.
		if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.close() }()
	current := writer.Observe()
	if !current.HasActive || old.Active.SameFile(current.Active.identity) ||
		!current.Active.SameFile(opened.fileInfo(t)) ||
		opened.size != current.Active.Size {
		t.Fatalf("reader used stale frontier on new active file: old=%+v current=%+v", old, current)
	}
}

func (segment *readerSegment) fileInfo(t *testing.T) os.FileInfo {
	t.Helper()
	info, err := segment.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestReaderCursorFindsRenamedArchiveAndDetectsReplacement(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	nearlyFillObservedLog(t, writer)
	r, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	active, err := r.openObservedActive(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := readRecordForward(context.Background(), active.file, 0, active.size)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodePageCursor(newPageCursor(Forward, active.key, 0, active.size, cursorProof(original)))
	_ = active.close()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.resolveCursor(context.Background(), cursor)
	if err != nil {
		t.Fatalf("cursor did not follow active journal into archive: %v", err)
	}
	if resolved.archive != 1 || !bytes.Equal(original, mustReadOne(t, resolved)) {
		t.Fatal("resolved the wrong archived record")
	}
	_ = resolved.close()

	// Removing an archive makes an exact old position unavailable; never
	// attribute it to a newer file reusing the same suffix.
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolveCursor(context.Background(), cursor); !errors.Is(err, ErrInvalidCursor) {
		// Active hints may migrate; when no replacement is found, resync is
		// conservative. An explicit suffix-hinted cursor may report a gap.
		t.Fatalf("vanished active-hinted cursor unexpectedly resolved: %v", err)
	}
	// Reuse the deleted suffix in a fresh writer instance.
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	nearlyFillObservedLog(t, reopened)
	if err := reopened.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	r2, err := NewReader(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.resolveCursor(context.Background(), cursor); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("reused suffix silently matched old cursor: %v", err)
	}
}

func mustReadOne(t *testing.T, segment *readerSegment) []byte {
	t.Helper()
	line, _, err := readRecordForward(context.Background(), segment.file, 0, segment.size)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestReaderArchiveSuffixDiscoveryIsSparseAndOrdered(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	for _, suffix := range []string{"1", "4", "bogus", "03"} {
		if err := os.WriteFile(path+"."+suffix, []byte("{\"archive\":true}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	suffixes, err := reader.archiveSuffixes(context.Background())
	if err != nil || len(suffixes) != 2 || suffixes[0] != 1 || suffixes[1] != 4 {
		t.Fatalf("sparse archive suffix discovery: %v %v", suffixes, err)
	}
	if _, err := NewReader(nil); !errors.Is(err, ErrHistoryDisabled) {
		t.Fatalf("disabled reader must refuse opening: %v", err)
	}
}
