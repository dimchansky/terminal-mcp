package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/httpguard"
	"terminalmcp/internal/mcpserver"
	"terminalmcp/internal/session"
	"terminalmcp/internal/terminal"
)

type options struct {
	transport     string
	listen        string
	tokenFile     string
	fileRoot      string
	auditFile     string
	auditMaxBytes int64
}

func parseOptions(args []string) (options, error) {
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	var opts options
	flags := flag.NewFlagSet("terminal-mcp", flag.ContinueOnError)
	flags.StringVar(&opts.transport, "transport", "http", "http (independent process) or stdio (owned by the MCP host)")
	flags.StringVar(&opts.listen, "listen", "127.0.0.1:8090", "HTTP literal loopback address")
	flags.StringVar(&opts.tokenFile, "token-file", "", "HTTP bearer token file (0600); defaults to TERMINAL_MCP_TOKEN or a generated user token")
	flags.StringVar(&opts.fileRoot, "file-root", "", "Root directory for file tools (overrides configured file_root)")
	flags.StringVar(&opts.auditFile, "audit-file", "", "JSONL audit log path; default user state in HTTP mode; off disables logging")
	flags.Int64Var(&opts.auditMaxBytes, "audit-max-bytes", audit.DefaultMaxHistoryBytes, "optional hard limit for all audit history in bytes (0 = unlimited, the default)")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("unexpected positional arguments")
	}
	if opts.transport != "http" && opts.transport != "stdio" {
		return opts, fmt.Errorf("transport must be http or stdio")
	}
	if opts.auditMaxBytes < 0 {
		return opts, fmt.Errorf("audit-max-bytes must be non-negative")
	}
	return opts, nil
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start", "status", "stop":
			if err := standaloneCommand(os.Args[1:]); err != nil {
				slog.Error("terminal-mcp standalone command failed", "error", err)
				os.Exit(1)
			}
			return
		case "update", "update-status", "cleanup", "_update-worker":
			if err := updateCommand(os.Args[1:]); err != nil {
				slog.Error("terminal-mcp updater command failed", "error", err)
				os.Exit(1)
			}
			return
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWithContext(ctx, os.Args[1:], nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("terminal-mcp server failed", "error", err)
		os.Exit(1)
	}
}

// runWithContext uses a supplied context for shutdown. A nil stdio transport
// selects the real process stdin/stdout. Tests can supply an in-memory transport.
func runWithContext(ctx context.Context, args []string, stdio mcp.Transport) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	manager := session.New(terminal.Start)
	defer manager.Close()

	configPath := ""
	if runtime.GOOS == "darwin" && opts.transport == "http" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		configPath = filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP", "config.json")
	}
	configuredRoot, err := fileRootFromConfig(opts.fileRoot, configPath)
	if err != nil {
		return err
	}
	files, err := filesystem.New(manager, configuredRoot)
	if err != nil {
		return err
	}
	defer files.Close()

	recorder, err := openAuditWithLimit(opts.auditFile, opts.transport, opts.auditMaxBytes)
	if err != nil {
		return fmt.Errorf("required audit initialization failed: %w", err)
	}
	if recorder != nil {
		defer func() {
			// Stdio has no serveHTTP shutdown defer. Drain independently
			// executing mutations before closing their audit destination.
			manager.Close()
			if err := recorder.Close(); err != nil {
				slog.Error("audit shutdown failed", "error", err)
			}
		}()
	}
	server := mcpserver.NewWithAudit(manager, recorder, files)
	if opts.transport == "stdio" {
		if stdio == nil {
			stdio = &mcp.StdioTransport{MaxLineLength: httpguard.MaxBody}
		}
		if err := recordServerStart(recorder, manager.ID()); err != nil {
			return fmt.Errorf("required audit startup event failed: %w", err)
		}
		err := server.Run(ctx, stdio)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	if err := httpguard.ValidateAddress(opts.listen); err != nil {
		return err
	}
	token, err := loadHTTPToken(opts.tokenFile)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return err
	}
	if err := recordServerStart(recorder, manager.ID()); err != nil {
		_ = listener.Close()
		return fmt.Errorf("required audit startup event failed: %w", err)
	}
	if len(args) == 0 || args[0] != "serve" {
		return serveHTTP(ctx, manager, server, listener, token)
	}
	// Only explicitly launched standalone servers can shut themselves down
	// for an update. Existing launchd-managed instances remain unchanged.
	info, err := standaloneInfo(args, manager.ID(), listener.Addr().String(), files.Root(),
		opts.tokenFile == "" && os.Getenv("TERMINAL_MCP_TOKEN") != "")
	if err != nil {
		_ = listener.Close()
		return err
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	return serveHTTP(serveCtx, manager, server, listener, token, &updateServer{
		info: info, recorder: recorder, shutdown: cancel,
		force: func() {
			files.AbortTransfers()
			manager.AbortManagedSessions()
			cancel()
		},
	})
}

func recordServerStart(recorder *audit.Log, serverID string) error {
	if recorder == nil {
		return nil
	}
	return recorder.Start(audit.NewEvent(audit.Event{
		ServerID: serverID, Tool: "server", Phase: "server_start", Status: "ok",
	}))
}

func openAudit(requestedPath, transport string) (*audit.Log, error) {
	return openAuditWithLimit(requestedPath, transport, audit.DefaultMaxHistoryBytes)
}

func openAuditWithLimit(requestedPath, transport string, maxBytes int64) (*audit.Log, error) {
	path := requestedPath
	if path == "" {
		if transport != "http" {
			return nil, nil // Stdio defaults to intentional opt-out.
		}
		var err error
		path, err = defaultAuditPath()
		if err != nil {
			return nil, fmt.Errorf("resolve default audit path: %w", err)
		}
	}
	if path == "off" {
		return nil, nil
	}
	if err := audit.PrepareDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("prepare audit directory %q: %w", filepath.Dir(path), err)
	}
	// Explicit custom paths remain untouched. Only the platform default is
	// cold-migrated, with both old/new locks held until the server stops.
	defaultPath, err := defaultAuditPath()
	if err != nil {
		return nil, err
	}
	var recorder *audit.Log
	if sameAuditPath(path, defaultPath) {
		legacy, legacyErr := legacyAuditPath()
		if legacyErr != nil {
			return nil, legacyErr
		}
		recorder, err = audit.OpenWithMigrationLimit(path, legacy, maxBytes)
	} else {
		recorder, err = audit.OpenWithLimit(path, maxBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("open required audit file %q: %w", path, err)
	}
	return recorder, nil
}

type updateServer struct {
	info     mcpserver.RuntimeInfo
	recorder *audit.Log
	shutdown context.CancelFunc
	force    func()
}

func standaloneInfo(args []string, serverID, listen, root string, envToken bool) (mcpserver.RuntimeInfo, error) {
	executable, err := os.Executable()
	if err != nil {
		return mcpserver.RuntimeInfo{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return mcpserver.RuntimeInfo{}, err
	}
	file, err := os.Open(executable)
	if err != nil {
		return mcpserver.RuntimeInfo{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return mcpserver.RuntimeInfo{}, copyErr
	}
	if closeErr != nil {
		return mcpserver.RuntimeInfo{}, closeErr
	}
	cwd, err := os.Getwd()
	if err != nil {
		return mcpserver.RuntimeInfo{}, err
	}
	stdoutLog, stderrLog := activeServerLogPaths()
	return mcpserver.RuntimeInfo{
		ServerID: serverID, Executable: executable,
		ExecutableSHA256: hex.EncodeToString(hash.Sum(nil)), Revision: sourceRevision(),
		WorkingDirectory: cwd, FileRoot: root, Listen: listen,
		Arguments: append([]string(nil), args...), PID: os.Getpid(),
		EnvToken: envToken, StdoutLog: stdoutLog, StderrLog: stderrLog,
	}, nil
}

// newHTTPHandler constructs only HTTP routing and the authentication guard.
// Tests can exercise this handler with httptest without starting a service.
func newHTTPHandler(manager *session.Manager, server *mcp.Server, host, token string, standalone ...*updateServer) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.HTTP(server))
	if len(standalone) != 0 && standalone[0] != nil {
		state := standalone[0]
		mux.Handle("/admin/maintenance", mcpserver.Maintenance(manager, state.shutdown, state.force))
		mux.HandleFunc("GET /admin/runtime", func(w http.ResponseWriter, r *http.Request) {
			// Do not wait for the audit sink here. A status query must work
			// even while the disk is failing or a tool write is blocked.
			info := state.info
			info.Audit = state.recorder.Status()
			info.DrainMode = manager.DrainMode()
			info.Draining = info.DrainMode != ""
			state, _ := manager.List(r.Context(), session.ListArgs{})
			info.ActiveOperations = state.ActiveOperations
			info.ActiveSessions = state.Counts.Running
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(info)
		})
	} else {
		mux.Handle("/admin/maintenance", mcpserver.Maintenance(manager))
	}
	return httpguard.Wrap(mux, host, token)
}

// serveHTTP owns the listener and session manager. Normal cancellation first
// closes admission for new work, then drains accepted operations and managed
// processes without an arbitrary shutdown deadline. A forced stop is explicit.
func serveHTTP(ctx context.Context, manager *session.Manager, server *mcp.Server, listener net.Listener, token string, standalone ...*updateServer) error {
	return serveHTTPWithTimeout(ctx, manager, server, listener, token, 0, standalone...)
}

// serveHTTPWithTimeout preserves an explicit bounded-close test hook. A
// positive timeout exercises forced HTTP drain for failure-path tests; a zero
// timeout follows the normal unbounded graceful shutdown contract.
func serveHTTPWithTimeout(ctx context.Context, manager *session.Manager, server *mcp.Server, listener net.Listener, token string, timeout time.Duration, standalone ...*updateServer) error {
	defer func() { _ = listener.Close() }()
	defer manager.Close()
	handler, err := newHTTPHandler(manager, server, listener.Addr().String(), token, standalone...)
	if err != nil {
		return err
	}
	// Cancellation signals the listener to drain. It must not cancel already
	// admitted tool calls before they can return a result to the client.
	requestBase := context.WithoutCancel(ctx)
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return requestBase },
	}
	slog.Info("listening at http://" + listener.Addr().String() + "/mcp (full local user privileges)")
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	select {
	case serveErr := <-done:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	case <-ctx.Done():
		if timeout == 0 {
			// SIGTERM/SIGINT must protect active work just like "stop". During
			// the drain, the listener remains available for status and for
			// input/signal/close actions on existing managed processes.
			if manager.DrainMode() == "" {
				if _, err := manager.BeginDrain(manager.ID(), "stop"); err != nil {
					return err
				}
			}
			if manager.DrainMode() == "stop" {
				for {
					if manager.TryFinishDrain(manager.ID(), "stop") == nil {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
			}
		}
		shutdownCtx := context.Background()
		cancel := func() {}
		if timeout > 0 {
			shutdownCtx, cancel = context.WithTimeout(shutdownCtx, timeout)
		}
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		cancel()
		var closeErr error
		if shutdownErr != nil {
			// Deadline exceeded: interrupt requests that did not finish.
			closeErr = httpServer.Close()
		}
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(shutdownErr, closeErr, serveErr)
	}
}
