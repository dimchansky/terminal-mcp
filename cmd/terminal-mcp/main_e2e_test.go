//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/session"
)

func TestRealCLIBinarySIGTERMAndAuditFlush(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI")
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	moduleRoot := filepath.Clean(filepath.Join(workingDir, "..", ".."))
	binary := filepath.Join(t.TempDir(), "terminal-mcp")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/terminal-mcp")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	// Exercise the real main entry point, not a testing-only imitation.
	help, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(help), "-listen") {
		t.Fatalf("real CLI --help failed: %v\n%s", err, help)
	}

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(stateDir, "token")
	if err := os.WriteFile(tokenFile, []byte(testToken), 0600); err != nil {
		t.Fatal(err)
	}
	auditFile := filepath.Join(stateDir, "audit.jsonl")
	stderrFile, err := os.Create(filepath.Join(stateDir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderrFile.Close() })
	cmd := exec.Command(binary,
		"--transport=http",
		"--listen=127.0.0.1:0",
		"--token-file="+tokenFile,
		"--file-root="+stateDir,
		"--audit-file="+auditFile,
	)
	cmd.Stderr = stderrFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		waited <- cmd.Wait()
		close(finished)
	}()

	var childPID int
	t.Cleanup(func() {
		select {
		case <-finished:
		default:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-finished:
			case <-time.After(4 * time.Second):
				if childPID > 1 {
					_ = syscall.Kill(-childPID, syscall.SIGKILL)
				}
				_ = cmd.Process.Kill()
				select {
				case <-finished:
				case <-time.After(2 * time.Second):
					t.Error("CLI subprocess could not be reaped")
				}
			}
		}
		if childPID > 1 {
			if err := syscall.Kill(childPID, 0); err == nil {
				_ = syscall.Kill(-childPID, syscall.SIGKILL)
				t.Errorf("managed child %d survived subprocess cleanup", childPID)
			} else if !errors.Is(err, syscall.ESRCH) {
				t.Errorf("could not check child %d: %v", childPID, err)
			}
		}
	})

	// The OS picks the port. Read the real address published by the binary
	// instead of allocating and releasing a guessed free port in the test.
	readyPattern := regexp.MustCompile("listening at http://(127[.]0[.]0[.]1:[0-9]+)/mcp")
	deadline := time.Now().Add(8 * time.Second)
	var address string
	for time.Now().Before(deadline) {
		lines, err := os.ReadFile(stderrFile.Name())
		if err != nil {
			t.Fatal(err)
		}
		if matches := readyPattern.FindSubmatch(lines); len(matches) == 2 {
			address = string(matches[1])
			break
		}
		select {
		case <-finished:
			t.Fatalf("real CLI exited before ready: %s", lines)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if address == "" {
		t.Fatal("real CLI did not publish its listening address")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	client := connectHTTP(t, ctx, address)
	tools, err := client.ListTools(ctx, nil)
	if err != nil || tools == nil || len(tools.Tools) != 12 {
		t.Fatalf("real CLI tool discovery: %v, %+v", err, tools)
	}
	list := mcpList(t, ctx, client)
	if list.ServerID == "" {
		t.Fatal("real CLI did not provide a server ID")
	}
	run := mcpCall[session.ActionResult](t, ctx, client, "terminal_start", session.StartArgs{
		ServerID: list.ServerID, OperationID: "real-sigterm-child",
		Argv: []string{"/bin/sleep", "60"}, Mode: "pipes", WaitMS: 0,
	})
	if run.Observation != nil {
		childPID = run.Observation.Session.PID // Also needed by test cleanup if assertions fail.
	}
	if run.Receipt.Status != "applied" || childPID <= 1 {
		t.Fatalf("real CLI could not start managed process: %+v", run)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close MCP client: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("real CLI failed on SIGTERM: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("real CLI failed to terminate after SIGTERM")
	}
	// The process has exited, so deferred audit.Close must have flushed every
	// accepted event. Verify the file is complete and that events correlate.
	contents, err := os.ReadFile(auditFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) == 0 || contents[len(contents)-1] != '\n' {
		t.Fatalf("audit was not flushed as complete JSONL (bytes=%d)", len(contents))
	}
	var started, toolStart, toolFinish int
	pairs := make(map[string][]audit.Event)
	for _, line := range strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n") {
		var event audit.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid audit record: %v", err)
		}
		if event.ServerID != list.ServerID || event.Time == "" {
			t.Fatalf("wrong audit instance/timestamp: %+v", event)
		}
		switch event.Phase {
		case "server_start":
			started++
		case "start":
			pairs[event.RequestID] = append(pairs[event.RequestID], event)
			if event.Tool == "terminal_start" {
				toolStart++
			}
		case "finish":
			pairs[event.RequestID] = append(pairs[event.RequestID], event)
			if event.Tool == "terminal_start" && event.SessionID == run.Receipt.SessionID && event.Status == "applied" {
				toolFinish++
			}
		default:
			t.Fatalf("unexpected audit phase: %+v", event)
		}
	}
	if started != 1 || toolStart != 1 || toolFinish != 1 {
		t.Fatalf("unexpected real CLI audit records: server_start=%d terminal_start=%d terminal_finish=%d", started, toolStart, toolFinish)
	}
	for id, pair := range pairs {
		if id == "" || len(pair) != 2 || pair[0].Phase != "start" || pair[1].Phase != "finish" ||
			pair[0].Tool != pair[1].Tool {
			t.Fatalf("unpaired audit event: request=%s events=%v", id, pair)
		}
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("managed process %d still exists after CLI SIGTERM: %v", childPID, err)
	}
}
