"use strict";

const fs = require("fs");
const https = require("https");
const os = require("os");
const path = require("path");
const THEME_IDS = (() => {
  const source = require("fs").readFileSync(path.join(__dirname, "../src/unified_themes.ts"), "utf8");
  const match = /UNIFIED_THEME_IDS = Object\.freeze\(\[([^\]]+)\]/.exec(source);
  return match ? match[1].split(",").map((entry) => entry.trim().replace(/^"|"$/g, "")).filter(Boolean) : [];
})();
const { startFixture } = require("./unified_reopen_fixture.cjs");
const { requestJSON: requestHTTPJSON } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_VIEW_DISCLOSURE_ENGINE || "chromium";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const EVIDENCE = path.resolve(process.env.PERSEA_VIEW_DISCLOSURE_EVIDENCE_DIR || path.join("/tmp", `persea-view_disclosure-${ENGINE}`));
const MUTANT = process.env.PERSEA_VIEW_DISCLOSURE_MUTANT || "";
const MUTANTS = new Set(["wide-toolbar", "standalone-view", "in-flow-popover", "duplicate-section", "focus-only-open", "old-section-order", "remove-inert", "refit_failure-active-state", "refit_failure-hide-focus", "refit_failure-symbolic-binding"]);
assert(!MUTANT || MUTANTS.has(MUTANT), `unknown view disclosure mutant ${MUTANT}`);
const SHAPES = Object.freeze([
  { name: "wide-1906", viewport: { width: 1906, height: 1270 }, touch: false },
  { name: "fine-1100", viewport: { width: 1100, height: 760 }, touch: false },
  { name: "desktop-1366", viewport: { width: 1366, height: 768 }, touch: false },
  { name: "phone-390", viewport: { width: 390, height: 844 }, touch: true },
  { name: "phone-360", viewport: { width: 360, height: 780 }, touch: true },
].filter((shape) => !process.env.PERSEA_VIEW_DISCLOSURE_CASE || shape.name === process.env.PERSEA_VIEW_DISCLOSURE_CASE));

function assert(value, message) { if (!value) throw new Error(message); }
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function prepareFixtureUI() {
  if (MUTANT !== "refit_failure-symbolic-binding") return { root: UI, cleanup() {} };

  // This compiling mutant removes the actual UnifiedTerminalPage -> Composer
  // ownership binding. It deliberately does not alter DOM classes or CSS, so
  // the 44px oracle can pass only when production mounting supplies the marker.
  const root = fs.mkdtempSync(path.join(process.env.TMPDIR || os.tmpdir(), "persea-view_disclosure-symbolic-binding-"));
  const dist = path.join(root, "dist");
  fs.cpSync(path.join(UI, "dist"), dist, { recursive: true });
  let loaded = 0;
  let replacements = 0;
  const esbuild = require(path.join(UI, "node_modules/esbuild"));
  const result = await esbuild.build({
    entryPoints: [path.join(UI, "src/app.ts")],
    bundle: true,
    format: "esm",
    platform: "browser",
    sourcemap: true,
    outfile: path.join(dist, "app.js"),
    metafile: true,
    plugins: [{
      name: "refit_failure-omit-symbolic-composer-binding",
      setup(build) {
        build.onLoad({ filter: /unified_terminal_page\.ts$/ }, (args) => {
          loaded += 1;
          const source = fs.readFileSync(args.path, "utf8");
          const contents = source.replace(/symbolicChrome:\s*true/g, () => {
            replacements += 1;
            return "symbolicChrome: false";
          });
          return { contents, loader: "ts", resolveDir: path.dirname(args.path) };
        });
      },
    }],
  });
  assert(loaded === 1 && replacements === 1,
    `symbolic binding mutant did not replace exactly one production binding: ${JSON.stringify({ loaded, replacements })}`);
  console.log(`view disclosure compiling mutant: omitted ${replacements} symbolicChrome production binding`);
  fs.writeFileSync(path.join(dist, "app.meta.json"), JSON.stringify(result.metafile));
  const remove = () => fs.rmSync(root, { recursive: true, force: true });
  process.once("exit", remove);
  return {
    root,
    cleanup() {
      process.removeListener("exit", remove);
      remove();
    },
  };
}

const requestJSON = (url, method = "GET", body) => {
  if (!url.startsWith("https:")) return requestHTTPJSON(url, method, body);
  return new Promise((resolve, reject) => {
    const request = https.request(url, { method, rejectUnauthorized: false, headers: body ? { "Content-Type": "application/json" } : {} }, (response) => {
      let text = "";
      response.setEncoding("utf8");
      response.on("data", (chunk) => { text += chunk; });
      response.on("end", () => { try { resolve(JSON.parse(text)); } catch (error) { reject(error); } });
    });
    request.on("error", reject);
    if (body) request.write(JSON.stringify(body));
    request.end();
  });
};

async function main() {
  const playwright = require(MODULE);
  assert(playwright[ENGINE], `Playwright has no ${ENGINE} engine`);
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const fixtureUI = await prepareFixtureUI();
  const fixture = await startFixture(fixtureUI.root, { tls: ENGINE === "webkit" });
  const control = (input) => requestJSON(`${fixture.origin}/__fixture/control`, "POST", input);
  const snapshot = () => requestJSON(`${fixture.origin}/__fixture/control`);
  const browser = await playwright[ENGINE].launch({
    headless: true,
    ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
  });
  const evidence = { engine: ENGINE, cases: [], screenshots: [] };
  try {
    for (const shape of SHAPES) {
      await control({ reset: true, switchSessions: true, sessionBState: "open" });
      const inventory = await requestJSON(`${fixture.origin}/api/inventory`);
      const session = inventory.realms[0].servers[0].sessions[0];
      const context = await browser.newContext({ viewport: shape.viewport, isMobile: shape.touch, hasTouch: shape.touch, deviceScaleFactor: shape.touch ? 2 : 1, ignoreHTTPSErrors: true });
      const page = await context.newPage();
      const errors = [];
      const httpErrors = [];
      page.on("pageerror", (error) => errors.push(String(error)));
      page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
      page.on("response", (response) => { if (response.status() >= 400) httpErrors.push({ status: response.status(), url: response.url() }); });
      const url = `${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" })}`;
      await page.goto(url, { waitUntil: "load" });
      await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
      if (MUTANT) {
        await page.evaluate((mutant) => {
          const style = document.createElement("style");
          style.nonce = document.querySelector('meta[name="persea-style-nonce"]')?.getAttribute("content") || "";
          style.dataset.view_disclosureMutant = mutant;
          if (mutant === "wide-toolbar") style.textContent = ".persea-unified-toolbar{height:143px!important;flex-wrap:wrap!important}";
          if (mutant === "in-flow-popover") style.textContent = ".persea-unified-view-popover{position:static!important;width:100%!important}";
          if (mutant === "focus-only-open") style.textContent = ".persea-unified-view-disclosure[aria-expanded=true]{color:#777!important;background:#777!important}";
          if (mutant === "refit_failure-active-state") style.textContent = ".persea-unified-composer-toggle[aria-expanded=true]{box-shadow:none!important}";
          if (mutant === "refit_failure-hide-focus") style.textContent = ".attachment-page__composer-close:focus-visible{outline:none!important}";
          if (style.textContent) document.head.append(style);
          if (mutant === "standalone-view") {
            const button = document.createElement("button");
            button.textContent = "Aa";
            document.querySelector(".persea-unified-toolbar")?.append(button);
          }
        }, MUTANT);
      }
      const resizeCount = async () => (await snapshot()).attachments.reduce((sum, attachment) => sum + attachment.resizes, 0);
      const inputCount = async () => (await snapshot()).attachments.reduce((sum, attachment) => sum + attachment.inputs.length, 0);
      const overlayState = (target = page) => target.evaluate(() => ({
        viewHidden: document.querySelector(".persea-unified-view-popover")?.hidden,
        viewExpanded: document.querySelector(".persea-unified-view-disclosure")?.getAttribute("aria-expanded"),
        tagHidden: document.querySelector(".persea-unified-identity__details")?.hidden,
        tagExpanded: document.querySelector(".persea-unified-tag")?.getAttribute("aria-expanded"),
        sheetHidden: document.querySelector(".persea-unified-sheet")?.hidden,
        sheetExpanded: document.querySelector('[aria-label="Quick actions"]')?.getAttribute("aria-expanded"),
        explainerHidden: document.querySelector(".persea-unified-explainer")?.hidden,
        openData: [...document.querySelectorAll('.persea-unified-view-disclosure[data-open="true"], .persea-unified-tag[data-open="true"], [aria-label="Quick actions"][data-open="true"]')]
          .map((node) => node.className),
        active: document.activeElement?.className || document.activeElement?.tagName,
      }));
      const assertOverlaysClosed = async (phase, target = page) => {
        const state = await overlayState(target);
        assert(state.viewHidden === true && state.viewExpanded === "false"
          && state.tagHidden === true && state.tagExpanded === "false"
          && state.sheetHidden === true && state.sheetExpanded === "false"
          && state.explainerHidden === true && state.openData.length === 0,
        `${shape.name}: ${phase} retained overlay/open state ${JSON.stringify(state)}`);
        return state;
      };
      const shellBoxes = () => page.evaluate(() => {
        const result = {};
        for (const selector of [".persea-unified-toolbar", ".persea-unified-stage", ".persea-unified-scroll", ".persea-unified-xterm"]) {
          const node = document.querySelector(selector);
          const box = node?.getBoundingClientRect();
          if (box) result[selector] = { x: box.x, y: box.y, width: box.width, height: box.height };
        }
        return result;
      });
      const sameBoxes = (left, right) => JSON.stringify(left) === JSON.stringify(right);
      const tile = (name) => page.locator(".persea-unified-sheet__tile").filter({ has: page.locator(".persea-unified-sheet__label", { hasText: name }) }).first();
      const initial = await page.evaluate(() => {
        const toolbar = document.querySelector(".persea-unified-toolbar");
        const stage = document.querySelector(".persea-unified-stage");
        if (!toolbar || !stage) throw new Error("view disclosure toolbar or stage absent");
        const box = toolbar.getBoundingClientRect();
        const stageBox = stage.getBoundingClientRect();
        const visible = [...toolbar.querySelectorAll("button, input, select")].filter((node) => {
          const nodeBox = node.getBoundingClientRect();
          return nodeBox.width > 0 && nodeBox.height > 0;
        });
        return {
          toolbar: { x: box.x, y: box.y, width: box.width, height: box.height, bottom: box.bottom, scrollWidth: toolbar.scrollWidth },
          stage: { y: stageBox.y },
          inputs: visible.filter((node) => node.matches("input, select")).length,
          controls: visible.map((node) => ({ label: node.getAttribute("aria-label") || node.textContent.trim(), text: node.textContent.trim(), box: (() => { const b = node.getBoundingClientRect(); return { x: b.x, y: b.y, width: b.width, height: b.height, right: b.right }; })() })),
        };
      });
      const file = path.join(EVIDENCE, `${ENGINE}-${shape.name}-baseline.png`);
      await page.screenshot({ path: file, fullPage: true });
      evidence.screenshots.push(file);
      assert(initial.inputs === 0, `${shape.name}: toolbar has ${initial.inputs} visible input/select descendants`);
      assert(Math.abs(initial.stage.y - initial.toolbar.bottom) <= 1, `${shape.name}: terminal stage does not begin directly below toolbar`);
      assert(!initial.controls.some((item) => ["Aa", "↕", "⋯"].includes(item.text)), `${shape.name}: obsolete standalone view control remains: ${JSON.stringify(initial.controls)}`);
      if (!shape.touch) {
        assert(initial.toolbar.height <= 52.1, `${shape.name}: toolbar height ${initial.toolbar.height}px exceeds 52px`);
      } else {
        assert(initial.toolbar.scrollWidth <= initial.toolbar.width + 1, `${shape.name}: toolbar overflows horizontally`);
        assert(initial.controls.every((item) => item.box.width >= 43.5 && item.box.height >= 43.5), `${shape.name}: undersized control ${JSON.stringify(initial.controls)}`);
        assert(initial.controls.every((item, index, list) => index === 0 || Math.abs(item.box.y - list[0].box.y) < 0.2), `${shape.name}: toolbar wraps`);
        const geometry = initial.controls.find((item) => /^\d+×\d+$/.test(item.text));
        assert(geometry, `${shape.name}: committed geometry disclosure is absent`);
      }

      const beforeViewBoxes = await shellBoxes();
      const beforeViewResizes = await resizeCount();
      const viewButton = page.locator(".persea-unified-view-disclosure");
      const viewButtonBox = await viewButton.boundingBox();
      let terminalFocusBeforeView;
      if (shape.name === "phone-390") {
        await page.locator(".xterm-helper-textarea").focus();
        terminalFocusBeforeView = await page.evaluate(() => ({
          active: document.activeElement?.className,
          visualHeight: window.visualViewport?.height ?? window.innerHeight,
          innerHeight: window.innerHeight,
        }));
      }
      await viewButton.click();
      const popover = page.locator(".persea-unified-view-popover");
      await popover.waitFor({ state: "visible" });
      const viewEvidence = await popover.evaluate((node) => {
        const box = node.getBoundingClientRect();
        const inputs = [...node.querySelectorAll("input")].map((input) => {
          const inputBox = input.getBoundingClientRect();
          const fontSize = Number.parseFloat(getComputedStyle(input).fontSize);
          return { label: input.getAttribute("aria-label"), width: inputBox.width, em: inputBox.width / fontSize };
        });
        return {
          width: box.width,
          direct: [...node.children].map((child) => child.className),
          zoom: [...node.querySelectorAll(".persea-unified-view-popover__zoom button")].map((button) => button.textContent.trim()),
          size: node.querySelector(".persea-unified-size")?.textContent.replace(/\s+/g, " ").trim(),
          inputs,
          themes: node.querySelectorAll(".persea-unified-preference").length,
        };
      });
      assert(viewEvidence.width <= 352.5, `${shape.name}: View popover exceeds the compact 22rem bound: ${JSON.stringify(viewEvidence)}`);
      assert(JSON.stringify(viewEvidence.zoom) === JSON.stringify(["Zoom out", "Fit font", "Zoom in"]), `${shape.name}: View zoom order is wrong: ${JSON.stringify(viewEvidence)}`);
      assert(JSON.stringify(viewEvidence.direct) === JSON.stringify([
        "persea-unified-view-popover__zoom",
        "persea-unified-size",
      ]), `${shape.name}: compact View groups are absent or unordered: ${JSON.stringify(viewEvidence)}`);
      // refit actions first (rows, then width), then the one form
      // row of Columns, Rows and a single Apply.
      assert(viewEvidence.size?.startsWith("↕ Fit rows")
        && viewEvidence.size.indexOf("↔ Fit width") > viewEvidence.size.indexOf("↕ Fit rows")
        && viewEvidence.size.indexOf("Cols") > viewEvidence.size.indexOf("↔ Fit width")
        && viewEvidence.size.indexOf("Rows") > viewEvidence.size.indexOf("Cols")
        && viewEvidence.size.endsWith("Apply")
        && !viewEvidence.size.includes("Refit")
        && !viewEvidence.size.includes("Terminal size"),
      `${shape.name}: compact geometry actions are absent, duplicated, or unordered: ${JSON.stringify(viewEvidence)}`);
      assert(viewEvidence.inputs.length === 2 && viewEvidence.inputs.every((input) => input.em <= 6.1), `${shape.name}: size inputs exceed 6ch: ${JSON.stringify(viewEvidence.inputs)}`);
      // preference pickers live on the dashboard, not in View.
      assert(viewEvidence.themes === 0, `${shape.name}: a preference picker leaked into the View popover`);
      assert(sameBoxes(beforeViewBoxes, await shellBoxes()), `${shape.name}: opening View moved toolbar/xterm boxes`);
      assert(await resizeCount() === beforeViewResizes, `${shape.name}: opening View emitted resize`);
      assert(await viewButton.getAttribute("aria-expanded") === "true", `${shape.name}: View semantic open state absent`);
      assert(JSON.stringify(viewButtonBox) === JSON.stringify(await viewButton.boundingBox()), `${shape.name}: View open state changed its box`);
      // no theme control in the chrome; the catalogue comes from
      // the product's theme module, the same list the dashboard card offers.
      const contrasts = await page.evaluate((themeIDs) => {
        const shell = document.querySelector(".persea-unified-terminal");
        const button = document.querySelector(".persea-unified-view-disclosure");
        const original = shell.dataset.theme;
        const channel = (value) => { const x = value / 255; return x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4; };
        const rgb = (value) => {
          const channels = (value.match(/[\d.]+/g) || []).slice(0, 3).map(Number);
          return value.startsWith("color(srgb ") ? channels.map((channelValue) => channelValue * 255) : channels;
        };
        const luminance = (value) => { const [r, g, b] = rgb(value).map(channel); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
        const ratio = (a, b) => { const hi = Math.max(a, b); const lo = Math.min(a, b); return (hi + 0.05) / (lo + 0.05); };
        const rows = themeIDs.map((id) => {
          shell.dataset.theme = id;
          const style = getComputedStyle(button);
          return { theme: id, ratio: ratio(luminance(style.color), luminance(style.backgroundColor)) };
        });
        shell.dataset.theme = original;
        return rows;
      }, THEME_IDS);
      assert(contrasts.length === 8 && contrasts.every((entry) => entry.ratio >= 4.5), `${shape.name}: open-state contrast failed: ${JSON.stringify(contrasts)}`);
      const viewFile = path.join(EVIDENCE, `${ENGINE}-${shape.name}-view-popover.png`);
      await page.screenshot({ path: viewFile, fullPage: true }); evidence.screenshots.push(viewFile);

      await page.keyboard.press("Escape");
      assert(await viewButton.getAttribute("aria-expanded") === "false" && !await popover.isVisible(), `${shape.name}: Escape did not clear View open state`);
      let terminalFocusAfterEscape;
      if (shape.name === "phone-390") {
        terminalFocusAfterEscape = await page.evaluate(() => ({
          active: document.activeElement?.className,
          visualHeight: window.visualViewport?.height ?? window.innerHeight,
          innerHeight: window.innerHeight,
        }));
        assert(terminalFocusBeforeView?.active.includes("xterm-helper-textarea")
          && terminalFocusAfterEscape.active.includes("xterm-helper-textarea")
          && terminalFocusAfterEscape.visualHeight === terminalFocusBeforeView.visualHeight
          && terminalFocusAfterEscape.innerHeight === terminalFocusBeforeView.innerHeight,
        `${shape.name}: Escape stole terminal/keyboard posture ${JSON.stringify({ terminalFocusBeforeView, terminalFocusAfterEscape })}`);
        const beforeEscapeInput = await inputCount();
        await page.keyboard.type("q");
        await page.waitForFunction((count) => window.fetch("/__fixture/control").then((response) => response.json())
          .then((value) => value.attachments.reduce((sum, attachment) => sum + attachment.inputs.length, 0) === count + 1), beforeEscapeInput);
        if (!await popover.isVisible()) await viewButton.click();
        await popover.waitFor({ state: "visible" });
        await popover.getByRole("textbox", { name: "Rows", exact: true }).focus();
        await page.keyboard.press("Escape");
        assert(await page.evaluate(() => document.activeElement === document.querySelector(".persea-unified-view-disclosure")),
          `${shape.name}: Escape from inside View did not restore disclosure focus`);
      }
      const beforeSheetResizes = await resizeCount();
      await page.getByRole("button", { name: "Quick actions", exact: true }).click();
      const sheet = page.locator(".persea-unified-sheet");
      await sheet.waitFor({ state: "visible" });
      if (MUTANT === "old-section-order" || MUTANT === "duplicate-section") {
        await sheet.evaluate((node, mutant) => {
          if (mutant === "old-section-order") {
            const sections = [...node.querySelectorAll(".persea-unified-sheet__section")];
            const selection = sections.find((section) => section.querySelector(".persea-unified-sheet__heading")?.textContent === "Selection & clipboard");
            if (selection) selection.parentElement?.append(selection);
          } else {
            const section = document.createElement("section");
            section.className = "persea-unified-sheet__section";
            section.innerHTML = '<h2 class="persea-unified-sheet__heading">View</h2>';
            node.append(section);
          }
        }, MUTANT);
      }
      const sheetEvidence = await sheet.evaluate((node) => {
        return {
          headings: [...node.querySelectorAll(".persea-unified-sheet__heading")].filter((heading) => !heading.parentElement.hidden).map((heading) => heading.textContent.trim()),
		  labels: [...node.querySelectorAll(".persea-unified-sheet__label")].map((label) => label.textContent.trim()),
        };
      });
      assert(!sheetEvidence.headings.some((heading) => ["View", "Terminal size", "Appearance"].includes(heading)), `${shape.name}: duplicate view section remains: ${JSON.stringify(sheetEvidence)}`);
	  assert(!sheetEvidence.headings.includes("Selection & clipboard") && !sheetEvidence.labels.some((label) => ["Select", "Copy", "Paste"].includes(label)),
		`${shape.name}: Quick actions duplicates the contextual clipboard owner: ${JSON.stringify(sheetEvidence)}`);
      assert(await resizeCount() === beforeSheetResizes, `${shape.name}: Quick actions emitted resize`);
      const sheetFile = path.join(EVIDENCE, `${ENGINE}-${shape.name}-quick-actions.png`);
      await page.screenshot({ path: sheetFile, fullPage: true }); evidence.screenshots.push(sheetFile);
	  await page.keyboard.press("Escape");
	  const select = page.locator(".persea-unified-select-context");
	  await select.click();
      if (MUTANT === "remove-inert") {
        await page.evaluate(() => {
          const scroll = document.querySelector(".persea-unified-scroll");
          if (scroll) { scroll.inert = false; scroll.removeAttribute("aria-hidden"); }
        });
      }
      const overlay = page.locator(".persea-unified-select");
      await overlay.waitFor({ state: "visible" });
      const selectState = await page.evaluate(() => ({
		pressed: document.querySelector(".persea-unified-select-context")?.getAttribute("aria-pressed"),
		state: document.querySelector(".persea-unified-select-context")?.dataset.selectState,
        inert: document.querySelector(".persea-unified-scroll")?.inert,
        hidden: document.querySelector(".persea-unified-scroll")?.getAttribute("aria-hidden"),
        rows: document.querySelectorAll(".persea-unified-select__row").length,
      }));
	  assert(selectState.pressed === "true" && selectState.state === "selecting" && selectState.inert === true && selectState.hidden === "true" && selectState.rows > 0, `${shape.name}: Select invariant failed: ${JSON.stringify(selectState)}`);
      const selectFile = path.join(EVIDENCE, `${ENGINE}-${shape.name}-select.png`);
      await page.screenshot({ path: selectFile, fullPage: true }); evidence.screenshots.push(selectFile);
	  await page.evaluate(() => {
		const row = [...document.querySelectorAll(".persea-unified-select__row")].find((entry) => (entry.textContent || "").trim() !== "");
		if (!row) throw new Error("missing frozen selection row");
		const range = document.createRange(); range.selectNodeContents(row);
		const selection = window.getSelection(); selection.removeAllRanges(); selection.addRange(range);
	  });
	  // the Paste slot is the Copy control while the range exists.
	  await page.waitForFunction(() => document.querySelector(".persea-unified-toolbar-paste")?.dataset.pasteState === "copy");
	  await page.locator(".persea-unified-toolbar-paste").click();
      await overlay.waitFor({ state: "hidden" });

      if (shape.name === "wide-1906" || shape.name === "phone-390") {
        await viewButton.click(); await popover.waitFor({ state: "visible" });
        const rows = popover.getByRole("textbox", { name: "Rows", exact: true });
        const apply = popover.getByRole("button", { name: "Apply", exact: true });
        const zero = await resizeCount();
        await rows.fill("25"); await rows.press("Enter");
        assert(await resizeCount() === zero, `${shape.name}: input or Enter emitted resize`);
        await apply.evaluate((button) => button.dispatchEvent(new MouseEvent("click", { bubbles: true })));
        assert(await resizeCount() === zero, `${shape.name}: untrusted Apply emitted resize`);
        await apply.click();
        await page.waitForFunction(() => document.querySelector(".persea-unified-view-disclosure")?.textContent === "80×25");
        assert(await resizeCount() === zero + 1, `${shape.name}: trusted Apply did not emit exactly once`);
        const fitRows = popover.getByRole("button", { name: "Fit rows", exact: false });
        await fitRows.click();
        await page.waitForFunction((count) => window.fetch("/__fixture/control").then((response) => response.json()).then((value) => value.attachments.reduce((sum, attachment) => sum + attachment.resizes, 0) === count), zero + 2);
        assert((await viewButton.textContent()).startsWith("80×"), `${shape.name}: rows-only Fit changed columns`);
      }
      await page.keyboard.press("Escape");

      const composer = page.locator(".persea-unified-composer-toggle");
      const composerPaint = () => composer.evaluate((button) => {
        const style = getComputedStyle(button);
        return { background: style.backgroundColor, border: style.borderColor, shadow: style.boxShadow };
      });
      const sameRect = (left, right) => left && right && ["x", "y", "width", "height"].every((key) => Math.abs(left[key] - right[key]) < 0.2);
      const beforeComposerResize = await resizeCount();
      const beforeComposerBox = await composer.boundingBox();
      const beforeComposerPaint = await composerPaint();
      assert(await composer.getAttribute("aria-label") === "Open composer", `${shape.name}: composer control does not name its closed action`);
      await composer.click();
      assert(await composer.getAttribute("aria-expanded") === "true", `${shape.name}: Composer semantic open state absent`);
      assert(await composer.getAttribute("aria-label") === "Hide composer", `${shape.name}: composer control does not name its open action`);
      await page.waitForTimeout(400);
      const composerFile = path.join(EVIDENCE, `${ENGINE}-${shape.name}-composer-open.png`);
      await page.screenshot({ path: composerFile, fullPage: true }); evidence.screenshots.push(composerFile);
      const projected = { closed: beforeComposerPaint, open: await composerPaint() };
      assert(projected.open.background !== projected.closed.background && projected.open.shadow !== projected.closed.shadow,
        `${shape.name}: composer open state has no persistent highlight: ${JSON.stringify(projected)}`);
      const mode = page.locator(".attachment-page__composer-mode-toggle");
      assert(await mode.getAttribute("aria-label") === "Code input" && await mode.getAttribute("aria-pressed") === "false", `${shape.name}: code mode semantics are ambiguous`);
      await mode.click();
      assert(await mode.getAttribute("aria-label") === "Prose writing mode" && await mode.getAttribute("aria-pressed") === "true", `${shape.name}: prose mode semantics are ambiguous`);
      const hideComposer = page.locator(".attachment-page__composer-close");
      assert(await hideComposer.getAttribute("aria-label") === "Hide composer — draft stays here", `${shape.name}: internal Hide composer action is not explicit`);
      const hideBox = await hideComposer.boundingBox();
      assert(hideBox && hideBox.width >= 44 && hideBox.height >= 44, `${shape.name}: Hide composer target is below 44px: ${JSON.stringify(hideBox)}`);
      const hideProjection = await hideComposer.evaluate((button) => ({
        symbolic: button.closest(".attachment-page__composer")?.classList.contains("attachment-page__composer--symbolic") === true,
        unifiedDock: button.closest(".persea-unified-composer-dock") !== null,
      }));
      assert(hideProjection.symbolic && hideProjection.unifiedDock,
        `${shape.name}: Hide composer target did not arrive through the unified symbolic host: ${JSON.stringify(hideProjection)}`);
      for (let step = 0; step < 4 && !await hideComposer.evaluate((button) => document.activeElement === button); step += 1) {
        await page.keyboard.press("Shift+Tab");
      }
      const hideFocus = await hideComposer.evaluate((button) => {
        const style = getComputedStyle(button);
        return { style: style.outlineStyle, width: Number.parseFloat(style.outlineWidth), active: document.activeElement === button };
      });
      assert(hideFocus.active && hideFocus.style !== "none" && hideFocus.width >= 2, `${shape.name}: Hide composer focus ring is absent: ${JSON.stringify(hideFocus)}`);
      assert(await resizeCount() === beforeComposerResize, `${shape.name}: Composer emitted resize`);
      await composer.click();
      assert(await composer.getAttribute("aria-label") === "Open composer", `${shape.name}: composer close state did not project`);
      const afterComposerBox = await composer.boundingBox();
      assert(sameRect(beforeComposerBox, afterComposerBox), `${shape.name}: composer toggle box shifted with state`);

      let lifecycleEvidence;
      if (shape.name === "phone-390") {
        // A terminal close is tested on a fresh, ordinary attachment: stale
        // target must replace View with the typed failure, never stack it.
        if (!await popover.isVisible()) await viewButton.click();
        await popover.waitFor({ state: "visible" });
        await control({ closeLive: "stale_target" });
        try {
          await page.waitForFunction(() => document.querySelector(".persea-unified-notice:not([hidden]) .persea-unified-notice__code")?.textContent?.includes("stale_target"), undefined, { timeout: 5_000 });
        } catch (error) {
          throw new Error(`stale_target did not render ${JSON.stringify({ overlays: await overlayState(), body: (await page.locator("body").innerText()).slice(-500), server: await snapshot() })}`, { cause: error });
        }
        const staleTarget = await assertOverlaysClosed("stale_target failure");
        await page.close();

        // Replacement is deliberately held before its synchronous identity
        // commit. View opens during that fallible interval; the commit must
        // remove it before the successor presentation can become visible.
        await control({ reset: true, switchSessions: true, sessionBState: "open" });
        const replacementInventory = await requestJSON(`${fixture.origin}/api/inventory`);
        const replacementSession = replacementInventory.realms[0].servers[0].sessions[0];
        const replacementPage = await context.newPage();
        replacementPage.on("pageerror", (error) => errors.push(String(error)));
        replacementPage.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
        replacementPage.on("response", (response) => { if (response.status() >= 400) httpErrors.push({ status: response.status(), url: response.url() }); });
        const replacementURL = `${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: replacementSession.handles.control, mode: "control", history: "1000", name: replacementSession.name, draft_scope: fixture.draftScope, engine: "unified-dev" })}`;
        await replacementPage.goto(replacementURL, { waitUntil: "load" });
        await replacementPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
        await control({ sessionBState: "adoptable", holdAdoptionMs: 1_500 });
        await replacementPage.locator(".persea-unified-tag").click();
        await replacementPage.getByRole("button", { name: "Refresh sessions" }).click();
        const beta = replacementPage.getByRole("button", { name: "Switch to beta" });
        await beta.waitFor();
        await beta.click();
        const replacementView = replacementPage.locator(".persea-unified-view-disclosure");
        const replacementPopover = replacementPage.locator(".persea-unified-view-popover");
        await replacementView.click();
        await replacementPopover.waitFor({ state: "visible" });
        await replacementPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("beta-replay"), undefined, { timeout: 10_000 });
        const replacement = await assertOverlaysClosed("session replacement", replacementPage);

        await replacementPage.locator(".persea-unified-tag").click();
        await replacementPage.locator(".persea-unified-identity__details").waitFor({ state: "visible" });
        const beforeReconnect = (await snapshot()).attachments.length;
        await control({ closeSession: { session: "B", reason: "subscriber_lagged" } });
        await replacementPage.waitForFunction(() => document.querySelector(".persea-unified-identity__details")?.hidden, undefined, { timeout: 2_000 });
        const tagClose = await assertOverlaysClosed("reconnect with tag open", replacementPage);
        await replacementPage.waitForFunction((count) => window.fetch("/__fixture/control").then((response) => response.json())
          .then((value) => value.attachments.length > count && value.attachments.at(-1)?.live), beforeReconnect, { timeout: 10_000 });
        lifecycleEvidence = { replacement, tagClose, staleTarget };
      }

      const filteredErrors = errors.filter((entry) => !/Refused to apply a stylesheet/.test(entry) && !/Failed to load resource: the server responded with a status of 403/.test(entry));
      const expectedWebKitClipRefusal = ENGINE === "webkit" && httpErrors.length <= 2 && httpErrors.every((entry) => entry.status === 403 && new URL(entry.url).pathname === "/api/snippets");
      assert(filteredErrors.length === 0, `${shape.name}: browser errors ${JSON.stringify(filteredErrors)}`);
      assert(httpErrors.length === 0 || expectedWebKitClipRefusal, `${shape.name}: HTTP errors ${JSON.stringify(httpErrors)}`);
      evidence.cases.push({
        shape: shape.name,
        initial,
        viewEvidence,
        contrasts,
        sheetEvidence,
        selectState,
        composerEvidence: { projected, hideBox, hideProjection, hideFocus },
        terminalFocusBeforeView,
        terminalFocusAfterEscape,
        lifecycleEvidence,
        errors,
        httpErrors,
      });
      await context.close();
    }
    fs.writeFileSync(path.join(EVIDENCE, `view_disclosure-${ENGINE}.json`), JSON.stringify(evidence, null, 2));
    console.log(`view disclosure ${ENGINE}: PASS (${evidence.cases.length} postures)`);
  } finally {
    await browser.close();
    await Promise.race([fixture.close(), delay(3_000)]);
    fixtureUI.cleanup();
  }
}

main().catch((error) => {
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const detail = String(error && error.stack || error);
  fs.writeFileSync(path.join(EVIDENCE, `view_disclosure-${ENGINE}-failure.log`), `${detail}\n`);
  console.error(detail);
  process.exitCode = 1;
});
