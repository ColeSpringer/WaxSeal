package minter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/colespringer/waxseal/internal/browser"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSession is an in-memory minterSession for testing the Minter's reliability
// logic without a browser.
type fakeSession struct {
	mint            func(identifier string) (browser.MintResult, error)
	playerCtx       func(videoID string) (browser.PlayerContext, error)
	ping            func() error // nil reports a healthy browser
	establishErr    error
	establishBlocks bool // EnsureEstablished blocks until ctx is done, honouring cancellation
	cookies         []*http.Cookie
	cookiesErr      error
	id              browser.Identity // zero value reports a default visitor_data
	established     bool
	lastProbe       browser.FullLengthProbe
	lastProbeAt     time.Time
	closed          atomic.Bool

	// proofMu guards the establishment bookkeeping, which a player-context call
	// writes while a concurrent health probe reads it.
	proofMu    sync.Mutex
	proofCalls int  // EnsureEstablished invocations
	provedOnce bool // set by a successful EnsureEstablished
}

func (f *fakeSession) Mint(_ context.Context, id string) (browser.MintResult, error) {
	return f.mint(id)
}
func (f *fakeSession) PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, error) {
	// Match the real session's cancellation behavior.
	if err := ctx.Err(); err != nil {
		return browser.PlayerContext{}, err
	}
	if f.playerCtx == nil {
		return browser.PlayerContext{ServerAbrStreamingURL: "https://example.googlevideo.com/videoplayback?n=scrambled", VisitorData: "vd"}, nil
	}
	return f.playerCtx(videoID)
}

// EnsureEstablished mirrors the real session: it proves once and reports the
// configured error every time when proving fails. establishBlocks simulates a
// proof still in flight when the caller's context ends, matching the real
// session's cancellation behavior.
func (f *fakeSession) EnsureEstablished(ctx context.Context) error {
	if f.establishBlocks {
		<-ctx.Done()
		return ctx.Err()
	}
	f.proofMu.Lock()
	defer f.proofMu.Unlock()
	f.proofCalls++
	if f.establishErr != nil {
		return f.establishErr
	}
	f.provedOnce = true
	return nil
}

// proofCount reports how many times EnsureEstablished was invoked.
func (f *fakeSession) proofCount() int {
	f.proofMu.Lock()
	defer f.proofMu.Unlock()
	return f.proofCalls
}

// Ping gives cancellation precedence over the configured result.
func (f *fakeSession) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.ping == nil {
		return nil
	}
	return f.ping()
}
func (f *fakeSession) AttestKind() string { return "integrity" }
func (f *fakeSession) Identity() browser.Identity {
	if f.id.VisitorData == "" {
		return browser.Identity{VisitorData: "vd"}
	}
	return f.id
}
func (f *fakeSession) BrowserCookies(context.Context) ([]*http.Cookie, error) {
	return f.cookies, f.cookiesErr
}
func (f *fakeSession) Established() bool {
	f.proofMu.Lock()
	defer f.proofMu.Unlock()
	return f.established || f.provedOnce
}
func (f *fakeSession) LastProof() (browser.FullLengthProbe, time.Time) {
	return f.lastProbe, f.lastProbeAt
}
func (f *fakeSession) Close() { f.closed.Store(true) }

// newBareMinter builds a browserless Minter with the mint-to-establishment
// separation disabled, so tests that do not measure the spacing itself never
// wait it out. The attestation pre-mint still runs, which is why several tests
// below count one more mint than the request they make. A test that measures the
// separation sets mintSeparation itself before its first call.
func newBareMinter(streamingMaxAge, reportDebounce time.Duration) *Minter {
	m := NewMinter("v", browser.Options{}, streamingMaxAge, reportDebounce, 0)
	m.mintSeparation = 0
	return m
}

// newTestMinter returns a Minter whose launcher records each created session and
// uses the supplied per-mint behaviour.
func newTestMinter(mint func(id string) (browser.MintResult, error)) (*Minter, *int64, *[]*fakeSession, *sync.Mutex) {
	m, launches, sessions, smu := newTestMinterFull(mint, nil)
	return m, launches, sessions, smu
}

// newTestMinterFull is newTestMinter with an explicit per-session PlayerContext
// behaviour (nil uses the fakeSession default).
func newTestMinterFull(mint func(id string) (browser.MintResult, error), playerCtx func(videoID string) (browser.PlayerContext, error)) (*Minter, *int64, *[]*fakeSession, *sync.Mutex) {
	var launches int64
	var sessions []*fakeSession
	var smu sync.Mutex
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		fs := &fakeSession{mint: mint, playerCtx: playerCtx}
		smu.Lock()
		sessions = append(sessions, fs)
		smu.Unlock()
		return fs, nil
	}
	return m, &launches, &sessions, &smu
}

// TestMinterSingleFlightAttestation: many concurrent callers during one launch
// coalesce into a single attestation.
func TestMinterSingleFlightAttestation(t *testing.T) {
	var launches int64
	var once sync.Once
	launchStarted := make(chan struct{})
	release := make(chan struct{})
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		once.Do(func() { close(launchStarted) })
		<-release // hold the launch open so concurrent callers pile up
		return &fakeSession{mint: func(string) (browser.MintResult, error) { return browser.MintResult{}, nil }}, nil
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = m.Warm(ctx) }()
	<-launchStarted // one launch is now in flight
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = m.Warm(ctx) }()
	}
	time.Sleep(50 * time.Millisecond) // let the 9 reach the single-flight wait
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&launches); got != 1 {
		t.Fatalf("launches = %d, want 1 (single-flight should coalesce)", got)
	}
	if got := m.metrics.Attestations.Load(); got != 1 {
		t.Errorf("attestations metric = %d, want 1", got)
	}
}

// TestMinterCacheNoReattest: a repeat request for the same (scope, binding) is
// served from cache, with no second mint and no second attestation (the
// "a 403-driven retry must not re-attest" guarantee). A new binding mints again
// on the same session (still one attestation).
func TestMinterCacheNoReattest(t *testing.T) {
	var mints int64
	m, launches, _, _ := newTestMinter(func(id string) (browser.MintResult, error) {
		atomic.AddInt64(&mints, 1)
		return browser.MintResult{Kind: "integrity", Token: "tok-" + id, TokenLen: 4, Identifier: id, Lifetime: 3600}, nil
	})
	ctx := context.Background()

	// The very first request asks for the session's own visitor_data, which the
	// attestation pre-mint already minted and cached. Mint re-checks the cache
	// after the launch it triggers, so this is served from the pre-mint instead
	// of minting a second, redundant token.
	r1, c1, err := m.Mint(ctx, "gvs", "vd")
	if err != nil || !c1 {
		t.Fatalf("first mint: cached=%v err=%v, want cached=true (served by the pre-mint)", c1, err)
	}
	r2, c2, err := m.Mint(ctx, "gvs", "vd")
	if err != nil || !c2 {
		t.Fatalf("repeat mint: cached=%v err=%v, want cached=true", c2, err)
	}
	if r1.Token != r2.Token {
		t.Errorf("cached token = %q, want same as first %q", r2.Token, r1.Token)
	}
	// One mint, not two: the pre-mint alone satisfied both requests above.
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Errorf("mints = %d, want 1 (the pre-mint; both requests asked for the pre-minted binding)", got)
	}
	// A different scope and binding mints again, but still on the one attestation.
	if _, c3, _ := m.Mint(ctx, "player", "vid"); c3 {
		t.Errorf("new binding should not be a cache hit")
	}
	if got := atomic.LoadInt64(&mints); got != 2 {
		t.Errorf("mints = %d, want 2 (pre-mint + the player/vid binding)", got)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (never re-attest for cache/new-binding)", got)
	}
}

// TestMinterMaxAgeRecycle: a session older than maxAge is proactively retired and
// relaunched on the next ensure.
func TestMinterMaxAgeRecycle(t *testing.T) {
	m, launches, sessions, smu := newTestMinter(func(string) (browser.MintResult, error) {
		return browser.MintResult{Kind: "integrity", Token: "t", Lifetime: 3600}, nil
	})
	m.maxAge = time.Millisecond
	ctx := context.Background()

	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // exceed maxAge
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("warm 2: %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (stale session recycled)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Errorf("recycled session should be closed")
	}
}

// TestMinterStreamingRecycleOnHandoff checks that a stale streaming session is
// recycled on the next PlayerContext handoff. The call returns a fresh
// generation, closes the old session, and bumps StreamingRecycles. The deadline
// is forced into the past so the test does not sleep.
func TestMinterStreamingRecycleOnHandoff(t *testing.T) {
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		nil, // default fake PlayerContext
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	gen1 := m.Generation()
	m.ExpireStreamingDeadlineForTest() // the next streaming handoff must recycle

	_, gen2, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("player-context: %v", err)
	}
	if gen2 <= gen1 {
		t.Errorf("generation = %d, want > %d (stale streaming session recycled on handoff)", gen2, gen1)
	}
	if got := m.metrics.StreamingRecycles.Load(); got != 1 {
		t.Errorf("streaming_recycles = %d, want 1", got)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (recycle relaunched on the handoff)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Error("recycled session should be closed")
	}
}

// TestMinterStreamingRecycleNotOnTokenOnly keeps token-only Mint from using the
// streaming handoff recycle. A stale streaming deadline must not relaunch an
// otherwise usable session for a bare token request.
func TestMinterStreamingRecycleNotOnTokenOnly(t *testing.T) {
	m, launches, _, _ := newTestMinter(func(id string) (browser.MintResult, error) {
		return browser.MintResult{Kind: "integrity", Token: "tok-" + id, Lifetime: 3600}, nil
	})
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	m.ExpireStreamingDeadlineForTest()

	if _, _, err := m.Mint(ctx, "gvs", "vd"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (a token-only mint must not recycle)", got)
	}
	if got := m.metrics.StreamingRecycles.Load(); got != 0 {
		t.Errorf("streaming_recycles = %d, want 0 (no streaming handoff occurred)", got)
	}
}

// TestMinterEscalationLadder: a mint that fails twice triggers one in-place retry
// then a relaunch (re-attest) on a fresh session; the old session is closed.
func TestMinterEscalationLadder(t *testing.T) {
	var attempt int64
	// Attempt 1 is the pre-mint at attestation; 2 and 3 are the request's mint and
	// its in-place retry; 4 is the relaunch's own pre-mint. Mint does not recheck
	// the cache after a relaunch, so attempt 4 failing or succeeding cannot change
	// which attempt serves the request; it is still made to fail here so the
	// sequence stays unambiguous, and the request's own post-relaunch mint (5) is
	// visibly the one that produces the token this test asserts on.
	m, launches, sessions, smu := newTestMinter(func(string) (browser.MintResult, error) {
		if n := atomic.AddInt64(&attempt, 1); n <= 4 {
			return browser.MintResult{}, fmt.Errorf("transient failure %d", n)
		}
		return browser.MintResult{Kind: "integrity", Token: "ok", Lifetime: 3600}, nil
	})
	ctx := context.Background()

	r, cached, err := m.Mint(ctx, "gvs", "vd")
	if err != nil {
		t.Fatalf("mint after escalation: %v", err)
	}
	if cached || r.Token != "ok" {
		t.Fatalf("got token=%q cached=%v, want token=ok cached=false", r.Token, cached)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (initial + one relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 1 {
		t.Errorf("escalations = %d, want 1", got)
	}
	if got := m.metrics.Crashes.Load(); got != 0 {
		t.Errorf("crashes = %d, want 0 (a mint failure relaunch is not a browser loss)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(*sessions) != 2 {
		t.Fatalf("sessions created = %d, want 2", len(*sessions))
	}
	if !(*sessions)[0].closed.Load() {
		t.Errorf("first (failed) session should be closed after escalation")
	}
	if (*sessions)[1].closed.Load() {
		t.Errorf("second (current) session should be live")
	}
}

// TestMinterCrashKeepsCacheThenRelaunchInvalidates: retiring a session (the path
// a crash takes) does not by itself force a re-attest; already-minted tokens
// outlive the browser, so a cached binding is still served (the per-IP-scarce
// attestation is preserved). A cache-missing request relaunches (bumping the
// generation), which then invalidates the old generation's cached tokens.
func TestMinterCrashKeepsCacheThenRelaunchInvalidates(t *testing.T) {
	var mints int64
	m, launches, sessions, smu := newTestMinter(func(id string) (browser.MintResult, error) {
		n := atomic.AddInt64(&mints, 1)
		return browser.MintResult{Kind: "integrity", Token: fmt.Sprintf("tok%d", n), Lifetime: 3600}, nil
	})
	ctx := context.Background()

	if _, _, err := m.Mint(ctx, "gvs", "vd"); err != nil { // gen 1; served by the pre-mint
		t.Fatalf("first mint: %v", err)
	}
	_, gen, err := m.ensure(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	m.retire(gen, "simulated crash", true) // Browser loss does not advance the generation.

	smu.Lock()
	firstClosed := (*sessions)[0].closed.Load()
	smu.Unlock()
	if !firstClosed {
		t.Errorf("retired session should be closed")
	}

	// The already minted token remains valid, so the cache serves it without a relaunch.
	if _, cached, _ := m.Mint(ctx, "gvs", "vd"); !cached {
		t.Errorf("cached token should survive a crash (no needless re-attest)")
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (a crash alone must not re-attest)", got)
	}

	// A new binding misses the cache and causes a generation-2 relaunch.
	if _, cached, _ := m.Mint(ctx, "player", "vid2"); cached {
		t.Errorf("new binding should not be cached")
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (cache miss after crash relaunches)", got)
	}

	// The generation bump clears older entries from the cache, so it holds only
	// what generation 2 produced: the relaunch's pre-mint, cached under both the
	// gvs and pot scopes, and the binding this request asked for. Three entries,
	// not two: the pre-mint fix added the second scope.
	m.mu.Lock()
	cacheLen := len(m.cache)
	m.mu.Unlock()
	if cacheLen != 3 {
		t.Errorf("cache size after relaunch = %d, want 3 (old entries cleared; pre-mint under two scopes + new binding)", cacheLen)
	}

	// The generation-1 gvs/vd entry is stale after the relaunch. The relaunch's
	// own pre-mint refilled that key, so the served token must be the new one
	// (tok2, the generation-2 pre-mint), never the generation-1 token (tok1).
	r, cached, _ := m.Mint(ctx, "gvs", "vd")
	if !cached {
		t.Errorf("gvs/vd should be served from the relaunch's pre-mint")
	}
	if r.Token != "tok2" {
		t.Errorf("gvs/vd token = %q, want tok2; the old-generation entry should be invalidated by the relaunch", r.Token)
	}
}

// TestMinterPlayerContextReusesWarmSession: PlayerContext serves off the warm
// attested session without a fresh attestation. The URL depends on the browser's
// provenance rather than a new mint.
func TestMinterPlayerContextReusesWarmSession(t *testing.T) {
	var calls int64
	m, launches, _, _ := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(videoID string) (browser.PlayerContext, error) {
			atomic.AddInt64(&calls, 1)
			return browser.PlayerContext{
				PlayabilityStatus:     "OK",
				ServerAbrStreamingURL: "https://r1.googlevideo.com/videoplayback?n=scram-" + videoID,
				VisitorData:           "vd",
				ClientVersion:         "2.20260606.02.00",
				AudioFormats:          []browser.AudioFormat{{Itag: 251, LMT: "1719185012384481", MimeType: "audio/webm"}},
			}, nil
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	pc, _, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("player-context: %v", err)
	}
	if pc.ServerAbrStreamingURL != "https://r1.googlevideo.com/videoplayback?n=scram-vid" || len(pc.AudioFormats) != 1 {
		t.Fatalf("unexpected context: %+v", pc)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (player-context reuses the warm session)", got)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("player-context calls = %d, want 1", got)
	}
}

// TestMinterPlayerContextEscalation: a player-context that fails twice triggers one
// in-place retry then a relaunch+re-attest on a fresh session, mirroring the mint
// escalation ladder; the failed session is closed.
func TestMinterPlayerContextEscalation(t *testing.T) {
	var attempt int64
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			if n := atomic.AddInt64(&attempt, 1); n <= 2 {
				return browser.PlayerContext{}, fmt.Errorf("transient failure %d", n)
			}
			return browser.PlayerContext{PlayabilityStatus: "OK", ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
		},
	)
	ctx := context.Background()

	pc, _, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("player-context after escalation: %v", err)
	}
	if pc.ServerAbrStreamingURL != "https://r/ok" {
		t.Fatalf("got URL=%q, want https://r/ok", pc.ServerAbrStreamingURL)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (initial + one relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 1 {
		t.Errorf("escalations = %d, want 1", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(*sessions) != 2 {
		t.Fatalf("sessions created = %d, want 2", len(*sessions))
	}
	if !(*sessions)[0].closed.Load() {
		t.Errorf("first (failed) session should be closed after escalation")
	}
	if (*sessions)[1].closed.Load() {
		t.Errorf("second (current) session should be live")
	}
}

// TestMinterPlayerContextUnplayableNoEscalation: a terminal ErrUnplayable does not
// walk the ladder (no relaunch, no re-attest, the warm session survives), since
// relaunching cannot make an unplayable video playable.
func TestMinterPlayerContextUnplayableNoEscalation(t *testing.T) {
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			return browser.PlayerContext{}, fmt.Errorf("%w: UNPLAYABLE", browser.ErrUnplayable)
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	_, _, err := m.PlayerContext(ctx, "vid")
	if err == nil || !errors.Is(err, browser.ErrUnplayable) {
		t.Fatalf("err = %v, want ErrUnplayable", err)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (unplayable must not relaunch/re-attest)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0", got)
	}
	if got := m.metrics.PlayerContextFailures.Load(); got != 1 {
		t.Errorf("player_context_failures = %d, want 1", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Errorf("session should not be retired for an unplayable video")
	}
}

// TestMinterPlayerContextUnplayableNegativeCache: a repeat request for a
// known-unplayable video is served from the negative cache: the session's
// PlayerContext is not called again (no mintMu, no eval).
func TestMinterPlayerContextUnplayableNegativeCache(t *testing.T) {
	var calls int64
	m, _, _, _ := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			atomic.AddInt64(&calls, 1)
			return browser.PlayerContext{}, fmt.Errorf("%w: LOGIN_REQUIRED", browser.ErrUnplayable)
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, browser.ErrUnplayable) {
		t.Fatalf("first: err = %v, want ErrUnplayable", err)
	}
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, browser.ErrUnplayable) {
		t.Fatalf("second: err = %v, want ErrUnplayable (from negative cache)", err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("session PlayerContext calls = %d, want 1 (second served from negative cache)", got)
	}
	if got := m.metrics.PlayerContextFailures.Load(); got != 2 {
		t.Errorf("player_context_failures = %d, want 2", got)
	}
}

// TestMinterPlayerContextCancelNoEscalation: a cancelled caller context fails without
// escalating: the warm attested session is not retired and there is no relaunch.
func TestMinterPlayerContextCancelNoEscalation(t *testing.T) {
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) { return browser.PlayerContext{}, context.Canceled },
	)
	if err := m.Warm(context.Background()); err != nil { // gen 1, live ctx
		t.Fatalf("warm: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client disconnected
	if _, _, err := m.PlayerContext(ctx, "vid"); err == nil {
		t.Fatal("want error on cancelled ctx")
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (a cancel must not relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0", got)
	}
	// A client cancel is not counted as a player-context failure (parity with Mint).
	if got := m.metrics.PlayerContextFailures.Load(); got != 0 {
		t.Errorf("player_context_failures = %d, want 0 (a client cancel is not a failure)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Errorf("warm session should survive a client cancel")
	}
}

// TestMinterPlayerContextStatus2OneRetryNoRelaunch checks that status-2
// confirmation failures get one in-place retry and then a refusal, with no
// relaunch. The warm session remains live and the request-level rejection counter
// advances once.
func TestMinterPlayerContextStatus2OneRetryNoRelaunch(t *testing.T) {
	var calls int64
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			atomic.AddInt64(&calls, 1)
			return browser.PlayerContext{}, fmt.Errorf("%w: budget expired", browser.ErrStatus2Unconfirmed)
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	_, _, err := m.PlayerContext(ctx, "vid")
	if err == nil || !errors.Is(err, browser.ErrStatus2Unconfirmed) {
		t.Fatalf("err = %v, want ErrStatus2Unconfirmed", err)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("session PlayerContext calls = %d, want 2 (initial + one in-place retry)", got)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (status-2 must not relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0 (no relaunch on status-2)", got)
	}
	if got := m.metrics.Status2Rejections.Load(); got != 1 {
		t.Errorf("status2_rejections = %d, want 1 (one refused request)", got)
	}
	if got := m.metrics.PlayerContextFailures.Load(); got != 2 {
		t.Errorf("player_context_failures = %d, want 2 (both attempts counted)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Errorf("warm session should survive a status-2 rejection (no relaunch)")
	}
}

// TestMinterPlayerContextStatus2TransientClears covers the recovery case: the
// single in-place retry succeeds, with no relaunch and no rejection counted.
func TestMinterPlayerContextStatus2TransientClears(t *testing.T) {
	var calls int64
	m, launches, _, _ := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			if n := atomic.AddInt64(&calls, 1); n == 1 {
				return browser.PlayerContext{}, fmt.Errorf("%w: budget expired", browser.ErrStatus2Unconfirmed)
			}
			return browser.PlayerContext{PlayabilityStatus: "OK", ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	pc, _, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("player-context after transient cleared: %v", err)
	}
	if pc.ServerAbrStreamingURL != "https://r/ok" {
		t.Fatalf("got URL=%q, want https://r/ok", pc.ServerAbrStreamingURL)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (in-place retry, no relaunch)", got)
	}
	if got := m.metrics.Status2Rejections.Load(); got != 0 {
		t.Errorf("status2_rejections = %d, want 0 (retry succeeded)", got)
	}
	if got := m.metrics.PlayerContexts.Load(); got != 1 {
		t.Errorf("player_contexts = %d, want 1", got)
	}
}

// An incomplete context, such as a video with no audio formats, is returned after
// the in-place retry without relaunching Chromium. The error is not
// negative-cached.
func TestMinterPlayerContextIncompleteNoRelaunch(t *testing.T) {
	var calls int64
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			atomic.AddInt64(&calls, 1)
			return browser.PlayerContext{}, fmt.Errorf("%w: no audio formats", browser.ErrIncompleteContext)
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	_, _, err := m.PlayerContext(ctx, "vid")
	if err == nil || !errors.Is(err, browser.ErrIncompleteContext) {
		t.Fatalf("err = %v, want ErrIncompleteContext", err)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("session PlayerContext calls = %d, want 2 (initial + one in-place retry)", got)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (incomplete context must not relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0 (no relaunch on incomplete context)", got)
	}
	// Not negative-cached: a second request runs again rather than returning a cached error.
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, browser.ErrIncompleteContext) {
		t.Fatalf("second request err = %v, want ErrIncompleteContext (not negative-cached)", err)
	}
	if got := atomic.LoadInt64(&calls); got != 4 {
		t.Errorf("session calls after a second request = %d, want 4 (not negative-cached)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Errorf("warm session should survive an incomplete-context rejection (no relaunch)")
	}
}

// TestMinterNegCacheBoundedEvicts: at capacity with every entry live, a new terminal
// result is still cached (evicting an older one) instead of dropped, so the map stays
// bounded and the newest unplayable id is the one kept.
func TestMinterNegCacheBoundedEvicts(t *testing.T) {
	m := newBareMinter(0, 0)
	for i := 0; i < minterNegCacheMax; i++ {
		m.negCachePut(fmt.Sprintf("vid%05d", i), browser.ErrUnplayable)
	}
	if got := len(m.negCache); got != minterNegCacheMax {
		t.Fatalf("negCache size = %d, want %d (filled to capacity)", got, minterNegCacheMax)
	}
	m.negCachePut("newestUnplay", browser.ErrUnplayable) // one past capacity, all others live
	if got := len(m.negCache); got != minterNegCacheMax {
		t.Errorf("negCache size = %d, want %d (stays bounded after eviction)", got, minterNegCacheMax)
	}
	if err := m.negCacheGet("newestUnplay"); !errors.Is(err, browser.ErrUnplayable) {
		t.Errorf("newest terminal result should be cached after eviction, got %v", err)
	}
}

// Refreshing an existing neg-cache entry at capacity must not evict a live entry,
// matching cachePut. The old code ran the eviction path on any insert, dropping an
// unrelated entry on a refresh. Each refresh must leave the cache full.
func TestMinterNegCacheRefreshNoEvict(t *testing.T) {
	m := newBareMinter(0, 0)
	for i := 0; i < minterNegCacheMax; i++ {
		m.negCachePut(fmt.Sprintf("vid%05d", i), browser.ErrUnplayable)
	}
	if got := len(m.negCache); got != minterNegCacheMax {
		t.Fatalf("setup: negCache size = %d, want %d", got, minterNegCacheMax)
	}
	for i := 0; i < 8; i++ { // distinct existing keys; eviction order is randomized
		m.negCachePut(fmt.Sprintf("vid%05d", i), browser.ErrUnplayable)
		if got := len(m.negCache); got != minterNegCacheMax {
			t.Fatalf("after refreshing an existing key, negCache size = %d, want %d (a live entry was evicted)", got, minterNegCacheMax)
		}
	}
}

// TestMinterCacheBoundedEvicts: at capacity, inserting a live token evicts one
// existing token rather than dropping the new one. The positive cache stays
// bounded and retains the latest insert.
func TestMinterCacheBoundedEvicts(t *testing.T) {
	m := newBareMinter(0, 0)
	m.gen = 1 // production never caches at gen 0
	for i := 0; i < minterCacheMax; i++ {
		m.cachePut(fmt.Sprintf("gvs|vd%05d", i), browser.MintResult{Lifetime: 3600}, m.gen)
	}
	if got := len(m.cache); got != minterCacheMax {
		t.Fatalf("cache size = %d, want %d (filled to capacity)", got, minterCacheMax)
	}
	if got := m.metrics.CacheEvictions.Load(); got != 0 {
		t.Fatalf("cache_evictions = %d, want 0 before exceeding capacity", got)
	}
	m.cachePut("gvs|newest", browser.MintResult{Token: "new", Lifetime: 3600}, m.gen) // one past capacity, all live
	if got := len(m.cache); got != minterCacheMax {
		t.Errorf("cache size = %d, want %d (stays bounded after eviction)", got, minterCacheMax)
	}
	if got := m.metrics.CacheEvictions.Load(); got != 1 {
		t.Errorf("cache_evictions = %d, want exactly 1 (an over-count would double here)", got)
	}
	if _, ok := m.cacheGet("gvs|newest"); !ok {
		t.Error("newest token should be cached after eviction")
	}
}

// TestMinterCacheEvictsNearestExpiry: at capacity with all entries live,
// inserting a token evicts the entry with the earliest expiry. This keeps a
// freshly minted token from replacing a longer-lived token by map iteration
// order.
func TestMinterCacheEvictsNearestExpiry(t *testing.T) {
	m := newBareMinter(0, 0)
	m.gen = 1
	now := time.Now()
	m.cache["gvs|soonest"] = cachedToken{gen: m.gen, expiry: now.Add(time.Minute)} // least remaining life
	for i := 0; i < minterCacheMax-1; i++ {
		m.cache[fmt.Sprintf("gvs|live%05d", i)] = cachedToken{gen: m.gen, expiry: now.Add(time.Hour)}
	}
	if got := len(m.cache); got != minterCacheMax {
		t.Fatalf("setup: cache size = %d, want %d", got, minterCacheMax)
	}
	m.cachePut("gvs|new", browser.MintResult{Lifetime: 3600}, m.gen) // forces exactly one eviction
	if got := m.metrics.CacheEvictions.Load(); got != 1 {
		t.Errorf("cache_evictions = %d, want 1", got)
	}
	if _, ok := m.cacheGet("gvs|soonest"); ok {
		t.Error("the soonest-to-expire entry should have been evicted")
	}
	if _, ok := m.cacheGet("gvs|new"); !ok {
		t.Error("the freshly inserted entry should survive")
	}
}

// TestMinterCachePutReclaimsExpired: when the cache is full of expired entries
// from the current generation, a new insert reclaims them during pruning. It
// should not count as a live-token eviction.
func TestMinterCachePutReclaimsExpired(t *testing.T) {
	m := newBareMinter(0, 0)
	m.gen = 1
	past := time.Now().Add(-time.Hour)
	for i := 0; i < minterCacheMax; i++ {
		m.cache[fmt.Sprintf("gvs|expired%05d", i)] = cachedToken{gen: m.gen, expiry: past} // current gen, expired
	}
	if got := len(m.cache); got != minterCacheMax {
		t.Fatalf("setup: cache size = %d, want %d", got, minterCacheMax)
	}
	m.cachePut("gvs|fresh", browser.MintResult{Lifetime: 3600}, m.gen)
	if got := m.metrics.CacheEvictions.Load(); got != 0 {
		t.Errorf("cache_evictions = %d, want 0 (expired entries reclaimed, no live eviction)", got)
	}
	if _, ok := m.cacheGet("gvs|fresh"); !ok {
		t.Error("freshly cached token should be present")
	}
	if got := len(m.cache); got != 1 {
		t.Errorf("cache size = %d, want 1 (expired entries reclaimed, fresh entry remains)", got)
	}
}

// TestMinterCacheGetEvictsExpired: cacheGet deletes an expired entry from the
// current generation on access, reclaiming it before the next session recycle.
func TestMinterCacheGetEvictsExpired(t *testing.T) {
	m := newBareMinter(0, 0)
	m.gen = 1
	m.cache["gvs|vd"] = cachedToken{gen: m.gen, expiry: time.Now().Add(-time.Minute)} // current gen, expired
	if _, ok := m.cacheGet("gvs|vd"); ok {
		t.Fatal("cacheGet returned a hit for an expired entry")
	}
	if got := len(m.cache); got != 0 {
		t.Errorf("cache size = %d, want 0 (expired entry deleted on access)", got)
	}
}

// TestMinterCloseClearsCaches: Close releases both cache maps so a retained
// reference to a closed Minter does not hold token entries.
func TestMinterCloseClearsCaches(t *testing.T) {
	m := newBareMinter(0, 0)
	m.cache["gvs|vd"] = cachedToken{gen: 1, expiry: time.Now().Add(time.Hour)}
	m.negCache["vid"] = negEntry{err: browser.ErrUnplayable, expiry: time.Now().Add(time.Hour)}
	m.Close() // session-less: no browser to tear down
	if got := len(m.cache); got != 0 {
		t.Errorf("cache size after Close = %d, want 0", got)
	}
	if got := len(m.negCache); got != 0 {
		t.Errorf("negCache size after Close = %d, want 0", got)
	}
}

// TestMinterNegCacheSurvivesRecycle: a generation bump clears only the positive
// cache. The negative cache is keyed by generation-independent unplayability, so
// a recycle must not probe the same unplayable video again. This guards the
// choice to leave m.negCache intact in ensure.
func TestMinterNegCacheSurvivesRecycle(t *testing.T) {
	m, _, _, _ := newTestMinter(okMint)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	m.negCachePut("deadvid", browser.ErrUnplayable)

	gen := m.Generation()
	if !m.retire(gen, "test recycle", false) {
		t.Fatalf("retire(%d) = false, want true", gen)
	}
	if err := m.Warm(ctx); err != nil { // gen 2; clear(m.cache) runs here
		t.Fatalf("warm 2: %v", err)
	}
	if m.Generation() == gen {
		t.Fatalf("generation did not advance past %d", gen)
	}
	if err := m.negCacheGet("deadvid"); !errors.Is(err, browser.ErrUnplayable) {
		t.Errorf("negCacheGet after recycle = %v, want the cached ErrUnplayable (neg cache must survive a gen bump)", err)
	}
}

// newPingMinter records each session and configures its Ping result.
func newPingMinter(ping func() error) (*Minter, *[]*fakeSession, *sync.Mutex) {
	var sessions []*fakeSession
	var smu sync.Mutex
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		fs := &fakeSession{
			mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
			ping: ping,
		}
		smu.Lock()
		sessions = append(sessions, fs)
		smu.Unlock()
		return fs, nil
	}
	return m, &sessions, &smu
}

// An unwarmed tenant must report ErrNoSession without launching a browser.
func TestMinterHealthNoSession(t *testing.T) {
	m, sessions, smu := newPingMinter(nil)
	if _, live, err := m.Health(context.Background()); live || !errors.Is(err, ErrNoSession) {
		t.Errorf("Health = (live=%v, %v), want (false, ErrNoSession)", live, err)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(*sessions) != 0 {
		t.Errorf("Health launched %d sessions, want 0", len(*sessions))
	}
}

// A successful probe returns a snapshot from the existing session.
func TestMinterHealthLivePing(t *testing.T) {
	m, sessions, smu := newPingMinter(nil)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	snap, live, err := m.Health(context.Background())
	if err != nil || !live {
		t.Errorf("Health = (live=%v, %v), want (true, nil)", live, err)
	}
	if snap.Identity.VisitorData == "" {
		t.Error("Health returned an empty identity for a live session")
	}
	if snap.AttestKind != "integrity" {
		t.Errorf("attestation grade = %q, want integrity", snap.AttestKind)
	}
	if snap.Generation == 0 {
		t.Error("snapshot generation is 0 for a live session")
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Error("a healthy session must not be retired")
	}
}

// A failed health probe retires the idle session and counts the browser loss.
func TestMinterHealthDeadPingRetires(t *testing.T) {
	m, sessions, smu := newPingMinter(func() error { return errors.New("cdp connection closed") })
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, _, err := m.Health(context.Background()); err == nil {
		t.Fatal("Health = nil, want the probe error")
	}
	if got := m.metrics.Crashes.Load(); got != 1 {
		t.Errorf("crashes = %d, want 1 (an unresponsive /ping counts as browser loss)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Error("failed probe did not retire the idle session")
	}
}

// After a retire leaves no live session, Health still reports the last-known
// generation. This keeps /ping consistent with /metrics in the retired-but-not-
// relaunched window, where /metrics reads m.gen (N) directly.
func TestMinterHealthNoSessionCarriesGeneration(t *testing.T) {
	m, _, _ := newPingMinter(nil)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	gen := m.Generation()
	if gen == 0 {
		t.Fatal("warm did not advance the generation")
	}
	if !m.retire(gen, "test retire", false) {
		t.Fatal("retire did not retire the live session")
	}
	snap, live, err := m.Health(context.Background())
	if live || !errors.Is(err, ErrNoSession) {
		t.Fatalf("Health = (live=%v, %v), want (false, ErrNoSession)", live, err)
	}
	if snap.Generation != gen {
		t.Errorf("no-session snapshot generation = %d, want %d (last-known)", snap.Generation, gen)
	}
}

// The probe-fail path is the most regression-prone one. It returns an error and
// retires the session, but the snapshot must still carry the just-failed
// generation so /ping reports N, not 0.
func TestMinterHealthDeadPingCarriesGeneration(t *testing.T) {
	m, _, _ := newPingMinter(func() error { return errors.New("cdp connection closed") })
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	gen := m.Generation()
	snap, live, err := m.Health(context.Background())
	if live || err == nil {
		t.Fatalf("Health = (live=%v, %v), want (false, the probe error)", live, err)
	}
	if snap.Generation != gen {
		t.Errorf("probe-fail snapshot generation = %d, want %d (the just-failed gen)", snap.Generation, gen)
	}
}

// retire counts a crash once for the current generation and ignores stale ones.
func TestMinterRetireCrashCount(t *testing.T) {
	m, _, _, _ := newTestMinter(okMint)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	gen := m.Generation()
	if m.retire(gen+1, "stale", true) {
		t.Error("retire(staleGen, crash) = true, want false")
	}
	if got := m.metrics.Crashes.Load(); got != 0 {
		t.Errorf("crashes after a stale retire = %d, want 0", got)
	}
	if !m.retire(gen, "browser connection lost", true) {
		t.Fatal("retire(gen, crash) = false, want true")
	}
	if got := m.metrics.Crashes.Load(); got != 1 {
		t.Errorf("crashes = %d, want 1", got)
	}
}

// A failed probe must not retire a session while its page is in use.
func TestMinterHealthDeadPingHeldMintMuNoRetire(t *testing.T) {
	m, sessions, smu := newPingMinter(func() error { return errors.New("cdp connection closed") })
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	m.mintMu.Lock() // Hold the page lock during the probe.
	_, _, err := m.Health(context.Background())
	m.mintMu.Unlock()
	if err == nil {
		t.Fatal("Health = nil, want the probe error")
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Error("probe retired a session while its page was in use")
	}
}

// Canceling the probe must not retire the session.
func TestMinterHealthCanceledCtxNoRetire(t *testing.T) {
	m, sessions, smu := newPingMinter(func() error { return errors.New("cdp connection closed") })
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := m.Health(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Health = %v, want context.Canceled", err)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Error("a canceled health check must not retire the session")
	}
}

// Health retries when the probed session is replaced concurrently.
func TestMinterHealthReprobesAfterRecycle(t *testing.T) {
	m := newBareMinter(0, 0)
	sess2 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd2"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
	}
	sess1 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd1"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		ping: func() error {
			// Replace the session while its probe is in progress.
			m.mu.Lock()
			m.sess = sess2
			m.gen++
			m.mu.Unlock()
			return errors.New("probed session was recycled")
		},
	}
	m.mu.Lock()
	m.sess = sess1
	m.gen = 1
	m.attestedAt = time.Now()
	m.mu.Unlock()

	snap, live, err := m.Health(context.Background())
	if err != nil || !live {
		t.Fatalf("Health after session replacement = (live=%v, %v), want (true, nil)", live, err)
	}
	if snap.Identity.VisitorData != "vd2" {
		t.Errorf("identity = %q, want vd2 from the replacement session", snap.Identity.VisitorData)
	}
	if sess1.closed.Load() {
		t.Error("Health retired the superseded session")
	}
}

// A successful probe of a session that was replaced mid-probe must not report the
// stale generation as live. Health re-probes and reports the current session.
func TestMinterHealthReprobesAfterRecycleOnSuccess(t *testing.T) {
	m := newBareMinter(0, 0)
	sess2 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd2"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
	}
	sess1 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd1"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		ping: func() error {
			// Replace the session while its probe runs, but still return success, as
			// if the old browser had not been torn down yet.
			m.mu.Lock()
			m.sess = sess2
			m.gen++
			m.mu.Unlock()
			return nil
		},
	}
	m.mu.Lock()
	m.sess = sess1
	m.gen = 1
	m.attestedAt = time.Now()
	m.mu.Unlock()

	snap, live, err := m.Health(context.Background())
	if err != nil || !live {
		t.Fatalf("Health after a superseded success = (live=%v, %v), want (true, nil)", live, err)
	}
	if snap.Identity.VisitorData != "vd2" {
		t.Errorf("identity = %q, want vd2: a stale success must not win over the replacement", snap.Identity.VisitorData)
	}
	if snap.Generation != 2 {
		t.Errorf("generation = %d, want 2 (the current session)", snap.Generation)
	}
}

// When the session is superseded on every probe attempt, Health exhausts its
// retries and reports a soft no-session (carrying the last-known generation)
// rather than a stale probe-failed error for an already-replaced session.
func TestMinterHealthPersistentSupersedeReportsNoSession(t *testing.T) {
	m := newBareMinter(0, 0)
	sess3 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd3"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
	}
	// sess2's probe swaps in sess3 and fails, mirroring sess1 (the second supersede).
	sess2 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd2"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		ping: func() error {
			m.mu.Lock()
			m.sess = sess3
			m.gen++
			m.mu.Unlock()
			return errors.New("probed session was recycled again")
		},
	}
	// sess1's probe swaps in sess2 and fails (the first supersede).
	sess1 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd1"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		ping: func() error {
			m.mu.Lock()
			m.sess = sess2
			m.gen++
			m.mu.Unlock()
			return errors.New("probed session was recycled")
		},
	}
	m.mu.Lock()
	m.sess = sess1
	m.gen = 1
	m.attestedAt = time.Now()
	m.mu.Unlock()

	snap, live, err := m.Health(context.Background())
	if live || !errors.Is(err, ErrNoSession) {
		t.Fatalf("Health under persistent supersession = (live=%v, %v), want (false, ErrNoSession)", live, err)
	}
	if snap.Generation != m.Generation() {
		t.Errorf("snapshot generation = %d, want %d (last-known)", snap.Generation, m.Generation())
	}
}

// SelfTest caches its GVS token under the regular mint key.
func TestMinterSelfTestCachesGVSMint(t *testing.T) {
	var mints int64
	m, _, _, _ := newTestMinter(func(id string) (browser.MintResult, error) {
		atomic.AddInt64(&mints, 1)
		return browser.MintResult{Kind: "integrity", Token: "gvs-" + id, TokenLen: 5, Identifier: id, Lifetime: 3600}, nil
	})
	ctx := context.Background()
	if err := m.SelfTest(ctx); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Errorf("mints during self-test = %d, want 1", got)
	}
	// The default fake identity reports visitor_data "vd".
	if _, cached, err := m.Mint(ctx, "gvs", "vd"); err != nil || !cached {
		t.Errorf("gvs/vd after self-test: cached=%v err=%v, want cached=true", cached, err)
	}
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Errorf("mints after the cache hit = %d, want 1", got)
	}
}

// SelfTest returns a persistent mint failure after its bounded retry.
func TestMinterSelfTestMintFatal(t *testing.T) {
	defer func(d time.Duration) { selfTestMintRetryDelay = d }(selfTestMintRetryDelay)
	selfTestMintRetryDelay = time.Millisecond
	m, _, _, _ := newTestMinter(func(string) (browser.MintResult, error) {
		return browser.MintResult{}, errors.New("mint broken")
	})
	if err := m.SelfTest(context.Background()); err == nil {
		t.Fatal("SelfTest = nil, want an error after a persistent mint failure")
	}
}

// SelfTest counts each failed mint attempt, same as the normal Mint path.
func TestMinterSelfTestMintFailuresCounted(t *testing.T) {
	defer func(d time.Duration) { selfTestMintRetryDelay = d }(selfTestMintRetryDelay)
	selfTestMintRetryDelay = time.Millisecond
	m, _, _, _ := newTestMinter(func(string) (browser.MintResult, error) {
		return browser.MintResult{}, errors.New("mint broken")
	})
	if err := m.SelfTest(context.Background()); err == nil {
		t.Fatal("SelfTest = nil, want a persistent mint failure")
	}
	// One per attempt, plus the pre-mint attestation makes before the self-test.
	if want := int64(selfTestMintAttempts) + 1; m.metrics.MintFailures.Load() != want {
		t.Errorf("mint_failures = %d, want %d (one per attempt, plus the pre-mint)", m.metrics.MintFailures.Load(), want)
	}
}

// SelfTest retries a transient mint failure without relaunching.
func TestMinterSelfTestMintRetrySucceeds(t *testing.T) {
	defer func(d time.Duration) { selfTestMintRetryDelay = d }(selfTestMintRetryDelay)
	selfTestMintRetryDelay = time.Millisecond
	var attempt int64
	m, launches, _, _ := newTestMinter(func(string) (browser.MintResult, error) {
		if atomic.AddInt64(&attempt, 1) == 1 {
			return browser.MintResult{}, errors.New("temporary mint failure")
		}
		return browser.MintResult{Kind: "integrity", Token: "ok", Lifetime: 3600}, nil
	})
	if err := m.SelfTest(context.Background()); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1", got)
	}
}

// SelfTest logs establishment failures instead of returning them.
func TestMinterSelfTestEstablishmentWarnOnly(t *testing.T) {
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{
			mint: func(string) (browser.MintResult, error) {
				return browser.MintResult{Kind: "integrity", Token: "t", Lifetime: 3600}, nil
			},
			establishErr: errors.New("full-length proof failed"),
		}, nil
	}
	if err := m.SelfTest(context.Background()); err != nil {
		t.Errorf("SelfTest = %v, want nil after a logged establishment failure", err)
	}
}

// SessionSnapshot returns identity and cookies after establishment.
func TestMinterSessionSnapshot(t *testing.T) {
	wantCookie := &http.Cookie{Name: "VISITOR_INFO1_LIVE", Value: "abc"}
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{
			id:      browser.Identity{VisitorData: "vd-snap"},
			cookies: []*http.Cookie{wantCookie},
			mint:    func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		}, nil
	}
	id, cookies, _, err := m.SessionSnapshot(context.Background())
	if err != nil {
		t.Fatalf("SessionSnapshot: %v", err)
	}
	if id.VisitorData != "vd-snap" {
		t.Errorf("visitor_data = %q, want vd-snap", id.VisitorData)
	}
	if len(cookies) != 1 || cookies[0].Name != "VISITOR_INFO1_LIVE" {
		t.Errorf("cookies = %+v, want one VISITOR_INFO1_LIVE", cookies)
	}
}

// SessionSnapshot must not export a session that failed establishment.
func TestMinterSessionSnapshotEstablishmentError(t *testing.T) {
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{
			id:           browser.Identity{VisitorData: "vd"},
			establishErr: errors.New("not established"),
			mint:         func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		}, nil
	}
	if _, _, _, err := m.SessionSnapshot(context.Background()); err == nil {
		t.Fatal("SessionSnapshot = nil, want the establishment error")
	}
}

// SessionSnapshot returns cookie-read failures.
func TestMinterSessionSnapshotCookieError(t *testing.T) {
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{
			id:         browser.Identity{VisitorData: "vd"},
			cookiesErr: errors.New("cdp cookie read failed"),
			mint:       func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		}, nil
	}
	if _, _, _, err := m.SessionSnapshot(context.Background()); err == nil {
		t.Fatal("SessionSnapshot = nil, want the propagated cookie error")
	}
}

func okMint(string) (browser.MintResult, error) {
	return browser.MintResult{Kind: "integrity", Token: "t", Lifetime: 3600}, nil
}

// newStreamingMinter builds a browserless Minter with a configurable streaming
// age limit.
func newStreamingMinter(streamingMaxAge time.Duration, playerCtx func(videoID string) (browser.PlayerContext, error)) (*Minter, *int64, *[]*fakeSession, *sync.Mutex) {
	var launches int64
	var sessions []*fakeSession
	var smu sync.Mutex
	m := newBareMinter(streamingMaxAge, 0)
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		fs := &fakeSession{mint: okMint, playerCtx: playerCtx}
		smu.Lock()
		sessions = append(sessions, fs)
		smu.Unlock()
		return fs, nil
	}
	return m, &launches, &sessions, &smu
}

// An overdue streaming deadline causes the next PlayerContext call to recycle the
// session.
func TestMinterStreamingFreshnessRecycleOnPlayerContext(t *testing.T) {
	m, launches, sessions, smu := newStreamingMinter(time.Hour, nil)
	ctx := context.Background()
	if _, gen, err := m.PlayerContext(ctx, "vid"); err != nil || gen != 1 {
		t.Fatalf("first player-context: gen=%d err=%v, want gen=1", gen, err)
	}
	// Force the streaming deadline into the past.
	m.mu.Lock()
	m.streamingDeadline = time.Now().Add(-time.Second)
	m.mu.Unlock()

	_, gen, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("second player-context: %v", err)
	}
	if gen != 2 {
		t.Errorf("gen after staleness recycle = %d, want 2", gen)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (stale streaming session recycled)", got)
	}
	if got := m.metrics.StreamingRecycles.Load(); got != 1 {
		t.Errorf("streaming_recycles = %d, want 1", got)
	}
	if got := m.metrics.ReportDrivenRecycles.Load(); got != 0 {
		t.Errorf("report_driven_recycles = %d, want 0 (staleness is not a report)", got)
	}
	if got := m.metrics.Crashes.Load(); got != 0 {
		t.Errorf("crashes = %d, want 0 (an age-driven streaming recycle is not browser loss)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Error("stale session should be closed")
	}
}

// The same freshness gate applies to SessionSnapshot.
func TestMinterStreamingFreshnessRecycleOnSessionSnapshot(t *testing.T) {
	m, launches, _, _ := newStreamingMinter(time.Hour, nil)
	ctx := context.Background()
	if _, _, gen, err := m.SessionSnapshot(ctx); err != nil || gen != 1 {
		t.Fatalf("first snapshot: gen=%d err=%v, want gen=1", gen, err)
	}
	m.mu.Lock()
	m.streamingDeadline = time.Now().Add(-time.Second)
	m.mu.Unlock()
	if _, _, gen, err := m.SessionSnapshot(ctx); err != nil || gen != 2 {
		t.Fatalf("second snapshot: gen=%d err=%v, want gen=2 (recycled)", gen, err)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2", got)
	}
	if got := m.metrics.StreamingRecycles.Load(); got != 1 {
		t.Errorf("streaming_recycles = %d, want 1", got)
	}
}

// Token-only traffic does not recycle a session that passed its streaming
// deadline.
func TestMinterPOTOnlyTrafficDoesNotRecycle(t *testing.T) {
	m, launches, _, _ := newStreamingMinter(time.Hour, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	m.mu.Lock()
	m.streamingDeadline = time.Now().Add(-time.Hour) // long past the window
	m.mu.Unlock()
	for i := 0; i < 5; i++ {
		if _, _, err := m.Mint(ctx, "gvs", fmt.Sprintf("vd%d", i)); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (POT-only traffic must not recycle)", got)
	}
	if got := m.metrics.StreamingRecycles.Load(); got != 0 {
		t.Errorf("streaming_recycles = %d, want 0", got)
	}
}

// A zero streaming age limit disables time-based recycling.
func TestMinterStreamingMaxAgeZeroNeverRecycles(t *testing.T) {
	m, launches, _, _ := newStreamingMinter(0, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	m.mu.Lock()
	zero := m.streamingDeadline.IsZero()
	m.mu.Unlock()
	if !zero {
		t.Error("streamingDeadline should be zero when streamingMaxAge=0")
	}
	for i := 0; i < 3; i++ {
		if _, gen, err := m.PlayerContext(ctx, "vid"); err != nil || gen != 1 {
			t.Fatalf("player-context %d: gen=%d err=%v, want gen=1 (no staleness recycle)", i, gen, err)
		}
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1", got)
	}
	if got := m.metrics.StreamingRecycles.Load(); got != 0 {
		t.Errorf("streaming_recycles = %d, want 0", got)
	}
}

// A report on the current generation, with no browser operation in progress,
// retires the session immediately and recovers on the next request.
func TestMinterReportDegradedImmediateRetire(t *testing.T) {
	m, launches, sessions, smu := newStreamingMinter(0, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	res := m.ReportDegraded(1, "vid", "incomplete-stream")
	if !res.Accepted || !res.Retired || res.Generation != 1 {
		t.Fatalf("ReportDegraded = %+v, want Accepted && Retired, Generation 1", res)
	}
	if got := m.metrics.DegradationReportsAccepted.Load(); got != 1 {
		t.Errorf("degradation_reports_accepted = %d, want 1", got)
	}
	if got := m.metrics.ReportDrivenRecycles.Load(); got != 1 {
		t.Errorf("report_driven_recycles = %d, want 1", got)
	}
	if got := m.metrics.StreamingRecycles.Load(); got != 0 {
		t.Errorf("streaming_recycles = %d, want 0 (a report is not a staleness recycle)", got)
	}
	if got := m.metrics.Crashes.Load(); got != 0 {
		t.Errorf("crashes = %d, want 0 (a consumer report is an intentional recycle)", got)
	}
	smu.Lock()
	if !(*sessions)[0].closed.Load() {
		smu.Unlock()
		t.Fatal("reported session should be retired")
	}
	smu.Unlock()
	// Retirement does not bump the generation; the next request does.
	if _, gen, err := m.PlayerContext(ctx, "vid"); err != nil || gen != 2 {
		t.Errorf("recovery: gen=%d err=%v, want gen=2", gen, err)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2", got)
	}
}

// A report naming an old generation is rejected as stale.
func TestMinterReportDegradedStaleGen(t *testing.T) {
	m, _, sessions, smu := newStreamingMinter(0, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	res := m.ReportDegraded(99, "vid", "cap")
	if res.Accepted || res.Generation != 1 {
		t.Fatalf("stale report = %+v, want !Accepted, Generation 1", res)
	}
	if got := m.metrics.DegradationReportsRejectedStale.Load(); got != 1 {
		t.Errorf("degradation_reports_rejected_stale = %d, want 1", got)
	}
	if got := m.metrics.DegradationReportsAccepted.Load(); got != 0 {
		t.Errorf("degradation_reports_accepted = %d, want 0", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Error("a stale report must not retire the live session")
	}
}

// A report for the current generation whose session was already retired is a
// benign no-op counted as already_retired, distinct from a stale-generation
// report. retire() clears the suspect mark, so the re-report lands in the
// no-live-session case rather than the pending or debounce branches.
func TestMinterReportDegradedAlreadyRetired(t *testing.T) {
	m, _, _, _ := newStreamingMinter(0, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	if res := m.ReportDegraded(1, "vid", "cap"); !res.Retired { // retires gen 1; sess→nil, gen stays 1
		t.Fatalf("first report = %+v, want Retired", res)
	}
	// Drain the report budget so the rate-limit predicate is also true for the
	// re-report, which is what makes the case ordering below observable.
	m.mu.Lock()
	m.reportTokens = 0
	m.mu.Unlock()
	// Report gen 1 again before any request relaunches the session.
	res := m.ReportDegraded(1, "vid", "cap")
	if res.Accepted || res.Generation != 1 {
		t.Fatalf("second report = %+v, want !Accepted, Generation 1", res)
	}
	if got := m.metrics.DegradationReportsAlreadyRetired.Load(); got != 1 {
		t.Errorf("degradation_reports_already_retired = %d, want 1", got)
	}
	if got := m.metrics.DegradationReportsRejectedStale.Load(); got != 0 {
		t.Errorf("degradation_reports_rejected_stale = %d, want 0 (current gen, not stale)", got)
	}
	// The sess==nil case must precede the rate-limit case: with the budget drained
	// above, this re-report also satisfies the rate-limit predicate. A miscount
	// here (rate_limited == 1) would mean the switch was reordered.
	if got := m.metrics.DegradationReportsRateLimited.Load(); got != 0 {
		t.Errorf("degradation_reports_rate_limited = %d, want 0 (already-retired must not route to debounce)", got)
	}
}

// Report-driven recycles allow a burst of ReportBurst before rate-limiting, so
// a consumer whose bulk-enumeration throttle escape rotates identities several
// times in one run is never declined mid-sequence. The report after the burst
// is rate-limited with a positive retry hint.
func TestMinterReportBurstThenRateLimited(t *testing.T) {
	m, _, _, _ := newStreamingMinter(0, func(string) (browser.PlayerContext, error) {
		return browser.PlayerContext{ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
	})
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	for gen := uint64(1); gen <= ReportBurst; gen++ {
		if res := m.ReportDegraded(gen, "vid", "cap"); !res.Retired {
			t.Fatalf("report %d = %+v, want Retired (within the burst allowance)", gen, res)
		}
		if _, g, err := m.PlayerContext(ctx, "vid"); err != nil || g != gen+1 { // relaunch
			t.Fatalf("relaunch %d: gen=%d err=%v, want gen=%d", gen, g, err, gen+1)
		}
	}
	res := m.ReportDegraded(ReportBurst+1, "vid", "cap") // budget spent
	if res.Accepted || res.RetryAfterSeconds <= 0 {
		t.Fatalf("report past the burst = %+v, want !Accepted and RetryAfterSeconds>0", res)
	}
	if got := m.metrics.DegradationReportsRateLimited.Load(); got != 1 {
		t.Errorf("degradation_reports_rate_limited = %d, want 1", got)
	}
	if got := m.metrics.ReportDrivenRecycles.Load(); got != ReportBurst {
		t.Errorf("report_driven_recycles = %d, want %d", got, ReportBurst)
	}
}

// startBlockingPlayerContext starts a PlayerContext call that holds mintMu until
// release is called.
func startBlockingPlayerContext(t *testing.T) (m *Minter, sessions *[]*fakeSession, smu *sync.Mutex, release func(), done <-chan struct{}) {
	t.Helper()
	var pcCount int64
	entered := make(chan struct{})
	rel := make(chan struct{})
	pcFn := func(string) (browser.PlayerContext, error) {
		if atomic.AddInt64(&pcCount, 1) == 1 {
			close(entered)
			<-rel // hold mintMu open
		}
		return browser.PlayerContext{ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
	}
	m, _, sessions, smu = newStreamingMinter(0, pcFn)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	d := make(chan struct{})
	go func() { defer close(d); _, _, _ = m.PlayerContext(ctx, "vid") }()
	<-entered // the blocked call now holds mintMu
	return m, sessions, smu, func() { close(rel) }, d
}

// A report waits for an in-flight browser operation before retiring the session.
func TestMinterReportRetirementPendingThenConsumed(t *testing.T) {
	m, sessions, smu, release, done := startBlockingPlayerContext(t)

	res := m.ReportDegraded(1, "vid", "cap")
	if !res.Accepted || !res.RetirementPending || res.Retired {
		t.Fatalf("report while busy = %+v, want Accepted && RetirementPending && !Retired", res)
	}
	smu.Lock()
	if (*sessions)[0].closed.Load() {
		smu.Unlock()
		t.Fatal("session closed while PlayerContext held mintMu")
	}
	smu.Unlock()

	release()
	<-done // the in-flight gen-1 call returns its context

	_, gen, err := m.PlayerContext(context.Background(), "vid")
	if err != nil {
		t.Fatalf("post-report player-context: %v", err)
	}
	if gen != 2 {
		t.Errorf("gen = %d, want 2 (suspect consumed by the next handoff)", gen)
	}
	if got := m.metrics.ReportDrivenRecycles.Load(); got != 1 {
		t.Errorf("report_driven_recycles = %d, want 1", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Error("gen-1 session should be retired once the suspect is consumed")
	}
}

// A duplicate pending report does not increment the accepted counter.
func TestMinterReportDuplicateWhilePending(t *testing.T) {
	m, _, _, release, done := startBlockingPlayerContext(t)
	defer func() { release(); <-done }()

	if res := m.ReportDegraded(1, "vid", "cap"); !res.RetirementPending {
		t.Fatalf("first report = %+v, want RetirementPending", res)
	}
	acc1 := m.metrics.DegradationReportsAccepted.Load()
	res := m.ReportDegraded(1, "vid", "cap") // duplicate while pending
	if !res.Accepted || !res.RetirementPending {
		t.Fatalf("duplicate report = %+v, want Accepted && RetirementPending", res)
	}
	if acc2 := m.metrics.DegradationReportsAccepted.Load(); acc2 != acc1 {
		t.Errorf("degradation_reports_accepted bumped on a duplicate-while-pending: %d -> %d", acc1, acc2)
	}
}

// A deferred report-driven retirement spends report budget just like an
// immediate one. With a single token left, the handoff's deferred retire
// consumes it, so the next report is rate-limited.
func TestMinterReportDeferredConsumesBudget(t *testing.T) {
	m, _, _, release, done := startBlockingPlayerContext(t)

	m.mu.Lock()
	m.reportTokens = 1 // the deferred retire below spends the last token
	m.mu.Unlock()

	if res := m.ReportDegraded(1, "vid", "cap"); !res.RetirementPending {
		t.Fatalf("report while busy = %+v, want RetirementPending", res)
	}
	release()
	<-done // the in-flight gen-1 call returns

	// The next handoff retires generation 1 and launches generation 2.
	if _, gen, err := m.PlayerContext(context.Background(), "vid"); err != nil || gen != 2 {
		t.Fatalf("post-report player-context: gen=%d err=%v, want gen=2", gen, err)
	}
	res := m.ReportDegraded(2, "vid", "cap")
	if res.Accepted || res.RetryAfterSeconds <= 0 {
		t.Fatalf("report after a deferred retire = %+v, want rate limited (the deferred path must spend report budget)", res)
	}
	if got := m.metrics.DegradationReportsRateLimited.Load(); got != 1 {
		t.Errorf("degradation_reports_rate_limited = %d, want 1", got)
	}
}

// Concurrent duplicate reports retire the generation at most once.
func TestMinterReportConcurrentAtMostOnce(t *testing.T) {
	m, _, sessions, smu := newStreamingMinter(0, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	var wg sync.WaitGroup
	var retired int64
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.ReportDegraded(1, "vid", "cap").Retired {
				atomic.AddInt64(&retired, 1)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&retired); got != 1 {
		t.Errorf("Retired=true count = %d, want exactly 1 (gen-guarded retire)", got)
	}
	if got := m.metrics.ReportDrivenRecycles.Load(); got != 1 {
		t.Errorf("report_driven_recycles = %d, want 1", got)
	}
	// Concurrent duplicates count as one accepted report.
	if got := m.metrics.DegradationReportsAccepted.Load(); got != 1 {
		t.Errorf("degradation_reports_accepted = %d, want 1 after concurrent duplicate reports", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Error("the reported session should be retired exactly once")
	}
}

// A report that loses the retire race to another goroutine must not spend
// report budget. A token is consumed only when this report actually recycled
// the session. Running several rounds makes the race show up without relying on
// timing.
func TestMinterReportNoopRetireDoesNotSpendBudget(t *testing.T) {
	for round := 0; round < 200; round++ {
		m, _, _, _ := newStreamingMinter(0, nil)
		if err := m.Warm(context.Background()); err != nil { // gen 1
			t.Fatalf("round %d warm: %v", round, err)
		}
		var res ReportResult
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); res = m.ReportDegraded(1, "vid", "cap") }()
		go func() { defer wg.Done(); m.retire(1, "concurrent retirement", false) }()
		wg.Wait()

		m.mu.Lock()
		spent := m.reportTokens < ReportBurst
		m.mu.Unlock()
		// Budget is spent only when this report's own retire succeeded. Refill
		// cannot mask a spend here: it tops out at ReportBurst, and a consumed
		// token cannot re-accrue within a test round under the 5m default.
		if spent != res.Retired {
			t.Fatalf("round %d: budget spent=%v but report Retired=%v (a no-op retire must not spend report budget)", round, spent, res.Retired)
		}
	}
}

// Close clears the suspect mark so it never outlives the disposed generation.
func TestMinterCloseClearsSuspect(t *testing.T) {
	m, _, _, _ := newStreamingMinter(0, nil)
	if err := m.Warm(context.Background()); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	m.mu.Lock()
	m.reportSuspectGen = 1
	m.reportSuspectVideoID = "vid"
	m.mu.Unlock()

	m.Close()
	m.mu.Lock()
	gen, vid := m.reportSuspectGen, m.reportSuspectVideoID
	m.mu.Unlock()
	if gen != 0 || vid != "" {
		t.Errorf("suspect after Close = (gen=%d, vid=%q), want cleared", gen, vid)
	}
}

// retire clears the suspect mark regardless of the retirement cause.
func TestMinterRetireClearsSuspect(t *testing.T) {
	m, _, _, _ := newStreamingMinter(0, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	m.mu.Lock()
	m.reportSuspectGen = 1
	m.reportSuspectVideoID = "vid"
	m.mu.Unlock()

	if !m.retire(1, "browser target crashed", true) {
		t.Fatal("retire(1) = false, want true for the current generation")
	}
	m.mu.Lock()
	gen, vid := m.reportSuspectGen, m.reportSuspectVideoID
	m.mu.Unlock()
	if gen != 0 || vid != "" {
		t.Errorf("suspect after retire = (gen=%d, vid=%q), want cleared", gen, vid)
	}
	// A retire of a non-current generation is a no-op returning false.
	if m.retire(99, "stale", false) {
		t.Error("retire(99) = true, want false for a non-current generation")
	}
}

// PlayerContext returns the generation that produced its context.
func TestMinterGenerationIdentifiesContext(t *testing.T) {
	var n int64
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		k := atomic.AddInt64(&n, 1)
		return &fakeSession{
			id:   browser.Identity{VisitorData: fmt.Sprintf("vd-%d", k)},
			mint: okMint,
			playerCtx: func(string) (browser.PlayerContext, error) {
				return browser.PlayerContext{ServerAbrStreamingURL: fmt.Sprintf("https://r/%d", k), VisitorData: fmt.Sprintf("vd-%d", k)}, nil
			},
		}, nil
	}
	ctx := context.Background()
	pc, gen, err := m.PlayerContext(ctx, "vid")
	if err != nil || gen != 1 || pc.ServerAbrStreamingURL != "https://r/1" {
		t.Fatalf("first: gen=%d url=%q err=%v, want gen=1 url=https://r/1", gen, pc.ServerAbrStreamingURL, err)
	}
	if res := m.ReportDegraded(1, "vid", "cap"); !res.Retired { // force a recycle
		t.Fatalf("report = %+v, want Retired", res)
	}
	pc, gen, err = m.PlayerContext(ctx, "vid")
	if err != nil || gen != 2 || pc.ServerAbrStreamingURL != "https://r/2" {
		t.Fatalf("second: gen=%d url=%q err=%v, want gen=2 url=https://r/2 (gen must match the producing session)", gen, pc.ServerAbrStreamingURL, err)
	}
}

// Health does not combine fields from different generations.
func TestMinterHealthSnapshotSingleGeneration(t *testing.T) {
	var n int64
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		k := atomic.AddInt64(&n, 1)
		return &fakeSession{
			id:          browser.Identity{VisitorData: fmt.Sprintf("vd-%d", k)},
			mint:        okMint,
			established: k == 1, // only generation 1 is established
			lastProbe:   browser.FullLengthProbe{Outcome: browser.OutcomeFullLength},
			lastProbeAt: time.Now(),
		}, nil
	}
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	snap, live, err := m.Health(ctx)
	if err != nil || !live || snap.Generation != 1 || !snap.BrowserProofEstablished {
		t.Fatalf("gen-1 snapshot = %+v (live=%v err=%v), want Generation 1 established", snap, live, err)
	}
	if !m.retire(1, "recycle", false) {
		t.Fatal("retire(1) failed")
	}
	if err := m.Warm(ctx); err != nil { // gen 2 (not established)
		t.Fatalf("warm 2: %v", err)
	}
	snap, _, err = m.Health(ctx)
	if err != nil {
		t.Fatalf("gen-2 health: %v", err)
	}
	if snap.Generation != 2 || snap.BrowserProofEstablished {
		t.Errorf("gen-2 snapshot = %+v, want Generation 2 and BrowserProofEstablished false (no mixing)", snap)
	}
	if snap.LastBrowserProofOutcome != browser.OutcomeFullLength {
		t.Errorf("last proof outcome = %q, want %q", snap.LastBrowserProofOutcome, browser.OutcomeFullLength)
	}
}

// MetricsSnapshot keeps the proof-detail fields present (with ""/null sentinels)
// before a probe runs and carries the suspect video only while a report is
// outstanding.
func TestMinterMetricsSnapshotStreamingFields(t *testing.T) {
	m, _, _, _ := newStreamingMinter(time.Hour, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1, never probed
		t.Fatalf("warm: %v", err)
	}
	snap := m.MetricsSnapshot()
	// Never probed: the proof fields are present with sentinel values, not absent.
	if v, ok := snap["last_browser_proof_outcome"]; !ok || v != "" {
		t.Errorf("last_browser_proof_outcome = %v (present=%v), want \"\" present", v, ok)
	}
	if v, ok := snap["last_browser_proof_age_secs"]; !ok || v != nil {
		t.Errorf("last_browser_proof_age_secs = %v (present=%v), want null present", v, ok)
	}
	if v, ok := snap["browser_proof_established"]; !ok || v != false {
		t.Errorf("browser_proof_established = %v (present=%v), want false present", v, ok)
	}
	if _, ok := snap["streaming_seconds_until_recycle"]; !ok {
		t.Error("streaming_seconds_until_recycle missing though a window is set")
	}
	if v := snap["streaming_suspect"]; v != false {
		t.Errorf("streaming_suspect = %v, want false", v)
	}
	// Present even with no outstanding report; the value is "" until one arrives.
	if v, ok := snap["streaming_suspect_video"]; !ok || v != "" {
		t.Errorf("streaming_suspect_video = %v (present=%v), want \"\" present", v, ok)
	}
	for _, k := range []string{"streaming_recycles", "report_driven_recycles", "degradation_reports_accepted", "degradation_reports_rejected_stale", "degradation_reports_already_retired", "degradation_reports_rate_limited"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("counter %q missing from the metrics snapshot", k)
		}
	}

	// An outstanding report includes its video ID.
	m.mu.Lock()
	m.reportSuspectGen = m.gen
	m.reportSuspectVideoID = "vidX"
	m.mu.Unlock()
	snap = m.MetricsSnapshot()
	if snap["streaming_suspect"] != true {
		t.Errorf("streaming_suspect = %v, want true", snap["streaming_suspect"])
	}
	if snap["streaming_suspect_video"] != "vidX" {
		t.Errorf("streaming_suspect_video = %v, want vidX", snap["streaming_suspect_video"])
	}
}

// MetricsSnapshot omits the recycle deadline when time-based recycling is
// disabled.
func TestMinterMetricsSnapshotOmitsRecycleWhenDisabled(t *testing.T) {
	m, _, _, _ := newStreamingMinter(0, nil)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, ok := m.MetricsSnapshot()["streaming_seconds_until_recycle"]; ok {
		t.Error("streaming_seconds_until_recycle present though recycling is disabled")
	}
}

func TestMinterMetricsSnapshotStableWhenNotLive(t *testing.T) {
	m, _, _, _ := newStreamingMinter(time.Hour, nil)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if !m.retire(1, "test", false) {
		t.Fatal("retire(1) returned false, want true")
	}
	snap := m.MetricsSnapshot()
	if snap["session_live"] != false {
		t.Fatalf("session_live = %v, want false", snap["session_live"])
	}
	if snap["attest_kind"] != "" {
		t.Errorf("attest_kind = %v, want empty when not live", snap["attest_kind"])
	}
	for _, k := range []string{"browser_proof_established", "streaming_suspect"} {
		v, present := snap[k]
		if !present {
			t.Errorf("%q absent when not live, want present (false)", k)
		} else if v != false {
			t.Errorf("%q = %v, want false", k, v)
		}
	}
	// Detail fields stay present after retirement. streamingMaxAge is enabled
	// here, so the recycle field is present as null when no session is live.
	wantSentinel := map[string]any{
		"last_browser_proof_outcome":      "",
		"last_browser_proof_age_secs":     nil,
		"streaming_suspect_video":         "",
		"streaming_seconds_until_recycle": nil,
	}
	for k, want := range wantSentinel {
		v, present := snap[k]
		if !present {
			t.Errorf("%q absent when not live, want present (%v) for a stable schema", k, want)
		} else if v != want {
			t.Errorf("%q = %v, want %v when not live", k, v, want)
		}
	}
}

// TestMetricsSnapshotNullEncoding verifies that not-applicable numeric fields
// marshal to JSON null in the never-probed, not-live state. They must not be
// omitted or encoded as 0, which means "just proved" for proof age.
func TestMetricsSnapshotNullEncoding(t *testing.T) {
	m, _, _, _ := newStreamingMinter(time.Hour, nil) // recycling enabled
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if !m.retire(1, "test", false) { // not live, never probed
		t.Fatal("retire(1) returned false, want true")
	}
	raw, err := json.Marshal(m.MetricsSnapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"last_browser_proof_age_secs", "streaming_seconds_until_recycle"} {
		v, ok := fields[k]
		if !ok {
			t.Errorf("%q absent, want JSON null", k)
			continue
		}
		if string(v) != "null" {
			t.Errorf("%q = %s, want null rather than zero or omission", k, v)
		}
	}
}

// TestCounterKeysAligned keeps counterValues and lifetimeCounterKeys in sync.
// Per-tenant metrics and the redacted aggregate both rely on that shared key set.
func TestCounterKeysAligned(t *testing.T) {
	m := NewMinter("v", browser.Options{}, 0, 0, 0)
	cv := m.counterValues()
	if len(cv) != len(lifetimeCounterKeys) {
		t.Errorf("counterValues has %d keys, lifetimeCounterKeys has %d", len(cv), len(lifetimeCounterKeys))
	}
	want := make(map[string]bool, len(lifetimeCounterKeys))
	for _, k := range lifetimeCounterKeys {
		want[k] = true
		if _, ok := cv[k]; !ok {
			t.Errorf("lifetimeCounterKeys has %q but counterValues does not", k)
		}
	}
	for k := range cv {
		if !want[k] {
			t.Errorf("counterValues has %q but lifetimeCounterKeys does not", k)
		}
	}
}

// jitter stays within 10 percent and leaves non-positive inputs disabled.
func TestJitterWithinBounds(t *testing.T) {
	d := time.Hour // a var, so the bound conversions truncate at runtime
	lo, hi := time.Duration(0.9*float64(d)), time.Duration(1.1*float64(d))
	for i := 0; i < 2000; i++ {
		j := jitter(d)
		if j < lo || j > hi {
			t.Fatalf("jitter(%v) = %v, out of [%v, %v]", d, j, lo, hi)
		}
	}
	if jitter(0) != 0 {
		t.Errorf("jitter(0) = %v, want 0", jitter(0))
	}
	if jitter(-time.Second) != 0 {
		t.Errorf("jitter(negative) = %v, want 0", jitter(-time.Second))
	}
}

// Concurrent reports, streaming handoffs, and health reads are race-free.
func TestMinterConcurrentReportPlayerContextHealth(t *testing.T) {
	m, _, _, _ := newStreamingMinter(20*time.Millisecond, func(string) (browser.PlayerContext, error) {
		return browser.PlayerContext{ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
	})
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	// A shared deadline stops every worker without coordinating channel receives.
	deadline := time.Now().Add(150 * time.Millisecond)
	var wg sync.WaitGroup
	worker := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				fn()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		worker(func() { _, _, _ = m.PlayerContext(ctx, "vid") })
	}
	for i := 0; i < 2; i++ {
		worker(func() { m.ReportDegraded(m.Generation(), "vid", "cap") })
	}
	for i := 0; i < 2; i++ {
		worker(func() { _, _, _ = m.Health(ctx); m.MetricsSnapshot() })
	}
	wg.Wait()
}

// The max-age recycle clears the suspect mark even though it bypasses retire.
func TestMinterMaxAgeRecycleClearsSuspect(t *testing.T) {
	m, launches, _, _ := newStreamingMinter(0, nil)
	m.maxAge = time.Millisecond
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	m.mu.Lock()
	m.reportSuspectGen = 1
	m.reportSuspectVideoID = "vid"
	m.mu.Unlock()

	time.Sleep(5 * time.Millisecond)    // exceed maxAge
	if err := m.Warm(ctx); err != nil { // max-age recycle -> gen 2
		t.Fatalf("warm 2: %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Fatalf("launches = %d, want 2 (max-age recycle)", got)
	}
	m.mu.Lock()
	gen, vid := m.reportSuspectGen, m.reportSuspectVideoID
	m.mu.Unlock()
	if gen != 0 || vid != "" {
		t.Errorf("suspect after max-age recycle = (gen=%d, vid=%q), want cleared", gen, vid)
	}
}

// An overdue recycle deadline is reported as zero rather than a negative value.
func TestMinterMetricsRecycleSecondsClampedAtZero(t *testing.T) {
	m, _, _, _ := newStreamingMinter(time.Hour, nil)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	m.mu.Lock()
	m.streamingDeadline = time.Now().Add(-time.Minute) // overdue
	m.mu.Unlock()
	v, ok := m.MetricsSnapshot()["streaming_seconds_until_recycle"]
	if !ok {
		t.Fatal("streaming_seconds_until_recycle missing")
	}
	if secs := v.(int); secs != 0 {
		t.Errorf("streaming_seconds_until_recycle = %d, want 0 (clamped, not negative)", secs)
	}
}

// ReportDebounce controls how fast spent report budget refills: one token per
// window, so with the budget drained a report is accepted only once a full
// window has passed.
func TestMinterReportDebounceConfigurable(t *testing.T) {
	m := newBareMinter(0, 250*time.Millisecond)
	m.launch = func(context.Context) (minterSession, error) { return &fakeSession{mint: okMint}, nil }
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	// 200ms of refill on a drained budget is 0.8 tokens under a 250ms window.
	m.mu.Lock()
	m.reportTokens = 0
	m.reportRefillAt = time.Now().Add(-200 * time.Millisecond)
	m.mu.Unlock()
	if res := m.ReportDegraded(1, "vid", "cap"); res.Accepted {
		t.Fatalf("report 200ms after draining the budget = %+v, want rate limited under a 250ms debounce", res)
	}
	// 300ms of refill is a full token.
	m.mu.Lock()
	m.reportTokens = 0
	m.reportRefillAt = time.Now().Add(-300 * time.Millisecond)
	m.mu.Unlock()
	if res := m.ReportDegraded(1, "vid", "cap"); !res.Accepted {
		t.Fatalf("report 300ms after draining the budget = %+v, want accepted under a 250ms debounce", res)
	}
}

// NewMinter falls back to the default debounce when given a non-positive window.
func TestMinterReportDebounceDefaultsWhenUnset(t *testing.T) {
	if m := NewMinter("v", browser.Options{}, 0, 0, 0); m.reportDebounce != DefaultReportDebounce {
		t.Errorf("reportDebounce = %v, want default %v", m.reportDebounce, DefaultReportDebounce)
	}
	if m := NewMinter("v", browser.Options{}, 0, -time.Second, 0); m.reportDebounce != DefaultReportDebounce {
		t.Errorf("reportDebounce (negative) = %v, want default %v", m.reportDebounce, DefaultReportDebounce)
	}
	if m := NewMinter("v", browser.Options{}, 0, 90*time.Second, 0); m.reportDebounce != 90*time.Second {
		t.Errorf("reportDebounce = %v, want 90s", m.reportDebounce)
	}
}

// TestMinterMintCancelNoEscalationNoRetire covers cancellation during Mint. The
// guard returns ctx.Err() before updating failure metrics, warning, retiring the
// session, or relaunching. The fake session fails unconditionally and ignores its
// ctx, so the pre-canceled context is the only signal the guard can use.
func TestMinterMintCancelNoEscalationNoRetire(t *testing.T) {
	m, launches, sessions, smu := newTestMinter(func(string) (browser.MintResult, error) {
		return browser.MintResult{}, errors.New("mint always fails")
	})
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	// The attestation pre-mint fails against this always-failing fake, so the
	// canceled request is measured against what Warm left behind, not against zero.
	afterWarm := m.metrics.MintFailures.Load()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has gone away

	_, _, err := m.Mint(ctx, "gvs", "vd")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Mint = %v, want context.Canceled", err)
	}
	if got := m.metrics.MintFailures.Load(); got != afterWarm {
		t.Errorf("mint_failures = %d, want %d (a canceled caller is not a mint failure)", got, afterWarm)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0 (no relaunch on cancel)", got)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (no relaunch)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Error("a canceled mint must not retire/relaunch the session")
	}
}

// The attestation mints the visitor's GVS token before publishing the session,
// so the token a consumer streams with is already old when the first context is
// established. PlayerContext is what launches here because it never mints on its
// own, so every mint the fake records comes from the pre-mint.
// TestEnsurePremintsGVSToken checks the attestation pre-mint under both
// launchers a cold request can take: a player-context call, and a /get_pot mint
// whose own binding happens to be the pre-minted one. Either way, exactly one
// fake mint runs, and both the gvs and pot scopes hit it.
func TestEnsurePremintsGVSToken(t *testing.T) {
	newPremintMinter := func() (m *Minter, launches, mints *int64, bindings *[]string, bmu *sync.Mutex) {
		mints = new(int64)
		bindings = new([]string)
		bmu = new(sync.Mutex)
		m, launches, _, _ = newTestMinterFull(func(id string) (browser.MintResult, error) {
			atomic.AddInt64(mints, 1)
			bmu.Lock()
			*bindings = append(*bindings, id)
			bmu.Unlock()
			return browser.MintResult{Kind: "integrity", Token: "pre-" + id, TokenLen: 6, Identifier: id, Lifetime: 3600}, nil
		}, nil)
		return m, launches, mints, bindings, bmu
	}

	checkPremint := func(t *testing.T, m *Minter, launches, mints *int64, bindings *[]string, bmu *sync.Mutex) {
		t.Helper()
		if got := atomic.LoadInt64(launches); got != 1 {
			t.Fatalf("launches = %d, want 1", got)
		}
		if got := atomic.LoadInt64(mints); got != 1 {
			t.Fatalf("fake mints = %d, want 1 (the pre-mint at attestation)", got)
		}
		bmu.Lock()
		gotBinding := (*bindings)[0]
		bmu.Unlock()
		if gotBinding != "vd" {
			t.Errorf("pre-mint bound to %q, want the session visitor_data %q", gotBinding, "vd")
		}
		if r, ok := m.cacheGet(cacheKey("gvs", "vd")); !ok || r.Token != "pre-vd" {
			t.Errorf("cache[gvs|vd] = (%q, ok=%v), want the pre-minted token", r.Token, ok)
		}
		// The pre-mint also fills the default (pot) scope: scope only namespaces
		// the cache, and the token is identical either way.
		if r, ok := m.cacheGet(cacheKey("pot", "vd")); !ok || r.Token != "pre-vd" {
			t.Errorf("cache[pot|vd] = (%q, ok=%v), want the pre-minted token", r.Token, ok)
		}
		m.mu.Lock()
		lastMint := m.lastMintAt
		m.mu.Unlock()
		if lastMint.IsZero() {
			t.Error("lastMintAt is zero after a successful pre-mint")
		}
		if got := m.metrics.Mints.Load(); got != 1 {
			t.Errorf("mints metric = %d, want 1 (the pre-mint counts like any mint)", got)
		}
	}

	t.Run("player-context launcher", func(t *testing.T) {
		m, launches, mints, bindings, bmu := newPremintMinter()
		ctx := context.Background()
		if _, _, err := m.PlayerContext(ctx, "vid"); err != nil {
			t.Fatalf("player-context: %v", err)
		}
		checkPremint(t, m, launches, mints, bindings, bmu)
		// The consumer's own token request is then served from the pre-minted entry.
		if _, cached, err := m.Mint(ctx, "gvs", "vd"); err != nil || !cached {
			t.Errorf("gvs/vd after the pre-mint: cached=%v err=%v, want cached=true", cached, err)
		}
		if got := atomic.LoadInt64(mints); got != 1 {
			t.Errorf("fake mints = %d, want 1 (the consumer request hit the pre-minted entry)", got)
		}
	})

	t.Run("mint launcher", func(t *testing.T) {
		m, launches, mints, bindings, bmu := newPremintMinter()
		ctx := context.Background()
		// A cold /get_pot for the session's own visitor_data is what triggers the
		// launch here. Mint re-checks the cache after ensure returns, so it must
		// serve the pre-mint's entry rather than minting a second, redundant token
		// and overwriting it.
		r, cached, err := m.Mint(ctx, "gvs", "vd")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !cached {
			t.Error("cached = false, want true: the pre-mint should have filled this entry before Mint minted again")
		}
		if r.Token != "pre-vd" {
			t.Errorf("token = %q, want the pre-minted token pre-vd", r.Token)
		}
		checkPremint(t, m, launches, mints, bindings, bmu)
	})
}

// A failed pre-mint still publishes the session: the next token request mints on
// demand.
func TestEnsurePremintFailureStillPublishes(t *testing.T) {
	var attempt int64
	m, launches, _, _ := newTestMinterFull(func(string) (browser.MintResult, error) {
		if atomic.AddInt64(&attempt, 1) == 1 {
			return browser.MintResult{}, errors.New("pre-mint failed")
		}
		return browser.MintResult{Kind: "integrity", Token: "later", Lifetime: 3600}, nil
	}, nil)
	ctx := context.Background()

	if _, _, err := m.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("player-context after a failed pre-mint: %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (a failed pre-mint must not fail the launch)", got)
	}
	if got := m.metrics.MintFailures.Load(); got != 1 {
		t.Errorf("mint_failures = %d, want 1 (the pre-mint attempt)", got)
	}
	m.mu.Lock()
	lastMint := m.lastMintAt
	m.mu.Unlock()
	if !lastMint.IsZero() {
		t.Error("lastMintAt is set after a failed pre-mint, want zero")
	}
	if r, cached, err := m.Mint(ctx, "gvs", "vd"); err != nil || cached || r.Token != "later" {
		t.Errorf("gvs/vd after a failed pre-mint = (%q, cached=%v, %v), want a fresh mint", r.Token, cached, err)
	}

	// SelfTest's own fallback mint, taken when a failed pre-mint left the gvs
	// entry missing, caches under both scopes too, matching the pre-mint's dual
	// write. A fresh minter here isolates the scenario: SelfTest itself must be
	// what triggers the launch, or the gvs entry would already be filled by the
	// time SelfTest checks for it.
	var attempt2 int64
	m2, _, _, _ := newTestMinterFull(func(string) (browser.MintResult, error) {
		if atomic.AddInt64(&attempt2, 1) == 1 {
			return browser.MintResult{}, errors.New("pre-mint failed")
		}
		return browser.MintResult{Kind: "integrity", Token: "later", Lifetime: 3600}, nil
	}, nil)
	if err := m2.SelfTest(ctx); err != nil {
		t.Fatalf("self-test after a failed pre-mint: %v", err)
	}
	if r, ok := m2.cacheGet(cacheKey("gvs", "vd")); !ok || r.Token != "later" {
		t.Errorf("cache[gvs|vd] after self-test's fallback mint = (%q, ok=%v), want the fallback-minted token", r.Token, ok)
	}
	if r, ok := m2.cacheGet(cacheKey("pot", "vd")); !ok || r.Token != "later" {
		t.Errorf("cache[pot|vd] after self-test's fallback mint = (%q, ok=%v), want the fallback-minted token", r.Token, ok)
	}
}

// A cache miss is counted only for a request that actually pays for an in-page
// mint. A request served by the pre-mint counts as a hit only, and a request
// whose launch fails is counted by launch_failures alone, not also as a miss.
func TestMintCacheMissCountedOnlyOnGenuineMiss(t *testing.T) {
	// Served by the pre-mint: no miss, one hit.
	m, _, _, _ := newTestMinter(okMint)
	if _, cached, err := m.Mint(context.Background(), "gvs", "vd"); err != nil || !cached {
		t.Fatalf("mint: cached=%v err=%v, want cached=true (served by the pre-mint)", cached, err)
	}
	if got := m.metrics.CacheMisses.Load(); got != 0 {
		t.Errorf("cache_misses = %d, want 0 (the pre-mint served the request)", got)
	}
	if got := m.metrics.CacheHits.Load(); got != 1 {
		t.Errorf("cache_hits = %d, want 1", got)
	}

	// A failed launch: no miss, one launch failure.
	failing := newBareMinter(0, 0)
	failing.launch = func(context.Context) (minterSession, error) {
		return nil, errors.New("launch failed")
	}
	if _, _, err := failing.Mint(context.Background(), "gvs", "vd"); err == nil {
		t.Fatal("mint returned a nil error, want the launch failure")
	}
	if got := failing.metrics.CacheMisses.Load(); got != 0 {
		t.Errorf("cache_misses = %d, want 0 (the launch failed before any mint was attempted)", got)
	}
	if got := failing.metrics.LaunchFailures.Load(); got != 1 {
		t.Errorf("launch_failures = %d, want 1", got)
	}

	// A genuine miss: the pre-mint's binding differs from the request's, so the
	// request pays for its own in-page mint and this is the only case that counts
	// a cache miss.
	m2, _, _, _ := newTestMinter(okMint)
	if _, cached, err := m2.Mint(context.Background(), "player", "vid2"); err != nil || cached {
		t.Fatalf("mint: cached=%v err=%v, want a fresh mint", cached, err)
	}
	if got := m2.metrics.CacheMisses.Load(); got != 1 {
		t.Errorf("cache_misses = %d, want 1", got)
	}
}

// A context establishment waits out the window since the last in-page mint, and
// a mint older than the window does not hold it back.
func TestPlayerContextWaitsForMintSeparation(t *testing.T) {
	ctx := context.Background()

	held, _, _, _ := newTestMinterFull(okMint, nil)
	if err := held.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	held.mintSeparation = 50 * time.Millisecond
	held.mu.Lock()
	held.lastMintAt = time.Now()
	held.mu.Unlock()

	start := time.Now()
	if _, _, err := held.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("player-context: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("player-context returned after %v, want at least the 50ms separation", elapsed)
	}
	if got := held.metrics.SeparationWaits.Load(); got != 1 {
		t.Errorf("separation_waits = %d, want 1", got)
	}

	// A five second window with a ten second old mint and proof: no wait. The
	// first call proves the session, so the second one reads the marks below
	// rather than a proof it just performed.
	free, _, _, _ := newTestMinterFull(okMint, nil)
	if err := free.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, _, err := free.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("first player-context: %v", err)
	}
	free.mintSeparation = 5 * time.Second
	free.mu.Lock()
	free.lastMintAt = time.Now().Add(-10 * time.Second)
	free.lastProofAt = time.Now().Add(-10 * time.Second)
	free.mu.Unlock()

	start = time.Now()
	if _, _, err := free.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("player-context: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("player-context took %v, want no wait for a mint older than the window", elapsed)
	}
	if got := free.metrics.SeparationWaits.Load(); got != 0 {
		t.Errorf("separation_waits = %d, want 0 (the mint was already outside the window)", got)
	}
}

// The same spacing applies the other way round: a mint waits out the window
// since the last establishment.
func TestMintWaitsAfterEstablishment(t *testing.T) {
	ctx := context.Background()

	held, _, _, _ := newTestMinter(okMint)
	if err := held.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	held.mintSeparation = 50 * time.Millisecond
	held.mu.Lock()
	held.lastEstablishAt = time.Now()
	held.mu.Unlock()

	start := time.Now()
	if _, cached, err := held.Mint(ctx, "player", "vid"); err != nil || cached {
		t.Fatalf("mint: cached=%v err=%v, want a fresh mint", cached, err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("mint returned after %v, want at least the 50ms separation", elapsed)
	}
	if got := held.metrics.SeparationWaits.Load(); got != 1 {
		t.Errorf("separation_waits = %d, want 1", got)
	}

	free, _, _, _ := newTestMinter(okMint)
	if err := free.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	free.mintSeparation = 5 * time.Second
	free.mu.Lock()
	free.lastEstablishAt = time.Now().Add(-10 * time.Second)
	free.mu.Unlock()

	start = time.Now()
	if _, cached, err := free.Mint(ctx, "player", "vid"); err != nil || cached {
		t.Fatalf("mint: cached=%v err=%v, want a fresh mint", cached, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("mint took %v, want no wait for an establishment older than the window", elapsed)
	}
	if got := free.metrics.SeparationWaits.Load(); got != 0 {
		t.Errorf("separation_waits = %d, want 0 (the establishment was already outside the window)", got)
	}
}

// A failed sess.PlayerContext attempt still arms the mint gate: the page was
// touched even though the attempt never succeeded, so a following cache-miss
// mint waits out the separation window just as it would after a successful
// context.
func TestFailedPlayerContextArmsMintGate(t *testing.T) {
	ctx := context.Background()
	failing := errors.New("player-context failed")
	m, _, _, _ := newTestMinterFull(okMint, func(string) (browser.PlayerContext, error) {
		return browser.PlayerContext{}, failing
	})
	// mintSeparation stays 0 (newBareMinter's default) through the failing call,
	// so its own internal waits do not slow this test down; only the effect on a
	// later mint is under test.
	if _, _, err := m.PlayerContext(ctx, "vid"); err == nil {
		t.Fatal("player-context = nil error, want the configured failure")
	}
	m.mintSeparation = 50 * time.Millisecond

	start := time.Now()
	if _, cached, err := m.Mint(ctx, "player", "vid2"); err != nil || cached {
		t.Fatalf("mint: cached=%v err=%v, want a fresh mint", cached, err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("mint returned after %v, want at least the 50ms separation from the failed player-context attempt", elapsed)
	}
	if got := m.metrics.SeparationWaits.Load(); got != 1 {
		t.Errorf("separation_waits = %d, want 1", got)
	}
}

// A caller that goes away during a separation wait gets the context error, and
// neither the establishment nor the mint it was waiting for happens.
func TestSeparationWaitHonoursContext(t *testing.T) {
	var pcCalls, mintCalls int64
	countingMint := func(string) (browser.MintResult, error) {
		atomic.AddInt64(&mintCalls, 1)
		return browser.MintResult{Kind: "integrity", Token: "t", Lifetime: 3600}, nil
	}

	// The establishment side.
	pcSide, _, _, _ := newTestMinterFull(countingMint, func(string) (browser.PlayerContext, error) {
		atomic.AddInt64(&pcCalls, 1)
		return browser.PlayerContext{ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
	})
	if err := pcSide.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	pcSide.mintSeparation = time.Minute
	pcSide.mu.Lock()
	pcSide.lastMintAt = time.Now()
	pcSide.mu.Unlock()
	afterWarm := atomic.LoadInt64(&mintCalls)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, _, err := pcSide.PlayerContext(ctx, "vid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("player-context err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt64(&pcCalls); got != 0 {
		t.Errorf("session player-context calls = %d, want 0 (the wait was abandoned)", got)
	}
	if got := atomic.LoadInt64(&mintCalls); got != afterWarm {
		t.Errorf("fake mints = %d, want %d (no mint during an abandoned wait)", got, afterWarm)
	}

	// The mint side.
	atomic.StoreInt64(&mintCalls, 0)
	mintSide, _, _, _ := newTestMinter(countingMint)
	if err := mintSide.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	mintSide.mintSeparation = time.Minute
	mintSide.mu.Lock()
	mintSide.lastEstablishAt = time.Now()
	mintSide.mu.Unlock()
	afterWarm = atomic.LoadInt64(&mintCalls)

	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel2()
	}()
	if _, _, err := mintSide.Mint(ctx2, "player", "vid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("mint err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt64(&mintCalls); got != afterWarm {
		t.Errorf("fake mints = %d, want %d (no mint during an abandoned wait)", got, afterWarm)
	}
}

// The self-test reuses the token the attestation already minted instead of
// spending a second in-page mint on the same binding.
func TestSelfTestSkipsMintWhenPreminted(t *testing.T) {
	var mints int64
	m, _, _, _ := newTestMinter(func(id string) (browser.MintResult, error) {
		atomic.AddInt64(&mints, 1)
		return browser.MintResult{Kind: "integrity", Token: "tok-" + id, Lifetime: 3600}, nil
	})
	if err := m.SelfTest(context.Background()); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Errorf("fake mints = %d, want 1 (the pre-mint; the self-test reused it)", got)
	}
	if got := m.metrics.Mints.Load(); got != 1 {
		t.Errorf("mints metric = %d, want 1", got)
	}
}

// resetMintSeparationWarnOnces clears the process-wide once-guards on
// mintSeparationFromEnv's warnings, so a test can observe a warning fire again
// regardless of which subtest, or which other test in the package, already
// triggered it first.
func resetMintSeparationWarnOnces() {
	mintSeparationUnparseableOnce = sync.Once{}
	mintSeparationLargeOnce = sync.Once{}
}

// WAXSEAL_MINT_SEPARATION overrides the default; anything but a positive
// duration keeps it. A value past mintSeparationWarn is still accepted, but logs
// a warning, since a wait that long risks the per-request budget. Each subtest
// resets the warning once-guards first so it observes its own first call, the
// same way a process's very first tenant would.
func TestMintSeparationOverride(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      string
		want     time.Duration
		wantWarn string // substring the warn log must contain; "" means don't check
	}{
		{"unset", "", defaultMintSeparation, ""},
		{"valid", "3s", 3 * time.Second, ""},
		{"valid sub-second", "250ms", 250 * time.Millisecond, ""},
		{"unparseable", "soon", defaultMintSeparation, ""},
		{"bare number", "12", defaultMintSeparation, ""},
		{"zero", "0s", defaultMintSeparation, ""},
		{"negative", "-5s", defaultMintSeparation, ""},
		{"large", "90s", 90 * time.Second, "make first contexts time out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(mintSeparationEnv, tc.env)
			resetMintSeparationWarnOnces()
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			got := NewMinter("v", browser.Options{Logger: log}, 0, 0, 0).mintSeparation
			if got != tc.want {
				t.Errorf("%s=%q gave mintSeparation %v, want %v", mintSeparationEnv, tc.env, got, tc.want)
			}
			if tc.wantWarn != "" && !strings.Contains(logs.String(), tc.wantWarn) {
				t.Errorf("%s=%q logged %q, want a warning containing %q", mintSeparationEnv, tc.env, logs.String(), tc.wantWarn)
			}
		})
	}
}

// The unparseable-value and large-value warnings each log at most once per
// process: a fleet of tenants that share one bad WAXSEAL_MINT_SEPARATION must not
// repeat the identical warning once per tenant constructor.
func TestMintSeparationWarnOncePerProcess(t *testing.T) {
	t.Setenv(mintSeparationEnv, "90s")
	resetMintSeparationWarnOnces()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	NewMinter("v1", browser.Options{Logger: log}, 0, 0, 0)
	NewMinter("v2", browser.Options{Logger: log}, 0, 0, 0)
	NewMinter("v3", browser.Options{Logger: log}, 0, 0, 0)

	if got := strings.Count(logs.String(), "make first contexts time out"); got != 1 {
		t.Errorf("large-value warning logged %d times across three tenant constructors, want 1", got)
	}
}

// A positive mintSeparation passed to NewMinter (as server.Config.MintSeparation
// reaches it) overrides WAXSEAL_MINT_SEPARATION outright: the env value is never
// consulted, so no env-parsing warning fires either. A non-positive constructor
// value keeps the env-derived fallback, matching every existing caller that
// passes 0.
func TestMintSeparationConstructorOverride(t *testing.T) {
	t.Setenv(mintSeparationEnv, "3s")
	resetMintSeparationWarnOnces()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	overridden := NewMinter("v", browser.Options{Logger: log}, 0, 0, 20*time.Second)
	if overridden.mintSeparation != 20*time.Second {
		t.Errorf("mintSeparation = %v, want the constructor override 20s", overridden.mintSeparation)
	}

	fallback := NewMinter("v", browser.Options{Logger: log}, 0, 0, 0)
	if fallback.mintSeparation != 3*time.Second {
		t.Errorf("mintSeparation = %v, want the env-derived 3s when the constructor value is non-positive", fallback.mintSeparation)
	}
}

// The daemon proves full-length streaming once per browser session before it
// hands out any context, so a consumer's context is never the session's first
// establishment. The proof does not repeat on later requests.
func TestPlayerContextProvesBeforeFirstContext(t *testing.T) {
	var proofsWhenContextTaken []int
	fs := &fakeSession{mint: okMint}
	fs.playerCtx = func(string) (browser.PlayerContext, error) {
		proofsWhenContextTaken = append(proofsWhenContextTaken, fs.proofCount())
		return browser.PlayerContext{ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
	}
	var launches int64
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		return fs, nil
	}
	ctx := context.Background()

	if _, _, err := m.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("first player-context: %v", err)
	}
	if _, _, err := m.PlayerContext(ctx, "vid2"); err != nil {
		t.Fatalf("second player-context: %v", err)
	}
	if got := fs.proofCount(); got != 1 {
		t.Errorf("EnsureEstablished calls = %d, want 1 (the session proves once)", got)
	}
	if len(proofsWhenContextTaken) != 2 {
		t.Fatalf("session player-context calls = %d, want 2", len(proofsWhenContextTaken))
	}
	if proofsWhenContextTaken[0] != 1 {
		t.Errorf("proofs completed when the first context was taken = %d, want 1 (the proof runs first)", proofsWhenContextTaken[0])
	}
	if got := atomic.LoadInt64(&launches); got != 1 {
		t.Errorf("launches = %d, want 1", got)
	}
	m.mu.Lock()
	lastEstablish := m.lastEstablishAt
	m.mu.Unlock()
	if lastEstablish.IsZero() {
		t.Error("lastEstablishAt is zero after the proof; the mint gate would not see it")
	}
	if got := m.metrics.UnprovenRejections.Load(); got != 0 {
		t.Errorf("unproven_rejections = %d, want 0", got)
	}
}

// A session that cannot prove full-length streaming produces no context: the
// request is refused without relaunching, without asking the page for a context,
// and without marking the video unplayable.
func TestPlayerContextRefusesWhenProofFails(t *testing.T) {
	var pcCalls int64
	fs := &fakeSession{
		mint:         okMint,
		establishErr: errors.New("full-length proof failed"),
		playerCtx: func(string) (browser.PlayerContext, error) {
			atomic.AddInt64(&pcCalls, 1)
			return browser.PlayerContext{ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
		},
	}
	var launches int64
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		return fs, nil
	}
	ctx := context.Background()

	_, _, err := m.PlayerContext(ctx, "vid")
	if !errors.Is(err, ErrUnproven) {
		t.Fatalf("err = %v, want ErrUnproven", err)
	}
	if got := m.metrics.UnprovenRejections.Load(); got != 1 {
		t.Errorf("unproven_rejections = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&pcCalls); got != 0 {
		t.Errorf("session player-context calls = %d, want 0 (an unproven session hands out nothing)", got)
	}
	if got := atomic.LoadInt64(&launches); got != 1 {
		t.Errorf("launches = %d, want 1 (a failed proof must not relaunch)", got)
	}
	if fs.closed.Load() {
		t.Error("a failed proof must not retire the session")
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0", got)
	}

	// A repeat request inside the cool-down is refused at once, without another
	// proof attempt: the video itself is not negative-cached, but the
	// session-level proof cool-down still applies.
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("second request err = %v, want ErrUnproven", err)
	}
	if got := fs.proofCount(); got != 1 {
		t.Errorf("EnsureEstablished calls = %d, want 1 (the cool-down must refuse without re-proving)", got)
	}
	if got := m.metrics.UnprovenRejections.Load(); got != 2 {
		t.Errorf("unproven_rejections = %d, want 2 (both refused requests)", got)
	}
}

// The window is measured from the last in-page mint or proof playback, whichever
// is later, so a proof holds the next context back even when the mint is already
// old.
func TestPlayerContextWaitsAfterProof(t *testing.T) {
	ctx := context.Background()

	held, _, _, _ := newTestMinterFull(okMint, nil)
	if err := held.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	// The first call proves the session, so the second one reads the marks set
	// below instead of a proof it just performed.
	if _, _, err := held.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("first player-context: %v", err)
	}
	held.mintSeparation = 50 * time.Millisecond
	held.mu.Lock()
	held.lastMintAt = time.Now().Add(-time.Hour) // an anchor on the mint alone would not wait
	held.lastProofAt = time.Now()
	held.mu.Unlock()

	start := time.Now()
	if _, _, err := held.PlayerContext(ctx, "vid2"); err != nil {
		t.Fatalf("player-context: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("player-context returned after %v, want at least the 50ms separation from the proof", elapsed)
	}
	if got := held.metrics.SeparationWaits.Load(); got != 1 {
		t.Errorf("separation_waits = %d, want 1", got)
	}

	// A context handed out earlier does not extend the window: with the mint and
	// the proof both old, a preceding handoff must not cause a wait.
	free, _, _, _ := newTestMinterFull(okMint, nil)
	if err := free.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, _, err := free.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("first player-context: %v", err)
	}
	free.mintSeparation = 5 * time.Second
	free.mu.Lock()
	free.lastMintAt = time.Now().Add(-10 * time.Second)
	free.lastProofAt = time.Now().Add(-10 * time.Second)
	free.lastEstablishAt = time.Now() // the handoff just performed
	free.mu.Unlock()

	start = time.Now()
	if _, _, err := free.PlayerContext(ctx, "vid2"); err != nil {
		t.Fatalf("player-context: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("player-context took %v, want no wait: a context handoff does not extend the window", elapsed)
	}
	if got := free.metrics.SeparationWaits.Load(); got != 0 {
		t.Errorf("separation_waits = %d, want 0", got)
	}
}

// A second SessionSnapshot call on an already-proved session neither waits nor
// moves lastProofAt. The real EnsureEstablished short-circuits on an
// already-proved session without playing anything, so treating that as a fresh
// proof would move the separation anchor for free and force every later call to
// wait for no reason.
func TestSecondSessionSnapshotSkipsPhantomProof(t *testing.T) {
	ctx := context.Background()
	m, _, sessions, smu := newTestMinter(okMint)

	if _, _, _, err := m.SessionSnapshot(ctx); err != nil {
		t.Fatalf("first session snapshot: %v", err)
	}
	m.mu.Lock()
	firstProof := m.lastProofAt
	m.mu.Unlock()
	if firstProof.IsZero() {
		t.Fatal("lastProofAt is zero after the first snapshot")
	}
	smu.Lock()
	fs := (*sessions)[0]
	smu.Unlock()
	if got := fs.proofCount(); got != 1 {
		t.Fatalf("EnsureEstablished calls after the first snapshot = %d, want 1", got)
	}

	// A window well clear of the immediately preceding call makes the timing
	// unambiguous: a phantom re-proof would wait almost the whole window because
	// the first call's mark is still fresh.
	m.mintSeparation = 300 * time.Millisecond

	start := time.Now()
	if _, _, _, err := m.SessionSnapshot(ctx); err != nil {
		t.Fatalf("second session snapshot: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("second session snapshot took %v, want no wait: the session was already proved", elapsed)
	}
	if got := m.metrics.SeparationWaits.Load(); got != 0 {
		t.Errorf("separation_waits = %d, want 0", got)
	}
	if got := fs.proofCount(); got != 1 {
		t.Errorf("EnsureEstablished calls after the second snapshot = %d, want 1 (an already-proved session must not be re-proved)", got)
	}
	m.mu.Lock()
	secondProof := m.lastProofAt
	m.mu.Unlock()
	if !secondProof.Equal(firstProof) {
		t.Errorf("lastProofAt moved from %v to %v; a short-circuited establishment must not move it", firstProof, secondProof)
	}
}

// A second SelfTest call on an already-proved session does not call
// EnsureEstablished again or move lastProofAt, for the same reason as
// SessionSnapshot above.
func TestSelfTestSecondCallSkipsPhantomProof(t *testing.T) {
	m, _, sessions, smu := newTestMinter(okMint)
	ctx := context.Background()

	if err := m.SelfTest(ctx); err != nil {
		t.Fatalf("first self-test: %v", err)
	}
	m.mu.Lock()
	firstProof := m.lastProofAt
	m.mu.Unlock()
	if firstProof.IsZero() {
		t.Fatal("lastProofAt is zero after the first self-test")
	}
	smu.Lock()
	fs := (*sessions)[0]
	smu.Unlock()
	if got := fs.proofCount(); got != 1 {
		t.Fatalf("EnsureEstablished calls after the first self-test = %d, want 1", got)
	}

	if err := m.SelfTest(ctx); err != nil {
		t.Fatalf("second self-test: %v", err)
	}
	if got := fs.proofCount(); got != 1 {
		t.Errorf("EnsureEstablished calls after the second self-test = %d, want 1 (an already-proved session must not be re-proved)", got)
	}
	m.mu.Lock()
	secondProof := m.lastProofAt
	m.mu.Unlock()
	if !secondProof.Equal(firstProof) {
		t.Errorf("lastProofAt moved from %v to %v; a short-circuited establishment must not move it", firstProof, secondProof)
	}
}

// A session whose proof fails is refused immediately on every request inside
// the cool-down, without paying another proof attempt.
func TestPlayerContextProofCooldownRefusesWithoutReproving(t *testing.T) {
	fs := &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) { return fs, nil }
	ctx := context.Background()

	// The first request fails the proof and starts the cool-down.
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("first request err = %v, want ErrUnproven", err)
	}
	if got := fs.proofCount(); got != 1 {
		t.Fatalf("EnsureEstablished calls after the first request = %d, want 1", got)
	}

	// A second request inside the cool-down is refused without another proof
	// attempt or a relaunch.
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("second request err = %v, want ErrUnproven", err)
	}
	if got := fs.proofCount(); got != 1 {
		t.Errorf("EnsureEstablished calls after the second request = %d, want 1 (the cool-down must refuse without re-proving)", got)
	}
	if got := m.metrics.UnprovenRejections.Load(); got != 2 {
		t.Errorf("unproven_rejections = %d, want 2", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0 (the cool-down must not relaunch)", got)
	}
	if fs.closed.Load() {
		t.Error("a cool-down refusal must not retire the session")
	}
}

// The cool-down warning logs once per window, not once per refused request: a
// burst of repeated refusals against the same recorded failure must not flood
// the log at warn level. Every refusal still counts toward unproven_rejections.
func TestProofCooldownWarnOncePerWindow(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	fs := &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}
	m := NewMinter("v", browser.Options{Logger: log}, 0, 0, 0)
	m.mintSeparation = 0
	m.launch = func(context.Context) (minterSession, error) { return fs, nil }
	ctx := context.Background()

	// The first request fails the proof and starts the cool-down.
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("first request err = %v, want ErrUnproven", err)
	}

	// Three more requests land inside the same cool-down window.
	for i := 0; i < 3; i++ {
		if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
			t.Fatalf("repeat request %d err = %v, want ErrUnproven", i, err)
		}
	}
	if got := m.metrics.UnprovenRejections.Load(); got != 4 {
		t.Errorf("unproven_rejections = %d, want 4 (every refusal counts)", got)
	}
	if got := strings.Count(logs.String(), "proof cool-down"); got != 1 {
		t.Errorf("cool-down warning logged %d times across 4 refusals in the same window, want 1", got)
	}
}

// A second proof failure on the same generation, once the cool-down has passed,
// relaunches the session exactly once and re-proves on the new session, like the
// mint and player-context ladders' second level.
func TestPlayerContextProofSecondFailureRelaunchesAfterCooldown(t *testing.T) {
	var launches int64
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		n := atomic.AddInt64(&launches, 1)
		if n == 1 {
			return &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}, nil
		}
		return &fakeSession{mint: okMint}, nil // the relaunched session proves cleanly
	}
	ctx := context.Background()

	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("first request err = %v, want ErrUnproven", err)
	}
	if got := atomic.LoadInt64(&launches); got != 1 {
		t.Fatalf("launches = %d, want 1", got)
	}
	gen1 := m.Generation()
	preSecond := m.metrics.UnprovenRejections.Load()

	// Move the recorded failure outside the cool-down so the next request
	// retries the proof instead of refusing on sight.
	m.mu.Lock()
	m.proofFailedAt = time.Now().Add(-proofRetryCooldown - time.Second)
	m.mu.Unlock()

	pc, gen2, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("second request after the cool-down: %v", err)
	}
	if pc.ServerAbrStreamingURL == "" {
		t.Error("second request returned an empty context")
	}
	if gen2 == gen1 {
		t.Errorf("generation = %d, want a new generation after the second failure relaunched", gen2)
	}
	if got := atomic.LoadInt64(&launches); got != 2 {
		t.Errorf("launches = %d, want 2 (exactly one relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 1 {
		t.Errorf("escalations = %d, want 1", got)
	}
	// The second request is served, not refused: its retry against generation 1
	// fails and triggers the relaunch, but that failure is never itself returned
	// to a caller, and the relaunched session's own proof succeeds.
	// unproven_rejections counts refused requests, not proof attempts, so it
	// stays exactly where the first request's refusal left it.
	if got := m.metrics.UnprovenRejections.Load(); got != preSecond {
		t.Errorf("unproven_rejections = %d, want %d (unchanged: the second request was served)", got, preSecond)
	}
}

// A successful proof clears the recorded failure state, so a session that
// recovers is not treated as still on cool-down, and its next failure (if any)
// starts a fresh count rather than relaunching immediately.
func TestPlayerContextProofSuccessClearsCooldown(t *testing.T) {
	fs := &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) { return fs, nil }
	ctx := context.Background()

	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("first request err = %v, want ErrUnproven", err)
	}
	m.mu.Lock()
	failGen := m.proofFailGen
	m.mu.Unlock()
	if failGen == 0 {
		t.Fatal("proofFailGen is 0 after a failed proof")
	}

	// The session recovers, and the next request, past the cool-down, proves
	// successfully.
	fs.establishErr = nil
	m.mu.Lock()
	m.proofFailedAt = time.Now().Add(-proofRetryCooldown - time.Second)
	m.mu.Unlock()

	if _, _, err := m.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("second request: %v", err)
	}
	m.mu.Lock()
	failGen, failedAt := m.proofFailGen, m.proofFailedAt
	m.mu.Unlock()
	if failGen != 0 || !failedAt.IsZero() {
		t.Errorf("proof-failure state = (gen=%d, at=%v), want cleared after a successful proof", failGen, failedAt)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0 (a successful proof past the cool-down must not relaunch)", got)
	}
	if got := fs.proofCount(); got != 2 {
		t.Errorf("EnsureEstablished calls = %d, want 2 (the failed attempt and the successful retry)", got)
	}
}

// A caller that goes away while the session is proving gets the context error,
// and the proof is not counted as a failure: it was abandoned, not failed, so no
// cool-down starts.
func TestEnsureProvenHonoursContextDuringProof(t *testing.T) {
	fs := &fakeSession{mint: okMint, establishBlocks: true}
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) { return fs, nil }
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("player-context err = %v, want context.Canceled", err)
	}
	if got := m.metrics.UnprovenRejections.Load(); got != 0 {
		t.Errorf("unproven_rejections = %d, want 0 (an abandoned proof is not a failure)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0", got)
	}
	if fs.closed.Load() {
		t.Error("an abandoned proof must not retire the session")
	}
	m.mu.Lock()
	failGen := m.proofFailGen
	m.mu.Unlock()
	if failGen != 0 {
		t.Errorf("proofFailGen = %d, want 0 (an abandoned proof must not start the cool-down)", failGen)
	}
}

// A self-test whose proof fails starts the same cool-down a first
// player-context failure would, so the very next player-context request is
// refused on sight instead of paying for another proof attempt against a
// session that just failed one.
func TestSelfTestFailureStartsCooldown(t *testing.T) {
	fs := &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) { return fs, nil }
	ctx := context.Background()

	if err := m.SelfTest(ctx); err != nil {
		t.Fatalf("SelfTest = %v, want nil after a logged establishment failure", err)
	}
	if got := fs.proofCount(); got != 1 {
		t.Fatalf("EnsureEstablished calls after self-test = %d, want 1", got)
	}

	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("player-context err = %v, want ErrUnproven", err)
	}
	if got := fs.proofCount(); got != 1 {
		t.Errorf("EnsureEstablished calls after player-context = %d, want 1 (the cool-down must refuse without another proof attempt)", got)
	}
	if got := m.metrics.UnprovenRejections.Load(); got != 1 {
		t.Errorf("unproven_rejections = %d, want 1", got)
	}
}

// A self-test failure never claims the failure streak's relaunch (it records
// through recordProofFailure with claimRelaunch=false), so it counts only as the
// generation's first failure. Once the cool-down has passed, the next
// PlayerContext failure on that same generation is graded as the second failure
// and relaunches exactly once, the same ladder a PlayerContext-only failure
// streak follows.
func TestSelfTestFailureThenPlayerContextRelaunchesAfterCooldown(t *testing.T) {
	var launches int64
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		n := atomic.AddInt64(&launches, 1)
		if n == 1 {
			return &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}, nil
		}
		return &fakeSession{mint: okMint}, nil // the relaunched session proves cleanly
	}
	ctx := context.Background()

	if err := m.SelfTest(ctx); err != nil {
		t.Fatalf("SelfTest = %v, want nil after a logged establishment failure", err)
	}
	gen1 := m.Generation()
	if got := atomic.LoadInt64(&launches); got != 1 {
		t.Fatalf("launches after self-test = %d, want 1", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Fatalf("escalations after self-test = %d, want 0 (a self-test failure must not relaunch)", got)
	}

	// Move the recorded failure outside the cool-down so the next request
	// retries the proof instead of refusing on sight.
	m.mu.Lock()
	m.proofFailedAt = time.Now().Add(-proofRetryCooldown - time.Second)
	m.mu.Unlock()

	pc, gen2, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("player-context after the cool-down: %v", err)
	}
	if pc.ServerAbrStreamingURL == "" {
		t.Error("player-context returned an empty context")
	}
	if gen2 == gen1 {
		t.Errorf("generation = %d, want a new generation: the self-test failure's second failure must relaunch", gen2)
	}
	if got := atomic.LoadInt64(&launches); got != 2 {
		t.Errorf("launches = %d, want 2 (exactly one relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 1 {
		t.Errorf("escalations = %d, want 1", got)
	}
}

// Publishing a new generation clears the previous session's mint, proof, and
// establishment marks, and any pending proof-failure cool-down: none of that
// history describes the fresh page. The relaunch's own pre-mint then sets
// lastMintAt again, so that mark alone is not left zero along with the rest.
func TestEnsureResetsMarksOnNewGeneration(t *testing.T) {
	m, launches, _, _ := newTestMinter(okMint)
	ctx := context.Background()

	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	// Give generation 1 a full set of marks and a recorded proof failure, as a
	// live session accumulates over time.
	m.mu.Lock()
	oldMint := time.Now().Add(-time.Hour)
	m.lastMintAt = oldMint
	m.lastProofAt = time.Now().Add(-time.Hour)
	m.lastEstablishAt = time.Now().Add(-time.Hour)
	m.proofFailGen = m.gen
	m.proofFailedAt = time.Now().Add(-time.Hour)
	gen1 := m.gen
	m.mu.Unlock()

	if !m.retire(gen1, "test", false) {
		t.Fatal("retire(gen1) returned false, want true")
	}
	if _, _, err := m.ensure(ctx); err != nil {
		t.Fatalf("ensure (relaunch): %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Fatalf("launches = %d, want 2", got)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen == gen1 {
		t.Fatal("generation did not advance")
	}
	if m.lastMintAt.IsZero() || m.lastMintAt.Equal(oldMint) {
		t.Errorf("lastMintAt = %v, want the new generation's pre-mint time, not zero or the old generation's mark", m.lastMintAt)
	}
	if !m.lastProofAt.IsZero() {
		t.Errorf("lastProofAt = %v, want zero on a fresh generation", m.lastProofAt)
	}
	if !m.lastEstablishAt.IsZero() {
		t.Errorf("lastEstablishAt = %v, want zero on a fresh generation", m.lastEstablishAt)
	}
	if m.proofFailGen != 0 {
		t.Errorf("proofFailGen = %d, want 0 on a fresh generation", m.proofFailGen)
	}
	if !m.proofFailedAt.IsZero() {
		t.Errorf("proofFailedAt = %v, want zero on a fresh generation", m.proofFailedAt)
	}
}

// If the relaunched session's own proof also fails inside ensureProven, the
// request is refused with ErrUnproven rather than looping: the ladder relaunches
// exactly once, and the fresh generation's own failure starts its own cool-down
// independent of the generation it replaced.
func TestPlayerContextProofSecondFailureRelaunchAlsoFails(t *testing.T) {
	var launches int64
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		return &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}, nil
	}
	ctx := context.Background()

	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("first request err = %v, want ErrUnproven", err)
	}
	gen1 := m.Generation()

	// Move the recorded failure outside the cool-down so the next request
	// retries the proof instead of refusing on sight.
	m.mu.Lock()
	m.proofFailedAt = time.Now().Add(-proofRetryCooldown - time.Second)
	m.mu.Unlock()

	_, gen2, err := m.PlayerContext(ctx, "vid")
	if !errors.Is(err, ErrUnproven) {
		t.Fatalf("second request err = %v, want ErrUnproven", err)
	}
	if gen2 == gen1 {
		t.Errorf("generation = %d, want a new generation after the relaunch", gen2)
	}
	if got := atomic.LoadInt64(&launches); got != 2 {
		t.Errorf("launches = %d, want 2 (exactly one relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 1 {
		t.Errorf("escalations = %d, want 1 (the relaunch ladder fires exactly once)", got)
	}
	// Two refusals, not three: the first request's failure (starts the
	// cool-down), and the second request's ultimate failure once the relaunched
	// generation's own proof also fails. The second request's initial retry
	// against generation 1 triggers the relaunch but is never itself returned to
	// a caller, so only a return that hands an error back to the caller counts.
	if got := m.metrics.UnprovenRejections.Load(); got != 2 {
		t.Errorf("unproven_rejections = %d, want 2", got)
	}
	m.mu.Lock()
	failGen, failedAt := m.proofFailGen, m.proofFailedAt
	m.mu.Unlock()
	if failGen != gen2 {
		t.Errorf("proofFailGen = %d, want %d (the relaunched generation, not the one it replaced)", failGen, gen2)
	}
	if failedAt.IsZero() {
		t.Error("proofFailedAt is zero after the relaunched session's failed proof")
	}
}

// A proof that runs out the request's own deadline is recorded as a failure
// rather than treated as an abandoned caller: the environment failed to prove
// within the budget the handler gave it, so the cool-down applies to the next
// request. The function still reports context.DeadlineExceeded, so the server's
// timeout mapping is unchanged.
func TestPlayerContextProofDeadlineExceededRecordsFailure(t *testing.T) {
	fs := &fakeSession{mint: okMint, establishBlocks: true}
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) { return fs, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, gen, err := m.PlayerContext(ctx, "vid")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	m.mu.Lock()
	failGen, failedAt := m.proofFailGen, m.proofFailedAt
	m.mu.Unlock()
	if failGen != gen {
		t.Errorf("proofFailGen = %d, want %d (the deadline failure must be recorded)", failGen, gen)
	}
	if failedAt.IsZero() {
		t.Error("proofFailedAt is zero after a deadline-exceeded proof")
	}
	if got := m.metrics.UnprovenRejections.Load(); got != 1 {
		t.Errorf("unproven_rejections = %d, want 1", got)
	}
}

// The proof-driven relaunch is spent once per failure streak, not once per
// generation: while it is unspent, a generation's second proof failure
// relaunches once; once spent, a second failure on any later generation
// (whether that generation came from the proof ladder itself or from some other
// relaunch entirely, such as a crash) records the failure and refuses without
// relaunching again. Only a successful proof frees the streak's relaunch for
// reuse.
func TestPlayerContextProofRelaunchSpentOncePerStreak(t *testing.T) {
	var launches int64
	var gen3Session *fakeSession
	m := newBareMinter(0, 0)
	m.launch = func(context.Context) (minterSession, error) {
		n := atomic.AddInt64(&launches, 1)
		fs := &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}
		if n == 3 {
			gen3Session = fs
		}
		return fs, nil
	}
	ctx := context.Background()
	rewindCooldown := func() {
		m.mu.Lock()
		m.proofFailedAt = time.Now().Add(-proofRetryCooldown - time.Second)
		m.mu.Unlock()
	}

	// Generation 1's first failure: recorded, no relaunch.
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("gen1 first request err = %v, want ErrUnproven", err)
	}
	gen1 := m.Generation()

	// Generation 1's second failure, past the cool-down: the streak's relaunch is
	// unspent, so this relaunches to generation 2, whose own inline proof also
	// fails as generation 2's first failure.
	rewindCooldown()
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("gen1 second request err = %v, want ErrUnproven", err)
	}
	gen2 := m.Generation()
	if gen2 == gen1 {
		t.Fatal("generation did not advance after the second failure")
	}
	if got := atomic.LoadInt64(&launches); got != 2 {
		t.Fatalf("launches = %d, want 2 (exactly one relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 1 {
		t.Fatalf("escalations = %d, want 1", got)
	}

	// Generation 2's second failure, past the cool-down: the streak already spent
	// its relaunch, so this refuses instead of relaunching.
	rewindCooldown()
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("gen2 second request err = %v, want ErrUnproven", err)
	}
	if got := atomic.LoadInt64(&launches); got != 2 {
		t.Errorf("launches = %d, want 2 (the streak's relaunch is already spent)", got)
	}

	// Something other than the proof ladder relaunches the browser again (a
	// crash, a mint failure, or a degradation report all retire the session and
	// let the next ensure relaunch it). Generation 3's own failures must still
	// honour the already-spent streak.
	if !m.retire(gen2, "test: simulated out-of-band relaunch", false) {
		t.Fatal("retire(gen2) returned false, want true")
	}
	if _, _, err := m.ensure(ctx); err != nil {
		t.Fatalf("ensure (simulated relaunch): %v", err)
	}
	gen3 := m.Generation()
	if gen3 == gen2 {
		t.Fatal("generation did not advance after the simulated relaunch")
	}
	if got := atomic.LoadInt64(&launches); got != 3 {
		t.Fatalf("launches = %d, want 3 (the simulated out-of-band relaunch)", got)
	}

	// Generation 3's first failure: recorded, no relaunch (not yet a second
	// failure on this generation).
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("gen3 first request err = %v, want ErrUnproven", err)
	}
	if got := atomic.LoadInt64(&launches); got != 3 {
		t.Errorf("launches = %d, want 3 (a first failure never relaunches)", got)
	}

	// Generation 3's second failure, past the cool-down: still refuses without
	// relaunching, because the streak's one relaunch was spent back on
	// generation 1 and a new generation does not reset it.
	rewindCooldown()
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("gen3 second request err = %v, want ErrUnproven", err)
	}
	if got := atomic.LoadInt64(&launches); got != 3 {
		t.Errorf("launches = %d, want 3 (the streak's relaunch stays spent across generations)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 1 {
		t.Errorf("escalations = %d, want 1 (no further relaunch across three failing generations)", got)
	}

	// The session recovers, and the next request, past the cool-down, proves
	// successfully: that clears the flag.
	gen3Session.establishErr = nil
	rewindCooldown()
	if _, _, err := m.PlayerContext(ctx, "vid"); err != nil {
		t.Fatalf("gen3 recovery request: %v", err)
	}
	m.mu.Lock()
	stillSet := m.proofRelaunched
	m.mu.Unlock()
	if stillSet {
		t.Fatal("proofRelaunched is still set after a successful proof")
	}

	// A later double failure relaunches again: clearing the flag freed a fresh
	// streak's one relaunch.
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		return &fakeSession{mint: okMint, establishErr: errors.New("full-length proof failed")}, nil
	}
	if !m.retire(gen3, "test: force a fresh failing generation", false) {
		t.Fatal("retire(gen3) returned false, want true")
	}
	if _, _, err := m.ensure(ctx); err != nil {
		t.Fatalf("ensure (fresh failing generation): %v", err)
	}
	launchesBeforeRelaunch := atomic.LoadInt64(&launches)
	gen4 := m.Generation()

	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("gen4 first request err = %v, want ErrUnproven", err)
	}
	rewindCooldown()
	if _, _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("gen4 second request err = %v, want ErrUnproven", err)
	}
	if got := atomic.LoadInt64(&launches); got != launchesBeforeRelaunch+1 {
		t.Errorf("launches = %d, want %d (a fresh streak relaunches once more)", got, launchesBeforeRelaunch+1)
	}
	if got := m.metrics.Escalations.Load(); got != 2 {
		t.Errorf("escalations = %d, want 2 (a second relaunch after the flag cleared)", got)
	}
	gen5 := m.Generation()
	if gen5 == gen4 {
		t.Error("generation did not advance on the fresh streak's relaunch")
	}
}
