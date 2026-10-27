//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"terminalmcp/internal/audit"
)

func currentAuditInTestHome(home string) string {
	return filepath.Join(home, ".config", "terminal-mcp", "logs", "audit.jsonl")
}

func TestUpdaterRetainsAuditAndStableDiagnosticLogs(t *testing.T) {
	const archived = "{\"historical_archive\":true}\n"
	var journal string
	f := newUpdateFixtureWithAudit(t, false, func(home string) string {
		journal = currentAuditInTestHome(home)
		if err := audit.PrepareDirectory(filepath.Dir(journal)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(journal+".1", []byte(archived), 0600); err != nil {
			t.Fatal(err)
		}
		return journal
	})
	original, err := os.ReadFile(journal)
	if err != nil || !bytes.Contains(original, []byte("server_start")) {
		t.Fatalf("initial audit missing: %q %v", original, err)
	}
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-buildid=audit-e2e-next", "-o", f.candidate, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build update candidate: %v\n%s", err, output)
	}
	f.update(f.candidate)
	result := f.waitResult()
	if result.Status != "applied" {
		t.Fatalf("update failed to preserve journal: %+v", result)
	}
	info, err := f.client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	if info.Audit.State != "ok" || !sameAuditPath(info.Audit.Path, journal) {
		t.Fatalf("activated server did not keep audit path: %+v", info.Audit)
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.HasPrefix(after, original) ||
		bytes.Count(after, []byte("\"phase\":\"server_start\"")) < 2 {
		t.Fatalf("update lost existing records or new server_start: %q %v", after, err)
	}
	gotArchive, err := os.ReadFile(journal + ".1")
	if err != nil || string(gotArchive) != archived {
		t.Fatalf("updater altered numbered archive: %q %v", gotArchive, err)
	}
	if writer, err := audit.Open(journal); !errors.Is(err, audit.ErrInUse) {
		if err == nil {
			_ = writer.Close()
		}
		t.Fatalf("running server did not retain the active audit lock: %v", err)
	}
	if _, err := os.Stat(journal + ".migrated.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("modern update created a migration marker: %v", err)
	}
	for _, filename := range []string{"server.stdout.log", "server.stderr.log"} {
		path := filepath.Join(filepath.Dir(journal), filename)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("missing/private stable log %s: %+v %v", path, info, err)
		}
	}
	stderr, err := os.ReadFile(filepath.Join(filepath.Dir(journal), "server.stderr.log"))
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
