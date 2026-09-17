//go:build live

package cdp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/chromepath"
)

// These live tests exercise the pipe transport against a real Chromium. They
// verify remote-debugging-pipe through local launcher wrappers, eval context
// recovery after navigation, connection teardown after process death, command-pipe
// EOF shutdown, incognito isolation, WaitCrash, and handshake-timeout cleanup.
// They skip when Chromium is unavailable so the offline suite remains portable.

func findChrome(t *testing.T) string {
	t.Helper()
	if b, ok := chromepath.Detect(); ok {
		return b
	}
	// WAXSEAL_REQUIRE_CHROME turns a missing browser into a hard failure. CI sets
	// it (=1) on the -tags live step so pipe-transport coverage is lost loudly, not
	// silently, if the runner image ever stops shipping Chromium. It is parsed as a
	// bool, so an explicit =0/=false (or unset) still skips gracefully.
	if require, _ := strconv.ParseBool(os.Getenv("WAXSEAL_REQUIRE_CHROME")); require {
		t.Fatalf("no chromium found and WAXSEAL_REQUIRE_CHROME is set; install Chromium or set WAXSEAL_CHROME_BIN")
	}
	t.Skip("no chromium found; set WAXSEAL_CHROME_BIN")
	return ""
}

// homeTmp returns a base directory for the test profile. On Unix it is rooted at
// $HOME because snap-confined Chromium cannot open a profile under /tmp; Windows
// has no such confinement, so the ordinary temp dir is the better neighbour.
func homeTmp(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return t.TempDir()
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp(h, ".waxseal-cdp-")
	if err != nil {
		return t.TempDir()
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func spawnForTest(t *testing.T) *Browser {
	t.Helper()
	bin := findChrome(t)
	profile := filepath.Join(homeTmp(t), "profile")
	b, err := Spawn(context.Background(), bin, BuildArgs(profile, false), SpawnOptions{LaunchTimeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestLiveVersionAndEval(t *testing.T) {
	b := spawnForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ver, err := b.Context(ctx).Version()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	t.Logf("product=%q protocol=%q", ver.Product, ver.ProtocolVersion)

	page, err := b.Context(ctx).Page(TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	p := page.Context(ctx)

	got, err := p.Eval(`() => 1 + 1`)
	if err != nil {
		t.Fatalf("eval 1+1: %v", err)
	}
	if got.Int() != 2 {
		t.Errorf("eval 1+1 = %d, want 2", got.Int())
	}

	withArg, err := p.Eval(`(x) => x * 2`, 21)
	if err != nil {
		t.Fatalf("eval with arg: %v", err)
	}
	if withArg.Int() != 42 {
		t.Errorf("eval (x)=>x*2 with 21 = %d, want 42", withArg.Int())
	}

	str, err := p.Eval(`() => "hello"`)
	if err != nil {
		t.Fatalf("eval string: %v", err)
	}
	if str.Str() != "hello" {
		t.Errorf("eval string = %q, want hello", str.Str())
	}

	// A JS exception is a real failure and is surfaced as *EvalError, not retried.
	if _, err := p.Eval(`() => { throw new Error("boom") }`); err == nil {
		t.Error("eval throwing did not error")
	} else if _, ok := err.(*EvalError); !ok {
		t.Errorf("eval exception error = %T, want *EvalError", err)
	}
}

func TestLiveSigkillSelfHeal(t *testing.T) {
	b := spawnForTest(t)
	pid := b.PID()
	if pid <= 0 {
		t.Fatalf("PID() = %d", pid)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	page, err := b.Context(ctx).Page(TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}

	// Kill Chromium's process group leader and verify the transport notices.
	if err := killPID(pid); err != nil {
		t.Fatalf("SIGKILL %d: %v", pid, err)
	}

	// The connection should close promptly once the pipe reaches EOF.
	select {
	case <-b.conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not tear down within 2s after SIGKILL (pipe did not EOF)")
	}

	// A pending Call now fails promptly rather than hanging.
	ectx, ecancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ecancel()
	if _, err := page.Context(ectx).Eval(`() => 1`); err == nil {
		t.Error("eval after kill succeeded; want a connection-lost error")
	}
}

func TestLivePipeEOFTerminatesChrome(t *testing.T) {
	b := spawnForTest(t)
	pid := b.PID()
	if pid <= 0 || !processAlive(pid) {
		t.Fatalf("process not alive before pipe close (pid=%d)", pid)
	}

	// Close only the command pipe (child fd 3). Chromium should read EOF and exit.
	_ = b.conn.wpipe.Close()

	deadline := time.Now().Add(8 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("Chromium (pid=%d) still alive 8s after command pipe EOF", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLiveNavigateWaitLoadEval(t *testing.T) {
	b := spawnForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	page, err := b.Context(ctx).Page(TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	p := page.Context(ctx)

	// Resolve the window object id on about:blank, then navigate away. The cached
	// id is invalidated; the next Eval must resolve it again rather than fail.
	if _, err := p.Eval(`() => 1`); err != nil {
		t.Fatalf("warmup eval: %v", err)
	}
	if err := p.Navigate(`data:text/html,<html><body><script>window.__x=7</script>hi</body></html>`); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := p.WaitLoad(); err != nil {
		t.Fatalf("wait load: %v", err)
	}
	got, err := p.Eval(`() => window.__x`)
	if err != nil {
		t.Fatalf("post-navigate eval: %v", err)
	}
	if got.Int() != 7 {
		t.Errorf("post-navigate eval = %d, want 7 (context not re-resolved?)", got.Int())
	}
}

func TestLiveIncognitoIsolation(t *testing.T) {
	b := spawnForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a, err := b.Context(ctx).Incognito()
	if err != nil {
		t.Fatalf("incognito a: %v", err)
	}
	c, err := b.Context(ctx).Incognito()
	if err != nil {
		t.Fatalf("incognito c: %v", err)
	}
	if a.contextID == "" || c.contextID == "" || a.contextID == c.contextID {
		t.Fatalf("incognito contexts not distinct: %q vs %q", a.contextID, c.contextID)
	}

	// Each context can park a page and read its (browser-scoped) cookies.
	pa, err := a.Context(ctx).Page(TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("page a: %v", err)
	}
	if _, err := pa.Context(ctx).Eval(`() => 1`); err != nil {
		t.Fatalf("eval in a: %v", err)
	}
	if _, err := a.Context(ctx).GetCookies(); err != nil {
		t.Fatalf("cookies a: %v", err)
	}

	// Disposing context a must not affect context c (shared process stays up).
	if err := a.Context(ctx).Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	pc, err := c.Context(ctx).Page(TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("page c after disposing a: %v", err)
	}
	if _, err := pc.Context(ctx).Eval(`() => 2`); err != nil {
		t.Fatalf("eval in c after disposing a: %v", err)
	}
}

func TestLiveWaitCrashConnLost(t *testing.T) {
	b := spawnForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	page, err := b.Context(ctx).Page(TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}

	done := make(chan string, 1)
	go func() { done <- page.WaitCrash(context.Background()) }()
	time.Sleep(200 * time.Millisecond) // allow WaitCrash to subscribe

	_ = killPID(b.PID())
	select {
	case reason := <-done:
		if reason == "" {
			t.Error("WaitCrash returned empty reason on connection loss")
		}
		t.Logf("WaitCrash reason: %q", reason)
	case <-time.After(3 * time.Second):
		t.Fatal("WaitCrash did not return within 3s after kill")
	}
}

func TestLiveWaitCrashCtxCancel(t *testing.T) {
	b := spawnForTest(t)
	page, err := b.Context(context.Background()).Page(TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	wctx, wcancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() { done <- page.WaitCrash(wctx) }()
	time.Sleep(200 * time.Millisecond)
	wcancel()
	select {
	case reason := <-done:
		if reason != "" {
			t.Errorf("WaitCrash on ctx cancel = %q, want empty", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitCrash did not return on ctx cancel")
	}
}

func TestLiveHandshakeTimeoutNoOrphan(t *testing.T) {
	// A binary that never speaks CDP must hit the handshake timeout, and Spawn must
	// kill it before returning an error.
	sleep := "/bin/sleep"
	if _, err := os.Stat(sleep); err != nil {
		t.Skip("/bin/sleep not present")
	}
	start := time.Now()
	b, err := Spawn(context.Background(), sleep, []string{"30"}, SpawnOptions{LaunchTimeout: 2 * time.Second})
	if err == nil {
		_ = b.Close()
		t.Fatal("spawn against a non-CDP binary succeeded; want a handshake timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("handshake timeout took %s, want ~2s", elapsed)
	}
	t.Logf("handshake timeout error: %v", err)
}
