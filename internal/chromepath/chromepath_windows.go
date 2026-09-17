//go:build windows

package chromepath

import (
	"os"
	"path/filepath"
	"strings"
)

// platformCandidates lists the Windows install locations: Google Chrome, then a
// Chromium build, under each Program Files root and the per-user install
// directory. The roots come from the environment so a 32-bit install, a per-user
// install, and a redirected Program Files all resolve, with the literal C: roots
// as the fallback for a stripped environment. An unset variable contributes
// nothing rather than a path rooted at the drive.
func platformCandidates() []string {
	rels := []string{`Google\Chrome\Application\chrome.exe`, `Chromium\Application\chrome.exe`}
	var roots []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "LOCALAPPDATA"} {
		if base := os.Getenv(env); base != "" {
			roots = append(roots, base)
		}
	}
	roots = append(roots, `C:\Program Files`, `C:\Program Files (x86)`)
	seen := make(map[string]bool)
	var out []string
	for _, rel := range rels {
		for _, root := range roots {
			p := filepath.Join(root, rel)
			if key := strings.ToLower(p); !seen[key] {
				seen[key] = true
				out = append(out, p)
			}
		}
	}
	return out
}
