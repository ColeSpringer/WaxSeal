//go:build unix

package cdp

import "testing"

// assertSpawnArgv checks what the helper was launched with. On Unix the spawn
// path adds nothing to the argv: the transport travels as fd 3 and fd 4 by
// convention, so the argv the caller passed is the argv the child sees.
// TestArgvGolden pins BuildArgs; this pins that nothing is appended after it.
func assertSpawnArgv(t *testing.T, argv []string, _ *Browser) {
	t.Helper()
	if flag, ok := helperArgvContains(argv, ioPipesFlagPrefix); ok {
		t.Errorf("the unix spawn path added %q; the transport is fd 3 and fd 4 here, not argv", flag)
	}
	if len(argv) != 1 {
		t.Errorf("helper argv = %s, want only the binary path", marshalIndent(argv))
	}
}
