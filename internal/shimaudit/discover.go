package shimaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/jsassets"
	"github.com/colespringer/waxseal/internal/jsruntime"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
)

// discoverWatchdog allows extra time for an autostub snapshot to run the VM to
// completion.
const discoverWatchdog = 15 * time.Second

// DiscoverResult contains the missing roots and full probe paths from one
// autostub discovery pass.
type DiscoverResult struct {
	Roots    []Root
	RawPaths []string
	// Tainted is always true because autostubbing changes VM behavior. Discovery
	// stops after the snapshot and never submits the result to GenerateIT.
	Tainted bool
}

// Discover fetches a challenge, runs the BotGuard VM snapshot with autostubbing
// enabled, and returns the APIs it probed but the shim lacks. It reads the
// structured probe set when possible and falls back to captured stderr if the
// runtime can no longer be queried.
//
// Autostubbing changes the snapshot, so the response is discarded and no token
// is produced.
func Discover(ctx context.Context, client *httpx.Client, userAgent string, ep botguard.Endpoint) (DiscoverResult, error) {
	stderr := &bytes.Buffer{}
	eng, err := quickjs.NewEngine(ctx, jsassets.QJSWasm, quickjs.Options{
		PreloadBundle: jsassets.BGBundle,
		Watchdog:      discoverWatchdog,
		Stderr:        stderr,
	})
	if err != nil {
		return DiscoverResult{}, fmt.Errorf("shimaudit: new engine: %w", err)
	}
	defer func() { _ = eng.Close(ctx) }()

	rt, err := eng.NewRuntime(ctx)
	if err != nil {
		return DiscoverResult{}, fmt.Errorf("shimaudit: new runtime: %w", err)
	}
	defer func() { _ = rt.Close(ctx) }()

	if _, err := rt.Eval(ctx, "globalThis.__wxDiscovery=true; globalThis.__wxAutoStub=true; globalThis.__wxClearProbes();"); err != nil {
		return DiscoverResult{}, fmt.Errorf("shimaudit: enable discovery: %w", err)
	}

	ch, err := botguard.FetchCreateChallenge(ctx, client, userAgent, ep)
	if err != nil {
		return DiscoverResult{}, fmt.Errorf("shimaudit: fetch challenge: %w", err)
	}

	// A snapshot error is non-fatal if the VM ran far enough to record probes.
	_, snapErr := botguard.Snapshot(ctx, rt, ch, nil)

	rawPaths := readProbes(ctx, rt)
	if len(rawPaths) == 0 {
		// Recover from captured stderr if the runtime can no longer be queried.
		rawPaths = botguard.DriftProbes(stderr.String())
	}
	if len(rawPaths) == 0 && snapErr != nil {
		return DiscoverResult{}, fmt.Errorf("shimaudit: discovery produced no probes: %w", snapErr)
	}

	return DiscoverResult{Roots: rootsFromPaths(rawPaths), RawPaths: rawPaths, Tainted: true}, nil
}

// readProbes returns nil when the runtime cannot provide its structured probes.
func readProbes(ctx context.Context, rt jsruntime.Runtime) []string {
	out, err := rt.Eval(ctx, "globalThis.__wxGetProbes()")
	if err != nil {
		return nil
	}
	var probes []string
	if err := json.Unmarshal(out, &probes); err != nil {
		return nil
	}
	return probes
}

// rootsFromPaths reduces nested paths to deduplicated first-segment roots:
// window.SpeechRecognition.start -> {window, SpeechRecognition}.
func rootsFromPaths(paths []string) []Root {
	seen := map[string]bool{}
	var roots []Root
	for _, p := range paths {
		t, name, ok := splitRoot(p)
		if !ok {
			continue
		}
		k := string(t) + "|" + name
		if seen[k] {
			continue
		}
		seen[k] = true
		roots = append(roots, Root{Target: t, Name: name})
	}
	return roots
}

// splitRoot extracts a target and first-segment name from a probe path. It
// removes the "new " prefix used for constructor probes.
func splitRoot(path string) (Target, string, bool) {
	p := strings.TrimPrefix(strings.TrimSpace(path), "new ")
	head, rest, ok := strings.Cut(p, ".")
	if !ok {
		return "", "", false
	}
	target := Target(head)
	if !ValidTarget(target) {
		return "", "", false
	}
	if i := strings.IndexAny(rest, ".([ "); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || len(rest) > MaxNameLen {
		return "", "", false
	}
	return target, rest, true
}
