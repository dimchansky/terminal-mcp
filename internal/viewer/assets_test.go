package viewer

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func scriptAssets(t *testing.T) []string {
	t.Helper()
	entries, err := static.ReadDir("static")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			names = append(names, e.Name())
		}
	}
	if len(names) < 2 {
		t.Fatalf("modules %v", names)
	}
	return names
}

// TestEveryModuleIsServedToModuleImports: a module import is a same-origin
// cors fetch with an Origin header; every module is its own exact route.
func TestEveryModuleIsServedToModuleImports(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, name := range scriptAssets(t) {
		w := serve(h, browser(http.MethodGet, Prefix+name, "", RouteAsset,
			header("Sec-Fetch-Mode", "cors"), header("Origin", "http://"+testHost)))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
			t.Errorf("%s: %d %s", name, w.Code, w.Header().Get("Content-Type"))
		}
		checkSecurityHeaders(t, w)
		if w := serve(h, browser(http.MethodGet, Prefix+name, "", RouteAsset,
			header("Sec-Fetch-Mode", "cors"), header("Origin", "http://evil.example"))); w.Code != http.StatusForbidden {
			t.Errorf("%s from another origin: %d", name, w.Code)
		}
	}
	if w := serve(h, browser(http.MethodGet, Prefix+"package.json", "", RouteAsset)); w.Code != http.StatusNotFound {
		t.Errorf("non-asset file served: %d", w.Code)
	}
}

// TestAssetsUseOnlyInertRendering: no page code may turn recorded text into
// markup or code, set inline styles, or log (logs could keep private data).
func TestAssetsUseOnlyInertRendering(t *testing.T) {
	banned := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|createContextualFragment|DOMParser|setHTMLUnsafe|parseHTMLUnsafe|srcdoc|document\.write|document\.domain|\beval\s*\(|\bFunction\s*\(|\bimport\s*\(|\.href\s*=|window\.open|setAttribute\(\s*["']style|setAttribute\(\s*["']on|javascript:|console\.`)
	for _, name := range scriptAssets(t) {
		body, err := static.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if m := banned.FindString(line); m != "" {
				t.Errorf("%s:%d uses %q", name, i+1, m)
			}
		}
	}
}

// TestThePageOffersNoPause (#9, #10): history is read by scrolling, not by
// pausing the live view; no asset offers a Pause control.
func TestThePageOffersNoPause(t *testing.T) {
	entries, err := static.ReadDir("static")
	if err != nil {
		t.Fatal(err)
	}
	pause := regexp.MustCompile(`(?i)\bpaus(e|ed|ing)\b|\x{23F8}`)
	for _, e := range entries {
		body, err := static.ReadFile("static/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if m := pause.Find(body); m != nil {
			t.Errorf("%s mentions %q", e.Name(), m)
		}
	}
}
