//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Unlike POSIX signals, Windows supports only terminating the process or its
// owned Job Object. Do not pretend HUP/STOP/CONT or foreground signalling work.
var signals = map[string]struct{}{"TERM": {}, "KILL": {}}

func configurePipes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// Each session owns one Job Object, so closing a session also closes its
// descendants (unless a child was explicitly permitted to break away).
func trackProcess(s *Session) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = windows.CloseHandle(job)
		}
	}()
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		return err
	}
	// After Start, the command may exit before it can be associated. Treat
	// failure to associate as fatal, kill and reap the process (no fallback
	// to unmanaged execution). Avoid granting PROCESS_ALL_ACCESS.
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false,
		uint32(s.cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(proc) }()
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		return fmt.Errorf("assign child to Windows Job Object: %w", err)
	}
	s.job = &windowsJob{handle: job}
	keep = true
	return nil
}

type windowsJob struct{ handle windows.Handle }

func (j *windowsJob) Terminate() error {
	return windows.TerminateJobObject(j.handle, 1)
}
func (j *windowsJob) Close() {
	_ = windows.CloseHandle(j.handle)
}

func (s *Session) waitCommand() error {
	if s.terminal == nil {
		return s.cmd.Wait()
	}
	// ConPTY launches using CreateProcess and populates cmd.Process; cmd.Wait
	// cannot be used for that process (golang/go#62710).
	state, err := s.cmd.Process.Wait()
	s.cmd.ProcessState = state
	if err == nil && state != nil && !state.Success() {
		err = &exec.ExitError{ProcessState: state}
	}
	return err
}

func ignorablePTYError(err error) bool {
	return errors.Is(err, os.ErrClosed)
}

func exitFromState(s *Session, err error) *Exit {
	exit := &Exit{}
	if s.cmd.ProcessState != nil {
		code := s.cmd.ProcessState.ExitCode()
		exit.Code = &code
	} else if err != nil {
		exit.Error = err.Error()
	}
	return exit
}

func (s *Session) signal(name, target string) error {
	s.mu.Lock()
	exited := s.exit != nil
	job := s.job
	s.mu.Unlock()
	if exited {
		return fmt.Errorf("process already exited")
	}
	if name != "TERM" && name != "KILL" {
		return fmt.Errorf("signal %s is unsupported on Windows", name)
	}
	if target == "foreground" {
		return fmt.Errorf("Windows ConPTY does not expose the foreground process group; use group")
	}
	if target == "process" {
		return s.cmd.Process.Kill()
	}
	if job == nil {
		return errors.New("managed Windows process has no Job Object")
	}
	return job.Terminate()
}

func (s *Session) close(_ time.Duration) {
	s.closeOnce.Do(func() {
		defer close(s.closeDone)
		s.mu.Lock()
		s.closed, s.inputClosed = true, true
		s.notifyLocked()
		s.mu.Unlock()
		if s.terminal == nil {
			_ = s.stdin.Close()
		}
		s.mu.Lock()
		job := s.job
		s.mu.Unlock()
		if job != nil {
			_ = job.Terminate()
		}
		end := time.NewTimer(time.Second)
		select {
		case <-s.done:
		case <-end.C:
		}
		end.Stop()
		for _, reader := range s.readers {
			_ = reader.Close()
		}
		_ = s.stdin.Close()
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
		s.releaseJob()
	})
	<-s.closeDone
}
