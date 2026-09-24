"use strict";

const fs = require("fs");
const https = require("https");
const path = require("path");
const { startFixture } = require("./unified_reopen_fixture.cjs");
const { requestJSON: requestHTTPJSON } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_TERMINAL_TOUCH_ENGINE || "chromium";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const EVIDENCE = path.resolve(process.env.PERSEA_TERMINAL_TOUCH_EVIDENCE_DIR || path.join("/tmp", `persea-terminal_touch-${ENGINE}`));
const SHAPE = process.env.PERSEA_TERMINAL_TOUCH_SHAPE || "";
const CASE = process.env.PERSEA_TERMINAL_TOUCH_CASE || "all";
const MUTANT = process.env.PERSEA_TERMINAL_TOUCH_MUTANT || "none";
const CASES = new Set(["all", "dictation", "alternate", "selection", "longpress", "geometry", "switcher", "typography", "focus"]);
const MUTANTS = new Set(["none", "edge-selection", "focus-session", "backspace-repeat", "geometry-action", "switcher-hierarchy", "late-console"]);
const enabled = (name) => CASE === "all" || CASE === name;
const SHAPES = [
  { name: "phone-360", viewport: { width: 360, height: 780 }, touch: true },
  { name: "phone-390", viewport: { width: 390, height: 844 }, touch: true },
  { name: "tablet-768", viewport: { width: 768, height: 1024 }, touch: true },
  { name: "touch-820", viewport: { width: 820, height: 900 }, touch: true },
  { name: "desktop-1180", viewport: { width: 1180, height: 820 }, touch: false },
  { name: "desktop-1440", viewport: { width: 1440, height: 900 }, touch: false },
].filter((shape) => SHAPE === "" || shape.name === SHAPE);

if (!CASES.has(CASE)) throw new Error(`unknown terminal touch case: ${CASE}`);
if (!MUTANTS.has(MUTANT)) throw new Error(`unknown terminal touch mutant: ${MUTANT}`);
if (SHAPES.length === 0) throw new Error(`unknown terminal touch shape: ${SHAPE}`);

function assert(value, message) { if (!value) throw new Error(message); }
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const resizeCount = (state) => (state.attachments || []).reduce((total, attachment) => total + (attachment.resizeRequests?.length || 0), 0);
const terminalRows = (columns, rows, edgeSentinel, finalRowSentinel) => Array.from({ length: rows }, (_, index) => {
  if (index === 0) return "x".repeat(columns - 1) + edgeSentinel;
  if (index === rows - 1) return finalRowSentinel + "x".repeat(columns - 1);
  return "x".repeat(columns);
});
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

async function longPressTerminalColumn(page, rowNeedle, column) {
  await page.evaluate((column) => {
    const screen = document.querySelector(".xterm-screen");
    const scroller = document.querySelector(".persea-unified-scroll");
    const geometry = document.querySelector(".persea-unified-view-disclosure")?.textContent || "";
    const columns = Number.parseInt(geometry, 10);
    if (!(screen instanceof HTMLElement) || !(scroller instanceof HTMLElement) || !Number.isInteger(columns) || columns <= 0) return;
    const box = screen.getBoundingClientRect();
    const cellWidth = box.width / columns;
    scroller.scrollLeft = Math.max(0, Math.min(scroller.scrollWidth - scroller.clientWidth, (column + 0.5) * cellWidth - scroller.clientWidth / 2));
    scroller.dispatchEvent(new Event("scroll"));
  }, column);
  await delay(80);
  const point = await page.evaluate(({ rowNeedle, column }) => {
    const screen = document.querySelector(".xterm-screen");
    const rows = [...document.querySelectorAll(".xterm-rows > div")];
    const row = rows.findIndex((node) => (node.textContent || "").includes(rowNeedle));
    const geometry = document.querySelector(".persea-unified-view-disclosure")?.textContent || "";
    const columns = Number.parseInt(geometry, 10);
    if (!(screen instanceof HTMLElement) || row < 0 || !Number.isInteger(columns) || columns <= 0) throw new Error(`cannot locate ${rowNeedle}`);
    const box = screen.getBoundingClientRect();
    const rowBox = rows[row]?.getBoundingClientRect();
    const x = box.left + (column + 0.5) * box.width / columns;
    const y = rowBox ? rowBox.top + rowBox.height / 2 : box.top + (row + 0.5) * box.height / rows.length;
    return { x, y, screen: { left: box.left, width: box.width, top: box.top, height: box.height }, rowBox: rowBox ? { top: rowBox.top, height: rowBox.height } : null, rowText: rows[row]?.textContent || "", row, rows: rows.length, target: document.elementFromPoint(x, y)?.className || "" };
  }, { rowNeedle, column });
  await page.mouse.move(point.x, point.y);
  await page.mouse.down();
  try {
    // Keep the one press held until the browser processes its long-press
    // timer. A Node-side sleep does not acknowledge browser event delivery.
    await page.locator(".persea-unified-select").waitFor({ state: "visible", timeout: 1_500 });
  } catch {
    throw new Error(`long press did not enter Select: ${JSON.stringify(point)}`);
  } finally {
    await page.mouse.up();
  }
  const selected = await page.evaluate(() => window.getSelection()?.toString() || "");
  return { selected, point };
}

async function copyFrozenSentinel(page, sentinel, expectedColumns, finalRow = false) {
  await page.waitForFunction(() => document.querySelector(".persea-unified-select-context")?.getAttribute("data-select-state") === "select", undefined, { timeout: 3_000 });
  await page.locator(".persea-unified-select-context").click();
  await page.locator(".persea-unified-select").waitFor({ state: "visible" });
  const location = await page.evaluate(({ sentinel, expectedColumns, finalRow }) => {
    const rows = [...document.querySelectorAll(".persea-unified-select__row")];
    const rowIndex = rows.findIndex((row) => (row.textContent || "").includes(sentinel));
    const row = rows[rowIndex];
    if (!(row instanceof HTMLElement)) throw new Error(`missing frozen sentinel ${sentinel}`);
    const text = row.textContent || "";
    const column = Array.from(text.slice(0, text.indexOf(sentinel))).length;
    if (finalRow && rowIndex !== rows.length - 1) throw new Error(`sentinel ${sentinel} is not on the final frozen row`);
    if (!finalRow && column !== expectedColumns - 1) throw new Error(`sentinel ${sentinel} column=${column}, want=${expectedColumns - 1}`);
    const walker = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
    let target;
    let offset = 0;
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const index = node.data.indexOf(sentinel);
      if (index >= 0) { target = node; offset = index; break; }
    }
    if (!(target instanceof Text)) throw new Error(`sentinel ${sentinel} has no selectable text node`);
    const range = document.createRange();
    range.setStart(target, offset);
    range.setEnd(target, offset + sentinel.length);
    const selection = window.getSelection();
    if (!selection) throw new Error("selection unavailable");
    selection.removeAllRanges();
    selection.addRange(range);
    document.dispatchEvent(new Event("selectionchange"));
    return { row: rowIndex, column, rows: rows.length, textColumns: Array.from(text).length };
  }, { sentinel, expectedColumns, finalRow });
  assert(location.textColumns === expectedColumns, `frozen sentinel row width=${location.textColumns}, want=${expectedColumns}`);
  // the Paste slot is the Copy control while a range exists.
  await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.getAttribute("data-paste-state") === "copy");
  await page.locator(".persea-unified-toolbar-paste").click();
  await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.getAttribute("data-paste-state") === "copied");
  const clipboard = await page.evaluate(() => navigator.clipboard.readText());
  assert(clipboard === sentinel, `frozen selection copied ${JSON.stringify(clipboard)}, want ${JSON.stringify(sentinel)}`);
  return { ...location, clipboard };
}

async function main() {
  assert(path.isAbsolute(MODULE), "PERSEA_PLAYWRIGHT_MODULE must be absolute");
  const playwright = require(MODULE);
  assert(playwright[ENGINE], `Playwright has no ${ENGINE} engine`);
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const fixture = await startFixture(UI, { tls: ENGINE === "webkit", liveEncoding: "utf8", playwrightScreenshotStyle: true });
  const control = (body) => requestJSON(`${fixture.origin}/__fixture/control`, "POST", body);
  const snapshot = () => requestJSON(`${fixture.origin}/__fixture/control`);
  const browser = await playwright[ENGINE].launch({
    headless: true,
    ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
  });
  const evidence = { engine: ENGINE, mutant: MUTANT, cases: [], screenshots: [], caseCounts: Object.fromEntries([...CASES].filter((name) => name !== "all").map((name) => [name, 0])) };
  try {
    for (const shape of SHAPES) {
      await control({ reset: true, switchSessions: true, sessionBState: "open", terminal_touchSwitcherMetadata: MUTANT !== "switcher-hierarchy" });
      const inventory = await requestJSON(`${fixture.origin}/api/inventory`);
      const session = inventory.realms[0].servers[0].sessions[0];
      const context = await browser.newContext({ viewport: shape.viewport, isMobile: shape.touch, hasTouch: shape.touch, deviceScaleFactor: shape.touch ? 2 : 1, ignoreHTTPSErrors: true });
      if (ENGINE === "chromium") await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin: fixture.origin });
      if (ENGINE === "webkit") {
        await context.addInitScript(() => {
          let value = "";
          Object.defineProperty(navigator, "clipboard", { configurable: true, value: {
            writeText: async (text) => { value = String(text); },
            readText: async () => value,
          } });
        });
      }
      const page = await context.newPage();
      const errors = [];
      const consoleMessages = [];
      page.on("pageerror", (error) => errors.push(String(error)));
      page.on("console", (message) => {
        consoleMessages.push(`${message.type()}:${message.text()}`);
        if (message.type() === "error") errors.push(`${message.text()} @ ${message.location().url || "unknown"}`);
      });
      page.on("response", (response) => {
        if (response.status() >= 400) errors.push(`HTTP ${response.status()} ${response.url()}`);
      });
      const url = `${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" })}`;
      await page.goto(url, { waitUntil: "load" });
      await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
      const baseline = await snapshot();
      const baselineResizes = resizeCount(baseline);

      if (enabled("dictation") && shape.touch) {
        evidence.caseCounts.dictation += 1;
        const runSession = (steps, resetFocus) => page.evaluate(({ steps, resetFocus }) => {
          const textarea = document.querySelector(".xterm-helper-textarea");
          if (!(textarea instanceof HTMLTextAreaElement)) throw new Error("missing xterm helper");
          if (resetFocus) textarea.blur();
          textarea.focus({ preventScroll: true });
          const deliver = (data, { owner = false, replace = false } = {}) => {
            if (owner) {
              const prelude = new KeyboardEvent("keydown", { key: "Process", code: "Unidentified", bubbles: true, cancelable: true });
              Object.defineProperty(prelude, "keyCode", { value: 229 });
              Object.defineProperty(prelude, "which", { value: 229 });
              textarea.dispatchEvent(prelude);
            }
            textarea.setSelectionRange(textarea.value.length, textarea.value.length);
            textarea.dispatchEvent(new InputEvent("beforeinput", { inputType: "insertText", data, bubbles: true, cancelable: true, composed: true }));
            const start = textarea.selectionStart;
            const end = textarea.selectionEnd;
            textarea.value = replace ? data : textarea.value.slice(0, start) + data + textarea.value.slice(end);
            textarea.setSelectionRange(textarea.value.length, textarea.value.length);
            textarea.dispatchEvent(new InputEvent("input", { inputType: "insertText", data, bubbles: true, cancelable: true, composed: true }));
          };
          for (const step of steps) deliver(step.data, step);
          return { value: textarea.value, valueLength: textarea.value.length, caret: textarea.selectionStart, active: document.activeElement === textarea };
        }, { steps, resetFocus });

        const beforeFirst = (await snapshot()).attachments.at(-1)?.inputs.length || 0;
        const firstHelper = await runSession([
          { data: "alpha " },
          { data: "alpha beta", owner: true, replace: true },
          { data: "alpha beta gamma", owner: true, replace: true },
          { data: "alpha beta gamma", owner: true, replace: true },
          { data: "alpha beta", owner: true, replace: true },
          { data: "alpha zeta", owner: true, replace: true },
          { data: "alpha zeta delta", owner: true, replace: true },
          { data: "alpha beta", owner: true, replace: false },
        ], true);
        await delay(50);
        const afterFirst = await snapshot();
        const firstDelivered = (afterFirst.attachments.at(-1)?.inputs || []).slice(beforeFirst);
        assert(firstDelivered.join("") === `alpha beta gamma${"\x7f".repeat(10)}zeta deltaalpha beta`, `${shape.name}: first textarea-owner session duplicated/deleted input: ${JSON.stringify(firstDelivered)}`);
        assert(firstHelper.valueLength === 26 && firstHelper.caret === 26 && firstHelper.active, `${shape.name}: first helper session drifted: ${JSON.stringify(firstHelper)}`);

        const beforeSecond = afterFirst.attachments.at(-1)?.inputs.length || 0;
        if (MUTANT !== "focus-session") {
          await page.evaluate(() => {
            const textarea = document.querySelector(".xterm-helper-textarea");
            if (!(textarea instanceof HTMLTextAreaElement)) throw new Error("missing xterm helper");
            textarea.blur();
            textarea.focus({ preventScroll: true });
          });
        }
        const secondHelper = await runSession([
          { data: "omega one", owner: true, replace: true },
          { data: "omega two", owner: true, replace: true },
        ], false);
        await delay(50);
        const afterSecond = await snapshot();
        const secondDelivered = (afterSecond.attachments.at(-1)?.inputs || []).slice(beforeSecond);
        assert(secondDelivered.join("") === `omega one${"\x7f".repeat(3)}two`, `${shape.name}: retained prefix crossed textarea-owner focus sessions: ${JSON.stringify(secondDelivered)}`);
        assert(secondHelper.value === "omega two" && secondHelper.active, `${shape.name}: second helper session drifted: ${JSON.stringify(secondHelper)}`);
        await page.evaluate(() => document.querySelector(".xterm-helper-textarea")?.blur());
      }

      if (enabled("alternate")) {
        evidence.caseCounts.alternate += 1;
        const alternateColumns = MUTANT === "edge-selection" ? 79 : 80;
        const alternateRows = terminalRows(alternateColumns, 24, "¤", "¶");
        await control({ writeLive: `NORMAL_ONLY\r\n\u001b[?1049h${alternateRows.join("\r\n")}` });
        await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("¶"));
        await page.locator(".persea-unified-select-context").click();
        await page.locator(".persea-unified-select").waitFor({ state: "visible" });
        const alternate = await page.evaluate(() => {
          const text = document.querySelector(".persea-unified-select__body")?.textContent || "";
          return { text, rows: document.querySelectorAll(".persea-unified-select__row").length };
        });
        assert(!alternate.text.includes("NORMAL_ONLY") && (alternate.text.match(/¤/g) || []).length === 1
          && (alternate.text.match(/¶/g) || []).length === 1 && alternate.rows === 24,
        `${shape.name}: alternate capture crossed buffers or duplicated the bottom row: ${JSON.stringify(alternate)}`);
        await page.locator(".persea-unified-select-context").click();
        const alternateEdgeCopy = await copyFrozenSentinel(page, "¤", 80, false);
        const alternateFinalRowCopy = await copyFrozenSentinel(page, "¶", 80, true);
        assert(alternateEdgeCopy.column === 79 && alternateFinalRowCopy.row === 23,
          `${shape.name}: alternate final-cell/final-row selection missed the real buffer edges`);
        await control({ writeLive: "\u001b[?1049l" });
      }

      let frozen = {};
      if (enabled("selection")) {
        evidence.caseCounts.selection += 1;
        const ansi16 = [...Array(8).keys()].map((index) => `\u001b[${30 + index}mC${index}\u001b[0m`).join(" ")
          + " " + [...Array(8).keys()].map((index) => `\u001b[${90 + index}mB${index}\u001b[0m`).join(" ");
        const decorations = "\u001b[44mBACKGROUND\u001b[0m \u001b[8mCONCEALED_SENTINEL\u001b[0m \u001b[5mBLINK\u001b[0m \u001b[53mOVERLINE\u001b[0m";
        const mapping = "ABCD\u4e2dZ e\u0301Z \ud83d\udc69\u200d\ud83d\udcbbZ ROWEND";
        await control({ writeLive: `${ansi16}\r\n\u001b[31mRED\u001b[0m \u001b[1;4mBOLD_UNDER\u001b[0m \u001b[7mINVERSE\u001b[0m ${decorations}\r\n${mapping}` });
        try {
          await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("ROWEND"), undefined, { timeout: 5_000 });
        } catch {
          const rendered = await page.evaluate(() => document.querySelector(".xterm-rows")?.textContent || "");
          throw new Error(`${shape.name}: initial map missing: ${JSON.stringify(rendered.slice(-400))}`);
        }
        const mappedSelections = [];
        if (shape.touch) {
          for (const [column, expected] of [[4, "中"], [5, "中"], [8, "e\u0301"], [12, "\ud83d\udc69\u200d\ud83d\udcbb"]]) {
            const outcome = await longPressTerminalColumn(page, "ROWEND", column);
            mappedSelections.push(outcome.selected);
            assert(outcome.selected === expected, `${shape.name}: terminal column ${column} selected ${JSON.stringify(outcome.selected)}, want ${JSON.stringify(expected)} point=${JSON.stringify(outcome.point)}`);
            await page.keyboard.press("Escape");
            await page.locator(".persea-unified-select").waitFor({ state: "hidden" });
          }
        }
        await page.waitForFunction(() => document.querySelector(".persea-unified-select-context")?.getAttribute("data-select-state") === "select", undefined, { timeout: 3_000 });
        await page.locator(".persea-unified-select-context").click();
        await page.locator(".persea-unified-select").waitFor({ state: "visible" });
        const presentation = await page.evaluate(() => {
          const cells = [...document.querySelectorAll(".persea-unified-select__cell")];
          return {
            cells: cells.length,
            styled: cells.filter((cell) => (cell.getAttribute("style") || "").trim() !== "").length,
            foregrounds: new Set(cells.map((cell) => cell.style.color).filter(Boolean)).size,
            backgrounds: cells.filter((cell) => cell.style.backgroundColor).length,
            concealed: (() => { const cell = document.querySelector('[data-invisible="true"]'); return cell ? { visibility: getComputedStyle(cell).visibility, aria: cell.getAttribute("aria-hidden") } : null; })(),
            blink: (() => { const cell = document.querySelector('[data-blink="true"]'); return cell ? getComputedStyle(cell).animationName : ""; })(),
            overline: (() => { const cell = document.querySelector('[data-overline="true"]'); return cell ? getComputedStyle(cell).textDecorationLine : ""; })(),
          };
        });
        assert(presentation.cells > 0 && presentation.styled > 0 && presentation.foregrounds >= 16 && presentation.backgrounds > 0, `${shape.name}: frozen selection flattened ANSI presentation: ${JSON.stringify(presentation)}`);
        assert(presentation.concealed?.visibility === "hidden" && presentation.concealed.aria === "true", `${shape.name}: concealed glyph was revealed: ${JSON.stringify(presentation)}`);
        assert(presentation.blink && presentation.blink !== "none", `${shape.name}: blink decoration was dropped: ${JSON.stringify(presentation)}`);
        assert(presentation.overline.includes("overline"), `${shape.name}: overline decoration was dropped: ${JSON.stringify(presentation)}`);
        await page.locator(".persea-unified-select-context").click();

        await page.locator(".persea-unified-view-disclosure").click();
        await page.locator('.persea-unified-view-popover input[aria-label="Columns"]').fill("240");
        // the single Apply refits when the typed columns differ.
        await page.locator(".persea-unified-view-popover .persea-unified-size__form").getByRole("button", { name: "Apply", exact: true }).click();
        await page.waitForFunction(() => (document.querySelector(".persea-unified-view-disclosure")?.textContent || "").startsWith("240×"), undefined, { timeout: 5_000 });
        for (let attempt = 0; attempt < 50; attempt += 1) {
          const state = await snapshot();
          if (state.attachments.at(-1)?.live) break;
          await delay(100);
          if (attempt === 49) throw new Error(`${shape.name}: refit did not establish a live successor attachment`);
        }
        await page.evaluate(() => {
          const scroll = document.querySelector(".persea-unified-scroll");
          if (scroll instanceof HTMLElement) {
            scroll.scrollLeft = 0;
            scroll.dispatchEvent(new Event("scroll"));
          }
        });
        await delay(100);
        const deepColumns = MUTANT === "edge-selection" ? 239 : 240;
        const deep = terminalRows(deepColumns, 200, "§", "¥");
        for (let index = 0; index < deep.length; index += 20) {
          const tail = index + 20 < deep.length ? "\r\n" : "";
          await control({ writeLive: `${deep.slice(index, index + 20).join("\r\n")}${tail}` });
        }
        await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("¥"));
        await page.locator(".persea-unified-select-context").click();
        await page.locator(".persea-unified-select").waitFor({ state: "visible" });
        frozen = await page.evaluate(() => {
        const body = document.querySelector(".persea-unified-select__body");
        const cells = [...document.querySelectorAll(".persea-unified-select__cell")];
        const rows = [...document.querySelectorAll(".persea-unified-select__row")];
        if (!(body instanceof HTMLElement)) throw new Error("missing frozen body");
        body.scrollLeft = body.scrollWidth;
        body.scrollTop = body.scrollHeight;
        const last = rows.at(-1)?.getBoundingClientRect();
        const box = body.getBoundingClientRect();
        return {
          cells: cells.length,
          styled: cells.filter((cell) => (cell.getAttribute("style") || "").trim() !== "").length,
          foregrounds: new Set(cells.map((cell) => cell.style.color).filter(Boolean)).size,
          touchAction: getComputedStyle(body).touchAction,
          horizontalEnd: Math.abs(body.scrollLeft + body.clientWidth - body.scrollWidth) <= 2,
          finalRowReachable: !!last && last.bottom <= box.bottom + 2,
          deepRows: rows.length,
        };
        });
        frozen.mappedSelections = mappedSelections;
        frozen.presentation = presentation;
        assert(/pan-x|auto|manipulation/.test(frozen.touchAction), `${shape.name}: frozen selection cannot pan horizontally: ${JSON.stringify(frozen)}`);
        assert(frozen.horizontalEnd && frozen.finalRowReachable && frozen.deepRows >= 200, `${shape.name}: frozen selection stranded an edge: ${JSON.stringify(frozen)}`);
        await page.locator(".persea-unified-select-context").click();
        frozen.normalEdgeCopy = await copyFrozenSentinel(page, "§", 240, false);
        frozen.normalFinalRowCopy = await copyFrozenSentinel(page, "¥", 240, true);
      }

      if (enabled("longpress") && shape.touch) {
        evidence.caseCounts.longpress += 1;
        const inputBaseline = (await snapshot()).attachments.at(-1)?.inputs.length || 0;
        const terminal = page.locator(".persea-unified-xterm");
        const box = await terminal.boundingBox();
        assert(box, `${shape.name}: missing terminal box`);
        await page.mouse.move(box.x + Math.min(90, box.width / 3), box.y + Math.min(80, box.height / 3));
        await page.mouse.down();
        try {
          await page.locator(".persea-unified-select").waitFor({ state: "visible", timeout: 1_500 });
        } finally {
          await page.mouse.up();
        }
        const selected = await page.evaluate(() => window.getSelection()?.toString() || "");
        assert(selected.length > 0, `${shape.name}: stationary long press did not preselect a cell/word`);
        await page.locator(".persea-unified-select-context").click();
        await terminal.waitFor({ state: "visible" });
        const moved = await terminal.boundingBox();
        assert(moved, `${shape.name}: terminal disappeared after selection cancel`);
        await page.mouse.move(moved.x + 40, moved.y + 40);
        await page.mouse.down();
        await page.mouse.move(moved.x + 90, moved.y + 90, { steps: 4 });
        await delay(600);
        await page.mouse.up();
        assert(await page.locator(".persea-unified-select").isHidden(), `${shape.name}: scrolling movement incorrectly entered Select`);
        const inputAfter = (await snapshot()).attachments.at(-1)?.inputs.length || 0;
        assert(inputAfter === inputBaseline, `${shape.name}: selection gesture emitted PTY input`);
      }

      let focusFlow = {};
      if (enabled("focus") && shape.touch) {
        evidence.caseCounts.focus += 1;
        await control({ writeLive: "\r\nFOCUSROW" });
        await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("FOCUSROW"));
        await page.locator(".xterm-screen").click({ position: { x: 24, y: 24 } });
        await page.waitForFunction(() => document.activeElement?.classList.contains("xterm-helper-textarea") === true);
        const before = await snapshot();
        const beforeInputs = before.attachments.at(-1)?.inputs.length || 0;
        const picked = await longPressTerminalColumn(page, "FOCUSROW", 0);
        assert(picked.selected === "F", `${shape.name}: focus sequence failed to select one terminal cell: ${JSON.stringify(picked)}`);
        await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copy");
        await page.locator(".persea-unified-toolbar-paste").click();
        await page.locator(".persea-unified-select").waitFor({ state: "hidden" });
        await page.waitForFunction(() => (document.querySelector(".persea-unified-toolbar-paste")?.textContent || "").includes("Copied"));
        // The Copied dwell ends before the slot is Paste again.
        await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "paste");
        await page.locator(".persea-unified-toolbar-paste").click();
        // The row's explicit paste action owns insertion. Showing the list
        // emits no input; original helper/backspace assertions remain below.
        const clipboard = page.locator("dialog.persea-clipboard");
        await clipboard.waitFor({ state: "visible" });
        const copiedRow = clipboard.locator(".persea-clipboard__item").filter({ has: page.locator(".persea-clipboard__preview", { hasText: /^F$/ }) }).first();
        await copiedRow.waitFor();
        assert(await copiedRow.locator(".persea-clipboard__preview").textContent() === "F", `${shape.name}: Clipboard row changed the selected text`);
        const previewInputs = (await snapshot()).attachments.at(-1)?.inputs.length || 0;
        assert(previewInputs === beforeInputs, `${shape.name}: Clipboard preview emitted PTY input before Send`);
        await copiedRow.getByRole("button", { name: "Paste text to terminal", exact: true }).click();
        await clipboard.waitFor({ state: "hidden" });
        await delay(50);
        const activeAfterPaste = await page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea") === true);
        assert(activeAfterPaste, `${shape.name}: Paste did not automatically restore the xterm helper`);
        const requiredRepeats = 3;
        const emittedRepeats = MUTANT === "backspace-repeat" ? 0 : requiredRepeats;
        await page.keyboard.down("Backspace");
        for (let repeat = 0; repeat < emittedRepeats; repeat += 1) {
          await delay(90);
          await page.keyboard.down("Backspace");
        }
        await page.keyboard.up("Backspace");
        await delay(50);
        const after = await snapshot();
        const delivered = (after.attachments.at(-1)?.inputs || []).slice(beforeInputs).join("");
        const helperStayedFocused = await page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea") === true);
        const expected = `F${"\x7f".repeat(requiredRepeats + 1)}`;
        assert(delivered === expected, `${shape.name}: Select→Copy→Paste→held-Backspace did not deliver every ordered repeat: ${JSON.stringify(delivered)}`);
        assert(helperStayedFocused, `${shape.name}: held Backspace displaced the real xterm helper`);
        focusFlow = { activeAfterPaste, helperStayedFocused, repeatCount: requiredRepeats, deliveredUtf16: delivered.length };
      }

      let geometry = {};
      if (enabled("geometry")) {
        evidence.caseCounts.geometry += 1;
        const presentationBox = () => page.evaluate(() => {
          const rect = (node) => { const box = node.getBoundingClientRect(); return { width: box.width, height: box.height, top: box.top, left: box.left }; };
          const disclosure = document.querySelector(".persea-unified-view-disclosure");
          const toolbar = document.querySelector(".persea-unified-toolbar");
          const host = document.querySelector(".persea-unified-terminal");
          if (!(disclosure instanceof HTMLElement) || !(toolbar instanceof HTMLElement) || !(host instanceof HTMLElement)) throw new Error("missing View presentation box");
          const style = getComputedStyle(disclosure);
          return { disclosure: { box: rect(disclosure), background: style.backgroundColor, border: style.borderColor }, toolbar: rect(toolbar), host: rect(host) };
        });
        const closedPresentation = await presentationBox();
        await page.locator(".persea-unified-view-disclosure").click();
        const viewScreenshot = path.join(EVIDENCE, `${shape.name}-view-open.png`);
        await page.screenshot({ path: viewScreenshot, animations: "disabled", caret: "hide" });
        evidence.screenshots.push(viewScreenshot);
        geometry = await page.evaluate(() => {
          const disclosure = document.querySelector(".persea-unified-view-disclosure")?.textContent || "";
          const match = disclosure.match(/(\d+)\s*[×x]\s*(\d+)/);
          const popover = document.querySelector(".persea-unified-view-popover");
          const toolbar = document.querySelector(".persea-unified-toolbar");
          const terminal = document.querySelector(".persea-unified-scroll");
          const disclosureNode = document.querySelector(".persea-unified-view-disclosure");
          if (!(popover instanceof HTMLElement) || !(toolbar instanceof HTMLElement) || !(disclosureNode instanceof HTMLElement)) throw new Error("missing View presentation");
          const popoverBox = popover.getBoundingClientRect();
          const popoverStyle = getComputedStyle(popover);
          const terminalBox = terminal instanceof HTMLElement ? terminal.getBoundingClientRect() : null;
          const disclosureBox = disclosureNode.getBoundingClientRect();
          const disclosureStyle = getComputedStyle(disclosureNode);
          const toolbarText = toolbar.textContent || "";
          const geometryStrings = (toolbarText.match(/\d+\s*[×x]\s*\d+/g) || []);
          const descendants = [...popover.querySelectorAll("button,input,select,label")].filter((node) => node instanceof HTMLElement).map((node) => {
            const box = node.getBoundingClientRect();
            return { tag: node.tagName, text: (node.textContent || node.getAttribute("aria-label") || "").trim(), left: box.left, right: box.right, top: box.top, bottom: box.bottom, width: box.width, height: box.height };
          });
          // one geometry block — the two refit actions above one
          // form row of Columns, Rows and a single Apply.
          const sizeBlock = document.querySelector(".persea-unified-size[data-surface=view]");
          const actionsRow = sizeBlock?.querySelector(".persea-unified-size__actions");
          const formRow = sizeBlock?.querySelector(".persea-unified-size__form");
          if (!(actionsRow instanceof HTMLElement) || !(formRow instanceof HTMLElement)) throw new Error("incomplete geometry block");
          const measureBox = (node) => { const box = node.getBoundingClientRect(); return { width: box.width, height: box.height, top: box.top, bottom: box.bottom, left: box.left, right: box.right }; };
          const apply = formRow.querySelector("button");
          if (!(apply instanceof HTMLElement)) throw new Error("geometry form lacks Apply");
          const measures = {
            actions: [...actionsRow.querySelectorAll("button")].map((button) => ({ text: (button.textContent || "").trim(), box: measureBox(button) })),
            captions: [...formRow.querySelectorAll(".persea-unified-size__caption")].map((node) => (node.textContent || "").trim()),
            inputs: [...formRow.querySelectorAll("input")].map((input) => ({ name: input.getAttribute("aria-label") || (input.closest("label")?.querySelector(".persea-unified-size__caption")?.textContent || "").trim(), box: measureBox(input) })),
            apply: { text: (apply.textContent || "").trim(), box: measureBox(apply) },
          };
          return {
            columns: Number(match?.[1]), rows: Number(match?.[2]), measures,
            popover: { left: popoverBox.left, right: popoverBox.right, top: popoverBox.top, bottom: popoverBox.bottom, width: popoverBox.width, height: popoverBox.height, scrollWidth: popover.scrollWidth, clientWidth: popover.clientWidth, maxHeight: popoverStyle.maxHeight, overflowY: popoverStyle.overflowY },
            terminalHeight: terminalBox?.height || 0,
            geometryStrings, toolbarText,
            descendants,
            disclosure: { box: { width: disclosureBox.width, height: disclosureBox.height, top: disclosureBox.top, left: disclosureBox.left }, background: disclosureStyle.backgroundColor, border: disclosureStyle.borderColor, expanded: disclosureNode.getAttribute("aria-expanded") },
          };
        });
        assert(geometry.popover.width <= Math.min(352, shape.viewport.width - 16) + 1
          && geometry.popover.height <= (shape.touch ? 300 : 260) + 1,
        `${shape.name}: View card exceeds its compact bound: ${JSON.stringify(geometry.popover)}`);
        assert(geometry.popover.left >= 8 && geometry.popover.right <= shape.viewport.width - 8
          && geometry.popover.top >= 8 && geometry.popover.bottom <= shape.viewport.height - 8
          && geometry.popover.scrollWidth === geometry.popover.clientWidth
          && geometry.popover.maxHeight !== "none" && geometry.popover.overflowY === "auto",
        `${shape.name}: View card escaped or horizontally scrolled: ${JSON.stringify(geometry.popover)}`);
        assert(geometry.descendants.every((node) => node.left >= geometry.popover.left && node.right <= geometry.popover.right
          && node.top >= geometry.popover.top && node.bottom <= geometry.popover.bottom),
        `${shape.name}: View child escaped the card: ${JSON.stringify(geometry.descendants)}`);
        assert(geometry.geometryStrings.length === 1, `${shape.name}: current geometry was duplicated: ${JSON.stringify(geometry.geometryStrings)}`);
        assert(shape.name !== "phone-390" || geometry.terminalHeight >= 360, `${shape.name}: View left too little terminal visible: ${geometry.terminalHeight}`);
        assert(geometry.disclosure.expanded === "true"
          && geometry.disclosure.background !== closedPresentation.disclosure.background
          && geometry.disclosure.border !== closedPresentation.disclosure.border
          && JSON.stringify(geometry.disclosure.box) === JSON.stringify(closedPresentation.disclosure.box),
        `${shape.name}: open disclosure lacked a distinct stable accent: ${JSON.stringify({ closedPresentation, open: geometry.disclosure })}`);
        const formBoxes = [...geometry.measures.inputs.map((input) => input.box), geometry.measures.apply.box];
        const formMid = (formBoxes[0].top + formBoxes[0].bottom) / 2;
        assert(formBoxes.every((box) => Math.abs((box.top + box.bottom) / 2 - formMid) <= 2)
          && geometry.measures.inputs[0].box.right <= geometry.measures.inputs[1].box.left
          && geometry.measures.inputs[1].box.right <= geometry.measures.apply.box.left,
        `${shape.name}: Columns, Rows and Apply are not one aligned row: ${JSON.stringify(geometry.measures)}`);
        assert(geometry.measures.captions.join(",") === "Cols,Rows"
          && geometry.measures.inputs.map((input) => input.name).join(",") === "Columns,Rows"
          && geometry.measures.apply.text === "Apply",
        `${shape.name}: geometry form does not read Columns, Rows, Apply: ${JSON.stringify(geometry.measures)}`);
        assert(geometry.measures.actions.map((action) => action.text).join(",") === "↕ Fit rows,↔ Fit width"
          && geometry.measures.actions.every((action) => action.box.bottom <= formBoxes[0].top + 1),
        `${shape.name}: refit actions are not ordered above the size form: ${JSON.stringify(geometry.measures)}`);
        // a three-digit box (3.4rem at the 16px floor ≈ 54px),
        // never below the 44px touch law, never back to the wide band.
        assert(geometry.measures.inputs.every((input) => input.box.width >= 48 && input.box.width <= 60 && input.box.height >= (shape.touch ? 44 : 32))
          && geometry.measures.apply.box.height >= (shape.touch ? 44 : 32)
          && geometry.measures.actions.every((action) => action.box.width <= 132 && action.box.height >= (shape.touch ? 44 : 32)),
        `${shape.name}: geometry controls missed compact posture bounds: ${JSON.stringify(geometry.measures)}`);
        const interactive = geometry.descendants.filter((node) => ["BUTTON", "INPUT", "SELECT"].includes(node.tag));
        assert(interactive.every((node) => node.width >= (shape.touch ? 44 : 32) && node.height >= (shape.touch ? 44 : 32)),
          `${shape.name}: View target fell below the posture minimum: ${JSON.stringify(interactive)}`);

        const presentationResizes = resizeCount(await snapshot());
        const measureSelectedOption = (locator) => locator.evaluate((select) => {
          if (!(select instanceof HTMLSelectElement)) throw new Error("missing preference select");
          const option = select.selectedOptions[0];
          const style = getComputedStyle(select);
          const box = select.getBoundingClientRect();
          const canvas = document.createElement("canvas");
          const context = canvas.getContext("2d");
          if (!option || !context) throw new Error("cannot measure selected option");
          context.font = style.font;
          const textWidth = context.measureText(option.textContent || "").width;
          const number = (value) => Number.parseFloat(value) || 0;
          const contentWidth = box.width - number(style.paddingLeft) - number(style.paddingRight)
            - number(style.borderLeftWidth) - number(style.borderRightWidth);
          // Native arrows are not represented by CSS padding in either engine.
          // Reserve at least 24 px (or 1.5em) beyond the selected label.
          const arrowReserve = Math.max(24, number(style.fontSize) * 1.5);
          return {
            value: select.value,
            label: option.textContent || "",
            box: { left: box.left, right: box.right, top: box.top, bottom: box.bottom, width: box.width, height: box.height },
            textWidth, contentWidth, arrowReserve,
            fits: textWidth + arrowReserve <= contentWidth + 0.5,
          };
        });
        // the theme is a dashboard setting; the terminal follows the
        // record. Each theme is set on the fixture record and reaches the page
        // through its foreground re-read (14.3b), exactly as another tab's save
        // would.
        const themeValues = [...fs.readFileSync(path.join(UI, 'src/unified_themes.ts'), "utf8").match(/UNIFIED_THEME_IDS = Object\.freeze\(\[([^\]]+)\]/)[1].matchAll(/"([a-z0-9-]+)"/g)].map((match) => match[1]);
        assert(themeValues.length === 8, `theme id list not found: ${JSON.stringify(themeValues)}`);
        let themeRevision = (await snapshot()).preferences.revision;
        const accents = [];
        for (const theme of themeValues) {
          themeRevision += 1;
          await control({ preferences: { theme, revision: themeRevision, stored: true } });
          await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
          await page.waitForFunction((id) => document.querySelector(".persea-unified-terminal")?.dataset.theme === id, theme, { timeout: 5_000 });
          const open = await page.evaluate(() => {
            const color = (value) => {
              const rgb = value.match(/rgba?\((?:\s*)([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)/i);
              if (rgb) return rgb.slice(1, 4).map((part) => Number(part) / 255);
              const srgb = value.match(/color\(srgb\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)/i);
              return srgb ? srgb.slice(1, 4).map(Number) : undefined;
            };
            const contrast = (a, b) => {
              const luminance = (rgb) => rgb.map((part) => part <= 0.04045 ? part / 12.92 : ((part + 0.055) / 1.055) ** 2.4)
                .reduce((sum, part, index) => sum + part * [0.2126, 0.7152, 0.0722][index], 0);
              const aa = color(a); const bb = color(b);
              if (!aa || !bb) return 0;
              const [high, low] = [luminance(aa), luminance(bb)].sort((left, right) => right - left);
              return (high + 0.05) / (low + 0.05);
            };
            const disclosure = document.querySelector(".persea-unified-view-disclosure");
            const popover = document.querySelector(".persea-unified-view-popover");
            if (!(disclosure instanceof HTMLElement) || !(popover instanceof HTMLElement)) throw new Error("missing themed View");
            const disclosureStyle = getComputedStyle(disclosure); const popoverStyle = getComputedStyle(popover);
            const box = disclosure.getBoundingClientRect();
            return { expanded: disclosure.getAttribute("aria-expanded"), background: disclosureStyle.backgroundColor, border: disclosureStyle.borderColor,
              box: { width: box.width, height: box.height, top: box.top, left: box.left },
              disclosureContrast: contrast(disclosureStyle.borderColor, disclosureStyle.backgroundColor),
              popoverContrast: contrast(popoverStyle.borderColor, popoverStyle.backgroundColor) };
          });
          await page.locator(".persea-unified-view-disclosure").click();
          const closed = await page.locator(".persea-unified-view-disclosure").evaluate((node) => {
            const style = getComputedStyle(node); const box = node.getBoundingClientRect();
            return { expanded: node.getAttribute("aria-expanded"), background: style.backgroundColor, border: style.borderColor, box: { width: box.width, height: box.height, top: box.top, left: box.left } };
          });
          assert(open.expanded === "true" && closed.expanded === "false"
            && open.background !== closed.background && open.border !== closed.border
            && JSON.stringify(open.box) === JSON.stringify(closed.box)
            && open.disclosureContrast >= 3 && open.popoverContrast >= 3,
          `${shape.name}: ${theme} lacked stable 3:1 open/boundary state: ${JSON.stringify({ open, closed })}`);
          accents.push({ theme, open, closed });
          await page.locator(".persea-unified-view-disclosure").click();
        }
        await page.getByRole("button", { name: "Zoom out", exact: true }).click();
        await page.getByRole("button", { name: "Fit font", exact: true }).click();
        await page.getByRole("button", { name: "Zoom in", exact: true }).click();
        // no Appearance rows in the View popover any more.
        assert(await page.locator(".persea-unified-view-popover .persea-unified-preference").count() === 0, `${shape.name}: a preference picker leaked into the View popover`);
        const appearanceScreenshot = path.join(EVIDENCE, `${ENGINE}-${shape.name}-appearance-values.png`);
        await page.screenshot({ path: appearanceScreenshot, animations: "disabled", caret: "hide" });
        evidence.screenshots.push(appearanceScreenshot);
        await delay(50);
        const afterPresentation = await presentationBox();
        assert(resizeCount(await snapshot()) === presentationResizes, `${shape.name}: presentation actions emitted RESIZE_REQUEST`);
        const stableRect = (left, right) => ["width", "height", "top", "left"].every((key) => Math.abs(left[key] - right[key]) <= 0.5);
        assert(stableRect(closedPresentation.toolbar, afterPresentation.toolbar) && stableRect(closedPresentation.host, afterPresentation.host),
          `${shape.name}: View presentation shifted host/toolbar layout: ${JSON.stringify({ closedPresentation, afterPresentation })}`);

        const beforeGeometry = await snapshot();
        const targetRows = geometry.rows + 1;
        await page.locator('.persea-unified-view-popover input[aria-label="Rows"]').fill(MUTANT === "geometry-action" ? "invalid" : String(targetRows));
        await page.getByRole("button", { name: "Apply", exact: true }).click();
        if (MUTANT !== "geometry-action") {
          await page.waitForFunction((expected) => (document.querySelector(".persea-unified-view-disclosure")?.textContent || "").includes(`×${expected}`), targetRows);
        } else await delay(150);
        const afterRows = await snapshot();
        const rowRequests = afterRows.attachments.flatMap((attachment) => attachment.resizeRequests || []).slice(resizeCount(beforeGeometry));
        assert(rowRequests.length === 1 && rowRequests[0].columns === geometry.columns && rowRequests[0].rows === targetRows,
          `${shape.name}: Rows action did not emit exactly one rows-only request: ${JSON.stringify(rowRequests)}`);

        await page.getByRole("button", { name: "Fit rows", exact: true }).click();
        for (let attempt = 0; attempt < 50; attempt += 1) {
          if (resizeCount(await snapshot()) === resizeCount(afterRows) + 1) break;
          await delay(50);
          if (attempt === 49) throw new Error(`${shape.name}: Fit rows did not emit its guarded request`);
        }
        const afterFit = await snapshot();
        const fitRequests = afterFit.attachments.flatMap((attachment) => attachment.resizeRequests || []).slice(resizeCount(afterRows));
        assert(fitRequests.length === 1 && fitRequests[0].columns === geometry.columns && Number.isInteger(fitRequests[0].rows),
          `${shape.name}: Fit rows crossed width authority or emitted more than once: ${JSON.stringify(fitRequests)}`);

        const targetColumns = geometry.columns === 300 ? 299 : geometry.columns + 1;
        const refitsBefore = afterFit.refitOperations.length;
        await page.locator('.persea-unified-view-popover input[aria-label="Columns"]').fill(String(targetColumns));
        // the single Apply refits when the typed columns differ.
        await page.locator(".persea-unified-view-popover .persea-unified-size__form").getByRole("button", { name: "Apply", exact: true }).click();
        for (let attempt = 0; attempt < 50; attempt += 1) {
          const state = await snapshot();
          if (state.refitOperations.length === refitsBefore + 1 && state.attachments.at(-1)?.live) break;
          await delay(100);
          if (attempt === 49) throw new Error(`${shape.name}: Columns action did not establish its refit successor`);
        }
        const afterColumns = await snapshot();
        const refit = afterColumns.refitOperations.at(-1);
        assert(refit?.columns === targetColumns && resizeCount(afterColumns) === resizeCount(beforeGeometry) + 2,
          `${shape.name}: geometry actions crossed row/column authority: ${JSON.stringify({ refit, resizes: resizeCount(afterColumns) - resizeCount(beforeGeometry) })}`);
        geometry = { ...geometry, targetRows, targetColumns, rowRequest: rowRequests[0], fitRequest: fitRequests[0], refit, accents };
        if (await page.locator(".persea-unified-view-popover").isVisible()) await page.locator(".persea-unified-view-disclosure").click();
      }

      let switcher = {};
      if (enabled("switcher")) {
        evidence.caseCounts.switcher += 1;
        await page.locator(".persea-unified-tag").click();
        await page.waitForFunction(() => document.querySelectorAll(".persea-session-switcher__row").length >= 2);
        const switchState = await snapshot();
        const secretValues = [...new Set([
          ...Object.values(session.handles),
          ...(switchState.handles || []).map((entry) => entry.handle),
          ...(switchState.offers || []).map((entry) => entry.handle),
        ].filter(Boolean))];
        const ariaText = typeof page.locator("body").ariaSnapshot === "function" ? await page.locator("body").ariaSnapshot() : "";
        switcher = await page.evaluate(({ secretValues, ariaText }) => {
          const groups = [...document.querySelectorAll(".persea-session-switcher__group")].filter((realm) => realm.getClientRects().length > 0).map((realm) => ({
            realm: realm.getAttribute("data-realm") || "",
            heading: realm.querySelector(".persea-session-switcher__group-heading")?.textContent?.trim() || "",
            servers: [...realm.querySelectorAll(".persea-session-switcher__server")].map((server) => ({
              server: server.getAttribute("data-server") || "",
              rows: [...server.querySelectorAll(".persea-session-switcher__row")].map((row) => ({
                name: row.querySelector(".persea-session-switcher__session-name")?.textContent?.trim() || "",
                alias: row.querySelector(".persea-session-switcher__alias")?.textContent?.trim() || "",
                meta: row.querySelector(".persea-session-switcher__meta")?.textContent?.trim() || "",
                state: row.getAttribute("data-state") || "",
                current: row.getAttribute("data-current") || "",
                aria: row.getAttribute("aria-label") || "",
              })),
            })),
          }));
          const attributes = [...document.querySelectorAll("*")].flatMap((node) => ["aria-label", "title", "alt", "placeholder", "value"].map((name) => node.getAttribute(name) || ""));
          const completeSurface = [document.body.innerText, document.body.textContent || "", ariaText, ...attributes].join("\n");
          return {
            groups,
            arbitrary: [...document.querySelectorAll("button")].some((button) => /^(Prev|Next)$/i.test((button.textContent || "").trim())),
            leakedSecrets: secretValues.filter((value) => completeSurface.includes(value)),
          };
        }, { secretValues, ariaText });
        const rows = switcher.groups.flatMap((group) => group.servers.flatMap((server) => server.rows));
        const hierarchy = switcher.groups.map((group) => `${group.realm}:${group.heading}:${group.servers.map((server) => server.server).join(",")}`).join("|");
        assert(hierarchy === "local:Local realm:private|remote:Remote realm:private", `${shape.name}: realm/user→server hierarchy is incomplete: ${JSON.stringify(switcher.groups)}`);
        assert(rows.length === 2 && rows[0].name === "alpha" && rows[0].alias === "primary shell"
          && rows[1].name === "beta" && rows[1].alias === "support shell",
        `${shape.name}: session names/aliases are incomplete: ${JSON.stringify(rows)}`);
        // This older inventory fixture supplies interaction activity only.
        // Output activity must stay unknown rather than borrowing that timer.
        assert(rows.every((row) => /\d+\s*[×x]\s*\d+/.test(row.meta) && /\d+ attached/.test(row.meta)
          && row.meta.includes("Output time unknown") && row.state === "open")
          && rows[0].meta.includes("1 attached") && rows[1].meta.includes("0 attached")
          && rows[0].aria === "Current session alpha · primary shell" && rows[1].aria === "Switch to beta · support shell",
        `${shape.name}: dimensions/attachment/activity/state metadata is dishonest: ${JSON.stringify(rows)}`);
        assert(!switcher.arbitrary && switcher.leakedSecrets.length === 0, `${shape.name}: switcher exposed arbitrary navigation or a capability: ${JSON.stringify(switcher)}`);
      }

      let typography = {};
      if (enabled("typography")) {
        evidence.caseCounts.typography += 1;
        typography = await page.evaluate(() => {
        const selectors = [
          ".persea-unified-select-context", ".persea-unified-toolbar-paste",
          ".persea-unified-view-disclosure", ".persea-unified-quick-actions",
          ".persea-unified-composer-toggle",
        ];
        const values = Object.fromEntries(selectors.map((selector) => {
          const node = document.querySelector(selector);
          if (!(node instanceof HTMLElement)) throw new Error(`missing ordinary toolbar control ${selector}`);
          const style = getComputedStyle(node);
          return [selector, [style.fontFamily, style.fontSize, style.fontWeight, style.minHeight, style.paddingTop, style.paddingRight, style.paddingBottom, style.paddingLeft].join("|")];
        }));
        const identity = getComputedStyle(document.querySelector(".persea-unified-tag__name")).fontFamily;
        return { values, identity };
        });
        assert(new Set(Object.values(typography.values)).size === 1, `${shape.name}: ordinary toolbar token differs: ${JSON.stringify(typography)}`);
        assert(/mono/i.test(typography.identity) && !/mono/i.test(Object.values(typography.values)[0]), `${shape.name}: code face escaped session identity: ${JSON.stringify(typography)}`);
      }

      const screenshot = path.join(EVIDENCE, `${ENGINE}-${shape.name}.png`);
      // A viewport screenshot is the measured posture. WebKit implements a
      // full-page capture by injecting a transient inline stylesheet, which
      // correctly trips this page's CSP and would turn the test tool itself
      // into the only policy violation being measured.
      await page.screenshot({ path: screenshot, animations: "allow", caret: "initial" });
      if (MUTANT === "late-console") await page.evaluate(() => console.error("TERMINAL_TOUCH_MUTANT_LATE_POLICY refused to apply policy"));
      await delay(250);
      await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      const finalState = await snapshot();
      const finalResizes = resizeCount(finalState);
      const expectedResizes = baselineResizes + (enabled("geometry") ? 2 : 0);
      assert(finalResizes === expectedResizes, `${shape.name}: presentation emitted an implicit/unrelated resize: got=${finalResizes} want=${expectedResizes}`);
      const unexpectedErrors = errors.filter((entry) => !(ENGINE === "webkit" && entry.includes("403") && entry.includes("/api/snippets")));
      assert(unexpectedErrors.length === 0, `${shape.name}: browser errors: ${unexpectedErrors.join(" | ")}`);
      const policyErrors = consoleMessages.filter((entry) => /content security policy|refused to|unsafe-inline|unsafe-eval/i.test(entry));
      assert(policyErrors.length === 0, `${shape.name}: browser policy errors: ${policyErrors.join(" | ")}`);
      evidence.cases.push({ shape: shape.name, frozen, focusFlow, geometry, switcher, typography, consoleMessages, resizes: finalResizes });
      evidence.screenshots.push(screenshot);
      await context.close();
    }
  } finally {
    await browser.close();
    await fixture.close();
  }
  assert(evidence.cases.length === SHAPES.length && evidence.cases.length > 0,
    `terminal touch case census mismatch: expected=${SHAPES.length} executed=${evidence.cases.length}`);
  const touchAvailable = SHAPES.some((shape) => shape.touch);
  const requiredCases = CASE === "all"
    ? [...CASES].filter((name) => name !== "all" && (touchAvailable || !["dictation", "longpress", "focus"].includes(name)))
    : [CASE];
  for (const name of requiredCases) assert(evidence.caseCounts[name] > 0, `terminal touch required case executed zero times: ${name}`);
  fs.writeFileSync(path.join(EVIDENCE, `${ENGINE}-terminal_touch.json`), JSON.stringify(evidence, null, 2) + "\n");
  console.log(`TERMINAL_TOUCH_BROWSER_PASS engine=${ENGINE} cases=${evidence.cases.length}`);
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
