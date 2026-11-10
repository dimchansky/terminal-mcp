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
	"net/http"
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
	Key         string   `json:"key"`
	Tool        string   `json:"tool"`
	Size        int      `json:"size"`
	Text        string   `json:"text"`
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

	d.waitLane(chatIdentity("chat-a"), 10)
	a := d.cards(chatIdentity("chat-a"))
	if len(a) != 10 {
		t.Fatalf("chat-a has %d cards, want 10", len(a))
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
		expect(start, "GOFLAGS=-count=1 go test -race ./...\n", "not a shell invocation", "Working directory "+root)
		if !hasAction(start, "copy-command") {
			t.Error("an exact command is not copyable")
		}
		expect(quoted, `BAD-NAME=1 printf 'it'\''s a "quoted" $HOME' 'tab\there'`, "Not copyable", "it's a \"quoted\" $HOME\n")
		if hasAction(quoted, "copy-command") {
			t.Error("a command that cannot round-trip is copyable")
		}
	}
	expect(start, "The audit records the launch, not the program's output.")
	reject(start, "go.mod file not found", "no Go files", "no packages")
	checkRecords(start, op("start"))

	// Schema-rejected input stays visible field by field and never ran.
	bad := byOp(a, op("bad-argv"))
	expect(bad, "rejected", "Supplied fields\nserver_id", "argv\nls -la\n", "schema_validation")
	reject(bad, "effect")
	checkRecords(bad, "")
	// Terminal input with visible escapes for ANSI, bidi and control bytes.
	write := byOp(a, op("write"))
	expect(write, `text echo \e[31mred\e[0m \u202Eevil\u202C\0\x07\r\n`, "key CTRL_C")
	checkRecords(write, "")
	// file_write content in full; every requested edit as a red/green pair.
	file := byOp(a, op("file"))
	expect(file, "File "+mainGo, original, "effect applied")
	checkRecords(file, op("file"))
	one := byOp(a, op("edit1"))
	expect(one, "Requested replacement 1 of 1", "- 5 * time.Second\n+ 10 * time.Second")
	checkRecords(one, op("edit1"))
	edit := byOp(a, op("edit"))
	expect(edit, "Requested replacement 1 of 2", "  \ttimeout := 10 * time.Second\n- \t_ = timeout\n+ \tlog.Print(timeout)",
		"Requested replacement 2 of 2", "+ // main runs.", "+ \tlog.Print(\"start\")", "no file line numbers are recorded")
	checkRecords(edit, op("edit"))
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
	expect(b[0], "replay of operation "+op("file")+" (no second effect)", "File "+mainGo, "openai/session\nchat-b")
	reject(b[0], "effect applied", "openai/session\nchat-a")
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
	expect(rejected, "Input not recorded · tool not executed", "audit_record_too_large", "audit event exceeds maximum file size")
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
