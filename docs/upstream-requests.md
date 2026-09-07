# Upstream requests

The standing list of things WaxSeal wants from the sibling Wax repos it
depends on. Only wax-series dependencies belong here, and today that is
WaxTap alone: the root module is WaxTap-free by design, `provider/go.mod`
is the one place a sibling is required, and WaxFlow and WaxLabel arrive
there only as WaxTap's transitive requirements, with no WaxSeal code
calling either. Every entry is a candidate for whenever upstream work is
next scheduled; nothing here implies timing, and none of it blocks
WaxSeal, since each entry names the workaround WaxSeal ships today and,
where one exists, the test that will notice the upstream change landing.
Agents: when you defer something because it needs upstream support, add
it here in the same change and put the WaxSeal-side follow-up in
[deferred-work.md](deferred-work.md); when upstream lands it, do the
follow-up and remove both entries.

## WaxTap

- **A sidecar error's `code` is discarded.** WaxSeal answers every
  failure with `{"error","code"}`, and the code carries the consumer's
  decision: `video-unavailable` (422) is terminal for the video, while
  `player-context-failed` (502) during the 30 s proof cool-down and
  `no-session` (503) in the re-establishment window are safe to retry.
  `sidecarReason` (`sidecar.go`) keeps only the `error` or `message`
  text, `SidecarResponseError` carries the status and that text, the CLI
  classifies by status alone (`sidecarResponseExit`: a 4xx other than
  408 and 429 exits 2, a 5xx exits 9), and the library wraps the error in
  `waxerr.ProviderError` and falls back down the client chain before
  delivery. So a 422 is never mapped to `ErrVideoUnavailable`: the chain
  learns the availability again from its own `/player`, and a chain with
  no fallback exits 2. And a 502 or 503 is a provider failure like any
  other: WaxTap leaves the WEB path for that download (the adopted
  session, then a non-WEB client) instead of waiting out a cool-down the
  daemon lifts on its own, and the WEB stream is what the sidecar exists
  to provide. Wanted: a `Code` on `SidecarResponseError` read from the
  JSON, `video-unavailable` mapped to `ErrVideoUnavailable` (skip-class,
  no fallback needed), and one retry after the cool-down (or after a
  `Retry-After`, once WaxSeal sends one; see deferred-work.md) for
  `player-context-failed` and `no-session` before the fallback. Shipped
  workaround: the README documents both refusals, and a library consumer
  that embeds WaxSeal's own `client` and `provider/` packages sees
  `*client.APIError` with the code; one on the sidecar path sees the
  text.

- **`potoken.Session` cannot carry the attested identity's user agent or
  client version.** `/session` exports `user_agent` and `client_version`
  beside `visitor_data` and the cookies so that attestation, token
  binding, and the download run under one browser identity, and
  `client.Session` carries both. `potoken.Session` has `VisitorData`,
  `Cookies`, and `Generation` only, and WaxTap's `/session` sidecar
  documents `user_agent`, `client_version`, and `cookie_header` as
  ignored, so an adopted session streams under WaxTap's own Chrome user
  agent (`clientident`) and its pinned WEB client version rather than the
  identity that attested the token and issued the cookies, where a player
  context's `ClientVersion` is already applied through
  `webContextProfile`. Wanted: `UserAgent` and `ClientVersion` on
  `potoken.Session` (empty meaning WaxTap's own), read by the sidecar and
  applied to the adopted WEB profile the way the context's version is.
  Shipped workaround: none is needed today. The session arm streams full
  length under WaxTap's own profile (`provider` e2e
  `TestSessionOnlyFullLengthHTTP`, 3 of 3 on 2026-09-02) and WaxTap keeps
  its Chrome major current, so this is a coherence gap rather than an
  observed failure. `provider.Session` drops the two fields; mapping them
  is the WaxSeal-side follow-up in deferred-work.md.

- **The sidecar clients' timeouts are fixed, or default below WaxSeal's
  documented first-call cost.** The `/get_pot` and `/session` sidecar
  clients use a fixed 30 s `http.Client` timeout with no `SidecarOption`
  to change it; `/player-context` has no client timeout and relies on
  `Timeouts.WebContext`, which the CLI defaults to 20 s
  (`cmd/waxtap/config.go`) and the library leaves unbounded at zero. On
  the WaxSeal side, the first `/player-context` on a browser session
  waits for the streaming proof and then the 12 s separation window
  (about 15 s on a warm session), `/session` runs the proof inline when
  the session is unproven, and after a relaunch (a crash, a report-driven
  retirement, `--streaming-max-age`) either call first pays the launch,
  attestation, and proof that startup spends 10 to 30 s on. So the CLI's
  20 s can expire on the first context after a relaunch, the fixed 30 s
  sits at the top of the documented range for `/session`, and a call that
  times out on the consumer's side has spent that wait for nothing.
  Wanted: a `SidecarOption` for the token and session clients' timeout,
  and a CLI web-context default that covers a proof plus the separation
  window after a relaunch, or the session sidecar honouring
  `Timeouts.WebContext` as the context sidecar does. Shipped workaround:
  the startup self-test proves the session before the daemon accepts
  traffic, so the common first call is fast; library callers such as the
  `provider` e2e harness set no web-context bound; CLI users raise
  `WAXTAP_WEB_CONTEXT_TIMEOUT`. No timeout failure has been observed; the
  entry records a mismatch between the two repos' documented numbers.
