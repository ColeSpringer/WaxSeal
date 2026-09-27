//go:build unix

package browser

import (
	"os"
	"syscall"
)

// This file carries the Unix half of profile ownership. The build tag is unix
// rather than !windows: !windows also selects plan9, js, and wasip1, where
// syscall.Flock does not exist, and internal/cdp tags its platform files the
// same way.

// holdProfileLock takes an exclusive advisory lock on marker and reports
// whether it succeeded. The returned file holds the lock until the caller
// closes it through profileHandle.cleanup.
func holdProfileLock(marker string) (*os.File, bool) {
	f, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, false
	}
	return f, true
}

// markerLockable reports whether marker's advisory lock is free. It releases the
// probe lock immediately. A marker that cannot be opened is treated as locked.
func markerLockable(marker string) bool {
	f, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false // A live owner holds the lock.
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true
}

// cleanupProfile removes the profile directory before releasing its advisory
// lock, so a ReapStaleProfiles racing in another process never acts on a
// half-removed directory: the lock is held until creator.pid is gone. A flock
// does not block the unlink, so removing first costs nothing.
func cleanupProfile(h profileHandle) {
	if h.dir != "" {
		if err := removeProfile(h.dir); err != nil {
			// The removal did not finish, so the marker is still dated from the
			// launch. Date it abandoned for the next startup sweep.
			_ = markProfileAbandoned(h.dir)
		}
	}
	if h.lock != nil {
		_ = h.lock.Close()
	}
}

// profileBase returns a $HOME-rooted base dir for the user-data-dir, because
// snap-confined Chromium cannot open a profile under /tmp. Snap is Linux-only,
// but a profile under $HOME is harmless on macOS too.
func profileBase() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.TempDir()
}
