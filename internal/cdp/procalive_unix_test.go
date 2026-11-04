//go:build unix

package cdp

import "syscall"

// processAlive reports whether pid still exists. A signal 0 is the standard
// existence probe. Spawn reaps its child, so a reaped pid answers false; the
// window in which the OS could recycle it is far shorter than any test's poll.
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
