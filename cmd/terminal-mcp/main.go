//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
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
	transport string
	listen    string
	tokenFile string
	fileRoot  string
	auditFile string
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
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("unexpected positional arguments")
	}
	if opts.transport != "http" && opts.transport != "stdio" {
		return opts, fmt.Errorf("transport must be http or stdio")
	}
	return opts, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWithContext(ctx, os.Args[1:], nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "terminal-mcp:", err)
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

	recorder := openAudit(opts.auditFile, opts.transport)
	if recorder != nil {
		defer func() {
			if err := recorder.Close(); err != nil {
				log.Printf("audit shutdown: %v", err)
			}
		}()
	}
	server := mcpserver.NewWithAudit(manager, recorder, files)
	if opts.transport == "stdio" {
		if stdio == nil {
			stdio = &mcp.StdioTransport{MaxLineLength: httpguard.MaxBody}
		}
		recordServerStart(recorder, manager.ID())
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
	recordServerStart(recorder, manager.ID())
	return serveHTTP(ctx, manager, server, listener, token)
}

func recordServerStart(recorder *audit.Log, serverID string) {
	if recorder == nil {
		return
	}
	if err := recorder.Write(audit.NewEvent(audit.Event{ServerID: serverID, Tool: "server", Phase: "server_start", Status: "ok"})); err != nil && !errors.Is(err, audit.ErrQueueFull) {
		log.Printf("audit server_start failure: %v", err)
	}
}

func openAudit(requestedPath, transport string) *audit.Log {
	path := requestedPath
	if path == "" && transport == "http" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Printf("audit unavailable; server will continue without audit: %v", err)
			return nil
		}
		if runtime.GOOS == "darwin" {
			path = filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP", "audit.jsonl")
		} else {
			base := os.Getenv("XDG_STATE_HOME")
			if base == "" {
				base = filepath.Join(home, ".local", "state")
			}
			path = filepath.Join(base, "terminal-mcp", "audit.jsonl")
		}
	}
	if path == "" || path == "off" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		log.Printf("audit unavailable; server will continue without audit: %v", err)
		return nil
	}
	recorder, err := audit.Open(path)
	if err != nil {
		log.Printf("audit unavailable; server will continue without audit: %v", err)
		return nil
	}
	return recorder
}

// newHTTPHandler constructs only HTTP routing and the authentication guard.
// Tests can exercise this handler with httptest without starting a service.
func newHTTPHandler(manager *session.Manager, server *mcp.Server, host, token string) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.HTTP(server))
	mux.Handle("/admin/maintenance", mcpserver.Maintenance(manager))
	return httpguard.Wrap(mux, host, token)
}

// serveHTTP owns the listener and session manager.
// On cancellation it stops accepting connections, lets admitted HTTP requests
// finish for up to five seconds, then stops any remaining managed processes.
func serveHTTP(ctx context.Context, manager *session.Manager, server *mcp.Server, listener net.Listener, token string) error {
	return serveHTTPWithTimeout(ctx, manager, server, listener, token, 5*time.Second)
}

// serveHTTPWithTimeout makes timeout behavior testable without a five-second
// sleep in every failure-path test. The shutdown deadline bounds HTTP draining,
// not the independent process-termination grace inside manager.Close.
func serveHTTPWithTimeout(ctx context.Context, manager *session.Manager, server *mcp.Server, listener net.Listener, token string, timeout time.Duration) error {
	defer func() { _ = listener.Close() }()
	defer manager.Close()
	handler, err := newHTTPHandler(manager, server, listener.Addr().String(), token)
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
	logger := log.New(os.Stderr, "terminal-mcp: ", log.LstdFlags)
	logger.Printf("listening at http://%s/mcp (full local user privileges)", listener.Addr())
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	select {
	case serveErr := <-done:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
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
