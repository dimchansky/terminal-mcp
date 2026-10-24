//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"terminalmcp/internal/httpguard"
)

func TestLoadHTTPTokenFirstRunAndReuse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TERMINAL_MCP_TOKEN", "")
	first, err := loadHTTPToken("")
	if err != nil {
		t.Fatal(err)
	}
	if err := httpguard.ValidateToken(first); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "terminal-mcp", "token")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != first {
		t.Fatalf("saved token differs from generated token: %v", err)
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Dir(path), 0700},
		{path, 0600},
	} {
		info, err := os.Stat(entry.path)
		if err != nil || info.Mode().Perm() != entry.mode {
			t.Fatalf("%s permissions: %v %v", entry.path, info, err)
		}
	}
	second, err := loadHTTPToken("")
	if err != nil || first != second {
		t.Fatalf("restarting changed bearer token: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".token-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("unremoved temporary token files: %v, %v", matches, err)
	}
}

func TestLoadHTTPTokenOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TERMINAL_MCP_TOKEN", "0123456789abcdef0123456789abcdef")
	token, err := loadHTTPToken("")
	if err != nil || token != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("environment token: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "terminal-mcp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("environment credential unexpectedly generated state: %v", err)
	}
	explicit := filepath.Join(home, "explicit-token")
	if err := os.WriteFile(explicit, []byte(strings.Repeat("x", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := loadHTTPToken(explicit); err != nil || token != strings.Repeat("x", 64) {
		t.Fatalf("explicit token did not override environment: %v", err)
	}
}

func TestEnsureDefaultTokenNoOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "token")
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	errorsCh := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := ensureDefaultToken(path)
			results <- created
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	created := 0
	for result := range results {
		if result {
			created++
		}
	}
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("expected exactly one token publication, got %d", created)
	}
	if _, err := httpguard.LoadToken(path); err != nil {
		t.Fatalf("published token invalid: %v", err)
	}
}

func TestEnsureDefaultTokenRejectsUnsafePaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir, path string)
	}{
		{"world-readable directory", func(t *testing.T, dir, path string) {
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink token", func(t *testing.T, dir, path string) {
			target := filepath.Join(dir, "real-token")
			if err := os.WriteFile(target, []byte(strings.Repeat("x", 64)), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"world-readable token", func(t *testing.T, dir, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("x", 64)), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "token")
			tc.setup(t, dir, path)
			if created, err := ensureDefaultToken(path); err == nil || created {
				t.Fatalf("unsafe path accepted: created=%t err=%v", created, err)
			}
		})
	}
}
