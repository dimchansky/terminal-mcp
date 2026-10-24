//go:build linux || darwin

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"terminalmcp/internal/httpguard"
	"terminalmcp/internal/mcpserver"
)

type updateClient struct {
	listen string
	token  string
	http   *http.Client
}

type updateHTTPError struct {
	status int
	body   string
}

func (e *updateHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

func newUpdateClient(listen, tokenFile string) (*updateClient, error) {
	if err := httpguard.ValidateAddress(listen); err != nil {
		return nil, err
	}
	token, err := httpguard.LoadToken(tokenFile)
	if err != nil {
		return nil, err
	}
	return &updateClient{listen: listen, token: token, http: &http.Client{
		Timeout:       3 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *updateClient) request(method, path string, payload any, result any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://"+c.listen+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("update response exceeds 1 MiB")
	}
	if resp.StatusCode != http.StatusOK {
		return &updateHTTPError{status: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	if result != nil {
		return json.Unmarshal(data, result)
	}
	return nil
}

func (c *updateClient) runtime() (mcpserver.RuntimeInfo, error) {
	var info mcpserver.RuntimeInfo
	err := c.request(http.MethodGet, "/admin/runtime", nil, &info)
	if err != nil {
		return info, err
	}
	if len(info.Arguments) == 0 || info.Arguments[0] != "serve" ||
		info.Executable == "" || info.ServerID == "" || info.PID < 2 ||
		!filepath.IsAbs(info.Executable) || !filepath.IsAbs(info.WorkingDirectory) {
		return info, errors.New("invalid standalone runtime response")
	}
	return info, nil
}

func (c *updateClient) maintenance(id, action string) error {
	return c.request(http.MethodPost, "/admin/maintenance",
		map[string]string{"server_id": id, "action": action}, nil)
}

func (c *updateClient) rpc(method string, params any, out any) error {
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.request(http.MethodPost, "/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	}, &response); err != nil {
		return err
	}
	if response.Error != nil {
		return errors.New(response.Error.Message)
	}
	if len(response.Result) == 0 {
		return errors.New("MCP response has no result")
	}
	return json.Unmarshal(response.Result, out)
}

func (c *updateClient) tool(name string, args any, out any) error {
	var result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := c.rpc("tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return err
	}
	if result.IsError {
		return fmt.Errorf("MCP %s failed", name)
	}
	if len(result.StructuredContent) != 0 {
		return json.Unmarshal(result.StructuredContent, out)
	}
	for _, content := range result.Content {
		if content.Type == "text" {
			return json.Unmarshal([]byte(content.Text), out)
		}
	}
	return fmt.Errorf("MCP %s returned no structured content", name)
}

func (c *updateClient) fingerprint() (string, error) {
	var initial struct {
		Instructions any `json:"instructions"`
	}
	if err := c.rpc("initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "terminal-mcp-updater", "version": "1"},
	}, &initial); err != nil {
		return "", err
	}
	var listed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := c.rpc("tools/list", map[string]any{}, &listed); err != nil {
		return "", err
	}
	sort.Slice(listed.Tools, func(i, j int) bool {
		return fmt.Sprint(listed.Tools[i]["name"]) < fmt.Sprint(listed.Tools[j]["name"])
	})
	data, err := json.Marshal(struct {
		Instructions any              `json:"instructions"`
		Tools        []map[string]any `json:"tools"`
	}{initial.Instructions, listed.Tools})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (c *updateClient) serverID() (string, error) {
	var state struct {
		ServerID string `json:"server_id"`
		Draining bool   `json:"draining"`
	}
	if err := c.tool("terminal_list", map[string]any{}, &state); err != nil {
		return "", err
	}
	if state.Draining {
		return "", errors.New("server is in maintenance mode")
	}
	return state.ServerID, nil
}

func defaultUpdateRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "terminal-mcp", "updates"), nil
}

func defaultTokenFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "terminal-mcp", "token"), nil
}

func secureUpdateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("update directory must be private (0700): %s", dir)
	}
	return nil
}

func reserveProbePort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := listener.Addr().String()
	return addr, listener.Close()
}
