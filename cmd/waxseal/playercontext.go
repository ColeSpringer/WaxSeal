package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/spf13/cobra"
)

// playerContextOpts holds player-context-subcommand flags.
type playerContextOpts struct {
	video   string
	headful bool
	verbose bool
}

// newPlayerContextCmd is the one-shot counterpart to the daemon's
// /player-context endpoint. It launches a new browser for each call.
func newPlayerContextCmd() *cobra.Command {
	var o playerContextOpts
	c := &cobra.Command{
		Use:   "player-context [video_id]",
		Short: "Launch a browser, attest, and print a video's streaming context",
		Long: "Launch a real Chromium browser, attest, load the given video, and print its\n" +
			"streaming context as JSON. Pass the video ID as the positional argument or\n" +
			"with --video. The response includes the SABR URL, player URL, visitor data,\n" +
			"and audio formats. For a warm daemon, POST to /player-context instead.",
		Example: "  waxseal player-context aqz-KE-bpKQ\n" +
			"  waxseal player-context --video aqz-KE-bpKQ --headful",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runPlayerContext(cmd, &o, args) },
	}
	f := c.Flags()
	f.StringVar(&o.video, "video", "", "video ID to fetch (optional when passed as a positional argument)")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "verbose logging to stderr")
	return c
}

// resolvePlayerContextVideoID returns the video ID supplied by either the
// --video flag or the positional argument.
func resolvePlayerContextVideoID(flag string, args []string) (string, error) {
	video := flag
	if len(args) == 1 {
		if video != "" {
			return "", &usageError{msg: "specify the video ID once, either as the positional argument or with --video"}
		}
		video = args[0]
	}
	if video == "" {
		return "", &usageError{msg: "provide a video ID as the positional argument or with --video"}
	}
	if err := validateLandingVideo(video); err != nil {
		return "", err
	}
	return video, nil
}

func runPlayerContext(cmd *cobra.Command, o *playerContextOpts, args []string) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	video, err := resolvePlayerContextVideoID(o.video, args)
	if err != nil {
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
	// Remove profiles left by a run that could not clean up after itself.
	browser.ReapStaleProfiles(logger)
	// Establish the session on the stable default video before querying the target.
	// This lets an unavailable target return ErrUnplayable instead of timing out
	// during the streaming proof.
	sess, err := browser.Launch(ctx, browser.DefaultVideo, browser.Options{Headful: o.headful, NormalizeUA: !o.headful, Logger: logger})
	if err != nil {
		return fmt.Errorf("browser launch/identity: %w", err)
	}
	defer sess.Close()

	if err := sess.Attest(ctx); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	pc, err := sess.PlayerContext(ctx, video)
	if err != nil {
		return fmt.Errorf("player-context: %w", err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(pc)
}
