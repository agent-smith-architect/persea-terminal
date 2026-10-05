"use strict";

// A link that stops passing data without closing: the server sends response
// headers and the first byte of the body, then nothing. Every page request
// must still end at its deadline, and the page must recover by itself once
// the server answers again. Runs the real page clients with their default
// request path in Chromium and WebKit. Each case has its own page; the server
// tells them apart by the page address the request comes from.
const http = require("node:http");
const path = require("node:path");
const assert = require("node:assert/strict");
const { buildSync } = require("esbuild");
const { chromium, webkit } = require("playwright");

const UI = path.resolve(__dirname, "..");
const harness = `
import { Dashboard } from "./src/dashboard";
import { OperatorPreferencesService } from "./src/operator_preferences";
import { WorkspaceInventory, WorkspacePage } from "./src/workspace_page";
import { leaf } from "./src/workspace_model";
const root = document.getElementById("app");
const kind = new URLSearchParams(location.search).get("case");
const result = window.result = { preferencesSettled: false };
if (kind === "stall") {
  new Dashboard(root).mount();
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
  window.joinShared = () => { window.shared = workspace.snapshot(undefined, -1).then(() => "resolved", (error) => error.name); };
  window.freshAttempt = () => workspace.snapshot(undefined, -1).then((snapshot) => snapshot.inventory.realms.length, (error) => "failed: " + error.message);
} else if (kind === "transfer") {
  // The image client the dashboard builds for itself, not a standalone one.
  const images = new Dashboard(root).clipboard.images;
  const started = performance.now();
  window.transfer = images.file({ id: "a".repeat(32), mediaType: "image/png", byteSize: 262144, expiresAt: null })
    .then((file) => ({ bytes: file.size, ms: performance.now() - started }), (error) => ({ error: error.name, ms: performance.now() - started }));
} else if (kind === "gate") {
  const dashboard = window.dashboard = new Dashboard(root);
  dashboard.mount();
  window.holdEdit = (held) => dashboard.gate.setMutating(held);
  window.setHidden = (hidden) => {
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => hidden ? "hidden" : "visible" });
    document.dispatchEvent(new Event("visibilitychange"));
  };
} else {
  // The request token cookie needs HTTPS (a __Host- cookie); this plain HTTP
  // test server does not check it, so the page reads a well-formed one.
  Object.defineProperty(document, "cookie", { get: () => "__Host-persea-terminal-csrf=" + "A".repeat(43) });
  const inventory = new WorkspaceInventory();
  const page = window.page = new WorkspacePage({ root, styleNonce: "AAAAAAAAAAAAAAAAAAAAAA", name: "stall", win: window, inventory, historyRows: 1000 });
  await inventory.snapshot(undefined, -1);
  await page.open(leaf({ realm: "local", server: "private", name: "missing" }, kind === "workspace" ? "create" : "offer"));
  window.paneState = () => page.panesSnapshot()[0].state;
}
window.ready = true;
`;
const imports = harness.match(/^import .*;$/gm).join("\n");
const script = buildSync({ stdin: { contents: `${imports}\n(async () => {${harness.replace(/^import .*;$/gm, "")}})();`, resolveDir: UI, loader: "ts" }, loader: { ".css": "empty" }, bundle: true, platform: "browser", format: "iife", write: false }).outputFiles[0].text;
const INVENTORY = JSON.stringify({ image_upload: false, aliases: [], realms: [] });

async function scenario(browserType, engine) {
  // Per case: what the session list read does ("ok", "stall", "error") and
  // how many requests each path received.
  const cases = { stall: { inventory: "stall" }, transfer: { inventory: "ok" }, gate: { inventory: "error" }, workspace: { inventory: "ok" }, create: { inventory: "ok" }, created: { inventory: "ok" } };
  for (const state of Object.values(cases)) state.counts = {};
  const held = [];
  const timers = new Set();
  const server = http.createServer((request, response) => {
    const url = new URL(request.url, "http://localhost");
    if (url.pathname === "/") { response.setHeader("Content-Type", "text/html"); response.end('<!doctype html><main id="app"></main><script src="/harness.js"></script>'); return; }
    if (url.pathname === "/harness.js") { response.setHeader("Content-Type", "text/javascript"); response.end(script); return; }
    const state = cases[new URL(request.headers.referer || "http://localhost").searchParams.get("case")];
    if (!state) { response.writeHead(404); response.end(); return; }
    const key = `${request.method} ${url.pathname}`;
    state.counts[key] = (state.counts[key] || 0) + 1;
    const stall = (url.pathname === "/api/preferences" && state === cases.stall) || (url.pathname === "/api/inventory" && state.inventory === "stall");
    if (stall) { response.setHeader("Content-Type", "application/json"); response.write("{"); held.push(response); return; }
    // A create whose reply never comes back (no headers at all), or one that
    // succeeds before the session list read stalls.
    if (key === "POST /api/sessions" && state === cases.created) { state.inventory = "stall"; response.writeHead(201); response.end("{}"); return; }
    if (key === "POST /api/sessions") { held.push(response); return; }
    if (url.pathname.startsWith("/api/clipboard/images/")) {
      // A file that takes longer than a small read's deadline to arrive.
      response.setHeader("Content-Type", "image/png"); response.write(Buffer.alloc(1));
      const timer = setTimeout(() => { timers.delete(timer); response.end(Buffer.alloc(262143)); }, 11_200);
      timers.add(timer); return;
    }
    if (url.pathname === "/api/inventory" && state.inventory === "error") { response.writeHead(503); response.end("unavailable"); return; }
    if (url.pathname === "/api/inventory") { response.setHeader("Content-Type", "application/json"); response.end(INVENTORY); return; }
    if (url.pathname === "/api/workspaces") { response.setHeader("Content-Type", "application/json"); response.end('{"version":1,"items":[]}'); return; }
    response.writeHead(204); response.end();
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const browser = await browserType.launch({ headless: true, ...(engine === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}) });
  const pageErrors = [];
  const open = async (kind) => {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
    page.on("pageerror", (error) => pageErrors.push(`${kind}: ${error}`));
    await page.goto(`http://127.0.0.1:${server.address().port}/?case=${kind}`);
    await page.waitForFunction(() => window.ready === true);
    return page;
  };
  const status = (page) => page.evaluate(() => document.querySelector(".dashboard-status")?.textContent ?? "");

  const stallCase = async () => {
    const page = await open("stall");
    const refresh = page.getByRole("button", { name: "Refresh sessions" });
    const detached = await Promise.all([0, 1, 2].map(() => page.evaluate(() => window.detachedAttempt())));
    assert.deepEqual(detached, ["AbortError", "AbortError", "AbortError"], `${engine}: a pane's own wait was not detachable`);
    assert.equal(await page.evaluate(() => window.workspace.requestCount()), 1, `${engine}: detached panes did not share one read`);
    await page.evaluate(() => window.joinShared());

    // Appearance preferences have a 5 s budget that covers the body.
    await page.waitForFunction(() => window.result.preferencesSettled, null, { timeout: 8_000 });
    // The session list has a 10 s budget; then the dashboard says so and
    // leaves Refresh usable.
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent.includes("No reply from the server."), null, { timeout: 14_000 });
    assert.match(await status(page), /Trying again automatically\./, `${engine}: the failure did not say recovery is automatic`);
    assert.equal(await refresh.isDisabled(), false, `${engine}: Refresh stayed disabled after the deadline`);

    // The shared read itself ends at its deadline.
    assert.equal(await page.evaluate(() => window.shared), "RequestTimeoutError", `${engine}: the shared session list read did not end at its deadline`);
    // The server answers again: the dashboard recovers without any action,
    // and a pane that asks now gets a new read instead of the expired one.
    cases.stall.inventory = "ok";
    const before = cases.stall.counts["GET /api/inventory"];
    assert.equal(await page.evaluate(() => window.freshAttempt()), 0, `${engine}: a later pane did not get a fresh read`);
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent === "", null, { timeout: 6_000 });
    assert.ok(cases.stall.counts["GET /api/inventory"] >= before + 2, `${engine}: recovery did not read the list again`);

    // After a failure, the network coming back retries at once, before the
    // backoff timer (at least 1.6 s) can.
    cases.stall.inventory = "error";
    await refresh.click();
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent.includes("Trying again automatically."), null, { timeout: 5_000 });
    cases.stall.inventory = "ok";
    await page.evaluate(() => window.dispatchEvent(new Event("online")));
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent === "", null, { timeout: 1_000 });
  };

  // An image transfer from the dashboard has the transfer budget, not the
  // 10 s budget of a small read.
  const transferCase = async () => {
    const page = await open("transfer");
    const transfer = await page.evaluate(() => window.transfer);
    assert.equal(transfer.bytes, 262144, `${engine}: a slow image transfer from the dashboard failed: ${JSON.stringify(transfer)}`);
    assert.ok(transfer.ms > 11_000, `${engine}: the transfer did not take the paced time: ${JSON.stringify(transfer)}`);
  };

  // A retry that falls while the page is hidden and a save is in progress is
  // not lost, also when the page comes back before the save ends: it runs
  // once the save ends, long before the 60 s periodic read.
  const gateCase = async () => {
    const page = await open("gate");
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent.includes("Trying again automatically."), null, { timeout: 5_000 });
    await page.evaluate(() => { window.holdEdit(true); window.setHidden(true); });
    cases.gate.inventory = "ok";
    const blocked = cases.gate.counts["GET /api/inventory"];
    // The first retry comes within 2.4 s; stay hidden past it, then return
    // while the save is still in progress.
    await page.waitForTimeout(3_000);
    await page.evaluate(() => window.setHidden(false));
    await page.waitForTimeout(500);
    assert.equal(cases.gate.counts["GET /api/inventory"], blocked, `${engine}: a refresh ran during the save`);
    await page.evaluate(() => window.holdEdit(false));
    await page.waitForFunction(() => document.querySelector(".dashboard-status")?.textContent === "", null, { timeout: 6_000 });
  };

  // A pane's Retry whose session list read gets no reply ends with Retry
  // offered again, and works once the server answers.
  const workspaceCase = async () => {
    const page = await open("workspace");
    cases.workspace.inventory = "stall";
    await page.getByRole("button", { name: "Retry", exact: true }).click();
    await page.waitForFunction(() => window.paneState().kind === "failed", null, { timeout: 14_000 });
    assert.deepEqual(await page.evaluate(() => window.paneState()), { kind: "failed", reason: "session_list_unavailable" }, `${engine}: the pane did not report the unread list`);
    cases.workspace.inventory = "ok";
    await page.getByRole("button", { name: "Retry", exact: true }).click();
    await page.waitForFunction(() => window.paneState().kind === "missing", null, { timeout: 5_000 });
  };

  // A create that gets no reply says the outcome is unknown; Retry reads the
  // session list and never sends the create again.
  const createCase = async () => {
    const page = await open("create");
    await page.getByRole("button", { name: "Create session", exact: true }).click();
    await page.waitForFunction(() => window.paneState().kind === "failed", null, { timeout: 14_000 });
    assert.deepEqual(await page.evaluate(() => window.paneState()), { kind: "failed", reason: "create_outcome_unknown" }, `${engine}: a create without a reply did not say the outcome is unknown`);
    const reads = cases.create.counts["GET /api/inventory"];
    await page.getByRole("button", { name: "Retry", exact: true }).click();
    await page.waitForFunction(() => window.paneState().kind === "missing", null, { timeout: 5_000 });
    assert.equal(cases.create.counts["GET /api/inventory"], reads + 1, `${engine}: Retry did not read the session list`);
    assert.equal(cases.create.counts["POST /api/sessions"], 1, `${engine}: the create was sent again`);
  };

  // A create that succeeds, followed by a session list read without a reply,
  // ends with Retry offered, not at "Finding this session".
  const createdCase = async () => {
    const page = await open("created");
    await page.getByRole("button", { name: "Create session", exact: true }).click();
    await page.waitForFunction(() => window.paneState().kind === "failed", null, { timeout: 14_000 });
    assert.deepEqual(await page.evaluate(() => window.paneState()), { kind: "failed", reason: "session_list_unavailable" }, `${engine}: the pane did not report the unread list after a create`);
    assert.equal(await page.getByRole("button", { name: "Retry", exact: true }).count(), 1, `${engine}: Retry was not offered after a create`);
  };

  try {
    await Promise.all([stallCase(), transferCase(), gateCase(), workspaceCase(), createCase(), createdCase()]);
    assert.deepEqual(pageErrors, [], `${engine}: page errors`);
    console.log(`${engine}: request stall recovery passed`);
  } finally {
    await browser.close();
    for (const timer of timers) clearTimeout(timer);
    for (const response of held) response.destroy();
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
}

(async () => {
  const only = process.env.PERSEA_STALL_ENGINE;
  for (const [engine, browserType] of [["chromium", chromium], ["webkit", webkit]]) if (!only || only === engine) await scenario(browserType, engine);
})().catch((error) => { console.error(error); process.exitCode = 1; });
