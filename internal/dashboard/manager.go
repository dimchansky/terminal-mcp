package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
	"terminalmcp/internal/viewer"
)

var (
	// ErrWindowGone: unknown window, another session's window, or a window
	// that was closed or released. The answers are indistinguishable.
	ErrWindowGone = errors.New("dashboard: window gone")
	// ErrNoFloor: an archive was requested from a window that has not
	// evicted anything; its history chain still reads older pages.
	ErrNoFloor = errors.New("dashboard: the window has no floor yet")
	// ErrGenerationChanged: an ID or continuation from an older generation
	// of the window; the client must take a new snapshot.
	ErrGenerationChanged = errors.New("dashboard: window generation changed")
)

// RebuildingError asks the client to retry after a backoff.
type RebuildingError struct{ RetryAfter time.Duration }

func (e *RebuildingError) Error() string { return "dashboard: window is rebuilding" }

// rebuildBackoff is the wait before each rebuild attempt; after the last
// one fails the window is failed for good.
var rebuildBackoff = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

const idleRelease = 15 * time.Minute

// Mode selects a live window or a history-only archive window.
type Mode string

const (
	ModeLive    Mode = "live"
	ModeArchive Mode = "archive"
)

// Manager owns every projection window of the server.
type Manager struct {
	reader   *audit.Reader
	serverID string
	capacity int
	now      func() time.Time
	// beforeRaise lets the in-flight index sync windows before eviction.
	beforeRaise func(*activity.Projection)
	index       *Index

	mu      sync.Mutex
	windows map[string]*managed
}

// ManagerConfig configures a manager.
type ManagerConfig struct {
	ServerID    string
	Capacity    int // Per window; 0 selects 50,000.
	Now         func() time.Time
	BeforeRaise func(*activity.Projection)
	Index       *Index // Server-wide in-flight index; may be nil.
}

// NewManager returns a manager reading reader (nil when audit is disabled).
func NewManager(reader *audit.Reader, cfg ManagerConfig) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.BeforeRaise == nil && cfg.Index != nil {
		cfg.BeforeRaise = cfg.Index.Sync
	}
	return &Manager{reader: reader, serverID: cfg.ServerID, capacity: cfg.Capacity, now: cfg.Now,
		beforeRaise: cfg.BeforeRaise, index: cfg.Index, windows: make(map[string]*managed)}
}

type managed struct {
	mg      *Manager
	id      string
	owner   viewer.Attacher
	mode    Mode
	win     *Window
	hints   *hints
	release func()

	mu          sync.Mutex
	lanes       *laneIDs
	catchUp     *catchUpState
	rebuilding  bool
	closed      bool
	lastUsed    time.Time
	failures    int
	nextRebuild time.Time
}

// Stop implements viewer.Resource: it marks the window closed and lets the
// manager forget it asynchronously, never blocking or releasing here.
func (m *managed) Stop(error, time.Time) {
	if m.markClosed() && m.mg != nil {
		go m.mg.forget(m)
	}
}

func (m *managed) markClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	first := !m.closed
	m.closed = true
	return first
}

func (m *managed) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Open builds a window for the session. An archive window replaces the
// session's previous archive window (a sliding archive) and starts at the
// current floor of fromWindow, a window of the same session.
func (mg *Manager) Open(ctx context.Context, owner viewer.Attacher, mode Mode, fromWindow string) (string, error) {
	var from audit.Cursor
	if mode == ModeArchive {
		src, err := mg.get(owner, fromWindow)
		if err != nil {
			return "", err
		}
		var cov activity.CoverageView
		src.win.With(func(p *activity.Projection) { cov = p.Coverage() })
		if !cov.FloorSet {
			return "", ErrNoFloor
		}
		from = audit.Cursor(cov.Floor)
		mg.closeArchives(owner)
	} else if mode != ModeLive {
		return "", errors.New("dashboard: unknown window mode")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	mg.sweep()
	m := &managed{mg: mg, id: hex.EncodeToString(raw[:]), owner: owner, mode: mode, hints: newHints(), lastUsed: mg.now()}
	release, err := owner.Attach(viewer.KindWindow, m)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.release = release
	m.mu.Unlock()
	cfg := WindowConfig{ServerID: mg.serverID, Capacity: mg.capacity, OnApply: m.hints.observe, BeforeRaise: mg.beforeRaise}
	var w *Window
	if mode == ModeArchive {
		w, err = NewArchiveWindow(ctx, mg.reader, from, cfg)
	} else {
		w, err = NewLiveWindow(ctx, mg.reader, cfg)
	}
	if err != nil && !errors.Is(err, ErrDisabled) {
		release()
		return "", err
	}
	m.win = w
	mg.mu.Lock()
	mg.windows[m.id] = m
	mg.mu.Unlock()
	if m.isClosed() { // The session ended while the window was built.
		mg.forget(m)
		return "", ErrWindowGone
	}
	return m.id, nil
}

// get returns the session's open window, sweeping closed and idle windows.
func (mg *Manager) get(owner viewer.Attacher, id string) (*managed, error) {
	mg.sweep()
	mg.mu.Lock()
	m, ok := mg.windows[id]
	mg.mu.Unlock()
	if !ok || m.owner != owner || m.isClosed() {
		return nil, ErrWindowGone
	}
	m.mu.Lock()
	m.lastUsed = mg.now()
	m.mu.Unlock()
	return m, nil
}

// Close releases a window of the session.
func (mg *Manager) Close(owner viewer.Attacher, id string) error {
	m, err := mg.get(owner, id)
	if err != nil {
		return err
	}
	mg.drop(m)
	return nil
}

func (mg *Manager) closeArchives(owner viewer.Attacher) {
	mg.mu.Lock()
	var old []*managed
	for _, m := range mg.windows {
		if m.owner == owner && m.mode == ModeArchive {
			old = append(old, m)
		}
	}
	mg.mu.Unlock()
	for _, m := range old {
		mg.drop(m)
	}
}

// drop closes and forgets a window synchronously.
func (mg *Manager) drop(m *managed) {
	m.markClosed()
	mg.forget(m)
}

// forget removes a window and releases its session resource (idempotent).
func (mg *Manager) forget(m *managed) {
	mg.mu.Lock()
	delete(mg.windows, m.id)
	mg.mu.Unlock()
	m.mu.Lock()
	release := m.release
	m.release = nil
	m.mu.Unlock()
	if release != nil {
		release()
	}
}

// sweep forgets windows whose session ended and releases idle ones.
func (mg *Manager) sweep() {
	now := mg.now()
	mg.mu.Lock()
	var gone []*managed
	for _, m := range mg.windows {
		m.mu.Lock()
		dead := m.closed || now.Sub(m.lastUsed) >= idleRelease
		m.mu.Unlock()
		if dead {
			gone = append(gone, m)
		}
	}
	mg.mu.Unlock()
	for _, m := range gone {
		mg.drop(m)
	}
}

// Windows reports how many windows are held (for tests and status).
func (mg *Manager) Windows() int {
	mg.sweep()
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return len(mg.windows)
}

// run executes op on the window; a window that must be rebuilt is rebuilt
// lazily with backoff, never sleeping in the request.
func (mg *Manager) run(ctx context.Context, owner viewer.Attacher, id string, op func(*managed) error) error {
	m, err := mg.get(owner, id)
	if err != nil {
		return err
	}
	if _, state, _, _ := m.win.Info(); state == StateFailed || state == StateCapacity || state == StateDisabled {
		return stateError(state)
	}
	if mg.index != nil {
		_ = mg.index.CatchUp(ctx, catchUpPages) // Transient errors show in the index status.
		m.win.With(mg.index.Sync)
	}
	gen, _, _, _ := m.win.Info()
	err = op(m)
	if !errors.Is(err, ErrRebuild) {
		return err
	}
	return mg.rebuild(ctx, m, gen)
}

// rebuild runs one rebuild at a time per window. The managed lock is never
// held while the window lock is taken.
// gen is the generation the failed operation saw; if another request has
// rebuilt since, the client only needs the new snapshot.
func (mg *Manager) rebuild(ctx context.Context, m *managed, gen uint64) error {
	current, _, _, _ := m.win.Info()
	m.mu.Lock()
	now := mg.now()
	switch {
	case current != gen && !m.rebuilding:
		m.mu.Unlock()
		return ErrRebuild
	case m.rebuilding:
		m.mu.Unlock()
		return &RebuildingError{RetryAfter: time.Second}
	case now.Before(m.nextRebuild):
		wait := m.nextRebuild.Sub(now)
		m.mu.Unlock()
		return &RebuildingError{RetryAfter: wait}
	}
	m.rebuilding = true
	m.mu.Unlock()
	m.hints.reset()
	err := m.win.Rebuild(ctx)
	m.mu.Lock()
	m.rebuilding = false
	if err == nil {
		m.failures, m.catchUp = 0, nil
		m.mu.Unlock()
		return ErrRebuild // The client must take the new generation's snapshot.
	}
	failed := m.failures >= len(rebuildBackoff)
	var wait time.Duration
	if !failed {
		wait = rebuildBackoff[m.failures]
		m.nextRebuild = mg.now().Add(wait)
		m.failures++
	}
	m.mu.Unlock()
	if failed {
		m.win.fail()
		return stateError(StateFailed)
	}
	return &RebuildingError{RetryAfter: wait}
}

// StateError reports a terminal window state.
type StateError struct{ State State }

func (e *StateError) Error() string { return "dashboard: window " + string(e.State) }

func stateError(s State) error { return &StateError{State: s} }
