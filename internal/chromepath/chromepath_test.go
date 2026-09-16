package chromepath

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The list must never be empty on a supported platform: an empty one turns
// browser detection into an unconditional "no Chromium found" that only shows up
// at runtime.
func TestCandidatesNotEmpty(t *testing.T) {
	got := Candidates()
	if len(got) == 0 {
		t.Fatal("Candidates() is empty; DetectChrome would always fail")
	}
	for i, p := range got {
		if p == "" {
			t.Errorf("candidate %d is empty", i)
		}
		if !filepath.IsAbs(p) {
			t.Errorf("candidate %d = %q, want an absolute path", i, p)
		}
		if strings.Contains(strings.ToLower(p), "edge") {
			t.Errorf("candidate %d = %q: Edge must not be auto-detected, it reports a different browser identity", i, p)
		}
	}
}

// Each platform's list must name that platform's browser, so a build-tag mix-up
// cannot hand one platform the other's paths.
func TestCandidatesMatchThePlatform(t *testing.T) {
	got := Candidates()
	switch runtime.GOOS {
	case "windows":
		for i, p := range got {
			if !strings.HasSuffix(strings.ToLower(p), ".exe") {
				t.Errorf("candidate %d = %q, want a .exe on windows", i, p)
			}
		}
	case "darwin":
		found := false
		for _, p := range got {
			if strings.Contains(p, "Google Chrome.app") {
				found = true
			}
		}
		if !found {
			t.Error("no Chrome.app bundle among the candidates; darwin is a shipped release target")
		}
	default:
		found := false
		for _, p := range got {
			if strings.HasPrefix(p, "/usr/bin/") {
				found = true
			}
		}
		if !found {
			t.Errorf("no /usr/bin candidate on %s", runtime.GOOS)
		}
	}
}
