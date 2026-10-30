package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDamagedCompleteHistoryHasByteExactDetailInBothDirections(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	rows := [][]byte{
		[]byte("\n"),
		[]byte("{\"valid\":1}\n"),
		[]byte("bad\x00json\n"),
		[]byte("{\"valid\":2}\n"),
	}
	if err := os.WriteFile(path+".2", bytes.Join(rows, nil), 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	for _, direction := range []PageDirection{Forward, Backward} {
		t.Run(map[PageDirection]string{Forward: "forward", Backward: "backward"}[direction], func(t *testing.T) {
			var cursor Cursor
			for i := range rows {
				page, err := reader.ReadPage(context.Background(), cursor, direction,
					ReadBudget{MaxRecords: 1, MaxBytes: 1, MaxScanBytes: 16})
				if err != nil || len(page.Records) != 1 || page.Next == "" || page.Next == cursor ||
					page.HasMore != (i < len(rows)-1) {
					t.Fatalf("page %d: %+v, %v", i, page, err)
				}
				row := page.Records[0]
				want := rows[i]
				if direction == Backward {
					want = rows[len(rows)-1-i]
				}
				bad := !json.Valid(want[:len(want)-1])
				if !row.Historical || row.Ref == (RecordRef{}) {
					t.Fatalf("row %d has no persistent reference or provenance: %+v", i, row)
				}
				if bad {
					if row.Problem == nil || !row.Problem.Complete ||
						!errors.Is(row.Problem, ErrCorruptRecord) ||
						row.Data != nil || row.Deferred {
						t.Fatalf("corruption silently disappeared: %+v", row)
					}
				} else if row.Problem != nil || !row.Deferred || row.Data != nil {
					t.Fatalf("valid record did not obey the page byte budget: %+v", row)
				}
				parsed, err := ParseRecordRef(row.Ref.String())
				if err != nil || parsed != row.Ref {
					t.Fatalf("ref round trip: %+v %v", parsed, err)
				}
				raw, historical, err := reader.ReadRecord(context.Background(), parsed)
				if err != nil || !historical || !bytes.Equal(raw, want) {
					t.Fatalf("row %d changed raw bytes: %q, historical=%v, error=%v", i, raw, historical, err)
				}
				cursor = page.Next
			}
		})
	}
}

func TestDamagedHistoryReferenceBoundariesAndValidation(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	maximum := append(bytes.Repeat([]byte{'x'}, int(maxFileBytes)-1), '\n')
	oversized := append(bytes.Repeat([]byte{'y'}, int(maxFileBytes)), '\n')
	last := []byte("{\"ok\":true}\n")
	if err := os.WriteFile(path+".1", bytes.Join([][]byte{maximum, oversized, last}, nil), 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	readOne := func(cursor Cursor) HistoryPage {
		t.Helper()
		page, err := reader.ReadPage(context.Background(), cursor, Forward,
			ReadBudget{MaxRecords: 1, MaxBytes: 1, MaxScanBytes: 32 << 20})
		if err != nil || len(page.Records) != 1 {
			t.Fatalf("cannot read damaged boundary: %+v %v", page, err)
		}
		return page
	}
	first := readOne("")
	ref := first.Records[0].Ref
	if ref == (RecordRef{}) || first.Records[0].Problem == nil ||
		!errors.Is(first.Records[0].Problem, ErrCorruptRecord) ||
		first.Records[0].Data != nil || first.Records[0].Deferred ||
		first.Records[0].Size != maxFileBytes {
		t.Fatalf("4 MiB malformed record must have detail reference: %+v", first)
	}
	raw, _, err := reader.ReadRecord(context.Background(), ref)
	if err != nil || !bytes.Equal(raw, maximum) {
		t.Fatalf("4 MiB raw source unavailable: %d %v", len(raw), err)
	}
	next := readOne(first.Next)
	if next.Records[0].Problem == nil || !errors.Is(next.Records[0].Problem, ErrRecordTooLarge) ||
		!next.Records[0].Problem.Complete ||
		next.Records[0].Ref != (RecordRef{}) || next.Records[0].Data != nil || next.Records[0].Deferred {
		t.Fatalf(">4 MiB damaged source must have explicit non-fetchable problem: %+v", next)
	}
	if _, _, err := reader.ReadRecord(context.Background(), next.Records[0].Ref); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("unbounded damaged read accepted: %v", err)
	}
	final := readOne(next.Next)
	if final.HasMore || final.Records[0].Problem != nil || final.Next == next.Next {
		t.Fatalf("oversized source broke forward cursor: %+v", final)
	}
	for _, forgery := range []RecordRef{
		func() RecordRef { r := ref; r.Pos.digest = sha256.Sum256([]byte("forged")); return r }(),
		func() RecordRef { r := ref; r.Pos.start++; return r }(),
		func() RecordRef { r := ref; r.Pos.start++; r.Pos.length--; return r }(),
		func() RecordRef { r := ref; r.Pos.length++; return r }(),
	} {
		if got, _, err := reader.ReadRecord(context.Background(), forgery); !errors.Is(err, ErrInvalidCursor) || got != nil {
			t.Fatalf("forged reference exposed damaged bytes: len=%d error=%v", len(got), err)
		}
	}
	f, err := os.OpenFile(path+".1", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteAt([]byte{'z'}, int64(len(maximum)-2))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(errors.Join(writeErr, closeErr))
	}
	if got, _, err := reader.ReadRecord(context.Background(), ref); !errors.Is(err, ErrInvalidCursor) || got != nil {
		t.Fatalf("changed persisted record passed digest proof: len=%d error=%v", len(got), err)
	}
}

func TestDamagedReferenceSurvivesRotationRestartAndReportsGap(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	malformed := []byte("malformed-startup\n")
	if err := os.WriteFile(path, malformed, 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 || page.Records[0].Problem == nil ||
		page.Records[0].Ref == (RecordRef{}) || !page.Records[0].Historical {
		t.Fatalf("startup damaged row not indexed: %+v %v", page, err)
	}
	ref := page.Records[0].Ref

	large := NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": ""}})
	empty, err := json.Marshal(large)
	if err != nil {
		t.Fatal(err)
	}
	padding := int(maxFileBytes-writer.Observe().Active.Size) - len(empty) - 2
	if padding <= 0 {
		t.Fatal("unexpected test fixture size")
	}
	large.Args = map[string]any{"content": strings.Repeat("x", padding)}
	if err := writer.Start(large); err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	if raw, historical, err := reader.ReadRecord(context.Background(), ref); err != nil ||
		!historical || !bytes.Equal(raw, malformed) {
		t.Fatalf("rotation invalidated damaged reference: %q %v %v", raw, historical, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	after, err := NewReader(restarted)
	if err != nil {
		t.Fatal(err)
	}
	if raw, historical, err := after.ReadRecord(context.Background(), ref); err != nil ||
		!historical || !bytes.Equal(raw, malformed) {
		t.Fatalf("restart invalidated damaged reference: %q %v %v", raw, historical, err)
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := after.ReadRecord(context.Background(), ref); !errors.Is(err, ErrHistoryGap) || raw != nil {
		t.Fatalf("deleted archived damaged record not reported as gap: %q %v", raw, err)
	}
}

func TestUnacknowledgedMalformedPhysicalRecordHasNoRawAccess(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := reader.openObservedActive(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	start, key := segment.size, segment.key
	if err := segment.close(); err != nil {
		t.Fatal(err)
	}
	physical := []byte("unacknowledged-invalid-json\n")
	if _, err := writer.sink.(*fileSink).f.Write(physical); err != nil {
		t.Fatal(err)
	}
	forged := RecordRef{Pos: positionForRecord(key, start, physical)}
	if got, _, err := reader.ReadRecord(context.Background(), forged); !errors.Is(err, ErrInvalidCursor) || got != nil {
		t.Fatalf("raw detail leaked beyond acknowledged frontier: %q %v", got, err)
	}
	page, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{})
	if err != nil || len(page.Records) != 1 || page.Records[0].Problem != nil {
		t.Fatalf("physical malformed data leaked into history: %+v %v", page, err)
	}
}

func TestMissingDamagedArchiveDoesNotAliasIdenticalArchive(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	malformed := []byte("\n")
	for _, suffix := range []string{".1", ".2"} {
		if err := os.WriteFile(path+suffix, malformed, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 || page.Records[0].Ref.Hint != 2 ||
		page.Records[0].Problem == nil {
		t.Fatalf("newer damaged archive not selected: %+v %v", page, err)
	}
	older, err := reader.ReadPage(context.Background(), page.Next, Backward, ReadBudget{MaxRecords: 1})
	if err != nil || len(older.Records) != 1 || older.Records[0].Ref.Hint != 1 ||
		older.Records[0].Ref.Pos != page.Records[0].Ref.Pos {
		t.Fatalf("fixture requires byte-identical first records: %+v %v", older, err)
	}
	forward, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{MaxRecords: 2})
	if err != nil || len(forward.Records) != 2 || forward.Records[1].Ref.Hint != 2 {
		t.Fatalf("forward fixture did not reach the newer archive: %+v %v", forward, err)
	}
	if err := os.Remove(path + ".2"); err != nil {
		t.Fatal(err)
	}
	if got, _, err := reader.ReadRecord(context.Background(), page.Records[0].Ref); !errors.Is(err, ErrHistoryGap) || got != nil {
		t.Fatalf("deleted hinted source was mistaken for identical older archive: %q %v", got, err)
	}
	for _, tc := range []struct {
		name   string
		cursor Cursor
		dir    PageDirection
	}{{"backward", page.Next, Backward}, {"forward", forward.Next, Forward}} {
		t.Run(tc.name, func(t *testing.T) {
			if result, err := reader.ReadPage(context.Background(), tc.cursor, tc.dir, ReadBudget{}); !errors.Is(err, ErrHistoryGap) {
				t.Fatalf("deleted source was silently substituted: %+v %v", result, err)
			}
		})
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	after, err := NewReader(restarted)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := after.ReadRecord(context.Background(), page.Records[0].Ref); !errors.Is(err, ErrHistoryGap) || got != nil {
		t.Fatalf("restart silently reused the older archive: %q %v", got, err)
	}
}

func TestMissingDamagedArchiveDoesNotAliasIdenticalActive(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	same := []byte("\n")
	for _, suffix := range []string{"", ".1", ".2"} {
		if err := os.WriteFile(path+suffix, same, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{MaxRecords: 2})
	if err != nil || len(page.Records) != 2 || page.Records[1].Ref.Hint != 2 {
		t.Fatalf("archive fixture: %+v %v", page, err)
	}
	if err := os.Remove(path + ".2"); err != nil {
		t.Fatal(err)
	}
	if got, _, err := reader.ReadRecord(context.Background(), page.Records[1].Ref); !errors.Is(err, ErrHistoryGap) || got != nil {
		t.Fatalf("deleted hinted archive was aliased to identical current active: %q %v", got, err)
	}
}
