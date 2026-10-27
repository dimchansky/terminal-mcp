package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	tunnelclient "github.com/openai/tunnel-client"
	tunnelcfg "github.com/openai/tunnel-client/pkg/config"

	"terminalmcp/internal/mcpserver"
)

type tunnelConfig struct {
	id, apiKey          string
	controlPlaneBaseURL string // Test-only fake control plane; production uses SDK default.
	pollTimeout         time.Duration
	logWriter           io.Writer // Injectable for isolated tests; nil uses SDK stderr.
}

// loadTunnelConfig validates local credentials before opening an HTTP listener.
// Standalone services use a private key file; foreground serve also supports an
// environment key for short-lived runs. Neither the key nor its contents are
// included in runtime status or process command-line arguments.
func loadTunnelConfig(opts options) (tunnelConfig, error) {
	if !opts.tunnel {
		return tunnelConfig{}, nil
	}
	id := strings.TrimSpace(opts.tunnelID)
	if id == "" {
		id = strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID"))
	}
	if id == "" {
		return tunnelConfig{}, errors.New("tunnel enabled but CONTROL_PLANE_TUNNEL_ID / --tunnel-id is missing")
	}
	var key string
	if opts.tunnelKeyFile != "" {
		var err error
		key, err = loadTunnelKeyFile(opts.tunnelKeyFile)
		if err != nil {
			return tunnelConfig{}, err
		}
	} else {
		key = strings.TrimSpace(os.Getenv("CONTROL_PLANE_API_KEY"))
	}
	if key == "" {
		return tunnelConfig{}, errors.New("tunnel enabled but runtime API key is missing; supply --tunnel-key-file")
	}
	if err := tunnelcfg.ValidateTunnelID(id); err != nil {
		return tunnelConfig{}, fmt.Errorf("invalid tunnel ID: %w", err)
	}
	return tunnelConfig{id: id, apiKey: key}, nil
}

// resolveStandaloneTunnelConfig never persists API key bytes or inherits them
// into detached service/update processes. The operator provisions the file.
func resolveStandaloneTunnelConfig(enabled bool, id, keyFile string) (string, string, error) {
	if !enabled {
		return id, keyFile, nil
	}
	if id == "" {
		id = strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID"))
	}
	if id == "" {
		return "", "", errors.New("tunnel ID required: --tunnel-id or CONTROL_PLANE_TUNNEL_ID")
	}
	if err := tunnelcfg.ValidateTunnelID(id); err != nil {
		return "", "", fmt.Errorf("invalid tunnel ID: %w", err)
	}
	if keyFile == "" {
		return "", "", errors.New("background tunnel requires --tunnel-key-file pointing to a private runtime API key file")
	}
	abs, err := filepath.Abs(keyFile)
	if err != nil {
		return "", "", err
	}
	if _, err := loadTunnelKeyFile(abs); err != nil {
		return "", "", err
	}
	return id, abs, nil
}

func loadTunnelKeyFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("tunnel key file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("tunnel key must be a regular file, not a symlink: %s", abs)
	}
	if err := checkPrivateToken(abs, info); err != nil {
		return "", fmt.Errorf("tunnel key permissions: %w", err)
	}
	file, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("open tunnel key: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect tunnel key: %w", err)
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return "", errors.New("tunnel key changed while being validated")
	}
	if err := checkPrivateToken(abs, openedInfo); err != nil {
		return "", fmt.Errorf("tunnel key permissions: %w", err)
	}
	const maxKeyBytes = 16 << 10
	if openedInfo.Size() > maxKeyBytes {
		return "", errors.New("tunnel key file is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxKeyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read tunnel key file: %w", err)
	}
	if len(data) > maxKeyBytes {
		return "", errors.New("tunnel key file is too large")
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("tunnel key file is empty: %s", abs)
	}
	if strings.ContainsAny(key, "\x00\r\n") || strings.HasPrefix(key, "\ufeff") {
		return "", errors.New("invalid tunnel runtime key file encoding (expected plain UTF-8 without BOM)")
	}
	return key, nil
}

// tunnelRoundTripper pins SDK-originated MCP traffic to the exact local /mcp
// endpoint. Remote authorization and Origin headers must never replace the
// locally authenticated principal or reach administrative paths.
type tunnelRoundTripper struct {
	base           http.RoundTripper
	address, token string
}

func (t tunnelRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" || req.URL.Host != t.address ||
		req.URL.Path != "/mcp" || req.URL.RawQuery != "" || req.URL.User != nil {
		return nil, errors.New("tunnel MCP transport attempted to access an unexpected local endpoint")
	}
	copy := req.Clone(req.Context())
	copy.Header = req.Header.Clone()
	copy.Header.Del("Origin")
	copy.Header.Del("Forwarded")
	for name := range copy.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
			copy.Header.Del(name)
		}
	}
	copy.Header.Set("Authorization", "Bearer "+t.token)
	copy.Host = t.address
	return t.base.RoundTrip(copy)
}

const (
	tunnelInitializing int32 = iota
	tunnelStarting
	tunnelReadyOnce
	tunnelShutdownRequested
	tunnelStartFailed
	tunnelStopping
	tunnelStopped
)

type tunnelRuntime struct {
	client    *tunnelclient.Client
	transport *http.Transport
	state     atomic.Int32
	stopOnce  sync.Once
	stopCh    chan struct{}
	watchers  sync.WaitGroup
}

// newTunnelRuntime prepares one embedded SDK client and a loopback-only HTTP
// transport. It does not dial or register a remote control-plane poller.
func newTunnelRuntime(cfg tunnelConfig, address, token string) (*tunnelRuntime, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	address = strings.TrimSpace(address)
	transport := &mcp.StreamableClientTransport{
		Endpoint: "http://" + address + "/mcp",
		HTTPClient: &http.Client{
			Transport:     tunnelRoundTripper{base: base, address: address, token: token},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	client, err := tunnelclient.New(tunnelclient.Config{
		TunnelID: cfg.id, APIKey: cfg.apiKey, ControlPlaneBaseURL: cfg.controlPlaneBaseURL,
		PollTimeout: cfg.pollTimeout, LogWriter: cfg.logWriter,
	}, transport)
	if err != nil {
		return nil, err
	}
	return &tunnelRuntime{client: client, transport: base, stopCh: make(chan struct{})}, nil
}

func (t *tunnelRuntime) start() {
	if t == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.client.Start(ctx); err != nil {
		t.state.Store(tunnelStartFailed)
		slog.Error("embedded MCP tunnel failed to start; local MCP remains available", "error", err)
		return
	}
	t.state.Store(tunnelStarting)
	t.watchers.Add(1)
	go func() {
		defer t.watchers.Done()
		select {
		case <-t.client.Ready():
			if t.state.CompareAndSwap(tunnelStarting, tunnelReadyOnce) {
				slog.Info("embedded MCP tunnel completed first control-plane poll")
			}
		case <-t.client.Done():
			t.shutdownRequested()
			return
		case <-t.stopCh:
			return
		}
		select {
		case <-t.client.Done():
			t.shutdownRequested()
		case <-t.stopCh:
		}
	}()
}

func (t *tunnelRuntime) shutdownRequested() {
	for {
		state := t.state.Load()
		if state == tunnelStopping || state == tunnelStopped {
			return
		}
		if t.state.CompareAndSwap(state, tunnelShutdownRequested) {
			slog.Warn("embedded MCP tunnel SDK requested shutdown; local MCP remains available")
			return
		}
	}
}

func (t *tunnelRuntime) stop() error {
	if t == nil {
		return nil
	}
	t.state.Store(tunnelStopping)
	t.stopOnce.Do(func() { close(t.stopCh) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// SDK Stop is documented as safe even when Start never succeeded.
	err := t.client.Stop(ctx)
	t.transport.CloseIdleConnections()
	t.watchers.Wait()
	t.state.Store(tunnelStopped)
	return err
}

func (t *tunnelRuntime) status() mcpserver.TunnelStatus {
	if t == nil {
		return mcpserver.TunnelStatus{State: "disabled"}
	}
	switch t.state.Load() {
	case tunnelInitializing:
		return mcpserver.TunnelStatus{State: "initializing"}
	case tunnelReadyOnce:
		return mcpserver.TunnelStatus{State: "first_poll_succeeded"}
	case tunnelShutdownRequested:
		return mcpserver.TunnelStatus{State: "shutdown_requested"}
	case tunnelStartFailed:
		return mcpserver.TunnelStatus{State: "start_failed"}
	case tunnelStopping:
		return mcpserver.TunnelStatus{State: "stopping"}
	case tunnelStopped:
		return mcpserver.TunnelStatus{State: "stopped"}
	default:
		return mcpserver.TunnelStatus{State: "starting"}
	}
}

// validateTunnelCandidate is a private self-update probe: it exercises the
// candidate's exact credential loading and SDK construction, but never starts
// polling the OpenAI control plane. The updater calls it before stopping the
// currently active server; running two pollers with the same tunnel ID is unsafe.
func validateTunnelCandidate(args []string) error {
	flags := flag.NewFlagSet("_tunnel-check", flag.ContinueOnError)
	id := flags.String("tunnel-id", "", "existing tunnel ID")
	keyFile := flags.String("tunnel-key-file", "", "private runtime credential file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *id == "" || *keyFile == "" {
		return errors.New("_tunnel-check requires --tunnel-id and --tunnel-key-file")
	}
	cfg, err := loadTunnelConfig(options{tunnel: true, tunnelID: *id, tunnelKeyFile: *keyFile})
	if err != nil {
		return err
	}
	client, err := newTunnelRuntime(cfg, "127.0.0.1:1", "preflight-local-token")
	if err != nil {
		return fmt.Errorf("construct embedded tunnel runtime: %w", err)
	}
	return client.stop()
}
