//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func remotePrintenv(t *testing.T, client *updateClient, serverID, operationID, name string) string {
	t.Helper()
	var answer struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
		Observation struct {
			Events []struct {
				Text   *string `json:"text"`
				Stream string  `json:"stream"`
			} `json:"events"`
		} `json:"observation"`
	}
	err := client.tool("terminal_start", map[string]any{
		"server_id": serverID, "operation_id": operationID,
		"argv":          []string{"/usr/bin/printenv", name},
		"close_on_exit": true, "wait_for": "completion", "wait_ms": 3000,
	}, &answer)
	if err != nil || answer.Receipt.Status != "applied" {
		t.Fatalf("could not read child environment: %+v, %v", answer, err)
	}
	var text strings.Builder
	for _, e := range answer.Observation.Events {
		if e.Text != nil && e.Stream == "stdout" {
			text.WriteString(*e.Text)
		}
	}
	return strings.TrimSpace(text.String())
}

func TestStandaloneEnvironmentPolicyAcrossRestart(t *testing.T) {
	home, binary := standaloneTestBinary(t)
	addr, err := reserveProbePort()
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, ".config", "terminal-mcp", "token")
	serviceDir := filepath.Join(home, "stable-service-working-dir")
	if err := os.Mkdir(serviceDir, 0700); err != nil {
		t.Fatal(err)
	}
	otherCallerDir := filepath.Join(home, "different-caller")
	if err := os.Mkdir(otherCallerDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = standaloneTestCommand(binary, home, "stop") })

	const extraKey = "TERMINAL_MCP_EXPLICIT_TEST_VALUE"
	firstPath := "/usr/bin:/bin:/first-launch"
	secondPath := "/usr/bin:/bin:/second-launch"
	thirdPath := "/usr/bin:/bin:/third-launch"
	t.Setenv("PATH", firstPath)
	one, err := standaloneTestCommand(binary, home, "start",
		"--listen="+addr, "--file-root="+home, "--audit-file=off",
		"--working-directory="+serviceDir, "--env="+extraKey+"=remember-me")
	if err != nil {
		t.Fatalf("first background launch failed: %v %s", err, one)
	}
	client, err := newUpdateClient(addr, token)
	if err != nil {
		t.Fatal(err)
	}
	check := func(path, value, op string) {
		t.Helper()
		info, err := client.runtime()
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(serviceDir)
		if err != nil || info.WorkingDirectory != resolved {
			t.Fatalf("server cwd changed: %+v %v", info, err)
		}
		if got := remotePrintenv(t, client, info.ServerID, op+"-path", "PATH"); got != path {
			t.Fatalf("PATH from wrong launch: got=%q want=%q", got, path)
		}
		if got := remotePrintenv(t, client, info.ServerID, op+"-explicit", extraKey); got != value {
			t.Fatalf("explicit environment override got=%q want=%q", got, value)
		}
	}
	check(firstPath, "remember-me", "one")

	if result, err := standaloneTestCommand(binary, home, "stop"); err != nil {
		t.Fatalf("first stop failed: %v %s", err, result)
	}
	t.Setenv("PATH", secondPath)
	// The second CLI deliberately runs from a different working directory,
	// but the service retains its *configured* cwd, not the caller's cwd.
	startAgain := exec.Command(binary, "start")
	startAgain.Dir = otherCallerDir
	startAgain.Env = append(withoutEnvToken(os.Environ()), "HOME="+home)
	result, err := startAgain.CombinedOutput()
	if err != nil {
		t.Fatalf("restart with a different CLI cwd failed: %v %s", err, result)
	}
	check(secondPath, "remember-me", "two")

	if result, err := standaloneTestCommand(binary, home, "stop"); err != nil {
		t.Fatalf("second stop failed: %v %s", err, result)
	}
	t.Setenv("PATH", thirdPath)
	cleared, err := standaloneTestCommand(binary, home, "start", "--clear-env")
	if err != nil {
		t.Fatalf("clear-env restart failed: %v %s", err, cleared)
	}
	check(thirdPath, "", "three")
	config, err := savedStandaloneConfig(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	if err != nil || len(config.EnvOverrides) != 0 {
		t.Fatalf("explicit env overrides were not cleared: %+v %v", config.EnvOverrides, err)
	}
	content, err := os.ReadFile(filepath.Join(home, ".config", "terminal-mcp", "standalone", "config.json"))
	if err != nil || strings.Contains(string(content), `"path":`) {
		t.Fatalf("automatic PATH snapshot reappeared in config: %s %v", content, err)
	}
}
