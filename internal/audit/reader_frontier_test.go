package audit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// terminalForwardPageCursor checks the independent historical pager's last
// boundary, including the exact segment hint and raw predecessor proof.
func terminalForwardPageCursor(t *testing.T, reader *Reader) (Cursor, error) {
	t.Helper()
	var cursor Cursor
	for range 1024 {
		page, err := reader.ReadPage(context.Background(), cursor, Forward,
			ReadBudget{MaxRecords: 1, MaxBytes: 256, MaxScanBytes: 4 << 20})
		if err != nil {
			return "", err
		}
		if page.Next != "" {
			if page.HasMore && page.Next == cursor {
				t.Fatalf("forward paging did not progress: %q", cursor)
			}
			cursor = page.Next
		}
		if !page.HasMore {
			return cursor, nil
		}
	}
	t.Fatal("unexpectedly long journal in frontier fixture")
	return "", nil
}

func TestFrontierEqualsTerminalForwardCursorForQuiescentHistory(t *testing.T) {
	for _, name := range []string{
		"empty", "active", "only-legacy-with-empty-newer", "malformed-last-line", "closed-nonempty",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(privateAuditDir(t), "audit.jsonl")
			if name == "only-legacy-with-empty-newer" || name == "malformed-last-line" {
				older := []byte("{\"first\":true}\n")
				if err := os.WriteFile(path+".2", older, 0600); err != nil {
					t.Fatal(err)
				}
				newer := []byte("{\"newer\":true}\n")
				if name == "malformed-last-line" {
					newer = []byte("{\"newer\":true}\nnot-json\n")
				}
				if err := os.WriteFile(path+".6", newer, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".7", nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			writer, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Close() }()
			if name == "active" || name == "closed-nonempty" {
				if err := writer.Start(NewEvent(Event{Tool: "terminal_start", Phase: "request_start",
					RequestID: "before"})); err != nil {
					t.Fatal(err)
				}
				if err := writer.Start(NewEvent(Event{Tool: "file_edit", Phase: "request_start",
					RequestID: "later"})); err != nil {
					t.Fatal(err)
				}
			}
			reader, err := NewReader(writer)
			if err != nil {
				t.Fatal(err)
			}
			if name == "closed-nonempty" {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				ob := writer.Observe()
				if !ob.Closed || !ob.HasActive || ob.Active.Size == 0 {
					t.Fatalf("closed writer did not preserve confirmed active: %+v", ob)
				}
			}
			frontier, err := reader.FrontierCursor(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			paged, err := terminalForwardPageCursor(t, reader)
			if err != nil {
				t.Fatal(err)
			}
			if frontier != paged {
				t.Fatalf("frontier != exact terminal Forward cursor:\nfrontier %q\npage %q", frontier, paged)
			}
			if name == "empty" && frontier != "" {
				t.Fatal("empty history produced a fabricated cursor")
			}
			if name != "empty" && frontier == "" {
				t.Fatal("nonempty retained history has no frontier")
			}
			if frontier != "" {
				state, err := decodePageCursor(frontier)
				if err != nil || state.dir != Forward {
					t.Fatalf("frontier is not a valid forward cursor: %+v, %v", state, err)
				}
				if name == "active" || name == "closed-nonempty" {
					if state.hint != writer.Observe().NextArchive {
						t.Fatalf("active cursor hint is not its future archive suffix: %d", state.hint)
					}
				}
				if name == "only-legacy-with-empty-newer" || name == "malformed-last-line" {
					if state.hint != 6 {
						t.Fatalf("frontier ignored latest nonempty historical source: %d", state.hint)
					}
				}
			}
			edge, err := reader.ReadPage(context.Background(), frontier, Forward, ReadBudget{})
			if err != nil || len(edge.Records) != 0 || edge.Next != frontier {
				t.Fatalf("frontier does not resume at historical end: %+v %v", edge, err)
			}
		})
	}
}

func TestFrontierMatchesFrozenOldBoundaryAfterAppend(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "old"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "after-snapshot"})); err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.frontierCursorFromSnapshot(context.Background(), frozen)
	if err != nil {
		t.Fatal(err)
	}
	info, err := decodePageCursor(cursor)
	if err != nil || info.offset != frozen.observation.Active.Size ||
		info.hint != frozen.observation.NextArchive {
		t.Fatalf("cursor advanced beyond captured writer frontier: %+v %v", info, err)
	}
	page, err := reader.ReadPage(context.Background(), cursor, Forward, ReadBudget{MaxRecords: 2})
	if err != nil || len(page.Records) != 1 ||
		!bytes.Contains(page.Records[0].Data, []byte("after-snapshot")) {
		t.Fatalf("resume from frozen frontier did not return exactly later append: %+v %v", page, err)
	}
}

func TestFrontierFrozenActiveAfterGrowthAndRotation(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	// Leave enough space for a small append AFTER taking the snapshot;
	// the following large event rotates the enlarged A into .NextArchive.
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "original-A"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "additional-A"})); err != nil {
		t.Fatal(err)
	}
	large := NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": strings.Repeat("x", int(maxFileBytes)-750)}})
	if err := writer.Start(large); err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_start", Phase: "request_start",
		RequestID: "in-new-B",
		Args:      map[string]any{"command_note": strings.Repeat("y", 2048)}})); err != nil {
		t.Fatal(err)
	}
	// A is now renamed and its physical size is larger than the captured
	// frontier. Frontier must use the SAME inode but frozen shorter boundary.
	state := writer.Observe()
	if state.NextArchive <= frozen.observation.NextArchive {
		t.Fatalf("test must rotate A: snapshot=%+v current=%+v", frozen.observation, state)
	}
	archived, err := os.Stat(path + ".1")
	if err != nil || archived.Size() <= frozen.observation.Active.Size {
		t.Fatalf("renamed A did not physically grow after snapshot: stat=%v error=%v", archived, err)
	}
	cursor, err := reader.frontierCursorFromSnapshot(context.Background(), frozen)
	if err != nil {
		t.Fatal(err)
	}
	position, err := decodePageCursor(cursor)
	if err != nil || position.hint != frozen.observation.NextArchive ||
		position.offset != frozen.observation.Active.Size {
		t.Fatalf("renamed active gained post-snapshot bytes: %+v %v", position, err)
	}
	page, err := reader.ReadPage(context.Background(), cursor, Forward, ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 ||
		!bytes.Contains(page.Records[0].Data, []byte("additional-A")) {
		t.Fatalf("resuming after relocated active skipped appended data: %+v %v", page, err)
	}
	largePage, err := reader.ReadPage(context.Background(), page.Next, Forward, ReadBudget{MaxRecords: 1})
	if err != nil || len(largePage.Records) != 1 || !largePage.Records[0].Deferred {
		t.Fatalf("middle large event in A was missed: %+v %v", largePage, err)
	}
	newest, err := reader.ReadPage(context.Background(), largePage.Next, Forward, ReadBudget{MaxRecords: 1})
	if err != nil || len(newest.Records) != 1 ||
		!bytes.Contains(newest.Records[0].Data, []byte("in-new-B")) ||
		newest.Records[0].Ref.Hint <= frozen.observation.NextArchive {
		t.Fatalf("resume did not eventually reach post-snapshot B in order: %+v %v", newest, err)
	}
}

func TestFrontierOnlyWriterSealedWithNoActive(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	nearlyFillObservedLog(t, writer)
	sink := writer.sink.(*fileSink)
	sink.rename = func(before, after string) error {
		if err := os.Rename(before, after); err != nil {
			return err
		}
		return os.Mkdir(before, 0700) // old confirmed archive remains.
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err == nil {
		t.Fatal("test should fail to create a new active after actual rename")
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	ob := writer.Observe()
	if ob.HasActive || len(ob.Sealed) != 1 || ob.Sealed[0].Size == 0 {
		t.Fatalf("test did not produce sealed-only journal: %+v", ob)
	}
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodePageCursor(cursor)
	if err != nil || state.hint != ob.Sealed[0].Archive ||
		state.offset != ob.Sealed[0].Size {
		t.Fatalf("missing sealed-only frontier: %+v %v", state, err)
	}
	terminal, err := terminalForwardPageCursor(t, reader)
	if err != nil || terminal != cursor {
		t.Fatalf("frontier != terminal page of sealed-only history: %+v %v", terminal, err)
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.FrontierCursor(context.Background()); !errors.Is(err, ErrHistoryGap) ||
		errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("known sealed archive vanished without a clean gap category: %v", err)
	}
	// Recreate a different file at the same archive name. The current
	// observation still identifies the original sealed inode, not this file.
	if err := os.WriteFile(path+".1", []byte("{\"replacement\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.FrontierCursor(context.Background()); !errors.Is(err, ErrHistoryGap) ||
		errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("replaced sealed archive misreported as owner cursor error: %v", err)
	}
}

func TestFrontierRejectsIncompleteHistoricalTail(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	history := []byte("{\"ok\":1}\n{\"incomplete\":true")
	if err := os.WriteFile(path+".3", history, 0600); err != nil {
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
	_, err = reader.FrontierCursor(context.Background())
	var problem *RecordProblem
	if !errors.As(err, &problem) || !errors.Is(err, ErrCorruptRecord) ||
		problem.Complete || problem.End != int64(len(history)) {
		t.Fatalf("unclosed historical tail silently accepted: %+v", err)
	}
	// Page history can still return earlier good rows, but eventually
	// reaches the SAME incomplete tail and reports its corruption.
	first, err := reader.ReadPage(context.Background(), "", Forward, ReadBudget{})
	if err != nil || len(first.Records) != 1 || !first.HasMore {
		t.Fatalf("history before malformed tail not visible: %+v %v", first, err)
	}
	if _, err := reader.ReadPage(context.Background(), first.Next, Forward, ReadBudget{}); !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("paged reader silently passed the unclosed tail: %v", err)
	}
}

func TestFrontierEmptyThenLaterAppendAndRestart(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := reader.FrontierCursor(context.Background())
	if err != nil || empty != "" {
		t.Fatalf("empty journal had frontier: %q %v", empty, err)
	}
	if err := writer.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "first"})); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	newReader, err := NewReader(reopened)
	if err != nil {
		t.Fatal(err)
	}
	preAppend, err := newReader.FrontierCursor(context.Background())
	if err != nil || preAppend == "" {
		t.Fatalf("historic current active unavailable after restart: %q %v", preAppend, err)
	}
	if err := reopened.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "after-restart"})); err != nil {
		t.Fatal(err)
	}
	page, err := newReader.ReadPage(context.Background(), preAppend, Forward, ReadBudget{})
	if err != nil || len(page.Records) != 1 ||
		!bytes.Contains(page.Records[0].Data, []byte("after-restart")) {
		t.Fatalf("saved frontier not resumable after restart: %+v %v", page, err)
	}
}

func TestFrontierCancelAndDisabled(t *testing.T) {
	if _, err := (*Reader)(nil).FrontierCursor(context.Background()); !errors.Is(err, ErrHistoryDisabled) {
		t.Fatalf("nil reader not rejected: %v", err)
	}
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.FrontierCursor(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled frontier returned a false cursor: %v", err)
	}
}

// A cursor emitted BEFORE restart must still identify the same physical
// bytes after a new writer opens the file, appends, then rotates it.
func TestFrontierCursorSurvivesRestartThenRotation(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "before-restart"})); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(first)
	if err != nil {
		t.Fatal(err)
	}
	oldCursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	newReader, err := NewReader(second)
	if err != nil {
		t.Fatal(err)
	}
	same, err := newReader.FrontierCursor(context.Background())
	if err != nil || same != oldCursor {
		t.Fatalf("restart changed stable frontier: %q vs %q, %v", oldCursor, same, err)
	}
	if err := second.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "after-restart-before-rotation"})); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"content": strings.Repeat("a", int(maxFileBytes)-1200)}})); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start",
		RequestID: "force-rotation", Args: map[string]any{"content": strings.Repeat("b", 2048)}})); err != nil {
		t.Fatal(err)
	}
	if second.Observe().NextArchive < 2 {
		t.Fatal("fixture did not rotate the restarted writer")
	}
	page, err := newReader.ReadPage(context.Background(), oldCursor, Forward,
		ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 ||
		!bytes.Contains(page.Records[0].Data, []byte("after-restart-before-rotation")) {
		t.Fatalf("old frontier did not resume after restart + rotate: %+v %v", page, err)
	}
}

func TestFrontierCapturedSnapshotHonorsCancellation(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	for i := 1; i <= 36; i++ {
		name := path + "." + strconv.Itoa(i)
		if err := os.WriteFile(name, nil, 0600); err != nil {
			t.Fatal(err)
		}
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
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Cancel only after opening and closing several actual historical
	// segments, not at the entry check. checkedContext is a committed
	// test helper; this verifies cancellation within the historical loop.
	ctx := &checkedContext{Context: context.Background(), max: 6}
	if _, err := reader.frontierCursorFromSnapshot(ctx, snapshot); !errors.Is(err, context.Canceled) ||
		ctx.calls <= ctx.max {
		t.Fatalf("historical enumeration did not honor mid-loop cancellation: %v calls=%d", err, ctx.calls)
	}
	// The recent archived file must no longer have any reader descriptor
	// open; on Windows a reader lacking FILE_SHARE_DELETE would block this.
	if err := os.Rename(path+".36", path+".36.moved"); err != nil {
		t.Fatalf("canceled reader leaked a historical file handle: %v", err)
	}
}
