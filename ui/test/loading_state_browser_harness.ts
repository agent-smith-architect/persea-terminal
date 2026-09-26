import { UnifiedTerminalPage } from "../src/unified_terminal_page";

const SOURCE_PREFIX = "loading_state-source";
const EPOCH = 41n;
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const encoder = new TextEncoder();

type Mounted = { page: UnifiedTerminalPage; generation: number; cut: bigint; name: string };
const mounted: Mounted[] = [];
const sent: Array<{ pane: string; type: string; mode?: string }> = [];
const detaches: string[] = [];
const finalized: string[] = [];
const caughtUp: number[] = [];
let focusEvents = 0;
let transportOpens = 0;
let socketConstructions = 0;
let reconnectRequests = 0;
document.addEventListener("focusin", () => { focusEvents += 1; }, true);
// The reattach burst window reads Date.now(). Moving the wall clock lets a
// check step past that window, so the rule under test is the only one that
// can stop the page.
let wallClockOffset = 0;
const realDateNow = Date.now.bind(Date);
Date.now = () => realDateNow() + wallClockOffset;
const NativeWebSocket = window.WebSocket;
window.WebSocket = new Proxy(NativeWebSocket, {
  construct(target, args) {
    socketConstructions += 1;
    return Reflect.construct(target, args);
  },
}) as typeof WebSocket;

const frame = (pane: Mounted, extra: Record<string, unknown>): Record<string, unknown> => ({
  version: 1,
  source: `${SOURCE_PREFIX}-${pane.name}`,
  epoch: EPOCH,
  ...extra,
});

const visible = (node: HTMLElement | null): boolean => Boolean(node && !node.hidden && getComputedStyle(node).display !== "none" && node.getClientRects().length > 0);

function snapshot(): Record<string, unknown> {
  const panes = Array.from(document.querySelectorAll<HTMLElement>(".loading-harness__pane")).map((root) => {
    const loading = root.querySelector<HTMLElement>(".persea-unified-loading");
    const toolbar = root.querySelector<HTMLElement>(".persea-unified-toolbar");
    const notice = root.querySelector<HTMLElement>(".persea-unified-notice");
    const rect = toolbar?.getBoundingClientRect();
    return {
      name: root.dataset.name,
      loadingVisible: visible(loading),
      loadingText: loading?.textContent?.replace(/\s+/g, " ").trim() ?? "",
      loadingState: loading?.dataset.state ?? null,
      noticeVisible: visible(notice),
      noticeHeadline: root.querySelector<HTMLElement>(".persea-unified-notice__headline")?.textContent ?? "",
      reconnectVisible: visible(root.querySelector<HTMLElement>(".persea-unified-notice__reconnect")),
      rendered: root.querySelector<HTMLElement>(".xterm-rows")?.textContent ?? "",
      toolbar: rect ? { x: rect.x, y: rect.y, width: rect.width, height: rect.height } : null,
    };
  });
  return {
    panes,
    sent: [...sent],
    resizeRequests: sent.filter((entry) => entry.type === "RESIZE_REQUEST").length,
    inputFrames: sent.filter((entry) => entry.type === "INPUT").length,
    modeRequests: sent.filter((entry) => entry.type === "MODE_REQUEST").map((entry) => entry.mode),
    detaches: [...detaches],
    finalized: [...finalized],
    caughtUp: [...caughtUp],
    focusEvents,
    transportOpens,
    socketConstructions,
    reconnectRequests,
    activeTag: document.activeElement?.tagName ?? null,
  };
}

function reset(count: number, capabilityMode: "control" | "observe" = "control"): Record<string, unknown> {
  for (const pane of mounted.splice(0)) pane.page.destroy();
  sent.splice(0);
  detaches.splice(0);
  finalized.splice(0);
  caughtUp.splice(0);
  focusEvents = 0;
  transportOpens = 0;
  socketConstructions = 0;
  reconnectRequests = 0;
  const host = document.getElementById("loading-host");
  if (!(host instanceof HTMLElement)) throw new Error("loading host missing");
  host.replaceChildren();
  host.dataset.count = String(count);
  for (let index = 0; index < count; index += 1) {
    const name = `pane-${String(index + 1).padStart(2, "0")}`;
    const root = document.createElement("section");
    root.className = "loading-harness__pane";
    root.dataset.name = name;
    host.append(root);
    const pane: Mounted = { page: undefined as unknown as UnifiedTerminalPage, generation: index + 1, cut: BigInt(index + 11), name };
    pane.page = new UnifiedTerminalPage({
      root,
      port: {
        trySend: (_generation, value) => { sent.push({ pane: name, type: value.type, ...(value.type === "MODE_REQUEST" ? { mode: value.mode } : {}) }); return "ACCEPTED"; },
        finalize: (intent) => { finalized.push(intent.cause); },
        detach: (reason) => { detaches.push(reason ?? ""); },
        connectionCaughtUp: (generation) => { caughtUp.push(generation); },
        attachAgain: () => {
          reconnectRequests += 1;
          pane.page.reconnectStatus({ state: "WAITING", attempt: 1, delayMs: 0 });
        },
      },
      capabilityMode,
      styleNonce: NONCE,
      sessionName: name,
      rememberSource: () => undefined,
      claimFocusOnCommit: () => false,
    });
    pane.page.openTransport(pane.generation);
    transportOpens += 1;
    mounted.push(pane);
  }
  return snapshot();
}

async function prepareAll(): Promise<Record<string, unknown>> {
  const readyBefore = sent.filter((entry) => entry.type === "READY").length;
  for (const pane of mounted) {
    pane.page.receiveDecoded(pane.generation, frame(pane, {
      type: "PREPARE",
      cut: pane.cut,
      kind: "INITIAL",
      columns: 80,
      rows: 24,
      history: [],
      truncated: false,
      replay: encoder.encode(`LOADING_STATE-REPLAY-${pane.name}\r\n`),
    }));
  }
  const deadline = performance.now() + 5_000;
  while (sent.filter((entry) => entry.type === "READY").length - readyBefore !== mounted.length) {
    if (performance.now() > deadline) throw new Error("replay writes did not settle");
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  return snapshot();
}

function commitAll(): Record<string, unknown> {
  const before = snapshot();
  for (const pane of mounted) pane.page.receiveDecoded(pane.generation, frame(pane, { type: "COMMIT", cut: pane.cut }));
  // Both snapshots are taken in this one JavaScript task. This is the causal
  // fence: a loader removed by a timer/frame after COMMIT fails the test.
  return { before, after: snapshot() };
}

function modeAll(mode: "CONTROL" | "OBSERVE" = "CONTROL"): Record<string, unknown> {
  for (const pane of mounted) pane.page.receiveDecoded(pane.generation, frame(pane, { type: "MODE", mode }));
  return snapshot();
}

function advanceWallClock(ms: number): Record<string, unknown> {
  wallClockOffset += ms;
  return snapshot();
}

// The session switcher's identity commit, before the new session's endpoint
// opens.
function switchSessionAll(): Record<string, unknown> {
  for (const pane of mounted) pane.page.replaceSessionPresentation({ sessionName: `${pane.name}-next`, composerStorageScope: `${pane.name}-next` });
  return snapshot();
}

// The transport's next attempt: a new generation and cut for every pane.
function readmitAll(): Record<string, unknown> {
  for (const pane of mounted) {
    pane.generation += 100;
    pane.cut += 100n;
    pane.page.openTransport(pane.generation);
  }
  return snapshot();
}

type TerminalProbe = {
  write(data: string | Uint8Array, callback?: () => void): void;
  buffer: { active: { length: number; getLine(row: number): { translateToString(trim?: boolean): string } | undefined } };
};
const terminalOf = (pane: Mounted): TerminalProbe => (pane.page as unknown as { terminal: TerminalProbe }).terminal;
const bufferHas = (terminal: TerminalProbe, marker: string): boolean => {
  const active = terminal.buffer.active;
  for (let row = Math.max(0, active.length - 50); row < active.length; row += 1) {
    if (active.getLine(row)?.translateToString(true).includes(marker)) return true;
  }
  return false;
};

// The flow acknowledgement for a frame fires only once the terminal holds
// everything the frame wrote. A frame near the LIVE limit takes several of
// xterm's write batches; its acknowledgement point is requested at once, as
// the transport does.
async function acknowledgeAfterWrite(): Promise<Record<string, unknown>> {
  const pane = mounted[0]!;
  const terminal = terminalOf(pane);
  const marker = "FLOW-ACK-FENCE-END";
  const data = encoder.encode(`${"ack-body ".repeat(50_000)}\r\n${marker}\r\n`);
  pane.page.receiveDecoded(pane.generation, frame(pane, { type: "LIVE", cut: pane.cut, data }));
  const markerAtAck = await new Promise<boolean>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("acknowledgement never fired")), 10_000);
    pane.page.afterConsumed(pane.generation, () => { clearTimeout(timer); resolve(bufferHas(terminal, marker)); });
  });
  return { markerAtAck, markerNow: bufferHas(terminal, marker), bytes: data.byteLength };
}

// A write callback that throws must not stop xterm's write loop: the
// attachment ends with a failure, and later writes still complete.
async function throwingWriteCallback(): Promise<Record<string, unknown>> {
  const pane = mounted[0]!;
  const terminal = terminalOf(pane);
  const page = pane.page as unknown as { syncNativeScroll: (...args: unknown[]) => void };
  page.syncNativeScroll = () => { throw new Error("write callback failure"); };
  pane.page.receiveDecoded(pane.generation, frame(pane, { type: "LIVE", cut: pane.cut, data: encoder.encode("CALLBACK-THROWS\r\n") }));
  const loopAlive = await new Promise<boolean>((resolve) => {
    const timer = setTimeout(() => resolve(false), 3_000);
    terminal.write("AFTER-THE-FAILURE\r\n", () => { clearTimeout(timer); resolve(true); });
  });
  delete (page as { syncNativeScroll?: unknown }).syncNativeScroll;
  return { ...snapshot(), loopAlive, afterWritten: bufferHas(terminal, "AFTER-THE-FAILURE") };
}

// The PREPARE replay write's callback throws: the attachment ends with
// REPLAY_FAILED, and later writes still complete.
async function throwingReplayCallback(): Promise<Record<string, unknown>> {
  const pane = mounted[0]!;
  const terminal = terminalOf(pane);
  const page = pane.page as unknown as { syncNativeScroll: (...args: unknown[]) => void };
  page.syncNativeScroll = () => { throw new Error("replay callback failure"); };
  pane.page.receiveDecoded(pane.generation, frame(pane, {
    type: "PREPARE", cut: pane.cut, kind: "INITIAL", columns: 80, rows: 24, history: [], truncated: false,
    replay: encoder.encode("REPLAY-CALLBACK-THROWS\r\n"),
  }));
  const loopAlive = await new Promise<boolean>((resolve) => {
    const timer = setTimeout(() => resolve(false), 3_000);
    terminal.write("AFTER-THE-REPLAY-FAILURE\r\n", () => { clearTimeout(timer); resolve(true); });
  });
  delete (page as { syncNativeScroll?: unknown }).syncNativeScroll;
  return { ...snapshot(), loopAlive, afterWritten: bufferHas(terminal, "AFTER-THE-REPLAY-FAILURE") };
}

// The flow acknowledgement callback throws: the attachment ends, and later
// writes still complete.
async function throwingAcknowledgement(): Promise<Record<string, unknown>> {
  const pane = mounted[0]!;
  const terminal = terminalOf(pane);
  pane.page.afterConsumed(pane.generation, () => { throw new Error("acknowledgement failure"); });
  const loopAlive = await new Promise<boolean>((resolve) => {
    const timer = setTimeout(() => resolve(false), 3_000);
    terminal.write("AFTER-THE-ACK-FAILURE\r\n", () => { clearTimeout(timer); resolve(true); });
  });
  return { ...snapshot(), loopAlive, afterWritten: bufferHas(terminal, "AFTER-THE-ACK-FAILURE") };
}

function failAll(reason = "unified_unavailable"): Record<string, unknown> {
  for (const pane of mounted) pane.page.transportClosed(pane.generation, reason);
  return snapshot();
}

// Without a reason: the fast phase is spent and the transport went OFFLINE.
// With one: an unrecoverable EXHAUSTED outcome.
function exhaustAll(reason?: string): Record<string, unknown> {
  for (const pane of mounted) {
    pane.page.transportClosed(pane.generation, "transport_error");
    pane.page.reconnectStatus(reason === undefined ? { state: "OFFLINE", attempt: 6 } : { state: "EXHAUSTED", attempt: 6, reason });
  }
  return snapshot();
}

declare global {
  interface Window {
    __loading_state: {
      reset(count: number, capabilityMode?: "control" | "observe"): Record<string, unknown>;
      prepareAll(): Promise<Record<string, unknown>>;
      commitAll(): Record<string, unknown>;
      modeAll(mode?: "CONTROL" | "OBSERVE"): Record<string, unknown>;
      readmitAll(): Record<string, unknown>;
      advanceWallClock(ms: number): Record<string, unknown>;
      switchSessionAll(): Record<string, unknown>;
      acknowledgeAfterWrite(): Promise<Record<string, unknown>>;
      throwingWriteCallback(): Promise<Record<string, unknown>>;
      throwingReplayCallback(): Promise<Record<string, unknown>>;
      throwingAcknowledgement(): Promise<Record<string, unknown>>;
      failAll(reason?: string): Record<string, unknown>;
      exhaustAll(reason?: string): Record<string, unknown>;
      snapshot(): Record<string, unknown>;
    };
  }
}

window.__loading_state = { reset, prepareAll, commitAll, modeAll, readmitAll, advanceWallClock, switchSessionAll, acknowledgeAfterWrite, throwingWriteCallback, throwingReplayCallback, throwingAcknowledgement, failAll, exhaustAll, snapshot };
document.body.dataset.loading_stateReady = "true";
