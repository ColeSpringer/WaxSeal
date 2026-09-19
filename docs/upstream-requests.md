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

- **The sidecar pause policy is unexported.** WaxTap took the retry rule as
  `SidecarRetryWait` (2026-09-19), so `provider/call` now asks WaxTap whether a
  refusal earns a retry and how long to wait. The other half of that decision,
  what to do when the caller's own budget cannot fit the wait, is
  `httpx.PauseBlocked` in `internal/httpx`, so `provider/call` still carries a
  copy of it: a cancellation outranks the pending refusal, a deadline that
  cannot fit the wait plus a second of headroom returns the refusal now, and the
  comparison refuses at equality. Wanted: that policy exported beside
  `SidecarRetryWait`, for example `PauseBlocked(ctx, wait, pending) error`, so
  one rule governs both adapters. Shipped workaround: the copy is aligned to
  WaxTap's, including the boundary, and two arms of
  `TestProviderRetriesOnceAfterStatedWait` bracket it, so a drift shows up in
  this repo's own tests. Opened 2026-09-19, when the retry half landed.
