//go:build linux || darwin

package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/mutation"
	"terminalmcp/internal/session"
)

func readLifecycleAudit(t *testing.T, path string) []audit.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []audit.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var event audit.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid JSONL record: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func TestMCPRequestCancellationDoesNotMisreportCompletedMutation(t *testing.T) {
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
	server := mcp.NewServer(&mcp.Implementation{Name: "audited-lifecycle-test", Version: "1"}, nil)
	server.AddReceivingMiddleware(auditToolRequests(recorder, manager.ID()))

	effectStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		manager.Close()
		if err := recorder.Close(); err != nil {
			t.Error(err)
		}
	}()
	var effects atomic.Int32
	type output struct {
		Status   string `json:"status"`
		Replayed bool   `json:"replayed"`
	}
	// A synthetic effect uses real session.StartArgs for the same input
	// projection as terminal_start; a controllable barrier simulates a slow
	// accepted mutation with a canceled MCP request.
	add(server, recorder, manager.ID(), "controlled_mutation", "Controlled mutation for audit lifecycle tests", false,
		func(ctx context.Context, args session.StartArgs) (output, error) {
			operationCtx := ctx
			if args.WaitMS == 1 {
				var cancel context.CancelFunc
				operationCtx, cancel = context.WithCancel(ctx)
				defer cancel()
				go func() {
					select {
					case <-effectStarted:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			result, replayed, err := manager.Mutate(operationCtx, args.ServerID, args.OperationID,
				"controlled_mutation", args.Argv, func() mutation.Receipt {
					effects.Add(1)
					close(effectStarted)
					<-release
					return mutation.Receipt{Status: "applied", Data: json.RawMessage(`{"effect":"executed"}`)}
				})
			return output{Status: result.Status, Replayed: replayed}, err
		})

	one, two := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	servConn, err := server.Connect(ctx, two, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = servConn.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "audit-test-client", Version: "1"}, nil).Connect(ctx, one, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	initial := session.StartArgs{ServerID: manager.ID(), OperationID: "controlled-1", Argv: []string{"fake", "effect"}, WaitMS: 1}
	firstResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "controlled_mutation", Arguments: initial})
	if err != nil || firstResult == nil || !firstResult.IsError {
		t.Fatalf("cancelled request must report an MCP error: result=%+v err=%v", firstResult, err)
	}

	// The *request* has ended, but the accepted mutation still awaits release.
	before := readLifecycleAudit(t, path)
	var firstRequestID string
	var requestStart, requestFinish, operationStart, operationFinish int
	for _, e := range before {
		switch e.Phase {
		case "request_start":
			requestStart++
			firstRequestID = e.RequestID
		case "request_finish":
			requestFinish++
			if e.Status != "canceled" || e.Outcome != "unknown" {
				t.Fatalf("canceled request misreported effect outcome: %+v", e)
			}
		case "operation_start":
			operationStart++
		case "operation_finish":
			operationFinish++
		}
	}
	if requestStart != 1 || requestFinish != 1 || operationStart != 1 || operationFinish != 0 || firstRequestID == "" {
		t.Fatalf("incorrect pre-completion lifecycle: %+v", before)
	}
	releaseOnce.Do(func() { close(release) })

	replay := initial
	replay.WaitMS = 0 // Observation differs; effect identity stays the same.
	replayResult := call[output](t, ctx, client, "controlled_mutation", replay)
	if !replayResult.Replayed || replayResult.Status != "applied" || effects.Load() != 1 {
		t.Fatalf("replay reran effect or lost receipt: %+v, calls=%d", replayResult, effects.Load())
	}

	conflict := replay
	conflict.Argv = []string{"fake", "different"}
	conflictResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "controlled_mutation", Arguments: conflict})
	if err != nil || conflictResult == nil || !conflictResult.IsError {
		t.Fatalf("conflicting operation_id was not rejected: result=%+v err=%v", conflictResult, err)
	}

	// The original effect has one start/finish; later requests have their own
	// request IDs but never create a second effect or alter its result.
	manager.Close()
	events := readLifecycleAudit(t, path)
	requests := map[string][]audit.Event{}
	var opEvents []audit.Event
	for _, e := range events {
		if e.SchemaVersion != audit.EventSchemaVersion {
			t.Fatalf("wrong schema version: %+v", e)
		}
		switch e.Phase {
		case "request_start", "request_finish":
			requests[e.RequestID] = append(requests[e.RequestID], e)
		case "operation_start", "operation_finish":
			opEvents = append(opEvents, e)
		default:
			t.Fatalf("unexpected audit event: %+v", e)
		}
	}
	if len(requests) != 3 || len(opEvents) != 2 || opEvents[0].Phase != "operation_start" ||
		opEvents[1].Phase != "operation_finish" || opEvents[1].Status != "applied" ||
		opEvents[0].RequestID != firstRequestID || opEvents[1].RequestID != firstRequestID ||
		opEvents[1].OperationID != "controlled-1" || effects.Load() != 1 {
		t.Fatalf("original effect not reported exactly once: requests=%+v operation=%+v", requests, opEvents)
	}
	for id, pair := range requests {
		if len(pair) != 2 || pair[0].Phase != "request_start" || pair[1].Phase != "request_finish" ||
			pair[0].OperationID != "controlled-1" || pair[1].OperationID != "controlled-1" {
			t.Fatalf("unpaired or uncorrelated request %s: %+v", id, pair)
		}
	}
	if requests[firstRequestID][1].Status != "canceled" || requests[firstRequestID][1].Outcome != "unknown" {
		t.Fatalf("request cancellation overwritten by effect result: %+v", requests[firstRequestID])
	}
}
