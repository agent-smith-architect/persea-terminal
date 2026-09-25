"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");

// Use the real Composer with a visible native scrollbar and hold only frames
// queued by the publication. Native scrolling remains free to run between them.
module.exports = async function composerScrollGestures(browser, engine) {
  const bundle = await require("esbuild").build({
    stdin: {
      contents: `import { Composer } from "./src/composer";
        import { DEFAULT_PREFERENCES } from "./src/preferences";
        window.composer = new Composer({
          page: document.body, dock: document.querySelector("#dock"),
          interactionGeneration: () => 0, availability: () => ({canInject: false}),
          inject: () => "REFUSED_NO_CONTROL", refocusTerminal() {}, insetBudget: () => 400,
          commitOverlayInset: async () => true, revealLiveEdgeForOverlay() {},
          cancelPendingOverlayReveal() {}, preferences: () => DEFAULT_PREFERENCES,
          updatePreferences() {}, initialComposerFontSize: 11,
        });
        composer.showWithoutFocus();`,
      resolveDir: path.resolve(__dirname, ".."),
    }, bundle: true, write: false, format: "iife", platform: "browser",
  });
  const context = await browser.newContext({ viewport: { width: 800, height: 600 } });
  const page = await context.newPage();
  const messages = [];
  page.on("console", (message) => messages.push(message.text()));
  page.on("pageerror", (error) => messages.push(String(error)));
  const evidence = [];
  try {
    for (const kind of ["native-thumb", "pointerup", "pointercancel", "lostpointercapture", "touch-held", "touchend", "touchcancel", "programmatic"]) {
      if (kind === "native-thumb" && engine !== "chromium") continue;
      await page.setContent(`<style>
        textarea { width: 400px; height: 200px !important; overflow: scroll; scrollbar-gutter: stable;
          font: var(--persea-composer-font-size, 11px) monospace; }
        textarea::-webkit-scrollbar { width: 18px; height: 18px; }
        textarea::-webkit-scrollbar-thumb { background: #888; min-height: 20px; }
        textarea::-webkit-scrollbar-track { background: #eee; }
      </style><div id="dock"></div>`);
      await page.addScriptTag({ content: bundle.outputFiles[0].text });
      await page.evaluate(async () => {
        const t = composer.textarea;
        t.value = Array.from({ length: 300 }, (_, i) => `line ${i}`).join("\n");
        t.dispatchEvent(new Event("input"));
        t.focus();
        t.setSelectionRange(0, 0);
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        t.scrollTop = 100;
        // WebKit has no constructible Touch. Exercise DOM listener ordering
        // here; platform touch scrolling and momentum require device coverage.
        window.dispatchComposerTouch = (type) => {
          const event = new Event(type, { bubbles: true });
          Object.defineProperty(event, "changedTouches", { value: [{ identifier: 7 }] });
          t.dispatchEvent(event);
        };
      });
      await page.waitForFunction(() => composer.textarea.scrollTop === 100);
      const thumb = await page.evaluate(() => {
        const t = composer.textarea;
        const r = t.getBoundingClientRect();
        return { x: r.right - 9, y: r.top + 12 + t.scrollTop / (t.scrollHeight - t.clientHeight) * (t.clientHeight - 20),
          gutter: t.offsetWidth - t.clientWidth };
      });
      if (kind === "native-thumb") {
        assert(thumb.gutter >= 18, `native scrollbar must be visible: ${JSON.stringify(thumb)}`);
        await page.mouse.move(thumb.x, thumb.y);
        await page.mouse.down();
      } else if (kind !== "programmatic") {
        await page.evaluate((kind) => {
          const t = composer.textarea;
          if (kind.startsWith("touch")) {
            dispatchComposerTouch("touchstart");
          } else t.dispatchEvent(new PointerEvent("pointerdown", { pointerId: 7, bubbles: true }));
        }, kind);
      }
      await page.evaluate(() => {
        window.savedRAF = window.requestAnimationFrame;
        window.savedCancelRAF = window.cancelAnimationFrame;
        window.heldFrames = new Map();
        let serial = 100000;
        window.requestAnimationFrame = (callback) => { heldFrames.set(++serial, callback); return serial; };
        window.cancelAnimationFrame = (id) => { heldFrames.delete(id); savedCancelRAF(id); };
        window.beforePublication = composer.textarea.scrollTop;
        composer.applyTypographyState({ size: 12, status: "ready", message: "", enabled: true });
        // Let measurement finish before moving the thumb. Only the final
        // typography repair remains held, so geometry cannot move the thumb.
        const pending = [...heldFrames.entries()];
        for (const [id, callback] of pending.slice(0, -1)) {
          heldFrames.delete(id);
          callback(performance.now());
        }
      });
      if (kind === "native-thumb") {
        await page.mouse.move(thumb.x, thumb.y + 90, { steps: 6 });
        await page.waitForFunction(() => composer.textarea.scrollTop > beforePublication + 100, null, { polling: 10 });
      } else {
        await page.evaluate((kind) => {
          if (kind === "programmatic") composer.scrollDraftTo(250, 0);
          else {
            composer.textarea.scrollTop = 250;
            composer.textarea.dispatchEvent(new Event("scroll"));
          }
        }, kind);
      }
      const proof = await page.evaluate(async (kind) => {
        const t = composer.textarea;
        // Native thumb movement reaches the main thread asynchronously. Let
        // browser frames deliver it while the product repair remains held.
        await new Promise((resolve) => savedRAF(() => savedRAF(resolve)));
        const expected = { top: t.scrollTop, start: t.selectionStart, end: t.selectionEnd };
        // End some gestures before draining; others remain active until after
        // repair. Both orderings must invalidate the captured posture.
        if (kind !== "native-thumb" && kind !== "programmatic" && kind !== "touch-held") {
          if (kind.startsWith("touch")) dispatchComposerTouch(kind);
          else t.dispatchEvent(new PointerEvent(kind, { pointerId: 7, bubbles: true }));
        }
        window.requestAnimationFrame = savedRAF;
        window.cancelAnimationFrame = savedCancelRAF;
        const frames = [...heldFrames.values()];
        heldFrames.clear();
        for (const callback of frames) callback(performance.now());
        const repairedTop = t.scrollTop;
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        return { kind, before: beforePublication, expected, repairedTop,
          actual: { top: t.scrollTop, start: t.selectionStart, end: t.selectionEnd } };
      }, kind);
      if (kind === "native-thumb") await page.mouse.up();
      evidence.push(proof);
      assert(Math.abs(proof.repairedTop - proof.expected.top) <= 1, `repair overwrote ongoing gesture: ${JSON.stringify(proof)}`);
      assert(Math.abs(proof.actual.top - proof.expected.top) <= 1, `ongoing gesture overwritten: ${JSON.stringify(proof)}`);
      assert.equal(proof.actual.start, proof.expected.start);
      assert.equal(proof.actual.end, proof.expected.end);
      const releasedTop = await page.evaluate(async (kind) => {
        const t = composer.textarea;
        if (kind === "touch-held") dispatchComposerTouch("touchend");
        t.scrollTop = 100;
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        const callbacks = [];
        window.requestAnimationFrame = (callback) => { callbacks.push(callback); return 200000 + callbacks.length; };
        composer.applyTypographyState({ size: 13, status: "ready", message: "", enabled: true });
        // Same layout-notification ordering as the main Controls regression.
        // After each completion path, a new repair must be allowed again.
        t.scrollTop = 200;
        t.dispatchEvent(new Event("scroll"));
        window.requestAnimationFrame = savedRAF;
        for (const callback of callbacks) callback(performance.now());
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        return t.scrollTop;
      }, kind);
      assert(Math.abs(releasedTop - 100) <= 1, `${kind}: completed gesture still blocked layout repair: ${releasedTop}`);
      await page.evaluate(() => composer.destroy());
    }
    const completionEvidence = [];
    for (const signal of ["pointercancel", "lostpointercapture", "touchcancel", "blur", "hidden",
      "unrelated-pointer", "remaining-pointer", "unrelated-touch", "remaining-touch", "visible", "textarea-blur"]) {
      await page.setContent(`<style>
        textarea { width: 400px; height: 200px !important; overflow: scroll;
          font: var(--persea-composer-font-size, 11px) monospace; }
      </style><div id="dock"></div>`);
      await page.addScriptTag({ content: bundle.outputFiles[0].text });
      completionEvidence.push(await page.evaluate(async (signal) => {
        const t = composer.textarea;
        t.value = Array.from({ length: 300 }, (_, i) => `line ${i}`).join("\n");
        t.dispatchEvent(new Event("input"));
        t.focus();
        t.setSelectionRange(0, 0);
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        const pointer = (type, id) => t.dispatchEvent(new PointerEvent(type, { pointerId: id, bubbles: true }));
        const touch = (type, id) => {
          const event = new Event(type, { bubbles: true });
          Object.defineProperty(event, "changedTouches", { value: [{ identifier: id }] });
          t.dispatchEvent(event);
        };
        const usesTouch = signal.includes("touch");
        if (usesTouch) touch("touchstart", 7);
        else pointer("pointerdown", 7);
        // Lifecycle interruptions must clear both representations of a touch
        // gesture, even when neither release event can reach the document.
        if (signal === "blur" || signal === "hidden") touch("touchstart", 7);
        if (signal === "pointercancel" || signal === "lostpointercapture") pointer(signal, 7);
        else if (signal === "touchcancel") touch(signal, 7);
        else if (signal === "blur") window.dispatchEvent(new Event("blur"));
        else if (signal === "hidden" || signal === "visible") {
          Object.defineProperty(document, "visibilityState", { configurable: true, value: signal });
          document.dispatchEvent(new Event("visibilitychange"));
          delete document.visibilityState;
        } else if (signal === "textarea-blur") t.blur();
        else if (signal === "unrelated-pointer") pointer("pointercancel", 8);
        else if (signal === "remaining-pointer") {
          pointer("pointerdown", 8);
          pointer("lostpointercapture", 7);
        } else if (signal === "unrelated-touch") touch("touchcancel", 8);
        else if (signal === "remaining-touch") {
          touch("touchstart", 8);
          touch("touchcancel", 7);
        }
        t.scrollTop = 100;
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        const saved = window.requestAnimationFrame;
        const callbacks = [];
        window.requestAnimationFrame = (callback) => { callbacks.push(callback); return 300000 + callbacks.length; };
        composer.applyTypographyState({ size: 13, status: "ready", message: "", enabled: true });
        // Force a layout scroll between publication and repair so a skipped
        // repair cannot pass simply because the browser kept the old offset.
        t.scrollTop = 200;
        t.dispatchEvent(new Event("scroll"));
        window.requestAnimationFrame = saved;
        for (const callback of callbacks) callback(performance.now());
        const repairedTop = t.scrollTop;
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        const actualTop = t.scrollTop;
        composer.destroy();
        return { signal, repairedTop, actualTop };
      }, signal));
    }
    const completed = new Set(["pointercancel", "lostpointercapture", "touchcancel", "blur", "hidden"]);
    assert.deepEqual(completionEvidence.filter((proof) => {
      const expected = completed.has(proof.signal) ? 100 : 200;
      return Math.abs(proof.repairedTop - expected) > 1 || Math.abs(proof.actualTop - expected) > 1;
    }), [], "completed gestures must allow repair; remaining gestures must keep their scroll");
    evidence.push(...completionEvidence);
    assert.deepEqual(messages, [], "composer gesture console must stay clean");
    return evidence;
  } finally {
    await context.close();
  }
};

if (require.main === module) {
  const engine = process.env.PERSEA_TERMINAL_CONTROLS_ENGINE || "chromium";
  (async () => {
    const browser = await require("playwright")[engine].launch({ headless: true, ignoreDefaultArgs: ["--hide-scrollbars"],
      ...(engine === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}) });
    try { console.log(JSON.stringify(await module.exports(browser, engine))); }
    finally { await browser.close(); }
  })().catch((error) => { console.error(error); process.exitCode = 1; });
}
