//go:build windows

package audit

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// This must execute on a NATIVE Windows runner. Cross-compilation cannot
// prove that a read handle permits the writer to rename a full active journal.
func TestWindowsAuditReaderHeldAcrossRotationAndRepair(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	recorder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recorder.Close() }()
	nearlyFillObservedLog(t, recorder)

	reader, err := openAuditReader(path)
	if err != nil {
		t.Fatalf("reader could not open writer's active journal: %v", err)
	}
	defer func() { _ = reader.Close() }()
	before, err := reader.Stat()
	if err != nil {
		t.Fatal(err)
	}

	// The original read handle stays OPEN during the real os.Rename inside
	// rotate(); no mocks, sleeps, or t.Skip.
	next := NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})
	if err := recorder.Start(next); err != nil {
		t.Fatalf("held read handle blocked Windows rename or new admission: %v", err)
	}
	archived, err := os.Open(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	archivedInfo, err := archived.Stat()
	_ = archived.Close()
	if err != nil || !os.SameFile(before, archivedInfo) {
		t.Fatalf("held reader was not attached to rotated archived file: %v", err)
	}
	// The old handle still reads original evidence after the rename.
	var first [1]byte
	if _, err := reader.ReadAt(first[:], 0); err != nil || first[0] != '{' {
		t.Fatalf("held reader cannot read archived record: byte=%q error=%v", first, err)
	}

	// Keep a second read descriptor open while the failed partial append is
	// truncated. Sharing WRITE permits opening the reader beside the writer;
	// sharing DELETE is proven separately by rotation above. No memory mapping.
	activeReader, err := openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = activeReader.Close() }()
	s := recorder.sink.(*fileSink)
	file := s.f
	failed := errors.New("injected partial append failure")
	s.f = &faultFile{auditFile: file,
		writeFn: func(data []byte) (int, error) {
			n, err := file.Write(data[:min(len(data), 5)])
			if err != nil {
				return n, err
			}
			return n, errors.Join(io.ErrShortWrite, failed)
		},
	}
	if err := recorder.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start"})); !errors.Is(err, failed) {
		t.Fatalf("test did not exercise partial append recovery: %v", err)
	}
	if s.repair {
		t.Fatal("successful immediate truncate unexpectedly left repair pending")
	}
	s.f = file
	if err := recorder.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatalf("held reader blocked repair/retry: %v", err)
	}
	if state := recorder.Status(); state.State != "ok" {
		t.Fatalf("reader broke audit health: %+v", state)
	}
	info, err := activeReader.Stat()
	if err != nil || info.Size() != s.size {
		t.Fatalf("repair left incomplete audit bytes: file %v tracked %d error %v", info, s.size, err)
	}
	t.Log("native Windows reader with FILE_SHARE_DELETE allowed rename, recovery and admission")
}

func TestWindowsAuditReaderRejectsPublicFilesAndSpecialPaths(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "readable.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("cannot modify")); err == nil {
		t.Fatal("Windows audit reader unexpectedly writable")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	makePublicDACL(t, path)
	if f, err := openAuditReader(path); err == nil {
		_ = f.Close()
		t.Fatal("reader accepted public ACL on the opened file handle")
	} else if !strings.Contains(err.Error(), "grants access to another identity") {
		t.Fatalf("public ACL rejection did not come from handle DACL: %v", err)
	}
	subdir := filepath.Join(dir, "subdirectory")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	if f, err := openAuditReader(subdir); err == nil {
		_ = f.Close()
		t.Fatal("reader accepted a directory")
	} else if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("directory rejection had unexpected cause: %v", err)
	}
	// Only the optional symlink fixture depends on Windows developer privileges.
	target := filepath.Join(dir, "private-target.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.jsonl")
	if err := os.Symlink(target, link); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI must support symlink negative test: %v", err)
		}
		t.Logf("symlink fixture unavailable: %v", err)
	} else if f, err := openAuditReader(link); err == nil {
		_ = f.Close()
		t.Fatal("reader followed a Windows symlink/reparse point")
	} else if !strings.Contains(err.Error(), "refuses reparse points") {
		t.Fatalf("symlink rejection had unexpected cause: %v", err)
	}
}
