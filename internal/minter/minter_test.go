package minter

import (
	"context"
	"errors"
	"fmt"
	"github.com/colespringer/waxseal/internal/browser"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSession is an in-memory minterSession for testing the Minter's reliability
// logic without a browser.
type fakeSession struct {
	mint         func(identifier string) (browser.MintResult, error)
	playerCtx    func(videoID string) (browser.PlayerContext, error)
	ping         func() error // nil reports a healthy browser
	establishErr error
	cookies      []*http.Cookie
	cookiesErr   error
	id           browser.Identity // zero value reports a default visitor_data
	established  bool
	lastProbe    browser.FullLengthProbe
	lastProbeAt  time.Time
	closed       atomic.Bool
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
func (f *fakeSession) EnsureEstablished(context.Context) error { return f.establishErr }

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
func (f *fakeSession) BrowserCookies() ([]*http.Cookie, error) { return f.cookies, f.cookiesErr }
func (f *fakeSession) Established() bool                       { return f.established }
func (f *fakeSession) LastProof() (browser.FullLengthProbe, time.Time) {
	return f.lastProbe, f.lastProbeAt
}
func (f *fakeSession) Close() { f.closed.Store(true) }

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
	m := NewMinter("v", browser.Options{}, 0, 0)
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
	m := NewMinter("v", browser.Options{}, 0, 0)
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

	r1, c1, err := m.Mint(ctx, "gvs", "vd")
	if err != nil || c1 {
		t.Fatalf("first mint: cached=%v err=%v, want cached=false", c1, err)
	}
	r2, c2, err := m.Mint(ctx, "gvs", "vd")
	if err != nil || !c2 {
		t.Fatalf("repeat mint: cached=%v err=%v, want cached=true", c2, err)
	}
	if r1.Token != r2.Token {
		t.Errorf("cached token = %q, want same as first %q", r2.Token, r1.Token)
	}
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Errorf("mints = %d, want 1 (second served from cache)", got)
	}
	// A different binding mints again, but still on the one attestation.
	if _, c3, _ := m.Mint(ctx, "player", "vid"); c3 {
		t.Errorf("new binding should not be a cache hit")
	}
	if got := atomic.LoadInt64(&mints); got != 2 {
		t.Errorf("mints = %d, want 2", got)
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

// TestMinterEscalationLadder: a mint that fails twice triggers one in-place retry
// then a relaunch (re-attest) on a fresh session; the old session is closed.
func TestMinterEscalationLadder(t *testing.T) {
	var attempt int64
	m, launches, sessions, smu := newTestMinter(func(string) (browser.MintResult, error) {
		if n := atomic.AddInt64(&attempt, 1); n <= 2 {
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

	if _, _, err := m.Mint(ctx, "gvs", "vd"); err != nil { // gen 1, cached
		t.Fatalf("first mint: %v", err)
	}
	_, gen, err := m.ensure(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	m.retire(gen, "simulated crash") // browser dies; generation unchanged

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

	// The generation-1 gvs/vd entry is stale after the relaunch.
	if _, cached, _ := m.Mint(ctx, "gvs", "vd"); cached {
		t.Errorf("old-generation cache entry should be invalidated by the relaunch")
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
				Status:                "OK",
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
			return browser.PlayerContext{Status: "OK", ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
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
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Errorf("warm session should survive a client cancel")
	}
}

// TestMinterNegCacheBoundedEvicts: at capacity with every entry live, a new terminal
// result is still cached (evicting an older one) instead of dropped, so the map stays
// bounded and the newest unplayable id is the one kept.
func TestMinterNegCacheBoundedEvicts(t *testing.T) {
	m := NewMinter("v", browser.Options{}, 0, 0)
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

// newPingMinter records each session and configures its Ping result.
func newPingMinter(ping func() error) (*Minter, *[]*fakeSession, *sync.Mutex) {
	var sessions []*fakeSession
	var smu sync.Mutex
	m := NewMinter("v", browser.Options{}, 0, 0)
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

// An unwarmed tenant must report errNoSession without launching a browser.
func TestMinterHealthNoSession(t *testing.T) {
	m, sessions, smu := newPingMinter(nil)
	if _, live, err := m.Health(context.Background()); live || !errors.Is(err, errNoSession) {
		t.Errorf("Health = (live=%v, %v), want (false, errNoSession)", live, err)
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

// A failed probe retires the session when no browser operation is in progress.
func TestMinterHealthDeadPingRetires(t *testing.T) {
	m, sessions, smu := newPingMinter(func() error { return errors.New("cdp connection closed") })
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, _, err := m.Health(context.Background()); err == nil {
		t.Fatal("Health = nil, want the probe error")
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Error("failed probe did not retire the idle session")
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
	m := NewMinter("v", browser.Options{}, 0, 0)
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
	m := NewMinter("v", browser.Options{}, 0, 0)
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
	m := NewMinter("v", browser.Options{}, 0, 0)
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
	m := NewMinter("v", browser.Options{}, 0, 0)
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
	m := NewMinter("v", browser.Options{}, 0, 0)
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
	m := NewMinter("v", browser.Options{}, streamingMaxAge, 0)
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

// A second report within the debounce window is rate-limited.
func TestMinterReportDegradedRateLimited(t *testing.T) {
	m, _, _, _ := newStreamingMinter(0, func(string) (browser.PlayerContext, error) {
		return browser.PlayerContext{ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
	})
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	if res := m.ReportDegraded(1, "vid", "cap"); !res.Retired { // retires gen 1, starts debounce
		t.Fatalf("first report = %+v, want Retired", res)
	}
	if _, gen, err := m.PlayerContext(ctx, "vid"); err != nil || gen != 2 { // relaunch, gen 2 live
		t.Fatalf("relaunch: gen=%d err=%v, want gen=2", gen, err)
	}
	res := m.ReportDegraded(2, "vid", "cap") // within debounce
	if res.Accepted || res.RetryAfterSeconds <= 0 {
		t.Fatalf("within-debounce report = %+v, want !Accepted and RetryAfterSeconds>0", res)
	}
	if got := m.metrics.DegradationReportsRateLimited.Load(); got != 1 {
		t.Errorf("degradation_reports_rate_limited = %d, want 1", got)
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

// A deferred report-driven retirement starts the debounce window.
func TestMinterReportDeferredStartsDebounce(t *testing.T) {
	m, _, _, release, done := startBlockingPlayerContext(t)

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
		t.Fatalf("report after a deferred retire = %+v, want rate limited (the deferred path must start the debounce)", res)
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

	if !m.retire(1, "browser target crashed") {
		t.Fatal("retire(1) = false, want true for the current generation")
	}
	m.mu.Lock()
	gen, vid := m.reportSuspectGen, m.reportSuspectVideoID
	m.mu.Unlock()
	if gen != 0 || vid != "" {
		t.Errorf("suspect after retire = (gen=%d, vid=%q), want cleared", gen, vid)
	}
	// A retire of a non-current generation is a no-op returning false.
	if m.retire(99, "stale") {
		t.Error("retire(99) = true, want false for a non-current generation")
	}
}

// PlayerContext returns the generation that produced its context.
func TestMinterGenerationIdentifiesContext(t *testing.T) {
	var n int64
	m := NewMinter("v", browser.Options{}, 0, 0)
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
	m := NewMinter("v", browser.Options{}, 0, 0)
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
	if !m.retire(1, "recycle") {
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

// MetricsSnapshot omits proof details until a probe runs and includes the suspect
// video only while a report is outstanding.
func TestMinterMetricsSnapshotStreamingFields(t *testing.T) {
	m, _, _, _ := newStreamingMinter(time.Hour, nil)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1, never probed
		t.Fatalf("warm: %v", err)
	}
	snap := m.MetricsSnapshot()
	if _, ok := snap["last_browser_proof_outcome"]; ok {
		t.Error("last_browser_proof_outcome present though the session was never probed")
	}
	if _, ok := snap["last_browser_proof_age_secs"]; ok {
		t.Error("last_browser_proof_age_secs present though the session was never probed")
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
	if _, ok := snap["streaming_suspect_video"]; ok {
		t.Error("streaming_suspect_video present when no report is outstanding")
	}
	for _, k := range []string{"streaming_recycles", "report_driven_recycles", "degradation_reports_accepted", "degradation_reports_rejected_stale", "degradation_reports_rate_limited"} {
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

// ReportDebounce controls when a subsequent report is accepted.
func TestMinterReportDebounceConfigurable(t *testing.T) {
	m := NewMinter("v", browser.Options{}, 0, 250*time.Millisecond)
	m.launch = func(context.Context) (minterSession, error) { return &fakeSession{mint: okMint}, nil }
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	// A report 200ms after retirement is within the 250ms window.
	m.mu.Lock()
	m.lastReportRetireAt = time.Now().Add(-200 * time.Millisecond)
	m.mu.Unlock()
	if res := m.ReportDegraded(1, "vid", "cap"); res.Accepted {
		t.Fatalf("report 200ms after a retire = %+v, want rate limited under a 250ms debounce", res)
	}
	// A report 300ms after retirement is outside the configured window.
	m.mu.Lock()
	m.lastReportRetireAt = time.Now().Add(-300 * time.Millisecond)
	m.mu.Unlock()
	if res := m.ReportDegraded(1, "vid", "cap"); !res.Accepted {
		t.Fatalf("report 300ms after a retire = %+v, want accepted under a 250ms debounce", res)
	}
}

// NewMinter falls back to the default debounce when given a non-positive window.
func TestMinterReportDebounceDefaultsWhenUnset(t *testing.T) {
	if m := NewMinter("v", browser.Options{}, 0, 0); m.reportDebounce != DefaultReportDebounce {
		t.Errorf("reportDebounce = %v, want default %v", m.reportDebounce, DefaultReportDebounce)
	}
	if m := NewMinter("v", browser.Options{}, 0, -time.Second); m.reportDebounce != DefaultReportDebounce {
		t.Errorf("reportDebounce (negative) = %v, want default %v", m.reportDebounce, DefaultReportDebounce)
	}
	if m := NewMinter("v", browser.Options{}, 0, 90*time.Second); m.reportDebounce != 90*time.Second {
		t.Errorf("reportDebounce = %v, want 90s", m.reportDebounce)
	}
}
