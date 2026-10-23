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
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	server := mcpserver.New(manager)
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
	handler, err := httpguard.Wrap(mcpserver.HTTP(server), listener.Addr().String(), token)
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
