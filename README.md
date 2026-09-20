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

WaxSeal runs as a container, and the published image bundles Chromium, so the
host needs only Docker. The repository's `compose.yaml` is the whole deployment;
copy it as it is:

```yaml
# WaxSeal, a YouTube PO-token service
services:
  waxseal:
    image: ghcr.io/colespringer/waxseal:${WAXSEAL_VERSION:-latest}
    container_name: waxseal
    restart: unless-stopped # the image runs tini as PID 1, so no init: is needed
    ports:
      # Loopback only. The daemon is keyless by default, so set API keys
      # before publishing on 0.0.0.0 or a LAN address.
      - "127.0.0.1:4416:4416"
    shm_size: 1gb # headless Chromium's working space
    stop_grace_period: 70s # covers the daemon's 60s request drain; compose's default is 10s
    # Chromium runs with --no-sandbox, so the container is the sandbox: the
    # image's non-root user, no capabilities, no privilege escalation.
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    logging: # cap log growth for a long-running daemon
      driver: json-file
      options:
        max-size: 10m
        max-file: "3"
```

```sh
docker compose up -d --wait   # pulls ghcr.io/colespringer/waxseal; returns once the daemon is healthy
```

That pulls `:latest`, and `WAXSEAL_VERSION=1.3.0 docker compose up -d --wait`
pins a release. [docs/deployment.md](docs/deployment.md) lists everything the
file leaves out, each as a snippet to paste in: pinning and verifying a release,
API keys and secrets, memory limits, a read-only rootfs, a consumer sharing the
daemon's egress IP, `docker run` without compose, and building the image from
source.

The container is ready when its healthcheck passes, which is what `--wait`
returns on. The daemon binds its socket before browser startup but serves only
once `/ping` returns `{"ok":true,...}`; startup attests the first tenant, caches
a GVS token, and runs a full-length streaming proof, typically under ten seconds
on a warm host; the image's healthcheck allows two minutes. A mint failure stops
startup; a failed streaming proof is logged and retried by `/player-context` or
`/session`. The first token or context request after that may wait up to 12
seconds behind the mint-separation gate described under `/player-context`. Once
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
media must egress the same IP as WaxSeal. A consumer on the host itself already
does, and reaches the daemon through the published port. One that runs as its
own container shares the daemon's network namespace instead, so both leave
through one address and it reaches the daemon on loopback; the file's `ports:`
block still publishes the daemon to the host, so drop it if nothing there should
reach it. The snippet is in
[docs/deployment.md](docs/deployment.md#run-a-consumer-on-the-same-egress-ip).
Publishing beyond loopback requires API keys, described under
[Authentication and tenants](#authentication-and-tenants).

### From source

Build and run without Docker, on Linux, macOS, or Windows. This path needs Go and
a system Chromium or Chrome:

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
bare video ID, not a URL. The one-shot `player-context` prints the same object the
endpoint returns, minus `session_generation`: there is no daemon session to report.

`doctor` can also stop short of the full check. `--skip-attest` reports the
captured identity without attesting, leaving the `attest` key out of the report
rather than showing an empty grade. `--stop-after-load` stops earlier still, at
the load event of a page the command serves to itself on loopback, so it verifies
that Chromium renders and navigates with no external network at all;
`--landing-url` aims that check at some other page. Neither combines with
`--full`, which needs an attested session. The container image is smoke-tested
with `waxseal doctor --stop-after-load` on an isolated network, and `make
docker-build docker-smoke` runs the same check locally.

On Windows, Chrome or a Chromium build is auto-detected under Program Files and
the per-user install directory (`WAXSEAL_CHROME_BIN` overrides it, and Edge is never picked up on its
own because it reports a different browser identity), browser profiles live under
`%TEMP%`, and Ctrl-C stops the daemon the same way it does elsewhere. The
container remains the recommended deployment on every host.

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
browser session, not only the startup proof, may wait up to that long. A
consumer degradation report on the current generation drops its cached tokens at
once, and the next request that takes the page, a token request included,
retires the session and serves the replacement.

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
  "player_url": "https://www.youtube.com/s/player/<hash>/<variant>/en_US/base.js",  // the path segment varies (player_ias, player_es6, ...); pass it through as given
  "server_abr_streaming_url": "https://...&n=<scrambled>",   // descramble n with player_url before use
  "video_playback_ustreamer_config": "<base64>",
  "visitor_data": "<base64>",
  "client_version": "2.YYYYMMDD.NN.NN",
  "user_agent": "Mozilla/5.0 ...",        // the identity the context was minted under; the same value /session exports
  "title": "<video title>",
  "author": "<channel name>",
  "length_seconds": 634,
  "channel_id": "UC...",                  // the "UC..." id of the channel that owns the video
  "description": "<full description>",
  "thumbnails": [                         // the player response's own order, smallest first; not
    {"url": "https://i.ytimg.com/vi/...", "width": 168, "height": 94}   // reordered here, because consumers sort it
  ],
  "is_live_content": false,               // true for anything that was ever a broadcast, finished VODs included
  "is_live_now": false,                   // true only while a broadcast is on air
  "is_upcoming": false,                   // true for a scheduled premiere or broadcast
  "publish_date": "2015-04-10T00:00:00-07:00",  // the microformat's string: RFC3339 or a bare date; empty when absent
  "audio_formats": [
    {
      "itag": 251,
      "lmt": "1699999999999999",
      "xtags": "",                          // "" when the video has one audio track; multi-track videos label every entry (such as "en-US.4"), and the original track's xtags decode to acont=original
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
      "itag": 251, "lmt": "1699999999999998", "xtags": "CggKA2RyYxIBMQ", "is_drc": true
      // same itag as the clean track, different lmt and xtags: the DRC variant.
      // A third variant, xtags "CgcKAnZiEgEx" with is_drc false, can share the itag too.
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
window already clear. A context that clears both steps is then confirmed per
request before it is served. A video at or under the roughly 70 second preview
cap is served as it is, since a preview cannot truncate it. A longer video is
seeked past the cap and must buffer beyond the seek target before the daemon
re-reads and serves the transitioned context; a video only a few seconds over
the cap must buffer to its end. A context the daemon cannot confirm before its
budget is retried once in place and then refused as `player-context-failed`
(502) with no relaunch, counted in `status2_rejections`; that refusal carries no
`Retry-After` and is worth one quick retry. A confirmed context logs a
`player-context confirmed` line with the band, buffered end, and outcome; a
video under the cap logs a `cap-safe` line instead. The startup self-test
performs the proof before the daemon accepts traffic.
`WAXSEAL_MINT_SEPARATION` overrides the spacing with any positive Go duration,
for example `20s`.

A bot check ("Sign in to confirm you're not a bot") describes the browser
session, not the video, so it is never answered as `video-unavailable` and the
video is never negative-cached. The wall keys on the visitor identity, and the
only thing that lifts it is a fresh one: the daemon relaunches once per 10
minutes on a bot check and serves the request from the replacement, which is the
common case. A check on the replacement, or one inside that window, is refused as
`player-context-failed` (502) with a 2 minute `Retry-After`. The browser
presents an `en-US,en` language list, so the wall arrives in English and is
recognised whatever the host's locale. That list rides on the user-agent
override, which a `--headful` run does not install, so a headful session keeps
the host's own language and a non-English wall reads there as a timeout.

A broadcast that is on air is refused as `video-unavailable` (422) with `details`
`LIVE_BROADCAST` and negative-cached like any terminal verdict: it has no length
to confirm past the cap, and WaxSeal serves finished videos only. A finished
broadcast is an ordinary video; an upcoming one is refused with YouTube's own
status, `LIVE_STREAM_OFFLINE`.

A terminal verdict is remembered for 5 minutes in a per-tenant negative cache of
at most 256 video IDs, shared by the GET and POST forms and keyed by the video ID
alone. It survives a report, a recycle, and a relaunch, because the video is
unavailable whatever the session; a repeat inside that window is refused without
touching the browser and counted in `player_context_negative_cache_hits`.

### `GET /session`

Exports the guest identity for the session-adoption path (`--session-url` plus
`--potoken-url`), after verifying full-length streaming with the same cool-down
and one-relaunch-per-streak policy described under `/player-context`, including
its bot-check relaunch. No request
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
is rejected with `retry_after_seconds` and a `Retry-After` header, and a
generation other than the current one, older or not yet issued, is ignored.

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
(retired the live session, or queued its retirement for the next request that
takes the page), `degradation_reports_rate_limited` (past the report budget),
`degradation_reports_rejected_stale` (a generation other than the current one:
replaced, or not yet issued),
`degradation_reports_already_retired` (the current generation, already retired by
a crash or a prior report; a benign no-op), and
`degradation_reports_duplicate_pending` (a repeat report for a generation whose
retirement is already queued; it answers `accepted: true`, because the session it
named is on its way out either way, and counts here rather than under
`accepted`).

An accepted report drops the reported generation's cached tokens immediately,
whether or not its retirement had to wait. `accepted: true` with neither
`retired` nor `retirement_pending` set means the session was already gone when
the report was applied, a crash having landed first; nothing was left to retire.

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
variable is unset fails loudly instead of quietly checking the browser alone. A
key that is only whitespace is empty too. On the wire, HTTP parsing strips the
spaces around a header value, so an `X-API-Key` header holding only spaces
arrives empty and reads as no key, while `?key=` keeps its value and a blank one
is a wrong key; `waxseal ping` refuses a blank key before sending anything, and
it fails when a keyless daemon ignored the `--key` it sent.
`--tenant-keys` takes `label=key` entries or bare keys (which get generated
labels), separated by commas or newlines; labels and keys must be non-empty and
unique, keys must not contain whitespace, and a bare key must not contain `=`, so
a base64 key with padding needs a label. An invalid set stops startup before
Chromium launches. A keyless daemon that binds a non-loopback
address exposes its guest identity through `/session` and `/player-context`, and
warns at startup naming the address it bound, so use `--tenant-keys` when
exposing the service.

Both keys reach the daemon from four places: `--tenant-keys` and `--metrics-key`,
the `--tenant-keys-file` and `--metrics-key-file` paths, and the four environment
variables `WAXSEAL_TENANT_KEYS`, `WAXSEAL_TENANT_KEYS_FILE`, `WAXSEAL_METRICS_KEY`,
and `WAXSEAL_METRICS_KEY_FILE`. Flags outrank the environment, and setting a value
and a file in the same tier is a usage error rather than a silent winner. A file
is read whole with trailing whitespace removed, and an empty one is a usage error
rather than a keyless daemon nobody asked for. The same trimming applies to every
source; a blank or whitespace-only environment variable counts as unset, and a
blank flag value is a usage error. Prefer a file in a container: a key
on the command line shows in `ps` and in `docker inspect`, a key in the
environment shows in `docker inspect`, and a `/run/secrets` path shows only the
path. Outside swarm, compose bind-mounts a secret's host file as it is and
ignores the secret's `uid`, `gid`, and `mode`, so that file has to be readable
by the image's non-root user (uid 10001).

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
two daemon-wide browser counters below, where `tenants` counts the tenants used
since start, the same set `per_tenant` lists, so a probe or request on a
never-used tenant raises it; the startup log's `configured_tenants` is the
configured count. Each tenant object carries lifetime
counters (`mints`, `crashes`, `player_contexts`,
`separation_waits` for requests held back to keep a mint and an establishment
apart, `unproven_rejections` for contexts refused because the session could not
prove full-length streaming, `status2_rejections` for contexts refused because
the per-request confirmation did not clear the preview cap, the five
`degradation_reports_*` dispositions above, and so on) plus current state.

These counters are worth knowing exactly:

- `player_context_failures` counts real failed attempts against the browser and
  nothing else, including the first attempt of a request that then succeeds on
  its in-place retry, so it can exceed the number of failed requests.
- `player_context_negative_cache_hits` counts requests refused from the
  negative cache without touching the browser. They are counted apart because one
  caller looping on a single unplayable video can drive this by six orders of
  magnitude while every real request succeeds, which would bury the failure rate.
- `bot_checks` counts each bot check the browser answered, at a proof or at a
  context. The refusal that follows a bot-check cool-down counts under
  `unproven_rejections`, as a proof cool-down's does.
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
- `separation_waits` counts the waits performed; requests queued behind a wait
  share it and are not counted.
- `cache_entries` reports **servable** entries: current generation, not yet
  expired, which is what a token request would actually be served from. A
  consumer degradation report drops that generation's cached tokens, so it takes
  `cache_entries` for the tenant to 0. A crash does not drop the cache: a token
  is not invalidated by the browser that minted it dying, so the replacement
  serves the same tokens until they expire.

Detail fields are always present so the schema stays stable across retirement,
crash, and recycle; a field that does not apply is `null` or `""` rather than
omitted. For example `last_browser_proof_age_secs` is `null` until the first
proof, which reserves `0` for "just proved", and
`streaming_seconds_until_recycle` appears only when time-based recycling is
enabled (`--streaming-max-age` > 0) and counts down to a deadline jittered by
up to ten percent of `--streaming-max-age`, so a fleet does not recycle in
lockstep. The redacted view is
`{"redacted":true,"aggregate":{...},...}`: the same counters summed across
tenants, with no labels and no tenant count, plus the two daemon-wide browser
counters at top level.

### Errors

Recognized endpoints and unknown paths return
`{"error":"<message>","code":"<machine-readable-code>"}`. `video-unavailable`
adds a `details` field with the playability status, or `LIVE_BROADCAST` for a
broadcast that is on air. `/ping` health bodies do
not use this envelope; they report health directly (see
[Operations](#operations)). Only its `400` and `401` rejections do.

A refusal the daemon expects to lift on its own also sends `Retry-After` and
`retry_after_seconds` with the remaining wait: `player-context-failed` and
`no-session` during a cool-down after a failed proof or a bot check, and
`mint-failed`, `player-context-failed`, or `no-session` while the shared
browser's relaunch is backing off. A 502 without them is worth one quick retry;
a 422 is not.

| Code | HTTP | Meaning |
|---|---:|---|
| `invalid-request` | 400 | Malformed or invalid input |
| `unauthorized` | 401 | Missing or invalid API key |
| `not-found` | 404 | Unknown path or endpoint |
| `method-not-allowed` | 405 | Unsupported HTTP method |
| `video-unavailable` | 422 | Terminal playability status |
| `mint-failed`, `player-context-failed` | 502 | Upstream operation failed; may carry `retry_after_seconds` |
| `no-session` | 503 | No attested session is available; may carry `retry_after_seconds` |
| `timeout` | 504 | Deadline elapsed for `/get_pot`, `/player-context`, or `/session` |

Two cases skip the envelope, both handled by `http.ServeMux` before any WaxSeal
handler runs. A non-canonical path (with `.`, `..`, or repeated slashes, such as
`//get_pot`) gets a **307** redirect to its cleaned form with the short
`text/html` or empty body that `http.Redirect` produces, so a client that does
not follow redirects must not expect JSON there. A trailing slash is a distinct
path, so `/get_pot/` returns the structured **404**.

`/report` decodes strictly: an unknown field, often a typo such as `raeson` for
`reason` or a case variant such as `Reason`, is rejected with **400
`invalid-request`** naming the key, since its optional fields would otherwise be
dropped silently. `/get_pot` and
`/player-context` stay lenient and ignore unknown fields, because `/get_pot` must
tolerate the extra fields a generic yt-dlp client sends (`proxy`, `bypass_cache`,
`source_address`) and `/player-context` reads `video_id` from the body or the
query string. Duplicate keys are lenient everywhere, since `encoding/json` keeps
the last value. A body that is not a JSON object (`null`, a number, an array, a
string) is rejected with `request body must be a JSON object`, and a body still
arriving when the 30 second read timeout fires with `request body was not
received before the read timeout`. The `client` package parses these into
`*client.APIError` with matching code constants.

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
hosts keep each daemon's profiles private. On SIGINT or SIGTERM the daemon closes
its listener, so `/ping` fails from the first signal, drains in-flight requests
for `--shutdown-timeout`, then tears the browser down and exits 0, at any point
including warm-up. A second signal cuts the drain short. The last log line is
`waxseal server stopped`.

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
HTTP 200 with `ok:true` or `ok:false`, `probe:"tenant"`, `keyed` (whether the
daemon requires a key at all), and an always-present
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
`probe`, `keyed`, `reason`, `browser_relaunched`, and on failure `error`: the
browser check above on its own, with `ok` meaning a running Chromium answered,
possibly after a relaunch. That is less than the redacted `/metrics` already
serves anyone, which is why the probe needs neither a key nor a loopback
source: an
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
trips and a session teardown, and two browser round trips, each bounded at 5
seconds; a browser teardown of 7, which is a 2 second graceful close plus the
launcher's 5 second wait; and a relaunch, which normally takes a second or two
and is bounded by the 60 second launch handshake. That is 102 seconds, or 105 on
Windows, which also removes the profile. `waxseal ping` allows itself 108, that
worst case plus its own connect, transfer, and decode, and takes `--timeout` to
change it; the image's `HEALTHCHECK` allows 110. The image runs `waxseal ping --strict` with no key, which checks
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

Most daemon settings have both a flag and an environment variable, and where both
exist the flag outranks the variable. A container reaches for the variable, since
a flag there means overriding the image's `CMD`. The rows naming no flag have
only the variable.

| Variable | Sets |
|---|---|
| `WAXSEAL_TENANT_KEYS`, `WAXSEAL_TENANT_KEYS_FILE` | `--tenant-keys`, `--tenant-keys-file` |
| `WAXSEAL_METRICS_KEY`, `WAXSEAL_METRICS_KEY_FILE` | `--metrics-key`, `--metrics-key-file` |
| `WAXSEAL_STREAMING_MAX_AGE` | `--streaming-max-age` |
| `WAXSEAL_REPORT_DEBOUNCE` | `--report-debounce` |
| `WAXSEAL_SHUTDOWN_TIMEOUT` | `--shutdown-timeout` |
| `WAXSEAL_MINT_SEPARATION` | the mint-to-establishment gate, which has no flag; an unparseable value stops startup |
| `WAXSEAL_CHROME_BIN` | the browser binary, for every command |
| `WAXSEAL_UA_HINTS` | the client-hint source, `real` or `synthetic` |

WaxSeal is meant for loopback or a trusted network and does not implement CORS;
because it mints tokens, browser-origin access is out of scope.
`go run ./cmd/waxseal server --help` describes every flag and lists the three
environment-only variables.

## Development

```sh
go test ./...                              # offline unit tests; no browser or network
(cd provider && go test ./...)             # the nested module's offline tests
go test -tags live ./internal/cdp          # real-Chromium CDP pipe-transport tests
(cd provider && go test -tags e2e ./...)   # provider network e2e; needs WAXSEAL_URL/WAXSEAL_KEY
make vet                                   # both modules, plus a windows cross-vet (CI vets Windows natively)
make fmt-check                             # fail if any file needs gofmt
make test                                  # both modules' offline tests, race-enabled
make live                                  # the live CDP tests above
make tidy-check                            # fail if go mod tidy would change either module
make vulncheck                             # govulncheck over both modules
make consumer-check                        # build provider/ as an external go get sees it
make compose-check                         # validate compose.yaml and check it pulls the published image
make verify-assets                         # rebuild the browser bundle and diff it against the committed one
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
truncated stream), `=2` (how much separation between a mint and a served context
is enough), or `=3` (the same question for the distance between the proof
playback and the served context) is set, runs for tens of minutes, and only ever
reports a tally, never a pass/fail on truncation. `WAXSEAL_E2E_AGING_N` overrides the
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
`=3` refuses `WAXSEAL_E2E_AGING_SEPARATION` instead of honouring it: every arm of
that matrix sets the gate to its own gap, which is what holds the served context
exactly that far past the proof, so the gate is the measuring instrument rather
than something to remove. Each of its records carries an `anchor=` column read
out of the daemon's own log, so a run proves per iteration that it waited on the
proof and not on a mint.
The `client` package is a reusable, consumer-agnostic HTTP client; the
`provider/` module adapts it to the token-provider interface a streaming
consumer expects.

`provider/` builds against this checkout through a `replace`, while its `require`
names the root version a consumer resolves, the `replace` being ignored there. So
a change that adds a root member `provider/` uses is pushed first and pinned
second: `go list -m github.com/colespringer/waxseal@<sha>` prints the
pseudo-version of the pushed commit, then `go mod edit
-require=github.com/colespringer/waxseal@<pseudo-version>` and `go mod tidy` from
`provider/`; after each root tag the pin moves to the tag. `make consumer-check`
builds the module with the `replace` dropped, and the release gate runs it, so a
stale pin stops a release rather than a consumer.

Debug logging (`-v`) includes full video IDs and the `id`, `expire`, and `spc`
parameters of streaming URLs, which INFO lines leave out.

CLI exit codes: `0` success, `1` runtime failure, `2` usage error, `3` unavailable
video, `130` interruption of a one-shot command; `server` exits 0 on a requested
stop. A bot check is a runtime failure (`1`), not an unavailable video: it
describes the browser session rather than the video.

Some coverage stays out of `go test ./...` because it needs a display or a long
run: **headful mode** (`go run ./cmd/waxseal server --headful`) to watch a real
session, a **time-based recycling soak** (a short `--streaming-max-age` with
continuous streaming to watch `streaming_seconds_until_recycle`), and a
**cache-exhaustion loop** (`POST /get_pot` 1000+ times with distinct
`content_binding` values to exercise cache eviction).

Work cut from a change is tracked in [docs/deferred-work.md](docs/deferred-work.md),
and what WaxSeal wants from the sibling repos it depends on in
[docs/upstream-requests.md](docs/upstream-requests.md).

## License

MIT. Implemented independently. The GPL-3.0 bgutil project is a behavioral and
wire reference only. See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
