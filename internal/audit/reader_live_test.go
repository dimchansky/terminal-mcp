package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func liveTestReader(t *testing.T) (*Log, *Reader, string) {
	t.Helper()
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
	return writer, reader, path
}

func liveEvent(id string) Event {
	return NewEvent(Event{Tool: "terminal_start", Phase: "request_start",
		RequestID: id, Args: map[string]any{
			"argv":         []string{"go", "test", "./..."},
			"env":          map[string]string{"OWNER_SECRET": "exact-input-" + id},
			"download_url": "http://localhost/signed?token=" + id,
		}})
}

func TestWaitNextPageReturnsExistingByteExactHistoryWithoutWaiting(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	if err := writer.Start(liveEvent("pending")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	page, err := reader.WaitNextPage(ctx, "", ReadBudget{})
	if err != nil || len(page.Records) != 1 || page.Records[0].Problem != nil ||
		page.Records[0].Deferred || !page.AtLiveEdge || page.Closed ||
		page.Changed == nil || page.HasMore {
		t.Fatalf("live history not immediately returned: %+v err=%v", page, err)
	}
	original, historical, err := reader.ReadRecord(ctx, page.Records[0].Ref)
	if err != nil || historical || !bytes.Equal(original, page.Records[0].Data) ||
		!bytes.Contains(original, []byte("exact-input-pending")) ||
		!bytes.Contains(original, []byte("http://localhost/signed?token=pending")) {
		t.Fatalf("live path lost original model input: historical=%v err=%v", historical, err)
	}
	ordinary, err := reader.ReadPage(ctx, "", Forward, ReadBudget{})
	if err != nil || ordinary.AtLiveEdge || ordinary.Changed != nil || ordinary.Closed {
		t.Fatalf("ordinary historical API should remain non-live: %+v err=%v", ordinary, err)
	}
}

func TestWaitNextPageNoLostWakeBeforeScanAndBeforeSelect(t *testing.T) {
	for _, mode := range []string{"before-scan", "between-scan-and-select"} {
		t.Run(mode, func(t *testing.T) {
			writer, reader, _ := liveTestReader(t)
			if err := writer.Start(liveEvent("old")); err != nil {
				t.Fatal(err)
			}
			after, err := reader.FrontierCursor(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := reader.captureSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			emit := func() {
				if err := writer.Start(liveEvent("arrived")); err != nil {
					t.Fatal(err)
				}
			}
			var beforeWait func()
			if mode == "before-scan" {
				emit()
			} else {
				beforeWait = emit // Runs synchronously after the frozen scan.
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			page, err := reader.waitNextPageFromSnapshot(ctx, snapshot, after, ReadBudget{}, beforeWait)
			if err != nil || len(page.Records) != 1 || !page.AtLiveEdge ||
				page.Next == after || !bytes.Contains(page.Records[0].Data, []byte("arrived")) {
				t.Fatalf("%s lost acknowledged wake: %+v err=%v", mode, page, err)
			}
			select {
			case <-snapshot.observation.Changed:
			default:
				t.Fatal("writer did not close the channel from frozen observation")
			}
		})
	}
}

func TestWaitNextPageBurstPaginatesWithoutDuplicates(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	if err := writer.Start(liveEvent("existing")); err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const n = 13
	for i := range n {
		if err := writer.Start(liveEvent(fmt.Sprintf("new-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for seen := 0; seen < n; {
		page, err := reader.WaitNextPage(ctx, cursor, ReadBudget{MaxRecords: 3, MaxBytes: 4 << 20})
		if err != nil || len(page.Records) == 0 || len(page.Records) > 3 || page.Next == cursor {
			t.Fatalf("burst stalled at %d: %+v err=%v", seen, page, err)
		}
		for _, row := range page.Records {
			want := fmt.Sprintf("new-%02d", seen)
			if !bytes.Contains(row.Data, []byte(want)) {
				t.Fatalf("burst record %d out of order: %q", seen, row.Data)
			}
			seen++
		}
		cursor = page.Next
		if seen == n && (!page.AtLiveEdge || page.Closed) {
			t.Fatalf("last burst page did not reach open live edge: %+v", page)
		}
	}
	sleepCtx, stop := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer stop()
	if _, err := reader.WaitNextPage(sleepCtx, cursor, ReadBudget{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("already seen burst was replayed or waiter did not block: %v", err)
	}
}

func TestWaitNextPageFailedCompleteAppendNeverWakes(t *testing.T) {
	writer, reader, path := liveTestReader(t)
	if err := writer.Start(liveEvent("before")); err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	s := writer.sink.(*fileSink)
	real := s.f
	failure := errors.New("failed after a complete physical line")
	s.f = &faultFile{auditFile: real,
		writeFn: func(data []byte) (int, error) {
			n, err := real.Write(data)
			if err != nil {
				return n, err
			}
			return n, failure
		},
		truncateFn: func(int64) error { return errors.New("injected repair failure") },
	}
	if err := writer.Start(liveEvent("denied")); !errors.Is(err, failure) {
		t.Fatalf("rejected append did not occur: %v", err)
	}
	select {
	case <-snapshot.observation.Changed:
		t.Fatal("unacknowledged write incorrectly woke live waiter")
	default:
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("exact-input-denied")) {
		t.Fatalf("test must leave complete rejected line physically present: %v", err)
	}
	// Both non-waiting directions must stop at the acknowledged boundary,
	// even though a complete, newline-terminated failed append exists on disk.
	backward, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{})
	if err != nil || len(backward.Records) != 1 || backward.HasMore ||
		backward.Records[0].Problem != nil ||
		!bytes.Contains(backward.Records[0].Data, []byte("exact-input-before")) ||
		bytes.Contains(backward.Records[0].Data, []byte("denied")) {
		t.Fatalf("backward history did not preserve the acknowledged line: %+v %v", backward, err)
	}
	forward, err := reader.ReadPage(context.Background(), cursor, Forward, ReadBudget{})
	if err != nil || len(forward.Records) != 0 || forward.HasMore || forward.Next != cursor {
		t.Fatalf("forward history crossed the unacknowledged frontier: %+v %v", forward, err)
	}
	timeoutCtx, stop := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer stop()
	if _, err := reader.WaitNextPage(timeoutCtx, cursor, ReadBudget{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed audit write was delivered or caused a wake: %v", err)
	}
	s.f = real
	if err := writer.Start(liveEvent("recovered")); err != nil {
		t.Fatal(err)
	}
	page, err := reader.WaitNextPage(context.Background(), cursor, ReadBudget{})
	if err != nil || len(page.Records) != 1 ||
		bytes.Contains(page.Records[0].Data, []byte("denied")) ||
		!bytes.Contains(page.Records[0].Data, []byte("recovered")) {
		t.Fatalf("live resume exposed rejected append: %+v err=%v", page, err)
	}
}

func TestWaitNextPageRotationAcrossLiveBoundary(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	nearlyFillObservedLog(t, writer)
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	initialSuffix := writer.Observe().NextArchive
	entered := make(chan struct{}, 1)
	type answer struct {
		page HistoryPage
		err  error
	}
	done := make(chan answer, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		snap, err := reader.captureSnapshot()
		if err != nil {
			done <- answer{err: err}
			return
		}
		page, err := reader.waitNextPageFromSnapshot(ctx, snap, cursor, ReadBudget{},
			func() {
				select {
				case entered <- struct{}{}:
				default:
				}
			})
		done <- answer{page: page, err: err}
	}()
	select {
	case <-entered: // Reader has completed scan and closed all read handles.
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := writer.Start(liveEvent("after-rotation")); err != nil {
		t.Fatalf("open live reader prevented journal rename or append: %v", err)
	}
	if writer.Observe().NextArchive <= initialSuffix {
		t.Fatal("test did not actually rotate the active journal")
	}
	var delivered Cursor
	select {
	case result := <-done:
		if result.err != nil || len(result.page.Records) != 1 ||
			!bytes.Contains(result.page.Records[0].Data, []byte("after-rotation")) ||
			result.page.Next == cursor {
			t.Fatalf("rotation lost live event: %+v error=%v", result.page, result.err)
		}
		delivered = result.page.Next
	case <-ctx.Done():
		t.Fatal("live waiter did not resume after writer rotation")
	}
	// Once acknowledged and consumed, the same rotated record must not replay.
	idle, stop := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer stop()
	if _, err := reader.WaitNextPage(idle, delivered, ReadBudget{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rotated event replayed or live wait failed to block: %v", err)
	}
}

func TestWaitNextPageCloseEndsWaitWithoutRearming(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	if err := writer.Start(liveEvent("old")); err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	type answer struct {
		page HistoryPage
		err  error
	}
	done := make(chan answer, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		snap, err := reader.captureSnapshot()
		if err != nil {
			done <- answer{err: err}
			return
		}
		page, err := reader.waitNextPageFromSnapshot(ctx, snap, cursor, ReadBudget{},
			func() {
				select {
				case entered <- struct{}{}:
				default:
				}
			})
		done <- answer{page, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal("Close is not idempotent:", err)
	}
	if err := writer.Start(liveEvent("after-close")); !errors.Is(err, ErrClosed) {
		t.Fatalf("writer admitted a record after Close: %v", err)
	}
	select {
	case result := <-done:
		if result.err != nil || !result.page.Closed || !result.page.AtLiveEdge ||
			result.page.Changed != nil || len(result.page.Records) != 0 || result.page.Next != cursor {
			t.Fatalf("Close did not wake terminal reader cleanly: %+v err=%v", result.page, result.err)
		}
	case <-ctx.Done():
		t.Fatal("terminal wake did not return")
	}
	second, err := reader.WaitNextPage(context.Background(), cursor, ReadBudget{})
	if err != nil || !second.Closed || second.Changed != nil || second.Next != cursor {
		t.Fatalf("repeated terminal read spun or lost cursor: %+v %v", second, err)
	}
	ob := writer.Observe()
	if !ob.Closed {
		t.Fatal("write after Close rearmed closed observation")
	}
	select {
	case <-ob.Changed:
	default:
		t.Fatal("terminal Changed was not closed")
	}
}

func TestWaitNextPageClosedBacklogIsDeliveredBeforeTerminalFlag(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	for _, id := range []string{"one", "two", "three"} {
		if err := writer.Start(liveEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	cursor := Cursor("")
	for i, want := range []string{"one", "two", "three"} {
		page, err := reader.WaitNextPage(context.Background(), cursor, ReadBudget{MaxRecords: 1})
		if err != nil || len(page.Records) != 1 ||
			!bytes.Contains(page.Records[0].Data, []byte("exact-input-"+want)) {
			t.Fatalf("closed backlog row %d missing: %+v error %v", i, page, err)
		}
		if i < 2 && (page.Closed || !page.HasMore || page.AtLiveEdge || page.Changed != nil) {
			t.Fatalf("writer close prematurely ended unread backlog: %+v", page)
		}
		if i == 2 && (!page.Closed || !page.AtLiveEdge || page.Changed != nil) {
			t.Fatalf("last closed backlog row did not mark terminal: %+v", page)
		}
		cursor = page.Next
	}
	finished, err := reader.WaitNextPage(context.Background(), cursor, ReadBudget{})
	if err != nil || !finished.Closed || len(finished.Records) != 0 || finished.Next != cursor {
		t.Fatalf("post-close repeat not terminal: %+v err=%v", finished, err)
	}
}

func TestWaitNextPageErrorsAndContextCancellation(t *testing.T) {
	if _, err := (*Reader)(nil).WaitNextPage(context.Background(), "", ReadBudget{}); !errors.Is(err, ErrHistoryDisabled) {
		t.Fatal("nil reader was not rejected:", err)
	}
	writer, reader, _ := liveTestReader(t)
	if err := writer.Start(liveEvent("old")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.WaitNextPage(context.Background(), Cursor("%%%bad"), ReadBudget{}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatal("invalid cursor was not rejected:", err)
	}
	backwards, err := reader.ReadPage(context.Background(), "", Backward, ReadBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.WaitNextPage(context.Background(), backwards.Next, ReadBudget{}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatal("backward cursor was accepted by forward live API:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.WaitNextPage(ctx, "", ReadBudget{}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled wait did not return promptly:", err)
	}
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer stop()
	if _, err := reader.WaitNextPage(deadline, cursor, ReadBudget{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("idle wait ignored deadline:", err)
	}
}

func TestWaitNextPageConcurrentReadersReceiveSameCommittedEvent(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	if err := writer.Start(liveEvent("old")); err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	entered := make(chan struct{}, n)
	type result struct {
		page HistoryPage
		err  error
	}
	results := make(chan result, n)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var waits [n]int
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := reader.captureSnapshot()
			if err != nil {
				results <- result{err: err}
				return
			}
			page, err := reader.waitNextPageFromSnapshot(ctx, snap, cursor, ReadBudget{},
				func() {
					waits[i]++
					if waits[i] == 1 {
						entered <- struct{}{} // Each waiter contributes one readiness signal.
					}
				})
			results <- result{page, err}
		}()
	}
	for range n {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("concurrent readers failed to reach wait point")
		}
	}
	if err := writer.Start(liveEvent("one-published-for-all")); err != nil {
		t.Fatal(err)
	}
	var expected Cursor
	for range n {
		select {
		case got := <-results:
			if got.err != nil || len(got.page.Records) != 1 ||
				!bytes.Contains(got.page.Records[0].Data, []byte("one-published-for-all")) {
				t.Fatalf("concurrent waiter lost input: %+v, %v", got.page, got.err)
			}
			if expected == "" {
				expected = got.page.Next
			} else if got.page.Next != expected {
				t.Fatalf("concurrent readers disagreed on exact committed cursor: %q vs %q", got.page.Next, expected)
			}
		case <-ctx.Done():
			t.Fatal("not all concurrent readers resumed")
		}
	}
	wg.Wait()
	for i, count := range waits {
		if count != 1 {
			t.Fatalf("waiter %d entered select %d times", i, count)
		}
	}
}

func TestWaitNextPageEmptyJournalThenFirstAppend(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reader.frontierCursorFromSnapshot(context.Background(), snapshot); err != nil || got != "" {
		t.Fatalf("empty journal frontier should be empty: %q %v", got, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	page, err := reader.waitNextPageFromSnapshot(ctx, snapshot, "", ReadBudget{}, func() {
		if err := writer.Start(liveEvent("first")); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil || len(page.Records) != 1 || !bytes.Contains(page.Records[0].Data, []byte("first")) {
		t.Fatalf("initial empty live handoff lost first event: %+v %v", page, err)
	}
}

// Rotation can physically seal the previously acknowledged segment even if
// the next active file cannot be opened. No fresh record was acknowledged,
// so this relocation must NOT wake the waiter. The following successful
// recovery append wakes it and it must resolve the old cursor across .1.
func TestWaitNextPageRotationRelocationWithoutWakeThenRecovery(t *testing.T) {
	writer, reader, path := liveTestReader(t)
	nearlyFillObservedLog(t, writer)
	oldCursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	type answer struct {
		page HistoryPage
		err  error
	}
	got := make(chan answer, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		page, err := reader.waitNextPageFromSnapshot(ctx, snapshot, oldCursor,
			ReadBudget{}, func() {
				select {
				case ready <- struct{}{}:
				default:
				}
			})
		got <- answer{page, err}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("reader failed to reach frozen live edge")
	}
	sink := writer.sink.(*fileSink)
	sink.rename = func(before, after string) error {
		if err := os.Rename(before, after); err != nil {
			return err
		}
		// Actual relocation succeeds; a directory now blocks new active open.
		return os.Mkdir(before, 0700)
	}
	if err := writer.Start(liveEvent("unacknowledged-after-rotation")); err == nil {
		t.Fatal("fault injection failed to block opening new active file")
	}
	after := reader.writer.Observe()
	if after.NextArchive <= snapshot.observation.NextArchive ||
		len(after.Sealed) == 0 || after.HasActive {
		t.Fatalf("writer did not publish relocated old frontier: %+v", after)
	}
	select {
	case <-snapshot.observation.Changed:
		t.Fatal("rotation-only failure emitted a false live append wake")
	default:
	}
	select {
	case result := <-got:
		t.Fatalf("no acknowledged new record, but waiter returned: %+v %v", result.page, result.err)
	default:
	}
	// A NEW live waiter starting while writer has no active handle should
	// reach the frozen edge and block, not misclassify a legal relocation as
	// an invalid cursor or return an empty page as a new event.
	idle, stop := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer stop()
	if _, err := reader.WaitNextPage(idle, oldCursor, ReadBudget{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fresh waiter in relocated/degraded state did not safely block: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	sink.rename = os.Rename
	if err := writer.Start(liveEvent("recovered-after-rotation")); err != nil {
		t.Fatal("writer could not recover from failed open:", err)
	}
	select {
	case result := <-got:
		if result.err != nil || len(result.page.Records) != 1 ||
			!bytes.Contains(result.page.Records[0].Data, []byte("recovered-after-rotation")) ||
			bytes.Contains(result.page.Records[0].Data, []byte("unacknowledged-after-rotation")) {
			t.Fatalf("rotation-only recovery dropped or fabricated entries: %+v %v", result.page, result.err)
		}
	case <-ctx.Done():
		t.Fatal("recovered audit did not wake frozen frontier consumer")
	}
}

// A DeadlineExceeded response does NOT prove we blocked: a busy loop over a
// closed channel also exits after the deadline. Count every entry to the
// select by injecting a per-wait callback. The idle writer gets exactly ONE.
func TestWaitNextPageIdleUsesOneBlockingWaitRatherThanBusySpin(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	if err := writer.Start(liveEvent("old")); err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Millisecond)
	defer cancel()
	var actualWaits int
	if _, err := reader.waitNextPageFromSnapshot(ctx, snapshot, cursor, ReadBudget{},
		func() { actualWaits++ }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle history wait should end on deadline: %v", err)
	}
	if actualWaits != 1 {
		t.Fatalf("idle waiter spun %d times instead of blocking on one select", actualWaits)
	}
}

// The writer has persisted an invalid (physically complete) line, but denied
// that append because its sink failed. This must NOT cause any additional
// select iterations; only a confirmed subsequent write may wake the waiter.
func TestWaitNextPageDeniedAppendDoesNotCauseBusySpin(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	if err := writer.Start(liveEvent("before")); err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	s := writer.sink.(*fileSink)
	real := s.f
	fail := errors.New("test: complete write but denied")
	s.f = &faultFile{auditFile: real, writeFn: func(data []byte) (int, error) {
		n, err := real.Write(data)
		if err != nil {
			return n, err
		}
		return n, fail
	}, truncateFn: func(int64) error { return errors.New("cannot truncate") }}
	if err := writer.Start(liveEvent("rejected")); !errors.Is(err, fail) {
		t.Fatalf("fault injection did not deny append: %v", err)
	}
	s.f = real
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Millisecond)
	defer cancel()
	var actualWaits int
	if _, err := reader.waitNextPageFromSnapshot(ctx, snapshot, cursor, ReadBudget{},
		func() { actualWaits++ }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed append caused false event or spinning: %v", err)
	}
	if actualWaits != 1 {
		t.Fatalf("rejected write caused %d idle loops", actualWaits)
	}
}

func TestWaitNextPageClosedBacklogHasNoWakeChannel(t *testing.T) {
	writer, reader, _ := liveTestReader(t)
	for _, id := range []string{"one", "two"} {
		if err := writer.Start(liveEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	page, err := reader.WaitNextPage(context.Background(), "", ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 || !page.HasMore || page.Closed || page.Changed != nil {
		t.Fatalf("closed writer returned a live wake channel while backlog remains: %+v %v", page, err)
	}
	complete, err := reader.WaitNextPage(context.Background(), page.Next, ReadBudget{})
	if err != nil || !complete.Closed || complete.Changed != nil {
		t.Fatalf("terminal backlog did not terminate cleanly: %+v %v", complete, err)
	}
}

// An archive-only frontier remains a valid live handoff when the current
// journal is initially empty. The next append must be delivered from the same
// frozen observation's Changed channel without polling or missing the wake.
func TestWaitNextPageArchiveOnlyFrontierToFirstActive(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	old := []byte("{\"request_id\":\"archived\"}\n")
	if err := os.WriteFile(path+".4", old, 0600); err != nil {
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
	cursor, err := reader.FrontierCursor(context.Background())
	if err != nil || cursor == "" {
		t.Fatalf("archive-only history must have a resumable frontier: %q %v", cursor, err)
	}
	history, err := reader.ReadPage(context.Background(), cursor, Forward, ReadBudget{})
	if err != nil || len(history.Records) != 0 || history.HasMore || history.Next != cursor ||
		history.AtLiveEdge || history.Changed != nil {
		t.Fatalf("non-waiting history must not return live metadata: %+v %v", history, err)
	}
	snapshot, err := reader.captureSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	called := false
	page, err := reader.waitNextPageFromSnapshot(ctx, snapshot, cursor, ReadBudget{}, func() {
		if called {
			t.Fatal("archive-only handoff waited more than once")
		}
		called = true
		if err := writer.Start(liveEvent("first-active")); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil || !called || len(page.Records) != 1 ||
		page.Records[0].Historical || page.HasMore || !page.AtLiveEdge ||
		page.Next == cursor || !bytes.Contains(page.Records[0].Data, []byte("exact-input-first-active")) {
		t.Fatalf("archive-only frontier lost first acknowledged active event: %+v %v", page, err)
	}
	if frontier, err := reader.FrontierCursor(context.Background()); err != nil || frontier != page.Next {
		t.Fatalf("live continuation ended at wrong frontier: %q, want %q, error %v", frontier, page.Next, err)
	}
}
