package session

import (
	"strings"
	"testing"
)

func TestChildEnvDoesNotInheritTunnelRuntimeKey(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_KEY", "secret-from-parent")
	t.Setenv("control_plane_api_key", "secret-in-alternate-case")
	for _, e := range childEnv(nil, false) {
		if strings.EqualFold(strings.SplitN(e, "=", 2)[0], "CONTROL_PLANE_API_KEY") {
			t.Fatalf("inherited credential leaked: %s", e)
		}
	}
	// An explicit, fully audited tool-call override is still honored by design.
	var found bool
	for _, e := range childEnv(map[string]string{"CONTROL_PLANE_API_KEY": "explicit-override"}, false) {
		if e == "CONTROL_PLANE_API_KEY=explicit-override" {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit tool-supplied override was suppressed")
	}
}
