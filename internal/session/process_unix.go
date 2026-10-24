//go:build linux || darwin

package session

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

var signals = map[string]struct{}{"INT": {}, "TERM": {}, "KILL": {}, "HUP": {}, "CONT": {}, "STOP": {}}

func configurePipes(cmd *exec.Cmd)     { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func trackProcess(_ *Session) error    { return nil }
func (s *Session) waitCommand() error  { return s.cmd.Wait() }
func ignorablePTYError(err error) bool { return errors.Is(err, syscall.EIO) }

func exitFromState(s *Session, err error) *Exit {
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
	return exit
}

func (s *Session) signal(name string, target string) error {
	sig := map[string]syscall.Signal{"INT": syscall.SIGINT, "TERM": syscall.SIGTERM,
		"KILL": syscall.SIGKILL, "HUP": syscall.SIGHUP, "CONT": syscall.SIGCONT, "STOP": syscall.SIGSTOP}[name]
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
		for _, signal := range []string{"TERM", "CONT"} {
			if s.terminal != nil {
				_ = s.signal(signal, "foreground")
			}
			_ = s.signal(signal, "group")
		}
		timer := time.NewTimer(grace)
		select {
		case <-s.done:
		case <-timer.C:
			if s.terminal != nil {
				_ = s.signal("KILL", "foreground")
			}
			_ = s.signal("KILL", "group")
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
