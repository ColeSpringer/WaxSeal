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
	"sync"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/cdp"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/innertube"
)

// DefaultVideo is the landing video used to capture the browser identity. It is
// Blender's Creative Commons movie "Big Buck Bunny."
const DefaultVideo = "aqz-KE-bpKQ"

// playerContextTimeout bounds how long PlayerContext waits for the player to load
// a video and expose its status-1 getPlayerResponse result.
const playerContextTimeout = 25 * time.Second

// playerContextPollInterval paces the player-context polling loops.
const playerContextPollInterval = 300 * time.Millisecond

// Pool recovery timings. The liveness timeout allows for a busy host, while the
// capped relaunch backoff limits process creation during a crash loop.
const (
	aliveProbeTimeout   = 5 * time.Second
	relaunchBackoffBase = 10 * time.Second
	relaunchBackoffMax  = 60 * time.Second
	teardownTimeout     = 5 * time.Second
	// relaunchStableWindow must exceed relaunchBackoffMax so waiting through the
	// maximum backoff does not reset the streak during a crash loop.
	relaunchStableWindow = 2 * relaunchBackoffMax
)

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

// ErrStatus2Unconfirmed reports that WaxSeal could not confirm the requested
// streaming context past the status-2 preview cap before its deadline. The cap is
// currently about 70 seconds. These failures are session-local and usually timing
// related under load, so the minter retries in place and never negative-caches
// the video.
var ErrStatus2Unconfirmed = errors.New("waxseal: status-1 streaming not confirmed before deadline")

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
	browser *cdp.Browser
	page    *cdp.Page
	dispose func() // closes the browser from Launch or the context from Pool
	id      Identity
	client  *httpx.Client // egresses with the browser's cookies
	log     *slog.Logger

	// landingVideo is the watch video used to initialize the session. Establishment
	// falls back to DefaultVideo when this video is too short for the proof.
	landingVideo string

	// probeMu guards proof state shared by playback, health, and metrics paths.
	probeMu              sync.Mutex
	establishedStreaming bool
	lastProbe            FullLengthProbe // most recent proveFullLength result
	lastProbeAt          time.Time       // when lastProbe completed; zero means never probed

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
//
// Launch does not validate videoID. Callers that accept user input should check it
// with ValidVideoID before calling Launch.
func Launch(ctx context.Context, videoID string, opts Options) (*Session, error) {
	opts = withDefaults(opts)
	browser, profile, err := launchChromium(opts)
	if err != nil {
		return nil, err
	}
	teardown := func() {
		_ = browser.Close()
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

// launchChromium starts Chromium over a CDP pipe. The caller must close the
// returned browser and remove the profile directory.
func launchChromium(opts Options) (*cdp.Browser, string, error) {
	bin := opts.ChromeBin
	if bin == "" {
		b, err := DetectChrome()
		if err != nil {
			return nil, "", err
		}
		bin = b
	}
	opts.Logger.Info("waxseal: launching chromium", "bin", bin, "headful", opts.Headful)

	// Snap-confined Chromium can only write a user-data-dir under $HOME, not /tmp.
	profileDir, err := os.MkdirTemp(homeTmpBase(), profilePrefix)
	if err != nil {
		return nil, "", fmt.Errorf("waxseal: temp profile: %w", err)
	}
	// The startup reaper only removes marked profiles whose ownership lock is free.
	markProfileDir(profileDir)

	// Keep the launch argv byte-compatible with the previous CDP driver wherever
	// flags affect Chromium's fingerprint or process model. BuildArgs is pinned by
	// a golden in internal/cdp. It also omits enable-automation so
	// navigator.webdriver stays false. The pipe transport gives startup a
	// cancellable version handshake owned by this process.
	browser, err := cdp.Spawn(context.Background(), bin, cdp.BuildArgs(profileDir, opts.Headful), cdp.SpawnOptions{
		LaunchTimeout: launchTimeout,
		Logger:        opts.Logger,
	})
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return nil, "", fmt.Errorf("waxseal: launch chromium: %w", err)
	}
	return browser, profileDir, nil
}

// launchTimeout limits how long startup waits for Chromium's version handshake.
const launchTimeout = 60 * time.Second

// setupSession parks a page in browser (the main browser, or an incognito context
// for a tenant), navigates to videoID's watch page, captures the identity, injects
// the bundle, and builds the HTTP client. dispose is left for the caller to set.
// On error the caller is responsible for teardown.
func setupSession(ctx context.Context, browser *cdp.Browser, videoID string, opts Options) (_ *Session, err error) {
	s := &Session{browser: browser, log: opts.Logger, landingVideo: videoID}

	// Bind page creation (createTarget/attachToTarget/Page.enable) to the caller's
	// ctx so an alive-but-unresponsive Chromium cannot hang setup past the deadline.
	page, err := browser.Context(ctx).Page(cdp.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("waxseal: new page: %w", err)
	}
	s.page = page.Context(ctx)

	// Bypass CSP so the injected bundle's new Function(interpreter) can run on the
	// youtube.com origin (which otherwise forbids unsafe-eval).
	if err = s.page.SetBypassCSP(true); err != nil {
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

// errPoolClosed is returned when a pool operation runs after Close.
var errPoolClosed = errors.New("waxseal: browser pool is closed")

// browserInstance groups a Chromium connection with the resources that must be
// released with it. Pool relaunches replace the entire instance.
type browserInstance struct {
	browser      *cdp.Browser
	profile      string
	onTeardown   func()    // test hook; nil in production
	teardownOnce sync.Once // teardown runs at most once even if Close races a relaunch
}

// teardown closes the browser (group-killing the process) and removes the profile.
// The bounded browser close prevents a stalled CDP connection from blocking
// recovery. teardown is idempotent and accepts partially initialized instances.
func (i *browserInstance) teardown() {
	if i == nil {
		return
	}
	i.teardownOnce.Do(func() {
		if i.browser != nil {
			tctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
			_ = i.browser.Context(tctx).Close()
			cancel()
		}
		if i.profile != "" {
			_ = os.RemoveAll(i.profile)
		}
		if i.onTeardown != nil {
			i.onTeardown()
		}
	})
}

// launchInstance launches Chromium and groups its resources for teardown.
func launchInstance(opts Options) (*browserInstance, error) {
	b, profile, err := launchChromium(opts)
	if err != nil {
		return nil, err
	}
	return &browserInstance{browser: b, profile: profile}, nil
}

// Pool owns one Chromium and creates isolated incognito-context Sessions. Each
// session has its own guest identity, cookies, and storage. All sessions share
// the browser's egress IP.
//
// If Chromium dies, the next NewSession attempts to relaunch it. Concurrent
// callers share one relaunch, and repeated relaunches are subject to a capped
// backoff.
type Pool struct {
	opts Options

	// Tests replace newInstance to exercise recovery without launching Chromium.
	newInstance func() (*browserInstance, error)

	mu             sync.Mutex
	cur            *browserInstance
	closed         bool
	relaunching    chan struct{} // non-nil while a relaunch is in progress
	lastRelaunchAt time.Time     // start time of the last relaunch attempt
	relaunchStreak int           // consecutive relaunches within the stability window
}

// LaunchPool starts the shared Chromium. Close it to tear everything down.
func LaunchPool(opts Options) (*Pool, error) {
	opts = withDefaults(opts)
	p := &Pool{opts: opts, newInstance: func() (*browserInstance, error) { return launchInstance(opts) }}
	inst, err := p.newInstance()
	if err != nil {
		return nil, err
	}
	p.cur = inst
	return p, nil
}

// acquire returns the current instance unless the pool is closed.
func (p *Pool) acquire() (*browserInstance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.cur == nil {
		return nil, errPoolClosed
	}
	return p.cur, nil
}

// NewSession creates a fresh isolated browser context and parks a Session in it.
// Closing the Session disposes its context without closing the shared browser. If
// the shared Chromium has died, NewSession relaunches it once and retries.
func (p *Pool) NewSession(ctx context.Context, videoID string) (*Session, error) {
	inst, err := p.acquire()
	if err != nil {
		return nil, err
	}
	// Bind the context creation to the caller's ctx so an unresponsive browser
	// cannot hang the request past its deadline.
	incog, err := inst.browser.Context(ctx).Incognito()
	if err != nil {
		// Relaunch only when the context error came from a dead browser.
		if browserAlive(inst.browser) {
			return nil, fmt.Errorf("waxseal: new browser context: %w", err)
		}
		p.opts.Logger.Warn("waxseal: pooled chromium is unreachable; relaunching", "err", err)
		inst, err = p.relaunch(inst)
		if err != nil {
			return nil, err
		}
		if incog, err = inst.browser.Context(ctx).Incognito(); err != nil {
			return nil, fmt.Errorf("waxseal: new browser context after relaunch: %w", err)
		}
	}
	// Incognito copies the browser connection by value. Closing this copy disposes
	// the original context without affecting a replacement pool instance. The
	// dispose is bounded so an unresponsive browser cannot block session teardown.
	dispose := func() {
		dctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
		defer cancel()
		_ = incog.Context(dctx).Close()
	}
	s, err := setupSession(ctx, incog, videoID, p.opts)
	if err != nil {
		dispose()
		return nil, err
	}
	s.dispose = dispose
	return s, nil
}

// browserAlive reports whether the browser answers a CDP Version request within
// aliveProbeTimeout.
func browserAlive(b *cdp.Browser) bool {
	actx, cancel := context.WithTimeout(context.Background(), aliveProbeTimeout)
	defer cancel()
	_, err := b.Context(actx).Version()
	return err == nil
}

// relaunch replaces stale with a new browser instance. Concurrent callers that
// observed the same stale instance wait for the same relaunch.
//
// Attempts are counted before launch so the backoff also covers browsers that
// start successfully and die during session setup. The streak resets only after
// relaunchStableWindow without another relaunch.
func (p *Pool) relaunch(stale *browserInstance) (*browserInstance, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errPoolClosed
		}
		if p.cur != stale {
			cur := p.cur // Another caller replaced the stale instance.
			p.mu.Unlock()
			return cur, nil
		}
		if p.relaunching != nil {
			ch := p.relaunching
			p.mu.Unlock()
			<-ch // Wait for the in-progress relaunch, then check again.
			continue
		}
		now := time.Now()
		// A browser that survives the stability window starts a new backoff streak.
		if !p.lastRelaunchAt.IsZero() && now.Sub(p.lastRelaunchAt) >= relaunchStableWindow {
			p.relaunchStreak = 0
		}
		if p.relaunchStreak > 0 {
			if wait := p.lastRelaunchAt.Add(p.backoffWindow()).Sub(now); wait > 0 {
				streak := p.relaunchStreak
				p.mu.Unlock()
				return nil, fmt.Errorf("waxseal: pooled chromium relaunch backing off %s after %d consecutive relaunches", wait.Round(time.Second), streak)
			}
		}
		// Count the attempt before launching so an immediate post-launch crash
		// increases the next backoff.
		p.relaunchStreak++
		p.lastRelaunchAt = now
		ch := make(chan struct{})
		p.relaunching = ch
		p.mu.Unlock()

		stale.teardown()
		inst, lerr := p.newInstance()

		p.mu.Lock()
		p.relaunching = nil
		close(ch)
		if lerr != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("waxseal: relaunch chromium: %w", lerr)
		}
		if p.closed {
			p.mu.Unlock()
			inst.teardown() // Close won the race; discard the replacement.
			return nil, errPoolClosed
		}
		p.cur = inst
		p.mu.Unlock()
		return inst, nil
	}
}

// CurrentBrowserPID returns the process ID of the current Chromium process, or 0
// if none is available. cdp owns the Chromium process directly, so this is its
// PID; cdp.Browser.PID guards a nil browser as 0.
func (p *Pool) CurrentBrowserPID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil {
		return 0
	}
	return p.cur.browser.PID()
}

// backoffWindow returns the capped exponential delay for relaunchStreak.
func (p *Pool) backoffWindow() time.Duration {
	backoff := relaunchBackoffBase
	for i := 1; i < p.relaunchStreak; i++ {
		if backoff *= 2; backoff >= relaunchBackoffMax {
			return relaunchBackoffMax
		}
	}
	return backoff
}

// Close tears down the shared browser and removes its temporary profile. A
// concurrent relaunch may finish, but its replacement is discarded.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	inst := p.cur
	p.cur = nil
	p.mu.Unlock()
	inst.teardown()
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
			if jerr := json.Unmarshal([]byte(obj.Str()), &raw); jerr == nil && raw.VD != "" {
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

// chromeMajorRE extracts the Chrome major version from a user agent.
var chromeMajorRE = regexp.MustCompile(`Chrome/(\d+)`)

// uaOverride builds the Network.setUserAgentOverride request for realUA. It
// removes HeadlessChrome, derives UA-CH from the observed Chrome major version,
// and falls back only for malformed input. TestUAOverride pins the exact JSON
// shape used on the wire.
func uaOverride(realUA string) *cdp.NetworkSetUserAgentOverride {
	fixed := strings.Replace(realUA, "HeadlessChrome", "Chrome", 1)
	major := "149"
	if m := chromeMajorRE.FindStringSubmatch(fixed); m != nil {
		major = m[1]
	}
	full := major + ".0.0.0"
	return &cdp.NetworkSetUserAgentOverride{
		UserAgent: fixed,
		UserAgentMetadata: &cdp.UserAgentMetadata{
			Brands: []*cdp.UserAgentBrandVersion{
				{Brand: "Chromium", Version: major},
				{Brand: "Not)A;Brand", Version: "24"},
			},
			FullVersionList: []*cdp.UserAgentBrandVersion{
				{Brand: "Chromium", Version: full},
				{Brand: "Not)A;Brand", Version: "24.0.0.0"},
			},
			Platform:        "Linux",
			PlatformVersion: "",
			Architecture:    "x86",
			Bitness:         "64",
			Mobile:          false,
			FullVersion:     full,
		},
	}
}

// normalizeUA removes the HeadlessChrome marker from navigator.userAgent before
// navigation and keeps UA-CH consistent. No other fingerprint values are changed.
func (s *Session) normalizeUA(ctx context.Context) error {
	obj, err := s.page.Context(ctx).Eval(`() => navigator.userAgent`)
	if err != nil {
		return fmt.Errorf("waxseal: read ua for normalize: %w", err)
	}
	override := uaOverride(obj.Str())
	if err := s.page.Context(ctx).SetUserAgentOverride(override); err != nil {
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
	s.id.STS = obj.Int()
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
// adopt the same guest session. ctx bounds the CDP read.
//
// It reads the browser-level cookie store because page-level reads can be empty
// after the page leaves youtube.com.
func (s *Session) BrowserCookies(ctx context.Context) ([]*http.Cookie, error) {
	cs, err := s.browser.Context(ctx).GetCookies()
	if err != nil {
		return nil, fmt.Errorf("waxseal: read browser cookies: %w", err)
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
	return out, nil
}

// WaitCrash blocks until the session's browser target crashes or detaches, the
// CDP connection is lost, or ctx is cancelled. It returns a diagnostic reason, or
// "" on cancellation. A session with no live page waits for ctx cancellation so
// callers can use the same cleanup path.
func (s *Session) WaitCrash(ctx context.Context) string {
	if s.page == nil {
		<-ctx.Done()
		return ""
	}
	return s.page.WaitCrash(ctx)
}

// Ping checks whether the session page answers a CDP request. It does not mint or
// establish the session.
func (s *Session) Ping(ctx context.Context) error {
	_, err := s.page.Context(ctx).Eval(`() => true`)
	return err
}

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
	botguardResponse := obj.Str()
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
	token := mintObj.Str()
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
	// PlayabilityStatus is playabilityStatus.status, which is "OK" when the video
	// is streamable. It is distinct from the SABR status-1 protection code embedded
	// in ServerAbrStreamingURL.
	PlayabilityStatus            string        `json:"playability_status"`
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
			playability_status: status,
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
			playability_status: status,
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

// playerContextCleanupJS stops playback to release buffered media and restores
// the page's native visibility state before the shared page is reused. The
// fallback definitions keep the page hidden if an own-property accessor cannot be
// deleted.
const playerContextCleanupJS = `() => {
	try { const p = document.getElementById('movie_player'); if (p && p.stopVideo) p.stopVideo(); } catch (e) {}
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
	if raw.PlayabilityStatus != "" && raw.PlayabilityStatus != "OK" && raw.VideoIDMatch {
		return &UnplayableError{Status: raw.PlayabilityStatus, Detail: raw.Reason}, true
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
// A terminal playabilityStatus returns ErrUnplayable. If confirmation cannot
// clear the status-2 preview cap before the deadline, PlayerContext returns
// ErrStatus2Unconfirmed instead of a possibly capped URL. Playback and visibility
// changes are reverted before the shared page is reused.
func (s *Session) PlayerContext(ctx context.Context, videoID string) (PlayerContext, error) {
	// A cold status-2 stream can buffer within the preview window. Seek past the
	// cap before returning a context claimed to support full-length streaming.
	if err := s.EnsureEstablished(ctx); err != nil {
		return PlayerContext{}, err
	}

	page := s.page.Context(ctx)
	defer s.revertPlayerContext(ctx)

	// Establish the requested video and prove its stream crosses the status-2
	// preview cap before returning the context.
	raw, err := s.establishStatus1(ctx, page, videoID, playerContextTimeout, playerContextConfirmBudget)
	if err != nil {
		return PlayerContext{}, err
	}
	if raw.ServerAbrStreamingURL == "" {
		return PlayerContext{}, fmt.Errorf("waxseal: player-context: no serverAbrStreamingUrl (playabilityStatus %q)", raw.PlayabilityStatus)
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
func (s *Session) establish(ctx context.Context, page *cdp.Page, videoID string, deadline time.Time) (playerContextRaw, error) {
	// Bind page Evals to the establish deadline. The loops below check the
	// deadline only between Evals; without this, one Eval retrying after context
	// loss could exceed the deadline while holding the tenant's mintMu.
	ectx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	page = page.Context(ectx)
	// Phase 1: wait for the player API to hydrate, then point it at videoID once.
	for {
		ready, err := page.Eval(playerReadyJS)
		if err != nil {
			return playerContextRaw{}, fmt.Errorf("waxseal: player-context ready probe: %w", err)
		}
		if ready.Bool() {
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
	if !loaded.Bool() {
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
		if err := json.Unmarshal([]byte(obj.Str()), &raw); err != nil {
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
	// fullLengthHardTimeout bounds the entire proof when the caller has no deadline.
	fullLengthHardTimeout = 60 * time.Second
)

// Per-request status-1 confirmation uses a tighter budget than the session proof
// because it runs before returning the requested video's context.
const (
	// previewCapSecs is the documented status-2 "attestation pending" preview
	// cap. A video no longer than this is fully covered by the preview window.
	previewCapSecs = 70
	// verifyEndTol leaves room for fullLengthTolSecs when seeking near the end.
	// It is fullLengthTolSecs + 1, rounded to a whole second.
	verifyEndTol = 3
	// residualEndTol is the end tolerance for short over-cap videos. It is tight
	// enough that a stream stopped near previewCapSecs cannot pass as full-length.
	residualEndTol = 0.5
	// playerContextConfirmBudget bounds the per-request seek-and-confirm. It is
	// shorter than the session-proof budget because status-2 usually clears within
	// a few seconds; the remaining room covers CPU contention.
	playerContextConfirmBudget = 12 * time.Second
	// playerContextReReadBudget is a separate post-confirm window, so a slow
	// confirm cannot leave the re-read with no time. Healthy re-reads are
	// immediate; the budget covers occasional CDP delays.
	playerContextReReadBudget = 4 * time.Second
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
	// OutcomeCanceled means polling stopped before the probe reached a verdict.
	OutcomeCanceled = "canceled"
	// OutcomeConfirmUnavailable means the confirm could not start, for example
	// because seekTo was unavailable. That points to a wedged page rather than a
	// status-2 timing condition.
	OutcomeConfirmUnavailable = "confirm-unavailable"
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

// proofCandidates lists fallback videos for full-length playback checks.
// Candidates are ordered by preference.
var proofCandidates = []string{
	DefaultVideo,  // Big Buck Bunny
	"R6MlUcmOul8", // Tears of Steel
	"1UaBgr_sq9A", // NASA
}

// establishFromCandidates tries candidates until one proves full-length
// playback. It retries only when a candidate is unavailable or too short. Any
// other outcome or error is returned immediately because it reflects session
// health rather than candidate suitability. Empty and duplicate IDs are ignored.
//
// When all candidates are exhausted, the returned error records each retryable
// failure without wrapping ErrUnplayable. These videos are internal probes, not
// the video requested by the caller.
func establishFromCandidates(ctx context.Context, prove func(string) (FullLengthProbe, error), candidates []string, log *slog.Logger) error {
	var failures []string
	seen := make(map[string]bool, len(candidates))
	for _, videoID := range candidates {
		// Do not start another proof after cancellation.
		if err := ctx.Err(); err != nil {
			return err
		}
		if videoID == "" || seen[videoID] {
			continue
		}
		seen[videoID] = true
		probe, err := prove(videoID)
		switch {
		case err == nil && probe.Outcome == OutcomeFullLength:
			return nil
		case errors.Is(err, ErrUnplayable):
			failures = append(failures, fmt.Sprintf("%s: %v", videoID, err))
			log.Info("waxseal: proof video unplayable; trying the next candidate", "video", videoID, "err", err)
		case err == nil && probe.Outcome == OutcomeVideoTooShort:
			failures = append(failures, fmt.Sprintf("%s: too short (%s)", videoID, probe.Reason))
			log.Info("waxseal: proof video too short; trying the next candidate", "video", videoID, "reason", probe.Reason)
		case err != nil:
			// Errors other than ErrUnplayable are not candidate-specific.
			return err
		default:
			// A non-retryable outcome reflects session health. Trying another video
			// could hide it.
			return fmt.Errorf("waxseal: session not established: full-length proof outcome %q: %s", probe.Outcome, probe.Reason)
		}
	}
	if len(failures) > 0 {
		// Keep the aggregate text-only so errors.Is does not classify the caller's
		// requested video as unplayable.
		return fmt.Errorf("waxseal: session not established: no usable proof video after trying %d candidates: %s", len(seen), strings.Join(failures, "; "))
	}
	return fmt.Errorf("waxseal: session not established: no proof candidates")
}

// EnsureEstablished proves once per session that playback can advance beyond the
// status-2 preview cap.
//
// The proof uses the landing video first, then tries proofCandidates when a video
// is unavailable or too short. Successful establishment applies to later videos
// requested through the same session.
//
// proveFullLength restores the shared page before returning.
func (s *Session) EnsureEstablished(ctx context.Context) error {
	if s.Established() {
		return nil
	}
	candidates := append([]string{s.landingVideo}, proofCandidates...)
	prove := func(videoID string) (FullLengthProbe, error) { return s.proveFullLength(ctx, videoID) }
	if err := establishFromCandidates(ctx, prove, candidates, s.log); err != nil {
		return err
	}
	s.probeMu.Lock()
	s.establishedStreaming = true
	s.probeMu.Unlock()
	return nil
}

// Established reports whether the once-per-session full-length proof has passed.
// It reflects the browser's own proof of playback, not the health of a URL handed
// to and fetched by a consumer.
func (s *Session) Established() bool {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	return s.establishedStreaming
}

// LastProof returns the most recent full-length probe and its completion time. A
// zero time means the session has never been probed.
func (s *Session) LastProof() (FullLengthProbe, time.Time) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	return s.lastProbe, s.lastProbeAt
}

// VerifyFullLength checks whether the attested browser can stream beyond the
// roughly 70-second status-2 preview cap.
func (s *Session) VerifyFullLength(ctx context.Context, videoID string) (FullLengthProbe, error) {
	return s.proveFullLength(ctx, videoID)
}

// proveFullLength establishes a player context, seeks beyond the cap, and
// requires playback progress and buffered media at the target.
//
// A negative result does not prove that status-2 caused the failure. Reason
// records the observed establishment failure, player error, stall, or timeout.
// The returned error is non-nil when the context is canceled, the hard timeout
// expires, or the video has a terminal playability status.
//
// The probe seeks and drives playback, so it should be run on demand rather than
// as a frequent health check.
func (s *Session) proveFullLength(ctx context.Context, videoID string) (FullLengthProbe, error) {
	ctx, cancelHard := context.WithTimeout(ctx, fullLengthHardTimeout)
	defer cancelHard()
	page := s.page.Context(ctx)
	defer s.revertPlayerContext(ctx)

	start := time.Now()
	probe := FullLengthProbe{VideoID: videoID, TargetSeconds: fullLengthTargetSecs}
	finish := func() {
		probe.ElapsedMs = int(time.Since(start).Milliseconds())
		s.probeMu.Lock()
		s.lastProbe = probe
		s.lastProbeAt = time.Now()
		s.probeMu.Unlock()
	}

	raw, err := s.establish(ctx, page, videoID, time.Now().Add(playerContextTimeout))
	if err != nil {
		// Establishment failures are reported as probe outcomes unless the caller
		// canceled the operation or the video is terminally unplayable.
		probe.Outcome = OutcomeNotEstablished
		probe.Reason = err.Error()
		finish()
		if ctx.Err() != nil {
			return probe, ctx.Err()
		}
		// A terminal playability status describes the video, not the session.
		// Return it so callers can select another proof candidate or report that the
		// requested video is unavailable.
		if errors.Is(err, ErrUnplayable) {
			return probe, err
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

	// The session proof still requires playback progress beyond the target and a
	// buffer range that covers it.
	cp, cerr := s.confirmPastCap(ctx, page, fullLengthTargetSecs, fullLengthTolSecs, establishedURL, time.Now().Add(fullLengthProbeBudget),
		func(b bufferedSample) bool {
			return b.Current > float64(fullLengthTargetSecs)+fullLengthTolSecs && b.CoversTarget
		})
	cp.VideoID = videoID
	cp.LengthSeconds = raw.LengthSeconds
	probe = cp
	finish()
	return probe, cerr
}

// bufferedSample is one observation of playback and buffering at the seek target,
// decoded from playerBufferedJS.
type bufferedSample struct {
	Current      float64 `json:"current"`
	BufferedEnd  float64 `json:"buffered_end"`
	CoversTarget bool    `json:"covers_target"`
	State        int     `json:"state"`
	PlayerError  int     `json:"player_error"`
	ABRURL       string  `json:"abr_url"`
	Error        string  `json:"error"`
}

// confirmPastCap seeks to target and drives muted playback until confirmed accepts
// a buffered sample, the player reaches a terminal error or stall, or deadline
// expires. It is shared by the session proof and the per-request gate and does not
// update LastProof on its own. establishedURL is used only to record whether the
// player switched SABR URLs during the probe.
//
// A failed seek returns OutcomeConfirmUnavailable because the confirm never ran.
// Only request cancellation is returned as an error; other negative outcomes are
// encoded in the probe.
func (s *Session) confirmPastCap(ctx context.Context, page *cdp.Page, target int, tol float64, establishedURL string, deadline time.Time, confirmed func(bufferedSample) bool) (FullLengthProbe, error) {
	// Scope CDP evals to the confirm deadline. The outer ctx still drives
	// cancellation reporting, keeping caller cancellation distinct from timeout.
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	page = page.Context(dctx)

	probe := FullLengthProbe{TargetSeconds: target}
	if seeked, serr := page.Eval(playerSeekJS, target); serr != nil || !seeked.Bool() {
		if ctx.Err() != nil {
			probe.Outcome = OutcomeCanceled
			probe.Reason = "confirm canceled during seek: " + ctx.Err().Error()
			return probe, ctx.Err()
		}
		// A failed seek means the page could not run the confirm. Let the caller
		// relaunch this session instead of classifying it as a capped stream.
		probe.Outcome = OutcomeConfirmUnavailable
		if serr != nil {
			probe.Reason = "seek past the cap failed: " + serr.Error()
		} else {
			probe.Reason = "movie_player.seekTo unavailable"
		}
		return probe, nil
	}

	lastProgressAt := time.Now()
	var lastCurrent, lastBuffered float64
	for {
		select {
		case <-ctx.Done():
			// Distinguish a canceled probe from a session that was never probed.
			probe.Outcome = OutcomeCanceled
			probe.Reason = "probe canceled before reaching the target: " + ctx.Err().Error()
			return probe, ctx.Err()
		case <-time.After(playerContextPollInterval):
		}
		// Check the deadline before the next eval so a budget timeout reports the
		// last observed buffer position instead of a deadline-canceled CDP error.
		if time.Now().After(deadline) {
			probe.Outcome = OutcomeTargetNotBuffered
			probe.Reason = fmt.Sprintf("budget expired at %.1fs/%ds target (state %d, buffered end %.1f)", probe.CurrentSeconds, target, probe.PlayerState, probe.BufferedEnd)
			return probe, nil
		}
		_, _ = page.Eval(playerDriveJS)
		obj, evalErr := page.Eval(playerBufferedJS, target, tol)
		if evalErr != nil {
			if time.Now().After(deadline) {
				probe.Outcome = OutcomeTargetNotBuffered
				probe.Reason = "buffered probe eval error: " + evalErr.Error()
				return probe, nil
			}
			// Retry transient CDP failures until the deadline. The debug log keeps
			// slow confirms visible under load.
			s.log.Debug("waxseal: confirm-past-cap transient eval error; retrying", "err", evalErr)
			continue
		}
		var b bufferedSample
		if jerr := json.Unmarshal([]byte(obj.Str()), &b); jerr != nil {
			// A malformed payload may be transient, but it must not extend past the
			// deadline.
			if time.Now().After(deadline) {
				probe.Outcome = OutcomeTargetNotBuffered
				probe.Reason = "buffered probe decode error: " + jerr.Error()
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

		if confirmed(b) {
			probe.Outcome = OutcomeFullLength
			probe.FullLength = true
			probe.Reason = fmt.Sprintf("advanced to %.1fs and buffered to %.1fs past the %ds cap", b.Current, b.BufferedEnd, previewCapSecs)
			return probe, nil
		}
		// A terminal player error will not recover within the probe budget.
		if b.PlayerError != 0 {
			probe.Outcome = OutcomeTargetNotBuffered
			probe.Reason = fmt.Sprintf("player error %d (state %d) at %.1fs before reaching the target", b.PlayerError, b.State, b.Current)
			return probe, nil
		}
		// Buffer growth counts as liveness for the per-request gate; a successful
		// confirm can fill the buffer before currentTime advances.
		if b.Current > lastCurrent+0.25 || b.BufferedEnd > lastBuffered+0.25 {
			lastCurrent = b.Current
			lastBuffered = b.BufferedEnd
			lastProgressAt = time.Now()
		} else if time.Since(lastProgressAt) > fullLengthStallWindow {
			probe.Outcome = OutcomeTargetNotBuffered
			probe.Reason = fmt.Sprintf("playback and buffering stalled at %.1fs (state %d, buffered end %.1f); never reached the %ds target", b.Current, b.State, b.BufferedEnd, target)
			return probe, nil
		}
	}
}

// seekTarget chooses the per-request confirm target for a known video length.
// Unknown or invalid lengths use the normal full-length target. Known lengths are
// clamped so target+fullLengthTolSecs stays within the video; targets at or below
// previewCapSecs are handled by the cap-safe and residual bands.
func seekTarget(length int) int {
	if length <= 0 {
		return fullLengthTargetSecs
	}
	if target := length - verifyEndTol; target < fullLengthTargetSecs {
		return target
	}
	return fullLengthTargetSecs
}

// confirmBand selects how establishStatus1 confirms a video's context.
type confirmBand int

const (
	// bandCapSafe means the whole video fits within the preview cap.
	bandCapSafe confirmBand = iota
	// bandVerify means a seek target can clear the cap with tolerance room.
	bandVerify
	// bandResidual means the video is just over the cap, so confirmation must
	// reach the true end.
	bandResidual
)

// classifyBand chooses the confirm path from LengthSeconds. Unknown length
// verifies at the full-length target. Known lengths at or below previewCapSecs are
// cap-safe; the narrow interval above the cap is residual.
func classifyBand(length int) confirmBand {
	if length > 0 && length <= previewCapSecs {
		return bandCapSafe
	}
	if seekTarget(length) > previewCapSecs {
		return bandVerify
	}
	return bandResidual
}

// bufferedReachesEnd accepts a residual-band sample only when buffering reaches
// the true end within residualEndTol. That keeps a stream stopped near
// previewCapSecs from passing for any video longer than the cap.
func bufferedReachesEnd(length int, bufferedEnd float64) bool {
	return bufferedEnd >= float64(length)-residualEndTol
}

// establishStatus1 establishes the requested video and returns its context only
// after the stream is safe from the status-2 preview cap. It returns
// ErrStatus2Unconfirmed when the confirm runs but does not clear the cap before
// confirmBudget expires.
//
// LengthSeconds controls the confirm path:
//   - length <= previewCapSecs: the whole video fits in the preview window.
//   - seekTarget(length) > previewCapSecs: seek past the cap, require buffered
//     media beyond the target, then re-read the transitioned context.
//   - previewCapSecs < length <= previewCapSecs+verifyEndTol: no seek target can
//     clear the cap with tolerance room, so require buffering to reach the true
//     end.
//
// Unknown length verifies at the full-length target. This fails closed for live,
// premiere, or otherwise unreadable durations: long content can still confirm,
// while short unknown-length content is refused and left to the consumer fallback.
func (s *Session) establishStatus1(ctx context.Context, page *cdp.Page, videoID string, establishBudget, confirmBudget time.Duration) (playerContextRaw, error) {
	raw, err := s.establish(ctx, page, videoID, time.Now().Add(establishBudget))
	if err != nil {
		return playerContextRaw{}, err
	}
	length := raw.LengthSeconds
	establishedURL := raw.ServerAbrStreamingURL

	switch classifyBand(length) {
	case bandCapSafe:
		// A preview covers the full video, so the URL cannot truncate.
		return raw, nil
	case bandVerify:
		// Buffering past the target is the status-2 discriminator. Do not require
		// strict playback advance here; under load, the buffer can prove the
		// transition before currentTime moves.
		return s.confirmAndReRead(ctx, page, videoID, seekTarget(length), establishedURL, confirmBudget,
			func(b bufferedSample) bool { return b.CoversTarget })
	default: // bandResidual
		// For the narrow over-cap band, seek at the cap and require the buffer to
		// reach the true end.
		return s.confirmAndReRead(ctx, page, videoID, previewCapSecs, establishedURL, confirmBudget,
			func(b bufferedSample) bool { return bufferedReachesEnd(length, b.BufferedEnd) })
	}
}

// confirmAndReRead runs the per-request confirmation, maps the result to the
// minter recovery class, then returns a fresh context after the player
// transitions. OutcomeConfirmUnavailable becomes a generic error so the minter
// can use its normal relaunch path. A probe that ran but did not clear the cap
// becomes ErrStatus2Unconfirmed, which is retried in place and not relaunched.
//
// The re-read has its own small budget so a slow confirm does not starve a
// healthy post-transition extraction.
func (s *Session) confirmAndReRead(ctx context.Context, page *cdp.Page, videoID string, target int, establishedURL string, confirmBudget time.Duration, confirmed func(bufferedSample) bool) (playerContextRaw, error) {
	cp, cerr := s.confirmPastCap(ctx, page, target, fullLengthTolSecs, establishedURL, time.Now().Add(confirmBudget), confirmed)
	if cerr != nil {
		return playerContextRaw{}, cerr // canceled
	}
	if err := confirmError(cp); err != nil {
		return playerContextRaw{}, err
	}
	return s.reReadContext(ctx, page, videoID, time.Now().Add(playerContextReReadBudget))
}

// confirmError maps a completed confirm probe to the minter recovery class. A
// confirm that could not start is relaunchable session trouble. A confirm that
// ran and failed to clear the cap is ErrStatus2Unconfirmed.
func confirmError(cp FullLengthProbe) error {
	switch {
	case cp.FullLength:
		return nil
	case cp.Outcome == OutcomeConfirmUnavailable:
		return fmt.Errorf("waxseal: player-context: confirm could not run: %s", cp.Reason)
	default:
		return fmt.Errorf("%w: %s", ErrStatus2Unconfirmed, cp.Reason)
	}
}

// reReadContext extracts a fresh player context after status-1 confirmation so
// callers receive the post-transition serverAbrStreamingUrl and format list. The
// video is already loaded and buffered, so a short retry budget is enough for
// transient CDP errors. A terminal playability transition during this window
// returns ErrUnplayable for normal negative caching.
func (s *Session) reReadContext(ctx context.Context, page *cdp.Page, videoID string, deadline time.Time) (playerContextRaw, error) {
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	page = page.Context(dctx)

	evalErrs := 0
	for {
		obj, evalErr := page.Eval(playerContextExtractJS, videoID)
		if evalErr != nil {
			evalErrs++
			if evalErrs >= 3 || time.Now().After(deadline) {
				return playerContextRaw{}, fmt.Errorf("waxseal: player-context re-read: %w", evalErr)
			}
			// A transient CDP error is retried; surface it at debug so a slow re-read is
			// diagnosable under load.
			s.log.Debug("waxseal: player-context re-read transient eval error; retrying", "err", evalErr, "attempt", evalErrs)
		} else {
			var raw playerContextRaw
			if err := json.Unmarshal([]byte(obj.Str()), &raw); err != nil {
				return playerContextRaw{}, fmt.Errorf("waxseal: player-context re-read parse: %w", err)
			}
			// A video that went terminal between confirm and re-read must surface as
			// ErrUnplayable, not a generic re-read failure, so it is negative-cached.
			if ue, ok := confirmTerminal(raw, videoID); ok {
				return playerContextRaw{}, ue
			}
			if raw.Error == "" {
				return raw, nil
			}
			if time.Now().After(deadline) {
				return playerContextRaw{}, fmt.Errorf("waxseal: player-context re-read: %s", raw.Error)
			}
		}
		select {
		case <-ctx.Done():
			return playerContextRaw{}, ctx.Err()
		case <-time.After(playerContextPollInterval):
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
