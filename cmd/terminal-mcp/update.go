//go:build linux || darwin

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"terminalmcp/internal/mcpserver"
)

type updateJob struct {
	ID        string                `json:"id"`
	Directory string                `json:"directory"`
	Candidate string                `json:"candidate"`
	TokenFile string                `json:"token_file"`
	Runtime   mcpserver.RuntimeInfo `json:"runtime"`
}

type updateStatus struct {
	ID                   string `json:"id"`
	Status               string `json:"status"`
	Error                string `json:"error,omitempty"`
	ServerID             string `json:"server_id,omitempty"`
	RefreshToolsRequired bool   `json:"refresh_tools_required,omitempty"`
}

func updateCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("missing update command")
	}
	switch args[0] {
	case "update":
		return queueUpdate(args[1:])
	case "update-status":
		return showUpdateStatus(args[1:])
	case "_update-worker":
		if len(args) != 2 {
			return errors.New("_update-worker requires manifest path")
		}
		return runUpdateWorker(args[1])
	default:
		return fmt.Errorf("unknown update command: %s", args[0])
	}
}

func updateDir(explicit string) (string, error) {
	if explicit == "" {
		return defaultUpdateRoot()
	}
	return filepath.Abs(explicit)
}

func queueUpdate(args []string) error {
	flags := flag.NewFlagSet("terminal-mcp update", flag.ContinueOnError)
	var candidate, listen, tokenFile, state string
	flags.StringVar(&candidate, "candidate", "", "path to a separately built candidate binary")
	flags.StringVar(&listen, "listen", "127.0.0.1:8090", "address of the running standalone MCP server")
	flags.StringVar(&tokenFile, "token-file", "", "bearer token file used by the running server")
	flags.StringVar(&state, "state-dir", "", "private updater state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || candidate == "" {
		return errors.New("usage: terminal-mcp update --candidate=/absolute/path [--listen=127.0.0.1:8090] [--token-file=...]")
	}
	var err error
	if tokenFile == "" {
		tokenFile, err = defaultTokenFile()
		if err != nil {
			return err
		}
	}
	tokenFile, err = filepath.Abs(tokenFile)
	if err != nil {
		return err
	}
	client, err := newUpdateClient(listen, tokenFile)
	if err != nil {
		return err
	}
	info, err := client.runtime()
	if err != nil {
		return fmt.Errorf("running server is not a standalone 'serve' instance: %w", err)
	}
	if info.Listen != listen {
		return fmt.Errorf("server listen address changed (%s)", info.Listen)
	}
	if info.EnvToken {
		return errors.New("self-update requires a token file; restart the server without TERMINAL_MCP_TOKEN")
	}
	serverID, err := client.serverID()
	if err != nil {
		return err
	}
	if info.ServerID != serverID {
		return errors.New("server changed during update preparation")
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return err
	}
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return err
	}
	src, err := os.Stat(info.Executable)
	if err != nil {
		return err
	}
	dst, err := os.Stat(candidate)
	if err != nil {
		return err
	}
	if !dst.Mode().IsRegular() || dst.Mode().Perm()&0111 == 0 || os.SameFile(src, dst) {
		return errors.New("candidate must be a separate executable regular file")
	}
	currentHash, err := fileSHA256(info.Executable)
	if err != nil {
		return err
	}
	if currentHash != info.ExecutableSHA256 {
		return errors.New("running executable was replaced after startup; build candidates to a separate path")
	}
	root, err := updateDir(state)
	if err != nil {
		return err
	}
	if err = secureUpdateDir(root); err != nil {
		return err
	}
	pending := filepath.Join(root, "pending")
	if err = os.Mkdir(pending, 0700); err != nil {
		return fmt.Errorf("another update may be pending; inspect update-status: %w", err)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = os.Remove(pending)
		}
	}()
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return err
	}
	job := updateJob{ID: hex.EncodeToString(id[:]), Candidate: candidate, TokenFile: tokenFile, Runtime: info}
	job.Directory = filepath.Join(root, job.ID)
	if err := os.Mkdir(job.Directory, 0700); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(job.Directory, "manifest.json"), job); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(job.Directory, "status.json"), updateStatus{ID: job.ID, Status: "queued"}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(root, "latest.json"), map[string]string{"id": job.ID}); err != nil {
		return err
	}
	// The worker must survive a restart of the MCP process that launched this
	// command. Copy the current executable and detach from its process group.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	worker := filepath.Join(job.Directory, "worker")
	if err := copyExecutable(self, worker, 0700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(job.Directory, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = devnull.Close() }()
	cmd := exec.Command(worker, "_update-worker", filepath.Join(job.Directory, "manifest.json"))
	cmd.Dir = job.Directory
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = withoutEnvToken(os.Environ())
	if err := cmd.Start(); err != nil {
		_ = writeJSON(filepath.Join(job.Directory, "status.json"),
			updateStatus{ID: job.ID, Status: "failed", Error: err.Error()})
		return err
	}
	_ = cmd.Process.Release()
	handedOff = true
	return json.NewEncoder(os.Stdout).Encode(map[string]string{
		"status": "queued", "id": job.ID, "status_file": filepath.Join(job.Directory, "status.json"),
	})
}

func showUpdateStatus(args []string) error {
	flags := flag.NewFlagSet("terminal-mcp update-status", flag.ContinueOnError)
	var state string
	flags.StringVar(&state, "state-dir", "", "private updater state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("update-status takes no positional arguments")
	}
	root, err := updateDir(state)
	if err != nil {
		return err
	}
	var latest struct {
		ID string `json:"id"`
	}
	if err := readJSON(filepath.Join(root, "latest.json"), &latest); err != nil {
		return err
	}
	var status updateStatus
	if err := readJSON(filepath.Join(root, latest.ID, "status.json"), &status); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}

func withoutEnvToken(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		if !strings.HasPrefix(item, "TERMINAL_MCP_TOKEN=") {
			out = append(out, item)
		}
	}
	return out
}

func readJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".update-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }() // Best effort, including after successful rename.
	if _, err = temp.Write(append(data, '\n')); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), path)
}

func copyExecutable(source, destination string, mode os.FileMode) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular executable: %s", source)
	}
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	copied := false
	defer func() {
		if !copied {
			_ = os.Remove(destination)
		}
	}()
	_, copyErr := io.Copy(dst, src)
	if copyErr == nil {
		copyErr = dst.Chmod(mode)
	}
	if copyErr == nil {
		copyErr = dst.Sync()
	}
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	copied = true
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
