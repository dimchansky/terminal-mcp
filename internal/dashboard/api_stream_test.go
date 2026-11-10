package dashboard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
			if ev.Items[0].Summary == nil || ev.Items[0].Summary.Text != "x" {
				t.Fatalf("item %+v", ev.Items[0])
			}
			// The start and finish are two appends: a slow runner may
			// report the open request first; its finish follows.
			if ev.Items[0].State == "finished" {
				break
			}
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

// stallingWriter is a response writer whose client never reads: every
// write blocks until the write deadline passes.
type stallingWriter struct {
	header   http.Header
	mu       sync.Mutex
	deadline time.Time
}

func (w *stallingWriter) Header() http.Header { return w.header }
func (w *stallingWriter) WriteHeader(int)     {}
func (w *stallingWriter) Flush()              {}

func (w *stallingWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = t
	return nil
}

func (w *stallingWriter) Write([]byte) (int, error) {
	for {
		w.mu.Lock()
		d := w.deadline
		w.mu.Unlock()
		if !d.IsZero() && time.Now().After(d) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStalledStreamIsCutOff: a stream whose client stopped reading is
// cut off by the per-write deadline, while another viewer's stream goes on.
func TestAPIStalledStreamIsCutOff(t *testing.T) {
	j := openJournal(t)
	api, mg := streamAPI(t, j)
	tokenA, tokenB := api.signIn(), api.signIn()
	var envA, envB Envelope
	api.ok(tokenA, PathWindow, map[string]any{}, &envA)
	api.ok(tokenB, PathWindow, map[string]any{}, &envB)
	b, code, data := api.openSSE(tokenB, map[string]any{"window": envB.Window, "generation": envB.Generation, "after": envB.LiveCursor})
	if code != http.StatusOK {
		t.Fatalf("stream B %d %s", code, data)
	}
	b.next(EventReady)

	body, _ := json.Marshal(map[string]any{"window": envA.Window, "generation": envA.Generation, "after": envA.LiveCursor})
	req := httptest.NewRequest(http.MethodPost, "http://"+api.host+PathStream, bytes.NewReader(body))
	req.Host = api.host
	for k, v := range map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
		"Origin": "http://" + api.host, "Content-Type": "application/json", "Authorization": "Viewer " + tokenA} {
		req.Header.Set(k, v)
	}
	ended := make(chan time.Time, 1)
	start := time.Now()
	go func() {
		api.handler.ServeHTTP(&stallingWriter{header: http.Header{}}, req)
		ended <- time.Now()
	}()
	j.requests(1)
	b.next(EventActivity) // B goes on while A is stuck.
	select {
	case at := <-ended:
		if at.Sub(start) < chunkTimeout/2 {
			t.Fatalf("A ended after %v, before its write deadline", at.Sub(start))
		}
	case <-time.After(chunkTimeout + 10*time.Second):
		t.Fatal("the stalled stream was not cut off")
	}
	if n := mg.streams(); n != 1 {
		t.Fatalf("%d streams held", n)
	}
	j.requests(1)
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
