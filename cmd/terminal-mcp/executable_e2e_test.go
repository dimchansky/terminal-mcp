//go:build linux || darwin

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadedCLIStartIsInstallOnceAndStatusUsesRunningRevision(t *testing.T) {
	home, original := standaloneTestBinary(t)
	address, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(home, ".config", "terminal-mcp", "standalone", "terminal-mcp")
	t.Cleanup(func() { _, _ = standaloneTestCommand(original, home, "stop") })
	first, err := standaloneTestCommand(original, home, "start",
		"--listen="+address, "--file-root="+home, "--audit-file=off")
	if err != nil {
		t.Fatalf("first downloaded executable could not install: %v %s", err, first)
	}
	var started struct {
		InstalledNew bool   `json:"installed_new"`
		Executable   string `json:"executable"`
	}
	if err := json.Unmarshal(first, &started); err != nil || !started.InstalledNew ||
		!sameAuditPath(started.Executable, installed) {
		t.Fatalf("initial start did not show installation location: %+v %s %v", started, first, err)
	}
	currentHash, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}
	assertRunningStatus := func(binary string) {
		t.Helper()
		status, err := standaloneTestCommand(binary, home, "status")
		if err != nil {
			t.Fatalf("status from %q failed: %s %v", binary, status, err)
		}
		var decoded struct {
			Status           string `json:"status"`
			Executable       string `json:"executable"`
			ExecutableSHA256 string `json:"executable_sha256"`
			Revision         string `json:"revision"`
		}
		if err := json.Unmarshal(status, &decoded); err != nil ||
			decoded.Status != "running" || !sameAuditPath(decoded.Executable, installed) ||
			decoded.ExecutableSHA256 != currentHash {
			t.Fatalf("status failed to identify actually running binary: %+v %s %v", decoded, status, err)
		}
	}
	assertRunningStatus(original)
	if out, err := standaloneTestCommand(original, home, "stop"); err != nil {
		t.Fatalf("initial stop: %s %v", out, err)
	}
	status, err := standaloneTestCommand(original, home, "status")
	if err != nil || !strings.Contains(string(status), currentHash) {
		t.Fatalf("stopped status forgot installed executable identity: %q %v", status, err)
	}
	different := filepath.Join(home, "different-downloaded-build")
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-buildid=different-downloaded-version",
		"-o", different, ".")
	if result, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build different downloaded version: %v %s", err, result)
	}
	if newHash, err := fileSHA256(different); err != nil || newHash == currentHash {
		t.Fatalf("test failed to build different binary: %q %v", newHash, err)
	}
	denied, err := standaloneTestCommand(different, home, "start")
	if err == nil || !strings.Contains(string(denied), "installed_version_mismatch") {
		t.Fatalf("different downloaded version silently replaced installation: %s %v", denied, err)
	}
	if after, err := fileSHA256(installed); err != nil || after != currentHash {
		t.Fatalf("start modified installed binary: %s %v", after, err)
	}
	// status/stop are client commands: even a different downloaded version
	// must be able to manage the known instance without installing itself.
	restarted, err := standaloneTestCommand(original, home, "start")
	if err != nil || !strings.Contains(string(restarted), `"installed_new":false`) {
		t.Fatalf("known version could not safely restart: %s %v", restarted, err)
	}
	assertRunningStatus(different)
	if out, err := standaloneTestCommand(different, home, "stop"); err != nil ||
		!strings.Contains(string(out), `"status":"stopped"`) {
		t.Fatalf("downloaded binary could not stop installed server: %s %v", out, err)
	}
}

func TestStartRefusesReinstallFromSymlinkOrWrongVersionWithoutLaunching(t *testing.T) {
	home := t.TempDir()
	original := filepath.Join(home, "binary")
	target := filepath.Join(home, "installed")
	if err := os.WriteFile(original, []byte("a"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("b"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := installIfMissing(original, target, 0700); err == nil {
		t.Fatal("unexpected upgrade during start")
	}
}
