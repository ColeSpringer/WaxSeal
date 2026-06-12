//go:build windows

package browser

// guardPathUnsafe is a no-op on Windows because the Unix ownership checks do not
// apply.
func guardPathUnsafe(string) (bool, string) { return false, "" }

// holdProfileLock reports that advisory profile locks are unavailable on Windows.
func holdProfileLock(string) bool { return false }

// markerLockable returns false on Windows because the startup reaper's advisory
// lock protocol is not implemented there.
func markerLockable(string) bool { return false }
