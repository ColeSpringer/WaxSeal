//go:build windows

package chromepath

import (
	"os"
	"path/filepath"
)

// platformCandidates lists the Windows install locations. They are built from the
// environment rather than hardcoded so a 32-bit install, a per-user install, and
// a redirected Program Files all resolve. An unset variable contributes nothing
// rather than a path rooted at the drive.
func platformCandidates() []string {
	const rel = `Google\Chrome\Application\chrome.exe`
	var out []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "LOCALAPPDATA"} {
		if base := os.Getenv(env); base != "" {
			out = append(out, filepath.Join(base, rel))
		}
	}
	return out
}
