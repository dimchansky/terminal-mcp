//go:build linux || darwin

package terminal

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
	"golang.org/x/sys/unix"
	"terminalmcp/internal/session"
)

type replies struct {
	data     []byte
	overflow bool
}

func (r *replies) Write(p []byte) (int, error) {
	n := len(p)
	room := 4096 - len(r.data)
	if len(p) > room {
		p = p[:room]
		r.overflow = true
	}
	r.data = append(r.data, p...)
	return n, nil
}

type device struct {
	*os.File
	mu      sync.Mutex
	vt      vt10x.Terminal
	pending []byte
	reply   replies
	failed  string
}

func newDevice(rows, cols int) *device {
	d := &device{}
	d.vt = vt10x.New(vt10x.WithSize(cols, rows), vt10x.WithWriter(&d.reply))
	return d
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

func (d *device) Feed(p []byte) (response []byte, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer func() {
		if recover() != nil {
			d.failed = "terminal emulator failed; inspect raw events instead"
			response = nil
			err = fmt.Errorf("%s", d.failed)
		}
	}()
	if d.failed != "" {
		return nil, fmt.Errorf("%s", d.failed)
	}
	d.reply.data = d.reply.data[:0]
	d.reply.overflow = false
	input := append(d.pending, p...)
	var complete []byte
	for len(input) > 0 && utf8.FullRune(input) {
		r, n := utf8.DecodeRune(input)
		complete = utf8.AppendRune(complete, r)
		input = input[n:]
	}
	d.pending = append([]byte(nil), input...)
	if len(complete) > 0 {
		n, writeErr := d.vt.Write(complete)
		if writeErr != nil {
			return nil, writeErr
		}
		if n != len(complete) {
			return nil, io.ErrShortWrite
		}
	}
	response = append([]byte(nil), d.reply.data...)
	if d.reply.overflow {
		return response, fmt.Errorf("terminal reply exceeds bounded buffer")
	}
	return response, nil
}

func (d *device) Snapshot(maxBytes int) session.Screen {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.vt.Lock()
	defer d.vt.Unlock()
	cols, rows := d.vt.Size()
	cur := d.vt.Cursor()
	s := session.Screen{Rows: rows, Cols: cols, CursorX: cur.X, CursorY: cur.Y, CursorVisible: d.vt.CursorVisible(), Alternate: d.vt.Mode()&vt10x.ModeAltScreen != 0, Error: d.failed}
	var text strings.Builder
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			r := d.vt.Cell(x, y).Char
			if r == 0 {
				r = ' '
			}
			if text.Len()+utf8.RuneLen(r) > maxBytes {
				s.Truncated = true
				s.Text = text.String()
				return s
			}
			text.WriteRune(r)
		}
		if y+1 < rows {
			if text.Len() == maxBytes {
				s.Truncated = true
				s.Text = text.String()
				return s
			}
			text.WriteByte('\n')
		}
	}
	s.Text = text.String()
	return s
}

func (d *device) Key(name string) ([]byte, error) {
	if len(name) == 6 && strings.HasPrefix(name, "CTRL_") && name[5] >= 'A' && name[5] <= 'Z' {
		return []byte{name[5] - 'A' + 1}, nil
	}
	keys := map[string]string{
		"ENTER": "\r", "TAB": "\t", "BACKSPACE": "\x7f", "ESCAPE": "\x1b", "SHIFT_TAB": "\x1b[Z",
		"PAGE_UP": "\x1b[5~", "PAGE_DOWN": "\x1b[6~", "INSERT": "\x1b[2~", "DELETE": "\x1b[3~",
		"F1": "\x1bOP", "F2": "\x1bOQ", "F3": "\x1bOR", "F4": "\x1bOS",
		"F5": "\x1b[15~", "F6": "\x1b[17~", "F7": "\x1b[18~", "F8": "\x1b[19~",
		"F9": "\x1b[20~", "F10": "\x1b[21~", "F11": "\x1b[23~", "F12": "\x1b[24~",
	}
	if key, ok := keys[name]; ok {
		return []byte(key), nil
	}
	cursorKeys := map[string]string{"UP": "A", "DOWN": "B", "RIGHT": "C", "LEFT": "D", "HOME": "H", "END": "F"}
	if suffix, ok := cursorKeys[name]; ok {
		d.mu.Lock()
		d.vt.Lock()
		application := d.vt.Mode()&vt10x.ModeAppCursor != 0
		d.vt.Unlock()
		d.mu.Unlock()
		prefix := "\x1b["
		if application {
			prefix = "\x1bO"
		}
		return []byte(prefix + suffix), nil
	}
	return nil, fmt.Errorf("unknown key %q", name)
}

var _ session.Terminal = (*device)(nil)
