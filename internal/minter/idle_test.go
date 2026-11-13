package minter

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
)

// idleTenants builds a browserless registry that has been up for a day, with
// an hour's idle timeout and a prober that counts the browser closes it is
// asked for. The loop that would call closeIdle is not started, so a test
// drives each pass itself.
func idleTenants(t *testing.T, keys map[string]string) (*Tenants, *fakeProber) {
	t.Helper()
	tn, _ := newTestTenants(keys)
	tn.idleTimeout = time.Hour
	tn.created = tn.created.Add(-24 * time.Hour)
	p := &fakeProber{}
	tn.SetBrowserProberForTest(p)
	return tn, p
}

// liveTenant reports whether the tenant behind key has a session.
func liveTenant(t *testing.T, tn *Tenants, key string) bool {
	t.Helper()
	m, _, err := tn.Minter(key)
	if err != nil {
		t.Fatalf("Minter(%q): %v", key, err)
	}
	return m.MetricsSnapshot()["session_live"] == true
}

// rewindTenant makes the tenant behind key look unused for d longer.
func rewindTenant(t *testing.T, tn *Tenants, key string, d time.Duration) {
	t.Helper()
	m, _, err := tn.Minter(key)
	if err != nil {
		t.Fatalf("Minter(%q): %v", key, err)
	}
	rewindLastUse(m, d)
}

// Each tenant idles on its own clock, and the browser outlives them only while
// one is in use: it closes once the busiest tenant has been idle for the
// timeout.
func TestTenantsCloseIdleClosesIdleTenantsThenTheBrowser(t *testing.T) {
	tn, prober := idleTenants(t, map[string]string{"KEYA": "alice", "KEYB": "bob"})
	ctx := context.Background()
	for _, k := range []string{"KEYA", "KEYB"} {
		if err := tn.WarmOne(ctx, k); err != nil {
			t.Fatalf("WarmOne(%s): %v", k, err)
		}
	}

	rewindTenant(t, tn, "KEYA", 2*time.Hour)
	tn.closeIdle()
	if liveTenant(t, tn, "KEYA") || !liveTenant(t, tn, "KEYB") {
		t.Fatalf("alice live=%v bob live=%v, want false/true", liveTenant(t, tn, "KEYA"), liveTenant(t, tn, "KEYB"))
	}
	if got := prober.suspends.Load(); got != 0 {
		t.Fatalf("browser closes = %d while bob is in use, want 0", got)
	}

	rewindTenant(t, tn, "KEYB", 2*time.Hour)
	tn.closeIdle()
	if liveTenant(t, tn, "KEYB") {
		t.Error("bob is still live after his own timeout")
	}
	if got := prober.suspends.Load(); got != 1 {
		t.Errorf("browser closes = %d once every tenant is idle, want 1", got)
	}
}

// A browser nobody has used yet is measured from when the daemon started, so
// a keyed daemon waiting for its first tenant keeps it for the whole timeout.
func TestTenantsCloseIdleMeasuresAnUnusedBrowserFromStartup(t *testing.T) {
	tn, prober := idleTenants(t, map[string]string{"KEYA": "alice"})
	tn.created = time.Now()
	tn.closeIdle()
	if got := prober.suspends.Load(); got != 0 {
		t.Fatalf("browser closes = %d right after startup, want 0", got)
	}
	tn.created = tn.created.Add(-2 * time.Hour)
	tn.closeIdle()
	if got := prober.suspends.Load(); got != 1 {
		t.Errorf("browser closes = %d two hours after startup with no tenant, want 1", got)
	}
}

// A session retired for another reason leaves no session behind, but its
// tenant was just in use and is about to ask again: that is not idleness.
func TestTenantsCloseIdleKeepsTheBrowserForATenantInUse(t *testing.T) {
	tn, prober := idleTenants(t, nil)
	ctx := context.Background()
	if err := tn.WarmOne(ctx, ""); err != nil {
		t.Fatalf("WarmOne: %v", err)
	}
	m, _, _ := tn.Minter("")
	if res := m.ReportDegraded(m.Generation(), "vid", "truncated"); !res.Retired {
		t.Fatalf("ReportDegraded = %+v, want the session retired", res)
	}
	tn.closeIdle()
	if got := prober.suspends.Load(); got != 0 {
		t.Errorf("browser closes = %d moments after a request, want 0", got)
	}
}

// CloseIdleAfter runs the passes on its own until Close, which waits for the
// one in flight so it cannot outlive the pool.
func TestTenantsCloseIdleAfterRunsUntilClose(t *testing.T) {
	tn, _ := newTestTenants(nil)
	prober := &fakeProber{}
	tn.SetBrowserProberForTest(prober)
	tn.CloseIdleAfter(40 * time.Millisecond)
	if err := tn.WarmOne(context.Background(), ""); err != nil {
		t.Fatalf("WarmOne: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for liveTenant(t, tn, "") || prober.suspends.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after 5s: session live=%v, browser closes=%d; want the session and then the browser closed", liveTenant(t, tn, ""), prober.suspends.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	tn.Close()
	after := prober.suspends.Load()
	time.Sleep(120 * time.Millisecond)
	if got := prober.suspends.Load(); got != after {
		t.Errorf("browser closes went %d to %d after Close: the idle loop is still running", after, got)
	}
}

// Without CloseIdleAfter nothing is ever closed for idleness.
func TestTenantsIdleCloseIsOffByDefault(t *testing.T) {
	tn, _ := newTestTenants(nil)
	prober := &fakeProber{}
	tn.SetBrowserProberForTest(prober)
	if err := tn.WarmOne(context.Background(), ""); err != nil {
		t.Fatalf("WarmOne: %v", err)
	}
	rewindTenant(t, tn, "", 1000*time.Hour)
	tn.created = tn.created.Add(-1000 * time.Hour)
	tn.closeIdle()
	if !liveTenant(t, tn, "") || prober.suspends.Load() != 0 {
		t.Errorf("session live=%v browser closes=%d with no idle timeout set, want true/0", liveTenant(t, tn, ""), prober.suspends.Load())
	}
	tn.Close()
}

// rewindLastUse makes m look unused for d longer than it is.
func rewindLastUse(m *Minter, d time.Duration) {
	m.mu.Lock()
	m.lastUsedAt = m.lastUsedAt.Add(-d)
	m.mu.Unlock()
}

// A session no request has used for the timeout is closed, counted as an idle
// close and not as a crash, and its cached tokens stay servable: a PO token
// outlives the page that minted it.
func TestMinterCloseIdleClosesAnUnusedSession(t *testing.T) {
	m, launches, sessions, _ := newTestMinter(okMint)
	ctx := context.Background()
	if _, _, err := m.Mint(ctx, "player", "vid"); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	rewindLastUse(m, 2*time.Hour)

	if !m.closeIdle(time.Hour) {
		t.Fatal("closeIdle = false for a session unused for two hours")
	}
	if !(*sessions)[0].closed.Load() {
		t.Error("the idle session was not closed")
	}
	snap := m.MetricsSnapshot()
	if snap["session_live"] != false || snap["idle_closes"] != int64(1) || snap["crashes"] != int64(0) {
		t.Errorf("session_live=%v idle_closes=%v crashes=%v, want false/1/0", snap["session_live"], snap["idle_closes"], snap["crashes"])
	}
	if _, cached, err := m.Mint(ctx, "player", "vid"); err != nil || !cached {
		t.Errorf("Mint for a cached binding after the idle close = (cached=%v, %v), want a cache hit", cached, err)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1: a cache hit must not wake the session", got)
	}
	if m.closeIdle(time.Hour) {
		t.Error("closeIdle reported a second close with no session live")
	}
}

// A session inside the timeout is left alone, and a fresh one starts with the
// whole timeout ahead of it.
func TestMinterCloseIdleKeepsARecentlyUsedSession(t *testing.T) {
	m, _, sessions, _ := newTestMinter(okMint)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if m.closeIdle(time.Hour) {
		t.Fatal("closeIdle closed a session that was just launched")
	}
	if (*sessions)[0].closed.Load() || m.MetricsSnapshot()["session_live"] != true {
		t.Error("the session is gone")
	}
}

// A request holding the page is using the session, however long it has run.
func TestMinterCloseIdleSkipsABusyPage(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mints atomic.Int64
	m, _, sessions, _ := newTestMinter(func(id string) (browser.MintResult, error) {
		if mints.Add(1) == 2 { // the first is the attestation pre-mint
			close(entered)
			<-release
		}
		return okMint(id)
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Mint(context.Background(), "player", "vid")
		done <- err
	}()
	<-entered
	rewindLastUse(m, 2*time.Hour)
	if m.closeIdle(time.Hour) {
		t.Error("closeIdle closed a session a request is minting on")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if (*sessions)[0].closed.Load() {
		t.Error("the session was closed under the request")
	}
	// The request's end is a use, so the timeout starts over from there.
	if m.closeIdle(time.Hour) {
		t.Error("closeIdle closed the session right after the request finished")
	}
}

// The request after an idle close launches a fresh session and proves it, as
// any relaunch does.
func TestMinterRequestAfterIdleCloseRelaunches(t *testing.T) {
	m, launches, sessions, _ := newTestMinter(okMint)
	ctx := context.Background()
	if _, _, err := m.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("PlayerContext: %v", err)
	}
	rewindLastUse(m, 2*time.Hour)
	if !m.closeIdle(time.Hour) {
		t.Fatal("closeIdle = false")
	}

	_, gen, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("PlayerContext after the idle close: %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 2 || gen != 2 {
		t.Errorf("launches=%d generation=%d, want 2/2", got, gen)
	}
	if got := (*sessions)[1].proofCount(); got != 1 {
		t.Errorf("proofs on the fresh session = %d, want 1", got)
	}
	if m.closeIdle(time.Hour) {
		t.Error("closeIdle closed the session the request just used")
	}
}

// Every request that takes the page is a use. A cached token, a probe, and a
// report are not: none of them needs the browser.
func TestMinterUseIsWhatTakesThePage(t *testing.T) {
	m, _, _, _ := newTestMinter(okMint)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	uses := []struct {
		name string
		do   func() error
	}{
		{"mint", func() error { _, _, err := m.Mint(ctx, "player", "vid"); return err }},
		{"player-context", func() error { _, _, err := m.PlayerContext(ctx, "vid"); return err }},
		{"session", func() error { _, _, _, err := m.SessionSnapshot(ctx); return err }},
		{"self-test", func() error { return m.SelfTest(ctx) }},
	}
	for _, u := range uses {
		rewindLastUse(m, time.Hour)
		if err := u.do(); err != nil {
			t.Fatalf("%s: %v", u.name, err)
		}
		if idle := time.Since(m.lastUsed()); idle > time.Minute {
			t.Errorf("%s left the session looking idle for %s", u.name, idle.Round(time.Second))
		}
	}

	rewindLastUse(m, time.Hour)
	if _, cached, err := m.Mint(ctx, "player", "vid"); err != nil || !cached {
		t.Fatalf("Mint = (cached=%v, %v), want a cache hit", cached, err)
	}
	if _, live, err := m.Health(ctx); err != nil || !live {
		t.Fatalf("Health = (live=%v, %v), want a live session", live, err)
	}
	m.ReportDegraded(99, "vid", "stale") // a stale report: nothing retires
	if idle := time.Since(m.lastUsed()); idle < 50*time.Minute {
		t.Errorf("a cache hit, a probe, or a report counted as use: idle for %s, want about an hour", idle.Round(time.Second))
	}
}

// blockedMint returns a minter whose first request mint blocks until release
// is closed, and a channel that closes when that mint has the page.
func blockedMint(t *testing.T) (m *Minter, entered, release chan struct{}) {
	t.Helper()
	entered, release = make(chan struct{}), make(chan struct{})
	var mints atomic.Int64
	m, _, _, _ = newTestMinter(func(id string) (browser.MintResult, error) {
		if mints.Add(1) == 2 { // the first is the attestation pre-mint
			close(entered)
			<-release
		}
		return okMint(id)
	})
	return m, entered, release
}

// A request holding the page is use for as long as it runs, not only at its
// two ends: one that outlasts the timeout must not read as an idle tenant, or
// the browser could be closed in the gap where it swaps sessions.
func TestMinterRequestInFlightIsUseHoweverLongItRuns(t *testing.T) {
	m, entered, release := blockedMint(t)
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Mint(context.Background(), "player", "vid")
		done <- err
	}()
	<-entered
	rewindLastUse(m, 2*time.Hour) // the request has been running for two hours
	if idle := time.Since(m.lastUsed()); idle > time.Minute {
		t.Errorf("a tenant with a request in flight reads as idle for %s", idle.Round(time.Second))
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if idle := time.Since(m.lastUsed()); idle > time.Minute {
		t.Errorf("the tenant reads as idle for %s right after its request", idle.Round(time.Second))
	}
}

// The same at the registry: the browser is not closed while any tenant has a
// request in flight, whatever the clocks say.
func TestTenantsCloseIdleKeepsTheBrowserUnderALongRequest(t *testing.T) {
	tn, prober := idleTenants(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var mints atomic.Int64
	tn.newSession = func(context.Context, string) (minterSession, error) {
		return &fakeSession{mint: func(id string) (browser.MintResult, error) {
			if mints.Add(1) == 2 {
				close(entered)
				<-release
			}
			return okMint(id)
		}}, nil
	}
	m, _, _ := tn.Minter("")
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Mint(context.Background(), "player", "vid")
		done <- err
	}()
	<-entered
	rewindLastUse(m, 2*time.Hour)
	tn.closeIdle()
	if got := prober.suspends.Load(); got != 0 {
		t.Errorf("browser closes = %d under a request in flight, want 0", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Mint: %v", err)
	}
}

// A consumer's report still pending when the session goes idle is what closes
// it: the close is booked as the report's recycle, against the report budget,
// and not as an idle close.
func TestMinterCloseIdleBooksAPendingReport(t *testing.T) {
	m, entered, release := blockedMint(t)
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Mint(context.Background(), "player", "vid")
		done <- err
	}()
	<-entered
	res := m.ReportDegraded(m.Generation(), "vid", "truncated")
	if !res.Accepted || !res.RetirementPending {
		t.Fatalf("ReportDegraded during a request = %+v, want it accepted and pending", res)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Mint: %v", err)
	}
	m.mu.Lock()
	budget := m.reportTokens
	m.mu.Unlock()

	rewindLastUse(m, 2*time.Hour)
	if m.closeIdle(time.Hour) {
		t.Error("closeIdle reported an idle close for a session a report retired")
	}
	snap := m.MetricsSnapshot()
	if snap["session_live"] != false || snap["report_driven_recycles"] != int64(1) || snap["idle_closes"] != int64(0) {
		t.Errorf("session_live=%v report_driven_recycles=%v idle_closes=%v, want false/1/0",
			snap["session_live"], snap["report_driven_recycles"], snap["idle_closes"])
	}
	m.mu.Lock()
	spent := budget - m.reportTokens
	m.mu.Unlock()
	if spent < 0.9 {
		t.Errorf("report budget moved by %.2f, want one recycle spent", spent)
	}
}

// A request queued for the page is use as much as the one holding it: the
// idle pass must neither close the session it is about to take nor let the
// browser be closed under it.
func TestMinterRequestWaitingForThePageIsUse(t *testing.T) {
	m, _, sessions, _ := newTestMinter(okMint)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	rewindLastUse(m, 2*time.Hour)

	m.mintMu.Lock() // something else has the page, as the idle pass does while it decides
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Mint(context.Background(), "player", "vid")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Since(m.lastUsed()) > time.Minute {
		if time.Now().After(deadline) {
			m.mintMu.Unlock()
			t.Fatal("a tenant with a request waiting for the page still reads as idle for hours")
		}
		time.Sleep(time.Millisecond)
	}
	if m.closeIdleLocked(time.Hour) {
		t.Error("the idle close took a session a queued request was about to use")
	}
	m.mintMu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if (*sessions)[0].closed.Load() {
		t.Error("the session was closed under the queued request")
	}
}

// Use is taking the browser. A launch that runs is use even when it fails, but
// the refusals of the cool-down that follows never touch the browser, so a
// retry storm against a failing tenant does not keep it awake.
func TestMinterCooldownRefusalsAreNotUse(t *testing.T) {
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		return nil, errors.New("waxseal: ytcfg visitor_data not available before deadline")
	}
	ctx := context.Background()
	if _, _, err := m.Mint(ctx, "player", "vid"); err == nil {
		t.Fatal("Mint succeeded with a launcher that always fails")
	}
	launched := m.lastUsed()
	if launched.IsZero() {
		t.Fatal("a launch that ran was not counted as use")
	}

	time.Sleep(5 * time.Millisecond)
	for i := range 3 {
		if _, _, err := m.Mint(ctx, "player", "vid"); !errors.Is(err, ErrLaunchCooldown) {
			t.Fatalf("Mint %d = %v, want a cool-down refusal", i, err)
		}
	}
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrLaunchCooldown) {
		t.Fatalf("PlayerContext = %v, want a cool-down refusal", err)
	}
	if used := m.lastUsed(); !used.Equal(launched) {
		t.Errorf("cool-down refusals moved the last use by %s, want it left at the launch", used.Sub(launched))
	}
}

// StopIdleClose ends the idle passes without closing anything, for a daemon
// that is shutting down: a pass must not close a browser a draining request
// would have to launch again. A pass that runs anyway does nothing.
func TestTenantsStopIdleCloseEndsThePasses(t *testing.T) {
	tn, _ := newTestTenants(nil)
	prober := &fakeProber{}
	tn.SetBrowserProberForTest(prober)
	tn.created = tn.created.Add(-time.Hour)
	tn.CloseIdleAfter(20 * time.Millisecond)
	if err := tn.WarmOne(context.Background(), ""); err != nil {
		t.Fatalf("WarmOne: %v", err)
	}
	tn.StopIdleClose()
	tn.StopIdleClose() // a second stop is harmless

	rewindTenant(t, tn, "", time.Hour)
	time.Sleep(100 * time.Millisecond) // five passes' worth, had the loop kept going
	tn.closeIdle()
	if !liveTenant(t, tn, "") || prober.suspends.Load() != 0 {
		t.Errorf("session live=%v browser closes=%d after StopIdleClose, want true/0", liveTenant(t, tn, ""), prober.suspends.Load())
	}
	tn.Close()
}

// The daemon-wide idle-close counter is read through the registry like the
// other browser counters.
func TestTenantsBrowserIdleCloses(t *testing.T) {
	tn, _ := newTestTenants(nil)
	tn.SetBrowserProberForTest(&fakeProber{idle: 7})
	if got := tn.BrowserIdleCloses(); got != 7 {
		t.Errorf("BrowserIdleCloses = %d, want 7", got)
	}
}
