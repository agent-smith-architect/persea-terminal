"use strict";

// A link that stops passing data without closing: the server sends response
// headers and the first byte of the body, then nothing. Every page request
// must still end at its deadline, and the page must recover by itself once
// the server answers again. Runs the real page clients with their default
// request path in Chromium and WebKit.
const http = require("node:http");
const path = require("node:path");
const assert = require("node:assert/strict");
const { buildSync } = require("esbuild");
const { chromium, webkit } = require("playwright");

const UI = path.resolve(__dirname, "..");
const harness = `
import { Dashboard } from "./src/dashboard";
import { OperatorPreferencesService } from "./src/operator_preferences";
import { WorkspaceInventory } from "./src/workspace_page";
const result = window.result = { preferencesSettled: false, attempts: [] };
new Dashboard(document.getElementById("app")).mount();
const preferences = new OperatorPreferencesService();
preferences.load().then(() => { result.preferencesSettled = true; });
const workspace = window.workspace = new WorkspaceInventory();
// Three panes ask for the session list and give up waiting; the shared read
// they joined must still end, so a later pane is not stuck behind it.
window.detachedAttempt = () => {
  const controller = new AbortController();
  const pending = workspace.snapshot(controller.signal, -1).then(() => "resolved", (error) => error.name);
  setTimeout(() => controller.abort(), 50);
  return pending;
};
window.freshAttempt = () => workspace.snapshot(undefined, -1).then((snapshot) => snapshot.inventory.realms.length, (error) => "failed: " + error.message);
window.ready = true;
`;
const script = buildSync({ stdin: { contents: harness, resolveDir: UI, loader: "ts" }, loader: { ".css": "empty" }, bundle: true, platform: "browser", format: "iife", write: false }).outputFiles[0].text;
const INVENTORY = JSON.stringify({ image_upload: false, aliases: [], realms: [] });

async function scenario(browserType, engine) {
  const state = { inventory: "stall", counts: {} };
  const held = [];
  const server = http.createServer((request, response) => {
    const url = new URL(request.url, "http://localhost");
    state.counts[url.pathname] = (state.counts[url.pathname] || 0) + 1;
    if (url.pathname === "/") { response.setHeader("Content-Type", "text/html"); response.end('<!doctype html><main id="app"></main><script src="/harness.js"></script>'); return; }
    if (url.pathname === "/harness.js") { response.setHeader("Content-Type", "text/javascript"); response.end(script); return; }
    const stall = url.pathname === "/api/preferences" || (url.pathname === "/api/inventory" && state.inventory === "stall");
    if (stall) { response.setHeader("Content-Type", "application/json"); response.write("{"); held.push(response); return; }
    if (url.pathname === "/api/inventory" && state.inventory === "error") { response.writeHead(503); response.end("unavailable"); return; }
    if (url.pathname === "/api/inventory") { response.setHeader("Content-Type", "application/json"); response.end(INVENTORY); return; }
    response.writeHead(204); response.end();
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const browser = await browserType.launch({ headless: true, ...(engine === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}) });
  const pageErrors = [];
  try {
    const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
    page.on("pageerror", (error) => pageErrors.push(String(error)));
    await page.goto(`http://127.0.0.1:${server.address().port}/`);
    await page.waitForFunction(() => window.ready === true);
    const status = page.locator(".dashboard-status");
    const refresh = page.getByRole("button", { name: "Refresh sessions" });

    const detached = await Promise.all([0, 1, 2].map(() => page.evaluate(() => window.detachedAttempt())));
    assert.deepEqual(detached, ["AbortError", "AbortError", "AbortError"], `${engine}: a pane's own wait was not detachable`);
    assert.equal(await page.evaluate(() => window.workspace.requestCount()), 1, `${engine}: detached panes did not share one read`);

    // Appearance preferences have a 5 s budget that covers the body.
    await page.waitForFunction(() => window.result.preferencesSettled, null, { timeout: 8_000 });
    // The session list has a 10 s budget; then the dashboard says so and
    // leaves Refresh usable.
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent.includes("No reply from the server."), null, { timeout: 14_000 });
    assert.match(await status.textContent(), /Trying again automatically\./, `${engine}: the failure did not say recovery is automatic`);
    assert.equal(await refresh.isDisabled(), false, `${engine}: Refresh stayed disabled after the deadline`);

    // The server answers again: the dashboard recovers without any action,
    // and a pane that asks now gets a new read instead of the expired one.
    state.inventory = "ok";
    const before = state.counts["/api/inventory"];
    assert.equal(await page.evaluate(() => window.freshAttempt()), 0, `${engine}: a later pane did not get a fresh read`);
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent === "", null, { timeout: 6_000 });
    assert.ok(state.counts["/api/inventory"] >= before + 2, `${engine}: recovery did not read the list again`);

    // After a failure, the network coming back retries at once, before the
    // backoff timer (at least 1.6 s) can.
    state.inventory = "error";
    await refresh.click();
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent.includes("Trying again automatically."), null, { timeout: 5_000 });
    state.inventory = "ok";
    await page.evaluate(() => window.dispatchEvent(new Event("online")));
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent === "", null, { timeout: 1_000 });
    assert.deepEqual(pageErrors, [], `${engine}: page errors`);
    console.log(`${engine}: request stall recovery passed`);
  } finally {
    await browser.close();
    for (const response of held) response.destroy();
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
}

(async () => {
  const only = process.env.PERSEA_STALL_ENGINE;
  for (const [engine, browserType] of [["chromium", chromium], ["webkit", webkit]]) if (!only || only === engine) await scenario(browserType, engine);
})().catch((error) => { console.error(error); process.exitCode = 1; });
