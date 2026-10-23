//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
)

// TestServeHTTPDrainsActiveMCPRequest confirms an accepted request is allowed
// to finish when the server is asked to shut down.

func TestServeHTTPForcesCloseAfterDrainDeadline(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	manager := session.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveHTTPWithTimeout(ctx, manager, mcpserver.New(manager), listener, testToken, 150*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		manager.Close()
	})

	requestCtx, requestCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer requestCancel()
	client := connectHTTP(t, requestCtx, address)
	list := mcpList(t, requestCtx, client)
	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	callDone := make(chan callResult, 1)
	go func() {
		result, err := client.CallTool(requestCtx, &mcp.CallToolParams{
			Name: "terminal_start",
			Arguments: session.StartArgs{
				ServerID: list.ServerID, OperationID: "shutdown-stuck-request",
				Argv: []string{"/bin/sh", "-c", "printf ready > \"$1\"; sleep 60", "sh", ready},
				Mode: "pipes", WaitFor: "completion", WaitMS: 9000,
			},
		})
		callDone <- callResult{result, err}
	}()
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tool was not admitted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected shutdown timeout, got %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("server did not force-close a stuck request")
	}
	select {
	case <-callDone:
		// The forced close may surface an EOF or cancellation through the SDK.
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP client did not observe closed connection")
	}
	check, err := manager.List(context.Background(), session.ListArgs{IncludeClosed: true})
	if err != nil || check.Counts.Running != 0 || check.Counts.ExitedUnclosed != 0 {
		t.Fatalf("managed processes survived HTTP shutdown: %+v, %v", check, err)
	}
	conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("server still accepts new connections")
	}
}

func TestServeHTTPDrainsActiveMCPRequest(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	manager := session.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, manager, mcpserver.New(manager), listener, testToken) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("shutdown cleanup: %v", err)
			}
		case <-time.After(7 * time.Second):
			t.Error("HTTP server leaked at cleanup")
		}
		manager.Close()
	})

	requestCtx, requestCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer requestCancel()
	client := connectHTTP(t, requestCtx, address)
	list := mcpList(t, requestCtx, client)
	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	got := make(chan callResult, 1)
	go func() {
		result, err := client.CallTool(requestCtx, &mcp.CallToolParams{
			Name: "terminal_start",
			Arguments: session.StartArgs{
				ServerID:    list.ServerID,
				OperationID: "shutdown-inflight-request",
				Argv:        []string{"/bin/sh", "-c", "printf ready > \"$1\"; while [ ! -e \"$2\" ]; do sleep 0.02; done; printf request-complete", "sh", ready, release},
				CloseOnExit: true,
				WaitFor:     "completion",
				WaitMS:      4000,
			},
		})
		got <- callResult{result, err}
	}()
	pollUntil := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(pollUntil) {
			t.Fatal("terminal tool never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Cancellation starts HTTP draining. It must not abort this accepted call.
	cancel()
	if err := os.WriteFile(release, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-got:
		if call.err != nil || call.result == nil || call.result.IsError {
			t.Fatalf("in-flight request was aborted: result=%+v err=%v", call.result, call.err)
		}
		bytes, err := json.Marshal(call.result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out session.ActionResult
		if err := json.Unmarshal(bytes, &out); err != nil {
			t.Fatal(err)
		}
		if out.Receipt.Status != "applied" || out.Observation == nil ||
			out.Observation.Session.Exit == nil || out.Observation.Session.Exit.Code == nil ||
			*out.Observation.Session.Exit.Code != 0 || !out.Observation.Session.Closed {
			t.Fatalf("in-flight tool did not complete successfully: %+v", out)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("in-flight HTTP tool did not return")
	}
}
