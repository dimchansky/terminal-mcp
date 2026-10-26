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

func TestDirectHTTPDefaultMigratesColdLegacyAudit(t *testing.T) {
	_ = isolatedLogHome(t)
	source, err := legacyAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	target, err := defaultAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	if source == target {
		t.Fatal("old and new paths are identical")
	}
	if err := audit.PrepareDirectory(filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
	oldRecord := []byte("{\"historical\":true}\n")
	if err := os.WriteFile(source, oldRecord, 0600); err != nil {
		t.Fatal(err)
	}
	archive := []byte("{\"older\":true}\n")
	if err := os.WriteFile(source+".1", archive, 0600); err != nil {
		t.Fatal(err)
	}
	recorder, err := openAudit("", "http")
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Status().Path; !sameAuditPath(got, target) {
		t.Fatalf("default uses wrong audit location: %q", got)
	}
	if _, err := audit.Open(source); !errors.Is(err, audit.ErrInUse) {
		t.Fatalf("old server could write during new server migration: %v", err)
	}
	if err := recordServerStart(recorder, "test-instance"); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(original, oldRecord) {
		t.Fatalf("cold migration destroyed source history: %q %v", original, err)
	}
	copied, err := os.ReadFile(target)
	if err != nil || !bytes.HasPrefix(copied, oldRecord) || !bytes.Contains(copied, []byte("server_start")) {
		t.Fatalf("cold migration lost active history: %q %v", copied, err)
	}
	oldArchive, err := os.ReadFile(source + ".1")
	if err != nil || !bytes.Equal(oldArchive, archive) {
		t.Fatalf("cold migration modified source archive: %q %v", oldArchive, err)
	}
	newArchive, err := os.ReadFile(target + ".1")
	if err != nil || !bytes.Equal(newArchive, archive) {
		t.Fatalf("cold migration did not preserve numbered archive: %q %v", newArchive, err)
	}
	if _, err := os.Stat(target + ".migrated.json"); err != nil {
		t.Fatalf("migration manifest missing: %v", err)
	}
}

func TestExplicitCustomAuditDoesNotMigrateLegacyDefault(t *testing.T) {
	home := isolatedLogHome(t)
	source, err := legacyAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.PrepareDirectory(filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("{\"old\":true}\n"), 0600); err != nil {
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
		t.Fatalf("explicit custom path was accidentally migrated: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source history was lost: %v", err)
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
