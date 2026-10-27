//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
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
		{name: "serve alias", args: []string{"serve", "--transport=stdio"}},
		{name: "all flags", args: []string{"--transport=stdio", "--file-root=/tmp", "--audit-file=off", "--audit-max-bytes=131072", "--listen=127.0.0.1:8123", "--token-file=/tmp/token"}},
		{name: "explicit unlimited history", args: []string{"--audit-max-bytes=0"}},
		{name: "negative history budget", args: []string{"--audit-max-bytes=-1"}, wantErr: "audit-max-bytes must be non-negative"},
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
				if got.transport != "http" || got.listen != "127.0.0.1:8090" || got.fileRoot != "" || got.auditFile != "" || got.auditMaxBytes != audit.DefaultMaxHistoryBytes {
					t.Fatalf("unexpected defaults: %+v", got)
				}
			case "serve alias":
				if got.transport != "stdio" {
					t.Fatalf("serve subcommand was not parsed: %+v", got)
				}
			case "explicit unlimited history":
				if got.auditMaxBytes != 0 {
					t.Fatalf("explicit unlimited limit changed: %+v", got)
				}
			case "all flags":
				if got.transport != "stdio" || got.listen != "127.0.0.1:8123" ||
					got.tokenFile != "/tmp/token" || got.fileRoot != "/tmp" || got.auditFile != "off" || got.auditMaxBytes != 131072 {
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
		{name: "missing explicit token", args: []string{"--file-root=" + dir, "--audit-file=off", "--listen=127.0.0.1:0", "--token-file=" + filepath.Join(dir, "missing")}, wantErr: "no such file"},
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
	if err != nil || len(tools.Tools) != 14 {
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
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(copy)
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
	if err != nil || result == nil || result.IsError {
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

func TestServeHTTPDrainsIdleManagedSessions(t *testing.T) {
	release := filepath.Join(t.TempDir(), "child-finish")
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("continue"), 0600) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	manager := session.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- serveHTTP(ctx, manager, mcpserver.New(manager), listener, testToken)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(6 * time.Second):
			t.Error("HTTP server did not stop during cleanup")
		}
	})

	requestCtx, requestCancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer requestCancel()
	client := connectHTTP(t, requestCtx, listener.Addr().String())
	list := mcpList(t, requestCtx, client)
	started := mcpCall[session.ActionResult](t, requestCtx, client, "terminal_start", session.StartArgs{
		ServerID: list.ServerID, OperationID: "shutdown-child",
		Argv: []string{"/bin/sh", "-c", "while [ ! -f \"$1\" ]; do sleep 0.05; done", "sh", release},
		Mode: "pipes", WaitMS: 0, CloseOnExit: true,
	})
	if started.Receipt.Status != "applied" || started.Receipt.SessionID == "" {
		t.Fatalf("could not start long-lived process: %+v", started)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close MCP client before shutdown: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("HTTP shutdown interrupted a live child: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HTTP shutdown: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("HTTP shutdown did not finish after the child exited")
	}
	output, err := manager.Read(context.Background(), session.ReadArgs{
		SessionID: started.Receipt.SessionID, WaitFor: "completion", WaitMS: 0,
	})
	if err != nil || output.Session.Exit == nil || !output.Session.Closed {
		t.Fatalf("managed process was not closed: %+v, %v", output.Session, err)
	}
}

func TestRunWithContextStdioNoAuditRequired(t *testing.T) {
	// Stdio defaults to disabled auditing unless an explicit path is selected.
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	fileRoot := t.TempDir()
	go func() {
		done <- runWithContext(ctx, []string{"--transport=stdio", "--file-root=" + fileRoot}, serverTransport)
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
			t.Fatalf("stdio failed when audit was not enabled: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdio did not stop")
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

func TestRunWithContextMacOSIgnoresRetiredConfig(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the retired controller path is macOS-specific")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TERMINAL_MCP_TOKEN", testToken)
	legacyDir := filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	legacyConfig := filepath.Join(legacyDir, "config.json")
	args := []string{"--transport=http", "--listen=127.0.0.1:0", "--audit-file=off"}
	for _, old := range []string{
		`{"file_root":"relative-path"}`,
		`{broken-json`,
	} {
		if err := os.WriteFile(legacyConfig, []byte(old), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := runWithContext(ctx, args, nil); err != nil {
			t.Fatalf("retired config %q affected default HTTP startup: %v", old, err)
		}
		override := append(append([]string(nil), args...), "--file-root="+t.TempDir())
		if err := runWithContext(ctx, override, nil); err != nil {
			t.Fatalf("retired config %q affected explicit file root: %v", old, err)
		}
	}
	invalid := append(append([]string(nil), args...), "--file-root="+filepath.Join(home, "missing"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runWithContext(ctx, invalid, nil); err == nil {
		t.Fatal("an invalid explicitly configured file root was accepted")
	}
}
