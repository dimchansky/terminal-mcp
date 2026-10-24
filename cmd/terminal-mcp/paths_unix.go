//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func defaultTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "terminal-mcp", "token"), nil
}
func prepareUpdateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("update directory must be private (0700): %s", dir)
	}
	return nil
}
