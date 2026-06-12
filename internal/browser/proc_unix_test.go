//go:build !windows

package browser

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestGuardPathUnsafe(t *testing.T) {
	dir := t.TempDir()

	if unsafe, _ := guardPathUnsafe(filepath.Join(dir, "absent")); unsafe {
		t.Error("absent path flagged unsafe; want safe (leakless extracts a verified copy)")
	}

	safe := filepath.Join(dir, "guard")
	if err := os.WriteFile(safe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if unsafe, why := guardPathUnsafe(safe); unsafe {
		t.Errorf("self-owned 0755 path flagged unsafe: %s", why)
	}

	ww := filepath.Join(dir, "worldwritable")
	if err := os.WriteFile(ww, []byte("x"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ww, 0o777); err != nil { // Ensure the test is independent of umask.
		t.Fatal(err)
	}
	if unsafe, _ := guardPathUnsafe(ww); !unsafe {
		t.Error("world-writable path not flagged; want unsafe")
	}

	gw := filepath.Join(dir, "groupwritable")
	if err := os.WriteFile(gw, []byte("x"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gw, 0o770); err != nil {
		t.Fatal(err)
	}
	if unsafe, _ := guardPathUnsafe(gw); !unsafe {
		t.Error("group-writable path not flagged; want unsafe")
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(safe, link); err != nil {
		t.Fatal(err)
	}
	if unsafe, _ := guardPathUnsafe(link); !unsafe {
		t.Error("symlink not flagged; want unsafe (could redirect the executed target)")
	}
}

func TestMarkerLockable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), creatorMarkerFile)
	if err := os.WriteFile(marker, []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !markerLockable(marker) {
		t.Fatal("fresh marker should be lockable (no owner)")
	}

	f, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	if markerLockable(marker) {
		t.Error("held marker should not be lockable (live owner)")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if !markerLockable(marker) {
		t.Error("released marker should be lockable again")
	}
}

func TestReapStaleProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	mk := func(name string, marker bool) string {
		dir := filepath.Join(home, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if marker {
			if err := os.WriteFile(filepath.Join(dir, creatorMarkerFile), []byte("1"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	dead := mk(".waxseal-11111111", true)
	live := mk(".waxseal-22222222", true)
	markerless := mk(".waxseal-33333333", false)
	backup := mk(".waxseal-backup", false)
	sentinel := filepath.Join(backup, "important.txt")
	if err := os.WriteFile(sentinel, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hold the live profile's marker lock, simulating an in-use sibling.
	lf, err := os.OpenFile(filepath.Join(live, creatorMarkerFile), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	ReapStaleProfiles(nil)

	gone := func(p string) bool { _, err := os.Stat(p); return os.IsNotExist(err) }
	for _, c := range []struct {
		dir      string
		wantGone bool
		why      string
	}{
		{dead, true, "marked + lock free"},
		{live, false, "marked + lock held by a live owner"},
		{markerless, false, "unmarked profile"},
		{backup, false, "unrelated .waxseal-backup"},
	} {
		if gone(c.dir) != c.wantGone {
			t.Errorf("%s: gone=%v, want %v (%s)", filepath.Base(c.dir), gone(c.dir), c.wantGone, c.why)
		}
	}
	if gone(sentinel) {
		t.Error("user data inside .waxseal-backup was deleted")
	}
}
