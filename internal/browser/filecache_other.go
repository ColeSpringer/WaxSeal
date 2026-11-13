//go:build !(linux && (amd64 || arm64))

package browser

// The page cache is released only where the image runs. Elsewhere the idle
// close frees the browser's processes and leaves the cache to the system.

func browserExe(int) string { return "" }

func releaseBrowserCache(string, int) (kept bool) { return false }
