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
	defer func() { _ = server.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "audit-test", Version: "1"}, nil).Connect(context.Background(), one, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
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
	for id, pair := range events {
		if id == "" || pair[0].Phase != "request_start" || pair[len(pair)-1].Phase != "request_finish" ||
			pair[0].Tool != pair[len(pair)-1].Tool {
			t.Fatalf("bad request lifecycle: %+v", pair)
		}
		for _, event := range pair {
			if event.SchemaVersion != audit.EventSchemaVersion {
				t.Fatalf("missing audit schema version: %+v", event)
			}
		}
		if pair[0].Tool == "terminal_list" {
			if len(pair) != 2 {
				t.Fatalf("read-only tool created operation events: %+v", pair)
			}
			continue
		}
		if pair[0].Tool != "terminal_start" || len(pair) != 4 ||
			pair[1].Phase != "operation_start" || pair[2].Phase != "operation_finish" {
			t.Fatalf("mutation lifecycle is not correlated: %+v", pair)
		}
		args, ok := pair[0].Args.(map[string]any)
		if !ok || args["operation_id"] != "audit-test-1" || pair[3].SessionID != run.Receipt.SessionID {
			t.Fatalf("missing command/session context: %+v", pair)
		}
		argv, ok := args["argv"].([]any)
		if !ok || len(argv) != 3 || argv[2] != "printf "+commandArgument {
			t.Fatalf("command arguments changed: %+v", args)
		}
		for _, e := range pair {
			if e.OperationID != "audit-test-1" {
				t.Fatalf("lost operation correlation: %+v", pair)
			}
		}
		if pair[2].Status != "applied" || pair[2].SessionID != run.Receipt.SessionID {
			t.Fatalf("operation_finish does not describe successful spawn: %+v", pair[2])
		}
		receipt, ok := pair[2].Receipt.(map[string]any)
		if !ok || receipt["status"] != "applied" || receipt["operation_id"] != "audit-test-1" {
			t.Fatalf("operation receipt missing: %+v", pair[2])
		}
	}
}

func TestAuditUnavailableRefusesToolExecution(t *testing.T) {
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
	defer func() { _ = server.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "audit-test", Version: "1"}, nil).Connect(context.Background(), one, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args any
	}{
		{"terminal_list", struct{}{}},
		{"terminal_start", session.StartArgs{
			ServerID: manager.ID(), OperationID: "audit-unavailable-run",
			Argv: []string{"/usr/bin/true"}, CloseOnExit: true,
		}},
	} {
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("%s executed with closed auditor: result=%+v err=%v", tc.name, result, err)
		}
	}
	info, err := manager.List(context.Background(), session.ListArgs{})
	if err != nil || info.OperationsUsed != 0 || info.Counts.Running != 0 {
		t.Fatalf("audit failure allowed new operation: info=%+v err=%v", info, err)
	}
}
