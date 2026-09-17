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

## Browser

- `[in-repo]` **A bot check is recognised by an English phrase.**
  `isBotCheck` (`internal/browser/browser.go`) matches "not a bot" in
  `playabilityStatus.reason`, which is localized user-facing text, and
  WaxSeal pins no browser locale or `hl`. A wall phrased in another
  language falls through to the per-video path: the video is
  negative-cached, the session is not relaunched, and the daemon looks
  like it is refusing every video in turn. The status cannot decide this
  instead, because a private video and an age gate share
  `LOGIN_REQUIRED` with the wall. Options when picked up: pin the
  browser's language (this changes the launch argv the goldens hold and
  the fingerprint, so it needs its own verification), or carry the known
  translations. Opened 2026-09-17, from a review of the bot-check change.

## Provider (the WaxTap adapter)

- `[upstream]` **`ProvidePlayerContext` drops the context's
  `user_agent`.** `/player-context` exports it (2026-09-16, answering
  WaxTap's ask) so a consumer streams under the identity the URL was
  issued to. `potoken.PlayerContext` has no field for it
  (upstream-requests.md, WaxTap: the context's user agent), so the
  adapter maps `ClientVersion` and drops the UA. When the field exists,
  map it and pin it in `provider_test.go`. Opened 2026-09-17 with the
  ask.
