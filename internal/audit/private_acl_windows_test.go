//go:build windows

package audit

import (
	"errors"
	"strings"

	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// makePublicDACL deliberately broadens a test fixture's DACL. Production
// directories and journals must reject the resulting permissions.
func makePublicDACL(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsRefusesPublicAuditDirectoryACL(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatalf("private baseline failed: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	makePublicDACL(t, dir)
	if log, err := Open(path); err == nil {
		_ = log.Close()
		t.Fatal("opened audit journal in publicly accessible directory")
	} else if !strings.Contains(err.Error(), "grants access to another identity") {
		t.Fatalf("rejection not caused by the deliberately broad DACL: %v", err)
	}
}

func TestWindowsRefusesPublicAuditLockACL(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "audit.jsonl")
	lockPath := path + ".lock"
	log, err := Open(path)
	if err != nil {
		t.Fatalf("private baseline failed: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	makePublicDACL(t, lockPath)
	if log, err := Open(path); err == nil {
		_ = log.Close()
		t.Fatal("opened audit journal with publicly accessible lock file")
	} else if !strings.Contains(err.Error(), "grants access to another identity") {
		t.Fatalf("rejection not caused by the deliberately broad lock DACL: %v", err)
	}
}

// The single-writer lock protects cooperating loggers. If a noncooperating
// process writes directly to the journal, do not overwrite or truncate bytes
// that weren't accounted for by this server's audit writer.
func TestWindowsRefusesOutOfBandAuditAppend(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "audit.jsonl")
	sink, err := openFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sink.close(); err != nil {
			t.Error(err)
		}
	}()
	rogue, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"external\":true}\n")
	if _, err := rogue.Write(original); err != nil {
		_ = rogue.Close()
		t.Fatal(err)
	}
	if err := rogue.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.appendLine([]byte("{\"ours\":true}\n")); !errors.Is(err, ErrJournalChanged) {
		t.Fatalf("external append was not detected: %v", err)
	}
	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != string(original) {
		t.Fatalf("external append overwritten: %q", result)
	}
}

// Rotation must refuse external journal bytes before archiving, preserving
// evidence and leaving the active journal under its existing writer lock.
func TestWindowsRejectsOutOfBandJournalChangeBeforeRotation(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	sink, err := openFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sink.close(); err != nil {
			t.Error(err)
		}
	}()
	if err := sink.appendLine(almostFullLogLine()); err != nil {
		t.Fatal(err)
	}
	rogue, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	external := []byte("external\n")
	if _, err := rogue.Write(external); err != nil {
		_ = rogue.Close()
		t.Fatal(err)
	}
	if err := rogue.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.appendLine([]byte("our larger line to force an archive rotation\n")); !errors.Is(err, ErrJournalChanged) {
		t.Fatalf("rotation failed to reject journal change: %v", err)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive unexpectedly created from unaccounted bytes: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != sink.size+int64(len(external)) {
		t.Fatalf("unexpectedly altered journal length: on disk %d tracked %d", info.Size(), sink.size)
	}
}
