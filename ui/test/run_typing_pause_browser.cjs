"use strict";

// The typing pause, in a real browser. After an input_paused refusal the page
// must send nothing — no keystroke, paste or composer Insert — until the
// operator presses Resume typing; the notice must outlast a passing refusal;
// the press must restore sending without replaying anything; and the pause
// must survive a reconnect of the same session but not a session switch.
const fs = require("fs");
const http = require("http");
const path = require("path");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_TYPING_PAUSE_ENGINE || "chromium";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const EVIDENCE = path.resolve(process.env.PERSEA_TYPING_PAUSE_EVIDENCE_DIR || path.join(process.env.TMPDIR || "/tmp", `persea-typing_pause-${ENGINE}`));
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const CSP = `default-src 'self'; script-src 'self' 'nonce-${NONCE}'; style-src 'self' 'nonce-${NONCE}'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'`;
const PAUSED_MESSAGE = "Catching up — what you typed was not sent (input_paused)";
const PAUSED_REASON = "Typing is paused. Press Resume typing first.";
// The passing-notice lifetime, read from the source so the check cannot drift.
const REFUSAL_NOTICE_MS = Number(fs.readFileSync(path.join(UI, "src/unified_refusal_notice.ts"), "utf8").match(/REFUSAL_NOTICE_MS = ([\d_]+);/)[1].replaceAll("_", ""));
const SHAPES = Object.freeze([
  { name: "desktop-1100", width: 1100, height: 760, touch: false },
  { name: "phone-390", width: 390, height: 844, touch: true },
  { name: "phone-320", width: 320, height: 568, touch: true },
]);

function assert(value, message) { if (!value) throw new Error(`TYPING_PAUSE_ASSERTION: ${message}`); }

function colorChannels(value) {
  const numbers = String(value).match(/[\d.]+/g)?.map(Number) || [];
  if (String(value).startsWith("color(srgb") && numbers.length >= 3) return numbers.slice(0, 3).map((part) => part * 255);
  return numbers.slice(0, 3);
}
function contrast(a, b) {
  const luminance = (value) => {
    const channels = colorChannels(value);
    assert(channels.length === 3, `unreadable computed color ${value}`);
    const linear = channels.map((part) => {
      const unit = part / 255;
      return unit <= 0.04045 ? unit / 12.92 : ((unit + 0.055) / 1.055) ** 2.4;
    });
    return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
  };
  const [bright, dark] = [luminance(a), luminance(b)].sort((left, right) => right - left);
  return (bright + 0.05) / (dark + 0.05);
}

async function until(label, read, timeout = 5_000) {
  const deadline = Date.now() + timeout;
  let last;
  while (Date.now() < deadline) {
    last = await read();
    if (last) return last;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error(`TYPING_PAUSE_ASSERTION: ${label} timed out`);
}

async function main() {
  assert(MODULE && path.isAbsolute(MODULE), "PERSEA_PLAYWRIGHT_MODULE must name the installed Playwright module");
  assert(REFUSAL_NOTICE_MS > 0, "could not read REFUSAL_NOTICE_MS");
  const playwright = require(MODULE);
  assert(playwright[ENGINE], `Playwright has no ${ENGINE} engine`);
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const html = fs.readFileSync(path.join(UI, "test/typing_pause_browser_harness.html"), "utf8").replaceAll("__PERSEA_STYLE_NONCE__", NONCE);
  const files = new Map([
    ["/app.css", [path.join(UI, "dist/app.css"), "text/css"]],
    ["/typing_pause_browser_harness.css", [path.join(UI, "test/typing_pause_browser_harness.css"), "text/css"]],
    ["/typing_pause_browser_harness.js", [path.join(UI, "dist/test/typing_pause_browser_harness.js"), "text/javascript"]],
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
  const evidence = { engine: ENGINE, refusalNoticeMs: REFUSAL_NOTICE_MS, shapes: [] };
  try {
    browser = await playwright[ENGINE].launch({
      headless: true,
      ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
    });
    for (const shape of SHAPES) {
      const context = await browser.newContext({ viewport: { width: shape.width, height: shape.height }, hasTouch: shape.touch, isMobile: shape.touch, deviceScaleFactor: shape.touch ? 2 : 1 });
      const page = await context.newPage();
      const pageErrors = []; const consoleErrors = [];
      page.on("pageerror", (error) => pageErrors.push(String(error)));
      page.on("console", (message) => { if (message.type() === "error") consoleErrors.push(message.text()); });
      await page.goto(origin, { waitUntil: "load" });
      await page.waitForFunction(() => document.body?.dataset.typingPauseReady === "true");
      const state = () => page.evaluate(() => window.__typing_pause.snapshot());
      const inputs = async () => (await state()).inputs;
      const focusTerminal = () => page.locator("#typing-host .xterm-helper-textarea").focus();
      // Screenshots are Chromium evidence only: WebKit's capture applies a
      // stylesheet this page's style policy refuses, which would surface as a
      // console error the gate rightly fails on.
      const shot = async (name) => {
        if (ENGINE === "chromium") await page.screenshot({ path: path.join(EVIDENCE, `${shape.name}-${name}.png`), caret: "initial" });
      };
      // Trusted activation as the shape's pointer makes it.
      const press = (locator, options = {}) => shape.touch ? locator.tap(options) : locator.click(options);
      const slotTop = await page.evaluate(() => 3.25 * parseFloat(getComputedStyle(document.documentElement).fontSize));
      const caseEvidence = { shape: shape.name };
      const expectPaused = (value, label) => {
        assert(value.paused.shown && value.paused.hidden === false, `${shape.name}: ${label}: the paused notice is not shown: ${JSON.stringify(value.paused)}`);
        assert(value.paused.message === PAUSED_MESSAGE, `${shape.name}: ${label}: paused message ${JSON.stringify(value.paused.message)}`);
        assert(value.paused.buttonText === "Resume typing" && value.paused.buttonType === "button", `${shape.name}: ${label}: Resume button ${JSON.stringify(value.paused.buttonText)}`);
      };
      // A passing notice shown while paused lands below the paused notice and
      // inside the page, so it never covers the Resume button.
      const expectStacked = async (label) => {
        const stacked = await until(`${shape.name}: ${label}: passing notice below the paused notice`, async () => {
          const value = await state();
          return value.strip.shown && value.paused.shown && value.strip.box.top >= value.paused.box.bottom ? value : undefined;
        });
        assert(stacked.strip.box.left >= 0 && stacked.strip.box.right <= shape.width, `${shape.name}: ${label}: passing notice outside the page: ${JSON.stringify(stacked.strip.box)}`);
        return stacked;
      };

      await page.evaluate(() => window.__typing_pause.reset());
      await page.evaluate(() => window.__typing_pause.admit());
      await focusTerminal();
      await page.keyboard.type("aw");
      assert(JSON.stringify(await inputs()) === '["a","w"]', `${shape.name}: an admitted page did not send keystrokes: ${JSON.stringify(await inputs())}`);

      // A refusal for an earlier generation belongs to a socket that is gone.
      const stale = await page.evaluate(() => window.__typing_pause.refuse("input_paused", -1));
      assert(!stale.paused.shown && stale.paused.hidden === true, `${shape.name}: a stale-generation refusal paused typing`);

      const pausedAt = Date.now();
      const paused = await page.evaluate(() => window.__typing_pause.refuse("input_paused"));
      expectPaused(paused, "after input_paused");
      assert(paused.paused.text === `${PAUSED_MESSAGE}Resume typing` && paused.paused.messageRole === "status" && paused.paused.messageLive === "polite",
        `${shape.name}: the paused notice's message and button are not separate: ${JSON.stringify(paused.paused)}`);
      assert(!paused.strip.shown, `${shape.name}: input_paused also showed the passing strip: ${JSON.stringify(paused.strip)}`);
      const availability = await page.evaluate(() => window.__typing_pause.composerAvailability());
      assert(availability.canInject === false && availability.reason === PAUSED_REASON, `${shape.name}: composer availability while paused: ${JSON.stringify(availability)}`);
      // Layout: the strip's slot, inside the page, a full touch target, and a
      // message that wraps rather than clips.
      assert(Math.abs(paused.paused.box.top - paused.shell.top - slotTop) < 1, `${shape.name}: the paused notice left the strip's slot: ${JSON.stringify({ box: paused.paused.box, shell: paused.shell, slotTop })}`);
      assert(paused.paused.box.left >= 0 && paused.paused.box.right <= shape.width, `${shape.name}: the paused notice leaves the page: ${JSON.stringify(paused.paused.box)}`);
      assert(paused.paused.buttonBox.width >= 44 && paused.paused.buttonBox.height >= 44, `${shape.name}: Resume typing is below a 44 px touch target: ${JSON.stringify(paused.paused.buttonBox)}`);
      assert(paused.paused.messageClipped === false, `${shape.name}: the paused message is clipped`);
      assert(paused.paused.colors.pointerEvents !== "none", `${shape.name}: the paused notice does not take pointer events`);
      caseEvidence.pausedLayout = { box: paused.paused.box, buttonBox: paused.paused.buttonBox, slotTop };
      await shot("paused");

      // While paused nothing leaves the page: keys, a paste, the shared insert
      // path, and the composer — through its hook and its Insert button.
      await focusTerminal();
      await page.keyboard.type("xyz");
      await page.keyboard.press("Enter");
      await page.evaluate(() => window.__typing_pause.paste("PASTED-WHILE-PAUSED"));
      const inserted = await page.evaluate(() => window.__typing_pause.insertText("INSERTED-WHILE-PAUSED"));
      assert(inserted.result === "REFUSED_NO_CONTROL", `${shape.name}: insert while paused returned ${inserted.result}`);
      const injected = await page.evaluate(() => window.__typing_pause.injectComposer("COMPOSED-WHILE-PAUSED"));
      assert(injected.result === "REFUSED_NO_CONTROL", `${shape.name}: composer hook while paused returned ${injected.result}`);
      await press(page.locator(".persea-unified-composer-toggle"));
      await page.locator(".attachment-page__composer-textarea").fill("DRAFT-WHILE-PAUSED");
      const composerPaused = await until(`${shape.name}: composer refuses Insert while paused`, async () => {
        const value = await state();
        return value.composer.sendDisabled === true && value.composer.status.includes(PAUSED_REASON) ? value : undefined;
      });
      caseEvidence.composerWhilePaused = composerPaused.composer;
      await press(page.locator(".attachment-page__composer-send"), { force: true });
      const afterAttempts = await state();
      assert(JSON.stringify(afterAttempts.inputs) === '["a","w"]', `${shape.name}: input left the page while paused: ${JSON.stringify(afterAttempts.inputs)}`);
      expectPaused(afterAttempts, "after typing, paste and Insert while paused");

      // Refusals for keys already in flight (the second key) land while paused
      // and change nothing.
      const again = await page.evaluate(() => window.__typing_pause.refuse("input_paused"));
      expectPaused(again, "after an in-flight refusal");
      assert(!again.strip.shown, `${shape.name}: an in-flight input_paused showed the passing strip`);

      // Another code still passes: it shows below the paused notice and leaves
      // by itself, and the paused notice outlasts it.
      const failedFit = await page.evaluate(() => window.__typing_pause.refuse("resize_failed"));
      assert(failedFit.strip.text === "Fit didn't apply (resize_failed)", `${shape.name}: resize_failed while paused: ${JSON.stringify(failedFit.strip)}`);
      caseEvidence.stacked = (await expectStacked("resize_failed while paused")).strip.box;
      await shot("paused-with-passing-notice");
      await until(`${shape.name}: the passing notice left by itself`, async () => !(await state()).strip.shown, REFUSAL_NOTICE_MS + 3_000);
      const outlasted = await state();
      assert(Date.now() - pausedAt > REFUSAL_NOTICE_MS, `${shape.name}: checked the pause before ${REFUSAL_NOTICE_MS} ms had passed`);
      expectPaused(outlasted, `${REFUSAL_NOTICE_MS} ms later`);
      await focusTerminal();
      await page.keyboard.type("q");
      assert(JSON.stringify(await inputs()) === '["a","w"]', `${shape.name}: a keystroke left the page after the passing notice expired`);

      // Only the operator's own press resumes typing.
      await page.evaluate(() => document.querySelector("#typing-host .persea-unified-paused__resume").click());
      expectPaused(await state(), "after a synthetic click");
      // Playwright's WebKit turns a touch tap into a mouse-identified click,
      // which a generation-fenced button rightly refuses (the native click
      // activation gate records the same limit), so WebKit presses it with
      // the mouse; Chromium taps it.
      const resume = page.getByRole("button", { name: "Resume typing", exact: true });
      await (shape.touch && ENGINE === "webkit" ? resume.click() : press(resume));
      const resumed = await until(`${shape.name}: Resume typing cleared the pause`, async () => {
        const value = await state();
        return !value.paused.shown && value.paused.hidden === true ? value : undefined;
      });
      assert(resumed.paused.message === "", `${shape.name}: the paused message stayed after Resume typing`);
      assert(resumed.activeIsTerminal, `${shape.name}: Resume typing did not return focus to the terminal: ${resumed.activeClass}`);
      const ready = await page.evaluate(() => window.__typing_pause.composerAvailability());
      assert(ready.canInject === true, `${shape.name}: the composer still refuses after Resume typing: ${JSON.stringify(ready)}`);
      await page.keyboard.type("b");
      assert(JSON.stringify(await inputs()) === '["a","w","b"]', `${shape.name}: Resume typing did not restore sending, or replayed input: ${JSON.stringify(await inputs())}`);
      const composerResumed = await until(`${shape.name}: composer Insert available after Resume typing`, async () => {
        const value = await state();
        return value.composer.sendDisabled === false && !value.composer.status.includes(PAUSED_REASON) ? value : undefined;
      });
      caseEvidence.composerAfterResume = composerResumed.composer;
      await press(page.locator(".attachment-page__composer-send"));
      await until(`${shape.name}: composer Insert sent after Resume typing`, async () => (await inputs()).at(-1) === "DRAFT-WHILE-PAUSED");
      await page.evaluate(() => window.__typing_pause.paste("PASTED-AFTER-RESUME"));
      assert((await inputs()).at(-1) === "PASTED-AFTER-RESUME", `${shape.name}: a paste after Resume typing was not sent: ${JSON.stringify(await inputs())}`);
      // Exactly what was typed outside the pause, in order: nothing typed while
      // paused was queued or replayed.
      const sentAfterResume = await inputs();
      const expected = ["a", "w", "b", "DRAFT-WHILE-PAUSED", "PASTED-AFTER-RESUME"];
      assert(JSON.stringify(sentAfterResume) === JSON.stringify(expected), `${shape.name}: input typed while paused was replayed: ${JSON.stringify(sentAfterResume)}`);

      // Another refusal pauses again.
      const repaused = await page.evaluate(() => window.__typing_pause.refuse("input_paused"));
      expectPaused(repaused, "after a second pause");
      await focusTerminal();
      await page.keyboard.type("c");
      assert(JSON.stringify(await inputs()) === JSON.stringify(sentAfterResume), `${shape.name}: a keystroke left the page after the second pause`);

      if (!shape.touch) {
        // A reconnect of the same session keeps the pause. Its notice stands
        // aside while the reconnect strip is up and returns with the new
        // attachment, and typing on that attachment is still dropped.
        const reconnecting = await page.evaluate(() => window.__typing_pause.closeTransport("transport_error"));
        assert(reconnecting.connection !== "" && !reconnecting.paused.shown && reconnecting.paused.hidden === false,
          `${shape.name}: the paused notice did not stand aside for the reconnect strip: ${JSON.stringify({ connection: reconnecting.connection, paused: reconnecting.paused })}`);
        const readmitted = await page.evaluate(() => window.__typing_pause.readmit());
        assert(readmitted.connection === "", `${shape.name}: reconnect strip left after readmission`);
        expectPaused(readmitted, "after a reconnect of the same session");
        await focusTerminal();
        await page.keyboard.type("d");
        assert(JSON.stringify(await inputs()) === JSON.stringify(sentAfterResume), `${shape.name}: a reconnect cleared the pause`);
        await page.getByRole("button", { name: "Resume typing", exact: true }).click();
        await until(`${shape.name}: resumed after the reconnect`, async () => !(await state()).paused.shown);
        await page.keyboard.type("e");
        assert((await inputs()).at(-1) === "e", `${shape.name}: typing did not resume after the reconnect`);

        // Other codes keep the passing strip and never pause typing.
        const refused = await page.evaluate(() => window.__typing_pause.refuse("input_refused"));
        assert(refused.strip.shown && refused.strip.text === "Input was refused — try again (input_refused)" && !refused.paused.shown,
          `${shape.name}: input_refused did not stay a passing notice: ${JSON.stringify({ strip: refused.strip, paused: refused.paused })}`);
        await until(`${shape.name}: input_refused left by itself`, async () => !(await state()).strip.shown, REFUSAL_NOTICE_MS + 3_000);
        await focusTerminal();
        await page.keyboard.type("f");
        assert((await inputs()).at(-1) === "f", `${shape.name}: input_refused paused typing`);

        // A session switch clears the pause: it reported typing lost in the
        // previous session.
        expectPaused(await page.evaluate(() => window.__typing_pause.refuse("input_paused")), "before a session switch");
        const switched = await page.evaluate(() => window.__typing_pause.switchSession("pause-two"));
        assert(!switched.between.paused.shown && switched.between.paused.hidden === true && switched.between.paused.message === "", `${shape.name}: a session switch kept the pause`);
        assert(!switched.after.paused.shown, `${shape.name}: the switched session admitted paused`);
        await focusTerminal();
        await page.keyboard.type("g");
        assert((await inputs()).at(-1) === "g", `${shape.name}: typing did not work on the switched session`);

        // Light and dark themes: the notice is a fixed pair, readable on both.
        caseEvidence.themes = {};
        for (const theme of ["default", "rose-pine-dawn"]) {
          await page.evaluate((id) => window.__typing_pause.setTheme(id), theme);
          await focusTerminal();
          await page.keyboard.type("t");
          const themed = await page.evaluate(() => window.__typing_pause.refuse("input_paused"));
          expectPaused(themed, `on ${theme}`);
          const colors = themed.paused.colors;
          const measured = {
            text: contrast(colors.color, colors.background),
            button: contrast(colors.buttonColor, colors.buttonBackground),
            buttonEdge: contrast(colors.buttonBorder, colors.background),
          };
          assert(measured.text >= 4.5 && measured.button >= 4.5 && measured.buttonEdge >= 3, `${shape.name}: ${theme}: paused notice contrast ${JSON.stringify({ measured, colors })}`);
          caseEvidence.themes[theme] = { colors, measured };
          await shot(`paused-${theme}`);
          await page.getByRole("button", { name: "Resume typing", exact: true }).click();
          await until(`${shape.name}: resumed on ${theme}`, async () => !(await state()).paused.shown);
        }

        // Closing the page clears the pause.
        await focusTerminal();
        await page.keyboard.type("h");
        expectPaused(await page.evaluate(() => window.__typing_pause.refuse("input_paused")), "before closing");
        const closed = await page.evaluate(() => window.__typing_pause.destroy());
        assert(closed.paused.hidden === true && !closed.paused.shown, `${shape.name}: closing the page kept the paused notice`);
      }
      assert(pageErrors.length === 0 && consoleErrors.length === 0, `${shape.name}: browser errors: ${JSON.stringify({ pageErrors, consoleErrors })}`);
      caseEvidence.inputs = sentAfterResume;
      evidence.shapes.push(caseEvidence);
      await context.close();
    }
    evidence.inputResults = await inputResults(browser, origin);
    fs.writeFileSync(path.join(EVIDENCE, "typing-pause.json"), JSON.stringify(evidence, null, 2));
    console.log(`typing pause ${ENGINE}: PASS (${evidence.shapes.length} layouts; keys, paste, insert and composer dropped while paused; notice outlasts ${REFUSAL_NOTICE_MS} ms; Resume, re-pause, reconnect, switch, close, themes; input results: ${evidence.inputResults.join(", ")})`);
  } finally {
    await browser?.close();
    await new Promise((resolve) => server.close(resolve));
  }
}

// Input results (ui/src/input_results.ts), on one desktop page: what the
// page and its composer do with each result, and with input that has none
// when its socket ends.
const UNCERTAIN = "The connection dropped before the terminal confirmed your last input — check that it arrived";
async function inputResults(browser, origin) {
  const checked = [];
  const progress = (step) => { if (process.env.PERSEA_INPUT_RESULTS_TRACE) console.error(`input results: ${step}`); };
  const context = await browser.newContext({ viewport: { width: 1100, height: 760 } });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(String(error)));
  page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
  await page.goto(origin, { waitUntil: "load" });
  await page.waitForFunction(() => document.body?.dataset.typingPauseReady === "true");
  const call = (name, ...args) => page.evaluate(([method, values]) => window.__typing_pause[method](...values), [name, args]);
  const state = () => call("snapshot");
  const focusTerminal = () => page.locator("#typing-host .xterm-helper-textarea").focus();
  const composer = page.locator(".attachment-page__composer-textarea");
  const insert = () => page.locator(".attachment-page__composer-send").click();
  const status = async () => (await state()).composer.status;
  // After an Insert the composer shows its receipt; the operator reopens it
  // with the composer toggle to write the next draft.
  const draft = async (text) => {
    const toggle = page.locator(".persea-unified-composer-toggle");
    if (!(await composer.isVisible())) await toggle.click();
    if (!(await composer.isVisible())) await toggle.click();
    await composer.fill(text);
  };
  await call("reset", "results-one");
  await call("admit");

  // Keys with results: nothing to say when the socket then ends.
  await focusTerminal();
  await page.keyboard.type("k1");
  await call("answer");
  let value = await call("closeTransport", "transport_error");
  assert(!value.strip.shown, `answered keys drew a notice at close: ${JSON.stringify(value.strip)}`);
  checked.push("answered keys"); progress(checked.at(-1));

  // A key without a result when its socket ends may not have arrived: one
  // notice. Closing again says nothing more, and a late result for that
  // socket is ignored, not a protocol fault.
  await call("readmit");
  await focusTerminal();
  await page.keyboard.type("u");
  value = await call("closeTransport", "transport_error");
  assert(value.strip.shown && value.strip.text === UNCERTAIN, `unanswered key at close: ${JSON.stringify(value.strip)}`);
  await call("closeTransport", "transport_error");
  value = await call("rawResult", 1);
  assert(value.finalized.length === 0, `a late result after close was judged: ${JSON.stringify(value.finalized)}`);
  checked.push("unanswered key at close"); progress(checked.at(-1));

  // Results arrive in order and never pass what was sent.
  for (const [label, through] of [["ahead of what was sent", 9], ["not advancing", 1]]) {
    await call("readmit");
    await focusTerminal();
    await page.keyboard.type("x");
    if (through === 1) await call("answer");
    value = await call("rawResult", through);
    assert(JSON.stringify(value.finalized.slice(-1)) === '["MALFORMED_FRAME"]', `result ${label}: ${JSON.stringify(value.finalized)}`);
  }
  checked.push("result order"); progress(checked.at(-1));

  // Composer: Inserting until the result, then Inserted when the terminal
  // took it all.
  await call("readmit");
  await draft("HELLO");
  await insert();
  await until("Insert pending", async () => (await status()) === "Inserting 5 ch\u2026");
  await call("answer");
  await until("Insert confirmed", async () => (await status()) === "Inserted 5 ch");
  checked.push("Insert written"); progress(checked.at(-1));

  // While an Insert waits, a second Insert and Restore are held back.
  await draft("FIRST");
  await insert();
  await until("Insert pending", async () => (await status()).startsWith("Inserting"));
  await draft("SECOND");
  const sentWhileWaiting = (await state()).inputs.length;
  await composer.press("Control+Enter");
  value = await state();
  assert(value.composer.sendDisabled === true && value.composer.status.includes("Waiting for the last Insert to reach the terminal")
    && value.inputs.length === sentWhileWaiting && value.composer.draft === "SECOND",
    `second Insert while the first waits: ${JSON.stringify({ ...value.composer, sent: value.inputs.length - sentWhileWaiting })}`);
  await draft("");
  value = await state();
  assert(!value.composer.restoreShown, `Restore offered the waiting Insert: ${JSON.stringify(value.composer)}`);
  await call("answer");
  await draft("SECOND");
  assert((await state()).composer.sendDisabled === false, "Insert stayed held after the result");
  await draft("");
  checked.push("one Insert at a time"); progress(checked.at(-1));

  // Clear is held too: the waiting Insert may need the restore slot, and a
  // late refusal must not cost the newer draft.
  await draft("FIRST-INSERT");
  await insert();
  await until("Insert pending", async () => (await status()).startsWith("Inserting"));
  await draft("SECOND-DRAFT");
  assert(await page.locator(".attachment-page__composer-clear").isDisabled(), "Clear was offered while an Insert waits");
  value = await call("refuse", "input_refused");
  assert(value.composer.draft === "SECOND-DRAFT" && value.composer.status.includes("Restore brings the text back"),
    `newer draft at a late refusal: ${JSON.stringify(value.composer)}`);
  await draft("");
  await page.locator(".attachment-page__composer-restore").click();
  assert((await state()).composer.draft === "FIRST-INSERT", "Restore lost the refused Insert");
  await draft("");
  checked.push("Clear held"); progress(checked.at(-1));

  // A held Ctrl+Enter on the reopened empty composer changes nothing: the
  // waiting Insert is still confirmed, or still comes back when refused.
  for (const [code, check] of [["", (c) => c.status === "Inserted 9 ch"], ["input_refused", (c) => c.draft === "UNTOUCHED" && c.status.includes("Your text is back")]]) {
    await draft("UNTOUCHED");
    await insert();
    await until("Insert pending", async () => (await status()).startsWith("Inserting"));
    await draft("");
    await composer.press("Control+Enter");
    value = code === "" ? (await call("answer"), await state()) : await call("refuse", code);
    assert(check(value.composer), `held Ctrl+Enter then ${code || "written"}: ${JSON.stringify(value.composer)}`);
    await draft("");
  }
  checked.push("held Ctrl+Enter"); progress(checked.at(-1));

  // Nothing of it reached the terminal: the text is back in the empty draft.
  await draft("AGAIN");
  await insert();
  await until("Insert pending", async () => (await status()).startsWith("Inserting"));
  value = await call("refuse", "input_refused");
  assert(value.composer.draft === "AGAIN" && value.composer.status.includes("Not inserted — nothing reached the terminal. Your text is back."),
    `refused Insert: ${JSON.stringify(value.composer)}`);
  checked.push("Insert refused"); progress(checked.at(-1));

  // Unknown: the socket ended first. The text stays out of the draft, in
  // Restore, and the operator is told to check.
  await insert();
  await until("Insert pending", async () => (await status()).startsWith("Inserting"));
  value = await call("closeTransport", "transport_error");
  assert(value.composer.draft === "" && value.composer.restoreShown && value.composer.status.includes("The connection dropped before the terminal confirmed this Insert"),
    `uncertain Insert: ${JSON.stringify(value.composer)}`);
  assert(value.strip.text !== UNCERTAIN, `an uncertain Insert also drew the keystroke notice: ${JSON.stringify(value.strip)}`);
  await page.locator(".attachment-page__composer-restore").click();
  assert((await state()).composer.draft === "AGAIN", "Restore did not bring the uncertain Insert back");
  checked.push("Insert uncertain"); progress(checked.at(-1));

  // Part of it reached the terminal.
  await call("readmit");
  await insert();
  await until("Insert pending", async () => (await status()).startsWith("Inserting"));
  value = await call("refuse", "input_partial");
  assert(value.composer.draft === "" && value.composer.status.includes("Only part of this Insert reached the terminal"), `partial Insert: ${JSON.stringify(value.composer)}`);
  checked.push("Insert partial"); progress(checked.at(-1));

  // The focus report the Insert's own focus sends is not the Insert: only the
  // paste's result decides.
  await call("live", "\x1b[?1004h");
  await draft("FOCUSED");
  await composer.focus();
  const before = (await state()).inputs.length;
  await insert();
  await until("Insert pending", async () => (await status()).startsWith("Inserting"));
  const sent = (await state()).inputs.slice(before);
  assert(sent.length === 2 && sent[0] === "\x1b[I" && sent[1].includes("FOCUSED"), `focus report and paste: ${JSON.stringify(sent)}`);
  // Everything before the paste (the reports reopening the composer and
  // the Insert's focus sent) was written; the paste was refused.
  await call("answer", "", 1);
  value = await call("refuse", "input_refused");
  assert(value.composer.draft === "FOCUSED" && value.composer.status.includes("Not inserted"), `Insert after a focus report: ${JSON.stringify(value.composer)}`);
  await call("live", "\x1b[?1004l");
  checked.push("focus report"); progress(checked.at(-1));

  // Not sent at all, or too large for one frame: the draft stays.
  await call("setPortResult", "SATURATED");
  await insert();
  value = await state();
  assert(value.composer.draft === "FOCUSED" && value.composer.status.includes("Not sent — the connection is not ready. Your text is kept."),
    `saturated Insert: ${JSON.stringify(value.composer)}`);
  await call("setPortResult", "ACCEPTED");
  const large = "x".repeat(262_145);
  await draft(large);
  const sentBefore = (await state()).inputs.length;
  await insert();
  value = await state();
  assert(value.composer.draft.length === large.length && value.composer.status.includes("Too long to insert at once") && value.inputs.length === sentBefore,
    `oversized Insert: ${JSON.stringify({ ...value.composer, draft: value.composer.draft.length })}`);
  await draft("");
  checked.push("not sent", "too large"); progress(checked.at(-1));

  // A session switch settles the old session's input before the composer
  // changes session: the waiting Insert does not reach the new session.
  await draft("SWITCHED-AWAY");
  await insert();
  await until("Insert pending", async () => (await status()).startsWith("Inserting"));
  value = (await call("switchSession", "results-two")).after;
  assert(value.composer.draft === "" && !value.composer.restoreShown && !value.composer.status.includes("connection dropped"),
    `Insert pending across a session switch: ${JSON.stringify(value.composer)}`);
  checked.push("session switch"); progress(checked.at(-1));

  assert(errors.length === 0, `browser errors: ${JSON.stringify(errors)}`);
  await context.close();
  return checked;
}

main().catch((error) => { console.error(error && error.stack || error); process.exitCode = 1; });
