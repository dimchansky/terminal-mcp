package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func segmentPageFixture(t *testing.T, records [][]byte, baseline int64, hint uint64) *readerSegment {
	t.Helper()
	f, size := openLineFixture(t, bytes.Join(records, nil))
	if size == 0 {
		return &readerSegment{file: f, size: 0, baseline: 0, archive: hint}
	}
	key, err := segmentFingerprint(context.Background(), f, size)
	if err != nil {
		t.Fatal(err)
	}
	if baseline < 0 {
		baseline = size
	}
	return &readerSegment{
		file: f, size: size, baseline: baseline, key: key, archive: hint,
	}
}

func makePageRows(n int) [][]byte {
	lines := make([][]byte, n)
	for i := range lines {
		// Deliberately cross multiple 64 KiB windows, including records that
		// themselves cross a window boundary.
		value := strings.Repeat("x", (i%11)*320+(i%7)*1451)
		lines[i] = []byte(fmt.Sprintf("{\"request_id\":\"%03d\",\"content\":%q}\n", i, value))
	}
	return lines
}

func TestSegmentPageForwardBackwardExactBytesAndSourceKeys(t *testing.T) {
	const count = 327
	lines := makePageRows(count)
	var historicLimit int64
	for _, line := range lines[:120] {
		historicLimit += int64(len(line))
	}
	seg := segmentPageFixture(t, lines, historicLimit, 42)
	budget := ReadBudget{MaxRecords: 1000, MaxBytes: 8 << 20, MaxScanBytes: 32 << 20}
	for _, direction := range []PageDirection{Forward, Backward} {
		offset := int64(0)
		if direction == Backward {
			offset = seg.size
		}
		page, err := readSegmentPage(context.Background(), seg, direction, offset, budget)
		if err != nil {
			t.Fatalf("%v: %v", direction, err)
		}
		if len(page.Records) != len(lines) || page.HasMore || page.AtLiveEdge ||
			page.Changed != nil || page.ScannedBytes != seg.size {
			t.Fatalf("direction %v returned wrong bounds: count %d more=%t scanned %d",
				direction, len(page.Records), page.HasMore, page.ScannedBytes)
		}
		pos := int64(0)
		if direction == Backward {
			pos = seg.size
		}
		for i, row := range page.Records {
			index := i
			if direction == Backward {
				index = len(lines) - i - 1
			}
			if !bytes.Equal(row.Data, lines[index]) || row.Deferred || row.Problem != nil ||
				row.Size != int64(len(lines[index])) ||
				row.Ref.Hint != seg.archive || row.Ref.Pos.segment != seg.key {
				t.Fatalf("%v row %d lost bytes or identity: size=%d deferred=%t problem=%v",
					direction, i, row.Size, row.Deferred, row.Problem)
			}
			if direction == Forward {
				if row.Ref.Pos.start != pos {
					t.Fatalf("forward row %d start %d wanted %d", i, row.Ref.Pos.start, pos)
				}
				if row.Historical != (pos < historicLimit) {
					t.Fatalf("wrong historical classification row %d", i)
				}
				pos += row.Size
			} else {
				pos -= row.Size
				if row.Ref.Pos.start != pos || row.Historical != (pos < historicLimit) {
					t.Fatalf("backward row %d start or historical classification incorrect", i)
				}
			}
		}
		expectedEnd := int64(0)
		if direction == Forward {
			expectedEnd = seg.size
		}
		if pos != expectedEnd {
			t.Fatalf("%v ended at %d wanted %d", direction, pos, expectedEnd)
		}
		cursor, err := decodePageCursor(page.Next)
		if err != nil || cursor.offset != expectedEnd || cursor.hint != seg.archive ||
			cursor.dir != direction || cursor.segment != seg.key {
			t.Fatalf("cursor did not represent end of segment: %+v %v", cursor, err)
		}
		if err := verifyCursorBoundary(context.Background(), seg.file, seg.size, cursor); err != nil {
			t.Fatalf("unverifiable page cursor: %v", err)
		}
		again, err := readSegmentPage(context.Background(), seg, direction, expectedEnd, budget)
		if err != nil || len(again.Records) != 0 || again.HasMore || again.Next != page.Next {
			t.Fatalf("re-reading edge incorrectly restarted history: %+v %v", again, err)
		}
	}
}

func TestSegmentPagePaginationResumesWithoutLossInBothDirections(t *testing.T) {
	lines := makePageRows(208)
	seg := segmentPageFixture(t, lines, 0, 7)
	for _, direction := range []PageDirection{Forward, Backward} {
		offset := int64(0)
		if direction == Backward {
			offset = seg.size
		}
		seen := 0
		var prev Cursor
		for range len(lines) + 1 {
			page, err := readSegmentPage(context.Background(), seg, direction, offset,
				ReadBudget{MaxRecords: 17, MaxBytes: 8 << 20, MaxScanBytes: 32 << 20})
			if err != nil {
				t.Fatalf("page %d: %v", seen, err)
			}
			if page.Next == "" || (prev != "" && page.Next == prev) {
				t.Fatal("page made no cursor progress")
			}
			state, err := decodePageCursor(page.Next)
			if err != nil || state.hint != 7 || state.dir != direction {
				t.Fatalf("bad cursor/hint: %+v, %v", state, err)
			}
			if err := verifyCursorBoundary(context.Background(), seg.file, seg.size, state); err != nil {
				t.Fatalf("page position not authenticated: %v", err)
			}
			for _, row := range page.Records {
				index := seen
				if direction == Backward {
					index = len(lines) - 1 - seen
				}
				if !bytes.Equal(row.Data, lines[index]) {
					t.Fatalf("page row %d lost or duplicated data", seen)
				}
				seen++
			}
			if len(page.Records) > 17 {
				t.Fatal("MaxRecords ignored")
			}
			if !page.HasMore {
				break
			}
			prev = page.Next
			offset = state.offset
		}
		if seen != len(lines) {
			t.Fatalf("%v saw %d of %d records", direction, seen, len(lines))
		}
	}
}

func TestSegmentPageBudgetDefersButReadRecordRestoresCompleteOriginal(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	for i := range 5 {
		if err := writer.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start",
			RequestID: fmt.Sprint(i), Args: map[string]any{
				"content": "full sensitive input is preserved " + strings.Repeat("r", 800),
			}})); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	seg, err := reader.openFrozen(context.Background(), snapshot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seg.close() }()
	page, err := readSegmentPage(context.Background(), seg, Forward, 0,
		ReadBudget{MaxRecords: 5, MaxBytes: 64, MaxScanBytes: 256})
	if err != nil || len(page.Records) != 1 || page.ScannedBytes < 256 || !page.HasMore {
		t.Fatalf("soft scan budget should progress a complete row: %+v err %v", page, err)
	}
	for _, row := range page.Records {
		if row.Problem != nil || !row.Deferred || row.Data != nil {
			t.Fatalf("valid input should be deferred but never truncated: %+v", row)
		}
		data, historical, err := reader.ReadRecord(context.Background(), row.Ref)
		if err != nil || historical || int64(len(data)) != row.Size ||
			!bytes.Contains(data, []byte("full sensitive input is preserved")) {
			t.Fatalf("deferred full record not recoverable: %d %v, historical=%t", len(data), err, historical)
		}
	}
	again, err := readSegmentPage(context.Background(), seg, Forward,
		mustSegmentCursorOffset(t, page.Next),
		ReadBudget{MaxRecords: 5, MaxBytes: 64, MaxScanBytes: 256})
	if err != nil || len(again.Records) == 0 || again.Next == page.Next {
		t.Fatalf("deferred page failed to advance: %+v %v", again, err)
	}
}

func mustSegmentCursorOffset(t *testing.T, cursor Cursor) int64 {
	t.Helper()
	decoded, err := decodePageCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	return decoded.offset
}

func TestSegmentPageCorruptionAndIncompleteTailPreserveProgress(t *testing.T) {
	first := []byte("{\"ok\":1}\n")
	bad := []byte("not JSON\n")
	large := []byte("{\"payload\":\"" + strings.Repeat("x", int(maxFileBytes)) + "\"}\n")
	last := []byte("{\"ok\":2}\n")
	seg := segmentPageFixture(t, [][]byte{first, bad, large, last}, -1, 11)
	for _, direction := range []PageDirection{Forward, Backward} {
		offset := int64(0)
		if direction == Backward {
			offset = seg.size
		}
		page, err := readSegmentPage(context.Background(), seg, direction, offset,
			ReadBudget{MaxRecords: 10, MaxBytes: 8 << 20, MaxScanBytes: 32 << 20})
		if err != nil || len(page.Records) != 4 {
			t.Fatalf("corrupt/oversize row disappeared in %v: %d err=%v", direction, len(page.Records), err)
		}
		var invalid, oversized int
		for _, row := range page.Records {
			if row.Problem != nil {
				if row.Deferred || row.Data != nil || !row.Problem.Complete {
					t.Fatalf("damaged record silently converted to valid/deferred: %+v", row)
				}
				if errors.Is(row.Problem, ErrRecordTooLarge) {
					oversized++
				}
				if errors.Is(row.Problem, ErrCorruptRecord) {
					invalid++
				}
			}
		}
		if invalid != 1 || oversized != 1 {
			t.Fatalf("expected both kinds of damaged records: %d %d", invalid, oversized)
		}
	}
	incomplete := segmentPageFixture(t, [][]byte{first, []byte("{\"incomplete\":true")}, -1, 12)
	initial, err := readSegmentPage(context.Background(), incomplete, Forward, 0, ReadBudget{})
	if err != nil || len(initial.Records) != 1 || !initial.HasMore ||
		initial.Next == "" || !bytes.Equal(initial.Records[0].Data, first) {
		t.Fatalf("partial data before damaged tail disappeared: %+v %v", initial, err)
	}
	nextOffset := mustSegmentCursorOffset(t, initial.Next)
	if nextOffset != int64(len(first)) {
		t.Fatalf("partial row caused cursor to skip content: %d", nextOffset)
	}
	_, err = readSegmentPage(context.Background(), incomplete, Forward, nextOffset, ReadBudget{})
	if !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("later call must encounter the SAME damaged tail, got %v", err)
	}
}

func TestSegmentPageEmptyBoundaryCancellationAndByteTruth(t *testing.T) {
	empty := segmentPageFixture(t, nil, 0, 1)
	page, err := readSegmentPage(context.Background(), empty, Forward, 0, ReadBudget{})
	if err != nil || len(page.Records) != 0 || page.Next != "" || page.HasMore {
		t.Fatalf("empty segment should have no fabricated cursor: %+v %v", page, err)
	}
	if _, err := readSegmentPage(context.Background(), empty, Forward, 1, ReadBudget{}); !errors.Is(err, ErrRecordBoundary) {
		t.Fatalf("out-of-range empty offset accepted: %v", err)
	}
	rows := [][]byte{[]byte("{\"a\":1}\n"), []byte("{\"b\":2}\n")}
	seg := segmentPageFixture(t, rows, 0, 2)
	if _, err := readSegmentPage(context.Background(), seg, Forward, 2, ReadBudget{}); !errors.Is(err, ErrRecordBoundary) {
		t.Fatalf("interior-byte offset accepted: %v", err)
	}
	if _, err := readSegmentPage(context.Background(), seg, PageDirection(77), 0, ReadBudget{}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("invalid direction accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readSegmentPage(ctx, seg, Forward, 0, ReadBudget{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation reported as cursor damage: %v", err)
	}
	// A boundary after the first row is valid, not the interior of a row.
	if page, err := readSegmentPage(context.Background(), seg, Forward, int64(len(rows[0])), ReadBudget{}); err != nil ||
		len(page.Records) != 1 || !bytes.Equal(page.Records[0].Data, rows[1]) {
		t.Fatalf("forward boundary resume failed: %+v %v", page, err)
	}
}

func TestSegmentPageNeverReadsPastFrozenWriterFrontier(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start", RequestID: "acknowledged"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	seg, err := reader.openFrozen(context.Background(), snapshot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seg.close() }()
	// Physical bytes appear after snapshot, but committed limit stays frozen.
	physical := []byte("{\"tool\":\"file_write\",\"content\":\"unacknowledged\"}\n")
	sink := writer.sink.(*fileSink)
	if _, err := sink.f.Write(physical); err != nil {
		t.Fatal(err)
	}
	page, err := readSegmentPage(context.Background(), seg, Forward, 0, ReadBudget{})
	if err != nil || len(page.Records) != 1 || bytes.Contains(page.Records[0].Data, []byte("unacknowledged")) ||
		page.ScannedBytes != seg.size {
		t.Fatalf("read beyond frozen confirmed bytes: %+v %v", page, err)
	}
}

// An unverified historical tail without its final newline is a damaged raw
// journal fragment, not a fabricated "bad cursor". It is never skipped here:
// bounded historical-tail recovery is handled in later 4d stages.
func TestSegmentPageBackwardReportsIncompleteHistoricalTail(t *testing.T) {
	first := []byte("{\"ok\":true}\n")
	pending := []byte("{\"incomplete\":true")
	seg := segmentPageFixture(t, [][]byte{first, pending}, -1, 3)
	page, err := readSegmentPage(context.Background(), seg, Backward, seg.size, ReadBudget{})
	var problem *RecordProblem
	if !errors.As(err, &problem) || !errors.Is(err, ErrCorruptRecord) ||
		problem.Complete || problem.Start != int64(len(first)) || problem.End != seg.size ||
		len(page.Records) != 0 || page.Next != "" {
		t.Fatalf("partial historical tail was not reported honestly: %+v issue %+v", page, err)
	}
	// A writer-acknowledged range cannot have an incomplete final record.
	seg.baseline = 0
	if _, err := readSegmentPage(context.Background(), seg, Backward, seg.size, ReadBudget{}); !errors.Is(err, ErrRecordBoundary) {
		t.Fatalf("corrupt committed boundary was accepted as recoverable history: %v", err)
	}
}

// The predecessor at the cursor boundary may be over 16 MiB and lack any
// verifiable start. Preserve the newest row(s) when possible by rolling back
// the oldest valid row to serve as the next page's predecessor proof.
func TestSegmentPageBackwardPreservesValidRowsBeforeUnboundedPredecessor(t *testing.T) {
	unbounded := []byte(strings.Repeat("z", int(maxCorruptScan)+1) + "\n")
	older := []byte("{\"result\":\"older\"}\n")
	newer := []byte("{\"result\":\"newer\"}\n")
	seg := segmentPageFixture(t, [][]byte{unbounded, older, newer}, -1, 19)
	page, err := readSegmentPage(context.Background(), seg, Backward, seg.size,
		ReadBudget{MaxRecords: 2, MaxBytes: 8 << 20, MaxScanBytes: 32 << 20})
	if err != nil || len(page.Records) != 1 || !page.HasMore ||
		!bytes.Equal(page.Records[0].Data, newer) ||
		page.ScannedBytes != int64(len(newer)) {
		t.Fatalf("backward page lost all valid context on unprovable predecessor: %+v err %v", page, err)
	}
	position, err := decodePageCursor(page.Next)
	if err != nil || position.offset != int64(len(unbounded)+len(older)) {
		t.Fatalf("rolled-back cursor did not target the retained older row: %+v err %v", position, err)
	}
	if err := verifyCursorBoundary(context.Background(), seg.file, seg.size, position); err != nil {
		t.Fatalf("rolled-back cursor proof is not verifiable: %v", err)
	}
	if _, err := readSegmentPage(context.Background(), seg, Backward, position.offset,
		ReadBudget{MaxRecords: 1}); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("unbounded corrupted predecessor must remain an explicit barrier: %v", err)
	}
}

// A completed malformed/oversized newline-terminated row still creates a
// genuine cursor boundary. Page size 1 stresses the proof at EVERY boundary.
func TestSegmentPageOneRecordPagesAcrossDamagedRows(t *testing.T) {
	lines := [][]byte{
		[]byte("{\"ok\":1}\n"),
		[]byte("not JSON\n"),
		[]byte("{\"data\":\"" + strings.Repeat("z", int(maxFileBytes)) + "\"}\n"),
		[]byte("{\"ok\":2}\n"),
	}
	seg := segmentPageFixture(t, lines, -1, 31)
	if maxFileBytes >= maxCorruptScan {
		t.Fatal("writer's largest supported record must fit the bounded corruption/proof scan")
	}
	for _, direction := range []PageDirection{Forward, Backward} {
		offset := int64(0)
		if direction == Backward {
			offset = seg.size
		}
		for i := range lines {
			page, err := readSegmentPage(context.Background(), seg, direction, offset,
				ReadBudget{MaxRecords: 1, MaxBytes: 8 << 20, MaxScanBytes: 32 << 20})
			if err != nil || len(page.Records) != 1 {
				t.Fatalf("%v page %d: %+v err %v", direction, i, page, err)
			}
			state, err := decodePageCursor(page.Next)
			if err != nil || state.hint != 31 || state.dir != direction {
				t.Fatalf("invalid position at page %d: %+v err %v", i, state, err)
			}
			if err := verifyCursorBoundary(context.Background(), seg.file, seg.size, state); err != nil {
				t.Fatalf("unverifiable damaged-row cursor at page %d: %v", i, err)
			}
			index := i
			if direction == Backward {
				index = len(lines) - 1 - i
			}
			row := page.Records[0]
			if index == 1 || index == 2 {
				if row.Problem == nil || !row.Problem.Complete || row.Deferred {
					t.Fatalf("damaged row mislabeled at page %d: %+v", i, row)
				}
			} else if !bytes.Equal(row.Data, lines[index]) || row.Problem != nil {
				t.Fatalf("valid row missing after damaged predecessor: %+v", row)
			}
			offset = state.offset
			if page.HasMore != (i < len(lines)-1) {
				t.Fatalf("%v incorrect HasMore at page %d", direction, i)
			}
		}
	}
}

// Unbounded damage cannot be made into a valid cursor, but a page with
// several subsequent good rows must still return the newest row(s). The
// unrecoverable predecessor remains a visible explicit barrier.
// The same rollback must work if the removed row was marked Deferred.
func TestSegmentPageBackwardRollbackOnIncompleteScanWithDeferredRows(t *testing.T) {
	unbounded := []byte(strings.Repeat("z", int(maxCorruptScan)+1) + "\n")
	older := []byte("{\"order\":\"older\"}\n")
	newer := []byte("{\"order\":\"newer\"}\n")
	seg := segmentPageFixture(t, [][]byte{unbounded, older, newer}, -1, 45)
	page, err := readSegmentPage(context.Background(), seg, Backward, seg.size,
		ReadBudget{MaxRecords: 3, MaxBytes: 1, MaxScanBytes: 32 << 20})
	if err != nil || len(page.Records) != 1 || !page.HasMore ||
		!page.Records[0].Deferred || page.Records[0].Data != nil ||
		page.Records[0].Size != int64(len(newer)) {
		t.Fatalf("deferred row was lost or exposed after rollback: %+v %v", page, err)
	}
	cursor, err := decodePageCursor(page.Next)
	if err != nil || cursor.hint != seg.archive ||
		cursor.offset != int64(len(unbounded)+len(older)) {
		t.Fatalf("rollback cursor not at the last proven boundary: %+v %v", cursor, err)
	}
	if err := verifyCursorBoundary(context.Background(), seg.file, seg.size, cursor); err != nil {
		t.Fatalf("deferred row cursor proof invalid: %v", err)
	}
}

func TestSegmentPageCancellationAfterOneRowDoesNotExposeUnresumablePartialPage(t *testing.T) {
	lines := [][]byte{
		[]byte("{\"row\":1}\n"), []byte("{\"row\":2}\n"), []byte("{\"row\":3}\n"),
	}
	seg := segmentPageFixture(t, lines, 0, 16)
	// First row succeeds; cancel during the next read, after some data was
	// collected. No partial page without its proper cursor may escape.
	ctx := &checkedContext{Context: context.Background(), max: 4}
	page, err := readSegmentPage(ctx, seg, Forward, 0, ReadBudget{})
	if !errors.Is(err, context.Canceled) || len(page.Records) != 0 || page.Next != "" {
		t.Fatalf("canceled page leaked unresumable partial results: %+v %v", page, err)
	}
}
