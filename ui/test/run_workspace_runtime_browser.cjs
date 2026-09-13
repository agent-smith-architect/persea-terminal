"use strict";

// M9 W1b workspace runtime gate: the REAL bundled /workspace document in real
// Chromium against the workspace fixture (six independent sessions with the
// front door's one-time handles, per-session leases, takeover offers, source
// bindings, and broker automata). Falsifiers W1B-F1…F15 in their fixture
// form; the real-stack forms (tmux geometry witness, front-door CSP on both
// engines, B1 eviction on a real broker) live in run_workspace_stack_browser.
//
// Every scenario records its receipts into the evidence JSON; a failed
// assertion fails the gate (exit 1) after bounded teardown.

const fs = require("fs");
const os = require("os");
const path = require("path");
const { startWorkspaceFixture, STYLE_NONCE } = require("./workspace_fixture.cjs");
const { assert, delay, requestJSON, Tab: BaseTab, launchChrome, stopChrome } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const EVIDENCE_DIR = process.env.PERSEA_WS_RUNTIME_EVIDENCE_DIR || fs.mkdtempSync(path.join(os.tmpdir(), "persea-ws-runtime-evidence-"));
fs.mkdirSync(EVIDENCE_DIR, { recursive: true });
const SESSIONS = ["ws01", "ws02", "ws03", "ws04", "ws05", "ws06"];
const WORKSPACE = "ops";

const STATE = `(() => {
  const rect = (el) => { if (!el) return null; const r = el.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2, w: r.width, h: r.height }; };
  const visible = (el) => el && !el.closest("[hidden]") && el.getBoundingClientRect().width > 0;
  // A control counts only when its centre lies inside its own cell: a toolbar
  // that overflows the cell puts its buttons over a neighbour or off-screen.
  const inside = (el, cell) => { if (!el) return false; const r = el.getBoundingClientRect(); const b = cell.getBoundingClientRect(); const x = r.x + r.width / 2; const y = r.y + r.height / 2; return x >= b.x && x <= b.x + b.width && y >= b.y && y <= b.y + b.height; };
  const cells = [...document.querySelectorAll(".ws-cell")].map((c) => {
    const view = c.querySelector(".persea-unified-view-disclosure");
    const fit = [...c.querySelectorAll(".persea-unified-view-popover button")]
      .find((button) => (button.getAttribute("aria-label") || button.textContent || "").trim() === "Fit rows");
    const take = c.querySelector(".persea-unified-notice__take-control");
    const opener = c.querySelector(".persea-unified-quick-actions");
    const composerToggle = c.querySelector(".persea-unified-composer-toggle");
    const toolbar = c.querySelector(".persea-unified-toolbar");
    const header = c.querySelector(".ws-cell__header");
    return {
      session: c.dataset.wsSession, state: c.dataset.wsState, tone: c.dataset.wsTone, designated: c.dataset.wsDesignated === "true", stamp: c.dataset.wsStamp ?? null,
      badge: c.querySelector(".ws-cell__badge")?.textContent ?? "", headline: c.querySelector(".ws-cell__state-headline")?.textContent ?? "",
      detail: c.querySelector(".ws-cell__state-detail")?.textContent ?? "", code: c.querySelector(".ws-cell__state-code")?.textContent ?? "",
      overlayHidden: c.querySelector(".ws-cell__state")?.hidden ?? null,
      // What the OPERATOR can read. The textContent fields above are blind
      // to both ways a live cell hides its copy (the overlay's hidden
      // attribute and the header badge's display:none), so a projection
      // detail can be asserted present and still be invisible on screen.
      wsDetail: c.dataset.wsDetail ?? null,
      deferralNotice: (() => { const el = c.querySelector(".ws-cell__notice"); if (!el) return null; const r = el.getBoundingClientRect(); return { hidden: el.hidden, display: getComputedStyle(el).display, text: (el.textContent ?? "").trim(), area: Math.round(r.width) * Math.round(r.height) }; })(),
      readsBadgePhrase: /rotation deferred/i.test(c.innerText), readsDeferralSentence: /full-screen app is active/i.test(c.innerText),
      actions: [...c.querySelectorAll(".ws-cell__action")].filter((a) => visible(a)).map((a) => ({ affordance: a.dataset.wsAffordance, label: a.textContent, point: rect(a) })),
      actionsHiddenDisplay: c.querySelector(".ws-cell__state-actions") ? getComputedStyle(c.querySelector(".ws-cell__state-actions")).display : null,
      actionsHiddenAttr: c.querySelector(".ws-cell__state-actions")?.hidden ?? null,
      xterm: !!c.querySelector(".xterm"), pages: c.querySelectorAll(".persea-unified-terminal").length,
      rows: c.querySelector(".xterm-rows")?.textContent ?? "", rowCount: c.querySelector(".xterm-rows")?.children.length ?? 0,
      textarea: rect(c.querySelector(".xterm-helper-textarea")), screen: rect(c.querySelector(".xterm-screen")),
      viewPoint: view && visible(view) && inside(view, c) ? rect(view) : null,
      viewPopover: (() => {
        const popover = c.querySelector(".persea-unified-view-popover");
        if (!popover || !visible(popover)) return null;
        const box = popover.getBoundingClientRect(); const cellBox = c.getBoundingClientRect();
        return { left: box.left, right: box.right, top: box.top, bottom: box.bottom, width: box.width, height: box.height,
          insideCell: box.left >= cellBox.left - 1 && box.right <= cellBox.right + 1 && box.top >= cellBox.top - 1 && box.bottom <= cellBox.bottom + 1,
          insideViewport: box.left >= 8 && box.right <= innerWidth - 8 && box.top >= 8 && box.bottom <= innerHeight - 8,
          horizontalScroll: popover.scrollWidth !== popover.clientWidth };
      })(),
      fitPoint: fit && !fit.disabled && visible(fit) ? rect(fit) : null,
      fitHit: (() => { if (!fit || !visible(fit)) return false; const r = fit.getBoundingClientRect(); const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2); return !!hit && (hit === fit || fit.contains(hit)); })(),
      fitHitBy: (() => { if (!fit || !visible(fit)) return ""; const r = fit.getBoundingClientRect(); const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2); return hit ? hit.tagName + "." + String(hit.className || "") : "none"; })(),
      fitDiagnostics: fit ? (() => {
        const popover = fit.closest(".persea-unified-view-popover");
        const terminal = fit.closest(".persea-unified-terminal");
        const host = terminal.querySelector(".persea-unified-xterm");
        const xterm = terminal.querySelector(".xterm");
        return {
          toolbarZ: getComputedStyle(toolbar).zIndex, toolbarPosition: getComputedStyle(toolbar).position,
          popoverZ: getComputedStyle(popover).zIndex, popoverPointer: getComputedStyle(popover).pointerEvents,
          terminalZ: getComputedStyle(terminal).zIndex, terminalPosition: getComputedStyle(terminal).position,
          xtermZ: getComputedStyle(xterm).zIndex, xtermPosition: getComputedStyle(xterm).position,
          hostZ: getComputedStyle(host).zIndex, hostPosition: getComputedStyle(host).position,
          cellZ: getComputedStyle(c).zIndex, focusWithin: c.matches(":focus-within"), box: rect(fit),
          cellBox: rect(c), trackBox: rect(c.closest(".ws-track")), toolbarBox: rect(toolbar),
          popoverBox: rect(popover), terminalBox: rect(terminal),
        };
      })() : null,
      fitDisabled: fit ? fit.disabled : null, fitVisible: visible(fit),
      takePoint: take && !take.hidden ? rect(take) : null,
      openerPoint: visible(opener) ? rect(opener) : null,
      composerPoint: visible(composerToggle) && inside(composerToggle, c) ? rect(composerToggle) : null,
      composer: { expanded: composerToggle ? composerToggle.getAttribute("aria-expanded") : null, textareaValue: c.querySelector(".attachment-page__composer-textarea")?.value ?? null, status: c.querySelector(".attachment-page__composer-status")?.textContent ?? null, panelInCell: !!c.querySelector(".attachment-page__composer") },
      refusal: c.querySelector(".persea-unified-refusal")?.textContent ?? "", refusalHidden: c.querySelector(".persea-unified-refusal")?.hidden ?? null,
      loading: (() => { const el = c.querySelector(".persea-unified-loading"); if (!el) return null; const r = el.getBoundingClientRect(); return { hidden: el.hidden, text: el.textContent ?? "", w: r.width, h: r.height }; })(),
      sheetOpen: !!c.querySelector(".persea-unified-sheet:not([hidden])"),
      geometry: c.querySelector(".persea-unified-geometry")?.textContent ?? "", fontSize: c.querySelector(".xterm") ? getComputedStyle(c.querySelector(".xterm")).fontSize : null,
      theme: c.querySelector(".persea-unified-terminal")?.dataset.theme ?? "",
      fontBaseline: Number(c.querySelector(".persea-unified-terminal")?.dataset.fontBaseline ?? "NaN"),
      themeValues: [...c.querySelectorAll(".persea-unified-preference__theme")].map((select) => select.value),
      viewportHeight: c.querySelector(".xterm-viewport")?.clientHeight ?? null,
      viewportScrollTop: c.querySelector(".xterm-viewport")?.scrollTop ?? null,
      xtermStamp: c.querySelector(".xterm")?.dataset.ep4Stamp ?? null,
      connection: c.querySelector(".persea-unified-connection")?.textContent ?? "",
      notice: c.querySelector(".persea-unified-notice__headline")?.textContent ?? "",
      alias: c.querySelector(".ws-cell__hint")?.textContent ?? "",
      header: rect(header), toolbar: rect(toolbar), toolbarStamp: toolbar?.dataset.wsToolbarStamp ?? null,
      toolbarCount: c.querySelectorAll(".persea-unified-toolbar").length,
      geometryCount: c.querySelectorAll(".persea-unified-geometry").length,
      fitCount: [...c.querySelectorAll(".persea-unified-view-popover button")].filter((button) => (button.getAttribute("aria-label") || button.textContent || "").trim() === "Fit rows").length,
      composeCount: c.querySelectorAll(".persea-unified-composer-toggle").length,
      moreCount: c.querySelectorAll(".persea-unified-toolbar__more").length,
      quickActionsCount: c.querySelectorAll(".persea-unified-quick-actions").length,
      // U3 inside a cell (review FOLLOW-UP 2, finding 10). Six panes on
      // screen, so the one thing a pane must say is which session it is
      // showing. Laid out is not shown: the bar's controls come later in
      // document order, so an over-subscribed bar paints them on top of a tag
      // that still measures a box. The probe therefore hit-tests the tag's
      // centre and its dot's centre, and intersects the tag box with every
      // other control in the same bar.
      tag: (() => {
        const t = c.querySelector(".persea-unified-tag");
        if (!t) return null;
        const box = t.getBoundingClientRect();
        const dot = t.querySelector(".persea-unified-tag__dot");
        const alias = t.querySelector(".persea-unified-tag__alias");
        const bar = t.closest(".persea-unified-toolbar");
        const barBox = bar ? bar.getBoundingClientRect() : null;
        const buttons = bar ? [...bar.querySelectorAll("button")] : [];
        const renderedButtons = buttons.map((button) => ({ button, box: button.getBoundingClientRect() }))
          .filter(({ box: r }) => r.width > 0 && r.height > 0);
        const boxes = renderedButtons.map(({ box }) => box);
        const others = buttons.filter((b) => b !== t).map((b) => b.getBoundingClientRect()).filter((r) => r.width > 0 && r.height > 0);
        const owns = (node) => Boolean(node) && (node === t || t.contains(node));
        const hitAt = (x, y) => { const node = document.elementFromPoint(x, y); return { owned: owns(node), at: node ? (node.className || node.tagName) : null }; };
        const dotBox = dot ? dot.getBoundingClientRect() : null;
        const centre = hitAt(box.x + box.width / 2, box.y + box.height / 2);
        const dotPoint = dotBox ? hitAt(dotBox.x + dotBox.width / 2, dotBox.y + dotBox.height / 2) : { owned: false, at: null };
        return {
          width: box.width, height: box.height, lines: t.getClientRects().length,
          dot: dotBox ? dotBox.width : 0,
          dotState: dot ? (dot.dataset.state ?? null) : null,
          name: t.querySelector(".persea-unified-tag__name")?.textContent ?? "",
          aliasWidth: alias ? alias.getBoundingClientRect().width : 0,
          controls: boxes.length,
          controlBoxes: renderedButtons.map(({ button, box: r }) => ({
            className: button.className,
            label: button.getAttribute("aria-label") || button.textContent || "",
            left: r.left, right: r.right, width: r.width,
          })),
          outside: barBox ? boxes.filter((r) => r.left < barBox.left - 0.5 || r.right > barBox.right + 0.5).length : -1,
          oneRow: boxes.length > 0 && Math.max(...boxes.map((r) => r.top)) < Math.min(...boxes.map((r) => r.bottom)),
          hit: centre.owned, hitAt: centre.at,
          dotHit: dotPoint.owned, dotHitAt: dotPoint.at,
          covered: others.filter((r) => r.left < box.right - 0.5 && r.right > box.left + 0.5 && r.top < box.bottom - 0.5 && r.bottom > box.top + 0.5).length,
        };
      })(),
      cell: rect(c),
    };
  });
  const active = document.activeElement;
  const dividers = [...document.querySelectorAll(".ws-divider")].map((d) => ({ split: d.dataset.wsSplit, index: d.dataset.wsDivider, orientation: d.getAttribute("aria-orientation"), value: d.getAttribute("aria-valuenow"), point: rect(d) }));
  const landing = document.querySelector(".ws-landing");
  return {
    href: location.href, ready: document.readyState, harnessReady: document.body?.dataset.wsHarnessReady === "true",
    selectionText: String(document.getSelection() ?? ""), activeDetail: document.activeElement ? document.activeElement.tagName + "." + document.activeElement.className + "|" + (document.activeElement.closest(".ws-cell")?.dataset.wsSession ?? "") + "|" + (document.activeElement.closest(".xterm") ? "xterm" : "") : null,
    navigationType: performance.getEntriesByType("navigation")[0]?.type ?? null,
    fatal: document.querySelector(".persea-terminal-fatal")?.textContent ?? null,
    landing: landing ? landing.dataset.wsLanding : null,
    landingBoxes: landing ? [...landing.querySelectorAll(".ws-landing__session-box")].map((b) => ({ session: b.closest(".ws-landing__session")?.dataset.wsSession, checked: b.checked, disabled: b.disabled, point: rect(b) })) : [],
    openPoint: rect(document.querySelector(".ws-landing__open")), openDisabled: document.querySelector(".ws-landing__open")?.disabled ?? null,
    openText: document.querySelector(".ws-landing__open")?.textContent ?? "",
    phone: !!document.querySelector(".ws-phone"), phoneNotice: document.querySelector(".ws-phone__notice")?.textContent ?? "",
    phoneLeaves: [...document.querySelectorAll(".ws-phone__leaf")].map((l) => ({ session: l.dataset.wsSession, state: l.dataset.wsState, stateText: l.querySelector(".ws-phone__leaf-state")?.textContent ?? "", href: l.querySelector("a")?.getAttribute("href") ?? null, linkHeight: l.querySelector("a")?.getBoundingClientRect().height ?? 0 })),
    unavailable: document.querySelector(".ws-unavailable")?.dataset.wsUnavailable ?? null,
    workspaceTitle: document.querySelector(".ws-workspace-title")?.textContent ?? "",
    editing: !!document.querySelector(".ws-editor:not([hidden])"),
    editorStatus: document.querySelector(".ws-editor__status")?.textContent ?? "",
    editorConflict: !!document.querySelector(".ws-editor__conflict"),
    dashboardMode: document.body.classList.contains("dashboard-mode"),
    dashboardSessions: document.querySelectorAll(".session-row, .dashboard-session").length,
    cells, dividers,
    activeCell: active?.closest?.(".ws-cell")?.dataset.wsSession ?? null, activeTag: active?.tagName ?? null, activeClass: active?.className ?? null,
    xtermCount: document.querySelectorAll(".xterm").length, pageCount: document.querySelectorAll(".persea-unified-terminal").length,
    metaNonces: document.querySelectorAll('meta[name="persea-style-nonce"]').length,
    metaNonce: document.querySelector('meta[name="persea-style-nonce"]')?.content ?? "",
    styleNodes: [...document.querySelectorAll("style")].map((s) => ({ nonce: s.nonce, rules: (() => { try { return s.sheet ? s.sheet.cssRules.length : -1; } catch { return -2; } })() })),
    unnoncedScripts: [...document.querySelectorAll("script")].filter((s) => !s.src && !s.nonce).length,
    inlineStyleAttrs: document.querySelectorAll("[style]").length,
    cspViolations: window.__perseaCspViolations ?? -1,
    resizeCount: window.__perseaResizeEvents ?? -1,
    documentWidth: document.documentElement.scrollWidth,
    ephemeral: (() => { try { return sessionStorage.getItem("persea-workspace-ephemeral-v1:${WORKSPACE}"); } catch { return null; } })(),
    inner: { w: innerWidth, h: innerHeight }, screen: { w: screen.width, h: screen.height },
    coarse: matchMedia("(pointer: coarse)").matches, fine: matchMedia("(pointer: fine)").matches, pointerNone: matchMedia("(pointer: none)").matches,
    touchTargets: (() => {
      const out = [];
      for (const el of document.querySelectorAll("button, a[href], input, [role='separator']")) {
        if (el.matches(".xterm-helper-textarea") || el.closest("[hidden]")) continue;
        const cs = getComputedStyle(el);
        if (cs.display === "none" || cs.visibility === "hidden") continue;
        const r = el.getBoundingClientRect();
        if (r.width === 0 || r.height === 0) continue;
        out.push({ cls: String(el.className || "").split(" ")[0], label: (el.getAttribute("aria-label") || (el.textContent || "").trim()).slice(0, 30), w: r.width, h: r.height });
      }
      return out;
    })(),
  };
})()`;

const INSTRUMENT = `
  window.__perseaCspViolations = 0;
  document.addEventListener("securitypolicyviolation", () => { window.__perseaCspViolations += 1; });
  window.__perseaResizeEvents = 0;
  window.addEventListener("resize", () => { window.__perseaResizeEvents += 1; });
  // The workspace page arms a long-period presentation poll at mount, and
  // every tick of it issues a real inventory GET. The document keyboard
  // preferences service has its own long-period GET. Several scenarios here
  // assert an EXACT number of inventory GETs across a measured window (the
  // shared in-flight fetch, "a structural update must not refetch"), so a
  // tick landing inside such a window is a spurious RED. The poll is the
  // product's own liveness mechanism and must not be weakened, and the
  // assertions are exact laws that must not be relaxed, so the clock moves
  // instead: long intervals are HELD here and fire only when a scenario
  // calls __perseaTickIntervals(). Short timers (cursor blink, the keyboard
  // watchdog) are untouched.
  window.__perseaHeldIntervals = [];
  const perseaSetInterval = window.setInterval.bind(window);
  const perseaClearInterval = window.clearInterval.bind(window);
  window.setInterval = function (handler, delay, ...args) {
    if (typeof delay === "number" && delay >= 10000 && typeof handler === "function") {
      window.__perseaHeldIntervals.push({ id: -(window.__perseaHeldIntervals.length + 1), handler, delay, args, cleared: false });
      return window.__perseaHeldIntervals[window.__perseaHeldIntervals.length - 1].id;
    }
    return perseaSetInterval(handler, delay, ...args);
  };
  window.clearInterval = function (id) {
    const held = window.__perseaHeldIntervals.find((entry) => entry.id === id);
    if (held) { held.cleared = true; return undefined; }
    return perseaClearInterval(id);
  };
  window.__perseaHeldPeriods = () => window.__perseaHeldIntervals.filter((entry) => !entry.cleared).map((entry) => entry.delay);
  window.__perseaTickIntervals = () => {
    const live = window.__perseaHeldIntervals.filter((entry) => !entry.cleared);
    for (const entry of live) entry.handler(...entry.args);
    return live.length;
  };
`;

class Tab extends BaseTab {
  static async open(name, debugPort, origin) {
    const tab = await super.open(name, debugPort, origin, STATE);
    await tab.cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: INSTRUMENT });
    return tab;
  }

  async emulate(metrics, coarse) {
    await this.cdp.send("Emulation.setDeviceMetricsOverride", metrics);
    await this.cdp.send("Emulation.setTouchEmulationEnabled", { enabled: coarse, maxTouchPoints: coarse ? 5 : 1 });
    await this.cdp.send("Emulation.setEmulatedMedia", {
      features: coarse
        ? [{ name: "pointer", value: "coarse" }, { name: "any-pointer", value: "coarse" }, { name: "hover", value: "none" }, { name: "any-hover", value: "none" }]
        : [{ name: "pointer", value: "fine" }, { name: "any-pointer", value: "fine" }, { name: "hover", value: "hover" }, { name: "any-hover", value: "hover" }],
    });
  }

  async clearEmulation() {
    await this.cdp.send("Emulation.clearDeviceMetricsOverride");
    await this.cdp.send("Emulation.setTouchEmulationEnabled", { enabled: false });
    await this.cdp.send("Emulation.setEmulatedMedia", { features: [] });
  }

  async typeText(text) {
    for (const character of text) {
      await this.cdp.send("Input.dispatchKeyEvent", { type: "keyDown", text: character, key: character, unmodifiedText: character });
      await this.cdp.send("Input.dispatchKeyEvent", { type: "keyUp", key: character });
    }
  }

  async pressKey(key) {
    const keyCode = { Tab: 9, Enter: 13, Escape: 27 }[key];
    await this.cdp.send("Input.dispatchKeyEvent", { type: "keyDown", key, code: key, windowsVirtualKeyCode: keyCode, nativeVirtualKeyCode: keyCode });
    await this.cdp.send("Input.dispatchKeyEvent", { type: "keyUp", key, code: key, windowsVirtualKeyCode: keyCode, nativeVirtualKeyCode: keyCode });
  }

  async pointerDrag(from, to, steps = 10) {
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: from.x, y: from.y });
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: from.x, y: from.y, button: "left", buttons: 1, clickCount: 1 });
    for (let i = 1; i <= steps; i += 1) {
      const x = from.x + ((to.x - from.x) * i) / steps;
      const y = from.y + ((to.y - from.y) * i) / steps;
      await this.cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x, y, button: "left", buttons: 1 });
    }
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: to.x, y: to.y, button: "left", buttons: 0, clickCount: 1 });
  }

  async screenshot(file) {
    const shot = await this.cdp.send("Page.captureScreenshot", { format: "png" });
    fs.writeFileSync(path.join(EVIDENCE_DIR, file), Buffer.from(shot.data, "base64"));
  }
}

const DESKTOP = { width: 1280, height: 800, deviceScaleFactor: 1, mobile: false, screenWidth: 1280, screenHeight: 800 };
const PHONE = { width: 390, height: 844, deviceScaleFactor: 3, mobile: true, screenWidth: 390, screenHeight: 844 };
const PHONE_LANDSCAPE = { width: 844, height: 390, deviceScaleFactor: 3, mobile: true, screenWidth: 844, screenHeight: 390 };
const TABLET = { width: 1024, height: 768, deviceScaleFactor: 2, mobile: true, screenWidth: 1024, screenHeight: 768 };
const TABLET_KEYBOARD = { width: 1024, height: 350, deviceScaleFactor: 2, mobile: true, screenWidth: 1024, screenHeight: 768 };
// A coarse-pointer desktop (a touch laptop): the quick-actions opener exists,
// and six cells still have room for a sheet with a list open.
const TOUCH_DESKTOP = { width: 1440, height: 1100, deviceScaleFactor: 1, mobile: true, screenWidth: 1440, screenHeight: 1100 };

process.on("exit", (code) => { if (!globalThis.__gateSummaryPrinted) console.error(`process exit ${code} before the gate summary`); });
process.on("beforeExit", () => { if (!globalThis.__gateSummaryPrinted) console.error(`event loop drained before the gate summary; active handles: ${(process._getActiveHandles ? process._getActiveHandles() : []).map((h) => h.constructor && h.constructor.name).join(",")}`); });

// PERSEA_WS_RUNTIME_TRACE=1 prints every driver step to stderr, so a run that
// stalls or drains the event loop names the step it was on.
const TRACE = process.env.PERSEA_WS_RUNTIME_TRACE === "1";
const trace = (label) => { if (TRACE) console.error(`[trace ${new Date().toISOString()}] ${label}`); };
if (TRACE) {
  for (const proto of [Tab.prototype, BaseTab.prototype]) {
    for (const key of Object.getOwnPropertyNames(proto)) {
      if (key === "constructor" || typeof proto[key] !== "function") continue;
      const original = proto[key];
      proto[key] = async function traced(...args) {
        trace(`${this.name}.${key}(${args.length > 0 && typeof args[0] === "string" ? args[0].slice(0, 80) : ""})`);
        const out = await original.apply(this, args);
        trace(`${this.name}.${key} done`);
        return out;
      };
    }
  }
}

async function main() {
  const harnessIndex = fs.readFileSync(path.join(UI, "test/workspace_page_browser_harness.html"), "utf8").replace("__PERSEA_STYLE_NONCE__", STYLE_NONCE);
  const fixture = await startWorkspaceFixture(UI, {
    documents: {
      "/workspace-harness": { body: () => harnessIndex },
      "/workspace_page_harness.js": { file: path.join(UI, "dist/test/workspace_page_browser_harness.js"), type: "text/javascript" },
      "/workspace_page_harness.css": { file: path.join(UI, "dist/test/workspace_page_browser_harness.css"), type: "text/css" },
    },
  });
  const origin = fixture.origin;
  const control = async (input) => { trace(`control ${JSON.stringify(input).slice(0, 120)}`); const out = await requestJSON(`${origin}/__fixture/control`, "POST", input); trace("control done"); return out; };
  const snapshot = async () => { trace("snapshot"); const out = await requestJSON(`${origin}/__fixture/control`); trace("snapshot done"); return out; };
  const workspaceURL = (name = WORKSPACE, extra = "") => `${origin}/workspace?engine=unified-dev#name=${encodeURIComponent(name)}${extra}`;
  const arrangementFor = (names) => {
    const leaves = names.map((name) => ({ kind: "leaf", session: { realm: "local", server: "private", name }, on_missing: "offer" }));
    const root = leaves.length === 1 ? leaves[0]
      : leaves.length <= 3 ? { kind: "split", direction: "row", weights: leaves.map(() => 1), children: leaves }
        : { kind: "split", direction: "column", weights: [1, 1], children: [
          { kind: "split", direction: "row", weights: leaves.slice(0, Math.ceil(leaves.length / 2)).map(() => 1), children: leaves.slice(0, Math.ceil(leaves.length / 2)) },
          { kind: "split", direction: "row", weights: leaves.slice(Math.ceil(leaves.length / 2)).map(() => 1), children: leaves.slice(Math.ceil(leaves.length / 2)) },
        ] };
    return JSON.stringify({ version: 1, root });
  };
  const chrome = await launchChrome("persea-ws-runtime-");
  const debugPort = chrome.debugPort;
  const tabs = [];
  const failures = [];
  const evidence = { falsifiers: {} };
  const record = (id, receipt) => { evidence.falsifiers[id] = { ...(evidence.falsifiers[id] || {}), ...receipt }; };
  const fail = (id, message, detail) => { failures.push(`${id}: ${message} ${JSON.stringify(detail)}`); console.error(`FAIL ${id}: ${message}`, JSON.stringify(detail)); };
  const check = (id, condition, message, detail) => { if (!condition) fail(id, message, detail); return condition; };
  const begin = (id) => console.log(`scenario ${id}`);
  // The product's handle/takeover helpers each precede their POST with one
  // CSRF-refresh GET of /api/inventory; workspace snapshot fetches are the
  // inventory GETs that remain once those are subtracted.
  const snapshotFetches = (snap) => snap.counters.inventory - snap.counters.handleRequests - snap.counters.takeovers;

  const openTab = async (name) => { const tab = await Tab.open(name, debugPort, origin); tabs.push(tab); await tab.emulate(DESKTOP, false); return tab; };
  // Seeds the tab's sessionStorage arrangement (a restored tab), then lands
  // on the workspace document without any trusted event.
  const seedArrangement = async (tab, names, workspace = WORKSPACE) => {
    const serialized = JSON.parse(arrangementFor(names));
    await control({ workspace: { name: workspace, tree: serialized.root } });
    await tab.navigate(`${origin}/`);
    // Contradictory W1 bytes remain present but are not workspace authority.
    const ephemeral = arrangementFor([...names].reverse());
    await tab.evaluate(`sessionStorage.setItem(${JSON.stringify(`persea-workspace-ephemeral-v1:${workspace}`)}, ${JSON.stringify(ephemeral)})`);
  };
  const landOn = async (tab, url, expectLanding) => {
    await tab.navigate(url);
    const landed = await tab.waitUntil((s) => (expectLanding === "phone" ? s.phone : s.landing === expectLanding) || s.unavailable !== null || s.fatal !== null, 8_000);
    assert(landed.state, `${tab.name}: never landed on ${url}: ${JSON.stringify(landed.last)}`);
    return landed.state;
  };
  const trustedOpen = async (tab) => {
    const before = await tab.state();
    assert(before.openPoint && !before.openDisabled, `${tab.name}: the Open workspace action is not available: ${JSON.stringify({ landing: before.landing, text: before.openText })}`);
    await tab.trustedClick(before.openPoint);
  };
  const allLive = (s, names = SESSIONS) => s.cells.length === names.length && names.every((name) => s.cells.some((c) => c.session === name && c.state === "live"));
  const waitLive = async (tab, names = SESSIONS, timeout = 15_000) => {
    const result = await tab.waitUntil((s) => allLive(s, names), timeout);
    assert(result.state, `${tab.name}: panes did not all go live: ${JSON.stringify(result.last && result.last.cells.map((c) => [c.session, c.state, c.code]))}`);
    return result.state;
  };
  const cellOf = (s, name) => s.cells.find((c) => c.session === name);
  // A deferral an operator can actually read: the reviewed badge phrase AND
  // the reviewed sentence in the cell's rendered text, carried by a notice
  // that is laid out. A live cell hides its state overlay and its header
  // badge, so `badge`/`headline`/`detail` textContent alone proves only that
  // the detail reached the DOM — the product surface can still be blank.
  const readsDeferral = (cell) => !!cell && cell.readsBadgePhrase && cell.readsDeferralSentence
    && !!cell.deferralNotice && cell.deferralNotice.hidden === false && cell.deferralNotice.display !== "none" && cell.deferralNotice.area > 0;
  const waitFixture = async (predicate, timeout) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) { const snap = await snapshot(); if (predicate(snap)) return snap; await delay(50); }
    return null;
  };
  const attachmentsOf = (snap, name) => snap.attachments.filter((a) => a.session === name);
  const liveAttachmentOf = (snap, name) => attachmentsOf(snap, name).find((a) => a.live);
  const inputsOf = (snap, name) => attachmentsOf(snap, name).flatMap((a) => a.inputs);
  const firstIndex = (frames, prefix) => frames.findIndex((f) => f.startsWith(prefix));
  const permutations = [[0, 1, 2, 3, 4, 5], [5, 4, 3, 2, 1, 0], [2, 5, 0, 4, 1, 3], [3, 0, 5, 1, 4, 2]];
  const holdOrder = async (order, stepMs = 120) => {
    for (let i = 0; i < SESSIONS.length; i += 1) await control({ session: SESSIONS[order[i]], holdPrepareMs: (i + 1) * stepMs });
  };
  const clearHolds = async () => { for (const name of SESSIONS) await control({ session: name, holdPrepareMs: 0 }); };

  const scenarios = [];
    // W1B-F1 — strict route branch and B3 non-mutation.
    scenarios.push(["F1", async () => {
      const id = "W1B-F1";
      await control({ reset: true, sessions: SESSIONS });
      begin(id);
      const tab = await openTab("f1");
      const landings = [];
      // (a) direct navigation with no durable record fails visibly before
      // inventory/attachment work. W2 never falls back to sessionStorage.
      let inventoryBefore = (await snapshot()).counters.inventory;
      let s = await landOn(tab, workspaceURL(), "resume");
      await delay(300);
      landings.push({ url: "missing", unavailable: s.unavailable, landing: s.landing, xterms: s.xtermCount, inventoryFetches: (await snapshot()).counters.inventory - inventoryBefore });
      check(id, s.unavailable === "workspace_not_found", "missing durable workspace did not fail closed", s);
      // (a2) the durable store's typed limiter refusal is visible and does not
      // fall back to the contradictory W1 bytes or begin inventory/transport.
      await seedArrangement(tab, SESSIONS);
      await delay(300); // let the dashboard document finish its own list fetch
      await control({ rateLimit: { path: "/api/workspaces", count: 1 } });
      inventoryBefore = (await snapshot()).counters.inventory;
      s = await landOn(tab, workspaceURL(), "resume");
      await delay(100);
      landings.push({ url: "rate_limited", unavailable: s.unavailable, landing: s.landing, xterms: s.xtermCount, inventoryFetches: (await snapshot()).counters.inventory - inventoryBefore });
      check(id, s.unavailable === "workspace_rate_limited" && s.xtermCount === 0 && s.cells.length === 0, "workspace-store 429 did not render a typed non-attaching refusal", s);
      // (b) a durable record with contradictory W1 sessionStorage: the
      // durable tree alone supplies the resume landing.
      await seedArrangement(tab, SESSIONS);
      inventoryBefore = (await snapshot()).counters.inventory;
      s = await landOn(tab, workspaceURL(), "resume");
      await delay(300);
      landings.push({ url: "resume", landing: s.landing, xterms: s.xtermCount, boxes: s.landingBoxes.filter((b) => b.checked).length, inventoryFetches: (await snapshot()).counters.inventory - inventoryBefore });
      let snap = await snapshot();
      check(id, snap.counters.websockets === 0 && snap.counters.adoptions === 0 && snap.counters.takeovers === 0 && snap.counters.handleRequests === 0 && snap.counters.sessionsCreated === 0
        && snap.attachments.length === 0 && snap.sessions.every((x) => x.handlesConsumed === 0),
      "landing consumed a capability or performed a mutation before any trusted tap", snap.counters);
      check(id, landings[0].inventoryFetches === 0 && landings[1].inventoryFetches === 0 && landings[2].inventoryFetches === 1, "durable failure/load inventory budget", landings);
      check(id, s.xtermCount === 0 && s.cells.length === 0 && s.landing === "resume" && s.landingBoxes.filter((b) => b.checked).length === 6, "resume landing shape", { xterms: s.xtermCount, cells: s.cells.length, landing: s.landing });
      // (c) the dashboard document and a query the dashboard refuses: no capability either.
      for (const url of [`${origin}/`, `${origin}/?resume=1`]) {
        await tab.navigate(url);
        await delay(400);
        const st = await tab.state();
        landings.push({ url, dashboardMode: st.dashboardMode, fatal: st.fatal, xterms: st.xtermCount });
      }
      snap = await snapshot();
      check(id, snap.counters.websockets === 0 && snap.attachments.length === 0, "dashboard/refused documents consumed a capability", snap.counters);
      // (d) route strictness: every non-exact workspace address is a typed refusal in place, never a navigation.
      const refusals = [];
      for (const [url, code] of [
        [`${origin}/workspace#name=ops`, "query_missing"],
        [`${origin}/workspace?engine=legacy#name=ops`, "query_mismatch"],
        [`${origin}/workspace?engine=unified-dev&x=1#name=ops`, "query_mismatch"],
        [`${origin}/workspace?engine=unified-dev&engine=unified-dev#name=ops`, "query_mismatch"],
        [`${origin}/workspace?engine=unified-dev#name=ops&engine=unified-dev`, "fragment_engine_rejected"],
        [`${origin}/workspace?engine=unified-dev#engine=unified-dev&name=ops`, "fragment_engine_rejected"],
        [`${origin}/workspace?engine=unified-dev#name=ops&handle=abc`, "fragment_unknown_key"],
        [`${origin}/workspace?engine=unified-dev`, "name_missing"],
      ]) {
        await tab.navigate(url);
        const st = (await tab.waitUntil((x) => x.unavailable !== null || x.fatal !== null || x.landing !== null, 5_000)).state || await tab.state();
        refusals.push({ url, unavailable: st.unavailable, href: st.href });
        check(id, st.unavailable === code && st.href === url, `route refusal ${code} must render in place without navigation`, { url, got: st.unavailable, href: st.href });
      }
      // (e) FW-R(e): a /terminal document with a fragment engine still self-heals exactly as before.
      await tab.cdp.send("Page.navigate", { url: "about:blank" });
      await delay(50);
      await tab.cdp.send("Page.navigate", { url: `${origin}/terminal#engine=unified-dev` });
      const healed = await tab.waitUntil((x) => x.href === `${origin}/terminal?engine=unified-dev#engine=unified-dev`, 5_000);
      check(id, healed.state !== null, "FW-R(e): /terminal fragment engine self-heal regressed", { last: healed.last && healed.last.href });
      snap = await snapshot();
      check(id, snap.counters.websockets === 0, "refusals/self-heal must open no socket", snap.counters);
      // (f) the trusted tap starts the six-pane load.
      await seedArrangement(tab, SESSIONS);
      s = await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      const live = await waitLive(tab);
      snap = await snapshot();
      check(id, snap.counters.websockets === 6 && snap.attachments.length === 6 && new Set(snap.attachments.map((a) => a.session)).size === 6, "trusted open must produce six attachments on six sessions", snap.counters);
      await tab.screenshot("f1_six_panes_desktop.png");
      record(id, { landings, refusals, healed: healed.state !== null, afterTap: snap.counters, cells: live.cells.map((c) => [c.session, c.state, c.badge]) });
      await tab.close(debugPort);
    }]);
    // W1B-F2 — one controller per pane, zero pre-grant input, permuted COMMIT order and outcomes.
    // W1B-F9 — focus is a monotone veto: designated pane holds focus; six MODE_REQUESTs and grants.
    scenarios.push(["F2", async () => {
      begin("W1B-F2/F9");
      const runs = [];
      for (const order of permutations) {
        await control({ reset: true, sessions: SESSIONS });
        await holdOrder(order);
        const tab = await openTab("f2");
        await seedArrangement(tab, SESSIONS);
        await landOn(tab, workspaceURL(), "resume");
        await trustedOpen(tab);
        const live = await waitLive(tab);
        await delay(200);
        const snap = await snapshot();
        const perPane = SESSIONS.map((name) => {
          const attachments = attachmentsOf(snap, name);
          const a = attachments[0];
          const frames = a ? a.frames : [];
          const modeIndex = firstIndex(frames, "MODE_REQUEST");
          const inputIndex = firstIndex(frames, "INPUT:");
          const resizeIndex = firstIndex(frames, "RESIZE_REQUEST");
          return { session: name, attachments: attachments.length, mode: a && a.mode, modeIndex, inputIndex, resizeIndex, frames: frames.slice(0, 6) };
        });
        const cell = live.cells.map((c) => ({ session: c.session, state: c.state, xterm: c.xterm, pages: c.pages }));
        const designated = live.cells.find((c) => c.designated);
        runs.push({ order: order.map((i) => SESSIONS[i]), perPane, activeCell: live.activeCell, activeTag: live.activeTag, designated: designated && designated.session });
        check("W1B-F2", perPane.every((p) => p.attachments === 1 && p.mode === "CONTROL" && p.modeIndex >= 0 && (p.inputIndex === -1 || p.inputIndex > p.modeIndex) && p.resizeIndex === -1),
          "every pane must hold exactly one attachment with its own CONTROL grant and no INPUT/RESIZE before its MODE_REQUEST", perPane);
        check("W1B-F2", live.xtermCount === 6 && live.pageCount === 6 && cell.every((c) => c.xterm && c.pages === 1), "six distinct page/xterm instances, one per cell", { xterms: live.xtermCount, pages: live.pageCount, cell });
        check("W1B-F9", live.activeCell === SESSIONS[0] && live.activeTag === "TEXTAREA" && designated && designated.session === SESSIONS[0],
          `designated pane ${SESSIONS[0]} must hold focus after every COMMIT order`, { order: order.map((i) => SESSIONS[i]), activeCell: live.activeCell, designated: designated && designated.session });
        await tab.close(debugPort);
      }
      await clearHolds();
      record("W1B-F2", { runs });
      record("W1B-F9", { runs: runs.map((r) => ({ order: r.order, activeCell: r.activeCell, designated: r.designated })) });
    }]);
    // W1B-F2 (outcomes) / W1B-F13 — every connection outcome renders in its own pane; siblings stay live.
    scenarios.push(["F2", async () => {
      const id = "W1B-F13";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      await control({ session: "ws02", projection: "adoptable", adopted: false });
      await control({ session: "ws03", projection: "missing" });
      await control({ session: "ws04", projection: "blocked_alt_screen" });
      await control({ session: "ws05", foreignHolder: true });
      await control({ session: "ws06", closeOnAttach: "stale_target" });
      const tab = await openTab("f13");
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      const settled = await tab.waitUntil((s) => {
        const c = (n) => cellOf(s, n);
        return c("ws01")?.state === "live" && c("ws02")?.state === "live" && c("ws03")?.state === "missing" && c("ws04")?.state === "projection" && c("ws05")?.state === "live" && c("ws06")?.state === "failed";
      }, 15_000);
      assert(settled.state, `${id}: outcomes did not settle: ${JSON.stringify(settled.last && settled.last.cells.map((c) => [c.session, c.state, c.code]))}`);
      const s = settled.state;
      const snap = await snapshot();
      const ws05 = snap.sessions.find((x) => x.name === "ws05");
      check(id, snap.counters.adoptions === 1 && snap.sessions.find((x) => x.name === "ws02").adopted === true, "adoptable pane must adopt exactly once before attaching", snap.counters);
      check(id, ws05.displacedForeign === 1 && attachmentsOf(snap, "ws05").length === 2 && attachmentsOf(snap, "ws05")[0].closeReason === "lease_held" && attachmentsOf(snap, "ws05")[1].takeover === true,
        "lease_held pane must auto-take control exactly once (its own budget), replacing its own socket only", { ws05, attachments: attachmentsOf(snap, "ws05").map((a) => [a.id, a.closeReason, a.takeover]) });
      check(id, s.cells.every((c) => c.badge.length > 0 && (c.state === "live" || (c.headline.length > 0 && c.detail.length > 0 && c.actions.length > 0))), "every non-live pane renders headline, detail, and labelled actions", s.cells.map((c) => [c.session, c.state, c.headline, c.actions.map((a) => a.affordance)]));
      check(id, cellOf(s, "ws06").code === "stale_target" && cellOf(s, "ws04").code === "blocked_alt_screen", "typed codes must be visible", { ws06: cellOf(s, "ws06").code, ws04: cellOf(s, "ws04").code });
      check(id, ["ws01", "ws02", "ws05"].every((n) => attachmentsOf(snap, n).some((a) => a.live)), "sibling panes must stay live while others render failures", snap.sessions.map((x) => [x.name, x.liveAttachments]));
      // The missing pane's Create affordance creates that session and attaches it; nothing else moves.
      const before = snap.attachments.map((a) => [a.id, a.live]);
      const create = cellOf(s, "ws03").actions.find((a) => a.affordance === "create");
      assert(create && create.point, `${id}: create affordance missing on the missing pane`);
      await tab.trustedClick(create.point);
      const created = await tab.waitUntil((x) => cellOf(x, "ws03")?.state === "live", 10_000);
      check(id, created.state !== null, "Create session must attach the created session into its own pane", created.last && cellOf(created.last, "ws03"));
      const after = await snapshot();
      check(id, after.counters.sessionsCreated === 1 && after.counters.inventory === snap.counters.inventory + 1 && before.every(([idx, live]) => !live || after.attachments.find((a) => a.id === idx).live),
        "creation must refresh the shared snapshot once and touch no sibling socket", { before: snap.counters, after: after.counters });
      // An in-band END ("closed" is what the front sends when the session's
      // epoch ends) must take the pane out of live with the shared
      // classification and a visible code; siblings stay live.
      await control({ session: "ws05", endLive: "closed" });
      const ended = await tab.waitUntil((x) => cellOf(x, "ws05")?.state !== "live", 5_000);
      check(id, ended.state !== null && cellOf(ended.state, "ws05").headline.length > 0 && cellOf(ended.state, "ws05").badge.length > 0 && ["ws01", "ws02", "ws03"].every((n) => cellOf(ended.state, n).state === "live"),
        "an in-band END must end the pane through the shared policy and leave siblings live", ended.last && ended.last.cells.map((c) => [c.session, c.state, c.code]));
      await tab.screenshot("f13_mixed_outcomes_desktop.png");
      record(id, { cells: s.cells.map((c) => [c.session, c.state, c.code, c.actions.map((a) => a.affordance)]), counters: after.counters });
      await tab.close(debugPort);
    }]);
    // W1B-F3 — input and action isolation; W1B-F10 (fixture form) — ↕ per pane, zero implicit resize.
    // W1B-F15 — divider commits mutate nothing; hidden action container; 44×44 census on coarse.
    scenarios.push(["F3", async () => {
      begin("W1B-F3/F10/F15");
      await control({ reset: true, sessions: SESSIONS });
      // 80×60 sessions: in a cell the presentation fit lands below 60 rows, so an explicit ↕ has a real request to make.
      for (const name of SESSIONS) await control({ session: name, rows: 60 });
      const tab = await openTab("f3");
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      let s = await waitLive(tab);
      const typed = {};
      for (const name of SESSIONS) {
        const cell = cellOf(s, name);
        await tab.trustedClick({ x: cell.screen.x, y: cell.screen.y });
        await delay(60);
        const focused = await tab.state();
        check("W1B-F3", focused.activeCell === name, `clicking pane ${name} must move focus there`, { activeCell: focused.activeCell });
        await tab.typeText(`t-${name};`);
        typed[name] = `t-${name};`;
      }
      await delay(300);
      let snap = await snapshot();
      const inputs = Object.fromEntries(SESSIONS.map((name) => [name, inputsOf(snap, name).join("")]));
      check("W1B-F3", SESSIONS.every((name) => inputs[name] === typed[name]), "INPUT must appear only on the focused pane's own socket", inputs);
      check("W1B-F3", snap.attachments.length === 6 && snap.attachments.every((a) => a.live), "focus moves must not reconnect any pane", snap.attachments.map((a) => [a.id, a.session, a.live]));
      // Composer injection on one pane.
      s = await tab.state();
      const target = cellOf(s, "ws04");
      if (target.composerPoint) {
        await tab.trustedClick(target.composerPoint);
        await delay(150);
        const withComposer = await tab.evaluate(`(() => { const c = document.querySelector('.ws-cell[data-ws-session="ws04"] .attachment-page__composer textarea, .ws-cell[data-ws-session="ws04"] textarea:not(.xterm-helper-textarea)'); if (!c) return null; c.focus(); const r = c.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
        if (withComposer) {
          await tab.typeText("composer-ws04");
          const insert = await tab.evaluate(`(() => { const b = document.querySelector('.ws-cell[data-ws-session="ws04"] button[title="Insert into terminal without running it"]'); if (!b) return null; const r = b.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
          if (insert) await tab.trustedClick(insert);
          await delay(200);
          snap = await snapshot();
          const others = SESSIONS.filter((n) => n !== "ws04").map((n) => inputsOf(snap, n).join(""));
          const composerState = cellOf(await tab.state(), "ws04");
          check("W1B-F3", inputsOf(snap, "ws04").join("").includes("composer-ws04") && others.every((v) => !v.includes("composer")), "composer injection must land on its own pane only", { ws04: inputsOf(snap, "ws04").join(""), others, insert, composer: composerState.composer, activeCell: composerState.activeCell });
        } else {
          fail("W1B-F3", "the composer textarea did not render inside the pane's cell", cellOf(await tab.state(), "ws04").composer);
        }
      }
      // Fixture-form F10: layout storm → zero RESIZE_REQUEST; then ↕ per pane → exactly one each, columns unchanged.
      s = await tab.state();
      const resizeBefore = snap.attachments.map((a) => a.resizes);
      for (const divider of s.dividers) {
        const from = { x: divider.point.x, y: divider.point.y };
        const delta = divider.orientation === "vertical" ? { x: 40, y: 0 } : { x: 0, y: 30 };
        await tab.pointerDrag(from, { x: from.x + delta.x, y: from.y + delta.y }, 10);
        await tab.pointerDrag({ x: from.x + delta.x, y: from.y + delta.y }, from, 10);
      }
      const requestsBeforeStorm = (await snapshot()).counters.requests;
      const ephemeralBefore = (await tab.state()).ephemeral;
      for (let i = 0; i < 10; i += 1) {
        const d = (await tab.state()).dividers[0];
        await tab.pointerDrag({ x: d.point.x, y: d.point.y }, { x: d.point.x + (i % 2 ? 12 : -12), y: d.point.y }, 3);
      }
      await tab.emulate({ ...DESKTOP, width: 1000, height: 700 }, false);
      await delay(150);
      await tab.emulate(DESKTOP, false);
      await delay(150);
      snap = await snapshot();
      const afterState = await tab.state();
      check("W1B-F10", snap.attachments.every((a, i) => a.resizes === resizeBefore[i]) && snap.attachments.every((a) => a.frames.every((f) => !f.startsWith("RESIZE_REQUEST"))), "layout storm emitted a RESIZE_REQUEST", snap.attachments.map((a) => [a.session, a.resizes]));
      check("W1B-F15", snap.counters.requests === requestsBeforeStorm && afterState.ephemeral === ephemeralBefore && snap.attachments.every((a) => a.live),
        "divider commits must perform no network, attachment, or persistence mutation", { requestsBefore: requestsBeforeStorm, requestsAfter: snap.counters.requests, ephemeralChanged: afterState.ephemeral !== ephemeralBefore });
      check("W1B-F15", afterState.cells.every((c) => c.state !== "live" || (c.actionsHiddenAttr === true && c.actionsHiddenDisplay === "none")), "a hidden state-action container must compute display:none", afterState.cells.map((c) => [c.session, c.actionsHiddenAttr, c.actionsHiddenDisplay]));
      check("W1B-F15", afterState.resizeCount > 0, "the window resize storm must have fired resize events (witness)", { resizeCount: afterState.resizeCount });
      const fits = [];
      for (const name of SESSIONS) {
        const st = await tab.state();
        let cell = cellOf(st, name);
        assert(cell.viewPoint, `W1B-F10: View and appearance is not available on live pane ${name}`);
        await tab.trustedClick(cell.viewPoint);
        await delay(100);
        cell = cellOf(await tab.state(), name);
        assert(cell.fitPoint, `W1B-F10: ↕ is not available on live pane ${name}: ${JSON.stringify({ disabled: cell.fitDisabled })}`);
        if (name === SESSIONS[0]) await tab.screenshot("ux11_workspace_view_popover.png");
        assert(cell.fitHit, `W1B-F10: ↕ is covered by ${cell.fitHitBy} before activation on live pane ${name}: ${JSON.stringify(cell.fitDiagnostics)}`);
        await tab.trustedClick(cell.fitPoint);
        await delay(150);
        const sn = await snapshot();
        const after = cellOf(await tab.state(), name);
        fits.push({ session: name, resizes: sn.attachments.map((a) => [a.session, a.resizes]), refusal: after.refusal, geometry: after.geometry, fontSize: after.fontSize, viewportHeight: after.viewportHeight, rowCount: after.rowCount, fitDisabled: after.fitDisabled });
      }
      snap = await snapshot();
      const resizeFrames = Object.fromEntries(SESSIONS.map((n) => [n, attachmentsOf(snap, n)[0].frames.filter((f) => f.startsWith("RESIZE_REQUEST"))]));
      check("W1B-F10", SESSIONS.every((n) => attachmentsOf(snap, n)[0].resizes === 1 && resizeFrames[n].length === 1 && resizeFrames[n][0].startsWith("RESIZE_REQUEST:80x")), "each ↕ must produce exactly one RESIZE_REQUEST on its own pane with columns unchanged", resizeFrames);
      record("W1B-F3", { inputs, attachments: snap.attachments.map((a) => [a.id, a.session, a.live]) });
      record("W1B-F10", { fixtureForm: true, resizeFrames, fits, storm: { dividers: s.dividers.length, resizeEvents: afterState.resizeCount } });
      // Coarse-pointer census on a tablet after the live mount (F15), with the sheet open on one pane.
      await tab.emulate(TABLET, true);
      await delay(300);
      const tablet = await tab.state();
      const small = tablet.touchTargets.filter((t) => (t.w < 44 || t.h < 44) && !/xterm/.test(t.cls));
      check("W1B-F15", tablet.cells.length === 6 && tablet.xtermCount === 6, "a coarse tablet keeps the six live panes", { cells: tablet.cells.length, xterms: tablet.xtermCount, phone: tablet.phone });
      check("W1B-F15", small.filter((t) => /^ws-/.test(t.cls)).length === 0, "every workspace coarse-pointer control must measure >= 44x44 after the live mount", small);
      // W1B-R4: the state-action census is causal only when state actions are
      // VISIBLE. Under the same coarse profile, end two panes into visible
      // action states (a typed failure with Retry/Dashboard, a displacement
      // with Take control) and measure every visible .ws-cell__action.
      await control({ session: "ws06", closeLive: "stale_target" });
      await control({ session: "ws05", closeLive: "control_displaced" });
      const acted = await tab.waitUntil((x) => cellOf(x, "ws06")?.state === "failed" && cellOf(x, "ws05")?.state === "displaced" && cellOf(x, "ws06").actions.length > 0 && cellOf(x, "ws05").actions.length > 0, 8_000);
      assert(acted.state, `W1B-F15: panes did not reach visible action states: ${JSON.stringify(acted.last && acted.last.cells.map((c) => [c.session, c.state, c.actions.length]))}`);
      const actionTargets = acted.state.touchTargets.filter((t) => t.cls === "ws-cell__action");
      const smallActions = actionTargets.filter((t) => t.w < 44 || t.h < 44);
      check("W1B-F15", actionTargets.length >= 3 && smallActions.length === 0, "every VISIBLE state action on a coarse pointer must measure >= 44x44", { visibleActions: actionTargets.map((t) => [t.label, t.w, t.h]), small: smallActions });
      check("W1B-F15", acted.state.cells.filter((c) => c.state === "live").every((c) => c.actionsHiddenAttr === true && c.actionsHiddenDisplay === "none"), "live panes keep their action container hidden with display:none", acted.state.cells.map((c) => [c.session, c.state, c.actionsHiddenDisplay]));
      record("W1B-F15", { requestsDuringStorm: snap.counters.requests - requestsBeforeStorm, coarseCensusUnder44: small, coarseTargets: tablet.touchTargets.length, visibleStateActions: actionTargets.map((t) => [t.label, Math.round(t.w), Math.round(t.h)]) });
      await tab.screenshot("f15_tablet_coarse_six_panes.png");
      await tab.close(debugPort);
    }]);
    // W1B-F4 (fixture form) — typed lag eviction on one pane: exactly one reattach, siblings untouched, sentinels in order.
    scenarios.push(["F4", async () => {
      const id = "W1B-F4";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      const tab = await openTab("f4");
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      await waitLive(tab);
      const before = await snapshot();
      const beforeIds = Object.fromEntries(SESSIONS.map((n) => [n, attachmentsOf(before, n).map((a) => a.id)]));
      for (let i = 1; i <= 3; i += 1) await control({ writeLiveAll: `SENTINEL-${i}` });
      // The triggering event lands in the journal as the wedged pane is evicted.
      await control({ session: "ws04", writeLive: "TRIGGER-EVENT" });
      await control({ session: "ws04", closeLive: "subscriber_lagged" });
      for (let i = 4; i <= 6; i += 1) await control({ writeLiveAll: `SENTINEL-${i}` });
      const reattached = await tab.waitUntil((s) => cellOf(s, "ws04")?.state === "live" && cellOf(s, "ws04").rows.includes("SENTINEL-6"), 15_000);
      assert(reattached.state, `${id}: the evicted pane did not reattach: ${JSON.stringify(reattached.last && cellOf(reattached.last, "ws04"))}`);
      await control({ writeLiveAll: "SENTINEL-7" });
      const done = await tab.waitUntil((s) => SESSIONS.every((n) => cellOf(s, n).rows.includes("SENTINEL-7")), 8_000);
      assert(done.state, `${id}: post-reconnect sentinel did not arrive everywhere`);
      const after = await snapshot();
      const ws04 = attachmentsOf(after, "ws04");
      const siblingsUnchanged = SESSIONS.filter((n) => n !== "ws04").every((n) => JSON.stringify(attachmentsOf(after, n).map((a) => a.id)) === JSON.stringify(beforeIds[n]) && attachmentsOf(after, n).every((a) => a.live));
      check(id, ws04.length === 2 && ws04[0].closeReason === "subscriber_lagged" && ws04[1].live && ws04[1].replayLines >= 4, "the wedged pane must receive one typed close and reattach exactly once with the triggering event in its replay", ws04.map((a) => [a.id, a.closeReason, a.live, a.replayLines]));
      check(id, siblingsUnchanged, "sibling panes must keep their attachments and never reconnect", after.sessions.map((x) => [x.name, x.attachments]));
      const s = done.state;
      const order = (rows) => [1, 2, 3, 4, 5, 6, 7].map((i) => rows.indexOf(`SENTINEL-${i}`));
      const inOrder = (idx) => idx.every((v, i) => v >= 0 && (i === 0 || v > idx[i - 1]));
      check(id, SESSIONS.every((n) => inOrder(order(cellOf(s, n).rows))), "every pane must show every sentinel in order", Object.fromEntries(SESSIONS.map((n) => [n, order(cellOf(s, n).rows)])));
      check(id, cellOf(s, "ws04").rows.includes("TRIGGER-EVENT") && (cellOf(s, "ws04").rows.match(/SENTINEL-7/g) || []).length === 1, "the wedged pane must replay the triggering event and receive the post-reconnect sentinel exactly once", { rows: cellOf(s, "ws04").rows.slice(0, 400) });
      const handleRequestsForWs04 = after.sessions.find((x) => x.name === "ws04").handlesMinted;
      record(id, { fixtureForm: true, ws04: ws04.map((a) => [a.id, a.closeReason, a.live, a.replayLines]), siblings: after.sessions.map((x) => [x.name, x.attachments]), inventoryRequests: after.counters.inventory - before.counters.inventory, handlesMintedWs04: handleRequestsForWs04, activeCell: s.activeCell });
      check(id, ws04[1].mintedBy === "attachment-handles" && snapshotFetches(after) === snapshotFetches(before), "a source-bound reattach must mint from its own binding and not touch the shared inventory snapshot", { mintedBy: ws04[1].mintedBy, snapshotsBefore: snapshotFetches(before), snapshotsAfter: snapshotFetches(after) });
      await tab.close(debugPort);
    }]);
    // W1B-F5 — pinned identity: A dies, same-name B appears, automatic reconnect ends session_gone; B is never attached.
    scenarios.push(["F5", async () => {
      const id = "W1B-F5";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      const tab = await openTab("f5");
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      await waitLive(tab);
      const before = await snapshot();
      const pinnedKey = fixture.draftScopeFor("ws03");
      // Expire the binding and the initial handle (already consumed), kill A, create B with the same name, then drop the socket silently.
      await control({ session: "ws03", expireBindings: true });
      await control({ session: "ws03", kill: true, killReason: "" });
      await control({ createSession: "ws03" });
      const decoyKey = fixture.draftScopeFor("ws03");
      const ended = await tab.waitUntil((s) => cellOf(s, "ws03")?.state === "failed" && cellOf(s, "ws03").code === "session_gone", 40_000);
      assert(ended.state, `${id}: pane did not end session_gone: ${JSON.stringify(ended.last && cellOf(ended.last, "ws03"))}`);
      const after = await snapshot();
      const decoy = after.sessions.find((x) => x.name === "ws03" && x.created);
      check(id, decoyKey !== pinnedKey, "the decoy must be a different incarnation", { pinnedKey, decoyKey });
      check(id, decoy && decoy.handlesConsumed === 0 && decoy.attachments.length === 0, "no handle for the same-name successor may be consumed and no attachment opened", decoy);
      check(id, snapshotFetches(after) === snapshotFetches(before) + 1, "the identity re-mint must read exactly one fresh snapshot", { before: snapshotFetches(before), after: snapshotFetches(after), counters: after.counters });
      check(id, SESSIONS.filter((n) => n !== "ws03").every((n) => attachmentsOf(after, n).length === 1 && attachmentsOf(after, n)[0].live), "siblings must be untouched by one pane's identity outcome", after.sessions.map((x) => [x.name, x.attachments]));
      // The explicit Retry re-resolves by name (new operator intent) and may reach B.
      const retry = cellOf(ended.state, "ws03").actions.find((a) => a.affordance === "retry");
      assert(retry && retry.point, `${id}: retry affordance missing`);
      await tab.trustedClick(retry.point);
      const rebound = await tab.waitUntil((s) => cellOf(s, "ws03")?.state === "live", 15_000);
      check(id, rebound.state !== null, "an explicit retry is new operator intent and may attach the successor", rebound.last && cellOf(rebound.last, "ws03"));
      const rebindSnap = await snapshot();
      check(id, rebindSnap.sessions.find((x) => x.name === "ws03" && x.created).handlesConsumed === 1, "the explicit rebind consumes exactly one successor handle", rebindSnap.sessions.find((x) => x.name === "ws03" && x.created));
      record(id, { pinnedKey, decoyKey, decoyBeforeRetry: decoy, cellAfterAuto: cellOf(ended.state, "ws03").code, inventoryRequests: after.counters.inventory - before.counters.inventory });
      await tab.close(debugPort);
    }]);
    // W1B-F6 — a second tab takes over every pane exactly once; the first tab is displaced and never counter-takes.
    scenarios.push(["F6", async () => {
      const id = "W1B-F6";
      begin(id);
      const runs = [];
      for (const order of permutations.slice(0, 2)) {
        await control({ reset: true, sessions: SESSIONS });
        const tabA = await openTab("f6a");
        await seedArrangement(tabA, SESSIONS);
        await landOn(tabA, workspaceURL(), "resume");
        await trustedOpen(tabA);
        await waitLive(tabA);
        const mid = await snapshot();
        await holdOrder(order, 80);
        const tabB = await openTab("f6b");
        await seedArrangement(tabB, SESSIONS);
        await landOn(tabB, workspaceURL(), "resume");
        await trustedOpen(tabB);
        await waitLive(tabB, SESSIONS, 25_000);
        const displaced = await tabA.waitUntil((s) => SESSIONS.every((n) => cellOf(s, n)?.state === "displaced" && cellOf(s, n).code === "control_displaced"), 15_000);
        if (!displaced.state) {
          const probe = await snapshot();
          assert(false, `${id}: tab A panes were not all displaced: ${JSON.stringify({ cells: displaced.last && displaced.last.cells.map((c) => [c.session, c.state, c.code]), attachments: probe.attachments.map((a) => [a.id, a.session, a.takeover, a.live, a.closeReason, a.committed]), counters: probe.counters, consoleA: tabA.console.slice(-12), consoleB: tabB.console.slice(-12) })}`);
        }
        await delay(500);
        const after = await snapshot();
        const perSession = SESSIONS.map((n) => {
          const attachments = attachmentsOf(after, n);
          return { session: n, ids: attachments.map((a) => [a.id, a.closeReason, a.takeover, a.live]) };
        });
        const takeoversAfterA = after.counters.takeovers - mid.counters.takeovers;
        check(id, takeoversAfterA === 6 && perSession.every((p) => p.ids.length === 3 && p.ids[0][1] === "control_displaced" && p.ids[1][1] === "lease_held" && p.ids[2][2] === true && p.ids[2][3] === true),
          "tab B must perform six exact A→B takeovers, one per pane; tab A receives six control_displaced and performs zero counter-takeovers", { takeovers: takeoversAfterA, perSession });
        check(id, after.sessions.every((x) => x.leaseAttachment !== null && after.attachments.find((a) => a.id === x.leaseAttachment).takeover === true), "every lease must end with tab B's own takeover attachment", after.sessions.map((x) => [x.name, x.leaseAttachment]));
        runs.push({ order: order.map((i) => SESSIONS[i]), takeovers: takeoversAfterA, perSession });
        await clearHolds();
        await tabA.close(debugPort);
        await tabB.close(debugPort);
      }
      record(id, { runs });
    }]);
    // W1B-F7 — single-flight inventory after sleep; lease_held ×6; adoption ×6; one injected 429 delays one pane only.
    scenarios.push(["F7", async () => {
      const id = "W1B-F7";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      const tab = await openTab("f7");
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      await waitLive(tab);
      const before = await snapshot();
      // Sleep: every binding expired, every socket dropped at once. Recovery
      // is judged on the fixture (six new upgrades, each session live again)
      // before the cells are read, so a poll cannot catch the pre-drop state.
      await control({ expireAllBindings: true });
      await control({ closeAllLive: "" });
      const recovered = await waitFixture((snap) => snap.counters.websockets >= before.counters.websockets + 6 && snap.sessions.every((x) => x.liveAttachments.length === 1), 40_000);
      assert(recovered, `${id}: the fixture never saw six recovered attachments: ${JSON.stringify((await snapshot()).sessions.map((x) => [x.name, x.liveAttachments]))}`);
      const back = await tab.waitUntil((s) => allLive(s), 15_000);
      assert(back.state, `${id}: panes did not all recover after sleep: ${JSON.stringify(back.last && back.last.cells.map((c) => [c.session, c.state, c.code]))}`);
      await delay(300);
      const after = await snapshot();
      const consumed = after.sessions.map((x) => [x.name, x.handlesConsumed, x.consumedFrom]);
      const identityProvenance = new Set(after.sessions.map((x) => x.consumedFrom[1]));
      check(id, snapshotFetches(after) === snapshotFetches(before) + 1 && identityProvenance.size === 1, "six simultaneous identity re-mints must share exactly one inventory snapshot", { before: snapshotFetches(before), after: snapshotFetches(after), identityProvenance: [...identityProvenance], counters: after.counters });
      check(id, after.sessions.every((x) => x.handlesConsumed === 2) && after.attachments.length === 12, "every pane must consume one distinct fresh handle from the shared snapshot", { consumed, attachments: after.attachments.length });
      check(id, after.counters.handleRequests >= 6, "source-bound re-mints stay per pane (each pane tried its own binding first)", after.counters);
      const sleep = { snapshotFetches: snapshotFetches(after) - snapshotFetches(before), identityProvenance: [...identityProvenance], inventoryGets: after.counters.inventory - before.counters.inventory, handleRequests: after.counters.handleRequests - before.counters.handleRequests, consumed };
      await tab.close(debugPort);

      // Six lease_held initial attempts, six takeovers, six replacement upgrades, six adoptions, and one injected 429 (on ws05's adoption).
      await control({ reset: true, sessions: SESSIONS });
      for (const name of SESSIONS) await control({ session: name, projection: "adoptable", adopted: false, foreignHolder: true });
      await control({ rateLimit: { path: "/api/session-adoptions", session: "ws05", count: 1 } });
      const tab2 = await openTab("f7b");
      await seedArrangement(tab2, SESSIONS);
      // The dashboard visit above fetched its own inventory; the workspace's
      // single snapshot is the delta from here.
      const preLanding = await snapshot();
      await landOn(tab2, workspaceURL(), "resume");
      const t0 = Date.now();
      await trustedOpen(tab2);
      const sawRateLimited = await tab2.waitUntil((s) => cellOf(s, "ws05")?.state === "rate_limited", 5_000);
      await waitLive(tab2, SESSIONS, 25_000);
      const cold = await snapshot();
      const perSession = SESSIONS.map((n) => ({ session: n, attachments: attachmentsOf(cold, n).map((a) => [a.closeReason, a.takeover, a.live]), adopted: cold.sessions.find((x) => x.name === n).adopted, displaced: cold.sessions.find((x) => x.name === n).displacedForeign }));
      check(id, cold.counters.adoptions === 7 && cold.counters.takeovers === 6 && cold.counters.websockets === 12 && snapshotFetches(cold) - snapshotFetches(preLanding) === 1,
        "cold load: six adoptions (+1 retry after the 429), six takeovers, twelve upgrades, one inventory snapshot", { counters: cold.counters, snapshotFetches: snapshotFetches(cold) - snapshotFetches(preLanding) });
      check(id, perSession.every((p) => p.attachments.length === 2 && p.attachments[0][0] === "lease_held" && p.attachments[1][1] === true && p.attachments[1][2] === true && p.displaced === 1), "every pane: one lease_held refusal, one takeover replacement, its own foreign holder displaced", perSession);
      check(id, sawRateLimited.state !== null, "the 429-charged pane must render rate_limited while it backs off", { last: sawRateLimited.last && cellOf(sawRateLimited.last, "ws05") });
      record(id, { sleep, cold: { counters: cold.counters, perSession, rateLimitedSeen: sawRateLimited.state !== null, elapsedMs: Date.now() - t0 } });
      await tab2.close(debugPort);
    }]);
    // W1B-F9 (coarse half) — on a coarse pointer the designated policy never PROMOTES focus: E-P1's pointer rule vetoes first.
    scenarios.push(["F9", async () => {
      const id = "W1B-F9";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      const tab = await openTab("f9c");
      await tab.emulate(TABLET, true);
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      const s0 = await tab.state();
      await tab.tap(s0.openPoint);
      const live = await waitLive(tab);
      await delay(200);
      const snap = await snapshot();
      check(id, live.activeCell === null && live.activeTag !== "TEXTAREA", "a coarse-pointer COMMIT must not raise focus (no keyboard) even on the designated pane", { activeCell: live.activeCell, activeTag: live.activeTag });
      check(id, SESSIONS.every((n) => attachmentsOf(snap, n)[0].mode === "CONTROL"), "every pane still receives its CONTROL grant regardless of focus", snap.attachments.map((a) => [a.session, a.mode]));
      record(id, { coarse: { activeCell: live.activeCell, activeTag: live.activeTag, modes: snap.attachments.map((a) => [a.session, a.mode]) } });
      // Clicking another pane transfers focus without reconnecting; input goes to its already-Control socket.
      await tab.clearEmulation();
      await tab.emulate(DESKTOP, false);
      await delay(200);
      const st = await tab.state();
      const cell = cellOf(st, "ws03");
      await tab.trustedClick({ x: cell.screen.x, y: cell.screen.y });
      await delay(80);
      await tab.typeText("k");
      await delay(200);
      const after = await snapshot();
      const moved = await tab.state();
      check(id, moved.activeCell === "ws03" && after.attachments.length === 6 && inputsOf(after, "ws03").join("") === "k" && SESSIONS.filter((n) => n !== "ws03").every((n) => inputsOf(after, n).length === 0),
        "clicking another pane transfers focus without reconnecting and input stays on its own Control socket", { activeCell: moved.activeCell, attachments: after.attachments.length, inputs: after.sessions.map((x) => [x.name, inputsOf(after, x.name).join("")]) });
      await tab.close(debugPort);
    }]);
    // W1B-F11 (fixture form) — one document nonce, six xterms, zero CSP violations, no unnonced style/script.
    scenarios.push(["F11", async () => {
      const id = "W1B-F11";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      const tab = await openTab("f11");
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      const live = await waitLive(tab);
      await delay(300);
      const s = await tab.state();
      const nonced = s.styleNodes.filter((n) => n.nonce === s.metaNonce).length;
      const applied = s.styleNodes.filter((n) => n.rules > 0).length;
      check(id, s.metaNonces === 1 && s.styleNodes.length >= 6 && nonced === s.styleNodes.length && applied === s.styleNodes.length && s.unnoncedScripts === 0 && s.cspViolations === 0 && live.xtermCount === 6,
        "every xterm style node must carry the one document nonce and apply, with zero CSP violations", { metaNonces: s.metaNonces, styles: s.styleNodes.length, nonced, applied, unnoncedScripts: s.unnoncedScripts, violations: s.cspViolations });
      record(id, { fixtureForm: true, metaNonces: s.metaNonces, styleNodes: s.styleNodes.length, nonced, applied, violations: s.cspViolations, inlineStyleAttrs: s.inlineStyleAttrs });
      await tab.close(debugPort);
    }]);
    // W1B-F12 — honest phone posture and keyboard stability.
    scenarios.push(["F12", async () => {
      const id = "W1B-F12";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      await control({ session: "ws03", projection: "missing" });
      const tab = await openTab("f12");
      await tab.emulate(PHONE, true);
      await seedArrangement(tab, SESSIONS);
      let s = await landOn(tab, workspaceURL(), "phone");
      let snap = await snapshot();
      check(id, s.phone && s.cells.length === 0 && s.xtermCount === 0 && s.phoneLeaves.length === 6 && snap.counters.websockets === 0 && s.phoneNotice === "workspace view is not available on this device yet",
        "phone class must render the honest state with zero xterms and zero transports", { phone: s.phone, cells: s.cells.length, xterms: s.xtermCount, leaves: s.phoneLeaves.length, ws: snap.counters.websockets, notice: s.phoneNotice });
      const singleTerminalLink = (href) => { if (!href) return false; const u = new URL(href, origin); return u.pathname === "/terminal" && u.search === "?engine=unified-dev" && /(^#|&)handle=/.test(u.hash) && /mode=control/.test(u.hash); };
      check(id, s.phoneLeaves.filter((l) => singleTerminalLink(l.href)).length === 5 && s.phoneLeaves.find((l) => l.session === "ws03").state === "missing" && s.phoneLeaves.find((l) => l.session === "ws03").href === null,
        "each resolved leaf links to its single terminal; the missing leaf shows its state and no dead link", s.phoneLeaves);
      check(id, s.phoneLeaves.every((l) => l.href === null || l.linkHeight >= 44), "phone links must be >= 44 px tall", s.phoneLeaves.map((l) => l.linkHeight));
      // J-W1B-2: a card on the honest-state page never carries a transport
      // state that does not exist; a linkable session says it is not opened here.
      check(id, s.phoneLeaves.every((l) => !/connect|attach|live|reattach/i.test(l.stateText) && !/connecting|live|reconnecting|reattaching/.test(l.state)) && s.phoneLeaves.filter((l) => l.href !== null).every((l) => l.state === "not_opened"),
        "phone cards must show the honest posture, never a transport state", s.phoneLeaves.map((l) => [l.session, l.state, l.stateText]));
      await tab.screenshot("f12_phone_honest_state.png");
      // Rotation keeps the phone posture; a fine pointer at phone width is desktop; a coarse tablet is desktop.
      await tab.emulate(PHONE_LANDSCAPE, true);
      await delay(200);
      const rotated = await tab.state();
      check(id, rotated.phone && rotated.xtermCount === 0, "rotating the phone must keep the honest state", { phone: rotated.phone });
      await tab.emulate(PHONE, false);
      s = await landOn(tab, workspaceURL(), "resume");
      check(id, !s.phone && s.landing === "resume", "a fine pointer at phone width is desktop", { phone: s.phone, landing: s.landing });
      await tab.emulate(TABLET, true);
      s = await landOn(tab, workspaceURL(), "resume");
      check(id, !s.phone && s.landing === "resume", "a coarse tablet above the threshold is desktop", { phone: s.phone, landing: s.landing });
      // Keyboard stability: with six panes live on the tablet, a keyboard-shrunk viewport (1024×350) must not tear down or instantiate anything.
      await control({ reset: true, sessions: SESSIONS });
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      const s1 = await tab.state();
      await tab.tap(s1.openPoint);
      await waitLive(tab);
      const stable = await snapshot();
      for (let i = 0; i < 3; i += 1) {
        await tab.emulate(TABLET_KEYBOARD, true);
        await tab.evaluate("window.dispatchEvent(new Event('orientationchange')); window.dispatchEvent(new Event('resize')); true");
        await delay(200);
        await tab.emulate(TABLET, true);
        await tab.evaluate("window.dispatchEvent(new Event('orientationchange')); window.dispatchEvent(new Event('resize')); true");
        await delay(200);
      }
      await tab.emulate({ ...TABLET, width: 768, height: 1024 }, true);
      await delay(200);
      const after = await tab.state();
      snap = await snapshot();
      check(id, !after.phone && after.xtermCount === 6 && after.cells.length === 6 && snap.counters.websockets === stable.counters.websockets && snap.attachments.every((a) => a.live) && after.cells.every((c) => c.state === "live"),
        "keyboard/viewport/orientation changes must neither tear down nor instantiate controllers", { phone: after.phone, xterms: after.xtermCount, wsBefore: stable.counters.websockets, wsAfter: snap.counters.websockets, cells: after.cells.map((c) => c.state) });
      record(id, { phoneLeaves: s.phoneLeaves, rotationKeepsPhone: rotated.phone, keyboard: { wsBefore: stable.counters.websockets, wsAfter: snap.counters.websockets, xterms: after.xtermCount } });
      await tab.screenshot("f12_tablet_after_keyboard_storm.png");
      await tab.close(debugPort);
    }]);
    // M11LF-F3A/F3B/F3C — inventory detail reaches the exact mounted
    // controller as presentation only, clears without transport action, and
    // the ordinary typed rotation close still causes one reattach.
    scenarios.push(["M11LF-F3", async () => {
      const id = "M11LF-F3";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      const tab = await openTab("m11lf-f3");
      await tab.navigate(`${origin}/workspace-harness`);
      const ready = await tab.waitUntil((s) => s.harnessReady, 8_000);
      assert(ready.state, `${id}: the workspace page harness never became ready`);
      await tab.evaluate(`window.__wsPage.open(${JSON.stringify(arrangementFor(SESSIONS))})`);
      await waitLive(tab);
      const before = await snapshot();
      const beforePanes = await tab.evaluate("window.__wsPage.panes()");
      await tab.evaluate("window.__wsPage.savePresentationSnapshot()");

      // Inventory presentation and shared keyboard preferences each poll once
      // per document, even with six panes. Hold both timers outside exact-count
      // windows, then prove one real request to each API from this controlled
      // tick. The inventory response must still update the mounted controller.
      const heldPeriods = await tab.evaluate("window.__perseaHeldPeriods()");
      check(id, beforePanes.length === SESSIONS.length, "the document poll probe did not mount all six panes", beforePanes.length);
      check(id, Array.isArray(heldPeriods) && heldPeriods.length === 2 && heldPeriods.every((period) => period === 30_000),
        "the workspace document must arm exactly two 30-second service polls", heldPeriods);
      await control({ session: "ws04", detail: "rotation_deferred_alt_screen" });
      const beforePoll = await snapshot();
      const ticked = await tab.evaluate("window.__perseaTickIntervals()");
      check(id, ticked === 2, "the held document service polls did not both fire", ticked);
      // The poll is judged by what it puts in front of the operator, not by
      // what it puts in the DOM: a live cell hides its overlay AND its header
      // badge, so a detail that only reaches `textContent` reaches nobody.
      const polled = await tab.waitUntil((s) => readsDeferral(cellOf(s, "ws04")), 6_000);
      const polledState = polled.state ?? polled.last ?? await tab.state();
      const afterPoll = await snapshot();
      const pollRequests = Object.entries(afterPoll.requestsByPath)
        .map(([route, count]) => [route, count - (beforePoll.requestsByPath[route] || 0)])
        .filter(([, count]) => count !== 0).sort(([a], [b]) => a.localeCompare(b));
      check(id, JSON.stringify(pollRequests) === JSON.stringify([["/api/inventory", 1], ["/api/keyboard-preferences", 1]]),
        "six panes must share exactly one inventory and one keyboard-preferences poll request", pollRequests);
      check(id, !!polled.state, "the periodic presentation refresh did not put the deferral in front of the operator",
        polledState.cells.map((cell) => [cell.session, cell.badge, cell.wsDetail, cell.deferralNotice, cell.readsBadgePhrase, cell.readsDeferralSentence]));
      check(id, cellOf(polledState, "ws04")?.badge === "rotation deferred",
        "the periodic presentation refresh did not carry the detail to the mounted pane",
        polledState.cells.map((cell) => [cell.session, cell.badge]));
      await control({ session: "ws04", detail: null });
      await tab.evaluate("window.__wsPage.refreshPresentation()");

      await control({ session: "ws03", detail: "rotation_deferred_alt_screen" });
      await tab.evaluate("window.__wsPage.refreshPresentation()");
      let state = await tab.state();
      let fixtureState = await snapshot();
      let panes = await tab.evaluate("window.__wsPage.panes()");
      const deferred = cellOf(state, "ws03");
      check(id, deferred.badge === "rotation deferred" && deferred.state === "live" && deferred.overlayHidden === true,
        "the mounted exact pane did not render the deferred-rotation detail in place", deferred);
      // The overlay stays hidden — a live terminal is never covered — so the
      // reviewed words must be readable somewhere else on the same cell.
      check(id, readsDeferral(deferred) && deferred.wsDetail === "rotation_deferred_alt_screen"
        && /rotation deferred/i.test(deferred.deferralNotice.text) && /full-screen app is active/i.test(deferred.deferralNotice.text),
      "the deferral reached the DOM but not the operator: the live cell renders no readable notice", deferred);
      check(id, SESSIONS.filter((name) => name !== "ws03").every((name) => {
        const sibling = cellOf(state, name);
        return sibling.wsDetail === null && !sibling.readsBadgePhrase && (sibling.deferralNotice === null || sibling.deferralNotice.hidden === true);
      }), "a sibling pane rendered a deferral notice", state.cells.map((cell) => [cell.session, cell.wsDetail, cell.deferralNotice && cell.deferralNotice.hidden]));
      check(id, SESSIONS.filter((name) => name !== "ws03").every((name) => cellOf(state, name).badge === "80×24"),
        "a detail update changed a sibling pane", state.cells.map((cell) => [cell.session, cell.badge]));
      check(id, panes.find((pane) => pane.key.includes('"ws03"'))?.controllerDetail === "rotation_deferred_alt_screen",
        "the mounted controller did not receive the detail", panes);
      check(id, fixtureState.counters.websockets === before.counters.websockets && fixtureState.counters.handleRequests === before.counters.handleRequests
        && fixtureState.counters.takeovers === before.counters.takeovers && fixtureState.counters.adoptions === before.counters.adoptions
        && fixtureState.attachments.every((attachment) => attachment.inputs.length === 0 && attachment.resizes === 0),
      "presentation detail caused transport, capability, input, resize, takeover, or adoption activity", { before: before.counters, after: fixtureState.counters, attachments: fixtureState.attachments });

      await tab.evaluate("window.__wsPage.refreshSavedPresentation()");
      state = await tab.state();
      panes = await tab.evaluate("window.__wsPage.panes()");
      check(id, cellOf(state, "ws03").badge === "rotation deferred"
        && panes.find((pane) => pane.key.includes('"ws03"'))?.controllerDetail === "rotation_deferred_alt_screen",
      "a stale inventory refresh overwrote the newer presentation detail", { cell: cellOf(state, "ws03"), panes });

      await control({ session: "ws03", detail: null });
      await tab.evaluate("window.__wsPage.refreshPresentation()");
      state = await tab.state();
      fixtureState = await snapshot();
      check(id, cellOf(state, "ws03").badge === "80×24" && fixtureState.counters.websockets === before.counters.websockets,
        "clearing the detail changed transport or failed to restore the live badge", { cell: cellOf(state, "ws03"), counters: fixtureState.counters });
      // A product that never builds the notice has nothing on screen either, so
      // absent counts as cleared here. The positive check above is what fails
      // when the operator is shown nothing; this one must not crash before it.
      check(id, (cellOf(state, "ws03").deferralNotice === null || cellOf(state, "ws03").deferralNotice.hidden === true) && cellOf(state, "ws03").wsDetail === null
        && !cellOf(state, "ws03").readsBadgePhrase && !cellOf(state, "ws03").readsDeferralSentence,
      "clearing the detail left the deferral notice on the operator's screen", cellOf(state, "ws03"));

      await control({ session: "ws03", closeLive: "generation_rotated" });
      const reattached = await waitFixture((value) => attachmentsOf(value, "ws03").filter((attachment) => attachment.live).length === 1
        && attachmentsOf(value, "ws03").length === 2, 8_000);
      check(id, reattached !== null && reattached.counters.websockets === before.counters.websockets + 1,
        "alt-screen exit did not produce exactly one ordinary typed reattach", reattached && { counters: reattached.counters, attachments: attachmentsOf(reattached, "ws03") });
      await waitLive(tab);

      const beforeReplacement = await snapshot();
      await control({ replaceSession: "ws03", replacementDetail: "rotation_deferred_alt_screen" });
      await tab.evaluate("window.__wsPage.refreshPresentation()");
      state = await tab.state();
      fixtureState = await snapshot();
      panes = await tab.evaluate("window.__wsPage.panes()");
      check(id, cellOf(state, "ws03").badge === "80×24" && panes.find((pane) => pane.key.includes('"ws03"'))?.controllerDetail === null
        && !readsDeferral(cellOf(state, "ws03")) && cellOf(state, "ws03").wsDetail === null,
      "a same-name replacement decorated the stale controller", { cell: cellOf(state, "ws03"), panes });
      check(id, fixtureState.counters.websockets === beforeReplacement.counters.websockets
        && fixtureState.counters.handleRequests === beforeReplacement.counters.handleRequests
        && fixtureState.counters.takeovers === beforeReplacement.counters.takeovers,
      "replacement detail caused a transport or capability action", { before: beforeReplacement.counters, after: fixtureState.counters });
      record(id, { before: before.counters, deferred: { badge: deferred.badge, controller: panes }, after: fixtureState.counters, initialControllers: beforePanes, presentationPollPeriods: heldPeriods, documentPollRequests: pollRequests, pollPaneCount: beforePanes.length });
      await tab.close(debugPort);
    }]);
    // SEAM1-F1 — M11's rotation detail, E-P4's in-place switch and UX-7's
    // loading surface meet on ONE pane. After a pane switches session in
    // place: the shared presentation refresh must reach the session the pane
    // NOW runs and never the one its durable leaf still names, and the pane
    // must show the honest loading surface from the switch commit until the
    // new session's first COMMIT.
    scenarios.push(["SEAM1", async () => {
      const id = "SEAM1-F1";
      begin(id);
      const leaves = ["ws01", "ws02", "ws03"];
      await control({ reset: true, sessions: [...leaves, "ws07"] });
      const tab = await openTab("seam1");
      await tab.navigate(`${origin}/workspace-harness`);
      const ready = await tab.waitUntil((s) => s.harnessReady, 8_000);
      assert(ready.state, `${id}: the workspace page harness never became ready`);
      await tab.evaluate(`window.__wsPage.open(${JSON.stringify(arrangementFor(leaves))})`);
      await waitLive(tab, leaves);
      const before = await snapshot();
      const paneOf = (list) => list.find((pane) => pane.key.includes('"ws03"'));

      // The pane carries A's detail before it switches.
      await control({ session: "ws03", detail: "rotation_deferred_alt_screen" });
      await tab.evaluate("window.__wsPage.refreshPresentation()");
      const decorated = await tab.waitUntil((s) => readsDeferral(cellOf(s, "ws03")) && cellOf(s, "ws03").badge === "rotation deferred", 6_000);
      assert(decorated.state, `${id}: the pane never rendered its own session's detail: ${JSON.stringify(decorated.last && cellOf(decorated.last, "ws03"))}`);
      await control({ session: "ws03", detail: null });

      // Hold B's PREPARE so the window between the switch commit and B's
      // first COMMIT is observable rather than instantaneous.
      await control({ session: "ws07", holdPrepareMs: 2_500 });
      const point = async (source, label) => {
        const value = await tab.evaluate(source);
        assert(value, `${id}: ${label} is unavailable`);
        return value;
      };
      const switchPoint = await point(`(() => { const e=document.querySelector('.ws-cell[data-ws-session="ws03"] .persea-unified-tag'); if(!e)return null; const r=e.getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2}; })()`, "the pane's session tag");
      await tab.trustedClick(switchPoint);
      await delay(120);
      await tab.evaluate(`(() => { const c=document.querySelector('.ws-cell[data-ws-session="ws03"]'); const e=[...c.querySelectorAll('.persea-session-switcher__row')].find((n)=>n.getAttribute('aria-label')==='Switch to ws07'); e?.scrollIntoView({ block: "center" }); return !!e; })()`);
      await delay(80);
      const rowPoint = await point(`(() => { const c=document.querySelector('.ws-cell[data-ws-session="ws03"]'); const e=[...c.querySelectorAll('.persea-session-switcher__row')].find((n)=>n.getAttribute('aria-label')==='Switch to ws07'); if(!e)return null; const r=e.getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2,hidden:e.closest('[hidden]')!==null}; })()`, "the ws07 row");
      assert(!rowPoint.hidden, `${id}: the ws07 row is not on screen`);
      await tab.trustedClick(rowPoint);

      // UX-7: the switched pane is never a blank viewport with no state, and
      // the surface names the session it is now waiting for.
      const loading = await tab.waitUntil((s) => {
        const cell = cellOf(s, "ws03");
        return !!cell && !!cell.loading && cell.loading.hidden === false && cell.loading.w > 0;
      }, 8_000);
      check(id, !!loading.state, "the switched pane showed no loading surface before the new session committed",
        loading.last && cellOf(loading.last, "ws03"));
      check(id, loading.state ? cellOf(loading.state, "ws03").loading.text.includes("ws07") : false,
        "the restored loading surface named the wrong session", loading.state && cellOf(loading.state, "ws03").loading);

      await control({ session: "ws03", closeLive: "generation_rotated" });
      const committed = await tab.waitUntil((s) => {
        const cell = cellOf(s, "ws03");
        return !!cell && cell.state === "live" && cell.rows.includes("fixture-live ws07");
      }, 15_000);
      assert(committed.state, `${id}: the pane did not commit ws07: ${JSON.stringify(committed.last && cellOf(committed.last, "ws03"))}`);
      check(id, cellOf(committed.state, "ws03").loading?.hidden === true,
        "the loading surface survived the new session's COMMIT", cellOf(committed.state, "ws03").loading);

      // M11 F3 through the switched pane: the detail of the session it NOW
      // runs reaches it, although its leaf still names the old one.
      const beforeProjection = await snapshot();
      await control({ session: "ws07", detail: "rotation_deferred_alt_screen" });
      await tab.evaluate("window.__wsPage.refreshPresentation()");
      const projected = await tab.waitUntil((s) => readsDeferral(cellOf(s, "ws03")) && cellOf(s, "ws03").badge === "rotation deferred", 6_000);
      const projectedPanes = await tab.evaluate("window.__wsPage.panes()");
      check(id, !!projected.state && paneOf(projectedPanes)?.controllerDetail === "rotation_deferred_alt_screen",
        "the running session's rotation detail did not reach the switched pane",
        { cell: (projected.state ?? projected.last) && cellOf(projected.state ?? projected.last, "ws03"), panes: projectedPanes });
      const afterProjection = await snapshot();
      check(id, afterProjection.counters.websockets === beforeProjection.counters.websockets
        && afterProjection.counters.handleRequests === beforeProjection.counters.handleRequests
        && afterProjection.counters.takeovers === beforeProjection.counters.takeovers,
      "projecting a detail onto the switched pane caused transport or capability activity",
      { before: beforeProjection.counters, after: afterProjection.counters });

      // M11LF-F3C at page level: the pane switched AWAY from ws03, so ws03's
      // detail must not decorate it even though the leaf still names ws03.
      await control({ session: "ws07", detail: null });
      await control({ session: "ws03", detail: "rotation_deferred_alt_screen" });
      await tab.evaluate("window.__wsPage.refreshPresentation()");
      await delay(200);
      const stale = await tab.state();
      const stalePanes = await tab.evaluate("window.__wsPage.panes()");
      check(id, cellOf(stale, "ws03")?.badge !== "rotation deferred" && paneOf(stalePanes)?.controllerDetail === null
        && !readsDeferral(cellOf(stale, "ws03")) && cellOf(stale, "ws03")?.wsDetail === null,
      "the detail of the session the pane switched away from decorated the pane",
      { cell: cellOf(stale, "ws03"), panes: stalePanes });
      check(id, leaves.filter((name) => name !== "ws03").every((name) => cellOf(stale, name)?.badge === "80×24"),
        "a switched pane's projection changed a sibling", stale.cells.map((cell) => [cell.session, cell.badge]));
      record(id, { before: before.counters, after: afterProjection.counters, panes: projectedPanes, stalePanes,
        loading: { switching: loading.state && cellOf(loading.state, "ws03").loading, committed: cellOf(committed.state, "ws03").loading } });
      await tab.screenshot("seam1_switched_pane_projection.png");
      await tab.close(debugPort);
    }]);
    // W1B-F14 — a structural update through the real page: surviving cells,
    // controllers, sockets, focus, selection, and the designated pane persist;
    // only the added leaf attaches and only the removed leaf retires.
    scenarios.push(["F14", async () => {
      const id = "W1B-F14";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      const tab = await openTab("f14");
      await tab.navigate(`${origin}/workspace-harness`);
      const ready = await tab.waitUntil((s) => s.harnessReady, 8_000);
      assert(ready.state, `${id}: the workspace page harness never became ready: ${JSON.stringify(ready.last && ready.last.fatal)}`);
      const three = ["ws01", "ws02", "ws03"];
      await tab.evaluate(`window.__wsPage.open(${JSON.stringify(arrangementFor(three))})`);
      await waitLive(tab, three);
      const before = await snapshot();
      // Focus pane ws02 and put a DOM selection on its header name (the page saves
      // and restores window selection ranges across an update).
      const s0 = await tab.state();
      await tab.trustedClick({ x: cellOf(s0, "ws02").screen.x, y: cellOf(s0, "ws02").screen.y });
      await tab.typeText("f14;");
      await tab.evaluate(`(() => { document.querySelectorAll(".ws-cell").forEach((c) => { c.dataset.wsStamp = "keep"; }); const name = document.querySelector('.ws-cell[data-ws-session="ws02"] .ws-cell__name'); const range = document.createRange(); range.selectNodeContents(name); const sel = getSelection(); sel.removeAllRanges(); sel.addRange(range); return String(sel); })()`);
      const pre = await tab.state();
      check(id, pre.activeCell === "ws02" && cellOf(pre, "ws02").designated, "the focused pane must be designated before the update", { activeCell: pre.activeCell, designated: pre.cells.map((c) => [c.session, c.designated]) });
      // Structural update 1: add ws04 beside the three.
      const four = ["ws01", "ws02", "ws03", "ws04"];
      const accepted = await tab.evaluate(`window.__wsPage.updateTree(${JSON.stringify(arrangementFor(four))})`);
      check(id, accepted === true, "the page must accept a valid structural update", { accepted });
      await waitLive(tab, four);
      const post = await tab.state();
      const after = await snapshot();
      check(id, three.every((n) => cellOf(post, n).stamp === "keep") && cellOf(post, "ws04").stamp === null, "surviving cells must keep their elements; the added cell is new", post.cells.map((c) => [c.session, c.stamp]));
      check(id, three.every((n) => attachmentsOf(after, n).length === attachmentsOf(before, n).length && liveAttachmentOf(after, n).id === liveAttachmentOf(before, n).id) && attachmentsOf(after, "ws04").length === 1,
        "surviving panes must keep their controller and socket; only the added leaf attaches", { before: before.attachments.map((a) => [a.id, a.session, a.live]), after: after.attachments.map((a) => [a.id, a.session, a.live]) });
      check(id, post.activeCell === "ws02" && cellOf(post, "ws02").designated && post.activeDetail === pre.activeDetail && post.activeDetail.endsWith("|ws02|xterm"), "focus must stay in the pane that held it, on the terminal's own textarea", { activeCell: post.activeCell, activeBefore: pre.activeDetail, activeAfter: post.activeDetail, designated: post.cells.map((c) => [c.session, c.designated]) });
      check(id, post.selectionText === pre.selectionText && pre.selectionText.length > 0, "the DOM selection must survive the update", { before: pre.selectionText, after: post.selectionText });
      check(id, after.counters.inventory === before.counters.inventory, "a structural update must not refetch the inventory", { before: before.counters.inventory, after: after.counters.inventory });
      await tab.typeText("after;");
      await delay(200);
      const typed = await snapshot();
      check(id, inputsOf(typed, "ws02").join("").includes("f14;after;") && four.filter((n) => n !== "ws02").every((n) => !inputsOf(typed, n).join("").includes("after")), "typing after the update must reach the same pane's own socket only", Object.fromEntries(four.map((n) => [n, inputsOf(typed, n).join("")])));
      // Structural update 2: remove ws01; its controller retires, siblings stay.
      const removed = await tab.evaluate(`window.__wsPage.updateTree(${JSON.stringify(arrangementFor(["ws02", "ws03", "ws04"]))})`);
      check(id, removed === true, "the page must accept a removal update", { removed });
      const gone = await waitFixture((snap) => liveAttachmentOf(snap, "ws01") === undefined, 5_000);
      const s2 = await tab.state();
      check(id, gone !== null && ["ws02", "ws03", "ws04"].every((n) => liveAttachmentOf(gone, n).id === liveAttachmentOf(after, n).id) && s2.cells.length === 3 && !cellOf(s2, "ws01"),
        "removing a leaf must close only its own socket and keep the siblings' attachments", { cells: s2.cells.map((c) => c.session), live: gone && gone.attachments.filter((a) => a.live).map((a) => [a.id, a.session]) });
      check(id, s2.activeCell === "ws02" && cellOf(s2, "ws02").designated, "focus and designation survive the removal", { activeCell: s2.activeCell });
      // Invalid tree: refused, nothing changes.
      const refused = await tab.evaluate(`window.__wsPage.updateTree(${JSON.stringify(JSON.stringify({ version: 1, root: { kind: "split", direction: "row", weights: [1], children: [] } }))})`);
      const s3 = await tab.state();
      check(id, refused === false && s3.cells.length === 3, "an invalid tree must be refused without touching the layout", { refused, cells: s3.cells.length });
      await tab.screenshot("f14_structural_update_desktop.png");
      record(id, { before: before.counters, after: after.counters, selection: [pre.selectionText, post.selectionText], active: post.activeDetail });
      await tab.close(debugPort);
    }]);
    // EP3 — one document-global snippet service across six panes: target-pane
    // INPUT authority (EP3-F1), one poll for the whole document (EP3-F4), and
    // the OSC record's economics under a two-document flood (EP3-F8).
    scenarios.push(["EP3", async () => {
      const id = "EP3";
      await control({ reset: true, sessions: SESSIONS });
      begin(id);
      const tab = await openTab("ep3");
      await tab.emulate(TOUCH_DESKTOP, true);
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      await waitLive(tab);
      await delay(400);
      await control({ snippetSeed: { snippets: 3, clips: 20 } });

      // A point inside ONE cell, so every tap names the pane it belongs to.
      const cellPoint = (session, expression) => tab.evaluate(`(() => {
        const cell = document.querySelector('.ws-cell[data-ws-session=' + ${JSON.stringify(JSON.stringify(session))} + ']');
        if (!cell) return null;
        const el = ${expression};
        if (!el) return null;
        el.scrollIntoView({ block: "center", inline: "nearest" });
        const r = el.getBoundingClientRect();
        return { x: r.x + r.width / 2, y: r.y + r.height / 2, w: r.width, h: r.height, disabled: el.disabled === true };
      })()`);
      const tilePoint = (session, label) => cellPoint(session, `Array.from(cell.querySelectorAll('.persea-unified-sheet__tile')).find((t) => (t.querySelector('.persea-unified-sheet__label') || {}).textContent === ${JSON.stringify(label)})`);
      const clipboardPoint = (expression) => cellPoint(SESSIONS[0], expression);
      const clipboardAction = async (label) => {
        const point = await clipboardPoint(`Array.from(document.querySelectorAll('.persea-clipboard[open] button')).find(button => button.getAttribute('aria-label') === ${JSON.stringify(label)})`);
        assert(point, `${id}: Clipboard action ${label} is absent`); await tab.trustedClick(point); await delay(120);
      };
      const clipboardState = () => tab.evaluate(`(() => { const panels = [...document.querySelectorAll('.persea-clipboard[open]')]; const panel = panels[0]; return { count: panels.length, saved: !!panel?.querySelector('button[aria-label="Saved text"][aria-pressed="true"]'), rows: panel?.querySelectorAll('[data-clipboard-item]').length || 0 }; })()`);
      const rowPoint = (session, index) => clipboardPoint(`document.querySelectorAll('.persea-clipboard[open] [data-clipboard-item]')[${index}]`);
      // Idempotent: opens the sheet only when it is dismissed, and taps the
      // Snippets tile only when this pane is not already showing that list
      // (the tile toggles).
      const cellNow = async (session) => (await tab.state()).cells.find((c) => c.session === session);
      const openSnippetList = async (session) => {
        if ((await clipboardState()).count) await clipboardAction("Close clipboard");
        let cell = await cellNow(session);
        if (!cell.sheetOpen) {
          assert(cell.openerPoint, `${id}: pane ${session} has no sheet opener`);
          await tab.tap(cell.openerPoint);
          await delay(200);
          cell = await cellNow(session);
        }
        const tile = await tilePoint(session, "Clipboard");
        assert(tile, `${id}: pane ${session} has no Clipboard route`);
        await tab.tap(tile); await delay(250);
      };

      // --- EP3-F4 (six panes, ONE poll loop) -------------------------------
      // A sheet is dismissed by a pointer outside it, so at most one pane's
      // list is on screen at a time — and that is exactly the point: the poll
      // belongs to the DOCUMENT's one service, not to a pane. Six panes are
      // opened in turn (each open costs its own first read, as any first read
      // does), and then the steady-state rate is measured: one loop, never six.
      const beforeLists = (await snapshot()).snippets;
      const perOpen = [];
      for (const session of SESSIONS) {
        const before = (await snapshot()).snippets.get;
        await openSnippetList(session);
        await delay(250);
        const state = await tab.state();
        const clipboard = await clipboardState();
        perOpen.push({ session, reads: (await snapshot()).snippets.get - before, view: "list", rows: clipboard.rows, openSheets: clipboard.count });
      }
      const settled = (await snapshot()).snippets;
      const windowStart = Date.now();
      await delay(9_000);
      const windowEnd = (await snapshot()).snippets;
      const elapsed = Date.now() - windowStart;
      record(id, { polling: { getsBefore: beforeLists.get, afterOpens: settled.get, afterWindow: windowEnd.get, elapsedMs: elapsed, peak: windowEnd.getsInFlightPeak, perOpen } });
      check(id, windowEnd.getsInFlightPeak === 1, "the document ran overlapping polls", windowEnd);
      // One 4s loop over ~9s is at most 3 reads; six loops would be ~18.
      check(id, windowEnd.get - settled.get <= 4, "the steady-state poll rate scales with panes: this is not one loop per document", { afterOpens: settled.get, afterWindow: windowEnd.get, elapsed });
      check(id, perOpen.every((entry) => entry.reads <= 1), "opening one pane's list cost more than one read", perOpen);
      check(id, perOpen.every((entry) => entry.view === "list" && entry.rows >= 3), "a pane's Clipboard did not render the shared authoritative snapshot", perOpen);
      check(id, perOpen.every((entry) => entry.openSheets === 1), "more than one pane rendered a snippet panel at once", perOpen);
      await tab.screenshot("ep3_six_pane_snippets.png");

      // --- EP3-F1 (target-pane INPUT authority) ----------------------------
      // Focus pane A's terminal, then insert from pane C's list. The bytes
      // belong to C: DOM focus, "most recent controller" and session name are
      // all irrelevant to which pane a row addresses.
      const paneA = SESSIONS[0];
      const paneC = SESSIONS[2];
      // Focus moves PROGRAMMATICALLY: a pointer in pane A would dismiss pane
      // C's sheet (E-P1's outside-dismissal), and the property under test is
      // that DOM focus does not choose the pane, not that a tap does.
      await clipboardAction("Close clipboard");
      const focusMoved = await tab.evaluate(`(() => {
        const cell = document.querySelector('.ws-cell[data-ws-session=' + ${JSON.stringify(JSON.stringify(paneA))} + ']');
        const textarea = cell && cell.querySelector('.xterm-helper-textarea');
        if (!textarea) return null;
        textarea.focus({ preventScroll: true });
        return document.activeElement === textarea;
      })()`);
      assert(focusMoved === true, `${id}: pane ${paneA}'s textarea did not take focus`);
      await delay(200);
      const focused = await tab.state();
      check(id, focused.activeCell === paneA, "the focus precondition did not take", { activeCell: focused.activeCell });
      // The modal intentionally makes sibling panes inert. Establish the
      // unrelated focus owner first, then capture pane C through its route.
      await openSnippetList(paneC);
      const before = await snapshot();
      const inputsBefore = Object.fromEntries(SESSIONS.map((n) => [n, inputsOf(before, n).join("")]));
      const insertPointC = await rowPoint(paneC, 0);
      assert(insertPointC && !insertPointC.disabled, `${id}: pane ${paneC} has no enabled insert row: ${JSON.stringify(insertPointC)}`);
      await tab.tap(insertPointC);
      const preview = await snapshot();
      check(id, SESSIONS.every(n => inputsOf(preview, n).join("") === inputsBefore[n]), "opening a Clipboard preview sent input before explicit Send", preview.attachments);
      await clipboardAction("Cancel");
      await clipboardAction("Paste text to terminal");
      await delay(500);
      const after = await snapshot();
      const inputsAfter = Object.fromEntries(SESSIONS.map((n) => [n, inputsOf(after, n).join("")]));
      const deltas = Object.fromEntries(SESSIONS.map((n) => [n, inputsAfter[n].slice(inputsBefore[n].length)]));
      record(id, { targetPane: { focusedCell: focused.activeCell, insertedFrom: paneC, deltas } });
      check(id, deltas[paneC].length > 0, "the insert did not reach the pane whose row was tapped", deltas);
      check(id, SESSIONS.filter((n) => n !== paneC).every((n) => deltas[n] === ""), "an insert reached a pane other than the one whose row was tapped", deltas);
      // Every pane's INPUT still follows its own MODE grant.
      check(id, after.attachments.every((a) => firstIndex(a.frames, "INPUT") === -1 || firstIndex(a.frames, "INPUT") > a.frames.indexOf("MODE_REQUEST")), "INPUT preceded a pane's MODE_REQUEST", after.attachments.map((a) => [a.session, a.frames.slice(0, 6)]));
      // Presentation only: no pane's geometry moved.
      check(id, after.attachments.every((a, i) => a.resizes === before.attachments[i].resizes), "a snippet action moved a pane's tmux geometry", { before: before.attachments.map((a) => [a.session, a.resizes]), after: after.attachments.map((a) => [a.session, a.resizes]) });

      // --- UX12 (six-pane selection/paste authority) -----------------------
	  // Contextual Select/Copy and primary Paste belong to pane C's controller even while
      // pane A had focus. The lifecycle must not write, resize or remint any
      // sibling attachment.
      await tab.pressKey("Escape");
	  const selectPointC = await cellPoint(paneC, `cell.querySelector('.persea-unified-select-context')`);
      assert(selectPointC && !selectPointC.disabled, `${id}: pane ${paneC} has no enabled Select action`);
      await tab.trustedClick(selectPointC);
      await delay(100);
      const selected = await tab.evaluate(`(() => {
        const cell = document.querySelector('.ws-cell[data-ws-session=' + ${JSON.stringify(JSON.stringify(paneC))} + ']');
        const row = cell && cell.querySelector('.persea-unified-select__row');
        if (!row || !row.firstChild || !(row.textContent || '').length) return false;
        const range = document.createRange(); range.selectNodeContents(row);
        const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range);
		return true;
      })()`);
	  assert(selected === true, `${id}: pane ${paneC} frozen selection could not be established`);
	  // UX14 §14.2: the pane's Paste slot becomes Copy while the frozen range
	  // exists; Select stays in its Selecting state.
	  let copyReady = false;
	  for (let attempt = 0; attempt < 100 && !copyReady; attempt += 1) {
		copyReady = await tab.evaluate(`document.querySelector('.ws-cell[data-ws-session=' + ${JSON.stringify(JSON.stringify(paneC))} + '] .persea-unified-toolbar-paste')?.dataset.pasteState === 'copy'`);
		if (!copyReady) await delay(20);
	  }
	  const selectionDiagnostic = copyReady ? null : await tab.evaluate(`(() => {
		const selection = getSelection();
		const cell = document.querySelector('.ws-cell[data-ws-session=' + ${JSON.stringify(JSON.stringify(paneC))} + ']');
		return { text: selection?.toString(), ranges: selection?.rangeCount, collapsed: selection?.isCollapsed,
		  state: cell?.querySelector('.persea-unified-select-context')?.dataset.selectState,
		  paste: cell?.querySelector('.persea-unified-toolbar-paste')?.dataset.pasteState,
		  rows: cell?.querySelectorAll('.persea-unified-select__row').length };
	  })()`);
	  assert(copyReady, `${id}: pane ${paneC} frozen selection did not turn its Paste slot into Copy ${JSON.stringify(selectionDiagnostic)}`);
	  const copyPointC = await cellPoint(paneC, `cell.querySelector('.persea-unified-toolbar-paste')`);
      assert(copyPointC && !copyPointC.disabled, `${id}: pane ${paneC} has no enabled frozen Copy action`);
      await tab.trustedClick(copyPointC);
      // The Copied ✓ dwell (1.2 s) ends before the slot is Paste again.
      let pasteBack = false;
      for (let attempt = 0; attempt < 150 && !pasteBack; attempt += 1) {
		pasteBack = await tab.evaluate(`document.querySelector('.ws-cell[data-ws-session=' + ${JSON.stringify(JSON.stringify(paneC))} + '] .persea-unified-toolbar-paste')?.dataset.pasteState === 'paste'`);
		if (!pasteBack) await delay(20);
      }
      assert(pasteBack, `${id}: pane ${paneC} Paste slot did not return to Paste after Copy`);
	  await tab.evaluate(`Object.defineProperty(navigator, "clipboard", { configurable: true, value: {
		readText: () => Promise.resolve("UX12-WS-PASTE"), writeText: () => Promise.resolve(),
	  } })`);
      const beforePaste = await snapshot();
      const pasteInputsBefore = Object.fromEntries(SESSIONS.map((n) => [n, inputsOf(beforePaste, n).join("")]));
	  const pastePointC = await cellPoint(paneC, `cell.querySelector('.persea-unified-toolbar-paste')`);
      assert(pastePointC && !pastePointC.disabled, `${id}: pane ${paneC} has no enabled Paste action`);
      await tab.trustedClick(pastePointC);
      await clipboardAction("Paste from device");
      for (let attempt = 0; attempt < 100; attempt++) {
        if (await tab.evaluate(`Array.from(document.querySelectorAll('.persea-clipboard__preview')).some(node => node.textContent === 'UX12-WS-PASTE')`)) break;
        await delay(30);
      }
      const previewPaste = await snapshot();
      check(id, SESSIONS.every(n => inputsOf(previewPaste, n).join("") === pasteInputsBefore[n]), "device import sent input before explicit Send", previewPaste.attachments);
      const importedPastePoint = await clipboardPoint(`Array.from(document.querySelectorAll('.persea-clipboard [data-clipboard-item]')).find(row => row.querySelector('.persea-clipboard__preview')?.textContent === 'UX12-WS-PASTE')?.querySelector('button[aria-label="Paste text to terminal"]')`);
      assert(importedPastePoint, `${id}: imported text has no Paste action`);
      await tab.trustedClick(importedPastePoint);
      await delay(500);
      const afterPaste = await snapshot();
      const pasteInputsAfter = Object.fromEntries(SESSIONS.map((n) => [n, inputsOf(afterPaste, n).join("")]));
      const pasteDeltas = Object.fromEntries(SESSIONS.map((n) => [n, pasteInputsAfter[n].slice(pasteInputsBefore[n].length)]));
      record(id, { ux12: { pane: paneC, deltas: pasteDeltas } });
      check(id, pasteDeltas[paneC].length > 0, "UX12 Paste did not reach its own selected pane", pasteDeltas);
      check(id, SESSIONS.filter((n) => n !== paneC).every((n) => pasteDeltas[n] === ""), "UX12 selection/paste touched a sibling pane", pasteDeltas);
      check(id, afterPaste.attachments.length === beforePaste.attachments.length
        && afterPaste.attachments.every((attachment, index) => attachment.resizes === beforePaste.attachments[index].resizes),
      "UX12 selection/paste reminted or resized an attachment", { before: beforePaste.attachments, after: afterPaste.attachments });

      // --- EP3-F8 (two documents flood the ONE global OSC record) ----------
      // A second workspace document on its own sessions is a second device as
      // far as the store is concerned: both flood OSC 52 while manual clips
      // sit at the ring's limit.
      await control({ createSession: "ws07" });
      await control({ createSession: "ws08" });
      const tab2 = await openTab("ep3b");
      await tab2.emulate(TOUCH_DESKTOP, true);
      await seedArrangement(tab2, ["ws07", "ws08"], "ops2");
      await landOn(tab2, workspaceURL("ops2"), "resume");
      await trustedOpen(tab2);
      await tab2.waitUntil((s) => s.cells.length === 2 && s.cells.every((c) => c.state === "live"), 15_000);
      await delay(400);
      const oscSequence = (value) => `\x1b]52;c;${Buffer.from(value, "utf8").toString("base64")}\x07`;
      const floodSnapshotBefore = await snapshot();
      const inputsBeforeFlood = Object.fromEntries([...SESSIONS, "ws07", "ws08"].map((n) => [n, inputsOf(floodSnapshotBefore, n).join("")]));
      const floodBefore = floodSnapshotBefore.snippets;
      for (let round = 0; round < 5; round += 1) {
        for (const session of [...SESSIONS, "ws07", "ws08"]) {
          await control({ session, writeLive: oscSequence(`flood-${session}-${round}`) });
        }
      }
      await delay(2_000);
      const floodSnapshot = await snapshot();
      const floodAfter = floodSnapshot.snippets;
      record(id, { flood: { putsDelta: floodAfter.oscPut - floodBefore.oscPut, osc: floodAfter.osc, manualClips: floodAfter.manualClips, body: floodAfter.oscBody, ids: floodAfter.ids.filter((x) => !/^[0-9a-f]{32}$/.test(x)) } });
      check(id, floodAfter.osc === 1, "the two-document flood produced more than one OSC record", floodAfter);
      // The editable canonical OSC value is one ordinary clip in addition
      // to the bounded 20-entry ring; the private publication stays separate.
      check(id, floodAfter.manualClips === 21, "the flood changed the clip ring plus canonical OSC capacity", floodAfter);
      check(id, floodAfter.ids.filter((x) => !/^[0-9a-f]{32}$/.test(x)).join(",") === "osc52", "the flood minted an id other than the one global record", floodAfter.ids);
      // Two documents, forty writes: each document coalesces its own stream, so
      // the operator-wide limiter is never spent on a per-write basis.
      check(id, floodAfter.oscPut - floodBefore.oscPut <= 12, "the two-document flood was not coalesced", { putsDelta: floodAfter.oscPut - floodBefore.oscPut });
      const floodState = await tab.state();
      check(id, floodState.cells.every((c) => c.pages === 1), "a pane lost or duplicated its page during the flood", floodState.cells.map((c) => [c.session, c.pages]));
      const inputsAfterFlood = Object.fromEntries([...SESSIONS, "ws07", "ws08"].map((n) => [n, inputsOf(floodSnapshot, n).join("")]));
      check(id, [...SESSIONS, "ws07", "ws08"].every((n) => inputsAfterFlood[n] === (inputsBeforeFlood[n] ?? "")), "an OSC sequence put bytes back into a pane", { before: inputsBeforeFlood, after: inputsAfterFlood });
      await tab2.close(debugPort);
      await tab.close(debugPort);
    }]);

    // W2-F4/F6-F10/F14 — the durable editor is a local draft until one
    // explicit Save.  Surviving panes keep their real controller, socket,
    // toolbar DOM, focus surface and geometry authority through the commit;
    // a conflicting Save retires nothing.
    scenarios.push(["W2", async () => {
      const id = "W2-F4/F6/F7/F8/F9/F10/F14";
      begin(id);
      await control({ reset: true, sessions: SESSIONS });
      const tab = await openTab("w2-durable-editor");
      await seedArrangement(tab, SESSIONS);
      let s = await landOn(tab, workspaceURL(), "resume");
      check(id, s.landingBoxes.map((box) => box.session).join(",") === SESSIONS.join(","), "durable tree must win over the contradictory reversed sessionStorage record", { durable: s.landingBoxes.map((box) => box.session), ephemeral: s.ephemeral });
      await trustedOpen(tab);
      s = await waitLive(tab);
      await tab.evaluate(`document.querySelectorAll(".persea-unified-toolbar").forEach((node, index) => { node.dataset.wsToolbarStamp = "w2-toolbar-" + index; })`);
      const beforeState = await tab.state();
      const beforeFixture = await snapshot();
      const beforeAttachments = new Map(SESSIONS.map((name) => [name, liveAttachmentOf(beforeFixture, name)?.id]));
      check(id, beforeState.cells.every((cell) => cell.toolbarCount === 1 && cell.geometryCount === 1 && cell.fitCount === 1 && cell.composeCount === 1 && cell.moreCount === 0 && cell.quickActionsCount === 1), "every pane must own one real toolbar, one committed geometry disclosure, and one of each control", beforeState.cells);
      // Every pane names its own session in its own bar, on one line, inside
      // the bar, with the alias dropped, the dot at full size, and nothing
      // from the control row painted over it.
      check(id, beforeState.cells.every((cell) => cell.tag
        && cell.tag.width > 0 && cell.tag.lines === 1 && cell.tag.dot >= 4 && cell.tag.dotState !== null
        && cell.tag.name === cell.session && cell.tag.aliasWidth === 0
        && cell.tag.outside === 0 && cell.tag.oneRow
        && cell.tag.hit && cell.tag.dotHit && cell.tag.covered === 0),
      "every pane must name its own session in its floating bar, on one line, inside the bar, with nothing painted over it",
      beforeState.cells.map((cell) => [cell.session, cell.tag]));
      await tab.evaluate(`(() => {
        const cell = document.querySelector(".ws-cell");
        const toolbar = cell.querySelector(".persea-unified-toolbar");
        cell.querySelector(".ws-cell__header").focus();
        window.__perseaMenuEscapePrevented = false;
        document.addEventListener("keydown", (event) => {
          if (event.key === "Escape") window.__perseaMenuEscapePrevented = event.defaultPrevented;
        }, true);
      })()`);
      const tabOrder = [];
      // UX11 replaces the standalone ↕/readout pair and old More stop with one
      // committed-geometry disclosure. The pin still says the same thing —
      // every header control, in document order, and only then the terminal.
      //
      // UX-10 removes the standalone Switch stop: the session tag now opens
      // the identity/switcher popover itself. UX12 adds the enabled contextual
      // Select owner and direct Paste before View; Composer and terminal keep
      // their existing order after Quick actions.
      for (let index = 0; index < 7; index += 1) {
        await tab.pressKey("Tab");
        tabOrder.push(await tab.evaluate(`document.activeElement?.getAttribute("aria-label") || document.activeElement?.textContent?.trim() || document.activeElement?.getAttribute("role") || ""`));
      }
      check(id, tabOrder.length === 7
        && tabOrder[0] === "Session ws01 \u2014 attached. Show session details"
        && tabOrder[1] === "Select terminal text"
        && tabOrder[2] === "Open clipboard"
        && /^View and size, committed 80×/.test(tabOrder[3])
        && JSON.stringify(tabOrder.slice(4)) === JSON.stringify(["Quick actions", "Open composer", "Terminal input"]),
      "compact toolbar Tab order must be enabled semantic disclosures then terminal", tabOrder);
      const viewOwner = (await tab.state()).cells.find((cell) => cell.viewPoint !== null);
      assert(viewOwner?.viewPoint, `${id}: no trusted View target in six-pane workspace`);
      await tab.trustedClick(viewOwner.viewPoint);
      await tab.evaluate(`document.querySelector(".ws-cell .persea-unified-view-popover button").focus()`);
      const directViewOpen = await tab.evaluate(`(() => { const view=document.querySelector(".ws-cell .persea-unified-view-disclosure"); const popover=document.querySelector(".ws-cell .persea-unified-view-popover"); const box=popover?.getBoundingClientRect(); return { expanded:view?.getAttribute("aria-expanded"), hidden:popover?.hidden, display:popover ? getComputedStyle(popover).display : null, box:box ? {x:box.x,y:box.y,w:box.width,h:box.height} : null }; })()`);
      const workspaceViewOpen = await tab.state();
      const openViews = workspaceViewOpen.cells.filter((cell) => cell.viewPopover !== null);
      // UX15 §15.6: the compact View carries zoom and the size block only; the
      // appearance selects live on the dashboard's Settings · Appearance card.
      const workspaceAppearance = await tab.evaluate(`(() => {
        const popover = document.querySelector(".ws-cell .persea-unified-view-popover:not([hidden])");
        if (!popover) throw new Error("missing open workspace View");
        return {
          selects: popover.querySelectorAll("select").length,
          pickers: popover.querySelectorAll(".persea-unified-preference").length,
          zoom: popover.querySelectorAll(".persea-unified-view-popover__zoom").length,
          size: popover.querySelectorAll(".persea-unified-size").length,
        };
      })()`);
      check(id, directViewOpen.expanded === "true" && directViewOpen.hidden === false && openViews.length === 1 && openViews[0].viewPopover.width <= 352
        && openViews[0].viewPopover.insideCell && openViews[0].viewPopover.insideViewport
        && !openViews[0].viewPopover.horizontalScroll,
      "one compact View popover must stay inside its six-pane workspace cell and viewport", { directViewOpen, openViews });
      check(id, workspaceAppearance.selects === 0 && workspaceAppearance.pickers === 0 && workspaceAppearance.zoom === 1 && workspaceAppearance.size === 1,
      "six-pane View must carry zoom and the size block only (UX15 §15.6)", workspaceAppearance);
      await tab.screenshot("ux13_view_popover_six_pane.png");
      await tab.pressKey("Escape");
      const escapeFocus = await tab.evaluate(`(() => {
        const view = document.querySelector(".ws-cell .persea-unified-view-disclosure");
        return { open: view.getAttribute("aria-expanded"), activeIsView: document.activeElement === view, prevented: window.__perseaMenuEscapePrevented };
      })()`);
      check(id, escapeFocus.open === "false" && escapeFocus.activeIsView, "Escape must close View and appearance and synchronously restore trigger focus", escapeFocus);
      const outsideFocus = await tab.evaluate(`(() => {
        const toolbar = document.querySelector(".ws-cell .persea-unified-toolbar");
        const view = toolbar.querySelector(".persea-unified-view-disclosure");
        const first = toolbar.querySelector(".persea-unified-view-popover button");
        view.click();
        first.focus();
        document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
        return { open: view.getAttribute("aria-expanded"), activeIsView: document.activeElement === view, activeText: document.activeElement?.textContent?.trim() };
      })()`);
      check(id, outsideFocus.open === "false" && outsideFocus.activeIsView, "outside-pointer dismissal must close onto the semantic disclosure without focusing the terminal", outsideFocus);
      const toolbarFitsHeader = (state) => state.cells.every((cell) => {
        if (!cell.header || !cell.toolbar) return false;
        const headerLeft = cell.header.x - cell.header.w / 2; const headerRight = cell.header.x + cell.header.w / 2;
        const headerTop = cell.header.y - cell.header.h / 2; const headerBottom = cell.header.y + cell.header.h / 2;
        const toolbarLeft = cell.toolbar.x - cell.toolbar.w / 2; const toolbarRight = cell.toolbar.x + cell.toolbar.w / 2;
        const toolbarTop = cell.toolbar.y - cell.toolbar.h / 2; const toolbarBottom = cell.toolbar.y + cell.toolbar.h / 2;
        return toolbarLeft >= headerLeft - 1 && toolbarRight <= headerRight + 1 && toolbarTop >= headerTop - 1 && toolbarBottom <= headerBottom + 1;
      });
      check(id, toolbarFitsHeader(beforeState) && beforeState.documentWidth <= beforeState.inner.w && beforeState.cspViolations === 0, "1280×800 compact headers must fit without horizontal scroll or CSP violations", { cells: beforeState.cells.map((cell) => ({ session: cell.session, header: cell.header, toolbar: cell.toolbar })), documentWidth: beforeState.documentWidth, inner: beforeState.inner, csp: beforeState.cspViolations });
      await tab.emulate({ ...DESKTOP, width: 1440, height: 900, screenWidth: 1440, screenHeight: 900 }, false);
      await delay(100);
      const wideState = await tab.state();
      check(id, toolbarFitsHeader(wideState) && wideState.documentWidth <= wideState.inner.w && wideState.cells.length === 6, "1440×900 compact headers must fit all six panes", { cells: wideState.cells.map((cell) => ({ session: cell.session, header: cell.header, toolbar: cell.toolbar })), documentWidth: wideState.documentWidth, inner: wideState.inner });
      await tab.emulate({ ...DESKTOP, width: 900, height: 800, screenWidth: 900, screenHeight: 800 }, false);
      await delay(100);
      const narrowState = await tab.state();
      check(id, toolbarFitsHeader(narrowState) && narrowState.documentWidth <= narrowState.inner.w && narrowState.cells.length === 6, "narrow fine-pointer compact headers must fit all six panes", { cells: narrowState.cells.map((cell) => ({ session: cell.session, header: cell.header, toolbar: cell.toolbar })), documentWidth: narrowState.documentWidth, inner: narrowState.inner });
      await tab.emulate(DESKTOP, false);
      await delay(100);

      await tab.evaluate(`document.querySelector(".ws-workspace-edit").click()`);
      s = await tab.waitUntil((state) => state.editing && state.dividers.length > 0, 5_000).then((result) => result.state);
      assert(s, `${id}: editor did not open`);
      const durableDividerValue = s.dividers[0].value;
      const divider = s.dividers[0];
      const dragTarget = divider.orientation === "vertical" ? { x: divider.point.x + 90, y: divider.point.y } : { x: divider.point.x, y: divider.point.y + 90 };
      await tab.pointerDrag(divider.point, dragTarget, 1_000);
      const dragged = await tab.state();
      const afterDragFixture = await snapshot();
      check(id, dragged.dividers[0].value !== durableDividerValue, "1,000 pointer moves must alter the local draft divider", { before: durableDividerValue, after: dragged.dividers[0].value });
      check(id, afterDragFixture.counters.workspaceWrites === beforeFixture.counters.workspaceWrites, "drag must perform zero durable writes", { before: beforeFixture.counters, after: afterDragFixture.counters });
      await tab.evaluate(`document.querySelector(".ws-editor__cancel").click()`);
      const cancelled = await tab.waitUntil((state) => !state.editing, 5_000).then((result) => result.state);
      assert(cancelled, `${id}: Cancel did not close the editor`);
      check(id, cancelled.dividers[0].value === durableDividerValue, "Cancel must restore the durable divider weight", { durableDividerValue, cancelled: cancelled.dividers[0].value });

      await tab.evaluate(`document.querySelector(".ws-workspace-edit").click()`);
      await tab.waitUntil((state) => state.editing, 5_000);
      const nameLawInputs = await tab.evaluate(`(() => ({
        workspaceHasMaxLength: document.querySelector('input[name="workspace_name"]').hasAttribute("maxlength"),
        aliasesHaveMaxLength: [...document.querySelectorAll(".ws-editor__alias input")].some((input) => input.hasAttribute("maxlength")),
      }))()`);
      check(id, !nameLawInputs.workspaceHasMaxLength && !nameLawInputs.aliasesHaveMaxLength, "workspace labels must not use UTF-16 maxlength as authority", nameLawInputs);
      await tab.evaluate(`(() => {
        const row = [...document.querySelectorAll(".ws-editor__pane")].find((node) => node.dataset.wsSession === "ws02");
        row.querySelector(".ws-editor__alias input").value = " padded ";
        row.querySelector(".ws-editor__alias-save").click();
      })()`);
      const paddedAlias = await tab.state();
      check(id, /leading or trailing whitespace/i.test(paddedAlias.editorStatus), "padded aliases must be visibly refused without rewrite", paddedAlias.editorStatus);
      await tab.evaluate(`(() => {
        const row = [...document.querySelectorAll(".ws-editor__pane")].find((node) => node.dataset.wsSession === "ws02");
        row.querySelector(".ws-editor__alias input").value = "\ud800";
        row.querySelector(".ws-editor__alias-save").click();
      })()`);
      const surrogateAlias = await tab.state();
      check(id, /Unicode scalar/i.test(surrogateAlias.editorStatus), "ill-formed surrogate aliases must be visibly refused", surrogateAlias.editorStatus);
      await tab.evaluate(`(() => {
        const row = [...document.querySelectorAll(".ws-editor__pane")].find((node) => node.dataset.wsSession === "ws02");
        const input = row.querySelector(".ws-editor__alias input");
        input.value = "Primary shell";
        input.dispatchEvent(new Event("input", { bubbles: true }));
        row.querySelector(".ws-editor__alias-save").click();
      })()`);
      await tab.evaluate(`(() => {
        const row = [...document.querySelectorAll(".ws-editor__pane")].find((node) => node.dataset.wsSession === "ws01");
        row.querySelector(".ws-editor__remove").click();
      })()`);
      await tab.evaluate(`(() => {
        const input = document.querySelector('input[name="workspace_name"]');
        input.value = "Ops New";
        input.dispatchEvent(new Event("input", { bubbles: true }));
      })()`);
      const draftFixture = await snapshot();
      const draftState = await tab.state();
      check(id, draftState.cells.length === 6 && liveAttachmentOf(draftFixture, "ws01")?.id === beforeAttachments.get("ws01"), "draft removal must not retire or rebuild a pane", { cells: draftState.cells.length, ws01: liveAttachmentOf(draftFixture, "ws01") });
      check(id, draftFixture.counters.workspaceWrites === beforeFixture.counters.workspaceWrites, "draft alias/remove/rename must perform zero durable writes", { before: beforeFixture.counters.workspaceWrites, draft: draftFixture.counters.workspaceWrites });
      await tab.screenshot("w2_workspace_editor_desktop.png");
      await tab.evaluate(`document.querySelector(".ws-editor__save").click()`);
      const saved = await tab.waitUntil((state) => !state.editing && state.cells.length === 5 && state.workspaceTitle === "Ops New", 8_000).then((result) => result.state);
      assert(saved, `${id}: explicit Save did not publish the durable tree`);
      const afterSave = await waitFixture((snap) => liveAttachmentOf(snap, "ws01") === undefined, 5_000);
      assert(afterSave, `${id}: removed controller did not retire after successful Save`);
      check(id, afterSave.counters.workspaceWrites === beforeFixture.counters.workspaceWrites + 1, "Save must issue exactly one PUT", { before: beforeFixture.counters, after: afterSave.counters });
      check(id, SESSIONS.slice(1).every((name) => liveAttachmentOf(afterSave, name)?.id === beforeAttachments.get(name)), "successful Save must preserve every surviving controller/socket", afterSave.attachments.map((attachment) => [attachment.session, attachment.id, attachment.live]));
      check(id, saved.cells.every((cell) => cell.toolbarStamp === `w2-toolbar-${SESSIONS.indexOf(cell.session)}` && cell.toolbarCount === 1 && cell.geometryCount === 1 && cell.fitCount === 1 && cell.composeCount === 1 && cell.moreCount === 0 && cell.quickActionsCount === 1), "compaction and Save must preserve the original real toolbar nodes, committed geometry disclosure, and unique controls", saved.cells);
      check(id, cellOf(saved, "ws02").alias === "Primary shell" && /#name=Ops\+New$/.test(saved.href), "saved alias and canonical renamed URL must publish only after commit", { alias: cellOf(saved, "ws02").alias, href: saved.href });
      check(id, afterSave.attachments.every((attachment) => attachment.resizes === 0), "workspace editor actions must emit zero resize frames", afterSave.attachments.map((attachment) => [attachment.session, attachment.resizes]));

      await tab.evaluate(`document.querySelector(".ws-workspace-edit").click()`);
      await tab.waitUntil((state) => state.editing, 5_000);
      await tab.evaluate(`(() => {
        const row = [...document.querySelectorAll(".ws-editor__pane")].find((node) => node.dataset.wsSession === "ws03");
        row.querySelector(".ws-editor__remove").click();
      })()`);
      const committed = (await snapshot()).workspaces.find((record) => record.normalized_name === "ops new");
      await control({ workspace: { name: committed.name, tree: committed.tree } });
      const beforeConflict = await snapshot();
      await tab.evaluate(`document.querySelector(".ws-editor__save").click()`);
      const conflict = await tab.waitUntil((state) => state.editorConflict, 8_000).then((result) => result.state);
      assert(conflict, `${id}: stale Save did not render the explicit conflict choice`);
      const afterConflict = await snapshot();
      check(id, afterConflict.counters.workspaceWrites === beforeConflict.counters.workspaceWrites + 1, "conflict must issue one request and never retry automatically", { before: beforeConflict.counters, after: afterConflict.counters });
      check(id, SESSIONS.slice(1).every((name) => liveAttachmentOf(afterConflict, name)?.id === beforeAttachments.get(name)), "failed Save must retire no controller", afterConflict.attachments.map((attachment) => [attachment.session, attachment.id, attachment.live]));
      await tab.evaluate(`document.querySelector(".ws-editor__keep-editing").click(); document.querySelector(".ws-editor__cancel").click()`);
      const finalState = await tab.waitUntil((state) => !state.editing, 5_000).then((result) => result.state);
      assert(finalState, `${id}: conflict Cancel did not restore the durable view`);
      await tab.screenshot("w2_durable_editor_desktop.png");
      record(id, {
        writes: { before: beforeFixture.counters.workspaceWrites, afterDrag: afterDragFixture.counters.workspaceWrites, afterSave: afterSave.counters.workspaceWrites, afterConflict: afterConflict.counters.workspaceWrites },
        sockets: Object.fromEntries(SESSIONS.slice(1).map((name) => [name, liveAttachmentOf(afterConflict, name)?.id])),
        toolbarStamps: finalState.cells.map((cell) => [cell.session, cell.toolbarStamp]),
        compactGeometry: { desktop: beforeState.cells.map((cell) => ({ session: cell.session, header: cell.header, toolbar: cell.toolbar, viewportHeight: cell.viewportHeight, rows: cell.rowCount })), wide: wideState.cells.map((cell) => ({ session: cell.session, header: cell.header, toolbar: cell.toolbar })), narrow: narrowState.cells.map((cell) => ({ session: cell.session, header: cell.header, toolbar: cell.toolbar })) },
        divider: { durable: durableDividerValue, dragged: dragged.dividers[0].value, cancelled: cancelled.dividers[0].value },
        screenshots: ["w2_workspace_editor_desktop.png", "w2_durable_editor_desktop.png"],
      });
      await tab.close(debugPort);
    }]);

    // EP4-F6/F7 — six pane-local switch owners share one inventory request,
    // and switching C in place cannot disturb any sibling controller/socket.
    scenarios.push(["EP4", async () => {
      const id = "EP4-F6/F7";
      begin(id);
      const candidates = [...SESSIONS, "ws07"];
      await control({ reset: true, sessions: candidates });
      const tab = await openTab("ep4");
      await seedArrangement(tab, SESSIONS);
      await landOn(tab, workspaceURL(), "resume");
      await trustedOpen(tab);
      await waitLive(tab);
      for (let line = 0; line < 40; line += 1) await control({ writeLiveAll: `EP4-L${line}` });
      await delay(150);
      await tab.evaluate(`(() => {
        for (const cell of document.querySelectorAll(".ws-cell")) {
          const session = cell.dataset.wsSession;
          const xterm = cell.querySelector(".xterm");
          if (xterm) xterm.dataset.ep4Stamp = "xterm-" + session;
          const composer = cell.querySelector(".attachment-page__composer-textarea");
          if (composer) { composer.value = "draft-" + session; composer.dispatchEvent(new Event("input", { bubbles: true })); }
          const viewport = cell.querySelector(".xterm-viewport");
          if (viewport) viewport.scrollTop = Math.max(0, viewport.scrollHeight - viewport.clientHeight - 10);
        }
      })()`);
      const before = await snapshot();
      const beforeState = await tab.state();
      const siblingNames = SESSIONS.filter((name) => name !== "ws03");
      const point = async (source, label) => {
        const value = await tab.evaluate(source);
        assert(value, `${id}: ${label} is unavailable`);
        return value;
      };
      const switchPoint = await point(`(() => { const e=document.querySelector('.ws-cell[data-ws-session="ws03"] .persea-unified-tag'); if(!e)return null; const r=e.getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2}; })()`, "C session tag");
      const inventoryBefore = (await snapshot()).counters.inventory;
      await control({ session: "ws07", projection: "adoptable", adopted: false, foreignHolder: true, holdAdoptionMs: 1_800, holdLeaseRefusalMs: 2_800 });
      await control({ session: "ws03", holdHandleMs: 2_200 });
      // Force the old C source-remint promise to settle even after its
      // controller aborts the transport attempt; the operation-token guard,
      // not AbortSignal timing, must own stale authority.
      await tab.evaluate(`(() => { const original=window.fetch.bind(window); window.fetch=(input,init)=>{ const value=typeof input==='string'?input:input?.url; return original(input,value==='/api/attachment-handles'&&init?{...init,signal:undefined}:init); }; return true; })()`);
      await control({ holdInventoryMs: 250 });
      await tab.trustedClick(switchPoint);
      // While C's request is in flight, open the other five pane-local lists.
      // All six controllers share WorkspaceInventory's one promise.
      const siblingSwitches = await tab.evaluate(`(() => [...document.querySelectorAll('.ws-cell')].filter((c)=>c.dataset.wsSession!=='ws03').map((c)=>{const e=c.querySelector('.persea-unified-tag');const r=e.getBoundingClientRect();return{x:r.x+r.width/2,y:r.y+r.height/2,session:c.dataset.wsSession}}))()`);
      for (const item of siblingSwitches) {
        await tab.trustedClick(item);
      }
      await delay(350);
      const afterSharedFetch = await snapshot();
      check(id, afterSharedFetch.counters.inventory === inventoryBefore + 1, "six pane lists must share one in-flight inventory GET", { before: inventoryBefore, after: afterSharedFetch.counters.inventory });
      // One Escape closes every open pane-local sheet without changing focus;
      // reopen C alone so no sibling overlay can intercept its row tap.
      await tab.evaluate(`document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })); true`);
      await tab.trustedClick(switchPoint);
      await delay(120);
      await tab.evaluate(`(() => { const c=document.querySelector('.ws-cell[data-ws-session="ws03"]'); const e=[...c.querySelectorAll('.persea-session-switcher__row')].find((n)=>n.getAttribute('aria-label')==='Switch to ws07'); e?.scrollIntoView({ block: "center" }); return !!e; })()`);
      await delay(80);
      const rowPoint = await point(`(() => { const c=document.querySelector('.ws-cell[data-ws-session="ws03"]'); const e=[...c.querySelectorAll('.persea-session-switcher__row')].find((n)=>n.getAttribute('aria-label')==='Switch to ws07'); if(!e)return null; const r=e.getBoundingClientRect(); const x=r.x+r.width/2,y=r.y+r.height/2; const hit=document.elementFromPoint(x,y); return {x,y,w:r.width,h:r.height,hidden:e.closest('[hidden]')!==null,hit:hit?.closest('.persea-session-switcher__row')?.getAttribute('aria-label')||hit?.className||hit?.tagName}; })()`, "ws07 row");
      assert(rowPoint.w > 0 && rowPoint.h > 0 && !rowPoint.hidden && rowPoint.hit === "Switch to ws07", `${id}: ws07 row is covered: ${JSON.stringify(rowPoint)}`);
      await tab.trustedClick(rowPoint);
      await delay(35);
      await control({ session: "ws03", closeLive: "generation_rotated" });
      const switched = await tab.waitUntil((s) => {
        const c = s.cells.find((cell) => cell.session === "ws03");
        return !!c && c.rows.includes("fixture-live ws07") && c.state === "live";
      }, 12_000);
      assert(switched.state, `${id}: pane C did not commit ws07: ${JSON.stringify({ cell: switched.last && cellOf(switched.last, "ws03"), fixture: await snapshot() })}`);
      const after = await snapshot();
      const afterState = await tab.state();
      const siblingStable = siblingNames.every((name) => {
        const b = cellOf(beforeState, name), a = cellOf(afterState, name);
        return liveAttachmentOf(before, name)?.id === liveAttachmentOf(after, name)?.id
          && b.rows === a.rows && b.geometry === a.geometry && b.fontSize === a.fontSize
          && b.composer.textareaValue === a.composer.textareaValue
          && b.viewportScrollTop === a.viewportScrollTop && b.xtermStamp === a.xtermStamp;
      });
      check(id, siblingStable, "switching pane C mutated a sibling transcript/socket/draft/scroll/geometry/renderer", { before: beforeState.cells, after: afterState.cells });
      check(id, liveAttachmentOf(after, "ws03") === undefined && liveAttachmentOf(after, "ws07") !== undefined
        && attachmentsOf(after, "ws03").at(-1)?.closeReason === "generation_rotated",
      "pane C's rotated old socket must stay closed while one ws07 socket commits", after.attachments.map((a) => [a.id, a.session, a.closeReason, a.live]));
      const staleCHandles = after.handles.filter((entry) => entry.session === "ws03" && entry.mintedBy === "attachment-handles" && !before.handles.some((prior) => prior.handle === entry.handle));
      const refusedTarget = after.attachments.find((entry) => entry.session === "ws07" && entry.handlePurpose === "control" && entry.closeReason === "lease_held");
      const targetClaim = after.takeoverClaims.at(-1);
      check(id, staleCHandles.length === 1 && staleCHandles[0].consumed === false
        && refusedTarget?.offeredHandle && targetClaim?.kind === "offer" && targetClaim.value === refusedTarget.offeredHandle,
      "stale C remint must stay unconsumed and cannot replace B's exact takeover offer", { staleCHandles, refusedTarget, targetClaim, offers: after.offers });
      check(id, after.attachments.filter((a) => a.live).length === 6 && after.attachments.filter((a) => a.resizes > 0).length === 0,
        "workspace switch must retain six live sockets and emit zero resize frames", after.attachments.map((a) => [a.session, a.live, a.resizes]));
      record(id, { inventory: [inventoryBefore, afterSharedFetch.counters.inventory], before: before.attachments, after: after.attachments, siblings: siblingNames });
      await tab.screenshot("ep4_f6_workspace_switch.png");
      await tab.close(debugPort);

      // C3: C's held reconnect belongs to C while B switches. A module-global
      // operation token would let B invalidate C and strand its minted handle.
      await control({ reset: true, sessions: candidates });
      const tokenTab = await openTab("ep4-controller-token");
      await seedArrangement(tokenTab, SESSIONS);
      await landOn(tokenTab, workspaceURL(), "resume");
      await trustedOpen(tokenTab);
      await waitLive(tokenTab);
      const tokenBefore = await snapshot();
      await control({ session: "ws07", projection: "adoptable", adopted: false, holdAdoptionMs: 900 });
      await control({ session: "ws03", holdHandleMs: 2_200, closeLive: "generation_rotated" });
      const cRemintStarted = await waitFixture((value) => value.counters.handleRequests > tokenBefore.counters.handleRequests, 5_000);
      assert(cRemintStarted, `${id}: pane C did not reach its held remint edge`);
      const bSwitchPoint = await tokenTab.evaluate(`(() => { const e=document.querySelector('.ws-cell[data-ws-session="ws02"] .persea-unified-tag'); if(!e)return null; const r=e.getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2}; })()`);
      assert(bSwitchPoint, `${id}: pane B session tag is unavailable`);
      await tokenTab.trustedClick(bSwitchPoint);
      await delay(100);
      const bRowPoint = await tokenTab.evaluate(`(() => { const c=document.querySelector('.ws-cell[data-ws-session="ws02"]'); const e=[...c.querySelectorAll('.persea-session-switcher__row')].find((n)=>n.getAttribute('aria-label')==='Switch to ws07'); if(!e)return null; e.scrollIntoView({block:'center'}); const r=e.getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2}; })()`);
      assert(bRowPoint, `${id}: pane B ws07 row is unavailable`);
      await tokenTab.trustedClick(bRowPoint);
      const tokenSettled = await tokenTab.waitUntil((s) => cellOf(s, "ws02")?.rows.includes("fixture-live ws07")
        && cellOf(s, "ws03")?.rows.includes("fixture-live ws03") && cellOf(s, "ws03")?.state === "live", 12_000);
      assert(tokenSettled.state, `${id}: B's switch invalidated C's pane-local remint: ${JSON.stringify({ page: tokenSettled.last, fixture: await snapshot() })}`);
      const tokenAfter = await snapshot();
      const cRemintHandles = tokenAfter.handles.filter((entry) => entry.session === "ws03" && entry.mintedBy === "attachment-handles" && !tokenBefore.handles.some((prior) => prior.handle === entry.handle));
      check(id, cRemintHandles.length === 1 && cRemintHandles[0].consumed === true && liveAttachmentOf(tokenAfter, "ws03")?.mintedBy === "attachment-handles",
        "pane C must consume its own remint after pane B switches", { cRemintHandles, liveC: liveAttachmentOf(tokenAfter, "ws03") });
      check(id, tokenAfter.attachments.filter((entry) => entry.live).length === 6 && liveAttachmentOf(tokenAfter, "ws07") !== undefined,
        "the adverse token matrix must settle with six live pane-owned sockets", tokenAfter.attachments.map((entry) => [entry.session, entry.live, entry.mintedBy]));
      record(id, { controllerToken: { before: tokenBefore.attachments, after: tokenAfter.attachments, cRemintHandles } });
      await tokenTab.screenshot("ep4_f7_controller_token.png");
      await tokenTab.close(debugPort);
    }]);

    // E-P5-F4 — one document-global preference service across N controllers.
    // A late controller receives the current value synchronously before its
    // first render; a retired controller is not retained as a publication
    // target. No pane owns its own GET or persistence loop.
    scenarios.push(["EP5", async () => {
      const id = "EP5";
      // An explicit font in the seeded record (J-UX-9 tri-state): the late
      // controller's baseline is then a value only the RECORD can supply —
      // neither the store default (auto) nor the page's construction seed.
      await control({ reset: true, sessions: SESSIONS, preferences: { font_size: 17 } });
      begin(id);
      const tab = await openTab("ep5");
      await tab.navigate(`${origin}/workspace-harness`);
      const ready = await tab.waitUntil((state) => state.harnessReady, 8_000);
      assert(ready.state, `${id}: the workspace page harness never became ready`);
      await control({ resetRequests: true });
      await tab.evaluate(`window.__wsPage.open(${JSON.stringify(arrangementFor(SESSIONS))})`);
      await waitLive(tab);
      await delay(300);
      const initial = await snapshot();
      check(id, initial.counters.preferencesGet === 1, "six pane controllers performed other than one document preference GET", initial.counters);
      // UX15 §15.6: no pane carries a theme control. The theme is a record
      // stored elsewhere (the dashboard card, another tab) and reaches the
      // document's ONE preference service through the `storage` signal, which
      // is exactly one authoritative GET per signal (14.3b) — never one per
      // pane controller.
      const publishRecord = async (patch) => {
        const current = await snapshot();
        await control({ preferences: { ...patch, revision: current.preferences.revision + 1, stored: true } });
        const signalled = await tab.evaluate(`(() => {
          window.dispatchEvent(new StorageEvent("storage", { key: "persea-terminal.operator-preferences-hint.v1", newValue: "{}" }));
          return true;
        })()`);
        assert(signalled, `${id}: the storage signal could not be dispatched`);
      };
      await publishRecord({ theme: "catppuccin-mocha" });
      const saved = await waitFixture((snap) => snap.preferences.theme === "catppuccin-mocha", 8_000);
      assert(saved, `${id}: the shared theme record was not stored`);
      const shared = await tab.waitUntil((state) => state.cells.length === 6 && state.cells.every((cell) => cell.theme === "catppuccin-mocha" && cell.themeValues.every((value) => value === "catppuccin-mocha")), 8_000);
      check(id, !!shared.state, "one controller's preference publication did not reach all six panes", shared.last && shared.last.cells.map((cell) => [cell.session, cell.theme, cell.themeValues]));

      // Retire one controller before adding its replacement: the workspace's
      // six-pane bound stays intact while both disposal and late subscription
      // are exercised.
      const surviving = SESSIONS.filter((name) => name !== "ws01");
      const removed = await tab.evaluate(`window.__wsPage.updateTree(${JSON.stringify(arrangementFor(surviving))})`);
      check(id, removed === true, "the disposal fixture update was refused", { removed });
      await waitLive(tab, surviving);
      const replacement = [...surviving, "ws01"];
      const added = await tab.evaluate(`window.__wsPage.updateTree(${JSON.stringify(arrangementFor(replacement))})`);
      check(id, added === true, "the late-controller fixture update was refused", { added });
      const late = await waitLive(tab, replacement);
      const afterAdd = await snapshot();
      const lateCell = cellOf(late, "ws01");
      check(id, lateCell.theme === "catppuccin-mocha" && lateCell.fontBaseline === 17 && lateCell.themeValues.every((value) => value === "catppuccin-mocha"), "the late controller painted before receiving the current preference", lateCell);
      // One GET at mount plus one per storage signal; adding a controller adds none.
      check(id, afterAdd.counters.preferencesGet === 2, "adding a controller issued another preference GET", afterAdd.counters);

      await publishRecord({ theme: "dracula" });
      assert(await waitFixture((snap) => snap.preferences.theme === "dracula", 8_000), `${id}: second shared theme record was not stored`);
      const disposed = await tab.waitUntil((state) => state.cells.length === replacement.length && state.cells.every((cell) => cell.theme === "dracula"), 8_000);
      check(id, !!disposed.state && disposed.state.cells.filter((cell) => cell.session === "ws01").length === 1, "the disposed controller remained alongside its one replacement", disposed.last && disposed.last.cells);
      const final = await snapshot();
      check(id, final.counters.preferencesGet === 3, "the document preference service multiplied after add/remove", final.counters);
      record(id, {
        gets: final.counters.preferencesGet, puts: final.counters.preferencesPut,
        six: shared.state && shared.state.cells.map((cell) => [cell.session, cell.theme]),
        late: [lateCell.session, lateCell.theme], remaining: disposed.state && disposed.state.cells.map((cell) => [cell.session, cell.theme]),
      });
      await tab.screenshot("ep5_shared_theme_workspace.png");
      await tab.close(debugPort);
    }]);

  const only = (process.env.PERSEA_WS_RUNTIME_ONLY || "").split(",").map((v) => v.trim()).filter(Boolean);
  try {
    for (const [name, run] of scenarios) {
      if (only.length > 0 && !only.some((o) => name.split("/").includes(o))) continue;
      await run();
    }
  } catch (error) {
    const message = `gate error: ${error && error.stack ? error.stack : String(error)}`;
    console.error(message);
    failures.push(message);
  } finally {
    // Teardown is bounded: a browser or fixture that will not close still
    // yields the summary and a nonzero exit instead of a drained event loop.
    const summarize = () => {
      if (globalThis.__gateSummaryPrinted) return;
      globalThis.__gateSummaryPrinted = true;
      evidence.status = failures.length === 0 ? "PASS" : "FAIL";
      evidence.failures = failures;
      evidence.evidenceDir = EVIDENCE_DIR;
      fs.writeFileSync(path.join(EVIDENCE_DIR, "workspace_runtime_gate.json"), JSON.stringify(evidence, null, 2));
      console.log(JSON.stringify({ status: evidence.status, failures: failures.length, evidenceDir: EVIDENCE_DIR }));
      if (failures.length > 0) {
        for (const failure of failures) console.error(failure);
        process.exitCode = 1;
      }
    };
    const watchdog = setTimeout(() => { failures.push("teardown did not complete within 30s"); summarize(); process.exit(1); }, 30_000);
    const bounded = async (label, promise, ms) => {
      let timer;
      const late = new Promise((resolve) => { timer = setTimeout(() => resolve("timeout"), ms); });
      const outcome = await Promise.race([promise.then(() => "done", () => "done"), late]);
      clearTimeout(timer);
      if (outcome === "timeout") failures.push(`${label} did not finish within ${ms}ms`);
    };
    for (const tab of tabs) { try { await bounded(`${tab.name}.close`, tab.close(debugPort), 5_000); } catch { /* closed */ } }
    await bounded("stopChrome", stopChrome(chrome), 12_000);
    await bounded("fixture.close", fixture.close(), 5_000);
    clearTimeout(watchdog);
    summarize();
  }
}

main().catch((error) => {
  console.error(error && error.stack ? error.stack : String(error));
  process.exitCode = 1;
});
