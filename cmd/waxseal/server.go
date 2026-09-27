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
	tenantKeysFile  string
	streamingMaxAge string
	reportDebounce  string
	shutdownTimeout string
	metricsPublic   bool
	metricsKey      string
	metricsKeyFile  string
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

	// defaultShutdownTimeout bounds the drain on SIGTERM or SIGINT. A cold
	// /player-context takes 6 to 10 s and a first session usually under 10 s,
	// so a request past a minute is pathological. Sizing it to the 3 minute
	// requestProcessTimeout would force a matching stop_grace_period and hang
	// `docker compose down` that long on a wedged daemon.
	defaultShutdownTimeout = 60 * time.Second
)

func newServerCmd() *cobra.Command {
	var o serverOpts
	c := &cobra.Command{
		Use:   "server",
		Short: "Run the bgutil-compatible HTTP daemon",
		Long: "Run the HTTP daemon over a real headless Chromium. It defaults to loopback\n" +
			"at 127.0.0.1:4416. Set --host 0.0.0.0 to expose it. With tenant keys (see\n" +
			"--tenant-keys), each key receives an isolated browser context; without\n" +
			"them, the server is keyless. On SIGTERM or SIGINT it drains in-flight requests\n" +
			"for up to --shutdown-timeout (default 60s) before tearing the browser down.\n\n" +
			"Three settings are read from the environment only:\n" +
			"  WAXSEAL_MINT_SEPARATION  spacing kept between a token mint and a context\n" +
			"                           establishment; a positive Go duration, default 12s\n" +
			"  WAXSEAL_CHROME_BIN       the Chromium binary, for every command\n" +
			"  WAXSEAL_UA_HINTS         client-hint source, real (default) or synthetic",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runServer(cmd, &o) },
	}
	f := c.Flags()
	f.StringVar(&o.host, "host", "127.0.0.1", "bind address (set 0.0.0.0 to expose)")
	f.IntVar(&o.port, "port", 4416, "listen port")
	f.StringVar(&o.video, "video", browser.DefaultVideo, "landing video for each tenant session")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.StringVar(&o.tenantKeys, "tenant-keys", "",
		`multi-tenant API keys in "label1=key1,label2=key2" form (or --tenant-keys-file,`+"\n"+
			`WAXSEAL_TENANT_KEYS, or WAXSEAL_TENANT_KEYS_FILE; flags outrank envs)`)
	f.StringVar(&o.tenantKeysFile, "tenant-keys-file", "",
		"path to a file holding what --tenant-keys would carry, so the keys stay out of\n"+
			"the process arguments. Setting both is a usage error.")
	f.StringVar(&o.streamingMaxAge, "streaming-max-age", "",
		"recycle a session on its next streaming handoff once older than this Go duration\n"+
			"(flag > WAXSEAL_STREAMING_MAX_AGE env > 45m default; \"0\" disables). The\n"+
			"first streaming request after a recycle waits for re-attestation and\n"+
			"establishment. Idle sessions are not recycled. Minimum 1m. The deadline is\n"+
			"jittered by up to ten percent so a fleet does not recycle in lockstep;\n"+
			"streaming_seconds_until_recycle counts down to the jittered deadline.")
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
			"unauthenticated scrapes on a keyed daemon. Ignored without tenant keys\n"+
			"because keyless daemons already serve full detail.")
	f.StringVar(&o.metricsKey, "metrics-key", "",
		"operator key that unlocks full per-tenant /metrics detail on a keyed daemon.\n"+
			"Without it (or --metrics-public), a keyed daemon serves unauthenticated\n"+
			"scrapes a redacted, label-free aggregate. Must differ from every tenant\n"+
			"key. Ignored without tenant keys, though a source that cannot be read is\n"+
			"still a usage error. Also reachable through --metrics-key-file,\n"+
			"WAXSEAL_METRICS_KEY, and WAXSEAL_METRICS_KEY_FILE; flags outrank envs.")
	f.StringVar(&o.metricsKeyFile, "metrics-key-file", "",
		"path to a file holding what --metrics-key would carry. Setting both is a\n"+
			"usage error.")
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

// resolveShutdownTimeout applies flag, environment, and default precedence. A
// non-positive value is a usage error, like an unparseable one: as a drain
// budget it would mean either severing requests at once or waiting forever.
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

// resolveMintSeparation reads WAXSEAL_MINT_SEPARATION, which has no flag, the
// way the duration flags are read: blank keeps the default, anything else is a
// positive Go duration or a usage error, decided before the socket binds.
func resolveMintSeparation(logger *slog.Logger) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(minter.MintSeparationEnv))
	d, err := minter.ParseMintSeparation(raw)
	if err != nil {
		return 0, &usageError{msg: err.Error()}
	}
	switch {
	case raw == "":
		logger.Info("mint-separation set", "value", d, "source", "default")
	case d > minter.MintSeparationWarn:
		logger.Warn(minter.MintSeparationEnv+" is large; values near the request budget make first contexts time out", "value", d)
	default:
		logger.Info("mint-separation set", "value", d, "source", minter.MintSeparationEnv)
	}
	return d, nil
}

// keySource names the four places one piece of key material can come from: a
// value flag and a file flag, then a value env and a file env. The _FILE envs
// follow the Docker convention, so a compose file reaches a secret from an
// environment block without overriding the image's CMD.
type keySource struct {
	valueFlag, fileFlag string
	valueEnv, fileEnv   string
	value, file         string
}

// resolveKeyMaterial applies flag, environment, and default precedence like the
// duration options, except that a value and a file in the same tier are a usage
// error rather than a silent winner, which could key the daemon with something
// the operator did not choose. It also returns the flag or variable that
// supplied the value ("" if none did), so a later refusal can name it. No error
// it returns carries key material.
func resolveKeyMaterial(cmd *cobra.Command, src keySource) (value, from string, err error) {
	valueSet, fileSet := cmd.Flags().Changed(src.valueFlag), cmd.Flags().Changed(src.fileFlag)
	switch {
	case valueSet && fileSet:
		return "", "", &usageError{msg: fmt.Sprintf("--%s and --%s are mutually exclusive; pass one", src.valueFlag, src.fileFlag)}
	case valueSet:
		// A flag the operator typed is a choice, so a blank one is refused
		// rather than falling through to the environment it was meant to override.
		v := strings.TrimSpace(src.value)
		if v == "" {
			return "", "", &usageError{msg: fmt.Sprintf("--%s is empty: pass a value, or omit the flag", src.valueFlag)}
		}
		return v, "--" + src.valueFlag, nil
	case fileSet:
		from = "--" + src.fileFlag
		value, err = readKeyFile(from, src.file)
		return value, from, err
	}
	envValue, hasValue := lookupNonEmpty(src.valueEnv)
	envFile, hasFile := lookupNonEmpty(src.fileEnv)
	switch {
	case hasValue && hasFile:
		return "", "", &usageError{msg: fmt.Sprintf("%s and %s are mutually exclusive; set one", src.valueEnv, src.fileEnv)}
	case hasValue:
		return envValue, src.valueEnv, nil
	case hasFile:
		value, err = readKeyFile(src.fileEnv, envFile)
		return value, src.fileEnv, err
	}
	return "", "", nil
}

// lookupNonEmpty reads an environment variable, reporting false when it is
// unset, empty, or only whitespace. A blank variable is how a compose file says
// nothing, and counting it as a value would put it in conflict with the file
// variable beside it and refuse to start over a setting the operator never made.
func lookupNonEmpty(name string) (string, bool) {
	if v, ok := os.LookupEnv(name); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v, true
		}
	}
	return "", false
}

// readKeyFile reads one secret from path; source names the flag or environment
// variable that pointed here, so a failure says which to fix. Surrounding
// whitespace and a leading byte-order mark are stripped: a trailing newline or
// an editor's BOM would otherwise yield a key that silently matches nothing. An
// empty file is a usage error, not a keyless daemon.
func readKeyFile(source, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", &usageError{msg: fmt.Sprintf("%s: %v", source, err)}
	}
	v := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	if v == "" {
		// Plain quotes rather than %q, which would double every separator in a
		// Windows path and name a file the operator never passed.
		return "", &usageError{msg: fmt.Sprintf(`%s "%s" is empty`, source, path)}
	}
	return v, nil
}

// unbracketHost removes one pair of surrounding brackets, so --host accepts an
// IPv6 literal bare or bracketed without net.JoinHostPort doubling them.
func unbracketHost(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

// validatePort rejects a port outside the range a listener can bind. It runs
// with the rest of the configuration, before anything is announced, and again
// inside bindListener for callers that reach it directly.
func validatePort(port int) error {
	if port < 0 || port > 65535 {
		return &usageError{msg: fmt.Sprintf("invalid --port %d: must be 0-65535", port)}
	}
	return nil
}

// bindListener validates the port and binds the listen address. An invalid port
// is a usage error. Other bind failures retain the error returned by net.Listen.
// Port 0 asks the operating system to select an available port.
func bindListener(host string, port int) (net.Listener, error) {
	if err := validatePort(port); err != nil {
		return nil, err
	}
	return net.Listen("tcp", net.JoinHostPort(unbracketHost(host), strconv.Itoa(port)))
}

// isExposedAddr reports whether the bound address accepts connections from
// off the machine. The listener's own address is what counts: "LOCALHOST" and
// the host's own name resolve to loopback, and 0.0.0.0 binds dual-stack.
func isExposedAddr(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return !ok || !tcp.IP.IsLoopback()
}

// logMetricsAccess reports the effective /metrics access mode. Metrics flags
// apply only to keyed daemons; keyless daemons already serve full detail. When
// both flags are set, --metrics-public wins.
func logMetricsAccess(logger *slog.Logger, keyed, metricsPublic, metricsKeySet bool) {
	switch {
	case !keyed:
		if metricsPublic || metricsKeySet {
			logger.Warn("--metrics-public/--metrics-key are ignored without tenant keys; a keyless daemon already serves full /metrics detail")
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
func warnKeylessExposure(logger *slog.Logger, keyed bool, addr net.Addr) {
	if !keyed && isExposedAddr(addr) {
		logger.Warn("keyless daemon exposes the guest identity through /session and /player-context; pass --tenant-keys to require authentication", "addr", addr.String())
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
	logger := buildLogger(level, cmd.OutOrStdout()) // daemon logs to stdout

	// Validate configuration before binding a socket or launching Chromium.
	if err := validateLandingVideo(o.video); err != nil {
		return failStartup(logger, err)
	}
	if err := validatePort(o.port); err != nil {
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
	mintSeparation, err := resolveMintSeparation(logger)
	if err != nil {
		return failStartup(logger, err)
	}
	tenantKeys, tenantKeysFrom, err := resolveKeyMaterial(cmd, keySource{
		valueFlag: "tenant-keys", fileFlag: "tenant-keys-file",
		valueEnv: "WAXSEAL_TENANT_KEYS", fileEnv: "WAXSEAL_TENANT_KEYS_FILE",
		value: o.tenantKeys, file: o.tenantKeysFile,
	})
	if err != nil {
		return failStartup(logger, err)
	}
	metricsKey, _, err := resolveKeyMaterial(cmd, keySource{
		valueFlag: "metrics-key", fileFlag: "metrics-key-file",
		valueEnv: "WAXSEAL_METRICS_KEY", fileEnv: "WAXSEAL_METRICS_KEY_FILE",
		value: o.metricsKey, file: o.metricsKeyFile,
	})
	if err != nil {
		return failStartup(logger, err)
	}
	keys, err := server.ParseTenantKeys(tenantKeys)
	if err != nil {
		return failStartup(logger, &usageError{msg: tenantKeysFrom + ": " + err.Error()})
	}
	if metricsKey != "" {
		if err := server.CheckKeyChars(metricsKey); err != nil {
			return failStartup(logger, &usageError{msg: "metrics key: " + err.Error()})
		}
	}
	// Reject a metrics key that is also a tenant key before launching the browser.
	// Name the tenant label, never the key material. server.New enforces the same
	// rule for programmatic callers.
	if label, collides := server.MetricsKeyCollision(keys, metricsKey); collides {
		return failStartup(logger, &usageError{
			msg: fmt.Sprintf("metrics key collides with API key for tenant %q", label)})
	}

	// Bind before launching Chromium so an invalid or unavailable address fails
	// without running browser startup and attestation.
	ln, err := bindListener(o.host, o.port)
	if err != nil {
		// A backstop: validatePort above already refused a bad port, so this only
		// fires if that check ever moves, and then logs the refusal as invalid
		// configuration rather than a bind failure.
		if ue, ok := errors.AsType[*usageError](err); ok {
			return failStartup(logger, ue)
		}
		logger.Error("startup: bind listen address failed", "err", err)
		return err
	}
	logger.Info("listening socket bound; launching browser", "addr", ln.Addr().String(), "version", version)
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
	warnKeylessExposure(logger, len(keys) > 0, ln.Addr())
	logMetricsAccess(logger, len(keys) > 0, o.metricsPublic, metricsKey != "")

	srv, err := server.NewWithContext(cmd.Context(), server.Config{
		Addr:            ln.Addr().String(),
		Video:           o.video,
		Headful:         o.headful,
		TenantKeys:      keys,
		Logger:          logger,
		StreamingMaxAge: streamingMaxAge,
		ReportDebounce:  reportDebounce,
		MintSeparation:  mintSeparation,
		MetricsPublic:   o.metricsPublic,
		MetricsKey:      metricsKey,
	})
	if err != nil {
		if cmd.Context().Err() != nil {
			logger.Info("interrupted during browser launch; exiting")
			return nil
		}
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
		if cmd.Context().Err() != nil {
			// A stop requested during warm-up is a clean stop, as it is once
			// serving: a supervisor that restarts on non-zero must not see the
			// two differently.
			logger.Info("interrupted during startup checks; shutting down")
			_ = srv.Shutdown(context.Background())
			return nil
		}
		logger.Error("startup checks failed", "err", err)
		_ = srv.Shutdown(context.Background())
		return err
	}
	if len(keys) == 0 {
		logger.Info("mode: keyless single-tenant")
	} else {
		logger.Info("mode: multi-tenant", "configured_tenants", len(keys))
	}

	errCh := make(chan error, 1)
	// Serve closes the listener before returning.
	served = true
	go func() {
		logger.Info("waxseal server listening (bgutil /get_pot)", "addr", ln.Addr().String())
		errCh <- srv.Serve(ln)
	}()

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			stop()
			logger.Error("listen failed", "err", err)
			_ = srv.Shutdown(context.Background())
			return err
		}
	}
	// A second signal cancels the drain context, so Shutdown returns and the
	// browser is torn down at once. It is registered before the first handler
	// is released, so a signal between the two is caught instead of killing
	// the process through the default disposition.
	sigCtx, stopSecond := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSecond()
	stop() // the first signal is consumed; the drain's own handler owns the rest
	logger.Info("shutting down; a second signal cuts the drain short", "drain_timeout", drainTimeout)
	shutCtx, c := context.WithTimeout(sigCtx, drainTimeout)
	defer c()
	err = srv.Shutdown(shutCtx)
	switch {
	case err == nil:
	case sigCtx.Err() != nil && errors.Is(err, context.Canceled):
		logger.Warn("second signal received; in-flight requests were severed")
	// An expired drain budget only severed requests (see shutdownOutcome); any
	// other error is a failed stop and is returned.
	case shutdownOutcome(err):
		logger.Warn("drain budget expired; in-flight requests were severed", "err", err, "drain_timeout", drainTimeout)
	default:
		logger.Error("shutdown failed", "err", err, "drain_timeout", drainTimeout)
		return err
	}
	logger.Info("waxseal server stopped")
	return nil
}

// shutdownOutcome reports whether srv.Shutdown's error is routine: nil, or a
// DeadlineExceeded (wrapped or bare) from a drain budget that ran out. Shutdown
// tears the browser and its profile down either way, so an expired budget only
// severed in-flight requests. Any other error means the stop itself failed.
func shutdownOutcome(err error) (routine bool) {
	return err == nil || errors.Is(err, context.DeadlineExceeded)
}
