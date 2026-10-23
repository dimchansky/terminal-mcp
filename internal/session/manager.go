//go:build linux || darwin

package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

type Manager struct {
	mu              sync.Mutex
	id              string
	limits          Limits
	sessions        map[string]*Session
	starting        int
	starts          sync.WaitGroup
	closed          bool
	closeOnce       sync.Once
	closeDone       chan struct{}
	ops             ledger
	startPTY        PTYStarter
	drainGeneration uint64
}

func newID() string {
	var p [16]byte
	if _, err := rand.Read(p[:]); err != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(p[:])
}

func New(startPTY PTYStarter) *Manager {
	return &Manager{id: newID(), limits: defaultLimits(), sessions: make(map[string]*Session), startPTY: startPTY, closeDone: make(chan struct{})}
}

func (m *Manager) checkServer(id string) error {
	if id != m.id {
		return fmt.Errorf("server_instance_changed: obtain terminal_list, investigate the old outcome, and do not blindly resubmit the old action")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("server is shutting down")
	}
	return nil
}

// pruneLocked keeps only completed sessions eligible for eviction.
func (m *Manager) pruneLocked() {
	type expired struct {
		id string
		at time.Time
	}
	var completed []expired
	for id, s := range m.sessions {
		s.mu.Lock()
		if s.exit != nil && s.outputDone {
			completed = append(completed, expired{id, s.exitedAt})
		}
		s.mu.Unlock()
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].at.Before(completed[j].at) })
	now := time.Now()
	for i, item := range completed {
		if len(completed)-i > m.limits.RetainedSessions || now.Sub(item.at) > time.Duration(m.limits.RetentionSeconds)*time.Second {
			delete(m.sessions, item.id)
		}
	}
}

func validateSessionID(id string) error {
	if len(id) != 32 {
		return fmt.Errorf("invalid session_id")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return fmt.Errorf("invalid session_id")
	}
	return nil
}

func (m *Manager) get(id string) (*Session, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.pruneLocked()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("session_not_found_or_expired")
	}
	return s, nil
}

func (m *Manager) List(context.Context) (ListResult, error) {
	m.mu.Lock()
	m.pruneLocked()
	out := ListResult{ServerID: m.id, Limits: m.limits, Sessions: make([]Summary, 0, len(m.sessions)), Modes: []string{"pipes"}, Keys: supportedKeys()}
	if m.startPTY != nil {
		out.Modes = append(out.Modes, "pty")
	}
	for _, s := range m.sessions {
		s.mu.Lock()
		state := "running"
		if s.exit != nil {
			state = "exited"
		}
		out.Sessions = append(out.Sessions, Summary{SessionID: s.id, Mode: s.mode, PID: s.cmd.Process.Pid, ProcessState: state, Closed: s.closed, OutputDone: s.outputDone})
		s.mu.Unlock()
	}
	m.mu.Unlock()
	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].SessionID < out.Sessions[j].SessionID })
	out.OperationsUsed = m.ops.count()
	out.Draining = m.ops.raw.Frozen()
	return out, nil
}

func (m *Manager) Start(ctx context.Context, args StartArgs) (ActionResult, error) {
	if err := m.checkServer(args.ServerID); err != nil {
		return ActionResult{}, err
	}
	a, err := m.normalizeStart(args)
	if err != nil {
		return ActionResult{}, err
	}
	effect := a
	effect.ServerID, effect.OperationID, effect.WaitMS, effect.MaxBytes, effect.WaitFor = "", "", 0, 0, ""
	receipt, replayed, err := m.ops.do(ctx, m.limits.MaxOperations, a.OperationID, "start", effect, func() Receipt {
		m.mu.Lock()
		m.pruneLocked()
		active := m.starting
		for _, s := range m.sessions {
			s.mu.Lock()
			if s.exit == nil || !s.outputDone {
				active++
			}
			s.mu.Unlock()
		}
		if m.closed || active >= m.limits.ActiveSessions {
			m.mu.Unlock()
			return Receipt{Status: "failed", Error: "server shutting down or active session limit reached"}
		}
		m.starting++
		m.starts.Add(1)
		m.mu.Unlock()
		defer m.starts.Done()
		s, spawnErr := spawn(a, m.limits, m.startPTY)
		m.mu.Lock()
		m.starting--
		if s != nil {
			m.sessions[s.id] = s
		}
		m.mu.Unlock()
		if spawnErr != nil {
			return Receipt{Status: "failed", Error: spawnErr.Error()}
		}
		return Receipt{SessionID: s.id, Status: "applied", BeforeCursor: makeCursor(s.id, 0)}
	})
	if err != nil {
		return ActionResult{}, err
	}
	return m.observeAction(ctx, receipt, replayed, "", a.WaitMS, a.MaxBytes, a.WaitFor), nil
}

func (m *Manager) Read(ctx context.Context, args ReadArgs) (Observation, error) {
	a, err := m.readOptions(args)
	if err != nil {
		return Observation{}, err
	}
	s, err := m.get(a.SessionID)
	if err != nil {
		return Observation{}, err
	}
	return s.read(ctx, a)
}

func (m *Manager) observeAction(ctx context.Context, receipt Receipt, replayed bool, cursor string, waitMS, maxBytes int, waitFor ...string) ActionResult {
	result := ActionResult{Receipt: receipt, Replayed: replayed}
	if receipt.SessionID == "" {
		return result
	}
	if cursor == "" {
		cursor = receipt.BeforeCursor
	}
	wait := ""
	if len(waitFor) != 0 {
		wait = waitFor[0]
	}
	obs, err := m.Read(ctx, ReadArgs{SessionID: receipt.SessionID, Cursor: cursor, WaitMS: waitMS, MaxBytes: maxBytes, WaitFor: wait})
	if err != nil {
		result.ObservationError = err.Error()
	} else {
		result.Observation = &obs
	}
	return result
}

func (m *Manager) Write(ctx context.Context, a WriteArgs) (ActionResult, error) {
	if err := m.checkServer(a.ServerID); err != nil {
		return ActionResult{}, err
	}
	if err := validateSessionID(a.SessionID); err != nil {
		return ActionResult{}, err
	}
	if _, err := m.readOptions(ReadArgs{WaitMS: a.WaitMS, MaxBytes: a.MaxBytes, Cursor: a.Cursor}); err != nil {
		return ActionResult{}, err
	}
	effect := a
	effect.ServerID, effect.OperationID, effect.Cursor, effect.WaitMS, effect.MaxBytes = "", "", "", 0, 0
	r, replayed, err := m.ops.do(ctx, m.limits.MaxOperations, a.OperationID, "write", effect, func() Receipt {
		s, err := m.get(a.SessionID)
		if err != nil {
			return Receipt{SessionID: a.SessionID, Status: "failed", Error: err.Error()}
		}
		if err := m.validateInput(a, s.terminal != nil); err != nil {
			return Receipt{SessionID: a.SessionID, Status: "failed", Error: err.Error()}
		}
		if err := s.validateCursor(a.Cursor); err != nil {
			return Receipt{SessionID: a.SessionID, Status: "failed", Error: err.Error()}
		}
		return s.write(a.Input, a.CloseStdin)
	})
	if err != nil {
		return ActionResult{}, err
	}
	return m.observeAction(ctx, r, replayed, a.Cursor, a.WaitMS, a.MaxBytes), nil
}

func (m *Manager) Signal(ctx context.Context, a SignalArgs) (ActionResult, error) {
	if err := m.checkServer(a.ServerID); err != nil {
		return ActionResult{}, err
	}
	if err := validateSessionID(a.SessionID); err != nil {
		return ActionResult{}, err
	}
	if a.Target == "" {
		a.Target = "group"
	}
	if _, ok := signals[a.Signal]; !ok {
		return ActionResult{}, fmt.Errorf("unsupported signal")
	}
	if a.Target != "process" && a.Target != "group" && a.Target != "foreground" {
		return ActionResult{}, fmt.Errorf("target must be process, group or foreground")
	}
	effect := a
	effect.ServerID, effect.OperationID = "", ""
	r, replayed, err := m.ops.do(ctx, m.limits.MaxOperations, a.OperationID, "signal", effect, func() Receipt {
		s, err := m.get(a.SessionID)
		if err != nil {
			return Receipt{SessionID: a.SessionID, Status: "failed", Error: err.Error()}
		}
		r := s.receipt()
		if a.Target == "foreground" && s.terminal == nil {
			r.Status, r.Error = "failed", "foreground target requires PTY"
			return r
		}
		if err := s.signal(signals[a.Signal], a.Target); err != nil {
			r.Status, r.Error = "failed", err.Error()
		}
		return r
	})
	if err != nil {
		return ActionResult{}, err
	}
	return m.observeAction(ctx, r, replayed, "", 0, 0), nil
}

func (m *Manager) Resize(ctx context.Context, a ResizeArgs) (ActionResult, error) {
	if err := m.checkServer(a.ServerID); err != nil {
		return ActionResult{}, err
	}
	if err := validateSessionID(a.SessionID); err != nil {
		return ActionResult{}, err
	}
	if err := m.dimensions(a.Rows, a.Cols); err != nil {
		return ActionResult{}, err
	}
	effect := a
	effect.ServerID, effect.OperationID = "", ""
	r, replayed, err := m.ops.do(ctx, m.limits.MaxOperations, a.OperationID, "resize", effect, func() Receipt {
		s, err := m.get(a.SessionID)
		if err != nil {
			return Receipt{SessionID: a.SessionID, Status: "failed", Error: err.Error()}
		}
		r := s.receipt()
		if s.terminal == nil {
			r.Status, r.Error = "failed", "resize requires PTY"
			return r
		}
		s.mu.Lock()
		unavailable := s.closed || s.exit != nil
		s.mu.Unlock()
		if unavailable {
			r.Status, r.Error = "failed", "session has ended"
			return r
		}
		if err := s.terminal.Resize(a.Rows, a.Cols); err != nil {
			r.Status, r.Error = "failed", err.Error()
			return r
		}
		s.mu.Lock()
		s.revision++
		s.notifyLocked()
		s.mu.Unlock()
		return r
	})
	if err != nil {
		return ActionResult{}, err
	}
	return m.observeAction(ctx, r, replayed, "", 0, 0), nil
}

func (m *Manager) CloseSession(ctx context.Context, a CloseArgs) (ActionResult, error) {
	if err := m.checkServer(a.ServerID); err != nil {
		return ActionResult{}, err
	}
	if err := validateSessionID(a.SessionID); err != nil {
		return ActionResult{}, err
	}
	if a.GraceMS < 0 || a.GraceMS > 2000 {
		return ActionResult{}, fmt.Errorf("grace_ms must be 0..2000")
	}
	effect := a
	effect.ServerID, effect.OperationID = "", ""
	r, replayed, err := m.ops.do(ctx, m.limits.MaxOperations, a.OperationID, "close", effect, func() Receipt {
		s, err := m.get(a.SessionID)
		if err != nil {
			return Receipt{SessionID: a.SessionID, Status: "failed", Error: err.Error()}
		}
		r := s.receipt()
		s.close(time.Duration(a.GraceMS) * time.Millisecond)
		s.mu.Lock()
		if s.exit == nil {
			r.Status, r.Error = "unknown", "termination requested but process exit was not observed within the deadline"
		}
		s.mu.Unlock()
		return r
	})
	if err != nil {
		return ActionResult{}, err
	}
	return m.observeAction(ctx, r, replayed, "", 0, 0), nil
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.starts.Wait()
		m.mu.Lock()
		ss := make([]*Session, 0, len(m.sessions))
		for _, s := range m.sessions {
			ss = append(ss, s)
		}
		m.mu.Unlock()
		var wg sync.WaitGroup
		for _, s := range ss {
			wg.Add(1)
			go func(s *Session) { defer wg.Done(); s.close(200 * time.Millisecond) }(s)
		}
		wg.Wait()
		close(m.closeDone)
	})
	<-m.closeDone
}

// PrepareRestart atomically excludes new mutations after checking managed jobs are finished.
func (m *Manager) PrepareRestart(id string) error {
	if err := m.checkServer(id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.starting != 0 {
		return fmt.Errorf("active_sessions: process launch in progress")
	}
	for _, s := range m.sessions {
		s.mu.Lock()
		active := s.exit == nil || !s.outputDone
		s.mu.Unlock()
		if active {
			return fmt.Errorf("active_sessions: update requires all managed processes to finish")
		}
	}
	if err := m.ops.raw.Freeze(); err != nil {
		return err
	}
	m.drainGeneration++
	generation := m.drainGeneration
	// A dead updater must not leave a healthy server permanently read-only.
	time.AfterFunc(time.Minute, func() { m.expireDrain(generation) })
	return nil
}
func (m *Manager) CancelRestart(id string) error {
	if err := m.checkServer(id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drainGeneration++
	m.ops.raw.Resume()
	return nil
}
func (m *Manager) expireDrain(generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drainGeneration == generation {
		m.ops.raw.Resume()
	}
}
