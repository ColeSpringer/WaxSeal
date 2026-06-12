//go:build e2e

package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
	"github.com/colespringer/waxseal/server"
	waxtap "github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/potoken"
)

// These manual e2e tests require Chromium and network access. Unless WAXSEAL_URL
// names an external daemon, each test starts a fresh daemon and browser session.
// Big Buck Bunny is used under its Creative Commons license.
const (
	bbbVideoID       = "aqz-KE-bpKQ"
	bbbURL           = "https://www.youtube.com/watch?v=" + bbbVideoID
	bbbContentLength = 30767611 // approximate reference size for logs
	fullLengthFloor  = 8 << 20  // safely beyond BBB's roughly 1 MB status-2 preview
)

// startColdDaemon starts an isolated in-process daemon or uses WAXSEAL_URL when
// set. Another process may claim the selected loopback port before server startup.
// waitDaemonReady reports that failure. Teardown is registered before startup so
// failures still close the browser.
func startColdDaemon(t *testing.T) string {
	t.Helper()
	if ext := os.Getenv("WAXSEAL_URL"); ext != "" {
		t.Logf("using external daemon at %s (WAXSEAL_URL)", ext)
		return ext
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv, err := server.New(server.Config{Addr: addr})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	// Register teardown before Warm/readiness so a t.Fatalf in either path still
	// shuts down the launched browser instead of leaking it.
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})

	warmCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := srv.Warm(warmCtx, ""); err != nil {
		t.Fatalf("warm cold daemon (browser attest): %v", err)
	}
	go func() { _ = srv.ListenAndServe() }()

	base := "http://" + addr
	waitDaemonReady(t, base)
	return base
}

// waitDaemonReady waits for the server goroutine to bind its listener.
func waitDaemonReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/metrics")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon at %s never became ready: %v", base, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// playerContexts sums the per-tenant counter because API keys do not expose their
// internal tenant labels.
func playerContexts(t *testing.T, base string) int64 {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var m struct {
		PerTenant map[string]struct {
			PlayerContexts int64 `json:"player_contexts"`
		} `json:"per_tenant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode /metrics: %v", err)
	}
	var total int64
	for _, v := range m.PerTenant {
		total += v.PlayerContexts
	}
	return total
}

// classifyStream uses the reported content length when available and a conservative
// byte threshold otherwise.
func classifyStream(n, contentLength int64) string {
	if contentLength > 0 {
		if n >= int64(0.98*float64(contentLength)) {
			return "full"
		}
		return "capped"
	}
	if n > fullLengthFloor {
		return "full"
	}
	return "capped"
}

// TestPlayerContextFullLengthHTTP verifies both full-length streaming and use of
// the player-context path. A successful fallback stream could also be full length,
// so byte count alone is insufficient.
func TestPlayerContextFullLengthHTTP(t *testing.T) {
	base := startColdDaemon(t)

	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sess, err := p.Session(ctx)
	if err != nil {
		t.Fatalf("session handoff: %v", err)
	}
	if sess.VisitorData == "" {
		t.Fatalf("daemon returned an empty visitor_data")
	}

	// Count calls to distinguish this path from a successful fallback.
	var pcCalls atomic.Int64
	countingPC := potoken.PlayerContextProviderFunc(func(ctx context.Context, videoID string) (potoken.PlayerContext, error) {
		pcCalls.Add(1)
		return p.ProvidePlayerContext(ctx, videoID)
	})

	var mu sync.Mutex
	var fellBack bool
	capture := func(ev waxtap.Event) {
		if ev.Stage == waxtap.StageWarning && ev.Warning != nil && ev.Warning.Code == waxtap.WarnWebContextFallback {
			mu.Lock()
			fellBack = true
			mu.Unlock()
		}
	}

	pcBefore := playerContexts(t, base)

	jar, _ := cookiejar.New(nil)
	tap, err := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Jar: jar, Timeout: 120 * time.Second},
		POTokenProvider:       p,          // GVS PO token for the WEB stream (required alongside a PC provider)
		PlayerContextProvider: countingPC, // the opt-in WEB SABR path under test
		Session:               sess,       // adopt the coherent identity unchanged
		Client:                "WEB",      // uniform chain (required for session adoption); also the fallback
	})
	if err != nil {
		t.Fatalf("waxtap.New: %v", err)
	}

	rc, info, err := tap.Stream(ctx, waxtap.Request{
		URL:         bbbURL,
		ProcessSpec: waxtap.ProcessSpec{Events: capture},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer rc.Close()

	n, rerr := io.Copy(io.Discard, rc)
	if rerr != nil {
		t.Fatalf("read stream: %v", rerr)
	}

	if got := pcCalls.Load(); got < 1 {
		t.Errorf("ProvidePlayerContext was not called (count=%d)", got)
	}
	mu.Lock()
	fb := fellBack
	mu.Unlock()
	if fb {
		t.Errorf("WEB player-context fell back (WarnWebContextFallback emitted); the player-context path did not hold")
	}
	if pcAfter := playerContexts(t, base); pcAfter <= pcBefore {
		t.Errorf("player_contexts did not increase across the call: before=%d after=%d", pcBefore, pcAfter)
	}
	// Content length can be under-reported, so only enforce lower bounds.
	if n <= fullLengthFloor {
		t.Errorf("streamed only %d bytes (<= %d floor); expected full-length past the ~70s cap", n, fullLengthFloor)
	}
	if info.ContentLength > 0 && n < int64(0.98*float64(info.ContentLength)) {
		t.Errorf("streamed %d bytes < 98%% of contentLength %d", n, info.ContentLength)
	}
	t.Logf("full-length stream: %d bytes (%s; contentLength=%d, reference=%d), ProvidePlayerContext calls=%d",
		n, classifyStream(n, info.ContentLength), info.ContentLength, bbbContentLength, pcCalls.Load())
}

// TestPlainWEBBaselineHTTP records the result of the plain WEB path without a
// player context. A capped stream is valid on an unfavored egress. Set
// WAXSEAL_EXPECT_PLAIN to "full" or "capped" to enforce a known environment.
func TestPlainWEBBaselineHTTP(t *testing.T) {
	base := startColdDaemon(t)

	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sess, err := p.Session(ctx)
	if err != nil {
		t.Fatalf("session handoff: %v", err)
	}
	if sess.VisitorData == "" {
		t.Fatalf("daemon returned an empty visitor_data")
	}

	jar, _ := cookiejar.New(nil)
	tap, err := waxtap.New(waxtap.Options{
		HTTPClient:      &http.Client{Jar: jar, Timeout: 120 * time.Second},
		POTokenProvider: p,     // GVS PO token only; no player-context provider
		Session:         sess,  // adopt the coherent identity verbatim
		Client:          "WEB", // uniform client chain is required for session adoption
	})
	if err != nil {
		t.Fatalf("waxtap.New: %v", err)
	}

	rc, info, err := tap.Stream(ctx, waxtap.Request{URL: bbbURL})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer rc.Close()

	n, rerr := io.Copy(io.Discard, rc)
	if rerr != nil {
		t.Fatalf("read stream: %v", rerr)
	}
	if n <= 0 {
		t.Fatalf("stream yielded no bytes (info=%+v)", info)
	}

	classification := classifyStream(n, info.ContentLength)
	t.Logf("plain WEB baseline: streamed %d bytes (%s; contentLength=%d)", n, classification, info.ContentLength)

	// A cap is valid here, but the stream must progress beyond initialization.
	if n < 256<<10 {
		t.Errorf("plain WEB did not stream past init: %d bytes", n)
	}
	if want := os.Getenv("WAXSEAL_EXPECT_PLAIN"); want != "" && want != classification {
		t.Errorf("WAXSEAL_EXPECT_PLAIN=%q but classified %q (n=%d, contentLength=%d)", want, classification, n, info.ContentLength)
	}
}

// escalationMetrics contains the counters used to detect an unnecessary relaunch.
// Values are summed so the test does not depend on the cold daemon's tenant label.
type escalationMetrics struct {
	Generation            int64
	Attestations          int64
	Escalations           int64
	PlayerContextFailures int64
}

func readEscalationMetrics(t *testing.T, base string) escalationMetrics {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var m struct {
		PerTenant map[string]struct {
			Generation            int64 `json:"generation"`
			Attestations          int64 `json:"attestations"`
			Escalations           int64 `json:"escalations"`
			PlayerContextFailures int64 `json:"player_context_failures"`
		} `json:"per_tenant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode /metrics: %v", err)
	}
	var out escalationMetrics
	for _, v := range m.PerTenant {
		out.Generation += v.Generation
		out.Attestations += v.Attestations
		out.Escalations += v.Escalations
		out.PlayerContextFailures += v.PlayerContextFailures
	}
	return out
}

// TestPlayerContextUnavailableFastHTTP verifies that unavailable videos fail
// without relaunching, are negatively cached, and do not affect the next valid
// video. The short first-call deadline catches regressions to the slow relaunch
// path.
func TestPlayerContextUnavailableFastHTTP(t *testing.T) {
	base := startColdDaemon(t)
	c := client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY")))

	// call measures a request made with an independent deadline.
	call := func(videoID string, d time.Duration) (*client.PlayerContext, error, time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		start := time.Now()
		pc, err := c.PlayerContext(ctx, videoID)
		return pc, err, time.Since(start)
	}

	requireUnavailable := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("dead id returned no error")
		}
		apiErr, ok := errors.AsType[*client.APIError](err)
		if !ok {
			t.Fatalf("error = %T, want *client.APIError; the slow relaunch path likely timed out: %v", err, err)
		}
		if apiErr.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", apiErr.StatusCode)
		}
		if apiErr.Code != client.CodeVideoUnavailable {
			t.Errorf("code = %q, want %q", apiErr.Code, client.CodeVideoUnavailable)
		}
		if apiErr.Details == "" {
			t.Error("details is empty, want the playabilityStatus")
		}
	}

	const deadID = "aaaaaaaaaaa" // well-formed but nonexistent

	before := readEscalationMetrics(t, base)

	// The first request must return a 422 without escalating. The 10-second
	// deadline catches the old relaunch path, which took about 80 seconds.
	_, err, elapsed := call(deadID, 10*time.Second)
	requireUnavailable(t, err)
	if elapsed > 9*time.Second {
		t.Errorf("dead id took %v, want well under the 10s deadline", elapsed)
	}
	t.Logf("dead id returned 422 in %v", elapsed)

	after := readEscalationMetrics(t, base)
	if after.Generation != before.Generation {
		t.Errorf("generation changed from %d to %d (a relaunch happened)", before.Generation, after.Generation)
	}
	if after.Attestations != before.Attestations {
		t.Errorf("attestations changed from %d to %d (a re-attest happened)", before.Attestations, after.Attestations)
	}
	if after.Escalations != before.Escalations {
		t.Errorf("escalations changed from %d to %d", before.Escalations, after.Escalations)
	}
	// player_context_failures counts failed attempts and negative-cache hits.
	if after.PlayerContextFailures <= before.PlayerContextFailures {
		t.Errorf("player_context_failures did not increase from %d to %d", before.PlayerContextFailures, after.PlayerContextFailures)
	}

	// A repeat request should be served from the negative cache.
	_, err2, elapsed2 := call(deadID, 10*time.Second)
	requireUnavailable(t, err2)
	if elapsed2 > 2*time.Second {
		t.Errorf("negative-cache repeat took %v, want near-instant", elapsed2)
	}
	t.Logf("dead id repeat (negative cache) in %v", elapsed2)

	// A valid ID immediately afterward must still establish.
	pc, err3, _ := call(bbbVideoID, 90*time.Second)
	if err3 != nil {
		t.Fatalf("good id after dead id: %v", err3)
	}
	if pc.Status != "OK" {
		t.Errorf("good id status = %q, want OK", pc.Status)
	}
}
