//go:build unix

package chromepath

// platformCandidates lists the Unix install locations. The darwin entry is the
// standard Chrome bundle; the rest are the Debian, Ubuntu, and snap paths.
func platformCandidates() []string {
	return []string{
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/snap/bin/chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
}
