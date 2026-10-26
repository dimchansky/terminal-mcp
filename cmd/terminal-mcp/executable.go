package main

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// sourceRevision is only descriptive: correctness and update/rollback
// authorization continue to depend on the complete SHA-256 executable hash.
func sourceRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return revisionFromSettings(info.Settings)
}

func executableRevision(path string) string {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return ""
	}
	return revisionFromSettings(info.Settings)
}

// Older installed servers do not yet return their VCS revision in /admin/runtime.
// In that case inspect the binary only when its hash still matches the
// executable that the running process reported at startup.
func verifiedRunningRevision(path, expectedSHA, revision string) string {
	if revision != "" {
		return revision
	}
	hash, err := fileSHA256(path)
	if err != nil || hash != expectedSHA {
		return ""
	}
	return executableRevision(path)
}

func revisionFromSettings(settings []debug.BuildSetting) string {
	var revision, dirty string
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if revision == "" {
		return ""
	}
	return revision + dirty
}

// installIfMissing installs the executable only once. A subsequent 'start'
// must never act as a covert upgrade or downgrade, even when the caller uses
// a different copy of the same program.
func installIfMissing(source, installed string, mode os.FileMode) (bool, error) {
	if source == "" || installed == "" {
		return false, errors.New("executable paths must be nonempty")
	}
	info, err := os.Lstat(installed)
	if errors.Is(err, os.ErrNotExist) {
		if err := copyExecutable(source, installed, mode); err != nil {
			return false, fmt.Errorf("first install: %w", err)
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("installed executable is not a regular file: %s", installed)
	}
	sourceHash, err := fileSHA256(source)
	if err != nil {
		return false, fmt.Errorf("inspect requested executable: %w", err)
	}
	currentHash, err := fileSHA256(installed)
	if err != nil {
		return false, fmt.Errorf("inspect installed executable: %w", err)
	}
	if !strings.EqualFold(sourceHash, currentHash) {
		return false, fmt.Errorf("installed_version_mismatch: requested binary (%s) differs from installed binary (%s); "+
			"start never replaces an installation; use terminal-mcp update --candidate=<new-binary> instead",
			shortHash(sourceHash), shortHash(currentHash))
	}
	return false, nil
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
