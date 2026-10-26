//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
)

func auditTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRequiredAuditStartupFailureDoesNotStartMCP(t *testing.T) {
	dir := t.TempDir()
	parentFile := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(parentFile, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(parentFile, "audit.jsonl")
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			err := runWithContext(context.Background(), []string{
				"--transport=" + transport,
				"--listen=127.0.0.1:0",
				"--file-root=" + dir,
				"--audit-file=" + broken,
			}, nil)
			if err == nil || !strings.Contains(err.Error(), "required audit initialization failed") {
				t.Fatalf("audit failed but %s server did not refuse startup: %v", transport, err)
			}
		})
	}
}

func TestAuditStartupLockIsNotSilentlyBypassed(t *testing.T) {
	path := filepath.Join(auditTestDir(t), "audit.jsonl")
	owner, err := openAudit(path, "stdio")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = openAudit(path, "stdio")
	if !errors.Is(err, audit.ErrInUse) {
		t.Fatalf("second audit owner was accepted or error was hidden: %v", err)
	}
	if got, err := openAudit("off", "http"); err != nil || got != nil {
		t.Fatalf("explicit off did not disable auditing: %v, %v", got, err)
	}
	if got, err := openAudit("", "stdio"); err != nil || got != nil {
		t.Fatalf("stdio default did not preserve intentional opt-out: %v, %v", got, err)
	}
}

func TestServerStartAuditFailureAbortsBeforeServing(t *testing.T) {
	path := filepath.Join(auditTestDir(t), "audit.jsonl")
	recorder, err := openAudit(path, "stdio")
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recordServerStart(recorder, "instance"); !errors.Is(err, audit.ErrClosed) {
		t.Fatalf("failed startup write was not surfaced: %v", err)
	}
}

func TestStandaloneRuntimeAuditHealthAndMaintenanceStillAccessible(t *testing.T) {
	dir := auditTestDir(t)
	recorder, err := openAudit(filepath.Join(dir, "audit.jsonl"), "http")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recorder.Close() }()
	manager := session.New(nil)
	defer manager.Close()
	const address = "127.0.0.1:45678"
	var shutdownCalled bool
	state := &updateServer{
		info:     mcpserver.RuntimeInfo{ServerID: manager.ID()},
		recorder: recorder,
		shutdown: func() { shutdownCalled = true },
	}
	handler, err := newHTTPHandler(manager, mcpserver.NewWithAudit(manager, recorder), address, testToken, state)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://"+address+path, strings.NewReader(body))
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+testToken)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	if got := request(http.MethodGet, "/admin/runtime", "", false); got.Code != http.StatusUnauthorized {
		t.Fatalf("status without bearer accepted: %d", got.Code)
	}
	get := func() mcpserver.RuntimeInfo {
		t.Helper()
		w := request(http.MethodGet, "/admin/runtime", "", true)
		if w.Code != http.StatusOK {
			t.Fatalf("authenticated status returned %d: %s", w.Code, w.Body)
		}
		var info mcpserver.RuntimeInfo
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	if info := get(); info.Audit.State != "ok" || info.Audit.Path == "" || info.Audit.LastError != "" {
		t.Fatalf("incorrect healthy audit status: %+v", info.Audit)
	}
	const fault = "injected metadata write error"
	recorder.Degrade(errors.New(fault))
	if info := get(); info.Audit.State != "degraded" || !strings.Contains(info.Audit.LastError, fault) {
		t.Fatalf("could not diagnose degraded audit: %+v", info.Audit)
	}
	// This is an administrative request outside the MCP audit. A broken
	// audit must not prevent a normal, safe stop of an idle server.
	body := `{"server_id":"` + manager.ID() + `","action":"begin"}`
	if w := request(http.MethodPost, "/admin/maintenance", body, true); w.Code != http.StatusOK {
		t.Fatalf("degraded audit prevented maintenance begin: %d %s", w.Code, w.Body)
	}
	body = `{"server_id":"` + manager.ID() + `","action":"shutdown"}`
	if w := request(http.MethodPost, "/admin/maintenance", body, true); w.Code != http.StatusOK || !shutdownCalled {
		t.Fatalf("degraded audit prevented controlled stop: %d %s", w.Code, w.Body)
	}
}
