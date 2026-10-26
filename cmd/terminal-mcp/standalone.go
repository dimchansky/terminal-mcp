//go:build linux || darwin

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
	"runtime"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"terminalmcp/internal/httpguard"
)

type standaloneConfig struct {
	Listen           string            `json:"listen"`
	TokenFile        string            `json:"token_file"`
	FileRoot         string            `json:"file_root"`
	AuditFile        string            `json:"audit_file,omitempty"`
	AuditMaxBytes    int64             `json:"audit_max_bytes,omitempty"`
	WorkingDirectory string            `json:"working_directory"`
	EnvOverrides     map[string]string `json:"env_overrides,omitempty"`
}

func standaloneCommand(args []string) error {
	switch args[0] {
	case "start":
		return startStandalone(args[1:])
	case "status", "stop":
		return controlStandalone(args[0], args[1:])
	default:
		return fmt.Errorf("unknown standalone command: %s", args[0])
	}
}

func standaloneDirectory() (string, error) {
	token, err := defaultTokenFile()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(token), "standalone"), nil
}

func savedStandaloneConfig(path string) (standaloneConfig, error) {
	var c standaloneConfig
	err := readJSON(path, &c)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	return c, err
}

// A saved standalone configuration wins. On the first macOS migration, reuse
// the launchd controller's settings; do not silently change file_root from /.
func initialStandaloneConfig(stateDir string) (standaloneConfig, error) {
	c, err := savedStandaloneConfig(filepath.Join(stateDir, "config.json"))
	if err != nil {
		return c, err
	}
	if c.Listen != "" {
		if c.TokenFile == "" || c.WorkingDirectory == "" || c.FileRoot == "" {
			return c, errors.New("stored standalone configuration is incomplete")
		}
		return c, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return c, err
	}
	token, err := defaultTokenFile()
	if err != nil {
		return c, err
	}
	c = standaloneConfig{
		Listen: "127.0.0.1:8090", TokenFile: token, FileRoot: cwd,
		WorkingDirectory: cwd,
	}
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return c, err
		}
		legacy := filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP", "config.json")
		var source struct {
			Project   string `json:"project"`
			Listen    string `json:"listen"`
			TokenFile string `json:"token_file"`
			FileRoot  string `json:"file_root"`
		}
		if err := readJSON(legacy, &source); err != nil && !errors.Is(err, os.ErrNotExist) {
			return c, err
		}
		if source.Project != "" {
			c.WorkingDirectory = source.Project
		}
		if source.Listen != "" {
			c.Listen = source.Listen
		}
		if source.TokenFile != "" {
			c.TokenFile = source.TokenFile
		}
		if source.FileRoot != "" {
			c.FileRoot = source.FileRoot
		}
	}
	return c, nil
}

func resolveStandaloneConfig(c standaloneConfig) (standaloneConfig, error) {
	if err := httpguard.ValidateAddress(c.Listen); err != nil {
		return c, err
	}
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c, err
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return c, err
	}
	if n == 0 {
		return c, errors.New("background start requires a fixed, nonzero port")
	}
	if c.TokenFile, err = filepath.Abs(c.TokenFile); err != nil {
		return c, err
	}
	if c.WorkingDirectory, err = filepath.Abs(c.WorkingDirectory); err != nil {
		return c, err
	}
	if c.WorkingDirectory, err = filepath.EvalSymlinks(c.WorkingDirectory); err != nil {
		return c, err
	}
	if err := validateDirectory(c.WorkingDirectory); err != nil {
		return c, err
	}
	if err := validateEnvironmentOverrides(c.EnvOverrides); err != nil {
		return c, err
	}
	if c.FileRoot, err = filepath.Abs(c.FileRoot); err != nil {
		return c, err
	}
	if c.FileRoot, err = filepath.EvalSymlinks(c.FileRoot); err != nil {
		return c, err
	}
	info, err := os.Stat(c.FileRoot)
	if err != nil {
		return c, err
	}
	if !info.IsDir() {
		return c, errors.New("file root must be a directory")
	}
	if c.AuditFile != "" && c.AuditFile != "off" {
		if c.AuditFile, err = filepath.Abs(c.AuditFile); err != nil {
			return c, err
		}
	}
	if c.AuditMaxBytes < 0 {
		return c, errors.New("audit-max-bytes must be positive")
	}
	return c, nil
}

// This lock serializes concurrent starts without a persistent supervisor or
// stale PID file. An OS lock is released even if the CLI crashes.
func lockStandalone(stateDir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(stateDir, "controller.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another standalone operation is running: %w", err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func listenerInUse(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func startStandalone(args []string) error {
	flags := flag.NewFlagSet("terminal-mcp start", flag.ContinueOnError)
	var listen, tokenFile, fileRoot, auditFile, workingDirectory string
	var auditMaxBytes int64
	var envFlags environmentFlags
	var clearEnv bool
	flags.StringVar(&workingDirectory, "working-directory", "", "absolute server working directory; default retains configured initial cwd")
	flags.Var(&envFlags, "env", "explicit, persistent KEY=VALUE environment override (repeatable)")
	flags.BoolVar(&clearEnv, "clear-env", false, "discard previously saved environment overrides")
	flags.Int64Var(&auditMaxBytes, "audit-max-bytes", 0, "hard audit history limit in bytes; 0 keeps saved/default limit")
	flags.StringVar(&listen, "listen", "", "standalone loopback listening address")
	flags.StringVar(&tokenFile, "token-file", "", "existing bearer token file")
	flags.StringVar(&fileRoot, "file-root", "", "root directory for MCP file tools")
	flags.StringVar(&auditFile, "audit-file", "", "JSONL audit file; off disables it; default restores the platform default")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("start does not accept positional arguments")
	}
	stateDir, err := standaloneDirectory()
	if err != nil {
		return err
	}
	// A previously started standalone instance must not have its installed
	// executable overwritten, even when the caller requests a different port.
	// The old launchd port is deliberately not checked here: a new standalone
	// instance can first be tested beside it on a separate port.
	previous, err := savedStandaloneConfig(filepath.Join(stateDir, "config.json"))
	if err != nil {
		return err
	}
	if previous.Listen != "" && listenerInUse(previous.Listen) {
		return fmt.Errorf("standalone port %s already has a listener; stop it before starting another", previous.Listen)
	}
	config, err := initialStandaloneConfig(stateDir)
	if err != nil {
		return err
	}
	if listen != "" {
		config.Listen = listen
	}
	if tokenFile != "" {
		config.TokenFile = tokenFile
	}
	if fileRoot != "" {
		config.FileRoot = fileRoot
	}
	if workingDirectory != "" {
		config.WorkingDirectory = workingDirectory
	}
	config.EnvOverrides, err = withEnvironmentOptions(config.EnvOverrides, clearEnv, envFlags)
	if err != nil {
		return err
	}
	if auditFile == "default" {
		config.AuditFile = ""
	} else if auditFile != "" {
		config.AuditFile = auditFile
	}
	if auditMaxBytes < 0 {
		return errors.New("audit-max-bytes must be positive")
	}
	if auditMaxBytes != 0 {
		config.AuditMaxBytes = auditMaxBytes
	}
	config, err = resolveStandaloneConfig(config)
	if err != nil {
		return err
	}
	if listenerInUse(config.Listen) {
		return fmt.Errorf("port %s already has a listener; refusing to replace it", config.Listen)
	}
	if err := secureUpdateDir(stateDir); err != nil {
		return err
	}
	unlock, err := lockStandalone(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(filepath.Dir(stateDir), "updates", "pending")); err == nil {
		return errors.New("update in progress; inspect update-status before starting")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if listenerInUse(config.Listen) {
		return fmt.Errorf("port %s was taken while preparing the start", config.Listen)
	}
	if _, err := os.Stat(config.TokenFile); errors.Is(err, os.ErrNotExist) {
		if config.TokenFile != filepath.Join(filepath.Dir(stateDir), "token") {
			return fmt.Errorf("specified token file does not exist: %s", config.TokenFile)
		}
		if _, err := ensureDefaultToken(config.TokenFile); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := httpguard.LoadToken(config.TokenFile); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	installed := filepath.Join(stateDir, "terminal-mcp")
	current, srcErr := os.Stat(executable)
	existing, dstErr := os.Stat(installed)
	if srcErr != nil {
		return srcErr
	}
	if dstErr != nil && !errors.Is(dstErr, os.ErrNotExist) {
		return dstErr
	}
	if dstErr != nil || !os.SameFile(current, existing) {
		if err := replaceExecutable(executable, installed, 0700); err != nil {
			return err
		}
	}

	logs, err := openServerLogs()
	if err != nil {
		return err
	}
	defer func() { _ = logs.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = devnull.Close() }()
	if config.AuditFile == "" {
		config.AuditFile, err = defaultAuditPath()
		if err != nil {
			return err
		}
	}
	serverArgs := []string{"serve", "--listen=" + config.Listen, "--token-file=" + config.TokenFile,
		"--file-root=" + config.FileRoot, "--audit-file=" + config.AuditFile}
	if config.AuditMaxBytes != 0 {
		serverArgs = append(serverArgs, fmt.Sprintf("--audit-max-bytes=%d", config.AuditMaxBytes))
	}
	cmd := exec.Command(installed, serverArgs...)
	cmd.Dir = config.WorkingDirectory
	cmd.Env = childEnvironment(cmd.Environ(), config.EnvOverrides)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logs.stdout, logs.stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			stopStartedProcess(cmd)
		}
	}()
	client, err := newUpdateClient(config.Listen, config.TokenFile)
	if err != nil {
		return err
	}
	hash, err := fileSHA256(installed)
	if err != nil {
		return err
	}
	info, err := waitForStandalone(client, "", hash, 12*time.Second)
	if err != nil {
		return fmt.Errorf("standalone startup failed; inspect %s: %w", logs.stderr.Name(), err)
	}
	installedFile, err := os.Stat(installed)
	if err != nil {
		return err
	}
	runningFile, err := os.Stat(info.Executable)
	if err != nil {
		return err
	}
	if info.PID != cmd.Process.Pid || !os.SameFile(installedFile, runningFile) ||
		info.FileRoot != config.FileRoot || info.WorkingDirectory != config.WorkingDirectory {
		return errors.New("another server responded during standalone startup; refusing takeover")
	}
	if err := writeJSON(filepath.Join(stateDir, "config.json"), config); err != nil {
		return err
	}
	if err := cmd.Process.Release(); err != nil {
		return err
	}
	started = true
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"status": "started", "pid": info.PID, "server_id": info.ServerID,
		"stdout_log": logs.stdout.Name(), "stderr_log": logs.stderr.Name(),
	})
}

func controlStandalone(action string, args []string) error {
	flags := flag.NewFlagSet("terminal-mcp "+action, flag.ContinueOnError)
	var listen, tokenFile string
	flags.StringVar(&listen, "listen", "", "standalone loopback listening address")
	flags.StringVar(&tokenFile, "token-file", "", "bearer token file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s takes no positional arguments", action)
	}
	stateDir, err := standaloneDirectory()
	if err != nil {
		return err
	}
	config, err := initialStandaloneConfig(stateDir)
	if err != nil {
		return err
	}
	if listen != "" {
		config.Listen = listen
	}
	if tokenFile != "" {
		config.TokenFile = tokenFile
	}
	if err := httpguard.ValidateAddress(config.Listen); err != nil {
		return err
	}
	if !listenerInUse(config.Listen) {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "stopped"})
	}
	client, err := newUpdateClient(config.Listen, config.TokenFile)
	if err != nil {
		return err
	}
	info, err := client.runtime()
	if err != nil {
		return fmt.Errorf("no manageable standalone server at %s: %w", config.Listen, err)
	}
	if action == "status" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"status": "running", "pid": info.PID, "server_id": info.ServerID,
			"listen": info.Listen, "file_root": info.FileRoot, "audit": info.Audit,
			"stdout_log": info.StdoutLog, "stderr_log": info.StderrLog,
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(stateDir), "updates", "pending")); err == nil {
		return errors.New("standalone update in progress; stop refused")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := client.maintenance(info.ServerID, "begin"); err != nil {
		return fmt.Errorf("cannot stop a server with active sessions or mutations: %w", err)
	}
	if err := client.maintenance(info.ServerID, "shutdown"); err != nil {
		return fmt.Errorf("shutdown acknowledgement was lost; verify status manually: %w", err)
	}
	if err := waitForExit(info.PID, 20*time.Second); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"status": "stopped", "server_id": info.ServerID,
	})
}
