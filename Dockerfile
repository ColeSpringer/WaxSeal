# syntax=docker/dockerfile:1
#
# WaxSeal is a real-browser PO-token service. The image includes Chromium and
# drives it through the Chrome DevTools Protocol. Chromium runs with
# --no-sandbox inside the container, so the container boundary provides the
# isolation. The image uses a non-root user, and the compose file drops
# capabilities and disables privilege escalation.

# build
FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
# The RUNs below mount Go's module and build caches so rebuilds reuse them. The
# caches never land in an image layer, and go.sum still verifies downloads.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
# Version stamping: pass `--build-arg VERSION=1.2.3`. ARG must be declared in this
# stage for the RUN to see it.
ARG VERSION=docker
# Disable CGO for a pure Go binary. The embedded JavaScript bundle does not remove
# the runtime dependency on Chromium.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/waxseal ./cmd/waxseal

# runtime
FROM debian:trixie-slim
# Chromium renders WebGL with its own bundled SwiftShader because --disable-gpu is
# set (unconditional in internal/cdp/launch.go), so the Mesa/LLVM stack chromium
# pulls in is never loaded. Purging it drops 272 MB, and the dangling dlopen
# targets it leaves behind have no runtime effect.
#
# The assertion guards the list, which is release specific (bookworm had
# libllvm15, no mesa-libgallium) and fails silently: dpkg --purge exits 0 with a
# warning for a package that is not installed. The globs catch a rename that
# leaves its files, dpkg-query catches a package in any state short of
# not-installed, such as a remove that left a config record. If a build trips
# either, fix the list rather than the assertion.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium fonts-liberation ca-certificates tini \
 && dpkg --purge --force-depends libgl1-mesa-dri mesa-libgallium libllvm19 libz3-4 \
 && for pat in 'libLLVM*.so*' 'libgallium*.so*' 'libz3*.so*'; do \
      if ls /usr/lib/*/$pat >/dev/null 2>&1; then \
        echo "ERROR: $pat survived the purge; the purge list is stale" >&2; \
        ls -d /usr/lib/*/$pat >&2; exit 1; fi; \
    done \
 && if ls -d /usr/lib/*/dri >/dev/null 2>&1; then \
      echo "ERROR: a Mesa dri/ directory survived the purge" >&2; \
      ls -d /usr/lib/*/dri >&2; exit 1; fi \
 && left=$(dpkg-query -W -f '${Package} ${db:Status-Status}\n' \
      'libllvm*' 'mesa-libgallium*' 'libgl1-mesa-dri*' 'libz3-*' 2>/dev/null \
      | awk '$2 != "not-installed" { print $1 }') \
 && if [ -n "$left" ]; then \
      echo "ERROR: still installed after the purge: $left" >&2; exit 1; fi \
 && rm -rf /var/lib/apt/lists/*

# Non-root user with a writable HOME (the browser profile lives under $HOME).
RUN useradd --create-home --uid 10001 waxseal
COPY --from=build /out/waxseal /usr/local/bin/waxseal
# The image redistributes this code, and MIT requires the notice to travel with it.
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/doc/waxseal/

# Link the GHCR package to the source repository, and stamp the version so
# `docker inspect` says which release a pulled image is. The ARG is redeclared
# because the build stage's declaration does not reach this one.
ARG VERSION=docker
LABEL org.opencontainers.image.source="https://github.com/ColeSpringer/WaxSeal" \
      org.opencontainers.image.description="YouTube PO-token service running BotGuard in a real headless Chromium" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"

ENV WAXSEAL_CHROME_BIN=/usr/bin/chromium \
    HOME=/home/waxseal
USER waxseal
EXPOSE 4416

# tini reaps the many short-lived Chromium child processes (PID-1 zombie reaping).
ENTRYPOINT ["/usr/bin/tini", "--", "waxseal"]
CMD ["server", "--host", "0.0.0.0"]

# Use the built-in health probe instead of curl. The start period covers browser
# warm-up, and the timeout covers a lazy attestation. --strict fails only on a
# probe failure: a `POST /report` retires the session and re-establishment is lazy,
# and that benign window must not mark the container unhealthy. The probe sends
# no key on purpose. A keyed daemon (--tenant-keys) answers a keyless /ping with
# the shared browser's health, relaunching a browser that has exited, so this
# works unchanged once the daemon is keyed. Add `--key <key>` to also probe that
# tenant's session; the browser is checked either way.
HEALTHCHECK --interval=30s --timeout=110s --start-period=120s --retries=3 \
  CMD ["waxseal", "ping", "--addr", "127.0.0.1:4416", "--strict"]
