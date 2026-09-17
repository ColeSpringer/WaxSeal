//go:build windows

package browser

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// Windows has no flock and does not need one: a file opened with no sharing
// rights cannot be opened again while the handle lives, and the kernel drops it
// on any exit, crash included. That is the same liveness signal flock gives, with
// no PID to misread. Deletion is where the two differ, which is why
// cleanupProfile does not use the Unix ordering.

const (
	// errorSharingViolation is what CreateFile reports when another handle already
	// has the file open without sharing. It is the signal that a live owner holds
	// the profile.
	errorSharingViolation = syscall.Errno(32)

	// profileRemoveTimeout bounds the retry after the lock is released. Handles
	// close asynchronously once Chromium's job is terminated, so a first
	// RemoveAll can still see files in use for a moment.
	profileRemoveTimeout = 3 * time.Second
	// profileRemoveInterval paces that retry.
	profileRemoveInterval = 100 * time.Millisecond
)

// openMarkerExclusive opens marker with no sharing rights, which is the lock.
func openMarkerExclusive(marker string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(marker)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), marker), nil
}

// holdProfileLock opens marker exclusively and returns the file holding that
// lock. The caller keeps it open for as long as the profile is in use and closes
// it through profileHandle.cleanup.
func holdProfileLock(marker string) (*os.File, bool) {
	f, err := openMarkerExclusive(marker)
	if err != nil {
		return nil, false
	}
	return f, true
}

// markerLockable reports whether marker is free, by taking the same exclusive
// open and closing it at once. A sharing violation means a live owner; any other
// failure (a missing or unreadable marker) is treated as locked, so the reaper
// leaves the directory alone.
func markerLockable(marker string) bool {
	f, err := openMarkerExclusive(marker)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// cleanupProfile removes the profile directory and releases the marker lock.
//
// The first pass runs with the lock held, so a concurrent reaper still reads a
// live owner and leaves the tree alone. It cannot delete creator.pid or the
// directory holding it, so the lock is released and the removal retried; what a
// reaper can catch unlocked is one file, for two syscalls. The retry loop is for
// Chromium's own handles, which close asynchronously after the job kill.
func cleanupProfile(h profileHandle) {
	if h.dir != "" && h.lock != nil {
		// Expected to fail on creator.pid and on the directory itself; what it
		// removes is everything Chromium left behind.
		_ = os.RemoveAll(h.dir)
	}
	if h.lock != nil {
		_ = h.lock.Close()
	}
	if h.dir == "" {
		return
	}
	deadline := time.Now().Add(profileRemoveTimeout)
	for {
		if err := os.RemoveAll(h.dir); err == nil {
			return
		}
		if time.Now().After(deadline) {
			// RemoveAll keeps going past a file it cannot delete, so by now it has
			// taken creator.pid and left the directory markerless, which the reaper
			// retains rather than collects. Put the marker back so the next startup
			// sweep finds an unlocked, marked directory and removes it.
			_ = writeMarker(h.dir)
			return
		}
		time.Sleep(profileRemoveInterval)
	}
}

// profileBase returns the base dir for the user-data-dir. The snap-confinement
// rule that puts it under $HOME on Linux does not apply here, and .waxseal-*
// directories under C:\Users\<name> would be an unwelcome surprise, so profiles
// live under the temp directory. The reaper globs this same function, so it
// sweeps wherever this points.
func profileBase() string { return os.TempDir() }

// isSharingViolation reports whether err is the exclusive-open refusal, so the
// tests can assert the lock rather than just "some error".
func isSharingViolation(err error) bool {
	return errors.Is(err, errorSharingViolation)
}
