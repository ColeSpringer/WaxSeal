# syntax=docker/dockerfile:1
#
# WaxSeal is a real-browser PO-token service. The image includes Chromium and
# drives it through the Chrome DevTools Protocol. Chromium runs with
# --no-sandbox inside the container, so the container boundary provides the
# isolation. The image uses a non-root user, and the compose files drop
# capabilities and disable privilege escalation.

# build
FROM golang:1.26-bookworm AS build
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
FROM debian:bookworm-slim
# Chromium renders WebGL with its own bundled SwiftShader because --disable-gpu is
# set, so the system Mesa/LLVM software-GL stack is never loaded at runtime.
# chromium-common declares those packages as a dependency, so force-purge them after
# the install to drop ~158 MB. The daemon never runs apt again, so the unmet-
# dependency note this leaves in the dpkg database has no runtime effect.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium fonts-liberation ca-certificates tini \
 && dpkg --purge --force-depends libgl1-mesa-dri libllvm15 libz3-4 \
 && rm -rf /var/lib/apt/lists/*

# Non-root user with a writable HOME (the browser profile lives under $HOME).
RUN useradd --create-home --uid 10001 waxseal
COPY --from=build /out/waxseal /usr/local/bin/waxseal

# Link the GHCR package to the source repository.
LABEL org.opencontainers.image.source="https://github.com/ColeSpringer/WaxSeal" \
      org.opencontainers.image.description="YouTube PO-token service running BotGuard in a real headless Chromium" \
      org.opencontainers.image.licenses="MIT"

ENV WAXSEAL_CHROME_BIN=/usr/bin/chromium \
    HOME=/home/waxseal
USER waxseal
EXPOSE 4416

# tini reaps the many short-lived Chromium child processes (PID-1 zombie reaping).
ENTRYPOINT ["/usr/bin/tini", "--", "waxseal"]
CMD ["server", "--host", "0.0.0.0"]

# Use the built-in health probe instead of curl. The start period covers browser
# warm-up, and the timeout covers a lazy attestation. Multi-tenant deployments
# must add `--key <key>`.
HEALTHCHECK --interval=30s --timeout=110s --start-period=120s --retries=3 \
  CMD ["waxseal", "ping", "--addr", "127.0.0.1:4416"]
