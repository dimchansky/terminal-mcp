package mcpserver

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
)

// auditValidationRejections records the request lifecycle for calls rejected by
// SDK argument validation before the typed tool handler runs. Valid tool calls
// are recorded by add; neither path duplicates the other. Full raw arguments
// for rejected calls are introduced in stage 3.
func auditValidationRejections(recorder *audit.Log, serverID string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			started := time.Now()
			result, err := next(ctx, method, req)
			if err != nil {
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
				id := audit.RequestID()
				start := audit.Event{
					SchemaVersion: audit.EventSchemaVersion,
					Time:          started.UTC().Format(time.RFC3339Nano),
					ServerID:      serverID, RequestID: id, Tool: call.Name,
					Phase: "request_start",
				}
				if writeErr := recorder.Write(start); writeErr != nil {
					log.Printf("audit rejected request_start failure (tool result unchanged): %v", writeErr)
				}
				finish := audit.NewEvent(audit.Event{
					ServerID: serverID, RequestID: id, Tool: call.Name,
					Phase: "request_finish", Status: "rejected",
					Reason: "schema_validation", Error: "invalid tool arguments",
					DurationMS: time.Since(started).Milliseconds(),
				})
				if writeErr := recorder.Write(finish); writeErr != nil {
					log.Printf("audit rejected request_finish failure (tool result unchanged): %v", writeErr)
				}
				break
			}
			return result, err
		}
	}
}
