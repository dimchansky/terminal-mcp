package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archivePagerFixture(t *testing.T) (*Log, *Reader, []byte, [][]byte) {
	t.Helper()
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	records := [][]byte{
		[]byte("{\"archive\":1,\"request_id\":\"older-A\"}\n"),
		[]byte("{\"archive\":1,\"request_id\":\"older-B\"}\n"),
		[]byte("{\"archive\":4,\"request_id\":\"middle-A\"}\n"),
		[]byte("{\"archive\":9,\"request_id\":\"newer-A\"}\n"),
	}
	for _, part := range []struct {
		suffix string
		from   int
		to     int
	}{{"1", 0, 2}, {"4", 2, 3}, {"9", 3, 4}} {
		var content []byte
		for _, line := range records[part.from:part.to] {
			content = append(content, line...)
		}
		if err := os.WriteFile(path+"."+part.suffix, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	newEvent := NewEvent(Event{Tool: "terminal_start", Phase: "request_start",
		RequestID: "active-A", Args: map[string]any{"argv": []string{"go", "test", "./..."}}})
	if err := writer.Start(newEvent); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records = append(records, live)
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	return writer, reader, live, records
}

func TestArchivePageSparseHistoryForwardBackwardAndExactBoundary(t *testing.T) {
	_, reader, _, rows := archivePagerFixture(t)
	for _, direction := range []PageDirection{Forward, Backward} {
		var current Cursor
		var seen [][]byte
		for n := 0; n < 2*len(rows)+2; n++ {
			page, err := reader.ReadPage(context.Background(), current, direction, ReadBudget{
				MaxRecords: 1, MaxBytes: 1 << 20, MaxScanBytes: 1 << 20,
			})
			if err != nil {
				t.Fatalf("%v page %d: %v", direction, n, err)
			}
			if len(page.Records) == 0 {
				if page.HasMore {
					t.Fatalf("%v empty page advertised progress without records: %+v", direction, page)
				}
				break
			}
			if len(page.Records) != 1 || page.Next == "" || page.Next == current {
				t.Fatalf("%v page %d lacks cursor progress: %+v", direction, n, page)
			}
			row := page.Records[0]
			if row.Deferred || row.Problem != nil {
				t.Fatalf("%v page %d lost original input: %+v", direction, n, row)
			}
			next, err := decodePageCursor(page.Next)
			if err != nil || next.dir != direction ||
				next.hint != row.Ref.Hint || next.segment != row.Ref.Pos.segment {
				t.Fatalf("cursor/hint mismatch: %+v, %v", next, err)
			}
			seen = append(seen, row.Data)
			current = page.Next
			if !page.HasMore {
				break
			}
		}
		if len(seen) != len(rows) {
			t.Fatalf("%v saw %d of %d rows", direction, len(seen), len(rows))
		}
		for i, row := range seen {
			want := rows[i]
			if direction == Backward {
				want = rows[len(rows)-1-i]
			}
			if !bytes.Equal(row, want) {
				t.Fatalf("%v row %d mismatch: %q vs %q", direction, i, row, want)
			}
		}
		edge, err := reader.ReadPage(context.Background(), current, direction, ReadBudget{})
		if err != nil || len(edge.Records) != 0 || edge.HasMore || edge.Next != current {
			t.Fatalf("%v edge restarted history: %+v %v", direction, edge, err)
		}
	}
}

func TestArchivePageGlobalDataBudgetDefersAcrossFiles(t *testing.T) {
	_, reader, _, rows := archivePagerFixture(t)
	bytesForFirst := int64(len(rows[0]) + len(rows[1]))
	page, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{
		MaxRecords: 10, MaxBytes: bytesForFirst, MaxScanBytes: 1 << 20,
	})
	if err != nil || len(page.Records) != len(rows) {
		t.Fatalf("page did not traverse all sources with a shared budget: %+v %v", page, err)
	}
	for i, row := range page.Records {
		if i < 2 {
			if row.Deferred || !bytes.Equal(row.Data, rows[i]) {
				t.Fatalf("record %d falsely deferred: %+v", i, row)
			}
		} else {
			if !row.Deferred || row.Data != nil {
				t.Fatalf("source %d exceeded global data cap: %+v", i, row)
			}
			full, historical, err := reader.ReadRecord(context.Background(), row.Ref)
			if err != nil || !bytes.Equal(full, rows[i]) || historical != (i < 4) {
				t.Fatalf("deferred raw source %d unrecoverable: historical=%t %v", i, historical, err)
			}
		}
	}
	if page.ScannedBytes != int64(len(bytes.Join(rows, nil))) {
		t.Fatalf("scanned bytes %d do not match total source bytes", page.ScannedBytes)
	}
}

func TestArchivePageSkipsManyEmptyHistoricalFiles(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	for i := 1; i <= 310; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "after-empty"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 || !bytes.Contains(page.Records[0].Data, []byte("after-empty")) {
		t.Fatalf("empty archives blocked real history: %+v %v", page, err)
	}
	if page.Records[0].Historical || page.HasMore {
		t.Fatalf("empty archives produced phantom records: %+v", page)
	}
}

func TestArchivePageHistoricalGapAndUnknownFutureFiles(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	content := []byte("{\"old\":true}\n")
	if err := os.WriteFile(path+".1", content, 0600); err != nil {
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
	page, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("first old page: %+v %v", page, err)
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadPage(context.Background(), page.Next, Forward, ReadBudget{}); !errors.Is(err, ErrHistoryGap) {
		t.Fatalf("a known missing archive silently disappeared: %v", err)
	}
	if _, _, err := reader.ReadRecord(context.Background(), page.Records[0].Ref); !errors.Is(err, ErrHistoryGap) {
		t.Fatalf("detail lookup did not report the same missing archive: %v", err)
	}

	// A file with a future suffix was created externally after snapshot
	// and must never be attributed to this writer's historical activity.
	future := []byte("{\"unexpected\":true}\n")
	if err := os.WriteFile(path+".3", future, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "real-active"})); err != nil {
		t.Fatal(err)
	}
	now, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{})
	if err != nil || len(now.Records) != 1 ||
		bytes.Contains(now.Records[0].Data, []byte("unexpected")) {
		t.Fatalf("post-start external archive contaminated snapshot: %+v %v", now, err)
	}
}

func TestArchivePageActiveHintWorksBeforeAndAfterManyRotations(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	marker := "opaque-secret-argument-" + strings.Repeat("V", 100)
	if err := writer.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": marker}})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 || page.Records[0].Ref.Hint != 1 {
		t.Fatalf("active record lacks stable eventual archive suffix: %+v %v", page, err)
	}
	ref := page.Records[0].Ref
	raw := bytes.Clone(page.Records[0].Data)
	if got, _, err := reader.ReadRecord(context.Background(), ref); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("eventual-suffix hint failed before rotation: %v", err)
	}
	// Each nearly-full record triggers another rotation. Twenty later
	// generations must not make the original RecordRef require an index scan.
	for range 20 {
		near := NewEvent(Event{Tool: "file_write", Phase: "request_start",
			Args: map[string]any{"content": strings.Repeat("X", int(maxFileBytes)-800)}})
		if err := writer.Start(near); err != nil {
			t.Fatal(err)
		}
	}
	if after := writer.Observe(); after.NextArchive < 20 {
		t.Fatalf("test did not actually rotate enough: %+v", after)
	}
	got, historical, err := reader.ReadRecord(context.Background(), ref)
	if err != nil || historical || !bytes.Equal(got, raw) {
		t.Fatalf("stable old active reference failed after many rotations: %v historical=%t", err, historical)
	}
	restored, err := reader.ReadPage(context.Background(), page.Next, Backward, ReadBudget{MaxRecords: 1})
	if err != nil || len(restored.Records) != 0 {
		t.Fatalf("cursor from oldest generation failed after many rotations: %+v %v", restored, err)
	}
	// Direction is part of a cursor's contract. Construct the forward
	// boundary after the source record, rather than reusing Backward.Next.
	forward := newPageCursor(Forward, ref.Pos.segment, ref.Hint,
		ref.Pos.start+int64(ref.Pos.length), ref.Pos.digest)
	continued, err := reader.ReadPage(context.Background(), forward, Forward,
		ReadBudget{MaxRecords: 1, MaxBytes: 64})
	// The first nearly-full append can still fit beside the initial small
	// record, so both may live in the original archive. The source hint must
	// remain stable and the cursor move forward; it need not equal 2 here.
	if err != nil || len(continued.Records) != 1 || !continued.Records[0].Deferred ||
		continued.Records[0].Ref.Hint < ref.Hint || continued.Next == forward {
		t.Fatalf("forward cursor from original active failed across many generations: %+v %v", continued, err)
	}
	// A restart transforms all in-process sealed archives into historical
	// files. The original eventual-suffix reference must still resolve.
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	r2, err := NewReader(restarted)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := r2.captureSnapshot()
	if err != nil || frozen.observation.NextArchive <= ref.Hint || len(frozen.observation.Sealed) != 0 {
		t.Fatalf("archives were not reclassified after writer restart: %+v %v", frozen, err)
	}
	persisted, old, err := r2.ReadRecord(context.Background(), ref)
	if err != nil || !old || !bytes.Equal(persisted, raw) {
		t.Fatalf("old record lost after restart: len=%d historic=%t err=%v", len(persisted), old, err)
	}
	pageAfterRestart, err := r2.ReadPage(context.Background(), forward, Forward,
		ReadBudget{MaxRecords: 1})
	if err != nil || len(pageAfterRestart.Records) != 1 ||
		!pageAfterRestart.Records[0].Historical {
		t.Fatalf("forward cursor lost across writer restart: %+v %v", pageAfterRestart, err)
	}
}

func TestArchivePageCanceledRequestDoesNotReturnUnresumablePartialRows(t *testing.T) {
	_, reader, _, _ := archivePagerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if page, err := reader.ReadPage(ctx, "", Forward, ReadBudget{}); !errors.Is(err, context.Canceled) ||
		len(page.Records) != 0 {
		t.Fatalf("canceled page leaked unresumable results: %+v %v", page, err)
	}
}

func TestArchivePageWriterOwnedSealedAndActiveOrder(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	nearlyFillObservedLog(t, writer)
	newer := NewEvent(Event{Tool: "terminal_start", Phase: "request_start",
		RequestID: "after-writer-rotation", Args: map[string]any{"argv": []string{"go", "test", "./..."}}})
	if err := writer.Start(newer); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	fwd, err := reader.ReadPage(context.Background(), "", Forward,
		ReadBudget{MaxRecords: 10, MaxBytes: 1024, MaxScanBytes: 16 << 20})
	if err != nil || len(fwd.Records) != 2 || !fwd.Records[0].Deferred ||
		fwd.Records[0].Historical || fwd.Records[1].Deferred || fwd.Records[1].Historical ||
		fwd.Records[0].Ref.Hint != 1 || fwd.Records[1].Ref.Hint != 2 {
		t.Fatalf("writer-sealed then current active order incorrect: %+v, %v", fwd, err)
	}
	rev, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{MaxRecords: 1})
	if err != nil || len(rev.Records) != 1 || rev.Records[0].Ref.Hint != 2 {
		t.Fatalf("reverse must begin at active (future suffix): %+v, %v", rev, err)
	}
	previous, err := reader.ReadPage(context.Background(), rev.Next, Backward, ReadBudget{MaxRecords: 1})
	if err != nil || len(previous.Records) != 1 ||
		previous.Records[0].Ref.Hint != 1 || previous.Records[0].Historical {
		t.Fatalf("reverse must continue into writer-sealed source: %+v, %v", previous, err)
	}
	full, historical, err := reader.ReadRecord(context.Background(), fwd.Records[0].Ref)
	if err != nil || historical || len(full) < int(maxFileBytes)-1024 {
		t.Fatalf("full original writer-sealed record not available: %d historic=%t %v", len(full), historical, err)
	}
}

func TestArchivePageBackwardSkipsEmptyCurrentActiveAndUsesLegacy(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	row := []byte("{\"historical\":\"visible\"}\n")
	if err := os.WriteFile(path+".2", row, 0600); err != nil {
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
	page, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{})
	if err != nil || len(page.Records) != 1 || !page.Records[0].Historical ||
		!bytes.Equal(page.Records[0].Data, row) || page.HasMore {
		t.Fatalf("backward history missed archive under empty active: %+v %v", page, err)
	}
}

func TestArchivePageSoftScanBudgetOvershootsAtMostOneRecordAcrossFiles(t *testing.T) {
	_, reader, _, rows := archivePagerFixture(t)
	firstBytes := int64(len(rows[0]))
	page, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{
		MaxRecords: 99, MaxBytes: firstBytes, MaxScanBytes: firstBytes + 1,
	})
	if err != nil || len(page.Records) != 2 || !page.HasMore ||
		page.ScannedBytes != firstBytes+int64(len(rows[1])) ||
		page.Records[0].Deferred || !page.Records[1].Deferred {
		t.Fatalf("global scan budget overshot more than one record: %+v, %v", page, err)
	}
	next, err := reader.ReadPage(context.Background(), page.Next, Forward,
		ReadBudget{MaxRecords: 1})
	if err != nil || len(next.Records) != 1 || !bytes.Equal(next.Records[0].Data, rows[2]) {
		t.Fatalf("scan budget page lost next archived source: %+v, %v", next, err)
	}
}

// A frozen active file can relocate to its expected numbered pathname while
// its snapshot is in use. This tests the same openArchiveSource path used
// by aggregate ReadPage without adding a production test hook.
func TestArchivePageFrozenActiveSourceExcludesPostCaptureRotation(t *testing.T) {
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
	snap, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	before := snap.observation.Active.Size
	// After snapshot, A rotates to .1 and a new B receives a different event.
	if err := writer.Start(NewEvent(Event{Tool: "terminal_start", Phase: "request_start",
		RequestID: "post-snapshot-do-not-admit"})); err != nil {
		t.Fatal(err)
	}
	old, err := reader.openArchiveSource(context.Background(), snap, archiveSource{active: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.close() }()
	if old.archive != snap.observation.NextArchive || old.size != before {
		t.Fatalf("historical source moved its frontier: %+v", old)
	}
	part, err := readSegmentPage(context.Background(), old, Backward, old.size, ReadBudget{})
	if err != nil || len(part.Records) != 1 ||
		bytes.Contains(part.Records[0].Data, []byte("post-snapshot-do-not-admit")) {
		t.Fatalf("post-snapshot bytes leaked through relocated active: %+v %v", part, err)
	}
}

// Empty archives have no record identity/cursor. A second arbitrary limit
// below maxDirectoryEntries would make valid newer history unreachable.
func TestArchivePageCrossesMoreThanFormerEmptySourceLimit(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	first := []byte("{\"source\":\"oldest\"}\n")
	if err := os.WriteFile(path+".1", first, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 4115; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "newest-after-4114-empty-archives"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{MaxRecords: 3})
	if err != nil || len(page.Records) != 2 || !bytes.Equal(page.Records[0].Data, first) ||
		!bytes.Contains(page.Records[1].Data, []byte("newest-after-4114-empty-archives")) ||
		page.HasMore {
		t.Fatalf("empty archive chain lost newer history: count=%d more=%t err=%v",
			len(page.Records), page.HasMore, err)
	}
}

// A claimed future pathname is never a license to replace an existing file.
// The writer refuses a suffix collision and the reader must not attribute
// a foreign archive to this process's frozen audit history.
func TestArchivePageRotationRejectsArchiveSuffixCollision(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	nearlyFillObservedLog(t, writer)
	future := []byte("{\"foreign\":\"not from writer\"}\n")
	if err := os.WriteFile(path+".1", future, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "should-not-overwrite"})); err == nil {
		t.Fatal("writer silently overwrote existing archive suffix")
	}
	saved, err := os.ReadFile(path + ".1")
	if err != nil || !bytes.Equal(saved, future) {
		t.Fatalf("foreign historical content was overwritten: %v", err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 ||
		bytes.Contains(page.Records[0].Data, []byte("foreign")) ||
		page.Records[0].Historical || page.Records[0].Ref.Hint != 1 {
		t.Fatalf("foreign suffix falsely entered committed observation: %+v, %v", page, err)
	}
}

// A complete damaged historical row must remain visible as a problem, but it
// cannot prevent byte-exact pagination from older records through the
// startup-historical prefix and on to newly acknowledged live input.
func TestArchivePageLegacyCorruptionContinuesIntoAcknowledgedActive(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	old := []byte("{\"phase\":\"start\"}\n")
	if err := os.WriteFile(path+".2", append(bytes.Clone(old), []byte("not-json\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	startup := []byte("{\"oldactive\":true}\n")
	if err := os.WriteFile(path, startup, 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start", RequestID: "new"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}

	var cursor Cursor
	var records []HistoryRecord
	for i := range 4 {
		page, err := reader.ReadPage(context.Background(), cursor, Forward, ReadBudget{MaxRecords: 1})
		if err != nil || len(page.Records) != 1 || page.Next == cursor || page.HasMore != (i < 3) {
			t.Fatalf("page %d must advance exactly once: %+v %v", i, page, err)
		}
		records = append(records, page.Records[0])
		cursor = page.Next
	}
	if frontier, err := reader.FrontierCursor(context.Background()); err != nil || frontier != cursor {
		t.Fatalf("legacy pagination ended at wrong frontier: %q, want %q, error %v", frontier, cursor, err)
	}
	if len(records) != 4 ||
		!records[0].Historical || !bytes.Equal(records[0].Data, old) ||
		!records[1].Historical || records[1].Problem == nil ||
		!records[1].Problem.Complete || !errors.Is(records[1].Problem, ErrCorruptRecord) ||
		records[1].Data != nil ||
		records[1].Problem.Start != int64(len(old)) ||
		records[1].Problem.End != int64(len(old)+len("not-json\n")) ||
		!records[2].Historical || !bytes.Equal(records[2].Data, startup) ||
		records[3].Historical || !bytes.Contains(records[3].Data, []byte("\"request_id\":\"new\"")) {
		t.Fatalf("old/corrupt/startup/live row sequence changed: %+v", records)
	}
}
