package minter

import (
	"context"
	"errors"
	"github.com/colespringer/waxseal/internal/browser"
	"log/slog"
	"sync"
	"time"
)

// ErrUnknownTenant is returned when a request presents an API key that is not
// registered (multi-tenant mode only).
var ErrUnknownTenant = errors.New("waxseal: unknown tenant API key")

// BrowserProber is the shared browser as Tenants sees it: the check behind
// /ping, the idle close, and their counters. *browser.Pool is the production
// prober. Tests in dependent packages install a fake through
// SetBrowserProberForTest to drive a handler through every browser outcome,
// and assert its counters, without Chromium.
type BrowserProber interface {
	// Health probes the shared Chromium, relaunching it when it has exited and
	// tearing it down first when it is wedged; see browser.Pool.Health.
	Health(ctx context.Context) (browser.Recovery, error)
	// ProbeFailures counts browsers Health confirmed unresponsive and tore down.
	ProbeFailures() int64
	// RelaunchFailures counts relaunch attempts whose launch failed.
	RelaunchFailures() int64
	// Suspend closes the shared Chromium if no session is using it, and reports
	// whether it did; see browser.Pool.Suspend.
	Suspend() bool
	// IdleCloses counts the browsers Suspend closed.
	IdleCloses() int64
}

// noPool is the prober of a registry built without a pool, which only tests
// do. It has no browser to lose, so it always answers healthy and counts zero.
type noPool struct{}

func (noPool) Health(context.Context) (browser.Recovery, error) { return browser.RecoveryNone, nil }
func (noPool) ProbeFailures() int64                             { return 0 }
func (noPool) RelaunchFailures() int64                          { return 0 }
func (noPool) Suspend() bool                                    { return false }
func (noPool) IdleCloses() int64                                { return 0 }

// Tenants routes API keys to isolated Minters. Each tenant has its own browser
// context, guest identity, cookies, and token cache. Tenant Minters are created
// on first use and run concurrently on separate pages in a shared browser.
//
// Keyless mode (no keys registered) keeps the bgutil wire unauthenticated for
// generic yt-dlp use: every request maps to one shared "default" tenant.
type Tenants struct {
	pool            *browser.Pool
	video           string
	opts            browser.Options
	log             *slog.Logger
	streamingMaxAge time.Duration // forwarded to each tenant Minter; 0 disables
	reportDebounce  time.Duration // forwarded to each tenant Minter; <=0 uses the default
	mintSeparation  time.Duration // forwarded to each tenant Minter; positive overrides the env-derived default

	// newSession creates an attested tenant session. Tests replace it to avoid
	// launching a browser.
	newSession func(ctx context.Context, videoID string) (minterSession, error)
	// prober is the browser check behind BrowserHealth and the browser counters
	// in the metrics views: the pool, or noPool when there is none. Like
	// newSession it is set only before the registry serves, so it needs no lock;
	// SetBrowserProberForTest follows the same rule.
	prober BrowserProber

	// idleTimeout is how long a session, and then the browser, may go unused
	// before closeIdle closes it; zero never does. created is where a browser no
	// tenant has used yet is measured from. idleStop ends the loop
	// CloseIdleAfter starts, and idleDone closes when it has returned.
	idleTimeout time.Duration
	created     time.Time
	idleStop    chan struct{}
	idleDone    chan struct{}
	stopIdle    sync.Once

	mu      sync.Mutex
	keys    map[string]string  // API key to tenant label; only labels appear in logs and metrics
	minters map[string]*Minter // tenant label to lazily created Minter
	// closed marks the registry terminal, so a Minter created lazily after
	// shutdown is born closed rather than able to launch into a pool that is gone.
	closed bool
}

const defaultTenant = "default"

// NewTenants builds a registry over pool. keys maps API keys to tenant labels;
// an empty map selects keyless single-tenant mode. streamingMaxAge,
// reportDebounce, and mintSeparation configure each tenant's Minter as in
// NewMinter: a positive mintSeparation overrides the env-derived
// mint-to-establishment spacing, and a non-positive one keeps it.
func NewTenants(pool *browser.Pool, video string, keys map[string]string, opts browser.Options, streamingMaxAge, reportDebounce, mintSeparation time.Duration) *Tenants {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	t := &Tenants{
		pool:            pool,
		video:           video,
		opts:            opts,
		log:             log,
		streamingMaxAge: streamingMaxAge,
		reportDebounce:  reportDebounce,
		mintSeparation:  mintSeparation,
		keys:            keys,
		minters:         make(map[string]*Minter),
		created:         time.Now(),
	}
	t.newSession = t.poolSession
	t.prober = noPool{}
	if pool != nil {
		t.prober = pool
	}
	return t
}

// poolSession is the default tenant session factory: a fresh isolated context,
// attested.
func (t *Tenants) poolSession(ctx context.Context, videoID string) (minterSession, error) {
	s, err := t.pool.NewSession(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if err := s.Attest(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// resolve maps an API key to a tenant label, enforcing auth in multi-tenant mode.
func (t *Tenants) resolve(apiKey string) (string, error) {
	if len(t.keys) == 0 {
		return defaultTenant, nil // keyless: one shared tenant
	}
	label, ok := t.keys[apiKey]
	if !ok {
		return "", ErrUnknownTenant
	}
	return label, nil
}

// Minter returns the (lazily created) Minter for the tenant the API key selects,
// plus the tenant label. In keyless mode any key resolves to the default tenant.
func (t *Tenants) Minter(apiKey string) (*Minter, string, error) {
	label, err := t.resolve(apiKey)
	if err != nil {
		return nil, "", err
	}
	t.mu.Lock()
	if m, ok := t.minters[label]; ok {
		t.mu.Unlock()
		return m, label, nil
	}
	if t.closed {
		t.mu.Unlock()
		// A request that outlives the shutdown drain still resolves its key, so it
		// gets its usual 502 or 503 rather than a spurious 401. Its Minter is built
		// closed and not registered: shutdown already tore down every registered
		// Minter and will not run again, so registering this one would leak it and
		// count a tenant in /metrics that the daemon never ran.
		m := t.newMinter()
		m.Close() // Close owns the terminal flag; there is no session to tear down.
		return m, label, nil
	}
	m := t.newMinter()
	t.minters[label] = m
	t.mu.Unlock()
	t.log.Info("tenants: tenant minter created", "tenant", label)
	return m, label, nil
}

// newMinter builds a tenant Minter that launches through this registry. Every
// tenant Minter is built here, including the one a post-Close request is handed,
// so none keeps NewMinter's default launcher, which would start a second
// Chromium outside the pool.
func (t *Tenants) newMinter() *Minter {
	m := NewMinter(t.video, t.opts, t.streamingMaxAge, t.reportDebounce, t.mintSeparation)
	m.launch = func(ctx context.Context) (minterSession, error) {
		return t.newSession(ctx, t.video)
	}
	return m
}

// idleCheckMax caps the interval between idle passes. The loop looks four
// times per timeout, so a close lands at most a quarter of the timeout late,
// and never more than this.
const idleCheckMax = 30 * time.Second

// CloseIdleAfter makes the registry close what goes unused for d: a tenant's
// session once no request has taken its page for that long, and the shared
// browser once that is true of every tenant. The request that next needs a
// session launches both again. Call it at most once, before the registry
// serves; a non-positive d leaves idle closing off.
func (t *Tenants) CloseIdleAfter(d time.Duration) {
	if d <= 0 {
		return
	}
	t.idleTimeout = d
	t.idleStop, t.idleDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(t.idleDone)
		tick := time.NewTicker(min(max(d/4, time.Millisecond), idleCheckMax))
		defer tick.Stop()
		for {
			select {
			case <-t.idleStop:
				return
			case <-tick.C:
				t.closeIdle()
			}
		}
	}()
}

// closeIdle is one idle pass: every tenant's session that has gone unused for
// the timeout, then the browser if the most recently used tenant has. A
// session retired for another reason leaves its tenant's last use standing,
// so the browser is not closed under a tenant about to ask again, and the
// pool itself refuses while any session is live or being created.
func (t *Tenants) closeIdle() {
	if t.idleTimeout <= 0 || t.idleStopped() {
		return
	}
	t.mu.Lock()
	ms := make([]*Minter, 0, len(t.minters))
	for _, m := range t.minters {
		ms = append(ms, m)
	}
	t.mu.Unlock()
	newest := t.created
	for _, m := range ms {
		if t.idleStopped() {
			return
		}
		m.closeIdle(t.idleTimeout)
		if used := m.lastUsed(); used.After(newest) {
			newest = used
		}
	}
	if time.Since(newest) >= t.idleTimeout && !t.idleStopped() {
		t.prober.Suspend()
	}
}

// StopIdleClose ends idle closing without waiting for a pass in flight, which
// stops at its next step. A daemon calls it as it begins to shut down, so no
// pass closes a browser that a request still draining would have to launch
// again. Close stops it too, and waits.
func (t *Tenants) StopIdleClose() {
	if t.idleStop != nil {
		t.stopIdle.Do(func() { close(t.idleStop) })
	}
}

// idleStopped reports whether StopIdleClose or Close has run.
func (t *Tenants) idleStopped() bool {
	if t.idleStop == nil {
		return false
	}
	select {
	case <-t.idleStop:
		return true
	default:
		return false
	}
}

// WarmOne attests the tenant selected by apiKey. Other tenants remain lazy.
func (t *Tenants) WarmOne(ctx context.Context, apiKey string) error {
	m, _, err := t.Minter(apiKey)
	if err != nil {
		return err
	}
	return m.Warm(ctx)
}

// SelfTestOne runs the startup mint and streaming checks for the selected tenant.
// Other tenants remain lazy.
func (t *Tenants) SelfTestOne(ctx context.Context, apiKey string) error {
	m, _, err := t.Minter(apiKey)
	if err != nil {
		return err
	}
	return m.SelfTest(ctx)
}

// CurrentBrowserPID returns the process ID of the shared Chromium process, or 0
// when no pool or browser is available.
func (t *Tenants) CurrentBrowserPID() int {
	if t.pool == nil {
		return 0
	}
	return t.pool.CurrentBrowserPID()
}

// BrowserHealth runs the browser check without touching any tenant, for a
// keyless probe on a keyed daemon or a failed tenant probe that must tell a
// wedged browser from its page's own failure. It returns nil when a running
// Chromium answers, with the Recovery it needed (see browser.Pool.Health).
func (t *Tenants) BrowserHealth(ctx context.Context) (browser.Recovery, error) {
	return t.prober.Health(ctx)
}

// BrowserProbeFailures counts the browsers BrowserHealth confirmed unresponsive
// and tore down, one per browser lost. It is daemon-wide, not per tenant.
func (t *Tenants) BrowserProbeFailures() int64 { return t.prober.ProbeFailures() }

// BrowserRelaunchFailures counts the relaunch attempts, from a probe or a
// request, whose launch failed. It is daemon-wide, not per tenant.
func (t *Tenants) BrowserRelaunchFailures() int64 { return t.prober.RelaunchFailures() }

// BrowserIdleCloses counts the times the shared browser was closed for going
// unused. It is daemon-wide, not per tenant.
func (t *Tenants) BrowserIdleCloses() int64 { return t.prober.IdleCloses() }

// Keyed reports whether the registry runs in multi-tenant (keyed) mode. The key
// set is fixed in NewTenants and never mutated, so this needs no lock.
func (t *Tenants) Keyed() bool { return len(t.keys) > 0 }

// MetricsSnapshot returns per-tenant metrics, the tenant count, and the
// daemon-wide browser counters.
func (t *Tenants) MetricsSnapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	per := make(map[string]any, len(t.minters))
	for label, m := range t.minters {
		per[label] = m.MetricsSnapshot()
	}
	return map[string]any{
		"tenants":                   len(t.minters),
		"per_tenant":                per,
		"browser_probe_failures":    t.BrowserProbeFailures(),
		"browser_relaunch_failures": t.BrowserRelaunchFailures(),
		"browser_idle_closes":       t.BrowserIdleCloses(),
	}
}

// AggregateMetricsSnapshot returns the redacted /metrics body for keyed daemons
// when the request lacks the operator metrics key: lifetime counters summed
// across existing tenants, with no labels, tenant count, or per-tenant state.
// Every counter is present, at zero before any tenant is used, and a scrape
// never creates tenant state. The daemon-wide browser counters sit beside the
// sums, not among them, since they describe the shared browser.
func (t *Tenants) AggregateMetricsSnapshot() map[string]any {
	sums := make(map[string]int64, len(lifetimeCounterKeys))
	for _, k := range lifetimeCounterKeys {
		sums[k] = 0
	}
	t.mu.Lock()
	for _, m := range t.minters {
		for k, v := range m.counterValues() {
			sums[k] += v
		}
	}
	t.mu.Unlock()
	return map[string]any{
		"redacted":                  true,
		"aggregate":                 sums,
		"browser_probe_failures":    t.BrowserProbeFailures(),
		"browser_relaunch_failures": t.BrowserRelaunchFailures(),
		"browser_idle_closes":       t.BrowserIdleCloses(),
	}
}

// Close tears down every tenant Minter (disposing each context) and the shared
// browser, and makes the registry terminal. t.minters is left populated so
// MetricsSnapshot still reports what the daemon did before it stopped.
func (t *Tenants) Close() {
	// The idle loop goes first, and its pass in flight is waited out, so none
	// runs against a pool that is closing.
	if t.idleStop != nil {
		t.StopIdleClose()
		<-t.idleDone
	}
	t.mu.Lock()
	t.closed = true
	ms := make([]*Minter, 0, len(t.minters))
	for _, m := range t.minters {
		ms = append(ms, m)
	}
	t.mu.Unlock()
	for _, m := range ms {
		m.Close()
	}
	t.pool.Close()
}
