//go:build linux && (amd64 || arm64)

package browser

import (
	"os"
	"strconv"
	"syscall"
	"time"
)

// fadvDontNeed is POSIX_FADV_DONTNEED, which the syscall package does not name.
const fadvDontNeed = 4

// browserExe returns the binary process pid is running, or "" when it cannot
// be read. /proc names what was exec'd, so a launcher script such as Debian's
// /usr/bin/chromium resolves to the real browser.
func browserExe(pid int) string {
	if pid <= 0 {
		return ""
	}
	exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return exe
}

// releaseFileCache asks the kernel to drop the cached pages of each file. It
// drops only clean pages no process has mapped, so it cannot hurt a browser
// that is running or starting, and a file that cannot be opened is skipped:
// the cost of a miss is memory the kernel reclaims later anyway.
func releaseFileCache(files []string) {
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, fadvDontNeed, 0, 0)
		_ = f.Close()
	}
}

// browserExitWait is how long releaseBrowserCache gives the closed browser's
// own processes to exit. They were killed as a group and are gone in moments.
const browserExitWait = 2 * time.Second

// releaseBrowserCache releases the page cache of a browser that has just been
// closed, given the binary it ran and its process group, and reports whether
// it kept the cache instead. The cache belongs to the files, not to this
// daemon, so it is kept while anything else this process can see runs that
// binary, such as a desktop Chrome on the same install.
func releaseBrowserCache(exe string, pgid int) (kept bool) {
	if exe == "" {
		return false
	}
	groupGone(pgid, browserExitWait)
	if exeRunning(exe) {
		return true
	}
	releaseFileCache(browserFiles(exe))
	return false
}

// groupGone waits up to wait for process group pgid to have no member left,
// and reports whether that came true. Signal 0 only asks whether one exists.
func groupGone(pgid int, wait time.Duration) bool {
	if pgid <= 1 {
		return true // no group of ours; -0 and -1 would name other processes
	}
	for deadline := time.Now().Add(wait); syscall.Kill(-pgid, 0) != syscall.ESRCH; time.Sleep(10 * time.Millisecond) {
		if !time.Now().Before(deadline) {
			return false
		}
	}
	return true
}

// exeRunning reports whether any process this one can see is running exe.
// Another container's processes are in another PID namespace and not seen.
func exeRunning(exe string) bool {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, p := range procs {
		if pid, err := strconv.Atoi(p.Name()); err == nil && browserExe(pid) == exe {
			return true
		}
	}
	return false
}
