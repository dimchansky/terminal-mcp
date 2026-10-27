//go:build linux || darwin

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/mcpserver"
)

func TestUpdateAuditPathUsesOneJournalWithoutMigration(t *testing.T) {
	base := t.TempDir()
	current := filepath.Join(base, "active.jsonl")
	other := filepath.Join(base, "another.jsonl")

	for _, tc := range []struct {
		name      string
		info      mcpserver.RuntimeInfo
		want      string
		wantError string
	}{
		{"current journal from status", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "ok", Path: current},
		}, current, ""},
		{"metadata overrides old args", mcpserver.RuntimeInfo{
			Audit:     audit.Status{State: "ok", Path: current},
			Arguments: []string{"serve", "--audit-file=" + other},
		}, current, ""},
		{"modern disabled", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "disabled"},
		}, "off", ""},
		{"former binary explicit absolute", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file=" + current},
		}, current, ""},
		{"former binary explicit off", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file=off"},
		}, "off", ""},
		{"former binary split flag", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file", current},
		}, current, ""},
		{"former binary single-dash flag", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "-audit-file", current},
		}, current, ""},
		{"former binary single-dash equals", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "-audit-file=" + current},
		}, current, ""},
		{"last explicit flag wins", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file=off", "--audit-file", current},
		}, current, ""},
		{"last explicit disables", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file=" + current, "--audit-file=off"},
		}, "off", ""},
		{"former binary relative path", mcpserver.RuntimeInfo{
			WorkingDirectory: base,
			Arguments:        []string{"serve", "--audit-file=active.jsonl"},
		}, current, ""},
		{"no status and no explicit audit flag", mcpserver.RuntimeInfo{
			Arguments: []string{"serve"},
		}, "", "cannot determine"},
		{"ambiguous default flag", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file=default"},
		}, "", "cannot determine"},
		{"explicit empty flag", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file="},
		}, "", "cannot determine"},
		{"split flag without value", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file"},
		}, "", "missing value"},
		{"relative path without cwd", mcpserver.RuntimeInfo{
			Arguments: []string{"serve", "--audit-file=active.jsonl"},
		}, "", "absolute working directory"},
		{"relative path with relative cwd", mcpserver.RuntimeInfo{
			WorkingDirectory: "relative",
			Arguments:        []string{"serve", "--audit-file=active.jsonl"},
		}, "", "absolute working directory"},
		{"inconsistent modern metadata", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "ok"}, Arguments: []string{"serve", "--audit-file=" + current},
		}, "", "absolute audit path"},
		{"non-absolute published metadata", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "ok", Path: "active.jsonl"},
		}, "", "absolute audit path"},
		{"degraded cannot update", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "degraded", Path: current},
		}, "", "degraded"},
		{"unknown state must not be guessed", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "other", Path: current},
		}, "", "unknown state"},
		{"disabled with conflicting path", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "disabled", Path: current},
		}, "", "disabled audit unexpectedly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := updateAuditPath(tc.info)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("updateAuditPath(%+v) error = %v, want %q", tc.info, err, tc.wantError)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("updateAuditPath(%+v) = %q, %v; want %q", tc.info, got, err, tc.want)
			}
		})
	}
}

// Reject missing or unsafe audit metadata before copying a candidate,
// fencing admissions, stopping the old process, or touching its journal.
func TestUpdaterRejectsUnidentifiedAuditBeforeDrain(t *testing.T) {
	for _, tc := range []struct {
		name string
		info mcpserver.RuntimeInfo
	}{
		{"missing audit metadata", mcpserver.RuntimeInfo{Arguments: []string{"serve"}}},
		{"degraded audit", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "degraded", Path: filepath.Join(t.TempDir(), "audit.jsonl")},
		}},
		{"incomplete metadata", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "ok"}, Arguments: []string{"serve", "--audit-file=off"},
		}},
		{"unknown state", mcpserver.RuntimeInfo{
			Audit: audit.Status{State: "invalid"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := performUpdate(updateJob{Runtime: tc.info}, func(stage string) error {
				t.Fatalf("unsafe updater entered stage %s", stage)
				return nil
			})
			if status.Status != "failed" || err == nil || !strings.Contains(err.Error(), "resolve update audit path") {
				t.Fatalf("updater accepted an ambiguous/degraded audit: %+v, %v", status, err)
			}
		})
	}
}

func TestVerifyRestartAuditPinsJournalAndHealth(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "audit.jsonl")
	other := filepath.Join(root, "elsewhere.jsonl")
	for _, tc := range []struct {
		name   string
		status mcpserver.RuntimeInfo
		want   string
		fail   bool
	}{
		{"healthy current", mcpserver.RuntimeInfo{Audit: audit.Status{State: "ok", Path: journal}}, journal, false},
		{"disabled", mcpserver.RuntimeInfo{Audit: audit.Status{State: "disabled"}}, "off", false},
		{"old release verified by explicit flag", mcpserver.RuntimeInfo{Arguments: []string{"serve", "--audit-file=" + journal}}, journal, false},
		{"old release disabled", mcpserver.RuntimeInfo{Arguments: []string{"serve", "--audit-file=off"}}, "off", false},
		{"wrong journal", mcpserver.RuntimeInfo{Audit: audit.Status{State: "ok", Path: other}}, journal, true},
		{"unexpected disabled", mcpserver.RuntimeInfo{Audit: audit.Status{State: "disabled"}}, journal, true},
		{"unexpected enabled", mcpserver.RuntimeInfo{Audit: audit.Status{State: "ok", Path: journal}}, "off", true},
		{"degraded writer", mcpserver.RuntimeInfo{Audit: audit.Status{State: "degraded", Path: journal}}, journal, true},
		{"missing metadata and flag", mcpserver.RuntimeInfo{Arguments: []string{"serve"}}, journal, true},
		{"old release missing flag while expected disabled", mcpserver.RuntimeInfo{Arguments: []string{"serve"}}, "off", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyRestartAudit(tc.status, tc.want)
			if (err != nil) != tc.fail {
				t.Fatalf("verifyRestartAudit(%+v, %q): %v; fail=%t", tc.status, tc.want, err, tc.fail)
			}
		})
	}
}
