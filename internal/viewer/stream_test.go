package viewer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStopWakesAWriteBlockedOnANonReadingClient: a client that sends a
// request and never reads must not keep a stream handler alive after Stop.
func TestStopWakesAWriteBlockedOnANonReadingClient(t *testing.T) {
	streams := make(chan *Stream, 1)
	returned := make(chan error, 1)
	var written atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewStream(w)
		defer s.Release()
		if err := s.Extend(time.Time{}); err != nil { // No deadline: blocks.
			returned <- err
			return
		}
		streams <- s
		chunk := []byte(strings.Repeat("x", 64<<10))
		for {
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				returned <- err
				return
			}
		}
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", srv.Listener.Addr()); err != nil {
		t.Fatal(err)
	}
	s := <-streams
	// Wait until the handler's writes stall: the client never reads, so the
	// socket buffers fill and a write blocks.
	stalled := false
	for last, i := written.Load(), 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		now := written.Load()
		if now == last {
			stalled = true
			break
		}
		last = now
	}
	if !stalled {
		t.Skip("socket buffers never filled; cannot observe a blocked write here")
	}
	select {
	case err := <-returned:
		t.Fatalf("handler returned before Stop: %v", err)
	default:
	}
	s.Stop(ErrRevoked, time.Now())
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("blocked write did not fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not wake the blocked write")
	}
	if stopped, _, cause := s.Stopped(); !stopped || !errors.Is(cause, ErrRevoked) {
		t.Fatalf("stopped=%v cause=%v", stopped, cause)
	}
	if err := s.Extend(time.Now().Add(time.Hour)); !errors.Is(err, ErrStreamStopped) {
		t.Fatalf("extend after stop: %v", err)
	}
}

// TestGraceAllowsAFinalEvent: a stop with a future deadline lets the
// handler write a short final event to a reading client.
func TestGraceAllowsAFinalEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewStream(w)
		defer s.Release()
		s.Stop(&StopError{Reason: StopNormal}, time.Now().Add(time.Second))
		if _, deadline, _ := s.Stopped(); time.Until(deadline) <= 0 {
			t.Error("grace deadline already passed")
		}
		_, _ = io.WriteString(w, "event: server_stopping\n\n")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "event: server_stopping\n\n" {
		t.Fatalf("body %q", body)
	}
}

// TestStopAfterReleaseDoesNotAffectTheNextRequest: a late Stop must not set
// a deadline on a keep-alive connection that serves another request.
func TestStopAfterReleaseDoesNotAffectTheNextRequest(t *testing.T) {
	first := make(chan *Stream, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			s := NewStream(w)
			_, _ = io.WriteString(w, "one")
			s.Release()
			first <- s
			return
		}
		time.Sleep(200 * time.Millisecond) // Would exceed a past deadline.
		_, _ = io.WriteString(w, "two")
	}))
	defer srv.Close()
	client := srv.Client()
	get := func(path string) (string, bool) {
		var reused bool
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := bufio.NewReader(resp.Body).ReadString('!')
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("%s body: %v", path, err)
		}
		return body, reused
	}
	if body, _ := get("/first"); body != "one" {
		t.Fatalf("first %q", body)
	}
	(<-first).Stop(ErrRevoked, time.Now().Add(-time.Hour))
	body, reused := get("/second")
	if body != "two" || !reused {
		t.Fatalf("second %q reused=%v", body, reused)
	}
}
