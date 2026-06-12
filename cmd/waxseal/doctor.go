package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/spf13/cobra"
)

// doctorOpts holds doctor-subcommand flags.
type doctorOpts struct {
	video   string
	headful bool
	verbose bool
	full    bool
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
			"the probe verifies full-length streaming.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runDoctor(cmd, &o) },
	}
	f := c.Flags()
	f.StringVar(&o.video, "video", browser.DefaultVideo, "landing video for the browser session")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "verbose logging to stderr")
	f.BoolVar(&o.full, "full", false, "verify streaming past the roughly 70-second preview cap")
	return c
}

func runDoctor(cmd *cobra.Command, o *doctorOpts) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	level := "warn"
	if o.verbose {
		level = "info"
	}
	logger := buildLogger(level, stderr)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	sess, err := browser.Launch(ctx, o.video, browser.Options{Headful: o.headful, NormalizeUA: !o.headful, Logger: logger})
	if err != nil {
		return fmt.Errorf("browser launch/identity: %w", err)
	}
	defer sess.Close()

	if err := sess.Attest(ctx); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	kind := sess.AttestKind()
	report := map[string]any{
		"attest":   kind,
		"identity": sess.Identity(),
	}

	// Run the optional probe before writing the report so failed and skipped probes
	// are included in the output.
	var probe browser.FullLengthProbe
	var probeErr error
	if o.full {
		probe, probeErr = sess.VerifyFullLength(ctx, o.video)
		report["full_length_probe"] = probe
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
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
