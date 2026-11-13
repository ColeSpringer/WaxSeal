# Upstream requests

What WaxSeal wants from the sibling Wax repos it depends on.

## WaxTap

- **`PingSidecar` does not carry `browser_idle`.** `/ping` marks a daemon whose
  browser the idle timeout closed with `browser_idle:true`, beside the benign
  `no-session`. `SidecarHealth` drops the field, so a consumer cannot tell a
  daemon that is asleep from one whose session was retired. Wanted:
  `BrowserIdle bool` on `SidecarHealth`. Shipped workaround: none needed, since
  the probe already reads an idle daemon as healthy;
  `TestIdleBrowserCloseAndWakeHTTP` reads the field from the raw body. Opened
  2026-11-13.
