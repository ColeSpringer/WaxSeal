//go:build e2e

package provider_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
	"github.com/colespringer/waxseal/server"
	waxtap "github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/youtube"
	"google.golang.org/protobuf/encoding/protowire"
)

// These manual e2e tests require Chromium and network access. Unless WAXSEAL_URL
// names an external daemon, each test starts a fresh daemon and browser session.
// All video IDs are freely licensed: Big Buck Bunny and Tears of Steel under
// Creative Commons (Blender Foundation); the NASA clip is U.S.-government public
// domain. The long videos run well past the status-2 preview cap, so a capped
// stream shows as a truncation.
const (
	bbbVideoID      = "aqz-KE-bpKQ" // Big Buck Bunny (Blender, CC-BY), approximately 635 seconds
	bbbURL          = "https://www.youtube.com/watch?v=" + bbbVideoID
	tearsVideoID    = "R6MlUcmOul8" // Tears of Steel (Blender, CC-BY), approximately 734 seconds
	tearsURL        = "https://www.youtube.com/watch?v=" + tearsVideoID
	shortVideoID    = "1UaBgr_sq9A" // NASA: 60 Years in 60 Seconds (public domain), approximately 60 seconds
	shortURL        = "https://www.youtube.com/watch?v=" + shortVideoID
	fullLengthFloor = 8 << 20 // safely beyond a status-2 preview of a long video

	clientWebContext = "WEB_CONTEXT" // info.Client when the attested player-context path is used
	clientWeb        = "WEB"         // info.Client for the plain WEB chain
)

// startColdDaemon uses WAXSEAL_URL when set. Otherwise it starts an isolated
// keyless daemon and warms one session, without SelfTest, so the first endpoint
// call exercises on-demand establishment.
func startColdDaemon(t *testing.T) string {
	t.Helper()
	if ext := os.Getenv("WAXSEAL_URL"); ext != "" {
		t.Logf("using external daemon at %s (WAXSEAL_URL)", ext)
		return ext
	}
	srv, addr, ln := newInProcessDaemon(t, server.Config{})
	warmCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := srv.Warm(warmCtx, ""); err != nil {
		t.Fatalf("warm cold daemon (browser attest): %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	base := "http://" + addr
	waitDaemonReady(t, base)
	return base
}

// waxsealE2ELogLevelEnv optionally raises the in-process daemon's log level from
// the default info to debug, which also prints the reduced SABR URLs logged
// around the status-2 confirm path.
const waxsealE2ELogLevelEnv = "WAXSEAL_E2E_LOG_LEVEL"

// testDaemonLogger routes the in-process daemon's logs, including the status-2
// confirm outcome, to "go test -v" output instead of the server's default
// discard logger. Every browser session the daemon launches shares it, so
// warm-up and every later request log here.
func testDaemonLogger(t *testing.T) *slog.Logger {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv(waxsealE2ELogLevelEnv), "debug") {
		level = slog.LevelDebug
	}
	w := &testLogWriter{t: t}
	t.Cleanup(w.close)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// testLogWriter is an io.Writer over t.Log, so each slog record becomes one
// test log line. Calling t.Log after the test returns panics. The daemon
// normally logs while the test waits on Warm or a request; close, registered
// with t.Cleanup, covers a goroutine that logs later: once closed is set, Write
// drops the record, and close waits for any Write already inside t.Log.
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
	// Hold the lock across t.Log, not only the check, so close cannot return
	// while this call is still on its way into t.Log.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// newInProcessDaemon binds a loopback listener and registers server cleanup. The
// listener comes back open for the caller to hand to srv.Serve after warming;
// its close is a cleanup here because a failed warm-up returns before Serve
// owns it. Holding the port from the start keeps anything else from taking it.
func newInProcessDaemon(t *testing.T, cfg server.Config) (*server.Server, string, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab free port: %v", err)
	}
	// Registered before the server's cleanup, so it runs after Shutdown; it is a
	// no-op unless Serve never ran.
	t.Cleanup(func() { _ = ln.Close() })
	addr := ln.Addr().String()

	cfg.Addr = addr
	if cfg.Logger == nil {
		cfg.Logger = testDaemonLogger(t)
	}
	srv, err := server.NewWithContext(context.Background(), cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})
	return srv, addr, ln
}

// waitDaemonReady waits for the server goroutine to start serving. Each attempt
// has its own timeout: the listener is already bound, so a request sent before
// Serve starts waits in the backlog instead of being refused, and without one
// the deadline below would never be reached.
func waitDaemonReady(t *testing.T, base string) {
	t.Helper()
	hc := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := hc.Get(base + "/metrics")
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
// plain WEB and the consumer's warnings. A copy error is reported here, with n
// (the byte offset reached), contentLength, and the warnings, and leaves ok
// false so the caller can skip requireFullLength instead of reporting the same
// truncation twice.
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

// describeWarnings renders collected warnings for a failure message. An empty
// list reads "none captured", since a caller without an Events callback
// collects nothing even when the consumer warned.
func describeWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return "warnings: none captured"
	}
	return "warnings: " + strings.Join(warnings, "; ")
}

// requireFullLength asserts a stream reached 98% of the consumer's
// contentLength and cleared fullLengthFloor, which also covers an unknown
// contentLength. A known contentLength under the floor, a short video, skips
// the floor. A failure names the byte offset reached, contentLength, and the
// warnings streamWEBContext collected.
func requireFullLength(t *testing.T, n int64, info waxtap.StreamInfo, label string, warnings []string) {
	t.Helper()
	consumerReported := describeWarnings(warnings)
	if n <= fullLengthFloor && (info.ContentLength <= 0 || info.ContentLength > fullLengthFloor) {
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
	t.Logf("player-context only: %d bytes (%s; contentLength=%d)", n, classifyStream(n, info.ContentLength), info.ContentLength)
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
	// No Events callback on this path, so there are no warnings to pass.
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

// A short first request must stream in full and must not keep a later long
// video from streaming fully.
func TestPlayerContextShortThenLongHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The short video ends before the preview cap.
	t.Logf("short video stream start (wall clock): %s", time.Now().Format("2006-01-02T15:04:05.000Z07:00"))
	nShort, infoShort, fellBackShort, warningsShort, okShort := streamWEBContext(t, ctx, p, nil, shortURL)
	if fellBackShort {
		t.Errorf("WEB player-context fell back for the short video")
	}
	if okShort {
		// Without a contentLength, requireFullLength would judge this ~1 MB video
		// by the long-video floor, so name what is missing instead.
		if infoShort.ContentLength <= 0 {
			t.Errorf("short video first: contentLength unknown, so full length cannot be checked (%d bytes streamed); %s",
				nShort, describeWarnings(warningsShort))
		} else {
			requireFullLength(t, nShort, infoShort, "short video first", warningsShort)
		}
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
	// TenantKeys maps API key to tenant label, not the reverse.
	srv, addr, ln := newInProcessDaemon(t, server.Config{TenantKeys: map[string]string{warmKey: "warm", lazyKey: "lazy"}})
	warmCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	if err := srv.Warm(warmCtx, warmKey); err != nil { // warm only the "warm" tenant
		cancel()
		t.Fatalf("warm warm-tenant: %v", err)
	}
	cancel()
	go func() { _ = srv.Serve(ln) }()
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
	srv, addr, ln := newInProcessDaemon(t, server.Config{Video: shortVideoID})
	warmCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	if err := srv.Warm(warmCtx, ""); err != nil {
		cancel()
		t.Fatalf("warm with a short landing video: %v", err)
	}
	cancel()
	go func() { _ = srv.Serve(ln) }()
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
	call := func(videoID string, d time.Duration) (*client.PlayerContext, time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		start := time.Now()
		pc, err := c.PlayerContext(ctx, videoID)
		return pc, time.Since(start), err
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

	// 60 s fits first-use establishment; the slow relaunch path takes about 80 s.
	_, elapsed, err := call(deadID, 60*time.Second)
	requireUnavailable(t, err)
	t.Logf("dead id returned 422 in %v", elapsed)

	after := readEscalationMetrics(t, base)
	switch {
	case before.GenerationKnown && after.GenerationKnown:
		if after.Generation != before.Generation {
			t.Errorf("generation changed from %d to %d (a relaunch happened)", before.Generation, after.Generation)
		}
	default:
		t.Logf("generation not compared: this daemon's /metrics named no single tenant's generation, either redacted or serving several; the attestations check below covers the same relaunch")
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

	// A repeat should be served from the negative cache. Both counters come from
	// one scrape on each side of that single call, so their window is the repeat
	// alone; reusing the earlier snapshot would blame the counter split for any
	// unrelated traffic on a shared daemon.
	beforeRepeat := readMetrics(t, base)
	negBefore := beforeRepeat.counter(t, "player_context_negative_cache_hits")
	pcfBefore := beforeRepeat.counter(t, "player_context_failures")

	_, elapsed2, err2 := call(deadID, 10*time.Second)
	requireUnavailable(t, err2)
	if elapsed2 > 2*time.Second {
		t.Errorf("negative-cache repeat took %v, want near-instant", elapsed2)
	}
	t.Logf("dead id repeat (negative cache) in %v", elapsed2)

	afterRepeat := readMetrics(t, base)
	if negAfter := afterRepeat.counter(t, "player_context_negative_cache_hits"); negAfter <= negBefore {
		t.Errorf("player_context_negative_cache_hits did not increase from %d to %d", negBefore, negAfter)
	}
	if pcfAfter := afterRepeat.counter(t, "player_context_failures"); pcfAfter != pcfBefore {
		t.Errorf("player_context_failures moved from %d to %d across a negative-cache hit; the split exists to stop that", pcfBefore, pcfAfter)
	}

	// A valid ID immediately afterward must still establish.
	pc, _, err3 := call(bbbVideoID, 90*time.Second)
	if err3 != nil {
		t.Fatalf("good id after dead id: %v", err3)
	}
	if pc.PlayabilityStatus != "OK" {
		t.Errorf("good id playability_status = %q, want OK", pc.PlayabilityStatus)
	}
}

// multitrackVideoEnv names a video with several audio tracks for
// TestPlayerContextFieldsHTTP's multitrack subtest. The suite's freely licensed
// videos have one track each, and the labels WaxTap ranks the original track by
// only appear on a multi-track video, so the operator supplies one.
const multitrackVideoEnv = "WAXSEAL_E2E_MULTITRACK_VIDEO"

// TestPlayerContextFieldsHTTP checks real contexts, as the provider hands them
// to WaxTap, against what WaxTap consumes: one subtest per concern, on one
// daemon. The first subtest that needs the Big Buck Bunny context fetches it,
// so a focused multitrack run skips that fetch and a Big Buck Bunny failure
// stays in its own subtests. The player's /player response on the watch page
// is a WEB response and carries a microformat, but a response without one is
// legal, so the publish-date check accepts an empty field.
func TestPlayerContextFieldsHTTP(t *testing.T) {
	base := startColdDaemon(t)
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var (
		bbbOnce sync.Once
		bbbPC   potoken.PlayerContext
		bbbErr  error
	)
	bbb := func(t *testing.T) potoken.PlayerContext {
		t.Helper()
		bbbOnce.Do(func() { bbbPC, bbbErr = p.ProvidePlayerContext(ctx, bbbVideoID) })
		if bbbErr != nil {
			t.Fatalf("ProvidePlayerContext(%s): %v", bbbVideoID, bbbErr)
		}
		return bbbPC
	}

	// The metadata WaxTap asked for has to arrive from a real player response,
	// not only from the wire structs.
	t.Run("metadata", func(t *testing.T) {
		pc := bbb(t)
		if !strings.HasPrefix(pc.ChannelID, "UC") {
			t.Errorf("channel_id = %q, want the owner's UC... id", pc.ChannelID)
		}
		if pc.Description == "" {
			t.Error("description is empty; videoDetails.shortDescription was not read")
		}
		if len(pc.Thumbnails) == 0 {
			t.Error("thumbnails is empty; the ladder was not read")
		}
		for i, th := range pc.Thumbnails {
			if th.URL == "" {
				t.Errorf("thumbnails[%d] has no url", i)
			}
			if th.Width <= 0 || th.Height <= 0 {
				t.Errorf("thumbnails[%d] = %dx%d, want positive dimensions", i, th.Width, th.Height)
			}
		}
		// Big Buck Bunny is an ordinary uploaded VOD, so all three flags are false.
		if pc.IsLiveContent || pc.IsLiveNow || pc.IsUpcoming {
			t.Errorf("live flags = %v/%v/%v, want all false for a VOD", pc.IsLiveContent, pc.IsLiveNow, pc.IsUpcoming)
		}
		switch {
		case pc.PublishDate == "":
			t.Log("publish_date is empty: this player response carried no microformat, which is legal; the field is documented as empty when absent")
		default:
			if _, err := time.Parse(time.RFC3339, pc.PublishDate); err != nil {
				if _, dErr := time.Parse("2006-01-02", pc.PublishDate); dErr != nil {
					t.Errorf("publish_date = %q parses as neither RFC 3339 (%v) nor 2006-01-02 (%v)", pc.PublishDate, err, dErr)
				} else {
					t.Logf("publish_date = %q (bare date)", pc.PublishDate)
				}
			} else {
				t.Logf("publish_date = %q (RFC 3339)", pc.PublishDate)
			}
		}
		// The context's identity must be the one /session exports, or a consumer
		// that streams under the context would present a different browser than
		// the one the URL was issued to.
		if pc.UserAgent == "" {
			t.Error("user_agent is empty; the session identity was not carried onto the context")
		}
		sess, err := p.Session(ctx)
		if err != nil {
			t.Fatalf("Session: %v", err)
		}
		if pc.UserAgent != sess.UserAgent {
			t.Errorf("user_agent = %q, want /session's %q", pc.UserAgent, sess.UserAgent)
		}
		if pc.ClientVersion != "" && sess.ClientVersion != "" && pc.ClientVersion != sess.ClientVersion {
			t.Errorf("client_version = %q, want /session's %q", pc.ClientVersion, sess.ClientVersion)
		}
		t.Logf("metadata: channel_id=%s title=%q author=%q description_len=%d thumbnails=%d user_agent=%q",
			pc.ChannelID, pc.Title, pc.Author, len(pc.Description), len(pc.Thumbnails), pc.UserAgent)
	})

	// audio_formats must carry what WaxTap's SABR selection reads (see
	// checkFormats). Big Buck Bunny has one audio track, so no entry carries an
	// audio role, but its itags have come in clean, DRC, and vb renditions, and
	// the tags let the two-way drc check tell an untouched value from a
	// rewritten one. A daemon that drops xtags from an is_drc rendition fails in
	// checkFormats; a day with no tagged rendition has nothing to check and skips.
	t.Run("formats", func(t *testing.T) {
		pc := bbb(t)
		tagged, drc := 0, 0
		for i, pairs := range checkFormats(t, pc.AudioFormats) {
			if pairs != nil {
				tagged++
			}
			if pc.AudioFormats[i].IsDrc {
				drc++
			}
		}
		if tagged == 0 && drc == 0 {
			t.Skip("no entry carries xtags or is_drc: this video offers no tagged rendition to check today")
		}
	})

	// Original-track labeling only shows on a multi-track video. The daemon
	// must name every entry's track and state the player's default flag; WaxTap,
	// given the same context, must rank every track, find one original, and
	// select it.
	t.Run("multitrack", func(t *testing.T) {
		id := os.Getenv(multitrackVideoEnv)
		if id == "" {
			t.Skipf("%s names no video with several audio tracks", multitrackVideoEnv)
		}
		pc, err := p.ProvidePlayerContext(ctx, id)
		if err != nil {
			t.Fatalf("ProvidePlayerContext(%s): %v", id, err)
		}
		tracks := make(map[string]int)
		for _, f := range pc.AudioFormats {
			if f.AudioTrackID != "" {
				tracks[f.AudioTrackID]++
			}
		}
		if len(tracks) < 2 {
			t.Fatalf("%s names %s, which offers %d audio track(s) %v; this subtest needs a video with several", multitrackVideoEnv, id, len(tracks), tracks)
		}
		checkFormats(t, pc.AudioFormats)
		defaults := 0
		for i, f := range pc.AudioFormats {
			if f.AudioTrackID == "" {
				t.Errorf("audio_formats[%d] (itag %d) names no track on a multi-track video", i, f.Itag)
			}
			switch {
			case f.AudioIsDefault == nil:
				t.Errorf("audio_formats[%d] (itag %d, track %q) states no audio_is_default; the player marks every track of a multi-track video", i, f.Itag, f.AudioTrackID)
			case *f.AudioIsDefault:
				defaults++
			}
		}
		if defaults == 0 {
			t.Error("no entry states audio_is_default true; the player marks its default track and the daemon dropped it")
		}

		// WaxTap reads the audio role from xtags, falls back to the default flag,
		// and selects by the result. It gets this context from memory, so its
		// verdict is on the entries checked above, not a second fetch.
		yt := youtube.New(youtube.Config{PlayerContextProvider: potoken.PlayerContextProviderFunc(
			func(context.Context, string) (potoken.PlayerContext, error) { return pc, nil })})
		ext, err := yt.ExtractWebContext(ctx, id)
		if err != nil {
			t.Fatalf("WaxTap rejected the context: %v", err)
		}
		formats := ext.Video().Formats
		originals := make(map[string]int)
		for i, f := range formats {
			track := ""
			if f.AudioTrack != nil {
				track = f.AudioTrack.ID
			}
			switch f.IsOriginal {
			case format.Unknown:
				t.Errorf("WaxTap could not rank Formats[%d] (itag %d, track %q): neither xtags nor audio_is_default decided", i, f.Itag, track)
			case format.Yes:
				originals[track]++
			}
		}
		if len(originals) != 1 {
			t.Errorf("WaxTap ranks %d tracks as the original (%v), want exactly one", len(originals), originals)
		}
		idx, err := format.BestForTarget(formats, format.MinimizeLoss(), format.Target{})
		if err != nil {
			t.Fatalf("BestForTarget: %v", err)
		}
		if formats[idx].IsOriginal != format.Yes {
			t.Errorf("selection picked Formats[%d] (itag %d, original %v), want the original track", idx, formats[idx].Itag, formats[idx].IsOriginal)
		}
		t.Logf("multitrack %s: %d entries over %d tracks, %d state default; WaxTap ranks %v as the original and selects itag %d",
			id, len(pc.AudioFormats), len(tracks), defaults, originals, formats[idx].Itag)
	})
}

// checkFormats runs the checks every context's audio_formats must pass and
// returns each entry's decoded xtags pairs (nil for an entry with none). Each
// (itag, lmt, xtags) triple must be unique, so a reload finds the rendition it
// was streaming. xtags must be the player's value (see xtagsPairs), and its
// drc=1 tag must mark exactly the entries is_drc marks, since WaxTap declares
// DRC on the wire from is_drc. An entry without a track id states no default
// flag, since the flag lives in the player's audioTrack.
func checkFormats(t *testing.T, formats []potoken.PlayerContextFormat) []map[string]string {
	t.Helper()
	if len(formats) == 0 {
		t.Fatal("audio_formats is empty")
	}
	type encoding struct {
		itag       int
		lmt, xtags string
	}
	seen := make(map[encoding]int)
	decoded := make([]map[string]string, len(formats))
	for i, f := range formats {
		key := encoding{f.Itag, f.LMT, f.XTags}
		if prev, dup := seen[key]; dup {
			t.Errorf("audio_formats[%d] repeats the (itag, lmt, xtags) triple of audio_formats[%d]: %+v", i, prev, key)
		}
		seen[key] = i
		if f.LMT == "" {
			t.Errorf("audio_formats[%d] (itag %d) has no lmt", i, f.Itag)
		}
		if f.AudioTrackID == "" && f.AudioIsDefault != nil {
			t.Errorf("audio_formats[%d] (itag %d) states audio_is_default=%v without a track id", i, f.Itag, *f.AudioIsDefault)
		}
		if f.XTags == "" {
			if f.IsDrc {
				t.Errorf("audio_formats[%d] (itag %d) has is_drc set but no xtags to carry the drc tag", i, f.Itag)
			}
			continue
		}
		pairs, err := xtagsPairs(f.XTags)
		if err != nil {
			t.Errorf("audio_formats[%d] (itag %d) xtags %q is not the player's value: %v", i, f.Itag, f.XTags, err)
			continue
		}
		decoded[i] = pairs
		t.Logf("audio_formats[%d]: itag %d lmt %s track %q xtags %s decodes to %v (is_drc=%v, audio_is_default=%s)",
			i, f.Itag, f.LMT, f.AudioTrackID, f.XTags, pairs, f.IsDrc, stated(f.AudioIsDefault))
		if drc := pairs["drc"] == "1"; drc != f.IsDrc {
			t.Errorf("audio_formats[%d] (itag %d): xtags says drc=%q but is_drc is %v", i, f.Itag, pairs["drc"], f.IsDrc)
		}
	}
	return decoded
}

// stated renders a tri-state flag for a log line.
func stated(b *bool) string {
	if b == nil {
		return "unstated"
	}
	return strconv.FormatBool(*b)
}

// xtagsPairs decodes an xtags value as the contract states it: unpadded
// base64url over a protobuf of repeated pairs (field 1, each {key=1, value=2}),
// unknown fields skipped, and the first non-empty value of a repeated key kept,
// as WaxTap keeps it. It is stricter than WaxTap, which accepts padding and
// either alphabet, because it checks that the daemon passed the player's value
// on untouched: SABR keys the rendition on those bytes, so a re-encoding WaxTap
// could still read would cost the consumer a reload. The decoder skips CR and
// LF, which the player never sends.
func xtagsPairs(s string) (map[string]string, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, err
	}
	pairs := make(map[string]string)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		if num != 1 || typ != protowire.BytesType {
			if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
				return nil, protowire.ParseError(n)
			}
			b = b[n:]
			continue
		}
		pair, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		key, value, err := xtagsPair(pair)
		if err != nil {
			return nil, err
		}
		if pairs[key] == "" {
			pairs[key] = value
		}
	}
	return pairs, nil
}

// xtagsPair decodes one {key=1, value=2} pair.
func xtagsPair(b []byte) (key, value string, err error) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", "", protowire.ParseError(n)
		}
		b = b[n:]
		if (num != 1 && num != 2) || typ != protowire.BytesType {
			if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
				return "", "", protowire.ParseError(n)
			}
			b = b[n:]
			continue
		}
		v, n := protowire.ConsumeString(b)
		if n < 0 {
			return "", "", protowire.ParseError(n)
		}
		if !utf8.ValidString(v) {
			return "", "", fmt.Errorf("field %d is not UTF-8: %q", num, v)
		}
		b = b[n:]
		if num == 1 {
			key = v
		} else {
			value = v
		}
	}
	return key, value, nil
}
