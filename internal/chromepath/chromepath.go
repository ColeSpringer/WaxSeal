// Package chromepath holds the one list of places a Chromium or Chrome install is
// looked for. It is its own package because internal/browser imports internal/cdp,
// so cdp's live tests cannot import browser back to reach the list.
// WAXSEAL_CHROME_BIN overrides it and is handled by the callers.
package chromepath

// Candidates returns this platform's install locations, in the order to try them.
// Edge is deliberately absent: it reports a different brand list and user agent,
// so picking it up would silently change the identity a token binds to.
func Candidates() []string { return platformCandidates() }
