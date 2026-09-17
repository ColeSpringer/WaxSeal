//go:build windows

package cdp

import (
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows has no process group and no SIGKILL. A job object is the equivalent:
// everything in it dies when the job handle closes, which covers both an explicit
// kill and this process exiting for any reason. It is the second line of defense;
// the first is the pipe EOF that Unix relies on too. syscall exports none of the
// job calls, so they come from kernel32.
var (
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

const (
	// jobObjectExtendedLimitInformation is the JobObjectInfoClass for the
	// extended limit struct below.
	jobObjectExtendedLimitInformation = 9
	// jobObjectLimitKillOnJobClose kills every assigned process when the last
	// job handle closes, which is what makes this a parent-death guard.
	jobObjectLimitKillOnJobClose = 0x00002000

	// processSetQuota is the one right AssignProcessToJobObject needs that syscall
	// does not export; PROCESS_TERMINATE comes from there.
	processSetQuota = 0x0100
)

// ioCounters mirrors IO_COUNTERS: six ULONGLONGs.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobObjectBasicLimitInformation mirrors JOB_OBJECT_BASIC_LIMIT_INFORMATION.
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// jobObjectExtendedLimitInfo mirrors JOB_OBJECT_EXTENDED_LIMIT_INFORMATION. A
// hand-rolled Win32 layout is where a port breaks silently, so its size is pinned
// by a test.
type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// procGuard carries the platform-specific work of spawning Chromium and making
// sure it dies with, or before, this process.
//
// kill and release run on different goroutines: a force-close can come from the
// read loop or a stalled write while the reaper is returning from cmd.Wait. The
// Unix guard holds no mutable state, but this one owns a handle, so mu guards it.
// Without that the handle could be closed out from under a kill still reading it.
type procGuard struct {
	log *slog.Logger

	mu  sync.Mutex
	job syscall.Handle // 0 when the job could not be created, or once released
	// assigned records that Chromium actually reached the job. A job exists from
	// attach onward, but started can still fail to put the process in it, and
	// TerminateJobObject reports success on a job holding no processes. Killing on
	// job != 0 would therefore swallow the direct-kill fallback in exactly the case
	// it exists for.
	assigned bool

	// warned keeps a job failure to one log line rather than one per teardown.
	warned bool
}

// newProcGuard returns a guard that logs its own failures.
func newProcGuard(log *slog.Logger) *procGuard { return &procGuard{log: log} }

// attach prepares cmd to inherit the two pipe ends, before Start. Windows has no
// fd convention, so the handle values go in the argv as <read>,<write>, appended
// here rather than in BuildArgs because they only exist once the pipes do; that
// keeps the golden-pinned argv untouched. Without --enable-logging=stderr
// Chromium logs to a file and a failed handshake's stderr tail comes back empty.
func (g *procGuard) attach(cmd *exec.Cmd, cmdPipe, evtPipe *pipePair) {
	// Fd is a plain getter here: Spawn builds the child ends without
	// FILE_FLAG_OVERLAPPED, so there is no poller association for it to drop. The
	// parent ends are overlapped and must never be read through Fd.
	inHandle := uint32(cmdPipe.child.Fd())
	outHandle := uint32(evtPipe.child.Fd())
	cmd.Args = append(cmd.Args,
		fmt.Sprintf("%s%d,%d", ioPipesFlagPrefix, inHandle, outHandle),
		"--enable-logging=stderr",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(inHandle), syscall.Handle(outHandle)},
	}

	// Create the job before the spawn so started has one to assign into. A failure
	// here is not fatal: the pipe-EOF shutdown still applies, and kill falls back
	// to terminating the one process.
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if syscall.Handle(h) == 0 {
		g.warnOnce("cdp: could not create a job object; relying on pipe EOF and a direct kill", err)
		return
	}
	job := syscall.Handle(h)
	info := jobObjectExtendedLimitInfo{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	// SyscallN rather than Call: a pointer converted to uintptr is kept alive and
	// in place for the call only when the conversion sits in the argument list of
	// a syscall.Syscall* function, and Call is an ordinary Go method in between.
	ret, _, serr := syscall.SyscallN(procSetInformationJobObject.Addr(),
		uintptr(job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if ret == 0 {
		g.warnOnce("cdp: could not set kill-on-job-close; relying on pipe EOF and a direct kill", serr)
		_ = syscall.CloseHandle(job)
		return
	}
	g.mu.Lock()
	g.job = job
	g.mu.Unlock()
}

// started assigns the spawned process to the job. os/exec does not expose its
// process handle, so this opens its own.
//
// Assigning after Start is the one place this guard is weaker than the Unix one,
// where Setpgid applies before exec: a helper spawned in that window escapes the
// job. Closing it needs CREATE_SUSPENDED and a resume os/exec gives no handle
// for, so the window is accepted; it is two syscalls wide and Chromium spawns
// nothing that early.
func (g *procGuard) started(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return
	}
	h, err := syscall.OpenProcess(processSetQuota|syscall.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		g.warnLocked("cdp: could not open the spawned process to assign it to the job", err)
		return
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	if ret, _, aerr := procAssignProcessToJobObject.Call(uintptr(g.job), uintptr(h)); ret == 0 {
		g.warnLocked("cdp: could not assign chromium to the job object", aerr)
		return
	}
	g.assigned = true
}

// kill terminates the job, which takes Chromium and every helper it started.
// Without a job that Chromium actually reached it falls back to killing the one
// process, which leaves helpers to exit on their own IPC loss.
func (g *procGuard) kill(cmd *exec.Cmd) {
	// The lock is held across TerminateJobObject, not just the read: releasing it
	// first would let release close the handle before the call reached the kernel.
	g.mu.Lock()
	if g.assigned && g.job != 0 {
		ret, _, terr := procTerminateJobObject.Call(uintptr(g.job), 1)
		if ret != 0 {
			g.mu.Unlock()
			return
		}
		g.warnLocked("cdp: TerminateJobObject failed; killing the process directly", terr)
	}
	g.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// release closes the job handle after Wait. Because of
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, this is also what guarantees that a parent
// which exits without ever reaching kill still takes Chromium with it.
func (g *procGuard) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return
	}
	_ = syscall.CloseHandle(g.job)
	g.job = 0
}

// warnOnce logs the first job-object failure and stays quiet after that, so a
// degraded guard does not repeat itself on every teardown.
func (g *procGuard) warnOnce(msg string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.warnLocked(msg, err)
}

// warnLocked is warnOnce for callers already holding mu.
func (g *procGuard) warnLocked(msg string, err error) {
	if g.warned || g.log == nil {
		return
	}
	g.warned = true
	g.log.Warn(msg, "err", err)
}
