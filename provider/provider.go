// Package provider adapts a WaxSeal HTTP client to WaxTap's potoken.Provider
// interface. It lives in a separate Go module so the rest of WaxSeal does not
// depend on WaxTap.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxtap/potoken"
)

// ErrUnsupportedScope is returned for scopes WaxSeal does not serve. At present,
// only ScopeSubtitles is unsupported.
var ErrUnsupportedScope = errors.New("waxseal/provider: unsupported PO-token scope")

// Provider adapts a *client.Client to potoken.Provider.
type Provider struct {
	c *client.Client
}

var (
	_ potoken.Provider              = (*Provider)(nil)
	_ potoken.PlayerContextProvider = (*Provider)(nil)
)

// New wraps a WaxSeal client as a WaxTap potoken.Provider. Configure
// authentication and HTTP behavior on the client before calling New.
func New(c *client.Client) *Provider { return &Provider{c: c} }

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
	return potoken.Response{Token: tok.Value, ExpiresAt: tok.ExpiresAt}, nil
}

// Session fetches WaxSeal's coherent guest session as a *potoken.Session, ready
// for WaxTap's Options.Session.
func (p *Provider) Session(ctx context.Context) (*potoken.Session, error) {
	s, err := p.c.Session(ctx)
	if err != nil {
		return nil, err
	}
	return &potoken.Session{VisitorData: s.VisitorData, Cookies: s.Cookies}, nil
}

// ProvidePlayerContext fetches the attested WEB player context for videoID and
// maps it to WaxTap's SABR audio context. It rejects incomplete responses before
// WaxTap begins SABR setup.
func (p *Provider) ProvidePlayerContext(ctx context.Context, videoID string) (potoken.PlayerContext, error) {
	pc, err := p.c.PlayerContext(ctx, videoID)
	if err != nil {
		return potoken.PlayerContext{}, err
	}
	if pc.Status != "" && !strings.EqualFold(pc.Status, "OK") {
		return potoken.PlayerContext{}, fmt.Errorf("waxseal/provider: player-context returned status %q", pc.Status)
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
	}, nil
}
