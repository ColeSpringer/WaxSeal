# WaxSeal build orchestration.
#
# The committed artifacts internal/jsassets/qjs.wasm and bg_bundle.js are build
# outputs (treated like vendored deps): `go build`/`go test` need no C/Node
# toolchain. `make wasm`/`make jsbundle` regenerate them; CI diffs the result
# against the committed hash for reproducibility.
#
# Pins (record SHAs/hashes alongside in build/PROVENANCE.md):
QUICKJS_VERSION ?= v0.15.1
WASI_SDK_VERSION ?= 33
BINARYEN_VERSION ?= version_119

# Toolchains live under .toolchains/ (gitignored). `make deps` fetches them.
TOOLCHAINS    := $(CURDIR)/.toolchains
WASI_SDK      ?= $(TOOLCHAINS)/wasi-sdk-$(WASI_SDK_VERSION).0-x86_64-linux
QUICKJS_SRC   ?= $(TOOLCHAINS)/quickjs-ng
WASI_CC       := $(WASI_SDK)/bin/clang
BINARYEN      ?= $(TOOLCHAINS)/binaryen-$(BINARYEN_VERSION)
WASM_OPT      := $(BINARYEN)/bin/wasm-opt
# -Os measured best for size and snapshot speed. wazero re-optimizes during AOT,
# so wasm-level -O3/-O4 add bytes without a runtime win. --strip-* makes the
# committed bytes deterministic regardless of host.
WASM_OPT_FLAGS := -Os --strip-debug --strip-producers

ASSETS        := internal/jsassets
WASM_OUT      := $(ASSETS)/qjs.wasm
BUNDLE_OUT    := $(ASSETS)/bg_bundle.js

# Release matrix. The embedded qjs.wasm/bg_bundle.js are arch-neutral and reused
# unchanged by every target.
VERSION           ?= dev
DIST              := dist
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# JS C stack (shadow stack) in linear memory. quickjs needs more than the wasm
# default; JS-level recursion is separately bounded by JS_SetMaxStackSize.
WASM_STACK_SIZE ?= 4194304

QJS_SOURCES := \
	$(QUICKJS_SRC)/quickjs.c \
	$(QUICKJS_SRC)/libregexp.c \
	$(QUICKJS_SRC)/libunicode.c \
	$(QUICKJS_SRC)/dtoa.c

CFLAGS_WASM := \
	-O2 -DNDEBUG \
	-D_GNU_SOURCE -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_SIGNAL \
	-I$(QUICKJS_SRC) \
	-mexec-model=reactor \
	-fno-ident

LDFLAGS_WASM := \
	-lwasi-emulated-process-clocks -lwasi-emulated-signal \
	-Wl,--stack-first -Wl,-z,stack-size=$(WASM_STACK_SIZE) \
	-Wl,--export-table

.PHONY: all wasm jsbundle deps clean provenance test verify-assets release chrome-globals

all: wasm jsbundle

# test runs the offline suite. No C/Node toolchain is needed because the
# artifacts are committed; this is what CI and `go test ./...` both exercise.
test:
	go test ./...

# verify-assets rebuilds the embedded artifacts from the pinned toolchains and
# fails if they differ from the committed bytes. It depends on `make deps` having
# fetched the toolchains. The outputs are removed first so make cannot treat the
# committed files as up to date and skip the rebuild; on a fresh checkout git
# does not preserve mtimes, so committed artifacts can otherwise look newer than
# their sources.
verify-assets:
	rm -f $(WASM_OUT) $(BUNDLE_OUT)
	$(MAKE) wasm jsbundle
	@git diff --exit-code -- $(WASM_OUT) $(BUNDLE_OUT) \
	  && echo "OK: rebuilt artifacts reproduce the committed bytes" \
	  || { echo "ERROR: rebuilt artifacts differ from the committed bytes"; exit 1; }

# release cross-compiles the CLI/daemon for the full GOOS/GOARCH matrix into
# dist/. Each output is a static binary with embedded wasm/JS and no CGo.
release:
	@mkdir -p $(DIST)
	@for p in $(RELEASE_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	  out=$(DIST)/waxseal-$$os-$$arch$$ext; \
	  echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
	    -ldflags "-s -w -X main.version=$(VERSION)" -o $$out ./cmd/waxseal || exit 1; \
	done
	@echo "release binaries in $(DIST)/"

# qjs.wasm is the WaxSeal host ABI plus quickjs-ng core, built as an
# architecture-neutral WASI reactor.
wasm: $(WASM_OUT)

$(WASM_OUT): build/wasm/host.c $(QJS_SOURCES) | $(ASSETS)
	@test -x "$(WASI_CC)" || { echo "wasi-sdk clang not found at $(WASI_CC); run 'make deps'"; exit 1; }
	@test -x "$(WASM_OPT)" || { echo "wasm-opt not found at $(WASM_OPT); run 'make deps'"; exit 1; }
	$(WASI_CC) $(CFLAGS_WASM) -o $@ build/wasm/host.c $(QJS_SOURCES) $(LDFLAGS_WASM)
	@echo "compiled $@ ($$(wc -c < $@) bytes, pre-opt)"
	$(WASM_OPT) $(WASM_OPT_FLAGS) $@ -o $@
	@echo "wasm-opt $(WASM_OPT_FLAGS) -> $@ ($$(wc -c < $@) bytes)"

# bg_bundle.js is bgutils-js, the browser shim, and the BotGuard entrypoint
# bundled as an ES2020 IIFE. build.mjs reads chrome_version.json, so changing the
# emulated Chrome version must rebuild the bundle.
jsbundle: $(BUNDLE_OUT)

$(BUNDLE_OUT): build/js/build.mjs build/js/shim.js build/js/dom.js build/js/entrypoint.js build/js/package.json chrome_version.json | $(ASSETS)
	cd build/js && npm install --no-audit --no-fund --silent && node build.mjs
	@echo "built $@ ($$(wc -c < $@) bytes)"

$(ASSETS):
	mkdir -p $(ASSETS)

# chrome-globals refreshes the shim audit's Chrome reference fixture. The
# fixture is captured data rather than a reproducible build output, so this
# target is intentionally excluded from all and verify-assets. Run it on Windows
# with WX_CHROME_EXECUTABLE pointing to the pinned Chrome for Testing build, then
# review the JSON diff.
chrome-globals:
	cd build/js && npm install --no-audit --no-fund --silent && node capture-globals.mjs

# deps fetches pinned toolchains into .toolchains/ and is idempotent.
deps:
	mkdir -p $(TOOLCHAINS)
	@if [ ! -x "$(WASI_CC)" ]; then \
		echo "fetching wasi-sdk-$(WASI_SDK_VERSION)..."; \
		curl -sSL -o $(TOOLCHAINS)/wasi-sdk.tar.gz \
		  "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-$(WASI_SDK_VERSION)/wasi-sdk-$(WASI_SDK_VERSION).0-x86_64-linux.tar.gz"; \
		tar -xzf $(TOOLCHAINS)/wasi-sdk.tar.gz -C $(TOOLCHAINS); \
	fi
	@if [ ! -d "$(QUICKJS_SRC)" ]; then \
		echo "cloning quickjs-ng $(QUICKJS_VERSION)..."; \
		git clone --depth 1 --branch $(QUICKJS_VERSION) https://github.com/quickjs-ng/quickjs.git $(QUICKJS_SRC); \
	fi
	@if [ ! -x "$(WASM_OPT)" ]; then \
		echo "fetching binaryen $(BINARYEN_VERSION)..."; \
		curl -sSL -o $(TOOLCHAINS)/binaryen.tar.gz \
		  "https://github.com/WebAssembly/binaryen/releases/download/$(BINARYEN_VERSION)/binaryen-$(BINARYEN_VERSION)-x86_64-linux.tar.gz"; \
		tar -xzf $(TOOLCHAINS)/binaryen.tar.gz -C $(TOOLCHAINS); \
	fi
	cd build/js && npm install --no-audit --no-fund

provenance:
	@echo "quickjs-ng $(QUICKJS_VERSION) @ $$(git -C $(QUICKJS_SRC) rev-parse HEAD 2>/dev/null || echo '?')"
	@echo "wasi-sdk   $(WASI_SDK_VERSION)"
	@echo "binaryen   $(BINARYEN_VERSION) ($$($(WASM_OPT) --version 2>/dev/null || echo '?'))"
	@echo "qjs.wasm   sha256 $$(sha256sum $(WASM_OUT) 2>/dev/null | cut -d' ' -f1) ($$(wc -c < $(WASM_OUT) 2>/dev/null) B)"
	@echo "bg_bundle  sha256 $$(sha256sum $(BUNDLE_OUT) 2>/dev/null | cut -d' ' -f1) ($$(wc -c < $(BUNDLE_OUT) 2>/dev/null) B)"

clean:
	rm -f $(WASM_OUT) $(BUNDLE_OUT)
	rm -rf $(DIST)
