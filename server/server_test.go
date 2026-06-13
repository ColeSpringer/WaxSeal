package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		wantCode int    // checked only when !wantOK
		wantMsg  string // exact error message, checked when set
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
		{name: "array body", body: `[1,2,3]`, wantOK: false, wantCode: http.StatusBadRequest, wantMsg: "request body must be a JSON object"},
		{name: "trailing array bracket", body: `{"video_id":"abc"}]`, wantOK: false, wantCode: http.StatusBadRequest, wantMsg: "request body must be a single JSON object"},
		{name: "two objects", body: `{"video_id":"abc"}{"x":1}`, wantOK: false, wantCode: http.StatusBadRequest, wantMsg: "request body must be a single JSON object"},
		{name: "object then oversize whitespace", body: `{"video_id":"abc"}` + strings.Repeat(" ", 1<<20), wantOK: false, wantCode: http.StatusBadRequest, wantMsg: "request body too large (max 1 MiB)"},
		{name: "trailing whitespace", body: "{\"video_id\":\"abc\"}\n", wantID: "abc", wantOK: true},
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
			if tt.wantMsg != "" && env.Error != tt.wantMsg {
				t.Errorf("message = %q, want %q", env.Error, tt.wantMsg)
			}
			if strings.Contains(env.Error, "struct") {
				t.Errorf("message %q leaks a Go struct type", env.Error)
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
		{http.MethodPost, "/report", "POST /report"},
		{http.MethodGet, "/report", "/report"},
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
		tenants: minter.NewTenants(nil, "", map[string]string{"GOODKEY": "alice"}, browser.Options{}, 0, 0),
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
		tenants: minter.NewTenants(nil, "", map[string]string{"GOODKEY": "alice"}, browser.Options{}, 0, 0),
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

func TestDecodeErrMsg(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"too large", &http.MaxBytesError{Limit: 1 << 20}, "request body too large (max 1 MiB)"},
		{"eof", io.EOF, "request body is empty"},
		{"unexpected eof", io.ErrUnexpectedEOF, "request body is truncated (incomplete JSON)"},
		{"type mismatch top-level", &json.UnmarshalTypeError{Value: "array"}, "request body must be a JSON object"},
		{"type mismatch field", &json.UnmarshalTypeError{Field: "content_binding", Value: "number"}, `field "content_binding" has the wrong type`},
		{"type mismatch nested", &json.UnmarshalTypeError{Field: "a.b", Value: "number"}, "request body contains a field with the wrong type"},
		{"syntax error", &json.SyntaxError{}, "request body contains malformed JSON"},
		{"plain error", errors.New("boom"), "request body contains invalid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeErrMsg(tt.err); got != tt.want {
				t.Errorf("decodeErrMsg = %q, want %q", got, tt.want)
			}
			if strings.Contains(decodeErrMsg(tt.err), "struct") {
				t.Errorf("decodeErrMsg(%s) leaks a Go struct type", tt.name)
			}
		})
	}
}

// postGetPot sends a request with a valid API key through the full server mux.
func postGetPot(body string) *httptest.ResponseRecorder {
	s := &Server{
		tenants: minter.NewTenants(nil, "", map[string]string{"K": "alice"}, browser.Options{}, 0, 0),
		log:     slog.New(slog.DiscardHandler),
	}
	r := httptest.NewRequest(http.MethodPost, "/get_pot", strings.NewReader(body))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

func TestGetPotArrayBodyDoesNotLeakType(t *testing.T) {
	w := postGetPot(`[1,2,3]`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
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
	if env.Error != "request body must be a JSON object" {
		t.Errorf("message = %q, want %q", env.Error, "request body must be a JSON object")
	}
	for _, leak := range []string{"struct", "ContentBinding"} {
		if strings.Contains(env.Error, leak) {
			t.Errorf("message %q leaks internal Go detail %q", env.Error, leak)
		}
	}
}

func TestGetPotTrailingDataRejected(t *testing.T) {
	for _, body := range []string{
		`{"content_binding":"VID"}{"x":1}`, // a second object
		`{"content_binding":"VID"}]`,       // trailing junk
	} {
		w := postGetPot(body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want %d", body, w.Code, http.StatusBadRequest)
			continue
		}
		var env struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("body %q: error body is not JSON: %v (%q)", body, err, w.Body.String())
		}
		if env.Code != CodeInvalidRequest {
			t.Errorf("body %q: code = %q, want %q", body, env.Code, CodeInvalidRequest)
		}
		if env.Error != "request body must be a single JSON object" {
			t.Errorf("body %q: message = %q, want the single-object message", body, env.Error)
		}
	}
}

// fakePlayerSession exercises live-session handlers without a browser.
type fakePlayerSession struct {
	abrURL      string
	vd          string
	established bool
	closed      atomic.Bool
}

func (f *fakePlayerSession) Mint(context.Context, string) (browser.MintResult, error) {
	return browser.MintResult{Kind: "integrity", Lifetime: 3600}, nil
}
func (f *fakePlayerSession) PlayerContext(context.Context, string) (browser.PlayerContext, error) {
	return browser.PlayerContext{Status: "OK", ServerAbrStreamingURL: f.abrURL, VisitorData: f.vd}, nil
}
func (f *fakePlayerSession) EnsureEstablished(context.Context) error { return nil }
func (f *fakePlayerSession) Ping(context.Context) error              { return nil }
func (f *fakePlayerSession) AttestKind() string                      { return "integrity" }
func (f *fakePlayerSession) Identity() browser.Identity {
	return browser.Identity{VisitorData: f.vd, UserAgent: "UA", ClientVersion: "2.x"}
}
func (f *fakePlayerSession) BrowserCookies() ([]*http.Cookie, error) {
	return []*http.Cookie{{Name: "VISITOR_INFO1_LIVE", Value: "abc"}}, nil
}
func (f *fakePlayerSession) Established() bool { return f.established }
func (f *fakePlayerSession) LastProof() (browser.FullLengthProbe, time.Time) {
	return browser.FullLengthProbe{}, time.Time{}
}
func (f *fakePlayerSession) Close() { f.closed.Store(true) }

// liveServer builds a Server whose listed tenants have an injected fake session
// (generation 1), so live-session handlers run without a browser.
func liveServer(t *testing.T, keys map[string]string, sessions map[string]*fakePlayerSession) *Server {
	t.Helper()
	tn := minter.NewTenants(nil, "v", keys, browser.Options{}, 0, 0)
	for key, sess := range sessions {
		if _, err := tn.InjectSessionForTest(context.Background(), key, sess); err != nil {
			t.Fatalf("inject session for %q: %v", key, err)
		}
	}
	return &Server{tenants: tn, log: slog.New(slog.DiscardHandler)}
}

func TestPlayerContextEchoesGeneration(t *testing.T) {
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}})
	r := httptest.NewRequest(http.MethodPost, "/player-context", strings.NewReader(`{"video_id":"aqz-KE-bpKQ"}`))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["server_abr_streaming_url"] != "https://r/ok" {
		t.Errorf("server_abr_streaming_url = %v, want the context URL (embedded fields must stay top-level)", resp["server_abr_streaming_url"])
	}
	if resp["session_generation"] != float64(1) {
		t.Errorf("session_generation = %v, want 1", resp["session_generation"])
	}
}

func TestSessionEchoesGeneration(t *testing.T) {
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd-x"}})
	r := httptest.NewRequest(http.MethodGet, "/session", nil)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["visitor_data"] != "vd-x" {
		t.Errorf("visitor_data = %v", resp["visitor_data"])
	}
	if resp["session_generation"] != float64(1) {
		t.Errorf("session_generation = %v, want 1", resp["session_generation"])
	}
}

func TestPingHealthFields(t *testing.T) {
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}})
	r := httptest.NewRequest(http.MethodGet, "/ping", nil)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}
	if resp["attest"] != "integrity" {
		t.Errorf("attest = %v, want integrity", resp["attest"])
	}
	if resp["generation"] != float64(1) {
		t.Errorf("generation = %v, want 1", resp["generation"])
	}
	if resp["browser_proof_established"] != false {
		t.Errorf("browser_proof_established = %v, want false", resp["browser_proof_established"])
	}
	for _, k := range []string{"browser_proof_established", "last_browser_proof_outcome", "streaming_suspect"} {
		if _, ok := resp[k]; !ok {
			t.Errorf("/ping missing field %q", k)
		}
	}
}

func TestHandleReportLive(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd"}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(`{"session_generation":1,"video_id":"aqz-KE-bpKQ","reason":"incomplete-stream"}`))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["accepted"] != true {
		t.Errorf("accepted = %v, want true", resp["accepted"])
	}
	if resp["retired"] != true && resp["retirement_pending"] != true {
		t.Errorf("want retired or retirement_pending, got %v", resp)
	}
	if resp["generation"] != float64(1) {
		t.Errorf("generation = %v, want 1", resp["generation"])
	}
	if !sess.closed.Load() {
		t.Error("an idle reported session should be retired")
	}
}

func TestHandleReportValidation(t *testing.T) {
	newSrv := func() *Server {
		return &Server{tenants: minter.NewTenants(nil, "", map[string]string{"K": "alice"}, browser.Options{}, 0, 0), log: slog.New(slog.DiscardHandler)}
	}
	t.Run("no auth is 401", func(t *testing.T) {
		s := newSrv()
		r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(`{"session_generation":1}`))
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})
	t.Run("non-POST is 405", func(t *testing.T) {
		s := newSrv()
		r := httptest.NewRequest(http.MethodGet, "/report", nil)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", w.Code)
		}
	})
	bad := []struct{ name, body, wantContains string }{
		{"missing generation", `{"reason":"x"}`, "session_generation is required"},
		{"zero generation", `{"session_generation":0}`, "session_generation is required"},
		{"bad video_id", `{"session_generation":1,"video_id":"bad/../x"}`, "video_id"},
		{"reason bad charset", `{"session_generation":1,"reason":"has space"}`, "reason must contain"},
		{"reason too long", `{"session_generation":1,"reason":"` + strings.Repeat("a", 65) + `"}`, "reason must contain"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			s := newSrv()
			r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(tt.body))
			r.Header.Set("X-API-Key", "K")
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body)
			}
			var env struct{ Error, Code string }
			json.Unmarshal(w.Body.Bytes(), &env)
			if env.Code != CodeInvalidRequest {
				t.Errorf("code = %q, want %q", env.Code, CodeInvalidRequest)
			}
			if !strings.Contains(env.Error, tt.wantContains) {
				t.Errorf("message = %q, want it to contain %q", env.Error, tt.wantContains)
			}
		})
	}
}

func TestHandleReportNoSessionReflectsResult(t *testing.T) {
	// A report for an unwarmed tenant is returned as not accepted.
	s := &Server{tenants: minter.NewTenants(nil, "", map[string]string{"K": "alice"}, browser.Options{}, 0, 0), log: slog.New(slog.DiscardHandler)}
	r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(`{"session_generation":1,"reason":"cap"}`))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["accepted"] != false {
		t.Errorf("accepted = %v, want false (no live session)", resp["accepted"])
	}
	if resp["generation"] != float64(0) {
		t.Errorf("generation = %v, want 0", resp["generation"])
	}
}

func TestHandleReportRateLimited(t *testing.T) {
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}})
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("X-API-Key", "K")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	if w := post("/report", `{"session_generation":1,"reason":"cap"}`); w.Code != http.StatusOK {
		t.Fatalf("first report status = %d", w.Code)
	}
	// Relaunch to a live generation 2.
	pcW := post("/player-context", `{"video_id":"aqz-KE-bpKQ"}`)
	var pc map[string]any
	json.Unmarshal(pcW.Body.Bytes(), &pc)
	gen2, _ := pc["session_generation"].(float64)
	if gen2 != 2 {
		t.Fatalf("relaunch generation = %v, want 2", pc["session_generation"])
	}
	w := post("/report", fmt.Sprintf(`{"session_generation":%d,"reason":"cap"}`, int(gen2)))
	if w.Code != http.StatusOK {
		t.Fatalf("rate-limited report status = %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a rate-limited report must carry a Retry-After header")
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["accepted"] != false {
		t.Errorf("accepted = %v, want false", resp["accepted"])
	}
	if ra, _ := resp["retry_after_seconds"].(float64); ra <= 0 {
		t.Errorf("retry_after_seconds = %v, want > 0", resp["retry_after_seconds"])
	}
}

func TestHandleReportCrossTenant(t *testing.T) {
	alice := &fakePlayerSession{abrURL: "https://r/a", vd: "vd-a"}
	bob := &fakePlayerSession{abrURL: "https://r/b", vd: "vd-b"}
	s := liveServer(t, map[string]string{"KA": "alice", "KB": "bob"}, map[string]*fakePlayerSession{"KA": alice, "KB": bob})

	r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(`{"session_generation":1,"reason":"cap"}`))
	r.Header.Set("X-API-Key", "KA")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("alice report status = %d", w.Code)
	}
	if !alice.closed.Load() {
		t.Error("alice's session should be retired by her own report")
	}
	if bob.closed.Load() {
		t.Error("bob's session must not be touched by alice's report")
	}
	// Bob's session is still live.
	pr := httptest.NewRequest(http.MethodGet, "/ping", nil)
	pr.Header.Set("X-API-Key", "KB")
	pw := httptest.NewRecorder()
	s.routes().ServeHTTP(pw, pr)
	var resp map[string]any
	json.Unmarshal(pw.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Errorf("bob /ping ok = %v, want true (bob unaffected)", resp["ok"])
	}
}

func TestGetPotDecodeMessages(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"empty", "", "request body is empty"},
		{"truncated object", `{"content_binding":`, "request body is truncated (incomplete JSON)"},
		{"malformed no eof", `{"content_binding": }`, "request body contains malformed JSON"},
		{"trailing comma", `{"content_binding":"x",}`, "request body contains malformed JSON"},
		{"unterminated string", `{"content_binding": "hi`, "request body is truncated (incomplete JSON)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := postGetPot(c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body)
			}
			var env struct{ Error, Code string }
			json.Unmarshal(w.Body.Bytes(), &env)
			if env.Error != c.want {
				t.Errorf("message = %q, want %q", env.Error, c.want)
			}
		})
	}
}

func TestPlayerContextEmptyBodyReportsMissingVideoID(t *testing.T) {
	s := &Server{tenants: minter.NewTenants(nil, "", map[string]string{"K": "alice"}, browser.Options{}, 0, 0), log: slog.New(slog.DiscardHandler)}
	r := httptest.NewRequest(http.MethodPost, "/player-context", strings.NewReader("")) // empty body, no query
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var env struct{ Error, Code string }
	json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error != "video_id is required" {
		t.Errorf("message = %q, want 'video_id is required'", env.Error)
	}
}
