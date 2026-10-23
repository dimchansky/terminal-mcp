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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "terminal-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	transport := flag.String("transport", "http", "http (independent process) or stdio (owned by the MCP host)")
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP literal loopback address")
	tokenFile := flag.String("token-file", "", "0600 file holding the HTTP bearer token; otherwise use TERMINAL_MCP_TOKEN")
	fileRoot := flag.String("file-root", "", "Root directory for file tools (overrides configured file_root)")
	auditFile := flag.String("audit-file", "", "JSONL audit log path; default user state in HTTP mode; off disables logging")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *transport != "http" && *transport != "stdio" {
		return fmt.Errorf("transport must be http or stdio")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manager := session.New(terminal.Start)
	defer manager.Close()
	configPath := ""
	if runtime.GOOS == "darwin" && *transport == "http" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		configPath = filepath.Join(home, "Library", "Application Support", "LocalTerminalMCP", "config.json")
	}
	configuredRoot, err := fileRootFromConfig(*fileRoot, configPath)
	if err != nil {
		return err
	}
	files, err := filesystem.New(manager, configuredRoot)
	if err != nil {
		return err
	}
	defer files.Close()
	var recorder *audit.Log
	path := *auditFile
	if path == "" && *transport == "http" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
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
	if path != "" && path != "off" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return fmt.Errorf("prepare audit directory: %w", err)
		}
		recorder, err = audit.Open(path)
		if err != nil {
			return fmt.Errorf("open audit log: %w", err)
		}
		defer recorder.Close()
	}
	server := mcpserver.NewWithAudit(manager, recorder, files)
	if *transport == "stdio" {
		err := server.Run(ctx, &mcp.StdioTransport{MaxLineLength: httpguard.MaxBody})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	if err := httpguard.ValidateAddress(*listen); err != nil {
		return err
	}
	token, err := httpguard.LoadToken(*tokenFile)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.HTTP(server))
	mux.Handle("/admin/maintenance", mcpserver.Maintenance(manager))
	handler, err := httpguard.Wrap(mux, listener.Addr().String(), token)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	logger := log.New(os.Stderr, "terminal-mcp: ", log.LstdFlags)
	logger.Printf("listening at http://%s/mcp (full local user privileges)", listener.Addr())
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		manager.Close()
		if err := httpServer.Shutdown(shutdown); err != nil {
			_ = httpServer.Close()
			return err
		}
		return nil
	}
}
