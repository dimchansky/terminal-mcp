package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func envValue(env []string, name string) (string, int) {
	var value string
	var matches int
	for _, item := range env {
		k, v, ok := strings.Cut(item, "=")
		if ok && (k == name || runtime.GOOS == "windows" && strings.EqualFold(k, name)) {
			value, matches = v, matches+1
		}
	}
	return value, matches
}

func TestChildEnvironmentInheritsCurrentPathWithoutSnapshot(t *testing.T) {
	input := []string{"PATH=/current/launcher/bin", "PROJECT=from-launcher",
		"TERMINAL_MCP_TOKEN=private-secret", "terminal_mcp_token=other-secret", "HOME=/home/test"}
	got := childEnvironment(input, nil)
	if value, count := envValue(got, "PATH"); count != 1 || value != "/current/launcher/bin" {
		t.Fatalf("inherited PATH altered: %v", got)
	}
	if value, count := envValue(got, "PROJECT"); count != 1 || value != "from-launcher" {
		t.Fatalf("other inherited variable altered: %v", got)
	}
	if _, count := envValue(got, "TERMINAL_MCP_TOKEN"); count != 0 {
		t.Fatal("auth secret leaked to child")
	}
	for _, item := range got {
		if strings.Contains(item, "secret") {
			t.Fatalf("secret leaked to child: %v", got)
		}
	}
	override, err := withEnvironmentOptions(nil, false, []string{"PATH=/explicit/tools", "PROJECT=explicit-value"})
	if err != nil {
		t.Fatal(err)
	}
	got = childEnvironment(input, override)
	if value, count := envValue(got, "PATH"); count != 1 || value != "/explicit/tools" {
		t.Fatalf("explicit PATH was not last/unique: %v", got)
	}
	if value, count := envValue(got, "PROJECT"); count != 1 || value != "explicit-value" {
		t.Fatalf("explicit PROJECT was not applied: %v", got)
	}
}

func TestEnvironmentOptionsUpdateAndClear(t *testing.T) {
	saved := map[string]string{"PATH": "/old", "A": "original"}
	result, err := withEnvironmentOptions(saved, false, []string{"PATH=/new=with-equals", "B=added", "A=replaced"})
	if err != nil {
		t.Fatal(err)
	}
	if result["PATH"] != "/new=with-equals" || result["A"] != "replaced" ||
		result["B"] != "added" || saved["PATH"] != "/old" {
		t.Fatalf("unexpected mutation/persistence: got=%+v saved=%+v", result, saved)
	}
	cleared, err := withEnvironmentOptions(result, true, nil)
	if err != nil || len(cleared) != 0 {
		t.Fatalf("clear-env did not remove explicit overrides: %+v err=%v", cleared, err)
	}
	next, err := withEnvironmentOptions(result, true, []string{"ONLY=after-clear"})
	if err != nil || len(next) != 1 || next["ONLY"] != "after-clear" {
		t.Fatalf("clear-env plus new explicit value: %+v err=%v", next, err)
	}
	// An invalid saved map can still be repaired deliberately with clear-env.
	next, err = withEnvironmentOptions(map[string]string{"TERMINAL_MCP_TOKEN": "invalid"}, true, nil)
	if err != nil || len(next) != 0 {
		t.Fatalf("clear-env failed to repair invalid saved overrides: %+v err=%v", next, err)
	}
}

func TestEnvironmentFlagsValidateKeysAndDoNotPrintSecretValues(t *testing.T) {
	for _, raw := range []string{"NO_EQUAL_SIGN", "=emptykey", "9LEADING=digit", "BAD-KEY=x",
		"TERMINAL_MCP_TOKEN=secret", "terminal_mcp_token=secret", "VAR=has\x00nul"} {
		var flags environmentFlags
		if err := flags.Set(raw); err == nil {
			t.Errorf("invalid explicit environment override %q accepted", raw)
		}
	}
	for _, input := range []map[string]string{
		{"PATH": "a", "PaTH": "b"},
		{"a": "b", "A": "c", "B": "d"},
		{"TERMINAL_MCP_TOKEN": "secret"},
	} {
		if err := validateEnvironmentOverrides(input); err == nil {
			t.Errorf("invalid/ambiguous saved map accepted: %+v", input)
		}
	}
	var flags environmentFlags
	if err := flags.Set("EXPLICIT=some-secret-value"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(flags.String(), "some-secret-value") {
		t.Fatal("flag's diagnostic String method exposes secret value")
	}
	if _, err := withEnvironmentOptions(map[string]string{"FOO": "a"}, false, []string{"foo=b"}); err == nil {
		t.Fatal("case-insensitive name collision accepted")
	}
}

func TestConfigBackwardsCompatibilityIgnoresLegacyPath(t *testing.T) {
	const old = `{"listen":"127.0.0.1:8090","working_directory":"/srv/app","path":"/old/snapshot","file_root":"/"}`
	var unixConfig standaloneConfigForTest
	if err := json.Unmarshal([]byte(old), &unixConfig); err != nil {
		t.Fatal(err)
	}
	if unixConfig.WorkingDirectory != "/srv/app" || len(unixConfig.EnvOverrides) != 0 {
		t.Fatalf("legacy configuration altered cwd or persisted legacy PATH: %+v", unixConfig)
	}
	raw, err := json.Marshal(unixConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"path"`) || strings.Contains(string(raw), "snapshot") {
		t.Fatalf("legacy snapshot was emitted again: %s", raw)
	}
}

type standaloneConfigForTest struct {
	WorkingDirectory string            `json:"working_directory"`
	EnvOverrides     map[string]string `json:"env_overrides,omitempty"`
}

func TestWorkingDirectoryIsExplicitAndMustExist(t *testing.T) {
	dir := t.TempDir()
	if err := validateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateDirectory(file); err == nil {
		t.Fatal("regular file accepted as working directory")
	}
	if err := validateDirectory(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing configured working directory was silently ignored")
	}
}
