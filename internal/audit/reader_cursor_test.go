package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRecordPositionAndCursorSurviveRotation(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "audit.jsonl")
	rows := []byte("{\"phase\":\"request_start\",\"request_id\":\"alpha\"}\n{\"phase\":\"request_finish\",\"request_id\":\"alpha\"}\n")
	if err := os.WriteFile(path, rows, 0600); err != nil {
		t.Fatal(err)
	}
	original, err := openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	first, next, err := readRecordForward(context.Background(), original, 0, int64(len(rows)))
	if err != nil {
		t.Fatal(err)
	}
	segKey, err := segmentFingerprint(context.Background(), original, int64(len(rows)))
	if err != nil {
		t.Fatal(err)
	}
	pos := positionForRecord(segKey, 0, first)
	decodedPos, err := ParseRecordPos(pos.String())
	if err != nil || pos != decodedPos {
		t.Fatalf("position did not round-trip: %v", err)
	}
	book := newPageCursor(Forward, segKey, 0, next, cursorProof(first))
	p, err := decodePageCursor(book)
	if err != nil || p.hint != 0 || p.offset != next || p.dir != Forward {
		t.Fatalf("cursor did not round-trip: %+v, %v", p, err)
	}
	if err := verifyCursorBoundary(context.Background(), original, int64(len(rows)), p); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	_ = original.Close()
	archived, err := openAuditReader(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archived.Close() }()
	newKey, err := segmentFingerprint(context.Background(), archived, int64(len(rows)))
	if err != nil || newKey != segKey {
		t.Fatalf("rotation changed segment fingerprint: %v", err)
	}
	if err := verifyCursorBoundary(context.Background(), archived, int64(len(rows)), p); err != nil {
		t.Fatalf("resume after rotation failed: %v", err)
	}
}

func TestRecordCursorRejectsStaleAndMutatedBoundaries(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "history.jsonl")
	initial := []byte("{\"phase\":\"start\"}\n{\"phase\":\"finish\"}\n")
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := segmentFingerprint(context.Background(), f, int64(len(initial)))
	if err != nil {
		t.Fatal(err)
	}
	first, off, err := readRecordForward(context.Background(), f, 0, int64(len(initial)))
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodePageCursor(newPageCursor(Backward, key, 3, off, cursorProof(first)))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCursorBoundary(context.Background(), f, int64(len(initial)), cursor); err != nil {
		t.Fatal(err)
	}
	beyond := cursor
	beyond.offset = int64(len(initial) + 1)
	if err := verifyCursorBoundary(context.Background(), f, int64(len(initial)), beyond); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("out-of-frontier cursor accepted: %v", err)
	}
	_ = f.Close()
	changed := bytes.Replace(initial, []byte("start"), []byte("other"), 1)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	f, err = openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := verifyCursorBoundary(context.Background(), f, int64(len(changed)), cursor); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("changed previously acknowledged row undetected: %v", err)
	}
	newKey, err := segmentFingerprint(context.Background(), f, int64(len(changed)))
	if err != nil || newKey == key {
		t.Fatalf("file identity reused despite modified first row: %v", err)
	}
}

func TestParseRecordPosAndCursorRejectCorruption(t *testing.T) {
	key := sha256.Sum256([]byte("segment"))
	record := []byte("{\"ok\":true}\n")
	pos := positionForRecord(key, 4, record)
	p, err := ParseRecordPos(pos.String())
	if err != nil || p != pos {
		t.Fatalf("record position round-trip: %v", err)
	}
	for _, bad := range []string{"", "x", "@@@", pos.String()[:len(pos.String())-1]} {
		if _, err := ParseRecordPos(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("bad position %q accepted: %v", bad, err)
		}
	}
	if _, err := decodePageCursor(Cursor("junk")); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad cursor accepted: %v", err)
	}
	valid := newPageCursor(Forward, key, 42, 0, [32]byte{})
	info, err := decodePageCursor(valid)
	if err != nil || info.hint != 42 || info.offset != 0 {
		t.Fatalf("zero-boundary cursor invalid: %+v, %v", info, err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("cursor constructor accepted a predecessor for offset zero")
			}
		}()
		_ = newPageCursor(Forward, key, 42, 0, cursorProof(record))
	}()
}
