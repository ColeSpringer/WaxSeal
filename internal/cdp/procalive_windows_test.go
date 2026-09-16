//go:build windows

package cdp

import (
	"os"
	"syscall"
)

const (
	processQueryInformation = 0x0400
	synchronize             = 0x00100000
	waitTimeout             = 0x00000102
)

// processAlive reports whether pid still exists. OpenProcess alone is not the
// answer on Windows: it succeeds for an exited process whose object is still
// referenced, so the handle is waited on with a zero timeout. Only WAIT_TIMEOUT,
// meaning the process object has not been signaled, says the process is running.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryInformation|synchronize, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	ev, werr := syscall.WaitForSingleObject(h, 0)
	return werr == nil && ev == waitTimeout
}

// killPID terminates one process by pid, for tests that need to simulate a
// browser dying under the connection.
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
