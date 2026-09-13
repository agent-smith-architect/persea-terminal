"use strict";

// One focused regression, two real engines. --helper-source bundles an isolated
// source variant for RED proof; it never replaces the checked-out product file.
const fs = require("fs");
const path = require("path");
const { build } = require("esbuild");
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));

const args = process.argv.slice(2);
let helperSource;
if (args.length) {
  if (args.length !== 2 || args[0] !== "--helper-source") throw new Error("Usage: node run_native_click_activation_browser.cjs [--helper-source FILE]");
  helperSource = path.resolve(args[1]);
}
const requestedEngine = process.env.PERSEA_NATIVE_CLICK_ENGINE || "all";
const engines = requestedEngine === "all" ? ["chromium", "webkit"] : [requestedEngine];
if (engines.some((engine) => !["chromium", "webkit"].includes(engine))) throw new Error("PERSEA_NATIVE_CLICK_ENGINE must be all, chromium, or webkit");

function assert(value, message) {
  if (!value) throw new Error(`NATIVE_CLICK_ASSERTION: ${message}`);
}

async function bundle() {
  const result = await build({
    entryPoints: [path.join(__dirname, "native_click_activation_browser.test.ts")],
    bundle: true,
    format: "iife",
    globalName: "NativeClickRegression",
    write: false,
    plugins: helperSource ? [{
      name: "isolated-helper-source",
      setup(plugin) {
        plugin.onLoad({ filter: /[/\\]src[/\\]tap_activation\.ts$/ }, () => ({
          contents: fs.readFileSync(helperSource, "utf8"),
          loader: "ts",
          resolveDir: path.dirname(helperSource),
        }));
      },
    }] : [],
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
    name: "completed-native-touch-keeps-its-click",
    reason: "Playwright 1.62.1 WebKit FakeTouchTap synthesizes a mouse-identified click after touch release; raw touchStart/touchEnd emits no compatibility click. Native touch-click acceptance is not proven by this lane.",
    source: "https://github.com/microsoft/playwright/blob/v1.62.1/browser_patches/webkit/patches/bootstrap.diff#L18392",
  }, {
    name: "explicit-pointer-id-cannot-borrow-another-contact",
    reason: "WebKit inspector suppresses the mouse release when touch is held; the genuine hybrid branch is tested in Chromium only.",
  }] : [];
  const page = await browser.newPage({ viewport: { width: 640, height: 480 }, hasTouch: true });
  page.on("console", (message) => diagnostics.push(`console ${message.type()}: ${message.text()}`));
  page.on("pageerror", (error) => diagnostics.push(`pageerror: ${error.message}`));
  const snapshot = () => page.evaluate(() => NativeClickRegression.snapshot());
  const mount = () => page.evaluate(() => NativeClickRegression.mount());
  async function check(name, run) {
    try { await mount(); await run(); checks.push({ name, pass: true }); }
    catch (error) { checks.push({ name, pass: false, error: error.message, receipt: await snapshot() }); }
  }
  try {
    await page.setContent("<!doctype html><html><head><title>Native activation regression</title></head><body></body></html>");
    await page.addScriptTag({ content: source });
    await check("canceled-pointer-cannot-regain-authority", async () => {
      await page.mouse.move(70, 40);
      await page.mouse.down();
      await page.mouse.move(250, 150);
      await page.evaluate(() => NativeClickRegression.advanceGeneration());
      await page.mouse.move(70, 40);
      await page.mouse.up();
      const stale = await snapshot();
      const down = stale.events.find((event) => event.type === "pointerdown");
      const leave = stale.events.find((event) => event.type === "pointerleave");
      const click = stale.events.find((event) => event.type === "click");
      assert(down?.trusted && leave?.trusted && click?.trusted && click.detail > 0,
        "driver did not deliver the genuine canceled-pointer click sequence");
      assert(down.generation === 0 && click.generation === 1, "generation cut did not fall inside the gesture");
      assert(stale.actions.length === 0, "canceled native pointer click regained authority after generation cut");
      await page.mouse.click(70, 40);
      const fresh = await snapshot();
      assert(fresh.actions.length === 1 && fresh.actions[0].trusted, "fresh pointer click after cancellation did not act exactly once");
    });
    await check("keyboard-captureless-and-programmatic-controls", async () => {
      await page.evaluate(() => NativeClickRegression.focus());
      await page.keyboard.press("Enter");
      await page.keyboard.press("Space");
      assert((await snapshot()).actions.length === 2, "native Enter and Space must each act once");
      await page.keyboard.down("Space");
      await page.evaluate(() => NativeClickRegression.advanceGeneration());
      await page.keyboard.up("Space");
      assert((await snapshot()).actions.length === 2, "stale Space release invoked an action");
      await page.evaluate(() => NativeClickRegression.withholdNextKeydown());
      await page.keyboard.press("Enter");
      const captureless = await snapshot();
      assert(captureless.actions.length === 3 && captureless.actions[2].trusted && captureless.actions[2].detail === 0,
        "next captureless trusted activation was lost or duplicated");
      await page.evaluate(() => NativeClickRegression.programmaticClick());
      const programmed = await snapshot();
      assert(programmed.actions.length === 4 && !programmed.actions[3].trusted,
        "intentional HTMLElement.click() contract changed");
    });
    if (engine === "chromium") await check("completed-native-touch-keeps-its-click", async () => {
      await page.touchscreen.tap(70, 40);
      const touch = await snapshot();
      assert(touch.events.some((event) => event.type === "pointerdown" && event.pointerType === "touch" && event.trusted),
        "driver did not deliver a genuine touch contact");
      assert(touch.actions.length === 1 && touch.actions[0].trusted, "completed native touch did not act exactly once");
    });
    // Chromium can interleave native mouse and touch protocol events. WebKit's
    // inspector suppresses this mouse release; do not count that as a product
    // success or forge trusted DOM events to manufacture the missing branch.
    if (engine === "chromium") await check("explicit-pointer-id-cannot-borrow-another-contact", async () => {
      const touchSession = await page.context().newCDPSession(page);
      const contact = { x: 70, y: 40, id: 47 };
      try {
        await page.mouse.move(70, 40);
        await page.mouse.down();
        await page.mouse.move(250, 150);
        await page.evaluate(() => NativeClickRegression.advanceGeneration());
        await touchSession.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [contact] });
        await page.mouse.move(70, 40);
        await page.mouse.up();
        const mixed = await snapshot();
        const mouseDown = mixed.events.find((event) => event.type === "pointerdown" && event.pointerType === "mouse");
        const touchDown = mixed.events.find((event) => event.type === "pointerdown" && event.pointerType === "touch");
        const mouseClick = mixed.events.find((event) => event.type === "click" && event.pointerType === "mouse");
        assert(mouseDown?.trusted && touchDown?.trusted && mouseClick?.trusted
          && mouseDown.pointerId !== touchDown.pointerId && mouseClick.pointerId === mouseDown.pointerId,
        "driver did not produce distinct genuine mouse/touch contacts and an explicit old-pointer click");
        assert(mixed.actions.length === 0, "old pointer click borrowed a different contact's acquisition");
        await touchSession.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
        const finished = await snapshot();
        assert(finished.actions.length === 1 && finished.actions[0].pointerId === touchDown.pointerId,
          "rejecting the old pointer destroyed the still-held contact's valid acquisition");
      } finally {
        await touchSession.detach();
      }
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
