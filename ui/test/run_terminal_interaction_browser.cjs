"use strict";

const fs = require("fs");
const https = require("https");
const path = require("path");
const { startClipboardFixture: startFixture } = require("./clipboard_fixture.cjs");
const { requestJSON: requestHTTPJSON } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_TERMINAL_INTERACTION_ENGINE || "chromium";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const EVIDENCE = path.resolve(process.env.PERSEA_TERMINAL_INTERACTION_EVIDENCE_DIR || path.join("/tmp", `persea-terminal_interaction-${ENGINE}`));
const SHAPE = process.env.PERSEA_TERMINAL_INTERACTION_SHAPE || "";
const SHAPES = [
  { name: "desktop", viewport: { width: 1180, height: 820 }, touch: false },
  { name: "tablet", viewport: { width: 820, height: 900 }, touch: true },
  { name: "phone-390", viewport: { width: 390, height: 844 }, touch: true },
  { name: "phone-360", viewport: { width: 360, height: 780 }, touch: true },
  { name: "phone-landscape", viewport: { width: 844, height: 390 }, touch: true },
].filter((shape) => SHAPE === "" || shape.name === SHAPE);
const REFIT_LIFECYCLE_CASE = process.env.PERSEA_REFIT_LIFECYCLE_CASE || "all";
const refitLifecycleEnabled = (name) => REFIT_LIFECYCLE_CASE === "all" || REFIT_LIFECYCLE_CASE === name;

function assert(value, message) { if (!value) throw new Error(message); }
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const requestJSON = (url, method = "GET", body) => {
  if (!url.startsWith("https:")) return requestHTTPJSON(url, method, body);
  return new Promise((resolve, reject) => {
    const request = https.request(url, { method, rejectUnauthorized: false, headers: body ? { "Content-Type": "application/json" } : {} }, (response) => {
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

async function main() {
  assert(path.isAbsolute(MODULE), "PERSEA_PLAYWRIGHT_MODULE must be absolute");
  const playwright = require(MODULE);
  assert(playwright[ENGINE], `Playwright has no ${ENGINE} engine`);
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const fixture = await startFixture(UI, { tls: ENGINE === "webkit" });
  const control = (input) => requestJSON(`${fixture.origin}/__fixture/control`, "POST", input);
  const snapshot = () => requestJSON(`${fixture.origin}/__fixture/control`);
  const browser = await playwright[ENGINE].launch({
    headless: true,
    ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
  });
  const evidence = { engine: ENGINE, cases: [], screenshots: [] };
  try {
    for (const shape of SHAPES) {
      console.log(`TERMINAL_INTERACTION_PHASE ${ENGINE} ${shape.name} start`);
      await control({ reset: true, switchSessions: true, sessionBState: "open" });
      const inventory = await requestJSON(`${fixture.origin}/api/inventory`);
      const session = inventory.realms[0].servers[0].sessions[0];
      const context = await browser.newContext({ viewport: shape.viewport, isMobile: shape.touch, hasTouch: shape.touch, deviceScaleFactor: shape.touch ? 2 : 1, ignoreHTTPSErrors: true });
      // the software keyboard is not drivable from Playwright,
      // but the page reads window.visualViewport once at construction, so a
      // transparent proxy (real readings, real events forwarded) with an
      // override lets a probe shrink the visual height and dispatch `resize`
      // exactly as iOS does when the keyboard rises.
      await context.addInitScript(() => {
        const real = window.visualViewport;
        if (!real) return;
        const override = { height: null, scale: null };
        const target = new EventTarget();
        const fake = {
          addEventListener: (...args) => target.addEventListener(...args),
          removeEventListener: (...args) => target.removeEventListener(...args),
          dispatchEvent: (event) => target.dispatchEvent(event),
          get width() { return real.width; },
          get height() { return override.height ?? real.height; },
          get scale() { return override.scale ?? real.scale; },
          get offsetTop() { return real.offsetTop; },
          get offsetLeft() { return real.offsetLeft; },
          get pageTop() { return real.pageTop; },
          get pageLeft() { return real.pageLeft; },
        };
        for (const type of ["resize", "scroll"]) real.addEventListener(type, () => target.dispatchEvent(new Event(type)));
        Object.defineProperty(window, "visualViewport", { configurable: true, get: () => fake });
        Object.defineProperty(window, "__terminal_layoutViewport", { configurable: true, value: {
          set(next) { Object.assign(override, next); target.dispatchEvent(new Event("resize")); },
        } });
      });
      await context.addInitScript(() => {
        const state = { writes: [], fail: false, readFail: false };
        Object.defineProperty(window, "__terminal_interactionClipboard", { configurable: true, value: state });
        Object.defineProperty(Navigator.prototype, "clipboard", { configurable: true, get() { return {
          writeText(text) { state.writes.push(String(text)); return state.fail ? Promise.reject(new Error("fixture clipboard refusal")) : Promise.resolve(); },
          readText() { return state.readFail ? Promise.reject(new Error("fixture clipboard read refusal")) : Promise.resolve(state.writes.at(-1) || ""); },
        }; } });
      });
      const page = await context.newPage();
      const errors = [];
      const httpErrors = [];
      page.on("pageerror", (error) => errors.push(String(error)));
      page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
      page.on("response", (response) => { if (response.status() >= 400) httpErrors.push({ status: response.status(), url: response.url() }); });
      const url = `${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" })}`;
      await page.goto(url, { waitUntil: "load" });
      await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
      await page.locator(".persea-unified-view-disclosure").waitFor();
      assert((await page.locator(".persea-unified-select-context").textContent())?.trim() === "Select", `${shape.name}: contextual selection owner is not idle`);
      assert(await page.locator(".persea-unified-toolbar-paste").getAttribute("aria-label") === "Open clipboard", `${shape.name}: direct Clipboard action is absent`);
      await delay(150);
      assert((await snapshot()).snippets.get === 0, `${shape.name}: closed Saved text polled before an explicit request`);

      const tile = (name) => page.locator(".persea-unified-sheet__tile").filter({ has: page.locator(".persea-unified-sheet__label", { hasText: name }) }).first();
      const openSheet = async () => {
        const sheet = page.locator(".persea-unified-sheet");
        if (!await sheet.isVisible()) await page.getByRole("button", { name: "Quick actions", exact: true }).click();
        await sheet.waitFor({ state: "visible" });
      };
      const openView = async () => {
        const popover = page.locator(".persea-unified-view-popover");
        if (!await popover.isVisible()) await page.locator(".persea-unified-view-disclosure").click();
        await popover.waitFor({ state: "visible" });
        return popover;
      };
      const resizes = async () => (await snapshot()).attachments.reduce((sum, attachment) => sum + attachment.resizes, 0);
      const inputTranscript = async () => (await snapshot()).attachments.flatMap((attachment) => attachment.inputs).join("");
      const toolbarBox = () => page.locator(".persea-unified-toolbar").boundingBox();
      const sameBox = (a, b) => a && b && ["x", "y", "width", "height"].every((key) => Math.abs(a[key] - b[key]) < 0.2);
      const shellBoxes = () => page.evaluate(() => {
        const selectors = [".persea-unified-toolbar", ".persea-unified-stage", ".persea-unified-scroll", ".persea-unified-composer", ".persea-unified-keybar"];
        const boxes = {};
        for (const selector of selectors) {
          const node = document.querySelector(selector);
          if (!node) continue;
          const box = node.getBoundingClientRect();
          boxes[selector] = { x: box.x, y: box.y, width: box.width, height: box.height };
        }
        boxes.visualViewport = window.visualViewport
          ? { width: window.visualViewport.width, height: window.visualViewport.height }
          : { width: window.innerWidth, height: window.innerHeight };
        return boxes;
      });
      const sameShell = (a, b) => Object.keys(a).every((key) => b[key] && Object.keys(a[key]).every((part) => Math.abs(a[key][part] - b[key][part]) < 0.2));
      const sameShellSize = (a, b) => Object.keys(a).every((key) => b[key] && ["width", "height"].every((part) => a[key][part] === undefined || Math.abs(a[key][part] - b[key][part]) < 0.2));
      const shot = async (suffix) => {
        const file = path.join(EVIDENCE, `${ENGINE}-${shape.name}-${suffix}.png`);
        await page.screenshot({ path: file, fullPage: true }); evidence.screenshots.push(file);
      };
      const longPress = async (locator, move = false) => {
        await locator.scrollIntoViewIfNeeded();
        const box = await locator.boundingBox(); assert(box, `missing long-press target for ${shape.name}`);
        const x = box.x + box.width / 2; const y = box.y + box.height / 2;
        await page.mouse.move(x, y); await page.mouse.down();
        if (move) { await delay(80); await page.mouse.move(x + 20, y); }
        await delay(700); await page.mouse.up();
      };
      const frozenMetrics = () => page.evaluate(() => {
        const liveRows = document.querySelector(".xterm-rows");
        const screen = document.querySelector(".xterm-screen");
        const body = document.querySelector(".persea-unified-select__body");
        const frozenRow = document.querySelector(".persea-unified-select__row");
        if (!liveRows || !screen || !body || !frozenRow) throw new Error("missing live or frozen metric owner");
        const liveStyle = getComputedStyle(liveRows);
        const frozenStyle = getComputedStyle(body);
        const renderedRows = liveRows.children.length;
        return {
          live: {
            fontFamily: liveStyle.fontFamily,
            fontSize: Number.parseFloat(liveStyle.fontSize),
            rowHeight: screen.getBoundingClientRect().height / renderedRows,
          },
          frozen: {
            fontFamily: frozenStyle.fontFamily,
            fontSize: Number.parseFloat(frozenStyle.fontSize),
            rowHeight: frozenRow.getBoundingClientRect().height,
          },
        };
      });
      const assertFrozenMetrics = async (label) => {
        const metrics = await frozenMetrics();
        assert(metrics.live.fontFamily === metrics.frozen.fontFamily
          && Math.abs(metrics.live.fontSize - metrics.frozen.fontSize) <= 0.05
          && Math.abs(metrics.live.rowHeight - metrics.frozen.rowHeight) <= 0.1,
        `${shape.name}/${label}: frozen metrics differ from xterm: ${JSON.stringify(metrics)}`);
        return metrics;
      };
      const selectAction = () => page.locator(".persea-unified-select-context");
      const pasteAction = () => page.locator(".persea-unified-toolbar-paste");
      // every state of the two text-bearing controls fits its own
      // content box. Phones (coarse, <720px container) present the state as a
      // glyph with the words kept for the accessible name; everywhere else the
      // text range must fit the content box. Targets stay ≥44px on touch and
      // the boxes are asserted constant by the flows around each call.
      const fitsBox = async (locator, label) => {
        const measure = await locator.evaluate((node) => {
          const style = getComputedStyle(node);
          const content = node.clientWidth - Number.parseFloat(style.paddingLeft) - Number.parseFloat(style.paddingRight);
          const word = node.querySelector(".persea-unified-toolbar__word");
          const glyphNode = node.querySelector(".persea-unified-toolbar__glyph");
          const range = document.createRange(); range.selectNodeContents(word || node);
          return {
            text: (node.textContent || "").trim(), state: node.dataset.selectState || node.dataset.pasteState || null,
            textWidth: range.getBoundingClientRect().width, content, clientWidth: node.clientWidth, scrollWidth: node.scrollWidth,
            glyph: glyphNode ? getComputedStyle(glyphNode, "::before").content : null,
            glyphShown: glyphNode ? getComputedStyle(glyphNode).display !== "none" : false,
            wordShown: word ? getComputedStyle(word).display !== "none" : true,
            fontSize: style.fontSize,
          };
        });
        const box = await locator.boundingBox();
        // words on every shape — the word must fit the content box
        // and nothing may overflow the button.
        const iconOnly = false;
        const fits = measure.wordShown && !measure.glyphShown && measure.textWidth <= measure.content + 0.5 && measure.scrollWidth <= measure.clientWidth;
        const target = !shape.touch || (box && box.width >= 44 && box.height >= 44);
        assert(fits && target, `${shape.name}: ${label} does not fit its box: ${JSON.stringify({ iconOnly, measure, box })}`);
        evidence.cases.push({ shape: `${shape.name}-fit-${label}`, iconOnly, ...measure, box });
      };
      // Paste opens the shared picker. Device import is a separate trusted
      // request; refusal offers local text entry and never sends terminal input.
      if (shape.name === "desktop" && refitLifecycleEnabled("c5")) {
        await page.evaluate(() => { window.__terminal_interactionClipboard.readFail = true; });
        const beforeDeniedImport = (await snapshot()).attachments.at(-1).inputs.length;
        await pasteAction().click();
        const clipboard = page.locator(".persea-clipboard");
        await clipboard.getByRole("button", { name: "Paste from device", exact: true }).click();
        const denied = clipboard.locator(".persea-clipboard__status");
        await denied.waitFor({ state: "visible" });
        await clipboard.getByRole("button", { name: "Add text", exact: true }).waitFor();
        assert(await clipboard.locator("textarea").count() === 0, `${shape.name}: denied import opened the text editor`);
        const message = (await denied.textContent()) || "";
        assert(/cancelled|blocked/i.test(message), `${shape.name}: denied device import omitted its status: ${JSON.stringify(message)}`);
        assert((await snapshot()).attachments.at(-1).inputs.length === beforeDeniedImport, `${shape.name}: denied import emitted terminal input`);
        await clipboard.getByRole("button", { name: "Close clipboard", exact: true }).click();
        await page.evaluate(() => { window.__terminal_interactionClipboard.readFail = false; });
      }
      const openFrozenSnapshot = async (label) => {
        await selectAction().click();
        await page.locator(".persea-unified-select").waitFor({ state: "visible" });
        const metrics = await assertFrozenMetrics(label);
        await selectAction().click();
        await page.locator(".persea-unified-select").waitFor({ state: "hidden" });
        return metrics;
      };

      // a programmatic Terminal.paste clears xterm's helper
      // textarea. The unified delivery sink must reconcile the iOS router in
      // the same tick so native repeat still has deletable sentinel content.
      // Synthetic input events model only the browser-side mechanism: real
      // acceleration remains an iPhone hardware gate.
      if (shape.name === "phone-390") {
        await openSheet();
        await tile("Clipboard").click();
        for (let attempt = 0; attempt < 100 && (await snapshot()).snippets.get === 0; attempt += 1) await delay(20);
        assert((await snapshot()).snippets.get > 0, `${shape.name}: snippet/clipboard service did not reach its ready snapshot`);
        await page.locator(".persea-clipboard").getByRole("button", { name: "Close clipboard", exact: true }).click();
        await page.locator(".persea-clipboard").waitFor({ state: "hidden" });
        const helper = page.locator(".xterm-helper-textarea");
        await helper.evaluate((node) => node.focus({ preventScroll: true }));
        await page.waitForFunction(() => document.querySelector(".xterm-helper-textarea")?.value === "\u200b");

        await selectAction().click();
        const overlay = page.locator(".persea-unified-select");
        await overlay.waitFor({ state: "visible" });
        await page.evaluate(() => {
          const row = [...document.querySelectorAll(".persea-unified-select__row")].find((entry) => (entry.textContent || "").trim() !== "");
          if (!row) throw new Error("no frozen refit row to select");
          const range = document.createRange();
          range.selectNodeContents(row);
          const selection = window.getSelection();
          selection.removeAllRanges();
          selection.addRange(range);
        });
        // Copy lives on the Paste slot while the range exists.
        await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copy");
        await pasteAction().click();
        await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copied");
        await overlay.waitFor({ state: "hidden" });
        // The Copied ✓ dwell (1.2 s) ignores taps; Paste is back after it.
        await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "paste");

        // Desktop emulation does not raise an iOS keyboard, so restore the
        // same active helper posture explicitly before the trusted Paste tap.
        await helper.evaluate((node) => node.focus({ preventScroll: true }));
        await page.waitForFunction(() => document.querySelector(".xterm-helper-textarea")?.value === "\u200b");
        const beforePaste = await snapshot();
        await pasteAction().click();
        const clipboard = page.locator(".persea-clipboard");
        await clipboard.getByRole("button", { name: "Paste from device", exact: true }).click();
        await page.waitForFunction(() => document.querySelector(".persea-clipboard")?.getAttribute("aria-busy") === "false");
        const importedText = await page.evaluate(() => window.__terminal_interactionClipboard.writes.at(-1).replace(/\r\n?/g, "\n"));
        const importedRow = clipboard.locator("[data-clipboard-item]").filter({ has: page.locator(".persea-clipboard__preview", { hasText: importedText }) }).first();
        assert((await snapshot()).attachments.at(-1).inputs.length === beforePaste.attachments.at(-1).inputs.length, `${shape.name}: import sent input before explicit Paste`);
        await importedRow.getByRole("button", { name: "Paste text to terminal", exact: true }).click();
        const afterPaste = await snapshot();
        // Clipboard is a modal editor; returning to native terminal editing is
        // explicit. Preserve the repeat-delete gate after that focus transition.
        await helper.focus();
        const beforeDeleteCount = afterPaste.attachments.at(-1).inputs.length;
        const cycle = await page.evaluate(() => {
          const textarea = document.querySelector(".xterm-helper-textarea");
          if (!(textarea instanceof HTMLTextAreaElement)) throw new Error("missing xterm helper textarea");
          const afterPasteSeeded = textarea.value === "\u200b";
          let nativeCycles = 0;
          for (let index = 0; index < 12; index += 1) {
            textarea.dispatchEvent(new KeyboardEvent("keydown", {
              key: "Backspace",
              code: "Backspace",
              bubbles: true,
              cancelable: true,
            }));
            // An empty field cannot produce iOS's applied native deletion
            // sequence; the first ordinary keydown is the final event.
            if (textarea.value === "") break;
            textarea.dispatchEvent(new InputEvent("beforeinput", {
              inputType: "deleteContentBackward",
              bubbles: true,
              cancelable: true,
            }));
            textarea.value = "";
            textarea.dispatchEvent(new InputEvent("input", {
              inputType: "deleteContentBackward",
              bubbles: true,
              cancelable: false,
            }));
            nativeCycles += 1;
          }
          return { afterPasteSeeded, finalSeeded: textarea.value === "\u200b", nativeCycles };
        });
        await page.waitForTimeout(50);
        const afterDeletes = await snapshot();
        const deleteInputs = afterDeletes.attachments.at(-1).inputs.slice(beforeDeleteCount);
        assert(afterPaste.attachments.at(-1).inputs.length === beforePaste.attachments.at(-1).inputs.length + 1,
          `${shape.name}: trusted Paste did not emit exactly one input`);
        assert(cycle.afterPasteSeeded && cycle.finalSeeded && cycle.nativeCycles === 12,
          `${shape.name}: programmatic Paste did not preserve the iOS repeat sentinel: ${JSON.stringify(cycle)}`);
        assert(deleteInputs.length === 12 && deleteInputs.every((input) => input === "\u007f"),
          `${shape.name}: held Backspace was not 12 ordered PTY delete intents: ${JSON.stringify(deleteInputs)}`);
        evidence.cases.push({ shape: `${shape.name}-refit-programmatic-paste-backspace`, cycle, deleteInputs: deleteInputs.length });
        console.log(`PASTE_INPUT ${ENGINE} ${shape.name} pass cycles=${cycle.nativeCycles} deletes=${deleteInputs.length}`);
      }

      const beforeTag = await toolbarBox(); const beforeTagShell = await shellBoxes();
      assert(await page.locator(".persea-unified-session-switch").count() === 0, `${shape.name}: obsolete top-row switch exists`);
      await page.locator(".persea-unified-tag").click();
      await page.locator(".persea-unified-identity__details").waitFor({ state: "visible" });
      await page.locator(".persea-unified-identity__details .persea-session-switcher").waitFor({ state: "visible" });
      assert(await page.locator(".persea-unified-identity__session-actions").getByRole("button", { name: "Open the dashboard" }).isVisible(),
        `${shape.name}: tag popover lacks Dashboard`);
      assert(await page.locator(".persea-unified-identity__session-actions").getByRole("button", { name: /Prev|Next/ }).count() === 0,
        `${shape.name}: tag popover retained arbitrary Prev/Next`);
      assert(sameBox(beforeTag, await toolbarBox()), `${shape.name}: tag popover changed toolbar box`);
      assert(sameShell(beforeTagShell, await shellBoxes()), `${shape.name}: tag popover shifted terminal shell`);
      assert(!await page.locator(".persea-unified-identity__details input:focus").count(), `${shape.name}: tag popover stole focus`);
      await shot("tag-popover"); await page.keyboard.press("Escape");
      assert(sameShell(beforeTagShell, await shellBoxes()), `${shape.name}: closing tag shifted terminal shell`);

      await openSheet();
      const selectionLabels = await page.locator(".persea-unified-sheet__section")
        .filter({ hasText: "Selection & clipboard" }).locator(".persea-unified-sheet__label").allTextContents();
      const savedGetsBefore = (await snapshot()).snippets.get;
      await page.waitForTimeout(100);
      assert(selectionLabels.length === 0, `${shape.name}: Quick actions duplicated contextual selection controls: ${JSON.stringify(selectionLabels)}`);
      assert(await tile("Clipboard").isVisible() && !await page.locator(".persea-clipboard").isVisible()
        && await tile("Snippets").count() === 0 && await tile("Clips").count() === 0,
      `${shape.name}: task menu must expose one closed Clipboard route`);
      assert((await snapshot()).snippets.get === savedGetsBefore, `${shape.name}: closed Saved text polled`);
      const sessionLabels = await page.locator(".persea-unified-sheet__section").filter({ hasText: "Sessions" }).locator(".persea-unified-sheet__label").allTextContents();
      assert(sessionLabels[0] === "Dashboard", `${shape.name}: Dashboard is not first: ${JSON.stringify(sessionLabels)}`);
      assert(await selectAction().isVisible() && await pasteAction().isVisible(), `${shape.name}: primary contextual Select/Paste actions are absent`);
      assert(await page.locator(".persea-unified-select__strip, .persea-unified-select__copy, .persea-unified-select__copy-screen").count() === 0,
        `${shape.name}: frozen overlay retained duplicate Copy/Done controls`);
      await page.keyboard.press("Escape");
      const beforeSelectResizes = await resizes();
      const selectTile = selectAction();
      await selectTile.evaluate((node) => {
        window.__terminal_interactionLongPressEvents = [];
        for (const type of ["pointerdown", "pointermove", "pointerleave", "pointerup", "lostpointercapture", "click"]) {
          node.addEventListener(type, (event) => window.__terminal_interactionLongPressEvents.push({ type, trusted: event.isTrusted, x: event.clientX, y: event.clientY }));
        }
      });
      await longPress(selectTile); await page.waitForTimeout(100);
      const selectLongPress = await page.evaluate(() => ({ visible: !document.querySelector(".persea-unified-explainer")?.hidden, events: window.__terminal_interactionLongPressEvents }));
      assert(selectLongPress.visible, `${shape.name}: Select long-press did not explain: ${JSON.stringify(selectLongPress.events)}`);
      assert(!await page.locator(".persea-unified-select").isVisible() && await resizes() === beforeSelectResizes, `${shape.name}: Select long-press ran the primary action`);
      await page.keyboard.press("Escape"); await page.waitForTimeout(50);
      await page.evaluate(() => { document.documentElement.scrollLeft = 0; document.body.scrollLeft = 0; window.scrollTo(0, 0); });
      await page.waitForTimeout(50);
      const beforeSelectShell = await shellBoxes();
      await selectAction().click();
      const overlay = page.locator(".persea-unified-select"); await overlay.waitFor({ state: "visible" });
      assert(await selectAction().getAttribute("data-select-state") === "selecting", `${shape.name}: Select did not become Selecting…`);
      const selectState = await page.evaluate(() => ({
        inert: document.querySelector(".persea-unified-scroll")?.inert,
        hidden: document.querySelector(".persea-unified-scroll")?.getAttribute("aria-hidden"),
        rows: document.querySelectorAll(".persea-unified-select__row").length,
        bodyOverflow: getComputedStyle(document.querySelector(".persea-unified-select__body")).overflowY,
        helperActive: document.activeElement?.classList.contains("xterm-helper-textarea") === true,
        overlayText: [...document.querySelectorAll(".persea-unified-select__row")].map((row) => row.textContent || "").join("\n"),
        renderedText: document.querySelector(".xterm-rows")?.textContent || "",
      }));
      assert(selectState.inert === true && selectState.hidden === "true" && selectState.rows > 0 && /auto|scroll/.test(selectState.bodyOverflow) && !selectState.helperActive, `${shape.name}: Select isolation: ${JSON.stringify(selectState)}`);
      assert(selectState.overlayText.includes("fixture-live") && selectState.renderedText.includes("fixture-live"), `${shape.name}: frozen overlay does not match the rendered buffer marker`);
      const autoNormalMetrics = await assertFrozenMetrics("auto-normal");
      assert(sameShellSize(beforeSelectShell, await shellBoxes()), `${shape.name}: Select changed terminal shell box sizes`);
      await page.locator(".persea-unified-select__body").click({ position: { x: 8, y: 8 } });
      assert(!await page.locator(".xterm-helper-textarea:focus").count(), `${shape.name}: overlay tap focused xterm helper`);
      await page.evaluate(() => {
        const row = [...document.querySelectorAll(".persea-unified-select__row")].find((entry) => (entry.textContent || "").trim() !== "");
        if (!row) throw new Error("no frozen row to select");
        const range = document.createRange(); range.selectNodeContents(row);
        const selection = window.getSelection(); selection.removeAllRanges(); selection.addRange(range);
      });
      // the Paste slot becomes Copy while a range exists; Select
      // stays a toggle and never turns into Copy itself.
      const selectCopy = pasteAction();
      await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copy");
      assert(await selectAction().getAttribute("data-select-state") === "selecting", `${shape.name}: Select changed state when a range appeared`);
      const writesBefore = await page.evaluate(() => window.__terminal_interactionClipboard.writes.length);
      await fitsBox(selectAction(), "selecting"); await fitsBox(pasteAction(), "copy");
      const selectCopyReadyBox = await selectCopy.boundingBox();
      if (shape.name === "desktop" && refitLifecycleEnabled("c4")) await control({ snippetMutationDelayMs: 1_500 });
      await selectCopy.click();
      if (shape.name === "desktop" && refitLifecycleEnabled("c4")) {
        await page.waitForFunction((before) => window.__terminal_interactionClipboard.writes.length === before + 1, writesBefore);
        assert(!await overlay.isVisible(), `${shape.name}: local clipboard success waited for the secondary Clips mutation before exiting selection`);
      }
      await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copied");
      assert(await page.evaluate(() => window.__terminal_interactionClipboard.writes.length) === writesBefore + 1, `${shape.name}: Select Copy local write count`);
      await fitsBox(pasteAction(), "copied");
      assert(sameBox(selectCopyReadyBox, await selectCopy.boundingBox()), `${shape.name}: Copy slot box changed on success`);
      assert(!await overlay.isVisible(), `${shape.name}: successful Copy did not exit selection immediately`);
      assert(await selectAction().getAttribute("data-select-state") === "select", `${shape.name}: successful Copy did not return Select to idle`);
      await shot("copy-done");
      await page.waitForTimeout(1_300);
      assert(await pasteAction().getAttribute("data-paste-state") === "paste" && await pasteAction().getAttribute("aria-label") === "Open clipboard", `${shape.name}: Copied dwell did not return to Clipboard`);
      await fitsBox(selectAction(), "select"); await fitsBox(pasteAction(), "paste");
      // A click on an enabled control paints the tap flash
      // (class + the stylesheet's keyframe) and clears it after the dwell; a
      // disabled or aria-disabled control never flashes. The probe stops the
      // click at the target so the product's own handler does not run.
      const tapFeedback = await page.evaluate(() => {
        const probe = (button) => {
          button.addEventListener("click", (event) => { event.stopImmediatePropagation(); event.preventDefault(); }, { capture: true, once: true });
          button.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
          return { tapped: button.classList.contains("persea-tapped"), animation: getComputedStyle(button).animationName };
        };
        const select = document.querySelector(".persea-unified-select-context");
        const paste = document.querySelector(".persea-unified-toolbar-paste");
        const apply = [...document.querySelectorAll(".persea-unified-size button")].find((node) => (node.textContent || "").trim() === "Apply");
        if (!select || !paste || !apply) return null;
        const enabled = { select: probe(select), paste: probe(paste), apply: probe(apply) };
        // The enabled probes leave their flash on for 340 ms; clear it so the
        // disabled probes measure their own click only.
        paste.classList.remove("persea-tapped"); apply.classList.remove("persea-tapped");
        const disabledWas = paste.disabled; paste.disabled = true;
        const disabled = probe(paste); paste.disabled = disabledWas; paste.classList.remove("persea-tapped");
        const ariaWas = apply.getAttribute("aria-disabled"); apply.setAttribute("aria-disabled", "true");
        const ariaDisabled = probe(apply);
        if (ariaWas === null) apply.removeAttribute("aria-disabled"); else apply.setAttribute("aria-disabled", ariaWas);
        apply.classList.remove("persea-tapped");
        return { enabled, disabled, ariaDisabled };
      });
      assert(tapFeedback, `${shape.name}: tap feedback probe could not find Select, Paste and Apply`);
      for (const [name, result] of Object.entries(tapFeedback.enabled)) {
        assert(result.tapped && result.animation === "persea-tap", `${shape.name}: ${name} painted no tap feedback: ${JSON.stringify(result)}`);
      }
      assert(!tapFeedback.disabled.tapped, `${shape.name}: a disabled control painted tap feedback: ${JSON.stringify(tapFeedback.disabled)}`);
      assert(!tapFeedback.ariaDisabled.tapped, `${shape.name}: an aria-disabled control painted tap feedback: ${JSON.stringify(tapFeedback.ariaDisabled)}`);
      await page.waitForFunction(() => !document.querySelector(".persea-tapped"), undefined, { timeout: 2_000 });
      // tap-feedback-timer (): a repeat tap inside the dwell restarts its own flash;
      // the first tap's timer must not cut the second tap's feedback short.
      const repeatTap = await page.evaluate(async () => {
        const select = document.querySelector(".persea-unified-select-context");
        const tap = () => {
          select.addEventListener("click", (event) => { event.stopImmediatePropagation(); event.preventDefault(); }, { capture: true, once: true });
          select.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
        };
        const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
        tap(); await wait(300); tap(); await wait(80);
        const secondStillShown = select.classList.contains("persea-tapped");
        await wait(400);
        return { secondStillShown, clearedAfterDwell: !select.classList.contains("persea-tapped") };
      });
      assert(repeatTap.secondStillShown, `${shape.name}: the first tap timeout truncated the second tap's feedback dwell`);
      assert(repeatTap.clearedAfterDwell, `${shape.name}: the repeat tap's feedback never cleared`);
      evidence.cases.push({ shape: `${shape.name}-tap-feedback`, ...tapFeedback, repeatTap });
      if (!shape.touch) {
        // a Copy of a LIVE xterm range (real mouse drag, no frozen
        // Select) is one local write; the copied range is cleared so the slot
        // returns to Paste after the dwell, with no terminal input or resize.
        const liveRow = page.locator(".xterm-rows > div").filter({ hasText: /\S/ }).first();
        const liveBox = await liveRow.boundingBox();
        assert(liveBox, `${shape.name}: no live row to drag over`);
        const liveInputsBefore = (await snapshot()).attachments.reduce((sum, attachment) => sum + attachment.inputs.length, 0);
        const liveResizesBefore = await resizes();
        const liveWritesBefore = await page.evaluate(() => window.__terminal_interactionClipboard.writes.length);
        await page.mouse.move(liveBox.x + 2, liveBox.y + liveBox.height / 2);
        await page.mouse.down();
        await page.mouse.move(liveBox.x + 120, liveBox.y + liveBox.height / 2, { steps: 6 });
        await page.mouse.up();
        await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copy");
        await page.waitForFunction(() => (document.querySelector(".xterm-selection")?.childElementCount || 0) > 0);
        assert(await page.evaluate(() => (document.querySelector(".xterm-selection")?.childElementCount || 0) > 0), `${shape.name}: the drag did not select a live range`);
        const liveCopyBox = await pasteAction().boundingBox();
        await pasteAction().click();
        await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copied");
        assert(await page.evaluate(() => window.__terminal_interactionClipboard.writes.length) === liveWritesBefore + 1, `${shape.name}: live-range Copy local write count`);
        assert(sameBox(liveCopyBox, await pasteAction().boundingBox()), `${shape.name}: Paste slot box changed on live-range Copy`);
        await shot("live-copy-done");
        await page.waitForTimeout(1_300);
        assert(await pasteAction().getAttribute("data-paste-state") === "paste", `${shape.name}: live-range Copy never returned the slot to Paste`);
        assert(await page.evaluate(() => (document.querySelector(".xterm-selection")?.childElementCount || 0) === 0), `${shape.name}: the copied live range survived a successful Copy`);
        assert((await snapshot()).attachments.reduce((sum, attachment) => sum + attachment.inputs.length, 0) === liveInputsBefore, `${shape.name}: live-range Copy sent terminal input`);
        assert(await resizes() === liveResizesBefore, `${shape.name}: live-range Copy emitted resize`);
      }
      await control({ writeLive: "\u001b[?1049hAUTO_ALT_METRIC\r\n" });
      await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("AUTO_ALT_METRIC"));
      const autoAlternateMetrics = await openFrozenSnapshot("auto-alternate");
      const alternateView = await openView(); await alternateView.getByRole("button", { name: "Zoom out", exact: true }).click();
      // on the alternate screen Fit width is disabled up front,
      // Apply stays for rows, and a typed column change is refused without a
      // request — the broker would refuse the rebuild anyway.
      const alternateBlock = alternateView.locator(".persea-unified-size[data-surface=view]");
      const alternateFitWidth = alternateBlock.getByRole("button", { name: "Fit width", exact: true });
      assert(!await alternateFitWidth.evaluate((button) => button.disabled) && await alternateFitWidth.getAttribute("aria-disabled") === "true", `${shape.name}: unavailable Fit width must remain focusable`);
      const alternateReasonId = await alternateFitWidth.getAttribute("aria-describedby");
      assert(alternateReasonId, `${shape.name}: unavailable Fit width has no described reason`);
      const alternateReason = alternateBlock.locator(`#${alternateReasonId}`);
      const alternateReasonText = "Width cannot change while a full-screen program is running; rows can";
      const activateUnavailableWidth = async (kind) => {
        const before = { refits: (await snapshot()).counters.refits, resizes: await resizes() };
        if (kind === "tap") await alternateFitWidth.tap({ force: true });
        else if (kind === "click") await alternateFitWidth.click({ force: true });
        else { await alternateFitWidth.focus(); await page.keyboard.press(kind); }
        await alternateReason.waitFor({ state: "visible" });
        assert((await alternateReason.textContent())?.trim() === alternateReasonText, `${shape.name}: ${kind} exposed the wrong unavailable reason`);
        assert((await snapshot()).counters.refits === before.refits && await resizes() === before.resizes, `${shape.name}: ${kind} mutated unavailable Fit width`);
      };
      if (shape.touch) await activateUnavailableWidth("tap");
      await activateUnavailableWidth("click");
      await activateUnavailableWidth("Enter");
      await activateUnavailableWidth("Space");
      assert(!await alternateBlock.getByRole("button", { name: "Fit rows", exact: true }).isDisabled(), `${shape.name}: Fit rows withdrawn on the alternate screen`);
      const alternateApply = alternateBlock.getByRole("button", { name: "Apply", exact: true });
      assert(!await alternateApply.isDisabled(), `${shape.name}: Apply withdrawn on the alternate screen`);
      const alternateRefitsBefore = (await snapshot()).counters.refits;
      const alternateResizesBefore = await resizes();
      await alternateBlock.getByRole("textbox", { name: "Columns", exact: true }).fill("100");
      assert(await alternateApply.getAttribute("aria-disabled") === "true" && !await alternateApply.evaluate((button) => button.disabled), `${shape.name}: typed alternate-screen Apply is not focusable-but-unavailable`);
      await alternateApply.click({ force: true });
      await alternateReason.filter({ hasText: "full-screen program" }).waitFor({ state: "visible" });
      await delay(150);
      assert((await snapshot()).counters.refits === alternateRefitsBefore && await resizes() === alternateResizesBefore, `${shape.name}: alternate-screen Apply reached the broker`);
      await page.keyboard.press("Escape");
      await page.waitForTimeout(100);
      const explicitAlternateMetrics = await openFrozenSnapshot("explicit-alternate");
      await control({ writeLive: "\u001b[?1049l" });
      await page.waitForFunction(() => !document.querySelector(".xterm-rows")?.textContent?.includes("AUTO_ALT_METRIC"));
      const normalView = await openView();
      const normalFitWidth = normalView.locator(".persea-unified-size[data-surface=view]").getByRole("button", { name: "Fit width", exact: true });
      assert(!await normalFitWidth.isDisabled() && await normalFitWidth.getAttribute("aria-disabled") === "false", `${shape.name}: Fit width not restored after the alternate screen closed: ${await normalFitWidth.getAttribute("title")}`);
      await page.keyboard.press("Escape");
      await page.waitForTimeout(100);
      const explicitNormalMetrics = await openFrozenSnapshot("explicit-normal");
      evidence.cases.push({ shape: `${shape.name}-frozen-metrics`, autoNormalMetrics, autoAlternateMetrics, explicitAlternateMetrics, explicitNormalMetrics });
      const afterSelectShell = await shellBoxes();
      assert(sameShellSize(beforeSelectShell, afterSelectShell), `${shape.name}: Select exit changed terminal shell box sizes: before=${JSON.stringify(beforeSelectShell)} after=${JSON.stringify(afterSelectShell)}`);
      assert(await resizes() === beforeSelectResizes, `${shape.name}: Select emitted resize`);

      await selectAction().click(); await overlay.waitFor({ state: "visible" });
      await page.evaluate(() => {
        const row = [...document.querySelectorAll(".persea-unified-select__row")].find((entry) => (entry.textContent || "").trim() !== "");
        if (!row) throw new Error("no frozen failure row");
        const range = document.createRange(); range.selectNodeContents(row);
        const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range);
      });
      await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copy");
      await page.evaluate(() => { window.__terminal_interactionClipboard.fail = true; });
      const screenCopy = pasteAction(); const screenCopyReadyBox = await screenCopy.boundingBox(); await screenCopy.click();
      await page.waitForFunction(() => document.querySelector(".persea-unified-select__status")?.textContent?.includes("Copy failed"));
      assert(await overlay.isVisible() && await screenCopy.getAttribute("data-paste-state") === "failed" && await selectAction().getAttribute("data-select-state") === "selecting", `${shape.name}: failed Copy exited or lost its range`);
      assert(sameBox(screenCopyReadyBox, await screenCopy.boundingBox()), `${shape.name}: Copy slot box changed on failure`);
      await fitsBox(pasteAction(), "failed");
      await shot("select-failed");
      await page.evaluate(() => { window.__terminal_interactionClipboard.fail = false; });
      // Select is a toggle — a second tap leaves selection with the
      // range still in place, and the Paste slot returns to Paste.
      await selectAction().click(); await overlay.waitFor({ state: "hidden" });
      assert(await selectAction().getAttribute("data-select-state") === "select", `${shape.name}: Select toggle did not leave selection`);
      await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "paste");

      const r4HardRows = Array.from({ length: 160 }, (_, index) => `R4H${String(index).padStart(3, "0")}`).join("\r\n");
      const r4SoftRows = `R4SOFTA${"a".repeat(73)}R4SOFTB${"b".repeat(20)}`;
      await control({ writeLive: `\r\n${r4HardRows}\r\n${r4SoftRows}\r\nR4AFTER\r\n` });
      await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("R4AFTER"));
      const r4Live = await page.evaluate(() => ({
        scrollTop: document.querySelector(".xterm-viewport")?.scrollTop ?? 0,
        firstRow: document.querySelector(".xterm-rows > div")?.textContent?.trimEnd() ?? "",
      }));
      await selectAction().click(); await overlay.waitFor({ state: "visible" });
      const r4Geometry = await page.evaluate(({ liveFirstRow, liveScrollTop }) => {
        const body = document.querySelector(".persea-unified-select__body");
        const rows = [...document.querySelectorAll(".persea-unified-select__row")];
        if (!body || rows.length === 0) throw new Error("missing frozen R4 geometry owners");
        const findRow = (prefix) => rows.find((row) => (row.textContent || "").startsWith(prefix));
        const hardA = findRow("R4H000"); const hardB = findRow("R4H001");
        const softA = findRow("R4SOFTA"); const softB = findRow("R4SOFTB");
        if (!hardA || !hardB || !softA || !softB) throw new Error("missing frozen R4 sentinel rows");
        const rowHeight = hardA.getBoundingClientRect().height;
        const bodyRect = body.getBoundingClientRect();
        const contentTop = bodyRect.top + Number.parseFloat(getComputedStyle(body).paddingTop);
        const liveFirstIndex = rows.findIndex((row) => (row.textContent || "").trimEnd() === liveFirstRow);
        const maxScrollTop = body.scrollHeight - body.clientHeight;
        const expectedScrollTop = Math.min(liveFirstIndex * rowHeight, maxScrollTop);
        const liveFirstTop = liveFirstIndex >= 0 ? rows[liveFirstIndex].getBoundingClientRect().top : Number.NaN;
        const expectedLiveFirstTop = contentTop + liveFirstIndex * rowHeight - expectedScrollTop;
        return {
          rowHeight,
          hardDelta: hardB.getBoundingClientRect().top - hardA.getBoundingClientRect().top,
          softDelta: softB.getBoundingClientRect().top - softA.getBoundingClientRect().top,
          liveScrollTop,
          frozenScrollTop: body.scrollTop,
          liveFirstIndex,
          maxScrollTop,
          expectedScrollTop,
          liveFirstTop,
          expectedLiveFirstTop,
        };
      }, { liveFirstRow: r4Live.firstRow, liveScrollTop: r4Live.scrollTop });
      assert(Math.abs(r4Geometry.hardDelta - r4Geometry.rowHeight) <= 0.1,
        `${shape.name}: consecutive hard rows did not advance one captured row: ${JSON.stringify(r4Geometry)}`);
      assert(Math.abs(r4Geometry.softDelta - r4Geometry.rowHeight) <= 0.1,
        `${shape.name}: soft-wrapped rows did not advance one captured row: ${JSON.stringify(r4Geometry)}`);
      assert(r4Geometry.frozenScrollTop > 0 && r4Geometry.liveFirstIndex >= 0
        && Math.abs(r4Geometry.frozenScrollTop - r4Geometry.expectedScrollTop) <= 0.2
        && Math.abs(r4Geometry.liveFirstTop - r4Geometry.expectedLiveFirstTop) <= 0.2,
      `${shape.name}: nonzero frozen viewport did not preserve the live top row: ${JSON.stringify(r4Geometry)}`);
      await selectAction().click(); await overlay.waitFor({ state: "hidden" });

      let view = await openView();
      const fit = view.getByRole("button", { name: "Fit rows", exact: false }); const beforeLong = await resizes();
      await longPress(fit); await page.locator(".persea-unified-explainer").waitFor({ state: "visible" });
      assert(await resizes() === beforeLong, `${shape.name}: Fit long-press resized`);
      await shot("fit-explainer"); await page.keyboard.press("Escape");
      view = await openView();
      await longPress(fit, true);
      assert(!await page.locator(".persea-unified-explainer").isVisible() && await resizes() === beforeLong, `${shape.name}: moved long-press acted`);
      await page.keyboard.press("Escape");

	  const refitColumns = 96;
	  view = await openView();
	  const refitForm = view.locator(".persea-unified-size[data-surface=view]");
	  const columns = refitForm.getByRole("textbox", { name: "Columns", exact: true });
	  // the single Apply performs the width refit when the typed
	  // columns differ from the committed ones (rows untouched → no rows sent).
	  const refit = refitForm.getByRole("button", { name: "Apply", exact: true });
	  const beforeRefit = await snapshot();
	  const beforeRefitResizes = await resizes();
	  const helper = page.locator(".xterm-helper-textarea");
	  await helper.evaluate((node) => node.focus({ preventScroll: true }));
	  await page.keyboard.type("REFIT_PRE;");
	  await page.waitForFunction(() => document.activeElement?.classList.contains("xterm-helper-textarea"));
	  await control({ holdRefitMs: 350 });
	  await refit.evaluate((button) => {
	    window.__refitRefitEvents = [];
	    for (const type of ["pointerdown", "mousedown", "pointerup", "click"])
	      button.addEventListener(type, (event) => window.__refitRefitEvents.push({ type, trusted: event.isTrusted, detail: "detail" in event ? event.detail : null, disabled: button.disabled }));
	  });
	  await columns.fill(String(refitColumns));
	  await columns.press("Enter");
	  assert((await snapshot()).counters.refits === beforeRefit.counters.refits, `${shape.name}: Enter emitted a width refit`);
	  await refit.evaluate((button) => button.dispatchEvent(new MouseEvent("click", { bubbles: true })));
	  assert((await snapshot()).counters.refits === beforeRefit.counters.refits, `${shape.name}: untrusted click emitted a width refit`);
	  await refit.click();
	  try {
	    // The block's first buttons (Fit rows / Fit width) are also aria-disabled
	    // while a refit is pending; the Refitting state lives on Apply.
	    await page.waitForFunction(() => [...document.querySelectorAll(".persea-unified-size[data-surface=view] button")].some((button) => button.getAttribute("aria-disabled") === "true" && (button.textContent || "").includes("Refitting")), undefined, { timeout: 10_000 });
	  } catch (error) {
	    const diagnostic = await page.evaluate(() => ({
	      apply: [...document.querySelectorAll(".persea-unified-size[data-surface=view] button")].map((button) => ({ text: button.textContent, disabled: button.disabled, ariaDisabled: button.getAttribute("aria-disabled") })),
	      inputs: [...document.querySelectorAll(".persea-unified-size[data-surface=view] input")].map((input) => ({ label: input.getAttribute("aria-label"), value: input.value, disabled: input.disabled })),
	      refusal: document.querySelector(".persea-unified-refusal")?.textContent,
	      toast: document.querySelector(".persea-unified-toast:not([hidden])")?.textContent,
	      geometry: document.querySelector(".persea-unified-view-disclosure")?.textContent,
	      events: window.__refitRefitEvents,
	    }));
	    throw new Error(`${shape.name}: Apply did not enter the Refitting state: ${JSON.stringify({ diagnostic, fixture: (await snapshot()).counters })}; ${error}`);
	  }
	  await helper.evaluate((node) => node.focus({ preventScroll: true }));
	  await page.keyboard.type("REFIT_DURING;");
	  // Predecessor replay during refit: replaying the predecessor tuple while the HTTP refit is
	  // pending must not open the exact-operation seal. Only the captured
	  // successor COMMIT owns that transition.
	  if (shape.name === "desktop" && refitLifecycleEnabled("c3")) {
	    await control({ recommitSession: "A" });
	    await page.waitForTimeout(50);
	    await helper.evaluate((node) => node.focus({ preventScroll: true }));
	    await page.keyboard.type("REFIT_WRONG_COMMIT;");
		for (let attempt = 0; attempt < 100; attempt += 1) {
		  if ((await snapshot()).attachments.some((entry) => entry.closeReason === "generation_refit")) break;
		  await delay(20);
		}
		await page.waitForTimeout(300);
		await helper.evaluate((node) => node.focus({ preventScroll: true }));
		await page.keyboard.type("REFIT_AFTER_CLOSE;");
	  }
	  try {
	    await page.waitForFunction((label) => document.querySelector(".persea-unified-view-disclosure")?.textContent === label, `${refitColumns}×24`, { timeout: 10_000 });
	  } catch (error) {
	    const diagnostic = await page.evaluate(() => ({
	      geometry: document.querySelector(".persea-unified-view-disclosure")?.textContent,
	      status: document.querySelector(".persea-unified-connection-status")?.textContent,
	      refusal: document.querySelector(".persea-unified-refusal")?.textContent,
	      events: window.__refitRefitEvents,
	    }));
	    const fixtureState = await snapshot();
	    throw new Error(`${shape.name}: width refit did not publish successor geometry: ${JSON.stringify({ diagnostic, fixtureState })}; ${error}`);
	  }
	  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
	  await helper.evaluate((node) => node.focus({ preventScroll: true }));
	  await page.keyboard.type("REFIT_POST;");
	  for (let attempt = 0; attempt < 100 && !(await inputTranscript()).includes("REFIT_POST;"); attempt += 1) await delay(20);
	  const afterRefit = await snapshot();
	  assert(afterRefit.counters.refits === beforeRefit.counters.refits + 1, `${shape.name}: trusted refit did not issue exactly once`);
	  assert(afterRefit.attachments.filter((entry) => entry.closeReason === "generation_refit").length === 1, `${shape.name}: refit did not close exactly one predecessor`);
	  const refitOperation = afterRefit.refitOperations.at(-1);
	  assert(refitOperation?.successor && refitOperation.successor !== refitOperation.source, `${shape.name}: broker did not name one distinct successor source: ${JSON.stringify(refitOperation)}`);
	  assert(await resizes() === beforeRefitResizes, `${shape.name}: width refit emitted an attachment RESIZE_REQUEST`);
	  const refitInputs = await inputTranscript();
	  assert(refitInputs.includes("REFIT_PRE;") && refitInputs.includes("REFIT_POST;") && !refitInputs.includes("REFIT_DURING;") && !refitInputs.includes("REFIT_WRONG_COMMIT;"), `${shape.name}: refit input seal violated ordering/drop contract: ${JSON.stringify(refitInputs)}`);
	  const droppedRefitBytes = shape.name === "desktop" && refitLifecycleEnabled("c3") ? 50 : 13;
	  const refitDropToast = page.locator(".persea-unified-toast:not([hidden])");
	  const actualRefitDropToast = await refitDropToast.textContent();
	  assert(actualRefitDropToast === `${droppedRefitBytes} input bytes were not sent during width refit`, `${shape.name}: successful refit did not settle its dropped input exactly once: ${JSON.stringify(actualRefitDropToast)}`);
	  if (!shape.touch) {
	    view = await openView();
	    const successorFit = view.getByRole("button", { name: "Fit rows", exact: false });
	    const beforeSuccessorFit = await resizes();
	    await successorFit.click();
	    for (let attempt = 0; attempt < 100 && await resizes() !== beforeSuccessorFit + 1; attempt += 1) await delay(20);
	    assert(await resizes() === beforeSuccessorFit + 1, `${shape.name}: post-refit Fit did not emit exactly once`);
	    const successorState = await snapshot();
	    const requests = successorState.attachments.flatMap((entry) => entry.resizeRequests || []);
	    assert(requests.length > 0 && requests.at(-1).columns === refitColumns, `${shape.name}: post-refit Fit did not preserve successor columns: ${JSON.stringify(requests)}`);
	  }

      let rowEvidence;
      if (shape.touch && shape.viewport.width < 720) {
        rowEvidence = await page.locator(".persea-unified-toolbar__controls").evaluate((node) => {
          const toolbar = node.closest(".persea-unified-toolbar");
          const controls = [...node.querySelectorAll("button")].map((button) => { const box = button.getBoundingClientRect(); return { label: button.getAttribute("aria-label") || button.textContent, width: box.width, height: box.height, left: box.left, right: box.right, top: box.top }; }).filter((entry) => entry.width > 0 && entry.height > 0);
          const tag = toolbar.querySelector(".persea-unified-tag").getBoundingClientRect();
          return { controls, tag: { width: tag.width, height: tag.height }, readoutVisible: getComputedStyle(toolbar.querySelector(".persea-unified-geometry:not(.persea-unified-geometry--phone)")).display !== "none" };
        });
        assert(rowEvidence.tag.width >= 64 && rowEvidence.controls.length === 5 && rowEvidence.controls.every((entry) => entry.width >= 43.5 && entry.height >= 43.5), `phone row budget: ${JSON.stringify(rowEvidence)}`);
        assert(rowEvidence.controls.every((entry, index, list) => index === 0 || entry.left - list[index - 1].right <= 4.2), `phone gaps exceed 4px: ${JSON.stringify(rowEvidence)}`);
        assert(rowEvidence.controls.every((entry) => Math.abs(entry.top - rowEvidence.controls[0].top) < 0.2), `phone controls wrapped`);
        assert(rowEvidence.readoutVisible === true, "phone row hides the committed geometry disclosure");
        const disclosure = page.locator(".persea-unified-view-disclosure"); const beforeView = await toolbarBox();
        await disclosure.click(); const viewPopover = page.locator(".persea-unified-view-popover"); await viewPopover.waitFor({ state: "visible" });
        assert(sameBox(beforeView, await toolbarBox()), "View disclosure changed toolbar box");
        assert(await viewPopover.getByRole("button", { name: "Zoom out" }).isVisible() && await viewPopover.locator(".persea-unified-size[data-surface=view]").isVisible(), "View popover lacks zoom/size");
        // theme and composer text live on the dashboard; the View
        // popover carries no preference picker.
        assert(await viewPopover.locator(".persea-unified-preference").count() === 0, `${shape.name}: a preference picker leaked into the View popover`);
        await shot("view-popover");
        const form = viewPopover.locator(".persea-unified-size[data-surface=view]"); const rows = form.getByRole("textbox", { name: "Rows", exact: true }); const apply = form.getByRole("button", { name: "Apply", exact: true });
        const beforeInput = await resizes(); await rows.fill("40"); await rows.press("Enter");
        assert(await resizes() === beforeInput, "input/change/Enter emitted resize");
        await apply.evaluate((button) => button.dispatchEvent(new MouseEvent("click", { bubbles: true })));
        assert(await resizes() === beforeInput, "untrusted Apply emitted resize");
		await apply.click(); await page.waitForFunction((label) => document.querySelector(".persea-unified-view-disclosure")?.textContent === label, `${refitColumns}×40`);
        assert(await resizes() === beforeInput + 1, "trusted rows-only Apply did not emit once");
		const widthRequests = (await snapshot()).attachments.flatMap((entry) => entry.resizeRequests || []);
		assert(widthRequests.length > 0 && widthRequests.at(-1).columns === refitColumns && widthRequests.at(-1).rows === 40, `rows-only Apply drifted successor columns: ${JSON.stringify(widthRequests)}`);
        await rows.fill("0"); const beforeInvalid = await resizes(); await apply.click();
        await page.locator(".persea-unified-refusal:not([hidden])").waitFor({ state: "visible" });
        assert(await resizes() === beforeInvalid, "out-of-policy rows emitted a resize");
        await control({ holdResizeMs: 300 }); await rows.fill("41"); await apply.click();
        const sealState = await form.evaluate((node) => [...node.querySelectorAll("button")].map((button) => ({ text: button.textContent, disabled: button.disabled, ariaDisabled: button.getAttribute("aria-disabled") })));
        assert(sealState.some((button) => button.text === "Applying…" && !button.disabled && button.ariaDisabled === "true")
          && sealState.some((button) => button.text === "↕ Fit rows" && !button.disabled && button.ariaDisabled === "true"), `${shape.name}: rows-only Apply and Fit do not share the focusable pending seal: ${JSON.stringify(sealState)}`);
		await page.waitForFunction((label) => document.querySelector(".persea-unified-view-disclosure")?.textContent === label, `${refitColumns}×41`);
        assert(await resizes() === beforeInvalid + 1, "pending rows-only Apply emitted more than once");
        await disclosure.click(); await longPress(disclosure); await page.locator(".persea-unified-explainer").waitFor({ state: "visible" });
        assert(!await viewPopover.isVisible(), "View long-press also opened its primary popover"); await page.keyboard.press("Escape");
      }

      await openSheet(); assert(await tile("Help").isVisible(), `${shape.name}: Help absent`); await tile("Help").click();
      const help = page.locator(".persea-unified-explainer"); await help.waitFor({ state: "visible" });
      const helpTerms = await help.locator("dt").allTextContents();
      assert(JSON.stringify(helpTerms) === JSON.stringify(["Copy", "Select", "Paste / Copy", "Fit rows / Fit width", "View and size", "Terminal size", "Sessions"]), `${shape.name}: Help incomplete: ${JSON.stringify(helpTerms)}`);
      assert(await help.getByRole("button", { name: "Copy the recent input trace for a bug report" }).isVisible(), `${shape.name}: Help lacks the input trace copy`);
      // the trace is one local clipboard write of valid JSON with
      // the recorded event sequence — never a network request.
      const traceWritesBefore = await page.evaluate(() => window.__terminal_interactionClipboard.writes.length);
      const traceRequestsBefore = httpErrors.length + (await snapshot()).attachments.length;
      await help.getByRole("button", { name: "Copy the recent input trace for a bug report" }).click();
      await page.waitForFunction((before) => window.__terminal_interactionClipboard.writes.length === before + 1, traceWritesBefore);
      const trace = JSON.parse(await page.evaluate(() => window.__terminal_interactionClipboard.writes.at(-1)));
      assert(trace.version === 1 && typeof trace.userAgent === "string" && Array.isArray(trace.entries) && trace.entries.length > 0 && trace.entries.length <= 300
        && trace.entries.every((entry) => typeof entry.t === "number" && typeof entry.kind === "string" && entry.router && typeof entry.router.active === "boolean")
        && trace.entries.some((entry) => entry.kind === "send"),
        `${shape.name}: input trace malformed: ${JSON.stringify(trace).slice(0, 400)}`);
      assert(httpErrors.length + (await snapshot()).attachments.length === traceRequestsBefore, `${shape.name}: copying the trace touched the network`);
      await shot("help"); await page.keyboard.press("Escape");
      // a software-keyboard / visual-viewport transition
      // while the View popover is open refreshes the Fit width affordance
      // (activation was already fail-closed; the control must say so), and
      // the target returns unchanged once the keyboard goes.
      view = await openView();
      const guardBlock = view.locator(".persea-unified-size[data-surface=view]");
      const guardFit = guardBlock.getByRole("button", { name: "Fit width", exact: true });
      const guardBefore = { disabled: await guardFit.isDisabled(), aria: await guardFit.getAttribute("aria-disabled"), title: await guardFit.getAttribute("title") };
      assert(!guardBefore.disabled && guardBefore.aria === "false" && /^Rebuild this session at \d+ columns/.test(guardBefore.title || ""), `${shape.name}: Fit width not offered before the keyboard probe: ${JSON.stringify(guardBefore)}`);
      const guardRefits = (await snapshot()).counters.refits;
      const guardResizes = await resizes();
      await page.evaluate(() => window.__terminal_layoutViewport.set({ height: Math.max(120, Math.round(window.innerHeight * 0.55)) }));
      const fitWidthState = () => [...document.querySelectorAll(".persea-unified-size[data-surface=view] button")]
        .filter((button) => (button.textContent || "").includes("Fit width"))
        .map((button) => ({ disabled: button.disabled, aria: button.getAttribute("aria-disabled"), title: button.title }))[0];
      await page.waitForFunction(() => {
        const button = [...document.querySelectorAll(".persea-unified-size[data-surface=view] button")]
          .find((candidate) => (candidate.textContent || "").includes("Fit width"));
        return button && !button.disabled && button.getAttribute("aria-disabled") === "true";
      }, undefined, { timeout: 5_000 })
        .catch(async (error) => { throw new Error(`${shape.name}: Fit width stayed offered through a keyboard rise: ${JSON.stringify(await page.evaluate(fitWidthState))}; ${error}`); });
      const guardAfter = { disabled: await guardFit.evaluate((button) => button.disabled), aria: await guardFit.getAttribute("aria-disabled"), title: await guardFit.getAttribute("title") };
      assert(guardAfter.title === "Close the software keyboard before fitting to the visible area.", `${shape.name}: keyboard-guarded Fit width title: ${JSON.stringify(guardAfter)}`);
      // Apply carries a typed size — it stays available while the
      // keyboard is up, and a typed rows change during the rise is exactly one
      // live RESIZE_REQUEST; the measured fits stay withdrawn.
      const guardApply = guardBlock.getByRole("button", { name: "Apply", exact: true });
      assert(!await guardApply.isDisabled() && await guardApply.getAttribute("aria-disabled") === "false", `${shape.name}: Apply withdrawn through a keyboard rise`);
      assert(await guardBlock.getByRole("button", { name: "Fit rows", exact: true }).isDisabled(), `${shape.name}: Fit rows offered through a keyboard rise`);
      await delay(150);
      assert((await snapshot()).counters.refits === guardRefits && await resizes() === guardResizes, `${shape.name}: the keyboard probe produced refit or resize traffic`);
      const guardReadout = (await page.locator(".persea-unified-view-disclosure").textContent() || "").trim();
      const [guardColumns, guardRows] = guardReadout.split("×").map(Number);
      const typedRows = guardRows + 2;
      await guardBlock.getByRole("textbox", { name: "Rows", exact: true }).fill(String(typedRows));
      await guardApply.click();
      await page.waitForFunction((label) => (document.querySelector(".persea-unified-view-disclosure")?.textContent || "").trim() === label, `${guardColumns}×${typedRows}`, { timeout: 5_000 })
        .catch(async (error) => { throw new Error(`${shape.name}: a typed rows Apply during the keyboard rise did not commit ${guardColumns}×${typedRows}: readout=${await page.locator(".persea-unified-view-disclosure").textContent()} refusal=${await page.locator(".persea-unified-refusal").textContent().catch(() => "")}; ${error}`); });
      await delay(150);
      assert(await resizes() === guardResizes + 1 && (await snapshot()).counters.refits === guardRefits, `${shape.name}: a typed rows Apply during the keyboard rise did not issue exactly one RESIZE_REQUEST`);
      const guardResizesAfterApply = await resizes();
      await page.evaluate(() => window.__terminal_layoutViewport.set({ height: null }));
      await page.waitForFunction((source) => { const state = new Function(`return (${source})()`)(); return state && !state.disabled && state.aria === "false"; }, fitWidthState.toString(), { timeout: 5_000 })
        .catch(async (error) => { throw new Error(`${shape.name}: Fit width not restored after the keyboard went: ${JSON.stringify(await page.evaluate((source) => new Function(`return (${source})()`)(), fitWidthState.toString()))}; ${error}`); });
      assert(await guardFit.getAttribute("title") === guardBefore.title, `${shape.name}: Fit width target changed across the keyboard probe: ${await guardFit.getAttribute("title")} vs ${guardBefore.title}`);
      assert((await snapshot()).counters.refits === guardRefits && await resizes() === guardResizesAfterApply, `${shape.name}: the keyboard release produced refit or resize traffic`);
      evidence.cases.push({ shape: `${shape.name}-keyboard-guard`, before: guardBefore, guarded: guardAfter });
      await page.keyboard.press("Escape"); await page.waitForTimeout(100);
      // Fit width is the measured-columns rebuild; once the width
      // fits, the control withdraws itself ("Width already fits") instead of
      // offering a no-op refit. The measured width is read from the control's
      // own title, so the pin holds on every shape.
      view = await openView();
      const fitWidthButton = view.locator(".persea-unified-size[data-surface=view]").getByRole("button", { name: "Fit width", exact: true });
      const fitWidthTitle = await fitWidthButton.getAttribute("title");
      const fitWidthColumns = Number((fitWidthTitle || "").match(/at (\d+) columns/)?.[1]);
      assert(!await fitWidthButton.isDisabled() && Number.isInteger(fitWidthColumns) && fitWidthColumns >= 20, `${shape.name}: Fit width is not offering a measured width: ${fitWidthTitle}`);
      // Rows are the successor's own (the fixture rebuilds at its session
      // rows, not the attachment's last live fit), so only the columns are
      // the measured contract here.
      const beforeFitWidth = (await snapshot()).counters.refits;
      const beforeFitWidthResizes = await resizes();
      await fitWidthButton.click();
      try {
        await page.waitForFunction((prefix) => (document.querySelector(".persea-unified-view-disclosure")?.textContent || "").startsWith(prefix), `${fitWidthColumns}×`, { timeout: 10_000 });
      } catch (error) {
        const diagnostic = await page.evaluate(() => ({
          geometry: document.querySelector(".persea-unified-view-disclosure")?.textContent,
          status: document.querySelector(".persea-unified-connection-status")?.textContent,
          refusal: document.querySelector(".persea-unified-refusal")?.textContent,
          toast: document.querySelector(".persea-unified-toast:not([hidden])")?.textContent,
          buttons: [...document.querySelectorAll(".persea-unified-size[data-surface=view] button")].map((button) => ({ text: button.textContent, title: button.title, disabled: button.disabled })),
        }));
        throw new Error(`${shape.name}: Fit width did not publish the measured columns ${fitWidthColumns}: ${JSON.stringify({ diagnostic, fixture: (await snapshot()).counters })}; ${error}`);
      }
      await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
      assert((await snapshot()).counters.refits === beforeFitWidth + 1 && await resizes() === beforeFitWidthResizes, `${shape.name}: Fit width did not issue exactly one refit`);
      await page.waitForTimeout(300);
      if (await page.locator(".persea-unified-view-popover").isVisible()) { await page.keyboard.press("Escape"); await page.waitForTimeout(100); }
      view = await openView();
      const fittedWidth = view.locator(".persea-unified-size[data-surface=view]").getByRole("button", { name: "Fit width", exact: true });
      const fittedTitle = await fittedWidth.getAttribute("title");
      assert(await fittedWidth.isDisabled() && await fittedWidth.getAttribute("aria-disabled") === "true" && (fittedTitle || "").startsWith("Width already fits"), `${shape.name}: Fit width still offered when the width already fits: ${fittedTitle}`);
      assert(!await view.locator(".persea-unified-size[data-surface=view]").getByRole("button", { name: "Apply", exact: true }).isDisabled(), `${shape.name}: Apply withdrawn when the width already fits`);
      evidence.cases.push({ shape: `${shape.name}-fit-width`, offered: fitWidthTitle, fitted: fittedTitle, columns: fitWidthColumns });
      await page.keyboard.press("Escape"); await page.waitForTimeout(100);
	      assert(errors.filter((entry) => !/Refused to apply a stylesheet/.test(entry) && !/Failed to load resource: the server responded with a status of 403/.test(entry)).length === 0, `${shape.name}: browser errors: ${JSON.stringify(errors)}`);
      const expectedWebKitClipRefusals = shape.name === "phone-390" ? 3 : 2;
      const expectedWebKitClipRefusal = ENGINE === "webkit" && httpErrors.length === expectedWebKitClipRefusals
        && httpErrors.every((entry) => entry.status === 403 && new URL(entry.url).pathname === "/api/snippets");
      assert(httpErrors.length === 0 || expectedWebKitClipRefusal, `${shape.name}: HTTP errors: ${JSON.stringify(httpErrors)}`);
      evidence.cases.push({ shape: shape.name, row: rowEvidence, selectState, r4Geometry, errors, httpErrors }); await context.close();
      console.log(`TERMINAL_INTERACTION_PHASE ${ENGINE} ${shape.name} pass`);
    }
    fs.writeFileSync(path.join(EVIDENCE, `terminal_interaction-${ENGINE}.json`), JSON.stringify(evidence, null, 2));
    console.log(`terminal interaction ${ENGINE}: PASS (${evidence.cases.length} postures)`);
  } finally {
    await browser.close();
    await Promise.race([fixture.close(), delay(3_000)]);
  }
}

main().catch((error) => { console.error(error && error.stack || error); process.exitCode = 1; });
