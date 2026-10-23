//go:build linux || darwin

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProcessHelper(t *testing.T) {
	if os.Getenv("TERMINAL_MCP_TEST_CHILD") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(90)
	}
	mode := args[1]
	switch mode {
	case "both":
		fmt.Fprint(os.Stdout, "OUT")
		fmt.Fprint(os.Stderr, "ERR")
		os.Exit(7)
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "silent":
		time.Sleep(10 * time.Second)
	case "flood":
		until := time.Now().Add(3 * time.Second)
		for time.Now().Before(until) {
			_, _ = os.Stdout.Write([]byte(strings.Repeat("x", 1024)))
		}
	case "slow":
		for i := 0; i < 100; i++ {
			fmt.Fprint(os.Stdout, "x")
			time.Sleep(20 * time.Millisecond)
		}
	case "interrupt":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT)
		fmt.Fprint(os.Stdout, "READY")
		<-ch
		fmt.Fprint(os.Stdout, "INTERRUPTED")
	case "once":
		f, err := os.OpenFile(args[2], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(91)
		}
		_, _ = f.WriteString("once\n")
		_ = f.Close()
	case "env":
		cwd, _ := os.Getwd()
		fmt.Fprintf(os.Stdout, "%s|%s|%s", cwd, os.Getenv("TEST_VALUE"), os.Getenv("TERMINAL_MCP_TOKEN"))
	case "closed-output":
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		time.Sleep(time.Second)
	case "inherited":
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessHelper", "--", "hold-output")
		cmd.Env = os.Environ()
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if cmd.Start() != nil {
			os.Exit(92)
		}
	case "hold-output":
		time.Sleep(1500 * time.Millisecond)
	default:
		os.Exit(93)
	}
	os.Exit(0)
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	m := New(nil)
	t.Cleanup(m.Close)
	return m
}

func helperArgs(m *Manager, id, mode string, rest ...string) StartArgs {
	return StartArgs{ServerID: m.id, OperationID: id, Argv: append([]string{os.Args[0], "-test.run=TestProcessHelper", "--", mode}, rest...), Env: map[string]string{"TERMINAL_MCP_TEST_CHILD": "1"}}
}

func startHelper(t *testing.T, m *Manager, id, mode string) string {
	t.Helper()
	r, err := m.Start(context.Background(), helperArgs(m, id, mode))
	if err != nil || r.Receipt.Status != "applied" {
		t.Fatalf("start failed: %#v %v", r, err)
	}
	return r.Receipt.SessionID
}

func readToEnd(t *testing.T, m *Manager, id string) (Observation, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cursor := ""
	var all []byte
	for {
		o, err := m.Read(ctx, ReadArgs{SessionID: id, Cursor: cursor, WaitMS: 100, MaxBytes: m.limits.MaxReadBytes})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, eventBytes(o.Events)...)
		cursor = o.NextCursor
		if o.Session.Exit != nil && o.Session.OutputDone && !o.HasMore {
			return o, all
		}
		select {
		case <-ctx.Done():
			t.Fatal("never drained")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestPipesStreamsExitAndReplayRead(t *testing.T) {
	m := testManager(t)
	id := startHelper(t, m, "both", "both")
	o, data := readToEnd(t, m, id)
	if o.Session.Exit.Code == nil || *o.Session.Exit.Code != 7 || len(data) != 6 || !strings.Contains(string(data), "OUT") || !strings.Contains(string(data), "ERR") {
		t.Fatalf("bad exit/output: %#v %q", o, data)
	}
	r1, _ := m.Read(context.Background(), ReadArgs{SessionID: id})
	r2, _ := m.Read(context.Background(), ReadArgs{SessionID: id})
	if string(eventBytes(r1.Events)) != string(eventBytes(r2.Events)) {
		t.Fatal("read consumed output")
	}
}

func TestStartRetriesAreAtMostOnce(t *testing.T) {
	m := testManager(t)
	path := filepath.Join(t.TempDir(), "counter")
	a := helperArgs(m, "once", "once", path)
	var wg sync.WaitGroup
	ids := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := m.Start(context.Background(), a)
			if e != nil {
				t.Error(e)
				return
			}
			ids <- r.Receipt.SessionID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for next := range ids {
		if id != "" && id != next {
			t.Fatal("duplicate process")
		}
		id = next
	}
	readToEnd(t, m, id)
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "once\n" {
		t.Fatalf("executed more than once: %q %v", b, err)
	}
	a.Argv = append(a.Argv, "different")
	if _, err := m.Start(context.Background(), a); err == nil {
		t.Fatal("conflicting start accepted")
	}
}

func TestStaleServerIDAndValidation(t *testing.T) {
	m := testManager(t)
	a := helperArgs(m, "stale", "silent")
	a.ServerID = "old"
	if _, err := m.Start(context.Background(), a); err == nil {
		t.Fatal("stale server accepted")
	}
	a = helperArgs(m, "bad", "silent")
	a.Argv = []string{"x\x00y"}
	if _, err := m.Start(context.Background(), a); err == nil {
		t.Fatal("NUL accepted")
	}
	a = helperArgs(m, "bad2", "silent")
	a.Mode = "unknown"
	if _, err := m.Start(context.Background(), a); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestWriteRetryAndEOF(t *testing.T) {
	m := testManager(t)
	id := startHelper(t, m, "echo", "echo")
	text := "hello\n"
	a := WriteArgs{ServerID: m.id, OperationID: "write1", SessionID: id, Input: []Input{{Text: &text}}}
	one, e := m.Write(context.Background(), a)
	if e != nil || one.Receipt.BytesWritten != len(text) {
		t.Fatalf("write: %#v %v", one, e)
	}
	two, e := m.Write(context.Background(), a)
	if e != nil || !two.Replayed {
		t.Fatal("not replayed")
	}
	_, e = m.Write(context.Background(), WriteArgs{ServerID: m.id, OperationID: "eof", SessionID: id, CloseStdin: true})
	if e != nil {
		t.Fatal(e)
	}
	_, data := readToEnd(t, m, id)
	if string(data) != text {
		t.Fatalf("duplicated or changed input: %q", data)
	}
}

func TestReadCancelDoesNotStopProcess(t *testing.T) {
	m := testManager(t)
	id := startHelper(t, m, "silent", "silent")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := m.Read(ctx, ReadArgs{SessionID: id, WaitMS: 5000})
	if err == nil {
		t.Fatal("cancelled read returned no error")
	}
	o, err := m.Read(context.Background(), ReadArgs{SessionID: id})
	if err != nil || o.Session.ProcessState != "running" {
		t.Fatal("cancellation killed process")
	}
}

func TestReadNeverBlocksSignal(t *testing.T) {
	m := testManager(t)
	id := startHelper(t, m, "signal", "interrupt")
	ready, err := m.Read(context.Background(), ReadArgs{SessionID: id, Until: "READY", WaitMS: 5000})
	if err != nil || ready.ReturnReason != "matched" {
		t.Fatalf("not ready: %#v %v", ready, err)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = m.Read(context.Background(), ReadArgs{SessionID: id, Cursor: ready.NextCursor, WaitMS: 5000})
	}()
	start := time.Now()
	r, err := m.Signal(context.Background(), SignalArgs{ServerID: m.id, OperationID: "int", SessionID: id, Signal: "INT", Target: "group"})
	if err != nil || r.Receipt.Status != "applied" {
		t.Fatalf("signal failed: %#v %v", r, err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("signal waited for read")
	}
	_, data := readToEnd(t, m, id)
	if !strings.Contains(string(data), "INTERRUPTED") {
		t.Fatal("process did not receive SIGINT")
	}
	<-readDone
}

func TestAbsoluteWaitUnderContinuousOutput(t *testing.T) {
	m := testManager(t)
	id := startHelper(t, m, "slow", "slow")
	start := time.Now()
	o, err := m.Read(context.Background(), ReadArgs{SessionID: id, Until: "never", WaitMS: 120})
	if err != nil || o.ReturnReason != "deadline" {
		t.Fatalf("bad wait: %#v %v", o, err)
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("output extended deadline: %v", elapsed)
	}
}

func TestFloodIsBoundedAndReportsGap(t *testing.T) {
	m := testManager(t)
	m.limits.JournalBytes = 4096
	m.limits.JournalEvents = 8
	id := startHelper(t, m, "flood", "flood")
	deadline := time.Now().Add(3 * time.Second)
	for {
		o, err := m.Read(context.Background(), ReadArgs{SessionID: id, MaxBytes: 512})
		if err != nil {
			t.Fatal(err)
		}
		if o.Gap != nil {
			if len(eventBytes(o.Events)) > 512 || !o.HasMore {
				t.Fatal("unbounded response")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flood produced no gap")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBlockedInputIsBoundedAndDoesNotBlockKill(t *testing.T) {
	m := testManager(t)
	m.limits.WriteTimeoutMS = 150
	m.limits.MaxInputBytes = 2 << 20 // Exceed platform-dependent pipe capacity in this test.
	id := startHelper(t, m, "blocked", "silent")
	text := strings.Repeat("x", m.limits.MaxInputBytes)
	a := WriteArgs{ServerID: m.id, OperationID: "large-input", SessionID: id, Input: []Input{{Text: &text}}}
	done := make(chan ActionResult, 1)
	go func() {
		r, e := m.Write(context.Background(), a)
		if e != nil {
			t.Error(e)
		}
		done <- r
	}()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	_, err := m.Signal(context.Background(), SignalArgs{ServerID: m.id, OperationID: "kill", SessionID: id, Signal: "KILL"})
	if err != nil || time.Since(start) > 500*time.Millisecond {
		t.Fatal("blocked stdin prevented kill")
	}
	select {
	case r := <-done:
		if r.Receipt.Status != "partial" && r.Receipt.Status != "failed" {
			t.Fatalf("expected partial/failed write, got %#v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("stdin write did not finish")
	}
}

func TestWriteDeadlinePartialReceiptIsStable(t *testing.T) {
	m := testManager(t)
	m.limits.WriteTimeoutMS = 100
	m.limits.MaxInputBytes = 2 << 20 // Exercise a partial write, not a fully buffered one.
	id := startHelper(t, m, "timeout-input", "silent")
	text := strings.Repeat("x", m.limits.MaxInputBytes)
	a := WriteArgs{ServerID: m.id, OperationID: "partial", SessionID: id, Input: []Input{{Text: &text}}}
	start := time.Now()
	r, err := m.Write(context.Background(), a)
	if err != nil || r.Receipt.Status != "partial" || r.Receipt.BytesWritten == 0 || time.Since(start) > time.Second {
		t.Fatalf("partial write: %#v %v", r, err)
	}
	replay, err := m.Write(context.Background(), a)
	if err != nil || !replay.Replayed || replay.Receipt != r.Receipt {
		t.Fatal("partial action was not recovered")
	}
}

func TestRuntimeLimitAndClosedOutputState(t *testing.T) {
	m := testManager(t)
	a := helperArgs(m, "runtime", "silent")
	a.MaxRuntimeMS = 100
	r, err := m.Start(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := readToEnd(t, m, r.Receipt.SessionID)
	if !strings.Contains(strings.Join(o.Session.Diagnostics, " "), "maximum_runtime_exceeded") {
		t.Fatal("missing runtime deadline diagnostic")
	}
	id := startHelper(t, m, "closed-output", "closed-output")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		o, e := m.Read(ctx, ReadArgs{SessionID: id, WaitMS: 10})
		if e != nil {
			t.Fatal(e)
		}
		if o.Session.OutputDone {
			if o.Session.ProcessState != "running" {
				t.Fatal("confused EOF and process exit")
			}
			break
		}
	}
}

func TestEnvironmentAndWorkingDirectory(t *testing.T) {
	t.Setenv("TERMINAL_MCP_TOKEN", "must-not-inherit")
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "cwd-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		cwd  string
	}{
		{"direct", dir},
		{"symlink", link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(t)
			a := helperArgs(m, "env", "env")
			a.Cwd = tc.cwd
			a.Env["TEST_VALUE"] = "value"
			r, err := m.Start(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			_, data := readToEnd(t, m, r.Receipt.SessionID)
			gotDir, ok := strings.CutSuffix(string(data), "|value|")
			if !ok {
				t.Fatalf("wrong environment: %q", data)
			}

			// Getwd may return a physical path instead of the supplied symlink.
			gotInfo, err := os.Stat(gotDir)
			if err != nil {
				t.Fatalf("stat child cwd %q: %v", gotDir, err)
			}
			wantInfo, err := os.Stat(tc.cwd)
			if err != nil {
				t.Fatalf("stat requested cwd %q: %v", tc.cwd, err)
			}
			if !os.SameFile(gotInfo, wantInfo) {
				t.Fatalf("wrong working directory: got %q, want %q", gotDir, tc.cwd)
			}
		})
	}
}

func TestExecutableUsesChildPATH(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "custom")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nprintf located\n"), 0700); err != nil {
		t.Fatal(err)
	}
	m := testManager(t)
	r, e := m.Start(context.Background(), StartArgs{ServerID: m.id, OperationID: "path", Argv: []string{"custom"}, Env: map[string]string{"PATH": dir}})
	if e != nil || r.Receipt.Status != "applied" {
		t.Fatalf("PATH lookup failed: %#v %v", r, e)
	}
	_, data := readToEnd(t, m, r.Receipt.SessionID)
	if string(data) != "located" {
		t.Fatal("custom PATH ignored")
	}
}

func TestActiveLimitAndRetainedEviction(t *testing.T) {
	m := testManager(t)
	m.limits.ActiveSessions = 1
	m.limits.RetainedSessions = 1
	id := startHelper(t, m, "first", "silent")
	r, e := m.Start(context.Background(), helperArgs(m, "over-limit", "silent"))
	if e != nil || r.Receipt.Status != "failed" {
		t.Fatal("active limit ignored")
	}
	_, e = m.CloseSession(context.Background(), CloseArgs{ServerID: m.id, OperationID: "close-first", SessionID: id})
	if e != nil {
		t.Fatal(e)
	}
	readToEnd(t, m, id)
	id2 := startHelper(t, m, "second", "both")
	readToEnd(t, m, id2)
	list, _ := m.List(context.Background(), ListArgs{})
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != id2 {
		t.Fatal("completed sessions not bounded")
	}
	r, e = m.Start(context.Background(), helperArgs(m, "first", "silent"))
	if e != nil || !r.Replayed || r.Receipt.SessionID != id || r.ObservationError == "" {
		t.Fatal("evicted start reran or expiry not reported")
	}
}

func TestInheritedOutputGetsDrainDeadline(t *testing.T) {
	m := testManager(t)
	id := startHelper(t, m, "inherited", "inherited")
	o, _ := readToEnd(t, m, id)
	if !o.Session.OutputIncomplete {
		t.Fatal("inherited output incorrectly reported complete")
	}
}

func TestFutureAndCrossSessionCursorRejected(t *testing.T) {
	m := testManager(t)
	id := startHelper(t, m, "cursor", "silent")
	for _, cursor := range []string{makeCursor(id, 100), makeCursor("different", 0)} {
		if _, err := m.Read(context.Background(), ReadArgs{SessionID: id, Cursor: cursor}); err == nil {
			t.Fatal("invalid cursor accepted")
		}
		text := "should not be sent"
		if r, err := m.Write(context.Background(), WriteArgs{ServerID: m.id, OperationID: "invalid-cursor", SessionID: id, Cursor: cursor, Input: []Input{{Text: &text}}}); err == nil && r.Receipt.Status != "failed" {
			t.Fatal("invalid observation caused mutation")
		}
	}
}

func TestFailedStartRetryAndShutdown(t *testing.T) {
	m := testManager(t)
	a := StartArgs{ServerID: m.id, OperationID: "missing", Argv: []string{"/definitely/missing/program"}}
	r, e := m.Start(context.Background(), a)
	if e != nil || r.Receipt.Status != "failed" {
		t.Fatal("missing start not recorded")
	}
	r, e = m.Start(context.Background(), a)
	if e != nil || !r.Replayed {
		t.Fatal("failed start not replayed")
	}
	id := startHelper(t, m, "shutdown", "silent")
	m.Close()
	s, _ := m.get(id)
	s.mu.Lock()
	exited := s.exit != nil
	s.mu.Unlock()
	if !exited {
		t.Fatal("shutdown did not stop process")
	}
	if _, err := m.Start(context.Background(), helperArgs(m, "after", "silent")); err == nil {
		t.Fatal("accepted start after shutdown")
	}
}

func TestInputValidation(t *testing.T) {
	m := testManager(t)
	text, raw := "x", "@@bad-base64@@"
	bad := []WriteArgs{{Input: []Input{{}}}, {Input: []Input{{Text: &text, Key: "ENTER"}}}, {Input: []Input{{BytesBase64: &raw}}}, {Input: []Input{{Key: "UNKNOWN"}}}, {}}
	for _, a := range bad {
		if err := m.validateInput(a, false); err == nil {
			t.Fatalf("accepted %#v", a)
		}
	}
	if err := m.validateInput(WriteArgs{CloseStdin: true}, true); err == nil {
		t.Fatal("PTY EOF accepted as pipe closure")
	}
}

func TestMutationReceiptsSurviveSessionEviction(t *testing.T) {
	m := testManager(t)
	id := startHelper(t, m, "retained", "silent")
	ctx := context.Background()
	text := "buffered input"
	write := WriteArgs{ServerID: m.id, OperationID: "old-write", SessionID: id, Input: []Input{{Text: &text}}}
	signal := SignalArgs{ServerID: m.id, OperationID: "old-signal", SessionID: id, Signal: "CONT"}
	closeArgs := CloseArgs{ServerID: m.id, OperationID: "old-close", SessionID: id}
	for _, run := range []func() (ActionResult, error){
		func() (ActionResult, error) { return m.Write(ctx, write) },
		func() (ActionResult, error) { return m.Signal(ctx, signal) },
		func() (ActionResult, error) { return m.CloseSession(ctx, closeArgs) },
	} {
		r, err := run()
		if err != nil || r.Receipt.Status != "applied" {
			t.Fatalf("first action: %#v %v", r, err)
		}
	}
	readToEnd(t, m, id)
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	for _, run := range []func() (ActionResult, error){
		func() (ActionResult, error) { return m.Write(ctx, write) },
		func() (ActionResult, error) { return m.Signal(ctx, signal) },
		func() (ActionResult, error) { return m.CloseSession(ctx, closeArgs) },
	} {
		r, err := run()
		if err != nil || !r.Replayed || r.Receipt.Status != "applied" || r.ObservationError == "" {
			t.Fatalf("expired replay: %#v %v", r, err)
		}
	}
}

func TestListDoesNotEchoLargeArguments(t *testing.T) {
	m := testManager(t)
	a := helperArgs(m, "large-args", "silent")
	a.Argv = append(a.Argv, strings.Repeat("x", 60000))
	r, err := m.Start(context.Background(), a)
	if err != nil || r.Receipt.Status != "applied" {
		t.Fatalf("start: %#v %v", r, err)
	}
	list, err := m.List(context.Background(), ListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(list)
	if err != nil || len(data) > 4096 {
		t.Fatal("list echoed large launch metadata")
	}
	obs, err := m.Read(context.Background(), ReadArgs{SessionID: r.Receipt.SessionID})
	if err != nil || len(obs.Session.Argv[len(obs.Session.Argv)-1]) != 60000 {
		t.Fatal("detail lost original argv")
	}
}

func TestMutationRejectsMalformedSessionIDBeforeReservation(t *testing.T) {
	m := testManager(t)
	text := "x"
	_, err := m.Write(context.Background(), WriteArgs{ServerID: m.id, OperationID: "bad-id", SessionID: strings.Repeat("x", 10000), Input: []Input{{Text: &text}}})
	if err == nil || m.ops.count() != 0 {
		t.Fatal("malformed ID consumed ledger space")
	}
}
