//go:build windows

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
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
	if err := client.rpc("tools/list", map[string]any{}, &listed); err != nil || len(listed.Tools) != 13 {
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

func TestWindowsBackgroundStartStatusStopAndAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs an actual Windows executable")
	}
	root := t.TempDir()
	appdata := filepath.Join(root, "AppData")
	if err := winsecure.EnsurePrivateDir(appdata); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "terminal-mcp.exe")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	if result, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Windows CLI: %v: %s", err, result)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = root
		cmd.Env = append(withoutEnvToken(os.Environ()), "APPDATA="+appdata)
		result, err := cmd.CombinedOutput()
		return string(result), err
	}
	defer func() {
		_, _ = run("stop")
	}()
	address, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	stableCWD := filepath.Join(root, "stable-cwd")
	if err := os.Mkdir(stableCWD, 0700); err != nil {
		t.Fatal(err)
	}
	const explicitEnvKey = "TERMINAL_MCP_WINDOWS_POLICY_TEST"
	output, err := run("start", "--listen="+address, "--file-root="+root,
		"--working-directory="+stableCWD, "--env="+explicitEnvKey+"=from-explicit-config")
	if err != nil {
		t.Fatalf("Windows background start failed: %v: %s", err, output)
	}
	if !strings.Contains(output, `"status":"started"`) {
		t.Fatalf("start did not report success: %s", output)
	}
	output, err = run("status")
	installed := filepath.Join(appdata, "terminal-mcp", "standalone", "terminal-mcp.exe")
	currentHash, hashErr := fileSHA256(installed)
	if err != nil || hashErr != nil || !strings.Contains(output, `"status":"running"`) ||
		!strings.Contains(output, currentHash) || !strings.Contains(output, installed) {
		t.Fatalf("Windows background status does not identify installed executable: %v %v: %s", err, hashErr, output)
	}
	if output, err := run("start"); err == nil || !strings.Contains(output, "server already listening") {
		t.Fatalf("duplicate Windows start was allowed: %v %s", err, output)
	}
	state := filepath.Join(appdata, "terminal-mcp", "standalone")
	var saved winStandaloneConfig
	if err := readJSON(filepath.Join(state, "config.json"), &saved); err != nil ||
		saved.EnvOverrides[explicitEnvKey] != "from-explicit-config" ||
		saved.WorkingDirectory != stableCWD {
		t.Fatalf("Windows standalone did not persist explicit cwd/env: %+v %v", saved, err)
	}
	configBytes, err := os.ReadFile(filepath.Join(state, "config.json"))
	if err != nil || strings.Contains(string(configBytes), `"path":`) {
		t.Fatalf("Windows persisted an obsolete PATH snapshot: %s %v", configBytes, err)
	}
	client, err := newUpdateClient(address, filepath.Join(appdata, "terminal-mcp", "token"))
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := client.serverID()
	if err != nil {
		t.Fatal(err)
	}
	var ran struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
		Observation struct {
			Events []struct {
				Text *string `json:"text"`
			} `json:"events"`
		} `json:"observation"`
	}
	cmdExe := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	if err := client.tool("terminal_start", map[string]any{
		"server_id": serverID, "operation_id": "windows-explicit-env-e2e",
		"argv":          []string{cmdExe, "/c", "echo %" + explicitEnvKey + "%"},
		"close_on_exit": true, "wait_for": "completion", "wait_ms": 3000,
	}, &ran); err != nil || ran.Receipt.Status != "applied" {
		t.Fatalf("Windows server could not spawn command with env: %+v %v", ran, err)
	}
	foundEnv := false
	for _, event := range ran.Observation.Events {
		if event.Text != nil && strings.Contains(*event.Text, "from-explicit-config") {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Fatalf("Windows child did not inherit explicit environment: %+v", ran.Observation)
	}
	for _, file := range []string{
		filepath.Join(state, "config.json"),
		filepath.Join(appdata, "terminal-mcp", "logs", "server.stdout.log"),
		filepath.Join(appdata, "terminal-mcp", "logs", "server.stderr.log"),
	} {
		if err := winsecure.CheckPrivate(file); err != nil {
			t.Fatalf("Windows standalone file is not private: %s: %v", file, err)
		}
	}
	output, err = run("stop")
	if err != nil || !strings.Contains(output, `"status":"stopped"`) {
		t.Fatalf("Windows stop: %v %s", err, output)
	}
	different := filepath.Join(root, "different-terminal-mcp.exe")
	if out, err := exec.Command("go", "build", "-trimpath", "-ldflags=-buildid=other-windows-version",
		"-o", different, ".").CombinedOutput(); err != nil {
		t.Fatalf("build different Windows CLI: %s %v", out, err)
	}
	differentCmd := exec.Command(different, "start")
	differentCmd.Dir = root
	differentCmd.Env = append(withoutEnvToken(os.Environ()), "APPDATA="+appdata)
	if refused, err := differentCmd.CombinedOutput(); err == nil ||
		!strings.Contains(string(refused), "installed_version_mismatch") {
		t.Fatalf("Windows start silently installed a different version: %s %v", refused, err)
	}
	if hash, err := fileSHA256(installed); err != nil || hash != currentHash {
		t.Fatalf("Windows mismatched start changed installed executable: %q %v", hash, err)
	}
	audit := filepath.Join(appdata, "terminal-mcp", "logs", "audit.jsonl")
	data, err := os.ReadFile(audit)
	if err != nil || !strings.Contains(string(data), "server_start") {
		t.Fatalf("Windows default audit was not flushed: %v: %s", err, data)
	}
	if err := winsecure.CheckPrivate(audit); err != nil {
		t.Fatalf("Windows audit file is not private: %v", err)
	}
}
