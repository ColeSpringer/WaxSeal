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

// A profile whose removal could not finish is re-marked so that a sweep collects
// it. The marker cannot be dated now: markerGrace would hold the sweep off the
// directory it was written for, and on Windows that is the very next one, once
// the handles pinning those files are gone.
func TestReapCollectsAnAbandonedProfile(t *testing.T) {
	setProfileBase(t, t.TempDir())
	dir, err := os.MkdirTemp(profileBase(), profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := markProfileAbandoned(dir); err != nil {
		t.Fatalf("markProfileAbandoned: %v", err)
	}

	ReapStaleProfiles(nil)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the sweep left the abandoned profile behind: %v", err)
	}
}

// pinProfileFile puts a file in dir that a removal cannot unlink, the way a
// lingering Chromium handle does on Windows. It reports whether the platform
// enforced the pin: Windows ignores a directory mode, and so does root.
func pinProfileFile(t *testing.T, dir string) (unpin func(), enforced bool) {
	t.Helper()
	sub := filepath.Join(dir, "held")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "Local State"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	unpin = func() { _ = os.Chmod(sub, 0o700) }
	t.Cleanup(unpin)
	return unpin, os.Remove(filepath.Join(sub, "Local State")) != nil
}

// A sweep that cannot finish a removal has to leave the directory marked.
// RemoveAll keeps going past the file it cannot delete, so it takes creator.pid
// on the way, and a markerless directory is one every later sweep retains.
func TestReapKeepsAFailedRemovalCollectible(t *testing.T) {
	setProfileBase(t, t.TempDir())
	dir, err := os.MkdirTemp(profileBase(), profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := markProfileAbandoned(dir); err != nil {
		t.Fatal(err)
	}
	unpin, enforced := pinProfileFile(t, dir)
	if !enforced {
		t.Skip("this platform does not enforce the directory mode, so no removal can fail here")
	}

	ReapStaleProfiles(nil)

	if _, err := os.Stat(filepath.Join(dir, creatorMarkerFile)); err != nil {
		t.Fatalf("the sweep left the profile markerless (%v); nothing would ever collect it", err)
	}

	// With the pin gone, the next sweep finishes the job.
	unpin()
	ReapStaleProfiles(nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the second sweep left the profile behind: %v", err)
	}
}

// A removal that cannot finish has to leave the marker exactly as it found it.
// Rewriting it would date the abandonment to now, and markerGrace hides that
// from the very next sweep, which is the one that runs when the files come free.
func TestRemoveProfileTakesTheMarkerLast(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := markProfileAbandoned(dir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, creatorMarkerFile)
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	unpin, enforced := pinProfileFile(t, dir)
	if !enforced {
		t.Skip("this platform does not enforce the directory mode, so no removal can fail here")
	}

	if err := removeProfile(dir); err == nil {
		t.Fatal("removeProfile reported success over a file it could not delete")
	}

	after, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("the marker is gone after a removal that did not finish: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the marker was rewritten (%v, was %v); it should have been left alone", after.ModTime(), before.ModTime())
	}

	// With the pin gone the marker goes too, along with the directory holding it.
	unpin()
	if err := removeProfile(dir); err != nil {
		t.Fatalf("removeProfile: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("removeProfile left the directory behind: %v", err)
	}
}

// The marker goes one syscall before the directory holding it, so a removal that
// gets that far and then cannot take the directory has to put the marker back:
// what is left is a directory no later sweep would recognize.
func TestRemoveProfileRemarksADirectoryThatOutlivesItsMarker(t *testing.T) {
	base := t.TempDir()
	dir, err := os.MkdirTemp(base, profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := markProfileAbandoned(dir); err != nil {
		t.Fatal(err)
	}
	// Removing a directory needs write permission on its parent, so a read-only
	// base pins the directory itself while leaving everything inside removable.
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) })

	if err := removeProfile(dir); err == nil {
		t.Skip("this platform let the directory go despite the read-only parent")
	}

	if _, err := os.Stat(filepath.Join(dir, creatorMarkerFile)); err != nil {
		t.Errorf("the directory outlived its marker (%v); nothing would ever collect it", err)
	}
}
