//go:build linux || darwin

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStandaloneHistoryBudgetIsPersistedAndReported(t *testing.T) {
	home, binary := standaloneTestBinary(t)
	address, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, ".config", "terminal-mcp", "token")
	t.Cleanup(func() {
		_, _ = standaloneTestCommand(binary, home, "stop")
	})
	const limit int64 = 1 << 20
	out, err := standaloneTestCommand(binary, home, "start", "--listen="+address,
		"--file-root="+home, "--audit-max-bytes=1048576")
	if err != nil {
		t.Fatalf("standalone could not start with a custom budget: %s %v", out, err)
	}
	client, err := newUpdateClient(address, token)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.runtime()
	if err != nil {
		t.Fatal(err)
	}
	wantAudit := filepath.Join(home, ".config", "terminal-mcp", "logs", "audit.jsonl")
	if info.Audit.State != "ok" || !sameAuditPath(info.Audit.Path, wantAudit) ||
		info.Audit.BytesLimit != limit || info.Audit.BytesUsed == 0 {
		t.Fatalf("live runtime omitted storage budget or startup event: %+v", info.Audit)
	}
	diagnostics := filepath.Dir(wantAudit)
	if info.StdoutLog != filepath.Join(diagnostics, "server.stdout.log") ||
		info.StderrLog != filepath.Join(diagnostics, "server.stderr.log") {
		t.Fatalf("runtime did not identify redirected process descriptors: stdout=%q stderr=%q",
			info.StdoutLog, info.StderrLog)
	}
	statusOutput, err := standaloneTestCommand(binary, home, "status")
	if err != nil {
		t.Fatalf("standalone status failed: %v %s", err, statusOutput)
	}
	var status struct {
		Audit struct {
			BytesLimit int64 `json:"bytes_limit"`
		} `json:"audit"`
		StderrLog string `json:"stderr_log"`
	}
	if err := json.Unmarshal(statusOutput, &status); err != nil ||
		status.Audit.BytesLimit != limit || status.StderrLog != info.StderrLog {
		t.Fatalf("CLI status did not report retention and diagnostics: %+v %v", status, err)
	}
	saved, err := savedStandaloneConfig(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	if err != nil || saved.AuditMaxBytes != limit {
		t.Fatalf("standalone did not persist budget: %+v %v", saved, err)
	}
	found := false
	for _, arg := range info.Arguments {
		if arg == "--audit-max-bytes=1048576" {
			found = true
		}
	}
	if !found {
		t.Fatalf("serve did not receive explicit storage limit: %v", info.Arguments)
	}
	output, err := standaloneTestCommand(binary, home, "stop")
	if err != nil {
		t.Fatalf("could not stop standalone test process: %s %v", output, err)
	}
	infoDisk, err := os.Stat(wantAudit)
	if err != nil || infoDisk.Size() == 0 {
		t.Fatalf("audit journal not retained on normal shutdown: %+v %v", infoDisk, err)
	}
}
