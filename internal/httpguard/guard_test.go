package httpguard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoopbackOnly(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8090", "[::1]:8090", "127.0.0.2:0"} {
		if err := ValidateAddress(address); err != nil {
			t.Errorf("rejected %s: %v", address, err)
		}
	}
	for _, address := range []string{":8090", "0.0.0.0:8090", "[::]:8090", "localhost:8090", "192.168.1.2:8090", "127.0.0.1:http", "127.0.0.1:99999"} {
		if ValidateAddress(address) == nil {
			t.Errorf("accepted %s", address)
		}
	}
}

func TestHTTPTrustBoundary(t *testing.T) {
	token := strings.Repeat("a", 64)
	h, err := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), "127.0.0.1:8090", token)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, host, auth, origin, path string
		status                         int
	}{
		{"ok", "127.0.0.1:8090", "Bearer " + token, "", "/mcp", 204},
		{"missing auth", "127.0.0.1:8090", "", "", "/mcp", 401},
		{"wrong auth", "127.0.0.1:8090", "Bearer wrong", "", "/mcp", 401},
		{"rebinding", "attacker.example:8090", "Bearer " + token, "", "/mcp", 403},
		{"browser", "127.0.0.1:8090", "Bearer " + token, "http://attacker.example", "/mcp", 403},
		{"same origin browser", "127.0.0.1:8090", "Bearer " + token, "http://127.0.0.1:8090", "/mcp", 403},
		{"wrong path", "127.0.0.1:8090", "Bearer " + token, "", "/mcp/extra", 404},
		{"unknown path without auth", "127.0.0.1:8090", "", "", "/mcp/extra", 404},
		{"discovery host check", "attacker.example:8090", "", "", "/.well-known/oauth-protected-resource", 403},
		{"discovery origin check", "127.0.0.1:8090", "", "http://attacker.example", "/.well-known/oauth-protected-resource", 403},
		{"query token", "127.0.0.1:8090", "Bearer " + token, "", "/mcp?token=leak", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+c.host+c.path, nil)
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatalf("got %d want %d", w.Code, c.status)
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("unexpected CORS")
			}
		})
	}
}

func TestDuplicateAuthAndBodyLimit(t *testing.T) {
	token := strings.Repeat("b", 64)
	h, _ := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(204)
	}), "127.0.0.1:8090", token)
	r := httptest.NewRequest("POST", "http://127.0.0.1:8090/mcp", nil)
	r.Header.Add("Authorization", "Bearer "+token)
	r.Header.Add("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("duplicate Authorization accepted")
	}
	for _, known := range []bool{true, false} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8090/mcp", strings.NewReader(strings.Repeat("x", MaxBody+1)))
		if !known {
			r.ContentLength = -1
		}
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 413 {
			t.Fatalf("unbounded body, known=%v", known)
		}
	}
}

func TestTokenValidationAndPermissions(t *testing.T) {
	if ValidateToken("short") == nil || ValidateToken(strings.Repeat("a", 40)+"\n") == nil {
		t.Fatal("invalid token accepted")
	}
	file := filepath.Join(t.TempDir(), "token")
	value := strings.Repeat("c", 64)
	if err := os.WriteFile(file, []byte(value+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadToken(file); err != nil || got != value {
		t.Fatalf("token file: %q %v", got, err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(file); err == nil {
		t.Fatal("insecure token file accepted")
	}
	t.Setenv("TERMINAL_MCP_TOKEN", value)
	if got, err := LoadToken(""); err != nil || got != value {
		t.Fatal("environment token failed")
	}
}

func TestAbsentOAuthMetadataReturnsNotFound(t *testing.T) {
	token := strings.Repeat("d", 64)
	h, err := Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("OAuth discovery reached the MCP handler")
	}), "127.0.0.1:8090", token)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/",
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		t.Run(path, func(t *testing.T) {
			for _, auth := range []string{"", "Bearer wrong", "Bearer " + token} {
				r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8090"+path, nil)
				if auth != "" {
					r.Header.Set("Authorization", auth)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusNotFound {
					t.Errorf("auth present=%v: got %d, want 404; body=%q", auth != "", w.Code, w.Body.String())
				}
				if w.Header().Get("WWW-Authenticate") != "" {
					t.Error("absent OAuth metadata must not advertise an authorization flow")
				}
			}
		})
	}
}

func TestMaintenanceUsesSameAuthentication(t *testing.T) {
	called := false
	h, err := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }), "127.0.0.1:8090", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8090/admin/maintenance", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || called {
		t.Fatal("unprotected maintenance")
	}
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || !called {
		t.Fatal("authorized maintenance rejected")
	}
}
