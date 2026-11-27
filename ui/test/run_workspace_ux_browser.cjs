"use strict";

// Focused browser regression suite for Workspaces & Dashboard UX upgrades:
// 1. Divider nudge/drag never erases staged editor drafts; diverged live seams lock.
// 2. Normalized-name 409 collision never retargets this.record or reconciles another workspace.
// 3. Locked dividers outside Edit mode are presentation-only with no grip or Tab stop.
// 4. Presentation-only Maximize + Restore + header pane picker + capture Alt+Shift+F6
//    (zero INPUT leaked to xterm, zero RESIZE_REQUEST, zero inline [style] on workspace chrome,
//     hidden sibling output preserved on Restore).
// 5. Focused durable Landing card (member leaves only, mini diagram, theme parity including Dawn).
// 6. Dashboard multi-session builder + presets + passive availability updates + Duplicate +
//    left-aligned mini previews + scoped nav Left/Right focus + phone hides Duplicate.

const assert = require("node:assert/strict");
const http = require("node:http");
const https = require("node:https");
const path = require("node:path");
const playwright = require("playwright");
const chromiumPath = require("./browser_path.cjs");
const { startWorkspaceFixture } = require("./workspace_fixture.cjs");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_WORKSPACE_UX_ENGINE || "chromium";
if (ENGINE !== "chromium" && ENGINE !== "webkit") {
  throw new Error(`Unsupported PERSEA_WORKSPACE_UX_ENGINE: ${ENGINE}`);
}

function leaf(name) {
  return { kind: "leaf", session: { realm: "local", server: "private", name }, on_missing: "offer" };
}

function requestJSON(url, method, body) {
  return new Promise((resolve, reject) => {
    const parsed = new URL(url);
    const mod = parsed.protocol === "https:" ? https : http;
    const payload = body === undefined ? undefined : Buffer.from(JSON.stringify(body));
    const req = mod.request(parsed, {
      method,
      rejectUnauthorized: false,
      headers: payload ? { "Content-Type": "application/json", "Content-Length": payload.length } : {},
    }, (res) => {
      const chunks = [];
      res.on("data", (chunk) => chunks.push(chunk));
      res.on("end", () => {
        const text = Buffer.concat(chunks).toString("utf8");
        if (res.statusCode !== 200) {
          reject(new Error(`fixture control ${url} returned ${res.statusCode}: ${text}`));
          return;
        }
        resolve(JSON.parse(text));
      });
    });
    req.on("error", reject);
    if (payload) req.write(payload);
    req.end();
  });
}

const postJSON = (url, body) => requestJSON(url, "POST", body);
const getJSON = (url) => requestJSON(url, "GET");

async function main() {
  const useTLS = ENGINE === "webkit";
  const fixture = await startWorkspaceFixture(UI, { tls: useTLS });
  const browser = ENGINE === "webkit"
    ? await playwright.webkit.launch({ headless: true })
    : await playwright.chromium.launch({
      executablePath: chromiumPath(),
      headless: true,
      args: ["--no-sandbox"],
    });
  const contextOptions = (extra = {}) => ({ ...(useTLS ? { ignoreHTTPSErrors: true } : {}), ...extra });
  const control = (body) => postJSON(`${fixture.origin}/__fixture/control`, body);
  const snap = () => getJSON(`${fixture.origin}/__fixture/control`);

  try {
    // --- 1 & 2 & 3 & 4 & 5: Workspace runtime UX, editor safety, zoom, capture shortcut, landing & theme ---
    {
      await control({
        reset: true,
        sessions: ["ws01", "ws02", "ws03", "ws04"],
        preferences: { theme: "rose-pine-dawn", revision: 1, stored: true, available: true },
        workspace: {
          name: "Ops",
          tree: { kind: "split", direction: "row", weights: [50, 50], children: [leaf("ws01"), leaf("ws02")] },
        },
      });
      await control({
        workspace: {
          name: "Other",
          tree: { kind: "split", direction: "row", weights: [50, 50], children: [leaf("ws03"), leaf("ws04")] },
        },
      });
      const initialSnap = await snap();
      const opsRecord = initialSnap.workspaces.find((w) => w.name === "Ops");
      assert.ok(opsRecord, "Ops record must exist");

      const context = await browser.newContext(contextOptions({ viewport: { width: 1280, height: 800 } }));
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
      page.on("console", (m) => { if (m.type() === "error" && !/409 \(Conflict\)/.test(m.text())) errors.push(`console: ${m.text()}`); });

      await page.goto(`${fixture.origin}/workspace?engine=unified-dev#name=Ops`);
      await page.locator(".ws-landing__open").waitFor();

      // 5. Focused durable landing: ONLY the 2 member panes (not ws03/ws04), mini diagram, Dawn theme, zero inline styles
      const landingCheck = await page.evaluate(() => ({
        theme: document.body.dataset.wsTheme,
        colorScheme: getComputedStyle(document.body).colorScheme,
        hasDiagram: !!document.querySelector(".ws-landing__diagram .ws-mini"),
        hasFilter: !!document.querySelector(".ws-landing__filter"),
        sessions: [...document.querySelectorAll(".ws-landing__session")].map((n) => n.dataset.wsSession),
        boxes: [...document.querySelectorAll(".ws-landing__session-box")].map((b) => ({ checked: b.checked, disabled: b.disabled })),
        notice: document.querySelector(".ws-landing__notice")?.textContent ?? "",
        stateTexts: [...document.querySelectorAll(".ws-landing__session-state")].map((n) => n.textContent),
        inlineStyles: document.querySelectorAll(".ws-landing [style]").length,
      }));
      assert.equal(landingCheck.theme, "rose-pine-dawn", "landing must apply saved Dawn theme");
      assert.equal(landingCheck.colorScheme, "light", "Dawn theme must set light color-scheme");
      assert.equal(landingCheck.hasDiagram, true, "durable landing must show miniature layout diagram");
      assert.equal(landingCheck.hasFilter, false, "durable landing must omit dead filter input");
      assert.deepEqual(landingCheck.sessions, ["ws01", "ws02"], "durable landing must list only member panes");
      assert.equal(landingCheck.boxes.length, 2);
      assert.ok(landingCheck.boxes.every((b) => b.checked && b.disabled));
      assert.ok(landingCheck.stateTexts.every((t) => t === "ready to open"), `unexpected state copy: ${JSON.stringify(landingCheck.stateTexts)}`);
      assert.equal(landingCheck.inlineStyles, 0, "landing must use zero inline style attributes");

      // Open workspace via trusted click
      await page.locator(".ws-landing__open").click();
      await page.waitForFunction(() => document.querySelectorAll('.ws-cell[data-ws-state="live"] .xterm-rows').length === 2);

      // 3. Locked dividers outside Edit mode
      const lockedDivider = await page.evaluate(() => {
        const d = document.querySelector(".ws-divider");
        const grip = d?.querySelector(".ws-divider__grip");
        return {
          editing: document.querySelector(".ws-root")?.dataset.wsEditing,
          role: d?.getAttribute("role"),
          disabled: d?.dataset.wsDisabled,
          tabIndex: d?.tabIndex,
          gripDisplay: grip ? getComputedStyle(grip).display : null,
          cursor: d ? getComputedStyle(d).cursor : null,
        };
      });
      assert.equal(lockedDivider.editing, "false");
      assert.equal(lockedDivider.role, "presentation");
      assert.equal(lockedDivider.disabled, "true");
      assert.equal(lockedDivider.tabIndex, -1);
      assert.equal(lockedDivider.gripDisplay, "none");
      assert.equal(lockedDivider.cursor, "default");

      // 4. Capture shortcut Alt+Shift+F6 from active xterm -> zero INPUT leaked, switches panes & maximizes cleanly
      await page.locator('.ws-cell[data-ws-session="ws01"] .xterm-screen').click();
      const beforeInputs = await snap();
      const countInput = (s, name) => s.attachments.filter((a) => a.session === name).reduce((acc, a) => acc + a.inputs.length, 0);
      const ws01In0 = countInput(beforeInputs, "ws01");
      const ws02In0 = countInput(beforeInputs, "ws02");

      await page.keyboard.press("Alt+Shift+F6");
      const inPicker = await page.evaluate(() => document.activeElement?.classList.contains("ws-pane-pill") && document.activeElement?.dataset.wsSession);
      assert.equal(inPicker, "ws01", "Alt+Shift+F6 must focus active pane pill");
      await page.waitForTimeout(80);
      const afterShortcut = await snap();
      assert.equal(countInput(afterShortcut, "ws01"), ws01In0, "Alt+Shift+F6 must send zero INPUT to ws01");
      assert.equal(countInput(afterShortcut, "ws02"), ws02In0, "Alt+Shift+F6 must send zero INPUT to ws02");

      // ArrowRight + Enter switches to ws02; typing goes only to ws02; unreserved Alt+Shift+ArrowLeft still reaches xterm
      await page.keyboard.press("ArrowRight");
      await page.keyboard.press("Enter");
      const focusedAfterPick = await page.evaluate(() => document.activeElement?.closest(".ws-cell")?.dataset.wsSession);
      assert.equal(focusedAfterPick, "ws02", "ArrowRight+Enter in pane picker must focus ws02");
      await page.keyboard.type("hello-ws02");
      await page.keyboard.press("Alt+Shift+ArrowLeft");
      await page.waitForTimeout(120);
      const afterTyping = await snap();
      assert.equal(countInput(afterTyping, "ws01"), ws01In0, "typing in ws02 must not reach ws01");
      const ws02Combined = afterTyping.attachments.filter((a) => a.session === "ws02").flatMap((a) => a.inputs).join("");
      assert.ok(ws02Combined.includes("hello-ws02"), `ws02 did not receive typed text: ${JSON.stringify(ws02Combined)}`);
      assert.ok(ws02Combined.includes("\x1b[1;4D"), "unreserved Alt+Shift+ArrowLeft must still reach terminal");

      // Maximize ws02 -> ws01 hidden, zero RESIZE_REQUEST, zero workspace chrome [style]
      const beforeZoomAtts = afterTyping.attachments.filter((a) => a.live).map((a) => [a.session, a.id]);
      await page.locator(".ws-workspace-zoom").click();
      const zoomedState = await page.evaluate(() => ({
        rootZoomed: document.querySelector(".ws-root")?.dataset.wsZoomed,
        btnText: document.querySelector(".ws-workspace-zoom")?.textContent,
        btnPressed: document.querySelector(".ws-workspace-zoom")?.getAttribute("aria-pressed"),
        ws01Display: getComputedStyle(document.querySelector('.ws-cell[data-ws-session="ws01"]')).display,
        ws02Display: getComputedStyle(document.querySelector('.ws-cell[data-ws-session="ws02"]')).display,
        wsChromeStyles: [...document.querySelectorAll(".ws-workspace-header, .ws-workspace-header *, .ws-root > .ws-split, .ws-divider, .ws-cell, .ws-cell__header")].filter((n) => n.hasAttribute("style")).length,
      }));
      assert.equal(zoomedState.rootZoomed, "true");
      assert.equal(zoomedState.btnText, "Restore grid");
      assert.equal(zoomedState.btnPressed, "true");
      assert.equal(zoomedState.ws01Display, "none");
      assert.notEqual(zoomedState.ws02Display, "none");
      assert.equal(zoomedState.wsChromeStyles, 0);

      // Push output to hidden ws01 while zoomed on ws02, then Restore grid
      await control({ session: "ws01", writeLive: "HIDDEN_SIBLING_MARKER_0125\r\n" });
      await page.waitForTimeout(120);
      await page.locator(".ws-workspace-zoom").click();
      await page.waitForFunction(() => (document.querySelector('.ws-cell[data-ws-session="ws01"]')?.textContent ?? "").includes("HIDDEN_SIBLING_MARKER_0125"));
      const afterRestore = await snap();
      assert.ok(afterRestore.attachments.every((a) => a.resizes === 0), "Maximize/Restore must emit zero RESIZE_REQUEST");
      const afterZoomAtts = afterRestore.attachments.filter((a) => a.live).map((a) => [a.session, a.id]);
      assert.deepEqual(afterZoomAtts, beforeZoomAtts, "Maximize/Restore must preserve every live socket");

      // 1. Blocker 1 regression: stage alias label -> nudge divider -> Save -> BOTH survive in single PUT!
      await page.locator(".ws-workspace-edit").click();
      await page.locator(".ws-editor:not([hidden])").waitFor();
      const ws01Row = page.locator('.ws-editor__pane[data-ws-session="ws01"]');
      await ws01Row.locator(".ws-editor__alias input").fill("Primary shell");
      await ws01Row.locator(".ws-editor__alias-save").click();
      // Now nudge live divider while alias is staged
      await page.locator(".ws-divider").first().hover();
      await page.locator('.ws-divider__nudge[data-ws-nudge="1"]').first().click();
      const writesBefore = (await snap()).counters.workspaceWrites;
      await page.locator(".ws-editor__save").click();
      await page.waitForFunction(() => document.querySelector(".ws-editor")?.hidden === true);
      const afterAliasNudge = await snap();
      assert.equal(afterAliasNudge.counters.workspaceWrites, writesBefore + 1, "Save must issue exactly 1 PUT");
      const savedOps = afterAliasNudge.workspaces.find((w) => w.workspace_id === opsRecord.workspace_id);
      assert.equal(savedOps.tree.children[0].alias_hint, "Primary shell", "divider nudge must NOT erase staged pane alias");
      assert.equal(savedOps.tree.weights[0], 55, `divider nudge weight must persist alongside alias (got ${JSON.stringify(savedOps.tree.weights)})`);

      // Structural divergence locks ambiguous live divider so it cannot clobber staged 3rd pane
      await page.locator(".ws-workspace-edit").click();
      await page.locator(".ws-editor__add-row").click();
      const divergedDivider = await page.evaluate(() => {
        const d = document.querySelector(".ws-divider");
        return { role: d?.getAttribute("role"), disabled: d?.dataset.wsDisabled, tabIndex: d?.tabIndex };
      });
      assert.equal(divergedDivider.disabled, "true", "diverged live seam must disable after structural draft edit");
      assert.equal(divergedDivider.role, "presentation");
      assert.equal(divergedDivider.tabIndex, -1);
      await page.locator(".ws-editor__cancel").click();

      // 2. Blocker 2 regression: 409 normalized-name collision must NOT retarget to Other!
      await page.locator(".ws-workspace-edit").click();
      await page.locator('input[name="workspace_name"]').fill("other");
      await page.locator(".ws-editor__save").click();
      await page.waitForFunction(() => /already in use/i.test(document.querySelector(".ws-editor__status")?.textContent ?? ""));
      const collisionState = await page.evaluate(() => ({
        hasReload: !!document.querySelector(".ws-editor__reload"),
        title: document.querySelector(".ws-workspace-title")?.textContent,
        sessions: [...document.querySelectorAll(".ws-cell")].map((c) => c.dataset.wsSession),
        hash: location.hash,
      }));
      assert.equal(collisionState.hasReload, false, "name collision must NOT show cross-workspace Reload button");
      assert.equal(collisionState.title, "Ops");
      assert.deepEqual(collisionState.sessions, ["ws01", "ws02"]);
      assert.equal(collisionState.hash, "#name=Ops");

      // Next valid rename MUST still target Ops's own workspace_id (not Other's!)
      await page.locator('input[name="workspace_name"]').fill("Ops Final");
      await page.locator(".ws-editor__save").click();
      await page.waitForFunction(() => document.querySelector(".ws-editor")?.hidden === true && document.querySelector(".ws-workspace-title")?.textContent === "Ops Final");
      const finalSnap = await snap();
      const renamedOps = finalSnap.workspaces.find((w) => w.workspace_id === opsRecord.workspace_id);
      const intactOther = finalSnap.workspaces.find((w) => w.name === "Other");
      assert.equal(renamedOps?.name, "Ops Final", "Ops itself must be renamed");
      assert.ok(intactOther && intactOther.tree.children[0].session.name === "ws03", "Other workspace must remain completely untouched");

      // 7. Pending pane Display label without clicking "Set label": survives preset + Save, keeps focus live, refuses invalid on Save
      await page.locator(".ws-workspace-edit").click();
      await page.locator(".ws-editor:not([hidden])").waitFor();
      const ws01Input = page.locator('.ws-editor__pane[data-ws-session="ws01"] .ws-editor__alias input');
      await ws01Input.click();
      await ws01Input.fill("Live typed label");
      const liveAliasCheck = await page.evaluate(() => ({
        focusedStill: document.activeElement?.closest(".ws-editor__pane")?.dataset.wsSession === "ws01"
          && document.activeElement?.closest(".ws-editor__alias") !== null,
        diagramText: document.querySelector(".ws-editor__diagram-frame")?.textContent ?? "",
      }));
      assert.equal(liveAliasCheck.focusedStill, true, "typing Display label must keep focus on that input");
      assert.ok(liveAliasCheck.diagramText.includes("Live typed label"), `draft diagram must reflect typed label live (got ${JSON.stringify(liveAliasCheck.diagramText)})`);
      await page.locator(".ws-editor__preset", { hasText: "Stacked" }).click();
      const afterPresetVal = await page.locator('.ws-editor__pane[data-ws-session="ws01"] .ws-editor__alias input').inputValue();
      assert.equal(afterPresetVal, "Live typed label", "typed label must survive preset layout change without clicking Set label");
      await page.locator(".ws-editor__save").click();
      await page.waitForFunction(() => document.querySelector(".ws-editor")?.hidden === true);
      const savedLiveLabel = (await snap()).workspaces.find((w) => w.workspace_id === opsRecord.workspace_id);
      assert.equal(savedLiveLabel.tree.direction, "column", "Stacked preset must persist");
      assert.equal(savedLiveLabel.tree.children[0].alias_hint, "Live typed label", "typed label must persist on Save without clicking Set label");

      // Invalid typed label (leading/trailing spaces) must be refused on Save with ZERO PUT
      await page.locator(".ws-workspace-edit").click();
      await page.locator(".ws-editor:not([hidden])").waitFor();
      const writesBeforeInvalid = (await snap()).counters.workspaceWrites;
      await page.locator('.ws-editor__pane[data-ws-session="ws02"] .ws-editor__alias input').fill("  padded  ");
      await page.locator(".ws-editor__save").click();
      await page.waitForFunction(() => /leading or trailing whitespace/i.test(document.querySelector(".ws-editor__status")?.textContent ?? ""));
      assert.equal((await snap()).counters.workspaceWrites, writesBeforeInvalid, "invalid pending label must refuse Save with zero PUT");
      await page.locator(".ws-editor__cancel").click();

      // 8. Cancel-during-Save race: Cancel and header Edit button stay disabled while PUT is in flight
      await page.locator(".ws-workspace-edit").click();
      await page.locator(".ws-editor:not([hidden])").waitFor();
      await page.locator(".ws-editor__preset", { hasText: "Side by side" }).click();
      await control({ holdWorkspaceWriteMs: 350 });
      await page.locator(".ws-editor__save").click();
      const midSave = await page.evaluate(() => ({
        saveDisabled: document.querySelector(".ws-editor__save")?.disabled,
        cancelDisabled: document.querySelector(".ws-editor__cancel")?.disabled,
        editDisabled: document.querySelector(".ws-workspace-edit")?.disabled,
      }));
      assert.equal(midSave.saveDisabled, true, "Save must disable while PUT is in flight");
      assert.equal(midSave.cancelDisabled, true, "Cancel must disable while PUT is in flight");
      assert.equal(midSave.editDisabled, true, "header Edit button must disable while PUT is in flight");
      await page.waitForFunction(() => document.querySelector(".ws-editor")?.hidden === true);
      const afterMidSave = await page.evaluate(() => ({
        editDisabled: document.querySelector(".ws-workspace-edit")?.disabled,
        hiddenHeaderDisplays: [...document.querySelectorAll(".ws-workspace-header [hidden]")].map((el) => getComputedStyle(el).display),
      }));
      assert.equal(afterMidSave.editDisabled, false, "header Edit button must re-enable after Save finishes");
      assert.ok(afterMidSave.hiddenHeaderDisplays.every((d) => d === "none"), `hidden header controls must compute display:none (got ${JSON.stringify(afterMidSave.hiddenHeaderDisplays)})`);

      // 9. Key-reconciled in-place pane switch -> one-click "Save switch" keeps live socket & zero RESIZE_REQUEST
      const switchPane = async (paneSession, targetSession) => {
        await page.locator(`.ws-cell[data-ws-session="${paneSession}"] .persea-unified-tag`).click();
        const row = page.locator(`.ws-cell[data-ws-session="${paneSession}"] .persea-unified-identity__details:not([hidden]) .persea-session-switcher__row[aria-label="Switch to ${targetSession}"]`);
        await row.waitFor({ state: "visible" });
        await row.click();
        await page.waitForFunction(
          ([p, t]) => (document.querySelector(`.ws-cell[data-ws-session="${p}"]`)?.textContent ?? "").includes(`fixture-live ${t}`),
          [paneSession, targetSession],
        );
      };
      await switchPane("ws01", "ws03");
      const beforeSaveSwitch = await snap();
      const ws03LiveBefore = beforeSaveSwitch.attachments.find((a) => a.session === "ws03" && a.live);
      assert.ok(ws03LiveBefore, "ws03 must have a live attachment after in-place switch");
      const switchHeaderState = await page.evaluate(() => {
        const btn = document.querySelector(".ws-workspace-save-switch");
        const pillTag = document.querySelector('.ws-pane-pill[data-ws-session="ws01"] .ws-pane-pill__tag');
        return {
          pillTag: pillTag?.textContent ?? "",
          btnHidden: btn?.hidden,
          btnDisabled: btn?.disabled,
          btnLabel: btn?.getAttribute("aria-label") ?? "",
          btnDisplay: btn ? getComputedStyle(btn).display : null,
        };
      });
      assert.equal(switchHeaderState.pillTag, "Switched");
      assert.equal(switchHeaderState.btnHidden, false, "Save switch button must appear when designated pane is switched");
      assert.equal(switchHeaderState.btnDisabled, false, "Save switch must be enabled for a non-duplicate target");
      assert.ok(switchHeaderState.btnLabel.includes("ws03"), `Save switch aria-label must name ws03 (got ${switchHeaderState.btnLabel})`);

      await page.locator(".ws-workspace-save-switch").click();
      await page.waitForFunction(() => document.querySelector(".ws-workspace-save-switch")?.hidden === true
        && !!document.querySelector('.ws-cell[data-ws-session="ws03"]'));
      const afterSaveSwitch = await snap();
      const ws03LiveAfter = afterSaveSwitch.attachments.find((a) => a.session === "ws03" && a.live);
      assert.equal(ws03LiveAfter?.id, ws03LiveBefore.id, "Save switch must keep the EXACT same live WebSocket attachment (zero reconnect/remount)");
      assert.ok(afterSaveSwitch.attachments.every((a) => a.resizes === 0), "Save switch must emit zero RESIZE_REQUEST");
      const savedSwitchWs = afterSaveSwitch.workspaces.find((w) => w.workspace_id === opsRecord.workspace_id);
      assert.equal(savedSwitchWs.tree.children[0].session.name, "ws03", "durable workspace must now reference ws03");
      assert.equal(savedSwitchWs.tree.children[0].alias_hint, "Live typed label", "Save switch must preserve pane label");

      // Duplicate target (switching pane 1 to ws02 which is already pane 2) -> Save switch disabled, zero PUT
      await switchPane("ws03", "ws02");
      const dupSwitchState = await page.evaluate(() => ({
        hidden: document.querySelector(".ws-workspace-save-switch")?.hidden,
        disabled: document.querySelector(".ws-workspace-save-switch")?.disabled,
      }));
      assert.equal(dupSwitchState.hidden, false, "Save switch button should explain why duplicate cannot be saved");
      assert.equal(dupSwitchState.disabled, true, "Save switch must be disabled when target is already another pane");

      // Switch pane 1 to ws04 -> open Edit -> "Use current" stages ws04 -> Save keeps live ws04 socket!
      await switchPane("ws03", "ws04");
      const ws04LiveBefore = (await snap()).attachments.find((a) => a.session === "ws04" && a.live);
      assert.ok(ws04LiveBefore, "ws04 must be live after in-place switch");
      await page.locator(".ws-workspace-edit").click();
      await page.locator(".ws-editor:not([hidden])").waitFor();
      const useCurrentBtn = page.locator('.ws-editor__pane[data-ws-session="ws03"] .ws-editor__use-current');
      await useCurrentBtn.waitFor({ state: "visible" });
      await useCurrentBtn.click();
      await page.locator(".ws-editor__save").click();
      await page.waitForFunction(() => document.querySelector(".ws-editor")?.hidden === true
        && !!document.querySelector('.ws-cell[data-ws-session="ws04"]'));
      const afterEditorSwitch = await snap();
      const ws04LiveAfter = afterEditorSwitch.attachments.find((a) => a.session === "ws04" && a.live);
      assert.equal(ws04LiveAfter?.id, ws04LiveBefore.id, "Editor Use current + Save must preserve live ws04 WebSocket without reconnect");
      assert.ok(afterEditorSwitch.attachments.every((a) => a.resizes === 0), "Editor Use current + Save must emit zero RESIZE_REQUEST");

      assert.deepEqual(errors, [], `Workspace UX browser errors: ${JSON.stringify(errors)}`);
      await context.close();
    }

    // --- 6: Dashboard multi-session builder, passive availability, Duplicate, left-aligned previews, nav arrows, phone M5 ---
    {
      await control({ reset: true, sessions: ["ws01", "ws02", "ws03", "ws04"] });
      const context = await browser.newContext(contextOptions({ viewport: { width: 1280, height: 900 } }));
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
      page.on("console", (m) => { if (m.type() === "error") errors.push(`console: ${m.text()}`); });

      await page.goto(`${fixture.origin}/`);
      await page.locator(".session-card").first().waitFor();

      // Left-aligned mini-preview screen
      const previewAlign = await page.evaluate(() => getComputedStyle(document.querySelector(".session-preview-row-screen")).textAlign);
      assert.equal(previewAlign, "left", `dashboard mini-preview must be left-aligned (got ${previewAlign})`);

      // Keyboard navigation on tabs: ArrowRight moves focus to Workspaces WITHOUT firing /api/workspaces; Enter activates
      const readsBeforeArrow = (await snap()).counters.workspaceReads;
      await page.locator('.dashboard-navigation button').first().focus();
      await page.keyboard.press("ArrowRight");
      const focusedTab = await page.evaluate(() => document.activeElement?.textContent);
      assert.equal(focusedTab, "Workspaces");
      await page.waitForTimeout(80);
      assert.equal((await snap()).counters.workspaceReads, readsBeforeArrow, "ArrowRight on nav must not eagerly fetch workspaces");
      await page.keyboard.press("Enter");
      await page.locator(".workspace-panel:not([hidden])").waitFor();

      // Multi-session builder: create a 3-pane workspace with Main + stack preset in ONE POST
      await page.getByLabel("New workspace name", { exact: true }).fill("Dev Trio");
      const extraBoxes = page.locator(".workspace-panel__extra-chip input[type='checkbox']");
      await extraBoxes.nth(0).check();
      await extraBoxes.nth(1).check();
      await page.locator(".workspace-panel__presets:not([hidden])").waitFor();
      await page.getByRole("button", { name: "Main + stack", exact: true }).click();
      await page.getByRole("button", { name: "New workspace", exact: true }).click();
      await page.locator(".workspace-panel__item").first().waitFor();

      const createdSnap = await snap();
      const trio = createdSnap.workspaces.find((w) => w.name === "Dev Trio");
      assert.ok(trio, "Dev Trio must be created");
      assert.equal(trio.tree.kind, "split");
      assert.deepEqual(trio.tree.weights, [55, 45], "Main + stack weights must persist");
      assert.equal(trio.tree.children[1].kind, "split");
      assert.equal(trio.tree.children[1].children.length, 2);

      // Inline Open link after creation + card diagram + "3 panes"
      const cardInfo = await page.evaluate(() => ({
        createdOpenHref: document.querySelector(".workspace-panel__created-open")?.getAttribute("href"),
        hasDiagram: !!document.querySelector(".workspace-panel__item .ws-mini"),
        summary: document.querySelector(".workspace-panel__summary")?.textContent,
      }));
      assert.ok(cardInfo.createdOpenHref && cardInfo.createdOpenHref.includes("#name=Dev+Trio"));
      assert.equal(cardInfo.hasDiagram, true);
      assert.equal(cardInfo.summary, "3 panes");

      // Drop ws02 from inventory and refresh -> cached card (same revision!) updates passive availability in-place to "3 panes · 1 missing"
      await control({ session: "ws02", kill: true });
      await page.locator(".dashboard-workspace-refresh").click();
      // Also refresh inventory via Sessions refresh or background; dashboard-workspace-refresh refreshes workspaces,
      // while inventory updates via Refresh on Sessions. Trigger Sessions refresh and verify availability updates passively!
      await page.getByRole("button", { name: "Sessions", exact: true }).click();
      await page.getByRole("button", { name: "Refresh sessions", exact: true }).click();
      await page.getByRole("button", { name: "Workspaces", exact: true }).click();
      await page.waitForFunction(() => document.querySelector(".workspace-panel__summary")?.textContent === "3 panes · 1 missing");

      // Duplicate workspace under Manage -> creates "Dev Trio copy" with same 3-pane tree
      await page.locator(".workspace-panel__manage > summary").first().click();
      await page.locator(".workspace-panel__duplicate").first().click();
      await page.waitForFunction(() => document.querySelectorAll(".workspace-panel__item").length === 2);

      assert.deepEqual(errors, [], `Dashboard UX browser errors: ${JSON.stringify(errors)}`);
      await context.close();

      // Phone posture omits both Create form AND Duplicate button
      const phoneCtx = await browser.newContext(contextOptions({ viewport: { width: 390, height: 844 }, screen: { width: 390, height: 844 }, hasTouch: true }));
      const phonePage = await phoneCtx.newPage();
      await phonePage.emulateMedia({ reducedMotion: "no-preference" });
      await phonePage.goto(`${fixture.origin}/`);
      await phonePage.locator(".session-card").first().waitFor();
      await phonePage.getByRole("button", { name: "Workspaces", exact: true }).click();
      await phonePage.locator(".workspace-panel:not([hidden])").waitFor();
      const phoneCheck = await phonePage.evaluate(() => ({
        createForms: document.querySelectorAll("form.workspace-panel__create").length,
        duplicates: document.querySelectorAll(".workspace-panel__duplicate").length,
        items: document.querySelectorAll(".workspace-panel__item").length,
      }));
      assert.equal(phoneCheck.createForms, 0, "phone must omit workspace create form");
      assert.equal(phoneCheck.duplicates, 0, "phone must omit workspace duplicate button");
      assert.equal(phoneCheck.items, 2, "phone must still list saved workspaces");
      await phoneCtx.close();
    }

    console.log(`workspace UX & dashboard polish browser tests (${ENGINE}): PASS`);
  } finally {
    await browser.close();
    await fixture.close();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
