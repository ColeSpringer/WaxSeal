package botguard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/colespringer/waxseal/internal/httpx"
)

// Wire constants. WAA requires a WebKit user agent. Other user agents produce
// invalid tokens (rustypipe lib.rs:123).
const (
	RequestKey       = "O43z0dpjhgX20SCx4KAo"
	GoogAPIKey       = "AIzaSyDyT5W0Jh49F30Pqqtyfdf7pDLFKLJoAnw"
	contentTypeProto = "application/json+protobuf"
	xUserAgent       = "grpc-web-javascript/0.1"
	DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.10 Safari/605.1.1"

	// CreateURL/GenerateITURL are the default endpoint mode (youtube.com/api/jnn/v1).
	CreateURL     = "https://www.youtube.com/api/jnn/v1/Create"
	GenerateITURL = "https://www.youtube.com/api/jnn/v1/GenerateIT"

	maxChallengeBody        = 4 << 20  // bounded response bodies
	maxInterpreterBody      = 24 << 20 // the obfuscated interpreter can be large
	maxInterpreterRedirects = 3
)

// EndpointMode selects the WAA host for Create and GenerateIT calls. The default
// youtube.com/api/jnn/v1 path needs no authentication. The googleapis mode uses
// jnn-pa.googleapis.com for bgutil compatibility. InnerTube att/get always uses
// youtube.com.
type EndpointMode string

const (
	EndpointYouTube    EndpointMode = "youtube"    // youtube.com/api/jnn/v1 (default)
	EndpointGoogleAPIs EndpointMode = "googleapis" // jnn-pa.googleapis.com
)

// Endpoint carries the resolved Create/GenerateIT URLs for a mode.
type Endpoint struct {
	CreateURL     string
	GenerateITURL string
}

// DefaultEndpoint is the youtube.com/api/jnn/v1 mode.
var DefaultEndpoint = Endpoint{CreateURL: CreateURL, GenerateITURL: GenerateITURL}

// ResolveEndpoint maps a mode string to its Endpoint. An empty mode selects the
// default, and an unknown mode returns an error.
func ResolveEndpoint(mode string) (Endpoint, error) {
	switch EndpointMode(NormalizeEndpointMode(mode)) {
	case EndpointYouTube:
		return DefaultEndpoint, nil
	case EndpointGoogleAPIs:
		return Endpoint{
			CreateURL:     "https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/Create",
			GenerateITURL: "https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/GenerateIT",
		}, nil
	default:
		return Endpoint{}, fmt.Errorf("botguard: unknown endpoint mode %q (want %q or %q)", mode, EndpointYouTube, EndpointGoogleAPIs)
	}
}

// NormalizeEndpointMode lowercases/trims a mode and maps empty to the default. It
// is the canonical string mixed into cache/minter keys.
func NormalizeEndpointMode(mode string) string {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return string(EndpointYouTube)
	}
	return m
}

// orDefault substitutes DefaultEndpoint for a zero Endpoint value (defensive for
// callers/tests that leave it unset).
func (e Endpoint) orDefault() Endpoint {
	if e.CreateURL == "" || e.GenerateITURL == "" {
		return DefaultEndpoint
	}
	return e
}

// Stage identifies where a BotGuard operation failed. Telemetry and circuit
// breakers can use it without parsing error messages.
type Stage string

const (
	StageTransport  Stage = "transport"
	StageDescramble Stage = "descramble"
	StageParse      Stage = "parse"
	StageInterp     Stage = "interpreter-fetch"
	StageVM         Stage = "vm"
	StageGenerateIT Stage = "generateit"
	StageMint       Stage = "mint"
	StageValidate   Stage = "validate"
)

// StageError carries the stage without embedding raw Google payloads or tokens.
type StageError struct {
	Stage Stage
	Err   error
}

func (e *StageError) Error() string { return string(e.Stage) + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

func stageErr(s Stage, format string, a ...any) error {
	return &StageError{Stage: s, Err: fmt.Errorf(format, a...)}
}

// Challenge is the parsed (and interpreter-resolved) BotGuard challenge.
type Challenge struct {
	InterpreterJS   string // resolved inline interpreter JS (the only JS we run)
	Program         string // arr[4]
	GlobalName      string // arr[5]
	InterpreterURL  string // set when sourced from a URL (for fetching/telemetry)
	InterpreterHash string // att/get's interpreterHash, when supplied (cache key)
}

// FetchCreateChallenge posts to WAA Create, parses the response, and resolves the
// interpreter. HTTP requests use the shared httpx layer. userAgent must belong to
// the active browser profile and use the WebKit family.
func FetchCreateChallenge(ctx context.Context, client *httpx.Client, userAgent string, ep Endpoint) (*Challenge, error) {
	ep = ep.orDefault()
	body, _ := json.Marshal([]string{RequestKey})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.CreateURL, bytes.NewReader(body))
	if err != nil {
		return nil, stageErr(StageTransport, "build Create request: %w", err)
	}
	setProtoHeaders(req, userAgent)

	raw, err := client.DoJSON(req, maxChallengeBody)
	if err != nil {
		return nil, stageErr(StageTransport, "Create: %w", err)
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, stageErr(StageParse, "Create response not an array: %w", err)
	}

	ch, err := parseCreateArray(arr)
	if err != nil {
		return nil, err
	}
	if err := ResolveInterpreter(ctx, client, ch, userAgent); err != nil {
		return nil, err
	}
	return ch, nil
}

// parseCreateArray handles both Create response families: a scrambled base64
// string at arr[1], or a structured challenge array at arr[0].
func parseCreateArray(arr []json.RawMessage) (*Challenge, error) {
	if len(arr) >= 2 {
		var scrambled string
		if json.Unmarshal(arr[1], &scrambled) == nil && scrambled != "" {
			descrambled, err := descramble(scrambled)
			if err != nil {
				return nil, stageErr(StageDescramble, "%w", err)
			}
			var cdata []json.RawMessage
			if err := json.Unmarshal(descrambled, &cdata); err != nil {
				return nil, stageErr(StageParse, "descrambled challenge not an array: %w", err)
			}
			return parseChallengeData(cdata)
		}
	}
	if len(arr) >= 1 {
		var cdata []json.RawMessage
		if json.Unmarshal(arr[0], &cdata) == nil && len(cdata) > 0 {
			return parseChallengeData(cdata)
		}
	}
	return nil, stageErr(StageParse, "unrecognized Create response shape")
}

// descramble ports rustypipe's descramble: standard base64 decode, then +97 per
// byte (wrapping) yields the JSON challenge array.
func descramble(scrambled string) ([]byte, error) {
	bts, err := base64.StdEncoding.DecodeString(scrambled)
	if err != nil {
		// Tolerate raw (unpadded) base64 too.
		bts, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(scrambled, "="))
		if err != nil {
			return nil, fmt.Errorf("base64: %w", err)
		}
	}
	out := make([]byte, len(bts))
	for i, b := range bts {
		out[i] = b + 97 // wrapping add (byte arithmetic wraps mod 256)
	}
	return out, nil
}

// ParseProvidedChallenge parses a caller-supplied challenge from /get_pot or a
// page into an unresolved Challenge. Accepted shapes are bgutil's structured
// object, a challenge-data array, and the legacy scrambled string. Interpreter
// URLs are resolved by the caller with ResolveInterpreter.
func ParseProvidedChallenge(raw json.RawMessage) (*Challenge, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, stageErr(StageParse, "empty challenge")
	}
	switch trimmed[0] {
	case '{':
		return parseObjectChallenge(trimmed)
	case '[':
		var cdata []json.RawMessage
		if err := json.Unmarshal(trimmed, &cdata); err != nil {
			return nil, stageErr(StageParse, "challenge array: %w", err)
		}
		return parseChallengeData(cdata)
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, stageErr(StageParse, "challenge string: %w", err)
		}
		return parseStringChallenge(s)
	}
	return nil, stageErr(StageParse, "unrecognized challenge shape")
}

// parseObjectChallenge reads bgutil's structured-object shape.
func parseObjectChallenge(raw []byte) (*Challenge, error) {
	var obj struct {
		InterpreterURL struct {
			Priv string `json:"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue"`
		} `json:"interpreterUrl"`
		Program    string `json:"program"`
		GlobalName string `json:"globalName"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, stageErr(StageParse, "challenge object: %w", err)
	}
	if obj.Program == "" || obj.GlobalName == "" {
		return nil, stageErr(StageParse, "challenge object missing program/globalName")
	}
	if obj.InterpreterURL.Priv == "" {
		return nil, stageErr(StageParse, "challenge object missing interpreterUrl")
	}
	return &Challenge{InterpreterURL: obj.InterpreterURL.Priv, Program: obj.Program, GlobalName: obj.GlobalName}, nil
}

// parseStringChallenge handles the legacy string shape: scrambled base64 or, for
// compatibility, a plain JSON challenge array encoded as a string.
func parseStringChallenge(s string) (*Challenge, error) {
	if descrambled, err := descramble(s); err == nil {
		var cdata []json.RawMessage
		if json.Unmarshal(descrambled, &cdata) == nil && len(cdata) >= 6 {
			return parseChallengeData(cdata)
		}
	}
	var cdata []json.RawMessage
	if json.Unmarshal([]byte(s), &cdata) == nil && len(cdata) >= 6 {
		return parseChallengeData(cdata)
	}
	return nil, stageErr(StageParse, "unrecognized string challenge")
}

// parseChallengeData ports parse_challenge_data. The interpreter is the first
// non-empty string in cdata[1] for inline JavaScript or cdata[2] for a URL. The
// program and global name are in cdata[4] and cdata[5].
func parseChallengeData(cdata []json.RawMessage) (*Challenge, error) {
	if len(cdata) < 6 {
		return nil, stageErr(StageParse, "challenge array len %d < 6", len(cdata))
	}
	ch := &Challenge{}
	if js := firstNonEmptyString(cdata[1]); js != "" {
		ch.InterpreterJS = js
	} else if u := firstNonEmptyString(cdata[2]); u != "" {
		ch.InterpreterURL = u
	} else {
		return nil, stageErr(StageParse, "no interpreter JS or URL")
	}
	if err := json.Unmarshal(cdata[4], &ch.Program); err != nil || ch.Program == "" {
		return nil, stageErr(StageParse, "program (arr[4]) missing")
	}
	if err := json.Unmarshal(cdata[5], &ch.GlobalName); err != nil || ch.GlobalName == "" {
		return nil, stageErr(StageParse, "globalName (arr[5]) missing")
	}
	return ch, nil
}

// firstNonEmptyString returns the first non-empty string in a JSON array value
// (or the string itself), else "".
func firstNonEmptyString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, item := range arr {
			if json.Unmarshal(item, &s) == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

// ResolveInterpreter fetches a URL-sourced interpreter after validating the host
// against google.com/youtube.com. Inline interpreters pass through. The fetch
// uses a redirect-guarded clone of the shared transport and enforces
// maxInterpreterBody. InnerTube att/get uses this path because it returns a
// bgChallenge with only an interpreterUrl.
func ResolveInterpreter(ctx context.Context, client *httpx.Client, ch *Challenge, userAgent string) error {
	if ch.InterpreterJS != "" {
		return nil
	}
	// Reuse an interpreter fetched for the same hash or URL. Interpreter code is
	// independent of the browser profile and egress IP.
	cacheKey := interpKey(ch)
	if cacheKey != "" {
		if js, ok := interpCache.get(cacheKey); ok {
			ch.InterpreterJS = js
			return nil
		}
	}
	rawURL := ch.InterpreterURL
	if strings.HasPrefix(rawURL, "//") {
		rawURL = "https:" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return stageErr(StageInterp, "parse interpreter URL: %w", err)
	}
	if u.Scheme != "https" || !hostAllowed(u.Hostname()) {
		return stageErr(StageInterp, "interpreter host not allowlisted: %q", u.Hostname())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return stageErr(StageInterp, "build request: %w", err)
	}
	req.Header.Set("User-Agent", uaOrDefault(userAgent))

	// Re-validate the host on every redirect hop and cap the count.
	base := http.DefaultClient
	if client != nil && client.HTTP != nil {
		base = client.HTTP
	}
	guarded := *base
	guarded.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= maxInterpreterRedirects {
			return fmt.Errorf("too many redirects")
		}
		if r.URL.Scheme != "https" || !hostAllowed(r.URL.Hostname()) {
			return fmt.Errorf("redirect to non-allowlisted host %q", r.URL.Hostname())
		}
		return nil
	}

	resp, err := guarded.Do(req)
	if err != nil {
		return stageErr(StageInterp, "fetch interpreter: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return stageErr(StageInterp, "interpreter status %d", resp.StatusCode)
	}
	data, err := httpx.ReadBodyCapped(resp.Body, maxInterpreterBody)
	if err != nil {
		return stageErr(StageInterp, "read interpreter: %w", err)
	}
	ch.InterpreterJS = string(data)
	if cacheKey != "" {
		interpCache.put(cacheKey, ch.InterpreterJS)
	}
	return nil
}

// interpKey is the interpreter cache key: the att/get-supplied hash when present,
// else the source URL (stable per interpreter version). Empty means uncacheable.
func interpKey(ch *Challenge) string {
	if ch.InterpreterHash != "" {
		return "h:" + ch.InterpreterHash
	}
	if ch.InterpreterURL != "" {
		return "u:" + ch.InterpreterURL
	}
	return ""
}

// interpreterCache stores fetched interpreter JavaScript in memory by interpKey.
// It is process-wide and bounded.
type interpreterCache struct {
	mu  sync.Mutex
	max int
	m   map[string]string
}

var interpCache = &interpreterCache{max: 4, m: make(map[string]string)}

func (c *interpreterCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	js, ok := c.m[key]
	return js, ok
}

func (c *interpreterCache) put(key, js string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[key]; !exists && len(c.m) >= c.max {
		for k := range c.m { // versions change rarely; evicting any is fine
			delete(c.m, k)
			break
		}
	}
	c.m[key] = js
}

// ClearInterpreterCache drops all cached interpreters. Forced refreshes and
// tests use it to clear process-wide state.
func ClearInterpreterCache() {
	interpCache.mu.Lock()
	defer interpCache.mu.Unlock()
	clear(interpCache.m)
}

// hostAllowed permits google.com/youtube.com and their subdomains only, by exact
// or dotted-suffix match. A substring such as "evilgoogle.com" is rejected.
func hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, base := range []string{"google.com", "youtube.com"} {
		if host == base || strings.HasSuffix(host, "."+base) {
			return true
		}
	}
	return false
}

// setProtoHeaders sets the JSON+protobuf attestation headers. userAgent is the
// active profile's attestation UA; an empty value falls back to the WebKit
// DefaultUserAgent (a non-WebKit UA yields invalid tokens).
func setProtoHeaders(req *http.Request, userAgent string) {
	req.Header.Set("Content-Type", contentTypeProto)
	req.Header.Set("x-goog-api-key", GoogAPIKey)
	req.Header.Set("x-user-agent", xUserAgent)
	req.Header.Set("User-Agent", uaOrDefault(userAgent))
}

func uaOrDefault(ua string) string {
	if ua == "" {
		return DefaultUserAgent
	}
	return ua
}
