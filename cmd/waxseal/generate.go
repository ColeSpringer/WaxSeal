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

// runGenerate launches a browser, attests, mints one token, and prints JSON on
// the last stdout line. On failure it prints "{}" to satisfy the bgutil
// script-provider contract, then returns the error for centralized reporting.
// One-shot mode launches a fresh browser for every call. Use `waxseal server`
// for yt-dlp.
func runGenerate(cmd *cobra.Command, g *genOpts) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if g.contentBinding == "" {
		fmt.Fprintln(stdout, "{}")
		return &usageError{msg: "content-binding (-c) is required"}
	}
	if len(g.contentBinding) > browser.MaxContentBindingBytes {
		// The bgutil script-provider contract requires {} on stdout for failures.
		fmt.Fprintln(stdout, "{}")
		return &usageError{msg: fmt.Sprintf("content-binding too long (max %d bytes)", browser.MaxContentBindingBytes)}
	}
	if browser.HasControlChars(g.contentBinding) {
		// Keep CLI validation aligned with /get_pot and preserve bgutil's
		// empty-object response for invalid input.
		fmt.Fprintln(stdout, "{}")
		return &usageError{msg: "content-binding must not contain control characters"}
	}
	// Preserve bgutil's empty-object failure response for invalid input.
	if err := validateLandingVideo(g.video); err != nil {
		fmt.Fprintln(stdout, "{}")
		return err
	}
	// content_binding is opaque, so a URL-shaped value is warned, not rejected.
	maybeWarnURLBinding(stderr, g.contentBinding)
	level := "error"
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
// pasted watch URL. content_binding is opaque and may legitimately be arbitrary
// (for example visitor_data), so a URL is flagged, not rejected. The warning goes
// to stderr, never stdout, because stdout stays reserved for the bgutil {}/token
// contract. It bypasses the logger because the CLI defaults to error level, where
// a logger.Warn would be silent for a normal `waxseal -c <url>`.
func maybeWarnURLBinding(w io.Writer, binding string) {
	if msg, warn := browser.URLBindingWarningFor("content-binding", binding); warn {
		fmt.Fprintln(w, "waxseal: warning: "+msg)
	}
}
