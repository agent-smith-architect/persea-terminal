"use strict";

// Shared plumbing for the Node-hosted unified browser gates: a Chromium-family
// browser driven over raw CDP (no Playwright dependency), one tab per driven
// document with console capture, and the polling probes the scenarios use.
// Behaviour here is deliberately identical to what run_unified_reopen_browser
// grew inline; it was lifted out so the ergonomics gate can drive the same
// fixture the same way.

const fs = require("fs");
const http = require("http");
const net = require("net");
const os = require("os");
const path = require("path");
const childProcess = require("child_process");

function assert(value, message) {
  if (!value) throw new Error(message);
}

function browserBinary() {
  const candidate = require("./browser_path.cjs")();
  assert(candidate && path.isAbsolute(candidate), "set CHROME_BIN to an absolute Chromium-family browser executable");
  fs.accessSync(candidate, fs.constants.X_OK);
  return candidate;
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      server.close(() => resolve(address.port));
    });
  });
}

function requestJSON(url, method = "GET", body) {
  return new Promise((resolve, reject) => {
    const request = http.request(url, { method, headers: body ? { "Content-Type": "application/json" } : {} }, (response) => {
      let text = "";
      response.setEncoding("utf8");
      response.on("data", (chunk) => { text += chunk; });
      response.on("end", () => {
        try { resolve(JSON.parse(text)); } catch (error) { reject(error); }
      });
    });
    request.on("error", reject);
    if (body) request.write(JSON.stringify(body));
    request.end();
  });
}

class CDP {
  constructor(url) {
    this.url = url;
    this.next = 1;
    this.pending = new Map();
    this.listeners = new Map();
  }

  async open() {
    this.ws = new WebSocket(this.url);
    await new Promise((resolve, reject) => {
      this.ws.addEventListener("open", resolve, { once: true });
      this.ws.addEventListener("error", reject, { once: true });
    });
    // A closed session never leaves a caller awaiting forever: every pending
    // command rejects, so a gate fails loudly instead of draining the event
    // loop and exiting 0 before its summary.
    this.ws.addEventListener("close", (event) => {
      const reason = `CDP session closed (${event.code}${event.reason ? ` ${event.reason}` : ""})`;
      for (const [id, pending] of this.pending) { this.pending.delete(id); pending.reject(new Error(reason)); }
    }, { once: true });
    this.ws.addEventListener("message", (event) => {
      const message = JSON.parse(String(event.data));
      if (message.id) {
        const pending = this.pending.get(message.id);
        if (!pending) return;
        this.pending.delete(message.id);
        if (message.error) pending.reject(new Error(message.error.message));
        else pending.resolve(message.result);
        return;
      }
      for (const listener of this.listeners.get(message.method) ?? []) listener(message.params);
    });
  }

  send(method, params = {}) {
    const id = this.next++;
    return new Promise((resolve, reject) => {
      if (!this.ws || this.ws.readyState !== WebSocket.OPEN) { reject(new Error(`CDP session is not open for ${method}`)); return; }
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  on(method, listener) {
    const listeners = this.listeners.get(method) ?? [];
    listeners.push(listener);
    this.listeners.set(method, listeners);
  }

  close() {
    this.ws?.close();
  }
}

// One driven tab: its CDP session, console capture, and the page probes. The
// state probe is an expression the gate supplies, so each gate reads exactly
// the page facts its scenarios judge.
class Tab {
  constructor(name, cdp, origin, stateExpression) {
    this.name = name;
    this.cdp = cdp;
    this.origin = origin;
    this.console = [];
    this.stateExpression = stateExpression;
  }

  static async open(name, debugPort, origin, stateExpression) {
    const target = await requestJSON(`http://127.0.0.1:${debugPort}/json/new?about:blank`, "PUT");
    assert(target?.webSocketDebuggerUrl, `${name}: Chrome target unavailable`);
    const cdp = new CDP(target.webSocketDebuggerUrl);
    await cdp.open();
    const tab = new this(name, cdp, origin, stateExpression);
    tab.targetId = target.id;
    cdp.on("Runtime.consoleAPICalled", (params) => tab.console.push({
      tab: name, kind: params.type, text: params.args.map((argument) => argument.value ?? argument.description ?? "").join(" "),
    }));
    cdp.on("Runtime.exceptionThrown", (params) => tab.console.push({
      tab: name, kind: "exception", text: params.exceptionDetails.exception?.description || params.exceptionDetails.text,
    }));
    cdp.on("Log.entryAdded", (params) => tab.console.push({
      tab: name, kind: `log:${params.entry.level}`, text: params.entry.text, url: params.entry.url,
    }));
    await cdp.send("Runtime.enable");
    await cdp.send("Page.enable");
    await cdp.send("Log.enable");
    await cdp.send("Network.enable");
    return tab;
  }

  async evaluate(expression) {
    const result = await this.cdp.send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
    if (result.exceptionDetails) {
      throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text || "unknown exception");
    }
    return result.result.value;
  }

  // Polls a predicate over page state; returns the first satisfying state or
  // null. Every intermediate connection-strip text is recorded so a brief
  // "Reconnecting to alpha…" can be proven even though it is transient.
  async waitUntil(predicate, timeoutMs = 8_000) {
    const deadline = Date.now() + timeoutMs;
    const strips = new Set();
    while (Date.now() < deadline) {
      let current = null;
      try { current = await this.state(); } catch { /* document navigation in flight */ }
      if (current) {
        if (current.connection) strips.add(current.connection);
        if (predicate(current)) return { state: current, strips: [...strips] };
      }
      await delay(15);
    }
    let last = null;
    try { last = await this.state(); } catch { /* unchanged */ }
    return { state: null, last, strips: [...strips] };
  }

  async navigate(url) {
    await this.cdp.send("Page.navigate", { url: "about:blank" });
    await delay(50);
    await this.cdp.send("Page.navigate", { url });
    const loaded = await this.waitUntil((state) => state.href === url && state.ready === "complete", 10_000);
    assert(loaded.state, `${this.name}: document did not complete navigation to ${url}`);
  }

  async reload() {
    await this.cdp.send("Page.reload");
    await delay(100);
  }

  state() {
    return this.evaluate(this.stateExpression);
  }

  async type(text) {
    await this.evaluate("document.querySelector('.xterm-helper-textarea')?.focus()");
    for (const character of text) {
      await this.cdp.send("Input.dispatchKeyEvent", { type: "keyDown", text: character, key: character, unmodifiedText: character });
      await this.cdp.send("Input.dispatchKeyEvent", { type: "keyUp", key: character });
    }
  }

  async trustedClick(point) {
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: point.x, y: point.y });
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: point.x, y: point.y, button: "left", buttons: 1, clickCount: 1 });
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: point.x, y: point.y, button: "left", buttons: 0, clickCount: 1 });
  }

  // A real touch tap: Chromium synthesizes pointerdown/pointerup (pointerType
  // touch) and the click a tap produces — the events a phone delivers.
  async tap(point) {
    await this.cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: point.x, y: point.y }] });
    await this.cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  }

  async close(debugPort) {
    try { this.cdp.close(); } catch { /* already gone */ }
    try { await requestJSON(`http://127.0.0.1:${debugPort}/json/close/${this.targetId}`); } catch { /* target already closed */ }
  }
}

// Headless Chromium on a private profile; returns handles the caller passes
// to stopChrome in its finally block.
async function launchChrome(profilePrefix) {
  const debugPort = await freePort();
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), profilePrefix));
  const chrome = childProcess.spawn(browserBinary(), [
    "--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
    `--remote-debugging-port=${debugPort}`, `--user-data-dir=${profile}`,
    "--no-first-run", "--no-default-browser-check", "about:blank",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  const stderr = [];
  const handle = { chrome, debugPort, profile, stderr, exit: null };
  chrome.stderr.on("data", (chunk) => { if (stderr.length < 64) stderr.push(String(chunk)); });
  chrome.on("exit", (code, signal) => { handle.exit = { code, signal }; });
  for (let attempt = 0; attempt < 300 && !handle.exit; attempt += 1) {
    try { await requestJSON(`http://127.0.0.1:${debugPort}/json/version`); break; } catch { await delay(100); }
  }
  assert(!handle.exit, `Chrome exited early: ${JSON.stringify({ exit: handle.exit, stderr: stderr.join("").slice(-2_000) })}`);
  return handle;
}

async function stopChrome(handle) {
  const waitForExit = (timeoutMs) => new Promise((resolve) => {
    if (handle.exit !== null) { resolve(true); return; }
    const timer = setTimeout(() => resolve(false), timeoutMs);
    handle.chrome.once("exit", () => { clearTimeout(timer); resolve(true); });
  });
  handle.chrome.kill("SIGTERM");
  if (!(await waitForExit(5_000))) {
    handle.chrome.kill("SIGKILL");
    await waitForExit(5_000);
  }
  fs.rmSync(handle.profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
}

module.exports = { assert, browserBinary, delay, freePort, requestJSON, CDP, Tab, launchChrome, stopChrome };
