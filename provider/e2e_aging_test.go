//go:build e2e

package provider_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
	"github.com/colespringer/waxseal/server"
	waxtap "github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/potoken"
)

// The aging matrix measures which artifact's age, if any, predicts a capped
// stream on the player-context path. The daemon cannot tell a good context from
// a bad one: the URL never changes between a stream that completes and one that
// stops at the same byte, and the browser keeps buffering past the cap on the
// very same URL. What is left to separate is how long each artifact has existed
// when the stream begins: the attested session, the GVS token, and the issued
// context URL. Each arm ages exactly one of those and leaves the others fresh.
//
// The second matrix follows the first. Once the separating variable is known to
// be the distance between the token's mint and the context that gets streamed,
// its arms hold everything else fixed and vary only that distance, and one arm
// asks whether the daemon's own startup sequence already supplies it.
//
// These are measurements, not assertions. A capped stream is a result, so no arm
// fails on truncation; the test only requires that every iteration produced a
// record.
//
// The daemon's own protections floor every arm regardless of what it ages:
// attestation always pre-mints a token, and every mint or context handoff keeps
// mintSeparation clear of the browser's last in-page activity. By default this
// suite leaves that spacing at the daemon's own default (12s unless
// WAXSEAL_MINT_SEPARATION overrides it), which makes the default run a
// regression check rather than a raw-gap measurement: every arm is expected to
// stream full length. Set WAXSEAL_E2E_AGING_SEPARATION to a Go duration such as
// "1ms" to override server.Config.MintSeparation on every in-process daemon this
// suite starts, which removes the gate so the arms measure the gaps they name
// again. Because the token is minted at attestation rather than at an arm's own
// mint call (which usually just returns that same cached token), the token_age
// and both_age arms' measured age is really the time since warmDone
// (attestation), not since tokenMinted.

const (
	// agingEnableEnv selects which matrix to run, "1" or "2". Either runs fresh
	// browsers for tens of minutes, so the normal e2e suite must not pick one up.
	agingEnableEnv = "WAXSEAL_E2E_AGING"
	// agingIterationsEnv overrides the per-arm iteration count.
	agingIterationsEnv = "WAXSEAL_E2E_AGING_N"
	// agingDelayEnv overrides the run-wide delay. An arm carrying its own gap
	// ignores it.
	agingDelayEnv = "WAXSEAL_E2E_AGING_DELAY"
	// agingSeparationEnv overrides server.Config.MintSeparation on every
	// in-process daemon this suite starts. Unset, the daemon's own default
	// applies and the matrix is a regression check (every arm expected full); a
	// small positive duration such as "1ms" effectively removes the gate so the
	// arms measure raw gaps again.
	agingSeparationEnv = "WAXSEAL_E2E_AGING_SEPARATION"

	agingDefaultIterations = 6
	agingDefaultDelay      = 30 * time.Second

	// agingIterationBudget bounds one iteration: a cold warm, the arm's delay, a
	// player-context call, and the full download.
	agingIterationBudget = 15 * time.Minute

	// agingStamp is the timestamp layout for the AGING records.
	agingStamp = "2006-01-02T15:04:05.000Z07:00"
)

// agingRecord is one iteration's measurement. Every field it reports is a wall
// clock instant rather than a duration, so ages can be recomputed against any
// reference point after the run.
type agingRecord struct {
	arm  string
	iter int

	warmDone      time.Time // attestation finished; the session's identity exists from here
	tokenMinted   time.Time // the /get_pot that produced the streamed GVS token
	contextIssued time.Time // the player-context call that produced the streamed URL
	streamStart   time.Time // just before the consumer's Stream call

	outcome       string // full, truncated, or error when the iteration could not stream
	bytes         int64
	contentLength int64

	potCache  string // the daemon's X-POT-Cache verdict for the consumer's own token fetch
	tokenSame string // whether the consumer received the token this arm pre-minted
	potCalls  int    // token fetches the consumer made
	pcCalls   int    // player-context fetches the consumer made
	note      string

	// gap is the arm's own delay, rendered for the record. It is set only by a
	// matrix whose arms carry individual delays, so the run-wide-delay matrix
	// keeps its original columns.
	gap string
	// anchor is which instant the daemon's own separation gate held the streamed
	// context back from: "proof" or "mint". It is what makes a proof-gap record
	// self-certifying, because an arm that means to measure the proof edge but
	// waited on a mint measured something else. Empty when the arm did not ask.
	anchor string
	// selfTestDone is when the daemon's startup self-test returned. It bounds a
	// mint the consumer never saw, because the self-test mints inside the daemon.
	selfTestDone time.Time
}

// line renders the machine-readable record. One line per iteration is the
// contract the run is read back through, so it is emitted even for an iteration
// that never reached the stream.
func (r *agingRecord) line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "AGING arm=%s iter=%d outcome=%s bytes=%d of=%d warm_done=%s token_minted=%s context_issued=%s stream_start=%s pot_cache=%s token_same=%s pot_calls=%d pc_calls=%d",
		r.arm, r.iter, r.status(), r.bytes, r.contentLength,
		agingTime(r.warmDone), agingTime(r.tokenMinted), agingTime(r.contextIssued), agingTime(r.streamStart),
		agingField(r.potCache), agingField(r.tokenSame), r.potCalls, r.pcCalls)
	if r.gap != "" {
		fmt.Fprintf(&b, " gap=%s", r.gap)
	}
	if r.anchor != "" {
		fmt.Fprintf(&b, " anchor=%s", r.anchor)
	}
	if !r.selfTestDone.IsZero() {
		fmt.Fprintf(&b, " selftest_done=%s", agingTime(r.selfTestDone))
	}
	if r.note != "" {
		fmt.Fprintf(&b, " note=%q", r.note)
	}
	return b.String()
}

// status is the iteration's outcome. An iteration that aborted before it could
// stream never set one, and reports as an error rather than as an empty field.
func (r *agingRecord) status() string {
	if r.outcome == "" {
		return "error"
	}
	return r.outcome
}

// agingTime renders a timestamp, or a dash when the iteration never reached the
// step that would have stamped it.
func agingTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(agingStamp)
}

// agingField renders an unset string field as a dash, so no column is ever empty.
func agingField(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// agingAge is how old an artifact was when the stream began.
func agingAge(from, to time.Time) string {
	if from.IsZero() || to.IsZero() {
		return "-"
	}
	return to.Sub(from).Round(time.Millisecond).String()
}

// potCacheRecorder records the daemon's X-POT-Cache verdict for every /get_pot
// response. The client package does not surface that header, and the matrix
// needs it to tell a token reused from the daemon's cache from one minted afresh
// at stream start.
type potCacheRecorder struct {
	base http.RoundTripper

	mu       sync.Mutex
	first    time.Time
	verdicts []string
}

func (rt *potCacheRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, "/get_pot") {
		return resp, err
	}
	verdict := resp.Header.Get("X-POT-Cache")
	if verdict == "" {
		verdict = "unknown"
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.first.IsZero() {
		rt.first = time.Now()
	}
	rt.verdicts = append(rt.verdicts, verdict)
	return resp, nil
}

// firstMint is when the daemon answered the first /get_pot of the iteration.
func (rt *potCacheRecorder) firstMint() time.Time {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.first
}

// lastVerdict is the cache verdict of the most recent /get_pot, which is the one
// that served the stream.
func (rt *potCacheRecorder) lastVerdict() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.verdicts) == 0 {
		return ""
	}
	return rt.verdicts[len(rt.verdicts)-1]
}

// agingPOToken records what the consumer's own token fetch returned, so an arm
// that pre-minted can confirm the consumer got that same token back rather than
// a fresh one.
type agingPOToken struct {
	inner potoken.Provider

	mu    sync.Mutex
	calls int
	last  string
}

func (p *agingPOToken) ProvidePOToken(ctx context.Context, req potoken.Request) (potoken.Response, error) {
	resp, err := p.inner.ProvidePOToken(ctx, req)
	p.mu.Lock()
	p.calls++
	if resp.Token != "" {
		p.last = resp.Token
	}
	p.mu.Unlock()
	return resp, err
}

// snapshot reports the consumer's token-fetch count and the last token it got.
func (p *agingPOToken) snapshot() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.last
}

// agingContextSource is the player-context arm of one iteration. issued reports
// when the streamed context was obtained and how many times the consumer asked
// for one, which is how a mid-stream reload shows up in the record.
type agingContextSource interface {
	potoken.PlayerContextProvider
	issued() (time.Time, int)
}

// agingLiveContext lets the consumer fetch its own context inside Stream and
// stamps when it did. This is the shape of every arm that does not age the URL.
type agingLiveContext struct {
	inner potoken.PlayerContextProvider

	mu    sync.Mutex
	calls int
	first time.Time
}

func newAgingLive(inner potoken.PlayerContextProvider) *agingLiveContext {
	return &agingLiveContext{inner: inner}
}

func (p *agingLiveContext) ProvidePlayerContext(ctx context.Context, videoID string) (potoken.PlayerContext, error) {
	pc, err := p.inner.ProvidePlayerContext(ctx, videoID)
	now := time.Now()
	p.mu.Lock()
	p.calls++
	if err == nil && p.first.IsZero() {
		p.first = now
	}
	p.mu.Unlock()
	return pc, err
}

func (p *agingLiveContext) issued() (time.Time, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.first, p.calls
}

// agingFixedContext hands the consumer one already-issued context, so the aged
// URL is the URL that gets streamed. The consumer re-requests a context on a
// mid-stream reload; returning the same aged one keeps the arm honest, and the
// call count reports that it happened.
type agingFixedContext struct {
	pc potoken.PlayerContext
	at time.Time

	mu    sync.Mutex
	calls int
}

func newAgingFixed(pc potoken.PlayerContext, at time.Time) *agingFixedContext {
	return &agingFixedContext{pc: pc, at: at}
}

func (p *agingFixedContext) ProvidePlayerContext(context.Context, string) (potoken.PlayerContext, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return p.pc, nil
}

func (p *agingFixedContext) issued() (time.Time, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.at, p.calls
}

// separationLog collects the anchors the minter reported waiting on. Only
// waitBeforeEstablish reports "mint" or "proof"; waitBeforeMint reports
// "establishment", so filtering on the anchor name isolates the gate that held
// the streamed context back.
type separationLog struct {
	mu      sync.Mutex
	anchors []string
}

func (l *separationLog) record(after string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.anchors = append(l.anchors, after)
}

// contextAnchors returns the anchors that held a context establishment back, in
// order.
func (l *separationLog) contextAnchors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, a := range l.anchors {
		if a == "mint" || a == "proof" {
			out = append(out, a)
		}
	}
	return out
}

// separationWatcher forwards every record to inner and copies the anchor out of
// the minter's separation-wait line. Reading the daemon's own log is what makes
// a proof-gap record self-certifying: the arm states which anchor it means to
// measure, and the record proves which one the daemon actually used.
//
// Enabled delegates, which is correct here because the line is logged at Info
// and testDaemonLogger runs at Info or Debug.
type separationWatcher struct {
	inner slog.Handler
	log   *separationLog
}

func (h separationWatcher) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h separationWatcher) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "minter: separation wait" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "after" {
				h.log.record(a.Value.String())
				return false
			}
			return true
		})
	}
	return h.inner.Handle(ctx, r)
}

func (h separationWatcher) WithAttrs(as []slog.Attr) slog.Handler {
	return separationWatcher{inner: h.inner.WithAttrs(as), log: h.log}
}

func (h separationWatcher) WithGroup(name string) slog.Handler {
	return separationWatcher{inner: h.inner.WithGroup(name), log: h.log}
}

// agingHarness is one iteration's fresh daemon and the client wiring around it.
type agingHarness struct {
	srv  *server.Server
	base string
	c    *client.Client
	p    *provider.Provider
	rt   *potCacheRecorder
	po   *agingPOToken
	sep  *separationLog
}

// newAgingHarness starts a fresh cold daemon and warms one session the way
// startColdDaemon does, then optionally runs the startup self-test the daemon
// binary runs before it accepts traffic. rec.warmDone is stamped when
// attestation finishes, before any self-test, because that is when the session's
// identity came into being.
//
// Warm and SelfTest share one 120 second budget, matching the daemon's startup.
// The server registers its own shutdown with t.Cleanup, so running each
// iteration as a subtest is what tears the browser down between iterations.
// separation becomes server.Config.MintSeparation; 0 leaves the daemon's own
// default in place.
func newAgingHarness(t *testing.T, rec *agingRecord, selfTest bool, separation time.Duration) *agingHarness {
	t.Helper()
	sepLog := &separationLog{}
	srv, addr := newInProcessDaemon(t, server.Config{
		MintSeparation: separation,
		Logger:         slog.New(separationWatcher{inner: testDaemonLogger(t).Handler(), log: sepLog}),
	})
	startCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := srv.Warm(startCtx, ""); err != nil {
		t.Fatalf("warm cold daemon (browser attest): %v", err)
	}
	rec.warmDone = time.Now()
	if selfTest {
		if err := srv.SelfTest(startCtx, ""); err != nil {
			// The daemon binary refuses to serve here. The iteration keeps going so
			// the arm still contributes a record, and the note says what happened.
			rec.note = "self-test failed: " + err.Error()
			t.Errorf("self-test: %v", err)
		}
		rec.selfTestDone = time.Now()
	}
	go func() { _ = srv.ListenAndServe() }()
	base := "http://" + addr
	waitDaemonReady(t, base)

	rt := &potCacheRecorder{base: http.DefaultTransport}
	// The timeout must clear the daemon's own per-request budget so a slow
	// player-context call fails at the daemon rather than at the socket.
	c := client.New(base,
		client.WithAPIKey(os.Getenv("WAXSEAL_KEY")),
		client.WithHTTPClient(&http.Client{Transport: rt, Timeout: 4 * time.Minute}))
	p := provider.New(c)
	return &agingHarness{srv: srv, base: base, c: c, p: p, rt: rt, po: &agingPOToken{inner: p}, sep: sepLog}
}

// agingStream streams videoURL over the attested player-context path. It mirrors
// streamWEBContext's wiring but returns the truncation instead of failing the
// test, and takes the two providers separately so an arm can stream an
// already-issued context rather than one the consumer fetches at stream time.
func agingStream(t *testing.T, ctx context.Context, po potoken.Provider, pc potoken.PlayerContextProvider, videoURL string) (n int64, info waxtap.StreamInfo, warnings []string, err error) {
	t.Helper()
	var mu sync.Mutex
	capture := func(ev waxtap.Event) {
		if ev.Stage != waxtap.StageWarning || ev.Warning == nil {
			return
		}
		mu.Lock()
		warnings = append(warnings, fmt.Sprintf("code=%d %s", ev.Warning.Code, ev.Warning.Detail))
		mu.Unlock()
	}
	jar, _ := cookiejar.New(nil)
	tap, newErr := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Jar: jar, Timeout: 120 * time.Second},
		POTokenProvider:       po,
		PlayerContextProvider: pc,
		Client:                clientWeb, // the fallback chain; the context path is preferred
	})
	if newErr != nil {
		t.Fatalf("waxtap.New: %v", newErr)
	}
	rc, info, streamErr := tap.Stream(ctx, waxtap.Request{URL: videoURL, ProcessSpec: waxtap.ProcessSpec{Events: capture}})
	if streamErr != nil {
		mu.Lock()
		defer mu.Unlock()
		return 0, info, warnings, streamErr
	}
	defer rc.Close()
	n, err = io.Copy(io.Discard, rc)
	mu.Lock()
	defer mu.Unlock()
	return n, info, warnings, err
}

// agingPrep is what an arm hands the stream: the context provider to stream
// through and, when the arm pre-minted one, the GVS token it expects the
// consumer to be handed back.
type agingPrep struct {
	pc       agingContextSource
	preToken string
}

// agingArm is one row of the matrix. selfTest asks for the daemon binary's
// startup check; run performs everything the arm ages before the stream begins.
type agingArm struct {
	name     string
	selfTest bool
	// gap overrides the run-wide delay with one this arm owns. Nil takes the
	// run-wide delay and leaves the gap column out of the record.
	gap *time.Duration
	// separation overrides the run-wide server.Config.MintSeparation with one
	// this arm owns. An arm sets it when the daemon's own gate is the instrument
	// rather than something to get out of the way. Nil takes the run-wide value.
	separation *time.Duration
	// watchAnchor asks the iteration to record which anchor the daemon's
	// separation gate held the streamed context back from.
	watchAnchor bool
	run         func(t *testing.T, ctx context.Context, h *agingHarness, rec *agingRecord, d time.Duration) agingPrep
}

// agingGap makes an arm's own delay addressable.
func agingGap(d time.Duration) *time.Duration { return &d }

// agingArms isolates one artifact's age per row. Every arm streams the same
// video from a daemon warmed moments earlier, so the only difference between two
// rows is which artifact was allowed to get old.
var agingArms = []agingArm{
	{
		// Nothing is aged: the token is minted and the context issued inside Stream.
		name: "baseline",
		run: func(_ *testing.T, _ context.Context, h *agingHarness, _ *agingRecord, _ time.Duration) agingPrep {
			return agingPrep{pc: newAgingLive(h.p)}
		},
	},
	{
		// Only the attested session and identity are old; the token and URL are
		// minted after the wait.
		name: "session_age",
		run: func(t *testing.T, ctx context.Context, h *agingHarness, _ *agingRecord, d time.Duration) agingPrep {
			agingSleep(t, ctx, d)
			return agingPrep{pc: newAgingLive(h.p)}
		},
	},
	{
		// Only the GVS token is old. The first player-context call exists solely to
		// learn the visitor data the token binds to; the streamed context is a fresh
		// one taken after the wait.
		name: "token_age",
		run: func(t *testing.T, ctx context.Context, h *agingHarness, rec *agingRecord, d time.Duration) agingPrep {
			pc, err := h.c.PlayerContext(ctx, bbbVideoID)
			if err != nil {
				t.Fatalf("player-context to learn visitor_data: %v", err)
			}
			tok, err := h.c.POToken(ctx, pc.VisitorData, "gvs")
			if err != nil {
				t.Fatalf("pre-mint gvs token: %v", err)
			}
			rec.tokenMinted = time.Now()
			agingSleep(t, ctx, d)
			return agingPrep{pc: newAgingLive(h.p), preToken: tok.Value}
		},
	},
	{
		// Only the context URL is old. The consumer mints its token at stream start.
		name: "url_age",
		run: func(t *testing.T, ctx context.Context, h *agingHarness, _ *agingRecord, d time.Duration) agingPrep {
			pc, err := h.p.ProvidePlayerContext(ctx, bbbVideoID)
			if err != nil {
				t.Fatalf("player-context: %v", err)
			}
			issued := time.Now()
			agingSleep(t, ctx, d)
			return agingPrep{pc: newAgingFixed(pc, issued)}
		},
	},
	{
		// Both the URL and the token it streams under are old.
		name: "both_age",
		run: func(t *testing.T, ctx context.Context, h *agingHarness, rec *agingRecord, d time.Duration) agingPrep {
			pc, err := h.p.ProvidePlayerContext(ctx, bbbVideoID)
			if err != nil {
				t.Fatalf("player-context: %v", err)
			}
			issued := time.Now()
			tok, err := h.c.POToken(ctx, pc.VisitorData, "gvs")
			if err != nil {
				t.Fatalf("pre-mint gvs token: %v", err)
			}
			rec.tokenMinted = time.Now()
			agingSleep(t, ctx, d)
			return agingPrep{pc: newAgingFixed(pc, issued), preToken: tok.Value}
		},
	},
	{
		// What an operator actually gets: warm, then the startup self-test, then
		// serve. The self-test caches a GVS token under the same key the consumer's
		// fetch uses, so this arm ages the token by however long the self-test's
		// full-length establishment took.
		name:     "production_path",
		selfTest: true,
		run: func(_ *testing.T, _ context.Context, h *agingHarness, _ *agingRecord, _ time.Duration) agingPrep {
			return agingPrep{pc: newAgingLive(h.p)}
		},
	},
}

// agingMintGapArm builds one row of the second matrix. The arm learns the
// identity through /session rather than a player context, so no context for the
// target video is issued before the token is minted, then mints the GVS token,
// waits gap, and lets the consumer take a fresh context and stream. The only
// thing that varies across these rows is gap.
//
// /session runs the daemon's own establishment proof on the landing video when
// the session has not been established yet, so the arm removes a preceding
// context for the target video rather than removing playback altogether.
func agingMintGapArm(name string, gap time.Duration) agingArm {
	return agingArm{
		name: name,
		gap:  agingGap(gap),
		run: func(t *testing.T, ctx context.Context, h *agingHarness, rec *agingRecord, d time.Duration) agingPrep {
			sess, err := h.c.Session(ctx)
			if err != nil {
				t.Fatalf("session to learn visitor_data: %v", err)
			}
			if sess.VisitorData == "" {
				t.Fatalf("daemon returned an empty visitor_data")
			}
			tok, err := h.c.POToken(ctx, sess.VisitorData, "gvs")
			if err != nil {
				t.Fatalf("pre-mint gvs token: %v", err)
			}
			rec.tokenMinted = time.Now()
			agingSleep(t, ctx, d)
			return agingPrep{pc: newAgingLive(h.p), preToken: tok.Value}
		},
	}
}

// agingMintGapArms measures how much distance between the token's mint and the
// streamed context is enough, and whether the daemon's own startup sequence
// already provides it. Every row streams a context the consumer takes fresh
// after the wait, so the token is the only aged artifact.
var agingMintGapArms = []agingArm{
	agingMintGapArm("mint_gap0", 0),
	agingMintGapArm("mint_gap10", 10*time.Second),
	agingMintGapArm("mint_gap20", 20*time.Second),
	agingMintGapArm("mint_gap30", 30*time.Second),
	agingMintGapArm("mint_gap45", 45*time.Second),
	{
		// The daemon's startup sequence in full: warm, then the self-test, which
		// mints the GVS token and then runs the proof playback. The wait follows,
		// so the token reaches the stream already old without the consumer having
		// arranged it.
		name:     "selftest_gap30",
		selfTest: true,
		gap:      agingGap(30 * time.Second),
		run: func(t *testing.T, ctx context.Context, h *agingHarness, _ *agingRecord, d time.Duration) agingPrep {
			agingSleep(t, ctx, d)
			return agingPrep{pc: newAgingLive(h.p)}
		},
	},
}

// agingProofGapArm builds one row of the third matrix. The arm sets the daemon's
// separation window to gap and runs the self-test, which mints and then proves,
// so the proof is the later anchor waitBeforeEstablish measures from and the
// daemon holds the context exactly gap past it. The gate is the instrument here,
// which is why the arm sets it rather than removing it. The consumer's own token
// fetch is a cache hit, so it cannot move the mint anchor; anchor= in the record
// proves that per iteration.
func agingProofGapArm(name string, gap time.Duration) agingArm {
	return agingArm{
		name:        name,
		selfTest:    true,
		gap:         agingGap(gap),
		separation:  agingGap(gap),
		watchAnchor: true,
		run: func(_ *testing.T, _ context.Context, h *agingHarness, _ *agingRecord, _ time.Duration) agingPrep {
			return agingPrep{pc: newAgingLive(h.p)}
		},
	}
}

// agingProofGapArms fills the interval the earlier matrix left untested: it saw
// the proof anchor fail at 3s and pass at 12s with nothing between. Both are kept
// as controls, so a run where proof_gap3 passes measured something other than
// what the earlier one did.
//
// The reading rule, fixed before the run: a truncation at gap g puts the edge at
// or above g, an arm passes only at 4 of 4, and the default moves only if an arm
// at or above 10s truncates.
var agingProofGapArms = []agingArm{
	agingProofGapArm("proof_gap3", 3*time.Second),
	agingProofGapArm("proof_gap4", 4*time.Second),
	agingProofGapArm("proof_gap6", 6*time.Second),
	agingProofGapArm("proof_gap8", 8*time.Second),
	agingProofGapArm("proof_gap10", 10*time.Second),
	agingProofGapArm("proof_gap12", 12*time.Second),
}

// agingSleep is the arm's delay, interruptible by the iteration budget.
func agingSleep(t *testing.T, ctx context.Context, d time.Duration) {
	t.Helper()
	if d <= 0 {
		return
	}
	t.Logf("aging %s before the stream", d)
	select {
	case <-ctx.Done():
		t.Fatalf("aging delay interrupted: %v", ctx.Err())
	case <-time.After(d):
	}
}

// runAgingIteration performs one cell of the matrix and fills in rec. separation
// is passed through to newAgingHarness as server.Config.MintSeparation.
func runAgingIteration(t *testing.T, arm agingArm, rec *agingRecord, d, separation time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), agingIterationBudget)
	defer cancel()

	delay := d
	if arm.gap != nil {
		delay = *arm.gap
		rec.gap = delay.String()
	}
	if arm.separation != nil {
		separation = *arm.separation
	}
	h := newAgingHarness(t, rec, arm.selfTest, separation)
	prep := arm.run(t, ctx, h, rec, delay)

	rec.streamStart = time.Now()
	n, info, warnings, err := agingStream(t, ctx, h.po, prep.pc, bbbURL)

	rec.bytes, rec.contentLength = n, info.ContentLength
	if err != nil || classifyStream(n, info.ContentLength) != "full" {
		rec.outcome = "truncated"
	} else {
		rec.outcome = "full"
	}
	rec.contextIssued, rec.pcCalls = prep.pc.issued()
	calls, streamed := h.po.snapshot()
	rec.potCalls = calls
	if calls > 0 {
		rec.potCache = h.rt.lastVerdict()
	}
	// An arm that did not pre-mint has no token instant of its own, so the
	// daemon's first answered /get_pot is when the streamed token came into being.
	// A self-test is the exception: it mints inside the daemon, where the consumer
	// cannot see the instant, and selftest_done bounds it instead.
	if rec.tokenMinted.IsZero() && !arm.selfTest {
		rec.tokenMinted = h.rt.firstMint()
	}
	switch {
	case prep.preToken == "":
	case streamed == prep.preToken:
		rec.tokenSame = "yes"
	default:
		rec.tokenSame = "no"
	}

	// An arm that put a token in the daemon's cache before the stream only
	// measures an aged token if the consumer was served that same one.
	if (prep.preToken != "" || arm.selfTest) && rec.potCache != "hit" {
		rec.appendNote("consumer token fetch was not a cache hit (pot_cache=" + agingField(rec.potCache) + ")")
	}
	if arm.watchAnchor {
		// The last context-establishment wait of the iteration is the one that held
		// the streamed context back. No wait at all, or one anchored on the mint,
		// means this iteration did not measure the gap the arm names.
		anchors := h.sep.contextAnchors()
		switch {
		case len(anchors) == 0:
			rec.appendNote("no separation wait observed: the daemon did not hold the context back, so this iteration measured no gap")
		default:
			rec.anchor = anchors[len(anchors)-1]
			if rec.anchor != "proof" {
				rec.appendNote("separation anchor was the " + rec.anchor + ", not the proof: this iteration did not measure the proof edge")
			}
			if len(anchors) > 1 {
				rec.appendNote(fmt.Sprintf("%d context waits in this iteration (anchors %s); the last one is reported", len(anchors), strings.Join(anchors, ",")))
			}
		}
	}
	if info.Client != clientWebContext {
		rec.appendNote(fmt.Sprintf("client=%s (not the player-context path)", info.Client))
	}
	if err != nil {
		rec.appendNote("stream error: " + err.Error())
	}
	if len(warnings) > 0 {
		t.Logf("consumer warnings: %s", describeWarnings(warnings))
	}
}

// appendNote adds one more diagnostic without dropping an earlier one.
func (r *agingRecord) appendNote(s string) {
	if r.note == "" {
		r.note = s
		return
	}
	r.note += "; " + s
}

// agingIterations reads the per-arm iteration count.
func agingIterations(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(agingIterationsEnv))
	if raw == "" {
		return agingDefaultIterations
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		t.Fatalf("invalid %s=%q: want a positive integer", agingIterationsEnv, raw)
	}
	return n
}

// agingDelay reads the delay each aged arm inserts before streaming.
func agingDelay(t *testing.T) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(agingDelayEnv))
	if raw == "" {
		return agingDefaultDelay
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		t.Fatalf("invalid %s=%q: want a non-negative Go duration such as 30s", agingDelayEnv, raw)
	}
	return d
}

// agingSeparation reads the server.Config.MintSeparation override passed to
// every in-process daemon this suite starts. Unset returns 0, which leaves each
// daemon to resolve its own env-derived default (see resolveMintSeparation in
// the minter package) rather than disabling the gate: only a positive override
// takes effect, so removing the gate takes a small positive value such as "1ms",
// not "0".
func agingSeparation(t *testing.T) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(agingSeparationEnv))
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		t.Fatalf("invalid %s=%q: want a positive Go duration such as 1ms (only a positive value overrides the daemon's own default)", agingSeparationEnv, raw)
	}
	return d
}

// TestAgingMatrix runs every arm N times, one round at a time, and reports what
// each iteration did. It is opt-in and never fails on a capped stream; the only
// requirement is that every iteration produced a record.
func TestAgingMatrix(t *testing.T) {
	var arms []agingArm
	var label string
	raw := strings.TrimSpace(os.Getenv(agingEnableEnv))
	switch raw {
	case "1":
		arms, label = agingArms, "1 (which artifact's age separates a full stream from a capped one)"
	case "2":
		arms, label = agingMintGapArms, "2 (how much distance between the mint and the streamed context is enough)"
	case "3":
		arms, label = agingProofGapArms, "3 (how much distance between the proof playback and the streamed context is enough)"
	case "":
		t.Skipf("set %s=1, %s=2, or %s=3 to run an aging matrix (a long measurement run, not an assertion)", agingEnableEnv, agingEnableEnv, agingEnableEnv)
	default:
		t.Fatalf("%s=%q is not a recognized value; use 1, 2, or 3", agingEnableEnv, raw)
	}
	if ext := os.Getenv("WAXSEAL_URL"); ext != "" {
		t.Skipf("the aging matrix needs its own cold daemon per iteration; unset WAXSEAL_URL (currently %s)", ext)
	}
	n := agingIterations(t)
	d := agingDelay(t)
	sep := agingSeparation(t)
	// Matrix 3 makes the daemon's gate its instrument and sets it per arm, so a
	// run-wide override would fight every arm's own value rather than refine it.
	if raw == "3" && sep > 0 {
		t.Fatalf("%s=%s cannot be combined with %s=3: every arm of that matrix sets the gate to its own gap, which is what holds the context the measured distance after the proof", agingSeparationEnv, sep, agingEnableEnv)
	}
	t.Logf("aging matrix %s: %d arms x %d iterations, video %s", label, len(arms), n, bbbVideoID)
	t.Logf("run-wide delay %s (arms carrying their own gap ignore it)", d)
	switch {
	case raw == "3":
		t.Logf("%s unset, as this matrix requires: each arm sets the daemon's mint-to-establishment gate to its own gap, so the gate is the instrument that holds the streamed context exactly that long after the proof playback", agingSeparationEnv)
	case sep > 0:
		t.Logf("%s=%s: the daemon's mint-to-establishment gate is effectively removed; arms measure raw gaps", agingSeparationEnv, sep)
	default:
		t.Logf("%s unset: the daemon's own default mint-to-establishment gate applies; this run is a regression check (every arm expected full length)", agingSeparationEnv)
	}

	var records []*agingRecord
	// The rounds are interleaved so a drift in YouTube's behavior over the run
	// lands on every arm in the same round instead of on whichever arm ran last.
	for i := 1; i <= n; i++ {
		for _, arm := range arms {
			rec := &agingRecord{arm: arm.name, iter: i}
			records = append(records, rec)
			t.Run(fmt.Sprintf("%s_iter%d", arm.name, i), func(t *testing.T) {
				// Emitted from a defer so an iteration that aborts still leaves its
				// record behind.
				defer func() { t.Log(rec.line()) }()
				runAgingIteration(t, arm, rec, d, sep)
			})
		}
	}
	agingSummary(t, records)
	// Every record is appended before its t.Run starts, so len(records) alone
	// cannot catch an iteration that aborted partway (a fatal setup failure, or a
	// t.Fatal inside agingStream): the record exists either way. outcome and
	// streamStart are the fields runAgingIteration sets on every path it can
	// return from (a full stream, a truncated one, or a stream error), so a
	// record still missing either one never reached that point.
	for _, r := range records {
		if r.outcome == "" || r.streamStart.IsZero() {
			t.Errorf("arm=%s iter=%d did not complete: outcome=%q stream_start=%s",
				r.arm, r.iter, r.outcome, agingTime(r.streamStart))
		}
	}
}

// agingSummary prints the per-arm tally and, for every iteration that did not
// stream full length, how old each artifact was when its stream began.
func agingSummary(t *testing.T, records []*agingRecord) {
	t.Helper()
	type tally struct{ full, total int }
	byArm := make(map[string]*tally)
	var order []string
	for _, r := range records {
		tl, ok := byArm[r.arm]
		if !ok {
			tl = &tally{}
			byArm[r.arm] = tl
			order = append(order, r.arm)
		}
		tl.total++
		if r.outcome == "full" {
			tl.full++
		}
	}
	t.Log("aging matrix summary (arm: full/N)")
	for _, a := range order {
		tl := byArm[a]
		t.Logf("  %-16s %d/%d", a, tl.full, tl.total)
	}
	// An arm that watched the separation anchor is only meaningful when every one
	// of its iterations waited on the anchor it names, so the tally is printed
	// beside the outcomes rather than left to be grepped out of the per-iteration
	// lines.
	anchored := false
	for _, r := range records {
		if r.anchor != "" || strings.Contains(r.note, "separation") {
			anchored = true
			break
		}
	}
	if anchored {
		t.Log("separation anchor each iteration waited on (an arm measures the proof edge only at proof for every iteration):")
		for _, a := range order {
			counts := make(map[string]int)
			for _, r := range records {
				if r.arm != a {
					continue
				}
				counts[agingField(r.anchor)]++
			}
			var parts []string
			for _, key := range []string{"proof", "mint", "-"} {
				if counts[key] > 0 {
					parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
				}
			}
			t.Logf("  %-16s %s", a, strings.Join(parts, " "))
		}
	}
	t.Log("iterations that did not stream full length, artifact age at stream start:")
	anyTruncated := false
	for _, r := range records {
		if r.outcome == "full" {
			continue
		}
		anyTruncated = true
		t.Logf("  %-16s iter=%d outcome=%s gap=%s session=%s token=%s url=%s bytes=%d of=%d",
			r.arm, r.iter, r.status(), agingField(r.gap),
			agingAge(r.warmDone, r.streamStart),
			agingAge(r.tokenMinted, r.streamStart),
			agingAge(r.contextIssued, r.streamStart),
			r.bytes, r.contentLength)
	}
	if !anyTruncated {
		t.Log("  none")
	}
}
