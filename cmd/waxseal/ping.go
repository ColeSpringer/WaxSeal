package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxseal/server"
)

// hasScheme reports whether s carries a URL scheme. The --addr guard only needs
// to catch a doubled scheme (http://<addr>); browser.LooksLikeWatchURL would
// also reject a legitimate youtube.com host such as "youtube.com:4416".
func hasScheme(s string) bool { return strings.Contains(s, "://") }

// pingOpts holds ping-subcommand flags.
type pingOpts struct {
	addr    string
	key     string
	strict  bool
	timeout time.Duration
}

// pingTimeout is the whole probe's budget: the daemon's worst case plus the
// probe's own connect, transfer, and decode. That worst case is 102 s: four
// session round trips (pingProbeTimeout) and a teardownTimeout, two browser
// round trips (aliveProbeTimeout), gracefulCloseTimeout plus waitDelay, and a
// launchTimeout relaunch. Windows adds profileRemoveTimeout, for 105 s.
const pingTimeout = 108 * time.Second

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
	f.DurationVar(&p.timeout, "timeout", pingTimeout,
		"budget for the whole probe, as a Go duration. The default covers the\n"+
			"daemon's worst case: session and browser probes, a teardown, and one\n"+
			"relaunch of the browser.")
	f.BoolVar(&p.strict, "strict", false,
		"treat the benign no-session and busy windows as healthy and fail only\n"+
			"on probe failure (sends ?strict=true). Use this for container or\n"+
			"systemd liveness checks while sessions are re-established lazily.")
	return c
}

func runPing(cmd *cobra.Command, p *pingOpts) error {
	// An empty --key is a usage error, not "no key": with `--key ${VAR}` and
	// VAR unset, a keyed daemon would get no header and answer the daemon-level
	// probe, which passes while the tenant goes unchecked.
	if cmd.Flags().Changed("key") && strings.TrimSpace(p.key) == "" {
		return &usageError{msg: "--key is empty or only whitespace: pass the tenant key, or omit --key to probe the daemon's browser"}
	}
	if p.timeout <= 0 {
		return &usageError{msg: fmt.Sprintf("invalid --timeout %v: must be positive", p.timeout)}
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
	// A bad port is a usage error (exit 2), as with `waxseal server --port`,
	// not a dial failure (exit 1). Dialing port 0 is meaningless, so the range
	// is 1-65535, not bindListener's 0-65535. ParseUint with bitSize 16 caps
	// the range and rejects the leading '+' that Atoi accepts.
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n < 1 {
		return &usageError{msg: fmt.Sprintf("invalid --addr %q: port must be 1-65535", p.addr)}
	}
	// An empty RawQuery adds no "?". NewRequestWithContext rejects any
	// malformed authority the checks above let through.
	u := (&url.URL{Scheme: "http", Host: p.addr, Path: "/ping", RawQuery: q.Encode()}).String()
	ctx, cancel := context.WithTimeout(cmd.Context(), p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		// Keep malformed authorities (spaces, bad escapes, broken brackets) on the
		// usage-error path.
		return &usageError{msg: fmt.Sprintf("invalid --addr %q: %v", p.addr, err)}
	}
	// The key goes in a header, never the query: proxies and container
	// runtimes log request lines, and a health check runs every few seconds.
	// ?strict is not a secret, so it stays in the query.
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
		Keyed      *bool  `json:"keyed"`              // nil from a daemon older than the field
	}
	// An unreadable body is not health, whatever the status line says. Naming
	// it apart from ok=false tells an operator they are probing something that
	// is not this daemon.
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if err := dec.Decode(&body); err != nil {
		return fmt.Errorf("unhealthy: status=%d unreadable body: %v", resp.StatusCode, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unhealthy: status=%d unreadable body: trailing data after the health object", resp.StatusCode)
	}
	// A keyless daemon ignores the key, so the tenant probe the operator
	// configured silently becomes an unkeyed one.
	if p.key != "" && body.Keyed != nil && !*body.Keyed {
		return errors.New("unhealthy: the daemon is keyless and ignored --key; drop --key or key the daemon")
	}
	// By default only ok:true is healthy, so the benign no-session window reads
	// as not ready; strict also accepts the benign reasons. Even in strict mode
	// a 200 is not enough: older daemons ignore ?strict and can answer 200 with
	// {"ok":false}, and so can non-WaxSeal endpoints.
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
	// Name a relaunch here; otherwise it shows only in the daemon's own log.
	detail := ""
	if body.Relaunched {
		detail = ", browser relaunched"
	}
	switch {
	case !body.OK: // strict mode, a benign window: healthy but nothing live to describe
		fmt.Fprintf(cmd.OutOrStdout(), "ok (reason=%s%s)\n", body.Reason, detail)
	case body.Attest != "":
		fmt.Fprintf(cmd.OutOrStdout(), "ok (attest=%s%s)\n", body.Attest, detail)
	case body.Probe != "": // no attest to name, so say what was checked instead
		fmt.Fprintf(cmd.OutOrStdout(), "ok (probe=%s%s)\n", body.Probe, detail)
	default:
		// Every daemon that reports a relaunch also reports what it probed, so
		// there is nothing left to put in parentheses here.
		fmt.Fprintln(cmd.OutOrStdout(), "ok")
	}
	return nil
}
