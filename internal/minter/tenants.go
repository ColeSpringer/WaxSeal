package minter

import (
	"context"
	"errors"
	"github.com/colespringer/waxseal/internal/browser"
	"log/slog"
	"sync"
)

// ErrUnknownTenant is returned when a request presents an API key that is not
// registered (multi-tenant mode only).
var ErrUnknownTenant = errors.New("waxseal: unknown tenant API key")

// Tenants routes API keys to isolated Minters. Each tenant has its own browser
// context, guest identity, cookies, and token cache. Tenant Minters are created
// on first use and run concurrently on separate pages in a shared browser.
//
// Keyless mode (no keys registered) keeps the bgutil wire unauthenticated for
// generic yt-dlp use: every request maps to one shared "default" tenant.
type Tenants struct {
	pool  *browser.Pool
	video string
	opts  browser.Options
	log   *slog.Logger

	// newSession creates an attested tenant session. Tests replace it to avoid
	// launching a browser.
	newSession func(ctx context.Context, videoID string) (minterSession, error)

	mu      sync.Mutex
	keys    map[string]string  // API key to tenant label; only labels appear in logs and metrics
	minters map[string]*Minter // tenant label to lazily created Minter
}

const defaultTenant = "default"

// NewTenants builds a registry over pool. Keys maps API keys to tenant labels. An
// empty map selects keyless single-tenant mode.
func NewTenants(pool *browser.Pool, video string, keys map[string]string, opts browser.Options) *Tenants {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	t := &Tenants{
		pool:    pool,
		video:   video,
		opts:    opts,
		log:     log,
		keys:    keys,
		minters: make(map[string]*Minter),
	}
	t.newSession = t.poolSession
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
	defer t.mu.Unlock()
	m, ok := t.minters[label]
	if !ok {
		m = NewMinter(t.video, t.opts)
		m.launch = func(ctx context.Context) (minterSession, error) {
			return t.newSession(ctx, t.video)
		}
		t.minters[label] = m
		t.log.Info("tenants: tenant minter created", "tenant", label)
	}
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

// MetricsSnapshot returns per-tenant metrics plus the tenant count.
func (t *Tenants) MetricsSnapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	per := make(map[string]any, len(t.minters))
	for label, m := range t.minters {
		per[label] = m.MetricsSnapshot()
	}
	return map[string]any{
		"tenants":    len(t.minters),
		"per_tenant": per,
	}
}

// Close tears down every tenant Minter (disposing each context) and the shared
// browser.
func (t *Tenants) Close() {
	t.mu.Lock()
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
