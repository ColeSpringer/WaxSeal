package minter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
)

// errRefused is the launch failure of an address YouTube answers without an
// identity.
var errRefused = errors.New("waxseal: ytcfg visitor_data not available before deadline")

// newRefusedMinter returns a Minter whose every launch fails with errRefused,
// and the count of launches it attempted.
func newRefusedMinter() (*Minter, *atomic.Int64) {
	var attempts atomic.Int64
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		attempts.Add(1)
		return nil, errRefused
	}
	return m, &attempts
}

// expireLaunchCooldown moves the open launch cool-down into the past, so the
// next request launches without the test sleeping through the window.
func expireLaunchCooldown(m *Minter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.launchFailedAt = m.launchFailedAt.Add(-launchRetryMax)
}

// requireWait asserts err is a refusal that states a wait in (0, max].
func requireWait(t *testing.T, err error, max time.Duration) time.Duration {
	t.Helper()
	ra, ok := errors.AsType[*RetryAfterError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *RetryAfterError", err, err)
	}
	if ra.After <= 0 || ra.After > max {
		t.Fatalf("After = %v, want a wait in (0, %v]", ra.After, max)
	}
	return ra.After
}

// A failed launch opens a cool-down: the request that paid for it is told the
// whole window, and requests inside the window are refused without launching.
// Before the cool-down every request on a refused address launched a browser
// context and loaded a page of its own.
func TestFailedLaunchOpensCooldown(t *testing.T) {
	m, attempts := newRefusedMinter()
	ctx := context.Background()

	_, _, err := m.Mint(ctx, "gvs", "b")
	if !errors.Is(err, errRefused) {
		t.Fatalf("first mint err = %v, want the launch failure", err)
	}
	if errors.Is(err, ErrLaunchCooldown) {
		t.Errorf("first mint err = %v, want the launch's own failure, not a cool-down refusal", err)
	}
	if got := requireWait(t, err, launchRetryBase); got != launchRetryBase {
		t.Errorf("first refusal After = %v, want the whole window %v", got, launchRetryBase)
	}

	for _, call := range []struct {
		name string
		do   func() error
	}{
		{"mint", func() error { _, _, err := m.Mint(ctx, "gvs", "b"); return err }},
		{"player-context", func() error { _, _, err := m.PlayerContext(ctx, "vid"); return err }},
		{"session", func() error { _, _, _, err := m.SessionSnapshot(ctx); return err }},
		{"warm", func() error { return m.Warm(ctx) }},
	} {
		err := call.do()
		if !errors.Is(err, ErrLaunchCooldown) {
			t.Errorf("%s inside the window: err = %v, want ErrLaunchCooldown", call.name, err)
			continue
		}
		if !errors.Is(err, errRefused) {
			t.Errorf("%s inside the window: err = %v, want it to name the failure that opened the window", call.name, err)
		}
		requireWait(t, err, launchRetryBase)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("launch attempts = %d, want 1 (requests inside the window must not launch)", got)
	}
	if got := m.metrics.LaunchFailures.Load(); got != 1 {
		t.Errorf("launch_failures = %d, want 1 (a refusal is not a failed attempt)", got)
	}
	if got := m.metrics.LaunchCooldownRefusals.Load(); got != 4 {
		t.Errorf("launch_cooldown_refusals = %d, want 4 (one per request inside the window)", got)
	}
}

// gatedLauncher installs a launch that blocks until release is closed and then
// returns what outcome yields. Like a real launch it stops early only when its
// own context ends, so a test can tell whose context the Minter handed it.
func gatedLauncher(m *Minter, outcome func() (minterSession, error)) (attempts *atomic.Int64, started, release chan struct{}) {
	attempts, started, release = new(atomic.Int64), make(chan struct{}, 16), make(chan struct{})
	m.launch = func(ctx context.Context) (minterSession, error) {
		attempts.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return outcome()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return attempts, started, release
}

// awaitPageHeld blocks until some request holds mintMu, which with a launch in
// flight means it is inside ensure waiting on that launch.
func awaitPageHeld(t *testing.T, m *Minter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.mintMu.TryLock() {
		m.mintMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("no request took the page")
		}
		time.Sleep(time.Millisecond)
	}
}

// A consumer whose timeout is shorter than a refused launch must not keep the
// cool-down from opening. The launch runs on the Minter's clock, so it reaches
// its verdict after the caller has gone, and the callers that left cost one
// launch between them rather than one each.
func TestLaunchOutlivesAnImpatientCaller(t *testing.T) {
	m := newBareMinter(0, 0)
	attempts, started, release := gatedLauncher(m, func() (minterSession, error) { return nil, errRefused })

	for i := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, _, err := m.Mint(ctx, "gvs", "b")
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("impatient request %d: err = %v, want its own deadline", i+1, err)
		}
	}
	<-started
	if got := attempts.Load(); got != 1 {
		t.Fatalf("launch attempts = %d, want 1: each caller that left must not start another", got)
	}

	close(release)
	// The verdict lands without any request waiting for it.
	deadline := time.Now().Add(5 * time.Second)
	for m.MetricsSnapshot()["consecutive_launch_failures"] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned launch was never graded")
		}
		time.Sleep(time.Millisecond)
	}
	_, _, err := m.Mint(context.Background(), "gvs", "b")
	if !errors.Is(err, ErrLaunchCooldown) || !errors.Is(err, errRefused) {
		t.Errorf("request after the verdict: err = %v, want a cool-down refusal naming the failure", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("launch attempts = %d, want 1", got)
	}
}

// The same holds when the launch succeeds: a caller that gave up leaves the
// next one a session instead of a second cold start.
func TestAbandonedLaunchStillPublishesItsSession(t *testing.T) {
	m := newBareMinter(0, 0)
	attempts, started, release := gatedLauncher(m, func() (minterSession, error) { return &fakeSession{mint: okMint}, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Warm(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("impatient Warm: err = %v, want its own deadline", err)
	}
	<-started
	close(release)

	if _, cached, err := m.Mint(context.Background(), "gvs", "vd"); err != nil || !cached {
		t.Fatalf("mint after the abandoned launch: cached=%v err=%v, want its pre-minted token", cached, err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("launch attempts = %d, want 1", got)
	}
	if got := m.Generation(); got != 1 {
		t.Errorf("generation = %d, want 1", got)
	}
}

// A request that arrives while a launch is in flight waits on that launch and
// shares its outcome, the failure included: it is told the whole window, and
// it does not launch again once the first one fails.
func TestWaiterSharesTheFailureOfTheLaunchItQueuedOn(t *testing.T) {
	m := newBareMinter(0, 0)
	attempts, started, release := gatedLauncher(m, func() (minterSession, error) { return nil, errRefused })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Warm(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the caller that started the launch: err = %v, want its own deadline", err)
	}
	<-started

	waiter := make(chan error, 1)
	go func() { _, _, err := m.Mint(context.Background(), "gvs", "b"); waiter <- err }()
	awaitPageHeld(t, m)
	close(release)

	err := <-waiter
	if !errors.Is(err, errRefused) || errors.Is(err, ErrLaunchCooldown) {
		t.Fatalf("waiter err = %v, want the launch's own failure", err)
	}
	if got := requireWait(t, err, launchRetryBase); got != launchRetryBase {
		t.Errorf("waiter After = %v, want the whole window %v", got, launchRetryBase)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("launch attempts = %d, want 1", got)
	}
	if got := m.metrics.LaunchFailures.Load(); got != 1 {
		t.Errorf("launch_failures = %d, want 1 (counted per launch, not per waiter)", got)
	}
}

// The budget covers what one launch can legitimately contain: a relaunch of
// the shared Chromium, the navigation, and the attestation behind it. Graded
// short of that, a slow but healthy launch opens a cool-down nothing earned.
func TestLaunchBudgetCoversARelaunchAndANavigation(t *testing.T) {
	const relaunchHandshake, navigation = 60 * time.Second, 45 * time.Second
	if floor := relaunchHandshake + navigation + 30*time.Second; launchBudget < floor {
		t.Errorf("launchBudget = %v, want at least %v", launchBudget, floor)
	}
}

// A launch that never returns is cut off by its own budget and graded like any
// other failure: no request's deadline bounds it any more.
func TestLaunchBudgetIsGraded(t *testing.T) {
	defer func(d time.Duration) { launchBudget = d }(launchBudget)
	launchBudget = 30 * time.Millisecond
	m := newBareMinter(0, 0)
	attempts, _, _ := gatedLauncher(m, nil) // never released: only its context ends it

	err := m.Warm(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("Warm = %v, want the launch's own budget running out", err)
	}
	if got := requireWait(t, err, launchRetryBase); got != launchRetryBase {
		t.Errorf("After = %v, want the whole window %v", got, launchRetryBase)
	}
	if err := m.Warm(context.Background()); !errors.Is(err, ErrLaunchCooldown) {
		t.Errorf("Warm inside the window = %v, want a cool-down refusal", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("launch attempts = %d, want 1", got)
	}
}

// Close ends a launch in flight: nothing would close the session it produced.
func TestCloseCancelsALaunchInFlight(t *testing.T) {
	m := newBareMinter(0, 0)
	_, started, _ := gatedLauncher(m, nil)

	done := make(chan error, 1)
	go func() { done <- m.Warm(context.Background()) }()
	<-started
	m.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Warm across Close = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the launch in flight")
	}
	if got := m.MetricsSnapshot()["consecutive_launch_failures"]; got != 0 {
		t.Errorf("consecutive_launch_failures = %v, want 0 (a shutdown is not a verdict)", got)
	}
}

// Each consecutive failure doubles the window up to launchRetryMax, and a
// request past the window is what pays for the next attempt.
func TestLaunchCooldownBacksOff(t *testing.T) {
	m, attempts := newRefusedMinter()
	ctx := context.Background()
	want := []time.Duration{
		15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		5 * time.Minute, 5 * time.Minute,
	}
	for i, w := range want {
		_, _, err := m.Mint(ctx, "gvs", "b")
		if errors.Is(err, ErrLaunchCooldown) {
			t.Fatalf("attempt %d was refused by the cool-down, want a launch: %v", i+1, err)
		}
		if got := requireWait(t, err, launchRetryMax); got != w {
			t.Errorf("window after failure %d = %v, want %v", i+1, got, w)
		}
		expireLaunchCooldown(m)
	}
	if got := attempts.Load(); got != int64(len(want)) {
		t.Errorf("launch attempts = %d, want %d", got, len(want))
	}
}

// An attested session ends the streak: the next failure opens the base window
// again, not the one the earlier streak had reached.
func TestAttestedSessionClearsLaunchCooldown(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		if fail.Load() {
			return nil, errRefused
		}
		return &fakeSession{mint: okMint}, nil
	}
	ctx := context.Background()
	for range 3 {
		if err := m.Warm(ctx); err == nil {
			t.Fatal("Warm succeeded on a failing launcher")
		}
		expireLaunchCooldown(m)
	}

	fail.Store(false)
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("Warm after the refusal lifted: %v", err)
	}
	if got := m.MetricsSnapshot()["consecutive_launch_failures"]; got != 0 {
		t.Errorf("consecutive_launch_failures = %v after an attested session, want 0", got)
	}

	fail.Store(true)
	crashUnder(m)
	err := m.Warm(ctx)
	if got := requireWait(t, err, launchRetryMax); got != launchRetryBase {
		t.Errorf("window after the next failure = %v, want the base %v", got, launchRetryBase)
	}
}

// A launch that did not run to a verdict opens no window: the pool is shut or
// backing off on its own clock, or the browser died under it. None of them
// says the next launch would fail.
func TestUngradedLaunchFailuresOpenNoCooldown(t *testing.T) {
	backoff := &browser.RelaunchBackoffError{Wait: 7 * time.Second, Streak: 2}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"pool backoff", backoff},
		{"pool closed", browser.ErrPoolClosed},
		{"browser died", fmt.Errorf("waxseal: navigate landing page: %w", browser.ErrConnClosed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int64
			m := newBareMinter(0, 0)
			m.launch = func(context.Context) (minterSession, error) {
				attempts.Add(1)
				return nil, tc.err
			}
			for range 2 {
				_, _, err := m.Mint(context.Background(), "gvs", "b")
				if !errors.Is(err, tc.err) || errors.Is(err, ErrLaunchCooldown) {
					t.Fatalf("err = %v, want the launch's own failure each time", err)
				}
			}
			if got := attempts.Load(); got != 2 {
				t.Errorf("launch attempts = %d, want 2 (no window between them)", got)
			}
			if got := m.MetricsSnapshot()["consecutive_launch_failures"]; got != 0 {
				t.Errorf("consecutive_launch_failures = %v, want 0", got)
			}
		})
	}
}

// A token the aged generation cached outlives a replacement that cannot be
// launched, and the cool-down must not take that away: the request is served
// from the cache whether its launch failed or was refused. Only the failed
// launch warns. A refusal is instant, so warning for each would log at the
// request rate for the whole window.
func TestLaunchCooldownKeepsServingTheAgedCache(t *testing.T) {
	var attempts atomic.Int64
	var fail atomic.Bool
	var logs bytes.Buffer
	m := newBareMinter(0, 0)
	m.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m.launch = func(context.Context) (minterSession, error) {
		attempts.Add(1)
		if fail.Load() {
			return nil, errRefused
		}
		return &fakeSession{mint: okMint}, nil
	}
	ctx := context.Background()
	if _, cached, err := m.Mint(ctx, "gvs", "b"); err != nil || cached {
		t.Fatalf("first mint: cached=%v err=%v, want a fresh token", cached, err)
	}
	m.mu.Lock()
	m.attestedAt = time.Now().Add(-48 * time.Hour)
	m.mu.Unlock()
	fail.Store(true)

	const requests = 5
	for i := range requests {
		if _, cached, err := m.Mint(ctx, "gvs", "b"); err != nil || !cached {
			t.Fatalf("mint %d on the aged generation: cached=%v err=%v, want the cached token", i+1, cached, err)
		}
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("launch attempts = %d, want 2 (the first session, then one failed replacement)", got)
	}
	const line = "serving a cached token from the aged generation"
	var warns, debugs int
	for l := range strings.Lines(logs.String()) {
		if !strings.Contains(l, line) {
			continue
		}
		switch {
		case strings.Contains(l, "level=WARN"):
			warns++
		case strings.Contains(l, "level=DEBUG"):
			debugs++
		}
	}
	if warns != 1 || debugs != requests-1 {
		t.Errorf("aged-cache lines: %d at warn, %d at debug; want 1 and %d", warns, debugs, requests-1)
	}
}

// With no session, Health says why: how many launches have failed in a row,
// what the last one hit, and when the next may run.
func TestHealthReportsLaunchFailures(t *testing.T) {
	m, _ := newRefusedMinter()
	ctx := context.Background()

	snap, live, err := m.Health(ctx)
	if live || !errors.Is(err, ErrNoSession) || snap.ConsecutiveLaunchFailures != 0 || snap.LaunchRetryAfterSecs != 0 {
		t.Fatalf("Health before any launch = (%+v, live=%v, %v), want a bare no-session", snap, live, err)
	}

	if err := m.Warm(ctx); err == nil {
		t.Fatal("Warm succeeded on a failing launcher")
	}
	snap, live, err = m.Health(ctx)
	if live || !errors.Is(err, ErrNoSession) {
		t.Fatalf("Health = (live=%v, %v), want ErrNoSession", live, err)
	}
	if !strings.Contains(err.Error(), "visitor_data not available") {
		t.Errorf("Health err = %v, want it to carry the launch failure", err)
	}
	if snap.ConsecutiveLaunchFailures != 1 {
		t.Errorf("ConsecutiveLaunchFailures = %d, want 1", snap.ConsecutiveLaunchFailures)
	}
	if max := int(launchRetryBase / time.Second); snap.LaunchRetryAfterSecs < 1 || snap.LaunchRetryAfterSecs > max {
		t.Errorf("LaunchRetryAfterSecs = %d, want 1 to %d", snap.LaunchRetryAfterSecs, max)
	}

	// Past the window the daemon is still refused as far as it knows, but
	// nothing holds the next launch back.
	expireLaunchCooldown(m)
	snap, _, _ = m.Health(ctx)
	if snap.ConsecutiveLaunchFailures != 1 || snap.LaunchRetryAfterSecs != 0 {
		t.Errorf("Health past the window = %+v, want the streak kept and no wait", snap)
	}
}

// The metrics view carries the same state: the streak, and the wait as null
// when no window is open.
func TestMetricsReportLaunchCooldown(t *testing.T) {
	m, _ := newRefusedMinter()
	snap := m.MetricsSnapshot()
	if got := snap["consecutive_launch_failures"]; got != 0 {
		t.Errorf("consecutive_launch_failures = %v before any launch, want 0", got)
	}
	if got, ok := snap["launch_retry_after_secs"]; !ok || got != nil {
		t.Errorf("launch_retry_after_secs = %v (present=%v) before any launch, want present and null", got, ok)
	}

	if err := m.Warm(context.Background()); err == nil {
		t.Fatal("Warm succeeded on a failing launcher")
	}
	snap = m.MetricsSnapshot()
	if got := snap["consecutive_launch_failures"]; got != 1 {
		t.Errorf("consecutive_launch_failures = %v, want 1", got)
	}
	secs, ok := snap["launch_retry_after_secs"].(int)
	if !ok || secs < 1 || secs > int(launchRetryBase/time.Second) {
		t.Errorf("launch_retry_after_secs = %v, want 1 to %d", snap["launch_retry_after_secs"], int(launchRetryBase/time.Second))
	}
}

// errBrokenBrowser is a launch that failed in the browser itself, as the
// browser package marks it.
var errBrokenBrowser = fmt.Errorf("waxseal: new page: target closed: %w", browser.ErrLocalFault)

// A probe reports what it observes, and a launch that failed is history, not an
// observation: one failure in the browser must not turn an idle daemon
// unhealthy, where nothing would launch again to clear the verdict. It reads
// as any failed launch does, benign, with the cause in the error.
func TestHealthKeepsABrowserFaultBenign(t *testing.T) {
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) { return nil, errBrokenBrowser }
	if err := m.Warm(context.Background()); !errors.Is(err, browser.ErrLocalFault) {
		t.Fatalf("Warm = %v, want the browser fault", err)
	}

	snap, live, err := m.Health(context.Background())
	if live || !errors.Is(err, ErrNoSession) {
		t.Fatalf("Health = (live=%v, %v), want the benign ErrNoSession", live, err)
	}
	if !strings.Contains(err.Error(), "new page: target closed") {
		t.Errorf("Health err = %v, want it to carry the fault", err)
	}
	if snap.ConsecutiveLaunchFailures != 1 || snap.LaunchRetryAfterSecs < 1 {
		t.Errorf("snapshot = %+v, want the streak and the wait", snap)
	}
}

// The self-test's mint failure is its own error, so startup can tell a daemon
// that attested and cannot mint from one YouTube refused.
func TestSelfTestMintFailureIsNamed(t *testing.T) {
	defer func(d time.Duration) { selfTestMintRetryDelay = d }(selfTestMintRetryDelay)
	selfTestMintRetryDelay = time.Millisecond
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{mint: func(string) (browser.MintResult, error) {
			return browser.MintResult{}, errors.New("minter gone")
		}}, nil
	}
	err := m.SelfTest(context.Background())
	if !errors.Is(err, ErrSelfTestMint) {
		t.Fatalf("SelfTest = %v, want ErrSelfTestMint", err)
	}
	if want := "minter: self-test mint failed after 3 attempts: minter gone"; err.Error() != want {
		t.Errorf("SelfTest error = %q, want %q", err, want)
	}
}

// A caller gets the session its launch published, as it stands. Reading it
// back through the recycle check would retire a session the moment it was
// born whenever a bound had already passed, and launch again without end.
func TestEnsureReturnsTheSessionItsLaunchPublished(t *testing.T) {
	var attempts atomic.Int64
	m := newBareMinter(0, 0)
	m.maxAge = time.Nanosecond // every session is past its bound once published
	m.launch = func(context.Context) (minterSession, error) {
		if attempts.Add(1) > 5 {
			return nil, errors.New("launched again and again")
		}
		return &fakeSession{mint: okMint}, nil
	}
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("launch attempts = %d, want 1", got)
	}
	// The next caller is the one that recycles it, once.
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("second Warm: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("launch attempts = %d after the recycle, want 2", got)
	}
}
