//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"terminalmcp/internal/httpguard"
	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/winsecure"
)

type winStandaloneConfig struct {
	Listen           string            `json:"listen"`
	TokenFile        string            `json:"token_file"`
	FileRoot         string            `json:"file_root"`
	AuditFile        string            `json:"audit_file,omitempty"`
	AuditMaxBytes    int64             `json:"audit_max_bytes,omitempty"`
	WorkingDirectory string            `json:"working_directory"`
	EnvOverrides     map[string]string `json:"env_overrides,omitempty"`
}

func windowsStateDir() (string, error) {
	token, err := defaultTokenPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(token), "standalone"), nil
}

func windowsConfig(stateDir string) (winStandaloneConfig, error) {
	var saved winStandaloneConfig
	err := readJSON(filepath.Join(stateDir, "config.json"), &saved)
	if err == nil {
		if saved.Listen == "" || saved.TokenFile == "" || saved.FileRoot == "" ||
			saved.WorkingDirectory == "" {
			return saved, errors.New("incomplete standalone config")
		}
		return saved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return saved, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return saved, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return saved, err
	}
	token, err := defaultTokenPath()
	if err != nil {
		return saved, err
	}
	return winStandaloneConfig{Listen: "127.0.0.1:8090", TokenFile: token,
		FileRoot: home, WorkingDirectory: cwd}, nil
}

func windowsListenerInUse(listen string) bool {
	conn, err := net.DialTimeout("tcp", listen, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func windowsStandaloneCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("missing command")
	}
	switch args[0] {
	case "start":
		return windowsStart(args[1:])
	case "status", "stop":
		return windowsControl(args[0], args[1:])
	default:
		return fmt.Errorf("unsupported command: %s", args[0])
	}
}

func standaloneCommand(args []string) error { return windowsStandaloneCommand(args) }

func windowsStart(args []string) error {
	flags := flag.NewFlagSet("terminal-mcp start", flag.ContinueOnError)
	var listen, root, token, auditFile, workingDirectory string
	var auditMaxBytes int64
	var envFlags environmentFlags
	var clearEnv bool
	flags.StringVar(&workingDirectory, "working-directory", "", "server working directory; default retains configured initial cwd")
	flags.Var(&envFlags, "env", "explicit persistent KEY=VALUE environment override (repeatable)")
	flags.BoolVar(&clearEnv, "clear-env", false, "discard previously saved environment overrides")
	flags.StringVar(&listen, "listen", "", "loopback listen address")
	flags.StringVar(&root, "file-root", "", "root for file tools")
	flags.StringVar(&token, "token-file", "", "private HTTP bearer token file")
	flags.StringVar(&auditFile, "audit-file", "", "audit log path; off disables; default restores platform default")
	flags.Int64Var(&auditMaxBytes, "audit-max-bytes", 0, "hard audit history limit in bytes; 0 keeps saved/default limit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected start arguments")
	}
	stateDir, err := windowsStateDir()
	if err != nil {
		return err
	}
	cfg, err := windowsConfig(stateDir)
	if err != nil {
		return err
	}
	if windowsListenerInUse(cfg.Listen) {
		return fmt.Errorf("server already listening at %s; refusing duplicate start", cfg.Listen)
	}
	if listen != "" {
		cfg.Listen = listen
	}
	if token != "" {
		cfg.TokenFile = token
	}
	if root != "" {
		cfg.FileRoot = root
	}
	if workingDirectory != "" {
		cfg.WorkingDirectory = workingDirectory
	}
	cfg.EnvOverrides, err = withEnvironmentOptions(cfg.EnvOverrides, clearEnv, envFlags)
	if err != nil {
		return err
	}
	if auditFile == "default" {
		cfg.AuditFile = ""
	} else if auditFile != "" {
		cfg.AuditFile = auditFile
	}
	if auditMaxBytes < 0 {
		return errors.New("audit-max-bytes must be positive")
	}
	if auditMaxBytes != 0 {
		cfg.AuditMaxBytes = auditMaxBytes
	}
	if cfg.AuditFile == "" {
		cfg.AuditFile, err = defaultAuditPath()
		if err != nil {
			return err
		}
	} else if cfg.AuditFile != "off" {
		cfg.AuditFile, err = filepath.Abs(cfg.AuditFile)
		if err != nil {
			return err
		}
	}
	if err := httpguard.ValidateAddress(cfg.Listen); err != nil {
		return err
	}
	if windowsListenerInUse(cfg.Listen) {
		return fmt.Errorf("port %s is occupied", cfg.Listen)
	}
	cfg.FileRoot, err = filepath.Abs(cfg.FileRoot)
	if err != nil {
		return err
	}
	cfg.FileRoot, err = filepath.EvalSymlinks(cfg.FileRoot)
	if err != nil {
		return err
	}
	cfg.WorkingDirectory, err = filepath.Abs(cfg.WorkingDirectory)
	if err != nil {
		return err
	}
	if cfg.WorkingDirectory, err = filepath.EvalSymlinks(cfg.WorkingDirectory); err != nil {
		return err
	}
	if err := validateDirectory(cfg.WorkingDirectory); err != nil {
		return err
	}
	cfg.TokenFile, err = filepath.Abs(cfg.TokenFile)
	if err != nil {
		return err
	}
	if err := winsecure.EnsurePrivateDir(stateDir); err != nil {
		return err
	}
	if err := winsecure.EnsurePrivateDir(filepath.Dir(cfg.TokenFile)); err != nil {
		return err
	}
	defaultToken, err := defaultTokenPath()
	if err != nil {
		return err
	}
	if cfg.TokenFile == defaultToken {
		if _, err := ensureDefaultToken(cfg.TokenFile); err != nil {
			return err
		}
	}
	if _, err := httpguard.LoadToken(cfg.TokenFile); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	installed := filepath.Join(stateDir, "terminal-mcp.exe")
	firstInstall, err := installIfMissing(self, installed, 0700)
	if err != nil {
		return err
	}
	logs, err := openServerLogs()
	if err != nil {
		return err
	}
	defer func() { _ = logs.Close() }()
	serverArgs := []string{"serve", "--listen=" + cfg.Listen,
		"--token-file=" + cfg.TokenFile, "--file-root=" + cfg.FileRoot, "--audit-file=" + cfg.AuditFile}
	if cfg.AuditMaxBytes != 0 {
		serverArgs = append(serverArgs, fmt.Sprintf("--audit-max-bytes=%d", cfg.AuditMaxBytes))
	}
	cmd := exec.Command(installed, serverArgs...)
	cmd.Dir = cfg.WorkingDirectory
	cmd.Env = childEnvironment(cmd.Environ(), cfg.EnvOverrides)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	cmd.Stdout, cmd.Stderr = logs.stdout, logs.stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	client, err := newUpdateClient(cfg.Listen, cfg.TokenFile)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	expectedHash, err := fileSHA256(installed)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	var ready mcpserver.RuntimeInfo
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		ready, err = client.runtime()
		if err == nil && ready.PID == cmd.Process.Pid &&
			ready.ExecutableSHA256 == expectedHash && ready.FileRoot == cfg.FileRoot {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ready.PID != cmd.Process.Pid || err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("standalone did not become ready; inspect %s: %w", logs.stderr.Name(), err)
	}
	if err := writeJSON(filepath.Join(stateDir, "config.json"), cfg); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	if err := cmd.Process.Release(); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"status": "started", "server_id": ready.ServerID, "pid": ready.PID,
		"executable": ready.Executable, "revision": ready.Revision,
		"installed_new": firstInstall,
	})
}

func windowsControl(action string, args []string) error {
	flags := flag.NewFlagSet("terminal-mcp "+action, flag.ContinueOnError)
	var force bool
	flags.BoolVar(&force, "force", false, "forcibly terminate active work (stop only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (force && action != "stop") {
		return fmt.Errorf("invalid arguments for %s", action)
	}
	dir, err := windowsStateDir()
	if err != nil {
		return err
	}
	cfg, err := windowsConfig(dir)
	if err != nil {
		return err
	}
	if !windowsListenerInUse(cfg.Listen) {
		installed := filepath.Join(dir, "terminal-mcp.exe")
		out := map[string]any{"status": "stopped"}
		if info, err := os.Lstat(installed); err == nil && info.Mode().IsRegular() {
			out["executable"] = installed
			out["revision"] = executableRevision(installed)
			if digest, err := fileSHA256(installed); err == nil {
				out["executable_sha256"] = digest
			}
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	client, err := newUpdateClient(cfg.Listen, cfg.TokenFile)
	if err != nil {
		return err
	}
	info, err := client.runtime()
	if err != nil {
		return err
	}
	if action == "status" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"status": "running", "pid": info.PID, "server_id": info.ServerID,
			"draining": info.Draining, "drain_mode": info.DrainMode,
			"active_operations": info.ActiveOperations, "active_sessions": info.ActiveSessions,
			"file_root": info.FileRoot, "listen": info.Listen, "audit": info.Audit,
			"executable":        info.Executable,
			"revision":          verifiedRunningRevision(info.Executable, info.ExecutableSHA256, info.Revision),
			"executable_sha256": info.ExecutableSHA256,
			"working_directory": info.WorkingDirectory,
			"stdout_log":        info.StdoutLog, "stderr_log": info.StderrLog,
		})
	}
	if force {
		if err := client.maintenance(info.ServerID, "force"); err != nil {
			return err
		}
	} else if err := client.maintenance(info.ServerID, "stop"); err != nil {
		return err
	}
	deadline := time.Now().Add(800 * time.Millisecond)
	if force {
		deadline = time.Now().Add(5 * time.Second)
	}
	for time.Now().Before(deadline) {
		if !windowsListenerInUse(cfg.Listen) {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "stopped", "forced": force})
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !force {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "draining"})
	}
	current, err := client.runtime()
	if err != nil || current.ServerID != info.ServerID || current.PID != info.PID {
		return errors.New("forced stop not confirmed: server identity changed or unavailable")
	}
	process, err := os.FindProcess(info.PID)
	if err != nil {
		return err
	}
	if err := process.Kill(); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "stopped", "forced": true})
}
