"use strict";

// One focused regression, two real engines: a tap owns its release even when
// the page layout moves the control under a still pointer.
const path = require("path");
const { build } = require("esbuild");
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));

const requestedEngine = process.env.PERSEA_TAP_SHIFT_ENGINE || "all";
const engines = requestedEngine === "all" ? ["chromium", "webkit"] : [requestedEngine];
if (engines.some((engine) => !["chromium", "webkit"].includes(engine))) throw new Error("PERSEA_TAP_SHIFT_ENGINE must be all, chromium, or webkit");

function assert(value, message) {
  if (!value) throw new Error(`TAP_SHIFT_ASSERTION: ${message}`);
}

async function bundle() {
  const result = await build({
    entryPoints: [path.join(__dirname, "tap_layout_shift_browser.test.ts")],
    bundle: true,
    format: "iife",
    globalName: "TapShiftRegression",
    write: false,
  });
  return result.outputFiles[0].text;
}

async function runEngine(engine, source) {
  const browser = await playwright[engine].launch(engine === "chromium" ? {
    executablePath: require("./browser_path.cjs")(),
    headless: true,
    args: ["--no-sandbox"],
  } : { headless: true });
  const diagnostics = [];
  const checks = [];
  const unsupported = engine === "webkit" ? [{
    name: "stationary-touch-survives-layout-shift",
    reason: "Playwright WebKit can only tap; it cannot hold a touch contact while the layout changes.",
  }] : [];
  const page = await browser.newPage({ viewport: { width: 640, height: 480 }, hasTouch: true });
  page.on("console", (message) => diagnostics.push(`console ${message.type()}: ${message.text()}`));
  page.on("pageerror", (error) => diagnostics.push(`pageerror: ${error.message}`));
  const snapshot = () => page.evaluate(() => TapShiftRegression.snapshot());
  const box = () => page.evaluate(() => TapShiftRegression.box());
  const showStatus = () => page.evaluate(() => TapShiftRegression.showStatus());
  async function check(name, run) {
    try { await page.evaluate(() => TapShiftRegression.mount()); await run(); checks.push({ name, pass: true }); }
    catch (error) { checks.push({ name, pass: false, error: error.message, receipt: await snapshot() }); }
  }
  try {
    await page.setContent("<!doctype html><html><head><title>Tap layout shift regression</title></head><body></body></html>");
    await page.addScriptTag({ content: source });
    await check("stationary-press-survives-layout-shift", async () => {
      const before = await box();
      const press = { x: before.x + before.width / 2, y: before.y + 10 };
      await page.mouse.move(press.x, press.y);
      await page.mouse.down();
      await showStatus();
      const after = await box();
      assert(press.y < after.y, `the status line did not move the control out from under the pointer: ${JSON.stringify({ before, after, press })}`);
      await page.mouse.up();
      const result = await snapshot();
      assert(result.actions.length === 1 && result.actions[0].trusted && result.actions[0].pointerType === "mouse",
        "a still press lost its activation when the layout moved the control");
    });
    await check("layout-driven-pointerleave-keeps-the-press", async () => {
      const before = await box();
      await page.mouse.move(before.x + before.width / 2, before.y + 10);
      await page.mouse.down();
      await showStatus();
      await page.evaluate(() => TapShiftRegression.leave());
      await page.mouse.up();
      const result = await snapshot();
      assert(result.actions.length === 1 && result.actions[0].trusted, "a pointerleave caused by the layout cancelled a still press");
    });
    await check("drag-off-still-cancels", async () => {
      const control = await box();
      const center = { x: control.x + control.width / 2, y: control.y + control.height / 2 };
      await page.mouse.move(center.x, center.y);
      await page.mouse.down();
      await page.mouse.move(center.x, control.y + control.height + 30, { steps: 4 });
      await page.mouse.up();
      assert((await snapshot()).actions.length === 0, "dragging off the control still activated it");
      await page.mouse.click(center.x, center.y);
      assert((await snapshot()).actions.length === 1, "a fresh tap after a drag-off did not act exactly once");
    });
    await check("presentation-change-still-drops-the-press", async () => {
      const control = await box();
      await page.mouse.move(control.x + control.width / 2, control.y + control.height / 2);
      await page.mouse.down();
      await page.evaluate(() => TapShiftRegression.advanceGeneration());
      await page.mouse.up();
      assert((await snapshot()).actions.length === 0, "a press survived an interaction generation change");
    });
    await check("hidden-control-release-does-not-act", async () => {
      const control = await box();
      await page.mouse.move(control.x + control.width / 2, control.y + control.height / 2);
      await page.mouse.down();
      await page.evaluate(() => TapShiftRegression.hideList());
      await page.mouse.up();
      assert((await snapshot()).actions.length === 0, "a captured release activated a control hidden during the press");
    });
    await check("invisible-control-release-does-not-act", async () => {
      const control = await box();
      await page.mouse.move(control.x + control.width / 2, control.y + control.height / 2);
      await page.mouse.down();
      await page.evaluate(() => TapShiftRegression.concealList());
      await page.mouse.up();
      assert((await snapshot()).actions.length === 0, "a captured release activated a control made invisible during the press");
    });
    await check("disabled-control-release-does-not-act", async () => {
      const control = await box();
      await page.mouse.move(control.x + control.width / 2, control.y + control.height / 2);
      await page.mouse.down();
      await page.evaluate(() => TapShiftRegression.disable());
      await page.mouse.up();
      assert((await snapshot()).actions.length === 0, "a captured release activated a control disabled during the press");
    });
    await check("failed-capture-retires-the-press", async () => {
      const control = await box();
      await page.evaluate(() => TapShiftRegression.detachOnNextPress());
      await page.mouse.move(control.x + control.width / 2, control.y + control.height / 2);
      await page.mouse.down();
      await page.mouse.up();
      assert((await snapshot()).actions.length === 0, "a press whose control left the page activated it");
    });
    if (engine === "chromium") await check("stationary-touch-survives-layout-shift", async () => {
      const before = await box();
      const touchSession = await page.context().newCDPSession(page);
      try {
        await touchSession.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: before.x + before.width / 2, y: before.y + 10, id: 3 }] });
        await showStatus();
        assert(before.y + 10 < (await box()).y, "the status line did not move the control out from under the contact");
        await touchSession.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
      } finally {
        await touchSession.detach();
      }
      const result = await snapshot();
      assert(result.actions.length === 1 && result.actions[0].trusted && result.actions[0].pointerType === "touch",
        "a still touch lost its activation when the layout moved the control");
    });
    assert(diagnostics.length === 0, `browser diagnostics: ${diagnostics.join("; ")}`);
    return { engine, checks, unsupported, diagnostics, pass: checks.every((check) => check.pass) };
  } finally { await browser.close(); }
}

async function main() {
  const source = await bundle();
  let passed = true;
  for (const engine of engines) {
    const result = await runEngine(engine, source);
    console.log(JSON.stringify(result));
    passed = passed && result.pass;
  }
  if (!passed) process.exitCode = 1;
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
