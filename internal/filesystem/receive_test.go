package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	return ReceiveArgs{
		ServerID: id, OperationID: operation, Path: path,
		File: FileReference{DownloadURL: source, FileID: "file_test", MIMEType: "application/octet-stream", FileName: "sample.zip"},
	}
}

func TestReceiveBinaryAndReplay(t *testing.T) {
	files, dir, id := receiveFixture(t)
	payload := append([]byte{0, 0xff, 0x80, '\n', '\r', 0}, bytes.Repeat([]byte{0, 1, 255, 4}, 1024)...)
	var fetched atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	testReceiver(files, server)
	path := filepath.Join(dir, "archive.zip")
	a := receiveInput(id, "receive-binary", path, server.URL+"/download?token=private")
	out, err := files.Receive(context.Background(), a)
	if err != nil || out.Receipt.Status != "applied" || out.Replayed {
		t.Fatalf("receive: %+v err=%v", out, err)
	}
	sum := sha256.Sum256(payload)
	if out.Receipt.Version != "sha256:"+hex.EncodeToString(sum[:]) || out.Receipt.Bytes != len(payload) {
		t.Fatalf("wrong file receipt: %+v", out.Receipt)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("binary contents changed: bytes=%d err=%v", len(got), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("received file should be private: %v %v", info, err)
	}
	again, err := files.Receive(context.Background(), a)
	if err != nil || !again.Replayed || again.Receipt.Version != out.Receipt.Version || fetched.Load() != 1 {
		t.Fatalf("retry must not download twice: %+v %v calls=%d", again, err, fetched.Load())
	}
	a.OperationID = "try-existing"
	denied, err := files.Receive(context.Background(), a)
	if err != nil || denied.Receipt.Status != "failed" || !strings.Contains(denied.Receipt.Error, "file_exists") || fetched.Load() != 1 {
		t.Fatalf("existing file overwritten or downloaded: %+v %v", denied, err)
	}
	a.OperationID = "receive-binary"
	a.Path = filepath.Join(dir, "other.zip")
	if _, err := files.Receive(context.Background(), a); err == nil || !strings.Contains(err.Error(), "operation_id_conflict") {
		t.Fatalf("reused operation_id with different path: %v", err)
	}
}

func TestReceiveURLSyntaxAllowsAnyHTTPSource(t *testing.T) {
	accepted := []string{
		"https://files.oaiusercontent.com/file?id=a",
		"https://someoneelse.blob.core.windows.net/files/x/raw?sig=test",
		"https://example.com:8443/download?key=private",
		"http://127.0.0.1:8080/internal",
		"http://localhost:5000/file",
		"http://[::1]:8090/data",
		"https://username:password@another.example.org/private",
		"https://files.oaiusercontent.com.evil.test/a",
		"https://example.com/file#fragment",
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
			t.Fatalf("unsupported or malformed URL accepted: %q", raw)
		}
		out, err := files.Receive(context.Background(), receiveInput(id,
			"invalid-url-"+string(rune('a'+i)), filepath.Join(dir, "never-created.zip"), raw))
		if err == nil || out.Receipt.Status == "applied" {
			t.Fatalf("invalid URL should be rejected without side effects: %+v %v", out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "never-created.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid URL created file: %v", err)
	}
}

func TestReceiveHTTPAndCrossOriginRedirect(t *testing.T) {
	files, dir, id := receiveFixture(t)
	payload := append([]byte{0, 255, 128}, bytes.Repeat([]byte{7, 0, 99}, 1024)...)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/download", http.StatusFound)
	}))
	defer redirect.Close()
	testReceiver(files, redirect)

	for _, tc := range []struct {
		name, source string
	}{
		{"localhost-http", target.URL + "/download"},
		{"https-to-http-redirect", redirect.URL + "/redirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".zip")
			out, err := files.Receive(context.Background(), receiveInput(id, tc.name, path, tc.source))
			if err != nil || out.Receipt.Status != "applied" {
				t.Fatalf("transfer failed: %+v %v", out, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("bytes changed after transfer: got %d bytes, %v", len(got), err)
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
		{"too-large", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "134217729")
			w.WriteHeader(http.StatusOK)
		}, "exceeds"},
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
			out, err := files.Receive(context.Background(), a)
			if err != nil || out.Receipt.Status != "failed" || !strings.Contains(out.Receipt.Error, tc.want) {
				t.Fatalf("unexpected failed receipt: %+v err=%v", out, err)
			}
			if strings.Contains(out.Receipt.Error, "token=private") {
				t.Fatal("signed URL leaked in failure receipt")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed receive created destination: %v", err)
			}
			if candidates, _ := filepath.Glob(filepath.Join(dir, ".terminal-mcp-*.tmp")); len(candidates) != 0 {
				t.Fatalf("staging files left behind: %v", candidates)
			}
		})
	}
}

func TestReceiveClientCancellationDoesNotAbortAdmittedTransfer(t *testing.T) {
	files, dir, id := receiveFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var fetched atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		close(started)
		<-release
		_, _ = w.Write([]byte{0, 0xff, 0x01})
	}))
	defer server.Close()
	testReceiver(files, server)
	a := receiveInput(id, "receive-cancelled", filepath.Join(dir, "received.bin"), server.URL+"/asset")
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, 1)
	go func() {
		_, err := files.Receive(ctx, a)
		results <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case err := <-results:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancelled observation: %v", err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("cancelled tool did not return")
	}
	close(release)
	again, err := files.Receive(context.Background(), a)
	if err != nil || !again.Replayed || again.Receipt.Status != "applied" || fetched.Load() != 1 {
		t.Fatalf("accepted effect did not finish: %+v %v fetched=%d", again, err, fetched.Load())
	}
}

func TestReceiveConcurrentCreateOnly(t *testing.T) {
	files, dir, id := receiveFixture(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{0, 255}, 100))
	}))
	defer server.Close()
	testReceiver(files, server)
	path := filepath.Join(dir, "same.bin")
	var wg sync.WaitGroup
	results := make(chan ActionResult, 2)
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			out, err := files.Receive(context.Background(), receiveInput(id, name, path, server.URL+"/asset"))
			if err != nil {
				t.Error(err)
			}
			results <- out
		}(name)
	}
	wg.Wait()
	close(results)
	success := 0
	for out := range results {
		if out.Receipt.Status == "applied" {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("concurrent create-only published %d files", success)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{0, 255}, 100)) {
		t.Fatalf("incorrect concurrent result: %v", err)
	}
}
