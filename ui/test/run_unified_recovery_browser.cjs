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
//                           OFFLINE with Reconnect, then recovery on the
//                           browser's online signal and, without any signal,
//                           on the slow probe;
//   lag_burst               a view evicted over and over stops with Reconnect;
//   restored_page           a page back from the back/forward cache reattaches.
//
// The console is captured on every page. The only tolerated entries are
// Chromium's reports of requests this gate itself aborts while the network is
// cut, and only inside that phase.

const path = require("path");
const { startFixture } = require("./unified_reopen_fixture.cjs");
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
      await page.route("**/api/**", (route) => (cut ? route.abort("internetdisconnected") : route.continue()));
      phase = "network-cut";
      await control({ closeLive: "websocket_read" });
      await notice(page, "reconnect_offline", FAST_PHASE_BOUND_MS);
      const offline = await page.evaluate(STATE);
      assert(offline.reconnectVisible, "OFFLINE offered no Reconnect");
      assert(offline.screen.includes("fixture-live"), "OFFLINE discarded the retained output");
      const count = (await attachments()).length;
      cut = false;
      phase = "product";
      if (signal) {
        await page.evaluate(() => window.dispatchEvent(new Event("online")));
        await live(page, "an OFFLINE page after the online signal", 5_000);
      } else {
        await live(page, "an OFFLINE page with no signal at all", OFFLINE_PROBE_BOUND_MS);
      }
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
  } finally {
    if (context) await context.close();
    await browser.close();
    await fixture.close();
  }
  assert(findings.length === 0, `console/page findings:\n${findings.join("\n")}`);
  console.log("unified recovery browser gate: PASS");
}

main().catch((error) => { console.error(error && error.stack || error); process.exitCode = 1; });
