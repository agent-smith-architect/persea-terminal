"use strict";

const fs = require("fs");
const path = require("path");
const https = require("https");
const assert = require("assert/strict");
const { chromium, webkit } = require("playwright");
const { startFixture } = require("./unified_reopen_fixture.cjs");

const UI = path.resolve(__dirname, "..");
const request = (url, body) => new Promise((resolve, reject) => {
  const req = https.request(url, { rejectUnauthorized: false, method: body ? "POST" : "GET", headers: body ? { "Content-Type": "application/json" } : {} }, res => {
    let text = "";
    res.on("data", part => { text += part; });
    res.on("end", () => { try { resolve(JSON.parse(text)); } catch (error) { reject(error); } });
  });
  req.on("error", reject);
  req.end(body ? JSON.stringify(body) : undefined);
});

async function main() {
  const results = [];
  const selectedEngine = process.env.PERSEA_RECONNECT_OUTAGE_ENGINE;
  assert(!selectedEngine || ["chromium", "webkit"].includes(selectedEngine), "unsupported browser engine");
  const selectedScenario = process.env.PERSEA_RECONNECT_OUTAGE_SCENARIO;
  const scenarios = ["no_server", "stale_socket", "error", "broker_down", "realm_missing", "server_missing", "healthy_absent", "new_identity"];
  assert(!selectedScenario || scenarios.includes(selectedScenario), "unsupported outage scenario");
  try {
    for (const [engine, browserType] of [["chromium", chromium], ["webkit", webkit]]) {
      if (selectedEngine && engine !== selectedEngine) continue;
      const browser = await browserType.launch({ headless: true, ...(engine === "chromium" ? { executablePath: process.env.CHROME_BIN, args: ["--no-sandbox"] } : {}) });
      try {
        for (const unavailable of scenarios) {
          if (selectedScenario && unavailable !== selectedScenario) continue;
          const fixture = await startFixture(UI, { tls: true });
          const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 800 } });
          const page = await context.newPage();
          const result = { engine, unavailable, console: [], pageErrors: [], outageReads: 0 };
          results.push(result);
          let phase = "healthy";
          let outage = false;
          let resolveSecondRead;
          const secondRead = new Promise(resolve => { resolveSecondRead = resolve; });
          const control = body => request(`${fixture.origin}/__fixture/control`, body);
          try {
            page.on("console", msg => result.console.push({ phase, type: msg.type(), text: msg.text(), location: msg.location() }));
            page.on("pageerror", error => result.pageErrors.push({ phase, error: String(error) }));
            const inventory = await request(`${fixture.origin}/api/inventory`);
            const session = inventory.realms[0].servers[0].sessions[0];
            await page.route("**/api/inventory", async route => {
              if (!outage) { await route.continue(); return; }
              result.outageReads++;
              const realm = { name: "local", display_name: "Local realm", servers: [] };
              if (["no_server", "stale_socket", "error"].includes(unavailable)) realm.servers.push({ label: "private", status: unavailable, can_create: false, sessions: [] });
              if (unavailable === "broker_down") {
                realm.error = "broker unavailable";
                realm.servers.push({ label: "private", status: "ok", sessions: [] });
              }
              if (unavailable === "healthy_absent") realm.servers.push({ label: "private", status: "ok", sessions: [] });
              realm.servers.push({ label: "other", status: "ok", sessions: [] });
              let body = { realms: unavailable === "realm_missing" ? [] : [realm], aliases: [] };
              body.realms.push({ name: "other", display_name: "Other realm", servers: [{ label: "private", status: "ok", sessions: [] }] });
              if (unavailable === "new_identity") {
                body = structuredClone(inventory);
                body.realms[0].servers[0].sessions[0].authority.server_start++;
              }
              await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
              if (result.outageReads === 2) resolveSecondRead();
            });
            await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" })}`);
            await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent.includes("fixture-live") && document.querySelector(".persea-unified-notice")?.hidden === true);
            result.attachmentsBefore = (await control({})).attachments.length;
            phase = "injected outage";
            outage = true;
            await control({ bindingsExpired: true, closeLive: "websocket_read" });
            if (unavailable === "healthy_absent" || unavailable === "new_identity") {
              await page.waitForFunction(() => document.querySelector(".persea-unified-notice__code")?.textContent.includes("session_gone"));
              assert.equal((await control({})).attachments.length, result.attachmentsBefore, "a new identity must not replace the pinned session");
            } else {
              // The second inventory proves that the first outage was transient.
              // A terminal notice is an immediate failure, without a timing guess.
              await Promise.race([
                secondRead,
                page.waitForFunction(() => document.querySelector(".persea-unified-notice__code")?.textContent.includes("session_gone")).then(() => { throw new Error(`${unavailable} incorrectly became session_gone`); }),
              ]);
              assert(!await page.locator(".persea-unified-notice__code").textContent().then(text => text.includes("session_gone")));
              await control({ bindingsExpired: false });
              outage = false;
              phase = "recovery";
              await page.waitForFunction(() => document.querySelector(".persea-unified-notice")?.hidden === true && document.querySelector(".persea-unified-tag__dot")?.dataset.state === "live");
              assert((await control({})).attachments.length > result.attachmentsBefore, "recovery must create a new attachment to the same session");
            }
            assert.equal(result.pageErrors.length, 0, JSON.stringify(result.pageErrors));
            const unexpected = result.console.filter(entry => !(entry.phase === "injected outage" && entry.type === "error" && entry.text.includes("410") && entry.location.url.includes("/api/attachment-handles")));
            assert.equal(unexpected.length, 0, JSON.stringify(unexpected));
            result.passed = true;
          } finally {
            await context.close();
            await fixture.close();
          }
        }
      } finally { await browser.close(); }
    }
  } finally {
    if (process.env.PERSEA_RECONNECT_OUTAGE_EVIDENCE) fs.writeFileSync(process.env.PERSEA_RECONNECT_OUTAGE_EVIDENCE, JSON.stringify(results, null, 2) + "\n");
    console.log(JSON.stringify(results));
  }
}

main().catch(error => { console.error(error.stack || error); process.exitCode = 1; });
