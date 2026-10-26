//go:build linux || darwin

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/mutation"
	"terminalmcp/internal/session"
)

type auditActionResult struct {
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
}

func newFailureAuditClient(t *testing.T, register func(*mcp.Server, *session.Manager, *audit.Log)) (*mcp.ClientSession, *session.Manager, *audit.Log, string) {
	t.Helper()
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
	server := mcp.NewServer(&mcp.Implementation{Name: "audit-failure-test", Version: "1"}, nil)
	server.AddReceivingMiddleware(auditToolRequests(recorder, manager.ID()))
	register(server, manager, recorder)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	one, two := mcp.NewInMemoryTransports()
	serverConn, err := server.Connect(ctx, two, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "audit-failure-client", Version: "1"}, nil).Connect(ctx, one, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = serverConn.Close()
		manager.Close()
		if err := recorder.Close(); err != nil {
			t.Error(err)
		}
	})
	return client, manager, recorder, path
}

func TestFailedOperationStartDoesNotConsumeKeyOrExecuteEffect(t *testing.T) {
	const fault = "injected failure after request_start but before operation_start"
	var effects atomic.Int32
	client, manager, recorder, path := newFailureAuditClient(t, func(s *mcp.Server, mgr *session.Manager, a *audit.Log) {
		add(s, a, mgr.ID(), "failing_pre_effect", "Simulate loss of audit between request start and mutation admission", false,
			func(ctx context.Context, in session.StartArgs) (auditActionResult, error) {
				// This models an async audit write failure in another request.
				a.Degrade(errors.New(fault))
				r, replayed, err := mgr.Mutate(ctx, in.ServerID, in.OperationID, "failing_pre_effect", in.Argv,
					func() mutation.Receipt {
						effects.Add(1)
						return mutation.Receipt{Status: "applied"}
					})
				return auditActionResult{Status: r.Status, Replayed: replayed}, err
			})
	})
	args := session.StartArgs{ServerID: manager.ID(), OperationID: "first-key", Argv: []string{"effect"}}
	response, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "failing_pre_effect", Arguments: args})
	if err != nil || response == nil || !response.IsError {
		t.Fatalf("unaudited effect was allowed: response=%+v err=%v", response, err)
	}
	body := ""
	for _, content := range response.Content {
		if txt, ok := content.(*mcp.TextContent); ok {
			body += txt.Text
		}
	}
	if !strings.Contains(body, "operation_not_executed") || effects.Load() != 0 {
		t.Fatalf("did not report safe non-execution: response=%q effects=%d", body, effects.Load())
	}
	info, err := manager.List(context.Background(), session.ListArgs{})
	if err != nil || info.OperationsUsed != 0 {
		t.Fatalf("failed audit consumed retry key: %+v err=%v", info, err)
	}
	if recorder.Status().State != "degraded" || !strings.Contains(recorder.Status().LastError, fault) {
		t.Fatalf("audit failure was not exposed: %+v", recorder.Status())
	}
	// An already admitted request may record its actual failed admission
	// outcome. But no operation_start / operation_finish may be emitted.
	var requestStarted, requestFinished bool
	for _, event := range readLifecycleAudit(t, path) {
		switch event.Phase {
		case "request_start":
			requestStarted = true
		case "request_finish":
			requestFinished = true
			if event.Status != "error" || !strings.Contains(event.Error, "operation_not_executed") {
				t.Fatalf("pre-effect failure result not explained: %+v", event)
			}
		case "operation_start", "operation_finish":
			t.Fatalf("pre-effect denial created a fictitious operation event: %+v", event)
		}
	}
	if !requestStarted || !requestFinished {
		t.Fatalf("failed admission did not preserve request evidence: start=%t finish=%t", requestStarted, requestFinished)
	}
	// Any *new* request must be rejected by the outer admission gate.
	second, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "failing_pre_effect", Arguments: args})
	if err != nil || second == nil || !second.IsError || effects.Load() != 0 {
		t.Fatalf("degraded audit allowed subsequent work: %+v err=%v effects=%d", second, err, effects.Load())
	}
}

func TestFailedOperationFinishPreservesActualReceiptButBlocksNewWork(t *testing.T) {
	var effects atomic.Int32
	client, manager, recorder, path := newFailureAuditClient(t, func(s *mcp.Server, mgr *session.Manager, a *audit.Log) {
		add(s, a, mgr.ID(), "big_receipt", "Simulate an audit failure after a completed effect", false,
			func(ctx context.Context, in session.StartArgs) (auditActionResult, error) {
				r, replayed, err := mgr.Mutate(ctx, in.ServerID, in.OperationID, "big_receipt", in.Argv,
					func() mutation.Receipt {
						effects.Add(1)
						// Raw receipt is valid and was really committed. Its
						// JSONL operation_finish would exceed the 4 MiB limit.
						blob, _ := json.Marshal(strings.Repeat("&", 720000))
						return mutation.Receipt{Status: "applied", Data: blob}
					})
				return auditActionResult{Status: r.Status, Replayed: replayed}, err
			})
	})
	args := session.StartArgs{ServerID: manager.ID(), OperationID: "applied-once", Argv: []string{"effect"}}
	out := call[auditActionResult](t, context.Background(), client, "big_receipt", args)
	if out.Status != "applied" || out.Replayed || effects.Load() != 1 {
		t.Fatalf("actual effect result was falsified: %+v effects=%d", out, effects.Load())
	}
	if health := recorder.Status(); health.State != "degraded" ||
		!strings.Contains(health.LastError, "maximum file size") {
		t.Fatalf("post-effect audit loss did not become sticky: %+v", health)
	}
	var requestFinished, operationStarted, operationFinished bool
	for _, event := range readLifecycleAudit(t, path) {
		switch event.Phase {
		case "request_finish":
			requestFinished = true
			if event.Status != "ok" {
				t.Fatalf("request finish wrongly reported a tool error after a completed effect: %+v", event)
			}
		case "operation_start":
			operationStarted = true
		case "operation_finish":
			operationFinished = true
		}
	}
	if !operationStarted || operationFinished || !requestFinished {
		t.Fatalf("unexpected evidence after a failed operation_finish: started=%t finished=%t request=%t",
			operationStarted, operationFinished, requestFinished)
	}
	second, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "big_receipt", Arguments: session.StartArgs{
		ServerID: manager.ID(), OperationID: "different-new-action", Argv: []string{"effect"},
	}})
	if err != nil || second == nil || !second.IsError || effects.Load() != 1 {
		t.Fatalf("new effect admitted after degraded audit: %+v err=%v effects=%d", second, err, effects.Load())
	}

	// The in-memory ledger still knows the *actual* original receipt, even
	// though its journal finish record could not be written.
	r, replayed, err := manager.Mutate(context.Background(), manager.ID(), "applied-once",
		"big_receipt", args.Argv, func() mutation.Receipt {
			effects.Add(1)
			return mutation.Receipt{Status: "failed"}
		})
	if err != nil || !replayed || r.Status != "applied" || effects.Load() != 1 {
		t.Fatalf("receipt was changed by audit error or effect was rerun: %+v replayed=%v err=%v", r, replayed, err)
	}
}
