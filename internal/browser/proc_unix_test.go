//go:build unix

package browser

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
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
