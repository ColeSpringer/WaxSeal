// Package browser manages the Chromium sessions used for BotGuard attestation,
// token minting, and player-context capture.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/innertube"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// DefaultVideo is the landing video used to capture the browser identity. It is
// Blender's Creative Commons movie "Big Buck Bunny."
const DefaultVideo = "aqz-KE-bpKQ"

// playerContextTimeout bounds how long PlayerContext waits for the player to load
// a video and expose its status-1 getPlayerResponse result.
const playerContextTimeout = 25 * time.Second

// playerContextPollInterval paces the player-context polling loops.
const playerContextPollInterval = 300 * time.Millisecond

// ErrUnplayable marks a terminal playabilityStatus. The minter caches this error
// instead of relaunching and attesting again.
var ErrUnplayable = errors.New("waxseal: video unplayable")

// UnplayableError reports a terminal playabilityStatus, such as a private,
// deleted, age-gated, region-blocked, or login-gated video. It wraps
// ErrUnplayable and preserves the status for structured error responses.
type UnplayableError struct {
	Status string // playabilityStatus, such as "LOGIN_REQUIRED"
	Detail string // player-provided reason, when present
}

func (e *UnplayableError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s (playabilityStatus %q)", ErrUnplayable.Error(), e.Detail, e.Status)
	}
	return fmt.Sprintf("%s (playabilityStatus %q)", ErrUnplayable.Error(), e.Status)
}

func (e *UnplayableError) Unwrap() error { return ErrUnplayable }

// Options configure a browser Session. The zero value auto-detects Chromium,
// runs in the new headless mode, and discards logs.
type Options struct {
	ChromeBin   string        // explicit Chromium binary; "" auto-detects (WAXSEAL_CHROME_BIN, then well-known paths)
	Headful     bool          // run headful (needs a display/Xvfb); default is headless=new
	NormalizeUA bool          // remove the HeadlessChrome marker in headless mode; UA-CH already matches Chromium
	Logger      *slog.Logger  // nil discards
	NavTimeout  time.Duration // watch-page navigation budget (default 45s)
}

// Identity contains the browser session values that a consumer needs to adopt
// the same guest identity.
type Identity struct {
	WatchURL      string `json:"watch_url"`
	VisitorData   string `json:"visitor_data"`
	ClientVersion string `json:"client_version"`
	APIKey        string `json:"api_key,omitempty"`
	UserAgent     string `json:"user_agent"`
	Webdriver     bool   `json:"navigator_webdriver"` // must be false; true means an automation artifact leaked
	Cookies       int    `json:"cookie_count"`
	STS           int    `json:"signature_timestamp"` // from base.js; required or /player returns UNPLAYABLE
}

// Session owns a Chromium page with the browser bundle installed. Its Go HTTP
// client uses the page's cookies so att/get and GenerateIT share the browser's
// session and egress IP.
type Session struct {
	browser *rod.Browser
	page    *rod.Page
	dispose func() // closes the browser from Launch or the context from Pool
	id      Identity
	client  *httpx.Client // egresses with the browser's cookies
	log     *slog.Logger

	// One attestation installs a warm minter that mints many identifiers: a player
	// token bound to a video_id, or a GVS token bound to a visitor_data.
	attestKind     string // "", "integrity", or "fallback"
	fallbackToken  string // set on the fallback path (no per-identifier minter)
	lifetimeSecs   int
	tokenExpiresAt time.Time // when tokens from this attestation expire (attest time + lifetime)
}

// Launch starts a dedicated Chromium for one Session, parks a page on videoID's
// watch page, captures the identity, injects the bundle, and builds the Go HTTP
// client. The caller must Close the returned Session. For multiple isolated
// identities on one browser (multi-tenant), use LaunchPool and Pool.NewSession.
func Launch(ctx context.Context, videoID string, opts Options) (*Session, error) {
	opts = withDefaults(opts)
	browser, l, profile, err := launchChromium(opts)
	if err != nil {
		return nil, err
	}
	teardown := func() {
		_ = browser.Close()
		l.Kill()
		_ = os.RemoveAll(profile)
	}
	s, err := setupSession(ctx, browser, videoID, opts)
	if err != nil {
		teardown()
		return nil, err
	}
	s.dispose = teardown
	return s, nil
}

func withDefaults(opts Options) Options {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.NavTimeout <= 0 {
		opts.NavTimeout = 45 * time.Second
	}
	return opts
}

// launchChromium starts Chromium and connects rod to it. The caller must close
// the browser, stop the launcher, and remove the returned profile directory.
func launchChromium(opts Options) (*rod.Browser, *launcher.Launcher, string, error) {
	bin := opts.ChromeBin
	if bin == "" {
		b, err := DetectChrome()
		if err != nil {
			return nil, nil, "", err
		}
		bin = b
	}
	opts.Logger.Info("waxseal: launching chromium", "bin", bin, "headful", opts.Headful)

	// Snap-confined Chromium can only write a user-data-dir under $HOME, not /tmp.
	profileDir, err := os.MkdirTemp(homeTmpBase(), ".waxseal-")
	if err != nil {
		return nil, nil, "", fmt.Errorf("waxseal: temp profile: %w", err)
	}
	l := launcher.New().
		Bin(bin).
		Leakless(false). // avoid go-rod's leakless helper download; Close() kills the browser
		Set("user-data-dir", profileDir).
		Set("no-sandbox").            // snap confinement provides isolation; experiment-only
		Set("disable-dev-shm-usage"). // WSL2 /dev/shm is small
		Set("disable-gpu").
		Set("mute-audio").
		// Without these, go-rod leaves navigator.webdriver === true, which no real
		// browser sets. This removes the automation flag without changing the
		// remaining fingerprint.
		Delete("enable-automation").
		Set("disable-blink-features", "AutomationControlled")
	if opts.Headful {
		l = l.Headless(false)
	} else {
		l = l.Headless(false).Set("headless", "new")
	}

	controlURL, err := l.Launch()
	if err != nil {
		l.Kill()
		_ = os.RemoveAll(profileDir)
		return nil, nil, "", fmt.Errorf("waxseal: launch chromium: %w", err)
	}
	// NoDefaultDevice prevents go-rod from replacing the browser's Linux user
	// agent with its built-in Chrome 114 macOS value.
	browser := rod.New().ControlURL(controlURL).NoDefaultDevice()
	if err := browser.Connect(); err != nil {
		l.Kill()
		_ = os.RemoveAll(profileDir)
		return nil, nil, "", fmt.Errorf("waxseal: connect cdp: %w", err)
	}
	return browser, l, profileDir, nil
}

// setupSession parks a page in browser (the main browser, or an incognito context
// for a tenant), navigates to videoID's watch page, captures the identity, injects
// the bundle, and builds the HTTP client. dispose is left for the caller to set.
// On error the caller is responsible for teardown.
func setupSession(ctx context.Context, browser *rod.Browser, videoID string, opts Options) (_ *Session, err error) {
	s := &Session{browser: browser, log: opts.Logger}

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("waxseal: new page: %w", err)
	}
	s.page = page.Context(ctx)

	// Bypass CSP so the injected bundle's new Function(interpreter) can run on the
	// youtube.com origin (which otherwise forbids unsafe-eval).
	if err = (proto.PageSetBypassCSP{Enabled: true}).Call(s.page); err != nil {
		return nil, fmt.Errorf("waxseal: bypass csp: %w", err)
	}

	if opts.NormalizeUA {
		if err = s.normalizeUA(ctx); err != nil {
			return nil, err
		}
	}

	watchURL := "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
	navCtx, cancel := context.WithTimeout(ctx, opts.NavTimeout)
	defer cancel()
	if err = s.page.Context(navCtx).Navigate(watchURL); err != nil {
		return nil, fmt.Errorf("waxseal: navigate watch page: %w", err)
	}
	if err = s.page.Context(navCtx).WaitLoad(); err != nil {
		return nil, fmt.Errorf("waxseal: wait load: %w", err)
	}

	if err = s.captureIdentity(navCtx, watchURL); err != nil {
		return nil, err
	}
	// signatureTimestamp is mandatory: a /player request without it returns
	// UNPLAYABLE regardless of the token, so it must be captured before any consume.
	if err = s.captureSTS(navCtx); err != nil {
		return nil, err
	}
	opts.Logger.Info("waxseal: identity",
		"visitor_data_len", len(s.id.VisitorData),
		"client_version", s.id.ClientVersion,
		"webdriver", s.id.Webdriver,
		"cookies", s.id.Cookies,
		"sts", s.id.STS)
	if s.id.Webdriver {
		return nil, fmt.Errorf("waxseal: navigator.webdriver is true; automation artifact leaked")
	}

	// Inject the bundle (defines runBotguard/newMinter/mint on globalThis).
	if _, err = s.page.Eval(`(src) => { (0, eval)(src); return true }`, browserBundle); err != nil {
		return nil, fmt.Errorf("waxseal: inject bundle: %w", err)
	}

	if err = s.buildCoherentClient(); err != nil {
		return nil, err
	}
	return s, nil
}

// Pool owns one Chromium and creates isolated incognito-context Sessions. Each
// session has its own guest identity, cookies, and storage. All sessions share
// the browser's egress IP.
type Pool struct {
	browser  *rod.Browser
	launcher *launcher.Launcher
	profile  string
	opts     Options
}

// LaunchPool starts the shared Chromium. Close it to tear everything down.
func LaunchPool(opts Options) (*Pool, error) {
	opts = withDefaults(opts)
	browser, l, profile, err := launchChromium(opts)
	if err != nil {
		return nil, err
	}
	return &Pool{browser: browser, launcher: l, profile: profile, opts: opts}, nil
}

// NewSession creates a fresh isolated browser context and parks a Session in it.
// Closing the Session disposes its context without closing the shared browser.
func (p *Pool) NewSession(ctx context.Context, videoID string) (*Session, error) {
	incog, err := p.browser.Incognito()
	if err != nil {
		return nil, fmt.Errorf("waxseal: new browser context: %w", err)
	}
	cid := incog.BrowserContextID
	dispose := func() { _ = proto.TargetDisposeBrowserContext{BrowserContextID: cid}.Call(p.browser) }
	s, err := setupSession(ctx, incog, videoID, p.opts)
	if err != nil {
		dispose()
		return nil, err
	}
	s.dispose = dispose
	return s, nil
}

// Close tears down the shared browser and removes the temp profile.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	if p.browser != nil {
		_ = p.browser.Close()
	}
	if p.launcher != nil {
		p.launcher.Kill()
	}
	if p.profile != "" {
		_ = os.RemoveAll(p.profile)
	}
}

// captureIdentity polls ytcfg after the SPA boots and records visitor_data, the
// client version, the API key, navigator.userAgent, and navigator.webdriver.
func (s *Session) captureIdentity(ctx context.Context, watchURL string) error {
	const js = `() => {
		const c = (typeof ytcfg !== 'undefined' && ytcfg) ? ytcfg : (window.ytcfg || null);
		const ctxData = c && c.get ? c.get('INNERTUBE_CONTEXT') : null;
		return JSON.stringify({
			vd:   (c && c.get && (c.get('VISITOR_DATA') || (ctxData && ctxData.client && ctxData.client.visitorData))) || "",
			cv:   (c && c.get && c.get('INNERTUBE_CLIENT_VERSION')) || "",
			key:  (c && c.get && c.get('INNERTUBE_API_KEY')) || "",
			ua:   navigator.userAgent || "",
			wd:   navigator.webdriver === true,
		});
	}`
	deadline := time.Now().Add(30 * time.Second)
	var ident struct {
		VD, CV, Key, UA string
		WD              bool
	}
	for {
		obj, err := s.page.Context(ctx).Eval(js)
		if err == nil {
			var raw struct {
				VD  string `json:"vd"`
				CV  string `json:"cv"`
				Key string `json:"key"`
				UA  string `json:"ua"`
				WD  bool   `json:"wd"`
			}
			if jerr := json.Unmarshal([]byte(obj.Value.Str()), &raw); jerr == nil && raw.VD != "" {
				ident.VD, ident.CV, ident.Key, ident.UA, ident.WD = raw.VD, raw.CV, raw.Key, raw.UA, raw.WD
				break
			}
		}
		if time.Now().After(deadline) {
			if ident.UA == "" {
				return fmt.Errorf("waxseal: ytcfg visitor_data not available before deadline")
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	cookies, err := s.page.Context(ctx).Cookies([]string{"https://www.youtube.com"})
	if err != nil {
		return fmt.Errorf("waxseal: read cookies: %w", err)
	}
	s.id = Identity{
		WatchURL:      watchURL,
		VisitorData:   ident.VD,
		ClientVersion: ident.CV,
		APIKey:        ident.Key,
		UserAgent:     ident.UA,
		Webdriver:     ident.WD,
		Cookies:       len(cookies),
	}
	return nil
}

// normalizeUA removes the HeadlessChrome marker from navigator.userAgent before
// navigation and keeps UA-CH consistent. No other fingerprint values are changed.
func (s *Session) normalizeUA(ctx context.Context) error {
	obj, err := s.page.Context(ctx).Eval(`() => navigator.userAgent`)
	if err != nil {
		return fmt.Errorf("waxseal: read ua for normalize: %w", err)
	}
	realUA := obj.Value.Str()
	fixed := strings.Replace(realUA, "HeadlessChrome", "Chrome", 1)
	major := "149"
	if m := regexp.MustCompile(`Chrome/(\d+)`).FindStringSubmatch(fixed); m != nil {
		major = m[1]
	}
	full := major + ".0.0.0"
	md := &proto.EmulationUserAgentMetadata{
		Brands: []*proto.EmulationUserAgentBrandVersion{
			{Brand: "Chromium", Version: major},
			{Brand: "Not)A;Brand", Version: "24"},
		},
		FullVersionList: []*proto.EmulationUserAgentBrandVersion{
			{Brand: "Chromium", Version: full},
			{Brand: "Not)A;Brand", Version: "24.0.0.0"},
		},
		Platform:        "Linux",
		PlatformVersion: "",
		Architecture:    "x86",
		Bitness:         "64",
		Mobile:          false,
		FullVersion:     full,
	}
	if err := (proto.NetworkSetUserAgentOverride{UserAgent: fixed, UserAgentMetadata: md}).Call(s.page); err != nil {
		return fmt.Errorf("waxseal: ua override: %w", err)
	}
	s.log.Info("waxseal: normalized UA (HeadlessChrome->Chrome)")
	return nil
}

// captureSTS extracts signatureTimestamp from the player base.js (via an in-page
// fetch, so it uses the page's own session). Without it /player is UNPLAYABLE.
func (s *Session) captureSTS(ctx context.Context) error {
	const js = `async () => {
		const c = (typeof ytcfg !== 'undefined' && ytcfg) ? ytcfg : window.ytcfg;
		const playerUrl = (c && c.get && c.get('PLAYER_JS_URL')) || '';
		if (!playerUrl) return 0;
		try {
			const r = await fetch(new URL(playerUrl, location.origin).href, { credentials: "include" });
			const txt = await r.text();
			const m = txt.match(/signatureTimestamp:(\d+)/) || txt.match(/sts:(\d+)/);
			return m ? parseInt(m[1], 10) : 0;
		} catch (e) { return 0; }
	}`
	obj, err := s.page.Context(ctx).Eval(js)
	if err != nil {
		return fmt.Errorf("waxseal: capture sts: %w", err)
	}
	s.id.STS = obj.Value.Int()
	if s.id.STS == 0 {
		s.log.Warn("waxseal: signatureTimestamp not found; /player will likely be UNPLAYABLE")
	}
	return nil
}

// buildCoherentClient seeds a Go cookie jar from the browser's youtube.com
// cookies so the Go-side att/get and GenerateIT calls carry the same session as the
// page (the egress IP matches automatically, since it is the same host).
func (s *Session) buildCoherentClient() error {
	cookies, err := s.page.Cookies([]string{"https://www.youtube.com"})
	if err != nil {
		return fmt.Errorf("waxseal: cookies for jar: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	yt, _ := url.Parse("https://www.youtube.com")
	hc := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		hc = append(hc, &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path})
	}
	jar.SetCookies(yt, hc)
	s.client = httpx.New(&http.Client{Jar: jar, Timeout: 60 * time.Second})
	return nil
}

// Identity returns the captured real session identity.
func (s *Session) Identity() Identity { return s.id }

// BrowserCookies returns the browser's youtube.com cookies so a consumer can
// adopt the same guest session.
//
// It reads the browser-level cookie store because page-level reads can be empty
// after the page leaves youtube.com.
func (s *Session) BrowserCookies() []*http.Cookie {
	cs, err := s.browser.GetCookies()
	if err != nil {
		return nil
	}
	out := make([]*http.Cookie, 0, len(cs))
	for _, c := range cs {
		if !strings.Contains(c.Domain, "youtube.com") {
			continue
		}
		out = append(out, &http.Cookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HttpOnly: c.HTTPOnly,
		})
	}
	return out
}

// Page returns the underlying rod page used by the minter's crash watcher.
func (s *Session) Page() *rod.Page { return s.page }

// MintResult is one mint outcome: the token, whether it came from the integrity
// or fallback path, and its binding.
type MintResult struct {
	Kind       string    `json:"kind"` // "integrity" | "fallback"
	Token      string    `json:"-"`    // never logged/serialized raw
	TokenLen   int       `json:"token_len"`
	Identifier string    `json:"identifier"` // video_id (player) or visitor_data (gvs)
	Lifetime   int       `json:"lifetime_secs"`
	ExpiresAt  time.Time `json:"-"` // absolute expiry (attest time + lifetime); zero if unknown
}

// Attest runs the once-per-session attestation. Go fetches the att/get challenge,
// the browser runs BotGuard, and Go calls GenerateIT. Later Mint calls reuse the
// resulting integrity minter or fallback token.
func (s *Session) Attest(ctx context.Context) error {
	if s.attestKind != "" {
		return nil
	}
	ua := s.id.UserAgent

	ictx := innertube.GuestContext(s.id.VisitorData, s.id.ClientVersion)
	ch, err := innertube.GetChallenge(ctx, s.client, ua, ictx)
	if err != nil {
		return fmt.Errorf("waxseal: challenge: %w", err)
	}
	s.log.Info("waxseal: challenge resolved", "interp_len", len(ch.InterpreterJS), "global", ch.GlobalName)

	obj, err := s.page.Context(ctx).Eval(
		`(interp, prog, name) => runBotguard(interp, prog, name)`,
		ch.InterpreterJS, ch.Program, ch.GlobalName,
	)
	if err != nil {
		return fmt.Errorf("waxseal: runBotguard: %w", err)
	}
	botguardResponse := obj.Value.Str()
	if botguardResponse == "" {
		return fmt.Errorf("waxseal: empty botguardResponse")
	}
	s.log.Info("waxseal: snapshot ok", "botguard_response_len", len(botguardResponse))

	it, err := botguard.GenerateIT(ctx, s.client, ua, botguardResponse, botguard.DefaultEndpoint)
	if err != nil {
		return fmt.Errorf("waxseal: GenerateIT: %w", err)
	}
	s.lifetimeSecs = it.LifetimeSecs
	if it.LifetimeSecs > 0 {
		// Tokens from this attestation expire when the attestation does (its
		// lifetime measured from attest time), regardless of when an individual
		// token is later minted off the warm minter.
		s.tokenExpiresAt = time.Now().Add(time.Duration(it.LifetimeSecs) * time.Second)
	}

	if !it.HasIntegrity() {
		if !it.HasFallback() {
			return fmt.Errorf("waxseal: GenerateIT returned no token")
		}
		if _, verr := botguard.ValidatePOToken(it.FallbackToken); verr != nil {
			return fmt.Errorf("waxseal: fallback failed field-6 validation: %w", verr)
		}
		s.attestKind = "fallback"
		s.fallbackToken = it.FallbackToken
		s.log.Warn("waxseal: only a fallback token was granted (no integrity); IP/session not granting integrity right now")
		return nil
	}
	if _, err = s.page.Context(ctx).Eval(`(tok) => newMinter(tok)`, it.IntegrityToken); err != nil {
		return fmt.Errorf("waxseal: newMinter: %w", err)
	}
	s.attestKind = "integrity"
	s.log.Info("waxseal: INTEGRITY attestation installed; warm minter ready", "lifetime_secs", it.LifetimeSecs)
	return nil
}

// Mint produces a token bound to identifier from the session's attestation. The
// integrity path mints a new token in the browser. The fallback path returns the
// single fallback token from Google. Both paths validate protobuf field 6.
func (s *Session) Mint(ctx context.Context, identifier string) (MintResult, error) {
	if err := s.Attest(ctx); err != nil {
		return MintResult{}, err
	}
	if s.attestKind == "fallback" {
		return MintResult{Kind: "fallback", Token: s.fallbackToken, TokenLen: len(s.fallbackToken), Identifier: identifier, Lifetime: s.lifetimeSecs, ExpiresAt: s.tokenExpiresAt}, nil
	}
	mintObj, err := s.page.Context(ctx).Eval(`(id) => mint(id)`, identifier)
	if err != nil {
		return MintResult{}, fmt.Errorf("waxseal: mint: %w", err)
	}
	token := mintObj.Value.Str()
	if token == "" {
		return MintResult{}, fmt.Errorf("waxseal: empty minted token")
	}
	if _, err = botguard.ValidatePOToken(token); err != nil {
		return MintResult{}, fmt.Errorf("waxseal: minted token failed field-6 validation: %w", err)
	}
	s.log.Info("waxseal: integrity token minted", "len", len(token), "identifier_len", len(identifier))
	return MintResult{Kind: "integrity", Token: token, TokenLen: len(token), Identifier: identifier, Lifetime: s.lifetimeSecs, ExpiresAt: s.tokenExpiresAt}, nil
}

// PlayerContext is the status-1 streaming context returned by the attested
// browser for one video. The consumer must descramble the SABR URL's n parameter
// with PlayerURL before starting the stream.
//
// client.PlayerContext mirrors this wire format without importing the browser
// package. Keep the JSON tags in sync.
type PlayerContext struct {
	Status                       string        `json:"status"`     // playabilityStatus.status; "OK" when streamable
	PlayerURL                    string        `json:"player_url"` // base.js URL used to descramble the SABR URL's n parameter
	ServerAbrStreamingURL        string        `json:"server_abr_streaming_url"`
	VideoPlaybackUstreamerConfig string        `json:"video_playback_ustreamer_config"`
	VisitorData                  string        `json:"visitor_data"`
	ClientVersion                string        `json:"client_version"`
	Title                        string        `json:"title"`
	Author                       string        `json:"author"`
	LengthSeconds                int           `json:"length_seconds"`
	AudioFormats                 []AudioFormat `json:"audio_formats"`
}

// AudioFormat describes one adaptive audio format. Itag, LMT, and XTags must be
// used together. An inconsistent selector makes the SABR server request a
// player-response reload instead of returning media.
type AudioFormat struct {
	Itag             int    `json:"itag"`
	LMT              string `json:"lmt"` // lastModified, kept as a string because it is a large opaque URL parameter
	XTags            string `json:"xtags"`
	MimeType         string `json:"mime_type"`
	Bitrate          int    `json:"bitrate"`
	ContentLength    int64  `json:"content_length"`
	ApproxDurationMs int    `json:"approx_duration_ms"`
	AudioSampleRate  int    `json:"audio_sample_rate"`
	AudioChannels    int    `json:"audio_channels"`
	AudioQuality     string `json:"audio_quality"`
	IsDrc            bool   `json:"is_drc"`         // whether client_abr_state.drc_enabled is required
	AudioTrackID     string `json:"audio_track_id"` // audioTrack.id; empty for the default or only track
}

// playerReadyJS reports whether the player exposes the APIs required to load and
// inspect a video.
const playerReadyJS = `() => { const p = document.getElementById('movie_player'); return !!(p && p.loadVideoById && p.getPlayerResponse); }`

// playerLoadJS resets the player, applies the visibility overrides needed for
// headless playback, and loads videoID. The reset keeps a repeated request from
// observing buffered state from the previous request.
//
// Each load gets a new generation and clears the previous error marker. The
// onError marker records both the generation and the player's video ID so late
// errors from a previous load can be ignored.
const playerLoadJS = `(videoId) => {
	try {
		try { Object.defineProperty(document, 'visibilityState', { get: () => 'visible', configurable: true }); } catch (e) {}
		try { Object.defineProperty(document, 'hidden', { get: () => false, configurable: true }); } catch (e) {}
		document.dispatchEvent(new Event('visibilitychange'));
		const p = document.getElementById('movie_player');
		if (!p || !p.loadVideoById) return false;
		var gen = (window.__wsGen || 0) + 1;
		window.__wsGen = gen;
		window.__wsErr = null;
		try { if (window.__wsErrH && p.removeEventListener) p.removeEventListener('onError', window.__wsErrH); } catch (e) {}
		var handler = function (code) {
			if (window.__wsGen !== gen) return;
			// movie_player passes a number today, while the iframe API uses {data, target}.
			// Accept both forms so a change in event shape does not disable fast failure.
			var c = (code && typeof code === 'object') ? code.data : code;
			var vid = '';
			try { var vd = p.getVideoData && p.getVideoData(); vid = (vd && vd.video_id) || ''; } catch (e) {}
			window.__wsErr = { gen: gen, code: Number(c), vid: vid };
		};
		window.__wsErrH = handler;
		try { if (p.addEventListener) p.addEventListener('onError', handler); } catch (e) {}
		try { if (p.stopVideo) p.stopVideo(); } catch (e) {}
		p.loadVideoById(videoId);
		return true;
	} catch (e) { return false; }
}`

// playerDriveJS keeps muted playback active while the Go side polls for an
// established context. Promise rejections are handled in-page to avoid unhandled
// rejection events.
const playerDriveJS = `() => {
	try {
		const p = document.getElementById('movie_player');
		if (p && p.playVideo) { try { p.playVideo(); } catch (e) {} }
		const v = document.querySelector('video');
		if (v) { v.muted = true; try { const pr = v.play(); if (pr && pr.catch) pr.catch(function () {}); } catch (e) {} }
	} catch (e) {}
	return true;
}`

// playerContextExtractJS reads the player context and returns it once videoID has
// loaded and media has buffered. Until then, it returns polling state and the
// evidence confirmTerminal needs to reject stale errors from previous loads.
const playerContextExtractJS = `(videoId) => {
	try {
		const c = (typeof ytcfg !== 'undefined' && ytcfg) ? ytcfg : window.ytcfg;
		const p = document.getElementById('movie_player');
		if (!p || !p.getPlayerResponse) return JSON.stringify({ error: 'player api unavailable' });
		const j = p.getPlayerResponse();
		const errMark = window.__wsErr;
		const status = (j && j.playabilityStatus && j.playabilityStatus.status) || '';
		const evidence = {
			status: status,
			reason: (j && j.playabilityStatus && j.playabilityStatus.reason) || '',
			error_code: (errMark && typeof errMark.code === 'number') ? errMark.code : 0,
			err_gen_match: !!(errMark && errMark.gen === window.__wsGen),
			err_video_id: (errMark && errMark.vid) || '',
			video_id_match: !!(j && j.videoDetails && j.videoDetails.videoId === videoId),
		};
		if (!evidence.video_id_match) return JSON.stringify(Object.assign({ error: 'pending: player response not yet for ' + videoId }, evidence));
		if (status && status !== 'OK') return JSON.stringify(Object.assign({ error: 'unplayable: ' + status }, evidence));
		const sd = j.streamingData || {};
		if (!sd.serverAbrStreamingUrl) return JSON.stringify(Object.assign({ error: 'pending: no serverAbrStreamingUrl' }, evidence));
		const v = document.querySelector('video');
		const buffered = (v && v.buffered && v.buffered.length) ? v.buffered.end(v.buffered.length - 1) : 0;
		if (buffered <= 0) return JSON.stringify(Object.assign({ error: 'pending: session not established (no buffered media yet)' }, evidence));
		const vd = j.videoDetails;
		const urc = (j.playerConfig && j.playerConfig.mediaCommonConfig && j.playerConfig.mediaCommonConfig.mediaUstreamerRequestConfig) || {};
		const ctxData = (c && c.get) ? c.get('INNERTUBE_CONTEXT') : null;
		const playerJs = (c && c.get) ? (c.get('PLAYER_JS_URL') || '') : '';
		const audioFormats = (sd.adaptiveFormats || [])
			.filter(function (f) { return (f.mimeType || '').indexOf('audio/') === 0; })
			.map(function (f) {
				return {
					itag: f.itag, lmt: String(f.lastModified || ''), xtags: f.xtags || '', mime_type: f.mimeType || '', bitrate: f.bitrate || 0,
					content_length: Number(f.contentLength || 0), approx_duration_ms: Number(f.approxDurationMs || 0),
					audio_sample_rate: Number(f.audioSampleRate || 0), audio_channels: Number(f.audioChannels || 0), audio_quality: f.audioQuality || '',
					is_drc: f.isDrc === true, audio_track_id: (f.audioTrack && f.audioTrack.id) || '',
				};
			});
		const visitorData = (function () {
			if (j.responseContext && j.responseContext.visitorData) return j.responseContext.visitorData;
			if (c && c.get) return c.get('VISITOR_DATA') || (ctxData && ctxData.client && ctxData.client.visitorData) || '';
			return '';
		})();
		return JSON.stringify({
			status: status,
			player_url: playerJs ? new URL(playerJs, location.origin).href : '',
			server_abr_streaming_url: sd.serverAbrStreamingUrl,
			video_playback_ustreamer_config: urc.videoPlaybackUstreamerConfig || '',
			visitor_data: visitorData,
			client_version: (c && c.get) ? (c.get('INNERTUBE_CLIENT_VERSION') || '') : '',
			title: vd.title || '', author: vd.author || '', length_seconds: Number(vd.lengthSeconds || 0),
			audio_formats: audioFormats,
		});
	} catch (e) {
		return JSON.stringify({ error: String(e) });
	}
}`

// playerContextCleanupJS stops playback and restores the page's native visibility
// state before the shared page is reused. The fallback definitions keep the page
// hidden if an own-property accessor cannot be deleted.
const playerContextCleanupJS = `() => {
	try { const p = document.getElementById('movie_player'); if (p && p.pauseVideo) p.pauseVideo(); } catch (e) {}
	try {
		delete document.visibilityState;
		if (Object.getOwnPropertyDescriptor(document, 'visibilityState')) Object.defineProperty(document, 'visibilityState', { get: () => 'hidden', configurable: true });
	} catch (e) {}
	try {
		delete document.hidden;
		if (Object.getOwnPropertyDescriptor(document, 'hidden')) Object.defineProperty(document, 'hidden', { get: () => true, configurable: true });
	} catch (e) {}
	return true;
}`

// playerContextRaw combines the public context with polling state and the evidence
// used to reject stale player errors.
type playerContextRaw struct {
	PlayerContext
	Error        string `json:"error"`
	Reason       string `json:"reason"`         // playabilityStatus.reason, when present
	ErrCode      int    `json:"error_code"`     // movie_player onError code; zero if none
	ErrGenMatch  bool   `json:"err_gen_match"`  // error marker belongs to the current load generation
	ErrVideoID   string `json:"err_video_id"`   // video reported by the player when onError fired
	VideoIDMatch bool   `json:"video_id_match"` // player response belongs to the requested video
}

// confirmTerminal returns a terminal error only when the evidence belongs to
// videoID. The generation and video ID checks reject late errors from a previous
// load.
func confirmTerminal(raw playerContextRaw, videoID string) (*UnplayableError, bool) {
	if raw.ErrGenMatch && raw.ErrVideoID == videoID && isUnavailableCode(raw.ErrCode) {
		return &UnplayableError{Status: "ERROR", Detail: fmt.Sprintf("player onError %d", raw.ErrCode)}, true
	}
	if raw.Status != "" && raw.Status != "OK" && raw.VideoIDMatch {
		return &UnplayableError{Status: raw.Status, Detail: raw.Reason}, true
	}
	return nil, false
}

// isUnavailableCode reports whether a movie_player onError code describes a video
// that cannot become playable on retry. Codes 2, 100, 101, and 150 cover invalid
// IDs, missing videos, and playback restrictions. Recoverable and unknown errors
// remain subject to the normal polling deadline.
func isUnavailableCode(code int) bool {
	switch code {
	case 2, 100, 101, 150:
		return true
	default:
		return false
	}
}

// PlayerContext returns videoID's status-1 streaming context from the real player.
// A bare in-page /player request lacks the playback signals needed for status 1.
// The returned SABR URL still contains a throttling nonce for the consumer to
// descramble with PlayerURL.
//
// A terminal playabilityStatus returns ErrUnplayable. Playback and visibility
// changes are reverted before the shared page is reused.
func (s *Session) PlayerContext(ctx context.Context, videoID string) (PlayerContext, error) {
	page := s.page.Context(ctx)
	defer s.revertPlayerContext(ctx)

	raw, err := s.establish(ctx, page, videoID, time.Now().Add(playerContextTimeout))
	if err != nil {
		return PlayerContext{}, err
	}
	if raw.ServerAbrStreamingURL == "" {
		return PlayerContext{}, fmt.Errorf("waxseal: player-context: no serverAbrStreamingUrl (playabilityStatus %q)", raw.Status)
	}
	if raw.PlayerURL == "" {
		return PlayerContext{}, fmt.Errorf("waxseal: player-context: no player_url (PLAYER_JS_URL missing); consumer cannot descramble n")
	}
	if raw.AudioFormats == nil {
		raw.AudioFormats = []AudioFormat{}
	}
	return raw.PlayerContext, nil
}

// revertPlayerContext stops playback and restores the visibility override used by
// PlayerContext and VerifyFullLength. Its detached context allows cleanup to run
// after the request context has been canceled.
func (s *Session) revertPlayerContext(ctx context.Context) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, _ = s.page.Context(cctx).Eval(playerContextCleanupJS)
}

// establish loads videoID and drives muted playback until the player returns an
// established context. Both PlayerContext and VerifyFullLength use this path so
// the diagnostic probe exercises the same setup as the production endpoint. The
// caller is responsible for restoring the shared page.
func (s *Session) establish(ctx context.Context, page *rod.Page, videoID string, deadline time.Time) (playerContextRaw, error) {
	// Phase 1: wait for the player API to hydrate, then point it at videoID once.
	for {
		ready, err := page.Eval(playerReadyJS)
		if err != nil {
			return playerContextRaw{}, fmt.Errorf("waxseal: player-context ready probe: %w", err)
		}
		if ready.Value.Bool() {
			break
		}
		if time.Now().After(deadline) {
			return playerContextRaw{}, fmt.Errorf("waxseal: player-context: movie_player.loadVideoById unavailable before deadline")
		}
		select {
		case <-ctx.Done():
			return playerContextRaw{}, ctx.Err()
		case <-time.After(playerContextPollInterval):
		}
	}
	loaded, err := page.Eval(playerLoadJS, videoID)
	if err != nil {
		return playerContextRaw{}, fmt.Errorf("waxseal: player-context loadVideoById: %w", err)
	}
	if !loaded.Value.Bool() {
		return playerContextRaw{}, fmt.Errorf("waxseal: player-context: movie_player.loadVideoById unavailable")
	}

	// Wait once before the first read so the asynchronous stop and load operations
	// cannot expose state left by a previous request for the same video.
	evalErrs := 0
	for {
		select {
		case <-ctx.Done():
			return playerContextRaw{}, ctx.Err()
		case <-time.After(playerContextPollInterval):
		}
		_, _ = page.Eval(playerDriveJS)
		obj, evalErr := page.Eval(playerContextExtractJS, videoID)
		if evalErr != nil {
			// Tolerate a one-off CDP hiccup, but fail fast on a crashed/closed page
			// (persistent errors) instead of spinning to the deadline.
			evalErrs++
			if evalErrs >= 3 || time.Now().After(deadline) {
				return playerContextRaw{}, fmt.Errorf("waxseal: player-context extract: %w", evalErr)
			}
			continue
		}
		evalErrs = 0
		var raw playerContextRaw
		if err := json.Unmarshal([]byte(obj.Value.Str()), &raw); err != nil {
			return playerContextRaw{}, fmt.Errorf("waxseal: player-context parse: %w", err)
		}
		if ue, ok := confirmTerminal(raw, videoID); ok {
			return playerContextRaw{}, ue
		}
		if raw.Error == "" {
			return raw, nil // established context captured
		}
		if time.Now().After(deadline) {
			return playerContextRaw{}, fmt.Errorf("waxseal: player-context: %s", raw.Error)
		}
	}
}

// The full-length probe seeks beyond the roughly 70-second status-2 preview cap
// and confirms that playback advances through buffered media at the target.
const (
	fullLengthTargetSecs   = 100              // seek target beyond the preview cap
	fullLengthMinVideoSecs = 120              // minimum duration that leaves enough media after the target
	fullLengthTolSecs      = 2.0              // required buffered media after the target
	fullLengthProbeBudget  = 30 * time.Second // maximum time spent after establishment
	fullLengthStallWindow  = 8 * time.Second  // maximum time without playback progress
)

const (
	// OutcomeFullLength means playback reached buffered media beyond the cap.
	OutcomeFullLength = "full-length"
	// OutcomeTargetNotBuffered means the context established but the target was
	// not reached. It does not distinguish a cap from a player error or stall.
	OutcomeTargetNotBuffered = "target-not-buffered"
	// OutcomeNotEstablished means the player context failed before the seek.
	OutcomeNotEstablished = "not-established"
	// OutcomeVideoTooShort means the video has no suitable target beyond the cap.
	OutcomeVideoTooShort = "video-too-short"
)

// playerSeekJS seeks past the preview cap and allows the player to request media at
// the target immediately.
const playerSeekJS = `(seconds) => {
	try {
		const p = document.getElementById('movie_player');
		if (!p || !p.seekTo) return false;
		p.seekTo(seconds, true);
		return true;
	} catch (e) { return false; }
}`

// playerBufferedJS reports playback and buffering at the seek target. It also
// returns the player state, player error, and current serverAbrStreamingUrl for
// diagnostics.
const playerBufferedJS = `(target, tol) => {
	try {
		const p = document.getElementById('movie_player');
		const v = document.querySelector('video');
		const cur = (p && p.getCurrentTime) ? Number(p.getCurrentTime() || 0) : (v ? Number(v.currentTime || 0) : 0);
		let bufEnd = 0, coversTarget = false;
		if (v && v.buffered && v.buffered.length) {
			for (let i = 0; i < v.buffered.length; i++) {
				const st = v.buffered.start(i), en = v.buffered.end(i);
				if (en > bufEnd) bufEnd = en;
				if (st <= target && en >= target + tol) coversTarget = true;
			}
		}
		let state = -2; try { if (p && p.getPlayerState) state = Number(p.getPlayerState()); } catch (e) {}
		let perr = 0; try { if (p && p.getPlayerError) perr = Number(p.getPlayerError() || 0); } catch (e) {}
		let abr = ''; try { const j = (p && p.getPlayerResponse) ? p.getPlayerResponse() : null; abr = (j && j.streamingData && j.streamingData.serverAbrStreamingUrl) || ''; } catch (e) {}
		return JSON.stringify({ current: cur, buffered_end: bufEnd, covers_target: coversTarget, state: state, player_error: perr, abr_url: abr });
	} catch (e) {
		return JSON.stringify({ error: String(e) });
	}
}`

// FullLengthProbe records whether playback reached buffered media beyond the
// status-2 preview cap. A negative result is diagnostic only and does not prove
// that the stream was status-2 capped.
type FullLengthProbe struct {
	Outcome           string  `json:"outcome"`     // one of the Outcome* constants
	FullLength        bool    `json:"full_length"` // true when Outcome is OutcomeFullLength
	Reason            string  `json:"reason"`      // human-readable diagnostic
	VideoID           string  `json:"video_id"`
	LengthSeconds     int     `json:"length_seconds"`      // duration reported by the established context
	TargetSeconds     int     `json:"target_seconds"`      // seek target
	CurrentSeconds    float64 `json:"current_seconds"`     // last observed playback position
	BufferedEnd       float64 `json:"buffered_end"`        // furthest observed buffered position
	ElapsedMs         int     `json:"elapsed_ms"`          // elapsed wall-clock time
	PlayerState       int     `json:"player_state"`        // last getPlayerState result
	ContextURLChanged bool    `json:"context_url_changed"` // whether serverAbrStreamingUrl changed during the probe
}

// VerifyFullLength checks whether the attested browser can stream beyond the
// roughly 70-second status-2 preview cap. It establishes a player context, seeks
// beyond the cap, and requires playback progress and buffered media at the target.
//
// A negative result does not prove that status-2 caused the failure. Reason
// records the observed establishment failure, player error, stall, or timeout.
// The returned error is non-nil only when the caller's context is canceled.
//
// The probe seeks and drives playback, so it should be run on demand rather than
// as a frequent health check.
func (s *Session) VerifyFullLength(ctx context.Context, videoID string) (FullLengthProbe, error) {
	page := s.page.Context(ctx)
	defer s.revertPlayerContext(ctx)

	start := time.Now()
	probe := FullLengthProbe{VideoID: videoID, TargetSeconds: fullLengthTargetSecs}
	finish := func() { probe.ElapsedMs = int(time.Since(start).Milliseconds()) }

	raw, err := s.establish(ctx, page, videoID, time.Now().Add(playerContextTimeout))
	if err != nil {
		// Establishment failures are reported as probe outcomes unless the caller
		// canceled the operation.
		probe.Outcome = OutcomeNotEstablished
		probe.Reason = err.Error()
		finish()
		if ctx.Err() != nil {
			return probe, ctx.Err()
		}
		return probe, nil
	}
	probe.LengthSeconds = raw.LengthSeconds
	establishedURL := raw.ServerAbrStreamingURL

	if raw.LengthSeconds > 0 && raw.LengthSeconds <= fullLengthMinVideoSecs {
		probe.Outcome = OutcomeVideoTooShort
		probe.Reason = fmt.Sprintf("video duration is %ds; probing requires more than %ds", raw.LengthSeconds, fullLengthMinVideoSecs)
		finish()
		return probe, nil
	}

	if seeked, serr := page.Eval(playerSeekJS, fullLengthTargetSecs); serr != nil || !seeked.Value.Bool() {
		probe.Outcome = OutcomeTargetNotBuffered
		if serr != nil {
			probe.Reason = "seek past the cap failed: " + serr.Error()
		} else {
			probe.Reason = "movie_player.seekTo unavailable"
		}
		finish()
		return probe, nil
	}

	budgetDeadline := time.Now().Add(fullLengthProbeBudget)
	lastProgressAt := time.Now()
	var lastCurrent float64
	for {
		select {
		case <-ctx.Done():
			finish()
			return probe, ctx.Err()
		case <-time.After(playerContextPollInterval):
		}
		_, _ = page.Eval(playerDriveJS)
		obj, evalErr := page.Eval(playerBufferedJS, fullLengthTargetSecs, fullLengthTolSecs)
		if evalErr != nil {
			if time.Now().After(budgetDeadline) {
				probe.Outcome = OutcomeTargetNotBuffered
				probe.Reason = "buffered probe eval error: " + evalErr.Error()
				finish()
				return probe, nil
			}
			continue
		}
		var b struct {
			Current      float64 `json:"current"`
			BufferedEnd  float64 `json:"buffered_end"`
			CoversTarget bool    `json:"covers_target"`
			State        int     `json:"state"`
			PlayerError  int     `json:"player_error"`
			ABRURL       string  `json:"abr_url"`
			Error        string  `json:"error"`
		}
		if jerr := json.Unmarshal([]byte(obj.Value.Str()), &b); jerr != nil {
			// A malformed payload may be transient, but it must not extend the
			// configured probe budget.
			if time.Now().After(budgetDeadline) {
				probe.Outcome = OutcomeTargetNotBuffered
				probe.Reason = "buffered probe decode error: " + jerr.Error()
				finish()
				return probe, nil
			}
			continue
		}
		probe.CurrentSeconds = b.Current
		probe.BufferedEnd = b.BufferedEnd
		probe.PlayerState = b.State
		if b.ABRURL != "" && b.ABRURL != establishedURL {
			probe.ContextURLChanged = true
		}

		// Require both playback progress and buffered media at the target.
		if b.Current > float64(fullLengthTargetSecs)+fullLengthTolSecs && b.CoversTarget {
			probe.Outcome = OutcomeFullLength
			probe.FullLength = true
			probe.Reason = fmt.Sprintf("advanced to %.1fs and buffered past the %ds cap", b.Current, fullLengthTargetSecs)
			finish()
			return probe, nil
		}
		// A terminal player error will not recover within the probe budget.
		if b.PlayerError != 0 {
			probe.Outcome = OutcomeTargetNotBuffered
			probe.Reason = fmt.Sprintf("player error %d (state %d) at %.1fs before reaching the target", b.PlayerError, b.State, b.Current)
			finish()
			return probe, nil
		}
		// Use playback progress rather than player state to identify a stall because
		// transient buffering is normal.
		if b.Current > lastCurrent+0.25 {
			lastCurrent = b.Current
			lastProgressAt = time.Now()
		} else if time.Since(lastProgressAt) > fullLengthStallWindow {
			probe.Outcome = OutcomeTargetNotBuffered
			probe.Reason = fmt.Sprintf("playback stalled at %.1fs (state %d, buffered end %.1f); never reached the %ds target", b.Current, b.State, b.BufferedEnd, fullLengthTargetSecs)
			finish()
			return probe, nil
		}
		if time.Now().After(budgetDeadline) {
			probe.Outcome = OutcomeTargetNotBuffered
			probe.Reason = fmt.Sprintf("budget expired at %.1fs/%ds target (state %d, buffered end %.1f)", b.Current, fullLengthTargetSecs, b.State, b.BufferedEnd)
			finish()
			return probe, nil
		}
	}
}

// AttestKind reports "integrity" or "fallback" after Attest or Mint. It returns
// an empty string before attestation.
func (s *Session) AttestKind() string { return s.attestKind }

// Close releases the browser created by Launch or the context created by
// Pool.NewSession. It is idempotent.
func (s *Session) Close() {
	if s == nil || s.dispose == nil {
		return
	}
	d := s.dispose
	s.dispose = nil
	d()
}

// DetectChrome resolves a Chromium binary from WAXSEAL_CHROME_BIN or common
// system paths.
func DetectChrome() (string, error) {
	if b := os.Getenv("WAXSEAL_CHROME_BIN"); b != "" {
		return b, nil
	}
	for _, p := range []string{
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/snap/bin/chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("waxseal: no Chromium found; set WAXSEAL_CHROME_BIN")
}

// homeTmpBase returns a $HOME-rooted base dir for the user-data-dir, because
// snap-confined Chromium cannot open a profile under /tmp.
func homeTmpBase() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.TempDir()
}
