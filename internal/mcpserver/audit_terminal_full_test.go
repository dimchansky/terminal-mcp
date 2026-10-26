//go:build linux || darwin

package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/session"
)

func TestAuditKeepsTerminalEnvironmentAndExactInput(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	one, two := mcp.NewInMemoryTransports()
	server, err := NewWithAudit(manager, recorder).Connect(ctx, one, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "exact-input-test", Version: "1"}, nil).Connect(ctx, two, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	const envValue = "environment <&> \"quotes\" with Unicode: русский"
	start := call[session.ActionResult](t, ctx, client, "terminal_start", session.StartArgs{
		ServerID: manager.ID(), OperationID: "env-process", Argv: []string{"/bin/cat"},
		Mode: "pipes", Env: map[string]string{"AUDIT_ENV": envValue},
	})
	if start.Receipt.Status != "applied" || start.Receipt.SessionID == "" {
		t.Fatalf("cannot start test process: %+v", start)
	}
	const inputText = "literal command & <markup> \"quotes\" and password=test-placeholder\n"
	const inputBase64 = "YWJjAAo=" // byte-exact input with a NUL.
	binary := inputBase64
	plain := inputText
	written := call[session.ActionResult](t, ctx, client, "terminal_write", session.WriteArgs{
		ServerID: manager.ID(), OperationID: "exact-input", SessionID: start.Receipt.SessionID,
		Input: []session.Input{{Text: &plain}, {BytesBase64: &binary}}, CloseStdin: true,
	})
	if written.Receipt.Status != "applied" {
		t.Fatalf("cannot submit exact terminal input: %+v", written)
	}

	seenEnv, seenText, seenBase64 := false, false, false
	for _, event := range readLifecycleAudit(t, path) {
		if event.Phase != "request_start" {
			continue
		}
		args, ok := event.Args.(map[string]any)
		if !ok {
			t.Fatalf("tool arguments are missing: %+v", event)
		}
		switch event.OperationID {
		case "env-process":
			env, ok := args["env"].(map[string]any)
			if !ok || env["AUDIT_ENV"] != envValue || event.Tool != "terminal_start" {
				t.Fatalf("environment override lost or changed: %+v", event)
			}
			seenEnv = true
		case "exact-input":
			inputs, ok := args["input"].([]any)
			if !ok || len(inputs) != 2 || args["session_id"] != start.Receipt.SessionID ||
				args["close_stdin"] != true {
				t.Fatalf("terminal_write input shape changed: %+v", event)
			}
			first, ok1 := inputs[0].(map[string]any)
			second, ok2 := inputs[1].(map[string]any)
			if !ok1 || !ok2 {
				t.Fatalf("terminal_write items not preserved: %+v", inputs)
			}
			seenText = first["text"] == inputText
			seenBase64 = second["bytes_base64"] == inputBase64
		}
	}
	if !seenEnv || !seenText || !seenBase64 {
		t.Fatalf("missing exact inputs: env=%t text=%t bytes_base64=%t", seenEnv, seenText, seenBase64)
	}
}
