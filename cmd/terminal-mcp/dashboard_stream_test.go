package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
	"terminalmcp/internal/dashboard"
	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
	"terminalmcp/internal/viewer"
)

// liveServer runs the production HTTP server with a real audit and the
// dashboard, as serve does.
type liveServer struct {
	t        *testing.T
	address  string
	manager  *session.Manager
	recorder *audit.Log
	viewers  *viewer.Registry
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{} // Closed when serve returned; err holds its result.
	err      error
}

// smallBuffers makes accepted connections' send buffers tiny, so a client
// that stops reading blocks the server's writes quickly on every platform.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

// liveOptions vary the live server for a test.
type liveOptions struct {
	small    bool   // Tiny send buffers (see smallBuffers).
	fileRoot string // Registers the file tools rooted here.
	capacity int    // Per-window dashboard capacity; 0 is the production default (newViewerRuntime).
}

func startLiveServer(t *testing.T) *liveServer { return startLiveServerWith(t, liveOptions{}) }

func startLiveServerWith(t *testing.T, opts liveOptions) *liveServer {
	t.Helper()
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
	if err := recordServerStart(recorder, manager.ID()); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if opts.small {
		listener = smallBuffers{listener}
	}
	viewers := viewer.NewRegistry(viewer.Config{})
	rt := newViewerRuntime(context.Background(), viewers, recorder, manager.ID())
	if opts.capacity > 0 {
		// The same wiring as newViewerRuntime, with a smaller capacity.
		reader, err := audit.NewReader(recorder)
		if err != nil {
			t.Fatal(err)
		}
		f0, err := reader.FrontierCursor(context.Background())
		index := dashboard.NewIndex(reader, manager.ID(), f0, err == nil, recorder.Status)
		rt.dashboard = dashboard.NewManager(reader, dashboard.ManagerConfig{ServerID: manager.ID(), Capacity: opts.capacity,
			Index: index, Status: recorder.Status})
	}
	var files []*filesystem.Manager
	if opts.fileRoot != "" {
		fm, err := filesystem.New(manager, opts.fileRoot)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, fm)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &liveServer{t: t, address: listener.Addr().String(), manager: manager, recorder: recorder, viewers: viewers,
		listener: listener, cancel: cancel, done: make(chan struct{})}
	go func() {
		s.err = serveHTTPWithOptions(ctx, manager, mcpserver.NewWithAudit(manager, recorder, files...), listener, viewerTestToken, 0, nil, rt)
		close(s.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("serve did not return")
		}
	})
	return s
}

func (s *liveServer) login() string {
	s.t.Helper()
	code, _, err := s.viewers.Issue()
	if err != nil {
		s.t.Fatal(err)
	}
	token, _, err := s.viewers.Login(code)
	if err != nil {
		s.t.Fatal(err)
	}
	return token
}

func (s *liveServer) request(path, token string, body any) *http.Request {
	data, _ := json.Marshal(body)
	r, _ := http.NewRequest(http.MethodPost, "http://"+s.address+path, bytes.NewReader(data))
	for k, v := range map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
		"Origin": "http://" + s.address, "Content-Type": "application/json", "Authorization": "Viewer " + token} {
		r.Header.Set(k, v)
	}
	return r
}

func (s *liveServer) post(path, token string, body, out any) {
	s.t.Helper()
	resp, err := http.DefaultClient.Do(s.request(path, token, body))
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("%s: %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		s.t.Fatal(err)
	}
}

type streamEvent struct {
	name string
	data json.RawMessage
}

// openStream opens a window and its stream; events arrive on the channel,
// which is closed when the stream ends.
func (s *liveServer) openStream(token string) (dashboard.Envelope, <-chan streamEvent) {
	s.t.Helper()
	var env dashboard.Envelope
	s.post(dashboard.PathWindow, token, map[string]any{}, &env)
	resp, err := http.DefaultClient.Do(s.request(dashboard.PathStream, token, //nolint:bodyclose // Closed by the reader goroutine.
		map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor}))
	if err != nil {
		s.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		s.t.Fatalf("stream: %d", resp.StatusCode)
	}
	events := make(chan streamEvent, 1000)
	done := s.t.Context().Done()
	go func() {
		defer close(events)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var ev streamEvent
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			case line == "":
				select {
				case events <- ev:
				case <-done:
					return
				}
				ev = streamEvent{}
			}
		}
	}()
	return env, events
}

// waitEvent returns the next event with the name; ok is false if the stream
// ended first.
func waitEvent(t *testing.T, events <-chan streamEvent, name string, within time.Duration) (streamEvent, bool) {
	t.Helper()
	timeout := time.After(within)
	for {
		select {
		case ev, open := <-events:
			if !open {
				return streamEvent{}, false
			}
			if ev.name == name {
				return ev, true
			}
		case <-timeout:
			t.Fatalf("no %s within %v", name, within)
		}
	}
}

func (s *liveServer) audit(id string) {
	s.t.Helper()
	for _, phase := range []string{"request_start", "request_finish"} {
		e := audit.Event{ServerID: s.manager.ID(), RequestID: id, Tool: "file_read", Phase: phase, Status: "ok"}
		if phase == "request_start" {
			e.Status, e.Args = "", map[string]any{"path": id}
		}
		if err := s.recorder.Write(audit.NewEvent(e)); err != nil {
			s.t.Fatal(err)
		}
	}
}

func TestDashboardStreamEndsWithServerStopping(t *testing.T) {
	for _, mode := range []string{"stop", "update"} {
		t.Run(mode, func(t *testing.T) {
			s := startLiveServer(t)
			_, events := s.openStream(s.login())
			waitEvent(t, events, dashboard.EventReady, 5*time.Second)
			if mode == "update" {
				if err := s.manager.PrepareRestart(s.manager.ID()); err != nil {
					t.Fatal(err)
				}
				if err := s.manager.CanShutdownForUpdate(s.manager.ID()); err != nil {
					t.Fatal(err)
				}
			}
			stopAt := time.Now()
			s.cancel()
			ev, ok := waitEvent(t, events, dashboard.EventServerStopped, 10*time.Second)
			if !ok || !strings.Contains(string(ev.data), `"`+mode+`"`) {
				t.Fatalf("server_stopping %v %s", ok, ev.data)
			}
			if late := time.Since(stopAt); late > viewerStopGrace+3*time.Second {
				t.Fatalf("server_stopping after %v", late)
			}
			select {
			case <-s.done:
				if s.err != nil {
					t.Fatal(s.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown was held up")
			}
		})
	}
}

func TestDashboardStreamEndsOnForceAndAbnormalExits(t *testing.T) {
	t.Run("force", func(t *testing.T) {
		s := startLiveServer(t)
		_, events := s.openStream(s.login())
		waitEvent(t, events, dashboard.EventReady, 5*time.Second)
		start := time.Now()
		forceStop(s.viewers, func() {}, s.cancel)()
		for ev := range events {
			if ev.name == dashboard.EventServerStopped {
				t.Fatalf("force stop wrote %s", ev.data)
			}
		}
		if took := time.Since(start); took > viewerStopGrace/2+2*time.Second {
			t.Fatalf("a forced stop took %v to end the stream", took)
		}
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not return")
		}
	})
	t.Run("abnormal", func(t *testing.T) {
		s := startLiveServer(t)
		_, events := s.openStream(s.login())
		waitEvent(t, events, dashboard.EventReady, 5*time.Second)
		_ = s.listener.Close() // Serve fails; the deferred fence ends viewers.
		for ev := range events {
			if ev.name == dashboard.EventServerStopped {
				t.Fatal("an abnormal exit wrote server_stopping")
			}
		}
		select {
		case <-s.done:
			if s.err == nil {
				t.Fatal("serve returned no error after its listener failed")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not return")
		}
	})
}

// TestDashboardStreamCompleteAcrossRotation: parallel calls while two
// viewers stream, across a writer rotation; every request reaches each
// viewer through notifications plus catch-up.
func TestDashboardStreamCompleteAcrossRotation(t *testing.T) {
	s := startLiveServer(t)
	type viewerState struct {
		token  string
		env    dashboard.Envelope
		events <-chan streamEvent
	}
	var viewers []viewerState
	for i := 0; i < 2; i++ {
		token := s.login()
		env, events := s.openStream(token)
		waitEvent(t, events, dashboard.EventReady, 5*time.Second)
		viewers = append(viewers, viewerState{token, env, events})
	}
	var wg sync.WaitGroup
	var ids []string
	var mu sync.Mutex
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := fmt.Sprintf("p%d-%d", g, i)
				s.audit(id)
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
				if i == 10 && g == 0 { // Cross the 4 MiB rotation threshold.
					for k := 0; k < 3; k++ {
						big := audit.Event{ServerID: s.manager.ID(), RequestID: fmt.Sprintf("big-%d", k), Tool: "file_write",
							Phase: "request_start", Args: map[string]any{"content": strings.Repeat("x", 1800<<10)}}
						if err := s.recorder.Write(audit.NewEvent(big)); err != nil {
							t.Error(err)
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	if _, err := os.Stat(s.recorder.Status().Path + ".1"); err != nil {
		t.Fatalf("the writer did not rotate: %v", err)
	}
	for _, v := range viewers {
		// The notifications say what changed; catch-up delivers everything.
		got := map[string]bool{}
		after := v.env.LiveCursor
		more := false
		for round := 0; round < 50 && len(got) < len(ids); round++ {
			if !more {
				waitEvent(t, v.events, dashboard.EventActivity, 10*time.Second)
			}
			var page dashboard.CatchUpPage // Fresh per answer: absent fields must stay absent.
			s.post(dashboard.PathCatchUp, v.token, map[string]any{"window": v.env.Window, "generation": v.env.Generation, "after": after}, &page)
			for {
				for _, it := range page.Items {
					if it.Action != nil && it.Action.State == activity.RequestFinished {
						got[it.Action.Key.RequestID] = true // Finished, not merely started.
					}
				}
				if page.Next == nil {
					break
				}
				next := page.Next
				page = dashboard.CatchUpPage{}
				s.post(dashboard.PathCatchUp, v.token, map[string]any{"window": v.env.Window, "generation": v.env.Generation,
					"after": after, "before": next}, &page)
			}
			after, more = page.Head, page.More
			if round == 0 { // A re-snapshot while streaming keeps the generation.
				var again dashboard.Envelope
				s.post(dashboard.PathWindow, v.token, map[string]any{"window": v.env.Window}, &again)
				if again.Generation != v.env.Generation {
					t.Fatalf("re-snapshot changed the generation")
				}
			}
		}
		for _, id := range ids {
			if !got[id] {
				t.Fatalf("a viewer never received %s", id)
			}
		}
	}
	if errs := s.recorder.Status(); errs.State != "ok" {
		t.Fatalf("audit %+v", errs)
	}
}

// TestLongDashboardStream keeps a stream open past the server's read and
// write timeouts (15 s and 30 s) with the production heartbeat, whose gap is
// longer than the 10 s per-write deadline, then still delivers activity. It
// runs in real time, so it stays out of the repeated Windows job (its name
// does not start with TestDashboard).
func TestLongDashboardStream(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time test")
	}
	s := startLiveServer(t)
	_, events := s.openStream(s.login())
	waitEvent(t, events, dashboard.EventReady, 5*time.Second)
	start := time.Now()
	last := start
	for time.Since(start) < 35*time.Second {
		if _, ok := waitEvent(t, events, dashboard.EventHeartbeat, 20*time.Second); !ok {
			t.Fatalf("the stream ended after %v", time.Since(start))
		}
		if gap := time.Since(last); gap < 10*time.Second {
			t.Fatalf("heartbeat gap %v: not the production interval", gap)
		}
		last = time.Now()
	}
	s.audit("late")
	for {
		ev, ok := waitEvent(t, events, dashboard.EventActivity, 10*time.Second)
		if !ok {
			t.Fatal("the stream ended")
		}
		if strings.Contains(string(ev.data), `"a:`+s.manager.ID()+`/late"`) {
			break
		}
	}
}

// TestDashboardStreamSlowConsumer: while a viewer's stream is never read,
// another viewer's stream, MCP calls and audit appends go on, and a normal
// stop completes. (A stream's output is bounded by backpressure and compact
// events, so a non-reading client rarely blocks a write at all; when it
// does, the per-write deadline cuts it: see TestAPIStalledStreamIsCutOff.)
func TestDashboardStreamSlowConsumer(t *testing.T) {
	s := startLiveServerWith(t, liveOptions{small: true})
	tokenA := s.login()
	var envA dashboard.Envelope
	s.post(dashboard.PathWindow, tokenA, map[string]any{}, &envA)
	_, events := s.openStream(s.login())
	waitEvent(t, events, dashboard.EventReady, 5*time.Second)

	conn, err := net.Dial("tcp", s.address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096) // A never reads its events.
	}
	body, _ := json.Marshal(map[string]any{"window": envA.Window, "generation": envA.Generation, "after": envA.LiveCursor})
	req := s.request(dashboard.PathStream, tokenA, nil)
	_, _ = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\n", dashboard.PathStream, s.address)
	for k := range req.Header {
		_, _ = fmt.Fprintf(conn, "%s: %s\r\n", k, req.Header.Get(k))
	}
	_, _ = fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(body), body)

	// A's request was admitted: its status line arrives (A reads nothing more).
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if status, err := bufio.NewReader(conn).ReadString('\n'); err != nil || !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("A's stream: %q %v", status, err)
	}
	// Stay below B's backpressure threshold (800 rows), which B never
	// releases by catching up in this test.
	long := strings.Repeat("p", 150)
	for i := 0; i < 200; i++ {
		s.audit(fmt.Sprintf("%s-%d", long, i))
	}
	waitEvent(t, events, dashboard.EventActivity, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.mcpListTerminals(ctx)
	s.audit("after-mcp")
	for {
		ev, ok := waitEvent(t, events, dashboard.EventActivity, 10*time.Second)
		if !ok {
			t.Fatal("B's stream ended")
		}
		if strings.Contains(string(ev.data), "after-mcp") {
			break
		}
	}
	stopAt := time.Now()
	s.cancel()
	select {
	case <-s.done:
		if took := time.Since(stopAt); took > viewerStopGrace+5*time.Second {
			t.Fatalf("stop took %v with a non-reading viewer", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a non-reading viewer held up the stop")
	}
}

func TestDashboardStreamLogoutAndClosedAudit(t *testing.T) {
	t.Run("logout", func(t *testing.T) {
		s := startLiveServer(t)
		token := s.login()
		_, events := s.openStream(token)
		waitEvent(t, events, dashboard.EventReady, 5*time.Second)
		if err := s.viewers.Logout(token); err != nil {
			t.Fatal(err)
		}
		for ev := range events {
			if ev.name != dashboard.EventHeartbeat && ev.name != dashboard.EventActivity {
				t.Fatalf("logout wrote %s", ev.name)
			}
		}
	})
	t.Run("closed audit", func(t *testing.T) {
		s := startLiveServer(t)
		_, events := s.openStream(s.login())
		waitEvent(t, events, dashboard.EventReady, 5*time.Second)
		_ = s.recorder.Close()
		if _, ok := waitEvent(t, events, dashboard.EventAuditClosed, 10*time.Second); !ok {
			t.Fatal("the stream ended without audit_closed")
		}
	})
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpListTerminals makes one real MCP tool call over /mcp.
func (s *liveServer) mcpListTerminals(ctx context.Context) {
	s.t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "dashboard-stream-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: "http://" + s.address + "/mcp", DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Timeout: 5 * time.Second, Transport: bearerTransport{viewerTestToken}},
	}, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "terminal_list", Arguments: struct{}{}})
	if err != nil || res == nil || res.IsError {
		s.t.Fatalf("MCP terminal_list: %v %+v", err, res)
	}
}
