package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

type operation struct {
	fingerprint [32]byte
	done        chan struct{}
	receipt     Receipt
}

type Ledger struct {
	mu       sync.Mutex
	entries  map[string]*operation
	inFlight int
	frozen   bool
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
		go func() {
			defer func() { l.mu.Lock(); l.inFlight--; close(op.done); l.mu.Unlock() }()
			defer func() {
				if recover() != nil {
					op.receipt = Receipt{OperationID: id, Status: "unknown", Error: "internal failure; outcome may be unknown; do not retry with a new key"}
				}
			}()
			op.receipt = fn()
			op.receipt.OperationID = id
			if len(op.receipt.Error) > 1024 {
				op.receipt.Error = strings.ToValidUTF8(op.receipt.Error[:1024], "�") + " [truncated]"
			}
		}()
	}
	l.mu.Unlock()
	select {
	case <-op.done:
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
