package browser

import (
	"os"
	"path/filepath"
)

// An idle close ends every browser process, but the files Chromium read stay in
// the page cache: about 320 MB, nearly all of it the binary, which Chromium
// reads whole. The kernel would give that back under pressure, but until then
// it is charged to the container and reported as its memory, so the idle close
// releases it (releaseBrowserCache). The next launch reads the files back,
// about half a second on an SSD.
//
// The release drops only pages no process has mapped. A browser in another
// container on the same image layers keeps what it has mapped and loses the
// rest of its cache for those files, which it reads back as it needs them.

// releaseIdleCache is releaseBrowserCache, as a variable so a test can watch
// where in the close it runs.
var releaseIdleCache = releaseBrowserCache

// browserMarker sits beside the binary in every Chromium and Chrome install.
// Finding it says the directory is the browser's own.
const browserMarker = "icudtl.dat"

// browserFiles lists the files to release for a browser whose binary is exe:
// the regular files of its directory when that is the browser's own, and
// otherwise exe alone, so a binary installed among other programs does not
// take their cache with it. An empty exe lists nothing.
func browserFiles(exe string) []string {
	if exe == "" {
		return nil
	}
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, browserMarker)); err != nil {
		return []string{exe}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{exe}
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
}
