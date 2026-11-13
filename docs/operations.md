# Operations

One Chromium process hosts an isolated incognito context per tenant. Startup
warms the tenant whose label sorts first; the others attest on their first
token, player-context, or session request.

## Startup

The daemon opens its port at once but serves nothing until its warm-up ends:
it attests one tenant, caches a GVS token, and runs a full-length streaming
proof, typically within ten seconds (the healthcheck allows two minutes). The
first token or context request may wait up to 12 seconds more behind the
mint-separation gate (see
[`/player-context`](api.md#get-post-player-context)).

A warm-up that YouTube refuses, or that cannot reach it, is logged and does not
stop the daemon. It serves, launching a session for a request that needs one
(see [Errors](api.md#errors)), and retries the launch in the background until a
session attests, doubling the wait from 15 seconds up to 30 minutes. The
streaming proof then runs with the first request that needs it. A warm-up that
fails in the browser itself twice running, or attests and cannot mint, stops
startup.

## Idle timeout

Chromium stays running between requests unless `--idle-timeout`
(`WAXSEAL_IDLE_TIMEOUT`, minimum `1m`) is set. Each tenant is then timed on its
own: its session closes once that tenant has gone that long without a request
that uses the browser, whatever the other tenants are doing. Chromium itself
exits once the last of them has. On Linux the daemon also releases the page
cache Chromium's files leave behind, so the container's reported memory falls
too, unless something else it can see is running the same browser; the next
launch reads those files back from disk. A close can land a quarter of the
timeout late, 30 seconds at most.

Use means a request that takes the browser: a token the cache does not hold, a
context, or a session export. `/ping`, `/metrics`, `/report`, cached tokens,
and requests refused inside a launch cool-down are not use, and cached tokens
are still served while the browser is closed.

The request that next needs the browser launches Chromium and attests a
session: about 4 seconds for a token, and about 20 for a context, which also
waits for the streaming proof and the mint-separation gate. The session is a
new identity, and it drops the tokens cached under the old one.

What that costs depends on the consumer. A context or a session export recycles
a session older than `--streaming-max-age` (45 minutes by default) anyway, so
with a timeout at or above that they pay only Chromium's launch on top, under a
second. A consumer that only asks for tokens never recycles that way: for it
every wake is the full 4 seconds, and the end of an identity and a token cache
it would otherwise have kept for up to 11 hours.

`/ping` answers an idle daemon with the benign `no-session` and
`browser_idle:true`, and never wakes it (see [Health checks](#health-checks)).
It cannot tell whether a browser would start: the request that wakes the daemon
finds out, and a launch that fails reads as it does at any other time.
`idle_closes` and `browser_idle_closes` in `/metrics` count the closes.

## Shutdown and cleanup

A normal teardown terminates Chromium's process group and removes its profile;
on a clean exit Chromium also quits when its CDP pipe closes. If the daemon dies
without teardown (SIGKILL, OOM), a browser may linger briefly: the next startup
removes abandoned profile directories it can prove unused, without killing
processes, and Chromium generally exits once its profile is gone. Profiles live
under `$HOME` so snap-confined Chromium can open them and they stay private on
shared hosts.

On SIGINT or SIGTERM the daemon closes its listener (so `/ping` fails from the
first signal), drains in-flight requests for up to `--shutdown-timeout`, tears
the browser down, and exits 0, as it also does when stopped during warm-up. A
second signal cuts the drain short. A stop while serving logs
`waxseal server stopped` last.

## Health checks

Health checks use `/ping`. With a tenant key, or with no key on a keyless daemon
(which selects its one tenant), it probes that tenant's session and returns HTTP
200 with `ok`, `probe:"tenant"`, `keyed` (whether the daemon requires a key at
all), and an always-present `reason`. A failed probe is confirmed by a second
one before anything is retired, so one transient CDP hiccup does not destroy a
warm session. The reasons are:

- `ok`: the session answered.
- `no-session`: benign. A `POST /report` retires the session and
  re-establishment is lazy, so `ok` briefly reads `false`. A tenant whose
  session launches fail (see [Errors](api.md#errors)) reads the same, whatever
  failed, with `consecutive_launch_failures` above 0, the failure in `error`,
  and `retry_after_seconds` while the cool-down is open. So does a session the
  [idle timeout](#idle-timeout) closed.
- `busy`: benign. The probe failed twice while a request held the page, so
  nothing was retired; the next probe re-checks once that request finishes.
- `probe-failed`: this probe confirmed a loss, logged at `warn`.

When no page answered, the probe also checks the shared Chromium, since a
wedged browser looks the same as a retired, busy, or missing page and would
stall the next request for its whole budget. A browser that answers, or had
exited and is relaunched on the spot (`browser_relaunched:true`), leaves the
tenant reason standing. So does one the idle timeout closed
(`browser_idle:true`), which the probe leaves closed. One that misses two
probes is torn down, replaced, counted in `browser_probe_failures`, and
reported as `probe-failed` with its error after the tenant's; one that cannot
be replaced is `probe-failed` with the launch error. A healthy probe skips this
check, costing one round trip.

On a keyed daemon, a `/ping` with no key gets that browser check alone instead
of `401`. The body says `probe:"daemon"` and carries only `ok` (a running
Chromium answered, possibly after a relaunch), `probe`, `keyed`, `reason`,
`browser_relaunched`, `browser_idle`, and on failure `error`. A browser the
idle timeout closed answers `ok:false` with `no-session`.

That reveals less than the redacted `/metrics`, so the check needs no key and
no loopback source. A caller cannot make a healthy browser fail it, so it can
only relaunch a browser that is wedged or gone, and relaunches are
single-flighted and backed off. A wrong key is still `401`, so a typo in a
probe's `--key` fails the probe rather than downgrading it.

Alert only on `probe-failed`; a caller that disconnects mid-probe never
produces one. A `consecutive_launch_failures` that keeps growing means
launches keep failing, and `error` says where: at YouTube, or in the browser.
For status-code-only checks (k8s, `curl -f`, HAProxy), `?strict=true` (or a
bare `?strict`) maps `probe-failed` to **503** and keeps `no-session`, `busy`,
and healthy at **200**; `waxseal ping --strict` does the same. A value
`strconv.ParseBool` cannot read (`yes`, `on`, `banana`) returns **400**.

Size a probe's timeout for the worst case: four session round trips, a session
teardown, and two browser round trips at up to 5 seconds each; a 7 second
browser teardown (a 2 second graceful close, then the launcher's 5 second
wait); and a relaunch, normally a second or two but bounded by the 60 second
launch handshake. That is 102 seconds, 105 on Windows. `waxseal ping` allows
108 (`--timeout` changes it) and the image's `HEALTHCHECK` 110.

The image runs `waxseal ping --strict` with no key: on a keyless daemon that
probes the one tenant's session, and on a keyed one the browser, so it keeps
working once keys are set. Add `--key <key>` to probe a tenant's session too.

## Browser identity

Headless Chromium puts a `HeadlessChrome` token in `navigator.userAgent` and its
brand list, so WaxSeal overrides both with `Chrome` and keeps the rest of the
browser's own `navigator.userAgentData`: the real GREASE brand, four-part build
version, platform, architecture, and bitness, which a fabricated block gets
wrong. `WAXSEAL_UA_HINTS=synthetic` switches to a fabricated block should the
real one ever grade worse; `real` is the default, and any other value is
ignored with a warning.

The image's Debian `chromium` reports no `Google Chrome` brand while its user
agent says `Chrome/<version>`; real Debian Chromium does the same, so it is not
a bug.

## CLI

Commands that take `--video` want a bare video ID, not a URL. The one-shot
`player-context` prints the endpoint's object minus `session_generation`, since
there is no daemon session.

`doctor --full` also verifies streaming past the roughly 70 second preview cap.
`--skip-attest` reports the identity without attesting, omitting the `attest`
key. `--stop-after-load` stops earlier, at the load event of a page it serves
itself on loopback, to show that Chromium renders and navigates with no
external network; `--landing-url` loads another page instead. Neither combines
with `--full`. `make docker-smoke` builds the image and runs
`doctor --stop-after-load` in it on an isolated network, as the release does.

On Windows, WaxSeal finds Chrome or Chromium under Program Files and the
per-user install directory (`WAXSEAL_CHROME_BIN` overrides), but never picks
Edge on its own, since Edge reports a different browser identity. Profiles live
under `%TEMP%`, and Ctrl-C stops the daemon as elsewhere. The container is
still the recommended deployment on every host.

Debug logging (`-v`) includes full video IDs and the `id`, `expire`, and `spc`
parameters of streaming URLs, which INFO lines leave out.

CLI exit codes: `0` success, `1` runtime failure, `2` usage error, `3`
unavailable video, `130` interruption of a one-shot command; `server` exits 0
on a requested stop. A bot check exits `1`, not `3`: it describes the browser
session, not the video.
