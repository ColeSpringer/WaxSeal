package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/cdp"
)

// These tests replace newInstance and use partially initialized browserInstance
// values to exercise pool recovery without launching Chromium.

// Concurrent callers that observed the same stale instance share one relaunch.
func TestPoolRelaunchSingleFlight(t *testing.T) {
	stale := &browserInstance{}
	var created int64
	var once sync.Once
	started := make(chan struct{})
	release := make(chan struct{})
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) {
		atomic.AddInt64(&created, 1)
		once.Do(func() { close(started) })
		<-release // Keep the relaunch in progress until all callers are waiting.
		return &browserInstance{}, nil
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]*browserInstance, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = p.relaunch(stale) }(i)
	}
	<-started
	time.Sleep(25 * time.Millisecond) // Allow the remaining callers to begin waiting.
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&created); got != 1 {
		t.Errorf("newInstance called %d times, want 1", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("relaunch[%d] err = %v", i, errs[i])
		}
		if results[i] != p.cur {
			t.Errorf("relaunch[%d] did not return the current instance", i)
		}
	}
}

// A caller with an obsolete snapshot receives the current instance.
func TestPoolRelaunchShortCircuitsWhenAlreadySwapped(t *testing.T) {
	stale := &browserInstance{}
	fresh := &browserInstance{}
	var created int64
	p := &Pool{opts: withDefaults(Options{}), cur: fresh}
	p.newInstance = func() (*browserInstance, error) { atomic.AddInt64(&created, 1); return &browserInstance{}, nil }

	got, err := p.relaunch(stale)
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got != fresh {
		t.Error("relaunch should return the already-current instance")
	}
	if atomic.LoadInt64(&created) != 0 {
		t.Error("newInstance was called after the stale instance was replaced")
	}
}

// A failed relaunch starts a backoff window before another launch is allowed.
func TestPoolRelaunchBackoffAfterFailure(t *testing.T) {
	stale := &browserInstance{}
	var created int64
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) {
		atomic.AddInt64(&created, 1)
		return nil, errors.New("launch failed")
	}

	if _, err := p.relaunch(stale); err == nil {
		t.Fatal("first relaunch should return the launch error")
	}
	if got := atomic.LoadInt64(&created); got != 1 {
		t.Fatalf("newInstance calls = %d, want 1", got)
	}
	if _, err := p.relaunch(stale); err == nil {
		t.Fatal("second relaunch within the backoff window should fail fast")
	}
	if got := atomic.LoadInt64(&created); got != 1 {
		t.Errorf("newInstance calls during backoff = %d, want 1", got)
	}
}

// The relaunch delay grows exponentially up to relaunchBackoffMax.
func TestPoolBackoffWindow(t *testing.T) {
	p := &Pool{}
	for _, c := range []struct {
		fails int
		want  time.Duration
	}{
		{1, relaunchBackoffBase},
		{2, 2 * relaunchBackoffBase},
		{3, 4 * relaunchBackoffBase},
		{100, relaunchBackoffMax},
	} {
		p.relaunchStreak = c.fails
		if got := p.backoffWindow(); got != c.want {
			t.Errorf("backoffWindow(streak=%d) = %v, want %v", c.fails, got, c.want)
		}
	}
}

// A browser that survives relaunchStableWindow starts a new backoff streak.
func TestPoolRelaunchResetsStreakAfterStableGap(t *testing.T) {
	stale := &browserInstance{}
	var created int64
	p := &Pool{opts: withDefaults(Options{}), cur: stale, relaunchStreak: 5, lastRelaunchAt: time.Now().Add(-time.Hour)}
	p.newInstance = func() (*browserInstance, error) { atomic.AddInt64(&created, 1); return &browserInstance{}, nil }
	if _, err := p.relaunch(stale); err != nil {
		t.Fatalf("a relaunch after a long stable gap must not back off: %v", err)
	}
	if got := atomic.LoadInt64(&created); got != 1 {
		t.Errorf("newInstance calls = %d, want 1", got)
	}
	if p.relaunchStreak != 1 {
		t.Errorf("relaunchStreak after stability window = %d, want 1", p.relaunchStreak)
	}
}

// Waiting through the maximum backoff must not reset an active crash-loop streak.
func TestPoolRelaunchHoldsCeilingDuringCrashLoop(t *testing.T) {
	stale := &browserInstance{}
	// The previous relaunch is old enough to clear the maximum backoff, but not the
	// stability window.
	p := &Pool{opts: withDefaults(Options{}), cur: stale, relaunchStreak: 5, lastRelaunchAt: time.Now().Add(-(relaunchBackoffMax + time.Second))}
	p.newInstance = func() (*browserInstance, error) { return &browserInstance{}, nil }
	if _, err := p.relaunch(stale); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if p.relaunchStreak <= 1 {
		t.Errorf("relaunchStreak after maximum backoff = %d, want greater than 1", p.relaunchStreak)
	}
}

// A browser that starts and dies during setup still contributes to the backoff.
func TestPoolRelaunchBackoffOnCrashLoop(t *testing.T) {
	stale := &browserInstance{}
	var created int64
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) {
		atomic.AddInt64(&created, 1)
		return &browserInstance{}, nil
	}
	inst1, err := p.relaunch(stale)
	if err != nil {
		t.Fatalf("first relaunch: %v", err)
	}
	if _, err := p.relaunch(inst1); err == nil {
		t.Fatal("immediate relaunch after a post-launch crash should be rejected")
	}
	if got := atomic.LoadInt64(&created); got != 1 {
		t.Errorf("newInstance calls = %d, want 1", got)
	}
}

// Relaunch must not restore a closed pool.
func TestPoolRelaunchClosedBlocks(t *testing.T) {
	stale := &browserInstance{}
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) { return &browserInstance{}, nil }
	p.Close()
	if _, err := p.relaunch(stale); !errors.Is(err, errPoolClosed) {
		t.Errorf("relaunch after Close = %v, want errPoolClosed", err)
	}
}

// Concurrent relaunch callers tear down the stale instance exactly once.
func TestPoolRelaunchDisposesStaleOnce(t *testing.T) {
	var teardowns int64
	stale := &browserInstance{onTeardown: func() { atomic.AddInt64(&teardowns, 1) }}
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) { return &browserInstance{}, nil }

	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.relaunch(stale) }()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&teardowns); got != 1 {
		t.Errorf("stale teardown count = %d, want 1", got)
	}
}

// Close discards and tears down a replacement that finishes concurrently.
func TestPoolCloseDuringRelaunch(t *testing.T) {
	var newTorn int64
	stale := &browserInstance{}
	enter := make(chan struct{})
	release := make(chan struct{})
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) {
		close(enter)
		<-release
		return &browserInstance{onTeardown: func() { atomic.AddInt64(&newTorn, 1) }}, nil
	}

	done := make(chan struct{})
	var relErr error
	go func() { _, relErr = p.relaunch(stale); close(done) }()

	<-enter
	p.Close()
	close(release)
	<-done

	if !errors.Is(relErr, errPoolClosed) {
		t.Errorf("relaunch during Close = %v, want errPoolClosed", relErr)
	}
	if got := atomic.LoadInt64(&newTorn); got != 1 {
		t.Errorf("replacement teardown count = %d, want 1", got)
	}
	if p.cur != nil {
		t.Error("cur should be nil after Close")
	}
}

// teardown runs at most once when Close races with relaunch.
func TestBrowserInstanceTeardownIdempotent(t *testing.T) {
	var n int64
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&n, 1) }}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); inst.teardown() }()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&n); got != 1 {
		t.Errorf("teardown ran %d times, want 1", got)
	}
}

// Pool.Health is the browser check behind /ping. These tests replace ping and
// newInstance to exercise its policy without Chromium.

// healthPool builds a pool over inst whose probe is ping and whose relaunches
// produce fresh instances, counting them.
func healthPool(inst *browserInstance, ping func(context.Context, *browserInstance) error) (*Pool, *int64) {
	var launches int64
	p := &Pool{opts: withDefaults(Options{}), cur: inst}
	p.ping = ping
	p.newInstance = func() (*browserInstance, error) {
		atomic.AddInt64(&launches, 1)
		return &browserInstance{}, nil
	}
	return p, &launches
}

// connClosed is the error a call on a torn-down connection returns.
func connClosed() error { return fmt.Errorf("%w: read: EOF", cdp.ErrConnClosed) }

func wantCounts(t *testing.T, p *Pool, probeFailures, relaunchFailures int64) {
	t.Helper()
	if got := p.ProbeFailures(); got != probeFailures {
		t.Errorf("ProbeFailures = %d, want %d", got, probeFailures)
	}
	if got := p.RelaunchFailures(); got != relaunchFailures {
		t.Errorf("RelaunchFailures = %d, want %d", got, relaunchFailures)
	}
}

// A browser that answers the round trip is live: one probe, nothing torn down,
// nothing launched, nothing counted.
func TestPoolHealthAlive(t *testing.T) {
	var torn, pings int64
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&torn, 1) }}
	p, launches := healthPool(inst, func(context.Context, *browserInstance) error {
		atomic.AddInt64(&pings, 1)
		return nil
	})
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoveryNone {
		t.Fatalf("Health = (%v, %v), want (RecoveryNone, nil)", rec, err)
	}
	if pings != 1 || torn != 0 || *launches != 0 {
		t.Errorf("pings=%d teardowns=%d launches=%d, want 1/0/0", pings, torn, *launches)
	}
	wantCounts(t, p, 0, 0)
}

// A connection Chromium has torn down is a known death: no confirmation, no
// teardown of our own, and a replacement is launched so the probe answers for a
// browser that is running rather than for one that used to.
func TestPoolHealthExitedBrowserIsRelaunched(t *testing.T) {
	var torn, pings int64
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&torn, 1) }}
	p, launches := healthPool(inst, func(context.Context, *browserInstance) error {
		atomic.AddInt64(&pings, 1)
		return connClosed()
	})
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoveryRelaunched {
		t.Fatalf("Health = (%v, %v), want (RecoveryRelaunched, nil)", rec, err)
	}
	if pings != 1 {
		t.Errorf("pings = %d, want 1 (a known death needs no confirmation)", pings)
	}
	// relaunch tears the stale instance down itself; Health counts no probe loss.
	if torn != 1 || *launches != 1 {
		t.Errorf("teardowns=%d launches=%d, want 1/1", torn, *launches)
	}
	if p.cur == inst {
		t.Error("the exited instance is still current after the relaunch")
	}
	wantCounts(t, p, 0, 0)
}

// A browser that exits between the first probe and its confirmation is a known
// death too, not a wedged browser: relaunched, with no probe loss counted.
func TestPoolHealthExitDuringConfirmIsRelaunched(t *testing.T) {
	var pings int64
	p, launches := healthPool(&browserInstance{}, func(context.Context, *browserInstance) error {
		if atomic.AddInt64(&pings, 1) == 1 {
			return context.DeadlineExceeded
		}
		return connClosed()
	})
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoveryRelaunched {
		t.Fatalf("Health = (%v, %v), want (RecoveryRelaunched, nil)", rec, err)
	}
	if pings != 2 || *launches != 1 {
		t.Errorf("pings=%d launches=%d, want 2/1", pings, *launches)
	}
	wantCounts(t, p, 0, 0)
}

// An exited browser another caller has already replaced needs no launch: the
// probe reports the replacement.
func TestPoolHealthExitedBrowserAlreadyReplaced(t *testing.T) {
	stale := &browserInstance{}
	fresh := &browserInstance{}
	var p *Pool
	var launches *int64
	p, launches = healthPool(stale, func(context.Context, *browserInstance) error {
		p.mu.Lock()
		p.cur = fresh // a concurrent NewSession relaunched while we probed
		p.mu.Unlock()
		return connClosed()
	})
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoveryRelaunched {
		t.Fatalf("Health = (%v, %v), want (RecoveryRelaunched, nil)", rec, err)
	}
	if *launches != 0 || p.cur != fresh {
		t.Errorf("launches=%d cur==fresh=%v, want 0/true", *launches, p.cur == fresh)
	}
}

// When the replacement cannot be launched the probe fails with that error and
// the failure is counted: this is the state a health check must surface, a
// daemon with no browser and no way to get one.
func TestPoolHealthRelaunchFailureIsReported(t *testing.T) {
	launchErr := errors.New("chromium: exec: no such file")
	p, _ := healthPool(&browserInstance{}, func(context.Context, *browserInstance) error { return connClosed() })
	p.newInstance = func() (*browserInstance, error) { return nil, launchErr }
	rec, err := p.Health(context.Background())
	if !errors.Is(err, launchErr) {
		t.Fatalf("Health = (%v, %v), want the launch error", rec, err)
	}
	wantCounts(t, p, 0, 1)
}

// A relaunch refused by the crash-loop backoff fails the probe with the backoff
// error; the next probe tries again once the window has passed.
func TestPoolHealthRelaunchBackoffIsReported(t *testing.T) {
	p, launches := healthPool(&browserInstance{}, func(context.Context, *browserInstance) error { return connClosed() })
	p.relaunchStreak = 1
	p.lastRelaunchAt = time.Now()
	_, err := p.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "backing off") {
		t.Fatalf("Health err = %v, want the backoff error", err)
	}
	if *launches != 0 {
		t.Errorf("launches = %d, want 0 during backoff", *launches)
	}
	wantCounts(t, p, 0, 0) // a refusal is not a launch failure
}

// A closed pool has no browser and never will: an error, with ping untouched.
func TestPoolHealthClosedPoolFails(t *testing.T) {
	p, _ := healthPool(&browserInstance{}, func(context.Context, *browserInstance) error {
		t.Error("ping called on a closed pool")
		return nil
	})
	p.Close()
	if _, err := p.Health(context.Background()); !errors.Is(err, errPoolClosed) {
		t.Errorf("Health after Close = %v, want errPoolClosed", err)
	}
}

// A nil pool reads as closed, like Close on a nil pool is a no-op.
func TestPoolHealthNilPoolFails(t *testing.T) {
	var p *Pool
	if _, err := p.Health(context.Background()); !errors.Is(err, errPoolClosed) {
		t.Errorf("(*Pool)(nil).Health = %v, want errPoolClosed", err)
	}
	wantCounts(t, p, 0, 0)
}

// One missed round trip is confirmed before anything happens, so a browser that
// answers the second probe is live and keeps its instance.
func TestPoolHealthTransientFailureRecovers(t *testing.T) {
	var torn, pings int64
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&torn, 1) }}
	p, launches := healthPool(inst, func(context.Context, *browserInstance) error {
		if atomic.AddInt64(&pings, 1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	})
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoveryNone {
		t.Fatalf("Health = (%v, %v), want (RecoveryNone, nil) after the confirmation answered", rec, err)
	}
	if pings != 2 || torn != 0 || *launches != 0 {
		t.Errorf("pings=%d teardowns=%d launches=%d, want 2/0/0", pings, torn, *launches)
	}
	wantCounts(t, p, 0, 0)
}

// Two missed round trips mean the browser is wedged. It is torn down, the loss
// is counted, and a replacement is launched, so the next request finds a
// browser instead of stalling on the wedged one for its whole budget.
func TestPoolHealthConfirmedFailureTearsDownAndRelaunches(t *testing.T) {
	var torn, pings int64
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&torn, 1) }}
	p, launches := healthPool(inst, func(context.Context, *browserInstance) error {
		atomic.AddInt64(&pings, 1)
		return errors.New("cdp: Browser.getVersion: context deadline exceeded")
	})
	rec, err := p.Health(context.Background())
	if err != nil || rec != RecoveryTornDown {
		t.Fatalf("Health = (%v, %v), want (RecoveryTornDown, nil)", rec, err)
	}
	if pings != 2 || torn != 1 || *launches != 1 {
		t.Errorf("pings=%d teardowns=%d launches=%d, want 2/1/1", pings, torn, *launches)
	}
	if p.cur == inst {
		t.Error("the torn-down instance is still current after the relaunch")
	}
	wantCounts(t, p, 1, 0)
}

// A wedged browser whose replacement cannot be launched reports the launch
// error; the teardown still counts, since the wedge was real.
func TestPoolHealthConfirmedFailureThenRelaunchFailure(t *testing.T) {
	launchErr := errors.New("chromium: exec: no such file")
	p, _ := healthPool(&browserInstance{}, func(context.Context, *browserInstance) error {
		return errors.New("cdp: Browser.getVersion: context deadline exceeded")
	})
	p.newInstance = func() (*browserInstance, error) { return nil, launchErr }
	if _, err := p.Health(context.Background()); !errors.Is(err, launchErr) {
		t.Fatalf("Health err = %v, want the launch error", err)
	}
	wantCounts(t, p, 1, 1)
}

// Concurrent probes that confirm the same wedged browser tear it down once,
// count one loss, and launch one replacement. Only the probe that acted reports
// the teardown; the rest report the relaunch they observed, or nothing at all
// when they started late enough to probe the replacement.
func TestPoolHealthConcurrentConfirmationsActOnce(t *testing.T) {
	var torn int64
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&torn, 1) }}
	p, launches := healthPool(inst, func(_ context.Context, probed *browserInstance) error {
		if probed == inst {
			return errors.New("cdp: Browser.getVersion: context deadline exceeded")
		}
		return nil // the replacement answers
	})

	const n = 6
	recs := make([]Recovery, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); recs[i], errs[i] = p.Health(context.Background()) }(i)
	}
	wg.Wait()
	if torn != 1 || *launches != 1 {
		t.Errorf("teardowns=%d launches=%d, want 1/1", torn, *launches)
	}
	wantCounts(t, p, 1, 0)
	tornDown := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("Health[%d] err = %v", i, errs[i])
		}
		if recs[i] == RecoveryTornDown {
			tornDown++
		}
	}
	if tornDown != 1 {
		t.Errorf("probes reporting the teardown = %d, want exactly 1", tornDown)
	}
}

// A caller that goes away mid-probe learns nothing about the browser. Health
// returns the context error and neither confirms, tears down, nor launches.
func TestPoolHealthCancelledContextDoesNotAct(t *testing.T) {
	for _, cancelOn := range []int64{1, 2} {
		t.Run(fmt.Sprintf("cancel on probe %d", cancelOn), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var torn, pings int64
			inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&torn, 1) }}
			p, launches := healthPool(inst, func(ctx context.Context, _ *browserInstance) error {
				if atomic.AddInt64(&pings, 1) == cancelOn {
					cancel()
					return ctx.Err()
				}
				return context.DeadlineExceeded
			})
			if _, err := p.Health(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Health err = %v, want context.Canceled", err)
			}
			if pings != cancelOn || torn != 0 || *launches != 0 {
				t.Errorf("pings=%d teardowns=%d launches=%d, want %d/0/0", pings, torn, *launches, cancelOn)
			}
			wantCounts(t, p, 0, 0)
		})
	}
}
