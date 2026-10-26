//go:build linux || darwin

package main

import (
	"fmt"
	"path/filepath"

	"terminalmcp/internal/mcpserver"
)

// updateEnvironmentOverrides pins the explicitly configured variables into
// the detached worker's environment. It does not snapshot PATH (or any other
// inherited variable), and no secrets are copied into the update manifest.
//
// A direct 'serve' invocation has no standalone config and simply inherits
// the environment of the command that scheduled the update.
func updateEnvironmentOverrides(info mcpserver.RuntimeInfo, tokenFile string) (map[string]string, error) {
	stateDir, err := standaloneDirectory()
	if err != nil {
		return nil, err
	}
	config, err := savedStandaloneConfig(filepath.Join(stateDir, "config.json"))
	if err != nil {
		return nil, err
	}
	if len(config.EnvOverrides) == 0 {
		return nil, nil
	}
	// A directly launched 'serve' binary is not controlled by the saved
	// standalone config, even if it happens to share its port and cwd.
	// In that case use only the environment of the update command.
	if !sameAuditPath(info.Executable, filepath.Join(stateDir, "terminal-mcp")) {
		return nil, nil
	}
	// Do not silently inject overrides from a stale unrelated config into
	// a directly launched 'serve' instance. The running server is authoritative
	// for the listen address, file root and stable working directory.
	if config.Listen != info.Listen || !sameAuditPath(config.WorkingDirectory, info.WorkingDirectory) ||
		!sameAuditPath(config.FileRoot, info.FileRoot) || !sameAuditPath(config.TokenFile, tokenFile) {
		return nil, fmt.Errorf("persisted environment overrides belong to a different standalone instance; refusing update")
	}
	if err := validateEnvironmentOverrides(config.EnvOverrides); err != nil {
		return nil, err
	}
	return config.EnvOverrides, nil
}
