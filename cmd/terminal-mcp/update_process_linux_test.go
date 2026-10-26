//go:build linux

package main

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestWaitForExitRecognizesUnreapedLinuxZombie(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	// The test process owns the child and does not call Wait until cleanup,
	// guaranteeing that the exited child remains a zombie for this check.
	deadline := time.Now().Add(5 * time.Second)
	for !processZombie(cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !processZombie(cmd.Process.Pid) {
		t.Fatal("test child never became a zombie")
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("Linux zombie unexpectedly returned ESRCH: %v", err)
	}
	if err := waitForExit(cmd.Process.Pid, 300*time.Millisecond); err != nil {
		t.Fatalf("waitForExit mistook a zombie for a running server: %v", err)
	}
}
