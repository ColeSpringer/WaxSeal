package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
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
	// The default is read from the environment, so clear the kill switch first:
	// otherwise this reads whatever WAXSEAL_UA_HINTS the developer has exported.
	t.Setenv(uaHintsEnv, "")
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

	// Client hints default to the browser's own, and the kill switch is honoured
	// from either the field or the environment. An unrecognised env value keeps
	// the default rather than disabling the override.
	if o.UAHints != UAHintsReal {
		t.Errorf("UAHints default = %q, want %q", o.UAHints, UAHintsReal)
	}
	if got := withDefaults(Options{UAHints: UAHintsSynthetic}).UAHints; got != UAHintsSynthetic {
		t.Errorf("explicit UAHints overwritten: %q", got)
	}
	for _, tc := range []struct{ env, want string }{
		{UAHintsSynthetic, UAHintsSynthetic},
		{"  " + UAHintsSynthetic + "  ", UAHintsSynthetic}, // trimmed
		{UAHintsReal, UAHintsReal},
		{"banana", UAHintsReal},
		{"", UAHintsReal},
	} {
		t.Setenv(uaHintsEnv, tc.env)
		if got := withDefaults(Options{}).UAHints; got != tc.want {
			t.Errorf("%s=%q gives UAHints %q, want %q", uaHintsEnv, tc.env, got, tc.want)
		}
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
		{"ua hints real", Options{UAHints: UAHintsReal}, false},
		{"ua hints synthetic", Options{UAHints: UAHintsSynthetic}, false},
		{"ua hints typo", Options{UAHints: "reall"}, true},
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
	_, err := LaunchPool(context.Background(), Options{LandingURL: "http://127.0.0.1:1/"})
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
	const want = `{"userAgent":"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36","acceptLanguage":"en-US,en","userAgentMetadata":{"brands":[{"brand":"Chromium","version":"149"},{"brand":"Not)A;Brand","version":"24"}],"fullVersionList":[{"brand":"Chromium","version":"149.0.0.0"},{"brand":"Not)A;Brand","version":"24.0.0.0"}],"fullVersion":"149.0.0.0","platform":"Linux","platformVersion":"","architecture":"x86","model":"","mobile":false,"bitness":"64"}}`

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

// TestUAOverrideFromMetadata covers the real-hint path: everything the browser
// reports is passed through untouched, and the only edit is the headless marker.
// The inputs are shaped like real getHighEntropyValues payloads, including the
// randomised GREASE brand and the four-part build version, neither of which can
// be derived from the reduced UA string.
func TestUAOverrideFromMetadata(t *testing.T) {
	decode := func(t *testing.T, raw string) *uaMetadata {
		t.Helper()
		var m uaMetadata
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("unmarshal capture: %v", err)
		}
		return &m
	}

	t.Run("linux headless keeps the real brands", func(t *testing.T) {
		// A headless build on Linux/x86_64. This capture carries a HeadlessChrome
		// brand as well as the marker in the UA string, so both substitutions run.
		m := decode(t, `{"ua":"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/152.0.0.0 Safari/537.36",
			"brands":[{"brand":"Chromium","version":"152"},{"brand":"Not?A_Brand","version":"24"},{"brand":"HeadlessChrome","version":"152"},{"brand":"Google Chrome","version":"152"}],
			"mobile":false,"platform":"Linux",
			"hints":{"architecture":"x86","bitness":"64","model":"","platformVersion":"6.18.0","uaFullVersion":"152.0.7977.75","wow64":false,
				"fullVersionList":[{"brand":"Chromium","version":"152.0.7977.75"},{"brand":"Not?A_Brand","version":"24.0.0.0"},{"brand":"HeadlessChrome","version":"152.0.7977.75"},{"brand":"Google Chrome","version":"152.0.7977.75"}]}}`)
		got := uaOverrideFromMetadata(m)
		if got == nil {
			t.Fatal("uaOverrideFromMetadata = nil, want an override")
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "HeadlessChrome") {
			t.Errorf("headless marker survived anywhere in the payload: %s", raw)
		}
		if got.UserAgent != "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36" {
			t.Errorf("userAgent = %q", got.UserAgent)
		}
		md := got.UserAgentMetadata
		// The brand the old synthesized block dropped, the GREASE value it replaced
		// with a constant, and the build version it coarsened to x.0.0.0.
		if !hasBrand(md.Brands, "Google Chrome", "152") {
			t.Errorf("brands lost the Google Chrome entry: %+v", brandPairs(md.Brands))
		}
		if !hasBrand(md.Brands, "Not?A_Brand", "24") {
			t.Errorf("brands lost the real GREASE entry: %+v", brandPairs(md.Brands))
		}
		if !hasBrand(md.FullVersionList, "Google Chrome", "152.0.7977.75") {
			t.Errorf("fullVersionList lost the four-part build version: %+v", brandPairs(md.FullVersionList))
		}
		if md.FullVersion != "152.0.7977.75" {
			t.Errorf("fullVersion = %q, want the four-part build version", md.FullVersion)
		}
		// The headless brand is renamed, not dropped, so the list length holds.
		if len(md.Brands) != 4 || len(md.FullVersionList) != 4 {
			t.Errorf("brand list lengths = %d/%d, want 4/4", len(md.Brands), len(md.FullVersionList))
		}
		if md.PlatformVersion != "6.18.0" {
			t.Errorf("platformVersion = %q, want the reported 6.18.0", md.PlatformVersion)
		}
		// The capture reports no language, so the real-hint path has to carry the
		// pin itself or an overridden page falls back to the host's locale.
		if got.AcceptLanguage != acceptLanguage {
			t.Errorf("acceptLanguage = %q, want %q", got.AcceptLanguage, acceptLanguage)
		}
	})

	t.Run("macos arm64 follows the capture", func(t *testing.T) {
		// The case the synthesized block gets wrong: it pins Linux/x86/64 on every
		// platform, so a darwin arm64 build emitted Linux hints under a Macintosh UA.
		m := decode(t, `{"ua":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
			"brands":[{"brand":"Chromium","version":"152"},{"brand":"Not?A_Brand","version":"24"}],
			"mobile":false,"platform":"macOS",
			"hints":{"architecture":"arm","bitness":"64","model":"","platformVersion":"15.3.1","uaFullVersion":"152.0.7977.75","wow64":false,
				"fullVersionList":[{"brand":"Chromium","version":"152.0.7977.75"},{"brand":"Not?A_Brand","version":"24.0.0.0"}]}}`)
		md := uaOverrideFromMetadata(m).UserAgentMetadata
		for _, tc := range []struct{ name, got, want string }{
			{"platform", md.Platform, "macOS"},
			{"architecture", md.Architecture, "arm"},
			{"bitness", md.Bitness, "64"},
			{"platformVersion", md.PlatformVersion, "15.3.1"},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q (it must follow the capture, not the Linux constants)", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("an unusable capture falls back", func(t *testing.T) {
		for name, raw := range map[string]string{
			"empty object":    `{}`,
			"no ua":           `{"brands":[{"brand":"Chromium","version":"152"}],"platform":"Linux"}`,
			"no brands":       `{"ua":"Mozilla/5.0 Chrome/152.0.0.0","platform":"Linux"}`,
			"no platform":     `{"ua":"Mozilla/5.0 Chrome/152.0.0.0","brands":[{"brand":"Chromium","version":"152"}]}`,
			"brands are null": `{"ua":"Mozilla/5.0 Chrome/152.0.0.0","brands":null,"platform":"Linux"}`,
			// The high-entropy fields are omitempty on the wire, so a capture without
			// them must not become an override that advertises no full-version list.
			"no fullVersionList": `{"ua":"Mozilla/5.0 Chrome/152.0.0.0","brands":[{"brand":"Chromium","version":"152"}],"platform":"Linux","hints":{"uaFullVersion":"152.0.7977.75"}}`,
			"no uaFullVersion":   `{"ua":"Mozilla/5.0 Chrome/152.0.0.0","brands":[{"brand":"Chromium","version":"152"}],"platform":"Linux","hints":{"fullVersionList":[{"brand":"Chromium","version":"152.0.7977.75"}]}}`,
		} {
			if got := uaOverrideFromMetadata(decode(t, raw)); got != nil {
				t.Errorf("%s: uaOverrideFromMetadata = %+v, want nil so normalizeUA falls back", name, got)
			}
		}
		if got := uaOverrideFromMetadata(nil); got != nil {
			t.Errorf("nil capture: uaOverrideFromMetadata = %+v, want nil", got)
		}
		// The fallback itself still has to produce a coherent block.
		fb := uaOverride("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/152.0.0.0 Safari/537.36")
		if fb.UserAgentMetadata == nil || len(fb.UserAgentMetadata.Brands) == 0 || fb.UserAgentMetadata.Platform == "" {
			t.Errorf("synthesized fallback is not coherent: %+v", fb)
		}
		if strings.Contains(fb.UserAgent, "HeadlessChrome") {
			t.Errorf("synthesized fallback kept the marker: %q", fb.UserAgent)
		}
	})
}

// hasBrand reports whether brands carries the given brand at the given version.
func hasBrand(brands []*cdp.UserAgentBrandVersion, brand, version string) bool {
	return slices.ContainsFunc(brands, func(b *cdp.UserAgentBrandVersion) bool {
		return b.Brand == brand && b.Version == version
	})
}

// brandPairs renders a brand list for a failure message; the slice holds pointers,
// which print as addresses.
func brandPairs(brands []*cdp.UserAgentBrandVersion) []string {
	out := make([]string, 0, len(brands))
	for _, b := range brands {
		out = append(out, b.Brand+"/"+b.Version)
	}
	return out
}

// TestUAOverrideCacheKeepsOnlyAUsableCapture pins the memoisation rule: the first
// usable capture is kept for the browser's lifetime and later sessions never
// capture again, while a failed or incomplete capture leaves the cache empty so
// the next session retries instead of inheriting the fallback.
func TestUAOverrideCacheKeepsOnlyAUsableCapture(t *testing.T) {
	var c uaOverrideCache
	calls := 0
	if got := c.realOverride(func() *cdp.NetworkSetUserAgentOverride { calls++; return nil }); got != nil {
		t.Fatalf("a failed capture returned %+v, want nil", got)
	}
	want := &cdp.NetworkSetUserAgentOverride{UserAgent: "Mozilla/5.0 Chrome/152.0.0.0"}
	if got := c.realOverride(func() *cdp.NetworkSetUserAgentOverride { calls++; return want }); got != want {
		t.Fatalf("second capture returned %+v, want the override it produced", got)
	}
	if got := c.realOverride(func() *cdp.NetworkSetUserAgentOverride {
		t.Error("capture ran again after a usable one was cached")
		return nil
	}); got != want {
		t.Errorf("cached call returned %+v, want the memoised override", got)
	}
	if calls != 2 {
		t.Errorf("capture ran %d times, want 2 (a failure, then the one that was kept)", calls)
	}
}

func TestDefaultVideoSet(t *testing.T) {
	if DefaultVideo == "" {
		t.Error("DefaultVideo must be a non-empty (non-copyrighted) video id")
	}
}

// TestAudioFormatTagDrift keeps the extracted JSON fields in sync with AudioFormat.
func TestAudioFormatTagDrift(t *testing.T) {
	const payload = `{"itag":251,"lmt":"171","is_drc":true,"audio_track_id":"en.4","audio_is_default":false}`
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
	if f.AudioIsDefault == nil || *f.AudioIsDefault {
		t.Error("audio_is_default did not decode into AudioIsDefault as a stated false")
	}
	// The snippet leaves the key out when the player states nothing, and that
	// has to decode as unstated, not as false.
	var bare AudioFormat
	if err := json.Unmarshal([]byte(`{"itag":251}`), &bare); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if bare.AudioIsDefault != nil {
		t.Errorf("audio_is_default = %v without the key, want nil", *bare.AudioIsDefault)
	}
}

// TestPlayerContextTagDrift keeps the metadata keys emitted by
// playerContextExtractJS in sync with PlayerContext, the same way
// TestAudioFormatTagDrift covers the format list. The payload is the JS shape,
// not the Go field names.
func TestPlayerContextTagDrift(t *testing.T) {
	const payload = `{"channel_id":"UCabc","description":"desc","thumbnails":[{"url":"https://t/1.jpg","width":120,"height":90}],` +
		`"is_live_content":true,"is_live_now":true,"is_upcoming":true,"publish_date":"2015-04-10"}`
	var pc PlayerContext
	if err := json.Unmarshal([]byte(payload), &pc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pc.ChannelID != "UCabc" || pc.Description != "desc" || pc.PublishDate != "2015-04-10" {
		t.Errorf("metadata = %+v", pc)
	}
	if !pc.IsLiveContent || !pc.IsLiveNow || !pc.IsUpcoming {
		t.Errorf("live flags = %v/%v/%v, want all true", pc.IsLiveContent, pc.IsLiveNow, pc.IsUpcoming)
	}
	if len(pc.Thumbnails) != 1 || pc.Thumbnails[0].URL != "https://t/1.jpg" || pc.Thumbnails[0].Width != 120 || pc.Thumbnails[0].Height != 90 {
		t.Errorf("thumbnails = %+v", pc.Thumbnails)
	}
}

// sessionFilledContextKeys are the PlayerContext keys Session.PlayerContext
// fills from the captured identity rather than the page, so the extraction
// snippet is not expected to emit them.
var sessionFilledContextKeys = map[string]string{
	"user_agent": "the identity holds the post-override navigator.userAgent already",
}

// TestPlayerContextExtractJSEmitsEveryKey pins that the extraction snippet names
// every documented key in the object literal it returns on success, bar the ones
// the session fills itself, and every grading field in the evidence block it
// attaches to a pending or unplayable payload. The JS runs only in Chromium, so a
// key dropped from either literal would otherwise surface as an empty field in a
// live run, and a missing evidence key reads as a zero value that confirmTerminal
// grades on. Only those literals are searched, and only for a whole property
// name, so a key that survives elsewhere in the snippet, or as the tail of
// another key, cannot satisfy the check.
func TestPlayerContextExtractJSEmitsEveryKey(t *testing.T) {
	const open = "return JSON.stringify({\n"
	start := strings.Index(playerContextExtractJS, open)
	if start < 0 {
		t.Fatal("playerContextExtractJS has no multi-line success literal")
	}
	literal := playerContextExtractJS[start+len(open):]
	end := strings.Index(literal, "});")
	if end < 0 {
		t.Fatal("playerContextExtractJS success literal is unterminated")
	}
	literal = literal[:end]
	typ := reflect.TypeOf(PlayerContext{})
	names := func(t *testing.T, in, key string) {
		t.Helper()
		if !regexp.MustCompile(`(^|[\s{,])` + regexp.QuoteMeta(key) + `:`).MatchString(in) {
			t.Errorf("playerContextExtractJS never emits %q", key)
		}
	}
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if _, ok := sessionFilledContextKeys[name]; ok {
			continue
		}
		names(t, literal, name)
	}
	// Not a PlayerContext key: the success payload sets it so video_id_match means
	// one thing on every payload rather than reading false on the one that matched.
	names(t, literal, "video_id_match")

	const evidenceOpen = "const evidence = {\n"
	start = strings.Index(playerContextExtractJS, evidenceOpen)
	if start < 0 {
		t.Fatal("playerContextExtractJS has no evidence literal")
	}
	evidence := playerContextExtractJS[start+len(evidenceOpen):]
	if end = strings.Index(evidence, "};"); end < 0 {
		t.Fatal("playerContextExtractJS evidence literal is unterminated")
	}
	evidence = evidence[:end]
	raw := reflect.TypeOf(playerContextRaw{})
	for i := range raw.NumField() {
		f := raw.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		// The embedded context is the success literal's business, and every payload
		// sets error itself rather than taking it from the evidence block.
		if f.Anonymous || name == "" || name == "error" {
			continue
		}
		names(t, evidence, name)
	}
	// On the embedded struct, so the loop above cannot reach it, and the one
	// grading field both confirmTerminal and the establish deadline read.
	names(t, evidence, "playability_status")
}

// TestConfirmTerminal covers stale evidence that must not mark the current video
// unavailable, and the split between a verdict about the video and a bot check,
// which describes the session instead.
func TestConfirmTerminal(t *testing.T) {
	const want = "vid123"
	raw := func(mut func(*playerContextRaw)) playerContextRaw {
		r := playerContextRaw{Error: "pending"}
		mut(&r)
		return r
	}
	status := func(st, reason string) playerContextRaw {
		return raw(func(r *playerContextRaw) { r.PlayabilityStatus = st; r.Reason = reason; r.VideoIDMatch = true })
	}
	tests := []struct {
		name       string
		raw        playerContextRaw
		wantErr    error // nil, ErrUnplayable, or ErrBotCheck
		wantStatus string
	}{
		{"gen-matched onError 100, id match", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = want }), ErrUnplayable, "ERROR"},
		{"gen-matched onError 150, id match", raw(func(r *playerContextRaw) { r.ErrCode = 150; r.ErrGenMatch = true; r.ErrVideoID = want }), ErrUnplayable, "ERROR"},
		{"gen-matched onError 100, stale video id", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = "othervid" }), nil, ""},
		{"gen-matched onError 100, empty video id", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = "" }), nil, ""},
		{"non-OK status + id match", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "LOGIN_REQUIRED"; r.VideoIDMatch = true }), ErrUnplayable, "LOGIN_REQUIRED"},
		{"non-OK status for another video", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "ERROR"; r.VideoIDMatch = false }), nil, ""},
		{"onError 100 with gen mismatch", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = false; r.ErrVideoID = want }), nil, ""},
		{"onError 5 (non-terminal code)", raw(func(r *playerContextRaw) { r.ErrCode = 5; r.ErrGenMatch = true; r.ErrVideoID = want }), nil, ""},
		{"status OK + id match", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "OK"; r.VideoIDMatch = true }), nil, ""},
		{"no evidence", raw(func(r *playerContextRaw) {}), nil, ""},
		// The wall YouTube ships uses a curly apostrophe; fixtures and logs often
		// carry the ASCII one. Both are the same wall.
		{"bot check, curly apostrophe", status("LOGIN_REQUIRED", "Sign in to confirm you\u2019re not a bot"), ErrBotCheck, "LOGIN_REQUIRED"},
		{"bot check, ASCII apostrophe", status("LOGIN_REQUIRED", "Sign in to confirm you're not a bot"), ErrBotCheck, "LOGIN_REQUIRED"},
		{"bot check under another status", status("UNPLAYABLE", "Sign in to confirm you're not a bot"), ErrBotCheck, "UNPLAYABLE"},
		// These two share LOGIN_REQUIRED with the wall and stay per-video verdicts.
		{"private video", status("LOGIN_REQUIRED", "This video is private"), ErrUnplayable, "LOGIN_REQUIRED"},
		{"age gate", status("LOGIN_REQUIRED", "Sign in to confirm your age"), ErrUnplayable, "LOGIN_REQUIRED"},
		// YouTube omits videoDetails when it refuses this way, which leaves
		// video_id_match false. Gating the wall on that match would hide it behind a
		// poll timeout, so the feature would never fire in production.
		{"bot check without videoDetails", raw(func(r *playerContextRaw) {
			r.PlayabilityStatus = "LOGIN_REQUIRED"
			r.Reason = "Sign in to confirm you're not a bot"
			r.VideoIDMatch = false
		}), ErrBotCheck, "LOGIN_REQUIRED"},
		// A wall that also trips a terminal onError code is still the session's
		// problem: the phrase decides, and it is read before the code.
		{"bot check alongside a terminal onError code", raw(func(r *playerContextRaw) {
			r.ErrCode = 150
			r.ErrGenMatch = true
			r.ErrVideoID = want
			r.PlayabilityStatus = "LOGIN_REQUIRED"
			r.Reason = "Sign in to confirm you're not a bot"
			r.VideoIDMatch = true
		}), ErrBotCheck, "LOGIN_REQUIRED"},
		// A per-video reason never outranks the onError evidence, so the ordering
		// change is confined to the wall.
		{"private video alongside a terminal onError code", raw(func(r *playerContextRaw) {
			r.ErrCode = 150
			r.ErrGenMatch = true
			r.ErrVideoID = want
			r.PlayabilityStatus = "LOGIN_REQUIRED"
			r.Reason = "This video is private"
			r.VideoIDMatch = true
		}), ErrUnplayable, "LOGIN_REQUIRED"},
		// The onError branch keeps its literal only when the page has no fresh
		// status to report; a fresh non-OK status is the verdict.
		{"onError 150 with a fresh LOGIN_REQUIRED and no videoDetails", raw(func(r *playerContextRaw) {
			r.ErrCode = 150
			r.ErrGenMatch = true
			r.ErrVideoID = want
			r.PlayabilityStatus = "LOGIN_REQUIRED"
			r.Reason = "Private video"
			r.ResponseChanged = true
		}), ErrUnplayable, "LOGIN_REQUIRED"},
		// The response object is still the previous load's, so its status is not
		// this video's verdict; the onError literal stands.
		{"onError 150 while the response is still the previous load's", raw(func(r *playerContextRaw) {
			r.ErrCode = 150
			r.ErrGenMatch = true
			r.ErrVideoID = want
			r.PlayabilityStatus = "LOGIN_REQUIRED"
			r.Reason = "Private video"
			r.ResponseChanged = false
		}), ErrUnplayable, "ERROR"},
		// A refusal without videoDetails is terminal on its own once the response
		// is the new load's, without waiting for onError.
		{"non-OK status, no videoDetails, response changed", raw(func(r *playerContextRaw) {
			r.PlayabilityStatus = "LOGIN_REQUIRED"
			r.Reason = "Private video"
			r.ResponseChanged = true
		}), ErrUnplayable, "LOGIN_REQUIRED"},
		{"non-OK status, no videoDetails, response unchanged", raw(func(r *playerContextRaw) {
			r.PlayabilityStatus = "LOGIN_REQUIRED"
			r.Reason = "Private video"
		}), nil, ""},
		// An on-air broadcast is refused before any confirmation runs.
		{"live broadcast on air", raw(func(r *playerContextRaw) {
			r.PlayabilityStatus = "OK"
			r.VideoIDMatch = true
			r.IsLiveNow = true
		}), ErrUnplayable, LiveBroadcastStatus},
		{"live flag on another video's response", raw(func(r *playerContextRaw) {
			r.PlayabilityStatus = "OK"
			r.IsLiveNow = true
		}), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := confirmTerminal(tt.raw, want)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil (not terminal)", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tt.wantErr)
			}
			if errors.Is(err, ErrBotCheck) && errors.Is(err, ErrUnplayable) {
				t.Fatal("a bot check unwrapped to ErrUnplayable; the minter would negative-cache the video")
			}
			var gotStatus string
			if bc, ok := errors.AsType[*BotCheckError](err); ok {
				gotStatus = bc.Status
			} else if ue, ok := errors.AsType[*UnplayableError](err); ok {
				gotStatus = ue.Status
			} else {
				t.Fatalf("err = %v, want a typed terminal error", err)
			}
			if gotStatus != tt.wantStatus {
				t.Errorf("status = %q, want %q", gotStatus, tt.wantStatus)
			}
			// When a fresh status wins over an onError code, the detail keeps
			// both: the player's reason, and the code it also raised.
			if ue, ok := errors.AsType[*UnplayableError](err); ok && tt.wantStatus == "LOGIN_REQUIRED" && tt.raw.ErrCode != 0 {
				for _, sub := range []string{tt.raw.Reason, fmt.Sprintf("onError %d", tt.raw.ErrCode)} {
					if !strings.Contains(ue.Detail, sub) {
						t.Errorf("detail = %q, want it to contain %q", ue.Detail, sub)
					}
				}
			}
		})
	}
}

func TestIsBotCheck(t *testing.T) {
	for _, reason := range []string{
		"Sign in to confirm you\u2019re not a bot",
		"Sign in to confirm you're not a bot",
		"SIGN IN TO CONFIRM YOU'RE NOT A BOT",
	} {
		if !isBotCheck(reason) {
			t.Errorf("isBotCheck(%q) = false, want true", reason)
		}
	}
	for _, reason := range []string{
		"",
		"This video is private",
		"Sign in to confirm your age",
		"Video unavailable",
	} {
		if isBotCheck(reason) {
			t.Errorf("isBotCheck(%q) = true, want false", reason)
		}
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
// tolerance window stays within the video, and the zero floor keeps a video
// shorter than verifyEndTol from asking for a negative seek.
func TestSeekTarget(t *testing.T) {
	for _, tt := range []struct {
		length int
		want   int
	}{
		{0, fullLengthTargetSecs},   // unknown length uses the full-length target
		{-5, fullLengthTargetSecs},  // guard: a bogus negative length is treated as unknown
		{1, 0},                      // below verifyEndTol: the floor is why this is 0, not -2
		{2, 0},                      // still below verifyEndTol
		{3, 0},                      // length == verifyEndTol: the last floored length
		{4, 1},                      // first length the subtraction survives on its own
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
	// The clamp is the point: no length may ever produce a negative seek target,
	// whatever verifyEndTol or previewCapSecs become.
	for l := -5; l < 200; l++ {
		if got := seekTarget(l); got < 0 {
			t.Fatalf("seekTarget(%d) = %d, want >= 0", l, got)
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
		{0, bandVerify}, // unknown length verifies at the full-length target
		// The three lengths seekTarget's zero floor fires for. They are cap-safe on
		// the first branch, which never consults the target, so these rows pin that
		// the floor stays unreachable from here rather than exercising it.
		{1, bandCapSafe},
		{2, bandCapSafe},
		{3, bandCapSafe},
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
		{
			// url.Parse takes this happily as a relative path, so only the host
			// check keeps it from being echoed straight back into a log line.
			name: "parses, but is not a URL",
			in:   "not a url at all",
			want: "<unparseable>",
		},
		{
			name: "relative path",
			in:   "/videoplayback?id=o-ShortIdHere",
			want: "<unparseable>",
		},
		{
			// An absent URL is absent, not unparseable: established_url must not
			// claim there was one.
			name: "empty",
			in:   "",
			want: "",
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

// TestProofCandidatesClearTheMinimum pins that every fallback candidate can
// actually be probed. A candidate at or below fullLengthMinVideoSecs is rejected
// as OutcomeVideoTooShort before it is ever tried, so it costs a page load and an
// establish and can never establish a session.
func TestProofCandidatesClearTheMinimum(t *testing.T) {
	// Durations are recorded here rather than fetched, so the offline suite stays
	// offline. Update alongside proofCandidates. Measured with
	// `waxseal doctor --full --video <id>`, which reports length_seconds.
	lengths := map[string]int{
		DefaultVideo:  635, // Big Buck Bunny
		"R6MlUcmOul8": 734, // Tears of Steel
		"eRsGyueVLvQ": 888, // Sintel
	}
	for _, id := range proofCandidates {
		n, ok := lengths[id]
		if !ok {
			t.Fatalf("candidate %s has no recorded duration; add one", id)
		}
		if n <= fullLengthMinVideoSecs {
			t.Errorf("candidate %s is %ds, at or below the %ds minimum, so proveFullLength always rejects it", id, n, fullLengthMinVideoSecs)
		}
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
	botCheck := res{FullLengthProbe{Outcome: OutcomeNotEstablished}, &BotCheckError{Status: "LOGIN_REQUIRED", Reason: "Sign in to confirm you\u2019re not a bot"}}

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
			// A bot check is about the session, so another candidate would meet the
			// same wall.
			name:       "bot check stops the candidate walk",
			candidates: []string{"walled", "good"},
			results:    map[string]res{"walled": botCheck, "good": {full, nil}},
			wantErr:    true,
			errIs:      ErrBotCheck,
			errIsNot:   ErrUnplayable,
			wantCalls:  []string{"walled"},
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

// setupSession cannot be driven by a fake page (it takes a real *cdp.Browser),
// so the eval-to-warning mapping is tested on its own. A landing video the page
// refused is only ever a warning: attestation does not read it.
func TestWarnLandingPlayability(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantWarn  bool
	}{
		{"playable", `{"Status":"OK","Reason":""}`, false},
		{"no status read", `{}`, false},
		{"unparseable", `not json`, false},
		{"login required", `{"Status":"LOGIN_REQUIRED","Reason":"Private video"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			warnLandingPlayability(slog.New(slog.NewTextHandler(&logs, nil)), "vid", tc.raw)
			if got := strings.Contains(logs.String(), "landing video is not playable"); got != tc.wantWarn {
				t.Errorf("warned = %v, want %v (log %q)", got, tc.wantWarn, logs.String())
			}
			if tc.wantWarn && !strings.Contains(logs.String(), "Private video") {
				t.Errorf("log = %q, want it to carry the player's reason", logs.String())
			}
		})
	}

	// A wall is the session's problem, not the video's, so it says so rather
	// than implying another candidate would work.
	var logs strings.Builder
	warnLandingPlayability(slog.New(slog.NewTextHandler(&logs, nil)), "vid",
		`{"Status":"LOGIN_REQUIRED","Reason":"Sign in to confirm you\u2019re not a bot"}`)
	if !strings.Contains(logs.String(), "bot check") {
		t.Errorf("log = %q, want the wall named", logs.String())
	}
	if strings.Contains(logs.String(), "falls back to another candidate") {
		t.Errorf("log = %q, want it not to promise a fallback that is also walled", logs.String())
	}
}
