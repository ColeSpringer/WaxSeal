# Deferred work

The tracked list of WaxSeal work that was cut from an otherwise shipped
change, or that waits on a sibling repo. Work that never started does not
belong here, and the reasoning behind something deliberately not built
belongs in the doc comment beside the code it constrains; this list is
for the residuals that would otherwise survive only as a sentence in a
plan or a progress note. Agents: when you cut something, add it here in
the same change; when it lands, remove the entry. Asks of the sibling
repos live in [upstream-requests.md](upstream-requests.md), and an
`[upstream]` entry here names the ask it waits on.

Gate tags:

- `[in-repo]` nothing blocks it; it was cut for scope and is ours to
  build when picked up.
- `[upstream]` needs sibling-repo work first; the ask is in
  upstream-requests.md.

## HTTP API

- `[in-repo]` **`/player-context` carries only a title, an author, and a
  length.** Asked by WaxTap (its `docs/upstream-requests.md`,
  2026-09-06): the `Video` it builds on the WEB-context path has no
  channel ID, description, thumbnail ladder, publish date, or live
  state, where every other path fills them from the same `videoDetails`,
  so `Result.Metadata` and `--write-info-json` on that path alone come
  back without them. `playerContextExtractJS`
  (`internal/browser/browser.go`) reads `title`, `author`, and
  `lengthSeconds` from the player's own `getPlayerResponse()`; the same
  object carries `channelId`, `shortDescription`, the
  `thumbnail.thumbnails` ladder, and `isLiveContent` with the live and
  upcoming flags, and its `microformat` carries `publishDate` when the
  embedded player's response includes one. Work: extend the extraction,
  `browser.PlayerContext`, and `client.PlayerContext` (the JSON tags stay
  in sync and the README's shape is authoritative), document the fields
  under `/player-context`, and map them in `provider.ProvidePlayerContext`
  once `potoken.PlayerContext` grows matching fields. WaxTap's own sidecar
  reads the JSON directly, so the endpoint half waits on nothing.

- `[upstream]` **The retryable refusals carry no `Retry-After`.**
  `player-context-failed` (502) during the 30 s proof cool-down
  (`proofRetryCooldown`, `internal/minter/minter.go`) and `no-session`
  (503) in the lazy re-establishment window are documented as safe to
  retry, but only `/report`'s rate limit sends `Retry-After` and
  `retry_after_seconds` (`server/server.go`); the two refusals name the
  cause in `error` text alone. A consumer cannot honour a wait it is not
  told about, which is half of the sidecar error-code ask
  (upstream-requests.md, WaxTap). When that ask is taken up, send the
  remaining cool-down (the minter already computes it for its log line)
  on both refusals and document it under Errors. On its own the header
  would go to a consumer that ignores it, which is why this waits.
  Opened 2026-09-06 with the ask.

## Provider (the WaxTap adapter)

- `[upstream]` **`Session` drops the exported user agent and client
  version.** `provider.Session` builds a `potoken.Session` from
  `VisitorData`, `Cookies`, and `SessionGeneration` because the WaxTap
  type has no field for `client.Session.UserAgent` or `ClientVersion`
  (upstream-requests.md, WaxTap: the adopted session's identity fields).
  When the fields exist, map both and pin the mapping in
  `provider_test.go`. Opened 2026-09-06 with the ask.

## Platforms and packaging

- `[in-repo]` **The image is linux/amd64 only.** `release.yml` builds and
  pushes one platform and says so in its own comment ("add buildx for
  multi-arch later"), and `make docker-build` is a plain `docker build`
  for the host. The release binaries cover linux and darwin on amd64 and
  arm64, so an arm64 Docker host (Apple silicon, a Raspberry Pi) gets the
  amd64 image under emulation or nothing. Work: a buildx multi-platform
  build in `release.yml` and the Makefile, with the `--network none`
  `doctor --stop-after-load` smoke test run per platform. Debian ships
  `chromium` for arm64, so the Dockerfile should need no change beyond
  the build itself.

- `[in-repo]` **The daemon does not run on Windows.**
  `internal/cdp/proc_windows.go` sets `platformSupported` to false
  because `exec.Cmd.ExtraFiles` cannot hand Chromium the
  `--remote-debugging-pipe` descriptors, so `Spawn` refuses to launch;
  `internal/browser/proc_windows.go` has no advisory profile lock or
  reaper marker lock either. Windows left the release and CI cross
  matrices in July 2026 (`00ea316`), and CI vets `GOOS=windows` so the
  stubs cannot rot. Work if taken up: a transport that passes Chromium
  inheritable pipe handles the way the Unix descriptors are passed today
  (or a loopback WebSocket fallback), the profile and marker locks, and
  the platform back in both matrices. The container and WSL2 cover a
  Windows host meanwhile.

## Tests and measurements

- `[in-repo]` **`internal/browser` has no page fake.** The establish,
  confirm, proof, and identity-capture paths run only against a real
  Chromium (the `provider` e2e suite; `-tags live` covers `internal/cdp`
  alone), so the identity-capture grace branch added on 2026-09-02
  shipped with no unit coverage and every change to those paths is
  verified by network runs. The seam was designed during the September
  remediation (a narrow `pageDriver` interface over `Eval`, an adapter
  for `*cdp.Page`, and a poll-interval accessor so offline tests run in
  milliseconds) as part of a request-observer fix that measurement then
  refuted, so it was never built.

- `[in-repo]` **The proof-to-context edge is bracketed, not pinned.** The
  aging matrix that set the 12 s separation window
  (`provider/e2e_aging_test.go`, `WAXSEAL_E2E_AGING=2`) saw the mint
  anchor fail at 0.6 s (0 of 6 full) and pass at 10.6 s (6 of 6), and the
  proof anchor fail at 3 s (0 of 4) and pass at 12 s (4 of 4); nothing
  between either pair was run. The window is margin over the mint edge
  and an untested margin over the proof edge. If truncations return with
  `separation_waits` logging `after=proof`, run the matrix at
  intermediate delays before moving `WAXSEAL_MINT_SEPARATION`'s default.
