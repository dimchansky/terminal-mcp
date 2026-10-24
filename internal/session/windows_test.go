//go:build windows

package session_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terminalmcp/internal/session"
	"terminalmcp/internal/terminal"
)

func windowsCommand() string {
	if shell := os.Getenv("ComSpec"); shell != "" {
		return shell
	}
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
}

func TestWindowsManagedPipesAndConPTY(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
	}{
		{"pipes", "WINDOWS_PIPES_OK"},
		{"pty", "WINDOWS_PTY_OK"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			m := session.New(terminal.Start)
			defer m.Close()
			r, err := m.Start(context.Background(), session.StartArgs{
				ServerID: m.ID(), OperationID: "windows-" + tc.mode,
				Argv: []string{windowsCommand(), "/d", "/c", "echo " + tc.want},
				Cwd:  t.TempDir(), Mode: tc.mode,
				CloseOnExit: true, WaitMS: 3000, WaitFor: "completion",
			})
			if err != nil || r.Receipt.Status != "applied" {
				t.Fatalf("start %s: %+v, %v", tc.mode, r, err)
			}
			if r.Receipt.SessionID == "" {
				t.Fatal("missing session ID")
			}
			seen := false
			obs, err := m.Read(context.Background(), session.ReadArgs{
				SessionID: r.Receipt.SessionID, WaitFor: "completion", WaitMS: 5000,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range obs.Events {
				if event.Text != nil && strings.Contains(*event.Text, tc.want) {
					seen = true
				}
			}
			if !seen {
				t.Fatalf("Windows %s output not observed: %+v", tc.mode, obs)
			}
			if obs.Session.Exit == nil || obs.Session.Exit.Code == nil || *obs.Session.Exit.Code != 0 {
				t.Fatalf("unexpected exit: %+v", obs.Session)
			}
		})
	}
}

func TestWindowsJobCloseStopsChild(t *testing.T) {
	m := session.New(nil)
	defer m.Close()
	r, err := m.Start(context.Background(), session.StartArgs{
		ServerID: m.ID(), OperationID: "job-close",
		Argv: []string{windowsCommand(), "/d", "/c", "ping -n 30 127.0.0.1 >nul"},
		Cwd:  t.TempDir(), Mode: "pipes", WaitMS: 0,
	})
	if err != nil || r.Receipt.Status != "applied" {
		t.Fatalf("cannot start background command: %+v %v", r, err)
	}
	stop, err := m.CloseSession(context.Background(), session.CloseArgs{
		ServerID: m.ID(), OperationID: "close-job", SessionID: r.Receipt.SessionID,
	})
	if err != nil || stop.Receipt.Status != "applied" {
		t.Fatalf("job close failed: %+v %v", stop, err)
	}
	obs, err := m.Read(context.Background(), session.ReadArgs{
		SessionID: r.Receipt.SessionID, WaitFor: "completion", WaitMS: 5000,
	})
	if err != nil || obs.Session.Exit == nil || !obs.Session.Closed {
		t.Fatalf("job did not close and reap: %+v %v", obs, err)
	}
}

func TestWindowsConPTYAcceptsInteractiveInput(t *testing.T) {
	m := session.New(terminal.Start)
	defer m.Close()
	r, err := m.Start(context.Background(), session.StartArgs{
		ServerID: m.ID(), OperationID: "interactive-start", Mode: "pty",
		Cwd: t.TempDir(), Argv: []string{windowsCommand(), "/d", "/v:on", "/c",
			"set /p X= && echo CONPTY_GOT_!X!"},
		WaitMS: 0, CloseOnExit: true,
	})
	if err != nil || r.Receipt.Status != "applied" {
		t.Fatalf("cannot start Windows PTY for interactive input: %+v %v", r, err)
	}
	line := "native-pty\r"
	write, err := m.Write(context.Background(), session.WriteArgs{
		ServerID: m.ID(), OperationID: "interactive-write",
		SessionID: r.Receipt.SessionID, Input: []session.Input{{Text: &line}},
		WaitMS: 1000,
	})
	if err != nil || write.Receipt.Status != "applied" {
		t.Fatalf("ConPTY interactive write failed: %+v %v", write, err)
	}
	obs, err := m.Read(context.Background(), session.ReadArgs{
		SessionID: r.Receipt.SessionID, WaitFor: "completion", WaitMS: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, event := range obs.Events {
		if event.Text != nil {
			all.WriteString(*event.Text)
		}
	}
	if !strings.Contains(all.String(), "CONPTY_GOT_native-pty") ||
		obs.Session.Exit == nil || obs.Session.Exit.Code == nil || *obs.Session.Exit.Code != 0 {
		t.Fatalf("interactive Windows ConPTY did not echo the input: output=%q, session=%+v", all.String(), obs.Session)
	}
}
