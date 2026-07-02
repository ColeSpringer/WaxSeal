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

## HTTP API

| Method | Endpoint | Purpose |
|---|---|---|
| `POST` | `/get_pot` | Mint or retrieve a cached PO token |
| `GET`, `POST` | `/player-context` | Return an attested streaming context |
| `GET` | `/session` | Export the attested guest identity and cookies |
| `POST` | `/report` | Report a degraded stream and recycle its session |
| `GET` | `/ping` | Check the current session without minting |
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
when served from the cache or `miss` when freshly minted.

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

### `GET /session`

Exports the guest identity for the session-adoption path (`--session-url` plus
`--potoken-url`), after verifying full-length streaming. No request body, and no
Google login.

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
rate-limited per tenant: after a report-driven recycle, another report within
`--report-debounce` (default `5m`) is rejected with `retry_after_seconds`, and
stale or future generations are ignored.

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
(applied to the live session), `degradation_reports_rate_limited` (within the
debounce window), `degradation_reports_rejected_stale` (an old or replaced
generation), and `degradation_reports_already_retired` (the current generation,
already retired by a crash or a prior report; a benign no-op).

### Authentication and tenants

The daemon is keyless and single-tenant by default. Pass `--tenant-keys` to run
isolated browser contexts keyed by API key:

```sh
go run ./cmd/waxseal server --tenant-keys "alice=KEYA,bob=KEYB"
curl -s localhost:4416/get_pot -H "X-API-Key: KEYA" -d '{"content_binding":"<id>"}'
```

Keys travel in `X-API-Key`, `Authorization: Bearer <key>`, or `?key=<key>`.
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

The full view is `{"tenants":N,"per_tenant":{"<label>":{...}}}`, each tenant
object carrying lifetime counters (`mints`, `crashes`, `player_contexts`, the
four `degradation_reports_*` dispositions above, and so on) plus current state.
Detail fields are always present so the schema stays stable across retirement,
crash, and recycle; a field that does not apply is `null` or `""` rather than
omitted. For example `last_browser_proof_age_secs` is `null` until the first
proof, which reserves `0` for "just proved", and `streaming_seconds_until_recycle`
appears only when time-based recycling is enabled (`--streaming-max-age` > 0). The
redacted view is `{"redacted":true,"aggregate":{...}}`: the same counters summed
across tenants, with no labels and no tenant count.

### Errors

Recognized endpoints and unknown paths return
`{"error":"<message>","code":"<machine-readable-code>"}`. `video-unavailable`
adds a `details` field with the playability status. `/ping` never uses this
envelope; it reports health directly (see [Operations](#operations)).

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
failed health probe, not retirement from age, a report, or operation retries.
`--report-debounce` (default `5m`) throttles all report-driven recycles for a
tenant across generations, not just repeats of one generation; this is deliberate
anti-storm behavior, and workloads with legitimately bursty degradations may
lower it.

Health checks use `/ping`, which after authentication returns HTTP 200 with
`ok:true` or `ok:false` and an always-present `reason`: `ok`, `no-session`
(benign, since a `POST /report` retires the session and re-establishment is lazy,
so `ok` briefly reads `false`), or `probe-failed` (a live session's probe failed,
logged at `warn`). Alert only on `probe-failed`; a caller that disconnects
mid-probe is not counted as one. For status-code-only checks (k8s, `curl -f`,
HAProxy), `?strict=true` maps `probe-failed` to **503** while `no-session` and
healthy stay **200**, and `waxseal ping --strict` does the same from the CLI, so
liveness probes do not fail during the benign re-establishment window.

WaxSeal is meant for loopback or a trusted network and does not implement CORS;
because it mints tokens, browser-origin access is out of scope. Run
`go run ./cmd/waxseal server --help` for the rest: session recycling, report
debounce, bind address, headful mode, and metrics access.

## Development

```sh
go test ./...                              # offline unit tests; no browser or network
go test -tags live ./internal/cdp          # real-Chromium CDP pipe-transport tests
(cd provider && go test -tags e2e ./...)   # provider network e2e; needs WAXSEAL_URL/WAXSEAL_KEY
make deps                                  # install browser-bundle build dependencies
make jsbundle-browser                      # regenerate internal/browser/bg_browser_bundle.js
```

`go test ./...` is fully offline and deterministic: no browser, no network. The
committed browser bundle means normal builds do not need Node. The live CDP tests
self-skip when no browser is found (`WAXSEAL_CHROME_BIN` picks one,
`WAXSEAL_REQUIRE_CHROME=1` fails instead of skipping, which CI sets). The `e2e`
tests live in the nested `provider/` module and must run from that directory,
since a root-level `go test -tags e2e ./...` silently descends into nothing; they
need a warm daemon and include the full-length WEB SABR download
(`go test -tags e2e -run PlayerContextOnlyFullLength ./...`). The `client` package
is a reusable, WaxTap-free HTTP client; the `provider/` module adapts it to
WaxTap's `potoken.Provider` interface.

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
