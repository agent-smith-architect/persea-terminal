"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const { startClipboardFixture } = require("./clipboard_fixture.cjs");
const playwright = require("playwright");

const engine = process.env.PERSEA_SELECTION_ENGINE || "chromium";
const shapes = [
  { name: "desktop", width: 1180, height: 820, touch: false },
  { name: "small-phone", width: 360, height: 780, touch: true },
  { name: "phone", width: 390, height: 844, touch: true },
  { name: "large-phone", width: 430, height: 932, touch: true },
  { name: "landscape", width: 844, height: 390, touch: true },
].filter(shape => !process.env.PERSEA_SELECTION_SHAPE || shape.name === process.env.PERSEA_SELECTION_SHAPE);

async function main() {
  const browser = await playwright[engine].launch({ headless: true,
    ...(engine === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
  });
  const fixture = await startClipboardFixture(path.resolve(__dirname, ".."), { tls: true });
  const api = await playwright.request.newContext({ baseURL: fixture.origin, ignoreHTTPSErrors: true });
  const control = async data => (await api.post("/__fixture/control", { data })).json();
  const snapshot = async () => (await api.get("/__fixture/control")).json();
  try {
    for (const shape of shapes) {
      await control({ reset: true });
      const inventory = await (await api.get("/api/inventory")).json();
      const session = inventory.realms[0].servers[0].sessions[0];
      const context = await browser.newContext({ viewport: { width: shape.width, height: shape.height },
        isMobile: shape.touch, hasTouch: shape.touch, ignoreHTTPSErrors: true });
      const page = await context.newPage();
      const consoleMessages = [];
      page.on("console", message => consoleMessages.push(`${message.type()}: ${message.text()}`));
      page.on("pageerror", error => consoleMessages.push(String(error)));
      try {
        const hash = new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000",
          name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" });
        await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${hash}`);
        await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
        // Mouse reporting makes any click leaking into xterm visible on the wire.
        await control({ writeLive: "\u001b[?1000h\u001b[?1006h\r\nSelect these words\r\n\r\n\r\n$ " });
        await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("Select these words"));
        const select = page.locator(".persea-unified-select-context");
        const overlay = page.locator(".persea-unified-select");
        const enter = async () => { await select.click(); await overlay.waitFor({ state: "visible" }); };
        const tap = async point => shape.touch ? page.touchscreen.tap(point.x, point.y) : page.mouse.click(point.x, point.y);
        const focused = () => page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea") === true);
        const wire = async () => (await snapshot()).attachments.map(a => ({ inputs: a.inputs, resizes: a.resizes }));
        const before = await wire();
        await page.evaluate(() => document.activeElement?.blur());
        await enter();
        await page.evaluate(() => {
          window.__selectionInputEvents = [];
          for (const type of ["pointerdown", "pointerup", "touchend", "click", "focusin", "focusout"]) {
            document.addEventListener(type, event => {
              window.__selectionInputEvents.push({ type, target: event.target?.className,
                active: document.activeElement?.className, prevented: event.defaultPrevented });
              if (window.__selectionInputEvents.length > 30) window.__selectionInputEvents.shift();
            });
          }
        });

        // Pick actual rendered coordinates: no guessed font/cell dimensions.
        const points = async () => page.evaluate(() => {
          const body = document.querySelector(".persea-unified-select__body");
          const row = [...body.querySelectorAll(".persea-unified-select__row")]
            .find(node => node.textContent.startsWith("Select these words"));
          if (!row) throw new Error("missing selection fixture row");
          const range = document.createRange();
          range.setStart(row.firstChild.firstChild || row.firstChild, 0);
          range.setEnd(row.firstChild.firstChild || row.firstChild, 6);
          const rect = range.getBoundingClientRect();
          const bounds = body.getBoundingClientRect();
          const empty = row.nextElementSibling.nextElementSibling.getBoundingClientRect();
          return { text: { x: rect.left + 3, y: rect.top + rect.height / 2 },
            blank: { x: bounds.right - 35, y: empty.top + empty.height / 2 },
            handle: { x: rect.right + 8, y: rect.bottom + 5 } };
        });
        let point = await points();
        await tap(point.blank);
        await overlay.waitFor({ state: "hidden", timeout: 1500 });
        assert.equal(await select.getAttribute("aria-pressed"), "false", "tap did not clear Select state");
        assert.equal(await focused(), true, `input tap did not keep terminal focus: ${JSON.stringify(await page.evaluate(() => window.__selectionInputEvents))}`);
        assert.deepEqual(await wire(), before, "input tap emitted terminal input or resized");
        await page.keyboard.type("typed-after-selection");
        // Keyboard dispatch completes before the WebSocket receiver records all
        // bytes. Wait for the exact wire condition, retaining the equality check.
        let afterInput;
        const inputDeadline = Date.now() + 10_000;
        do {
          afterInput = await wire();
          if (afterInput.at(-1).inputs.slice(before.at(-1).inputs.length).join("") === "typed-after-selection") break;
          await page.waitForTimeout(20);
        } while (Date.now() < inputDeadline);
        assert.equal(afterInput.at(-1).inputs.slice(before.at(-1).inputs.length).join(""), "typed-after-selection");

        await enter();
        point = await points();
        await tap(point.text);
        assert.equal(await overlay.isVisible(), true, "text tap stole native selection gestures");
        assert.equal(await focused(), false, "text tap opened input while selecting");
        await page.mouse.dblclick(point.text.x, point.text.y);
        assert.equal(await overlay.isVisible(), true, "double-click selection resumed input");
        assert.equal(await page.evaluate(() => String(getSelection()).trim()), "Select", "native word selection stopped working");

        // Synthetic activations cannot dismiss selection. Cancellation and a
        // changed snapshot also invalidate an otherwise completed tap.
        await overlay.evaluate(node => {
          const row = node.querySelector(".persea-unified-select__row");
          row.dispatchEvent(new MouseEvent("click", { bubbles: true }));
        });
        assert.equal(await overlay.isVisible(), true, "synthetic click resumed input");
        await page.mouse.move(point.blank.x, point.blank.y);
        await page.mouse.down();
        await page.evaluate(() => window.dispatchEvent(new PointerEvent("pointercancel")));
        await page.mouse.up();
        assert.equal(await overlay.isVisible(), true, "cancelled pointer resumed input");
        await page.mouse.down();
        await page.evaluate(() => document.querySelector(".persea-unified-select__body").dispatchEvent(new Event("scroll")));
        await page.mouse.up();
        assert.equal(await overlay.isVisible(), true, "scrolling resumed input");
        await page.mouse.down();
        await page.keyboard.press("Escape");
        await select.press("Enter");
        await overlay.waitFor({ state: "visible" });
        await page.mouse.up();
        assert.equal(await overlay.isVisible(), true, "old pointer dismissed a new selection snapshot");

        // The same real mouse sequence exercises drag/hold in both engine families.
        // Actual iOS selection handles and software keyboard require device testing.
        await page.mouse.move(point.blank.x, point.blank.y);
        await page.mouse.down();
        await page.mouse.move(point.blank.x - 40, point.blank.y + 20, { steps: 4 });
        await page.mouse.move(point.blank.x, point.blank.y, { steps: 4 });
        await page.mouse.up();
        assert.equal(await overlay.isVisible(), true, "drag returning to its origin resumed input");
        await page.mouse.move(point.blank.x, point.blank.y);
        await page.mouse.down();
        await page.waitForTimeout(650);
        await page.mouse.up();
        assert.equal(await overlay.isVisible(), true, "long press resumed input");

        // Keep native copy-menu taps and the area around range handles selectable.
        await page.evaluate(() => {
          const row = [...document.querySelectorAll(".persea-unified-select__row")]
            .find(node => node.textContent.startsWith("Select these words"));
          const node = row.firstChild.firstChild || row.firstChild;
          const range = document.createRange(); range.setStart(node, 0); range.setEnd(node, 6);
          getSelection().removeAllRanges(); getSelection().addRange(range);
        });
        await tap(point.handle);
        assert.equal(await overlay.isVisible(), true, "tap near selection handle resumed input");
        await tap(point.blank);
        await overlay.waitFor({ state: "hidden" });
        assert.equal(await focused(), true);
        assert.deepEqual(await wire(), afterInput, "selection gestures emitted input or resized");

        // The release that created selection must not also dismiss it.
        if (shape.touch) {
          const live = await page.locator(".xterm-screen").boundingBox();
          await page.mouse.move(live.x + 15, live.y + 10);
          await page.mouse.down(); await page.waitForTimeout(650); await page.mouse.up();
          assert.equal(await overlay.isVisible(), true, "long-press release dismissed its new selection");
          await select.click(); await overlay.waitFor({ state: "hidden" });
        }
        assert.deepEqual(consoleMessages, [], "browser console was not clean");
        console.log(`PASS selection input ${engine}/${shape.name}`);
      } finally { await context.close(); }
    }
  } finally { await api.dispose(); await browser.close(); await fixture.close(); }
}

main().catch(error => { console.error(error); process.exitCode = 1; });
