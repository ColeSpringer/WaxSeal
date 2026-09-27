# Deploying WaxSeal

The `compose.yaml` at the repository root is the whole deployment: one service,
already hardened, publishing the daemon on loopback. Copy the file anywhere
and run

```sh
docker compose up -d --wait
```

`--wait` returns once the image's healthcheck passes and the daemon can serve
tokens, typically within ten seconds of Chromium starting. The first token or
context request may still wait up to 12 seconds behind the mint-separation gate
(README, `/player-context`).

`docker compose logs -f` follows startup, and `docker compose ps` shows health.
A container that never turns healthy is either restarting (the daemon exited on
a startup failure) or up and unhealthy (its probe keeps failing); the log says
which. The daemon needs no volume: browser profiles are temporary and rebuilt
at every start.

Everything below is optional. Unless a section says otherwise, paste its
snippet into that file under the `waxseal:` service at the indentation shown;
none needs a second compose file.

## Pin a release

The file pulls `:latest`. `WAXSEAL_VERSION` picks a tag instead, on the command
line or in a `.env` file beside `compose.yaml`, which compose reads on its own:

```sh
WAXSEAL_VERSION=1.5.1 docker compose up -d --wait
```

```sh
# .env
WAXSEAL_VERSION=1.5.1
```

Each release tag is a manifest covering linux/amd64 and linux/arm64, so Docker
pulls the right architecture; `<version>-amd64` and `<version>-arm64` pin one
platform.

Each image tag and release binary carries a GitHub build provenance attestation
naming the commit and workflow run that built it, and the image's
`org.opencontainers.image.version` label (see `docker inspect`) names the
release. Releases through 1.3.0 predate this: linux/amd64 only, with no
per-architecture tags, attestation, or version label.

```sh
gh attestation verify oci://ghcr.io/colespringer/waxseal:<version> --repo ColeSpringer/WaxSeal
gh attestation verify waxseal-linux-amd64 --repo ColeSpringer/WaxSeal   # a downloaded release binary
```

## Expose the daemon and require API keys

A keyless daemon hands its guest identity to anyone who can reach `/session` or
`/player-context`, so publish it on all interfaces or a LAN address only with
API keys. Each key gets its own isolated browser context; a consumer sends it as
`X-API-Key` or `Authorization: Bearer <key>`. This snippet replaces the file's
`ports:` block (a service cannot carry two):

```yaml
    ports:
      - "0.0.0.0:4416:4416"
    environment:
      WAXSEAL_TENANT_KEYS: alice=KEYA,bob=KEYB
```

Compose interpolates `$` in that value, so `alice=aB3$xK9q` reaches the daemon
as `alice=aB3`, with only a warning on stderr. Write `$$` for each `$`, or use
the file form below, which compose never interpolates. The image's healthcheck
needs no change: a keyed daemon answers its keyless probe with the browser's
liveness, not `401`.

A value under `environment` shows in `docker inspect`; a file does not. The
`_FILE` variant names a path the daemon reads the keys from. To use it, swap the
`environment:` block above for this one and give the service the secret:

```yaml
    environment:
      WAXSEAL_TENANT_KEYS_FILE: /run/secrets/waxseal_tenant_keys
    secrets:
      - waxseal_tenant_keys
```

Then define the secret at the top level of the file, beside `services:` rather
than under it:

```yaml
secrets:
  waxseal_tenant_keys:
    file: ./tenant-keys
```

Outside swarm, compose mounts the host file as it is and ignores a secret's
`uid`, `gid`, and `mode`, so the image's non-root user (uid 10001) must be able
to read it: `chmod 0444 tenant-keys`, or chown it to that uid.

The file holds what the variable would, `alice=KEYA,bob=KEYB` or one entry per
line, with surrounding whitespace ignored. An empty file, or one with no
entries, stops startup rather than starting a keyless daemon. Setting
`WAXSEAL_TENANT_KEYS` as well is a startup error, unless it is blank, which
counts as unset.

### Metrics on a keyed daemon

A keyed daemon serves an unauthenticated `/metrics` scrape a redacted aggregate
without tenant labels. `WAXSEAL_METRICS_KEY`, or `WAXSEAL_METRICS_KEY_FILE` with
the same secrets pattern, sets an operator key that unlocks the full per-tenant
detail; it must differ from every tenant key. A keyless daemon already serves
the full detail.

## Limit memory

Chromium's memory use is spiky and grows with the tenant count, so the file sets
no limit. Size a cap to the tenants and the load: too low a limit kills Chromium
under load, which the daemon counts in `crashes` and relaunches, or kills the
daemon itself, which restarts the container:

```yaml
    deploy:
      resources:
        limits:
          memory: 2g
    memswap_limit: 2g # equal to the limit; without it Docker grants the same amount of swap on top
```

`memswap_limit` is a service-level key, not part of `deploy.resources.limits`,
so it sits at the service indentation. On a host with swap, leaving it out moves
the kill point from the limit to twice the limit. The `docker run` equivalent is
`--memory 2g --memory-swap 2g`.

## Read-only root filesystem

Chromium needs a writable home for its profile and a writable `/tmp`, so a
read-only rootfs mounts both as tmpfs:

```yaml
    read_only: true
    tmpfs:
      - /home/waxseal:mode=1777
      - /tmp:mode=1777
```

The `mode` matters: a bare tmpfs mounts root-owned, so the non-root user cannot
create its profile under `/home/waxseal` and the container loops on `permission
denied`. `docker run --tmpfs /home/waxseal:mode=1777 --tmpfs /tmp:mode=1777` is
the equivalent.

## Run a consumer on the same egress IP

A PO token is bound to the minting host's egress IP, so an application that
fetches media with WaxSeal's tokens must leave from the same address. A consumer
on the host already does and reaches the daemon through the published port. A
containerized consumer shares the daemon's network namespace, which guarantees
the same address on any host and puts the daemon at `127.0.0.1:4416`. This
snippet is a second service, beside `waxseal:` rather than under it:

```yaml
  consumer:
    image: your/image:tag # the application that calls the daemon
    network_mode: service:waxseal
    depends_on:
      waxseal:
        condition: service_healthy
    # Point the consumer at the daemon on loopback, in whatever setting it
    # reads. The endpoints are /get_pot, /player-context, and /session.
    # environment:
    #   POTOKEN_URL: http://127.0.0.1:4416
```

The consumer starts once the daemon's healthcheck passes. It has no network of
its own, so any port it needs is published on the `waxseal` service. A daemon
that serves only that consumer needs no host port at all: delete the `ports:`
block from `waxseal`.

## Tune the daemon

Settings go under `environment:` like the keys above; the README's
[Operations](../README.md#operations) section lists every variable and the flag
it stands in for. `WAXSEAL_SHUTDOWN_TIMEOUT` (default 60s) is how long a
stopping daemon drains in-flight requests, and the file's `stop_grace_period`
of 70s covers it: raise both together, or Docker kills the container
mid-request. `WAXSEAL_MINT_SEPARATION` must be a positive Go duration, or the
daemon refuses to start.

## Health checks

The image's own healthcheck runs `waxseal ping --strict` on loopback every 30
seconds, with a 110 second timeout and a two minute start period, so compose,
`docker ps`, and `--wait` see the daemon's health with nothing added to the
file.

An orchestrator probing over HTTP uses `GET /ping?strict=true`: 200 while the
daemon is healthy or in the benign window after a session is retired, 503 once
a probe confirms the session or the browser lost. Give it the same generous
timeout, since a probe that finds a wedged browser relaunches it before
answering. The README's [Operations](../README.md#operations) section has the
details, including what a probe on a keyed daemon checks.

## Upgrade, follow, stop

```sh
docker compose pull && docker compose up -d --wait   # move to the newest image the tag resolves to
docker compose logs -f                               # follow the daemon
docker compose down                                  # stop; in-flight requests get stop_grace_period
```

## Without compose

The same container from `docker run`, setting for setting:

```sh
docker run -d --name waxseal --restart unless-stopped \
  -p 127.0.0.1:4416:4416 --shm-size 1g --stop-timeout 70 \
  --cap-drop ALL --security-opt no-new-privileges \
  --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 \
  ghcr.io/colespringer/waxseal:latest
```

## Build the image yourself

`make docker-build` builds the image from the checkout and tags it under the
published name, `:latest` included, so `docker compose up` on that machine runs
the local build instead of pulling. `make docker-smoke` builds it and proves it
can start Chromium and render a page on an isolated network, the check the
release runs before it publishes.
