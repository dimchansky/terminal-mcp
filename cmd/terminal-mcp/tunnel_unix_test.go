//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTunnelPrivateCredentialFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("fake-runtime-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const id = "tunnel_sdktestaaaaaaaaaaaaaaaaaaaaaaaaa"
	savedID, savedPath, err := resolveStandaloneTunnelConfig(true, id, path)
	if err != nil || savedID != id || savedPath != path {
		t.Fatalf("file not accepted: %s %s %v", savedID, savedPath, err)
	}
	if err := validateTunnelCandidate([]string{"--tunnel-id=" + id, "--tunnel-key-file=" + path}); err != nil {
		t.Fatalf("offline SDK candidate preflight failed: %v", err)
	}
	if err := validateTunnelCandidate([]string{"--tunnel-id=" + id}); err == nil {
		t.Fatal("candidate preflight accepted missing secret file")
	}
	cfg, err := loadTunnelConfig(options{tunnel: true, tunnelID: id, tunnelKeyFile: path})
	if err != nil || cfg.apiKey != "fake-runtime-key" {
		t.Fatalf("credential not read: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTunnelKeyFile(path); err == nil {
		t.Fatal("world-readable key accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "key-link")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTunnelKeyFile(alias); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("key symlink accepted: %v", err)
	}
}
