package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/config"
	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/shimaudit"
	"github.com/spf13/cobra"
)

// newShimCmd assembles the offline coverage audit and online discovery commands.
func newShimCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "shim",
		Short: "Audit the browser shim against real Chrome",
		Long: "Audit the browser shim against the pinned Chrome reference:\n" +
			"  coverage  (offline) audit the committed shim against the pinned Chrome snapshot\n" +
			"  discover  (online)  run an autostub pass and merge newly observed missing roots",
		Args: cobra.NoArgs,
	}
	c.AddCommand(newShimCoverageCmd(), newShimDiscoverCmd())
	return c
}

// newShimCoverageCmd builds the offline coverage command.
func newShimCoverageCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "coverage",
		Short:         "Compare shim coverage with Chrome",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runShimCoverage(cmd) },
	}
}

func runShimCoverage(cmd *cobra.Command) error {
	stdout := cmd.OutOrStdout()
	fmt.Fprintln(stdout, "waxseal shim coverage", version)

	ref, err := shimaudit.EmbeddedReference()
	if err != nil {
		report(stdout, "reference", false, err.Error())
		return err
	}
	roots, err := shimaudit.EmbeddedRoots()
	if err != nil {
		report(stdout, "roots", false, err.Error())
		return err
	}
	ctx := cmd.Context()
	shim, err := shimaudit.ShimSurface(ctx)
	if err != nil {
		report(stdout, "shim surface", false, err.Error())
		return err
	}
	bare, err := shimaudit.BareRuntimeSurface(ctx)
	if err != nil {
		report(stdout, "bare surface", false, err.Error())
		return err
	}

	reportLine(stdout, "ok", "reference", fmt.Sprintf("%s %s (window=%d navigator=%d document=%d)",
		ref.Meta.Source, ref.Meta.FullVersion, len(ref.Window), len(ref.Navigator), len(ref.Document)))
	reportLine(stdout, "ok", "shim surface", fmt.Sprintf("window=%d navigator=%d document=%d (roots tracked=%d)",
		len(shim.Window), len(shim.Navigator), len(shim.Document), len(roots.Roots)))

	rep := shimaudit.Audit(ref.Surface, shim, bare, roots.Roots)

	// Always print gated findings with their placement hints.
	if n := len(rep.MissingProbedReal); n == 0 {
		report(stdout, "gated coverage", true, "no probed Chrome APIs missing")
	} else {
		reportLine(stdout, "FAIL", "gated coverage", fmt.Sprintf("%d probed Chrome API(s) missing; add to build/js/dom.js, then make jsbundle", n))
		for _, f := range rep.MissingProbedReal {
			reportLine(stdout, "add", string(f.Target)+"."+f.Name, shimaudit.PlacementHint(f.Shape))
		}
	}

	// MissingReal and OverCoverage are not useful against an incomplete seed
	// reference, so suppress their entries until a browser capture is available.
	if ref.IsCapture() {
		printAdvisory(stdout, "missing real", rep.MissingReal)
		printAdvisory(stdout, "over-coverage", rep.OverCoverage)
	} else {
		reportLine(stdout, "info", "advisory buckets",
			"suppressed against a seed reference; refresh with make chrome-globals on Windows for the full comparison")
	}
	printAdvisory(stdout, "absent from reference", rep.AbsentFromReference)

	if len(rep.MissingProbedReal) > 0 {
		return errors.New("shim coverage: gated APIs missing")
	}
	return nil
}

// printAdvisory prints an advisory count followed by its findings.
func printAdvisory(w io.Writer, label string, findings []shimaudit.Finding) {
	reportLine(w, "info", label, fmt.Sprintf("%d", len(findings)))
	for _, f := range findings {
		reportLine(w, "·", string(f.Target)+"."+f.Name, shimaudit.PlacementHint(f.Shape))
	}
}

// newShimDiscoverCmd builds the online autostub discovery command.
func newShimDiscoverCmd() *cobra.Command {
	var (
		configPath string
		proxy      string
		mergePath  string
	)
	c := &cobra.Command{
		Use:   "discover",
		Short: "Online autostub discovery of missing APIs",
		Long: "Fetch a challenge, run one autostub pass of the BotGuard VM, and merge the APIs\n" +
			"it probes but the shim lacks into the roots fixture. The modified VM result is\n" +
			"discarded and no token is emitted.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runShimDiscover(cmd, configPath, proxy, mergePath)
		},
	}
	f := c.Flags()
	f.StringVar(&configPath, "config", "", "path to a JSON config file")
	f.StringVar(&proxy, "proxy", "", "egress proxy URL")
	f.StringVar(&mergePath, "merge", "", "path to the observed-missing-roots fixture to update (required)")
	return c
}

func runShimDiscover(cmd *cobra.Command, configPath, proxy, mergePath string) error {
	stdout := cmd.OutOrStdout()
	fmt.Fprintln(stdout, "waxseal shim discover", version)

	if mergePath == "" {
		err := errors.New("--merge PATH is required (the explicit roots fixture to read-modify-write)")
		report(stdout, "merge", false, err.Error())
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		report(stdout, "config", false, err.Error())
		return err
	}
	if proxy != "" {
		cfg.Proxy = proxy
	}
	ep, err := botguard.ResolveEndpoint(cfg.EndpointMode)
	if err != nil {
		report(stdout, "endpoint", false, err.Error())
		return err
	}
	client, err := discoverClient(cfg.Proxy)
	if err != nil {
		report(stdout, "init", false, err.Error())
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()

	res, err := shimaudit.Discover(ctx, client, waxseal.DefaultProfile().UserAgent, ep)
	if err != nil {
		report(stdout, "discover", false, err.Error())
		return err
	}
	report(stdout, "discover", true, fmt.Sprintf("probed %d missing path(s); %d root(s)", len(res.RawPaths), len(res.Roots)))
	if res.Tainted {
		reportLine(stdout, "warn", "modified run", "autostubbing changed VM behavior; snapshot and response discarded; no token emitted")
	}

	// Merge into the requested path, starting with an empty fixture if needed.
	prev := shimaudit.RootsFile{SchemaVersion: shimaudit.SchemaVersion}
	if b, rerr := os.ReadFile(mergePath); rerr == nil {
		if prev, err = shimaudit.LoadRoots(b); err != nil {
			report(stdout, "merge", false, fmt.Sprintf("parse %s: %v", mergePath, err))
			return err
		}
	} else if !errors.Is(rerr, os.ErrNotExist) {
		report(stdout, "merge", false, rerr.Error())
		return rerr
	}

	beforeRoots, beforeRaw := len(prev.Roots), len(prev.RawPaths)
	merged, warnings := shimaudit.MergeRoots(prev, res.Roots, res.RawPaths, time.Now())
	out, err := merged.Marshal()
	if err != nil {
		report(stdout, "merge", false, err.Error())
		return err
	}
	if err := os.WriteFile(mergePath, out, 0o644); err != nil {
		report(stdout, "merge", false, err.Error())
		return err
	}
	report(stdout, "merge", true, fmt.Sprintf("wrote %s (roots %d->%d, rawPaths %d->%d)",
		mergePath, beforeRoots, len(merged.Roots), beforeRaw, len(merged.RawPaths)))
	for _, w := range warnings {
		reportLine(stdout, "warn", "merge cap", w)
	}
	return nil
}

// discoverClient builds the HTTP client used to fetch a discovery challenge.
func discoverClient(proxy string) (*httpx.Client, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	if proxy != "" {
		pu, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", proxy, err)
		}
		hc.Transport = &http.Transport{Proxy: http.ProxyURL(pu)}
	}
	return httpx.New(hc), nil
}
