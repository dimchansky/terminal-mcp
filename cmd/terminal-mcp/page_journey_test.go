package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/dashboard"
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
// page does with a pasted code; it answers the viewer token.
func (d *pageDriver) signIn(s *liveServer) string {
	d.t.Helper()
	code, _, err := s.viewers.Issue()
	if err != nil {
		d.t.Fatal(err)
	}
	var r struct{ Token string }
	d.do(map[string]any{"cmd": "login", "code": code}, &r)
	return r.Token
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
	Key         string   `json:"key"`
	Tool        string   `json:"tool"`
	Size        int      `json:"size"`
	Text        string   `json:"text"`
	Head        string   `json:"head"` // The text without the folded Details.
	Actions     []string `json:"actions"`
	Occurrences []string `json:"occurrences"`
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

// chatIdentity is the page's lane identity of a claimed chat.
func chatIdentity(chat string) string { return "c\x00openai/session\x00" + chat }

// waitLane waits until the driver's live view has at least n items in the
// lane, then selects it and waits for its first page and records.
func (d *pageDriver) waitLane(identity string, n int) {
	d.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, l := range d.lanes() {
			if l.Identity == identity && l.Items >= n {
				d.do(map[string]any{"cmd": "select", "identity": identity}, nil)
				return
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("lane %q never had %d items: %+v", identity, n, d.lanes())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPageShowsWhatTheModelAskedFor (#10 forensic acceptance): real tool
// calls with awkward inputs, all through MCP over HTTP, render as the owner
// must see them, and every card's records are its own journal lines.
func TestPageShowsWhatTheModelAskedFor(t *testing.T) {
	root := t.TempDir()
	s := startLiveServerWith(t, liveOptions{fileRoot: root})
	d := startPageDriver(t, s)
	d.signIn(s)
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)
	cs := s.mcpSession(t.Context())
	sid := s.manager.ID()
	op := func(n string) string { return "op-forensic-" + n }
	mainGo := filepath.Join(root, "main.go")
	original := "package main\n\nfunc main() {\n\ttimeout := 5 * time.Second\n\t_ = timeout\n}\n"
	outcome := func(res *mcp.CallToolResult, wantError bool, what string) {
		t.Helper()
		if res.IsError != wantError {
			t.Fatalf("%s: error %v, want %v: %s", what, res.IsError, wantError, toolText(res))
		}
	}

	call(t, cs, "chat-a", "terminal_start", map[string]any{"server_id": sid, "operation_id": op("start"), "argv": []string{"go", "test", "-race", "./..."},
		"env": map[string]string{"GOFLAGS": "-count=1"}, "cwd": root, "close_on_exit": true, "wait_for": "completion", "wait_ms": 5000})
	call(t, cs, "chat-a", "terminal_start", map[string]any{"server_id": sid, "operation_id": op("quoted"), "argv": []string{"printf", `it's a "quoted" $HOME`, "tab\there"},
		"env": map[string]string{"BAD-NAME": "1"}, "cwd": root, "close_on_exit": true, "wait_for": "completion", "wait_ms": 5000})
	call(t, cs, "chat-a", "terminal_start", map[string]any{"server_id": sid, "operation_id": op("script"), "argv": []string{"/bin/sh", "-c", `echo "$1"`, "_", "a b"},
		"cwd": root, "close_on_exit": true, "wait_for": "completion", "wait_ms": 5000})
	if runtime.GOOS != "windows" {
		// Ended by the runtime limit: a signal, not a success.
		call(t, cs, "chat-a", "terminal_start", map[string]any{"server_id": sid, "operation_id": op("killed"), "argv": []string{"sleep", "5"},
			"max_runtime_ms": 200, "close_on_exit": true, "wait_for": "completion", "wait_ms": 5000})
		// A multi-line script in a long working directory: both in full.
		deep := filepath.Join(root, strings.Repeat("a-long-directory-name/", 6))
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatal(err)
		}
		call(t, cs, "chat-a", "terminal_start", map[string]any{"server_id": sid, "operation_id": op("multi"), "argv": []string{"/bin/sh", "-c", "set -e\necho first-line\necho second-line"},
			"cwd": deep, "close_on_exit": true, "wait_for": "completion", "wait_ms": 5000})
		// Running at launch, its exit seen later by a read.
		late := call(t, cs, "chat-a", "terminal_start", map[string]any{"server_id": sid, "operation_id": op("late"), "argv": []string{"sleep", "0.3"}, "wait_ms": 0})
		var lateResult struct {
			Receipt struct {
				SessionID string `json:"session_id"`
			} `json:"receipt"`
		}
		if err := json.Unmarshal([]byte(toolText(late)), &lateResult); err != nil || lateResult.Receipt.SessionID == "" {
			t.Fatalf("terminal_start result %q: %v", toolText(late), err)
		}
		call(t, cs, "chat-a", "terminal_read", map[string]any{"session_id": lateResult.Receipt.SessionID, "wait_ms": 0})
		call(t, cs, "chat-a", "terminal_read", map[string]any{"session_id": lateResult.Receipt.SessionID, "wait_for": "completion", "wait_ms": 5000})
	}
	outcome(call(t, cs, "chat-a", "terminal_start", json.RawMessage(`{"server_id":"`+sid+`","operation_id":"`+op("bad-argv")+`","argv":"ls -la"}`)),
		true, "schema-rejected argv")
	outcome(call(t, cs, "chat-a", "terminal_write", map[string]any{"server_id": sid, "operation_id": op("write"), "session_id": "no-such-session",
		"input": []map[string]any{{"text": "echo \x1b[31mred\x1b[0m \u202Eevil\u202C\x00\x07\r\n"}, {"key": "CTRL_C"}}}), true, "terminal_write")
	outcome(call(t, cs, "chat-a", "file_write", map[string]any{"server_id": sid, "operation_id": op("file"), "path": mainGo, "content": original}), false, "file_write")
	sum := sha256.Sum256([]byte(original))
	edited := strings.Replace(original, "5 * time.Second", "10 * time.Second", 1)
	outcome(call(t, cs, "chat-a", "file_edit", map[string]any{"server_id": sid, "operation_id": op("edit1"), "path": mainGo,
		"expected_version": "sha256:" + hex.EncodeToString(sum[:]),
		"edits":            []map[string]string{{"old_text": "5 * time.Second", "new_text": "10 * time.Second"}}}), false, "file_edit with one replacement")
	sum = sha256.Sum256([]byte(edited))
	outcome(call(t, cs, "chat-a", "file_edit", map[string]any{"server_id": sid, "operation_id": op("edit"), "path": mainGo,
		"expected_version": "sha256:" + hex.EncodeToString(sum[:]),
		"edits": []map[string]string{{"old_text": "\ttimeout := 10 * time.Second\n\t_ = timeout\n", "new_text": "\ttimeout := 10 * time.Second\n\tlog.Print(timeout)\n"},
			{"old_text": "func main() {\n", "new_text": "// main runs.\nfunc main() {\n\tlog.Print(\"start\")\n"}}}), false, "file_edit with several replacements")
	outcome(call(t, cs, "chat-a", "file_read", map[string]any{"files": []map[string]any{{"path": mainGo}}}), false, "file_read")
	outcome(call(t, cs, "chat-a", "file_write", map[string]any{"server_id": sid, "operation_id": op("unknown"), "path": filepath.Join(root, "x.txt"), "content": "x",
		"x_debug": map[string]any{"level": 1, "tags": []string{"a", "b"}}}), true, "unknown fields")
	outcome(call(t, cs, "chat-a", "file_write", json.RawMessage(`{"server_id":"`+sid+`","operation_id":"`+op("dup")+`","path":"`+jsonPath(filepath.Join(root, "dup.txt"))+
		`","path":"`+jsonPath(filepath.Join(root, "dup2.txt"))+`","content":"x","n":9007199254740993}`)), true, "duplicate keys")
	// The same operation from another chat: a replay. Its arguments must be
	// identical, so its own start record (with chat-b's metadata) is what
	// proves the card shows the replay's input, not the original's.
	replay := call(t, cs, "chat-b", "file_write", map[string]any{"server_id": sid, "operation_id": op("file"), "path": mainGo, "content": original})
	outcome(replay, false, "replay")
	if !strings.Contains(toolText(replay), "replayed") {
		t.Fatalf("the chat-b call was not a replay: %s", toolText(replay))
	}
	// A near-4-MiB record: JSON escapes each '<' as six bytes, so a body
	// under the 1 MiB HTTP limit becomes a 3.7 MiB journal line (the tool
	// refuses content over 64 KiB; the audit keeps all of it).
	outcome(call(t, cs, "chat-c", "file_write", map[string]any{"server_id": sid, "operation_id": "op-big", "path": filepath.Join(root, "big.txt"),
		"content": strings.Repeat("<", 620_000)}), true, "near-4-MiB input")
	// An input whose audit line would exceed 4 MiB is rejected before any
	// effect, with its reason.
	huge := call(t, cs, "chat-c", "file_write", map[string]any{"server_id": sid, "operation_id": "op-huge", "path": filepath.Join(root, "huge.txt"),
		"content": strings.Repeat("<", 720_000)})
	outcome(huge, true, "oversized input")
	if _, err := os.Stat(filepath.Join(root, "huge.txt")); !os.IsNotExist(err) {
		t.Fatalf("an unrecorded input had an effect: %v", err)
	}

	journal, err := os.ReadFile(s.journal)
	if err != nil {
		t.Fatal(err)
	}
	// byRequest maps a request ID to its journal lines.
	byRequest := map[string][]string{}
	byOperation := map[string][]string{}
	for _, l := range strings.Split(string(journal), "\n") {
		var e struct {
			RequestID   string `json:"request_id"`
			OperationID string `json:"operation_id"`
			Phase       string `json:"phase"`
		}
		if json.Unmarshal([]byte(l), &e) != nil {
			continue
		}
		byRequest[e.RequestID] = append(byRequest[e.RequestID], l)
		if strings.HasPrefix(e.Phase, "operation_") {
			byOperation[e.OperationID] = append(byOperation[e.OperationID], l)
		}
	}

	expect := func(c journeyCard, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(c.Text, w) {
				t.Errorf("%s card lacks %q:\n%s", c.Tool, w, c.Text)
			}
		}
	}
	reject := func(c journeyCard, unwanted ...string) {
		t.Helper()
		for _, w := range unwanted {
			if strings.Contains(c.Text, w) {
				t.Errorf("%s card shows %q:\n%s", c.Tool, w, c.Text)
			}
		}
	}
	hasAction := func(c journeyCard, action string) bool { return slices.Contains(c.Actions, action) }
	// requestOf is the request a card shows (item keys end in its ID).
	requestOf := func(c journeyCard) string { return c.Key[strings.LastIndex(c.Key, "/")+1:] }
	byOp := func(cards []journeyCard, opID string) journeyCard {
		t.Helper()
		for _, c := range cards {
			for _, l := range byRequest[requestOf(c)] {
				if strings.Contains(l, `"operation_id":"`+opID+`"`) {
					return c
				}
			}
		}
		t.Fatalf("no card for %s", opID)
		return journeyCard{}
	}
	// checkRecords loads every record of a card and requires each to be one
	// of the card's own journal lines, byte for byte: its request's lines,
	// or its operation's. With an operation ID, the card must also carry at
	// least one of that operation's records (its effect).
	checkRecords := func(c journeyCard, opID string) {
		t.Helper()
		if len(c.Occurrences) == 0 {
			t.Fatalf("%s card %s has no records", c.Tool, c.Key)
		}
		own := map[string]bool{}
		for _, l := range append(byRequest[requestOf(c)], byOperation[opID]...) {
			own[l] = true
		}
		effect := false
		for _, occ := range c.Occurrences {
			var r struct{ B64 string }
			d.do(map[string]any{"cmd": "record", "occurrence": occ}, &r)
			data, err := base64.StdEncoding.DecodeString(r.B64)
			if err != nil || len(data) == 0 {
				t.Fatalf("record %s: %v (%d bytes)", occ, err, len(data))
			}
			line := strings.TrimSuffix(string(data), "\n")
			if !own[line] {
				t.Errorf("record %s of the %s card is not one of its journal lines: %.200q", occ, c.Tool, data)
			}
			effect = effect || slices.Contains(byOperation[opID], line)
		}
		if opID != "" && !effect {
			t.Errorf("the %s card of %s carries none of its operation's records", c.Tool, opID)
		}
	}
	// safe requires that no raw control or bidi character reaches a card.
	safe := func(cards []journeyCard) {
		t.Helper()
		for _, c := range cards {
			reject(c, "\x1b", "\x00", "\x07", "\r", "\u202e", "\u202c")
		}
	}

	cardsA := 11
	if runtime.GOOS != "windows" {
		cardsA += 4 // The killed and multi-line commands, and a command with a group of later reads.
	}
	d.waitLane(chatIdentity("chat-a"), cardsA)
	a := d.cards(chatIdentity("chat-a"))
	if len(a) != cardsA {
		t.Fatalf("chat-a has %d cards, want %d", len(a), cardsA)
	}
	safe(a)

	// Commands: readable, env first, copyable only when exact (POSIX); on
	// Windows the exact tokens and env, never a POSIX line. The audit has
	// no process output, so none is shown.
	start := byOp(a, op("start"))
	quoted := byOp(a, op("quoted"))
	if runtime.GOOS == "windows" {
		expect(start, "go\ntest\n-race\n./...", "GOFLAGS", "-count=1")
		if hasAction(start, "copy-command") || hasAction(quoted, "copy-command") {
			t.Error("a Windows command offers a POSIX copy")
		}
	} else {
		expect(start, "GOFLAGS=-count=1 go test -race ./...\n", "not a shell invocation", "in "+root)
		if !hasAction(start, "copy-command") {
			t.Error("an exact command is not copyable")
		}
		expect(quoted, `BAD-NAME=1 printf 'it'\''s a "quoted" $HOME' 'tab\there'`, "Not copyable", "it's a \"quoted\" $HOME\n")
		if hasAction(quoted, "copy-command") {
			t.Error("a command that cannot round-trip is copyable")
		}
	}
	expect(start, "The audit records the launch, not the program's output.")
	if strings.Contains(start.Head, "The audit records the launch") {
		t.Error("a caveat shows at a glance instead of behind its note")
	}
	// A shell script: the script once, the tokens around it, the full line
	// in Details, Copy still there.
	if runtime.GOOS != "windows" {
		script := byOp(a, op("script"))
		for _, want := range []string{"via /bin/sh -c", `echo "$1"`, "Arguments after the script ($0 first)", "_", "'a b'"} {
			if !strings.Contains(script.Head, want) {
				t.Errorf("script card lacks %q at a glance:\n%s", want, script.Head)
			}
		}
		expect(script, `Full command line`, `/bin/sh -c 'echo "$1"' _ 'a b'`)
		if strings.Contains(script.Head, "Full command line") || !hasAction(script, "copy-command") {
			t.Errorf("script card: full line at a glance or no Copy:\n%s %v", script.Head, script.Actions)
		}
		checkRecords(script, op("script"))
	}
	// The outcome says how the program ended, never "running now". A
	// header reads as one line: verb, outcome, note, time.
	glance := func(c journeyCard) string { return strings.Join(strings.Fields(c.Head), " ") }
	if !strings.Contains(glance(start), "Run command Exited · 1 exit seen ") {
		t.Errorf("a failing command does not read as its exit:\n%s", start.Head)
	}
	if runtime.GOOS != "windows" {
		if script := byOp(a, op("script")); !strings.Contains(glance(script), "Run command Exited · 0 exit seen ") {
			t.Errorf("a successful command does not read as exit 0:\n%s", script.Head)
		}
		if late := byOp(a, op("late")); !strings.Contains(glance(late), "Run command Exited · 0 exit seen ") || strings.Contains(glance(late), "Last seen running") {
			t.Errorf("a command whose exit a later read saw does not read as that exit:\n%s", late.Head)
		}
		// Details keep the launch's own result and name the later observation.
		lateCard := byOp(a, op("late"))
		expect(lateCard, "Result when this call returned · ", "Latest recorded process observation · ", "Seen by Read output")
		// Its own result still says running; the observation says exited.
		own := lateCard.Text[strings.Index(lateCard.Text, "Result when this call returned"):]
		if i := strings.Index(own, "Latest recorded process observation"); i < 0 || !strings.Contains(own[:i], "running") || !strings.Contains(own[i:], "exited") {
			t.Errorf("the launch's own result and the later observation are not told apart:\n%s", lateCard.Text)
		}
		var reads []journeyCard
		for _, c := range a {
			if c.Tool == "terminal_read" {
				reads = append(reads, c)
			}
		}
		if len(reads) != 1 || reads[0].Size != 2 {
			t.Errorf("the two reads are not one group: %+v", reads)
		}
		if !hasAction(lateCard, "jump") {
			t.Errorf("no way to show the read the exit came from: %v", lateCard.Actions)
		}
		multi := byOp(a, op("multi"))
		for _, want := range []string{"set -e\necho first-line\necho second-line", "in " + filepath.Join(root, strings.Repeat("a-long-directory-name/", 6))} {
			if !strings.Contains(multi.Head, want) {
				t.Errorf("multi-line command card lacks %q at a glance:\n%s", want, multi.Head)
			}
		}
		// The limit terminates first and kills only a program that stays.
		if killed := byOp(a, op("killed")); !strings.Contains(glance(killed), "Run command Terminated · SIGTERM exit seen ") &&
			!strings.Contains(glance(killed), "Run command Killed · SIGKILL exit seen ") {
			t.Errorf("a killed command does not read as its signal:\n%s", killed.Head)
		}
	}
	reject(start, "go.mod file not found", "no Go files", "no packages")
	checkRecords(start, op("start"))

	// Schema-rejected input stays visible field by field and never ran.
	bad := byOp(a, op("bad-argv"))
	if !strings.HasPrefix(glance(bad), "Run command Not run ") {
		t.Errorf("a rejected command does not read Not run at a glance:\n%s", bad.Head)
	}
	expect(bad, "the arguments did not match the tool's schema", "Supplied fields\nserver_id", "argv\nls -la\n", "schema_validation")
	reject(bad, "The action took", "Started")
	if !strings.Contains(bad.Head, sid) || !strings.Contains(bad.Head, op("bad-argv")) {
		t.Errorf("a rejected call folded its protocol fields:\n%s", bad.Head)
	}
	checkRecords(bad, "")
	// Terminal input with visible escapes for ANSI, bidi and control bytes.
	write := byOp(a, op("write"))
	expect(write, `text echo \e[31mred\e[0m \u202Eevil\u202C\0\x07\r\n`, "key CTRL_C")
	checkRecords(write, "")
	// file_write content in full; every requested edit as a red/green pair.
	file := byOp(a, op("file"))
	expect(file, "Create file Done", "File "+mainGo, original, "The action took")
	checkRecords(file, op("file"))
	one := byOp(a, op("edit1"))
	expect(one, "Requested replacement 1 of 1", "- 5 * time.Second\n+ 10 * time.Second")
	checkRecords(one, op("edit1"))
	edit := byOp(a, op("edit"))
	expect(edit, "Requested replacement 1 of 2", "  \ttimeout := 10 * time.Second\n- \t_ = timeout\n+ \tlog.Print(timeout)",
		"Requested replacement 2 of 2", "+ // main runs.", "+ \tlog.Print(\"start\")", "no file line numbers are recorded")
	checkRecords(edit, op("edit"))
	// At a glance: the command, not the call's retry identity or waits.
	for _, c := range []journeyCard{start, file, one, edit} {
		if strings.Contains(c.Head, sid) || strings.Contains(c.Head, "op-forensic-") || strings.Contains(c.Head, "wait_ms") {
			t.Errorf("%s card shows protocol fields at a glance:\n%s", c.Tool, c.Head)
		}
		if !strings.Contains(c.Text, sid) || !strings.Contains(c.Text, "op-forensic-") {
			t.Errorf("%s card lost its protocol fields from Details:\n%s", c.Tool, c.Text)
		}
	}
	// file_read: the request only; the file's contents are not in the audit.
	var read journeyCard
	for _, c := range a {
		if c.Tool == "file_read" {
			read = c
		}
	}
	expect(read, "The audit records the request, not the file contents returned.", mainGo)
	reject(read, "log.Print", "package main")
	checkRecords(read, "")
	// Unknown nested fields are listed, not dropped.
	unknown := byOp(a, op("unknown"))
	expect(unknown, "x_debug\nlevel\n1\ntags\na\nb")
	checkRecords(unknown, "")
	// Duplicate keys and inexact numbers: flagged and shown as recorded.
	dup := byOp(a, op("dup"))
	expect(dup, `duplicate key "path"`, "9007199254740993 cannot be represented exactly", "dup.txt", "dup2.txt", `"n":9007199254740993`)
	reject(dup, "9007199254740992")
	checkRecords(dup, "")

	// The replay from another chat shows its own request and lane and
	// points to the original operation, whose effect stays with chat-a.
	d.waitLane(chatIdentity("chat-b"), 1)
	b := d.cards(chatIdentity("chat-b"))
	if len(b) != 1 {
		t.Fatalf("chat-b cards: %+v", b)
	}
	safe(b)
	expect(b[0], "Repeat of an earlier call, not run again", "of operation "+op("file"), "the original was done", "File "+mainGo, "openai/session\nchat-b")
	reject(b[0], "The action took", "openai/session\nchat-a")
	if requestOf(b[0]) == requestOf(file) {
		t.Fatal("the replay card shows the original request")
	}
	checkRecords(b[0], "")

	// The near-4-MiB record is deferred with its exact size and loads whole.
	d.waitLane(chatIdentity("chat-c"), 2)
	c := d.cards(chatIdentity("chat-c"))
	safe(c)
	big := byOp(c, "op-big")
	var bigLine string
	for _, l := range byRequest[requestOf(big)] {
		if strings.Contains(l, `"phase":"request_start"`) {
			bigLine = l
		}
	}
	if len(bigLine) < 3<<20 || len(bigLine) >= 4<<20 {
		t.Fatalf("the big line is %d bytes, want 3–4 MiB", len(bigLine))
	}
	if !hasAction(big, "load-record") {
		t.Fatalf("the big record is not deferred: %+v", big.Actions)
	}
	expect(big, fmt.Sprintf("Large record (%s bytes)", groupDigits(len(bigLine)+1)))
	checkRecords(big, "")

	// The oversized input: rejected before any effect, saying so and why,
	// in the chat that sent it (the rejection keeps the call metadata).
	rejected := byOp(c, "op-huge")
	expect(rejected, "Not run", "the input was too large to record in the audit", "Input not recorded · tool not executed", "audit event exceeds maximum file size")
	if hasAction(rejected, "load-record") {
		t.Error("an unrecorded input offers a record to load")
	}
	checkRecords(rejected, "")
	for _, l := range d.lanes() {
		if l.Kind == "unattributed" {
			t.Errorf("a lane of unattributed activity exists: %+v", l)
		}
	}

	// Nothing private leaves memory: the client's storage holds window IDs
	// only, and API paths never carry input.
	var st struct{ Storage map[string]string }
	d.do(map[string]any{"cmd": "status"}, &st)
	for k, v := range st.Storage {
		if k != "terminal-mcp-dashboard-window" || strings.Contains(v, "op-") || strings.Contains(v, root) {
			t.Errorf("client storage holds %q=%q", k, v)
		}
	}
	var rq struct{ Counts map[string]int }
	d.do(map[string]any{"cmd": "requests"}, &rq)
	for path := range rq.Counts {
		if !slices.Contains([]string{"login", "window", "stream", "catch-up", "lane-items", "record", "earlier", "discover", "origins"}, path) {
			t.Errorf("unexpected API path %q", path)
		}
	}
}

// groupDigits formats n with thousands separators, as the page does.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// jsonPath escapes a path for a JSON string literal (Windows backslashes).
func jsonPath(p string) string {
	b, _ := json.Marshal(p)
	return string(b[1 : len(b)-1])
}

// journeyRequests is the driver's request count.
type journeyRequests struct {
	Counts  map[string]int `json:"counts"`
	Records map[string]int `json:"records"`
}

func (d *pageDriver) requests() journeyRequests {
	d.t.Helper()
	var r journeyRequests
	d.do(map[string]any{"cmd": "requests"}, &r)
	return r
}

// TestPageFirstScreenRequestBudget (#10): the first screen of recorded
// history costs one window request and at most one lane page; ordinary records,
// mid-size ones included, arrive inside pages, only a record too large for
// any page waits for Load, and no record is read twice. Switching chats
// costs one page per chat, once.
func TestPageFirstScreenRequestBudget(t *testing.T) {
	root := t.TempDir()
	s := startLiveServerWith(t, liveOptions{fileRoot: root})
	cs := s.mcpSession(t.Context())
	sid := s.manager.ID()
	write := func(chat, id, content string) {
		call(t, cs, chat, "file_write", map[string]any{"server_id": sid, "operation_id": "op-budget-" + id,
			"path": filepath.Join(root, id+".txt"), "content": content})
	}
	// Recorded before the page opens: chat-b with small records; chat-c
	// with mid-size ones (about 470 KB of base64 each: two fit a page);
	// chat-a, the newest, with small ones and one too large for any page.
	for i := range 10 {
		write("chat-b", fmt.Sprintf("b%d", i), fmt.Sprintf("line %d\n", i))
	}
	for i := range 6 {
		write("chat-c", fmt.Sprintf("c%d", i), strings.Repeat("<", 55_000)+strconv.Itoa(i))
	}
	for i := range 20 {
		write("chat-a", fmt.Sprintf("a%d", i), fmt.Sprintf("line %d\n", i))
	}
	write("chat-a", "big", strings.Repeat("<", 620_000))
	for i := 20; i < 25; i++ {
		write("chat-a", fmt.Sprintf("a%d", i), fmt.Sprintf("line %d\n", i))
	}

	d := startPageDriver(t, s)
	d.signIn(s)
	d.do(map[string]any{"cmd": "reset"}, nil) // Count from the page's start, after sign-in.
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)
	var settled struct{ Current string }
	d.do(map[string]any{"cmd": "settle"}, &settled)
	if settled.Current != chatIdentity("chat-a") {
		t.Fatalf("the first screen shows %q, want the newest chat", settled.Current)
	}
	// The window's recent page already holds the newest chat whole, so the
	// first screen needs no lane page of its own.
	first := d.requests()
	if first.Counts["window"] != 1 || first.Counts["lane-items"] != 0 || first.Counts["stream"] != 1 || first.Counts["record"] != 0 {
		t.Fatalf("first screen requests: %+v", first.Counts)
	}
	// The large record does not end the page: every chat-a card is there,
	// and only the large one waits for Load.
	cards := d.cards(chatIdentity("chat-a"))
	if len(cards) != 26 {
		t.Fatalf("chat-a shows %d cards, want 26", len(cards))
	}
	var big journeyCard
	for _, c := range cards {
		if slices.Contains(c.Actions, "load-record") {
			if big.Key != "" || !strings.Contains(c.Text, "Large record") {
				t.Fatalf("an ordinary record waits for Load: %s", c.Text)
			}
			big = c
		}
	}
	if big.Key == "" {
		t.Fatal("the large record is not deferred")
	}
	// Loading it reads it once; switching away and back never again.
	for _, occ := range big.Occurrences {
		d.do(map[string]any{"cmd": "record", "occurrence": occ}, nil)
	}
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-b")}, nil)
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-a")}, nil)
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-b")}, nil)
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-a")}, nil)
	after := d.requests()
	if after.Counts["lane-items"] != 1 || after.Counts["window"] != 1 { // chat-b's first page, once.
		t.Fatalf("switching chats: %+v", after.Counts)
	}
	if len(after.Records) == 0 {
		t.Fatal("the large record was not loaded")
	}
	for occ, n := range after.Records {
		if n != 1 {
			t.Errorf("record %s read %d times", occ, n)
		}
	}

	// Mid-size records page through Earlier, every one inline.
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-c")}, nil)
	for range 10 {
		var e struct{ More bool }
		d.do(map[string]any{"cmd": "earlier"}, &e)
		if !e.More {
			break
		}
	}
	mid := d.cards(chatIdentity("chat-c"))
	if len(mid) != 6 {
		t.Fatalf("chat-c shows %d cards after paging, want 6", len(mid))
	}
	for _, c := range mid {
		if slices.Contains(c.Actions, "load-record") {
			t.Errorf("a mid-size record waits for Load: %.200s", c.Text)
		}
	}
	if n := d.requests().Counts["record"]; n != after.Counts["record"] {
		t.Errorf("paging mid-size records read %d records on their own", n-after.Counts["record"])
	}
}

// writeOld appends finished calls of a chat directly to the audit, to build
// history quickly (MCP calls would add nothing but time).
func (s *liveServer) writeOld(chat string, n int) {
	s.t.Helper()
	for i := range n {
		id := fmt.Sprintf("old-%s-%d", chat, i)
		for _, e := range []audit.Event{
			{ServerID: s.manager.ID(), RequestID: id, Tool: "file_read", Phase: "request_start",
				Args: map[string]any{"files": []map[string]any{{"path": fmt.Sprintf("/old/%s/%d.txt", chat, i)}}}, CallMeta: map[string]any{"openai/session": chat}},
			{ServerID: s.manager.ID(), RequestID: id, Tool: "file_read", Phase: "request_finish", Status: "ok"},
		} {
			if err := s.recorder.Write(audit.NewEvent(e)); err != nil {
				s.t.Fatal(err)
			}
		}
	}
}

// waitItems waits until a lane of the live view has at least n items.
func (d *pageDriver) waitItems(identity string, n int) journeyLane {
	d.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, l := range d.lanes() {
			if l.Identity == identity && l.Items >= n {
				return l
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("lane %q never had %d items: %+v", identity, n, d.lanes())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPageHistoryJourney (#10): a chat recorded only beyond the live
// window is found by Discover older conversations and read in the archive
// view; the live view meanwhile keeps its own chats and selection, and new
// live calls extend the same timeline.
func TestPageHistoryJourney(t *testing.T) {
	root := t.TempDir()
	s := startLiveServerWith(t, liveOptions{fileRoot: root, capacity: 250})
	s.writeOld("chat-old", 300)    // 600 rows, older than everything below.
	s.writeOld("chat-filler", 150) // 300 newer rows: the first history page holds none of chat-old.
	cs := s.mcpSession(t.Context())
	sid := s.manager.ID()
	for i := range 4 {
		call(t, cs, "chat-a", "file_write", map[string]any{"server_id": sid, "operation_id": fmt.Sprintf("op-hist-a%d", i),
			"path": filepath.Join(root, fmt.Sprintf("a%d.txt", i)), "content": "a"})
	}

	d := startPageDriver(t, s)
	d.signIn(s)
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)
	d.do(map[string]any{"cmd": "settle"}, nil)
	for _, l := range d.lanes() {
		if l.Identity == chatIdentity("chat-old") {
			t.Fatalf("the old chat is in the live window before any scan: %+v", l)
		}
	}
	// The owner reads another chat than the default one.
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-filler")}, nil)

	// Discover older conversations scans until the window is full; its
	// coverage then says older history needs the archive view.
	var disc struct {
		Coverage struct {
			End        string `json:"end"`
			UseArchive bool   `json:"use_archive"`
		}
	}
	for range 20 {
		d.do(map[string]any{"cmd": "discover"}, &disc)
		if disc.Coverage.UseArchive {
			break
		}
	}
	if !disc.Coverage.UseArchive || disc.Coverage.End != "evicted" {
		t.Fatalf("coverage after discovery: %+v", disc.Coverage)
	}

	// The archive view shows the old chat with its recorded input.
	var arch struct{ Lanes []journeyLane }
	d.do(map[string]any{"cmd": "archive"}, &arch)
	found := false
	for _, l := range arch.Lanes {
		found = found || l.Identity == chatIdentity("chat-old")
	}
	if !found {
		t.Fatalf("the archive has no old chat: %+v", arch.Lanes)
	}
	var old struct{ Cards []journeyCard }
	d.do(map[string]any{"cmd": "archiveCards", "identity": chatIdentity("chat-old")}, &old)
	if len(old.Cards) == 0 {
		t.Fatal("the archive shows no card of the old chat")
	}
	for _, c := range old.Cards {
		if !strings.Contains(c.Text, "/old/chat-old/") || slices.Contains(c.Actions, "load-record") {
			t.Fatalf("an archive card of another chat, or not loaded: %.300s", c.Text)
		}
	}
	d.do(map[string]any{"cmd": "closeArchive"}, nil)

	// Back in the live view: the owner's live selection is intact, and a
	// new call extends chat-a's timeline.
	var st struct{ Current string }
	d.do(map[string]any{"cmd": "settle"}, &st)
	if st.Current != chatIdentity("chat-filler") {
		t.Fatalf("live selection after the archive: %q", st.Current)
	}
	call(t, cs, "chat-a", "file_write", map[string]any{"server_id": sid, "operation_id": "op-hist-live",
		"path": filepath.Join(root, "live.txt"), "content": "live"})
	d.waitItems(chatIdentity("chat-a"), 5)
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-a")}, nil)
	cards := d.cards(chatIdentity("chat-a"))
	if len(cards) != 5 || !strings.Contains(cards[len(cards)-1].Text, "live.txt") {
		t.Fatalf("live chat-a cards: %d, last %.200s", len(cards), cards[len(cards)-1].Text)
	}
}

// journeyStatus is the part of the driver's status the tests read.
type journeyStatus struct {
	Status struct {
		Connection     string `json:"connection"`
		PossibleUnseen bool   `json:"possibleUnseen"`
	} `json:"status"`
	Window     string `json:"window"`
	Generation uint64 `json:"generation"`
}

// unread maps lane identities to their unread counts.
func (d *pageDriver) unread() map[string]int {
	d.t.Helper()
	out := map[string]int{}
	for _, l := range d.lanes() {
		if l.Unread > 0 {
			out[l.Identity] = l.Unread
		}
	}
	return out
}

// TestPageUnreadAndAutoSwitch (#10): unread counts logical actions per
// chat, once per poll group, and never again for rows delivered again
// (after a reconnect, or from an earlier cursor) or for a rebuilt window,
// which reports possible unseen activity instead; auto-switch follows only
// new non-polling starts in another claimed chat, outside the focus hold,
// waits while the owner reads, and never moves when it is off; request and
// effect durations stay separate. Earlier and mid-size paging are covered
// by TestPageFirstScreenRequestBudget, the archive by TestPageHistoryJourney;
// late origins and short-ID collisions by unit tests (internal/activity,
// internal/dashboard, attention.test.js).
func TestPageUnreadAndAutoSwitch(t *testing.T) {
	root := t.TempDir()
	s := startLiveServerWith(t, liveOptions{fileRoot: root})
	cs := s.mcpSession(t.Context())
	sid := s.manager.ID()
	d := startPageDriver(t, s)
	token := d.signIn(s)
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)
	n := 0
	write := func(chat string) {
		n++
		call(t, cs, chat, "file_write", map[string]any{"server_id": sid, "operation_id": fmt.Sprintf("op-unread-%d", n),
			"path": filepath.Join(root, fmt.Sprintf("u%d.txt", n)), "content": "x"})
	}
	type attentionState struct {
		Switches []string
		Pending  *string
		Current  string
	}
	attend := func(opts map[string]any) attentionState {
		t.Helper()
		opts["cmd"] = "attention"
		var a attentionState
		d.do(opts, &a)
		return a
	}
	// eventually waits until unread matches want.
	eventually := func(what string, want map[string]int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !maps.Equal(d.unread(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: unread %v, want %v", what, d.unread(), want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	unattributed := "unattributed"

	// The first chat is shown by default; the owner turns auto-switch on.
	write("chat-a")
	d.waitItems(chatIdentity("chat-a"), 1)
	attend(map[string]any{"autoSwitch": true, "clock": 100_000})
	// A new start in another chat moves the selection.
	write("chat-b")
	d.waitItems(chatIdentity("chat-b"), 1)
	if a := attend(map[string]any{}); a.Current != chatIdentity("chat-b") || len(a.Switches) != 1 {
		t.Fatalf("no switch to chat-b: %+v", a)
	}
	// Within the hold, another chat's start does not.
	attend(map[string]any{"clock": 101_000})
	write("chat-c")
	d.waitItems(chatIdentity("chat-c"), 1)
	if a := attend(map[string]any{}); a.Current != chatIdentity("chat-b") {
		t.Fatalf("switched within the hold: %+v", a)
	}
	// After the hold: polls and an unattributed call never switch. The
	// polls read a real session (started in the "unattributed" lane, so
	// the chats' counts stay as they are).
	started := call(t, cs, "", "terminal_start", map[string]any{"server_id": sid, "operation_id": "op-unread-poll", "argv": []string{"go", "version"},
		"cwd": root, "wait_for": "completion", "wait_ms": 5000})
	var startResult struct {
		Receipt struct {
			SessionID string `json:"session_id"`
		} `json:"receipt"`
	}
	if err := json.Unmarshal([]byte(toolText(started)), &startResult); err != nil || startResult.Receipt.SessionID == "" {
		t.Fatalf("terminal_start result %q: %v", toolText(started), err)
	}
	pollSession := startResult.Receipt.SessionID
	attend(map[string]any{"clock": 200_000})
	for range 6 {
		call(t, cs, "chat-c", "terminal_read", map[string]any{"session_id": pollSession, "wait_ms": 0})
	}
	call(t, cs, "", "file_write", map[string]any{"server_id": sid, "operation_id": "op-unread-nobody", "path": filepath.Join(root, "nobody.txt"), "content": "x"})
	d.waitItems(chatIdentity("chat-c"), 7)
	d.waitItems(unattributed, 2)
	if a := attend(map[string]any{}); a.Current != chatIdentity("chat-b") || len(a.Switches) != 1 {
		t.Fatalf("a poll or an unattributed call switched: %+v", a)
	}
	// The six polls are one group card.
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-c")}, nil)
	groups := 0
	for _, c := range d.cards(chatIdentity("chat-c")) {
		if c.Tool == "terminal_read" {
			groups++
			if c.Size != 6 {
				t.Errorf("a poll group of %d", c.Size)
			}
		}
	}
	if groups != 1 {
		t.Fatalf("%d poll groups, want 1", groups)
	}

	// Unread: chat-a 1, chat-b 1, chat-c 2 (a start, and six polls as one
	// group), the unattributed call in its own lane. Rendering is not
	// seeing; seeing a chat clears it.
	want := map[string]int{chatIdentity("chat-a"): 1, chatIdentity("chat-b"): 1, chatIdentity("chat-c"): 2, unattributed: 2}
	eventually("first activity", want)
	d.do(map[string]any{"cmd": "cards", "identity": chatIdentity("chat-b"), "seen": true}, nil)
	delete(want, chatIdentity("chat-b"))
	eventually("after seeing chat-b", want)

	// Auto-switch off: new work in another chat never moves the selection.
	attend(map[string]any{"autoSwitch": false, "clock": 300_000})
	write("chat-b")
	want[chatIdentity("chat-b")] = 1
	eventually("with auto-switch off", want)
	if a := attend(map[string]any{}); a.Current != chatIdentity("chat-c") || a.Pending != nil {
		t.Fatalf("auto-switch off moved or queued: %+v", a)
	}
	// Reading defers a switch; it happens once reading ends, and a manual
	// selection drops a deferred one.
	attend(map[string]any{"autoSwitch": true, "deferred": true, "clock": 400_000})
	write("chat-a")
	want[chatIdentity("chat-a")]++
	eventually("while reading", want)
	if a := attend(map[string]any{}); a.Current != chatIdentity("chat-c") || a.Pending == nil || *a.Pending != chatIdentity("chat-a") {
		t.Fatalf("not deferred: %+v", a)
	}
	if a := attend(map[string]any{"deferred": false, "resume": true}); a.Current != chatIdentity("chat-a") || a.Pending != nil {
		t.Fatalf("the deferred switch did not happen: %+v", a)
	}
	attend(map[string]any{"deferred": true, "clock": 500_000})
	write("chat-b")
	want[chatIdentity("chat-b")]++
	eventually("a second deferral", want)
	if a := attend(map[string]any{}); a.Pending == nil || *a.Pending != chatIdentity("chat-b") {
		t.Fatalf("no deferred switch to drop: %+v", a)
	}
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-c")}, nil)
	if a := attend(map[string]any{}); a.Pending != nil {
		t.Fatalf("a manual selection kept a deferred switch: %+v", a)
	}
	if a := attend(map[string]any{"deferred": false, "clock": 600_000, "resume": true}); a.Current != chatIdentity("chat-c") {
		t.Fatalf("a dropped switch happened later: %+v", a)
	}
	attend(map[string]any{"autoSwitch": false})

	// Calls made while the stream is down arrive once on reconnect, each
	// counted once.
	d.do(map[string]any{"cmd": "hold", "on": true}, nil)
	d.do(map[string]any{"cmd": "cut"}, nil)
	for range 3 {
		write("chat-a")
	}
	d.do(map[string]any{"cmd": "hold", "on": false}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)
	want[chatIdentity("chat-a")] += 3
	eventually("after a reconnect", want)
	// Rows delivered again from an earlier cursor change nothing.
	var cur struct{ Cursor string }
	d.do(map[string]any{"cmd": "cursor"}, &cur)
	for range 5 {
		write("chat-b")
	}
	want[chatIdentity("chat-b")] += 5
	eventually("five more calls", want)
	var newest struct{ Cursor string }
	d.do(map[string]any{"cmd": "cursor"}, &newest)
	var gen journeyStatus
	d.do(map[string]any{"cmd": "status"}, &gen)
	catchUps := d.requests().Counts["catch-up"]
	var rw struct{ Cursor string }
	d.do(map[string]any{"cmd": "rewind", "cursor": cur.Cursor}, &rw)
	var genAfter journeyStatus
	d.do(map[string]any{"cmd": "status"}, &genAfter)
	// The rewound catch-up moved past the earlier cursor again (rows that
	// arrived meanwhile, such as late finishes, may take it further than
	// the cursor read before), in the same generation.
	if rw.Cursor == cur.Cursor || newest.Cursor == cur.Cursor || genAfter.Generation != gen.Generation || d.requests().Counts["catch-up"] <= catchUps {
		t.Fatalf("the rows were not delivered again: cursor %q (earlier %q), generation %d -> %d", rw.Cursor, cur.Cursor, gen.Generation, genAfter.Generation)
	}
	if got := d.unread(); !maps.Equal(got, want) {
		t.Fatalf("after rows delivered again: unread %v, want %v", got, want)
	}

	// A rebuilt window (forced as in TestAPIResnapshotAfterRebuild): a new
	// snapshot of a new generation replaces the old one, without new
	// unread; the page says activity may have been missed instead.
	var st journeyStatus
	d.do(map[string]any{"cmd": "status"}, &st)
	if st.Status.PossibleUnseen {
		t.Fatal("possible unseen activity before any rebuild")
	}
	gen0 := st.Generation
	resp, err := http.DefaultClient.Do(s.request(dashboard.PathCatchUp, token,
		map[string]any{"window": st.Window, "generation": gen0, "after": "not-issued"}))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("forcing a rebuild: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		d.do(map[string]any{"cmd": "status"}, &st)
		if st.Generation > gen0 && st.Status.PossibleUnseen && st.Status.Connection == "live" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new snapshot after a rebuild: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.unread(); !maps.Equal(got, want) {
		t.Fatalf("after a rebuild: unread %v, want %v", got, want)
	}

	// Durations, and finishes never switch: a download in chat-b whose
	// effect outlasts its request finishes while the owner reads chat-a,
	// outside the hold.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // Finishes only once auto-switch is on (below).
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(func() { // Release a blocked handler first: Close waits for it.
		select {
		case <-release:
		default:
			close(release)
		}
		slow.Close()
	})
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-b")}, nil)
	itemsB := d.waitItems(chatIdentity("chat-b"), 1).Items
	call(t, cs, "chat-b", "file_receive", map[string]any{"server_id": sid, "operation_id": "op-unread-receive", "path": filepath.Join(root, "dl.bin"),
		"file": map[string]any{"download_url": slow.URL + "/f", "file_id": "f1"}, "wait_ms": 0})
	d.waitItems(chatIdentity("chat-b"), itemsB+1) // Its start is in; only the finish is to come.
	d.do(map[string]any{"cmd": "select", "identity": chatIdentity("chat-a")}, nil)
	before := attend(map[string]any{"autoSwitch": true, "clock": 900_000})
	close(release)
	var receive map[string]any
	deadline = time.Now().Add(30 * time.Second)
	for receive == nil {
		for _, c := range d.cards(chatIdentity("chat-b")) {
			if c.Tool != "file_receive" {
				continue
			}
			var it struct{ Item map[string]any }
			d.do(map[string]any{"cmd": "item", "key": c.Key}, &it)
			if e, _ := it.Item["effect"].(map[string]any); e != nil && e["state"] == "finished" {
				receive = it.Item
			}
		}
		if receive == nil && time.Now().After(deadline) {
			t.Fatal("the download never finished in the page")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if a := attend(map[string]any{}); a.Current != chatIdentity("chat-a") || len(a.Switches) != len(before.Switches) {
		t.Fatalf("a finish switched: %+v", a)
	}
	ms := func(v any) float64 {
		dur, _ := v.(map[string]any)["duration"].(map[string]any)
		switch dur["state"] {
		case "measured":
			return dur["milliseconds"].(float64)
		case "sub_millisecond":
			return 0
		}
		t.Fatalf("duration not measured: %v", v)
		return 0
	}
	request, effect := ms(receive["action"]), ms(receive["effect"])
	if effect < 350 || request >= effect {
		t.Fatalf("request %v ms, effect %v ms", request, effect)
	}
	raw, _ := json.Marshal(receive)
	if strings.Contains(string(raw), "runtime") {
		t.Fatalf("a download reports a process runtime: %s", raw)
	}
}

// TestPageEndsOnStopAndNeedsANewCode (#10): a normal stop ends an open
// page with server_stopping, also while record reads are queued, after
// which it makes no request; a restarted server refuses the old viewer
// token, so the page asks for a new code, which works.
func TestPageEndsOnStopAndNeedsANewCode(t *testing.T) {
	root := t.TempDir()
	s := startLiveServerWith(t, liveOptions{fileRoot: root})
	cs := s.mcpSession(t.Context())
	sid := s.manager.ID()
	for i := range 3 { // Records too large for any page: each waits for Load.
		call(t, cs, "chat-a", "file_write", map[string]any{"server_id": sid, "operation_id": fmt.Sprintf("op-stop-%d", i),
			"path": filepath.Join(root, fmt.Sprintf("s%d.txt", i)), "content": strings.Repeat("<", 620_000)})
	}
	d := startPageDriver(t, s)
	d.signIn(s)
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)
	d.do(map[string]any{"cmd": "settle"}, nil)
	// The owner clicks Load on all three; the reads are held, two in flight
	// and one queued, when the server stops.
	beforeLoads := d.requests().Counts["record"] // The page's own loads so far.
	d.do(map[string]any{"cmd": "hold", "on": true, "path": "record"}, nil)
	loads := 0
	for _, c := range d.cards(chatIdentity("chat-a")) {
		if slices.Contains(c.Actions, "load-record") {
			for _, occ := range c.Occurrences {
				d.do(map[string]any{"cmd": "load", "occurrence": occ}, nil)
				loads++
			}
		}
	}
	if loads < 3 {
		t.Fatalf("%d records to load, want 3", loads)
	}
	var ld struct{ Loading int }
	d.do(map[string]any{"cmd": "status"}, &ld)
	if ld.Loading < 3 {
		t.Fatalf("%d record reads queued, want 3", ld.Loading)
	}

	s.cancel() // A normal stop, as SIGTERM or `stop` do.
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return")
	}
	var end struct {
		Ended string `json:"ended"`
	}
	d.do(map[string]any{"cmd": "wait", "connection": "ended"}, &end)
	if end.Ended != "server_stopping" {
		t.Fatalf("the page ended with %q, want server_stopping", end.Ended)
	}
	var st struct {
		Text struct{ Connection string } `json:"text"`
	}
	d.do(map[string]any{"cmd": "status"}, &st)
	if st.Text.Connection != "Server stopped" {
		t.Fatalf("the page shows %q", st.Text.Connection)
	}
	// The held reads are released: the stopped page sends none of them.
	after := d.requests().Counts
	d.do(map[string]any{"cmd": "hold", "on": false, "path": "record"}, nil)
	var stopped struct {
		Loading int
		Stopped bool
	}
	deadline := time.Now().Add(15 * time.Second)
	for d.do(map[string]any{"cmd": "status"}, &stopped); stopped.Loading > 0; d.do(map[string]any{"cmd": "status"}, &stopped) {
		if time.Now().After(deadline) {
			t.Fatalf("%d record reads never ended", stopped.Loading)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // The client is stopped; this only guards against a regression.
	if again := d.requests().Counts; !stopped.Stopped || after["record"] != beforeLoads || !maps.Equal(after, again) {
		t.Fatalf("requests after the stop: %v, then %v (stopped %v)", after, again, stopped.Stopped)
	}

	// A restarted server has new sessions: the old token is refused and
	// the page ends with unauthorized (it asks for a new code).
	restarted := startLiveServerWith(t, liveOptions{})
	d.do(map[string]any{"cmd": "rebase", "url": "http://" + restarted.address}, nil)
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "ended"}, &end)
	if end.Ended != "unauthorized" {
		t.Fatalf("the old token on a restarted server: %q", end.Ended)
	}
	// A new code works.
	d.signIn(restarted)
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)
}

// TestPageBaseline (#10, opt-in: TERMINAL_MCP_BASELINE=1) measures, for
// docs/RELEASE_READINESS.md, the first screen over 50,000 recorded rows
// and MCP call latency with and without an open dashboard. A baseline on
// one machine, not a guarantee.
func TestPageBaseline(t *testing.T) {
	if os.Getenv("TERMINAL_MCP_BASELINE") != "1" {
		t.Skip("set TERMINAL_MCP_BASELINE=1 to measure the baseline")
	}
	s := startLiveServerWith(t, liveOptions{fileRoot: t.TempDir()})
	for c := range 50 {
		s.writeOld(fmt.Sprintf("chat-%02d", c), 500) // 50,000 rows in all.
	}
	cs := s.mcpSession(t.Context())
	latency := func(label string) {
		t.Helper()
		var d []time.Duration
		for range 300 {
			start := time.Now()
			if res := call(t, cs, "chat-bench", "terminal_list", map[string]any{}); res.IsError {
				t.Fatal(toolText(res))
			}
			d = append(d, time.Since(start))
		}
		slices.Sort(d)
		t.Logf("tools/call terminal_list %s: median %v, p95 %v", label, d[len(d)/2], d[len(d)*95/100])
	}
	for range 100 { // Warm up: connections, caches and the audit file.
		call(t, cs, "chat-bench", "terminal_list", map[string]any{})
	}
	latency("without a dashboard")

	d := startPageDriver(t, s)
	d.signIn(s)
	start := time.Now()
	d.do(map[string]any{"cmd": "start"}, nil)
	d.do(map[string]any{"cmd": "wait", "connection": "live"}, nil)
	d.do(map[string]any{"cmd": "settle"}, nil)
	t.Logf("first screen over 50,000 rows: %v, requests %v", time.Since(start), d.requests().Counts)
	start = time.Now()
	scans := 0
	for {
		var disc struct {
			Coverage struct {
				End string `json:"end"`
			}
		}
		d.do(map[string]any{"cmd": "discover"}, &disc)
		scans++
		if disc.Coverage.End != "open" {
			break
		}
	}
	t.Logf("discovering all 50 chats: %v in %d requests, %d lanes", time.Since(start), scans, len(d.lanes()))
	latency("with an open dashboard (live window and stream)")
}
