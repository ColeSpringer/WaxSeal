// Package server implements the WaxSeal HTTP service. It exposes the
// bgutil-compatible /get_pot endpoint and the related player-context, session,
// health, and metrics endpoints.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/internal/minter"
)

// Config configures a Server. The zero value is usable: keyless, loopback, a
// stable landing video, headless.
type Config struct {
	Addr       string            // listen address (default 127.0.0.1:4416)
	Video      string            // landing video for each tenant session (default a stable id)
	Headful    bool              // run headful (needs a display/Xvfb)
	TenantKeys map[string]string // API key to tenant label; nil selects keyless single-tenant mode
	Logger     *slog.Logger      // nil discards

	// StreamingMaxAge recycles a session at the next streaming handoff after it
	// reaches this age. A zero value disables time-based recycling.
	StreamingMaxAge time.Duration

	// ReportDebounce is the minimum interval between session recycles caused by
	// consumer reports. A non-positive value uses minter.DefaultReportDebounce.
	ReportDebounce time.Duration
}

// Server is the running HTTP service over a real-browser minter.
type Server struct {
	tenants *minter.Tenants
	log     *slog.Logger
	srv     *http.Server
}

// requestProcessTimeout bounds how long one request can hold the per-tenant page
// mutex. It allows the full cold-start retry sequence while preventing a hung
// request from holding the mutex indefinitely.
const requestProcessTimeout = 3 * time.Minute

// New launches the shared Chromium and builds the service. It does not attest
// until Warm or the first request. Shutdown tears the browser down.
func New(cfg Config) (*Server, error) {
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
	opts := browser.Options{
		Headful:     cfg.Headful,
		NormalizeUA: !cfg.Headful, // remove the HeadlessChrome marker in headless mode
		Logger:      log,
	}
	pool, err := browser.LaunchPool(opts)
	if err != nil {
		return nil, err
	}
	s := &Server{
		tenants: minter.NewTenants(pool, cfg.Video, cfg.TenantKeys, opts, cfg.StreamingMaxAge, cfg.ReportDebounce),
		log:     log,
	}
	s.srv = &http.Server{Addr: cfg.Addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// routes registers method-specific handlers and path-only 405 fallbacks.
// ServeMux routes HEAD requests to GET handlers. Because authentication runs in
// endpoint handlers, unsupported methods are rejected before tenant lookup.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /get_pot", s.handleGetPot)
	mux.HandleFunc("/get_pot", methodNotAllowed(http.MethodPost))
	mux.HandleFunc("GET /player-context", s.handlePlayerContext)
	mux.HandleFunc("POST /player-context", s.handlePlayerContext) // body or ?video_id=
	mux.HandleFunc("/player-context", methodNotAllowed(http.MethodGet, http.MethodPost))
	mux.HandleFunc("GET /ping", s.handlePing)
	mux.HandleFunc("/ping", methodNotAllowed(http.MethodGet))
	mux.HandleFunc("GET /session", s.handleSession)
	mux.HandleFunc("/session", methodNotAllowed(http.MethodGet))
	mux.HandleFunc("POST /report", s.handleReport)
	mux.HandleFunc("/report", methodNotAllowed(http.MethodPost))
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("/metrics", methodNotAllowed(http.MethodGet))
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

// Warm attests the tenant selected by apiKey. Pass an empty key in keyless mode.
func (s *Server) Warm(ctx context.Context, apiKey string) error {
	return s.tenants.WarmOne(ctx, apiKey)
}

// SelfTest mints and caches a GVS token for the selected tenant, then attempts a
// full-length streaming proof. Pass an empty key in keyless mode.
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

// Shutdown drains in-flight requests, then tears down the browser.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	s.tenants.Close()
	return err
}

// apiKey extracts the tenant key from the header (preferred) or a query param.
func apiKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return r.URL.Query().Get("key")
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
	if !decodeJSONBody(w, r, &req, false) {
		return
	}
	if req.ContentBinding == "" {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "content_binding is required (the video_id for player, or the visitor_data for gvs)")
		return
	}
	scope, ok := normalizeScope(req.Scope)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, `scope must be "player", "gvs", or omitted`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestProcessTimeout)
	defer cancel()
	res, cached, err := m.Mint(ctx, scope, req.ContentBinding)
	if err != nil {
		writeErr(w, http.StatusBadGateway, CodeMintFailed, "mint failed: "+err.Error())
		return
	}
	// Use the token's real expiry (fixed at attest time, preserved through the
	// cache) rather than the current time plus lifetime. Otherwise, a cache hit
	// overstates expiry by the token's age.
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
	writeJSON(w, http.StatusOK, map[string]any{
		"poToken":        res.Token,
		"contentBinding": req.ContentBinding,
		"expiresAt":      expires.UTC().Format(time.RFC3339),
	})
}

// handlePlayerContext returns the attested browser's streaming context for a
// video_id. The response contains the status-1 SABR URL, player URL, ustreamer
// config, visitor data, client version, and audio formats. The consumer performs
// the streaming request.
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
		// Separate terminal and timeout failures so callers can choose whether to
		// retry.
		switch {
		case errors.Is(err, browser.ErrUnplayable):
			// Preserve the playabilityStatus so clients do not need to parse the
			// human-readable error.
			status := ""
			if ue, ok := errors.AsType[*browser.UnplayableError](err); ok {
				status = ue.Status
			}
			writeErrDetails(w, http.StatusUnprocessableEntity, CodeVideoUnavailable, err.Error(), status)
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, http.StatusGatewayTimeout, CodeTimeout, "player-context timed out")
		default:
			writeErr(w, http.StatusBadGateway, CodePlayerContextFailed, "player-context failed: "+err.Error())
		}
		return
	}
	s.log.Info("player-context handed out", "tenant", label, "video_id_len", len(videoID), "generation", gen,
		"status", pc.Status, "abr_url_len", len(pc.ServerAbrStreamingURL), "audio_formats", len(pc.AudioFormats))
	// Keep the embedded context fields at the top level for wire compatibility.
	writeJSON(w, http.StatusOK, struct {
		browser.PlayerContext
		SessionGeneration uint64 `json:"session_generation"`
	}{pc, gen})
}

// playerContextVideoID reads video_id from the JSON body or query string. An
// empty body falls through to the query form. The function writes an error
// response and returns false when the input is missing or malformed.
func playerContextVideoID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		VideoID string `json:"video_id"`
	}
	if !decodeJSONBody(w, r, &req, true) {
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

// normalizeScope canonicalizes the cache scope for /get_pot. It trims whitespace
// and accepts names case-insensitively. Empty scope and "pot" map to the generic
// scope. The content_binding, not the scope, determines the token type.
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

// handlePing probes an existing tenant session without launching Chromium,
// attesting, or minting. A failed probe may retire the session. After
// authentication, the handler reports health in an HTTP 200 response body.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	snap, live, err := m.Health(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "tenant": label, "error": err.Error()})
		return
	}
	// Browser proof describes playback in the daemon. A consumer report can still
	// mark the session suspect after a successful proof.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                         live,
		"tenant":                     label,
		"attest":                     snap.AttestKind,
		"identity":                   snap.Identity,
		"generation":                 snap.Generation,
		"browser_proof_established":  snap.BrowserProofEstablished,
		"last_browser_proof_outcome": snap.LastBrowserProofOutcome,
		"streaming_suspect":          snap.StreamingSuspect,
	})
}

// sessionCookie is the wire representation of one youtube.com cookie.
type sessionCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"http_only"`
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
		writeErr(w, http.StatusServiceUnavailable, CodeNoSession, "no session: "+err.Error())
		return
	}
	cookies := make([]sessionCookie, 0, len(raw))
	pairs := make([]string, 0, len(raw))
	for _, c := range raw {
		cookies = append(cookies, sessionCookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HttpOnly,
		})
		pairs = append(pairs, c.Name+"="+c.Value)
	}
	s.log.Info("session handed out", "tenant", label, "visitor_data_len", len(id.VisitorData), "cookies", len(cookies), "generation", gen)
	writeJSON(w, http.StatusOK, map[string]any{
		"visitor_data":       id.VisitorData,
		"user_agent":         id.UserAgent,
		"client_version":     id.ClientVersion,
		"cookies":            cookies,
		"cookie_header":      strings.Join(pairs, "; "),
		"session_generation": gen,
	})
}

// reportReasonRe bounds the optional reason field and excludes log control
// characters.
var reportReasonRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// handleReport accepts a consumer's report that a prior session produced a
// degraded stream. The body names the session_generation returned by
// /player-context or /session. Reports are scoped and rate-limited per tenant.
// Consumers must inspect the accepted response field to learn whether the report
// applied to the current session.
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
	if !decodeJSONBody(w, r, &req, false) {
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
	body := map[string]any{
		"accepted":           res.Accepted,
		"retired":            res.Retired,
		"retirement_pending": res.RetirementPending,
		"generation":         res.Generation,
	}
	if res.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(res.RetryAfterSeconds))
		body["retry_after_seconds"] = res.RetryAfterSeconds
	}
	writeJSON(w, http.StatusOK, body)
}

// handleMetrics returns unauthenticated operational data. It includes per-tenant
// counters and state, but no tokens, cookies, or keys.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tenants.MetricsSnapshot())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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
	// CodeVideoUnavailable indicates a terminal playabilityStatus.
	CodeVideoUnavailable = "video-unavailable"
	// CodeTimeout indicates that the player-context deadline elapsed.
	CodeTimeout = "timeout"
	// CodePlayerContextFailed indicates a non-terminal player-context failure.
	CodePlayerContextFailed = "player-context-failed"
	// CodeNoSession indicates that no attested session or cookies are available.
	CodeNoSession = "no-session"
)

// errEnvelope is the JSON error response shared by the API endpoints.
type errEnvelope struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Details string `json:"details,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errEnvelope{Error: msg, Code: code})
}

// decodeErrMsg returns a stable client-facing message for a JSON decoding error
// without exposing Go type information.
func decodeErrMsg(err error) string {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return "request body too large (max 1 MiB)"
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

// decodeJSONBody decodes exactly one JSON object from r.Body and limits the body
// to 1 MiB. It writes an invalid-request response on failure. When allowEmpty
// is true, an empty body is accepted so the caller can use another input source.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	err := dec.Decode(dst)
	if allowEmpty && errors.Is(err, io.EOF) {
		return true
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, decodeErrMsg(err))
		return false
	}
	// A second decode rejects non-whitespace data after the first JSON value.
	err = dec.Decode(&struct{}{})
	if errors.Is(err, io.EOF) {
		return true
	}
	// MaxBytesReader may not report an oversized body until this second decode,
	// such as when a valid object is followed by too much whitespace.
	msg := "request body must be a single JSON object"
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		msg = decodeErrMsg(err)
	}
	writeErr(w, http.StatusBadRequest, CodeInvalidRequest, msg)
	return false
}

func writeErrDetails(w http.ResponseWriter, status int, code, msg, details string) {
	writeJSON(w, status, errEnvelope{Error: msg, Code: code, Details: details})
}

// ParseTenantKeys parses "label1=key1,label2=key2" or a bare key into a map from
// API key to tenant label. Empty input selects keyless single-tenant mode.
func ParseTenantKeys(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for i, pair := range strings.Split(s, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		before, after, found := strings.Cut(pair, "=")
		key, label := strings.TrimSpace(before), ""
		if found {
			label, key = strings.TrimSpace(before), strings.TrimSpace(after)
		} else {
			label = "t" + strconv.Itoa(i+1) // don't echo the key as a label
		}
		if key != "" {
			out[key] = label
		}
	}
	return out
}
