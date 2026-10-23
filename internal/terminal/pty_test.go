//go:build linux || darwin

package terminal

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func feed(t *testing.T, d *device, p string) []byte {
	t.Helper()
	response, err := d.Feed([]byte(p))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestScreenOverwriteAndSplitUTF8(t *testing.T) {
	d := newDevice(3, 12)
	feed(t, d, "abc\rZ")
	if s := d.Snapshot(1024); !strings.HasPrefix(s.Text, "Zbc") {
		t.Fatalf("overwrite: %q", s.Text)
	}
	feed(t, d, "\x1b[2J\x1b[H")
	p := []byte("Ж€")
	for _, b := range p {
		feed(t, d, string([]byte{b}))
	}
	if s := d.Snapshot(1024); !strings.HasPrefix(s.Text, "Ж€") {
		t.Fatalf("split UTF8: %q", s.Text)
	}
	if s := d.Snapshot(1); !s.Truncated || len(s.Text) > 1 {
		t.Fatal("screen byte bound ignored")
	}
}

func TestKeysAlternateScreenAndProtocolReply(t *testing.T) {
	d := newDevice(4, 20)
	for key, want := range map[string]string{"CTRL_C": "\x03", "ENTER": "\r", "UP": "\x1b[A"} {
		got, err := d.Key(key)
		if err != nil || string(got) != want {
			t.Fatalf("key %s: %q %v", key, got, err)
		}
	}
	feed(t, d, "\x1b[?1h")
	if got, _ := d.Key("UP"); string(got) != "\x1bOA" {
		t.Fatalf("application cursor: %q", got)
	}
	feed(t, d, "\x1b[?1049h")
	if !d.Snapshot(1024).Alternate {
		t.Fatal("alternate screen not detected")
	}
	feed(t, d, "\x1b[?1049l")
	if d.Snapshot(1024).Alternate {
		t.Fatal("alternate screen not restored")
	}
	if response := feed(t, d, "\x1b[6n"); len(response) == 0 || !bytes.HasSuffix(response, []byte("R")) {
		t.Fatalf("no cursor response: %q", response)
	}
	if _, err := d.Key("NOT_A_KEY"); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestReplyBufferIsBounded(t *testing.T) {
	var r replies
	n, err := r.Write(bytes.Repeat([]byte{'x'}, 8192))
	if err != nil || n != 8192 || !r.overflow || len(r.data) != 4096 {
		t.Fatal("reply quota failed")
	}
}

func readUntil(t *testing.T, d *device, marker string) string {
	t.Helper()
	if err := d.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.SetReadDeadline(time.Time{}) }()
	var data []byte
	buf := make([]byte, 4096)
	for !bytes.Contains(data, []byte(marker)) {
		n, err := d.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			t.Fatalf("read before %q: %q, %v", marker, data, err)
		}
		if len(data) > 65536 {
			t.Fatal("unexpected flood")
		}
	}
	return string(data)
}

func assertNonblocking(t *testing.T, d *device) {
	t.Helper()
	raw, err := d.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var flagErr error
	if err = raw.Control(func(fd uintptr) { flags, flagErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if flagErr != nil || flags&unix.O_NONBLOCK == 0 {
		t.Fatalf("PTY descriptor became blocking: %v", flagErr)
	}
}

func TestRealPTYResizeAndCtrlC(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "stty -echo; trap 'printf INTERRUPTED; exit 0' INT; stty size; printf READY; while :; do sleep 1; done")
	term, err := Start(cmd, 26, 101)
	if err != nil {
		t.Fatal(err)
	}
	d := term.(*device)
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = d.Close()
	})
	out := readUntil(t, d, "READY")
	if !strings.Contains(out, "26 101") {
		t.Fatalf("initial size: %q", out)
	}
	assertNonblocking(t, d)
	if group, err := d.ForegroundGroup(); err != nil || group != cmd.Process.Pid {
		t.Fatalf("foreground group: %d %v", group, err)
	}
	if err := d.Resize(32, 109); err != nil {
		t.Fatal(err)
	}
	assertNonblocking(t, d)
	if s := d.Snapshot(65536); s.Rows != 32 || s.Cols != 109 {
		t.Fatal("emulator not resized")
	}
	raw, _ := d.SyscallConn()
	var size *unix.Winsize
	var sizeErr error
	if err := raw.Control(func(fd uintptr) { size, sizeErr = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ) }); err != nil {
		t.Fatal(err)
	}
	if sizeErr != nil || size.Row != 32 || size.Col != 109 {
		t.Fatalf("kernel size: %#v %v", size, sizeErr)
	}
	if err := d.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	readUntil(t, d, "INTERRUPTED")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		waited = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		waitErr := <-done // Reap the process before failing the test.
		waited = true
		t.Fatalf("Ctrl+C did not end foreground command (wait: %v)", waitErr)
	}
}

func TestPTYCloseInterruptsRead(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	term, err := Start(cmd, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := term.Read(make([]byte, 32)); done <- err }()
	time.Sleep(20 * time.Millisecond)
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	select {
	case err := <-done:
		if err == nil || err == io.ErrNoProgress {
			t.Fatalf("unexpected read result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing PTY did not interrupt read")
	}
}
