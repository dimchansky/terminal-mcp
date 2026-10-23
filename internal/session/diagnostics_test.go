//go:build linux || darwin

package session

import (
	"context"
	"strings"
	"testing"
)

func TestMissingSessionHasRecoveryHint(t *testing.T) {
	m := New(nil)
	defer m.Close()
	_, err := m.Read(context.Background(), ReadArgs{SessionID: "0123456789abcdef0123456789abcdef"})
	if err == nil {
		t.Fatal("missing session accepted")
	}
	for _, part := range []string{"session_not_found_or_expired", "may have restarted", "terminal_list"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("missing %q in session error %q", part, err)
		}
	}
	if strings.Contains(err.Error(), "make update") {
		t.Fatal("generic MCP error contains project-specific command")
	}
}
