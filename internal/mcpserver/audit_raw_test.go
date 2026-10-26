//go:build linux || darwin

package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

func TestAuditFullFileInputsAndRejectedRawArguments(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	recorder, err := audit.Open(path)
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
	files, err := filesystem.New(manager, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server, err := NewWithAudit(manager, recorder, files).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "raw-audit-client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	const original = "Content <full> & \"double\" and 'single'\nUnicode: русский, lietuvių, 日本語\\slash\n"
	const oldText = "Content <full> & \"double\""
	const newText = "Revised & <full> \"double\" with preserved details"
	filePath := filepath.Join(dir, "contents.txt")
	written := call[filesystem.ActionResult](t, ctx, client, "file_write", filesystem.WriteArgs{
		ServerID: manager.ID(), OperationID: "raw-write", Path: filePath, Content: original,
	})
	if written.Receipt.Status != "applied" {
		t.Fatalf("file_write failed: %+v", written)
	}
	read := call[filesystem.ReadResult](t, ctx, client, "file_read", filesystem.ReadArgs{
		Files: []filesystem.Range{{Path: filePath}},
	})
	if read.Files[0].Version == "" {
		t.Fatalf("file version missing: %+v", read.Files[0])
	}
	edited := call[filesystem.ActionResult](t, ctx, client, "file_edit", filesystem.EditArgs{
		ServerID: manager.ID(), OperationID: "raw-edit", Path: filePath,
		ExpectedVersion: read.Files[0].Version,
		Edits:           []filesystem.Edit{{OldText: oldText, NewText: newText}},
	})
	if edited.Receipt.Status != "applied" {
		t.Fatalf("file_edit failed: %+v", edited)
	}
	wantFile := strings.Replace(original, oldText, newText, 1)
	actual, err := os.ReadFile(filePath)
	if err != nil || string(actual) != wantFile {
		t.Fatalf("wrong file result: %q, err=%v", actual, err)
	}

	// Validation fails before the typed handler. Include fields the SDK
	// does not recognize, and a value with the wrong JSON type.
	const rawNote = "unknown-value <&> русский"
	badPath := filepath.Join(dir, "must-not-exist.txt")
	invalid := map[string]any{
		"server_id": manager.ID(), "operation_id": "raw-rejected",
		"path": badPath, "content": 12345,
		"unknown_payload": map[string]any{"text": rawNote, "count": 17, "nested": []any{"alpha", true, nil}},
	}
	rejected, err := client.CallTool(ctx, &mcp.CallToolParams{
		Name: "file_write", Arguments: invalid,
		Meta: mcp.Meta{"audit_note": "metadata <&> from client"},
	})
	if err != nil || rejected == nil || !rejected.IsError {
		t.Fatalf("SDK failed to reject invalid input: result=%+v err=%v", rejected, err)
	}
	if _, err := os.Stat(badPath); !os.IsNotExist(err) {
		t.Fatalf("schema-rejected file_write had a side effect: %v", err)
	}
	events := readLifecycleAudit(t, path)
	starts := make(map[string]audit.Event)
	finishes := make(map[string]audit.Event)
	operationStarts := make(map[string]int)
	for _, e := range events {
		switch e.Phase {
		case "request_start":
			starts[e.RequestID] = e
		case "request_finish":
			finishes[e.RequestID] = e
		case "operation_start":
			operationStarts[e.OperationID]++
		}
	}
	if len(starts) != 4 || len(finishes) != 4 || operationStarts["raw-write"] != 1 ||
		operationStarts["raw-edit"] != 1 || operationStarts["raw-rejected"] != 0 {
		t.Fatalf("incomplete request or operation audit: starts=%d finishes=%d effects=%+v",
			len(starts), len(finishes), operationStarts)
	}
	var writeSeen, editSeen, rejectSeen bool
	for id, start := range starts {
		finish, ok := finishes[id]
		if !ok || finish.Tool != start.Tool || start.SchemaVersion != audit.EventSchemaVersion {
			t.Fatalf("unpaired MCP request: %+v", start)
		}
		input, ok := start.Args.(map[string]any)
		if !ok {
			t.Fatalf("raw input was not a JSON object: %+v", start)
		}
		switch start.OperationID {
		case "raw-write":
			writeSeen = true
			if input["content"] != original || input["operation_id"] != "raw-write" ||
				input["server_id"] != manager.ID() || finish.Status != "applied" {
				t.Fatalf("full file_write content was changed/omitted: %+v", start)
			}
		case "raw-edit":
			editSeen = true
			edits, ok := input["edits"].([]any)
			if !ok || len(edits) != 1 {
				t.Fatalf("full edit array missing: %+v", start)
			}
			item, ok := edits[0].(map[string]any)
			if !ok || item["old_text"] != oldText || item["new_text"] != newText ||
				input["expected_version"] != read.Files[0].Version || finish.Status != "applied" {
				t.Fatalf("full file_edit inputs not preserved: %+v", start)
			}
		case "raw-rejected":
			rejectSeen = true
			extra, ok := input["unknown_payload"].(map[string]any)
			if !ok || input["content"] != float64(12345) || extra["text"] != rawNote ||
				extra["count"] != float64(17) || finish.Status != "rejected" ||
				finish.Reason != "schema_validation" {
				t.Fatalf("schema-rejected raw input lost fields: start=%+v finish=%+v", start, finish)
			}
			meta, ok := start.CallMeta.(map[string]any)
			if !ok || meta["audit_note"] != "metadata <&> from client" {
				t.Fatalf("schema-rejected tool-call metadata was lost: %+v", start)
			}
		}
	}
	if !writeSeen || !editSeen || !rejectSeen {
		t.Fatalf("missing complete input records (write=%v edit=%v rejected=%v)", writeSeen, editSeen, rejectSeen)
	}
}

func TestOversizedAuditedRequestIsRejectedBeforeEffect(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	recorder, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := recorder.Close(); err != nil {
			t.Error(err)
		}
	}()
	server := mcp.NewServer(&mcp.Implementation{Name: "audit-size-test", Version: "1"}, nil)
	server.AddReceivingMiddleware(auditToolRequests(recorder, "size-test"))
	var effects atomic.Int32
	add(server, recorder, "size-test", "size_test_mutation", "A mock mutation for audit size testing", false,
		func(_ context.Context, _ session.StartArgs) (struct {
			OK bool `json:"ok"`
		}, error) {
			effects.Add(1)
			return struct {
				OK bool `json:"ok"`
			}{OK: true}, nil
		})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverConn, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverConn.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "size-test-client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	// Go's default JSON marshaler HTML-escapes each '&' into six bytes.
	// The serialized event exceeds the 4 MiB audit-record limit, even
	// though no actual tool operation uses the string.
	tooLarge := session.StartArgs{
		OperationID: "too-large", ServerID: "size-test", Argv: []string{strings.Repeat("&", 720000)},
	}
	rejected, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "size_test_mutation", Arguments: tooLarge})
	if err != nil || rejected == nil || !rejected.IsError {
		t.Fatalf("oversized input was not explicitly rejected: result=%+v err=%v", rejected, err)
	}
	msg := ""
	for _, content := range rejected.Content {
		if c, ok := content.(*mcp.TextContent); ok {
			msg += c.Text
		}
	}
	if !strings.Contains(msg, "tool not executed") || effects.Load() != 0 {
		t.Fatalf("oversized input was truncated or executed: response=%q effects=%d", msg, effects.Load())
	}

	var denial bool
	for _, e := range readLifecycleAudit(t, path) {
		if e.Tool == "size_test_mutation" {
			if e.Phase != "request_rejected" || e.Status != "rejected" ||
				e.Reason != "audit_record_too_large" || e.Args != nil {
				t.Fatalf("oversized input was misrepresented as fully audited: %+v", e)
			}
			denial = true
		}
	}
	if !denial {
		t.Fatal("no auditable oversized-input rejection was recorded")
	}

	// A sizable input just below the 4 MiB file limit must be preserved
	// in full and executed. This also verifies recovery after a denial.
	nearBoundary := session.StartArgs{
		OperationID: "near-limit", ServerID: "size-test", Argv: []string{strings.Repeat("&", 620000)},
	}
	nearResult := call[struct {
		OK bool `json:"ok"`
	}](t, ctx, client, "size_test_mutation", nearBoundary)
	if !nearResult.OK || effects.Load() != 1 {
		t.Fatalf("near-boundary input did not execute: %+v effects=%d", nearResult, effects.Load())
	}
	for _, e := range readLifecycleAudit(t, path) {
		if e.Phase == "request_start" && e.OperationID == "near-limit" {
			args, isObject := e.Args.(map[string]any)
			argv, ok := args["argv"].([]any)
			if !isObject || !ok || len(argv) != 1 || argv[0] != nearBoundary.Argv[0] {
				t.Fatalf("near-boundary input was truncated or modified: %+v", e)
			}
			return
		}
	}
	t.Fatal("near-boundary input has no complete request_start")
}
