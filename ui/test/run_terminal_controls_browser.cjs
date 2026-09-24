"use strict";

// terminal controls focused real-browser gate. It drives the bundled terminal against the
// existing reopen fixture. Product clicks are trusted browser pointer events;
// fixture controls only arrange server state and terminal modes.
const fs = require("fs");
const https = require("https");
const os = require("os");
const path = require("path");
const { startFixture, STYLE_NONCE } = require("./unified_reopen_fixture.cjs");
const { requestJSON: requestHTTPJSON } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_TERMINAL_CONTROLS_ENGINE || "chromium";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const EVIDENCE = path.resolve(process.env.PERSEA_TERMINAL_CONTROLS_EVIDENCE_DIR || path.join(os.tmpdir(), `persea-terminal_controls-${ENGINE}`));
const MUTANT = process.env.PERSEA_TERMINAL_CONTROLS_MUTANT || "";
const MUTANTS = new Set([
  "no-keybox-bank-entry",
  "clipped-status-notice",
  "native-key-echo-lost",
  "f12-keycode", "double-function-dispatch", "no-control-gate", "no-bank-reset", "row-drift", "second-row",
  "typography-refocus", "selection-loss", "typography-input", "typography-resize", "popover-in-flow",
  "no-cas", "clamp-enabled", "conflict-lie", "unavailable-lie", "function-active-lie",
  "function-hidden-reachable", "keyboard-back-focus", "standard-marker", "no-typography-claim",
  "keyboard-entry-focus", "no-typography-competitor-close", "no-typography-lifecycle-close",
  "composer-overflow-visible", "theme-border-collapse", "theme-status-collapse", "popover-no-flip",
  "bank-pointerdown-activation", "portal-controls-downsize", "theme-weak-palette",
  "function-bank-retains-ctrl", "typography-native-disable", "typography-stale-frame-restore",
  "function-bank-ctrl-enabled", "sheet-gesture-generation-fence",
  "keyboard-activation-generation-fence", "keyboard-canceled-tombstone",
  "standard-key-generation-fence", "paste-generation-fence", "keyboard-enter-repeat-provenance",
  "keyboard-canceled-release-retirement", "toolbar-composer-generation-fence",
  "composer-control-generation-fence", "session-generation-provider-omission", "session-sheet-generation-provider-omission",
  "keyboard-unrelated-key-provenance", "tap-pointer-identity",
  "transport-key-authority-reset", "typography-layout-reanchor",
]);
const THEME_IDS = Object.freeze(["default", "rose-pine", "rose-pine-dawn", "solarized-dark", "gruvbox-dark", "one-dark", "dracula", "catppuccin-mocha"]);
const F_BYTES = Object.freeze(["\x1bOP", "\x1bOQ", "\x1bOR", "\x1bOS", "\x1b[15~", "\x1b[17~", "\x1b[18~", "\x1b[19~", "\x1b[20~", "\x1b[21~", "\x1b[23~", "\x1b[24~"]);
const ALL_SHAPES = Object.freeze([
  { name: "phone-320", width: 320, height: 568, touch: true },
  { name: "phone-360", width: 360, height: 780, touch: true },
  { name: "phone-390", width: 390, height: 844, touch: true },
  { name: "desktop-1280", width: 1280, height: 800, touch: false },
]);
const selectedCase = process.env.PERSEA_TERMINAL_CONTROLS_CASE;
const SHAPES = ALL_SHAPES.filter((shape) => !selectedCase || shape.name === selectedCase);

function assert(value, message) {
  if (value) return;
  const error = new Error(`TERMINAL_CONTROLS_ASSERTION: ${message}`);
  error.name = "TERMINAL_CONTROLSAssertionError";
  throw error;
}
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function trustedPhonePressDrag(page, start, end) {
  const interpolate = (step) => ({
    x: Math.round(start.x + ((end.x - start.x) * step) / 6),
    y: Math.round(start.y + ((end.y - start.y) * step) / 6),
    id: 0,
  });
  if (ENGINE === "chromium") {
    const session = await page.context().newCDPSession(page);
    await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [interpolate(0)] });
    for (let step = 1; step <= 6; step += 1) {
      await session.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [interpolate(step)] });
      await delay(25);
    }
    await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    await session.detach();
    return;
  }
  // WebKit exposes trusted touch dispatch through its inspector protocol but
  // does not turn those automation touch points into platform scrolling. Keep
  // the real touch press/move/release sequence, then feed the same trusted wheel
  // event its RawMouseImpl uses while the press is held so the scroll surface
  // advances without weakening mobile/coarse emulation.
  const implementation = page._connection?.toImpl?.(page);
  const delegate = implementation?.delegate;
  assert(delegate?._session && delegate?._pageProxySession, "WebKit trusted input bridge is unavailable");
  await delegate._pageProxySession.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [interpolate(0)] });
  for (let step = 1; step <= 6; step += 1) {
    await delegate._pageProxySession.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [interpolate(step)] });
    await delay(25);
  }
  await delegate._session.send("Page.updateScrollingState");
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(resolve)));
  await delegate._pageProxySession.send("Input.dispatchWheelEvent", { x: end.x, y: end.y, deltaX: 240, deltaY: 0, modifiers: 0 });
  await delegate._pageProxySession.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [interpolate(6)] });
}

// Two simultaneous browser-owned contacts. Control goes down first; Function
// goes down second and releases first, so its bank/sheet transition happens
// before the stale Control release. Both inspector protocols accept changed
// touch points for start/end, preserving the contacts independently.
async function trustedTwoTouchOrder(page, first, second) {
  let session;
  let detach = async () => undefined;
  if (ENGINE === "chromium") {
    session = await page.context().newCDPSession(page);
    detach = () => session.detach();
  } else {
    const implementation = page._connection?.toImpl?.(page);
    session = implementation?.delegate?._pageProxySession;
    assert(session, "WebKit trusted two-touch bridge is unavailable");
  }
  const control = { x: Math.round(first.x), y: Math.round(first.y), id: 31 };
  const functions = { x: Math.round(second.x), y: Math.round(second.y), id: 47 };
  try {
    await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [control] });
    await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [functions] });
    await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [functions] });
    await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [control] });
  } finally {
    await detach();
  }
}

// Three independently identified contacts. A begins on the terminal key in
// generation G; the disclosure contact completes and advances authority; B
// then begins on the same key in G+1. A's later release must consult A's own
// capture, not B's newer one. The caller inspects state before finishing B.
async function beginTrustedPointerIdentityOrder(page, first, disclosure, second) {
  let session;
  let detach = async () => undefined;
  if (ENGINE === "chromium") {
    session = await page.context().newCDPSession(page);
    detach = () => session.detach();
  } else {
    const implementation = page._connection?.toImpl?.(page);
    session = implementation?.delegate?._pageProxySession;
    assert(session, "WebKit trusted pointer-identity bridge is unavailable");
  }
  const point = (source, id) => ({ x: Math.round(source.x), y: Math.round(source.y), id });
  const a = point(first, 61);
  const transition = point(disclosure, 67);
  const b = point(second, 73);
  let finished = false;
  try {
    await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [a] });
    await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [transition] });
    await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [transition] });
    await page.waitForFunction(() => document.querySelector(".persea-unified-sheet")?.hidden === false);
    await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [b] });
    await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [a] });
  } catch (error) {
    await detach();
    throw error;
  }
  return Object.freeze({
    async finish() {
      if (finished) return;
      finished = true;
      try {
        await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [b] });
      } finally {
        await detach();
      }
    },
  });
}

function colorChannels(value) {
  const numbers = String(value).match(/[\d.]+/g)?.map(Number) || [];
  if (String(value).startsWith("color(srgb") && numbers.length >= 3) return numbers.slice(0, 3).map((part) => part * 255);
  return numbers.slice(0, 3);
}
function contrast(a, b) {
  const luminance = (value) => {
    const channels = colorChannels(value);
    assert(channels.length === 3, `unreadable computed color ${value}`);
    const linear = channels.map((part) => {
      const unit = part / 255;
      return unit <= 0.04045 ? unit / 12.92 : ((unit + 0.055) / 1.055) ** 2.4;
    });
    return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
  };
  const [bright, dark] = [luminance(a), luminance(b)].sort((left, right) => right - left);
  return (bright + 0.05) / (dark + 0.05);
}

const requestJSON = (url, method = "GET", body) => {
  if (!url.startsWith("https:")) return requestHTTPJSON(url, method, body);
  return new Promise((resolve, reject) => {
    const request = https.request(url, {
      method, rejectUnauthorized: false,
      headers: body ? { "Content-Type": "application/json" } : {},
    }, (response) => {
      let text = "";
      response.setEncoding("utf8");
      response.on("data", (chunk) => { text += chunk; });
      response.on("end", () => { try { resolve(JSON.parse(text)); } catch (error) { reject(error); } });
    });
    request.on("error", reject);
    if (body) request.write(JSON.stringify(body));
    request.end();
  });
};

function replace(source, pattern, replacement, expected, label) {
  let count = 0;
  source.replace(pattern, (match) => { count += 1; return match; });
  assert(count === expected, `${label} mutant replaced ${count}, expected ${expected}`);
  return source.replace(pattern, replacement);
}

async function prepareFixtureUI() {
  if (!MUTANT) return { root: UI, cleanup() {} };
  assert(MUTANTS.has(MUTANT), `unknown terminal controls mutant ${MUTANT}`);
  const root = fs.mkdtempSync(path.join(process.env.TMPDIR || os.tmpdir(), "terminal_controls-mutant-"));
  const dist = path.join(root, "dist");
  fs.cpSync(path.join(UI, "dist"), dist, { recursive: true });
  const esbuild = require(path.join(UI, "node_modules/esbuild"));
  let touched = 0;
  const result = await esbuild.build({
    entryPoints: [path.join(UI, "src/app.ts")], bundle: true, format: "esm", platform: "browser",
    sourcemap: true, outfile: path.join(dist, "app.js"), metafile: true,
    plugins: [{
      name: `terminal_controls-${MUTANT}`,
      setup(build) {
        build.onLoad({ filter: /\.(ts|css)$/ }, (args) => {
          let source = fs.readFileSync(args.path, "utf8");
          const file = path.basename(args.path);
          if (MUTANT === "f12-keycode" && file === "unified_key_bar.ts") source = replace(source, 'f12: { key: "F12", code: "F12", keyCode: 123 }', 'f12: { key: "F12", code: "F12", keyCode: 122 }', 1, MUTANT), touched += 1;
          if (MUTANT === "no-control-gate" && file === "unified_terminal_page.ts") {
            source = replace(source, " || !this.controlGranted || this.options.capabilityMode", " || false || this.options.capabilityMode", 1, MUTANT);
            source = replace(source, "this.selectMode || !availability.canInject", "this.selectMode || false", 1, MUTANT);
            source = replace(source, "authority !== this.keyInteractionGeneration || !this.composerAvailability().canInject", "authority !== this.keyInteractionGeneration", 1, MUTANT);
            touched += 1;
          }
          if (MUTANT === "row-drift" && file === "unified_terminal_page.ts") source = replace(source, "    this.keyRow = keyRow;", "    keyRow.firstElementChild?.remove();\n    this.keyRow = keyRow;", 1, MUTANT), touched += 1;
          if (MUTANT === "second-row" && file === "unified_terminal_page.ts") source = replace(source, "keyBar.append(keyRow, keyBankToggle);", "keyBar.append(keyRow.cloneNode(true), keyRow, keyBankToggle);", 1, MUTANT), touched += 1;
          if (MUTANT === "clipped-status-notice" && file === "attachment_page.css") source += '\n.persea-unified-toast { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }\n', touched += 1;
          if (MUTANT === "native-key-echo-lost" && file === "unified_terminal_page.ts") source = replace(source, 'this.iosBackspace.onXtermKeyHandled(domEvent, key)', 'this.iosBackspace.onXtermKeyHandled(domEvent)', 1, MUTANT), touched += 1;
          if (MUTANT === "typography-refocus" && file === "composer.ts") source = replace(source, /(this\.onToggleTypography,\n\s*)\(\) => undefined,/, "$1() => this.restoreTextareaFocus(),", 1, MUTANT), touched += 1;
          if (MUTANT === "selection-loss" && file === "composer.ts") source = replace(source, "this.textarea.setSelectionRange(selectionStart, selectionEnd, selectionDirection);", 'this.textarea.setSelectionRange(0, 0, "none");', 1, MUTANT), touched += 1;
          if (MUTANT === "typography-input" && file === "unified_terminal_page.ts") source = replace(source, "    this.applyComposerFontSize(value, saving);", '    this.applyComposerFontSize(value, saving);\n    this.sendKeyBarKey("tab");', 1, MUTANT), touched += 1;
          if (MUTANT === "typography-resize" && file === "unified_terminal_page.ts") source = replace(source, "    this.applyComposerFontSize(value, saving);", '    this.applyComposerFontSize(value, saving);\n    const resizeTarget = this.prepared;\n    if (resizeTarget) this.send({ type: "RESIZE_REQUEST", version: 1, source: resizeTarget.source, epoch: resizeTarget.epoch, columns: this.committedColumns, rows: this.committedRows });', 1, MUTANT), touched += 1;
          if (MUTANT === "popover-in-flow" && file === "composer.ts") source = replace(source, "      options.page.append(this.typographyPopover);", '      this.typographyRoot.append(this.typographyPopover);\n      this.typographyPopover.style.position = "static";', 1, MUTANT), touched += 1;
          if (MUTANT === "no-cas" && file === "operator_preferences.ts") source = replace(source, '"If-Match": `"${revision}"`', '"X-Mutant-If-Match": `"${revision}"`', 1, MUTANT), touched += 1;
          if (MUTANT === "clamp-enabled" && file === "composer.ts") source = replace(source, "setUnavailable(plus, !state.enabled || saving || state.size >= COMPOSER_FONT_SIZE_MAX);", "setUnavailable(plus, !state.enabled || saving);", 1, MUTANT), touched += 1;
          if (MUTANT === "conflict-lie" && file === "unified_terminal_page.ts") source = replace(source, /snapshot\.status === "conflict"\n\s*\? "conflict"/, 'snapshot.status === "conflict"\n        ? "ready"', 1, MUTANT), touched += 1;
          if (MUTANT === "unavailable-lie" && file === "unified_terminal_page.ts") source = replace(source, /snapshot\.status === "unavailable"\n\s*\? "unavailable"/, 'snapshot.status === "unavailable"\n          ? "ready"', 1, MUTANT), touched += 1;
          if (MUTANT === "standard-marker" && file === "unified_terminal_page.ts") source = replace(source, "    this.keyRow = keyRow;", '    keyRow.dataset.bank = "standard";\n    this.keyRow = keyRow;', 1, MUTANT), touched += 1;
          if (MUTANT === "no-typography-claim" && file === "unified_terminal_page.ts") source = replace(source, '      claimTypographyPopover: () => this.claimPopover("typography"),', '      claimTypographyPopover: () => undefined,', 1, MUTANT), touched += 1;
          if (MUTANT === "no-typography-competitor-close" && file === "unified_terminal_page.ts") source = replace(source, '    if (owner !== "typography") this.composer?.closeTypographyPopover(true);', '    if (false) this.composer?.closeTypographyPopover(true);', 1, MUTANT), touched += 1;
          if (MUTANT === "no-typography-lifecycle-close" && file === "unified_terminal_page.ts") source = replace(source, '    if (!this.explainerPopover.hidden) this.closeExplainer(true);\n    this.composer?.closeTypographyPopover(true);', '    if (!this.explainerPopover.hidden) this.closeExplainer(true);', 1, MUTANT), touched += 1;
          if (MUTANT === "composer-overflow-visible" && file === "attachment_page.css") source = replace(source, '  box-shadow: none;\n  overflow: hidden;\n}', '  box-shadow: none;\n  overflow: visible;\n}', 1, MUTANT), touched += 1;
          if (MUTANT === "theme-border-collapse" && file === "attachment_page.css") source = replace(source, '  border: 1px solid var(--persea-chrome-control-border);', '  border: 1px solid var(--persea-chrome-bg);', 1, MUTANT), touched += 1;
          if (MUTANT === "theme-status-collapse" && file === "attachment_page.css") source = replace(source, '  color: var(--persea-chrome-notice-fg);\n}', '  color: var(--persea-chrome-bg);\n}', 1, MUTANT), touched += 1;
          if (MUTANT === "popover-no-flip" && file === "composer.ts") source = replace(source, '    const above = roomAbove >= measured.height || roomAbove > roomBelow;', '    const above = true;', 1, MUTANT), touched += 1;
          if (MUTANT === "portal-controls-downsize" && file === "attachment_page.css") source = replace(source, '  .persea-unified-terminal > .attachment-page__composer-typography-popover .attachment-page__button {\n    min-width: 44px;\n    min-height: 44px;\n  }', '  .persea-unified-terminal > .attachment-page__composer-typography-popover .attachment-page__button {\n    min-width: 40px;\n    min-height: 40px;\n  }', 1, MUTANT), touched += 1;
          if (MUTANT === "theme-weak-palette" && file === "app.css") source = replace(source, '  --persea-chrome-control-border: var(--persea-accent);', '  --persea-chrome-control-border: color-mix(in srgb, var(--persea-chrome-fg) 38%, var(--persea-chrome-bg));', 1, MUTANT), touched += 1;
          if (MUTANT === "typography-native-disable" && file === "composer.ts") source = replace(source, "      button.disabled = false;", "      button.disabled = unavailable;", 1, MUTANT), touched += 1;
          if (MUTANT === "typography-stale-frame-restore" && file === "composer.ts") source = replace(source, '      if (gestureActive || this.textareaGestureActive()\n        || this.typographyInteractionGeneration !== interactionGeneration\n        || this.textarea.value !== value\n        || this.textarea.selectionStart !== selectionStart\n        || this.textarea.selectionEnd !== selectionEnd\n        || this.textarea.selectionDirection !== selectionDirection) return;', '      if (false) return;', 1, MUTANT), touched += 1;
          if (MUTANT === "sheet-gesture-generation-fence" && file === "tap_activation.ts") source = replace(source, '    if (!event.isTrusted || !isLive() || capture.generation !== interactionGeneration()) return;', '    if (!event.isTrusted || !isLive()) return;', 1, MUTANT), touched += 1;
          if (MUTANT === "keyboard-activation-generation-fence" && file === "tap_activation.ts") source = replace(source, '  const generationIsCurrent = (capture: Exclude<KeyboardActivationState, Readonly<{ kind: "idle" }>>): boolean =>\n    capture.generation === interactionGeneration();', '  const generationIsCurrent = (_capture: Exclude<KeyboardActivationState, Readonly<{ kind: "idle" }>>): boolean => true;', 1, MUTANT), touched += 1;
          if (MUTANT === "keyboard-canceled-tombstone" && file === "tap_activation.ts") source = replace(source, '    state = Object.freeze({ ...state, kind: "canceled" });', '    state = idle;', 1, MUTANT), touched += 1;
          if (MUTANT === "keyboard-canceled-release-retirement" && file === "tap_activation.ts") source = replace(source, '    if (event.cancelable) event.preventDefault();\n    clear();', '    state = Object.freeze({ ...state, released: true });', 1, MUTANT), touched += 1;
          if (MUTANT === "keyboard-unrelated-key-provenance" && file === "tap_activation.ts") source = replace(source, '    if (!key) {\n      // An unrelated physical key neither ends nor transfers the authority of\n      // a held Enter/Space press. Its matching release must still see the\n      // original control and generation.\n      return;\n    }', '    if (!key) {\n      if (!event.repeat) clear();\n      return;\n    }', 1, MUTANT), touched += 1;
          if (MUTANT === "tap-pointer-identity" && file === "tap_activation.ts") source = replace(source, '    onPointerMove(event);\n    const capture = armedPointers.get(event.pointerId);', '    onPointerMove(event);\n    const capture = [...armedPointers.values()].at(-1);', 1, MUTANT), touched += 1;
          if (MUTANT === "toolbar-composer-generation-fence" && file === "unified_terminal_page.ts") source = replace(source, '    this.cleanupListeners.push(bindTapActivation(\n      composerToggle,\n      (event) => this.toggleComposer(event),\n      () => undefined,\n      () => !this.closed,\n      () => this.keyInteractionGeneration,\n    ));', '    composerToggle.addEventListener("click", (event) => this.toggleComposer(event));', 1, MUTANT), touched += 1;
          if (MUTANT === "composer-control-generation-fence" && file === "composer.ts") source = replace(source, '      this.options.interactionGeneration,\n    );\n    return button;', '      () => 0,\n    );\n    return button;', 1, MUTANT), touched += 1;
          if (MUTANT === "session-generation-provider-omission" && file === "session_switcher.ts") {
            source = replace(source, '  interactionGeneration(): number;', '  interactionGeneration?(): number;', 1, MUTANT);
            source = replace(source, '}, () => undefined, () => !button.disabled, this.options.interactionGeneration);', '}, () => undefined, () => !button.disabled, () => this.options.interactionGeneration?.() ?? 0);', 1, MUTANT);
            touched += 1;
          }
          if (MUTANT === "session-generation-provider-omission" && file === "unified_terminal_page.ts") source = replace(source, '        select: (session) => { void this.selectSession(session, "tag"); },\n        interactionGeneration: () => this.keyInteractionGeneration,', '        select: (session) => { void this.selectSession(session, "tag"); },', 1, MUTANT), touched += 1;
          if (MUTANT === "session-sheet-generation-provider-omission" && file === "session_switcher.ts") {
            source = replace(source, '  interactionGeneration(): number;', '  interactionGeneration?(): number;', 1, MUTANT);
            source = replace(source, '}, () => undefined, () => !button.disabled, this.options.interactionGeneration);', '}, () => undefined, () => !button.disabled, () => this.options.interactionGeneration?.() ?? 0);', 1, MUTANT);
            touched += 1;
          }
          if (MUTANT === "session-sheet-generation-provider-omission" && file === "unified_terminal_page.ts") source = replace(source, '        select: (session) => { void this.selectSession(session, "sheet"); },\n        interactionGeneration: () => this.keyInteractionGeneration,', '        select: (session) => { void this.selectSession(session, "sheet"); },', 1, MUTANT), touched += 1;
          if (MUTANT === "standard-key-generation-fence" && file === "unified_terminal_page.ts") source = replace(source, "bindTapActivation(button, activate, () => {}, () => !this.closed, () => this.keyInteractionGeneration)", "bindTapActivation(button, activate, () => {}, () => !this.closed, () => 0)", 1, MUTANT), touched += 1;
          if (MUTANT === "paste-generation-fence" && file === "unified_terminal_page.ts") source = replace(source, '      () => !this.closed && !paste.disabled,\n      () => this.keyInteractionGeneration,', '      () => !this.closed && !paste.disabled,\n      () => 0,', 1, MUTANT), touched += 1;
          if (MUTANT === "keyboard-enter-repeat-provenance" && file === "tap_activation.ts") source = replace(source, '    if (event.repeat) {\n      if (state.kind === "idle") {\n        state = Object.freeze({ kind: "canceled", key, generation: interactionGeneration(), released: false, pendingClick: true });\n      } else if (state.key === key && !state.released) {\n        state = Object.freeze({ ...state, pendingClick: true });\n      }\n      return;\n    }', '    if (event.repeat) {\n      clear();\n      return;\n    }', 1, MUTANT), touched += 1;
          if (MUTANT === "typography-layout-reanchor" && file === "composer.ts") source = replace(source, '    this.typographyPositionFrame = window.requestAnimationFrame(() => {', '    return;\n    this.typographyPositionFrame = window.requestAnimationFrame(() => {', 1, MUTANT), touched += 1;
          return { contents: source, loader: file.endsWith(".css") ? "css" : "ts", resolveDir: path.dirname(args.path) };
        });
      },
    }],
  });
  const expectedTouches = MUTANT === "session-generation-provider-omission" || MUTANT === "session-sheet-generation-provider-omission" ? 2 : 1;
  assert(touched === expectedTouches, `${MUTANT} mutant touched ${touched} source modules, expected ${expectedTouches}`);
  fs.writeFileSync(path.join(dist, "app.meta.json"), JSON.stringify(result.metafile));
  const cleanup = () => fs.rmSync(root, { recursive: true, force: true });
  process.once("exit", cleanup);
  return { root, cleanup: () => { process.removeListener("exit", cleanup); cleanup(); } };
}

function wire(snapshot) {
  return Object.freeze({
    attachments: snapshot.attachments.length,
    inputs: snapshot.attachments.reduce((sum, item) => sum + item.inputs.length, 0),
    resizes: snapshot.attachments.reduce((sum, item) => sum + (item.resizes || 0), 0),
  });
}

function allInputs(snapshot) { return snapshot.attachments.flatMap((item) => item.inputs); }

async function main() {
  assert(SHAPES.length > 0, `no terminal controls shape matched ${selectedCase}`);
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const playwright = require(MODULE);
  assert(playwright[ENGINE], `Playwright has no ${ENGINE} engine`);
  const fixtureUI = await prepareFixtureUI();
  const fixture = await startFixture(fixtureUI.root, { tls: ENGINE === "webkit" });
  const control = (input) => requestJSON(`${fixture.origin}/__fixture/control`, "POST", input);
  const snapshot = () => requestJSON(`${fixture.origin}/__fixture/control`);
  const waitSnapshot = async (predicate, timeout = 8_000) => {
    const deadline = Date.now() + timeout;
    let last;
    while (Date.now() < deadline) {
      last = await snapshot();
      if (predicate(last)) return last;
      await delay(40);
    }
    throw new Error(`fixture wait timed out: ${JSON.stringify(last)}`);
  };
  const fresh = async (purpose = "control") => {
    const inventory = await requestJSON(`${fixture.origin}/api/inventory`);
    const session = inventory.realms[0].servers[0].sessions[0];
    return { session, handle: session.handles[purpose] };
  };
  const terminalURL = ({ session, handle }, purpose = "control") => `${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({
    handle, mode: purpose, history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev",
  })}`;
  const browser = await playwright[ENGINE].launch({
    headless: true,
    ignoreDefaultArgs: ["--hide-scrollbars"],
    ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
  });
  const evidence = { engine: ENGINE, mutant: MUTANT || null, themes: THEME_IDS, cases: [] };
  try {
    if (!MUTANT) evidence.composerScrollGestures = await require("./composer_scroll_gesture_browser.cjs")(browser, ENGINE);
    for (let shapeIndex = 0; shapeIndex < SHAPES.length; shapeIndex += 1) {
      const shape = SHAPES[shapeIndex];
      const detailed = shapeIndex === 0;
      await control({ reset: true, holdModeGrant: detailed });
      const context = await browser.newContext({
        viewport: { width: shape.width, height: shape.height },
        isMobile: shape.touch, hasTouch: shape.touch, deviceScaleFactor: shape.touch ? 2 : 1,
        ignoreHTTPSErrors: true,
      });
      const page = await context.newPage();
      const browserErrors = [];
      const httpErrors = [];
      const socketInputFrames = [];
      let pagePreferenceGets = 0;
      page.on("pageerror", (error) => browserErrors.push(String(error)));
      page.on("console", (message) => { if (message.type() === "error") browserErrors.push(message.text()); });
      page.on("response", (response) => { if (response.status() >= 400) httpErrors.push({ status: response.status(), url: response.url() }); });
      page.on("request", (request) => {
        if (request.method() === "GET" && new URL(request.url()).pathname === "/api/preferences") pagePreferenceGets += 1;
      });
      page.on("websocket", (socket) => socket.on("framesent", (frame) => {
        try {
          const decoded = JSON.parse(String(frame.payload));
          if (decoded?.type === "INPUT") socketInputFrames.push(decoded);
        } catch { /* protocol/liveness frames that are not JSON objects */ }
      }));
      if (detailed && !MUTANT) await page.addInitScript(() => {
        // Observe registration identity, not callback names or source text.
        // Unrelated page listeners form the closed-popover baseline.
        const add = EventTarget.prototype.addEventListener;
        const remove = EventTarget.prototype.removeEventListener;
        const viewport = window.visualViewport;
        const identities = new WeakMap();
        const active = new Map();
        let nextId = 0;
        const registration = (target, type, listener, options) => {
          const owner = target === document && ["pointerdown", "keydown"].includes(type) ? "document"
            : target === window && type === "resize" ? "window"
              : target === viewport && ["resize", "scroll"].includes(type) ? "viewport" : null;
          if (!owner || !listener || !["function", "object"].includes(typeof listener)) return null;
          if (!identities.has(listener)) identities.set(listener, ++nextId);
          const capture = typeof options === "boolean" ? options : !!options?.capture;
          return { key: `${owner}:${type}:${capture}:${identities.get(listener)}`, target: owner, type, capture };
        };
        EventTarget.prototype.addEventListener = function (type, listener, options) {
          add.call(this, type, listener, options);
          const item = registration(this, type, listener, options);
          if (item) active.set(item.key, item);
        };
        EventTarget.prototype.removeEventListener = function (type, listener, options) {
          remove.call(this, type, listener, options);
          const item = registration(this, type, listener, options);
          if (item) active.delete(item.key);
        };
        window.__terminal_controlsTypographyListenerLedger = {
          active: () => Array.from(active.values()),
          stop: () => {
            EventTarget.prototype.addEventListener = add;
            EventTarget.prototype.removeEventListener = remove;
          },
        };
      });
      await page.goto(terminalURL(await fresh()), { waitUntil: "load" });
      await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"), undefined, { timeout: 10_000 });
      await delay(150);

      const uiState = () => page.evaluate(() => {
        const q = (selector) => document.querySelector(selector);
        const rect = (node) => {
          if (!node) return null;
          const value = node.getBoundingClientRect();
          return { x: value.x, y: value.y, right: value.right, bottom: value.bottom, width: value.width, height: value.height };
        };
        const active = document.activeElement;
        const textarea = q(".attachment-page__composer-textarea");
        const helper = q(".xterm-helper-textarea");
        const popover = q(".attachment-page__composer-typography-popover");
        const trigger = q(".attachment-page__composer-typography-trigger");
        const standardRow = q(".persea-unified-keybar-row");
        const catalog = q('.persea-terminal-keys:not([hidden])[data-view="Fn"][data-group="Function"]');
        const row = catalog?.querySelector(".persea-terminal-keys__grid") || standardRow;
        const triggerStyle = trigger ? getComputedStyle(trigger) : null;
        const expectedNonce = q('meta[name="persea-style-nonce"]')?.getAttribute("content") || "";
        return {
          active: active === textarea ? "composer" : active === helper ? "xterm" : active === document.body ? "body" : active?.className || active?.tagName || "none",
          activeLabel: active?.getAttribute?.("aria-label") || active?.textContent?.trim() || "",
          composerOpen: q(".attachment-page__composer")?.dataset.open,
          composerMode: q(".attachment-page__composer")?.dataset.mode,
          composerFont: q(".persea-unified-terminal")?.dataset.composerFont,
          popoverHidden: popover?.hidden,
          popoverStatus: popover?.dataset.status,
          popoverPlacement: popover?.dataset.placement || null,
          popoverMessage: q(".attachment-page__composer-typography-status")?.value || "",
          triggerExpanded: trigger?.getAttribute("aria-expanded"),
          triggerText: trigger?.textContent || "", triggerLabel: trigger?.getAttribute("aria-label") || "",
          triggerPaint: triggerStyle ? { background: triggerStyle.backgroundColor, border: triggerStyle.borderColor, shadow: triggerStyle.boxShadow } : null,
          triggerRect: rect(trigger), popoverRect: rect(popover), panelRect: rect(q(".attachment-page__composer")),
          headerRect: rect(q(".attachment-page__composer-header")), closeRect: rect(q(".attachment-page__composer-close")),
          selection: textarea ? { start: textarea.selectionStart, end: textarea.selectionEnd, direction: textarea.selectionDirection, top: textarea.scrollTop, left: textarea.scrollLeft } : null,
          visualHeight: window.visualViewport?.height || window.innerHeight,
          visualRect: (() => { const value = window.visualViewport; const x = value?.offsetLeft || 0; const y = value?.offsetTop || 0; const width = value?.width || window.innerWidth; const height = value?.height || window.innerHeight; return { x, y, right: x + width, bottom: y + height, width, height }; })(),
          keyBarHidden: q(".persea-unified-keybar")?.hidden,
          bankToggleRect: rect(q(".persea-unified-keybar-key--bank")),
          bankToggleText: q(".persea-unified-keybar-key--bank")?.textContent,
          bankTogglePressed: q(".persea-unified-keybar-key--bank")?.getAttribute("aria-pressed"),
          bank: catalog ? "function" : standardRow?.getAttribute("data-bank") || null, rowCount: document.querySelectorAll(".persea-unified-keybar-row").length,
          rowAttributes: standardRow ? Array.from(standardRow.attributes, (attribute) => [attribute.name, attribute.value]) : [],
          keyLabels: row ? Array.from(row.children).map((node) => node.textContent) : [],
          keyRects: row ? Array.from(row.children).map(rect) : [],
          rowClientWidth: catalog?.clientHeight || row?.clientWidth || 0, rowScrollWidth: catalog?.scrollHeight || row?.scrollWidth || 0, rowScrollLeft: catalog?.scrollTop || row?.scrollLeft || 0,
          functionTilePressed: q("[aria-label=\"Open Terminal Keys\"]")?.getAttribute("aria-pressed"),
          functionTileDisabled: q("[aria-label=\"Open Terminal Keys\"]")?.disabled,
          functionTileReason: q("[aria-label=\"Open Terminal Keys\"] .persea-unified-sheet__reason")?.textContent || "",
          sheetHidden: q(".persea-unified-sheet")?.hidden,
          viewHidden: q(".persea-unified-view-popover")?.hidden,
          announcement: q(".persea-unified-keybar-announcement")?.textContent || "",
          plusDisabled: q('[aria-label="Increase composer text size"]')?.getAttribute("aria-disabled") === "true",
          minusDisabled: q('[aria-label="Decrease composer text size"]')?.getAttribute("aria-disabled") === "true",
          resetDisabled: q('.attachment-page__composer-typography-reset')?.getAttribute("aria-disabled") === "true",
          plusNativeDisabled: q('[aria-label="Increase composer text size"]')?.disabled,
          minusNativeDisabled: q('[aria-label="Decrease composer text size"]')?.disabled,
          resetNativeDisabled: q('.attachment-page__composer-typography-reset')?.disabled,
          popoverControls: popover ? Array.from(popover.querySelectorAll("button"), (node) => ({
            label: node.getAttribute("aria-label") || node.textContent?.trim() || "",
            disabled: node.disabled || node.getAttribute("aria-disabled") === "true",
            nativeDisabled: node.disabled,
            ariaDisabled: node.getAttribute("aria-disabled") === "true",
            rect: rect(node),
          })) : [],
          sizeActionInPopover: popover?.contains(q(".attachment-page__composer-size")) === true,
          styleCount: document.querySelectorAll("style").length,
          invalidStyleNonces: Array.from(document.querySelectorAll("style")).filter((node) => node.nonce !== expectedNonce).length,
          horizontalOverflow: document.documentElement.scrollWidth > window.innerWidth + 1,
          panelOverflow: q(".attachment-page__composer") ? getComputedStyle(q(".attachment-page__composer")).overflow : "",
          coarsePointer: matchMedia("(pointer: coarse)").matches,
          maxTouchPoints: navigator.maxTouchPoints,
          standardCtrlPressed: window.__terminal_controlsStandardNodes?.[2]?.getAttribute?.("aria-pressed") || null,
          // Favorites has no dedicated modifier row. Check every Ctrl control
          // currently rendered, including the persistent accessory control.
          ctrlSurfaces: Array.from(document.querySelectorAll('[data-bar-modifier="ctrl"], .persea-terminal-keys__modifier[aria-label="Ctrl"]'), (node) => node.getAttribute('aria-pressed')),
          keysCtrlPressed: q(".persea-terminal-keys__modifier[aria-label=\"Ctrl\"]")?.getAttribute("aria-pressed") || null,
          keysCtrlDisabled: q(".persea-terminal-keys__modifier[aria-label=\"Ctrl\"]")?.disabled,
          keysCtrlReason: q(".persea-terminal-keys__status")?.textContent || "",
        };
      });
      const stableBox = (a, b) => a && b && ["x", "y", "width", "height"].every((key) => Math.abs(a[key] - b[key]) <= 0.75);
      const shot = async (name) => {
        // Playwright injects a screenshot stylesheet in WebKit, which the
        // production CSP correctly rejects. Chromium supplies the visual
        // artifacts; both engines still run every DOM and style assertion.
        if (!MUTANT && ENGINE === "chromium") await page.screenshot({ path: path.join(EVIDENCE, `${shape.name}-${name}.png`), fullPage: true, caret: "initial" });
      };
      const dispatchPreferenceSignal = () => page.evaluate(() => window.dispatchEvent(new StorageEvent("storage", { key: "persea-terminal.operator-preferences-hint.v1" })));
      const publishPreference = async (patch) => {
        const before = await snapshot();
        await control({ preferences: { ...patch, revision: before.preferences.revision + 1, stored: true, available: true }, preferencesAvailable: true });
        await dispatchPreferenceSignal();
        return waitSnapshot((value) => value.preferences.revision === before.preferences.revision + 1);
      };
      const openComposer = async () => {
        if ((await uiState()).composerOpen !== "true") await page.locator(".persea-unified-composer-toggle").click();
        await page.waitForFunction(() => document.querySelector(".attachment-page__composer")?.dataset.open === "true");
      };
      const closeComposer = async () => {
        if ((await uiState()).composerOpen === "true") await page.locator(".attachment-page__composer-close").click();
      };
      const enterFunctionBank = async () => {
        if (await page.locator('.persea-terminal-keys').isHidden()) {
          if (await page.locator('.persea-unified-sheet').isHidden()) await page.locator('.persea-unified-quick-actions').click();
          await page.getByRole('button', { name: 'Open Terminal Keys', exact: true }).click();
        }
        const fn = page.getByRole('button', { name: 'Function keys F1–F12', exact: true });
        const labelFits = await fn.evaluate((node) => node.scrollWidth <= node.clientWidth + 1);
        assert(labelFits, `${shape.name}: Function keys action label is truncated`);
        if (await fn.getAttribute('aria-pressed') !== 'true') await fn.click();
        await page.waitForFunction(() => document.querySelector('.persea-terminal-keys:not([hidden])[data-view="Fn"][data-group="Function"]') !== null);
      };
      const keyboardHeight = Math.max(300, shape.height - 336);
      const showKeyBar = async () => {
        await closeComposer();
        await page.locator(".xterm-helper-textarea").focus();
        await page.setViewportSize({ width: shape.width, height: keyboardHeight });
        await page.waitForFunction(() => document.querySelector(".persea-unified-keybar")?.hidden === false, undefined, { timeout: 5_000 });
      };
      const hideKeyBar = async () => {
        await page.setViewportSize({ width: shape.width, height: shape.height });
        await page.waitForFunction(() => !document.querySelector(".persea-unified-terminal")?.style.height, undefined, { timeout: 5_000 });
      };

      assert((await uiState()).rowCount === 1, `${shape.name}: key bar has more than one row`);
      const caseEvidence = { shape: shape.name, touch: shape.touch };
      await page.evaluate(() => {
        const row = document.querySelector(".persea-unified-keybar-row");
        window.__terminal_controlsStandardNodes = Array.from(row.children);
        window.__terminal_controlsStandardRowAttributes = Array.from(row.attributes, (attribute) => [attribute.name, attribute.value]);
      });
      const pristine = await uiState();
      assert(pristine.bank === null, `${shape.name}: standard row exposes a transient bank marker`);
      assert(pristine.panelOverflow === "hidden", `${shape.name}: composer containment was disabled (${pristine.panelOverflow})`);
      await page.locator(".persea-unified-quick-actions").click();
      const hiddenFunctionTile = await uiState();
      assert(hiddenFunctionTile.sheetHidden === false && hiddenFunctionTile.functionTileDisabled === false && hiddenFunctionTile.functionTileReason === "", `${shape.name}: keyboard-hidden Function keys access is unavailable: ${JSON.stringify(hiddenFunctionTile)}`);
      await page.locator(".persea-unified-quick-actions").click();
      caseEvidence.hiddenFunctionTile = { disabled: hiddenFunctionTile.functionTileDisabled, reason: hiddenFunctionTile.functionTileReason };

      // The authority floor is exercised before the held MODE(CONTROL) grant.
      if (detailed && shape.touch) {
        await showKeyBar();
        await enterFunctionBank();
        const before = allInputs(await snapshot()).length;
        const socketBefore = socketInputFrames.length;
        await page.getByRole("button", { name: "F1", exact: true }).click();
        await delay(100);
        const after = allInputs(await snapshot()).length;
        assert(after === before && socketInputFrames.length === socketBefore, `${shape.name}: F1 sent INPUT before MODE(CONTROL)`);
        await page.getByRole("button", { name: "Terminal Keys" }).click();
        await hideKeyBar();
        await control({ releaseMode: true });
        await delay(100);
        caseEvidence.preControlInputDelta = after - before;
      } else if (detailed) {
        await control({ releaseMode: true });
      }

      // Composer disclosure: open/close from an unfocused posture, then change
      // from a focused textarea with a nontrivial selection and scroll offset.
      await openComposer();
      await page.evaluate(() => document.activeElement?.blur());
      await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      const unfocusedBefore = await uiState();
      const wireBeforeOpen = wire(await snapshot());
      const typographyListenerBaseline = detailed && !MUTANT
        ? await page.evaluate(() => window.__terminal_controlsTypographyListenerLedger.active()) : null;
      const typographyListenerDelta = async () => {
        const active = await page.evaluate(() => window.__terminal_controlsTypographyListenerLedger.active());
        return active.filter((item) => !typographyListenerBaseline.some((baseline) => baseline.key === item.key));
      };
      const assertTypographyListeners = async (expectedOpen, phase) => {
        const delta = await typographyListenerDelta();
        const actual = delta.map(({ target, type, capture }) => `${target}:${type}:${capture}`).sort();
        const expected = expectedOpen ? ["document:pointerdown:false", "document:keydown:true", "window:resize:false", "viewport:resize:false", "viewport:scroll:false"].sort() : [];
        assert(JSON.stringify(actual) === JSON.stringify(expected),
          `${shape.name}: typography global listeners do not match ${phase} lifecycle: ${JSON.stringify({ expected, actual })}`);
        return delta;
      };
      await page.locator(".attachment-page__composer-typography-trigger").click();
      const open = await uiState();
      if (typographyListenerBaseline) caseEvidence.typographyListenerLifecycle = {
        closedBaseline: typographyListenerBaseline,
        open: await assertTypographyListeners(true, "open"),
        disposal: "source-reviewed; pagehide is not a Composer.dispose browser proof",
      };
      assert(open.active === unfocusedBefore.active, `${shape.name}: typography opening changed focus (${unfocusedBefore.active} -> ${open.active})`);
      assert(open.popoverHidden === false && open.triggerExpanded === "true", `${shape.name}: typography popover did not open`);
      assert(open.triggerText === "11" && /text size 11 pixels/i.test(open.triggerLabel), `${shape.name}: disclosure does not communicate current size`);
      assert(JSON.stringify(open.triggerPaint) !== JSON.stringify(unfocusedBefore.triggerPaint), `${shape.name}: open disclosure has no active-state paint`);
      assert(open.visualHeight === unfocusedBefore.visualHeight && open.keyBarHidden === unfocusedBefore.keyBarHidden, `${shape.name}: typography opening raised or changed the keyboard`);
      assert(open.invalidStyleNonces === 0, `${shape.name}: a style node is outside the CSP nonce boundary`);
      assert(stableBox(unfocusedBefore.panelRect, open.panelRect) && stableBox(unfocusedBefore.headerRect, open.headerRect) && stableBox(unfocusedBefore.triggerRect, open.triggerRect), `${shape.name}: opening typography moved composer boxes: ${JSON.stringify({ before: unfocusedBefore, open })}`);
      assert(open.sizeActionInPopover, `${shape.name}: displaced panel-height action is not inside typography popover`);
      assert(open.popoverRect && open.popoverRect.x >= open.visualRect.x - 0.75 && open.popoverRect.right <= open.visualRect.right + 0.75 && open.popoverRect.y >= open.visualRect.y - 0.75 && open.popoverRect.bottom <= open.visualRect.bottom + 0.75, `${shape.name}: typography popover escaped the visual viewport: ${JSON.stringify({ popover: open.popoverRect, viewport: open.visualRect })}`);
      assert(open.popoverPlacement === "top", `${shape.name}: bottom-anchored typography did not prefer the space above`);
      const minimum = shape.touch ? 44 : 40;
      assert(open.triggerRect.width >= minimum && open.triggerRect.height >= minimum, `${shape.name}: typography disclosure is under ${minimum}px`);
      if (shape.touch) {
        assert(open.popoverControls.length >= 4 && open.popoverControls.every((control) => control.rect?.width >= 44 && control.rect?.height >= 44), `${shape.name}: portal control under 44px on coarse pointer: ${JSON.stringify(open.popoverControls)}`);
      }
      // Competing disclosures claim the page's single-popover owner.
      const quickActions = page.locator(".persea-unified-quick-actions");
      await quickActions.focus();
      await page.keyboard.press("Enter");
      let coordinated = await uiState();
      assert(coordinated.popoverHidden === true && coordinated.sheetHidden === false, `${shape.name}: sheet did not close typography through the shared owner`);
      await page.keyboard.press("Enter");
      await page.locator(".attachment-page__composer-typography-trigger").click();
      await page.locator(".persea-unified-view-disclosure").click();
      coordinated = await uiState();
      assert(coordinated.popoverHidden === true && coordinated.viewHidden === false, `${shape.name}: View did not close typography through the shared owner`);
      const typographyTrigger = page.locator(".attachment-page__composer-typography-trigger");
      await typographyTrigger.focus();
      await page.keyboard.press("Enter");
      coordinated = await uiState();
      assert(coordinated.popoverHidden === false && coordinated.viewHidden === true, `${shape.name}: typography did not close View through the shared owner`);
      const preFlip = coordinated;

      // Move only the anchored panel's paint to exercise the opposite vertical
      // placement; transforms do not participate in layout, so restoring it
      // also verifies that the portal never shifted the composer.
      const flipped = await page.evaluate(() => {
        const anchorRoot = document.querySelector(".attachment-page__composer-typography");
        const trigger = document.querySelector(".attachment-page__composer-typography-trigger");
        const before = trigger.getBoundingClientRect();
        anchorRoot.style.transform = `translateY(${12 - before.top}px)`;
        window.visualViewport.dispatchEvent(new Event("resize"));
        const popover = document.querySelector(".attachment-page__composer-typography-popover");
        const rect = popover.getBoundingClientRect();
        return { placement: popover.dataset.placement, rect: { x: rect.x, y: rect.y, right: rect.right, bottom: rect.bottom } };
      });
      assert(flipped.placement === "bottom" && flipped.rect.y >= -0.75 && flipped.rect.bottom <= open.visualRect.bottom + 0.75, `${shape.name}: typography did not flip below a top anchor: ${JSON.stringify(flipped)}`);
      await page.evaluate(() => {
        document.querySelector(".attachment-page__composer-typography").style.removeProperty("transform");
        window.visualViewport.dispatchEvent(new Event("scroll"));
      });
      const restoredPopover = await uiState();
      assert(stableBox(preFlip.panelRect, restoredPopover.panelRect) && stableBox(preFlip.headerRect, restoredPopover.headerRect), `${shape.name}: portal positioning shifted the composer after restore: ${JSON.stringify({ before: { panel: preFlip.panelRect, header: preFlip.headerRect }, restored: { panel: restoredPopover.panelRect, header: restoredPopover.headerRect } })}`);
      if (detailed && (!MUTANT || MUTANT === "typography-layout-reanchor")) {
        // Composer auto-grow and a face change move the fixed portal's anchor
        // without a window/visual-viewport event. The post-layout position
        // frame must follow both changes and remain clamped to the same visual
        // viewport instead of leaving a stale overlay over the draft.
        const anchorGap = (state) => state.popoverPlacement === "top"
          ? state.triggerRect.y - state.popoverRect.bottom
          : state.popoverRect.y - state.triggerRect.bottom;
        const assertAnchored = (state, label) => {
          const gap = anchorGap(state);
          assert(Math.abs(gap - 6) <= 1.25
            && state.popoverRect.x >= state.visualRect.x - 0.75 && state.popoverRect.right <= state.visualRect.right + 0.75
            && state.popoverRect.y >= state.visualRect.y - 0.75 && state.popoverRect.bottom <= state.visualRect.bottom + 0.75,
          `${shape.name}: typography portal did not follow composer layout (${label}): ${JSON.stringify({ gap, state })}`);
          return gap;
        };
        await page.evaluate(() => {
          window.__terminal_controlsUnexpectedLayoutResize = 0;
          window.__terminal_controlsLayoutResizeListener = () => { window.__terminal_controlsUnexpectedLayoutResize += 1; };
          window.addEventListener("resize", window.__terminal_controlsLayoutResizeListener);
          const popover = document.querySelector(".attachment-page__composer-typography-popover");
          if (!(popover instanceof HTMLElement)) throw new Error("typography layout probe is missing");
          // Seed a stale portal coordinate. Auto-grow is the only event that
          // follows; a post-layout anchor pass must repair it without relying
          // on a synthetic resize notification.
          popover.style.left = "8px";
          popover.style.top = "8px";
        });
        const beforeAutoGrow = await uiState();
        await page.locator(".attachment-page__composer-textarea").fill(Array.from({ length: 12 }, (_, index) => `layout line ${index + 1}`).join("\n"));
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        const afterAutoGrow = await uiState();
        const autoGrowGap = assertAnchored(afterAutoGrow, "auto-grow");
        assert(Math.abs(afterAutoGrow.panelRect.height - beforeAutoGrow.panelRect.height) >= 2,
          `${shape.name}: typography auto-grow proof did not reflow the panel: ${JSON.stringify({ beforeAutoGrow, afterAutoGrow })}`);

        const beforeLayoutFont = await snapshot();
        await page.evaluate(() => {
          const terminal = document.querySelector(".persea-unified-terminal");
          const popover = document.querySelector(".attachment-page__composer-typography-popover");
          if (!(terminal instanceof HTMLElement) || !(popover instanceof HTMLElement)) throw new Error("typography font probe is missing");
          window.__terminal_controlsFontLayoutProbe = false;
          window.__terminal_controlsFontLayoutObserver = new MutationObserver(() => {
            if (terminal.dataset.composerFont !== "12") return;
            window.__terminal_controlsFontLayoutProbe = true;
            popover.style.left = "8px";
            popover.style.top = "8px";
            window.__terminal_controlsFontLayoutObserver.disconnect();
          });
          window.__terminal_controlsFontLayoutObserver.observe(terminal, { attributes: true, attributeFilter: ["data-composer-font"] });
        });
        await page.getByRole("button", { name: "Increase composer text size" }).click();
        await waitSnapshot((value) => value.counters.preferencesPut === beforeLayoutFont.counters.preferencesPut + 1);
        await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "12"
          && document.querySelector(".attachment-page__composer-typography-popover")?.dataset.status === "ready");
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        const afterLayoutFont = await uiState();
        const fontGap = assertAnchored(afterLayoutFont, "font change");
        const layoutSignals = await page.evaluate(() => {
          window.removeEventListener("resize", window.__terminal_controlsLayoutResizeListener);
          delete window.__terminal_controlsLayoutResizeListener;
          window.__terminal_controlsFontLayoutObserver.disconnect();
          return { unexpectedResizeEvents: window.__terminal_controlsUnexpectedLayoutResize, fontProbe: window.__terminal_controlsFontLayoutProbe };
        });
        assert(layoutSignals.unexpectedResizeEvents === 0 && layoutSignals.fontProbe === true,
          `${shape.name}: typography portal did not follow composer layout (font frame): ${JSON.stringify(layoutSignals)}`);
        await page.getByRole("button", { name: "Reset composer text size to 11 pixels" }).click();
        await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "11"
          && document.querySelector(".attachment-page__composer-typography-popover")?.dataset.status === "ready");
        caseEvidence.typographyLayoutReanchor = { before: beforeAutoGrow, autoGrow: afterAutoGrow, font: afterLayoutFont, autoGrowGap, fontGap, layoutSignals };
      }
      await page.evaluate(() => document.activeElement?.blur());
      const closeFocusBefore = (await uiState()).active;
      await shot("composer-popover");
      await page.locator(".attachment-page__composer-typography-trigger").click();
      assert((await uiState()).active === closeFocusBefore, `${shape.name}: typography close changed focus`);
      if (typographyListenerBaseline) caseEvidence.typographyListenerLifecycle.closed = await assertTypographyListeners(false, "closed");

      const textarea = page.locator(".attachment-page__composer-textarea");
      await textarea.fill("zero one two three four five six seven eight nine\nline two\nline three\nline four\nline five\nline six\nline seven\nline eight");
      await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      await page.evaluate(() => {
        const node = document.querySelector(".attachment-page__composer-textarea");
        node.focus({ preventScroll: true });
        node.setSelectionRange(5, 18, "forward");
        node.scrollTop = Math.max(1, node.scrollHeight - node.clientHeight);
        node.scrollLeft = 0;
      });
      const selectionBefore = (await uiState()).selection;
      await page.locator(".attachment-page__composer-typography-trigger").click();
      if (typographyListenerBaseline) caseEvidence.typographyListenerLifecycle.reopened = await assertTypographyListeners(true, "reopened");
      await page.keyboard.press("Escape");
      const escaped = await uiState();
      assert(escaped.popoverHidden === true && escaped.composerOpen === "true" && escaped.active === "composer", `${shape.name}: Escape did not close only the typography disclosure`);
      assert(escaped.selection.start === selectionBefore.start && escaped.selection.end === selectionBefore.end, `${shape.name}: Escape changed the textarea selection`);
      if (typographyListenerBaseline) {
        caseEvidence.typographyListenerLifecycle.escapeClosed = await assertTypographyListeners(false, "Escape-closed");
        await page.evaluate(() => {
          window.__terminal_controlsTypographyListenerLedger.stop();
          delete window.__terminal_controlsTypographyListenerLedger;
        });
      }
      await page.locator(".attachment-page__composer-typography-trigger").click();
      let observer;
      if (detailed) {
        observer = await context.newPage();
        observer.on("pageerror", (error) => browserErrors.push(String(error)));
        observer.on("console", (message) => { if (message.type() === "error") browserErrors.push(message.text()); });
        observer.on("response", (response) => { if (response.status() >= 400) httpErrors.push({ status: response.status(), url: response.url() }); });
        await observer.goto(terminalURL(await fresh("observe"), "observe"), { waitUntil: "load" });
        await observer.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"), undefined, { timeout: 10_000 });
      }
      const beforePlus = await snapshot();
      const preferenceGetsBeforeActions = pagePreferenceGets;
      const styleCount = (await uiState()).styleCount;
      // Keep the original selectionBefore invariant; these action-local metrics
      // distinguish earlier disclosure drift from typography/layout restoration.
      const typographyScrollMetrics = () => page.evaluate(() => {
        const node = document.querySelector(".attachment-page__composer-textarea");
        if (!(node instanceof HTMLTextAreaElement)) return null;
        const style = getComputedStyle(node);
        return {
          selection: { start: node.selectionStart, end: node.selectionEnd, direction: node.selectionDirection, top: node.scrollTop, left: node.scrollLeft },
          scrollHeight: node.scrollHeight, clientHeight: node.clientHeight,
          fontSize: style.fontSize, lineHeight: style.lineHeight, fontFamily: style.fontFamily,
          fontsStatus: document.fonts.status,
        };
      });
      const immediateBeforePlus = await typographyScrollMetrics();
      await page.getByRole("button", { name: "Increase composer text size" }).click();
      const afterPlus = await waitSnapshot((value) => value.counters.preferencesPut === beforePlus.counters.preferencesPut + 1);
      const plusOperation = afterPlus.preferenceOperations.at(-1);
      assert(plusOperation.composer_font_size === 12 && plusOperation.if_match === `"${beforePlus.preferences.revision}"` && plusOperation.outcome === "saved", `${shape.name}: typography PUT/CAS drifted: ${JSON.stringify(plusOperation)}`);
      await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "12" && document.querySelector(".attachment-page__composer-typography-popover")?.dataset.status === "ready");
      if (observer) await observer.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "12", undefined, { timeout: 5_000 });
      // Saving the preference precedes the frame that repairs font reflow.
      // Observe that repair before asserting the original selection posture.
      await page.waitForFunction(top => {
        const textarea = document.querySelector(".attachment-page__composer-textarea");
        return textarea instanceof HTMLTextAreaElement && Math.abs(textarea.scrollTop - top) <= 1;
      }, selectionBefore.top, { polling: "raf", timeout: 5_000 });
      const changed = await uiState();
      assert(changed.active === "composer", `${shape.name}: typography change moved focus from textarea`);
      assert(changed.selection.start === selectionBefore.start && changed.selection.end === selectionBefore.end && changed.selection.direction === selectionBefore.direction && Math.abs(changed.selection.top - selectionBefore.top) <= 1, `${shape.name}: typography change lost selection or textarea scroll: ${JSON.stringify({ before: selectionBefore, immediateBeforePlus, after: changed.selection, finalMetrics: await typographyScrollMetrics() })}`);
      assert(changed.composerMode === open.composerMode, `${shape.name}: typography change altered Code/Prose mode`);
      assert(changed.styleCount === styleCount, `${shape.name}: typography action created a CSP style node`);
      assert(changed.visualHeight === open.visualHeight && changed.keyBarHidden === open.keyBarHidden, `${shape.name}: typography change raised or changed the keyboard`);
      await page.getByRole("button", { name: "Reset composer text size to 11 pixels" }).click();
      const afterReset = await waitSnapshot((value) => value.counters.preferencesPut === afterPlus.counters.preferencesPut + 1);
      await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "11"
        && document.querySelector(".attachment-page__composer-typography-popover")?.dataset.status === "ready");
      assert(afterReset.preferenceOperations.at(-1).composer_font_size === 11, `${shape.name}: Reset did not store explicit 11px`);
      assert(afterReset.preferences.composer_font_size === 11 && afterReset.preferences.stored === true, `${shape.name}: explicit default 11px was collapsed into an implicit default`);
      let keyboardSuccess;
      if (detailed) {
        const plusLabel = "Increase composer text size";
        const resetLabel = "Reset composer text size to 11 pixels";

        const beforeKeyboardPlus = await snapshot();
        await control({ holdPreferencePuts: 1 });
        await page.getByRole("button", { name: plusLabel }).focus();
        await page.keyboard.press("Enter");
        await waitSnapshot((value) => value.pendingPreferencePuts === 1);
        const plusSaving = await uiState();
        assert(plusSaving.activeLabel === plusLabel && plusSaving.plusDisabled === true && plusSaving.plusNativeDisabled === false,
          `${shape.name}: typography keyboard focus destination became unusable while Plus saved: ${JSON.stringify(plusSaving)}`);
        await control({ releasePreferencePut: true });
        const afterKeyboardPlus = await waitSnapshot((value) => value.counters.preferencesPut === beforeKeyboardPlus.counters.preferencesPut + 1
          && value.preferenceOperations.at(-1)?.outcome === "saved");
        await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "12"
          && document.querySelector(".attachment-page__composer-typography-popover")?.dataset.status === "ready");
        const plusSettled = await uiState();
        assert(plusSettled.activeLabel === plusLabel && plusSettled.plusNativeDisabled === false,
          `${shape.name}: typography keyboard focus was not retained after Plus success: ${JSON.stringify(plusSettled)}`);

        const beforeKeyboardReset = afterKeyboardPlus;
        await control({ holdPreferencePuts: 1 });
        await page.getByRole("button", { name: resetLabel }).focus();
        await page.keyboard.press("Space");
        await waitSnapshot((value) => value.pendingPreferencePuts === 1);
        const resetSaving = await uiState();
        assert(resetSaving.activeLabel === resetLabel && resetSaving.resetDisabled === true && resetSaving.resetNativeDisabled === false,
          `${shape.name}: typography keyboard focus was not retained while Reset saved: ${JSON.stringify(resetSaving)}`);
        await control({ releasePreferencePut: true });
        await waitSnapshot((value) => value.counters.preferencesPut === beforeKeyboardReset.counters.preferencesPut + 1
          && value.preferenceOperations.at(-1)?.outcome === "saved");
        await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "11"
          && document.querySelector(".attachment-page__composer-typography-popover")?.dataset.status === "ready");
        const resetSettled = await uiState();
        assert(resetSettled.activeLabel === resetLabel && resetSettled.resetDisabled === true && resetSettled.resetNativeDisabled === false,
          `${shape.name}: typography keyboard focus was not retained after Reset success/default bound: ${JSON.stringify(resetSettled)}`);
        keyboardSuccess = { plusSaving, plusSettled, resetSaving, resetSettled };
      }
      const wireAfterComposer = wire(await snapshot());
      assert(wireAfterComposer.inputs === wireBeforeOpen.inputs && wireAfterComposer.resizes === wireBeforeOpen.resizes && wireAfterComposer.attachments === wireBeforeOpen.attachments + (observer ? 1 : 0), `${shape.name}: composer controls touched terminal wire: ${JSON.stringify({ wireBeforeOpen, wireAfterComposer })}`);
      assert(pagePreferenceGets === preferenceGetsBeforeActions, `${shape.name}: typography activations created a second preference fetch loop`);
      caseEvidence.composer = { open, changed, plusOperation, keyboardSuccess, wireBeforeOpen, wireAfterComposer, crossTab: observer ? "12" : null };
      if (observer) await observer.close();

      if (detailed) {
        // A preference publication captures the textarea posture and queues
        // one repair frame. Intervene in the MutationObserver microtask after
        // capture but before that frame, then prove typing, caret movement,
        // and manual scrolling each win over the stale snapshot. A scroll
        // notification from layout alone must still allow the repair.
        const interleaveTypography = async (kind, size) => {
          await page.evaluate(({ kind, size }) => {
            const terminal = document.querySelector(".persea-unified-terminal");
            const textarea = document.querySelector(".attachment-page__composer-textarea");
            if (!(terminal instanceof HTMLElement) || !(textarea instanceof HTMLTextAreaElement)) throw new Error("typography interleave fixture is missing");
            textarea.focus({ preventScroll: true });
            textarea.scrollTop = 0;
            textarea.setSelectionRange(kind === "caret" ? 1 : 2, kind === "caret" ? 1 : 2, "none");
            window.__terminal_controlsTypographyInterleave = { done: false, kind, size };
            const observer = new MutationObserver(() => {
              if (terminal.dataset.composerFont !== String(size)) return;
              observer.disconnect();
              if (kind === "type") {
                const at = textarea.selectionEnd;
                textarea.setRangeText("Z", at, at, "end");
                textarea.dispatchEvent(new InputEvent("input", { bubbles: true, data: "Z", inputType: "insertText" }));
              } else if (kind === "caret") {
                textarea.setSelectionRange(9, 9, "none");
              } else {
                if (kind === "scroll") textarea.dispatchEvent(new WheelEvent("wheel", { deltaY: 37 }));
                textarea.scrollTop = Math.min(37, Math.max(1, textarea.scrollHeight - textarea.clientHeight));
                textarea.dispatchEvent(new Event("scroll"));
              }
              const expected = {
                value: textarea.value,
                start: textarea.selectionStart,
                end: textarea.selectionEnd,
                top: kind === "reflow" ? 0 : textarea.scrollTop,
                left: textarea.scrollLeft,
              };
              requestAnimationFrame(() => requestAnimationFrame(() => {
                window.__terminal_controlsTypographyInterleave = {
                  done: true,
                  kind,
                  size,
                  expected,
                  actual: {
                    value: textarea.value,
                    start: textarea.selectionStart,
                    end: textarea.selectionEnd,
                    top: textarea.scrollTop,
                    left: textarea.scrollLeft,
                  },
                };
              }));
            });
            observer.observe(terminal, { attributes: true, attributeFilter: ["data-composer-font"] });
          }, { kind, size });
          await publishPreference({ composer_font_size: size });
          await page.waitForFunction(() => window.__terminal_controlsTypographyInterleave?.done === true);
          const proof = await page.evaluate(() => window.__terminal_controlsTypographyInterleave);
          const preserved = kind === "type"
            ? proof.actual.value === proof.expected.value && proof.actual.start === proof.expected.start && proof.actual.end === proof.expected.end
            : kind === "caret"
              ? proof.actual.start === proof.expected.start && proof.actual.end === proof.expected.end
              : Math.abs(proof.actual.top - proof.expected.top) <= 1 && Math.abs(proof.actual.left - proof.expected.left) <= 1;
          if (kind === "reflow") assert(preserved, `${shape.name}: typography layout scroll notification canceled repair: ${JSON.stringify(proof)}`);
          else assert(preserved, `${shape.name}: typography deferred restore overwrote newer interaction: ${JSON.stringify(proof)}`);
          return proof;
        };
        caseEvidence.typographyDeferredInteractions = [
          await interleaveTypography("reflow", 12),
          await interleaveTypography("type", 13),
          await interleaveTypography("caret", 14),
          await interleaveTypography("scroll", 15),
        ];

        // Range/default boundaries remain keyboard-focusable and honest via
        // aria-disabled, but Enter and Space are activation-guarded and never
        // clamp into another PUT.
        await publishPreference({ composer_font_size: 24 });
        await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "24");
        const upperBefore = await snapshot();
        const plusLabel = "Increase composer text size";
        await page.getByRole("button", { name: plusLabel }).focus();
        await page.keyboard.press("Enter");
        await page.keyboard.press("Space");
        await delay(80);
        const upper = await uiState();
        const upperAfter = await snapshot();
        assert(upper.plusDisabled === true && upper.plusNativeDisabled === false && upper.activeLabel === plusLabel
          && upperAfter.counters.preferencesPut === upperBefore.counters.preferencesPut,
        `${shape.name}: plus bound is not focusable aria-disabled with guarded activation: ${JSON.stringify({ upper, before: upperBefore.counters, after: upperAfter.counters })}`);
        await publishPreference({ composer_font_size: 9 });
        await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "9");
        const lowerBefore = await snapshot();
        const minusLabel = "Decrease composer text size";
        await page.getByRole("button", { name: minusLabel }).focus();
        await page.keyboard.press("Space");
        await page.keyboard.press("Enter");
        await delay(80);
        const lower = await uiState();
        const lowerAfter = await snapshot();
        assert(lower.minusDisabled === true && lower.minusNativeDisabled === false && lower.activeLabel === minusLabel
          && lowerAfter.counters.preferencesPut === lowerBefore.counters.preferencesPut,
        `${shape.name}: minus bound is not focusable aria-disabled with guarded activation: ${JSON.stringify({ lower, before: lowerBefore.counters, after: lowerAfter.counters })}`);
        await publishPreference({ composer_font_size: 11 });
        await page.waitForFunction(() => document.querySelector(".persea-unified-terminal")?.dataset.composerFont === "11");
        const resetBefore = await snapshot();
        const resetLabel = "Reset composer text size to 11 pixels";
        await page.getByRole("button", { name: resetLabel }).focus();
        await page.keyboard.press("Enter");
        await page.keyboard.press("Space");
        await delay(80);
        const resetBound = await uiState();
        const resetAfter = await snapshot();
        assert(resetBound.resetDisabled === true && resetBound.resetNativeDisabled === false && resetBound.activeLabel === resetLabel
          && resetAfter.counters.preferencesPut === resetBefore.counters.preferencesPut,
        `${shape.name}: Reset bound is not focusable aria-disabled with guarded activation: ${JSON.stringify({ resetBound, before: resetBefore.counters, after: resetAfter.counters })}`);
        caseEvidence.typographyBounds = { upper, lower, reset: resetBound };
      }

      // Every curated theme paints the same open, bounded controls at every
      // required width in both engines. No theme may introduce a new style
      // node or horizontal page overflow.
      caseEvidence.themeControls = [];
      for (const id of THEME_IDS) {
          await publishPreference({ theme: id });
          await page.waitForFunction((theme) => document.querySelector(".persea-unified-terminal")?.dataset.theme === theme, id);
          const themeState = await page.evaluate(() => {
            const trigger = document.querySelector(".attachment-page__composer-typography-trigger");
            const popover = document.querySelector(".attachment-page__composer-typography-popover");
            const status = document.querySelector(".attachment-page__composer-typography-status");
            const component = document.querySelector('[aria-label="Decrease composer text size"]');
            const style = getComputedStyle(trigger);
            const panel = getComputedStyle(popover);
            const componentStyle = getComputedStyle(component);
            const rect = popover.getBoundingClientRect();
            const normal = {
              border: panel.borderColor,
              status: getComputedStyle(status).color,
              component: {
                color: componentStyle.color,
                background: componentStyle.backgroundColor,
                border: componentStyle.borderColor,
              },
            };
            popover.dataset.status = "conflict";
            status.hidden = false;
            const alert = { border: getComputedStyle(popover).borderColor, status: getComputedStyle(status).color };
            popover.dataset.status = "ready";
            status.hidden = true;
            const viewport = window.visualViewport;
            const x = viewport?.offsetLeft || 0;
            const y = viewport?.offsetTop || 0;
            const width = viewport?.width || window.innerWidth;
            const height = viewport?.height || window.innerHeight;
            return { theme: document.querySelector(".persea-unified-terminal").dataset.theme, triggerColor: style.color, triggerBackground: style.backgroundColor, border: style.borderColor, popoverColor: panel.color, popoverBackground: panel.backgroundColor, normal, alert, rect: { x: rect.x, y: rect.y, right: rect.right, bottom: rect.bottom }, viewport: { x, y, right: x + width, bottom: y + height }, overflow: document.documentElement.scrollWidth > window.innerWidth + 1, styles: document.querySelectorAll("style").length };
          });
          const themeContrast = {
            text: contrast(themeState.popoverColor, themeState.popoverBackground),
            status: contrast(themeState.normal.status, themeState.popoverBackground),
            alert: contrast(themeState.alert.status, themeState.popoverBackground),
            border: contrast(themeState.normal.border, themeState.popoverBackground),
            componentText: contrast(themeState.normal.component.color, themeState.normal.component.background),
            componentFill: contrast(themeState.normal.component.background, themeState.popoverBackground),
            componentBorder: contrast(themeState.normal.component.border, themeState.popoverBackground),
          };
          assert(!themeState.overflow
            && themeState.rect.x >= themeState.viewport.x - 0.75 && themeState.rect.right <= themeState.viewport.right + 0.75
            && themeState.rect.y >= themeState.viewport.y - 0.75 && themeState.rect.bottom <= themeState.viewport.bottom + 0.75
            && themeContrast.text >= 4.5 && themeContrast.status >= 4.5 && themeContrast.alert >= 4.5 && themeContrast.border >= 3
            && themeContrast.componentText >= 4.5 && themeContrast.componentFill > 1.02 && themeContrast.componentBorder >= 3
            && themeState.normal.border !== themeState.alert.border && themeState.styles === styleCount,
          `${shape.name}: ${id} typography semantic contrast/containment failed: ${JSON.stringify({ themeState, themeContrast })}`);
          themeState.contrast = themeContrast;
          caseEvidence.themeControls.push(themeState);
          await shot(`theme-${id}`);
      }

      if (detailed) {
        // Conflict reverts to server authority and names the collision.
        await publishPreference({ theme: "default", composer_font_size: 11 });
        const conflictBefore = await snapshot();
        await control({ holdPreferencePuts: 1 });
        const conflictLabel = "Increase composer text size";
        await page.getByRole("button", { name: conflictLabel }).focus();
        await page.keyboard.press("Space");
        await waitSnapshot((value) => value.pendingPreferencePuts === 1);
        const optimistic = await uiState();
        assert(optimistic.composerFont === "12" && optimistic.popoverStatus === "saving"
          && optimistic.activeLabel === conflictLabel && optimistic.plusDisabled === true && optimistic.plusNativeDisabled === false,
        `${shape.name}: conflict-saving typography focus/state is not immediate and honest: ${JSON.stringify(optimistic)}`);
        await control({ preferences: { composer_font_size: 16, revision: conflictBefore.preferences.revision + 1, stored: true, available: true }, releasePreferencePut: true });
        const conflictAfter = await waitSnapshot((value) => value.preferenceOperations.at(-1)?.outcome === "conflict");
        await page.waitForFunction(() => document.querySelector(".attachment-page__composer-typography-popover")?.dataset.status !== "saving");
        const conflict = await uiState();
        assert(conflict.composerFont === "16" && conflict.popoverStatus === "conflict" && /changed elsewhere/i.test(conflict.popoverMessage)
          && conflict.activeLabel === conflictLabel && conflict.plusNativeDisabled === false,
        `${shape.name}: conflict status/value is not honest (including focus): ${JSON.stringify(conflict)}`);
        assert(conflictAfter.counters.preferencesPut === conflictBefore.counters.preferencesPut + 1, `${shape.name}: conflict activation did not issue exactly one PUT`);

        // A mid-write outage retains the attempted local face and says page-only.
        const unavailableBefore = await snapshot();
        await control({ holdPreferencePuts: 1 });
        const unavailableLabel = "Decrease composer text size";
        await page.getByRole("button", { name: unavailableLabel }).focus();
        await page.keyboard.press("Enter");
        await waitSnapshot((value) => value.pendingPreferencePuts === 1);
        const unavailableSaving = await uiState();
        assert(unavailableSaving.activeLabel === unavailableLabel && unavailableSaving.minusDisabled === true && unavailableSaving.minusNativeDisabled === false,
          `${shape.name}: typography keyboard focus was not retained while Minus saved: ${JSON.stringify(unavailableSaving)}`);
        await control({ preferencesAvailable: false, releasePreferencePut: true });
        const unavailableAfter = await waitSnapshot((value) => value.preferenceOperations.at(-1)?.outcome === "unavailable");
        await page.waitForFunction(() => document.querySelector(".attachment-page__composer-typography-popover")?.dataset.status !== "saving");
        const unavailable = await uiState();
        assert(unavailable.composerFont === "15" && unavailable.popoverStatus === "unavailable" && /page only|unavailable/i.test(unavailable.popoverMessage)
          && unavailable.activeLabel === unavailableLabel && unavailable.minusDisabled === true && unavailable.minusNativeDisabled === false,
        `${shape.name}: unavailable status/value is not honest (including focus): ${JSON.stringify(unavailable)}`);
        assert(unavailableAfter.counters.preferencesPut === unavailableBefore.counters.preferencesPut + 1, `${shape.name}: unavailable activation did not issue exactly one PUT`);
        const notice = await page.locator(".persea-unified-toast").evaluate((node) => {
          const rect = node.getBoundingClientRect();
          return { text: node.textContent, hidden: node.hidden, width: node.clientWidth, height: node.clientHeight,
            contentWidth: node.scrollWidth, contentHeight: node.scrollHeight, left: rect.left, right: rect.right };
        });
        assert(!notice.hidden && /preferences unavailable/.test(notice.text)
          && notice.contentWidth <= notice.width + 1 && notice.contentHeight <= notice.height + 1
          && notice.left >= 0 && notice.right <= shape.width,
        `${shape.name}: status notice clips its explanation: ${JSON.stringify(notice)}`);
        caseEvidence.statusNotice = notice;
        await shot("complete-status-notice");
        await page.setViewportSize({ width: shape.width, height: keyboardHeight });
        await page.waitForFunction(() => {
          const popover = document.querySelector(".attachment-page__composer-typography-popover");
          const viewport = window.visualViewport;
          if (!popover || !viewport) return false;
          const rect = popover.getBoundingClientRect();
          return rect.top >= viewport.offsetTop - 1 && rect.bottom <= viewport.offsetTop + viewport.height + 1;
        });
        const reducedError = await uiState();
        assert(reducedError.popoverRect.y >= reducedError.visualRect.y - 0.75 && reducedError.popoverRect.bottom <= reducedError.visualRect.bottom + 0.75 && reducedError.popoverRect.x >= reducedError.visualRect.x - 0.75 && reducedError.popoverRect.right <= reducedError.visualRect.right + 0.75, `${shape.name}: wrapped unavailable status escaped the reduced visual viewport: ${JSON.stringify(reducedError)}`);
        await page.setViewportSize({ width: shape.width, height: shape.height });
        caseEvidence.preferenceFailures = { optimistic, conflict, unavailableSaving, unavailable, reducedError };
      }

      if (shape.touch) {
        // Recover fixture preference availability after the detailed failure leg.
        if (detailed) {
          await control({ preferencesAvailable: true, preferences: { available: true } });
          await hideKeyBar().catch(() => undefined);
        }
        await closeComposer();
        await showKeyBar();
        const standard = await uiState();
        assert(standard.bank === null && standard.keyLabels.join(" ") === "Esc ⇥ Ctrl ← ↓ ↑ →", `${shape.name}: default row drifted: ${JSON.stringify(standard)}`);
        assert(standard.bankToggleText === "Keys" && standard.keyRects.every((rect) => rect.width >= 44 && rect.height >= 44), `${shape.name}: stable Keys strip targets drifted`);
        const directBefore = allInputs(await snapshot());
        await page.getByRole("button", { name: "Terminal Keys", exact: true }).tap();
        assert(await page.locator('.persea-terminal-keys[data-view="Favorites"]').isVisible(), `${shape.name}: fresh Keys did not open Favorites`);
        await page.getByRole("button", { name: "Terminal Keys", exact: true }).tap();
        assert(allInputs(await snapshot()).length === directBefore.length, `${shape.name}: Keys disclosure sent input`);
        await openComposer();
        const draft = page.locator(".attachment-page__composer-textarea");
        await draft.fill("draft remains draft"); await draft.evaluate((node) => node.setSelectionRange(2, 9));
        const draftBefore = await uiState();
        await page.getByRole("button", { name: "Terminal Keys", exact: true }).tap();
        await page.getByRole("button", { name: "Terminal Keys", exact: true }).tap();
        const draftAfter = await uiState();
        assert(await draft.evaluate((node) => document.activeElement === node) && await draft.inputValue() === "draft remains draft" && JSON.stringify(draftBefore.selection) === JSON.stringify(draftAfter.selection) && allInputs(await snapshot()).length === directBefore.length,
          `${shape.name}: Keys disclosure changed composer focus/draft/selection or sent input`);
        await closeComposer();
        caseEvidence.directKeybox = { standard, draftBefore, draftAfter };
        if (shape.name === "phone-390" && (!MUTANT || MUTANT === "native-key-echo-lost")) {
          // Load the same native event fixture used by the router/controller
          // tests, then verify bytes accepted by the actual page's transport.
          const fixtureModule = { exports: {} };
          const compiled = require(path.join(UI, "node_modules/esbuild")).transformSync(
            fs.readFileSync(path.join(__dirname, "fixtures/ios_firefox_dictation_restart_20260905.ts"), "utf8"),
            { loader: "ts", format: "cjs" },
          );
          require("node:vm").runInNewContext(compiled.code, { module: fixtureModule });
          const nativeEvents = fixtureModule.exports.nativeDictationRestart;
          await page.locator(".xterm-helper-textarea").focus();
          const resetBefore = allInputs(await snapshot()).length;
          await page.keyboard.press("Control+c");
          const resetAfter = await waitSnapshot((value) => allInputs(value).length > resetBefore && allInputs(value).at(-1) === "\x03");
          const nativeBefore = allInputs(resetAfter).length;
          await page.evaluate((entries) => {
            const textarea = document.querySelector(".xterm-helper-textarea");
            let held = false;
            const physicalKey = (type) => {
              const event = new KeyboardEvent(type, { key: " ", code: "Space", bubbles: true, cancelable: true, composed: true });
              for (const [property, value] of Object.entries({ keyCode: 32, which: 32, charCode: type === "keypress" ? 32 : 0 })) {
                Object.defineProperty(event, property, { get: () => value });
              }
              textarea.dispatchEvent(event);
            };
            for (const entry of entries) {
              if (entry.kind === "input") textarea.value = entry.field;
              // A sole owned sentinel and an empty pre-edit field represent
              // the same draft; cleanup now follows the browser's applied edit.
              else if (textarea.value !== entry.field && !(textarea.value === "\u200b" && entry.field === "")) throw new Error(`native input field diverged before ${entry.t}`);
              textarea.setSelectionRange(...entry.selection);
              if (entry.kind === "keydown") {
                physicalKey("keydown"); physicalKey("keypress"); held = true;
              } else {
                textarea.dispatchEvent(new InputEvent(entry.kind, {
                  inputType: entry.inputType ?? "", data: entry.data ?? null, isComposing: entry.composing,
                  bubbles: true, cancelable: true, composed: true,
                }));
                if (entry.kind === "input" && held) { physicalKey("keyup"); held = false; }
              }
            }
          }, nativeEvents);
          // An explicit key after the replay is a transport ordering barrier.
          await page.keyboard.press("Control+c");
          const nativeAfter = await waitSnapshot((value) => allInputs(value).length > nativeBefore && allInputs(value).at(-1) === "\x03");
          const nativeBytes = allInputs(nativeAfter).slice(nativeBefore, -1);
          const held = [];
          for (const point of nativeBytes.join("")) {
            if (point === "\x7f") held.pop();
            else held.push(point);
          }
          assert(held.join("") === "test 123 best again 123" && !/[\r\n]/.test(nativeBytes.join("")),
            `${shape.name}: native dictation restart duplicated text or sent Enter: ${JSON.stringify(nativeBytes)}`);
          caseEvidence.nativeDictationRestart = { events: nativeEvents.length, bytes: nativeBytes, text: held.join("") };
        }
        if (detailed && (!MUTANT || MUTANT === "keyboard-activation-generation-fence")) {
          // Hold Space on F1, then use a real touch to transfer disclosure
          // ownership before Space releases its native detail=0 click. The
          // click is trusted, but its initiating generation is stale and must
          // emit no terminal INPUT.
          await enterFunctionBank();
          const heldF1 = page.getByRole("button", { name: "F1", exact: true });
          await heldF1.focus();
          await page.evaluate(() => {
            const button = document.querySelector('.persea-terminal-keys button[aria-label="F1"]');
            if (!(button instanceof HTMLButtonElement)) throw new Error("keyboard fence F1 is missing");
            window.__terminal_controlsKeyboardFenceEvents = [];
            for (const type of ["keydown", "keyup", "click", "blur"]) {
              button.addEventListener(type, (event) => window.__terminal_controlsKeyboardFenceEvents.push({
                type, trusted: event.isTrusted, key: event.key || null, detail: event.detail ?? null,
              }));
            }
          });
          const keyboardFenceBefore = allInputs(await snapshot());
          await page.keyboard.down("Space");
          const disclosureBox = await page.locator(".persea-unified-quick-actions").boundingBox();
          assert(disclosureBox, `${shape.name}: keyboard/touch disclosure target is missing`);
          await page.touchscreen.tap(disclosureBox.x + disclosureBox.width / 2, disclosureBox.y + disclosureBox.height / 2);
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await page.keyboard.up("Space");
          await delay(80);
          const keyboardFenceAfter = allInputs(await snapshot());
          const keyboardFenceDelta = keyboardFenceAfter.slice(keyboardFenceBefore.length);
          const keyboardFenceEvents = await page.evaluate(() => window.__terminal_controlsKeyboardFenceEvents);
          assert(keyboardFenceDelta.length === 0
            && keyboardFenceEvents.some((event) => event.type === "keydown" && event.trusted && event.key === " ")
            && keyboardFenceEvents.filter((event) => event.type === "click").every((event) => event.trusted && event.detail === 0),
          `${shape.name}: keyboard activation crossed disclosure generation: ${JSON.stringify({ keyboardFenceDelta, keyboardFenceEvents })}`);
          await page.locator(".persea-unified-quick-actions").click();
          caseEvidence.keyboardDisclosureFence = { delta: keyboardFenceDelta, events: keyboardFenceEvents };
        }
        if (detailed && !MUTANT) {
          // A trusted detail-zero click with no keydown observed by the
          // component is the accessibility branch, not a stale keyboard
          // compatibility click. Stop propagation without preventing the
          // browser's native activation so both engines exercise that branch.
          await enterFunctionBank();
          const accessibilityF2 = page.getByRole("button", { name: "F2", exact: true });
          await accessibilityF2.focus();
          await page.evaluate(() => {
            const button = document.querySelector('.persea-terminal-keys button[aria-label="F2"]');
            if (!(button instanceof HTMLButtonElement)) throw new Error("accessibility-path F2 is missing");
            window.__terminal_controlsAccessibilityEvents = [];
            const record = (event) => window.__terminal_controlsAccessibilityEvents.push({
              phase: "button", type: event.type, trusted: event.isTrusted,
              key: event.key || null, detail: event.detail ?? null,
            });
            const blockKeydown = (event) => {
              if (event.target !== button || event.key !== "Enter") return;
              window.__terminal_controlsAccessibilityEvents.push({
                phase: "withheld-before-control", type: event.type,
                trusted: event.isTrusted, key: event.key, detail: event.detail ?? null,
              });
              document.removeEventListener("keydown", blockKeydown, true);
              event.stopImmediatePropagation();
            };
            document.addEventListener("keydown", blockKeydown, true);
            for (const type of ["keydown", "click"]) button.addEventListener(type, record);
            window.__terminal_controlsAccessibilityCleanup = () => {
              document.removeEventListener("keydown", blockKeydown, true);
              for (const type of ["keydown", "click"]) button.removeEventListener(type, record);
            };
          });
          const accessibilityBefore = allInputs(await snapshot());
          await page.keyboard.press("Enter");
          await delay(120);
          const accessibilityAfter = allInputs(await snapshot());
          const accessibilityDelta = accessibilityAfter.slice(accessibilityBefore.length);
          const accessibilityEvents = await page.evaluate(() => {
            window.__terminal_controlsAccessibilityCleanup?.();
            delete window.__terminal_controlsAccessibilityCleanup;
            return window.__terminal_controlsAccessibilityEvents;
          });
          assert(accessibilityDelta.length === 1 && accessibilityDelta[0] === F_BYTES[1]
            && accessibilityEvents.filter((event) => event.phase === "button" && event.type === "click" && event.trusted && event.detail === 0).length === 1
            && accessibilityEvents.some((event) => event.phase === "withheld-before-control" && event.type === "keydown" && event.trusted && event.key === "Enter")
            && !accessibilityEvents.some((event) => event.phase === "button" && event.type === "keydown"),
          `${shape.name}: trusted no-keydown accessibility activation was rejected or duplicated: ${JSON.stringify({ accessibilityDelta, accessibilityEvents })}`);
          caseEvidence.accessibilityNoKeydown = { delta: accessibilityDelta, events: accessibilityEvents };
          await page.getByRole("button", { name: "Terminal Keys" }).click();
        }
        if (detailed && (!MUTANT || MUTANT === "keyboard-canceled-tombstone" || MUTANT === "keyboard-canceled-release-retirement")) {
          // Mixed modality on one disclosure: the real touch pointerup opens
          // the sheet once, then the held Space release must consume any
          // trusted detail-zero compatibility click without toggling again.
          const sameControl = page.locator(".persea-unified-quick-actions");
          if (!(await uiState()).sheetHidden) await sameControl.click();
          await sameControl.focus();
          await page.evaluate(() => {
            const button = document.querySelector(".persea-unified-quick-actions");
            if (!(button instanceof HTMLButtonElement)) throw new Error("same-control disclosure is missing");
            window.__terminal_controlsSameControlAudit = { events: [], expanded: [] };
            const record = (event) => window.__terminal_controlsSameControlAudit.events.push({
              type: event.type, trusted: event.isTrusted, key: event.key || null,
              detail: event.detail ?? null, pointerType: event.pointerType || null,
            });
            for (const type of ["keydown", "pointerdown", "pointerup", "keyup", "click"]) button.addEventListener(type, record);
            const observer = new MutationObserver((records) => {
              for (const entry of records) window.__terminal_controlsSameControlAudit.expanded.push({
                old: entry.oldValue, value: button.getAttribute("aria-expanded"),
              });
            });
            observer.observe(button, { attributes: true, attributeFilter: ["aria-expanded"], attributeOldValue: true });
            window.__terminal_controlsSameControlReset = () => {
              window.__terminal_controlsSameControlAudit.events.length = 0;
              window.__terminal_controlsSameControlAudit.expanded.length = 0;
            };
            window.__terminal_controlsSameControlCleanup = () => {
              observer.disconnect();
              for (const type of ["keydown", "pointerdown", "pointerup", "keyup", "click"]) button.removeEventListener(type, record);
            };
          });
          const sameControlBefore = allInputs(await snapshot());
          await page.keyboard.down("Space");
          const sameControlBox = await sameControl.boundingBox();
          assert(sameControlBox, `${shape.name}: same-control disclosure target is missing`);
          await page.touchscreen.tap(sameControlBox.x + sameControlBox.width / 2, sameControlBox.y + sameControlBox.height / 2);
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await delay(40);
          const sameControlAfterPointer = await uiState();
          const sameControlPointerAudit = await page.evaluate(() => structuredClone(window.__terminal_controlsSameControlAudit));
          await page.keyboard.up("Space");
          await delay(120);
          const sameControlAfterRelease = await uiState();
          const sameControlDelta = allInputs(await snapshot()).slice(sameControlBefore.length);
          const sameControlAudit = await page.evaluate(() => structuredClone(window.__terminal_controlsSameControlAudit));
          const compatibilityClicks = sameControlAudit.events.filter((event) => event.type === "click" && event.trusted && event.detail === 0).length;
          assert(sameControlAfterPointer.sheetHidden === false && sameControlAfterRelease.sheetHidden === false
            && sameControlPointerAudit.expanded.length === 1 && sameControlPointerAudit.expanded[0].value === "true"
            && sameControlAudit.expanded.length === 1 && sameControlDelta.length === 0
            && compatibilityClicks === 0
            && sameControlAudit.events.some((event) => event.type === "keydown" && event.trusted && event.key === " ")
            && sameControlAudit.events.some((event) => event.type === "keyup" && event.trusted && event.key === " ")
            && sameControlAudit.events.some((event) => event.type === "pointerup" && event.trusted && event.pointerType === "touch")
            && sameControlAudit.events.filter((event) => event.type === "click").every((event) => event.trusted && event.detail === 0),
          `${shape.name}: canceled keyboard activation was reclassified as no-keydown accessibility activation: ${JSON.stringify({ sameControlAfterPointer, sameControlAfterRelease, sameControlDelta, sameControlAudit })}`);

          // WebKit naturally emits no compatibility click for the mixed
          // sequence above; Chromium's stale Space default is canceled at its
          // source. The release must nevertheless retire before the next
          // independent trusted no-keydown accessibility activation, without
          // a blur or focus change. Both engines must accept this one.
          await page.evaluate(() => {
            const button = document.querySelector(".persea-unified-quick-actions");
            if (!(button instanceof HTMLButtonElement)) throw new Error("post-canceled accessibility disclosure is missing");
            window.__terminal_controlsSameControlReset();
            const blockKeydown = (event) => {
              if (event.target !== button || event.key !== "Enter") return;
              document.removeEventListener("keydown", blockKeydown, true);
              event.stopImmediatePropagation();
            };
            document.addEventListener("keydown", blockKeydown, true);
            window.__terminal_controlsPostCanceledATCleanup = () => document.removeEventListener("keydown", blockKeydown, true);
          });
          const postCanceledBefore = allInputs(await snapshot());
          await page.keyboard.press("Enter");
          // Read the state after the trusted sequence instead of waiting for
          // the expected close: the retirement mutant must die at the named
          // semantic assertion, not as a locator timeout.
          await delay(80);
          const postCanceledState = await uiState();
          const postCanceledDelta = allInputs(await snapshot()).slice(postCanceledBefore.length);
          const postCanceledAudit = await page.evaluate(() => {
            window.__terminal_controlsPostCanceledATCleanup?.();
            delete window.__terminal_controlsPostCanceledATCleanup;
            return structuredClone(window.__terminal_controlsSameControlAudit);
          });
          assert(postCanceledState.sheetHidden === true && postCanceledDelta.length === 0
            && postCanceledAudit.expanded.length === 1 && postCanceledAudit.expanded[0].value === "false"
            && postCanceledAudit.events.filter((event) => event.type === "click" && event.trusted && event.detail === 0).length === 1
            && !postCanceledAudit.events.some((event) => event.type === "keydown"),
          `${shape.name}: released canceled tombstone rejected the next trusted no-keydown accessibility activation: ${JSON.stringify({ postCanceledState, postCanceledDelta, postCanceledAudit })}`);

          // Fresh pointer and ordinary keyboard activation each toggle the
          // disclosure exactly once; neither may inherit the retired tombstone.
          await page.locator(".xterm-helper-textarea").focus();
          await page.evaluate(() => window.__terminal_controlsSameControlReset());
          await sameControl.click();
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await delay(40);
          const pointerAudit = await page.evaluate(() => structuredClone(window.__terminal_controlsSameControlAudit));
          assert(pointerAudit.expanded.length === 1 && pointerAudit.expanded[0].value === "true"
            && pointerAudit.events.filter((event) => event.type === "pointerup" && event.trusted).length === 1,
          `${shape.name}: ordinary pointer activation did not toggle exactly once: ${JSON.stringify(pointerAudit)}`);

          await page.evaluate(() => window.__terminal_controlsSameControlReset());
          await sameControl.focus();
          const keyboardBefore = allInputs(await snapshot());
          await page.keyboard.press("Enter");
          await page.locator(".persea-unified-sheet").waitFor({ state: "hidden" });
          await delay(40);
          const keyboardDelta = allInputs(await snapshot()).slice(keyboardBefore.length);
          const keyboardAudit = await page.evaluate(() => structuredClone(window.__terminal_controlsSameControlAudit));
          assert(keyboardAudit.expanded.length === 1 && keyboardAudit.expanded[0].value === "false" && keyboardDelta.length === 0
            && keyboardAudit.events.filter((event) => event.type === "click" && event.trusted && event.detail === 0).length === 1,
          `${shape.name}: ordinary keyboard activation did not toggle exactly once: ${JSON.stringify({ keyboardDelta, keyboardAudit })}`);
          await page.locator(".xterm-helper-textarea").focus();
          await page.evaluate(() => {
            window.__terminal_controlsSameControlCleanup?.();
            delete window.__terminal_controlsSameControlCleanup;
            delete window.__terminal_controlsSameControlReset;
          });
          caseEvidence.keyboardTombstone = {
            sameControl: { afterPointer: sameControlAfterPointer.sheetHidden, afterRelease: sameControlAfterRelease.sheetHidden, delta: sameControlDelta, audit: sameControlAudit },
            postCanceledAccessibility: { state: postCanceledState.sheetHidden, delta: postCanceledDelta, audit: postCanceledAudit },
            pointer: pointerAudit, keyboard: { delta: keyboardDelta, audit: keyboardAudit },
          };
        }
        if (detailed && (!MUTANT || MUTANT === "toolbar-composer-generation-fence")) {
          // The always-present toolbar Composer is a persistent disclosure,
          // not a raw-click exception. Its Space authority cannot survive a
          // Quick-actions disclosure transition.
          await closeComposer();
          if (!(await uiState()).sheetHidden) await page.locator(".persea-unified-quick-actions").click();
          const toolbarComposer = page.locator(".persea-unified-composer-toggle");
          await toolbarComposer.focus();
          await page.evaluate(() => {
            const button = document.querySelector(".persea-unified-composer-toggle");
            if (!(button instanceof HTMLButtonElement)) throw new Error("toolbar composer generation control is missing");
            window.__terminal_controlsToolbarComposerFence = [];
            for (const type of ["keydown", "keyup", "click", "pointerdown", "pointerup"]) {
              button.addEventListener(type, (event) => window.__terminal_controlsToolbarComposerFence.push({
                type, trusted: event.isTrusted, key: event.key || null, detail: event.detail ?? null,
              }));
            }
          });
          const toolbarComposerBefore = allInputs(await snapshot());
          await page.keyboard.down("Space");
          const toolbarDisclosureBox = await page.locator(".persea-unified-quick-actions").boundingBox();
          assert(toolbarDisclosureBox, `${shape.name}: toolbar Composer competing disclosure is missing`);
          await page.touchscreen.tap(toolbarDisclosureBox.x + toolbarDisclosureBox.width / 2, toolbarDisclosureBox.y + toolbarDisclosureBox.height / 2);
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await page.keyboard.up("Space");
          await delay(100);
          const toolbarComposerState = await uiState();
          const toolbarComposerDelta = allInputs(await snapshot()).slice(toolbarComposerBefore.length);
          const toolbarComposerEvents = await page.evaluate(() => window.__terminal_controlsToolbarComposerFence);
          assert(toolbarComposerState.composerOpen !== "true" && toolbarComposerState.sheetHidden === false && toolbarComposerDelta.length === 0
            && toolbarComposerEvents.some((event) => event.type === "keydown" && event.trusted && event.key === " ")
            && toolbarComposerEvents.some((event) => event.type === "keyup" && event.trusted && event.key === " "),
          `${shape.name}: toolbar composer keyboard activation crossed disclosure generation: ${JSON.stringify({ toolbarComposerState, toolbarComposerDelta, toolbarComposerEvents })}`);
          await page.locator(".persea-unified-quick-actions").click();
          caseEvidence.toolbarComposerGenerationFence = { state: toolbarComposerState.composerOpen, delta: toolbarComposerDelta, events: toolbarComposerEvents };
        }
        if (detailed && (!MUTANT || MUTANT === "composer-control-generation-fence")) {
          // Composer owns persistent terminal-mutating buttons. Insert uses
          // the page's real authority rather than a private generation-zero
          // universe while the panel remains visible across a sheet change.
          await openComposer();
          const composerText = `terminal_controls-c7-stale-composer-${ENGINE}`;
          const composerTextarea = page.locator(".attachment-page__composer-textarea");
          await composerTextarea.fill(composerText);
          const composerInsert = page.getByRole("button", { name: "Insert into terminal without running it", exact: true });
          await composerInsert.focus();
          const composerFenceBefore = allInputs(await snapshot());
          await page.keyboard.down("Space");
          const composerDisclosureBox = await page.locator(".persea-unified-quick-actions").boundingBox();
          assert(composerDisclosureBox, `${shape.name}: Composer Insert competing disclosure is missing`);
          await page.touchscreen.tap(composerDisclosureBox.x + composerDisclosureBox.width / 2, composerDisclosureBox.y + composerDisclosureBox.height / 2);
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await page.keyboard.up("Space");
          await delay(120);
          const composerFenceDelta = allInputs(await snapshot()).slice(composerFenceBefore.length);
          const composerFenceValue = await composerTextarea.inputValue();
          assert(composerFenceDelta.length === 0 && composerFenceValue === composerText,
          `${shape.name}: Composer Insert keyboard activation crossed disclosure generation: ${JSON.stringify({ composerFenceDelta, composerFenceValue })}`);
          await page.locator(".persea-unified-quick-actions").click();
          await composerTextarea.fill("");
          await closeComposer();
          caseEvidence.composerControlGenerationFence = { delta: composerFenceDelta, valueRetained: composerFenceValue === composerText };
        }
        if (detailed && (!MUTANT || MUTANT === "keyboard-unrelated-key-provenance")) {
          // An unrelated key does not end the physical Space press. Retaining
          // its original capture ensures the eventual click remains stale
          // after a disclosure cut instead of being mistaken for AT input.
          if (!(await uiState()).sheetHidden) await page.locator(".persea-unified-quick-actions").click();
          if ((await uiState()).bank !== "function") await enterFunctionBank();
          const unrelatedF1 = page.getByRole("button", { name: "F1", exact: true });
          await unrelatedF1.focus();
          await page.evaluate(() => {
            const button = document.querySelector('.persea-terminal-keys button[aria-label="F1"]');
            if (!(button instanceof HTMLButtonElement)) throw new Error("unrelated-key F1 is missing");
            window.__terminal_controlsUnrelatedKeyFence = [];
            for (const type of ["keydown", "keyup", "click"]) button.addEventListener(type, (event) => window.__terminal_controlsUnrelatedKeyFence.push({
              type, trusted: event.isTrusted, key: event.key || null, detail: event.detail ?? null,
            }));
          });
          const unrelatedBefore = allInputs(await snapshot());
          await page.keyboard.down("Space");
          await page.keyboard.press("a");
          const unrelatedDisclosureBox = await page.locator(".persea-unified-quick-actions").boundingBox();
          assert(unrelatedDisclosureBox, `${shape.name}: unrelated-key competing disclosure is missing`);
          await page.touchscreen.tap(unrelatedDisclosureBox.x + unrelatedDisclosureBox.width / 2, unrelatedDisclosureBox.y + unrelatedDisclosureBox.height / 2);
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await page.keyboard.up("Space");
          await delay(100);
          const unrelatedDelta = allInputs(await snapshot()).slice(unrelatedBefore.length);
          const unrelatedEvents = await page.evaluate(() => window.__terminal_controlsUnrelatedKeyFence);
          assert(unrelatedDelta.length === 0
            && unrelatedEvents.some((event) => event.type === "keydown" && event.key === "a" && event.trusted)
            && unrelatedEvents.some((event) => event.type === "keyup" && event.key === "a" && event.trusted),
          `${shape.name}: unrelated key erased held keyboard activation provenance: ${JSON.stringify({ unrelatedDelta, unrelatedEvents })}`);
          await page.locator(".persea-unified-quick-actions").click();
          await enterFunctionBank();
          const unrelatedTapBefore = allInputs(await snapshot());
          await unrelatedF1.click();
          const unrelatedTapAfter = await waitSnapshot((value) => allInputs(value).length >= unrelatedTapBefore.length + 1);
          const unrelatedTapDelta = allInputs(unrelatedTapAfter).slice(unrelatedTapBefore.length);
          assert(unrelatedTapDelta.length === 1 && unrelatedTapDelta[0] === F_BYTES[0],
            `${shape.name}: unrelated-key fence changed fresh F1 behavior: ${JSON.stringify(unrelatedTapDelta)}`);
          caseEvidence.unrelatedKeyFence = { delta: unrelatedDelta, tapDelta: unrelatedTapDelta, events: unrelatedEvents };
        }
        if (detailed && (!MUTANT || MUTANT === "tap-pointer-identity" || MUTANT === "sheet-gesture-generation-fence")) {
          // Each simultaneous contact keeps its own acquisition generation.
          // A stale A-up cannot spend B's newer authority; B's complete tap
          // remains the one permitted F1 action.
          if (!(await uiState()).sheetHidden) await page.locator(".persea-unified-quick-actions").click();
          if ((await uiState()).bank === "function") await page.getByRole("button", { name: "Terminal Keys", exact: true }).click();
          const pointerF1 = page.locator('.persea-unified-keybar-row button[title="Esc"]');
          const pointerF1Box = await pointerF1.boundingBox();
          const pointerQuickBox = await page.locator(".persea-unified-quick-actions").boundingBox();
          assert(pointerF1Box && pointerQuickBox, `${shape.name}: pointer-identity targets are missing`);
          await page.evaluate(() => {
            const key = document.querySelector('.persea-unified-keybar-row button[title="Esc"]');
            const quick = document.querySelector(".persea-unified-quick-actions");
            if (!(key instanceof HTMLButtonElement) || !(quick instanceof HTMLButtonElement)) throw new Error("pointer-identity controls are missing");
            window.__terminal_controlsPointerIdentity = [];
            for (const [name, button] of [["f1", key], ["quick", quick]]) {
              for (const type of ["pointerdown", "pointerup", "pointercancel", "pointerleave"]) button.addEventListener(type, (event) => {
                window.__terminal_controlsPointerIdentity.push({ name, type, trusted: event.isTrusted, pointerId: event.pointerId, pointerType: event.pointerType });
              });
            }
          });
          const pointerBefore = allInputs(await snapshot());
          const pointerOrder = await beginTrustedPointerIdentityOrder(
            page,
            { x: pointerF1Box.x + pointerF1Box.width / 2 - 3, y: pointerF1Box.y + pointerF1Box.height / 2 },
            { x: pointerQuickBox.x + pointerQuickBox.width / 2, y: pointerQuickBox.y + pointerQuickBox.height / 2 },
            { x: pointerF1Box.x + pointerF1Box.width / 2 + 3, y: pointerF1Box.y + pointerF1Box.height / 2 },
          );
          const pointerAfterStale = allInputs(await snapshot()).slice(pointerBefore.length);
          await pointerOrder.finish();
          const pointerAfterFreshSnapshot = await waitSnapshot((value) => allInputs(value).length >= pointerBefore.length + 1);
          const pointerFinalDelta = allInputs(pointerAfterFreshSnapshot).slice(pointerBefore.length);
          const pointerEvents = await page.evaluate(() => window.__terminal_controlsPointerIdentity);
          const f1DownIds = pointerEvents.filter((event) => event.name === "f1" && event.type === "pointerdown").map((event) => event.pointerId);
          assert(pointerAfterStale.length === 0 && pointerFinalDelta.length === 1 && pointerFinalDelta[0] === "\x1b"
            && f1DownIds.length === 2 && f1DownIds[0] !== f1DownIds[1]
            && pointerEvents.every((event) => event.trusted && event.pointerType === "touch"),
          `${shape.name}: overlapping pointer release borrowed another contact's authority: ${JSON.stringify({ pointerAfterStale, pointerFinalDelta, pointerEvents })}`);
          await page.locator(".persea-unified-quick-actions").click();
          caseEvidence.pointerIdentityFence = { staleDelta: pointerAfterStale, finalDelta: pointerFinalDelta, events: pointerEvents };
        }
        if (detailed && (!MUTANT || MUTANT === "session-generation-provider-omission" || MUTANT === "session-sheet-generation-provider-omission")) {
          // Both renderings of the persistent session switcher receive the
          // page's authority. The test-only display override keeps the prior
          // row rendered when its owner closes, so focus/visibility cannot
          // accidentally mask a missing generation fence.
          await control({ switchSessions: true, holdModeGrant: false });
          if (!(await uiState()).sheetHidden) await page.locator(".persea-unified-quick-actions").click();
          await page.setViewportSize({ width: 1280, height: 800 });
          await delay(80);
          await page.evaluate((nonce) => {
            const style = document.createElement("style");
            style.id = "terminal_controls-session-host-probe";
            style.nonce = nonce;
            style.textContent = ".persea-unified-identity { display: flex !important; }";
            document.head.append(style);
          }, STYLE_NONCE);
          const tag = page.locator(".persea-unified-tag");
          if (await page.locator(".persea-unified-identity__details").getAttribute("hidden") !== null) await tag.click();
          const identityRoot = page.locator(".persea-unified-identity__sessions");
          await identityRoot.getByRole("button", { name: "Refresh sessions", exact: true }).click();
          await delay(300);
          const tagBeta = identityRoot.locator('.persea-session-switcher__row[aria-label="Switch to beta"]');
          const tagRows = await identityRoot.locator(".persea-session-switcher__row").evaluateAll((buttons) => buttons.map((button) => ({ label: button.getAttribute("aria-label"), disabled: button.disabled, hidden: button.hidden })));
          assert(tagRows.some((row) => row.label === "Switch to beta"), `${shape.name}: identity SessionSwitcher did not render beta: ${JSON.stringify(tagRows)}`);
          await page.evaluate((nonce) => {
            const style = document.createElement("style");
            style.id = "terminal_controls-session-tag-focus-probe";
            style.nonce = nonce;
            style.textContent = ".persea-unified-identity__details[hidden] { display: block !important; }";
            document.head.append(style);
          }, STYLE_NONCE);
          await tagBeta.focus();
          await page.keyboard.down("Space");
          const tagCompetingBox = await page.locator(".persea-unified-quick-actions").boundingBox();
          assert(tagCompetingBox, `${shape.name}: tag switcher competing disclosure is missing`);
          await page.touchscreen.tap(tagCompetingBox.x + tagCompetingBox.width / 2, tagCompetingBox.y + tagCompetingBox.height / 2);
          await page.waitForFunction(() => document.querySelector(".persea-unified-sheet")?.hidden === false);
          await page.keyboard.up("Space");
          await delay(120);
          const tagSwitcherName = await page.locator(".persea-unified-tag__name").textContent();
          assert(tagSwitcherName === "alpha",
            `${shape.name}: identity SessionSwitcher keyboard activation crossed disclosure generation: ${JSON.stringify({ tagSwitcherName })}`);
          await page.evaluate(() => document.querySelector("#terminal_controls-session-tag-focus-probe")?.remove());
          await page.locator(".persea-unified-quick-actions").click();

          await page.locator(".persea-unified-quick-actions").click();
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await page.getByRole("button", { name: "Choose another session", exact: true }).click();
          const sheetRoot = page.locator(".persea-unified-sheet .persea-session-switcher");
          const sheetBeta = sheetRoot.locator('.persea-session-switcher__row[aria-label="Switch to beta"]');
          await sheetBeta.waitFor({ state: "visible" });
          await page.evaluate((nonce) => {
            const style = document.createElement("style");
            style.id = "terminal_controls-session-sheet-focus-probe";
            style.nonce = nonce;
            style.textContent = ".persea-unified-sheet[hidden] { display: block !important; }";
            document.head.append(style);
          }, STYLE_NONCE);
          await sheetBeta.focus();
          await page.keyboard.down("Space");
          const sheetCompetingBox = await tag.boundingBox();
          assert(sheetCompetingBox, `${shape.name}: sheet switcher competing disclosure is missing`);
          await page.touchscreen.tap(sheetCompetingBox.x + sheetCompetingBox.width / 2, sheetCompetingBox.y + sheetCompetingBox.height / 2);
          await page.waitForFunction(() => document.querySelector(".persea-unified-identity__details")?.hidden === false);
          await page.keyboard.up("Space");
          await delay(120);
          const sheetSwitcherName = await page.locator(".persea-unified-tag__name").textContent();
          assert(sheetSwitcherName === "alpha",
            `${shape.name}: sheet SessionSwitcher keyboard activation crossed disclosure generation: ${JSON.stringify({ sheetSwitcherName })}`);
          await page.evaluate(() => document.querySelector("#terminal_controls-session-sheet-focus-probe")?.remove());
          if (await page.locator(".persea-unified-identity__details").getAttribute("hidden") === null) await tag.click();
          await page.evaluate(() => document.querySelector("#terminal_controls-session-host-probe")?.remove());
          await page.setViewportSize({ width: shape.width, height: shape.height });
          caseEvidence.sessionSwitcherGenerationFence = { tag: tagSwitcherName, sheet: sheetSwitcherName };
        }
        if (detailed && (!MUTANT || MUTANT === "standard-key-generation-fence" || MUTANT === "keyboard-activation-generation-fence")) {
          // Standard-row controls share the same generation authority. A held
          // Space on Ctrl cannot re-arm the latch after Quick actions changes
          // disclosure ownership, and the next ordinary terminal key is plain.
          if ((await uiState()).keyBarHidden) await showKeyBar();
          if ((await uiState()).bank === "function") await page.getByRole("button", { name: "Terminal Keys" }).click();
          if (!(await uiState()).sheetHidden) await page.locator(".persea-unified-quick-actions").click();
          const standardCtrl = page.locator(".persea-unified-keybar-key--latch");
          await standardCtrl.focus();
          await page.evaluate(() => {
            const button = document.querySelector(".persea-unified-keybar-key--latch");
            if (!(button instanceof HTMLButtonElement)) throw new Error("standard Ctrl generation control is missing");
            window.__terminal_controlsStandardCtrlFenceEvents = [];
            const record = (event) => window.__terminal_controlsStandardCtrlFenceEvents.push({
              type: event.type, trusted: event.isTrusted, key: event.key || null, detail: event.detail ?? null,
            });
            for (const type of ["keydown", "keyup", "click", "blur"]) button.addEventListener(type, record);
          });
          const standardCtrlBefore = allInputs(await snapshot());
          await page.keyboard.down("Space");
          const standardDisclosureBox = await page.locator(".persea-unified-quick-actions").boundingBox();
          assert(standardDisclosureBox, `${shape.name}: standard Ctrl competing disclosure is missing`);
          await page.touchscreen.tap(standardDisclosureBox.x + standardDisclosureBox.width / 2, standardDisclosureBox.y + standardDisclosureBox.height / 2);
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await page.keyboard.up("Space");
          await delay(80);
          const standardCtrlAfterRelease = await uiState();
          const standardCtrlReleaseDelta = allInputs(await snapshot()).slice(standardCtrlBefore.length);
          const standardCtrlEvents = await page.evaluate(() => window.__terminal_controlsStandardCtrlFenceEvents);
          const standardLetterBefore = allInputs(await snapshot());
          await page.locator(".xterm-helper-textarea").focus();
          await page.keyboard.type("s");
          const standardLetterAfter = await waitSnapshot((value) => allInputs(value).length >= standardLetterBefore.length + 1);
          const standardLetterDelta = allInputs(standardLetterAfter).slice(standardLetterBefore.length);
          assert(standardCtrlReleaseDelta.length === 0
            && standardCtrlAfterRelease.standardCtrlPressed === "false" && standardCtrlAfterRelease.ctrlSurfaces.length > 0 && standardCtrlAfterRelease.ctrlSurfaces.every(value => value === "false")
            && standardLetterDelta.length === 1 && standardLetterDelta[0] === "s"
            && standardCtrlEvents.some((event) => event.type === "keydown" && event.trusted && event.key === " ")
            && standardCtrlEvents.some((event) => event.type === "keyup" && event.trusted && event.key === " ")
            && standardCtrlEvents.filter((event) => event.type === "click").every((event) => event.trusted && event.detail === 0),
          `${shape.name}: standard Ctrl keyboard activation crossed disclosure generation: ${JSON.stringify({ standardCtrlAfterRelease, standardCtrlReleaseDelta, standardLetterDelta, standardCtrlEvents })}`);
          await page.locator(".persea-unified-quick-actions").click();
          caseEvidence.standardCtrlKeyboardFence = { releaseDelta: standardCtrlReleaseDelta, letterDelta: standardLetterDelta, events: standardCtrlEvents };
        }
        if (detailed && (!MUTANT || MUTANT === "paste-generation-fence")) {
          // Paste opens an inert Clipboard picker. Its keyboard gesture is
          // fenced even though a different control owns the intervening touch.
          const paste = page.locator(".persea-unified-toolbar-paste");
          assert(await paste.getAttribute("data-paste-state") === "paste", `${shape.name}: Paste slot is not in paste state for generation proof`);
          await paste.focus();
          await page.evaluate(() => {
            const button = document.querySelector(".persea-unified-toolbar-paste");
            const clipboard = navigator.clipboard;
            if (!(button instanceof HTMLButtonElement) || !clipboard) throw new Error("Paste generation fixture is unavailable");
            const ownReadText = Object.getOwnPropertyDescriptor(clipboard, "readText");
            window.__terminal_controlsPasteFence = { reads: 0, events: [] };
            Object.defineProperty(clipboard, "readText", {
              configurable: true,
              value: async () => { window.__terminal_controlsPasteFence.reads += 1; return "terminal_controls-stale-paste"; },
            });
            const record = (event) => window.__terminal_controlsPasteFence.events.push({
              type: event.type, trusted: event.isTrusted, key: event.key || null, detail: event.detail ?? null,
            });
            for (const type of ["keydown", "keyup", "click", "blur"]) button.addEventListener(type, record);
            window.__terminal_controlsPasteFenceCleanup = () => {
              for (const type of ["keydown", "keyup", "click", "blur"]) button.removeEventListener(type, record);
              if (ownReadText) Object.defineProperty(clipboard, "readText", ownReadText);
              else delete clipboard.readText;
            };
          });
          const pasteBefore = allInputs(await snapshot());
          await page.keyboard.down("Space");
          const pasteDisclosureBox = await page.locator(".persea-unified-quick-actions").boundingBox();
          assert(pasteDisclosureBox, `${shape.name}: Paste competing disclosure is missing`);
          await page.touchscreen.tap(pasteDisclosureBox.x + pasteDisclosureBox.width / 2, pasteDisclosureBox.y + pasteDisclosureBox.height / 2);
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await page.keyboard.up("Space");
          await delay(160);
          const pasteAfter = allInputs(await snapshot()).slice(pasteBefore.length);
          const pasteFence = await page.evaluate(() => {
            const value = structuredClone(window.__terminal_controlsPasteFence);
            window.__terminal_controlsPasteFenceCleanup?.();
            delete window.__terminal_controlsPasteFenceCleanup;
            return value;
          });
          assert(pasteFence.reads === 0 && pasteAfter.length === 0
            && await page.locator('.persea-clipboard').isHidden()
            && pasteFence.events.some((event) => event.type === "keydown" && event.trusted && event.key === " ")
            && pasteFence.events.some((event) => event.type === "keyup" && event.trusted && event.key === " ")
            && pasteFence.events.filter((event) => event.type === "click").every((event) => event.trusted && event.detail === 0),
          `${shape.name}: Paste keyboard activation crossed disclosure generation: ${JSON.stringify({ pasteAfter, pasteFence })}`);
          await page.locator(".persea-unified-quick-actions").click();
          caseEvidence.pasteKeyboardFence = { delta: pasteAfter, ...pasteFence };
        }
        if (detailed && (!MUTANT || MUTANT === "keyboard-enter-repeat-provenance")) {
          // Enter's first native click occurs on keydown and opens the sheet.
          // Repeated keydowns from that same held physical press retain the
          // original generation and cannot masquerade as captureless AT clicks.
          const repeatDisclosure = page.locator(".persea-unified-quick-actions");
          if (!(await uiState()).sheetHidden) await repeatDisclosure.click();
          await repeatDisclosure.focus();
          await page.evaluate(() => {
            const button = document.querySelector(".persea-unified-quick-actions");
            if (!(button instanceof HTMLButtonElement)) throw new Error("Enter repeat disclosure is missing");
            window.__terminal_controlsEnterRepeat = { events: [], expanded: [] };
            const record = (event) => window.__terminal_controlsEnterRepeat.events.push({
              type: event.type, trusted: event.isTrusted, key: event.key || null,
              repeat: event.repeat ?? null, detail: event.detail ?? null,
            });
            for (const type of ["keydown", "keyup", "click"]) button.addEventListener(type, record);
            const observer = new MutationObserver((records) => {
              for (const entry of records) window.__terminal_controlsEnterRepeat.expanded.push({ old: entry.oldValue, value: button.getAttribute("aria-expanded") });
            });
            observer.observe(button, { attributes: true, attributeFilter: ["aria-expanded"], attributeOldValue: true });
            window.__terminal_controlsEnterRepeatCleanup = () => {
              observer.disconnect();
              for (const type of ["keydown", "keyup", "click"]) button.removeEventListener(type, record);
            };
          });
          const repeatBefore = allInputs(await snapshot());
          await page.keyboard.down("Enter");
          await page.locator(".persea-unified-sheet").waitFor({ state: "visible" });
          await delay(40);
          await page.keyboard.down("Enter");
          await delay(80);
          const repeatState = await uiState();
          const repeatDelta = allInputs(await snapshot()).slice(repeatBefore.length);
          await page.keyboard.up("Enter");
          const repeatAudit = await page.evaluate(() => {
            const value = structuredClone(window.__terminal_controlsEnterRepeat);
            window.__terminal_controlsEnterRepeatCleanup?.();
            delete window.__terminal_controlsEnterRepeatCleanup;
            return value;
          });
          assert(repeatState.sheetHidden === false && repeatDelta.length === 0 && repeatAudit.expanded.length === 1
            && repeatAudit.events.some((event) => event.type === "keydown" && event.trusted && event.key === "Enter" && event.repeat === false)
            && repeatAudit.events.some((event) => event.type === "keydown" && event.trusted && event.key === "Enter" && event.repeat === true)
            && repeatAudit.events.filter((event) => event.type === "click" && event.trusted && event.detail === 0).length >= 2,
          `${shape.name}: held Enter repeat was reclassified as no-keydown accessibility activation: ${JSON.stringify({ repeatState, repeatDelta, repeatAudit })}`);
          await page.locator(".xterm-helper-textarea").focus();
          await repeatDisclosure.click();
          await page.locator(".persea-unified-sheet").waitFor({ state: "hidden" });
          caseEvidence.enterRepeatFence = { delta: repeatDelta, audit: repeatAudit };
        }
        await enterFunctionBank();
        const bank = await uiState();
        assert(bank.coarsePointer && bank.rowCount === 1 && bank.keyLabels.join(" ") === "F1 F2 F3 F4 F5 F6 F7 F8 F9 F10 F11 F12" && bank.keyRects.every((rect) => rect.width >= 44 && rect.height >= 44), `${shape.name}: function catalog targets or complete key set drifted`);
        // Scrolling, release-only activation and visible prompt geometry are
        // exercised causally at all mobile shapes by terminal-keys-browser.
        const activePaint = { panelOpen: true };
        const clickFunctionSet = async (label) => {
          const beforeInputs = allInputs(await snapshot());
          for (let index = 1; index <= 12; index += 1) await page.getByRole("button", { name: `F${index}`, exact: true }).click();
          const afterInputs = allInputs(await snapshot());
          const delta = afterInputs.slice(beforeInputs.length);
          assert(delta.length === 12 && delta.every((value, index) => value === F_BYTES[index]), `${shape.name}: ${label} F-key bytes/events drifted: ${JSON.stringify(delta)}`);
          assert(await page.locator(".persea-terminal-keys").isVisible(), `${shape.name}: ${label} F-key closed the catalog`);
          return delta;
        };
        if (detailed) {
          caseEvidence.functionModes = {};
          caseEvidence.functionActivePaint = activePaint;
          caseEvidence.functionModes.normal = await clickFunctionSet("normal-buffer normal-mode");
          await control({ writeLive: "\x1b[?1h" }); await delay(80);
          caseEvidence.functionModes.application = await clickFunctionSet("normal-buffer application-mode");
          await control({ writeLive: "\x1b[?1049h" }); await delay(80);
          caseEvidence.functionModes.alternateApplication = await clickFunctionSet("alternate-buffer application-mode");
          await control({ writeLive: "\x1b[?1l" }); await delay(80);
          caseEvidence.functionModes.alternateNormal = await clickFunctionSet("alternate-buffer normal-mode");

          let reconnectCtrlArmed;
          if (!MUTANT || MUTANT === "transport-key-authority-reset") {
            // Reconnect from the already-standard row: lifecycle authority
            // cannot depend on a bank transition having changed visible DOM.
            await page.getByRole("button", { name: "Terminal Keys" }).click();
            await page.locator(".persea-unified-keybar-key--latch").click();
            reconnectCtrlArmed = await uiState();
            assert(reconnectCtrlArmed.bank === null && reconnectCtrlArmed.standardCtrlPressed === "true" && reconnectCtrlArmed.ctrlSurfaces.length > 0 && reconnectCtrlArmed.ctrlSurfaces.every(value => value === "true"),
              `${shape.name}: standard-bank Ctrl did not arm for reconnect authority proof: ${JSON.stringify(reconnectCtrlArmed)}`);
          }
          await openComposer();
          await page.locator(".attachment-page__composer-typography-trigger").click();
          let lifecycleKeyboardBefore;
          if (!MUTANT) {
            const lifecycleDisclosure = page.locator(".persea-unified-quick-actions");
            await lifecycleDisclosure.focus();
            await page.evaluate(() => {
              const button = document.querySelector(".persea-unified-quick-actions");
              if (!(button instanceof HTMLButtonElement)) throw new Error("lifecycle disclosure is missing");
              window.__terminal_controlsLifecycleKeyboardEvents = [];
              const record = (event) => window.__terminal_controlsLifecycleKeyboardEvents.push({
                type: event.type, trusted: event.isTrusted, key: event.key || null, detail: event.detail ?? null,
              });
              for (const type of ["keydown", "keyup", "click", "blur"]) button.addEventListener(type, record);
              window.__terminal_controlsLifecycleKeyboardCleanup = () => {
                for (const type of ["keydown", "keyup", "click", "blur"]) button.removeEventListener(type, record);
              };
            });
            lifecycleKeyboardBefore = allInputs(await snapshot());
            await page.keyboard.down("Space");
          }
          const beforeReconnect = await snapshot();
          await control({ closeLive: "subscriber_lagged" });
          await waitSnapshot((value) => value.attachments.length > beforeReconnect.attachments.length && value.attachments.at(-1)?.live, 12_000);
          if (lifecycleKeyboardBefore) {
            await page.keyboard.up("Space");
            await delay(100);
            const lifecycleState = await uiState();
            const lifecycleDelta = allInputs(await snapshot()).slice(lifecycleKeyboardBefore.length);
            const lifecycleEvents = await page.evaluate(() => {
              window.__terminal_controlsLifecycleKeyboardCleanup?.();
              delete window.__terminal_controlsLifecycleKeyboardCleanup;
              return window.__terminal_controlsLifecycleKeyboardEvents;
            });
            assert(lifecycleState.sheetHidden === true && lifecycleDelta.length === 0
              && lifecycleEvents.some((event) => event.type === "keydown" && event.trusted && event.key === " ")
              && lifecycleEvents.filter((event) => event.type === "click").every((event) => event.trusted && event.detail === 0),
            `${shape.name}: keyboard activation crossed the transport lifecycle generation: ${JSON.stringify({ lifecycleState, lifecycleDelta, lifecycleEvents })}`);
            caseEvidence.keyboardTombstone.lifecycle = { state: lifecycleState.sheetHidden, delta: lifecycleDelta, events: lifecycleEvents };
          }
          const reset = await page.evaluate(() => {
            const row = document.querySelector(".persea-unified-keybar-row");
            const nodes = Array.from(row.children);
            return { labels: nodes.map((node) => node.textContent), sameNodes: Array.isArray(window.__terminal_controlsStandardNodes) && nodes.every((node, index) => node === window.__terminal_controlsStandardNodes[index]), bank: row.getAttribute("data-bank"), attributes: Array.from(row.attributes, (attribute) => [attribute.name, attribute.value]), typographyClosed: document.querySelector(".attachment-page__composer-typography-popover")?.hidden };
          });
          const originalAttributes = await page.evaluate(() => window.__terminal_controlsStandardRowAttributes);
          assert(reset.labels.join(" ") === "Esc ⇥ Ctrl ← ↓ ↑ →" && reset.sameNodes && reset.bank === null && reset.typographyClosed === true && JSON.stringify(reset.attributes) === JSON.stringify(originalAttributes), `${shape.name}: reconnect did not restore exact row/popover lifecycle state: ${JSON.stringify(reset)}`);
          let reconnectAuthority;
          if (reconnectCtrlArmed) {
            await delay(120);
            const beforeReconnectLetter = allInputs(await snapshot());
            await page.locator(".xterm-helper-textarea").focus();
            await page.keyboard.type("r");
            const afterReconnectLetter = await waitSnapshot((value) => allInputs(value).length >= beforeReconnectLetter.length + 1);
            const reconnectLetterDelta = allInputs(afterReconnectLetter).slice(beforeReconnectLetter.length);
            const reconnectState = await uiState();
            assert(reconnectLetterDelta.length === 1 && reconnectLetterDelta[0] === "r"
              && reconnectState.standardCtrlPressed === "false" && reconnectState.ctrlSurfaces.length > 0 && reconnectState.ctrlSurfaces.every(value => value === "false"),
            `${shape.name}: transport generation retained Ctrl authority: ${JSON.stringify({ reconnectCtrlArmed, reconnectState, reconnectLetterDelta })}`);
            reconnectAuthority = { armed: reconnectCtrlArmed, settled: reconnectState, letterDelta: reconnectLetterDelta };
          }
          caseEvidence.reconnectReset = reset;
          caseEvidence.reconnectKeyAuthority = reconnectAuthority;

          await page.locator(".attachment-page__composer-typography-trigger").click();
          await control({ closeLive: "generation_failed" });
          await page.waitForFunction(() => document.querySelector(".persea-unified-notice")?.hidden === false, undefined, { timeout: 8_000 });
          const failed = await uiState();
          assert(failed.popoverHidden === true && failed.bank === null && failed.rowAttributes.length === 1 && failed.rowAttributes[0][0] === "class", `${shape.name}: terminal failure left typography/bank presentation alive: ${JSON.stringify(failed)}`);
          caseEvidence.failureReset = { popoverHidden: failed.popoverHidden, bank: failed.bank, rowAttributes: failed.rowAttributes };
        } else {
          const beforeInputs = allInputs(await snapshot());
          await page.getByRole("button", { name: "F1", exact: true }).click();
          const delta = allInputs(await snapshot()).slice(beforeInputs.length);
          assert(delta.length === 1 && delta[0] === F_BYTES[0], `${shape.name}: one F1 tap did not emit one xterm event`);
          await page.getByRole("button", { name: "Terminal Keys" }).click();
          const reset = await page.evaluate(() => {
            const row = document.querySelector(".persea-unified-keybar-row");
            const nodes = Array.from(row.children);
            return { labels: nodes.map((node) => node.textContent), sameNodes: nodes.every((node, index) => node === window.__terminal_controlsStandardNodes[index]), bank: row.getAttribute("data-bank"), attributes: Array.from(row.attributes, (attribute) => [attribute.name, attribute.value]) };
          });
          const originalAttributes = await page.evaluate(() => window.__terminal_controlsStandardRowAttributes);
          assert(reset.labels.join(" ") === "Esc ⇥ Ctrl ← ↓ ↑ →" && reset.sameNodes && reset.bank === null && JSON.stringify(reset.attributes) === JSON.stringify(originalAttributes), `${shape.name}: Back did not restore the exact ordinary row`);
          caseEvidence.backReset = reset;
        }
      }

      const toleratedHTTP = httpErrors.filter((entry) => !([412, 503].includes(entry.status) && new URL(entry.url).pathname === "/api/preferences"));
      const toleratedConsole = browserErrors.filter((entry) => !/WebSocket connection.*failed|Failed to load resource.*(?:412|503)/i.test(entry));
      assert(toleratedHTTP.length === 0, `${shape.name}: unexpected HTTP errors ${JSON.stringify(toleratedHTTP)}`);
      assert(toleratedConsole.length === 0, `${shape.name}: browser errors ${JSON.stringify(toleratedConsole)}`);
      caseEvidence.console = { browserErrors, httpErrors };
      evidence.cases.push(caseEvidence);
      await context.close();
    }
    fs.writeFileSync(path.join(EVIDENCE, `terminal_controls-${ENGINE}${MUTANT ? `-${MUTANT}` : ""}.json`), `${JSON.stringify(evidence, null, 2)}\n`);
    console.log(`terminal controls ${ENGINE}${MUTANT ? ` mutant ${MUTANT}` : ""}: PASS (${evidence.cases.length} shapes)`);
  } finally {
    await browser.close();
    await Promise.race([fixture.close(), delay(4_000)]);
    fixtureUI.cleanup();
  }
}

main().catch((error) => {
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const detail = String(error?.stack || error);
  fs.writeFileSync(path.join(EVIDENCE, `terminal_controls-${ENGINE}${MUTANT ? `-${MUTANT}` : ""}-failure.log`), `${detail}\n`);
  console.error(detail);
  process.exitCode = 1;
});
