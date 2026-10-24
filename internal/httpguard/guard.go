package httpguard

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const MaxBody = 1 << 20

func ValidateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen address must be a literal loopback IP and port: %w", err)
	}
	ip := net.ParseIP(host)
	p, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || p < 0 || p > 65535 {
		return fmt.Errorf("only literal loopback IPs and numeric ports are allowed")
	}
	return nil
}

func ValidateToken(token string) error {
	if len(token) < 32 || len(token) > 4096 {
		return fmt.Errorf("HTTP token must contain 32..4096 characters; generate a random token")
	}
	for _, c := range token {
		if c <= ' ' || c > '~' {
			return fmt.Errorf("HTTP token must be printable ASCII without whitespace")
		}
	}
	return nil
}

func LoadToken(file string) (string, error) {
	token := os.Getenv("TERMINAL_MCP_TOKEN")
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }() // Token file is read-only.
		info, err := f.Stat()
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return "", fmt.Errorf("token file must be regular and unreadable by group/other users (for example chmod 600)")
		}
		b, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil {
			return "", err
		}
		token = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	if err := ValidateToken(token); err != nil {
		return "", err
	}
	return token, nil
}

func Wrap(next http.Handler, expectedHost, token string) (http.Handler, error) {
	if err := ValidateAddress(expectedHost); err != nil {
		return nil, err
	}
	if err := ValidateToken(token); err != nil {
		return nil, err
	}
	want := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Host != expectedHost || len(r.Header.Values("Origin")) != 0 {
			http.Error(w, "unexpected Host or Origin", http.StatusForbidden)
			return
		}
		// Absent routes, including OAuth discovery, must be 404 rather than an auth challenge.
		if (r.URL.Path != "/mcp" && r.URL.Path != "/admin/maintenance" && r.URL.Path != "/admin/runtime") || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Values("Authorization")
		if len(auth) != 1 {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		got := sha256.Sum256([]byte(auth[0]))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if r.ContentLength > MaxBody {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
		next.ServeHTTP(w, r)
	}), nil
}
