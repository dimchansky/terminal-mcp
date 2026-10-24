package audit

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditLockHelper(t *testing.T) {
	path := os.Getenv("AUDIT_LOCK_HELPER_PATH")
	if path == "" {
		return
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := os.WriteFile(os.Getenv("AUDIT_LOCK_HELPER_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

// Locking must work across processes, not merely between Go objects sharing
// an address space. The child holds the lock until the parent releases stdin.
func TestExclusiveOwnershipAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAuditLockHelper$")
	cmd.Env = append(os.Environ(), "AUDIT_LOCK_HELPER_PATH="+path, "AUDIT_LOCK_HELPER_READY="+ready)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(dir, "helper.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	cmd.Stdout, cmd.Stderr = output, output
	outputText := func() string { data, _ := os.ReadFile(output.Name()); return string(data) }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Cmd.Wait releases all process resources on every exit path.
	waitDone := make(chan error, 1)
	finished := make(chan struct{})
	go func() { waitDone <- cmd.Wait(); close(finished) }()
	defer func() {
		_ = stdin.Close()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Error("audit lock helper could not be reaped")
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-waitDone:
			t.Fatalf("helper exited before holding lock: %v, output: %s", err, outputText())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper never acquired audit lock: %s", outputText())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if other, err := Open(path); !errors.Is(err, ErrInUse) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("independent process bypassed lock: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("helper failed: %v %s", err, outputText())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not release lock")
	}
	third, err := Open(path)
	if err != nil {
		t.Fatalf("could not reacquire lock after subprocess exit: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}
