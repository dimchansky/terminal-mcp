package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
)

type requestAuditKey struct{}

type requestAudit struct {
	id      string
	started time.Time
	handled bool // The typed handler took responsibility for request_finish.
}

func auditRequestFromContext(ctx context.Context) *requestAudit {
	info, _ := ctx.Value(requestAuditKey{}).(*requestAudit)
	return info
}

// auditToolRequests records all original JSON arguments before the SDK's typed
// unmarshalling and schema validation. This is the owner's forensic log of what
// MCP clients submitted. No supplied value is redacted for sensitivity, even a
// signed URL or password, and unknown/invalid fields must not be discarded.
//
// A complete request_start must be written before dispatch. Failed appends,
// degraded audit health, and unrepresentable input all refuse new tool work.
func auditToolRequests(recorder *audit.Log, serverID string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			call, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok || call == nil {
				slog.Error("audit: cannot inspect raw tools/call parameters; refusing request")
				return auditToolError("audit cannot inspect tool arguments; tool not executed"), nil
			}
			info := &requestAudit{id: audit.RequestID(), started: time.Now()}
			var rawArgs any
			if len(call.Arguments) != 0 {
				// Audit is for the computer owner to inspect *everything*
				// supplied by MCP clients, including passwords and signed URLs.
				// Keep the raw values before schema validation: no redaction.
				rawArgs = json.RawMessage(call.Arguments)
			}
			var callMeta, inputResponses any
			if len(call.Meta) != 0 {
				callMeta = call.Meta
			}
			if len(call.InputResponses) != 0 {
				inputResponses = call.InputResponses
			}
			start := audit.NewEvent(audit.Event{
				ServerID: serverID, RequestID: info.id, Tool: call.Name,
				OperationID: rawOperationID(call.Arguments),
				Phase:       "request_start", Args: rawArgs,
				CallMeta: callMeta, InputResponses: inputResponses,
				RequestState: call.RequestState,
			})
			if err := recorder.Start(start); err != nil {
				switch {
				case errors.Is(err, audit.ErrEventTooLarge), errors.Is(err, audit.ErrInvalidEvent):
					reason := "audit_record_too_large"
					if errors.Is(err, audit.ErrInvalidEvent) {
						reason = "audit_input_unrepresentable"
					}
					// The input cannot fit, so don't pretend a partial
					// request_start is complete. Record a small *rejection*
					// event without its arguments and refuse dispatch.
					rejection := audit.NewEvent(audit.Event{
						ServerID: serverID, RequestID: info.id, Tool: call.Name,
						OperationID: start.OperationID,
						Phase:       "request_rejected", Status: "rejected", Reason: reason,
						Error: err.Error(),
					})
					_ = recorder.Write(rejection) // The log records and diagnoses its own failures.
					return auditToolError("audit input cannot be fully recorded: " + err.Error() + "; tool not executed"), nil
				default:
					return auditToolError("audit_unavailable: " + err.Error() + "; tool not executed"), nil
				}
			}

			ctx = context.WithValue(ctx, requestAuditKey{}, info)
			result, err := next(ctx, method, req)
			if !info.handled {
				finish := audit.Event{
					ServerID: serverID, RequestID: info.id, Tool: call.Name,
					OperationID: start.OperationID,
					Phase:       "request_finish", Status: "error",
					DurationMS: time.Since(info.started).Milliseconds(),
				}
				if schemaRejection(result) {
					finish.Status = "rejected"
					finish.Reason = "schema_validation"
					finish.Error = "invalid tool arguments"
				} else if err != nil {
					finish.Error = err.Error()
				} else {
					finish.Error = "tool handler was not invoked"
				}
				if len(finish.Error) > 2048 {
					finish.Error = strings.ToValidUTF8(finish.Error[:2048], "�") + "..."
				}
				_ = recorder.Write(audit.NewEvent(finish))
			}
			return result, err
		}
	}
}

func rawOperationID(raw json.RawMessage) string {
	var args struct {
		OperationID string `json:"operation_id"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil {
		return ""
	}
	return args.OperationID
}

func schemaRejection(result mcp.Result) bool {
	output, ok := result.(*mcp.CallToolResult)
	if !ok || !output.IsError {
		return false
	}
	for _, content := range output.Content {
		msg, ok := content.(*mcp.TextContent)
		if ok && (strings.HasPrefix(msg.Text, "validating \"arguments\"") ||
			strings.HasPrefix(msg.Text, "unmarshaling arguments")) {
			return true
		}
	}
	return false
}

func auditToolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
	}
}
