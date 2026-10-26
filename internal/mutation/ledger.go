package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	done        chan struct{}
	receipt     Receipt
	startErr    error // No effect was executed; this key must not be reserved.
}

type Ledger struct {
	mu       sync.Mutex
	entries  map[string]*operation
	inFlight int
	frozen   bool
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

func (l *Ledger) Do(ctx context.Context, limit int, id, method string, effect any, fn func() Receipt) (Receipt, bool, error) {
	if !ValidID(id) {
		return Receipt{}, false, fmt.Errorf("operation_id must be 1..128 ASCII letters, digits, '.', ':', '_' or '-'")
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, false, err
	}
	b, err := json.Marshal(effect)
	if err != nil {
		return Receipt{}, false, err
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
			return Receipt{}, true, fmt.Errorf("operation_id_conflict: the key was already used for different effect arguments")
		}
	} else {
		if l.frozen {
			l.mu.Unlock()
			return Receipt{}, false, fmt.Errorf("server_draining: new mutations are paused for update")
		}
		if len(l.entries) >= limit {
			l.mu.Unlock()
			return Receipt{}, false, fmt.Errorf("operation_ledger_full: new mutations refused; read/list still work; close locally and restart when safe")
		}
		op = &operation{fingerprint: hash, done: make(chan struct{})}
		l.entries[id] = op
		l.inFlight++
		l.active.Add(1) // Admission and shutdown fencing are both under mu.
		go func() {
			// Completion (and audit notification) precedes the ledger receipt
			// becoming available to the original caller or any replayer.
			defer func() {
				l.mu.Lock()
				if op.startErr != nil {
					// A rejected pre-effect audit must not consume a retry key.
					// Replayers already waiting on op.done receive startErr.
					delete(l.entries, id)
				}
				l.inFlight--
				close(op.done)
				l.mu.Unlock()
				l.active.Done()
			}()
			if observer != nil {
				// Even a broken observer cannot cause an unaudited mutation.
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
					return
				}
			}
			func() {
				defer func() {
					if recover() != nil {
						op.receipt = Receipt{OperationID: id, Status: "unknown", Error: "internal failure; outcome may be unknown; do not retry with a new key"}
					}
				}()
				op.receipt = fn()
			}()
			op.receipt.OperationID = id
			if len(op.receipt.Error) > 1024 {
				op.receipt.Error = strings.ToValidUTF8(op.receipt.Error[:1024], "�") + " [truncated]"
			}
			if observer != nil {
				func() {
					defer func() {
						if p := recover(); p != nil {
							// The effect has already run. A broken completion
							// observer must not crash the whole MCP server or
							// overwrite the stored, replayable receipt.
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

func (l *Ledger) Count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.entries) }

// Freeze prevents a restart racing an accepted mutation. Existing receipts remain readable.
func (l *Ledger) Freeze() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight != 0 {
		return fmt.Errorf("mutations_in_flight")
	}
	l.frozen = true
	return nil
}

func (l *Ledger) Resume()      { l.mu.Lock(); defer l.mu.Unlock(); l.frozen = false }
func (l *Ledger) Frozen() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.frozen }

// StopAndWait refuses new effects, then waits for all already-admitted
// effects and their audit callbacks. Admission increments active under mu;
// freezing while holding the same mutex makes active.Wait safe when empty.
// Unlike Freeze, this is for irreversible service shutdown.
func (l *Ledger) StopAndWait() {
	l.mu.Lock()
	l.frozen = true
	l.mu.Unlock()
	l.active.Wait()
}
