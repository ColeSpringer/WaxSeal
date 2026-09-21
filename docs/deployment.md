# Deploying WaxSeal

The `compose.yaml` at the repository root is the whole deployment: one service,
already hardened, publishing the daemon on loopback. Copy the file anywhere
and run

```sh
docker compose up -d --wait
```

`--wait` returns once the image's healthcheck passes, which is when the daemon
has attested a session and can serve tokens, typically under ten seconds after
Chromium starts. The first token or context request may still wait up to 12
seconds behind the daemon's mint-separation gate (README, `/player-context`).
`docker compose logs -f` follows startup, and `docker compose ps` shows the
health state. A container that never turns healthy is either
restarting, because the daemon exited on a startup failure, or up and unhealthy,
because its probe keeps failing; the log says which. The daemon keeps no state
worth a volume: browser profiles are temporary and rebuilt at every start.

Everything below is optional. Unless a section says otherwise, its snippet is
pasted into that file under the `waxseal:` service, at the indentation shown,
and none of them needs a second compose file.

## Pin a release

The file pulls `:latest`. `WAXSEAL_VERSION` picks a tag instead, on the command
line or in a `.env` file beside `compose.yaml`, which compose reads on its own:

```sh
WAXSEAL_VERSION=1.4.1 docker compose up -d --wait
```

```sh
# .env
WAXSEAL_VERSION=1.4.1
```

The release pipeline publishes each tag as a manifest covering linux/amd64 and
linux/arm64, so Docker resolves the right architecture, and keeps the
per-architecture tags it is assembled from (`<version>-amd64`,
`<version>-arm64`) pullable for pinning one platform. Each image tag and each
release binary carries a GitHub build provenance attestation naming the commit
and workflow run that built it, and the image's
`org.opencontainers.image.version` label, in `docker inspect`, names the
release. Releases up to and including 1.3.0 predate that pipeline: they are
linux/amd64 only, with no per-architecture tags, no attestation, and no version
label, and on an arm64 host `:latest` resolves to that amd64 image until a newer
release moves it.

```sh
gh attestation verify oci://ghcr.io/colespringer/waxseal:<version> --repo ColeSpringer/WaxSeal
gh attestation verify waxseal-linux-amd64 --repo ColeSpringer/WaxSeal   # a downloaded release binary
```

## Expose the daemon and require API keys

A keyless daemon hands its guest identity to anyone who can reach `/session` or
`/player-context`, so publishing on every interface, or on a LAN address, goes
together with API keys. Each key gets its own isolated browser context, and a
consumer sends its key as `X-API-Key` or `Authorization: Bearer <key>`. This
one replaces the file's `ports:` block rather than adding to it, since a
service cannot carry two:

```yaml
    ports:
      - "0.0.0.0:4416:4416"
    environment:
      WAXSEAL_TENANT_KEYS: alice=KEYA,bob=KEYB
```

Compose interpolates `$` in that value, so `alice=aB3$xK9q` reaches the daemon
as `alice=aB3`, with nothing but a warning on stderr to show for it. Write `$$`
for each `$`, or use the file form below, which compose never interpolates. The
image's healthcheck needs no change either way: a keyed daemon answers the
keyless probe with the shared browser's liveness rather than `401`.

A value under `environment` shows in `docker inspect`. A file does not, and the
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

Compose mounts that host file at `/run/secrets/waxseal_tenant_keys` as it is, so
outside swarm it has to be readable by the image's non-root user, uid 10001:
`chmod 0444 tenant-keys`, or chown it to that uid. The file holds what the
variable would, `alice=KEYA,bob=KEYB`, one entry per line also works, with
surrounding whitespace ignored. An empty file stops startup rather than starting
a keyless daemon, and a non-empty value beside the `_FILE` variant is a startup
error rather than a silent winner; a blank or whitespace-only value counts as
unset.

### Metrics on a keyed daemon

A keyed daemon serves an unauthenticated `/metrics` scrape a redacted aggregate
without tenant labels. `WAXSEAL_METRICS_KEY`, or `WAXSEAL_METRICS_KEY_FILE` with
the same secrets pattern, sets an operator key that unlocks the full per-tenant
detail; it must differ from every tenant key. A keyless daemon already serves
the full detail.

## Limit memory

Chromium's memory use is spiky, and one browser hosts a context per tenant, so
the file sets no limit. To cap the container, size the limit to the tenant
count and the load. A limit set too low kills Chromium under load, which the
daemon counts in `crashes` and recovers from by relaunching it, or kills the
daemon itself, which restarts the container:

```yaml
    deploy:
      resources:
        limits:
          memory: 2g
    memswap_limit: 2g # equal to the limit; without it Docker grants the same amount of swap on top
```

`memswap_limit` is a service-level key, not part of `deploy.resources.limits`,
which is why it sits at the service indentation. On a host with swap, leaving it
out means the "kills Chromium" behaviour happens at twice the limit rather than
at it. The `docker run` equivalent is `--memory 2g --memory-swap 2g`.

## Read-only root filesystem

Chromium needs a writable home for its profile and a writable `/tmp`, so a
read-only rootfs mounts both as tmpfs:

```yaml
    read_only: true
    tmpfs:
      - /home/waxseal:mode=1777
      - /tmp:mode=1777
```

The `mode` is load-bearing: a bare tmpfs mounts root-owned, the non-root user
cannot create its profile under `/home/waxseal`, and the container loops on
`permission denied`. `docker run --tmpfs /home/waxseal:mode=1777 --tmpfs
/tmp:mode=1777` is the equivalent.

## Run a consumer on the same egress IP

A PO token is bound to the egress IP of the host that minted it, so an
application that fetches media with WaxSeal's tokens has to leave the network
from the same address. A consumer running on the host itself already does, and
reaches the daemon through the published port. A consumer that runs as its own
container shares the daemon's network namespace instead, which guarantees the
same address on any host and puts the daemon at `127.0.0.1:4416` for it. This
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
that serves only that consumer needs no port on the host at all, in which case
delete the `ports:` block from `waxseal`.

## Tune the daemon

The daemon reads its settings from the environment, and the README's
[Operations](../README.md#operations) section lists every variable with the
flag it stands in for. They go under `environment:` the way the keys do above.
One of them touches the file: `WAXSEAL_SHUTDOWN_TIMEOUT` is how long a stopping
daemon drains in-flight requests, 60s by default, and `stop_grace_period` is
70s to cover it. Raise them together, or Docker kills the container mid-request.
`WAXSEAL_MINT_SEPARATION` is read the same way and must parse as a positive Go
duration, or the daemon refuses to start.

## Health checks

The image declares its own healthcheck, `waxseal ping --strict` against the
daemon on loopback every 30 seconds, with a 110 second timeout and a two minute
start period, so compose, `docker ps`, and `--wait` all see the daemon's health
with nothing added to the file. An orchestrator that probes over HTTP uses
`GET /ping?strict=true`: 200 while the daemon is healthy or inside the benign
window after a session was retired, 503 only once a probe has confirmed the
session or the browser lost. Give such a probe the same generous timeout, since a
probe that finds a wedged browser tears it down and relaunches it before
answering. The README's Operations section has the full account, including what
a probe on a keyed daemon checks.

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
the local build rather than pulling. `make docker-smoke` builds it and proves it
can start Chromium and render a page on an isolated network, the same check the
release runs before it publishes.
