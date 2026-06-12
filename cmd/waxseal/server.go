package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/server"
	"github.com/spf13/cobra"
)

// serverOpts holds server-subcommand flags.
type serverOpts struct {
	host       string
	port       int
	video      string
	headful    bool
	tenantKeys string
	verbose    bool
}

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
	f.BoolVarP(&o.verbose, "verbose", "v", false, "enable debug logging")
	return c
}

func runServer(cmd *cobra.Command, o *serverOpts) error {
	level := "info"
	if o.verbose {
		level = "debug"
	}
	logger := buildLogger(level, os.Stdout) // daemon logs to stdout
	// Remove profiles left by prior daemon instances that could not run normal cleanup.
	browser.ReapStaleProfiles(logger)
	keys := server.ParseTenantKeys(o.tenantKeys)

	srv, err := server.New(server.Config{
		Addr:       net.JoinHostPort(o.host, strconv.Itoa(o.port)),
		Video:      o.video,
		Headful:    o.headful,
		TenantKeys: keys,
		Logger:     logger,
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
	warmCtx, cancel := context.WithTimeout(cmd.Context(), 90*time.Second)
	err = srv.Warm(warmCtx, warmKey)
	cancel()
	if err != nil {
		logger.Error("startup attestation failed", "err", err)
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
