//go:build linux || darwin

package mcpserver

import (
	"testing"

	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

func TestAuditCorrelationIdentifiers(t *testing.T) {
	cases := []struct {
		input     any
		operation string
		session   string
	}{
		{session.StartArgs{OperationID: "start"}, "start", ""},
		{session.WriteArgs{OperationID: "write", SessionID: "s1"}, "write", "s1"},
		{session.SignalArgs{OperationID: "signal", SessionID: "s2"}, "signal", "s2"},
		{session.ResizeArgs{OperationID: "resize", SessionID: "s3"}, "resize", "s3"},
		{session.CloseArgs{OperationID: "close", SessionID: "s4"}, "close", "s4"},
		{session.ReadArgs{SessionID: "s5"}, "", "s5"},
		{session.ListArgs{}, "", ""},
		{filesystem.WriteArgs{OperationID: "file-write"}, "file-write", ""},
		{filesystem.EditArgs{OperationID: "file-edit"}, "file-edit", ""},
		{filesystem.ReadArgs{}, "", ""},
		{filesystem.SearchArgs{}, "", ""},
		{filesystem.ListArgs{}, "", ""},
	}
	for _, tt := range cases {
		if got := auditOperationID(tt.input); got != tt.operation {
			t.Errorf("%T: operation ID %q, want %q", tt.input, got, tt.operation)
		}
		if got := auditSessionID(tt.input); got != tt.session {
			t.Errorf("%T: session ID %q, want %q", tt.input, got, tt.session)
		}
	}
}
