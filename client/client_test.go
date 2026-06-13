package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colespringer/waxseal/client"
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
				{"name": "YSC", "value": "abc", "domain": ".youtube.com", "path": "/", "secure": true, "http_only": true},
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
	if ck := s.Cookies[0]; ck.Name != "YSC" || ck.Value != "abc" || !ck.Secure || !ck.HttpOnly {
		t.Errorf("cookie = %+v", ck)
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
			"status":                   "OK",
			"player_url":               "https://www.youtube.com/s/player/abc/base.js",
			"server_abr_streaming_url": "https://r1.googlevideo.com/videoplayback?n=scram",
			"visitor_data":             "VD",
			"client_version":           "2.0",
			"session_generation":       3,
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
	if pc.SessionGeneration != 3 {
		t.Errorf("session_generation = %d, want 3", pc.SessionGeneration)
	}
	if len(pc.AudioFormats) != 1 || pc.AudioFormats[0].Itag != 251 || pc.AudioFormats[0].MimeType != "audio/webm" {
		t.Errorf("audio formats = %+v", pc.AudioFormats)
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
		})
	}
}

// TestAudioFormatTagDrift protects fields required by WaxTap's SABR setup from
// silent JSON tag changes.
func TestAudioFormatTagDrift(t *testing.T) {
	const payload = `{
		"itag": 251, "lmt": "171", "xtags": "X", "mime_type": "audio/webm", "bitrate": 130000,
		"content_length": 1234, "approx_duration_ms": 634000, "audio_sample_rate": 48000,
		"audio_channels": 2, "audio_quality": "AUDIO_QUALITY_MEDIUM",
		"is_drc": true, "audio_track_id": "en.4"
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
