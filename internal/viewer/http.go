package viewer

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"terminalmcp/internal/httpguard"
)

// Prefix is the path prefix of every viewer route.
const Prefix = "/dashboard/"

const maxViewerBody = 4 << 10

//go:embed static
var static embed.FS

// RouteKind selects the browser request shape a route accepts.
type RouteKind uint8

const (
	RoutePage  RouteKind = iota + 1 // Top-level navigation to a document.
	RouteAsset                      // Same-origin script or style.
	RouteAPI                        // Same-origin fetch.
)

// Route is one viewer route. The route table drives dispatch and the
// tunnel-exclusion tests: a route that is not in the table does not exist.
type Route struct {
	Method string
	Path   string
	Kind   RouteKind
	Auth   bool // Requires a viewer token.
}

// AuthorizedFunc serves an authenticated viewer request.
type AuthorizedFunc func(w http.ResponseWriter, r *http.Request, s *Session)

type route struct {
	Route
	serve func(http.ResponseWriter, *http.Request, *Session)
}

// Handler serves viewer routes under Prefix with the viewer guard: exact
// Host, Fetch Metadata and Origin checks, a viewer token in the
// Authorization header (never a cookie, never the MCP bearer), small bodies,
// strict security headers and no CORS.
type Handler struct {
	host     string
	origin   string
	registry *Registry

	mu     sync.RWMutex
	routes map[string]route // Keyed by method + " " + path.
}

// NewHandler returns the viewer handler for the canonical listener address
// host (for example 127.0.0.1:8090).
func NewHandler(host string, registry *Registry) (*Handler, error) {
	if err := httpguard.ValidateAddress(host); err != nil {
		return nil, err
	}
	h := &Handler{host: host, origin: "http://" + host, registry: registry, routes: make(map[string]route)}
	h.add(Route{http.MethodGet, Prefix, RoutePage, false}, h.serveStatic("static/index.html", "text/html; charset=utf-8"))
	h.add(Route{http.MethodGet, Prefix + "app.js", RouteAsset, false}, h.serveStatic("static/app.js", "text/javascript; charset=utf-8"))
	h.add(Route{http.MethodGet, Prefix + "app.css", RouteAsset, false}, h.serveStatic("static/app.css", "text/css; charset=utf-8"))
	h.add(Route{http.MethodPost, Prefix + "api/login", RouteAPI, false}, h.login)
	h.add(Route{http.MethodPost, Prefix + "api/renew", RouteAPI, true}, h.renew)
	h.add(Route{http.MethodPost, Prefix + "api/logout", RouteAPI, true}, h.logout)
	h.add(Route{http.MethodGet, Prefix + "api/session", RouteAPI, true}, h.session)
	return h, nil
}

// HandleAuthorized adds an authenticated API route (used by later history,
// detail and stream routes).
func (h *Handler) HandleAuthorized(method, path string, fn AuthorizedFunc) {
	if !strings.HasPrefix(path, Prefix+"api/") || (method != http.MethodGet && method != http.MethodPost) {
		panic("viewer: API routes are GET or POST under " + Prefix + "api/")
	}
	h.add(Route{method, path, RouteAPI, true}, fn)
}

func (h *Handler) add(rt Route, fn func(http.ResponseWriter, *http.Request, *Session)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := rt.Method + " " + rt.Path
	if _, dup := h.routes[key]; dup {
		panic("viewer: duplicate route " + key)
	}
	h.routes[key] = route{Route: rt, serve: fn}
}

// Routes lists the route table.
func (h *Handler) Routes() []Route {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Route, 0, len(h.routes))
	for _, rt := range h.routes {
		out = append(out, rt.Route)
	}
	return out
}

// Owns reports whether a request path belongs to the viewer.
func Owns(path string) bool {
	return path == strings.TrimSuffix(Prefix, "/") || strings.HasPrefix(path, Prefix)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if r.Host != h.host || r.URL.RawQuery != "" || r.URL.RawPath != "" || r.URL.Fragment != "" {
		refuse(w, r, http.StatusForbidden, "forbidden")
		return
	}
	h.mu.RLock()
	rt, ok := h.routes[r.Method+" "+r.URL.Path]
	if !ok {
		var allow []string
		for _, other := range h.routes {
			if other.Path == r.URL.Path {
				allow = append(allow, other.Method)
			}
		}
		h.mu.RUnlock()
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			refuse(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		} else {
			refuse(w, r, http.StatusNotFound, "not_found")
		}
		return
	}
	h.mu.RUnlock()
	if !h.fetchAllowed(r, rt.Kind) {
		refuse(w, r, http.StatusForbidden, "forbidden")
		return
	}
	auth := r.Header.Values("Authorization")
	for _, value := range auth {
		if !strings.HasPrefix(value, "Viewer ") {
			writeError(w, http.StatusForbidden, "forbidden") // Never the MCP bearer.
			return
		}
	}
	if r.ContentLength > maxViewerBody {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxViewerBody)
	var s *Session
	if rt.Auth {
		if len(auth) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var err error
		if s, err = h.registry.Authorize(strings.TrimPrefix(auth[0], "Viewer ")); err != nil {
			writeRegistryError(w, err)
			return
		}
	} else if len(auth) != 0 {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	rt.serve(w, r, s)
}

// fetchAllowed checks Fetch Metadata and Origin. Requests without Fetch
// Metadata are refused: every supported browser sends it.
func (h *Handler) fetchAllowed(r *http.Request, kind RouteKind) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	mode := r.Header.Get("Sec-Fetch-Mode")
	dest := r.Header.Get("Sec-Fetch-Dest")
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || (len(origins) == 1 && origins[0] != h.origin) {
		return false
	}
	switch kind {
	case RoutePage:
		return (site == "none" || site == "same-origin") && mode == "navigate" && dest == "document"
	case RouteAsset:
		wantDest := "script"
		if strings.HasSuffix(r.URL.Path, ".css") {
			wantDest = "style"
		}
		return site == "same-origin" && (mode == "no-cors" || mode == "cors" || mode == "same-origin") && dest == wantDest
	case RouteAPI:
		if r.Method != http.MethodGet && len(origins) != 1 {
			return false // Browsers always send Origin on POST.
		}
		return site == "same-origin" && (mode == "cors" || mode == "same-origin") && dest == "empty"
	}
	return false
}

func setSecurityHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; "+
		"connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

func (h *Handler) serveStatic(name, contentType string) func(http.ResponseWriter, *http.Request, *Session) {
	return func(w http.ResponseWriter, _ *http.Request, _ *Session) {
		body, err := static.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}
}

type expiryJSON struct {
	ExpiresAt         time.Time `json:"expires_at"`
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at"`
}

func expiryOf(s *Session) expiryJSON {
	idle, absolute := s.Expiry()
	return expiryJSON{ExpiresAt: idle.UTC(), AbsoluteExpiresAt: absolute.UTC()}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request, _ *Session) {
	var in struct {
		Code string `json:"code"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(&struct{}{}) != io.EOF || in.Code == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	token, s, err := h.registry.Login(in.Code)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Token string `json:"token"`
		expiryJSON
	}{token, expiryOf(s)})
}

func (h *Handler) renew(w http.ResponseWriter, r *http.Request, _ *Session) {
	s, err := h.registry.Renew(strings.TrimPrefix(r.Header.Get("Authorization"), "Viewer "))
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, expiryOf(s))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, _ *Session) {
	if err := h.registry.Logout(strings.TrimPrefix(r.Header.Get("Authorization"), "Viewer ")); err != nil {
		writeRegistryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) session(w http.ResponseWriter, _ *http.Request, s *Session) {
	writeJSON(w, http.StatusOK, struct {
		Session uint64 `json:"session"`
		expiryJSON
	}{s.ID(), expiryOf(s)})
}

func writeRegistryError(w http.ResponseWriter, err error) {
	var stop *StopError
	switch {
	case errors.Is(err, ErrStopping), errors.As(err, &stop):
		w.Header().Set("Connection", "close")
		writeError(w, http.StatusServiceUnavailable, "server_stopping")
	case errors.Is(err, ErrReplaced):
		writeError(w, http.StatusUnauthorized, "session_replaced")
	case errors.Is(err, ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "rate_limited")
	case errors.Is(err, ErrCapacity):
		writeError(w, http.StatusServiceUnavailable, "capacity")
	case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrSessionClosed):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		writeError(w, http.StatusInternalServerError, "internal")
	}
}

// WriteError writes a registry error (for example ErrCapacity or a stop) as
// a JSON error with a stable code; later viewer routes use it.
func WriteError(w http.ResponseWriter, err error) { writeRegistryError(w, err) }

// refuse answers a refused request. A person navigating to a wrong address
// gets a short plain-text hint instead of JSON; it never names the expected
// host, so a DNS-rebinding probe learns nothing.
func refuse(w http.ResponseWriter, r *http.Request, status int, code string) {
	if r.Header.Get("Sec-Fetch-Dest") != "document" {
		writeError(w, status, code)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "This page is not available here. Open the dashboard with "+
		"`terminal-mcp dashboard`, which prints a one-time code and opens the exact local address.\n")
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
