package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"terminalmcp/internal/dashboard"
	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
	"terminalmcp/internal/viewer"
)

const viewerTestToken = "0123456789abcdef0123456789abcdef-viewer"

func viewerAPIRequest(method, address, path, body, token string) *http.Request {
	r := httptest.NewRequest(method, "http://"+address+path, strings.NewReader(body))
	r.Host = address
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Sec-Fetch-Dest", "empty")
	if method != http.MethodGet {
		r.Header.Set("Origin", "http://"+address)
	}
	if token != "" {
		r.Header.Set("Authorization", "Viewer "+token)
	}
	return r
}

func adminRequest(method, address, path string) *http.Request {
	r := httptest.NewRequest(method, "http://"+address+path, nil)
	r.Host = address
	r.Header.Set("Authorization", "Bearer "+viewerTestToken)
	return r
}

func record(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestViewerAndAdminRoutesThroughTheMainHandler(t *testing.T) {
	manager := session.New(nil)
	t.Cleanup(manager.Close)
	const address = "127.0.0.1:45679"
	viewers := viewer.NewRegistry(viewer.Config{})
	h, err := newHTTPHandler(manager, mcpserver.New(manager), address, viewerTestToken, &viewerRuntime{registry: viewers})
	if err != nil {
		t.Fatal(err)
	}

	// The owner's CLI asks for a code with the MCP bearer.
	w := record(h, adminRequest(http.MethodPost, address, viewer.AdminGrantPath))
	var grant struct {
		Code string `json:"code"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &grant) != nil || grant.Code == "" {
		t.Fatalf("grant %d %s", w.Code, w.Body)
	}
	if w := record(h, adminRequest(http.MethodGet, address, viewer.AdminGrantPath)); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET grant %d", w.Code)
	}
	noBearer := adminRequest(http.MethodPost, address, viewer.AdminGrantPath)
	noBearer.Header.Del("Authorization")
	if w := record(h, noBearer); w.Code != http.StatusUnauthorized {
		t.Fatalf("grant without bearer %d", w.Code)
	}
	fromBrowser := adminRequest(http.MethodPost, address, viewer.AdminGrantPath)
	fromBrowser.Header.Set("Origin", "http://"+address)
	if w := record(h, fromBrowser); w.Code != http.StatusForbidden {
		t.Fatalf("grant with Origin %d", w.Code)
	}

	// The browser exchanges it for a viewer token.
	w = record(h, viewerAPIRequest(http.MethodPost, address, viewer.Prefix+"api/login", `{"code":"`+grant.Code+`"}`, ""))
	var login struct {
		Token string `json:"token"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &login) != nil {
		t.Fatalf("login %d %s", w.Code, w.Body)
	}
	if w := record(h, viewerAPIRequest(http.MethodGet, address, viewer.Prefix+"api/session", "", login.Token)); w.Code != http.StatusOK {
		t.Fatalf("session %d", w.Code)
	}

	// Credentials never cross between the guards.
	mcp := httptest.NewRequest(http.MethodPost, "http://"+address+"/mcp", strings.NewReader("{}"))
	mcp.Host = address
	mcp.Header.Set("Authorization", "Viewer "+login.Token)
	if w := record(h, mcp); w.Code != http.StatusUnauthorized {
		t.Fatalf("viewer token on /mcp %d", w.Code)
	}
	bearerOnViewer := viewerAPIRequest(http.MethodGet, address, viewer.Prefix+"api/session", "", "")
	bearerOnViewer.Header.Set("Authorization", "Bearer "+viewerTestToken)
	if w := record(h, bearerOnViewer); w.Code != http.StatusForbidden {
		t.Fatalf("MCP bearer on a viewer route %d", w.Code)
	}

	// The owner revokes every viewer.
	if w := record(h, adminRequest(http.MethodPost, address, viewer.AdminRevokePath)); w.Code != http.StatusNoContent {
		t.Fatalf("revoke %d", w.Code)
	}
	if w := record(h, viewerAPIRequest(http.MethodGet, address, viewer.Prefix+"api/session", "", login.Token)); w.Code != http.StatusUnauthorized {
		t.Fatalf("session after revoke %d", w.Code)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestTunnelCannotReachViewerRoutes enumerates the viewer route table and the
// viewer admin paths: the embedded tunnel transport must refuse every one,
// whatever credentials the remote side presents.
func TestTunnelCannotReachViewerRoutes(t *testing.T) {
	const address = "127.0.0.1:45680"
	h, err := newViewerHandler(address, &viewerRuntime{registry: viewer.NewRegistry(viewer.Config{})})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/dashboard", viewer.AdminGrantPath, viewer.AdminRevokePath}
	for _, rt := range h.Routes() {
		paths = append(paths, rt.Path)
	}
	reached := false
	tunnel := tunnelRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		reached = true
		return nil, errors.New("must not be reached")
	}), address: address, token: viewerTestToken}
	for _, path := range paths {
		for _, auth := range []string{"Bearer " + viewerTestToken, "Viewer some-token", ""} {
			req, _ := http.NewRequest(http.MethodPost, "http://"+address+path, nil)
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := tunnel.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "unexpected local endpoint") {
				t.Fatalf("tunnel reached %s: %v", path, err)
			}
		}
	}
	stream := false
	for _, rt := range h.Routes() {
		stream = stream || rt.Path == dashboard.PathStream
	}
	if reached || !stream || len(paths) != len(h.Routes())+3 || len(h.Routes()) != 17 { // 7 viewer + 10 dashboard routes.
		t.Fatalf("reached=%v paths=%v", reached, paths)
	}
}

func TestViewerLoginWorksDuringDrain(t *testing.T) {
	manager := session.New(nil)
	t.Cleanup(manager.Close)
	const address = "127.0.0.1:45681"
	h, err := newHTTPHandler(manager, mcpserver.New(manager), address, viewerTestToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginDrain(manager.ID(), "stop"); err != nil {
		t.Fatal(err)
	}
	w := record(h, adminRequest(http.MethodPost, address, viewer.AdminGrantPath))
	var grant struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &grant)
	if w := record(h, viewerAPIRequest(http.MethodPost, address, viewer.Prefix+"api/login", `{"code":"`+grant.Code+`"}`, "")); w.Code != http.StatusOK {
		t.Fatalf("login during drain %d %s", w.Code, w.Body)
	}
}

// streamRoute registers a test-only stream that writes until stopped and
// reports how it ended.
func streamRoute(t *testing.T, started chan<- struct{}, ended chan<- *viewer.Stream, written *atomic.Int64) {
	t.Helper()
	testViewerRoutes = func(h *viewer.Handler) {
		h.HandleAuthorized(http.MethodGet, viewer.Prefix+"api/test-stream", func(w http.ResponseWriter, _ *http.Request, s *viewer.Session) {
			st := viewer.NewStream(w)
			release, err := s.Attach(viewer.KindStream, st)
			if err != nil {
				viewer.WriteError(w, err)
				return
			}
			defer release()
			_ = st.Extend(time.Time{}) // Tests only: no per-write deadline.
			started <- struct{}{}
			chunk := []byte(strings.Repeat("x", 64<<10))
			for {
				n, err := w.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					break
				}
			}
			ended <- st
		})
	}
	t.Cleanup(func() { testViewerRoutes = nil })
}

func TestShutdownFencesAViewerStreamWhoseClientStoppedReading(t *testing.T) {
	for _, mode := range []string{"stop", "update"} {
		t.Run(mode, func(t *testing.T) {
			started, ended := make(chan struct{}, 1), make(chan *viewer.Stream, 1)
			var written atomic.Int64
			streamRoute(t, started, ended, &written)
			manager := session.New(nil)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			viewers := viewer.NewRegistry(viewer.Config{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- serveHTTPWithOptions(ctx, manager, mcpserver.New(manager), listener, viewerTestToken, 0, nil, &viewerRuntime{registry: viewers})
			}()
			code, _, _ := viewers.Issue()
			token, _, err := viewers.Login(code)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_, _ = fmt.Fprintf(conn, "GET %sapi/test-stream HTTP/1.1\r\nHost: %s\r\nSec-Fetch-Site: same-origin\r\n"+
				"Sec-Fetch-Mode: cors\r\nSec-Fetch-Dest: empty\r\nAuthorization: Viewer %s\r\n\r\n", viewer.Prefix, address, token)
			<-started
			for last, i := written.Load(), 0; i < 100; i++ { // Wait for the writes to stall.
				time.Sleep(50 * time.Millisecond)
				if now := written.Load(); now == last {
					break
				} else {
					last = now
				}
			}
			if mode == "update" {
				if err := manager.PrepareRestart(manager.ID()); err != nil {
					t.Fatal(err)
				}
			}
			stopAt := time.Now()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("a non-reading viewer held up shutdown")
			}
			// Admission was fenced before Shutdown: a new viewer request is
			// refused (or its connection is, once the listener closed).
			if resp, err := http.DefaultClient.Do(viewerClientRequest(address, token)); err == nil {
				if resp.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("viewer request after stop: %d", resp.StatusCode)
				}
				_ = resp.Body.Close()
			}
			st := <-ended
			stopped, deadline, cause := st.Stopped()
			var stop *viewer.StopError
			if !stopped || !errors.As(cause, &stop) || string(stop.Reason) != mode {
				t.Fatalf("stopped=%v cause=%v", stopped, cause)
			}
			if grace := deadline.Sub(stopAt); grace < viewerStopGrace/2 || grace > viewerStopGrace+5*time.Second {
				t.Fatalf("grace %v", grace)
			}
		})
	}
}

func viewerClientRequest(address, token string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://"+address+viewer.Prefix+"api/session", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Sec-Fetch-Dest", "empty")
	r.Header.Set("Authorization", "Viewer "+token)
	return r
}

func TestForceStopEndsViewersFirst(t *testing.T) {
	viewers := viewer.NewRegistry(viewer.Config{})
	code, _, _ := viewers.Issue()
	_, s, _ := viewers.Login(code)
	var order []string
	force := forceStop(viewers, func() {
		var stop *viewer.StopError
		if errors.As(context.Cause(s.Context()), &stop) && stop.Reason == viewer.StopForce {
			order = append(order, "viewers")
		}
		order = append(order, "abort")
	}, func() { order = append(order, "cancel") })
	force()
	if strings.Join(order, ",") != "viewers,abort,cancel" {
		t.Fatalf("order %v", order)
	}
	viewers.CloseAdmission(viewer.StopNormal) // The later normal fence keeps "force".
	if _, _, err := viewers.Issue(); !errors.Is(err, viewer.ErrStopping) {
		t.Fatalf("issue after force: %v", err)
	}
}

func TestAbnormalServeExitFencesViewers(t *testing.T) {
	manager := session.New(nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	viewers := viewer.NewRegistry(viewer.Config{})
	code, _, _ := viewers.Issue()
	_, s, _ := viewers.Login(code)
	done := make(chan error, 1)
	go func() {
		done <- serveHTTPWithOptions(context.Background(), manager, mcpserver.New(manager), listener, viewerTestToken, 0, nil, &viewerRuntime{registry: viewers})
	}()
	time.Sleep(50 * time.Millisecond)
	_ = listener.Close() // Serve fails.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve reported a clean exit after its listener failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	var stop *viewer.StopError
	if !errors.As(context.Cause(s.Context()), &stop) || stop.Reason != viewer.StopAbnormal {
		t.Fatalf("cause %v", context.Cause(s.Context()))
	}
}

func TestRuntimeReportsViewerCounts(t *testing.T) {
	manager := session.New(nil)
	t.Cleanup(manager.Close)
	const address = "127.0.0.1:45682"
	viewers := viewer.NewRegistry(viewer.Config{})
	code, _, _ := viewers.Issue()
	_, _, _ = viewers.Login(code)
	_, _, _ = viewers.Issue()
	state := &updateServer{info: mcpserver.RuntimeInfo{ServerID: manager.ID()}, shutdown: func() {}}
	h, err := newHTTPHandler(manager, mcpserver.New(manager), address, viewerTestToken, &viewerRuntime{registry: viewers}, state)
	if err != nil {
		t.Fatal(err)
	}
	w := record(h, adminRequest(http.MethodGet, address, "/admin/runtime"))
	var info mcpserver.RuntimeInfo
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &info) != nil ||
		info.ViewerSessions != 1 || info.ViewerPendingCodes != 1 {
		t.Fatalf("runtime %d %s", w.Code, w.Body)
	}
}
