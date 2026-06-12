// Package minter adds caching, retries, crash recovery, and tenant routing to
// browser sessions.
package minter

import (
	"context"
	"errors"
	"fmt"
	"github.com/colespringer/waxseal/internal/browser"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// Minter adds token caching, single-flight attestation, retries, crash recovery,
// session recycling, and metrics to one browser identity. Mint and PlayerContext
// calls serialize because they share one page. Tenants manages multiple Minters.
type Minter struct {
	video  string
	opts   browser.Options
	log    *slog.Logger
	maxAge time.Duration // recycle the session once it is older than this

	// launch starts and attests a session. Tests replace it so the reliability
	// logic can run without a browser.
	launch func(ctx context.Context) (minterSession, error)

	mu          sync.Mutex
	sess        minterSession
	gen         uint64 // bumps on each (re)attest; invalidates older cache entries
	attestedAt  time.Time
	watchCancel context.CancelFunc // cancels the live session's crash watcher on teardown
	launching   chan struct{}      // non-nil while an attestation is in flight (single-flight)
	cache       map[string]cachedToken
	negCache    map[string]negEntry // terminal player-context errors by video_id, guarded by mu

	mintMu  sync.Mutex // serializes the in-browser mint calls (single page)
	metrics minterMetrics
}

// minterSession is the part of browser.Session used by Minter. Tests replace it
// with an in-memory implementation.
type minterSession interface {
	Mint(ctx context.Context, identifier string) (browser.MintResult, error)
	PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, error)
	AttestKind() string
	Identity() browser.Identity
	BrowserCookies() []*http.Cookie
	Close()
}

type cachedToken struct {
	res    browser.MintResult
	expiry time.Time
	gen    uint64
}

// negEntry records a terminal player-context error and its expiry. It is not tied
// to a session generation because relaunching cannot make the video playable.
type negEntry struct {
	err    error
	expiry time.Time
}

// minterMetrics contains process-lifetime counters. Failure counters count
// attempts, not requests. PlayerContextFailures also counts negative-cache hits.
type minterMetrics struct {
	Attestations          atomic.Int64
	LaunchFailures        atomic.Int64
	Mints                 atomic.Int64
	MintFailures          atomic.Int64 // per attempt (see minterMetrics doc)
	Escalations           atomic.Int64
	CacheHits             atomic.Int64
	CacheMisses           atomic.Int64
	Crashes               atomic.Int64
	PlayerContexts        atomic.Int64
	PlayerContextFailures atomic.Int64 // failed attempts and negative-cache hits
}

const (
	minterMaxCacheTTL   = 6 * time.Hour
	minterCacheMargin   = 5 * time.Minute // don't hand out a token within this of expiry
	minterDefaultMaxAge = 11 * time.Hour  // < the ~12h integrity lifetime
	minterNegCacheTTL   = 5 * time.Minute // remember an unplayable video_id this long
	minterNegCacheMax   = 256             // bound the negative cache
)

// NewMinter builds a single-identity minter for video (the landing watch id). It
// does not launch a browser until the first Warm/Mint/Identity call.
func NewMinter(video string, opts browser.Options) *Minter {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Minter{
		video:    video,
		opts:     opts,
		log:      log,
		maxAge:   minterDefaultMaxAge,
		cache:    make(map[string]cachedToken),
		negCache: make(map[string]negEntry),
	}
	m.launch = m.launchReal
	return m
}

// launchReal starts a browser session and attests it.
func (m *Minter) launchReal(ctx context.Context) (minterSession, error) {
	sess, err := browser.Launch(ctx, m.video, m.opts)
	if err != nil {
		return nil, err
	}
	if err := sess.Attest(ctx); err != nil {
		sess.Close()
		return nil, err
	}
	return sess, nil
}

// Warm performs the single-flight attestation before the first request.
func (m *Minter) Warm(ctx context.Context) error {
	_, _, err := m.ensure(ctx)
	return err
}

// ensure returns the live session and its generation. Concurrent launches
// coalesce into one attestation, and sessions older than maxAge are recycled.
func (m *Minter) ensure(ctx context.Context) (minterSession, uint64, error) {
	for {
		m.mu.Lock()
		// Max-age recycle: retire a session older than maxAge.
		if m.sess != nil && m.maxAge > 0 && time.Since(m.attestedAt) > m.maxAge {
			old, gen, age := m.sess, m.gen, time.Since(m.attestedAt)
			m.sess = nil
			cancel := m.watchCancel
			m.watchCancel = nil
			m.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			m.log.Info("minter: max-age recycle", "gen", gen, "age", age.Round(time.Second))
			old.Close()
			continue
		}
		if m.sess != nil {
			s, g := m.sess, m.gen
			m.mu.Unlock()
			return s, g, nil
		}
		if m.launching != nil { // another goroutine is attesting; wait for it.
			ch := m.launching
			m.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
			continue
		}
		// We own the (single-flighted) launch.
		ch := make(chan struct{})
		m.launching = ch
		m.mu.Unlock()

		sess, err := m.launch(ctx)

		m.mu.Lock()
		m.launching = nil
		close(ch)
		if err != nil {
			m.mu.Unlock()
			m.metrics.LaunchFailures.Add(1)
			return nil, 0, err
		}
		m.sess = sess
		m.gen++
		m.attestedAt = time.Now()
		g := m.gen
		// The crash watcher must outlive the (transient) launch ctx, so give it a
		// session-scoped context cancelled only when this session is torn down.
		watchCtx, cancel := context.WithCancel(context.Background())
		m.watchCancel = cancel
		m.mu.Unlock()
		m.metrics.Attestations.Add(1)
		m.log.Info("minter: session ready", "gen", g, "attest", sess.AttestKind())
		go m.watchCrash(sess, watchCtx, g)
		return sess, g, nil
	}
}

// retire closes generation gen if it is still current, causing the next ensure
// call to relaunch. A stale generation is a no-op.
func (m *Minter) retire(gen uint64, reason string) {
	m.mu.Lock()
	if m.sess == nil || m.gen != gen {
		m.mu.Unlock()
		return
	}
	old := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.log.Warn("minter: retiring session", "gen", gen, "reason", reason)
	old.Close()
}

// watchCrash retires the session if its browser target crashes or detaches, so a
// crash is recovered proactively (next request relaunches) instead of only after
// a failed mint. No-op for a non-*browser.Session (test fake).
func (m *Minter) watchCrash(s minterSession, ctx context.Context, gen uint64) {
	real, ok := s.(*browser.Session)
	if !ok || real.Page() == nil {
		return
	}
	// Use a session-scoped context so the watcher survives the launch request and
	// exits when the session is torn down.
	wait := real.Page().Context(ctx).EachEvent(
		func(*proto.InspectorTargetCrashed) (stop bool) {
			m.metrics.Crashes.Add(1)
			m.retire(gen, "browser target crashed")
			return true
		},
		func(e *proto.InspectorDetached) (stop bool) {
			m.retire(gen, "browser detached: "+e.Reason)
			return true
		},
	)
	wait()
}

// Mint returns a token for (scope, binding), reporting whether it came from cache.
// The retry policy serves cached tokens first, retries one failed mint in place,
// then relaunches and attests before the final attempt. Repeated requests for the
// same binding continue to use the cached token.
func (m *Minter) Mint(ctx context.Context, scope, binding string) (res browser.MintResult, cached bool, err error) {
	key := scope + "|" + binding
	if r, ok := m.cacheGet(key); ok {
		m.metrics.CacheHits.Add(1)
		return r, true, nil
	}

	m.mintMu.Lock() // one page, so mints serialize
	defer m.mintMu.Unlock()
	// Another goroutine may have filled the cache while this call waited for mintMu.
	if r, ok := m.cacheGet(key); ok {
		m.metrics.CacheHits.Add(1)
		return r, true, nil
	}
	m.metrics.CacheMisses.Add(1)

	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return browser.MintResult{}, false, err
	}
	res, err = sess.Mint(ctx, binding)
	if err != nil { // level 1: transient failure, one in-place retry, no re-attest.
		m.metrics.MintFailures.Add(1)
		m.log.Warn("minter: mint failed; retrying on same session", "gen", gen, "err", err)
		res, err = sess.Mint(ctx, binding)
	}
	if err != nil { // level 2: escalate to a relaunch and re-attest on a fresh session.
		m.metrics.MintFailures.Add(1)
		m.metrics.Escalations.Add(1)
		m.retire(gen, "mint failed twice; relaunching")
		sess, gen, err = m.ensure(ctx)
		if err != nil {
			return browser.MintResult{}, false, err
		}
		if res, err = sess.Mint(ctx, binding); err != nil {
			m.metrics.MintFailures.Add(1)
			return browser.MintResult{}, false, fmt.Errorf("minter: mint failed after relaunch: %w", err)
		}
	}
	m.metrics.Mints.Add(1)
	m.cachePut(key, res, gen)
	return res, false, nil
}

// PlayerContext returns the attested browser's streaming context for videoID. It
// reuses the warm session and follows the same retry and relaunch policy as Mint.
// Successful contexts are not cached because their URLs contain a short-lived
// nonce. Terminal unplayable errors are cached briefly.
func (m *Minter) PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, error) {
	// A known-unplayable video fails before mintMu and the session, so a consumer
	// retrying a 502 (or a malicious caller) can't grind the tenant into relaunches.
	if err := m.negCacheGet(videoID); err != nil {
		m.metrics.PlayerContextFailures.Add(1)
		return browser.PlayerContext{}, err
	}

	m.mintMu.Lock() // one page, so player-context calls serialize with mints
	defer m.mintMu.Unlock()

	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return browser.PlayerContext{}, err
	}

	pc, err := sess.PlayerContext(ctx, videoID)
	if err == nil {
		m.metrics.PlayerContexts.Add(1)
		return pc, nil
	}
	if m.playerContextStop(ctx, videoID, err) { // terminal or cancelled: don't escalate.
		return browser.PlayerContext{}, err
	}

	// level 1: transient failure, one in-place retry, no re-attest.
	m.log.Warn("minter: player-context failed; retrying on same session", "gen", gen, "err", err)
	pc, err = sess.PlayerContext(ctx, videoID)
	if err == nil {
		m.metrics.PlayerContexts.Add(1)
		return pc, nil
	}
	if m.playerContextStop(ctx, videoID, err) {
		return browser.PlayerContext{}, err
	}

	// level 2: escalate to a relaunch and re-attest on a fresh session.
	m.metrics.Escalations.Add(1)
	m.retire(gen, "player-context failed twice; relaunching")
	sess, _, err = m.ensure(ctx)
	if err != nil {
		return browser.PlayerContext{}, err
	}
	pc, err = sess.PlayerContext(ctx, videoID)
	if err != nil {
		m.metrics.PlayerContextFailures.Add(1)
		if errors.Is(err, browser.ErrUnplayable) {
			m.negCachePut(videoID, err)
			return browser.PlayerContext{}, err
		}
		return browser.PlayerContext{}, fmt.Errorf("minter: player-context failed after relaunch: %w", err)
	}
	m.metrics.PlayerContexts.Add(1)
	return pc, nil
}

// playerContextStop records a failed attempt and reports whether retries should
// stop. Terminal unplayable errors are cached, and canceled requests never cause
// a relaunch.
func (m *Minter) playerContextStop(ctx context.Context, videoID string, err error) bool {
	m.metrics.PlayerContextFailures.Add(1)
	if errors.Is(err, browser.ErrUnplayable) {
		m.negCachePut(videoID, err)
		return true
	}
	return ctx.Err() != nil
}

// negCacheGet returns a cached terminal error for videoID until its TTL expires.
func (m *Minter) negCacheGet(videoID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.negCache[videoID]
	if !ok {
		return nil
	}
	if time.Now().After(e.expiry) {
		delete(m.negCache, videoID)
		return nil
	}
	return e.err
}

// negCachePut remembers a terminal error for videoID for a short TTL. It removes
// expired entries first and evicts an arbitrary live entry if the map remains
// full.
func (m *Minter) negCachePut(videoID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if len(m.negCache) >= minterNegCacheMax {
		for k, e := range m.negCache {
			if now.After(e.expiry) {
				delete(m.negCache, k)
			}
		}
		for len(m.negCache) >= minterNegCacheMax { // all live: evict one to make room
			for k := range m.negCache {
				delete(m.negCache, k)
				break
			}
		}
	}
	m.negCache[videoID] = negEntry{err: err, expiry: now.Add(minterNegCacheTTL)}
}

func (m *Minter) cacheGet(key string) (browser.MintResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cache[key]
	if !ok || c.gen != m.gen || time.Now().After(c.expiry) {
		return browser.MintResult{}, false
	}
	return c.res, true
}

func (m *Minter) cachePut(key string, res browser.MintResult, gen uint64) {
	ttl := time.Duration(res.Lifetime) * time.Second
	if ttl <= 0 || ttl > minterMaxCacheTTL {
		ttl = minterMaxCacheTTL
	}
	if ttl -= minterCacheMargin; ttl < 0 {
		ttl = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.gen { // session was recycled mid-mint; don't cache a stale-gen token.
		return
	}
	m.cache[key] = cachedToken{res: res, expiry: time.Now().Add(ttl), gen: gen}
}

// Identity ensures a session and returns its captured identity (for /ping).
func (m *Minter) Identity(ctx context.Context) (browser.Identity, error) {
	s, _, err := m.ensure(ctx)
	if err != nil {
		return browser.Identity{}, err
	}
	return s.Identity(), nil
}

// Cookies ensures a session and returns its live youtube.com cookies (for
// /session, the coherence handoff).
func (m *Minter) Cookies(ctx context.Context) ([]*http.Cookie, error) {
	s, _, err := m.ensure(ctx)
	if err != nil {
		return nil, err
	}
	return s.BrowserCookies(), nil
}

// AttestKind ensures a session and returns its attestation grade.
func (m *Minter) AttestKind(ctx context.Context) (string, error) {
	s, _, err := m.ensure(ctx)
	if err != nil {
		return "", err
	}
	return s.AttestKind(), nil
}

// MetricsSnapshot returns counters and current state for the /metrics endpoint.
func (m *Minter) MetricsSnapshot() map[string]any {
	m.mu.Lock()
	gen := m.gen
	live := m.sess != nil
	kind := ""
	var ageSecs int
	if m.sess != nil {
		kind = m.sess.AttestKind()
		ageSecs = int(time.Since(m.attestedAt).Seconds())
	}
	cacheN := len(m.cache)
	m.mu.Unlock()
	return map[string]any{
		"generation":              gen,
		"session_live":            live,
		"attest_kind":             kind,
		"session_age_secs":        ageSecs,
		"cache_entries":           cacheN,
		"attestations":            m.metrics.Attestations.Load(),
		"mints":                   m.metrics.Mints.Load(),
		"mint_failures":           m.metrics.MintFailures.Load(),
		"escalations":             m.metrics.Escalations.Load(),
		"player_contexts":         m.metrics.PlayerContexts.Load(),
		"player_context_failures": m.metrics.PlayerContextFailures.Load(),
		"crashes":                 m.metrics.Crashes.Load(),
		"cache_hits":              m.metrics.CacheHits.Load(),
		"cache_misses":            m.metrics.CacheMisses.Load(),
		"launch_failures":         m.metrics.LaunchFailures.Load(),
	}
}

// Close tears down the live session.
func (m *Minter) Close() {
	m.mu.Lock()
	s := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s != nil {
		s.Close()
	}
}
