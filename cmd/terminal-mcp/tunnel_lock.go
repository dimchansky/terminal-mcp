package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// lockTunnelID prevents two terminal-mcp instances owned by this OS user from
// polling the same tunnel. The OS lock is released even if the process crashes.
// External tunnel-client processes do not participate: operators must still
// stop those explicitly before enabling an embedded tunnel with the same ID.
func lockTunnelID(id string) (func(), error) {
	token, err := defaultTokenPath()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(token), "tunnel-locks")
	if err := prepareUpdateDir(dir); err != nil {
		return nil, fmt.Errorf("prepare tunnel lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(id))
	path := filepath.Join(dir, hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("open tunnel lock: %w", err)
	}
	unlock, err := lockTunnelFile(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("tunnel %s is already active in another terminal-mcp process: %w", id, err)
	}
	return func() { unlock(); _ = file.Close() }, nil
}
