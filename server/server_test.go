package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/internal/minter"
)

func TestParseTenantKeys(t *testing.T) {
	if got := ParseTenantKeys("  "); got != nil {
		t.Errorf("empty input = %v, want nil (keyless)", got)
	}
	m := ParseTenantKeys("alice=KEYA, bob=KEYB")
	if len(m) != 2 || m["KEYA"] != "alice" || m["KEYB"] != "bob" {
		t.Errorf("labelled keys = %v", m)
	}
	bare := ParseTenantKeys("RAWKEY")
	if lbl := bare["RAWKEY"]; lbl == "" || lbl == "RAWKEY" {
		t.Errorf("bare key label = %q, want a generated label that is not the key", lbl)
	}
}

func TestAPIKeyExtraction(t *testing.T) {
	header := httptest.NewRequest(http.MethodGet, "/", nil)
	header.Header.Set("X-API-Key", "H")
	if got := apiKey(header); got != "H" {
		t.Errorf("X-API-Key = %q, want H", got)
	}
	bearer := httptest.NewRequest(http.MethodGet, "/", nil)
	bearer.Header.Set("Authorization", "Bearer B")
	if got := apiKey(bearer); got != "B" {
		t.Errorf("Bearer = %q, want B", got)
	}
	query := httptest.NewRequest(http.MethodGet, "/?key=Q", nil)
	if got := apiKey(query); got != "Q" {
		t.Errorf("query key = %q, want Q", got)
	}
	if got := apiKey(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Errorf("no key = %q, want empty", got)
	}
}

func TestPlayerContextVideoID(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		query    string
		wantID   string
		wantOK   bool
		wantCode int // checked only when !wantOK
	}{
		{name: "body", body: `{"video_id":"VID"}`, wantID: "VID", wantOK: true},
		{name: "empty body + query", body: "", query: "?video_id=QID", wantID: "QID", wantOK: true},
		{name: "body wins over query", body: `{"video_id":"BID"}`, query: "?video_id=QID", wantID: "BID", wantOK: true},
		{name: "empty body no query", body: "", wantOK: false, wantCode: http.StatusBadRequest},
		{name: "empty json no query", body: `{}`, wantOK: false, wantCode: http.StatusBadRequest},
		{name: "malformed json", body: `{not json`, wantOK: false, wantCode: http.StatusBadRequest},
		{name: "bad charset in body", body: `{"video_id":"bad id/../x"}`, wantOK: false, wantCode: http.StatusBadRequest},
		{name: "bad charset in query", query: "?video_id=" + url.QueryEscape("a b!"), wantOK: false, wantCode: http.StatusBadRequest},
		{name: "over length", body: `{"video_id":"` + strings.Repeat("a", 65) + `"}`, wantOK: false, wantCode: http.StatusBadRequest},
		{name: "URL rejected", body: `{"video_id":"https://youtu.be/x"}`, wantOK: false, wantCode: http.StatusBadRequest},
		{name: "real id", body: `{"video_id":"aqz-KE-bpKQ"}`, wantID: "aqz-KE-bpKQ", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/player-context"+tt.query, strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			id, ok := playerContextVideoID(w, r)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (body=%q query=%q)", ok, tt.wantOK, tt.body, tt.query)
			}
			if tt.wantOK {
				if id != tt.wantID {
					t.Errorf("id = %q, want %q", id, tt.wantID)
				}
				return
			}
			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			var env struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body is not JSON: %v (%q)", err, w.Body.String())
			}
			if env.Code != CodeInvalidRequest {
				t.Errorf("code = %q, want %q", env.Code, CodeInvalidRequest)
			}
		})
	}
}

func TestNormalizeScope(t *testing.T) {
	ok := map[string]string{
		"":       "pot",
		"pot":    "pot",
		"player": "player",
		"gvs":    "gvs",
		" GVS ":  "gvs",
		"Player": "player",
	}
	for in, want := range ok {
		got, valid := normalizeScope(in)
		if !valid || got != want {
			t.Errorf("normalizeScope(%q) = (%q, %v), want (%q, true)", in, got, valid, want)
		}
	}
	for _, bad := range []string{"garbagescope", "subtitles", "web", "sabr"} {
		if got, valid := normalizeScope(bad); valid {
			t.Errorf("normalizeScope(%q) = (%q, true), want rejected", bad, got)
		}
	}
}

func TestMethodNotAllowedHandler(t *testing.T) {
	h := methodNotAllowed(http.MethodGet, http.MethodPost)
	r := httptest.NewRequest(http.MethodPut, "/player-context", nil)
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q, want %q", got, "GET, POST")
	}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not JSON: %v (%q)", err, w.Body.String())
	}
	if env.Code != CodeMethodNotAllowed {
		t.Errorf("code = %q, want %q", env.Code, CodeMethodNotAllowed)
	}
	if env.Error == "" {
		t.Error("error message is empty")
	}
}

func TestRoutesMethodMatching(t *testing.T) {
	mux := (&Server{}).routes()
	tests := []struct {
		method, path, wantPattern string
	}{
		{http.MethodPost, "/get_pot", "POST /get_pot"},
		{http.MethodGet, "/get_pot", "/get_pot"},
		{http.MethodGet, "/player-context", "GET /player-context"},
		{http.MethodPost, "/player-context", "POST /player-context"},
		{http.MethodPut, "/player-context", "/player-context"},
		{http.MethodOptions, "/player-context", "/player-context"},
		{http.MethodGet, "/ping", "GET /ping"},
		{http.MethodDelete, "/ping", "/ping"},
		{http.MethodGet, "/session", "GET /session"},
		{http.MethodPost, "/session", "/session"},
		{http.MethodGet, "/metrics", "GET /metrics"},
		{http.MethodPost, "/metrics", "/metrics"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, tt.path, nil)
		if _, pattern := mux.Handler(r); pattern != tt.wantPattern {
			t.Errorf("%s %s matched %q, want %q", tt.method, tt.path, pattern, tt.wantPattern)
		}
	}
}

func TestMethodNotAllowedBeforeAuth(t *testing.T) {
	s := &Server{
		tenants: minter.NewTenants(nil, "", map[string]string{"GOODKEY": "alice"}, browser.Options{}),
		log:     slog.New(slog.DiscardHandler),
	}
	r := httptest.NewRequest(http.MethodGet, "/get_pot", nil) // no API key
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d (405 must precede auth)", w.Code, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want %q", got, http.MethodPost)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not JSON: %v (%q)", err, w.Body.String())
	}
	if env.Code != CodeMethodNotAllowed {
		t.Errorf("code = %q, want %q (got 401 unauthorized instead?)", env.Code, CodeMethodNotAllowed)
	}
}

func TestTenantUnauthorizedCode(t *testing.T) {
	s := &Server{
		tenants: minter.NewTenants(nil, "", map[string]string{"GOODKEY": "alice"}, browser.Options{}),
		log:     slog.New(slog.DiscardHandler),
	}
	r := httptest.NewRequest(http.MethodPost, "/get_pot", nil)
	r.Header.Set("X-API-Key", "BADKEY")
	w := httptest.NewRecorder()

	if _, _, ok := s.tenant(w, r); ok {
		t.Fatal("tenant() accepted an unknown key")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not JSON: %v (%q)", err, w.Body.String())
	}
	if env.Code != CodeUnauthorized {
		t.Errorf("code = %q, want %q", env.Code, CodeUnauthorized)
	}
	if env.Error == "" {
		t.Error("error message is empty")
	}
}
