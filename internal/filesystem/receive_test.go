package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"terminalmcp/internal/session"
)

func receiveFixture(t *testing.T) (*Manager, string, string) {
	t.Helper()
	host := session.New(nil)
	root := t.TempDir()
	files, err := New(host, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.Close()
		files.Close()
	})
	return files, root, host.ID()
}
func testReceiver(files *Manager, server *httptest.Server) {
	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.DisableCompression = true
	client.Transport = transport
	files.receiver.client = client
}
func receiveInput(id, operation, path, source string) ReceiveArgs {
	return ReceiveArgs{ServerID: id, OperationID: operation, Path: path,
		File: FileReference{DownloadURL: source, FileID: "file_test", MIMEType: "application/octet-stream", FileName: "sample.zip"}}
}
func receiveStatusEventually(t *testing.T, files *Manager, id, operation string) ReceiveStatusResult {
	t.Helper()
	until := time.Now().Add(6 * time.Second)
	for time.Now().Before(until) {
		result, err := files.ReceiveStatus(context.Background(), ReceiveStatusArgs{ServerID: id, OperationID: operation})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "running" {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("file_receive did not finish")
	return ReceiveStatusResult{}
}

func TestReceiveBinaryAndReplay(t *testing.T) {
	files, dir, id := receiveFixture(t)
	payload := append([]byte{0, 0xff, 0x80, '\n', '\r', 0}, bytes.Repeat([]byte{0, 1, 255, 4}, 1024)...)
	var fetched atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched.Add(1)
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	testReceiver(files, server)
	path := filepath.Join(dir, "archive.zip")
	a := receiveInput(id, "receive-binary", path, server.URL+"/download?token=private")
	out, err := files.Receive(context.Background(), a)
	if err != nil || out.OperationID != a.OperationID || out.Replayed {
		t.Fatalf("receive: %+v err=%v", out, err)
	}
	finished := receiveStatusEventually(t, files, id, a.OperationID)
	sum := sha256.Sum256(payload)
	expected := "sha256:" + hex.EncodeToString(sum[:])
	if finished.Status != "completed" || finished.Receipt == nil ||
		finished.Receipt.Version != expected || finished.Receipt.Bytes != len(payload) ||
		finished.Percent == nil || *finished.Percent != 100 ||
		finished.CompletedAt == nil {
		t.Fatalf("incorrect final receipt: %+v", finished)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("binary contents changed: bytes=%d err=%v", len(got), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("received file should be private: %v %v", info, err)
	}

	for range 3 {
		again, err := files.Receive(context.Background(), a)
		if err != nil || !again.Replayed || again.Status != "completed" || fetched.Load() != 1 {
			t.Fatalf("retry must not download twice: %+v %v calls=%d", again, err, fetched.Load())
		}
		status := receiveStatusEventually(t, files, id, a.OperationID)
		if status.Receipt == nil || status.Receipt.Version != expected || status.Status != "completed" {
			t.Fatalf("completed receipt must be repeatable: %+v", status)
		}
	}
	a.OperationID = "try-existing"
	denied, err := files.Receive(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	existing := receiveStatusEventually(t, files, id, a.OperationID)
	if existing.Status != "failed" || existing.Receipt == nil || !strings.Contains(existing.Receipt.Error, "file_exists") || fetched.Load() != 1 {
		t.Fatalf("existing file overwritten or downloaded: submission=%+v status=%+v", denied, existing)
	}
	a.OperationID = "receive-binary"
	a.Path = filepath.Join(dir, "other.zip")
	if _, err := files.Receive(context.Background(), a); err == nil || !strings.Contains(err.Error(), "operation_id_conflict") {
		t.Fatalf("reused operation ID with different path: %v", err)
	}
	if _, err := files.ReceiveStatus(context.Background(), ReceiveStatusArgs{ServerID: id, OperationID: "does-not-exist"}); err == nil {
		t.Fatal("unknown operation unexpectedly had status")
	}
	if _, err := files.ReceiveStatus(context.Background(), ReceiveStatusArgs{ServerID: "old-server", OperationID: "receive-binary"}); err == nil {
		t.Fatal("old server ID unexpectedly had status")
	}
}

func TestReceiveURLSyntaxAllowsAnyHTTPSource(t *testing.T) {
	accepted := []string{
		"https://files.oaiusercontent.com/file?id=a", "https://someoneelse.blob.core.windows.net/files/x/raw?sig=test",
		"https://example.com:8443/download?key=private", "http://127.0.0.1:8080/internal",
		"http://localhost:5000/file", "http://[::1]:8090/data", "https://username:password@another.example.org/private",
		"https://files.oaiusercontent.com.evil.test/a", "https://example.com/file#fragment",
	}
	for _, raw := range accepted {
		u, err := parseDownloadURL(raw)
		if err != nil || u.String() != raw {
			t.Fatalf("valid HTTP(S) URL rejected: %q: %v", raw, err)
		}
	}
	invalid := []string{"", "/tmp/archive.zip", "//example.com/archive", "file:///tmp/archive.zip",
		"ftp://example.com/archive", "https://", "http://", "invalid url", "https://host:invalid-port/data"}
	files, dir, id := receiveFixture(t)
	for i, raw := range invalid {
		if _, err := parseDownloadURL(raw); err == nil {
			t.Fatalf("unsupported/malformed URL accepted: %q", raw)
		}
		out, err := files.Receive(context.Background(), receiveInput(id, "invalid-url-"+string(rune('a'+i)), filepath.Join(dir, "never-created.zip"), raw))
		if err == nil || out.Status == "completed" {
			t.Fatalf("invalid URL was accepted: %+v %v", out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "never-created.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid URL created file: %v", err)
	}
}

func TestReceiveHTTPAndCrossOriginRedirect(t *testing.T) {
	files, dir, id := receiveFixture(t)
	payload := append([]byte{0, 255, 128}, bytes.Repeat([]byte{7, 0, 99}, 1024)...)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/download", http.StatusFound)
	}))
	defer redirect.Close()
	testReceiver(files, redirect)
	for _, tc := range []struct{ name, source string }{
		{"localhost-http", target.URL + "/download"}, {"https-to-http-redirect", redirect.URL + "/redirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".zip")
			if _, err := files.Receive(context.Background(), receiveInput(id, tc.name, path, tc.source)); err != nil {
				t.Fatal(err)
			}
			end := receiveStatusEventually(t, files, id, tc.name)
			if end.Status != "completed" {
				t.Fatalf("transfer failed: %+v", end)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("bytes changed: %d %v", len(got), err)
			}
		})
	}
}

func TestReceiveFailureCleanupAndRedirect(t *testing.T) {
	tests := []struct {
		name   string
		handle http.HandlerFunc
		want   string
	}{
		{"http-error", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusForbidden) }, "HTTP 403"},
		{"encoding", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte("no"))
		}, "Content-Encoding"},
		{"redirect-loop", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/asset?token=private", http.StatusFound)
		}, "10 redirects"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files, dir, id := receiveFixture(t)
			server := httptest.NewTLSServer(tc.handle)
			defer server.Close()
			testReceiver(files, server)
			path := filepath.Join(dir, "output.pdf")
			a := receiveInput(id, "receive-"+tc.name, path, server.URL+"/asset?token=private")
			if _, err := files.Receive(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			out := receiveStatusEventually(t, files, id, a.OperationID)
			if out.Status != "failed" || out.Receipt == nil || !strings.Contains(out.Receipt.Error, tc.want) {
				t.Fatalf("unexpected failed receipt: %+v", out)
			}
			if strings.Contains(out.Receipt.Error, "token=private") {
				t.Fatal("signed URL leaked in failure receipt")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed download created destination: %v", err)
			}
			if candidates, _ := filepath.Glob(filepath.Join(dir, ".terminal-mcp-*.tmp")); len(candidates) != 0 {
				t.Fatalf("staging files left behind: %v", candidates)
			}
		})
	}
}

func TestReceiveProgressKnownAndUnknownLength(t *testing.T) {
	for _, known := range []bool{true, false} {
		name := "unknown"
		if known {
			name = "known"
		}
		t.Run(name, func(t *testing.T) {
			files, dir, id := receiveFixture(t)
			part := bytes.Repeat([]byte{0, 255, 42}, 15000)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			sent := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if known {
					w.Header().Set("Content-Length", fmt.Sprint(len(part)*2))
				}
				_, _ = w.Write(part)
				w.(http.Flusher).Flush()
				close(sent)
				<-release
				_, _ = w.Write(part)
			}))
			defer server.Close()
			a := receiveInput(id, "progress-"+name, filepath.Join(dir, name+".bin"), server.URL+"/file")
			start := time.Now()
			ack, err := files.Receive(context.Background(), a)
			if err != nil || ack.OperationID != a.OperationID || time.Since(start) > time.Second {
				t.Fatalf("asynchronous submission blocked: %+v %v", ack, err)
			}
			select {
			case <-sent:
			case <-time.After(3 * time.Second):
				t.Fatal("source never sent first block")
			}
			var mid ReceiveStatusResult
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				mid, err = files.ReceiveStatus(context.Background(), ReceiveStatusArgs{ServerID: id, OperationID: a.OperationID})
				if err != nil {
					t.Fatal(err)
				}
				if mid.BytesDownloaded >= int64(len(part)) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if mid.Status != "running" || mid.BytesDownloaded < int64(len(part)) {
				t.Fatalf("missing in-flight progress: %+v", mid)
			}
			if known {
				if mid.TotalBytes == nil || *mid.TotalBytes != int64(len(part)*2) || mid.Percent == nil ||
					*mid.Percent <= 0 || *mid.Percent >= 100 {
					t.Fatalf("invalid known progress: %+v", mid)
				}
			} else if mid.TotalBytes != nil || mid.Percent != nil {
				t.Fatalf("invented unknown length: %+v", mid)
			}
			unblock()
			done := receiveStatusEventually(t, files, id, a.OperationID)
			if done.Status != "completed" || done.Percent == nil || *done.Percent != 100 || done.Receipt.Bytes != len(part)*2 {
				t.Fatalf("incomplete final status: %+v", done)
			}
		})
	}
}

func TestReceiveClientCancellationDoesNotAbortAdmittedTransfer(t *testing.T) {
	files, dir, id := receiveFixture(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	started := make(chan struct{})
	var fetched atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		close(started)
		<-release
		_, _ = w.Write([]byte{0, 255, 1})
	}))
	defer server.Close()
	testReceiver(files, server)
	a := receiveInput(id, "receive-cancelled", filepath.Join(dir, "received.bin"), server.URL+"/asset")
	ctx, cancel := context.WithCancel(context.Background())
	start, err := files.Receive(ctx, a)
	if err != nil || start.OperationID != a.OperationID {
		t.Fatalf("submission failed: %+v %v", start, err)
	}
	<-started
	cancel()
	mid, err := files.ReceiveStatus(context.Background(), ReceiveStatusArgs{ServerID: id, OperationID: a.OperationID})
	if err != nil || mid.Status != "running" {
		t.Fatalf("request cancellation stopped background work: %+v %v", mid, err)
	}
	unblock()
	done := receiveStatusEventually(t, files, id, a.OperationID)
	if done.Status != "completed" || fetched.Load() != 1 {
		t.Fatalf("accepted work did not finish: %+v, calls=%d", done, fetched.Load())
	}
}

func TestReceiveAbortCancelsActiveTransfer(t *testing.T) {
	files, dir, id := receiveFixture(t)
	opened := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(opened)
		<-r.Context().Done()
	}))
	defer server.Close()
	a := receiveInput(id, "aborted", filepath.Join(dir, "aborted.bin"), server.URL+"/asset")
	if _, err := files.Receive(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	<-opened
	files.AbortTransfers()
	done := receiveStatusEventually(t, files, id, a.OperationID)
	if done.Status != "failed" || done.Receipt == nil || done.Receipt.Version != "" {
		t.Fatalf("explicit cancellation published a file: %+v", done)
	}
	if _, err := os.Stat(a.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aborted transfer published destination: %v", err)
	}
}

func TestReceiveConcurrentCreateOnly(t *testing.T) {
	files, dir, id := receiveFixture(t)
	payload := bytes.Repeat([]byte{0, 255}, 100)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) }))
	defer server.Close()
	testReceiver(files, server)
	path := filepath.Join(dir, "same.bin")
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, err := files.Receive(context.Background(), receiveInput(id, name, path, server.URL+"/asset")); err != nil {
				t.Error(err)
			}
		}(name)
	}
	wg.Wait()
	success := 0
	for _, name := range []string{"first", "second"} {
		out := receiveStatusEventually(t, files, id, name)
		if out.Status == "completed" {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("concurrent create-only published %d files", success)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("incorrect concurrent result: %v", err)
	}
}
