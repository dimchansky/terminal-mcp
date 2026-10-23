//go:build linux || darwin

package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

func auditJSON(t *testing.T, input any) (string, string) {
	t.Helper()
	value, id := auditArguments(input)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded), id
}

func TestAuditTerminalCommandsAndSessionInputs(t *testing.T) {
	const token = "a shell argument which must be visible"
	s, id := auditJSON(t, session.StartArgs{
		OperationID: "start-1", ServerID: "instance",
		Argv: []string{"/bin/sh", "-c", "printf %s " + token},
		Cwd:  "/tmp/work", Env: map[string]string{"CHECK": "yes"},
		CloseOnExit: true, WaitFor: "completion",
	})
	if id != "" || !strings.Contains(s, token) || !strings.Contains(s, "/tmp/work") ||
		!strings.Contains(s, "CHECK") || !strings.Contains(s, "start-1") {
		t.Fatalf("terminal start arguments missing: %s / %s", s, id)
	}
	command := "git status --short\n"
	s, id = auditJSON(t, session.WriteArgs{
		SessionID: "terminal-1", OperationID: "write-1",
		Input:      []session.Input{{Text: &command}, {Key: "ENTER"}},
		CloseStdin: true,
	})
	if id != "terminal-1" || !strings.Contains(s, command[:len(command)-1]) ||
		!strings.Contains(s, "ENTER") || !strings.Contains(s, "close_stdin") {
		t.Fatalf("PTY input not captured: %s / %s", s, id)
	}
	s, id = auditJSON(t, session.ReadArgs{SessionID: "terminal-1", WaitMS: 250, Cursor: "next"})
	if id != "terminal-1" || !strings.Contains(s, "next") {
		t.Fatalf("read context missing: %s / %s", s, id)
	}
}

func TestAuditFilesOnlyRecordPathsAndMetadata(t *testing.T) {
	const oldText = "old file content sentinel"
	const newText = "new file content sentinel"
	for _, item := range []struct {
		name     string
		input    any
		required []string
	}{
		{"write", filesystem.WriteArgs{Path: "/tmp/new.go", Content: newText, OperationID: "write-new"},
			[]string{"/tmp/new.go", "create", "content_bytes"}},
		{"edit", filesystem.EditArgs{Path: "/tmp/old.go", ExpectedVersion: "sha256:one", Edits: []filesystem.Edit{{OldText: oldText, NewText: newText}}},
			[]string{"/tmp/old.go", "edits_count", "old_bytes", "new_bytes"}},
		{"read", filesystem.ReadArgs{Files: []filesystem.Range{{Path: "/tmp/old.go", StartLine: 12, EndLine: 23}}},
			[]string{"/tmp/old.go", "start_line", "end_line"}},
		{"search", filesystem.SearchArgs{Path: "/tmp", Pattern: "FindMe", Glob: "*.go"},
			[]string{"/tmp", "FindMe", "*.go"}},
		{"list", filesystem.ListArgs{Path: "/tmp", Glob: "*.go"},
			[]string{"/tmp", "*.go"}},
	} {
		t.Run(item.name, func(t *testing.T) {
			s, id := auditJSON(t, item.input)
			if id != "" || strings.Contains(s, oldText) || strings.Contains(s, newText) {
				t.Fatalf("file content leaked or session populated: %s / %s", s, id)
			}
			for _, must := range item.required {
				if !strings.Contains(s, must) {
					t.Fatalf("missing %q in %s", must, s)
				}
			}
		})
	}
}
