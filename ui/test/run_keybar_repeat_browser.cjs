"use strict";
const fs = require("fs");
const path = require("path");
const http = require("http");
const https = require("https");
const { startFixture } = require("./unified_reopen_fixture.cjs");
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const UI = process.env.PERSEA_KEYS_UI || path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_KEYS_ENGINE || "chromium";
const EVIDENCE = process.env.PERSEA_KEYBAR_EVIDENCE || require("node:path").join(require("node:os").tmpdir(), "persea-terminal-tests", "run_keybar_repeat_browser");
const CASE = process.env.PERSEA_KEYBAR_CASE || "all";
const delay = (ms) => new Promise(resolve => setTimeout(resolve, ms));
function json(url, method = "GET", body) {
  return new Promise((resolve, reject) => {
    const request = (url.startsWith("https:") ? https : http).request(url, { method, rejectUnauthorized: false, headers: body ? { "Content-Type": "application/json" } : {} }, response => {
      let data = ""; response.on("data", chunk => { data += chunk; });
      response.on("end", () => { try { resolve(JSON.parse(data)); } catch (error) { reject(error); } });
    });
    request.on("error", reject); if (body) request.write(JSON.stringify(body)); request.end();
  });
}
async function main() {
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const fixture = await startFixture(UI, { tls: ENGINE === "webkit", playwrightScreenshotStyle: true });
  const browser = await playwright[ENGINE].launch({ headless: true, ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}) });
  const report = { engine: ENGINE, case: CASE, pages: [], passed: false };
  const control = body => json(`${fixture.origin}/__fixture/control`, "POST", body);
  const snapshot = () => json(`${fixture.origin}/__fixture/control`);
  try {
    const shapes = process.env.PERSEA_KEYS_ONE_SHAPE ? [[390, 844]] : [[320, 568], [390, 844], [430, 932], [844, 390], [1440, 1000]];
    for (const [width, height] of shapes) {
      await control({ reset: true });
      const context = await browser.newContext({ viewport: { width, height }, hasTouch: true, isMobile: true, ignoreHTTPSErrors: true });
      const page = await context.newPage();
      const results = { width, height, assertions: [], holds: [], console: [] }; report.pages.push(results);
      const assert = (value, message) => { if (!value) throw new Error(`${width}x${height}: ${message}`); results.assertions.push(message); };
      page.on("console", message => results.console.push(`${message.type()}: ${message.text()}`));
      page.on("pageerror", error => results.console.push(String(error)));
      const inventory = await json(`${fixture.origin}/api/inventory`);
      const session = inventory.realms[0].servers[0].sessions[0];
      await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" })}`);
      const helper = page.locator(".persea-unified-xterm .xterm-helper-textarea");
      const panel = page.locator(".persea-terminal-keys");
      const ctrl = page.locator('[data-bar-modifier="ctrl"]');
      const bank = page.locator(".persea-unified-keybar-key--bank");
      const arrow = direction => page.locator(`[data-bar-key="key:arrow-${direction}:0"]`);
      const inputs = async () => (await snapshot()).attachments.flatMap(a => a.inputs).join("");
      await helper.waitFor();
      for (let i = 0; i < 100 && !(await snapshot()).attachments.some(a => a.mode === "CONTROL"); i++) await delay(30);
      await helper.focus();
      await delay(100);
      await page.setViewportSize({ width, height: Math.max(220, height - 336) });
      await ctrl.waitFor({ state: "visible" });
      await delay(150);
      await page.evaluate(() => {
        window.__barArrowCount = 0; window.__barArrowsAtRelease = 0;
        document.addEventListener("keydown", event => { if (/^Arrow/.test(event.key)) window.__barArrowCount++; }, true);
        document.addEventListener("pointerup", () => { window.__barArrowsAtRelease = window.__barArrowCount; }, true);
      });
      assert(!await panel.isVisible(), "Keyboard accessory starts with Keys closed");
      const press = async locator => {
        await locator.scrollIntoViewIfNeeded();
        const box = await locator.boundingBox();
        if (!box) throw new Error("Missing key geometry");
        const point = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
        await page.mouse.move(point.x, point.y); await page.mouse.down(); return point;
      };
      if (CASE !== "repeat") {
        const before = await inputs();
        await helper.evaluate(node => { node.value = "native draft"; node.setSelectionRange(2, 5); window.__barFocusEvents = []; node.addEventListener("focus", () => window.__barFocusEvents.push("focus")); node.addEventListener("blur", () => window.__barFocusEvents.push("blur")); });
        const geometry = await panel.evaluate(node => node.parentElement.getBoundingClientRect().height);
        await ctrl.tap();
        assert(!await panel.isVisible(), "Ctrl latches without opening Keys");
        assert(await ctrl.getAttribute("aria-pressed") === "true", "Ctrl visibly latches");
        assert(await helper.evaluate(node => node === document.activeElement && node.value === "native draft" && node.selectionStart === 2 && node.selectionEnd === 5 && window.__barFocusEvents.length === 0), "Ctrl preserves editor focus, draft and selection");
        assert(await panel.evaluate(node => node.parentElement.getBoundingClientRect().height) === geometry, "Ctrl does not change terminal layout");
        await page.screenshot({ path: path.join(EVIDENCE, `ctrl-latched-${ENGINE}-${width}x${height}.png`), caret: "initial" });
        await ctrl.tap();
        assert(!await panel.isVisible() && await ctrl.getAttribute("aria-pressed") === "false" && await inputs() === before, "Second Ctrl tap cancels without input or menu");
        await helper.evaluate(node => { node.value = ""; });
        await ctrl.tap(); await page.keyboard.press("c"); await delay(60);
        assert(await inputs() === before + "\x03", "Closed-panel Ctrl plus typed C sends Ctrl+C once");
        assert(await ctrl.getAttribute("aria-pressed") === "false" && !await panel.isVisible(), "Typed chord consumes latch and keeps Keys closed");
        await page.keyboard.press("c"); await delay(60);
        assert(await inputs() === before + "\x03c", "Next typed letter is unmodified");
        await ctrl.tap(); await page.keyboard.insertText("d"); await delay(60);
        assert(await inputs() === before + "\x03c\x04", "Native text insertion consumes the closed-panel Ctrl latch");
        await ctrl.tap(); await page.keyboard.insertText("paste"); await delay(60);
        assert(await inputs() === before + "\x03c\x04paste" && await ctrl.getAttribute("aria-pressed") === "false", "Multiple-character input stays intact and disarms Ctrl");
        await ctrl.tap();
        await page.locator(".persea-unified-xterm .xterm-screen").tap({ position: { x: 12, y: 12 } });
        assert(await ctrl.getAttribute("aria-pressed") === "false", "Terminal pointer cancels the closed-panel latch visibly");
        await bank.tap(); assert(await panel.isVisible(), "Dedicated Keys button still opens the menu");
        await bank.tap(); assert(!await panel.isVisible(), "Dedicated Keys button closes the menu");
      }
      if (CASE !== "modifier") {
        await helper.focus();
        for (const application of [false, true]) {
          await control({ writeLive: application ? "\x1b[?1h" : "\x1b[?1l" }); await delay(70);
          for (const [direction, suffix] of [["left", "D"], ["right", "C"], ["up", "A"], ["down", "B"]]) {
            const sequence = `\x1b${application ? "O" : "["}${suffix}`;
            let before = await inputs();
            await arrow(direction).tap(); await delay(50);
            assert(await inputs() === before + sequence, `${direction} quick tap sends one ${application ? "application" : "normal"} arrow`);
            before = await inputs();
            const arrowsBefore = await page.evaluate(() => window.__barArrowCount);
            await press(arrow(direction)); await delay(200);
            assert(await inputs() === before, `${direction} permits drag cancellation before hold delay`);
            await delay(560);
            const held = (await inputs()).slice(before.length), count = held.length / sequence.length;
            assert(Number.isInteger(count) && count >= 3 && count <= 12 && held === sequence.repeat(count), `${direction} repeats the current cursor mode while held`);
            await page.mouse.up(); await delay(180);
            const release = await page.evaluate(() => ({ atRelease: window.__barArrowsAtRelease, now: window.__barArrowCount }));
            assert(release.now === release.atRelease && await inputs() === before + sequence.repeat(release.atRelease - arrowsBefore), `${direction} release stops repeat without an extra arrow`);
            results.holds.push({ direction, application, count });
          }
        }
        await control({ writeLive: "\x1b[?1l" }); await delay(50);
        await ctrl.tap();
        let before = await inputs();
        const modifiedBefore = await page.evaluate(() => window.__barArrowCount);
        await press(arrow("left")); await delay(760);
        const modified = (await inputs()).slice(before.length), modifiedCount = modified.length / 6;
        assert(Number.isInteger(modifiedCount) && modifiedCount >= 3 && modified === "\x1b[1;5D".repeat(modifiedCount), "Held Ctrl+Left keeps the same modifier for every repeat");
        await page.mouse.up(); await delay(150);
        const modifiedRelease = await page.evaluate(() => ({ atRelease: window.__barArrowsAtRelease, now: window.__barArrowCount }));
        assert(modifiedRelease.now === modifiedRelease.atRelease && await inputs() === before + "\x1b[1;5D".repeat(modifiedRelease.atRelease - modifiedBefore) && await ctrl.getAttribute("aria-pressed") === "false", "Modified hold releases and consumes Ctrl once");
        before = await inputs();
        const point = await press(arrow("left"));
        await page.mouse.move(point.x + 14, point.y, { steps: 3 }); await delay(550); await page.mouse.up(); await delay(50);
        assert(await inputs() === before, "Dragging before hold cancels the entire gesture");
        await press(arrow("left")); await delay(600); await page.mouse.move(1, 1);
        const left = await inputs(); await delay(200); await page.mouse.up(); await delay(60);
        assert(left.length > before.length && await inputs() === left, "Leaving a held key stops repeat");
        await press(arrow("left")); await delay(550); await bank.focus();
        const blurred = await inputs(); await delay(200); await page.mouse.up(); await delay(60);
        assert(await inputs() === blurred, "Moving focus stops held-key input");
        await helper.focus();
        // CDP delivers trusted touch streams, including the browser's cancel
        // event. WebKit uses trusted taps and mouse holds above.
        if (ENGINE === "chromium" && width === 390) {
          const cdp = await context.newCDPSession(page);
          await arrow("left").scrollIntoViewIfNeeded(); const box = await arrow("left").boundingBox();
          const touch = { x: box.x + box.width / 2, y: box.y + box.height / 2, id: 1 };
          before = await inputs();
          await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [touch] }); await delay(680);
          const held = await inputs(); assert(held.length >= before.length + 9, "Trusted touch hold repeats before release");
          await cdp.send("Input.dispatchTouchEvent", { type: "touchCancel", touchPoints: [] }); await delay(200);
          assert(await inputs() === held, "Trusted touch cancellation stops repeat");
          await cdp.detach();
        }
        await press(arrow("left")); await delay(560);
        await control({ closeLive: "transport_lost", holdPrepareMs: 1000 }); await delay(100);
        const disconnected = await inputs(); await delay(550); await page.mouse.up(); await delay(80);
        assert(await inputs() === disconnected, "Transport change cancels a hold without replay");
      }
      const end = await snapshot();
      assert(end.attachments.every(a => a.resizes === 0 && a.resizeRequests.length === 0), "Keybar gestures never resize the remote terminal");
      assert(results.console.length === 0, `Clean browser console: ${results.console.join("; ")}`);
      await page.screenshot({ path: path.join(EVIDENCE, `keybar-${ENGINE}-${width}x${height}-${CASE}.png`), caret: "initial" });
      await context.close();
    }
    report.passed = true;
    console.log(`PASS keybar ${ENGINE}: ${report.pages.length} shapes, ${report.pages.reduce((n, p) => n + p.assertions.length, 0)} assertions`);
  } catch (error) { report.failure = String(error.stack || error); throw error; }
  finally {
    fs.writeFileSync(path.join(EVIDENCE, `keybar-${ENGINE}-${CASE}.json`), JSON.stringify(report, null, 2) + "\n");
    await browser.close(); await fixture.close();
  }
}
main().catch(error => { console.error(error.stack || error); process.exitCode = 1; });
