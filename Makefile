# WaxSeal build orchestration.
#
# WaxSeal mints YouTube PO tokens from a real headless Chromium, driven through
# the Chrome DevTools Protocol by internal/cdp.
# Node and esbuild produce the browser bundle embedded in internal/browser. The
# bundle is committed, so `go build` and `go test` do not need Node. The CLI and
# daemon still require Chromium at runtime.

VERSION           ?= dev
DIST              := dist
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

BROWSER_BUNDLE_OUT := internal/browser/bg_browser_bundle.js

REGISTRY    ?= ghcr.io
IMAGE_OWNER ?= colespringer
IMAGE       := $(REGISTRY)/$(IMAGE_OWNER)/waxseal

# ARCH names the platform docker-build tags for. dpkg prints Docker's own arch
# names (amd64, arm64) on Debian and Ubuntu, which is where the release job
# builds; a dev box without dpkg falls through to Go, whose GOARCH values agree
# for the two architectures this project publishes. The release image job has no
# setup-go step, so it works today only because the Ubuntu runners happen to ship
# Go: dpkg is what makes that incidental dependency unnecessary.
ARCH ?= $(shell dpkg --print-architecture 2>/dev/null || go env GOARCH)

# ARCHES lists the platforms docker-manifest assembles the multi-arch tag from.
# Each one is built and smoke-tested on its own native runner first; no QEMU is
# involved.
ARCHES ?= amd64 arm64

# PUSH_LATEST controls whether docker-manifest moves the :latest tag. It is the
# manifest that owns :latest, because the plain tag must resolve to every
# architecture; docker-push handles one arch and never touches it. The default
# publishes only VERSION. Set PUSH_LATEST=1 for a release that should also become
# :latest.
PUSH_LATEST ?= 0

.PHONY: all help fmt-check vet test live jsbundle-browser verify-assets release deps clean \
        docker-build docker-login docker-push docker-manifest docker-manifest-authed \
        release-guard

all: jsbundle-browser

# help lists the common targets; run `make help` to print it.
help:
	@echo "WaxSeal make targets:"
	@echo "  fmt-check         fail if any file needs gofmt (covers provider/ too)"
	@echo "  vet               go vet the root and provider/ modules, plus a windows cross-vet"
	@echo "  test              offline Go test suite, race-enabled (root + provider/)"
	@echo "  live              real-Chromium CDP transport tests (needs a browser)"
	@echo "  jsbundle-browser  rebuild the embedded browser bundle (needs Node)"
	@echo "  verify-assets     rebuild the bundle in a temp dir, fail if the checked-in one differs"
	@echo "  release           build Linux/macOS/Windows amd64+arm64 binaries into $(DIST)/"
	@echo "  docker-build      build the runtime image for this host's arch (VERSION=x.y.z to tag a release)"
	@echo "  docker-push       publish this host's per-arch tag to $(REGISTRY)"
	@echo "  docker-manifest   assemble $(IMAGE):VERSION from the per-arch tags (PUSH_LATEST=1 also moves :latest)"
	@echo "  deps              install the Node toolchain for the bundle"
	@echo "  clean             remove build output"

# fmt-check fails when any file needs gofmt. `gofmt -l` exits 0 even when it
# lists files, so the output itself is the signal.
fmt-check:
	@out=$$(gofmt -l . 2>&1); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# vet runs what CI vets, in one target: the root module, the nested provider/
# module including its e2e-tagged files, and a windows cross-vet. The Windows job
# in CI compiles and runs those files for real; the cross-vet here is the cheap
# local check that catches a break before a push.
vet:
	go vet ./...
	GOOS=windows go vet ./...
	cd provider && go vet ./... && go vet -tags e2e ./...

# test runs the offline suite: the root module with the race detector (matching
# CI), then the nested provider/ module. The committed bundle means it does not
# need Node. The -tags e2e suite needs network and a warm daemon; the README
# documents running it separately.
test: fmt-check
	go test -race ./...
	cd provider && go test -race ./...

# live runs the real-Chromium CDP transport tests. They drive a local browser
# over the pipe transport and need no network; set WAXSEAL_CHROME_BIN when the
# browser is not on a well-known path.
live:
	go test -race -tags live ./internal/cdp

# jsbundle-browser builds the bgutils-js and BotGuard entrypoint as an ES2020
# IIFE. Chromium evaluates the committed bundle, which Go embeds from
# internal/browser.
jsbundle-browser: $(BROWSER_BUNDLE_OUT)

# WAXSEAL_BUNDLE_OUT is passed explicitly, not left to the script's default, so
# an exported value in the caller's environment cannot silently redirect the
# build while the echo below reports the untouched committed file's size.
$(BROWSER_BUNDLE_OUT): build/js/build-browser.mjs build/js/browser_entrypoint.js build/js/package.json build/js/package-lock.json
	cd build/js && npm ci --no-audit --no-fund --silent \
	  && WAXSEAL_BUNDLE_OUT="$(CURDIR)/$@" node build-browser.mjs
	@echo "built $@ ($$(wc -c < $@) bytes)"

# verify-assets rebuilds the embedded bundle into a scratch directory and fails if
# it differs from the checked-in file (reproducibility check for CI). It never
# touches that file, so a failed `npm ci` leaves a working tree behind rather than
# one `go build` cannot compile.
#
# It compares against the working tree, not the git index, so it also runs from a
# source tarball and checks the bytes go:embed actually reads. On a clean checkout
# those are the committed bytes, which is the case CI runs; locally it reports
# whether the file you are about to build with reproduces from source.
#
# It cannot delegate to jsbundle-browser: that rule writes to the committed path,
# so the npm line is spelled out here with its own output. Building and comparing
# are separate steps on purpose, so a build failure reports itself instead of
# being misreported as a bundle that differs.
verify-assets:
	@tmp=$$(mktemp -d) || exit 1; \
	  trap 'rm -rf "$$tmp"' EXIT; \
	  ( cd build/js && npm ci --no-audit --no-fund --silent \
	      && WAXSEAL_BUNDLE_OUT="$$tmp/bundle.js" node build-browser.mjs ) \
	    || { echo "ERROR: could not rebuild the bundle (npm ci or esbuild failed)"; exit 1; }; \
	  if cmp -s "$$tmp/bundle.js" $(BROWSER_BUNDLE_OUT); then \
	    echo "OK: $(BROWSER_BUNDLE_OUT) reproduces from source"; \
	  else \
	    echo "ERROR: $(BROWSER_BUNDLE_OUT) differs from a fresh build"; exit 1; \
	  fi

# release builds the CLI/daemon for Linux, macOS, and Windows (amd64 and arm64)
# into dist/. Each binary embeds the JS bundle but requires a system Chromium or
# Chrome at runtime.
release:
	@mkdir -p $(DIST)
	@for p in $(RELEASE_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  out=$(DIST)/waxseal-$$os-$$arch; \
	  if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
	  echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
	    -ldflags "-s -w -X main.version=$(VERSION)" -o $$out ./cmd/waxseal || exit 1; \
	done
	@echo "release binaries in $(DIST)/ (each requires a system Chromium at runtime)"

# Publish the runtime image to GitHub Container Registry. The image is published
# for linux/amd64 and linux/arm64, one native build per architecture with no
# QEMU: each platform builds, smoke-tests, and pushes its own $(IMAGE):VERSION-ARCH
# tag, then one docker-manifest run assembles the plain $(IMAGE):VERSION tag from
# them. Authentication reuses the gh login and pipes the token to docker on
# stdin. A full release from two machines, or from the release workflow's matrix,
# is:
#   make docker-push VERSION=1.0.0          # on an amd64 host
#   make docker-push VERSION=1.0.0          # on an arm64 host
#   PUSH_LATEST=1 make docker-manifest VERSION=1.0.0

# docker-build builds the runtime image for this host's architecture. It tags the
# per-arch name the manifest is assembled from, and also the plain VERSION and
# latest tags locally, so the README's "build instead of pull" compose flow keeps
# working on the machine that built it. BuildKit is required: the Dockerfile
# carries a syntax directive and mounts build caches.
docker-build:
	DOCKER_BUILDKIT=1 docker build --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE):$(VERSION)-$(ARCH) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

# release-guard refuses to publish the default/empty VERSION, which would tag an
# unreleased build and (with PUSH_LATEST=1) repoint the public :latest at it. It
# guards both the per-arch push and the manifest that assembles them.
release-guard:
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
	  echo "ERROR: publishing needs VERSION=x.y.z (not empty or 'dev')"; exit 1; fi

# docker-login signs in to GHCR with the gh token. It reports whether gh is not
# logged in or is missing the write:packages scope.
docker-login:
	@gh auth status >/dev/null 2>&1 || { \
	  echo "not logged in to gh. Run once, then retry:"; \
	  echo "    gh auth login"; \
	  exit 1; }
	@gh api -i user 2>/dev/null | grep -qi '^X-Oauth-Scopes:.*write:packages' || { \
	  echo "gh token is missing the 'write:packages' scope. Run once, then retry:"; \
	  echo "    gh auth refresh -h github.com -s write:packages"; \
	  exit 1; }
	@gh auth token | docker login $(REGISTRY) -u $(IMAGE_OWNER) --password-stdin

# docker-push validates VERSION and authentication before building. It pushes
# this host's per-arch tag alone. The plain VERSION and latest tags belong to the
# manifest, so nothing here can repoint them at a single architecture.
docker-push: release-guard docker-login docker-build
	docker push $(IMAGE):$(VERSION)-$(ARCH)
	@echo "pushed $(IMAGE):$(VERSION)-$(ARCH); run docker-manifest once every arch is pushed"

# docker-manifest assembles the plain VERSION tag from the per-arch tags already
# in the registry. It signs in the way docker-push does, for a human running it
# by hand after each architecture has been pushed.
#
# A caller that is already authenticated some other way runs
# docker-manifest-authed instead. The release workflow is one: it logs in with
# the Actions token, which is not a gh OAuth token and so cannot pass
# docker-login's scope check.
docker-manifest: docker-login docker-manifest-authed

# docker-manifest-authed does the assembly itself, assuming a registry login is
# already in place. It moves :latest too when PUSH_LATEST=1, then inspects what
# it published and fails unless every arch in ARCHES appears, so the multi-arch
# claim is proved at release time rather than by hand.
docker-manifest-authed: release-guard
	docker buildx imagetools create -t $(IMAGE):$(VERSION) \
	  $(foreach a,$(ARCHES),$(IMAGE):$(VERSION)-$(a))
	@if [ "$(PUSH_LATEST)" = "1" ]; then \
	  docker buildx imagetools create -t $(IMAGE):latest \
	    $(foreach a,$(ARCHES),$(IMAGE):$(VERSION)-$(a)) && echo "moved :latest"; \
	else \
	  echo "PUSH_LATEST=0; :latest not moved"; \
	fi
	@out=$$(docker buildx imagetools inspect $(IMAGE):$(VERSION)) || exit 1; \
	  echo "$$out"; \
	  for a in $(ARCHES); do \
	    echo "$$out" | grep -q "linux/$$a" || { \
	      echo "ERROR: $(IMAGE):$(VERSION) has no linux/$$a entry"; exit 1; }; \
	  done; \
	  echo "OK: $(IMAGE):$(VERSION) covers $(ARCHES)"

# deps installs the Node toolchain used to rebuild the browser bundle
# (deterministically, from the committed lockfile).
deps:
	cd build/js && npm ci --no-audit --no-fund

clean:
	rm -rf $(DIST)
