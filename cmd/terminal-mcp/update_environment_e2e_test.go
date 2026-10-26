//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitStandaloneUpdate(t *testing.T, state string) updateStatus {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		var latest struct {
			ID string `json:"id"`
		}
		if err := readJSON(filepath.Join(state, "latest.json"), &latest); err == nil && latest.ID != "" {
			var status updateStatus
			if err := readJSON(filepath.Join(state, latest.ID, "status.json"), &status); err == nil {
				switch status.Status {
				case "applied", "failed", "rolled_back":
					return status
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("updater did not reach a terminal status")
	return updateStatus{}
}

func TestStandaloneUpdaterPreservesExplicitEnvironmentAndStableCWD(t *testing.T) {
	home, binary := standaloneTestBinary(t)
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	address, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(home, "long-lived-project")
	updateDir := filepath.Join(home, "unrelated-update-cwd")
	for _, dir := range []string{workDir, updateDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	token := filepath.Join(home, ".config", "terminal-mcp", "token")
	t.Cleanup(func() {
		client, err := newUpdateClient(address, token)
		if err == nil {
			if info, err := client.runtime(); err == nil {
				_ = client.maintenance(info.ServerID, "begin")
				_ = client.maintenance(info.ServerID, "shutdown")
			}
		}
	})
	const variable = "TERMINAL_MCP_POLICY_EXPLICIT_TEST"
	startPath := "/usr/bin:/bin:/first-launch"
	updatePath := "/usr/bin:/bin:/second-launch"
	t.Setenv("PATH", startPath)
	initial, err := standaloneTestCommand(binary, home, "start",
		"--listen="+address, "--file-root="+home, "--audit-file=off",
		"--working-directory="+workDir, "--env="+variable+"=persistent-value")
	if err != nil {
		t.Fatalf("initial standalone start: %s %v", initial, err)
	}
	client, err := newUpdateClient(address, token)
	if err != nil {
		t.Fatal(err)
	}
	before, err := client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	canonicalDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if before.WorkingDirectory != canonicalDir ||
		remotePrintenv(t, client, before.ServerID, "pre-update-env", variable) != "persistent-value" ||
		remotePrintenv(t, client, before.ServerID, "pre-update-path", "PATH") != startPath {
		t.Fatalf("initial service environment/cwd incorrect: %+v", before)
	}

	candidate := filepath.Join(home, "candidate")
	build := exec.Command(goExecutable, "build", "-trimpath", "-ldflags=-buildid=policy-update-test", "-o", candidate, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build update candidate: %v\n%s", err, output)
	}

	t.Setenv("PATH", updatePath)
	t.Setenv(variable, "wrong-from-update-launcher")
	installed := filepath.Join(home, ".config", "terminal-mcp", "standalone", "terminal-mcp")
	update := exec.Command(installed, "update", "--candidate="+candidate,
		"--listen="+address, "--token-file="+token)
	update.Dir = updateDir
	update.Env = append(withoutEnvToken(os.Environ()), "HOME="+home)
	output, err := update.CombinedOutput()
	if err != nil {
		t.Fatalf("update scheduling failed: %s %v", output, err)
	}
	var queued struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &queued); err != nil || queued.Status != "queued" {
		t.Fatalf("unexpected update response: %s %v", output, err)
	}
	latest := waitStandaloneUpdate(t, filepath.Join(home, ".config", "terminal-mcp", "updates"))
	if latest.Status != "applied" {
		t.Fatalf("candidate did not start with explicitly configured environment: %+v", latest)
	}
	after, err := client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	if after.ServerID == before.ServerID || after.WorkingDirectory != canonicalDir ||
		after.Audit.State != "disabled" {
		t.Fatalf("update altered stable working directory or audit setting: %+v", after)
	}
	if got := remotePrintenv(t, client, after.ServerID, "post-update-env", variable); got != "persistent-value" {
		t.Fatalf("explicit env override lost during update, got %q", got)
	}
	if got := remotePrintenv(t, client, after.ServerID, "post-update-path", "PATH"); got != updatePath {
		t.Fatalf("updater reused old PATH snapshot: got %q, want new launcher PATH %q", got, updatePath)
	}
	if got := remotePrintenv(t, client, after.ServerID, "post-update-pwd", "PWD"); got != canonicalDir {
		t.Fatalf("updated process had inconsistent PWD %q vs cwd %q", got, canonicalDir)
	}
	// The private update manifest must not duplicate explicit environment
	// values. They were pinned in the independent worker's environment.
	var latestJob struct {
		ID string `json:"id"`
	}
	state := filepath.Join(home, ".config", "terminal-mcp", "updates")
	if err := readJSON(filepath.Join(state, "latest.json"), &latestJob); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(state, latestJob.ID, "manifest.json"))
	if err != nil || bytes.Contains(manifest, []byte("persistent-value")) ||
		bytes.Contains(manifest, []byte("wrong-from-update-launcher")) {
		t.Fatalf("update manifest leaked configured environment values: %s %v", manifest, err)
	}
	saved, err := savedStandaloneConfig(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	if err != nil || saved.EnvOverrides[variable] != "persistent-value" {
		t.Fatalf("explicit overrides were not kept in private standalone config: %+v %v", saved.EnvOverrides, err)
	}

	// Next simulate a candidate that passes preflight but fails *after*
	// activation on the live address. Rollback must preserve the stable
	// working directory, explicit overrides, and the second updater's PATH.
	goodHash, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	const marker = `if len(args) == 0 || args[0] != "serve" {`
	if !strings.Contains(string(source), marker) {
		t.Fatal("cannot instrument candidate's live-only startup failure")
	}
	injected := fmt.Sprintf("if opts.listen == %q { return fmt.Errorf(%q) }\n\t", address, "injected activation failure")
	updatedSource := strings.Replace(string(source), marker, injected+marker, 1)
	overlaySource := filepath.Join(home, "bad-start-main.go")
	if err := os.WriteFile(overlaySource, []byte(updatedSource), 0600); err != nil {
		t.Fatal(err)
	}
	mainPath, err := filepath.Abs("main.go")
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(home, "bad-start-overlay.json")
	if err := writeJSON(overlay, map[string]any{"Replace": map[string]string{mainPath: overlaySource}}); err != nil {
		t.Fatal(err)
	}
	badCandidate := filepath.Join(home, "bad-candidate")
	compileBad := exec.Command(goExecutable, "build", "-trimpath", "-overlay="+overlay,
		"-ldflags=-buildid=policy-bad-activation", "-o", badCandidate, ".")
	if result, err := compileBad.CombinedOutput(); err != nil {
		t.Fatalf("build fault-injected candidate: %v\\n%s", err, result)
	}
	rollbackPath := "/usr/bin:/bin:/rollback-launch"
	t.Setenv("PATH", rollbackPath)
	t.Setenv(variable, "wrong-from-rollback-launcher")
	rollback := exec.Command(installed, "update", "--candidate="+badCandidate,
		"--listen="+address, "--token-file="+token)
	rollback.Dir = updateDir
	rollback.Env = append(withoutEnvToken(os.Environ()), "HOME="+home)
	if result, err := rollback.CombinedOutput(); err != nil {
		t.Fatalf("queue rollback scenario: %v %s", err, result)
	}
	reverted := waitStandaloneUpdate(t, state)
	if reverted.Status != "rolled_back" {
		t.Fatalf("failed candidate was not safely rolled back: %+v", reverted)
	}
	recovered, err := client.runtime()
	if err != nil || recovered.ServerID == after.ServerID || recovered.WorkingDirectory != canonicalDir {
		t.Fatalf("rollback changed service cwd or could not restart: %+v %v", recovered, err)
	}
	restoredHash, err := fileSHA256(installed)
	if err != nil || restoredHash != goodHash {
		t.Fatalf("rollback did not restore the previous executable: got=%s want=%s err=%v",
			restoredHash, goodHash, err)
	}
	if got := remotePrintenv(t, client, recovered.ServerID, "post-rollback-env", variable); got != "persistent-value" {
		t.Fatalf("rollback lost explicit override: got %q", got)
	}
	if got := remotePrintenv(t, client, recovered.ServerID, "post-rollback-path", "PATH"); got != rollbackPath {
		t.Fatalf("rollback reused stale PATH: got %q want %q", got, rollbackPath)
	}
	if got := remotePrintenv(t, client, recovered.ServerID, "post-rollback-pwd", "PWD"); got != canonicalDir {
		t.Fatalf("rollback changed process PWD: got %q want %q", got, canonicalDir)
	}
}
