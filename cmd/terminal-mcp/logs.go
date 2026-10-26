package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"terminalmcp/internal/audit"
)

// defaultLogDirectory is a stable user-owned location. Do not use
// os.UserConfigDir on macOS: it points to Library/Application Support,
// whereas the standalone controller already uses ~/.config/terminal-mcp.
func defaultLogDirectory() (string, error) {
	if runtime.GOOS == "windows" {
		appdata, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(appdata, "terminal-mcp", "logs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "terminal-mcp", "logs"), nil
}

// sameAuditPath compares existing parent directories through their real
// paths, not their spelling. macOS commonly aliases /var to /private/var.
func sameAuditPath(a, b string) bool {
	normalize := func(path string) string {
		dir, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err == nil {
			path = filepath.Join(dir, filepath.Base(path))
		}
		return filepath.Clean(path)
	}
	left, right := normalize(a), normalize(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func defaultAuditPath() (string, error) {
	dir, err := defaultLogDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "audit.jsonl"), nil
}

// legacyAuditPath returns only the old platform default. Explicit custom
// audit paths are never automatically migrated.
func legacyAuditPath() (string, error) {
	if runtime.GOOS == "windows" {
		appdata, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(appdata, "terminal-mcp", "audit.jsonl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP", "audit.jsonl"), nil
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "terminal-mcp", "audit.jsonl"), nil
}

type serverLogs struct {
	stdout *os.File
	stderr *os.File
}

func (s serverLogs) Close() error {
	var err error
	if s.stdout != nil {
		err = errors.Join(err, s.stdout.Close())
	}
	if s.stderr != nil {
		err = errors.Join(err, s.stderr.Close())
	}
	return err
}

// activeServerLogPaths reports only logs that actually back the current
// process descriptors. A manually launched foreground server is not assumed
// to be writing to the shared detached-server log files.
func activeServerLogPaths() (stdout, stderr string) {
	dir, err := defaultLogDirectory()
	if err != nil {
		return "", ""
	}
	for _, stream := range []struct {
		handle *os.File
		name   string
		output *string
	}{
		{os.Stdout, "server.stdout.log", &stdout},
		{os.Stderr, "server.stderr.log", &stderr},
	} {
		openInfo, err := stream.handle.Stat()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, stream.name)
		fileInfo, err := os.Stat(path)
		if err == nil && os.SameFile(openInfo, fileInfo) {
			*stream.output = path
		}
	}
	return stdout, stderr
}

// The OS retains its own duplicate handles for the detached child after Start.
// Keep them open for the process lifetime; there is no rename-based log rotation
// until the explicitly designed descriptor-safe stage 6.
func openServerLogs() (serverLogs, error) {
	dir, err := defaultLogDirectory()
	if err != nil {
		return serverLogs{}, err
	}
	if err := audit.PrepareDirectory(dir); err != nil {
		return serverLogs{}, fmt.Errorf("prepare diagnostics: %w", err)
	}
	out, err := openPrivateDiagnostic(filepath.Join(dir, "server.stdout.log"))
	if err != nil {
		return serverLogs{}, err
	}
	stderr, err := openPrivateDiagnostic(filepath.Join(dir, "server.stderr.log"))
	if err != nil {
		return serverLogs{}, errors.Join(err, out.Close())
	}
	return serverLogs{stdout: out, stderr: stderr}, nil
}

func openPrivateDiagnostic(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("diagnostic log is not a regular file: %s", path)
		}
		if err := checkDiagnosticPrivate(path, info); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err != nil {
		return nil, errors.Join(err, f.Close())
	} else if err := checkDiagnosticPrivate(path, info); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
