//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"terminalmcp/internal/mcpserver"
)

func runUpdateWorker(manifest string) error {
	var job updateJob
	if err := readJSON(manifest, &job); err != nil {
		return err
	}
	pending := filepath.Join(filepath.Dir(job.Directory), "pending")
	defer func() {
		if err := os.Remove(pending); err != nil {
			slog.Error("update: cannot remove pending marker", "error", err)
		}
	}()
	phase := func(status string) error {
		return writeJSON(filepath.Join(job.Directory, "status.json"),
			updateStatus{ID: job.ID, Status: status})
	}
	if err := phase("preflight"); err != nil {
		return err
	}
	result, err := performUpdate(job, phase)
	if err != nil {
		result = updateStatus{ID: job.ID, Status: "failed", Error: err.Error()}
	}
	result.ID = job.ID
	if writeErr := writeJSON(filepath.Join(job.Directory, "status.json"), result); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	// Only terminal states with a confirmed running server may discard
	// transient rollback/candidate/worker files. Failed or ambiguous jobs
	// retain their recovery evidence for manual investigation.
	if result.Status == "applied" || result.Status == "rolled_back" {
		if cleanupErr := cleanupCompletedJobPayloads(job.Directory); cleanupErr != nil {
			slog.Error("completed update payload cleanup failed", "error", cleanupErr)
		}
	}
	return err
}

func performUpdate(job updateJob, phase func(string) error) (updateStatus, error) {
	old := job.Runtime
	failure := updateStatus{Status: "failed"}
	auditPath, err := updateAuditPath(old)
	if err != nil {
		return failure, fmt.Errorf("resolve update audit path: %w", err)
	}
	// Do not overwrite an executable rebuilt in-place since this server started:
	// its on-disk bytes would no longer be a valid rollback image.
	original, err := fileSHA256(old.Executable)
	if err != nil {
		return failure, err
	}
	if original != old.ExecutableSHA256 {
		return failure, errors.New("running executable changed on disk; refused unsafe rollback")
	}
	targetInfo, err := os.Lstat(old.Executable)
	if err != nil {
		return failure, err
	}
	if !targetInfo.Mode().IsRegular() {
		return failure, errors.New("running executable is not a regular file")
	}
	previous := filepath.Join(job.Directory, "previous")
	if err := copyExecutable(old.Executable, previous, 0700); err != nil {
		return failure, err
	}
	backupHash, err := fileSHA256(previous)
	if err != nil {
		return failure, err
	}
	if backupHash != old.ExecutableSHA256 {
		return failure, errors.New("saved rollback executable differs from the running version")
	}
	candidate := filepath.Join(job.Directory, "candidate")
	if err := copyExecutable(job.Candidate, candidate, 0700); err != nil {
		return failure, err
	}
	candidateHash, err := fileSHA256(candidate)
	if err != nil {
		return failure, err
	}
	client, err := newUpdateClient(old.Listen, job.TokenFile)
	if err != nil {
		return failure, err
	}
	if err := verifyRunningInstance(client, old); err != nil {
		return failure, err
	}
	before, err := client.fingerprint()
	if err != nil {
		return failure, err
	}
	candidateFingerprint, err := preflightCandidate(job, candidate, candidateHash)
	if err != nil {
		return failure, fmt.Errorf("candidate preflight failed (nothing stopped): %w", err)
	}
	// Keep exactly one verified rollback executable, independently of the
	// job-scoped transient files. A failure here aborts BEFORE old shutdown.
	if _, err := saveRollbackSlot(filepath.Dir(job.Directory), previous, old.ExecutableSHA256); err != nil {
		return failure, fmt.Errorf("cannot preserve rollback executable: %w", err)
	}
	if err := phase("waiting_for_idle"); err != nil {
		return failure, err
	}
	if err := waitForIdleFence(client, old.ServerID); err != nil {
		return failure, err
	}
	fenced := true
	wasStopped := false
	switched := false
	var newProcess *exec.Cmd
	defer func() {
		if fenced {
			_ = client.maintenance(old.ServerID, "cancel")
		}
	}()
	if err := phase("stopping"); err != nil {
		return failure, err
	}
	shutdownErr := client.maintenance(old.ServerID, "shutdown")
	// An interrupted HTTP reply does not prove that shutdown was rejected.
	// Never restart on a busy port; first establish that the old PID has exited.
	if err := waitForExit(old.PID, 20*time.Second); err != nil {
		return failure, errors.Join(shutdownErr, err)
	}
	wasStopped = true
	fenced = false
	if shutdownErr != nil {
		// A lost acknowledgement may mean the user stopped the server manually.
		// Without proof of our own shutdown, do not resurrect it.
		return failure, fmt.Errorf("shutdown outcome ambiguous; restart manually: %w", shutdownErr)
	}
	err = phase("activating")
	if err == nil {
		var currentHash string
		currentHash, err = fileSHA256(old.Executable)
		if err == nil && currentHash != old.ExecutableSHA256 {
			err = errors.New("server binary was changed externally during the update")
		}
	}
	if err == nil {
		err = replaceExecutable(candidate, old.Executable, targetInfo.Mode().Perm())
		if err == nil {
			switched = true
		}
	}
	if err == nil {
		newProcess, err = launchStandalone(job, old.Executable, auditPath)
	}
	if err == nil {
		var current mcpserver.RuntimeInfo
		current, err = waitForStandalone(client, old.ServerID, candidateHash, 12*time.Second)
		if err == nil {
			var after string
			after, err = client.fingerprint()
			if err == nil && after != candidateFingerprint {
				err = errors.New("running MCP metadata differs from preflight candidate")
			}
			if err == nil {
				err = verifyRestartAudit(current, auditPath)
			}
			if err == nil {
				_ = newProcess.Process.Release()
				return updateStatus{Status: "applied", ServerID: current.ServerID,
					RefreshToolsRequired: before != after}, nil
			}
		}
	}
	if !wasStopped {
		return failure, err
	}
	// Rollback is needed after any failure that follows the old server's exit,
	// including failure to install the candidate or start the new process.
	if newProcess != nil {
		stopStartedProcess(newProcess)
	}
	currentHash, hashErr := fileSHA256(old.Executable)
	if switched || hashErr != nil || currentHash != old.ExecutableSHA256 {
		if restoreErr := replaceExecutable(previous, old.Executable, targetInfo.Mode().Perm()); restoreErr != nil {
			return failure, errors.Join(err, fmt.Errorf("rollback replacement failed: %w", restoreErr))
		}
	}
	// Old releases without a tunnel status cannot accept new tunnel flags.
	// A previous tunnel-aware binary may resume its own configured mode.
	legacyRollback := old.Tunnel.State == ""
	restored, restartErr := launchStandalone(job, old.Executable, auditPath, legacyRollback)
	if restartErr != nil {
		return failure, errors.Join(err, fmt.Errorf("rollback launch failed: %w", restartErr))
	}
	recovered, readyErr := waitForStandalone(client, old.ServerID, old.ExecutableSHA256, 12*time.Second)
	if readyErr != nil {
		stopStartedProcess(restored)
		return failure, errors.Join(err, fmt.Errorf("rollback readiness failed: %w", readyErr))
	}
	if auditErr := verifyRestartAudit(recovered, auditPath); auditErr != nil {
		stopStartedProcess(restored)
		return failure, errors.Join(err, fmt.Errorf("rollback audit path check failed: %w", auditErr))
	}
	_ = restored.Process.Release()
	return updateStatus{Status: "rolled_back", Error: err.Error(), ServerID: recovered.ServerID,
		TunnelDisabled: old.Tunnel.State != "disabled" && recovered.Tunnel.State == "disabled"}, nil
}

func verifyRunningInstance(client *updateClient, want mcpserver.RuntimeInfo) error {
	current, err := client.runtime()
	if err != nil {
		return err
	}
	if current.ServerID != want.ServerID || current.PID != want.PID ||
		current.Executable != want.Executable || current.ExecutableSHA256 != want.ExecutableSHA256 {
		return errors.New("running instance changed while preparing the update")
	}
	return nil
}

func waitForIdleFence(client *updateClient, serverID string) error {
	// Shut the admission gate *first*. New terminal processes and downloads
	// cannot postpone the update indefinitely while old work is draining.
	if err := client.maintenance(serverID, "begin"); err != nil {
		return err
	}
	for {
		err := client.maintenance(serverID, "ready")
		if err == nil {
			return nil
		}
		var httpErr *updateHTTPError
		if errors.As(err, &httpErr) && httpErr.status == http.StatusBadRequest &&
			strings.Contains(httpErr.body, "unknown action") {
			// Legacy servers (before asynchronous draining) have no "ready".
			// Their successful "begin" already guarantees an idle final fence.
			// This permits safely upgrading a previously installed instance.
			return nil
		}
		if !errors.As(err, &httpErr) || httpErr.status != http.StatusConflict {
			return err
		}
		info, infoErr := client.runtime()
		if infoErr != nil || info.ServerID != serverID {
			return errors.New("running server changed while draining for update")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func waitForExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) ||
			(err == nil && processZombie(pid)) {
			// On Linux an unreaped zombie still responds to kill(pid, 0),
			// despite having exited and released its sockets/descriptors.
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("old server did not exit; refusing to start another instance")
}

func waitForStandalone(client *updateClient, previousID, expectedHash string, timeout time.Duration, exited ...<-chan struct{}) (mcpserver.RuntimeInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(exited) != 0 {
			select {
			case <-exited[0]:
				return mcpserver.RuntimeInfo{}, errors.New("candidate process exited before readiness")
			default:
			}
		}
		info, err := client.runtime()
		if err == nil && info.ServerID != previousID && info.ExecutableSHA256 == expectedHash {
			switch info.Tunnel.State {
			case "start_failed", "shutdown_requested", "stopping", "stopped":
				// These are local SDK lifecycle errors, not temporary
				// control-plane connectivity failures. Do not install a
				// candidate that cannot keep its local tunnel runtime alive.
				return mcpserver.RuntimeInfo{}, fmt.Errorf("embedded tunnel lifecycle failed: %s", info.Tunnel.State)
			case "initializing":
				// The local HTTP server is already serving, but SDK.Start
				// has not completed. A first OpenAI poll is NOT required.
			default:
				id, toolErr := client.serverID()
				if toolErr == nil && id == info.ServerID {
					return info, nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return mcpserver.RuntimeInfo{}, errors.New("new server failed readiness or executable verification")
}

func preflightCandidate(job updateJob, candidate, expectedHash string) (string, error) {
	// A candidate must validate the selected tunnel's local configuration
	// without registering a competing control-plane poller. The regular
	// preflight below still runs with the tunnel disabled.
	if job.Runtime.Tunnel.State != "" && job.Runtime.Tunnel.State != "disabled" {
		opts, err := parseOptions(job.Runtime.Arguments)
		if err != nil {
			return "", fmt.Errorf("read current tunnel flags: %w", err)
		}
		if !opts.tunnel || opts.tunnelKeyFile == "" {
			return "", errors.New("embedded tunnel update requires a persistent private key file")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		check := exec.CommandContext(ctx, candidate, "_tunnel-check",
			"--tunnel-id="+opts.tunnelID, "--tunnel-key-file="+opts.tunnelKeyFile)
		check.Dir = job.Runtime.WorkingDirectory
		check.Env = withoutEnvToken(check.Environ())
		if err := check.Run(); err != nil {
			return "", fmt.Errorf("candidate tunnel configuration preflight: %w", err)
		}
	}
	address, err := reserveProbePort()
	if err != nil {
		return "", err
	}
	args := withoutTunnelFlags(job.Runtime.Arguments)
	args = append(args, "--listen="+address, "--file-root="+job.Runtime.FileRoot,
		"--token-file="+job.TokenFile, "--audit-file=off")
	logFile, err := os.OpenFile(filepath.Join(job.Directory, "preflight.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer func() { _ = logFile.Close() }()
	cmd := exec.Command(candidate, args...)
	cmd.Dir = job.Runtime.WorkingDirectory
	cmd.Env = withoutEnvToken(cmd.Environ())
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	defer func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	client, err := newUpdateClient(address, job.TokenFile)
	if err != nil {
		return "", err
	}
	info, err := waitForStandalone(client, "", expectedHash, 10*time.Second, done)
	if err != nil {
		return "", err
	}
	if info.FileRoot != job.Runtime.FileRoot {
		return "", errors.New("candidate file root differs from running server")
	}
	fingerprint, err := client.fingerprint()
	if err != nil {
		return "", err
	}
	var executed struct {
		Receipt struct {
			Status string `json:"status"`
		} `json:"receipt"`
		Observation struct {
			Session struct {
				Closed bool `json:"closed"`
				Exit   struct {
					Code *int `json:"code"`
				} `json:"exit"`
			} `json:"session"`
		} `json:"observation"`
	}
	if err := client.tool("terminal_start", map[string]any{
		"server_id": info.ServerID, "operation_id": "update-preflight",
		"argv": []string{"/usr/bin/true"}, "close_on_exit": true,
		"wait_for": "completion", "wait_ms": 2000,
	}, &executed); err != nil {
		return "", err
	}
	if executed.Receipt.Status != "applied" || !executed.Observation.Session.Closed ||
		executed.Observation.Session.Exit.Code == nil || *executed.Observation.Session.Exit.Code != 0 {
		return "", errors.New("candidate cannot run and reap an ordinary process")
	}
	var listing any
	if err := client.tool("file_list", map[string]any{
		"path": job.Runtime.FileRoot, "limit": 1,
	}, &listing); err != nil {
		return "", err
	}
	return fingerprint, nil
}

// withoutTunnelFlags ensures preflight and legacy rollback never register a
// second instance of the same tunnel or pass unsupported flags to old code.
func withoutTunnelFlags(original []string) []string {
	out := make([]string, 0, len(original))
	for i := 0; i < len(original); i++ {
		arg := original[i]
		switch {
		case arg == "--tunnel", strings.HasPrefix(arg, "--tunnel="),
			strings.HasPrefix(arg, "--tunnel-id="), strings.HasPrefix(arg, "--tunnel-key-file="):
			continue
		case arg == "--tunnel-id", arg == "--tunnel-key-file":
			// The flag parser also accepts: --tunnel-id VALUE.
			// Remove both the name and the value for old binaries.
			if i+1 < len(original) {
				i++
			}
			continue
		default:
			out = append(out, arg)
		}
	}
	return out
}

func launchStandalone(job updateJob, executable, auditPath string, disableTunnel ...bool) (*exec.Cmd, error) {
	args := append([]string(nil), job.Runtime.Arguments...)
	if len(disableTunnel) != 0 && disableTunnel[0] {
		args = withoutTunnelFlags(args)
	}
	args = append(args, "--listen="+job.Runtime.Listen, "--file-root="+job.Runtime.FileRoot,
		"--token-file="+job.TokenFile, "--audit-file="+auditPath)
	cmd := exec.Command(executable, args...)
	cmd.Dir = job.Runtime.WorkingDirectory
	cmd.Env = withoutEnvToken(cmd.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logs, err := openServerLogs()
	if err != nil {
		return nil, err
	}
	defer func() { _ = logs.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer func() { _ = devnull.Close() }()
	cmd.Stdin = devnull
	cmd.Stdout, cmd.Stderr = logs.stdout, logs.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func stopStartedProcess(cmd *exec.Cmd) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func replaceExecutable(source, target string, mode os.FileMode) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	temp, err := os.CreateTemp(filepath.Dir(target), ".terminal-mcp-update-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	_, err = io.Copy(temp, src)
	if err == nil {
		err = temp.Chmod(mode)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), target)
}
