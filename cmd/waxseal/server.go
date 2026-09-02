package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/internal/minter"
	"github.com/colespringer/waxseal/server"
	"github.com/spf13/cobra"
)

// serverOpts holds server-subcommand flags.
type serverOpts struct {
	host            string
	port            int
	video           string
	headful         bool
	tenantKeys      string
	streamingMaxAge string
	reportDebounce  string
	shutdownTimeout string
	metricsPublic   bool
	metricsKey      string
	verbose         bool
}

const (
	// streamingMaxAgeDefault limits how long a session serves streaming requests.
	streamingMaxAgeDefault = 45 * time.Minute
	// streamingMaxAgeFloor prevents excessive re-attestation.
	streamingMaxAgeFloor = time.Minute
	// streamingMaxAgeWarn marks values that provide little automatic recycling.
	streamingMaxAgeWarn = 4 * time.Hour

	// reportDebounceFloor prevents consumer reports from causing excessive
	// re-attestation.
	reportDebounceFloor = 5 * time.Second
	// reportDebounceWarn marks values that make report-driven recycling infrequent.
	reportDebounceWarn = time.Hour

	// defaultShutdownTimeout bounds the drain on SIGTERM or SIGINT. It is sized to
	// the work a real request does, not to requestProcessTimeout: cold
	// /player-context calls measure 6 to 10 seconds and first-session establishment
	// is documented at 10 to 30, so this covers both with roughly twice the headroom.
	// Matching the 3 minute request timeout instead would make `docker compose down`
	// hang that long against a wedged daemon, since stop_grace_period has to match.
	// A request still running after a minute is pathological: it is severed, logged,
	// and the daemon still exits 0. Operators who need longer set --shutdown-timeout.
	defaultShutdownTimeout = 60 * time.Second
)

func newServerCmd() *cobra.Command {
	var o serverOpts
	c := &cobra.Command{
		Use:   "server",
		Short: "Run the bgutil-compatible HTTP daemon",
		Long: "Run the HTTP daemon over a real headless Chromium. It defaults to loopback\n" +
			"at 127.0.0.1:4416. Set --host 0.0.0.0 to expose it. With --tenant-keys,\n" +
			"each key receives an isolated browser context. Without it, the server is\n" +
			"keyless. On SIGTERM or SIGINT it drains in-flight requests for up to\n" +
			"--shutdown-timeout (default 60s) before tearing the browser down.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runServer(cmd, &o) },
	}
	f := c.Flags()
	f.StringVar(&o.host, "host", "127.0.0.1", "bind address (set 0.0.0.0 to expose)")
	f.IntVar(&o.port, "port", 4416, "listen port")
	f.StringVar(&o.video, "video", browser.DefaultVideo, "landing video for each tenant session")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.StringVar(&o.tenantKeys, "tenant-keys", "", `multi-tenant API keys in "label1=key1,label2=key2" form`)
	f.StringVar(&o.streamingMaxAge, "streaming-max-age", "",
		"recycle a session on its next streaming handoff once older than this Go duration\n"+
			"(flag > WAXSEAL_STREAMING_MAX_AGE env > 45m default; \"0\" disables). The\n"+
			"first streaming request after a recycle waits for re-attestation and\n"+
			"establishment. Idle sessions are not recycled. Minimum 1m.")
	f.StringVar(&o.reportDebounce, "report-debounce", "",
		fmt.Sprintf("sustained spacing between consumer-report-driven (POST /report) session\n"+
			"recycles (flag > WAXSEAL_REPORT_DEBOUNCE env > 5m default). Bursts of up\n"+
			"to %d recycles are allowed before rate-limiting; the budget refills at one\n"+
			"recycle per interval. This limits re-attestation caused by reports and\n"+
			"applies separately to each tenant. Minimum 5s; report rate-limiting\n"+
			"cannot be disabled.", minter.ReportBurst))
	f.StringVar(&o.shutdownTimeout, "shutdown-timeout", "",
		fmt.Sprintf("how long shutdown waits for in-flight requests to finish on\n"+
			"SIGTERM or SIGINT (flag > WAXSEAL_SHUTDOWN_TIMEOUT env > %s default).\n"+
			"A request still running when the budget expires is severed and logged;\n"+
			"the daemon still exits 0.", defaultShutdownTimeout))
	f.BoolVar(&o.metricsPublic, "metrics-public", false,
		"serve full per-tenant /metrics detail (tenant labels + activity) to\n"+
			"unauthenticated scrapes on a keyed daemon. Ignored without\n"+
			"--tenant-keys because keyless daemons already serve full detail.")
	f.StringVar(&o.metricsKey, "metrics-key", "",
		"operator key that unlocks full per-tenant /metrics detail on a keyed daemon.\n"+
			"Without it (or --metrics-public), a keyed daemon serves unauthenticated\n"+
			"scrapes a redacted, label-free aggregate. Must differ from every tenant\n"+
			"key. Ignored without --tenant-keys.")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "enable debug logging")
	return c
}

// resolveStreamingMaxAge applies flag, environment, and default precedence.
// Empty and zero values disable time-based recycling.
func resolveStreamingMaxAge(cmd *cobra.Command, o *serverOpts, logger *slog.Logger) (time.Duration, error) {
	raw := streamingMaxAgeDefault.String()
	if v, ok := os.LookupEnv("WAXSEAL_STREAMING_MAX_AGE"); ok {
		raw = v
	}
	if cmd.Flags().Changed("streaming-max-age") {
		raw = o.streamingMaxAge
	}
	if raw = strings.TrimSpace(raw); raw == "" {
		logStreamingMaxAge(logger, 0)
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		// Preserve usageError as the top-level type so this maps to exit code 2.
		return 0, &usageError{msg: fmt.Sprintf("invalid --streaming-max-age %q: %v (use a Go duration like 45m, or 0 to disable)", raw, err)}
	}
	if d < 0 {
		return 0, &usageError{msg: fmt.Sprintf("invalid --streaming-max-age %q: must not be negative (use 0 to disable)", raw)}
	}
	if d > 0 && d < streamingMaxAgeFloor {
		return 0, &usageError{msg: fmt.Sprintf("invalid --streaming-max-age %q: must be at least %s to prevent excessive re-attestation", raw, streamingMaxAgeFloor)}
	}
	logStreamingMaxAge(logger, d)
	return d, nil
}

// logStreamingMaxAge reports configurations that provide little or no automatic
// recycling.
func logStreamingMaxAge(logger *slog.Logger, d time.Duration) {
	switch {
	case d == 0:
		logger.Warn("streaming-max-age disabled; sessions recycle only after POST /report")
	case d > streamingMaxAgeWarn:
		logger.Warn("streaming-max-age is large; consider a shorter interval", "value", d)
	default:
		logger.Info("streaming-max-age set", "value", d)
	}
}

// resolveReportDebounce applies flag, environment, and default precedence. Empty
// values use the default. Report rate-limiting cannot be disabled.
func resolveReportDebounce(cmd *cobra.Command, o *serverOpts, logger *slog.Logger) (time.Duration, error) {
	raw := minter.DefaultReportDebounce.String()
	if v, ok := os.LookupEnv("WAXSEAL_REPORT_DEBOUNCE"); ok {
		raw = v
	}
	if cmd.Flags().Changed("report-debounce") {
		raw = o.reportDebounce
	}
	if raw = strings.TrimSpace(raw); raw == "" {
		return minter.DefaultReportDebounce, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, &usageError{msg: fmt.Sprintf("invalid --report-debounce %q: %v (use a Go duration like 5m)", raw, err)}
	}
	if d < reportDebounceFloor {
		return 0, &usageError{msg: fmt.Sprintf("invalid --report-debounce %q: must be at least %s to prevent excessive re-attestation", raw, reportDebounceFloor)}
	}
	if d > reportDebounceWarn {
		logger.Warn("report-debounce is large; report-driven recycling will be infrequent", "value", d)
	} else {
		logger.Info("report-debounce set", "value", d)
	}
	return d, nil
}

// resolveShutdownTimeout applies flag, environment, and default precedence. The
// result bounds the SIGTERM/SIGINT drain; a non-positive value would either
// abandon in-flight requests immediately or make shutdown wait forever, so
// both are rejected the same as an unparseable duration.
func resolveShutdownTimeout(cmd *cobra.Command, o *serverOpts, logger *slog.Logger) (time.Duration, error) {
	raw := defaultShutdownTimeout.String()
	if v, ok := os.LookupEnv("WAXSEAL_SHUTDOWN_TIMEOUT"); ok {
		raw = v
	}
	if cmd.Flags().Changed("shutdown-timeout") {
		raw = o.shutdownTimeout
	}
	if raw = strings.TrimSpace(raw); raw == "" {
		return defaultShutdownTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, &usageError{msg: fmt.Sprintf("invalid --shutdown-timeout %q: %v (use a Go duration like 60s)", raw, err)}
	}
	if d <= 0 {
		return 0, &usageError{msg: fmt.Sprintf("invalid --shutdown-timeout %q: must be positive", raw)}
	}
	logger.Info("shutdown-timeout set", "value", d)
	return d, nil
}

// unbracketHost removes one pair of surrounding brackets. This allows --host to
// accept IPv6 literals in bare or bracketed form before passing them to
// net.JoinHostPort or net.ParseIP.
func unbracketHost(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

// bindListener validates the port and binds the listen address. An invalid port
// is a usage error. Other bind failures retain the error returned by net.Listen.
// Port 0 asks the operating system to select an available port.
func bindListener(host string, port int) (net.Listener, error) {
	if port < 0 || port > 65535 {
		return nil, &usageError{msg: fmt.Sprintf("invalid --port %d: must be 0-65535", port)}
	}
	return net.Listen("tcp", net.JoinHostPort(unbracketHost(host), strconv.Itoa(port)))
}

// isExposedHost reports whether host may accept connections from outside the
// local machine. Only "localhost" and literal loopback addresses are considered
// private. All other values, including wildcard addresses and hostnames, are
// considered exposed.
func isExposedHost(host string) bool {
	host = unbracketHost(host)
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}

// logMetricsAccess reports the effective /metrics access mode. Metrics flags
// apply only to keyed daemons; keyless daemons already serve full detail. When
// both flags are set, --metrics-public wins.
func logMetricsAccess(logger *slog.Logger, keyed, metricsPublic, metricsKeySet bool) {
	switch {
	case !keyed:
		if metricsPublic || metricsKeySet {
			logger.Warn("--metrics-public/--metrics-key are ignored without --tenant-keys; a keyless daemon already serves full /metrics detail")
		}
	case metricsPublic:
		if metricsKeySet {
			logger.Warn("both --metrics-public and --metrics-key set; --metrics-public wins: /metrics serves full per-tenant detail unauthenticated")
		} else {
			logger.Warn("--metrics-public: /metrics serves full per-tenant detail (tenant labels + activity) to unauthenticated scrapes")
		}
	case metricsKeySet:
		logger.Info("/metrics serves a redacted aggregate to unauthenticated scrapes; --metrics-key unlocks full per-tenant detail")
	default:
		logger.Info("/metrics serves a redacted aggregate on this keyed daemon; pass --metrics-key (authenticated) or --metrics-public (trusted network) for full per-tenant detail")
	}
}

// warnKeylessExposure reports a configuration that exposes guest identity data
// from an unauthenticated daemon.
func warnKeylessExposure(logger *slog.Logger, keyed bool, host string) {
	if !keyed && isExposedHost(host) {
		logger.Warn("keyless daemon exposes the guest identity through /session and /player-context; pass --tenant-keys to require authentication", "host", host)
	}
}

// failStartup logs a configuration error once and preserves its exit-code type.
func failStartup(logger *slog.Logger, err error) error {
	logger.Error("startup: invalid configuration", "err", err)
	return err
}

func runServer(cmd *cobra.Command, o *serverOpts) error {
	level := "info"
	if o.verbose {
		level = "debug"
	}
	logger := buildLogger(level, os.Stdout) // daemon logs to stdout

	// Validate configuration before binding a socket or launching Chromium.
	if err := validateLandingVideo(o.video); err != nil {
		return failStartup(logger, err)
	}
	streamingMaxAge, err := resolveStreamingMaxAge(cmd, o, logger)
	if err != nil {
		return failStartup(logger, err)
	}
	reportDebounce, err := resolveReportDebounce(cmd, o, logger)
	if err != nil {
		return failStartup(logger, err)
	}
	drainTimeout, err := resolveShutdownTimeout(cmd, o, logger)
	if err != nil {
		return failStartup(logger, err)
	}
	keys, err := server.ParseTenantKeys(o.tenantKeys)
	if err != nil {
		return failStartup(logger, &usageError{msg: err.Error()})
	}
	// Reject a metrics key that is also a tenant key before launching the browser.
	// Name the tenant label, never the key material. server.New enforces the same
	// rule for programmatic callers.
	if label, collides := server.MetricsKeyCollision(keys, o.metricsKey); collides {
		return failStartup(logger, &usageError{
			msg: fmt.Sprintf("metrics key collides with API key for tenant %q", label)})
	}

	// Bind before launching Chromium so an invalid or unavailable address fails
	// without running browser startup and attestation.
	ln, err := bindListener(o.host, o.port)
	if err != nil {
		logger.Error("startup: bind listen address failed", "err", err)
		return err
	}
	logger.Info("listening socket bound; launching browser", "addr", ln.Addr().String())
	// Close the listener on startup failures. Serve owns it after startup succeeds.
	served := false
	defer func() {
		if !served {
			_ = ln.Close()
		}
	}()

	// Remove profiles left by prior daemon instances that could not run normal cleanup.
	browser.ReapStaleProfiles(logger)
	// Warn before browser startup when unauthenticated callers can access the guest
	// identity.
	warnKeylessExposure(logger, len(keys) > 0, o.host)
	logMetricsAccess(logger, len(keys) > 0, o.metricsPublic, o.metricsKey != "")

	srv, err := server.New(server.Config{
		Addr:            ln.Addr().String(),
		Video:           o.video,
		Headful:         o.headful,
		TenantKeys:      keys,
		Logger:          logger,
		StreamingMaxAge: streamingMaxAge,
		ReportDebounce:  reportDebounce,
		MetricsPublic:   o.metricsPublic,
		MetricsKey:      o.metricsKey,
	})
	if err != nil {
		logger.Error("startup: launch browser failed", "err", err)
		return err
	}

	// Warm one tenant so the first request is fast and startup catches attestation
	// failures. Other tenants attest on first use.
	warmKey := ""
	for k := range keys {
		warmKey = k
		break
	}
	warmCtx, cancel := context.WithTimeout(cmd.Context(), 120*time.Second)
	err = srv.Warm(warmCtx, warmKey)
	if err == nil {
		// Verify minting before accepting traffic and attempt the full-length
		// streaming proof. SelfTest reports mint failures and logs proof failures;
		// /player-context and /session retry the proof on demand.
		err = srv.SelfTest(warmCtx, warmKey)
	}
	cancel()
	if err != nil {
		logger.Error("startup checks failed", "err", err)
		_ = srv.Shutdown(context.Background())
		return err
	}
	if len(keys) == 0 {
		logger.Info("mode: keyless single-tenant")
	} else {
		logger.Info("mode: multi-tenant", "tenants", len(keys))
	}

	errCh := make(chan error, 1)
	// Serve closes the listener before returning.
	served = true
	go func() {
		logger.Info("waxseal server listening (bgutil /get_pot)", "addr", ln.Addr().String())
		errCh <- srv.Serve(ln)
	}()

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			logger.Error("listen failed", "err", err)
			_ = srv.Shutdown(context.Background())
			return err
		}
	}
	logger.Info("shutting down", "drain_timeout", drainTimeout)
	shutCtx, c := context.WithTimeout(context.Background(), drainTimeout)
	defer c()
	if err := srv.Shutdown(shutCtx); err != nil {
		// The browser and its profile are torn down by Shutdown regardless of the
		// drain result, so a drain budget that simply ran out
		// (context.DeadlineExceeded, wrapped or bare) is routine: it is a warning
		// about severed connections, not a failed stop. Any other error means the
		// stop itself failed, and is returned so the existing exit-code mapping
		// reports a real failure instead of every busy shutdown logging as a
		// routine warning.
		if !shutdownOutcome(err) {
			logger.Error("shutdown failed", "err", err, "drain_timeout", drainTimeout)
			return err
		}
		logger.Warn("drain budget expired; in-flight requests were severed", "err", err, "drain_timeout", drainTimeout)
	}
	return nil
}

// shutdownOutcome classifies the error srv.Shutdown returned. A nil error or a
// context.DeadlineExceeded (wrapped or bare) means the drain budget simply ran
// out: routine reports true, since the browser and its profile are torn down
// by Shutdown regardless of the drain result and only in-flight requests were
// severed. Any other error means the stop itself failed, so routine reports
// false and the caller returns the error instead of warning past it.
func shutdownOutcome(err error) (routine bool) {
	return err == nil || errors.Is(err, context.DeadlineExceeded)
}
