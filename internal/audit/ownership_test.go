package audit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExclusiveOwnership(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err == nil {
		_ = second.Close()
		_ = first.Close()
		t.Fatal("another writer acquired the same active audit path")
	}
	if !errors.Is(err, ErrInUse) {
		_ = first.Close()
		t.Fatalf("wanted ErrInUse from second Open, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Open(path)
	if err != nil {
		t.Fatalf("could not reacquire lock after Close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockAliasesResolveToSameOwner(t *testing.T) {
	dir := privateAuditDir(t)
	alias := filepath.Join(privateAuditDir(t), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(filepath.Join(alias, "events.jsonl")); !errors.Is(err, ErrInUse) {
		if second != nil {
			_ = second.Close()
		}
		_ = writer.Close()
		t.Fatalf("symlink alias bypassed same directory lock: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockHeldAcrossRotation(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "events.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	event := NewEvent(Event{Tool: "terminal_start", Phase: "start", Args: strings.Repeat("x", 2<<20)})
	for i := 0; i < 3; i++ {
		if err := writer.Write(event); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path + ".1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("log never rotated")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if second, err := Open(path); !errors.Is(err, ErrInUse) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second process could write during rotation: %v", err)
	}
}

func TestLockFileMustBePrivateAndNotSymlink(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "audit.jsonl")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path+".lock"); err != nil {
		t.Fatal(err)
	}
	if logger, err := Open(path); err == nil {
		_ = logger.Close()
		t.Fatal("followed symlink lock file")
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != "preserve" {
		t.Fatalf("lock file symlink target changed: %q, %v", contents, err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+".lock", 0644); err != nil {
		t.Fatal(err)
	}
	if logger, err := Open(path); err == nil {
		_ = logger.Close()
		t.Fatal("opened publicly writable/readable lock file")
	}
}
