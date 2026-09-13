"use strict";

// Unified tab reopen gate.
//
// The operator's contract: a unified terminal tab that was closed — same tab
// restored, a new tab on the same device, or another device — reopens into
// live control without a hand. Two production defects sat behind the reported
// dead end ("This terminal is unavailable · code: input_refused"):
//
//   R1  the page sent its first INPUT (xterm's focus-in report, emitted by
//       terminal.focus() once the replayed journal had enabled DECSET 1004)
//       BEFORE the MODE_REQUEST, the broker refused it, and the front door
//       closed the attachment with input_refused — a terminal class;
//   R2  a consumed or expired fragment handle answered 410 at /ws, the
//       transport knew no source binding, and the retry exhausted into
//       source_binding_unavailable.
//
// Every scenario drives the REAL bundled page in real Chromium against the
// fixture's one-time handles, control lease, takeover offers, source bindings,
// and broker automaton (see unified_reopen_fixture.cjs). The console is
// captured on every document; the only tolerated entry is Chromium's own
// report of the 410 WebSocket refusal a consumed handle MUST produce.

const fs = require("fs");
const os = require("os");
const path = require("path");
const childProcess = require("child_process");
const { startFixture } = require("./unified_reopen_fixture.cjs");
const { assert, browserBinary, delay, freePort, requestJSON, Tab: BaseTab } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");

// The page facts this gate's scenarios judge.
const STATE = `(() => {
      const notice = document.querySelector(".persea-unified-notice");
      const take = document.querySelector(".persea-unified-notice__take-control");
      const rows = document.querySelector(".xterm-rows");
      const takeRect = take && !take.hidden ? take.getBoundingClientRect() : null;
      const refusal = document.querySelector(".persea-unified-refusal");
      const view = document.querySelector(".persea-unified-view-disclosure");
      const viewRect = view ? view.getBoundingClientRect() : null;
      const fit = document.querySelector('.persea-unified-view-popover button[aria-label="Fit rows"]');
      const fitRect = fit ? fit.getBoundingClientRect() : null;
      return {
        href: location.href,
        ready: document.readyState,
        navigationType: performance.getEntriesByType("navigation")[0]?.type ?? null,
        fatal: document.querySelector(".persea-terminal-fatal")?.textContent ?? null,
        bodyText: (document.body.textContent ?? "").trim().length,
        screen: rows ? rows.textContent ?? "" : "",
        connection: document.querySelector(".persea-unified-connection")?.textContent ?? "",
        noticeHidden: notice ? notice.hidden : true,
        headline: document.querySelector(".persea-unified-notice__headline")?.textContent ?? "",
        detail: document.querySelector(".persea-unified-notice__detail")?.textContent ?? "",
        code: document.querySelector(".persea-unified-notice__code")?.textContent ?? "",
        dashboardHref: document.querySelector(".persea-unified-notice__dashboard")?.getAttribute("href") ?? "",
        takeHidden: take ? take.hidden : true,
        takeDisabled: take ? take.disabled : true,
        takeText: take?.textContent ?? "",
        takePoint: takeRect ? { x: takeRect.x + takeRect.width / 2, y: takeRect.y + takeRect.height / 2 } : null,
        reloadHandle: (() => { try { return sessionStorage.getItem("persea-unified-terminal-reload-handle-v1") !== null; } catch { return false; } })(),
        refusal: refusal && !refusal.hidden ? refusal.textContent ?? "" : "",
        viewExpanded: view?.getAttribute("aria-expanded") === "true",
        viewPoint: viewRect && viewRect.width > 0 ? { x: viewRect.x + viewRect.width / 2, y: viewRect.y + viewRect.height / 2 } : null,
        fitDisabled: fit ? fit.disabled : true,
        fitPoint: fitRect && fitRect.width > 0 ? { x: fitRect.x + fitRect.width / 2, y: fitRect.y + fitRect.height / 2 } : null,
        terminalRows: (() => { const host = document.querySelector(".xterm"); return host ? Number(host.querySelector(".xterm-rows")?.children.length ?? 0) : 0; })(),
      };
    })()`;

class Tab extends BaseTab {
  static open(name, debugPort, origin) {
    return super.open(name, debugPort, origin, STATE);
  }
}

const isLive = (state) => state.screen.includes("fixture-live") && state.noticeHidden && state.connection === "";
const hasNotice = (state) => !state.noticeHidden;
// Packet F6: no typed failure state may render a blank page.
const nonBlank = (state) => !state.noticeHidden && state.headline.length > 0 && state.detail.length > 0 && state.code.length > 0 && state.dashboardHref === "/";

async function main() {
  const fixture = await startFixture(UI);
  const origin = fixture.origin;
  const control = (input) => requestJSON(`${origin}/__fixture/control`, "POST", input);
  const snapshot = () => requestJSON(`${origin}/__fixture/control`);
  const freshControlHandle = async () => {
    const inventory = await requestJSON(`${origin}/api/inventory`);
    return inventory.realms[0].servers[0].sessions[0].handles.control;
  };
  const unifiedURL = (handle) => `${origin}/terminal?engine=unified-dev#${new URLSearchParams({
    handle, mode: "control", history: "1000", name: "alpha", draft_scope: fixture.draftScope, engine: "unified-dev",
  }).toString()}`;
  const lastAttachment = async () => {
    const current = await snapshot();
    return current.attachments[current.attachments.length - 1];
  };
  const firstInputIndex = (frames) => frames.findIndex((frame) => frame.startsWith("INPUT:"));

  const debugPort = await freePort();
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), "persea-terminal-unified-reopen-"));
  const chrome = childProcess.spawn(browserBinary(), [
    "--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
    `--remote-debugging-port=${debugPort}`, `--user-data-dir=${profile}`,
    "--no-first-run", "--no-default-browser-check", "about:blank",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  const chromeStderr = [];
  let chromeExit = null;
  chrome.stderr.on("data", (chunk) => { if (chromeStderr.length < 64) chromeStderr.push(String(chunk)); });
  chrome.on("exit", (code, signal) => { chromeExit = { code, signal }; });
  const tabs = [];
  const failures = [];
  const evidence = {};
  const fail = (scenario, message, detail) => failures.push(`${scenario}: ${message} ${JSON.stringify(detail)}`);
  try {
    for (let attempt = 0; attempt < 300 && !chromeExit; attempt += 1) {
      try { await requestJSON(`http://127.0.0.1:${debugPort}/json/version`); break; } catch { await delay(100); }
    }
    assert(!chromeExit, `Chrome exited early: ${JSON.stringify({ chromeExit, stderr: chromeStderr.join("").slice(-2_000) })}`);
    const tabA = await Tab.open("A", debugPort, origin);
    tabs.push(tabA);

    // Retired deep links must fail before creating any attachment WebSocket.
    const retiredRoutes = [];
    for (const width of [1280, 390]) {
      await control({ reset: true });
      await tabA.cdp.send("Emulation.setDeviceMetricsOverride", { width, height: 844, deviceScaleFactor: 1, mobile: width < 500 });
      let socketRequests = 0;
      const countSocket = () => { socketRequests += 1; };
      tabA.cdp.on("Network.webSocketCreated", countSocket);
      const before = await snapshot();
      const retiredURL = origin + "/terminal#" + new URLSearchParams({ handle: await freshControlHandle(), mode: "control", history: "1000" });
      await tabA.navigate(retiredURL);
      const refused = await tabA.waitUntil(state => state.fatal?.includes("engine must be unified-dev"));
      assert(refused.state, "retired terminal deep link did not use existing fatal handling");
      const after = await snapshot();
      assert(socketRequests === 0 && after.counters.websockets === before.counters.websockets, "retired URL created an attachment WebSocket");
      await tabA.navigate(origin + "/terminal");
      assert(await tabA.evaluate('document.querySelector(".dashboard") !== null'), "handleless terminal landing lost Dashboard");
      await tabA.navigate(unifiedURL(await freshControlHandle()));
      assert((await tabA.waitUntil(isLive)).state, "current Unified attachment failed after retired URL refusal");
      assert(await tabA.evaluate('Array.from(document.querySelectorAll("button,a,summary")).every(node => !/legacy/i.test(node.textContent || ""))'), "current menu exposes a Legacy action");
      retiredRoutes.push({ width, refused: true, zeroSockets: true, dashboard: true, unified: true, noLegacyMenu: true });
    }
    evidence.retiredRoutes = retiredRoutes;
    await tabA.cdp.send("Emulation.setDeviceMetricsOverride", { width: 1280, height: 844, deviceScaleFactor: 1, mobile: false });

    // --- S0 precondition: a fresh inventory handle opens into live control ----
    await control({ reset: true });
    const firstURL = unifiedURL(await freshControlHandle());
    await tabA.navigate(firstURL);
    const s0 = await tabA.waitUntil(isLive);
    assert(s0.state, `S0 precondition: a fresh handle did not reach live control: ${JSON.stringify(s0.last)}`);
    await tabA.type("x");
    await delay(150);
    const s0Attachment = await lastAttachment();
    assert(s0Attachment.frames[0] === "READY" && s0Attachment.frames[1] === "MODE_REQUEST" && s0Attachment.inputs.includes("x"),
      `S0 precondition: the live attachment did not carry READY, MODE_REQUEST, then input: ${JSON.stringify(s0Attachment)}`);
    assert(s0.state.reloadHandle, "S0 precondition: the page did not remember a reload handle");
    evidence.s0 = { frames: s0Attachment.frames, navigationType: s0.state.navigationType };

    // --- R1 no INPUT before the control grant (the production trigger) ---------
    // The broker refuses INPUT while its automaton is still in observe mode,
    // and the front door turns that refusal into a socket close. The page must
    // therefore never emit INPUT — a keystroke OR xterm's focus-in report,
    // which the replayed journal's DECSET 1004 arms — before the MODE grant.
    // Holding the grant opens exactly that window deterministically.
    await control({ reset: true, replayFocusReporting: true, holdModeGrant: true });
    const focusURL = unifiedURL(await freshControlHandle());
    await tabA.navigate(focusURL);
    // The page is committed but ungranted: wait for the request to be recorded.
    const r1Windowed = await tabA.waitUntil((state) => state.screen.includes("fixture-live") || hasNotice(state));
    await delay(150);
    await tabA.type("k"); // a keystroke in the ungranted window
    await delay(200);
    let r1Attachment = await lastAttachment();
    const r1Mode = r1Attachment.frames.indexOf("MODE_REQUEST");
    const r1EarlyInput = r1Attachment.frames.slice(0, r1Mode < 0 ? undefined : r1Mode + 1).some((frame) => frame.startsWith("INPUT:"));
    const r1PreGrantInput = r1Attachment.frames.filter((frame, index) => frame.startsWith("INPUT:") && (r1Attachment.mode !== "CONTROL"));
    evidence.r1 = { frames: r1Attachment.frames, closeReason: r1Attachment.closeReason, windowed: r1Windowed.state ?? r1Windowed.last };
    if (r1EarlyInput || r1PreGrantInput.length > 0 || r1Attachment.closeReason === "input_refused") {
      fail("R1", "the page sent INPUT to the broker before the control grant, so the broker refused it (input_refused)", evidence.r1);
    }
    // Release the grant: the page must now be live and typed input must land.
    await control({ releaseMode: true });
    const r1Live = await tabA.waitUntil(isLive);
    if (!r1Live.state) {
      fail("R1", "after the control grant the page did not reach live control", { evidence: evidence.r1, state: r1Live.last });
    } else {
      await tabA.type("j");
      await delay(150);
      r1Attachment = await lastAttachment();
      if (!r1Attachment.inputs.includes("j")) fail("R1", "typed input after the grant was not delivered", r1Attachment);
    }

    // --- R2 consumed handle, fresh document (tab restored / URL reopened) ------
    await control({ replayFocusReporting: true });
    // The fragment still carries the consumed handle; /ws answers 410 before the
    // upgrade. The page must re-mint from the identity in the fragment and land
    // in live control by itself, showing a reconnecting strip on the way.
    const beforeR2 = await snapshot();
    await tabA.navigate(focusURL);
    const r2 = await tabA.waitUntil((state) => isLive(state) || hasNotice(state));
    const afterR2 = await snapshot();
    const r2Attachment = afterR2.attachments[afterR2.attachments.length - 1];
    evidence.r2 = {
      navigationType: (r2.state ?? r2.last)?.navigationType, strips: r2.strips, state: r2.state ?? r2.last,
      inventoryFetches: afterR2.counters.inventory - beforeR2.counters.inventory,
      websockets: afterR2.counters.websockets - beforeR2.counters.websockets,
      frames: r2Attachment?.frames,
    };
    if (!r2.state || !isLive(r2.state)) {
      fail("R2", "reopening the unified page with its consumed handle dead-ended instead of re-minting", evidence.r2);
    } else {
      if (!r2.strips.some((text) => text.includes("Reconnecting to alpha"))) fail("R2", "no 'Reconnecting to alpha…' strip was shown while re-minting", evidence.r2);
      if (evidence.r2.inventoryFetches < 1) fail("R2", "the page re-attached without resolving its identity through the inventory", evidence.r2);
      const r2Mode = r2Attachment.frames.indexOf("MODE_REQUEST");
      const r2Input = firstInputIndex(r2Attachment.frames);
      if (r2Mode < 0 || (r2Input >= 0 && r2Input < r2Mode)) fail("R2", "the re-minted attachment sent input before its control grant", evidence.r2);
      await tabA.type("m");
      await delay(150);
      const typed = await lastAttachment();
      if (!typed.inputs.includes("m")) fail("R2", "typed input after the silent re-attach was not delivered", typed);
    }

    // --- R3 control: a document reload still reopens into live control --------
    // (The remembered reload handle carries the reload; a fresh navigate is the
    // re-mint path proven by R2. This is the regression guard that the reload
    // path was not disturbed.)
    const beforeR3 = await snapshot();
    await tabA.reload();
    const r3 = await tabA.waitUntil((state) => state.navigationType === "reload" && (isLive(state) || hasNotice(state)));
    const afterR3 = await snapshot();
    evidence.r3 = { state: r3.state ?? r3.last, websockets: afterR3.counters.websockets - beforeR3.counters.websockets };
    if (!r3.state || !isLive(r3.state)) fail("R3", "a document reload did not reopen into live control", evidence.r3);

    // --- R4 cross-device: another tab reopens the same URL while this one holds
    // control. Reopening is explicit intent: the newcomer takes control by
    // itself; the displaced tab shows the displacement with a working manual
    // claim, so the operator can take it back from either side.
    const tabB = await Tab.open("B", debugPort, origin);
    tabs.push(tabB);
    const beforeR4 = await snapshot();
    await tabB.navigate(focusURL);
    const r4B = await tabB.waitUntil((state) => isLive(state) || hasNotice(state));
    const r4A = await tabA.waitUntil((state) => hasNotice(state) || !state.screen.includes("fixture-live"), 4_000);
    const afterR4 = await snapshot();
    evidence.r4 = { tabB: r4B.state ?? r4B.last, stripsB: r4B.strips, tabA: r4A.state ?? r4A.last, takeovers: afterR4.counters.takeovers - beforeR4.counters.takeovers, lease: afterR4.leaseAttachment, attachments: afterR4.attachments.slice(-3).map((item) => ({ id: item.id, takeover: item.takeover, closeReason: item.closeReason, live: item.live })) };
    if (!r4B.state || !isLive(r4B.state)) {
      fail("R4", "a second device reopening the session did not take control automatically", evidence.r4);
    } else {
      if (evidence.r4.takeovers !== 1) fail("R4", "the newcomer did not claim control through exactly one takeover", evidence.r4);
      if (!r4A.state || !nonBlank(r4A.state) || !r4A.state.code.includes("control_displaced") || r4A.state.takeHidden) {
        fail("R4", "the displaced tab did not show control_displaced with a manual take-control claim", evidence.r4);
      } else {
        // Reclaim from the displaced tab. Liveness here is proven FUNCTIONALLY
        // by input delivery, not by scraped DOM text: after a displace→reclaim
        // on an already-mounted terminal, xterm's committed buffer is correct
        // and control is granted, but its DOM repaint is not guaranteed to have
        // flushed at any given poll under headless CDP. Input only flows when
        // the page is committed AND control-granted, so a delivered keystroke is
        // the honest proof that the reclaim restored live control.
        const beforeBack = await snapshot();
        await tabA.trustedClick(r4A.state.takePoint);
        // The claim resolves and re-attaches; the displacement notice clears.
        const reclaimed = await tabA.waitUntil((state) => state.noticeHidden, 8_000);
        const afterBack = await snapshot();
        evidence.r4.back = {
          tabA: reclaimed.state ?? reclaimed.last,
          takeovers: afterBack.counters.takeovers - beforeBack.counters.takeovers,
          attachments: afterBack.attachments.slice(-3).map((item) => ({ id: item.id, takeover: item.takeover, frames: item.frames, closeReason: item.closeReason, live: item.live, mode: item.mode })),
        };
        if (!reclaimed.state) {
          fail("R4", "the manual claim from the displaced tab did not clear the displacement notice", evidence.r4.back);
        } else if (evidence.r4.back.takeovers !== 1) {
          fail("R4", "the manual reclaim did not issue exactly one takeover", evidence.r4.back);
        } else {
          await tabA.type("a");
          await delay(200);
          const typed = await lastAttachment();
          evidence.r4.back.reclaimInputs = typed.inputs.slice();
          if (!typed.inputs.includes("a")) fail("R4", "the reclaimed tab did not deliver input to the session (not live)", evidence.r4.back);
          const bDisplaced = await tabB.waitUntil((state) => hasNotice(state) && state.code.includes("control_displaced"), 4_000);
          evidence.r4.back.tabB = bDisplaced.state ?? bDisplaced.last;
          if (!bDisplaced.state || !nonBlank(bDisplaced.state)) fail("R4", "the newcomer was not shown its displacement non-blank", evidence.r4.back);
        }
      }
    }
    await tabB.close(debugPort);
    tabs.pop();

    // --- R5 mid-session input_refused: re-attach, never a dead end ------------
    await control({ reset: true });
    await tabA.navigate(unifiedURL(await freshControlHandle()));
    const r5Live = await tabA.waitUntil(isLive);
    assert(r5Live.state, `R5 precondition: fresh open did not reach live control: ${JSON.stringify(r5Live.last)}`);
    await control({ refuseInputs: 1 });
    const beforeR5 = await snapshot();
    await tabA.type("y");
    const r5 = await tabA.waitUntil((state) => hasNotice(state) || (isLive(state) && state.connection === "" && false), 50);
    const r5Recovered = await tabA.waitUntil((state) => isLive(state) || hasNotice(state), 8_000);
    const afterR5 = await snapshot();
    evidence.r5 = { strips: [...new Set([...r5.strips, ...r5Recovered.strips])], state: r5Recovered.state ?? r5Recovered.last, attachments: afterR5.attachments.length - beforeR5.attachments.length, reasons: afterR5.attachments.map((item) => item.closeReason) };
    if (!r5Recovered.state || !isLive(r5Recovered.state)) {
      fail("R5", "an input_refused close stranded the page instead of re-attaching", evidence.r5);
    } else {
      if (!evidence.r5.strips.some((text) => text.includes("Reconnecting to alpha"))) fail("R5", "no reconnecting strip was shown during the re-attach", evidence.r5);
      await tabA.type("z");
      await delay(150);
      const typed = await lastAttachment();
      if (!typed.inputs.includes("z")) fail("R5", "input after the re-attach was not delivered", typed);
    }

    // --- R6 a refusal burst degrades to a visible terminal notice, never a loop
    await control({ refuseInputs: 50 });
    let r6 = null;
    for (let round = 0; round < 8 && !r6; round += 1) {
      const live = await tabA.waitUntil((state) => isLive(state) || (hasNotice(state) && state.code.includes("input_refused")), 8_000);
      if (!live.state) break;
      if (hasNotice(live.state)) { r6 = live.state; break; }
      await tabA.type("q");
      await delay(100);
    }
    const afterR6 = await snapshot();
    evidence.r6 = { state: r6, attachments: afterR6.attachments.length, liveSockets: afterR6.attachments.filter((item) => item.live).length };
    if (!r6 || !nonBlank(r6)) fail("R6", "a burst of refusals did not settle into a visible terminal notice", evidence.r6);
    else if (afterR6.attachments.length > 8) fail("R6", "the re-attach loop is unbounded", evidence.r6);

    // --- R7 genuinely unresolvable: the session is gone ------------------------
    await control({ reset: true });
    const goneURL = unifiedURL(await freshControlHandle());
    await tabA.navigate(goneURL);
    assert((await tabA.waitUntil(isLive)).state, "R7 precondition: fresh open did not reach live control");
    await control({ sessionState: "missing", closeLive: "websocket_read" });
    await tabA.navigate(goneURL);
    const r7 = await tabA.waitUntil(hasNotice);
    evidence.r7 = { state: r7.state ?? r7.last, strips: r7.strips };
    if (!r7.state || !nonBlank(r7.state) || !r7.state.code.includes("session_gone")) {
      fail("R7", "a vanished session did not land on the session_gone dead end with a dashboard way out", evidence.r7);
    }

    // --- R8 adoptable session (broker restarted since): adopt, then attach ---
    // The realistic reopen: the tab was live, the tab closed, and the session
    // fell back to adoptable (its journal lost). The fragment handle is now
    // consumed, so the reopen re-mints from identity, which adopts the idle
    // session and attaches it.
    await control({ reset: true });
    const r8URL = unifiedURL(await freshControlHandle());
    await tabA.navigate(r8URL);
    assert((await tabA.waitUntil(isLive)).state, "R8 precondition: fresh open did not reach live control");
    await control({ sessionState: "adoptable", closeLive: "websocket_read" });
    const beforeR8 = await snapshot();
    await tabA.navigate(r8URL); // the fragment handle is consumed now
    const r8 = await tabA.waitUntil((state) => isLive(state) || hasNotice(state));
    const afterR8 = await snapshot();
    evidence.r8 = { state: r8.state ?? r8.last, adoptions: afterR8.counters.adoptions - beforeR8.counters.adoptions, strips: r8.strips };
    if (!r8.state || !isLive(r8.state) || evidence.r8.adoptions !== 1) {
      fail("R8", "an adoptable session was not adopted and attached on reopen", evidence.r8);
    }

    // --- R9 an in-band resize_failed is a passing notice, never a close -------
    // The operator's iPhone report: the explicit Fit on an adopted session
    // failed and the page showed "This terminal is unavailable ·
    // code: resize_failed". A current front door relays an operational
    // refusal in-band; the page must show it, open the Fit control again,
    // keep the SAME attachment, and keep delivering input.
    await control({ reset: true, refuseResize: "resize_failed" });
    // A taller viewport than the fixture's 24-row birth geometry, so the
    // explicit Fit has something to ask for (a measurement equal to the
    // committed height is a deliberate no-op).
    await tabA.cdp.send("Emulation.setDeviceMetricsOverride", { width: 900, height: 1100, deviceScaleFactor: 1, mobile: false });
    await tabA.navigate(unifiedURL(await freshControlHandle()));
    const r9View = await tabA.waitUntil((state) => isLive(state) && state.viewPoint !== null);
    assert(r9View.state, `R9 precondition: fresh open did not expose View & appearance: ${JSON.stringify(r9View.last)}`);
    await tabA.trustedClick(r9View.state.viewPoint);
    const r9Live = await tabA.waitUntil((state) => isLive(state) && !state.fitDisabled && state.fitPoint !== null);
    assert(r9Live.state, `R9 precondition: fresh open did not reach live control with an enabled Fit: ${JSON.stringify(r9Live.last)}`);
    const beforeR9 = await snapshot();
    await tabA.trustedClick(r9Live.state.fitPoint);
    const r9 = await tabA.waitUntil((state) => state.refusal.includes("resize_failed") || hasNotice(state) || state.connection !== "", 8_000);
    const r9Settled = await tabA.waitUntil((state) => state.refusal.includes("resize_failed") && !state.fitDisabled, 4_000);
    const afterR9 = await snapshot();
    const r9Attachment = afterR9.attachments[afterR9.attachments.length - 1];
    evidence.r9 = {
      state: r9.state ?? r9.last, settled: r9Settled.state ?? r9Settled.last,
      websockets: afterR9.counters.websockets - beforeR9.counters.websockets,
      resizes: r9Attachment?.resizes, refusals: r9Attachment?.refusals, live: r9Attachment?.live, closeReason: r9Attachment?.closeReason,
    };
    if (!r9.state || !r9.state.refusal.includes("resize_failed") || hasNotice(r9.state) || r9.state.connection !== "") {
      fail("R9", "an in-band resize_failed did not render as a passing refusal notice on a live page", evidence.r9);
    } else {
      if (evidence.r9.resizes !== 1 || evidence.r9.refusals !== 1) fail("R9", "the Fit click did not produce exactly one RESIZE_REQUEST and one refusal", evidence.r9);
      if (evidence.r9.websockets !== 0 || !evidence.r9.live) fail("R9", "the refusal cost the page its attachment (reconnect or close)", evidence.r9);
      if (!r9Settled.state) fail("R9", "the Fit control was not restored after the refusal", evidence.r9);
      await tabA.type("f");
      await delay(150);
      const typed = await lastAttachment();
      if (!typed.inputs.includes("f")) fail("R9", "input after the refused Fit was not delivered on the same attachment", typed);
      // The notice is passing: it leaves by itself.
      const gone = await tabA.waitUntil((state) => state.refusal === "" && state.noticeHidden, 8_000);
      if (!gone.state) fail("R9", "the refusal notice did not dismiss itself", gone.last);
      // Once the cause is gone the same attachment fits.
      await control({ refuseResize: "" });
      const r9Again = await tabA.waitUntil((state) => !state.fitDisabled && state.fitPoint !== null, 4_000);
      if (!r9Again.state) fail("R9", "the Fit control did not re-enable for a second attempt", r9Again.last);
      else {
        const rowsBefore = r9Again.state.terminalRows;
        await tabA.trustedClick(r9Again.state.fitPoint);
        const fitted = await tabA.waitUntil((state) => state.terminalRows !== rowsBefore && !state.fitDisabled, 8_000);
        const afterFit = await lastAttachment();
        evidence.r9.secondFit = { rowsBefore, state: fitted.state ?? fitted.last, resizes: afterFit.resizes, live: afterFit.live };
        if (!fitted.state || afterFit.resizes !== 2 || !afterFit.live) fail("R9", "a Fit after the refused one did not apply on the same attachment", evidence.r9.secondFit);
      }
    }

    await tabA.cdp.send("Emulation.clearDeviceMetricsOverride");

    // --- R10 an in-band input_refused is a passing notice, never a re-attach ---
    await control({ reset: true });
    await tabA.navigate(unifiedURL(await freshControlHandle()));
    assert((await tabA.waitUntil(isLive)).state, "R10 precondition: fresh open did not reach live control");
    await control({ refuseInputsInBand: 1 });
    const beforeR10 = await snapshot();
    await tabA.type("y");
    const r10 = await tabA.waitUntil((state) => state.refusal.includes("input_refused") || hasNotice(state) || state.connection !== "", 8_000);
    await tabA.type("z");
    await delay(200);
    const afterR10 = await snapshot();
    const r10Attachment = afterR10.attachments[afterR10.attachments.length - 1];
    evidence.r10 = { state: r10.state ?? r10.last, websockets: afterR10.counters.websockets - beforeR10.counters.websockets, inputs: r10Attachment?.inputs, refusals: r10Attachment?.refusals, live: r10Attachment?.live };
    if (!r10.state || !r10.state.refusal.includes("input_refused") || hasNotice(r10.state) || r10.state.connection !== "") {
      fail("R10", "an in-band input_refused did not render as a passing refusal notice on a live page", evidence.r10);
    } else if (evidence.r10.websockets !== 0 || !evidence.r10.live || !r10Attachment.inputs.includes("z") || r10Attachment.inputs.includes("y")) {
      fail("R10", "the in-band refusal was not the end of it: the page reconnected, or the next keystroke was lost", evidence.r10);
    }

    // --- R11 a fatal post-mutation resize closes; the reopen re-mints into live
    // The broker's verdict on a Fit whose transaction failed AFTER tmux was
    // mutated (witness recheck, attachment PTY resize, or durable commit) is
    // attachment_failed: never an in-band refusal that would leave a dead
    // attachment open at the old geometry. The page must render the close as a
    // terminal notice with the code and a dashboard way out — no reconnect
    // loop on a typed verdict — and reopening the same URL must re-mint from
    // identity into live control, where a Fit applies on the NEW attachment.
    await control({ reset: true, closeResize: "attachment_failed" });
    await tabA.cdp.send("Emulation.setDeviceMetricsOverride", { width: 900, height: 1100, deviceScaleFactor: 1, mobile: false });
    const r11URL = unifiedURL(await freshControlHandle());
    await tabA.navigate(r11URL);
    const r11View = await tabA.waitUntil((state) => isLive(state) && state.viewPoint !== null);
    assert(r11View.state, `R11 precondition: fresh open did not expose View & appearance: ${JSON.stringify(r11View.last)}`);
    await tabA.trustedClick(r11View.state.viewPoint);
    const r11Live = await tabA.waitUntil((state) => isLive(state) && !state.fitDisabled && state.fitPoint !== null);
    assert(r11Live.state, `R11 precondition: fresh open did not reach live control with an enabled Fit: ${JSON.stringify(r11Live.last)}`);
    const beforeR11 = await snapshot();
    await tabA.trustedClick(r11Live.state.fitPoint);
    const r11 = await tabA.waitUntil(hasNotice, 8_000);
    await delay(500);
    const afterR11 = await snapshot();
    const r11Attachment = afterR11.attachments[afterR11.attachments.length - 1];
    evidence.r11 = {
      state: r11.state ?? r11.last, strips: r11.strips,
      websockets: afterR11.counters.websockets - beforeR11.counters.websockets,
      resizes: r11Attachment?.resizes, closeReason: r11Attachment?.closeReason, live: r11Attachment?.live,
    };
    if (!r11.state || !nonBlank(r11.state) || !r11.state.code.includes("attachment_failed")) {
      fail("R11", "a fatal post-mutation resize close did not land on a terminal notice carrying attachment_failed", evidence.r11);
    } else {
      if (evidence.r11.websockets !== 0 || evidence.r11.live) fail("R11", "the page retried a typed fatal verdict instead of stopping", evidence.r11);
      if (evidence.r11.resizes !== 1 || evidence.r11.closeReason !== "attachment_failed") fail("R11", "the Fit did not produce exactly one RESIZE_REQUEST closed with attachment_failed", evidence.r11);
      // The reopen: the fragment handle is consumed, so the page re-mints from
      // identity and lands in live control on a fresh attachment.
      const beforeReopen = await snapshot();
      await tabA.navigate(r11URL);
      const reopenView = await tabA.waitUntil((state) => (isLive(state) && state.viewPoint !== null) || hasNotice(state));
      if (reopenView.state && isLive(reopenView.state)) await tabA.trustedClick(reopenView.state.viewPoint);
      const reopened = await tabA.waitUntil((state) => (isLive(state) && !state.fitDisabled && state.fitPoint !== null) || hasNotice(state));
      const afterReopen = await snapshot();
      evidence.r11.reopen = {
        state: reopened.state ?? reopened.last, strips: reopened.strips,
        handleRequests: afterReopen.counters.handleRequests - beforeReopen.counters.handleRequests,
        websockets: afterReopen.counters.websockets - beforeReopen.counters.websockets,
      };
      if (!reopened.state || !isLive(reopened.state)) {
        fail("R11", "reopening after a fatal resize close did not re-mint into live control", evidence.r11.reopen);
      } else {
        if (evidence.r11.reopen.handleRequests < 1) fail("R11", "the reopen reused a consumed handle instead of re-minting", evidence.r11.reopen);
        const rowsBefore = reopened.state.terminalRows;
        await tabA.trustedClick(reopened.state.fitPoint);
        const fitted = await tabA.waitUntil((state) => state.terminalRows !== rowsBefore && !state.fitDisabled, 8_000);
        const afterFit = await lastAttachment();
        evidence.r11.reopen.fit = { rowsBefore, state: fitted.state ?? fitted.last, resizes: afterFit.resizes, live: afterFit.live };
        if (!fitted.state || afterFit.resizes !== 1 || !afterFit.live) fail("R11", "a Fit on the re-minted attachment did not apply", evidence.r11.reopen.fit);
      }
    }
    await tabA.cdp.send("Emulation.clearDeviceMetricsOverride");

    // --- R12 subscriber_lagged: a typed subscriber close re-attaches, never a dead end
    // The broker evicts a journal subscriber that fell behind the committed
    // stream and ends that attachment with the typed reason subscriber_lagged
    // (advisor B1); the front door closes the socket with it. The page must
    // classify it reconnectable: re-mint once on the same session, land back
    // in live CONTROL with no operator action, show the reconnecting strip
    // meanwhile, and deliver input on the new attachment. A terminal notice
    // here is exactly the silently-dead-pane class B1 killed, inverted.
    await control({ reset: true });
    await tabA.navigate(unifiedURL(await freshControlHandle()));
    const r12Live = await tabA.waitUntil(isLive);
    assert(r12Live.state, `R12 precondition: fresh open did not reach live control: ${JSON.stringify(r12Live.last)}`);
    const beforeR12 = await snapshot();
    await control({ closeLive: "subscriber_lagged" });
    const r12 = await tabA.waitUntil((state) => hasNotice(state), 50);
    const r12Recovered = await tabA.waitUntil((state) => isLive(state) || hasNotice(state), 8_000);
    await delay(200);
    const afterR12 = await snapshot();
    const r12Old = afterR12.attachments[beforeR12.attachments.length - 1];
    const r12New = afterR12.attachments[afterR12.attachments.length - 1];
    evidence.r12 = {
      strips: [...new Set([...r12.strips, ...r12Recovered.strips])], state: r12Recovered.state ?? r12Recovered.last,
      attachments: afterR12.attachments.length - beforeR12.attachments.length,
      websockets: afterR12.counters.websockets - beforeR12.counters.websockets,
      handleRequests: afterR12.counters.handleRequests - beforeR12.counters.handleRequests,
      oldCloseReason: r12Old?.closeReason, oldLive: r12Old?.live,
      newMode: r12New?.mode, newLive: r12New?.live, newCloseReason: r12New?.closeReason,
    };
    if (!r12Recovered.state || !isLive(r12Recovered.state)) {
      fail("R12", "a subscriber_lagged close stranded the page instead of re-attaching", evidence.r12);
    } else {
      if (evidence.r12.oldCloseReason !== "subscriber_lagged" || evidence.r12.oldLive) fail("R12", "the evicted attachment was not closed with subscriber_lagged", evidence.r12);
      if (evidence.r12.attachments !== 1 || evidence.r12.websockets !== 1 || evidence.r12.handleRequests < 1) fail("R12", "the page did not re-mint and re-attach exactly once", evidence.r12);
      if (evidence.r12.newMode !== "CONTROL" || !evidence.r12.newLive || evidence.r12.newCloseReason) fail("R12", "the re-attached session is not live in control mode", evidence.r12);
      if (!evidence.r12.strips.some((text) => text.includes("Reconnecting to alpha"))) fail("R12", "no reconnecting strip was shown during the re-attach", evidence.r12);
      await tabA.type("w");
      await delay(150);
      const typed = await lastAttachment();
      evidence.r12.inputs = typed.inputs.slice();
      if (!typed.inputs.includes("w")) fail("R12", "input after the re-attach was not delivered", evidence.r12);
    }

    // --- console triage ---------------------------------------------------------
    const tolerated = (entry) => /WebSocket connection to .*\/ws.*failed/.test(entry.text) && /410|Error during WebSocket handshake|Unexpected response code/.test(entry.text);
    const consoleFindings = tabs.flatMap((tab) => tab.console).filter((entry) => !tolerated(entry));
    if (consoleFindings.length !== 0) fail("console", "unexpected console entries", consoleFindings);

    if (failures.length !== 0) {
      process.stdout.write(JSON.stringify({ status: "FAIL", failures, evidence }, null, 2) + "\n");
      throw new Error(`unified reopen gate RED:\n${failures.join("\n")}`);
    }
    process.stdout.write(JSON.stringify({ status: "PASS", evidence }) + "\n");
  } finally {
    for (const tab of tabs) { try { tab.cdp.close(); } catch { /* gone */ } }
    const waitForChromeExit = (timeoutMs) => new Promise((resolve) => {
      if (chromeExit !== null) { resolve(true); return; }
      const timer = setTimeout(() => resolve(false), timeoutMs);
      chrome.once("exit", () => { clearTimeout(timer); resolve(true); });
    });
    chrome.kill("SIGTERM");
    if (!(await waitForChromeExit(5_000))) {
      chrome.kill("SIGKILL");
      await waitForChromeExit(5_000);
    }
    await fixture.close();
    fs.rmSync(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  }
}

main().catch((error) => {
  console.error(error.stack || String(error));
  process.exitCode = 1;
});
