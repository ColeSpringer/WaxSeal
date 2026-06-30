package minter

import (
	"context"
	"fmt"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
)

// InjectSessionForTest installs sess as generation 1 for the tenant selected by
// apiKey and returns that tenant's Minter.
//
// Tests in dependent packages use it to exercise live-session handlers without
// launching Chromium. Production code must not call it.
func (t *Tenants) InjectSessionForTest(ctx context.Context, apiKey string, sess minterSession) (*Minter, error) {
	m, _, err := t.Minter(apiKey)
	if err != nil {
		return nil, err
	}
	m.launch = func(context.Context) (minterSession, error) { return sess, nil }
	if err := m.Warm(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// ExpireStreamingDeadlineForTest moves the current session's streaming deadline
// into the past, forcing the next streaming handoff to recycle without sleeping.
// If streaming-age recycling is disabled, it enables a test interval so the
// replacement session is armed. Production code must not call it.
func (m *Minter) ExpireStreamingDeadlineForTest() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streamingMaxAge <= 0 {
		m.streamingMaxAge = time.Hour
	}
	m.streamingDeadline = time.Now().Add(-time.Hour)
}

// FillCachePastBoundForTest fills the positive cache until at least one capacity
// eviction occurs. Dependent package tests use it to assert cache_evictions
// metrics without running 1024 mint operations. Production code must not call it.
func (m *Minter) FillCachePastBoundForTest() {
	gen := m.Generation()
	for i := 0; i <= minterCacheMax; i++ {
		m.cachePut(fmt.Sprintf("gvs|fill%05d", i), browser.MintResult{Lifetime: 3600}, gen)
	}
}
