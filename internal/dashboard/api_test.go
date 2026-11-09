package dashboard

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/viewer"
)

type apiServer struct {
	t        *testing.T
	url      string
	host     string
	registry *viewer.Registry
	handler  *viewer.Handler
	records  atomic.Int32 // Record handlers running.
}

func (a *apiServer) detailBusy() int32 { return a.records.Load() }

// newAPI serves the dashboard API on a real listener, so streamed writes
// use real connection deadlines.
func newAPI(t *testing.T, mg *Manager) *apiServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := ln.Addr().String()
	registry := viewer.NewRegistry(viewer.Config{Logger: slog.New(slog.DiscardHandler)})
	h, err := viewer.NewHandler(host, registry)
	if err != nil {
		t.Fatal(err)
	}
	Register(h, mg)
	a := &apiServer{t: t, host: host, registry: registry, handler: h}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathRecord {
			a.records.Add(1)
			defer a.records.Add(-1)
		}
		h.ServeHTTP(w, r)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

func (a *apiServer) signIn() string {
	a.t.Helper()
	code, _, err := a.registry.Issue()
	if err != nil {
		a.t.Fatal(err)
	}
	token, _, err := a.registry.Login(code)
	if err != nil {
		a.t.Fatal(err)
	}
	return token
}

// post sends a same-origin browser fetch and returns status and body.
func (a *apiServer) post(token, path string, body any) (int, []byte, http.Header) {
	a.t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		a.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, a.url+path, bytes.NewReader(data))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Host = a.host
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Origin", "http://"+a.host)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Viewer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		a.t.Fatalf("%s without no-store", path)
	}
	return resp.StatusCode, out, resp.Header
}

func (a *apiServer) ok(token, path string, body, out any) {
	a.t.Helper()
	code, data, _ := a.post(token, path, body)
	if code != http.StatusOK {
		a.t.Fatalf("%s: %d %s", path, code, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		a.t.Fatalf("%s: %v", path, err)
	}
}

func errCode(t *testing.T, data []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	return e.Error
}

func TestAPIWindowCatchUpRecordAndClose(t *testing.T) {
	j := openJournal(t)
	chatStart(j, "a1", "chat-a")
	chatFinish(j, "a1")
	big := j.bigRequest(2 << 20)
	mg := NewManager(j.reader, ManagerConfig{ServerID: testServer, Status: j.log.Status})
	api := newAPI(t, mg)
	token := api.signIn()

	var env Envelope
	api.ok(token, PathWindow, map[string]any{"mode": "live"}, &env)
	if env.State != StateOK || env.LiveCursor == "" || env.Health.State != "ok" || len(env.Lanes) != 2 {
		t.Fatalf("envelope %+v", env)
	}
	// The deferred 2 MiB record ends the first page.
	if got := requestIDs(env.Recent); got != big || env.Recent.Next == nil {
		t.Fatalf("recent %q", got)
	}
	var older Page
	api.ok(token, PathRecent, map[string]any{"window": env.Window, "before": env.Recent.Next}, &older)
	if requestIDs(older) != "a1" || older.Next != nil {
		t.Fatalf("older %q", requestIDs(older))
	}
	lines := journalLines(t, j)
	var deferred *Record
	for i, r := range env.Recent.Records {
		if r.Deferred {
			deferred = &env.Recent.Records[i]
			continue
		}
		data, _ := base64.StdEncoding.DecodeString(r.DataB64)
		if !lines[string(data)] {
			t.Fatalf("record %s is not an exact journal line", r.Occurrence)
		}
	}
	if deferred == nil {
		t.Fatal("the 2 MiB record was not deferred")
	}
	code, data, header := api.post(token, PathRecord, map[string]any{"window": env.Window, "occurrence_id": deferred.Occurrence})
	if code != http.StatusOK || !lines[string(data)] || header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("record %d %d bytes", code, len(data))
	}

	chatStart(j, "a2", "chat-a")
	var cu CatchUpPage
	api.ok(token, PathCatchUp, map[string]any{"window": env.Window, "generation": env.Generation, "after": env.LiveCursor}, &cu)
	if requestIDs(cu.Page) != "a2" || len(cu.Records) != 1 || cu.Head == env.LiveCursor {
		t.Fatalf("catch-up %+v", cu)
	}
	code, data, _ = api.post(token, PathCatchUp, map[string]any{"window": env.Window, "generation": env.Generation + 1, "after": cu.Head})
	if code != http.StatusConflict || errCode(t, data) != "generation_changed" {
		t.Fatalf("other generation %d %s", code, data)
	}

	if code, _, _ := api.post(token, PathClose, map[string]any{"window": env.Window}); code != http.StatusNoContent {
		t.Fatalf("close %d", code)
	}
	code, data, _ = api.post(token, PathRecent, map[string]any{"window": env.Window})
	if code != http.StatusNotFound || errCode(t, data) != "window_gone" {
		t.Fatalf("after close %d %s", code, data)
	}
}

func TestAPIRefusesBadRequestsAndOtherSessions(t *testing.T) {
	j := openJournal(t)
	j.requests(2)
	mg := newManager(j)
	api := newAPI(t, mg)
	token := api.signIn()
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	for name, body := range map[string]any{
		"unknown field": map[string]any{"window": env.Window, "extra": 1},
		"bad mode":      map[string]any{"mode": "all"},
	} {
		path := PathRecent
		if name == "bad mode" {
			path = PathWindow
		}
		if code, data, _ := api.post(token, path, body); code != http.StatusBadRequest || errCode(t, data) != "bad_request" {
			t.Errorf("%s: %d %s", name, code, data)
		}
	}
	other := api.signIn()
	for _, path := range []string{PathRecent, PathDiscover, PathOrigins, PathClose} {
		if code, data, _ := api.post(other, path, map[string]any{"window": env.Window}); code != http.StatusNotFound ||
			errCode(t, data) != "window_gone" {
			t.Errorf("%s from another session: %d %s", path, code, data)
		}
	}
	if code, _, _ := api.post("", PathRecent, map[string]any{"window": env.Window}); code != http.StatusUnauthorized {
		t.Errorf("without a token: %d", code)
	}
	// The window limit per session (2) answers capacity.
	api.ok(token, PathWindow, map[string]any{}, &Envelope{})
	if code, data, _ := api.post(token, PathWindow, map[string]any{}); code != http.StatusConflict ||
		errCode(t, data) != "window_limit" {
		t.Errorf("third window: %d %s", code, data)
	}
}

func TestAPIDisabledAuditAnswersWithState(t *testing.T) {
	mg := NewManager(nil, ManagerConfig{ServerID: testServer, Status: func() audit.Status { return audit.DisabledStatus() }})
	api := newAPI(t, mg)
	var env Envelope
	api.ok(api.signIn(), PathWindow, map[string]any{}, &env)
	if env.State != StateDisabled || env.Health.State != "disabled" || len(env.Recent.Items) != 0 {
		t.Fatalf("envelope %+v", env)
	}
}

// TestAPILongConversationIDIsBrowsableByLaneID: a conversation ID over the
// 4 KiB body limit never has to travel in a request.
func TestAPILongConversationIDIsBrowsableByLaneID(t *testing.T) {
	j := openJournal(t)
	long := strings.Repeat("c", 6<<10)
	chatStart(j, "x1", long)
	chatFinish(j, "x1")
	j.requests(1)
	mg := newManager(j)
	api := newAPI(t, mg)
	token := api.signIn()
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	var lane string
	for _, l := range env.Lanes {
		if l.ConversationID != nil && *l.ConversationID == long {
			lane = l.ID
		}
	}
	var page Page
	api.ok(token, PathLaneItems, map[string]any{"window": env.Window, "lane_id": lane}, &page)
	if lane == "" || requestIDs(page) != "x1" {
		t.Fatalf("lane %q page %q", lane, requestIDs(page))
	}
	var lanes LanesPage
	api.ok(token, PathDiscover, map[string]any{"window": env.Window}, &lanes)
	if lanes.Coverage.End != "reached_start" || len(lanes.Lanes) != 2 {
		t.Fatalf("discover %+v", lanes)
	}
}

func TestAPIRoutesAreAuthenticatedPOST(t *testing.T) {
	mg := NewManager(nil, ManagerConfig{ServerID: testServer})
	api := newAPI(t, mg)
	found := 0
	for _, rt := range api.handler.Routes() {
		if strings.HasPrefix(rt.Path, viewer.Prefix+"api/") && rt.Path != viewer.Prefix+"api/login" {
			if !rt.Auth {
				t.Errorf("%s is not authenticated", rt.Path)
			}
		}
		switch rt.Path {
		case PathWindow, PathRecent, PathLaneItems, PathEarlier, PathDiscover, PathOrigins, PathRecord, PathCatchUp, PathClose:
			found++
			if rt.Method != http.MethodPost {
				t.Errorf("%s is %s", rt.Path, rt.Method)
			}
		}
	}
	if found != 9 {
		t.Fatalf("%d dashboard routes", found)
	}
}

// TestAPIResnapshotAfterRebuild: a rebuilt window answers generation_changed;
// its new snapshot reports possible unseen activity and a working cursor.
func TestAPIResnapshotAfterRebuild(t *testing.T) {
	j := openJournal(t)
	j.requests(2)
	mg := newManager(j)
	api := newAPI(t, mg)
	token := api.signIn()
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	code, data, _ := api.post(token, PathCatchUp, map[string]any{"window": env.Window, "generation": env.Generation, "after": "not-issued"})
	if code != http.StatusConflict || errCode(t, data) != "generation_changed" {
		t.Fatalf("unknown cursor %d %s", code, data)
	}
	var again Envelope
	api.ok(token, PathWindow, map[string]any{"window": env.Window}, &again)
	if again.Generation == env.Generation || !again.PossibleUnseen || again.LiveCursor == "" || requestIDs(again.Recent) != requestIDs(env.Recent) {
		t.Fatalf("re-snapshot gen %d->%d unseen %v", env.Generation, again.Generation, again.PossibleUnseen)
	}
	j.requests(1)
	var cu CatchUpPage
	api.ok(token, PathCatchUp, map[string]any{"window": env.Window, "generation": again.Generation, "after": again.LiveCursor}, &cu)
	if len(cu.Items) != 1 || !cu.PossibleUnseen || cu.State != StateOK {
		t.Fatalf("catch-up after rebuild %+v", cu)
	}
	for _, body := range []map[string]any{
		{"window": env.Window, "generation": again.Generation, "after": ""},
		{"window": env.Window, "mode": "live"},
	} {
		path := PathCatchUp
		if _, ok := body["mode"]; ok {
			path = PathWindow
		}
		if code, data, _ := api.post(token, path, body); code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", body, code, data)
		}
	}
}

// TestAPIFailedSnapshotReleasesTheNewWindow: the client never learned the
// window's ID, so the server closes it.
func TestAPIFailedSnapshotReleasesTheNewWindow(t *testing.T) {
	j := openJournal(t)
	j.requests(1)
	mg := newManager(j)
	api := newAPI(t, mg)
	token := api.signIn()
	mg.snapshotErr = func() error { return errors.New("broken") }
	for i := 0; i < 3; i++ {
		if code, _, _ := api.post(token, PathWindow, map[string]any{}); code != http.StatusInternalServerError {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	mg.snapshotErr = nil
	api.ok(token, PathWindow, map[string]any{}, &Envelope{})
	if mg.Windows() != 1 {
		t.Fatalf("%d windows held", mg.Windows())
	}
}

func TestAPIBusySlotsAndRecordErrors(t *testing.T) {
	j := openJournal(t)
	j.requests(2)
	mg := newManager(j)
	api := newAPI(t, mg)
	code, _, _ := api.registry.Issue()
	token, sess, err := api.registry.Login(code)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	for name, body := range map[string]string{"unknown": "AQ", "weak": "w:x:1:0-5"} {
		code, data, _ := api.post(token, PathRecord, map[string]any{"window": env.Window, "occurrence_id": body})
		want := map[string]string{"unknown": "unknown_occurrence", "weak": "unavailable"}[name]
		if errCode(t, data) != want {
			t.Errorf("%s record: %d %s", name, code, data)
		}
	}
	var lanes LanesPage
	api.ok(token, PathOrigins, map[string]any{"window": env.Window}, &lanes)
	var earlier Page
	api.ok(token, PathEarlier, map[string]any{"window": env.Window, "lane_id": lanes.Lanes[0].ID, "before": env.Recent.Next}, &earlier)
	if earlier.Coverage.End != "reached_start" {
		t.Fatalf("earlier coverage %+v", earlier.Coverage)
	}
	// Both page slots of the session are busy: the next page is refused.
	// A handler releases its slot just after the client has read the whole
	// answer, so wait for the previous requests' slots.
	for i := 0; i < 2; i++ {
		deadline := time.Now().Add(5 * time.Second)
		for {
			release, err := sess.Attach(viewer.KindPage, viewer.NewStream(httptest.NewRecorder()))
			if err == nil {
				defer release()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	code2, data, header := api.post(token, PathRecent, map[string]any{"window": env.Window})
	if code2 != http.StatusTooManyRequests || errCode(t, data) != "capacity" || header.Get("Retry-After") == "" {
		t.Fatalf("busy: %d %s", code2, data)
	}
}

// TestAPIStalledReaderCannotHoldUpStop: a client that stops reading a large
// record is cut off by the viewer stop, well before the write timeout.
func TestAPIStalledReaderCannotHoldUpStop(t *testing.T) {
	j := openJournal(t)
	j.bigRequest(4<<20 - 4096) // Near the 4 MiB record limit.
	mg := newManager(j)
	api := newAPI(t, mg)
	token := api.signIn()
	var env Envelope
	api.ok(token, PathWindow, map[string]any{}, &env)
	var occurrence string
	for _, r := range env.Recent.Records {
		if r.Deferred && r.Size > 4<<20-8192 { // The large request_start.
			occurrence = r.Occurrence
		}
	}
	if occurrence == "" {
		t.Fatalf("no large deferred record: %+v", env.Recent.Records)
	}
	conn, err := net.Dial("tcp", api.host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.(*net.TCPConn).SetReadBuffer(4096) // Let the server's writes block soon.
	body := `{"window":"` + env.Window + `","occurrence_id":"` + occurrence + `"}`
	_, err = io.WriteString(conn, "POST "+PathRecord+" HTTP/1.1\r\nHost: "+api.host+"\r\nSec-Fetch-Site: same-origin\r\n"+
		"Sec-Fetch-Mode: cors\r\nSec-Fetch-Dest: empty\r\nOrigin: http://"+api.host+"\r\nAuthorization: Viewer "+token+
		"\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the handler holds its detail slot and is blocked writing.
	deadline := time.Now().Add(5 * time.Second)
	for api.detailBusy() == 0 {
		if time.Now().After(deadline) {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			head := make([]byte, 300)
			n, _ := conn.Read(head)
			t.Fatalf("the record handler did not start: %q", head[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if api.detailBusy() == 0 {
		t.Skip("the handler finished: this platform's socket buffers took the whole record")
	}
	start := time.Now()
	api.registry.CloseAdmission(viewer.StopNormal)
	api.registry.StopAll(0)
	for api.detailBusy() != 0 {
		if time.Since(start) > chunkTimeout/2 {
			t.Fatal("a stalled reader held the handler after the stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
