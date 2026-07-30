// Package provider adapts a WaxSeal HTTP client to WaxTap's potoken.Provider
// interface. It lives in a separate Go module so the rest of WaxSeal does not
// depend on WaxTap.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxtap/v3/potoken"
)

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
	tok, err := p.c.POToken(ctx, binding, scope)
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
	s, err := p.c.Session(ctx)
	if err != nil {
		return nil, err
	}
	return &potoken.Session{VisitorData: s.VisitorData, Cookies: s.Cookies, Generation: s.SessionGeneration}, nil
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
	res, err := p.c.Report(ctx, inv.Generation, inv.VideoID, inv.Reason)
	if err != nil {
		if !rejectedDiagnostics(err, inv) {
			return err
		}
		p.log.Warn("waxseal/provider: daemon rejected the report's diagnostics; retrying without them",
			"video_id", preview(inv.VideoID), "reason", preview(inv.Reason), "err", err)
		if res, err = p.c.Report(ctx, inv.Generation, "", ""); err != nil {
			return err
		}
	}
	if res.RetryAfterSeconds > 0 {
		return fmt.Errorf("waxseal/provider: session recycling is rate-limited; retry in %ds", res.RetryAfterSeconds)
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
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest
}

// ProvidePlayerContext fetches the attested WEB player context for videoID and
// maps it to WaxTap's SABR audio context. It rejects incomplete responses before
// WaxTap begins SABR setup.
func (p *Provider) ProvidePlayerContext(ctx context.Context, videoID string) (potoken.PlayerContext, error) {
	pc, err := p.c.PlayerContext(ctx, videoID)
	if err != nil {
		return potoken.PlayerContext{}, err
	}
	if pc.PlayabilityStatus != "" && !strings.EqualFold(pc.PlayabilityStatus, "OK") {
		return potoken.PlayerContext{}, fmt.Errorf("waxseal/provider: player-context returned playability status %q", pc.PlayabilityStatus)
	}
	if pc.ServerAbrStreamingURL == "" || pc.PlayerURL == "" || pc.VisitorData == "" || pc.VideoPlaybackUstreamerConfig == "" || len(pc.AudioFormats) == 0 {
		return potoken.PlayerContext{}, fmt.Errorf("waxseal/provider: player-context missing server_abr_streaming_url, player_url, visitor_data, video_playback_ustreamer_config, or audio_formats")
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
	return potoken.PlayerContext{
		ServerAbrURL:    pc.ServerAbrStreamingURL,
		PlayerURL:       pc.PlayerURL,
		UstreamerConfig: pc.VideoPlaybackUstreamerConfig,
		VisitorData:     pc.VisitorData,
		ClientVersion:   pc.ClientVersion,
		Title:           pc.Title,
		Author:          pc.Author,
		LengthSeconds:   pc.LengthSeconds,
		AudioFormats:    formats,
		Generation:      pc.SessionGeneration,
	}, nil
}
