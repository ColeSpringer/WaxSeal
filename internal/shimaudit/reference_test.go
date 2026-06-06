package shimaudit

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rootKeys(rf RootsFile) []string {
	out := make([]string, len(rf.Roots))
	for i, r := range rf.Roots {
		out[i] = string(r.Target) + "." + r.Name
	}
	return out
}

// TestMergeRootsMonotonicUnion verifies timestamp handling for new and existing
// roots.
func TestMergeRootsMonotonicUnion(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prev := RootsFile{
		SchemaVersion: SchemaVersion,
		Roots:         []Root{{Target: TargetWindow, Name: "Existing", FirstObservedMissing: t0.Format(time.RFC3339), LastObservedMissing: t0.Format(time.RFC3339)}},
	}
	t1 := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	merged, warns := MergeRoots(prev, []Root{
		{Target: TargetWindow, Name: "Existing"}, // re-observed
		{Target: TargetNavigator, Name: "gpu"},   // new
	}, []string{"navigator.gpu.requestAdapter"}, t1)

	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if got := rootKeys(merged); !slices.Equal(got, []string{"navigator.gpu", "window.Existing"}) {
		t.Fatalf("roots = %v, want sorted union", got)
	}
	for _, r := range merged.Roots {
		switch r.Name {
		case "Existing":
			if r.FirstObservedMissing != t0.Format(time.RFC3339) {
				t.Errorf("existing root lost its first-observed timestamp: %q", r.FirstObservedMissing)
			}
			if r.LastObservedMissing != t1.Format(time.RFC3339) {
				t.Errorf("existing root last-observed not advanced: %q", r.LastObservedMissing)
			}
		case "gpu":
			if r.FirstObservedMissing != t1.Format(time.RFC3339) || r.LastObservedMissing != t1.Format(time.RFC3339) {
				t.Errorf("new root timestamps = %q/%q, want both %q", r.FirstObservedMissing, r.LastObservedMissing, t1.Format(time.RFC3339))
			}
		}
	}
	if !slices.Contains(merged.RawPaths, "navigator.gpu.requestAdapter") {
		t.Errorf("rawPaths missing the merged path: %v", merged.RawPaths)
	}
}

// TestMergeRootsNeverRemoves verifies that an empty observation retains roots.
func TestMergeRootsNeverRemoves(t *testing.T) {
	prev := RootsFile{SchemaVersion: SchemaVersion, Roots: []Root{
		{Target: TargetWindow, Name: "A"}, {Target: TargetWindow, Name: "B"},
	}}
	merged, _ := MergeRoots(prev, nil, nil, time.Now())
	if got := rootKeys(merged); !slices.Equal(got, []string{"window.A", "window.B"}) {
		t.Errorf("roots = %v, want both retained", got)
	}
}

// TestMergeRootsDropsInvalid verifies that invalid roots are rejected.
func TestMergeRootsDropsInvalid(t *testing.T) {
	merged, warns := MergeRoots(RootsFile{SchemaVersion: SchemaVersion}, []Root{
		{Target: "globalThis", Name: "X"}, // invalid target
		{Target: TargetWindow, Name: ""},  // empty name
		{Target: TargetWindow, Name: "Valid"},
	}, nil, time.Now())

	if got := rootKeys(merged); !slices.Equal(got, []string{"window.Valid"}) {
		t.Errorf("roots = %v, want only window.Valid", got)
	}
	if len(warns) == 0 {
		t.Error("expected a warning about dropped roots")
	}
}

// TestMergeRootsCaps verifies that merged fixtures stay within their size caps.
func TestMergeRootsCaps(t *testing.T) {
	var observed []Root
	for i := 0; i < MaxRoots+50; i++ {
		observed = append(observed, Root{Target: TargetWindow, Name: "N" + strconv.Itoa(i)})
	}
	longPath := "window." + strings.Repeat("x", MaxPathLen+1)
	merged, warns := MergeRoots(RootsFile{SchemaVersion: SchemaVersion}, observed, []string{longPath, "window.ok"}, time.Now())

	if len(merged.Roots) > MaxRoots {
		t.Errorf("roots count %d exceeds cap %d", len(merged.Roots), MaxRoots)
	}
	if slices.Contains(merged.RawPaths, longPath) {
		t.Error("over-long raw path was not dropped")
	}
	if !slices.Contains(merged.RawPaths, "window.ok") {
		t.Error("valid raw path was dropped")
	}
	if len(warns) == 0 {
		t.Error("expected cap warnings")
	}
}
