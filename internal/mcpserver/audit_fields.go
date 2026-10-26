package mcpserver

import (
	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

// auditSessionID correlates an MCP request with its existing managed session.
func auditSessionID(value any) string {
	switch a := value.(type) {
	case session.ReadArgs:
		return a.SessionID
	case session.WriteArgs:
		return a.SessionID
	case session.SignalArgs:
		return a.SessionID
	case session.ResizeArgs:
		return a.SessionID
	case session.CloseArgs:
		return a.SessionID
	default:
		return ""
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
