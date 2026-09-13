import { UnifiedTerminalPage } from "../src/unified_terminal_page";

const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const encoder = new TextEncoder();

type Mounted = {
  root: HTMLElement;
  page: UnifiedTerminalPage;
  state: any;
  sent: Array<{ generation: number; type: string }>;
  finalized: Array<{ generation: number; cause: string }>;
  toasts: string[];
};

function mount(name: string): Mounted {
  const root = document.createElement("section");
  root.dataset.name = name;
  document.getElementById("host")!.append(root);
  const sent: Mounted["sent"] = [];
  const finalized: Mounted["finalized"] = [];
  const toasts: string[] = [];
  const page = new UnifiedTerminalPage({
    root,
    port: {
      trySend: (generation, value) => { sent.push({ generation, type: value.type }); return "ACCEPTED"; },
      finalize: (value) => { finalized.push(value); },
      detach: () => undefined,
    },
    capabilityMode: "control",
    styleNonce: NONCE,
    sessionName: name,
    rememberSource: () => undefined,
    claimFocusOnCommit: () => false,
  });
  const state = page as any;
  state.showToast = (text: string) => { toasts.push(text); };
  return { root, page, state, sent, finalized, toasts };
}

function stage(mounted: Mounted, operation: string, before: number, after: number): void {
  const { page, state } = mounted;
  page.openTransport(1);
  state.prepared = { source: `source-${operation}`, epoch: 1n, cut: 1n };
  state.committed = true;
  state.controlGranted = true;
  state.pendingRefit = {
    operation,
    predecessorSource: `source-${operation}`,
    predecessorIncarnation: `incarnation-${operation}`,
    predecessorGeneration: 1,
    predecessorEndpointOperation: state.endpointOperation,
    successorSource: "",
    awaitingSuccessor: false,
    droppedBytes: 0,
  };
  state.sendInput(new Uint8Array(before));
  page.transportClosed(1, "generation_refit");
  state.sendInput(new Uint8Array(after));
}

function expectToast(mounted: Mounted, bytes: number, label: string): void {
  const expected = `${bytes} input bytes were not sent during width refit`;
  if (mounted.toasts.length !== 1 || mounted.toasts[0] !== expected) {
    throw new Error(`${label}: expected one ${JSON.stringify(expected)}, got ${JSON.stringify(mounted.toasts)}`);
  }
  if (mounted.state.pendingRefit !== undefined) throw new Error(`${label}: settlement retained the pending owner`);
}

async function matchingSuccess(): Promise<Record<string, unknown>> {
  const pane = mount("matching-success");
  stage(pane, "match-op", 32, 18);
  pane.state.pendingRefit.successorSource = "successor-exact";
  pane.page.openTransport(2);
  pane.page.receiveDecoded(2, {
    type: "PREPARE", version: 1, source: "successor-exact", epoch: 2n, cut: 2n,
    kind: "INITIAL", columns: 91, rows: 24, history: [], truncated: false,
    replay: new Uint8Array(),
  });
  const deadline = performance.now() + 5_000;
  while (!pane.sent.some((entry) => entry.generation === 2 && entry.type === "READY")) {
    if (performance.now() > deadline) throw new Error("matching-success: READY timeout");
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  pane.page.receiveDecoded(2, { type: "COMMIT", version: 1, source: "successor-exact", epoch: 2n, cut: 2n });
  expectToast(pane, 50, "matching-success");
  pane.page.destroy();
  return { toasts: pane.toasts, finalized: pane.finalized, sent: pane.sent };
}

function terminalLoss(): Record<string, unknown> {
  const pane = mount("terminal-loss");
  stage(pane, "loss-op", 32, 18);
  pane.page.transportClosed(1, "stale_target");
  expectToast(pane, 50, "terminal-loss");
  pane.page.destroy();
  return { toasts: pane.toasts };
}

function sessionSwitch(): Record<string, unknown> {
  const pane = mount("session-switch");
  stage(pane, "switch-op", 32, 18);
  pane.page.replaceSessionPresentation({ sessionName: "replacement", composerStorageScope: "replacement" });
  expectToast(pane, 50, "session-switch");
  pane.page.destroy();
  return { toasts: pane.toasts };
}

function dispose(): Record<string, unknown> {
  const pane = mount("dispose");
  stage(pane, "dispose-op", 32, 18);
  pane.page.destroy();
  expectToast(pane, 50, "dispose");
  return { toasts: pane.toasts };
}

function doubleSettlement(): Record<string, unknown> {
  const pane = mount("double-settlement");
  stage(pane, "double-op", 32, 18);
  pane.state.settlePendingRefit("double-op");
  pane.state.settlePendingRefit("double-op");
  pane.page.transportClosed(1, "stale_target");
  expectToast(pane, 50, "double-settlement");
  pane.page.destroy();
  return { toasts: pane.toasts };
}

function unrelatedSuccessor(): Record<string, unknown> {
  const pane = mount("unrelated-successor");
  stage(pane, "unrelated-op", 32, 18);
  pane.state.pendingRefit.successorSource = "successor-exact";
  pane.page.openTransport(2);
  pane.page.receiveDecoded(2, {
    type: "PREPARE", version: 1, source: "successor-wrong", epoch: 2n, cut: 2n,
    kind: "INITIAL", columns: 99, rows: 24, history: [], truncated: false,
    replay: new Uint8Array(),
  });
  if (!pane.state.pendingRefit || pane.toasts.length !== 0 || pane.finalized.length !== 1 || pane.finalized[0].cause !== "ACTIVE_TUPLE_MISMATCH") {
    throw new Error(`unrelated-successor: wrong source changed owner ${JSON.stringify({ pending: Boolean(pane.state.pendingRefit), toasts: pane.toasts, finalized: pane.finalized })}`);
  }
  pane.state.sendInput(new Uint8Array(7));
  pane.page.transportClosed(2, "stale_target");
  expectToast(pane, 57, "unrelated-successor");
  pane.page.transportClosed(2, "stale_target");
	if (pane.toasts.slice().length !== 1) throw new Error(`unrelated-successor: redundant transition reported twice: ${JSON.stringify(pane.toasts)}`);
  pane.page.destroy();
  return { toasts: pane.toasts, finalized: pane.finalized };
}

function twoPanes(): Record<string, unknown> {
  const first = mount("two-first");
  const second = mount("two-second");
  stage(first, "first-op", 3, 4);
  stage(second, "second-op", 5, 6);
  first.state.settlePendingRefit("first-op");
  if (!second.state.pendingRefit || second.toasts.length !== 0) throw new Error("two-panes: first settlement touched second");
  second.page.transportClosed(1, "stale_target");
  expectToast(first, 7, "two-panes first");
  expectToast(second, 11, "two-panes second");
  first.page.destroy();
  second.page.destroy();
  return { first: first.toasts, second: second.toasts };
}

function ordinaryDisconnected(): Record<string, unknown> {
  const pane = mount("ordinary-disconnected");
  pane.page.openTransport(1);
  pane.state.sendInput(encoder.encode("ordinary"));
  if (pane.toasts.length !== 0 || pane.sent.some((entry) => entry.type === "INPUT")) throw new Error("ordinary disconnected input changed behavior");
  pane.page.destroy();
  return { toasts: pane.toasts, sent: pane.sent };
}

async function run(): Promise<Record<string, unknown>> {
  return {
    matchingSuccess: await matchingSuccess(),
    terminalLoss: terminalLoss(),
    sessionSwitch: sessionSwitch(),
    dispose: dispose(),
    doubleSettlement: doubleSettlement(),
    unrelatedSuccessor: unrelatedSuccessor(),
    twoPanes: twoPanes(),
    ordinaryDisconnected: ordinaryDisconnected(),
  };
}

declare global {
  interface Window {
    __ux12RefitAccounting: { run(): Promise<Record<string, unknown>> };
  }
}

window.__ux12RefitAccounting = { run };
document.body.dataset.ready = "true";
