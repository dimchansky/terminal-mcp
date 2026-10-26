//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"terminalmcp/internal/audit"
)

func legacyAuditInTestHome(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP", "audit.jsonl")
	}
	return filepath.Join(home, ".local", "state", "terminal-mcp", "audit.jsonl")
}

func TestUpdaterMigratesLegacyAuditAndKeepsStableDiagnosticLogs(t *testing.T) {
	const oldArchive = "{\"historical_archive\":true}\n"
	var legacyPath string
	f := newUpdateFixtureWithAudit(t, false, func(home string) string {
		legacyPath = legacyAuditInTestHome(home)
		if err := os.MkdirAll(filepath.Dir(legacyPath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(legacyPath+".1", []byte(oldArchive), 0600); err != nil {
			t.Fatal(err)
		}
		return legacyPath
	})
	original, err := os.ReadFile(legacyPath)
	if err != nil || !bytes.Contains(original, []byte("server_start")) {
		t.Fatalf("initial legacy audit missing: %s %v", original, err)
	}
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-buildid=migrate-e2e-next", "-o", f.candidate, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build update candidate: %v\n%s", err, output)
	}
	f.update(f.candidate)
	result := f.waitResult()
	if result.Status != "applied" {
		t.Fatalf("update failed instead of migrating legacy audit: %+v", result)
	}
	info, err := f.client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.home, ".config", "terminal-mcp", "logs", "audit.jsonl")
	if info.Audit.State != "ok" || !sameAuditPath(info.Audit.Path, target) {
		t.Fatalf("activated server did not use unified audit journal: %+v", info.Audit)
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.HasPrefix(after, original) {
		t.Fatalf("migration lost the old active records: %q %v", after, err)
	}
	sourceStill, err := os.ReadFile(legacyPath)
	if err != nil || !bytes.HasPrefix(sourceStill, original) {
		t.Fatalf("migration removed original historical records: %q %v", sourceStill, err)
	}
	// The updater's fingerprint/preflight calls finish on the old server
	// before its shutdown, so migration must include those final events too.
	if !bytes.HasPrefix(after, sourceStill) {
		t.Fatalf("migration missed the final old-server events: new=%q old=%q", after, sourceStill)
	}
	for _, path := range []string{legacyPath + ".1", target + ".1"} {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != oldArchive {
			t.Fatalf("migration lost archived history %s: %q %v", path, b, err)
		}
	}
	if oldWriter, err := audit.Open(legacyPath); !errors.Is(err, audit.ErrInUse) {
		if err == nil {
			_ = oldWriter.Close()
		}
		t.Fatalf("legacy audit lock not held throughout new server lifetime: %v", err)
	}
	for _, filename := range []string{"server.stdout.log", "server.stderr.log"} {
		path := filepath.Join(filepath.Dir(target), filename)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("missing/private stable log %s: %+v %v", path, info, err)
		}
	}
	stderr, err := os.ReadFile(filepath.Join(filepath.Dir(target), "server.stderr.log"))
	if err != nil || !strings.Contains(string(stderr), "listening at http://") {
		t.Fatalf("candidate stderr was not redirected to stable diagnostics: %s %v", stderr, err)
	}
	var latest struct {
		ID string `json:"id"`
	}
	if err := readJSON(filepath.Join(f.state, "latest.json"), &latest); err != nil {
		t.Fatal(err)
	}
	job := filepath.Join(f.state, latest.ID)
	for _, expected := range []string{"worker.log", "preflight.log"} {
		if _, err := os.Stat(filepath.Join(job, expected)); err != nil {
			t.Fatalf("per-job worker/preflight evidence missing: %s: %v", expected, err)
		}
	}
	for _, oldName := range []string{"server.log", "rollback.log"} {
		if _, err := os.Stat(filepath.Join(job, oldName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("server diagnostics unexpectedly stored in update job: %s: %v", oldName, err)
		}
	}
}
