package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/spf13/cobra"
)

// doctorOpts holds doctor-subcommand flags.
type doctorOpts struct {
	video         string
	headful       bool
	verbose       bool
	full          bool
	skipAttest    bool
	landingURL    string
	stopAfterLoad bool
}

func newDoctorCmd() *cobra.Command {
	var o doctorOpts
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Launch a browser, attest, and report the identity and token grade",
		Long: "Launch a real Chromium, run the BotGuard attestation, and report the\n" +
			"captured identity and token grade. The command exits nonzero if the browser\n" +
			"or egress IP cannot attest, or if only a fallback token is available.\n\n" +
			"With --full, it also checks whether the browser can stream beyond the\n" +
			"roughly 70-second status-2 preview cap. The command exits nonzero unless\n" +
			"the probe verifies full-length streaming.\n\n" +
			"--skip-attest reports the captured identity and stops without attesting.\n" +
			"--stop-after-load stops earlier still, at the load event of a page the\n" +
			"command serves to itself on loopback, so it checks that Chromium can render\n" +
			"and navigate with no external network at all. --landing-url points that\n" +
			"check at another page instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runDoctor(cmd, &o) },
	}
	f := c.Flags()
	f.StringVar(&o.video, "video", browser.DefaultVideo, "landing video for the browser session (validated but ignored with --stop-after-load)")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "verbose logging to stderr")
	f.BoolVar(&o.full, "full", false, "verify streaming past the roughly 70-second preview cap")
	f.BoolVar(&o.skipAttest, "skip-attest", false, "report the identity and stop, without attesting")
	f.BoolVar(&o.stopAfterLoad, "stop-after-load", false, "stop at the load event, before identity capture; serves its own loopback page")
	f.StringVar(&o.landingURL, "landing-url", "", "park on this URL instead (needs --stop-after-load)")
	return c
}

// validateDoctorStage rejects flag combinations that ask for a check the chosen
// stage cannot deliver.
func validateDoctorStage(o *doctorOpts) error {
	if o.full && (o.skipAttest || o.stopAfterLoad) {
		return &usageError{msg: "--full needs an attested session, so it cannot be combined with --skip-attest or --stop-after-load"}
	}
	if o.stopAfterLoad && o.skipAttest {
		return &usageError{msg: "--stop-after-load already stops before attestation runs, so it cannot be combined with --skip-attest"}
	}
	if o.landingURL != "" && !o.stopAfterLoad {
		return &usageError{msg: "--landing-url needs --stop-after-load, because the identity is only readable on a watch page"}
	}
	return nil
}

func runDoctor(cmd *cobra.Command, o *doctorOpts) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := validateDoctorStage(o); err != nil {
		return err
	}
	if err := validateLandingVideo(o.video); err != nil {
		return err
	}
	level := "warn"
	if o.verbose {
		level = "info"
	}
	logger := buildLogger(level, stderr)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	landingURL := o.landingURL
	if o.stopAfterLoad && landingURL == "" {
		pageURL, stop, serr := serveLandingPage()
		if serr != nil {
			return serr
		}
		defer stop()
		landingURL = pageURL
	}

	sess, err := browser.Launch(ctx, o.video, browser.Options{
		Headful:       o.headful,
		NormalizeUA:   !o.headful,
		Logger:        logger,
		LandingURL:    landingURL,
		StopAfterLoad: o.stopAfterLoad,
	})
	if err != nil {
		return fmt.Errorf("browser launch/identity: %w", err)
	}
	defer sess.Close()

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")

	if o.stopAfterLoad {
		// Nothing past the load event ran, so the parked page is the whole report.
		// Reaching here means Chromium fetched the page, started a renderer, and
		// evaluated JavaScript in the loaded document.
		_ = enc.Encode(map[string]any{"stage": "load", "landing_url": sess.Identity().WatchURL})
		return nil
	}

	if o.skipAttest {
		_ = enc.Encode(doctorReport(sess.Identity(), ""))
		return nil
	}

	if err := sess.Attest(ctx); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	kind := sess.AttestKind()
	report := doctorReport(sess.Identity(), kind)

	// Run the optional probe before writing the report so failed and skipped probes
	// are included in the output.
	var probe browser.FullLengthProbe
	var probeErr error
	if o.full {
		probe, probeErr = sess.VerifyFullLength(ctx, o.video)
		report["full_length_probe"] = probe
	}

	_ = enc.Encode(report)

	if o.full {
		// A successful full-length probe is stronger evidence than the
		// attestation grade.
		if probeErr != nil {
			return fmt.Errorf("full-length probe: %w", probeErr)
		}
		if probe.Outcome != browser.OutcomeFullLength {
			return fmt.Errorf("full-length not verified (outcome %q): %s", probe.Outcome, probe.Reason)
		}
		// Once full-length playback is verified, a non-integrity attestation grade
		// is informational rather than fatal.
		if kind != "integrity" {
			fmt.Fprintf(stderr, "waxseal: note: attestation grade is %q, but full-length streaming was verified\n", kind)
		}
		return nil
	}
	// Without the full-length probe, require an integrity attestation.
	if kind != "integrity" {
		return fmt.Errorf("attestation grade is %q, not integrity", kind)
	}
	return nil
}

// doctorReport assembles the report for a session that reached the identity. An
// empty kind means no attestation ran, and the attest key is then left out
// entirely so a skipped check cannot be misread as a failed one.
func doctorReport(id browser.Identity, kind string) map[string]any {
	report := map[string]any{"identity": id}
	if kind != "" {
		report["attest"] = kind
	}
	return report
}

// landingPageHTML is the inert page --stop-after-load serves to itself.
const landingPageHTML = "<!doctype html><title>waxseal renderer check</title><p>ok\n"

// serveLandingPage starts an HTTP server on loopback that serves one inert page
// and returns its URL and a shutdown function. Navigating to it exercises
// Chromium's network stack as well as its renderer, which a data: or file: URL
// does not, while still reaching nothing outside the host.
func serveLandingPage() (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("landing page listener: %w", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, landingPageHTML)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String() + "/", func() { _ = srv.Close() }, nil
}
