"use strict";
const fs = require("fs");
const path = require("path");
const https = require("https");
const http = require("http");
const { startFixture } = require("./unified_reopen_fixture.cjs");
const UI = process.env.PERSEA_KEYS_UI || path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_KEYS_ENGINE || "chromium";
const EVIDENCE = process.env.PERSEA_KEYS_EVIDENCE || require("node:path").join(require("node:os").tmpdir(), "persea-terminal-tests", "run_terminal_keys_browser");
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const assertions = [];
function assert(value, message) { if (!value) throw new Error(message); assertions.push(message.split(":")[0]); }
function json(url, method = "GET", body) {
  return new Promise((resolve, reject) => {
    const request = (url.startsWith("https:") ? https : http).request(url, { method, rejectUnauthorized: false, headers: body ? { "Content-Type": "application/json" } : {} }, (response) => {
      let data = ""; response.on("data", (chunk) => { data += chunk; }); response.on("end", () => { try { resolve(JSON.parse(data)); } catch (e) { reject(e); } });
    });
    request.on("error", reject); if (body) request.write(JSON.stringify(body)); request.end();
  });
}
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
async function main() {
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const fixture = await startFixture(UI, { tls: ENGINE === "webkit", playwrightScreenshotStyle: true });
  const browser = await playwright[ENGINE].launch({ headless: true, ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}) });
  const control = (body) => json(`${fixture.origin}/__fixture/control`, "POST", body);
  const snapshot = () => json(`${fixture.origin}/__fixture/control`);
  const evidence = { engine: ENGINE, shapes: [], console: [], assertions };
  try {
    for (const [width, height] of (process.env.PERSEA_KEYS_ONE_SHAPE ? [[390, 844]] : [[320, 568], [390, 844], [430, 932], [844, 390]])) {
      await control({ reset: true });
      const context = await browser.newContext({ viewport: { width, height }, hasTouch: true, isMobile: true, ignoreHTTPSErrors: true });
      const page = await context.newPage();
      const errors = [];
      let phase = "load";
      const pageEvidence = { width, height, errors };
      (evidence.pages ??= []).push(pageEvidence);
      page.on("console", (message) => errors.push(`${phase}: console ${message.type()}: ${message.text()}`));
      page.on("pageerror", (error) => errors.push(`${phase}: pageerror: ${error}`));
      const inventory = await json(`${fixture.origin}/api/inventory`);
      const session = inventory.realms[0].servers[0].sessions[0];
      const url = `${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" })}`;
      await page.goto(url);
      await page.waitForSelector(".persea-unified-xterm .xterm-helper-textarea");
      for (let i = 0; i < 100; i++) { if ((await snapshot()).attachments.some((a) => a.mode === "CONTROL")) break; await delay(30); }
      const panel = page.locator(".persea-terminal-keys");
      const toggle = page.locator(".persea-unified-keybar-key--bank");
      const helper = page.locator(".persea-unified-xterm .xterm-helper-textarea");
      const inputs = async () => (await snapshot()).attachments.flatMap((a) => a.inputs).join("");
      const click = async (id) => { await panel.locator(`[data-key-control="${id}"]`).click(); };
      const openKeys = async () => {
        if (await toggle.isVisible()) await toggle.click();
        else { await page.getByRole("button", { name: "Quick actions", exact: true }).click(); await page.getByRole("button", { name: "Open Terminal Keys", exact: true }).click(); }
      };
      assert(!await toggle.isVisible(), "Accessory stays hidden with keyboard closed");
      phase = "disclosure-focus";
      const initialInput = await inputs();
      await helper.evaluate((node) => node.blur());
      await page.getByRole("button", { name: "Quick actions", exact: true }).tap();
      const menu = page.getByRole("group", { name: "Terminal menu", exact: true });
      assert(await menu.isVisible(), "Task menu is accessible with keyboard hidden");
      const menuLabels = await menu.locator("button").evaluateAll(nodes => nodes.map(node => node.getAttribute("aria-label")));
      assert(menuLabels.includes("Open Terminal Keys") && !menuLabels.some(label => /^(?:Escape|Tab|Enter|Arrow|Ctrl\+|Alt\+|F\d)/.test(label || "")), "Task menu routes to Keys without duplicate individual key tiles");
      for (const target of await menu.locator("button:visible").all()) {
        await target.scrollIntoViewIfNeeded();
        const geometry = await target.evaluate(node => { const r = node.getBoundingClientRect(); const s = node.closest(".persea-unified-sheet").getBoundingClientRect(); return { label: node.getAttribute("aria-label"), x: r.x, y: r.y, right: r.right, bottom: r.bottom, width: r.width, height: r.height, sheet: { x: s.x, y: s.y, right: s.right, bottom: s.bottom }, viewport: { width: innerWidth, height: innerHeight } }; });
        (pageEvidence.menuTargets ||= []).push(geometry);
        assert(geometry.width >= 43.9 && geometry.height >= 43.9 && geometry.x >= Math.max(0, geometry.sheet.x) - 1 && geometry.y >= Math.max(0, geometry.sheet.y) - 1 && geometry.right <= Math.min(geometry.viewport.width, geometry.sheet.right) + 1 && geometry.bottom <= Math.min(geometry.viewport.height, geometry.sheet.bottom) + 1, `Task menu target is contained and reachable: ${JSON.stringify(geometry)}`);
      }
      await menu.getByRole("button", { name: "Close terminal menu", exact: true }).tap();
      assert(await inputs() === initialInput && !await helper.evaluate(node => node === document.activeElement), "Menu inspection does not emit input or summon keyboard");
      await openKeys();
      assert(await panel.isVisible(), "Keys accessible with keyboard hidden");
      assert(!await helper.evaluate((node) => node === document.activeElement), "Keys must not summon keyboard");
      await openKeys();
      await helper.focus();
      await helper.evaluate((node) => { node.value = "native draft"; node.setSelectionRange(3, 7); window.__keyFocus = 0; node.addEventListener("focus", () => window.__keyFocus++); });
      await openKeys();
      assert(await helper.evaluate((node) => node === document.activeElement && node.value === "native draft" && node.selectionStart === 3 && node.selectionEnd === 7), "Keys preserves native text focus and selection");
      assert(await page.evaluate(() => window.__keyFocus) === 0, "Keys does not refocus editor");
      phase = "short-layout";
      const reducedHeight = Math.max(240, height - 336);
      await page.setViewportSize({ width, height: reducedHeight });
      await delay(80);
      const rects = await page.evaluate(() => {
        const rect = (selector) => { const r = document.querySelector(selector).getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height, bottom: r.bottom }; };
        return { panel: rect(".persea-terminal-keys"), stage: rect(".persea-unified-stage"), bar: rect(".persea-unified-keybar"), shell: rect(".persea-unified-terminal"), overflow: document.documentElement.scrollWidth > innerWidth + 1,
          compact: document.querySelector(".persea-terminal-keys").dataset.compact === "true",
          targets: [...document.querySelectorAll(".persea-terminal-keys button, .persea-unified-keybar button")].filter((node) => node.getClientRects().length).map((node) => ({ label: node.textContent, w: node.getBoundingClientRect().width, h: node.getBoundingClientRect().height })) };
      });
      assert(!rects.overflow, "No horizontal document overflow");
      assert(rects.stage.h >= 44, `Terminal content remains visible: ${JSON.stringify(rects)}`);
      assert(rects.compact ? rects.panel.y >= rects.bar.y && rects.panel.bottom <= rects.bar.bottom : rects.panel.bottom <= rects.bar.y + 1, "Panel occupies strip in short bands and sits above it otherwise");
      assert(rects.bar.bottom <= rects.shell.bottom + 1, "Strip stays inside shell");
      assert(rects.targets.every((r) => r.w >= 43.9 && r.h >= 43.9), `44px targets: ${JSON.stringify(rects.targets)}`);
      await delay(80);
      const settled = await page.evaluate(() => [".persea-unified-stage", ".persea-unified-keybar", ".persea-terminal-keys"].map((selector) => { const r = document.querySelector(selector).getBoundingClientRect(); return { y: r.y, h: r.height }; }));
      assert(settled.every((rect, i) => Math.abs(rect.y - [rects.stage, rects.bar, rects.panel][i].y) < 1 && Math.abs(rect.h - [rects.stage, rects.bar, rects.panel][i].h) < 1), "Keyboard geometry remains stable after settling");
      assert(await inputs() === initialInput, "Disclosure/focus/selection has no input");
      phase = "native-panel-gap";
      await click("view:All keys");
      const gap = await panel.evaluate(node => {
        const nav = node.querySelector(".persea-terminal-keys__nav"), r = node.getBoundingClientRect();
        let point;
        for (let y = r.top + 6; y < r.bottom - 6 && !point; y += 6) for (let x = r.left + 6; x < r.right - 6; x += 6) {
          if (document.elementFromPoint(x, y) === nav) { point = { x, y }; break; }
        }
        window.__gapEvents = [];
        node.addEventListener("pointerdown", event => window.__gapEvents.push({ trusted: event.isTrusted, interactive: !!event.target.closest("button,input,textarea,select,a,[contenteditable]"), target: event.target.className }), true);
        window.__gapFocusEvents = [];
        const helper = document.querySelector(".persea-unified-xterm .xterm-helper-textarea");
        for (const type of ["focusin", "focusout"]) helper.addEventListener(type, () => window.__gapFocusEvents.push(type));
        return point;
      });
      assert(gap, "Native gap has a visible noninteractive target");
      // Chromium touch adjustment can legitimately choose a nearby key in
      // narrower gaps. The standard phone reproduces the real touch defect;
      // other shapes test the same native focus default with precise mouse input.
      if (width === 390) await page.touchscreen.tap(gap.x, gap.y);
      else await page.mouse.click(gap.x, gap.y);
      await delay(100);
      const gapState = await helper.evaluate(node => ({ focused: node === document.activeElement, draft: node.value, selection: [node.selectionStart, node.selectionEnd], focusEvents: window.__gapFocusEvents, pointerEvents: window.__gapEvents }));
      pageEvidence.gap = { point: gap, mechanism: width === 390 ? "trusted touch" : "trusted mouse", ...gapState };
      assert(gapState.pointerEvents.length === 1 && gapState.pointerEvents[0].trusted && !gapState.pointerEvents[0].interactive, "Native gap tap targets panel space, not a nearby key");
      assert(gapState.focused && gapState.draft === "native draft" && gapState.selection[0] === 3 && gapState.selection[1] === 7 && gapState.focusEvents.length === 0, `Native panel gap preserves focus, draft and selection: ${JSON.stringify(gapState)}`);
      assert(await panel.isVisible() && await inputs() === initialInput, "Native gap retains panel and emits no input");
      await click("view:Favorites");
      phase = "composer-open";
      await page.locator(".persea-unified-composer-toggle").click();
      phase = "composer-fill";
      await page.locator(".attachment-page__composer-textarea").fill("local draft");
      await page.locator(".attachment-page__composer-textarea").evaluate((node) => node.setSelectionRange(2, 6));
      phase = "composer-shrink";
      await page.setViewportSize({ width, height: 240 }); await delay(80);
      phase = "composer-keys";
      await openKeys(); await delay(80);
      const composerBand = await page.locator(".persea-unified-stage").boundingBox();
      assert(composerBand.height >= 44, `Composer and Keys preserve terminal content: ${JSON.stringify({ width, height: composerBand.height, layout: await page.evaluate(() => [".persea-unified-terminal", ".persea-unified-toolbar", ".attachment-page__composer", ".persea-unified-keybar", ".persea-terminal-keys"].map((selector) => { const el = document.querySelector(selector); const r = el?.getBoundingClientRect(); const s = el && getComputedStyle(el); return { selector, height: r?.height, top: r?.top, max: s?.maxHeight, padding: s?.padding, compact: el?.dataset.compact }; })) })}`);
      assert(await page.locator(".attachment-page__composer-textarea").evaluate((node) => node === document.activeElement && node.value === "local draft" && node.selectionStart === 2 && node.selectionEnd === 6), "Combined short layout preserves draft, focus and selection");
      await delay(80);
      const composerSettled = await page.locator(".persea-unified-stage").boundingBox();
      assert(Math.abs(composerSettled.height - composerBand.height) < 1 && Math.abs(composerSettled.y - composerBand.y) < 1, "Composer terminal geometry remains stable after settling");
      phase = "composer-hide-keyboard";
      await click("hide-keyboard");
      assert(await page.locator(".attachment-page__composer-textarea").evaluate((node) => node !== document.activeElement && node.value === "local draft" && node.selectionStart === 2 && node.selectionEnd === 6), "Explicit Hide keyboard blurs without changing draft or selection");
      phase = "composer-close";
      await page.locator(".attachment-page__composer-close").click();
      await page.setViewportSize({ width, height: reducedHeight }); await delay(80);
      await openKeys();
      await click("view:All keys"); await click("modifier:ctrl");
      await openKeys();
      assert(await page.locator(".persea-unified-keybar-row").isVisible(), "Keys close restores core strip");
      assert(await page.locator(".persea-unified-keybar-key[data-latched]").getAttribute("data-latched") === "false", "Keys close clears modifiers");
      await openKeys();
      await click("view:Favorites");
      phase = "repeated-explicit";
      await click("key:c:1");
      await click("key:c:1");
      await delay(30);
      assert((await inputs()).slice(initialInput.length) === "\x03\x03", "Repeated favorite emits one byte each and remains open");
      await click("view:All keys");
      await click("view:Fn");
      await click("key:f12:0");
      assert(await panel.isVisible(), "Repeated catalog remains open");
      await click("view:All keys");
      await click("key-group"); await click("choose-group:Letters");
      await click("modifier:ctrl");
      phase = "native-insertion";
      const beforeText = await inputs();
      await helper.evaluate((node) => { node.value = ""; });
      await helper.focus();
      await page.keyboard.insertText("dictated words");
      await delay(30);
      assert(!await panel.isVisible(), "Native text closes panel");
      assert((await inputs()).slice(beforeText.length) === "dictated words", "Native text is not consumed by modifiers");
      for (const method of ["press", "insertText"]) {
        await helper.focus(); await openKeys(); await click("view:All keys"); await click("modifier:ctrl");
        const before = await inputs();
        await page.keyboard[method]("c"); await delay(40);
        assert((await inputs()).slice(before.length) === "\x03", `Native ${method} consumes Ctrl once without plain C`);
        await page.keyboard[method]("a"); await delay(40);
        const nativeBytes=(await inputs()).slice(before.length);
        (evidence.nativeChords??=[]).push({method,bytes:nativeBytes,state:await helper.evaluate(node=>({focused:document.activeElement===node,value:node.value}))});
        assert(nativeBytes === "\x03a", `Native ${method} clears Ctrl after one letter: ${JSON.stringify(nativeBytes)}`);
      }
      await helper.focus(); await openKeys(); await click("view:Favorites");
      assert(await panel.locator('[data-key-control="modifier:alt"]').isVisible(), "Default Favorites includes Alt");
      await click("modifier:alt");
      const beforeAlt = await inputs(); await page.keyboard.insertText("b"); await delay(40);
      assert((await inputs()).slice(beforeAlt.length) === "\x1bb", "Favorite Alt applies to native letter once");
      await openKeys();
      assert(await panel.locator("[data-key-control=customize]").count() === 1, "Keyboard settings remains reachable");
      phase = "settings";
      await click("customize");
      const settings = page.getByRole("dialog");
      await settings.getByRole("button", { name: "This device", exact: true }).click();
      await settings.getByRole("button", { name: "Prefixes", exact: true }).click();
      await settings.getByLabel("New prefix name", { exact: true }).fill("Herdr");
      await settings.getByRole("button", { name: "Add prefix", exact: true }).click();
      await settings.getByRole("button", { name: "ctrl modifier", exact: true }).click();
      await settings.getByRole("button", { name: /^Add Ctrl\+A$/i }).click();
      await settings.getByRole("button", { name: "Save for this device", exact: true }).click();
      const customPrefix = await page.evaluate(() => JSON.parse(localStorage.getItem("persea-terminal.keyboard-device.v1")));
      assert(customPrefix?.prefixes?.Herdr === "key:a:1", "User-defined provider prefix persists independently");
      await settings.getByRole("button", { name: "Favorites", exact: true }).click();
      await settings.getByRole("button", { name: "Add key or chord", exact: true }).click();
      await settings.getByLabel("Key group", { exact: true }).selectOption("Sequences");
      await settings.getByLabel("Sequence prefix", { exact: true }).selectOption("Herdr");
      if (await settings.getByRole("button", { name: "ctrl modifier", exact: true }).getAttribute("aria-pressed") === "true") await settings.getByRole("button", { name: "ctrl modifier", exact: true }).click();
      await settings.getByRole("button", { name: /^Add Herdr: prefix, n$/i }).click();
      await settings.getByRole("button", { name: "Save for this device", exact: true }).click();
      await settings.getByRole("button", { name: "Close keyboard settings", exact: true }).click();
      await openKeys();
      await click("view:Favorites");
      const sequenceBefore = await inputs();
      await click("sequence:Herdr:key:n:0"); await delay(30);
      assert((await inputs()).slice(sequenceBefore.length) === "\x01n", "Configured prefix and suffix arrive in order");
      await click("customize");
      await settings.getByRole("button", { name: "This device", exact: true }).click();
      await settings.getByRole("button", { name: "Favorites", exact: true }).click();
      await settings.getByLabel("Keyboard preset", { exact: true }).selectOption("Shell");
      await settings.locator('[data-settings-control="key:a:1:move:1"]').click();
      await settings.getByRole("button", { name: "Save for this device", exact: true }).click();
      const stored = await page.evaluate(() => JSON.parse(localStorage.getItem("persea-terminal.keyboard-device.v1")));
      assert(stored.layout.favorites[1] === "key:a:1", "Favorite order persists after explicit save");
      await settings.getByRole("button", { name: "Close keyboard settings", exact: true }).click();
      await openKeys(); await click("view:Favorites");
      phase = "dictation-replay";
      if (width === 390) {
        const fixtureModule = { exports: {} };
        const compiled = require(path.join(UI, "node_modules/esbuild")).transformSync(fs.readFileSync(path.join(__dirname, "fixtures/ios_firefox_dictation_restart_20260905.ts"), "utf8"), { loader: "ts", format: "cjs" });
        require("node:vm").runInNewContext(compiled.code, { module: fixtureModule });
        await helper.focus(); await page.keyboard.press("Control+c"); await delay(30);
        const nativeBefore = await inputs();
        await page.evaluate((entries) => {
          const textarea = document.querySelector(".xterm-helper-textarea"); let held = false;
          const physicalKey = (type) => {
            const event = new KeyboardEvent(type, { key: " ", code: "Space", bubbles: true, cancelable: true, composed: true });
            for (const [property, value] of Object.entries({ keyCode: 32, which: 32, charCode: type === "keypress" ? 32 : 0 })) Object.defineProperty(event, property, { get: () => value });
            textarea.dispatchEvent(event);
          };
          for (const entry of entries) {
            if (entry.kind === "input") textarea.value = entry.field;
            // The capture predates deferred sentinel removal. A retained sole
            // sentinel and an empty field represent the same native draft.
            else if (textarea.value !== entry.field && !(textarea.value === "\u200b" && entry.field === "")) throw new Error(`Native field diverged before ${entry.t}`);
            textarea.setSelectionRange(...entry.selection);
            if (entry.kind === "keydown") { physicalKey("keydown"); physicalKey("keypress"); held = true; }
            else { textarea.dispatchEvent(new InputEvent(entry.kind, { inputType: entry.inputType ?? "", data: entry.data ?? null, isComposing: entry.composing, bubbles: true, cancelable: true, composed: true })); if (entry.kind === "input" && held) { physicalKey("keyup"); held = false; } }
          }
        }, fixtureModule.exports.nativeDictationRestart);
        await page.keyboard.press("Control+c"); await delay(40);
        const nativeBytes = (await inputs()).slice(nativeBefore.length, -1);
        const held = []; for (const char of nativeBytes) { if (char === "\x7f") held.pop(); else held.push(char); }
        assert(held.join("") === "test 123 best again 123" && !/[\r\n]/.test(nativeBytes), `Native dictation replay: ${JSON.stringify(nativeBytes)}`);
        evidence.nativeDictation = { text: held.join(""), bytes: nativeBytes };
        await openKeys();
      }
      phase = "held-pointer";
      // A held pointer is not a tap. Lifecycle closure invalidates its release.
      await page.setViewportSize({ width, height: 1000 }); await delay(80);
      if (!await panel.isVisible()) await openKeys();
      await click("view:Favorites");
      assert(await panel.getAttribute("data-compact") !== "true", "Tall band uses separate panel");
      const stationary = await panel.locator('[data-key-control="view:Favorites"]').boundingBox();
      await page.mouse.move(stationary.x + stationary.width / 2, stationary.y + stationary.height / 2); await page.mouse.down();
      await page.setViewportSize({ width, height: 240 }); await delay(80);
      assert(await panel.getAttribute("data-compact") !== "true", "Layout does not switch under held pointer");
      await page.mouse.move(2, 2); await page.mouse.up(); await delay(80);
      assert(await panel.getAttribute("data-compact") === "true", `Layout switches after release: ${JSON.stringify(await page.evaluate(() => ({active: document.activeElement?.className, barHidden: document.querySelector(".persea-unified-keybar").hidden, panelHidden: document.querySelector(".persea-terminal-keys").hidden, compact: document.querySelector(".persea-terminal-keys").dataset.compact})))}`);
      const key = panel.locator("[data-key-control=\"key:c:1\"]");
      await key.scrollIntoViewIfNeeded();
      const box = await key.boundingBox(); const beforeDrag = await inputs();
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
      await page.mouse.down();
      assert(await inputs() === beforeDrag, "Press does not emit before release");
      await page.mouse.move(box.x + box.width / 2 + 15, box.y + box.height / 2, { steps: 3 });
      await page.mouse.up(); await delay(30);
      assert(await inputs() === beforeDrag, "Drag cancels key");
      await key.scrollIntoViewIfNeeded(); const held = await key.boundingBox();
      await page.mouse.move(held.x + held.width / 2, held.y + held.height / 2); await page.mouse.down();
      await control({ closeLive: "transport_lost" }); await delay(80); await page.mouse.up();
      assert(await inputs() === beforeDrag, "Reconnect never replays held key");
      assert(!await panel.isVisible(), "Transport closes pending panel state");
      const end = await snapshot();
      assert(end.attachments.every((a) => a.resizes === 0 && a.resizeRequests.length === 0), "Panel/viewport actions never resize remote terminal");
      // A fresh attachment in the same browser reads persistent keys.
      phase = "reload";
      const inventory2 = await json(`${fixture.origin}/api/inventory`); const session2 = inventory2.realms[0].servers[0].sessions[0];
      await page.goto("about:blank");
      await page.setViewportSize({ width, height });
      await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session2.handles.control, mode: "control", history: "1000", name: session2.name, draft_scope: fixture.draftScope, engine: "unified-dev" })}`);
      await page.waitForSelector(".persea-unified-xterm .xterm-helper-textarea"); await openKeys(); await click("view:Favorites");
      assert(await panel.locator("[data-key-control=\"key:a:1\"]").count() === 1, "Favorites survive page replacement");
      phase = "screenshot";
      await page.screenshot({ path: path.join(EVIDENCE, `keys-${ENGINE}-${width}x${height}.png`), caret: "initial" });
      // Collect every viewport before enforcing the unchanged clean-console gate.
      evidence.shapes.push({ width, height, rects, passed: true });
      await context.close();
    }
    const context = await browser.newContext({ ignoreHTTPSErrors: true });
    const page = await context.newPage();
    page.on("console", (message) => evidence.console.push(message.text())); page.on("pageerror", (error) => evidence.console.push(String(error)));
    await page.route("**/__key_catalog", (route) => route.fulfill({ contentType: "text/html", body: "<!doctype html><html><body></body></html>" }));
    await page.route("**/__key_catalog.js", (route) => route.fulfill({ contentType: "text/javascript", body: fs.readFileSync(path.join(UI, "dist/test/terminal_keys_catalog_harness.js")) }));
    await page.goto(`${fixture.origin}/__key_catalog`);
    await page.addScriptTag({ url: `${fixture.origin}/__key_catalog.js` });
    evidence.catalogCases = await page.evaluate(() => window.runTerminalKeyCatalog());
    assert(evidence.catalogCases > 600, "Complete supported catalog covers both cursor modes");
    assert(evidence.console.length === 0, "Catalog console clean");
    for (const tested of evidence.pages) assert(tested.errors.length === 0, `Clean console ${tested.width}x${tested.height}: ${tested.errors.join("; ")}`);
    await context.close();
    fs.writeFileSync(path.join(EVIDENCE, `keys-${ENGINE}.json`), JSON.stringify(evidence, null, 2));
    console.log(`Keys ${ENGINE}: PASS (${evidence.shapes.length} viewports, ${evidence.catalogCases} mode/chord encodings)`);
  } catch (error) { evidence.failure = String(error.stack || error); fs.writeFileSync(path.join(EVIDENCE, `keys-${ENGINE}-failure.json`), JSON.stringify(evidence, null, 2)); throw error; } finally { await browser.close(); await fixture.close(); }
}
main().catch((error) => { console.error(error.stack || String(error)); process.exitCode = 1; });
