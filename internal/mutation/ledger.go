package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Receipt retains only bounded outcome metadata, not request payloads.
type Receipt struct {
	OperationID string          `json:"operation_id"`
	Status      string          `json:"status"`
	Error       string          `json:"error,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

// Observer receives the lifecycle of one newly admitted effect. Started
// can veto execution if its pre-effect audit cannot be recorded. Callbacks
// run outside the ledger mutex; replays do not generate another lifecycle.
type Observer interface {
	Started(operationID string) error
	Finished(receipt Receipt)
}

type observerKey struct{}

// WithObserver attaches per-request effect observation without coupling the
// retry ledger to any particular audit or logging implementation.
func WithObserver(ctx context.Context, observer Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, observer)
}

type operation struct {
	fingerprint [32]byte
	method      string
	done        chan struct{}
	started     chan struct{}
	receipt     Receipt
	startErr    error // No effect was executed; this key must not be reserved.
	progress    Progress
	startedAt   time.Time
	completedAt time.Time
}

type Ledger struct {
	mu       sync.Mutex
	entries  map[string]*operation
	inFlight int
	draining bool           // Reject new work; allow controls of existing terminal sessions.
	frozen   bool           // Final fence: no more mutations of any kind.
	active   sync.WaitGroup // Admitted effects, including their audit callbacks.
}

func ValidID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

type Progress struct {
	Stage           string `json:"stage,omitempty"`
	Path            string `json:"path,omitempty"`
	BytesDownloaded int64  `json:"bytes_downloaded"`
	TotalBytes      *int64 `json:"total_bytes"`
}

type Snapshot struct {
	OperationID string
	Status      string
	Progress    Progress
	Receipt     *Receipt
	StartedAt   time.Time
	CompletedAt *time.Time
}

func (l *Ledger) Do(ctx context.Context, limit int, id, method string, effect any, fn func() Receipt) (Receipt, bool, error) {
	op, replayed, err := l.admit(ctx, limit, id, method, effect, func(_ func(Progress)) Receipt { return fn() })
	if err != nil {
		return Receipt{}, replayed, err
	}
	select {
	case <-op.done:
		if op.startErr != nil {
			return Receipt{}, replayed, op.startErr
		}
		return op.receipt, replayed, nil
	case <-ctx.Done():
		return Receipt{}, replayed, ctx.Err()
	}
}

// Start begins an independent effect but only returns after the pre-effect
// audit start has been written successfully. It never waits for the effect.
func (l *Ledger) Start(ctx context.Context, limit int, id, method string, effect any, fn func(func(Progress)) Receipt) (Snapshot, bool, error) {
	op, replayed, err := l.admit(ctx, limit, id, method, effect, fn)
	if err != nil {
		return Snapshot{}, replayed, err
	}
	select {
	case <-op.started:
		if op.startErr != nil {
			return Snapshot{}, replayed, op.startErr
		}
		out, err := l.Snapshot(id, method)
		return out, replayed, err
	case <-ctx.Done():
		return Snapshot{}, replayed, ctx.Err()
	}
}

// WaitSnapshot waits for an operation's final receipt up to the observation
// deadline. The effect is independent of the requesting context and timeout.
func (l *Ledger) WaitSnapshot(ctx context.Context, id, method string, wait time.Duration) (Snapshot, error) {
	if wait <= 0 {
		return l.Snapshot(id, method)
	}
	l.mu.Lock()
	op := l.entries[id]
	if op == nil || op.method != method {
		l.mu.Unlock()
		return Snapshot{}, fmt.Errorf("operation_not_found: no %s operation with this ID in the current server instance", method)
	}
	done := op.done
	l.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	return l.Snapshot(id, method)
}

func (l *Ledger) Snapshot(id, method string) (Snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op := l.entries[id]
	if op == nil || op.method != method {
		return Snapshot{}, fmt.Errorf("operation_not_found: no %s operation with this ID in the current server instance", method)
	}
	// The observer sets startErr before closing started. Read it only after
	// that close: status polling can race a slow pre-effect audit callback.
	select {
	case <-op.started:
		if op.startErr != nil {
			return Snapshot{}, fmt.Errorf("operation_not_found: %s was not admitted", id)
		}
	default:
	}
	out := Snapshot{OperationID: id, Status: "running", Progress: op.progress, StartedAt: op.startedAt}
	select {
	case <-op.done:
		receipt := op.receipt
		out.Receipt = &receipt
		completed := op.completedAt
		out.CompletedAt = &completed
		switch receipt.Status {
		case "applied":
			out.Status = "completed"
			out.Progress.Stage = "completed"
		case "failed":
			out.Status = "failed"
			out.Progress.Stage = "failed"
		default:
			out.Status = "unknown"
			out.Progress.Stage = "unknown"
		}
	default:
	}
	return out, nil
}

func (l *Ledger) admit(ctx context.Context, limit int, id, method string, effect any, fn func(func(Progress)) Receipt) (*operation, bool, error) {
	if !ValidID(id) {
		return nil, false, fmt.Errorf("operation_id must be 1..128 ASCII letters, digits, '.', ':', '_' or '-'")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	b, err := json.Marshal(effect)
	if err != nil {
		return nil, false, err
	}
	hash := sha256.Sum256(append([]byte(method+"\x00"), b...))
	observer, _ := ctx.Value(observerKey{}).(Observer)
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*operation)
	}
	op, replayed := l.entries[id]
	if replayed {
		if op.fingerprint != hash {
			l.mu.Unlock()
			return nil, true, fmt.Errorf("operation_id_conflict: the key was already used for different effect arguments")
		}
	} else {
		if l.frozen || (l.draining && method != "write" && method != "signal" && method != "close") {
			l.mu.Unlock()
			return nil, false, fmt.Errorf("server_draining: new work is refused during shutdown or update")
		}
		if len(l.entries) >= limit {
			l.mu.Unlock()
			return nil, false, fmt.Errorf("operation_ledger_full: new mutations refused; read/list still work; close locally and restart when safe")
		}
		op = &operation{
			fingerprint: hash, method: method,
			done: make(chan struct{}), started: make(chan struct{}),
			progress: Progress{Stage: "queued"}, startedAt: time.Now().UTC(),
		}
		l.entries[id] = op
		l.inFlight++
		l.active.Add(1)
		go func() {
			defer func() {
				l.mu.Lock()
				if op.startErr != nil {
					delete(l.entries, id)
				}
				op.completedAt = time.Now().UTC()
				l.inFlight--
				close(op.done)
				l.mu.Unlock()
				l.active.Done()
			}()
			if observer != nil {
				startErr := func() (err error) {
					defer func() {
						if p := recover(); p != nil {
							err = fmt.Errorf("pre-effect observer panicked: %v", p)
						}
					}()
					return observer.Started(id)
				}()
				if startErr != nil {
					op.startErr = fmt.Errorf("operation_not_executed: pre-effect audit failed: %w", startErr)
					close(op.started)
					return
				}
			}
			close(op.started)
			report := func(p Progress) {
				l.mu.Lock()
				op.progress = p
				l.mu.Unlock()
			}
			func() {
				defer func() {
					if recover() != nil {
						op.receipt = Receipt{OperationID: id, Status: "unknown", Error: "internal failure; outcome may be unknown; do not retry with a new key"}
					}
				}()
				op.receipt = fn(report)
			}()
			op.receipt.OperationID = id
			if len(op.receipt.Error) > 1024 {
				op.receipt.Error = strings.ToValidUTF8(op.receipt.Error[:1024], "�") + " [truncated]"
			}
			if observer != nil {
				func() {
					defer func() {
						if p := recover(); p != nil {
							slog.Error("mutation completion observer panicked; outcome remains recorded in ledger",
								"operation_id", id, "panic", p)
						}
					}()
					observer.Finished(op.receipt)
				}()
			}
		}()
	}
	l.mu.Unlock()
	return op, replayed, nil
}

func (l *Ledger) Count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.entries) }

// Freeze prevents a restart racing an accepted mutation. Existing receipts remain readable.
func (l *Ledger) Freeze() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight != 0 {
		return fmt.Errorf("mutations_in_flight")
	}
	l.draining = true
	l.frozen = true
	return nil
}

// BeginDrain atomically closes admission for new work BEFORE waiting for
// already admitted operations. Existing-session controls remain available.
func (l *Ledger) BeginDrain() {
	l.mu.Lock()
	l.draining = true
	l.mu.Unlock()
}

func (l *Ledger) Resume() {
	l.mu.Lock()
	l.draining, l.frozen = false, false
	l.mu.Unlock()
}
func (l *Ledger) Frozen() bool   { l.mu.Lock(); defer l.mu.Unlock(); return l.frozen }
func (l *Ledger) Draining() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.draining }
func (l *Ledger) InFlight() int  { l.mu.Lock(); defer l.mu.Unlock(); return l.inFlight }

// StopAndWait refuses new effects, then waits for all already-admitted
// effects and their audit callbacks. Admission increments active under mu;
// freezing while holding the same mutex makes active.Wait safe when empty.
// Unlike Freeze, this is for irreversible service shutdown.
func (l *Ledger) StopAndWait() {
	l.mu.Lock()
	l.draining, l.frozen = true, true
	l.mu.Unlock()
	l.active.Wait()
}
