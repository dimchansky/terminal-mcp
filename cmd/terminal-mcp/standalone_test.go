//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func standaloneTestCommand(binary, home string, args ...string) ([]byte, error) {
	cmd := exec.Command(binary, args...)
	cmd.Dir = home
	cmd.Env = append(withoutEnvToken(os.Environ()), "HOME="+home)
	return cmd.CombinedOutput()
}

func standaloneTestBinary(t *testing.T) (string, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs a real standalone CLI")
	}
	home := t.TempDir()
	exe := filepath.Join(home, "from-build")
	build := exec.Command("go", "build", "-trimpath", "-o", exe, ".")
	if result, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test binary: %v\n%s", err, result)
	}
	return home, exe
}

func TestStandaloneBackgroundStartStop(t *testing.T) {
	home, binary := standaloneTestBinary(t)
	addr, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(home, ".config", "terminal-mcp", "token")
	t.Cleanup(func() {
		client, err := newUpdateClient(addr, tokenFile)
		if err != nil {
			return
		}
		info, err := client.runtime()
		if err == nil && strings.HasPrefix(info.Executable, home+string(filepath.Separator)) {
			if client.maintenance(info.ServerID, "begin") == nil {
				_ = client.maintenance(info.ServerID, "shutdown")
			}
		}
	})
	result, err := standaloneTestCommand(binary, home, "start", "--listen="+addr,
		"--file-root="+home, "--audit-file=off", "--audit-max-bytes=1048576")
	if err != nil {
		t.Fatalf("standalone start: %v\n%s", err, result)
	}
	var started struct {
		Status    string `json:"status"`
		PID       int    `json:"pid"`
		ServerID  string `json:"server_id"`
		StdoutLog string `json:"stdout_log"`
		StderrLog string `json:"stderr_log"`
	}
	if err := json.Unmarshal(result, &started); err != nil ||
		started.Status != "started" || started.ServerID == "" || started.PID < 2 ||
		started.StdoutLog != filepath.Join(home, ".config", "terminal-mcp", "logs", "server.stdout.log") ||
		started.StderrLog != filepath.Join(home, ".config", "terminal-mcp", "logs", "server.stderr.log") {
		t.Fatalf("invalid start result: %v %s", err, result)
	}
	client, err := newUpdateClient(addr, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	physicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerID != started.ServerID || info.PID != started.PID || info.FileRoot != physicalHome ||
		info.Audit.BytesLimit != 0 { // Audit explicitly disabled for this fixture.
		t.Fatalf("unexpected runtime after standalone start: %+v", info)
	}
	saved, err := savedStandaloneConfig(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	if err != nil || saved.Listen != addr || saved.FileRoot != physicalHome ||
		saved.TokenFile != tokenFile || saved.WorkingDirectory != physicalHome {
		t.Fatalf("unexpected saved standalone configuration: %+v, %v", saved, err)
	}
	installed := filepath.Join(home, ".config", "terminal-mcp", "standalone", "terminal-mcp")
	installedFile, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	actualFile, err := os.Stat(info.Executable)
	if err != nil || !os.SameFile(actualFile, installedFile) || info.Executable == binary {
		t.Fatalf("standalone process is not using its own installed copy: %s %v", info.Executable, err)
	}
	beforeHash, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}
	otherPort, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	if result, err := standaloneTestCommand(binary, home, "start", "--listen="+otherPort); err == nil ||
		!strings.Contains(string(result), "already has a listener") {
		t.Fatalf("duplicate start did not fail safely: %v %s", err, result)
	}
	if afterHash, err := fileSHA256(installed); err != nil || beforeHash != afterHash {
		t.Fatalf("duplicate start overwrote the running executable: %v", err)
	}
	status, err := standaloneTestCommand(binary, home, "status")
	if err != nil || !strings.Contains(string(status), started.ServerID) ||
		!strings.Contains(string(status), `"status":"running"`) {
		t.Fatalf("standalone status: %v %s", err, status)
	}
	stopped, err := standaloneTestCommand(binary, home, "stop")
	if err != nil || !strings.Contains(string(stopped), `"status":"stopped"`) {
		t.Fatalf("standalone stop: %v %s", err, stopped)
	}
	status, err = standaloneTestCommand(binary, home, "status")
	if err != nil || !strings.Contains(string(status), `"status":"stopped"`) {
		t.Fatalf("status after stop: %v %s", err, status)
	}

	// A zero flag explicitly clears a persisted budget; omission retains it.
	restarted, err := standaloneTestCommand(binary, home, "start", "--audit-max-bytes=0")
	if err != nil {
		t.Fatalf("restart with unlimited audit budget: %v %s", err, restarted)
	}
	saved, err = savedStandaloneConfig(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	if err != nil || saved.AuditMaxBytes != 0 {
		t.Fatalf("explicit unlimited budget was not persisted: %+v %v", saved, err)
	}
	if output, err := standaloneTestCommand(binary, home, "stop"); err != nil ||
		!strings.Contains(string(output), `"status":"stopped"`) {
		t.Fatalf("stop after unlimited restart: %v %s", err, output)
	}
}

func TestStandaloneStartRefusesOccupiedPort(t *testing.T) {
	home, binary := standaloneTestBinary(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	output, err := standaloneTestCommand(binary, home, "start", "--listen="+listener.Addr().String())
	if err == nil || !strings.Contains(string(output), "already has a listener") {
		t.Fatalf("start attempted to take over existing port: %v %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "terminal-mcp", "standalone")); !os.IsNotExist(err) {
		t.Fatalf("failed start created standalone installation: %v", err)
	}
}

func TestStandaloneMigratesLegacyConfig(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("legacy launchd controller is macOS-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "files")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	original := standaloneConfig{
		Listen: "127.0.0.1:8912", FileRoot: root,
		WorkingDirectory: home, TokenFile: filepath.Join(home, "existing-token"),
	}
	// The saved launchd format uses "project" rather than "working_directory".
	if err := writeJSON(filepath.Join(legacy, "config.json"), map[string]string{
		"listen": original.Listen, "token_file": original.TokenFile,
		"file_root": original.FileRoot, "project": original.WorkingDirectory,
		"path": "/opt/test/bin:/usr/bin",
	}); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, ".config", "terminal-mcp", "standalone")
	c, err := initialStandaloneConfig(state)
	if err != nil || !reflect.DeepEqual(c, original) {
		t.Fatalf("legacy migration lost configuration: %+v %v", c, err)
	}
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(state, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(legacy, "config.json")); err != nil {
		t.Fatal(err)
	}
	after, err := initialStandaloneConfig(state)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatalf("standalone configuration depends on removed legacy files: %+v %v", after, err)
	}
}

func TestStandaloneStopDoesNotInterruptActiveSession(t *testing.T) {
	home, binary := standaloneTestBinary(t)
	addr, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, ".config", "terminal-mcp", "token")
	if out, err := standaloneTestCommand(binary, home, "start", "--listen="+addr, "--audit-file=off"); err != nil {
		t.Fatalf("start: %v %s", err, out)
	}
	defer func() {
		_, _ = standaloneTestCommand(binary, home, "stop")
	}()
	client, err := newUpdateClient(addr, token)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	var start struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
	}
	if err := client.tool("terminal_start", map[string]any{
		"server_id": info.ServerID, "operation_id": "block-manual-stop",
		"argv": []string{"/bin/sleep", "2"}, "close_on_exit": true,
	}, &start); err != nil || start.Receipt.Status != "applied" {
		t.Fatalf("cannot launch active session: %+v %v", start, err)
	}
	output, err := standaloneTestCommand(binary, home, "stop")
	if err != nil || !strings.Contains(string(output), `"status":"draining"`) {
		t.Fatalf("stop must begin graceful drain with an active session: %v %s", err, output)
	}
	current, err := client.runtime()
	if err != nil || current.ServerID != info.ServerID || !current.Draining {
		t.Fatalf("server stopped or was not draining: %+v %v", current, err)
	}
	if err := waitForExit(info.PID, 5*time.Second); err != nil {
		t.Fatalf("graceful stop did not complete after process exit: %v", err)
	}
}

func TestStandaloneGracefulStopDrainsActiveFileReceive(t *testing.T) {
	home, binary := standaloneTestBinary(t)
	addr, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0, 255, 128, 42}, 16384)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	started := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload[:32768])
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = w.Write(payload[32768:])
	}))
	defer source.Close()
	if out, err := standaloneTestCommand(binary, home, "start", "--listen="+addr, "--file-root="+home, "--audit-file=off"); err != nil {
		t.Fatalf("start: %v %s", err, out)
	}
	defer func() {
		unblock()
		_, _ = standaloneTestCommand(binary, home, "stop", "--force")
	}()
	token := filepath.Join(home, ".config", "terminal-mcp", "token")
	client, err := newUpdateClient(addr, token)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(info.FileRoot, "received.bin")
	var accepted struct {
		OperationID string `json:"operation_id"`
		Status      string `json:"status"`
	}
	if err := client.tool("file_receive", map[string]any{
		"server_id": info.ServerID, "operation_id": "graceful-receive", "wait_ms": 0,
		"path": dest, "file": map[string]any{
			"file_id": "third-party-file", "download_url": source.URL + "/download",
		},
	}, &accepted); err != nil || accepted.OperationID != "graceful-receive" {
		t.Fatalf("asynchronous admission: %+v %v (file_root=%q)", accepted, err, info.FileRoot)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("file download did not start")
	}
	var progress struct {
		Status string `json:"status"`
	}
	if err := client.tool("file_receive_result", map[string]any{
		"server_id": info.ServerID, "operation_id": "graceful-receive", "wait_ms": 0,
	}, &progress); err != nil || progress.Status != "running" {
		t.Fatalf("download was not running: %+v %v", progress, err)
	}
	out, err := standaloneTestCommand(binary, home, "stop")
	if err != nil || !strings.Contains(string(out), `"status":"draining"`) {
		t.Fatalf("stop must drain accepted download: %v %s", err, out)
	}
	current, err := client.runtime()
	if err != nil || current.ServerID != info.ServerID || !current.Draining ||
		current.ActiveOperations < 1 {
		t.Fatalf("active transfer was not protected by drain: %+v %v", current, err)
	}
	unblock()
	if err := waitForExit(info.PID, 6*time.Second); err != nil {
		t.Fatalf("server did not stop after transfer finished: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("drain interrupted binary download: %d bytes, %v", len(got), err)
	}
}

func TestStandaloneForceStopTerminatesManagedProcess(t *testing.T) {
	home, binary := standaloneTestBinary(t)
	addr, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, ".config", "terminal-mcp", "token")
	if out, err := standaloneTestCommand(binary, home, "start", "--listen="+addr, "--audit-file=off"); err != nil {
		t.Fatalf("start: %v %s", err, out)
	}
	defer func() { _, _ = standaloneTestCommand(binary, home, "stop", "--force") }()
	client, err := newUpdateClient(addr, token)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	var start struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
	}
	if err := client.tool("terminal_start", map[string]any{
		"server_id": info.ServerID, "operation_id": "force-stop-process",
		"argv": []string{"/bin/sleep", "90"}, "close_on_exit": false,
	}, &start); err != nil || start.Receipt.Status != "applied" {
		t.Fatalf("cannot launch managed job: %+v %v", start, err)
	}
	if out, err := standaloneTestCommand(binary, home, "stop", "--force"); err != nil ||
		!strings.Contains(string(out), `"forced":true`) {
		t.Fatalf("forced stop failed: %v %s", err, out)
	}
	if err := waitForExit(info.PID, 5*time.Second); err != nil {
		t.Fatalf("server survived explicit --force: %v", err)
	}
}

func TestStandaloneCanaryDoesNotDisruptLegacyListener(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the old launchd configuration is specific to macOS")
	}
	home, binary := standaloneTestBinary(t)
	legacyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacyListener.Close() }()
	canary, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, ".config", "terminal-mcp", "token")
	if err := os.MkdirAll(filepath.Dir(token), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte(testToken), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(legacy, "config.json"), map[string]string{
		"project": home, "listen": legacyListener.Addr().String(),
		"token_file": token, "file_root": "/", "path": "/usr/bin:/bin",
	}); err != nil {
		t.Fatal(err)
	}
	out, err := standaloneTestCommand(binary, home, "start", "--listen="+canary, "--audit-file=off")
	if err != nil {
		t.Fatalf("canary could not start alongside legacy listener: %v %s", err, out)
	}
	t.Cleanup(func() {
		_, _ = standaloneTestCommand(binary, home, "stop")
	})
	client, err := newUpdateClient(canary, token)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	if info.FileRoot != "/" || info.WorkingDirectory == "" || info.Listen != canary {
		t.Fatalf("canary lost legacy settings: %+v", info)
	}
	var env struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
		Observation struct {
			Events []struct {
				Text *string `json:"text"`
			} `json:"events"`
		} `json:"observation"`
	}
	err = client.tool("terminal_start", map[string]any{
		"server_id": info.ServerID, "operation_id": "verify-inherited-path",
		"argv": []string{"/usr/bin/printenv", "PATH"}, "close_on_exit": true,
		"wait_for": "completion", "wait_ms": 3000,
	}, &env)
	if err != nil || env.Receipt.Status != "applied" || len(env.Observation.Events) == 0 ||
		env.Observation.Events[0].Text == nil || strings.TrimSpace(*env.Observation.Events[0].Text) != os.Getenv("PATH") {
		t.Fatalf("canary lost legacy PATH: %+v %v", env, err)
	}
	if !listenerInUse(legacyListener.Addr().String()) {
		t.Fatal("canary installation stopped the legacy listener")
	}
	saved, err := savedStandaloneConfig(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	if err != nil || saved.FileRoot != "/" || len(saved.EnvOverrides) != 0 {
		t.Fatalf("standalone config should keep root but discard legacy PATH: %+v %v", saved, err)
	}
	savedJSON, err := os.ReadFile(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	if err != nil || strings.Contains(string(savedJSON), `"path":`) {
		t.Fatalf("legacy PATH snapshot was persisted again: %q %v", savedJSON, err)
	}
	// After the canary stops and the legacy service is removed, the operator
	// can restore production auditing without retaining the canary's "off".
	out, err = standaloneTestCommand(binary, home, "stop")
	if err != nil {
		t.Fatalf("stop canary: %v %s", err, out)
	}
	out, err = standaloneTestCommand(binary, home, "start", "--listen="+canary, "--audit-file=default")
	if err != nil {
		t.Fatalf("restart with default audit: %v %s", err, out)
	}
	saved, err = savedStandaloneConfig(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	// Subprocess HOME is isolated. The persisted default must use the
	// canary's HOME rather than this Go test runner's home.
	wantDefault := filepath.Join(home, ".config", "terminal-mcp", "logs", "audit.jsonl")
	if err != nil || saved.AuditFile != wantDefault {
		t.Fatalf("canary audit disable was not cleared: %+v want=%q %v", saved, wantDefault, err)
	}
	if !listenerInUse(legacyListener.Addr().String()) {
		t.Fatal("legacy listener was affected by canary restart")
	}
}
