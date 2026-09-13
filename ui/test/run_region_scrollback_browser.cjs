"use strict";

// Falsifier runner for scroll-region history preservation. It bundles the real
// product modules (including the patched @xterm/xterm build the app ships) and
// replays codex-style DECSTBM scroll sequences through UnifiedTerminalPage in
// a real browser. Expectations are pinned by tmux control probes; see
// test/region_scrollback_browser_harness.ts.

const fs = require("fs");
const http = require("http");
const net = require("net");
const os = require("os");
const path = require("path");
const childProcess = require("child_process");

const UI = path.resolve(__dirname, "..");
function assert(value, message) { if (!value) throw new Error(message); }
function browserBinary() {
  const candidate = require("./browser_path.cjs")();
  assert(candidate && path.isAbsolute(candidate), "set CHROME_BIN to an absolute Chromium-family browser executable");
  fs.accessSync(candidate, fs.constants.X_OK);
  return candidate;
}
function delay(ms) { return new Promise((resolve) => setTimeout(resolve, ms)); }
async function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const port = server.address().port;
      server.close(() => resolve(port));
    });
  });
}
function requestJSON(url, method = "GET") {
  return new Promise((resolve, reject) => {
    const request = http.request(url, { method }, (response) => {
      let body = "";
      response.setEncoding("utf8");
      response.on("data", (chunk) => { body += chunk; });
      response.on("end", () => {
        try { resolve(JSON.parse(body)); } catch (error) { reject(error); }
      });
    });
    request.on("error", reject);
    request.end();
  });
}

async function evaluate(cdp, expression) {
  const result = await cdp.send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
  if (result.exceptionDetails) {
    const description = result.exceptionDetails.exception?.description
      || result.exceptionDetails.text
      || "browser harness exception";
    throw new Error(description);
  }
  return result.result.value;
}

class CDP {
  constructor(url) {
    this.url = url;
    this.next = 1;
    this.pending = new Map();
  }
  async open() {
    this.ws = new WebSocket(this.url);
    await new Promise((resolve, reject) => {
      this.ws.addEventListener("open", resolve, { once: true });
      this.ws.addEventListener("error", reject, { once: true });
    });
    this.ws.addEventListener("message", (event) => {
      const message = JSON.parse(String(event.data));
      if (!message.id) return;
      const pending = this.pending.get(message.id);
      if (!pending) return;
      this.pending.delete(message.id);
      message.error ? pending.reject(new Error(message.error.message)) : pending.resolve(message.result);
    });
  }
  send(method, params = {}) {
    const id = this.next++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }
  close() { this.ws?.close(); }
}

async function main() {
  childProcess.execFileSync(path.join(UI, "node_modules/.bin/esbuild"), [
    path.join(UI, "test/region_scrollback_browser_harness.ts"),
    "--bundle", "--platform=browser", "--format=iife",
    `--outfile=${path.join(UI, "dist/test/region_scrollback_browser_harness.js")}`,
  ], { stdio: "inherit" });

  const html = `<!doctype html>
<html><head><meta charset="utf-8"><link rel="stylesheet" href="/xterm.css"><link rel="stylesheet" href="/attachment.css">
<style>html,body{margin:0;width:100%;height:100%;overflow:hidden;background:#111318}</style>
</head><body><div id="root"></div><script src="/region_scrollback_harness.js"></script></body></html>`;
  const server = http.createServer((request, response) => {
    const pathname = new URL(request.url, "http://localhost").pathname;
    if (pathname === "/") {
      response.setHeader("Content-Type", "text/html");
      response.end(html);
      return;
    }
    const routes = {
      "/region_scrollback_harness.js": path.join(UI, "dist/test/region_scrollback_browser_harness.js"),
      "/xterm.css": path.join(UI, "node_modules/@xterm/xterm/css/xterm.css"),
      "/attachment.css": path.join(UI, "src/attachment_page.css"),
    };
    const file = routes[pathname];
    if (!file) { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", file.endsWith(".css") ? "text/css" : "text/javascript");
    response.end(fs.readFileSync(file));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const webPort = server.address().port;
  const debugPort = await freePort();
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), "persea-region-scrollback-"));
  const chrome = childProcess.spawn(browserBinary(), [
    "--headless=new",
    "--no-sandbox",
    "--disable-gpu",
    "--disable-dev-shm-usage",
    `--remote-debugging-port=${debugPort}`,
    `--user-data-dir=${profile}`,
    "--no-first-run",
    "--no-default-browser-check",
    "about:blank",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  const chromeStderr = [];
  let chromeExit = null;
  chrome.stderr.on("data", (chunk) => { if (chromeStderr.length < 64) chromeStderr.push(String(chunk)); });
  chrome.on("exit", (code, signal) => { chromeExit = { code, signal }; });
  let cdp;
  let outcome;
  try {
    let target;
    for (let attempt = 0; attempt < 300; attempt++) {
      if (chromeExit) break;
      try {
        target = await requestJSON(`http://127.0.0.1:${debugPort}/json/new?http://127.0.0.1:${webPort}/`, "PUT");
        break;
      } catch {
        await delay(100);
      }
    }
    assert(target?.webSocketDebuggerUrl, `Chrome target unavailable: ${JSON.stringify({
      chromeExit,
      stderr: chromeStderr.join("").slice(-2000),
    })}`);
    cdp = new CDP(target.webSocketDebuggerUrl);
    await cdp.open();
    await cdp.send("Runtime.enable");
    await cdp.send("Page.enable");
    for (let attempt = 0; attempt < 200; attempt++) {
      const ready = await cdp.send("Runtime.evaluate", {
        expression: "window.regionScrollbackHarnessReady === true",
        returnByValue: true,
      });
      if (ready.result.value === true) break;
      await delay(25);
    }
    outcome = await evaluate(cdp, "window.__regionScrollbackHarness.run()");
  } finally {
    cdp?.close();
    server.close();
    if (!chromeExit) {
      chrome.kill("SIGTERM");
      await Promise.race([
        new Promise((resolve) => chrome.once("exit", resolve)),
        delay(5000),
      ]);
    }
    if (!chromeExit) {
      chrome.kill("SIGKILL");
      await new Promise((resolve) => chrome.once("exit", resolve));
    }
    fs.rmSync(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
  }

  const artifact = process.env.PERSEA_REGION_SCROLLBACK_ARTIFACT;
  if (artifact) fs.writeFileSync(artifact, `${JSON.stringify(outcome, null, 2)}\n`);
  process.stdout.write(`${JSON.stringify(outcome, null, 2)}\n`);
  assert(outcome && outcome.failures.length === 0,
    `region scrollback falsifier failed:\n  ${(outcome?.failures ?? ["no outcome"]).join("\n  ")}`);
  process.stdout.write("region scrollback falsifier PASS\n");
}

main().catch((error) => {
  console.error(error instanceof Error ? error.stack || error.message : error);
  process.exitCode = 1;
});
