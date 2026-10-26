//go:build linux || darwin

package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"terminalmcp/internal/filesystem"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"terminalmcp/internal/httpguard"
	"terminalmcp/internal/session"
	"terminalmcp/internal/terminal"
)

func call[T any](t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args any) T {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatalf("tool %s failed: %#v", name, r.Content)
	}
	var payload []byte
	if r.StructuredContent != nil {
		payload, err = json.Marshal(r.StructuredContent)
	} else {
		for _, c := range r.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				payload = []byte(text.Text)
				break
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode %s: %v; %s", name, err, payload)
	}
	return out
}

func TestInMemoryToolContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager := session.New(nil)
	defer manager.Close()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := New(manager).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 7 {
		t.Fatalf("expected seven tools, got %d", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		wantReadOnly := tool.Name == "terminal_list" || tool.Name == "terminal_read"
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != wantReadOnly {
			t.Fatalf("wrong annotation: %s", tool.Name)
		}
	}
	list := call[session.ListResult](t, ctx, cs, "terminal_list", struct{}{})
	a := session.StartArgs{ServerID: list.ServerID, OperationID: "start-1", Argv: []string{"/bin/sh", "-c", "printf hello"}}
	first := call[session.ActionResult](t, ctx, cs, "terminal_start", a)
	if first.Receipt.Status != "applied" {
		t.Fatalf("start failed: %#v", first)
	}
	retry := call[session.ActionResult](t, ctx, cs, "terminal_start", a)
	if !retry.Replayed || retry.Receipt.SessionID != first.Receipt.SessionID {
		t.Fatal("MCP start replay reran the process")
	}
	cursor := ""
	var output strings.Builder
	for {
		o := call[session.Observation](t, ctx, cs, "terminal_read", session.ReadArgs{SessionID: first.Receipt.SessionID, Cursor: cursor, WaitMS: 100})
		for _, e := range o.Events {
			if e.Text != nil {
				output.WriteString(*e.Text)
			}
		}
		cursor = o.NextCursor
		if o.Session.Exit != nil && o.Session.OutputDone && !o.HasMore {
			break
		}
	}
	if output.String() != "hello" {
		t.Fatalf("output: %q", output.String())
	}
	closed := call[session.ActionResult](t, ctx, cs, "terminal_close", session.CloseArgs{ServerID: list.ServerID, OperationID: "close-1", SessionID: first.Receipt.SessionID})
	if closed.Receipt.Status != "applied" {
		t.Fatalf("close: %+v", closed)
	}
	defaultList := call[session.ListResult](t, ctx, cs, "terminal_list", struct{}{})
	if len(defaultList.Sessions) != 0 || defaultList.Counts.ClosedRetained != 1 {
		t.Fatalf("default list should hide closed session: %+v", defaultList)
	}
	fullList := call[session.ListResult](t, ctx, cs, "terminal_list", session.ListArgs{IncludeClosed: true})
	if len(fullList.Sessions) != 1 || fullList.Sessions[0].SessionID != first.Receipt.SessionID || !fullList.Sessions[0].Closed {
		t.Fatalf("full list should retain closed session: %+v", fullList)
	}
}

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (tr bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+tr.token)
	return tr.next.RoundTrip(copy)
}

func TestHTTPReconnectRetainsProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	manager := session.New(nil)
	defer manager.Close()
	httpServer := httptest.NewUnstartedServer(nil)
	token := strings.Repeat("a", 64)
	guard, err := httpguard.Wrap(HTTP(New(manager)), httpServer.Listener.Addr().String(), token)
	if err != nil {
		t.Fatal(err)
	}
	httpServer.Config.Handler = guard
	httpServer.Start()
	defer httpServer.Close()
	connect := func() *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "reconnect-test", Version: "1"}, nil)
		hc := &http.Client{Transport: bearerTransport{token, http.DefaultTransport}, Timeout: 15 * time.Second}
		cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp", HTTPClient: hc, DisableStandaloneSSE: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	first := connect()
	list := call[session.ListResult](t, ctx, first, "terminal_list", struct{}{})
	r := call[session.ActionResult](t, ctx, first, "terminal_start", session.StartArgs{ServerID: list.ServerID, OperationID: "persistent", Argv: []string{"/bin/sh", "-c", "printf running; sleep 30"}})
	if r.Receipt.Status != "applied" {
		t.Fatalf("start: %#v", r)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := connect()
	defer func() { _ = second.Close() }()
	list = call[session.ListResult](t, ctx, second, "terminal_list", struct{}{})
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != r.Receipt.SessionID || list.Sessions[0].ProcessState != "running" {
		t.Fatal("process lost on client disconnect")
	}
	stop := call[session.ActionResult](t, ctx, second, "terminal_signal", session.SignalArgs{ServerID: list.ServerID, OperationID: "stop", SessionID: r.Receipt.SessionID, Signal: "KILL"})
	if stop.Receipt.Status != "applied" {
		t.Fatalf("stop: %#v", stop)
	}
}

func TestFileToolsAndCompletionOverMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager := session.New(nil)
	defer manager.Close()
	files, err := filesystem.New(manager, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := New(manager, files).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "files-contract", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 14 {
		t.Fatalf("%+v %v", tools, err)
	}
	for _, tool := range tools.Tools {
		if strings.HasPrefix(tool.Name, "file_") {
			wantOpenWorld := tool.Name == "file_receive"
			if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint != wantOpenWorld {
				t.Fatalf("wrong open-world annotation for %s", tool.Name)
			}
		}
	}
	list := call[session.ListResult](t, ctx, cs, "terminal_list", struct{}{})
	path := filepath.Join(files.Root(), "new.txt")
	created := call[filesystem.ActionResult](t, ctx, cs, "file_write", filesystem.WriteArgs{ServerID: list.ServerID, OperationID: "create", Path: path, Content: "hello\n"})
	if created.Receipt.Status != "applied" {
		t.Fatal(created)
	}
	read := call[filesystem.ReadResult](t, ctx, cs, "file_read", filesystem.ReadArgs{Files: []filesystem.Range{{Path: path}}})
	if read.Files[0].Version != created.Receipt.Version {
		t.Fatal("wrong read version")
	}
	edited := call[filesystem.ActionResult](t, ctx, cs, "file_edit", filesystem.EditArgs{ServerID: list.ServerID, OperationID: "edit", Path: path, ExpectedVersion: created.Receipt.Version, Edits: []filesystem.Edit{{OldText: "hello", NewText: "world"}}})
	if edited.Receipt.Status != "applied" {
		t.Fatal(edited)
	}
	run := call[session.ActionResult](t, ctx, cs, "terminal_start", session.StartArgs{ServerID: list.ServerID, OperationID: "quick", Argv: []string{"/bin/sh", "-c", "printf done"}, CloseOnExit: true, WaitFor: "completion", WaitMS: 1000})
	if run.Observation == nil || run.Observation.ReturnReason != "completed" || !run.Observation.Session.Closed {
		t.Fatal(run)
	}
}

func TestMaintenanceEndpointContract(t *testing.T) {
	manager := session.New(nil)
	defer manager.Close()
	state, err := manager.List(context.Background(), session.ListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	handler := Maintenance(manager)
	cases := []struct {
		method, body string
		status       int
	}{
		{"GET", "", 405},
		{"POST", `{}`, 400},
		{"POST", `{"server_id":"old","action":"begin"}`, 409},
		{"POST", `{"server_id":"` + state.ServerID + `","action":"begin","unknown":1}`, 400},
		{"POST", `{"server_id":"` + state.ServerID + `","action":"begin"} {}`, 400},
		{"POST", `{"server_id":"` + state.ServerID + `","action":"shutdown"}`, 403},
		{"POST", `{"server_id":"` + state.ServerID + `","action":"begin"}`, 200},
		{"POST", `{"server_id":"` + state.ServerID + `","action":"cancel"}`, 200},
	}
	for _, tc := range cases {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, "/admin/maintenance", strings.NewReader(tc.body)))
		if response.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.body, response.Code, response.Body.String())
		}
	}
}
func TestStandaloneMaintenanceShutdownRequiresFence(t *testing.T) {
	manager := session.New(nil)
	defer manager.Close()
	stopped := 0
	handler := Maintenance(manager, func() { stopped++ })
	id := manager.ID()
	request := func(id, action string) int {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", "/admin/maintenance",
			strings.NewReader(`{"server_id":"`+id+`","action":"`+action+`"}`)))
		return response.Code
	}
	if got := request(id, "shutdown"); got != 409 || stopped != 0 {
		t.Fatalf("shutdown without fence: status=%d callbacks=%d", got, stopped)
	}
	if got := request("stale", "shutdown"); got != 409 || stopped != 0 {
		t.Fatalf("stale shutdown: status=%d callbacks=%d", got, stopped)
	}
	if got := request(id, "begin"); got != 200 {
		t.Fatalf("fence: %d", got)
	}
	if got := request(id, "shutdown"); got != 200 || stopped != 1 {
		t.Fatalf("fenced shutdown: status=%d callbacks=%d", got, stopped)
	}
}

func TestPTYAutoCloseKeepsScreen(t *testing.T) {
	manager := session.New(terminal.Start)
	defer manager.Close()
	list, _ := manager.List(context.Background(), session.ListArgs{})
	result, err := manager.Start(context.Background(), session.StartArgs{
		ServerID: list.ServerID, OperationID: "pty-complete", Mode: "pty",
		Argv:        []string{"/bin/sh", "-c", "printf rendered"},
		CloseOnExit: true, WaitFor: "completion", WaitMS: 3000,
	})
	if err != nil || result.Observation == nil || !result.Observation.Session.Closed {
		t.Fatalf("%+v %v", result, err)
	}
	out, err := manager.Read(context.Background(), session.ReadArgs{SessionID: result.Receipt.SessionID, View: "screen"})
	if err != nil || out.Screen == nil || !strings.Contains(out.Screen.Text, "rendered") {
		t.Fatalf("%+v %v", out, err)
	}
}
