"use strict";
// Workspace layout shell browser gate.
//
// Serves the harness under the BASE nonce'd CSP (no style-src-attr: the shell
// is class-based and must not need the unified route's relaxation), mounts six
// dummy cells, and runs the layout storm: divider drags (trusted pointer
// input), +/- nudges, keyboard nudges, cell resizes, window resizes, and an
// orientation change. After every step it asserts zero calls into the stub
// send port (the geometry sentinel), zero WebSocket constructions, zero CSP
// violations, six cells, and no blank cell in any pane state. Then: the
// positive control (one explicit fit → exactly one RESIZE_REQUEST), coarse
// pointer touch targets ≥ 44 px at tablet width, and the honest phone-class
// state (zero cells, per-leaf single-terminal links, zero transports).
//
// Engine: Chromium over CDP by default (PERSEA_WS_LAYOUT_ENGINE=chromium).
// WebKit runs through Playwright when PERSEA_WS_LAYOUT_ENGINE=webkit and
// PERSEA_PLAYWRIGHT_MODULE names an absolute Playwright module path.
const fs = require("fs");
const http = require("http");
const net = require("net");
const os = require("os");
const path = require("path");
const childProcess = require("child_process");

const UI = path.resolve(__dirname, "..");
const STYLE_NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'nonce-" + STYLE_NONCE + "'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'";
const ENGINE = process.env.PERSEA_WS_LAYOUT_ENGINE || "chromium";
// F-1: how much of a live terminal a status strip may cover. One line plus its
// padding is about 28px; the cap leaves room for a larger default font without
// letting the strip creep over the key bar.
const NOTICE_MAX_PX = 48;
const NOTICE_MAX_SHARE = 0.25;
const EVIDENCE_DIR = process.env.PERSEA_WS_LAYOUT_EVIDENCE_DIR || fs.mkdtempSync(path.join(os.tmpdir(), "persea-ws-layout-evidence-"));
const PROFILE_ROOT = process.env.PERSEA_WS_LAYOUT_PROFILE_ROOT || os.tmpdir();
const DESKTOP = { width: 1280, height: 800 };
const TABLET = { width: 1024, height: 768 };
const PHONE = { width: 390, height: 844 };
const TOUCH_TARGET = 44;

function assert(value, message) { if (!value) throw new Error(message); }
function delay(ms) { return new Promise((resolve) => setTimeout(resolve, ms)); }
async function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => { const { port } = server.address(); server.close(() => resolve(port)); });
  });
}
function requestJSON(url, method = "GET") {
  return new Promise((resolve, reject) => {
    const request = http.request(url, { method }, (response) => {
      let body = ""; response.setEncoding("utf8");
      response.on("data", (chunk) => { body += chunk; });
      response.on("end", () => { try { resolve(JSON.parse(body)); } catch (error) { reject(error); } });
    });
    request.on("error", reject); request.end();
  });
}
function browserBinary() {
  const candidate = require("./browser_path.cjs")();
  assert(candidate && path.isAbsolute(candidate), "set CHROME_BIN to an absolute Chromium-family browser executable");
  fs.accessSync(candidate, fs.constants.X_OK);
  return candidate;
}

class CDP {
  constructor(url) { this.url = url; this.next = 1; this.pending = new Map(); }
  async open() {
    this.ws = new WebSocket(this.url);
    await new Promise((resolve, reject) => { this.ws.addEventListener("open", resolve, { once: true }); this.ws.addEventListener("error", reject, { once: true }); });
    this.ws.addEventListener("message", (event) => {
      const message = JSON.parse(String(event.data));
      if (!message.id) return;
      const pending = this.pending.get(message.id);
      if (!pending) return;
      this.pending.delete(message.id);
      if (message.error) pending.reject(new Error(message.error.message)); else pending.resolve(message.result);
    });
  }
  send(method, params = {}) { const id = this.next++; return new Promise((resolve, reject) => { this.pending.set(id, { resolve, reject }); this.ws.send(JSON.stringify({ id, method, params })); }); }
  close() { this.ws?.close(); }
}

// --- Drivers: a minimal common surface over CDP-Chromium and Playwright.
// `session({ viewport, coarse, orientation })` yields a page on the harness
// origin with that environment applied; navigation is fresh per session so
// both engines (Playwright contexts are immutable on touch) behave the same.

async function chromiumDriver(origin) {
  const debugPort = await freePort();
  const profile = fs.mkdtempSync(path.join(PROFILE_ROOT, "persea-ws-layout-chromium-"));
  const chrome = childProcess.spawn(browserBinary(), ["--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", `--remote-debugging-port=${debugPort}`, `--user-data-dir=${profile}`, "--no-first-run", "--no-default-browser-check", "about:blank"], { stdio: ["ignore", "ignore", "pipe"] });
  const stderr = []; let exit = null;
  chrome.stderr.on("data", (chunk) => { if (stderr.length < 64) stderr.push(String(chunk)); });
  chrome.on("exit", (code, signal) => { exit = { code, signal }; });
  let target;
  for (let attempt = 0; attempt < 300 && !exit; attempt += 1) {
    try { target = await requestJSON(`http://127.0.0.1:${debugPort}/json/new?about:blank`, "PUT"); break; } catch { await delay(100); }
  }
  assert(target?.webSocketDebuggerUrl, `Chrome target unavailable: ${JSON.stringify({ exit, stderr: stderr.join("").slice(-2000) })}`);
  const cdp = new CDP(target.webSocketDebuggerUrl);
  await cdp.open();
  await cdp.send("Runtime.enable"); await cdp.send("Page.enable"); await cdp.send("Network.enable");
  const evaluate = async (expression) => {
    const result = await cdp.send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text || "evaluation failed");
    return result.result.value;
  };
  const applyEnvironment = async ({ viewport, coarse, orientation }) => {
    await cdp.send("Emulation.setDeviceMetricsOverride", { width: viewport.width, height: viewport.height, deviceScaleFactor: 1, mobile: Boolean(coarse), ...(orientation ? { screenOrientation: { type: orientation, angle: orientation.startsWith("portrait") ? 0 : 90 } } : {}) });
    await cdp.send("Emulation.setTouchEmulationEnabled", { enabled: Boolean(coarse), maxTouchPoints: coarse ? 5 : 1 });
    await cdp.send("Emulation.setEmulatedMedia", { features: coarse ? [{ name: "pointer", value: "coarse" }, { name: "any-pointer", value: "coarse" }, { name: "hover", value: "none" }] : [{ name: "pointer", value: "fine" }, { name: "any-pointer", value: "fine" }, { name: "hover", value: "hover" }] });
  };
  return {
    name: "chromium",
    async session(env) {
      await applyEnvironment(env);
      await cdp.send("Page.navigate", { url: `${origin}/` });
      const deadline = Date.now() + 10_000;
      while (Date.now() < deadline) { try { if (await evaluate("document.body?.dataset.wsHarnessReady === 'true'")) break; } catch {} await delay(25); }
      assert(await evaluate("document.body?.dataset.wsHarnessReady === 'true'"), "harness did not become ready");
      return {
        evaluate,
        setViewport: async (viewport, orientation) => { await applyEnvironment({ ...env, viewport, orientation }); await delay(80); },
        // A held button must ride every move (buttons: 1) or the browser
        // treats the move as buttonless and implicitly releases pointer
        // capture — exactly what Playwright's mouse does after mouse.down().
        mouse: (() => {
          let held = false;
          return {
            move: (x, y) => cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x, y, ...(held ? { button: "left", buttons: 1 } : {}) }),
            down: async (x, y) => { held = true; await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", buttons: 1, clickCount: 1 }); },
            up: async (x, y) => { held = false; await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", buttons: 0, clickCount: 1 }); },
          };
        })(),
        key: async (key) => {
          const codes = { ArrowRight: 39, ArrowDown: 40, ArrowLeft: 37, ArrowUp: 38 };
          await cdp.send("Input.dispatchKeyEvent", { type: "keyDown", key, code: key, windowsVirtualKeyCode: codes[key], nativeVirtualKeyCode: codes[key] });
          await cdp.send("Input.dispatchKeyEvent", { type: "keyUp", key, code: key, windowsVirtualKeyCode: codes[key], nativeVirtualKeyCode: codes[key] });
        },
        screenshot: async (file) => { const shot = await cdp.send("Page.captureScreenshot", { format: "png" }); fs.writeFileSync(file, Buffer.from(shot.data, "base64")); },
      };
    },
    async close() {
      try { cdp.close(); } catch {}
      chrome.kill("SIGTERM");
      await new Promise((resolve) => { const timer = setTimeout(() => { chrome.kill("SIGKILL"); resolve(); }, 5000); chrome.once("exit", () => { clearTimeout(timer); resolve(); }); if (exit) { clearTimeout(timer); resolve(); } });
      fs.rmSync(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
    },
  };
}

async function playwrightDriver(origin, engine) {
  const modulePath = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
  assert(modulePath && path.isAbsolute(modulePath), "PERSEA_PLAYWRIGHT_MODULE must be an absolute Playwright module path for the webkit engine");
  const playwright = require(modulePath);
  const browserType = playwright[engine];
  assert(browserType, `Playwright module has no ${engine} engine`);
  const browser = await browserType.launch({ headless: true });
  let context; let page;
  return {
    name: engine,
    async session(env) {
      if (context) await context.close();
      context = await browser.newContext({ viewport: env.viewport, hasTouch: Boolean(env.coarse), isMobile: Boolean(env.coarse), deviceScaleFactor: 1 });
      page = await context.newPage();
      await page.goto(`${origin}/`, { waitUntil: "load" });
      await page.waitForFunction(() => document.body?.dataset.wsHarnessReady === "true", null, { timeout: 10_000 });
      return {
        evaluate: (expression) => page.evaluate(expression),
        setViewport: async (viewport) => { await page.setViewportSize(viewport); await delay(80); },
        mouse: { move: (x, y) => page.mouse.move(x, y), down: () => page.mouse.down(), up: () => page.mouse.up() },
        key: (key) => page.keyboard.press(key),
        screenshot: (file) => page.screenshot({ path: file }),
      };
    },
    async close() { await context?.close(); await browser.close(); },
  };
}

async function main() {
  fs.mkdirSync(EVIDENCE_DIR, { recursive: true });
  const index = fs.readFileSync(path.join(UI, "test/workspace_layout_browser_harness.html"), "utf8").replace("__PERSEA_STYLE_NONCE__", STYLE_NONCE);
  const files = {
    "/workspace_layout_harness.js": path.join(UI, "dist/test/workspace_layout_browser_harness.js"),
    "/workspace_layout_harness.css": path.join(UI, "dist/test/workspace_layout_browser_harness.css"),
    "/harness.css": path.join(UI, "test/workspace_layout_browser_harness.css"),
  };
  for (const file of Object.values(files)) assert(fs.existsSync(file), `missing harness asset ${file} (build the harness first)`);
  const requests = [];
  const server = http.createServer((request, response) => {
    const url = new URL(request.url, "http://localhost");
    requests.push(url.pathname);
    response.setHeader("Content-Security-Policy", CSP);
    if (url.pathname === "/") { response.setHeader("Content-Type", "text/html"); response.end(index); return; }
    const file = files[url.pathname];
    if (!file) { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", file.endsWith(".css") ? "text/css" : "text/javascript");
    response.end(fs.readFileSync(file));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const driver = ENGINE === "chromium" ? await chromiumDriver(origin) : await playwrightDriver(origin, ENGINE);
  const evidence = { engine: driver.name, steps: [], evidenceDir: EVIDENCE_DIR, screenshots: {} };
  try {
    // ---- Desktop: layout storm and pane-state visibility.
    let page = await driver.session({ viewport: DESKTOP, coarse: false });
    const audit = () => page.evaluate("window.__wsHarness.audit()");
    const check = (label, a, expectations = {}) => {
      const expectCells = expectations.cells ?? 6;
      assert(a.cellCount === expectCells, `${label}: expected ${expectCells} cells, saw ${a.cellCount}`);
      // A deferral notice may describe the terminal; it may not hide it. The
      // strip is out of flow over the mount, so an unbounded one grows by
      // wrapping as the cell narrows and buries the key bar underneath.
      for (const cover of a.noticeCover ?? []) {
        assert(cover.noticeHeight !== null && cover.mountHeight !== null,
          `${label}: a live cell carries a detail with no notice to measure ${JSON.stringify(cover)}`);
        assert(cover.noticeHeight <= NOTICE_MAX_PX,
          `${label}: the deferral notice is ${cover.noticeHeight}px tall, over the ${NOTICE_MAX_PX}px cap ${JSON.stringify(cover)}`);
        assert(cover.share !== null && cover.share <= NOTICE_MAX_SHARE,
          `${label}: the deferral notice covers ${(cover.share * 100).toFixed(1)}% of the live terminal ${JSON.stringify(cover)}`);
      }
      assert(a.blanks.length === 0, `${label}: blank cells ${JSON.stringify(a.blanks)}`);
      assert(a.sentinel.resizeRequests === 0 && a.sentinel.portCalls.length === 0, `${label}: layout reached the send port ${JSON.stringify(a.sentinel.portCalls)}`);
      assert(a.sentinel.webSockets === 0, `${label}: a transport was constructed`);
      assert(a.sentinel.cspViolations.length === 0, `${label}: CSP violations ${JSON.stringify(a.sentinel.cspViolations)}`);
      assert(a.styleAttributes === 0, `${label}: ${a.styleAttributes} element(s) carry a style attribute`);
      for (const split of a.weights) assert(split.weights.every((w) => Number.isInteger(w) && w >= 1 && w <= 100), `${label}: weight class out of range ${JSON.stringify(split)}`);
      evidence.steps.push({ label, cells: a.cellCount, states: a.states, weights: a.weights, sentinel: a.sentinel, treeChanges: a.treeChanges, resizeObserverFires: a.sentinel.resizeObserverFires });
    };
    let a = await audit();
    assert(a.posture === "desktop" && a.coarse === false, `desktop posture expected: ${JSON.stringify({ posture: a.posture, coarse: a.coarse })}`);
    check("initial render", a);
    assert(a.dividers.length === 5, `six panes in this tree need five dividers, saw ${a.dividers.length}`);
    const initialWeights = JSON.stringify(a.weights);

    // Divider drags: trusted pointer input from the divider centre to 30 % of
    // the pair extent in each direction.
    for (const target of a.dividers) {
      const horizontal = target.orientation === "vertical"; // vertical line = row split = drag on x
      for (const sign of [1, -1]) {
        const fresh = await audit();
        const before = fresh.weights;
        // The divider moved with the previous drag: re-read its box each time.
        const divider = fresh.dividers.find((item) => item.split === target.split && item.index === target.index);
        assert(divider, `divider ${target.split}#${target.index} vanished`);
        // Grip buttons sit at the divider centre; a drag starts a quarter of the way along the line, as a hand would.
        const centre = horizontal ? { x: divider.rect.x + divider.rect.width / 2, y: divider.rect.y + divider.rect.height * 0.25 } : { x: divider.rect.x + divider.rect.width * 0.25, y: divider.rect.y + divider.rect.height / 2 };
        await page.mouse.move(centre.x, centre.y);
        await page.mouse.down(centre.x, centre.y);
        for (let step = 1; step <= 6; step += 1) {
          const offset = sign * 25 * step;
          await page.mouse.move(horizontal ? centre.x + offset : centre.x, horizontal ? centre.y : centre.y + offset);
        }
        const end = horizontal ? { x: centre.x + sign * 150, y: centre.y } : { x: centre.x, y: centre.y + sign * 150 };
        await page.mouse.up(end.x, end.y);
        await delay(40);
        a = await audit();
        check(`drag ${divider.split}#${divider.index} ${sign > 0 ? "+" : "-"}`, a);
        const split = a.weights.find((item) => item.split === divider.split);
        assert(split && split.weights.reduce((x, y) => x + y, 0) === 100, `drag left the split un-normalized: ${JSON.stringify(split)}; pointer trail ${JSON.stringify(a.sentinel.pointerLog.slice(-12))}`);
        assert(JSON.stringify(a.weights) !== JSON.stringify(before), `drag ${divider.split}#${divider.index} did not move any weight`);
      }
    }
    assert(a.treeChanges > 0 && String(a.lastTreeChange).startsWith("divider_drag:"), "drags did not report tree changes");

    // +/- nudges: trusted clicks on every nudge button.
    for (const target of (await audit()).dividers) {
      for (const direction of ["-1", "1"]) {
        const fresh = await audit();
        const before = fresh.weights;
        // The previous nudge moved the divider and its grip: re-read both boxes.
        const divider = fresh.dividers.find((item) => item.split === target.split && item.index === target.index);
        const nudge = divider?.nudges.find((item) => item.direction === direction);
        assert(divider && nudge, `nudge ${target.split}#${target.index} ${direction} vanished`);
        const x = nudge.rect.x + nudge.rect.width / 2; const y = nudge.rect.y + nudge.rect.height / 2;
        // On a fine pointer the grip appears on hovering the divider line; the
        // hand reaches the button by sliding from the line onto it.
        const line = divider.orientation === "vertical" ? { x: divider.rect.x + divider.rect.width / 2, y } : { x, y: divider.rect.y + divider.rect.height / 2 };
        await page.mouse.move(line.x, line.y); await page.mouse.move(x, y); await page.mouse.down(x, y); await page.mouse.up(x, y);
        await delay(30);
        a = await audit();
        check(`nudge ${divider.split}#${divider.index} ${nudge.direction}`, a);
        assert(JSON.stringify(a.weights) !== JSON.stringify(before), `nudge ${divider.split}#${divider.index} ${nudge.direction} did not move any weight; button ${JSON.stringify(nudge.rect)} divider ${JSON.stringify(divider.rect)} hit ${await page.evaluate(`document.elementFromPoint(${x}, ${y})?.className ?? null`)} trail ${JSON.stringify(a.sentinel.pointerLog.slice(-6))} weights ${JSON.stringify({ before, after: a.weights })}`);
      }
    }
    assert(String(a.lastTreeChange).startsWith("divider_nudge:"), "nudges did not report tree changes");

    // Keyboard nudge on a focused divider.
    await page.evaluate("document.querySelector('.ws-divider--row').focus()");
    await page.key("ArrowRight");
    await delay(30);
    a = await audit();
    check("keyboard nudge", a);
    assert(String(a.lastTreeChange).startsWith("divider_key:"), "keyboard nudge did not report a tree change");

    // Cell resizes through the host, window resizes, orientation change; the
    // states rotate across cells on every step so each state is seen resized.
    let shift = 1;
    for (const preset of ["narrow", "short", "tiny", "full"]) {
      await page.evaluate(`window.__wsHarness.resizeHost(${JSON.stringify(preset)}); window.__wsHarness.setStates(${shift++})`);
      await delay(60);
      check(`host ${preset}`, await audit());
    }
    for (const viewport of [{ width: 900, height: 600 }, { width: 1600, height: 1000 }, DESKTOP]) {
      await page.setViewport(viewport, "landscapePrimary");
      await page.evaluate(`window.__wsHarness.setStates(${shift++})`);
      await delay(60);
      a = await audit();
      check(`window ${viewport.width}x${viewport.height}`, a);
      assert(a.viewport.width === viewport.width && a.viewport.height === viewport.height, `viewport did not apply: ${JSON.stringify(a.viewport)}`);
    }
    await page.setViewport({ width: DESKTOP.height, height: DESKTOP.width }, "portraitPrimary");
    await page.evaluate(`window.__wsHarness.setStates(${shift++})`);
    await delay(60);
    check("orientation portrait", await audit());
    await page.setViewport(DESKTOP, "landscapePrimary");
    await delay(60);
    check("orientation landscape", await audit());

    // FW-blank: every sample state in every cell.
    const sampleStateCount = (await audit()).sampleStateCount;
    assert(sampleStateCount >= 16, `expected the full pane-state sample, saw ${sampleStateCount}`);
    for (let index = 0; index < sampleStateCount; index += 1) {
      await page.evaluate(`window.__wsHarness.setAllStates(${index})`);
      check(`state ${index}`, await audit());
    }
    // Cell affordances are wired (a click reaches the handler; nothing else).
    await page.evaluate("window.__wsHarness.setAllStates(3)"); // missing/offer → create + skip
    const action = (await audit()).actions[0];
    assert(action && action.affordance === "create", `expected a create affordance, saw ${JSON.stringify(action)}`);
    const actionPoint = { x: action.rect.x + action.rect.width / 2, y: action.rect.y + action.rect.height / 2 };
    await page.mouse.move(actionPoint.x, actionPoint.y);
    await page.mouse.down(actionPoint.x, actionPoint.y); await page.mouse.up(actionPoint.x, actionPoint.y);
    a = await audit();
    check("affordance click", a);
    assert(a.affordances.length === 1 && a.affordances[0].endsWith(":create"), `affordance handler did not fire once: ${JSON.stringify(a.affordances)}`);
    // Cumulative, not per-step: WebKit delivers some ResizeObserver callbacks
    // after the audit that follows a host/window change, so a monotonic
    // per-step check would be flaky; the storm as a whole must have resized cells.
    assert(a.sentinel.resizeObserverFires > 0, "the dummy panes' ResizeObservers never fired during the storm");
    assert(JSON.stringify(a.weights) !== initialWeights, "the storm never changed the initial weights");

    // Positive control: exactly one explicit fit reaches the sentinel.
    await page.evaluate("window.__wsHarness.explicitFit('qt20')");
    a = await audit();
    assert(a.sentinel.resizeRequests === 1 && a.sentinel.portCalls.length === 1 && a.sentinel.portCalls[0] === "RESIZE_REQUEST", `positive control: expected exactly one RESIZE_REQUEST, saw ${JSON.stringify(a.sentinel.portCalls)}`);
    await page.evaluate("window.__wsHarness.resetSentinel()");
    evidence.positiveControl = { resizeRequests: 1 };

    // Showcase screenshot: six cells in mixed states.
    await page.evaluate("window.__wsHarness.resizeHost('full'); window.__wsHarness.showcase()");
    await delay(80);
    check("showcase", await audit());
    const desktopShot = path.join(EVIDENCE_DIR, `workspace_shell_six_cells_${driver.name}.png`);
    await page.screenshot(desktopShot);
    evidence.screenshots.desktop = desktopShot;

    // Workspace-level refusal renders as a visible full surface.
    await page.evaluate("window.__wsHarness.mountUnavailable()");
    a = await audit();
    assert(a.cellCount === 0 && typeof a.unavailable === "string" && a.unavailable.includes("Dashboard") && a.unavailable.includes("fragment_engine_rejected"), `workspace-level refusal not rendered: ${JSON.stringify(a.unavailable)}`);

    // ---- Coarse pointer at tablet width: still the desktop shell, targets ≥ 44 px.
    page = await driver.session({ viewport: TABLET, coarse: true });
    a = await audit();
    assert(a.coarse === true, `coarse pointer emulation did not apply on ${driver.name}`);
    assert(a.posture === "desktop", `tablet width must keep the desktop shell, saw ${a.posture}`);
    check("coarse tablet", a);
    await page.evaluate("window.__wsHarness.setAllStates(3)");
    a = await audit();
    for (const divider of a.dividers) {
      const size = divider.orientation === "vertical" ? divider.rect.width : divider.rect.height;
      assert(size >= TOUCH_TARGET, `divider ${divider.split}#${divider.index} hit box ${size}px < ${TOUCH_TARGET}px on a coarse pointer`);
      for (const nudge of divider.nudges) assert(nudge.rect.width >= TOUCH_TARGET && nudge.rect.height >= TOUCH_TARGET, `nudge ${JSON.stringify(nudge)} under ${TOUCH_TARGET}px`);
    }
    assert(a.actions.length > 0, "no affordance buttons rendered for the touch-target check");
    for (const item of a.actions) assert(item.rect.width >= TOUCH_TARGET && item.rect.height >= TOUCH_TARGET, `action ${JSON.stringify(item)} under ${TOUCH_TARGET}px`);
    // A drag on a coarse pointer still only moves weights.
    const divider = a.dividers[0];
    const cx = divider.rect.x + divider.rect.width / 2; const cy = divider.rect.y + divider.rect.height * 0.25;
    await page.mouse.move(cx, cy); await page.mouse.down(cx, cy); await page.mouse.move(cx + 60, cy); await page.mouse.move(cx + 120, cy); await page.mouse.up(cx + 120, cy);
    check("coarse drag", await audit());
    evidence.coarseTablet = { dividers: a.dividers.map((item) => ({ split: item.split, index: item.index, rect: item.rect, nudges: item.nudges.map((n) => n.rect) })), actions: a.actions.map((item) => item.rect) };

    // ---- Phone class: the honest state, zero cells, zero transports.
    page = await driver.session({ viewport: PHONE, coarse: true });
    a = await audit();
    assert(a.coarse === true && a.posture === "phone", `phone posture expected: ${JSON.stringify({ coarse: a.coarse, posture: a.posture, viewport: a.viewport })}`);
    assert(a.cellCount === 0, `phone state mounted ${a.cellCount} cells`);
    assert(a.phone && a.phone.notice === a.phone.expectedNotice && a.phone.notice === "workspace view is not available on this device yet", `phone notice: ${JSON.stringify(a.phone?.notice)}`);
    assert(a.phone.title === "ops wall", "phone state lacks the workspace name");
    assert(a.phone.leaves.length === 6, `phone state lists ${a.phone.leaves.length} leaves`);
    const linked = a.phone.leaves.filter((item) => item.href !== null);
    assert(linked.length === 5, `expected five single-terminal links, saw ${linked.length}`);
    for (const item of linked) {
      const url = new URL(item.href);
      const fragment = new URLSearchParams(url.hash.slice(1));
      assert(url.origin === origin && url.pathname === "/terminal" && url.search === "?engine=unified-dev" && fragment.get("engine") === "unified-dev" && fragment.get("mode") === "control" && fragment.get("name") === item.name, `leaf link is not the dashboard's unified terminal URL: ${item.href}`);
      assert(item.linkRect.height >= TOUCH_TARGET, `phone link under ${TOUCH_TARGET}px: ${JSON.stringify(item.linkRect)}`);
    }
    const unresolved = a.phone.leaves.find((item) => item.href === null);
    assert(unresolved && unresolved.name === "scratch" && unresolved.state === "missing" && unresolved.stateText.length > 0, `unresolved leaf must show its state without a dead link: ${JSON.stringify(unresolved)}`);
    assert(a.phone.leaves.every((item) => item.stateText.length > 0), "every phone leaf shows its resolution state");
    assert(a.phone.dashboardRect && a.phone.dashboardRect.height >= TOUCH_TARGET, `phone back-to-dashboard link under ${TOUCH_TARGET}px: ${JSON.stringify(a.phone.dashboardRect)}`);
    assert(a.sentinel.webSockets === 0 && a.sentinel.resizeRequests === 0 && a.sentinel.cspViolations.length === 0 && a.styleAttributes === 0, `phone state touched a forbidden path: ${JSON.stringify(a.sentinel)}`);
    assert(!requests.includes("/ws") && !requests.some((item) => item.startsWith("/api/")), `phone state made network requests: ${JSON.stringify(requests)}`);
    // Rotating the phone keeps the phone posture (short edge rules).
    await page.setViewport({ width: PHONE.height, height: PHONE.width }, "landscapePrimary");
    await page.evaluate("window.__wsHarness.applyPosture()");
    a = await audit();
    assert(a.posture === "phone" && a.cellCount === 0, `landscape phone flipped posture: ${JSON.stringify({ posture: a.posture, cells: a.cellCount })}`);
    await page.setViewport(PHONE, "portraitPrimary");
    await page.evaluate("window.__wsHarness.applyPosture()");
    await delay(80);
    const phoneShot = path.join(EVIDENCE_DIR, `workspace_phone_honest_state_${driver.name}.png`);
    await page.screenshot(phoneShot);
    evidence.screenshots.phone = phoneShot;
    evidence.phone = a.phone;
    // The workspace-level refusal on a coarse pointer: its only way back must be a thumb target too.
    await page.evaluate("window.__wsHarness.mountUnavailable()");
    a = await audit();
    assert(a.cellCount === 0 && typeof a.unavailable === "string" && a.unavailable.includes("Dashboard"), `coarse refusal not rendered: ${JSON.stringify(a.unavailable)}`);
    assert(a.unavailableDashboardRect && a.unavailableDashboardRect.height >= TOUCH_TARGET, `refusal back-to-dashboard link under ${TOUCH_TARGET}px: ${JSON.stringify(a.unavailableDashboardRect)}`);
    evidence.coarseRefusal = { dashboardRect: a.unavailableDashboardRect, phoneDashboardRect: evidence.phone.dashboardRect };

    // A fine pointer at phone width is NOT phone class (a narrow desktop window).
    page = await driver.session({ viewport: PHONE, coarse: false });
    a = await audit();
    assert(a.posture === "desktop" && a.cellCount === 6, `narrow fine-pointer window must keep the desktop shell: ${JSON.stringify({ posture: a.posture, cells: a.cellCount })}`);
    check("narrow fine pointer", a);

    evidence.requests = [...new Set(requests)];
    fs.writeFileSync(path.join(EVIDENCE_DIR, `workspace_layout_gate_${driver.name}.json`), JSON.stringify(evidence, null, 2));
    process.stdout.write(JSON.stringify({ status: "PASS", engine: driver.name, steps: evidence.steps.length, evidenceDir: EVIDENCE_DIR, screenshots: evidence.screenshots }) + "\n");
  } finally {
    await driver.close();
    server.close();
  }
}

main().catch((error) => { console.error(error.stack || String(error)); process.exitCode = 1; });
