//go:build !windows

package browser

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// guardPathUnsafe reports whether path is unsafe for executing the leakless
// helper. Missing paths are safe because leakless creates a fresh copy. Ownership
// and write permissions are checked without relying on the process umask.
func guardPathUnsafe(path string) (bool, string) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "" // Leakless will create a missing path.
		}
		return true, fmt.Sprintf("%s cannot be inspected: %v", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return true, fmt.Sprintf("%s is a symlink (unexpected for the leakless guard)", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true, fmt.Sprintf("%s ownership cannot be determined", path)
	}
	if euid := os.Geteuid(); int(st.Uid) != euid {
		return true, fmt.Sprintf("%s is owned by uid %d, not this process (uid %d)", path, st.Uid, euid)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return true, fmt.Sprintf("%s is group- or world-writable (mode %#o)", path, perm)
	}
	return false, ""
}

// heldProfileLocks keeps marker file descriptors open so their locks remain held.
var heldProfileLocks struct {
	sync.Mutex
	files []*os.File
}

// holdProfileLock takes an exclusive advisory lock on marker and retains it for
// the process lifetime. It reports whether the lock was acquired.
func holdProfileLock(marker string) bool {
	f, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return false
	}
	heldProfileLocks.Lock()
	heldProfileLocks.files = append(heldProfileLocks.files, f)
	heldProfileLocks.Unlock()
	return true
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
