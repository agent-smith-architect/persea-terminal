"use strict";

// session switch hermetic browser gate. The real bundled controller, transport, page,
// xterm and shared session-list renderer run against the reopen fixture's
// one-time capabilities, leases, adoption endpoint and broker automaton.

const fs = require("fs");
const https = require("https");
const path = require("path");
const { startFixture } = require("./unified_reopen_fixture.cjs");
const { requestJSON: requestHTTPJSON } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_SESSION_SWITCH_ENGINE || "chromium";
const POINTER = process.env.PERSEA_SESSION_SWITCH_POINTER || "coarse";
const AUTHORITY_RACES_ONLY = process.env.PERSEA_SESSION_SWITCH_AUTHORITY_RACES_ONLY === "1";
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const EVIDENCE = process.env.PERSEA_SESSION_SWITCH_EVIDENCE_DIR ? path.resolve(process.env.PERSEA_SESSION_SWITCH_EVIDENCE_DIR) : null;

function assert(value, message) { if (!value) throw new Error(message); }
const delay = (ms) => new Promise((done) => setTimeout(done, ms));
const phase = (name) => process.stdout.write(`SESSION_SWITCH_PHASE ${name}\n`);
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
  assert(path.isAbsolute(MODULE), "PERSEA_PLAYWRIGHT_MODULE must be absolute");
  const playwright = require(MODULE);
  const fixture = await startFixture(UI, { tls: ENGINE === "webkit" });
  const control = (input) => requestJSON(`${fixture.origin}/__fixture/control`, "POST", input);
  const snapshot = () => requestJSON(`${fixture.origin}/__fixture/control`);
  const waitSnapshot = async (predicate, timeoutMs = 12_000) => {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const value = await snapshot();
      if (predicate(value)) return value;
      await delay(40);
    }
    return null;
  };
  const browserType = ENGINE === "webkit" ? playwright.webkit : playwright.chromium;
  const browser = await browserType.launch({
    headless: true,
    ...(ENGINE === "chromium" ? { executablePath: require("./browser_path.cjs")() } : {}),
  });
  assert(POINTER === "coarse" || POINTER === "fine", `unsupported pointer mode ${POINTER}`);
  const contextOptions = POINTER === "coarse"
    ? { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2, ignoreHTTPSErrors: true }
    : { viewport: { width: 1280, height: 800 }, ignoreHTTPSErrors: true };
  const context = await browser.newContext(contextOptions);
  const page = await context.newPage();
  const installPageInstrumentation = () => {
    const original = window.fetch.bind(window);
    const originalFocus = HTMLTextAreaElement.prototype.focus;
    window.__session_switchInventoryFetches = [];
    window.__session_switchTextareaFocuses = [];
    window.__session_switchCommitSamples = [];
    window.__session_switchIgnoredAborts = [];
    window.__session_switchHandleResolutions = 0;
    window.__session_switchInventoryResolutions = 0;
    window.__session_switchAuthorityResults = [];
    window.__perseaSessionSwitchAuthorityProbe = (event) => { window.__session_switchAuthorityResults.push(event); };
    window.__perseaSessionSwitchSwitchProbe = (phase, sample) => {
      window.__session_switchSwitchSample = sample;
      const immediate = sample();
      queueMicrotask(() => window.__session_switchCommitSamples.push({ phase, immediate, scheduled: sample() }));
    };
    window.fetch = (...args) => {
      const value = typeof args[0] === "string" ? args[0] : args[0]?.url;
      let trackedResolution = "";
      if (value === "/api/inventory") window.__session_switchInventoryFetches.push(new Error("inventory fetch").stack);
      if (value === "/api/attachment-handles" && window.__session_switchIgnoreHandleAbort && args[1]) {
        window.__session_switchIgnoredAborts.push(value);
        args[1] = { ...args[1], signal: undefined };
        trackedResolution = "handle";
      }
      if (value === "/api/inventory" && window.__session_switchIgnoreInventoryAbort && args[1]) {
        window.__session_switchIgnoredAborts.push(value);
        args[1] = { ...args[1], signal: undefined };
        trackedResolution = "inventory";
      }
      if (value === "/api/control-takeovers" && window.__session_switchIgnoreTakeoverAbort && args[1]) {
        args[1] = { ...args[1], signal: undefined };
      }
      const pending = original(...args);
      if (trackedResolution === "handle") return pending.then((response) => { window.__session_switchHandleResolutions += 1; return response; });
      if (trackedResolution === "inventory") return pending.then((response) => { window.__session_switchInventoryResolutions += 1; return response; });
      return pending;
    };
    HTMLTextAreaElement.prototype.focus = function (...args) {
      window.__session_switchTextareaFocuses.push(this.className);
      return originalFocus.apply(this, args);
    };
  };
  await page.addInitScript(installPageInstrumentation);
  const consoleErrors = [];
  page.on("console", (message) => { if (message.type() === "error") consoleErrors.push(`${message.text()} @ ${JSON.stringify(message.location())}`); });
  page.on("pageerror", (error) => consoleErrors.push(String(error)));
  const evidence = { engine: ENGINE, pointer: POINTER, timelines: [], measurements: {} };

  const terminalText = () => page.locator(".xterm-rows").innerText();
  // terminal interaction gives the tag and sheet two presentation entry points over the same
  // inventory loader. Count the authority request itself, not either caller's
  // stack name, so the one-request law is shared by both surfaces.
  const listFetches = () => page.evaluate(() => window.__session_switchInventoryFetches.length);
  const openSheet = async () => {
    const sheet = page.locator(".persea-unified-sheet");
    if (!await sheet.isVisible()) await page.getByRole("button", { name: "Quick actions", exact: true }).click();
  };
  const tile = (name) => page.locator(".persea-unified-sheet__tile").filter({ has: page.locator(".persea-unified-sheet__label", { hasText: name }) }).first();
  const openSessions = async () => {
    if (POINTER === "fine") await page.locator(".persea-unified-tag").click();
    else {
      await openSheet();
      await tile("Switch").click();
    }
    await page.locator(".persea-session-switcher:visible .persea-session-switcher__row").first().waitFor();
  };
  const waitSession = async (text) => {
    try {
      await page.waitForFunction((wanted) => document.querySelector(".xterm-rows")?.textContent?.includes(wanted), text, { timeout: 10_000 });
    } catch (error) {
      phase(`wait-failed ${JSON.stringify({ wanted: text, url: page.url(), errors: consoleErrors, terminal: await page.locator(".xterm-rows").innerText().catch(() => ""), body: (await page.locator("body").innerText().catch(() => "")).slice(0, 500), server: await snapshot() })}`);
      throw error;
    }
  };

  try {
    await control({ reset: true, switchSessions: true, sessionBState: "blocked_alt_screen", imageStaging: true, replayFocusReporting: true });
    const initialInventory = await requestJSON(`${fixture.origin}/api/inventory`);
    const initial = initialInventory.realms[0].servers[0].sessions[0];
    const url = `${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({
      handle: initial.handles.control, mode: "control", history: "1000", name: "alpha",
      draft_scope: fixture.draftScope, engine: "unified-dev",
    })}`;
    await page.goto(url, { waitUntil: "load" });
    await waitSession("fixture-live");
    // ------------------------------------------------------------- workspace access F4
    // Clipboard and Keys are task routes in the quick-actions sheet. Since U5
    // exactly ONE control opens it, on every pointer: the top-bar opener. The
    // floating puck it replaced is deleted, not hidden, so this also asserts
    // its absence from the document.
    {
      const opener = page.locator(".persea-unified-quick-actions");
      const puck = page.locator(".persea-unified-puck");
      const sheet = page.locator(".persea-unified-sheet");
      assert(await sheet.isVisible() === false, "the quick-actions sheet must start closed");
      if (POINTER === "fine") {
        assert(await opener.isVisible(), "a fine pointer has no visible quick-actions opener");
        assert(await puck.count() === 0, "the floating puck is still in the document");
        const shape = await opener.evaluate((node) => ({
          tag: node.tagName, type: node.getAttribute("type"), disabled: node.disabled,
          tabIndex: node.tabIndex, label: node.getAttribute("aria-label"),
          expanded: node.getAttribute("aria-expanded"),
        }));
        assert(shape.tag === "BUTTON" && shape.type === "button" && !shape.disabled && shape.tabIndex === 0 && shape.label === "Quick actions" && shape.expanded === "false",
          `the quick-actions opener is not a keyboard-reachable closed-state button: ${JSON.stringify(shape)}`);
        // Keyboard activation, not a mouse-only target.
        await opener.press("Enter");
        await sheet.waitFor({ state: "visible" });
        assert(await opener.getAttribute("aria-expanded") === "true", "the opener did not report the sheet open");
        assert(await opener.getAttribute("aria-controls") === await sheet.getAttribute("id"), "the opener does not control the sheet it opens");
        assert(await tile("Clipboard").isVisible(), "the sheet the opener opened has no visible Clipboard route");
        assert(await tile("Keys").isVisible() && await page.locator(".persea-unified-toolbar-paste").isVisible(), "the opened sheet is missing Keys or the primary Paste route");
        // The same control closes it: its own pointerdown must not be read as
        // an outside dismissal that the click would then immediately undo.
        await opener.click();
        await sheet.waitFor({ state: "hidden" });
        assert(await opener.getAttribute("aria-expanded") === "false", "the opener did not report the sheet closed");
      } else {
        // U5: the same opener, in the same place, on a coarse pointer.
        assert(await puck.count() === 0, "the floating puck is still in the document");
        assert(await opener.isVisible(), "a coarse pointer has no sheet opener in the control row");
        const box = await opener.boundingBox();
        assert(box && box.width >= 43.5 && box.height >= 43.5, `the coarse sheet opener is below 44px: ${JSON.stringify(box)}`);
        await opener.click();
        await sheet.waitFor({ state: "visible" });
        assert(await opener.getAttribute("aria-expanded") === "true", "the coarse opener did not report the sheet open");
        assert(await tile("Clipboard").isVisible(), "the sheet the coarse opener opened has no visible Clipboard route");
        await opener.click();
        await sheet.waitFor({ state: "hidden" });

        // U3 at 390pt, on the engine a phone actually runs. The row is the
        // only surface a phone has for naming the session, so the tag lives
        // here too: a dot at full size, a name that can ellipsise, no alias,
        // one line, and nothing outside the viewport.
        const identity = page.locator(".persea-unified-tag");
        assert(await identity.isVisible(), "a coarse pointer has no session tag in the control row");
        const tagShape = await identity.evaluate((node) => {
          const box = node.getBoundingClientRect();
          const dot = node.querySelector(".persea-unified-tag__dot");
          const name = node.querySelector(".persea-unified-tag__name");
          const alias = node.querySelector(".persea-unified-tag__alias");
          const nameStyle = name ? getComputedStyle(name) : null;
          const row = node.closest(".persea-unified-toolbar");
          const boxes = Array.from(row.querySelectorAll("button, .persea-unified-geometry"))
            .map((child) => child.getBoundingClientRect())
            .filter((r) => r.width > 0 && r.height > 0);
          return {
            width: box.width, height: box.height, lines: node.getClientRects().length,
            dot: dot ? dot.getBoundingClientRect().width : 0,
            dotState: dot ? dot.dataset.state : null,
            name: name ? name.textContent : null,
            aliasWidth: alias ? alias.getBoundingClientRect().width : 0,
            label: node.getAttribute("aria-label") || "",
            truncates: nameStyle ? (nameStyle.overflow !== "visible" && nameStyle.textOverflow === "ellipsis" && nameStyle.whiteSpace === "nowrap") : false,
            outside: boxes.filter((r) => r.left < -0.5 || r.right > window.innerWidth + 0.5).length,
            lowestTop: Math.max(...boxes.map((r) => r.top)),
            highestBottom: Math.min(...boxes.map((r) => r.bottom)),
            rowHeight: row.getBoundingClientRect().height,
          };
        });
        assert(tagShape.width >= 43.5 && tagShape.height >= 43.5, `the coarse session tag is below 44px: ${JSON.stringify(tagShape)}`);
        assert(tagShape.lines === 1, `the coarse session tag wrapped: ${JSON.stringify(tagShape)}`);
        assert(tagShape.dot >= 4 && tagShape.dotState !== null, `the coarse session tag lost its status dot: ${JSON.stringify(tagShape)}`);
        assert(tagShape.name === "alpha", `the coarse session tag does not name the session: ${JSON.stringify(tagShape)}`);
        assert(tagShape.aliasWidth === 0, `the alias claims row width at 390pt: ${JSON.stringify(tagShape)}`);
        assert(tagShape.truncates, `the coarse session tag's name cannot truncate: ${JSON.stringify(tagShape)}`);
        assert(tagShape.outside === 0, `a control leaves the 390pt viewport: ${JSON.stringify(tagShape)}`);
        assert(tagShape.lowestTop < tagShape.highestBottom, `the coarse control row wrapped to a second line: ${JSON.stringify(tagShape)}`);
        evidence.measurements.coarseSessionTag = tagShape;
      }
      phase("quick-actions-opener");
    }

    await page.getByRole("button", { name: "Open composer" }).first().click();
    await page.locator(".attachment-page__composer-textarea").fill("alpha draft");
    if (POINTER === "coarse") {
      const composer = page.locator(".attachment-page__composer-textarea");
      const stillComposer = () => page.evaluate(() => document.activeElement?.classList.contains("attachment-page__composer-textarea") === true);
      assert(await stillComposer(), "coarse keyboard fixture did not start focused");
      await page.locator(".persea-unified-tag").click();
      await page.locator(".persea-unified-identity__details").waitFor({ state: "visible" });
      assert(await stillComposer(), "tag opener closed the software-keyboard focus owner");
      await page.getByRole("button", { name: "Refresh sessions" }).click();
      assert(await stillComposer(), "tag Refresh closed the software-keyboard focus owner");
      assert(await page.getByRole("button", { name: "Dashboard" }).count() === 1,
        "tag popover does not expose its bounded Dashboard escape");
      assert(await page.getByRole("button", { name: /Prev|Next/ }).count() === 0,
        "tag popover still exposes arbitrary Prev/Next cycling");
      assert(await stillComposer(), "tag navigation census closed the software-keyboard focus owner");
      await page.locator(".persea-unified-tag").click();
      assert(await stillComposer(), "tag closer closed the software-keyboard focus owner");
      await composer.evaluate((node) => node.blur());
      assert(!await stillComposer(), "coarse keyboard-closed fixture did not release focus");
      await page.locator(".persea-unified-tag").click();
      await page.getByRole("button", { name: "Refresh sessions" }).click();
      assert(!await stillComposer() && !await page.locator(".xterm-helper-textarea:focus").count(), "tag controls reopened a closed software keyboard");
      await page.locator(".persea-unified-tag").click();
      await composer.focus();
      evidence.timelines.push({ phase: "coarse-focus-neutral-tag-controls", keyboardOpen: true, keyboardClosed: true });
    }
    const listFetchesBeforeSwitcher = await listFetches();

    // Blocked rows are visible but inert; opening consumes exactly one shared
    // inventory request, and the current exact identity is marked.
    await openSessions();
    const alpha = page.getByRole("button", { name: "Current session alpha" });
    const beta = page.getByRole("button", { name: "Switch to beta" });
    assert(await alpha.isDisabled(), "current alpha row must be inert");
    assert(await beta.isDisabled(), "blocked beta row must be inert");
    assert((await beta.innerText()).includes("full-screen app"), "blocked beta reason is not visible");
    phase("blocked-row");
    let server = await snapshot();
    assert(await listFetches() === listFetchesBeforeSwitcher + 1, "list open must add exactly one inventory GET");

    // Manual refresh is exactly one more GET. A long adoption leaves A fully
    // live; a second tap cannot spend a second adoption or capability.
    await control({ sessionBState: "adoptable", holdAdoptionMs: 1500 });
    await page.getByRole("button", { name: "Refresh sessions" }).click();
    await page.waitForFunction(() => !document.querySelector('.persea-session-switcher__row[aria-label="Switch to beta"]')?.hasAttribute("disabled"));
    server = await snapshot();
    assert(await listFetches() === listFetchesBeforeSwitcher + 2, "manual refresh must add exactly one inventory GET");
    phase("adoptable-refresh");

    const betaButton = page.getByRole("button", { name: "Switch to beta" });
    await betaButton.click();
    await betaButton.click({ force: true });
    phase("adoption-started");
    await control({ writeLive: "A_DURING_ADOPTION\r\n" });
    await page.locator(".xterm-helper-textarea").pressSequentially("a-before");
    await delay(80);
    assert((await terminalText()).includes("A_DURING_ADOPTION"), "A stopped receiving output before the switch commit");
    const focusCallsBeforeCommit = await page.evaluate(() => window.__session_switchTextareaFocuses.length);
    phase("precommit-a-live");
    evidence.timelines.push({ phase: "adoption-pending", href: page.url(), text: await terminalText() });

    await waitSession("beta-replay");
    await page.waitForFunction((scope) => location.hash.includes(encodeURIComponent(scope)), fixture.draftScopeB);
    const commitSamples = await page.evaluate(() => window.__session_switchCommitSamples);
    const commitFinal = commitSamples.find((entry) => entry.phase === "endpoint_replaced")?.immediate;
    assert(commitFinal, `switch commit never reached endpoint replacement: ${JSON.stringify(commitSamples)}`);
    const scheduledStates = commitSamples.map((entry) => entry.scheduled);
    assert(scheduledStates.length === 6, `switch commit exposed ${scheduledStates.length} scheduling edges`);
    assert(scheduledStates.every((state) => JSON.stringify(state) === JSON.stringify(commitFinal)), `mixed A/B state escaped a scheduling edge: ${JSON.stringify(commitSamples)}`);
    assert(commitFinal.identity === fixture.draftScopeB
      && commitFinal.urlIdentity === fixture.draftScopeB
      && commitFinal.transcriptOwner === "beta"
      && commitFinal.draftScope === fixture.draftScopeB
      && commitFinal.imageRealm === "remote"
      && commitFinal.controlOfferIdentity === fixture.draftScopeB
      && commitFinal.admission.prepared === false
      && commitFinal.admission.committed === false
      && commitFinal.admission.controlGranted === false,
    `commit bound-state witness was not exact B: ${JSON.stringify(commitFinal)}`);
    assert(!(await terminalText()).includes("fixture-replay"), "A transcript survived B PREPARE");
    assert(await page.locator(".attachment-page__composer-textarea").inputValue() === "", "A draft crossed into B scope");
    assert((await page.locator(".persea-unified-geometry:not(.persea-unified-geometry--phone)").innerText()) === "100×30", "B committed geometry did not replace A geometry");
    assert(new URLSearchParams(page.url().split("#")[1]).get("image_realm") === "remote", "B image staging authority was not rebound");
    const switchFocusCalls = (await page.evaluate(() => window.__session_switchTextareaFocuses.length)) - focusCallsBeforeCommit;
    assert(switchFocusCalls === (POINTER === "fine" ? 1 : 0), `${POINTER} switch made ${switchFocusCalls} textarea focus calls`);
    await page.locator(".attachment-page__composer-textarea").fill("beta draft");
    await page.locator(".xterm-helper-textarea").pressSequentially("b-after");
    server = await snapshot();
    assert(server.counters.adoptions === 1, `pending double-tap spent ${server.counters.adoptions} adoptions`);
    const aAttachment = server.attachments.find((entry) => entry.session === "A");
    const bAttachment = server.attachments.find((entry) => entry.session === "B" && entry.live);
    assert(aAttachment?.closeReason === "session_switch", `A close reason ${aAttachment?.closeReason}`);
    assert(bAttachment, "B did not own one live socket");
    assert(bAttachment.frames.indexOf("MODE_REQUEST") < bAttachment.frames.findIndex((frame) => frame.startsWith("INPUT:")), "B INPUT preceded MODE_REQUEST");
    assert(server.attachments.every((entry) => entry.resizes === 0), "session switching emitted RESIZE_REQUEST");
    const rememberedB = JSON.parse(await page.evaluate(() => localStorage.getItem("persea-terminal.last-session.v1")));
    assert(rememberedB.draftScope === fixture.draftScopeB, "last-session memory was not written after B COMMIT");
    phase("b-committed");

    // Switch back from one freshly opened list. The old draft is restored only
    // in A's scope and one xterm renderer remains.
    const listFetchesBeforeReopen = await listFetches();
    await openSessions();
    const listFetchesAfterReopen = await listFetches();
    assert(listFetchesAfterReopen === listFetchesBeforeReopen + 1, `reopened list must add exactly one inventory GET: before=${listFetchesBeforeReopen} after=${listFetchesAfterReopen}`);
    await page.getByRole("button", { name: "Switch to alpha" }).click();
    await waitSession("fixture-replay");
    assert(await page.locator(".attachment-page__composer-textarea").inputValue() === "alpha draft", "A draft did not restore after A→B→A");
    assert((await page.locator(".persea-unified-geometry:not(.persea-unified-geometry--phone)").innerText()) === "80×24", "A geometry did not restore after A→B→A");
    assert(new URLSearchParams(page.url().split("#")[1]).get("image_realm") === "local", "A image staging authority did not restore");
    assert(await page.locator(".xterm-screen").count() === 1, "switch created a second renderer");
    server = await snapshot();
    assert(server.attachments.filter((entry) => entry.live).length === 1, "switch left more than one live attachment");
    assert(server.attachments.every((entry) => entry.resizes === 0), "A→B→A emitted resize traffic");
    phase("a-returned");

    // Touch targets and scroll gesture honesty on the real coarse surface.
    await openSessions();
    const beforeScroll = await snapshot();
    await page.locator(".persea-session-switcher:visible .persea-session-switcher__list").evaluate((list) => {
      const row = list.querySelector(".persea-session-switcher__row:not([disabled])");
      if (!(row instanceof HTMLElement)) throw new Error("switch row unavailable");
      const rect = row.getBoundingClientRect();
      const init = { bubbles: true, cancelable: true, pointerId: 1, pointerType: "touch", isPrimary: true, clientX: rect.left + 10, clientY: rect.top + 10 };
      row.dispatchEvent(new PointerEvent("pointerdown", init));
      row.dispatchEvent(new PointerEvent("pointermove", { ...init, clientY: rect.top + 70 }));
      row.dispatchEvent(new PointerEvent("pointerup", { ...init, clientY: rect.top + 70 }));
    });
    await delay(80);
    const afterScroll = await snapshot();
    assert(afterScroll.counters.websockets === beforeScroll.counters.websockets, "list scroll gesture activated a session row");
    const boxes = await page.locator(".persea-unified-sheet__tile, .persea-session-switcher__row, .persea-session-switcher__search, .persea-session-switcher__refresh").evaluateAll((nodes) => nodes.filter((node) => {
      const style = getComputedStyle(node); return style.display !== "none" && node.getClientRects().length > 0;
    }).map((node) => { const r = node.getBoundingClientRect(); return { label: node.getAttribute("aria-label") || node.textContent, width: r.width, height: r.height }; }));
    assert(boxes.every((box) => box.width >= 43.5 && box.height >= 43.5), `undersized targets: ${JSON.stringify(boxes.filter((box) => box.width < 43.5 || box.height < 43.5))}`);
    evidence.measurements = { targets: boxes, inventoryGets: server.counters.inventory, attachments: server.attachments.length };

    const styleState = await page.locator("style").evaluateAll((styles) => styles.map((style) => ({
      nonce: style.getAttribute("nonce"),
      nonceProperty: style.nonce,
      rules: style.sheet?.cssRules?.length ?? -1,
      textLength: style.textContent?.length ?? 0,
    })));
    // Screenshot capture is outside the product observation interval. On
    // Playwright WebKit its utility world deliberately inserts one unnonced
    // synchronizer stylesheet (browser instrumentation); real Safari never runs that helper.
    assert(consoleErrors.length === 0, `browser errors: ${JSON.stringify(consoleErrors)} styles=${JSON.stringify(styleState)}`);
    assert(styleState.length === 3 && styleState.every((style) => style.nonceProperty && style.rules > 0), `xterm styles were not all nonced/applied: ${JSON.stringify(styleState)}`);
    if (EVIDENCE) {
      fs.mkdirSync(EVIDENCE, { recursive: true });
      const screenshotErrorStart = consoleErrors.length;
      await page.screenshot({ path: path.join(EVIDENCE, `session-switch-${ENGINE}-${POINTER}.png`), fullPage: true });
      await delay(50);
      const screenshotErrors = consoleErrors.slice(screenshotErrorStart);
      if (ENGINE === "webkit") assert(screenshotErrors.length === 1 && /Refused to apply a stylesheet/.test(screenshotErrors[0]), `unexpected WebKit screenshot attribution: ${JSON.stringify(screenshotErrors)}`);
      else assert(screenshotErrors.length === 0, `Chromium screenshot emitted errors: ${JSON.stringify(screenshotErrors)}`);
      evidence.measurements.styleState = styleState;
      evidence.measurements.screenshotInstrumentation = screenshotErrors;
      fs.writeFileSync(path.join(EVIDENCE, `session-switch-${ENGINE}-${POINTER}.json`), `${JSON.stringify({ ...evidence, server }, null, 2)}\n`);
    }

    // F3/F4: a rotation-triggered A re-mint may resolve on either side of the
    // B adoption. The switch commit invalidates it in both schedules; only B
    // remains live and the stale A work never overwrites the committed URL.
    await context.close();
    const openScenario = async (knobs) => {
      await control({ reset: true, switchSessions: true, ...knobs });
      const inventory = await requestJSON(`${fixture.origin}/api/inventory`);
      const initialSession = inventory.realms[0].servers[0].sessions[0];
      const scenarioContext = await browser.newContext(contextOptions);
      const scenarioPage = await scenarioContext.newPage();
      await scenarioPage.addInitScript(installPageInstrumentation);
      const scenarioErrors = [];
      scenarioPage.on("console", (message) => { if (message.type() === "error") scenarioErrors.push(message.text()); });
      scenarioPage.on("pageerror", (error) => scenarioErrors.push(String(error)));
      await scenarioPage.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({
        handle: initialSession.handles.control, mode: "control", history: "1000", name: "alpha",
        draft_scope: fixture.draftScope, engine: "unified-dev",
      })}`, { waitUntil: "load" });
      await scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"), null, { timeout: 10_000 });
      const openList = async (target = "beta") => {
        if (POINTER === "fine") await scenarioPage.locator(".persea-unified-tag").click();
        else {
          await scenarioPage.getByRole("button", { name: "Quick actions", exact: true }).click();
          await scenarioPage.locator(".persea-unified-sheet__tile").filter({ has: scenarioPage.locator(".persea-unified-sheet__label", { hasText: "Switch" }) }).first().click();
        }
        await scenarioPage.locator(".persea-session-switcher:visible").getByRole("button", { name: `Switch to ${target}` }).waitFor();
      };
      return { scenarioContext, scenarioPage, scenarioErrors, openList };
    };

    // F2 pre-commit: the inventory row was adoptable, but eligibility changed
    // before the authoritative POST. The typed refusal leaves A byte-for-byte
    // live and consumes no B handle or socket.
    const refused = await openScenario({ sessionBState: "adoptable" });
    await refused.openList();
    const refusalBefore = await snapshot();
    await control({ sessionBState: "blocked_alt_screen" });
    await refused.scenarioPage.getByRole("button", { name: "Switch to beta" }).click();
    await refused.scenarioPage.waitForFunction(() => document.querySelector(".persea-unified-sheet__status")?.textContent?.includes("full-screen"), null, { timeout: 5_000 });
    const refusalAfter = await snapshot();
    assert(refusalAfter.counters.websockets === refusalBefore.counters.websockets, "pre-commit refusal opened a B socket");
    assert(refusalAfter.attachments.length === refusalBefore.attachments.length && refusalAfter.attachments[0].id === refusalBefore.attachments[0].id && refusalAfter.attachments[0].live, "pre-commit refusal changed A attachment");
    assert(refused.scenarioPage.url().includes(encodeURIComponent(fixture.draftScope)), "pre-commit refusal changed A URL identity");
    const refusalNetworkErrors = refused.scenarioErrors.filter((value) => value === "Failed to load resource: the server responded with a status of 409 (Conflict)");
    assert(refusalNetworkErrors.length === 1, `pre-commit refusal did not expose exactly one typed HTTP 409: ${JSON.stringify(refused.scenarioErrors)}`);
    assert(refused.scenarioErrors.length === refusalNetworkErrors.length, `pre-commit refusal browser errors ${JSON.stringify(refused.scenarioErrors)}`);
    evidence.timelines.push({ phase: "precommit-refusal", server: refusalAfter });
    await refused.scenarioContext.close();

    for (const timing of [
      { name: "b-before-stale-a", holdAdoptionMs: 180, holdHandleMs: 450, expectAReopen: false },
      { name: "stale-a-before-b", holdAdoptionMs: 450, holdHandleMs: 0, expectAReopen: true },
    ]) {
      const scenario = await openScenario({ sessionBState: "adoptable", holdAdoptionMs: timing.holdAdoptionMs, holdHandleMs: timing.holdHandleMs });
      await scenario.openList();
      await scenario.scenarioPage.getByRole("button", { name: "Switch to beta" }).click();
      await delay(35);
      await control({ closeSession: { session: "A", reason: "generation_rotated" } });
      await scenario.scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("beta-replay"), null, { timeout: 10_000 });
      await delay(550);
      const raced = await snapshot();
      assert(raced.attachments.filter((entry) => entry.session === "B" && entry.live).length === 1, `${timing.name}: B did not own exactly one live socket`);
      assert(raced.attachments.filter((entry) => entry.session === "A" && entry.live).length === 0, `${timing.name}: stale A work remained live`);
      assert((raced.attachments.filter((entry) => entry.session === "A").length >= 2) === timing.expectAReopen, `${timing.name}: wrong stale-A completion schedule`);
      assert(scenario.scenarioPage.url().includes(encodeURIComponent(fixture.draftScopeB)), `${timing.name}: stale A overwrote B URL identity`);
      assert(scenario.scenarioErrors.length === 0, `${timing.name}: browser errors ${JSON.stringify(scenario.scenarioErrors)}`);
      evidence.timelines.push({ phase: timing.name, server: raced });
      await scenario.scenarioContext.close();
    }

    // C1: a reload-handle mint begun by A's PREPARE must remain owned by A's
    // controller operation even when its fetch ignores abort and resolves only
    // after B commits. The stored capability and an actual reload both stay B.
    const reloadAuthority = await openScenario({ sessionBState: "adoptable", holdAdoptionMs: 180 });
    await reloadAuthority.openList();
    await reloadAuthority.scenarioPage.evaluate(() => {
      window.__session_switchIgnoreHandleAbort = true;
      window.__session_switchIgnoreInventoryAbort = true;
    });
    const reloadBefore = await snapshot();
    await control({ holdHandleMs: 5_000, reprepareSession: "A" });
    const reloadMintStarted = await waitSnapshot((value) => value.counters.handleRequests > reloadBefore.counters.handleRequests, 5_000);
    assert(reloadMintStarted, "reload-authority: A reload mint never reached its held request edge");
    await reloadAuthority.scenarioPage.waitForFunction(() => document.querySelector(".persea-unified-identity__details")?.hidden
      && document.querySelector(".persea-unified-sheet")?.hidden, undefined, { timeout: 2_000 });
    const reloadBeta = reloadAuthority.scenarioPage.getByRole("button", { name: "Switch to beta" });
    // A reprepare is a presentation replacement and therefore owns closing
    // every pane-local disclosure. Reopen the one switch surface explicitly;
    // do not rely on the pre-reprepare sheet remaining above the new surface.
    await reloadAuthority.openList();
    await reloadBeta.click();
    await reloadAuthority.scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("beta-replay"), null, { timeout: 10_000 });
    await delay(2_000);
    const reloadSettled = await snapshot();
    const reloadHandle = await reloadAuthority.scenarioPage.evaluate(() => sessionStorage.getItem("persea-unified-terminal-reload-handle-v1"));
    const reloadRecord = reloadSettled.handles.find((entry) => entry.handle === reloadHandle);
    assert(reloadSettled.handleLedger.some((entry) => entry.session === "A") && reloadRecord?.session === "B",
      `reload-authority: B's reload capability was replaced: ${JSON.stringify({ reloadHandle, reloadRecord, ledger: reloadSettled.handleLedger })}`);
    const reloadAttachmentCount = reloadSettled.attachments.length;
    await reloadAuthority.scenarioPage.reload({ waitUntil: "load" });
    await reloadAuthority.scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("beta-replay"), null, { timeout: 10_000 });
    const reloaded = await waitSnapshot((value) => value.attachments.slice(reloadAttachmentCount).some((entry) => entry.session === "B" && entry.live), 10_000);
    assert(reloaded, "reload-authority: reload did not consume B's surviving capability");
    const reloadAttachments = reloaded.attachments.slice(reloadAttachmentCount);
    assert(reloadAttachments.length === 1 && reloadAttachments[0].session === "B", `reload-authority: reload resurrected stale A authority: ${JSON.stringify(reloadAttachments)}`);
    evidence.timelines.push({ phase: "reload-authority", reloadHandle, reloadRecord, server: reloaded });
    await reloadAuthority.scenarioContext.close();

    // Source-remint and identity-remint races. In each case A work begins
    // while adoption keeps A live, settles only after B is authoritative, and
    // B then enters lease_held. The exact B offer must be the only authority
    // claimed; the stale A capability remains unconsumed.
    for (const race of [
      { name: "source-remint-authority", holdAdoptionMs: 1_800, holdHandleMs: 2_200, holdInventoryMs: 0, holdInventoryAfter: 0, bindingsExpired: false, holdLeaseRefusalMs: 8_000 },
      { name: "identity-remint-authority", holdAdoptionMs: 5_000, holdHandleMs: 0, holdInventoryMs: 8_000, holdInventoryAfter: 1, bindingsExpired: true, holdLeaseRefusalMs: 12_000 },
    ]) {
      const scenario = await openScenario({ sessionBState: "adoptable", holdAdoptionMs: race.holdAdoptionMs });
      await scenario.openList();
      await scenario.scenarioPage.evaluate((kind) => {
        window.__session_switchIgnoreHandleAbort = kind === "source-remint-authority";
        window.__session_switchIgnoreInventoryAbort = kind === "identity-remint-authority";
      }, race.name);
      const authorityBefore = await snapshot();
      await control({
        holdLeaseB: true,
        holdHandleMs: race.holdHandleMs,
        holdInventoryMs: race.holdInventoryMs,
        holdInventoryAfter: race.holdInventoryAfter,
        holdLeaseRefusalMs: race.holdLeaseRefusalMs,
        bindingsExpired: race.bindingsExpired,
      });
      await scenario.scenarioPage.getByRole("button", { name: "Switch to beta" }).click();
      await delay(35);
      await control({ closeSession: { session: "A", reason: "generation_rotated" } });
      const authorityKind = race.name === "source-remint-authority" ? "source_remint" : "identity_remint";
      try {
        await scenario.scenarioPage.waitForFunction((kind) => window.__session_switchAuthorityResults.some((event) => event.kind === kind
          && (event.stage === "discarded" || event.stage === "recorded")), authorityKind, { timeout: 15_000 });
      } catch (error) {
        throw new Error(`${race.name}: authority callback edge missing: ${JSON.stringify({ ignored: await scenario.scenarioPage.evaluate(() => window.__session_switchIgnoredAborts), authority: await scenario.scenarioPage.evaluate(() => window.__session_switchAuthorityResults), server: await snapshot() })}`, { cause: error });
      }
      const authorityResults = await scenario.scenarioPage.evaluate(() => window.__session_switchAuthorityResults);
      const staleAuthorityResults = authorityResults.filter((event) => event.kind === authorityKind);
      assert(staleAuthorityResults.some((event) => event.stage === "resolved" && event.operation !== event.currentOperation && event.identity === fixture.draftScope && event.currentIdentity === fixture.draftScopeB)
        && staleAuthorityResults.some((event) => event.stage === "discarded")
        && !staleAuthorityResults.some((event) => event.stage === "recorded"),
      `${race.name}: stale callback was not discarded at its own guard: ${JSON.stringify(staleAuthorityResults)}`);
      const authorityAtStaleSettlement = await scenario.scenarioPage.evaluate(() => window.__session_switchSwitchSample());
      assert(authorityAtStaleSettlement.identity === fixture.draftScopeB && authorityAtStaleSettlement.controlOfferIdentity === fixture.draftScopeB,
        `${race.name}: stale callback replaced B offer authority: ${JSON.stringify(authorityAtStaleSettlement)}`);
      let raced = await waitSnapshot((value) => value.attachments.some((entry) => entry.session === "B" && entry.takeover && entry.live), 15_000);
      assert(raced, `${race.name}: B never completed its exact-offer takeover`);
      await scenario.scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("beta-replay"), null, { timeout: 15_000 });
      raced = await waitSnapshot((value) => race.holdHandleMs > 0
        ? value.handleLedger.slice(authorityBefore.handleLedger.length).some((entry) => entry.session === "A")
        : value.inventoryLedger.slice(authorityBefore.inventoryLedger.length).some((entry) => entry.session === "A"), 8_000);
      assert(raced, `${race.name}: stale callback never reached its held completion edge; ignored=${JSON.stringify(await scenario.scenarioPage.evaluate(() => window.__session_switchIgnoredAborts))}`);
      const refusedB = raced.attachments.find((entry) => entry.session === "B" && entry.handlePurpose === "control" && entry.closeReason === "lease_held");
      const claimed = raced.takeoverClaims.at(-1);
      assert(refusedB?.offeredHandle, `${race.name}: B losing handle was not recorded`);
      assert(claimed?.kind === "offer" && claimed.value === refusedB.offeredHandle, `${race.name}: takeover did not consume B's exact offer: ${JSON.stringify({ refusedB, claimed })}`);
      const staleHandle = race.holdHandleMs > 0
        ? raced.handleLedger.slice(authorityBefore.handleLedger.length).find((entry) => entry.session === "A")?.handle
        : raced.inventoryLedger.slice(authorityBefore.inventoryLedger.length).find((entry) => entry.session === "A")?.control;
      assert(staleHandle, `${race.name}: stale A capability was not minted by the exercised callback: ${JSON.stringify({ before: authorityBefore.counters, after: raced.counters, handleLedger: raced.handleLedger, inventoryLedger: raced.inventoryLedger, attachments: raced.attachments })}`);
      const staleRecord = raced.handles.find((entry) => entry.handle === staleHandle);
      assert(staleRecord && !staleRecord.consumed, `${race.name}: stale A capability was consumed: ${JSON.stringify(staleRecord)}`);
      assert(!raced.takeoverClaims.some((entry) => entry.value === staleHandle || entry.value === fixture.source), `${race.name}: stale A authority reached takeover: ${JSON.stringify(raced.takeoverClaims)}`);
      assert(raced.attachments.filter((entry) => entry.session === "B" && entry.takeover).length === 1, `${race.name}: B takeover socket count drifted`);
      const expectedSourceExpiry = race.bindingsExpired
        ? scenario.scenarioErrors.filter((value) => value === "Failed to load resource: the server responded with a status of 410 (Gone)")
        : [];
      assert(expectedSourceExpiry.length === (race.bindingsExpired ? 2 : 0)
        && scenario.scenarioErrors.length === expectedSourceExpiry.length,
      `${race.name}: browser errors ${JSON.stringify(scenario.scenarioErrors)}`);
      evidence.timelines.push({ phase: race.name, staleHandle, claimed, authorityAtStaleSettlement, server: raced });
      await scenario.scenarioContext.close();
    }

    // Takeover callback race. Ignore AbortSignal only at the fixture edge so
    // the already-issued B promise really resolves after A becomes current.
    // The controller token must retain A's current offer and the stale B
    // takeover handle must remain unconsumed.
    const staleTakeover = await openScenario({ sessionBState: "open" });
    await staleTakeover.openList();
    await staleTakeover.scenarioPage.evaluate(() => { window.__session_switchIgnoreTakeoverAbort = true; });
    await control({ holdLeaseB: true, holdTakeoverMs: 650 });
    await staleTakeover.scenarioPage.getByRole("button", { name: "Switch to beta" }).click();
    const takeoverIssued = await waitSnapshot((value) => value.counters.takeovers === 1 && value.handles.some((entry) => entry.purpose === "control-takeover" && entry.session === "B"), 8_000);
    assert(takeoverIssued, "takeover race never issued B's held promise");
    await staleTakeover.openList("alpha");
    await control({ holdPrepareMs: 1_200 });
    await staleTakeover.scenarioPage.getByRole("button", { name: "Switch to alpha" }).click();
    await staleTakeover.scenarioPage.waitForFunction((scope) => location.hash.includes(encodeURIComponent(scope)), fixture.draftScope, { timeout: 5_000 });
    await staleTakeover.scenarioPage.waitForFunction(() => window.__session_switchAuthorityResults.some((event) => event.kind === "takeover"
      && (event.stage === "discarded" || event.stage === "recorded") && event.operation !== event.currentOperation), null, { timeout: 5_000 });
    const takeoverAuthorityResults = await staleTakeover.scenarioPage.evaluate(() => window.__session_switchAuthorityResults.filter((event) => event.kind === "takeover" && event.operation !== event.currentOperation));
    assert(takeoverAuthorityResults.some((event) => event.stage === "resolved")
      && takeoverAuthorityResults.some((event) => event.stage === "discarded")
      && !takeoverAuthorityResults.some((event) => event.stage === "recorded"),
    `stale takeover callback was not discarded at its own guard: ${JSON.stringify(takeoverAuthorityResults)}`);
    const currentAuthority = await staleTakeover.scenarioPage.evaluate(() => window.__session_switchSwitchSample());
    assert(currentAuthority.identity === fixture.draftScope && currentAuthority.controlOfferIdentity === fixture.draftScope,
      `stale B takeover mutated A authority: ${JSON.stringify(currentAuthority)}`);
    const takeoverSettled = await snapshot();
    const staleTakeoverHandle = takeoverSettled.handles.find((entry) => entry.purpose === "control-takeover" && entry.session === "B");
    assert(staleTakeoverHandle && !staleTakeoverHandle.consumed, `stale B takeover handle was consumed: ${JSON.stringify(staleTakeoverHandle)}`);
    assert(takeoverSettled.attachments.filter((entry) => entry.session === "B" && entry.takeover).length === 0, "stale B takeover installed a socket");
    await staleTakeover.scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-replay"), null, { timeout: 10_000 });
    const takeoverFinal = await snapshot();
    assert(takeoverFinal.attachments.filter((entry) => entry.session === "A" && entry.live).length === 1
      && takeoverFinal.attachments.filter((entry) => entry.session === "B" && entry.live && entry.handlePurpose !== "foreign-control").length === 0,
      `takeover race ended on stale authority: ${JSON.stringify(takeoverFinal.attachments)}`);
    assert(staleTakeover.scenarioErrors.length === 0, `takeover race browser errors ${JSON.stringify(staleTakeover.scenarioErrors)}`);
    evidence.timelines.push({ phase: "takeover-authority", currentAuthority, server: takeoverFinal });
    await staleTakeover.scenarioContext.close();
    if (AUTHORITY_RACES_ONLY) {
      process.stdout.write(`run_session_switch_browser authority-races (${ENGINE}, ${POINTER}): PASS\n`);
      return;
    }

    // F2: the identity crosses to B before its socket can open. Exhaustion is
    // finite and visible, never reconnects A, and cannot write B memory because
    // no B COMMIT occurred.
    const failed = await openScenario({ sessionBState: "open", failSessionBConnections: 100 });
    await failed.openList();
    await failed.scenarioPage.getByRole("button", { name: "Switch to beta" }).click();
    await failed.scenarioPage.waitForFunction((scope) => location.hash.includes(encodeURIComponent(scope)), fixture.draftScopeB, { timeout: 5_000 });
    try {
      await failed.scenarioPage.locator(".persea-unified-notice").waitFor({ state: "visible", timeout: 55_000 });
    } catch (error) {
      phase(`b-exhaustion-wait ${JSON.stringify({ status: await failed.scenarioPage.locator(".persea-unified-connection").innerText(), server: await snapshot() })}`);
      throw error;
    }
    const failedServer = await snapshot();
    assert(failedServer.attachments.filter((entry) => entry.session === "A" && entry.live).length === 0, "post-commit B failure resurrected A");
    assert(failedServer.counters.websockets === 8, `B must spend its initial socket plus the existing six-attempt retry budget: ${failedServer.counters.websockets}`);
    const failedMemoryRaw = await failed.scenarioPage.evaluate(() => localStorage.getItem("persea-terminal.last-session.v1"));
    const failedMemory = failedMemoryRaw === null ? null : JSON.parse(failedMemoryRaw);
    assert(failedMemory === null || failedMemory.draftScope === fixture.draftScope, "B memory was written before a B COMMIT");
    assert(failed.scenarioErrors.filter((value) => !value.startsWith("WebSocket connection to")).length === 0, `post-commit failure emitted unexpected errors: ${JSON.stringify(failed.scenarioErrors)}`);
    evidence.timelines.push({ phase: "postcommit-b-exhausted", server: failedServer });
    await failed.scenarioContext.close();

    // F5: a foreign B Control lease forces exactly one takeover. Switching
    // straight back to A leaves one A socket, no B authority, and no
    // lease_held/takeover loop while the displaced B holder settles.
    const leased = await openScenario({ sessionBState: "open", holdLeaseB: true });
    await leased.openList();
    await leased.scenarioPage.getByRole("button", { name: "Switch to beta" }).click();
    await leased.scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("beta-replay"), null, { timeout: 10_000 });
    await leased.openList("alpha");
    await leased.scenarioPage.getByRole("button", { name: "Switch to alpha" }).click();
    await leased.scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-replay"), null, { timeout: 10_000 });
    await delay(250);
    const leaseRace = await snapshot();
    const bAttachments = leaseRace.attachments.filter((entry) => entry.session === "B");
    assert(leaseRace.counters.takeovers === 1, `deferred lease race issued ${leaseRace.counters.takeovers} takeovers`);
    assert(bAttachments.filter((entry) => entry.closeReason === "lease_held").length === 1, "B lease refusal was not singular");
    assert(bAttachments.filter((entry) => entry.takeover).length === 1, "B did not consume exactly one takeover socket");
    assert(leaseRace.attachments.filter((entry) => entry.session === "A" && entry.live).length === 1, "A did not end with exactly one live socket");
    assert(leaseRace.attachments.filter((entry) => entry.session === "B" && entry.live).length === 0, "B authority leaked after switching back");
    assert(leased.scenarioPage.url().includes(encodeURIComponent(fixture.draftScope)), "lease race ended on the wrong A identity");
    assert(leased.scenarioErrors.length === 0, `lease race browser errors ${JSON.stringify(leased.scenarioErrors)}`);
    evidence.timelines.push({ phase: "lease-a-b-a", server: leaseRace });
    await leased.scenarioContext.close();

    // ------------------------------------------------------------- workspace access F1
    // Terminal topbar behavior on this engine's real bundle at this pointer medium.
    // With nothing stored on WebKit/coarse, the
    // fit floors at 9px on a 390pt phone, and the old Zoom stepped the stored
    // 14 to 15 — six pixels away from what the operator could see, and gone
    // again after a reload. Three legs, one behaviour: auto when nothing is
    // stored; a step of one pixel from the RENDERED size, stored as explicit
    // and surviving a reload; and Fit restoring auto AND storing auto, which
    // is the only way back from a zoom.
    {
      const f1 = await openScenario({});
      const fontState = () => f1.scenarioPage.evaluate(() => ({
        rendered: parseFloat(getComputedStyle(document.querySelector(".xterm-rows")).fontSize),
        preference: document.querySelector(".persea-unified-terminal").dataset.fontBaseline,
      }));
      const viewControl = (name) => f1.scenarioPage.locator(".persea-unified-view-popover")
        .getByRole("button", { name, exact: true }).first();
      const openF1View = async () => {
        const view = f1.scenarioPage.locator(".persea-unified-view-popover");
        if (!await view.isVisible()) await f1.scenarioPage.locator(".persea-unified-view-disclosure").click();
        await view.waitFor({ state: "visible" });
      };
      const reload = async () => {
        const inventory = await requestJSON(`${fixture.origin}/api/inventory`);
        const session = inventory.realms[0].servers[0].sessions[0];
        await f1.scenarioPage.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({
          handle: session.handles.control, mode: "control", history: "1000", name: "alpha",
          draft_scope: fixture.draftScope, engine: "unified-dev",
        })}`, { waitUntil: "load" });
        await f1.scenarioPage.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"), null, { timeout: 10_000 });
        await delay(400);
      };
      // The View popover is opened before the baseline reading: its own box change is
      // a resize, and in auto mode a resize refits.
      await openF1View();
      await delay(300);
      const autoState = await fontState();
      const autoServer = await snapshot();
      const clamp = (value) => Math.max(9, Math.min(24, value));
      const fitted = Math.round(autoState.rendered);
      // Which way the one-pixel step goes. The law under test is the same in
      // both directions: one tile press moves the RENDERED size by exactly one
      // pixel, stores it as an explicit preference, writes exactly one PUT, and
      // survives a reload. Deriving the direction from the fitted size is what
      // keeps the case runnable on a host whose auto-fit lands on a clamp --
      // at 1280x800 this fixture's session fits at 24px, the ceiling, on this
      // machine's font stack, and a fixed "Zoom in" then had nothing to step
      // to. That is not new: 436f99e fails this same precondition here, and
      // the receipt is in the terminal topbar evidence root under base-controls/.
      const direction = fitted >= 24 ? "Zoom out" : "Zoom in";
      const stepped = clamp(fitted + (direction === "Zoom out" ? -1 : 1));
      assert(autoState.preference === "auto", `an empty record did not render as auto-fit: ${JSON.stringify(autoState)}`);
      assert(autoServer.preferences.font_size === null, `an empty record did not read as auto: ${JSON.stringify(autoServer.preferences)}`);
      assert(stepped !== fitted, `this viewport pins the zoom against both clamps: ${JSON.stringify(autoState)}`);

      const putsBeforeZoom = autoServer.counters.preferencesPut;
      await viewControl(direction).click();
      const zoomSaved = await waitSnapshot((value) => value.counters.preferencesPut === putsBeforeZoom + 1, 8_000);
      assert(zoomSaved, "the zoom never reached the preference record");
      const zoomState = await fontState();
      assert(zoomState.rendered === stepped, `${direction} did not step one pixel from the rendered size: ${JSON.stringify({ autoState, zoomState, stepped, direction })}`);
      assert(zoomState.preference === String(stepped), `${direction} did not become the explicit preference: ${JSON.stringify({ zoomState, direction })}`);
      assert(zoomSaved.preferences.font_size === stepped, `the zoom was not stored as an explicit size: ${JSON.stringify(zoomSaved.preferences)}`);

      await reload();
      const reloadedState = await fontState();
      assert(reloadedState.rendered === stepped && reloadedState.preference === String(stepped),
        `an explicit size did not override the fit after a reload: ${JSON.stringify({ reloadedState, stepped })}`);

      await openF1View();
      const putsBeforeFit = (await snapshot()).counters.preferencesPut;
      await viewControl("Fit font").click();
      const fitSaved = await waitSnapshot((value) => value.counters.preferencesPut === putsBeforeFit + 1, 8_000);
      assert(fitSaved, "Fit font never reached the preference record");
      assert(fitSaved.preferences.font_size === null, `Fit font stored a number instead of auto: ${JSON.stringify(fitSaved.preferences)}`);
      await delay(400);
      const fitState = await fontState();
      assert(fitState.preference === "auto", `Fit font did not restore auto-fit: ${JSON.stringify(fitState)}`);

      await reload();
      // Compare like for like: Fit's settled state and the reload sample both
      // have the View popover open. Comparing an open-popover auto-fit to a closed
      // reload was the stale diagnostic, not a product font regression.
      await openF1View();
      await delay(300);
      const reloadedAuto = await fontState();
      assert(reloadedAuto.preference === "auto", `auto did not survive the reload: ${JSON.stringify(reloadedAuto)}`);
      assert(Math.abs(reloadedAuto.rendered - fitState.rendered) <= 0.05,
        `the reloaded open-sheet page did not fit to the same size: ${JSON.stringify({ reloadedAuto, fitState })}`);
      const f1Server = await snapshot();
      assert(f1Server.counters.preferencesPut === putsBeforeFit + 1, `the font tri-state wrote unexpected extra PUTs: ${f1Server.counters.preferencesPut}`);
      assert(f1.scenarioErrors.length === 0, `font tri-state browser errors ${JSON.stringify(f1.scenarioErrors)}`);
      evidence.timelines.push({ phase: "workspace_access-f1-font-tristate", autoState, zoomState, reloadedState, fitState, reloadedAuto, stepped, direction, server: f1Server });
      phase("workspace_access-f1-font-tristate");
      await f1.scenarioContext.close();
    }

    if (EVIDENCE) fs.writeFileSync(path.join(EVIDENCE, `session-switch-${ENGINE}.json`), `${JSON.stringify({ ...evidence, server: leaseRace }, null, 2)}\n`);

    process.stdout.write(`run_session_switch_browser (${ENGINE}, ${POINTER}): PASS\n`);
  } finally {
    await context.close().catch(() => undefined);
    await browser.close().catch(() => undefined);
    await fixture.close().catch(() => undefined);
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
