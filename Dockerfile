# syntax=docker/dockerfile:1
#
# Chromium runs with --no-sandbox, so the container is the isolation boundary:
# the image uses a non-root user, and the compose file drops capabilities and
# disables privilege escalation.

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
# A static, pure Go binary; it still needs Chromium at runtime.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/waxseal ./cmd/waxseal

# runtime
FROM debian:trixie-slim
# --disable-gpu (always set in internal/cdp/launch.go) makes Chromium render
# WebGL with its bundled SwiftShader, so the Mesa/LLVM stack chromium pulls in
# is never loaded. Purging it saves 272 MB; the dlopen targets it leaves
# dangling are never used. The list is release specific and dpkg --purge exits
# 0 for a package that is not installed, so the checks after it fail the build
# when the list goes stale. Fix the list, not the checks.
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

# tini, as PID 1, reaps Chromium's many short-lived child processes.
ENTRYPOINT ["/usr/bin/tini", "--", "waxseal"]
CMD ["server", "--host", "0.0.0.0"]

# The built-in probe replaces curl. The start period covers browser warm-up, and
# the timeout outlasts ping's own budget (TestImageHealthcheckOutlastsPing).
# --strict fails only on a probe failure, not in the benign window after a
# `POST /report` retires the session. The probe sends no key and still works on
# a keyed daemon (--tenant-keys), which reports the shared browser's health and
# relaunches an exited one; add `--key <key>` to also probe a tenant's session.
HEALTHCHECK --interval=30s --timeout=110s --start-period=120s --retries=3 \
  CMD ["waxseal", "ping", "--addr", "127.0.0.1:4416", "--strict"]
