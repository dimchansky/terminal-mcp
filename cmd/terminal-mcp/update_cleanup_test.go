//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeCleanupFixture(t *testing.T, root, id, status string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "status.json"), updateStatus{ID: id, Status: status}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"candidate", "previous", "worker"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("small fake binary"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"manifest.json", "worker.log", "preflight.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("important evidence"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCleanupOnlyCompletedUpdateBinaries(t *testing.T) {
	home, bin := standaloneTestBinary(t)
	root := filepath.Join(home, "updates")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	appliedID := strings.Repeat("a", 32)
	rolledID := strings.Repeat("b", 32)
	failedID := strings.Repeat("c", 32)
	runningID := strings.Repeat("d", 32)
	applied := writeCleanupFixture(t, root, appliedID, "applied")
	rolled := writeCleanupFixture(t, root, rolledID, "rolled_back")
	failed := writeCleanupFixture(t, root, failedID, "failed")
	running := writeCleanupFixture(t, root, runningID, "preflight")
	legacyCandidate := filepath.Join(root, "candidate-previous-manual")
	if err := os.WriteFile(legacyCandidate, []byte("manually staged binary"), 0700); err != nil {
		t.Fatal(err)
	}
	rbDir := filepath.Join(home, "rollback")
	if err := os.Mkdir(rbDir, 0700); err != nil {
		t.Fatal(err)
	}
	rollback := filepath.Join(rbDir, "terminal-mcp")
	if err := os.WriteFile(rollback, []byte("last good version"), 0700); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) cleanupResult {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"cleanup", "--state-dir=" + root}, args...)...)
		cmd.Dir = home
		cmd.Env = append(withoutEnvToken(os.Environ()), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cleanup %v failed: %s %v", args, out, err)
		}
		var result cleanupResult
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("invalid cleanup JSON: %s %v", out, err)
		}
		return result
	}
	dry := run("--dry-run")
	if dry.Mode != "dry_run" || len(dry.Files) != 6 || dry.TotalBytes != 6*int64(len("small fake binary")) ||
		len(dry.SkippedJobs) != 2 {
		t.Fatalf("unexpected dry-run: %+v", dry)
	}
	for _, dir := range []string{applied, rolled, failed, running} {
		if _, err := os.Stat(filepath.Join(dir, "worker")); err != nil {
			t.Fatalf("dry run changed files: %s %v", dir, err)
		}
	}

	appliedRun := run("--apply")
	if appliedRun.Mode != "applied" || len(appliedRun.Files) != 6 {
		t.Fatalf("apply removed unexpected files: %+v", appliedRun)
	}
	for _, dir := range []string{applied, rolled} {
		for _, name := range []string{"candidate", "previous", "worker"} {
			if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed job still retains binary %s: %v", name, err)
			}
		}
		for _, name := range []string{"manifest.json", "worker.log", "preflight.log", "status.json"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("cleanup lost historical evidence %s: %v", name, err)
			}
		}
	}
	for _, dir := range []string{failed, running} {
		if _, err := os.Stat(filepath.Join(dir, "worker")); err != nil {
			t.Fatalf("cleanup destroyed recovery evidence for incomplete/failed job: %s %v", dir, err)
		}
	}
	for _, preserve := range []string{rollback, legacyCandidate} {
		if _, err := os.Stat(preserve); err != nil {
			t.Fatalf("cleanup removed unmanaged/rollback copy %s: %v", preserve, err)
		}
	}
	if again := run("--apply"); len(again.Files) != 0 {
		t.Fatalf("cleanup is not idempotent: %+v", again)
	}
}

func TestCleanupRefusesActiveUpdateAndUnexpectedSymlink(t *testing.T) {
	home, bin := standaloneTestBinary(t)
	root := filepath.Join(home, "updates")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir := writeCleanupFixture(t, root, strings.Repeat("e", 32), "applied")
	pending := filepath.Join(root, "pending")
	if err := os.Mkdir(pending, 0700); err != nil {
		t.Fatal(err)
	}
	run := func() ([]byte, error) {
		cmd := exec.Command(bin, "cleanup", "--apply", "--state-dir="+root)
		cmd.Dir = home
		cmd.Env = append(withoutEnvToken(os.Environ()), "HOME="+home)
		return cmd.CombinedOutput()
	}
	if out, err := run(); err == nil || !strings.Contains(string(out), "in progress") {
		t.Fatalf("cleanup ran with pending update: %q %v", out, err)
	}
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "protected-binary")
	if err := os.WriteFile(target, []byte("unrelated"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "worker")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "worker")); err != nil {
		t.Skip(err)
	}
	if out, err := run(); err == nil || !strings.Contains(string(out), "nonregular") {
		t.Fatalf("cleanup followed symlinked job file: %q %v", out, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "unrelated" {
		t.Fatalf("cleanup changed symlink destination: %s %v", got, err)
	}
	// Failure must be detected before unlinking any sibling files.
	if _, err := os.Stat(filepath.Join(dir, "candidate")); err != nil {
		t.Fatalf("cleanup partially removed artifacts before rejecting symlink: %v", err)
	}
}

func TestSingleRollbackSlotAtomicallyReplacesPriorVersion(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "updates")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(home, "original")
	for i, contents := range []string{"old version", "newer last known good"} {
		if err := os.WriteFile(previous, []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
		sha, err := fileSHA256(previous)
		if err != nil {
			t.Fatal(err)
		}
		path, err := saveRollbackSlot(root, previous, sha)
		if err != nil {
			t.Fatalf("unable to store rollback version %d: %v", i, err)
		}
		if path != filepath.Join(home, "rollback", "terminal-mcp") {
			t.Fatalf("rollback version stored at surprising path: %s", path)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != contents {
			t.Fatalf("rollback version %d not atomically replaced: %q %v", i, got, err)
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil || len(entries) != 1 {
			t.Fatalf("rollback creates more than one long-lived executable: %+v %v", entries, err)
		}
	}
}
