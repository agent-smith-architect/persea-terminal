"use strict";

// regression test for the unified terminal scroll projection, run against a real
// headless Chromium across three viewport shapes.
//
// Two invariants, both of which the shipped geometry broke:
//   REACHABLE  every cell the grid renders can be scrolled into view, and every
//              buffer row the emulator holds can be projected.
//   TAIL       at follow-tail the projection shows the live screen, so the last
//              line written is the last line rendered, and it is visible.
//
// The regression set around them: a viewport larger than the grid must behave
// exactly as before, the anchor must survive a zoom, the alternate screen must
// still collapse its range, and the replay path must land at the tail.

const fs = require("fs");
const http = require("http");
const net = require("net");
const os = require("os");
const path = require("path");
const childProcess = require("child_process");

const UI = path.resolve(__dirname, "..");

// wide-short overflows the grid horizontally and vertically once zoomed;
// narrow-tall leaves the grid far smaller than the viewport, which is where a
// range built from total buffer height strands the live screen.
const SHAPES = [
  { name: "default", width: 1280, height: 800 },
  { name: "wide-short", width: 900, height: 620 },
  { name: "narrow-tall", width: 640, height: 1200 },
];

// Device pixel ratios. A Retina panel or a browser page-zoom step lets the
// renderer place rows on fractional CSS pixels, so the measured cell height
// stops being a whole number and top-accumulated row math drifts over a long
// scrollback. deviceScaleFactor 1 with integer cells never shows it.
const SCALES = [1, 1.25, 1.5, 2];

function assert(value, message) { if (!value) throw new Error(message); }
function delay(ms) { return new Promise((resolve) => setTimeout(resolve, ms)); }
function near(a, b, tolerance) { return Math.abs(a - b) <= tolerance; }

function browserBinary() {
  const candidate = require("./browser_path.cjs")();
  assert(candidate && path.isAbsolute(candidate), "set CHROME_BIN to an absolute Chromium-family browser executable");
  fs.accessSync(candidate, fs.constants.X_OK);
  return candidate;
}

async function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const port = server.address().port;
      server.close(() => resolve(port));
    });
  });
}

function requestJSON(url) {
  return new Promise((resolve, reject) => {
    const request = http.request(url, { method: "GET" }, (response) => {
      let body = "";
      response.setEncoding("utf8");
      response.on("data", (chunk) => { body += chunk; });
      response.on("end", () => { try { resolve(JSON.parse(body)); } catch (error) { reject(error); } });
    });
    request.on("error", reject);
    request.end();
  });
}

class CDP {
  constructor(url) {
    this.url = url;
    this.next = 1;
    this.pending = new Map();
  }
  async open() {
    this.ws = new WebSocket(this.url);
    await new Promise((resolve, reject) => {
      this.ws.addEventListener("open", resolve, { once: true });
      this.ws.addEventListener("error", reject, { once: true });
    });
    this.ws.addEventListener("message", (event) => {
      const message = JSON.parse(String(event.data));
      if (!message.id) return;
      const pending = this.pending.get(message.id);
      if (!pending) return;
      this.pending.delete(message.id);
      message.error ? pending.reject(new Error(message.error.message)) : pending.resolve(message.result);
    });
  }
  send(method, params = {}) {
    const id = this.next++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }
  close() { this.ws?.close(); }
}

async function evaluate(cdp, expression) {
  const result = await cdp.send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
  if (result.exceptionDetails) {
    throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text || "harness exception");
  }
  return result.result.value;
}

// REACHABLE, at the far corner of the range.
function checkReachable(shape, label, s, failures) {
  const where = `${shape.name}/${label}`;
  if (s.rightOvershoot > 1) failures.push(`${where}: columns stranded right of the viewport: ${JSON.stringify(s)}`);
  if (s.bottomOvershoot > 1) failures.push(`${where}: final row stranded below the viewport: ${JSON.stringify(s)}`);
  if (s.maxProjectableRow < s.baseY) {
    failures.push(`${where}: range cannot project every buffer row (max ${s.maxProjectableRow} < baseY ${s.baseY}): ${JSON.stringify(s)}`);
  }
  if (s.renderedRowCount !== 24) failures.push(`${where}: grid stopped rendering its full row count: ${JSON.stringify(s)}`);
}

// TAIL: the tail must show the live screen, not a window above it.
function checkTail(shape, label, s, failures) {
  const where = `${shape.name}/${label}`;
  if (s.lastRowText !== s.tailLine) {
    failures.push(`${where}: tail does not render the last written line (saw ${JSON.stringify(s.lastRowText)}, wanted ${JSON.stringify(s.tailLine)}): ${JSON.stringify(s)}`);
  }
  if (s.projectedRow < s.baseY - 0.01) {
    failures.push(`${where}: tail projected row ${s.projectedRow} sits above baseY ${s.baseY}: ${JSON.stringify(s)}`);
  }
  if (s.bottomOvershoot > 1) failures.push(`${where}: tail hides the bottom of the grid: ${JSON.stringify(s)}`);
}

// The operator's case: a normal window, no manual zoom, straight off the
// replay. Nothing here touches Fit or scrolls by hand — this is what the page
// does on its own when it lands at the tail.
async function runTailCondition(cdp, shape, scale, report, failures) {
  const label = `${shape.name}@dpr${scale}`;
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: shape.width, height: shape.height, deviceScaleFactor: scale, mobile: false,
  });
  await delay(250);
  await evaluate(cdp, "window.__harness.boot(200)");
  const settled = await evaluate(cdp, "window.__harness.state()");
  checkTail({ name: label }, "replay-tail", settled, failures);

  // A live line arriving while following must also land at the tail.
  await evaluate(cdp, "window.__harness.live('\\r\\nHARNESS-DPR-LIVE')");
  const live = await evaluate(cdp, "window.__harness.state()");
  if (live.lastRowText !== "HARNESS-DPR-LIVE") {
    failures.push(`${label}/live-tail: follow-tail did not render the live line (saw ${JSON.stringify(live.lastRowText)}): ${JSON.stringify(live)}`);
  }
  report[label] = { settled, live };
}

// Sweep the sub-pixel offsets the scroller's content box can be laid out at.
// The tail has to survive every one of them; which offset a given machine hits
// is decided by its font metrics, not by anything the page controls.
async function runSubPixelSweep(cdp, shape, report, failures) {
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: shape.width, height: shape.height, deviceScaleFactor: 1, mobile: false,
  });
  await delay(200);
  const offsets = [];
  for (let tenth = 0; tenth < 10; tenth++) offsets.push(tenth / 10);
  for (const offset of offsets) {
    const label = `${shape.name}+${offset.toFixed(1)}px`;
    const box = await evaluate(cdp, `window.__harness.setShellHeight(${shape.height - 40 + offset})`);
    await evaluate(cdp, "window.__harness.boot(200)");
    const settled = await evaluate(cdp, "window.__harness.state()");
    checkTail({ name: label }, "subpixel-tail", settled, failures);
    report[label] = { box, settled };
  }
  // Keep the trusted positive case causally distinct from the 24-row birth
  // geometry even when the expanded terminal interaction toolbar changes the stage budget.
  await evaluate(cdp, `window.__harness.setShellHeight(${Math.max(320, shape.height - 180)})`);
}

// Page zoom is the condition that makes the rendered cell height fractional,
// so top-accumulated row math drifts across a long scrollback.
const ZOOMS = [0.8, 0.9, 1.1, 1.25, 1.33, 1.5, 1.75];

async function runZoomSweep(cdp, shape, report, failures) {
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: shape.width, height: shape.height, deviceScaleFactor: 1, mobile: false,
  });
  await delay(200);
  for (const factor of ZOOMS) {
    const label = `${shape.name}@zoom${factor}`;
    await evaluate(cdp, `window.__harness.setZoom(${factor})`);
    await evaluate(cdp, "window.__harness.boot(200)");
    const settled = await evaluate(cdp, "window.__harness.state()");
    checkTail({ name: label }, "zoom-tail", settled, failures);
    report[label] = { settled };
  }
  await evaluate(cdp, "window.__harness.setZoom(1)");
}

// Explicit vertical Fit, in a real browser.
//
// The claim under test is mostly negative: nothing except one trusted click on
// one control may ever produce a RESIZE_REQUEST. Window resize, page zoom,
// font fit, the page's own ResizeObserver, admission and reconnect all have to
// send zero. The positive half is one request carrying the committed columns
// unchanged and the measured rows, and a committed geometry event that resizes
// the emulator without destroying what is on it.
async function runVerticalFit(cdp, shape, report, failures) {
  const label = `${shape.name}/vertical-fit`;
  const note = (message, detail) => failures.push(`${label}: ${message}: ${JSON.stringify(detail)}`);
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: shape.width, height: shape.height, deviceScaleFactor: 1, mobile: false,
  });
  await delay(250);

  // Admission sizes the emulator from the generation's birth geometry, and
  // sends nothing. A birth geometry unlike the constructor's default is used
  // first, so an admission that quietly kept the current size is a red.
  await evaluate(cdp, "window.__harness.clearSentFrames()");
  await evaluate(cdp, "window.__harness.bootWithGeometry(60, 100, 30)");
  const born = await evaluate(cdp, "window.__harness.fitState()");
  if (born.columns !== 100 || born.rows !== 30 || born.readout !== "100×30") {
    note("admission did not adopt the journal's birth geometry", born);
  }
  if (born.resizeRequests.length !== 0) note("admission produced a resize request", born.resizeRequests);
  await evaluate(cdp, "window.__harness.bootWithGeometry(200, 80, 24)");
  await evaluate(cdp, "window.__harness.clearSentFrames()");
  const admitted = await evaluate(cdp, "window.__harness.fitClickState()");
  if (admitted.columns !== 80 || admitted.rows !== 24) {
    note("admission did not size the emulator from the birth geometry", admitted);
  }
  if (admitted.readout !== "80×24") note("geometry readout is wrong at admission", admitted);
  if (admitted.accessibleName !== "Fit rows") note("accessible name is wrong", admitted);
  if (admitted.symbol !== "↕ Fit rows") note("the action is not the compact rows label", admitted);
  if (admitted.ariaLive !== "polite") note("the geometry readout is not announced", admitted);
  if (admitted.disabled || admitted.ariaDisabled !== "false") note("the action is not enabled on a committed control attachment", admitted);
  if (admitted.width < 44 || admitted.height < 44) note("the action is below the touch-target minimum", admitted);

  // Everything that must send nothing.
  await evaluate(cdp, "window.__harness.zoom('Zoom in', 2)");
  await evaluate(cdp, "window.__harness.zoom('Fit font', 1)");
  await evaluate(cdp, "window.__harness.zoom('Zoom out', 1)");
  await evaluate(cdp, `window.__harness.setShellHeight(${shape.height - 120})`);
  await evaluate(cdp, "window.__harness.setZoom(1.25)");
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: Math.max(360, shape.width - 240), height: Math.max(320, shape.height - 200), deviceScaleFactor: 1, mobile: false,
  });
  await delay(300);
  await evaluate(cdp, "window.__harness.setZoom(1)");
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: shape.width, height: shape.height, deviceScaleFactor: 1, mobile: false,
  });
  await delay(300);
  await evaluate(cdp, `window.__harness.setShellHeight(${shape.height})`);
  const untrusted = await evaluate(cdp, "window.__harness.clickFitHeightUntrusted()");
  if (untrusted.resizeRequests.length !== 0) {
    note("something other than a trusted click produced a resize request", untrusted.resizeRequests);
  }

  // Reconnect: a differently sized viewport must not make the session follow it.
  await evaluate(cdp, "window.__harness.bootWithGeometry(120, 80, 24)");
  const reconnected = await evaluate(cdp, "window.__harness.fitState()");
  if (reconnected.resizeRequests.length !== 0) {
    note("reconnect produced a resize request", reconnected.resizeRequests);
  }

  // The one path that may: a trusted click at the control's rendered position.
  await evaluate(cdp, "window.__harness.clearSentFrames()");
  // The harness owns the viewport seam directly here: changing the outer
  // viewport without touching xterm's 24-row screen gives Fit a real,
  // deterministic rows-only delta instead of relying on incidental chrome.
  await evaluate(cdp, "window.__harness.setViewportHeight(780)");
  const before = await evaluate(cdp, "window.__harness.fitClickState()");
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: before.x, y: before.y });
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: before.x, y: before.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: before.x, y: before.y, button: "left", buttons: 0, clickCount: 1 });
  const clicked = await evaluate(cdp, "window.__harness.settle()");
  const requests = clicked.resizeRequests;
  if (requests.length !== 1) {
    note("a trusted click did not produce exactly one request", { requests, before });
    return;
  }
  const request = requests[0];
  if (request.columns !== before.columns) {
    note("the browser changed columns", { request, before });
  }
  if (!(request.rows >= 8 && request.rows <= 120 && request.rows !== before.rows)) {
    note("the requested rows are not a real in-policy vertical change", { request, before });
  }
  if (request.columns * request.rows > 36000) note("the request exceeds the cell ceiling", request);
  if (clicked.disabled || clicked.ariaDisabled !== "true") {
    note("the pending action was not focusable and aria-disabled", clicked);
  }

  // A second click while one is pending must add nothing.
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: before.x, y: before.y });
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: before.x, y: before.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: before.x, y: before.y, button: "left", buttons: 0, clickCount: 1 });
  const pending = await evaluate(cdp, "window.__harness.settle()");
  if (pending.resizeRequests.length !== 1) {
    note("a second click while pending produced another request", pending.resizeRequests);
  }

  // The committed geometry event: buffer-preserving, never a reset.
  const beforeGeometry = await evaluate(cdp, "window.__harness.state()");
  const applied = await evaluate(cdp, `window.__harness.applyCommittedGeometry(${request.columns}, ${request.rows})`);
  if (applied.columns !== request.columns || applied.rows !== request.rows) {
    note("the committed geometry was not applied to the emulator", applied);
  }
  if (applied.readout !== `${request.columns}×${request.rows}`) note("the readout did not follow the committed geometry", applied);
  if (applied.disabled || applied.ariaDisabled !== "true" || applied.title !== `Height already fits (${request.rows}).`) {
    note("the committed already-fit action was not focusable with an exact reason", applied);
  }
  const afterGeometry = await evaluate(cdp, "window.__harness.state()");
  if (afterGeometry.tailLine !== beforeGeometry.tailLine || afterGeometry.lastRowText !== beforeGeometry.tailLine) {
    note("the transcript did not survive the committed geometry", { beforeGeometry, afterGeometry });
  }
  if (afterGeometry.baseY <= 0) {
    note("scrollback was destroyed by the committed geometry", afterGeometry);
  }

  // Alternate screen: a resize must carry both buffers, and leaving the
  // alternate screen must still restore the normal one.
  const alternate = await evaluate(cdp, "window.__harness.enterAlternate()");
  if (alternate.bufferType !== "alternate") note("alternate screen did not activate", alternate);
  const alternateGeometry = await evaluate(cdp, `window.__harness.applyCommittedGeometry(${request.columns}, ${Math.max(8, request.rows - 3)})`);
  if (alternateGeometry.bufferType !== "alternate" || alternateGeometry.rows !== Math.max(8, request.rows - 3)) {
    note("a committed geometry in the alternate screen did not apply cleanly", alternateGeometry);
  }
  const restored = await evaluate(cdp, "window.__harness.leaveAlternate()");
  if (restored.bufferType !== "normal" || restored.lastRowText !== beforeGeometry.tailLine) {
    note("leaving the alternate screen after a resize lost the normal buffer", { restored, beforeGeometry });
  }

  report[label] = { admitted, reconnected, request, applied, afterGeometry, alternateGeometry, restored };
  await evaluate(cdp, "window.__harness.resetViewportHeight()");
  await evaluate(cdp, "window.__harness.setViewPopover(false)");
}

// The pending-Fit input seal, raced in a real browser.
//
// A resize is a cut, and the attachment's protocol reducer does not accept
// input during one. The required behaviour is a bounded local seal: from the
// moment a trusted click is accepted until the committed geometry event has
// been applied, keystrokes are dropped — never queued, never replayed, never
// resent across the cut, a refusal, or a transport loss. Anything the operator
// typed before the click must still be on the wire, in order, ahead of the
// request; anything typed after the geometry is applied must be sent once, in
// order, behind it.
async function runPendingFitInputSeal(cdp, shape, report, failures) {
  const label = `${shape.name}/input-seal`;
  const note = (message, detail) => failures.push(`${label}: ${message}: ${JSON.stringify(detail)}`);
  const inputsIn = (order) => order.filter((frameType) => frameType === "INPUT").length;
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: shape.width, height: shape.height, deviceScaleFactor: 1, mobile: false,
  });
  await delay(250);
  await evaluate(cdp, "window.__harness.bootWithGeometry(120, 80, 24)");
  await evaluate(cdp, "window.__harness.clearSentFrames()");
  // sealedInputBytes is cumulative for the page instance, which outlives one
  // shape, so every claim about it here is a delta.
  const baseline = await evaluate(cdp, "window.__harness.inputState()");

  // Accepted pre-click input. It must be on the wire, in order, ahead of the
  // request: xterm emits one INPUT frame per character.
  const before = await evaluate(cdp, "window.__harness.type('BEFORE')");
  if (before.sentText !== "BEFORE" || before.inputFrameCount !== 6 || before.sealedInputBytes !== baseline.sealedInputBytes) {
    note("pre-click input was not sent normally", { baseline, before });
    return;
  }

  const control = await evaluate(cdp, "window.__harness.fitClickState()");
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: control.x, y: control.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: control.x, y: control.y, button: "left", buttons: 0, clickCount: 1 });
  const clicked = await evaluate(cdp, "window.__harness.inputState()");
  if (clicked.resizeRequests.length !== 1) {
    note("a trusted click did not produce exactly one request", clicked);
    return;
  }
  if (clicked.fitPending !== true) {
    note("the click did not seal input synchronously", clicked);
    return;
  }
  const boundaryIndex = clicked.frameOrder.indexOf("RESIZE_REQUEST");
  if (boundaryIndex !== 6 || inputsIn(clicked.frameOrder.slice(0, boundaryIndex)) !== 6) {
    note("pre-click input is not ordered ahead of the request", clicked);
  }

  // Paced input across the pending window. None of it may reach the wire, and
  // none of it may be retained anywhere that could replay it.
  let during = clicked;
  for (const chunk of ["D", "U", "R", "I", "N", "G"]) {
    during = await evaluate(cdp, `window.__harness.type(${JSON.stringify(chunk)})`);
  }
  if (during.sentText !== "BEFORE") {
    note("input was sent while a resize was pending", during);
  }
  if (during.inputFrameCount !== 6) {
    note("the seal let extra input frames through", during);
  }
  if (during.sealedInputBytes - baseline.sealedInputBytes !== 6) {
    note("the seal did not account for exactly the dropped bytes", { baseline, during });
  }
  if (during.resizeRequests.length !== 1) {
    note("input during the pending window produced another resize request", during);
  }

  // The committed geometry event opens the seal. It must not replay anything.
  const request = clicked.resizeRequests[0];
  const applied = await evaluate(cdp, `window.__harness.applyCommittedGeometry(${request.columns}, ${request.rows})`);
  if (applied.rows !== request.rows) note("the committed geometry was not applied", applied);
  const resumed = await evaluate(cdp, "window.__harness.inputState()");
  if (resumed.fitPending !== false) note("the seal did not open on the committed geometry", resumed);
  if (resumed.sentText !== "BEFORE" || resumed.inputFrameCount !== 6) {
    note("applying the geometry replayed sealed keystrokes", resumed);
  }

  const after = await evaluate(cdp, "window.__harness.type('AFTER')");
  if (after.sentText !== "BEFOREAFTER" || after.inputFrameCount !== 11) {
    note("post-commit input was not accepted exactly once, in order", after);
  }
  if (after.frameOrder.indexOf("RESIZE_REQUEST") !== boundaryIndex) {
    note("the boundary moved relative to pre-click input", after);
  }
  if (after.frameOrder.filter((frameType) => frameType !== "INPUT" && frameType !== "RESIZE_REQUEST").length !== 0) {
    note("an unexpected frame type entered the record", after.frameOrder);
  }

  // A refused send clears the seal without replaying anything. Re-admit at the
  // birth geometry first: a click that measures the height already committed is
  // a no-op by design and would make the rest of this vacuous.
  await evaluate(cdp, "window.__harness.bootWithGeometry(60, 80, 24)");
  await evaluate(cdp, "window.__harness.clearSentFrames()");
  await evaluate(cdp, "window.__harness.setPortResult('CLOSED')");
  const refusedControl = await evaluate(cdp, "window.__harness.fitClickState()");
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: refusedControl.x, y: refusedControl.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: refusedControl.x, y: refusedControl.y, button: "left", buttons: 0, clickCount: 1 });
  await evaluate(cdp, "window.__harness.setPortResult('ACCEPTED')");
  const refused = await evaluate(cdp, "window.__harness.inputState()");
  if (refused.resizeRequests.length !== 1) {
    note("the refusal case did not actually attempt a request", refused);
  }
  if (refused.fitPending !== false) {
    note("a refused request left input sealed", refused);
  }
  const afterRefusal = await evaluate(cdp, "window.__harness.type('REFUSED')");
  if (afterRefusal.sentText !== "REFUSED") {
    note("input did not resume after a refused request, or sealed bytes were replayed", afterRefusal);
  }

  // Transport loss clears it too, and a reconnect carries nothing forward.
  await evaluate(cdp, "window.__harness.bootWithGeometry(60, 80, 24)");
  await evaluate(cdp, "window.__harness.clearSentFrames()");
  const lostControl = await evaluate(cdp, "window.__harness.fitClickState()");
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: lostControl.x, y: lostControl.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: lostControl.x, y: lostControl.y, button: "left", buttons: 0, clickCount: 1 });
  await evaluate(cdp, "window.__harness.type('LOST')");
  const lost = await evaluate(cdp, "window.__harness.inputState()");
  if (lost.resizeRequests.length !== 1 || lost.fitPending !== true) {
    note("the transport-loss case did not reach a sealed pending request", lost);
  }
  if (lost.sentText !== "") {
    note("input was sent while a resize was pending, before transport loss", lost);
  }
  await evaluate(cdp, "window.__harness.transportClosed()");
  const closed = await evaluate(cdp, "window.__harness.inputState()");
  if (closed.fitPending !== false) note("transport loss left input sealed", closed);
  await evaluate(cdp, "window.__harness.bootWithGeometry(60, 80, 24)");
  const reconnected = await evaluate(cdp, "window.__harness.inputState()");
  if (inputsIn(reconnected.frameOrder) !== inputsIn(closed.frameOrder)) {
    note("a reconnect replayed keystrokes sealed by a lost request", { closed, reconnected });
  }

  report[label] = { baseline, before, clicked, during, resumed, after, refused, afterRefusal, lost, closed, reconnected };
  await evaluate(cdp, "window.__harness.setViewPopover(false)");
}

async function runShape(cdp, shape, report, failures) {
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: shape.width, height: shape.height, deviceScaleFactor: 1, mobile: false,
  });
  await delay(250);
  await evaluate(cdp, "window.__harness.boot(200)");
  await evaluate(cdp, "window.__harness.zoom('Fit font', 1)");

  // Replay path: acceptPrepare ends with syncNativeScroll(true).
  const afterReplay = await evaluate(cdp, "window.__harness.state()");
  checkTail(shape, "after-replay", afterReplay, failures);

  const fitted = await evaluate(cdp, "window.__harness.toFarCorner()");
  checkReachable(shape, "fitted", fitted, failures);
  checkTail(shape, "fitted-tail", fitted, failures);

  // Live append while following must stay at the tail.
  await evaluate(cdp, "window.__harness.live('\\r\\nHARNESS-LIVE-APPEND')");
  const afterLive = await evaluate(cdp, "window.__harness.state()");
  if (afterLive.lastRowText !== "HARNESS-LIVE-APPEND") {
    failures.push(`${shape.name}/live-append: follow-tail did not reveal live output: ${JSON.stringify(afterLive)}`);
  }

  // Anchor preservation: the buffer row at the top of the grid must survive a
  // zoom, which is the only thing that makes scrolled-up reading usable.
  const parked = await evaluate(cdp, `window.__harness.scrollTo(${Math.floor(afterLive.cellHeight * 40 + afterLive.cellHeight * 0.25)})`);
  await evaluate(cdp, "window.__harness.zoom('Zoom in', 1)");
  const zoomedAnchor = await evaluate(cdp, "window.__harness.state()");
  if (zoomedAnchor.topRowText !== parked.topRowText) {
    failures.push(`${shape.name}/anchor: zoom moved the top buffer row from ${JSON.stringify(parked.topRowText)} to ${JSON.stringify(zoomedAnchor.topRowText)}`);
  }
  await evaluate(cdp, "window.__harness.zoom('Fit font', 1)");

  // Zoom past the fit, then demand reachability on both axes.
  await evaluate(cdp, "window.__harness.zoom('Zoom in', 14)");
  const zoomed = await evaluate(cdp, "window.__harness.toFarCorner()");
  const overflowsBoth = zoomed.gridWidth > zoomed.clientWidth && zoomed.gridHeight > zoomed.clientHeight;
  if (overflowsBoth) {
    checkReachable(shape, "zoomed", zoomed, failures);
    if (!(zoomed.horizontalRange > 0)) {
      failures.push(`${shape.name}/zoomed: no horizontal range for a grid wider than the viewport: ${JSON.stringify(zoomed)}`);
    }
    // Horizontal scroll must not disturb the vertical projection.
    const atLeft = await evaluate(cdp, `window.__harness.scrollTo(${zoomed.scrollTop}, 0)`);
    if (!near(atLeft.hostTop, zoomed.hostTop, 0.5) || atLeft.projectedRow !== zoomed.projectedRow) {
      failures.push(`${shape.name}/zoomed: horizontal scroll moved the vertical projection: ${JSON.stringify({ zoomed, atLeft })}`);
    }
  }
  await evaluate(cdp, "window.__harness.zoom('Fit font', 1)");

  // Top of the range must project buffer row 0.
  const atTop = await evaluate(cdp, "window.__harness.toTop()");
  if (atTop.projectedRow > 0.01) {
    failures.push(`${shape.name}/top: range top does not project buffer row 0: ${JSON.stringify(atTop)}`);
  }

  // Alternate screen: no transcript range, pinned to the top.
  await evaluate(cdp, "window.__harness.toFarCorner()");
  const alternate = await evaluate(cdp, "window.__harness.enterAlternate()");
  if (alternate.bufferType !== "alternate") {
    failures.push(`${shape.name}/alternate: buffer did not switch: ${JSON.stringify(alternate)}`);
  } else if (alternate.verticalRange > 1 || alternate.scrollTop > 1) {
    failures.push(`${shape.name}/alternate: alternate screen kept a transcript range: ${JSON.stringify(alternate)}`);
  }
  const restored = await evaluate(cdp, "window.__harness.leaveAlternate()");
  if (restored.bufferType !== "normal" || restored.verticalRange <= 1) {
    failures.push(`${shape.name}/alternate-exit: normal transcript range was not restored: ${JSON.stringify(restored)}`);
  }

  report[shape.name] = { afterReplay, fitted, afterLive, parked, zoomedAnchor, zoomed, atTop, alternate, restored };
}

// every typed failure state renders visible text — never a blank
// page — and control-held closes offer a working take-control claim. Drives
// the REAL page's transportClosed/reconnectStatus surface and asserts the DOM.
async function runFailureSurface(cdp, report, failures) {
  const note = (message, detail) => failures.push(`failure-surface/${message}: ${JSON.stringify(detail)}`);

  // Terminal broker-typed refusal: visible notice, dashboard way out, retry
  // loop stopped through port.detach, no claim affordance.
  await evaluate(cdp, "window.__harness.boot(10)");
  await evaluate(cdp, "window.__harness.clearFailureRecords()");
  const unavailable = await evaluate(cdp, "window.__harness.closeTransport('unified_unavailable')");
  if (unavailable.noticeHidden || unavailable.noticeText.length === 0) {
    note("unified_unavailable rendered a blank page", unavailable);
  }
  if (!unavailable.codeLine.includes("unified_unavailable") || unavailable.dashboardHref !== "/") {
    note("unified_unavailable lost its code or dashboard affordance", unavailable);
  }
  if (!unavailable.detachCalls.includes("unified_unavailable")) {
    note("terminal refusal did not stop the reconnect loop", unavailable);
  }
  if (!unavailable.takeControlHidden) {
    note("non-policy refusal offered a takeover claim", unavailable);
  }

  // A fresh admission clears the notice.
  await evaluate(cdp, "window.__harness.boot(10)");
  const readmitted = await evaluate(cdp, "window.__harness.failureState()");
  if (!readmitted.noticeHidden) note("a fresh admission left the failure notice up", readmitted);

  // Control-held: reopening is explicit operator intent and the operator is
  // singular, so a lease held by another live attachment (the operator's own
  // other tab/device) is claimed AUTOMATICALLY — no dead-end notice, no click.
  // A reconnecting strip shows while the claim resolves and the endpoint
  // reaches the port on its own. (control_displaced/takeover_superseded keep
  // the manual button; those are covered by the reopen browser gate.)
  await evaluate(cdp, "window.__harness.clearFailureRecords()");
  await evaluate(cdp, "window.__harness.closeTransport('lease_held')");
  let claimed;
  for (let attempt = 0; attempt < 100; attempt++) {
    claimed = await evaluate(cdp, "window.__harness.failureState()");
    if (claimed.portTakeovers > 0) break;
    await delay(25);
  }
  if (!claimed || claimed.portTakeovers !== 1 || claimed.takeoverClaims.length !== 1) {
    note("lease_held did not auto-claim control through exactly one takeover", claimed);
  }
  if (claimed && !claimed.noticeHidden) note("lease_held auto-claim left a terminal notice up", claimed);
  if (claimed && claimed.detachCalls.length !== 0) note("lease_held auto-claim stopped the transport", claimed);
  if (claimed && (claimed.connectionText.length === 0 || !claimed.connectionVisible)) {
    note("lease_held auto-claim showed no reconnecting strip", claimed);
  }

  // Transient loss: NO terminal notice; a visible reconnecting state instead,
  // and retry exhaustion lands terminally with its own visible text.
  await evaluate(cdp, "window.__harness.boot(10)");
  await evaluate(cdp, "window.__harness.clearFailureRecords()");
  const dropped = await evaluate(cdp, "window.__harness.closeTransport('websocket_1006')");
  if (!dropped.noticeHidden) note("a transient loss rendered the terminal notice", dropped);
  if (dropped.connectionText.length === 0 || !dropped.connectionVisible) {
    note("a transient loss rendered no visible reconnecting state", dropped);
  }
  if (dropped.detachCalls.length !== 0) note("a transient loss stopped the retry loop", dropped);
  const waiting = await evaluate(cdp, "window.__harness.reconnectStatus('WAITING', 2)");
  if (!waiting.connectionText.includes("2")) note("the reconnect attempt count is invisible", waiting);
  const exhausted = await evaluate(cdp, "window.__harness.reconnectStatus('OFFLINE', 6)");
  if (exhausted.noticeHidden || exhausted.noticeText.length === 0 || !exhausted.codeLine.includes("reconnect_offline")) {
    note("a spent fast phase rendered no visible offline state", exhausted);
  }
  if (exhausted.connectionText.length !== 0) {
    note("retry exhaustion left the reconnecting strip up alongside the notice", exhausted);
  }

  // Recovery control: the surface resets for the next admission.
  await evaluate(cdp, "window.__harness.boot(10)");
  const recovered = await evaluate(cdp, "window.__harness.failureState()");
  if (!recovered.noticeHidden || recovered.connectionText.length !== 0) {
    note("the failure surface did not reset on re-admission", recovered);
  }

  report.failureSurface = { unavailable, readmitted, leaseHeldAutoClaim: claimed, dropped, waiting, exhausted, recovered };
}

// Coarse-pointer emulation, the same route the key-input regression test uses.
// Touch emulation is what actually flips the pointer/hover media features in
// Chromium (measured: setEmulatedMedia's pointer features alone are inert),
// and it fires 'change' on MediaQueryLists the page created earlier. The page
// decides key-bar presence and the keyboard inset by the pointer, so the
// mobile scenarios flip it on and restore a fine pointer afterwards.
async function emulateCoarsePointer(cdp, coarse) {
  await cdp.send("Emulation.setTouchEmulationEnabled", coarse
    ? { enabled: true, maxTouchPoints: 5 }
    : { enabled: false, maxTouchPoints: 1 });
  await cdp.send("Emulation.setEmulatedMedia", {
    features: coarse
      ? [
        { name: "pointer", value: "coarse" },
        { name: "any-pointer", value: "coarse" },
        { name: "hover", value: "none" },
        { name: "any-hover", value: "none" },
      ]
      : [],
  });
}

// The key bar on narrow phones. The row is exactly eight keys — Esc, ⇥,
// the Ctrl latch, the arrow cluster, the Keys switch; no overflow toggle,
// the remaining keys live on the quick-actions sheet — visible and tappable
// at 390 and 320 CSS pixels with zero horizontal page overflow, each a 44px
// target at 390. Below the fit floor the row scrolls within itself; the page
// never gains a horizontal scrollbar.
async function runMobileKeyBarFit(cdp, report, failures) {
  const note = (message, detail) => failures.push(`mobile-keybar/${message}: ${JSON.stringify(detail)}`);
  await emulateCoarsePointer(cdp, true);
  await evaluate(cdp, "window.__harness.resetShellHeight()");
  for (const width of [390, 320]) {
    await cdp.send("Emulation.setDeviceMetricsOverride", { width, height: 844, deviceScaleFactor: 3, mobile: true });
    await delay(250);
    await evaluate(cdp, "window.__harness.announceViewport()");
    await evaluate(cdp, "window.__harness.boot(50)");
    // The bar shows only while the software keyboard is open now, so the
    // geometry is measured in that state.
    await evaluate(cdp, "window.__harness.setViewportInset(508)");
    const bar = await evaluate(cdp, "window.__harness.keyBarState()");
    if (bar.hidden) {
      note(`${width}: key bar hidden while the software keyboard is open`, bar);
      await evaluate(cdp, "window.__harness.setViewportInset(null)");
      continue;
    }
    if (bar.keyCount !== 8) note(`${width}: the bar does not carry exactly its eight keys`, bar);
    const order = (bar.keys || []).map((key) => key.label).join(" ");
    if (order !== "Esc ⇥ Ctrl ← ↓ ↑ → Keys") note(`${width}: key order drifted`, { order });
    if (!bar.keys.some((key) => key.label === "Keys" && key.width >= 44 && key.height >= 44)) note(`${width}: direct Keys target missing`, bar);
    if (!bar.ctrl || bar.toggle) note(`${width}: ctrl latch absent or an overflow toggle rendered`, bar);
    const stray = (bar.keys || []).filter((key) => key.left < -0.5 || key.right > width + 0.5);
    if (width >= 390 && stray.length !== 0) note(`${width}: primary keys land outside the viewport`, { stray, viewportWidth: bar.viewportWidth });
    const short = (bar.keys || []).filter((key) => key.height < 44 || key.width < 44);
    if (short.length !== 0) note(`${width}: touch target below the 44px law`, { short });
    // The iOS focus auto-zoom trigger: a focused input under 16px. The helper
    // textarea otherwise carries the UA's default ~13px monospace font.
    if (bar.helperFontSize !== "16px") note(`${width}: helper textarea font invites iOS auto-zoom`, { helperFontSize: bar.helperFontSize });
    if (width >= 390 && bar.rowScrollWidth > bar.rowClientWidth + 1) note(`${width}: primary row needs scrolling to reach its keys`, bar);
    if (bar.shellScrollWidth > bar.shellClientWidth + 1 || bar.pageScrollWidth > width + 1) {
      note(`${width}: the page gained horizontal overflow`, bar);
    }
    report[`mobile-keybar@${width}`] = bar;
    await evaluate(cdp, "window.__harness.setViewportInset(null)");
  }
  // Below the fit floor: the row must overflow (otherwise this probe has gone
  // vacuous and needs a narrower width), and it scrolls within itself — the
  // page still must not scroll sideways.
  await cdp.send("Emulation.setDeviceMetricsOverride", { width: 280, height: 653, deviceScaleFactor: 3, mobile: true });
  await delay(250);
  await evaluate(cdp, "window.__harness.announceViewport()");
  await evaluate(cdp, "window.__harness.boot(50)");
  await evaluate(cdp, "window.__harness.setViewportInset(350)");
  const narrow = await evaluate(cdp, "window.__harness.keyBarState()");
  if (!(narrow.rowScrollWidth > narrow.rowClientWidth + 1)) {
    note("280: the row no longer overflows — the row-scroll probe is vacuous", narrow);
  } else if (narrow.toggle) {
    note("280: an overflow toggle rendered", narrow);
  }
  if (narrow.shellScrollWidth > narrow.shellClientWidth + 1 || narrow.pageScrollWidth > 281) {
    note("280: the page gained horizontal overflow", narrow);
  }
  report["mobile-keybar@280"] = narrow;
  await evaluate(cdp, "window.__harness.setViewportInset(null)");
  await emulateCoarsePointer(cdp, false);
}

// The software keyboard. One state machine drives the shell's inset pin and
// the key bar's visibility together: pinned and visible exactly while the
// keyboard reads open, stylesheet height and a hidden bar the moment it does
// not — through every dismissal path, including the ones iOS announces badly
// (no vv resize, a drifted window.innerHeight, a pinch while pinned). The
// fitted font must not move through any transition.
async function runMobileKeyboardInset(cdp, report, failures) {
  const note = (message, detail) => failures.push(`keyboard-inset/${message}: ${JSON.stringify(detail)}`);
  const font = (label, s, reference) => {
    if (Math.abs(s.fontSize - reference.fontSize) > 0.001) note(`${label} changed the fitted font`, { before: reference.fontSize, after: s.fontSize });
  };
  await emulateCoarsePointer(cdp, true);
  await evaluate(cdp, "window.__harness.setViewPopover(false)");
  await cdp.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await delay(250);
  await evaluate(cdp, "window.__harness.resetShellHeight()");
  // Seed the page's resting high-water mark, the way a real visualViewport
  // announces the post-rotation window.
  await evaluate(cdp, "window.__harness.announceViewport()");
  // 40 columns: width leaves fit headroom, so under the keyboard the HEIGHT
  // ratio is the binding one — the configuration where an ungated refit would
  // actually flap the font. 80 columns would mask the gate behind the floor.
  await evaluate(cdp, "window.__harness.bootWithGeometry(200, 40, 24)");
  await evaluate(cdp, "window.__harness.zoom('Fit font', 1)");
  const before = await evaluate(cdp, "window.__harness.insetState()");
  if (!before.keybarHidden) note("key bar showed before the keyboard opened", before);
  if (before.styleHeight !== "") note("shell height pinned before any keyboard", before);
  checkTail({ name: "keyboard-inset" }, "before-open", before, failures);

  // Closed-keyboard access is deliberate: Quick actions opens Keys without
  // focusing native input or sending terminal bytes, and Keys closes it again.
  const accessBefore = await evaluate(cdp, "(() => { window.__keysAccessFocus = document.activeElement; return window.__harness.inputTexts(); })()");
  const tapControl = async (selector) => {
    const point = await evaluate(cdp, `(() => { const node = document.querySelector(${JSON.stringify(selector)}); if (!node || !node.getClientRects().length) return null; node.scrollIntoView({ block: "nearest", inline: "nearest" }); const rect = node.getBoundingClientRect(); return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2, width: rect.width, height: rect.height }; })()`);
    assert(point && point.width >= 44 && point.height >= 44, `keyboard-inset/closed-keyboard control unavailable: ${selector} ${JSON.stringify(point)}`);
    await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: point.x, y: point.y, button: "left", buttons: 1, clickCount: 1 });
    await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: point.x, y: point.y, button: "left", buttons: 0, clickCount: 1 });
    await delay(100);
  };
  await tapControl('button[aria-label="Quick actions"]');
  await tapControl('button[aria-label="Open Terminal Keys"]');
  const accessOpen = await evaluate(cdp, `(() => { const panel = document.querySelector(".persea-terminal-keys"); return { visible: !!panel && !panel.hidden, view: panel?.dataset.view, focusPreserved: document.activeElement === window.__keysAccessFocus, inputs: window.__harness.inputTexts() }; })()`);
  if (!accessOpen.visible || accessOpen.view !== "Favorites" || !accessOpen.focusPreserved || JSON.stringify(accessOpen.inputs) !== JSON.stringify(accessBefore)) note("Quick actions did not open Favorites without focus or input", accessOpen);
  assert(accessOpen.visible, `keyboard-inset/closed-keyboard Keys did not open: ${JSON.stringify(accessOpen)}`);
  await tapControl(".persea-unified-keybar-key--bank");
  const accessClosed = await evaluate(cdp, "window.__harness.insetState()");
  if (!accessClosed.keybarHidden) note("manual Keys close did not restore hidden accessory", accessClosed);
  report.closedKeyboardKeysAccess = { opened: accessOpen, closed: accessClosed.keybarHidden };


  // Keyboard opens: an iPhone-ish 336px keyboard leaves 508. The pin and the
  // bar arrive together.
  const open = await evaluate(cdp, "window.__harness.setViewportInset(508)");
  if (open.styleHeight !== "508px") note("shell was not pinned to the visual viewport height", open);
  if (Math.abs(open.shellHeight - 508) > 1) note("pinned shell does not match the keyboard inset", open);
  if (open.keybarHidden) note("key bar did not show with the keyboard", open);
  if (open.keybarBottom > 509) note("key bar sits below the keyboard's top edge", open);
  font("keyboard open", open, before);
  if (open.scrollTop < open.maximumTop - 1) note("tail-reveal did not run on keyboard open", open);
  checkTail({ name: "keyboard-inset" }, "open", open, failures);

  // Open the catalog explicitly: Ctrl only changes its modifier state.
  await tapControl(".persea-unified-keybar-key--bank");
  await tapControl('[data-key-control="view:All keys"]');
  // Latch Ctrl through a trusted tap while the bar shows; the close below
  // must clear it — a latched modifier on an invisible bar is a trap.
  const latchPoint = await evaluate(cdp, "window.__harness.ctrlLatchPoint()");
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: latchPoint.x, y: latchPoint.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: latchPoint.x, y: latchPoint.y, button: "left", buttons: 0, clickCount: 1 });
  await delay(100);
  const latched = await evaluate(cdp, "window.__harness.ctrlState()");
  if (!latched.latched) note("ctrl latch did not arm through a trusted tap", { latchPoint, latched });
  const afterCatalogOpen = await evaluate(cdp, "window.__harness.insetState()");
  checkTail({ name: "keyboard-inset" }, "catalog-open", afterCatalogOpen, failures);

  // The keyboard changes height while up (dictation, accessory views). The
  // bar stays, and so does the latch — only hiding clears it.
  const adjusted = await evaluate(cdp, "window.__harness.setViewportInset(600)");
  if (adjusted.styleHeight !== "600px") note("a keyboard height change was not followed", adjusted);
  if (adjusted.keybarHidden) note("a keyboard height change hid the key bar", adjusted);
  font("a keyboard height change", adjusted, before);
  checkTail({ name: "keyboard-inset" }, "adjusted", adjusted, failures);

  // A long catalog exercises the later height-cap writer as well as the
  // intrinsic catalog render above. Neither may turn following into history.
  await tapControl('[data-key-control="key-group"]');
  await tapControl('[data-key-control="choose-group:Symbols"]');
  const catalogState = () => evaluate(cdp, `({ ...window.__harness.insetState(), panelHeight: document.querySelector(".persea-terminal-keys").getBoundingClientRect().height })`);
  const catalogBeforeResize = await catalogState();
  checkTail({ name: "keyboard-inset" }, "catalog-change", catalogBeforeResize, failures);
  await evaluate(cdp, "window.__harness.setViewportInset(508)");
  const catalogSmall = await catalogState();
  checkTail({ name: "keyboard-inset" }, "catalog-small", catalogSmall, failures);
  await evaluate(cdp, "window.__harness.setViewportInset(600)");
  const catalogLarge = await catalogState();
  if (catalogLarge.panelHeight <= catalogSmall.panelHeight + 1) note("catalog did not grow with its available height", { catalogSmall, catalogLarge });
  checkTail({ name: "keyboard-inset" }, "catalog-large", catalogLarge, failures);

  // A reader who deliberately leaves the tail keeps that exact history
  // position, including its fractional row, through the same layout changes.
  const historyBefore = await evaluate(cdp, `(async () => { const viewport = document.querySelector(".persea-unified-scroll"); viewport.scrollTop -= 400.25; await window.__harness.settle(); return window.__harness.insetState(); })()`);
  const historySmall = await evaluate(cdp, "window.__harness.setViewportInset(508)");
  const historyLarge = await evaluate(cdp, "window.__harness.setViewportInset(600)");
  for (const [label, state] of [["history-small", historySmall], ["history-large", historyLarge]]) {
    if (Math.abs(state.scrollTop - historyBefore.scrollTop) > 1) note(`${label} moved the reader's history position`, { before: historyBefore, after: state });
    font(label, state, before);
  }
  await evaluate(cdp, `(async () => { const viewport = document.querySelector(".persea-unified-scroll"); viewport.scrollTop = viewport.scrollHeight; await window.__harness.settle(); })()`);
  report.keyboardInsetCatalog = { catalogBeforeResize, catalogSmall, catalogLarge, historyBefore, historySmall, historyLarge };

  // Keyboard closes via the ordinary vv resize: the stylesheet's own height
  // returns, the bar hides in the same batch, the latch clears, the tail holds.
  const closed = await evaluate(cdp, "window.__harness.setViewportInset(null)");
  if (closed.styleHeight !== "") note("keyboard close did not restore the stylesheet height", closed);
  if (Math.abs(closed.shellHeight - 844) > 1) note("restored shell does not fill the window", closed);
  if (!closed.keybarHidden) note("key bar stayed visible after keyboard close", closed);
  font("keyboard close", closed, before);
  if (closed.scrollTop < closed.maximumTop - 1) note("tail did not hold across keyboard close", closed);
  checkTail({ name: "keyboard-inset" }, "closed", closed, failures);
  const latchAfterHide = await evaluate(cdp, "window.__harness.ctrlState()");
  if (latchAfterHide.latched || latchAfterHide.ariaPressed === "true") note("pending panel modifiers survived keyboard dismissal", latchAfterHide);

  // The innerHeight-drift close (the operator's stuck pin): iOS Safari moved
  // window.innerHeight while the keyboard was up (browser chrome collapse),
  // so the keyboard closes onto a vv.height that still sits under the stale
  // innerHeight. The old predicate "vv.height + 1 < innerHeight" stayed true
  // here forever and the shell stayed pinned mid-screen; the resting-mark
  // measure must read 750-of-844 as chrome, not keyboard.
  const driftOpen = await evaluate(cdp, "window.__harness.setViewportInset(508)");
  if (driftOpen.styleHeight !== "508px") note("drift scenario failed to pin on open", driftOpen);
  const driftClosed = await evaluate(cdp, "window.__harness.setViewportInset(750)");
  if (driftClosed.styleHeight !== "") note("the pin survived a close under a drifted innerHeight", driftClosed);
  if (!driftClosed.keybarHidden) note("the key bar survived a close under a drifted innerHeight", driftClosed);
  font("a drifted-innerHeight close", driftClosed, before);
  await evaluate(cdp, "window.__harness.setViewportInset(null)");

  // Dismissal with NO viewport event, announced only by blur (the swipe-down
  // and dismiss-key paths iOS sometimes under-reports): the focusout
  // re-evaluation reads the live viewport and self-corrects.
  const blurOpen = await evaluate(cdp, "window.__harness.setViewportInset(508)");
  if (blurOpen.styleHeight !== "508px") note("blur scenario failed to pin on open", blurOpen);
  await evaluate(cdp, "window.__harness.setViewportInsetSilently(null)");
  const blurred = await evaluate(cdp, "window.__harness.blurTerminal()");
  if (blurred.styleHeight !== "") note("blur without a viewport event left the pin behind", blurred);
  if (!blurred.keybarHidden) note("blur without a viewport event left the key bar up", blurred);
  font("a blur-only close", blurred, before);

  // Dismissal with no event of any kind: the OPEN-state watchdog re-reads the
  // live viewport within a second and releases everything.
  const watchdogOpen = await evaluate(cdp, "window.__harness.setViewportInset(508)");
  if (watchdogOpen.styleHeight !== "508px") note("watchdog scenario failed to pin on open", watchdogOpen);
  await evaluate(cdp, "window.__harness.setViewportInsetSilently(null)");
  await delay(1600);
  const watchdog = await evaluate(cdp, "window.__harness.insetState()");
  if (watchdog.styleHeight !== "") note("a silent dismissal escaped the keyboard watchdog", watchdog);
  if (!watchdog.keybarHidden) note("the key bar escaped the keyboard watchdog", watchdog);
  font("a watchdog-caught close", watchdog, before);

  // Pinch while pinned: releasing the pin is never gated on scale — a zoomed
  // operator must not strand it — while the bar follows the keyboard state,
  // which is still open. The close that follows clears everything.
  const pinchOpen = await evaluate(cdp, "window.__harness.setViewportInset(508)");
  if (pinchOpen.styleHeight !== "508px") note("pinch scenario failed to pin on open", pinchOpen);
  const pinchWhilePinned = await evaluate(cdp, "window.__harness.setViewportInset(254, 2)");
  if (pinchWhilePinned.styleHeight !== "") note("a pinch while pinned did not release the height pin", pinchWhilePinned);
  if (pinchWhilePinned.keybarHidden) note("the key bar hid while the keyboard was still up under zoom", pinchWhilePinned);
  const pinchClosed = await evaluate(cdp, "window.__harness.setViewportInset(null)");
  if (pinchClosed.styleHeight !== "") note("close after a pinch left the pin behind", pinchClosed);
  if (!pinchClosed.keybarHidden) note("close after a pinch left the key bar up", pinchClosed);
  font("a close after pinching", pinchClosed, before);

  // Residual pan after keyboard close: Safari occasionally strands the visual
  // viewport shifted at scale 1 with nothing left to scroll. The page must
  // nudge the scroll home.
  await evaluate(cdp, "window.__harness.clearScrollToCalls()");
  const panned = await evaluate(cdp, "window.__harness.setViewportOffsets(60, 40)");
  if (!(panned.scrollToCalls || []).some((call) => call.x === 0 && call.y === 0)) {
    note("a residual pan after keyboard close was not nudged back", panned);
  }
  await evaluate(cdp, "window.__harness.setViewportOffsets(0, 0)");

  // Pinch zoom at rest is not a keyboard (realistic reading: the same layout
  // height seen through a 2x zoom halves vv.height) — no pin, no bar, and a
  // panned position while genuinely zoomed is the operator's own, never fought.
  await evaluate(cdp, "window.__harness.clearScrollToCalls()");
  const pinched = await evaluate(cdp, "window.__harness.setViewportInset(422, 2)");
  if (pinched.styleHeight !== "") note("pinch zoom was mistaken for a keyboard", pinched);
  if (!pinched.keybarHidden) note("pinch zoom at rest summoned the key bar", pinched);
  const pinchPanned = await evaluate(cdp, "window.__harness.setViewportOffsets(60, 40)");
  if ((pinchPanned.scrollToCalls || []).length !== 0) {
    note("the page fought a pan while the operator was pinch-zoomed", pinchPanned);
  }
  await evaluate(cdp, "window.__harness.setViewportOffsets(0, 0)");
  await evaluate(cdp, "window.__harness.setViewportInset(null)");

  // A fine-pointer device never gets the inset or the bar.
  await emulateCoarsePointer(cdp, false);
  await evaluate(cdp, "window.__harness.settle()");
  const finePointer = await evaluate(cdp, "window.__harness.setViewportInset(508)");
  if (finePointer.styleHeight !== "") note("a fine pointer received a keyboard inset", finePointer);
  if (!finePointer.keybarHidden) note("a fine pointer received the key bar", finePointer);
  await evaluate(cdp, "window.__harness.setViewportInset(null)");
  report.keyboardInset = { before, open, adjusted, closed, driftClosed, blurred, watchdog, pinchWhilePinned, pinched, finePointer };
}

// The composer dock. Opened from the key bar while the software keyboard is
// up, the panel must appear as an in-flow row between the viewport and the
// key bar inside the pinned shell — above the keyboard — and both the open
// and the close must be height-only changes: the fitted font never moves
// (the coarse-pointer height-only refit gate), the textarea holds the 16px
// iOS focus-zoom guard, and the live edge stays revealed.
async function runMobileComposerDock(cdp, report, failures) {
  const note = (message, detail) => failures.push(`composer-dock/${message}: ${JSON.stringify(detail)}`);
  await emulateCoarsePointer(cdp, true);
  await cdp.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await delay(250);
  await evaluate(cdp, "window.__harness.resetShellHeight()");
  await evaluate(cdp, "window.__harness.announceViewport()");
  // 40 columns leaves fit headroom so the height ratio binds: the shape where
  // an ungated refit through the composer's box change would actually move
  // the font.
  await evaluate(cdp, "window.__harness.bootWithGeometry(200, 40, 24)");
  await evaluate(cdp, "window.__harness.zoom('Fit font', 1)");
  const before = await evaluate(cdp, "window.__harness.composerState()");
  if (!before.panelHidden) note("composer visible before any toggle", before);
  if (before.dockHeight > 0.5) note("closed dock row consumes height", before);
  if (before.toolbarToggleExpanded !== "false") note("composer toggle reads expanded while closed", before);

  // Keyboard opens; the bar (with the toggle) arrives.
  const keyboardOpen = await evaluate(cdp, "window.__harness.setViewportInset(508)");
  if (keyboardOpen.keybarHidden) note("key bar absent with the keyboard open", keyboardOpen);

  // Open through a trusted tap on the key-bar toggle.
  const togglePoint = await evaluate(cdp, "window.__harness.toolbarComposerTogglePoint()");
  if (!togglePoint.visible) note("composer toggle not visible for the tap", togglePoint);
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: togglePoint.x, y: togglePoint.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: togglePoint.x, y: togglePoint.y, button: "left", buttons: 0, clickCount: 1 });
  await delay(150);
  const open = await evaluate(cdp, "window.__harness.settle().then(() => window.__harness.composerState())");
  if (open.panelHidden) note("composer did not open from the key-bar toggle", open);
  if (open.shellStyleHeight !== "508px") note("shell lost the keyboard pin when the composer opened", open);
  if (open.panelPosition !== "static") note("composer panel is not in flow", open);
  if (open.panelBottom > open.keybarTop + 0.5) note("composer row is not directly above the key bar", open);
  if (open.keybarBottom > 509) note("key bar fell below the keyboard's top edge", open);
  if (open.viewportBottom > open.panelTop + 0.5) note("viewport does not end above the composer row", open);
  if (Math.abs(open.fontSize - before.fontSize) > 0.001) note("opening the composer refit the font", { before: before.fontSize, after: open.fontSize });
  // / unrestricted-zoom: the composer face is the operator's chosen size on
  // every pointer, the viewport never caps page zoom (WCAG 1.4.4), and the iOS
  // focus zoom is prevented at the composer instead — a 16px face for the
  // instant of focus on a coarse pointer, measured by the terminal topbar test on the phone.
  const viewportMeta = (fs.readFileSync(path.join(UI, "index.html"), "utf8").match(/<meta name="viewport" content="([^"]+)"/) || [])[1] || "";
  const zoomCap = /(^|,)\s*maximum-scale=(0|1)(\.\d+)?(\s|,|$)/.test(viewportMeta) || /user-scalable=(no|0)/.test(viewportMeta);
  if (zoomCap) note("the viewport meta caps page zoom", { viewportMeta });
  if (open.textareaFontSize !== "11px") note("composer textarea does not show the default 11px face", open);
  if (!open.textareaFocused) note("composer textarea did not take focus on open", open);
  if (open.toolbarToggleExpanded !== "true") note("composer toggle does not reflect the open state", open);
  if (open.scrollTop < open.maximumTop - 1) note("live edge was not revealed above the composer", open);
  if (open.pageScrollWidth > 391) note("the composer row gave the page horizontal overflow", open);

  // Close through the same toggle: height-only again, font and tail hold.
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: togglePoint.x, y: togglePoint.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: togglePoint.x, y: togglePoint.y, button: "left", buttons: 0, clickCount: 1 });
  await delay(150);
  const closed = await evaluate(cdp, "window.__harness.settle().then(() => window.__harness.composerState())");
  if (!closed.panelHidden) note("composer did not close from the key-bar toggle", closed);
  if (closed.dockHeight > 0.5) note("closed dock row retains height", closed);
  if (Math.abs(closed.fontSize - before.fontSize) > 0.001) note("closing the composer refit the font", { before: before.fontSize, after: closed.fontSize });
  if (closed.toolbarToggleExpanded !== "false") note("composer toggle does not reflect the closed state", closed);
  if (closed.scrollTop < closed.maximumTop - 1) note("tail did not hold across the composer close", closed);
  report.composerDock = { before, open, closed };
  await evaluate(cdp, "window.__harness.setViewportInset(null)");
  await emulateCoarsePointer(cdp, false);
}

// The composer image-insert flow against the mocked staging network: paste
// and picker staging, upload/refusal/retry chips, Insert gating during an
// upload, and the final single INPUT payload carrying the staged paths after
// the normalized text.
async function runComposerImageInsert(cdp, report, failures) {
  const note = (message, detail) => failures.push(`composer-image/${message}: ${JSON.stringify(detail)}`);
  await emulateCoarsePointer(cdp, false);
  await cdp.send("Emulation.setDeviceMetricsOverride", { width: 1024, height: 768, deviceScaleFactor: 1, mobile: false });
  await delay(250);
  await evaluate(cdp, "window.__harness.resetShellHeight()");
  await evaluate(cdp, "window.__harness.announceViewport()");
  await evaluate(cdp, "window.__harness.bootWithGeometry(50, 80, 24)");
  const baseline = await evaluate(cdp, "window.__harness.composerImageState()");
  if (!baseline.attachPresent) note("attach affordance absent despite the capability", baseline);
  if (baseline.inputAccept !== "image/png,image/jpeg,image/gif,image/webp") note("accept list drifted", baseline);
  if (baseline.chipCount !== 0 || baseline.panelAttachments !== "") note("chips existed before any attachment", baseline);

  const togglePoint = await evaluate(cdp, "window.__harness.toolbarComposerTogglePoint()");
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: togglePoint.x, y: togglePoint.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: togglePoint.x, y: togglePoint.y, button: "left", buttons: 0, clickCount: 1 });
  await delay(150);
  const open = await evaluate(cdp, "window.__harness.settle().then(() => window.__harness.composerState())");
  if (open.panelHidden) note("composer did not open from the toolbar toggle", open);

  await evaluate(cdp, "window.__harness.setComposerText('review this')");
  const textPaste = await evaluate(cdp, "window.__harness.pasteText('plain clipboard')");
  if (textPaste.defaultPrevented) note("text-only paste was intercepted", textPaste);
  if (textPaste.chipCount !== 0) note("text-only paste produced a chip", textPaste);

  const uploading = await evaluate(cdp, "window.__harness.pasteImage('image/png', 'shot.png')");
  if (!uploading.defaultPrevented) note("image paste was not captured", uploading);
  if (uploading.chipCount !== 1 || uploading.chipStatuses[0] !== "uploading") note("image paste did not produce an uploading chip", uploading);
  if (!uploading.sendDisabled || !uploading.statusText.includes("Waiting for 1 image upload")) note("Insert was not blocked during the upload", uploading);
  if (uploading.panelAttachments !== "present") note("panel did not enter the attachments layout", uploading);

  const staged = await evaluate(cdp, "window.__harness.resolveUpload()");
  if (staged.chipStatuses[0] !== "staged") note("resolved upload did not stage the chip", staged);
  if (staged.sendDisabled) note("Insert stayed blocked after the upload settled", staged);
  // This fixture mounts the composer without a storage scope, so its one
  // standing condition is the storage hint; nothing else may show for a
  // staged image, and the line is hidden exactly when it has no text.
  if (staged.statusHidden !== (staged.statusText === "") || (staged.statusText !== "" && staged.statusText !== "Draft held in this tab only")) note("a staged image with nothing to act on shows more than the fixture's storage condition", staged);

  const refused = await evaluate(cdp, "window.__harness.pasteImage('image/heic', 'IMG_1.heic')");
  if (!refused.defaultPrevented) note("HEIC paste escaped capture", refused);
  if (refused.chipCount !== 2 || refused.chipStatuses[1] !== "failed") note("HEIC paste did not produce a failed chip", refused);
  if (!String(refused.chipNotes[1]).includes("HEIC images are not supported")) note("HEIC refusal copy drifted", refused);
  if (refused.sendDisabled) note("a failed chip blocked Insert", refused);

  const mixed = await evaluate(cdp, "window.__harness.pasteImage('image/png', 'both.png', ' and this')");
  if (!String(mixed.textareaValue).includes("and this")) note("mixed clipboard lost its text half", mixed);
  if (mixed.chipCount !== 3) note("mixed clipboard lost its image half", mixed);
  await evaluate(cdp, "window.__harness.resolveUpload()");

  const removed = await evaluate(cdp, "window.__harness.clickChipButton(1, '×')");
  if (removed.chipCount !== 2) note("chip removal failed", removed);
  if (removed.chipRemoveLabels.join("|") !== "Remove image 1 of 2|Remove image 2 of 2") note("remove labels did not renumber", removed);

  const picked = await evaluate(cdp, "window.__harness.attachFiles(['image/jpeg'])");
  if (picked.chipCount !== 3 || picked.chipStatuses[2] !== "uploading") note("picker files did not stage", picked);
  const failed = await evaluate(cdp, "window.__harness.rejectUpload('too_large')");
  if (failed.chipStatuses[2] !== "failed" || !String(failed.chipNotes[2]).includes("larger than the 10 MB limit")) note("upload failure copy drifted", failed);
  if (!String(failed.statusText).includes("1 of 3 images failed to upload")) note("aggregate failure line absent", failed);
  const retried = await evaluate(cdp, "window.__harness.clickChipButton(2, 'Retry')");
  if (retried.chipStatuses[2] !== "uploading") note("retry did not restart the upload", retried);
  const settled = await evaluate(cdp, "window.__harness.resolveUpload()");
  if (settled.chipStatuses[2] !== "staged") note("retried upload did not stage", settled);

  // The trailing-newline hazard end to end: a dictation-shaped draft ending
  // in newlines must insert as one line with the paths after the text.
  await evaluate(cdp, "window.__harness.setComposerText('look at this\\n\\n')");
  await evaluate(cdp, "window.__harness.clearInputTexts()");
  const sendPoint = await evaluate(cdp, "window.__harness.composerSendPoint()");
  if (sendPoint.disabled) note("Insert disabled before the final insert", sendPoint);
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: sendPoint.x, y: sendPoint.y, button: "left", buttons: 1, clickCount: 1 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: sendPoint.x, y: sendPoint.y, button: "left", buttons: 0, clickCount: 1 });
  await delay(150);
  const inputs = await evaluate(cdp, "window.__harness.settle().then(() => window.__harness.inputTexts())");
  if (!Array.isArray(inputs) || inputs.length !== 1) note("Insert did not produce exactly one INPUT frame", inputs);
  const payload = Array.isArray(inputs) ? inputs[0] ?? "" : "";
  if (!/^look at this( \/var\/lib\/persea-terminal-staging\/harness\/img-[0-9a-f]{32}\.png){3}$/.test(payload)) note("inserted payload drifted", { payload });
  if (/[\r\n]/.test(payload)) note("inserted payload carried a line break", { payload });
  const after = await evaluate(cdp, "window.__harness.composerImageState()");
  if (after.chipCount !== 0 || after.textareaValue !== "") note("draft did not clear after insert", after);
  if (!String(after.statusText).includes("3 images") || String(after.statusText).includes("not run") || after.statusHidden !== false) note("receipt did not count the images, kept the 'not run' qualifier, or stayed hidden", after);
  report.composerImageInsert = { baseline, uploading, staged, refused, removed, failed, retried, payload, after };
}

// The phone chrome. A control row never scrolls: at 390 and 320 CSS pixels on
// a coarse pointer, keyboard closed and open, every essential toolbar
// control (committed-size disclosure, Quick actions, ✎) and every composer
// action (⊕ ⌫ ➤) renders inside the viewport at a ≥44px target. The committed
// size opens the one positioned View & appearance surface without layout;
// Quick actions remains a separate sheet.
// composer shows no counter and no idle status line (composer input) and ➤ is the
// chrome row's right edge; the page shell gains no horizontal overflow even
// with a full-width 80-column TUI frame painted. The fitted font holds
// through every transition, and a width change still refits it (the
// coarse-pointer gate is height-only) — so a full-width TUI's remaining
// horizontal range is attributed with numbers: the grid's own width at the
// font floor inside its scroller, never a page-level source.
async function runMobilePhoneChrome(cdp, report, failures) {
  const note = (message, detail) => failures.push(`phone-chrome/${message}: ${JSON.stringify(detail)}`);
  const inside = (label, box, width) => {
    if (!box || !box.visible) { note(`${label} is not visible`, box); return; }
    if (box.left < -0.5 || box.right > width + 0.5) note(`${label} lands outside the viewport`, { box, width });
  };
  const target = (label, box) => {
    if (box && box.visible && (box.height < 44 || box.width < 44)) note(`${label} is below the 44px touch target`, box);
  };
  const tap = async (point) => {
    await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: point.x, y: point.y });
    await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: point.x, y: point.y, button: "left", buttons: 1, clickCount: 1 });
    await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: point.x, y: point.y, button: "left", buttons: 0, clickCount: 1 });
    await delay(150);
    await evaluate(cdp, "window.__harness.settle()");
  };
  const phone = async (width) => {
    await cdp.send("Emulation.setDeviceMetricsOverride", { width, height: 844, deviceScaleFactor: 3, mobile: true });
    await delay(250);
    await evaluate(cdp, "window.__harness.announceViewport()");
    await evaluate(cdp, "window.__harness.settle()");
  };
  await emulateCoarsePointer(cdp, true);

  // The width-refit control: 40 columns fit 390 above the font floor, and
  // narrowing to 320 — a width change — refits on a coarse pointer. Without
  // this, "the font did not move" below would be indistinguishable from "the
  // font never fits".
  await phone(390);
  await evaluate(cdp, "window.__harness.bootWithGeometry(60, 40, 24)");
  await evaluate(cdp, "window.__harness.resetShellHeight()");
  await evaluate(cdp, "window.__harness.announceViewport()");
  await evaluate(cdp, "window.__harness.zoom('Fit font', 1)");
  const fit390 = await evaluate(cdp, "window.__harness.chromeState()");
  if (!(fit390.fontSize > 9.001)) note("40 columns at 390 did not fit above the font floor", { fontSize: fit390.fontSize, gridWidth: fit390.gridWidth });
  await phone(320);
  const fit320 = await evaluate(cdp, "window.__harness.chromeState()");
  if (!(fit320.fontSize < fit390.fontSize - 0.5)) note("a width change on a coarse pointer did not refit the font", { at390: fit390.fontSize, at320: fit320.fontSize });
  if (fit320.gridWidth > fit320.scrollerClientWidth - fit320.hostPadding + 0.5) note("40 columns overflow 320 after the width refit", fit320);
  report["phone-chrome/width-refit"] = { at390: fit390.fontSize, at320: fit320.fontSize, gridWidth320: fit320.gridWidth };

  for (const width of [390, 320]) {
    await phone(width);
    await evaluate(cdp, "window.__harness.bootWithGeometry(60, 80, 24)");
    await evaluate(cdp, "window.__harness.resetShellHeight()");
    await evaluate(cdp, "window.__harness.announceViewport()");
    await evaluate(cdp, "window.__harness.zoom('Fit font', 1)");
    // Make sure the composer starts closed: earlier scenarios may leave it up.
    const resting = await evaluate(cdp, "window.__harness.composerState()");
    if (!resting.panelHidden) await tap(await evaluate(cdp, "window.__harness.toolbarComposerTogglePoint()"));

    // The full-width TUI frame, and the item-3 attribution.
    const painted = await evaluate(cdp, "window.__harness.paintFullWidthFrame()");
    const contentWidth = painted.scrollerClientWidth - painted.hostPadding;
    const inherent = painted.gridWidth > contentWidth + 0.5;
    if (painted.pageScrollWidth !== painted.pageClientWidth || painted.shellScrollWidth !== painted.shellClientWidth) {
      note(`${width}: the page shell gained horizontal overflow under a full-width TUI`, painted);
    }
    const expectedScrollerWidth = Math.max(painted.scrollerClientWidth, painted.gridWidth + painted.hostPadding);
    if (Math.abs(painted.scrollerScrollWidth - expectedScrollerWidth) > 1) {
      note(`${width}: the scroller's horizontal range is not the grid's own width`, { ...painted, expectedScrollerWidth });
    }
    if (inherent && painted.fontSize > 9.001) note(`${width}: the grid overflows while the font sits above its floor — a width refit was skipped`, painted);
    report[`phone-chrome@${width}/full-width-tui`] = {
      inherent,
      fontSize: painted.fontSize,
      columns: painted.columns,
      cellWidth: painted.cellWidth,
      gridWidth: painted.gridWidth,
      contentWidth,
      scrollerRange: painted.scrollerScrollWidth - painted.scrollerClientWidth,
      pageRange: painted.pageScrollWidth - painted.pageClientWidth,
      columnsThatFit: painted.cellWidth > 0 ? Math.floor(contentWidth / painted.cellWidth) : 0,
      fontToFit: painted.gridWidth > 0 ? Number((painted.fontSize * contentWidth / painted.gridWidth).toFixed(2)) : 0,
    };

    const checkToolbar = (phase, s) => {
      if (s.toolbar.overflowX === "auto" || s.toolbar.overflowX === "scroll") note(`${width}/${phase}: the toolbar is a scroller`, s.toolbar);
      if (s.toolbar.scrollWidth > s.toolbar.clientWidth + 1 || s.toolbar.scrollLeft !== 0) note(`${width}/${phase}: the toolbar scrolls`, s.toolbar);
      inside(`${width}/${phase}: committed size`, s.readout, width);
      inside(`${width}/${phase}: ✎`, s.composerToggle, width);
      inside(`${width}/${phase}: sheet opener`, s.opener, width);
      if (s.puckPresent) note(`${width}/${phase}: the floating puck is still in the document`, s.puckPresent);
      target(`${width}/${phase}: committed size`, s.readout);
      target(`${width}/${phase}: ✎`, s.composerToggle);
      if (s.opener && s.opener.visible && (s.opener.width < 43.5 || s.opener.height < 43.5)) note(`${width}/${phase}: the sheet opener is below 44px`, s.opener);
      if (s.pageScrollWidth > s.pageClientWidth || s.shellScrollWidth > s.shellClientWidth) note(`${width}/${phase}: the page gained horizontal overflow`, s);
    };
    const closed = await evaluate(cdp, "window.__harness.chromeState()");
    checkToolbar("keyboard-closed", closed);
    if (closed.secondary.visible) note(`${width}: View & appearance renders before its disclosure is tapped`, closed);
    if (!closed.opener || closed.opener.expanded !== "false" || (closed.sheet && closed.sheet.visible)) note(`${width}: the sheet renders before the opener is tapped`, closed);
    if (closed.opener && closed.opener.controls !== (closed.sheet && closed.sheet.id)) note(`${width}: the opener does not reference its sheet`, closed.opener);

    await tap({ x: (closed.readout.left + closed.readout.right) / 2, y: (closed.readout.top + closed.readout.bottom) / 2 });
    const phoneView = await evaluate(cdp, "window.__harness.chromeState()");
    inside(`${width}/View: geometry disclosure`, phoneView.phoneReadout, width);
    if (phoneView.phoneReadout.text !== "80×24") note(`${width}/View: readout is not the compact form`, phoneView.phoneReadout);
    if (!phoneView.secondary.visible || phoneView.secondary.open !== "true") note(`${width}: committed size did not open View & appearance`, phoneView.secondary);
    await tap({ x: (phoneView.readout.left + phoneView.readout.right) / 2, y: (phoneView.readout.top + phoneView.readout.bottom) / 2 });

    // The quick-actions sheet: opens inside the viewport, in a positioned
    // layer (the dock; no in-flow height change for the refit gate to absorb), carries
    // the ⋯ panel's items as 44px tiles, closes again from the opener.
    const openerPoint = await evaluate(cdp, "window.__harness.openerPoint()");
    if (!openerPoint.visible) note(`${width}: the opener is not rendered for the tap`, openerPoint);
    await tap(openerPoint);
    const menu = await evaluate(cdp, "window.__harness.chromeState()");
    if (!menu.opener || menu.opener.expanded !== "true" || !menu.sheet || !menu.sheet.visible) note(`${width}: the opener did not reveal the sheet`, { opener: menu.opener, sheet: menu.sheet });
    if (menu.sheet && menu.sheet.layer !== "absolute") note(`${width}: the sheet is in flow`, menu.sheet);
    if (menu.sheet && (menu.sheet.left < -0.5 || menu.sheet.right > width + 0.5)) note(`${width}: the sheet lands outside the viewport`, menu.sheet);
	for (const label of ["Clipboard", "Keys", "Composer"]) {
	  const button = menu.sheetTiles.find((candidate) => candidate && candidate.text === label);
      if (!button || !button.visible) note(`${width}/sheet: ${label} is not visible`, button);
      else if (button.left < -0.5 || button.right > width + 0.5) note(`${width}/sheet: ${label} lands outside the viewport`, { button, width });
	  if (button && button.visible && (button.height < 44 || button.width < 44)) note(`${width}/sheet: ${label} is below the 44px touch target`, button);
	}
	for (const label of ["Select", "Open clipboard"]) {
	  const button = menu.primaryControls.find((candidate) => candidate && (candidate.text === label || candidate.label.startsWith(label)));
	  if (!button || !button.visible) note(`${width}/toolbar: ${label} is not visible`, button);
	  else if (button.height < 44 || button.width < 44) note(`${width}/toolbar: ${label} is below the 44px touch target`, button);
	}
    if (Math.abs(menu.fontSize - closed.fontSize) > 0.001) note(`${width}: opening the sheet refit the font`, { before: closed.fontSize, after: menu.fontSize });
    if (menu.pageScrollWidth > menu.pageClientWidth) note(`${width}: the sheet gave the page horizontal overflow`, menu);
    // The opener stays in the control row above the sheet (it covers no tile),
    // so its point is re-read for the closing tap.
    await tap(await evaluate(cdp, "window.__harness.openerPoint()"));
    const menuClosed = await evaluate(cdp, "window.__harness.chromeState()");
    if (!menuClosed.opener || menuClosed.opener.expanded !== "false" || (menuClosed.sheet && menuClosed.sheet.visible)) note(`${width}: the opener did not close the sheet`, menuClosed.opener);

    // Keyboard up: the toolbar holds, the font holds.
    await evaluate(cdp, "window.__harness.setViewportInset(508)");
    const keyboard = await evaluate(cdp, "window.__harness.chromeState()");
    checkToolbar("keyboard-open", keyboard);
    if (Math.abs(keyboard.fontSize - closed.fontSize) > 0.001) note(`${width}: the keyboard refit the font`, { before: closed.fontSize, after: keyboard.fontSize });

    // The composer with a long draft and two images: no counter renders
    // and, with nothing to act on, no status line either — under the text
    // there is only the chrome row.
    const togglePoint = await evaluate(cdp, "window.__harness.toolbarComposerTogglePoint()");
    if (!togglePoint.visible) note(`${width}: toolbar composer toggle not visible for the tap`, togglePoint);
    await tap(togglePoint);
    const draft = Array.from({ length: 12 }, (_, line) => "x".repeat(line === 11 ? 1234 - 11 * 100 - 11 : 100)).join("\n");
    await evaluate(cdp, `window.__harness.setComposerText(${JSON.stringify(draft)})`);
    await evaluate(cdp, "window.__harness.attachFiles(['image/png', 'image/png'])");
    await evaluate(cdp, "window.__harness.resolveUpload()");
    await evaluate(cdp, "window.__harness.resolveUpload()");
    const composer = await evaluate(cdp, "window.__harness.chromeState()");
    const c = composer.composer;
    if (!c || c.hidden) { note(`${width}: composer did not open`, c); continue; }
    checkToolbar("composer-open", composer);
    if (Math.abs(composer.fontSize - closed.fontSize) > 0.001) note(`${width}: the composer refit the font`, { before: closed.fontSize, after: composer.fontSize });
    if (c.footer.overflowX === "auto" || c.footer.overflowX === "scroll") note(`${width}: the composer chrome row is a scroller`, c.footer);
    if (c.footer.scrollWidth > c.footer.clientWidth + 1 || c.footer.scrollLeft !== 0) note(`${width}: the composer chrome row scrolls`, c.footer);
    if (c.footer.height < 44 || c.footer.height > 45) note(`${width}: the chrome row is not 44px`, c.footer);
    if (c.counter) note(`${width}: a counter renders`, c.counter);
    // With nothing to act on the status line is hidden; this fixture mounts
    // the composer without a storage scope, so the one standing condition
    // is the storage hint — allowed, on its own full-width line, unclipped.
    if (c.status && c.status.visible) {
      if (c.status.text !== "Draft held in this tab only") note(`${width}: the status line renders with nothing to say`, c.status);
      if (c.status.bottom > c.footer.top + 0.5) note(`${width}: the status shares a row with the chrome`, { status: c.status, footer: c.footer });
      if (c.status.scrollWidth > c.status.clientWidth + 1) note(`${width}: the status is clipped`, c.status);
    }
    if (!c.modeToggle || !c.modeToggle.visible) note(`${width}: the autocorrect toggle is not rendered`, c.modeToggle);
    else target(`${width}/composer: autocorrect toggle`, c.modeToggle);
    inside(`${width}/composer: ➤`, c.send, width);
    inside(`${width}/composer: ⊕`, c.attach, width);
    inside(`${width}/composer: ⌫`, c.clear, width);
    // Vertically too: a long wrapped draft must not push the chrome row under
    // the keyboard (the shell is pinned to the 508px visual viewport here).
    for (const [label, box] of [["➤", c.send], ["⊕", c.attach], ["⌫", c.clear]]) {
      if (box && box.visible && (box.bottom > composer.shellBottom + 0.5 || (composer.keybarTop !== null && box.bottom > composer.keybarTop + 0.5))) {
        note(`${width}/composer: ${label} sits below the pinned shell or under the key bar`, { box, shellBottom: composer.shellBottom, keybarTop: composer.keybarTop });
      }
    }
    target(`${width}/composer: ➤`, c.send);
    target(`${width}/composer: ⊕`, c.attach);
    target(`${width}/composer: ⌫`, c.clear);
    if (c.send && c.send.visible && c.send.right < c.footer.right - 1) note(`${width}: ➤ is not the chrome row's right edge`, { send: c.send, footer: c.footer });
    if (c.attach && c.clear && c.send && !(c.attach.right <= c.clear.left + 0.5 && c.clear.right <= c.send.left + 0.5)) note(`${width}: the actions are not ordered ⊕ ⌫ ➤`, c);
    for (const button of c.headerButtons) {
      if (button && button.visible) {
        inside(`${width}/composer: header ${button.text}`, button, width);
        target(`${width}/composer: header ${button.text}`, button);
        if (c.send && button.left >= c.send.left) note(`${width}: a header control sits right of ➤`, { button, send: c.send });
      }
    }
    if (!c.close || !c.close.visible) note(`${width}: the neutral Hide composer control is missing`, c.close);
    else target(`${width}/composer: Hide composer`, c.close);
    report[`phone-chrome@${width}/composer`] = c;

    // The crowded rare state: smart punctuation in the draft reveals Fix, a
    // word button, beside the actions, and the status line \u2014 the phone's
    // only composer feedback channel \u2014 on its own full-width line above the
    // chrome row, never clipped. Fix stays whole at 390 and the actions
    // never move.
    await evaluate(cdp, `window.__harness.setComposerText(${JSON.stringify(`\u201c${draft}\u201d`)})`);
    const crowded = await evaluate(cdp, "window.__harness.chromeState()");
    const k = crowded.composer;
    if (!k.status || !k.status.visible) note(`${width}: the status line is not rendered for smart punctuation`, k.status);
    else {
      if (k.status.bottom > k.footer.top + 0.5) note(`${width}: the status shares a row with the chrome`, { status: k.status, footer: k.footer });
      if (k.status.scrollWidth > k.status.clientWidth + 1) note(`${width}: the status is clipped`, k.status);
    }
    if (!k.fix || !k.fix.visible) note(`${width}: Fix did not appear for smart punctuation`, k.fix);
    else {
      inside(`${width}/crowded: Fix`, k.fix, width);
      if (width >= 360 && k.fix.scrollWidth > k.fix.clientWidth + 1) note(`${width}: Fix was squeezed`, k.fix);
      if (k.send && k.fix.right > k.attach.left + 0.5) note(`${width}: Fix overlaps the actions`, { fix: k.fix, attach: k.attach });
    }
    if (k.footer.scrollWidth > k.footer.clientWidth + 1) note(`${width}/crowded: the composer chrome row scrolls`, k.footer);
    if (k.send && k.send.right < k.footer.right - 1) note(`${width}/crowded: ➤ left the row's right edge`, { send: k.send, footer: k.footer });
    if (k.send && k.send.width < 40) note(`${width}/crowded: ➤ was squeezed`, k.send);
    report[`phone-chrome@${width}/composer-crowded`] = k;

    if (width < 360) {
      // The receipt row is exempt from the narrow-width hiding: after an
      // Insert the status line and the ⌄ close ARE the row.
      await evaluate(cdp, "window.__harness.clearInputTexts()");
      // One line: a multi-line draft trips the paste guard instead of inserting.
      await evaluate(cdp, "window.__harness.setComposerText('receipt probe')");
      const sendPoint = await evaluate(cdp, "window.__harness.composerSendPoint()");
      if (sendPoint.disabled) note(`${width}: Insert is disabled with a staged draft`, sendPoint);
      await tap(sendPoint);
      const sent = await evaluate(cdp, "window.__harness.chromeState()");
      const r = sent.composer;
      if (!r || r.content !== "sent") note(`${width}: Insert did not produce the receipt row`, r);
      else {
        inside(`${width}/receipt: status`, r.status, width);
        inside(`${width}/receipt: ⌄`, r.close, width);
        if (sent.pageScrollWidth > sent.pageClientWidth) note(`${width}: the receipt row gave the page horizontal overflow`, sent);
      }
      report[`phone-chrome@${width}/receipt`] = r;
    } else {
      // Clear the draft so the next width starts clean.
      for (let chip = 0; chip < 2; chip++) await evaluate(cdp, "window.__harness.clickChipButton(0, '×')");
      await evaluate(cdp, "window.__harness.setComposerText('')");
    }
    await tap(togglePoint);
    await evaluate(cdp, "window.__harness.setViewportInset(null)");
    await evaluate(cdp, "window.__harness.leaveAlternate()");
    report[`phone-chrome@${width}`] = { closed, menu, keyboard };
  }
  await emulateCoarsePointer(cdp, false);
}

async function main() {
  childProcess.execFileSync(path.join(UI, "node_modules/.bin/esbuild"), [
    path.join(UI, "test/unified_scroll_geometry_browser_harness.ts"),
    "--bundle", "--platform=browser", "--format=iife",
    `--outfile=${path.join(UI, "dist/test/unified_scroll_geometry_browser_harness.js")}`,
  ], { stdio: "inherit" });

  const html = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"><link rel="stylesheet" href="/xterm.css"><link rel="stylesheet" href="/attachment.css">
<style>html,body{margin:0;width:100%;height:100%;overflow:hidden;background:#111318}</style>
</head><body><div id="root"></div><script src="/geometry_harness.js"></script></body></html>`;
  const server = http.createServer((request, response) => {
    const pathname = new URL(request.url, "http://localhost").pathname;
    if (pathname === "/") {
      response.setHeader("Content-Type", "text/html");
      response.end(html);
      return;
    }
    const routes = {
      "/geometry_harness.js": path.join(UI, "dist/test/unified_scroll_geometry_browser_harness.js"),
      "/xterm.css": path.join(UI, "node_modules/@xterm/xterm/css/xterm.css"),
      "/attachment.css": path.join(UI, "src/attachment_page.css"),
      "/attachment_shell.css": path.join(UI, "src/attachment_shell.css"),
      "/attachment_composer.css": path.join(UI, "src/attachment_composer.css"),
      "/attachment_surface.css": path.join(UI, "src/attachment_surface.css"),
      "/unified_terminal_surface.css": path.join(UI, "src/unified_terminal_surface.css"),
      "/unified_terminal_toolbar.css": path.join(UI, "src/unified_terminal_toolbar.css"),
      "/unified_terminal_composer.css": path.join(UI, "src/unified_terminal_composer.css"),
      "/unified_terminal_overlays.css": path.join(UI, "src/unified_terminal_overlays.css"),
      "/unified_terminal_disclosures.css": path.join(UI, "src/unified_terminal_disclosures.css"),
    };
    const file = routes[pathname];
    if (!file) { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", file.endsWith(".css") ? "text/css" : "text/javascript");
    response.end(fs.readFileSync(file));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const webPort = server.address().port;
  const debugPort = await freePort();
  // Chromium's singleton socket lives under the profile; a long path exceeds
  // sun_path and the browser dies before it ever listens.
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), "persea-scroll-geometry-"));
  const chrome = childProcess.spawn(browserBinary(), [
    "--headless=new",
    "--no-sandbox",
    "--disable-gpu",
    "--disable-dev-shm-usage",
    `--remote-debugging-port=${debugPort}`,
    `--user-data-dir=${profile}`,
    "--no-first-run",
    "--no-default-browser-check",
    "about:blank",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  const chromeStderr = [];
  chrome.stderr.on("data", (chunk) => { if (chromeStderr.length < 64) chromeStderr.push(String(chunk)); });

  const failures = [];
  const report = {};
  let cdp;
  try {
    let target;
    for (let attempt = 0; attempt < 200 && !target; attempt++) {
      try {
        const list = await requestJSON(`http://127.0.0.1:${debugPort}/json/list`);
        target = list.find((entry) => entry.type === "page" && entry.webSocketDebuggerUrl);
      } catch { /* browser still starting */ }
      if (!target) await delay(50);
    }
    assert(target, `Chrome target unavailable: ${chromeStderr.join("").slice(-2000)}`);
    cdp = new CDP(target.webSocketDebuggerUrl);
    await cdp.open();
    await cdp.send("Runtime.enable");
    await cdp.send("Page.enable");
    await cdp.send("Page.navigate", { url: `http://127.0.0.1:${webPort}/` });
    for (let attempt = 0; attempt < 200; attempt++) {
      if (await evaluate(cdp, "Boolean(window.__harness)")) break;
      await delay(50);
    }
    assert(await evaluate(cdp, "Boolean(window.__harness)"), "harness never installed");

    for (const shape of SHAPES) {
      for (const scale of SCALES) await runTailCondition(cdp, shape, scale, report, failures);
    }
    for (const shape of SHAPES) await runSubPixelSweep(cdp, shape, report, failures);
    for (const shape of SHAPES) await runZoomSweep(cdp, shape, report, failures);
    for (const shape of SHAPES) await runShape(cdp, shape, report, failures);
    for (const shape of SHAPES) await runVerticalFit(cdp, shape, report, failures);
    for (const shape of SHAPES) await runPendingFitInputSeal(cdp, shape, report, failures);
    await runFailureSurface(cdp, report, failures);
    await runMobileKeyBarFit(cdp, report, failures);
    await runMobileKeyboardInset(cdp, report, failures);
    await runMobileComposerDock(cdp, report, failures);
    await runComposerImageInsert(cdp, report, failures);
    await runMobilePhoneChrome(cdp, report, failures);
  } finally {
    cdp?.close();
    chrome.kill("SIGKILL");
    server.close();
    fs.rmSync(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
  }

  const artifact = process.env.PERSEA_SCROLL_GEOMETRY_ARTIFACT;
  if (artifact) fs.writeFileSync(artifact, `${JSON.stringify({ shapes: SHAPES, report, failures }, null, 2)}\n`);
  process.stdout.write(`${JSON.stringify({ failures }, null, 2)}\n`);
  assert(failures.length === 0, `unified scroll geometry regression test failed:\n  ${failures.join("\n  ")}`);
  process.stdout.write("unified scroll geometry regression test PASS\n");
}

main().catch((error) => { process.stderr.write(`${error.stack || error}\n`); process.exit(1); });
