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

No open requests.
