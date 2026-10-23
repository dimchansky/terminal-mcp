//go:build linux || darwin

package session

import (
	"bytes"
	"encoding/base64"
	"math/rand"
	"strconv"
	"testing"
)

func eventBytes(events []Event) []byte {
	var out []byte
	for _, e := range events {
		if e.Text != nil {
			out = append(out, (*e.Text)...)
		} else {
			b, _ := base64.StdEncoding.DecodeString(e.BytesBase64)
			out = append(out, b...)
		}
	}
	return out
}

func TestJournalBoundsAndIndependentReaders(t *testing.T) {
	j := newJournal(6, 4)
	j.append("stdout", []byte("abcd"))
	j.append("stderr", []byte("EFGH"))
	for i := 0; i < 2; i++ {
		e, next, gap, _ := j.read(0, 10, 10)
		if string(eventBytes(e)) != "cdEFGH" || next != 8 || gap == nil || gap.From != "0" || gap.To != "2" {
			t.Fatalf("read: %#v, %d, %#v", e, next, gap)
		}
		if e[0].Stream != "stdout" || e[1].Stream != "stderr" {
			t.Fatal("stream labels lost")
		}
	}
	e, next, gap, _ := j.read(2, 3, 10)
	if string(eventBytes(e)) != "cdE" || next != 5 || gap != nil {
		t.Fatal("bounded read is incorrect")
	}
}

func TestJournalEventCap(t *testing.T) {
	j := newJournal(100, 3)
	for _, b := range []byte("12345") {
		j.append("stdout", []byte{b})
	}
	e, n, gap, _ := j.read(0, 100, 2)
	if string(eventBytes(e)) != "34" || n != 4 || gap.To != "2" || j.count != 3 {
		t.Fatal("event-count bound not enforced")
	}
}

func TestJournalBinaryAndSplitUTF8(t *testing.T) {
	j := newJournal(100, 10)
	j.append("stdout", []byte("Ж"))
	e, next, _, _ := j.read(0, 1, 10)
	if e[0].Text != nil || e[0].BytesBase64 == "" || next != 1 {
		t.Fatal("split UTF-8 must be byte-exact")
	}
	e2, _, _, _ := j.read(next, 5, 10)
	if !bytes.Equal(append(eventBytes(e), eventBytes(e2)...), []byte("Ж")) {
		t.Fatal("UTF-8 bytes changed")
	}
	j.append("stderr", []byte{0xff, 0, 0xfe})
	e, _, _, _ = j.read(2, 5, 10)
	if !bytes.Equal(eventBytes(e), []byte{0xff, 0, 0xfe}) {
		t.Fatal("binary bytes changed")
	}
}

func TestJournalRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for run := 0; run < 40; run++ {
		j := newJournal(1+rng.Intn(100), 1+rng.Intn(20))
		var all []byte
		for i := 0; i < 100; i++ {
			b := make([]byte, 1+rng.Intn(140))
			_, _ = rng.Read(b)
			j.append("stdout", b)
			all = append(all, b...)
			from := uint64(rng.Intn(len(all) + 1))
			e, next, gap, raw := j.read(from, 1+rng.Intn(60), 1+rng.Intn(20))
			start := from
			if gap != nil {
				start = j.first()
			}
			if !bytes.Equal(raw, all[start:next]) || !bytes.Equal(eventBytes(e), raw) {
				t.Fatalf("journal mismatch, run %d", run)
			}
			if j.size > j.maxBytes || j.count > len(j.chunks) {
				t.Fatal("unbounded journal")
			}
		}
	}
}

func TestCursorValidation(t *testing.T) {
	for _, n := range []uint64{0, 1, 999, ^uint64(0)} {
		r, err := parseCursor("session", makeCursor("session", n))
		if err != nil || r != n {
			t.Fatal("cursor round trip")
		}
	}
	for _, bad := range []string{"other:0", "session:", "session:-1", "session:" + strconv.FormatUint(^uint64(0), 36) + "z"} {
		if _, err := parseCursor("session", bad); err == nil {
			t.Fatalf("accepted cursor %q", bad)
		}
	}
}
