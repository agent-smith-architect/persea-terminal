"use strict";

// The terminal's ordered renderer and resume point, in a real browser. A
// geometry applies between the output around it, a new admission's reset
// follows the output queued before it, the page offers only a position its
// terminal has parsed, and a resumed admission is accepted only when it
// continues exactly what the terminal shows.
const fs = require("fs");
const http = require("http");
const path = require("path");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_RESUME_ENGINE || "chromium";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const CSP = `default-src 'self'; script-src 'self' 'nonce-${NONCE}'; style-src 'self' 'nonce-${NONCE}'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'`;
const STREAM = "ABCDEFGHIJKLMNOPQRSTUVWXYZ";
const HTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="persea-style-nonce" content="${NONCE}"><link rel="stylesheet" href="/app.css"><title>Resume order harness</title></head>
<body><main id="resume-host"></main><script nonce="${NONCE}" src="/resume_order_browser_harness.js"></script></body></html>`;

function assert(value, message) { if (!value) throw new Error(`RESUME_ORDER_ASSERTION: ${message}`); }
const same = (actual, expected, label) => assert(JSON.stringify(actual) === JSON.stringify(expected), `${label}: ${JSON.stringify(actual)}, want ${JSON.stringify(expected)}`);

async function main() {
  const playwright = require(MODULE);
  const files = new Map([
    ["/app.css", [path.join(UI, "dist/app.css"), "text/css"]],
    ["/resume_order_browser_harness.js", [path.join(UI, "dist/test/resume_order_browser_harness.js"), "text/javascript"]],
  ]);
  const server = http.createServer((request, response) => {
    response.setHeader("Content-Security-Policy", CSP);
    if (request.url === "/") { response.setHeader("Content-Type", "text/html"); response.end(HTML); return; }
    if (request.url === "/favicon.ico") { response.writeHead(204); response.end(); return; }
    if (request.method === "GET" && request.url === "/api/keyboard-preferences") {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("ETag", '"0"');
      response.end(JSON.stringify({ version: 1, layout: { bar: ["key:escape:0"], favorites: ["key:enter:0"] }, prefixes: { tmux: "key:b:1" }, revision: 0, stored: false, available: true }));
      return;
    }
    const entry = files.get(request.url);
    if (!entry) { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", entry[1]); response.end(fs.readFileSync(entry[0]));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  let browser;
  try {
    browser = await playwright[ENGINE].launch({ headless: true, ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}) });
    const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
    await page.goto(`http://127.0.0.1:${server.address().port}`, { waitUntil: "load" });
    await page.waitForFunction(() => document.body?.dataset.resumeReady === "true");
    const run = (name, ...args) => page.evaluate(([method, values]) => window.__resume[method](...values), [name, args]);

    const order = await run("geometryBetweenOutput");
    same(order.lines, ["first", `${" ".repeat(79)}X`, `${" ".repeat(119)}Y`], "geometry between output");
    assert(order.columns === 120 && order.queued === null && order.offer === `persea-resume.${STREAM}.4.23`, `geometry between output: ${JSON.stringify(order)}`);

    same((await run("resetAfterQueuedOutput")).lines, ["NEW"], "reset after queued output");

    const resumed = await run("resumeContinues");
    assert(resumed.offer === `persea-resume.${STREAM}.2.12`, `offer ${resumed.offer}`);
    same(resumed.resumed.lines, ["first-second-third"], "resumed admission");
    assert(resumed.resumed.finalized.length === 0 && resumed.resumed.ready === 1, `resumed admission: ${JSON.stringify(resumed.resumed)}`);
    same(resumed.after.lines, ["first-second-third-fourth"], "output after a resumed admission");
    assert(resumed.after.offer === `persea-resume.${STREAM}.3.25`, `point after a resumed admission: ${resumed.after.offer}`);

    for (const variant of ["never offered", "point moved", "output queued", "other stream", "other geometry", "position behind", "replay does not follow"]) {
      const refused = await run("resumeRefused", variant);
      same(refused.finalized.map((entry) => entry.cause), ["ADMISSION_INVARIANT"], variant);
      assert(refused.offer === null && refused.ready === 0, `${variant}: ${JSON.stringify(refused)}`);
    }

    const offer = await run("offerWaitsForTheRenderer");
    assert(offer.queued === null && offer.drained === `persea-resume.${STREAM}.2.10`, `offer while output is queued: ${JSON.stringify(offer)}`);

    const gap = await run("positionGap");
    same(gap.lines, ["first-more"], "position gap");
    assert(gap.offer === null && gap.finalized.length === 0, `position gap: ${JSON.stringify(gap)}`);

    assert(errors.length === 0, `browser errors: ${JSON.stringify(errors)}`);
    await context.close();
    console.log(`resume order ${ENGINE}: PASS (geometry between output, ordered reset, resumed admission, 7 refused resumes, offer timing, position gap)`);
  } finally {
    await browser?.close();
    await new Promise((resolve) => server.close(resolve));
  }
}

main().catch((error) => { console.error(error && error.stack || error); process.exitCode = 1; });
