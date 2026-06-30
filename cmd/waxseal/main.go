// Command waxseal mints YouTube PO tokens from a real headless Chromium. With no
// subcommand, it runs the bgutil-compatible one-shot generation mode. Use
// `waxseal server` when callers need a warm browser.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(execute(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// execute runs the root command and returns its process exit code. The stdout and
// stderr parameters let tests inspect output without spawning a process.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		renderError(stderr, err)
		return exitCodeFor(err)
	}
	return 0
}

// newRootCmd assembles the command tree. The root runs generate mode, so
// `waxseal -c <binding>` works with no subcommand.
func newRootCmd() *cobra.Command {
	var g genOpts
	root := &cobra.Command{
		Use:   "waxseal",
		Short: "Real-browser YouTube PO Token (POT) provider",
		Long: "WaxSeal mints YouTube PO tokens from a real headless Chromium.\n" +
			"With no subcommand, it runs the bgutil-compatible one-shot mode and prints\n" +
			"the token as JSON on the last stdout line. Failures print {} and exit\n" +
			"nonzero. For yt-dlp, prefer `waxseal server`.",
		Version: version,
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runGenerate(cmd, &g) },
	}
	bindGenerateFlags(root, &g)
	root.AddCommand(newServerCmd(), newDoctorCmd(), newGetPotCmd(), newPingCmd(), newPlayerContextCmd())
	// Cobra normally creates these commands during Execute. Initialize them before
	// wrapping validators so their usage errors also exit with code 2.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	hardenHelpCompletionExitCodes(root)
	// Apply shared error handling after building the complete command tree.
	wrapUsageErrors(root)
	return root
}

// hardenHelpCompletionExitCodes brings Cobra's generated help and completion
// commands under the same usage-error policy as the rest of the CLI. Cobra builds
// them outside our command constructors, so we locate them by name after the
// default commands are initialized and before wrapUsageErrors runs.
func hardenHelpCompletionExitCodes(root *cobra.Command) {
	for _, c := range root.Commands() {
		switch c.Name() {
		case "help":
			// `help` is runnable, so Cobra validates Args. Bare help and real command
			// topics pass; unknown topics or extra words become usage errors.
			c.Args = func(cmd *cobra.Command, args []string) error {
				if len(args) == 0 {
					return nil
				}
				_, remaining, _ := cmd.Root().Find(args)
				if len(remaining) > 0 {
					return fmt.Errorf("unknown help topic %q", strings.Join(args, " "))
				}
				return nil
			}
		case "completion":
			// The completion parent owns Cobra's NoArgs validator, but without Run it
			// prints help and exits 0 before validation. Making it runnable routes stray
			// words through NoArgs.
			c.Run = func(cmd *cobra.Command, _ []string) { _ = cmd.Help() }
		}
	}
}

// buildLogger builds a slog logger at the given level, writing to w (stderr for
// the CLI, stdout for the daemon).
func buildLogger(level string, w io.Writer) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))
}
