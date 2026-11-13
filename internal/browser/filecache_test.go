package browser

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeFiles creates each named file under dir, making parent directories.
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// A browser installed in a directory of its own, which icudtl.dat beside the
// binary shows, has every file there released with it. Subdirectories hold
// little that was ever read and are left.
func TestBrowserFilesTakesTheBrowsersOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "chromium", "icudtl.dat", "resources.pak", "locales/en-US.pak")
	got := browserFiles(filepath.Join(dir, "chromium"))
	want := []string{filepath.Join(dir, "chromium"), filepath.Join(dir, "icudtl.dat"), filepath.Join(dir, "resources.pak")}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("browserFiles = %v, want %v", got, want)
	}
}

// A binary that shares its directory with other programs is released alone.
func TestBrowserFilesInASharedDirectoryIsTheBinaryAlone(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "chrome", "ls", "sh")
	exe := filepath.Join(dir, "chrome")
	if got := browserFiles(exe); !slices.Equal(got, []string{exe}) {
		t.Errorf("browserFiles = %v, want only %s", got, exe)
	}
}

// With no binary known, as on a platform that cannot name it, nothing is
// released.
func TestBrowserFilesWithoutABinary(t *testing.T) {
	if got := browserFiles(""); len(got) != 0 {
		t.Errorf("browserFiles(\"\") = %v, want none", got)
	}
}
