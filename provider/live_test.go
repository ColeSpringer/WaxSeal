//go:build (e2e || live) && unix

package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/internal/chromepath"
	"github.com/colespringer/waxseal/server"
	waxtap "github.com/colespringer/waxtap/v3"
)

// TestFailedLaunchPingHTTP asks WaxTap's own probe about a daemon whose session
// launches fail: a healthy no-session that carries the count, the launch's
// cause, and what is left of the cool-down. It needs a browser but no network,
// so -tags live runs it too: the wrapper leaves Chromium able to resolve only
// loopback, and every launch fails at the landing page.
func TestFailedLaunchPingHTTP(t *testing.T) {
	if os.Getenv("WAXSEAL_URL") != "" {
		t.Skip("failed-launch test requires an in-process daemon")
	}
	chrome, ok := chromepath.Detect()
	if !ok {
		// As the CDP live tests do: CI sets WAXSEAL_REQUIRE_CHROME so a runner
		// without a browser fails instead of dropping the test.
		if require, _ := strconv.ParseBool(os.Getenv("WAXSEAL_REQUIRE_CHROME")); require {
			t.Fatal("no chromium found and WAXSEAL_REQUIRE_CHROME is set; install Chromium or set WAXSEAL_CHROME_BIN")
		}
		t.Skip("no chromium found; set WAXSEAL_CHROME_BIN")
	}
	offlineChrome(t, chrome)

	srv, ln := newInProcessDaemon(t, server.Config{})
	base := serveDaemon(t, srv, ln)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Nothing has launched yet. The first request that needs a session runs
	// the launch and is refused inside the cool-down its failure opened, which
	// docs/api.md puts at 15 seconds for a first failure.
	const firstCooldown = 15 * time.Second
	_, err := client.New(base).Session(ctx)
	refusal, ok := errors.AsType[*client.APIError](err)
	if !ok {
		t.Fatalf("session = %v, want the daemon's refusal", err)
	}
	if refusal.StatusCode != http.StatusServiceUnavailable || refusal.Code != server.CodeNoSession {
		t.Fatalf("session refused as %d %q, want %d %q: %v",
			refusal.StatusCode, refusal.Code, http.StatusServiceUnavailable, server.CodeNoSession, err)
	}
	if refusal.RetryAfter <= 0 || refusal.RetryAfter > firstCooldown {
		t.Fatalf("session refusal states a wait of %v, want one inside the first cool-down of %v", refusal.RetryAfter, firstCooldown)
	}

	health, err := waxtap.PingSidecar(ctx, base)
	if err != nil {
		t.Fatalf("PingSidecar: %v, want a healthy answer, since the probe observed no loss", err)
	}
	if health.OK || health.Probe != server.PingProbeTenant || health.Reason != server.PingReasonNoSession {
		t.Errorf("health = %+v, want a tenant probe that is not ok for %q", health, server.PingReasonNoSession)
	}
	if health.LaunchFailures < 1 {
		t.Errorf("LaunchFailures = %d, want the failed launch counted", health.LaunchFailures)
	}
	if health.RetryAfter <= 0 || health.RetryAfter > refusal.RetryAfter {
		t.Errorf("RetryAfter = %v, want what is left of the refusal's %v", health.RetryAfter, refusal.RetryAfter)
	}
	// An idle no-session has an error too, so only the cause shows that the
	// account is the launch's.
	const cause = "ERR_NAME_NOT_RESOLVED"
	if !strings.Contains(health.Error, cause) {
		t.Errorf("Error = %q, want the failed launch's cause, %s", health.Error, cause)
	}
}

// offlineChrome points the daemon at a wrapper that leaves Chromium able to
// resolve only loopback, so every session launch fails at the landing page
// without touching the network. 127.0.0.1 stays reachable for the client-hint
// page a launch serves itself.
func offlineChrome(t *testing.T, chrome string) {
	t.Helper()
	wrapper := filepath.Join(t.TempDir(), "chromium-offline")
	script := "#!/bin/sh\nexec \"$WAXSEAL_TEST_REAL_CHROME\" --host-resolver-rules='MAP * ~NOTFOUND, EXCLUDE 127.0.0.1' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write the browser wrapper: %v", err)
	}
	t.Setenv("WAXSEAL_TEST_REAL_CHROME", chrome)
	t.Setenv("WAXSEAL_CHROME_BIN", wrapper)
}

// TestIdleBrowserCloseAndWakeHTTP runs a daemon with an idle timeout against a
// real Chromium. The browser exits once nothing has used it for the timeout, a
// probe finds the daemon healthy and leaves it asleep, and the next request
// that needs a session starts a browser again. Offline, as above: the launch
// that wakes it still fails at the landing page, which is all this needs.
func TestIdleBrowserCloseAndWakeHTTP(t *testing.T) {
	if os.Getenv("WAXSEAL_URL") != "" {
		t.Skip("idle-close test requires an in-process daemon")
	}
	chrome, ok := chromepath.Detect()
	if !ok {
		if require, _ := strconv.ParseBool(os.Getenv("WAXSEAL_REQUIRE_CHROME")); require {
			t.Fatal("no chromium found and WAXSEAL_REQUIRE_CHROME is set; install Chromium or set WAXSEAL_CHROME_BIN")
		}
		t.Skip("no chromium found; set WAXSEAL_CHROME_BIN")
	}
	offlineChrome(t, chrome)

	const idle = 3 * time.Second
	srv, ln := newInProcessDaemon(t, server.Config{IdleTimeout: idle})
	base := serveDaemon(t, srv, ln)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	first := srv.BrowserPID()
	if first == 0 {
		t.Fatal("BrowserPID = 0 at startup, want the browser the daemon launched")
	}
	waitBrowserClosed(t, srv, first, idle)

	// The probe is healthy, says why nothing is live, and wakes nothing.
	health, err := waxtap.PingSidecar(ctx, base)
	if err != nil {
		t.Fatalf("PingSidecar on an idle daemon: %v, want a healthy answer", err)
	}
	if health.OK || health.Reason != server.PingReasonNoSession || health.LaunchFailures != 0 || health.BrowserRelaunched {
		t.Errorf("health = %+v, want a benign %q with nothing failed or relaunched", health, server.PingReasonNoSession)
	}
	if body := rawPing(t, ctx, base); body["browser_idle"] != true {
		t.Errorf("/ping body = %v, want browser_idle:true", body)
	}
	if pid := srv.BrowserPID(); pid != 0 {
		t.Fatalf("BrowserPID = %d after a probe, want 0: a probe must not wake an idle browser", pid)
	}

	// A request that needs a session does. Its launch fails offline, but in a
	// browser the daemon had to start for it.
	if _, err := client.New(base).Session(ctx); err == nil {
		t.Fatal("session succeeded offline; the wrapper is not in effect")
	}
	second := srv.BrowserPID()
	if second == 0 || second == first {
		t.Fatalf("BrowserPID = %d after a request (was %d before the idle close), want a new browser", second, first)
	}
	if body := rawPing(t, ctx, base); body["browser_idle"] != false || body["consecutive_launch_failures"] != float64(1) {
		t.Errorf("/ping body after the wake = %v, want browser_idle:false and the failed launch counted", body)
	}

	// Left alone again, it goes back to sleep. The counter moves once the close
	// has finished, a moment after the process is gone.
	waitBrowserClosed(t, srv, second, idle)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		got := rawGet(t, ctx, base+"/metrics")["browser_idle_closes"]
		if got == float64(2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("browser_idle_closes = %v, want 2", got)
		}
	}
}

// waitBrowserClosed waits for the idle close of the browser pid: the daemon
// reports none, and the process is gone.
func waitBrowserClosed(t *testing.T, srv *server.Server, pid int, idle time.Duration) {
	t.Helper()
	deadline := time.Now().Add(idle + 20*time.Second)
	// Signal 0 probes for existence. The daemon stops reporting the browser
	// when it commits to the close, a moment before the process is reaped.
	for srv.BrowserPID() != 0 || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatalf("browser %d: the daemon reports pid %d and signal 0 answers %v, %s past an idle timeout of %s",
				pid, srv.BrowserPID(), syscall.Kill(pid, 0), 20*time.Second, idle)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// rawPing returns the strict /ping body as decoded JSON, for the fields
// WaxTap's probe does not carry.
func rawPing(t *testing.T, ctx context.Context, base string) map[string]any {
	t.Helper()
	return rawGet(t, ctx, base+"/ping?strict=true")
}

// rawGet decodes the JSON object a 200 from url carries.
func rawGet(t *testing.T, ctx context.Context, url string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return body
}
