// Package server implements the WaxSeal HTTP service: the bgutil-compatible
// /get_pot endpoint plus /player-context, /session, /report, /ping, and
// /metrics.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/internal/minter"
)

// Config configures a Server. The zero value is usable: keyless, loopback, a
// stable landing video, headless.
type Config struct {
	Addr       string            // listen address (default 127.0.0.1:4416)
	Video      string            // landing video for each tenant session (default a stable id)
	Headful    bool              // run headful (needs a display/Xvfb)
	TenantKeys map[string]string // API key to tenant label; nil selects keyless mode; NewWithContext lists what it rejects
	Logger     *slog.Logger      // nil discards

	// StreamingMaxAge recycles a session at its next streaming handoff once it is
	// older than this age, jittered by up to 10%. A non-positive value disables
	// time-based recycling.
	StreamingMaxAge time.Duration

	// ReportDebounce is the refill interval of the report-driven recycle budget:
	// bursts of up to minter.ReportBurst recycles are allowed before
	// rate-limiting, and past the burst the budget refills at one recycle per
	// interval. A non-positive value uses minter.DefaultReportDebounce.
	ReportDebounce time.Duration

	// MintSeparation, when positive, overrides for every tenant the spacing kept
	// between an in-page mint and a context establishment. Otherwise each tenant
	// reads WAXSEAL_MINT_SEPARATION (default 12s), logging and ignoring a value
	// that is not a positive duration. The server command parses the variable
	// itself, so a bad value stops startup, and passes the result here.
	MintSeparation time.Duration

	// MetricsPublic makes keyed daemons serve full per-tenant /metrics detail
	// without a metrics key. It is ignored for keyless daemons, which already
	// serve full detail.
	MetricsPublic bool

	// MetricsKey is the operator key that unlocks full per-tenant /metrics detail
	// on keyed daemons; tenant keys never do. It must differ from every tenant key
	// and is ignored for keyless daemons.
	MetricsKey string
}

// Server is the running HTTP service over a real-browser minter.
type Server struct {
	tenants        *minter.Tenants
	log            *slog.Logger
	srv            *http.Server
	metricsPublic  bool     // serve full /metrics detail unauthenticated on a keyed daemon
	metricsKeyed   bool     // an operator metrics key is configured
	metricsKeyHash [32]byte // SHA-256 of the operator key, precomputed at startup
}

// requestProcessTimeout bounds how long one request can hold the per-tenant page
// mutex: long enough for the full cold-start retry sequence, short enough that
// a hung request cannot hold it forever. Tests shorten it, so it is a var.
var requestProcessTimeout = 3 * time.Minute

// New is NewWithContext with a background context, so the version handshake
// runs to its own timeout whatever the caller does.
//
// Deprecated: use NewWithContext, which can be interrupted.
func New(cfg Config) (*Server, error) { return NewWithContext(context.Background(), cfg) }

// NewWithContext launches the shared Chromium and builds the service. ctx bounds
// that launch, so a signal during the version handshake stops startup instead of
// waiting it out. It does not attest until Warm or the first request. Shutdown
// tears the browser down. Before anything launches, it rejects a cfg.TenantKeys
// that is an empty map or holds an empty key or label or a key CheckKeyChars
// refuses (a label may repeat, so keys can rotate), and a cfg.MetricsKey equal
// to a tenant key.
func NewWithContext(ctx context.Context, cfg Config) (*Server, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:4416"
	}
	if cfg.Video == "" {
		cfg.Video = browser.DefaultVideo
	}
	if err := checkTenantKeys(cfg.TenantKeys); err != nil {
		return nil, err
	}
	// The CLI makes the same check so it can return a usage exit code.
	if label, collides := MetricsKeyCollision(cfg.TenantKeys, cfg.MetricsKey); collides {
		return nil, fmt.Errorf("waxseal: metrics key collides with API key for tenant %q", label)
	}
	opts := browser.Options{
		Headful:     cfg.Headful,
		NormalizeUA: !cfg.Headful, // remove the HeadlessChrome marker in headless mode
		Logger:      log,
	}
	pool, err := browser.LaunchPool(ctx, opts)
	if err != nil {
		return nil, err
	}
	s := &Server{
		tenants:       minter.NewTenants(pool, cfg.Video, cfg.TenantKeys, opts, cfg.StreamingMaxAge, cfg.ReportDebounce, cfg.MintSeparation),
		log:           log,
		metricsPublic: cfg.MetricsPublic,
		// Hash once here; metricsFull compares fixed-length digests.
		metricsKeyed:   cfg.MetricsKey != "",
		metricsKeyHash: sha256.Sum256([]byte(cfg.MetricsKey)),
	}
	s.srv = newHTTPServer(cfg.Addr, s.routes())
	return s, nil
}

// newHTTPServer builds the daemon's http.Server. WriteTimeout must exceed
// requestProcessTimeout because net/http applies it across handler execution
// and a cold /player-context can use the whole handler budget. It is separate
// so tests can check the timeouts without launching a browser.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,                       // bounds a slow request-body send (bodies <=1 MiB)
		WriteTimeout:      requestProcessTimeout + 30*time.Second, // backstop above the handler budget
		IdleTimeout:       120 * time.Second,
	}
}

// routes registers method-specific handlers and path-only 405 fallbacks, which
// reject unsupported methods before tenant lookup since auth runs in handlers.
// ServeMux sends HEAD to GET handlers, so /session and /player-context get
// explicit HEAD patterns that 405 instead of running browser work; give any new
// browser-backed GET endpoint the same gate. HEAD /ping and /metrics stay
// served (/metrics is cheap; /ping is a bounded probe with no navigation, which
// some balancers send as HEAD), so their fallbacks list HEAD in Allow.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /get_pot", s.handleGetPot)
	mux.HandleFunc("/get_pot", methodNotAllowed(http.MethodPost))
	mux.HandleFunc("GET /player-context", s.handlePlayerContext)
	mux.HandleFunc("POST /player-context", s.handlePlayerContext) // body or ?video_id=
	mux.HandleFunc("HEAD /player-context", methodNotAllowed(http.MethodGet, http.MethodPost))
	mux.HandleFunc("/player-context", methodNotAllowed(http.MethodGet, http.MethodPost))
	mux.HandleFunc("GET /ping", s.handlePing)
	mux.HandleFunc("/ping", methodNotAllowed(http.MethodGet, http.MethodHead))
	mux.HandleFunc("GET /session", s.handleSession)
	mux.HandleFunc("HEAD /session", methodNotAllowed(http.MethodGet))
	mux.HandleFunc("/session", methodNotAllowed(http.MethodGet))
	mux.HandleFunc("POST /report", s.handleReport)
	mux.HandleFunc("/report", methodNotAllowed(http.MethodPost))
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("/metrics", methodNotAllowed(http.MethodGet, http.MethodHead))
	// The "/" catch-all gives unknown paths and trailing-slash mismatches the
	// API's JSON 404 instead of ServeMux's plaintext one; more specific patterns,
	// method fallbacks included, still win. Non-canonical paths get ServeMux's
	// 307 redirect before dispatch.
	mux.HandleFunc("/", s.handleNotFound)
	return mux
}

// methodNotAllowed returns a structured 405 response and lists the supported
// methods in the Allow header.
func methodNotAllowed(allowed ...string) http.HandlerFunc {
	allow := strings.Join(allowed, ", ")
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		writeErr(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

// handleNotFound writes the API error envelope for paths that fall through to
// the "/" catch-all. It does not authenticate, so an unknown path on a keyed
// daemon returns 404 instead of an auth challenge.
func (s *Server) handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, http.StatusNotFound, CodeNotFound, "not found")
}

// Warm attests the tenant selected by apiKey. Pass an empty key in keyless mode.
func (s *Server) Warm(ctx context.Context, apiKey string) error {
	return s.tenants.WarmOne(ctx, apiKey)
}

// SelfTest attests the selected tenant if needed, mints its GVS token only if
// attestation did not cache one, then attempts a full-length streaming proof.
// Attestation and persistent mint failures are returned; a failed proof is
// logged, not returned, and a later /player-context or /session retries it.
// Pass an empty key in keyless mode.
func (s *Server) SelfTest(ctx context.Context, apiKey string) error {
	return s.tenants.SelfTestOne(ctx, apiKey)
}

// Addr is the configured listen address.
func (s *Server) Addr() string { return s.srv.Addr }

// BrowserPID returns the process ID of the shared Chromium launcher, or 0 if it
// is unavailable.
func (s *Server) BrowserPID() int { return s.tenants.CurrentBrowserPID() }

// ListenAndServe runs the HTTP server until Shutdown.
func (s *Server) ListenAndServe() error { return s.srv.ListenAndServe() }

// Serve accepts HTTP requests on ln and closes the listener before returning.
func (s *Server) Serve(ln net.Listener) error { return s.srv.Serve(ln) }

// Shutdown drains in-flight requests until they finish or ctx is done, then
// tears down the browser either way and returns the drain's error. The waxseal
// server command bounds ctx with --shutdown-timeout (default 60s).
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	s.tenants.Close()
	return err
}

// apiKey extracts the tenant key, preferring X-API-Key, then an Authorization
// Bearer header, then the key query parameter. Each source is skipped when it
// carries nothing, so a request may present the key in whichever one it can.
func apiKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if k := bearerKey(r.Header.Get("Authorization")); k != "" {
		return k
	}
	return r.URL.Query().Get("key")
}

// bearerKey returns the credentials from an RFC 7235 "Bearer" Authorization
// header, matching the scheme case-insensitively as the RFC requires. It
// returns "" for another scheme or an empty value, so apiKey falls through to
// ?key= instead of 401ing a request whose ?key= is fine.
func bearerKey(a string) string {
	scheme, rest, ok := strings.Cut(a, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

// tenant resolves the request's Minter. It writes a 401 response and returns
// false when the key is unknown.
func (s *Server) tenant(w http.ResponseWriter, r *http.Request) (*minter.Minter, string, bool) {
	m, label, err := s.tenants.Minter(apiKey(r))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or missing API key")
		return nil, "", false
	}
	return m, label, true
}

func (s *Server) handleGetPot(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	var req struct {
		ContentBinding string `json:"content_binding"`
		Scope          string `json:"scope"`
	}
	// Lenient: /get_pot is the bgutil-compatible endpoint, so a generic yt-dlp
	// client's extra fields are ignored rather than rejected.
	if !decodeJSONBody(w, r, &req, false, false) {
		return
	}
	if req.ContentBinding == "" {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "content_binding is required (the video_id for player, or the visitor_data for gvs)")
		return
	}
	if len(req.ContentBinding) > browser.MaxContentBindingBytes {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("content_binding too long (max %d bytes)", browser.MaxContentBindingBytes))
		return
	}
	// content_binding is opaque, so it does not get the video_id or reason
	// charset checks. The JSON decoder rejects raw C0 bytes; this rejects escaped
	// controls and DEL after decoding.
	if browser.HasControlChars(req.ContentBinding) {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "content_binding must not contain control characters")
		return
	}
	// A URL-shaped binding gets a warning on the 200 response, not a rejection:
	// the binding is opaque and may be visitor_data.
	warning, warnURL := browser.URLBindingWarningFor("content_binding", req.ContentBinding)
	if warnURL {
		s.log.Warn("content_binding looks like a URL", "tenant", label, "binding_len", len(req.ContentBinding))
	}
	scope, ok := normalizeScope(req.Scope)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, `scope must be "player", "gvs", "pot", or omitted`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestProcessTimeout)
	defer cancel()
	res, cached, err := m.Mint(ctx, scope, req.ContentBinding)
	if err != nil {
		if s.writeCtxErr(w, r, ctx, label) {
			return
		}
		writeRefusal(w, http.StatusBadGateway, CodeMintFailed, "mint failed: "+err.Error(), err)
		return
	}
	// Report the token's own expiry (attest time plus lifetime, kept through the
	// cache), not now plus lifetime, which would overstate a cache hit by its age.
	expires := res.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().Add(6 * time.Hour)
	}
	if cached {
		w.Header().Set("X-POT-Cache", "hit")
	} else {
		w.Header().Set("X-POT-Cache", "miss")
	}
	s.log.Info("minted", "tenant", label, "binding_len", len(req.ContentBinding), "scope", scope, "kind", res.Kind, "token_len", res.TokenLen, "cached", cached)
	writeJSON(w, http.StatusOK, TokenResponse{
		POToken:        res.Token,
		ContentBinding: req.ContentBinding,
		ExpiresAt:      expires.UTC().Format(time.RFC3339),
		Warning:        warning,
	})
}

// handlePlayerContext returns the attested browser's streaming context for a
// video_id, including the status-1 SABR URL, plus session_generation. The
// consumer makes the streaming request itself.
func (s *Server) handlePlayerContext(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	videoID, ok := playerContextVideoID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestProcessTimeout)
	defer cancel()
	pc, gen, err := m.PlayerContext(ctx, videoID)
	if err != nil {
		// A client disconnect or the server's own timeout is handled uniformly; the
		// switch below maps only operation-specific failures.
		if s.writeCtxErr(w, r, ctx, label) {
			return
		}
		switch {
		case errors.Is(err, browser.ErrUnplayable):
			// Preserve the status so clients do not need to parse the
			// human-readable error.
			status := ""
			if ue, ok := errors.AsType[*browser.UnplayableError](err); ok {
				status = ue.Status
			}
			writeErrDetails(w, http.StatusUnprocessableEntity, CodeVideoUnavailable, err.Error(), status)
		default:
			writeRefusal(w, http.StatusBadGateway, CodePlayerContextFailed, "player-context failed: "+err.Error(), err)
		}
		return
	}
	s.log.Info("player-context handed out", "tenant", label, "video_id_len", len(videoID), "generation", gen,
		"playability_status", pc.PlayabilityStatus, "abr_url_len", len(pc.ServerAbrStreamingURL), "audio_formats", len(pc.AudioFormats))
	// Keep the embedded context fields at the top level for wire compatibility.
	writeJSON(w, http.StatusOK, struct {
		browser.PlayerContext
		SessionGeneration uint64 `json:"session_generation"`
	}{pc, gen})
}

// playerContextVideoID reads video_id from the JSON body, or from the query
// string when the body is empty or omits it. It writes a 400 and returns false
// when the input is missing or malformed.
func playerContextVideoID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		VideoID string `json:"video_id"`
	}
	// Lenient: video_id may come from the query instead, and a typo'd body key
	// with no query fallback already fails the required check below.
	if !decodeJSONBody(w, r, &req, true, false) {
		return "", false
	}
	if req.VideoID == "" {
		req.VideoID = r.URL.Query().Get("video_id")
	}
	if req.VideoID == "" {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "video_id is required")
		return "", false
	}
	if !browser.ValidVideoID(req.VideoID) {
		msg := "video_id must contain 1 to 64 letters, digits, underscores, or hyphens"
		if strings.Contains(req.VideoID, "://") {
			msg = "video_id must be a bare ID, not a URL"
		}
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, msg)
		return "", false
	}
	return req.VideoID, true
}

// normalizeScope canonicalizes the /get_pot cache scope, ignoring case and
// surrounding space; "" means the generic "pot". The content_binding, not the
// scope, determines the token type.
func normalizeScope(raw string) (string, bool) {
	switch s := strings.ToLower(strings.TrimSpace(raw)); s {
	case "", "pot":
		return "pot", true
	case "player", "gvs":
		return s, true
	default:
		return "", false
	}
}

// strictPing reports whether ?strict asks /ping for 503 on probe-failed, and
// whether the value parsed. A bare ?strict enables it. An unparseable value is
// an error, not a silent false, so a typo in a liveness probe fails loudly.
func strictPing(r *http.Request) (strict, ok bool) {
	q := r.URL.Query()
	if !q.Has("strict") {
		return false, true
	}
	v := q.Get("strict")
	if v == "" { // bare ?strict or ?strict=: presence means enabled
		return true, true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false
	}
	return b, true
}

// strictPingUsage is the 400 message for a ?strict value strictPing cannot read.
const strictPingUsage = `strict must be a boolean ("true", "false", "1", "0"), a bare ?strict, or omitted`

// errBrowserTornDown is the probe error for a browser this probe found wedged.
// The pool has already replaced it by the time the probe reports.
var errBrowserTornDown = errors.New("the shared browser missed two probes and was torn down and relaunched")

// handlePing is the health probe; it never attests or mints. A tenant key, or
// no key on a keyless daemon, probes that tenant's session; no key on a keyed
// daemon probes the shared browser (see handleDaemonPing). A tenant probe whose
// page did not answer runs the browser check too, so a wedged Chromium is
// replaced by the probe instead of stalling the next request.
//
// The body is a health report, not the error envelope, and always carries a
// reason (the PingReason values). The probe runs on the raw request context
// because every step bounds itself (four session round trips of
// pingProbeTimeout, a session teardown, two browser round trips, a browser
// teardown, and a launch handshake), so only the caller leaving ends it early,
// and that writes nothing.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	// The image's HEALTHCHECK sends no key, so it lands here on a keyed daemon. A
	// present key is always resolved, so a typo in a probe still fails with 401.
	if s.tenants.Keyed() && apiKey(r) == "" {
		s.handleDaemonPing(w, r)
		return
	}
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	// Validate before probing, so a bad value is reported whatever the session's
	// health. A rejected parameter gets the error envelope like any other 400.
	strict, ok := strictPing(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, strictPingUsage)
		return
	}
	snap, live, err := m.Health(r.Context())
	reason, relaunched := PingReasonOK, false
	if err != nil {
		if s.pingAbandoned(r, "tenant", label) {
			return
		}
		reason = tenantPingReason(err)
		s.logPing(reason, err, "tenant", label)
		// Every reason escalates, since a retired page, a busy one, and none at
		// all look the same from a wedged Chromium. A browser that answers leaves
		// the tenant reason standing: the failure was the page's own, or
		// contention. The healthy path skips this because a page that answered has
		// proved the browser.
		reason, err, relaunched = s.checkBrowser(r.Context(), reason, err, "tenant", label)
		if s.pingAbandoned(r, "tenant", label) {
			return
		}
	}
	// No guest identity here; navigator_webdriver stays as a detection health
	// signal. Browser proof describes playback in the daemon, and a consumer
	// report can still mark a proven session suspect. On failure the snapshot
	// fields are zero except generation, the last-known one as in /metrics,
	// unless the probe was busy: that carries the live session's values.
	writePing(w, strict, reason, err, map[string]any{
		"ok":                         live,
		"probe":                      PingProbeTenant,
		"keyed":                      s.tenants.Keyed(),
		"tenant":                     label,
		"attest":                     snap.AttestKind,
		"generation":                 snap.Generation,
		"navigator_webdriver":        snap.Identity.Webdriver,
		"browser_proof_established":  snap.BrowserProofEstablished,
		"last_browser_proof_outcome": snap.LastBrowserProofOutcome,
		"streaming_suspect":          snap.StreamingSuspect,
		"browser_relaunched":         relaunched,
	})
}

// handleDaemonPing answers a keyless probe on a keyed daemon with the shared
// browser's health. The body says only whether a browser is running and, if
// not, why (keyed adds nothing a probe:"daemon" body does not imply), which is
// less than the redacted /metrics serves anyone. There is no loopback gate: an
// orchestrator probes from the node and Docker's port proxy from the bridge
// address, so the source address identifies no one. A caller cannot make a
// healthy browser fail, so a probe's teardown and relaunch hit only a wedged or
// dead browser, and the pool single-flights and backs off relaunches.
func (s *Server) handleDaemonPing(w http.ResponseWriter, r *http.Request) {
	strict, ok := strictPing(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, strictPingUsage)
		return
	}
	reason, err, relaunched := s.checkBrowser(r.Context(), PingReasonOK, nil) // it names the browser itself
	if err != nil && s.pingAbandoned(r, "probe", PingProbeDaemon) {
		return
	}
	writePing(w, strict, reason, err, map[string]any{
		"ok":                 err == nil,
		"probe":              PingProbeDaemon,
		"keyed":              s.tenants.Keyed(),
		"browser_relaunched": relaunched,
	})
}

// tenantPingReason maps a Minter.Health error to the reason a tenant probe
// reports: the two benign windows, or probe-failed for a retired session.
func tenantPingReason(err error) string {
	switch {
	case errors.Is(err, minter.ErrNoSession):
		// A report can retire the session before the next streaming request
		// lazily creates a replacement. Treat that gap as expected.
		return PingReasonNoSession
	case errors.Is(err, minter.ErrProbeBusy):
		// A confirmed failure that could not take the page from a running
		// request. Nothing was retired and it says as much about contention as
		// about the browser, so it stays 200 under strict: three in a row must not
		// mark a healthy container unhealthy.
		return PingReasonBusy
	}
	return PingReasonProbeFailed
}

// checkBrowser runs the browser check and folds its outcome into the probe's
// reason and error so far (a tenant probe's, or ok and nil at daemon level). A
// browser that answered leaves them, and so does one that had exited and was
// relaunched, since the daemon has a browser again; the relaunch is reported.
// One this probe found wedged, or could not replace, is a loss: the reason
// becomes probe-failed, logged at warn naming the browser so it is not read as
// one tenant's, and the browser's error is appended to any tenant probe error.
// Cancellation is returned as is, unlogged, for the abandoned-request check.
func (s *Server) checkBrowser(ctx context.Context, reason string, err error, attrs ...any) (string, error, bool) {
	rec, berr := s.tenants.BrowserHealth(ctx)
	if ctx.Err() != nil {
		return reason, ctx.Err(), false
	}
	relaunched := rec != browser.RecoveryNone
	switch {
	case berr != nil:
		berr = fmt.Errorf("no browser answers and none could be launched: %w", berr)
	case rec == browser.RecoveryTornDown:
		berr = errBrowserTornDown
	default:
		return reason, err, relaunched
	}
	s.logPing(PingReasonProbeFailed, berr, append(attrs, "probe", PingProbeDaemon)...)
	if err != nil {
		berr = fmt.Errorf("%v; %w", err, berr)
	}
	return PingReasonProbeFailed, berr, relaunched
}

// pingAbandoned reports whether the caller has gone away, logging it at debug.
// Nothing is written for one: the response would go nowhere, and a disconnect
// is not a server condition.
func (s *Server) pingAbandoned(r *http.Request, attrs ...any) bool {
	if !clientGone(r) {
		return false
	}
	s.log.Debug("request abandoned by client", append(attrs, "err", r.Context().Err())...)
	return true
}

// logPing records a probe outcome. A loss is logged at warn so it is visible
// even when nobody reads the body; the busy window at debug, since probe_busy
// already counts it and it is benign.
func (s *Server) logPing(reason string, err error, attrs ...any) {
	switch reason {
	case PingReasonProbeFailed:
		s.log.Warn("ping probe failed", append(attrs, "err", err)...)
	case PingReasonBusy:
		s.log.Debug("ping probe could not act: the session is busy", append(attrs, "err", err)...)
	}
}

// writePing writes a health body. Under strict, a failed probe whose reason is
// not benign (see BenignPingReason) gets 503; everything else is 200. The body
// skips the error envelope, so presentErr strips and clamps its error here.
func writePing(w http.ResponseWriter, strict bool, reason string, err error, body map[string]any) {
	status := http.StatusOK
	if strict && err != nil && !BenignPingReason(reason) {
		status = http.StatusServiceUnavailable
	}
	body["reason"] = reason
	if err != nil {
		body["error"] = presentErr(err.Error())
	}
	writeJSON(w, status, body)
}

// TokenResponse is the /get_pot response. ExpiresAt is RFC 3339 in UTC. Warning
// is omitted unless the daemon has something to say about the binding.
type TokenResponse struct {
	POToken        string `json:"poToken"`
	ContentBinding string `json:"contentBinding"`
	ExpiresAt      string `json:"expiresAt"`
	Warning        string `json:"warning,omitempty"`
}

// ReportResponse is the /report response. RetryAfterSeconds is omitted unless
// the report was rate-limited: any value there tells a consumer to back off, so
// an accepted report must not carry a zero.
type ReportResponse struct {
	Accepted          bool   `json:"accepted"`
	Retired           bool   `json:"retired"`
	RetirementPending bool   `json:"retirement_pending"`
	Generation        uint64 `json:"generation"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

// SessionResponse is the /session response. It is exported, with SessionCookie,
// so the README block stays a checkable contract (TestSessionShapeContract).
type SessionResponse struct {
	VisitorData       string          `json:"visitor_data"`
	UserAgent         string          `json:"user_agent"`
	ClientVersion     string          `json:"client_version"`
	Cookies           []SessionCookie `json:"cookies"`
	CookieHeader      string          `json:"cookie_header"`
	SessionGeneration uint64          `json:"session_generation"`
}

// SessionCookie is the wire representation of one youtube.com cookie.
type SessionCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"http_only"`
	// Expires is the absolute expiry in RFC3339 and is omitted for session cookies.
	// SameSite is "Strict", "Lax", or "None"; it is omitted when unset.
	Expires  string `json:"expires,omitempty"`
	SameSite string `json:"same_site,omitempty"`
}

// sameSiteWire maps an http.SameSite to its wire string. An unset or unknown
// value yields "" so the field is omitted.
func sameSiteWire(s http.SameSite) string {
	switch s {
	case http.SameSiteStrictMode:
		return "Strict"
	case http.SameSiteLaxMode:
		return "Lax"
	case http.SameSiteNoneMode:
		return "None"
	default:
		return ""
	}
}

// handleSession exports the tenant's anonymous visitor_data and cookies. A
// consumer can adopt them so its GVS token and requests use the same identity.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	// SessionSnapshot may perform the full-length proof, so apply the same timeout
	// used by the other browser-backed endpoints.
	ctx, cancel := context.WithTimeout(r.Context(), requestProcessTimeout)
	defer cancel()
	id, raw, gen, err := m.SessionSnapshot(ctx)
	if err != nil {
		if s.writeCtxErr(w, r, ctx, label) {
			return
		}
		writeRefusal(w, http.StatusServiceUnavailable, CodeNoSession, "no session: "+err.Error(), err)
		return
	}
	cookies := make([]SessionCookie, 0, len(raw))
	pairs := make([]string, 0, len(raw))
	for _, c := range raw {
		sc := SessionCookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HttpOnly, SameSite: sameSiteWire(c.SameSite),
		}
		if !c.Expires.IsZero() {
			sc.Expires = c.Expires.UTC().Format(time.RFC3339)
		}
		cookies = append(cookies, sc)
		pairs = append(pairs, c.Name+"="+c.Value)
	}
	s.log.Info("session handed out", "tenant", label, "visitor_data_len", len(id.VisitorData), "cookies", len(cookies), "generation", gen)
	writeJSON(w, http.StatusOK, SessionResponse{
		VisitorData:       id.VisitorData,
		UserAgent:         id.UserAgent,
		ClientVersion:     id.ClientVersion,
		Cookies:           cookies,
		CookieHeader:      strings.Join(pairs, "; "),
		SessionGeneration: gen,
	})
}

// reportReasonRe bounds the optional reason field and excludes log control
// characters.
var reportReasonRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// handleReport accepts a consumer's report that the session_generation it got
// from /player-context or /session produced a degraded stream. Reports are
// scoped and rate-limited per tenant. A valid report always gets 200; accepted
// says whether it applied to the current session.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	var req struct {
		SessionGeneration uint64 `json:"session_generation"`
		VideoID           string `json:"video_id"`
		Reason            string `json:"reason"`
	}
	if !decodeJSONBody(w, r, &req, false, true) {
		return
	}
	if req.SessionGeneration == 0 {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "session_generation is required (returned by /player-context and /session)")
		return
	}
	if req.VideoID != "" && !browser.ValidVideoID(req.VideoID) {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "video_id must contain 1 to 64 letters, digits, underscores, or hyphens")
		return
	}
	// Reject invalid reasons instead of silently changing their contents.
	if req.Reason != "" && !reportReasonRe.MatchString(req.Reason) {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "reason must contain 1 to 64 letters, digits, underscores, or hyphens")
		return
	}
	res := m.ReportDegraded(req.SessionGeneration, req.VideoID, req.Reason)
	s.log.Info("degradation reported", "tenant", label, "video_id_len", len(req.VideoID), "reason", req.Reason,
		"generation", req.SessionGeneration, "accepted", res.Accepted, "retired", res.Retired, "retirement_pending", res.RetirementPending)
	if res.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(res.RetryAfterSeconds))
	}
	writeJSON(w, http.StatusOK, ReportResponse{
		Accepted:          res.Accepted,
		Retired:           res.Retired,
		RetirementPending: res.RetirementPending,
		Generation:        res.Generation,
		RetryAfterSeconds: res.RetryAfterSeconds,
	})
}

// metricsFull reports whether a request may see full per-tenant /metrics detail.
// Keyless daemons and --metrics-public always serve full detail. On keyed
// daemons, only the operator metrics key unlocks detail; tenant keys do not.
func (s *Server) metricsFull(r *http.Request) bool {
	if s.metricsPublic || !s.tenants.Keyed() {
		return true
	}
	if !s.metricsKeyed {
		return false
	}
	// Hash the presented key before comparison so both operands have fixed length.
	// That avoids the length-based early return in ConstantTimeCompare.
	got := sha256.Sum256([]byte(apiKey(r)))
	return subtle.ConstantTimeCompare(got[:], s.metricsKeyHash[:]) == 1
}

// handleMetrics returns operational data: per-tenant counters and state, but no
// tokens, cookies, or keys. On keyed daemons, requests without the operator key
// receive a redacted aggregate. Every case returns HTTP 200.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metricsFull(r) {
		writeJSON(w, http.StatusOK, s.tenants.MetricsSnapshot())
		return
	}
	writeJSON(w, http.StatusOK, s.tenants.AggregateMetricsSnapshot())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clientGone reports whether the caller disconnected or its request context
// timed out. Browser-backed handlers derive their work context from it, so a
// failed work context with clientGone false means the server's timeout fired.
func clientGone(r *http.Request) bool { return r.Context().Err() != nil }

// writeCtxErr handles the request-lifecycle outcomes of a failed browser-backed
// operation and reports whether it did; on false the caller maps the error. ctx
// must be the handler's WithTimeout(r.Context(), requestProcessTimeout). A
// departed caller gets nothing written (see pingAbandoned); a fired
// requestProcessTimeout gets a 504. It tests ctx.Err(), not the error, since a
// shorter inner browser deadline (a ~45s nav or ~25s player-load stall) can
// also yield DeadlineExceeded but is an upstream failure.
func (s *Server) writeCtxErr(w http.ResponseWriter, r *http.Request, ctx context.Context, label string) bool {
	if clientGone(r) {
		s.log.Debug("request abandoned by client", "tenant", label, "err", r.Context().Err())
		return true
	}
	if ctx.Err() != nil {
		writeErr(w, http.StatusGatewayTimeout, CodeTimeout, "request timed out")
		return true
	}
	return false
}

const (
	// CodeUnauthorized indicates a missing or invalid API key.
	CodeUnauthorized = "unauthorized"
	// CodeMethodNotAllowed indicates that the endpoint does not support the
	// request method.
	CodeMethodNotAllowed = "method-not-allowed"
	// CodeInvalidRequest indicates malformed input or a missing required field.
	CodeInvalidRequest = "invalid-request"
	// CodeMintFailed indicates that the daemon could not mint a token.
	CodeMintFailed = "mint-failed"
	// CodeVideoUnavailable indicates a terminal playabilityStatus or an on-air
	// broadcast; details carries the status or "LIVE_BROADCAST".
	CodeVideoUnavailable = "video-unavailable"
	// CodeTimeout indicates that a browser-backed request (player-context, mint,
	// or session) used up the daemon's per-request time budget.
	CodeTimeout = "timeout"
	// CodePlayerContextFailed indicates a non-terminal player-context failure.
	CodePlayerContextFailed = "player-context-failed"
	// CodeNoSession indicates that no attested session or cookies are available.
	CodeNoSession = "no-session"
	// CodeNotFound indicates an unknown path or endpoint.
	CodeNotFound = "not-found"
)

// The /ping probe values. The probe field, present on every health body, says
// what the daemon checked: a tenant's session (a keyed request, or any request
// on a keyless daemon) or the shared browser (a keyless request on a keyed
// daemon). The 400 and 401 rejections use the error envelope instead.
const (
	// PingProbeTenant means the body describes one tenant's attested session.
	PingProbeTenant = "tenant"
	// PingProbeDaemon means the body describes the shared Chromium's liveness
	// and nothing about any tenant.
	PingProbeDaemon = "daemon"
)

// The /ping reason values. The reason field, present on every health body,
// carries exactly one of these, and only PingReasonProbeFailed maps to 503 under
// ?strict=true.
const (
	// PingReasonOK means a live session answered the probe, or at daemon level
	// that a browser is running.
	PingReasonOK = "ok"
	// PingReasonNoSession means no session is attested: a report retires one and
	// re-establishment is lazy. Benign.
	PingReasonNoSession = "no-session"
	// PingReasonBusy means a probe failed twice while a request held the page, so
	// nothing was retired and the next probe re-checks. Benign.
	PingReasonBusy = "busy"
	// PingReasonProbeFailed means this probe confirmed a loss: a live session's
	// probe failed twice and the session was retired, or the shared browser
	// missed two probes and was torn down, or no browser answers and none could
	// be launched.
	PingReasonProbeFailed = "probe-failed"
)

// BenignPingReason reports whether a not-ok /ping reason is expected on a
// working daemon. /ping keeps these at 200 even under ?strict=true, and
// `waxseal ping --strict` calls this rather than keeping its own list, so the
// two cannot disagree; the CLI reads the reason because a pre-strict daemon
// answers a real probe failure with 200.
func BenignPingReason(reason string) bool {
	return reason == PingReasonNoSession || reason == PingReasonBusy
}

// errEnvelope is the JSON error response shared by the API endpoints.
type errEnvelope struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Details string `json:"details,omitempty"`
	// RetryAfterSeconds mirrors the Retry-After header for a JSON consumer that
	// never sees it. Absent when the daemon cannot put a number on the wait.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// maxErrTextBytes bounds each error-envelope text field. err.Error() can carry
// multi-KiB CDP/V8 stack traces, and an envelope past the client's 64 KiB read
// cap can lose Code and Details. Even with JSON escaping at six bytes per byte
// (\u00XX), two clamped fields fit under that cap.
const maxErrTextBytes = 4 << 10

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeErrDetails(w, status, code, msg, "")
}

// writeRefusal is writeErr for a refusal the daemon expects to lift on its own
// (a proof or bot-check cool-down, or the shared browser's relaunch backoff).
// The minter's wait on err goes out as both Retry-After, for generic HTTP
// clients, and retry_after_seconds, for JSON consumers; no wait sets neither.
// /report sets its own: it answers 200, not a refusal.
func writeRefusal(w http.ResponseWriter, status int, code, msg string, err error) {
	secs := 0
	if ra, ok := errors.AsType[*minter.RetryAfterError](err); ok {
		secs = ra.Seconds()
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	writeErrEnvelope(w, status, errEnvelope{Error: msg, Code: code, RetryAfterSeconds: secs})
}

// clampErrText caps s at maxErrTextBytes and appends a marker. It may split a
// UTF-8 sequence; encoding/json replaces invalid bytes with U+FFFD, so the
// envelope stays valid JSON.
func clampErrText(s string) string {
	if len(s) <= maxErrTextBytes {
		return s
	}
	return s[:maxErrTextBytes] + "... [truncated]"
}

// presentErr strips the internal "waxseal: " prefix of package errors, as the
// CLI's renderError does, embedded copies included ("mint failed: waxseal: x"
// becomes "mint failed: x"), then clamps.
func presentErr(s string) string { return clampErrText(strings.ReplaceAll(s, "waxseal: ", "")) }

// decodeErrMsg returns a stable client-facing message for a JSON decoding error
// without exposing Go type information.
func decodeErrMsg(err error) string {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return "request body too large (max 1 MiB)"
	}
	if isReadTimeout(err) {
		return "request body was not received before the read timeout"
	}
	if errors.Is(err, io.EOF) {
		return "request body is empty"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "request body is truncated (incomplete JSON)"
	}
	// json.Decoder reports some syntax errors, including unterminated strings, as
	// io.ErrUnexpectedEOF. Other syntax errors reach this branch.
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return "request body contains malformed JSON"
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		switch {
		case typeErr.Field == "":
			return "request body must be a JSON object"
		case strings.Contains(typeErr.Field, "."):
			return "request body contains a field with the wrong type"
		default:
			return "field \"" + typeErr.Field + "\" has the wrong type"
		}
	}
	return "request body contains invalid JSON"
}

// decodeJSONBody decodes exactly one JSON object of at most 1 MiB from r.Body
// into dst, or writes a 400 invalid-request and returns false. allowEmpty
// accepts an empty body so the caller can use another input source;
// strictFields rejects unknown fields (see below).
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, allowEmpty, strictFields bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var raw json.RawMessage
	err := dec.Decode(&raw)
	if allowEmpty && errors.Is(err, io.EOF) {
		return true
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, decodeErrMsg(err))
		return false
	}
	// null would decode into dst as a no-op and read as a missing field, and a
	// number, array, or string as a type error; one message covers all four.
	// Decode yields the value without surrounding space, so the length check
	// only guards the index.
	if len(raw) == 0 || raw[0] != '{' {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "request body must be a JSON object")
		return false
	}
	// Only /report is strict: a typo'd key beside its optional fields (video_id,
	// reason) would otherwise vanish. /get_pot stays lenient for bgutil and
	// yt-dlp interop, since yt-dlp POSTs extra fields (proxy, bypass_cache,
	// source_address, ...). /player-context stays lenient because its one field
	// is required, so a typo already fails, and leniency keeps the query fallback.
	if strictFields {
		// encoding/json matches names case-insensitively, so DisallowUnknownFields
		// would let REASON through; unknownField matches keys to tags exactly.
		if msg, ok := unknownField(raw, dst); ok {
			writeErr(w, http.StatusBadRequest, CodeInvalidRequest, msg)
			return false
		}
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, decodeErrMsg(err))
		return false
	}
	// A second decode rejects non-whitespace data after the first JSON value.
	err = dec.Decode(&struct{}{})
	if errors.Is(err, io.EOF) {
		return true
	}
	// MaxBytesReader may not report an oversized body until this second decode,
	// such as when a valid object is followed by too much whitespace, and a client
	// that stopped sending mid-body arrives here as the read timeout.
	msg := "request body must be a single JSON object"
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) || isReadTimeout(err) {
		msg = decodeErrMsg(err)
	}
	writeErr(w, http.StatusBadRequest, CodeInvalidRequest, msg)
	return false
}

// unknownField reports the first key of the object in raw, in sorted order,
// that is not a json tag of dst's struct. Duplicate keys stay lenient.
func unknownField(raw json.RawMessage, dst any) (string, bool) {
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil {
		return "", false // decodeJSONBody's typed decode reports the syntax error
	}
	allowed := jsonFieldNames(reflect.TypeOf(dst))
	// The lexicographically smallest unknown key, so a body carrying several
	// typos names the same one on every request.
	first, found := "", false
	for k := range keys {
		if allowed[k] {
			continue
		}
		if !found || k < first {
			first, found = k, true
		}
	}
	if !found {
		return "", false
	}
	return fmt.Sprintf("request body contains unknown field %q", first), true
}

// jsonFieldNames returns the JSON names the struct type t accepts. An untagged
// embedded struct contributes its promoted fields, not its type name. A
// non-struct yields an empty set, so a caller that passes one rejects every key
// instead of panicking or silently losing the strictness it asked for.
func jsonFieldNames(t reflect.Type) map[string]bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	names := map[string]bool{}
	if t.Kind() != reflect.Struct {
		return names
	}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous && name == "" {
			for k := range jsonFieldNames(f.Type) {
				names[k] = true
			}
			continue
		}
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		names[name] = true
	}
	return names
}

// isReadTimeout reports whether err is the request body's read deadline firing.
func isReadTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func writeErrDetails(w http.ResponseWriter, status int, code, msg, details string) {
	writeErrEnvelope(w, status, errEnvelope{Error: msg, Code: code, Details: details})
}

// writeErrEnvelope is the one path every error response takes, so the text clamp
// applies wherever an envelope is built.
func writeErrEnvelope(w http.ResponseWriter, status int, env errEnvelope) {
	env.Error, env.Details = presentErr(env.Error), presentErr(env.Details)
	writeJSON(w, status, env)
}

// checkTenantKeys enforces NewWithContext's TenantKeys rules, which a
// caller-built map needs because it skips ParseTenantKeys: an empty map would
// read as keyless, and an empty key would match a request that sends none.
// Errors name labels, never keys.
func checkTenantKeys(keys map[string]string) error {
	if keys == nil {
		return nil
	}
	if len(keys) == 0 {
		return errors.New("waxseal: empty TenantKeys map; pass nil to run keyless")
	}
	// Sorted so the same entry is named on every run.
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		label := keys[key]
		switch {
		case strings.TrimSpace(label) == "":
			return errors.New("waxseal: TenantKeys entry has an empty label")
		case key == "":
			return fmt.Errorf("waxseal: TenantKeys entry for tenant %q has an empty key", label)
		}
		if err := CheckKeyChars(key); err != nil {
			return fmt.Errorf("waxseal: TenantKeys entry for tenant %q: %w", label, err)
		}
	}
	return nil
}

// MetricsKeyCollision reports the tenant label that shares an API key with
// metricsKey, if one exists. An empty metricsKey never collides. NewWithContext
// and the waxseal server command both call it before accepting a metrics key.
func MetricsKeyCollision(tenantKeys map[string]string, metricsKey string) (label string, collides bool) {
	if metricsKey == "" {
		return "", false
	}
	label, collides = tenantKeys[metricsKey]
	return label, collides
}

// CheckKeyChars rejects a key containing whitespace or a character that does
// not print. Such a key is a parsing accident (a newline from a file, a pasted
// space, a zero-width space or byte-order mark from a web page) and would match
// nothing a client sends. It tests unicode.IsPrint because IsControl sees only
// C0 and C1, not the invisible format characters. The error never includes the
// key.
func CheckKeyChars(key string) error {
	if strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) {
		return errors.New("key contains whitespace or a character that does not print")
	}
	return nil
}

// ParseTenantKeys parses label=key entries and bare API keys, separated by
// commas or newlines, into a map from API key to tenant label. Bare keys get
// generated labels (t1, t2, ...) that avoid explicit ones, and must not contain
// "=", so a padded base64 key needs a label. Blank input returns nil, which
// selects keyless single-tenant mode; other input with no entry, such as a
// lone comma, is an error.
//
// Empty or duplicate keys and labels are rejected, as is a key CheckKeyChars
// refuses. Errors never include API keys.
func ParseTenantKeys(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// An editor's UTF-8 BOM would otherwise become part of the first label.
	s = strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))
	out := map[string]string{} // API key -> tenant label
	labels := map[string]bool{}
	var bareKeys []string
	entry := 0
	for _, pair := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue // tolerate a stray or trailing separator
		}
		entry++
		before, after, found := strings.Cut(pair, "=")
		if !found {
			if err := CheckKeyChars(pair); err != nil {
				return nil, fmt.Errorf("tenant entry %d: %w", entry, err)
			}
			if _, dup := out[pair]; dup {
				return nil, errors.New("duplicate API key")
			}
			out[pair] = "" // placeholder; filled in the second pass
			bareKeys = append(bareKeys, pair)
			continue
		}
		// "alice=" and "Zm9vYmE=" look the same here: a label with no key, or a
		// padded bare key. Neither is guessed at, and the message names the entry
		// by position because either reading may be a key.
		if strings.Trim(after, "=") == "" {
			return nil, fmt.Errorf("tenant entry %d is a label with an empty key, or a bare key ending in \"=\"; write it as label=key", entry)
		}
		label, key := strings.TrimSpace(before), strings.TrimSpace(after)
		if label == "" {
			return nil, errors.New(`tenant entry has an empty label (use "label=key")`)
		}
		if err := CheckKeyChars(key); err != nil {
			return nil, fmt.Errorf("tenant label %q: %w", label, err)
		}
		if _, dup := out[key]; dup {
			return nil, errors.New("duplicate API key")
		}
		if labels[label] {
			return nil, fmt.Errorf("duplicate tenant label %q", label)
		}
		out[key] = label
		labels[label] = true
	}
	// Give each bare key the next unused t<N> label.
	n := 1
	for _, key := range bareKeys {
		for labels["t"+strconv.Itoa(n)] {
			n++
		}
		label := "t" + strconv.Itoa(n)
		out[key] = label
		labels[label] = true
		n++
	}
	if len(out) == 0 {
		return nil, errors.New("tenant keys contain no entries")
	}
	return out, nil
}
