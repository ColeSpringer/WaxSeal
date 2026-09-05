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
	"sync"
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

// TestAPIKeyExtraction pins where a tenant key may be presented and which source
// wins. The cases that matter beyond the happy path are the ones where a header
// carries no usable key: it has to fall through to ?key= instead of resolving to
// the empty key, which a keyed daemon would 401.
func TestAPIKeyExtraction(t *testing.T) {
	for _, tt := range []struct {
		name  string
		auth  string // Authorization header, sent only when non-empty
		key   string // X-API-Key header, sent only when non-empty
		query string // request target
		want  string
	}{
		{name: "X-API-Key", key: "H", query: "/", want: "H"},
		{name: "bearer", auth: "Bearer B", query: "/", want: "B"},
		{name: "query", query: "/?key=Q", want: "Q"},
		{name: "nothing", query: "/", want: ""},

		// Precedence, most preferred source first.
		{name: "header beats bearer", key: "H", auth: "Bearer B", query: "/?key=Q", want: "H"},
		{name: "bearer beats query", auth: "Bearer B", query: "/?key=Q", want: "B"},

		// RFC 7235 makes the scheme case insensitive.
		{name: "lowercase scheme", auth: "bearer B", query: "/", want: "B"},
		{name: "mixed-case scheme", auth: "BeArEr B", query: "/?key=Q", want: "B"},

		// A Bearer header with nothing usable in it defers to ?key=.
		{name: "empty bearer falls through", auth: "Bearer ", query: "/?key=Q", want: "Q"},
		{name: "whitespace bearer falls through", auth: "Bearer    \t  ", query: "/?key=Q", want: "Q"},
		{name: "another scheme falls through", auth: "Basic dXNlcjpwdw==", query: "/?key=Q", want: "Q"},
		{name: "scheme with no space falls through", auth: "Bearer", query: "/?key=Q", want: "Q"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.query, nil)
			if tt.key != "" {
				r.Header.Set("X-API-Key", tt.key)
			}
			if tt.auth != "" {
				r.Header.Set("Authorization", tt.auth)
			}
			if got := apiKey(r); got != tt.want {
				t.Errorf("apiKey() = %q, want %q", got, tt.want)
			}
		})
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
		// Lenient decode: an unmodeled body key does not block the query fallback
		// (a typo'd body field is ignored, and the query video_id is used).
		{name: "unknown body field falls through to query", body: `{"videoId":"abc"}`, query: "?video_id=QID", wantID: "QID", wantOK: true},
		// With no query fallback, a typo'd field still surfaces via the required-field
		// check instead of being dropped silently.
		{name: "unknown body field alone is required-error", body: `{"videoId":"abc"}`, wantOK: false, wantCode: http.StatusBadRequest, wantMsg: "video_id is required"},
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
		// HEAD on browser-backed endpoints hits the explicit HEAD pattern (more
		// specific than GET, which would otherwise also serve HEAD).
		{http.MethodHead, "/session", "HEAD /session"},
		{http.MethodHead, "/player-context", "HEAD /player-context"},
		// HEAD on the cheap endpoints still falls through to the GET handler.
		{http.MethodHead, "/ping", "GET /ping"},
		{http.MethodHead, "/metrics", "GET /metrics"},
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
		tenants: minter.NewTenants(nil, "", map[string]string{"GOODKEY": "alice"}, browser.Options{}, 0, 0, 0),
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

// HEAD on the browser-backed endpoints must 405 (ServeMux otherwise maps HEAD to
// the GET handler, driving a real proof/context fetch); HEAD /ping stays a cheap
// liveness probe. The methodNotAllowed handler runs before any tenant lookup, and
// /ping's Health reports no-session without launching a browser, so no Chromium is
// needed.
func TestHeadGate(t *testing.T) {
	s := &Server{
		tenants: minter.NewTenants(nil, "", nil, browser.Options{}, 0, 0, 0), // keyless, no browser
		log:     slog.New(slog.DiscardHandler),
	}
	mux := s.routes()

	for _, tt := range []struct{ path, wantAllow string }{
		{"/session", http.MethodGet},
		{"/player-context", http.MethodGet + ", " + http.MethodPost},
	} {
		r := httptest.NewRequest(http.MethodHead, tt.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("HEAD %s: status = %d, want 405", tt.path, w.Code)
		}
		if got := w.Header().Get("Allow"); got != tt.wantAllow {
			t.Errorf("HEAD %s: Allow = %q, want %q", tt.path, got, tt.wantAllow)
		}
	}

	r := httptest.NewRequest(http.MethodHead, "/ping", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("HEAD /ping: status = %d, want 200 (cheap liveness probe stays working)", w.Code)
	}
}

// newHTTPServer's WriteTimeout must exceed requestProcessTimeout, or it can cut
// off a cold-start full-length player-context fetch. The other timeouts must be
// set.
func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	if srv.WriteTimeout <= requestProcessTimeout {
		t.Errorf("WriteTimeout = %v, want > requestProcessTimeout (%v)", srv.WriteTimeout, requestProcessTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Errorf("timeouts must all be set: ReadHeader=%v Read=%v Idle=%v", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
}

// A large error message (e.g. a wrapped CDP V8 stack trace) must be clamped so the
// envelope stays small enough for the client to parse and Code is preserved.
func TestErrEnvelopeClampPreservesCode(t *testing.T) {
	w := httptest.NewRecorder()
	huge := strings.Repeat("x", 100<<10) // 100 KiB, far past the client's 64 KiB read cap
	writeErr(w, http.StatusBadGateway, CodeMintFailed, "mint failed: "+huge)

	if n := w.Body.Len(); n > maxErrTextBytes+1024 {
		t.Errorf("clamped body = %d bytes, want <= ~%d", n, maxErrTextBytes+1024)
	}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if env.Code != CodeMintFailed {
		t.Errorf("code = %q, want %q (clamp must keep the envelope parseable)", env.Code, CodeMintFailed)
	}
	if !strings.Contains(env.Error, "truncated") {
		t.Error("clamped message should carry the truncation marker")
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
		tenants: minter.NewTenants(nil, "", map[string]string{"GOODKEY": "alice"}, browser.Options{}, 0, 0, 0),
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
		tenants: minter.NewTenants(nil, "", map[string]string{"GOODKEY": "alice"}, browser.Options{}, 0, 0, 0),
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
		{"unknown field", errors.New(`json: unknown field "videoId"`), `request body contains unknown field "videoId"`},
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

// postKeyed sends body to path on a keyed daemon through the full server mux. No
// live session is needed because the decoder runs right after auth.
func postKeyed(path, body string) *httptest.ResponseRecorder {
	s := &Server{
		tenants: minter.NewTenants(nil, "", map[string]string{"K": "alice"}, browser.Options{}, 0, 0, 0),
		log:     slog.New(slog.DiscardHandler),
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

// TestDecodeRejectsUnknownField checks DisallowUnknownFields on /report, whose
// optional fields would otherwise swallow a typo'd key with no error. A client
// typo returns a 400 invalid-request that names the offending key. /get_pot and
// /player-context are excluded here because they are lenient (see
// TestGetPotToleratesBgutilFields and TestPlayerContextVideoID).
func TestDecodeRejectsUnknownField(t *testing.T) {
	cases := []struct {
		name, path, body, want string
	}{
		{"report camelCase typo", "/report", `{"session_generation":1,"videoId":"x"}`, `request body contains unknown field "videoId"`},
		{"report extra field", "/report", `{"session_generation":1,"typo":1}`, `request body contains unknown field "typo"`},
		// Without strict decode, "raeson" would be dropped and the report accepted
		// with no reason at all.
		{"report typo'd optional reason", "/report", `{"session_generation":1,"raeson":"truncated"}`, `request body contains unknown field "raeson"`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			w := postKeyed(tt.path, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (%q)", w.Code, http.StatusBadRequest, w.Body.String())
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
			if env.Error != tt.want {
				t.Errorf("message = %q, want %q", env.Error, tt.want)
			}
		})
	}
}

// TestGetPotToleratesBgutilFields checks that /get_pot stays lenient for the
// bgutil/yt-dlp use case. The plugin's extra request fields are ignored rather
// than rejected. The request reaches the content_binding required-field check,
// which proves the decoder accepted the unknown fields, instead of failing as an
// unknown field.
func TestGetPotToleratesBgutilFields(t *testing.T) {
	// The field set the real yt-dlp bgutil HTTP plugin POSTs, with an empty
	// content_binding so the request stops at the required-field check rather than
	// attempting a real mint.
	body := `{"content_binding":"","bypass_cache":false,"challenge":"c","disable_tls_verification":true,"proxy":"http://p","innertube_context":{"client":{}},"source_address":"0.0.0.0"}`
	w := postGetPot(body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (%q)", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not JSON: %v (%q)", err, w.Body.String())
	}
	if strings.Contains(env.Error, "unknown field") {
		t.Fatalf("bgutil request rejected as an unknown field: %q", env.Error)
	}
	if !strings.HasPrefix(env.Error, "content_binding is required") {
		t.Errorf("message = %q, want the content_binding-required message", env.Error)
	}
}

// TestDecodeSecondObjectStaysGeneric checks that DisallowUnknownFields on the
// second decode does not leak a field name. On a strict endpoint, a trailing
// object still returns the generic single-object message rather than "unknown
// field".
func TestDecodeSecondObjectStaysGeneric(t *testing.T) {
	w := postKeyed("/report", `{"session_generation":1} {"typo":1}`)
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
	if env.Error != "request body must be a single JSON object" {
		t.Errorf("message = %q, want the generic single-object message", env.Error)
	}
	if strings.Contains(env.Error, "typo") {
		t.Errorf("message %q leaked the second object's field name", env.Error)
	}
}

// postGetPot posts body to /get_pot on a keyed daemon through the full server
// mux (see postKeyed).
func postGetPot(body string) *httptest.ResponseRecorder {
	return postKeyed("/get_pot", body)
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
	abrURL          string
	vd              string
	established     bool
	pingErr         error          // non-nil makes Ping fail, so a probe failure can be injected
	mintErr         error          // non-nil makes Mint fail, so the mint ladder can be exercised
	pcErr           error          // non-nil makes PlayerContext fail
	establishErr    error          // non-nil makes EnsureEstablished fail (used by /session)
	mintBlocks      bool           // Mint blocks until ctx is done (to exercise the server timeout)
	pcBlocks        bool           // PlayerContext blocks until ctx is done (/player-context timeout)
	establishBlocks bool           // EnsureEstablished blocks until ctx is done (/session timeout)
	cookies         []*http.Cookie // when non-nil, BrowserCookies returns these
	// mintExpiresAt, when non-zero, is the attested expiry Mint reports. The
	// zero default keeps the response layer's now+6h sentinel exercised.
	mintExpiresAt time.Time
	// pcGate, when set, runs at the top of PlayerContext. A test uses it to keep a
	// request parked inside the minter, which is the only way to hold the page
	// lock the way a real in-flight request does.
	pcGate func(ctx context.Context)
	closed atomic.Bool
}

func (f *fakePlayerSession) Mint(ctx context.Context, _ string) (browser.MintResult, error) {
	if f.mintBlocks {
		<-ctx.Done()
		return browser.MintResult{}, ctx.Err()
	}
	if f.mintErr != nil {
		return browser.MintResult{}, f.mintErr
	}
	return browser.MintResult{Kind: "integrity", Lifetime: 3600, ExpiresAt: f.mintExpiresAt}, nil
}
func (f *fakePlayerSession) PlayerContext(ctx context.Context, _ string) (browser.PlayerContext, error) {
	if f.pcGate != nil {
		f.pcGate(ctx)
	}
	if f.pcBlocks {
		<-ctx.Done()
		return browser.PlayerContext{}, ctx.Err()
	}
	if f.pcErr != nil {
		return browser.PlayerContext{}, f.pcErr
	}
	return browser.PlayerContext{PlayabilityStatus: "OK", ServerAbrStreamingURL: f.abrURL, VisitorData: f.vd}, nil
}
func (f *fakePlayerSession) EnsureEstablished(ctx context.Context) error {
	if f.establishBlocks {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.establishErr
}
func (f *fakePlayerSession) Ping(context.Context) error { return f.pingErr }
func (f *fakePlayerSession) AttestKind() string         { return "integrity" }
func (f *fakePlayerSession) Identity() browser.Identity {
	return browser.Identity{VisitorData: f.vd, UserAgent: "UA", ClientVersion: "2.x"}
}
func (f *fakePlayerSession) BrowserCookies(context.Context) ([]*http.Cookie, error) {
	if f.cookies != nil {
		return f.cookies, nil
	}
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
	tn := minter.NewTenants(nil, "v", keys, browser.Options{}, 0, 0, 0)
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
	tn := minter.NewTenants(nil, "v", map[string]string{"K": "alice"}, browser.Options{}, 0, 0, 0)
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
	tn := minter.NewTenants(nil, "v", map[string]string{"K": "alice"}, browser.Options{}, 0, 0, 0)
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
	// A keyed probe says it checked the tenant's session, so a reader can tell
	// this body from the daemon-level one a keyless probe gets.
	if resp["probe"] != "tenant" {
		t.Errorf("probe = %v, want \"tenant\"", resp["probe"])
	}
	if resp["browser_relaunched"] != false {
		t.Errorf("browser_relaunched = %v, want false when the browser answered", resp["browser_relaunched"])
	}
	for _, k := range []string{"ok", "probe", "attest", "generation", "navigator_webdriver", "browser_proof_established", "last_browser_proof_outcome", "streaming_suspect", "reason", "browser_relaunched"} {
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
			noSess := &Server{tenants: minter.NewTenants(nil, "", tc.keys, browser.Options{}, 0, 0, 0), log: slog.New(slog.DiscardHandler)}
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

// TestBenignPingReason pins which /ping reasons stay healthy under ?strict=true.
// Both the handler's status code and `waxseal ping --strict` read this, so a
// reason added on one side and not the other is how the image's HEALTHCHECK
// starts failing on a healthy daemon.
func TestBenignPingReason(t *testing.T) {
	for _, r := range []string{PingReasonNoSession, PingReasonBusy} {
		if !BenignPingReason(r) {
			t.Errorf("BenignPingReason(%q) = false, want true", r)
		}
	}
	// "ok" arrives with ok:true, which the caller has already accepted. A
	// probe-failed, or a reason-less body from a pre-strict daemon, must still
	// read as unhealthy.
	for _, r := range []string{PingReasonOK, PingReasonProbeFailed, "", "banana"} {
		if BenignPingReason(r) {
			t.Errorf("BenignPingReason(%q) = true, want false", r)
		}
	}
}

// A probe failure that cannot take the page from a running request is reported
// as busy, not probe-failed: nothing was retired, so the failure says as much
// about contention as about the browser. It stays 200 even under ?strict=true,
// for the same reason no-session does. Three of these in a row would otherwise
// mark a healthy container unhealthy while it was simply busy.
func TestPingBusyStaysHealthyUnderStrict(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sess := &fakePlayerSession{
		abrURL:  "https://r/x",
		vd:      "vd",
		pingErr: errors.New("cdp connection closed"),
		pcGate: func(ctx context.Context) {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
			}
		},
	}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})

	// Park a /player-context inside the minter so it holds the page lock, the way
	// a real request does while /ping runs.
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodPost, "/player-context", strings.NewReader(`{"video_id":"aqz-KE-bpKQ"}`))
		r.Header.Set("X-API-Key", "K")
		s.routes().ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-entered
	defer func() { close(release); <-done }()

	r := httptest.NewRequest(http.MethodGet, "/ping?strict=true", nil)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (busy is benign, even under strict)", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["reason"] != "busy" {
		t.Errorf("reason = %v, want busy", resp["reason"])
	}
	if resp["ok"] != false {
		t.Errorf("ok = %v, want false", resp["ok"])
	}
}

// TestStrictPingParsing covers how the ?strict query parameter is read: a bare
// flag enables it, common truthy spellings enable it, absence and explicit false
// values disable it, and anything ParseBool cannot read is rejected rather than
// quietly disabling the mode the operator asked for.
func TestStrictPingParsing(t *testing.T) {
	type want struct{ strict, ok bool }
	cases := map[string]want{
		"/ping":               {false, true}, // absent
		"/ping?strict":        {true, true},  // bare flag (no value)
		"/ping?strict=":       {true, true},  // present, empty value
		"/ping?strict=true":   {true, true},
		"/ping?strict=1":      {true, true},
		"/ping?strict=True":   {true, true}, // ParseBool accepts these spellings
		"/ping?strict=t":      {true, true},
		"/ping?strict=false":  {false, true},
		"/ping?strict=0":      {false, true},
		"/ping?strict=yes":    {false, false}, // ParseBool does not read these
		"/ping?strict=on":     {false, false},
		"/ping?strict=banana": {false, false},
	}
	for target, w := range cases {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		strict, ok := strictPing(r)
		if strict != w.strict || ok != w.ok {
			t.Errorf("strictPing(%q) = (%v, %v), want (%v, %v)", target, strict, ok, w.strict, w.ok)
		}
	}
}

// TestPingStrict checks the opt-in status-code mapping: ?strict=true returns 503
// only for a real probe failure; no-session and healthy stay 200, and the
// default (no strict) is always 200.
func TestPingStrict(t *testing.T) {
	probeErr := errors.New("cdp connection closed")
	keys := map[string]string{"K": "alice"}

	pingTarget := func(s *Server, u string) int {
		r := httptest.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("X-API-Key", "K")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w.Code
	}
	ping := func(s *Server, strict bool) int {
		u := "/ping"
		if strict {
			u += "?strict=true"
		}
		return pingTarget(s, u)
	}
	newHealthy := func() *Server {
		return liveServer(t, keys, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}})
	}
	newFailing := func() *Server {
		return liveServer(t, keys, map[string]*fakePlayerSession{"K": {abrURL: "https://r/x", vd: "vd", pingErr: probeErr}})
	}
	newNoSession := func() *Server {
		return &Server{tenants: minter.NewTenants(nil, "", keys, browser.Options{}, 0, 0, 0), log: slog.New(slog.DiscardHandler)}
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

	// An unparseable value is a request error, not a silent downgrade to
	// non-strict, and it is reported whatever the session's health is.
	for _, u := range []string{"/ping?strict=banana", "/ping?strict=yes", "/ping?strict=on"} {
		if code := pingTarget(newHealthy(), u); code != http.StatusBadRequest {
			t.Errorf("%s on a healthy session: status = %d, want 400", u, code)
		}
		if code := pingTarget(newFailing(), u); code != http.StatusBadRequest {
			t.Errorf("%s on a failing probe: status = %d, want 400", u, code)
		}
	}
}

// TestGetPotWarnsOnURLBinding checks that a URL-shaped content_binding still
// mints (200) but carries an additive warning field, while a bare identifier
// produces no warning key.
func TestGetPotWarnsOnURLBinding(t *testing.T) {
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}})
	post := func(binding string) map[string]any {
		r := httptest.NewRequest(http.MethodPost, "/get_pot", strings.NewReader(`{"content_binding":"`+binding+`"}`))
		r.Header.Set("X-API-Key", "K")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("binding %q: status = %d, body = %s", binding, w.Code, w.Body)
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("binding %q: decode: %v", binding, err)
		}
		return resp
	}
	urlResp := post("https://youtube.com/watch?v=aqz-KE-bpKQ")
	if warn, ok := urlResp["warning"].(string); !ok || warn == "" {
		t.Errorf("URL binding: warning = %v (present=%v), want a non-empty string", urlResp["warning"], ok)
	}
	bareResp := post("aqz-KE-bpKQ")
	if v, present := bareResp["warning"]; present {
		t.Errorf("bare ID: warning = %v, want no warning key", v)
	}
}

// TestGetPotCacheHeaderAndExpiry covers the cache disposition a consumer reads
// off /get_pot: the first request for a binding mints and reports a miss, the
// repeat is served from the cache and reports a hit, and both echo the token's
// own attested expiry rather than one recomputed from the time of the response.
func TestGetPotCacheHeaderAndExpiry(t *testing.T) {
	// Whole seconds, because the response formats with RFC3339.
	expires := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{
		"K": {abrURL: "https://r/ok", vd: "vd", mintExpiresAt: expires},
	})
	get := func() (string, time.Time) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/get_pot", strings.NewReader(`{"content_binding":"aqz-KE-bpKQ"}`))
		r.Header.Set("X-API-Key", "K")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body)
		}
		var resp struct {
			ExpiresAt time.Time `json:"expiresAt"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return w.Header().Get("X-POT-Cache"), resp.ExpiresAt
	}

	disposition, exp := get()
	if disposition != "miss" {
		t.Errorf("first request: X-POT-Cache = %q, want %q", disposition, "miss")
	}
	if !exp.Equal(expires) {
		t.Errorf("first request: expiresAt = %v, want the token's own %v", exp, expires)
	}
	disposition, exp = get()
	if disposition != "hit" {
		t.Errorf("repeat request: X-POT-Cache = %q, want %q", disposition, "hit")
	}
	if !exp.Equal(expires) {
		t.Errorf("repeat request: expiresAt = %v, want the token's own %v (not recomputed on the hit)", exp, expires)
	}
}

// TestGetPotZeroExpiryFallsBackToSixHours covers the response-layer expiry
// sentinel: a mint result with a zero ExpiresAt (the fake session's default)
// yields an expiresAt about now+6h.
func TestGetPotZeroExpiryFallsBackToSixHours(t *testing.T) {
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}})
	before := time.Now()
	r := httptest.NewRequest(http.MethodPost, "/get_pot", strings.NewReader(`{"content_binding":"aqz-KE-bpKQ"}`))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	after := time.Now()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var resp struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The response formats with RFC3339 (whole-second precision), so allow a small
	// slack window around now+6h to absorb the truncation and scheduling jitter.
	const slack = 2 * time.Second
	lo, hi := before.Add(6*time.Hour-slack), after.Add(6*time.Hour+slack)
	if resp.ExpiresAt.Before(lo) || resp.ExpiresAt.After(hi) {
		t.Errorf("expiresAt = %v, want within [%v, %v] (now+6h sentinel)", resp.ExpiresAt, lo, hi)
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
		return &Server{tenants: minter.NewTenants(nil, "", map[string]string{"K": "alice"}, browser.Options{}, 0, 0, 0), log: slog.New(slog.DiscardHandler)}
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
	s := &Server{tenants: minter.NewTenants(nil, "", map[string]string{"K": "alice"}, browser.Options{}, 0, 0, 0), log: slog.New(slog.DiscardHandler)}
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
	// Spend the whole burst allowance: each report recycles, each player-context
	// relaunches the next generation.
	for gen := 1; gen <= minter.ReportBurst; gen++ {
		if w := post("/report", fmt.Sprintf(`{"session_generation":%d,"reason":"cap"}`, gen)); w.Code != http.StatusOK {
			t.Fatalf("report %d status = %d", gen, w.Code)
		}
		pcW := post("/player-context", `{"video_id":"aqz-KE-bpKQ"}`)
		var pc map[string]any
		json.Unmarshal(pcW.Body.Bytes(), &pc)
		if got, _ := pc["session_generation"].(float64); int(got) != gen+1 {
			t.Fatalf("relaunch %d generation = %v, want %d", gen, pc["session_generation"], gen+1)
		}
	}
	w := post("/report", fmt.Sprintf(`{"session_generation":%d,"reason":"cap"}`, minter.ReportBurst+1))
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

// TestHandleReportAlreadyRetiredMetric drives the already-retired disposition end
// to end over HTTP: a first report retires the live generation, then a second
// report for the same still-current generation is a benign no-op that increments
// degradation_reports_already_retired in /metrics without touching rejected_stale.
func TestHandleReportAlreadyRetiredMetric(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd"}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	report := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(`{"session_generation":1,"reason":"cap"}`))
		r.Header.Set("X-API-Key", "K")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	if w := report(); w.Code != http.StatusOK { // retires gen 1; sess→nil, gen stays 1
		t.Fatalf("first report status = %d, body = %s", w.Code, w.Body)
	}
	if !sess.closed.Load() {
		t.Fatal("first report should retire the live session")
	}
	// Second report for the still-current generation whose session is already gone.
	w := report()
	if w.Code != http.StatusOK {
		t.Fatalf("second report status = %d, body = %s", w.Code, w.Body)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["accepted"] != false || resp["generation"] != float64(1) {
		t.Errorf("second report = %v, want accepted=false generation=1", resp)
	}
	if got := aggregateCounter(t, s, "degradation_reports_already_retired"); got != 1 {
		t.Errorf("degradation_reports_already_retired = %v, want 1", got)
	}
	if got := aggregateCounter(t, s, "degradation_reports_rejected_stale"); got != 0 {
		t.Errorf("degradation_reports_rejected_stale = %v, want 0 (current gen, not stale)", got)
	}
	if got := aggregateCounter(t, s, "degradation_reports_rate_limited"); got != 0 {
		t.Errorf("degradation_reports_rate_limited = %v, want 0 (already-retired must not route to debounce)", got)
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
	s := &Server{tenants: minter.NewTenants(nil, "", map[string]string{"K": "alice"}, browser.Options{}, 0, 0, 0), log: slog.New(slog.DiscardHandler)}
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

// capturingHandler records slog records so a test can assert what was (and was
// not) logged. It is safe for the synchronous handler path exercised here.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

// WithAttrs and WithGroup intentionally return the same handler: the capturer
// records every record by level and message only, ignoring attrs and groups, so
// there is no child state to isolate. That keeps assertions simple and is safe for
// these synchronous, single-goroutine tests.
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// attrs returns the attributes of the first record at level with msg, as
// key/value strings in order, and false when there is none. Repeated keys are
// kept, so a caller can see a duplicated attribute.
func (h *capturingHandler) attrs(level slog.Level, msg string) ([][2]string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level != level || r.Message != msg {
			continue
		}
		var out [][2]string
		r.Attrs(func(a slog.Attr) bool {
			out = append(out, [2]string{a.Key, a.Value.String()})
			return true
		})
		return out, true
	}
	return nil, false
}

func (h *capturingHandler) has(level slog.Level, msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && r.Message == msg {
			return true
		}
	}
	return false
}

// TestPresentErr covers the public error text used by HTTP responses. It strips
// the internal "waxseal: " prefix from bare and embedded errors, then clamps long
// messages.
func TestPresentErr(t *testing.T) {
	for in, want := range map[string]string{
		"waxseal: boom":              "boom",
		"mint failed: waxseal: boom": "mint failed: boom",
		"no prefix here":             "no prefix here",
	} {
		if got := presentErr(in); got != want {
			t.Errorf("presentErr(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", maxErrTextBytes+100)
	if got := presentErr(long); !strings.HasSuffix(got, "... [truncated]") {
		t.Errorf("presentErr did not clamp oversized text (len=%d)", len(got))
	}
}

// TestPingClientGoneSuppressesProbeFailure checks that a client disconnect during
// /ping is not reported as a server failure. It should not log a probe-failed
// WARN, return a strict-mode 503, or write a response body.
func TestPingClientGoneSuppressesProbeFailure(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/x", vd: "vd", pingErr: errors.New("cdp connection closed")}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	cap := &capturingHandler{}
	s.log = slog.New(cap)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has gone away
	r := httptest.NewRequest(http.MethodGet, "/ping?strict=true", nil).WithContext(ctx)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)

	if w.Code == http.StatusServiceUnavailable {
		t.Error("status = 503; a client disconnect must not flip ?strict to a probe failure")
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q, want empty (nothing written for an abandoned request)", body)
	}
	if cap.has(slog.LevelWarn, "ping probe failed") {
		t.Error("logged a probe-failed WARN for a client disconnect")
	}
}

// TestPingErrorStripsPrefix checks that /ping's direct error write uses presentErr
// like the error-envelope path.
func TestPingErrorStripsPrefix(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/x", vd: "vd", pingErr: errors.New("waxseal: cdp closed")}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodGet, "/ping", nil)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["reason"] != "probe-failed" {
		t.Errorf("reason = %v, want probe-failed", resp["reason"])
	}
	if resp["error"] != "cdp closed" {
		t.Errorf("error = %v, want %q (internal prefix stripped)", resp["error"], "cdp closed")
	}
}

// TestSameSiteWire maps the http.SameSite enum to the /session wire string.
func TestSameSiteWire(t *testing.T) {
	for _, tc := range []struct {
		in   http.SameSite
		want string
	}{
		{http.SameSiteStrictMode, "Strict"},
		{http.SameSiteLaxMode, "Lax"},
		{http.SameSiteNoneMode, "None"},
		{http.SameSiteDefaultMode, ""},
		{http.SameSite(0), ""},
	} {
		if got := sameSiteWire(tc.in); got != tc.want {
			t.Errorf("sameSiteWire(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSessionExportsCookieExpiryAndSameSite checks that /session exports an
// absolute RFC3339 expiry and same_site, while omitting expires for session
// cookies.
func TestSessionExportsCookieExpiryAndSameSite(t *testing.T) {
	exp := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", cookies: []*http.Cookie{
		{Name: "PREF", Value: "p", Domain: ".youtube.com", Path: "/", Secure: true, SameSite: http.SameSiteLaxMode, Expires: exp},
		{Name: "YSC", Value: "s", Domain: ".youtube.com", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}, // session cookie: zero Expires
	}}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodGet, "/session", nil)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var resp struct {
		Cookies []map[string]any `json:"cookies"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Cookies) != 2 {
		t.Fatalf("cookies = %d, want 2", len(resp.Cookies))
	}
	if got := resp.Cookies[0]["expires"]; got != exp.Format(time.RFC3339) {
		t.Errorf("PREF expires = %v, want %q", got, exp.Format(time.RFC3339))
	}
	if got := resp.Cookies[0]["same_site"]; got != "Lax" {
		t.Errorf("PREF same_site = %v, want Lax", got)
	}
	if _, present := resp.Cookies[1]["expires"]; present {
		t.Errorf("session cookie must omit expires, got %v", resp.Cookies[1]["expires"])
	}
	if got := resp.Cookies[1]["same_site"]; got != "Lax" {
		t.Errorf("YSC same_site = %v, want Lax", got)
	}
}

// TestPlayerContextClientGoneNoMisleading502 checks that a client disconnect
// during /player-context is not reported as a 502 or 504 and writes no body.
func TestPlayerContextClientGoneNoMisleading502(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", pcErr: context.Canceled}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/player-context", strings.NewReader(`{"video_id":"aqz-KE-bpKQ"}`)).WithContext(ctx)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)

	if w.Code == http.StatusBadGateway || w.Code == http.StatusGatewayTimeout {
		t.Errorf("status = %d; a client disconnect must not become a 502/504", w.Code)
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

// TestGetPotClientGoneComposesGuards exercises the handler and minter guards
// together. With a failing mint and a canceled request, the handler writes no
// 502/body and the minter leaves counters and the live session untouched.
func TestGetPotClientGoneComposesGuards(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", mintErr: errors.New("mint boom")}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	mtr, _, err := s.tenants.Minter("K")
	if err != nil {
		t.Fatalf("minter: %v", err)
	}
	gen0 := mtr.Generation()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/get_pot", strings.NewReader(`{"content_binding":"aqz-KE-bpKQ"}`)).WithContext(ctx)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)

	if w.Code == http.StatusBadGateway {
		t.Error("status = 502; a client disconnect must not become a mint-failed 502")
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q, want empty", body)
	}
	if got := aggregateCounter(t, s, "mint_failures"); got != 0 {
		t.Errorf("mint_failures = %v, want 0 (the minter guard trips before counting)", got)
	}
	if got := aggregateCounter(t, s, "escalations"); got != 0 {
		t.Errorf("escalations = %v, want 0 (no relaunch)", got)
	}
	if sess.closed.Load() {
		t.Error("session was retired; a client disconnect must not relaunch Chromium")
	}
	if got := mtr.Generation(); got != gen0 {
		t.Errorf("generation = %d, want %d (no relaunch)", got, gen0)
	}
}

// shortRequestTimeout shrinks requestProcessTimeout for the duration of a test so
// the server's own deadline (not an injected error value) fires while the client
// stays connected. The blocking fakes below return only when this derived context
// is done.
func shortRequestTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := requestProcessTimeout
	requestProcessTimeout = d
	t.Cleanup(func() { requestProcessTimeout = old })
}

func decodeCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return env.Code
}

// TestGetPotServerTimeoutMaps504 exercises the real requestProcessTimeout path.
// The mint blocks until the server's own shortened deadline fires while the
// client stays connected, so /get_pot should report 504/timeout instead of a
// generic 502. The minter guard should return the timeout without counting a mint
// failure or relaunching.
func TestGetPotServerTimeoutMaps504(t *testing.T) {
	shortRequestTimeout(t, 20*time.Millisecond)
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", mintBlocks: true}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodPost, "/get_pot", strings.NewReader(`{"content_binding":"aqz-KE-bpKQ"}`))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if got := decodeCode(t, w.Body.Bytes()); got != CodeTimeout {
		t.Errorf("code = %q, want %q", got, CodeTimeout)
	}
	if got := aggregateCounter(t, s, "mint_failures"); got != 0 {
		t.Errorf("mint_failures = %v, want 0 (a server timeout is not a mint failure)", got)
	}
	if got := aggregateCounter(t, s, "escalations"); got != 0 {
		t.Errorf("escalations = %v, want 0 (no relaunch on server timeout)", got)
	}
	if sess.closed.Load() {
		t.Error("session was retired on a server timeout")
	}
}

// TestPlayerContextServerTimeoutMaps504 checks the same requestProcessTimeout
// mapping for /player-context. The outer budget fires while the client stays
// connected, so the response is 504 rather than 502.
func TestPlayerContextServerTimeoutMaps504(t *testing.T) {
	shortRequestTimeout(t, 20*time.Millisecond)
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", pcBlocks: true}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodPost, "/player-context", strings.NewReader(`{"video_id":"aqz-KE-bpKQ"}`))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if got := decodeCode(t, w.Body.Bytes()); got != CodeTimeout {
		t.Errorf("code = %q, want %q", got, CodeTimeout)
	}
}

// TestSessionServerTimeoutMaps504 checks that /session reports 504/timeout, not
// 503/no-session, when the server's own deadline fires while the client stays
// connected.
func TestSessionServerTimeoutMaps504(t *testing.T) {
	shortRequestTimeout(t, 20*time.Millisecond)
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", establishBlocks: true}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodGet, "/session", nil)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if got := decodeCode(t, w.Body.Bytes()); got != CodeTimeout {
		t.Errorf("code = %q, want %q", got, CodeTimeout)
	}
}

// TestInnerDeadlineIsNot504 checks that an inner browser deadline is not treated
// as the request timeout. When the outer request context is still live, a wrapped
// context.DeadlineExceeded should remain a generic upstream failure.
func TestInnerDeadlineIsNot504(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", mintErr: context.DeadlineExceeded}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodPost, "/get_pot", strings.NewReader(`{"content_binding":"aqz-KE-bpKQ"}`))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code == http.StatusGatewayTimeout {
		t.Fatalf("status = 504, want a non-timeout code (the outer request budget did not fire)")
	}
	if w.Code != http.StatusBadGateway || decodeCode(t, w.Body.Bytes()) != CodeMintFailed {
		t.Errorf("status/code = %d/%q, want 502/%q", w.Code, decodeCode(t, w.Body.Bytes()), CodeMintFailed)
	}
}

// TestPlayerContextUnprovenSessionMaps502 checks that a session which cannot
// prove full-length streaming reaches the client as 502/player-context-failed,
// the same shape any other refused player-context request uses.
func TestPlayerContextUnprovenSessionMaps502(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", establishErr: errors.New("full-length proof failed")}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodPost, "/player-context", strings.NewReader(`{"video_id":"aqz-KE-bpKQ"}`))
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", w.Code, w.Body)
	}
	if got := decodeCode(t, w.Body.Bytes()); got != CodePlayerContextFailed {
		t.Errorf("code = %q, want %q", got, CodePlayerContextFailed)
	}
	if got := aggregateCounter(t, s, "unproven_rejections"); got != 1 {
		t.Errorf("unproven_rejections = %v, want 1", got)
	}
}

// TestSessionUnprovenSessionMaps503 checks that a session which cannot prove
// full-length streaming reaches /session as 503/no-session, the same shape any
// other missing-session response uses. SessionSnapshot routes its proof through
// the same ensureProven ladder /player-context uses, so the failure also counts
// toward unproven_rejections.
func TestSessionUnprovenSessionMaps503(t *testing.T) {
	sess := &fakePlayerSession{abrURL: "https://r/ok", vd: "vd", establishErr: errors.New("full-length proof failed")}
	s := liveServer(t, map[string]string{"K": "alice"}, map[string]*fakePlayerSession{"K": sess})
	r := httptest.NewRequest(http.MethodGet, "/session", nil)
	r.Header.Set("X-API-Key", "K")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", w.Code, w.Body)
	}
	if got := decodeCode(t, w.Body.Bytes()); got != CodeNoSession {
		t.Errorf("code = %q, want %q", got, CodeNoSession)
	}
	if got := aggregateCounter(t, s, "unproven_rejections"); got != 1 {
		t.Errorf("unproven_rejections = %v, want 1", got)
	}
}

// fakeProber stands in for the browser pool behind /ping. health decides each
// check's outcome (nil answers); the counters are what /metrics reports, and a
// health func may bump them the way Pool.Health does.
type fakeProber struct {
	health     func(ctx context.Context) (browser.Recovery, error)
	calls      int
	probes     int64
	relaunches int64
}

func (f *fakeProber) Health(ctx context.Context) (browser.Recovery, error) {
	f.calls++
	if f.health == nil {
		return browser.RecoveryNone, nil
	}
	return f.health(ctx)
}
func (f *fakeProber) ProbeFailures() int64    { return f.probes }
func (f *fakeProber) RelaunchFailures() int64 { return f.relaunches }

// proberOf installs f as s's browser prober and returns f.
func proberOf(s *Server, f *fakeProber) *fakeProber {
	s.tenants.SetBrowserProberForTest(f)
	return f
}

// answer returns a health func with a fixed outcome.
func answer(rec browser.Recovery, err error) func(context.Context) (browser.Recovery, error) {
	return func(context.Context) (browser.Recovery, error) { return rec, err }
}

// pingWith issues GET target with key (or none) and returns the status and the
// decoded body, which is nil when the body is empty.
func pingWith(t *testing.T, s *Server, target, key string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if key != "" {
		r.Header.Set("X-API-Key", key)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Body.Len() == 0 {
		return w.Code, nil
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v (%q)", target, err, w.Body.String())
	}
	return w.Code, resp
}

var keyedOnly = map[string]string{"K": "alice"}

// A keyed daemon answers a probe that presents no key at daemon scope instead of
// 401, so the image's HEALTHCHECK works without being told a tenant key. The
// body says what was probed and carries none of the tenant-level fields.
func TestPingKeylessOnKeyedDaemonProbesTheBrowser(t *testing.T) {
	s := liveServer(t, keyedOnly, nil)
	f := proberOf(s, &fakeProber{})
	code, resp := pingWith(t, s, "/ping", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a keyless probe on a keyed daemon is not unauthorized)", code)
	}
	if f.calls != 1 {
		t.Errorf("browser checks = %d, want 1", f.calls)
	}
	if resp["ok"] != true || resp["reason"] != PingReasonOK || resp["probe"] != PingProbeDaemon || resp["browser_relaunched"] != false {
		t.Errorf("body = %v, want ok:true reason:ok probe:daemon browser_relaunched:false", resp)
	}
	for _, k := range []string{"tenant", "attest", "generation", "navigator_webdriver", "browser_proof_established", "last_browser_proof_outcome", "streaming_suspect", "identity", "error"} {
		if _, leak := resp[k]; leak {
			t.Errorf("daemon-level /ping carries %q; a keyless probe reports liveness only", k)
		}
	}
	// Nothing about the probe creates tenant state.
	if n := s.tenants.MetricsSnapshot()["tenants"]; n != 0 {
		t.Errorf("tenants after a keyless probe = %v, want 0", n)
	}
}

// The daemon-level outcomes. A browser that had exited and was relaunched is
// healthy and says so; a browser this probe found wedged is a loss, probe-failed
// and 503 under strict even though it was replaced, as a retired session is;
// a browser that cannot be replaced is the failure a health check exists to
// surface.
func TestPingDaemonProbeOutcomes(t *testing.T) {
	launchErr := errors.New("waxseal: relaunch chromium: exec: no such file")
	cases := []struct {
		name       string
		rec        browser.Recovery
		err        error
		ok         bool
		reason     string
		strictCode int
		relaunched bool
		wantErr    string
		warns      bool
	}{
		{"answered", browser.RecoveryNone, nil, true, PingReasonOK, http.StatusOK, false, "", false},
		{"relaunched", browser.RecoveryRelaunched, nil, true, PingReasonOK, http.StatusOK, true, "", false},
		{"torn down", browser.RecoveryTornDown, nil, false, PingReasonProbeFailed, http.StatusServiceUnavailable, true, "the shared browser missed two probes and was torn down and relaunched", true},
		{"unlaunchable", browser.RecoveryNone, launchErr, false, PingReasonProbeFailed, http.StatusServiceUnavailable, false, "no browser answers and none could be launched: relaunch chromium: exec: no such file", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := liveServer(t, keyedOnly, nil)
			proberOf(s, &fakeProber{health: answer(tc.rec, tc.err)})
			logs := &capturingHandler{}
			s.log = slog.New(logs)

			code, resp := pingWith(t, s, "/ping", "")
			if code != http.StatusOK {
				t.Errorf("non-strict status = %d, want 200", code)
			}
			if resp["ok"] != tc.ok || resp["reason"] != tc.reason || resp["probe"] != PingProbeDaemon || resp["browser_relaunched"] != tc.relaunched {
				t.Errorf("body = %v, want ok:%v reason:%s probe:daemon browser_relaunched:%v", resp, tc.ok, tc.reason, tc.relaunched)
			}
			if got, _ := resp["error"].(string); got != tc.wantErr {
				t.Errorf("error = %q, want %q", got, tc.wantErr)
			}
			if code, _ := pingWith(t, s, "/ping?strict=true", ""); code != tc.strictCode {
				t.Errorf("strict status = %d, want %d", code, tc.strictCode)
			}
			if logs.has(slog.LevelWarn, "ping probe failed") != tc.warns {
				t.Errorf("WARN logged = %v, want %v", !tc.warns, tc.warns)
			}
		})
	}
}

// A key that is present but unknown is still 401 on /ping. A typo in a probe's
// --key must surface as a failing probe, not quietly downgrade to the
// daemon-level check and hide the misconfiguration.
func TestPingWrongKeyOnKeyedDaemonStays401(t *testing.T) {
	s := liveServer(t, keyedOnly, nil)
	f := proberOf(s, &fakeProber{})
	code, resp := pingWith(t, s, "/ping", "WRONG")
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
	if resp["code"] != CodeUnauthorized {
		t.Errorf("code = %v, want %q", resp["code"], CodeUnauthorized)
	}
	if f.calls != 0 {
		t.Errorf("browser checks = %d, want 0 for a rejected key", f.calls)
	}
}

// A keyless daemon keeps answering a keyless probe at tenant scope: it has one
// tenant, the empty key selects it, and single-tenant deployments see the same
// body as before. The browser check still follows a tenant probe that found no
// page, as on any daemon; here the browser answers, so the tenant reason stands.
func TestPingKeylessDaemonKeepsTenantProbe(t *testing.T) {
	s := liveServer(t, nil, nil)
	proberOf(s, &fakeProber{})
	code, resp := pingWith(t, s, "/ping", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp["probe"] != PingProbeTenant || resp["tenant"] != "default" || resp["reason"] != PingReasonNoSession {
		t.Errorf("body = %v, want probe:tenant tenant:default reason:no-session", resp)
	}
}

// ?strict is validated before the daemon-level check runs, as it is before the
// tenant-level one, so a typo in the healthcheck is reported whatever the
// browser's state.
func TestPingDaemonProbeRejectsBadStrict(t *testing.T) {
	s := liveServer(t, keyedOnly, nil)
	f := proberOf(s, &fakeProber{})
	code, resp := pingWith(t, s, "/ping?strict=banana", "")
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	if resp["code"] != CodeInvalidRequest {
		t.Errorf("code = %v, want %q", resp["code"], CodeInvalidRequest)
	}
	if f.calls != 0 {
		t.Errorf("browser checks = %d, want 0 for a rejected parameter", f.calls)
	}
}

// A caller that disconnects during the browser check gets nothing written and
// nothing logged as a failure, on the daemon-level path and on a tenant probe
// that escalated.
func TestPingBrowserCheckClientGoneWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys map[string]string
		key  string
	}{
		{"daemon-level", keyedOnly, ""},
		{"tenant escalation", keyedOnly, "K"}, // no session: instant no-session, then the browser check
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := liveServer(t, tc.keys, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			proberOf(s, &fakeProber{health: func(context.Context) (browser.Recovery, error) {
				cancel()
				return browser.RecoveryNone, context.Canceled
			}})
			logs := &capturingHandler{}
			s.log = slog.New(logs)

			r := httptest.NewRequest(http.MethodGet, "/ping?strict=true", nil).WithContext(ctx)
			if tc.key != "" {
				r.Header.Set("X-API-Key", tc.key)
			}
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			if w.Code == http.StatusServiceUnavailable {
				t.Error("status = 503; a client disconnect must not read as a browser failure")
			}
			if body := w.Body.String(); body != "" {
				t.Errorf("body = %q, want empty (nothing written for an abandoned request)", body)
			}
			if logs.has(slog.LevelWarn, "ping probe failed") {
				t.Error("logged a probe-failed WARN for a client disconnect")
			}
		})
	}
}

// A tenant probe that found no answering page is followed by the browser
// check. A browser that answered, or had exited and was relaunched, leaves the
// tenant reason standing (and says when it relaunched); a browser this probe
// found wedged, or one that could not be replaced, is a loss the probe found,
// so the reason becomes probe-failed and the error says what happened to the
// browser after what the tenant probe said.
func TestPingTenantProbeEscalatesToBrowser(t *testing.T) {
	launchErr := errors.New("waxseal: relaunch chromium: exec: no such file")
	sessions := map[string]map[string]*fakePlayerSession{
		"no-session":   nil,
		"probe-failed": {"K": {abrURL: "https://r/x", vd: "vd", pingErr: errors.New("waxseal: cdp connection closed")}},
	}
	tenantErr := map[string]string{"no-session": "no attested session", "probe-failed": "cdp connection closed"}
	tenantCode := map[string]int{"no-session": http.StatusOK, "probe-failed": http.StatusServiceUnavailable}
	outcomes := []struct {
		name       string
		rec        browser.Recovery
		err        error
		loss       bool // the browser check turned the probe into probe-failed
		relaunched bool
		suffix     string
	}{
		{"answered", browser.RecoveryNone, nil, false, false, ""},
		{"relaunched", browser.RecoveryRelaunched, nil, false, true, ""},
		{"torn down", browser.RecoveryTornDown, nil, true, true, "; the shared browser missed two probes and was torn down and relaunched"},
		{"unlaunchable", browser.RecoveryNone, launchErr, true, false, "; no browser answers and none could be launched: relaunch chromium: exec: no such file"},
	}
	for state, sess := range sessions {
		for _, oc := range outcomes {
			t.Run(state+"/"+oc.name, func(t *testing.T) {
				s := liveServer(t, keyedOnly, sess)
				f := proberOf(s, &fakeProber{health: answer(oc.rec, oc.err)})
				logs := &capturingHandler{}
				s.log = slog.New(logs)

				code, resp := pingWith(t, s, "/ping?strict=true", "K")
				wantReason, wantCode := state, tenantCode[state]
				if oc.loss {
					wantReason, wantCode = PingReasonProbeFailed, http.StatusServiceUnavailable
				}
				if code != wantCode || resp["reason"] != wantReason || resp["probe"] != PingProbeTenant {
					t.Errorf("status=%d body=%v, want %d reason:%s probe:tenant", code, resp, wantCode, wantReason)
				}
				if resp["browser_relaunched"] != oc.relaunched {
					t.Errorf("browser_relaunched = %v, want %v", resp["browser_relaunched"], oc.relaunched)
				}
				if got, _ := resp["error"].(string); got != tenantErr[state]+oc.suffix {
					t.Errorf("error = %q, want %q", got, tenantErr[state]+oc.suffix)
				}
				if f.calls != 1 {
					t.Errorf("browser checks = %d, want 1", f.calls)
				}
				if warned := logs.has(slog.LevelWarn, "ping probe failed"); warned != (wantReason == PingReasonProbeFailed) {
					t.Errorf("WARN logged = %v for reason %s", warned, wantReason)
				}
			})
		}
	}
}

// A busy page is escalated too: the request holding it is stuck if the browser
// is wedged, and tearing the browser down is what frees it. When the browser
// answers, the page was merely in use and busy stands, still benign under strict.
func TestPingBusyEscalatesOnlyWhenTheBrowserIsWedged(t *testing.T) {
	for _, wedged := range []bool{false, true} {
		t.Run(fmt.Sprintf("wedged=%v", wedged), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			sess := &fakePlayerSession{
				abrURL:  "https://r/x",
				vd:      "vd",
				pingErr: errors.New("cdp connection closed"),
				pcGate: func(ctx context.Context) {
					once.Do(func() { close(entered) })
					select {
					case <-release:
					case <-ctx.Done():
					}
				},
			}
			s := liveServer(t, keyedOnly, map[string]*fakePlayerSession{"K": sess})
			rec := browser.RecoveryNone
			if wedged {
				rec = browser.RecoveryTornDown
			}
			proberOf(s, &fakeProber{health: answer(rec, nil)})

			done := make(chan struct{})
			go func() {
				defer close(done)
				r := httptest.NewRequest(http.MethodPost, "/player-context", strings.NewReader(`{"video_id":"aqz-KE-bpKQ"}`))
				r.Header.Set("X-API-Key", "K")
				s.routes().ServeHTTP(httptest.NewRecorder(), r)
			}()
			<-entered
			defer func() { close(release); <-done }()

			code, resp := pingWith(t, s, "/ping?strict=true", "K")
			if wedged {
				if code != http.StatusServiceUnavailable || resp["reason"] != PingReasonProbeFailed {
					t.Errorf("wedged browser under a busy page: status=%d body=%v, want 503 probe-failed", code, resp)
				}
			} else if code != http.StatusOK || resp["reason"] != PingReasonBusy {
				t.Errorf("answering browser under a busy page: status=%d body=%v, want 200 busy", code, resp)
			}
		})
	}
}

// A page that answered has proved the browser, so the healthy path does not
// check it again.
func TestPingHealthySessionSkipsBrowserCheck(t *testing.T) {
	s := liveServer(t, keyedOnly, map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}})
	f := proberOf(s, &fakeProber{})
	if code, resp := pingWith(t, s, "/ping?strict=true", "K"); code != http.StatusOK || resp["ok"] != true {
		t.Errorf("status=%d body=%v, want 200 ok:true", code, resp)
	}
	if f.calls != 0 {
		t.Errorf("browser checks = %d, want 0 after a page answered", f.calls)
	}
}

// A probe-failed the browser check produced is visible in /metrics: the pool
// counts the teardown, and both views carry the daemon-wide counters. The fake
// counts the way Pool.Health does, so this pins the response and the counter
// moving together, on the redacted view a keyed daemon serves without a key.
func TestPingBrowserLossMovesMetrics(t *testing.T) {
	s := liveServer(t, keyedOnly, nil)
	f := &fakeProber{}
	f.health = func(context.Context) (browser.Recovery, error) {
		f.probes++
		return browser.RecoveryTornDown, nil
	}
	proberOf(s, f)
	if _, resp := pingWith(t, s, "/ping", ""); resp["reason"] != PingReasonProbeFailed {
		t.Fatalf("reason = %v, want probe-failed", resp["reason"])
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var m struct {
		Redacted         bool  `json:"redacted"`
		ProbeFailures    int64 `json:"browser_probe_failures"`
		RelaunchFailures int64 `json:"browser_relaunch_failures"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode /metrics: %v", err)
	}
	if !m.Redacted || m.ProbeFailures != 1 || m.RelaunchFailures != 0 {
		t.Errorf("/metrics = %+v, want redacted with browser_probe_failures 1 and browser_relaunch_failures 0", m)
	}
}

// The warn for a browser loss names the browser once, and on a tenant probe the
// tenant whose probe found it, so a shared-browser death is neither read as one
// tenant's own nor logged with a doubled attribute.
func TestPingBrowserLossLogAttributes(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want [][2]string
	}{
		{"daemon-level", "", [][2]string{{"probe", "daemon"}}},
		{"tenant escalation", "K", [][2]string{{"tenant", "alice"}, {"probe", "daemon"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := liveServer(t, keyedOnly, nil)
			proberOf(s, &fakeProber{health: answer(browser.RecoveryTornDown, nil)})
			logs := &capturingHandler{}
			s.log = slog.New(logs)
			pingWith(t, s, "/ping", tc.key)
			got, ok := logs.attrs(slog.LevelWarn, "ping probe failed")
			if !ok {
				t.Fatal("no WARN for the browser loss")
			}
			// Everything but the trailing err attribute must match exactly.
			if len(got) == 0 || got[len(got)-1][0] != "err" {
				t.Fatalf("attrs = %v, want them to end with err", got)
			}
			got = got[:len(got)-1]
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("attrs = %v, want %v", got, tc.want)
			}
		})
	}
}
