# WaxSeal

WaxSeal is a YouTube **PO token** provider. It runs Google's BotGuard in a real
headless Chromium, which reliably yields tokens with the **integrity** grade,
and serves them from a bgutil-compatible HTTP daemon. A CLI and a Go client
come with it.

## Quick start

The host needs only Docker. This `compose.yaml` is the whole deployment:

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
docker compose up -d --wait   # returns once the daemon is healthy
curl -s localhost:4416/get_pot -d '{"content_binding":"<video_id>"}'
```

```json
{"poToken":"MnRV...","contentBinding":"<video_id>","expiresAt":"2026-07-01T18:00:00Z"}
```

The first token or context request may wait up to 12 seconds more behind the
mint-separation gate ([why](docs/api.md#get-post-player-context)).

Tokens are bound to the daemon's egress IP, so whatever fetches media with them
must leave from the same IP: run it on the same host, or in a container that
shares the daemon's network namespace
([snippet](docs/deployment.md#run-a-consumer-on-the-same-egress-ip)).

`WAXSEAL_VERSION=1.6.0 docker compose up -d --wait` pins a release.
[docs/deployment.md](docs/deployment.md) has the optional extras as paste-in
snippets: API keys from a secret file, a memory limit, an idle timeout, a
read-only root filesystem, `docker run` without compose, and verifying or
building the image.

## HTTP API

| Method | Endpoint | Purpose |
|---|---|---|
| `POST` | `/get_pot` | Mint or retrieve a cached PO token |
| `GET`, `POST` | `/player-context` | Return an attested streaming context for a video |
| `GET` | `/session` | Export the attested guest identity and cookies |
| `POST` | `/report` | Report a degraded stream and recycle its session |
| `GET` | `/ping` | Health check; `?strict=true` answers 503 on a failed probe |
| `GET` | `/metrics` | Operational counters |

```sh
curl -s localhost:4416/get_pot -d '{"content_binding":"<video_id | visitor_data>"}'
curl -s localhost:4416/player-context -d '{"video_id":"<video_id>"}'
curl -s localhost:4416/session
curl -s localhost:4416/ping
curl -s localhost:4416/metrics
```

For `/get_pot`, `content_binding` is a video ID for a player token or visitor
data for a GVS token. [Errors](docs/api.md#errors) are JSON,
`{"error":"...","code":"..."}`, and carry `Retry-After` when the daemon expects
the refusal to lift on its own.

[docs/api.md](docs/api.md) is the full reference: every request and response
field, error code, and metric. Go programs can use the [`client`](client)
package instead of raw HTTP.

## API keys

The daemon is keyless and single-tenant by default. A keyless daemon hands its
guest identity to anyone who can reach it, so set keys before publishing it
beyond loopback. `WAXSEAL_TENANT_KEYS=alice=KEYA,bob=KEYB` gives each key its
own isolated browser context. Send a key as `X-API-Key` or
`Authorization: Bearer <key>`:

```sh
curl -s localhost:4416/get_pot -H "X-API-Key: KEYA" -d '{"content_binding":"<video_id>"}'
```

The compose setup is in
[docs/deployment.md](docs/deployment.md#expose-the-daemon-and-require-api-keys).

## Configuration

The daemon reads these environment variables, and a flag outranks its variable.

| Variable | Flag | Default | Sets |
|---|---|---|---|
| `WAXSEAL_TENANT_KEYS`, `WAXSEAL_TENANT_KEYS_FILE` | `--tenant-keys`, `--tenant-keys-file` | none | API keys, inline or in a file |
| `WAXSEAL_METRICS_KEY`, `WAXSEAL_METRICS_KEY_FILE` | `--metrics-key`, `--metrics-key-file` | none | Operator key for per-tenant `/metrics` detail on a keyed daemon |
| `WAXSEAL_STREAMING_MAX_AGE` | `--streaming-max-age` | `45m` | Session age that triggers a recycle; `0` or a blank value disables |
| `WAXSEAL_REPORT_DEBOUNCE` | `--report-debounce` | `5m` | Spacing between recycles driven by `/report` |
| `WAXSEAL_IDLE_TIMEOUT` | `--idle-timeout` | off | Time without use after which the browser closes to free its memory; minimum `1m` |
| `WAXSEAL_SHUTDOWN_TIMEOUT` | `--shutdown-timeout` | `60s` | How long a stopping daemon drains in-flight requests |
| `WAXSEAL_MINT_SEPARATION` | none | `12s` | Spacing kept between a token mint, a streaming proof, and a served context |
| `WAXSEAL_CHROME_BIN` | none | auto-detected | The browser binary, for every command |
| `WAXSEAL_UA_HINTS` | none | `real` | Client-hint source, `real` or `synthetic` |

`--host`, `--port`, `--video`, and `--metrics-public` are flags only;
`waxseal server --help` describes every setting. In a container a flag means
overriding the image's command
([how](docs/deployment.md#tune-the-daemon)).

## Without Docker

Each [release](https://github.com/ColeSpringer/WaxSeal/releases) carries
binaries for Linux, macOS, and Windows. They bundle no browser and use the
system's Chromium or Chrome. From a checkout, with Go:

```sh
go run ./cmd/waxseal server                     # the daemon, on 127.0.0.1:4416
go run ./cmd/waxseal -c <content_binding>       # one-shot token
go run ./cmd/waxseal player-context <video_id>  # one-shot streaming context
go run ./cmd/waxseal doctor                     # report identity and token grade
go run ./cmd/waxseal ping                       # check a running daemon
```

The token, `player-context`, and `doctor` commands each launch their own
browser, so prefer the daemon for repeated requests.
[docs/operations.md](docs/operations.md) covers startup, health checks,
shutdown, and the CLI.

## Development

```sh
make test   # offline tests for both modules, race-enabled; no browser or network
make vet    # both modules, plus a Windows cross-vet
make live   # tests that drive a local Chromium; no network
```

[docs/development.md](docs/development.md) has the rest: the e2e tests, the
browser bundle, and how `provider/` pins the root module.

## License

MIT. Implemented independently. The GPL-3.0 bgutil project is a behavioral and
wire reference only. See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
