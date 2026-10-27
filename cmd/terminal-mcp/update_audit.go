//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"terminalmcp/internal/mcpserver"
)

// updateAuditPath selects one journal for both activation and rollback.
// The running server's authenticated audit status is authoritative. Older
// versions without audit status must publish an explicit --audit-file flag;
// never guess a default path or migrate an old journal during an update.
func updateAuditPath(info mcpserver.RuntimeInfo) (string, error) {
	switch info.Audit.State {
	case "ok":
		if info.Audit.Path == "" || !filepath.IsAbs(info.Audit.Path) {
			return "", errors.New("running server does not report an absolute audit path")
		}
		return filepath.Clean(info.Audit.Path), nil
	case "disabled":
		if info.Audit.Path != "" {
			return "", errors.New("disabled audit unexpectedly reports a journal path")
		}
		return "off", nil
	case "":
		// Former versions have no audit health metadata. A last explicit
		// audit flag is the only safe source of the actual journal location.
		value, found, err := lastAuditFileArgument(info.Arguments)
		if err != nil {
			return "", err
		}
		if !found {
			return "", errors.New("cannot determine running server audit path: no audit status or explicit --audit-file")
		}
		if value == "" || value == "default" {
			return "", fmt.Errorf("cannot determine running server audit path from %q", value)
		}
		if value == "off" {
			return "off", nil
		}
		if !filepath.IsAbs(value) {
			if !filepath.IsAbs(info.WorkingDirectory) {
				return "", errors.New("cannot resolve relative audit path without an absolute working directory")
			}
			value = filepath.Join(info.WorkingDirectory, value)
		}
		return filepath.Clean(value), nil
	case "degraded":
		return "", fmt.Errorf("running audit is degraded; repair it before updating")
	default:
		return "", fmt.Errorf("running audit has unknown state %q", info.Audit.State)
	}
}

// lastAuditFileArgument matches Go's flag parser: a later explicit flag wins.
// Other arguments may be from a version with different flags, so we must not
// parse or validate the complete historical argument list.
func lastAuditFileArgument(args []string) (value string, found bool, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--audit-file" || arg == "-audit-file":
			if i+1 >= len(args) {
				return "", false, errors.New("missing value for --audit-file")
			}
			i++
			value, found = args[i], true
		case strings.HasPrefix(arg, "--audit-file="):
			value, found = strings.TrimPrefix(arg, "--audit-file="), true
		case strings.HasPrefix(arg, "-audit-file="):
			value, found = strings.TrimPrefix(arg, "-audit-file="), true
		}
	}
	return value, found, nil
}

// verifyRestartAudit confirms that activation or rollback uses the selected
// journal. Modern servers must also report a healthy writer; older servers
// without audit status expose only their explicitly supplied serve arguments.
func verifyRestartAudit(info mcpserver.RuntimeInfo, expected string) error {
	actual, err := updateAuditPath(info)
	if err != nil {
		return fmt.Errorf("restarted server audit check: %w", err)
	}
	if actual == "off" && expected == "off" {
		return nil
	}
	if actual == "off" || expected == "off" || !sameAuditPath(actual, expected) {
		return fmt.Errorf("restarted server audit path %q differs from requested %q", actual, expected)
	}
	return nil
}
