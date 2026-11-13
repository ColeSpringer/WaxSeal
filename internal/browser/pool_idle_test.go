package browser

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests cover the idle close: Suspend, the checkout that resumes from it,
// and what Health says meanwhile. Like pool_test.go they run without Chromium.

// idlePool builds a pool over a browser whose teardowns are counted. Its probe
// answers a running instance and fails a torn-down one the way a closed
// connection does.
func idlePool() (p *Pool, launches, teardowns *int64) {
	teardowns = new(int64)
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(teardowns, 1) }}
	p, launches = healthPool(inst, func(_ context.Context, probed *browserInstance) error {
		if probed.tornDown.Load() {
			return connClosed()
		}
		return nil
	})
	return p, launches, teardowns
}

// An idle close tears the browser down and counts it, and from then on a probe
// reports the suspension without probing or launching anything.
func TestPoolSuspendClosesTheBrowser(t *testing.T) {
	p, launches, teardowns := idlePool()
	if !p.Suspend() {
		t.Fatal("Suspend = false on a running browser nothing uses")
	}
	if *teardowns != 1 || p.IdleCloses() != 1 {
		t.Errorf("teardowns=%d IdleCloses=%d, want 1/1", *teardowns, p.IdleCloses())
	}
	p.ping = func(context.Context, *browserInstance) error {
		t.Error("Health probed a suspended browser")
		return nil
	}
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoverySuspended {
		t.Fatalf("Health = (%v, %v), want (RecoverySuspended, nil)", rec, err)
	}
	if *launches != 0 {
		t.Errorf("launches = %d, want 0: a probe must not wake an idle browser", *launches)
	}
	if p.Suspend() {
		t.Error("a second Suspend reported another close")
	}
	wantCounts(t, p, 0, 0)
}

// A browser with a session live or being created is in use, however long ago
// a request touched it.
func TestPoolSuspendLeavesABrowserInUse(t *testing.T) {
	p, _, teardowns := idlePool()
	if _, err := p.checkout(context.Background()); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if p.Suspend() {
		t.Fatal("Suspend closed a browser a session holds")
	}
	if *teardowns != 0 {
		t.Errorf("teardowns = %d, want 0", *teardowns)
	}
	p.release()
	if !p.Suspend() {
		t.Error("Suspend = false once the session was released")
	}
}

// The first session after an idle close launches a browser, and the pool is
// an ordinary running one again.
func TestPoolCheckoutResumesASuspendedBrowser(t *testing.T) {
	p, launches, _ := idlePool()
	stale := p.cur
	p.Suspend()

	inst, err := p.checkout(context.Background())
	if err != nil {
		t.Fatalf("checkout after Suspend: %v", err)
	}
	if *launches != 1 || inst == stale || inst != p.cur {
		t.Errorf("launches=%d fresh=%v current=%v, want 1/true/true", *launches, inst != stale, inst == p.cur)
	}
	if rec, err := p.Health(context.Background()); err != nil || rec != RecoveryNone {
		t.Errorf("Health after the resume = (%v, %v), want (RecoveryNone, nil)", rec, err)
	}
}

// Sessions created together after an idle close share one launch.
func TestPoolConcurrentCheckoutsShareOneResume(t *testing.T) {
	p, launches, _ := idlePool()
	p.Suspend()

	const n = 8
	insts := make([]*browserInstance, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { insts[i], errs[i] = p.checkout(context.Background()) })
	}
	wg.Wait()
	if *launches != 1 {
		t.Errorf("launches = %d, want 1", *launches)
	}
	for i := range n {
		if errs[i] != nil || insts[i] != p.cur {
			t.Errorf("checkout[%d] = (current=%v, %v), want the one resumed browser", i, insts[i] == p.cur, errs[i])
		}
	}
	if p.Suspend() {
		t.Error("Suspend closed a browser eight sessions hold")
	}
}

// Idle closes and resumes are not crashes, so cycling through them never
// trips the crash-loop backoff.
func TestPoolIdleCyclesDoNotBackOff(t *testing.T) {
	p, launches, _ := idlePool()
	for i := range 5 {
		if !p.Suspend() {
			t.Fatalf("cycle %d: Suspend = false", i)
		}
		if _, err := p.checkout(context.Background()); err != nil {
			t.Fatalf("cycle %d: checkout: %v", i, err)
		}
		p.release()
	}
	if *launches != 5 {
		t.Errorf("launches = %d, want 5", *launches)
	}
}

// A resume that cannot launch leaves a pool that wanted a browser and has
// none. That is a loss, not idleness: the session gets the launch error, the
// next probe tries again instead of reporting a healthy suspension, and
// Suspend cannot hide it.
func TestPoolFailedResumeIsALoss(t *testing.T) {
	p, _, _ := idlePool()
	p.Suspend()
	launchErr := errors.New("chromium: exec: no such file")
	p.newInstance = func() (*browserInstance, error) { return nil, launchErr }

	if _, err := p.checkout(context.Background()); !errors.Is(err, launchErr) {
		t.Fatalf("checkout = %v, want the launch error", err)
	}
	if p.users != 0 {
		t.Errorf("users = %d after a failed checkout, want 0", p.users)
	}
	rec, err := p.Health(context.Background())
	if err == nil || rec == RecoverySuspended {
		t.Errorf("Health = (%v, %v) after a failed resume, want an error", rec, err)
	}
	if p.Suspend() {
		t.Error("Suspend marked a pool with no browser idle again")
	}
	wantCounts(t, p, 0, 1)
}

// An idle close does not start while a probe is in flight. The probe's verdict
// then describes a browser nothing else closed under it, so an idle close is
// never booked as a probe loss, and a probe never reports a relaunch that was
// really a wake.
func TestPoolSuspendWaitsOutAProbeInFlight(t *testing.T) {
	var p *Pool
	var teardowns int64
	var refused atomic.Bool
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&teardowns, 1) }}
	p, launches := healthPool(inst, func(context.Context, *browserInstance) error {
		refused.Store(!p.Suspend()) // the idle pass, landing mid-probe
		return nil
	})
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoveryNone {
		t.Fatalf("Health = (%v, %v), want (RecoveryNone, nil)", rec, err)
	}
	if !refused.Load() || teardowns != 0 || *launches != 0 {
		t.Errorf("mid-probe Suspend refused=%v teardowns=%d launches=%d, want true/0/0", refused.Load(), teardowns, *launches)
	}
	wantCounts(t, p, 0, 0)

	p.ping = func(context.Context, *browserInstance) error { return nil }
	if !p.Suspend() {
		t.Error("Suspend = false once the probe had returned")
	}
}

// A browser that did not answer Suspend's probe is not probed again pass after
// pass: that is the health check's to settle, and each try on a wedged browser
// is five seconds. A probe or a session that finds it answering clears that.
func TestPoolSuspendProbesAnUnansweringBrowserOnce(t *testing.T) {
	var pings atomic.Int64
	var answers atomic.Bool
	p, _ := healthPool(&browserInstance{}, func(context.Context, *browserInstance) error {
		pings.Add(1)
		if answers.Load() {
			return nil
		}
		return errors.New("cdp: Browser.getVersion: context deadline exceeded")
	})
	for range 3 {
		if p.Suspend() {
			t.Fatal("Suspend closed a browser that did not answer")
		}
	}
	if got := pings.Load(); got != 1 {
		t.Fatalf("probes over three passes = %d, want 1", got)
	}

	answers.Store(true)
	if rec, err := p.Health(context.Background()); err != nil || rec != RecoveryNone {
		t.Fatalf("Health = (%v, %v), want the browser found answering", rec, err)
	}
	if !p.Suspend() {
		t.Error("Suspend = false after a probe found the browser answering again")
	}
}

// A session that uses the browser clears it too.
func TestPoolCheckoutClearsAnUnansweringBrowser(t *testing.T) {
	var answers atomic.Bool
	p, _ := healthPool(&browserInstance{}, func(context.Context, *browserInstance) error {
		if answers.Load() {
			return nil
		}
		return errors.New("cdp: Browser.getVersion: context deadline exceeded")
	})
	p.Suspend()
	answers.Store(true)
	if _, err := p.checkout(context.Background()); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	p.release()
	if !p.Suspend() {
		t.Error("Suspend = false after a session had used the browser")
	}
}

// The idle close's cache release is part of the close: a session that wakes
// the pool meanwhile launches only once it has finished, so the release cannot
// evict what the new browser has just read.
func TestPoolWakeWaitsForTheIdleCloseToFinish(t *testing.T) {
	releasing, finish := make(chan struct{}), make(chan struct{})
	old := releaseIdleCache
	releaseIdleCache = func(string, int) bool {
		close(releasing)
		<-finish
		return false
	}
	t.Cleanup(func() { releaseIdleCache = old })

	p, launches, _ := idlePool()
	closed := make(chan bool, 1)
	go func() { closed <- p.Suspend() }()
	<-releasing

	woke := make(chan error, 1)
	go func() {
		_, err := p.checkout(context.Background())
		woke <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(launches); got != 0 {
		t.Fatalf("launches = %d while the idle close was still releasing the cache, want 0", got)
	}
	close(finish)
	if err := <-woke; err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if !<-closed || atomic.LoadInt64(launches) != 1 {
		t.Errorf("launches = %d after the close finished, want 1", atomic.LoadInt64(launches))
	}
}

// A browser that died on its own while nobody was using it is a loss, not an
// idle close: Suspend leaves it, uncounted and with the crash streak standing,
// for the probe to find, relaunch, and report.
func TestPoolSuspendLeavesADeadBrowserToTheProbe(t *testing.T) {
	var teardowns int64
	crashed := &browserInstance{onTeardown: func() { atomic.AddInt64(&teardowns, 1) }}
	p, launches := healthPool(crashed, func(_ context.Context, probed *browserInstance) error {
		if probed == crashed {
			return connClosed()
		}
		return nil
	})
	p.relaunchStreak = 2
	p.lastRelaunchAt = time.Now().Add(-relaunchBackoffMax) // past its backoff, inside the stable window

	if p.Suspend() {
		t.Fatal("Suspend counted a browser that had crashed as an idle close")
	}
	if p.IdleCloses() != 0 || teardowns != 0 {
		t.Errorf("IdleCloses=%d teardowns=%d, want 0/0", p.IdleCloses(), teardowns)
	}
	if p.relaunchStreak != 2 {
		t.Errorf("relaunchStreak = %d, want the crash streak left at 2", p.relaunchStreak)
	}
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoveryRelaunched || *launches != 1 {
		t.Errorf("Health = (%v, %v) with %d launches, want the crash found and relaunched", rec, err, *launches)
	}
}

// While a session's wake is launching, the pool is still suspended to a probe:
// nothing was lost, so the probe reports the idle close and launches nothing
// of its own.
func TestPoolHealthDuringAWakeReportsSuspended(t *testing.T) {
	p, launches, _ := idlePool()
	p.Suspend()
	launching := make(chan struct{})
	release := make(chan struct{})
	p.newInstance = func() (*browserInstance, error) {
		atomic.AddInt64(launches, 1)
		close(launching)
		<-release
		return &browserInstance{}, nil
	}

	woke := make(chan error, 1)
	go func() {
		_, err := p.checkout(context.Background())
		woke <- err
	}()
	<-launching
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoverySuspended {
		t.Errorf("Health during the wake = (%v, %v), want (RecoverySuspended, nil)", rec, err)
	}
	close(release)
	if err := <-woke; err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if *launches != 1 {
		t.Errorf("launches = %d, want the wake's one", *launches)
	}
	if rec, err := p.Health(context.Background()); err != nil || rec != RecoveryNone {
		t.Errorf("Health after the wake = (%v, %v), want (RecoveryNone, nil)", rec, err)
	}
}

// Suspend has nothing to close on a nil or closed pool, and leaves a relaunch
// in flight alone.
func TestPoolSuspendRefusals(t *testing.T) {
	var nilPool *Pool
	if nilPool.Suspend() || nilPool.IdleCloses() != 0 {
		t.Error("a nil pool reported an idle close")
	}

	closed, _, _ := idlePool()
	closed.Close()
	if closed.Suspend() {
		t.Error("Suspend = true on a closed pool")
	}
	if _, err := closed.checkout(context.Background()); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("checkout on a closed pool = %v, want ErrPoolClosed", err)
	}

	relaunching, _, teardowns := idlePool()
	relaunching.relaunching = make(chan struct{})
	if relaunching.Suspend() || *teardowns != 0 {
		t.Error("Suspend acted during a relaunch")
	}
}

// Close after an idle close has no browser left to tear down and still ends
// the pool.
func TestPoolCloseWhileSuspended(t *testing.T) {
	p, launches, teardowns := idlePool()
	p.Suspend()
	p.Close()
	if *teardowns != 1 {
		t.Errorf("teardowns = %d, want 1 (the idle close's)", *teardowns)
	}
	if _, err := p.checkout(context.Background()); !errors.Is(err, ErrPoolClosed) || *launches != 0 {
		t.Errorf("checkout after Close = %v with %d launches, want ErrPoolClosed and none", err, *launches)
	}
	if rec, err := p.Health(context.Background()); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Health after Close = (%v, %v), want ErrPoolClosed", rec, err)
	}
}
