//go:build windows

package chromepath

import (
	"strings"
	"testing"
)

func setRoots(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "LOCALAPPDATA"} {
		t.Setenv(env, vars[env])
	}
}

func assertCandidates(t *testing.T, want []string) {
	t.Helper()
	if got := Candidates(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Candidates() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A stripped environment still yields the default install locations, for both
// browsers.
func TestWindowsCandidatesWithoutEnvironment(t *testing.T) {
	setRoots(t, nil)
	assertCandidates(t, []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files\Chromium\Application\chrome.exe`,
		`C:\Program Files (x86)\Chromium\Application\chrome.exe`,
	})
}

// Every root contributes both browsers, Chrome under every root comes before any
// Chromium, and a root named by two variables is listed once.
func TestWindowsCandidatesFromEnvironment(t *testing.T) {
	setRoots(t, map[string]string{
		"ProgramFiles": `C:\Program Files`,
		"ProgramW6432": `C:\Program Files`,
		"LOCALAPPDATA": `C:\Users\me\AppData\Local`,
	})
	assertCandidates(t, []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Users\me\AppData\Local\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files\Chromium\Application\chrome.exe`,
		`C:\Users\me\AppData\Local\Chromium\Application\chrome.exe`,
		`C:\Program Files (x86)\Chromium\Application\chrome.exe`,
	})
}
