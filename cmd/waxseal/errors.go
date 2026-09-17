package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/spf13/cobra"
)

// usageError marks invalid command-line input. It maps to exit code 2, matching
// WaxTap and allowing scripts to distinguish usage errors from runtime failures.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// renderError writes one error line with a single "waxseal: " prefix. Some
// internal errors already include the prefix, so embedded copies are removed
// before the CLI prefix is added.
func renderError(w io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(w, "waxseal: %s\n", strings.ReplaceAll(err.Error(), "waxseal: ", ""))
}

// exitCodeFor maps usage errors to 2, unavailable videos to 3, interrupts to 130,
// and all other failures to 1. A bot check is one of those others on purpose: it
// describes the browser session, not the video, so exit 3 would tell a script the
// wrong thing about a video that is fine.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return 130
	case errors.Is(err, browser.ErrUnplayable):
		return 3
	}
	if _, ok := errors.AsType[*usageError](err); ok {
		return 2
	}
	return 1
}

// wrapUsageErrors maps flag and argument errors to exit code 2 throughout the
// command tree. It also suppresses Cobra's error and usage output so renderError
// reports each failure once.
func wrapUsageErrors(cmd *cobra.Command) {
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{msg: err.Error()}
	})
	if inner := cmd.Args; inner != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			if err := inner(c, args); err != nil {
				return &usageError{msg: err.Error()}
			}
			return nil
		}
	}
	for _, sub := range cmd.Commands() {
		wrapUsageErrors(sub)
	}
}

// validateLandingVideo requires a bare video ID and returns a usage error for
// invalid input. A pasted watch link gets the targeted "not a URL" message
// instead of the generic charset error.
func validateLandingVideo(video string) error {
	switch {
	case browser.LooksLikeWatchURL(video):
		return &usageError{msg: "provide a bare video ID (for example, aqz-KE-bpKQ), not a URL"}
	case !browser.ValidVideoID(video):
		return &usageError{msg: "video ID must contain 1 to 64 letters, digits, underscores, or hyphens"}
	}
	return nil
}
