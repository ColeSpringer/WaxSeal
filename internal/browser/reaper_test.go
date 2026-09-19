package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyStaleProfiles(t *testing.T) {
	free := map[string]bool{
		filepath.Join("/dead", creatorMarkerFile):     true,
		filepath.Join("/live", creatorMarkerFile):     false,
		filepath.Join("/starting", creatorMarkerFile): true,
	}
	lockable := func(marker string) bool { return free[marker] }

	states := []profileState{
		{path: "/dead", hasMarker: true},
		{path: "/live", hasMarker: true},
		{path: "/markerless", hasMarker: false},
		// A browser between writing its marker and taking the lock: marked, lock
		// free, and not to be touched.
		{path: "/starting", hasMarker: true, fresh: true},
	}

	got := classifyStaleProfiles(states, lockable)
	removed := map[string]bool{}
	for _, st := range got {
		removed[st.path] = true
	}

	want := map[string]bool{"/dead": true, "/live": false, "/markerless": false, "/starting": false}
	for path, w := range want {
		if removed[path] != w {
			t.Errorf("classify %s: removed = %v, want %v", path, removed[path], w)
		}
	}
	if len(got) != 1 {
		t.Errorf("removed %d directories, want 1 (%v)", len(got), got)
	}
}

func TestProfileDirPattern(t *testing.T) {
	match := []string{".waxseal-0", ".waxseal-853875248", ".waxseal-2016821984"}
	noMatch := []string{
		".waxseal-backup", ".waxseal-bakeoff-3666812682", ".waxseal-",
		".waxseal-abc", ".waxseal-1a", ".waxsealx-1", "waxseal-1",
	}
	for _, m := range match {
		if !profileDirPattern.MatchString(m) {
			t.Errorf("%q should match the profile directory pattern", m)
		}
	}
	for _, m := range noMatch {
		if profileDirPattern.MatchString(m) {
			t.Errorf("%q unexpectedly matched the profile directory pattern", m)
		}
	}
}

func TestWriteMarker(t *testing.T) {
	dir := t.TempDir()
	if err := writeMarker(dir); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, creatorMarkerFile)); err != nil {
		t.Errorf("marker not written: %v", err)
	}
}
