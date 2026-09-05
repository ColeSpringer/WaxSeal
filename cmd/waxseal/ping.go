package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxseal/server"
)

// hasScheme reports whether s carries a URL scheme. Unlike
// browser.LooksLikeWatchURL, it does not treat a bare host such as
// "youtube.com:4416" as a URL, so a legitimate --addr with a youtube.com host is
// accepted. The --addr guard only ever needs to catch a doubled scheme
// (http://<addr>).
func hasScheme(s string) bool { return strings.Contains(s, "://") }

// pingOpts holds ping-subcommand flags.
type pingOpts struct {
	addr   string
	key    string
	strict bool
}

// newPingCmd checks a running server with GET /ping and exits nonzero on failure.
// It is a curl-free probe for scripts, systemd, and container health checks.
func newPingCmd() *cobra.Command {
	var p pingOpts
	c := &cobra.Command{
		Use:   "ping",
		Short: "Check the health of a running WaxSeal server",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runPing(cmd, &p) },
	}
	f := c.Flags()
	f.StringVar(&p.addr, "addr", "127.0.0.1:4416", "server address to connect to")
	f.StringVar(&p.key, "key", "",
		"tenant API key: probe that tenant's session. Without it a keyed daemon\n"+
			"answers with the shared browser's liveness instead, which is what a\n"+
			"container health check needs; a keyless daemon probes its one tenant.")
	f.BoolVar(&p.strict, "strict", false,
		"treat the benign no-session and busy windows as healthy and fail only\n"+
			"on probe failure (sends ?strict=true). Use this for container or\n"+
			"systemd liveness checks while sessions are re-established lazily.")
	return c
}

func runPing(cmd *cobra.Command, p *pingOpts) error {
	// An empty --key is a usage error rather than "no key". A compose file that
	// passes `--key ${VAR}` with the variable unset would otherwise send no
	// header, and on a keyed daemon that turns the tenant probe the operator
	// configured into the daemon-level one without a word: healthy, while the
	// tenant it meant to watch is never checked.
	if cmd.Flags().Changed("key") && p.key == "" {
		return &usageError{msg: "--key is empty: pass the tenant key, or omit --key to probe the daemon's browser"}
	}
	q := url.Values{}
	if p.strict {
		q.Set("strict", "true")
	}
	// --addr is host:port. Reject URL input before building http://<addr>/ping;
	// otherwise the doubled scheme parses and fails later as an unreachable host.
	if hasScheme(p.addr) {
		return &usageError{msg: fmt.Sprintf("invalid --addr %q: use host:port, not a URL", p.addr)}
	}
	// Require an explicit host:port. SplitHostPort also rejects bare hosts, extra
	// colons, and unbalanced brackets.
	_, port, err := net.SplitHostPort(p.addr)
	if err != nil {
		return &usageError{msg: fmt.Sprintf("invalid --addr %q: use host:port (%v)", p.addr, err)}
	}
	// Validate the port here so an out-of-range, empty, non-numeric, or signed port
	// is a usage error (exit 2) rather than a plain dial failure (exit 1), like
	// `waxseal server --port`. Unlike a listener bind, a client dial to port 0 is
	// meaningless, so require 1-65535 (not bindListener's 0-65535). ParseUint with
	// bitSize 16 bounds the upper end and rejects the leading '+' that Atoi accepts.
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n < 1 {
		return &usageError{msg: fmt.Sprintf("invalid --addr %q: port must be 1-65535", p.addr)}
	}
	// Build the URL with url.URL instead of string concatenation. p.addr is already
	// validated as host:port above. An empty RawQuery yields no "?", and
	// NewRequestWithContext still rejects anything malformed that slips through.
	u := (&url.URL{Scheme: "http", Host: p.addr, Path: "/ping", RawQuery: q.Encode()}).String()
	ctx, cancel := context.WithTimeout(cmd.Context(), 100*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		// Keep malformed authorities (spaces, bad escapes, broken brackets) on the
		// usage-error path. Passing a nil request to http.DefaultClient.Do would panic.
		return &usageError{msg: fmt.Sprintf("invalid --addr %q: %v", p.addr, err)}
	}
	// The key travels in a header, never in the query string. A health check runs
	// every few seconds, and reverse proxies and container runtimes log request
	// lines, so ?key= would write the tenant key into those logs forever. ?strict
	// stays in the query because it is not a secret.
	if p.key != "" {
		req.Header.Set("X-API-Key", p.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		OK         bool   `json:"ok"`
		Probe      string `json:"probe"` // what the daemon checked; absent from older daemons
		Attest     string `json:"attest"`
		Reason     string `json:"reason"`
		Relaunched bool   `json:"browser_relaunched"` // the probe found the browser gone and relaunched it
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	// Health semantics:
	//   default: require a live session or browser (ok:true), so the benign
	//   no-session window reads as not ready.
	//   strict: accept the benign reasons as healthy, but still fail on probe-failed.
	//
	// Do not trust HTTP 200 alone in strict mode. Older daemons ignore ?strict and
	// can return 200 with {"ok":false}; non-WaxSeal endpoints can do the same.
	healthy := body.OK
	if p.strict {
		// server.BenignPingReason is the daemon's own strict-200 policy, so the
		// probe and the daemon cannot disagree about what counts as unhealthy.
		healthy = body.OK || server.BenignPingReason(body.Reason)
	}
	if resp.StatusCode != http.StatusOK || !healthy {
		// reason distinguishes the benign windows (no-session, busy) from a real
		// probe failure; older servers omit it.
		if body.Reason != "" {
			return fmt.Errorf("unhealthy: status=%d ok=%v reason=%s", resp.StatusCode, body.OK, body.Reason)
		}
		return fmt.Errorf("unhealthy: status=%d ok=%v", resp.StatusCode, body.OK)
	}
	// A relaunch is worth a word in the health log: the probe found the browser
	// gone and replaced it, which otherwise shows only in the daemon's own log.
	detail := ""
	if body.Relaunched {
		detail = ", browser relaunched"
	}
	switch {
	case !body.OK: // strict mode, a benign window: healthy but nothing live to describe
		fmt.Fprintf(cmd.OutOrStdout(), "ok (reason=%s%s)\n", body.Reason, detail)
	case body.Probe == server.PingProbeDaemon: // a keyed daemon probed without a key: no session, so no attest
		fmt.Fprintf(cmd.OutOrStdout(), "ok (probe=%s%s)\n", body.Probe, detail)
	default:
		fmt.Fprintf(cmd.OutOrStdout(), "ok (attest=%s%s)\n", body.Attest, detail)
	}
	return nil
}
