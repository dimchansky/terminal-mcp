package session

import (
	"context"
	"encoding/json"
	"terminalmcp/internal/mutation"
)

type ledger struct{ raw mutation.Ledger }

func validOperationID(id string) bool { return mutation.ValidID(id) }
func (l *ledger) count() int          { return l.raw.Count() }
func (l *ledger) do(ctx context.Context, limit int, id, method string, effect any, fn func() Receipt) (Receipt, bool, error) {
	r, replayed, err := l.raw.Do(ctx, limit, id, method, effect, func() mutation.Receipt {
		out := fn()
		// Status and errors are stored once by the shared retry ledger.
		status, message := out.Status, out.Error
		out.Status, out.Error = "", ""
		data, err := json.Marshal(out)
		if err != nil {
			panic(err)
		}
		return mutation.Receipt{Status: status, Error: message, Data: data}
	})
	out := Receipt{}
	if len(r.Data) > 0 {
		if e := json.Unmarshal(r.Data, &out); e != nil {
			return out, replayed, e
		}
	}
	out.OperationID, out.Status, out.Error = r.OperationID, r.Status, r.Error
	return out, replayed, err
}

// Mutate shares retry identity, capacity and server-instance fencing with terminal actions.
func (m *Manager) Mutate(ctx context.Context, serverID, id, method string, effect any, fn func() mutation.Receipt) (mutation.Receipt, bool, error) {
	if err := m.checkServer(serverID); err != nil {
		return mutation.Receipt{}, false, err
	}
	return m.ops.raw.Do(ctx, m.limits.MaxOperations, id, method, effect, fn)
}

// StartMutation admits and audits independent work, then immediately returns
// its current snapshot instead of waiting for completion.
func (m *Manager) StartMutation(ctx context.Context, serverID, id, method string, effect any, fn func(func(mutation.Progress)) mutation.Receipt) (mutation.Snapshot, bool, error) {
	if err := m.checkServer(serverID); err != nil {
		return mutation.Snapshot{}, false, err
	}
	return m.ops.raw.Start(ctx, m.limits.MaxOperations, id, method, effect, fn)
}

func (m *Manager) MutationSnapshot(serverID, id, method string) (mutation.Snapshot, error) {
	if err := m.checkServer(serverID); err != nil {
		return mutation.Snapshot{}, err
	}
	return m.ops.raw.Snapshot(id, method)
}
