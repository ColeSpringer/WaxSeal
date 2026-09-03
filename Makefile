# WaxSeal build orchestration.
#
# WaxSeal mints YouTube PO tokens from a real headless Chromium, driven through
# the Chrome DevTools Protocol by internal/cdp.
# Node and esbuild produce the browser bundle embedded in internal/browser. The
# bundle is committed, so `go build` and `go test` do not need Node. The CLI and
# daemon still require Chromium at runtime.

VERSION           ?= dev
DIST              := dist
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

BROWSER_BUNDLE_OUT := internal/browser/bg_browser_bundle.js

REGISTRY    ?= ghcr.io
IMAGE_OWNER ?= colespringer
IMAGE       := $(REGISTRY)/$(IMAGE_OWNER)/waxseal

# PUSH_LATEST controls whether docker-push moves the :latest tag. The default
# publishes only VERSION. Set PUSH_LATEST=1 for a release that should also become
# :latest.
PUSH_LATEST ?= 0

.PHONY: all help fmt-check test jsbundle-browser verify-assets release deps clean \
        docker-build docker-login docker-push release-guard

all: jsbundle-browser

# help lists the common targets; run `make help` to print it.
help:
	@echo "WaxSeal make targets:"
	@echo "  fmt-check         fail if any file needs gofmt (covers provider/ too)"
	@echo "  test              offline Go test suite, race-enabled (root + provider/)"
	@echo "  jsbundle-browser  rebuild the embedded browser bundle (needs Node)"
	@echo "  verify-assets     rebuild the bundle in a temp dir, fail if the checked-in one differs"
	@echo "  release           build Linux/macOS amd64+arm64 binaries into $(DIST)/"
	@echo "  docker-build      build the runtime image (VERSION=x.y.z to tag a release)"
	@echo "  docker-push       publish to $(REGISTRY) (PUSH_LATEST=1 also moves :latest)"
	@echo "  deps              install the Node toolchain for the bundle"
	@echo "  clean             remove build output"

# fmt-check fails when any file needs gofmt. `gofmt -l` exits 0 even when it
# lists files, so the output itself is the signal.
fmt-check:
	@out=$$(gofmt -l . 2>&1); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# test runs the offline suite: the root module with the race detector (matching
# CI), then the nested provider/ module. The committed bundle means it does not
# need Node. The -tags e2e suite needs network and a warm daemon; the README
# documents running it separately.
test: fmt-check
	go test -race ./...
	cd provider && go test -race ./...

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

# release builds the CLI/daemon for Linux and macOS (amd64 and arm64) into dist/.
# Windows is excluded: it compiles, but the CDP pipe transport does not run there.
# Each binary embeds the JS bundle but requires a system Chromium at runtime.
release:
	@mkdir -p $(DIST)
	@for p in $(RELEASE_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  out=$(DIST)/waxseal-$$os-$$arch; \
	  echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
	    -ldflags "-s -w -X main.version=$(VERSION)" -o $$out ./cmd/waxseal || exit 1; \
	done
	@echo "release binaries in $(DIST)/ (each requires a system Chromium at runtime)"

# Publish the runtime image to GitHub Container Registry. Authentication reuses
# the gh login and pipes the token to docker on stdin. Publish 1.0.0 and move
# :latest with:
#   PUSH_LATEST=1 make docker-push VERSION=1.0.0

# docker-build builds the runtime image, tagged VERSION and latest. BuildKit is
# required: the Dockerfile carries a syntax directive and mounts build caches.
docker-build:
	DOCKER_BUILDKIT=1 docker build --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

# release-guard refuses to publish the default/empty VERSION, which would tag an
# unreleased build and (with PUSH_LATEST=1) repoint the public :latest at it.
release-guard:
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
	  echo "ERROR: docker-push needs VERSION=x.y.z (not empty or 'dev')"; exit 1; fi

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

# docker-push validates VERSION and authentication before building. It pushes the
# VERSION tag and pushes :latest only when PUSH_LATEST=1.
docker-push: release-guard docker-login docker-build
	docker push $(IMAGE):$(VERSION)
	@if [ "$(PUSH_LATEST)" = "1" ]; then \
	  docker push $(IMAGE):latest && echo "pushed $(IMAGE):$(VERSION) and moved :latest"; \
	else \
	  echo "pushed $(IMAGE):$(VERSION) (PUSH_LATEST=0; :latest not moved)"; \
	fi

# deps installs the Node toolchain used to rebuild the browser bundle
# (deterministically, from the committed lockfile).
deps:
	cd build/js && npm ci --no-audit --no-fund

clean:
	rm -rf $(DIST)
