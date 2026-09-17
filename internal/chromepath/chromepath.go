// Package chromepath holds the one list of places a Chromium or Chrome install is
// looked for, and the rule that picks one. It is its own package because
// internal/browser imports internal/cdp, so cdp's live tests cannot import
// browser back to reach either.
package chromepath

import "os"

// Candidates returns this platform's install locations, in the order to try them.
// Edge is deliberately absent: it reports a different brand list and user agent,
// so picking it up would silently change the identity a token binds to.
func Candidates() []string { return platformCandidates() }

// Detect returns the browser to launch: WAXSEAL_CHROME_BIN when set, which is the
// escape hatch for any browser including the ones Candidates leaves out, and
// otherwise the first candidate that exists as a file. ok is false when neither
// names one.
func Detect() (bin string, ok bool) {
	if b := os.Getenv("WAXSEAL_CHROME_BIN"); b != "" {
		return b, true
	}
	for _, p := range platformCandidates() {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}
