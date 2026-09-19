//go:build unix

package browser

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// setProfileBase points profileBase at dir for the duration of the test. On Unix
// that is $HOME, because snap-confined Chromium cannot open a profile under /tmp.
func setProfileBase(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
}

// The Unix lock is an advisory flock, so a lock this package holds must be
// visible to a plain flock attempt from the same test, and an unrelated open of
// the marker must still succeed: nothing here relies on exclusive file access.
func TestUnixProfileLockIsAnAdvisoryFlock(t *testing.T) {
	marker := filepath.Join(t.TempDir(), creatorMarkerFile)
	if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, ok := holdProfileLock(marker)
	if !ok {
		t.Fatal("holdProfileLock failed on a free marker")
	}
	defer func() { _ = held.Close() }()

	// Opening is still allowed; only the flock is contended.
	probe, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("an advisory lock must not block an ordinary open: %v", err)
	}
	defer func() { _ = probe.Close() }()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Error("a second flock succeeded while the profile lock was held")
	}

	// And the unlink is unaffected, which is why cleanupProfile removes before it
	// releases on this platform.
	if err := os.Remove(marker); err != nil {
		t.Errorf("removing a flocked file failed: %v", err)
	}
}

// cleanupProfile can fail to remove the tree, for instance when a still-exiting
// child writes into the profile after the directory scan. RemoveAll has taken
// creator.pid by then, so the marker has to go back for the startup sweep.
func TestUnixCleanupProfileLeavesMarkerWhenRemovalFails(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	lock := markProfileDir(dir)
	if lock == nil {
		t.Fatal("markProfileDir returned a nil lock")
	}
	if _, enforced := pinProfileFile(t, dir); !enforced {
		t.Skip("this platform does not enforce the directory mode, so no removal can fail here")
	}

	cleanupProfile(profileHandle{dir: dir, lock: lock})

	marker, err := os.Stat(filepath.Join(dir, creatorMarkerFile))
	if err != nil {
		t.Fatalf("creator.pid is gone after a cleanup that could not finish (%v); the reaper would never collect this directory", err)
	}
	// The marker still carries its launch date, which for a short run sits inside
	// markerGrace and reads as a browser that is still starting.
	if age := time.Since(marker.ModTime()); age < markerGrace {
		t.Errorf("the marker is dated %v ago, inside the %v grace; the next sweep would read an abandoned profile as a live launch", age, markerGrace)
	}
}
