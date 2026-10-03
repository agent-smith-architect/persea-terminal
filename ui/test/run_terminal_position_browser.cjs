"use strict";

// Terminal position, proven in a real engine. A grid smaller than its space
// sits where the operator's choice puts it; a grid larger on an axis starts at
// that axis's edge and keeps its whole scroll range. With Center, everything
// that reads the projection — history scrolling, follow-tail, anchors, wheel
// and touch scrolling, taps, long-press and the frozen Select surface — still
// maps to the right rows and columns. The choice is made in the real Settings
// control and must reach an open terminal and workspace panes without a reload.
//
// One engine per process: PERSEA_POSITION_ENGINE=chromium|webkit. Both run
// over TLS because WebKit refuses the fixture's Secure cookies on plain http.

const fs = require("node:fs");
const path = require("node:path");
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || "playwright");
const { startFixture } = require("./unified_reopen_fixture.cjs");
const { startWorkspaceFixture } = require("./workspace_fixture.cjs");

const UI = path.resolve(process.env.PERSEA_POSITION_UI || path.join(__dirname, ".."));
const ENGINE = process.env.PERSEA_POSITION_ENGINE || "chromium";
const CASE = process.env.PERSEA_POSITION_CASE || "all";
const SHOTS = process.env.PERSEA_POSITION_SCREENSHOTS || "";
const CASES = ["placement", "history", "select", "loading", "live", "workspace", "screenshots"];
if (CASE !== "all" && !CASES.includes(CASE)) throw new Error(`unknown terminal position case: ${CASE}`);
if (CASE === "screenshots" && !SHOTS) throw new Error("the screenshots case needs PERSEA_POSITION_SCREENSHOTS");
// Screenshots are review evidence, not a gate: they run only when asked for.
const enabled = (name) => CASE === name || (CASE === "all" && (name !== "screenshots" || SHOTS !== ""));

const POSITIONS = ["top-center", "top-left", "center"];
const LABELS = { "top-center": "Top center", "top-left": "Top left", center: "Center" };
const VIEWPORTS = [
  { name: "desktop", width: 1280, height: 800, touch: false },
  { name: "phone", width: 390, height: 844, touch: true },
];
// At the fixed 14px font: smaller than both viewports, wider, taller, and
// larger on both axes.
const GRIDS = [
  { name: "small", columns: 36, rows: 10 },
  { name: "wide", columns: 200, rows: 10 },
  { name: "tall", columns: 36, rows: 80 },
  { name: "large", columns: 200, rows: 80 },
];
const TOLERANCE = 1;

class PositionFailure extends Error {}
function check(condition, message, detail) {
  if (!condition) throw new PositionFailure(`TERMINAL_POSITION ${message}${detail === undefined ? "" : `: ${JSON.stringify(detail)}`}`);
}
const near = (actual, expected, tolerance = TOLERANCE) => Math.abs(actual - expected) <= tolerance;
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Retries an assertion block until it holds or the deadline passes: placement
// is settled by a reconcile frame after layout, and only the settled state is
// the product's answer. The last failure is the one reported.
async function eventually(block, timeoutMs = 3_000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    try {
      return await block();
    } catch (error) {
      if (!(error instanceof PositionFailure) || Date.now() > deadline) throw error;
      await delay(40);
    }
  }
}

const consoleMessages = [];
function watch(page, label) {
  page.on("console", (message) => consoleMessages.push(`${label} ${message.type()}: ${message.text()}`));
  page.on("pageerror", (error) => consoleMessages.push(`${label} pageerror: ${error}`));
  page.on("response", (response) => { if (response.status() >= 400) consoleMessages.push(`${label} HTTP ${response.status()} ${response.url()}`); });
}
function checkConsole(stage) {
  check(consoleMessages.length === 0, `${stage}: console was not clean`, consoleMessages.splice(0));
}

// Everything relative to the scroller's content box: the space the grid has.
function placement() {
  const shell = document.querySelector(".persea-unified-terminal");
  const scroller = shell.querySelector(".persea-unified-scroll");
  const host = shell.querySelector(".persea-unified-xterm");
  const screen = host.querySelector(".xterm-screen");
  const box = scroller.getBoundingClientRect();
  const originX = box.left + scroller.clientLeft;
  const originY = box.top + scroller.clientTop;
  const hostBox = host.getBoundingClientRect();
  const screenBox = screen.getBoundingClientRect();
  return {
    position: shell.dataset.terminalPosition,
    clientWidth: scroller.clientWidth,
    clientHeight: scroller.clientHeight,
    scrollWidth: scroller.scrollWidth,
    scrollHeight: scroller.scrollHeight,
    scrollLeft: scroller.scrollLeft,
    scrollTop: scroller.scrollTop,
    host: { left: hostBox.left - originX, top: hostBox.top - originY, width: hostBox.width, height: hostBox.height },
    screen: { x: screenBox.left, y: screenBox.top, width: screenBox.width, height: screenBox.height },
    rows: [...screen.querySelectorAll(".xterm-rows > div")].map((row) => (row.textContent || "").replace(/ /g, " ").trimEnd()),
  };
}
const measure = (page) => page.evaluate(placement);
const scrollTo = (page, { top, left }) => page.evaluate(({ top, left }) => {
  const scroller = document.querySelector(".persea-unified-scroll");
  if (top !== undefined) scroller.scrollTop = top;
  if (left !== undefined) scroller.scrollLeft = left;
}, { top, left });

// Where the grid belongs with the scroller at rest on the tail.
function expectedRest(m, position) {
  const freeX = m.clientWidth - m.host.width;
  const freeY = m.clientHeight - m.host.height;
  return {
    left: freeX > 0 ? (position === "top-left" ? 0 : freeX / 2) : -m.scrollLeft,
    // A grid taller than the space rests on the tail with its bottom row at
    // the bottom edge; scrolling to the top brings its first row to the top.
    top: freeY > 0 ? (position === "center" ? Math.floor(freeY / 2) : 0) : freeY,
  };
}

async function launch() {
  return playwright[ENGINE].launch({
    headless: true,
    ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
  });
}

async function openTerminal(page, fixture, api) {
  const inventory = await (await api.get("/api/inventory")).json();
  const session = inventory.realms[0].servers[0].sessions[0];
  const hash = new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000",
    name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" });
  // A URL that differs only in its fragment would not load a new document.
  await page.goto("about:blank");
  await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${hash}`);
  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
}

async function shot(page, name) {
  if (!SHOTS) return;
  fs.mkdirSync(SHOTS, { recursive: true });
  await page.screenshot({ path: path.join(SHOTS, `${ENGINE}-${name}.png`) });
}

// ---------------------------------------------------------------------------
// Placement: every choice, viewport and grid shape, at rest and at both ends
// of each scrollable axis.
async function placementCase(browser, fixture, api, control) {
  for (const viewport of VIEWPORTS) {
    const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height },
      isMobile: viewport.touch, hasTouch: viewport.touch, ignoreHTTPSErrors: true });
    const page = await context.newPage();
    watch(page, `${viewport.name}`);
    try {
      for (const grid of GRIDS) {
        for (const position of POSITIONS) {
          const label = `${viewport.name} ${grid.name} ${grid.columns}x${grid.rows} ${position}`;
          await control({ reset: true, geometryA: { columns: grid.columns, rows: grid.rows }, preferences: { font_size: 14, terminal_position: position } });
          await openTerminal(page, fixture, api);
          const rest = await eventually(async () => {
            const m = await measure(page);
            const want = expectedRest(m, position);
            check(m.position === position, `${label}: shell does not carry the choice`, m.position);
            check(near(m.host.left, want.left), `${label}: grid is not placed horizontally as chosen`, { want, host: m.host, clientWidth: m.clientWidth });
            check(near(m.host.top, want.top), `${label}: grid is not placed vertically as chosen`, { want, host: m.host, clientHeight: m.clientHeight, scrollTop: m.scrollTop });
            return m;
          });
          const narrow = rest.host.width < rest.clientWidth;
          const short = rest.host.height < rest.clientHeight;
          if (narrow) check(rest.scrollWidth <= rest.clientWidth, `${label}: a narrower grid made the scroller scroll sideways`, rest);
          if (short) check(rest.scrollHeight <= rest.clientHeight, `${label}: a shorter grid made the scroller scroll`, rest);
          if (!narrow) {
            // A wider grid starts at the left edge and every column is reachable.
            await scrollTo(page, { left: 0 });
            await eventually(async () => {
              const m = await measure(page);
              check(near(m.host.left, 0), `${label}: wider grid does not start at the left edge`, m.host);
            });
            await scrollTo(page, { left: 1e6 });
            await eventually(async () => {
              const m = await measure(page);
              check(m.scrollLeft > 0 && near(m.host.left + m.host.width, m.clientWidth), `${label}: wider grid's last column is out of reach`, { host: m.host, clientWidth: m.clientWidth, scrollLeft: m.scrollLeft, scrollWidth: m.scrollWidth });
            });
            await scrollTo(page, { left: 0 });
          }
          if (!short) {
            // A taller grid starts at the top edge and every row is reachable.
            await scrollTo(page, { top: 0 });
            await eventually(async () => {
              const m = await measure(page);
              check(near(m.host.top, 0), `${label}: taller grid does not start at the top edge`, { host: m.host, scrollTop: m.scrollTop });
            });
            await scrollTo(page, { top: 1e6 });
            await eventually(async () => {
              const m = await measure(page);
              check(m.scrollTop > 0 && near(m.host.top + m.host.height, m.clientHeight), `${label}: taller grid's last row is out of reach`, { host: m.host, clientHeight: m.clientHeight, scrollTop: m.scrollTop, scrollHeight: m.scrollHeight });
            });
          }
        }
      }
      checkConsole(`placement ${viewport.name}`);
    } finally {
      await context.close();
    }
  }
}

// ---------------------------------------------------------------------------
// Review screenshots, taken only on request: a session with some output on a
// desktop and a phone, and a narrow session in a wide window, for each choice.
// Placement is asserted before each capture.
const SAMPLE = [
  "$ uname -sr", "Linux 6.8.0-146-generic",
  "$ ls", "README.md  deploy  go.mod  internal  ui",
  "$ git log --oneline -3",
  "f17c7aa Put the dashboard first in the session list",
  "11d70fb Check terminal aliases against session inventory",
  "75c8a13 Preserve aliases in seeds and conflict responses",
  "$ ",
].join("\r\n");

async function screenshotCase(browser, fixture, api, control) {
  const shapes = [
    { name: "desktop", width: 1280, height: 800, touch: false, columns: 80, rows: 24 },
    { name: "phone", width: 390, height: 844, touch: true, columns: 40, rows: 20 },
    { name: "wide-window", width: 1920, height: 1080, touch: false, columns: 100, rows: 30 },
  ];
  for (const shape of shapes) {
    const context = await browser.newContext({ viewport: { width: shape.width, height: shape.height },
      isMobile: shape.touch, hasTouch: shape.touch, ignoreHTTPSErrors: true });
    const page = await context.newPage();
    watch(page, `screenshot ${shape.name}`);
    try {
      for (const position of POSITIONS) {
        await control({ reset: true, geometryA: { columns: shape.columns, rows: shape.rows }, preferences: { font_size: 14, terminal_position: position } });
        await openTerminal(page, fixture, api);
        await control({ writeLive: SAMPLE });
        await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("75c8a13"));
        await eventually(async () => {
          const m = await measure(page);
          const want = expectedRest(m, position);
          check(near(m.host.left, want.left) && near(m.host.top, want.top), `screenshot ${shape.name} ${position}: grid misplaced`, { want, host: m.host });
        });
        await shot(page, `${shape.name}-${position}`);
      }
      checkConsole(`screenshots ${shape.name}`);
    } finally {
      await context.close();
    }
  }
}

// ---------------------------------------------------------------------------
// History under Center. The transcript is known line by line, so the row the
// projection shows can be checked against the scroll offset exactly.
function transcriptLine(index) {
  if (index === 0) return "fixture-replay";
  if (index === 1) return "fixture-live";
  return `line-${String(index - 1).padStart(3, "0")} abcdefghijklmnopqrstuvwxyz`;
}

async function historyCase(browser, fixture, api, control, snapshot) {
  const ROWS = 12;
  const facts = [];
  for (const viewport of VIEWPORTS) {
    const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height },
      isMobile: viewport.touch, hasTouch: viewport.touch, ignoreHTTPSErrors: true });
    const page = await context.newPage();
    watch(page, `history ${viewport.name}`);
    try {
      await control({ reset: true, geometryA: { columns: 36, rows: ROWS }, preferences: { font_size: 14, terminal_position: "center" } });
      await openTerminal(page, fixture, api);
      let written = 150;
      await control({ writeLive: Array.from({ length: written }, (_, index) => transcriptLine(index + 2)).join("\r\n") + "\r\n" });
      await page.waitForFunction((needle) => document.querySelector(".xterm-rows")?.textContent?.includes(needle), transcriptLine(written + 1).slice(0, 8));
      // Lines 0..written+1 hold text; the cursor waits on the empty line after.
      const baseY = () => written + 3 - ROWS;
      const lineAt = (index) => (index === written + 2 ? "" : transcriptLine(index));

      // The projection's whole contract for one scroll offset: the rows it
      // paints, and where it paints them.
      const mapping = async (label) => eventually(async () => {
        const m = await measure(page);
        const cell = m.screen.height / ROWS;
        const offset = Math.floor((m.clientHeight - m.host.height) / 2);
        check(offset > cell, `${label}: Center left no band to test against`, { offset, cell });
        const tail = m.scrollTop >= m.scrollHeight - m.clientHeight - 1;
        const row = tail ? baseY() : Math.floor((m.scrollTop + 0.001) / cell);
        check(m.rows[0] === lineAt(row) && m.rows[ROWS - 1] === lineAt(row + ROWS - 1),
          `${label}: painted rows do not match the scroll offset`, { scrollTop: m.scrollTop, cell, row, want: [lineAt(row), lineAt(row + ROWS - 1)], got: [m.rows[0], m.rows[ROWS - 1]] });
        check(near(m.host.top, row * cell + offset - m.scrollTop),
          `${label}: grid is not where the projection puts it`, { hostTop: m.host.top, want: row * cell + offset - m.scrollTop, row, cell, offset, scrollTop: m.scrollTop });
        check(m.host.top >= -TOLERANCE && m.host.top + m.host.height <= m.clientHeight + TOLERANCE,
          `${label}: Center pushed part of the grid out of view`, { host: m.host, clientHeight: m.clientHeight });
        return { ...m, cell, offset, row, tail };
      });
      const scrollSettled = async () => {
        let last = "";
        for (let attempt = 0; attempt < 80; attempt++) {
          const now = await page.evaluate(() => { const s = document.querySelector(".persea-unified-scroll"); return `${s.scrollTop}/${s.scrollHeight}`; });
          if (now === last) return;
          last = now;
          await delay(60);
        }
        throw new PositionFailure("TERMINAL_POSITION scroll did not settle");
      };
      const wheel = async (deltaY) => {
        const m = await measure(page);
        await page.mouse.move(m.screen.x + m.screen.width / 2, m.screen.y + m.screen.height / 2);
        await page.mouse.wheel(0, deltaY);
        await scrollSettled();
      };
      // WebKit caps how far one wheel event scrolls; an edge takes several.
      const wheelToEdge = async (direction) => {
        for (let turn = 0; turn < 40; turn++) {
          const m = await measure(page);
          if (direction < 0 ? m.scrollTop <= 0 : m.scrollTop >= m.scrollHeight - m.clientHeight - 1) return;
          await wheel(direction * 2_000);
        }
        throw new PositionFailure("TERMINAL_POSITION wheel never reached the edge");
      };

      let state = await mapping(`${viewport.name} tail`);
      check(state.tail && near(state.host.top, state.offset), `${viewport.name}: Center does not rest on the tail with the grid centred`, state);
      const restingOffset = state.offset;

      // Wheel through history, landing between rows as well as on them.
      if (!viewport.touch) {
        for (const step of ["top", 37, 211, 1000, -523, "end"]) {
          if (step === "top" || step === "end") await wheelToEdge(step === "top" ? -1 : 1);
          else await wheel(step);
          state = await mapping(`${viewport.name} wheel ${step}`);
          if (step === "top") check(state.row === 0 && near(state.host.top, state.offset), `${viewport.name}: top of history is not centred`, state);
        }
        check(state.tail, `${viewport.name}: wheel to the end did not reach the tail`, state);
      } else {
        await scrollTo(page, { top: 0 });
        await scrollSettled();
        state = await mapping(`${viewport.name} top of history`);
        check(state.row === 0 && near(state.host.top, state.offset), `${viewport.name}: top of history is not centred`, state);
        await scrollTo(page, { top: 1e6 });
        await scrollSettled();
        state = await mapping(`${viewport.name} back to tail`);
      }

      // Inside one row the projection paints the same row and must still hold
      // the band: a scroll that keeps the row schedules no reconcile to lean on.
      for (const within of [1, state.cell - 2]) {
        await scrollTo(page, { top: Math.round(30 * state.cell + within) });
        await scrollSettled();
        const inside = await mapping(`${viewport.name} within row 30 (+${within})`);
        check(inside.row === 30, `${viewport.name}: a scroll inside row 30 painted another row`, { row: inside.row, scrollTop: inside.scrollTop });
      }
      await scrollTo(page, { top: 1e6 });
      await scrollSettled();
      state = await mapping(`${viewport.name} tail again`);

      // Follow-tail: new output keeps the tail in view, still centred.
      written += 1;
      await control({ writeLive: `${transcriptLine(written + 1)}\r\n` });
      await page.waitForFunction((needle) => document.querySelector(".xterm-rows")?.textContent?.includes(needle), transcriptLine(written + 1).slice(0, 8));
      state = await mapping(`${viewport.name} follow-tail`);
      check(state.tail && near(state.host.top, restingOffset), `${viewport.name}: new output did not keep the centred tail in view`, state);

      // Away from the tail, new output leaves the rows in view alone.
      await scrollTo(page, { top: Math.round(state.cell * 40.4) });
      await scrollSettled();
      const reading = await mapping(`${viewport.name} reading history`);
      written += 1;
      await control({ writeLive: `${transcriptLine(written + 1)}\r\n` });
      await page.waitForFunction((height) => document.querySelector(".persea-unified-scroll").scrollHeight > height, reading.scrollHeight);
      state = await mapping(`${viewport.name} output while reading`);
      check(state.row === reading.row && near(state.scrollTop, reading.scrollTop), `${viewport.name}: output while reading moved the rows in view`, { before: reading.row, after: state.row });

      // The anchor holds across a resize that changes the band.
      await page.setViewportSize({ width: viewport.width, height: viewport.height - 140 });
      state = await eventually(async () => {
        const m = await mapping(`${viewport.name} shorter window`);
        check(m.offset < restingOffset - 30, `${viewport.name}: the shorter window did not shrink the band`, { offset: m.offset, restingOffset });
        return m;
      });
      check(state.row === reading.row, `${viewport.name}: a resize moved the anchored row`, { before: reading.row, after: state.row });
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      state = await mapping(`${viewport.name} window restored`);
      check(state.row === reading.row, `${viewport.name}: restoring the window moved the anchored row`, { before: reading.row, after: state.row });

      if (viewport.touch) {
        // Touch scrolling, natively, from the grid and from the band below it.
        const session = ENGINE === "chromium" ? await context.newCDPSession(page) : undefined;
        for (const origin of ["grid", "band"]) {
          const before = await mapping(`${viewport.name} before ${origin} drag`);
          if (session) {
            const x = before.screen.x + before.screen.width / 2;
            const y = origin === "grid"
              ? before.screen.y + before.screen.height * 0.3
              : before.screen.y + before.screen.height + (before.clientHeight - before.host.top - before.host.height) / 2;
            await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y, radiusX: 1, radiusY: 1, force: 1 }] });
            for (const step of [20, 50, 90, 130]) {
              await session.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x, y: y + step, radiusX: 1, radiusY: 1, force: 1 }] });
              await delay(30);
            }
            await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
          } else {
            // WebKit has no trusted touch drag to synthesize, and mobile
            // WebKit no wheel: move the scroller natively by script, which
            // reaches the projection through the same scroll event.
            await page.evaluate(() => document.querySelector(".persea-unified-scroll").scrollBy(0, -130));
          }
          await scrollSettled();
          const after = await mapping(`${viewport.name} after ${origin} drag`);
          check(after.scrollTop < before.scrollTop - before.cell, `${viewport.name}: a drag from the ${origin} did not scroll history`, { before: before.scrollTop, after: after.scrollTop });
          facts.push(`${viewport.name} ${origin} drag via ${session ? "trusted touch" : "scripted scroll"}: ${before.scrollTop}->${after.scrollTop}`);
        }

        // Long-press selects exactly the cell under the finger, in history,
        // with the grid centred: the last two digits pin the row, the letters
        // pin the column.
        if (await page.evaluate(() => matchMedia("(pointer: coarse)").matches)) {
          state = await mapping(`${viewport.name} before long-press`);
          for (const [visibleRow, column] of [[3, 7], [8, 6], [5, 14]]) {
            const expected = lineAt(state.row + visibleRow)[column];
            const x = state.screen.x + (column + 0.5) * state.screen.width / 36;
            const y = state.screen.y + (visibleRow + 0.5) * state.cell;
            await page.mouse.move(x, y);
            await page.mouse.down();
            try {
              await page.locator(".persea-unified-select").waitFor({ state: "visible", timeout: 2_000 });
            } finally {
              await page.mouse.up();
            }
            const selected = await page.evaluate(() => String(getSelection()));
            check(selected === expected, `${viewport.name}: long-press selected the wrong cell`, { visibleRow, column, selected, expected, row: state.row + visibleRow });
            await page.keyboard.press("Escape");
            await page.locator(".persea-unified-select").waitFor({ state: "hidden" });
            facts.push(`${viewport.name} long-press row ${state.row + visibleRow} column ${column}: ${JSON.stringify(selected)}`);
          }
        } else {
          facts.push(`${viewport.name} long-press: no coarse pointer in this engine`);
        }
      }

      // Entering Select freezes history where it is shown, band included: at
      // the tail and on a row boundary inside history, frozen text sits on
      // the live text.
      for (const where of ["tail", "row 40"]) {
        await scrollTo(page, { top: where === "tail" ? 1e6 : Math.round(40 * state.cell) });
        await scrollSettled();
        const live = await mapping(`${viewport.name} before Select at ${where}`);
        const needle = lineAt(live.row + 4).slice(0, 8);
        const livePoint = { x: live.screen.x, y: live.screen.y + 4 * live.cell };
        await page.locator(".persea-unified-select-context").click();
        await page.locator(".persea-unified-select").waitFor({ state: "visible" });
        const frozen = await page.evaluate((needle) => {
          const row = [...document.querySelectorAll(".persea-unified-select__row")].find((node) => node.textContent.startsWith(needle));
          const node = row && document.createTreeWalker(row, NodeFilter.SHOW_TEXT).nextNode();
          if (!node) return undefined;
          const range = document.createRange();
          range.setStart(node, 0);
          range.setEnd(node, 1);
          return { x: range.getBoundingClientRect().left, y: row.getBoundingClientRect().top };
        }, needle);
        check(frozen && near(frozen.x, livePoint.x) && near(frozen.y, livePoint.y), `${viewport.name}: entering Select at ${where} moved the text`, { live: livePoint, frozen, needle });
        await page.keyboard.press("Escape");
        await page.locator(".persea-unified-select").waitFor({ state: "hidden" });
      }

      // A click lands on the cell under the pointer: xterm reports the cell
      // it hit once mouse reporting is on.
      await scrollTo(page, { top: 1e6 });
      await scrollSettled();
      state = await mapping(`${viewport.name} before click`);
      await control({ writeLive: "\u001b[?1000h\u001b[?1006h" });
      await delay(100);
      const inputsBefore = (await snapshot()).attachments.at(-1).inputs.length;
      const cellWidth = state.screen.width / 36;
      const target = { column: 9, row: 4 };
      const point = { x: state.screen.x + (target.column + 0.5) * cellWidth, y: state.screen.y + (target.row + 0.5) * state.cell };
      if (viewport.touch) await page.touchscreen.tap(point.x, point.y);
      else await page.mouse.click(point.x, point.y);
      const report = `\u001b[<0;${target.column + 1};${target.row + 1}M`;
      await eventually(async () => {
        const inputs = (await snapshot()).attachments.at(-1).inputs.slice(inputsBefore).join("");
        check(inputs.includes(report), `${viewport.name}: a click on a centred grid reported the wrong cell`, { inputs, report });
      });
      facts.push(`${viewport.name} ${viewport.touch ? "tap" : "click"} reported ${JSON.stringify(report)}`);
      await control({ writeLive: "\u001b[?1000l\u001b[?1006l" });
      checkConsole(`history ${viewport.name}`);
    } finally {
      await context.close();
    }
  }
  for (const fact of facts) console.log(`terminal position ${ENGINE}: ${fact}`);
}

// ---------------------------------------------------------------------------
// Select freezes the screen where it is: entering it moves no text, on any
// choice, for a grid smaller than its space and one wider.
async function selectCase(browser, fixture, api, control) {
  for (const viewport of VIEWPORTS) {
    const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height },
      isMobile: viewport.touch, hasTouch: viewport.touch, ignoreHTTPSErrors: true });
    const page = await context.newPage();
    watch(page, `select ${viewport.name}`);
    try {
      for (const grid of [{ columns: 36, rows: 10 }, { columns: 200, rows: 10 }]) {
        for (const position of POSITIONS) {
          const label = `${viewport.name} ${grid.columns}x${grid.rows} ${position}`;
          await control({ reset: true, geometryA: grid, preferences: { font_size: 14, terminal_position: position } });
          await openTerminal(page, fixture, api);
          await control({ writeLive: "\r\nfirst MARK-SELECT row\r\n$ " });
          await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("MARK-SELECT"));
          const live = await eventually(async () => {
            const m = await measure(page);
            const want = expectedRest(m, position);
            check(near(m.host.left, want.left) && near(m.host.top, want.top), `${label}: grid misplaced before Select`, { want, host: m.host });
            return page.evaluate(() => {
              const rows = [...document.querySelectorAll(".xterm-rows > div")];
              const index = rows.findIndex((row) => row.textContent.includes("MARK-SELECT"));
              const screen = document.querySelector(".xterm-screen").getBoundingClientRect();
              const columns = Number.parseInt(document.querySelector(".persea-unified-view-disclosure")?.textContent || "", 10);
              const column = rows[index].textContent.indexOf("MARK-SELECT");
              return { x: screen.left + column * screen.width / columns, y: rows[index].getBoundingClientRect().top };
            });
          });
          await page.locator(".persea-unified-select-context").click();
          await page.locator(".persea-unified-select").waitFor({ state: "visible" });
          const frozen = await page.evaluate(() => {
            const row = [...document.querySelectorAll(".persea-unified-select__row")].find((node) => node.textContent.includes("MARK-SELECT"));
            const walker = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
            for (let node = walker.nextNode(); node; node = walker.nextNode()) {
              const index = node.data.indexOf("MARK-SELECT");
              if (index < 0) continue;
              const range = document.createRange();
              range.setStart(node, index);
              range.setEnd(node, index + 1);
              const box = range.getBoundingClientRect();
              return { x: box.left, y: row.getBoundingClientRect().top };
            }
            return undefined;
          });
          check(frozen && near(frozen.x, live.x) && near(frozen.y, live.y), `${label}: entering Select moved the text`, { live, frozen });
          await page.keyboard.press("Escape");
          await page.locator(".persea-unified-select").waitFor({ state: "hidden" });
        }
      }
      checkConsole(`select ${viewport.name}`);
    } finally {
      await context.close();
    }
  }
}

// ---------------------------------------------------------------------------
// The replay notice sits where the terminal will appear.
async function loadingCase(browser, fixture, api, control) {
  for (const viewport of VIEWPORTS) {
    const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height },
      isMobile: viewport.touch, hasTouch: viewport.touch, ignoreHTTPSErrors: true });
    const page = await context.newPage();
    watch(page, `loading ${viewport.name}`);
    try {
      for (const position of POSITIONS) {
        const label = `${viewport.name} ${position}`;
        // The hold outlasts the bounded retry below, so a misplaced notice is
        // reported as misplaced rather than as gone.
        await control({ reset: true, holdPrepareMs: 4_000, preferences: { font_size: 14, terminal_position: position } });
        const inventory = await (await api.get("/api/inventory")).json();
        const session = inventory.realms[0].servers[0].sessions[0];
        const hash = new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000",
          name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" });
        await page.goto("about:blank");
        await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${hash}`);
        await page.locator(".persea-unified-loading").waitFor({ state: "visible" });
        const box = await eventually(async () => {
          const m = await page.evaluate(() => {
            const panel = document.querySelector(".persea-unified-loading");
            const stage = panel.getBoundingClientRect();
            const identity = panel.querySelector(".persea-unified-loading__identity").getBoundingClientRect();
            const detail = panel.querySelector(".persea-unified-loading__detail").getBoundingClientRect();
            return {
              position: document.querySelector(".persea-unified-terminal").dataset.terminalPosition,
              visible: !panel.hidden,
              stage: { left: stage.left, top: stage.top, width: stage.width, height: stage.height },
              content: { left: Math.min(identity.left, detail.left), right: Math.max(identity.right, detail.right), top: identity.top, bottom: detail.bottom },
            };
          });
          const padding = 16;
          const centreX = (m.content.left + m.content.right) / 2;
          const centreY = (m.content.top + m.content.bottom) / 2;
          check(m.visible && m.position === position, `${label}: replay notice not showing the choice`, m);
          if (position === "top-left") check(near(m.content.left, m.stage.left + padding) && near(m.content.top, m.stage.top + padding), `${label}: replay notice is not top left`, m);
          if (position === "top-center") check(near(centreX, m.stage.left + m.stage.width / 2) && near(m.content.top, m.stage.top + padding), `${label}: replay notice is not top center`, m);
          if (position === "center") check(near(centreX, m.stage.left + m.stage.width / 2) && near(centreY, m.stage.top + m.stage.height / 2), `${label}: replay notice is not centred`, m);
          return m;
        }, 1_500);
        if (SHOTS && position === "center") await shot(page, `${viewport.name}-loading-center`);
        check(await page.locator(".persea-unified-loading").isVisible(), `${label}: replay notice was gone before it was measured`, box);
      }
      checkConsole(`loading ${viewport.name}`);
    } finally {
      await context.close();
    }
  }
}

// ---------------------------------------------------------------------------
// Settings → Appearance → Terminal position reaches an open terminal live:
// same document, rows in view kept.
async function liveCase(browser, fixture, api, control, snapshot) {
  for (const viewport of VIEWPORTS) {
    const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height },
      isMobile: viewport.touch, hasTouch: viewport.touch, ignoreHTTPSErrors: true });
    const terminal = await context.newPage();
    watch(terminal, `live ${viewport.name} terminal`);
    try {
      await control({ reset: true, geometryA: { columns: 36, rows: 10 }, preferences: { font_size: 14, terminal_position: "top-center" } });
      await openTerminal(terminal, fixture, api);
      await control({ writeLive: Array.from({ length: 60 }, (_, index) => transcriptLine(index + 2)).join("\r\n") + "\r\n" });
      await terminal.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("line-060"));
      await terminal.evaluate(() => { window.__positionDocument = "kept"; });
      // Read history while the setting changes: the rows in view must stay.
      const cell = (await measure(terminal)).screen.height / 10;
      await scrollTo(terminal, { top: Math.round(cell * 20) });
      const anchored = await eventually(async () => {
        const m = await measure(terminal);
        check(m.rows[0] === transcriptLine(20), `live ${viewport.name}: could not park on history row 20`, m.rows[0]);
        return m;
      });

      const dashboard = await context.newPage();
      watch(dashboard, `live ${viewport.name} dashboard`);
      await dashboard.goto(`${fixture.origin}/`);
      await dashboard.getByRole("button", { name: "Settings" }).click();
      await dashboard.locator(".dashboard-appearance__position").waitFor();
      check(await dashboard.getByLabel("Top center", { exact: true }).isChecked(), `live ${viewport.name}: Settings does not show the stored choice`);
      if (viewport.name === "desktop") {
        if (SHOTS) await dashboard.locator(".dashboard-appearance").first().screenshot({ path: path.join(SHOTS, `${ENGINE}-settings-desktop.png`) });
      } else if (SHOTS) {
        await dashboard.locator(".dashboard-appearance").first().screenshot({ path: path.join(SHOTS, `${ENGINE}-settings-phone.png`) });
      }

      for (const position of ["center", "top-left", "top-center"]) {
        await dashboard.getByLabel(LABELS[position], { exact: true }).check();
        await eventually(async () => {
          const operation = (await snapshot()).preferenceOperations.at(-1);
          check(operation?.terminal_position === position, `live ${viewport.name}: Settings did not save ${position}`, operation);
        });
        await eventually(async () => {
          const m = await measure(terminal);
          const want = expectedRest({ ...m, host: m.host }, position);
          const offset = position === "center" ? Math.floor((m.clientHeight - m.host.height) / 2) : 0;
          check(m.position === position, `live ${viewport.name}: the open terminal did not take ${position}`, m.position);
          check(near(m.host.left, want.left), `live ${viewport.name}: the open terminal is not placed horizontally for ${position}`, { want, host: m.host, clientWidth: m.clientWidth });
          check(near(m.host.top, 20 * cell + offset - m.scrollTop), `live ${viewport.name}: the open terminal is not placed vertically for ${position}`, { host: m.host, scrollTop: m.scrollTop, offset });
          check(m.rows[0] === anchored.rows[0], `live ${viewport.name}: changing the position moved the rows in view`, { before: anchored.rows[0], after: m.rows[0] });
        }, 5_000);
        check(await terminal.evaluate(() => window.__positionDocument === "kept"), `live ${viewport.name}: the terminal reloaded to apply ${position}`);
        if (SHOTS && viewport.name === "desktop" && position === "center") {
          await dashboard.locator(".dashboard-appearance").first().screenshot({ path: path.join(SHOTS, `${ENGINE}-settings-desktop-center.png`) });
        }
      }
      checkConsole(`live ${viewport.name}`);
    } finally {
      await context.close();
    }
  }
}

// ---------------------------------------------------------------------------
// Workspace panes follow the setting like the single terminal does.
async function workspaceCase(browser) {
  const fixture = await startWorkspaceFixture(UI, { tls: true, playwrightScreenshotStyle: SHOTS !== "" });
  const api = await playwright.request.newContext({ baseURL: fixture.origin, ignoreHTTPSErrors: true });
  const control = async (data) => (await api.post("/__fixture/control", { data })).json();
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, ignoreHTTPSErrors: true });
  try {
    const names = ["ws01", "ws02"];
    const tree = { kind: "split", direction: "row", weights: [1, 1],
      children: names.map((name) => ({ kind: "leaf", session: { realm: "local", server: "private", name }, on_missing: "offer" })) };
    await control({ reset: true, sessions: names, workspace: { name: "ops", tree }, preferences: { font_size: 14, terminal_position: "top-center" } });
    for (const name of names) await control({ session: name, columns: 30, rows: 8 });
    const workspace = await context.newPage();
    watch(workspace, "workspace");
    await workspace.goto(`${fixture.origin}/workspace?engine=unified-dev#name=ops`);
    await workspace.getByRole("button", { name: /^Open workspace/ }).click();
    await workspace.waitForFunction((count) => document.querySelectorAll('.ws-cell[data-ws-state="live"] .xterm-rows').length === count, names.length);
    await workspace.evaluate(() => { window.__positionDocument = "kept"; });
    const panes = () => workspace.evaluate(() => [...document.querySelectorAll(".ws-cell .persea-unified-terminal")].map((shell) => {
      const scroller = shell.querySelector(".persea-unified-scroll");
      const box = scroller.getBoundingClientRect();
      const host = shell.querySelector(".persea-unified-xterm").getBoundingClientRect();
      return { position: shell.dataset.terminalPosition, clientWidth: scroller.clientWidth, clientHeight: scroller.clientHeight, scrollLeft: scroller.scrollLeft,
        host: { left: host.left - box.left - scroller.clientLeft, top: host.top - box.top - scroller.clientTop, width: host.width, height: host.height } };
    }));
    const expectPanes = (position) => eventually(async () => {
      const all = await panes();
      check(all.length === names.length, "workspace: panes missing", all);
      for (const [index, pane] of all.entries()) {
        const want = expectedRest(pane, position);
        check(pane.position === position && near(pane.host.left, want.left) && near(pane.host.top, want.top),
          `workspace pane ${index}: not placed for ${position}`, { want, pane });
        check(pane.host.width < pane.clientWidth && pane.host.height < pane.clientHeight, `workspace pane ${index}: grid is not smaller than its pane`, pane);
      }
    }, 5_000);
    await expectPanes("top-center");

    const dashboard = await context.newPage();
    watch(dashboard, "workspace dashboard");
    await dashboard.goto(`${fixture.origin}/`);
    await dashboard.getByRole("button", { name: "Settings" }).click();
    for (const position of ["center", "top-left", "top-center"]) {
      await dashboard.getByLabel(LABELS[position], { exact: true }).check();
      await expectPanes(position);
      if (position === "center") await shot(workspace, "workspace-center");
    }
    check(await workspace.evaluate(() => window.__positionDocument === "kept"), "workspace: the workspace reloaded to apply the position");
    checkConsole("workspace");
  } finally {
    await context.close();
    await api.dispose();
    await fixture.close();
  }
}

async function main() {
  if (!playwright[ENGINE]) throw new Error(`Playwright has no ${ENGINE} engine`);
  const browser = await launch();
  // The gate runs under the exact document policy; only a screenshot run
  // authorizes Playwright's no-op capture style.
  const fixture = await startFixture(UI, { tls: true, playwrightScreenshotStyle: SHOTS !== "" });
  const api = await playwright.request.newContext({ baseURL: fixture.origin, ignoreHTTPSErrors: true });
  const control = async (data) => (await api.post("/__fixture/control", { data })).json();
  const snapshot = async () => (await api.get("/__fixture/control")).json();
  try {
    if (enabled("placement")) { await placementCase(browser, fixture, api, control); console.log(`terminal position ${ENGINE}: placement PASS`); }
    if (enabled("history")) { await historyCase(browser, fixture, api, control, snapshot); console.log(`terminal position ${ENGINE}: history, touch and tap PASS`); }
    if (enabled("select")) { await selectCase(browser, fixture, api, control); console.log(`terminal position ${ENGINE}: select PASS`); }
    if (enabled("loading")) { await loadingCase(browser, fixture, api, control); console.log(`terminal position ${ENGINE}: loading PASS`); }
    if (enabled("live")) { await liveCase(browser, fixture, api, control, snapshot); console.log(`terminal position ${ENGINE}: live Settings PASS`); }
    if (enabled("workspace")) { await workspaceCase(browser); console.log(`terminal position ${ENGINE}: workspace PASS`); }
    if (enabled("screenshots")) { await screenshotCase(browser, fixture, api, control); console.log(`terminal position ${ENGINE}: screenshots written to ${SHOTS}`); }
    console.log(`terminal position browser ${ENGINE}: PASS`);
  } finally {
    await api.dispose();
    await browser.close();
    await fixture.close();
  }
}

main().catch((error) => {
  console.error(error instanceof Error ? error.stack || error.message : error);
  process.exitCode = 1;
});
