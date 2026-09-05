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

// BrowserProber is the browser check behind /ping and its counters, as Tenants
// sees them. *browser.Pool is the production prober. Tests in dependent
// packages install a fake through SetBrowserProberForTest so a handler can be
// driven through every browser outcome, and its counters asserted, without
// Chromium.
type BrowserProber interface {
	// Health probes the shared Chromium, relaunching it when it has exited and
	// tearing it down first when it is wedged; see browser.Pool.Health.
	Health(ctx context.Context) (browser.Recovery, error)
	// ProbeFailures counts browsers Health confirmed unresponsive and tore down.
	ProbeFailures() int64
	// RelaunchFailures counts relaunch attempts whose launch failed.
	RelaunchFailures() int64
}

// noPool is the prober of a registry built without a pool, which only tests
// do. It has no browser to lose, so it answers and counts nothing.
type noPool struct{}

func (noPool) Health(context.Context) (browser.Recovery, error) { return browser.RecoveryNone, nil }
func (noPool) ProbeFailures() int64                             { return 0 }
func (noPool) RelaunchFailures() int64                          { return 0 }

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
	// newSession it is set before the registry serves and never after, so it
	// needs no lock; SetBrowserProberForTest replaces it under the same rule.
	prober BrowserProber

	mu      sync.Mutex
	keys    map[string]string  // API key to tenant label; only labels appear in logs and metrics
	minters map[string]*Minter // tenant label to lazily created Minter
	// closed marks the registry terminal, so a Minter created lazily after
	// shutdown is born closed rather than able to launch into a pool that is gone.
	closed bool
}

const defaultTenant = "default"

// NewTenants builds a registry over pool. Keys maps API keys to tenant labels. An
// empty map selects keyless single-tenant mode. streamingMaxAge and reportDebounce
// configure each tenant's Minter. mintSeparation, when positive, overrides the
// env-derived mint-to-establishment spacing for every tenant's Minter; a
// non-positive value leaves each Minter to resolve its own env-derived default.
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
		// gets its usual 502 or 503 rather than a spurious 401. What it gets is a
		// Minter that cannot launch against a pool that is gone, built and closed
		// here rather than registered: shutdown has already torn down every
		// registered Minter and will not run again, so registering this one would
		// leak it, log a tenant created after the daemon stopped, and inflate the
		// tenant count /metrics reports for the run.
		m := NewMinter(t.video, t.opts, t.streamingMaxAge, t.reportDebounce, t.mintSeparation)
		m.Close() // Close owns the terminal flag; there is no session to tear down.
		return m, label, nil
	}
	m := NewMinter(t.video, t.opts, t.streamingMaxAge, t.reportDebounce, t.mintSeparation)
	m.launch = func(ctx context.Context) (minterSession, error) {
		return t.newSession(ctx, t.video)
	}
	t.minters[label] = m
	t.mu.Unlock()
	t.log.Info("tenants: tenant minter created", "tenant", label)
	return m, label, nil
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

// BrowserHealth runs the browser check without touching any tenant, so a
// caller that presents no key on a keyed daemon can still learn whether the
// daemon has a running browser, and a tenant probe that found no answering page
// can tell a wedged browser from a page's own failure. It reports nil when a
// running Chromium answers, plus what it took to get one; see
// browser.Pool.Health for the policy.
func (t *Tenants) BrowserHealth(ctx context.Context) (browser.Recovery, error) {
	return t.prober.Health(ctx)
}

// BrowserProbeFailures counts the browsers BrowserHealth confirmed unresponsive
// and tore down, one per browser lost. It is daemon-wide, not per tenant.
func (t *Tenants) BrowserProbeFailures() int64 { return t.prober.ProbeFailures() }

// BrowserRelaunchFailures counts the relaunch attempts, from a probe or a
// request, whose launch failed. It is daemon-wide, not per tenant.
func (t *Tenants) BrowserRelaunchFailures() int64 { return t.prober.RelaunchFailures() }

// Keyed reports whether the registry runs in multi-tenant (keyed) mode. The key
// set is fixed in NewTenants and never mutated, so this needs no lock.
func (t *Tenants) Keyed() bool { return len(t.keys) > 0 }

// MetricsSnapshot returns per-tenant metrics plus the tenant count.
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
	}
}

// AggregateMetricsSnapshot returns the redacted /metrics body for keyed daemons
// when the request lacks the operator metrics key. It sums lifetime counters and
// omits labels, tenant count, and per-tenant state. The map is seeded from
// lifetimeCounterKeys so every counter is present even before any tenant has
// been used. It only iterates existing minters; a scrape never creates tenant
// state. The daemon-wide browser counters ride alongside the sums rather than
// among them: they describe the shared browser, not any tenant.
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
	}
}

// Close tears down every tenant Minter (disposing each context) and the shared
// browser, and makes the registry terminal. t.minters is left populated so
// MetricsSnapshot still reports what the daemon did before it stopped.
func (t *Tenants) Close() {
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
