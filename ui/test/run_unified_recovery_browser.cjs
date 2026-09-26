"use strict";

// Unified connection recovery gate.
//
// The operator's contract: on a poor connection an open terminal comes back on
// its own, and whenever it cannot, it says so and offers a way back. Each
// scenario drives the REAL bundled page in real Chromium against the reopen
// fixture's handles, control lease, takeover offers and broker automaton, and
// breaks the connection the way a network or the server actually does:
//
//   takeover_then_drop      a page that took control is dropped, both after and
//                           before its takeover commits, and must reattach;
//   server_proof_expiry     the front door ends a page that stopped proving
//                           liveness (browser_liveness): loss, not a verdict;
//   abandoned_attempt       a reconnect attempt that times out must really close
//                           its socket, so it cannot hold the lease against the
//                           next attempt and force a takeover;
//   offline_then_back       the network stays down past the fast retry phase:
//                           OFFLINE with Reconnect, a probe that leaves open
//                           details alone, then recovery on the browser's
//                           online signal and, without any signal, on the
//                           slow probe;
//   lag_burst               a view evicted over and over stops with Reconnect;
//   restored_page           a page back from the back/forward cache reattaches,
//                           but one whose control moved elsewhere stays stopped;
//   hidden_recovery         a hidden page recovering on its own finds control
//                           held elsewhere and stops instead of taking it;
//   workspace_*             the same three stops inside a workspace pane: the
//                           pane shows the stop with Retry instead of an idle
//                           "Reattaching", OFFLINE recovers on the online
//                           signal, and a restored workspace reattaches its
//                           panes instead of leaving dead Retry buttons.
//
// The console is captured on every page. The only tolerated entries are
// Chromium's reports of requests this gate itself aborts while the network is
// cut, and only inside that phase.

const path = require("path");
const { startFixture } = require("./unified_reopen_fixture.cjs");
const { startWorkspaceFixture } = require("./workspace_fixture.cjs");
const chromiumPath = require("./browser_path.cjs");

const UI = path.resolve(__dirname, "..");
const MODULE = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
// The transport's fast phase at the widest jitter ends near 14 s; OFFLINE then
// probes every 15 s ± 20 %. These bounds leave room for both, never less.
const FAST_PHASE_BOUND_MS = 25_000;
const OFFLINE_PROBE_BOUND_MS = 25_000;
const LIVE_BOUND_MS = 15_000;

function assert(value, message) { if (!value) throw new Error(message); }

const STATE = () => {
  const notice = document.querySelector(".persea-unified-notice");
  const reconnect = document.querySelector(".persea-unified-notice__reconnect");
  return {
    phase: document.querySelector(".persea-unified-tag__dot")?.dataset.state ?? null,
    connection: document.querySelector(".persea-unified-connection")?.textContent ?? "",
    noticeVisible: notice ? !notice.hidden : false,
    code: document.querySelector(".persea-unified-notice__code")?.textContent ?? "",
    headline: document.querySelector(".persea-unified-notice__headline")?.textContent ?? "",
    reconnectVisible: reconnect ? !reconnect.hidden : false,
    screen: document.querySelector(".xterm-rows")?.textContent ?? "",
  };
};

async function main() {
  const playwright = require(MODULE);
  const fixture = await startFixture(UI);
  const workspaceFixture = await startWorkspaceFixture(UI);
  const browser = await playwright.chromium.launch({ executablePath: chromiumPath(), headless: true, args: ["--no-sandbox", "--disable-dev-shm-usage"] });
  const control = async (body) => {
    const response = await fetch(`${fixture.origin}/__fixture/control`, body === undefined ? {} : {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
    });
    return response.json();
  };
  const findings = [];
  let context;
  let scenario = "";
  let phase = "product";
  const newScenario = async (name) => {
    if (context) await context.close();
    await control({ reset: true });
    scenario = name;
    phase = "product";
    context = await browser.newContext({ viewport: { width: 1280, height: 844 } });
    context.on("page", (page) => {
      page.on("console", (message) => {
        const text = message.text();
        if (phase === "network-cut" && /net::ERR_INTERNET_DISCONNECTED/.test(text)) return;
        findings.push(`${scenario}/${phase}: console ${message.type()}: ${text}`);
      });
      page.on("pageerror", (error) => findings.push(`${scenario}/${phase}: page error: ${String(error)}`));
    });
  };
  const open = async () => {
    const inventory = await (await fetch(`${fixture.origin}/api/inventory`)).json();
    const handle = inventory.realms[0].servers[0].sessions[0].handles.control;
    const page = await context.newPage();
    await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({
      handle, mode: "control", history: "1000", name: "alpha", draft_scope: fixture.draftScope, engine: "unified-dev",
    })}`);
    await live(page, "the first attach");
    return page;
  };
  const live = (page, what, timeout = LIVE_BOUND_MS) => page.waitForFunction(() => (
    document.querySelector(".persea-unified-tag__dot")?.dataset.state === "live"
    && document.querySelector(".xterm-rows")?.textContent.includes("fixture-live")
    && document.querySelector(".persea-unified-notice")?.hidden
  ), null, { timeout }).catch(async () => { throw new Error(`${scenario}: ${what} never went live: ${JSON.stringify(await page.evaluate(STATE))}`); });
  const notice = (page, code, timeout) => page.waitForFunction((expected) => (
    !document.querySelector(".persea-unified-notice")?.hidden
    && document.querySelector(".persea-unified-notice__code")?.textContent === `code: ${expected}`
  ), code, { timeout }).catch(async () => { throw new Error(`${scenario}: notice ${code} never appeared: ${JSON.stringify(await page.evaluate(STATE))}`); });
  const lost = (page) => page.waitForFunction(() => document.querySelector(".persea-unified-tag__dot")?.dataset.state !== "live", null, { timeout: LIVE_BOUND_MS });
  const attachments = async () => (await control()).attachments;

  try {
    // A1: a takeover must not leave recovery disabled.
    await newScenario("takeover_then_drop");
    await open();
    const taker = await open();
    const before = await attachments();
    assert(before.some((attachment) => attachment.takeover && attachment.live), "the second page did not take control");
    await control({ closeLive: "websocket_read" });
    await lost(taker);
    await live(taker, "the page that took control, dropped after its takeover committed");
    const afterDrop = await attachments();
    assert(afterDrop.length > before.length, "the dropped takeover page did not open a fresh attachment");
    // Drop again before the replacement's COMMIT: hold PREPARE, drop, release.
    await control({ holdPrepareMs: 1_500, closeLive: "websocket_read" });
    await lost(taker);
    await control({ closeLive: "websocket_read" });
    await live(taker, "the takeover page dropped before COMMIT");

    // A2: the front door's proof expiry is recoverable.
    await newScenario("server_proof_expiry");
    const expired = await open();
    await control({ closeLive: "browser_liveness" });
    await lost(expired);
    await live(expired, "a page ended by server proof expiry");

    // A4: a timed-out attempt closes its socket with a code the browser sends.
    await newScenario("abandoned_attempt");
    const slow = await open();
    await control({ holdPrepareMs: 6_000, closeLive: "websocket_read" });
    await lost(slow);
    await live(slow, "the page after one slow attempt", LIVE_BOUND_MS + 6_000);
    const history = await attachments();
    const abandoned = history.find((attachment) => attachment.closeReason === "reconnect_attempt_timeout");
    assert(abandoned, `the timed-out attempt never closed its socket: ${JSON.stringify(history.map(({ id, closeReason, live, takeover }) => ({ id, closeReason, live, takeover })))}`);
    assert(abandoned.peerCloseCode >= 3000 && abandoned.peerCloseCode <= 4999, `the timed-out attempt closed with ${abandoned.peerCloseCode}, not an application code`);
    assert(!history.some((attachment) => attachment.takeover), "the abandoned attempt held the lease and forced a takeover");

    // A6: past the fast phase, OFFLINE with Reconnect; recovery on online, and
    // on the slow probe without any signal.
    for (const signal of [true, false]) {
      await newScenario(signal ? "offline_then_back_online_signal" : "offline_then_back_slow_probe");
      const page = await open();
      let cut = true;
      let mintsRefused = 0;
      await page.route("**/api/**", (route) => {
        if (!cut) return route.continue();
        // Every endpoint a reconnect attempt can start with: the source
        // re-mint, and the identity path's inventory read and adoption.
        if (["/api/attachment-handles", "/api/inventory", "/api/session-adoptions"].includes(new URL(route.request().url()).pathname)) mintsRefused += 1;
        return route.abort("internetdisconnected");
      });
      phase = "network-cut";
      await control({ closeLive: "websocket_read" });
      await notice(page, "reconnect_offline", FAST_PHASE_BOUND_MS);
      const offline = await page.evaluate(STATE);
      assert(offline.reconnectVisible, "OFFLINE offered no Reconnect");
      assert(offline.screen.includes("fixture-live"), "OFFLINE discarded the retained output");
      if (signal) {
        // A probe that fails again must leave what the operator opened alone.
        await page.locator(".persea-unified-tag").click();
        await page.waitForFunction(() => document.querySelector(".persea-unified-identity__details")?.hidden === false, null, { timeout: 5_000 });
        const refusedBefore = mintsRefused;
        await page.evaluate(() => window.dispatchEvent(new Event("focus")));
        for (const deadline = Date.now() + 6_000; mintsRefused === refusedBefore;) {
          assert(Date.now() < deadline, "a focus signal did not probe from OFFLINE");
          await page.waitForTimeout(50);
        }
        await notice(page, "reconnect_offline", 5_000);
        await page.waitForFunction(() => document.querySelector(".persea-unified-connection")?.textContent === "", null, { timeout: 5_000 });
        assert(await page.evaluate(() => document.querySelector(".persea-unified-identity__details")?.hidden === false), "an OFFLINE probe closed the details the operator had opened");
      }
      const count = (await attachments()).length;
      cut = false;
      if (signal) {
        await page.evaluate(() => window.dispatchEvent(new Event("online")));
        await live(page, "an OFFLINE page after the online signal", 5_000);
      } else {
        await live(page, "an OFFLINE page with no signal at all", OFFLINE_PROBE_BOUND_MS);
      }
      phase = "product";
      assert((await attachments()).length === count + 1, "recovery from OFFLINE did not open exactly one attachment");
      await page.unroute("**/api/**");
    }

    // A5: a view evicted over and over stops with a way back.
    await newScenario("lag_burst");
    const lagging = await open();
    for (let round = 0; round < 3; round += 1) {
      await control({ closeLive: "subscriber_lagged" });
      await lost(lagging);
      await live(lagging, `lag round ${round + 1}`);
    }
    await control({ closeLive: "subscriber_lagged" });
    await notice(lagging, "subscriber_lagged", LIVE_BOUND_MS);
    assert((await lagging.evaluate(STATE)).reconnectVisible, "the lag stop offered no Reconnect");
    await lagging.getByRole("button", { name: "Reconnect", exact: true }).click();
    await live(lagging, "Reconnect after the lag stop");

    // A page restored from the back/forward cache was detached on pagehide.
    await newScenario("restored_page");
    const restored = await open();
    const beforeRestore = (await attachments()).length;
    await restored.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true })));
    await lost(restored);
    await restored.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true })));
    await live(restored, "a restored page");
    assert((await attachments()).length === beforeRestore + 1, "a restored page did not open exactly one attachment");

    // A restore must not undo a stop: a page whose control moved elsewhere
    // stays stopped, so it can neither replay the refusal nor take control back.
    await newScenario("restored_after_displacement");
    const displaced = await open();
    const newOwner = await open();
    await notice(displaced, "control_displaced", LIVE_BOUND_MS);
    const beforeDisplacedRestore = (await attachments()).length;
    await displaced.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true })));
    await displaced.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true })));
    // Observation window for an absence: a resume mints and reattaches at once.
    await displaced.waitForTimeout(2_000);
    assert((await attachments()).length === beforeDisplacedRestore, "restoring a displaced page reattached it");
    await notice(displaced, "control_displaced", 1_000);
    await live(newOwner, "the page that holds control, after the other page's restore", 1_000);

    // A page recovering on its own must not take control from where the
    // operator moved it: here a hidden page comes back while another holds it.
    await newScenario("hidden_recovery");
    const away = await open();
    let awayCut = true;
    await away.route("**/api/**", (route) => (awayCut ? route.abort("internetdisconnected") : route.continue()));
    phase = "network-cut";
    await control({ closeLive: "websocket_read" });
    await lost(away);
    const holder = await open();
    await away.evaluate(() => {
      Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "hidden" });
      document.dispatchEvent(new Event("visibilitychange"));
    });
    awayCut = false;
    await notice(away, "lease_held", FAST_PHASE_BOUND_MS);
    // Chromium reports a request aborted just before the network returned a
    // moment later; leave the cut phase only once a request has succeeded.
    phase = "product";
    await live(holder, "the page that holds control, after the hidden page came back", 1_000);
    assert(!(await attachments()).some((attachment) => attachment.takeover), "a hidden recovering page took control");
    await away.unroute("**/api/**");

    // Workspace panes: the same stops, rendered by the pane.
    const workspaceControl = async (body) => {
      const response = await fetch(`${workspaceFixture.origin}/__fixture/control`, body === undefined ? {} : {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
      });
      return response.json();
    };
    const tree = { kind: "split", direction: "row", weights: [1, 1], children: ["ws01", "ws02"].map((name) => ({ kind: "leaf", session: { realm: "local", server: "private", name }, on_missing: "offer" })) };
    const cellState = (page, name) => page.evaluate((session) => {
      const cell = document.querySelector(`.ws-cell[data-ws-session="${session}"]`);
      return {
        state: cell?.dataset.wsState ?? null,
        code: cell?.querySelector(".ws-cell__state-code")?.textContent ?? "",
        actions: [...(cell?.querySelectorAll(".ws-cell__action") ?? [])].filter((action) => !action.closest("[hidden]")).map((action) => action.dataset.wsAffordance),
      };
    }, name);
    const workspaceLive = (page, what, timeout = LIVE_BOUND_MS) => page.waitForFunction(() => {
      const cells = [...document.querySelectorAll(".ws-cell")];
      return cells.length === 2 && cells.every((cell) => cell.dataset.wsState === "live" && cell.querySelector(".xterm-rows"));
    }, null, { timeout }).catch(async () => { throw new Error(`${scenario}: ${what}: panes never all went live: ${JSON.stringify([await cellState(page, "ws01"), await cellState(page, "ws02")])}`); });
    const cellStop = (page, name, code, timeout) => page.waitForFunction(([session, expected]) => {
      const cell = document.querySelector(`.ws-cell[data-ws-session="${session}"]`);
      return cell?.dataset.wsState === "failed" && cell.querySelector(".ws-cell__state-code")?.textContent?.includes(expected);
    }, [name, code], { timeout }).catch(async () => { throw new Error(`${scenario}: pane ${name} never showed ${code}: ${JSON.stringify(await cellState(page, name))}`); });
    const openWorkspace = async () => {
      await workspaceControl({ reset: true, sessions: ["ws01", "ws02"], workspace: { name: "ops", tree } });
      const page = await context.newPage();
      await page.goto(`${workspaceFixture.origin}/workspace?engine=unified-dev#name=ops`);
      await page.getByRole("button", { name: /^Open workspace/ }).click();
      await workspaceLive(page, "the first open");
      return page;
    };
    const workspaceAttachments = async (name) => (await workspaceControl()).attachments.filter((attachment) => attachment.session === name);

    await newScenario("workspace_lag_stop");
    const lagWorkspace = await openWorkspace();
    for (let round = 0; round < 3; round += 1) {
      await workspaceControl({ session: "ws01", closeLive: "subscriber_lagged" });
      await lagWorkspace.waitForFunction(() => document.querySelector('.ws-cell[data-ws-session="ws01"]')?.dataset.wsState !== "live");
      await workspaceLive(lagWorkspace, `lag round ${round + 1}`);
    }
    await workspaceControl({ session: "ws01", closeLive: "subscriber_lagged" });
    await cellStop(lagWorkspace, "ws01", "subscriber_lagged", LIVE_BOUND_MS);
    assert((await cellState(lagWorkspace, "ws01")).actions.includes("retry"), "the stopped pane offered no Retry");
    await lagWorkspace.locator('.ws-cell[data-ws-session="ws01"] [data-ws-affordance="retry"]').click();
    await workspaceLive(lagWorkspace, "Retry after the lag stop");

    await newScenario("workspace_offline");
    const offlineWorkspace = await openWorkspace();
    let handlesCut = true;
    // The whole API goes: a pane recovers through its source re-mint or, failing
    // that, through the inventory, which carries handles too.
    await offlineWorkspace.route("**/api/**", (route) => (handlesCut ? route.abort("internetdisconnected") : route.continue()));
    phase = "network-cut";
    await workspaceControl({ session: "ws01", closeLive: "websocket_read" });
    await cellStop(offlineWorkspace, "ws01", "reconnect_offline", FAST_PHASE_BOUND_MS);
    assert((await cellState(offlineWorkspace, "ws01")).actions.includes("retry"), "the OFFLINE pane offered no Retry");
    const ws02Before = (await workspaceAttachments("ws02")).length;
    handlesCut = false;
    await offlineWorkspace.evaluate(() => window.dispatchEvent(new Event("online")));
    await workspaceLive(offlineWorkspace, "an OFFLINE pane after the online signal", 5_000);
    phase = "product";
    assert((await workspaceAttachments("ws02")).length === ws02Before, "recovering one pane reattached its healthy neighbour");
    await offlineWorkspace.unroute("**/api/**");

    await newScenario("workspace_restored");
    const restoredWorkspace = await openWorkspace();
    const counts = [(await workspaceAttachments("ws01")).length, (await workspaceAttachments("ws02")).length];
    await restoredWorkspace.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true })));
    await restoredWorkspace.waitForFunction(() => [...document.querySelectorAll(".ws-cell")].every((cell) => cell.dataset.wsState !== "live"), null, { timeout: LIVE_BOUND_MS });
    await restoredWorkspace.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true })));
    await workspaceLive(restoredWorkspace, "a restored workspace");
    assert((await workspaceAttachments("ws01")).length === counts[0] + 1 && (await workspaceAttachments("ws02")).length === counts[1] + 1, "a restored workspace did not reattach each pane exactly once");
  } finally {
    if (context) await context.close();
    await browser.close();
    await fixture.close();
    await workspaceFixture.close();
  }
  assert(findings.length === 0, `console/page findings:\n${findings.join("\n")}`);
  console.log("unified recovery browser gate: PASS");
}

main().catch((error) => { console.error(error && error.stack || error); process.exitCode = 1; });
