package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Bad --addr values should be usage errors (exit 2), not nil-request panics. URL
// input is rejected before request construction. The rest fail URL parsing.
func TestPingCLIInvalidAddr(t *testing.T) {
	for _, addr := range []string{
		"http://127.0.0.1:4416", // URL instead of host:port
		"127.0.0.1",             // bare host, no port (would otherwise dial :80)
		"host with space:80",    // space in authority
		"[::1",                  // unbalanced bracket
		"%zz",                   // bad percent-escape
	} {
		t.Run(addr, func(t *testing.T) {
			c := newPingCmd()
			c.SetArgs([]string{"--addr", addr})
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			err := c.Execute()
			if err == nil {
				t.Fatalf("addr %q: want an error, got nil", addr)
			}
			if _, ok := errors.AsType[*usageError](err); !ok {
				t.Fatalf("addr %q: error %v is not a *usageError", addr, err)
			}
			if got := exitCodeFor(err); got != 2 {
				t.Errorf("addr %q: exit code = %d, want 2", addr, got)
			}
		})
	}
}

// An out-of-range, zero, empty, non-numeric, or signed port gets the clear
// "port must be 1-65535" usage message (exit 2), not a generic parse error at
// dial time. The signed case (:+80) guards the ParseUint-over-Atoi choice.
func TestPingCLIPortRangeMessage(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:99999", // out of range
		"127.0.0.1:0",     // port 0 is meaningless for a client dial
		"127.0.0.1:",      // empty port
		"127.0.0.1:http",  // named (non-numeric) port
		"127.0.0.1:+80",   // leading sign Atoi would accept but a port cannot carry
	} {
		t.Run(addr, func(t *testing.T) {
			c := newPingCmd()
			c.SetArgs([]string{"--addr", addr})
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			err := c.Execute()
			if err == nil || !strings.Contains(err.Error(), "port must be 1-65535") {
				t.Fatalf("addr %q: error = %v, want it to contain %q", addr, err, "port must be 1-65535")
			}
			if got := exitCodeFor(err); got != 2 {
				t.Errorf("addr %q: exit code = %d, want 2", addr, got)
			}
		})
	}
}

// TestPingCLIStrict verifies the exit semantics of `waxseal ping` with and
// without --strict against canned /ping responses. Without --strict a live
// session (ok:true) is required; with --strict the CLI defers to the server's
// status code, so the benign no-session window (HTTP 200) is healthy and only a
// real probe failure (non-200) fails.
func TestPingCLIStrict(t *testing.T) {
	var status int
	var payload string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, payload)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	run := func(strict bool) error {
		c := newPingCmd()
		args := []string{"--addr", addr}
		if strict {
			args = append(args, "--strict")
		}
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		return c.Execute()
	}

	// Healthy: both modes succeed.
	status, payload = http.StatusOK, `{"ok":true,"attest":"integrity","reason":"ok"}`
	if err := run(false); err != nil {
		t.Errorf("healthy non-strict: %v, want success", err)
	}
	if err := run(true); err != nil {
		t.Errorf("healthy strict: %v, want success", err)
	}
	// --strict travels in the query, where the server reads it. It is not a secret,
	// unlike the key, which TestPingSendsKeyAsHeader pins to the header.
	if got := gotQuery.Get("strict"); got != "true" {
		t.Errorf("strict query = %q, want %q", got, "true")
	}
	if err := run(false); err != nil {
		t.Errorf("healthy non-strict (second call): %v, want success", err)
	}
	if gotQuery.Has("strict") {
		t.Errorf("non-strict sent ?strict=%q, want it absent", gotQuery.Get("strict"))
	}

	// Benign no-session (HTTP 200, ok:false): non-strict reports not-ready, strict
	// treats it as healthy so a liveness probe does not flap.
	status, payload = http.StatusOK, `{"ok":false,"reason":"no-session"}`
	if err := run(false); err == nil {
		t.Error("no-session non-strict: want error (no live session)")
	}
	if err := run(true); err != nil {
		t.Errorf("no-session strict: %v, want success (benign window)", err)
	}

	// Benign busy (HTTP 200, ok:false): a probe failed twice but a request held the
	// page, so nothing was retired. --strict must treat it as healthy, or the
	// image's HEALTHCHECK marks a busy but healthy container unhealthy after three
	// probes. Non-strict still reports not-ready: there is no confirmed live
	// session.
	status, payload = http.StatusOK, `{"ok":false,"reason":"busy"}`
	if err := run(false); err == nil {
		t.Error("busy non-strict: want error (no confirmed live session)")
	}
	if err := run(true); err != nil {
		t.Errorf("busy strict: %v, want success (benign window)", err)
	}

	// Real probe failure: a strict-aware daemon maps it to 503; both modes fail.
	status, payload = http.StatusServiceUnavailable, `{"ok":false,"reason":"probe-failed","error":"cdp closed"}`
	if err := run(true); err == nil {
		t.Error("probe-failed strict (503): want error")
	}
	status, payload = http.StatusOK, `{"ok":false,"reason":"probe-failed","error":"cdp closed"}`
	if err := run(false); err == nil {
		t.Error("probe-failed non-strict: want error")
	}

	// A pre-strict daemon ignores ?strict and returns 200 {"ok":false} for a probe
	// failure. --strict must still flag it rather than trusting the 200 alone.
	status, payload = http.StatusOK, `{"ok":false,"reason":"probe-failed","error":"cdp closed"}`
	if err := run(true); err == nil {
		t.Error("probe-failed strict (200 body): want error (must not mask an unhealthy target)")
	}

	// A non-WaxSeal service returning a bare 200 at /ping has no ok field; strict
	// mode must not report it healthy.
	status, payload = http.StatusOK, `{}`
	if err := run(true); err == nil {
		t.Error("empty 200 body strict: want error")
	}
}

// TestPingSendsKeyAsHeader pins that the API key never reaches the query string,
// where it would land in proxy and container access logs. The healthcheck runs
// every few seconds, so a key in the request line is written to those logs for
// the life of the container.
func TestPingSendsKeyAsHeader(t *testing.T) {
	const key = "s3cr3t-tenant-key"
	var gotHeader, gotQueryKey, gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-API-Key")
		gotQueryKey = r.URL.Query().Get("key")
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"attest":"integrity","reason":"ok"}`)
	}))
	defer srv.Close()

	c := newPingCmd()
	c.SetArgs([]string{"--addr", strings.TrimPrefix(srv.URL, "http://"), "--key", key})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); err != nil {
		t.Fatalf("ping --key: %v, want success", err)
	}
	if gotHeader != key {
		t.Errorf("X-API-Key = %q, want %q", gotHeader, key)
	}
	if gotQueryKey != "" {
		t.Errorf("?key = %q, want it absent", gotQueryKey)
	}
	// Also check the raw query, so a differently named parameter carrying the key
	// cannot pass. This is the value that appears in an access log request line.
	if strings.Contains(gotRawQuery, key) {
		t.Errorf("raw query %q contains the key", gotRawQuery)
	}
}

// TestPingCLIDaemonProbe covers the body a keyed daemon returns to a probe that
// sends no key: the shared browser's liveness rather than a tenant's session.
// That is what the image's HEALTHCHECK receives once the daemon is keyed, so
// both modes must read it, and the output must say what was checked instead of
// printing an empty attest.
func TestPingCLIDaemonProbe(t *testing.T) {
	var status int
	var payload string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, payload)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	run := func(strict bool) (string, error) {
		c := newPingCmd()
		args := []string{"--addr", addr}
		if strict {
			args = append(args, "--strict")
		}
		c.SetArgs(args)
		var out strings.Builder
		c.SetOut(&out)
		c.SetErr(io.Discard)
		err := c.Execute()
		return out.String(), err
	}

	// Browser answered: healthy in both modes, and the output names the probe.
	status, payload = http.StatusOK, `{"ok":true,"probe":"daemon","reason":"ok"}`
	for _, strict := range []bool{false, true} {
		out, err := run(strict)
		if err != nil {
			t.Errorf("alive strict=%v: %v, want success", strict, err)
		}
		if out != "ok (probe=daemon)\n" {
			t.Errorf("alive strict=%v: output = %q, want %q", strict, out, "ok (probe=daemon)\n")
		}
	}

	// Browser had exited and the probe relaunched it: healthy, and the output
	// says so, since that is the one place a relaunch shows outside the daemon log.
	status, payload = http.StatusOK, `{"ok":true,"probe":"daemon","reason":"ok","browser_relaunched":true}`
	for _, strict := range []bool{false, true} {
		out, err := run(strict)
		if err != nil {
			t.Errorf("relaunched strict=%v: %v, want success", strict, err)
		}
		if out != "ok (probe=daemon, browser relaunched)\n" {
			t.Errorf("relaunched strict=%v: output = %q, want %q", strict, out, "ok (probe=daemon, browser relaunched)\n")
		}
	}
	// The same note on a tenant probe's benign window.
	status, payload = http.StatusOK, `{"ok":false,"probe":"tenant","reason":"no-session","browser_relaunched":true}`
	if out, err := run(true); err != nil || out != "ok (reason=no-session, browser relaunched)\n" {
		t.Errorf("no-session with relaunch, strict: out=%q err=%v, want %q", out, err, "ok (reason=no-session, browser relaunched)\n")
	}

	// Browser confirmed unresponsive: a failure in both modes.
	status, payload = http.StatusServiceUnavailable, `{"ok":false,"probe":"daemon","reason":"probe-failed","error":"cdp: Browser.getVersion: context deadline exceeded"}`
	if _, err := run(true); err == nil {
		t.Error("probe-failed strict (503): want error")
	}
	status, payload = http.StatusOK, `{"ok":false,"probe":"daemon","reason":"probe-failed","error":"cdp: Browser.getVersion: context deadline exceeded"}`
	if _, err := run(false); err == nil {
		t.Error("probe-failed non-strict: want error")
	}
}

// TestPingCLIEmptyKeyIsUsageError pins that `--key ""` is refused. A compose
// file that passes `--key ${SOME_VAR}` with the variable unset would otherwise
// send no header, and on a keyed daemon that quietly turns the tenant probe the
// operator configured into the daemon-level one, which stays healthy while the
// tenant it meant to watch is never checked.
func TestPingCLIEmptyKeyIsUsageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the daemon was probed despite the empty --key")
	}))
	defer srv.Close()
	c := newPingCmd()
	c.SetArgs([]string{"--addr", strings.TrimPrefix(srv.URL, "http://"), "--key", ""})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	err := c.Execute()
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("ping --key \"\" = %v, want a usage error", err)
	}
	if !strings.Contains(err.Error(), "--key") {
		t.Errorf("usage error %q does not name --key", err)
	}
}
