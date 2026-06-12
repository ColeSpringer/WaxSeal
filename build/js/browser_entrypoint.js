/*
 * WaxSeal BotGuard entrypoint for a real Chromium.
 *
 * BgUtils runs the BotGuard VM against the browser's real global scope. No
 * navigator, window, or performance shim is installed.
 *
 * Exposes runBotguard, newMinter, and mint on globalThis for calls from Go via
 * page.Eval. All HTTP requests remain in Go.
 */
import { BG } from 'bgutils-js';

const G = globalThis;

// Keep the helpers out of normal page-global enumeration.
const defHidden = (name, value) =>
  Object.defineProperty(G, name, { value, configurable: true, writable: true, enumerable: false });

// The snapshot signal and minter must survive across calls (live JS closures).
let webPoSignalOutput;
let minter;

/**
 * Load the fetched interpreter into the real global scope, create the BotGuard
 * client against the real navigator, and retain the snapshot signal.
 *
 * @param {string} interpreterJavascript - descrambled interpreter JS
 * @param {string} program - challenge program (arr[4])
 * @param {string} globalName - VM global name (arr[5])
 * @returns {Promise<string>} botguardResponse
 */
defHidden('runBotguard', async (interpreterJavascript, program, globalName) => {
  // Requires script-src 'unsafe-eval'; Go sets Page.setBypassCSP(true) before this.
  new Function(interpreterJavascript)();

  const botguard = await BG.BotGuardClient.create({
    program,
    globalName,
    globalObj: G,
  });

  webPoSignalOutput = [];
  const botguardResponse = await botguard.snapshot({ webPoSignalOutput });
  return botguardResponse;
});

/**
 * Create the WebPoMinter from the GenerateIT integrity token and the live
 * webPoSignalOutput captured during snapshot. Must run on the same page as
 * runBotguard (webPoSignalOutput[0] is a live closure).
 *
 * @param {string} integrityToken - base64 integrity token from GenerateIT
 * @returns {Promise<boolean>}
 */
defHidden('newMinter', async (integrityToken) => {
  minter = await BG.WebPoMinter.create({ integrityToken }, webPoSignalOutput);
  return true;
});

/**
 * Mint a websafe-base64 PO token bound to `identifier` (video_id or
 * visitor_data). One minter mints many identifiers until expiry.
 *
 * @param {string} identifier
 * @returns {Promise<string>} websafe base64 token
 */
defHidden('mint', async (identifier) => {
  return await minter.mintAsWebsafeString(identifier);
});
