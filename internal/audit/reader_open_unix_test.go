//go:build darwin || linux

package audit

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAuditReaderUnixIsReadOnlyPrivateAndNoFollow(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := l.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); err != nil {
		t.Fatal(err)
	}
	reader, err := openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var first [1]byte
	if _, err := reader.ReadAt(first[:], 0); err != nil || first[0] != '{' {
		t.Fatalf("cannot read original journal: %q, %v", first, err)
	}
	if _, err := reader.Write([]byte("alter the audit")); err == nil {
		t.Fatal("reader opened the journal for writing")
	}

	link := filepath.Join(dir, "linked-audit.jsonl")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openAuditReader(link); err == nil {
		_ = f.Close()
		t.Fatal("audit reader followed symlink")
	}
	special := filepath.Join(dir, "directory.jsonl")
	if err := os.Mkdir(special, 0700); err != nil {
		t.Fatal(err)
	}
	if f, err := openAuditReader(special); err == nil {
		_ = f.Close()
		t.Fatal("audit reader accepted a directory")
	}
	fifo := filepath.Join(dir, "fifo.jsonl")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	// Opening a FIFO without O_NONBLOCK would block this test forever.
	if f, err := openAuditReader(fifo); err == nil {
		_ = f.Close()
		t.Fatal("audit reader accepted FIFO")
	}
	exposed := filepath.Join(dir, "public.jsonl")
	if err := os.WriteFile(exposed, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// The test runner may have a restrictive umask; force a public mode.
	if err := os.Chmod(exposed, 0644); err != nil {
		t.Fatal(err)
	}
	if f, err := openAuditReader(exposed); err == nil {
		_ = f.Close()
		t.Fatal("audit reader accepted public file")
	}
	if err := os.Chmod(exposed, 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := openAuditReader(exposed); err != nil {
		t.Fatal(err)
	} else if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
