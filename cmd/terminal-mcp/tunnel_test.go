package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/tunnel-client/testsupport/mocktunnelservice"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
)

func TestTunnelConfigValidation(t *testing.T) {
	if _, err := parseOptions([]string{"--transport=stdio", "--tunnel"}); err == nil {
		t.Fatal("stdio accepted tunnel")
	}
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "")
	t.Setenv("CONTROL_PLANE_API_KEY", "")
	if _, err := loadTunnelConfig(options{tunnel: true}); err == nil {
		t.Fatal("missing tunnel ID accepted")
	}
	if _, _, err := resolveStandaloneTunnelConfig(true, "tunnel_sdktestaaaaaaaaaaaaaaaaaaaaaaaaa", ""); err == nil {
		t.Fatal("standalone accepted absent key file")
	}
	if conf, err := loadTunnelConfig(options{}); err != nil || conf.apiKey != "" {
		t.Fatalf("disabled tunnel unexpectedly loaded credentials: %+v %v", conf, err)
	}
}

func TestTunnelRoundTripperPinsAuthAndPaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-secret" || r.Header.Get("Origin") != "" ||
			r.Header.Get("X-Forwarded-Host") != "" {
			t.Errorf("unexpected forwarded headers: auth=%q origin=%q forwarded=%q",
				r.Header.Get("Authorization"), r.Header.Get("Origin"), r.Header.Get("X-Forwarded-Host"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	address := strings.TrimPrefix(upstream.URL, "http://")
	tr := tunnelRoundTripper{base: http.DefaultTransport, address: address, token: "local-secret"}
	u := upstream.URL + "/mcp"
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer injected-remote-token")
	req.Header.Set("Origin", "https://untrusted.example")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("code=%d", resp.StatusCode)
	}
	for _, path := range []string{"/admin/runtime", "/mcp/extra", "/mcp?bad=1"} {
		req, _ := http.NewRequest(http.MethodPost, upstream.URL+path, nil)
		resp, err := tr.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatalf("unexpected access to %s", path)
		}
	}
}

func TestEmbeddedTunnelUsesActualMCPAuditAndHTTP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-audit")
	if err := audit.PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	recorder, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := recorder.Close(); err != nil {
			t.Error(err)
		}
	}()
	manager := session.New(nil)
	server := mcpserver.NewWithAudit(manager, recorder)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	managerID := manager.ID()
	const tunnelID = "tunnel_sdktestaaaaaaaaaaaaaaaaaaaaaaaaa"
	const fakeKey = "sdk-test-api-key"
	const callCount = 24
	remoteCalls := make([]mocktunnelservice.CommandResponse, 0, callCount)
	for i := 0; i < callCount; i++ {
		requestID := fmt.Sprintf("remote-terminal-list-%d", i)
		callID := fmt.Sprintf("rpc-%d", i)
		raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"terminal_list","arguments":{}}}`, callID)
		remoteCalls = append(remoteCalls, mocktunnelservice.CommandResponse{
			Command: mocktunnelservice.NewCommand(requestID, json.RawMessage(raw), nil),
			ExpectedResponses: []mocktunnelservice.ExpectedResponse{{
				RequestID: requestID,
				Assert: func(tb testing.TB, response mocktunnelservice.ReceivedResponse) {
					tb.Helper()
					if response.ResponseCode != http.StatusOK || !strings.Contains(string(response.JSONResponse), managerID) {
						tb.Fatalf("remote result %d missing server_id: %s", response.ResponseCode, response.JSONResponse)
					}
				},
			}},
		})
	}
	controlPlane := mocktunnelservice.NewMockTunnelService(
		mocktunnelservice.WithAPIKey(fakeKey), mocktunnelservice.WithTunnelID(tunnelID),
		mocktunnelservice.WithInitializationPhaseCommandsWithoutSessionHeaders(),
		mocktunnelservice.WithCommandResponses(remoteCalls...),
	)
	controlPlane.Start(t)
	// SDK.Start must not require a live OpenAI control plane. Serve local
	// MCP immediately, then let polling recover when the endpoint returns.
	controlPlane.SetPollStatus(http.StatusServiceUnavailable)
	runtime, err := newTunnelRuntime(tunnelConfig{
		id: tunnelID, apiKey: fakeKey, controlPlaneBaseURL: controlPlane.BaseURL().String(),
		pollTimeout: 10 * time.Millisecond, logWriter: io.Discard,
	}, listener.Addr().String(), tunnelTestToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveHTTPWithTunnel(ctx, manager, server, listener, tunnelTestToken, runtime) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("HTTP server failed: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("embedded tunnel/HTTP shutdown leaked")
		}
	}()
	timeoutCtx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	if err := controlPlane.WaitForPollFailures(timeoutCtx, 1); err != nil {
		t.Fatalf("embedded SDK failed to poll an unavailable control plane: %v", err)
	}
	preRecoveryClient := tunnelConnectHTTP(t, timeoutCtx, listener.Addr().String())
	if state := tunnelList(t, timeoutCtx, preRecoveryClient); state.ServerID != managerID {
		t.Fatalf("local MCP not serving before first successful poll: %+v", state)
	}
	if status := runtime.status().State; status != "starting" {
		t.Fatalf("incorrect tunnel state during initial control-plane outage: %s", status)
	}
	controlPlane.SetPollStatus(0)
	if err := controlPlane.WaitUntilIdle(timeoutCtx); err != nil {
		t.Fatal(err)
	}
	got := controlPlane.ReceivedResponses(mocktunnelservice.ResponseMatchMatched)
	if len(got) != callCount+2 {
		t.Fatalf("matched responses %d, want %d", len(got), callCount+2)
	}
	if state := runtime.status().State; state != "first_poll_succeeded" {
		t.Fatalf("tunnel not ready after successful poll: %s", state)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var starts, finishes int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event struct {
			Phase    string          `json:"phase"`
			Tool     string          `json:"tool"`
			Args     json.RawMessage `json:"args"`
			ServerID string          `json:"server_id"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Tool == "terminal_list" {
			if event.ServerID != managerID {
				t.Errorf("wrong audit server ID: %s", event.ServerID)
			}
			switch event.Phase {
			case "request_start":
				starts++
				if string(event.Args) != "{}" {
					t.Errorf("raw arguments lost: %s", event.Args)
				}
			case "request_finish":
				finishes++
			}
		}
	}
	if starts != callCount+1 || finishes != callCount+1 {
		// The extra pair comes from the authenticated local MCP probe that
		// deliberately runs while the OpenAI control plane is unavailable.
		t.Fatalf("audit pairs %d start %d finish (expected %d)", starts, finishes, callCount+1)
	}
	// Inject a recoverable OpenAI poll outage. The locally authenticated MCP
	// endpoint must remain available while the control plane returns 503.
	controlPlane.SetPollStatus(http.StatusServiceUnavailable)
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer pollCancel()
	if err := controlPlane.WaitForPollFailures(pollCtx, 1); err != nil {
		t.Fatalf("SDK did not encounter the simulated outage: %v", err)
	}
	localCtx, localCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer localCancel()
	localClient := tunnelConnectHTTP(t, localCtx, listener.Addr().String())
	if result := tunnelList(t, localCtx, localClient); result.ServerID != managerID {
		t.Fatalf("local MCP unavailable during control-plane outage: %+v", result)
	}
	controlPlane.SetPollStatus(0)
	if state := runtime.status().State; state != "first_poll_succeeded" {
		t.Fatalf("historical first-poll status incorrectly claims ongoing connectivity: %s", state)
	}
	t.Logf("embedded tunnel passed real HTTP guard, MCP middleware and JSONL audit for server %s", managerID)
}

func TestEmbeddedTunnelShutdownDeliversAcceptedResponse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-audit")
	if err := audit.PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	recorder, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recorder.Close() }()
	manager := session.New(nil)
	server := mcpserver.NewWithAudit(manager, recorder)
	entered := make(chan struct{})
	release := make(chan struct{})
	mcp.AddTool(server, &mcp.Tool{Name: "hold", Description: "wait for test release"}, func(ctx context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, map[string]string, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "completed-through-tunnel"}}}, map[string]string{"ok": "completed-through-tunnel"}, nil
	})
	const tid = "tunnel_sdktestaaaaaaaaaaaaaaaaaaaaaaaaa"
	const apikey = "sdk-test-api-key"
	remoteJSON := "{\"jsonrpc\":\"2.0\",\"id\":\"slow-id\",\"method\":\"tools/call\",\"params\":{\"name\":\"hold\",\"arguments\":{}}}"
	command := mocktunnelservice.CommandResponse{
		Command: mocktunnelservice.NewCommand("slow-command", json.RawMessage(remoteJSON), nil),
		ExpectedResponses: []mocktunnelservice.ExpectedResponse{{
			RequestID: "slow-command",
			Assert: func(tb testing.TB, response mocktunnelservice.ReceivedResponse) {
				tb.Helper()
				if response.ResponseCode != http.StatusOK || !strings.Contains(string(response.JSONResponse), "completed-through-tunnel") {
					tb.Fatalf("lost remote response after drain: %d %s", response.ResponseCode, response.JSONResponse)
				}
			},
		}},
	}
	cp := mocktunnelservice.NewMockTunnelService(
		mocktunnelservice.WithAPIKey(apikey), mocktunnelservice.WithTunnelID(tid),
		mocktunnelservice.WithInitializationPhaseCommandsWithoutSessionHeaders(),
		mocktunnelservice.WithCommandResponses(command),
	)
	cp.Start(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newTunnelRuntime(tunnelConfig{id: tid, apiKey: apikey, controlPlaneBaseURL: cp.BaseURL().String(), pollTimeout: 10 * time.Millisecond, logWriter: io.Discard}, listener.Addr().String(), tunnelTestToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveHTTPWithTunnel(ctx, manager, server, listener, tunnelTestToken, rt) }()
	completed := false
	defer func() {
		cancel()
		closeTestRelease(release)
		if !completed {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("shutdown: %v", err)
				}
			case <-time.After(8 * time.Second):
				t.Error("shutdown leaked")
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(6 * time.Second):
		t.Fatal("remote call not delivered")
	}
	cancel()
	select {
	case err := <-done:
		completed = true
		t.Fatalf("shutdown returned before accepted tool completed: %v", err)
	case <-time.After(120 * time.Millisecond):
	}
	closeTestRelease(release)
	responseCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := cp.WaitUntilIdle(responseCtx); err != nil {
		t.Fatalf("remote response not delivered before graceful shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		completed = true
	case <-time.After(8 * time.Second):
		t.Fatal("draining server did not exit")
	}
}
func closeTestRelease(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// These helpers deliberately live in this platform-neutral test file. The
// ordinary HTTP lifecycle helpers in main_test.go are Unix-only.
const tunnelTestToken = "0123456789abcdef0123456789abcdef"

type tunnelAuthenticatedTransport struct{ token string }

func (tunnelAuth tunnelAuthenticatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	cloned.Header.Set("Authorization", "Bearer "+tunnelAuth.token)
	return http.DefaultTransport.RoundTrip(cloned)
}

func tunnelConnectHTTP(t *testing.T, ctx context.Context, address string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "embedded-tunnel-test", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "http://" + address + "/mcp",
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
		HTTPClient:           &http.Client{Timeout: 4 * time.Second, Transport: tunnelAuthenticatedTransport{token: tunnelTestToken}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func tunnelList(t *testing.T, ctx context.Context, client *mcp.ClientSession) session.ListResult {
	t.Helper()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "terminal_list", Arguments: struct{}{}})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("terminal_list: result=%+v err=%v", result, err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out session.ListResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
