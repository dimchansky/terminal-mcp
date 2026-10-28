package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRecordPreservesExactOriginalAcrossHistoryAndLive(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	// Persisted JSONL can preserve duplicate object keys and numbers that a
	// JavaScript JSON.parse round trip would not represent losslessly.
	history := []byte("{\"duplicate\":1,\"duplicate\":2,\"number\":9007199254740993}\n")
	if err := os.WriteFile(path+".1", history, 0600); err != nil {
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
	ctx := context.Background()
	archive, err := reader.openOne(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	archiveRef := RecordRef{Pos: positionForRecord(archive.key, 0, history), Hint: 1}
	if err := archive.close(); err != nil {
		t.Fatal(err)
	}
	raw, historical, err := reader.ReadRecord(ctx, archiveRef)
	if err != nil || !historical || !bytes.Equal(raw, history) {
		t.Fatalf("history record changed: historical=%t len=%d err=%v", historical, len(raw), err)
	}

	if err := writer.Start(NewEvent(Event{Tool: "terminal_start", Phase: "request_start",
		Args: map[string]any{"argv": []string{"go", "test", "-race", "./..."}}})); err != nil {
		t.Fatal(err)
	}
	live, err := reader.openObservedActive(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	line, _, err := readRecordForward(ctx, live.file, 0, live.size)
	if err != nil {
		t.Fatal(err)
	}
	ref := RecordRef{Pos: positionForRecord(live.key, 0, line), Hint: 0}
	if err := live.close(); err != nil {
		t.Fatal(err)
	}
	original, historical, err := reader.ReadRecord(ctx, ref)
	if err != nil || historical || !bytes.Equal(original, line) {
		t.Fatalf("confirmed live record not preserved: historical=%t len=%d err=%v", historical, len(original), err)
	}
	changed := ref
	changed.Pos.digest = sha256.Sum256([]byte("fabricated"))
	if _, _, err := reader.ReadRecord(ctx, changed); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("forged detail reference accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := reader.ReadRecord(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled record read mislabeled as cursor error: %v", err)
	}
}

func TestReadRecordNeverExposesPhysicallyPresentUnacknowledgedLine(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	current, err := reader.openObservedActive(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	frontier, key := current.size, current.key
	if err := current.close(); err != nil {
		t.Fatal(err)
	}
	rejected := []byte("{\"tool\":\"file_write\",\"content\":\"unacknowledged\"}\n")
	// Only a test fixture: a complete newline physically present after the
	// acknowledged frontier must not become visible to the reader.
	sink := writer.sink.(*fileSink)
	if _, err := sink.f.Write(rejected); err != nil {
		t.Fatal(err)
	}
	ref := RecordRef{Pos: positionForRecord(key, frontier, rejected), Hint: 0}
	if _, _, err := reader.ReadRecord(context.Background(), ref); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("detail API exposed unacknowledged bytes: %v", err)
	}
}

func TestReadRecordRejectsCrossRecordReference(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	first := []byte("{\"first\":1}\n")
	second := []byte("{\"second\":2}\n")
	data := append(bytes.Clone(first), second...)
	if err := os.WriteFile(path+".1", data, 0600); err != nil {
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
	segment, err := reader.openOne(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	key := segment.key
	if err := segment.close(); err != nil {
		t.Fatal(err)
	}
	// JSON permits whitespace before a value, but a single JSONL record
	// must begin exactly after the previous newline, not at that newline.
	start := int64(len(first) - 1)
	forged := data[start:]
	if !json.Valid(forged[:len(forged)-1]) {
		t.Fatal("test fixture must be a syntactically valid standalone JSON value")
	}
	ref := RecordRef{Pos: positionForRecord(key, start, forged), Hint: 1}
	if _, _, err := reader.ReadRecord(context.Background(), ref); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cross-line forged record accepted: %v", err)
	}
}
