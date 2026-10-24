//go:build linux || darwin

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"terminalmcp/internal/httpguard"
)

// loadHTTPToken leaves explicit credentials untouched. Only a default HTTP
// launch creates a token, so stdio clients need no token or local state.
func loadHTTPToken(explicit string) (string, error) {
	if explicit != "" || os.Getenv("TERMINAL_MCP_TOKEN") != "" {
		return httpguard.LoadToken(explicit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".config", "terminal-mcp", "token")
	created, err := ensureDefaultToken(path)
	if err != nil {
		return "", err
	}
	if created {
		fmt.Fprintf(os.Stderr, "terminal-mcp: created local HTTP token at %s (keep it secret)\n", path)
	}
	return httpguard.LoadToken(path)
}

func ensureDefaultToken(path string) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return false, fmt.Errorf("token directory must be private (0700): %s", dir)
	}
	info, err = os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return false, fmt.Errorf("token must be a private regular file: %s", path)
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	// Publish a complete token atomically; never replace one another process created.
	tmp, err := os.CreateTemp(dir, ".token-")
	if err != nil {
		return false, err
	}
	defer func() {
		if err := os.Remove(tmp.Name()); err != nil {
			fmt.Fprintf(os.Stderr, "terminal-mcp: remove temporary token: %v\n", err)
		}
	}()
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err == nil {
		_, err = tmp.WriteString(hex.EncodeToString(secret[:]))
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
