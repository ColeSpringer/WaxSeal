//go:build e2e

package provider_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"sync"
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

// waxsealE2ELogLevelEnv optionally raises the in-process daemon's log level from
// the default info to debug, which also prints the reduced SABR URLs logged
// around the status-2 confirm path.
const waxsealE2ELogLevelEnv = "WAXSEAL_E2E_LOG_LEVEL"

// testDaemonLogger builds the in-process daemon's logger so its info-level
// diagnostics, including the status-2 confirm outcome logged around the
// preview-cap confirmation, reach "go test -v" output instead of server.New's
// default discard logger. Every browser session the daemon launches shares this
// logger, so both the warm-time proof and later per-request player-context calls
// are covered.
func testDaemonLogger(t *testing.T) *slog.Logger {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv(waxsealE2ELogLevelEnv), "debug") {
		level = slog.LevelDebug
	}
	w := &testLogWriter{t: t}
	t.Cleanup(w.close)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// testLogWriter adapts an io.Writer to t.Log, so each slog record becomes one
// test log line instead of going to the default discard handler. Writing through
// t.Log from a goroutine other than the test's own is safe as long as it happens
// before the test function returns; every write here happens inside a handler for
// a request the test is still synchronously waiting on, or during Warm, which the
// test also calls synchronously, so that normally holds. close is registered with
// t.Cleanup as a last-resort guard: once closed is set, Write drops the record
// instead of calling t.Log, so a daemon goroutine that logs after the test has
// already returned can never panic the test binary. close also waits for any
// Write already inside t.Log, so it cannot return while one is in flight.
type testLogWriter struct {
	t *testing.T

	mu     sync.Mutex
	closed bool
}

func (w *testLogWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	// The lock is held across t.Log, not just the closed check: releasing it first
	// would let close observe an unset flag, return, and allow the test to finish
	// while this call is still on its way into t.Log.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
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
	if cfg.Logger == nil {
		cfg.Logger = testDaemonLogger(t)
	}
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

// streamWEBContext builds a WaxTap client over the attested player-context path
// and streams videoURL to completion, reporting whether WaxTap fell back to
// plain WEB and the consumer-reported warnings. io.Copy reports bytes copied
// before an error alongside it, so n is the byte offset the stream reached
// before a truncation (often the consumer's own error, which already names the
// segment) stopped it. A copy error is reported here, with the byte offset,
// contentLength, and consumer warnings together; ok reports whether the stream
// completed without one, so a caller can skip requireFullLength's own floor
// check on the same byte count instead of reporting the identical truncation a
// second time.
func streamWEBContext(t *testing.T, ctx context.Context, p *provider.Provider, sess *potoken.Session, videoURL string) (n int64, info waxtap.StreamInfo, fellBack bool, warnings []string, ok bool) {
	t.Helper()
	var fb atomic.Bool
	var mu sync.Mutex
	capture := func(ev waxtap.Event) {
		if ev.Stage != waxtap.StageWarning || ev.Warning == nil {
			return
		}
		if ev.Warning.Code == waxtap.WarnWebContextFallback {
			fb.Store(true)
		}
		mu.Lock()
		warnings = append(warnings, fmt.Sprintf("code=%d %s", ev.Warning.Code, ev.Warning.Detail))
		mu.Unlock()
	}
	jar, _ := cookiejar.New(nil)
	tap, err := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Jar: jar, Timeout: 120 * time.Second},
		POTokenProvider:       p, // GVS token required by the WEB context
		PlayerContextProvider: p,
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
	var copyErr error
	n, copyErr = io.Copy(io.Discard, rc)
	mu.Lock()
	defer mu.Unlock()
	fellBack = fb.Load()
	if copyErr != nil {
		t.Errorf("read stream %s: truncated at byte offset %d (contentLength=%d): %v; %s",
			videoURL, n, info.ContentLength, copyErr, describeWarnings(warnings))
		return
	}
	ok = true
	return
}

// describeWarnings renders the warnings streamWEBContext collected for a failure
// message, or a placeholder when the caller never wired up an Events callback (so
// nil does not read as "the consumer reported nothing").
func describeWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return "warnings: none captured"
	}
	return "warnings: " + strings.Join(warnings, "; ")
}

// requireFullLength asserts a long-video stream cleared the status-2 preview cap.
// warnings is whatever streamWEBContext collected from the consumer during the
// stream; a truncation failure names the byte offset reached, the contentLength
// the consumer reported, and those warnings together, so the failure is readable
// without rerunning under a debugger.
func requireFullLength(t *testing.T, n int64, info waxtap.StreamInfo, label string, warnings []string) {
	t.Helper()
	consumerReported := describeWarnings(warnings)
	if n <= fullLengthFloor {
		t.Errorf("%s: truncated at byte offset %d (<= %d floor); contentLength=%d; %s",
			label, n, fullLengthFloor, info.ContentLength, consumerReported)
	}
	if info.ContentLength > 0 && n < int64(0.98*float64(info.ContentLength)) {
		t.Errorf("%s: truncated at byte offset %d, %.1f%% of contentLength %d; %s",
			label, n, 100*float64(n)/float64(info.ContentLength), info.ContentLength, consumerReported)
	}
}

// The player-context path must stream full length without an adopted session.
func TestPlayerContextOnlyFullLengthHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pcBefore := playerContexts(t, base)
	t.Logf("stream start (wall clock): %s", time.Now().Format("2006-01-02T15:04:05.000Z07:00"))
	n, info, fellBack, warnings, ok := streamWEBContext(t, ctx, p, nil, bbbURL)
	if fellBack {
		t.Errorf("WEB player-context fell back without an adopted session")
	}
	if info.Client != clientWebContext {
		t.Errorf("info.Client = %q, want %q (the player-context path)", info.Client, clientWebContext)
	}
	if pcAfter := playerContexts(t, base); pcAfter <= pcBefore {
		t.Errorf("player_contexts did not increase: before=%d after=%d", pcBefore, pcAfter)
	}
	if ok {
		requireFullLength(t, n, info, "player-context only", warnings)
	}
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
		t.Fatalf("read stream: truncated at byte offset %d (contentLength=%d): %v", n, info.ContentLength, rerr)
	}
	if info.Client != clientWeb {
		t.Errorf("info.Client = %q, want %q (plain WEB)", info.Client, clientWeb)
	}
	// This path streams through waxtap.New directly, with no Events callback, so
	// there are no consumer-reported warnings to pass here.
	requireFullLength(t, n, info, "session only", nil)
	t.Logf("session only: %d bytes (%s; contentLength=%d)", n, classifyStream(n, info.ContentLength), info.ContentLength)
}

// A proof completed on the landing video must apply to another long video.
func TestPlayerContextCrossVideoFullLengthHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The first request targets a different video from the session's landing page.
	n, info, fellBack, warnings, ok := streamWEBContext(t, ctx, p, nil, tearsURL)
	if fellBack {
		t.Errorf("WEB player-context fell back; establishment did not carry over to another video")
	}
	if info.Client != clientWebContext {
		t.Errorf("info.Client = %q, want %q", info.Client, clientWebContext)
	}
	if ok {
		requireFullLength(t, n, info, "cross-video player-context", warnings)
	}
	t.Logf("cross-video player-context (%s): %d bytes (%s; contentLength=%d)", tearsVideoID, n, classifyStream(n, info.ContentLength), info.ContentLength)
}

// A short first request must not prevent a later long video from streaming fully.
func TestPlayerContextShortThenLongHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The short video ends before the preview cap.
	t.Logf("short video stream start (wall clock): %s", time.Now().Format("2006-01-02T15:04:05.000Z07:00"))
	nShort, infoShort, fellBackShort, warningsShort, _ := streamWEBContext(t, ctx, p, nil, shortURL)
	if fellBackShort {
		t.Errorf("WEB player-context fell back for the short video")
	}
	if nShort <= 0 {
		t.Errorf("short video streamed no bytes")
	}
	t.Logf("short video first: %d bytes (%s; contentLength=%d; warnings=%v)", nShort, classifyStream(nShort, infoShort.ContentLength), infoShort.ContentLength, warningsShort)

	t.Logf("long video stream start (wall clock): %s", time.Now().Format("2006-01-02T15:04:05.000Z07:00"))
	nLong, infoLong, fellBackLong, warningsLong, okLong := streamWEBContext(t, ctx, p, nil, bbbURL)
	if fellBackLong {
		t.Errorf("WEB player-context fell back for the long video after a short first call")
	}
	if okLong {
		requireFullLength(t, nLong, infoLong, "long after short", warningsLong)
	}
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
	n, info, fellBack, warnings, ok := streamWEBContext(t, ctx, p, nil, bbbURL)
	if fellBack {
		t.Errorf("lazy tenant's first call fell back from the player-context path")
	}
	if info.Client != clientWebContext {
		t.Errorf("info.Client = %q, want %q", info.Client, clientWebContext)
	}
	if ok {
		requireFullLength(t, n, info, "lazy tenant first call", warnings)
	}
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
	t.Logf("stream start (wall clock): %s", time.Now().Format("2006-01-02T15:04:05.000Z07:00"))
	n, info, fellBack, warnings, ok := streamWEBContext(t, ctx, p, nil, tearsURL)
	if fellBack {
		t.Errorf("WEB player-context fell back; the default proof video did not establish the session")
	}
	if info.Client != clientWebContext {
		t.Errorf("info.Client = %q, want %q", info.Client, clientWebContext)
	}
	if ok {
		requireFullLength(t, n, info, "short landing video", warnings)
	}
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
	switch {
	case before.GenerationKnown && after.GenerationKnown:
		if after.Generation != before.Generation {
			t.Errorf("generation changed from %d to %d (a relaunch happened)", before.Generation, after.Generation)
		}
	default:
		t.Logf("generation not compared: this daemon redacts /metrics, which drops per-tenant state; the attestations check below covers the same relaunch")
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
