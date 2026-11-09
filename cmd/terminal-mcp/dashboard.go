package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"terminalmcp/internal/httpguard"
	"terminalmcp/internal/viewer"
)

// browserCommand returns the command that opens a grant-free local URL; its
// argv never carries a secret.
func browserCommand(goos, url string) *exec.Cmd {
	switch goos {
	case "darwin":
		return exec.Command("open", url)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return exec.Command("xdg-open", url)
	}
}

// openBrowser is a variable so tests can observe what would be opened.
var openBrowser = func(url string) error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" &&
		os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return errors.New("no graphical display")
	}
	cmd := browserCommand(runtime.GOOS, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// dashboardCommand implements `terminal-mcp dashboard`: it asks the local
// server for a one-time viewer code using the MCP bearer token, prints the
// code to this terminal and opens the grant-free dashboard URL. The code is
// never placed in a URL, browser argv or a log.
func dashboardCommand(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("terminal-mcp dashboard", flag.ContinueOnError)
	var listen, tokenFile string
	var noOpen, revokeAll bool
	flags.StringVar(&listen, "listen", "", "loopback address of a foreground server (with --token-file or TERMINAL_MCP_TOKEN)")
	flags.StringVar(&tokenFile, "token-file", "", "bearer token file of that server")
	flags.BoolVar(&noOpen, "no-open", false, "print the address and code without opening a browser")
	flags.BoolVar(&revokeAll, "revoke-all", false, "end every dashboard session and unused code")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("dashboard takes no positional arguments")
	}
	if listen == "" {
		if tokenFile != "" {
			return errors.New("--token-file requires --listen")
		}
		saved, savedToken, ok, err := savedDashboardTarget()
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("no standalone configuration found; for a foreground server pass --listen and --token-file (or set TERMINAL_MCP_TOKEN)")
		}
		listen, tokenFile = saved, savedToken
	}
	if err := httpguard.ValidateAddress(listen); err != nil {
		return err
	}
	if tokenFile == "" && os.Getenv("TERMINAL_MCP_TOKEN") == "" {
		return errors.New("no bearer token: pass --token-file or set TERMINAL_MCP_TOKEN")
	}
	if conn, err := net.DialTimeout("tcp", listen, time.Second); err != nil {
		return fmt.Errorf("no terminal-mcp server is listening at %s", listen)
	} else {
		_ = conn.Close()
	}
	client, err := newUpdateClient(listen, tokenFile)
	if err != nil {
		return err
	}
	if revokeAll {
		if err := client.request(http.MethodPost, viewer.AdminRevokePath, nil, nil); err != nil {
			return dashboardError(err)
		}
		_, err := fmt.Fprintln(out, "All dashboard sessions and unused codes were revoked.")
		return err
	}
	var grant struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := client.request(http.MethodPost, viewer.AdminGrantPath, nil, &grant); err != nil {
		return dashboardError(err)
	}
	if grant.Code == "" || grant.ExpiresAt.IsZero() {
		return errors.New("the server returned an invalid dashboard code")
	}
	url := "http://" + listen + viewer.Prefix
	if _, err := fmt.Fprintf(out, "Dashboard: %s\nOne-time code: %s\nThe code works once and expires at %s.\n",
		url, grant.Code, grant.ExpiresAt.Local().Format("15:04:05")); err != nil {
		return err
	}
	if noOpen {
		return nil
	}
	if err := openBrowser(url); err != nil {
		_, _ = fmt.Fprintf(out, "Could not open a browser (%v); open the address above yourself.\n", err)
	}
	return nil
}

// dashboardError explains server answers without echoing response bodies.
func dashboardError(err error) error {
	var httpErr *updateHTTPError
	if !errors.As(err, &httpErr) {
		return fmt.Errorf("dashboard request failed: %w", err)
	}
	switch httpErr.status {
	case http.StatusNotFound:
		return errors.New("this server does not support the dashboard; update it first")
	case http.StatusUnauthorized:
		return errors.New("the server rejected the bearer token; check --token-file or TERMINAL_MCP_TOKEN")
	case http.StatusServiceUnavailable:
		return errors.New("the server is stopping; try again after it restarts")
	default:
		return fmt.Errorf("dashboard request failed with HTTP %d", httpErr.status)
	}
}

// runDashboard writes to the terminal: if stdout is redirected (a file, pipe
// or log) but stderr is a terminal, the code goes to stderr instead, so it is
// not persisted by accident.
func runDashboard(args []string) error {
	out := os.Stdout
	if !isTerminal(os.Stdout) && isTerminal(os.Stderr) {
		out = os.Stderr
	}
	return dashboardCommand(args, out)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
