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

- **`potoken.PlayerContext` cannot carry the context's user agent.**
  `/player-context` now sends `user_agent`, the session identity the
  context was minted under and the same value `/session` exports,
  answering WaxTap's own ask for it (its `docs/upstream-requests.md`,
  2026-09-16). `potoken.PlayerContext` has `ClientVersion` only and the
  sidecar's `playerContextResponse` reads no `user_agent`, so the context
  arm streams under WaxTap's own user agent with the context's client
  version, which is the same coherence gap the session arm had before
  `potoken.Session` grew the pair. Wanted: `UserAgent` on
  `potoken.PlayerContext`, read by the sidecar and applied through
  `webContextProfile` the way the session's is. Shipped workaround: none
  is needed. Delivery is full length under WaxTap's identity, as WaxTap's
  own ask says, so this is coherence rather than an observed failure. The
  test that will notice the field landing is the mapping pin in
  `provider_test.go`. Opened 2026-09-17.

- **`SidecarResponseError` cannot carry the cause, and the retry rule is
  unexported.** `provider/` translates a `*client.APIError` into a
  `*waxtap.SidecarResponseError` so WaxTap classifies and waits exactly
  as it does for its own sidecar. Two things follow from the types.
  `SidecarResponseError` has no field for an underlying error and its
  `Unwrap` is reserved for the playability verdict, so the original
  `*client.APIError` cannot travel with it: a consumer that reaches
  through `provider/` for `errors.AsType[*client.APIError]` no longer
  finds one, and has to use the WaxSeal `client` package directly for
  that. And `sidecarCall`, `sidecarRetryWait`, and
  `retryableSidecarStatus` are unexported, so the one-retry rule is
  copied into `provider/call` rather than shared, and the two can drift
  apart silently. Wanted: a `Cause error` on `SidecarResponseError`
  (reported, not unwrapped, so the verdict `Unwrap` is unchanged), and
  the retry rule exported in some form, for example a
  `RetryAfterFor(err) (time.Duration, bool)`. Shipped workaround: the
  copy mirrors WaxTap `9a53a55` line for line and
  `TestProviderRetriesOnceAfterStatedWait` pins every arm of it, so a
  drift is at least visible in this repo's own tests. Opened 2026-09-17,
  from a review of the adapter change.
