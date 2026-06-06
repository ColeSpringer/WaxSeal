// Capture the global surface of the pinned Chrome for Testing build for the shim
// audit. The result is captured data, not a reproducible build output, so it is
// excluded from verify-assets and should be reviewed before it is committed.
// Run this on Windows with the exact pinned browser:
//
//   set WX_CHROME_EXECUTABLE=C:\path\to\chrome-for-testing\chrome.exe
//   node capture-globals.mjs            (from build/js)
//   # or via the repo root:  make chrome-globals
//
// The script does not fall back to Playwright's bundled Chromium or another
// installed browser. It writes the fixture only after validating the platform,
// browser version, origin, and launch arguments.
import { chromium } from 'playwright-core';
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import process from 'node:process';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, '../../');
const FIXTURE = resolve(repoRoot, 'internal/shimaudit/fixtures/chrome_windows_desktop_globals.json');
const ENUMERATOR = resolve(repoRoot, 'internal/shimaudit/enumerate.js');

const chrome = JSON.parse(readFileSync(resolve(repoRoot, 'chrome_version.json'), 'utf8'));
const PINNED = chrome.fullVersion; // exact full version, e.g. "149.0.7827.3"

// Use an HTTPS origin because about:, data:, and file: expose different
// SecureContext APIs, including the SpeechRecognition family.
const ORIGIN = 'https://localhost';

// Keep this in sync with TestChromeReferenceMatchesProfile. Launch flags can
// change the exposed browser surface, so unknown flags reject the capture.
const ALLOWED_FLAGS = new Set([
  '--headless', '--headless=new', '--hide-scrollbars', '--mute-audio',
  '--no-sandbox', '--disable-gpu', '--remote-debugging-pipe', '--remote-debugging-port=0',
]);

const EXECUTABLE = process.env.WX_CHROME_EXECUTABLE;
const HEADLESS = process.env.WX_HEADLESS !== '0'; // default headless; WX_HEADLESS=0 for headed
const EXTRA_ARGS = (process.env.WX_CHROME_ARGS || '').split(/\s+/).filter(Boolean);

function fail(msg) {
  console.error('capture-globals: ' + msg);
  process.exit(1);
}

if (!EXECUTABLE) {
  fail('set WX_CHROME_EXECUTABLE to the Chrome for Testing ' + PINNED +
    ' binary (the bundled Playwright Chromium is intentionally unused)');
}
if (process.platform !== 'win32') {
  fail('must run on Windows (the shim claims Win32); platform=' + process.platform +
    '. Capture the committed fixture on a Windows host.');
}
for (const f of EXTRA_ARGS) {
  if (!ALLOWED_FLAGS.has(f)) {
    fail('launch arg ' + f + ' is outside the allowed set: ' + [...ALLOWED_FLAGS].join(' '));
  }
}

// This descriptor-based enumerator is also embedded by internal/shimaudit.
const enumerator = readFileSync(ENUMERATOR, 'utf8').trim();

const browser = await chromium.launch({ executablePath: EXECUTABLE, headless: HEADLESS, args: EXTRA_ARGS });
try {
  const version = browser.version();
  if (version !== PINNED) {
    fail('launched Chrome ' + version + ' != pinned ' + PINNED +
      '; install the exact CfT build, or update chrome_version.json to a selected available build (never capture a mismatch)');
  }

  const context = await browser.newContext();
  const page = await context.newPage();

  // Fulfill the request directly so the page has an HTTPS origin without a
  // separate local server.
  await page.route('**/*', (route) =>
    route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: '<!doctype html><meta charset="utf-8"><title>wx</title>' }));

  // Capture before page scripts can modify the global surface.
  await page.addInitScript('globalThis.__wxCaptureResult = (\n' + enumerator + '\n);');

  await page.goto(ORIGIN + '/', { waitUntil: 'domcontentloaded' });

  const surface = await page.evaluate(() => globalThis.__wxCaptureResult);
  const ctx = await page.evaluate(() => ({ origin: location.origin, isSecureContext: window.isSecureContext === true }));

  if (!surface || !surface.window || Object.keys(surface.window).length === 0) {
    fail('enumeration produced no window surface');
  }
  if (ctx.origin !== ORIGIN) {
    fail('final origin ' + ctx.origin + ' != expected ' + ORIGIN);
  }
  if (ctx.isSecureContext !== true) {
    fail('isSecureContext !== true; capture over a real https:// origin');
  }

  const fixture = {
    schemaVersion: 1,
    meta: {
      source: 'capture',
      os: 'windows',
      fullVersion: version,
      headless: HEADLESS,
      origin: ctx.origin,
      isSecureContext: ctx.isSecureContext,
      launchFlags: EXTRA_ARGS.slice().sort(),
      capturedAt: new Date().toISOString(),
    },
    window: surface.window,
    navigator: surface.navigator || {},
    document: surface.document || {},
  };

  writeFileSync(FIXTURE, stableStringify(fixture) + '\n');
  console.error('capture-globals: wrote ' + FIXTURE +
    ' (window=' + Object.keys(fixture.window).length +
    ' navigator=' + Object.keys(fixture.navigator).length +
    ' document=' + Object.keys(fixture.document).length + ') for Chrome ' + version);
} finally {
  await browser.close();
}

// stableStringify sorts object keys and keeps small Shape records on one line,
// producing compact, deterministic fixture diffs.
function stableStringify(value) {
  const sp = '  ';
  const inlineable = (v) =>
    v && typeof v === 'object' && !Array.isArray(v) &&
    Object.values(v).every((x) => x === null || typeof x !== 'object');
  const enc = (v, depth) => {
    if (v === null || typeof v !== 'object') return JSON.stringify(v);
    const pad = sp.repeat(depth), padIn = sp.repeat(depth + 1);
    if (Array.isArray(v)) {
      if (v.length === 0) return '[]';
      return '[\n' + v.map((x) => padIn + enc(x, depth + 1)).join(',\n') + '\n' + pad + ']';
    }
    const keys = Object.keys(v).sort();
    if (keys.length === 0) return '{}';
    if (inlineable(v)) {
      return '{ ' + keys.map((k) => JSON.stringify(k) + ': ' + JSON.stringify(v[k])).join(', ') + ' }';
    }
    return '{\n' + keys.map((k) => padIn + JSON.stringify(k) + ': ' + enc(v[k], depth + 1)).join(',\n') + '\n' + pad + '}';
  };
  return enc(value, 0);
}
