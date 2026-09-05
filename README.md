# WaxSeal

WaxSeal is a YouTube **PO Token (POT)** provider that runs Google's BotGuard in
a real headless Chromium, driven over the Chrome DevTools Protocol by a
repository-local standard-library client. It ships a bgutil-compatible HTTP
daemon, a CLI, and reusable Go clients.

A real browser lets BotGuard inspect the actual navigator and reliably produce
tokens with the **integrity** grade.

> The container image bundles Chromium. To run the Go binary directly instead,
> the host needs a system Chromium (auto-detected; set `WAXSEAL_CHROME_BIN` to
> override), since the binary is not self-contained.

## Quick start

WaxSeal usually runs as a container (example compose file found in repo), and
the published image bundles Chromium, so the host needs only Docker:

```sh
docker compose up -d  # pulls ghcr.io/colespringer/waxseal and starts on 127.0.0.1:4416
```

That pulls the `:latest` tag; pin a release with `WAXSEAL_VERSION`, for example
`WAXSEAL_VERSION=1.0.0 docker compose up -d`. To build the image from source instead
of pulling, run `make docker-build` first; it tags the same name locally.

The container is ready when its healthcheck passes. The daemon binds its socket
before browser startup but serves only once `/ping` returns `{"ok":true,...}`;
startup attests the first tenant, caches a GVS token, and runs a full-length
streaming proof, usually 10-30 seconds. A mint failure stops startup; a failed
streaming proof is logged and retried by `/player-context` or `/session`. Once
ready, call the API:

```sh
curl -s localhost:4416/get_pot -d '{"content_binding":"<video_id>"}'
curl -s localhost:4416/session
curl -s localhost:4416/player-context -d '{"video_id":"<video_id>"}'
curl -s localhost:4416/ping
curl -s localhost:4416/metrics
```

### Running with a consumer

A PO token is bound to the minting host's egress IP, so a consumer that fetches
media must egress the same IP as WaxSeal. `compose.full.yaml` runs the daemon and
a consumer in one network namespace to guarantee that; point `CONSUMER_IMAGE` at
your application, and the daemon stays unpublished:

```sh
CONSUMER_IMAGE=your/image:tag docker compose -f compose.full.yaml up
```

Both `compose.yaml` (standalone) and `compose.full.yaml` extend the shared,
hardened `compose.base.yaml`; see those files for the read-only, resource-limit,
and multi-tenant options. Publishing beyond loopback requires API keys, described
under [Authentication and tenants](#authentication-and-tenants).

### From source

Build and run without Docker, on Linux or macOS (the daemon does not run on
Windows). This path needs Go and a system Chromium:

```sh
go build ./...
go run ./cmd/waxseal server   # start the daemon on 127.0.0.1:4416
```

The CLI also runs one-shot commands, each against a fresh browser:

```sh
go run ./cmd/waxseal -c <content_binding>       # one-shot token
go run ./cmd/waxseal player-context <video_id>  # one-shot streaming context
go run ./cmd/waxseal doctor                     # report identity and token grade
go run ./cmd/waxseal ping                       # check a running daemon
```

Prefer the warm daemon for repeated requests. Commands that take `--video` want a
bare video ID, not a URL.

`doctor` can also stop short of the full check. `--skip-attest` reports the
captured identity without attesting, leaving the `attest` key out of the report
rather than showing an empty grade. `--stop-after-load` stops earlier still, at
the load event of a page the command serves to itself on loopback, so it verifies
that Chromium renders and navigates with no external network at all;
`--landing-url` aims that check at some other page. Neither combines with
`--full`, which needs an attested session. The container image is smoke-tested
with `waxseal doctor --stop-after-load` on an isolated network.

## HTTP API

| Method | Endpoint | Purpose |
|---|---|---|
| `POST` | `/get_pot` | Mint or retrieve a cached PO token |
| `GET`, `POST` | `/player-context` | Return an attested streaming context |
| `GET` | `/session` | Export the attested guest identity and cookies |
| `POST` | `/report` | Report a degraded stream and recycle its session |
| `GET` | `/ping` | Health of the tenant's session, or of the shared browser when a keyed daemon gets no key |
| `GET` | `/metrics` | Operational counters; keyed daemons redact tenant detail |

Tokens and exported identities are bound to the minting host's egress IP, so the
consumer must issue SABR media requests from that same IP. The `client` package
mirrors these shapes and keeps the JSON tags in sync, so the fields below are
authoritative. Optional fields are marked. Errors use a JSON envelope, described
under [Errors](#errors).

### `POST /get_pot`

`content_binding` is the value the token binds to: a **video ID** for a player
token or **visitor data** for a GVS token, up to 4096 bytes. The optional `scope`
(`player`, `gvs`, `pot`, or omitted) only namespaces cache entries;
`content_binding` selects the token type. The response sets `X-Pot-Cache: hit`
when served from the cache or `miss` when freshly minted. A cache miss keeps a
fresh mint at least 12 seconds clear of the last context establishment on that
browser session, for the same grading reason described under `/player-context`
below, so a request that misses the cache just after any establishment on that
browser session, not only the startup proof, may wait up to that long.

```jsonc
// request
{"content_binding": "<video_id | visitor_data>", "scope": "player"}  // scope optional
// response
{
  "poToken": "MnRV...",
  "contentBinding": "<echoed content_binding>",
  "expiresAt": "2026-07-01T18:00:00Z",   // RFC3339; now+6h when the grant lifetime is unknown
  "warning": "content_binding looks like a URL; ..."  // optional; only when the binding looks like a URL
}
```

### `GET`, `POST /player-context`

`POST /player-context {"video_id":"<id>"}` or `GET /player-context?video_id=<id>`
returns the browser's streaming context. Select each `audio_formats` entry by its
full `(itag, lmt, xtags)` tuple, never by `itag` alone: a clean track and a DRC
track can share `itag` 251 and differ only in `xtags`, and an inconsistent tuple
makes the SABR server return a player-response reload instead of media.
`playability_status` is YouTube's string status (such as `"OK"`), not the SABR
status-1 protection code embedded in the signed URL.

```jsonc
// response
{
  "playability_status": "OK",
  "player_url": "https://www.youtube.com/s/player/<hash>/player_ias.vflset/en_US/base.js",
  "server_abr_streaming_url": "https://...&n=<scrambled>",   // descramble n with player_url before use
  "video_playback_ustreamer_config": "<base64>",
  "visitor_data": "<base64>",
  "client_version": "2.YYYYMMDD.NN.NN",
  "title": "<video title>",
  "author": "<channel name>",
  "length_seconds": 634,
  "audio_formats": [
    {
      "itag": 251,
      "lmt": "1699999999999999",
      "xtags": "",                          // clean track
      "mime_type": "audio/webm; codecs=\"opus\"",
      "bitrate": 130000,
      "content_length": 10318791,
      "approx_duration_ms": 634601,
      "audio_sample_rate": 48000,
      "audio_channels": 2,
      "audio_quality": "AUDIO_QUALITY_MEDIUM",
      "is_drc": false,
      "audio_track_id": ""                  // empty for the default or only track
    },
    {
      "itag": 251, "lmt": "1699999999999999", "xtags": "CggKA2RyYxIBMQ", "is_drc": true
      // same itag and lmt as the clean track, different xtags: the DRC variant.
      // Remaining fields as above. Select by the full tuple, never itag alone.
    }
  ],
  "session_generation": 1
}
```

Two things happen before the daemon serves a context. It proves full-length
streaming once per browser session, on the landing video or, if that one is
unavailable or too short, a fallback candidate, because a context that is the
session's first playback is graded as a preview about as often as not. A session
that cannot prove it is refused rather than served, and a failed proof is not
retried on every request: it refuses immediately for the next 30 seconds without
another attempt, and if the proof still fails once that cool-down has passed, the
session is relaunched once and the fresh session is proved in its place, refusing
only if that also fails. The refusal arrives as the `player-context-failed` error
(502) and is safe to retry once the cool-down passes, so a consumer does not need
to treat it as a problem with the video. It also keeps the served context at
least 12 seconds away from the last token mint or proof playback on that browser
session, and keeps a token mint the same distance from the last establishment,
because a context taken within a few seconds of either is graded the same way.
Contexts served earlier do not extend that window, so back-to-back requests are
not delayed by one another. The first context after startup or a relaunch may
wait for both steps; later requests normally find the session proved and the
window already clear. A context that clears both steps is then served without
any further, per-request check: the daemon removes the measured cause of a
graded preview and refuses when it cannot prove the session, but it does not
itself grade the URL it hands out. The startup self-test performs the proof
before the daemon accepts traffic. `WAXSEAL_MINT_SEPARATION` overrides the
spacing with any positive Go duration, for example `20s`.

### `GET /session`

Exports the guest identity for the session-adoption path (`--session-url` plus
`--potoken-url`), after verifying full-length streaming with the same cool-down
and one-relaunch-per-streak policy described under `/player-context`. No request
body, and no Google login. If the startup self-test already proved the session
this call is immediate; if it did not, this call performs the proof itself, and
unlike a served context it is not held back by the mint-separation window, since
`/session` hands out the identity itself rather than a context tied to a recent
mint. A session that cannot prove full-length streaming is refused as
`no-session` (503) rather than exported.

```jsonc
// response
{
  "visitor_data": "<base64>",
  "user_agent": "Mozilla/5.0 ...",
  "client_version": "2.YYYYMMDD.NN.NN",
  "cookies": [
    {
      "name": "VISITOR_INFO1_LIVE",
      "value": "...",
      "domain": ".youtube.com",
      "path": "/",
      "secure": true,
      "http_only": true,
      "same_site": "None",               // optional: "Strict" | "Lax" | "None"; omitted when unset
      "expires": "2035-01-02T03:04:05Z"  // optional RFC3339; omitted for session cookies
    }
  ],
  "cookie_header": "VISITOR_INFO1_LIVE=...; YSC=...",
  "session_generation": 1
}
```

### `POST /report`

Report a degraded stream by the `session_generation` from `/session` or
`/player-context`. `session_generation` is required; optional `video_id` and
`reason` must be 1-64 characters from `[A-Za-z0-9_-]`. Reports are scoped and
rate-limited per tenant: report-driven recycles draw from a budget of 4 that
refills at one per `--report-debounce` (default `5m`). A report past the budget
is rejected with `retry_after_seconds`, and stale or future generations are
ignored.

```jsonc
// request
{"session_generation": 1, "video_id": "<id>", "reason": "truncated"}  // video_id, reason optional
// response
{
  "accepted": false,
  "retired": false,
  "retirement_pending": false,
  "generation": 1,
  "retry_after_seconds": 300   // optional; only when rate-limited
}
```

`/metrics` counts each report by disposition: `degradation_reports_accepted`
(applied to the live session), `degradation_reports_rate_limited` (past the
report budget), `degradation_reports_rejected_stale` (an old or replaced
generation), `degradation_reports_already_retired` (the current generation,
already retired by a crash or a prior report; a benign no-op), and
`degradation_reports_duplicate_pending` (a repeat report for a generation whose
retirement is already queued for the next streaming handoff).

### Authentication and tenants

The daemon is keyless and single-tenant by default. Pass `--tenant-keys` to run
isolated browser contexts keyed by API key:

```sh
go run ./cmd/waxseal server --tenant-keys "alice=KEYA,bob=KEYB"
curl -s localhost:4416/get_pot -H "X-API-Key: KEYA" -d '{"content_binding":"<id>"}'
```

Keys travel in `X-API-Key`, `Authorization: Bearer <key>`, or `?key=<key>`, and
are read in that order: the first source carrying a value wins, so a request may
present its key wherever is convenient. The `Bearer` scheme is matched case
insensitively, as RFC 7235 requires, and an `Authorization` header naming another
scheme or carrying no credentials falls through to `?key=` rather than resolving
to an empty key.

One consequence is worth naming for anyone upgrading: a `Bearer` header spelled
in any case now takes precedence over `?key=`, where previously only the exact
spelling `Bearer ` did. A deployment that sends a lowercase `authorization:
bearer` header for something other than WaxSeal, and relies on `?key=` for the
tenant key, has to move that key to `X-API-Key`, which outranks both.

Prefer a header. `?key=<key>` puts the key in the request line, which reverse
proxies and container runtimes write to their access logs, so a health check
polling every few seconds leaves the key in those logs for the life of the
deployment. `waxseal ping --key` and the `client` package both send the header.
A liveness check needs no key at all: a keyed daemon answers a keyless `/ping`
with the shared browser's health rather than `401` (see
[Operations](#operations)), which is what the image's `HEALTHCHECK` relies on.
An empty `--key` is a usage error rather than "no key", so a probe whose key
variable is unset fails loudly instead of quietly checking the browser alone.
`--tenant-keys` takes comma-separated `label=key` entries or bare keys (which get
generated labels); labels and keys must be non-empty and unique, and an invalid
set stops startup before Chromium launches. A keyless daemon on a non-loopback
host exposes its guest identity through `/session` and `/player-context`, so use
`--tenant-keys` when exposing the service.

### Metrics

`/metrics` reports operational counters and always returns HTTP 200; redaction is
a successful response, not a `401`. On a keyed daemon it is **redacted by
default** and unlocks only for the operator key or an explicit public flag:

| Daemon / request | `/metrics` returns |
|---|---|
| keyless (default) | full per-tenant detail |
| keyed, no key / tenant key / wrong key | redacted aggregate: daemon-wide summed counters, no labels, no tenant count |
| keyed, correct `--metrics-key` | full per-tenant detail |
| keyed, `--metrics-public` | full per-tenant detail, unauthenticated |

Tenant keys never unlock detail; only `--metrics-key` (which must differ from
every tenant key) or `--metrics-public` does, keeping minting keys separate from
metrics access. When both are set, `--metrics-public` wins. Both are ignored on a
keyless daemon.

The full view is `{"tenants":N,"per_tenant":{"<label>":{...}},...}` plus the
two daemon-wide browser counters below, each tenant object carrying lifetime
counters (`mints`, `crashes`, `player_contexts`,
`separation_waits` for requests held back to keep a mint and an establishment
apart, `unproven_rejections` for contexts refused because the session could not
prove full-length streaming, the five `degradation_reports_*` dispositions above,
and so on) plus current state.

These counters are worth knowing exactly:

- `player_context_failures` counts real failed attempts against the browser and
  nothing else.
- `player_context_negative_cache_hits` counts requests refused from the
  negative cache without touching the browser. They are counted apart because one
  caller looping on a single unplayable video can drive this by six orders of
  magnitude while every real request succeeds, which would bury the failure rate.
- `probe_failures` counts the sessions a tenant-level `/ping` confirmed
  unresponsive and retired, one per session. `crashes` also counts CDP-event
  deaths, so the two together separate probe-detected loss from the rest. A
  browser the probe tears down (below) retires every tenant's session on it,
  which each tenant counts as a crash: the sessions were already unusable on a
  wedged browser, and the teardown is what lets them come back.
- `probe_busy` counts the tenant probes that confirmed a page failure but found
  the page held by a request, so nothing was retired at the session level. It is
  benign, which is exactly why it is counted: without this, a daemon answering
  `busy` on every probe looks the same as a healthy one. The browser check that
  follows such a probe can still find the browser itself wedged, in which case
  the response reads `probe-failed` while this counter has moved.
- `browser_probe_failures` counts the browsers a `/ping` confirmed unresponsive
  and tore down, whether the daemon-level probe found it or a tenant-level probe
  escalated to it, one per browser lost rather than one per probe.
- `browser_relaunch_failures` counts the relaunch attempts, from a probe or a
  request, whose launch failed. Together with a failing probe it says the daemon
  has no browser and cannot get one. Both browser counters describe the shared
  Chromium, not a tenant, so they sit at the top level of both views instead of
  among the summed counters.
- `cache_entries` reports **servable** entries: current generation, not yet
  expired, which is what a token request would actually be served from. A
  consumer degradation report drops that generation's cached tokens, so it takes
  `cache_entries` for the tenant to 0.

Detail fields are always present so the schema stays stable across retirement,
crash, and recycle; a field that does not apply is `null` or `""` rather than
omitted. For example `last_browser_proof_age_secs` is `null` until the first
proof, which reserves `0` for "just proved", and
`streaming_seconds_until_recycle` appears only when time-based recycling is
enabled (`--streaming-max-age` > 0). The redacted view is
`{"redacted":true,"aggregate":{...},...}`: the same counters summed across
tenants, with no labels and no tenant count, plus the two daemon-wide browser
counters at top level.

### Errors

Recognized endpoints and unknown paths return
`{"error":"<message>","code":"<machine-readable-code>"}`. `video-unavailable`
adds a `details` field with the playability status. `/ping` health bodies do
not use this envelope; they report health directly (see
[Operations](#operations)). Only its `400` and `401` rejections do.

| Code | HTTP | Meaning |
|---|---:|---|
| `invalid-request` | 400 | Malformed or invalid input |
| `unauthorized` | 401 | Missing or invalid API key |
| `not-found` | 404 | Unknown path or endpoint |
| `method-not-allowed` | 405 | Unsupported HTTP method |
| `video-unavailable` | 422 | Terminal playability status |
| `mint-failed`, `player-context-failed` | 502 | Upstream operation failed |
| `no-session` | 503 | No attested session is available |
| `timeout` | 504 | Deadline elapsed for `/get_pot`, `/player-context`, or `/session` |

Two cases skip the envelope, both handled by `http.ServeMux` before any WaxSeal
handler runs. A non-canonical path (with `.`, `..`, or repeated slashes, such as
`//get_pot`) gets a **307** redirect to its cleaned form with the short
`text/html` or empty body that `http.Redirect` produces, so a client that does
not follow redirects must not expect JSON there. A trailing slash is a distinct
path, so `/get_pot/` returns the structured **404**.

`/report` decodes strictly: an unknown field, often a typo such as `raeson` for
`reason`, is rejected with **400 `invalid-request`** naming the key, since its
optional fields would otherwise be dropped silently. `/get_pot` and
`/player-context` stay lenient and ignore unknown fields, because `/get_pot` must
tolerate the extra fields a generic yt-dlp client sends (`proxy`, `bypass_cache`,
`source_address`) and `/player-context` reads `video_id` from the body or the
query string. Duplicate keys are lenient everywhere, since `encoding/json` keeps
the last value. The `client` package parses these into `*client.APIError` with
matching code constants.

## Operations

One Chromium process hosts an isolated incognito context per tenant; additional
tenants attest on their first token, player-context, or session request.

WaxSeal launches Chromium over a CDP pipe. On normal teardown it terminates
Chromium's process group and removes the profile, and a clean exit also lets
Chromium read EOF on the closed pipe and quit. If the daemon dies without
teardown (SIGKILL, OOM), a browser may linger briefly; the next startup removes
abandoned WaxSeal profile directories it can prove are unused, without scanning or
killing processes, and Chromium generally exits once its profile is gone.
Profiles live under `$HOME` so snap-confined Chromium can open them and so shared
hosts keep each daemon's profiles private.

The `crashes` metric counts unexpected browser loss from Chromium events or a
failed health probe, not retirement from age, a report, or operation retries. A
probe failure is confirmed by a second probe before the session is retired, so a
single transient CDP hiccup no longer destroys a warm generation; only the
confirmed loss counts. `probe_failures` is the probe-only view of the same
events.
`--report-debounce` (default `5m`) throttles all report-driven recycles for a
tenant across generations, not just repeats of one generation. Bursts of up to 4
recycles are allowed before the limit bites, enough for a consumer whose
bulk-enumeration throttle escape rotates its identity several times in quick
succession; past the burst, the budget refills at one recycle per interval. This
is deliberate anti-storm behavior; workloads that recycle faster on a sustained
basis may lower it.

Health checks use `/ping`. With a tenant key, or on a keyless daemon where the
empty key selects the one tenant, it probes that tenant's session and returns
HTTP 200 with `ok:true` or `ok:false`, `probe:"tenant"`, and an always-present
`reason`: `ok`, `no-session` (benign, since a `POST /report` retires the session
and re-establishment is lazy, so `ok` briefly reads `false`), `busy` (benign: a
probe failed twice but a request held the page, so nothing was retired and the
next probe re-checks once that request finishes), or `probe-failed` (this probe
confirmed a loss, logged at `warn`).

Whenever no page answered, the daemon also checks the shared Chromium, because
a wedged browser looks the same from a retired page, a page in use, or no page
at all, and the next request would otherwise stall on it for its whole budget
before the pool noticed. A browser that answers leaves the tenant reason
standing. One that had exited is relaunched on the spot, the tenant reason
stands, and the body says `browser_relaunched:true`. One that misses two probes
is torn down and replaced, counted in `browser_probe_failures`, and reported as
`probe-failed` with the browser's error after the tenant's, since the loss is
the probe's finding even though it was remedied. One that cannot be replaced is
`probe-failed` too, with the launch error. A page that answered has already
proved the browser, so a healthy probe costs one round trip.

On a keyed daemon, a `/ping` that presents no key is answered at daemon scope
instead of with `401`. The body says `probe:"daemon"` and carries only `ok`,
`reason`, `browser_relaunched`, and on failure `error`: the browser check above
on its own, with `ok` meaning a running Chromium answered, possibly after a
relaunch. That is less than the redacted `/metrics` already serves anyone,
which is why the probe needs neither a key nor a loopback source: an
orchestrator's probe arrives from the node, and a port published through
Docker's proxy arrives from the bridge address, so the source says nothing
about who is asking. A caller cannot make a healthy browser fail the check, so
the teardown and relaunch it can lead to happen only to a browser that is
wedged or gone, and the pool single-flights and backs off relaunches on its
own. A key that is present but wrong is still `401`, so a typo in a probe's
`--key` fails the probe instead of quietly downgrading it to the daemon-level
answer.

Alert only on `probe-failed`; a caller that disconnects mid-probe is not counted
as one. For status-code-only checks (k8s, `curl -f`, HAProxy), `?strict=true`
maps `probe-failed` to **503** while `no-session`, `busy`, and healthy stay
**200**, and `waxseal ping --strict` does the same from the CLI, so liveness
probes do not fail during the benign re-establishment window. A bare `?strict`
also enables it; a value `strconv.ParseBool` cannot read (`yes`, `on`, `banana`)
returns **400** rather than quietly running non-strict, so a typo in a probe is
visible. Size a probe's timeout for the worst case: up to four session round
trips and a session teardown, two browser round trips and a browser teardown,
each bounded at 5 seconds, plus a relaunch, which normally takes a second or two
and is bounded by the 60 second launch handshake; the image's `HEALTHCHECK`
allows 110. The image runs `waxseal ping --strict` with no key, which checks
the browser on a keyed daemon and the one tenant's session plus the browser on
a keyless one, and it keeps working once the daemon is keyed. Add `--key <key>`
to also probe that tenant's session; the CLI sends the key as a header and keeps
it out of access logs.

Headless Chromium reports a `HeadlessChrome` token in `navigator.userAgent` and
in its brand list, so WaxSeal installs a user-agent override that substitutes
`Chrome` for it. Everything else in that override is the browser's own
`navigator.userAgentData`, read back from a page WaxSeal serves to itself on
loopback (the API is exposed only in a secure context, and the `about:blank` the
override has to be installed on is not one). That keeps the randomised GREASE
brand, the four-part build version, and the real platform, architecture, and
bitness, all of which a fabricated block gets wrong in stable and inspectable
ways. `WAXSEAL_UA_HINTS=synthetic` restores the fabricated block if the real one
ever grades worse; `real` is the default and any other value is ignored with a
warning. Note that a Debian `chromium` build, which the image runs, reports no
`Google Chrome` brand at all while its user agent still says `Chrome/<version>`.
That is what real Debian Chromium looks like, not a bug.

WaxSeal is meant for loopback or a trusted network and does not implement CORS;
because it mints tokens, browser-origin access is out of scope. Run
`go run ./cmd/waxseal server --help` for the rest: session recycling, report
debounce, bind address, headful mode, and metrics access.

## Development

```sh
go test ./...                              # offline unit tests; no browser or network
(cd provider && go test ./...)             # the nested module's offline tests
go test -tags live ./internal/cdp          # real-Chromium CDP pipe-transport tests
(cd provider && go test -tags e2e ./...)   # provider network e2e; needs WAXSEAL_URL/WAXSEAL_KEY
make deps                                  # install browser-bundle build dependencies
make jsbundle-browser                      # regenerate internal/browser/bg_browser_bundle.js
```

`go test ./...` is fully offline and deterministic: no browser, no network. It
does not reach `provider/`, which is a separate module, so that module's own
offline tests (also pure `httptest`) need the second line; `make test` runs both.
The committed browser bundle means normal builds do not need Node. The live CDP
tests self-skip when no browser is found (`WAXSEAL_CHROME_BIN` picks one,
`WAXSEAL_REQUIRE_CHROME=1` fails instead of skipping, which CI sets). The `e2e`
tests live in the nested `provider/` module and must run from that directory,
since a root-level `go test -tags e2e ./...` silently descends into nothing; they
need a warm daemon and include the full-length WEB SABR download
(`go test -tags e2e -run PlayerContextOnlyFullLength ./...`). Set
`WAXSEAL_E2E_LOG_LEVEL=debug` to see the in-process daemon's debug logs in
`go test -v` output for that suite. `TestAgingMatrix` is a separate, opt-in
measurement of how an artifact's age affects a capped stream, not a regression
test: it skips unless `WAXSEAL_E2E_AGING=1` (which artifact's age predicts a
truncated stream) or `=2` (how much separation between a mint and a served
context is enough) is set, runs for tens of minutes, and only ever reports a
tally, never a pass/fail on truncation. `WAXSEAL_E2E_AGING_N` overrides the
per-arm iteration count (default 6) and `WAXSEAL_E2E_AGING_DELAY` overrides the
run-wide delay between warming and streaming (default `30s`; an arm carrying its
own delay ignores it). By default every in-process daemon the suite starts keeps
its own mint-to-establishment gate (12s unless `WAXSEAL_MINT_SEPARATION`
overrides it), so a default run is really a regression check: every arm is
expected to stream full length. `WAXSEAL_E2E_AGING_SEPARATION` (a Go duration
such as `1ms`) overrides that gate on those daemons so the arms measure raw gaps
again, the way the matrix originally separated them; because attestation always
pre-mints a token, the token age arms then measure time since attestation rather
than since their own mint call, which usually just returns that cached token.
The `client` package is a reusable, consumer-agnostic HTTP client; the
`provider/` module adapts it to the token-provider interface a streaming
consumer expects.

CLI exit codes: `0` success, `1` runtime failure, `2` usage error, `3` unavailable
video, `130` interruption.

Some coverage stays out of `go test ./...` because it needs a display or a long
run: **headful mode** (`go run ./cmd/waxseal server --headful`) to watch a real
session, a **time-based recycling soak** (a short `--streaming-max-age` with
continuous streaming to watch `streaming_seconds_until_recycle`), and a
**cache-exhaustion loop** (`POST /get_pot` 1000+ times with distinct
`content_binding` values to exercise cache eviction).

## License

MIT. Implemented independently. The GPL-3.0 bgutil project is a behavioral and
wire reference only. See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
