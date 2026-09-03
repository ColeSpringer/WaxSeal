// Bundle bgutils-js and browser_entrypoint.js into one ES2020 IIFE for Chromium.
// The committed output is embedded from internal/browser, so `go build` does not
// need Node.
// Rebuild: make jsbundle-browser.
import { build } from 'esbuild';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';

const pkgVersion = (name) =>
  JSON.parse(readFileSync(`node_modules/${name}/package.json`, 'utf8')).version;
const bgutilsVersion = pkgVersion('bgutils-js');
const esbuildVersion = pkgVersion('esbuild');

// Where the bundle is written. WAXSEAL_BUNDLE_OUT lets `make verify-assets`
// rebuild into a scratch directory instead of over the checked-in file. It must
// be absolute: this script runs with cwd build/js, which is what the fallback
// below is relative to. Both make rules pass it explicitly, so an exported value
// cannot redirect them; the fallback only serves a direct `node
// build-browser.mjs`. Nothing in the emitted bytes depends on this, so the output
// is reproducible either way.
const OUT = process.env.WAXSEAL_BUNDLE_OUT || '../../internal/browser/bg_browser_bundle.js';

const result = await build({
  entryPoints: ['browser_entrypoint.js'],
  bundle: true,
  format: 'iife',
  target: 'es2020',
  platform: 'browser',
  legalComments: 'none',
  minify: false, // keep the embedded bundle readable
  banner: {
    js: `// GENERATED - do not edit. Source: build/js/browser_entrypoint.js and bgutils-js@${bgutilsVersion}.\n`
      + `// Rebuild: make jsbundle-browser (esbuild@${esbuildVersion}).`
  },
  outfile: OUT
});

if (result.errors.length) {
  console.error(result.errors);
  process.exit(1);
}

const bytes = readFileSync(OUT);
const sha = createHash('sha256').update(bytes).digest('hex');
console.log(`bg_browser_bundle.js: ${bytes.length} bytes  sha256=${sha}`);
console.log(`  bgutils-js@${bgutilsVersion}  esbuild@${esbuildVersion}`);
