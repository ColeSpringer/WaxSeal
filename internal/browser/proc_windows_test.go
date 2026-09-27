//go:build windows

package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// setProfileBase points profileBase (the temp directory on Windows) at dir for
// the duration of the test. os.TempDir reads TMP, then TEMP, then USERPROFILE,
// so all three are set.
func setProfileBase(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("USERPROFILE", dir)
}

// The Windows lock is an exclusive open, so unlike a flock it refuses a second
// open outright and blocks deletion while the handle lives. The second property
// is why cleanupProfile releases the lock before removing the marker.
func TestWindowsProfileLockIsAnExclusiveOpen(t *testing.T) {
	marker := filepath.Join(t.TempDir(), creatorMarkerFile)
	if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, ok := holdProfileLock(marker)
	if !ok {
		t.Fatal("holdProfileLock failed on a free marker")
	}

	if _, err := os.OpenFile(marker, os.O_RDONLY, 0); err == nil {
		t.Error("a second open succeeded while the profile lock was held")
	} else if !isSharingViolation(err) {
		t.Errorf("second open failed with %v, want a sharing violation", err)
	}

	// An attribute-only open still works, which is what lets the reaper decide a
	// directory is marked before it tries the lock.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("os.Stat on a held marker failed: %v", err)
	}

	// This is the reason for the platform-specific cleanup order.
	if err := os.Remove(marker); err == nil {
		t.Error("a held marker was deleted; cleanupProfile could have kept the Unix order")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Errorf("removing the marker after releasing the lock failed: %v", err)
	}
}

// cleanupProfile must leave nothing behind once the lock is released, including
// the marker file its own lock was on.
func TestWindowsCleanupProfileRemovesLockedDirectory(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	lock := markProfileDir(dir)
	if lock == nil {
		t.Fatal("markProfileDir returned a nil lock")
	}
	cleanupProfile(profileHandle{dir: dir, lock: lock})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanupProfile left the directory behind: %v", err)
	}
}

// cleanupProfile can outlive its retry budget when a Chromium handle lingers on
// a profile file. It must then leave creator.pid behind, dated abandoned, so
// the next sweep collects the directory once the handle closes.
func TestWindowsCleanupProfileLeavesMarkerWhenFilesAreHeld(t *testing.T) {
	setProfileBase(t, t.TempDir())
	dir, err := os.MkdirTemp(profileBase(), profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	lock := markProfileDir(dir)
	if lock == nil {
		t.Fatal("markProfileDir returned a nil lock")
	}
	// Hold a profile file open with no sharing rights, the way a helper that has
	// not exited yet does, so RemoveAll cannot delete it.
	held := filepath.Join(dir, "Local State")
	if err := os.WriteFile(held, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := openMarkerExclusive(held)
	if err != nil {
		t.Fatal(err)
	}
	cleanupProfile(profileHandle{dir: dir, lock: lock})
	if _, err := os.Stat(filepath.Join(dir, creatorMarkerFile)); err != nil {
		t.Fatalf("creator.pid is gone after a cleanup that could not finish (%v); the reaper would never collect this directory", err)
	}
	// Once the handle is released, the startup sweep collects the directory.
	_ = h.Close()
	ReapStaleProfiles(nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("ReapStaleProfiles left the directory behind: %v", err)
	}
}
