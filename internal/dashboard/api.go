package dashboard

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/viewer"
)

// API paths, all POST with a JSON body under viewer.Prefix + "api/".
const (
	PathWindow    = viewer.Prefix + "api/window"
	PathRecent    = viewer.Prefix + "api/recent"
	PathLaneItems = viewer.Prefix + "api/lane-items"
	PathEarlier   = viewer.Prefix + "api/earlier"
	PathDiscover  = viewer.Prefix + "api/discover"
	PathOrigins   = viewer.Prefix + "api/origins"
	PathRecord    = viewer.Prefix + "api/record"
	PathCatchUp   = viewer.Prefix + "api/catch-up"
	PathClose     = viewer.Prefix + "api/close"
)

const (
	writeChunk   = 64 << 10
	chunkTimeout = 10 * time.Second
)

// Health is the audit writer's health without its path or error text.
type Health struct {
	State      string `json:"state"`
	BytesUsed  int64  `json:"bytes_used,omitempty"`
	BytesLimit int64  `json:"bytes_limit,omitempty"`
}

// NeedsInfo counts origins the window is missing: pending ones may be found
// by origins; beyond_coverage ones are outside the window's history.
type NeedsInfo struct {
	Pending        int `json:"pending"`
	BeyondCoverage int `json:"beyond_coverage"`
}

// Envelope is the snapshot of a window: its state, coverage, health, lanes
// and the newest items with their records.
type Envelope struct {
	Window         string     `json:"window"`
	Mode           Mode       `json:"mode"`
	Generation     uint64     `json:"generation"`
	State          State      `json:"state"`
	LiveCursor     string     `json:"live_cursor,omitempty"`
	PossibleUnseen bool       `json:"possible_unseen"`
	Coverage       Coverage   `json:"coverage"`
	Health         Health     `json:"health"`
	Index          *IndexInfo `json:"index,omitempty"`
	Needs          NeedsInfo  `json:"needs"`
	Lanes          []Lane     `json:"lanes"`
	Recent         Page       `json:"recent"`
}

// LanesPage answers discover and origins.
type LanesPage struct {
	Generation uint64    `json:"generation"`
	Coverage   Coverage  `json:"coverage"`
	Needs      NeedsInfo `json:"needs"`
	Lanes      []Lane    `json:"lanes"`
}

// CatchUpPage is a catch-up answer with the window's live status.
type CatchUpPage struct {
	CatchUpResult
	State          State      `json:"state"`
	PossibleUnseen bool       `json:"possible_unseen"`
	Index          *IndexInfo `json:"index,omitempty"`
	Needs          NeedsInfo  `json:"needs"`
}

func (mg *Manager) health() Health {
	if mg.status == nil {
		return Health{State: "ok"}
	}
	st := mg.status()
	return Health{State: st.State, BytesUsed: st.BytesUsed, BytesLimit: st.BytesLimit}
}

// liveStatus reads the index state and needs of a window (index lock inside the
// window lock).
func (mg *Manager) liveStatus(m *managed) (*IndexInfo, NeedsInfo) {
	var info *IndexInfo
	var needs NeedsInfo
	m.win.With(func(p *activity.Projection) {
		for _, n := range p.Needs() {
			if n.BeyondCoverage {
				needs.BeyondCoverage++
			} else {
				needs.Pending++
			}
		}
		if mg.index != nil && m.mode == ModeLive {
			i := mg.index.Info(p)
			info = &i
		}
	})
	return info, needs
}

// Snapshot returns the envelope of a window. A window that cannot serve
// views (failed, capacity, disabled) still answers with its state. The live
// cursor, possible_unseen and items come from one consistent window state.
func (mg *Manager) Snapshot(ctx context.Context, owner viewer.Attacher, id string) (Envelope, error) {
	if mg.snapshotErr != nil {
		if err := mg.snapshotErr(); err != nil {
			return Envelope{}, err
		}
	}
	m, err := mg.get(owner, id)
	if err != nil {
		return Envelope{}, err
	}
	gen, state, _, unseen := m.win.Info()
	env := Envelope{Window: id, Mode: m.mode, Generation: gen, State: state, PossibleUnseen: unseen,
		Health: mg.health(), Lanes: []Lane{}, Recent: Page{Generation: gen, Items: []Item{}, Records: []Record{}}}
	if state != StateOK {
		return env, nil
	}
	err = mg.run(ctx, owner, id, func(m *managed) error {
		s := mg.collect(m)
		env.Generation, env.Coverage, env.Lanes = s.generation, s.coverage, lanesOf(s.items)
		env.LiveCursor, env.PossibleUnseen = s.head, s.unseen
		env.Recent = mg.deliver(ctx, s, s.items, nil, nil)
		env.Index, env.Needs = mg.liveStatus(m)
		return nil
	})
	return env, err
}

// Discover scans older history within the budget and returns the lanes.
func (mg *Manager) Discover(ctx context.Context, owner viewer.Attacher, id string) (LanesPage, error) {
	return mg.lanesAfter(ctx, owner, id, func(m *managed) error { return mg.scan(ctx, m, scanPages) })
}

// Backfill looks for missing origins and returns the lanes.
func (mg *Manager) Backfill(ctx context.Context, owner viewer.Attacher, id string) (LanesPage, error) {
	if _, err := mg.Origins(ctx, owner, id); err != nil {
		return LanesPage{}, err
	}
	return mg.lanesAfter(ctx, owner, id, nil)
}

func (mg *Manager) lanesAfter(ctx context.Context, owner viewer.Attacher, id string, before func(*managed) error) (LanesPage, error) {
	var out LanesPage
	err := mg.run(ctx, owner, id, func(m *managed) error {
		if before != nil {
			if err := before(m); err != nil {
				return err
			}
		}
		s := mg.collect(m)
		out = LanesPage{Generation: s.generation, Coverage: s.coverage, Lanes: lanesOf(s.items)}
		_, out.Needs = mg.liveStatus(m)
		return nil
	})
	return out, err
}

// Register adds the dashboard API routes to the viewer handler. They are
// registered even when the audit is disabled; windows then answer with
// state disabled.
func Register(h *viewer.Handler, mg *Manager) {
	h.HandleAuthorized(http.MethodPost, PathWindow, func(w http.ResponseWriter, r *http.Request, s *viewer.Session) {
		req, ok := decode(w, r, "mode", "from_window", "window")
		if !ok {
			return
		}
		var opened string
		written := serveStreamed(w, r, s, viewer.KindPage, jsonBody(func(ctx context.Context) (any, error) {
			if req.Window != "" { // Snapshot of an existing window, for example after a rebuild.
				if req.Mode != "" || req.FromWindow != "" {
					return nil, errBadRequest
				}
				return mg.Snapshot(ctx, s, req.Window)
			}
			mode := Mode(req.Mode)
			if mode == "" {
				mode = ModeLive
			}
			if mode != ModeLive && mode != ModeArchive {
				return nil, errBadRequest
			}
			id, err := mg.Open(ctx, s, mode, req.FromWindow)
			if err != nil {
				return nil, err
			}
			opened = id
			return mg.Snapshot(ctx, s, id)
		}))
		if opened != "" && !written {
			_ = mg.Close(s, opened) // The client never learned the window's ID.
		}
	})
	pageRoute := func(path string, fields []string, fn func(ctx context.Context, s *viewer.Session, req *request) (any, error)) {
		h.HandleAuthorized(http.MethodPost, path, func(w http.ResponseWriter, r *http.Request, s *viewer.Session) {
			req, ok := decode(w, r, fields...)
			if !ok {
				return
			}
			serveStreamed(w, r, s, viewer.KindPage, jsonBody(func(ctx context.Context) (any, error) { return fn(ctx, s, req) }))
		})
	}
	pageRoute(PathRecent, []string{"window", "before"}, func(ctx context.Context, s *viewer.Session, req *request) (any, error) {
		return mg.Recent(ctx, s, req.Window, req.Before)
	})
	pageRoute(PathLaneItems, []string{"window", "lane_id", "before"}, func(ctx context.Context, s *viewer.Session, req *request) (any, error) {
		return mg.LaneItems(ctx, s, req.Window, req.Lane, req.Before)
	})
	pageRoute(PathEarlier, []string{"window", "lane_id", "before"}, func(ctx context.Context, s *viewer.Session, req *request) (any, error) {
		return mg.Earlier(ctx, s, req.Window, req.Lane, req.Before)
	})
	pageRoute(PathDiscover, []string{"window"}, func(ctx context.Context, s *viewer.Session, req *request) (any, error) {
		return mg.Discover(ctx, s, req.Window)
	})
	pageRoute(PathOrigins, []string{"window"}, func(ctx context.Context, s *viewer.Session, req *request) (any, error) {
		return mg.Backfill(ctx, s, req.Window)
	})
	pageRoute(PathCatchUp, []string{"window", "generation", "after", "before"}, func(ctx context.Context, s *viewer.Session, req *request) (any, error) {
		if req.Generation == nil {
			return nil, errBadRequest
		}
		res, err := mg.CatchUp(ctx, s, req.Window, *req.Generation, req.After, req.Before)
		if err != nil {
			return nil, err
		}
		out := CatchUpPage{CatchUpResult: res, State: StateOK}
		if m, err := mg.get(s, req.Window); err == nil {
			out.Index, out.Needs = mg.liveStatus(m)
			_, out.State, _, out.PossibleUnseen = m.win.Info()
		}
		return out, nil
	})
	h.HandleAuthorized(http.MethodPost, PathRecord, func(w http.ResponseWriter, r *http.Request, s *viewer.Session) {
		req, ok := decode(w, r, "window", "occurrence_id")
		if !ok {
			return
		}
		serveStreamed(w, r, s, viewer.KindDetail, func(ctx context.Context) ([]byte, string, error) {
			data, err := mg.RecordBytes(ctx, s, req.Window, req.Occurrence)
			return data, "application/octet-stream", err
		})
	})
	h.HandleAuthorized(http.MethodPost, PathClose, func(w http.ResponseWriter, r *http.Request, s *viewer.Session) {
		req, ok := decode(w, r, "window")
		if !ok {
			return
		}
		if err := mg.Close(s, req.Window); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// request is the union of all endpoint bodies. Each endpoint names the
// fields it accepts; any other field is refused.
type request struct {
	Mode       string        `json:"mode,omitempty"`
	FromWindow string        `json:"from_window,omitempty"`
	Window     string        `json:"window"`
	Lane       string        `json:"lane_id,omitempty"`
	Before     *Continuation `json:"before,omitempty"`
	Generation *uint64       `json:"generation,omitempty"`
	After      string        `json:"after,omitempty"`
	Occurrence string        `json:"occurrence_id,omitempty"`
}

var errBadRequest = errors.New("dashboard: bad request")

// decode reads one JSON object holding only the allowed fields.
func decode(w http.ResponseWriter, r *http.Request, allowed ...string) (*request, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, errBadRequest)
		return nil, false
	}
	var fields map[string]json.RawMessage
	var req request
	if json.Unmarshal(raw, &fields) != nil || fields == nil || !onlyFields(fields, allowed) {
		writeError(w, errBadRequest)
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, errBadRequest)
		return nil, false
	}
	return &req, true
}

func onlyFields(fields map[string]json.RawMessage, allowed []string) bool {
	for name := range fields {
		if !slices.Contains(allowed, name) {
			return false
		}
	}
	return true
}

// jsonBody adapts a JSON-producing endpoint to serveStreamed.
func jsonBody(fn func(ctx context.Context) (any, error)) func(ctx context.Context) ([]byte, string, error) {
	return func(ctx context.Context) ([]byte, string, error) {
		v, err := fn(ctx)
		if err != nil {
			return nil, "", err
		}
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "application/json", nil
	}
}

// serveStreamed computes a response with a context canceled when the
// session ends, and writes it in chunks through an attached stream, so a
// client that stopped reading cannot hold up shutdown: every write,
// including the final flush, runs under a bounded deadline while the stream
// is still attached. It reports whether the whole body was written.
func serveStreamed(w http.ResponseWriter, r *http.Request, s *viewer.Session, kind viewer.Kind,
	fn func(ctx context.Context) ([]byte, string, error)) bool {
	st := viewer.NewStream(w)
	release, err := s.Attach(kind, st)
	if err != nil {
		writeError(w, err)
		return false
	}
	defer release()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(s.Context(), cancel)()
	body, contentType, err := fn(ctx)
	if err == nil {
		err = ctx.Err()
	}
	// A stopped stream keeps its stop deadline; a writer without deadline
	// support (http.ErrNotSupported) is written to as is.
	if errors.Is(st.Extend(time.Now().Add(chunkTimeout)), viewer.ErrStreamStopped) {
		_, _, cause := st.Stopped()
		w.Header().Set("Connection", "close")
		writeError(w, cmp.Or(cause, viewer.ErrStopping))
		return false
	}
	if err != nil {
		if cause := context.Cause(s.Context()); cause != nil {
			err = cause
		}
		writeError(w, err)
		return false
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	for len(body) > 0 {
		if errors.Is(st.Extend(time.Now().Add(chunkTimeout)), viewer.ErrStreamStopped) {
			return false
		}
		n := min(len(body), writeChunk)
		if _, err := w.Write(body[:n]); err != nil {
			return false
		}
		body = body[n:]
	}
	if errors.Is(st.Extend(time.Now().Add(chunkTimeout)), viewer.ErrStreamStopped) {
		return false
	}
	// Flush the buffered tail while the deadline still applies; the server's
	// write timeout resets the deadline for the next request.
	err = http.NewResponseController(w).Flush()
	return err == nil || errors.Is(err, http.ErrNotSupported)
}

// writeError answers with a stable code and never with record bytes,
// conversation IDs or request bodies.
func writeError(w http.ResponseWriter, err error) {
	var rebuilding *RebuildingError
	var state *StateError
	var stop *viewer.StopError
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, errBadRequest):
		status, code = http.StatusBadRequest, "bad_request"
	case errors.Is(err, ErrWindowGone):
		status, code = http.StatusNotFound, "window_gone"
	case errors.Is(err, ErrGenerationChanged):
		status, code = http.StatusConflict, "generation_changed"
	case errors.Is(err, ErrRebuild): // Rebuilt: take a new snapshot of the same window.
		status, code = http.StatusConflict, "generation_changed"
	case errors.Is(err, ErrWindowLimit):
		status, code = http.StatusConflict, "window_limit"
	case errors.Is(err, ErrNoFloor):
		status, code = http.StatusConflict, "no_floor"
	case errors.Is(err, ErrUseArchive):
		status, code = http.StatusConflict, "use_archive"
	case errors.Is(err, ErrUnknownOccurrence):
		status, code = http.StatusNotFound, "unknown_occurrence"
	case errors.Is(err, ErrUnavailable):
		status, code = http.StatusGone, "unavailable"
	case errors.As(err, &rebuilding):
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(rebuilding.RetryAfter.Round(time.Second)/time.Second))))
		status, code = http.StatusServiceUnavailable, "rebuilding"
	case errors.As(err, &state):
		// A terminal window state: open another window (409, not retryable).
		status, code = http.StatusConflict, string(state.State)
	case errors.Is(err, ErrCapacity):
		status, code = http.StatusConflict, string(StateCapacity)
	case errors.Is(err, ErrDisabled):
		status, code = http.StatusConflict, string(StateDisabled)
	case errors.Is(err, viewer.ErrCapacity):
		// Every resource slot of the session is busy: retry shortly.
		w.Header().Set("Retry-After", "1")
		status, code = http.StatusTooManyRequests, "capacity"
	case errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "1")
		status, code = http.StatusServiceUnavailable, "timeout"
	case errors.Is(err, viewer.ErrSessionClosed), errors.As(err, &stop), errors.Is(err, viewer.ErrStopping),
		errors.Is(err, viewer.ErrLoggedOut), errors.Is(err, viewer.ErrExpired), errors.Is(err, viewer.ErrRevoked),
		errors.Is(err, viewer.ErrReplaced):
		viewerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{code})
}

// viewerError maps a session's end cause to the viewer's error codes.
func viewerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, viewer.ErrLoggedOut), errors.Is(err, viewer.ErrExpired), errors.Is(err, viewer.ErrRevoked):
		viewer.WriteError(w, viewer.ErrSessionClosed)
	default:
		viewer.WriteError(w, err)
	}
}
