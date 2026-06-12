package minter

import (
	"context"
	"errors"
	"fmt"
	"github.com/colespringer/waxseal/internal/browser"
	"sync"
	"sync/atomic"
	"testing"
)

// newTestTenants builds a Tenants whose session factory is faked (no browser):
// each call yields a distinct fake session with a unique visitor_data, so tenant
// identity isolation is observable.
func newTestTenants(keys map[string]string) (*Tenants, *int64) {
	var calls int64
	tn := NewTenants(nil, "v", keys, browser.Options{})
	tn.newSession = func(context.Context, string) (minterSession, error) {
		n := atomic.AddInt64(&calls, 1)
		return &fakeSession{
			id: browser.Identity{VisitorData: fmt.Sprintf("vd-%d", n)},
			mint: func(string) (browser.MintResult, error) {
				return browser.MintResult{Kind: "integrity", Token: fmt.Sprintf("tok-%d", n), Lifetime: 3600}, nil
			},
		}, nil
	}
	return tn, &calls
}

// TestTenantsKeylessSharesOneTenant: with no keys, every request (any key) maps to
// the one default tenant and reuses its Minter.
func TestTenantsKeylessSharesOneTenant(t *testing.T) {
	tn, _ := newTestTenants(nil)
	m1, l1, err1 := tn.Minter("anything")
	m2, l2, err2 := tn.Minter("whatever")
	if err1 != nil || err2 != nil {
		t.Fatalf("keyless mode should accept any key: %v %v", err1, err2)
	}
	if m1 != m2 {
		t.Errorf("keyless mode should reuse one Minter")
	}
	if l1 != defaultTenant || l2 != defaultTenant {
		t.Errorf("labels = %q,%q, want both %q", l1, l2, defaultTenant)
	}
}

// TestTenantsMultiTenantIsolation verifies that keys select distinct identities,
// unknown keys are rejected, and repeated requests use the cache.
func TestTenantsMultiTenantIsolation(t *testing.T) {
	tn, calls := newTestTenants(map[string]string{"KEYA": "alice", "KEYB": "bob"})
	ctx := context.Background()

	ma, la, err := tn.Minter("KEYA")
	if err != nil || la != "alice" {
		t.Fatalf("KEYA resolved to %q with err=%v, want alice", la, err)
	}
	mb, lb, err := tn.Minter("KEYB")
	if err != nil || lb != "bob" {
		t.Fatalf("KEYB resolved to %q with err=%v, want bob", lb, err)
	}
	if ma == mb {
		t.Fatal("distinct tenants must get distinct Minters")
	}
	if _, _, err := tn.Minter("NOPE"); !errors.Is(err, ErrUnknownTenant) {
		t.Errorf("unknown key err = %v, want ErrUnknownTenant", err)
	}

	ra, _, err := ma.Mint(ctx, "gvs", "x")
	if err != nil {
		t.Fatalf("alice mint: %v", err)
	}
	rb, _, err := mb.Mint(ctx, "gvs", "x")
	if err != nil {
		t.Fatalf("bob mint: %v", err)
	}
	if ra.Token == rb.Token {
		t.Errorf("tenants minted identical tokens (%q); identities not isolated", ra.Token)
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("session creations = %d, want 2 (one attestation per tenant)", got)
	}

	// Alice's repeated request uses the cache without another attestation.
	if _, cached, _ := ma.Mint(ctx, "gvs", "x"); !cached {
		t.Errorf("alice repeat should be cached")
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("session creations = %d after repeat, want 2 (cache, no re-attest)", got)
	}
}

// TestTenantsConcurrent: concurrent requests across tenants are served without
// data races and create exactly one session per tenant.
func TestTenantsConcurrent(t *testing.T) {
	keys := map[string]string{"A": "a", "B": "b", "C": "c"}
	tn, calls := newTestTenants(keys)
	ctx := context.Background()

	var wg sync.WaitGroup
	for _, k := range []string{"A", "B", "C"} {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				m, _, err := tn.Minter(key)
				if err != nil {
					t.Errorf("Minter(%q): %v", key, err)
					return
				}
				if _, _, err := m.Mint(ctx, "gvs", "vd"); err != nil {
					t.Errorf("Mint(%q): %v", key, err)
				}
			}(k)
		}
	}
	wg.Wait()
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Errorf("session creations = %d, want 3 (one per tenant, single-flighted)", got)
	}
}
