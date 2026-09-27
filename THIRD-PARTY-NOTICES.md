# Third-party notices

WaxSeal is **MIT**-licensed, implemented independently. The **GPL-3.0**
`bgutil-ytdlp-pot-provider` project was used only as a behavioral and wire
reference for interoperable details (endpoints, JSON field names); no GPL code
was copied. Algorithms ported from MIT sources are attributed below.

## Bundled at runtime

- **bgutils-js** (MIT): the BotGuard client and WebPoMinter. WaxSeal bundles it
  into `internal/browser/bg_browser_bundle.js` and evaluates it in Chromium.
  <https://github.com/LuanRT/BgUtils>

## Build-time only (not shipped)

- **esbuild** (MIT): bundles `bg_browser_bundle.js` from `build/js`.
  <https://github.com/evanw/esbuild>

## Go module dependencies

- **github.com/spf13/cobra** (Apache-2.0) and **spf13/pflag** (BSD-3-Clause): CLI framework.
- **github.com/inconshreveable/mousetrap** (Apache-2.0): a cobra dependency,
  compiled into the Windows binaries only.

WaxSeal speaks the Chrome DevTools Protocol to Chromium through
`internal/cdp`, a standard-library client maintained in this repository.

## Chromium

The release binaries drive a system **Chromium** at run time and do not bundle
it. The container image installs Debian's `chromium` package, which carries its
own (BSD-style) license. Every Debian package in the image, Chromium included,
ships its license at `/usr/share/doc/<package>/copyright`.

## Ported algorithms (MIT, with attribution)

- **rustypipe-botguard** (MIT): `validate_potoken` (protobuf field-6 scan) was
  ported to Go in `internal/botguard`. <https://codeberg.org/ThetaDev/rustypipe-botguard>
- **BgUtils** (MIT): the BotGuard client and WebPoMinter protocol informed the
  browser entrypoint and the validator. <https://github.com/LuanRT/BgUtils>
