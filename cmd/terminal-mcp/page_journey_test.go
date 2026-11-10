package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The page journey tests drive the dashboard page's own modules (client,
// store, attention and cards) in Node against the real HTTP server. They
// test the wire contract and the page logic together, not the DOM wiring.
// Without node they are skipped, unless TERMINAL_MCP_REQUIRE_NODE=1 (CI).

// pageDriver is a running journey.mjs.
type pageDriver struct {
	t       *testing.T
	in      io.WriteCloser
	answers <-chan []byte // One line per answer; closed when the driver ends.
	readErr error         // Why reading stopped; valid once answers is closed.
	kill    func()
}

// driverTimeout bounds one command, so a stuck journey fails with the
// command's name instead of running into the test binary's timeout.
const driverTimeout = 60 * time.Second

func startPageDriver(t *testing.T, s *liveServer) *pageDriver {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("TERMINAL_MCP_REQUIRE_NODE") == "1" {
			t.Fatal("node is required (TERMINAL_MCP_REQUIRE_NODE=1) but was not found")
		}
		t.Skip("node not found; the page journey needs Node.js 22 or newer")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "internal", "viewer", "webtest", "journey", "journey.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script, "http://"+s.address) // Ended by closing stdin in the cleanup.
	cmd.Stderr = &testWriter{t: t}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close() // The driver stops its client and exits.
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	answers := make(chan []byte, 16) // A late answer after a timeout never blocks the reader.
	d := &pageDriver{t: t, in: in, answers: answers, kill: func() { _ = cmd.Process.Kill() }}
	go func() {
		defer close(answers)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
		for sc.Scan() {
			answers <- append([]byte(nil), sc.Bytes()...)
		}
		d.readErr = sc.Err() // Read by do after the channel is closed.
	}()
	return d
}

// do sends one command and decodes its answer into out (may be nil). An
// answer that is not ok fails the test.
func (d *pageDriver) do(cmd map[string]any, out any) map[string]any {
	d.t.Helper()
	line, err := json.Marshal(cmd)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.in.Write(append(line, '\n')); err != nil {
		d.t.Fatal(err)
	}
	var raw []byte
	select {
	case a, open := <-d.answers:
		if !open {
			d.t.Fatalf("driver ended during %v (read error: %v)", cmd["cmd"], d.readErr)
		}
		raw = a
	case <-time.After(driverTimeout):
		d.kill()
		d.t.Fatalf("driver command %v took longer than %v", cmd["cmd"], driverTimeout)
	}
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		d.t.Fatalf("driver answer %q: %v", raw, err)
	}
	if answer["ok"] != true {
		d.t.Fatalf("driver %v: %v", cmd["cmd"], answer)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			d.t.Fatal(err)
		}
	}
	return answer
}

// signIn issues a pairing code and signs the driver in with it, as the
// page does with a pasted code.
func (d *pageDriver) signIn(s *liveServer) {
	d.t.Helper()
	code, _, err := s.viewers.Issue()
	if err != nil {
		d.t.Fatal(err)
	}
	d.do(map[string]any{"cmd": "login", "code": code}, nil)
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// journeyLane is a lane as the driver reports it.
type journeyLane struct {
	Identity     string `json:"identity"`
	Kind         string `json:"kind"`
	Conversation string `json:"conversation"`
	Items        int    `json:"items"`
	Loaded       int    `json:"loaded"`
	Unread       int    `json:"unread"`
}

type journeyCard struct {
	Key     string   `json:"key"`
	Tool    string   `json:"tool"`
	Size    int      `json:"size"`
	Text    string   `json:"text"`
	Actions []string `json:"actions"`
}

func (d *pageDriver) lanes() []journeyLane {
	d.t.Helper()
	var r struct{ Lanes []journeyLane }
	d.do(map[string]any{"cmd": "lanes"}, &r)
	return r.Lanes
}

func (d *pageDriver) cards(identity string) []journeyCard {
	d.t.Helper()
	var r struct{ Cards []journeyCard }
	d.do(map[string]any{"cmd": "cards", "identity": identity}, &r)
	return r.Cards
}

// mcpSession opens an MCP client session to the live server; call puts
// each call in a chat.
func (s *liveServer) mcpSession(ctx context.Context) *mcp.ClientSession {
	s.t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "page-journey", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: "http://" + s.address + "/mcp", DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: bearerTransport{viewerTestToken}},
	}, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call makes one tool call with chat metadata in _meta.
func call(t *testing.T, cs *mcp.ClientSession, chat, tool string, args any) *mcp.CallToolResult {
	t.Helper()
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	if chat != "" {
		params.Meta = mcp.Meta{"openai/session": chat}
	}
	res, err := cs.CallTool(t.Context(), params)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

// TestPageClientAgainstServer is the harness check: the page's client signs
// in with a pairing code, opens a live window over the real API and stream,
// and renders a chat's file_write as the owner sees it.
func TestPageClientAgainstServer(t *testing.T) {
	root := t.TempDir()
	s := startLiveServerWith(t, liveOptions{fileRoot: root})
	d := startPageDriver(t, s)
	d.signIn(s)
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)

	cs := s.mcpSession(t.Context())
	content := "package main\n\nfunc main() {}\n"
	res := call(t, cs, "chat-a", "file_write", map[string]any{"path": filepath.Join(root, "main.go"), "content": content,
		"server_id": s.manager.ID(), "operation_id": "op-harness-1"})
	if res.IsError {
		t.Fatalf("file_write: %s", toolText(res))
	}

	identity := "c\x00openai/session\x00chat-a"
	deadline := time.Now().Add(15 * time.Second)
	for {
		found := false
		for _, l := range d.lanes() {
			found = found || (l.Identity == identity && l.Items > 0)
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lane never appeared: %+v", d.lanes())
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.do(map[string]any{"cmd": "select", "identity": identity}, nil)
	cards := d.cards(identity)
	var write *journeyCard
	for i := range cards {
		if cards[i].Tool == "file_write" {
			write = &cards[i]
		}
	}
	if write == nil {
		t.Fatalf("no file_write card: %+v", cards)
	}
	for _, want := range []string{"main.go", "func main() {}", "op-harness-1"} {
		if !strings.Contains(write.Text, want) {
			t.Errorf("card text lacks %q:\n%s", want, write.Text)
		}
	}
	var st struct {
		Storage map[string]string
	}
	d.do(map[string]any{"cmd": "status"}, &st)
	for k := range st.Storage {
		if k != "terminal-mcp-dashboard-window" {
			t.Errorf("client storage holds %q", k)
		}
	}
}

// toolText is the text content of a tool result.
func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
