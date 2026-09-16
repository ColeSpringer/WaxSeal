//go:build unix

package browser

import (
	"os"
	"syscall"
)

// This file carries the Unix half of profile ownership and browser discovery.
// The build tag is unix rather than !windows: !windows also selects plan9, js,
// and wasip1, where syscall.Flock does not exist, and internal/cdp already tags
// its own platform files the same way.

// holdProfileLock takes an exclusive advisory lock on marker and returns the open
// file holding it; the caller must keep the file open for as long as the lock is
// needed and close it (via profileHandle.cleanup) to release it. The bool reports
// whether the lock was acquired.
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
// lock. That order matters because ReapStaleProfiles runs at daemon startup and
// can race a different process tearing a profile down. Holding the lock until
// creator.pid is gone keeps the reaper from acting on a half-removed directory.
// A flock does not stop the unlink, so removing first is free here.
func cleanupProfile(h profileHandle) {
	if h.dir != "" {
		_ = os.RemoveAll(h.dir)
	}
	if h.lock != nil {
		_ = h.lock.Close()
	}
}

// profileBase returns a $HOME-rooted base dir for the user-data-dir, because
// snap-confined Chromium cannot open a profile under /tmp. The rule is really a
// Linux one, but a profile under $HOME is harmless on macOS too.
func profileBase() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.TempDir()
}
