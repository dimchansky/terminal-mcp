package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// appendNearFullAuditEvent fills the *current* active segment to exactly one
// byte below its rotation threshold, without modifying the sink or bypassing
// the normal pre-effect audit path.
func appendNearFullAuditEvent(t *testing.T, writer *Log, id string) []byte {
	t.Helper()
	ev := NewEvent(Event{Tool: "file_write", Phase: "request_start",
		RequestID: id, Args: map[string]any{
			"content": "", "extra": "owner-input-" + id,
		}})
	empty, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := writer.Observe()
	if !snapshot.HasActive {
		t.Fatal("no active journal to fill")
	}
	padding := int(maxFileBytes-1-snapshot.Active.Size) - len(empty) - 1
	if padding <= 0 {
		t.Fatalf("active journal too full for padding event: %d", snapshot.Active.Size)
	}
	ev.Args = map[string]any{"content": strings.Repeat("x", padding), "extra": "owner-input-" + id}
	if err := writer.Start(ev); err != nil {
		t.Fatalf("append near-full event %s: %v", id, err)
	}
	if got := writer.Observe().Active.Size; got != maxFileBytes-1 {
		t.Fatalf("padding %s did not fill current segment: %d", id, got)
	}
	original, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return append(original, '\n')
}

func auditRecordRequestID(raw []byte) (string, error) {
	var record struct {
		RequestID string `json:"request_id"`
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return "", fmt.Errorf("record missing JSONL newline")
	}
	if err := json.Unmarshal(raw[:len(raw)-1], &record); err != nil {
		return "", err
	}
	if record.RequestID == "" {
		return "", fmt.Errorf("record has no request_id")
	}
	return record.RequestID, nil
}

func TestConcurrentHistoryReadersAcrossRotationsCloseAndRestart(t *testing.T) {
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

	const groups, smallPerGroup, viewers = 3, 5, 4
	expected := make([]string, 0, groups*(smallPerGroup+1))
	for group := range groups {
		expected = append(expected, fmt.Sprintf("pad-%d", group))
		for j := range smallPerGroup {
			expected = append(expected, fmt.Sprintf("small-%d-%d", group, j))
		}
	}

	written := make(map[string][]byte, len(expected))
	reached := make(chan struct{}, viewers*groups)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	type result struct {
		index int
		ids   []string
		err   error
	}
	results := make(chan result, viewers)
	ready := make(chan struct{}, viewers)
	for i := range viewers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ready <- struct{}{}
			var cursor Cursor
			var seen []string
			var sawInline, sawDeferred bool
			for pages := 0; pages < 128; pages++ {
				page, err := reader.WaitNextPage(ctx, cursor, ReadBudget{
					MaxRecords: 3, MaxBytes: 512, MaxScanBytes: 5 << 20,
				})
				if err != nil {
					results <- result{index, nil, fmt.Errorf("wait page %d: %w", pages, err)}
					return
				}
				if page.HasMore && len(page.Records) == 0 || len(page.Records) > 3 ||
					len(page.Records) > 0 && (page.Next == "" || page.Next == cursor) {
					results <- result{index, nil, fmt.Errorf("page without bounded cursor progress: %+v", page)}
					return
				}
				for _, record := range page.Records {
					if record.Problem != nil || record.Ref == (RecordRef{}) {
						results <- result{index, nil, fmt.Errorf("accepted event misclassified: %+v", record)}
						return
					}
					raw, historical, err := reader.ReadRecord(ctx, record.Ref)
					if err != nil || historical || record.Size != int64(len(raw)) {
						results <- result{index, nil, fmt.Errorf("detail: historical=%v size=%d raw=%d err=%v", historical, record.Size, len(raw), err)}
						return
					}
					if record.Deferred {
						sawDeferred = true
					} else {
						sawInline = true
					}
					if record.Deferred && record.Data != nil ||
						!record.Deferred && !bytes.Equal(record.Data, raw) {
						results <- result{index, nil, fmt.Errorf("byte budget changed original input: %+v", record)}
						return
					}
					id, err := auditRecordRequestID(raw)
					if err != nil {
						results <- result{index, nil, fmt.Errorf("decode exact original: %w", err)}
						return
					}
					if len(seen) >= len(expected) || id != expected[len(seen)] {
						results <- result{index, nil, fmt.Errorf("unexpected event at %d: %s", len(seen), id)}
						return
					}
					if strings.HasPrefix(id, "pad-") && !bytes.Contains(raw, []byte("owner-input-"+id)) ||
						strings.HasPrefix(id, "small-") && !bytes.Contains(raw, []byte("exact-input-"+id)) {
						results <- result{index, nil, fmt.Errorf("input from %s was lost or masked", id)}
						return
					}
					seen = append(seen, id)
					if strings.HasPrefix(id, "pad-") {
						reached <- struct{}{} // This reader consumed the pad before rotation.
					}
				}
				if page.Next != "" {
					cursor = page.Next
				}
				if page.Closed {
					if !sawInline || !sawDeferred || page.HasMore ||
						!page.AtLiveEdge || page.Changed != nil {
						results <- result{index, nil, fmt.Errorf("terminal/budget contract: inline=%t deferred=%t page=%+v", sawInline, sawDeferred, page)}
						return
					}
					results <- result{index, seen, nil}
					return
				}
			}
			results <- result{index, nil, fmt.Errorf("history did not reach close within 128 pages")}
		}(i)
	}
	for range viewers {
		<-ready
	}

	// Each group forces a real rotation, only AFTER every reader fetched
	// its near-full pad from the current active file.
	for group := range groups {
		id := fmt.Sprintf("pad-%d", group)
		written[id] = appendNearFullAuditEvent(t, writer, id)
		for range viewers {
			select {
			case <-reached:
			case failed := <-results:
				t.Fatalf("viewer %d stopped before rotation: %v", failed.index, failed.err)
			case <-ctx.Done():
				t.Fatalf("reader barrier before rotation: %v", ctx.Err())
			}
		}
		for j := range smallPerGroup {
			id := fmt.Sprintf("small-%d-%d", group, j)
			event := liveEvent(id)
			original, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			written[id] = append(original, '\n')
			if err := writer.Start(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	if observed := writer.Observe(); observed.NextArchive != groups+1 ||
		len(observed.Sealed) != groups {
		t.Fatalf("stress fixture did not rotate three times: %+v", observed)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for range viewers {
		select {
		case got := <-results:
			if got.err != nil || !reflect.DeepEqual(got.ids, expected) {
				t.Fatalf("viewer %d missed/duplicated records: %v\nseen=%v\nwant=%v", got.index, got.err, got.ids, expected)
			}
		case <-ctx.Done():
			t.Fatalf("concurrent history viewers did not reach terminal Close: %v", ctx.Err())
		}
	}

	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	replay, err := NewReader(restarted)
	if err != nil {
		t.Fatal(err)
	}
	var cursor Cursor
	var ids []string
	var replayInline, replayDeferred bool
	for pages := 0; pages < 32; pages++ {
		page, err := replay.ReadPage(ctx, cursor, Forward, ReadBudget{
			MaxRecords: 3, MaxBytes: 512, MaxScanBytes: 5 << 20,
		})
		if err != nil || len(page.Records) == 0 || page.Next == cursor {
			t.Fatalf("restart page %d did not progress: %+v %v", pages, page, err)
		}
		for _, record := range page.Records {
			raw, historical, err := replay.ReadRecord(ctx, record.Ref)
			if err != nil || !historical || len(raw) != int(record.Size) {
				t.Fatalf("restart detail changed provenance or bytes: %v %v", historical, err)
			}
			id, err := auditRecordRequestID(raw)
			if err != nil {
				t.Fatal(err)
			}
			if want, found := written[id]; !found || !bytes.Equal(raw, want) {
				t.Fatalf("persisted original differs from submitted event %q", id)
			}
			if record.Deferred {
				replayDeferred = true
				if record.Data != nil {
					t.Fatalf("deferred event exposed inline data: %q", id)
				}
			} else {
				replayInline = true
				if !bytes.Equal(record.Data, raw) {
					t.Fatalf("inline event changed raw bytes: %q", id)
				}
			}
			ids = append(ids, id)
		}
		cursor = page.Next
		if !page.HasMore {
			break
		}
	}
	if !reflect.DeepEqual(ids, expected) || !replayInline || !replayDeferred {
		t.Fatalf("restart history changed: seen=%v, want=%v, inline=%t deferred=%t",
			ids, expected, replayInline, replayDeferred)
	}
}

// This runs on all platforms, and its name selects it for the dedicated
// native Windows reader-delete-sharing gate in CI.
func TestWindowsAuditReaderHighLevelTrafficDuringRename(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	appendNearFullAuditEvent(t, writer, "pad-rename")
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	before, err := reader.ReadPage(context.Background(), "", Backward,
		ReadBudget{MaxRecords: 1, MaxBytes: 1})
	if err != nil || len(before.Records) != 1 || before.Records[0].Ref == (RecordRef{}) {
		t.Fatalf("cannot establish detail reference before rename: %+v %v", before, err)
	}
	ref := before.Records[0].Ref
	held, err := reader.openObservedActive(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.close() }()

	// Park a real live follower at the acknowledged edge before starting
	// rotation. Its original Changed channel must not fire for a rename
	// alone: only the successful new append may wake it.
	frontier, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, stopWait := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopWait()
	waitEntered := make(chan struct{})
	type waiterResult struct {
		page HistoryPage
		err  error
	}
	waitDone := make(chan waiterResult, 1)
	go func() {
		page, err := reader.waitNextPageFromSnapshot(waitCtx, snapshot, frontier,
			ReadBudget{MaxRecords: 1, MaxBytes: 512}, func() {
				select {
				case <-waitEntered:
				default:
					close(waitEntered)
				}
			})
		waitDone <- waiterResult{page: page, err: err}
	}()
	select {
	case <-waitEntered:
	case <-waitCtx.Done():
		t.Fatalf("live follower did not reach acknowledged edge: %v", waitCtx.Err())
	}

	renamed := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	writer.sink.(*fileSink).rename = func(old, new string) error {
		if err := os.Rename(old, new); err != nil {
			return err
		}
		close(renamed)
		<-release // Real rename done; writer has not yet published relocation.
		return nil
	}
	go func() {
		finished <- writer.Start(liveEvent("after-rename"))
	}()
	joined := false
	releaseRename := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	defer func() {
		releaseRename()
		if !joined {
			select {
			case err := <-finished:
				if err != nil {
					t.Errorf("writer could not finish after aborted test: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("writer could not finish after release")
			}
		}
	}()
	select {
	case <-renamed:
	case err := <-finished:
		joined = true
		t.Fatalf("real rename failed before publication: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("writer could not rename while a high-level reader was held open")
	}

	// The writer is suspended between actual os.Rename and its publication.
	// All high-level reads must complete without taking the writer mutex.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	answer := make(chan error, 1)
	go func() {
		page, err := reader.ReadPage(ctx, "", Backward, ReadBudget{MaxRecords: 1, MaxBytes: 1})
		if err != nil || len(page.Records) != 1 || page.Records[0].Ref.Pos != ref.Pos {
			answer <- fmt.Errorf("page during rename: %+v %v", page, err)
			return
		}
		raw, historical, err := reader.ReadRecord(ctx, ref)
		if err != nil || historical || len(raw) != int(ref.Pos.length) ||
			!bytes.Contains(raw, []byte("owner-input-pad-rename")) {
			answer <- fmt.Errorf("detail during rename: len=%d historical=%v err=%v", len(raw), historical, err)
			return
		}
		if _, err := reader.FrontierCursor(ctx); err != nil {
			answer <- fmt.Errorf("frontier during rename: %w", err)
			return
		}
		live, err := reader.WaitNextPage(ctx, "", ReadBudget{MaxRecords: 1, MaxBytes: 1})
		if err != nil || len(live.Records) != 1 || live.Records[0].Ref.Pos != ref.Pos {
			answer <- fmt.Errorf("live read with backlog during rename: %+v %v", live, err)
			return
		}
		answer <- nil
	}()
	select {
	case err := <-answer:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("high-level reader was blocked by writer rename: %v", ctx.Err())
	}
	select {
	case premature := <-waitDone:
		t.Fatalf("rename without append prematurely woke live follower: %+v", premature)
	default:
	}

	releaseRename()
	select {
	case err := <-finished:
		joined = true
		if err != nil {
			t.Fatalf("held reader prevented rotation/admission: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("writer failed to publish after readers completed")
	}
	select {
	case got := <-waitDone:
		if got.err != nil || len(got.page.Records) != 1 ||
			got.page.Records[0].Historical || got.page.Closed || !got.page.AtLiveEdge {
			t.Fatalf("published append not delivered after rename: %+v error=%v", got.page, got.err)
		}
		id, err := auditRecordRequestID(got.page.Records[0].Data)
		if err != nil || id != "after-rename" {
			t.Fatalf("published live record was lost: id=%q err=%v", id, err)
		}
	case <-waitCtx.Done():
		t.Fatalf("published append did not wake live follower: %v", waitCtx.Err())
	}
	// The original pre-rotation ref must still read the same acknowledged
	// bytes and provenance after publication, now from sealed archive .1.
	sealed, err := reader.ReadPage(ctx, "", Forward, ReadBudget{MaxRecords: 1, MaxBytes: 1})
	if err != nil || len(sealed.Records) != 1 || sealed.Records[0].Ref.Pos != ref.Pos ||
		sealed.Records[0].Historical || !sealed.HasMore {
		t.Fatalf("sealed archive not visible after publication: %+v %v", sealed, err)
	}
	oldRaw, historical, err := reader.ReadRecord(ctx, ref)
	if err != nil || historical || len(oldRaw) != int(ref.Pos.length) ||
		!bytes.Contains(oldRaw, []byte("owner-input-pad-rename")) {
		t.Fatalf("detail lost acknowledged bytes after rename: size=%d historical=%v err=%v",
			len(oldRaw), historical, err)
	}
	var heldByte [1]byte
	if _, err := held.file.ReadAt(heldByte[:], 0); err != nil || heldByte[0] != '{' {
		t.Fatalf("held pre-rename reader lost the archived descriptor: %q %v", heldByte, err)
	}
	if observed := writer.Observe(); observed.NextArchive != 2 ||
		len(observed.Sealed) != 1 || writer.Status().State != "ok" {
		t.Fatalf("reader traffic damaged writer health or rotation: %+v, %+v", observed, writer.Status())
	}
}
