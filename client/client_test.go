package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/internal/readmedoc"
)

func TestPOToken(t *testing.T) {
	var gotBinding, gotScope, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_pot" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotKey = r.Header.Get("X-API-Key")
		var req struct {
			ContentBinding string `json:"content_binding"`
			Scope          string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotBinding, gotScope = req.ContentBinding, req.Scope
		_ = json.NewEncoder(w).Encode(map[string]any{"poToken": "TOK", "expiresAt": "2030-01-01T00:00:00Z"})
	}))
	defer srv.Close()

	c := client.New(srv.URL, client.WithAPIKey("K"))
	tok, err := c.POToken(context.Background(), "VID", "player")
	if err != nil || tok.Value != "TOK" {
		t.Fatalf("token = %+v, err = %v", tok, err)
	}
	if gotBinding != "VID" || gotScope != "player" || gotKey != "K" {
		t.Errorf("binding=%q scope=%q key=%q", gotBinding, gotScope, gotKey)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("expiresAt not parsed")
	}
}

// TestPOTokenWarning checks that the daemon's optional warning field parses into
// Token.Warning, and that its absence leaves Token.Warning empty.
func TestPOTokenWarning(t *testing.T) {
	withWarning := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"poToken":   "TOK",
			"expiresAt": "2030-01-01T00:00:00Z",
			"warning":   "content_binding looks like a URL",
		})
	}))
	defer withWarning.Close()
	tok, err := client.New(withWarning.URL).POToken(context.Background(), "https://youtube.com/watch?v=x", "player")
	if err != nil {
		t.Fatalf("POToken: %v", err)
	}
	if tok.Warning != "content_binding looks like a URL" {
		t.Errorf("Token.Warning = %q, want the daemon's warning", tok.Warning)
	}

	noWarning := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"poToken": "TOK", "expiresAt": "2030-01-01T00:00:00Z"})
	}))
	defer noWarning.Close()
	tok, err = client.New(noWarning.URL).POToken(context.Background(), "vid", "player")
	if err != nil {
		t.Fatalf("POToken (no warning): %v", err)
	}
	if tok.Warning != "" {
		t.Errorf("Token.Warning = %q, want empty when the response has no warning", tok.Warning)
	}
}

func TestPOTokenEmptyBindingAndHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := client.New(srv.URL)
	if _, err := c.POToken(context.Background(), "", "gvs"); err == nil {
		t.Error("empty content_binding should error before any HTTP call")
	}
	if _, err := c.POToken(context.Background(), "VD", "gvs"); err == nil {
		t.Error("a non-200 from /get_pot should error")
	}
}

func TestSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"visitor_data":       "VD",
			"user_agent":         "UA",
			"client_version":     "CV",
			"session_generation": 7,
			"cookies": []map[string]any{
				{"name": "YSC", "value": "abc", "domain": ".youtube.com", "path": "/", "secure": true, "http_only": true, "expires": "2035-01-02T03:04:05Z", "same_site": "Lax"},
			},
		})
	}))
	defer srv.Close()

	s, err := client.New(srv.URL).Session(context.Background())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if s.VisitorData != "VD" || s.UserAgent != "UA" || s.ClientVersion != "CV" || len(s.Cookies) != 1 {
		t.Fatalf("session = %+v", s)
	}
	if s.SessionGeneration != 7 {
		t.Errorf("session_generation = %d, want 7", s.SessionGeneration)
	}
	ck := s.Cookies[0]
	if ck.Name != "YSC" || ck.Value != "abc" || !ck.Secure || !ck.HttpOnly {
		t.Errorf("cookie = %+v", ck)
	}
	if want := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC); !ck.Expires.Equal(want) {
		t.Errorf("cookie Expires = %v, want %v", ck.Expires, want)
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax (%v)", ck.SameSite, http.SameSiteLaxMode)
	}
}

func TestPlayerContext(t *testing.T) {
	var gotVideoID, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/player-context" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotKey = r.Header.Get("X-API-Key")
		var req struct {
			VideoID string `json:"video_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotVideoID = req.VideoID
		_ = json.NewEncoder(w).Encode(map[string]any{
			"playability_status":       "OK",
			"player_url":               "https://www.youtube.com/s/player/abc/base.js",
			"server_abr_streaming_url": "https://r1.googlevideo.com/videoplayback?n=scram",
			"visitor_data":             "VD",
			"client_version":           "2.0",
			"user_agent":               "Mozilla/5.0 (X11; Linux x86_64) Chrome/149.0.0.0",
			"session_generation":       3,
			"channel_id":               "UCabc",
			"description":              "line one\nline two",
			"is_live_content":          true,
			"is_live_now":              false,
			"is_upcoming":              false,
			"publish_date":             "2015-04-10T00:00:00-07:00",
			"thumbnails": []map[string]any{
				{"url": "https://i.ytimg.com/vi/VID/default.jpg", "width": 120, "height": 90},
				{"url": "https://i.ytimg.com/vi/VID/maxresdefault.jpg", "width": 1280, "height": 720},
			},
			"audio_formats": []map[string]any{
				{"itag": 251, "lmt": "1719185012384481", "mime_type": "audio/webm", "bitrate": 130000, "content_length": 1234, "audio_quality": "AUDIO_QUALITY_MEDIUM"},
			},
		})
	}))
	defer srv.Close()

	c := client.New(srv.URL, client.WithAPIKey("K"))
	pc, err := c.PlayerContext(context.Background(), "VID")
	if err != nil {
		t.Fatalf("PlayerContext: %v", err)
	}
	if gotVideoID != "VID" || gotKey != "K" {
		t.Errorf("video_id=%q key=%q", gotVideoID, gotKey)
	}
	if pc.ServerAbrStreamingURL != "https://r1.googlevideo.com/videoplayback?n=scram" || pc.PlayerURL == "" {
		t.Errorf("context = %+v", pc)
	}
	if pc.UserAgent != "Mozilla/5.0 (X11; Linux x86_64) Chrome/149.0.0.0" {
		t.Errorf("user_agent = %q, want the identity the context was minted under", pc.UserAgent)
	}
	if pc.SessionGeneration != 3 {
		t.Errorf("session_generation = %d, want 3", pc.SessionGeneration)
	}
	if pc.PlayabilityStatus != "OK" {
		t.Errorf("playability_status = %q, want OK", pc.PlayabilityStatus)
	}
	if len(pc.AudioFormats) != 1 || pc.AudioFormats[0].Itag != 251 || pc.AudioFormats[0].MimeType != "audio/webm" {
		t.Errorf("audio formats = %+v", pc.AudioFormats)
	}
	if pc.ChannelID != "UCabc" || pc.Description != "line one\nline two" {
		t.Errorf("channel_id = %q, description = %q", pc.ChannelID, pc.Description)
	}
	if !pc.IsLiveContent || pc.IsLiveNow || pc.IsUpcoming {
		t.Errorf("live flags = %v/%v/%v, want true/false/false", pc.IsLiveContent, pc.IsLiveNow, pc.IsUpcoming)
	}
	if pc.PublishDate != "2015-04-10T00:00:00-07:00" {
		t.Errorf("publish_date = %q", pc.PublishDate)
	}
	// The ladder arrives in the server's order and is not re-sorted by the client.
	if len(pc.Thumbnails) != 2 || pc.Thumbnails[0].Width != 120 || pc.Thumbnails[1].Height != 720 ||
		pc.Thumbnails[1].URL != "https://i.ytimg.com/vi/VID/maxresdefault.jpg" {
		t.Errorf("thumbnails = %+v", pc.Thumbnails)
	}
}

func TestPlayerContextEmptyVideoIDAndHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := client.New(srv.URL)
	if _, err := c.PlayerContext(context.Background(), ""); err == nil {
		t.Error("empty video_id should error before any HTTP call")
	}
	if _, err := c.PlayerContext(context.Background(), "VID"); err == nil {
		t.Error("a non-200 from /player-context should error")
	}
}

func TestAPIErrorShapes(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantCode    string
		wantMsg     string
		wantDetails string
	}{
		{"structured", http.StatusUnprocessableEntity, "application/json", `{"error":"video unplayable","code":"video-unavailable","details":"LOGIN_REQUIRED"}`, client.CodeVideoUnavailable, "video unplayable", "LOGIN_REQUIRED"},
		{"old-server", http.StatusBadGateway, "application/json", `{"error":"mint failed: boom"}`, "", "mint failed: boom", ""},
		{"non-json", http.StatusBadGateway, "text/plain", "bad gateway from a proxy", "", "bad gateway from a proxy", ""},
		{"empty", http.StatusServiceUnavailable, "", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				w.WriteHeader(tt.status)
				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
				}
			}))
			defer srv.Close()

			_, err := client.New(srv.URL).PlayerContext(context.Background(), "VID")
			apiErr, ok := errors.AsType[*client.APIError](err)
			if !ok {
				t.Fatalf("err = %v (%T), want *client.APIError", err, err)
			}
			if apiErr.Path != "/player-context" {
				t.Errorf("Path = %q, want /player-context", apiErr.Path)
			}
			if apiErr.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tt.status)
			}
			if apiErr.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", apiErr.Code, tt.wantCode)
			}
			if apiErr.Message != tt.wantMsg {
				t.Errorf("Message = %q, want %q", apiErr.Message, tt.wantMsg)
			}
			if apiErr.Details != tt.wantDetails {
				t.Errorf("Details = %q, want %q", apiErr.Details, tt.wantDetails)
			}
			// Envelope separates the daemon's own text from raw bytes an
			// intermediary sent, which a consumer must not forward.
			if want := tt.wantCode != "" || tt.name == "old-server"; apiErr.Envelope != want {
				t.Errorf("Envelope = %v, want %v", apiErr.Envelope, want)
			}
		})
	}
}

// A refusal that states a wait is read back as one, from the header or the
// envelope. The header wins where both are present, because a proxy can set the
// header and cannot rewrite the body.
func TestAPIErrorRetryAfter(t *testing.T) {
	httpDate := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	tests := []struct {
		name   string
		header string
		body   string
		want   time.Duration // 0 means none stated
		approx bool          // compare loosely: an HTTP-date is resolved against now
	}{
		{name: "header only", header: "12", body: `{"error":"x","code":"no-session"}`, want: 12 * time.Second},
		{name: "body only", body: `{"error":"x","code":"no-session","retry_after_seconds":7}`, want: 7 * time.Second},
		{name: "header wins", header: "12", body: `{"error":"x","code":"no-session","retry_after_seconds":7}`, want: 12 * time.Second},
		{name: "http date", header: httpDate, body: `{"error":"x","code":"no-session"}`, want: 45 * time.Second, approx: true},
		{name: "past http date is never negative", header: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), body: `{"error":"x","code":"no-session"}`},
		// A header that yields no wait still ahead must not discard a value the
		// daemon did send in the body.
		{name: "past http date falls back to the body", header: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), body: `{"error":"x","code":"no-session","retry_after_seconds":7}`, want: 7 * time.Second},
		{name: "empty body still reads the header", header: "12", want: 12 * time.Second},
		{name: "garbage header falls back to the body", header: "soon", body: `{"error":"x","code":"no-session","retry_after_seconds":7}`, want: 7 * time.Second},
		{name: "garbage header, no body value", header: "soon", body: `{"error":"x","code":"no-session"}`},
		{name: "neither", body: `{"error":"x","code":"no-session"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := client.New(srv.URL).PlayerContext(context.Background(), "VID")
			apiErr, ok := errors.AsType[*client.APIError](err)
			if !ok {
				t.Fatalf("err = %v (%T), want *client.APIError", err, err)
			}
			if want := tt.body != ""; apiErr.Envelope != want {
				t.Errorf("Envelope = %v, want %v", apiErr.Envelope, want)
			}
			switch {
			case tt.approx:
				if d := apiErr.RetryAfter - tt.want; d > 2*time.Second || d < -2*time.Second {
					t.Errorf("RetryAfter = %v, want about %v", apiErr.RetryAfter, tt.want)
				}
			case apiErr.RetryAfter != tt.want:
				t.Errorf("RetryAfter = %v, want %v", apiErr.RetryAfter, tt.want)
			}
			if apiErr.RetryAfter < 0 {
				t.Errorf("RetryAfter = %v, want never negative", apiErr.RetryAfter)
			}
		})
	}
}

// BaseURL reports the address New normalised, which is what a consumer labels
// its own errors with.
func TestBaseURL(t *testing.T) {
	if got := client.New("http://127.0.0.1:4416/").BaseURL(); got != "http://127.0.0.1:4416" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", got)
	}
}

// TestAudioFormatTagDrift protects fields required by WaxTap's SABR setup from
// silent JSON tag changes.
func TestAudioFormatTagDrift(t *testing.T) {
	const payload = `{
		"itag": 251, "lmt": "171", "xtags": "X", "mime_type": "audio/webm", "bitrate": 130000,
		"content_length": 1234, "approx_duration_ms": 634000, "audio_sample_rate": 48000,
		"audio_channels": 2, "audio_quality": "AUDIO_QUALITY_MEDIUM",
		"is_drc": true, "audio_track_id": "en.4", "audio_is_default": true
	}`
	var f client.AudioFormat
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !f.IsDrc {
		t.Error("is_drc did not decode into IsDrc")
	}
	if f.AudioTrackID != "en.4" {
		t.Errorf("audio_track_id = %q, want en.4", f.AudioTrackID)
	}
	if f.AudioIsDefault == nil || !*f.AudioIsDefault {
		t.Error("audio_is_default did not decode into AudioIsDefault as a stated true")
	}
	// An absent key is unstated, which a consumer must be able to tell from false.
	var bare client.AudioFormat
	if err := json.Unmarshal([]byte(`{"itag": 251, "audio_track_id": "fr.3"}`), &bare); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if bare.AudioIsDefault != nil {
		t.Errorf("audio_is_default = %v without the key, want nil", *bare.AudioIsDefault)
	}
}

func TestReport(t *testing.T) {
	var gotBody map[string]any
	var gotKey, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/report" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotKey, gotMethod = r.Header.Get("X-API-Key"), r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": true, "retired": true, "retirement_pending": false, "generation": 5,
		})
	}))
	defer srv.Close()

	c := client.New(srv.URL, client.WithAPIKey("K"))
	res, err := c.Report(context.Background(), 5, "VID", "incomplete-stream")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !res.Accepted || !res.Retired || res.Generation != 5 {
		t.Errorf("res = %+v, want accepted+retired, gen 5", res)
	}
	if gotKey != "K" || gotMethod != http.MethodPost {
		t.Errorf("key=%q method=%q", gotKey, gotMethod)
	}
	if gotBody["session_generation"] != float64(5) || gotBody["video_id"] != "VID" || gotBody["reason"] != "incomplete-stream" {
		t.Errorf("request body = %v", gotBody)
	}

	// A zero generation is rejected before any HTTP call.
	if _, err := c.Report(context.Background(), 0, "VID", "x"); err == nil {
		t.Error("Report with generation 0 should error before any HTTP call")
	}
}

// A deferred retirement is the other accepted outcome: the daemon took the
// report but a browser operation held the page, so the session goes at the next
// streaming handoff. It is the only case that sets retirement_pending, so
// without it that tag is never decoded from anything but false, which is also
// the zero value.
func TestReportRetirementPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": true, "retired": false, "retirement_pending": true, "generation": 3,
		})
	}))
	defer srv.Close()

	res, err := client.New(srv.URL).Report(context.Background(), 3, "", "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !res.Accepted || res.Retired || !res.RetirementPending || res.Generation != 3 {
		t.Errorf("res = %+v, want accepted with retirement pending at gen 3", res)
	}
}

func TestReportRateLimitedResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": false, "retry_after_seconds": 42, "generation": 2,
		})
	}))
	defer srv.Close()

	res, err := client.New(srv.URL).Report(context.Background(), 2, "", "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if res.Accepted || res.RetryAfterSeconds != 42 || res.Generation != 2 {
		t.Errorf("res = %+v, want !Accepted, RetryAfterSeconds 42, gen 2", res)
	}
}

func TestReportHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	if _, err := client.New(srv.URL).Report(context.Background(), 1, "VID", "x"); err == nil {
		t.Error("a non-200 from /report should error")
	}
}

// TestClientDecodesREADMEExamples serves each documented response example to the
// client and reads the values back. The shape contracts in server/ pin the
// README's keys against the structs that produce them; a key can match there and
// still decode into nothing here, which is the gap this closes.
//
// cookie_header is deliberately not checked: the client does not decode it,
// because it is derivable from the cookies it does.
func TestClientDecodesREADMEExamples(t *testing.T) {
	serve := func(t *testing.T, heading string) (*client.Client, map[string]any) {
		t.Helper()
		raw, err := readmedoc.Response("../README.md", heading)
		if err != nil {
			t.Fatalf("locate the README %s response block: %v", heading, err)
		}
		var documented map[string]any
		if err := json.Unmarshal(raw, &documented); err != nil {
			t.Fatalf("README %s response block is not JSON once its comments are stripped: %v", heading, err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
		}))
		t.Cleanup(srv.Close)
		return client.New(srv.URL), documented
	}
	// equal compares one decoded field against the value the README documents for
	// it. The documented side is an any, so a string, a bool, and a JSON number
	// all go through one helper.
	equal := func(t *testing.T, key string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want the documented %v", key, got, want)
		}
	}
	rfc3339 := func(t *testing.T, key string, v any) time.Time {
		t.Helper()
		ts, err := time.Parse(time.RFC3339, v.(string))
		if err != nil {
			t.Fatalf("the documented %s is not RFC3339: %v", key, err)
		}
		return ts
	}

	t.Run("get_pot", func(t *testing.T) {
		c, want := serve(t, "/get_pot")
		tok, err := c.POToken(context.Background(), "VID", "player")
		if err != nil {
			t.Fatalf("POToken: %v", err)
		}
		equal(t, "poToken", tok.Value, want["poToken"])
		equal(t, "warning", tok.Warning, want["warning"])
		if ts := rfc3339(t, "expiresAt", want["expiresAt"]); !tok.ExpiresAt.Equal(ts) {
			t.Errorf("expiresAt = %v, want the documented %v", tok.ExpiresAt, ts)
		}
	})

	t.Run("report", func(t *testing.T) {
		// The documented example is the rate-limited one, so its three booleans are
		// all false and only the generation and the wait carry a value here.
		// TestReport is what pins an accepted report's true ones.
		c, want := serve(t, "/report")
		res, err := c.Report(context.Background(), 1, "", "truncated")
		if err != nil {
			t.Fatalf("Report: %v", err)
		}
		equal(t, "accepted", res.Accepted, want["accepted"])
		equal(t, "retired", res.Retired, want["retired"])
		equal(t, "retirement_pending", res.RetirementPending, want["retirement_pending"])
		equal(t, "generation", float64(res.Generation), want["generation"])
		equal(t, "retry_after_seconds", float64(res.RetryAfterSeconds), want["retry_after_seconds"])
	})

	t.Run("session", func(t *testing.T) {
		c, want := serve(t, "/session")
		sess, err := c.Session(context.Background())
		if err != nil {
			t.Fatalf("Session: %v", err)
		}
		equal(t, "visitor_data", sess.VisitorData, want["visitor_data"])
		equal(t, "user_agent", sess.UserAgent, want["user_agent"])
		equal(t, "client_version", sess.ClientVersion, want["client_version"])
		equal(t, "session_generation", float64(sess.SessionGeneration), want["session_generation"])

		documented, _ := want["cookies"].([]any)
		if len(documented) == 0 || len(sess.Cookies) != len(documented) {
			t.Fatalf("cookies = %d, want the %d the README documents", len(sess.Cookies), len(documented))
		}
		for i, raw := range documented {
			doc := raw.(map[string]any)
			got := sess.Cookies[i]
			equal(t, "cookies.name", got.Name, doc["name"])
			equal(t, "cookies.value", got.Value, doc["value"])
			equal(t, "cookies.domain", got.Domain, doc["domain"])
			equal(t, "cookies.path", got.Path, doc["path"])
			equal(t, "cookies.secure", got.Secure, doc["secure"])
			equal(t, "cookies.http_only", got.HttpOnly, doc["http_only"])
			equal(t, "cookies.same_site", got.SameSite, wireSameSite(doc["same_site"]))
			if ts := rfc3339(t, "cookies.expires", doc["expires"]); !got.Expires.Equal(ts) {
				t.Errorf("cookies.expires = %v, want the documented %v", got.Expires, ts)
			}
		}
	})
}

// wireSameSite is the mapping the README documents for a cookie's same_site,
// spelled out here rather than reached for: the client's own mapper is
// unexported, and a test that called it could not tell a wrong mapping from a
// consistent one.
func wireSameSite(v any) http.SameSite {
	switch v {
	case "Strict":
		return http.SameSiteStrictMode
	case "Lax":
		return http.SameSiteLaxMode
	case "None":
		return http.SameSiteNoneMode
	default:
		return 0
	}
}
