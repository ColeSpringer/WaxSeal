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
	"strings"
	"testing"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
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
			"session_generation":              7,
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
	// Without the generation WaxTap cannot name this context's session in a report,
	// so a capped stream would have no escape.
	if pc.Generation != 7 {
		t.Errorf("generation = %d, want 7", pc.Generation)
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
