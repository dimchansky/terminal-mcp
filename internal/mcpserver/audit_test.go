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

func TestAuditToolCallsWithoutRawArguments(t *testing.T) {
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
	const secret = "secret-value-must-not-reach-audit"
	run := call[session.ActionResult](t, context.Background(), client, "terminal_start", session.StartArgs{
		ServerID: list.ServerID, OperationID: "audit-test-1",
		Argv:        []string{"/bin/sh", "-c", "printf " + secret},
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
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "audit-test-1") {
		t.Fatalf("sensitive input leaked into audit log: %s", data)
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
	}
}

func TestAuditStartWriteFailurePreventsToolExecution(t *testing.T) {
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
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "terminal_list", Arguments: map[string]any{}})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatal("tool ran without an audit writer")
	}
}
