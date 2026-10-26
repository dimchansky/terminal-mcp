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
	Listen           string `json:"listen"`
	TokenFile        string `json:"token_file"`
	FileRoot         string `json:"file_root"`
	WorkingDirectory string `json:"working_directory"`
	Path             string `json:"path"`
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
		FileRoot: home, WorkingDirectory: cwd, Path: os.Getenv("PATH")}, nil
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
	var listen, root, token string
	flags.StringVar(&listen, "listen", "", "loopback listen address")
	flags.StringVar(&root, "file-root", "", "root for file tools")
	flags.StringVar(&token, "token-file", "", "private HTTP bearer token file")
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
	if _, err := os.Stat(installed); errors.Is(err, os.ErrNotExist) {
		if err := copyExecutable(self, installed, 0700); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		// Avoid silently replacing an installed binary with unknown code.
		current, err := fileSHA256(installed)
		if err != nil {
			return err
		}
		source, err := fileSHA256(self)
		if err != nil {
			return err
		}
		if current != source {
			return errors.New("an older standalone executable is already installed; stop and replace it explicitly")
		}
	}
	logFile, err := os.OpenFile(filepath.Join(stateDir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	if err := winsecure.CheckPrivate(logFile.Name()); err != nil {
		return err
	}
	cmd := exec.Command(installed, "serve", "--listen="+cfg.Listen,
		"--token-file="+cfg.TokenFile, "--file-root="+cfg.FileRoot)
	cmd.Dir = cfg.WorkingDirectory
	cmd.Env = append(withoutEnvToken(os.Environ()), "PATH="+cfg.Path)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
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
		return fmt.Errorf("standalone did not become ready; inspect server.log: %w", err)
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
	})
}

func windowsControl(action string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%s takes no arguments", action)
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
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "stopped"})
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
			"file_root": info.FileRoot, "listen": info.Listen, "audit": info.Audit,
		})
	}
	if err := client.maintenance(info.ServerID, "begin"); err != nil {
		return err
	}
	if err := client.maintenance(info.ServerID, "shutdown"); err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !windowsListenerInUse(cfg.Listen) {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "stopped"})
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("server did not stop within deadline")
}
