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
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Minter adds token caching, single-flight attestation, retries, crash recovery,
// session recycling, and metrics to one browser identity. Mint and PlayerContext
// calls serialize because they share one page. Tenants manages multiple Minters.
type Minter struct {
	video           string
	opts            browser.Options
	log             *slog.Logger
	maxAge          time.Duration // recycle the session once it is older than this
	streamingMaxAge time.Duration // recycle on the next streaming handoff once older than this; 0 disables
	reportDebounce  time.Duration // minimum spacing between report-driven recycles

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

	// mu guards the streaming deadline, outstanding degradation report, and report
	// debounce state. A suspect mark must not outlive its generation.
	streamingDeadline    time.Time
	reportSuspectGen     uint64
	reportSuspectVideoID string
	lastReportRetireAt   time.Time

	mintMu  sync.Mutex // serializes the in-browser mint calls (single page)
	metrics minterMetrics
}

// minterSession is the part of browser.Session used by Minter. Tests replace it
// with an in-memory implementation.
type minterSession interface {
	Mint(ctx context.Context, identifier string) (browser.MintResult, error)
	PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, error)
	EnsureEstablished(ctx context.Context) error
	Ping(ctx context.Context) error
	AttestKind() string
	Identity() browser.Identity
	BrowserCookies(ctx context.Context) ([]*http.Cookie, error)
	Established() bool
	LastProof() (browser.FullLengthProbe, time.Time)
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
	Attestations   atomic.Int64
	LaunchFailures atomic.Int64
	Mints          atomic.Int64
	MintFailures   atomic.Int64 // per attempt (see minterMetrics doc)
	Escalations    atomic.Int64
	CacheHits      atomic.Int64
	CacheMisses    atomic.Int64
	CacheEvictions atomic.Int64 // positive-cache entries evicted at capacity
	// Crashes counts unexpected browser loss detected by CDP or a health probe.
	// Intentional session retirement does not count.
	Crashes               atomic.Int64
	PlayerContexts        atomic.Int64
	PlayerContextFailures atomic.Int64 // failed attempts and negative-cache hits
	// Status2Rejections counts refused requests where a player context could not
	// be confirmed beyond the status-2 cap. Attempts are counted by
	// PlayerContextFailures; this counter is once per request.
	Status2Rejections atomic.Int64

	// Session recycles are separated by cause.
	StreamingRecycles    atomic.Int64 // time-based recycle on a streaming handoff
	ReportDrivenRecycles atomic.Int64 // recycle triggered by a consumer degradation report

	// Consumer degradation reports, classified by disposition.
	DegradationReportsAccepted      atomic.Int64
	DegradationReportsRejectedStale atomic.Int64 // named an old or replaced generation
	DegradationReportsRateLimited   atomic.Int64 // rejected by the debounce
}

const (
	minterMaxCacheTTL   = 6 * time.Hour
	minterCacheMargin   = 5 * time.Minute // don't hand out a token within this of expiry
	minterDefaultMaxAge = 11 * time.Hour  // < the ~12h integrity lifetime
	minterNegCacheTTL   = 5 * time.Minute // remember an unplayable video_id this long
	minterNegCacheMax   = 256             // bound the negative cache
	// minterCacheMax bounds the positive token cache. Positive entries are keyed by
	// distinct video or visitor identities, so this cache is intentionally larger
	// than the negative cache. The bound keeps per-tenant memory predictable, about
	// 0.8 MB for typical entries and about 10 MB for unusually large tokens.
	minterCacheMax = 1024

	// DefaultReportDebounce limits report-driven re-attestation to 12 times per
	// hour.
	DefaultReportDebounce = 5 * time.Minute

	// pingProbeTimeout allows for a busy host without leaving /ping unbounded.
	pingProbeTimeout = 5 * time.Second

	// A short retry window tolerates transient startup failures without hiding a
	// persistent minting failure.
	selfTestMintAttempts = 3
)

// selfTestMintRetryDelay is variable so tests can shorten the retry interval.
var selfTestMintRetryDelay = 1 * time.Second

// ErrNoSession reports that the tenant has no existing attested session. It is
// exported so callers (e.g. server /ping) can distinguish the benign no-session
// state from a real probe failure.
var ErrNoSession = errors.New("waxseal: no attested session")

// NewMinter builds a single-identity minter for video (the landing watch id). It
// launches a browser only when an operation first needs a session.
// streamingMaxAge forces a fresh session on the next streaming handoff once the
// current one exceeds that age (0 disables); reportDebounce is the minimum spacing
// between report-driven recycles (<=0 uses DefaultReportDebounce).
func NewMinter(video string, opts browser.Options, streamingMaxAge, reportDebounce time.Duration) *Minter {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if reportDebounce <= 0 {
		reportDebounce = DefaultReportDebounce
	}
	m := &Minter{
		video:           video,
		opts:            opts,
		log:             log,
		maxAge:          minterDefaultMaxAge,
		streamingMaxAge: streamingMaxAge,
		reportDebounce:  reportDebounce,
		cache:           make(map[string]cachedToken),
		negCache:        make(map[string]negEntry),
	}
	m.launch = m.launchReal
	return m
}

// jitter varies d by up to 10 percent so a fleet of minters does not recycle in
// lockstep. Non-positive durations remain disabled.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
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
		// This path bypasses retire, so it must also clear the suspect mark.
		if m.sess != nil && m.maxAge > 0 && time.Since(m.attestedAt) > m.maxAge {
			old, gen, age := m.sess, m.gen, time.Since(m.attestedAt)
			m.sess = nil
			cancel := m.watchCancel
			m.watchCancel = nil
			m.reportSuspectGen = 0
			m.reportSuspectVideoID = ""
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
		// A generation bump invalidates every cached token. Under m.mu, no
		// new-generation cachePut can interleave before the clear, so every existing
		// entry belongs to the old session. Clear only the positive cache: an
		// unplayable video remains unplayable across a relaunch.
		clear(m.cache)
		m.attestedAt = time.Now()
		// Arm the streaming deadline for this generation.
		if m.streamingMaxAge > 0 {
			m.streamingDeadline = time.Now().Add(jitter(m.streamingMaxAge))
		}
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

// retire closes generation gen if it is current and reports whether it closed a
// session. It also clears any degradation report for that generation. If isCrash
// is true, it increments Crashes. The generation check makes concurrent
// retirement attempts idempotent.
func (m *Minter) retire(gen uint64, reason string, isCrash bool) bool {
	m.mu.Lock()
	if m.sess == nil || m.gen != gen {
		m.mu.Unlock()
		return false
	}
	old := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.reportSuspectGen = 0
	m.reportSuspectVideoID = ""
	if isCrash {
		m.metrics.Crashes.Add(1)
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.log.Warn("minter: retiring session", "gen", gen, "reason", reason)
	old.Close()
	return true
}

// watchCrash retires the session when its browser target crashes, detaches, or
// loses the CDP connection. That lets the next request relaunch instead of first
// failing against a dead session. Only browser.Session exposes the event stream;
// test fakes are ignored.
//
// The context is tied to the session lifetime, not the launch request. WaitCrash
// returns a reason on browser loss and "" when ctx is cancelled.
func (m *Minter) watchCrash(s minterSession, ctx context.Context, gen uint64) {
	real, ok := s.(*browser.Session)
	if !ok {
		return
	}
	reason := real.WaitCrash(ctx)
	// Intentional retirement cancels the watcher before closing the session.
	if ctx.Err() != nil {
		return
	}
	if reason == "" {
		return // no crash detected (e.g. the session had no live page)
	}
	m.retire(gen, reason, true)
}

// refreshStreamingSession replaces a stale or reported-degraded session before a
// streaming handoff. The caller must hold mintMu. Token-only requests bypass this
// check so they do not recycle an otherwise usable session.
func (m *Minter) refreshStreamingSession(ctx context.Context) (minterSession, uint64, error) {
	m.mu.Lock()
	cur := m.gen
	live := m.sess != nil
	suspect := live && m.reportSuspectGen == cur && cur != 0
	stale := live && !m.streamingDeadline.IsZero() && time.Now().After(m.streamingDeadline)
	m.mu.Unlock()

	if live && (suspect || stale) {
		// retire verifies that cur is still current.
		reason := "streaming session exceeded max age; relaunching"
		if suspect {
			reason = "consumer reported degradation; relaunching"
		}
		if m.retire(cur, reason, false) {
			if suspect {
				m.metrics.ReportDrivenRecycles.Add(1)
				// Deferred and immediate report-driven recycles share one debounce.
				m.mu.Lock()
				m.lastReportRetireAt = time.Now()
				m.mu.Unlock()
			} else {
				m.metrics.StreamingRecycles.Add(1)
			}
		}
	}
	return m.ensure(ctx)
}

// Generation returns the current session generation, or 0 before the first
// attestation. A consumer can pass it to ReportDegraded to name the exact session
// that produced a degraded context.
func (m *Minter) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

// ReportResult describes how the minter handled a degradation report. Accepted
// indicates that the report applies to the current session. Retired indicates
// that the session was closed immediately. RetirementPending indicates that it
// will be closed at the next streaming handoff. RetryAfterSeconds is set when the
// report was rate-limited.
type ReportResult struct {
	Accepted          bool
	Retired           bool
	RetirementPending bool
	Generation        uint64
	RetryAfterSeconds int
}

// ReportDegraded records that generation gen produced a degraded stream. videoID
// and reason are diagnostic. The report is rate-limited and applies only to the
// current generation. If a browser operation is in progress, retirement is
// deferred until the next streaming handoff.
func (m *Minter) ReportDegraded(gen uint64, videoID, reason string) ReportResult {
	// Marking the generation before releasing m.mu deduplicates concurrent reports.
	m.mu.Lock()
	cur := m.gen
	sinceLast := time.Since(m.lastReportRetireAt)
	switch {
	case m.sess == nil || gen != cur:
		// A report about an already-replaced session does nothing.
		m.mu.Unlock()
		m.metrics.DegradationReportsRejectedStale.Add(1)
		return ReportResult{Accepted: false, Generation: cur}
	case m.reportSuspectGen == gen:
		// Retirement is already queued for the next streaming handoff.
		m.mu.Unlock()
		return ReportResult{Accepted: true, RetirementPending: true, Generation: gen}
	case sinceLast < m.reportDebounce:
		// Recycled within the debounce window: tell the consumer how long to back off.
		retryAfter := ceilSeconds(m.reportDebounce - sinceLast)
		m.mu.Unlock()
		m.metrics.DegradationReportsRateLimited.Add(1)
		return ReportResult{Accepted: false, Generation: cur, RetryAfterSeconds: retryAfter}
	}
	m.reportSuspectGen = gen
	m.reportSuspectVideoID = videoID
	m.metrics.DegradationReportsAccepted.Add(1)
	m.mu.Unlock()

	// Only the first report for this generation attempts immediate retirement.
	if m.mintMu.TryLock() {
		acted := m.retire(gen, "consumer report: "+reason, false)
		m.mu.Lock()
		m.lastReportRetireAt = time.Now()
		m.mu.Unlock()
		if acted {
			m.metrics.ReportDrivenRecycles.Add(1)
		}
		m.mintMu.Unlock()
		return ReportResult{Accepted: true, Retired: acted, Generation: gen}
	}
	// A browser operation holds mintMu; defer retirement to the next handoff.
	return ReportResult{Accepted: true, RetirementPending: true, Generation: gen}
}

// ceilSeconds rounds a duration up to whole seconds.
func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// Mint returns a token for (scope, binding), reporting whether it came from cache.
// The retry policy serves cached tokens first, retries one failed mint in place,
// then relaunches and attests before the final attempt. Repeated requests for the
// same binding continue to use the cached token.
func (m *Minter) Mint(ctx context.Context, scope, binding string) (res browser.MintResult, cached bool, err error) {
	key := cacheKey(scope, binding)
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
		m.retire(gen, "mint failed twice; relaunching", false)
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
func (m *Minter) PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, uint64, error) {
	// A known-unplayable video fails before mintMu and the session, so a consumer
	// retrying a 502 (or a malicious caller) can't grind the tenant into relaunches.
	if err := m.negCacheGet(videoID); err != nil {
		m.metrics.PlayerContextFailures.Add(1)
		return browser.PlayerContext{}, 0, err
	}

	m.mintMu.Lock() // one page, so player-context calls serialize with mints
	defer m.mintMu.Unlock()

	sess, gen, err := m.refreshStreamingSession(ctx)
	if err != nil {
		return browser.PlayerContext{}, 0, err
	}

	pc, err := sess.PlayerContext(ctx, videoID)
	if err == nil {
		m.metrics.PlayerContexts.Add(1)
		return pc, gen, nil
	}
	if m.playerContextStop(ctx, videoID, err) { // terminal or cancelled: don't escalate.
		return browser.PlayerContext{}, gen, err
	}

	// level 1: transient failure, one in-place retry, no re-attest.
	m.log.Warn("minter: player-context failed; retrying on same session", "gen", gen, "err", err)
	pc, err = sess.PlayerContext(ctx, videoID)
	if err == nil {
		m.metrics.PlayerContexts.Add(1)
		return pc, gen, nil
	}
	if m.playerContextStop(ctx, videoID, err) {
		return browser.PlayerContext{}, gen, err
	}

	// Status-2 confirmation failures are timing related, not evidence of a dead
	// session. After the single in-place retry, refuse the request without
	// relaunching; a relaunch under load tends to worsen this condition.
	// playerContextStop has already counted both failed attempts.
	if errors.Is(err, browser.ErrStatus2Unconfirmed) {
		m.metrics.Status2Rejections.Add(1)
		return browser.PlayerContext{}, gen, err
	}

	// level 2: escalate to a relaunch and re-attest on a fresh session.
	m.metrics.Escalations.Add(1)
	m.retire(gen, "player-context failed twice; relaunching", false)
	sess, gen, err = m.ensure(ctx)
	if err != nil {
		return browser.PlayerContext{}, 0, err
	}
	pc, err = sess.PlayerContext(ctx, videoID)
	if err != nil {
		m.metrics.PlayerContextFailures.Add(1)
		if errors.Is(err, browser.ErrUnplayable) {
			m.negCachePut(videoID, err)
			return browser.PlayerContext{}, gen, err
		}
		return browser.PlayerContext{}, gen, fmt.Errorf("minter: player-context failed after relaunch: %w", err)
	}
	m.metrics.PlayerContexts.Add(1)
	return pc, gen, nil
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

// cacheKey returns the shared key format used by request and startup mints.
func cacheKey(scope, binding string) string { return scope + "|" + binding }

func (m *Minter) cacheGet(key string) (browser.MintResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cache[key]
	if !ok || c.gen != m.gen || time.Now().After(c.expiry) {
		if ok {
			// Generation mismatches should be rare because ensure clears the cache and
			// cachePut rejects stale writes. Keep the guard, and delete any unusable
			// entry reached here so expired tokens do not sit in the map until the next
			// generation.
			delete(m.cache, key)
		}
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
	now := time.Now() // one clock read, matching negCachePut
	// Only a new key can force eviction; refreshing a cached key must not remove
	// another token. First discard expired entries and track the live entry with the
	// earliest expiry. If the cache is still full, remove that entry so the new token
	// is retained and entries with more remaining lifetime stay available. The size
	// invariant means one eviction is enough.
	if _, exists := m.cache[key]; !exists && len(m.cache) >= minterCacheMax {
		var evictKey string
		var evictExp time.Time
		haveEvict := false
		for k, c := range m.cache {
			if now.After(c.expiry) {
				delete(m.cache, k)
				continue
			}
			if !haveEvict || c.expiry.Before(evictExp) {
				evictKey, evictExp, haveEvict = k, c.expiry, true
			}
		}
		if len(m.cache) >= minterCacheMax && haveEvict {
			delete(m.cache, evictKey)
			m.metrics.CacheEvictions.Add(1)
		}
	}
	m.cache[key] = cachedToken{res: res, expiry: now.Add(ttl), gen: gen}
}

// SessionSnapshot returns an established session's identity, cookies, and the
// producing generation. The operation holds mintMu so the values come from the
// same session generation, and it refreshes a stale or reported-degraded session
// first so a consumer adopts a fresh identity.
func (m *Minter) SessionSnapshot(ctx context.Context) (browser.Identity, []*http.Cookie, uint64, error) {
	m.mintMu.Lock()
	defer m.mintMu.Unlock()
	sess, gen, err := m.refreshStreamingSession(ctx)
	if err != nil {
		return browser.Identity{}, nil, 0, err
	}
	if err := sess.EnsureEstablished(ctx); err != nil {
		return browser.Identity{}, nil, 0, err
	}
	cookies, err := sess.BrowserCookies(ctx)
	if err != nil {
		return browser.Identity{}, nil, 0, err
	}
	return sess.Identity(), cookies, gen, nil
}

// HealthSnapshot is a consistent view of one session generation. Browser proof
// fields describe playback observed by the daemon. StreamingSuspect indicates
// that a consumer reported degradation.
type HealthSnapshot struct {
	Identity                browser.Identity
	AttestKind              string
	Generation              uint64
	BrowserProofEstablished bool
	LastBrowserProofOutcome string
	LastBrowserProofAt      time.Time
	StreamingSuspect        bool // a consumer reported this generation degraded
}

// Health probes the existing session and returns a consistent snapshot tied to
// one generation, plus whether a live session was found. It does not call ensure,
// so it cannot launch, attest, or recycle an expired session. On probe failure it
// retires the session only when mintMu is available, which prevents /ping from
// closing a session that is in use.
//
// If another goroutine replaces the session during the probe, Health retries once
// against the current session.
func (m *Minter) Health(ctx context.Context) (HealthSnapshot, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		m.mu.Lock()
		sess, gen := m.sess, m.gen
		m.mu.Unlock()
		if sess == nil {
			return HealthSnapshot{}, false, ErrNoSession
		}

		pctx, cancel := context.WithTimeout(ctx, pingProbeTimeout)
		err := sess.Ping(pctx)
		cancel()
		if err == nil {
			return m.healthSnapshot(sess, gen), true, nil
		}
		// Cancellation does not imply that the browser is dead.
		if ctx.Err() != nil {
			return HealthSnapshot{}, false, ctx.Err()
		}
		// Ignore a failure from a session that was replaced during the probe.
		m.mu.Lock()
		superseded := m.sess != sess || m.gen != gen
		m.mu.Unlock()
		if superseded && attempt == 0 {
			continue
		}
		if m.mintMu.TryLock() {
			m.retire(gen, "ping probe failed: "+err.Error(), true)
			m.mintMu.Unlock()
		}
		return HealthSnapshot{}, false, err
	}
	return HealthSnapshot{}, false, ErrNoSession
}

// healthSnapshot builds a HealthSnapshot for the probed (sess, gen). It reads the
// suspect mark under one m.mu acquisition tied to that generation so the snapshot
// never combines fields from different generations.
func (m *Minter) healthSnapshot(sess minterSession, gen uint64) HealthSnapshot {
	proof, proofAt := sess.LastProof()
	snap := HealthSnapshot{
		Identity:                sess.Identity(),
		AttestKind:              sess.AttestKind(),
		Generation:              gen,
		BrowserProofEstablished: sess.Established(),
		LastBrowserProofAt:      proofAt,
	}
	if !proofAt.IsZero() {
		snap.LastBrowserProofOutcome = proof.Outcome
	}
	m.mu.Lock()
	snap.StreamingSuspect = m.reportSuspectGen == gen && m.reportSuspectGen != 0
	m.mu.Unlock()
	return snap
}

// SelfTest mints and caches a GVS token for the current identity, then attempts
// full-length establishment. A persistent mint failure is returned. An
// establishment failure is logged and retried by the first endpoint that needs
// it. SelfTest retries minting in place and does not run the relaunch ladder.
func (m *Minter) SelfTest(ctx context.Context) error {
	m.mintMu.Lock()
	defer m.mintMu.Unlock()
	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return err
	}
	vd := sess.Identity().VisitorData

	var res browser.MintResult
	var mintErr error
	for attempt := 1; attempt <= selfTestMintAttempts; attempt++ {
		if res, mintErr = sess.Mint(ctx, vd); mintErr == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < selfTestMintAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(selfTestMintRetryDelay):
			}
		}
	}
	if mintErr != nil {
		return fmt.Errorf("minter: self-test mint failed after %d attempts: %w", selfTestMintAttempts, mintErr)
	}
	m.metrics.Mints.Add(1)
	m.cachePut(cacheKey("gvs", vd), res, gen)

	if err := sess.EnsureEstablished(ctx); err != nil {
		m.log.Warn("minter: self-test establishment failed; a later /session or /player-context request will retry", "err", err)
	}
	return nil
}

// lifetimeCounterKeys is the ordered set of process-lifetime counters returned
// by counterValues. Per-tenant metrics and the redacted aggregate both use this
// list, so their counter sets stay aligned.
var lifetimeCounterKeys = []string{
	"attestations",
	"mints",
	"mint_failures",
	"escalations",
	"player_contexts",
	"player_context_failures",
	"status2_rejections",
	"crashes",
	"cache_hits",
	"cache_misses",
	"cache_evictions",
	"launch_failures",
	"streaming_recycles",
	"report_driven_recycles",
	"degradation_reports_accepted",
	"degradation_reports_rejected_stale",
	"degradation_reports_rate_limited",
}

// counterValues returns each process-lifetime counter keyed by its metrics name.
// Its key set must match lifetimeCounterKeys.
func (m *Minter) counterValues() map[string]int64 {
	return map[string]int64{
		"attestations":                       m.metrics.Attestations.Load(),
		"mints":                              m.metrics.Mints.Load(),
		"mint_failures":                      m.metrics.MintFailures.Load(),
		"escalations":                        m.metrics.Escalations.Load(),
		"player_contexts":                    m.metrics.PlayerContexts.Load(),
		"player_context_failures":            m.metrics.PlayerContextFailures.Load(),
		"status2_rejections":                 m.metrics.Status2Rejections.Load(),
		"crashes":                            m.metrics.Crashes.Load(),
		"cache_hits":                         m.metrics.CacheHits.Load(),
		"cache_misses":                       m.metrics.CacheMisses.Load(),
		"cache_evictions":                    m.metrics.CacheEvictions.Load(),
		"launch_failures":                    m.metrics.LaunchFailures.Load(),
		"streaming_recycles":                 m.metrics.StreamingRecycles.Load(),
		"report_driven_recycles":             m.metrics.ReportDrivenRecycles.Load(),
		"degradation_reports_accepted":       m.metrics.DegradationReportsAccepted.Load(),
		"degradation_reports_rejected_stale": m.metrics.DegradationReportsRejectedStale.Load(),
		"degradation_reports_rate_limited":   m.metrics.DegradationReportsRateLimited.Load(),
	}
}

// MetricsSnapshot returns counters and current state for the /metrics endpoint.
//
// Session detail fields remain present after retirement so consumers can use one
// schema for live and not-live states. Fields that do not apply use sentinels:
// "" for string fields and nil map values, encoded as JSON null, for nullable
// numeric fields. streaming_seconds_until_recycle is present only when
// time-based recycling is enabled; absence means recycling is disabled.
func (m *Minter) MetricsSnapshot() map[string]any {
	m.mu.Lock()
	gen := m.gen
	sess := m.sess
	live := sess != nil
	kind := ""
	var ageSecs int
	suspect := live && m.reportSuspectGen == gen && m.reportSuspectGen != 0
	suspectVideo := ""
	if live {
		kind = sess.AttestKind()
		ageSecs = int(time.Since(m.attestedAt).Seconds())
		if suspect {
			suspectVideo = m.reportSuspectVideoID
		}
	}
	// The recycle field is controlled by static config. Its value comes from the
	// live session's armed deadline, read under m.mu. Ignore streamingDeadline
	// when no session is live because it may belong to an older generation.
	recycleEnabled := m.streamingMaxAge > 0
	var recycleSecs any // nil encodes as JSON null
	if recycleEnabled && live && !m.streamingDeadline.IsZero() {
		// Recycling waits for the next streaming handoff, so clamp an overdue
		// deadline to zero rather than reporting a negative remaining time.
		if secs := int(time.Until(m.streamingDeadline).Seconds()); secs > 0 {
			recycleSecs = secs
		} else {
			recycleSecs = 0
		}
	}
	cacheN := len(m.cache)
	m.mu.Unlock()

	// Read the session's proof state outside m.mu, as healthSnapshot does. The
	// session guards these fields with probeMu, and Close does not mutate them.
	var established bool
	var proofOutcome string
	var proofAge any // nil encodes as JSON null; int means an outcome time is known
	if live {
		established = sess.Established()
		if proof, proofAt := sess.LastProof(); !proofAt.IsZero() {
			proofOutcome, proofAge = proof.Outcome, int(time.Since(proofAt).Seconds())
		}
	}

	out := map[string]any{
		"generation":       gen,
		"session_live":     live,
		"attest_kind":      kind,
		"session_age_secs": ageSecs,
		"cache_entries":    cacheN,
		// Keep these detail fields present in every state. Use "" or null when the
		// field does not apply.
		"browser_proof_established":   established,
		"streaming_suspect":           suspect,
		"streaming_suspect_video":     suspectVideo,
		"last_browser_proof_outcome":  proofOutcome,
		"last_browser_proof_age_secs": proofAge,
	}
	for name, v := range m.counterValues() {
		out[name] = v
	}
	// Emit only when time-based recycling is enabled. A null value means recycling
	// is enabled but no live session has an armed deadline.
	if recycleEnabled {
		out["streaming_seconds_until_recycle"] = recycleSecs
	}
	return out
}

// Close tears down the live session.
func (m *Minter) Close() {
	m.mu.Lock()
	s := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.reportSuspectGen = 0
	m.reportSuspectVideoID = ""
	// Replace the maps rather than clearing them so their backing storage can be
	// reclaimed even if a closed Minter is retained. Tenant shutdown normally
	// releases the whole Minter, but Close should leave both caches empty on its own.
	m.cache = make(map[string]cachedToken)
	m.negCache = make(map[string]negEntry)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s != nil {
		s.Close()
	}
}
