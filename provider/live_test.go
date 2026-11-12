//go:build (e2e || live) && unix

package provider_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// 127.0.0.1 stays reachable for the client-hint page a launch serves itself.
	wrapper := filepath.Join(t.TempDir(), "chromium-offline")
	script := "#!/bin/sh\nexec \"$WAXSEAL_TEST_REAL_CHROME\" --host-resolver-rules='MAP * ~NOTFOUND, EXCLUDE 127.0.0.1' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write the browser wrapper: %v", err)
	}
	t.Setenv("WAXSEAL_TEST_REAL_CHROME", chrome)
	t.Setenv("WAXSEAL_CHROME_BIN", wrapper)

	srv, ln := newInProcessDaemon(t, server.Config{})
	base := serveDaemon(t, srv, ln)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Nothing has launched yet. The first request that needs a session runs
	// the launch and is refused inside the cool-down its failure opened, which
	// the README puts at 15 seconds for a first failure.
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
