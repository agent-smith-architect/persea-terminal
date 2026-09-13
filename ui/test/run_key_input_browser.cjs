"use strict";

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

const { CDP } = require("./unified_browser_lib.cjs");

async function main() {
  const chromePath = browserBinary();
  const html = `<!doctype html>
<html><head><meta charset="utf-8"><link rel="stylesheet" href="/xterm.css">
<style>html,body{margin:0;width:100%;height:100%;overflow:auto}</style>
</head><body><script src="/key_input_harness.js"></script></body></html>`;
  const server = http.createServer((request, response) => {
    const pathname = new URL(request.url, "http://localhost").pathname;
    if (pathname === "/") {
      response.setHeader("Content-Type", "text/html");
      response.end(html);
      return;
    }
    const routes = {
      "/key_input_harness.js": path.join(UI, "dist/test/key_input_browser_harness.js"),
      "/xterm.css": path.join(UI, "node_modules/@xterm/xterm/css/xterm.css"),
    };
    const file = routes[pathname];
    if (pathname === "/favicon.ico") { response.writeHead(204); response.end(); return; }
    if (!file) { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", file.endsWith(".css") ? "text/css" : "text/javascript");
    response.end(fs.readFileSync(file));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const webPort = server.address().port;
  const debugPort = await freePort();
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), "persea-terminal-key-input-browser-"));
  const chrome = childProcess.spawn(chromePath, [
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
  try {
    let target;
    for (let attempt = 0; attempt < 300; attempt++) {
      if (chromeExit) break;
      try {
        target = await requestJSON(`http://127.0.0.1:${debugPort}/json/new?about:blank`, "PUT");
        break;
      } catch {
        await delay(100);
      }
    }
    assert(target?.webSocketDebuggerUrl, `Chrome target unavailable: ${JSON.stringify({
      chromePath,
      chromeExit,
      stderr: chromeStderr.join("").slice(-2000),
    })}`);
    cdp = new CDP(target.webSocketDebuggerUrl);
    await cdp.open();
    const browserMessages = [];
    cdp.on("Runtime.consoleAPICalled", event => browserMessages.push(event));
    cdp.on("Runtime.exceptionThrown", event => browserMessages.push(event));
    cdp.on("Log.entryAdded", event => browserMessages.push(event));
    await cdp.send("Log.enable");
    await cdp.send("Runtime.enable");
    await cdp.send("Page.enable");
    await cdp.send("Emulation.setDeviceMetricsOverride", {
      width: 390,
      height: 844,
      deviceScaleFactor: 3,
      mobile: true,
    });
    await cdp.send("Emulation.setTouchEmulationEnabled", {
      enabled: true,
      maxTouchPoints: 5,
    });
    await cdp.send("Emulation.setEmulatedMedia", {
      features: [{ name: "any-pointer", value: "coarse" }],
    });
    await cdp.send("Page.navigate", { url: `http://127.0.0.1:${webPort}/` });
    for (let attempt = 0; attempt < 200; attempt++) {
      const ready = await cdp.send("Runtime.evaluate", {
        expression: "document.readyState === \"complete\" && window.keyInputHarnessReady === true",
        returnByValue: true,
      });
      if (ready.result.value === true) break;
      await delay(25);
    }
    await cdp.send("Page.bringToFront");
    const result = await cdp.send("Runtime.evaluate", {
      expression: "window.keyInputHarness.run()",
      awaitPromise: true,
      returnByValue: true,
    });
    if (result.exceptionDetails) {
      const description = result.exceptionDetails.exception?.description
        || result.exceptionDetails.text
        || "browser harness exception";
      throw new Error(description);
    }
    const coarse = await cdp.send("Runtime.evaluate", {
      expression: "matchMedia('(any-pointer: coarse)').matches",
      returnByValue: true,
    });
    assert(coarse.result.value === true, "Chromium did not apply any-pointer: coarse emulation");
    const prepared = await cdp.send("Runtime.evaluate", {
      expression: "window.keyInputHarness.preparePointerMediaChange()",
      awaitPromise: true,
      returnByValue: true,
    });
    if (prepared.exceptionDetails) throw new Error(prepared.exceptionDetails.exception?.description || prepared.exceptionDetails.text);
    await cdp.send("Emulation.setTouchEmulationEnabled", {
      enabled: false,
      maxTouchPoints: 1,
    });
    await cdp.send("Emulation.setDeviceMetricsOverride", {
      width: 1280,
      height: 800,
      deviceScaleFactor: 1,
      mobile: false,
    });
    await cdp.send("Emulation.setEmulatedMedia", {
      features: [{ name: "any-pointer", value: "fine" }],
    });
    const disabled = await cdp.send("Runtime.evaluate", {
      expression: "window.keyInputHarness.assertPointerMediaDisabled()",
      awaitPromise: true,
      returnByValue: true,
    });
    if (disabled.exceptionDetails) throw new Error(disabled.exceptionDetails.exception?.description || disabled.exceptionDetails.text);
    await cdp.send("Emulation.setDeviceMetricsOverride", {
      width: 390,
      height: 844,
      deviceScaleFactor: 3,
      mobile: true,
    });
    await cdp.send("Emulation.setTouchEmulationEnabled", {
      enabled: true,
      maxTouchPoints: 5,
    });
    await cdp.send("Emulation.setEmulatedMedia", {
      features: [{ name: "any-pointer", value: "coarse" }],
    });
    const restored = await cdp.send("Runtime.evaluate", {
      expression: "window.keyInputHarness.assertPointerMediaRestoredAndDestroy()",
      awaitPromise: true,
      returnByValue: true,
    });
    if (restored.exceptionDetails) throw new Error(restored.exceptionDetails.exception?.description || restored.exceptionDetails.text);
    assert(browserMessages.length === 0, "browser console not clean: " + JSON.stringify(browserMessages));
    console.log(`key input browser invariants passed: ${JSON.stringify({
      ...result.result.value,
      pointerMediaLifecycle: "passed",
    })}`);
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
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : error);
  process.exitCode = 1;
});
