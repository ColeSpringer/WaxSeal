# HTTP API

Tokens and exported identities are bound to the minting host's egress IP, so the
consumer must issue SABR media requests from that same IP. Contract tests hold
the server and the `client` package to the fields below; optional ones are
marked. Errors use the JSON envelope described under [Errors](#errors).

WaxSeal is meant for loopback or a trusted network and does not implement CORS;
because it mints tokens, browser-origin access is out of scope.

## Endpoints

The README's [table](../README.md#http-api) lists all six. `/ping` is covered
under [Health checks](operations.md#health-checks) and `/metrics` under
[Metrics](#metrics).

### `POST /get_pot`

`content_binding` is the value the token binds to, up to 4096 bytes: a **video
ID** for a player token or **visitor data** for a GVS token. The optional
`scope` (`player`, `gvs`, `pot`, or omitted) only namespaces cache entries;
`content_binding` selects the token type. The response sets `X-Pot-Cache: hit`
for a cached token and `miss` for a fresh mint.

A cache miss mints no sooner than 12 seconds after the last establishment (a
proof or a context) on that browser session, so it may wait up to that long;
`/player-context` below explains why. After an accepted `/report`, the next
token request mints on the replacement session.

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
returns the browser's streaming context. Its `playability_status` is YouTube's
string status (such as `"OK"`), not the SABR status-1 code in the signed URL.

Select each `audio_formats` entry by its full `(itag, lmt, xtags)` tuple: a
clean and a DRC track can share `itag` 251, and a mismatched tuple makes the
SABR server return a player-response reload instead of media. `xtags` is the
player response's value verbatim, an unpadded base64url protobuf of key/value
pairs; read the audio role (`acont`) from it to rank tracks, and send it back
byte for byte.

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
      "xtags": "",                          // "" for a plain single track
      "mime_type": "audio/webm; codecs=\"opus\"",
      "bitrate": 130000,
      "content_length": 10318791,
      "approx_duration_ms": 634601,
      "audio_sample_rate": 48000,
      "audio_channels": 2,
      "audio_quality": "AUDIO_QUALITY_MEDIUM",
      "is_drc": false,
      "audio_track_id": ""                  // "" on a single-track video, which also omits audio_is_default
    },
    {
      "itag": 251, "lmt": "1699999999999998", "xtags": "CggKA2RyYxIBMQ", "is_drc": true
      // same itag as the clean track, different lmt and xtags: the DRC variant.
      // A third variant, xtags "CgcKAnZiEgEx" with is_drc false, can share the itag too.
      // Remaining fields as above. Select by the full tuple, never itag alone.
    },
    {
      "itag": 251, "lmt": "1699999999999997", "xtags": "ChEKBWFjb250EghvcmlnaW5hbAoNCgRsYW5nEgVlbi1VUw",
      "audio_track_id": "en-US.4", "audio_is_default": true
      // an entry of a multi-track video, for the two fields a single-track one omits: every entry names
      // its track (audioTrack.id) and states audioTrack.audioIsDefault as the player does, true on the
      // default track, which can be a dub, and false on the rest; its xtags carries the audio role, here
      // acont=original, which a consumer ranks the original track by before falling back to the flag.
    }
  ],
  "session_generation": 1
}
```

Before serving a context, the daemon proves full-length streaming once per
browser session, on the landing video or a fallback if that one is unavailable
or too short, because a session's first playback is graded as a preview about
half the time. The startup self-test normally runs this proof.

A request whose session cannot prove is refused as `player-context-failed`
(502), and so is every request for the next 30 seconds, without another attempt.
If the proof fails again after that, the session is relaunched once and the
replacement proved; the request is refused only if that fails too. The refusal
says nothing about the video: retry once the cool-down passes.

A served context also stays 12 seconds clear of the session's last token mint
or proof, and a mint 12 seconds clear of its last establishment, because a
context taken within a few seconds of either is graded as a preview too.
Earlier contexts do not extend the window, so normally only the first context
after startup or a relaunch waits. `WAXSEAL_MINT_SEPARATION` overrides the 12
seconds with any positive Go duration, such as `20s`.

Each context is then confirmed before it is served. A video longer than the
roughly 70 second preview cap is seeked past the cap and must buffer beyond the
seek target (to its end, if only a few seconds over); the re-read context is
served and logged as `player-context confirmed`. A video at or under the cap is
served unconfirmed and logged as `cap-safe`: the browser cannot see a cap the
consumer meets, and the 12 second separation keeps these contexts full length.

A context not confirmed within its budget is retried once in place, then
refused as `player-context-failed` (502) with no relaunch and no `Retry-After`,
counted in `status2_rejections`. One quick retry is worthwhile.

A bot check ("Sign in to confirm you're not a bot") walls the visitor identity,
not the video, so it is never answered as `video-unavailable` or
negative-cached. The daemon relaunches for a fresh identity, at most once per
10 minutes, and usually serves the request from the replacement; a check on the
replacement or inside that window is refused as `player-context-failed` (502)
with a 2 minute `Retry-After`.

The browser presents an `en-US,en` language list, so the wall arrives in English
and is recognized whatever the host's locale. `--headful` skips the user-agent
override that carries the list, so there a non-English wall goes unrecognized
and is graded like any per-video verdict (422, cached).

While the list is pinned (always, unless `--headful`), a `LOGIN_REQUIRED`
refusal without video details whose reason names neither the bot check nor a
private video may be a wall the phrase check missed. It is answered as
`video-unavailable` (422) but never negative-cached, counts in
`unrecognized_login_refusals`, and logs one warning per distinct reason.

A broadcast that is on air is refused as `video-unavailable` (422) with
`details` `LIVE_BROADCAST`, a terminal verdict, since WaxSeal serves finished
videos only. A finished broadcast is an ordinary video; an upcoming one is
refused with YouTube's own status, `LIVE_STREAM_OFFLINE`.

A terminal verdict, except an unrecognized `LOGIN_REQUIRED` refusal, is cached
per tenant for 5 minutes, up to 256 video IDs, whichever form (GET or POST)
asked. It survives reports, recycles, and relaunches; a repeat inside the window
is refused without touching the browser and counted in
`player_context_negative_cache_hits`.

### `GET /session`

Exports the guest identity for a consumer that adopts the session and fetches
its tokens from `/get_pot`. It takes no body and needs no Google login. It
first proves full-length streaming under the `/player-context` policy
(cool-down, one relaunch per failure streak, bot-check relaunch), which costs
nothing when the startup self-test already proved the session. It skips the
mint-separation window. A session that cannot prove is refused as
`no-session` (503).

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
`/player-context`. `session_generation` is required; the optional `video_id` and
`reason` must be 1-64 characters from `[A-Za-z0-9_-]`. A generation other than
the current one, older or not yet issued, is ignored.

Reports are scoped and rate-limited per tenant, across generations:
report-driven recycles draw from a budget of 4 that refills at one per
`--report-debounce` (default `5m`), enough for a burst of identity rotations
while stopping a recycle storm. Lower the interval if a workload recycles
faster on a sustained basis. A report past the budget is rejected with
`retry_after_seconds` and a `Retry-After` header.

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

`/metrics` counts each report by disposition:

- `degradation_reports_accepted`: retired the live session, or queued its
  retirement for the next request that takes the page.
- `degradation_reports_rate_limited`: past the report budget.
- `degradation_reports_rejected_stale`: a generation other than the current
  one, replaced or not yet issued.
- `degradation_reports_already_retired`: the current generation, already
  retired by a crash or a prior report; a benign no-op.
- `degradation_reports_duplicate_pending`: a repeat report for a generation
  whose retirement is already queued; it answers `accepted: true` but counts
  here, not under `accepted`.

An accepted report drops the generation's cached tokens at once, even when its
retirement waits. `accepted: true` with neither `retired` nor
`retirement_pending` means a crash or recycle retired the session first.

## Authentication and tenants

The daemon is keyless and single-tenant by default. Bound to a non-loopback
address, a keyless daemon exposes its guest identity through `/session` and
`/player-context` and warns at startup, naming the address, so set keys before
exposing it. `--tenant-keys` runs an isolated browser context per API key:

```sh
go run ./cmd/waxseal server --tenant-keys "alice=KEYA,bob=KEYB"
curl -s localhost:4416/get_pot -H "X-API-Key: KEYA" -d '{"content_binding":"<id>"}'
```

Keys travel in `X-API-Key`, `Authorization: Bearer <key>`, or `?key=<key>`, and
the first of those carrying a value wins. `Bearer` matches in any letter case
(RFC 7235); an `Authorization` header with another scheme or no credentials
falls through to `?key=`. So a client that sends a `Bearer` header meant for
another service must put its tenant key in `X-API-Key`.

Prefer a header: `?key=` puts the key in the request line, which reverse proxies
and container runtimes write to access logs, so a polling health check leaves
it there for the life of the deployment. `waxseal ping --key` and the `client`
package send the header.

A keyed daemon answers a keyless `/ping` with the shared browser's health
rather than `401` (see [Health checks](operations.md#health-checks)). An empty
or whitespace-only `waxseal ping --key` is a usage error, so a probe with an
unset key variable fails instead of checking only the browser, and
`waxseal ping` fails if a keyless daemon ignored its key. HTTP strips the
spaces around a header value, so an all-space `X-API-Key` is no key, while an
all-space `?key=` is a wrong key.

`--tenant-keys` takes `label=key` entries or bare keys (which get generated
labels), separated by commas or newlines. Labels and keys must be non-empty and
unique, keys must not contain whitespace or non-printing characters, and a bare
key must not contain `=`, so a base64 key with padding needs a label. An invalid
set stops startup before Chromium launches, and the error names the flag or
variable that held it.

The tenant keys and the metrics key (see [Metrics](#metrics)) each have a value
and a file form, as flags (`--tenant-keys`, `--tenant-keys-file`,
`--metrics-key`, `--metrics-key-file`) or variables (`WAXSEAL_TENANT_KEYS`,
`WAXSEAL_TENANT_KEYS_FILE`, `WAXSEAL_METRICS_KEY`, `WAXSEAL_METRICS_KEY_FILE`).
Flags outrank variables; setting both forms in one tier is a usage error.
Sources are trimmed of surrounding whitespace, files also of a leading BOM. An
empty file, a blank flag, or a value with no entries (only separators or a lone
BOM) is a usage error, never a keyless daemon; a blank variable counts as unset.

Prefer a file in a container: a key on the command line shows in `ps` and
`docker inspect`, one in the environment shows in `docker inspect`, and a
`/run/secrets` path shows only the path.
[deployment.md](deployment.md#expose-the-daemon-and-require-api-keys)
has the compose setup, including the permissions the secret file needs.

## Metrics

`/metrics` reports operational counters and always returns HTTP 200, redacted or
not. On a keyed daemon it is **redacted by default** and unlocks only for the
operator key or an explicit public flag:

| Daemon / request | `/metrics` returns |
|---|---|
| keyless (default) | full per-tenant detail |
| keyed, no key / tenant key / wrong key | redacted aggregate: daemon-wide summed counters, no labels, no tenant count |
| keyed, correct `--metrics-key` | full per-tenant detail |
| keyed, `--metrics-public` | full per-tenant detail, unauthenticated |

`--metrics-key` must differ from every tenant key. When both flags are set,
`--metrics-public` wins; both are ignored on a keyless daemon.

The full view is `{"tenants":N,"per_tenant":{"<label>":{...}},...}` plus the
two browser counters below. `tenants` counts the tenants used since start (the
set `per_tenant` lists), so a probe or request on a never-used tenant raises
it; the startup log's `configured_tenants` is the configured count. Each tenant
object carries current state and lifetime counters such as `mints`, `crashes`,
`player_contexts`, `unproven_rejections` (contexts refused for a failed proof),
`status2_rejections` (contexts refused by the per-request confirmation), and
the five `degradation_reports_*` above.

These counters are worth knowing exactly:

- `crashes` counts unexpected browser loss from Chromium events or a confirmed
  probe failure, not retirement for age, a report, or an operation retry.
- `player_context_failures` counts failed attempts against the browser,
  including a first attempt that the in-place retry then rescued, so it can
  exceed the number of failed requests.
- `player_context_negative_cache_hits` counts requests refused from the
  negative cache. It is kept apart because one caller looping on an unplayable
  video can raise it by orders of magnitude and bury the failure rate.
- `bot_checks` counts each bot check met at a proof or a context. A refusal
  during the bot-check cool-down counts under `unproven_rejections`, as a proof
  cool-down's does.
- `unrecognized_login_refusals` counts `LOGIN_REQUIRED` refusals without video
  details that name neither the bot check nor a private video, while the
  language list is pinned (so never under `--headful`). A rising count suggests
  YouTube rephrased its bot wall.
- `probe_failures` counts the sessions a tenant `/ping` confirmed unresponsive
  and retired. `crashes` counts these too, so the two together separate
  probe-detected loss from the rest. A browser the probe tears down (see
  [Health checks](operations.md#health-checks)) retires every tenant's session
  on it, and each tenant counts that as a crash.
- `probe_busy` counts tenant probes that confirmed a page failure while a
  request held the page, so nothing was retired. It is benign, but counted so a
  daemon that is always `busy` does not look healthy. The browser check that
  follows can still find the browser wedged; the response then reads
  `probe-failed` while this counter moves.
- `browser_probe_failures` counts the browsers a `/ping` (daemon-level, or a
  tenant probe that escalated) confirmed unresponsive and tore down, one per
  browser, not per probe.
- `browser_relaunch_failures` counts failed relaunch attempts, from a probe or a
  request; with a failing probe, it means the daemon has no browser and cannot
  get one. Both browser counters describe the shared Chromium, so both views
  carry them at the top level, not among the summed counters.
- `launch_failures` counts failed session launches, and
  `launch_cooldown_refusals` the callers refused inside the cool-down a failure
  opens, without a launch: requests, and the startup retry when a request's
  failure reopened the window under it. `consecutive_launch_failures` counts
  the launches in a row that ran to a failure and opened a cool-down, 0 once a
  session attests; a launch cut short by a browser death, a relaunch backoff,
  or shutdown moves only `launch_failures`. `launch_retry_after_secs` is what
  is left of the cool-down, `null` when none is open.
- `separation_waits` counts the mint-separation waits performed; requests
  queued behind a wait share it and are not counted.
- `cache_entries` reports **servable** entries (current generation, not yet
  expired). An accepted report takes it to 0. A crash does not, since a token
  outlives its browser: the cache keeps serving until a request that needs the
  browser relaunches it, and the new generation clears the cache.

Detail fields are always present, across retirement, crash, and recycle; one
that does not apply is `null` or `""`. For example,
`last_browser_proof_age_secs` is `null` until the first proof, so `0` means
"just proved". The exception is `streaming_seconds_until_recycle`, present only
when time-based recycling is on (`--streaming-max-age` > 0); it counts down to
a deadline jittered by up to ten percent so a fleet does not recycle in
lockstep.

The redacted view is `{"redacted":true,"aggregate":{...},...}`: the counters
summed across tenants, plus the two browser counters at the top level.

## Errors

Every error, an unknown path's 404 included, returns
`{"error":"<message>","code":"<machine-readable-code>"}`. `video-unavailable`
adds `details`: the playability status, or `LIVE_BROADCAST` for a broadcast on
air. `/ping` reports health in its own body (see
[Health checks](operations.md#health-checks)) and uses the envelope only for
its `400` and `401`.

A refusal the daemon expects to lift on its own also sends `Retry-After` and
`retry_after_seconds` with the remaining wait: `player-context-failed` and
`no-session` during a cool-down after a failed proof or a bot check, and
`mint-failed`, `player-context-failed`, or `no-session` after a failed session
launch or while the shared browser's relaunch is backing off. A 502 without
them is worth one quick retry; a 422 is not.

A session launch fails when YouTube is unreachable, answers the landing page
without an identity, or refuses attestation, or when the browser itself cannot
set the session up. The launch belongs to the daemon, not to the request that
started it: it runs to its own verdict, within 3 minutes, even after that
caller disconnects, and requests arriving meanwhile wait on it.

A failed launch refuses the requests waiting on it and every request for the
next 15 seconds, without another launch; the first request after that launches
again. Each consecutive failure doubles the wait, up to 5 minutes, and an
attested session resets it. A daemon refused at startup serves the same way
while it retries its warm-up. Once a session has attested, nothing launches
without a request, so an idle daemon that later loses its session reports the
last failure until one arrives.

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

One case skips the envelope: a non-canonical path (with `.`, `..`, or repeated
slashes, such as `//get_pot`) gets `http.ServeMux`'s **307** redirect to its
cleaned form, with a short `text/html` or empty body, so a client that does not
follow redirects must not expect JSON there. A trailing slash is a different
path, so `/get_pot/` gets the JSON **404**.

`/report` rejects an unknown field, such as the typo `raeson` or the case
variant `Reason`, with **400 `invalid-request`** naming the key, so a typo
cannot silently drop an optional field. `/get_pot` ignores unknown fields,
since a generic yt-dlp client sends extras (`proxy`, `bypass_cache`,
`source_address`), and so does `/player-context`. Duplicate keys are accepted
everywhere; the last value wins.

A body that is not a JSON object (`null`, a number, an array, a string) is
rejected with `request body must be a JSON object`, and a body still arriving
when the 30 second read timeout fires with `request body was not received
before the read timeout`. The `client` package parses these errors into
`*client.APIError` with matching code constants.
