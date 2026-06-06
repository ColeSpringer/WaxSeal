package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Subcommands print their own user-facing errors (generate emits "{}" on
		// stdout; server/doctor log to stderr). Here we only set the exit code.
		os.Exit(1)
	}
}

// newRootCmd assembles the command tree. The root itself runs generate mode, so
// `waxseal -c <binding>` works with no subcommand; server, doctor, and the
// get-pot alias are added as subcommands.
func newRootCmd() *cobra.Command {
	var g genOpts
	root := &cobra.Command{
		Use:   "waxseal",
		Short: "Native-Go YouTube PO Token (POT) provider",
		Long: "WaxSeal mints YouTube PO tokens by running the BotGuard VM in QuickJS-on-wazero.\n" +
			"With no subcommand it runs generate mode, compatible with bgutil's script provider:\n" +
			"it prints the token as JSON on the last stdout line, or {} and a nonzero exit on failure.",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runGenerate(cmd, &g) },
	}
	bindGenerateFlags(root, &g)
	root.AddCommand(newServerCmd(), newDoctorCmd(), newGetPotCmd(), newPingCmd(), newShimCmd())
	return root
}
