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

- **github.com/go-rod/rod** (MIT): the Chrome DevTools Protocol driver used to run
  BotGuard in a real headless Chromium. It pulls
  `ysmood/{gson,goob,fetchup,leakless}` and `google.golang.org/protobuf`.
  <https://github.com/go-rod/rod>
- **github.com/spf13/cobra** and **spf13/pflag** (Apache-2.0): CLI framework.

The product drives an external system **Chromium** at run time; Chromium is not
bundled and carries its own (BSD-style) license.

## Ported algorithms (MIT, with attribution)

- **rustypipe-botguard** (MIT): `descramble` (`+97`/byte), `parse_challenge_data`,
  and `validate_potoken` (protobuf field-6 scan) were ported to Go in
  `internal/botguard`. <https://codeberg.org/ThetaDev/rustypipe-botguard>
- **BgUtils** (MIT): the BotGuard client and WebPoMinter protocol informed the
  browser entrypoint and the validator. <https://github.com/LuanRT/BgUtils>
