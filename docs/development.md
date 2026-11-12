# Development

```sh
go test ./...                              # offline unit tests; no browser or network
(cd provider && go test ./...)             # the nested module's offline tests
go test -tags live ./internal/cdp          # real-Chromium CDP pipe-transport tests
(cd provider && go test -tags live ./...)  # the nested module's test that needs Chromium but no network
(cd provider && go test -tags e2e ./...)   # provider network e2e; needs Chromium and network access
make vet                                   # both modules, plus a windows cross-vet (CI vets Windows natively)
make fmt-check                             # fail if any file needs gofmt
make test                                  # both modules' offline tests, race-enabled
make live                                  # both live lines above
make tidy-check                            # fail if go mod tidy would change either module
make vulncheck                             # govulncheck over both modules
make consumer-check                        # build provider/ as an external go get sees it
make compose-check                         # validate compose.yaml and check it pulls the published image
make verify-assets                         # rebuild the browser bundle and diff it against the committed one
make deps                                  # install browser-bundle build dependencies
make jsbundle-browser                      # regenerate internal/browser/bg_browser_bundle.js
```

`go test ./...` is offline and deterministic: no browser, no network. It does
not reach the separate `provider/` module, whose own offline tests need the
second line; `make test` runs both. The committed browser bundle means builds
need no Node. The live tests skip when no browser is found;
`WAXSEAL_CHROME_BIN` picks one, and `WAXSEAL_REQUIRE_CHROME=1` (set in CI) fails
instead of skipping.

The `e2e` tests must run from `provider/`, since a root-level
`go test -tags e2e ./...` silently runs nothing. Each starts its own daemon
unless `WAXSEAL_URL` names a running one (`WAXSEAL_KEY` if it is keyed). They
include the full-length WEB SABR download
(`go test -tags e2e -run PlayerContextOnlyFullLength ./...`).
`WAXSEAL_E2E_LOG_LEVEL=debug` adds the in-process daemon's debug logs to
`go test -v` output. `WAXSEAL_E2E_MULTITRACK_VIDEO=<id>` names a multi-track
video for the multitrack subtest of `TestPlayerContextFieldsHTTP`, which skips
without one.

`TestAgingMatrix` is an opt-in measurement of how an artifact's age affects a
capped stream, not a regression test. It runs for tens of minutes, reports a
tally rather than a pass/fail on truncation, and skips unless
`WAXSEAL_E2E_AGING` selects a matrix:

- `1`: which artifact's age predicts a truncated stream.
- `2`: how much separation between a mint and a served context is enough.
- `3`: the same question for the distance between the proof playback and the
  served context.

`WAXSEAL_E2E_AGING_N` sets the iterations per arm (default 6), and
`WAXSEAL_E2E_AGING_DELAY` the run-wide delay between warming and streaming
(default `30s`; an arm with its own delay ignores it). Every in-process daemon
keeps its mint-to-establishment gate (12s unless `WAXSEAL_MINT_SEPARATION`
overrides it), so a default run is a regression check: every arm should stream
full length.

`WAXSEAL_E2E_AGING_SEPARATION` (a Go duration such as `1ms`) overrides that gate
so the arms measure raw gaps; since attestation pre-mints the token, the
token-age arms then measure time since attestation. Matrix `3` refuses it,
because each of its arms sets the gate to its own gap, and its records carry an
`anchor=` column from the daemon's log proving each iteration waited on the
proof, not a mint.

The `client` package is a reusable, consumer-agnostic HTTP client; the
`provider/` module adapts it to the token-provider interface a streaming
consumer expects.

`provider/` builds against this checkout through a `replace`, which a consumer
ignores: it resolves the root version the `require` names. So push a root
change that `provider/` depends on first, then pin it:
`go list -m github.com/colespringer/waxseal@<sha>` prints the pseudo-version,
and from `provider/` run `go mod edit
-require=github.com/colespringer/waxseal@<pseudo-version>` and `go mod tidy`.
After each root tag, move the pin to the tag. `make consumer-check`, part of the
release gate, builds the module without the `replace`, so a stale pin stops a
release rather than a consumer.

Some coverage stays out of `go test ./...` because it needs a display or a long
run: **headful mode** (`go run ./cmd/waxseal server --headful`) to watch a real
session, a **time-based recycling soak** (a short `--streaming-max-age` with
continuous streaming to watch `streaming_seconds_until_recycle`), and a
**cache-exhaustion loop** (`POST /get_pot` 1000+ times with distinct
`content_binding` values to exercise cache eviction).

Work cut from a change is tracked in [deferred-work.md](deferred-work.md), and
what WaxSeal wants from the sibling repos it depends on in
[upstream-requests.md](upstream-requests.md).
