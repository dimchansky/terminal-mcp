package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
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
