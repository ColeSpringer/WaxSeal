package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/cdp"
)

// This file holds the offline page fake and the tests it makes possible. Before
// it, the establish, confirm, proof, and identity-capture loops were reachable
// only through a real Chromium, so every change to them was verified by a
// network run. The fake is a scripted pageDriver: each Eval is answered from a
// list keyed by the JS constant the production code passes, so a test states
// what the page reports and when, and the loops run in milliseconds.

// fakeStep is one scripted answer to an Eval. Exactly one of value, err, and
// block is meaningful.
type fakeStep struct {
	value json.RawMessage // returned as EvalResult.Value
	err   error           // returned instead of a value
	block bool            // block until the page's context is done, then return its error
	delay time.Duration   // wait this long first, so a deadline can expire inside the Eval
}

// jsBool scripts a bare JSON boolean, which is what the ready, load, drive, and
// seek snippets return.
func jsBool(b bool) fakeStep {
	if b {
		return fakeStep{value: json.RawMessage("true")}
	}
	return fakeStep{value: json.RawMessage("false")}
}

// jsStringified scripts what a JSON.stringify(...) snippet returns: a JSON
// string whose contents are the marshalled value. The extract, buffered, and
// identity snippets all report this way.
func jsStringified(t *testing.T, v any) fakeStep {
	t.Helper()
	inner, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal scripted value: %v", err)
	}
	outer, err := json.Marshal(string(inner))
	if err != nil {
		t.Fatalf("marshal scripted string: %v", err)
	}
	return fakeStep{value: outer}
}

// jsErr scripts an Eval failure, the way a CDP hiccup or a closed page surfaces.
func jsErr(err error) fakeStep { return fakeStep{err: err} }

// jsBlock scripts an Eval that never returns until the caller's context is done.
// It is how a test drives a loop to its deadline.
func jsBlock() fakeStep { return fakeStep{block: true} }

// jsSlowErr scripts an Eval that takes d and then fails with err. A caller's
// deadline can expire inside it, which is the only way to reach the branches
// that have to tell a timed-out call apart from a page that actually broke.
func jsSlowErr(d time.Duration, err error) fakeStep { return fakeStep{err: err, delay: d} }

// fakeCall is one Eval the session made: which snippet, and what it passed. The
// arguments matter as much as the snippet does. The page fake answers from a
// table keyed by the JS alone, so without recording them a session that loaded
// or extracted the wrong video would satisfy every scripted answer.
type fakeCall struct {
	js   string
	args []any
}

// fakePageState is the scripted state shared by every context-bound copy of a
// fakePage, the way a *cdp.Page's copies share one CDP session.
type fakePageState struct {
	mu         sync.Mutex
	steps      map[string][]fakeStep
	calls      []fakeCall // every Eval, in order
	cookies    []*cdp.Cookie
	cookiesErr error
	navigated  []string
	crash      string

	// wantVideoID, when set, makes Eval fail any load or extract call that names a
	// different video. The scripted answers are keyed by the snippet alone, so
	// without this a session that pointed the player at the wrong id, or asked for
	// the wrong one back, would be handed the same canned payload and pass.
	wantVideoID string
}

// fakePage is a pageDriver backed by fakePageState. It is a value so Context can
// return a copy with a different call context while sharing that state.
type fakePage struct {
	*fakePageState
	ctx context.Context
}

// newFakePage returns a page that answers the named snippets from steps. A
// snippet with no entry answers with a bare true, which satisfies the ready,
// drive, seek, and cleanup snippets whose result a test does not care about. A
// snippet whose list runs out repeats its last entry, so a polling loop can be
// held on one answer without scripting it per poll.
func newFakePage(steps map[string][]fakeStep) fakePage {
	return fakePage{&fakePageState{steps: steps}, context.Background()}
}

// newFakePageFor is newFakePage for a page that is expected to be driven at one
// video, and fails any load or extract call that names another.
func newFakePageFor(videoID string, steps map[string][]fakeStep) fakePage {
	p := newFakePage(steps)
	p.wantVideoID = videoID
	return p
}

// errWrongVideo reports a snippet called with a video the test did not expect.
var errWrongVideo = errors.New("fake page: snippet called for the wrong video")

func (p fakePage) Context(ctx context.Context) pageDriver {
	return fakePage{p.fakePageState, ctx}
}

func (p fakePage) Eval(js string, args ...any) (cdp.EvalResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, fakeCall{js: js, args: args})
	if wrong := p.wrongVideoLocked(js, args); wrong != nil {
		p.mu.Unlock()
		return cdp.EvalResult{}, wrong
	}
	list := p.steps[js]
	var step fakeStep
	switch {
	case len(list) == 0:
		step = jsBool(true)
	case len(list) == 1:
		step = list[0] // the last entry repeats
	default:
		step, p.steps[js] = list[0], list[1:]
	}
	p.mu.Unlock()

	if step.block {
		<-p.ctx.Done()
		return cdp.EvalResult{}, p.ctx.Err()
	}
	if step.delay > 0 {
		time.Sleep(step.delay)
	}
	if step.err != nil {
		return cdp.EvalResult{}, step.err
	}
	// A real Eval binds to the page's context, so a done context fails the call
	// even when an answer is scripted.
	if err := p.ctx.Err(); err != nil {
		return cdp.EvalResult{}, err
	}
	return cdp.EvalResult{Value: step.value}, nil
}

// wrongVideoLocked reports a video-scoped snippet called for a video other than
// wantVideoID. The caller holds mu.
func (p fakePage) wrongVideoLocked(js string, args []any) error {
	if p.wantVideoID == "" || (js != playerLoadJS && js != playerContextExtractJS) {
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("%w: called with no video id, want %q", errWrongVideo, p.wantVideoID)
	}
	got, ok := args[0].(string)
	if !ok || got != p.wantVideoID {
		return fmt.Errorf("%w: called with %v, want %q", errWrongVideo, args[0], p.wantVideoID)
	}
	return nil
}

func (p fakePage) Cookies([]string) ([]*cdp.Cookie, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cookies, p.cookiesErr
}

func (p fakePage) Navigate(url string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.navigated = append(p.navigated, url)
	return nil
}

func (p fakePage) WaitLoad() error                                             { return nil }
func (p fakePage) SetBypassCSP(bool) error                                     { return nil }
func (p fakePage) SetUserAgentOverride(*cdp.NetworkSetUserAgentOverride) error { return nil }
func (p fakePage) WaitCrash(context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.crash
}

// evalCount reports how many times js was evaluated.
func (p fakePage) evalCount(js string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, call := range p.calls {
		if call.js == js {
			n++
		}
	}
	return n
}

// evalArgs returns the arguments of every call to js, in order.
func (p fakePage) evalArgs(js string) [][]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out [][]any
	for _, call := range p.calls {
		if call.js == js {
			out = append(out, call.args)
		}
	}
	return out
}

// assertEveryCallCarries fails unless every call to js passed want as its first
// argument. It is what keeps the scripted answers honest: the fake keys on the
// snippet alone, so nothing else would notice a session asking about the wrong
// video.
func assertEveryCallCarries(t *testing.T, page fakePage, js, label, want string) {
	t.Helper()
	calls := page.evalArgs(js)
	if len(calls) == 0 {
		t.Errorf("%s was never evaluated", label)
		return
	}
	for i, args := range calls {
		if len(args) == 0 {
			t.Errorf("%s call %d passed no arguments, want %q", label, i, want)
			continue
		}
		if got, ok := args[0].(string); !ok || got != want {
			t.Errorf("%s call %d passed %v, want %q", label, i, args[0], want)
		}
	}
}

// fastTiming is defaultTiming with every wait scaled down so a loop that polls
// the page finishes in milliseconds. The ratios are kept roughly as shipped so a
// test exercises the same relative ordering of budgets.
func fastTiming() timing {
	return timing{
		poll:               2 * time.Millisecond,
		identityTimeout:    200 * time.Millisecond,
		clientVersionGrace: 20 * time.Millisecond,
		establishTimeout:   200 * time.Millisecond,
		confirmBudget:      100 * time.Millisecond,
		reReadBudget:       50 * time.Millisecond,
		probeBudget:        200 * time.Millisecond,
		stallWindow:        40 * time.Millisecond,
		hardTimeout:        2 * time.Second,
	}
}

// newFakeSession returns a session driving page with fastTiming and a discarding
// logger.
func newFakeSession(page fakePage) *Session {
	return &Session{page: page, log: slog.New(slog.DiscardHandler), timing: fastTiming()}
}

// establishedPayload is what the extract snippet returns once the video has
// loaded and buffered: no error, and the fields a consumer needs.
func establishedPayload(videoID string, lengthSeconds int) map[string]any {
	return map[string]any{
		"playability_status":              "OK",
		"video_id_match":                  true,
		"player_url":                      "https://www.youtube.com/s/player/abc/base.js",
		"server_abr_streaming_url":        "https://r1.googlevideo.com/videoplayback?id=" + videoID,
		"video_playback_ustreamer_config": "cfg",
		"visitor_data":                    "VD",
		"client_version":                  "2.x",
		"title":                           "a title",
		"author":                          "an author",
		"length_seconds":                  lengthSeconds,
		"channel_id":                      "UCabc",
		"description":                     "a description",
		"thumbnails": []map[string]any{
			{"url": "https://i.ytimg.com/vi/" + videoID + "/default.jpg", "width": 120, "height": 90},
		},
		"is_live_content": false,
		"is_live_now":     false,
		"is_upcoming":     false,
		"publish_date":    "2015-04-10T00:00:00-07:00",
		"audio_formats": []map[string]any{
			{"itag": 251, "lmt": "171", "mime_type": "audio/webm", "bitrate": 130000},
		},
	}
}

// pendingPayload is what the extract snippet returns while the player is still
// working: the error field is set and the evidence fields say the response is
// not terminal.
func pendingPayload(reason string) map[string]any {
	return map[string]any{"error": reason, "playability_status": "", "video_id_match": false}
}

// bufferedPayload is one playerBufferedJS sample.
func bufferedPayload(current, bufferedEnd float64, coversTarget bool) map[string]any {
	return map[string]any{
		"current": current, "buffered_end": bufferedEnd, "covers_target": coversTarget,
		"state": 1, "player_error": 0, "abr_url": "https://r1.googlevideo.com/videoplayback?id=vid",
	}
}

// A single transient CDP error in the ready probe must not fail establishment.
// Phase 2 and reReadContext have always tolerated two; before this, one hiccup
// here failed the whole request.
func TestEstablishToleratesTransientReadyProbeError(t *testing.T) {
	page := newFakePageFor("vid", map[string][]fakeStep{
		playerReadyJS:          {jsErr(errors.New("cdp: transient")), jsBool(true)},
		playerContextExtractJS: {jsStringified(t, establishedPayload("vid", 600))},
	})
	s := newFakeSession(page)

	raw, err := s.establish(context.Background(), page, "vid", time.Now().Add(s.timing.establishTimeout))
	if err != nil {
		t.Fatalf("establish: %v", err)
	}
	if raw.ServerAbrStreamingURL == "" {
		t.Error("established context has no server_abr_streaming_url")
	}
	if got := page.evalCount(playerReadyJS); got != 2 {
		t.Errorf("ready probe ran %d times, want 2 (one error then one success)", got)
	}
}

// Every video-scoped snippet must be handed the video the caller asked for. The
// page fake answers from a table keyed by the snippet alone, so nothing else in
// this file would notice a session that pointed the player at one video and then
// read the context of another; that mis-wiring would reach a consumer as a
// streaming URL for the wrong video.
func TestEstablishPassesTheRequestedVideoIDToThePage(t *testing.T) {
	const want = "requested-id"
	page := newFakePage(map[string][]fakeStep{
		playerContextExtractJS: {
			jsStringified(t, pendingPayload("pending: no serverAbrStreamingUrl")),
			jsStringified(t, establishedPayload(want, 635)),
		},
	})
	s := newFakeSession(page)

	if _, err := s.establish(context.Background(), page, want, time.Now().Add(s.timing.establishTimeout)); err != nil {
		t.Fatalf("establish: %v", err)
	}
	assertEveryCallCarries(t, page, playerLoadJS, "loadVideoById", want)
	assertEveryCallCarries(t, page, playerContextExtractJS, "the context extraction", want)

	// And the guard the other tests rely on must actually refuse a mismatch,
	// rather than passing because it never fires.
	strict := newFakePageFor(want, map[string][]fakeStep{
		playerContextExtractJS: {jsStringified(t, establishedPayload(want, 635))},
	})
	st := newFakeSession(strict)
	_, err := st.establish(context.Background(), strict, "some-other-id", time.Now().Add(st.timing.establishTimeout))
	if !errors.Is(err, errWrongVideo) {
		t.Errorf("establish with a mismatched id = %v, want it refused by the fake", err)
	}
}

// Three consecutive ready-probe errors are a closed or crashed page, not a
// hiccup, and must fail before the deadline rather than spin.
func TestEstablishFailsAfterThreeReadyProbeErrors(t *testing.T) {
	boom := errors.New("cdp: page closed")
	page := newFakePage(map[string][]fakeStep{playerReadyJS: {jsErr(boom)}})
	s := newFakeSession(page)

	start := time.Now()
	_, err := s.establish(context.Background(), page, "vid", time.Now().Add(s.timing.establishTimeout))
	if !errors.Is(err, boom) {
		t.Fatalf("establish error = %v, want it to wrap %v", err, boom)
	}
	if elapsed := time.Since(start); elapsed >= s.timing.establishTimeout {
		t.Errorf("establish took %v, want it to fail fast rather than spin to the %v deadline", elapsed, s.timing.establishTimeout)
	}
	if got := page.evalCount(playerReadyJS); got != 3 {
		t.Errorf("ready probe ran %d times, want 3 (the three-strike rule)", got)
	}
}

// establish returns as soon as the extraction reports an established context,
// and polls until then.
func TestEstablishReturnsContextOnceBuffered(t *testing.T) {
	page := newFakePageFor("vid", map[string][]fakeStep{
		playerContextExtractJS: {
			jsStringified(t, pendingPayload("pending: player response not yet for vid")),
			jsStringified(t, pendingPayload("pending: session not established (no buffered media yet)")),
			jsStringified(t, establishedPayload("vid", 635)),
		},
	})
	s := newFakeSession(page)

	raw, err := s.establish(context.Background(), page, "vid", time.Now().Add(s.timing.establishTimeout))
	if err != nil {
		t.Fatalf("establish: %v", err)
	}
	if raw.LengthSeconds != 635 || raw.Error != "" {
		t.Errorf("raw = %+v, want the established payload", raw)
	}
	if got := page.evalCount(playerContextExtractJS); got != 3 {
		t.Errorf("extract ran %d times, want 3 (two pending polls then the context)", got)
	}
	// The drive snippet keeps muted playback alive and must run on every poll.
	if got := page.evalCount(playerDriveJS); got != 3 {
		t.Errorf("drive ran %d times, want one per extract poll", got)
	}
}

// A terminal playabilityStatus for the requested video ends establishment at
// once with ErrUnplayable, so the minter negative-caches the video instead of
// relaunching.
func TestEstablishUnplayableIsTerminal(t *testing.T) {
	payload := map[string]any{
		"error": "unplayable: LOGIN_REQUIRED", "playability_status": "LOGIN_REQUIRED",
		"reason": "Sign in to confirm your age", "video_id_match": true,
	}
	page := newFakePageFor("vid", map[string][]fakeStep{
		playerContextExtractJS: {jsStringified(t, payload)},
	})
	s := newFakeSession(page)

	_, err := s.establish(context.Background(), page, "vid", time.Now().Add(s.timing.establishTimeout))
	if !errors.Is(err, ErrUnplayable) {
		t.Fatalf("establish error = %v, want ErrUnplayable", err)
	}
	var ue *UnplayableError
	if !errors.As(err, &ue) || ue.Status != "LOGIN_REQUIRED" {
		t.Errorf("error = %v, want an UnplayableError carrying LOGIN_REQUIRED", err)
	}
	if got := page.evalCount(playerContextExtractJS); got != 1 {
		t.Errorf("extract ran %d times, want 1 (a terminal status must not be polled)", got)
	}
}

// A bot check reaches establish the same way a per-video verdict does, but it
// describes the browser session, so it must not unwrap to ErrUnplayable: the
// minter relaunches on it instead of negative-caching the video.
func TestEstablishBotCheckIsSessionLevel(t *testing.T) {
	payload := map[string]any{
		"error": "unplayable: LOGIN_REQUIRED", "playability_status": "LOGIN_REQUIRED",
		"reason": "Sign in to confirm you\u2019re not a bot", "video_id_match": true,
	}
	page := newFakePageFor("vid", map[string][]fakeStep{
		playerContextExtractJS: {jsStringified(t, payload)},
	})
	s := newFakeSession(page)

	_, err := s.establish(context.Background(), page, "vid", time.Now().Add(s.timing.establishTimeout))
	if !errors.Is(err, ErrBotCheck) {
		t.Fatalf("establish error = %v, want ErrBotCheck", err)
	}
	if errors.Is(err, ErrUnplayable) {
		t.Fatal("the bot check unwrapped to ErrUnplayable; the video would be negative-cached")
	}
	bc, ok := errors.AsType[*BotCheckError](err)
	if !ok || bc.Status != "LOGIN_REQUIRED" {
		t.Errorf("error = %v, want a BotCheckError carrying LOGIN_REQUIRED", err)
	}
	if got := page.evalCount(playerContextExtractJS); got != 1 {
		t.Errorf("extract ran %d times, want 1 (a terminal status must not be polled)", got)
	}
}

// The shape YouTube actually refuses with: a LOGIN_REQUIRED playability status
// and no videoDetails, so the extract reports video_id_match false and the page
// looks like it is still loading. The wall must still be recognised on the first
// poll, or it hides behind the establish deadline and the session is never
// relaunched.
func TestEstablishBotCheckWithoutVideoDetails(t *testing.T) {
	payload := map[string]any{
		"error": "pending: player response not yet for vid", "playability_status": "LOGIN_REQUIRED",
		"reason": "Sign in to confirm you\u2019re not a bot", "video_id_match": false,
	}
	page := newFakePageFor("vid", map[string][]fakeStep{
		playerContextExtractJS: {jsStringified(t, payload)},
	})
	s := newFakeSession(page)

	_, err := s.establish(context.Background(), page, "vid", time.Now().Add(s.timing.establishTimeout))
	if !errors.Is(err, ErrBotCheck) {
		t.Fatalf("establish error = %v, want ErrBotCheck", err)
	}
	if got := page.evalCount(playerContextExtractJS); got != 1 {
		t.Errorf("extract ran %d times, want 1 (the wall must not be polled through)", got)
	}
}

// A bot check met while proving comes back as an error, not a nil error with a
// not-established outcome, so the caller can tell a walled session apart from
// one that merely failed to establish.
func TestProveFullLengthReturnsBotCheck(t *testing.T) {
	payload := map[string]any{
		"error": "unplayable: LOGIN_REQUIRED", "playability_status": "LOGIN_REQUIRED",
		"reason": "Sign in to confirm you\u2019re not a bot", "video_id_match": true,
	}
	page := newFakePageFor("long", map[string][]fakeStep{
		playerContextExtractJS: {jsStringified(t, payload)},
	})
	s := newFakeSession(page)

	probe, err := s.VerifyFullLength(context.Background(), "long")
	if !errors.Is(err, ErrBotCheck) {
		t.Fatalf("VerifyFullLength error = %v, want ErrBotCheck", err)
	}
	if errors.Is(err, ErrUnplayable) {
		t.Fatal("the bot check unwrapped to ErrUnplayable")
	}
	if probe.Outcome != OutcomeNotEstablished {
		t.Errorf("outcome = %q, want %q", probe.Outcome, OutcomeNotEstablished)
	}
	if s.Established() {
		t.Error("a walled proof marked the session established")
	}
}

// A deadline reached while the page is still pending reports the page's own last
// reason, not a bare context error: that string is what an operator reads.
func TestEstablishDeadlineReportsLastError(t *testing.T) {
	page := newFakePageFor("vid", map[string][]fakeStep{
		playerContextExtractJS: {jsStringified(t, pendingPayload("pending: no serverAbrStreamingUrl"))},
	})
	s := newFakeSession(page)

	_, err := s.establish(context.Background(), page, "vid", time.Now().Add(30*time.Millisecond))
	if err == nil {
		t.Fatal("establish returned no error at the deadline")
	}
	if !strings.Contains(err.Error(), "pending: no serverAbrStreamingUrl") {
		t.Errorf("error = %v, want it to carry the page's last reason", err)
	}
}

// The establish deadline is checked before each poll, so a budget expiry reports
// what the page was last waiting for. With no reason yet recorded, the message
// says the page never answered rather than coming back bare.
func TestEstablishDeadlineWithNoReasonNamesIt(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{
		playerContextExtractJS: {jsBlock()},
	})
	s := newFakeSession(page)

	_, err := s.establish(context.Background(), page, "vid", time.Now().Add(20*time.Millisecond))
	if err == nil {
		t.Fatal("establish returned no error at the deadline")
	}
	if !strings.Contains(err.Error(), "no player response before the deadline") {
		t.Errorf("error = %v, want it to say the page never answered", err)
	}
}

// An extraction that fails for a real reason must say so even when the deadline
// has also passed. The deadline is checked before each poll, so the only way to
// reach this is for the deadline to expire inside an Eval; the page's last
// pending reason must not be substituted for a genuine failure.
func TestEstablishGenuineEvalErrorSurvivesAnExpiredDeadline(t *testing.T) {
	boom := errors.New("cdp: eval exception: extraction threw")
	page := newFakePage(map[string][]fakeStep{
		playerContextExtractJS: {
			jsStringified(t, pendingPayload("pending: no serverAbrStreamingUrl")),
			// Outlives the deadline set below, so the branch sees both a real error
			// and an expired budget.
			jsSlowErr(60*time.Millisecond, boom),
		},
	})
	s := newFakeSession(page)

	_, err := s.establish(context.Background(), page, "vid", time.Now().Add(30*time.Millisecond))
	if err == nil {
		t.Fatal("establish returned no error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the eval failure rather than report the last pending reason", err)
	}
}

// The confirm accepts as soon as the acceptor does, and reports the sample it
// accepted on.
func TestConfirmPastCapConfirmsPastTarget(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{
		playerBufferedJS: {
			jsStringified(t, bufferedPayload(1, 10, false)),
			jsStringified(t, bufferedPayload(50, 80, false)),
			jsStringified(t, bufferedPayload(103, 140, true)),
		},
	})
	s := newFakeSession(page)

	probe, err := s.confirmPastCap(context.Background(), page, 100, fullLengthTolSecs, "established", time.Now().Add(s.timing.confirmBudget),
		func(b bufferedSample) bool { return b.Current > 102 && b.CoversTarget })
	if err != nil {
		t.Fatalf("confirmPastCap: %v", err)
	}
	if probe.Outcome != OutcomeFullLength || !probe.FullLength {
		t.Fatalf("outcome = %q (%s), want %q", probe.Outcome, probe.Reason, OutcomeFullLength)
	}
	if probe.CurrentSeconds != 103 || probe.BufferedEnd != 140 {
		t.Errorf("probe = %+v, want the accepted sample's positions", probe)
	}
	if got := page.evalCount(playerSeekJS); got != 1 {
		t.Errorf("seek ran %d times, want exactly 1", got)
	}
}

// Playback that never advances is target-not-buffered, which the minter treats
// as a status-2 timing failure, not a wedged page.
func TestConfirmPastCapStallIsUnconfirmed(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{
		playerBufferedJS: {jsStringified(t, bufferedPayload(1, 10, false))},
	})
	s := newFakeSession(page)

	probe, err := s.confirmPastCap(context.Background(), page, 100, fullLengthTolSecs, "established", time.Now().Add(s.timing.confirmBudget),
		func(b bufferedSample) bool { return b.CoversTarget })
	if err != nil {
		t.Fatalf("confirmPastCap: %v", err)
	}
	if probe.Outcome != OutcomeTargetNotBuffered {
		t.Fatalf("outcome = %q (%s), want %q", probe.Outcome, probe.Reason, OutcomeTargetNotBuffered)
	}
	if !strings.Contains(probe.Reason, "stalled") {
		t.Errorf("reason = %q, want the stall wording (the budget should not have been reached first)", probe.Reason)
	}
	if err := confirmError(probe); !errors.Is(err, ErrStatus2Unconfirmed) {
		t.Errorf("confirmError = %v, want ErrStatus2Unconfirmed (retried in place, never relaunched)", err)
	}
}

// A seek that returns false is a page that could not run the confirm at all.
// That stays confirm-unavailable so the minter can relaunch, rather than being
// read as a capped stream.
func TestConfirmPastCapSeekFailureIsUnavailable(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{playerSeekJS: {jsBool(false)}})
	s := newFakeSession(page)

	probe, err := s.confirmPastCap(context.Background(), page, 100, fullLengthTolSecs, "established", time.Now().Add(s.timing.confirmBudget),
		func(bufferedSample) bool { return true })
	if err != nil {
		t.Fatalf("confirmPastCap: %v", err)
	}
	if probe.Outcome != OutcomeConfirmUnavailable {
		t.Fatalf("outcome = %q (%s), want %q", probe.Outcome, probe.Reason, OutcomeConfirmUnavailable)
	}
	if probe.Reason != "movie_player.seekTo unavailable" {
		t.Errorf("reason = %q", probe.Reason)
	}
	if err := confirmError(probe); errors.Is(err, ErrStatus2Unconfirmed) {
		t.Error("confirmError classified an unrunnable confirm as a status-2 timing failure")
	}
}

// seekTo returns at once, so a seek that outlives the whole confirm budget is
// page trouble, not status-2 timing, and keeps the confirm-unavailable class.
// Only the reason names the budget rather than quoting the derived context's
// deadline error.
func TestConfirmPastCapSeekBudgetExpiryIsUnavailable(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{playerSeekJS: {jsBlock()}})
	s := newFakeSession(page)

	probe, err := s.confirmPastCap(context.Background(), page, 100, fullLengthTolSecs, "established", time.Now().Add(20*time.Millisecond),
		func(bufferedSample) bool { return true })
	if err != nil {
		t.Fatalf("confirmPastCap returned an error for a budget expiry: %v", err)
	}
	if probe.Outcome != OutcomeConfirmUnavailable {
		t.Fatalf("outcome = %q (%s), want %q: the page, not the grade, is what failed", probe.Outcome, probe.Reason, OutcomeConfirmUnavailable)
	}
	if !strings.Contains(probe.Reason, "confirm budget expired during the seek") {
		t.Errorf("reason = %q, want it to name the budget", probe.Reason)
	}
}

// A seek that fails for a real reason must say so even when the confirm budget
// has also run out. Only the budget's own deadline error is rewritten as a
// budget expiry; rewriting every failure past the deadline would throw away the
// one line that says what the page did.
func TestConfirmPastCapSeekErrorSurvivesAnExpiredBudget(t *testing.T) {
	boom := errors.New("cdp: eval exception: seekTo threw")
	page := newFakePage(map[string][]fakeStep{playerSeekJS: {jsErr(boom)}})
	s := newFakeSession(page)

	// A deadline already in the past, so dctx is done before the seek is scripted
	// to fail. The fake answers with the scripted error rather than the context's.
	probe, err := s.confirmPastCap(context.Background(), page, 100, fullLengthTolSecs, "established", time.Now().Add(-time.Second),
		func(bufferedSample) bool { return true })
	if err != nil {
		t.Fatalf("confirmPastCap: %v", err)
	}
	if probe.Outcome != OutcomeConfirmUnavailable {
		t.Fatalf("outcome = %q, want %q", probe.Outcome, OutcomeConfirmUnavailable)
	}
	if !strings.Contains(probe.Reason, boom.Error()) {
		t.Errorf("reason = %q, want it to carry the seek error rather than a budget message", probe.Reason)
	}
}

// A caller cancellation during the confirm is reported as a cancellation, not as
// a failed grade.
func TestConfirmPastCapCallerCancellationIsCanceled(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{playerBufferedJS: {jsStringified(t, bufferedPayload(1, 10, false))}})
	s := newFakeSession(page)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probe, err := s.confirmPastCap(ctx, page, 100, fullLengthTolSecs, "established", time.Now().Add(time.Second),
		func(bufferedSample) bool { return true })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if probe.Outcome != OutcomeCanceled {
		t.Errorf("outcome = %q, want %q", probe.Outcome, OutcomeCanceled)
	}
}

// VerifyFullLength records its verdict in LastProof either way, and only a
// full-length proof marks the session established.
func TestProveFullLengthTooShortAndFullLength(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		page := newFakePageFor("short", map[string][]fakeStep{
			playerContextExtractJS: {jsStringified(t, establishedPayload("short", 60))},
		})
		s := newFakeSession(page)

		probe, err := s.VerifyFullLength(context.Background(), "short")
		if err != nil {
			t.Fatalf("VerifyFullLength: %v", err)
		}
		if probe.Outcome != OutcomeVideoTooShort {
			t.Fatalf("outcome = %q (%s), want %q", probe.Outcome, probe.Reason, OutcomeVideoTooShort)
		}
		if s.Established() {
			t.Error("a too-short probe marked the session established")
		}
		last, at := s.LastProof()
		if at.IsZero() || last.Outcome != OutcomeVideoTooShort || last.ElapsedMs < 0 {
			t.Errorf("LastProof = %+v at %v, want the recorded too-short probe", last, at)
		}
		// The seek must never run: the candidate was rejected before the confirm.
		if got := page.evalCount(playerSeekJS); got != 0 {
			t.Errorf("seek ran %d times for a too-short video", got)
		}
	})

	t.Run("full length", func(t *testing.T) {
		page := newFakePageFor("long", map[string][]fakeStep{
			playerContextExtractJS: {jsStringified(t, establishedPayload("long", 635))},
			playerBufferedJS: {
				jsStringified(t, bufferedPayload(20, 40, false)),
				jsStringified(t, bufferedPayload(103, 140, true)),
			},
		})
		s := newFakeSession(page)
		// EnsureEstablished below proves on the session's landing video, falling
		// through to proofCandidates when it is empty. Name it, or the proof would
		// ask this page about DefaultVideo instead.
		s.landingVideo = "long"

		probe, err := s.VerifyFullLength(context.Background(), "long")
		if err != nil {
			t.Fatalf("VerifyFullLength: %v", err)
		}
		if probe.Outcome != OutcomeFullLength || !probe.FullLength {
			t.Fatalf("outcome = %q (%s), want %q", probe.Outcome, probe.Reason, OutcomeFullLength)
		}
		if probe.VideoID != "long" || probe.LengthSeconds != 635 {
			t.Errorf("probe = %+v, want the video id and length carried through", probe)
		}
		// VerifyFullLength is the diagnostic entry point: it records the proof but
		// does not itself mark the session established. EnsureEstablished does.
		if s.Established() {
			t.Error("VerifyFullLength marked the session established; only EnsureEstablished does that")
		}
		last, at := s.LastProof()
		if at.IsZero() || !last.FullLength {
			t.Errorf("LastProof = %+v at %v, want the full-length verdict", last, at)
		}
		if err := s.EnsureEstablished(context.Background()); err != nil {
			t.Fatalf("EnsureEstablished: %v", err)
		}
		if !s.Established() {
			t.Error("EnsureEstablished did not mark the session established after a full-length proof")
		}
	})
}

// identityPayload is one identityCaptureJS result.
func identityPayload(vd, cv string) map[string]any {
	return map[string]any{"vd": vd, "cv": cv, "key": "K", "ua": "UA", "wd": false}
}

// A page that exposes visitor_data but never a client version yields a usable
// session on the pinned fallback rather than failing over one late field.
func TestCaptureIdentityGraceUsesPinnedVersion(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{
		identityCaptureJS: {jsStringified(t, identityPayload("VD", ""))},
	})
	s := newFakeSession(page)

	start := time.Now()
	if err := s.captureIdentity(context.Background(), "https://youtube.com/watch?v=vid"); err != nil {
		t.Fatalf("captureIdentity: %v", err)
	}
	if s.id.VisitorData != "VD" {
		t.Errorf("visitor_data = %q, want the partial capture to be held", s.id.VisitorData)
	}
	if s.id.ClientVersion == "" {
		t.Error("client_version is empty; /session would export a session with no version at all")
	}
	// The grace window, not the whole capture budget, is what bounds the wait.
	if elapsed := time.Since(start); elapsed >= s.timing.identityTimeout {
		t.Errorf("capture took %v, want it bounded by the %v grace", elapsed, s.timing.clientVersionGrace)
	}
}

// Without visitor_data there is no identity at all, so the capture fails.
func TestCaptureIdentityFailsWithoutVisitorData(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{
		identityCaptureJS: {jsStringified(t, identityPayload("", ""))},
	})
	s := newFakeSession(page)

	err := s.captureIdentity(context.Background(), "https://youtube.com/watch?v=vid")
	if err == nil {
		t.Fatal("captureIdentity succeeded with no visitor_data")
	}
	if !strings.Contains(err.Error(), "visitor_data") {
		t.Errorf("error = %v, want it to name visitor_data", err)
	}
}

// setupSession shares one navigation budget across the client-hint capture, the
// navigation, this capture, and the signature timestamp, so the caller's
// deadline is often tighter than the capture's own. Clamping to it leaves the
// pinned-version fallback a poll to fire in, instead of the loop dying on a bare
// context error.
func TestCaptureIdentityClampsToContextDeadline(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{
		identityCaptureJS: {jsStringified(t, identityPayload("VD", ""))},
	})
	s := newFakeSession(page)
	// A grace longer than the caller's remaining budget: without the clamp the
	// loop would wait for the grace and be cut off by the context instead.
	s.timing.clientVersionGrace = time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.captureIdentity(ctx, "https://youtube.com/watch?v=vid"); err != nil {
		t.Fatalf("captureIdentity: %v", err)
	}
	if s.id.VisitorData != "VD" || s.id.ClientVersion == "" {
		t.Errorf("identity = %+v, want the partial capture held on the pinned version", s.id)
	}
}

// The captured identity carries the browser's cookie count, which is what tells
// an operator the guest session actually exists.
func TestCaptureIdentityReadsCookies(t *testing.T) {
	page := newFakePage(map[string][]fakeStep{
		identityCaptureJS: {jsStringified(t, identityPayload("VD", "2.99"))},
	})
	page.cookies = []*cdp.Cookie{{Name: "VISITOR_INFO1_LIVE"}, {Name: "YSC"}}
	s := newFakeSession(page)

	if err := s.captureIdentity(context.Background(), "https://youtube.com/watch?v=vid"); err != nil {
		t.Fatalf("captureIdentity: %v", err)
	}
	if s.id.Cookies != 2 || s.id.ClientVersion != "2.99" || s.id.APIKey != "K" || s.id.UserAgent != "UA" {
		t.Errorf("identity = %+v, want the full capture", s.id)
	}
	if s.id.WatchURL != "https://youtube.com/watch?v=vid" {
		t.Errorf("watch_url = %q", s.id.WatchURL)
	}
}

// The shared page is restored before the next request reuses it: playback is
// stopped and the visibility override is reverted.
func TestRevertPlayerContextEvalsCleanup(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)

	// A canceled request context must still run the cleanup: revertPlayerContext
	// detaches so the page is never left driving playback.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.revertPlayerContext(ctx)

	if got := page.evalCount(playerContextCleanupJS); got != 1 {
		t.Errorf("cleanup ran %d times, want 1 even after the request was canceled", got)
	}
}

// PlayerContext hands back the metadata the page reported, with a nil thumbnail
// ladder normalised to an empty slice so the wire never shows null.
func TestSessionPlayerContextFromFakePage(t *testing.T) {
	payload := establishedPayload("vid", 635)
	page := newFakePageFor("vid", map[string][]fakeStep{
		playerContextExtractJS: {jsStringified(t, payload)},
		playerBufferedJS:       {jsStringified(t, bufferedPayload(103, 140, true))},
	})
	s := newFakeSession(page)
	// Skip the once-per-session proof: this test is about the metadata path.
	s.establishedStreaming = true

	pc, err := s.PlayerContext(context.Background(), "vid")
	if err != nil {
		t.Fatalf("PlayerContext: %v", err)
	}
	if pc.ChannelID != "UCabc" || pc.Description != "a description" || pc.PublishDate != "2015-04-10T00:00:00-07:00" {
		t.Errorf("metadata = %+v", pc)
	}
	if pc.IsLiveContent || pc.IsLiveNow || pc.IsUpcoming {
		t.Errorf("live flags = %v/%v/%v, want all false", pc.IsLiveContent, pc.IsLiveNow, pc.IsUpcoming)
	}
	if len(pc.Thumbnails) != 1 || pc.Thumbnails[0].Width != 120 || pc.Thumbnails[0].Height != 90 {
		t.Errorf("thumbnails = %+v", pc.Thumbnails)
	}

	t.Run("identity fills the user agent and an empty client version", func(t *testing.T) {
		bare := establishedPayload("vid", 635)
		bare["client_version"] = "" // a page that answered before ytcfg was readable
		page := newFakePageFor("vid", map[string][]fakeStep{
			playerContextExtractJS: {jsStringified(t, bare)},
			playerBufferedJS:       {jsStringified(t, bufferedPayload(103, 140, true))},
		})
		s := newFakeSession(page)
		s.establishedStreaming = true
		s.id = Identity{UserAgent: "Mozilla/5.0 (test)", ClientVersion: "2.captured", VisitorData: "VD"}

		pc, err := s.PlayerContext(context.Background(), "vid")
		if err != nil {
			t.Fatalf("PlayerContext: %v", err)
		}
		if pc.UserAgent != s.id.UserAgent {
			t.Errorf("user_agent = %q, want the session identity's %q", pc.UserAgent, s.id.UserAgent)
		}
		if pc.ClientVersion != "2.captured" {
			t.Errorf("client_version = %q, want the identity backfill", pc.ClientVersion)
		}
	})

	t.Run("a page-reported client version wins over the identity", func(t *testing.T) {
		page := newFakePageFor("vid", map[string][]fakeStep{
			playerContextExtractJS: {jsStringified(t, establishedPayload("vid", 635))},
			playerBufferedJS:       {jsStringified(t, bufferedPayload(103, 140, true))},
		})
		s := newFakeSession(page)
		s.establishedStreaming = true
		s.id = Identity{UserAgent: "Mozilla/5.0 (test)", ClientVersion: "2.captured"}

		pc, err := s.PlayerContext(context.Background(), "vid")
		if err != nil {
			t.Fatalf("PlayerContext: %v", err)
		}
		if pc.ClientVersion != "2.x" {
			t.Errorf("client_version = %q, want the page's own value", pc.ClientVersion)
		}
	})

	t.Run("nil ladder becomes an empty slice", func(t *testing.T) {
		bare := establishedPayload("vid", 635)
		delete(bare, "thumbnails")
		page := newFakePageFor("vid", map[string][]fakeStep{
			playerContextExtractJS: {jsStringified(t, bare)},
			playerBufferedJS:       {jsStringified(t, bufferedPayload(103, 140, true))},
		})
		s := newFakeSession(page)
		s.establishedStreaming = true

		pc, err := s.PlayerContext(context.Background(), "vid")
		if err != nil {
			t.Fatalf("PlayerContext: %v", err)
		}
		if pc.Thumbnails == nil {
			t.Fatal("thumbnails is nil; it would serialize as null")
		}
		if len(pc.Thumbnails) != 0 {
			t.Errorf("thumbnails = %+v, want empty", pc.Thumbnails)
		}
		encoded, err := json.Marshal(pc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(encoded), `"thumbnails":[]`) {
			t.Errorf("encoded context = %s, want thumbnails as []", encoded)
		}
	})
}

// A Session built without setupSession has a zero timing, and a zero poll
// interval makes time.After fire immediately, so a loop reading it would spin
// the CPU instead of pacing. Every loop reads through tuning, which fills the
// production values back in.
func TestTuningFillsUnsetTimings(t *testing.T) {
	zero := (&Session{}).tuning()
	want := defaultTiming()
	if zero != want {
		t.Errorf("a zero Session's tuning = %+v, want the production defaults %+v", zero, want)
	}

	// A partially set timing keeps what it was given and fills only the rest.
	partial := (&Session{timing: timing{poll: 7 * time.Millisecond}}).tuning()
	if partial.poll != 7*time.Millisecond {
		t.Errorf("tuning overwrote an explicit poll: %v", partial.poll)
	}
	if partial.establishTimeout != want.establishTimeout {
		t.Errorf("tuning left establishTimeout at %v, want the default %v", partial.establishTimeout, want.establishTimeout)
	}

	// fastTiming sets every field, so the offline tests really do run on their own
	// values rather than silently falling back to the production ones.
	if fast := fastTiming(); fast.withDefaults() != fast {
		t.Errorf("fastTiming leaves a field unset, so that loop would run at production speed: %+v", fast)
	}
}

// An empty description, ladder, or publish date is legal and must never be read
// as an incomplete context: only the fields a consumer cannot stream without are
// gated.
func TestValidatePlayerContextIgnoresMetadata(t *testing.T) {
	complete := playerContextRaw{PlayerContext: PlayerContext{
		ServerAbrStreamingURL:        "https://r/ok",
		PlayerURL:                    "https://p/base.js",
		VideoPlaybackUstreamerConfig: "cfg",
		VisitorData:                  "VD",
		AudioFormats:                 []AudioFormat{{Itag: 251, MimeType: "audio/webm"}},
	}}
	if err := validatePlayerContext(complete); err != nil {
		t.Fatalf("a context with no metadata was rejected: %v", err)
	}
}
