//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestUpdaterPrunesCompletedPayloadsAndMaintainsSingleRollbackSlot(t *testing.T) {
	f := newUpdateFixture(t, false)
	rollbackSlot := filepath.Join(f.home, "rollback", "terminal-mcp")
	before, err := fileSHA256(f.target)
	if err != nil {
		t.Fatal(err)
	}
	for step := range 2 {
		candidateID := "cleanup-upgrade-one"
		if step != 0 {
			candidateID = "cleanup-upgrade-two"
		}
		build := exec.Command("go", "build", "-trimpath", "-ldflags=-buildid="+candidateID,
			"-o", f.candidate, ".")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build distinct candidate %d: %v\n%s", step, err, output)
		}
		expectedCandidate, err := fileSHA256(f.candidate)
		if err != nil || expectedCandidate == before {
			t.Fatalf("candidate %d is identical to installed binary: %v", step, err)
		}
		f.update(f.candidate)
		result := f.waitResult()
		if result.Status != "applied" {
			t.Fatalf("update %d did not apply: %+v", step, result)
		}
		// The worker writes the final status immediately before deleting
		// transient executables, then removes the pending marker on exit.
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Lstat(filepath.Join(f.state, "pending")); errors.Is(err, os.ErrNotExist) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := os.Lstat(filepath.Join(f.state, "pending")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("update worker left a pending marker after completion")
		}
		var latest struct {
			ID string `json:"id"`
		}
		if err := readJSON(filepath.Join(f.state, "latest.json"), &latest); err != nil {
			t.Fatal(err)
		}
		job := filepath.Join(f.state, latest.ID)
		for _, file := range []string{"candidate", "previous", "worker"} {
			if _, err := os.Lstat(filepath.Join(job, file)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("update %d retained temporary binary %s: %v", step, file, err)
			}
		}
		for _, file := range []string{"manifest.json", "worker.log", "preflight.log", "status.json"} {
			if _, err := os.Stat(filepath.Join(job, file)); err != nil {
				t.Fatalf("update %d lost diagnostic evidence %s: %v", step, file, err)
			}
		}
		storedHash, err := fileSHA256(rollbackSlot)
		if err != nil || storedHash != before {
			t.Fatalf("update %d did not preserve verified last known good: got=%q want=%q err=%v",
				step, storedHash, before, err)
		}
		after, err := fileSHA256(f.target)
		if err != nil || after != expectedCandidate {
			t.Fatalf("update %d running binary unexpected: got=%q want=%q err=%v", step, after, expectedCandidate, err)
		}
		before = after
	}
	slots, err := os.ReadDir(filepath.Dir(rollbackSlot))
	if err != nil || len(slots) != 1 || slots[0].Name() != "terminal-mcp" {
		t.Fatalf("rollback slot accumulated multiple executables: %+v %v", slots, err)
	}
}
