package botguard

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/colespringer/waxseal/internal/httpx"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mkResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// clearInterpreterCache empties the process-wide interpreter cache.
func clearInterpreterCache() {
	interpCache.mu.Lock()
	defer interpCache.mu.Unlock()
	clear(interpCache.m)
}

func TestEndpointOrDefault(t *testing.T) {
	if got := (Endpoint{}).orDefault(); got != DefaultEndpoint {
		t.Errorf("zero Endpoint.orDefault() = %+v, want DefaultEndpoint", got)
	}
	custom := Endpoint{GenerateITURL: "https://x/GenerateIT"}
	if got := custom.orDefault(); got != custom {
		t.Errorf("non-zero Endpoint.orDefault() changed it to %+v", got)
	}
}

// TestInterpreterCacheReuse confirms a fetched interpreter is reused for the same
// hash (no second fetch), and a different hash fetches again.
func TestInterpreterCacheReuse(t *testing.T) {
	clearInterpreterCache()
	t.Cleanup(clearInterpreterCache)

	var fetches atomic.Int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/bg.js") {
			fetches.Add(1)
			return mkResp(200, "INTERP=1;"), nil
		}
		return mkResp(404, ""), nil
	})
	client := httpx.New(&http.Client{Transport: rt})
	client.MaxRetries = 0
	ctx := context.Background()
	const url = "https://www.google.com/js/bg.js"

	first := &Challenge{InterpreterURL: url, InterpreterHash: "hashA"}
	if err := ResolveInterpreter(ctx, client, first, ""); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if first.InterpreterJS != "INTERP=1;" {
		t.Fatalf("interpreter not resolved: %q", first.InterpreterJS)
	}

	// Same hash: served from cache, no second fetch.
	second := &Challenge{InterpreterURL: url, InterpreterHash: "hashA"}
	if err := ResolveInterpreter(ctx, client, second, ""); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if second.InterpreterJS != "INTERP=1;" {
		t.Fatalf("cached interpreter content wrong: %q", second.InterpreterJS)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1 (same hash served from cache)", got)
	}

	// A new hash fetches again.
	third := &Challenge{InterpreterURL: url, InterpreterHash: "hashB"}
	if err := ResolveInterpreter(ctx, client, third, ""); err != nil {
		t.Fatalf("third resolve: %v", err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2 (new hash refetches)", got)
	}
}

// TestInterpreterCacheURLKey confirms the URL is the cache key when att/get
// supplies no interpreterHash.
func TestInterpreterCacheURLKey(t *testing.T) {
	clearInterpreterCache()
	t.Cleanup(clearInterpreterCache)

	var fetches atomic.Int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		fetches.Add(1)
		return mkResp(200, "URLINTERP=1;"), nil
	})
	client := httpx.New(&http.Client{Transport: rt})
	client.MaxRetries = 0
	ctx := context.Background()
	const url = "https://www.youtube.com/s/player/abc/bg.js"

	for i := 0; i < 3; i++ {
		ch := &Challenge{InterpreterURL: url} // no hash
		if err := ResolveInterpreter(ctx, client, ch, ""); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1 (same URL cached)", got)
	}
}

// TestInterpreterInlineSkipsCache confirms an inline interpreter is never cached
// or fetched.
func TestInterpreterInlineSkipsCache(t *testing.T) {
	clearInterpreterCache()
	t.Cleanup(clearInterpreterCache)

	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("inline interpreter must not trigger a fetch")
		return nil, nil
	})
	client := httpx.New(&http.Client{Transport: rt})
	ch := &Challenge{InterpreterJS: "INLINE=1;", InterpreterHash: "hashA"}
	if err := ResolveInterpreter(context.Background(), client, ch, ""); err != nil {
		t.Fatalf("resolve inline: %v", err)
	}
}
