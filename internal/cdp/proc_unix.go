//go:build unix

package cdp

import (
	"log/slog"
	"os"
	"os/exec"
	"syscall"
)

// procGuard carries the platform-specific work of spawning Chromium and making
// sure it dies with, or before, this process. On Unix that is a process group
// plus SIGKILL; on Windows it is a Job Object.
// Nothing here can fail in a way worth reporting: Setpgid is applied by the
// kernel at fork and SIGKILL to a group either lands or the group is already
// gone, so this guard holds no state and keeps no logger. The Windows one does,
// because its job object can fail to be created or assigned.
type procGuard struct{}

// newProcGuard returns a guard. The logger is accepted for signature parity with
// the Windows constructor, which does report its failures.
func newProcGuard(*slog.Logger) *procGuard { return &procGuard{} }

// attach prepares cmd to inherit the two pipe ends, before Start. The child sees
// the command pipe on fd 3 and the event pipe on fd 4, which is where
// --remote-debugging-pipe looks for them, so the argv needs no addition.
//
// Setpgid puts Chromium in its own process group so teardown can signal the whole
// group. Helpers that leave the group are expected to exit when Chromium closes
// its own IPC. Parent death is handled by the command pipe closing: the OS closes
// fd 3, and Chromium exits after reading EOF.
func (g *procGuard) attach(cmd *exec.Cmd, cmdPipe, evtPipe *pipePair) {
	cmd.ExtraFiles = []*os.File{cmdPipe.child, evtPipe.child}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// started runs right after a successful Start. Unix needs nothing here: the
// process group was requested before the spawn.
func (g *procGuard) started(*exec.Cmd) {}

// kill SIGKILLs the child's process group. With Setpgid and no explicit Pgid, the
// child is its own group leader, so the group id equals the child pid.
func (g *procGuard) kill(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// A non-positive pid would make syscall.Kill(-pid) target the caller's own
	// process group. A started process always has pid > 0; guard defensively.
	if cmd.Process.Pid <= 0 {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// release runs after Wait. Unix holds no handle to give back.
func (g *procGuard) release() {}
