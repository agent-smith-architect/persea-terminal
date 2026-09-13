"use strict";
// Independent Dashboard contract retained from the archived legacy surface suite.
const fs = require("fs"), http = require("http"), path = require("path");
const { assert, Tab, launchChrome, stopChrome } = require("./unified_browser_lib.cjs");
const UI = path.resolve(__dirname, "..");
async function main() {
  const server = http.createServer((request, response) => {
    const pathname = new URL(request.url, "http://localhost").pathname;
    if (pathname === "/favicon.ico") { response.writeHead(204); response.end(); return; }
    if (pathname === "/api/keyboard-preferences") {
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ version: 1, layout: { bar: ["key:escape:0"], favorites: [] }, prefixes: { tmux: "key:b:1", screen: "key:a:1" }, revision: 0, stored: false, available: true }));
      return;
    }
    if (pathname === "/") {
      response.setHeader("Content-Type", "text/html");
      response.setHeader("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:");
      response.end('<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/app.css"></head><body><script src="/harness.js"></script></body></html>'); return;
    }
    const file = { "/app.css": "dist/app.css", "/harness.js": "dist/test/slice24_browser_harness.js" }[pathname];
    if (!file) { response.writeHead(404); response.end(); return; }
    response.setHeader("Content-Type", pathname.endsWith(".css") ? "text/css" : "text/javascript");
    response.end(fs.readFileSync(path.join(UI, file)));
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  let chrome; const tabs = [];
  try {
    chrome = await launchChrome("persea-shared-dashboard-");
    const origin = "http://127.0.0.1:" + server.address().port;
    for (const width of [1280, 390]) {
      const tab = await Tab.open("dashboard-" + width, chrome.debugPort, origin, "({href:location.href,ready:document.readyState,harness:window.slice24Ready===true})");
      tabs.push(tab);
      await tab.cdp.send("Emulation.setDeviceMetricsOverride", { width, height: 844, deviceScaleFactor: 1, mobile: width < 500 });
      await tab.navigate(origin + "/");
      assert((await tab.waitUntil(state => state.harness)).state, "Dashboard harness unavailable");
      const dashboard = await tab.evaluate("window.slice24.dashboardRefreshPersistence()");
      assert(dashboard.inventoryReads === 2 && dashboard.aliasDraft === "Unsaved dashboard alias" && dashboard.sameEditor && dashboard.focused && dashboard.selection.join(",") === "2,8", "dashboard refresh lost the alias draft, focus, or text selection");
      assert(dashboard.sameOpenAction && dashboard.originalScope && dashboard.linkScopes.length === 1 && dashboard.linkScopes[0] === dashboard.originalScope && dashboard.linkHandles[0] === "refreshed-control-handle", "dashboard refresh lost its session identity or retained a stale Open handle");
      assert(dashboard.linkHistories.length === 1 && dashboard.linkHistories[0] === "1000" && dashboard.legacyControls === 0, "dashboard unified scrollback default was lost or removed legacy controls reappeared");
      assert(tab.console.length === 0, "browser console not clean: " + JSON.stringify(tab.console));
    }
    console.log("Shared Dashboard refresh browser contracts: PASS (desktop, phone)");
  } finally {
    for (const tab of tabs) tab.cdp.close();
    if (chrome) await stopChrome(chrome);
    await new Promise(resolve => server.close(resolve));
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
