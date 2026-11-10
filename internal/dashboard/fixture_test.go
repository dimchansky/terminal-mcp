package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/viewer"
)

// TestManualDashboardFixture serves the dashboard over a scripted journal
// for manual checks in a browser. It is skipped unless
// TERMINAL_MCP_DASHBOARD_FIXTURE=1 and runs until interrupted:
//
//	TERMINAL_MCP_DASHBOARD_FIXTURE=1 go test -count=1 -run TestManualDashboardFixture -timeout 0 -v ./internal/dashboard
//
// It uses a temporary directory and a free loopback port (or
// TERMINAL_MCP_DASHBOARD_FIXTURE_ADDR), never the installed server or its
// files, prints the address and a pairing code, and keeps appending live
// activity through the real audit writer.
func TestManualDashboardFixture(t *testing.T) {
	if os.Getenv("TERMINAL_MCP_DASHBOARD_FIXTURE") != "1" {
		t.Skip("manual fixture; set TERMINAL_MCP_DASHBOARD_FIXTURE=1")
	}
	dir := filepath.Join(t.TempDir(), "logs")
	if err := audit.PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	writeRawFixtureRows(t, path)
	f := &fixture{t: t, path: path, server: "srv-fixture-1"}
	f.open()
	// A small per-window capacity (in occurrences, at least one history
	// page) with older rows than it holds closes the history chain, so the
	// archive view can be checked by hand.
	capacity, _ := strconv.Atoi(os.Getenv("TERMINAL_MCP_DASHBOARD_FIXTURE_CAPACITY"))
	for i := 0; i < capacity; i++ {
		f.chat("omega", "file_read", map[string]any{"path": fmt.Sprintf("old-%d.txt", i)}, true)
	}
	f.history()
	f.restart("srv-fixture-2")
	f.chat("alpha", "terminal_start", map[string]any{"argv": []string{"make", "check"}, "cwd": "/src/project"}, true)

	addr := os.Getenv("TERMINAL_MCP_DASHBOARD_FIXTURE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	host := ln.Addr().String()
	registry := viewer.NewRegistry(viewer.Config{Logger: slog.New(slog.DiscardHandler)})
	h, err := viewer.NewHandler(host, registry)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := audit.NewReader(f.log)
	if err != nil {
		t.Fatal(err)
	}
	f0, err := reader.FrontierCursor(context.Background())
	index := NewIndex(reader, f.server, f0, err == nil, f.log.Status)
	Register(h, NewManager(reader, ManagerConfig{ServerID: f.server, Capacity: capacity, Index: index, Status: f.log.Status}))
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	issue := func() {
		code, expires, err := registry.Issue()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("\nDashboard fixture: http://%s/dashboard/\nPairing code: %s (valid until %s)\nJournal: %s\n\n",
			host, code, expires.Format(time.TimeOnly), path)
	}
	issue()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	f.live(ctx, issue) // Returns on Ctrl-C, so the temporary journal is removed.
}

// writeRawFixtureRows writes rows the typed writer cannot produce: a legacy
// unversioned start, a malformed row and a start with duplicate keys.
func writeRawFixtureRows(t *testing.T, path string) {
	t.Helper()
	now := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	rows := []string{
		`{"time":"` + now + `","request_id":"legacy-1","tool":"file_write","phase":"start","args":{"path":"notes.txt","size":42}}`,
		`{not json at all`,
		`{"schema_version":2,"time":"` + now + `","server_id":"srv-fixture-0","request_id":"dup-1","tool":"file_read","phase":"request_start","args":{"path":"a.txt"},"args":{"path":"b.txt"},"call_meta":{"openai/session":"chat-gamma"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	t      *testing.T
	path   string
	server string
	log    *audit.Log
	n      int
	lastOp string // Operation ID of the last call that admitted one.
}

func (f *fixture) open() {
	log, err := audit.Open(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.log = log
	f.write(audit.Event{Tool: "server", Phase: "server_start", Status: "ok"})
}

func (f *fixture) restart(server string) {
	_ = f.log.Close()
	f.server = server
	f.open()
}

func (f *fixture) write(e audit.Event) {
	e.ServerID = f.server
	if err := f.log.Write(audit.NewEvent(e)); err != nil {
		f.t.Fatal(err)
	}
}

func meta(chat string) map[string]any {
	if chat == "" {
		return nil
	}
	return map[string]any{"openai/session": "chat-" + chat, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "fixture-client"}}
}

// chat writes one call; finished adds its finish (and effect, for mutations).
func (f *fixture) chat(chat, tool string, args map[string]any, finished bool) string {
	f.n++
	id := fmt.Sprintf("req-%04d", f.n)
	op := ""
	if _, ok := map[string]bool{"terminal_start": true, "file_write": true, "file_edit": true, "file_receive": true, "terminal_write": true}[tool]; ok {
		op = fmt.Sprintf("op-%04d", f.n)
		args["operation_id"] = op
		f.lastOp = op
	}
	f.write(audit.Event{RequestID: id, Tool: tool, Phase: "request_start", OperationID: op, Args: args, CallMeta: meta(chat)})
	if op != "" {
		f.write(audit.Event{RequestID: id, OperationID: op, Tool: tool, Phase: "operation_start"})
	}
	if finished {
		if op != "" {
			f.write(audit.Event{RequestID: id, OperationID: op, Tool: tool, Phase: "operation_finish", Status: "ok", DurationMS: 12})
		}
		f.write(audit.Event{RequestID: id, OperationID: op, Tool: tool, Phase: "request_finish", Status: "ok", DurationMS: 340})
	}
	return id
}

func (f *fixture) history() {
	f.chat("alpha", "terminal_start", map[string]any{"argv": []string{"go", "test", "-race", "./..."}, "env": map[string]string{"GOFLAGS": "-count=1"}, "cwd": "/src/project"}, true)
	f.chat("alpha", "terminal_start", map[string]any{"argv": []string{"printf", "it's a \"quoted\" $HOME", "tab\there"}, "env": map[string]string{"BAD-NAME": "1"}}, true)
	f.chat("alpha", "terminal_start", map[string]any{"argv": []string{"/bin/zsh", "-lc", "cd web && npm test | tee out.log"}}, true)
	for i := 0; i < 8; i++ {
		f.chat("alpha", "terminal_read", map[string]any{"session_id": "sess-1", "wait_ms": 2000}, true)
	}
	f.chat("alpha", "file_edit", map[string]any{"path": "main.go", "expected_version": "sha256:abc",
		"edits": []map[string]string{{"old_text": "timeout := 5 * time.Second\n", "new_text": "timeout := 10 * time.Second\n"},
			{"old_text": "func a() {\n\treturn\n}\n", "new_text": "func a() {\n\tlog.Print(\"a\")\n\treturn\n}\n"}}}, true)
	f.chat("alpha", "file_write", map[string]any{"path": "README.md", "content": "# Title\n\nSome text with a bidi \u202Eoverride\u202C and a tab\t.\n"}, true)
	written := f.lastOp
	f.chat("beta", "file_receive", map[string]any{"server_id": f.server, "file": map[string]any{"download_url": "https://files.example/a.zip?sig=secret-token", "file_id": "file-1"}, "path": "a.zip"}, true)
	received := f.lastOp
	for i := 0; i < 5; i++ {
		f.chat("beta", "file_receive_result", map[string]any{"server_id": f.server, "operation_id": received}, true)
	}
	f.chat("beta", "file_read", map[string]any{"path": "/etc/hosts"}, true)
	f.chat("", "terminal_list", map[string]any{}, true)
	// A rejected call: input not recorded, tool not executed.
	f.n++
	f.write(audit.Event{RequestID: fmt.Sprintf("req-%04d", f.n), Tool: "file_write", Phase: "request_rejected", Status: "rejected", Reason: "audit_record_too_large"})
	// A replay of an earlier operation ID.
	f.n++
	replay := fmt.Sprintf("req-%04d", f.n)
	f.write(audit.Event{RequestID: replay, Tool: "file_write", Phase: "request_start", OperationID: written,
		Args: map[string]any{"operation_id": written, "path": "README.md", "content": "replayed"}, CallMeta: meta("alpha")})
	f.write(audit.Event{RequestID: replay, Tool: "file_write", Phase: "request_finish", OperationID: written, Status: "ok", Replayed: true})
	// A large record (about 3 MiB), deferred in pages.
	f.chat("beta", "file_write", map[string]any{"path": "big.txt", "content": strings.Repeat("0123456789abcdef\n", 3<<20/17)}, true)
}

// live appends activity until the test is interrupted.
func (f *fixture) live(ctx context.Context, issue func()) {
	chats := []string{"alpha", "beta", "delta", ""}
	reissue := time.NewTicker(4 * time.Minute)
	defer reissue.Stop()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var pending, pendingOp string
	for i := 0; ; {
		select {
		case <-ctx.Done():
			return
		case <-reissue.C:
			issue()
		case <-tick.C:
			i++
			chat := chats[i%len(chats)]
			switch i % 7 {
			case 0:
				f.chat(chat, "terminal_start", map[string]any{"argv": []string{"go", "build", fmt.Sprintf("./cmd/%d", i)}}, true)
			case 1:
				for k := 0; k < 4; k++ {
					f.chat(chat, "terminal_read", map[string]any{"session_id": "sess-live", "wait_ms": 1000}, true)
				}
			case 2:
				// The request answers at once; the download (its effect) runs on.
				pending = f.chat(chat, "file_receive", map[string]any{"server_id": f.server, "file": map[string]any{"download_url": "http://127.0.0.1:9/slow.bin"}, "path": "slow.bin"}, false)
				pendingOp = f.lastOp
				f.write(audit.Event{RequestID: pending, OperationID: pendingOp, Tool: "file_receive", Phase: "request_finish", Status: "ok", DurationMS: 2000})
			case 3:
				f.chat(chat, "file_edit", map[string]any{"path": "live.go", "edits": []map[string]string{{"old_text": fmt.Sprint("v", i-1), "new_text": fmt.Sprint("v", i)}}}, true)
			case 4:
				if pending != "" {
					f.write(audit.Event{RequestID: pending, OperationID: pendingOp, Tool: "file_receive", Phase: "operation_finish", Status: "ok", DurationMS: 9000})
					pending = ""
				}
			case 5:
				f.chat(chat, "file_read", map[string]any{"path": fmt.Sprintf("notes-%d.md", i)}, true)
			case 6:
				f.n++
				f.write(audit.Event{RequestID: fmt.Sprintf("req-%04d", f.n), Tool: "file_write", Phase: "request_rejected", Status: "rejected", Reason: "audit_record_too_large"})
			}
		}
	}
}
