//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "defaults"},
		{name: "all flags", args: []string{"--transport=stdio", "--file-root=/tmp", "--audit-file=off", "--listen=127.0.0.1:8123", "--token-file=/tmp/token"}},
		{name: "invalid transport", args: []string{"--transport=other"}, wantErr: "transport must be http or stdio"},
		{name: "unknown flag", args: []string{"--unknown"}, wantErr: "flag provided but not defined"},
		{name: "positional argument", args: []string{"unexpected"}, wantErr: "unexpected positional arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOptions(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseOptions(%v) err = %v, want %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch tt.name {
			case "defaults":
				if got.transport != "http" || got.listen != "127.0.0.1:8090" || got.fileRoot != "" || got.auditFile != "" {
					t.Fatalf("unexpected defaults: %+v", got)
				}
			case "all flags":
				if got.transport != "stdio" || got.listen != "127.0.0.1:8123" ||
					got.tokenFile != "/tmp/token" || got.fileRoot != "/tmp" || got.auditFile != "off" {
					t.Fatalf("flags were not applied: %+v", got)
				}
			}
		})
	}
	if _, err := parseOptions([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help returned %v, want flag.ErrHelp", err)
	}
	// A second parse must not retain values from the first.
	got, err := parseOptions(nil)
	if err != nil || got.transport != "http" {
		t.Fatalf("global flag state leaked across parses: %+v %v", got, err)
	}
}

func TestRunWithContextStartupErrors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "invalid CLI transport", args: []string{"--transport=bad"}, wantErr: "transport must be"},
		{name: "missing root", args: []string{"--transport=stdio", "--file-root=" + filepath.Join(dir, "missing"), "--audit-file=off"}, wantErr: "no such file"},
		{name: "non-loopback address", args: []string{"--file-root=" + dir, "--audit-file=off", "--listen=0.0.0.0:0"}, wantErr: "only literal loopback"},
		{name: "no token", args: []string{"--file-root=" + dir, "--audit-file=off", "--listen=127.0.0.1:0"}, wantErr: "token"},
	}
	t.Setenv("TERMINAL_MCP_TOKEN", "")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWithContext(context.Background(), tt.args, nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantErr)) {
				t.Fatalf("runWithContext error %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunWithContextStdioToolsAndExit(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	fileRoot := t.TempDir()
	go func() {
		done <- runWithContext(ctx, []string{
			"--transport=stdio", "--file-root=" + fileRoot, "--audit-file=off",
		}, serverTransport)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-lifecycle-test", Version: "1"}, nil)
	conn, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	tools, err := conn.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 12 {
		t.Fatalf("stdio MCP tool discovery: %v, tools %v", err, len(tools.Tools))
	}
	list := mcpList(t, ctx, conn)
	if list.ServerID == "" {
		t.Fatal("stdio terminal_list did not return server ID")
	}
	started := mcpCall[session.ActionResult](t, ctx, conn, "terminal_start", session.StartArgs{
		ServerID: list.ServerID, OperationID: "stdio-command",
		Argv:        []string{"/bin/sh", "-c", "printf stdio-ok"},
		CloseOnExit: true, WaitFor: "completion", WaitMS: 3000,
	})
	if started.Receipt.Status != "applied" || started.Observation == nil || !started.Observation.Session.Closed {
		t.Fatalf("stdio did not execute and close a command: %+v", started)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stdio server did not exit normally: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdio server did not stop after cancellation")
	}
}

type authenticatedTransport struct {
	token string
}

func (a authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connectHTTP(t *testing.T, ctx context.Context, address string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "http-lifecycle-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "http://" + address + "/mcp",
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
		HTTPClient:           &http.Client{Timeout: 4 * time.Second, Transport: authenticatedTransport{token: testToken}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func mcpCall[T any](t *testing.T, ctx context.Context, client *mcp.ClientSession, name string, args any) T {
	t.Helper()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || result.IsError {
		t.Fatalf("MCP %s: %v, result %+v", name, err, result)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output T
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func mcpList(t *testing.T, ctx context.Context, client *mcp.ClientSession) session.ListResult {
	t.Helper()
	return mcpCall[session.ListResult](t, ctx, client, "terminal_list", struct{}{})
}

func TestServeHTTPAuthenticationAndShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	manager := session.New(nil)
	defer manager.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, manager, mcpserver.New(manager), listener, testToken) }()

	endpoint := "http://" + listener.Addr().String() + "/mcp"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP without bearer token: status %d", res.StatusCode)
	}

	client := connectHTTP(t, context.Background(), listener.Addr().String())
	tools, err := client.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 7 {
		t.Fatalf("HTTP tool discovery: %v, count %d", err, len(tools.Tools))
	}
	list := mcpList(t, context.Background(), client)
	started := mcpCall[session.ActionResult](t, context.Background(), client, "terminal_start", session.StartArgs{
		ServerID:    list.ServerID,
		OperationID: "shutdown-child",
		Argv:        []string{"/bin/sh", "-c", "sleep 60"},
		Mode:        "pipes",
		WaitMS:      0,
	})
	if started.Receipt.Status != "applied" || started.Receipt.SessionID == "" {
		t.Fatalf("could not start long-lived process: %+v", started)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close MCP client before graceful HTTP shutdown: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveHTTP cancellation: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("HTTP shutdown blocked with active child")
	}
	output, err := manager.Read(context.Background(), session.ReadArgs{
		SessionID: started.Receipt.SessionID, WaitFor: "completion", WaitMS: 0,
	})
	if err != nil || output.Session.Exit == nil || !output.Session.Closed {
		t.Fatalf("child not closed on HTTP shutdown: %+v, err %v", output.Session, err)
	}
}

func TestRunWithContextStdioNoAuditRequired(t *testing.T) {
	// Keep stdio available when a caller selects a bad optional audit path.
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	fileRoot := t.TempDir()
	go func() {
		done <- runWithContext(ctx, []string{"--transport=stdio", "--file-root=" + fileRoot, "--audit-file=/dev/null/audit.jsonl"}, serverTransport)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-audit-test", Version: "1"}, nil)
	conn, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = mcpList(t, ctx, conn)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stdio failed due to optional audit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdio did not stop")
	}
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("TERMINAL_MCP_TEST_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWithContext(ctx, args, nil); err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestCLISIGTERMStopsProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level integration test")
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(testToken), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$", "--",
		"--transport=http", "--listen="+address, "--file-root="+t.TempDir(),
		"--audit-file=off", "--token-file="+tokenFile)
	cmd.Env = append(os.Environ(), "TERMINAL_MCP_TEST_HELPER=1")
	stderrFile, err := os.CreateTemp(t.TempDir(), "helper-stderr-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderrFile.Close() }()
	cmd.Stderr = stderrFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	finished := make(chan struct{})
	go func() { waited <- cmd.Wait(); close(finished) }()
	t.Cleanup(func() {
		select {
		case <-finished:
		default:
			_ = cmd.Process.Kill()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
			}
		}
	})
	stderrText := func() string { data, _ := os.ReadFile(stderrFile.Name()); return string(data) }
	deadline := time.Now().Add(8 * time.Second)
	connected := false
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			connected = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !connected {
		t.Fatalf("helper never started listener: %s", stderrText())
	}
	client := connectHTTP(t, context.Background(), address)
	list := mcpList(t, context.Background(), client)
	started := mcpCall[session.ActionResult](t, context.Background(), client, "terminal_start", session.StartArgs{
		ServerID: list.ServerID, OperationID: "sigterm-managed-child",
		Argv: []string{"/bin/sleep", "60"}, Mode: "pipes", WaitMS: 0,
	})
	if started.Receipt.Status != "applied" || started.Observation == nil {
		t.Fatalf("could not launch child for SIGTERM test: %+v", started)
	}
	childPID := started.Observation.Session.PID
	if err := client.Close(); err != nil {
		t.Fatalf("close MCP client before SIGTERM: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("helper failed on SIGTERM: %v; %s", err, stderrText())
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("helper did not stop after SIGTERM; %s", stderrText())
	}
	if childPID <= 1 || !errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH) {
		t.Fatalf("managed child PID %d survived SIGTERM shutdown", childPID)
	}
}

func TestServeHTTPRejectsBadTokenWithoutStarting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	manager := session.New(nil)
	defer manager.Close()
	err = serveHTTP(context.Background(), manager, mcpserver.New(manager), listener, "bad")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("serveHTTP bad token: %v", err)
	}
	if err := listener.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener was not released: %v", err)
	}
}

func TestServeHTTPPropagatesListenerError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	manager := session.New(nil)
	defer manager.Close()
	err = serveHTTP(context.Background(), manager, mcpserver.New(manager), listener, testToken)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed listener error was lost: %v", err)
	}
}

func TestRunWithContextMacOSConfiguredRoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("managed config file path is macOS-specific")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TERMINAL_MCP_TOKEN", testToken)
	dir := filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"file_root":"relative-path"}`), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--transport=http", "--listen=127.0.0.1:0", "--audit-file=off"}
	if err := runWithContext(context.Background(), args, nil); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative root in macOS config was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	override := append(append([]string(nil), args...), "--file-root="+t.TempDir())
	if err := runWithContext(ctx, override, nil); err != nil {
		t.Fatalf("explicit file root did not override managed config: %v", err)
	}
}
