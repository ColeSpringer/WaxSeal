//go:build linux && (amd64 || arm64)

package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// residentPages counts the pages of path the page cache holds.
func residentPages(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer func() { _ = syscall.Munmap(mem) }()
	page := os.Getpagesize()
	vec := make([]byte, (len(mem)+page-1)/page)
	if _, _, errno := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&mem[0])), uintptr(len(mem)), uintptr(unsafe.Pointer(&vec[0]))); errno != 0 {
		t.Fatalf("mincore: %v", errno)
	}
	n := 0
	for _, v := range vec {
		n += int(v & 1)
	}
	return n
}

// evictingFilesystems are the statfs types known to drop clean pages on
// POSIX_FADV_DONTNEED. Others keep them (tmpfs, where the pages are the file)
// or ignore the advice (network and passthrough filesystems), so the eviction
// can only be shown on these.
var evictingFilesystems = map[int64]string{
	0xEF53:     "ext4",
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0xF2F52010: "f2fs",
	0x794C7630: "overlayfs",
}

// evictingDir returns a scratch directory on a filesystem that evicts, or
// skips the test: the test's own temp directory first, then one under the
// user's cache directory, since /tmp is tmpfs on many machines. Neither being
// usable (a read-only home, a network mount) is no fault of the code.
func evictingDir(t *testing.T) string {
	t.Helper()
	evicts := func(dir string) bool {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(dir, &fs); err != nil {
			return false
		}
		_, ok := evictingFilesystems[int64(fs.Type)]
		return ok
	}
	if dir := t.TempDir(); evicts(dir) {
		return dir
	}
	if cache, err := os.UserCacheDir(); err == nil && evicts(cache) {
		if dir, err := os.MkdirTemp(cache, "waxseal-filecache-test-"); err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		}
	}
	t.Skip("no scratch directory on a filesystem known to evict on fadvise")
	return ""
}

// Releasing a file's cache evicts its clean pages, which is what returns an
// idle browser's memory to the system. The browser's own process exiting is
// what unmaps them first.
func TestReleaseFileCacheEvictsCleanPages(t *testing.T) {
	dir := evictingDir(t)

	path := filepath.Join(dir, "blob")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Only clean pages can be released, so the write has to reach the disk.
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	_ = f.Close()
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := residentPages(t, path); got == 0 {
		t.Fatal("the file is not cached after writing and reading it; the test cannot show an eviction")
	}

	releaseFileCache([]string{path, filepath.Join(dir, "missing")})
	if got := residentPages(t, path); got != 0 {
		t.Errorf("%d pages still cached after releaseFileCache, want 0", got)
	}
}

// The running browser's binary is read from /proc, which sees through a
// launcher script to the program that was exec'd.
func TestBrowserExeNamesTheRunningBinary(t *testing.T) {
	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if got := browserExe(os.Getpid()); got != want {
		t.Errorf("browserExe(self) = %q, want %q", got, want)
	}
	if got := browserExe(0); got != "" {
		t.Errorf("browserExe(0) = %q, want empty", got)
	}
}

// copyOfSleep puts a private copy of sleep(1) in a scratch directory, so the
// test owns a binary nothing else on the machine is running.
func copyOfSleep(t *testing.T) string {
	t.Helper()
	src, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary to run")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("read %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "sleep") // its own name: a multi-call binary dispatches on it
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

// The closed browser's own processes are killed as a group and take a moment
// to go. The release waits for that group, which costs one signal per look and
// names exactly the processes that are this daemon's.
func TestGroupGoneWaitsForTheGroupToExit(t *testing.T) {
	exe := copyOfSleep(t)
	cmd := exec.Command(exe, "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("start %s: %v", exe, err)
	}
	pgid := cmd.Process.Pid
	if groupGone(pgid, 100*time.Millisecond) {
		t.Fatal("groupGone = true while the group leader runs")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	if !groupGone(pgid, 5*time.Second) {
		t.Error("groupGone = false after the group was killed and reaped")
	}
	if !groupGone(0, time.Second) {
		t.Error("groupGone(0) = false: no group to wait for should read as gone")
	}
}

// Whoever else runs the binary is another browser on the same files, whose
// cache is not this daemon's to drop.
func TestExeRunningSeesAnotherProcessOnTheBinary(t *testing.T) {
	exe := copyOfSleep(t)
	if exeRunning(exe) {
		t.Fatal("exeRunning = true for a binary nothing runs")
	}
	cmd := exec.Command(exe, "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("start %s: %v", exe, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if !exeRunning(exe) {
		t.Error("exeRunning = false while a process runs the binary")
	}
	if kept := releaseBrowserCache(exe, 0); !kept {
		t.Error("releaseBrowserCache released a cache another process is running on")
	}
}
