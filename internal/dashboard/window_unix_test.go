//go:build linux || darwin

package dashboard

import (
	"os"
	"path/filepath"
	"testing"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
)

// TestIdenticalArchivesAreTwoOccurrences (design A2): byte-identical
// pre-existing archives share record positions but are distinct occurrences
// of one action, which is counted once.
func TestIdenticalArchivesAreTwoOccurrences(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := audit.PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	lines := `{"schema_version":2,"time":"t1","server_id":"old","request_id":"r1","tool":"file_read","phase":"request_start","args":{"path":"a"}}` + "\n" +
		`{"schema_version":2,"time":"t2","server_id":"old","request_id":"r1","tool":"file_read","phase":"request_finish","status":"ok"}` + "\n"
	path := filepath.Join(dir, "audit.jsonl")
	for _, suffix := range []string{".1", ".2"} {
		if err := os.WriteFile(path+suffix, []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	j := openJournalAt(t, path)
	w := buildLive(t, j, WindowConfig{}, nil)
	w.With(func(p *activity.Projection) {
		actions := p.Actions()
		if len(actions) != 1 || len(actions[0].Start.Occurrences) != 2 || len(actions[0].Finish.Occurrences) != 2 ||
			actions[0].Start.Conflict || actions[0].Start.Occurrences[0] == actions[0].Start.Occurrences[1] {
			t.Fatalf("actions %+v", actions)
		}
	})
}
