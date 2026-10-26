//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terminalmcp/internal/mcpserver"
)

func TestUpdaterRefusesStaleOrInvalidExplicitEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := standaloneDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	physicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, "token")
	saved := standaloneConfig{
		Listen: "127.0.0.1:8123", TokenFile: token,
		FileRoot: physicalHome, WorkingDirectory: physicalHome,
		EnvOverrides: map[string]string{"CUSTOM": "explicit"},
	}
	path := filepath.Join(dir, "config.json")
	if err := writeJSON(path, saved); err != nil {
		t.Fatal(err)
	}
	live := mcpserver.RuntimeInfo{
		Listen: saved.Listen, FileRoot: saved.FileRoot, WorkingDirectory: saved.WorkingDirectory,
		Executable: filepath.Join(dir, "terminal-mcp"),
	}
	direct := live
	direct.Executable = filepath.Join(home, "development", "terminal-mcp")
	other, err := updateEnvironmentOverrides(direct, token)
	if err != nil || len(other) != 0 {
		t.Fatalf("directly launched serve inherited standalone overrides: %+v %v", other, err)
	}
	actual, err := updateEnvironmentOverrides(live, token)
	if err != nil || actual["CUSTOM"] != "explicit" {
		t.Fatalf("could not load matching explicit overrides: %+v %v", actual, err)
	}
	different := live
	different.WorkingDirectory = filepath.Join(home, "different-cwd")
	if _, err := updateEnvironmentOverrides(different, token); err == nil ||
		!strings.Contains(err.Error(), "different standalone instance") {
		t.Fatalf("updater accepted overrides from mismatched runtime: %v", err)
	}
	saved.EnvOverrides = map[string]string{"TERMINAL_MCP_TOKEN": "must-not-reintroduce"}
	if err := writeJSON(path, saved); err != nil {
		t.Fatal(err)
	}
	if _, err := updateEnvironmentOverrides(live, token); err == nil {
		t.Fatal("updater accepted reserved authentication token override")
	}
}
