// Package client calls the WaxSeal HTTP API. It can mint PO tokens, fetch player
// contexts, and export guest sessions without depending on WaxTap. The provider
// module contains the optional WaxTap adapter.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to a WaxSeal daemon over HTTP.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// Token is a minted PO token and its expiry.
type Token struct {
	Value     string
	ExpiresAt time.Time // zero == unknown
}

// Session contains the guest identity and youtube.com cookies exported by
// WaxSeal. A consumer can adopt this session so attestation, token binding, and
// downloads use the same browser identity.
type Session struct {
	VisitorData   string
	UserAgent     string
	ClientVersion string
	Cookies       []*http.Cookie
	// SessionGeneration identifies the daemon session that produced this snapshot.
	// Use it to report a degraded stream.
	SessionGeneration uint64
}

// PlayerContext contains the status-1 streaming context for one video. The SABR
// URL includes a throttling nonce that the consumer must descramble with
// PlayerURL before starting the stream.
//
// This type mirrors browser.PlayerContext without importing the browser package
// and its Chromium dependencies. Keep the JSON tags in sync.
type PlayerContext struct {
	Status                       string        `json:"status"`
	PlayerURL                    string        `json:"player_url"`
	ServerAbrStreamingURL        string        `json:"server_abr_streaming_url"`
	VideoPlaybackUstreamerConfig string        `json:"video_playback_ustreamer_config"`
	VisitorData                  string        `json:"visitor_data"`
	ClientVersion                string        `json:"client_version"`
	Title                        string        `json:"title"`
	Author                       string        `json:"author"`
	LengthSeconds                int           `json:"length_seconds"`
	AudioFormats                 []AudioFormat `json:"audio_formats"`
	// SessionGeneration identifies the daemon session that produced this context.
	// Use it to report a degraded stream.
	SessionGeneration uint64 `json:"session_generation"`
}

// AudioFormat describes one adaptive audio format. Itag, LMT, and XTags must be
// used together when selecting the format from the SABR server. This type mirrors
// browser.AudioFormat over the wire. Keep the JSON tags in sync.
type AudioFormat struct {
	Itag             int    `json:"itag"`
	LMT              string `json:"lmt"`
	XTags            string `json:"xtags"`
	MimeType         string `json:"mime_type"`
	Bitrate          int    `json:"bitrate"`
	ContentLength    int64  `json:"content_length"`
	ApproxDurationMs int    `json:"approx_duration_ms"`
	AudioSampleRate  int    `json:"audio_sample_rate"`
	AudioChannels    int    `json:"audio_channels"`
	AudioQuality     string `json:"audio_quality"`
	IsDrc            bool   `json:"is_drc"`         // whether the rendition uses dynamic range compression
	AudioTrackID     string `json:"audio_track_id"` // audioTrack.id; empty for the default or only track
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sends X-API-Key with every request.
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithHTTPClient overrides the default HTTP client.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// New returns a client for the WaxSeal daemon at baseURL. A typical base URL is
// "http://127.0.0.1:4416".
func New(baseURL string, opts ...Option) *Client {
	c := &Client{baseURL: strings.TrimRight(baseURL, "/"), hc: http.DefaultClient}
	for _, o := range opts {
		o(c)
	}
	return c
}

// POToken mints a token bound to contentBinding. Use a video_id for a player
// token or visitor_data for a GVS token. Scope distinguishes cache entries. An
// empty scope uses the daemon's generic cache key.
func (c *Client) POToken(ctx context.Context, contentBinding, scope string) (Token, error) {
	if contentBinding == "" {
		return Token{}, errors.New("waxseal/client: content_binding is required")
	}
	payload := map[string]string{"content_binding": contentBinding}
	if scope != "" {
		payload["scope"] = scope
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/get_pot", bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Token{}, c.statusErr("/get_pot", resp)
	}
	var out struct {
		POToken   string    `json:"poToken"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Token{}, fmt.Errorf("waxseal/client: decode /get_pot: %w", err)
	}
	if out.POToken == "" {
		return Token{}, errors.New("waxseal/client: /get_pot returned an empty poToken")
	}
	return Token{Value: out.POToken, ExpiresAt: out.ExpiresAt}, nil
}

// Session fetches the daemon's guest identity and cookies.
func (c *Client) Session(ctx context.Context) (*Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/session", nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusErr("/session", resp)
	}
	var out struct {
		VisitorData   string `json:"visitor_data"`
		UserAgent     string `json:"user_agent"`
		ClientVersion string `json:"client_version"`
		Cookies       []struct {
			Name     string `json:"name"`
			Value    string `json:"value"`
			Domain   string `json:"domain"`
			Path     string `json:"path"`
			Secure   bool   `json:"secure"`
			HTTPOnly bool   `json:"http_only"`
		} `json:"cookies"`
		SessionGeneration uint64 `json:"session_generation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("waxseal/client: decode /session: %w", err)
	}
	cookies := make([]*http.Cookie, 0, len(out.Cookies))
	for _, ck := range out.Cookies {
		cookies = append(cookies, &http.Cookie{
			Name: ck.Name, Value: ck.Value, Domain: ck.Domain, Path: ck.Path,
			Secure: ck.Secure, HttpOnly: ck.HTTPOnly,
		})
	}
	return &Session{VisitorData: out.VisitorData, UserAgent: out.UserAgent, ClientVersion: out.ClientVersion, Cookies: cookies, SessionGeneration: out.SessionGeneration}, nil
}

// PlayerContext fetches the status-1 streaming context for videoID. The
// ServerAbrStreamingURL contains a scrambled n parameter that the consumer must
// descramble with PlayerURL.
func (c *Client) PlayerContext(ctx context.Context, videoID string) (*PlayerContext, error) {
	if videoID == "" {
		return nil, errors.New("waxseal/client: video_id is required")
	}
	body, _ := json.Marshal(map[string]string{"video_id": videoID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/player-context", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusErr("/player-context", resp)
	}
	var out PlayerContext
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("waxseal/client: decode /player-context: %w", err)
	}
	if out.ServerAbrStreamingURL == "" {
		return nil, errors.New("waxseal/client: /player-context returned no server_abr_streaming_url")
	}
	return &out, nil
}

// ReportResult describes how the daemon handled a degradation report. Accepted
// indicates that the report applies to the current session. Retired indicates
// that the session was closed immediately. RetirementPending indicates that it
// will be closed at the next streaming handoff. RetryAfterSeconds is set when the
// report was rate-limited.
type ReportResult struct {
	Accepted          bool
	Retired           bool
	RetirementPending bool
	Generation        uint64
	RetryAfterSeconds int
}

// Report tells the daemon that session generation gen produced a degraded stream.
// Pass the SessionGeneration from a prior PlayerContext or Session. videoID and
// reason are optional; reason may contain 1 to 64 letters, digits, underscores,
// or hyphens.
//
// A nil error means that the HTTP request succeeded. Callers must inspect
// ReportResult.Accepted and honor ReportResult.RetryAfterSeconds when set.
func (c *Client) Report(ctx context.Context, gen uint64, videoID, reason string) (ReportResult, error) {
	if gen == 0 {
		return ReportResult{}, errors.New("waxseal/client: session generation is required")
	}
	payload := map[string]any{"session_generation": gen}
	if videoID != "" {
		payload["video_id"] = videoID
	}
	if reason != "" {
		payload["reason"] = reason
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/report", bytes.NewReader(body))
	if err != nil {
		return ReportResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return ReportResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ReportResult{}, c.statusErr("/report", resp)
	}
	var out struct {
		Accepted          bool   `json:"accepted"`
		Retired           bool   `json:"retired"`
		RetirementPending bool   `json:"retirement_pending"`
		Generation        uint64 `json:"generation"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ReportResult{}, fmt.Errorf("waxseal/client: decode /report: %w", err)
	}
	return ReportResult{
		Accepted:          out.Accepted,
		Retired:           out.Retired,
		RetirementPending: out.RetirementPending,
		Generation:        out.Generation,
		RetryAfterSeconds: out.RetryAfterSeconds,
	}, nil
}

func (c *Client) auth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
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

// APIError describes a non-2xx response from the WaxSeal daemon. Callers can
// extract it with errors.AsType[*APIError] and inspect Code instead of matching
// Message.
//
// Code is empty for responses from older servers and for non-JSON proxy
// responses. Message contains the raw body when the response is not a recognized
// error envelope. StatusCode and Path are always set.
type APIError struct {
	Path       string // request path, such as "/player-context"
	StatusCode int    // HTTP status code
	Code       string // stable machine-readable code, when present
	Message    string // error message or unrecognized raw response body
	Details    string // optional machine-readable context
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "(no body)"
	}
	if e.Code != "" {
		return fmt.Sprintf("waxseal/client: %s %d (%s): %s", e.Path, e.StatusCode, e.Code, msg)
	}
	return fmt.Sprintf("waxseal/client: %s %d: %s", e.Path, e.StatusCode, msg)
}

func (c *Client) statusErr(path string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	body := bytes.TrimSpace(b)
	apiErr := &APIError{Path: path, StatusCode: resp.StatusCode}
	if len(body) == 0 {
		return apiErr
	}
	var env struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Details string `json:"details"`
	}
	if err := json.Unmarshal(body, &env); err == nil && (env.Error != "" || env.Code != "") {
		apiErr.Message, apiErr.Code, apiErr.Details = env.Error, env.Code, env.Details
		return apiErr
	}
	// Preserve proxy errors and other unrecognized bodies for diagnostics.
	apiErr.Message = string(body)
	return apiErr
}
