//go:build linux || darwin

package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

func TestAuditOutcomesAndRejectedSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const confidential = "PRIVATE-FILE-CONTENT-NOT-TO-LOG"
	valid := filepath.Join(dir, "exists.txt")
	missing := filepath.Join(dir, "missing.txt")
	if err := os.WriteFile(valid, []byte(confidential+"\nneedle\nneedle\n"), 0600); err != nil {
		t.Fatal(err)
	}

	manager := session.New(nil)
	defer manager.Close()
	files, err := filesystem.New(manager, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	auditPath := filepath.Join(dir, "audit.jsonl")
	writer, err := audit.Open(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	one, two := mcp.NewInMemoryTransports()
	server, err := NewWithAudit(manager, writer, files).Connect(ctx, two, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "audit-details-test", Version: "1"}, nil).Connect(ctx, one, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	result := call[filesystem.ReadResult](t, ctx, client, "file_read", filesystem.ReadArgs{
		Files: []filesystem.Range{{Path: valid}, {Path: missing}},
	})
	if len(result.Files) != 2 || result.Files[0].Error != "" || result.Files[1].Error == "" {
		t.Fatalf("incorrect partial file read: %+v", result)
	}
	invalid, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "file_read", Arguments: map[string]any{}})
	if err != nil || invalid == nil || !invalid.IsError {
		t.Fatalf("invalid schema not rejected: %+v, %v", invalid, err)
	}
	if _, err := exec.LookPath("rg"); err == nil {
		search := call[filesystem.SearchResult](t, ctx, client, "file_search", filesystem.SearchArgs{
			Path: dir, Pattern: "needle", Glob: "*.txt", MaxResults: 1,
		})
		if !search.Truncated || search.Reason != "limit" || len(search.Matches) != 1 {
			t.Fatalf("search limit not reported: %+v", search)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), confidential) {
		t.Fatal("file content leaked to audit")
	}
	events := make([]audit.Event, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event audit.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid JSONL audit: %v", err)
		}
		if event.ServerID != manager.ID() {
			t.Fatalf("missing server instance: %+v", event)
		}
		events = append(events, event)
	}
	var read, rejected, search int
	for _, e := range events {
		switch {
		case e.Tool == "file_read" && e.Phase == "request_finish" && e.Status == "ok":
			read++
			if e.Status != "ok" || e.Outcome != "partial" || e.FilesTotal != 2 || e.FilesFailed != 1 {
				t.Fatalf("missing partial read outcome: %+v", e)
			}
		case e.Tool == "file_read" && e.Phase == "request_finish" && e.Status == "rejected":
			rejected++
			if e.Reason != "schema_validation" || e.Status != "rejected" || e.Error != "invalid tool arguments" || e.Args != nil {
				t.Fatalf("invalid rejection event: %+v", e)
			}
		case e.Tool == "file_search" && e.Phase == "request_finish":
			search++
			if e.SearchMatches == nil || *e.SearchMatches != 1 || !e.Truncated || e.Reason != "limit" {
				t.Fatalf("missing search summary: %+v", e)
			}
		}
	}
	if read != 1 || rejected != 1 {
		t.Fatalf("expected one completed partial read and one schema rejection, got read=%d rejected=%d, events=%+v", read, rejected, events)
	}
	if _, err := exec.LookPath("rg"); err == nil && search != 1 {
		t.Fatalf("search summary missing, count %d", search)
	}
}
