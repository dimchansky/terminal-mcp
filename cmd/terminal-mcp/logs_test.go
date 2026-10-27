//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/mcpserver"
)

func isolatedLogHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestStableLogPathsAndDistinctStdoutStderr(t *testing.T) {
	home := isolatedLogHome(t)
	dir, err := defaultLogDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "terminal-mcp", "logs"); dir != want {
		t.Fatalf("logs location = %q, want %q", dir, want)
	}
	logs, err := openServerLogs()
	if err != nil {
		t.Fatal(err)
	}
	const stdout = "stdout payload only\n"
	const stderr = "stderr diagnostic only\n"
	if _, err := logs.stdout.WriteString(stdout); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.stderr.WriteString(stderr); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, want string }{
		{"server.stdout.log", stdout},
		{"server.stderr.log", stderr},
	} {
		path := filepath.Join(dir, tc.name)
		data, err := os.ReadFile(path)
		if err != nil || string(data) != tc.want {
			t.Fatalf("%s: got %q, want %q, err=%v", path, data, tc.want, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("diagnostics are not private: %s: %+v, %v", path, info, err)
		}
	}
	logs, err = openServerLogs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logs.stdout.WriteString("second stdout\n"); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "server.stdout.log"))
	if err != nil || !bytes.HasPrefix(data, []byte(stdout)) {
		t.Fatalf("reopening logs overwrote previous history: %s, %v", data, err)
	}
}

// The modern audit journal must never examine or pin the old controller's
// audit location, even if historical data and a broken migration marker exist.
func TestDirectHTTPDefaultIgnoresOldAuditHistory(t *testing.T) {
	_ = isolatedLogHome(t)
	source, err := legacyAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	target, err := defaultAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.PrepareDirectory(filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
	oldRecord := []byte("{\"old_controller\":true}\n")
	if err := os.WriteFile(source, oldRecord, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+".1", []byte("{\"old_archive\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := audit.PrepareDirectory(filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	marker := target + ".migrated.json"
	brokenMarker := []byte("not a valid migration marker")
	if err := os.WriteFile(marker, brokenMarker, 0600); err != nil {
		t.Fatal(err)
	}
	recorder, err := openAudit("", "http")
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Status().Path; !sameAuditPath(got, target) {
		t.Fatalf("default uses wrong audit location: %q", got)
	}
	// The old controller is not part of the new writer's lock domain.
	oldWriter, err := audit.Open(source)
	if err != nil {
		_ = recorder.Close()
		t.Fatalf("new default audit still locks old controller path: %v", err)
	}
	if err := oldWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recordServerStart(recorder, "modern-instance"); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(target)
	if err != nil || !bytes.Contains(current, []byte("modern-instance")) || bytes.Contains(current, oldRecord) {
		t.Fatalf("modern audit was merged with old history: %q %v", current, err)
	}
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, oldRecord) {
		t.Fatalf("old controller audit was modified: %q %v", got, err)
	}
	if got, err := os.ReadFile(marker); err != nil || !bytes.Equal(got, brokenMarker) {
		t.Fatalf("obsolete marker was read or changed: %q %v", got, err)
	}
	if _, err := os.Stat(target + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected archive copied from old controller: %v", err)
	}
}

func TestDirectHTTPDefaultNeverCreatesMigrationMarker(t *testing.T) {
	_ = isolatedLogHome(t)
	recorder, err := openAudit("", "http")
	if err != nil {
		t.Fatal(err)
	}
	target := recorder.Status().Path
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target + ".migrated.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new default created a migration marker: %v", err)
	}
}

func TestCustomAuditDoesNotTouchOldDefault(t *testing.T) {
	home := isolatedLogHome(t)
	source, err := legacyAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.PrepareDirectory(filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"old\":true}\n")
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(home, "custom", "audit.jsonl")
	recorder, err := openAudit(custom, "http")
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(custom + ".migrated.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom audit created a migration marker: %v", err)
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("custom audit changed old controller history: %q %v", got, err)
	}
}

func TestUpdateAuditPathPolicyAndRollback(t *testing.T) {
	_ = isolatedLogHome(t)
	legacy, err := legacyAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	unified, err := defaultAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Dir(unified)
	for _, tc := range []struct {
		name                string
		info                mcpserver.RuntimeInfo
		wantOld, wantTarget string
		migration           bool
	}{
		{"legacy published audit", mcpserver.RuntimeInfo{Audit: audit.Status{State: "ok", Path: legacy}},
			legacy, unified, true},
		{"older server no audit metadata", mcpserver.RuntimeInfo{Arguments: []string{"serve"}},
			legacy, unified, true},
		{"older explicit disabled", mcpserver.RuntimeInfo{Arguments: []string{"serve", "--audit-file=off"}},
			"off", "off", false},
		{"new default", mcpserver.RuntimeInfo{Audit: audit.Status{State: "ok", Path: unified}},
			unified, unified, false},
		{"custom", mcpserver.RuntimeInfo{Audit: audit.Status{State: "ok", Path: filepath.Join(workDir, "custom.jsonl")}},
			filepath.Join(workDir, "custom.jsonl"), filepath.Join(workDir, "custom.jsonl"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, target, migrating, err := updateAuditPaths(tc.info)
			if err != nil || old != tc.wantOld || target != tc.wantTarget || migrating != tc.migration {
				t.Fatalf("got %q %q %t %v; want %q %q %t",
					old, target, migrating, err, tc.wantOld, tc.wantTarget, tc.migration)
			}
		})
	}
	if got := rollbackAuditPath(legacy, unified, true); got != legacy {
		t.Fatalf("rollback before migration used %q, want old path", got)
	}
	if err := audit.PrepareDirectory(filepath.Dir(unified)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unified+".migrated.json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := rollbackAuditPath(legacy, unified, true); got != unified {
		t.Fatalf("rollback after migration used %q instead of unified path", got)
	}
	for _, tc := range []struct {
		info      mcpserver.RuntimeInfo
		expected  string
		wantError bool
	}{
		{mcpserver.RuntimeInfo{Audit: audit.Status{State: "ok", Path: unified}}, unified, false},
		{mcpserver.RuntimeInfo{Audit: audit.Status{State: "disabled"}}, "off", false},
		{mcpserver.RuntimeInfo{Audit: audit.Status{State: "ok", Path: legacy}}, unified, true},
		{mcpserver.RuntimeInfo{Audit: audit.Status{State: "degraded", Path: unified}}, unified, true},
		{mcpserver.RuntimeInfo{Arguments: []string{"serve", "--audit-file=" + unified}}, unified, false},
		{mcpserver.RuntimeInfo{Arguments: []string{"serve", "--audit-file=off"}}, unified, true},
	} {
		err := verifyRestartAudit(tc.info, tc.expected)
		if (err != nil) != tc.wantError {
			t.Fatalf("verification (%+v -> %q) error %v; wantError=%t", tc.info, tc.expected, err, tc.wantError)
		}
	}
}

func TestPrivateDiagnosticRefusesSymlinkAndPublicFile(t *testing.T) {
	dir := filepath.Join(isolatedLogHome(t), ".config", "terminal-mcp", "logs")
	if err := audit.PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "server.stdout.log")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := openServerLogs(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("diagnostic logs followed symlink: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "server.stdout.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.stderr.log"), []byte("world-readable"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "server.stderr.log"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := openServerLogs(); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("diagnostic logs accepted public permissions: %v", err)
	}
}
