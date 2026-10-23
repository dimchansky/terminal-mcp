//go:build linux || darwin

package main

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
)

func TestServerStartAuditHasInstanceID(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- runWithContext(ctx, []string{
			"--transport=stdio", "--file-root=" + dir, "--audit-file=" + path,
		}, b)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "startup-audit-test", Version: "1"}, nil)
	conn, err := client.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	list := mcpList(t, ctx, conn)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server failed to stop")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var starts int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event audit.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("corrupt audit JSONL: %v", err)
		}
		if event.ServerID != list.ServerID {
			t.Fatalf("audit server ID %q differs from MCP server ID %q", event.ServerID, list.ServerID)
		}
		if event.Tool == "server" && event.Phase == "server_start" {
			starts++
			if event.Status != "ok" || event.Time == "" {
				t.Fatalf("invalid startup event: %+v", event)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("expected one server_start event, found %d", starts)
	}
}
