package mcpserver

import (
	"encoding/json"
	"log"
	"time"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/mutation"
)

// operationAudit belongs to the original MCP request. The retry ledger invokes
// it exactly once for a newly admitted operation, even if the request stops
// waiting or later clients replay the same operation_id.
type operationAudit struct {
	recorder  *audit.Log
	serverID  string
	requestID string
	tool      string
	started   time.Time // Only accessed by the admitted mutation's goroutine.
}

func (a *operationAudit) Started(operationID string) {
	a.started = time.Now()
	event := audit.NewEvent(audit.Event{
		ServerID: a.serverID, RequestID: a.requestID, Tool: a.tool,
		OperationID: operationID, Phase: "operation_start",
	})
	if err := a.recorder.Write(event); err != nil {
		// Stage 2 retains existing fail-open behavior. Blocking a new effect
		// after an audit error is introduced in the later fail-closed stage.
		log.Printf("audit operation_start failure (operation still executes): %v", err)
	}
}

func (a *operationAudit) Finished(receipt mutation.Receipt) {
	event := audit.Event{
		ServerID: a.serverID, RequestID: a.requestID, Tool: a.tool,
		OperationID: receipt.OperationID, Phase: "operation_finish",
		Status: receipt.Status, Error: receipt.Error, Receipt: receipt,
	}
	if !a.started.IsZero() {
		event.DurationMS = time.Since(a.started).Milliseconds()
	}
	if len(receipt.Data) != 0 {
		var context struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(receipt.Data, &context); err == nil {
			event.SessionID = context.SessionID
		}
	}
	if err := a.recorder.Write(audit.NewEvent(event)); err != nil {
		log.Printf("audit operation_finish failure (operation outcome unchanged): %v", err)
	}
}
