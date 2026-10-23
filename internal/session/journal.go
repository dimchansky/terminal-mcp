//go:build linux || darwin

package session

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type chunk struct {
	stream string
	start  uint64
	data   []byte
}

type journal struct {
	chunks                      []chunk
	head, count, size, maxBytes int
	end                         uint64
}

func newJournal(bytes, events int) journal {
	return journal{chunks: make([]chunk, events), maxBytes: bytes}
}

func (j *journal) drop() {
	c := &j.chunks[j.head]
	j.size -= len(c.data)
	*c = chunk{}
	j.head = (j.head + 1) % len(j.chunks)
	j.count--
}

func (j *journal) append(stream string, p []byte) {
	if len(p) == 0 {
		return
	}
	start := j.end
	j.end += uint64(len(p))
	if len(p) > j.maxBytes {
		start += uint64(len(p) - j.maxBytes)
		p = p[len(p)-j.maxBytes:]
	}
	if j.count == len(j.chunks) {
		j.drop()
	}
	idx := (j.head + j.count) % len(j.chunks)
	j.chunks[idx] = chunk{stream, start, append([]byte(nil), p...)}
	j.count++
	j.size += len(p)
	for j.size > j.maxBytes {
		extra := j.size - j.maxBytes
		c := &j.chunks[j.head]
		if extra >= len(c.data) {
			j.drop()
			continue
		}
		c.start += uint64(extra)
		c.data = c.data[extra:]
		j.size -= extra
	}
}

func (j *journal) first() uint64 {
	if j.count == 0 {
		return j.end
	}
	return j.chunks[j.head].start
}

func (j *journal) read(from uint64, maxBytes, maxEvents int) (events []Event, next uint64, gap *Gap, raw []byte) {
	events = make([]Event, 0)
	if from < j.first() {
		gap = &Gap{strconv.FormatUint(from, 10), strconv.FormatUint(j.first(), 10)}
		from = j.first()
	}
	next = from
	for i := 0; i < j.count && len(raw) < maxBytes && len(events) < maxEvents; i++ {
		c := j.chunks[(j.head+i)%len(j.chunks)]
		if c.start+uint64(len(c.data)) <= next {
			continue
		}
		p := c.data[next-c.start:]
		if len(p) > maxBytes-len(raw) {
			p = p[:maxBytes-len(raw)]
		}
		e := Event{Stream: c.stream, Offset: strconv.FormatUint(next, 10), Bytes: len(p)}
		if utf8.Valid(p) {
			text := string(p)
			e.Text = &text
		} else {
			e.BytesBase64 = base64.StdEncoding.EncodeToString(p)
		}
		events = append(events, e)
		raw = append(raw, p...)
		next += uint64(len(p))
	}
	return
}

func makeCursor(id string, pos uint64) string { return id + ":" + strconv.FormatUint(pos, 36) }

func parseCursor(id, cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	prefix := id + ":"
	if !strings.HasPrefix(cursor, prefix) {
		return 0, fmt.Errorf("cursor does not belong to this session")
	}
	p, err := strconv.ParseUint(strings.TrimPrefix(cursor, prefix), 36, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor")
	}
	return p, nil
}
