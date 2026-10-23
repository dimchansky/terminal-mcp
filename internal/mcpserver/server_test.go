//go:build linux || darwin

package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"terminalmcp/internal/httpguard"
	"terminalmcp/internal/session"
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
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
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
	defer second.Close()
	list = call[session.ListResult](t, ctx, second, "terminal_list", struct{}{})
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != r.Receipt.SessionID || list.Sessions[0].ProcessState != "running" {
		t.Fatal("process lost on client disconnect")
	}
	stop := call[session.ActionResult](t, ctx, second, "terminal_signal", session.SignalArgs{ServerID: list.ServerID, OperationID: "stop", SessionID: r.Receipt.SessionID, Signal: "KILL"})
	if stop.Receipt.Status != "applied" {
		t.Fatalf("stop: %#v", stop)
	}
}
