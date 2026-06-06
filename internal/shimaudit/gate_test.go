package shimaudit

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestShimCoverageGate fails when a probed Chrome API is missing from the
// committed shim bundle. It runs offline as part of the default test suite.
func TestShimCoverageGate(t *testing.T) {
	ctx := context.Background()
	ref, err := EmbeddedReference()
	if err != nil {
		t.Fatalf("reference fixture: %v", err)
	}
	roots, err := EmbeddedRoots()
	if err != nil {
		t.Fatalf("roots fixture: %v", err)
	}
	shim, err := ShimSurface(ctx)
	if err != nil {
		t.Fatalf("shim surface: %v", err)
	}
	bare, err := BareRuntimeSurface(ctx)
	if err != nil {
		t.Fatalf("bare surface: %v", err)
	}

	rep := Audit(ref.Surface, shim, bare, roots.Roots)
	if len(rep.MissingProbedReal) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("shim is missing probed Chrome APIs (add to build/js/dom.js, then `make jsbundle`):\n")
	for _, f := range rep.MissingProbedReal {
		b.WriteString("  - ")
		b.WriteString(string(f.Target))
		b.WriteString(".")
		b.WriteString(f.Name)
		b.WriteString("  [")
		b.WriteString(PlacementHint(f.Shape))
		b.WriteString("]\n")
	}
	t.Fatal(b.String())
}

// TestChromeReferenceMatchesProfile validates the provenance of a captured
// reference. Seed references are skipped until replaced by a browser capture.
func TestChromeReferenceMatchesProfile(t *testing.T) {
	ref, err := EmbeddedReference()
	if err != nil {
		t.Fatalf("reference fixture: %v", err)
	}
	if !ref.IsCapture() {
		t.Skipf("reference is a %q placeholder, not a Windows + CfT capture; refresh with "+
			"`make chrome-globals` on Windows + Chrome for Testing %s to enforce provenance",
			ref.Meta.Source, pinnedFullVersion(t))
	}

	if want := pinnedFullVersion(t); ref.Meta.FullVersion != want {
		t.Errorf("snapshot fullVersion = %q, want pinned %q (refresh the snapshot from that exact CfT build)", ref.Meta.FullVersion, want)
	}
	if ref.Meta.OS != "windows" {
		t.Errorf("snapshot os = %q, want \"windows\" (the shim claims Win32)", ref.Meta.OS)
	}
	if !ref.Meta.IsSecureContext {
		t.Error("snapshot isSecureContext = false; capture over a true https:// origin")
	}
	if !strings.HasPrefix(ref.Meta.Origin, "https://") {
		t.Errorf("snapshot origin = %q, want a secure https:// origin", ref.Meta.Origin)
	}
	allowed := map[string]bool{
		"--headless":                true,
		"--headless=new":            true,
		"--hide-scrollbars":         true,
		"--mute-audio":              true,
		"--no-sandbox":              true,
		"--disable-gpu":             true,
		"--remote-debugging-pipe":   true,
		"--remote-debugging-port=0": true,
	}
	for _, f := range ref.Meta.LaunchFlags {
		if !allowed[f] {
			t.Errorf("snapshot launchFlag %q not in the allowed set; an unexpected flag can change the exposed surface", f)
		}
	}
}

// TestFixturesWellFormed validates the schema, roots, and size caps of both
// committed fixtures.
func TestFixturesWellFormed(t *testing.T) {
	ref, err := EmbeddedReference()
	if err != nil {
		t.Fatalf("reference fixture: %v", err)
	}
	if ref.SchemaVersion != SchemaVersion {
		t.Errorf("reference schemaVersion = %d, want %d", ref.SchemaVersion, SchemaVersion)
	}
	if ref.Meta.Source != "seed" && ref.Meta.Source != "capture" {
		t.Errorf("reference meta.source = %q, want \"seed\" or \"capture\"", ref.Meta.Source)
	}
	for _, tgt := range Targets {
		for name, sh := range ref.names(tgt) {
			if name == "" {
				t.Errorf("reference %s has an empty member name", tgt)
			}
			if sh.Descriptor != "" && sh.Descriptor != "data" && sh.Descriptor != "accessor" {
				t.Errorf("reference %s.%s descriptor = %q, want data|accessor", tgt, name, sh.Descriptor)
			}
		}
	}

	roots, err := EmbeddedRoots()
	if err != nil {
		t.Fatalf("roots fixture: %v", err)
	}
	if roots.SchemaVersion != SchemaVersion {
		t.Errorf("roots schemaVersion = %d, want %d", roots.SchemaVersion, SchemaVersion)
	}
	if len(roots.Roots) == 0 {
		t.Error("roots fixture is empty; it must retain at least the seed root")
	}
	if len(roots.Roots) > MaxRoots {
		t.Errorf("roots count %d exceeds cap %d", len(roots.Roots), MaxRoots)
	}
	if len(roots.RawPaths) > MaxRawPaths {
		t.Errorf("rawPaths count %d exceeds cap %d", len(roots.RawPaths), MaxRawPaths)
	}
	seen := map[string]bool{}
	for _, r := range roots.Roots {
		if !ValidTarget(r.Target) {
			t.Errorf("root has invalid target %q", r.Target)
		}
		if r.Name == "" || len(r.Name) > MaxNameLen {
			t.Errorf("root %q has an invalid name", r.Name)
		}
		k := string(r.Target) + "|" + r.Name
		if seen[k] {
			t.Errorf("duplicate root %s.%s", r.Target, r.Name)
		}
		seen[k] = true
	}
	for _, p := range roots.RawPaths {
		if len(p) > MaxPathLen {
			t.Errorf("rawPath %q exceeds length cap %d", p, MaxPathLen)
		}
	}
}

// pinnedFullVersion reads the version used by the profile and JavaScript build.
func pinnedFullVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../chrome_version.json")
	if err != nil {
		t.Fatalf("read chrome_version.json: %v", err)
	}
	var v struct {
		FullVersion string `json:"fullVersion"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse chrome_version.json: %v", err)
	}
	return v.FullVersion
}
