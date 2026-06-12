package browser

import _ "embed"

// browserBundle is the bgutils and BotGuard entrypoint evaluated in Chromium. It
// does not replace navigator or window, so BotGuard sees the real browser.
// Rebuild: make jsbundle-browser.
//
//go:embed bg_browser_bundle.js
var browserBundle string
