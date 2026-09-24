"use strict";

const fs = require("fs");
const http = require("http");
const path = require("path");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_REFIT_ACCOUNTING_ENGINE || process.env.PERSEA_TERMINAL_INTERACTION_ENGINE || "chromium";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const CSP = `default-src 'self'; script-src 'self' 'nonce-${NONCE}'; style-src 'self' 'nonce-${NONCE}'; connect-src 'self'; object-src 'none'`;

async function main() {
  if (!path.isAbsolute(MODULE)) throw new Error("PERSEA_PLAYWRIGHT_MODULE must be absolute");
  const playwright = require(MODULE);
  if (!playwright[ENGINE]) throw new Error(`Playwright has no ${ENGINE} engine`);
  const html = fs.readFileSync(path.join(__dirname, "refit_accounting_browser_harness.html"), "utf8").replaceAll("__PERSEA_STYLE_NONCE__", NONCE);
  const files = new Map([
    ["/app.css", [path.join(UI, "dist/app.css"), "text/css"]],
    ["/refit_accounting_browser_harness.js", [path.join(UI, "dist/test/refit_accounting_browser_harness.js"), "text/javascript"]],
  ]);
  const server = http.createServer((request, response) => {
    response.setHeader("Content-Security-Policy", CSP);
    if (request.url === "/") {
      response.setHeader("Content-Type", "text/html");
      response.end(html);
      return;
    }
    const entry = files.get(request.url);
    if (!entry) {
      response.writeHead(request.url === "/favicon.ico" ? 204 : 404);
      response.end();
      return;
    }
    response.setHeader("Content-Type", entry[1]);
    response.end(fs.readFileSync(entry[0]));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  let browser;
  try {
    browser = await playwright[ENGINE].launch({
      headless: true,
      ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
    });
    const page = await browser.newPage({ viewport: { width: 1100, height: 760 } });
    const errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.goto(`http://127.0.0.1:${server.address().port}`, { waitUntil: "load" });
    await page.waitForFunction(() => document.body.dataset.ready === "true");
    const result = await page.evaluate(() => window.__refitRefitAccounting.run());
    if (errors.length > 0) throw new Error(`page errors: ${JSON.stringify(errors)}`);
    console.log(JSON.stringify({ status: "PASS", engine: ENGINE, result }));
  } finally {
    await browser?.close();
    await new Promise((resolve) => server.close(resolve));
  }
}

main().catch((error) => {
  console.error(error?.stack || error);
  process.exitCode = 1;
});
