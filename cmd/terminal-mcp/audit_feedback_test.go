//go:build linux || darwin

package main

import (
	"bytes"
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

// waitForAudit finds a matching synchronous JSONL event. An accepted audit
// append is visible immediately, but the helper also covers concurrent hooks.
func waitForAudit(t *testing.T, path, tool, phase string, match func(audit.Event) bool) audit.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(raw, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var event audit.Event
			if json.Unmarshal(line, &event) == nil && event.Tool == tool && event.Phase == phase && match(event) {
				return event
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching audit event for %s/%s within deadline", tool, phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func textFromErrorResult(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || !result.IsError {
		t.Fatalf("expected MCP tool error, got %+v", result)
	}
	for _, part := range result.Content {
		if value, ok := part.(*mcp.TextContent); ok {
			return value.Text
		}
	}
	t.Fatalf("tool error has no text: %+v", result.Content)
	return ""
}

func TestMCPAuditFeedbackLoop(t *testing.T) {
	_, rgErr := exec.LookPath("rg")
	if rgErr != nil {
		t.Log("ripgrep unavailable; testing all scenarios except file_search")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const privateText = "PRIVATE-FILE-BODY-NOT-FOR-AUDIT"
	successPath := filepath.Join(dir, "ok.txt")
	ambiguousPath := filepath.Join(dir, "ambiguous.txt")
	searchPath := filepath.Join(dir, "search.txt")
	largePath := filepath.Join(dir, "large.txt")
	files := map[string]string{
		successPath:   privateText + "\n",
		ambiguousPath: "repeat and repeat\n",
		searchPath:    "needle\nneedle\n",
		largePath:     strings.Repeat(strings.Repeat("B", 1024)+"\n", 300),
	}
	for filename, body := range files {
		if err := os.WriteFile(filename, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	auditPath := filepath.Join(dir, "audit.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- runWithContext(ctx, []string{
			"--transport=stdio", "--file-root=" + dir, "--audit-file=" + auditPath,
		}, b)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "audit-feedback", Version: "1"}, nil)
	conn, err := client.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	list := mcpList(t, ctx, conn)
	if list.ServerID == "" {
		t.Fatal("server ID unavailable")
	}
	waitForAudit(t, auditPath, "server", "server_start", func(e audit.Event) bool {
		return e.ServerID == list.ServerID && e.Status == "ok"
	})

	run := mcpCall[session.ActionResult](t, ctx, conn, "terminal_start", session.StartArgs{
		ServerID: list.ServerID, OperationID: "feedback-command", Argv: []string{"/bin/sh", "-c", "printf feedback-ok"},
		CloseOnExit: true, WaitFor: "completion", WaitMS: 3000,
	})
	if run.Receipt.Status != "applied" || run.Observation == nil ||
		run.Observation.Session.Exit == nil || run.Observation.Session.Exit.Code == nil ||
		*run.Observation.Session.Exit.Code != 0 {
		t.Fatalf("terminal response did not report exit 0: %+v", run)
	}
	waitForAudit(t, auditPath, "terminal_start", "request_finish", func(e audit.Event) bool {
		return e.Status == "applied" && e.SessionID == run.Receipt.SessionID
	})

	missingID := "0123456789abcdef0123456789abcdef"
	badSession, err := conn.CallTool(ctx, &mcp.CallToolParams{
		Name: "terminal_read", Arguments: session.ReadArgs{SessionID: missingID},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := textFromErrorResult(t, badSession)
	if !strings.Contains(message, "session_not_found_or_expired") || !strings.Contains(message, "terminal_list") {
		t.Fatalf("missing recovery advice: %s", message)
	}
	waitForAudit(t, auditPath, "terminal_read", "request_finish", func(e audit.Event) bool {
		return e.Status == "error" && strings.Contains(e.Error, "terminal_list") && e.SessionID == missingID
	})

	read := mcpCall[filesystem.ReadResult](t, ctx, conn, "file_read", filesystem.ReadArgs{
		Files: []filesystem.Range{{Path: successPath}, {Path: filepath.Join(dir, "missing.txt")}},
	})
	if len(read.Files) != 2 || read.Files[0].Error != "" || read.Files[1].Error == "" {
		t.Fatalf("partial read response incorrect: %+v", read)
	}
	waitForAudit(t, auditPath, "file_read", "request_finish", func(e audit.Event) bool {
		return e.Status == "ok" && e.Outcome == "partial" && e.FilesTotal == 2 && e.FilesFailed == 1
	})
	ambiguous := mcpCall[filesystem.ReadResult](t, ctx, conn, "file_read", filesystem.ReadArgs{
		Files: []filesystem.Range{{Path: ambiguousPath}},
	})
	edit := mcpCall[filesystem.ActionResult](t, ctx, conn, "file_edit", filesystem.EditArgs{
		ServerID: list.ServerID, OperationID: "feedback-ambiguous-edit",
		Path: ambiguousPath, ExpectedVersion: ambiguous.Files[0].Version,
		Edits: []filesystem.Edit{{OldText: "repeat", NewText: "X"}},
	})
	if edit.Receipt.Status != "failed" || !strings.Contains(edit.Receipt.Error, "matches 2 locations") {
		t.Fatalf("ambiguous edit response: %+v", edit)
	}
	waitForAudit(t, auditPath, "file_edit", "request_finish", func(e audit.Event) bool {
		return e.Status == "failed" && strings.Contains(e.Error, "matches 2 locations")
	})
	unchanged, err := os.ReadFile(ambiguousPath)
	if err != nil || string(unchanged) != files[ambiguousPath] {
		t.Fatalf("file changed after rejected edit: %q %v", unchanged, err)
	}

	if rgErr == nil {
		search := mcpCall[filesystem.SearchResult](t, ctx, conn, "file_search", filesystem.SearchArgs{
			Path: dir, Pattern: "needle", Glob: "search.txt", MaxResults: 1,
		})
		if !search.Truncated || search.Reason != "limit" || len(search.Matches) != 1 {
			t.Fatalf("search response incorrect: %+v", search)
		}
		waitForAudit(t, auditPath, "file_search", "request_finish", func(e audit.Event) bool {
			return e.Status == "ok" && e.Outcome == "partial" && e.Reason == "limit" &&
				e.Truncated && e.SearchMatches != nil && *e.SearchMatches == 1
		})
	}

	big := mcpCall[filesystem.ReadResult](t, ctx, conn, "file_read", filesystem.ReadArgs{
		Files:    []filesystem.Range{{Path: largePath, StartLine: 1, EndLine: 300}},
		MaxBytes: 115000,
	})
	if len(big.Files) != 1 || big.Files[0].Error != "" || !big.Files[0].Truncated ||
		big.Files[0].NextLine == 0 || len(big.Files[0].Lines) < 100 {
		t.Fatalf("large read was incorrectly limited: %+v", big.Files)
	}
	waitForAudit(t, auditPath, "file_read", "request_finish", func(e audit.Event) bool {
		return e.Outcome == "ok" && e.FilesTotal == 1 && e.Truncated
	})

	invalid, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: "file_read", Arguments: filesystem.ReadArgs{
		Files:    []filesystem.Range{{Path: largePath}},
		MaxBytes: filesystem.MaxReadBytes + 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = textFromErrorResult(t, invalid)
	waitForAudit(t, auditPath, "file_read", "request_finish", func(e audit.Event) bool {
		return e.Status == "rejected" && e.Reason == "schema_validation" && e.Args == nil
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not flush audit and exit")
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), privateText) || strings.Contains(string(data), files[ambiguousPath]) {
		t.Fatal("audit contains source file content")
	}
	requests := make(map[string][]audit.Event)
	operations := make(map[string][]audit.Event)
	starts, rejected := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event audit.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("corrupt audit JSONL after graceful shutdown: %v", err)
		}
		if event.ServerID != list.ServerID || event.Time == "" || event.SchemaVersion != audit.EventSchemaVersion {
			t.Fatalf("wrong server context/schema: %+v", event)
		}
		switch event.Phase {
		case "server_start":
			starts++
		case "request_start", "request_finish":
			requests[event.RequestID] = append(requests[event.RequestID], event)
			if event.Phase == "request_finish" && event.Status == "rejected" {
				rejected++
				if event.Tool != "file_read" || event.Args != nil || event.Reason != "schema_validation" {
					t.Fatalf("invalid schema rejection audit: %+v", event)
				}
			}
		case "operation_start", "operation_finish":
			if event.OperationID == "" {
				t.Fatalf("operation event has no operation ID: %+v", event)
			}
			operations[event.OperationID] = append(operations[event.OperationID], event)
		default:
			t.Fatalf("unexpected event phase: %+v", event)
		}
	}
	wantCalls := 9 // Eight regular requests and one SDK schema rejection.
	if rgErr != nil {
		wantCalls--
	}
	if starts != 1 || rejected != 1 || len(requests) != wantCalls || len(operations) != 2 {
		t.Fatalf("unexpected counts: start=%d rejected=%d requests=%d want=%d operations=%d",
			starts, rejected, len(requests), wantCalls, len(operations))
	}
	for requestID, pair := range requests {
		if requestID == "" || len(pair) != 2 || pair[0].Phase != "request_start" ||
			pair[1].Phase != "request_finish" || pair[0].Tool != pair[1].Tool {
			t.Fatalf("bad audit request pair: %q %+v", requestID, pair)
		}
	}
	for id, pair := range operations {
		if len(pair) != 2 || pair[0].Phase != "operation_start" || pair[1].Phase != "operation_finish" ||
			pair[0].RequestID != pair[1].RequestID || pair[0].Tool != pair[1].Tool ||
			pair[1].Receipt == nil {
			t.Fatalf("bad actual operation pair: id=%q events=%+v", id, pair)
		}
		if pair[1].Tool == "terminal_start" && (id != "feedback-command" || pair[1].Status != "applied") {
			t.Fatalf("terminal_start operation was not applied: %+v", pair)
		}
		if pair[1].Tool == "file_edit" && (id != "feedback-ambiguous-edit" || pair[1].Status != "failed") {
			t.Fatalf("failed edit operation missing: %+v", pair)
		}
		if request := requests[pair[0].RequestID]; len(request) != 2 || request[0].OperationID != id {
			t.Fatalf("operation is not linked to originating MCP request: %+v", pair)
		}
	}

}
