//go:build linux || darwin

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"terminalmcp/internal/session"
)

type device struct {
	*os.File
	*emulator
}

func newDevice(rows, cols int) *device {
	return &device{emulator: newEmulator(rows, cols)}
}

// Start uses a nonblocking, poller-owned descriptor so Close and write deadlines
// can interrupt I/O. Calling File.Fd later would undo that property on some systems.
func Start(cmd *exec.Cmd, rows, cols int) (session.Terminal, error) {
	d := newDevice(rows, cols)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		_ = master.Close()
		if !ok {
			if d.File != nil {
				_ = d.File.Close()
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	raw, err := master.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var dupErr error
	if err = raw.Control(func(old uintptr) { fd, dupErr = unix.FcntlInt(old, unix.F_DUPFD_CLOEXEC, 0) }); err != nil {
		return nil, err
	}
	if dupErr != nil {
		return nil, dupErr
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	d.File = os.NewFile(uintptr(fd), "terminal-mcp-pty")
	if d.File == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("cannot wrap PTY descriptor")
	}
	if err = d.File.SetWriteDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("PTY write deadlines unavailable: %w", err)
	}
	ok = true
	return d, nil
}

func (d *device) Resize(rows, cols int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// pty v1.1.24 Setsize calls File.Fd, which disables nonblocking I/O.
	raw, err := d.File.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err = raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	}); err != nil {
		return err
	}
	if ioctlErr != nil {
		return ioctlErr
	}
	d.vt.Resize(cols, rows)
	return nil
}

func (d *device) ForegroundGroup() (int, error) {
	raw, err := d.File.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pgid int
	var ioctlErr error
	if err = raw.Control(func(fd uintptr) { pgid, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP) }); err != nil {
		return 0, err
	}
	return pgid, ioctlErr
}

var _ session.Terminal = (*device)(nil)
