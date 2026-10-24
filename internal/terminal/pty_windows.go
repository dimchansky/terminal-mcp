//go:build windows

package terminal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/x/conpty"
	"golang.org/x/sys/windows"
	"terminalmcp/internal/session"
)

// device wraps the native ConPTY handle and the same virtual screen emulator
// used on Unix. Windows doesn't expose POSIX foreground process groups.
type device struct {
	*emulator
	pty       *conpty.ConPty
	muWrite   sync.Mutex
	deadline  time.Time
	closeOnce sync.Once
	closed    atomic.Bool
}

func Start(cmd *exec.Cmd, rows, cols int) (session.Terminal, error) {
	pty, err := conpty.New(cols, rows, 0)
	if err != nil {
		return nil, err
	}
	pid, handle, err := pty.Spawn(cmd.Path, cmd.Args, &syscall.ProcAttr{
		Dir: cmd.Dir, Env: cmd.Env, Sys: cmd.SysProcAttr,
	})
	if err != nil {
		_ = pty.Close()
		return nil, err
	}
	proc, findErr := os.FindProcess(pid)
	if findErr != nil {
		_ = windows.TerminateProcess(windows.Handle(handle), 1)
		_ = windows.CloseHandle(windows.Handle(handle))
		_ = pty.Close()
		return nil, findErr
	}
	_ = windows.CloseHandle(windows.Handle(handle))
	cmd.Process = proc
	return &device{emulator: newEmulator(rows, cols), pty: pty}, nil
}

func (d *device) Read(b []byte) (int, error) {
	return d.pty.Read(b)
}

func (d *device) SetWriteDeadline(deadline time.Time) error {
	d.muWrite.Lock()
	d.deadline = deadline
	d.muWrite.Unlock()
	return nil
}

func (d *device) Write(b []byte) (int, error) {
	if d.closed.Load() {
		return 0, os.ErrClosed
	}
	d.muWrite.Lock()
	deadline := d.deadline
	d.muWrite.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	// ConPTY uses synchronous anonymous pipes without native write deadlines.
	// Time out a blocked write by permanently closing this PTY: a caller must
	// never retry the same bytes after the result becomes ambiguous.
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := d.pty.Write(b)
		done <- result{n: n, err: err}
	}()
	if deadline.IsZero() {
		r := <-done
		return r.n, r.err
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case r := <-done:
		return r.n, r.err
	case <-timer.C:
		_ = d.Close()
		return 0, fmt.Errorf("ConPTY input write timed out; output may be partial: %w", os.ErrDeadlineExceeded)
	}
}

func (d *device) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		err = d.pty.Close()
	})
	return err
}

func (d *device) Resize(rows, cols int) error {
	d.emulator.mu.Lock()
	defer d.emulator.mu.Unlock()
	if d.closed.Load() {
		return os.ErrClosed
	}
	if err := d.pty.Resize(cols, rows); err != nil {
		return err
	}
	d.vt.Resize(cols, rows)
	return nil
}

func (*device) ForegroundGroup() (int, error) {
	return 0, errors.New("Windows ConPTY does not expose a foreground process group")
}

var _ session.Terminal = (*device)(nil)
