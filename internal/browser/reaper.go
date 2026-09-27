package browser

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

const (
	// profilePrefix must remain consistent with profileDirPattern so the reaper
	// recognizes profiles created by os.MkdirTemp.
	profilePrefix = ".waxseal-"

	// creatorMarkerFile identifies a WaxSeal profile. Its ownership lock, rather
	// than the recorded PID, indicates whether the creator is still running.
	creatorMarkerFile = "creator.pid"

	// markerGrace keeps the reaper off a freshly written marker: markProfileDir
	// writes it before taking the lock, and a concurrent sweep (every command
	// reaps before launching) would delete a browser still starting. Markers
	// date from launch, so this delays an abandoned profile one sweep at most.
	markerGrace = 10 * time.Second
)

// profileDirPattern restricts cleanup to the numeric names created by
// os.MkdirTemp with profilePrefix. It excludes unrelated and legacy paths.
var profileDirPattern = regexp.MustCompile("^" + regexp.QuoteMeta(profilePrefix) + `[0-9]+$`)

// writeMarker records this process's PID in dir's marker file. The PID is for
// diagnostics only; the ownership lock determines liveness.
func writeMarker(dir string) error {
	return os.WriteFile(filepath.Join(dir, creatorMarkerFile), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// markProfileAbandoned marks a directory that is being left behind, so that a
// sweep collects it. The marker is dated past markerGrace, which is there for a
// launch that has not taken its lock yet: this directory has no owner left to
// wait for, and a marker dated now would only hide it from the next sweep.
func markProfileAbandoned(dir string) error {
	if err := writeMarker(dir); err != nil {
		return err
	}
	stale := time.Now().Add(-2 * markerGrace)
	return os.Chtimes(filepath.Join(dir, creatorMarkerFile), stale, stale)
}

// removeProfile removes a profile directory, taking creator.pid last. RemoveAll
// on the whole directory would continue past a file it cannot delete and unlink
// the marker, and every later sweep retains a markerless directory. Whatever
// stops the removal (a pinned file, a kill) has to leave a marked directory.
func removeProfile(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		if e.Name() == creatorMarkerFile {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if err := os.Remove(filepath.Join(dir, creatorMarkerFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		// The directory outlived its marker: something landed in it after the scan,
		// or the directory itself is held. Mark it again for a later sweep.
		_ = markProfileAbandoned(dir)
		return err
	}
	return nil
}

// markProfileDir writes the marker and takes its ownership lock, returning the
// open file holding the lock. The caller closes it via profileHandle.cleanup,
// which knows the platform's order relative to removing the directory. A held
// handle avoids the false liveness results a recorded PID gives under PID reuse
// and PID namespaces. If marking or locking fails it removes the marker and
// returns nil, so the reaper leaves the profile alone.
func markProfileDir(dir string) *os.File {
	marker := filepath.Join(dir, creatorMarkerFile)
	if err := writeMarker(dir); err != nil {
		_ = os.Remove(marker)
		return nil
	}
	lock, ok := holdProfileLock(marker)
	if !ok {
		_ = os.Remove(marker)
		return nil
	}
	return lock
}

// profileState describes one profile directory considered by the reaper.
type profileState struct {
	path      string
	hasMarker bool
	fresh     bool // the marker was written within markerGrace
}

// classifyStaleProfiles returns marked profiles whose ownership lock is free and
// whose marker is not newly written. Markerless and fresh profiles are always
// retained. lockable is injected for tests.
func classifyStaleProfiles(states []profileState, lockable func(marker string) bool) []profileState {
	var remove []profileState
	for _, st := range states {
		if st.hasMarker && !st.fresh && lockable(filepath.Join(st.path, creatorMarkerFile)) {
			remove = append(remove, st)
		}
	}
	return remove
}

// ReapStaleProfiles removes abandoned profile directories created by WaxSeal.
// Call it before launching a browser. A directory is removed only when its name
// matches profileDirPattern, its creator marker is older than markerGrace, and
// the marker's ownership lock is free; an unmarked directory is never removed.
// The kernel releases the lock (a flock on Unix, an exclusive open on Windows)
// on any exit, so it cannot go stale.
func ReapStaleProfiles(log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	matches, err := filepath.Glob(filepath.Join(profileBase(), profilePrefix+"*"))
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
		if mi, err := os.Stat(filepath.Join(path, creatorMarkerFile)); err == nil {
			st.hasMarker = true
			st.fresh = time.Since(mi.ModTime()) < markerGrace
		} else {
			markerless++
		}
		states = append(states, st)
	}

	for _, st := range classifyStaleProfiles(states, markerLockable) {
		if err := removeProfile(st.path); err != nil {
			log.Warn("waxseal: reap stale profile directory failed", "dir", st.path, "err", err)
			continue
		}
		log.Info("waxseal: reaped stale profile directory", "dir", st.path)
	}
	if markerless > 0 {
		log.Info("waxseal: left unmarked profile directories in place", "count", markerless)
	}
}
