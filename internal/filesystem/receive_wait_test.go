package filesystem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func ms(n int) *int { return &n }

func TestReceiveWaitAndReplayAreIndependentOfTransfer(t *testing.T) {
	files, dir, id := receiveFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ready := make(chan struct{})
	payload := bytes.Repeat([]byte{0, 255, 81}, 16<<10)
	var requests atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", "98304")
		_, _ = w.Write(payload)
		w.(http.Flusher).Flush()
		close(ready)
		<-release
		_, _ = w.Write(payload)
	}))
	defer source.Close()
	a := receiveInput(id, "wait-with-replay", filepath.Join(dir, "wait.bin"), source.URL+"/file")
	a.WaitMS = ms(0)
	start := time.Now()
	first, err := files.Receive(context.Background(), a)
	if err != nil || first.Status != "running" || first.Replayed == nil || *first.Replayed || time.Since(start) > time.Second {
		t.Fatalf("zero-wait submission failed: %+v %v", first, err)
	}
	<-ready

	// The same operation with a different wait_ms must replay rather than conflict.
	a.WaitMS = ms(120)
	start = time.Now()
	replayed, err := files.Receive(context.Background(), a)
	elapsed := time.Since(start)
	if err != nil || replayed.Status != "running" || replayed.Replayed == nil || !*replayed.Replayed ||
		elapsed < 85*time.Millisecond || elapsed > time.Second || requests.Load() != 1 ||
		replayed.BytesDownloaded == 0 {
		t.Fatalf("replay must wait but not restart: %+v %v elapsed=%s requests=%d", replayed, err, elapsed, requests.Load())
	}

	start = time.Now()
	poll, err := files.ReceiveResult(context.Background(), ReceiveResultArgs{ServerID: id, OperationID: a.OperationID, WaitMS: ms(0)})
	if err != nil || poll.Status != "running" || poll.Replayed != nil || time.Since(start) > time.Second {
		t.Fatalf("zero-wait result is not an immediate snapshot: %+v %v", poll, err)
	}

	// Result must wait for completion, not wake on download/progress updates.
	start = time.Now()
	poll, err = files.ReceiveResult(context.Background(), ReceiveResultArgs{ServerID: id, OperationID: a.OperationID, WaitMS: ms(160)})
	elapsed = time.Since(start)
	if err != nil || poll.Status != "running" || elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("bounded result polling returned prematurely: %+v %v elapsed=%s", poll, err, elapsed)
	}
	close(release)
	start = time.Now()
	done, err := files.ReceiveResult(context.Background(), ReceiveResultArgs{ServerID: id, OperationID: a.OperationID})
	if err != nil || done.Status != "completed" || done.Receipt == nil || done.Receipt.Bytes != len(payload)*2 ||
		done.Replayed != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("default long poll did not finish early: %+v %v", done, err)
	}
	final, err := files.Receive(context.Background(), a)
	if err != nil || final.Status != "completed" || final.Receipt == nil ||
		final.Receipt.Version != done.Receipt.Version || final.Replayed == nil || !*final.Replayed ||
		requests.Load() != 1 {
		t.Fatalf("completed retry lost same result: %+v %v", final, err)
	}
	got, err := os.ReadFile(a.Path)
	if err != nil || !bytes.Equal(got, append(append([]byte(nil), payload...), payload...)) {
		t.Fatalf("bad file: %v", err)
	}
}

func TestReceiveDefaultWaitExpiresButTransferContinues(t *testing.T) {
	files, dir, id := receiveFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	started := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		_, _ = w.Write([]byte("abc"))
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = w.Write([]byte("def"))
	}))
	defer source.Close()
	a := receiveInput(id, "default-wait", filepath.Join(dir, "default.bin"), source.URL+"/file")
	t0 := time.Now()
	ack, err := files.Receive(context.Background(), a)
	elapsed := time.Since(t0)
	if err != nil || ack.Status != "running" || ack.Replayed == nil || *ack.Replayed ||
		elapsed < 1700*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("default 2s wait did not expire correctly: %+v %v elapsed=%s", ack, err, elapsed)
	}
	<-started
	if ack.BytesDownloaded == 0 {
		t.Fatalf("default wait lost latest progress: %+v", ack)
	}
	close(release)
	end, err := files.ReceiveResult(context.Background(), ReceiveResultArgs{ServerID: id, OperationID: a.OperationID})
	if err != nil || end.Status != "completed" || end.Receipt == nil || end.Receipt.Bytes != 6 {
		t.Fatalf("download did not survive 2s observation timeout: %+v %v", end, err)
	}
}

func TestReceiveResultCancellationDoesNotStopDownload(t *testing.T) {
	files, dir, id := receiveFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ready := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		_, _ = w.Write([]byte("abc"))
		w.(http.Flusher).Flush()
		close(ready)
		<-release
		_, _ = w.Write([]byte("def"))
	}))
	defer source.Close()
	a := receiveInput(id, "cancel-wait", filepath.Join(dir, "cancel.bin"), source.URL+"/file")
	a.WaitMS = ms(0)
	if _, err := files.Receive(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	<-ready
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := files.ReceiveResult(ctx, ReceiveResultArgs{ServerID: id, OperationID: a.OperationID})
		finished <- err
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("long poll did not observe cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("long poll ignored client cancellation")
	}
	close(release)
	end := receiveStatusEventually(t, files, id, a.OperationID)
	if end.Status != "completed" {
		t.Fatalf("client cancellation aborted download: %+v", end)
	}
}

func TestReceiveWaitValidationDoesNotStartWork(t *testing.T) {
	files, dir, id := receiveFixture(t)
	for _, invalid := range []int{-1, 20001, 1 << 25} {
		a := receiveInput(id, fmt.Sprintf("invalid-wait-%d", invalid), filepath.Join(dir, "should-not-exist"), "http://127.0.0.1:1")
		a.WaitMS = ms(invalid)
		if _, err := files.Receive(context.Background(), a); err == nil {
			t.Fatalf("invalid receive wait_ms %d accepted", invalid)
		}
		if _, err := files.ReceiveResult(context.Background(), ReceiveResultArgs{ServerID: id, OperationID: "unknown", WaitMS: ms(invalid)}); err == nil {
			t.Fatalf("invalid receive result wait_ms %d accepted", invalid)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "should-not-exist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid wait_ms caused effect: %v", err)
	}
}
