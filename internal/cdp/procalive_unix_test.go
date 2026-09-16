//go:build unix

package cdp

import "syscall"

// processAlive reports whether pid still exists. A signal 0 is the standard
// existence probe. Spawn reaps its child, so a reaped pid answers false; the
// window in which the OS could recycle it is far shorter than any test's poll.
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// killPID terminates one process by pid, for tests that need to simulate a
// browser dying under the connection.
func killPID(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
