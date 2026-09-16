package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// Profile ownership is a lock on the marker file: an advisory flock on Unix, an
// exclusive open on Windows. The mechanism differs, the contract does not, so
// these tests are written against holdProfileLock and markerLockable and run on
// both. The platform-specific behaviour each one relies on is pinned in
// proc_unix_test.go and proc_windows_test.go.

// markProfileDir returns the open lock file instead of stashing it in a process
// global. cleanup then removes the profile directory and releases the lock,
// letting another daemon's reaper reclaim the slot.
func TestProfileHandleCleanup(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	lock := markProfileDir(dir)
	if lock == nil {
		t.Fatal("markProfileDir returned a nil lock; the handle must be handed back for cleanup")
	}
	marker := filepath.Join(dir, creatorMarkerFile)
	if markerLockable(marker) {
		t.Error("marker should be locked while the handle holds it")
	}

	profileHandle{dir: dir, lock: lock}.cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove the profile dir: %v", err)
	}
	// cleanup must be nil-safe for partially initialized handles.
	profileHandle{}.cleanup()
}

// A marker with no owner is free; one held by holdProfileLock is not; and
// releasing the handle frees it again. That last step is what keeps a crashed
// daemon from leaving a profile nobody may reclaim.
func TestMarkerLockable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), creatorMarkerFile)
	if err := os.WriteFile(marker, []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !markerLockable(marker) {
		t.Fatal("fresh marker should be lockable (no owner)")
	}

	f, ok := holdProfileLock(marker)
	if !ok {
		t.Fatal("holdProfileLock failed on a free marker")
	}
	if markerLockable(marker) {
		t.Error("held marker should not be lockable (live owner)")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !markerLockable(marker) {
		t.Error("released marker should be lockable again")
	}
}

// A marker that does not exist is not lockable, so the reaper leaves a directory
// it cannot reason about alone.
func TestMarkerLockableMissingFile(t *testing.T) {
	if markerLockable(filepath.Join(t.TempDir(), "nothing-here")) {
		t.Error("a missing marker reported as lockable; the reaper would act on an unknown directory")
	}
}

func TestReapStaleProfiles(t *testing.T) {
	base := t.TempDir()
	setProfileBase(t, base)
	if got := profileBase(); got != base {
		t.Fatalf("profileBase() = %q, want the redirected %q; the test would sweep the real one", got, base)
	}

	mk := func(name string, marker bool) string {
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if marker {
			if err := os.WriteFile(filepath.Join(dir, creatorMarkerFile), []byte("1"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	dead := mk(".waxseal-11111111", true)
	live := mk(".waxseal-22222222", true)
	markerless := mk(".waxseal-33333333", false)
	backup := mk(".waxseal-backup", false)
	sentinel := filepath.Join(backup, "important.txt")
	if err := os.WriteFile(sentinel, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hold the live profile's marker lock, simulating an in-use sibling.
	lf, ok := holdProfileLock(filepath.Join(live, creatorMarkerFile))
	if !ok {
		t.Fatal("could not hold the live profile's marker")
	}
	defer func() { _ = lf.Close() }()

	ReapStaleProfiles(nil)

	gone := func(p string) bool { _, err := os.Stat(p); return os.IsNotExist(err) }
	for _, c := range []struct {
		dir      string
		wantGone bool
		why      string
	}{
		{dead, true, "marked + lock free"},
		{live, false, "marked + lock held by a live owner"},
		{markerless, false, "unmarked profile"},
		{backup, false, "unrelated .waxseal-backup"},
	} {
		if gone(c.dir) != c.wantGone {
			t.Errorf("%s: gone=%v, want %v (%s)", filepath.Base(c.dir), gone(c.dir), c.wantGone, c.why)
		}
	}
	if gone(sentinel) {
		t.Error("user data inside .waxseal-backup was deleted")
	}
}
