//go:build linux || darwin

package session

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type inputWriter interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

type Session struct {
	mu                                                sync.Mutex
	id, mode, cwd                                     string
	argv                                              []string
	startedAt, exitedAt                               time.Time
	cmd                                               *exec.Cmd
	limits                                            Limits
	journal                                           journal
	changed                                           chan struct{}
	exit                                              *Exit
	closed, inputClosed, outputDone, outputIncomplete bool
	diagnostics                                       []string
	revision                                          uint64
	stdin                                             inputWriter
	terminal                                          Terminal
	readers                                           []io.ReadCloser
	writeGate                                         chan struct{}
	done, drained                                     chan struct{}
	closeOnExit                                       bool
	closeOnce                                         sync.Once
	closeDone                                         chan struct{}
}

func spawn(a StartArgs, limits Limits, startPTY PTYStarter) (*Session, error) {
	env := childEnv(a.Env, a.Mode == "pty")
	path, err := executable(a.Argv[0], a.Cwd, env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, a.Argv[1:]...)
	cmd.Dir, cmd.Env = a.Cwd, env
	s := &Session{closeOnExit: a.CloseOnExit, id: newID(), mode: a.Mode, cwd: a.Cwd, argv: append([]string(nil), a.Argv...),
		startedAt: time.Now(), cmd: cmd, limits: limits, journal: newJournal(limits.JournalBytes, limits.JournalEvents),
		changed: make(chan struct{}), writeGate: make(chan struct{}, 1), done: make(chan struct{}), drained: make(chan struct{}), closeDone: make(chan struct{})}
	var streams []string
	if a.Mode == "pty" {
		s.terminal, err = startPTY(cmd, a.Rows, a.Cols)
		if err != nil {
			return nil, err
		}
		s.stdin = s.terminal
		s.readers = []io.ReadCloser{s.terminal}
		streams = []string{"terminal"}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var opened []*os.File
		pipe := func() (*os.File, *os.File, error) {
			r, w, err := os.Pipe()
			if err == nil {
				opened = append(opened, r, w)
			}
			return r, w, err
		}
		inR, inW, err := pipe()
		if err == nil {
			var outR, outW, errR, errW *os.File
			outR, outW, err = pipe()
			if err == nil {
				errR, errW, err = pipe()
			}
			if err == nil {
				cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
				err = cmd.Start()
				if err == nil {
					_ = inR.Close()
					_ = outW.Close()
					_ = errW.Close()
					s.stdin = inW
					s.readers = []io.ReadCloser{outR, errR}
					streams = []string{"stdout", "stderr"}
				}
			}
		}
		if err != nil {
			for _, f := range opened {
				_ = f.Close()
			}
			return nil, err
		}
	}
	replies := make(chan []byte, 16)
	if s.terminal != nil {
		go func() {
			for p := range replies {
				_, err := s.writeBytes(p, false)
				if err != nil {
					s.diagnostic("terminal_reply_failed")
				}
			}
		}()
	}
	var wg sync.WaitGroup
	for i, reader := range s.readers {
		wg.Add(1)
		go func(r io.ReadCloser, stream string) {
			defer wg.Done()
			defer func() { _ = r.Close() }() // I/O reader can also be closed by session shutdown.
			buf := make([]byte, 4096)
			for {
				n, readErr := r.Read(buf)
				if n > 0 {
					var response []byte
					var feedErr error
					s.mu.Lock()
					if s.terminal != nil {
						response, feedErr = s.terminal.Feed(buf[:n])
						s.revision++
					}
					s.journal.append(stream, buf[:n])
					s.notifyLocked()
					s.mu.Unlock()
					if feedErr != nil {
						s.diagnostic("screen_emulation_error")
					}
					if len(response) > 0 {
						select {
						case replies <- response:
						default:
							s.diagnostic("terminal_reply_queue_full")
						}
					}
				}
				if readErr != nil {
					if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, os.ErrClosed) && !(s.terminal != nil && errors.Is(readErr, syscall.EIO)) {
						s.mu.Lock()
						s.outputIncomplete = true
						s.mu.Unlock()
						s.diagnostic("output_read_error: " + readErr.Error())
					}
					return
				}
			}
		}(reader, streams[i])
	}
	go func() {
		wg.Wait()
		close(replies)
		s.mu.Lock()
		s.outputDone = true
		s.notifyLocked()
		s.mu.Unlock()
		close(s.drained)
	}()
	go s.wait()
	if a.MaxRuntimeMS > 0 {
		go func() {
			timer := time.NewTimer(time.Duration(a.MaxRuntimeMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-s.done:
			case <-timer.C:
				s.diagnostic("maximum_runtime_exceeded")
				s.close(200 * time.Millisecond)
			}
		}()
	}
	return s, nil
}

func (s *Session) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }

func (s *Session) diagnostic(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.diagnostics {
		if old == message {
			return
		}
	}
	if len(s.diagnostics) < 8 {
		s.diagnostics = append(s.diagnostics, message)
	}
	s.notifyLocked()
}

func (s *Session) infoLocked() Info {
	state := "running"
	if s.exit != nil {
		state = "exited"
	}
	return Info{SessionID: s.id, Mode: s.mode, PID: s.cmd.Process.Pid, Argv: append([]string(nil), s.argv...), Cwd: s.cwd,
		StartedAt: s.startedAt.UTC().Format(time.RFC3339Nano), ProcessState: state, Exit: s.exit, Closed: s.closed,
		CloseOnExit: s.closeOnExit, OutputDone: s.outputDone, OutputIncomplete: s.outputIncomplete, EndCursor: makeCursor(s.id, s.journal.end), Diagnostics: append([]string(nil), s.diagnostics...)}
}

func (s *Session) wait() {
	err := s.cmd.Wait()
	exit := &Exit{}
	if s.cmd.ProcessState != nil {
		if ws, ok := s.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			exit.Signal = ws.Signal().String()
		} else {
			code := s.cmd.ProcessState.ExitCode()
			exit.Code = &code
		}
	} else if err != nil {
		exit.Error = err.Error()
	}
	s.mu.Lock()
	s.exit, s.exitedAt, s.inputClosed = exit, time.Now(), true
	s.notifyLocked()
	s.mu.Unlock()
	if s.terminal == nil {
		_ = s.stdin.Close()
	}
	close(s.done)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-s.drained:
	case <-timer.C:
		s.mu.Lock()
		s.outputIncomplete = true
		s.notifyLocked()
		s.mu.Unlock()
		s.diagnostic("output_drain_deadline: an inherited descriptor may still be open")
		for _, r := range s.readers {
			_ = r.Close()
		}
	}
	if s.closeOnExit {
		// Readers close independently; never truncate output merely because the leader exited.
		<-s.drained
		s.closeOnce.Do(func() {
			_ = s.stdin.Close()
			s.mu.Lock()
			s.closed = true
			s.notifyLocked()
			s.mu.Unlock()
			close(s.closeDone)
		})
	}
}

func (s *Session) validateCursor(cursor string) error {
	pos, err := parseCursor(s.id, cursor)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if pos > s.journal.end {
		return fmt.Errorf("cursor is beyond the output journal")
	}
	return nil
}

func (s *Session) read(ctx context.Context, a ReadArgs) (Observation, error) {
	from, err := parseCursor(s.id, a.Cursor)
	if err != nil {
		return Observation{}, err
	}
	if a.View == "screen" && s.terminal == nil {
		return Observation{}, fmt.Errorf("screen view requires PTY")
	}
	end := time.Now().Add(time.Duration(a.WaitMS) * time.Millisecond)
	deadline := time.NewTimer(time.Until(end))
	defer deadline.Stop()
	expired := a.WaitMS == 0
	for {
		if err := ctx.Err(); err != nil {
			return Observation{}, err
		}
		s.mu.Lock()
		if from > s.journal.end {
			s.mu.Unlock()
			return Observation{}, fmt.Errorf("cursor is beyond the output journal")
		}
		out := Observation{Session: s.infoLocked(), Events: make([]Event, 0)}
		var visible []byte
		if a.View == "screen" {
			screen := s.terminal.Snapshot(a.MaxBytes)
			screen.Revision = s.revision
			out.Screen = &screen
			visible = []byte(screen.Text)
			out.NextCursor = makeCursor(s.id, from)
		} else {
			var next uint64
			out.Events, next, out.Gap, visible = s.journal.read(from, a.MaxBytes, s.limits.ResponseEvents)
			out.NextCursor = makeCursor(s.id, next)
			out.HasMore = next < s.journal.end
		}
		changed, hasNew := s.changed, s.journal.end > from
		s.mu.Unlock()
		switch {
		case a.Until != "" && strings.Contains(string(visible), a.Until):
			out.ReturnReason = "matched"
		case out.Gap != nil:
			out.ReturnReason = "gap"
		case out.HasMore || (out.Screen != nil && out.Screen.Truncated):
			out.ReturnReason = "limit"
		case a.WaitFor == "completion" && out.Session.Exit != nil && out.Session.OutputDone && (!s.closeOnExit || out.Session.Closed):
			out.ReturnReason = "completed"
		case a.WaitFor != "completion" && out.Session.OutputDone:
			out.ReturnReason = "eof"
		case a.WaitFor != "completion" && a.Until == "" && hasNew:
			out.ReturnReason = "output"
		case a.WaitFor != "completion" && a.Until == "" && out.Session.Exit != nil:
			out.ReturnReason = "exited"
		case expired || !time.Now().Before(end):
			out.ReturnReason = "deadline"
		}
		if out.ReturnReason != "" {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return Observation{}, ctx.Err()
		case <-deadline.C:
			expired = true
		case <-changed:
		}
	}
}

func (s *Session) receipt() Receipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Receipt{SessionID: s.id, Status: "applied", BeforeCursor: makeCursor(s.id, s.journal.end)}
}

func (s *Session) write(input []Input, eof bool) Receipt {
	r := s.receipt()
	var data []byte
	for _, item := range input {
		switch {
		case item.Text != nil:
			data = append(data, (*item.Text)...)
		case item.BytesBase64 != nil:
			b, _ := base64.StdEncoding.Strict().DecodeString(*item.BytesBase64)
			data = append(data, b...)
		case item.Key != "":
			b, err := s.terminal.Key(item.Key)
			if err != nil {
				r.Status, r.Error = "failed", err.Error()
				return r
			}
			data = append(data, b...)
		}
	}
	n, err := s.writeBytes(data, eof)
	r.BytesWritten = n
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		if n > 0 {
			r.Status = "partial"
		}
	}
	return r
}

func (s *Session) writeBytes(p []byte, eof bool) (int, error) {
	deadline := time.Now().Add(time.Duration(s.limits.WriteTimeoutMS) * time.Millisecond)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case s.writeGate <- struct{}{}:
		defer func() { <-s.writeGate }()
	case <-timer.C:
		return 0, fmt.Errorf("input_busy_timeout")
	}
	s.mu.Lock()
	unavailable := s.closed || s.inputClosed || s.exit != nil
	s.mu.Unlock()
	if unavailable {
		return 0, fmt.Errorf("stdin is closed or the process has exited")
	}
	if err := s.stdin.SetWriteDeadline(deadline); err != nil {
		return 0, fmt.Errorf("cannot bound input write: %w", err)
	}
	n := 0
	for n < len(p) {
		written, err := s.stdin.Write(p[n:])
		n += written
		if err != nil {
			return n, err
		}
		if written == 0 {
			return n, io.ErrShortWrite
		}
	}
	if eof {
		s.mu.Lock()
		s.inputClosed = true
		s.mu.Unlock()
		if err := s.stdin.Close(); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *Session) signal(sig syscall.Signal, target string) error {
	s.mu.Lock()
	exited := s.exit != nil
	s.mu.Unlock()
	if exited {
		return fmt.Errorf("process already exited")
	}
	if target == "process" {
		return s.cmd.Process.Signal(sig)
	}
	// Never use a caller-supplied PID or a stale group after observed leader exit.
	if err := s.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		return err
	}
	group := s.cmd.Process.Pid
	if target == "foreground" {
		var err error
		group, err = s.terminal.ForegroundGroup()
		if err != nil {
			return err
		}
	}
	if group <= 1 || group == syscall.Getpgrp() {
		return fmt.Errorf("refusing invalid or server process group")
	}
	return syscall.Kill(-group, sig)
}

func (s *Session) close(grace time.Duration) {
	s.closeOnce.Do(func() {
		defer close(s.closeDone)
		s.mu.Lock()
		s.closed, s.inputClosed = true, true
		s.notifyLocked()
		s.mu.Unlock()
		if s.terminal == nil {
			_ = s.stdin.Close()
		}
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGCONT} {
			if s.terminal != nil {
				_ = s.signal(sig, "foreground")
			}
			_ = s.signal(sig, "group")
		}
		timer := time.NewTimer(grace)
		select {
		case <-s.done:
		case <-timer.C:
			if s.terminal != nil {
				_ = s.signal(syscall.SIGKILL, "foreground")
			}
			_ = s.signal(syscall.SIGKILL, "group")
		}
		timer.Stop()
		end := time.NewTimer(time.Second)
		select {
		case <-s.done:
		case <-end.C:
		}
		end.Stop()
		drain := time.NewTimer(200 * time.Millisecond)
		select {
		case <-s.drained:
		case <-drain.C:
			s.mu.Lock()
			s.outputIncomplete = true
			s.notifyLocked()
			s.mu.Unlock()
		}
		drain.Stop()
		for _, r := range s.readers {
			_ = r.Close()
		}
		_ = s.stdin.Close()
	})
	<-s.closeDone
}
