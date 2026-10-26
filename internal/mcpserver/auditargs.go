package mcpserver

import (
	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

// auditArguments records terminal command arguments and PTY input as supplied.
// File tool contents are intentionally excluded; navigation and target paths
// remain visible to the local owner.
func auditArguments(value any) (any, string) {
	switch a := value.(type) {
	case session.StartArgs:
		return a, ""
	case session.ListArgs:
		return a, ""
	case session.ReadArgs:
		return a, a.SessionID
	case session.WriteArgs:
		return a, a.SessionID
	case session.SignalArgs:
		return a, a.SessionID
	case session.ResizeArgs:
		return a, a.SessionID
	case session.CloseArgs:
		return a, a.SessionID
	case filesystem.ReadArgs:
		return a, ""
	case filesystem.ListArgs:
		return a, ""
	case filesystem.SearchArgs:
		return a, ""
	case filesystem.EditArgs:
		edits := make([]map[string]int, len(a.Edits))
		for i, edit := range a.Edits {
			edits[i] = map[string]int{"old_bytes": len(edit.OldText), "new_bytes": len(edit.NewText)}
		}
		return map[string]any{
			"server_id":        a.ServerID,
			"operation_id":     a.OperationID,
			"path":             a.Path,
			"expected_version": a.ExpectedVersion,
			"edits_count":      len(a.Edits),
			"edits":            edits,
		}, ""
	case filesystem.WriteArgs:
		mode := "create"
		if a.ExpectedVersion != "" {
			mode = "replace"
		}
		return map[string]any{
			"server_id":        a.ServerID,
			"operation_id":     a.OperationID,
			"path":             a.Path,
			"mode":             mode,
			"expected_version": a.ExpectedVersion,
			"content_bytes":    len(a.Content),
		}, ""
	default:
		return map[string]any{}, ""
	}
}

// auditOperationID correlates each mutating MCP request with the independent
// effect recorded by the shared retry ledger. Read-only calls have no key.
func auditOperationID(value any) string {
	switch a := value.(type) {
	case session.StartArgs:
		return a.OperationID
	case session.WriteArgs:
		return a.OperationID
	case session.SignalArgs:
		return a.OperationID
	case session.ResizeArgs:
		return a.OperationID
	case session.CloseArgs:
		return a.OperationID
	case filesystem.WriteArgs:
		return a.OperationID
	case filesystem.EditArgs:
		return a.OperationID
	default:
		return ""
	}
}

func auditProcess(o session.Observation) any {
	return map[string]any{
		"state":         o.Session.ProcessState,
		"exit":          o.Session.Exit,
		"output_done":   o.Session.OutputDone,
		"closed":        o.Session.Closed,
		"return_reason": o.ReturnReason,
	}
}
