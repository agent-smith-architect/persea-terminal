"use strict";
const fs = require("fs"), path = require("path"), http = require("http"), https = require("https");
const UI = process.env.PERSEA_KEYS_UI || path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_KEYS_ENGINE || "chromium";
const OUT = process.env.PERSEA_KEYS_EVIDENCE || require("node:path").join(require("node:os").tmpdir(), "persea-terminal-tests", "run_terminal_keys_picker_browser");
const { startFixture } = require(path.join(UI, "test/unified_reopen_fixture.cjs"));
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
function json(url, body) {
  return new Promise((resolve, reject) => {
    const req = (url.startsWith("https:") ? https : http).request(url, { method: body ? "POST" : "GET", rejectUnauthorized: false, headers: body ? { "Content-Type": "application/json" } : {} }, res => {
      let data = ""; res.on("data", part => { data += part; }); res.on("end", () => { try { resolve(JSON.parse(data)); } catch (error) { reject(error); } });
    });
    req.on("error", reject); if (body) req.write(JSON.stringify(body)); req.end();
  });
}
async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const report = { engine: ENGINE, shapes: [], assertions: [] };
  const check = (value, message) => { if (!value) throw new Error(message); report.assertions.push(message); };
  const fixture = await startFixture(UI, { tls: ENGINE === "webkit", playwrightScreenshotStyle: true });
  const browser = await playwright[ENGINE].launch({ headless: true, ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}) });
  const control = body => json(fixture.origin + "/__fixture/control", body);
  try {
    for (const [width, height] of (process.env.PERSEA_KEYS_ONE_SHAPE ? [[390, 844]] : [[320, 568], [390, 844], [430, 932], [844, 390]])) {
      await control({ reset: true });
      const context = await browser.newContext({ viewport: { width, height }, hasTouch: true, isMobile: true, ignoreHTTPSErrors: true });
      await context.addInitScript(() => localStorage.setItem("persea-terminal.keyboard-device.v1", JSON.stringify({ version: 1, prefixes: { tmux: "key:b:1", screen: "key:a:1", Herdr: "key:a:1" } })));
      const page = await context.newPage();
      const record = { width, height, errors: [], states: [] }; report.shapes.push(record);
      let phase = "load";
      page.on("console", event => record.errors.push(`${phase}: ${event.type()}: ${event.text()}`));
      page.on("pageerror", error => record.errors.push(`${phase}: ${error}`));
      const inventory = await json(fixture.origin + "/api/inventory"), session = inventory.realms[0].servers[0].sessions[0];
      await page.goto(fixture.origin + "/terminal?engine=unified-dev#" + new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" }));
      const helper = page.locator(".persea-unified-xterm .xterm-helper-textarea");
      const composer = page.locator(".attachment-page__composer-textarea");
      const panel = page.locator(".persea-terminal-keys");
      const button = id => panel.locator(`[data-key-control="${id}"]`);
      await helper.waitFor();
      for (let i = 0; i < 100; i++) { if ((await control()).attachments.some(a => a.mode === "CONTROL")) break; await delay(30); }
      const inputStart = (await control()).attachments.flatMap(a => a.inputs).join("");
      const open = async () => {
        if (await panel.isVisible()) return;
        const toggle = page.locator(".persea-unified-keybar-key--bank");
        if (await toggle.isVisible()) await toggle.tap();
        else { await page.getByRole("button", { name: "Quick actions", exact: true }).tap(); await page.getByRole("button", { name: "Open Terminal Keys", exact: true }).tap(); }
      };
      const seed = async (field, draft) => {
        await field.focus();
        await field.evaluate((node, value) => { node.value = value; node.setSelectionRange(2, 8); window.__pickerFocusEvents = []; window.__pickerObserved = node; }, draft);
        await page.evaluate(() => {
          if (window.__pickerListenerInstalled) return;
          window.__pickerListenerInstalled = true;
          for (const type of ["focusin", "focusout"]) document.addEventListener(type, event => { if (event.target === window.__pickerObserved) window.__pickerFocusEvents.push(type); }, true);
          document.addEventListener("pointerdown", event => { window.__pickerPointerId = event.pointerId; }, true);
        });
      };
      const intact = async (field, draft, message) => {
        const state = await field.evaluate(node => ({ focused: node === document.activeElement, draft: node.value, selection: [node.selectionStart, node.selectionEnd], focusEvents: window.__pickerFocusEvents.slice() }));
        record.states.push({ phase, message, ...state });
        check(state.focused && state.draft === draft && state.selection[0] === 2 && state.selection[1] === 8 && state.focusEvents.length === 0, `${message}: ${JSON.stringify(state)}`);
      };
      const heightFits = async (name, mustFit = false) => {
        await delay(80);
        const geometry = await panel.evaluate(node => {
          const shell = document.querySelector(".persea-unified-terminal").getBoundingClientRect();
          const stage = document.querySelector(".persea-unified-scroll").getBoundingClientRect();
          const dock = document.querySelector(".persea-unified-composer-dock").getBoundingClientRect();
          const bar = document.querySelector(".persea-unified-keybar").getBoundingClientRect();
          const style = getComputedStyle(node), height = node.getBoundingClientRect().height;
          const content = [...node.children].reduce((sum, child) => sum + (getComputedStyle(child).position === "absolute" ? 0 : child.getBoundingClientRect().height), 0);
          return { compact: node.dataset.compact === "true", height, natural: content + parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth), overflow: node.scrollHeight - node.clientHeight, terminal: stage.height, available: shell.bottom - stage.top - dock.height - bar.height };
        });
        (record.heights ||= []).push({ name, ...geometry });
        if (geometry.compact) return;
        check(geometry.height <= geometry.natural + 1, `${name} uses no extra space beyond its content`);
        check(geometry.terminal >= 87 && geometry.height <= geometry.available * .75 + 1, `${name} preserves readable terminal context: ${JSON.stringify(geometry)}`);
        if (mustFit) check(geometry.overflow <= 1, `${name} fits without unnecessary inner scrolling: ${JSON.stringify(geometry)}`);
      };
      const choose = async (kind, value, mode = "touch") => {
        const trigger = button(kind), option = button(`${kind === "key-group" ? "choose-group" : "choose-prefix"}:${value}`);
        const current = (await trigger.getAttribute("aria-label")).split(": ").slice(1).join(": ");
        const activate = locator => mode === "touch" ? locator.tap() : locator.click();
        await activate(trigger);
        check(await trigger.getAttribute("aria-expanded") === "true" && await panel.getAttribute("data-picker") === kind, `${mode} ${kind} opens chooser`);
        check(await option.getAttribute("role") !== "option" && await option.locator("xpath=..").getAttribute("role") === "group", `${kind} choices use a labelled button group`);
        check(!!await option.locator("xpath=..").getAttribute("aria-label"), `${kind} chooser is labelled`);
        const old = await panel.locator('[aria-pressed="true"][data-key-control^="choose-"]').count();
        check(old === 1, `${kind} exposes exactly one current choice`);
        check(await button(`${kind === "key-group" ? "choose-group" : "choose-prefix"}:${current}`).getAttribute("aria-pressed") === "true", `${kind} pressed state matches the selected value`);
        await activate(option);
        check(await panel.isVisible() && await trigger.getAttribute("aria-expanded") === "false" && !await panel.getAttribute("data-picker"), `${mode} ${kind} choice closes only chooser`);
        check((await trigger.getAttribute("aria-label")).endsWith(": " + value), `${kind} trigger announces ${value}`);
      };
      phase = "native-touch-acquisition";
      await seed(helper, "unsubmitted draft");
      await page.setViewportSize({ width, height: Math.max(240, height - 336) }); await delay(100);
      await open(); await button("view:All keys").tap();
      // This selector also exists on the old native select. The first assertion
      // fails baseline on actual focus/draft loss, before new markup is required.
      await button("key-group").tap(); await delay(30);
      await intact(helper, "unsubmitted draft", "Real group acquisition preserves native focus, draft and selection");
      check(await panel.isVisible(), "Real group acquisition keeps panel open");
      await button("choose-group:Letters").tap();
      await intact(helper, "unsubmitted draft", "Touch group choice preserves native editor");
      await heightFits("Letters above the keyboard", true);
      await choose("key-group", "Navigation", "mouse");
      await intact(helper, "unsubmitted draft", "Mouse group choice preserves native editor");
      await heightFits("Navigation above the keyboard", true);
      await button("view:Fn").tap(); await heightFits("Fn above the keyboard", true);
      await button("view:All keys").tap();
      await choose("key-group", "Symbols"); await heightFits("Symbols above the keyboard", width >= 430);
      if (ENGINE === "chromium") {
        const scroll = await panel.evaluate(node => ({ top: node.scrollTop, max: node.scrollHeight - node.clientHeight }));
        if (scroll.max > 2) {
          const box = await panel.boundingBox(), client = await context.newCDPSession(page);
          await client.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: box.x + box.width / 2, y: box.y + box.height - 12 }] });
          for (let step = 1; step <= 6; step++) { await client.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: box.x + box.width / 2, y: box.y + box.height - 12 - step * 10 }] }); await delay(20); }
          await client.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] }); await delay(120);
          const after = await panel.evaluate(node => node.scrollTop);
          (record.nativeScroll ||= []).push({ before: scroll.top, after, max: scroll.max });
          check(after > scroll.top, "Native touch drag scrolls overflowing Keys content");
          await intact(helper, "unsubmitted draft", "Native panel scrolling preserves native editor");
          await client.detach();
        } else record.nativeScroll = [{ skipped: "Catalog fits without vertical scrolling" }];
      } else record.nativeScroll = [{ skipped: "Mobile WebKit native touch-drag is unavailable through this Playwright interface" }];
      await button("key-group").tap();
      check(await button("choose-group:Sequences").count() === 0 && await button("sequence-prefix").count() === 0, "Everyday chooser contains ordinary keys without advanced sequences");
      await delay(80);
      const geometry = await panel.evaluate(node => ({ overflow: document.documentElement.scrollWidth > innerWidth + 1, targets: [...node.querySelectorAll('button[data-key-control^="choose-"]')].map(button => { const r = button.getBoundingClientRect(); return { width: r.width, height: r.height }; }) }));
      record.geometry = geometry;
      check(!geometry.overflow && geometry.targets.every(r => r.width >= 43.9 && r.height >= 43.9), `Chooser fits the viewport with 44px choice targets: ${JSON.stringify(geometry)}`);
      phase = "genuine-keyboard-dismissal";
      await page.setViewportSize({ width, height }); await delay(120);
      check(!await panel.isVisible(), "Genuine keyboard viewport restoration still closes an open chooser and panel");
      phase = "keyboard-hidden-access";
      await helper.evaluate(node => node.blur()); await open(); await button("view:All keys").tap();
      await choose("key-group", "Navigation");
      await heightFits("Navigation with the keyboard hidden", height > 390);
      await choose("key-group", "Symbols", "mouse");
      await heightFits("Symbols catalog with the keyboard hidden");
      check(!await helper.evaluate(node => node === document.activeElement), "Manual chooser access never summons the hidden keyboard");
      phase = "composer-pointer";
      await page.locator(".persea-unified-composer-toggle").click();
      await composer.fill("composer draft"); await seed(composer, "composer draft");
      await page.setViewportSize({ width, height: Math.max(240, height - 336) }); await delay(100);
      await open(); await button("view:All keys").tap();
      await choose("key-group", "Navigation"); await intact(composer, "composer draft", "Touch group preserves composer draft and selection");
      await choose("key-group", "Symbols", "mouse");
      await intact(composer, "composer draft", "Mouse group preserves composer draft and selection");
      await heightFits("Symbols catalog sharing space with the composer");
      phase = "trusted-keyboard";
      await button("key-group").focus(); await page.keyboard.press("Enter");
      check(await panel.getAttribute("data-picker") === "key-group", "Trusted Enter opens group chooser");
      await button("choose-group:Navigation").focus(); await page.keyboard.press("Space");
      check(await button("key-group").evaluate(node => node === document.activeElement) && await panel.getAttribute("data-group") === "Navigation", "Trusted Space chooses group and returns focus to trigger");
      await button("key-group").press("Space"); await button("choose-group:Symbols").focus(); await page.keyboard.press("Enter");
      check(await button("key-group").evaluate(node => node === document.activeElement) && (await button("key-group").getAttribute("aria-label")).endsWith(": Symbols"), "Trusted Enter chooses group and returns focus to trigger");
      await button("key-group").press("Space"); await button("choose-group:Letters").focus(); await page.keyboard.press("Escape");
      check(await panel.isVisible() && !await panel.getAttribute("data-picker") && await button("key-group").evaluate(node => node === document.activeElement), "Scoped Escape closes chooser and restores its trigger");
      check(await composer.evaluate(node => node.value === "composer draft" && node.selectionStart === 2 && node.selectionEnd === 8), "Keyboard chooser navigation preserves composer draft and selection");
      phase = "drag-and-cancel";
      await open(); await button("view:All keys").click(); await choose("key-group", "Navigation", "mouse");
      await button("key-group").click();
      const choice = button("choose-group:Letters");
      const down = async locator => { await locator.scrollIntoViewIfNeeded(); const r = await locator.boundingBox(); await page.mouse.move(r.x + r.width / 2, r.y + r.height / 2); await page.mouse.down(); return r; };
      let held = await down(choice);
      check(await panel.getAttribute("data-group") === "Navigation", "Held choice does not commit before release");
      await page.mouse.move(held.x + held.width / 2 + 18, held.y + held.height / 2, { steps: 3 }); await page.mouse.up();
      check(await panel.getAttribute("data-group") === "Navigation" && await panel.getAttribute("data-picker") === "key-group", "Dragging cancels choice without closing chooser");
      await down(choice);
      // Explicit cancellation is a lifecycle falsifier; acquisition above uses
      // real trusted pointer events in both engines.
      await choice.evaluate(node => node.dispatchEvent(new PointerEvent("pointercancel", { bubbles: true, pointerId: window.__pickerPointerId, pointerType: "mouse" })));
      await page.mouse.up();
      check(await panel.getAttribute("data-group") === "Navigation", "Pointer cancellation cannot commit a held choice");
      await down(choice); await page.keyboard.press("Escape"); await page.mouse.up();
      check(await panel.getAttribute("data-group") === "Navigation" && !await panel.getAttribute("data-picker"), "Scoped closure invalidates a held choice release");
      phase = "transport-invalidated-choice";
      await button("key-group").click();
      await down(button("choose-group:Symbols"));
      await control({ closeLive: "transport_lost" }); await delay(100); await page.mouse.up();
      check(!await panel.isVisible(), "Transport loss closes and invalidates a held group choice");
      const end = await control();
      check(end.attachments.flatMap(a => a.inputs).join("") === inputStart, "Chooser interactions emit no implicit terminal input");
      check(end.attachments.every(a => a.resizes === 0 && a.resizeRequests.length === 0), "Chooser and viewport interactions never resize the remote terminal");
      check(record.errors.length === 0, `Picker console clean: ${record.errors.join("; ")}`);
      record.passed = true;
      await context.close();
    }
    fs.writeFileSync(path.join(OUT, `picker-${ENGINE}.json`), JSON.stringify(report, null, 2));
    console.log(`Picker ${ENGINE}: PASS (${report.shapes.length} viewports, ${report.assertions.length} assertions)`);
  } catch (error) {
    report.failure = String(error.stack || error);
    fs.writeFileSync(path.join(OUT, `picker-${ENGINE}-failure.json`), JSON.stringify(report, null, 2));
    throw error;
  } finally { await browser.close(); await fixture.close(); }
}
main().catch(error => { console.error(error.stack || String(error)); process.exitCode = 1; });
