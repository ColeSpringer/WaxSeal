package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/spf13/cobra"
)

// genOpts holds generate-mode flags.
type genOpts struct {
	contentBinding string
	video          string
	headful        bool
	verbose        bool
}

func bindGenerateFlags(cmd *cobra.Command, g *genOpts) {
	f := cmd.Flags()
	f.StringVarP(&g.contentBinding, "content-binding", "c", "", "binding to mint (video_id for player, visitor_data for gvs)")
	f.StringVar(&g.video, "video", browser.DefaultVideo, "landing video for the browser session")
	f.BoolVar(&g.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&g.verbose, "verbose", "v", false, "verbose logging to stderr")
}

// newGetPotCmd is an explicit alias for the default generate command.
func newGetPotCmd() *cobra.Command {
	var g genOpts
	c := &cobra.Command{
		Use:   "get-pot",
		Short: "Generate a PO token (alias for the default command)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runGenerate(cmd, &g) },
	}
	bindGenerateFlags(c, &g)
	return c
}

// runGenerate launches a fresh browser, attests, mints one token, and prints
// JSON on the last stdout line. On failure it prints "{}", as the bgutil
// script-provider contract requires, and returns the error for renderError.
func runGenerate(cmd *cobra.Command, g *genOpts) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if g.contentBinding == "" {
		fmt.Fprintln(stdout, "{}")
		return &usageError{msg: "content-binding (-c) is required"}
	}
	if g.contentBinding == "=" {
		// pflag reads the shorthand form "-c=" (no value) as the value "=".
		fmt.Fprintln(stdout, "{}")
		return &usageError{msg: `content-binding "=" is not a binding; "-c=" with no value is read as "=", pass -c <binding>`}
	}
	if len(g.contentBinding) > browser.MaxContentBindingBytes {
		fmt.Fprintln(stdout, "{}")
		return &usageError{msg: fmt.Sprintf("content-binding too long (max %d bytes)", browser.MaxContentBindingBytes)}
	}
	if browser.HasControlChars(g.contentBinding) {
		// Keep CLI validation aligned with /get_pot.
		fmt.Fprintln(stdout, "{}")
		return &usageError{msg: "content-binding must not contain control characters"}
	}
	if err := validateLandingVideo(g.video); err != nil {
		fmt.Fprintln(stdout, "{}")
		return err
	}
	maybeWarnURLBinding(stderr, g.contentBinding)
	// Log at warn so a one-shot caller sees what it should act on (an ignored
	// WAXSEAL_UA_HINTS, a fallback-only token, the profile reaper) while a
	// clean run writes nothing to stderr.
	level := "warn"
	if g.verbose {
		level = "info"
	}
	logger := buildLogger(level, stderr)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	// Remove profiles left by a run that could not clean up after itself.
	browser.ReapStaleProfiles(logger)
	sess, err := browser.Launch(ctx, g.video, browser.Options{Headful: g.headful, NormalizeUA: !g.headful, Logger: logger})
	if err != nil {
		fmt.Fprintln(stdout, "{}")
		return err
	}
	defer sess.Close()

	res, err := sess.Mint(ctx, g.contentBinding)
	if err != nil {
		fmt.Fprintln(stdout, "{}")
		return err
	}
	expires := time.Now().Add(6 * time.Hour)
	if res.Lifetime > 0 {
		expires = time.Now().Add(time.Duration(res.Lifetime) * time.Second)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"poToken":        res.Token,
		"contentBinding": g.contentBinding,
		"expiresAt":      expires.UTC().Format(time.RFC3339),
	})
}

// maybeWarnURLBinding writes a one-line warning to w when binding looks like a
// pasted watch URL. content_binding is opaque (it may be visitor_data), so a
// URL is flagged, not rejected. Callers pass stderr: stdout is reserved for the
// bgutil {}/token contract.
func maybeWarnURLBinding(w io.Writer, binding string) {
	if msg, warn := browser.URLBindingWarningFor("content-binding", binding); warn {
		fmt.Fprintln(w, "waxseal: warning: "+msg)
	}
}
