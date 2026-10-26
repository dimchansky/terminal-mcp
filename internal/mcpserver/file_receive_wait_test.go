//go:build linux || darwin

package mcpserver

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

// Test both MCP tool calls together, including bounded waiting and the
// independent lifetime of a transfer whose remote source has paused.
func TestFileReceiveResultMCPWaitAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host := session.New(nil)
	defer host.Close()
	root := t.TempDir()
	files, err := filesystem.New(host, root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()

	part := bytes.Repeat([]byte{0, 255, 3}, 2048)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	sent := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "12288")
		_, _ = w.Write(part)
		w.(http.Flusher).Flush()
		close(sent)
		<-release
		_, _ = w.Write(part)
	}))
	defer source.Close()

	ct, st := mcp.NewInMemoryTransports()
	ss, err := New(host, files).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "receive-wait-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	zero := 0
	a := filesystem.ReceiveArgs{
		ServerID: host.ID(), OperationID: "wait-over-mcp", Path: filepath.Join(root, "out.bin"),
		File:   filesystem.FileReference{DownloadURL: source.URL + "/download", FileID: "file-test"},
		WaitMS: &zero,
	}
	ack := call[filesystem.ReceiveResult](t, ctx, cs, "file_receive", a)
	if ack.Status != "running" || ack.Replayed == nil || *ack.Replayed || ack.Receipt != nil {
		t.Fatalf("invalid immediate admission response: %+v", ack)
	}
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("the remote source did not start")
	}
	wait := 140
	start := time.Now()
	progress := call[filesystem.ReceiveResult](t, ctx, cs, "file_receive_result",
		filesystem.ReceiveResultArgs{ServerID: host.ID(), OperationID: a.OperationID, WaitMS: &wait})
	elapsed := time.Since(start)
	if progress.Status != "running" || progress.Replayed != nil || progress.BytesDownloaded == 0 ||
		elapsed < 95*time.Millisecond || elapsed > time.Second {
		t.Fatalf("result wait did not respect observation timeout: %+v elapsed=%s", progress, elapsed)
	}
	close(release)
	start = time.Now()
	finished := call[filesystem.ReceiveResult](t, ctx, cs, "file_receive_result",
		filesystem.ReceiveResultArgs{ServerID: host.ID(), OperationID: a.OperationID})
	if finished.Status != "completed" || finished.Receipt == nil || finished.Receipt.Bytes != len(part)*2 ||
		finished.Replayed != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("default result wait failed: %+v", finished)
	}
	longer := 2500
	a.WaitMS = &longer
	again := call[filesystem.ReceiveResult](t, ctx, cs, "file_receive", a)
	if again.Status != "completed" || again.Receipt == nil || again.Replayed == nil || !*again.Replayed ||
		again.Receipt.Version != finished.Receipt.Version {
		t.Fatalf("replay with different wait_ms started a new operation: %+v", again)
	}
}
