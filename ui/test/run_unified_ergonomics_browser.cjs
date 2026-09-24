"use strict";

// Ergonomics gate (terminal): keyboard only when asked, the quick-actions sheet
// and its one top-bar opener, the eight-key bar, the 44px touch-target law, the composer status
// line, the displacement toast and the one-time fit hint — every scenario
// drives the REAL bundled page in real Chromium against the reopen fixture's
// one-time handles, control lease, takeover offers and broker automaton
// (unified_reopen_fixture.cjs), under `(pointer: coarse)` emulation at iPhone
// metrics, with a fine-pointer golden alongside.
//
//   QF1  no keyboard on first open / reconnect / lease_held takeover / sheet
//        open+close on a coarse pointer; zero INPUT before MODE(CONTROL)
//   QF2  fine pointer: the textarea holds focus after COMMIT, as before
//   QF3  every Keys tile emits its bytes with the textarea unfocused; arrows
//        flip to ESC O A under application-cursor mode
//   QF5  sheet, Keys, View tiles: no RESIZE_REQUEST, no font change except
//        the zoom tiles' own ±1; the ↕ tile emits exactly one
//   QF6  keyboard open at the close → restored on reconnect; closed → not
//   D1   composer status on its own full-width line above the button row
//   D10  the newcomer shows "Control taken from another window" for ~3s
//   D11  the one-time "Tap ↕ to fit rows" hint beside ↕
//   T44  every visible interactive element on the phone terminal page (opener,
//        sheet open, keyboard open, composer open) and the dashboard ≥ 44×44
//
// PERSEA_TERMINAL_EVIDENCE_DIR (optional): screenshots and the census land there.

const fs = require("fs");
const path = require("path");
const { startClipboardFixture: startFixture } = require("./clipboard_fixture.cjs");
const { assert, delay, requestJSON, Tab: BaseTab, launchChrome, stopChrome } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const EVIDENCE = process.env.PERSEA_TERMINAL_EVIDENCE_DIR ? path.resolve(process.env.PERSEA_TERMINAL_EVIDENCE_DIR) : null;
const PHONE = { width: 390, height: 844, deviceScaleFactor: 3, mobile: true };
const PHONE_KEYBOARD = { width: 390, height: 508, deviceScaleFactor: 3, mobile: true };
const DESKTOP = { width: 1024, height: 768, deviceScaleFactor: 1, mobile: false };
const TOAST_TEXT = "Control taken from another window";
const IMAGE_TILE = "Attach image";

const STATE = `(() => {
  const q = (s) => document.querySelector(s);
  const rect = (el) => { if (!el) return null; const r = el.getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height, cx: r.x + r.width / 2, cy: r.y + r.height / 2 }; };
  const visible = (el) => !!el && !el.hidden && !el.closest("[hidden]") && getComputedStyle(el).display !== "none" && el.getClientRects().length > 0;
  // Computed colours arrive as rgb()/rgba() or, for a color-mix() result, as
  // color(srgb r g b) with 0-1 components. An unknown space returns NaN rather
  // than a plausible number: a ratio that cannot be computed must fail closed.
  const srgb = (value) => {
    const text = String(value);
    const numbers = (text.match(/[\\d.]+(?:e[-+]?\\d+)?/gi) ?? []).map(Number);
    if (/^rgba?\\(/i.test(text)) return numbers.slice(0, 3);
    if (/^color\\(\\s*srgb/i.test(text)) return numbers.slice(0, 3).map((channel) => channel * 255);
    return [];
  };
  const alphaOf = (value) => {
    const text = String(value);
    if (text === "transparent") return 0;
    const numbers = (text.match(/[\\d.]+(?:e[-+]?\\d+)?/gi) ?? []).map(Number);
    if (/^rgba\\(/i.test(text)) return numbers.length > 3 ? numbers[3] : 1;
    if (/^color\\(/i.test(text)) return numbers.length > 3 ? numbers[3] : 1;
    return 1;
  };
  const contrast = (foreground, background) => {
    const luminance = (value) => {
      const rgb = srgb(value).map((channel) => channel / 255).map((channel) => channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4);
      return rgb.length === 3 ? 0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2] : NaN;
    };
    const foregroundLuminance = luminance(foreground);
    const backgroundLuminance = luminance(background);
    return (Math.max(foregroundLuminance, backgroundLuminance) + 0.05) / (Math.min(foregroundLuminance, backgroundLuminance) + 0.05);
  };
  // theme-contrast is about what the operator sees, so a transparent element is
  // measured against the surface that actually paints behind it.
  const effectiveBackground = (node) => {
    for (let element = node; element; element = element.parentElement) {
      const style = getComputedStyle(element);
      // display: contents paints nothing at all, so it is not a surface.
      if (style.display === "contents") continue;
      if (alphaOf(style.backgroundColor) > 0) return style.backgroundColor;
    }
    return getComputedStyle(document.documentElement).backgroundColor;
  };
  const controlContrast = (node) => {
    const style = getComputedStyle(node);
    const background = effectiveBackground(node);
    const entry = {
      selector: node.className && typeof node.className === "string" ? node.className : node.tagName,
      label: node.getAttribute("aria-label") || node.textContent || node.tagName,
      foreground: style.color, background, ratio: contrast(style.color, background),
    };
    // The one control whose state is carried by its border: WCAG 1.4.11 wants
    // that boundary visible against both surfaces it separates.
    if (node.matches('.persea-unified-composer-toggle[aria-expanded="true"]')) {
      entry.border = style.borderTopColor;
      entry.borderOnFill = contrast(style.borderTopColor, background);
      entry.borderOnSurface = contrast(style.borderTopColor, node.parentElement ? effectiveBackground(node.parentElement) : background);
    }
    return entry;
  };
  const active = document.activeElement;
  const textarea = q(".xterm-helper-textarea");
  const composerTextarea = q(".attachment-page__composer-textarea");
  const rows = q(".xterm-rows");
  const notice = q(".persea-unified-notice");
  const keyBar = q(".persea-unified-keybar");
  const opener = q(".persea-unified-quick-actions");
  const sheet = q(".persea-unified-sheet");
  const viewPopover = q(".persea-unified-view-popover");
  const toast = q(".persea-unified-toast");
  const fitHint = q(".persea-unified-fit-hint");
  const fit = q(".persea-unified-view-disclosure");
  const more = q(".persea-unified-toolbar__more");
  const composerToggle = q(".persea-unified-composer-toggle");
  const status = q(".attachment-page__composer-status");
  const xtermRows = q(".xterm-rows");
  const tiles = {};
  if (sheet) for (const t of sheet.querySelectorAll(".persea-unified-sheet__tile")) {
    const label = t.querySelector(".persea-unified-sheet__label")?.textContent ?? "";
    const labelNode = t.querySelector(".persea-unified-sheet__label");
    tiles[label] = { disabled: t.disabled, reason: t.querySelector(".persea-unified-sheet__reason")?.textContent ?? "", rect: rect(t), pressed: t.getAttribute("aria-pressed"), labelClipped: !!labelNode && labelNode.scrollWidth > labelNode.clientWidth, ariaLabel: t.getAttribute("aria-label") };
  }
  return {
    href: location.href, ready: document.readyState,
    coarse: matchMedia("(pointer: coarse)").matches,
    screen: rows ? rows.textContent ?? "" : "",
    connection: q(".persea-unified-connection")?.textContent ?? "",
    noticeHidden: notice ? notice.hidden : true,
    code: q(".persea-unified-notice__code")?.textContent ?? "",
    activeKind: active === textarea ? "xterm" : active === composerTextarea ? "composer" : active === document.body ? "body" : ((active && active.tagName ? active.tagName.toLowerCase() : "none") + (active && active.className ? "." + String(active.className).split(" ")[0] : "")),
    keyBarHidden: keyBar ? keyBar.hidden : true, keyboardInset: !!q(".persea-unified-terminal")?.style.height,
    keyBarKeys: keyBar ? Array.from(keyBar.querySelectorAll("button")).map((b) => ({ label: b.textContent, rect: rect(b) })) : [],
    puckPresent: !!q(".persea-unified-puck"),
    openerVisible: visible(opener), openerRect: rect(opener),
    openerExpanded: opener ? opener.getAttribute("aria-expanded") : null,
    openerControls: opener ? opener.getAttribute("aria-controls") : null,
    sheetVisible: visible(sheet), sheetRect: rect(sheet), tiles,
    viewVisible: visible(viewPopover), viewRect: rect(viewPopover),
    // The sheet scrolls internally: its content is taller than its viewport.
    // terminal is about what is reachable WITHOUT that scroll.
    sheetScroll: sheet ? { top: sheet.scrollTop, scrollHeight: sheet.scrollHeight, clientHeight: sheet.clientHeight } : null,
    sheetHeadings: sheet ? Array.from(sheet.querySelectorAll(".persea-unified-sheet__heading")).filter(visible).map((h) => h.textContent) : [],
    toast: visible(toast) ? toast.textContent : "",
    fitHintVisible: visible(fitHint), fitHintRect: rect(fitHint),
    fitDisabled: fit ? fit.disabled : true, fitRect: rect(fit),
    moreVisible: visible(more), composerToggleRect: rect(composerToggle),
    composerOpen: q(".persea-unified-composer-dock")?.dataset.composer === "open",
    composerStatus: status && visible(status) ? { text: status.value ?? status.textContent, rect: rect(status), scrollWidth: status.scrollWidth, clientWidth: status.clientWidth } : null,
    composerActionsRect: rect(q(".attachment-page__composer-actions")),
    composerButtons: Array.from(document.querySelectorAll(".attachment-page__composer .attachment-page__button")).filter(visible).map((b) => ({ label: b.getAttribute("aria-label") || b.textContent, rect: rect(b) })),
    gripRect: rect(q(".attachment-page__composer-grip")),
    fontSize: xtermRows ? parseFloat(getComputedStyle(xtermRows).fontSize) : null,
    cellHeight: (() => { const s = q(".xterm-screen"); const r = s ? s.getBoundingClientRect() : null; return r && rows ? r.height / rows.children.length : null; })(),
    terminalRows: rows ? rows.children.length : 0,
    hostRect: rect(q(".persea-unified-xterm")),
    innerWidth: innerWidth, innerHeight: innerHeight,
    focusCalls: typeof window.__terminalFocusCalls === "number" ? window.__terminalFocusCalls : null,
    visualViewportHeight: window.visualViewport ? window.visualViewport.height : null,
    styleNodes: document.querySelectorAll("style").length,
    theme: q(".persea-unified-terminal")?.dataset.theme ?? "",
    fontBaseline: Number(q(".persea-unified-terminal")?.dataset.fontBaseline ?? "NaN"),
    // The terminal topbar tri-state verbatim: "9".."24" for an explicit preference, or
    // "auto". fontBaseline above stays the numeric view and reads NaN on auto.
    fontPreference: q(".persea-unified-terminal")?.dataset.fontBaseline ?? null,
    bodyTheme: document.body.dataset.perseaTheme ?? "",
    themeValues: Array.from(document.querySelectorAll(".persea-unified-preference__theme")).map((select) => select.value),
    themeOptions: Array.from(q(".persea-unified-preference__theme")?.options ?? []).map((option) => option.value),
    preferenceStatus: Array.from(document.querySelectorAll(".persea-unified-preference-status")).filter(visible).map((node) => node.textContent ?? ""),
    preferenceTargets: Array.from(document.querySelectorAll(".persea-unified-preference__theme")).filter(visible).map(rect),
    preferenceContrast: Array.from(document.querySelectorAll([
      ".persea-unified-toolbar button",
      ".persea-unified-preference__theme",
      ".persea-unified-preference__label",
      ".persea-unified-geometry",
      ".persea-unified-preference-status",
      ".persea-unified-tag__name",
      ".persea-unified-tag__alias",
      ".persea-unified-identity__row dt",
      ".persea-unified-identity__row dd",
      ".persea-unified-sheet__status",
      ".persea-unified-sheet__heading",
      ".persea-unified-sheet__tile",
      ".persea-unified-sheet__label",
    ].join(", "))).map(controlContrast),
    // The session tag's status dot (U3). Its meaning is carried by a fill
    // rather than by text, so WCAG 1.4.11 applies: the fill must clear 3:1
    // against the surface that actually paints behind it, on every theme.
    //
    // EVERY state, not the one this page happens to be in. Reading the live
    // page measures only the live state, because a committed attachment is
    // always live -- which is how a down fill that failed on four of the
    // eight themes went unmeasured. Each state is forced onto the dot, read
    // synchronously, and the real state put back; the page re-derives the dot
    // from connectionPhase() on every render, so nothing downstream depends
    // on the value left here.
    dotContrast: (() => {
      const dot = q(".persea-unified-tag__dot");
      if (!dot) return null;
      const actual = dot.dataset.state ?? "";
      const background = effectiveBackground(dot.parentElement);
      const states = ["live", "pending", "down"].map((state) => {
        dot.dataset.state = state;
        const fill = getComputedStyle(dot).backgroundColor;
        return { state, fill, ratio: contrast(fill, background) };
      });
      dot.dataset.state = actual;
      return { actual, background, states };
    })(),
    // The two toolbar affordances whose meaning is carried by their own
    // surface. The closed-theme rule repaints every toolbar button, so these
    // must be read back from computed styles, not assumed from source order.
    stateAffordances: (() => {
      const box = (element) => {
        if (!element) return null;
        const style = getComputedStyle(element);
        return { color: style.color, background: style.backgroundColor, border: style.borderTopColor };
      };
      const toggle = q(".persea-unified-composer-toggle");
      const view = q(".persea-unified-view-disclosure");
      return {
        view: box(view),
        viewExpanded: view ? view.getAttribute("aria-expanded") : null,
        toggle: box(toggle),
        toggleExpanded: toggle ? toggle.getAttribute("aria-expanded") : null,
        plainButton: box(q(".persea-unified-copy")),
      };
    })(),
    paint: (() => {
      const shell = q(".persea-unified-terminal");
      const host = q(".persea-unified-xterm");
      return {
        shellBackground: shell ? getComputedStyle(shell).backgroundColor : "",
        shellColor: shell ? getComputedStyle(shell).color : "",
        hostBackground: host ? getComputedStyle(host).backgroundColor : "",
      };
    })(),
    selectionText: String(document.getSelection() ?? ""),
    scrollY: window.scrollY,
    sheetStatus: q(".persea-unified-sheet__status")?.textContent ?? "",
    composerDraft: q(".attachment-page__composer-textarea")?.value ?? null,
  };
})()`;

// Every visible interactive element and its rendered box.
const CENSUS = `(() => {
  const out = [];
  const seen = new Set();
  for (const el of document.querySelectorAll('button, a[href], input, select, textarea, [role="button"], [tabindex]:not([tabindex="-1"])')) {
    if (seen.has(el)) continue;
    seen.add(el);
    if (el.matches(".xterm-helper-textarea")) continue;
    if (el.closest("[hidden]")) continue;
    const cs = getComputedStyle(el);
    if (cs.display === "none" || cs.visibility === "hidden" || Number(cs.opacity) === 0) continue;
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) continue;
    // Visually-hidden (clip rect) elements are announced, not tapped.
    if (cs.clip === "rect(0px, 0px, 0px, 0px)") continue;
    const label = (el.getAttribute("aria-label") || (el.textContent || "").trim() || el.getAttribute("title") || el.tagName).slice(0, 40);
    out.push({ tag: el.tagName.toLowerCase(), cls: String(el.className || "").split(" ")[0], label, w: Math.round(r.width * 100) / 100, h: Math.round(r.height * 100) / 100 });
  }
  return out;
})()`;

class Tab extends BaseTab {
  static open(name, debugPort, origin) {
    return super.open(name, debugPort, origin, STATE);
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

  // Activation that matches the emulated pointer: a real touch tap on a coarse
  // medium, a real mouse click on a fine one. Touch events are not delivered
  // while touch emulation is off, so a scenario that runs on both must not
  // hardcode either.
  async activate(state, point) {
    if (state.coarse) { await this.tap(point); return; }
    const target = point && typeof point.cx === "number" ? { x: point.cx, y: point.cy } : point;
    assert(target && target.x > 0 && target.y > 0, `${this.name}: activation target is not rendered: ${JSON.stringify(point)}`);
    await this.trustedClick(target);
  }

  // Taps land on a box's centre (rects carry cx/cy) or on an explicit point.
  async tap(point) {
    const target = point && typeof point.cx === "number" ? { x: point.cx, y: point.cy } : point;
    assert(target && target.x > 0 && target.y > 0, `${this.name}: tap target is not rendered: ${JSON.stringify(point)}`);
    await super.tap(target);
  }

  // Center of a sheet tile by label, scrolled into the sheet's view first.
  async tilePoint(label) {
    label = ({ Show: "Show keyboard", Hide: "Hide keyboard", Insert: "Attach image", Switch: "Switch session" })[label] || label;
    const keyIDs = { Escape: "escape", Tab: "tab", Left: "arrow-left", Down: "arrow-down", Up: "arrow-up", Right: "arrow-right", Home: "home", End: "end", "Page up": "page-up", "Page down": "page-down", Delete: "delete", Backspace: "backspace", Control: "ctrl" };
    if (keyIDs[label]) {
      const opened = await this.evaluate(`!document.querySelector('.persea-terminal-keys').hidden`);
      if (!opened) { await this.tap(await this.tilePoint("Keys")); await delay(100); }
      const keyControl = async id => this.pointFor(`document.querySelector('.persea-terminal-keys [data-key-control=' + ${JSON.stringify(JSON.stringify(id))} + ']')`);
      if (await this.evaluate(`document.querySelector('.persea-terminal-keys').dataset.view !== 'All keys'`)) { await this.tap(await keyControl("view:All keys")); await delay(70); }
      const group = await this.evaluate(`document.querySelector('.persea-terminal-keys').dataset.group`);
      if (group !== "Navigation") { await this.tap(await keyControl("key-group")); await this.tap(await keyControl("choose-group:Navigation")); await delay(70); }
      return keyControl(label === "Control" ? "modifier:ctrl" : `key:${keyIDs[label]}:0`);
    }
    const viewLabel = label === "Fit height" ? "Fit rows" : label;
    if (["Zoom out", "Fit font", "Zoom in", "Fit rows"].includes(viewLabel)) {
      const open = await this.evaluate(`document.querySelector(".persea-unified-view-disclosure")?.getAttribute("aria-expanded") === "true"`);
      if (!open) {
        const disclosure = await this.pointFor(`document.querySelector(".persea-unified-view-disclosure")`);
        assert(disclosure, `${this.name}: View and appearance disclosure is absent`);
        await this.tap(disclosure);
        await delay(120);
      }
      const point = await this.pointFor(`Array.from(document.querySelectorAll(".persea-unified-view-popover button")).find((button) => ((button.getAttribute("aria-label") || button.textContent || "").trim()) === ${JSON.stringify(viewLabel)})`);
      assert(point, `${this.name}: View and appearance control ${viewLabel} is absent`);
      return point;
    }
    const sheetOpen = await this.evaluate(`document.querySelector(".persea-unified-quick-actions")?.getAttribute("aria-expanded") === "true"`);
    if (!sheetOpen) {
      const opener = await this.pointFor(`document.querySelector(".persea-unified-quick-actions")`);
      assert(opener, `${this.name}: Quick actions disclosure is absent`);
      await this.tap(opener);
      await delay(120);
    }
    const point = await this.evaluate(`(() => {
      const sheet = document.querySelector(".persea-unified-sheet");
      const tile = sheet && Array.from(sheet.querySelectorAll(".persea-unified-sheet__tile")).find((t) => (t.querySelector(".persea-unified-sheet__label")?.textContent ?? "") === ${JSON.stringify(label)});
      if (!tile) return null;
      tile.scrollIntoView({ block: "center", inline: "nearest" });
      const r = tile.getBoundingClientRect();
      return { x: r.x + r.width / 2, y: r.y + r.height / 2, disabled: tile.disabled, w: r.width, h: r.height };
    })()`);
    assert(point, `${this.name}: sheet tile ${label} is absent`);
    return point;
  }

  // The centre box of an arbitrary element, scrolled into the sheet's view
  // first (the snippet list scrolls inside the sheet).
  async pointFor(expression) {
    return this.evaluate(`(() => {
      const el = ${expression};
      if (!el) return null;
      el.scrollIntoView({ block: "center", inline: "nearest" });
      const r = el.getBoundingClientRect();
      return { x: r.x + r.width / 2, y: r.y + r.height / 2, cx: r.x + r.width / 2, cy: r.y + r.height / 2, w: r.width, h: r.height, disabled: el.disabled === true };
    })()`);
  }

  async tapTile(label) {
    const point = await this.tilePoint(label);
    await this.tap(point);
    await delay(120);
    return point;
  }

  async pressKey(key, code, keyCode) {
    await this.cdp.send("Input.dispatchKeyEvent", { type: "keyDown", key, code, windowsVirtualKeyCode: keyCode, nativeVirtualKeyCode: keyCode });
    await this.cdp.send("Input.dispatchKeyEvent", { type: "keyUp", key, code, windowsVirtualKeyCode: keyCode, nativeVirtualKeyCode: keyCode });
  }

  async screenshot(name) {
    if (!EVIDENCE) return null;
    const shot = await this.cdp.send("Page.captureScreenshot", { format: "png" });
    const dir = path.join(EVIDENCE, "shots");
    fs.mkdirSync(dir, { recursive: true });
    const file = path.join(dir, `${name}.png`);
    fs.writeFileSync(file, Buffer.from(shot.data, "base64"));
    return path.basename(file);
  }

  census() {
    return this.evaluate(CENSUS);
  }
}

const isLive = (state) => state.screen.includes("fixture-live") && state.noticeHidden && state.connection === "";
const hasNotice = (state) => !state.noticeHidden;
// The composer grip is a drag handle with its own law (≥24px tall, asserted
// in D1); every other target is 44×44.
const under44 = (census) => census.filter((item) => item.cls !== "attachment-page__composer-grip" && (item.w < 43.5 || item.h < 43.5));
// While the sheet is open the opener may cover no tile: each tile's box is
// clipped to the sheet's visible box first (a tile scrolled out of the sheet
// is not on screen), then tested against the opener's box. Since U5 the
// opener is a control-row button and the sheet is docked to the stage's
// bottom edge, so this holds by composition — it is asserted anyway, because
// that composition is exactly what a later layout change could break.
const clipRect = (r, box) => {
  if (!r || !box) return null;
  const x1 = Math.max(r.x, box.x), y1 = Math.max(r.y, box.y), x2 = Math.min(r.x + r.w, box.x + box.w), y2 = Math.min(r.y + r.h, box.y + box.h);
  return x2 - x1 > 0.5 && y2 - y1 > 0.5 ? { x: x1, y: y1, w: x2 - x1, h: y2 - y1 } : null;
};
const intersects = (a, b) => !!a && !!b && a.x < b.x + b.w - 0.5 && b.x < a.x + a.w - 0.5 && a.y < b.y + b.h - 0.5 && b.y < a.y + a.h - 0.5;
const tilesUnderOpener = (state) => Object.entries(state.tiles).filter(([, t]) => intersects(clipRect(t.rect, state.sheetRect), state.openerRect)).map(([label]) => label);

async function main() {
  const fixture = await startFixture(UI);
  const origin = fixture.origin;
  const control = (input) => requestJSON(`${origin}/__fixture/control`, "POST", input);
  const snapshot = () => requestJSON(`${origin}/__fixture/control`);
  const freshControlHandle = async () => {
    const inventory = await requestJSON(`${origin}/api/inventory`);
    return inventory.realms[0].servers[0].sessions[0].handles.control;
  };
  const unifiedURL = (handle, extra = {}) => `${origin}/terminal?engine=unified-dev#${new URLSearchParams({
    handle, mode: "control", history: "1000", name: "alpha", draft_scope: fixture.draftScope, engine: "unified-dev", ...extra,
  }).toString()}`;
  const lastAttachment = async () => {
    const current = await snapshot();
    return current.attachments[current.attachments.length - 1];
  };
  const firstInputIndex = (frames) => frames.findIndex((frame) => frame.startsWith("INPUT:"));

  const chrome = await launchChrome("persea-terminal-unified-ergonomics-");
  const tabs = [];
  const failures = [];
  const evidence = {};
  const shots = [];
  const fail = (scenario, message, detail) => failures.push(`${scenario}: ${message} ${JSON.stringify(detail)}`);
  const shot = async (tab, name) => { const file = await tab.screenshot(name); if (file) shots.push(file); };
  try {
    const tabA = await Tab.open("A", chrome.debugPort, origin);
    tabs.push(tabA);

    // --- QF2 fine pointer golden: the textarea holds focus after COMMIT ------
    await tabA.emulate(DESKTOP, false);
    await control({ reset: true });
    await tabA.navigate(unifiedURL(await freshControlHandle()));
    const fine = await tabA.waitUntil(isLive);
    assert(fine.state, `QF2 precondition: a fresh handle did not reach live control: ${JSON.stringify(fine.last)}`);
    await delay(100);
    const fineState = await tabA.state();
    evidence.qf2 = { activeKind: fineState.activeKind, keyBarHidden: fineState.keyBarHidden, openerVisible: fineState.openerVisible, puckPresent: fineState.puckPresent, coarse: fineState.coarse };
    if (fineState.coarse) fail("QF2", "the fine-pointer emulation did not take", evidence.qf2);
    if (fineState.puckPresent) fail("QF2", "the floating puck is still in the document", evidence.qf2);
    if (!fineState.openerVisible) fail("QF2", "a fine pointer has no sheet opener in the control row", evidence.qf2);
    if (fineState.activeKind !== "xterm") fail("QF2", "on a fine pointer the textarea does not hold focus after COMMIT", evidence.qf2);
    await shot(tabA, "desktop-00-terminal-fine-pointer");
    if (!fineState.keyBarHidden) fail("QF2", "a fine pointer sees the key bar", evidence.qf2);
    // A reconnect on a fine pointer claims focus again, as before.
    await control({ closeLive: "websocket_read" });
    const fineBack = await tabA.waitUntil((state) => isLive(state) && state.activeKind === "xterm", 8_000);
    if (!fineBack.state) fail("QF2", "after a reconnect on a fine pointer the textarea did not hold focus", fineBack.last);

    // --- QF1a coarse first open: no focus, no key bar, zero INPUT ---------------
    // (QF1: keyboard only when asked. A first admission never arms the
    // one-shot restore; QF6 below is the only path that focuses after COMMIT.)
    await tabA.emulate(PHONE, true);
    await control({ reset: true, replayFocusReporting: true });
    const firstURL = unifiedURL(await freshControlHandle());
    await tabA.navigate(firstURL);
    const first = await tabA.waitUntil(isLive);
    assert(first.state, `QF1a precondition: a fresh handle did not reach live control on a coarse pointer: ${JSON.stringify(first.last)}`);
    await delay(300);
    const firstState = await tabA.state();
    const firstAttachment = await lastAttachment();
    // F1: the wire RESIZE_REQUEST count, recorded before the sheet is first
    // opened. Every presentation action below — sheet open/close, every Keys
    // action, both Control-latch transitions, each View action — must leave
    // it unchanged; only the trusted ↕ may advance it, by exactly one.
    const resizeBaseline = firstAttachment.resizes || 0;
    evidence.qf5 = { resizeBaseline, unchangedAfter: [] };
    const assertNoResize = async (label) => {
      const now = (await lastAttachment()).resizes || 0;
      evidence.qf5.unchangedAfter.push({ label, resizes: now });
      if (now !== resizeBaseline) fail("QF5", `${label} emitted RESIZE_REQUEST (wire count ${resizeBaseline} → ${now})`, { label, resizeBaseline, now });
    };
    evidence.qf1a = { activeKind: firstState.activeKind, keyBarHidden: firstState.keyBarHidden, openerVisible: firstState.openerVisible, openerRect: firstState.openerRect, moreVisible: firstState.moreVisible, frames: firstAttachment.frames, inputs: firstAttachment.inputs, coarse: firstState.coarse };
    if (!firstState.coarse) fail("QF1a", "the coarse-pointer emulation did not take", evidence.qf1a);
    if (firstState.activeKind === "xterm" || firstState.activeKind === "composer") fail("QF1a", "a coarse first open focused a text entry (keyboard raised without being asked)", evidence.qf1a);
    if (!firstState.keyBarHidden) fail("QF1a", "the key bar showed with the keyboard closed", evidence.qf1a);
    if (!firstState.openerVisible || firstState.openerRect.w < 43.5 || firstState.openerRect.h < 43.5) fail("QF1a", "the sheet opener is absent or under 44px", evidence.qf1a);
    if (firstState.moreVisible) fail("QF1a", "the toolbar ⋯ renders on a coarse pointer", evidence.qf1a);
    if (firstAttachment.frames[0] !== "READY" || firstAttachment.frames[1] !== "MODE_REQUEST") fail("QF1a", "the attachment did not carry READY then MODE_REQUEST", evidence.qf1a);
    if (firstInputIndex(firstAttachment.frames) !== -1) fail("QF1a", "the page emitted INPUT (a focus-in report) on a coarse first open", evidence.qf1a);
    await shot(tabA, "phone-01-terminal-closed-keyboard-opener");

    // --- D11 the compact phone fit affordance ---------------------------------
    // terminal interaction reserves the fixed six-control phone row for tag · ↕ · Aa · Copy ·
    // ☰ · ✎. The one-time prose hint therefore stays out of the compact row;
    // the always-present ↕ control remains the sole explicit-fit affordance.
    evidence.d11 = { hintVisible: firstState.fitHintVisible, hintRect: firstState.fitHintRect, terminalRows: firstState.terminalRows, cellHeight: firstState.cellHeight };
    if (firstState.fitHintVisible) fail("D11", "the retired prose hint consumed compact phone-row space", evidence.d11);

    // --- QF1d + sheet: open/close makes no focus call; tiles present ----------
    await tabA.tap((await tabA.state()).openerRect); // always the live rect
    await delay(150);
    const open = await tabA.state();
    await assertNoResize("sheet open");
    evidence.qf1d = { open: { sheetVisible: open.sheetVisible, openerExpanded: open.openerExpanded, activeKind: open.activeKind, keyBarHidden: open.keyBarHidden, sheetRect: open.sheetRect, tiles: Object.fromEntries(Object.entries(open.tiles).map(([k, v]) => [k, { disabled: v.disabled, reason: v.reason, w: v.rect && v.rect.w, h: v.rect && v.rect.h }])) } };
    if (!open.sheetVisible || open.openerExpanded !== "true") fail("QF1d", "the opener did not open the sheet", evidence.qf1d);
    // The tap's synthesized click must not reach a tile that the opening
    // sheet placed under the finger (it navigated to the legacy page once).
    if (open.href !== firstState.href) fail("QF1d", "the opener tap's synthesized click reached a tile: the page navigated", { before: firstState.href, after: open.href });
    if (open.activeKind !== firstState.activeKind || open.keyboardInset) fail("QF1d", "opening the sheet changed focus or showed the key bar", evidence.qf1d);
    if (open.sheetRect && (open.sheetRect.y < 0 || open.sheetRect.y + open.sheetRect.h > open.innerHeight + 1)) fail("QF1d", "the task menu exceeds the viewport", evidence.qf1d);
    evidence.qf1d.open.clippedLabels = Object.entries(open.tiles).filter(([, t]) => t.labelClipped).map(([label]) => label);
    if (evidence.qf1d.open.clippedLabels.length) fail("QF1d", "a tile's primary label ellipsizes at 390pt", evidence.qf1d.open.clippedLabels);
    evidence.qf1d.open.tilesUnderOpener = tilesUnderOpener(open);
    evidence.qf1d.open.openerRect = open.openerRect;
    if (evidence.qf1d.open.tilesUnderOpener.length) fail("QF1d", "the opener covers sheet tiles while the sheet is open", { tiles: evidence.qf1d.open.tilesUnderOpener, opener: open.openerRect, sheet: open.sheetRect });
    if (open.openerRect && open.sheetRect && open.openerRect.y + open.openerRect.h > open.sheetRect.y + 0.5) fail("QF1d", "the opener is not above the sheet it opens", { opener: open.openerRect, sheet: open.sheetRect });
	for (const label of ["Clipboard", "Keys", "Composer", "Dashboard", "Switch session", "Attach image", "Show keyboard", "Hide keyboard"]) {
      if (!open.tiles[label]) fail("QF1d", `sheet tile ${label} is absent`, Object.keys(open.tiles));
    }
	const primaryClipboard = await tabA.evaluate(`(() => {
	  const select = document.querySelector(".persea-unified-select-context");
	  const paste = document.querySelector(".persea-unified-toolbar-paste");
	  return {
	    select: select && { text: select.textContent.trim(), disabled: select.disabled, state: select.dataset.selectState },
	    paste: paste && { text: paste.textContent.trim(), label: paste.getAttribute('aria-label'), popup: paste.getAttribute('aria-haspopup'), icons: paste.querySelectorAll('svg').length, disabled: paste.disabled },
	  };
	})()`);
	evidence.qf1d.open.primaryClipboard = primaryClipboard;
	if (!primaryClipboard.select || primaryClipboard.select.text !== "Select" || primaryClipboard.select.disabled || primaryClipboard.select.state !== "select") fail("QF1d", "primary contextual Select is absent or unavailable", primaryClipboard);
	if (!primaryClipboard.paste || primaryClipboard.paste.label !== "Open clipboard" || primaryClipboard.paste.popup !== "dialog" || primaryClipboard.paste.icons !== 1 || primaryClipboard.paste.disabled) fail("QF1d", "primary Paste is absent or unavailable", primaryClipboard);
	if (open.tiles.Select || open.tiles.Copy || open.tiles.Paste) fail("QF1d", "Quick actions duplicates a primary clipboard control", { Select: open.tiles.Select, Copy: open.tiles.Copy, Paste: open.tiles.Paste });
    // TERMINAL-3: clipboard, session switch and preferences are all real; no future-phase placeholder
    // tile and no header for an empty section remains in the composed sheet.
    const placeholders = [];
    const placeholderReasons = Object.entries(open.tiles).filter(([, t]) => /Coming soon/i.test(t.reason)).map(([label]) => label);
    evidence.qf1d.open.placeholders = placeholders;
    evidence.qf1d.open.headings = open.sheetHeadings;
    if (placeholders.length || placeholderReasons.length) fail("QF1d", "placeholder (coming-soon) tiles render", { placeholders, placeholderReasons });
    // terminal moved Keyboard to the front so it lands inside the initial sheet
    // viewport; CLIPBOARD-J5 below measures that it actually does.
	if (open.sheetHeadings.join(" ") !== "Tools Keyboard Sessions Settings and help") fail("QF1d", "the task menu's sections are not Tools, Keyboard, Sessions, Settings and help", open.sheetHeadings);
    if (["Snippets", "Clips", "Escape", "Tab", "Control"].some(label => open.tiles[label])) fail("QF1d", "retired storage tiles or individual key tiles remain in the task menu", Object.keys(open.tiles));
    if (open.tiles.Prev || open.tiles.Next) fail("QF1d", "arbitrary Prev/Next session cycling remains in Quick actions", { Prev: open.tiles.Prev, Next: open.tiles.Next });
    if (open.tiles[IMAGE_TILE] && (!open.tiles[IMAGE_TILE].disabled || !/Images unavailable/.test(open.tiles[IMAGE_TILE].reason))) fail("QF1d", "Attach image is not disabled with its staging reason", open.tiles[IMAGE_TILE]);
    if (open.tiles["Show keyboard"]?.disabled) fail("QF1d", "Show keyboard is disabled with the keyboard closed", open.tiles["Show keyboard"]);
    if (!open.tiles["Hide keyboard"]?.disabled) fail("QF1d", "Hide keyboard is enabled with the keyboard closed", open.tiles["Hide keyboard"]);
    await shot(tabA, "phone-02-terminal-sheet-open");
    // Outside tap: the gap in the toolbar between ↕ (or its hint) and ✎.
    const gapX = ((open.fitHintVisible ? open.fitHintRect.x + open.fitHintRect.w : open.fitRect.x + open.fitRect.w) + open.composerToggleRect.x) / 2;
    await tabA.tap({ x: gapX, y: open.fitRect.cy });
    await delay(150);
    const outside = await tabA.state();
    await assertNoResize("outside dismissal");
    evidence.qf1d.outside = { sheetVisible: outside.sheetVisible, activeKind: outside.activeKind, keyBarHidden: outside.keyBarHidden, gapX };
    if (outside.sheetVisible) fail("QF1d", "a pointer outside the sheet did not dismiss it", evidence.qf1d.outside);
    if (outside.activeKind !== firstState.activeKind || outside.keyboardInset) fail("QF1d", "dismissing the sheet changed focus or showed the key bar", evidence.qf1d.outside);
    await tabA.tap((await tabA.state()).openerRect); // always the live rect
    await delay(150);
    await tabA.pressKey("Escape", "Escape", 27);
    await delay(150);
    const escaped = await tabA.state();
    await assertNoResize("Escape dismissal");
    evidence.qf1d.escape = { sheetVisible: escaped.sheetVisible, activeKind: escaped.activeKind };
    if (escaped.sheetVisible) fail("QF1d", "Escape did not dismiss the sheet", evidence.qf1d.escape);
    if (escaped.activeKind !== firstState.activeKind) fail("QF1d", "Escape dismissal changed focus", evidence.qf1d.escape);
    const afterDismiss = await lastAttachment();
    if (firstInputIndex(afterDismiss.frames) !== -1) fail("QF1d", "sheet open/close emitted INPUT", afterDismiss.frames);

    // --- QF3 Keys tiles with the textarea unfocused ---------------------------
    const expectedNormal = [
      ["Escape", "\x1b"], ["Tab", "\t"], ["Left", "\x1b[D"], ["Down", "\x1b[B"], ["Up", "\x1b[A"], ["Right", "\x1b[C"],
      ["Home", "\x1b[H"], ["End", "\x1b[F"], ["Page up", "\x1b[5~"], ["Page down", "\x1b[6~"], ["Delete", "\x1b[3~"], ["Backspace", "\x7f"],
    ];
    const expectedApplication = [["Left", "\x1bOD"], ["Down", "\x1bOB"], ["Up", "\x1bOA"], ["Right", "\x1bOC"], ["Home", "\x1bOH"], ["End", "\x1bOF"]];
    await tabA.tap((await tabA.state()).openerRect); // always the live rect
    await delay(150);
    evidence.qf3 = { normal: {}, application: {}, activeKindDuring: [] };
    const keyProbe = async (bucket, label, expected) => {
      const before = (await lastAttachment()).inputs.length;
      await tabA.tapTile(label);
      const after = await lastAttachment();
      const delta = after.inputs.slice(before).join("");
      const active = (await tabA.state()).activeKind;
      await assertNoResize(`${bucket} ${label}`);
      evidence.qf3[bucket][label] = { expected: JSON.stringify(expected), got: JSON.stringify(delta), activeKind: active };
      if (delta !== expected) fail("QF3", `${bucket} ${label} emitted ${JSON.stringify(delta)}, expected ${JSON.stringify(expected)}`, evidence.qf3[bucket][label]);
      if (active === "xterm") fail("QF3", `${label} focused the textarea (keyboard raised)`, evidence.qf3[bucket][label]);
    };
    for (const [label, expected] of expectedNormal) await keyProbe("normal", label, expected);
    // Application cursor keys: the fixture writes DECSET 1 into the live
    // stream; xterm's evaluator — not the page — must flip the arrows.
    await control({ writeLive: "\x1b[?1h" });
    await delay(150);
    for (const [label, expected] of expectedApplication) await keyProbe("application", label, expected);
    await control({ writeLive: "\x1b[?1l" });
    await delay(150);
    // Ctrl now opens explicit panel composition. Native text is never latched.
    await tabA.tapTile("Control");
    const chordPanel = await tabA.evaluate(`(() => {
      const panel = document.querySelector('.persea-terminal-keys');
      return { open: !panel.hidden, ctrl: panel.querySelector('[data-key-control="modifier:ctrl"]')?.getAttribute('aria-pressed') };
    })()`);
    await assertNoResize("Control chord panel");
    if (!chordPanel.open || chordPanel.ctrl !== "true") fail("QF3", "Control did not open explicit composition", chordPanel);
    await tabA.tapTile("Control");
    if (await tabA.evaluate(`document.querySelector('.persea-terminal-keys [data-key-control="modifier:ctrl"]')?.getAttribute('aria-pressed') !== 'false'`)) fail("QF3", "a second modifier tap did not cancel Ctrl", {});
    await assertNoResize("Clear panel modifiers");
    await tabA.tap((await tabA.state()).openerRect);

    // --- QF5 presentation only -------------------------------------------------
    // The fitted font sits at its 9px floor on a phone (80 columns need
    // 433px), so zoom in first, then out.
    //
    // a zoom step is one pixel from the size xterm is RENDERING, not
    // from the stored preference. On this phone the two differ by six pixels —
    // the fit floors at 9 while nothing is stored — so an implementation that
    // steps the stored number lands on 15 here and this case says so.
    const beforeState = await tabA.state();
    const fontBefore = beforeState.fontSize;
    await tabA.tapTile("Zoom in");
    const zoomedIn = await tabA.state();
    await assertNoResize("Zoom in");
    await tabA.tapTile("Zoom out");
    const zoomedOut = await tabA.state();
    await assertNoResize("Zoom out");
    await tabA.tapTile("Fit font");
    await delay(200);
    const fitted = await tabA.state();
    await assertNoResize("Fit font");
    Object.assign(evidence.qf5, {
      fontBefore, fontPreferenceBefore: beforeState.fontPreference,
      zoomedIn: zoomedIn.fontSize, zoomedInPreference: zoomedIn.fontPreference,
      zoomedOut: zoomedOut.fontSize, zoomedOutPreference: zoomedOut.fontPreference,
      fitted: fitted.fontSize, fittedPreference: fitted.fontPreference, activeKind: fitted.activeKind,
      expectedIn: Math.round(fontBefore) + 1, expectedOut: Math.round(fontBefore),
    });
    if (beforeState.fontPreference !== "auto") fail("QF5", "the phone did not start on auto-fit", evidence.qf5);
    if (zoomedIn.fontSize !== Math.round(fontBefore) + 1) fail("QF5", "Zoom in did not step one pixel from the rendered size", evidence.qf5);
    if (zoomedIn.fontPreference !== String(Math.round(fontBefore) + 1)) fail("QF5", "Zoom in did not become the explicit preference", evidence.qf5);
    if (zoomedOut.fontSize !== Math.round(fontBefore)) fail("QF5", "Zoom out did not step one pixel back", evidence.qf5);
    if (fitted.fontPreference !== "auto") fail("QF5", "Fit font did not restore auto-fit", evidence.qf5);
    if (fitted.activeKind === "xterm") fail("QF5", "a View tile focused the textarea", evidence.qf5);
    // The ↕ tile: the one tile on the geometry path, exactly one request.
    const fitTile = await tabA.tilePoint("Fit height");
    if (fitTile.disabled) fail("QF5", "the ↕ tile is disabled on a live, granted attachment", { fitTile, tiles: (await tabA.state()).tiles["Fit height"] });
    else {
      const rowsBefore = (await tabA.state()).terminalRows;
      await tabA.tap(fitTile);
      const resized = await tabA.waitUntil((state) => state.terminalRows !== rowsBefore, 8_000);
      const afterFit = await lastAttachment();
      evidence.qf5.fit = { rowsBefore, rowsAfter: (resized.state ?? resized.last).terminalRows, resizes: afterFit.resizes, hintVisible: (resized.state ?? resized.last).fitHintVisible };
      if (!resized.state || (afterFit.resizes || 0) !== resizeBaseline + 1) fail("QF5", "the ↕ tile did not produce exactly one RESIZE_REQUEST that applied", evidence.qf5.fit);
      if ((resized.state ?? resized.last).fitHintVisible) fail("D11", "the fit hint survived the first explicit fit", evidence.qf5.fit);
    }

    // --- QF5 RED pin ---------------------------------------
    // A Keys action that emits a same-geometry RESIZE_REQUEST must move the
    // wire counter the assertions above read. The mutant is installed in the
    // page for one tap: the page's outgoing frames are watched for the live
    // socket and the attachment's source/epoch, and a capture-phase pointerup
    // on the Escape tile then sends a RESIZE_REQUEST for the current geometry.
    // The detector must advance by exactly one under the mutant and not at all
    // once it is removed — otherwise the no-resize assertions are a false GREEN.
    const mutantBefore = (await lastAttachment()).resizes || 0;
    await tabA.evaluate(`(() => {
      const originalSend = WebSocket.prototype.send;
      let socket = null, source = null, epoch = null;
      WebSocket.prototype.send = function (data) {
        try { const f = JSON.parse(String(data)); if (f && f.source && f.epoch) { socket = this; source = f.source; epoch = f.epoch; } } catch {}
        return originalSend.call(this, data);
      };
      const tile = document.querySelector('.persea-terminal-keys');
      const inject = (event) => {
        if (!event.target.closest('[data-key-control="key:escape:0"]')) return;
        if (!socket || !source) return;
        const rows = document.querySelector(".xterm-rows")?.children.length ?? 24;
        originalSend.call(socket, JSON.stringify({ type: "RESIZE_REQUEST", version: 1, source, epoch, columns: 80, rows }));
      };
      tile.addEventListener("pointerup", inject, true);
      window.__terminalResizeMutant = { off() { tile.removeEventListener("pointerup", inject, true); WebSocket.prototype.send = originalSend; } };
    })()`);
    await tabA.tapTile("Tab"); // its INPUT frame shows the mutant the live socket and identity
    const mutantArmed = (await lastAttachment()).resizes || 0;
    await tabA.tapTile("Escape"); // the mutant injects here
    await delay(250);
    const mutantAfter = (await lastAttachment()).resizes || 0;
    await tabA.evaluate("window.__terminalResizeMutant.off()");
    await tabA.tapTile("Escape");
    await delay(250);
    const mutantOff = (await lastAttachment()).resizes || 0;
    evidence.qf5.mutant = { before: mutantBefore, armed: mutantArmed, after: mutantAfter, off: mutantOff };
    if (mutantArmed !== mutantBefore) fail("QF5-PIN", "arming the mutant (a Tab tap) itself moved the wire counter", evidence.qf5.mutant);
    if (mutantAfter !== mutantBefore + 1) fail("QF5-PIN", "the wire counter did not observe the injected Keys RESIZE_REQUEST — the no-resize assertions would be a false GREEN", evidence.qf5.mutant);
    if (mutantOff !== mutantAfter) fail("QF5-PIN", "the Keys tile still emits after the mutant was removed", evidence.qf5.mutant);
    await tabA.tap((await tabA.state()).openerRect); // always the live rect
    await delay(120);

    // --- F2: dedicated Keys still enforces the input floor -----------------
    // Keys remains browsable while input is unavailable. Its dispatched action
    // reports the refusal; retired menu-tile disabled styling is no longer an owner.
    evidence.f2 = {};
    const refusedKey = async (name, reason) => {
      const before = await lastAttachment(); await tabA.tapTile("Escape"); await delay(80);
      const after = await lastAttachment();
      const status = await tabA.evaluate(`document.querySelector('.persea-terminal-keys__status')?.textContent || ''`);
      const record = { before: before.inputs, after: after.inputs, status }; evidence.f2[name] = record;
      if (JSON.stringify(after.inputs) !== JSON.stringify(before.inputs)) fail("F2", `${name}: unavailable key emitted input`, record);
      if (!status.includes(reason)) fail("F2", `${name}: Keys omitted the input floor's refusal`, record);
    };
    await tabA.cdp.send("Emulation.setDeviceMetricsOverride", { ...PHONE, height: 700 }); await delay(200);
    await control({ holdResizeMs: 1800 }); const sealFit = await tabA.tilePoint("Fit height");
    assert(!sealFit.disabled, "F2: Fit-seal probe has no available Fit action");
    await tabA.tap(sealFit); await delay(100); await refusedKey("fitSeal", "Terminal typing is disabled locally.");
    await delay(1900); await control({ holdResizeMs: 0 });
    await tabA.cdp.send("Emulation.setDeviceMetricsOverride", PHONE); await delay(200);
    await control({ holdPrepareMs: 1800, closeLive: "websocket_read" });
    assert((await tabA.waitUntil(state => state.connection !== "", 4000)).state, "F2: reconnect outage did not become observable");
    const outagePanel = await tabA.evaluate(`({ hidden:document.querySelector('.persea-terminal-keys')?.hidden, rows:document.querySelector('.persea-terminal-keys')?.children.length })`);
    evidence.f2.reconnect = outagePanel;
    if (outagePanel.hidden === false) fail("F2", "transport teardown left the dedicated Keys disclosure open", outagePanel);
    assert((await tabA.waitUntil(isLive, 8000)).state, "F2: reconnect did not settle");
    if ((await lastAttachment()).inputs.length !== 0) fail("F2", "Keys replayed input across the transport teardown", (await lastAttachment()).inputs);
    const regrantedBefore = await lastAttachment(); await tabA.tapTile("Escape"); const regrantedAfter = await lastAttachment();
    if (regrantedAfter.inputs.slice(regrantedBefore.inputs.length).join("") !== "\x1b") fail("F2", "Keys did not resume after the new control grant", regrantedAfter.inputs);

    // --- QF6 the one-shot keyboard restore -------------------------
    // Focus is not keyboard state. The restore is armed only when BOTH held at
    // the close: the classifier said OPEN (the visual viewport shrank by a
    // keyboard's height) AND a text entry of the page had focus. Four cells ×
    // two reopen kinds — an ordinary reconnect (the socket drops, the source
    // binding survives) and a forced reopen (the binding expired and the
    // session fell back to adoptable: identity re-mint, adopt in place,
    // attach). Only focused+OPEN yields exactly one focus() after COMMIT; no
    // INPUT ever precedes MODE_REQUEST. focus() calls on xterm's textarea
    // are counted in the page, so a textarea that simply kept its focus
    // across the outage cannot pass for a restore.
    await tabA.evaluate(`(() => {
      if (typeof window.__terminalFocusCalls === "number") return;
      window.__terminalFocusCalls = 0;
      const original = HTMLTextAreaElement.prototype.focus;
      HTMLTextAreaElement.prototype.focus = function (...args) {
        if (this.classList.contains("xterm-helper-textarea")) window.__terminalFocusCalls += 1;
        return original.apply(this, args);
      };
    })()`);
    evidence.qf6 = {};
    const focusCalls = async () => (await tabA.state()).focusCalls;
    const setCell = async (focused, open) => {
      await tabA.cdp.send("Emulation.setDeviceMetricsOverride", open ? PHONE_KEYBOARD : PHONE);
      if (focused) await tabA.evaluate("document.querySelector('.xterm-helper-textarea')?.focus()");
      else await tabA.evaluate("document.activeElement && document.activeElement.blur()");
      // The key bar is the classifier's visible output: shown iff OPEN.
      const settled = await tabA.waitUntil((state) => state.keyboardInset === open && (state.activeKind === "xterm") === focused, 4_000);
      return settled.state ?? settled.last;
    };
    for (const kind of ["reconnect", "forced"]) {
      for (const [cellName, focused, open] of [["unfocused+CLOSED", false, false], ["focused+CLOSED", true, false], ["unfocused+OPEN", false, true], ["focused+OPEN", true, true]]) {
        const key = `${kind}.${cellName}`;
        const armed = await setCell(focused, open);
        const cellOK = armed.keyboardInset === open && (armed.activeKind === "xterm") === focused;
        const before = await snapshot();
        const callsBefore = await focusCalls();
        if (kind === "forced") await control({ sessionState: "adoptable", adopted: false, bindingsExpired: true, closeLive: "websocket_read" });
        else await control({ closeLive: "websocket_read" });
        const closing = await tabA.waitUntil((state) => state.connection !== "", 4_000);
        const back = await tabA.waitUntil((state) => isLive(state) && state.connection === "", 10_000);
        await delay(250);
        const after = await tabA.state();
        const callsAfter = await focusCalls();
        const attachment = await lastAttachment();
        const snap = await snapshot();
        const modeIndex = attachment.frames.indexOf("MODE_REQUEST");
        const inputIndex = firstInputIndex(attachment.frames);
        const record = {
          cellArmed: cellOK, armedKind: armed.activeKind, armedKeyBarHidden: armed.keyBarHidden,
          closingSeen: !!closing.state, live: !!back.state, focusCalls: callsAfter - callsBefore,
          activeKind: after.activeKind, keyBarHidden: after.keyBarHidden, hintVisible: after.fitHintVisible,
          frames: attachment.frames, adoptions: snap.counters.adoptions - before.counters.adoptions,
          attachments: snap.attachments.length - before.attachments.length,
        };
        evidence.qf6[key] = record;
        const expected = focused && open ? 1 : 0;
        if (!cellOK) fail("QF6", `${key}: the cell could not be armed (probe vacuous)`, record);
        if (!closing.state) fail("QF6", `${key}: the close was not observed (no reconnecting strip)`, record);
        if (!back.state) fail("QF6", `${key}: the reopen did not reach live control`, record);
        if (record.attachments !== 1) fail("QF6", `${key}: the reopen did not produce exactly one new attachment`, record);
        if (kind === "forced" && record.adoptions !== 1) fail("QF6", `${key}: the forced reopen did not adopt in place`, record);
        if (record.focusCalls !== expected) fail("QF6", `${key}: ${record.focusCalls} focus() after COMMIT, expected ${expected}`, record);
        if (modeIndex < 0 || (inputIndex >= 0 && inputIndex < modeIndex)) fail("QF6", `${key}: INPUT preceded MODE_REQUEST`, record);
        if (after.fitHintVisible) fail("D11", `${key}: the one-time hint came back on a reopen`, record);
        if (kind === "forced") await control({ sessionState: "open", bindingsExpired: false });
      }
    }
    await tabA.cdp.send("Emulation.setDeviceMetricsOverride", PHONE);
    await tabA.evaluate("document.activeElement && document.activeElement.blur()");
    await delay(300);

    // --- Keyboard open: the eight-key bar ----------------------------------------
    await tabA.evaluate("document.querySelector('.xterm-helper-textarea')?.focus()");
    await tabA.cdp.send("Emulation.setDeviceMetricsOverride", PHONE_KEYBOARD);
    const barShown = await tabA.waitUntil((state) => !state.keyBarHidden, 4_000);
    const bar = barShown.state ?? barShown.last;
    evidence.keyBar = { hidden: bar.keyBarHidden, keys: bar.keyBarKeys.map((k) => ({ label: k.label, w: k.rect.w, h: k.rect.h })), openerRect: bar.openerRect };
    if (bar.keyBarHidden) fail("KEYBAR", "the key bar did not show with the keyboard open", evidence.keyBar);
    else {
      const labels = bar.keyBarKeys.map((k) => k.label).join(" ");
      if (labels !== "Esc ⇥ Ctrl ← ↓ ↑ → Keys") fail("KEYBAR", "the bar is not exactly Esc ⇥ Ctrl ← ↓ ↑ → Keys", { labels });
      const small = bar.keyBarKeys.filter((k) => k.rect.w < 44 || k.rect.h < 44);
      if (small.length) fail("KEYBAR", "a bar key is under 44×44 at 390pt", small);
      // The opener sits in the control row, above the bar.
      const barTop = Math.min(...bar.keyBarKeys.map((k) => k.rect.y));
      if (bar.openerRect.y + bar.openerRect.h > barTop + 0.5) fail("KEYBAR", "the sheet opener is not above the key bar", { opener: bar.openerRect, barTop });
    }
    await shot(tabA, "phone-03-terminal-keyboard-open-8-key-bar");
    const keyboardCensus = under44(await tabA.census());
    evidence.censusKeyboardOpen = keyboardCensus;
    if (keyboardCensus.length) fail("T44", "under-44px targets with the keyboard open", keyboardCensus);
    await tabA.cdp.send("Emulation.setDeviceMetricsOverride", PHONE);
    await tabA.evaluate("document.activeElement && document.activeElement.blur()");
    await delay(300);

    // --- D1 composer status line + composer targets ---------------------------
    const closedState = await tabA.state();
    await tabA.tap(closedState.composerToggleRect);
    const composerOpen = await tabA.waitUntil((state) => state.composerOpen, 4_000);
    const idle = composerOpen.state ?? composerOpen.last;
    // the idle composer shows NO status line; a standing
    // condition (smart punctuation in the draft) reveals it on its own
    // full-width line above the button row.
    if (!composerOpen.state) fail("D1", "the composer did not open from the toolbar ✎", { status: idle.composerStatus });
    else if (idle.composerStatus !== null) fail("D1", "the idle composer shows a status line", idle.composerStatus);
    await tabA.evaluate(`(() => { const t = document.querySelector(".attachment-page__composer-textarea"); if (t) { t.value = "\\u201cquoted\\u201d"; t.dispatchEvent(new Event("input", { bubbles: true })); } })()`);
    const composerCondition = await tabA.waitUntil((state) => state.composerOpen && state.composerStatus !== null, 4_000);
    const c = composerCondition.state ?? composerCondition.last;
    evidence.d1 = { idleStatus: idle.composerStatus, status: c.composerStatus, actions: c.composerActionsRect, buttons: c.composerButtons.map((b) => ({ label: b.label, w: b.rect.w, h: b.rect.h, y: b.rect.y })), grip: c.gripRect };
    if (!composerCondition.state) fail("D1", "the status line did not appear for smart punctuation", evidence.d1);
    else {
      const status = c.composerStatus;
      if (status.rect.w < 200) fail("D1", "the status line is narrower than 200px", evidence.d1);
      if (status.scrollWidth > status.clientWidth + 1) fail("D1", "the status line is clipped", evidence.d1);
      if (c.composerActionsRect && status.rect.y + status.rect.h > c.composerActionsRect.y + 0.5) fail("D1", "the status shares a row with the button row", evidence.d1);
      for (const button of c.composerButtons) if (status.rect.y + status.rect.h > button.rect.y + 0.5) fail("D1", `the status shares a row with ${button.label}`, evidence.d1);
      const smallButtons = c.composerButtons.filter((b) => b.rect.w < 44 || b.rect.h < 44);
      if (smallButtons.length) fail("T44", "composer buttons under 44×44", smallButtons);
      if (!c.gripRect || c.gripRect.h < 24) fail("T44", "the composer grip is under 24px tall", c.gripRect);
    }
    await shot(tabA, "phone-04-terminal-composer-status-line");
    const composerCensus = under44(await tabA.census());
    evidence.censusComposerOpen = composerCensus;
    if (composerCensus.length) fail("T44", "under-44px targets with the composer open", composerCensus);
    await tabA.evaluate(`(() => { const t = document.querySelector(".attachment-page__composer-textarea"); if (t) { t.value = ""; t.dispatchEvent(new Event("input", { bubbles: true })); } })()`);
    await tabA.tap(closedState.composerToggleRect);
    await delay(200);

    // --- T44 census: terminal page closed keyboard, then the sheet ------------
    const closedCensus = under44(await tabA.census());
    evidence.censusClosed = closedCensus;
    if (closedCensus.length) fail("T44", "under-44px targets on the phone terminal page", closedCensus);
    await tabA.tap((await tabA.state()).openerRect);
    await delay(150);
    const sheetCensus = under44(await tabA.census());
    evidence.censusSheet = sheetCensus;
    if (sheetCensus.length) fail("T44", "under-44px targets with the sheet open", sheetCensus);
    const sheetTop = await tabA.state();
    evidence.censusSheetOpener = { top: tilesUnderOpener(sheetTop), opener: sheetTop.openerRect, sheet: sheetTop.sheetRect };
    const clippedLabels = Object.entries(sheetTop.tiles).filter(([, t]) => t.labelClipped).map(([label]) => label);
    evidence.censusSheetLabels = clippedLabels;
    if (clippedLabels.length) fail("T44", "tile primary labels ellipsize (scrollWidth > clientWidth)", clippedLabels);
    if (evidence.censusSheetOpener.top.length) fail("T44", "the opener covers sheet tiles (sheet at top)", evidence.censusSheetOpener);
    // Scrolled to its end: the Image and Keyboard sections (Sessions and
    // Snippets render nothing until their phase); the opener still covers nothing.
    await tabA.evaluate("(() => { const s = document.querySelector('.persea-unified-sheet'); s.scrollTop = s.scrollHeight; return s.scrollTop; })()");
    await delay(120);
    const sheetBottom = await tabA.state();
    evidence.censusSheetOpener.bottom = tilesUnderOpener(sheetBottom);
    if (evidence.censusSheetOpener.bottom.length) fail("T44", "the opener covers sheet tiles (sheet at bottom)", evidence.censusSheetOpener);
    await shot(tabA, "phone-02b-terminal-sheet-scrolled-bottom");
    await tabA.tap((await tabA.state()).openerRect);
    await delay(150);

    // --- Image: the ⊕ tile and the composer's ⊕ when staging is available ------
    // The fixture advertises no image staging by default (the tile then says
    // why it is unavailable — the state the other screenshots show). With
    // staging advertised the dashboard links image_realm; the tile is live
    // and opens the composer, whose ⊕ then renders at 44px.
    await control({ reset: true, imageStaging: true });
    await tabA.cdp.send("Page.setInterceptFileChooserDialog", { enabled: true });
    await tabA.navigate(unifiedURL(await freshControlHandle(), { image_realm: "local" }));
    const imageLive = await tabA.waitUntil(isLive);
    assert(imageLive.state, `image precondition: the page did not reach live control: ${JSON.stringify(imageLive.last)}`);
    await tabA.tap((await tabA.state()).openerRect);
    await delay(150);
    const imageSheet = await tabA.state();
    const imageTile = imageSheet.tiles[IMAGE_TILE];
    evidence.image = { tile: imageTile ? { disabled: imageTile.disabled, reason: imageTile.reason, w: imageTile.rect.w, h: imageTile.rect.h } : null };
    if (!imageTile || imageTile.disabled || imageTile.reason !== "") fail("IMAGE", "the ⊕ tile is not live with image staging advertised", evidence.image);
    else {
      await tabA.tapTile(IMAGE_TILE);
      const withComposer = await tabA.waitUntil((state) => state.composerOpen && !state.sheetVisible, 4_000);
      const ic = withComposer.state ?? withComposer.last;
      const attach = ic.composerButtons.find((b) => b.label === "Attach images");
      evidence.image.composer = { open: ic.composerOpen, sheetVisible: ic.sheetVisible, attach: attach ? { w: attach.rect.w, h: attach.rect.h } : null, activeKind: ic.activeKind };
      if (!withComposer.state) fail("IMAGE", "the ⊕ tile did not open the composer and close the sheet", evidence.image.composer);
      if (!attach) fail("IMAGE", "the composer renders no ⊕ (Attach images) with staging available", evidence.image.composer);
      else if (attach.w < 44 || attach.h < 44) fail("T44", "the composer ⊕ is under 44×44", evidence.image.composer);
      await shot(tabA, "phone-04b-terminal-composer-with-image-button");
      const imageCensus = under44(await tabA.census());
      evidence.censusComposerImage = imageCensus;
      if (imageCensus.length) fail("T44", "under-44px targets with the composer open (image staging)", imageCensus);
    }
    await tabA.cdp.send("Page.setInterceptFileChooserDialog", { enabled: false });

    // --- F2 (c): no input before a held first PREPARE/MODE grant ----------
    await control({ reset: true, holdPrepareMs: 2500 });
    await tabA.navigate(unifiedURL(await freshControlHandle()));
    const openerEarly = await tabA.waitUntil(state => state.openerVisible, 3000);
    assert(openerEarly.state, "F2: opener did not render before PREPARE");
    await refusedKey("firstOpen", "No terminal is attached.");
    const beforeFirstGrant = await lastAttachment();
    if (beforeFirstGrant.inputs.length || firstInputIndex(beforeFirstGrant.frames) !== -1) fail("F2", "input reached the wire before first control", beforeFirstGrant.frames);
    assert((await tabA.waitUntil(isLive, 8000)).state, "F2: first control grant did not settle");
    const firstOpenAttachment = await lastAttachment();
    if (firstOpenAttachment.frames[0] !== "READY" || firstOpenAttachment.frames[1] !== "MODE_REQUEST") fail("F2", "first open omitted READY then MODE_REQUEST", firstOpenAttachment.frames);
    await tabA.tapTile("Escape");
    if ((await lastAttachment()).inputs.join("") !== "\x1b") fail("F2", "first granted key did not emit exactly once", (await lastAttachment()).inputs);

    // --- D11 reopen: the compact phone row never resurrects the prose hint ----
    await control({ reset: true });
    await tabA.navigate(unifiedURL(await freshControlHandle()));
    const hinted = await tabA.waitUntil((state) => isLive(state), 8_000);
    if (!hinted.state) fail("D11", "a fresh compact page did not become live", hinted.last);
    else if (hinted.state.fitHintVisible) fail("D11", "a fresh compact page resurrected the retired prose hint", hinted.state);

    // --- QF1c + D10 lease_held auto-takeover on a coarse newcomer -------------
    // Tab A holds control (fine pointer). Tab B, a phone, reopens the same URL:
    // lease_held → automatic takeover → live. No keyboard on B; B says once
    // that it took control from another window.
    await tabA.emulate(DESKTOP, false);
    await control({ reset: true });
    const heldURL = unifiedURL(await freshControlHandle());
    await tabA.navigate(heldURL);
    assert((await tabA.waitUntil(isLive)).state, "QF1c precondition: tab A did not reach live control");
    const tabB = await Tab.open("B", chrome.debugPort, origin);
    tabs.push(tabB);
    await tabB.emulate(PHONE, true);
    const beforeB = await snapshot();
    await tabB.navigate(heldURL);
    const liveB = await tabB.waitUntil((state) => isLive(state) || hasNotice(state));
    const toastB = await tabB.waitUntil((state) => state.toast === TOAST_TEXT, 2_500);
    const afterB = await snapshot();
    const bState = toastB.state ?? toastB.last;
    evidence.qf1c = { activeKind: bState.activeKind, keyBarHidden: bState.keyBarHidden, toast: bState.toast, takeovers: afterB.counters.takeovers - beforeB.counters.takeovers, code: bState.code, live: liveB.state ? isLive(liveB.state) : false };
    if (!liveB.state || !isLive(liveB.state)) fail("QF1c", "the coarse newcomer did not take control automatically", evidence.qf1c);
    else {
      if (bState.activeKind === "xterm") fail("QF1c", "the auto-takeover COMMIT raised the keyboard on the newcomer", evidence.qf1c);
      if (bState.keyboardInset) fail("QF1c", "the key bar showed on the newcomer", evidence.qf1c);
      if (!toastB.state) fail("D10", "the newcomer did not show the displacement toast", evidence.qf1c);
      else {
        const gone = await tabB.waitUntil((state) => state.toast === "", 4_500);
        if (!gone.state) fail("D10", "the toast did not leave by itself", gone.last);
      }
      const bAttachment = await lastAttachment();
      const bMode = bAttachment.frames.indexOf("MODE_REQUEST");
      const bInput = firstInputIndex(bAttachment.frames);
      if (bMode < 0 || (bInput >= 0 && bInput < bMode)) fail("QF1c", "the takeover attachment sent INPUT before its control grant", bAttachment.frames);
      const displaced = await tabA.waitUntil((state) => hasNotice(state) && state.code.includes("control_displaced"), 4_000);
      if (!displaced.state) fail("QF1c", "the incumbent was not shown control_displaced", displaced.last);
      // The incumbent never sees the newcomer's toast.
      if ((await tabA.state()).toast !== "") fail("D10", "the displaced incumbent showed the takeover toast", await tabA.state());
    }
    // --- F2 (d) lease_held takeover: disabled until the control grant --------
    // A fresh phone tab C reopens against B's lease with the grant held: the
    // takeover attaches and COMMITs, but the Keys tiles stay disabled with the
    // composer's reason until MODE(CONTROL), and a tap delivers nothing. (A
    // new tab, not tab A: a background tab's renderer is throttled and xterm
    // paints its rows on animation frames, so the screen probe needs the
    // foreground tab.)
    await control({ holdModeGrant: true });
    const tabC = await Tab.open("C", chrome.debugPort, origin);
    tabs.push(tabC);
    await tabC.emulate(PHONE, true);
    const beforeTakeover = await snapshot();
    await tabC.navigate(heldURL);
    const committedHeld = await tabC.waitUntil((state) => isLive(state) || hasNotice(state), 8_000);
    const heldSnapshot = await snapshot();
    evidence.f2.leaseHeld = { live: committedHeld.state ? isLive(committedHeld.state) : false, takeovers: heldSnapshot.counters.takeovers - beforeTakeover.counters.takeovers };
    if (!committedHeld.state || !isLive(committedHeld.state) || evidence.f2.leaseHeld.takeovers !== 1) fail("F2", "precondition: the coarse takeover did not reach COMMIT with the grant held", { state: committedHeld.state ?? committedHeld.last, ...evidence.f2.leaseHeld });
    else {
      await tabC.tap((await tabC.state()).openerRect);
      await delay(120);
      const ungrantedAttachment = await lastAttachment();
      evidence.f2.leaseHeld.frames = ungrantedAttachment.frames;
      await tabC.tapTile("Escape"); await delay(100);
      const heldReason = await tabC.evaluate(`document.querySelector('.persea-terminal-keys__status')?.textContent || ''`);
      if (!heldReason.includes("No terminal is attached.")) fail("F2", "takeover Keys omitted the control-grant refusal", heldReason);
      if ((await lastAttachment()).inputs.length !== ungrantedAttachment.inputs.length) fail("F2", "held takeover key emitted input", (await lastAttachment()).inputs);
      await control({ releaseMode: true });
      await delay(200);
      const grantedAttachment = await lastAttachment();
      evidence.f2.leaseHeld.after = { frames: grantedAttachment.frames, inputs: grantedAttachment.inputs };
      const modeAt = grantedAttachment.frames.indexOf("MODE_REQUEST");
      const inputAt = firstInputIndex(grantedAttachment.frames);
      if (grantedAttachment.inputs.length !== 0 || inputAt !== -1) fail("F2", "input was delivered before the takeover's control grant", evidence.f2.leaseHeld.after);
      if (modeAt < 0) fail("F2", "the takeover attachment carried no MODE_REQUEST", evidence.f2.leaseHeld.after);
      await tabC.tapTile("Escape");
      if ((await lastAttachment()).inputs.join("") !== "\x1b") fail("F2", "takeover key did not emit exactly once after MODE", (await lastAttachment()).inputs);
      await tabC.tap((await tabC.state()).openerRect);
      await delay(120);
    }
    await tabC.close(chrome.debugPort);
    tabs.pop();
    await tabB.close(chrome.debugPort);
    tabs.pop();


    // ======================================================================
    // clipboard — shared Clipboard, explicit edit/Paste, local Copy and OSC 52
    // ======================================================================
    // The case IDs retain their original runtime responsibilities. Retired
    // last-N controls are exercised through exact terminal-selection Copy.
    // Opening text edits it; only the row's explicit Paste action emits input.
    {
    const clipboard = { f1: {}, f2: {}, f3: {}, f4: {}, f5: {}, f6: {}, f7: {}, f9: {}, f10: {}, r6: {}, r7: {} };
    evidence.clipboard = clipboard;
    const store = async () => (await snapshot()).snippets;
    const installFocusCounter = () => tabA.evaluate(`(() => {
      if (typeof window.__terminalFocusCalls === "number") return;
      window.__terminalFocusCalls = 0;
      const original = HTMLTextAreaElement.prototype.focus;
      HTMLTextAreaElement.prototype.focus = function (...args) {
        if (this.classList.contains("xterm-helper-textarea")) window.__terminalFocusCalls += 1;
        return original.apply(this, args);
      };
    })()`);
    const clipboardState = () => tabA.evaluate(`(() => {
      const panel = document.querySelector('.persea-clipboard[open]');
      if (!panel) return { visible: false, rows: [], status: '' };
      const box = node => { const r = node.getBoundingClientRect(); return { x:r.x, y:r.y, w:r.width, h:r.height }; };
      return { visible: true, rect: box(panel),
        handoff: !!panel.querySelector('.persea-clipboard__handoff:not([hidden])'),
        handoffText: panel.querySelector('.persea-clipboard__handoff:not([hidden])')?.textContent || '',
        status: panel.querySelector('.persea-clipboard__status')?.textContent || '', text: panel.textContent,
        draft: panel.querySelector('textarea')?.value ?? null,
        rows: [...panel.querySelectorAll('[data-clipboard-item]')].map(row => ({ id:row.dataset.clipboardItem, label:row.querySelector('.persea-clipboard__preview')?.textContent || '', meta:row.querySelector('.persea-clipboard__meta')?.textContent || '', expiry:row.querySelector('.persea-clipboard__expiry > span')?.textContent || '', rect:box(row), elements:row.querySelector('.persea-clipboard__preview')?.querySelectorAll('img,script,iframe,svg,object,embed').length || 0 })),
        actions: [...panel.querySelectorAll('button')].filter(node=>node.getClientRects().length).map(node=>({label:node.getAttribute('aria-label'),disabled:node.disabled,unavailable:node.dataset.clipboardUnavailable==='true',title:node.title,rect:box(node)})) };
    })()`);
    const waitClipboard = async (predicate, budget = 10_000) => {
      const deadline = Date.now() + budget; let current;
      do { current = await clipboardState(); if (predicate(current)) return current; await delay(100); } while (Date.now() < deadline);
      throw new Error(`Clipboard did not reach the required state: ${JSON.stringify(current)}`);
    };
    const clipboardPoint = label => tabA.pointFor(`Array.from(document.querySelectorAll('.persea-clipboard[open] button')).find(node => node.getAttribute('aria-label') === ${JSON.stringify(label)})`);
    const clipboardAction = async label => {
      const point = await clipboardPoint(label); assert(point, `Clipboard action ${label} is absent`);
      const before = await wire();
      await tabA.activate(await tabA.state(), point); await delay(100);
      if(label!=='Paste text to terminal')await noWireChange(before,`Clipboard ${label}`);
      else {const after=await wire();if(JSON.stringify(after.map(a=>a.resizes))!==JSON.stringify(before.map(a=>a.resizes)))fail('clipboard-send-preserves-geometry','Clipboard Send changed geometry',{before,after});}
    };
    const closeClipboard = async () => { if ((await clipboardState()).visible) await clipboardAction("Close clipboard"); };
    const openSheet = async () => {
      await closeClipboard(); const state = await tabA.state();
      if (!state.sheetVisible) { await tabA.tap(state.openerRect); await delay(120); }
    };
    const openList = async () => {
      let state = await clipboardState();
      if (!state.visible) { await tabA.tapTile("Clipboard"); await waitClipboard(value => value.visible); }
      else if (state.draft !== null) await clipboardAction("Cancel");
      return waitClipboard(value => value.visible && value.draft === null);
    };
    const freshClipboardPage = async (config = {}, coarse = true) => {
      await control({ reset: true, ...config }); await tabA.emulate(coarse ? PHONE : DESKTOP, coarse);
      await tabA.navigate(unifiedURL(await freshControlHandle()));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "clipboard: fresh attachment did not commit");
      await installFocusCounter();
    };
    const wire = async () => { const value = await snapshot(); return value.attachments.map(a=>({inputs:a.inputs.join(''),resizes:a.resizes || 0})); };
    const noWireChange = async (before, label) => {
      const after = await wire(); if (JSON.stringify(after) !== JSON.stringify(before)) fail("clipboard-send-preserves-geometry", `${label} changed terminal input or geometry`, {before,after});
    };
    const exactRow = async label => {
      await openList();
      const list = await waitClipboard(value => value.rows.some(row => row.label === label || row.id === label));
      const matches = list.rows.filter(row => row.label === label || row.id === label);
      assert(matches.length === 1, `Clipboard row must be exact and unique: ${label}`);
      return matches[0];
    };
    const rowPoint = async (label, selector) => {
      const row = await exactRow(label);
      return tabA.pointFor(`Array.from(document.querySelectorAll('.persea-clipboard [data-clipboard-item]')).find(node => node.dataset.clipboardItem === ${JSON.stringify(row.id)})?.querySelector(${JSON.stringify(selector)})`);
    };
    const rowAction = async (label, action) => {
      const point = await rowPoint(label, `button[aria-label="${action}"]`);
      assert(point, `Clipboard row action ${action} is absent for ${label}`);
      const before = await wire(); await tabA.activate(await tabA.state(), point); await delay(100);
      if (action !== 'Paste text to terminal') await noWireChange(before, `Clipboard row ${action}`);
      else { const after=await wire(); if(JSON.stringify(after.map(a=>a.resizes))!==JSON.stringify(before.map(a=>a.resizes)))fail('clipboard-send-preserves-geometry','Clipboard Send changed geometry',{before,after}); }
    };
    const preview = async label => {
      const point = await rowPoint(label, '.persea-clipboard__open');
      const before = await wire(); await tabA.activate(await tabA.state(), point);
      const shown = await waitClipboard(value => value.draft !== null);
      await noWireChange(before, "opening a text preview"); return shown;
    };
    const send = async label => {
      await exactRow(label); const before = await lastAttachment(), beforeState = await tabA.state();
      await rowAction(label, "Paste text to terminal"); await delay(200);
      const after = await lastAttachment();
      if (after.resizes !== before.resizes) fail("clipboard-send-preserves-geometry", "explicit text Send resized the terminal", {before:before.resizes,after:after.resizes});
      return {delta:after.inputs.slice(before.inputs.length).join(''),beforeState,state:await tabA.state()};
    };
    const installClipboardStub = () => tabA.evaluate(`(() => {
      window.__clipboardDevice = {writes:[],reads:0,fail:false,readFail:false,value:'device text'};
      // SnippetService captures this object at boot; retain its identity so
      // automatic OSC writes and gesture writes share the same refusal mock.
      Object.defineProperties(navigator.clipboard,{
        read:{configurable:true,value:undefined},
        writeText:{configurable:true,value(text){ const state=window.__clipboardDevice; if(state.fail)return Promise.reject(new Error('fixture clipboard refusal')); state.writes.push(String(text));return Promise.resolve();}},
        readText:{configurable:true,value(){ const state=window.__clipboardDevice;state.reads++;return state.readFail?Promise.reject(new Error('fixture read refusal')):Promise.resolve(state.value);}}
      });
    })()`);
    const selectTerminal = async () => {
      await closeClipboard();
      const selecting = await tabA.evaluate(`!document.querySelector('.persea-unified-select').hidden`);
      if (!selecting) await tabA.activate(await tabA.state(), await tabA.pointFor(`document.querySelector('.persea-unified-select-context')`));
      await delay(100);
      const text = await tabA.evaluate(`(() => { const rows=[...document.querySelectorAll('.persea-unified-select__row')].filter(row=>row.textContent.trim());if(!rows.length)throw new Error('no terminal text to copy');const range=document.createRange();range.setStart(rows[0],0);range.setEnd(rows[rows.length-1],rows[rows.length-1].childNodes.length);const selection=getSelection();selection.removeAllRanges();selection.addRange(range);return rows.map(row=>row.textContent.trimEnd()).join('\\n');})()`);
      assert(text.trim(), "clipboard: terminal selection is empty");
      // A prior successful copy owns its 1.2s receipt dwell. Wait for that
      // existing transaction to retire before acquiring the next Copy action.
      for(let attempt=0;attempt<100;attempt++){if(await tabA.evaluate(`document.querySelector('.persea-unified-toolbar-paste')?.dataset.pasteState==='copy'`))break;await delay(20);}
      return text;
    };
    const copyTerminal = async () => {
      const text = await selectTerminal(); const before = await wire();
      const point = await tabA.pointFor(`document.querySelector('.persea-unified-toolbar-paste[data-paste-state="copy"]')`);
      assert(point, `clipboard: exact terminal selection has no Copy action: ${JSON.stringify(await tabA.evaluate(`({state:document.querySelector('.persea-unified-toolbar-paste')?.dataset.pasteState,selected:String(getSelection()),anchor:getSelection()?.anchorNode?.parentElement?.className,focus:getSelection()?.focusNode?.parentElement?.className,overlayHidden:document.querySelector('.persea-unified-select')?.hidden,active:document.activeElement?.className})`))}`); await tabA.activate(await tabA.state(),point); await delay(180);
      await noWireChange(before,"terminal selection Copy"); return text.replace(/\r\n?/g,'\n');
    };
    const addText = async body => {
      await openList(); await clipboardAction("Add text");
      await tabA.evaluate(`(() => {const input=document.querySelector('.persea-clipboard textarea');input.value=${JSON.stringify(body)};input.dispatchEvent(new Event('input',{bubbles:true}));})()`);
      await clipboardAction("Save"); await delay(180);
    };

    // F3: opening the task menu is presentation; the shared service starts
    // only when Clipboard is explicitly opened. The pane geometry is untouched.
    await freshClipboardPage(); const initialWire = await wire();
    await openSheet(); const beforeList = await store();
    if (beforeList.get !== 0) fail("clipboard-send-preserves-geometry","task menu eagerly polled Clipboard",beforeList);
    await openList(); await waitClipboard(value=>/clipboard is empty/i.test(value.text));
    await noWireChange(initialWire,"opening the unified Clipboard list");
    clipboard.f3.access = {beforeReads:beforeList.get,afterReads:(await store()).get};

    // F2/F9/F10: editing cannot emit; Paste normalizes once, never appends
    // Return, and multiline/tabbed text still uses the guarded composer path.
    await control({snippetWrite:[
      {kind:'snippet',label:'zero',body:'echo zero'}, {kind:'snippet',label:'one',body:'echo one\n'},
      {kind:'snippet',label:'many',body:'echo many\n\n\n'}, {kind:'snippet',label:'multi',body:'line one\nline two'},
      {kind:'snippet',label:'tabbed',body:'col\tvalue'}, {kind:'snippet',label:'markup',body:'<img src=x onerror=alert(1)> & <script>'},
    ]});
    await waitClipboard(value=>value.rows.length>=6);
    for(const [label,expected] of [['zero','echo zero'],['one','echo one'],['many','echo many']]) {
      const body = {zero:'echo zero',one:'echo one\n',many:'echo many\n\n\n'}[label];
      const result=await send(body); clipboard.f2[label]={expected,got:result.delta};
      if(result.delta!==expected || /[\r\n]$/.test(result.delta))fail('clipboard-send-no-return',`${label} Send changed the no-Return contract`,clipboard.f2[label]);
      if(result.state.activeKind==='xterm')fail('clipboard-touch-focus','coarse Send summoned native terminal input',result.state.activeKind);
    }
    for(const keyboardOpen of [false,true]) {
      if(keyboardOpen){await closeClipboard();await tabA.tapTile('Show keyboard');await tabA.emulate(PHONE_KEYBOARD,true);await delay(100);}
    for(const [label,body] of [['multi','line one\nline two'],['tabbed','col\tvalue']]) {
      const result=await send(body);clipboard.f2[label]={delta:result.delta,draft:result.state.composerDraft};
      if(result.delta!=='' || result.state.composerDraft!==body || !result.state.composerOpen)fail('clipboard-send-no-return',`${label} did not remain in this pane's guarded composer`,clipboard.f2[label]);
      if(result.state.focusCalls!==result.beforeState.focusCalls || ['xterm','composer'].includes(result.state.activeKind) || result.state.visualViewportHeight!==result.beforeState.visualViewportHeight || Boolean(result.state.keyboardInset)!==Boolean(result.beforeState.keyboardInset))fail('clipboard-focus-ownership',`${label} guarded Send changed keyboard/focus state`,{keyboardOpen,before:result.beforeState,after:result.state});
      await tabA.evaluate(`(() => {const node=document.querySelector('.attachment-page__composer-textarea');node.value='';node.dispatchEvent(new Event('input',{bubbles:true}));})()`);
    }
      if(keyboardOpen){await closeClipboard();await tabA.tapTile('Hide keyboard');await tabA.emulate(PHONE,true);await delay(100);}
    }
    await control({writeLive:'\x1b[?2004h'});await delay(100);
    const framed=await send('line one\nline two');clipboard.f2.bracketed=framed.delta;
    if(framed.delta!=='\x1b[200~line one\rline two\x1b[201~')fail('clipboard-send-no-return','bracketed Send was not framed exactly',framed.delta);
    await control({writeLive:'\x1b[?2004l'});
    const markup=await preview('<img src=x onerror=alert(1)> & <script>');
    const markupElements=await tabA.evaluate(`document.querySelector('.persea-clipboard__content').querySelectorAll('script,iframe,svg,object,embed,img').length`);
    clipboard.f10.markup={draft:markup.draft,elements:markupElements};
    if(markupElements!==0 || markup.draft!=='<img src=x onerror=alert(1)> & <script>')fail('clipboard-markup-safety','text preview interpreted markup',clipboard.f10.markup);
    const cancelBefore=await wire();await clipboardAction('Cancel');await noWireChange(cancelBefore,'preview Cancel');

    // F1: selection is always local, but Send must obey the exact target's
    // MODE grant and lifecycle. Opening a row is never an implicit input action.
    await freshClipboardPage({holdModeGrant:true,snippetWrite:[{kind:'snippet',label:'zero',body:'echo zero'}]});
    const ungrantedEditorWire=await wire(), ungrantedEditor=await preview('echo zero');
    if(ungrantedEditor.draft!=='echo zero')fail('clipboard-local-editing','ungranted text cannot be edited locally',ungrantedEditor.draft);
    await clipboardAction('Cancel');await noWireChange(ungrantedEditorWire,'ungranted editor Cancel');
    await exactRow('echo zero'); const ungranted=await clipboardState(), heldSend=ungranted.actions.find(a=>a.label==='Paste text to terminal');
    clipboard.f1.ungranted=heldSend;
    if(!heldSend?.unavailable || !/attached|control|available/i.test(heldSend.title))fail('clipboard-local-editing','Send lacks the input authority refusal before MODE',heldSend);
    const heldWire=await wire();await rowAction('echo zero','Paste text to terminal');await noWireChange(heldWire,'ungranted Send');
    await control({releaseMode:true});await delay(150);await closeClipboard();
    const granted=await send('echo zero');if(granted.delta!=='echo zero')fail('clipboard-local-editing','granted Send did not reach its pane',granted.delta);
    const grantedAttachment=await lastAttachment();clipboard.f1.granted={delta:granted.delta,frames:grantedAttachment.frames.slice(0,8)};
    if(firstInputIndex(grantedAttachment.frames)<grantedAttachment.frames.indexOf('MODE_REQUEST'))fail('clipboard-local-editing','INPUT preceded the MODE request',clipboard.f1.granted);
    await exactRow('echo zero');const staleWire=await wire();
    const heldPoint=await rowPoint('echo zero','button[aria-label="Paste text to terminal"]');
    await tabA.cdp.send('Input.dispatchMouseEvent',{type:'mousePressed',x:heldPoint.x,y:heldPoint.y,button:'left',buttons:1,clickCount:1});
    await control({closeLive:'websocket_read',holdPrepareMs:700});await delay(200);
    await tabA.cdp.send('Input.dispatchMouseEvent',{type:'mouseReleased',x:heldPoint.x,y:heldPoint.y,button:'left',buttons:0,clickCount:1});
    assert((await tabA.waitUntil(isLive,10_000)).state,'clipboard-local-editing: reconnect did not settle');
    const reconnectedWire=await wire();clipboard.f1.stale={before:staleWire,after:reconnectedWire};
    if(reconnectedWire.some((a,i)=>a.inputs!==(staleWire[i]?.inputs||'')))fail('clipboard-local-editing','held Send replayed across the transport generation',clipboard.f1.stale);
    await closeClipboard();const reconnected=await send('echo zero');
    if(reconnected.delta!=='echo zero')fail('clipboard-local-editing','fresh preview did not target the reconnected attachment',reconnected.delta);
    await control({snippetDelayMs:1500});await closeClipboard();await tabA.navigate(unifiedURL(await freshControlHandle()));
    assert((await tabA.waitUntil(isLive)).state,'clipboard-local-editing: fresh page did not commit');
    await tabA.tapTile('Clipboard');await delay(100);const readsAtDispose=(await store()).get;
    await tabA.navigate(origin+'/');await delay(2200);const readsAfterDispose=(await store()).get;
    clipboard.f1.disposal={readsAtDispose,readsAfterDispose};
    if(readsAfterDispose>readsAtDispose+1)fail('clipboard-local-editing','disposed pane kept polling shared text',clipboard.f1.disposal);

    // F4: one document poll, cross-device publication, monotone final state.
    await freshClipboardPage();await openList('clips');const pollStart=await store();
    await control({snippetWrite:[{kind:'osc',body:'value from profile A',origin:'laptop'}]});
    await waitClipboard(value=>value.rows.some(row=>row.label==='value from profile A'));
    const arrived=await store();await delay(8500);const polled=await store();
    clipboard.f4={gets:polled.get-arrived.get,peak:polled.getsInFlightPeak,crossDeviceReads:arrived.get-pollStart.get};
    if(polled.get-arrived.get>4 || polled.getsInFlightPeak!==1)fail('clipboard-shared-polling','shared text polling overlaps or scales with views',clipboard.f4);
    await control({snippetDelayMs:1500});await addText('first overlapping publication');await addText('second overlapping publication');
    await waitClipboard(value=>value.rows.some(row=>row.label==='first overlapping publication')&&value.rows.some(row=>row.label==='second overlapping publication'),12000);
    await control({snippetDelayMs:0});

    // F5: refusal/conflict/capacity remain authoritative. Extending to no
    // expiry preserves one stable item; Delete uses its captured revision.
    await freshClipboardPage({snippetForce507:1});const fullBefore=await store();await addText('must not land');
    const full=await waitClipboard(value=>/full/i.test(value.status));const fullAfter=await store();
    clipboard.f5.full={status:full.status,before:fullBefore.items,after:fullAfter.items};
    if(fullAfter.items!==fullBefore.items)fail('clipboard-full-store-refusal','full-store refusal created an optimistic item',clipboard.f5.full);
    await freshClipboardPage({snippetWrite:[{kind:'clip',label:'pin me',body:'echo pin'}]});
    const pinRow=await exactRow('echo pin'), keepWire=await wire();
    await tabA.evaluate(`(() => { const row=Array.from(document.querySelectorAll('[data-clipboard-item]')).find(node=>node.dataset.clipboardItem===${JSON.stringify(pinRow.id)});const select=row.querySelector('select');if(select.options[select.options.length-1]?.value!=='0')throw new Error('No-expiry option missing');select.focus({preventScroll:true});})()`);
    await tabA.pressKey('End','End',35);
    const permanent=await waitClipboard(value=>value.rows.some(row=>row.id===pinRow.id&&row.expiry==='No expiry'));
    await noWireChange(keepWire,'extending text expiry');
    const kept=await store();clipboard.f5.keep={snippets:kept.snippets,clips:kept.manualClips,id:pinRow.id,rows:permanent.rows};
    if(kept.snippets!==0||kept.manualClips!==1||permanent.rows.length!==1)fail('clipboard-full-store-refusal','No expiry duplicated or replaced the original text',clipboard.f5.keep);
    await control({snippetForceConflict:1});const conflictBefore=await store();await rowAction('echo pin','Delete item');
    const conflicted=await waitClipboard(value=>/changed on another device/i.test(value.status));const conflictAfter=await store();
    clipboard.f5.conflict={status:conflicted.status,before:conflictBefore.items,after:conflictAfter.items};
    if(conflictAfter.items!==conflictBefore.items)fail('clipboard-full-store-refusal','conflicted deletion changed the store',clipboard.f5.conflict);
    await freshClipboardPage({snippetSeed:{snippets:256,clips:20},snippetWrite:[{kind:'osc',body:'automatic value'}]});
    await addText('ring replacement');const capacity=await store();clipboard.f5.capacity={snippets:capacity.snippets,manualClips:capacity.manualClips,osc:capacity.osc,body:capacity.oscBody};
    if(capacity.snippets!==256||capacity.manualClips!==21||capacity.osc!==1||capacity.oscBody!=='automatic value')fail('clipboard-full-store-refusal','manual clipboard insertion violated store capacity isolation',clipboard.f5.capacity);

    // R5/F5: exact terminal-selection copying settles on the device without
    // waiting for or depending on shared storage, including cold/warm outages,
    // failed mutation probes and text exceeding the store's 16KiB limit.
    for(const mode of ['cold','503','transport']) {
      await freshClipboardPage(mode==='cold'?{snippetUnavailable:true}:{});await installClipboardStub();await openList('clips');
      if(mode==='cold')await waitClipboard(value=>/Shared text is unavailable/.test(value.text));
      else {await waitClipboard(value=>/clipboard is empty/i.test(value.text));await control(mode==='503'?{snippetMutationsUnavailable:true}:{snippetMutationsDrop:true});}
      const before=await store(),expected=await copyTerminal();await delay(500);const first=await store();
      const firstStatus=(await tabA.state()).toast;
      const secondExpected=await copyTerminal();await delay(300);const after=await store();
      const writes=await tabA.evaluate('window.__clipboardDevice.writes');
      clipboard.f5[mode]={firstMutations:first.mutations-before.mutations,secondMutations:after.mutations-first.mutations,writes:writes.length,bytes:writes[0]?.length,status:firstStatus};
      if(!/Copied to this device.*(?:unavailable|unreachable)/i.test(firstStatus))fail('clipboard-store-recoverya',`${mode} hid successful local Copy behind the store outage`,{firstStatus});
      if(writes.length!==2||writes[0]!==expected||writes[1]!==secondExpected||!expected.trim())fail('clipboard-store-recovery',`${mode} outage broke local terminal Copy`,clipboard.f5[mode]);
      if(after.mutations!==first.mutations || (mode==='cold'&&first.mutations!==before.mutations))fail('clipboard-store-recovery',`${mode} kept mutating a store already known unavailable`,clipboard.f5[mode]);
    }
    await freshClipboardPage({snippetMutationDelayMs:1500});await installClipboardStub();await openList('clips');await waitClipboard(value=>/clipboard is empty/i.test(value.text));
    await copyTerminal();const independent=await tabA.evaluate(`({writes:window.__clipboardDevice.writes.length,selecting:!document.querySelector('.persea-unified-select').hidden})`);
    clipboard.f5.localFirst=independent;if(independent.writes!==1||independent.selecting)fail('clipboard-store-recovery','local Copy waited for the shared mutation',independent);
    await delay(1600);
    await freshClipboardPage();await installClipboardStub();await openList('clips');await closeClipboard();
    await control({writeLive:Array.from({length:320},(_,i)=>('large-'+i+' '+ 'x'.repeat(64))).join('\r\n')+'\r\n'});await delay(400);
    const oversizeBefore=await store(),largeText=await copyTerminal();await delay(250);const oversizeAfter=await store();
    const largeWrites=await tabA.evaluate('window.__clipboardDevice.writes');clipboard.f5.oversize={bytes:Buffer.byteLength(largeText),writes:largeWrites.length,mutations:oversizeAfter.mutations-oversizeBefore.mutations};
    if(Buffer.byteLength(largeText)<=16*1024)fail('clipboard-store-recovery','oversize copy precondition did not exceed store limit',clipboard.f5.oversize);
    else if(largeWrites.length!==1||largeWrites[0]!==largeText||oversizeAfter.mutations!==oversizeBefore.mutations)fail('clipboard-store-recovery','over-limit local Copy was lost or sent to shared storage',clipboard.f5.oversize);

    // F6: exercise the secured requester through Add text. Only the NEXT
    // mutation is damaged; concurrent document reads remain ordinary reads.
    await freshClipboardPage();await openList('clips');
    await tabA.evaluate(`(() => {const original=window.fetch;window.__clipboardStrip=what=>{window.__clipboardLast=null;window.fetch=(input,init)=>{
      if(!String(input).includes('/api/snippets')||!init||!['POST','PATCH','DELETE','PUT'].includes(init.method))return original(input,init);
      const next={...init},headers={...init.headers};if(what==='csrf')delete headers['X-Persea-CSRF'];if(what==='content-type')delete headers['Content-Type'];if(what==='wrong-content-type')headers['Content-Type']='text/plain';next.headers=headers;
      if(what==='unknown-field')next.body=JSON.stringify({...JSON.parse(String(init.body)),surprise:1});if(what==='trailing')next.body=String(init.body)+' x';if(what==='oversize')next.body=JSON.stringify({kind:'clip',body:'a'.repeat(200*1024)});
      window.fetch=original;return original(input,next).then(async response=>{const text=await response.clone().text();window.__clipboardLast={status:response.status,length:text.length,text:text.slice(0,200),cache:response.headers.get('cache-control')};return response;});};};})()`);
    clipboard.f6.attacks=[];
    for(const attack of ['csrf','content-type','wrong-content-type','unknown-field','trailing','oversize']) {
      const before=await store();await tabA.evaluate(`window.__clipboardStrip(${JSON.stringify(attack)})`);await addText('security '+attack);await delay(300);
      const after=await store(),last=await tabA.evaluate('window.__clipboardLast'),state=await clipboardState();
      const record={attack,response:last,before:before.items,after:after.items,status:state.status};clipboard.f6.attacks.push(record);
      if(!last||last.status<400||last.length>256||last.cache!=='no-store'||/a{50,}/.test(last.text)||after.items!==before.items||!state.status)fail('clipboard-malformed-request',`damaged ${attack} request was not safely refused`,record);
    }
    const securityBefore=await store();await addText('positive secured request');await waitClipboard(value=>value.rows.some(row=>row.label==='positive secured request'));const securityAfter=await store();
    clipboard.f6.control={before:securityBefore.items,after:securityAfter.items};if(securityAfter.items!==securityBefore.items+1)fail('clipboard-malformed-request','unmodified secured Add text did not land',clipboard.f6.control);
    const rawAttack=async headers=>{const response=await fetch(origin+'/api/snippets',{method:'POST',headers:{'Content-Type':'application/json',...headers},body:JSON.stringify({kind:'clip',body:'raw'})});const text=await response.text();return{status:response.status,length:text.length,cache:response.headers.get('cache-control')};};
    const rawBefore=await store(),token=require('./unified_reopen_fixture.cjs').CSRF_TOKEN;
    clipboard.f6.raw={noIdentity:await rawAttack({}),wrongToken:await rawAttack({'X-Persea-CSRF':'x'.repeat(43),Cookie:'__Host-persea-terminal-csrf='+token,Origin:origin}),crossOrigin:await rawAttack({'X-Persea-CSRF':token,Cookie:'__Host-persea-terminal-csrf='+token,Origin:'http://evil.example'})};
    for(const [name,result]of Object.entries(clipboard.f6.raw))if(result.status!==403||result.length>256||result.cache!=='no-store')fail('clipboard-malformed-request',`raw ${name} request was not safely refused`,result);
    if((await store()).items!==rawBefore.items)fail('clipboard-malformed-request','raw unauthenticated request changed the store',clipboard.f6.raw);

    // The source fence: every /api/snippets call site lives in ONE module and
    // goes through ONE requester. A second fetch site anywhere else is RED.
    {
      const sources = fs.readdirSync(path.join(UI, "src")).filter((name) => name.endsWith(".ts"));
      const offenders = sources.filter((name) => name !== "snippet_client.ts"
        && fs.readFileSync(path.join(UI, "src", name), "utf8").includes("/api/snippets"));
      const client = fs.readFileSync(path.join(UI, "src/snippet_client.ts"), "utf8");
      const fetchSites = (client.match(/this\.fetcher\(/g) || []).length;
      const rawFetch = (client.match(/window\.fetch\(/g) || []).length;
      clipboard.f6.fence = { offenders, fetchSites, rawFetch, sendSites: (client.match(/this\.send\(/g) || []).length };
      if (offenders.length !== 0) fail("clipboard-malformed-request", "a module outside snippet_client.ts names /api/snippets", offenders);
      if (fetchSites !== 1) fail("clipboard-malformed-request", "snippet_client.ts does not have exactly one transport call site", clipboard.f6.fence);
      if (rawFetch !== 1) fail("clipboard-malformed-request", "snippet_client.ts reaches window.fetch outside its injected default", clipboard.f6.fence);
    }

    // --- clipboard-osc-parser / clipboard-osc-budget: the OSC 52 parser and its economics ---------------
    // Sequences are written into the LIVE stream, so xterm's own parser — the
    // single authority — is what runs. A read or a malformed payload must
    // produce zero browser-to-pane bytes and zero persistence.
    {
      await control({ reset: true, snippetSeed: { clips: 20 } });
      await tabA.navigate(unifiedURL(await freshControlHandle()));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "clipboard-osc-parser precondition: the page did not commit");
      await installFocusCounter();
      const oscSequence = (payload) => `\x1b]52;${payload}\x07`;
      const b64 = (value) => Buffer.from(value, "utf8").toString("base64");
      clipboard.f7.corpus = [];
      const oscProbe = async (name, payload) => {
        const beforeAttachment = await lastAttachment();
        const beforeStore = await store();
        await control({ writeLive: oscSequence(payload) });
        await delay(1_100);
        const afterAttachment = await lastAttachment();
        const afterStore = await store();
        const record = {
          name,
          inputsDelta: afterAttachment.inputs.length - beforeAttachment.inputs.length,
          bytesDelta: afterAttachment.inputs.slice(beforeAttachment.inputs.length).join("").length,
          oscPutsDelta: afterStore.oscPut - beforeStore.oscPut,
          osc: afterStore.osc, manualClips: afterStore.manualClips,
        };
        clipboard.f7.corpus.push(record);
        return record;
      };
      for (const [name, payload] of [
        ["read", "c;?"],
        ["read-primary", "p;?"],
        ["bad-selection", `s;${b64("nope")}`],
        ["bad-base64", "c;not base64!"],
        ["bad-utf8", `c;${Buffer.from([0xff, 0xfe]).toString("base64")}`],
        ["oversize", `c;${b64("a".repeat(16 * 1024 + 1))}`],
        ["control-byte", `c;${b64("bad\x07bell")}`],
      ]) {
        const record = await oscProbe(name, payload);
        if (record.bytesDelta !== 0 || record.inputsDelta !== 0) fail("clipboard-osc-parser", `${name} put bytes into the pane`, record);
        if (record.oscPutsDelta !== 0) fail("clipboard-osc-parser", `${name} reached the store`, record);
      }
      // Valid writes: ASCII, CJK, emoji and exactly 16 KiB each round-trip.
      for (const [name, value] of [["ascii", "echo ascii"], ["cjk", "日本語のテキスト"], ["emoji", "🎉 done"], ["exact-16k", "a".repeat(16 * 1024)]]) {
        const record = await oscProbe(name, `c;${b64(value)}`);
        const after = await store();
        record.stored = after.oscBody === value;
        if (record.bytesDelta !== 0 || record.inputsDelta !== 0) fail("clipboard-osc-parser", `a valid ${name} write put bytes into the pane`, record);
        if (record.oscPutsDelta !== 1) fail("clipboard-osc-parser", `a valid ${name} write did not publish exactly once`, record);
        if (!record.stored) fail("clipboard-osc-parser", `a valid ${name} write did not store its value`, { record, oscBody: after.oscBody.slice(0, 40) });
        if (after.osc !== 1) fail("clipboard-osc-budget", `after a ${name} write the store holds ${after.osc} OSC records`, after);
        // Ordinary history holds the ring of 20 plus the current canonical
        // OSC value. The internal fixed-ID publication remains separate.
        if (after.manualClips !== 21 || after.items !== 22 || new Set(after.ids).size !== 22) fail("clipboard-osc-budget", `a ${name} write violated canonical clipboard capacity`, {manualClips:after.manualClips,items:after.items,ids:after.ids});
      }
      // Thirty rapid writes coalesce into exactly one publication of the LAST
      // valid value. This is the whole economics of the distinguished record.
      {
        const before = await store();
        for (let index = 0; index < 30; index += 1) {
          await control({ writeLive: oscSequence(`c;${b64(`flood-${index}`)}`) });
        }
        await delay(1_600);
        const after = await store();
        clipboard.f7.flood = { putsDelta: after.oscPut - before.oscPut, osc: after.osc, manualClips: after.manualClips, body: after.oscBody, revision: after.oscRevision };
        if (clipboard.f7.flood.putsDelta > 3) fail("clipboard-osc-budget", "a thirty-write flood was not coalesced", clipboard.f7.flood);
        if (after.oscBody !== "flood-29") fail("clipboard-osc-parser", "the coalesced publication is not the last valid value", clipboard.f7.flood);
        if (after.osc !== 1) fail("clipboard-osc-budget", "the flood produced more than one OSC record", clipboard.f7.flood);
        if (after.manualClips !== 21 || after.items !== 22 || new Set(after.ids).size !== 22) fail("clipboard-osc-budget", "the flood violated canonical clipboard capacity", clipboard.f7.flood);
      }
      // Destroy and recreate the pane: the old handler publishes nothing and
      // the new pane has exactly one handler (one write, one publication).
      {
        const beforeNavigate = await store();
        await tabA.navigate(`${origin}/`);
        await delay(600);
        const afterNavigate = await store();
        await tabA.navigate(unifiedURL(await freshControlHandle()));
        assert((await tabA.waitUntil(isLive, 10_000)).state, "clipboard-osc-parser precondition: the reopened page did not commit");
        await installFocusCounter();
        const beforeWrite = await store();
        await control({ writeLive: oscSequence(`c;${b64("after reopen")}`) });
        await delay(1_400);
        const afterWrite = await store();
        clipboard.f7.lifecycle = {
          postDestroyPuts: afterNavigate.oscPut - beforeNavigate.oscPut,
          reopenPuts: afterWrite.oscPut - beforeWrite.oscPut,
          osc: afterWrite.osc, body: afterWrite.oscBody,
        };
        if (clipboard.f7.lifecycle.postDestroyPuts !== 0) fail("clipboard-osc-parser", "a destroyed pane's handler published after destruction", clipboard.f7.lifecycle);
        if (clipboard.f7.lifecycle.reopenPuts !== 1) fail("clipboard-osc-parser", "the reopened pane does not have exactly one OSC handler", clipboard.f7.lifecycle);
        if (afterWrite.osc !== 1) fail("clipboard-osc-budget", "the reopened pane created a second OSC record", clipboard.f7.lifecycle);
      }
      // The OSC write path never uses POST and never mints a per-pane id: the
      // store's id set proves it.
      const finalStore = await store();
      clipboard.f7.ids = finalStore.ids.filter((id) => id === "osc52" || !/^[0-9a-f]{32}$/.test(id));
      if (clipboard.f7.ids.length !== 1 || clipboard.f7.ids[0] !== "osc52") fail("clipboard-osc-budget", "the OSC path minted an id other than the one global record", finalStore.ids);
      // clipboard-osc-budget is measured by the same OSC run as clipboard-osc-parser (one corpus, one
      // flood, one reopen). Rather than run it twice, the F8-owned facts are
      // surfaced under their own key so the acceptance ledger has an explicit
      // F8 row: store economics, not decoding.
      clipboard.f8 = { flood: clipboard.f7.flood, lifecycle: clipboard.f7.lifecycle, ids: clipboard.f7.ids, records: finalStore.osc, manualClips: finalStore.manualClips };
    }



    // F10/J3: actual Clipboard rows render text, disclose retention, retain
    // 44px targets and native scrolling, and do not put bytes into a pane.
    await freshClipboardPage({snippetSeed:{snippets:12,clips:12},snippetWrite:[{kind:'osc',body:'automatic row',origin:'laptop'}]});
    const stylesBeforeClipboard=await tabA.evaluate(`document.querySelectorAll('style').length`);
    await openList('clips');const populated=await waitClipboard(value=>value.rows.length>=13);
    clipboard.f10.rows=populated.rows;
    const panelTargets=populated.actions.filter(action=>action.rect.w>0&&action.rect.h>0);
    if(panelTargets.some(action=>action.rect.w<43.5||action.rect.h<43.5))fail('clipboard-markup-safety','Clipboard has targets smaller than 44px',panelTargets);
    const retention=populated.rows.map(row=>row.expiry);if(!retention.some(value=>/30 min left/.test(value))||!retention.some(value=>value==='No expiry'))fail('clipboard-markup-safety','Unified list fails to disclose each item retention',retention);
    const beforeScroll=await wire();
    const bodyBox=await tabA.pointFor(`document.querySelector('.persea-clipboard__body')`);
    const scrollBefore=await tabA.evaluate(`document.querySelector('.persea-clipboard__body').scrollTop`);
    await tabA.cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:bodyBox.x,y:bodyBox.y+bodyBox.h/3}]});
    for(let step=1;step<=8;step++){await tabA.cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:bodyBox.x,y:bodyBox.y+bodyBox.h/3-step*12}]});await delay(20);}
    await tabA.cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await delay(200);
    const scrollAfter=await tabA.evaluate(`document.querySelector('.persea-clipboard__body').scrollTop`);clipboard.f10.scroll={before:scrollBefore,after:scrollAfter};
    if(scrollAfter<=scrollBefore)fail('clipboard-markup-safety','native Clipboard drag did not scroll overflowing content',clipboard.f10.scroll);
    const clipboardStyles=await tabA.evaluate(`({count:document.querySelectorAll('style').length,unnonced:[...document.querySelectorAll('style')].filter(node=>!node.nonce).length})`);
    clipboard.f10.styles={before:stylesBeforeClipboard,...clipboardStyles};
    if(clipboardStyles.count!==stylesBeforeClipboard||clipboardStyles.unnonced!==0)fail('clipboard-markup-safety','Clipboard growth bypassed the fixed nonced style owner',clipboard.f10.styles);
    await noWireChange(beforeScroll,'native Clipboard scrolling');await shot(tabA,'phone-06-clipboard');

    // R6: automatic output reaches the list without terminal input. A refused
    // device copy remains visibly retryable; only a trusted copy of the owed
    // value acknowledges it. Unrelated/stale text must not replace that value.
    await freshClipboardPage();await installClipboardStub();await tabA.evaluate('window.__clipboardDevice.fail=true');
    await control({writeLive:'\x1b]52;c;'+Buffer.from('automatic A').toString('base64')+'\x07'});await delay(1200);
    await openList('clips');await waitClipboard(value=>value.handoff&&value.rows.some(row=>row.label==='automatic A'));
    const oscWire=await wire();await clipboardAction('Copy terminal text to this device');
    const refused=await waitClipboard(value=>/refused/i.test(value.status));clipboard.r6.refused={status:refused.status,handoff:refused.handoff};
    if(!refused.handoff)fail('clipboard-device-handoff','refused trusted Copy retired the device handoff',clipboard.r6.refused);
    // The floating message intentionally covers this header-adjacent handoff.
    // Dismiss it through its visible control before retrying the owed copy.
    await clipboardAction('Dismiss clipboard message');
    await tabA.evaluate('window.__clipboardDevice.fail=false');await clipboardAction('Copy terminal text to this device');
    await waitClipboard(value=>!value.handoff);const oscWrites=await tabA.evaluate('window.__clipboardDevice.writes');
    if(oscWrites.length!==1||oscWrites[0]!=='automatic A')fail('clipboard-device-handoff','trusted OSC Copy did not deliver the exact owed value',oscWrites);
    await noWireChange(oscWire,'OSC copy retry');
    await tabA.evaluate('window.__clipboardDevice.fail=true');
    await control({writeLive:'\x1b]52;c;'+Buffer.from('automatic B').toString('base64')+'\x07'});await delay(1200);
    await waitClipboard(value=>value.handoff&&value.rows.some(row=>row.label==='automatic B'));
    await control({snippetWrite:[{kind:'clip',body:'unrelated manual copy'}]});
    await exactRow('unrelated manual copy');await tabA.evaluate('window.__clipboardDevice.fail=false');await rowAction('unrelated manual copy','Copy to device');
    const unrelated=await waitClipboard(value=>/Copied/.test(value.status));
    if(!unrelated.handoff)fail('clipboard-device-handoff','copying unrelated manual text acknowledged the terminal handoff',unrelated);
    const oldB=await exactRow('automatic B');await tabA.evaluate('window.__clipboardDevice.fail=true');
    await control({writeLive:'\x1b]52;c;'+Buffer.from('automatic C').toString('base64')+'\x07'});await delay(1200);
    // The old canonical B row remains available while the owed copy is C.
    await waitClipboard(value=>value.rows.some(row=>row.label==='automatic C'));
    await tabA.evaluate('window.__clipboardDevice.fail=false');await rowAction(oldB.id,'Copy to device');
    const stale=await waitClipboard(value=>/Copied/.test(value.status));const staleWrites=await tabA.evaluate('window.__clipboardDevice.writes');clipboard.r6.stale={id:oldB.id,copied:staleWrites.at(-1),handoff:stale.handoff};
    if(staleWrites.at(-1)!=='automatic B'||!stale.handoff)fail('clipboard-device-handoff','stale preview acknowledged the newer terminal handoff',clipboard.r6.stale);
    const beforeAcknowledge=await store();await clipboardAction('Dismiss clipboard message');await clipboardAction('Copy terminal text to this device');await waitClipboard(value=>!value.handoff);const afterAcknowledge=await store();
    clipboard.r6.writes=await tabA.evaluate('window.__clipboardDevice.writes');
    if(clipboard.r6.writes.at(-1)!=='automatic C')fail('clipboard-device-handoff','matching handoff copied a stale value',clipboard.r6.writes);
    if(afterAcknowledge.mutations!==beforeAcknowledge.mutations)fail('clipboard-device-handoff','acknowledging a shared handoff renewed or recreated its item',{before:beforeAcknowledge.mutations,after:afterAcknowledge.mutations});
    await noWireChange(oscWire,'value-bound handoff acknowledgement');
    await closeClipboard();await openList();if((await clipboardState()).handoff)fail('clipboard-device-handoff','acknowledged handoff reappeared after reopening',{});

    // R7: device import stays in the list. Explicit edits and Cancel never
    // imply terminal Paste or resize; permission denial opens no editor.
    for(const keyboardOpen of [false,true]) {
      await freshClipboardPage();await installClipboardStub();
      if(keyboardOpen){await tabA.tapTile('Show keyboard');await tabA.emulate(PHONE_KEYBOARD,true);await delay(100);}
      const before=await wire(),focusBefore=(await tabA.state()).focusCalls;
      await tabA.activate(await tabA.state(),await tabA.pointFor(`document.querySelector('.persea-unified-toolbar-paste')`));
      const readsBeforeImport=await tabA.evaluate('window.__clipboardDevice.reads');if(readsBeforeImport!==0)fail('clipboard-explicit-import','Paste read the device clipboard before explicit Import',readsBeforeImport);
      await clipboardAction('Paste from device');await waitClipboard(value=>value.draft===null&&value.rows.some(row=>row.label==='device text'));await noWireChange(before,'device import');
      await preview('device text');await noWireChange(before,'explicit imported text editor');
      await clipboardAction('Cancel');await noWireChange(before,'import Cancel');
      if((await tabA.state()).focusCalls!==focusBefore)fail('clipboard-explicit-import','Clipboard transaction focused xterm implicitly',{keyboardOpen});
      await openList();const sendBefore=await lastAttachment();await rowAction('device text','Paste text to terminal');await delay(100);const sendAfter=await lastAttachment();
      if(sendAfter.inputs.slice(sendBefore.inputs.length).join('')!=='device text'||sendAfter.resizes!==sendBefore.resizes)fail('clipboard-explicit-import','explicit imported text Send changed bytes or geometry',{keyboardOpen});
      clipboard.r7[keyboardOpen?'open':'closed']={reads:await tabA.evaluate('window.__clipboardDevice.reads'),sent:sendAfter.inputs.slice(sendBefore.inputs.length).join('')};
    }
    await freshClipboardPage();await installClipboardStub();await tabA.evaluate('window.__clipboardDevice.readFail=true');await openList();const denialWire=await wire();await clipboardAction('Paste from device');
    const denial=await waitClipboard(value=>/cancelled or blocked/.test(value.status));if(denial.draft!==null||!/Add text/.test(denial.status)||!denial.actions.some(action=>action.label==='Add image'))fail('clipboard-explicit-import','permission refusal omitted the local manual route',denial);
    await noWireChange(denialWire,'device clipboard permission refusal');clipboard.r7.denial=denial.status;
    await clipboardAction('Add text');await waitClipboard(value=>value.draft==='');await clipboardAction('Cancel');await noWireChange(denialWire,'explicit manual fallback Cancel');
    await closeClipboard();

    // J5: keyboard actions stay discoverable in the task menu after using
    // Clipboard. The menu no longer embeds a long saved-text list.
    clipboard.j5=[];
    for(const keyboardOpen of [false,true]) {
      if(keyboardOpen){await tabA.tapTile('Show keyboard');await tabA.emulate(PHONE_KEYBOARD,true);await delay(100);}
      await openSheet();const taskMenu=await tabA.state();
      const label=keyboardOpen?'Hide keyboard':'Show keyboard',tile=taskMenu.tiles[label];
      const inside=tile?.rect&&taskMenu.sheetRect&&tile.rect.y>=taskMenu.sheetRect.y-.5&&tile.rect.y+tile.rect.h<=taskMenu.sheetRect.y+taskMenu.sheetRect.h+.5;
      const measured={keyboardOpen,label,headings:taskMenu.sheetHeadings,inside,scroll:taskMenu.sheetScroll};clipboard.j5.push(measured);
      if(!inside)fail('CLIPBOARD-J5','applicable keyboard action requires discovering a menu scroll',measured);
      await tabA.tap((await tabA.state()).openerRect);await delay(100);
    }
    await tabA.emulate(DESKTOP,false);await freshClipboardPage({},false);const fine=await tabA.state();clipboard.f9.finePointer={openerVisible:fine.openerVisible,puckPresent:fine.puckPresent};
    if(!fine.openerVisible||fine.puckPresent)fail('clipboard-touch-focus','fine pointer lacks the single Clipboard route',clipboard.f9.finePointer);
    await tabA.emulate(PHONE,true);

    }
    // --- preferences: one durable preference record, live themes, font baseline ----
    // This uses the same real bundle/attachment as the rest of this gate. A
    // theme change must repaint that xterm in place: no second xterm, style
    // node, websocket, INPUT or RESIZE_REQUEST is permitted.
    {
      const preferences = { themes: [], font: {}, degraded: {}, containment: {}, interleavings: {} };
      evidence.preferences = preferences;
      const waitPreference = async (predicate, timeout = 8_000) => {
        const deadline = Date.now() + timeout;
        let current;
        while (Date.now() < deadline) {
          current = await snapshot();
          if (predicate(current)) return current;
          await delay(50);
        }
        return current;
      };
      // the terminal has no theme control. A theme reaches it as a
      // record another surface stored (the dashboard's Appearance card, another
      // tab): the fixture stores the record at the next revision and the page
      // is told the way the product tells it — the `storage` event of the
      // preference hint key — which triggers one authoritative read (14.3b).
      const HINT_KEY = "persea-terminal.operator-preferences-hint.v1";
      const publishRecord = async (patch) => {
        const current = await snapshot();
        await control({ preferences: { ...patch, revision: current.preferences.revision + 1, stored: true } });
        const signalled = await tabA.evaluate(`(() => {
          window.dispatchEvent(new StorageEvent("storage", { key: ${JSON.stringify(HINT_KEY)}, newValue: "{}" }));
          return true;
        })()`);
        assert(signalled, "preferences precondition: the storage signal could not be dispatched");
      };
      const chooseTheme = async (id) => {
        await publishRecord({ theme: id });
        const themed = await tabA.waitUntil((state) => state.theme === id, 4_000);
        assert(themed.state, `theme-persistence precondition: the stored theme ${id} did not reach the terminal`);
        return waitPreference((value) => value.preferences.theme === id);
      };

      // preference-intent-ownership: the delayed Zoom publication owns 15,
      // even when an immediate theme publication first returns the stored 14.
      await tabA.emulate(DESKTOP, false);
      await tabA.navigate("about:blank");
      // An EXPLICIT 14 in the record. Under terminal topbar an empty record is auto, and
      // this regression test is about a delayed font publication owning its exact
      // value across an unrelated commit — it needs a known starting number,
      // not the viewport's. The tri-state itself is measured by font-auto-preference below.
      await control({ reset: true, preferences: { font_size: 14 } });
      await tabA.navigate(unifiedURL(await freshControlHandle()));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "preference-intent-ownership precondition: the preference page did not commit");
      const interleavingBefore = await tabA.state();
      assert(interleavingBefore.fontBaseline === 14, "preference-intent-ownership precondition: the live font baseline was not 14");
      assert(interleavingBefore.fontSize === 14, "preference-intent-ownership precondition: an explicit preference did not override the fit");
      // the theme now arrives as a stored record (another surface's
      // save) while this page's own delayed Zoom write is pending.
      const zoomClicked = await tabA.evaluate(`(() => {
        const zoom = Array.from(document.querySelectorAll("button")).find((node) => node.textContent === "Zoom in");
        if (!zoom) return false;
        zoom.click();
        return true;
      })()`);
      assert(zoomClicked, "preference-intent-ownership precondition: Zoom control absent");
      await publishRecord({ theme: "dracula" });
      const interleavingStored = await waitPreference((value) => value.counters.preferencesPut >= 1
        && value.preferences.font_size === 15 && value.preferences.theme === "dracula", 4_000);
      await delay(150);
      const interleavingLive = await tabA.state();
      preferences.interleavings.zoomThenTheme = {
        intendedFont: 15,
        storedFont: interleavingStored.preferences.font_size,
        liveBaseline: interleavingLive.fontBaseline,
        storedTheme: interleavingStored.preferences.theme,
        liveTheme: interleavingLive.theme,
        preferencePuts: interleavingStored.counters.preferencesPut,
      };
      if (interleavingStored.preferences.font_size !== 15 || interleavingLive.fontBaseline !== 15) {
        fail("preference-intent-ownership", "Zoom intent lost across theme commit", preferences.interleavings.zoomThenTheme);
      }
      if (interleavingStored.preferences.theme !== "dracula" || interleavingLive.theme !== "dracula") {
        fail("preference-intent-ownership", "theme intent lost across delayed Zoom commit", preferences.interleavings.zoomThenTheme);
      }

      const clickZoomIn = async (times = 1) => {
        const clicked = await tabA.evaluate(`((times) => {
          const button = Array.from(document.querySelectorAll("button")).find((node) => node.textContent === "Zoom in");
          if (!button) return false;
          for (let index = 0; index < times; index += 1) button.click();
          return true;
        })(${times})`);
        assert(clicked, "preference-intent-ownership matrix precondition: Zoom in control absent");
      };
      // A theme "dispatched" during the matrix is a record stored elsewhere and
      // signalled to this page (terminal appearance §15.6); the page's only preference write
      // is Zoom. `refresh()` is serialised behind writes, so a signal that
      // lands while a PUT is held is read only after that PUT settles.
      const dispatchTheme = async (id) => publishRecord({ theme: id });
      const startPreferenceTrace = () => tabA.evaluate(`(() => {
        const shell = document.querySelector(".persea-unified-terminal");
        if (!shell) return false;
        const trace = [{ font: Number(shell.dataset.fontBaseline), theme: shell.dataset.theme }];
        const observer = new MutationObserver(() => trace.push({ font: Number(shell.dataset.fontBaseline), theme: shell.dataset.theme }));
        observer.observe(shell, { attributes: true, attributeFilter: ["data-font-baseline", "data-theme"] });
        window.__preferencesPreferenceTrace = { trace, observer };
        return true;
      })()`);
      const finishPreferenceTrace = () => tabA.evaluate(`(() => {
        const value = window.__preferencesPreferenceTrace;
        if (!value) return [];
        value.observer.disconnect();
        delete window.__preferencesPreferenceTrace;
        return value.trace.filter((entry, index, all) => index === 0 || entry.font !== all[index - 1].font || entry.theme !== all[index - 1].theme);
      })()`);
      const runPreferenceInterleaving = async (name, exercise, expected) => {
        await tabA.navigate("about:blank");
        // An EXPLICIT 14 in the seeded record. Under terminal topbar an empty record is
        // auto and this desktop fits to ~21, so an unseeded matrix would move
        // its own starting number every time the viewport changed. The matrix
        // measures intent identity and publication order across interleaved
        // writes, not the tri-state; font-auto-preference below measures the tri-state.
        await control({ reset: true, preferences: { font_size: 14 } });
        await tabA.navigate(unifiedURL(await freshControlHandle()));
        assert((await tabA.waitUntil(isLive, 10_000)).state, `preference-intent-ownership/${name} precondition: page did not commit`);
        assert(await startPreferenceTrace(), `preference-intent-ownership/${name} precondition: trace did not install`);
        const beforeState = await tabA.state();
        const beforeFixture = await snapshot();
        const beforeAttachment = await lastAttachment();
        await exercise();
        await delay(1_300);
        const afterFixture = await snapshot();
        // the preference status lives in the View and appearance popover,
        // so the outcome is read where the operator meets it. The popover is
        // presentation-only and is closed again before the next scenario.
        const beforeView = await tabA.state();
        if (!beforeView.viewVisible) {
          await tabA.activate(beforeView, beforeView.fitRect);
          await delay(200);
        }
        const afterState = await tabA.state();
        if (!beforeView.viewVisible) {
          await tabA.activate(afterState, afterState.fitRect);
          await delay(150);
        }
        const afterAttachment = await lastAttachment();
        const trace = await finishPreferenceTrace();
        const operations = afterFixture.preferenceOperations.map(({ theme, font_size, outcome }) => ({ theme, font: font_size, outcome }));
        const receipt = {
          name,
          stored: { font: afterFixture.preferences.font_size, theme: afterFixture.preferences.theme },
          live: { font: afterState.fontBaseline, theme: afterState.theme },
          operations,
          trace,
          // the page's write outcome is spoken by the bounded toast.
          status: [afterState.toast].filter(Boolean),
          transport: {
            attachments: afterFixture.attachments.length - beforeFixture.attachments.length,
            sameAttachment: beforeAttachment.id === afterAttachment.id,
            inputs: afterAttachment.inputs.length - beforeAttachment.inputs.length,
            resizes: (afterAttachment.resizes || 0) - (beforeAttachment.resizes || 0),
            styleNodes: afterState.styleNodes - beforeState.styleNodes,
          },
        };
        preferences.interleavings[name] = receipt;
        const exact = (left, right) => JSON.stringify(left) === JSON.stringify(right);
        if (!exact(receipt.stored, expected.stored) || !exact(receipt.live, expected.live)) {
          fail("preference-intent-ownership", `${name} did not settle to the exact stored/live preferences`, receipt);
        }
        if (!exact(operations, expected.operations)) fail("preference-intent-ownership", `${name} changed preference operation order`, receipt);
        if (!exact(trace, expected.trace)) fail("preference-intent-ownership", `${name} transiently rolled back a live preference`, receipt);
        if (expected.status === "clear" && receipt.status.length !== 0) fail("preference-intent-ownership", `${name} left a stale preference status`, receipt);
        if (expected.status !== "clear" && (receipt.status.length === 0
            || !receipt.status.every((message) => new RegExp(expected.status, "i").test(message)))) {
          fail("preference-intent-ownership", `${name} did not render its ${expected.status} outcome`, receipt);
        }
        if (receipt.transport.attachments !== 0 || !receipt.transport.sameAttachment || receipt.transport.inputs !== 0
            || receipt.transport.resizes !== 0 || receipt.transport.styleNodes !== 0) {
          fail("preference-intent-ownership", `${name} touched transport, input, geometry, reconnect, or style nodes`, receipt);
        }
      };

      await runPreferenceInterleaving("zoom-theme", async () => {
        await clickZoomIn();
        await dispatchTheme("dracula");
      }, {
        stored: { font: 15, theme: "dracula" }, live: { font: 15, theme: "dracula" },
        operations: [
          { theme: "dracula", font: 15, outcome: "saved" },
        ],
        trace: [{ font: 14, theme: "default" }, { font: 15, theme: "default" }, { font: 15, theme: "dracula" }],
        status: "clear",
      });
      await runPreferenceInterleaving("theme-zoom", async () => {
        await chooseTheme("dracula");
        await clickZoomIn();
      }, {
        stored: { font: 15, theme: "dracula" }, live: { font: 15, theme: "dracula" },
        operations: [
          { theme: "dracula", font: 15, outcome: "saved" },
        ],
        trace: [{ font: 14, theme: "default" }, { font: 14, theme: "dracula" }, { font: 15, theme: "dracula" }],
        status: "clear",
      });
      await runPreferenceInterleaving("two-zooms-theme", async () => {
        await control({ holdPreferencePuts: 1 });
        await clickZoomIn();
        const held = await waitPreference((value) => value.pendingPreferencePuts === 1, 2_000);
        assert(held.pendingPreferencePuts === 1, "preference-intent-ownership/two-zooms-theme precondition: first font PUT was not held");
        await clickZoomIn();
        await dispatchTheme("dracula");
        await control({ releasePreferencePut: true });
      }, {
        stored: { font: 16, theme: "dracula" }, live: { font: 16, theme: "dracula" },
        // The held first Zoom write carries the pre-record If-Match and so
        // conflicts with the record stored meanwhile; the newer Zoom intent
        // survives that older operation's conflict and stores on the record.
        operations: [
          { theme: "default", font: 15, outcome: "conflict" },
          { theme: "dracula", font: 16, outcome: "saved" },
        ],
        trace: [
          { font: 14, theme: "default" }, { font: 15, theme: "default" },
          { font: 16, theme: "default" }, { font: 16, theme: "dracula" },
        ],
        status: "clear",
      });
      // A record stored elsewhere while this page's Zoom write is held: the
      // held write conflicts, the conflict publication is server authority,
      // and the matching Zoom intent settles to it (font back to the record's
      // 14, theme to the record's one-dark) — never a page-only 15.
      await runPreferenceInterleaving("zoom-conflict", async () => {
        await control({ holdPreferencePuts: 1 });
        await clickZoomIn();
        const held = await waitPreference((value) => value.pendingPreferencePuts === 1, 2_000);
        assert(held.pendingPreferencePuts === 1, "preference-intent-ownership/zoom-conflict precondition: the font PUT was not held");
        await dispatchTheme("one-dark");
        await control({ releasePreferencePut: true });
      }, {
        stored: { font: 14, theme: "one-dark" }, live: { font: 14, theme: "one-dark" },
        operations: [{ theme: "default", font: 15, outcome: "conflict" }],
        trace: [
          { font: 14, theme: "default" }, { font: 15, theme: "default" },
          { font: 15, theme: "one-dark" }, { font: 14, theme: "one-dark" },
        ],
        status: "Zoom not saved — changed on another device",
      });
      await runPreferenceInterleaving("font-unavailable", async () => {
        await chooseTheme("dracula");
        await clickZoomIn();
        await control({ preferencesAvailable: false });
      }, {
        stored: { font: 14, theme: "dracula" }, live: { font: 15, theme: "dracula" },
        operations: [{ theme: "dracula", font: 15, outcome: "unavailable" }],
        trace: [{ font: 14, theme: "default" }, { font: 14, theme: "dracula" }, { font: 15, theme: "dracula" }],
        status: "Zoom applied for this page only — preferences unavailable",
      });

      await tabA.emulate(DESKTOP, false);
      await control({ reset: true });
      await tabA.navigate(unifiedURL(await freshControlHandle()));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "theme-persistence precondition: the preference page did not commit");
      await delay(200);
      const before = await tabA.state();
      const beforeFixture = await snapshot();
      const beforeAttachment = await lastAttachment();
      // the catalogue is read from the product's theme module (the
      // dashboard card offers exactly this list); no terminal control lists it.
      const themeIDs = (() => {
        const source = fs.readFileSync(path.join(__dirname, "../src/unified_themes.ts"), "utf8");
        const match = /UNIFIED_THEME_IDS = Object\.freeze\(\[([^\]]+)\]/.exec(source);
        return match ? match[1].split(",").map((entry) => entry.trim().replace(/^"|"$/g, "")).filter(Boolean) : [];
      })();
      if (themeIDs.join(",") !== "default,rose-pine,rose-pine-dawn,solarized-dark,gruvbox-dark,one-dark,dracula,catppuccin-mocha") {
        fail("theme-persistence", "the theme module does not expose the closed eight-theme catalog", themeIDs);
      }
      for (const id of themeIDs) {
        if (id !== "default") await chooseTheme(id);
        await delay(80);
        const state = await tabA.state();
        preferences.themes.push({ id, applied: state.theme, body: state.bodyTheme, paint: state.paint, contrast: state.preferenceContrast, styleNodes: state.styleNodes, screen: state.screen });
        if (state.theme !== id || state.bodyTheme !== id || state.themeValues.some((value) => value !== id)) {
          fail("theme-persistence", "the stored theme did not publish to every view", preferences.themes.at(-1));
        }
        if (state.screen !== before.screen) fail("theme-persistence", "a theme change reconstructed or changed the terminal transcript", { id, before: before.screen, after: state.screen });
        const illegible = state.preferenceContrast.filter((control) => !(control.ratio >= 4.5));
        if (illegible.length) fail("preference-contrast", "a theme rendered an illegible preference control", { id, controls: illegible });
      }
      const afterThemes = await snapshot();
      const afterThemeAttachment = await lastAttachment();
      const backgrounds = new Set(preferences.themes.map((entry) => entry.paint.hostBackground));
      preferences.themeTransport = {
        attachmentsBefore: beforeFixture.attachments.length,
        attachmentsAfter: afterThemes.attachments.length,
        resizesBefore: beforeAttachment.resizes || 0,
        resizesAfter: afterThemeAttachment.resizes || 0,
        inputsBefore: beforeAttachment.inputs.length,
        inputsAfter: afterThemeAttachment.inputs.length,
        styleNodesBefore: before.styleNodes,
        styleNodesAfter: (await tabA.state()).styleNodes,
        distinctBackgrounds: backgrounds.size,
      };
      if (preferences.themeTransport.attachmentsAfter !== preferences.themeTransport.attachmentsBefore
          || preferences.themeTransport.resizesAfter !== preferences.themeTransport.resizesBefore
          || preferences.themeTransport.inputsAfter !== preferences.themeTransport.inputsBefore
          || preferences.themeTransport.styleNodesAfter !== preferences.themeTransport.styleNodesBefore) {
        fail("theme-persistence", "a live theme change touched transport, geometry, input, or CSP style nodes", preferences.themeTransport);
      }
      if (backgrounds.size < 7) fail("theme-persistence", "the curated themes do not produce distinct measured terminal paints", preferences.themeTransport);

      // Zoom is a durable baseline only after the 500 ms debounce. Fit font is
      // presentation-only and must not consume another preference revision.
      const fontBefore = await snapshot();
      const zoomed = await tabA.evaluate(`(() => {
        const button = Array.from(document.querySelectorAll("button")).find((node) => node.textContent === "Zoom in");
        if (!button) return false;
        button.click();
        return true;
      })()`);
      assert(zoomed, "font-persistence precondition: desktop Zoom in control absent");
      const afterZoom = await waitPreference((value) => value.preferences.revision > fontBefore.preferences.revision);
      await delay(650);
      const zoomState = await tabA.state();
      const putsAfterZoom = afterZoom.counters.preferencesPut;
      const fitFont = await tabA.evaluate(`(() => {
        const button = Array.from(document.querySelectorAll("button")).find((node) => node.textContent === "Fit font");
        if (!button) return false;
        button.click();
        return true;
      })()`);
      assert(fitFont, "font-persistence precondition: desktop Fit font control absent");
      await delay(900);
      const afterFitFont = await snapshot();
      preferences.font = {
        before: fontBefore.preferences,
        saved: afterZoom.preferences,
        visibleAfterZoom: zoomState.fontSize,
        visibleAfterFit: (await tabA.state()).fontSize,
        putsAfterZoom,
        putsAfterFit: afterFitFont.counters.preferencesPut,
        resizes: (await lastAttachment()).resizes || 0,
      };
      if (afterZoom.preferences.font_size < 9 || afterZoom.preferences.font_size > 24) fail("font-persistence", "the saved font baseline left the 9–24 range", preferences.font);
      // terminal topbar restates this pin rather than removing it. Fit font used to
      // write nothing, which is why a zoom could never be undone; it now writes
      // exactly one PUT, and that PUT is the auto state — never the fitted
      // number, which would pin a viewport-derived size onto every device.
      if (afterFitFont.counters.preferencesPut !== putsAfterZoom + 1) fail("font-persistence", "Fit font did not write exactly one preference PUT", preferences.font);
      if (afterFitFont.preferences.font_size !== null) fail("font-persistence", "Fit font stored a number instead of auto", preferences.font);
      if (((await lastAttachment()).resizes || 0) !== preferences.themeTransport.resizesBefore) fail("font-persistence", "font presentation controls emitted a tmux resize", preferences.font);

      // A fresh document reconciles from the server record before constructing
      // xterm. The local hint is presentation-only and cannot mint authority.
      const persistedTheme = afterFitFont.preferences.theme;
      const persistedFont = afterFitFont.preferences.font_size;
      await tabA.navigate(unifiedURL(await freshControlHandle()));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "font-persistence precondition: persisted preference reload did not commit");
      const reloaded = await tabA.state();
      preferences.reload = { theme: reloaded.theme, font: reloaded.fontSize, expectedTheme: persistedTheme, expectedFont: persistedFont };
      if (reloaded.theme !== persistedTheme) fail("font-persistence", "reload did not reconcile the durable theme", preferences.reload);
      if (persistedFont !== null || reloaded.fontPreference !== "auto") fail("font-persistence", "reload did not reconcile the durable auto font", preferences.reload);

      preferences.fontProfiles = [];
      for (const baseline of [9, 14, 24]) {
        await control({ preferences: { font_size: baseline } });
        await tabA.navigate(unifiedURL(await freshControlHandle()));
        assert((await tabA.waitUntil(isLive, 10_000)).state, `font-persistence precondition: font ${baseline} page did not commit`);
        await delay(250);
        const state = await tabA.state();
        const attachment = await lastAttachment();
        preferences.fontProfiles.push({ baseline, appliedBaseline: state.fontBaseline, effective: state.fontSize, cellHeight: state.cellHeight, rows: state.terminalRows, resizes: attachment.resizes || 0 });
        if (state.fontBaseline !== baseline) fail("font-persistence", "the server baseline was not present when the page constructed", preferences.fontProfiles.at(-1));
        // an explicit preference is what the terminal RENDERS, not a
        // starting point the auto-fit may overrule. This desktop viewport fits
        // to a size of its own, so a page that ran the fit would not land here.
        if (state.fontSize !== baseline) fail("font-persistence", "an explicit font preference did not override the auto-fit on load", preferences.fontProfiles.at(-1));
        if ((attachment.resizes || 0) !== 0) fail("font-persistence", "a server font baseline emitted a tmux resize", preferences.fontProfiles.at(-1));
      }

      // --- font-auto-preference: the font tri-state  ------------------------
      //
      // Three legs of one behaviour, on a desktop viewport whose auto-fit lands
      // strictly inside the 9-24 clamp, so no step and no fit is absorbed by an
      // edge: nothing stored means auto-fit; Zoom steps one pixel from the size
      // actually rendered and stores it as explicit; Fit restores auto-fit and
      // stores AUTO, which is what makes the zoom undoable and keeps a phone's
      // 9px floor off the same operator's desktop.
      {
        const clickFitFont = async () => {
          const clicked = await tabA.evaluate(`(() => {
            const button = Array.from(document.querySelectorAll("button")).find((node) => node.textContent === "Fit font");
            if (!button) return false;
            button.click();
            return true;
          })()`);
          assert(clicked, "font-auto-preference precondition: the Fit font control is absent");
        };
        const f1 = {};
        preferences.fontTriState = f1;
        await tabA.emulate(DESKTOP, false);
        await tabA.navigate("about:blank");
        await control({ reset: true });
        await tabA.navigate(unifiedURL(await freshControlHandle()));
        assert((await tabA.waitUntil(isLive, 10_000)).state, "font-auto-preference precondition: the auto page did not commit");
        await delay(300);
        const auto = await tabA.state();
        Object.assign(f1, { autoRendered: auto.fontSize, autoPreference: auto.fontPreference, autoStored: (await snapshot()).preferences.font_size });
        if (auto.fontPreference !== "auto") fail("font-auto-preference", "an empty record did not render as auto-fit", f1);
        if (f1.autoStored !== null) fail("font-auto-preference", "an empty record did not read as auto", f1);
        if (!(auto.fontSize > 9 && auto.fontSize < 24)) fail("font-auto-preference", "this viewport does not fit strictly inside the 9-24 clamp", f1);

        const stepped = Math.round(auto.fontSize) + 1;
        f1.expectedAfterZoom = stepped;
        const beforeZoom = await snapshot();
        await clickZoomIn(1);
        const savedZoom = await waitPreference((value) => value.counters.preferencesPut === beforeZoom.counters.preferencesPut + 1, 5_000);
        assert(savedZoom, "font-auto-preference: the zoom did not reach the record");
        const zoomed = await tabA.state();
        Object.assign(f1, { zoomRendered: zoomed.fontSize, zoomPreference: zoomed.fontPreference, zoomStored: savedZoom.preferences.font_size });
        if (zoomed.fontSize !== stepped) fail("font-auto-preference", "Zoom in did not step one pixel from the rendered auto-fit", f1);
        if (zoomed.fontPreference !== String(stepped)) fail("font-auto-preference", "Zoom in did not become the explicit preference", f1);
        if (savedZoom.preferences.font_size !== stepped) fail("font-auto-preference", "the zoom was not stored as an explicit size", f1);

        await tabA.navigate(unifiedURL(await freshControlHandle()));
        assert((await tabA.waitUntil(isLive, 10_000)).state, "font-auto-preference precondition: the explicit reload did not commit");
        await delay(300);
        const reloadedExplicit = await tabA.state();
        Object.assign(f1, { reloadRendered: reloadedExplicit.fontSize, reloadPreference: reloadedExplicit.fontPreference });
        if (reloadedExplicit.fontSize !== stepped) fail("font-auto-preference", "an explicit size did not survive the reload", f1);
        if (reloadedExplicit.fontPreference !== String(stepped)) fail("font-auto-preference", "the reloaded record was not explicit", f1);

        const beforeFit = await snapshot();
        await clickFitFont();
        const savedFit = await waitPreference((value) => value.counters.preferencesPut === beforeFit.counters.preferencesPut + 1, 5_000);
        assert(savedFit, "font-auto-preference: Fit font did not reach the record");
        await delay(400);
        const refit = await tabA.state();
        const afterFit = await snapshot();
        Object.assign(f1, {
          fitStored: savedFit.preferences.font_size, fitRendered: refit.fontSize, fitPreference: refit.fontPreference,
          fitPuts: afterFit.counters.preferencesPut - beforeFit.counters.preferencesPut,
        });
        if (savedFit.preferences.font_size !== null) fail("font-auto-preference", "Fit font stored a number instead of auto", f1);
        if (f1.fitPuts !== 1) fail("font-auto-preference", "Fit font did not write exactly one preference PUT", f1);
        if (refit.fontPreference !== "auto") fail("font-auto-preference", "Fit font did not restore auto-fit", f1);
        if (Math.abs(refit.fontSize - auto.fontSize) > 0.05) fail("font-auto-preference", "Fit font did not return to the fitted size", f1);

        await tabA.navigate(unifiedURL(await freshControlHandle()));
        assert((await tabA.waitUntil(isLive, 10_000)).state, "font-auto-preference precondition: the auto reload did not commit");
        await delay(300);
        const reloadedAuto = await tabA.state();
        Object.assign(f1, {
          autoReloadRendered: reloadedAuto.fontSize, autoReloadPreference: reloadedAuto.fontPreference,
          theme: reloadedAuto.theme, resizes: (await lastAttachment()).resizes || 0,
        });
        if (reloadedAuto.fontPreference !== "auto") fail("font-auto-preference", "auto did not survive the reload", f1);
        if (Math.abs(reloadedAuto.fontSize - auto.fontSize) > 0.05) fail("font-auto-preference", "the reloaded auto page did not fit to the same size", f1);
        if (reloadedAuto.theme !== auto.theme) fail("font-auto-preference", "the font tri-state disturbed the theme", f1);
        if (f1.resizes !== 0) fail("font-auto-preference", "the font tri-state emitted a tmux resize", f1);
      }

      // A valid default_session is consumed nowhere on a terminal document.
      // It neither navigates nor mints an attachment/session request.
      const neutralHandle = await freshControlHandle();
      const neutralBefore = await snapshot();
      await tabA.navigate(unifiedURL(neutralHandle));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "preference-reload precondition: neutral reload did not commit");
      const neutralAfter = await snapshot();
      const neutralDelta = {
        handles: neutralAfter.counters.handleRequests - neutralBefore.counters.handleRequests,
        inventory: neutralAfter.counters.inventory - neutralBefore.counters.inventory,
      };
      const containmentHandle = await freshControlHandle();
      const containmentBefore = await snapshot();
      await control({ preferences: { default_session: { realm: "elsewhere", server: "decoy", name: "not-this-pane" } } });
      await tabA.navigate(unifiedURL(containmentHandle));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "preference-reload precondition: containment reload did not commit");
      const containmentAfter = await snapshot();
      preferences.containment = {
        href: (await tabA.state()).href,
        handlesDelta: containmentAfter.counters.handleRequests - containmentBefore.counters.handleRequests,
        inventoryDelta: containmentAfter.counters.inventory - containmentBefore.counters.inventory,
        neutralDelta,
        session: containmentAfter.preferences.default_session,
      };
      if (preferences.containment.handlesDelta !== neutralDelta.handles
          || preferences.containment.inventoryDelta !== neutralDelta.inventory
          || /elsewhere|decoy|not-this-pane/.test(preferences.containment.href)) {
        fail("preference-reload", "default_session escaped into terminal navigation or authority", preferences.containment);
      }

      // Store failure is bounded, keeps the terminal live on safe defaults,
      // and exposes a calm page-only status without leaking a path.
      await control({ preferencesAvailable: false });
      await tabA.navigate(unifiedURL(await freshControlHandle()));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "preference-failure-isolation precondition: degraded preferences blocked terminal use");
      // Appearance and its status line live in the geometry disclosure,
      // so the degraded state is read where an operator actually meets it.
      const degradedClosed = await tabA.state();
      assert(degradedClosed.fitRect, "preference-failure-isolation precondition: the View and appearance disclosure is absent");
      await tabA.activate(degradedClosed, degradedClosed.fitRect);
      await delay(200);
      const degraded = await tabA.state();
      assert(degraded.viewVisible, "preference-failure-isolation precondition: the View and appearance popover did not open");
      preferences.degraded = { theme: degraded.theme, statuses: degraded.preferenceStatus, href: degraded.href };
      // the terminal carries no preference status line; a degraded
      // record is reported on the dashboard's Appearance card (landing gate and
      // preference-intent regression test). Here the terminal stays live on the
      // default theme, renders no status text, and leaks no path anywhere.
      if (degraded.preferenceStatus.length !== 0) fail("preference-failure-isolation", "the terminal still renders a preference status line", preferences.degraded);
      if (degraded.theme !== "default") fail("preference-failure-isolation", "degraded preferences did not keep the default theme", preferences.degraded);
      if (/\/(?:run|etc|opt|data4)\//.test(degraded.href)) fail("preference-failure-isolation", "the degraded page leaked a host path", preferences.degraded);

      await tabA.emulate(PHONE, true);
      await tabA.navigate(unifiedURL(await freshControlHandle()));
      assert((await tabA.waitUntil(isLive, 10_000)).state, "touch-preferences precondition: coarse preference page did not commit");
      await delay(250);
      const coarseClosed = await tabA.state();
      assert(coarseClosed.fitRect, "touch-preferences precondition: the coarse View and appearance disclosure is absent");
      await tabA.tap({ x: coarseClosed.fitRect.cx, y: coarseClosed.fitRect.cy });
      await delay(150);
      const coarse = await tabA.state();
      preferences.coarse = { targetRects: coarse.preferenceTargets, activeBefore: coarseClosed.activeKind, activeAfter: coarse.activeKind, keyBarHidden: coarse.keyBarHidden, visualBefore: coarseClosed.visualViewportHeight, visualAfter: coarse.visualViewportHeight };
      // no theme control anywhere in the terminal chrome.
      if (coarse.preferenceTargets.length !== 0) fail("touch-preferences", "the coarse View popover still exposes a theme control", preferences.coarse);
      if (coarse.activeKind !== coarseClosed.activeKind || coarse.visualViewportHeight !== coarseClosed.visualViewportHeight) fail("touch-preferences", "opening Appearance changed focus or the visual viewport", preferences.coarse);
      // theme-contrast is measured on real computed styles, on the terminal chrome AND
      // on the View and appearance popover, for every theme in the catalogue.
      // and the composer toggle is put into its expanded state first, so one
      // pass covers both surfaces plus the one control whose state is carried
      // by a border rather than by text. Each element is measured against the
      // surface that actually paints behind it, not against its own
      // (frequently transparent) background.
      await tabA.tap(coarse.composerToggleRect);
      await delay(250);
      let contrastState = await tabA.state();
      if (!contrastState.viewVisible) {
        await tabA.tap(contrastState.fitRect);
        await delay(200);
        contrastState = await tabA.state();
      }
      assert(contrastState.composerOpen && contrastState.viewVisible,
        "preference-contrast precondition: the open composer toggle and View popover are not both live");
      preferences.themeContrast = [];
      for (const id of themeIDs) {
        // the theme is stored elsewhere and reaches this page as a
        // record; the popover stays open across the publication.
        await publishRecord({ theme: id });
        const themed = await tabA.waitUntil((state) => state.theme === id, 4_000);
        const state = themed.state ?? themed.last;
        const measured = state.preferenceContrast;
        const toggle = measured.find((control) => control.borderOnSurface !== undefined);
        preferences.themeContrast.push({
          id, applied: state.theme, viewVisible: state.viewVisible, composerOpen: state.composerOpen,
          controls: measured.length,
          minimum: measured.reduce((worst, control) => (control.ratio < worst.ratio ? control : worst), measured[0]),
          toggle,
        });
        if (!themed.state) fail("preference-contrast", "the stored theme did not apply to the open chrome", preferences.themeContrast.at(-1));
        if (measured.length < 12) fail("preference-contrast", "the contrast sweep measured fewer controls than the sheet renders", preferences.themeContrast.at(-1));
        const illegible = measured.filter((control) => !(control.ratio >= 4.5));
        if (illegible.length) fail("preference-contrast", "a theme rendered illegible text on the chrome or the sheet", { id, controls: illegible });
        const dot = state.dotContrast;
        preferences.themeContrast.at(-1).dot = dot;
        if (!dot) fail("preference-contrast", "the session tag's status dot was not measured", preferences.themeContrast.at(-1));
        else {
          if (dot.states.length !== 3) {
            fail("preference-contrast", "the status-dot sweep did not measure all three states", { id, dot });
          }
          const dim = dot.states.filter((entry) => !(entry.ratio >= 3));
          if (dim.length) {
            fail("preference-contrast", "a status-dot state's fill is below 3:1 on this chrome", { id, dim, background: dot.background });
          }
          // Three states that painted the same fill would pass the ratio law
          // and still tell an operator nothing.
          if (new Set(dot.states.map((entry) => entry.fill)).size !== 3) {
            fail("preference-contrast", "the three status-dot states do not paint three different fills", { id, dot });
          }
        }
        if (!toggle) fail("preference-contrast", "the open composer toggle's state border was not measured", preferences.themeContrast.at(-1));
        else if (!(toggle.borderOnFill >= 3) || !(toggle.borderOnSurface >= 3)) {
          fail("preference-contrast", "the open composer toggle's state border is below 3:1", { id, toggle });
        }
        // The closed-theme rule repaints every toolbar button from the chrome
        // pair. Both open semantic disclosures must retain an accent distinct
        // from an ordinary Copy button.
        const affordances = state.stateAffordances;
        preferences.themeContrast.at(-1).stateAffordances = affordances;
        if (affordances.viewExpanded !== "true"
            || (affordances.view.background === affordances.plainButton.background
              && affordances.view.border === affordances.plainButton.border)) {
          fail("preference-contrast", "the theme rule flattened the open View disclosure into an ordinary button", { id, affordances });
        }
        if (affordances.toggleExpanded !== "true") fail("preference-contrast", "the composer toggle was not in its expanded state", { id, affordances });
        if (affordances.toggle.background === affordances.plainButton.background
            || affordances.toggle.border === affordances.plainButton.border) {
          fail("preference-contrast", "the theme rule flattened the open composer toggle into an ordinary button", { id, affordances });
        }
      }
      await shot(tabA, "phone-19-theme-preferences");
    }

    // --- T44 dashboard census (Chromium, coarse) ------------------------------
    await control({ reset: true });
    await tabA.emulate(PHONE, true);
    await tabA.navigate(`${origin}/`);
    const dash = await tabA.waitUntil((state) => state.ready === "complete", 6_000);
    assert(dash.state, "dashboard did not load");
    const rowReady = async () => {
      for (let attempt = 0; attempt < 200; attempt += 1) {
        if (await tabA.evaluate("!!document.querySelector('.session-disclosure')")) return true;
        await delay(25);
      }
      return false;
    };
    assert(await rowReady(), "the dashboard did not render a session row");
    const dashboardCensus = under44(await tabA.census());
    evidence.censusDashboard = dashboardCensus;
    if (dashboardCensus.length) fail("T44", "under-44px targets on the dashboard", dashboardCensus);
    const dashboardRow = await tabA.evaluate(`(() => {
      const attached = document.querySelector(".session-metadata");
      const badge = document.querySelector(".alias-badge");
      const uid = document.querySelector(".realm-uid");
      const title = document.querySelector(".realm-title h2");
      const rect = (el) => { if (!el) return null; const r = el.getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height }; };
      return { attached: attached ? { ...rect(attached), text: attached.textContent, color: getComputedStyle(attached).color, fontSize: getComputedStyle(attached).fontSize } : null, badge: rect(badge), uid: uid ? { ...rect(uid), text: uid.textContent } : null, title: rect(title), rows: Array.from(document.querySelectorAll(".session-card")).map((card) => rect(card).h) };
    })()`);
    evidence.dashboard = dashboardRow;
    // D8 protects a readable attachment signal. The redesigned row uses an
    // explicit count instead of an icon, so preserve the size and visibility
    // requirement and require the count's meaning to be present in the text.
    if (!dashboardRow.attached || dashboardRow.attached.w < 12 || dashboardRow.attached.h < 12 || !(parseFloat(dashboardRow.attached.fontSize) >= 12) || !/\d+ attached/.test(dashboardRow.attached.text)) fail("D8", "the attached-client metadata is absent or unreadable", dashboardRow.attached);
    if (dashboardRow.uid && dashboardRow.title && dashboardRow.uid.y < dashboardRow.title.y + dashboardRow.title.h - 0.5) fail("D12", "the identity caption is not under the realm heading", dashboardRow);
    const disclosure = await tabA.evaluate(`(() => { const b = document.querySelector(".session-disclosure"); const r = b.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2, w: r.width, h: r.height }; })()`);
    await tabA.tap(disclosure);
    await delay(300);
    // The row's information button reveals the facts directly, with no nested
    // disclosure. Preserve the real touch target and readable identity checks.
    if (disclosure.w < 44 || disclosure.h < 44) fail("D12", "Session information has an undersized control", disclosure);
    const information = await tabA.evaluate(`(() => { const section = document.querySelector(".session-information"); if (!section) return null; return { visible: section.getBoundingClientRect().height > 0, nested: !!section.querySelector(":scope > summary") }; })()`);
    if (!information?.visible || information.nested) fail("D12", "Session information is not directly visible after the row action", information);
    const identityFact = await tabA.evaluate(`(() => { const term = Array.from(document.querySelectorAll(".session-card dt")).find((node) => node.textContent === "Identity"); const value = term?.nextElementSibling; if (!value) return null; const rect = value.getBoundingClientRect(); return { text: value.textContent, width: rect.width, height: rect.height }; })()`);
    evidence.dashboardIdentity = identityFact;
    if (!identityFact || !/UID \d+/.test(identityFact.text) || identityFact.width <= 0 || identityFact.height <= 0) fail("D12", "the session identity is not readable in Details", identityFact);
    const expandedCensus = under44(await tabA.census());
    evidence.censusDashboardExpanded = expandedCensus;
    if (expandedCensus.length) fail("T44", "under-44px targets on the expanded dashboard row", expandedCensus);
    await shot(tabA, "phone-05-dashboard");

    // --- console triage ---------------------------------------------------------
    // The expired binding's 410 is the forced-reopen path itself.
    const tolerated = (entry) => (/WebSocket connection to .*\/ws.*failed/.test(entry.text) && /410|Error during WebSocket handshake|Unexpected response code/.test(entry.text))
      // QF6's forced reopens: the expired source binding answers 410 by
      // design, which is what sends the transport to the identity re-mint.
      || (/410/.test(entry.text) && /\/api\/attachment-handles/.test(entry.url ?? ""))
      // clipboard drives the store's refusal paths on purpose (403/400/409/413/507/503):
      // Chromium reports every non-2xx resource load, which is the probe working.
      || (/\/api\/snippets/.test(entry.url ?? "") && /\b(400|403|404|405|409|412|413|503|507)\b/.test(entry.text))
      // clipboard-store-recovery also drives a TRANSPORT loss: the fixture drops the socket with
      // no response, which is what "the store went away mid-write" looks like.
      // Narrow on purpose — only this error, only on the snippets route.
      || (/\/api\/snippets$/.test(entry.url ?? "") && /ERR_EMPTY_RESPONSE/.test(entry.text))
      // preferences drives the bounded degraded-store state deliberately.
      || (/\/api\/preferences$/.test(entry.url ?? "") && /\b(412|503)\b/.test(entry.text));
    const consoleFindings = tabs.flatMap((tab) => tab.console).filter((entry) => !tolerated(entry));
    if (consoleFindings.length !== 0) fail("console", "unexpected console entries", consoleFindings);

    if (EVIDENCE) {
      fs.mkdirSync(EVIDENCE, { recursive: true });
      fs.writeFileSync(path.join(EVIDENCE, "ergonomics-gate-evidence.json"), JSON.stringify({ status: failures.length ? "FAIL" : "PASS", failures, shots, evidence }, null, 2) + "\n");
    }
    if (failures.length !== 0) {
      process.stdout.write(JSON.stringify({ status: "FAIL", failures, evidence }, null, 2) + "\n");
      throw new Error(`unified ergonomics gate RED:\n${failures.join("\n")}`);
    }
    process.stdout.write(JSON.stringify({ status: "PASS", shots, evidence }) + "\n");
  } catch (error) {
    if (EVIDENCE) { fs.mkdirSync(EVIDENCE, { recursive: true }); fs.writeFileSync(path.join(EVIDENCE, "ergonomics-abort.json"), JSON.stringify({ status: "ABORT", error: String(error.stack || error), failures, evidence }, null, 2)); }
    // Reported before teardown: a teardown that hangs (a fixture socket the
    // browser never closed) must not swallow the failure.
    console.error(error.stack || String(error));
    process.exitCode = 1;
    throw error;
  } finally {
    for (const tab of tabs) { try { tab.cdp.close(); } catch { /* gone */ } }
    await Promise.race([stopChrome(chrome), delay(15_000)]);
    await Promise.race([fixture.close(), delay(5_000)]);
  }
}

main().catch((error) => {
  console.error(error.stack || String(error));
  process.exitCode = 1;
});
