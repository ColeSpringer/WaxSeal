# WaxSeal

A YouTube **PO Token (POT)** provider that runs Google's **BotGuard** in a real
headless Chromium through [go-rod](https://github.com/go-rod/rod). WaxSeal
handles attestation, token binding, caching, multi-tenancy, and its HTTP and CLI
interfaces in Go.

BotGuard can inspect the browser's real navigator instead of an emulated
JavaScript environment. This reliably earns the **integrity** token grade.
WaxSeal runs as a bgutil-compatible HTTP daemon or a CLI.

> **Requires a system Chromium at runtime** (auto-detected, or set
> `WAXSEAL_CHROME_BIN`). The Go binary cross-compiles normally, but it is not
> self-contained because it drives an external browser.

## Layout

```
client/              Reusable, WaxTap-free Go client for the HTTP API
cmd/waxseal/         CLI commands
server/              HTTP daemon and wire protocol
internal/browser/    Chromium sessions, contexts, and embedded browser bundle
internal/minter/     Token caching, retries, recovery, and tenant routing
internal/botguard/   Challenge parsing, GenerateIT, and token validation
internal/innertube/  InnerTube challenge and guest identity requests
internal/httpx/      Shared HTTP retries, backoff, and response limits
build/js/            Source and build script for the embedded browser bundle
provider/            WaxTap provider adapter in a separate Go module
```

Any Go application can use the `client` package to call a WaxSeal daemon:
`client.New(url).POToken(ctx, contentBinding, scope)` and `.Session(ctx)`. The
`provider/` module maps that client to WaxTap's `potoken.Provider` interface.

## Build and test

```
go build ./...          # no Node toolchain needed; the browser bundle is committed
go test ./...           # offline unit tests
make deps               # install dependencies used to rebuild the browser bundle
make jsbundle-browser   # regenerate internal/browser/bg_browser_bundle.js
```

## Run

```
# HTTP daemon (defaults to loopback 127.0.0.1:4416). Warms a browser at startup.
go run ./cmd/waxseal server
curl -s localhost:4416/get_pot -d '{"content_binding":"<videoID>"}'   # returns {"poToken",...}
curl -s localhost:4416/player-context -d '{"video_id":"<videoID>"}'   # returns a status-1 streaming context
curl -s localhost:4416/session                                        # returns visitor_data and cookies
curl -s localhost:4416/ping                                           # health check that never mints
curl -s localhost:4416/metrics                                        # per-tenant counters

# One-shot generation for bgutil script-provider integrations. This launches a
# fresh browser for every call. Prefer the warm server for yt-dlp.
go run ./cmd/waxseal -c <content_binding>

# Launch a browser, attest, and report the identity and token grade.
go run ./cmd/waxseal doctor

# Check a running daemon. The command exits nonzero when the daemon is unhealthy.
go run ./cmd/waxseal ping
```

The daemon runs Chromium under go-rod's embedded *leakless* process guard. If
WaxSeal terminates without running normal cleanup, such as after `SIGKILL`, a
crash, or an OOM kill, leakless terminates Chromium's process group. On the next
startup, WaxSeal removes abandoned profile directories that it can identify
safely. A directory must match `~/.waxseal-<digits>`, contain WaxSeal's marker,
and have an unlocked marker file. Locked, unmarked, and unrelated paths are left
in place. This startup cleanup is disabled on Windows because it relies on
advisory file locks. Normal `SIGTERM`, `SIGINT`, and `docker stop` shutdowns
remove profile directories during shutdown.

Leakless stores its guard in `os.TempDir()/leakless-<arch>-<version>/` and
executes it from there. On Unix, WaxSeal creates the directory with mode `0700`
when it does not exist, then rejects symlinks, paths owned by another user, and
paths writable by a group or other users. On a **shared multi-user host**, set
`TMPDIR` to a private directory. If `/tmp` is mounted `noexec`, use a writable
location on a filesystem that permits execution:
`TMPDIR=$HOME/tmp waxseal server`.

`content_binding` identifies what the token is bound to. Use a **video_id** for a
player token or **visitor_data** for a GVS token. The token is also bound to the
minting host's egress IP. The consumer must use the **same IP** for the SABR media
request.

The optional `scope` may be **`player`** or **`gvs`**. If omitted, WaxSeal uses
the generic, bgutil-compatible cache key. The `content_binding` determines the
token type; `scope` only distinguishes cache entries. An unknown scope returns
`400 invalid-request`.

## Multi-tenant

One Chromium hosts an isolated incognito **browser context** for each tenant.
Each context has its own guest identity and cookies. API keys select the tenant:

```
go run ./cmd/waxseal server --tenant-keys "alice=KEYA,bob=KEYB"
curl -s localhost:4416/get_pot -H "X-API-Key: KEYA" -d '{"content_binding":"<id>"}'
```

Without `--tenant-keys`, the daemon is **keyless single-tenant** so generic
yt-dlp integrations can use the bgutil protocol without authentication. Send a
tenant key as `X-API-Key`, `Authorization: Bearer <key>`, or `?key=<key>`. All
tenant contexts currently use the host's egress IP.

## Player context (`/player-context`)

`POST /player-context {"video_id":"<id>"}` (or `?video_id=<id>`) returns the
attested browser's **status-1** streaming context for a video. The response
includes the `server_abr_streaming_url`, ustreamer config, visitor data, client
version, player URL, and audio formats. The consumer must use `player_url` to
descramble the throttling nonce in the SABR URL. Each audio format includes the
`itag`, `lmt`, and `xtags` values needed to select that exact format.

WaxSeal obtains the context from the browser. The consumer performs the SABR
streaming request.

## Coherence handoff (`/session`)

`GET /session` exports the tenant context's anonymous `visitor_data` and cookies.
A consumer can adopt that identity and pair it with a `/get_pot` token bound to
the same `visitor_data`. The consumer must also use the same egress IP.

A matching token, session, and IP can still receive `STREAM_PROTECTION` status 2,
which limits playback to about 70 seconds. Use `/player-context` when the consumer
needs a status-1 context. Exported sessions do not contain a Google login.

## Error responses

Every error response from a recognized endpoint uses a JSON envelope with a
stable machine-readable `code` and a human-readable `error`, so consumers can
branch on the code without parsing the message. This includes the `405` returned
when an endpoint is called with an unsupported HTTP method:

```json
{
  "error": "waxseal: video unplayable (playabilityStatus \"LOGIN_REQUIRED\")",
  "code": "video-unavailable",
  "details": "LOGIN_REQUIRED"
}
```

`details` is optional. For `video-unavailable`, it contains the bare
`playabilityStatus`.

| `code` | HTTP | Meaning |
|---|---|---|
| `unauthorized` | 401 | Missing or invalid API key |
| `method-not-allowed` | 405 | Unsupported HTTP method for the endpoint |
| `invalid-request` | 400 | Malformed JSON or a missing or invalid field |
| `mint-failed` | 502 | Token mint failed |
| `video-unavailable` | 422 | Terminal `playabilityStatus`; `details` contains the status |
| `timeout` | 504 | Player-context deadline elapsed |
| `player-context-failed` | 502 | Other player-context failure |
| `no-session` | 503 | No attested session or cookies available |

`GET /ping` is the exception to this contract. It always returns 200 and reports
failures as `{ok:false,error}`. Unsupported methods on `/ping` or `/metrics`
still return the `405` envelope. Unknown paths use net/http's plain-text 404
response.

The `waxseal/client` package parses these envelopes into `*client.APIError` and
provides matching `Code*` constants. It also accepts the older `{error}` format
and non-JSON proxy responses.

## License

MIT. Implemented independently; the GPL-3.0 bgutil project is a behavioral/wire
reference only (no code copied). See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
