// Package provider adapts a WaxSeal HTTP client to WaxTap's potoken.Provider
// interface. It lives in a separate Go module so the rest of WaxSeal does not
// depend on WaxTap.
//
// Failures are reported as WaxTap's own sidecar error types, and a transient one
// is retried once after a wait, on the rule WaxTap's own HTTP sidecar retries
// and pauses by. A consumer therefore classifies refusals and waits the same
// way whichever of the two adapters it wires. The daemon's own error rides
// in the sidecar error's Cause for a caller that knows this adapter; nothing
// unwraps it, so classification is unchanged, and a body that was not the
// daemon's envelope travels no further than this package.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/colespringer/waxseal/client"
	waxtap "github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/potoken"
)

// Endpoint labels and paths. The label names this adapter in the errors WaxTap
// prints; the path completes the endpoint the daemon was asked at.
const (
	labelPOToken       = "WaxSeal PO-token endpoint"
	labelPlayerContext = "WaxSeal player-context endpoint"
	labelSession       = "WaxSeal session endpoint"
	labelReport        = "WaxSeal report endpoint"

	pathPOToken       = "/get_pot"
	pathPlayerContext = "/player-context"
	pathSession       = "/session"
	pathReport        = "/report"
)

// Text caps on the way into a sidecar error, matching WaxTap's own. An
// APIError.Message can be kilobytes of CDP trace, and these strings print into
// WaxTap's CLI hints.
const (
	reasonRunes = 200
	codeRunes   = 64
)

// sleep waits d or until ctx ends. It is a variable so a test can record the
// wait instead of serving it.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrUnsupportedScope is returned for a scope WaxSeal does not serve, such as
// ScopeSubtitles.
var ErrUnsupportedScope = errors.New("waxseal/provider: unsupported PO-token scope")

// Provider adapts a *client.Client to potoken.Provider.
type Provider struct {
	c   *client.Client
	log *slog.Logger
}

var (
	_ potoken.Provider              = (*Provider)(nil)
	_ potoken.PlayerContextProvider = (*Provider)(nil)
	_ potoken.SessionProvider       = (*Provider)(nil)
	_ potoken.SessionInvalidator    = (*Provider)(nil)
)

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sends the provider's structured logs to l, matching WaxTap's logging
// convention. The provider logs daemon advisories, such as a content_binding that
// looks like a URL, at Warn. A nil logger or no option discards logs.
func WithLogger(l *slog.Logger) Option { return func(p *Provider) { p.log = l } }

// New wraps a WaxSeal client as a WaxTap potoken.Provider. Configure
// authentication and HTTP behavior on the client before calling New. Pass
// WithLogger to surface daemon warnings to WaxTap-mediated callers; without it,
// logs are discarded.
func New(c *client.Client, opts ...Option) *Provider {
	p := &Provider{c: c}
	for _, o := range opts {
		o(p)
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	return p
}

// sidecarErr translates a client failure into the error type WaxTap's own HTTP
// sidecar would have produced, so its classifier, WEB-context skip window, CLI
// hints, and doctor output all work for this adapter unchanged. A nil error
// stays nil.
//
// Only a caller that actually went away gets its error back untranslated, which
// is why this reads ctx rather than the error: net/http reports its own client
// timeout as context.DeadlineExceeded too, and a daemon that is merely slow is a
// transport failure worth one retry, not a caller giving up.
func (p *Provider) sidecarErr(ctx context.Context, label, path string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	endpoint := p.c.BaseURL() + path
	if apiErr, ok := errors.AsType[*client.APIError](err); ok {
		// Only a recognized envelope's text is forwarded, and only its error rides
		// in Cause. Anything else in Message is raw bytes from whatever answered
		// instead, which the client's own documentation says to keep for local
		// diagnosis and not forward; Cause happens not to be printed today, but
		// that is WaxTap's promise rather than something this side can rely on.
		reason := "unrecognized response body"
		var cause error
		if apiErr.Envelope {
			reason, cause = capRunes(apiErr.Message, reasonRunes), apiErr
		}
		return &waxtap.SidecarResponseError{
			Label:      label,
			Endpoint:   endpoint,
			StatusCode: apiErr.StatusCode,
			Code:       capRunes(apiErr.Code, codeRunes),
			Details:    capRunes(apiErr.Details, reasonRunes),
			Reason:     reason,
			RetryAfter: apiErr.RetryAfter,
			Cause:      cause,
		}
	}
	// net.Error covers the *url.Error http.Client.Do wraps a transport failure in,
	// so one check answers both.
	if _, ok := errors.AsType[net.Error](err); ok {
		return &waxtap.SidecarError{Label: label, Endpoint: endpoint, Err: err}
	}
	// The daemon answered, but not with something usable: a decode failure, or one
	// of the shape checks below. A zero status marks a contract mismatch, which is
	// never retried.
	return &waxtap.SidecarResponseError{
		Label:    label,
		Endpoint: endpoint,
		Reason:   capRunes(err.Error(), reasonRunes),
		Cause:    err,
	}
}

// capRunes truncates s to at most n runes, appending an ellipsis when truncated,
// the way WaxTap caps the same fields. It counts runes so a multibyte character
// is never split.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\u2026"
}

// call runs fn and retries it once when WaxTap's own sidecar rule says the
// translated failure earns it, pausing under WaxTap's own policy too: a caller
// that has gone away gets its cancellation, a budget that cannot fit the wait
// gets the refusal now, and a deadline that expires mid-wait gets the refusal
// rather than a bare timeout, since it is what explains the run.
func (p *Provider) call(ctx context.Context, label string, fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}
	wait, retry := waxtap.SidecarRetryWait(err)
	if !retry {
		return err
	}
	if berr := waxtap.PauseBlocked(ctx, wait, err); berr != nil {
		return berr
	}
	p.log.Info("waxseal/provider: retrying once after a wait",
		"endpoint", label, "wait", wait, "code", sidecarCode(err))
	if serr := sleep(ctx, wait); serr != nil {
		return waxtap.PauseInterrupted(serr, err)
	}
	return fn()
}

// sidecarCode is the refusal's code for a log line, empty for anything else.
func sidecarCode(err error) string {
	if sre, ok := errors.AsType[*waxtap.SidecarResponseError](err); ok {
		return sre.Code
	}
	return ""
}

// ProvidePOToken maps a WaxTap scope to a WaxSeal content_binding and mints the
// token. ScopeGVS binds visitor_data, ScopePlayer binds video_id, ScopeNone does
// nothing, and ScopeSubtitles returns ErrUnsupportedScope.
func (p *Provider) ProvidePOToken(ctx context.Context, req potoken.Request) (potoken.Response, error) {
	var binding, scope string
	switch req.Scope {
	case potoken.ScopeNone:
		return potoken.Response{}, nil
	case potoken.ScopeGVS:
		binding, scope = req.VisitorData, "gvs"
	case potoken.ScopePlayer:
		binding, scope = req.VideoID, "player"
	default: // ScopeSubtitles or unknown
		return potoken.Response{}, fmt.Errorf("%w: %s", ErrUnsupportedScope, req.Scope)
	}
	var tok client.Token
	err := p.call(ctx, labelPOToken, func() error {
		var err error
		tok, err = p.c.POToken(ctx, binding, scope)
		return p.sidecarErr(ctx, labelPOToken, pathPOToken, err)
	})
	if err != nil {
		return potoken.Response{}, err
	}
	if tok.Warning != "" {
		// Surface the daemon's advisory (for example, a content_binding that looks
		// like a URL) to WaxTap-mediated callers, who otherwise never see it.
		p.log.Warn("waxseal/provider: daemon warning", "scope", scope, "warning", tok.Warning)
	}
	return potoken.Response{Token: tok.Value, ExpiresAt: tok.ExpiresAt}, nil
}

// Session fetches WaxSeal's coherent guest session as a *potoken.Session, ready
// for WaxTap's Options.Session.
//
// Prefer ProvideSession and Options.SessionProvider. WaxTap reaches
// InvalidateSession through the configured SessionProvider, so a session adopted
// by value here is the one arm a delivery cap cannot rotate: WaxTap holds it for
// the Client's lifetime with nowhere to report it.
func (p *Provider) Session(ctx context.Context) (*potoken.Session, error) {
	var s *client.Session
	err := p.call(ctx, labelSession, func() error {
		var err error
		s, err = p.c.Session(ctx)
		return p.sidecarErr(ctx, labelSession, pathSession, err)
	})
	if err != nil {
		return nil, err
	}
	// The identity travels whole: WaxTap applies the user agent and client version
	// to every WEB request made under this session, so the cookies, the visitor id,
	// and the requests present one browser.
	return &potoken.Session{
		VisitorData:   s.VisitorData,
		Cookies:       s.Cookies,
		UserAgent:     s.UserAgent,
		ClientVersion: s.ClientVersion,
		Generation:    s.SessionGeneration,
	}, nil
}

// ProvideSession is the pull-based form of Session, for WaxTap's
// Options.SessionProvider. It is what makes the session arm's delivery-cap
// escape work: WaxTap type-asserts SessionInvalidator on the SessionProvider it
// was given, so only a session adopted through here can be reported and
// replaced. The generation travels with the session to name it in that report.
func (p *Provider) ProvideSession(ctx context.Context) (potoken.Session, error) {
	s, err := p.Session(ctx)
	if err != nil {
		return potoken.Session{}, err
	}
	return *s, nil
}

// previewMax bounds a rejected field echoed into a log line.
const previewMax = 64

// preview renders a rejected field for diagnosis. What made it unreportable is
// usually invisible in a length (a stray space, a colon, a newline), so the text
// itself has to appear. It is bounded rather than escaped: slog's handlers
// already quote a value carrying spaces or control characters, so the log stays
// one line without this adding a second layer of quoting over the string a
// reader is trying to look at. The cut is by rune, so it cannot leave a mangled
// one behind.
func preview(s string) string {
	r := []rune(s)
	if len(r) > previewMax {
		return string(r[:previewMax]) + "..."
	}
	return s
}

// InvalidateSession reports the session named by inv to the daemon, which
// retires it so the next Session or ProvidePlayerContext call comes from a fresh
// one. WaxTap calls this when googlevideo caps delivery on a session, which no
// re-resolve under the same identity escapes.
//
// A nil error means the named session is gone. That covers a report the daemon
// rejects as stale or already retired, because the session it named is exactly
// what the caller wanted removed, and a retirement deferred to the next handoff,
// because the handoff is the next Session or ProvidePlayerContext call. Only a
// rate-limited report is an error: the daemon is asking for backoff and the
// current session survives it, so reporting success would hand WaxTap the same
// session back.
//
// video_id and reason are diagnostics, and the daemon constrains the shape of
// both. Neither is allowed to decide whether a capped session gets retired, so a
// report the daemon rejects as malformed is retried naming only the generation.
// Asking the daemon rather than pre-screening against a copy of its rules is
// what keeps the two from drifting apart.
func (p *Provider) InvalidateSession(ctx context.Context, inv potoken.SessionInvalidation) error {
	if inv.Generation == 0 {
		return errors.New("waxseal/provider: no session generation to report; the session or player-context response carried none")
	}
	// A report is sent once, so it is deliberately not routed through call: the
	// one resend below is about dropping a rejected diagnostic, not about waiting
	// out a transient failure.
	res, err := p.c.Report(ctx, inv.Generation, inv.VideoID, inv.Reason)
	if err != nil {
		if !rejectedDiagnostics(err, inv) {
			return p.sidecarErr(ctx, labelReport, pathReport, err)
		}
		p.log.Warn("waxseal/provider: daemon rejected the report's diagnostics; retrying without them",
			"video_id", preview(inv.VideoID), "reason", preview(inv.Reason), "err", err)
		if res, err = p.c.Report(ctx, inv.Generation, "", ""); err != nil {
			return p.sidecarErr(ctx, labelReport, pathReport, err)
		}
	}
	if res.RetryAfterSeconds > 0 {
		// The daemon accepted the request and refused the recycle, so this is a
		// refusal with a wait, shaped like every other one WaxTap reads.
		return &waxtap.SidecarResponseError{
			Label:      labelReport,
			Endpoint:   p.c.BaseURL() + pathReport,
			Reason:     "session recycling is rate-limited",
			RetryAfter: time.Duration(res.RetryAfterSeconds) * time.Second,
		}
	}
	return nil
}

// rejectedDiagnostics reports whether err is the daemon refusing the report over
// a diagnostic field, which a second attempt can drop and still get the session
// retired. It requires that there was a diagnostic to blame: with neither field
// set, the bare report is all that was sent and a 400 means something a retry
// cannot fix, so resending it would only cost a round trip.
func rejectedDiagnostics(err error, inv potoken.SessionInvalidation) bool {
	if inv.VideoID == "" && inv.Reason == "" {
		return false
	}
	apiErr, ok := errors.AsType[*client.APIError](err)
	return ok && apiErr.StatusCode == http.StatusBadRequest
}

// ProvidePlayerContext fetches the attested WEB player context for videoID and
// maps it to WaxTap's SABR audio context. It rejects incomplete responses before
// WaxTap begins SABR setup.
func (p *Provider) ProvidePlayerContext(ctx context.Context, videoID string) (potoken.PlayerContext, error) {
	var pc *client.PlayerContext
	err := p.call(ctx, labelPlayerContext, func() error {
		var err error
		pc, err = p.c.PlayerContext(ctx, videoID)
		return p.sidecarErr(ctx, labelPlayerContext, pathPlayerContext, err)
	})
	if err != nil {
		return potoken.PlayerContext{}, err
	}
	if pc.PlayabilityStatus != "" && !strings.EqualFold(pc.PlayabilityStatus, "OK") {
		// A 200 that names a non-OK status is the same verdict a 422
		// video-unavailable carries, so code it the same way and let WaxTap unwrap
		// both to the playability verdict.
		return potoken.PlayerContext{}, &waxtap.SidecarResponseError{
			Label:    labelPlayerContext,
			Endpoint: p.c.BaseURL() + pathPlayerContext,
			Code:     waxtap.SidecarCodeVideoUnavailable,
			Details:  capRunes(pc.PlayabilityStatus, reasonRunes),
			Reason:   fmt.Sprintf("playability_status %q", capRunes(pc.PlayabilityStatus, reasonRunes)),
		}
	}
	if pc.ServerAbrStreamingURL == "" || pc.PlayerURL == "" || pc.VisitorData == "" || pc.VideoPlaybackUstreamerConfig == "" || len(pc.AudioFormats) == 0 {
		return potoken.PlayerContext{}, p.sidecarErr(ctx, labelPlayerContext, pathPlayerContext,
			errors.New("player-context missing server_abr_streaming_url, player_url, visitor_data, video_playback_ustreamer_config, or audio_formats"))
	}

	formats := make([]potoken.PlayerContextFormat, 0, len(pc.AudioFormats))
	for _, f := range pc.AudioFormats {
		formats = append(formats, potoken.PlayerContextFormat{
			Itag:             f.Itag,
			LMT:              f.LMT,
			XTags:            f.XTags,
			MimeType:         f.MimeType,
			Bitrate:          f.Bitrate,
			AudioQuality:     f.AudioQuality,
			AudioChannels:    f.AudioChannels,
			AudioSampleRate:  f.AudioSampleRate,
			ContentLength:    f.ContentLength,
			ApproxDurationMs: int64(f.ApproxDurationMs),
			IsDrc:            f.IsDrc,
			AudioTrackID:     f.AudioTrackID,
		})
	}
	// Allocate the ladder only when the response carried rungs, so an absent or
	// empty thumbnails key leaves Thumbnails nil. A rung without a URL is dropped,
	// matching WaxTap's own sidecar. The order is the response's own, smallest
	// first; WaxTap sorts it itself.
	var thumbs []potoken.PlayerContextThumbnail
	for _, t := range pc.Thumbnails {
		if t.URL == "" {
			continue
		}
		thumbs = append(thumbs, potoken.PlayerContextThumbnail{URL: t.URL, Width: t.Width, Height: t.Height})
	}
	return potoken.PlayerContext{
		ServerAbrURL:    pc.ServerAbrStreamingURL,
		PlayerURL:       pc.PlayerURL,
		UstreamerConfig: pc.VideoPlaybackUstreamerConfig,
		VisitorData:     pc.VisitorData,
		UserAgent:       pc.UserAgent,
		ClientVersion:   pc.ClientVersion,
		Title:           pc.Title,
		Author:          pc.Author,
		LengthSeconds:   pc.LengthSeconds,
		ChannelID:       pc.ChannelID,
		Description:     pc.Description,
		Thumbnails:      thumbs,
		IsLiveContent:   pc.IsLiveContent,
		IsLiveNow:       pc.IsLiveNow,
		IsUpcoming:      pc.IsUpcoming,
		PublishDate:     pc.PublishDate,
		AudioFormats:    formats,
		Generation:      pc.SessionGeneration,
	}, nil
}
