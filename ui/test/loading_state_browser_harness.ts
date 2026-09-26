import { UnifiedTerminalPage } from "../src/unified_terminal_page";

const SOURCE_PREFIX = "loading_state-source";
const EPOCH = 41n;
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const encoder = new TextEncoder();

type Mounted = { page: UnifiedTerminalPage; generation: number; cut: bigint; name: string };
const mounted: Mounted[] = [];
const sent: Array<{ pane: string; type: string }> = [];
let focusEvents = 0;
let transportOpens = 0;
let socketConstructions = 0;
let reconnectRequests = 0;
document.addEventListener("focusin", () => { focusEvents += 1; }, true);
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
    focusEvents,
    transportOpens,
    socketConstructions,
    reconnectRequests,
    activeTag: document.activeElement?.tagName ?? null,
  };
}

function reset(count: number): Record<string, unknown> {
  for (const pane of mounted.splice(0)) pane.page.destroy();
  sent.splice(0);
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
        trySend: (_generation, value) => { sent.push({ pane: name, type: value.type }); return "ACCEPTED"; },
        finalize: () => undefined,
        detach: () => undefined,
        attachAgain: () => {
          reconnectRequests += 1;
          pane.page.reconnectStatus({ state: "WAITING", attempt: 1, delayMs: 0 });
        },
      },
      capabilityMode: "control",
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
  while (sent.filter((entry) => entry.type === "READY").length !== mounted.length) {
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
      reset(count: number): Record<string, unknown>;
      prepareAll(): Promise<Record<string, unknown>>;
      commitAll(): Record<string, unknown>;
      failAll(reason?: string): Record<string, unknown>;
      exhaustAll(reason?: string): Record<string, unknown>;
      snapshot(): Record<string, unknown>;
    };
  }
}

window.__loading_state = { reset, prepareAll, commitAll, failAll, exhaustAll, snapshot };
document.body.dataset.loading_stateReady = "true";
