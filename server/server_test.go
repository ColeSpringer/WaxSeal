package server

import (
	"context"
	"crypto/sha256"
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

// setMetricsKey configures the operator metrics key the same way server.New
// does, without launching a browser.
func setMetricsKey(s *Server, key string) {
	s.metricsKeyed = true
	s.metricsKeyHash = sha256.Sum256([]byte(key))
}

func TestParseTenantKeys(t *testing.T) {
	for _, in := range []string{"", "  "} {
		if got, err := ParseTenantKeys(in); got != nil || err != nil {
			t.Errorf("ParseTenantKeys(%q) = (%v, %v), want (nil, nil)", in, got, err)
		}
	}

	for _, in := range []string{
		"alice=,bob=",            // every key empty
		"=,=",                    // every label and key empty
		" = ",                    // whitespace label and key
		"alice=KEYA, bob=",       // one dropped pair
		"=KEYA",                  // empty label
		"alice=KEYA, bob=KEYA",   // duplicate key
		"alice=KEYA, alice=KEYB", // duplicate label collapses two identities
	} {
		got, err := ParseTenantKeys(in)
		if err == nil {
			t.Errorf("ParseTenantKeys(%q) = (%v, nil), want a usage error", in, got)
		}
		if err != nil && strings.Contains(err.Error(), "KEY") {
			t.Errorf("ParseTenantKeys(%q) error leaks key material: %q", in, err)
		}
	}

	m, err := ParseTenantKeys("alice=KEYA, bob=KEYB")
	if err != nil || len(m) != 2 || m["KEYA"] != "alice" || m["KEYB"] != "bob" {
		t.Errorf("labelled keys = (%v, %v)", m, err)
	}
	if tc, err := ParseTenantKeys("alice=KEYA,"); err != nil || len(tc) != 1 || tc["KEYA"] != "alice" {
		t.Errorf("trailing comma = (%v, %v), want one tenant", tc, err)
	}
	bare, err := ParseTenantKeys("RAWKEY")
	if err != nil {
		t.Fatalf("bare key: %v", err)
	}
	if lbl := bare["RAWKEY"]; lbl == "" || lbl == "RAWKEY" {
		t.Errorf("bare key label = %q, want a generated label that is not the key", lbl)
	}

	// Generated labels must not collide with explicit labels.
	for _, in := range []string{"t2=KEYA, KEYB", "KEYB, t1=KEYA"} {
		mix, err := ParseTenantKeys(in)
		if err != nil {
			t.Errorf("ParseTenantKeys(%q) = error %v, want two distinct tenants", in, err)
			continue
		}
		if len(mix) != 2 || mix["KEYA"] == "" || mix["KEYB"] == "" || mix["KEYA"] == mix["KEYB"] {
			t.Errorf("ParseTenantKeys(%q) = %v, want two distinct non-empty labels", in, mix)
		}
	}
}

func TestMetricsKeyCollision(t *testing.T) {
	keys := map[string]string{"KEYA": "alice", "KEYB": "bob"}
	// A metrics key equal to a tenant key collides and yields that tenant's label.
	if label, collides := MetricsKeyCollision(keys, "KEYA"); !collides || label != "alice" {
		t.Errorf("MetricsKeyCollision(KEYA) = (%q, %v), want (alice, true)", label, collides)
	}
	// A distinct operator key does not collide.
	if label, collides := MetricsKeyCollision(keys, "OPSKEY"); collides {
		t.Errorf("MetricsKeyCollision(OPSKEY) = (%q, true), want no collision", label)
	}
	// An empty metrics key never collides (no operator key configured).
	if _, collides := MetricsKeyCollision(keys, ""); collides {
		t.Error("empty metrics key reported a collision")
	}
	// A keyless registry has no tenant keys, so nothing collides.
	if _, collides := MetricsKeyCollision(nil, "OPSKEY"); collides {
		t.Error("keyless registry reported a collision")
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
		// Registered routes and their method fallbacks must beat the catch-all.
		{http.MethodGet, "/nope", "/"},
		{http.MethodPost, "/get_pot/", "/"},
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

// TestNotFoundJSONEnvelope verifies that unknown canonical paths use the
// structured 404 while method enforcement, auth, and ServeMux path cleaning keep
// their existing behavior.
func TestNotFoundJSONEnvelope(t *testing.T) {
	mux := (&Server{}).routes()

	// Unknown paths and trailing-slash mismatches both return the JSON 404.
	for _, tt := range []struct{ name, method, path string }{
		{"unknown path", http.MethodGet, "/nope"},
		{"trailing slash", http.MethodPost, "/get_pot/"},
	} {
		r := httptest.NewRequest(tt.method, tt.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (no redirect)", tt.name, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type = %q, want application/json", tt.name, ct)
		}
		var env struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: error body is not JSON: %v (%q)", tt.name, err, w.Body.String())
		}
		if env.Code != CodeNotFound {
			t.Errorf("%s: code = %q, want %q", tt.name, env.Code, CodeNotFound)
		}
		if env.Error == "" {
			t.Errorf("%s: error message is empty", tt.name)
		}
	}

	// Known paths with bad methods still use the method handler.
	r := httptest.NewRequest(http.MethodGet, "/get_pot", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /get_pot: status = %d, want 405 from method handler", w.Code)
	}

	// Unknown paths on keyed daemons return 404 without an auth challenge.
	keyed := &Server{
		tenants: minter.NewTenants(nil, "", map[string]string{"GOODKEY": "alice"}, browser.Options{}, 0, 0),
		log:     slog.New(slog.DiscardHandler),
	}
	r = httptest.NewRequest(http.MethodGet, "/nope", nil) // no API key
	w = httptest.NewRecorder()
	keyed.routes().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown path on keyed daemon: status = %d, want 404 before auth", w.Code)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("keyed 404 body is not JSON: %v (%q)", err, w.Body.String())
	}
	if env.Code != CodeNotFound {
		t.Errorf("keyed unknown path code = %q, want %q", env.Code, CodeNotFound)
	}

	// ServeMux redirects non-canonical paths before dispatch, so those requests do
	// not receive the JSON envelope.
	for _, p := range []string{"/foo/../bar", "//get_pot", "/get_pot/."} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusTemporaryRedirect {
			t.Errorf("%s: status = %d, want 307 from ServeMux path cleaning", p, w.Code)
		}
		if loc := w.Header().Get("Location"); loc == "" {
			t.Errorf("%s: redirect has no Location header", p)
		}
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
	pingErr     error // non-nil makes Ping fail, so a probe failure can be injected
	closed      atomic.Bool
}

func (f *fakePlayerSession) Mint(context.Context, string) (browser.MintResult, error) {
	return browser.MintResult{Kind: "integrity", Lifetime: 3600}, nil
}
func (f *fakePlayerSession) PlayerContext(context.Context, string) (browser.PlayerContext, error) {
	return browser.PlayerContext{PlayabilityStatus: "OK", ServerAbrStreamingURL: f.abrURL, VisitorData: f.vd}, nil
}
func (f *fakePlayerSession) EnsureEstablished(context.Context) error { return nil }
func (f *fakePlayerSession) Ping(context.Context) error              { return f.pingErr }
func (f *fakePlayerSession) AttestKind() string                      { return "integrity" }
func (f *fakePlayerSession) Identity() browser.Identity {
	return browser.Identity{VisitorData: f.vd, UserAgent: "UA", ClientVersion: "2.x"}
}
func (f *fakePlayerSession) BrowserCookies(context.Context) ([]*http.Cookie, error) {
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

// aggregateCounter reads one lifetime counter from the redacted /metrics aggregate
// (no operator key presented).
func aggregateCounter(t *testing.T, s *Server, name string) float64 {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", w.Code)
	}
	var resp struct {
		Aggregate map[string]any `json:"aggregate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /metrics: %v", err)
	}
	v, ok := resp.Aggregate[name].(float64)
	if !ok {
		t.Fatalf("/metrics aggregate has no numeric %q (body=%s)", name, w.Body.String())
	}
	return v
}

// TestPlayerContextRecyclesStaleStreamingSession exercises the HTTP boundary
// after the streaming deadline has passed. The handler recycles the stale
// session, echoes the new generation, and surfaces the recycle through /metrics.
// The deadline is forced into the past to keep the test deterministic.
func TestPlayerContextRecyclesStaleStreamingSession(t *testing.T) {
	ctx := context.Background()
	tn := minter.NewTenants(nil, "v", map[string]string{"K": "alice"}, browser.Options{}, 0, 0)
	m, err := tn.InjectSessionForTest(ctx, "K", &fakePlayerSession{abrURL: "https://r/ok", vd: "vd"})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	m.ExpireStreamingDeadlineForTest()
	s := &Server{tenants: tn, log: slog.New(slog.DiscardHandler)}

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
	if resp["session_generation"] != float64(2) {
		t.Errorf("session_generation = %v, want 2 (stale streaming session recycled on handoff)", resp["session_generation"])
	}
	if got := aggregateCounter(t, s, "streaming_recycles"); got != 1 {
		t.Errorf("streaming_recycles = %v, want 1", got)
	}
}

// TestMetricsSurfacesCacheEvictions confirms that cache_evictions reaches
// /metrics after a capacity eviction. It ties the minter's unit-covered cache
// behavior to the operator-visible metrics contract.
func TestMetricsSurfacesCacheEvictions(t *testing.T) {
	ctx := context.Background()
	tn := minter.NewTenants(nil, "v", map[string]string{"K": "alice"}, browser.Options{}, 0, 0)
	m, err := tn.InjectSessionForTest(ctx, "K", &fakePlayerSession{abrURL: "https://r/ok", vd: "vd"})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	m.FillCachePastBoundForTest()
	s := &Server{tenants: tn, log: slog.New(slog.DiscardHandler)}

	if got := aggregateCounter(t, s, "cache_evictions"); got < 1 {
		t.Errorf("cache_evictions = %v, want >= 1 (eviction surfaces through /metrics)", got)
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
	// /ping reports health without returning the guest identity.
	if _, ok := resp["identity"]; ok {
		t.Error("/ping leaks identity; it must report health only (use /session for identity)")
	}
	if v, ok := resp["navigator_webdriver"]; !ok || v != false {
		t.Errorf("navigator_webdriver = %v (present=%v), want present with value false", v, ok)
	}
	if resp["reason"] != "ok" {
		t.Errorf("reason = %v, want \"ok\" on a healthy ping", resp["reason"])
	}
	for _, k := range []string{"ok", "attest", "generation", "navigator_webdriver", "browser_proof_established", "last_browser_proof_outcome", "streaming_suspect", "reason"} {
		if _, ok := resp[k]; !ok {
			t.Errorf("/ping missing field %q", k)
		}
	}
}

func TestMetricsSchemaStableAfterReport(t *testing.T) {
	s := liveServer(t, nil, map[string]*fakePlayerSession{"": {abrURL: "https://r/ok", vd: "vd"}})

	rep := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(`{"session_generation":1,"reason":"e2e"}`))
	rw := httptest.NewRecorder()
	s.routes().ServeHTTP(rw, rep)
	if rw.Code != http.StatusOK {
		t.Fatalf("/report status = %d, body = %s", rw.Code, rw.Body)
	}

	mr := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mw := httptest.NewRecorder()
	s.routes().ServeHTTP(mw, mr)
	if mw.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", mw.Code)
	}
	var resp struct {
		PerTenant map[string]map[string]any `json:"per_tenant"`
	}
	if err := json.Unmarshal(mw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /metrics: %v", err)
	}
	tenant, ok := resp.PerTenant["default"]
	if !ok {
		t.Fatalf("per_tenant missing the keyless \"default\" tenant: %v", resp.PerTenant)
	}
	if tenant["session_live"] != false {
		t.Fatalf("session_live = %v, want false (the report should have retired the session)", tenant["session_live"])
	}
	// These fields remain present even when no session is live.
	for _, k := range []string{"browser_proof_established", "streaming_suspect"} {
		v, present := tenant[k]
		if !present {
			t.Errorf("%q absent after retire, want present (false) for a stable schema", k)
		} else if v != false {
			t.Errorf("%q = %v, want false", k, v)
		}
	}
	// Detail fields stay present after retirement. liveServer builds with
	// streamingMaxAge == 0, so the recycle field stays absent because time-based
	// recycling is disabled.
	wantSentinel := map[string]any{
		"last_browser_proof_outcome":  "",
		"last_browser_proof_age_secs": nil,
		"streaming_suspect_video":     "",
	}
	for k, want := range wantSentinel {
		v, present := tenant[k]
		if !present {
			t.Errorf("%q absent after retire, want present (%v) for a stable schema", k, want)
		} else if v != want {
			t.Errorf("%q = %v, want %v after retire", k, v, want)
		}
	}
	if _, present := tenant["streaming_seconds_until_recycle"]; present {
		t.Error("streaming_seconds_until_recycle present though recycling is disabled (streamingMaxAge == 0)")
	}
}

func TestMetricsRedaction(t *testing.T) {
	keys := map[string]string{"KA": "alice", "KB": "bob"}
	s := liveServer(t, keys, map[string]*fakePlayerSession{
		"KA": {abrURL: "https://r/a", vd: "vd-a"},
		"KB": {abrURL: "https://r/b", vd: "vd-b"},
	})
	setMetricsKey(s, "OPSKEY")

	get := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	// Drive asymmetric activity so the per-tenant counters differ and a real sum
	// is exercised (alice mints twice, bob once).
	mint := func(key, binding string) {
		r := httptest.NewRequest(http.MethodPost, "/get_pot", strings.NewReader(`{"content_binding":"`+binding+`"}`))
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("get_pot(%s,%s) status = %d, body=%s", key, binding, w.Code, w.Body)
		}
	}
	mint("KA", "vid-a1")
	mint("KA", "vid-a2")
	mint("KB", "vid-b1")

	// Full per-tenant detail with the operator key (always HTTP 200).
	fullW := get("OPSKEY")
	if fullW.Code != http.StatusOK {
		t.Fatalf("ops-key /metrics status = %d", fullW.Code)
	}
	var full struct {
		Tenants   float64                   `json:"tenants"`
		PerTenant map[string]map[string]any `json:"per_tenant"`
		Redacted  bool                      `json:"redacted"`
	}
	if err := json.Unmarshal(fullW.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode full: %v", err)
	}
	if full.Redacted {
		t.Error("ops-key view is redacted, want full per-tenant detail")
	}
	if full.Tenants != 2 || len(full.PerTenant) != 2 {
		t.Fatalf("full view tenants=%v per_tenant=%d, want 2/2", full.Tenants, len(full.PerTenant))
	}
	for _, label := range []string{"alice", "bob"} {
		if _, ok := full.PerTenant[label]; !ok {
			t.Errorf("full view missing tenant %q", label)
		}
	}

	// An unauthenticated scrape and a tenant key both get the redacted aggregate,
	// at HTTP 200, with no labels and no per-tenant breakdown.
	for _, key := range []string{"", "KA"} {
		w := get(key)
		if w.Code != http.StatusOK {
			t.Fatalf("redacted /metrics (key=%q) status = %d, want 200", key, w.Code)
		}
		var red struct {
			Redacted  bool           `json:"redacted"`
			Aggregate map[string]any `json:"aggregate"`
			PerTenant any            `json:"per_tenant"`
			Tenants   any            `json:"tenants"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &red); err != nil {
			t.Fatalf("decode redacted (key=%q): %v", key, err)
		}
		if !red.Redacted {
			t.Errorf("key=%q: redacted = false, want true (tenant keys do not unlock detail)", key)
		}
		if red.PerTenant != nil || red.Tenants != nil {
			t.Errorf("key=%q: redacted view leaks per_tenant/tenants", key)
		}
		if red.Aggregate == nil {
			t.Fatalf("key=%q: redacted view has no aggregate", key)
		}
		if body := w.Body.String(); strings.Contains(body, "alice") || strings.Contains(body, "bob") {
			t.Errorf("key=%q: redacted body leaks a tenant label: %s", key, body)
		}
		// Every aggregate counter equals the per-tenant sum.
		for k, rawAgg := range red.Aggregate {
			aggV, ok := rawAgg.(float64)
			if !ok {
				t.Errorf("key=%q: aggregate[%q] is %T, want a number", key, k, rawAgg)
				continue
			}
			av, _ := full.PerTenant["alice"][k].(float64)
			bv, _ := full.PerTenant["bob"][k].(float64)
			if aggV != av+bv {
				t.Errorf("key=%q: aggregate[%q] = %v, want %v (alice %v + bob %v)", key, k, aggV, av+bv, av, bv)
			}
		}
	}

	// --metrics-public serves full detail unauthenticated.
	pub := liveServer(t, keys, map[string]*fakePlayerSession{
		"KA": {abrURL: "https://r/a", vd: "vd-a"},
		"KB": {abrURL: "https://r/b", vd: "vd-b"},
	})
	pub.metricsPublic = true
	pubW := httptest.NewRecorder()
	pub.routes().ServeHTTP(pubW, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var pubResp struct {
		PerTenant map[string]any `json:"per_tenant"`
		Redacted  bool           `json:"redacted"`
	}
	json.Unmarshal(pubW.Body.Bytes(), &pubResp)
	if pubResp.Redacted || len(pubResp.PerTenant) != 2 {
		t.Errorf("--metrics-public view = redacted=%v per_tenant=%d, want full with 2 tenants", pubResp.Redacted, len(pubResp.PerTenant))
	}

	// A keyless daemon serves full detail with no key.
	keyless := liveServer(t, nil, map[string]*fakePlayerSession{"": {abrURL: "https://r/x", vd: "vd"}})
	klW := httptest.NewRecorder()
	keyless.routes().ServeHTTP(klW, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var klResp struct {
		PerTenant map[string]any `json:"per_tenant"`
		Redacted  bool           `json:"redacted"`
	}
	json.Unmarshal(klW.Body.Bytes(), &klResp)
	if klResp.Redacted || len(klResp.PerTenant) != 1 {
		t.Errorf("keyless view = redacted=%v per_tenant=%d, want full with 1 tenant", klResp.Redacted, len(klResp.PerTenant))
	}
}

// TestNewRejectsMetricsKeyCollision checks the public constructor path: a
// metrics key equal to a tenant key is rejected before the browser launches, and
// the error names the tenant label without leaking the key.
func TestNewRejectsMetricsKeyCollision(t *testing.T) {
	_, err := New(Config{
		TenantKeys: map[string]string{"TENANTKEY": "alice"},
		MetricsKey: "TENANTKEY",
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("New accepted a metrics key that collides with a tenant key")
	}
	if !strings.Contains(err.Error(), "alice") {
		t.Errorf("error = %v, want it to name the colliding tenant label", err)
	}
	if strings.Contains(err.Error(), "TENANTKEY") {
		t.Errorf("error leaks key material: %v", err)
	}
}

// TestPingReason covers the machine-readable /ping reason across keyless and
// keyed daemons: no-session versus probe-failed.
func TestPingReason(t *testing.T) {
	probeErr := errors.New("cdp connection closed")
	cases := []struct {
		name string
		keys map[string]string
		key  string // request API key ("" in keyless mode)
	}{
		{"keyless", nil, ""},
		{"keyed", map[string]string{"K": "alice"}, "K"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ping := func(s *Server) map[string]any {
				r := httptest.NewRequest(http.MethodGet, "/ping", nil)
				if tc.key != "" {
					r.Header.Set("X-API-Key", tc.key)
				}
				w := httptest.NewRecorder()
				s.routes().ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", w.Code)
				}
				var resp map[string]any
				json.Unmarshal(w.Body.Bytes(), &resp)
				return resp
			}

			// No session yet: benign no-session.
			noSess := &Server{tenants: minter.NewTenants(nil, "", tc.keys, browser.Options{}, 0, 0), log: slog.New(slog.DiscardHandler)}
			if resp := ping(noSess); resp["ok"] != false || resp["reason"] != "no-session" {
				t.Errorf("no-session: ok=%v reason=%v, want false/no-session", resp["ok"], resp["reason"])
			}

			// An injected session whose probe fails: probe-failed.
			s := liveServer(t, tc.keys, map[string]*fakePlayerSession{tc.key: {abrURL: "https://r/x", vd: "vd", pingErr: probeErr}})
			if resp := ping(s); resp["ok"] != false || resp["reason"] != "probe-failed" {
				t.Errorf("probe-failed: ok=%v reason=%v, want false/probe-failed", resp["ok"], resp["reason"])
			}
		})
	}
}

// TestStrictPingParsing covers how the ?strict query parameter is read: a bare
// flag enables it, common truthy spellings enable it, and absence or explicit
// false values disable it.
func TestStrictPingParsing(t *testing.T) {
	cases := map[string]bool{
		"/ping":              false, // absent
		"/ping?strict":       true,  // bare flag (no value)
		"/ping?strict=":      true,  // present, empty value
		"/ping?strict=true":  true,
		"/ping?strict=1":     true,
		"/ping?strict=True":  true, // ParseBool accepts these spellings
		"/ping?strict=t":     true,
		"/ping?strict=false": false,
		"/ping?strict=0":     false,
		"/ping?strict=nope":  false, // unparseable values are disabled
	}
	for target, want := range cases {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if got := strictPing(r); got != want {
			t.Errorf("strictPing(%q) = %v, want %v", target, got, want)
		}
	}
}

// TestPingStrict checks the opt-in status-code mapping: ?strict=true returns 503
// only for a real probe failure; no-session and healthy stay 200, and the
// default (no strict) is always 200.
func TestPingStrict(t *testing.T) {
	probeErr := errors.New("cdp connection closed")
	keys := map[string]string{"K": "alice"}

	ping := func(s *Server, strict bool) int {
		u := "/ping"
		if strict {
			u += "?strict=true"
		}
		r := httptest.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("X-API-Key", "K")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w.Code
	}
	newHealthy := func() *Server {
		return liveServer(t, keys, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}})
	}
	newFailing := func() *Server {
		return liveServer(t, keys, map[string]*fakePlayerSession{"K": {abrURL: "https://r/x", vd: "vd", pingErr: probeErr}})
	}
	newNoSession := func() *Server {
		return &Server{tenants: minter.NewTenants(nil, "", keys, browser.Options{}, 0, 0), log: slog.New(slog.DiscardHandler)}
	}

	// With ?strict=true only a real probe failure flips to 503.
	if code := ping(newFailing(), true); code != http.StatusServiceUnavailable {
		t.Errorf("strict probe-failed status = %d, want 503", code)
	}
	if code := ping(newNoSession(), true); code != http.StatusOK {
		t.Errorf("strict no-session status = %d, want 200 (benign)", code)
	}
	if code := ping(newHealthy(), true); code != http.StatusOK {
		t.Errorf("strict healthy status = %d, want 200", code)
	}

	// Without strict, every case stays 200.
	if code := ping(newFailing(), false); code != http.StatusOK {
		t.Errorf("non-strict probe-failed status = %d, want 200", code)
	}
	if code := ping(newNoSession(), false); code != http.StatusOK {
		t.Errorf("non-strict no-session status = %d, want 200", code)
	}
	if code := ping(newHealthy(), false); code != http.StatusOK {
		t.Errorf("non-strict healthy status = %d, want 200", code)
	}
}

func TestGetPotContentBindingTooLong(t *testing.T) {
	body := `{"content_binding":"` + strings.Repeat("a", browser.MaxContentBindingBytes+1) + `"}`
	w := postGetPot(body)
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
	if !strings.Contains(env.Error, "too long") {
		t.Errorf("message = %q, want it to mention 'too long'", env.Error)
	}
}

// TestGetPotRejectsControlChars covers decoded control characters that are valid
// JSON but invalid content_binding values.
func TestGetPotRejectsControlChars(t *testing.T) {
	for _, body := range []string{
		`{"content_binding":"a\nb"}`,       // raw Go string: \n reaches the server as two bytes
		"{\"content_binding\":\"a\x7fb\"}", // a literal DEL byte the decoder accepts
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
		if !strings.Contains(env.Error, "control characters") {
			t.Errorf("body %q: message = %q, want it to mention 'control characters'", body, env.Error)
		}
	}
}

// TestGetPotRawControlRejectedByDecoder checks that raw C0 bytes fail JSON
// decoding before content_binding validation runs.
func TestGetPotRawControlRejectedByDecoder(t *testing.T) {
	w := postGetPot("{\"content_binding\":\"a\nb\"}") // real newline byte, json.SyntaxError
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
	if !strings.Contains(env.Error, "malformed JSON") {
		t.Errorf("message = %q, want it to mention 'malformed JSON'", env.Error)
	}
	if strings.Contains(env.Error, "control characters") {
		t.Errorf("message = %q, raw C0 bytes must be rejected before content_binding validation", env.Error)
	}
}

// TestGetPotInvalidScopeMessage keeps the invalid-scope response in sync with
// the values accepted by normalizeScope.
func TestGetPotInvalidScopeMessage(t *testing.T) {
	w := postGetPot(`{"content_binding":"x","scope":"bogus"}`)
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
	want := `scope must be "player", "gvs", "pot", or omitted`
	if env.Error != want {
		t.Errorf("message = %q, want exactly %q", env.Error, want)
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
