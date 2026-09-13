"use strict";

const fs = require("fs");
const http = require("http");
const path = require("path");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_UX7_ENGINE || "chromium";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const EVIDENCE = path.resolve(process.env.PERSEA_UX7_EVIDENCE_DIR || path.join("/tmp", `persea-ux7-${ENGINE}`));
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const CSP = `default-src 'self'; script-src 'self' 'nonce-${NONCE}'; style-src 'self' 'nonce-${NONCE}'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'`;

function assert(value, message) { if (!value) throw new Error(message); }
function sameBox(a, b) { return a && b && Math.abs(a.x - b.x) < 0.1 && Math.abs(a.y - b.y) < 0.1 && Math.abs(a.width - b.width) < 0.1 && Math.abs(a.height - b.height) < 0.1; }

async function main() {
  assert(MODULE && path.isAbsolute(MODULE), "PERSEA_PLAYWRIGHT_MODULE must name the installed Playwright module");
  const playwright = require(MODULE);
  assert(playwright[ENGINE], `Playwright has no ${ENGINE} engine`);
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const html = fs.readFileSync(path.join(UI, "test/loading_state_browser_harness.html"), "utf8").replaceAll("__PERSEA_STYLE_NONCE__", NONCE);
  const files = new Map([
    ["/app.css", [path.join(UI, "dist/app.css"), "text/css"]],
    ["/loading_state_browser_harness.css", [path.join(UI, "test/loading_state_browser_harness.css"), "text/css"]],
    ["/loading_state_browser_harness.js", [path.join(UI, "dist/test/loading_state_browser_harness.js"), "text/javascript"]],
  ]);
  const server = http.createServer((request, response) => {
    response.setHeader("Content-Security-Policy", CSP);
    if (request.url === "/") { response.setHeader("Content-Type", "text/html"); response.end(html); return; }
    if (request.url === "/favicon.ico") { response.writeHead(204); response.end(); return; }
    if (request.method === "GET" && request.url === "/api/keyboard-preferences") {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("ETag", '"0"');
      response.end(JSON.stringify({
        version: 1,
        layout: {
          bar: ["key:escape:0", "key:tab:0", "modifier:ctrl", "key:arrow-left:0", "key:arrow-down:0", "key:arrow-up:0", "key:arrow-right:0"],
          favorites: ["key:c:1", "key:d:1", "key:enter:0", "key:home:0", "key:end:0", "key:page-up:0", "key:page-down:0", "key:f1:0"],
        },
        prefixes: { tmux: "key:b:1", screen: "key:a:1" },
        revision: 0, stored: false, available: true,
      }));
      return;
    }
    const entry = files.get(request.url);
    if (!entry) { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", entry[1]); response.end(fs.readFileSync(entry[0]));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  let browser;
  const evidence = { engine: ENGINE, cases: [] };
  try {
    browser = await playwright[ENGINE].launch({
      headless: true,
      ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
    });
    for (const scenario of [{ count: 1, viewport: { width: 1100, height: 760 }, label: "single-desktop" }, { count: 6, viewport: { width: 1280, height: 800 }, label: "workspace-six" }, { count: 1, viewport: { width: 390, height: 844 }, label: "single-phone" }]) {
      const context = await browser.newContext({ viewport: scenario.viewport });
      const page = await context.newPage();
      const pageErrors = []; const consoleErrors = [];
      page.on("pageerror", (error) => pageErrors.push(String(error)));
      page.on("console", (message) => { if (message.type() === "error") consoleErrors.push(message.text()); });
      await page.goto(origin, { waitUntil: "load" });
      await page.waitForFunction(() => document.body?.dataset.ux7Ready === "true");
      const mountStart = Date.now();
      const mounted = await page.evaluate((count) => window.__ux7.reset(count), scenario.count);
      await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => resolve(undefined))));
      const mountToFirstFrameMs = Date.now() - mountStart;
      const firstFrame = await page.evaluate(() => window.__ux7.snapshot());
      assert(firstFrame.panes.length === scenario.count && firstFrame.panes.every((pane) => pane.loadingVisible && pane.loadingState === "replaying" && pane.loadingText.includes(pane.name) && pane.loadingText.includes("Replaying")), `${scenario.label}: typed loading state missing within one frame: ${JSON.stringify(firstFrame)}`);
      assert(firstFrame.resizeRequests === 0 && firstFrame.inputFrames === 0 && firstFrame.focusEvents === 0 && firstFrame.transportOpens === scenario.count && firstFrame.socketConstructions === 0, `${scenario.label}: mount side effect: ${JSON.stringify(firstFrame)}`);
      const prepared = await page.evaluate(() => window.__ux7.prepareAll());
      await page.waitForTimeout(260);
      const delayed = await page.evaluate(() => window.__ux7.snapshot());
      assert(prepared.panes.every((pane) => pane.loadingVisible) && delayed.panes.every((pane) => pane.loadingVisible), `${scenario.label}: delayed COMMIT did not retain loading state`);
      assert(delayed.transportOpens === scenario.count && delayed.socketConstructions === 0 && delayed.resizeRequests === 0 && delayed.inputFrames === 0 && delayed.focusEvents === 0, `${scenario.label}: delayed state caused socket/transport/geometry/focus side effects`);
      const commitStart = Date.now();
      const committed = await page.evaluate(() => window.__ux7.commitAll());
      const commitTaskMs = Date.now() - commitStart;
      assert(committed.before.panes.every((pane) => pane.loadingVisible), `${scenario.label}: loader absent before COMMIT task`);
      assert(committed.after.panes.every((pane) => !pane.loadingVisible && pane.rendered.includes(`UX7-REPLAY-${pane.name}`)), `${scenario.label}: loader not removed synchronously with committed replay paint: ${JSON.stringify(committed.after.panes)}`);
      assert(committed.after.panes.every((pane, index) => sameBox(pane.toolbar, firstFrame.panes[index].toolbar)), `${scenario.label}: loading state shifted toolbar/header geometry`);
      assert(committed.after.resizeRequests === 0 && committed.after.inputFrames === 0 && committed.after.focusEvents === 0 && committed.after.transportOpens === scenario.count && committed.after.socketConstructions === 0, `${scenario.label}: COMMIT presentation side effect`);
      assert(pageErrors.length === 0 && consoleErrors.length === 0, `${scenario.label}: browser errors: ${JSON.stringify({ pageErrors, consoleErrors })}`);
      evidence.cases.push({ label: scenario.label, mountToFirstFrameMs, commitTaskMs, mounted, firstFrame, delayed, committed: committed.after, pageErrors, consoleErrors });
      await context.close();
    }

    const context = await browser.newContext({ viewport: { width: 1100, height: 760 } });
    const page = await context.newPage();
    await page.goto(origin, { waitUntil: "load" });
    await page.waitForFunction(() => document.body?.dataset.ux7Ready === "true");
    await page.evaluate(() => window.__ux7.reset(1));
    const failed = await page.evaluate(() => window.__ux7.failAll("unified_unavailable"));
    assert(failed.panes[0] && !failed.panes[0].loadingVisible && failed.panes[0].noticeVisible && failed.panes[0].noticeHeadline.length > 0, `typed failure left a blank/loading shell: ${JSON.stringify(failed)}`);
    assert(failed.resizeRequests === 0 && failed.inputFrames === 0 && failed.focusEvents === 0 && failed.transportOpens === 1 && failed.socketConstructions === 0, `typed failure presentation side effect: ${JSON.stringify(failed)}`);
    evidence.failure = failed;
    await page.evaluate(() => window.__ux7.reset(1));
    await page.evaluate(() => window.__ux7.prepareAll());
    await page.evaluate(() => window.__ux7.commitAll());
    await page.waitForFunction(() => window.__ux7.snapshot().panes[0]?.rendered.includes("UX7-REPLAY"));
    const exhausted = await page.evaluate(() => window.__ux7.exhaustAll());
    assert(exhausted.panes[0].reconnectVisible && exhausted.panes[0].rendered.includes("UX7-REPLAY"), `retry exhaustion must retain output and offer Reconnect: ${JSON.stringify(exhausted)}`);
    await page.evaluate(() => document.querySelector(".persea-unified-notice__reconnect").click());
    assert((await page.evaluate(() => window.__ux7.snapshot())).reconnectRequests === 0, "synthetic reconnect bypassed trusted activation");
    await page.getByRole("button", { name: "Reconnect", exact: true }).click();
    const reconnecting = await page.evaluate(() => window.__ux7.snapshot());
    assert(reconnecting.reconnectRequests === 1 && !reconnecting.panes[0].noticeVisible, "trusted reconnect did not start exactly one existing recovery cycle");
    assert(reconnecting.inputFrames === 0 && reconnecting.resizeRequests === 0, "manual recovery sent input or resized the session");
    for (const reason of ["session_gone", "identity_ambiguous", "source_binding_unavailable", "bad_liveness", "reconnect_unavailable"]) {
      const refused = await page.evaluate((reason) => window.__ux7.exhaustAll(reason), reason);
      assert(!refused.panes[0].reconnectVisible, `Reconnect must not bypass ${reason}`);
    }
    evidence.reconnect = { exhausted, reconnecting };
    await context.close();
    fs.writeFileSync(path.join(EVIDENCE, "loading-state.json"), JSON.stringify(evidence, null, 2));
    console.log(`UX7 loading state ${ENGINE}: PASS (${evidence.cases.length} layouts + typed failure)`);
  } finally {
    await browser?.close();
    await new Promise((resolve) => server.close(resolve));
  }
}

main().catch((error) => { console.error(error && error.stack || error); process.exitCode = 1; });
