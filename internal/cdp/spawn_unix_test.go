//go:build unix

package cdp

import "testing"

// assertSpawnArgv checks what the helper was launched with. TestArgvGolden
// pins BuildArgs; this pins that Spawn appends nothing after it, since on Unix
// the transport travels as fd 3 and fd 4.
func assertSpawnArgv(t *testing.T, argv []string, _ *Browser) {
	t.Helper()
	if flag, ok := helperArgvContains(argv, ioPipesFlagPrefix); ok {
		t.Errorf("the unix spawn path added %q; the transport is fd 3 and fd 4 here, not argv", flag)
	}
	if len(argv) != 1 {
		t.Errorf("helper argv = %s, want only the binary path", marshalIndent(argv))
	}
}
