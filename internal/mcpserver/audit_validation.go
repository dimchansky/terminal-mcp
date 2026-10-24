package mcpserver

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
)

// auditValidationRejections records schema errors before the typed tool handler
// is reached. Handler errors are already recorded by add and must not appear
// twice. Do not copy rejected input data or schema error text into the audit.
func auditValidationRejections(recorder *audit.Log, serverID string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if method != "tools/call" || err != nil {
				return result, err
			}
			call, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return result, err
			}
			output, ok := result.(*mcp.CallToolResult)
			if !ok || !output.IsError {
				return result, err
			}
			for _, content := range output.Content {
				msg, ok := content.(*mcp.TextContent)
				if !ok || (!strings.HasPrefix(msg.Text, "validating \"arguments\"") &&
					!strings.HasPrefix(msg.Text, "unmarshaling arguments")) {
					continue
				}
				event := audit.NewEvent(audit.Event{
					ServerID:  serverID,
					RequestID: audit.RequestID(),
					Tool:      call.Name,
					Phase:     "rejected",
					Status:    "rejected",
					Reason:    "schema_validation",
					Error:     "invalid tool arguments",
				})
				if writeErr := recorder.Write(event); writeErr != nil && !errors.Is(writeErr, audit.ErrQueueFull) {
					log.Printf("audit validation failure (tool result unchanged): %v", writeErr)
				}
				break
			}
			return result, err
		}
	}
}
