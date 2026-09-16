//go:build windows

package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// setProfileBase points profileBase at dir for the duration of the test. On
// Windows profiles live under the temp directory, which os.TempDir reads from
// TMP, then TEMP, then USERPROFILE. All three are set so the redirect holds
// whichever one the runtime reaches for.
func setProfileBase(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("USERPROFILE", dir)
}

// The Windows lock is an exclusive open, so it has consequences a flock does not:
// a second open is refused outright, and the file cannot be deleted while the
// handle lives. cleanupProfile releases before removing because of that second
// property, and this test is what pins it.
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
