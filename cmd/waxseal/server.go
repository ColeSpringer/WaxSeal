package main

import (
	"context"
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
)

func newServerCmd() *cobra.Command {
	var o serverOpts
	c := &cobra.Command{
		Use:   "server",
		Short: "Run the bgutil-compatible HTTP daemon",
		Long: "Run the HTTP daemon over a real headless Chromium. It defaults to loopback\n" +
			"at 127.0.0.1:4416. Set --host 0.0.0.0 to expose it. With --tenant-keys,\n" +
			"each key receives an isolated browser context. Without it, the server is\n" +
			"keyless. It drains in-flight requests on SIGTERM or SIGINT.",
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
		"minimum spacing between consumer-report-driven (POST /report) session recycles\n"+
			"(flag > WAXSEAL_REPORT_DEBOUNCE env > 5m default). This limits\n"+
			"re-attestation caused by reports and applies separately to each tenant.\n"+
			"Minimum 5s; report rate-limiting cannot be disabled.")
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
		return 0, fmt.Errorf("invalid --streaming-max-age %q: %w (use a Go duration like 45m, or 0 to disable)", raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid --streaming-max-age %q: must not be negative (use 0 to disable)", raw)
	}
	if d > 0 && d < streamingMaxAgeFloor {
		return 0, fmt.Errorf("invalid --streaming-max-age %q: must be at least %s to prevent excessive re-attestation", raw, streamingMaxAgeFloor)
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
		return 0, fmt.Errorf("invalid --report-debounce %q: %w (use a Go duration like 5m)", raw, err)
	}
	if d < reportDebounceFloor {
		return 0, fmt.Errorf("invalid --report-debounce %q: must be at least %s to prevent excessive re-attestation", raw, reportDebounceFloor)
	}
	if d > reportDebounceWarn {
		logger.Warn("report-debounce is large; report-driven recycling will be infrequent", "value", d)
	} else {
		logger.Info("report-debounce set", "value", d)
	}
	return d, nil
}

func runServer(cmd *cobra.Command, o *serverOpts) error {
	level := "info"
	if o.verbose {
		level = "debug"
	}
	logger := buildLogger(level, os.Stdout) // daemon logs to stdout
	streamingMaxAge, err := resolveStreamingMaxAge(cmd, o, logger)
	if err != nil {
		logger.Error("startup: invalid configuration", "err", err)
		return err
	}
	reportDebounce, err := resolveReportDebounce(cmd, o, logger)
	if err != nil {
		logger.Error("startup: invalid configuration", "err", err)
		return err
	}
	// Remove profiles left by prior daemon instances that could not run normal cleanup.
	browser.ReapStaleProfiles(logger)
	keys := server.ParseTenantKeys(o.tenantKeys)

	srv, err := server.New(server.Config{
		Addr:            net.JoinHostPort(o.host, strconv.Itoa(o.port)),
		Video:           o.video,
		Headful:         o.headful,
		TenantKeys:      keys,
		Logger:          logger,
		StreamingMaxAge: streamingMaxAge,
		ReportDebounce:  reportDebounce,
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
	go func() {
		logger.Info("waxseal server listening (bgutil /get_pot)", "addr", srv.Addr())
		errCh <- srv.ListenAndServe()
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
	logger.Info("shutting down")
	shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	return srv.Shutdown(shutCtx)
}
