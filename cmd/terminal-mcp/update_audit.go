//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"terminalmcp/internal/mcpserver"
)

// updateAuditPaths pins the audit journal for both activation and rollback.
// Older installed binaries lack RuntimeInfo.Audit, so we also interpret their
// original serve arguments. Always add the resulting --audit-file explicitly;
// otherwise a rollback to older code silently reverts to its old default.
func updateAuditPaths(old mcpserver.RuntimeInfo) (oldPath, target string, migrating bool, err error) {
	legacy, err := legacyAuditPath()
	if err != nil {
		return "", "", false, err
	}
	unified, err := defaultAuditPath()
	if err != nil {
		return "", "", false, err
	}
	if old.Audit.State == "disabled" {
		return "off", "off", false, nil
	}
	oldPath = old.Audit.Path
	if oldPath == "" {
		// Last flag wins, matching Go's flag parser. Older versions did not
		// expose an audit path in authenticated runtime metadata.
		for _, arg := range old.Arguments {
			if val, ok := strings.CutPrefix(arg, "--audit-file="); ok {
				oldPath = val
			}
		}
		if oldPath == "" || oldPath == "default" {
			oldPath = legacy
		}
	}
	if oldPath == "off" {
		return "off", "off", false, nil
	}
	if !filepath.IsAbs(oldPath) {
		oldPath = filepath.Join(old.WorkingDirectory, oldPath)
	}
	if sameAuditPath(oldPath, legacy) {
		return oldPath, unified, true, nil
	}
	if oldPath == "" {
		return "", "", false, fmt.Errorf("cannot determine running server's audit path")
	}
	return oldPath, oldPath, false, nil
}

// rollbackAuditPath keeps the previous journal if migration failed before its
// completion marker. If migration succeeded, both the updated server and the
// old binary must explicitly use the unified path.
func verifyRestartAudit(info mcpserver.RuntimeInfo, expected string) error {
	// A former binary may not expose Audit.State, but it still reports the
	// exact serve arguments it used. Inspect the last explicit audit flag.
	actual := info.Audit.Path
	if info.Audit.State == "disabled" {
		actual = "off"
	}
	if info.Audit.State == "" {
		for _, arg := range info.Arguments {
			if value, ok := strings.CutPrefix(arg, "--audit-file="); ok {
				actual = value
			}
		}
	}
	if actual == "" || (actual != "off" && !sameAuditPath(actual, expected)) ||
		(actual == "off" && expected != "off") {
		return fmt.Errorf("restarted server audit path %q differs from requested %q", actual, expected)
	}
	if info.Audit.State != "" && expected != "off" && info.Audit.State != "ok" {
		return fmt.Errorf("restarted server audit state is %q", info.Audit.State)
	}
	return nil
}

func rollbackAuditPath(oldPath, target string, migrating bool) string {
	if !migrating {
		return target
	}
	if _, err := os.Lstat(target + ".migrated.json"); err == nil {
		return target
	}
	return oldPath
}
