//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"terminalmcp/internal/audit"
)

// Candidate records server_start in the same audit but fails before it can
// serve the production address. Rollback must retain the same audited history.
func TestUpdaterRollbackAfterCandidateAuditStart(t *testing.T) {
	var journal string
	f := newUpdateFixtureWithAudit(t, false, func(home string) string {
		journal = currentAuditInTestHome(home)
		return journal
	})
	oldRecord, err := os.ReadFile(journal)
	if err != nil || !bytes.Contains(oldRecord, []byte("server_start")) {
		t.Fatalf("missing initial audit: %q %v", oldRecord, err)
	}

	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	marker := "if len(args) == 0 || args[0] != \"serve\" {"
	if !strings.Contains(string(source), marker) {
		t.Fatal("unable to instrument live-port failure after audit startup")
	}
	// The candidate passes preflight on its ephemeral port. Only the live
	// address fails after the candidate's server_start event.
	injected := fmt.Sprintf("if opts.listen == %q { return errors.New(\"injected after-audit launch failure\") }\n\t", f.address)
	modified := strings.Replace(string(source), marker, injected+marker, 1)
	overlaySource := filepath.Join(f.home, "candidate-main.go")
	if err := os.WriteFile(overlaySource, []byte(modified), 0600); err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs("main.go")
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(f.home, "overlay-audit.json")
	if err := writeJSON(overlay, map[string]any{"Replace": map[string]string{absolute: overlaySource}}); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-overlay="+overlay, "-o", f.candidate, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build injected candidate: %v\n%s", err, output)
	}
	f.update(f.candidate)
	result := f.waitResult()
	if result.Status != "rolled_back" || !strings.Contains(result.Error, "readiness") {
		t.Fatalf("expected rollback after candidate startup failure: %+v", result)
	}
	recovered, err := f.client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	target := journal
	if recovered.ServerID == f.oldID || recovered.Audit.State != "ok" ||
		!sameAuditPath(recovered.Audit.Path, target) {
		t.Fatalf("rollback changed the selected audit path or did not restart: %+v", recovered)
	}
	if current, err := fileSHA256(f.target); err != nil || current != recovered.ExecutableSHA256 {
		t.Fatalf("rollback did not restore running binary: %s %v", current, err)
	}
	if _, err := os.Stat(target + ".migrated.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback created a migration marker: %v", err)
	}
	logged, err := os.ReadFile(target)
	if err != nil || !bytes.HasPrefix(logged, oldRecord) ||
		bytes.Count(logged, []byte("\"phase\":\"server_start\"")) < 3 {
		t.Fatalf("rollback did not keep old/candidate/restarted server history: %s %v", logged, err)
	}
	if writer, err := audit.Open(journal); !errors.Is(err, audit.ErrInUse) {
		if err == nil {
			_ = writer.Close()
		}
		t.Fatalf("rollback did not retain the active journal lock: %v", err)
	}
	var latest struct {
		ID string `json:"id"`
	}
	if err := readJSON(filepath.Join(f.state, "latest.json"), &latest); err != nil {
		t.Fatal(err)
	}
	for _, legacyLog := range []string{"server.log", "rollback.log"} {
		if _, err := os.Stat(filepath.Join(f.state, latest.ID, legacyLog)); !os.IsNotExist(err) {
			t.Fatalf("rollback wrote diagnostics into job's %q: %v", legacyLog, err)
		}
	}
}
