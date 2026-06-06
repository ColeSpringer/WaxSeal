package shimaudit

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the on-disk version of both committed fixtures. Bump it when
// the fixture shape changes incompatibly.
const SchemaVersion = 1

// Hard caps applied when merging discovery output, so adversarial remote JS
// cannot inflate the roots fixture with random property names. Overflow is
// dropped with a logged warning rather than written.
const (
	MaxRoots    = 4096  // distinct (target,name) roots
	MaxRawPaths = 16384 // retained nested diagnostic paths
	MaxPathLen  = 512   // per raw path
	MaxNameLen  = 256   // per root name
)

// Fixtures are captured data rather than deterministic build outputs, so
// verify-assets excludes them.
//
//go:embed fixtures/chrome_windows_desktop_globals.json
var chromeRefJSON []byte

//go:embed fixtures/observed_missing_roots.json
var rootsJSON []byte

// ReferenceMeta records the environment used to capture a snapshot. The gate
// validates these fields for captured references.
type ReferenceMeta struct {
	// Source is "capture" for a browser capture or "seed" for a WPT-derived
	// placeholder. Only captured references are provenance-gated.
	Source          string   `json:"source"`
	OS              string   `json:"os"`
	FullVersion     string   `json:"fullVersion"`
	Headless        bool     `json:"headless"`
	Origin          string   `json:"origin"`
	IsSecureContext bool     `json:"isSecureContext"`
	LaunchFlags     []string `json:"launchFlags"`
	CapturedAt      string   `json:"capturedAt"`
}

// Reference is the parsed Chrome global-surface snapshot fixture. The embedded
// Surface promotes window/navigator/document to the top level on disk.
type Reference struct {
	SchemaVersion int           `json:"schemaVersion"`
	Meta          ReferenceMeta `json:"meta"`
	Surface
}

// IsCapture reports whether the snapshot came from Chrome for Testing.
func (r Reference) IsCapture() bool { return r.Meta.Source == "capture" }

// RootsFile is the monotonic union of observed missing roots. Roots are never
// removed; their timestamps describe missing observations and must not drive
// deletion.
type RootsFile struct {
	SchemaVersion int      `json:"schemaVersion"`
	Roots         []Root   `json:"roots"`
	RawPaths      []string `json:"rawPaths"`
	UpdatedAt     string   `json:"updatedAt"`
}

// LoadReference parses a Chrome snapshot fixture, defaulting the three target
// maps to non-nil.
func LoadReference(b []byte) (Reference, error) {
	var r Reference
	if err := json.Unmarshal(b, &r); err != nil {
		return Reference{}, fmt.Errorf("shimaudit: parse reference: %w", err)
	}
	r.Surface = r.Surface.normalized()
	return r, nil
}

// LoadRoots parses an observed-missing-roots fixture.
func LoadRoots(b []byte) (RootsFile, error) {
	var rf RootsFile
	if err := json.Unmarshal(b, &rf); err != nil {
		return RootsFile{}, fmt.Errorf("shimaudit: parse roots: %w", err)
	}
	return rf, nil
}

// EmbeddedReference returns the committed Chrome snapshot fixture.
func EmbeddedReference() (Reference, error) { return LoadReference(chromeRefJSON) }

// EmbeddedRoots returns the committed observed-missing-roots fixture.
func EmbeddedRoots() (RootsFile, error) { return LoadRoots(rootsJSON) }

// normalized ensures the three target maps are non-nil so callers never special
// case a missing section.
func (s Surface) normalized() Surface {
	if s.Window == nil {
		s.Window = map[string]Shape{}
	}
	if s.Navigator == nil {
		s.Navigator = map[string]Shape{}
	}
	if s.Document == nil {
		s.Document = map[string]Shape{}
	}
	return s
}

// ValidTarget reports whether t is one of the three enumeration roots.
func ValidTarget(t Target) bool { return slices.Contains(Targets, t) }

// MergeRoots adds observed roots and paths to prev without removing existing
// entries. It preserves first-observed timestamps, advances last-observed
// timestamps, drops invalid or excess input, and returns a sorted result.
func MergeRoots(prev RootsFile, observed []Root, rawPaths []string, now time.Time) (RootsFile, []string) {
	stamp := now.UTC().Format(time.RFC3339)
	var warnings []string

	// Index existing roots by (target,name); preserve first-seen timestamps.
	type key struct {
		t Target
		n string
	}
	idx := map[key]Root{}
	for _, r := range prev.Roots {
		idx[key{r.Target, r.Name}] = r
	}

	dropped := 0
	for _, r := range observed {
		if !ValidTarget(r.Target) || r.Name == "" || len(r.Name) > MaxNameLen {
			dropped++
			continue
		}
		k := key{r.Target, r.Name}
		if existing, ok := idx[k]; ok {
			existing.LastObservedMissing = stamp
			idx[k] = existing
			continue
		}
		if len(idx) >= MaxRoots {
			dropped++
			continue
		}
		idx[k] = Root{Target: r.Target, Name: r.Name, FirstObservedMissing: stamp, LastObservedMissing: stamp}
	}
	if dropped > 0 {
		warnings = append(warnings, fmt.Sprintf("dropped %d root(s): invalid or over the %d-root cap", dropped, MaxRoots))
	}

	merged := RootsFile{SchemaVersion: SchemaVersion, UpdatedAt: stamp}
	for _, r := range idx {
		merged.Roots = append(merged.Roots, r)
	}
	slices.SortFunc(merged.Roots, func(a, b Root) int {
		if a.Target != b.Target {
			return strings.Compare(string(a.Target), string(b.Target))
		}
		return strings.Compare(a.Name, b.Name)
	})

	// Raw paths: union, length-capped, count-capped, stably sorted.
	seen := map[string]bool{}
	for _, p := range append(slices.Clone(prev.RawPaths), rawPaths...) {
		if p == "" || len(p) > MaxPathLen || seen[p] {
			continue
		}
		seen[p] = true
		merged.RawPaths = append(merged.RawPaths, p)
	}
	slices.Sort(merged.RawPaths)
	if len(merged.RawPaths) > MaxRawPaths {
		warnings = append(warnings, fmt.Sprintf("truncated rawPaths from %d to the %d-path cap", len(merged.RawPaths), MaxRawPaths))
		merged.RawPaths = merged.RawPaths[:MaxRawPaths]
	}
	return merged, warnings
}

// Marshal renders a roots fixture as stably-formatted JSON (sorted by
// construction in MergeRoots) with a trailing newline, matching how the file is
// committed.
func (rf RootsFile) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
