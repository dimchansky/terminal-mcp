//go:build linux || darwin

package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/session"
)

func TestMCPHistoryBudgetExhaustionRefusesNewEffects(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	const limit int64 = 1000
	recorder, err := audit.OpenWithLimit(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	manager := session.New(nil)
	defer func() {
		manager.Close()
		if err := recorder.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	one, two := mcp.NewInMemoryTransports()
	server, err := NewWithAudit(manager, recorder).Connect(ctx, two, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "retention-test", Version: "1"}, nil).Connect(ctx, one, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	for range 30 {
		_, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "terminal_list", Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if recorder.Status().State == "degraded" {
			break
		}
	}
	status := recorder.Status()
	if status.State != "degraded" || status.BytesLimit != limit ||
		!strings.Contains(status.LastError, "audit_history_limit") {
		t.Fatalf("expected sticky storage-budget degradation: %+v", status)
	}
	// The attempted command must not be launched or consume a retry key.
	response, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "terminal_start",
		Arguments: session.StartArgs{
			ServerID: manager.ID(), OperationID: "after-hard-limit", Argv: []string{"/bin/true"},
		}})
	if err != nil || response == nil || !response.IsError {
		t.Fatalf("MCP allowed new work after audit history limit: response=%+v err=%v", response, err)
	}
	state, err := manager.List(context.Background(), session.ListArgs{})
	if err != nil || state.OperationsUsed != 0 || state.Counts.Running != 0 {
		t.Fatalf("history budget failure admitted a terminal mutation: %+v %v", state, err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || int64(len(stored)) != status.BytesUsed || int64(len(stored)) > limit {
		t.Fatalf("history limit corrupted file/accounting: %v bytes, %d used, err=%v", len(stored), status.BytesUsed, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(stored)), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("history contains an incomplete JSONL record: %q", line)
		}
	}
}
