"use strict";

// terminal topbar gate: the terminal top bar, the quick-actions sheet opener and the
// composer, driven as the REAL bundled page in real Chromium against the
// reopen fixture (unified_reopen_fixture.cjs), on a fine pointer at desktop
// metrics and on a coarse pointer at iPhone metrics.
//
// Every case is independent and reports its own verdict: a run against a tree
// that carries none of terminal topbar fails each case separately, which is what makes a
// per-item causal-RED receipt readable from one log.
//
//   U1  the selection note is gone; Copy selection is an icon button whose
//       colour state carries "a selection is ready", with no layout shift
//   U2  no "Open legacy" control in the top bar
//   U3  the top bar carries a session tag with a status dot and a details
//       popover instead of the "Unified terminal (development)" caption
//   U4  the theme control is not in the primary row at any width; view disclosure moves
//       its single instance into View & appearance rather than Quick actions
//   U5  the sheet opener is a top-bar button on every pointer; the floating
//       puck does not exist
//   C1  the composer face is the composer_font_size preference
//   C2  the expanded composer stays inside the visible viewport above the
//       keyboard, the key bar and the toolbar
//
// PERSEA_TERMINAL_TOPBAR_EVIDENCE_DIR (optional): screenshots land there.

const fs = require("fs");
const path = require("path");
const { startFixture } = require("./unified_reopen_fixture.cjs");
const { assert, delay, requestJSON, Tab: BaseTab, launchChrome, stopChrome } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const EVIDENCE = process.env.PERSEA_TERMINAL_TOPBAR_EVIDENCE_DIR ? path.resolve(process.env.PERSEA_TERMINAL_TOPBAR_EVIDENCE_DIR) : null;
const DESKTOP = { width: 1024, height: 768, deviceScaleFactor: 1, mobile: false };
const PHONE = { width: 390, height: 844, deviceScaleFactor: 3, mobile: true };
// The same phone with the software keyboard up: iOS reports the reduced band
// through visualViewport, and Chromium's device-metrics override reduces both
// the layout and the visual viewport, which is the closest a headless run gets
// to a keyboard inset.
const PHONE_KEYBOARD = { width: 390, height: 508, deviceScaleFactor: 3, mobile: true };

const STATE = `(() => {
  const q = (s) => document.querySelector(s);
  const rect = (el) => { if (!el) return null; const r = el.getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height, cx: r.x + r.width / 2, cy: r.y + r.height / 2, bottom: r.bottom, top: r.top }; };
  const visible = (el) => !!el && !el.hidden && !el.closest("[hidden]") && getComputedStyle(el).display !== "none" && el.getClientRects().length > 0;
  const toolbar = q(".persea-unified-toolbar");
  const rows = q(".xterm-rows");
  const notice = q(".persea-unified-notice");
  const sheet = q(".persea-unified-sheet");
  const opener = q(".persea-unified-quick-actions");
  const copy = q(".persea-unified-select-context") || q(".persea-unified-copy") || null;
  const tag = q(".persea-unified-tag");
  const popover = q(".persea-unified-identity__details");
  const view = q(".persea-unified-view-disclosure");
  const viewPopover = q(".persea-unified-view-popover");
  const composerPanel = q(".attachment-page__composer");
  const composerTextarea = q(".attachment-page__composer-textarea");
  const composerActions = q(".attachment-page__composer-actions");
  const keyBar = q(".persea-unified-keybar");
  const sheetTheme = q(".persea-unified-sheet .persea-unified-preference");
  const composerFont = q(".persea-unified-preference__composer-font");
  return {
    href: location.href, ready: document.readyState,
    coarse: matchMedia("(pointer: coarse)").matches,
    screen: rows ? rows.textContent ?? "" : "",
    connection: q(".persea-unified-connection")?.textContent ?? "",
    noticeHidden: notice ? notice.hidden : true,
    toolbarRect: rect(toolbar),
    toolbarText: toolbar ? (toolbar.textContent ?? "") : "",
    toolbarButtons: toolbar ? Array.from(toolbar.querySelectorAll("button")).filter(visible).map((b) => ({
      cls: String(b.className || "").split(" ")[0],
      text: (b.textContent || "").trim(),
      label: b.getAttribute("aria-label") || "",
      title: b.getAttribute("title") || "",
      rect: rect(b),
    })) : [],
    // Every control the top bar can reach, popover included: a control that is
    // merely collapsed under the ⋯ toggle has not left the top bar.
    toolbarControls: toolbar ? Array.from(toolbar.querySelectorAll("button, select, a")).map((b) => ({
      cls: String(b.className || "").split(" ")[0],
      tag: b.tagName.toLowerCase(),
      text: (b.textContent || "").trim(),
      label: b.getAttribute("aria-label") || "",
      title: b.getAttribute("title") || "",
    })) : [],
    puckPresent: !!q(".persea-unified-puck"),
    openerPresent: !!opener,
    openerVisible: visible(opener),
    openerRect: rect(opener),
    openerExpanded: opener ? opener.getAttribute("aria-expanded") : null,
    openerControls: opener ? opener.getAttribute("aria-controls") : null,
    openerLabel: opener ? (opener.getAttribute("aria-label") || "") : null,
    sheetPresent: !!sheet,
    sheetId: sheet ? sheet.id : null,
    sheetVisible: visible(sheet),
    sheetRect: rect(sheet),
    selectionStatusPresent: !!q(".persea-unified-selection-status"),
    copy: copy ? {
      cls: String(copy.className || "").split(" ")[0],
      text: (copy.textContent || "").trim(),
      label: copy.getAttribute("aria-label") || "",
      title: copy.getAttribute("title") || "",
      disabled: copy.disabled === true,
      selection: copy.dataset ? (copy.dataset.selectState ?? copy.dataset.selection ?? null) : null,
      rect: rect(copy),
      background: getComputedStyle(copy).backgroundColor,
      borderColor: getComputedStyle(copy).borderTopColor,
      color: getComputedStyle(copy).color,
    } : null,
    // the Paste slot is the control that becomes Copy.
    paste: (() => {
      const paste = q(".persea-unified-toolbar-paste");
      return paste ? {
        text: (paste.textContent || "").trim(),
        label: paste.getAttribute("aria-label") || "",
        iconCount: paste.querySelectorAll("svg").length,
        disabled: paste.disabled === true,
        state: paste.dataset ? (paste.dataset.pasteState ?? null) : null,
        rect: rect(paste),
        background: getComputedStyle(paste).backgroundColor,
        borderColor: getComputedStyle(paste).borderTopColor,
      } : null;
    })(),
    copyStatusText: q(".persea-unified-copy-status")?.textContent ?? null,
    tag: tag ? {
      text: (tag.textContent || "").trim(),
      name: q(".persea-unified-tag__name")?.textContent ?? "",
      alias: (() => { const a = q(".persea-unified-tag__alias"); return a && !a.hidden ? (a.textContent ?? "") : null; })(),
      dotState: q(".persea-unified-tag__dot")?.dataset.state ?? null,
      dotBackground: (() => { const d = q(".persea-unified-tag__dot"); return d ? getComputedStyle(d).backgroundColor : null; })(),
      dotRect: rect(q(".persea-unified-tag__dot")),
      label: tag.getAttribute("aria-label") || "",
      title: tag.getAttribute("title") || "",
      expanded: tag.getAttribute("aria-expanded"),
      controls: tag.getAttribute("aria-controls"),
      rect: rect(tag),
      nameRect: rect(q(".persea-unified-tag__name")),
      // The rendered box, not the hidden attribute: below the phone breakpoint
      // the alias is dropped by the stylesheet, which leaves the attribute
      // alone and the accessible name intact.
      aliasRect: rect(q(".persea-unified-tag__alias")),
      // Whether the name is ellipsised right now, and whether it CAN be: the
      // second is the property that matters at a width no test can enumerate.
      nameTruncation: (() => {
        const n = q(".persea-unified-tag__name");
        if (!n) return null;
        const style = getComputedStyle(n);
        return {
          overflowing: n.scrollWidth > n.clientWidth + 0.5,
          overflow: style.overflow,
          textOverflow: style.textOverflow,
          whiteSpace: style.whiteSpace,
        };
      })(),
      wrapped: (() => { const r = tag.getClientRects(); return r.length > 1; })(),
    } : null,
    popover: popover ? {
      id: popover.id,
      visible: visible(popover),
      rows: Array.from(popover.querySelectorAll(".persea-unified-identity__row")).map((row) => ({
        term: row.querySelector("dt")?.textContent ?? "",
        value: row.querySelector("dd")?.textContent ?? "",
      })),
    } : null,
    themeSelectsInPrimaryRow: q(".persea-unified-toolbar__controls")?.querySelectorAll(".persea-unified-preference__theme").length ?? 0,
    viewPoint: rect(view) ? { x: rect(view).cx, y: rect(view).cy } : null,
    viewExpanded: view?.getAttribute("aria-expanded") === "true",
    viewPopoverVisible: visible(viewPopover),
    viewThemeRows: viewPopover ? Array.from(viewPopover.querySelectorAll(".persea-unified-preference")).map((label) => ({
      caption: label.querySelector(".persea-unified-preference__label")?.textContent ?? "",
      controlRect: rect(label.querySelector("select")),
      rect: rect(label),
    })) : [],
    sheetThemeJustify: sheetTheme ? getComputedStyle(sheetTheme).justifyContent : null,
    sheetPreferenceRows: sheet ? Array.from(sheet.querySelectorAll(".persea-unified-preference")).map((label) => ({
      cls: String(label.className || ""),
      justify: getComputedStyle(label).justifyContent,
      caption: label.querySelector(".persea-unified-preference__label")?.textContent ?? "",
      controlRect: rect(label.querySelector("select")),
      rect: rect(label),
    })) : [],
    composerFontControl: composerFont ? { value: composerFont.value, options: Array.from(composerFont.options).map((o) => o.value), rect: rect(composerFont) } : null,
    composer: {
      open: q(".persea-unified-composer-dock")?.dataset.composer === "open",
      size: composerPanel ? composerPanel.dataset.size ?? null : null,
      panelRect: rect(composerPanel),
      textareaRect: rect(composerTextarea),
      actionsRect: rect(composerActions),
      fontSize: composerTextarea ? parseFloat(getComputedStyle(composerTextarea).fontSize) : null,
      insetBudget: composerPanel ? composerPanel.style.getPropertyValue("--persea-composer-inset-budget") : "",
    },
    composerToggleRect: rect(q(".persea-unified-composer-toggle")),
    keyBarHidden: keyBar ? keyBar.hidden : true,
    keyBarRect: keyBar && !keyBar.hidden ? rect(keyBar) : null,
    shellRect: rect(q(".persea-unified-terminal")),
    hostRect: rect(q(".persea-unified-xterm")),
    rowsRect: rect(q(".xterm-rows")),
    visual: window.visualViewport ? { height: window.visualViewport.height, offsetTop: window.visualViewport.offsetTop, scale: window.visualViewport.scale } : null,
    innerHeight: window.innerHeight,
    theme: q(".persea-unified-terminal")?.dataset.theme ?? "",
    terminalRows: rows ? rows.children.length : 0,
    fontBaseline: q(".persea-unified-terminal")?.dataset.fontBaseline ?? null,
  };
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

  async tap(point) {
    const target = point && typeof point.cx === "number" ? { x: point.cx, y: point.cy } : point;
    assert(target && target.x > 0 && target.y > 0, `${this.name}: tap target is not rendered: ${JSON.stringify(point)}`);
    await super.tap(target);
  }

  async click(point) {
    const target = point && typeof point.cx === "number" ? { x: point.cx, y: point.cy } : point;
    assert(target && target.x > 0 && target.y > 0, `${this.name}: click target is not rendered: ${JSON.stringify(point)}`);
    await this.trustedClick(target);
  }

  // A real mouse drag across the rendered rows: xterm's own selection service
  // is what turns it into a selection, so the page's onSelectionChange fires
  // exactly as it does under an operator's hand.
  async dragSelect(from, to) {
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: from.x, y: from.y });
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: from.x, y: from.y, button: "left", buttons: 1, clickCount: 1 });
    for (let step = 1; step <= 6; step += 1) {
      await this.cdp.send("Input.dispatchMouseEvent", {
        type: "mouseMoved", button: "left", buttons: 1,
        x: from.x + ((to.x - from.x) * step) / 6,
        y: from.y + ((to.y - from.y) * step) / 6,
      });
      await delay(10);
    }
    await this.cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: to.x, y: to.y, button: "left", buttons: 0, clickCount: 1 });
    await delay(80);
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
}


// The software keyboard, as the only thing a headless browser cannot produce:
// it shortens the VISUAL viewport and leaves the layout viewport alone. Every
// device-metrics override shortens both, which is precisely NOT the state C2
// is about. The double wraps the real visualViewport — same object identity
// for listeners, same scale and offsets — and subtracts a settable inset from
// its height, then dispatches the resize the platform would. The page under
// test is the real bundled page reading its real inset source.
const KEYBOARD_DOUBLE = `(() => {
  const real = window.visualViewport;
  if (!real) return;
  let inset = 0;
  const view = {
    get width() { return real.width; },
    get height() { return Math.max(0, real.height - inset); },
    get offsetLeft() { return real.offsetLeft; },
    get offsetTop() { return real.offsetTop; },
    get pageLeft() { return real.pageLeft; },
    get pageTop() { return real.pageTop; },
    get scale() { return real.scale; },
    addEventListener: (type, listener, options) => real.addEventListener(type, listener, options),
    removeEventListener: (type, listener, options) => real.removeEventListener(type, listener, options),
    dispatchEvent: (event) => real.dispatchEvent(event),
  };
  Object.defineProperty(window, "visualViewport", { configurable: true, get: () => view });
  window.__perseaKeyboardInset = (px) => {
    inset = Math.max(0, Number(px) || 0);
    real.dispatchEvent(new Event("resize"));
    window.dispatchEvent(new Event("resize"));
    return view.height;
  };
})();`;

// The composer opens expanded with no grip target of its own: the state in
// which the panel's only height limit is the stylesheet's, which is what the
// operator met. Seeded through the product's own local preference record.
const COMPOSER_EXPANDED_SEED = `(() => {
  try {
    window.localStorage.setItem("persea-terminal.preferences.v1", JSON.stringify({
      version: 1,
      composer: {
        density: "standard", mode: "prose", panelSize: "expanded",
        heightPercent: { standard: { compact: null, expanded: null }, compact: { compact: null, expanded: null } },
      },
      keyBar: { collapsed: false },
    }));
  } catch { /* an opaque origin (about:blank) has no storage; the real document does */ }
})();`;

const isLive = (state) => state.screen.includes("fixture-live") && state.noticeHidden && state.connection === "";

async function main() {
  const fixture = await startFixture(UI);
  const origin = fixture.origin;
  const control = (input) => requestJSON(`${origin}/__fixture/control`, "POST", input);
  const freshControlHandle = async () => {
    const inventory = await requestJSON(`${origin}/api/inventory`);
    return inventory.realms[0].servers[0].sessions[0].handles.control;
  };
  const unifiedURL = (handle, extra = {}) => `${origin}/terminal?engine=unified-dev#${new URLSearchParams({
    handle, mode: "control", history: "1000", name: "alpha", draft_scope: fixture.draftScope, engine: "unified-dev", ...extra,
  }).toString()}`;

  const chrome = await launchChrome("persea-terminal-terminal_topbar-");
  const tabs = [];
  const cases = [];
  const evidence = {};
  const shots = [];
  // Each case runs to completion and records its own verdict. A thrown
  // precondition is a RED for that case only, never an abort of the run: the
  // whole point of this gate is a per-item receipt from one pass.
  const run = async (name, body) => {
    const failures = [];
    const fail = (message, detail) => failures.push(`${message} ${JSON.stringify(detail ?? null)}`);
    try {
      await body(fail);
    } catch (error) {
      failures.push(`case aborted: ${error && error.message ? error.message : String(error)}`);
    }
    cases.push({ name, ok: failures.length === 0, failures });
    return failures.length === 0;
  };
  const shot = async (tab, name) => { const file = await tab.screenshot(name); if (file) shots.push(file); };

  try {
    const tab = await Tab.open("A", chrome.debugPort, origin);
    tabs.push(tab);

    // --- desktop, fine pointer -------------------------------------------
    await tab.emulate(DESKTOP, false);
    await control({ reset: true });
    // The alias travels in the display-only fragment, exactly as the dashboard
    // sends it, so the tag's alias path is exercised rather than assumed.
    await tab.navigate(unifiedURL(await freshControlHandle(), { alias: "dev-alias" }));
    const fine = await tab.waitUntil(isLive, 10_000);
    assert(fine.state, `precondition: the desktop page did not reach live control: ${JSON.stringify(fine.last)}`);
    await delay(200);
    const desktop = await tab.state();
    evidence.desktop = { toolbarText: desktop.toolbarText, controls: desktop.toolbarControls.map((c) => c.cls || c.tag) };
    await shot(tab, "desktop-01-toolbar");

    await run("U2 no Open legacy control in the top bar", async (fail) => {
      const legacy = desktop.toolbarControls.filter((control) => /legacy/i.test(`${control.text} ${control.label} ${control.title}`));
      if (legacy.length !== 0) fail("the top bar still carries a legacy control", legacy);
      if (/open legacy/i.test(desktop.toolbarText)) fail("the top bar still renders the words Open legacy", { toolbarText: desktop.toolbarText });
    });

    await run("U1 a frozen selection turns the Paste slot into Copy without moving the toolbar", async (fail) => {
      if (desktop.selectionStatusPresent) fail("the injected selection-status element is still in the document", null);
      if (/selection ready to copy/i.test(desktop.toolbarText)) fail("the top bar still injects the selection note", { toolbarText: desktop.toolbarText });
      const copy = desktop.copy;
      if (!copy) { fail("no copy-selection control was found in the top bar", null); return; }
	  if (copy.cls !== "persea-unified-copy") fail("the contextual selection owner lost its stable toolbar class", copy);
	  if (copy.title !== "Select terminal text" || copy.label !== "Select terminal text") fail("the contextual owner does not name its idle Select action", copy);
	  if (copy.text !== "Select" || copy.disabled || copy.selection !== "select") fail("the contextual owner does not start in Select state", copy);
	  await tab.click(copy.rect);
	  await delay(100);
	  await tab.evaluate(`(() => {
	    const row = Array.from(document.querySelectorAll(".persea-unified-select__row")).find((entry) => (entry.textContent || "").trim() !== "");
	    if (!row) throw new Error("missing frozen row");
	    const range = document.createRange(); range.selectNodeContents(row);
	    const selection = window.getSelection(); selection.removeAllRanges(); selection.addRange(range);
	  })()`);
	  await delay(100);
      const selected = await tab.state();
      evidence.u1 = { before: { copy: desktop.copy, paste: desktop.paste, toolbar: desktop.toolbarRect }, after: { copy: selected.copy, paste: selected.paste, toolbar: selected.toolbarRect } };
      // Select stays a toggle (Selecting); the Paste slot becomes
      // Copy while the range exists.
      if (!desktop.paste || desktop.paste.state !== "paste" || desktop.paste.label !== "Open clipboard" || desktop.paste.iconCount !== 1) { fail("the Paste slot does not start as the accessible Clipboard action", evidence.u1); return; }
	  // the word stays "Select"; the pressed state carries the mode.
	  if (selected.copy.disabled || selected.copy.selection !== "selecting" || selected.copy.text !== "Select") {
		fail("a frozen selection did not leave the Select toggle in its pressed (selecting) state", evidence.u1);
        return;
      }
      if (!selected.paste || selected.paste.disabled || selected.paste.state !== "copy" || selected.paste.text !== "Copy") {
        fail("a frozen selection did not turn the Paste slot into Copy", evidence.u1);
        return;
      }
      if (selected.paste.background === desktop.paste.background && selected.paste.borderColor === desktop.paste.borderColor) {
        fail("the ready state is not colour-coded: the Paste slot paints identically with and without a selection", evidence.u1);
      }
      // No layout shift: the row's box and both buttons' boxes are identical.
      const same = (a, b) => Math.abs(a - b) < 0.5;
      if (!same(selected.toolbarRect.h, desktop.toolbarRect.h) || !same(selected.toolbarRect.w, desktop.toolbarRect.w)) {
        fail("the control row's box moved when the selection state changed", evidence.u1);
      }
      for (const [name, before, after] of [["Select", desktop.copy.rect, selected.copy.rect], ["Paste slot", desktop.paste.rect, selected.paste.rect]]) {
        if (!same(after.w, before.w) || !same(after.h, before.h) || !same(after.x, before.x) || !same(after.y, before.y)) {
          fail(`the ${name} box moved when the selection state changed`, evidence.u1);
        }
      }
      await shot(tab, "desktop-02-copy-ready");
	  await tab.cdp.send("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27, nativeVirtualKeyCode: 27 });
	  await tab.cdp.send("Input.dispatchKeyEvent", { type: "keyUp", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27, nativeVirtualKeyCode: 27 });
	  await delay(100);
    });

    await run("U3 the top bar carries a session tag, a status dot and a details popover", async (fail) => {
      if (/unified terminal \(development\)/i.test(desktop.toolbarText)) {
        fail("the top bar still renders the build caption", { toolbarText: desktop.toolbarText });
      }
      const tag = desktop.tag;
      if (!tag) { fail("the top bar has no session tag", null); return; }
      if (tag.name !== "alpha") fail("the tag does not name the attached session", tag);
      if (tag.alias !== "dev-alias") fail("the tag does not render the alias the fragment carried", tag);
      if (tag.wrapped) fail("the tag wrapped onto a second line", tag);
      // The dot is live and green: this page is attached and replayed.
      if (tag.dotState !== "live") fail("a live attachment does not read as attached on the status dot", tag);
      if (!/attached/i.test(tag.label)) fail("the tag's accessible name does not state the connection state in words", tag);
      // Colour is never the only carrier, so the state word must be in the
      // name; and the dot must actually be painted.
      if (!tag.dotRect || tag.dotRect.w < 4 || tag.dotRect.h < 4) fail("the status dot is not rendered", tag);
      // The details are one tap away and cost the row nothing.
      if (desktop.popover && desktop.popover.visible) fail("the identity popover is open before it is asked for", desktop.popover);
      await tab.click(tag.rect);
      await delay(120);
      const opened = await tab.state();
      evidence.u3 = { tag: opened.tag, popover: opened.popover, toolbarBefore: desktop.toolbarRect, toolbarAfter: opened.toolbarRect, hostBefore: desktop.hostRect, hostAfter: opened.hostRect };
      if (!opened.popover || !opened.popover.visible) { fail("tapping the tag did not open the identity popover", evidence.u3); return; }
      if (opened.tag.expanded !== "true") fail("the tag does not report its popover as expanded", evidence.u3);
      if (opened.tag.controls !== opened.popover.id) fail("the tag does not reference its popover", evidence.u3);
      const terms = opened.popover.rows.map((row) => row.term);
      for (const wanted of ["Status", "Server", "Session", "Session id", "Size"]) {
        if (!terms.includes(wanted)) fail(`the identity popover does not list ${wanted}`, evidence.u3);
      }
      const size = opened.popover.rows.find((row) => row.term === "Size");
      if (size && !/^\d+\u00d7\d+$/.test(size.value)) fail("the popover's size row is not the committed geometry", evidence.u3);
      // Zero-RESIZE: the popover is positioned, so neither the control row nor
      // the terminal mount box moved when it opened.
      const same = (a, b) => Math.abs(a - b) < 0.5;
      if (!same(opened.toolbarRect.h, desktop.toolbarRect.h)) fail("opening the popover changed the control row's height", evidence.u3);
      if (!same(opened.hostRect.h, desktop.hostRect.h) || !same(opened.hostRect.w, desktop.hostRect.w)) {
        fail("opening the popover moved the terminal mount box", evidence.u3);
      }
      await shot(tab, "desktop-03-identity-popover");
      await tab.click(opened.tag.rect);
      await delay(120);
      const closed = await tab.state();
      if (closed.popover && closed.popover.visible) fail("a second tap on the tag did not close the popover", closed.popover);
      // The phone. This row is the ONLY place a phone says which session is on
      // screen, so the tag stays below the breakpoint --
      // and the arithmetic that used to be the argument for hiding it becomes
      // the thing measured: one line, a dot at full size, a name that can
      // ellipsise, no alias, and nothing outside 390 pt.
      await tab.emulate(PHONE, true);
      await delay(250);
      const phoneTag = await tab.state();
      evidence.u3narrow = {
        tag: phoneTag.tag,
        buttons: phoneTag.toolbarButtons.map((b) => ({ cls: b.cls, x: b.rect.x, w: b.rect.w, top: b.rect.top, bottom: b.rect.bottom })),
        toolbar: phoneTag.toolbarRect,
        innerWidth: 390,
      };
      if (!phoneTag.tag || !(phoneTag.tag.rect.w > 0)) {
        fail("the session tag is not rendered at 390pt", evidence.u3narrow);
      } else {
        const t = phoneTag.tag;
        if (t.wrapped) fail("the tag wrapped onto a second line at 390pt", evidence.u3narrow);
        if (!(t.dotRect.w >= 4)) fail("the status dot lost its size at 390pt", evidence.u3narrow);
        if (t.name !== "alpha") fail("the tag stopped naming the session at 390pt", evidence.u3narrow);
        if (t.aliasRect.w > 0.5) fail("the alias still claims row width at 390pt", evidence.u3narrow);
        // Dropping it from the row must not drop it from the spoken name.
        if (!t.label.includes("dev-alias")) fail("the narrow tag lost the alias from its accessible name", evidence.u3narrow);
        if (!(t.rect.w >= 43.5)) fail("the tag is below the 44px touch target at 390pt", evidence.u3narrow);
        // The name must be able to give way at a width no case can enumerate.
        if (!t.nameTruncation || t.nameTruncation.overflow === "visible"
          || t.nameTruncation.textOverflow !== "ellipsis" || t.nameTruncation.whiteSpace !== "nowrap") {
          fail("the tag's name cannot truncate at a narrow width", evidence.u3narrow);
        }
      }
      const offscreen = phoneTag.toolbarButtons.filter((b) => b.rect.x < -0.5 || b.rect.x + b.rect.w > 390.5);
      if (offscreen.length !== 0) fail("a top-bar control is outside the 390pt viewport", { offscreen, ...evidence.u3narrow });
      // One line, measured: every rendered control shares a vertical band, so
      // nothing was pushed to a second row -- which would move the terminal
      // mount box and break the zero-RESIZE contract.
      const phoneBoxes = phoneTag.toolbarButtons.filter((b) => b.rect.w > 0 && b.rect.h > 0);
      if (phoneBoxes.length < 4) fail("the phone row rendered fewer controls than expected", evidence.u3narrow);
      else {
        const lowestTop = Math.max(...phoneBoxes.map((b) => b.rect.top));
        const highestBottom = Math.min(...phoneBoxes.map((b) => b.rect.bottom));
        if (!(lowestTop < highestBottom)) {
          fail("the control row wrapped to a second line at 390pt", { lowestTop, highestBottom, ...evidence.u3narrow });
        }
      }
      await shot(tab, "phone-04-session-tag");
      // terminal interaction permanently reserves the compact phone row for the tag and its
      // five 44px controls. Even clearing the presentational hidden flag must
      // not resurrect the retired prose hint into that fixed budget.
      await tab.evaluate(`(() => {
        const hint = document.querySelector(".persea-unified-fit-hint");
        if (!hint) return false;
        hint.hidden = false;
        return true;
      })()`);
      await delay(250);
      const phoneHint = await tab.state();
      evidence.u3narrowHint = {
        tag: phoneHint.tag,
        buttons: phoneHint.toolbarButtons.map((b) => ({ cls: b.cls, x: b.rect.x, w: b.rect.w, top: b.rect.top, bottom: b.rect.bottom })),
        toolbar: phoneHint.toolbarRect,
      };
      const hintBox = phoneHint.toolbarButtons.find((b) => b.cls.includes("fit-hint"));
      if (hintBox && hintBox.rect.w > 0) fail("the retired prose fit hint consumed compact phone-row space", evidence.u3narrowHint);
      const hintOffscreen = phoneHint.toolbarButtons.filter((b) => b.rect.w > 0 && (b.rect.x < -0.5 || b.rect.x + b.rect.w > 390.5));
      if (hintOffscreen.length !== 0) fail("a compact control left the 390pt viewport", { offscreen: hintOffscreen, ...evidence.u3narrowHint });
      if (!phoneHint.tag || !(phoneHint.tag.rect.w >= 43.5)) fail("the tag gave up its touch target after the hidden-flag probe", evidence.u3narrowHint);
      if (phoneHint.tag && phoneHint.tag.wrapped) fail("the tag wrapped after the hidden-flag probe", evidence.u3narrowHint);
      if (!(phoneHint.tag.dotRect.w >= 4)) fail("the status dot vanished after the hidden-flag probe", evidence.u3narrowHint);
      if (Math.abs(phoneHint.toolbarRect.h - phoneTag.toolbarRect.h) > 0.5) {
        fail("the hidden-flag probe changed the compact row height", { with: phoneHint.toolbarRect, without: phoneTag.toolbarRect });
      }
      await shot(tab, "phone-05-session-tag-no-prose-hint");
      await tab.evaluate(`(() => {
        const hint = document.querySelector(".persea-unified-fit-hint");
        if (hint) hint.hidden = true;
        return true;
      })()`);
      await tab.emulate(DESKTOP, false);
      await delay(200);
    });

    await run("U4 theme lives on the dashboard: never in the primary row, Quick actions, or the View popover (terminal appearance §15.6)", async (fail) => {
      if (desktop.themeSelectsInPrimaryRow !== 0) fail("the primary row still carries a theme control", { count: desktop.themeSelectsInPrimaryRow });
      await tab.emulate({ width: 420, height: 768, deviceScaleFactor: 1, mobile: false }, false);
      await delay(200);
      const narrow = await tab.state();
      if (narrow.themeSelectsInPrimaryRow !== 0) fail("the narrow primary row carries a theme control", { count: narrow.themeSelectsInPrimaryRow });
      await tab.emulate(DESKTOP, false);
      await delay(200);
      const viewState = await tab.state();
      if (!viewState.viewPoint) { fail("View & appearance is not on the top bar", viewState); return; }
      await tab.click(viewState.viewPoint);
      await delay(200);
      const opened = await tab.state();
      const themeRows = opened.viewThemeRows.filter((row) => row.caption === "Theme");
      evidence.u4 = { viewExpanded: opened.viewExpanded, viewPopoverVisible: opened.viewPopoverVisible, themeRows };
      if (!opened.viewExpanded || !opened.viewPopoverVisible || themeRows.length !== 0) fail("the View popover still renders a theme control (it lives on the dashboard)", evidence.u4);
      await shot(tab, "desktop-04-view-appearance");
      await tab.click(viewState.viewPoint);
      await delay(150);
    });

    await run("U5 one sheet opener in the top bar on every pointer, no puck", async (fail) => {
      // Fine pointer: the opener is in the control row and the puck is gone
      // from the document, not merely hidden.
      if (desktop.puckPresent) fail("the floating puck is still in the document on a fine pointer", null);
      if (!desktop.openerVisible) fail("a fine pointer has no sheet opener in the control row", desktop);
      if (desktop.openerControls !== desktop.sheetId) fail("the opener does not reference the sheet it opens", { controls: desktop.openerControls, sheet: desktop.sheetId });
      if (!desktop.toolbarRect || !desktop.openerRect
        || desktop.openerRect.x < desktop.toolbarRect.x - 0.5
        || desktop.openerRect.bottom > desktop.toolbarRect.bottom + 0.5) {
        fail("the opener is not inside the control row", { opener: desktop.openerRect, toolbar: desktop.toolbarRect });
      }
      // Coarse pointer at phone metrics: the same control, in the same place,
      // at a 44px target, and still no puck.
      await tab.emulate(PHONE, true);
      await control({ reset: true });
      await tab.navigate(unifiedURL(await freshControlHandle()));
      const live = await tab.waitUntil(isLive, 10_000);
      assert(live.state, `U5 precondition: the phone page did not reach live control: ${JSON.stringify(live.last)}`);
      await delay(250);
      const phone = await tab.state();
      evidence.u5 = { coarse: phone.coarse, puckPresent: phone.puckPresent, opener: phone.openerRect, toolbar: phone.toolbarRect, host: phone.hostRect };
      if (!phone.coarse) { fail("the coarse-pointer emulation did not take", evidence.u5); return; }
      if (phone.puckPresent) fail("the floating puck is still in the document on a coarse pointer", evidence.u5);
      if (!phone.openerVisible) { fail("a coarse pointer has no sheet opener in the control row", evidence.u5); return; }
      if (phone.openerRect.w < 43.5 || phone.openerRect.h < 43.5) fail("the coarse sheet opener is below the 44px target law", evidence.u5);
      if (phone.openerRect.bottom > phone.toolbarRect.bottom + 0.5) fail("the coarse opener is not inside the control row", evidence.u5);
      await shot(tab, "phone-01-toolbar-opener");
      // It opens the same sheet, and opening it moves no measured box: the
      // sheet is an overlay on the stage, not a grid row.
      await tab.tap(phone.openerRect);
      await delay(220);
      const opened = await tab.state();
      evidence.u5.opened = { sheetVisible: opened.sheetVisible, expanded: opened.openerExpanded, host: opened.hostRect, sheet: opened.sheetRect };
      if (!opened.sheetVisible || opened.openerExpanded !== "true") { fail("the top-bar opener did not open the sheet on a coarse pointer", evidence.u5); return; }
      const same = (a, b) => Math.abs(a - b) < 0.5;
      if (!same(opened.hostRect.h, phone.hostRect.h) || !same(opened.hostRect.w, phone.hostRect.w)) {
        fail("opening the sheet moved the terminal mount box", evidence.u5);
      }
      // The opener is above the sheet it opens, so it can never cover a tile.
      if (opened.openerRect.bottom > opened.sheetRect.y + 0.5) fail("the opener overlaps the sheet it opens", evidence.u5);
      await shot(tab, "phone-02-sheet-open");
      await tab.tap(opened.openerRect);
      await delay(200);
      const reclosed = await tab.state();
      if (reclosed.sheetVisible) fail("the same control did not close the sheet", { expanded: reclosed.openerExpanded });
    });

    await run("C1 the composer face is the composer_font_size preference", async (fail) => {
      await tab.emulate(DESKTOP, false);
      await control({ reset: true });
      await tab.navigate(unifiedURL(await freshControlHandle()));
      const ready = await tab.waitUntil(isLive, 10_000);
      assert(ready.state, `C1 precondition: the page did not reach live control: ${JSON.stringify(ready.last)}`);
      await delay(250);
      // Open the composer so the face is measured on a rendered box.
      const before = await tab.state();
      await tab.click(before.composerToggleRect);
      await delay(300);
      const opened = await tab.state();
      if (!opened.composer.open) { fail("the composer did not open", opened.composer); return; }
      evidence.c1 = { defaultFace: opened.composer.fontSize };
      // The installed default is the operator's compact 11px face. An
      // explicit stored preference may still choose any value in 9…24.
      if (opened.composer.fontSize !== 11) fail("the default composer face is not 11px on a fine pointer", evidence.c1);
      // the control lives on the dashboard's Settings · Appearance
      // card, never in the sheet; the face reaches this page as a record.
      await tab.click(opened.openerRect);
      await delay(220);
      const sheet = await tab.state();
      if (!sheet.sheetVisible) { fail("the sheet did not open", sheet); return; }
      if (sheet.composerFontControl || sheet.sheetPreferenceRows.length !== 0) {
        fail("the sheet still carries a preference control (terminal appearance §15.6)", sheet.sheetPreferenceRows);
        return;
      }
      await tab.click(sheet.openerRect);
      await delay(200);
      // A record stored elsewhere (the dashboard card, another tab), told to
      // this page the way the product tells it: the `storage` event of the
      // preference hint key, which triggers one authoritative read (14.3b).
      const publishRecord = async (patch) => {
        const current = await requestJSON(`${origin}/__fixture/control`);
        await control({ preferences: { ...patch, revision: current.preferences.revision + 1, stored: true } });
        const signalled = await tab.evaluate(`(() => {
          window.dispatchEvent(new StorageEvent("storage", { key: "persea-terminal.operator-preferences-hint.v1", newValue: "{}" }));
          return true;
        })()`);
        if (!signalled) throw new Error("C1: the storage signal could not be dispatched");
      };
      const putsBefore = (await requestJSON(`${origin}/__fixture/control`)).counters.preferencesPut;
      await publishRecord({ composer_font_size: 18 });
      const chosen = await tab.waitUntil((state) => state.composer.fontSize === 18, 4_000);
      evidence.c1.chosen = (chosen.state ?? chosen.last).composer;
      if (!chosen.state) { fail("the stored face did not reach the composer", evidence.c1); return; }
      const stored = await requestJSON(`${origin}/__fixture/control`);
      evidence.c1.stored = { record: stored.preferences.composer_font_size, puts: stored.counters.preferencesPut - putsBefore };
      if (stored.counters.preferencesPut !== putsBefore) fail("following a stored face issued a write from the terminal", evidence.c1);
      await shot(tab, "desktop-05-composer-face");
      // no coarse floor — the viewport meta suppresses the iOS
      // focus zoom, so a stored 11 is 11 on a phone and on a desktop alike.
      await tab.emulate(PHONE, true);
      await delay(300);
      const coarseLarge = await tab.state();
      evidence.c1.coarseAt18 = coarseLarge.composer.fontSize;
      if (coarseLarge.composer.fontSize !== 18) fail("the stored face did not apply on a phone", evidence.c1);
      await publishRecord({ composer_font_size: 11 });
      const small = await tab.waitUntil((state) => state.composer.fontSize === 11, 4_000);
      evidence.c1.coarseAt11 = (small.state ?? small.last).composer.fontSize;
      if (!small.state) fail("a stored 11px face was floored or ignored on a coarse pointer (terminal appearance §15.5)", evidence.c1);
      // page zoom is never capped; on a coarse pointer the textarea
      // wears a 16px face for the instant of focus (iOS decides its focus zoom
      // then) and is back at the stored 11px once focus has settled.
      await tab.evaluate(`(() => {
        window.__composerFocusFace = null;
        document.addEventListener("focus", (event) => {
          const target = event.target;
          if (target && target.classList && target.classList.contains("attachment-page__composer-textarea")) window.__composerFocusFace = getComputedStyle(target).fontSize;
        }, true);
        const textarea = document.querySelector(".attachment-page__composer-textarea");
        textarea.blur();
        return true;
      })()`);
      await delay(150);
      const textareaPoint = await tab.evaluate(`(() => {
        const box = document.querySelector(".attachment-page__composer-textarea").getBoundingClientRect();
        return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
      })()`);
      await tab.tap(textareaPoint);
      await delay(400);
      const focusGuard = await tab.evaluate(`(() => {
        const textarea = document.querySelector(".attachment-page__composer-textarea");
        return { atFocus: window.__composerFocusFace, settled: getComputedStyle(textarea).fontSize, focused: document.activeElement === textarea, inline: textarea.style.fontSize };
      })()`);
      evidence.c1.focusGuard = focusGuard;
      if (!focusGuard.focused) fail("the tap did not focus the composer on the phone", focusGuard);
      else if (focusGuard.atFocus !== "16px") fail("the composer took focus under 16px on a coarse pointer (iOS focus zoom)", focusGuard);
      else if (focusGuard.settled !== "11px" || focusGuard.inline !== "") fail("the focus-time face did not return to the stored 11px", focusGuard);
      await tab.emulate(DESKTOP, false);
      await delay(250);
      const fineAt11 = await tab.waitUntil((state) => state.composer.fontSize === 11, 4_000);
      evidence.c1.fineAt11 = (fineAt11.state ?? fineAt11.last).composer.fontSize;
      if (!fineAt11.state) fail("the same 11px face did not apply on a fine pointer", evidence.c1);
    });

    await run("C2 the expanded composer stays inside the visible viewport", async (fail) => {
      await tab.cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: `${COMPOSER_EXPANDED_SEED}\n${KEYBOARD_DOUBLE}` });
      await tab.emulate(PHONE, true);
      await control({ reset: true });
      await tab.navigate(unifiedURL(await freshControlHandle()));
      const ready = await tab.waitUntil(isLive, 10_000);
      assert(ready.state, `C2 precondition: the phone page did not reach live control: ${JSON.stringify(ready.last)}`);
      await delay(300);
      const start = await tab.state();
      if (start.visual === null || Math.round(start.visual.height) !== 844) {
        fail("the keyboard double did not install cleanly", { visual: start.visual });
        return;
      }
      // Open the composer and give it more draft than any viewport can hold.
      await tab.tap(start.composerToggleRect);
      await delay(300);
      const filled = await tab.evaluate(`(() => {
        const textarea = document.querySelector(".attachment-page__composer-textarea");
        if (!textarea) return null;
        textarea.focus();
        textarea.value = Array.from({ length: 60 }, (unused, index) => "draft line " + index).join("\\n");
        textarea.dispatchEvent(new Event("input", { bubbles: true }));
        return textarea.value.length;
      })()`);
      if (!filled) { fail("the composer textarea could not be filled", null); return; }
      await delay(350);
      const beforeKeyboard = await tab.state();
      if (!beforeKeyboard.composer.open) { fail("the composer did not open", beforeKeyboard.composer); return; }
      if (beforeKeyboard.composer.size !== "expanded") {
        fail("the seeded record did not open the composer expanded", beforeKeyboard.composer);
        return;
      }
      // The keyboard arrives: the visible band drops to 508 of a layout
      // viewport that is still 844.
      const band = await tab.evaluate("window.__perseaKeyboardInset(336)");
      if (Math.round(band) !== 508) { fail("the keyboard double did not shorten the visual viewport", { band }); return; }
      await delay(500);
      const raised = await tab.state();
      evidence.c2 = {
        band: raised.visual, panel: raised.composer.panelRect, actions: raised.composer.actionsRect,
        keyBar: raised.keyBarRect, shell: raised.shellRect, insetBudget: raised.composer.insetBudget,
        size: raised.composer.size, host: raised.hostRect,
      };
      await shot(tab, "phone-03-composer-expanded-keyboard");
      const visibleBottom = raised.visual.offsetTop + raised.visual.height;
      const fits = (name, box) => {
        if (!box) return;
        if (box.bottom > visibleBottom + 0.5) fail(`${name} extends below the visible viewport`, { name, box, visibleBottom, ...evidence.c2 });
      };
      fits("the composer panel", raised.composer.panelRect);
      // The controls are the point: a panel whose bottom edge is off screen
      // takes ⊕ ⌫ ➤ with it.
      fits("the composer's action row", raised.composer.actionsRect);
      fits("the key bar", raised.keyBarRect);
      // And the terminal keeps a usable band rather than being crushed to zero.
      if (raised.hostRect && raised.hostRect.h < 4) fail("the composer took the whole terminal", evidence.c2);

      // Recomputation, not a one-off: a second, different band must re-cap.
      const narrower = await tab.evaluate("window.__perseaKeyboardInset(430)");
      if (Math.round(narrower) !== 414) { fail("the second keyboard band did not apply", { narrower }); return; }
      await delay(500);
      const shorter = await tab.state();
      evidence.c2.narrower = { band: shorter.visual, panel: shorter.composer.panelRect, actions: shorter.composer.actionsRect, keyBar: shorter.keyBarRect };
      const shorterBottom = shorter.visual.offsetTop + shorter.visual.height;
      for (const [name, box] of [["the composer panel", shorter.composer.panelRect], ["the composer's action row", shorter.composer.actionsRect], ["the key bar", shorter.keyBarRect]]) {
        if (box && box.bottom > shorterBottom + 0.5) fail(`${name} did not follow the viewport down`, { name, box, shorterBottom, ...evidence.c2 });
      }
      // The band going back up releases the cap rather than pinning the panel
      // small for the rest of the session.
      await tab.evaluate("window.__perseaKeyboardInset(0)");
      await delay(500);
      const released = await tab.state();
      evidence.c2.released = { band: released.visual, panel: released.composer.panelRect };
      if (released.composer.panelRect.h <= shorter.composer.panelRect.h) {
        fail("the composer stayed capped after the keyboard went away", evidence.c2);
      }
    });

  } finally {
    for (const tab of tabs) await tab.close(chrome.debugPort);
    await stopChrome(chrome);
    await Promise.race([fixture.close(), delay(5_000)]);
  }

  const failed = cases.filter((entry) => !entry.ok);
  const report = { cases: cases.map((entry) => ({ name: entry.name, verdict: entry.ok ? "GREEN" : "RED", failures: entry.failures })), evidence, shots };
  if (EVIDENCE) {
    fs.mkdirSync(EVIDENCE, { recursive: true });
    fs.writeFileSync(path.join(EVIDENCE, "terminal_topbar-topbar.json"), `${JSON.stringify(report, null, 2)}\n`);
  }
  for (const entry of cases) {
    process.stdout.write(`${entry.ok ? "GREEN" : "RED  "}  ${entry.name}\n`);
    for (const failure of entry.failures) process.stdout.write(`         ${failure}\n`);
  }
  process.stdout.write(`\nterminal_topbar top-bar gate: ${cases.length - failed.length}/${cases.length} green\n`);
  if (failed.length !== 0) {
    process.exitCode = 1;
    return;
  }
}

main().catch((error) => {
  process.stderr.write(`${error && error.stack ? error.stack : String(error)}\n`);
  process.exitCode = 1;
});
