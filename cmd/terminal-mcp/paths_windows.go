//go:build windows

package main

import (
	"os"
	"path/filepath"

	"terminalmcp/internal/winsecure"
)

func defaultTokenPath() (string, error) {
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "terminal-mcp", "token"), nil
}
func prepareUpdateDir(dir string) error { return winsecure.EnsurePrivateDir(dir) }
