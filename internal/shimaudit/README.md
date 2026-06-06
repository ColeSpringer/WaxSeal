# Shim coverage audit

`shimaudit` compares WaxSeal's browser shim (`build/js/shim.js` and
`build/js/dom.js`) with a version-pinned Chrome globals snapshot. The audit runs
offline and covers the `window`, `navigator`, and `document` surfaces.

| Bucket | Meaning | Gated? |
| --- | --- | --- |
| **MissingProbedReal** | Observed missing roots present in the reference but absent from the shim | Yes; CI fails |
| **MissingReal** | Other reference names absent from the shim | No (advisory) |
| **OverCoverage** | In the shim (minus the bare engine), not in Chrome | No (advisory) |
| **AbsentFromReference** | Probed roots not in the reference | No (advisory) |

The initial gate checks only whether a name exists. The snapshot also records
constructability, prototype parents, and alias identity, but those fields are
used only for placement hints. The audit does not modify the shim.

## Daily workflow (offline)

```sh
go run ./cmd/waxseal shim coverage
```

This enumerates the committed shim in QuickJS, subtracts a bare QuickJS baseline,
and compares the result with the embedded fixtures. Each gated miss includes a
placement hint derived from the snapshot.

1. Add the name to the appropriate battery or alias map in `build/js/dom.js`, or
   to `build/js/shim.js` for a `navigator` or value member.
2. `make jsbundle` (regenerates the committed `internal/jsassets/bg_bundle.js`).
3. Re-run `go run ./cmd/waxseal shim coverage` and
   `go test ./internal/shimaudit/...`.

Review `OverCoverage` as well. Before removing a name from `dom.js`, verify that
it is not gated by origin or browser flags that differ from the clean
`https://localhost` capture.

`constructable` cannot distinguish a real constructor from an illegal-constructor
interface object (both have `[[Construct]]`); the WPT/IDL decides between the
constructible and no-constructor paths. Use the snapshot, rather than the IDL,
to decide which names Chrome exposes.

## Refreshing the Chrome reference

The reference fixture (`fixtures/chrome_windows_desktop_globals.json`) is
captured data, not a build output. It is excluded from `verify-assets`. Refresh
it on Windows with the exact Chrome for Testing version from
`chrome_version.json`.

```sh
npx @puppeteer/browsers install chrome@<fullVersion>

set WX_CHROME_EXECUTABLE=C:\path\to\chrome.exe
make chrome-globals
```

It must run on native Windows (`process.platform === 'win32'`); the gate enforces
`meta.os == "windows"` because the shim reports Win32. WSL reports `linux`, so
the script rejects captures made inside WSL.

Review the JSON diff before committing it. The script sorts the fixture for
stable diffs.

See `CHROME-GLOBALS-CAPTURE.md` at the repository root for the full procedure.

## Update observed missing roots

```sh
go run ./cmd/waxseal shim discover --merge internal/shimaudit/fixtures/observed_missing_roots.json
```

This runs one autostub pass of the BotGuard VM and merges the missing API roots
into the fixture. The run is discarded and emits no token because autostubbing
changes VM behavior. Roots are never removed, and the merge path is explicit so
the command does not assume a repository layout.

## Files

- `audit.go`: the pure `Audit` function and report buckets.
- `surface.go`: QuickJS enumeration through `ShimSurface` and
  `BareRuntimeSurface`.
- `reference.go`: fixture schemas, embedding, and monotonic merge logic.
- `discover.go`: the online autostub runner.
- `enumerate.js`: the descriptor-based enumerator shared with
  `build/js/capture-globals.mjs`.
- `gate_test.go`: `TestShimCoverageGate`, `TestChromeReferenceMatchesProfile`,
  `TestFixturesWellFormed`.
- `fixtures/`: the committed Chrome snapshot and observed missing roots.

`doctor` continues to report post-mint drift in production without autostubbing.
