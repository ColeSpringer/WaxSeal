package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/internal/minter"
)

// fakeWarmer scripts the two startup checks and records how often each ran.
type fakeWarmer struct {
	warm, selfTest         func(ctx context.Context) error
	warmed, selfTested     bool
	warmCalls, selfTestRan int
	key                    string
}

func (f *fakeWarmer) Warm(ctx context.Context, apiKey string) error {
	f.warmed, f.key = true, apiKey
	f.warmCalls++
	if f.warm == nil {
		return nil
	}
	return f.warm(ctx)
}

func (f *fakeWarmer) SelfTest(ctx context.Context, _ string) error {
	f.selfTested = true
	f.selfTestRan++
	if f.selfTest == nil {
		return nil
	}
	return f.selfTest(ctx)
}

// A warm-up YouTube refuses does not stop the daemon: a container that exits
// here restarts into the same refusal every half minute and never turns
// healthy. Startup goes on to serve, and says why.
func TestWarmUpRefusalDoesNotStopStartup(t *testing.T) {
	refused := &minter.RetryAfterError{
		Err:   errors.New("waxseal: ytcfg visitor_data not available before deadline"),
		After: 15 * time.Second,
	}
	w := &fakeWarmer{warm: func(context.Context) error { return refused }}
	var logs bytes.Buffer
	retryIn, err := warmUp(context.Background(), w, "K", "alice", buildLogger("info", &logs))
	if err != nil {
		t.Fatalf("warmUp = %v, want nil: a refusal is not a startup failure", err)
	}
	if retryIn != 15*time.Second {
		t.Errorf("retryIn = %v, want the minter's stated wait, 15s", retryIn)
	}
	if w.key != "K" || w.selfTested {
		t.Errorf("warmed key %q, self-test ran = %v; want K and no self-test without a session", w.key, w.selfTested)
	}
	out := logs.String()
	// The cause is always on the line. A wait on the error does not mean the
	// minter logged it: the pool's relaunch backoff carries one and logs nothing.
	for _, want := range []string{"level=WARN", "tenant=alice", "retry_in=15s", "visitor_data not available"} {
		if !strings.Contains(out, want) {
			t.Errorf("log = %q, want it to contain %q", out, want)
		}
	}
}

// A warm-up cut short by its budget also serves: a request has longer than the
// warm-up did, and the launch it left running may yet finish.
func TestWarmUpTimeoutDoesNotStopStartup(t *testing.T) {
	for _, w := range []*fakeWarmer{
		{warm: func(context.Context) error { return context.DeadlineExceeded }},
		{selfTest: func(context.Context) error { return context.DeadlineExceeded }},
	} {
		var logs bytes.Buffer
		retryIn, err := warmUp(context.Background(), w, "", "", buildLogger("info", &logs))
		if err != nil {
			t.Fatalf("warmUp = %v, want nil", err)
		}
		if retryIn != warmRetryBase {
			t.Errorf("retryIn = %v, want the base wait %v when none was stated", retryIn, warmRetryBase)
		}
		if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "deadline exceeded") {
			t.Errorf("log = %q, want a WARN carrying the error", out)
		}
	}
}

// browserFaultFor is a launch that failed in the browser itself, graded, with
// the cool-down it opened.
func browserFaultFor(after time.Duration) error {
	return &minter.RetryAfterError{
		Err:   fmt.Errorf("waxseal: inject bundle: EvalError: %w", browser.ErrLocalFault),
		After: after,
	}
}

// A browser that cannot set a session up twice running, and a daemon that
// attested but cannot mint, are broken installs. They stop startup as they
// always did, so a supervisor shows a restarting service instead of a healthy
// one.
func TestWarmUpStopsOnABrokenInstall(t *testing.T) {
	cannotMint := fmt.Errorf("%w after 3 attempts: boom", minter.ErrSelfTestMint)
	for _, tc := range []struct {
		name      string
		w         *fakeWarmer
		want      error
		wantWarms int
	}{
		{"browser fault", &fakeWarmer{warm: func(context.Context) error { return browserFaultFor(time.Millisecond) }}, browser.ErrLocalFault, 2},
		{"self-test mint", &fakeWarmer{selfTest: func(context.Context) error { return cannotMint }}, minter.ErrSelfTestMint, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			retryIn, err := warmUp(context.Background(), tc.w, "", "", buildLogger("info", &logs))
			if !errors.Is(err, tc.want) || retryIn != 0 {
				t.Fatalf("warmUp = (%v, %v), want the fault returned so startup fails, and no retry", retryIn, err)
			}
			if tc.w.warmCalls != tc.wantWarms {
				t.Errorf("warm attempts = %d, want %d", tc.w.warmCalls, tc.wantWarms)
			}
			if strings.Contains(logs.String(), "serving") {
				t.Errorf("log = %q, want no claim to be serving", logs.String())
			}
		})
	}
}

// One failure in the browser is not yet a broken install: Chromium can crash
// once under memory pressure, and exiting on that is the restart loop over a
// transient this daemon exists to avoid. Startup tries once more, after the
// cool-down the failure opened, and goes by the second answer.
func TestWarmUpConfirmsABrowserFaultBeforeStopping(t *testing.T) {
	crashed := fmt.Errorf("waxseal: new page: %w: %w", browser.ErrConnClosed, browser.ErrLocalFault)
	for _, tc := range []struct {
		name        string
		first       error
		second      error
		wantRetryIn time.Duration
	}{
		{"then a session", browserFaultFor(time.Millisecond), nil, 0},
		{"a crash, then a session", crashed, nil, 0},
		{"then a refusal", browserFaultFor(time.Millisecond), refusedFor(30 * time.Second), 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &fakeWarmer{}
			w.warm = func(context.Context) error {
				if w.warmCalls == 1 {
					return tc.first
				}
				return tc.second
			}
			var logs bytes.Buffer
			retryIn, err := warmUp(context.Background(), w, "", "", buildLogger("info", &logs))
			if err != nil || retryIn != tc.wantRetryIn {
				t.Fatalf("warmUp = (%v, %v), want (%v, nil)", retryIn, err, tc.wantRetryIn)
			}
			if w.warmCalls != 2 {
				t.Errorf("warm attempts = %d, want 2", w.warmCalls)
			}
			if !strings.Contains(logs.String(), "trying once more") {
				t.Errorf("log = %q, want the first failure and the second try announced", logs.String())
			}
		})
	}
}

// A cool-down refusal quotes the failure that opened it, browser fault
// included. It is not a second failure, and must not confirm one.
func TestWarmUpDoesNotTakeARefusalForConfirmation(t *testing.T) {
	w := &fakeWarmer{}
	w.warm = func(context.Context) error {
		if w.warmCalls == 1 {
			return browserFaultFor(time.Millisecond)
		}
		return &minter.RetryAfterError{
			Err:   fmt.Errorf("%w (failed launches in a row: 1): %w", minter.ErrLaunchCooldown, browser.ErrLocalFault),
			After: 10 * time.Second,
		}
	}
	retryIn, err := warmUp(context.Background(), w, "", "", buildLogger("info", &bytes.Buffer{}))
	if err != nil || retryIn != 10*time.Second {
		t.Fatalf("warmUp = (%v, %v), want startup to go on and retry in 10s", retryIn, err)
	}
}

// A clean warm-up logs nothing at warn.
func TestWarmUpSuccessIsQuiet(t *testing.T) {
	var logs bytes.Buffer
	w := &fakeWarmer{}
	if retryIn, err := warmUp(context.Background(), w, "", "", buildLogger("info", &logs)); err != nil || retryIn != 0 {
		t.Fatalf("warmUp = (%v, %v), want a warm daemon with nothing left to retry", retryIn, err)
	}
	if !w.warmed || !w.selfTested {
		t.Errorf("warmed=%v selfTested=%v, want both", w.warmed, w.selfTested)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("log = %q, want no warning", logs.String())
	}
}

// A stop requested during warm-up is reported as the interrupt it is, so the
// command can exit 0 instead of going on to serve.
func TestWarmUpReportsInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &fakeWarmer{warm: func(wctx context.Context) error {
		cancel()
		<-wctx.Done()
		return wctx.Err()
	}}
	var logs bytes.Buffer
	if _, err := warmUp(ctx, w, "", "", buildLogger("info", &logs)); !errors.Is(err, context.Canceled) {
		t.Fatalf("warmUp = %v, want context.Canceled", err)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("log = %q, want no warm-up warning for an interrupt", logs.String())
	}
}

// The warmed tenant is the same on every start: the one whose label sorts
// first. A map walk picked a different one each boot.
func TestWarmTenantIsDeterministic(t *testing.T) {
	keys := map[string]string{"k9": "bob", "k5": "alice", "k1": "carol"}
	for range 50 {
		if key, label := warmTenant(keys); key != "k5" || label != "alice" {
			t.Fatalf("warmTenant = (%q, %q), want (k5, alice)", key, label)
		}
	}
	if key, label := warmTenant(nil); key != "" || label != "" {
		t.Errorf("warmTenant(nil) = (%q, %q), want the keyless tenant, which has no key to name", key, label)
	}
}

// refusedFor is a launch the minter graded, with the wait it stated.
func refusedFor(after time.Duration) error {
	return &minter.RetryAfterError{Err: errors.New("waxseal: ytcfg visitor_data not available before deadline"), After: after}
}

// An unprompted attempt backs off further than one a consumer is waiting on:
// the wait doubles past the request cool-down's 5 minute cap, up to
// warmRetryMax, and never undercuts what the minter stated.
func TestNextWarmWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		prev time.Duration
		err  error
		want time.Duration
	}{
		{"doubles", 15 * time.Second, errors.New("x"), 30 * time.Second},
		{"tracks the cool-down", 15 * time.Second, refusedFor(30 * time.Second), 30 * time.Second},
		{"passes the request cap", 4 * time.Minute, refusedFor(5 * time.Minute), 8 * time.Minute},
		{"honors a longer stated wait", 15 * time.Second, refusedFor(2 * time.Minute), 2 * time.Minute},
		{"caps", 16 * time.Minute, refusedFor(5 * time.Minute), warmRetryMax},
		{"stays capped", warmRetryMax, refusedFor(5 * time.Minute), warmRetryMax},
	} {
		if got := nextWarmWait(tc.prev, tc.err); got != tc.want {
			t.Errorf("%s: nextWarmWait(%v, %v) = %v, want %v", tc.name, tc.prev, tc.err, got, tc.want)
		}
	}
	// The whole schedule from the first refusal, attempt by attempt.
	var got []time.Duration
	for w := 15 * time.Second; len(got) < 9; w = nextWarmWait(w, errors.New("x")) {
		got = append(got, w)
	}
	want := []time.Duration{
		15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("schedule = %v, want %v", got, want)
	}
}

// A daemon that failed its warm-up attests a session without being asked: it
// keeps trying until one does, then stops. Without this an idle daemon stays
// cold, and not ready, after YouTube relents.
//
// It only launches. The streaming proof is left to the request that needs it,
// which runs it behind the proof and bot-check cool-downs; the self-test does
// not consult them, and run here it would replay a proof against an identity a
// request had just found walled, holding the page from every request.
func TestKeepWarmingAttestsASession(t *testing.T) {
	w := &fakeWarmer{}
	w.warm = func(context.Context) error {
		if w.warmCalls <= 3 {
			return refusedFor(time.Millisecond)
		}
		return nil
	}
	var logs bytes.Buffer
	keepWarming(context.Background(), w, "K", "alice", buildLogger("info", &logs), time.Millisecond)
	if w.warmCalls != 4 || w.selfTestRan != 0 {
		t.Errorf("warm attempts = %d, self-tests = %d; want 4 and 0", w.warmCalls, w.selfTestRan)
	}
	if w.key != "K" {
		t.Errorf("warmed key %q, want K", w.key)
	}
	out := logs.String()
	if !strings.Contains(out, "background warm-up attested a session") || !strings.Contains(out, "tenant=alice") {
		t.Errorf("log = %q, want the completion line naming the tenant", out)
	}
	if n := strings.Count(out, "next_in="); n != 3 {
		t.Errorf("log has %d failed-attempt lines, want 3", n)
	}
	if n := strings.Count(out, "visitor_data not available"); n != 3 {
		t.Errorf("log names the cause on %d of 3 failed attempts", n)
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("log = %q, want a refusal's retries at info", out)
	}
}

// A serving daemon does not exit, so the retry goes on through a failure in
// the browser itself, but says so at warn: startup would have stopped on it.
func TestKeepWarmingWarnsOnABrowserFault(t *testing.T) {
	w := &fakeWarmer{}
	w.warm = func(context.Context) error {
		if w.warmCalls == 1 {
			return browserFaultFor(time.Millisecond)
		}
		return nil
	}
	var logs bytes.Buffer
	keepWarming(context.Background(), w, "", "", buildLogger("info", &logs), time.Millisecond)
	if w.warmCalls != 2 {
		t.Errorf("warm attempts = %d, want 2", w.warmCalls)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "inject bundle") {
		t.Errorf("log = %q, want a WARN carrying the fault", out)
	}
}

// A stop ends the retry, in the wait and in an attempt alike, without a line
// about a failure that was only the shutdown.
func TestKeepWarmingStopsWhenCanceled(t *testing.T) {
	t.Run("while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w := &fakeWarmer{}
		keepWarming(ctx, w, "", "", buildLogger("info", &bytes.Buffer{}), time.Hour)
		if w.warmCalls != 0 {
			t.Errorf("warm attempts = %d, want 0", w.warmCalls)
		}
	})
	t.Run("during an attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		w := &fakeWarmer{warm: func(wctx context.Context) error {
			cancel()
			<-wctx.Done()
			return wctx.Err()
		}}
		var logs bytes.Buffer
		keepWarming(ctx, w, "", "", buildLogger("info", &logs), time.Millisecond)
		if w.warmCalls != 1 {
			t.Errorf("warm attempts = %d, want 1", w.warmCalls)
		}
		if logs.Len() != 0 {
			t.Errorf("log = %q, want nothing for a shutdown", logs.String())
		}
	})
}
