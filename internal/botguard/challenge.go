package botguard

import (
	"context"
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

	// GenerateITURL uses the youtube.com/api/jnn/v1 path, which needs no
	// authentication.
	GenerateITURL = "https://www.youtube.com/api/jnn/v1/GenerateIT"

	maxChallengeBody        = 4 << 20  // bounded response bodies
	maxInterpreterBody      = 24 << 20 // the obfuscated interpreter can be large
	maxInterpreterRedirects = 3
)

// Endpoint carries the WAA GenerateIT URL.
type Endpoint struct {
	GenerateITURL string
}

// DefaultEndpoint is the youtube.com/api/jnn/v1 endpoint.
var DefaultEndpoint = Endpoint{GenerateITURL: GenerateITURL}

// orDefault returns DefaultEndpoint when the URL is empty, so callers and tests
// can leave Endpoint unset.
func (e Endpoint) orDefault() Endpoint {
	if e.GenerateITURL == "" {
		return DefaultEndpoint
	}
	return e
}

// Stage identifies where a BotGuard operation failed, so callers can branch on
// it without parsing error messages.
type Stage string

const (
	StageTransport  Stage = "transport"
	StageParse      Stage = "parse"
	StageInterp     Stage = "interpreter-fetch"
	StageGenerateIT Stage = "generateit"
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

// Challenge is a BotGuard challenge parsed from att/get's bgChallenge.
type Challenge struct {
	InterpreterJS   string // fetched by ResolveInterpreter
	Program         string
	GlobalName      string
	InterpreterURL  string
	InterpreterHash string // att/get's interpreterHash, when supplied (cache key)
}

// ResolveInterpreter fetches a URL-sourced interpreter after validating the host
// against google.com/youtube.com. A Challenge that already has InterpreterJS
// passes through. The fetch uses a redirect-guarded copy of the shared client
// and enforces maxInterpreterBody. InnerTube att/get needs this path because
// its bgChallenge carries only an interpreterUrl.
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

// DomainMatches reports whether host is base or a dotted subdomain of base. It is
// shared by challenge URL validation and browser cookie filtering so both checks
// reject look-alike domains the same way. Callers normalize case and any leading
// or trailing dot before calling.
func DomainMatches(host, base string) bool {
	return host == base || strings.HasSuffix(host, "."+base)
}

// hostAllowed permits google.com/youtube.com and their subdomains only, by exact
// or dotted-suffix match. A substring such as "evilgoogle.com" is rejected.
func hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return DomainMatches(host, "google.com") || DomainMatches(host, "youtube.com")
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
