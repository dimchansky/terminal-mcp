package terminal

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/hinshun/vt10x"
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

type emulator struct {
	mu      sync.Mutex
	vt      vt10x.Terminal
	pending []byte
	reply   replies
	failed  string
}

func newEmulator(rows, cols int) *emulator {
	d := &emulator{}
	d.vt = vt10x.New(vt10x.WithSize(cols, rows), vt10x.WithWriter(&d.reply))
	return d
}

func (d *emulator) Feed(p []byte) (response []byte, err error) {
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

func (d *emulator) Snapshot(maxBytes int) session.Screen {
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

func (d *emulator) Key(name string) ([]byte, error) {
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
