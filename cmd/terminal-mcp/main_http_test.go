//go:build linux || darwin

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
)

// Handler tests use httptest; server lifecycle tests exercise serveHTTP itself.
func TestHTTPHandlerAuthenticationAndMCP(t *testing.T) {
	manager := session.New(nil)
	t.Cleanup(manager.Close)
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	handler, err := newHTTPHandler(manager, mcpserver.New(manager), server.Listener.Addr().String(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()

	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	unauthed, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(unauthed)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request returned %d, want 401", response.StatusCode)
	}
	client := connectHTTP(t, ctx, server.Listener.Addr().String())
	tools, err := client.ListTools(ctx, nil)
	if err != nil || tools == nil || len(tools.Tools) != 7 {
		t.Fatalf("unexpected HTTP tools: %v, %+v", err, tools)
	}
	list := mcpList(t, ctx, client)
	if list.ServerID != manager.ID() {
		t.Fatalf("HTTP MCP server ID %q differs from instance %q", list.ServerID, manager.ID())
	}
}

// The RoundTripper contract forbids mutations to the original request.
// A downstream transport must see the bearer header on a cloned request.
func TestAuthenticatedTransportDoesNotMutateRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("bearer header missing from transport request: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	req, err := http.NewRequest(http.MethodGet, server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Original", "unchanged")
	resp, err := (authenticatedTransport{token: testToken}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("X-Original") != "unchanged" {
		t.Fatalf("RoundTrip mutated its input headers: %v", req.Header)
	}
}

// Runtime details are private to authenticated standalone HTTP clients.
// A launchd/legacy invocation must not expose this update capability.
func TestStandaloneRuntimeEndpointAuthenticationAndLegacyIsolation(t *testing.T) {
	manager := session.New(nil)
	defer manager.Close()
	const address = "127.0.0.1:45678"
	state := &updateServer{info: mcpserver.RuntimeInfo{ServerID: manager.ID()},
		shutdown: func() {}}
	standalone, err := newHTTPHandler(manager, mcpserver.New(manager), address, testToken, state)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := newHTTPHandler(manager, mcpserver.New(manager), address, testToken)
	if err != nil {
		t.Fatal(err)
	}
	check := func(h http.Handler, auth bool, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://"+address+"/admin/runtime", nil)
		if auth {
			request.Header.Set("Authorization", "Bearer "+testToken)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("GET runtime (auth=%t): status=%d want=%d, response=%s",
				auth, response.Code, want, response.Body.String())
		}
		if want == http.StatusOK && !strings.Contains(response.Body.String(), manager.ID()) {
			t.Fatalf("runtime response lacks expected instance ID: %s", response.Body.String())
		}
	}
	check(standalone, false, http.StatusUnauthorized)
	check(standalone, true, http.StatusOK)
	check(legacy, true, http.StatusNotFound)
}
