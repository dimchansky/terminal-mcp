//go:build linux || darwin

package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type operation struct {
	fingerprint [32]byte
	done        chan struct{}
	receipt     Receipt
}

type ledger struct {
	mu      sync.Mutex
	entries map[string]*operation
}

func validOperationID(id string) bool {
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

func (l *ledger) do(ctx context.Context, limit int, id, method string, effect any, fn func() Receipt) (Receipt, bool, error) {
	if !validOperationID(id) {
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
		if len(l.entries) >= limit {
			l.mu.Unlock()
			return Receipt{}, false, fmt.Errorf("operation_ledger_full: new mutations refused; read/list still work; close locally and restart when safe")
		}
		op = &operation{fingerprint: hash, done: make(chan struct{})}
		l.entries[id] = op
		go func() {
			defer close(op.done)
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

func (l *ledger) count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.entries) }
