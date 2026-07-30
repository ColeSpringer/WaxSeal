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
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
	"github.com/colespringer/waxseal/server"
	waxtap "github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/potoken"
)

// These manual e2e tests require Chromium and network access. Unless WAXSEAL_URL
// names an external daemon, each test starts a fresh daemon and browser session.
// All video IDs are freely licensed: Big Buck Bunny and Tears of Steel under
// Creative Commons (Blender Foundation); the NASA clip is U.S.-government public
// domain. The long videos exist only to seek past the status-2 preview cap.
const (
	bbbVideoID       = "aqz-KE-bpKQ" // Big Buck Bunny (Blender, CC-BY), approximately 635 seconds
	bbbURL           = "https://www.youtube.com/watch?v=" + bbbVideoID
	bbbContentLength = 30767611      // approximate reference size for logs
	tearsVideoID     = "R6MlUcmOul8" // Tears of Steel (Blender, CC-BY), approximately 734 seconds
	tearsURL         = "https://www.youtube.com/watch?v=" + tearsVideoID
	shortVideoID     = "1UaBgr_sq9A" // NASA: 60 Years in 60 Seconds (public domain), approximately 60 seconds
	shortURL         = "https://www.youtube.com/watch?v=" + shortVideoID
	fullLengthFloor  = 8 << 20 // safely beyond a status-2 preview of a long video

	clientWebContext = "WEB_CONTEXT" // info.Client when the attested player-context path is used
	clientWeb        = "WEB"         // info.Client for the plain WEB chain
)

// startColdDaemon uses WAXSEAL_URL when set. Otherwise, it starts an isolated
// keyless daemon and warms one session.
//
// The in-process path omits SelfTest so the first endpoint call exercises on-demand
// establishment.
func startColdDaemon(t *testing.T) string {
	t.Helper()
	if ext := os.Getenv("WAXSEAL_URL"); ext != "" {
		t.Logf("using external daemon at %s (WAXSEAL_URL)", ext)
		return ext
	}
	srv, addr := newInProcessDaemon(t, server.Config{})
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

// newInProcessDaemon selects a loopback address and registers server cleanup. The
// address is not reserved after selection, so waitDaemonReady reports any bind
// failure. The caller is responsible for warming and serving the daemon.
func newInProcessDaemon(t *testing.T, cfg server.Config) (*server.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg.Addr = addr
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})
	return srv, addr
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

// classifyStream uses the reported content length when available and a
// conservative byte threshold otherwise.
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

// streamWEBContext streams through the attested player-context path and reports
// whether WaxTap fell back to plain WEB.
func streamWEBContext(t *testing.T, ctx context.Context, p *provider.Provider, sess *potoken.Session, videoURL string) (int64, waxtap.StreamInfo, bool) {
	t.Helper()
	var fellBack atomic.Bool
	capture := func(ev waxtap.Event) {
		if ev.Stage == waxtap.StageWarning && ev.Warning != nil && ev.Warning.Code == waxtap.WarnWebContextFallback {
			fellBack.Store(true)
		}
	}
	jar, _ := cookiejar.New(nil)
	tap, err := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Jar: jar, Timeout: 120 * time.Second},
		POTokenProvider:       p, // GVS token required by the WEB context
		PlayerContextProvider: p, // attested WEB SABR path under test
		Session:               sess,
		Client:                clientWeb, // the fallback chain; the PC path is preferred
	})
	if err != nil {
		t.Fatalf("waxtap.New: %v", err)
	}
	rc, info, err := tap.Stream(ctx, waxtap.Request{URL: videoURL, ProcessSpec: waxtap.ProcessSpec{Events: capture}})
	if err != nil {
		t.Fatalf("stream %s: %v", videoURL, err)
	}
	defer rc.Close()
	n, rerr := io.Copy(io.Discard, rc)
	if rerr != nil {
		t.Fatalf("read stream %s: %v", videoURL, rerr)
	}
	return n, info, fellBack.Load()
}

// requireFullLength asserts a long-video stream cleared the status-2 preview cap.
func requireFullLength(t *testing.T, n int64, info waxtap.StreamInfo, label string) {
	t.Helper()
	if n <= fullLengthFloor {
		t.Errorf("%s: streamed only %d bytes (<= %d floor); expected data past the roughly 70-second cap", label, n, fullLengthFloor)
	}
	if info.ContentLength > 0 && n < int64(0.98*float64(info.ContentLength)) {
		t.Errorf("%s: streamed %d bytes < 98%% of contentLength %d", label, n, info.ContentLength)
	}
}

// The player-context path must stream full length without an adopted session.
func TestPlayerContextOnlyFullLengthHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pcBefore := playerContexts(t, base)
	n, info, fellBack := streamWEBContext(t, ctx, p, nil, bbbURL)
	if fellBack {
		t.Errorf("WEB player-context fell back without an adopted session")
	}
	if info.Client != clientWebContext {
		t.Errorf("info.Client = %q, want %q (the player-context path)", info.Client, clientWebContext)
	}
	if pcAfter := playerContexts(t, base); pcAfter <= pcBefore {
		t.Errorf("player_contexts did not increase: before=%d after=%d", pcBefore, pcAfter)
	}
	requireFullLength(t, n, info, "player-context only")
	t.Logf("player-context only: %d bytes (%s; contentLength=%d, reference=%d)", n, classifyStream(n, info.ContentLength), info.ContentLength, bbbContentLength)
}

// An adopted session and GVS token must stream full length without a
// player-context provider.
func TestSessionOnlyFullLengthHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sess, err := p.ProvideSession(ctx)
	if err != nil {
		t.Fatalf("session handoff: %v", err)
	}
	if sess.VisitorData == "" {
		t.Fatalf("daemon returned an empty visitor_data")
	}
	// Without a generation the daemon's session cannot be named in a report, so a
	// delivery cap on it would have no escape.
	if sess.Generation == 0 {
		t.Fatalf("daemon returned no session_generation")
	}

	jar, _ := cookiejar.New(nil)
	tap, err := waxtap.New(waxtap.Options{
		HTTPClient:      &http.Client{Jar: jar, Timeout: 120 * time.Second},
		POTokenProvider: p,         // GVS token only; no player-context provider
		SessionProvider: p,         // the adoption arm WaxTap can invalidate when googlevideo caps it
		Client:          clientWeb, // uniform client chain is required for session adoption
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
	if info.Client != clientWeb {
		t.Errorf("info.Client = %q, want %q (plain WEB)", info.Client, clientWeb)
	}
	requireFullLength(t, n, info, "session only")
	t.Logf("session only: %d bytes (%s; contentLength=%d)", n, classifyStream(n, info.ContentLength), info.ContentLength)
}

// A proof completed on the landing video must apply to another long video.
func TestPlayerContextCrossVideoFullLengthHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The first request targets a different video from the session's landing page.
	n, info, fellBack := streamWEBContext(t, ctx, p, nil, tearsURL)
	if fellBack {
		t.Errorf("WEB player-context fell back; establishment did not carry over to another video")
	}
	if info.Client != clientWebContext {
		t.Errorf("info.Client = %q, want %q", info.Client, clientWebContext)
	}
	requireFullLength(t, n, info, "cross-video player-context")
	t.Logf("cross-video player-context (%s): %d bytes (%s; contentLength=%d)", tearsVideoID, n, classifyStream(n, info.ContentLength), info.ContentLength)
}

// A short first request must not prevent a later long video from streaming fully.
func TestPlayerContextShortThenLongHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The short video ends before the preview cap.
	nShort, infoShort, fellBackShort := streamWEBContext(t, ctx, p, nil, shortURL)
	if fellBackShort {
		t.Errorf("WEB player-context fell back for the short video")
	}
	if nShort <= 0 {
		t.Errorf("short video streamed no bytes")
	}
	t.Logf("short video first: %d bytes (%s; contentLength=%d)", nShort, classifyStream(nShort, infoShort.ContentLength), infoShort.ContentLength)

	nLong, infoLong, fellBackLong := streamWEBContext(t, ctx, p, nil, bbbURL)
	if fellBackLong {
		t.Errorf("WEB player-context fell back for the long video after a short first call")
	}
	requireFullLength(t, nLong, infoLong, "long after short")
}

// A lazy tenant's first player-context request must establish on demand.
func TestLazyTenantFirstCallFullLengthHTTP(t *testing.T) {
	if ext := os.Getenv("WAXSEAL_URL"); ext != "" {
		t.Skip("lazy-tenant test requires an in-process daemon")
	}
	const warmKey, lazyKey = "KEYWARM", "KEYLAZY"
	// TenantKeys maps API key to tenant label (see server.Config), so key the map by
	// the API key, not the label.
	srv, addr := newInProcessDaemon(t, server.Config{TenantKeys: map[string]string{warmKey: "warm", lazyKey: "lazy"}})
	warmCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	if err := srv.Warm(warmCtx, warmKey); err != nil { // warm only the "warm" tenant
		cancel()
		t.Fatalf("warm warm-tenant: %v", err)
	}
	cancel()
	go func() { _ = srv.ListenAndServe() }()
	base := "http://" + addr
	waitDaemonReady(t, base)

	p := provider.New(client.New(base, client.WithAPIKey(lazyKey)))
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel2()
	n, info, fellBack := streamWEBContext(t, ctx, p, nil, bbbURL)
	if fellBack {
		t.Errorf("lazy tenant's first call fell back from the player-context path")
	}
	if info.Client != clientWebContext {
		t.Errorf("info.Client = %q, want %q", info.Client, clientWebContext)
	}
	requireFullLength(t, n, info, "lazy tenant first call")
}

// A short landing video must fall back to the default proof video.
func TestShortLandingVideoEstablishesHTTP(t *testing.T) {
	if ext := os.Getenv("WAXSEAL_URL"); ext != "" {
		t.Skip("short-landing-video test requires an in-process daemon")
	}
	srv, addr := newInProcessDaemon(t, server.Config{Video: shortVideoID})
	warmCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	if err := srv.Warm(warmCtx, ""); err != nil {
		cancel()
		t.Fatalf("warm with a short landing video: %v", err)
	}
	cancel()
	go func() { _ = srv.ListenAndServe() }()
	base := "http://" + addr
	waitDaemonReady(t, base)

	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel2()
	n, info, fellBack := streamWEBContext(t, ctx, p, nil, tearsURL)
	if fellBack {
		t.Errorf("WEB player-context fell back; the default proof video did not establish the session")
	}
	if info.Client != clientWebContext {
		t.Errorf("info.Client = %q, want %q", info.Client, clientWebContext)
	}
	requireFullLength(t, n, info, "short landing video")
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

	// Allow time for first-use establishment while still detecting the old
	// relaunch path, which took about 80 seconds.
	_, err, elapsed := call(deadID, 60*time.Second)
	requireUnavailable(t, err)
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
	if pc.PlayabilityStatus != "OK" {
		t.Errorf("good id playability_status = %q, want OK", pc.PlayabilityStatus)
	}
}
