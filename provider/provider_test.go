package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
	waxtap "github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/potoken"
)

func newProvider(h http.HandlerFunc) (*provider.Provider, func()) {
	srv := httptest.NewServer(h)
	return provider.New(client.New(srv.URL)), srv.Close
}

func TestProvideScopeMapping(t *testing.T) {
	var gotBinding, gotScope string
	p, done := newProvider(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ContentBinding string `json:"content_binding"`
			Scope          string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotBinding, gotScope = req.ContentBinding, req.Scope
		_ = json.NewEncoder(w).Encode(map[string]any{"poToken": "TOK-" + req.Scope})
	})
	defer done()
	ctx := context.Background()

	r, err := p.ProvidePOToken(ctx, potoken.Request{Scope: potoken.ScopeGVS, VisitorData: "VD"})
	if err != nil || r.Token != "TOK-gvs" || gotBinding != "VD" || gotScope != "gvs" {
		t.Fatalf("gvs: token=%q binding=%q scope=%q err=%v", r.Token, gotBinding, gotScope, err)
	}
	r, err = p.ProvidePOToken(ctx, potoken.Request{Scope: potoken.ScopePlayer, VideoID: "VID"})
	if err != nil || r.Token != "TOK-player" || gotBinding != "VID" || gotScope != "player" {
		t.Errorf("player: token=%q binding=%q scope=%q err=%v", r.Token, gotBinding, gotScope, err)
	}
}

func TestProvideNoneAndUnsupported(t *testing.T) {
	called := false
	p, done := newProvider(func(http.ResponseWriter, *http.Request) { called = true })
	defer done()
	ctx := context.Background()

	if r, err := p.ProvidePOToken(ctx, potoken.Request{Scope: potoken.ScopeNone}); err != nil || r.Token != "" {
		t.Errorf("none: token=%q err=%v", r.Token, err)
	}
	if called {
		t.Error("ScopeNone must not call the daemon")
	}
	if _, err := p.ProvidePOToken(ctx, potoken.Request{Scope: potoken.ScopeSubtitles, VideoID: "v"}); !errors.Is(err, provider.ErrUnsupportedScope) {
		t.Errorf("subtitles err = %v, want ErrUnsupportedScope", err)
	}
}

// TestProvideLogsDaemonWarning checks that a non-empty warning from /get_pot
// reaches a WaxTap-mediated caller through the provider's logger, that no warning
// field logs nothing, and that the default (no WithLogger) discards safely.
func TestProvideLogsDaemonWarning(t *testing.T) {
	logged := func(body map[string]any) (p *provider.Provider, buf *bytes.Buffer, done func()) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(body)
		}))
		buf = &bytes.Buffer{}
		p = provider.New(client.New(srv.URL), provider.WithLogger(slog.New(slog.NewTextHandler(buf, nil))))
		return p, buf, srv.Close
	}

	t.Run("warning surfaces through the logger", func(t *testing.T) {
		p, buf, done := logged(map[string]any{"poToken": "TOK", "warning": "content_binding looks like a URL"})
		defer done()
		if _, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopePlayer, VideoID: "https://youtube.com/watch?v=x"}); err != nil {
			t.Fatalf("ProvidePOToken: %v", err)
		}
		if !strings.Contains(buf.String(), "content_binding looks like a URL") {
			t.Errorf("log = %q, want the daemon warning surfaced", buf.String())
		}
	})

	t.Run("no warning logs nothing", func(t *testing.T) {
		p, buf, done := logged(map[string]any{"poToken": "TOK"})
		defer done()
		if _, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopeGVS, VisitorData: "VD"}); err != nil {
			t.Fatalf("ProvidePOToken: %v", err)
		}
		if buf.Len() != 0 {
			t.Errorf("log = %q, want silence when the daemon returns no warning", buf.String())
		}
	})

	t.Run("default logger discards a warning without panicking", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"poToken": "TOK", "warning": "content_binding looks like a URL"})
		}))
		defer srv.Close()
		p := provider.New(client.New(srv.URL)) // no WithLogger
		r, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopePlayer, VideoID: "v"})
		if err != nil || r.Token != "TOK" {
			t.Fatalf("token=%q err=%v, want TOK with the nil-logger default", r.Token, err)
		}
	})
}

func TestProvidePlayerContextMapping(t *testing.T) {
	var gotVideoID string
	p, done := newProvider(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			VideoID string `json:"video_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotVideoID = req.VideoID
		_ = json.NewEncoder(w).Encode(map[string]any{
			"playability_status":              "OK",
			"player_url":                      "https://www.youtube.com/s/player/abc/base.js",
			"server_abr_streaming_url":        "https://r1.googlevideo.com/videoplayback?n=scram",
			"video_playback_ustreamer_config": "USTREAMER",
			"visitor_data":                    "VD",
			"client_version":                  "2.0",
			"title":                           "Big Buck Bunny",
			"author":                          "Blender",
			"length_seconds":                  634,
			"user_agent":                      "Mozilla/5.0 (X11; Linux x86_64) Chrome/149.0.0.0",
			"session_generation":              7,
			"channel_id":                      "UCabc",
			"description":                     "line one\nline two",
			"is_live_content":                 true,
			"is_live_now":                     true,
			"is_upcoming":                     true,
			"publish_date":                    "2015-04-10T00:00:00-07:00",
			"thumbnails": []map[string]any{
				{"url": "https://i.ytimg.com/vi/VID/default.jpg", "width": 120, "height": 90},
				{"url": "", "width": 320, "height": 180},
				{"url": "https://i.ytimg.com/vi/VID/maxresdefault.jpg", "width": 1280, "height": 720},
			},
			"audio_formats": []map[string]any{{
				"itag": 251, "lmt": "171", "xtags": "X", "mime_type": "audio/webm", "bitrate": 130000,
				"content_length": 1234, "approx_duration_ms": 634000, "audio_sample_rate": 48000,
				"audio_channels": 2, "audio_quality": "AUDIO_QUALITY_MEDIUM",
				"is_drc": true, "audio_track_id": "en.4",
			}},
		})
	})
	defer done()

	pc, err := p.ProvidePlayerContext(context.Background(), "VID")
	if err != nil {
		t.Fatalf("ProvidePlayerContext: %v", err)
	}
	if gotVideoID != "VID" {
		t.Errorf("video_id = %q, want VID", gotVideoID)
	}
	if pc.ServerAbrURL != "https://r1.googlevideo.com/videoplayback?n=scram" || pc.PlayerURL == "" ||
		pc.UstreamerConfig != "USTREAMER" || pc.VisitorData != "VD" || pc.ClientVersion != "2.0" {
		t.Fatalf("context = %+v", pc)
	}
	if pc.Title != "Big Buck Bunny" || pc.Author != "Blender" || pc.LengthSeconds != 634 {
		t.Errorf("metadata: title=%q author=%q len=%d", pc.Title, pc.Author, pc.LengthSeconds)
	}
	// The identity the URL was issued to has to travel with it, or WaxTap streams
	// the context under a user agent the daemon never minted it for.
	if pc.UserAgent != "Mozilla/5.0 (X11; Linux x86_64) Chrome/149.0.0.0" {
		t.Errorf("user agent = %q, want the one the context was minted under", pc.UserAgent)
	}
	// Without the generation WaxTap cannot name this context's session in a report,
	// so a capped stream would have no escape.
	if pc.Generation != 7 {
		t.Errorf("generation = %d, want 7", pc.Generation)
	}
	if pc.ChannelID != "UCabc" || pc.Description != "line one\nline two" || pc.PublishDate != "2015-04-10T00:00:00-07:00" {
		t.Errorf("metadata: channel=%q description=%q publish=%q", pc.ChannelID, pc.Description, pc.PublishDate)
	}
	// All three are set in the fixture, so a dropped mapping fails here. A fixture
	// of false would pass whether or not the field was ever read.
	if !pc.IsLiveContent || !pc.IsLiveNow || !pc.IsUpcoming {
		t.Errorf("live flags = %v/%v/%v, want all true", pc.IsLiveContent, pc.IsLiveNow, pc.IsUpcoming)
	}
	// The ladder keeps the response's own order (WaxTap sorts it itself), and a
	// rung with no URL is dropped rather than handed on as an unusable entry.
	if len(pc.Thumbnails) != 2 {
		t.Fatalf("thumbnails = %+v, want the two rungs that carry a URL", pc.Thumbnails)
	}
	if pc.Thumbnails[0].Width != 120 || pc.Thumbnails[0].Height != 90 ||
		pc.Thumbnails[1].Width != 1280 || pc.Thumbnails[1].Height != 720 {
		t.Errorf("thumbnails = %+v, want the wire order with widths and heights", pc.Thumbnails)
	}
	if len(pc.AudioFormats) != 1 {
		t.Fatalf("audio formats = %d, want 1", len(pc.AudioFormats))
	}
	f := pc.AudioFormats[0]
	if f.Itag != 251 || f.LMT != "171" || f.XTags != "X" || f.MimeType != "audio/webm" || f.Bitrate != 130000 {
		t.Errorf("format core = %+v", f)
	}
	if f.ContentLength != 1234 || f.ApproxDurationMs != 634000 || f.AudioSampleRate != 48000 ||
		f.AudioChannels != 2 || f.AudioQuality != "AUDIO_QUALITY_MEDIUM" {
		t.Errorf("format detail = %+v", f)
	}
	// These fields are required by SABR setup and must survive both mappings.
	if !f.IsDrc || f.AudioTrackID != "en.4" {
		t.Errorf("DRC/track fields dropped: is_drc=%v audio_track_id=%q", f.IsDrc, f.AudioTrackID)
	}
}

// TestProvidePlayerContextRejects starts from an otherwise-complete context so each
// mutation is the sole reason the provider's stricter SABR validation rejects it.
func TestProvidePlayerContextRejects(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{
			"playability_status":              "OK",
			"player_url":                      "https://www.youtube.com/s/player/abc/base.js",
			"server_abr_streaming_url":        "https://r1.googlevideo.com/videoplayback?n=s",
			"video_playback_ustreamer_config": "U",
			"visitor_data":                    "VD",
			"audio_formats":                   []map[string]any{{"itag": 251}},
		}
	}
	tests := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"non-ok status", func(m map[string]any) { m["playability_status"] = "LOGIN_REQUIRED" }},
		{"missing player_url", func(m map[string]any) { delete(m, "player_url") }},
		{"missing visitor_data", func(m map[string]any) { delete(m, "visitor_data") }},
		{"missing ustreamer config", func(m map[string]any) { delete(m, "video_playback_ustreamer_config") }},
		{"no audio formats", func(m map[string]any) { m["audio_formats"] = []map[string]any{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := full()
			tt.mutate(body)
			p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(body)
			})
			defer done()
			if _, err := p.ProvidePlayerContext(context.Background(), "VID"); err == nil {
				t.Fatalf("expected rejection for %q, got nil", tt.name)
			}
		})
	}
}

func TestSessionAdapts(t *testing.T) {
	p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"visitor_data":       "VD",
			"user_agent":         "Mozilla/5.0 (X11; Linux x86_64) Chrome/149.0.0.0",
			"client_version":     "2.0",
			"cookies":            []map[string]any{{"name": "YSC", "value": "a", "secure": true, "http_only": true}},
			"session_generation": 4,
		})
	})
	defer done()

	s, err := p.Session(context.Background())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if s.VisitorData != "VD" || len(s.Cookies) != 1 || s.Cookies[0].Name != "YSC" {
		t.Fatalf("session = %+v", s)
	}
	// WaxTap applies both to every WEB request under this session, so the cookies,
	// the visitor id, and the requests present one browser.
	if s.UserAgent != "Mozilla/5.0 (X11; Linux x86_64) Chrome/149.0.0.0" || s.ClientVersion != "2.0" {
		t.Errorf("identity: user_agent=%q client_version=%q", s.UserAgent, s.ClientVersion)
	}
	// The generation is what a later InvalidateSession names, so an adopted session
	// googlevideo caps can be retired instead of stranding the download.
	if s.Generation != 4 {
		t.Errorf("generation = %d, want 4", s.Generation)
	}
}

// reportRequest is the /report body the daemon receives.
type reportRequest struct {
	SessionGeneration uint64 `json:"session_generation"`
	VideoID           string `json:"video_id"`
	Reason            string `json:"reason"`
}

// TestInvalidateSessionOutcomes pins the mapping from the daemon's /report reply
// to what WaxTap concludes. A nil error tells WaxTap the session is gone and it
// may re-resolve; an error tells it to keep the one it has.
func TestInvalidateSessionOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		reply   map[string]any
		wantErr bool
	}{
		// The daemon closed the session immediately.
		{"retired", map[string]any{"accepted": true, "retired": true, "generation": 9}, false},
		// Queued for the next streaming handoff, which is the next /session or
		// /player-context call, so the replacement still arrives before WaxTap uses it.
		{"retirement pending", map[string]any{"accepted": true, "retirement_pending": true, "generation": 9}, false},
		// Rejected as stale: the session named is already gone, which is the outcome
		// the caller asked for.
		{"stale generation", map[string]any{"accepted": false, "generation": 12}, false},
		// The daemon is asking for backoff and the current session survives, so
		// reporting success would hand WaxTap the same capped session back.
		{"rate limited", map[string]any{"accepted": false, "generation": 9, "retry_after_seconds": 20}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got reportRequest
			p, done := newProvider(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				_ = json.NewEncoder(w).Encode(tt.reply)
			})
			defer done()

			err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{
				Generation: 9, VideoID: "VID", Reason: "delivery-cap",
			})
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got.SessionGeneration != 9 || got.VideoID != "VID" || got.Reason != "delivery-cap" {
				t.Errorf("report body = %+v, want generation 9, video VID, reason delivery-cap", got)
			}
		})
	}
}

// A daemon that cannot be reached leaves the session in place rather than
// reporting a retirement that never happened.
func TestInvalidateSessionDaemonError(t *testing.T) {
	p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer done()

	if err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{Generation: 9}); err == nil {
		t.Fatal("want an error when the daemon rejects the report")
	}
}

// An unversioned session cannot be named in a report, so the call fails locally
// rather than posting a request the daemon would reject.
func TestInvalidateSessionWithoutGeneration(t *testing.T) {
	called := false
	p, done := newProvider(func(http.ResponseWriter, *http.Request) { called = true })
	defer done()

	if err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{VideoID: "VID"}); err == nil {
		t.Fatal("want an error for a zero generation")
	}
	if called {
		t.Error("a zero generation must not reach the daemon")
	}
}

// strictReportServer mimics the daemon's /report validation: it 400s a request
// carrying a video_id or reason outside ^[A-Za-z0-9_-]{1,64}$, and records every
// body it received.
func strictReportServer(t *testing.T, got *[]reportRequest) (*httptest.Server, *bytes.Buffer, *provider.Provider) {
	t.Helper()
	ok := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req reportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		*got = append(*got, req)
		for _, f := range []string{req.VideoID, req.Reason} {
			if f != "" && !ok.MatchString(f) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad field", "code": "invalid-request"})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "retired": true, "generation": 9})
	}))
	buf := &bytes.Buffer{}
	return srv, buf, provider.New(client.New(srv.URL), provider.WithLogger(slog.New(slog.NewTextHandler(buf, nil))))
}

// A diagnostic the daemon refuses must not decide whether a capped session is
// retired. Rather than pre-screening against a copy of the daemon's rules, the
// provider lets it answer and retries naming only the generation.
func TestInvalidateSessionRetriesWithoutRejectedDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		inv  potoken.SessionInvalidation
	}{
		{"reason", potoken.SessionInvalidation{Generation: 9, Reason: "capped: 403 past 1 MB"}},
		{"video id", potoken.SessionInvalidation{Generation: 9, VideoID: "https://youtu.be/x", Reason: "delivery-cap"}},
		{"both", potoken.SessionInvalidation{Generation: 9, VideoID: "bad id", Reason: "bad reason"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []reportRequest
			srv, buf, p := strictReportServer(t, &got)
			defer srv.Close()

			if err := p.InvalidateSession(context.Background(), tt.inv); err != nil {
				t.Fatalf("InvalidateSession: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("daemon saw %d reports, want 2 (rejected then bare)", len(got))
			}
			if got[1].SessionGeneration != 9 || got[1].VideoID != "" || got[1].Reason != "" {
				t.Errorf("retry body = %+v, want generation 9 alone", got[1])
			}
			// The offending text is the diagnosis; a length would not show a stray
			// space or colon.
			log := buf.String()
			if tt.inv.Reason != "" && !strings.Contains(log, tt.inv.Reason) {
				t.Errorf("log = %q, want the rejected reason text", log)
			}
			if tt.inv.VideoID != "" && !strings.Contains(log, tt.inv.VideoID) {
				t.Errorf("log = %q, want the rejected video_id text", log)
			}
		})
	}
}

// A 400 on a report that named no diagnostic cannot be fixed by dropping them,
// so it surfaces instead of costing a second round trip.
func TestInvalidateSessionBareRejectionIsNotRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "nope", "code": "invalid-request"})
	}))
	defer srv.Close()
	p := provider.New(client.New(srv.URL))

	if err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{Generation: 9}); err == nil {
		t.Fatal("want the daemon's rejection to surface")
	}
	if hits != 1 {
		t.Errorf("daemon saw %d reports, want 1 (nothing to drop and retry)", hits)
	}
}

// A rejection that is not the daemon refusing a field surfaces as-is: dropping
// diagnostics cannot fix a 500, and retrying would double the load on a daemon
// already failing.
func TestInvalidateSessionNonBadRequestIsNotRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := provider.New(client.New(srv.URL))

	err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{
		Generation: 9, VideoID: "VID", Reason: "delivery-cap",
	})
	if err == nil {
		t.Fatal("want the daemon error to surface")
	}
	if hits != 1 {
		t.Errorf("daemon saw %d reports, want 1", hits)
	}
}

// Echoing a rejected field back must not let it forge log lines. slog's handlers
// escape it, so this pins the end-to-end property rather than the mechanism.
func TestInvalidateSessionPreviewStaysOneLine(t *testing.T) {
	var got []reportRequest
	srv, buf, p := strictReportServer(t, &got)
	defer srv.Close()

	if err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{
		Generation: 9, Reason: "capped\nlevel=ERROR msg=spoofed",
	}); err != nil {
		t.Fatalf("InvalidateSession: %v", err)
	}
	log := buf.String()
	if strings.Count(log, "\n") != 1 {
		t.Errorf("log = %q, want a single line (the newline must arrive escaped)", log)
	}
	if !strings.Contains(log, `\n`) {
		t.Errorf("log = %q, want the newline escaped rather than dropped", log)
	}
}

// A long rejected field is truncated so one report cannot flood the log.
func TestInvalidateSessionPreviewTruncates(t *testing.T) {
	var got []reportRequest
	srv, buf, p := strictReportServer(t, &got)
	defer srv.Close()

	long := strings.Repeat("é", 300) // multi-byte, so a byte-wise cut would mangle it
	if err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{
		Generation: 9, Reason: long,
	}); err != nil {
		t.Fatalf("InvalidateSession: %v", err)
	}
	log := buf.String()
	if strings.Contains(log, long) {
		t.Error("log carried the whole reason, want it truncated")
	}
	if !strings.Contains(log, "...") {
		t.Errorf("log = %q, want a truncated preview", log)
	}
	if strings.ContainsRune(log, '�') {
		t.Errorf("log = %q, want no replacement rune from a mid-rune cut", log)
	}
}

// ProvideSession is the arm WaxTap can invalidate: it type-asserts
// SessionInvalidator on the SessionProvider it was configured with, so a session
// adopted any other way cannot be rotated when googlevideo caps it.
func TestProvideSessionCarriesGeneration(t *testing.T) {
	p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"visitor_data":       "VD",
			"user_agent":         "UA",
			"client_version":     "2.0",
			"cookies":            []map[string]any{{"name": "YSC", "value": "a"}},
			"session_generation": 4,
		})
	})
	defer done()

	s, err := p.ProvideSession(context.Background())
	if err != nil {
		t.Fatalf("ProvideSession: %v", err)
	}
	if s.VisitorData != "VD" || len(s.Cookies) != 1 || s.Generation != 4 {
		t.Fatalf("session = %+v, want VD with one cookie and generation 4", s)
	}
	// Both are mapped here too, so this test fails on a dropped mapping rather
	// than only TestSessionAdapts. Asserting empty against an empty fixture would
	// pass whether or not the fields were read.
	if s.UserAgent != "UA" || s.ClientVersion != "2.0" {
		t.Errorf("identity = %q/%q, want the daemon's", s.UserAgent, s.ClientVersion)
	}
	// The pairing is the point: a provider WaxTap can pull from must also be one it
	// can report to.
	var sp potoken.SessionProvider = p
	if _, ok := sp.(potoken.SessionInvalidator); !ok {
		t.Fatal("the session provider must also be a SessionInvalidator")
	}
}

func TestProvideSessionPropagatesError(t *testing.T) {
	p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer done()

	if _, err := p.ProvideSession(context.Background()); err == nil {
		t.Fatal("want the daemon error to surface")
	}
}

// sidecarErr holds the *waxtap.SidecarResponseError inside err, or fails.
func sidecarResponseErr(t *testing.T, err error) *waxtap.SidecarResponseError {
	t.Helper()
	sre, ok := errors.AsType[*waxtap.SidecarResponseError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *waxtap.SidecarResponseError", err, err)
	}
	return sre
}

// The adapter speaks WaxTap's own sidecar error types, so WaxTap's classifier,
// its WEB-context skip window, its CLI hints, and its doctor output all work
// through this provider unchanged.
func TestProviderTranslatesRefusals(t *testing.T) {
	t.Run("video-unavailable classifies as the playability verdict", func(t *testing.T) {
		p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   `video unplayable: This video is private (playabilityStatus "LOGIN_REQUIRED")`,
				"code":    "video-unavailable",
				"details": "LOGIN_REQUIRED",
			})
		})
		defer done()

		_, err := p.ProvidePlayerContext(context.Background(), "VID")
		sre := sidecarResponseErr(t, err)
		if sre.Code != waxtap.SidecarCodeVideoUnavailable || sre.Details != "LOGIN_REQUIRED" || sre.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("error = %+v", sre)
		}
		if !strings.Contains(sre.Reason, "This video is private") {
			t.Errorf("reason = %q, want the daemon's own text", sre.Reason)
		}
		if !errors.Is(err, waxtap.ErrVideoRestricted) {
			t.Errorf("err = %v, want it to classify as a playability verdict", err)
		}
		// The endpoint is redacted by the error's own Error method.
		if strings.Contains(err.Error(), "?") {
			t.Errorf("error text = %q, want the endpoint redacted", err)
		}
	})

	t.Run("a refusal about the daemon states its wait and is not a verdict", func(t *testing.T) {
		p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "61")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "player-context failed: browser session hit a bot check", "code": "player-context-failed",
				"retry_after_seconds": 61,
			})
		})
		defer done()

		_, err := p.ProvidePlayerContext(context.Background(), "VID")
		sre := sidecarResponseErr(t, err)
		if sre.RetryAfter != 61*time.Second {
			t.Errorf("RetryAfter = %v, want 61s", sre.RetryAfter)
		}
		if sre.Code != "player-context-failed" {
			t.Errorf("code = %q", sre.Code)
		}
		if errors.Is(err, waxtap.ErrVideoUnavailable) {
			t.Error("a refusal about the daemon classified as a verdict on the video")
		}
	})

	t.Run("a long reason is capped", func(t *testing.T) {
		long := strings.Repeat("é", 4<<10)
		p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": long, "code": "player-context-failed"})
		})
		defer done()

		_, err := p.ProvidePlayerContext(context.Background(), "VID")
		sre := sidecarResponseErr(t, err)
		if n := len([]rune(sre.Reason)); n != 201 { // 200 runes plus the ellipsis
			t.Errorf("reason runes = %d, want 200 plus an ellipsis", n)
		}
		if !strings.HasSuffix(sre.Reason, "…") {
			t.Errorf("reason = %q, want it to end in an ellipsis", sre.Reason)
		}
	})

	t.Run("an unreachable daemon is a transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := srv.URL
		srv.Close() // nothing is listening now
		p := provider.New(client.New(addr))

		_, err := p.ProvidePlayerContext(context.Background(), "VID")
		if _, ok := errors.AsType[*waxtap.SidecarError](err); !ok {
			t.Fatalf("err = %v (%T), want a *waxtap.SidecarError", err, err)
		}
	})

	t.Run("a cancelled caller gets its own error, not an unreachable daemon", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
		})
		defer done()
		defer close(release)

		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		_, err := p.ProvidePlayerContext(ctx, "VID")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
		if _, ok := errors.AsType[*waxtap.SidecarError](err); ok {
			t.Error("a departed caller was reported as an unreachable daemon")
		}
	})

	t.Run("a 200 naming a non-OK status is the same verdict a 422 carries", func(t *testing.T) {
		p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"playability_status": "ERROR", "player_url": "https://p/base.js",
				"server_abr_streaming_url": "https://r/v", "video_playback_ustreamer_config": "U",
				"visitor_data": "VD", "audio_formats": []map[string]any{{"itag": 251}},
			})
		})
		defer done()

		_, err := p.ProvidePlayerContext(context.Background(), "VID")
		sre := sidecarResponseErr(t, err)
		if sre.Code != waxtap.SidecarCodeVideoUnavailable || sre.Details != "ERROR" {
			t.Errorf("error = %+v, want the video-unavailable code with the status in details", sre)
		}
		if sre.StatusCode != 0 {
			t.Errorf("status = %d, want 0: the daemon answered 200", sre.StatusCode)
		}
	})

	// An incomplete 200 is a contract mismatch whichever layer catches it: the
	// client rejects a context with no SABR URL, the provider rejects the rest of
	// what SABR setup needs. Both must reach WaxTap as a zero-status refusal, which
	// it never retries.
	for _, tt := range []struct {
		name, want string
		body       map[string]any
	}{
		{
			name: "caught by the client",
			want: "no server_abr_streaming_url",
			body: map[string]any{"playability_status": "OK"},
		},
		{
			name: "caught by the provider",
			want: "missing server_abr_streaming_url",
			body: map[string]any{
				"playability_status": "OK", "server_abr_streaming_url": "https://r/v",
				"video_playback_ustreamer_config": "U", "visitor_data": "VD",
				"audio_formats": []map[string]any{{"itag": 251}},
			}, // no player_url
		},
	} {
		t.Run("an incomplete 200 is a contract mismatch, "+tt.name, func(t *testing.T) {
			p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tt.body)
			})
			defer done()

			_, err := p.ProvidePlayerContext(context.Background(), "VID")
			sre := sidecarResponseErr(t, err)
			if sre.StatusCode != 0 || sre.Code != "" {
				t.Errorf("error = %+v, want an uncoded zero-status refusal", sre)
			}
			if !strings.Contains(sre.Reason, tt.want) {
				t.Errorf("reason = %q, want it to contain %q", sre.Reason, tt.want)
			}
			if _, retry := waxtap.SidecarRetryWait(err); retry {
				t.Error("a contract mismatch earned a retry")
			}
		})
	}
}

// The rate-limited report is a refusal with a wait, like every other one, so a
// consumer reads it the same way.
func TestInvalidateSessionRateLimited(t *testing.T) {
	p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": false, "generation": 9, "retry_after_seconds": 20})
	})
	defer done()

	err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{Generation: 9})
	sre := sidecarResponseErr(t, err)
	if sre.RetryAfter != 20*time.Second {
		t.Errorf("RetryAfter = %v, want 20s", sre.RetryAfter)
	}
	if !strings.Contains(sre.Reason, "rate-limited") {
		t.Errorf("reason = %q", sre.Reason)
	}
}

// The arms below run WaxTap's own rule through call, so a consumer behaves the
// same whichever adapter it wires: after the daemon's stated wait on a transient
// refusal, after a short poke on one that stated none, and never at all on a
// verdict or a stated wait too long to serve.
func TestProviderRetriesOnceAfterStatedWait(t *testing.T) {
	// slept records the waits served instead of serving them, so the table runs at
	// full speed and can assert on the wait itself.
	var slept []time.Duration
	restore := provider.SetSleepForTest(func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	})
	defer restore()

	// reply is one scripted response; the last entry repeats.
	type reply struct {
		status     int
		retryAfter string
		body       map[string]any
	}
	ok200 := reply{status: 200, body: map[string]any{
		"playability_status": "OK", "player_url": "https://p/base.js",
		"server_abr_streaming_url": "https://r/v", "video_playback_ustreamer_config": "U",
		"visitor_data": "VD", "audio_formats": []map[string]any{{"itag": 251}},
	}}
	refusal := func(status int, retryAfter string, code string) reply {
		body := map[string]any{"error": "refused", "code": code}
		return reply{status: status, retryAfter: retryAfter, body: body}
	}

	tests := []struct {
		name      string
		replies   []reply
		wantCalls int
		wantWaits []time.Duration
		wantErr   bool
	}{
		{
			name:      "a stated wait is served, then the call succeeds",
			replies:   []reply{refusal(502, "1", "player-context-failed"), ok200},
			wantCalls: 2, wantWaits: []time.Duration{time.Second},
		},
		{
			name:      "a 5xx that stated no wait earns the poke",
			replies:   []reply{refusal(503, "", "no-session"), ok200},
			wantCalls: 2, wantWaits: []time.Duration{500 * time.Millisecond},
		},
		{
			name:      "a stated wait at the cap is served",
			replies:   []reply{refusal(502, "60", "player-context-failed"), ok200},
			wantCalls: 2, wantWaits: []time.Duration{60 * time.Second},
		},
		{
			name:      "a stated wait past the maximum is reported, not slept through",
			replies:   []reply{refusal(502, "61", "player-context-failed")},
			wantCalls: 1, wantErr: true,
		},
		{
			name:      "a verdict is never retried",
			replies:   []reply{refusal(422, "", "video-unavailable")},
			wantCalls: 1, wantErr: true,
		},
		{
			name:      "a bare 429 says back off rather than retry",
			replies:   []reply{refusal(429, "", "")},
			wantCalls: 1, wantErr: true,
		},
		{
			name:      "a 429 with a wait is retried",
			replies:   []reply{refusal(429, "2", ""), ok200},
			wantCalls: 2, wantWaits: []time.Duration{2 * time.Second},
		},
		{
			name:      "only once: a second refusal is returned",
			replies:   []reply{refusal(502, "1", "player-context-failed"), refusal(502, "1", "player-context-failed")},
			wantCalls: 2, wantWaits: []time.Duration{time.Second}, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slept = nil
			var calls int
			p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
				r := tt.replies[min(calls, len(tt.replies)-1)]
				calls++
				if r.retryAfter != "" {
					w.Header().Set("Retry-After", r.retryAfter)
				}
				w.WriteHeader(r.status)
				_ = json.NewEncoder(w).Encode(r.body)
			})
			defer done()

			_, err := p.ProvidePlayerContext(context.Background(), "VID")
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if calls != tt.wantCalls {
				t.Errorf("requests = %d, want %d", calls, tt.wantCalls)
			}
			if !slices.Equal(slept, tt.wantWaits) {
				t.Errorf("waits = %v, want %v", slept, tt.wantWaits)
			}
		})
	}

	t.Run("a transport failure earns the poke", func(t *testing.T) {
		slept = nil
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := srv.URL
		srv.Close()
		p := provider.New(client.New(addr))

		if _, err := p.ProvidePlayerContext(context.Background(), "VID"); err == nil {
			t.Fatal("want the transport failure")
		}
		if !slices.Equal(slept, []time.Duration{500 * time.Millisecond}) {
			t.Errorf("waits = %v, want one 500ms poke", slept)
		}
	})

	// The arms below run WaxTap's pause policy through this adapter on the
	// caller's own budget, so a wrapper that dropped the context, or slept before
	// asking, would pass the table above and fail here: a budget has to clear the
	// wait plus a second of headroom, one that does not gets the refusal instead
	// of a sleep it cannot finish, and a wait the context ends early returns the
	// refusal for a deadline and the cancellation for a caller giving up.
	//
	// refuseAll answers every request with a 502 stating retryAfter and counts
	// them, for the arms that never reach a second attempt.
	refuseAll := func(calls *int, retryAfter string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			*calls++
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused", "code": "player-context-failed"})
		}
	}
	t.Run("a budget that clears the wait plus the headroom retries", func(t *testing.T) {
		slept = nil
		var calls int
		p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			if calls > 1 {
				_ = json.NewEncoder(w).Encode(ok200.body)
				return
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused", "code": "player-context-failed"})
		})
		defer done()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := p.ProvidePlayerContext(ctx, "VID"); err != nil {
			t.Fatalf("ProvidePlayerContext: %v", err)
		}
		if calls != 2 || !slices.Equal(slept, []time.Duration{time.Second}) {
			t.Errorf("requests = %d, waits = %v, want 2 and one 1s wait", calls, slept)
		}
	})

	t.Run("a budget too short for the wait gets the refusal now", func(t *testing.T) {
		slept = nil
		var calls int
		p, done := newProvider(refuseAll(&calls, "30"))
		defer done()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := p.ProvidePlayerContext(ctx, "VID")
		if _, ok := errors.AsType[*waxtap.SidecarResponseError](err); !ok {
			t.Fatalf("err = %v (%T), want the refusal itself", err, err)
		}
		if calls != 1 || len(slept) != 0 {
			t.Errorf("requests = %d, waits = %v, want one request and no wait", calls, slept)
		}
	})

	t.Run("a deadline that expires during the wait returns the refusal", func(t *testing.T) {
		undo := provider.SetSleepForTest(func(context.Context, time.Duration) error { return context.DeadlineExceeded })
		defer undo()
		var calls int
		p, done := newProvider(refuseAll(&calls, "1"))
		defer done()

		_, err := p.ProvidePlayerContext(context.Background(), "VID")
		if _, ok := errors.AsType[*waxtap.SidecarResponseError](err); !ok {
			t.Fatalf("err = %v (%T), want the refusal the deadline cut short, not a bare timeout", err, err)
		}
		if calls != 1 {
			t.Errorf("requests = %d, want one: a wait cut short by a deadline earns no retry", calls)
		}
	})

	t.Run("a caller that leaves during the wait gets its cancellation, not the wait", func(t *testing.T) {
		// The real wait, handed the caller's context, which the caller ends the
		// moment the wait starts: after the refusal, so a retry is due, and before
		// any of the wait has been served. A wait given a context of its own would
		// hold the caller for the whole stated wait, which is long enough here to
		// be unmistakable and is never served when the wiring is right.
		const stated = 30 * time.Second
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		undo := provider.SetSleepForTest(func(ctx context.Context, d time.Duration) error {
			cancel()
			return provider.RealSleep(ctx, d)
		})
		defer undo()
		var calls int
		p, done := newProvider(refuseAll(&calls, "30"))
		defer done()

		start := time.Now()
		_, err := p.ProvidePlayerContext(ctx, "VID")
		if elapsed := time.Since(start); elapsed >= stated {
			t.Errorf("the call took %v: the caller's cancellation did not end the %v wait", elapsed, stated)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v (%T), want the cancellation to outrank the refusal", err, err)
		}
		if calls != 1 {
			t.Errorf("requests = %d, want one: the caller gave up", calls)
		}
	})

	t.Run("the session endpoint retries too, the report never does", func(t *testing.T) {
		slept = nil
		var sessionCalls, reportCalls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/session":
				sessionCalls++
				if sessionCalls == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused", "code": "no-session", "retry_after_seconds": 1})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"visitor_data": "VD", "session_generation": 4})
			case "/report":
				reportCalls++
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused", "code": "no-session"})
			}
		}))
		defer srv.Close()
		p := provider.New(client.New(srv.URL))

		if _, err := p.Session(context.Background()); err != nil {
			t.Fatalf("Session: %v", err)
		}
		if sessionCalls != 2 || !slices.Equal(slept, []time.Duration{time.Second}) {
			t.Errorf("session requests = %d, waits = %v, want 2 and one 1s wait", sessionCalls, slept)
		}
		slept = nil
		if err := p.InvalidateSession(context.Background(), potoken.SessionInvalidation{Generation: 9}); err == nil {
			t.Fatal("want the report's refusal")
		}
		if reportCalls != 1 || len(slept) != 0 {
			t.Errorf("report requests = %d, waits = %v, want one request and no wait: a report is sent once", reportCalls, slept)
		}
	})
}

// net/http reports its own client timeout as context.DeadlineExceeded, so a
// merely slow daemon must still be translated and retried. Only a caller that
// actually walked away gets its own error back untouched.
func TestSlowDaemonIsATransportFailureNotACancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	// The client's own timeout fires while the caller's context is still live.
	hc := &http.Client{Timeout: 50 * time.Millisecond}
	p := provider.New(client.New(srv.URL, client.WithHTTPClient(hc)))

	_, err := p.ProvidePlayerContext(context.Background(), "VID")
	if err == nil {
		t.Fatal("want the timeout")
	}
	if _, ok := errors.AsType[*waxtap.SidecarError](err); !ok {
		t.Fatalf("err = %v (%T), want a *waxtap.SidecarError: the caller never went away", err, err)
	}
}

// A body that is not a WaxSeal error envelope is never echoed into the sidecar
// error. WaxTap prints Reason in its CLI hints and deliberately never forwards
// raw bytes, which may carry tokens or cookies from an intermediary.
func TestNonEnvelopeBodyIsNotEchoed(t *testing.T) {
	const secret = "<html>proxy at internal-host.corp: session=SUPERSECRET</html>"
	p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(secret))
	})
	defer done()

	_, err := p.ProvidePlayerContext(context.Background(), "VID")
	sre := sidecarResponseErr(t, err)
	if strings.Contains(sre.Reason, "SUPERSECRET") || strings.Contains(sre.Reason, "internal-host") {
		t.Errorf("reason = %q, want no raw response bytes", sre.Reason)
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Errorf("error text = %q, want no raw response bytes", err)
	}
	if sre.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 kept", sre.StatusCode)
	}
	// A recognised envelope still carries its own text, which is the daemon's.
	p2, done2 := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "player-context failed: boom", "code": "player-context-failed"})
	})
	defer done2()
	_, err = p2.ProvidePlayerContext(context.Background(), "VID")
	if sre := sidecarResponseErr(t, err); !strings.Contains(sre.Reason, "boom") {
		t.Errorf("reason = %q, want the daemon's own envelope text", sre.Reason)
	}
}

// The daemon's own error travels in Cause, for a caller that knows this adapter.
// It is a field rather than an unwrap, so classification through the sidecar
// error and the line it prints are both exactly what they were. A 400 is used
// rather than a 5xx so the call is not retried: this is about the error the
// first attempt produced.
func TestSidecarErrorCarriesCause(t *testing.T) {
	p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "video_id is required", "code": "invalid-request"})
	})
	defer done()

	_, err := p.ProvidePlayerContext(context.Background(), "VID")
	sre := sidecarResponseErr(t, err)
	apiErr, ok := errors.AsType[*client.APIError](sre.Cause)
	if !ok {
		t.Fatalf("Cause = %#v, want the *client.APIError the client returned", sre.Cause)
	}
	if apiErr.Path != "/player-context" || !apiErr.Envelope {
		t.Errorf("cause = %+v, want the daemon's own player-context envelope", apiErr)
	}
	if _, ok := errors.AsType[*client.APIError](err); ok {
		t.Error("errors.AsType reached the cause through the sidecar error")
	}
	bare := *sre
	bare.Cause = nil
	if sre.Error() != bare.Error() {
		t.Errorf("Error() = %q, want the same line as without a Cause (%q)", sre.Error(), bare.Error())
	}

	// A body that was not the daemon's envelope holds raw bytes from whatever
	// answered instead. Reason already refuses to echo them; Cause must not carry
	// them out of this package by another route.
	p2, done2 := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<html>proxy at internal-host.corp: session=SUPERSECRET</html>"))
	})
	defer done2()
	_, err = p2.ProvidePlayerContext(context.Background(), "VID")
	if cause := sidecarResponseErr(t, err).Cause; cause != nil {
		t.Errorf("Cause = %#v, want nil: the body was not the daemon's envelope", cause)
	}
}
