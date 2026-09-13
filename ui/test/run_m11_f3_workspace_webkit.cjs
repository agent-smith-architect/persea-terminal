"use strict";

// M11LF-F3 WebKit falsifier. The real WorkspacePage and six real pane
// controllers run over TLS because WebKit rejects the product's Secure CSRF
// cookie on a plaintext origin.

const fs = require("fs");
const https = require("https");
const path = require("path");
const { startWorkspaceFixture, STYLE_NONCE } = require("./workspace_fixture.cjs");

const UI = path.resolve(__dirname, "..");
const SESSIONS = ["ws01", "ws02", "ws03", "ws04", "ws05", "ws06"];

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function requestJSON(url, method = "GET", body) {
  return new Promise((resolve, reject) => {
    const payload = body === undefined ? undefined : Buffer.from(JSON.stringify(body));
    const request = https.request(url, {
      method,
      rejectUnauthorized: false,
      headers: payload ? { "Content-Type": "application/json", "Content-Length": payload.length } : {},
    }, (response) => {
      const chunks = [];
      response.on("data", (chunk) => chunks.push(chunk));
      response.on("end", () => {
        const text = Buffer.concat(chunks).toString("utf8");
        if ((response.statusCode || 500) >= 400) { reject(new Error(`${method} ${url}: ${response.statusCode} ${text}`)); return; }
        try { resolve(text ? JSON.parse(text) : {}); } catch (error) { reject(error); }
      });
    });
    request.on("error", reject);
    if (payload) request.write(payload);
    request.end();
  });
}

function arrangementFor(names) {
  const leaves = names.map((name) => ({ kind: "leaf", session: { realm: "local", server: "private", name }, on_missing: "offer" }));
  return JSON.stringify({ version: 1, root: {
    kind: "split", direction: "column", weights: [1, 1], children: [
      { kind: "split", direction: "row", weights: [1, 1, 1], children: leaves.slice(0, 3) },
      { kind: "split", direction: "row", weights: [1, 1, 1], children: leaves.slice(3) },
    ],
  } });
}

const countersEqual = (left, right) => ["websockets", "handleRequests", "takeovers", "adoptions"]
  .every((key) => left[key] === right[key]);

// A deferral an operator can read: both reviewed phrases in the cell's
// rendered text, carried by a notice that is laid out.
const readsDeferral = (cell) => !!cell && cell.readsBadgePhrase && cell.readsDeferralSentence
  && !!cell.notice && cell.notice.hidden === false && cell.notice.display !== "none" && cell.notice.area > 0;

async function main() {
  const modulePath = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
  assert(modulePath && path.isAbsolute(modulePath), "PERSEA_PLAYWRIGHT_MODULE must be an absolute Playwright module path");
  const playwright = require(modulePath);
  const harnessIndex = fs.readFileSync(path.join(UI, "test/workspace_page_browser_harness.html"), "utf8").replace("__PERSEA_STYLE_NONCE__", STYLE_NONCE);
  const fixture = await startWorkspaceFixture(UI, {
    tls: true,
    documents: {
      "/workspace-harness": { body: () => harnessIndex },
      "/workspace_page_harness.js": { file: path.join(UI, "dist/test/workspace_page_browser_harness.js"), type: "text/javascript" },
      "/workspace_page_harness.css": { file: path.join(UI, "dist/test/workspace_page_browser_harness.css"), type: "text/css" },
    },
  });
  let browser;
  let context;
  const browserMessages = [];
  try {
    browser = await playwright.webkit.launch({ headless: true });
    context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 800 } });
    const page = await context.newPage();
    page.on("console", (message) => browserMessages.push(`console.${message.type()}: ${message.text()}`));
    page.on("pageerror", (error) => browserMessages.push(`pageerror: ${error.message}`));
    await page.goto(`${fixture.origin}/workspace-harness`, { waitUntil: "load" });
    await page.waitForFunction(() => document.body.dataset.wsHarnessReady === "true");
    await page.evaluate((serialized) => window.__wsPage.open(serialized), arrangementFor(SESSIONS));
    await page.waitForFunction((count) => {
      const panes = window.__wsPage.panes();
      return panes.length === count && panes.every((pane) => pane.state === "live");
    }, SESSIONS.length);

    const before = await requestJSON(`${fixture.origin}/__fixture/control`);
    await page.evaluate(() => window.__wsPage.savePresentationSnapshot());
    await requestJSON(`${fixture.origin}/__fixture/control`, "POST", { session: "ws03", detail: "rotation_deferred_alt_screen" });
    await page.evaluate(() => window.__wsPage.refreshPresentation());
    const decorated = await page.evaluate(() => ({
      panes: window.__wsPage.panes(),
      cells: [...document.querySelectorAll(".ws-cell")].map((cell) => ({
        name: cell.dataset.wsSession,
        state: cell.dataset.wsState,
        badge: cell.querySelector(".ws-cell__badge")?.textContent || "",
        overlayHidden: cell.querySelector(".ws-cell__state")?.hidden,
        // A live cell hides its state overlay (so the terminal stays visible)
        // and its header badge (workspace.css), so textContent alone proves
        // only that the detail reached the DOM. These fields prove it reached
        // the operator.
        wsDetail: cell.dataset.wsDetail || null,
        notice: (() => { const el = cell.querySelector(".ws-cell__notice"); if (!el) return null; const r = el.getBoundingClientRect(); return { hidden: el.hidden, display: getComputedStyle(el).display, text: (el.textContent || "").trim(), area: Math.round(r.width) * Math.round(r.height) }; })(),
        readsBadgePhrase: /rotation deferred/i.test(cell.innerText), readsDeferralSentence: /full-screen app is active/i.test(cell.innerText),
      })),
    }));
    const ws03 = decorated.cells.find((cell) => cell.name === "ws03");
    assert(ws03 && ws03.state === "live" && ws03.badge === "rotation deferred" && ws03.overlayHidden === true,
      `exact cell was not decorated in place: ${JSON.stringify(ws03)}`);
    assert(readsDeferral(ws03) && ws03.wsDetail === "rotation_deferred_alt_screen"
      && /rotation deferred/i.test(ws03.notice.text) && /full-screen app is active/i.test(ws03.notice.text),
    `the deferral reached the DOM but not the operator: ${JSON.stringify(ws03)}`);
    assert(decorated.cells.filter((cell) => cell.name !== "ws03").every((cell) => cell.wsDetail === null && !cell.readsBadgePhrase && (cell.notice === null || cell.notice.hidden === true)),
      `a sibling cell rendered a deferral notice: ${JSON.stringify(decorated.cells.map((cell) => [cell.name, cell.wsDetail, cell.notice && cell.notice.hidden]))}`);
    assert(decorated.cells.filter((cell) => cell.name !== "ws03").every((cell) => cell.badge === "80×24"),
      `a sibling cell changed: ${JSON.stringify(decorated.cells)}`);
    assert(decorated.panes.find((pane) => pane.key.includes('"ws03"'))?.controllerDetail === "rotation_deferred_alt_screen",
      `exact controller missed detail: ${JSON.stringify(decorated.panes)}`);
    let after = await requestJSON(`${fixture.origin}/__fixture/control`);
    assert(countersEqual(before.counters, after.counters) && after.attachments.every((entry) => entry.inputs.length === 0 && entry.resizes === 0),
      `presentation update spent authority: ${JSON.stringify({ before: before.counters, after: after.counters })}`);

    await page.evaluate(() => window.__wsPage.refreshSavedPresentation());
    assert(await page.evaluate(() => document.querySelector('.ws-cell[data-ws-session="ws03"] .ws-cell__badge')?.textContent === "rotation deferred"),
      "a stale inventory refresh overwrote newer detail");

    await requestJSON(`${fixture.origin}/__fixture/control`, "POST", { session: "ws03", detail: null });
    await page.evaluate(() => window.__wsPage.refreshPresentation());
    assert(await page.evaluate(() => document.querySelector('.ws-cell[data-ws-session="ws03"] .ws-cell__badge')?.textContent === "80×24"),
      "clearing detail did not restore the geometry badge");
    assert(await page.evaluate(() => {
      const cell = document.querySelector('.ws-cell[data-ws-session="ws03"]');
      const notice = cell.querySelector(".ws-cell__notice");
      // Absent is as cleared as hidden; the positive assert above is what fails
      // when a product shows the operator nothing at all.
      return (notice === null || notice.hidden === true) && !cell.dataset.wsDetail && !/rotation deferred/i.test(cell.innerText);
    }), "clearing the detail left the deferral notice on the operator's screen");
    after = await requestJSON(`${fixture.origin}/__fixture/control`);
    assert(countersEqual(before.counters, after.counters), "clearing detail spent authority");

    await requestJSON(`${fixture.origin}/__fixture/control`, "POST", { session: "ws03", closeLive: "generation_rotated" });
    await page.waitForFunction((previous) => window.__wsPage.panes()
      .find((pane) => pane.key.includes('"ws03"'))?.generation === previous + 1, decorated.panes.find((pane) => pane.key.includes('"ws03"')).generation);
    after = await requestJSON(`${fixture.origin}/__fixture/control`);
    const ws03Attachments = after.attachments.filter((entry) => entry.session === "ws03");
    assert(after.counters.websockets === before.counters.websockets + 1 && ws03Attachments.length === 2 && ws03Attachments.filter((entry) => entry.live).length === 1,
      `typed rotation close did not cause exactly one reattach: ${JSON.stringify(ws03Attachments)}`);

    const beforeReplacement = after;
    await requestJSON(`${fixture.origin}/__fixture/control`, "POST", { replaceSession: "ws03", replacementDetail: "rotation_deferred_alt_screen" });
    await page.evaluate(() => window.__wsPage.refreshPresentation());
    const replacement = await page.evaluate(() => {
      const cell = document.querySelector('.ws-cell[data-ws-session="ws03"]');
      return {
        badge: cell.querySelector(".ws-cell__badge")?.textContent,
        wsDetail: cell.dataset.wsDetail || null,
        noticeHidden: cell.querySelector(".ws-cell__notice").hidden,
        readsBadgePhrase: /rotation deferred/i.test(cell.innerText),
        pane: window.__wsPage.panes().find((entry) => entry.key.includes('"ws03"')),
      };
    });
    after = await requestJSON(`${fixture.origin}/__fixture/control`);
    assert(replacement.badge === "80×24" && replacement.pane.controllerDetail === null
      && replacement.wsDetail === null && replacement.noticeHidden === true && !replacement.readsBadgePhrase,
    `same-name successor decorated stale controller: ${JSON.stringify(replacement)}`);
    assert(countersEqual(beforeReplacement.counters, after.counters), "same-name successor detail spent authority");
    assert(browserMessages.length === 0, `browser console was not clean: ${JSON.stringify(browserMessages)}`);
    console.log(JSON.stringify({ status: "PASS", engine: "webkit", falsifier: "M11LF-F3", counters: after.counters }));
  } finally {
    if (context) await context.close().catch(() => undefined);
    if (browser) await browser.close().catch(() => undefined);
    await fixture.close();
  }
}

main().catch((error) => { console.error(error.stack || error); process.exitCode = 1; });
