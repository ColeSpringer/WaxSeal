package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// spawnHelper launches this test binary in mode and completes Spawn's handshake
// against it. pidFile is where the helper records its own pid, which is the only
// way to watch a child whose Spawn returned no Browser. The returned Browser, when
// there is one, is the caller's to close.
func spawnHelper(t *testing.T, mode string, timeout time.Duration) (_ *Browser, pidFile string, _ error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile = filepath.Join(t.TempDir(), "helper.pid")
	// Spawn inherits this process's environment, so setting it here is what selects
	// the child's mode. t.Setenv restores it and refuses to run under t.Parallel.
	t.Setenv(waxsealTestHelperEnv, mode)
	t.Setenv(waxsealTestPIDFileEnv, pidFile)
	b, err := Spawn(context.Background(), self, nil, SpawnOptions{LaunchTimeout: timeout})
	return b, pidFile, err
}

// waitHelperPID reads the pid the helper recorded, waiting for it to appear.
func waitHelperPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(raw))); cerr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper never recorded its pid in %s", pidFile)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A spawned child must inherit both transport ends and be reachable over them,
// with no browser involved. This is the platform-specific half of the package:
// on Unix the ends arrive as fd 3 and fd 4, on Windows as two handle values in
// the argv, and the helper adopts whichever this platform uses.
func TestSpawnAgainstHelper(t *testing.T) {
	b, _, err := spawnHelper(t, "cdp-echo", 30*time.Second)
	if err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	// Spawn already completed one Browser.getVersion; a second proves the transport
	// keeps working rather than having answered once by luck.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var res struct {
		Product    string   `json:"product"`
		HelperArgv []string `json:"helperArgv"`
	}
	if err := b.conn.call(ctx, "", "Browser.getVersion", nil, &res); err != nil {
		t.Fatalf("getVersion over the inherited transport: %v", err)
	}
	if res.Product != "HelperChrome/1.0" {
		t.Errorf("product = %q, want the helper's", res.Product)
	}
	assertSpawnArgv(t, res.HelperArgv, b)
}

// A child that never answers must not leave a process behind when the handshake
// times out.
func TestSpawnHandshakeTimeoutKillsChild(t *testing.T) {
	b, pidFile, err := spawnHelper(t, "stall", 2*time.Second)
	if err == nil {
		_ = b.Close()
		t.Fatal("Spawn succeeded against a child that never answers")
	}
	if !strings.Contains(err.Error(), "launch handshake") {
		t.Errorf("error = %v, want it to name the handshake", err)
	}
	pid := waitHelperPID(t, pidFile)
	// Spawn force-closes on a failed handshake, which is what must leave no
	// process behind. Poll rather than checking once: the kill and the reap are
	// asynchronous.
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d was still running 10s after the handshake timeout; the failed launch leaked a process", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Spawn must report a launch failure rather than returning a Browser when the
// binary does not exist.
func TestSpawnMissingBinary(t *testing.T) {
	b, err := Spawn(context.Background(), filepathJoinNonexistent(t), nil, SpawnOptions{LaunchTimeout: time.Second})
	if err == nil {
		_ = b.Close()
		t.Fatal("Spawn succeeded with a nonexistent binary")
	}
	if !strings.Contains(err.Error(), "start chromium") {
		t.Errorf("error = %v, want it to name the start failure", err)
	}
}

// filepathJoinNonexistent returns a path inside a fresh temp dir that does not
// exist, so the failure is a missing binary rather than a permission problem.
func filepathJoinNonexistent(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "no-such-browser")
}

// A pipe pair's parent end must accept deadlines on every platform: the write
// stall guard and cancel-on-close both depend on it.
func TestPipePairParentIsPollable(t *testing.T) {
	for _, dir := range []pipeDir{pipeParentWrites, pipeParentReads} {
		p, err := newPipePair(dir, false)
		if err != nil {
			t.Fatalf("newPipePair(%d): %v", dir, err)
		}
		if err := p.parent.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
			t.Errorf("dir %d: parent SetReadDeadline: %v", dir, err)
		}
		if err := p.parent.SetWriteDeadline(time.Now().Add(time.Hour)); err != nil {
			t.Errorf("dir %d: parent SetWriteDeadline: %v", dir, err)
		}
		p.close()
	}
}

// closeChild releases only the parent's copy of the child end, which is what lets
// the response pipe reach EOF when the child exits. Closing the pair afterwards
// must still be safe.
func TestPipePairCloseChildIsIdempotent(t *testing.T) {
	p, err := newPipePair(pipeParentReads, false)
	if err != nil {
		t.Fatalf("newPipePair: %v", err)
	}
	p.closeChild()
	if p.child != nil {
		t.Error("closeChild left the child end referenced")
	}
	p.closeChild() // must not panic on a pair whose child is already gone
	p.close()
	if _, err := p.parent.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("parent read after close = %v, want os.ErrClosed", err)
	}
}

// helperArgvContains reports whether argv carries an element with prefix.
func helperArgvContains(argv []string, prefix string) (string, bool) {
	for _, a := range argv {
		if strings.HasPrefix(a, prefix) {
			return a, true
		}
	}
	return "", false
}

// marshalIndent is a small helper so a failure prints the argv readably.
func marshalIndent(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}
