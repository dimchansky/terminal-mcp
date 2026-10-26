package audit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func migrationTestPaths(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "legacy", "audit.jsonl")
	target := filepath.Join(root, "logs", "audit.jsonl")
	for _, dir := range []string{filepath.Dir(source), filepath.Dir(target)} {
		if err := PrepareDirectory(dir); err != nil {
			t.Fatal(err)
		}
		if err := checkAuditPrivateDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	return source, target
}

func writeAuditMigrationFixture(t *testing.T, legacy string) map[string][]byte {
	t.Helper()
	// Contents are syntactically valid JSONL, including every numbered
	// archive. The original source must never be modified by the copy.
	contents := map[string][]byte{
		"":   []byte("{\"old\":1}\n"),
		".1": []byte("{\"archive\":1}\n"),
		".2": []byte("{\"archive\":2}\n"),
		".3": []byte("{\"archive\":3}\n"),
		".5": []byte("{\"archive\":5}\n"),
	}
	for suffix, data := range contents {
		if err := os.WriteFile(legacy+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return contents
}

func TestMigrationCopiesAndPinsLegacyLockUntilShutdown(t *testing.T) {
	source, target := migrationTestPaths(t)
	original := writeAuditMigrationFixture(t, source)
	log, err := OpenWithMigration(target, source)
	if err != nil {
		t.Fatal(err)
	}
	if log.legacyLock == nil {
		t.Fatal("migration did not keep legacy writer fenced")
	}
	for suffix, data := range original {
		legacy, err := os.ReadFile(source + suffix)
		if err != nil || !bytes.Equal(legacy, data) {
			t.Fatalf("legacy source %q was modified: %q %v", suffix, legacy, err)
		}
		copied, err := os.ReadFile(target + suffix)
		if err != nil || !bytes.Equal(copied, data) {
			t.Fatalf("copied archive %q differs: %q %v", suffix, copied, err)
		}
	}
	if _, err := os.Stat(migrationMarker(target)); err != nil {
		t.Fatalf("migration marker missing: %v", err)
	}
	if other, err := Open(source); !errors.Is(err, ErrInUse) {
		if err == nil {
			_ = other.Close()
		}
		t.Fatalf("older server could write during new server lifetime: %v", err)
	}
	if other, err := Open(target); !errors.Is(err, ErrInUse) {
		if err == nil {
			_ = other.Close()
		}
		t.Fatalf("new destination allowed a second writer: %v", err)
	}
	if err := log.Start(NewEvent(Event{Tool: "server", Phase: "server_start"})); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening does not duplicate the family, reset the new events, or
	// corrupt the copied files.
	again, err := OpenWithMigration(target, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	for suffix, data := range original {
		if suffix == "" {
			continue // active log includes the additional server_start record
		}
		got, err := os.ReadFile(target + suffix)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("archive %q changed on repeat migration: %q %v", suffix, got, err)
		}
	}
	final, err := os.ReadFile(target)
	if err != nil || !bytes.HasPrefix(final, original[""]) || !bytes.Contains(final, []byte("server_start")) {
		t.Fatalf("repeated migration lost new records: %s %v", final, err)
	}
}

func TestMigrationRefusesActiveOldServerAndDestinationConflicts(t *testing.T) {
	source, target := migrationTestPaths(t)
	writeAuditMigrationFixture(t, source)
	old, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithMigration(target, source); !errors.Is(err, ErrInUse) {
		t.Fatalf("migrated while old server owned source lock: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".1", []byte("different independent audit history\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithMigration(target, source); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("overwrote independent destination audit: %v", err)
	}
	if _, err := os.Stat(migrationMarker(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial migration recorded success: %v", err)
	}
}

func TestMigrationResumesInterruptedCopyAndDetectsOldHistoryChanges(t *testing.T) {
	source, target := migrationTestPaths(t)
	contents := writeAuditMigrationFixture(t, source)
	// One archive was copied before the previous process crashed.
	if err := os.WriteFile(target+".1", contents[".1"], 0600); err != nil {
		t.Fatal(err)
	}
	log, err := OpenWithMigration(target, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// If an old-version binary later wrote to its legacy default (while
	// no new writer was active), do not accept silent split histories.
	f, err := os.OpenFile(source, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("{\"new_on_old_path\":true}\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithMigration(target, source); err == nil ||
		!strings.Contains(err.Error(), "changed after migration") {
		t.Fatalf("silent split history after old writer was not detected: %v", err)
	}
}

func TestMigrationRejectsSymlinkedArchive(t *testing.T) {
	source, target := migrationTestPaths(t)
	safe := filepath.Join(filepath.Dir(source), "unrelated")
	if err := os.WriteFile(safe, []byte("must not follow"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(safe, source+".1"); err != nil {
		t.Skipf("symlink creation unsupported: %v", err)
	}
	if _, err := OpenWithMigration(target, source); err == nil {
		t.Fatal("migration followed symlinked historical archive")
	}
}

func TestMigrationRejectsIndependentDestinationArchive(t *testing.T) {
	source, target := migrationTestPaths(t)
	writeAuditMigrationFixture(t, source)
	// An unrelated preexisting numbered archive must never be silently
	// merged into a migrated journal, even though none of the legacy
	// archive names conflict.
	extra := target + ".99"
	if err := os.WriteFile(extra, []byte("{\"independent_history\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithMigration(target, source); err == nil ||
		!strings.Contains(err.Error(), "unexpected destination history") {
		t.Fatalf("independent archive was silently retained: %v", err)
	}
	if data, err := os.ReadFile(extra); err != nil ||
		!bytes.Equal(data, []byte("{\"independent_history\":true}\n")) {
		t.Fatalf("conflicting destination archive was modified: %q %v", data, err)
	}
}

func TestMigrationRejectsMissingActiveJournalAfterMarker(t *testing.T) {
	source, target := migrationTestPaths(t)
	writeAuditMigrationFixture(t, source)
	journal, err := OpenWithMigration(target, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithMigration(target, source); err == nil ||
		!strings.Contains(err.Error(), "migrated active audit is missing") {
		t.Fatalf("migrated journal was silently recreated without history: %v", err)
	}
}

func TestMigrationWithoutLegacyDoesNotInventSourceHistory(t *testing.T) {
	source, target := migrationTestPaths(t)
	log, err := OpenWithMigration(target, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(migrationMarker(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("migration marker created without historical records: %v", err)
	}
}
