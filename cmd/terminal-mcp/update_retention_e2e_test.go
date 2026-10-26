//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUpdaterKeepsCustomAuditHistoryBudget(t *testing.T) {
	const budget int64 = 2 << 20
	var auditFile string
	f := newUpdateFixtureWithAudit(t, false, func(home string) string {
		auditFile = filepath.Join(home, "audit", "audit.jsonl")
		if err := os.MkdirAll(filepath.Dir(auditFile), 0700); err != nil {
			t.Fatal(err)
		}
		return auditFile
	}, "--audit-max-bytes=2097152")
	old, err := f.client.runtime()
	if err != nil || old.Audit.BytesLimit != budget {
		t.Fatalf("initial server did not apply budget: %+v err=%v", old.Audit, err)
	}
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-buildid=budget-update", "-o", f.candidate, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build different candidate: %v\n%s", err, output)
	}
	f.update(f.candidate)
	status := f.waitResult()
	if status.Status != "applied" {
		t.Fatalf("custom budget blocked safe update: %+v", status)
	}
	updated, err := f.client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServerID == old.ServerID || updated.Audit.BytesLimit != budget ||
		updated.Audit.State != "ok" || !sameAuditPath(updated.Audit.Path, auditFile) {
		t.Fatalf("update failed to preserve audit history budget: %+v", updated)
	}
}
