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

- `[upstream]` **`provider.ProvidePlayerContext` drops the metadata
  `/player-context` now sends.** The endpoint carries `channel_id`,
  `description`, the `thumbnails` ladder, `is_live_content`,
  `is_live_now`, `is_upcoming`, and `publish_date` beside the title,
  author, and length (2026-09-16, answering WaxTap's ask), and a sidecar
  consumer reads them straight off the JSON. The Go adapter cannot pass
  them on: `potoken.PlayerContext` has fields for the title, author, and
  length only (upstream-requests.md, WaxTap: the web-context metadata
  fields), so `ProvidePlayerContext` maps those three and discards the
  rest. When the upstream type grows the fields, map them and pin the
  mapping in `provider_test.go`. Opened 2026-09-16 with the ask.

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
