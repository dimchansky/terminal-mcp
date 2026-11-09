package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/viewer"
)

type sseEvent struct {
	name string
	data json.RawMessage
}

// sseClient reads one stream response.
type sseClient struct {
	t      *testing.T
	resp   *http.Response
	events chan sseEvent
	closed chan struct{}
}

// openSSE starts a stream request; a non-200 answer is returned instead.
func (a *apiServer) openSSE(token string, body any) (*sseClient, int, []byte) {
	a.t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, a.url+PathStream, bytes.NewReader(data))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Host = a.host
	for k, v := range map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
		"Origin": "http://" + a.host, "Content-Type": "application/json", "Authorization": "Viewer " + token} {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		var out bytes.Buffer
		_, _ = out.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		return nil, resp.StatusCode, out.Bytes()
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-store" {
		a.t.Fatalf("stream headers %v", resp.Header)
	}
	c := &sseClient{t: a.t, resp: resp, events: make(chan sseEvent, 1000), closed: make(chan struct{})}
	a.t.Cleanup(func() { _ = resp.Body.Close() })
	go func() {
		defer close(c.closed)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			case line == "":
				c.events <- ev
				ev = sseEvent{}
			}
		}
	}()
	return c, http.StatusOK, nil
}

func (c *sseClient) next(name string) sseEvent {
	c.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-c.events:
			if ev.name == name {
				return ev
			}
			if ev.name != EventHeartbeat && ev.name != EventActivity && ev.name != EventReady {
				c.t.Fatalf("got %s %s while waiting for %s", ev.name, ev.data, name)
			}
		case <-c.closed:
			// The reader may have queued events just before closing.
			for {
				select {
				case ev := <-c.events:
					if ev.name == name {
						return ev
					}
				default:
					c.t.Fatalf("stream ended while waiting for %s", name)
				}
			}
		case <-timeout:
			c.t.Fatalf("no %s event", name)
		}
	}
}

// ended waits until the server closes the stream.
func (c *sseClient) ended(within time.Duration) {
	c.t.Helper()
	select {
	case <-c.closed:
	case <-time.After(within):
		c.t.Fatal("the stream was not closed")
	}
}

func streamAPI(t *testing.T, j *journal) (*apiServer, *Manager) {
	t.Helper()
	mg := newManager(j)
	mg.heartbeat, mg.coalesce = 500*time.Millisecond, 10*time.Millisecond
	return newAPI(t, mg), mg
}

func TestAPIStreamNotifiesAndEnds(t *testing.T) {
	j := openJournal(t)
	j.requests(1)
	api, _ := streamAPI(t, j)
	token := api.signIn()
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	c, code, data := api.openSSE(token, map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor})
	if code != http.StatusOK {
		t.Fatalf("stream %d %s", code, data)
	}
	c.next(EventReady)
	id := j.request(`{"path":"x"}`)
	for {
		var ev ActivityEvent
		if err := json.Unmarshal(c.next(EventActivity).data, &ev); err != nil {
			t.Fatal(err)
		}
		if len(ev.Items) > 0 && ev.Items[0].Key == actionKey(requestKey(id)) {
			if ev.Items[0].Summary == nil || ev.Items[0].Summary.Text != "x" || ev.Items[0].State != "finished" {
				t.Fatalf("item %+v", ev.Items[0])
			}
			break
		}
	}
	// Catch-up delivers the complete item.
	var cu CatchUpPage
	api.ok(token, PathCatchUp, map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor}, &cu)
	if requestIDs(cu.Page) != id || len(cu.Records) != 2 {
		t.Fatalf("catch-up %q %d records", requestIDs(cu.Page), len(cu.Records))
	}
	c.next(EventHeartbeat)
	// Closing the window ends the stream.
	if code, _, _ := api.post(token, PathClose, map[string]any{"window": env.Window}); code != http.StatusNoContent {
		t.Fatal(code)
	}
	c.next(EventWindowGone)
	c.ended(5 * time.Second)
}

func TestAPIStreamStartErrors(t *testing.T) {
	j := openJournal(t)
	j.requests(1)
	api, _ := streamAPI(t, j)
	token := api.signIn()
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	for name, c := range map[string]struct {
		body map[string]any
		code int
		err  string
	}{
		"no generation":    {map[string]any{"window": env.Window, "after": env.LiveCursor}, http.StatusBadRequest, "bad_request"},
		"other generation": {map[string]any{"window": env.Window, "generation": env.Generation + 5, "after": env.LiveCursor}, http.StatusConflict, "generation_changed"},
		"unknown window":   {map[string]any{"window": "nope", "generation": env.Generation, "after": env.LiveCursor}, http.StatusNotFound, "window_gone"},
	} {
		if _, code, data := api.openSSE(token, c.body); code != c.code || errCode(t, data) != c.err {
			t.Errorf("%s: %d %s", name, code, data)
		}
	}
	// One stream per session: a stream of the session's other window is busy.
	c, _, _ := api.openSSE(token, map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor})
	c.next(EventReady)
	var other Envelope
	api.ok(token, PathWindow, map[string]any{}, &other)
	if _, code, data := api.openSSE(token, map[string]any{"window": other.Window, "generation": other.Generation, "after": other.LiveCursor}); code != http.StatusTooManyRequests || errCode(t, data) != "capacity" {
		t.Fatalf("second stream: %d %s", code, data)
	}
}

// TestAPIStreamReconnectReplacesTheOldStream: the same tab reconnecting
// while its old connection lingers gets a stream at once.
func TestAPIStreamReconnectReplacesTheOldStream(t *testing.T) {
	j := openJournal(t)
	api, _ := streamAPI(t, j)
	token := api.signIn()
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	body := map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor}
	first, _, _ := api.openSSE(token, body)
	first.next(EventReady)
	second, code, data := api.openSSE(token, body)
	if code != http.StatusOK {
		t.Fatalf("reconnect %d %s", code, data)
	}
	second.next(EventReady)
	first.ended(5 * time.Second)
	j.requests(1)
	second.next(EventActivity)
}

func TestAPIStreamEndsOnLogoutAndStop(t *testing.T) {
	j := openJournal(t)
	api, _ := streamAPI(t, j)
	stream := func() (*sseClient, string) {
		token := api.signIn()
		var env Envelope
		api.ok(token, PathWindow, map[string]any{}, &env)
		c, code, data := api.openSSE(token, map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor})
		if code != http.StatusOK {
			t.Fatalf("stream %d %s", code, data)
		}
		c.next(EventReady)
		return c, token
	}
	out, token := stream()
	stopped, _ := stream()
	if err := api.registry.Logout(token); err != nil {
		t.Fatal(err)
	}
	out.ended(5 * time.Second)
	out.noFinalEvent()
	api.registry.CloseAdmission(viewer.StopUpdate)
	api.registry.StopAll(time.Second)
	ev := stopped.next(EventServerStopped)
	if !strings.Contains(string(ev.data), `"update"`) {
		t.Fatalf("server_stopping %s", ev.data)
	}
	stopped.ended(5 * time.Second)
}

// TestAPIStalledStreamIsCutOff: a stream client that stops reading is cut
// off by the write deadline, while another viewer's stream goes on.
func TestAPIStalledStreamIsCutOff(t *testing.T) {
	j := openJournal(t)
	api, mg := streamAPI(t, j)
	mg.coalesce = time.Millisecond
	tokenA, tokenB := api.signIn(), api.signIn()
	var envA, envB Envelope
	api.ok(tokenA, PathWindow, map[string]any{}, &envA)
	api.ok(tokenB, PathWindow, map[string]any{}, &envB)

	conn, err := net.Dial("tcp", api.host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	body, _ := json.Marshal(map[string]any{"window": envA.Window, "generation": envA.Generation, "after": envA.LiveCursor})
	_, _ = conn.Write([]byte("POST " + PathStream + " HTTP/1.1\r\nHost: " + api.host + "\r\nSec-Fetch-Site: same-origin\r\n" +
		"Sec-Fetch-Mode: cors\r\nSec-Fetch-Dest: empty\r\nOrigin: http://" + api.host + "\r\nAuthorization: Viewer " + tokenA +
		"\r\nContent-Type: application/json\r\nContent-Length: " + itoa(len(body)) + "\r\n\r\n" + string(body)))
	b, code, data := api.openSSE(tokenB, map[string]any{"window": envB.Window, "generation": envB.Generation, "after": envB.LiveCursor})
	if code != http.StatusOK {
		t.Fatalf("stream B %d %s", code, data)
	}
	b.next(EventReady)
	// Large activity events (a new long lane each) fill the stalled client.
	long := strings.Repeat("c", 3<<10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for i := 0; ctx.Err() == nil && i < 5000; i++ {
			chatStart(j, "s"+itoa(i), long+itoa(i))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	b.next(EventActivity) // B keeps receiving meanwhile.
	attached := time.Now().Add(10 * time.Second)
	for mg.streams() != 2 { // A's stream is attached and then stalls.
		if time.Now().After(attached) {
			t.Fatal("the stalled client's stream never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline := time.Now().Add(3*chunkTimeout + 5*time.Second)
	for mg.streams() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the stalled stream is still held (%d streams)", mg.streams())
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.next(EventActivity)
}

// streams counts the windows with a stream attached.
func (mg *Manager) streams() int {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	n := 0
	for _, m := range mg.windows {
		m.mu.Lock()
		if m.stream != nil {
			n++
		}
		m.mu.Unlock()
	}
	return n
}

// noFinalEvent asserts that an ended stream carried no final event.
func (c *sseClient) noFinalEvent() {
	c.t.Helper()
	for {
		select {
		case ev := <-c.events:
			if ev.name != EventHeartbeat && ev.name != EventActivity && ev.name != EventReady {
				c.t.Fatalf("unexpected %s", ev.name)
			}
		default:
			return
		}
	}
}

func TestAPIStreamForceStopWritesNoEvent(t *testing.T) {
	j := openJournal(t)
	api, _ := streamAPI(t, j)
	token := api.signIn()
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	c, _, _ := api.openSSE(token, map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor})
	c.next(EventReady)
	api.registry.CloseAdmission(viewer.StopForce)
	api.registry.StopAll(0)
	c.ended(5 * time.Second)
	c.noFinalEvent()
}

func TestAPIStreamStartStates(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		mg := NewManager(nil, ManagerConfig{ServerID: testServer})
		api := newAPI(t, mg)
		token := api.signIn()
		var env Envelope
		api.ok(token, PathWindow, map[string]any{}, &env)
		if _, code, data := api.openSSE(token, map[string]any{"window": env.Window, "generation": env.Generation, "after": ""}); code != http.StatusConflict || errCode(t, data) != "disabled" {
			t.Fatalf("disabled: %d %s", code, data)
		}
	})
	t.Run("unknown cursor rebuilds", func(t *testing.T) {
		j := openJournal(t)
		j.requests(1)
		api, _ := streamAPI(t, j)
		token := api.signIn()
		var env Envelope
		api.ok(token, PathWindow, map[string]any{}, &env)
		if _, code, data := api.openSSE(token, map[string]any{"window": env.Window, "generation": env.Generation, "after": "nope"}); code != http.StatusConflict || errCode(t, data) != "generation_changed" {
			t.Fatalf("unknown cursor: %d %s", code, data)
		}
	})
	t.Run("rebuilding", func(t *testing.T) {
		j := openJournal(t)
		j.requests(1)
		api, mg := streamAPI(t, j)
		token := api.signIn()
		var env Envelope
		api.ok(token, PathWindow, map[string]any{}, &env)
		m, _ := mg.windowByID(env.Window)
		m.win.With(func(*activity.Projection) { m.win.buildErr = func() error { return errors.New("disk") } })
		// The first failing rebuild answers generation_changed to its caller
		// via rebuilding; a fresh snapshot then holds a not yet built window.
		_, _, _ = api.post(token, PathCatchUp, map[string]any{"window": env.Window, "generation": env.Generation, "after": "nope"})
		var again Envelope
		api.ok(token, PathWindow, map[string]any{"window": env.Window}, &again)
		_, code, data := api.openSSE(token, map[string]any{"window": env.Window, "generation": again.Generation, "after": again.LiveCursor})
		if code != http.StatusServiceUnavailable || errCode(t, data) != "rebuilding" {
			t.Fatalf("rebuilding: %d %s", code, data)
		}
	})
	t.Run("total stream limit", func(t *testing.T) {
		j := openJournal(t)
		api, _ := streamAPI(t, j)
		for i := 0; i < 5; i++ {
			token := api.signIn()
			var env Envelope
			api.ok(token, PathWindow, map[string]any{}, &env)
			c, code, data := api.openSSE(token, map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor})
			if i < 4 {
				if code != http.StatusOK {
					t.Fatalf("stream %d: %d %s", i, code, data)
				}
				c.next(EventReady)
				continue
			}
			if code != http.StatusTooManyRequests || errCode(t, data) != "capacity" {
				t.Fatalf("fifth stream: %d %s", code, data)
			}
		}
	})
}

func (mg *Manager) windowByID(id string) (*managed, bool) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	m, ok := mg.windows[id]
	return m, ok
}
