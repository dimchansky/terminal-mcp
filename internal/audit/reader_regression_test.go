package audit

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordRefHintIsReversibleWithoutChangingRecordIdentity(t *testing.T) {
	key := sha256.Sum256([]byte("segment-identity"))
	pos := positionForRecord(key, 51, []byte("{\"tool\":\"terminal_start\"}\n"))
	ref := RecordRef{Pos: pos, Hint: 178}
	recovered, err := ParseRecordRef(ref.String())
	if err != nil || recovered != ref {
		t.Fatalf("record reference round-trip: %+v, %v", recovered, err)
	}
	if shifted := (RecordRef{Pos: pos, Hint: 179}); shifted.Pos != recovered.Pos {
		t.Fatal("archive name hint unexpectedly changed logical row identity")
	}
	if _, err := ParseRecordRef("invalid"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("invalid reference unexpectedly accepted: %v", err)
	}
}

func TestCorruptUnrelatedCandidateCannotBlockValidCursor(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	// Create preexisting historical archives BEFORE Open. Archives invented
	// after the writer starts are not part of its acknowledged snapshot.
	if err := os.WriteFile(path+".1", []byte("not-json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record := []byte("{\"value\":\"found\"}\n")
	if err := os.WriteFile(path+".2", record, 0600); err != nil {
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
	f, err := openAuditReader(path + ".2")
	if err != nil {
		t.Fatal(err)
	}
	key, err := segmentFingerprint(context.Background(), f, int64(len(record)))
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodePageCursor(newPageCursor(Forward, key, 1, int64(len(record)), cursorProof(record)))
	if err != nil {
		t.Fatal(err)
	}
	seg, err := reader.resolveCursor(context.Background(), cursor)
	if err != nil {
		t.Fatalf("unrelated bad archive blocked valid cursor: %v", err)
	}
	if seg.archive != 2 {
		t.Fatalf("expected valid archive .2, got %d", seg.archive)
	}
	_ = seg.close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.resolveCursor(ctx, cursor); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was mislabeled as invalid cursor: %v", err)
	}
}

func TestCursorValidationPreservesContextCancellation(t *testing.T) {
	data := []byte("{\"record\":true}\n")
	f, size := openLineFixture(t, data)
	key, err := segmentFingerprint(context.Background(), f, size)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodePageCursor(newPageCursor(Forward, key, 0, size, cursorProof(data)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifyCursorBoundary(ctx, f, size, cursor); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan incorrectly invalidated cursor: %v", err)
	}
}

func TestSegmentFingerprintAllowsMalformedHistoricalFirstLine(t *testing.T) {
	data := []byte("unparseable line\n{\"tool\":\"file_write\"}\n")
	f, size := openLineFixture(t, data)
	key, err := segmentFingerprint(context.Background(), f, size)
	if err != nil || key == ([32]byte{}) {
		t.Fatalf("malformed first-line identity unexpectedly refused: %v", err)
	}
	key2, err := segmentFingerprint(context.Background(), f, size)
	if err != nil || key != key2 {
		t.Fatal("segment identity unstable across identical raw bytes")
	}
}

func TestReaderMatchesRenamedHandleBeforeWriterPublishes(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	nearlyFillObservedLog(t, writer)
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	before := writer.Observe()
	sealedName := make(chan struct{})
	releaseRename := make(chan struct{})
	defer func() {
		select {
		case <-releaseRename:
		default:
			close(releaseRename)
		}
	}()
	writer.sink.(*fileSink).rename = func(old, new string) error {
		if err := os.Rename(old, new); err != nil {
			return err
		}
		close(sealedName)
		<-releaseRename // Writer has NOT published its new Observation yet.
		return nil
	}
	finished := make(chan error, 1)
	go func() {
		finished <- writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"}))
	}()
	select {
	case <-sealedName:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach the rename-before-publication window")
	}
	if got := writer.Observe(); got.HasActive == false || len(got.Sealed) != 0 {
		t.Fatalf("test no longer exercised stale writer publication: %+v", got)
	}
	opened, err := reader.openOne(context.Background(), 1)
	if err != nil {
		t.Fatalf("reader failed to match renamed descriptor to old published active: %v", err)
	}
	if opened.size != before.Active.Size || opened.baseline != before.Active.Baseline {
		t.Fatalf("renamed segment was incorrectly treated as arbitrary historical bytes: %+v", opened)
	}
	_ = opened.close()
	close(releaseRename)
	if err := <-finished; err != nil {
		t.Fatalf("writer could not complete after concurrent read: %v", err)
	}
}
