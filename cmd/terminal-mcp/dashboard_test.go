package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
	"terminalmcp/internal/viewer"
)

// startDashboardServer serves the real handler on a loopback port.
func startDashboardServer(t *testing.T, handler func(address string) http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	srv := &http.Server{Handler: handler(address)}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return address
}

func dashboardServer(t *testing.T, viewers *viewer.Registry) string {
	t.Helper()
	manager := session.New(nil)
	t.Cleanup(manager.Close)
	return startDashboardServer(t, func(address string) http.Handler {
		h, err := newHTTPHandler(manager, mcpserver.New(manager), address, viewerTestToken, viewers)
		if err != nil {
			t.Fatal(err)
		}
		return h
	})
}

func stubBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	previous := openBrowser
	openBrowser = func(url string) error { opened = append(opened, url); return nil }
	t.Cleanup(func() { openBrowser = previous })
	return &opened
}

var codePattern = regexp.MustCompile(`One-time code: (\S+)`)

func TestDashboardPrintsACodeAndOpensAGrantFreeURL(t *testing.T) {
	t.Setenv("TERMINAL_MCP_TOKEN", viewerTestToken)
	viewers := viewer.NewRegistry(viewer.Config{})
	address := dashboardServer(t, viewers)
	opened := stubBrowser(t)
	var out bytes.Buffer
	if err := dashboardCommand([]string{"--listen", address}, &out); err != nil {
		t.Fatal(err)
	}
	m := codePattern.FindStringSubmatch(out.String())
	if m == nil || !strings.Contains(out.String(), "http://"+address+"/dashboard/") {
		t.Fatalf("output %q", out.String())
	}
	if len(*opened) != 1 || (*opened)[0] != "http://"+address+"/dashboard/" || strings.Contains((*opened)[0], m[1]) {
		t.Fatalf("opened %v", *opened)
	}
	if _, _, err := viewers.Login(m[1]); err != nil {
		t.Fatalf("printed code: %v", err)
	}

	out.Reset()
	if err := dashboardCommand([]string{"--listen", address, "--no-open"}, &out); err != nil || len(*opened) != 1 {
		t.Fatalf("--no-open: %v, opened %v", err, *opened)
	}
}

func TestDashboardRevokeAll(t *testing.T) {
	t.Setenv("TERMINAL_MCP_TOKEN", viewerTestToken)
	viewers := viewer.NewRegistry(viewer.Config{})
	address := dashboardServer(t, viewers)
	code, _, _ := viewers.Issue()
	_, s, _ := viewers.Login(code)
	var out bytes.Buffer
	if err := dashboardCommand([]string{"--listen", address, "--revoke-all"}, &out); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(s.Context()), viewer.ErrRevoked) || !strings.Contains(out.String(), "revoked") {
		t.Fatalf("cause %v output %q", context.Cause(s.Context()), out.String())
	}
}

func TestDashboardExplainsServerProblems(t *testing.T) {
	stubBrowser(t)
	t.Setenv("TERMINAL_MCP_TOKEN", viewerTestToken)

	old := startDashboardServer(t, func(string) http.Handler { return http.NotFoundHandler() })
	stopping := viewer.NewRegistry(viewer.Config{})
	stopping.CloseAdmission(viewer.StopNormal)
	stoppingAddress := dashboardServer(t, stopping)
	healthy := dashboardServer(t, viewer.NewRegistry(viewer.Config{}))
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedAddress := closed.Addr().String()
	_ = closed.Close()

	for _, tt := range []struct {
		name, want string
		args       []string
		token      string
	}{
		{"old server", "does not support the dashboard", []string{"--listen", old}, viewerTestToken},
		{"stopping", "is stopping", []string{"--listen", stoppingAddress}, viewerTestToken},
		{"wrong token", "rejected the bearer token", []string{"--listen", healthy}, strings.Repeat("w", 40)},
		{"not running", "no terminal-mcp server is listening", []string{"--listen", closedAddress}, viewerTestToken},
		{"token without listen", "requires --listen", []string{"--token-file", "x"}, viewerTestToken},
		{"positional", "no positional", []string{"--listen", healthy, "extra"}, viewerTestToken},
		{"not loopback", "loopback", []string{"--listen", "0.0.0.0:8090"}, viewerTestToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TERMINAL_MCP_TOKEN", tt.token)
			var out bytes.Buffer
			err := dashboardCommand(tt.args, &out)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), tt.token) || strings.Contains(out.String(), "One-time code") {
				t.Fatalf("secret or code leaked: %v %q", err, out.String())
			}
		})
	}
}

func TestBrowserCommandsCarryOnlyTheURL(t *testing.T) {
	const url = "http://127.0.0.1:8090/dashboard/"
	for goos, want := range map[string][]string{
		"darwin":  {"open", url},
		"linux":   {"xdg-open", url},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", url},
	} {
		cmd := browserCommand(goos, url)
		if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
			t.Errorf("%s argv %q", goos, cmd.Args)
		}
	}
}

func TestDashboardRejectsInvalidGrantsAndStoppingRevokes(t *testing.T) {
	stubBrowser(t)
	t.Setenv("TERMINAL_MCP_TOKEN", viewerTestToken)
	bad := startDashboardServer(t, func(string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		})
	})
	var out bytes.Buffer
	if err := dashboardCommand([]string{"--listen", bad}, &out); err == nil || !strings.Contains(err.Error(), "invalid dashboard code") {
		t.Fatalf("empty grant: %v", err)
	}
	stopping := viewer.NewRegistry(viewer.Config{})
	address := dashboardServer(t, stopping)
	stopping.CloseAdmission(viewer.StopNormal)
	// Revoke still works while stopping (it only ends sessions).
	if err := dashboardCommand([]string{"--listen", address, "--revoke-all"}, &out); err != nil {
		t.Fatalf("revoke while stopping: %v", err)
	}
	t.Setenv("TERMINAL_MCP_TOKEN", "")
	if err := dashboardCommand([]string{"--listen", address}, &out); err == nil || !strings.Contains(err.Error(), "no bearer token") {
		t.Fatalf("missing token: %v", err)
	}
}
