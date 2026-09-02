package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/cdp"
)

// Close must run dispose once even when called concurrently. Run this test under
// -race when changing Close.
func TestSessionCloseOnce(t *testing.T) {
	var calls atomic.Int64
	s := &Session{dispose: func() { calls.Add(1) }}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); s.Close() }()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("dispose called %d times, want exactly 1", got)
	}
	// A session that was never given a dispose must not panic.
	(&Session{}).Close()
}

func TestDetectChromeEnvOverride(t *testing.T) {
	t.Setenv("WAXSEAL_CHROME_BIN", "/custom/chromium")
	got, err := DetectChrome()
	if err != nil || got != "/custom/chromium" {
		t.Fatalf("DetectChrome() = %q, %v; want /custom/chromium", got, err)
	}
}

func TestWithDefaults(t *testing.T) {
	o := withDefaults(Options{})
	if o.Logger == nil {
		t.Error("Logger default is nil")
	}
	if o.NavTimeout <= 0 {
		t.Errorf("NavTimeout default = %v, want > 0", o.NavTimeout)
	}
	if got := withDefaults(Options{NavTimeout: 5 * time.Second}).NavTimeout; got != 5*time.Second {
		t.Errorf("explicit NavTimeout overwritten: %v", got)
	}
}

// LandingURL only makes sense together with StopAfterLoad: a LandingURL session
// that continued past the load event would have no identity to capture.
func TestValidateLaunchOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"landing url without stop after load", Options{LandingURL: "http://127.0.0.1:1/"}, true},
		{"landing url with stop after load", Options{LandingURL: "http://127.0.0.1:1/", StopAfterLoad: true}, false},
		{"stop after load alone", Options{StopAfterLoad: true}, false},
		{"neither set", Options{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLaunchOptions(tc.opts)
			if tc.wantErr && err == nil {
				t.Error("validateLaunchOptions = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateLaunchOptions = %v, want nil", err)
			}
		})
	}
}

// LaunchPool must reject the same invalid Options as Launch, and it must do so
// before it starts Chromium: an invalid LandingURL/StopAfterLoad combination
// returns the validation error straight away rather than a launch failure.
func TestLaunchPoolValidatesOptions(t *testing.T) {
	_, err := LaunchPool(Options{LandingURL: "http://127.0.0.1:1/"})
	if err == nil {
		t.Fatal("LaunchPool with LandingURL but no StopAfterLoad = nil error, want the validation error")
	}
	if !strings.Contains(err.Error(), "StopAfterLoad") {
		t.Errorf("LaunchPool error = %v, want it to name StopAfterLoad (the same error validateLaunchOptions returns)", err)
	}
}

// TestUAOverride pins normalizeUA's actual emitted Network.setUserAgentOverride
// payload. The CDP wire golden marshals a separate hand-written struct, so it
// cannot catch drift in this producer, such as Architecture changing to x86_64,
// brand version changes, or Bitness being omitted. This asserts the real
// producer's bytes.
func TestUAOverride(t *testing.T) {
	const realUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	const want = `{"userAgent":"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36","userAgentMetadata":{"brands":[{"brand":"Chromium","version":"149"},{"brand":"Not)A;Brand","version":"24"}],"fullVersionList":[{"brand":"Chromium","version":"149.0.0.0"},{"brand":"Not)A;Brand","version":"24.0.0.0"}],"fullVersion":"149.0.0.0","platform":"Linux","platformVersion":"","architecture":"x86","model":"","mobile":false,"bitness":"64"}}`

	got, err := json.Marshal(uaOverride(realUA))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("uaOverride payload drift:\n got: %s\nwant: %s", got, want)
	}
	for _, must := range []string{`"model":""`, `"platformVersion":""`} {
		if !strings.Contains(string(got), must) {
			t.Errorf("payload missing %s (omitempty regression)", must)
		}
	}

	// HeadlessChrome is rewritten and the major comes from the real UA, not the
	// hardcoded fallback.
	hl := uaOverride("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/151.0.0.0 Safari/537.36")
	if strings.Contains(hl.UserAgent, "HeadlessChrome") {
		t.Errorf("HeadlessChrome marker not removed: %q", hl.UserAgent)
	}
	if hl.UserAgentMetadata.Brands[0].Version != "151" {
		t.Errorf("major = %q, want 151 (derived from the real UA)", hl.UserAgentMetadata.Brands[0].Version)
	}
}

func TestDefaultVideoSet(t *testing.T) {
	if DefaultVideo == "" {
		t.Error("DefaultVideo must be a non-empty (non-copyrighted) video id")
	}
}

// TestAudioFormatTagDrift keeps the extracted JSON fields in sync with AudioFormat.
func TestAudioFormatTagDrift(t *testing.T) {
	const payload = `{"itag":251,"lmt":"171","is_drc":true,"audio_track_id":"en.4"}`
	var f AudioFormat
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !f.IsDrc {
		t.Error("is_drc did not decode into IsDrc")
	}
	if f.AudioTrackID != "en.4" {
		t.Errorf("audio_track_id = %q, want en.4", f.AudioTrackID)
	}
}

// TestConfirmTerminal covers stale evidence that must not mark the current video
// unavailable.
func TestConfirmTerminal(t *testing.T) {
	const want = "vid123"
	raw := func(mut func(*playerContextRaw)) playerContextRaw {
		r := playerContextRaw{Error: "pending"}
		mut(&r)
		return r
	}
	tests := []struct {
		name         string
		raw          playerContextRaw
		wantTerminal bool
		wantStatus   string
	}{
		{"gen-matched onError 100, id match", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = want }), true, "ERROR"},
		{"gen-matched onError 150, id match", raw(func(r *playerContextRaw) { r.ErrCode = 150; r.ErrGenMatch = true; r.ErrVideoID = want }), true, "ERROR"},
		{"gen-matched onError 100, stale video id", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = "othervid" }), false, ""},
		{"gen-matched onError 100, empty video id", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = "" }), false, ""},
		{"non-OK status + id match", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "LOGIN_REQUIRED"; r.VideoIDMatch = true }), true, "LOGIN_REQUIRED"},
		{"non-OK status for another video", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "ERROR"; r.VideoIDMatch = false }), false, ""},
		{"onError 100 with gen mismatch", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = false; r.ErrVideoID = want }), false, ""},
		{"onError 5 (non-terminal code)", raw(func(r *playerContextRaw) { r.ErrCode = 5; r.ErrGenMatch = true; r.ErrVideoID = want }), false, ""},
		{"status OK + id match", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "OK"; r.VideoIDMatch = true }), false, ""},
		{"no evidence", raw(func(r *playerContextRaw) {}), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ue, ok := confirmTerminal(tt.raw, want)
			if ok != tt.wantTerminal {
				t.Fatalf("terminal = %v, want %v", ok, tt.wantTerminal)
			}
			if !ok {
				if ue != nil {
					t.Errorf("non-terminal returned a non-nil error: %v", ue)
				}
				return
			}
			if ue.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", ue.Status, tt.wantStatus)
			}
		})
	}
}

func TestIsUnavailableCode(t *testing.T) {
	for _, c := range []int{2, 100, 101, 150} {
		if !isUnavailableCode(c) {
			t.Errorf("isUnavailableCode(%d) = false, want true", c)
		}
	}
	for _, c := range []int{0, 5, 3, 104, 999} {
		if isUnavailableCode(c) {
			t.Errorf("isUnavailableCode(%d) = true, want false", c)
		}
	}
}

func TestFullLengthProbeModel(t *testing.T) {
	outcomes := map[string]bool{
		OutcomeFullLength:         true,
		OutcomeTargetNotBuffered:  true,
		OutcomeNotEstablished:     true,
		OutcomeVideoTooShort:      true,
		OutcomeCanceled:           true,
		OutcomeConfirmUnavailable: true,
	}
	if len(outcomes) != 6 {
		t.Fatalf("outcome constants are not all distinct: %v", outcomes)
	}
	if OutcomeFullLength != "full-length" {
		t.Errorf("OutcomeFullLength = %q, want full-length", OutcomeFullLength)
	}
}

// TestConfirmBudgets keeps the re-read budget separate and smaller than the
// confirm budget. That guards against zero-time re-reads without letting one
// request run too long.
func TestConfirmBudgets(t *testing.T) {
	if playerContextReReadBudget <= 0 {
		t.Errorf("playerContextReReadBudget = %v, want > 0", playerContextReReadBudget)
	}
	if playerContextReReadBudget >= playerContextConfirmBudget {
		t.Errorf("re-read budget %v should be smaller than confirm budget %v", playerContextReReadBudget, playerContextConfirmBudget)
	}
}

// TestSeekTarget covers the length-aware confirmation target. Unknown or invalid
// lengths use the full-length target; known lengths clamp the target so the
// tolerance window stays within the video.
func TestSeekTarget(t *testing.T) {
	for _, tt := range []struct {
		length int
		want   int
	}{
		{0, fullLengthTargetSecs},   // unknown length uses the full-length target
		{-5, fullLengthTargetSecs},  // guard: a bogus negative length is treated as unknown
		{1, 1 - verifyEndTol},       // tiny videos fall well below the cap (caller treats as cap-safe)
		{70, 70 - verifyEndTol},     // at the cap
		{73, 73 - verifyEndTol},     // top of the residual band is 70
		{74, 74 - verifyEndTol},     // first verifiable length is 71, past the cap
		{102, 99},                   // just under: clamped down by verifyEndTol
		{103, fullLengthTargetSecs}, // length-verifyEndTol == target, clamps to target
		{200, fullLengthTargetSecs}, // long videos use the full-length target
	} {
		if got := seekTarget(tt.length); got != tt.want {
			t.Errorf("seekTarget(%d) = %d, want %d", tt.length, got, tt.want)
		}
	}
	// The unknown-length target must never produce a negative seek.
	if got := seekTarget(0); got <= 0 {
		t.Errorf("seekTarget(0) = %d, want a positive target", got)
	}
}

// TestClassifyBand pins the confirmation-band boundaries: cap-safe at or below the
// cap, residual just above it, verify beyond the residual band, and verify for an
// unknown length.
func TestClassifyBand(t *testing.T) {
	for _, tt := range []struct {
		length int
		want   confirmBand
	}{
		{0, bandVerify},  // unknown length verifies at the full-length target
		{1, bandCapSafe}, // tiny
		{previewCapSecs - 1, bandCapSafe},
		{previewCapSecs, bandCapSafe},                   // at the cap is still cap-safe
		{previewCapSecs + 1, bandResidual},              // 71: first over-cap length
		{previewCapSecs + verifyEndTol, bandResidual},   // 73: top of the residual band
		{previewCapSecs + verifyEndTol + 1, bandVerify}, // 74: first verifiable length
		{120, bandVerify},
	} {
		if got := classifyBand(tt.length); got != tt.want {
			t.Errorf("classifyBand(%d) = %d, want %d", tt.length, got, tt.want)
		}
	}
}

// TestBufferedReachesEnd pins the residual-band acceptance boundary. A buffer
// that stops at the preview cap must be refused for every residual length; only a
// buffer near the real end passes.
func TestBufferedReachesEnd(t *testing.T) {
	const cap2 = float64(previewCapSecs) // status-2 streams commonly stop here
	for _, tt := range []struct {
		length      int
		bufferedEnd float64
		want        bool
	}{
		{71, cap2, false}, // status-2 cap: refused for a 71s video
		{71, 70.4, false}, // still short of the 70.5 threshold
		{71, 71.0, true},  // status-1 reaches the true end
		{72, cap2, false}, // status-2 cap: refused
		{72, 71.4, false}, // just short of the 71.5 threshold
		{72, 72.0, true},  // status-1
		{73, cap2, false}, // status-2 cap: refused
		{73, 72.5, true},  // exactly at the 72.5 threshold
		{73, 73.0, true},  // status-1
	} {
		if got := bufferedReachesEnd(tt.length, tt.bufferedEnd); got != tt.want {
			t.Errorf("bufferedReachesEnd(%d, %.1f) = %v, want %v", tt.length, tt.bufferedEnd, got, tt.want)
		}
	}
}

// TestReduceStreamingURL pins reduceStreamingURL's output for the cases the
// diagnostic log lines depend on: a realistic googlevideo SABR URL, one missing
// some of the kept parameters, and input that net/url cannot parse at all. Every
// case's want string is checked to contain none of the signed fields (sig, lsig,
// pot, n) present in the realistic input, so a regression that widens the kept
// parameter set fails here instead of in a live log line.
func TestReduceStreamingURL(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "realistic googlevideo URL",
			in: "https://rr3---sn-4g5ednsz.googlevideo.com/videoplayback?expire=1750000000&ei=abcDEF123" +
				"&ip=203.0.113.5&id=o-AbCdEfGhIjKlMnOpQrStUv&itag=140&source=youtube&requiressl=yes" +
				"&spc=abcXYZ123_-Q&vprv=1&mime=audio%2Fmp4&clen=12345678&dur=634.624&mt=1750000000" +
				"&fvip=3&c=WEB&n=someNSigValue&sig=abcdef1234567890fedcba&lsig=xyz9876543210abc" +
				"&pot=aBogusPlayerOrGVSToken%3D%3D",
			want: "rr3---sn-4g5ednsz.googlevideo.com/videoplayback?id=o-AbCdEfGhIjKlMnOpQrStUv&expire=1750000000&spc=abcXYZ123_-Q",
		},
		{
			name: "missing some of the kept params",
			in:   "https://r5---sn-abcxyz.googlevideo.com/videoplayback?expire=1750000001&id=o-ShortIdHere&itag=251&mime=audio%2Fwebm",
			want: "r5---sn-abcxyz.googlevideo.com/videoplayback?id=o-ShortIdHere&expire=1750000001",
		},
		{
			name: "junk that fails to parse",
			in:   "%",
			want: "<unparseable>",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := reduceStreamingURL(tt.in)
			if got != tt.want {
				t.Errorf("reduceStreamingURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			for _, secret := range []string{"sig=", "lsig=", "pot=", "n=someNSigValue"} {
				if strings.Contains(got, secret) {
					t.Errorf("reduceStreamingURL(%q) = %q, must not contain %q", tt.in, got, secret)
				}
			}
		})
	}
}

// TestConfirmError pins the recovery class for each confirm result: success is
// nil, a confirm that could not start is relaunchable session trouble, and a
// confirm that ran but did not clear the cap is ErrStatus2Unconfirmed.
func TestConfirmError(t *testing.T) {
	if err := confirmError(FullLengthProbe{FullLength: true, Outcome: OutcomeFullLength}); err != nil {
		t.Errorf("full-length confirm error = %v, want nil", err)
	}
	wedged := confirmError(FullLengthProbe{Outcome: OutcomeConfirmUnavailable, Reason: "movie_player.seekTo unavailable"})
	if wedged == nil {
		t.Fatal("wedged-page confirm error = nil, want a relaunch-eligible error")
	}
	if errors.Is(wedged, ErrStatus2Unconfirmed) {
		t.Errorf("wedged-page error must not be ErrStatus2Unconfirmed (it would never relaunch): %v", wedged)
	}
	if errors.Is(wedged, ErrUnplayable) {
		t.Errorf("wedged-page error must not be ErrUnplayable: %v", wedged)
	}
	capped := confirmError(FullLengthProbe{Outcome: OutcomeTargetNotBuffered, Reason: "budget expired"})
	if !errors.Is(capped, ErrStatus2Unconfirmed) {
		t.Errorf("status-2 confirm error = %v, want ErrStatus2Unconfirmed", capped)
	}
}

// TestErrStatus2Unconfirmed verifies that the status-2 sentinel wraps cleanly and
// stays separate from ErrUnplayable. The minter retries the former in place and
// negative-caches the latter.
func TestErrStatus2Unconfirmed(t *testing.T) {
	wrapped := fmt.Errorf("%w: budget expired", ErrStatus2Unconfirmed)
	if !errors.Is(wrapped, ErrStatus2Unconfirmed) {
		t.Error("wrapped status-2 error does not match ErrStatus2Unconfirmed")
	}
	if errors.Is(ErrStatus2Unconfirmed, ErrUnplayable) || errors.Is(wrapped, ErrUnplayable) {
		t.Error("status-2 error must not be classified as ErrUnplayable")
	}
}

// TestBufferedSampleDecode guards against drift between playerBufferedJS's output
// keys and the bufferedSample struct confirmPastCap decodes them into.
func TestBufferedSampleDecode(t *testing.T) {
	const payload = `{"current":72.5,"buffered_end":101.2,"covers_target":true,"state":1,"player_error":0,"abr_url":"https://r/x"}`
	var b bufferedSample
	if err := json.Unmarshal([]byte(payload), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.Current != 72.5 || b.BufferedEnd != 101.2 || !b.CoversTarget || b.State != 1 || b.ABRURL != "https://r/x" {
		t.Errorf("decoded sample = %+v, want the payload values", b)
	}
}

func TestEstablishFromCandidates(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	full := FullLengthProbe{Outcome: OutcomeFullLength, FullLength: true}
	tooShort := FullLengthProbe{Outcome: OutcomeVideoTooShort, Reason: "too short"}
	capped := FullLengthProbe{Outcome: OutcomeTargetNotBuffered, Reason: "status-2 cap"}
	noEstablish := FullLengthProbe{Outcome: OutcomeNotEstablished, Reason: "no context"}

	type res struct {
		probe FullLengthProbe
		err   error
	}
	// A real proveFullLength reports an unplayable video as OutcomeNotEstablished
	// with a non-nil ErrUnplayable; the helper keys off the error, not the outcome.
	unplayable := res{FullLengthProbe{Outcome: OutcomeNotEstablished}, &UnplayableError{Status: "ERROR"}}

	tests := []struct {
		name        string
		candidates  []string
		results     map[string]res
		wantErr     bool
		wantErrText []string
		errIs       error
		errIsNot    error
		wantCalls   []string
	}{
		{
			name:       "dead first video falls through to a healthy candidate",
			candidates: []string{"dead", "good"},
			results:    map[string]res{"dead": unplayable, "good": {full, nil}},
			wantCalls:  []string{"dead", "good"},
		},
		{
			name:       "too-short advances to the next candidate",
			candidates: []string{"short", "good"},
			results:    map[string]res{"short": {tooShort, nil}, "good": {full, nil}},
			wantCalls:  []string{"short", "good"},
		},
		{
			name:        "target-not-buffered stops fallback",
			candidates:  []string{"capped", "good"},
			results:     map[string]res{"capped": {capped, nil}, "good": {full, nil}},
			wantErr:     true,
			wantErrText: []string{OutcomeTargetNotBuffered},
			wantCalls:   []string{"capped"},
		},
		{
			name:        "not-established stops fallback",
			candidates:  []string{"noctx", "good"},
			results:     map[string]res{"noctx": {noEstablish, nil}, "good": {full, nil}},
			wantErr:     true,
			wantErrText: []string{OutcomeNotEstablished},
			wantCalls:   []string{"noctx"},
		},
		{
			name:       "context cancellation propagates without further candidates",
			candidates: []string{"cancel", "good"},
			results:    map[string]res{"cancel": {FullLengthProbe{Outcome: OutcomeCanceled}, context.Canceled}, "good": {full, nil}},
			wantErr:    true,
			errIs:      context.Canceled,
			wantCalls:  []string{"cancel"},
		},
		{
			name:        "all unusable candidates return an aggregate error",
			candidates:  []string{"dead", "short"},
			results:     map[string]res{"dead": unplayable, "short": {tooShort, nil}},
			wantErr:     true,
			wantErrText: []string{"no usable proof video", "dead", "short"},
			wantCalls:   []string{"dead", "short"},
		},
		{
			// Do not let failures from internal proof videos mark the caller's video
			// as unavailable.
			name:        "exhausted candidates do not expose ErrUnplayable",
			candidates:  []string{"dead1", "dead2"},
			results:     map[string]res{"dead1": unplayable, "dead2": unplayable},
			wantErr:     true,
			wantErrText: []string{"no usable proof video", "dead1", "dead2"},
			errIsNot:    ErrUnplayable,
			wantCalls:   []string{"dead1", "dead2"},
		},
		{
			name:       "duplicate and empty candidates are skipped",
			candidates: []string{"good", "", "good"},
			results:    map[string]res{"good": {full, nil}},
			wantCalls:  []string{"good"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			prove := func(v string) (FullLengthProbe, error) {
				calls = append(calls, v)
				r, ok := tt.results[v]
				if !ok {
					t.Fatalf("prove called with unexpected video %q", v)
				}
				return r.probe, r.err
			}
			err := establishFromCandidates(context.Background(), prove, tt.candidates, log)
			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("err = nil, want an error")
			case !tt.wantErr && err != nil:
				t.Fatalf("err = %v, want nil (established)", err)
			}
			for _, text := range tt.wantErrText {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("err = %q, want it to contain %q", err.Error(), text)
				}
			}
			if tt.errIs != nil && !errors.Is(err, tt.errIs) {
				t.Errorf("err = %v, want errors.Is %v", err, tt.errIs)
			}
			if tt.errIsNot != nil && errors.Is(err, tt.errIsNot) {
				t.Errorf("err = %v, unexpectedly matches errors.Is %v", err, tt.errIsNot)
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

// The cookie filter accepts youtube.com and real subdomains after case and
// leading-dot normalization, and rejects look-alikes a substring match would allow.
func TestIsYouTubeCookieDomain(t *testing.T) {
	for _, tt := range []struct {
		domain string
		want   bool
	}{
		{"youtube.com", true},
		{".youtube.com", true},
		{".YouTube.com", true},
		{"www.youtube.com", true},
		{"music.youtube.com", true},
		{"youtube.com.evil.com", false},
		{"notyoutube.com", false},
		{"evil.com", false},
		{"", false},
	} {
		if got := isYouTubeCookieDomain(tt.domain); got != tt.want {
			t.Errorf("isYouTubeCookieDomain(%q) = %v, want %v", tt.domain, got, tt.want)
		}
	}
}

// validatePlayerContext accepts complete contexts and marks missing required
// fields as ErrIncompleteContext, so the minter retries without relaunching or
// caching a permanent unplayable result.
func TestValidatePlayerContext(t *testing.T) {
	full := PlayerContext{
		ServerAbrStreamingURL:        "https://r/abr",
		PlayerURL:                    "https://r/base.js",
		VideoPlaybackUstreamerConfig: "cfg",
		VisitorData:                  "vd",
		AudioFormats:                 []AudioFormat{{Itag: 140}},
	}
	if err := validatePlayerContext(playerContextRaw{PlayerContext: full}); err != nil {
		t.Errorf("complete context: unexpected error %v", err)
	}

	for name, mut := range map[string]func(*PlayerContext){
		"no abr url":        func(p *PlayerContext) { p.ServerAbrStreamingURL = "" },
		"no player url":     func(p *PlayerContext) { p.PlayerURL = "" },
		"no ustreamer cfg":  func(p *PlayerContext) { p.VideoPlaybackUstreamerConfig = "" },
		"no visitor data":   func(p *PlayerContext) { p.VisitorData = "" },
		"no audio formats":  func(p *PlayerContext) { p.AudioFormats = nil },
		"empty audio slice": func(p *PlayerContext) { p.AudioFormats = []AudioFormat{} },
	} {
		t.Run(name, func(t *testing.T) {
			pc := full
			mut(&pc)
			err := validatePlayerContext(playerContextRaw{PlayerContext: pc})
			if err == nil {
				t.Fatalf("want an error for %q", name)
			}
			if !errors.Is(err, ErrIncompleteContext) {
				t.Errorf("error must wrap ErrIncompleteContext (so the minter retries without relaunching): %v", err)
			}
			if errors.Is(err, ErrUnplayable) {
				t.Errorf("error must not be ErrUnplayable (so it is not negative-cached): %v", err)
			}
		})
	}
}

// TestUsableAudioFormats keeps only selectable formats: a positive itag and an
// audio/* MIME type. If every entry is filtered out, validatePlayerContext should
// reject the empty list.
func TestUsableAudioFormats(t *testing.T) {
	in := []AudioFormat{
		{Itag: 140, MimeType: "audio/mp4"},  // keep
		{Itag: 251, MimeType: "audio/webm"}, // keep
		{Itag: 0, MimeType: "audio/webm"},   // drop: itag <= 0
		{Itag: -1, MimeType: "audio/mp4"},   // drop: itag <= 0
		{Itag: 137, MimeType: "video/mp4"},  // drop: not audio/*
		{Itag: 141, MimeType: ""},           // drop: not audio/*
	}
	got := usableAudioFormats(in)
	if len(got) != 2 {
		t.Fatalf("kept %d formats, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Itag <= 0 || !strings.HasPrefix(f.MimeType, "audio/") {
			t.Errorf("kept an unusable format: %+v", f)
		}
	}

	allBad := usableAudioFormats([]AudioFormat{{Itag: 0, MimeType: "video/mp4"}})
	if len(allBad) != 0 {
		t.Errorf("all-bad list kept %d, want 0", len(allBad))
	}
	err := validatePlayerContext(playerContextRaw{PlayerContext: PlayerContext{
		ServerAbrStreamingURL: "u", PlayerURL: "p", VideoPlaybackUstreamerConfig: "c", VisitorData: "vd", AudioFormats: allBad,
	}})
	if !errors.Is(err, ErrIncompleteContext) {
		t.Errorf("all-filtered context error = %v, want ErrIncompleteContext", err)
	}
}

// TestHTTPCookieFromCDP maps CDP cookies to *http.Cookie values. Session cookies
// keep a zero Expires value, persistent cookies convert from Unix seconds, flags
// carry through, and sameSite maps to the net/http enum.
func TestHTTPCookieFromCDP(t *testing.T) {
	sessionCk := httpCookieFromCDP(&cdp.Cookie{Name: "YSC", Value: "s", Domain: ".youtube.com", Path: "/", Expires: -1, Session: true})
	if !sessionCk.Expires.IsZero() {
		t.Errorf("session cookie Expires = %v, want zero", sessionCk.Expires)
	}

	expiring := httpCookieFromCDP(&cdp.Cookie{Name: "PREF", Value: "p", Expires: 1750000000, Session: false, Secure: true, HTTPOnly: true})
	if want := time.Unix(1750000000, 0).UTC(); !expiring.Expires.Equal(want) {
		t.Errorf("Expires = %v, want %v", expiring.Expires, want)
	}
	if !expiring.Secure || !expiring.HttpOnly {
		t.Errorf("flags not carried: %+v", expiring)
	}

	for in, want := range map[string]http.SameSite{
		"Strict": http.SameSiteStrictMode,
		"Lax":    http.SameSiteLaxMode,
		"None":   http.SameSiteNoneMode,
		"":       0,
		"weird":  0,
	} {
		if got := httpCookieFromCDP(&cdp.Cookie{SameSite: in}).SameSite; got != want {
			t.Errorf("SameSite(%q) = %v, want %v", in, got, want)
		}
	}
}
