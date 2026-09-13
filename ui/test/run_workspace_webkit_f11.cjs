"use strict";

// Real-stack WebKit F11 gate. The stack is provisioned separately because it
// owns real tmux sessions, broker/frontdoor processes, and a hermetic TLS
// ingress. The default is the timing-clean release gate. Set
// PERSEA_WEBKIT_F11_LEDGER=1 only for the pre-navigation diagnostic ledger.
const crypto = require("node:crypto");
const fs = require("node:fs");
const https = require("node:https");
const path = require("node:path");

const STACK = process.env.STACK;
const EVIDENCE = process.env.EVIDENCE;
const PLAYWRIGHT = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
const LEDGER_ENABLED = process.env.PERSEA_WEBKIT_F11_LEDGER === "1";
const SCREENSHOT_ENABLED = process.env.PERSEA_WEBKIT_F11_SCREENSHOT === "1";
const EDIT_SAVE_RESTORE = process.env.PERSEA_WEBKIT_F11_EDIT_SAVE_RESTORE === "1";
const SESSION_COUNT = Number(process.env.PERSEA_WEBKIT_F11_SESSION_COUNT || "6");
if (!STACK || !EVIDENCE || !PLAYWRIGHT) throw new Error("STACK, EVIDENCE, and PERSEA_PLAYWRIGHT_MODULE are required");
if (!Number.isInteger(SESSION_COUNT) || SESSION_COUNT < 1 || SESSION_COUNT > 6) throw new Error("PERSEA_WEBKIT_F11_SESSION_COUNT must be 1..6");

const stackState = Object.fromEntries(fs.readFileSync(path.join(STACK, "state.env"), "utf8").trim().split("\n").map((line) => {
  const equals = line.indexOf("=");
  return [line.slice(0, equals), line.slice(equals + 1)];
}));
const port = Number(stackState.PORT);
const origin = `${stackState.SCHEME || "https"}://127.0.0.1:${port}`;
const sessions = Array.from({ length: SESSION_COUNT }, (_, index) => `ws${String(index + 1).padStart(2, "0")}`);
const workspaceName = `w1b-r5-${SESSION_COUNT}`;
const outputDir = path.join(EVIDENCE, "raw");
const screenshotDir = path.join(EVIDENCE, "screenshots");
fs.mkdirSync(outputDir, { recursive: true });
fs.mkdirSync(screenshotDir, { recursive: true });

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));
const sha256 = (value) => crypto.createHash("sha256").update(value).digest("hex");
const isCspRefusal = (message) => {
  if (message.type !== "error") return false;
  const text = message.text || "";
  return /Refused to apply a stylesheet/i.test(text) || (/Refused to/i.test(text) && /(content security policy|style-src|nonce|unsafe-inline|\bCSP\b)/i.test(text));
};

function arrangementFor(names) {
  const leaves = names.map((name) => ({ kind: "leaf", session: { realm: "local", server: "private", name }, on_missing: "offer" }));
  if (leaves.length === 1) return JSON.stringify({ version: 1, root: leaves[0] });
  const midpoint = Math.ceil(leaves.length / 2);
  const row = (children) => children.length === 1 ? children[0] : ({ kind: "split", direction: "row", weights: children.map(() => 1), children });
  return JSON.stringify({ version: 1, root: { kind: "split", direction: "column", weights: [1, 1], children: [row(leaves.slice(0, midpoint)), row(leaves.slice(midpoint))] } });
}

const BASE_INIT_SCRIPT = `
  window.__perseaCspViolations = [];
  document.addEventListener("securitypolicyviolation", (event) => {
    window.__perseaCspViolations.push({
      directive: event.violatedDirective,
      blocked: String(event.blockedURI).slice(0, 160),
      source: String(event.sourceFile).replace(/^https?:\\/\\/[^/]+/, ""),
      line: event.lineNumber,
      column: event.columnNumber,
    });
  });
`;

// This script does no hashing, stylesheet parsing, waiting, or synchronous DOM
// census. It records only the state available at the insertion/assignment call
// and buffers it. SHA-256 conversion happens in Node after one final read.
const LEDGER_INIT_SCRIPT = `
(() => {
  const events = [];
  const ids = new WeakMap();
  let nextId = 1;
  const idFor = (node) => {
    let id = ids.get(node);
    if (!id) { id = nextId++; ids.set(node, id); }
    return id;
  };
  const isSheetNode = (node) => node instanceof Element && (
    node.localName === "style" ||
    (node.localName === "link" && String(node.getAttribute("rel") || "").toLowerCase().split(/\\s+/).includes("stylesheet"))
  );
  const sheetNodes = (node) => {
    if (isSheetNode(node)) return [node];
    if (!(node instanceof DocumentFragment)) return [];
    const found = [];
    for (const child of node.childNodes) if (isSheetNode(child)) found.push(child);
    return found;
  };
  const stack = () => String(new Error().stack || "").split("\\n").slice(2, 10).join("\\n");
  const snapshot = (node, phase, operation, owner) => {
    events.push({
      phase,
      operation,
      at: performance.now(),
      id: idFor(node),
      kind: node.localName,
      rel: node.getAttribute("rel"),
      href: node.getAttribute("href"),
      nonce: node.getAttribute("nonce"),
      nonceIdl: node.nonce || "",
      text: node.localName === "style" ? (node.textContent || "") : (node.getAttribute("href") || ""),
      owner: owner === undefined ? stack() : owner,
    });
  };
  const observed = new WeakSet();
  const mutationObserver = new MutationObserver((records) => {
    const changed = new Set();
    for (const record of records) {
      const element = record.target instanceof Element ? record.target : record.target.parentElement;
      const sheet = isSheetNode(element) ? element : element?.closest?.("style, link[rel~='stylesheet' i]");
      if (sheet) changed.add(sheet);
    }
    for (const sheet of changed) snapshot(sheet, "mutate", "MutationObserver", null);
  });
  const observe = (sheet) => {
    if (observed.has(sheet)) return;
    observed.add(sheet);
    mutationObserver.observe(sheet, { attributes: true, childList: true, characterData: true, subtree: true, attributeFilter: ["nonce", "rel", "href"] });
  };
  const record = (node, phase, operation) => {
    for (const sheet of sheetNodes(node)) {
      observe(sheet);
      snapshot(sheet, phase, operation);
    }
  };
  const wrap = (prototype, name, before, after) => {
    const original = prototype[name];
    if (typeof original !== "function") return;
    Object.defineProperty(prototype, name, { configurable: true, writable: true, value: function (...args) {
      if (before) before.call(this, args);
      const result = Reflect.apply(original, this, args);
      if (after) after.call(this, args);
      return result;
    }});
  };
  wrap(Node.prototype, "appendChild", function (args) { record(args[0], "insert", "appendChild"); });
  wrap(Node.prototype, "insertBefore", function (args) { record(args[0], "insert", "insertBefore"); });
  wrap(Node.prototype, "replaceChild", function (args) { record(args[0], "insert", "replaceChild"); }, function (args) { record(args[1], "remove", "replaceChild"); });
  wrap(Node.prototype, "removeChild", null, function (args) { record(args[0], "remove", "removeChild"); });
  wrap(Element.prototype, "append", function (args) { for (const arg of args) record(arg, "insert", "append"); });
  wrap(Element.prototype, "prepend", function (args) { for (const arg of args) record(arg, "insert", "prepend"); });
  wrap(Element.prototype, "insertAdjacentElement", function (args) { record(args[1], "insert", "insertAdjacentElement"); });
  wrap(Element.prototype, "remove", null, function () { if (isSheetNode(this)) snapshot(this, "remove", "remove"); });
  const wrapAdopted = (prototype, owner) => {
    if (!prototype) return;
    const descriptor = Object.getOwnPropertyDescriptor(prototype, "adoptedStyleSheets");
    if (!descriptor || !descriptor.get || !descriptor.set) return;
    Object.defineProperty(prototype, "adoptedStyleSheets", {
      configurable: descriptor.configurable,
      enumerable: descriptor.enumerable,
      get: descriptor.get,
      set: function (sheets) {
        events.push({ phase: "assign", operation: "adoptedStyleSheets", ownerKind: owner, at: performance.now(),
          sheets: Array.from(sheets || [], (sheet, index) => ({ index, rules: (() => { try { return Array.from(sheet.cssRules || [], (rule) => rule.cssText).join("\\n"); } catch { return "<unreadable>"; } })() })), owner: stack() });
        return descriptor.set.call(this, sheets);
      },
    });
  };
  wrapAdopted(Document.prototype, "document");
  wrapAdopted(globalThis.ShadowRoot && ShadowRoot.prototype, "shadow-root");
  window.__perseaStyleLedger = events;
})();
`;

function headersOf(pathname) {
  return new Promise((resolve, reject) => {
    const request = https.request({ host: "127.0.0.1", port, path: pathname, method: "GET", headers: { Host: `127.0.0.1:${port}` }, rejectUnauthorized: false }, (response) => {
      response.resume();
      response.on("end", () => resolve({ status: response.statusCode, headers: response.headers }));
    });
    request.on("error", reject);
    request.end();
  });
}

const normalizedCsp = (csp) => (csp || "").replace(/'nonce-[^']+'/g, "'nonce-…'");
const consoleEntry = (message) => {
  const location = message.location();
  return {
    type: message.type(),
    text: message.text().slice(0, 500),
    location: location ? `${String(location.url).replace(/^https?:\/\/[^/]+/, "")}:${location.lineNumber}:${location.columnNumber}` : null,
  };
};

function hashLedger(ledger) {
  return (ledger || []).map((event) => {
    const result = { ...event };
    if (Object.prototype.hasOwnProperty.call(result, "text")) {
      result.contentBytes = Buffer.byteLength(result.text);
      result.contentSha256 = sha256(result.text);
      delete result.text;
    }
    if (Array.isArray(result.sheets)) {
      result.sheets = result.sheets.map((sheet) => ({ index: sheet.index, ruleBytes: Buffer.byteLength(sheet.rules), ruleSha256: sha256(sheet.rules) }));
    }
    return result;
  });
}

async function documentState(page) {
  return page.evaluate(() => {
    const visible = (element) => element && !element.closest("[hidden]") && element.getBoundingClientRect().width > 0;
    const inside = (element, cell) => {
      if (!element) return false;
      const outer = cell.getBoundingClientRect();
      const inner = element.getBoundingClientRect();
      const x = inner.x + inner.width / 2;
      const y = inner.y + inner.height / 2;
      return x >= outer.x && x <= outer.right && y >= outer.y && y <= outer.bottom;
    };
    const controls = [...document.querySelectorAll(".ws-cell")].map((cell) => {
      const fit = cell.querySelector(".persea-unified-fit-height");
      const composer = cell.querySelector(".persea-unified-composer-toggle");
      const fitRect = fit?.getBoundingClientRect();
      const composerRect = composer?.getBoundingClientRect();
      return {
        session: cell.dataset.wsSession,
        state: cell.dataset.wsState,
        fitInside: visible(fit) && inside(fit, cell),
        composerInside: visible(composer) && inside(composer, cell),
        fitSize: fitRect ? [fitRect.width, fitRect.height] : null,
        composerSize: composerRect ? [composerRect.width, composerRect.height] : null,
      };
    });
    const active = document.activeElement;
    const styles = [...document.querySelectorAll("style")].map((style) => ({
      nonce: style.nonce,
      rules: (() => { try { return style.sheet ? style.sheet.cssRules.length : -1; } catch { return -2; } })(),
      textBytes: (style.textContent || "").length,
    }));
    const firstCell = document.querySelector(".ws-cell");
    const row = firstCell?.querySelector(".xterm-rows");
    const cursor = firstCell?.querySelector(".xterm-cursor");
    const selection = firstCell?.querySelector(".xterm-selection");
    const computed = (element) => element ? getComputedStyle(element) : null;
    const rowStyle = computed(row);
    const cursorStyle = computed(cursor);
    const selectionStyle = computed(selection);
    return {
      controls,
      xtermCount: document.querySelectorAll(".xterm").length,
      metaNonces: document.querySelectorAll('meta[name="persea-style-nonce"]').length,
      styles,
      violations: window.__perseaCspViolations || [],
      activeCell: active?.closest?.(".ws-cell")?.dataset.wsSession || null,
      activeTag: active?.tagName || null,
      hiddenDisplays: [...document.querySelectorAll("[hidden]")].map((element) => getComputedStyle(element).display),
      requiredRules: {
        rows: rowStyle ? { pointerEvents: rowStyle.pointerEvents, whiteSpace: rowStyle.whiteSpace, fontSize: rowStyle.fontSize } : null,
        cursor: cursorStyle ? { display: cursorStyle.display, backgroundColor: cursorStyle.backgroundColor, color: cursorStyle.color } : null,
        selection: selectionStyle ? { position: selectionStyle.position, zIndex: selectionStyle.zIndex } : null,
      },
    };
  });
}

async function main() {
  const failures = [];
  const check = (id, condition, message, detail) => {
    if (!condition) {
      const failure = `${id}: ${message} ${JSON.stringify(detail)}`;
      failures.push(failure);
      console.error(`FAIL ${failure.slice(0, 1200)}`);
    }
    return condition;
  };
  const evidence = { engine: "webkit", ledgerEnabled: LEDGER_ENABLED, screenshotEnabled: SCREENSHOT_ENABLED, editSaveRestore: EDIT_SAVE_RESTORE, sessionCount: SESSION_COUNT, stack: STACK, origin, date: new Date().toISOString(), results: {} };
  const playwright = require(PLAYWRIGHT);
  const browser = await playwright.webkit.launch({ headless: true });
  const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 800 } });
  const page = await context.newPage();
  await page.addInitScript(BASE_INIT_SCRIPT);
  if (LEDGER_ENABLED) await page.addInitScript(LEDGER_INIT_SCRIPT);

  let documentConsole = [];
  page.on("console", (message) => { if (documentConsole.length < 200) documentConsole.push(consoleEntry(message)); });
  page.on("pageerror", (error) => { if (documentConsole.length < 200) documentConsole.push({ type: "pageerror", text: String(error).slice(0, 500), location: null }); });

  try {
    const terminalHeaders = await headersOf("/terminal?engine=unified-dev");
    const workspaceHeaders = await headersOf("/workspace?engine=unified-dev");
    evidence.results.cspHeaders = {
      terminal: { status: terminalHeaders.status, csp: normalizedCsp(terminalHeaders.headers["content-security-policy"]) },
      workspace: { status: workspaceHeaders.status, csp: normalizedCsp(workspaceHeaders.headers["content-security-policy"]) },
    };
    check("F11", evidence.results.cspHeaders.workspace.csp === evidence.results.cspHeaders.terminal.csp, "/workspace CSP equals /terminal CSP", evidence.results.cspHeaders);

    await page.goto(`${origin}/`, { waitUntil: "load" });
    await page.evaluate(([key, value]) => sessionStorage.setItem(key, value), [`persea-workspace-ephemeral-v1:${workspaceName}`, arrangementFor(sessions)]);
    await delay(4000);
    documentConsole = [];
    await page.goto(`${origin}/workspace?engine=unified-dev#name=${workspaceName}`, { waitUntil: "load" });
    await page.waitForFunction(() => !!document.querySelector(".ws-landing__open") || !!document.querySelector(".ws-unavailable") || !!document.querySelector(".persea-terminal-fatal"), null, { timeout: 15_000 });
    await page.click(".ws-landing__open");
    await page.waitForFunction((names) => {
      const cells = [...document.querySelectorAll(".ws-cell")];
      return cells.length === names.length && names.every((name) => cells.some((cell) => cell.dataset.wsSession === name && cell.dataset.wsState === "live"));
    }, sessions, { timeout: 60_000 }).catch(() => null);
    await delay(1500);
    const workspace = await documentState(page);
    check("F2", workspace.controls.length === SESSION_COUNT && workspace.controls.every((control) => control.state === "live"), `${SESSION_COUNT} real pane(s) live`, workspace.controls);
    check("F11", workspace.metaNonces === 1 && workspace.styles.length === SESSION_COUNT * 3 && workspace.styles.every((style) => style.nonce && style.rules > 0) && workspace.violations.length === 0, "one nonce meta, all xterm sheets nonced and applied, zero policy events", workspace);
    check("F11", workspace.requiredRules.rows?.pointerEvents === "none" && workspace.requiredRules.rows?.whiteSpace === "pre" && parseFloat(workspace.requiredRules.rows?.fontSize || "0") > 0, "xterm row rules are applied", workspace.requiredRules);
    check("F11", workspace.requiredRules.cursor && workspace.requiredRules.cursor.display !== "none" && workspace.requiredRules.cursor.backgroundColor !== "rgba(0, 0, 0, 0)", "xterm cursor rules are applied", workspace.requiredRules);
    check("F11", workspace.requiredRules.selection?.position === "absolute", "xterm selection layer rules are applied", workspace.requiredRules);
    check("F15", workspace.controls.every((control) => control.fitInside && control.composerInside && control.fitSize?.every((size) => size >= 44) && control.composerSize?.every((size) => size >= 44)), "44 px controls stay inside every workspace cell", workspace.controls);
    check("F15", workspace.hiddenDisplays.every((display) => display === "none"), "every hidden container computes to display:none", workspace.hiddenDisplays);

    await page.evaluate(() => {
      const cell = document.querySelector(".ws-cell");
      const toolbar = cell.querySelector(".persea-unified-toolbar");
      cell.querySelector(".ws-cell__header").focus();
      window.__perseaMenuEscapePrevented = false;
      toolbar.addEventListener("keydown", (event) => {
        if (event.key === "Escape") window.__perseaMenuEscapePrevented = event.defaultPrevented;
      });
    });
    const tabOrder = [];
    // Updated deliberately for UX-8 F4: the fine-pointer quick-actions opener
    // is a real toolbar control and takes its place in the row, between the ↕
    // pair and the composer toggle. This context is the 1280x800 desktop one,
    // so the opener is present here; the phone context below never sees it.
    //
    // Updated again for UX-9 FOLLOW-UP 2: the session tag is a real control in
    // this row too -- it opens the identity popover -- and it is the row's
    // first element, so it is the first stop. Its expected name is read from
    // the tag the page rendered rather than hardcoded, because the session
    // names here come from the real stack's seed, not from this file. Nothing
    // was removed: the previous six stops follow in the same order.
    const identityStop = await page.evaluate(() => document
      .querySelector(".ws-cell .persea-unified-tag")?.getAttribute("aria-label") ?? null);
    check("F15", typeof identityStop === "string" && /^Session .+ Show session details$/.test(identityStop),
      "the cell's session tag must carry a spoken identity", identityStop);
    for (let index = 0; index < 7; index += 1) {
      await page.keyboard.press("Tab");
      tabOrder.push(await page.evaluate(() => document.activeElement?.getAttribute("aria-label") || document.activeElement?.textContent?.trim() || document.activeElement?.getAttribute("role") || ""));
    }
    check("F15", JSON.stringify(tabOrder) === JSON.stringify([identityStop, "Switch session", "Fit terminal height", "Quick actions", "Open composer", "More controls", "Terminal input"]), "compact toolbar Tab order is header controls then terminal", tabOrder);
    await page.evaluate(() => {
      const toolbar = document.querySelector(".ws-cell .persea-unified-toolbar");
      const more = toolbar.querySelector(".persea-unified-toolbar__more");
      more.focus();
      more.click();
      toolbar.querySelector(".persea-unified-toolbar__secondary button").focus();
    });
    await page.keyboard.press("Escape");
    const escapeFocus = await page.evaluate(() => {
      const more = document.querySelector(".ws-cell .persea-unified-toolbar__more");
      return { open: more.getAttribute("aria-expanded"), activeIsMore: document.activeElement === more, prevented: window.__perseaMenuEscapePrevented };
    });
    check("F15", escapeFocus.open === "false" && escapeFocus.activeIsMore && escapeFocus.prevented, "Escape closes More, prevents terminal meaning, and synchronously restores trigger focus", escapeFocus);
    const outsideFocus = await page.evaluate(() => {
      const toolbar = document.querySelector(".ws-cell .persea-unified-toolbar");
      const more = toolbar.querySelector(".persea-unified-toolbar__more");
      const first = toolbar.querySelector(".persea-unified-toolbar__secondary button");
      more.click();
      first.focus();
      document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      return { open: more.getAttribute("aria-expanded"), activeIsMore: document.activeElement === more, activeText: document.activeElement?.textContent?.trim() };
    });
    check("F15", outsideFocus.open === "false" && !outsideFocus.activeIsMore && outsideFocus.activeText === "Zoom out", "outside-pointer dismissal closes without stealing focus", outsideFocus);
    evidence.results.toolbarKeyboard = { tabOrder, escapeFocus, outsideFocus };

    const target = await page.$(`.ws-cell[data-ws-session="${sessions[Math.min(SESSION_COUNT - 1, 3)]}"] .xterm-screen`);
    if (target) {
      await target.click();
      await page.keyboard.type("echo wk-w1br5;");
      await delay(300);
      const afterTyping = await documentState(page);
      evidence.results.afterTyping = afterTyping;
      check("F3", afterTyping.activeCell === sessions[Math.min(SESSION_COUNT - 1, 3)] && afterTyping.controls.every((control) => control.state === "live"), "typing keeps focus in the chosen pane and all panes live", afterTyping);
    } else {
      check("F3", false, "typing target exists", null);
    }
    const workspaceLedger = LEDGER_ENABLED ? hashLedger(await page.evaluate(() => window.__perseaStyleLedger || [])) : undefined;
    const workspaceConsole = documentConsole.slice();
    evidence.results.workspace = { state: workspace, console: workspaceConsole, ...(workspaceLedger ? { ledger: workspaceLedger } : {}) };
    check("F11", workspaceConsole.filter(isCspRefusal).length === 0, "WebKit console contains no CSP stylesheet refusal", workspaceConsole.filter(isCspRefusal));
    if (SCREENSHOT_ENABLED) {
      const consoleCount = documentConsole.length;
      await page.screenshot({ path: path.join(screenshotDir, `webkit_workspace_${SESSION_COUNT}.png`) });
      await delay(50);
      const screenshotConsole = documentConsole.slice(consoleCount);
      evidence.results.screenshotInstrumentation = { console: screenshotConsole };
      check("F11", screenshotConsole.filter(isCspRefusal).length === 0, "WebKit screenshot instrumentation contains no CSP stylesheet refusal", screenshotConsole.filter(isCspRefusal));
    }

    if (EDIT_SAVE_RESTORE) {
      const alias = "WebKit durable shell";
      await page.click(".ws-workspace-edit");
      const aliasInput = page.locator(`.ws-editor__pane[data-ws-session="${sessions[0]}"] .ws-editor__alias input`);
      await aliasInput.fill(alias);
      await page.locator(`.ws-editor__pane[data-ws-session="${sessions[0]}"] .ws-editor__alias-save`).click();
      await page.click(".ws-editor__save");
      await page.waitForFunction((expected) => !document.querySelector(".ws-editor:not([hidden])") && document.querySelector('.ws-cell[data-ws-session="ws01"] .ws-cell__hint')?.textContent === expected, alias, { timeout: 15_000 });
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector(".ws-landing__open", { timeout: 15_000 });
      const beforeOpen = await page.evaluate(() => ({ cells: document.querySelectorAll(".ws-cell").length, xterms: document.querySelectorAll(".xterm").length }));
      await page.click(".ws-landing__open");
      await page.waitForFunction((expected) => document.querySelectorAll(".ws-cell").length === 6 && document.querySelector('.ws-cell[data-ws-session="ws01"] .ws-cell__hint')?.textContent === expected, alias, { timeout: 60_000 }).catch(() => null);
      const restored = await page.evaluate(() => ({
        cells: document.querySelectorAll(".ws-cell").length,
        toolbars: document.querySelectorAll(".persea-unified-toolbar").length,
        alias: document.querySelector('.ws-cell[data-ws-session="ws01"] .ws-cell__hint')?.textContent || null,
      }));
      evidence.results.editSaveRestore = { alias, beforeOpen, restored };
      check("F4", beforeOpen.cells === 0 && beforeOpen.xterms === 0, "durable reload remains attachment-free until trusted Open", beforeOpen);
      check("F4", restored.cells === SESSION_COUNT && restored.toolbars === SESSION_COUNT && restored.alias === alias, "explicit Save survives reload and trusted reopen", restored);
    }

    // Single-terminal control: obtain the product-generated link from the same
    // session inventory, then capture its console and optional ledger separately.
    const phoneContext = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true });
    const phonePage = await phoneContext.newPage();
    await phonePage.goto(`${origin}/`, { waitUntil: "load" });
    await phonePage.evaluate(([key, value]) => sessionStorage.setItem(key, value), [`persea-workspace-ephemeral-v1:${workspaceName}`, arrangementFor(sessions)]);
    await phonePage.goto(`${origin}/workspace?engine=unified-dev#name=${workspaceName}`, { waitUntil: "load" });
    await phonePage.waitForFunction(() => !!document.querySelector(".ws-phone") || !!document.querySelector(".ws-unavailable"), null, { timeout: 15_000 });
    const singleHref = await phonePage.evaluate(() => document.querySelector(".ws-phone__leaf a")?.getAttribute("href") || null);
    await phoneContext.close();
    check("F12", !!singleHref, "phone posture exposes a single-terminal control link", singleHref);

    if (singleHref) {
      documentConsole = [];
      await page.goto(new URL(singleHref, origin).toString(), { waitUntil: "load" });
      await page.waitForFunction(() => document.querySelectorAll(".xterm").length === 1, null, { timeout: 30_000 }).catch(() => null);
      await delay(1500);
      const singleState = await page.evaluate(() => ({
        xterms: document.querySelectorAll(".xterm").length,
        styles: [...document.querySelectorAll("style")].map((style) => ({ nonce: style.nonce, rules: (() => { try { return style.sheet ? style.sheet.cssRules.length : -1; } catch { return -2; } })() })),
        violations: window.__perseaCspViolations || [],
      }));
      const singleLedger = LEDGER_ENABLED ? hashLedger(await page.evaluate(() => window.__perseaStyleLedger || [])) : undefined;
      const singleConsole = documentConsole.slice();
      evidence.results.singleTerminalControl = { state: singleState, console: singleConsole, ...(singleLedger ? { ledger: singleLedger } : {}) };
      check("F11", singleState.xterms === 1 && singleState.styles.length === 3 && singleState.styles.every((style) => style.nonce && style.rules > 0) && singleState.violations.length === 0, "single-terminal control has three applied nonced sheets and zero policy events", singleState);
      check("F11", singleConsole.filter(isCspRefusal).length === 0, "single-terminal console contains no CSP stylesheet refusal", singleConsole.filter(isCspRefusal));
    }
  } catch (error) {
    const message = `gate error: ${error && error.stack ? error.stack : String(error)}`;
    failures.push(message);
    console.error(message);
  } finally {
    await context.close().catch(() => {});
    await browser.close().catch(() => {});
  }

  evidence.status = failures.length === 0 ? "PASS" : "FAIL";
  evidence.failures = failures;
  const suffix = `${LEDGER_ENABLED ? "ledger" : "release"}_${SESSION_COUNT}${SCREENSHOT_ENABLED ? "_screenshot" : ""}`;
  fs.writeFileSync(path.join(outputDir, `webkit_f11_${suffix}.json`), JSON.stringify(evidence, null, 2));
  console.log(JSON.stringify({ status: evidence.status, failures: failures.length, ledger: LEDGER_ENABLED, sessions: SESSION_COUNT }));
  if (failures.length > 0) process.exitCode = 1;
}

main().catch((error) => {
  console.error(error && error.stack ? error.stack : String(error));
  process.exitCode = 1;
});
