package browser

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

const (
	// profilePrefix must remain consistent with profileDirPattern so the reaper
	// recognizes profiles created by os.MkdirTemp.
	profilePrefix = ".waxseal-"

	// creatorMarkerFile identifies a WaxSeal profile. Its advisory lock, rather
	// than the recorded PID, indicates whether the creator is still running.
	creatorMarkerFile = "creator.pid"
)

// profileDirPattern restricts cleanup to the numeric names created by
// os.MkdirTemp with profilePrefix. It excludes unrelated and legacy paths.
var profileDirPattern = regexp.MustCompile("^" + regexp.QuoteMeta(profilePrefix) + `[0-9]+$`)

// writeMarker records this process's PID in dir's marker file. The PID is for
// diagnostics only; the advisory lock determines liveness.
func writeMarker(dir string) error {
	return os.WriteFile(filepath.Join(dir, creatorMarkerFile), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// markProfileDir writes the marker and holds its advisory lock for the process
// lifetime. Advisory locks avoid false liveness results from PID reuse and PID
// namespaces. If marking or locking fails, the marker is removed so the reaper
// leaves the profile untouched.
func markProfileDir(dir string) {
	marker := filepath.Join(dir, creatorMarkerFile)
	if err := writeMarker(dir); err != nil {
		_ = os.Remove(marker)
		return
	}
	if !holdProfileLock(marker) {
		_ = os.Remove(marker)
	}
}

// profileState describes one profile directory considered by the reaper.
type profileState struct {
	path      string
	hasMarker bool
}

// classifyStaleProfiles returns marked profiles whose ownership lock is free.
// Markerless profiles are always retained. lockable is injected for tests.
func classifyStaleProfiles(states []profileState, lockable func(marker string) bool) []profileState {
	var remove []profileState
	for _, st := range states {
		if st.hasMarker && lockable(filepath.Join(st.path, creatorMarkerFile)) {
			remove = append(remove, st)
		}
	}
	return remove
}

// ReapStaleProfiles removes abandoned profile directories created by WaxSeal.
//
// A directory is removed only when its name matches profileDirPattern, it contains
// a creator marker, and the marker's advisory lock is free. Unmarked directories
// are left untouched. On Windows, markerLockable always returns false, so cleanup
// must be performed manually. Call ReapStaleProfiles before launching a browser.
func ReapStaleProfiles(log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	matches, err := filepath.Glob(filepath.Join(homeTmpBase(), profilePrefix+"*"))
	if err != nil {
		log.Warn("waxseal: profile sweep glob failed", "err", err)
		return
	}
	var states []profileState
	markerless := 0
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil || !fi.IsDir() || !profileDirPattern.MatchString(filepath.Base(path)) {
			continue
		}
		st := profileState{path: path}
		if _, err := os.Stat(filepath.Join(path, creatorMarkerFile)); err == nil {
			st.hasMarker = true
		} else {
			markerless++
		}
		states = append(states, st)
	}

	for _, st := range classifyStaleProfiles(states, markerLockable) {
		if err := os.RemoveAll(st.path); err != nil {
			log.Warn("waxseal: reap stale profile directory failed", "dir", st.path, "err", err)
			continue
		}
		log.Info("waxseal: reaped stale profile directory", "dir", st.path)
	}
	if markerless > 0 {
		log.Info("waxseal: left unmarked profile directories in place", "count", markerless)
	}
}
