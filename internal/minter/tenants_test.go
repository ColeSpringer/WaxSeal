package minter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/colespringer/waxseal/internal/browser"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newTestTenants builds a Tenants whose session factory is faked (no browser):
// each call yields a distinct fake session with a unique visitor_data, so tenant
// identity isolation is observable.
func newTestTenants(keys map[string]string) (*Tenants, *int64) {
	var calls int64
	tn := NewTenants(nil, "v", keys, browser.Options{}, 0, 0, 0)
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

// TestAggregateMetricsSnapshot covers the redacted aggregate shape, zero-seeded
// counters, and Keyed() for keyless and keyed registries.
func TestAggregateMetricsSnapshot(t *testing.T) {
	// Keyless: Keyed() is false; the aggregate still emits all counter keys at zero.
	keyless, _ := newTestTenants(nil)
	if keyless.Keyed() {
		t.Error("Keyed() = true for a keyless registry, want false")
	}
	emptyAgg := keyless.AggregateMetricsSnapshot()
	if emptyAgg["redacted"] != true {
		t.Errorf("redacted = %v, want true", emptyAgg["redacted"])
	}
	sums, ok := emptyAgg["aggregate"].(map[string]int64)
	if !ok {
		t.Fatalf("aggregate type = %T, want map[string]int64", emptyAgg["aggregate"])
	}
	if len(sums) != len(lifetimeCounterKeys) {
		t.Errorf("aggregate has %d keys with no minters, want %d (zero-seeded)", len(sums), len(lifetimeCounterKeys))
	}
	for _, k := range lifetimeCounterKeys {
		if v, present := sums[k]; !present || v != 0 {
			t.Errorf("aggregate[%q] = %v (present=%v), want 0", k, v, present)
		}
	}

	// Keyed with two tenants: Keyed() is true and counters sum across both.
	tn, _ := newTestTenants(map[string]string{"KA": "alice", "KB": "bob"})
	if !tn.Keyed() {
		t.Error("Keyed() = false for a keyed registry, want true")
	}
	ma, _, err := tn.Minter("KA")
	if err != nil {
		t.Fatalf("alice minter: %v", err)
	}
	mb, _, err := tn.Minter("KB")
	if err != nil {
		t.Fatalf("bob minter: %v", err)
	}
	ma.metrics.Mints.Add(3)
	ma.metrics.Crashes.Add(1)
	mb.metrics.Mints.Add(4)
	mb.metrics.PlayerContexts.Add(2)

	agg := tn.AggregateMetricsSnapshot()
	sums, _ = agg["aggregate"].(map[string]int64)
	if sums["mints"] != 7 {
		t.Errorf("aggregate mints = %d, want 7 (3+4)", sums["mints"])
	}
	if sums["crashes"] != 1 {
		t.Errorf("aggregate crashes = %d, want 1", sums["crashes"])
	}
	if sums["player_contexts"] != 2 {
		t.Errorf("aggregate player_contexts = %d, want 2", sums["player_contexts"])
	}

	// The redacted view leaks neither tenant identity, count, nor per-tenant data.
	for _, leak := range []string{"per_tenant", "tenants", "alice", "bob"} {
		if _, present := agg[leak]; present {
			t.Errorf("aggregate leaks top-level key %q", leak)
		}
	}
	raw, err := json.Marshal(agg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"alice", "bob", "per_tenant", "tenants"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("aggregate JSON leaks %q: %s", leak, raw)
		}
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

// A request that outlives the shutdown drain still resolves its key, so it must
// get a Minter (not a spurious 401), but that Minter must refuse to launch
// against a pool that is already closed. A lazily created one is born closed.
func TestTenantsCloseIsTerminal(t *testing.T) {
	tn, calls := newTestTenants(map[string]string{"KEYA": "alice", "KEYB": "bob"})
	ctx := context.Background()
	if err := tn.WarmOne(ctx, "KEYA"); err != nil {
		t.Fatalf("WarmOne: %v", err)
	}
	tn.Close()

	// An existing tenant: its Minter was closed by Close itself.
	if err := tn.WarmOne(ctx, "KEYA"); !errors.Is(err, ErrClosed) {
		t.Errorf("WarmOne on an existing tenant after Close = %v, want ErrClosed", err)
	}
	// A tenant created after Close: valid key, resolvable, but cannot launch.
	m, label, err := tn.Minter("KEYB")
	if err != nil {
		t.Fatalf("Minter(KEYB) after Close = %v, want a resolvable tenant", err)
	}
	if label != "bob" {
		t.Errorf("label = %q, want bob", label)
	}
	if err := m.Warm(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Warm on a tenant created after Close = %v, want ErrClosed", err)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("newSession calls = %d, want 1 (only the pre-Close warm)", got)
	}
	// The post-shutdown tenant is served but not registered. Shutdown has already
	// torn down every registered Minter and will not run again, so registering it
	// would leak it and count a tenant the daemon never ran. Close still leaves
	// the tenants that did run in place, so metrics report the run.
	snap := tn.MetricsSnapshot()
	if n, _ := snap["tenants"].(int); n != 1 {
		t.Errorf("tenants = %v, want 1 after Close (alice ran; bob never did)", snap["tenants"])
	}
	if _, ok := snap["per_tenant"].(map[string]any)["alice"]; !ok {
		t.Error("per_tenant lost alice, whose run metrics shutdown should still report")
	}
}
