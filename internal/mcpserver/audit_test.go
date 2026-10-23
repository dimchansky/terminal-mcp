//go:build linux || darwin

package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"terminalmcp/internal/audit"
	"terminalmcp/internal/session"
)

func TestAuditToolCallsWithCommandArguments(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	writer, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := session.New(nil)
	defer manager.Close()
	one, two := mcp.NewInMemoryTransports()
	server, err := NewWithAudit(manager, writer).Connect(context.Background(), two, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "audit-test", Version: "1"}, nil).Connect(context.Background(), one, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	list := call[session.ListResult](t, context.Background(), client, "terminal_list", struct{}{})
	const commandArgument = "full-argv-is-audited"
	run := call[session.ActionResult](t, context.Background(), client, "terminal_start", session.StartArgs{
		ServerID: list.ServerID, OperationID: "audit-test-1",
		Argv:        []string{"/bin/sh", "-c", "printf " + commandArgument},
		CloseOnExit: true, WaitFor: "completion", WaitMS: 3000,
	})
	if run.Receipt.Status != "applied" {
		t.Fatal(run)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), commandArgument) || !strings.Contains(string(data), "audit-test-1") {
		t.Fatalf("command arguments missing from audit log: %s", data)
	}
	events := map[string][]audit.Event{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event audit.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events[event.RequestID] = append(events[event.RequestID], event)
	}
	if len(events) != 2 {
		t.Fatalf("expected two audited tool calls: %s", data)
	}
	for _, pair := range events {
		if len(pair) != 2 || pair[0].Phase != "start" || pair[1].Phase != "finish" || pair[0].Tool != pair[1].Tool {
			t.Fatalf("bad audit pair: %+v", pair)
		}
		if pair[0].Tool == "terminal_start" {
			args, ok := pair[0].Args.(map[string]any)
			if !ok || args["operation_id"] != "audit-test-1" || pair[1].SessionID != run.Receipt.SessionID {
				t.Fatalf("missing command/session context: %+v", pair)
			}
			argv, ok := args["argv"].([]any)
			if !ok || len(argv) != 3 || argv[2] != "printf "+commandArgument {
				t.Fatalf("command arguments changed: %+v", args)
			}
		}
	}
}

func TestAuditUnavailableDoesNotPreventToolExecution(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	writer, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := session.New(nil)
	defer manager.Close()
	one, two := mcp.NewInMemoryTransports()
	server, err := NewWithAudit(manager, writer).Connect(context.Background(), two, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "audit-test", Version: "1"}, nil).Connect(context.Background(), one, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	list := call[session.ListResult](t, context.Background(), client, "terminal_list", struct{}{})
	started := call[session.ActionResult](t, context.Background(), client, "terminal_start", session.StartArgs{
		ServerID: list.ServerID, OperationID: "audit-unavailable-run", Argv: []string{"/usr/bin/true"},
		CloseOnExit: true, WaitFor: "completion", WaitMS: 2000,
	})
	if started.Receipt.Status != "applied" {
		t.Fatalf("audit failure prevented execution: %+v", started)
	}
}
