package viewer

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testHost = "127.0.0.1:8090"

func newTestHandler(t *testing.T) (*Handler, *Registry) {
	t.Helper()
	h, r, _ := newClockedHandler(t)
	return h, r
}

func newClockedHandler(t *testing.T) (*Handler, *Registry, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	r := NewRegistry(Config{Clock: clock, Logger: slog.New(slog.DiscardHandler)})
	h, err := NewHandler(testHost, r)
	if err != nil {
		t.Fatal(err)
	}
	return h, r, clock
}

type reqOpt func(*http.Request)

func header(name, value string) reqOpt { return func(r *http.Request) { r.Header.Set(name, value) } }
func without(name string) reqOpt       { return func(r *http.Request) { r.Header.Del(name) } }
func host(h string) reqOpt             { return func(r *http.Request) { r.Host = h } }

// browser builds a request as a supported browser sends it for the route
// kind, then applies opts.
func browser(method, path, body string, kind RouteKind, opts ...reqOpt) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://"+testHost+path, reader)
	r.Host = testHost
	switch kind {
	case RoutePage:
		r.Header.Set("Sec-Fetch-Site", "none")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Sec-Fetch-Dest", "document")
	case RouteAsset:
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Sec-Fetch-Mode", "no-cors")
		dest := "script"
		if strings.HasSuffix(path, ".css") {
			dest = "style"
		}
		r.Header.Set("Sec-Fetch-Dest", dest)
	case RouteAPI:
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Sec-Fetch-Mode", "cors")
		r.Header.Set("Sec-Fetch-Dest", "empty")
		if method != http.MethodGet {
			r.Header.Set("Origin", "http://"+testHost)
		}
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Error
}

func checkSecurityHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Cache-Control":                "no-store",
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for name, value := range want {
		if got := w.Header().Get(name); got != value {
			t.Fatalf("%s = %q, want %q", name, got, value)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") ||
		!strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe") {
		t.Fatalf("CSP %q", csp)
	}
	if w.Header().Get("Content-Type") == "" && w.Code != http.StatusNoContent {
		t.Fatal("missing Content-Type")
	}
	for _, name := range []string{"Access-Control-Allow-Origin", "Set-Cookie", "Location"} {
		if w.Header().Get(name) != "" {
			t.Fatalf("unexpected %s", name)
		}
	}
}

func signIn(t *testing.T, h *Handler, r *Registry) string {
	t.Helper()
	code, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	w := serve(h, browser(http.MethodPost, Prefix+"api/login", `{"code":"`+code+`"}`, RouteAPI))
	if w.Code != http.StatusOK {
		t.Fatalf("login %d %s", w.Code, w.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("login body %s", w.Body)
	}
	return out.Token
}

func TestStaticPagesAreServedToBrowsers(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, tt := range []struct {
		path, contentType string
		kind              RouteKind
		opts              []reqOpt
	}{
		{Prefix, "text/html", RoutePage, nil},
		{Prefix, "text/html", RoutePage, []reqOpt{header("Sec-Fetch-Site", "same-origin")}}, // Reload.
		{Prefix + "app.js", "text/javascript", RouteAsset, nil},
		{Prefix + "app.css", "text/css", RouteAsset, nil},
		{Prefix + "app.js", "text/javascript", RouteAsset, []reqOpt{header("Sec-Fetch-Mode", "cors")}},
	} {
		w := serve(h, browser(http.MethodGet, tt.path, "", tt.kind, tt.opts...))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), tt.contentType) {
			t.Fatalf("%s: %d %q", tt.path, w.Code, w.Header().Get("Content-Type"))
		}
		checkSecurityHeaders(t, w)
	}
	page := serve(h, browser(http.MethodGet, Prefix, "", RoutePage)).Body.String()
	lower := strings.ToLower(page)
	for _, banned := range []string{"<script>", "style=", "<form", "http:", "https:", "//"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("the page must stay CSP-compatible and self-contained; found %q", banned)
		}
	}
	if handler := regexp.MustCompile(`\son[a-z]+\s*=`).FindString(lower); handler != "" {
		t.Fatalf("inline event handler %q", handler)
	}
	if strings.Count(lower, "<script") != 1 || !strings.Contains(lower, `<script type="module" src="/dashboard/app.js"`) {
		t.Fatal("exactly one external same-origin script is expected")
	}
}

func TestGuardRefusesUnexpectedRequests(t *testing.T) {
	h, r := newTestHandler(t)
	token := signIn(t, h, r)
	viewer := header("Authorization", "Viewer "+token)
	session := Prefix + "api/session"
	login := Prefix + "api/login"
	tests := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"localhost host", browser("GET", Prefix, "", RoutePage, host("localhost:8090")), 403},
		{"other port host", browser("GET", Prefix, "", RoutePage, host("127.0.0.1:8091")), 403},
		{"ipv6 host", browser("GET", Prefix, "", RoutePage, host("[::1]:8090")), 403},
		{"query", browser("GET", Prefix+"?code=x", "", RoutePage), 403},
		{"encoded path", browser("GET", "/dashboard/api%2Fsession", "", RouteAPI, viewer), 403},
		{"cross-site navigation", browser("GET", Prefix, "", RoutePage, header("Sec-Fetch-Site", "cross-site")), 403},
		{"same-site navigation", browser("GET", Prefix, "", RoutePage, header("Sec-Fetch-Site", "same-site")), 403},
		{"framed page", browser("GET", Prefix, "", RoutePage, header("Sec-Fetch-Dest", "iframe")), 403},
		{"page without metadata", browser("GET", Prefix, "", RoutePage, without("Sec-Fetch-Site")), 403},
		{"script from another site", browser("GET", Prefix+"app.js", "", RouteAsset, header("Sec-Fetch-Site", "cross-site")), 403},
		{"script loaded as style", browser("GET", Prefix+"app.js", "", RouteAsset, header("Sec-Fetch-Dest", "style")), 403},
		{"api cross-site", browser("GET", session, "", RouteAPI, viewer, header("Sec-Fetch-Site", "cross-site")), 403},
		{"api same-site other port", browser("GET", session, "", RouteAPI, viewer, header("Sec-Fetch-Site", "same-site"), header("Origin", "http://127.0.0.1:9999")), 403},
		{"api navigation", browser("GET", session, "", RouteAPI, viewer, header("Sec-Fetch-Mode", "navigate"), header("Sec-Fetch-Dest", "document")), 403},
		{"api without metadata", browser("GET", session, "", RouteAPI, viewer, without("Sec-Fetch-Site"), without("Sec-Fetch-Mode"), without("Sec-Fetch-Dest")), 403},
		{"api other origin", browser("GET", session, "", RouteAPI, viewer, header("Origin", "http://localhost:8090")), 403},
		{"api null origin", browser("POST", login, `{"code":"x"}`, RouteAPI, header("Origin", "null")), 403},
		{"post without origin", browser("POST", login, `{"code":"x"}`, RouteAPI, without("Origin")), 403},
		{"mcp bearer on session", browser("GET", session, "", RouteAPI, header("Authorization", "Bearer "+strings.Repeat("t", 40))), 403},
		{"mcp bearer on login", browser("POST", login, `{"code":"x"}`, RouteAPI, header("Authorization", "Bearer x")), 403},
		{"viewer token on login", browser("POST", login, `{"code":"x"}`, RouteAPI, viewer), 403},
		{"cookie only", browser("GET", session, "", RouteAPI, header("Cookie", "token="+token)), 401},
		{"wrong token", browser("GET", session, "", RouteAPI, header("Authorization", "Viewer nope")), 401},
		{"too large", browser("POST", login, `{"code":"`+strings.Repeat("x", 5000)+`"}`, RouteAPI), 413},
		{"unknown fields", browser("POST", login, `{"code":"x","extra":1}`, RouteAPI), 400},
		{"trailing json", browser("POST", login, `{"code":"x"}{}`, RouteAPI), 400},
		{"method", browser("GET", login, "", RouteAPI), 405},
		{"head", browser("HEAD", Prefix, "", RoutePage), 405},
		{"options", browser("OPTIONS", session, "", RouteAPI), 405},
		{"unknown path", browser("GET", Prefix+"nope", "", RoutePage), 404},
		{"bare prefix", browser("GET", "/dashboard", "", RoutePage), 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(h, tt.req)
			if w.Code != tt.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.status, w.Body)
			}
			checkSecurityHeaders(t, w)
		})
	}

	dup := browser("GET", session, "", RouteAPI)
	dup.Header.Add("Authorization", "Viewer "+token)
	dup.Header.Add("Authorization", "Viewer "+token)
	if w := serve(h, dup); w.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate authorization: %d", w.Code)
	}
	if w := serve(h, browser("OPTIONS", login, "", RouteAPI)); w.Header().Get("Allow") != "POST" {
		t.Fatalf("Allow %q", w.Header().Get("Allow"))
	}
	chunked := browser("POST", login, "", RouteAPI)
	chunked.Body = io.NopCloser(strings.NewReader(`{"code":"` + strings.Repeat("x", 5000) + `"}`))
	chunked.ContentLength = -1
	if w := serve(h, chunked); w.Code != http.StatusBadRequest {
		t.Fatalf("chunked oversized body: %d", w.Code)
	}
	twoOrigins := browser("GET", session, "", RouteAPI, viewer)
	twoOrigins.Header.Add("Origin", "http://"+testHost)
	twoOrigins.Header.Add("Origin", "http://"+testHost)
	if w := serve(h, twoOrigins); w.Code != http.StatusForbidden {
		t.Fatalf("duplicate origin: %d", w.Code)
	}
}

func TestLoginRenewLogout(t *testing.T) {
	h, r := newTestHandler(t)
	token := signIn(t, h, r)
	auth := header("Authorization", "Viewer "+token)
	if w := serve(h, browser("GET", Prefix+"api/session", "", RouteAPI, auth)); w.Code != 200 || !strings.Contains(w.Body.String(), "absolute_expires_at") {
		t.Fatalf("session %d %s", w.Code, w.Body)
	}
	if w := serve(h, browser("POST", Prefix+"api/renew", "", RouteAPI, auth)); w.Code != 200 {
		t.Fatalf("renew %d %s", w.Code, w.Body)
	}
	if w := serve(h, browser("POST", Prefix+"api/logout", "", RouteAPI, auth)); w.Code != http.StatusNoContent {
		t.Fatalf("logout %d %s", w.Code, w.Body)
	}
	w := serve(h, browser("GET", Prefix+"api/session", "", RouteAPI, auth))
	if w.Code != 401 || errorCode(t, w) != "unauthorized" {
		t.Fatalf("after logout %d %s", w.Code, w.Body)
	}
}

func TestLoginFailuresAreUniformAndThrottled(t *testing.T) {
	h, r, clock := newClockedHandler(t)
	expired, _, _ := r.Issue()
	clock.Advance(6 * time.Minute)
	valid, _, _ := r.Issue()
	used, _, _ := r.Issue()
	_, _, _ = r.Login(used)
	var bodies []string
	for _, code := range []string{"unknown", used, expired} {
		w := serve(h, browser("POST", Prefix+"api/login", `{"code":"`+code+`"}`, RouteAPI))
		if w.Code != 401 {
			t.Fatalf("%s: %d", code, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("failure responses differ: %q", bodies)
	}
	for i := 0; i < 2; i++ {
		serve(h, browser("POST", Prefix+"api/login", `{"code":"junk"}`, RouteAPI))
	}
	w := serve(h, browser("POST", Prefix+"api/login", `{"code":"junk"}`, RouteAPI))
	if w.Code != http.StatusTooManyRequests || errorCode(t, w) != "rate_limited" {
		t.Fatalf("throttle %d %s", w.Code, w.Body)
	}
	if w := serve(h, browser("POST", Prefix+"api/login", `{"code":"`+valid+`"}`, RouteAPI)); w.Code != 200 {
		t.Fatalf("valid code while throttled: %d", w.Code)
	}
}

func TestStoppingAndReplacedResponses(t *testing.T) {
	h, r := newTestHandler(t)
	var tokens []string
	for i := 0; i < 9; i++ {
		tokens = append(tokens, signIn(t, h, r))
	}
	w := serve(h, browser("GET", Prefix+"api/session", "", RouteAPI, header("Authorization", "Viewer "+tokens[0])))
	if w.Code != 401 || errorCode(t, w) != "session_replaced" {
		t.Fatalf("replaced %d %s", w.Code, w.Body)
	}
	r.CloseAdmission(StopUpdate)
	for _, req := range []*http.Request{
		browser("GET", Prefix+"api/session", "", RouteAPI, header("Authorization", "Viewer "+tokens[8])),
		browser("POST", Prefix+"api/login", `{"code":"x"}`, RouteAPI),
	} {
		w := serve(h, req)
		if w.Code != 503 || errorCode(t, w) != "server_stopping" || w.Header().Get("Connection") != "close" {
			t.Fatalf("%s while stopping: %d %s %v", req.URL.Path, w.Code, w.Body, w.Header())
		}
		checkSecurityHeaders(t, w)
	}
}

func TestRouteTableAndExtraRoutes(t *testing.T) {
	h, r := newTestHandler(t)
	h.HandleAuthorized(http.MethodGet, Prefix+"api/history", func(w http.ResponseWriter, _ *http.Request, s *Session) {
		_, _ = io.WriteString(w, "history for "+s.String())
	})
	found, others := false, 0
	for _, rt := range h.Routes() {
		if !Owns(rt.Path) {
			t.Fatalf("route %s outside the viewer prefix", rt.Path)
		}
		found = found || (rt.Path == Prefix+"api/history" && rt.Auth && rt.Kind == RouteAPI)
		if rt.Kind != RouteAsset {
			others++
		}
	}
	if !found || others != 6 { // The page, 4 API routes and the extra one, plus every asset.
		t.Fatalf("routes %+v", h.Routes())
	}
	token := signIn(t, h, r)
	if w := serve(h, browser("GET", Prefix+"api/history", "", RouteAPI)); w.Code != 401 {
		t.Fatalf("extra route without token: %d", w.Code)
	}
	if w := serve(h, browser("GET", Prefix+"api/history", "", RouteAPI, header("Authorization", "Viewer "+token))); w.Code != 200 {
		t.Fatalf("extra route: %d", w.Code)
	}
	for _, bad := range [][2]string{{http.MethodGet, "/mcp"}, {http.MethodDelete, Prefix + "api/x"}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%v must panic", bad)
				}
			}()
			h.HandleAuthorized(bad[0], bad[1], nil)
		}()
	}
}

func TestNavigationRefusalsAreReadable(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, req := range []*http.Request{
		browser("GET", Prefix, "", RoutePage, host("localhost:8090")),
		browser("GET", "/dashboard", "", RoutePage),
	} {
		w := serve(h, req)
		if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") ||
			!strings.Contains(w.Body.String(), "terminal-mcp dashboard") || strings.Contains(w.Body.String(), testHost) {
			t.Fatalf("%s: %q %q", req.Host+req.URL.Path, w.Header().Get("Content-Type"), w.Body)
		}
		checkSecurityHeaders(t, w)
	}
}

func TestOwns(t *testing.T) {
	for path, want := range map[string]bool{
		"/dashboard": true, "/dashboard/": true, "/dashboard/api/x": true,
		"/dashboardx": false, "/mcp": false, "/admin/runtime": false, "/": false,
	} {
		if Owns(path) != want {
			t.Errorf("Owns(%q) != %v", path, want)
		}
	}
	if _, err := NewHandler("localhost:8090", NewRegistry(Config{})); err == nil {
		t.Fatal("a non-literal host must be refused")
	}
}
