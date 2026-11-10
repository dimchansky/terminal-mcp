package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/dashboard"
	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
	"terminalmcp/internal/viewer"
)

func dashboardWindow(t *testing.T, h http.Handler, address, token string) dashboard.Envelope {
	t.Helper()
	w := record(h, viewerAPIRequest(http.MethodPost, address, dashboard.PathWindow, `{}`, token))
	if w.Code != http.StatusOK {
		t.Fatalf("window %d %s", w.Code, w.Body)
	}
	var env dashboard.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func viewerLogin(t *testing.T, viewers *viewer.Registry) string {
	t.Helper()
	code, _, err := viewers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := viewers.Login(code)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestDashboardSeesWorkStartedBeforeLogin: the in-flight index starts at the
// frontier after server_start, so an operation admitted before any viewer
// logged in is reported to the first window.
func TestDashboardSeesWorkStartedBeforeLogin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := audit.PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	recorder, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	manager := session.New(nil)
	t.Cleanup(manager.Close)
	if err := recordServerStart(recorder, manager.ID()); err != nil {
		t.Fatal(err)
	}
	viewers := viewer.NewRegistry(viewer.Config{})
	rt := newViewerRuntime(context.Background(), viewers, recorder, manager.ID())
	for _, e := range []audit.Event{
		{ServerID: manager.ID(), RequestID: "r1", Tool: "file_write", Phase: "request_start", Args: map[string]any{"path": "a.txt"}},
		{ServerID: manager.ID(), RequestID: "r1", OperationID: "op-1", Tool: "file_write", Phase: "operation_start"},
	} {
		if err := recorder.Write(audit.NewEvent(e)); err != nil {
			t.Fatal(err)
		}
	}
	const address = "127.0.0.1:45681"
	h, err := newHTTPHandler(manager, mcpserver.New(manager), address, viewerTestToken, rt)
	if err != nil {
		t.Fatal(err)
	}
	env := dashboardWindow(t, h, address, viewerLogin(t, viewers))
	if env.State != dashboard.StateOK || env.Index == nil || env.Index.Status == "unknown" || env.Health.State != "ok" {
		t.Fatalf("envelope state %s index %+v health %+v", env.State, env.Index, env.Health)
	}
	var running bool
	for _, it := range env.Recent.Items {
		if it.Action != nil && it.Action.Key.RequestID == "r1" && it.Effect != nil {
			running = it.Effect.Key.OperationID == "op-1"
		}
	}
	if !running {
		t.Fatalf("the operation started before login is missing: %+v", env.Recent.Items)
	}
}

func TestDashboardRoutesAnswerWithoutAudit(t *testing.T) {
	manager := session.New(nil)
	t.Cleanup(manager.Close)
	viewers := viewer.NewRegistry(viewer.Config{})
	rt := newViewerRuntime(context.Background(), viewers, nil, manager.ID())
	const address = "127.0.0.1:45682"
	h, err := newHTTPHandler(manager, mcpserver.New(manager), address, viewerTestToken, rt)
	if err != nil {
		t.Fatal(err)
	}
	env := dashboardWindow(t, h, address, viewerLogin(t, viewers))
	if env.State != dashboard.StateDisabled || env.Health.State != "disabled" {
		t.Fatalf("envelope %+v", env)
	}
}

// TestBrowserNeverReceivesTheMCPBearer fetches the page, every asset and
// the dashboard answers the way a browser does and looks for the bearer.
func TestBrowserNeverReceivesTheMCPBearer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := audit.PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	recorder, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	manager := session.New(nil)
	t.Cleanup(manager.Close)
	if err := recordServerStart(recorder, manager.ID()); err != nil {
		t.Fatal(err)
	}
	viewers := viewer.NewRegistry(viewer.Config{})
	rt := newViewerRuntime(context.Background(), viewers, recorder, manager.ID())
	const address = "127.0.0.1:45683"
	h, err := newHTTPHandler(manager, mcpserver.New(manager), address, viewerTestToken, rt)
	if err != nil {
		t.Fatal(err)
	}
	vh, err := newViewerHandler(address, rt)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, route := range vh.Routes() {
		if route.Method != http.MethodGet || route.Auth {
			continue
		}
		r := httptest.NewRequest(http.MethodGet, "http://"+address+route.Path, nil)
		r.Host = address
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Sec-Fetch-Mode", "no-cors")
		r.Header.Set("Sec-Fetch-Dest", "script")
		if strings.HasSuffix(route.Path, ".css") {
			r.Header.Set("Sec-Fetch-Dest", "style")
		}
		if route.Kind == viewer.RoutePage {
			r.Header.Set("Sec-Fetch-Site", "none")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")
		}
		w := record(h, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", route.Path, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	token := viewerLogin(t, viewers)
	w := record(h, viewerAPIRequest(http.MethodPost, address, dashboard.PathWindow, `{}`, token))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"window"`) {
		t.Fatalf("window %d %s", w.Code, w.Body)
	}
	bodies = append(bodies, w.Body.String())
	for _, b := range bodies {
		if strings.Contains(b, viewerTestToken) {
			t.Fatal("the MCP bearer reached the browser")
		}
	}
	if len(bodies) < 5 {
		t.Fatalf("only %d answers checked", len(bodies))
	}
}
