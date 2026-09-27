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
// plus SIGKILL, neither of which can fail in a way worth reporting (Setpgid
// applies at fork; a group SIGKILL lands or the group is gone), so unlike the
// Windows guard this one holds no state and no logger.
type procGuard struct{}

// newProcGuard returns a guard. The logger is accepted for signature parity with
// the Windows constructor, which does report its failures.
func newProcGuard(*slog.Logger) *procGuard { return &procGuard{} }

// attach sets up cmd before Start. The child inherits the command pipe as fd 3
// and the event pipe as fd 4, where --remote-debugging-pipe looks. Setpgid lets
// teardown signal Chromium's whole group; helpers that leave it should exit
// when Chromium closes its IPC. Parent death needs no Pdeathsig: the OS closes
// this process's pipe ends, and Chromium exits on the EOF at fd 3.
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
	// syscall.Kill(-pid) with pid 0 would signal the caller's own process group,
	// and with -1 (the Pid Release leaves) pid 1. A started process has pid > 0.
	if cmd.Process.Pid <= 0 {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// release runs after Wait. Unix holds no handle to give back.
func (g *procGuard) release() {}
