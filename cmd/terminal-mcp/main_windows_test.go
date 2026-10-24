//go:build windows

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"terminalmcp/internal/winsecure"
)

func TestWindowsHTTPMCPAndFileMutation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := winsecure.EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	address, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	consumed := false
	go func() {
		done <- runWithContext(ctx, []string{"serve", "--listen=" + address,
			"--token-file=" + tokenFile, "--file-root=" + dir, "--audit-file=off"}, nil)
	}()
	defer func() {
		cancel()
		if consumed {
			return
		}
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("Windows MCP server failed to exit")
		}
	}()
	client, err := newUpdateClient(address, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	var infoErr error
	for time.Now().Before(deadline) {
		_, infoErr = client.runtime()
		if infoErr == nil {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if infoErr != nil {
		t.Fatalf("Windows HTTP server not ready: %v", infoErr)
	}
	var listed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := client.rpc("tools/list", map[string]any{}, &listed); err != nil || len(listed.Tools) != 12 {
		t.Fatalf("Windows tools/list: %v tools=%d", err, len(listed.Tools))
	}
	serverID, err := client.serverID()
	if err != nil || serverID == "" {
		t.Fatalf("terminal_list: %v", err)
	}
	path := filepath.Join(dir, "written.txt")
	var writeResult struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
	}
	if err := client.tool("file_write", map[string]any{
		"server_id": serverID, "operation_id": "windows-write", "path": path, "content": "windows-ok\n",
	}, &writeResult); err != nil || writeResult.Receipt.Status != "applied" {
		t.Fatalf("file_write: %+v, %v", writeResult, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "windows-ok\n" {
		t.Fatalf("unexpected written file %q: %v", data, err)
	}
	var readResult struct {
		Files []struct {
			Version string `json:"version"`
			Lines   []struct {
				Text string `json:"text"`
			} `json:"lines"`
		} `json:"files"`
	}
	if err := client.tool("file_read", map[string]any{
		"files": []map[string]any{{"path": path}},
	}, &readResult); err != nil || len(readResult.Files) != 1 ||
		readResult.Files[0].Version == "" {
		t.Fatalf("file_read: %+v, %v", readResult, err)
	}
	// Confirm a version-checked replacement is safe on NTFS, rather than
	// accidentally depending on Unix owner UID or inode structures.
	var editResult struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
	}
	if err := client.tool("file_edit", map[string]any{
		"server_id": serverID, "operation_id": "windows-edit", "path": path,
		"expected_version": readResult.Files[0].Version,
		"edits":            []map[string]string{{"old_text": "windows-ok", "new_text": "windows-edited"}},
	}, &editResult); err != nil || editResult.Receipt.Status != "applied" {
		t.Fatalf("file_edit: %+v, %v", editResult, err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "windows-edited\n" {
		t.Fatalf("Windows edit was not applied: %q: %v", data, err)
	}
	response, err := client.http.Get("http://" + address + "/admin/runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated runtime status %d, want 401", response.StatusCode)
	}
	if err := client.maintenance(serverID, "begin"); err != nil {
		t.Fatalf("prepare shutdown: %v", err)
	}
	if err := client.maintenance(serverID, "shutdown"); err != nil {
		t.Fatalf("fenced shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned: %v", err)
		}
		consumed = true
		cancel()
	case <-time.After(8 * time.Second):
		t.Fatal("Windows HTTP shutdown did not complete")
	}
}

func TestWindowsDefaultTokenACL(t *testing.T) {
	appdata := filepath.Join(t.TempDir(), "AppData")
	if err := winsecure.EnsurePrivateDir(appdata); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPDATA", appdata)
	t.Setenv("TERMINAL_MCP_TOKEN", "")
	token, err := loadHTTPToken("")
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("unexpected random token length %d", len(token))
	}
	path, err := defaultTokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := winsecure.CheckPrivate(path); err != nil {
		t.Fatalf("Windows token permissions are too broad: %v", err)
	}
}
