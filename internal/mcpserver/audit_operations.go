package mcpserver

import (
	"encoding/json"
	"fmt"
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

func (a *operationAudit) Started(operationID string) error {
	a.started = time.Now()
	event := audit.NewEvent(audit.Event{
		ServerID: a.serverID, RequestID: a.requestID, Tool: a.tool,
		OperationID: operationID, Phase: "operation_start",
	})
	if err := a.recorder.Start(event); err != nil {
		return err // Ledger must not reserve the retry key or invoke the effect.
	}
	return nil
}

func (a *operationAudit) Finished(receipt mutation.Receipt) {
	defer func() {
		if p := recover(); p != nil {
			// Completion panics happen after the effect ran. Keep the
			// real ledger receipt, but refuse later unaudited actions.
			a.recorder.Degrade(fmt.Errorf("operation_finish audit observer panicked: %v", p))
		}
	}()
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
	// This mutation already ran; the logger records and reports failures
	// without modifying the real receipt.
	_ = a.recorder.Write(audit.NewEvent(event))
}
