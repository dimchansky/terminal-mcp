//go:build linux || darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"terminalmcp/internal/mcpserver"
)

type updateFixture struct {
	t         *testing.T
	home      string
	target    string
	candidate string
	token     string
	address   string
	state     string
	client    *updateClient
	oldID     string
	autoToken bool
}

func newUpdateFixture(t *testing.T, autoToken bool) *updateFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and restarts real server processes")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, "installed")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(binDir, "terminal-mcp")
	build := exec.Command("go", "build", "-trimpath", "-o", target, ".")
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build standalone server: %v\n%s", err, output)
	}
	candidate := filepath.Join(home, "candidate")
	if err := copyExecutable(target, candidate, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, "token")
	if autoToken {
		token = filepath.Join(home, ".config", "terminal-mcp", "token")
	} else if err := os.WriteFile(token, []byte(testToken), 0600); err != nil {
		t.Fatal(err)
	}
	address, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(home, "initial.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	serverArgs := []string{"serve", "--listen=" + address, "--audit-file=off"}
	if !autoToken {
		serverArgs = append(serverArgs, "--token-file="+token)
	}
	cmd := exec.Command(target, serverArgs...)
	cmd.Dir = home
	cmd.Stderr = logFile
	cmd.Env = append(withoutEnvToken(os.Environ()), "HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	if autoToken {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(token); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	client, err := newUpdateClient(address, token)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &updateFixture{t: t, home: home, target: target, candidate: candidate,
		token: token, address: address, state: filepath.Join(home, "updates"), client: client, autoToken: autoToken}
	// os.Executable resolves /var/... to /private/var/... on macOS, while
	// t.TempDir may return the /var spelling. Compare canonical paths: otherwise
	// successful detached restarts survive the test and leak live servers.
	physicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		info, err := client.runtime()
		if err == nil {
			if !strings.HasPrefix(info.Executable, physicalHome+string(filepath.Separator)) {
				t.Errorf("test cleanup found an unexpected executable at %s; refusing to stop it", info.Executable)
			} else {
				if client.maintenance(info.ServerID, "begin") == nil {
					_ = client.maintenance(info.ServerID, "shutdown")
				} else {
					_ = syscall.Kill(info.PID, syscall.SIGTERM)
				}
				if err := waitForExit(info.PID, 5*time.Second); err != nil {
					t.Errorf("updater test server survived cleanup: %v", err)
				}
			}
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	info, err := pollStandalone(client, "", 12*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fixture.oldID = info.ServerID
	return fixture
}

func pollStandalone(client *updateClient, oldID string, timeout time.Duration) (mcpserver.RuntimeInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := client.runtime()
		if err == nil && info.ServerID != oldID {
			return info, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return mcpserver.RuntimeInfo{}, fmt.Errorf("standalone server was not ready")
}

func (f *updateFixture) update(candidate string) {
	f.t.Helper()
	command := exec.Command(f.target, "update", "--candidate="+candidate,
		"--listen="+f.address, "--token-file="+f.token, "--state-dir="+f.state)
	command.Env = append(withoutEnvToken(os.Environ()), "HOME="+f.home)
	result, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("queue update: %v\n%s", err, result)
	}
	var queued map[string]any
	if err := json.Unmarshal(result, &queued); err != nil || queued["status"] != "queued" {
		f.t.Fatalf("invalid queued status: %q, %v", result, err)
	}
}

// Updating through terminal_start proves the detached worker survives the
// termination of the very server that launched its original command.
func (f *updateFixture) updateFromMCP(candidate string) {
	f.t.Helper()
	var result struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
		Observation struct {
			Session struct {
				Closed bool `json:"closed"`
			} `json:"session"`
		} `json:"observation"`
	}
	args := []string{f.target, "update", "--candidate=" + candidate,
		"--listen=" + f.address, "--state-dir=" + f.state}
	if !f.autoToken {
		args = append(args, "--token-file="+f.token)
	}
	err := f.client.tool("terminal_start", map[string]any{
		"server_id": f.oldID, "operation_id": "self-update-from-managed-terminal",
		"argv":          args,
		"close_on_exit": true, "wait_for": "completion", "wait_ms": 5000,
	}, &result)
	if err != nil || result.Receipt.Status != "applied" || !result.Observation.Session.Closed {
		f.t.Fatalf("update command launched through MCP failed: %+v %v", result, err)
	}
}

func (f *updateFixture) status() (updateStatus, error) {
	var latest struct {
		ID string `json:"id"`
	}
	if err := readJSON(filepath.Join(f.state, "latest.json"), &latest); err != nil {
		return updateStatus{}, err
	}
	var result updateStatus
	err := readJSON(filepath.Join(f.state, latest.ID, "status.json"), &result)
	return result, err
}

func (f *updateFixture) waitResult() updateStatus {
	f.t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		state, err := f.status()
		if err == nil {
			switch state.Status {
			case "applied", "failed", "rolled_back":
				return state
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatal("update worker did not finish")
	return updateStatus{}
}

func TestStandaloneUpdaterEndToEnd(t *testing.T) {
	f := newUpdateFixture(t, true)
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-buildid=standalone-e2e-next", "-o", f.candidate, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build different candidate: %v\n%s", err, output)
	}
	f.updateFromMCP(f.candidate)
	result := f.waitResult()
	if result.Status != "applied" {
		t.Fatalf("update did not apply: %+v", result)
	}
	current, err := f.client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	if current.ServerID == f.oldID || current.ServerID != result.ServerID {
		t.Fatalf("server was not replaced: %+v", result)
	}
	if want, err := fileSHA256(f.candidate); err != nil || current.ExecutableSHA256 != want {
		t.Fatalf("wrong executable was launched: got %s, want %s, err %v", current.ExecutableSHA256, want, err)
	}
	if want, err := fileSHA256(f.target); err != nil || current.ExecutableSHA256 != want {
		t.Fatalf("installed executable differs from running one: %v", err)
	}
	var latest struct {
		ID string `json:"id"`
	}
	if err := readJSON(filepath.Join(f.state, "latest.json"), &latest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.state, "pending")); !os.IsNotExist(err) {
		t.Fatalf("pending marker retained after success: %v", err)
	}
}

func TestStandaloneUpdaterRejectsUnstartableCandidate(t *testing.T) {
	f := newUpdateFixture(t, false)
	bad := filepath.Join(f.home, "bad-candidate")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.update(bad)
	result := f.waitResult()
	if result.Status != "failed" || !strings.Contains(result.Error, "preflight") {
		t.Fatalf("bad candidate was not rejected by preflight: %+v", result)
	}
	info, err := f.client.runtime()
	if err != nil || info.ServerID != f.oldID {
		t.Fatalf("original server was interrupted: %+v, %v", info, err)
	}
}

func TestStandaloneUpdaterRollsBackFailedActivation(t *testing.T) {
	f := newUpdateFixture(t, false)
	var started struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
	}
	if err := f.client.tool("terminal_start", map[string]any{
		"server_id": f.oldID, "operation_id": "keep-updater-gated",
		"argv": []string{"/bin/sleep", "5"}, "close_on_exit": true,
	}, &started); err != nil || started.Receipt.Status != "applied" {
		t.Fatalf("cannot hold old session: %+v, %v", started, err)
	}
	f.update(f.candidate)
	deadline := time.Now().Add(15 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		status, err := f.status()
		if err == nil && status.Status == "waiting_for_idle" {
			waiting = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("worker never reached idle fence")
	}
	installedDir := filepath.Dir(f.target)
	if err := os.Chmod(installedDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(installedDir, 0700) })
	result := f.waitResult()
	if result.Status != "rolled_back" || !strings.Contains(result.Error, "permission") {
		t.Fatalf("expected rollback after install failed: %+v", result)
	}
	info, err := f.client.runtime()
	if err != nil || info.ServerID == f.oldID {
		t.Fatalf("original binary did not restart: %+v, %v", info, err)
	}
}

func TestStandaloneUpdaterRestoresOldBinaryAfterBadRestart(t *testing.T) {
	f := newUpdateFixture(t, false)
	// Build a real candidate from the current source with one test-only
	// difference: it boots on the preflight port but fails on the live port.
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	marker := "manager := session.New(terminal.Start)"
	if !strings.Contains(string(source), marker) {
		t.Fatal("cannot instrument candidate")
	}
	modified := strings.Replace(string(source), marker,
		"if opts.listen == "+fmt.Sprintf("%q", f.address)+
			" { return errors.New(\"injected restart failure\") }\n\t"+marker, 1)
	overlaySource := filepath.Join(f.home, "modified-main.go")
	if err := os.WriteFile(overlaySource, []byte(modified), 0600); err != nil {
		t.Fatal(err)
	}
	mainPath, err := filepath.Abs("main.go")
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(f.home, "overlay.json")
	if err := writeJSON(overlay, map[string]any{
		"Replace": map[string]string{mainPath: overlaySource},
	}); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-overlay="+overlay, "-o", f.candidate, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build injected candidate: %v\n%s", err, output)
	}
	before, err := fileSHA256(f.target)
	if err != nil {
		t.Fatal(err)
	}
	f.update(f.candidate)
	result := f.waitResult()
	if result.Status != "rolled_back" || !strings.Contains(result.Error, "readiness") {
		t.Fatalf("expected successful rollback after bad restart: %+v", result)
	}
	current, err := f.client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	after, err := fileSHA256(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if after != before || current.ExecutableSHA256 != before || current.ServerID == f.oldID {
		t.Fatalf("rollback did not restore the old executable and a new instance: %+v", current)
	}
}

func TestStandaloneUpdaterRejectsOverwrittenLiveExecutable(t *testing.T) {
	f := newUpdateFixture(t, false)
	temporary := f.target + ".replaced"
	if err := copyExecutable(f.target, temporary, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("in-place rebuild")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, f.target); err != nil {
		t.Fatal(err)
	}
	update := exec.Command(f.candidate, "update", "--candidate="+f.candidate,
		"--listen="+f.address, "--token-file="+f.token, "--state-dir="+f.state)
	update.Env = append(withoutEnvToken(os.Environ()), "HOME="+f.home)
	output, err := update.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "replaced after startup") {
		t.Fatalf("unsafe update not refused: %v, %s", err, output)
	}
	current, err := f.client.runtime()
	if err != nil || current.ServerID != f.oldID {
		t.Fatalf("original running process was interrupted: %+v, %v", current, err)
	}
}
