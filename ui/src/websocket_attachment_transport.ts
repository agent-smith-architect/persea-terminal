import type { AttachmentPagePort, FinalizeCause, FinalizeIntent, PortSendResult } from "./attachment_port";
import { MAX_ATTACHMENT_WIRE_BYTES, decodeServerFrame, encodeBrowserFrame } from "./attachment_wire";
import type { BrowserFrame, HistoryRequest } from "./attachment_protocol";
import { cloneBrowserFrame } from "./attachment_protocol";
import { HISTORY_CHOICES, type HistoryChoice } from "./dashboard";
import {
  TransportLiveness, browserTransportLivenessRuntime, decodeServerLivenessFrame,
  secureTransportLivenessNonce, type TransportLivenessNonceSource, type TransportLivenessRuntime,
} from "./transport_liveness";
import { FlowAcknowledger } from "./transport_flow";
import { UNIFIED_HANDOFF_REASONS, UNIFIED_TAKEOVER_REASONS } from "./unified_close_policy";
import { decodeServerRefusalFrame } from "./unified_refusal_notice";

export type AttachmentTransportSink = Readonly<{
  openTransport(generation: number): void;
  receiveDecoded(generation: number, value: unknown): "ENQUEUED" | "STALE" | "CLOSED";
  transportClosed(generation: number, reason: string): void;
  reconnectStatus?(status: ReconnectStatus): void;
  // An in-band operational refusal: one request's outcome on a transport
  // that stays open. Nothing about the attachment changes.
  operationalRefusal?(generation: number, code: string): void;
  // Calls done once everything the frame just delivered caused in the
  // terminal has been written. The transport acknowledges the frame to the
  // server only then, which is what paces output to a page that falls
  // behind. Absent means a frame is consumed on delivery.
  afterConsumed?(generation: number, done: () => void): void;
}>;

export type AttachmentEndpoint = Readonly<{ url: string; protocols: readonly string[] }>;
export type EndpointReplacementReason = "takeover_requested" | "session_switch";
// OFFLINE: the fast retry phase is spent. Recovery continues on its own —
// a slow probe while the page is visible, and an immediate one when the
// network or the page comes back — and the page offers Reconnect meanwhile.
// EXHAUSTED is reserved for outcomes a retry cannot change.
export type ReconnectStatus = Readonly<{ state: "DETACHED" | "WAITING" | "ATTEMPTING" | "CONNECTED" | "OFFLINE" | "EXHAUSTED"; attempt: number; delayMs?: number; reason?: string }>;
export type HistoryDepthOutcome =
  | Readonly<{ status: "pending_reader"; desired: HistoryChoice; effective: HistoryChoice; reason: import("./continuous_surface/types").ReconciliationDeferReason }>
  | Readonly<{ status: "effective"; desired: HistoryChoice; effective: HistoryChoice }>
  | Readonly<{ status: "failed"; cause: "reader_hold" | "cut_failure"; requested: HistoryChoice; desired: HistoryChoice; effective: HistoryChoice }>;
export type FreshEndpointProvider = (signal: AbortSignal, source: string) => Promise<AttachmentEndpoint>;
// Re-mints an attachment endpoint from the tab's own session IDENTITY (the URL
// fragment), used when no source binding is available to re-mint from: a
// reopened tab whose one-time handle was already consumed, or a source binding
// that expired. It may resolve the identity, adopt an idle session, and claim
// control — returning a ready endpoint — or reject. A ReconnectRefusal reject
// is a genuinely unresolvable identity (the session is gone, ambiguous, or the
// fragment carries no identity) and exhausts the retry immediately with that
// code; any other reject is treated as ordinary infrastructure loss and
// retried within the budget.
export type IdentityEndpointProvider = (signal: AbortSignal) => Promise<AttachmentEndpoint>;

// A terminal identity outcome. Its code is a canonical close reason the page's
// close policy maps to reviewed operator copy.
export class ReconnectRefusal extends Error {
  constructor(public readonly code: string) {
    super(code);
    this.name = "ReconnectRefusal";
  }
}
export type ReconnectRuntime = Readonly<{
  now(): number;
  setTimeout(callback: () => void, delayMs: number): unknown;
  clearTimeout(handle: unknown): void;
  // Signals that connectivity or the page may be back (online, a visible
  // page, a restored page, focus). Absent means no such signals.
  subscribeWake?(callback: () => void): () => void;
  // Absent means always visible.
  visible?(): boolean;
}>;

export const browserReconnectRuntime: ReconnectRuntime = Object.freeze({
  now: () => Date.now(),
  setTimeout: (callback: () => void, delayMs: number) => globalThis.setTimeout(callback, delayMs),
  clearTimeout: (handle: unknown) => globalThis.clearTimeout(handle as ReturnType<typeof setTimeout>),
  subscribeWake: (callback: () => void) => {
    const wake = () => callback();
    const visible = () => { if (document.visibilityState === "visible") callback(); };
    globalThis.addEventListener("online", wake);
    globalThis.addEventListener("focus", wake);
    globalThis.addEventListener("pageshow", wake);
    document.addEventListener("visibilitychange", visible);
    return () => {
      globalThis.removeEventListener("online", wake);
      globalThis.removeEventListener("focus", wake);
      globalThis.removeEventListener("pageshow", wake);
      document.removeEventListener("visibilitychange", visible);
    };
  },
  visible: () => document.visibilityState === "visible",
});

export type AttachmentWebSocket = Pick<WebSocket,
  "readyState" | "bufferedAmount" | "send" | "close" | "addEventListener"
>;

export type AttachmentWebSocketFactory = (url: string, protocols?: string[]) => AttachmentWebSocket;

type ReconnectAttempt = {
  readonly token: number;
  readonly attempt: number;
  readonly controller: AbortController;
  deadlineTimer?: unknown;
  socket?: AttachmentWebSocket;
  generation?: number;
};

type ActiveLiveness = Readonly<{
  socket: AttachmentWebSocket;
  generation: number;
  heartbeat: TransportLiveness;
}>;

// One WebSocket's flow acknowledgements. A superseded one needs no retiring:
// it can only send on the socket it was made for, and only while that socket
// is still this transport's open socket.
type ActiveFlow = Readonly<{
  socket: AttachmentWebSocket;
  generation: number;
  acks: FlowAcknowledger;
}>;

const SOCKET_CONNECTING = 0;
const SOCKET_OPEN = 1;
// The fast phase of one loss episode: up to MAX_RECONNECT_ATTEMPTS attempts
// within MAX_RECONNECT_ELAPSED_MS of disconnected time; time spent connected
// between momentary commits does not count. After it, the episode goes OFFLINE
// and probes every OFFLINE_RETRY_MS while the page is visible, so an outage
// longer than the fast phase still recovers without the operator.
export const MAX_RECONNECT_ATTEMPTS = 6;
export const MAX_RECONNECT_ELAPSED_MS = 30_000;
export const MAX_RECONNECT_ATTEMPT_MS = 5_000;
export const OFFLINE_RETRY_MS = 15_000;
// A wake signal brings the next OFFLINE probe forward, but never closer than
// this to the previous attempt's start: signals can arrive in bursts (focus,
// visibility and online together, or a page toggled repeatedly), and each
// probe costs the server an endpoint mint.
export const OFFLINE_WAKE_SPACING_MS = 3_000;
// A connection ends a loss episode only once its view has caught up and then
// stayed up this long (one full liveness proof lifetime). Catching up is the
// first MODE after COMMIT, which follows the whole history backlog. A
// connection that drops at once, or before its view catches up, keeps its
// episode: a commit-then-close loop, or a slow link that cannot finish the
// backlog, spends the fast phase and settles into OFFLINE probing instead of
// starting over with every attempt.
export const STABLE_CONNECTION_MS = 20_000;
const RECONNECT_BASE_DELAY_MS = 250;
const RECONNECT_MAX_DELAY_MS = 4_000;
// Page script may close a WebSocket only with 1000 or an application code in
// 3000–4999. Any other code throws InvalidAccessError and leaves the socket
// open after this transport has already let go of it, so an abandoned attempt
// would keep its server attachment and control lease alive. Client faults use
// application codes; the reason text carries the specific cause.
export const CLOSE_ORDERLY = 1000;
export const CLOSE_CLIENT_PROTOCOL_FAULT = 4002;
export const CLOSE_CLIENT_FAULT = 4011;
export const HISTORY_DEPTH_PENDING_TIMEOUT_MS = 60_000;

function historyProtocolValue(protocols?: readonly string[]): HistoryChoice | undefined {
  if (!protocols) return undefined;
  const values = protocols.filter((protocol) => protocol.startsWith("persea-history."));
  if (values.length !== 1) return undefined;
  const parsed = Number(values[0]!.slice("persea-history.".length));
  return HISTORY_CHOICES.find((choice) => choice === parsed);
}

function withHistoryProtocol(protocols: readonly string[], desired?: HistoryChoice): readonly string[] {
  if (desired === undefined) return protocols;
  const result = protocols.filter((protocol) => !protocol.startsWith("persea-history."));
  result.push(`persea-history.${desired}`);
  return Object.freeze(result);
}

const protocolFaults = new Set<FinalizeCause>([
  "MALFORMED_FRAME", "OUT_OF_STATE", "ACTIVE_TUPLE_MISMATCH", "ADMISSION_INVARIANT",
]);
const protocolFaultReasons = new Set(["malformed_frame", "non_text_frame", "liveness_protocol", "refusal_protocol"]);

function closeSocket(socket: AttachmentWebSocket, code: number, reason: string): void {
  // Every code used here is one the browser accepts; a throw can only mean
  // the socket is already closing, which is the outcome wanted.
  try { socket.close(code, reason.slice(0, 64)); } catch { /* already closing */ }
}
// The takeover policy set is the close policy's exported classification —
// never a private copy, so the transport and the page cannot drift apart.
const takeoverPolicyReasons = UNIFIED_TAKEOVER_REASONS;

export class WebSocketAttachmentTransport implements AttachmentPagePort {
  private sink?: AttachmentTransportSink;
  private socket?: AttachmentWebSocket;
  private generation = 0;
  private closedGeneration = 0;
  private retryTimer?: unknown;
  private retryDueAt?: number;
  private lastAttemptStartedAt?: number;
  private retryAttempt = 0;
  private episodeActive = false;
  // Disconnected time of the current episode: banked at each COMMIT, plus the
  // loss in progress (lossStartedAt, unset while a connection is committed).
  private retrySpentMs = 0;
  private lossStartedAt?: number;
  private retryToken = 0;
  private retryDisabled = false;
  private offline = false;
  private committedAt?: number;
  private caughtUpAt?: number;
  private unsubscribeWake?: () => void;
  private activeAttempt?: ReconnectAttempt;
  private activeLiveness?: ActiveLiveness;
  private activeFlow?: ActiveFlow;
  private preparedSource?: string;
  private preparedSourceGeneration = 0;
  private effectiveHistoryRows?: HistoryChoice;
  private desiredHistoryRows?: HistoryChoice;
  private pendingHistoryDepth = false;
  private consecutiveHistoryDepthFailures = 0;
  private failedDepthGeneration = 0;
  private readonly attachDepthByGeneration = new Map<number, HistoryChoice>();
  private preparedDepth?: Readonly<{ generation: number; cut: bigint; rows: HistoryChoice }>;
  private preparedAttachDepth?: Readonly<{ generation: number; cut: bigint; rows: HistoryChoice }>;
  private pendingHistoryTimer?: unknown;
  private readonly historyDepthListeners = new Set<(outcome: HistoryDepthOutcome) => void>();

  constructor(
    private url: string,
    private readonly createSocket: AttachmentWebSocketFactory = (value, protocols) => new WebSocket(value, protocols),
    private protocols?: readonly string[],
    private readonly freshEndpoint?: FreshEndpointProvider,
    private readonly random: () => number = Math.random,
    private readonly retryRuntime: ReconnectRuntime = browserReconnectRuntime,
    private readonly livenessRuntime: TransportLivenessRuntime = browserTransportLivenessRuntime,
    private readonly livenessNonce: TransportLivenessNonceSource = secureTransportLivenessNonce,
    // Appended at the tail so the established positional callers are untouched.
    private readonly identityEndpoint?: IdentityEndpointProvider,
  ) {
    if (url.length === 0) throw new Error("attachment WebSocket URL is required");
    const initialHistory = historyProtocolValue(protocols);
    this.effectiveHistoryRows = initialHistory;
    this.desiredHistoryRows = initialHistory;
  }

  bind(sink: AttachmentTransportSink): void {
    if (this.sink) throw new Error("attachment transport sink is already bound");
    this.sink = sink;
  }

  onHistoryDepthOutcome(listener: (outcome: HistoryDepthOutcome) => void): () => void {
    this.historyDepthListeners.add(listener);
    let active = true;
    return () => {
      if (!active) return;
      active = false;
      this.historyDepthListeners.delete(listener);
    };
  }

  retryHistoryDepth(generation: number, frame: HistoryRequest): PortSendResult {
    this.clearPendingHistoryTimer();
    return this.trySend(generation, frame);
  }

  connect(): number {
    return this.openSocket(this.url, this.protocols);
  }

  trySend(generation: number, frame: BrowserFrame): PortSendResult {
    let validated: BrowserFrame;
    try { validated = cloneBrowserFrame(frame); } catch { return "CLOSED"; }
    if (validated.type === "HISTORY_REQUEST") this.advanceDesiredHistory(validated.historyRows);
    const socket = this.socket;
    if (!socket || generation !== this.generation) return "CLOSED";
    if (frame.type === "INPUT") {
      const liveness = this.activeLiveness;
      if (!liveness || liveness.socket !== socket || liveness.generation !== generation) {
        if (socket.readyState === SOCKET_OPEN) {
          return this.closeCurrent(socket, generation, "liveness_unavailable", true) ? "TRANSPORT_LOST" : "CLOSED";
        }
      } else if (!liveness.heartbeat.beforeInput()) {
        return "TRANSPORT_LOST";
      }
    }
    let payload: string;
    try { payload = encodeBrowserFrame(validated); } catch { return "CLOSED"; }
    if (socket.readyState !== SOCKET_OPEN) {
      return this.closeCurrent(socket, generation, "transport_send_unavailable", true) ? "TRANSPORT_LOST" : "CLOSED";
    }
    const bytes = new TextEncoder().encode(payload).byteLength;
    if (socket.bufferedAmount > MAX_ATTACHMENT_WIRE_BYTES - bytes) return "SATURATED";
    try { socket.send(payload); } catch {
      return this.closeCurrent(socket, generation, "transport_send_failed", true) ? "TRANSPORT_LOST" : "CLOSED";
    }
    if (validated.type === "DEFER") this.recordHistoryDepthDefer(generation, validated.cut, validated.reason);
    return "ACCEPTED";
  }

  finalize(intent: FinalizeIntent): void {
    if (intent.generation !== this.generation) return;
    this.retryDisabled = true;
    this.clearPendingHistoryTimer();
    this.cancelRetryWork();
    this.setOffline(false);
    this.bankLoss();
    const socket = this.socket;
    this.cancelLiveness(socket, intent.generation);
    this.socket = undefined;
    if (socket) closeSocket(socket, protocolFaults.has(intent.cause) ? CLOSE_CLIENT_PROTOCOL_FAULT : CLOSE_CLIENT_FAULT, intent.cause);
    // The page stops this generation, so it must also be told it ended:
    // otherwise its last frame stays on screen with no notice and no action.
    this.notifyClosed(intent.generation, "attachment_fault");
    this.closedGeneration = Math.max(this.closedGeneration, intent.generation);
  }

  private recordPreparedSource(generation: number, source: string): void {
    if (generation !== this.generation || source.length === 0) return;
    if (this.preparedSource !== undefined && this.preparedSource !== source && this.preparedSourceGeneration === generation) {
      throw new Error("attachment source changed within one transport generation");
    }
    this.preparedSource = source;
    this.preparedSourceGeneration = generation;
  }

  connectionCommitted(generation: number): void {
    if (generation !== this.generation) return;
    this.cancelRetryWork();
    this.setOffline(false);
    // The episode's budget is kept until this connection proves stable; see
    // STABLE_CONNECTION_MS and beginLossEpisode. Its connected time is free.
    this.bankLoss();
    this.committedAt = this.retryRuntime.now();
    this.caughtUpAt = undefined;
    this.sink?.reconnectStatus?.(Object.freeze({ state: "CONNECTED", attempt: 0 }));
  }

  // The committed view has caught up: its first MODE arrived, after the whole
  // history backlog. From here the connection can prove itself stable; see
  // STABLE_CONNECTION_MS and beginLossEpisode.
  connectionCaughtUp(generation: number): void {
    if (generation !== this.generation || this.committedAt === undefined || this.caughtUpAt !== undefined) return;
    this.caughtUpAt = this.retryRuntime.now();
  }

  attachAgain(): void {
    if (!this.freshEndpoint) {
      this.sink?.reconnectStatus?.(Object.freeze({ state: "EXHAUSTED", attempt: 0, reason: "reconnect_unavailable" }));
      return;
    }
    if (this.socket && !this.activeAttempt && (this.socket.readyState === SOCKET_CONNECTING || this.socket.readyState === SOCKET_OPEN)) return;
    const superseded = this.socket;
    const supersededGeneration = this.generation;
    this.retryDisabled = false;
    this.cancelRetryWork();
    this.setOffline(false);
    if (superseded && this.socket === superseded) {
      this.cancelLiveness(superseded, supersededGeneration);
      this.socket = undefined;
      this.notifyClosed(supersededGeneration, "attach_again_superseded");
      closeSocket(superseded, CLOSE_ORDERLY, "attach_again_superseded");
    }
    this.committedAt = undefined;
    this.caughtUpAt = undefined;
    this.startEpisode();
    this.scheduleReconnect(0);
  }

  takeControl(endpoint: AttachmentEndpoint): void {
    this.replaceEndpoint(endpoint, "takeover_requested");
  }

  // One endpoint-replacement primitive. A session switch additionally starts
  // a fresh retry budget for the newly committed identity before opening its
  // socket. Neither leaves retries disabled: the replacement is an ordinary
  // attachment, so a later loss — even before its COMMIT — recovers like any
  // other. A close that refuses control (control_displaced,
  // takeover_superseded) still stops recovery in onClose.
  replaceEndpoint(endpoint: AttachmentEndpoint, reason: EndpointReplacementReason): void {
    if (!endpoint || endpoint.url.length === 0 || endpoint.protocols.length === 0) throw new Error("takeover endpoint is unavailable");
    this.retryDisabled = false;
    this.cancelRetryWork();
    this.setOffline(false);
    // A claim that ends a loss resumes its clock; the wait for the operator
    // before it was banked when retries stopped.
    if (reason === "takeover_requested" && this.episodeActive && this.committedAt === undefined) {
      this.lossStartedAt ??= this.retryRuntime.now();
    }
    if (reason === "session_switch") {
      this.committedAt = undefined;
      this.caughtUpAt = undefined;
      this.startEpisode();
      this.preparedSource = undefined;
      this.preparedSourceGeneration = 0;
      this.preparedDepth = undefined;
    }
    const prior = this.socket;
    const priorGeneration = this.generation;
    if (prior) {
      this.cancelLiveness(prior, priorGeneration);
      this.socket = undefined;
      this.notifyClosed(priorGeneration, reason);
      closeSocket(prior, CLOSE_ORDERLY, reason);
    }
    this.url = endpoint.url;
    this.protocols = [...endpoint.protocols];
    this.openSocket(endpoint.url, endpoint.protocols);
    this.protocols = undefined;
  }

  endpointReplacementBusy(): boolean {
    // An OFFLINE probe timer only retries the old endpoint slowly; a session
    // switch cancels it and starts its own episode, so it must not wait.
    return this.activeAttempt !== undefined || (this.retryTimer !== undefined && !this.offline);
  }

  // True while the transport is live or still recovering on its own; false
  // once it stopped for the page (a refusal, a burst stop, a detach). A page
  // restored from the back/forward cache reattaches only in the first case.
  recoveryActive(): boolean {
    return !this.retryDisabled;
  }

  // First half of a session-switch endpoint replacement. It invalidates every
  // old retry/remint callback without touching A's still-live socket; the
  // controller then changes identity and supplies the already-built B
  // endpoint in the same non-async task.
  invalidateEndpointWork(): void {
    this.cancelRetryWork();
  }

  sessionSwitchState(): Readonly<{
    retryToken: number;
    generation: number;
    socketState: "none" | "connecting" | "open" | "closing" | "closed";
  }> {
    const socket = this.socket;
    const socketState = socket === undefined ? "none"
      : socket.readyState === SOCKET_CONNECTING ? "connecting"
      : socket.readyState === SOCKET_OPEN ? "open"
      : socket.readyState === 2 ? "closing"
      : "closed";
    return Object.freeze({ retryToken: this.retryToken, generation: this.generation, socketState });
  }

  detach(reason = "detached"): void {
    this.retryDisabled = true;
    this.clearPendingHistoryTimer();
    this.cancelRetryWork();
    this.setOffline(false);
    this.bankLoss();
    this.disconnect(reason);
    this.sink?.reconnectStatus?.(Object.freeze({ state: "DETACHED", attempt: this.retryAttempt, reason }));
  }

  destroy(): void {
    this.retryDisabled = true;
    this.clearPendingHistoryTimer();
    this.cancelRetryWork();
    this.setOffline(false);
    this.disconnect("destroyed");
  }

  disconnect(reason = "client_disconnect"): void {
    this.clearPendingHistoryTimer();
    const socket = this.socket;
    if (!socket) return;
    const generation = this.generation;
    this.cancelLiveness(socket, generation);
    this.socket = undefined;
    this.notifyClosed(generation, reason);
    closeSocket(socket, CLOSE_ORDERLY, reason);
  }

  private openSocket(url: string, protocols?: readonly string[], attempt?: ReconnectAttempt): number {
    if (!this.sink) throw new Error("attachment transport has no sink");
    if (this.socket && (this.socket.readyState === SOCKET_CONNECTING || this.socket.readyState === SOCKET_OPEN)) return this.generation;
    if (this.generation >= Number.MAX_SAFE_INTEGER) throw new Error("attachment generation exhausted");
    const generation = ++this.generation;
    const attachDepth = historyProtocolValue(protocols);
    if (attachDepth !== undefined) this.attachDepthByGeneration.set(generation, attachDepth);
    const socket = this.createSocket(url, protocols ? [...protocols] : undefined);
    this.socket = socket;
    if (attempt) {
      attempt.socket = socket;
      attempt.generation = generation;
    }
    this.sink.openTransport(generation);
    socket.addEventListener("open", () => this.onOpen(socket, generation));
    socket.addEventListener("message", (event) => this.onMessage(socket, generation, event as MessageEvent<unknown>));
    socket.addEventListener("close", (event) => this.onClose(socket, generation, event as CloseEvent));
    socket.addEventListener("error", () => this.closeCurrent(socket, generation, "transport_error", true));
    return generation;
  }

  private onOpen(socket: AttachmentWebSocket, generation: number): void {
    if (this.socket !== socket || generation !== this.generation || this.closedGeneration === generation) return;
    this.cancelLiveness();
    const heartbeat = new TransportLiveness(
      (payload) => socket.send(payload),
      () => this.socket === socket && generation === this.generation && socket.readyState === SOCKET_OPEN,
      (reason) => { this.closeCurrent(socket, generation, reason, true); },
      this.livenessRuntime,
      this.livenessNonce,
    );
    this.activeLiveness = Object.freeze({ socket, generation, heartbeat });
    this.activeFlow = Object.freeze({ socket, generation, acks: new FlowAcknowledger((payload) => this.sendFlow(socket, generation, payload)) });
    heartbeat.start();
  }

  private sendFlow(socket: AttachmentWebSocket, generation: number, payload: string): boolean {
    if (this.socket !== socket || generation !== this.generation || socket.readyState !== SOCKET_OPEN) return false;
    try {
      socket.send(payload);
      return true;
    } catch {
      this.closeCurrent(socket, generation, "transport_send_failed", true);
      return false;
    }
  }

  private onMessage(socket: AttachmentWebSocket, generation: number, event: MessageEvent<unknown>): void {
    if (this.socket !== socket || generation !== this.generation || this.closedGeneration === generation) return;
    if (typeof event.data !== "string") {
      this.closeCurrent(socket, generation, "non_text_frame", false);
      return;
    }
    const liveness = this.activeLiveness;
    if (liveness && liveness.socket === socket && liveness.generation === generation) {
      const result = liveness.heartbeat.receive(event.data);
      if (result !== "ORDINARY") return;
    } else if (decodeServerLivenessFrame(event.data).type !== "ORDINARY") {
      this.closeCurrent(socket, generation, "liveness_protocol", true);
      return;
    }
    const refusal = decodeServerRefusalFrame(event.data);
    if (refusal.type === "REFUSAL") {
      this.sink?.operationalRefusal?.(generation, refusal.code);
      return;
    }
    if (refusal.type === "VIOLATION") {
      // The reserved namespace with an unreadable code: a peer protocol
      // violation, judged the same way a malformed liveness pong is.
      this.closeCurrent(socket, generation, "refusal_protocol", true);
      return;
    }
    const flow = this.activeFlow?.socket === socket && this.activeFlow.generation === generation ? this.activeFlow.acks : undefined;
    const sequence = flow?.receive();
    let result: "ENQUEUED" | "STALE" | "CLOSED" | undefined;
    try {
      const frame = decodeServerFrame(event.data);
      if (frame.type === "PREPARE") this.recordPreparedSource(generation, frame.source);
      if (frame.type === "PREPARE" && frame.kind === "HISTORY") {
        this.preparedDepth = Object.freeze({ generation, cut: frame.cut, rows: frame.effectiveHistoryRows });
      } else if (frame.type === "PREPARE" && (frame.kind === "INITIAL" || frame.kind === "RECONNECT")) {
        const rows = this.attachDepthByGeneration.get(generation);
        if (rows !== undefined) this.preparedAttachDepth = Object.freeze({ generation, cut: frame.cut, rows });
      } else if (frame.type === "COMMIT") {
        const prepared = this.preparedDepth;
        if (prepared && prepared.generation === generation && prepared.cut === frame.cut) {
          this.commitHistoryDepth(prepared.rows);
          this.preparedDepth = undefined;
        } else {
          const attach = this.preparedAttachDepth;
          if (attach && attach.generation === generation && attach.cut === frame.cut) {
            this.commitHistoryDepth(attach.rows);
            this.preparedAttachDepth = undefined;
          }
        }
      }
      result = this.sink?.receiveDecoded(generation, frame);
    } catch {
      this.closeCurrent(socket, generation, "malformed_frame", false);
      return;
    }
    if (result === "CLOSED") {
      this.closeCurrent(socket, generation, "page_closed", false);
      return;
    }
    if (!flow || sequence === undefined) return;
    const consumed = () => flow.consume(sequence);
    if (this.sink?.afterConsumed) this.sink.afterConsumed(generation, consumed);
    else consumed();
  }

  private onClose(socket: AttachmentWebSocket, generation: number, event: CloseEvent): void {
    if (this.socket !== socket || generation !== this.generation) return;
    this.cancelLiveness(socket, generation);
    this.socket = undefined;
    this.recordHistoryDepthFailure(generation);
    this.finishSocketAttempt(socket, true);
    const reason = event.reason || `websocket_${event.code}`;
    this.notifyClosed(generation, reason);
    if (takeoverPolicyReasons.has(reason)) {
      this.retryDisabled = true;
      this.cancelRetryWork();
      this.setOffline(false);
      // Waiting for the operator is not disconnected time: a manual claim
      // minutes later must not find the episode's budget spent.
      this.bankLoss();
      return;
    }
    if (UNIFIED_HANDOFF_REASONS.has(reason)) {
      this.committedAt = undefined;
      this.caughtUpAt = undefined;
      this.startEpisode();
    } else {
      this.beginLossEpisode();
    }
    this.scheduleReconnect();
  }

  private closeCurrent(socket: AttachmentWebSocket, generation: number, reason: string, retryable: boolean): boolean {
    if (this.socket !== socket || generation !== this.generation) return false;
    this.cancelLiveness(socket, generation);
    this.socket = undefined;
    if (retryable) this.recordHistoryDepthFailure(generation);
    this.finishSocketAttempt(socket, true);
    this.notifyClosed(generation, reason);
    if (!retryable) {
      this.retryDisabled = true;
      this.cancelRetryWork();
      this.setOffline(false);
      this.bankLoss();
    }
    closeSocket(socket, protocolFaultReasons.has(reason) ? CLOSE_CLIENT_PROTOCOL_FAULT : CLOSE_CLIENT_FAULT, reason);
    if (retryable) {
      this.beginLossEpisode();
      this.scheduleReconnect();
    }
    return true;
  }

  private notifyClosed(generation: number, reason: string): void {
    if (this.closedGeneration === generation) return;
    this.closedGeneration = generation;
    this.sink?.transportClosed(generation, reason);
  }

  private advanceDesiredHistory(rows: HistoryChoice): void {
    if (!HISTORY_CHOICES.includes(rows)) return;
    this.clearPendingHistoryTimer();
    if (this.desiredHistoryRows !== rows) {
      this.consecutiveHistoryDepthFailures = 0;
    }
    this.desiredHistoryRows = rows;
    this.pendingHistoryDepth = this.effectiveHistoryRows !== rows;
  }

  private commitHistoryDepth(rows: HistoryChoice): void {
    this.effectiveHistoryRows = rows;
    this.desiredHistoryRows = rows;
    this.pendingHistoryDepth = false;
    this.consecutiveHistoryDepthFailures = 0;
    this.failedDepthGeneration = 0;
    this.clearPendingHistoryTimer();
    this.emitHistoryDepthOutcome(Object.freeze({ status: "effective", desired: rows, effective: rows }));
  }

  private recordHistoryDepthDefer(generation: number, cut: bigint, reason: import("./continuous_surface/types").ReconciliationDeferReason): void {
    const prepared = this.preparedDepth;
    const desired = this.desiredHistoryRows;
    const effective = this.effectiveHistoryRows;
    if (!prepared || prepared.generation !== generation || prepared.cut !== cut || desired === undefined || effective === undefined) return;
    this.preparedDepth = undefined;
    this.emitHistoryDepthOutcome(Object.freeze({ status: "pending_reader", desired, effective, reason }));
    this.clearPendingHistoryTimer();
    this.pendingHistoryTimer = this.retryRuntime.setTimeout(() => {
      this.pendingHistoryTimer = undefined;
      if (!this.pendingHistoryDepth || this.preparedDepth !== undefined) return;
      const failedDesired = this.desiredHistoryRows;
      const retained = this.effectiveHistoryRows;
      if (failedDesired === undefined || retained === undefined || failedDesired === retained) return;
      this.desiredHistoryRows = retained;
      this.pendingHistoryDepth = false;
      this.consecutiveHistoryDepthFailures = 0;
      this.emitHistoryDepthOutcome(Object.freeze({
        status: "failed",
        cause: "reader_hold",
        requested: failedDesired,
        desired: retained,
        effective: retained,
      }));
    }, HISTORY_DEPTH_PENDING_TIMEOUT_MS);
  }

  private clearPendingHistoryTimer(): void {
    if (this.pendingHistoryTimer === undefined) return;
    this.retryRuntime.clearTimeout(this.pendingHistoryTimer);
    this.pendingHistoryTimer = undefined;
  }

  private emitHistoryDepthOutcome(outcome: HistoryDepthOutcome): void {
    for (const listener of [...this.historyDepthListeners]) listener(outcome);
  }

  private recordHistoryDepthFailure(generation: number): void {
    if (!this.pendingHistoryDepth || this.failedDepthGeneration === generation) return;
    const desired = this.desiredHistoryRows;
    const effective = this.effectiveHistoryRows;
    if (desired === undefined || effective === undefined || desired === effective) return;
    this.clearPendingHistoryTimer();
    this.failedDepthGeneration = generation;
    this.consecutiveHistoryDepthFailures += 1;
    if (this.consecutiveHistoryDepthFailures < 2) return;
    this.desiredHistoryRows = effective;
    this.pendingHistoryDepth = false;
    this.consecutiveHistoryDepthFailures = 0;
    this.preparedDepth = undefined;
    this.clearPendingHistoryTimer();
    this.emitHistoryDepthOutcome(Object.freeze({
      status: "failed",
      cause: "cut_failure",
      requested: desired,
      desired: effective,
      effective,
    }));
  }

  private cancelLiveness(socket?: AttachmentWebSocket, generation?: number): void {
    const active = this.activeLiveness;
    if (!active) return;
    if (socket && active.socket !== socket) return;
    if (generation !== undefined && active.generation !== generation) return;
    this.activeLiveness = undefined;
    active.heartbeat.retire();
  }

  private ensureRetrySession(): void {
    if (this.episodeActive) return;
    this.startEpisode();
  }

  // Judged once, at the moment a connection is lost: only a connection whose
  // view caught up and then stayed up for STABLE_CONNECTION_MS ends the
  // previous episode.
  private beginLossEpisode(): void {
    const caughtUpAt = this.caughtUpAt;
    this.committedAt = undefined;
    this.caughtUpAt = undefined;
    const stable = caughtUpAt !== undefined && this.retryRuntime.now() - caughtUpAt >= STABLE_CONNECTION_MS;
    if (this.episodeActive && !stable) {
      this.lossStartedAt ??= this.retryRuntime.now();
      return;
    }
    this.startEpisode();
  }

  private startEpisode(): void {
    this.setOffline(false);
    this.episodeActive = true;
    this.retryAttempt = 0;
    this.retrySpentMs = 0;
    this.lossStartedAt = this.retryRuntime.now();
    this.retryToken++;
  }

  private bankLoss(): void {
    if (this.lossStartedAt === undefined) return;
    this.retrySpentMs += Math.max(0, this.retryRuntime.now() - this.lossStartedAt);
    this.lossStartedAt = undefined;
  }

  private setOffline(offline: boolean): void {
    this.offline = offline;
    if (offline && !this.unsubscribeWake) {
      this.unsubscribeWake = this.retryRuntime.subscribeWake?.(() => this.onWake());
    } else if (!offline && this.unsubscribeWake) {
      const unsubscribe = this.unsubscribeWake;
      this.unsubscribeWake = undefined;
      unsubscribe();
    }
  }

  // Connectivity or the page may be back: probe now instead of waiting for
  // the slow timer, spaced from the previous attempt. A signal only ever
  // brings the pending probe forward, so a burst collapses into one probe.
  private onWake(): void {
    if (this.retryDisabled || !this.offline || this.activeAttempt) return;
    const now = this.retryRuntime.now();
    const spacing = this.lastAttemptStartedAt === undefined ? 0 : this.lastAttemptStartedAt + OFFLINE_WAKE_SPACING_MS - now;
    const delay = Math.max(0, spacing);
    // Keep an armed probe only while it is still due ahead. One already past
    // due by the clock has not fired because the device slept with the timer
    // paused; waking must replace it rather than wait for it.
    if (this.retryTimer !== undefined && this.retryDueAt !== undefined && now < this.retryDueAt && this.retryDueAt <= now + delay) return;
    this.clearRetryTimer();
    this.scheduleReconnect(delay);
  }

  private armRetry(token: number, delay: number): void {
    this.retryDueAt = this.retryRuntime.now() + delay;
    this.retryTimer = this.retryRuntime.setTimeout(() => {
      this.retryTimer = undefined;
      this.retryDueAt = undefined;
      void this.attemptReconnect(token);
    }, delay);
  }

  private clearRetryTimer(): void {
    if (this.retryTimer !== undefined) this.retryRuntime.clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
    this.retryDueAt = undefined;
  }

  private cancelRetryWork(): void {
    this.retryToken++;
    this.clearRetryTimer();
    if (this.activeAttempt) this.finishAttempt(this.activeAttempt, true);
  }

  private finishAttempt(attempt: ReconnectAttempt, abort: boolean): void {
    if (attempt.deadlineTimer !== undefined) this.retryRuntime.clearTimeout(attempt.deadlineTimer);
    attempt.deadlineTimer = undefined;
    if (abort && !attempt.controller.signal.aborted) attempt.controller.abort();
    if (this.activeAttempt === attempt) this.activeAttempt = undefined;
  }

  private finishSocketAttempt(socket: AttachmentWebSocket, abort: boolean): void {
    const attempt = this.activeAttempt;
    if (attempt?.socket === socket) this.finishAttempt(attempt, abort);
  }

  private remainingRetryMs(): number {
    if (!this.episodeActive) return MAX_RECONNECT_ELAPSED_MS;
    const current = this.lossStartedAt === undefined ? 0 : Math.max(0, this.retryRuntime.now() - this.lossStartedAt);
    return Math.max(0, MAX_RECONNECT_ELAPSED_MS - this.retrySpentMs - current);
  }

  // Outcomes a retry cannot change: an unresolvable identity, or no way to
  // mint an endpoint at all.
  private exhaustRetry(reason: string): void {
    const attempt = this.retryAttempt;
    this.retryDisabled = true;
    this.cancelRetryWork();
    this.setOffline(false);
    this.bankLoss();
    this.sink?.reconnectStatus?.(Object.freeze({ state: "EXHAUSTED", attempt, reason }));
  }

  private fastPhaseSpent(): boolean {
    return this.retryAttempt >= MAX_RECONNECT_ATTEMPTS || this.remainingRetryMs() <= 0;
  }

  private jitter(): number {
    return 0.8 + Math.min(1, Math.max(0, this.random())) * 0.4;
  }

  private scheduleReconnect(delayOverride?: number): void {
    if (this.retryDisabled || this.retryTimer !== undefined || this.activeAttempt) return;
    if (!this.freshEndpoint && !this.identityEndpoint) {
      this.exhaustRetry("reconnect_unavailable");
      return;
    }
    this.ensureRetrySession();
    const token = this.retryToken;
    if (this.offline || this.fastPhaseSpent()) {
      this.setOffline(true);
      // A hidden page waits for its wake signal rather than probing blind.
      const visible = this.retryRuntime.visible?.() ?? true;
      const delay = delayOverride ?? Math.round(OFFLINE_RETRY_MS * this.jitter());
      this.sink?.reconnectStatus?.(Object.freeze({ state: "OFFLINE", attempt: this.retryAttempt, ...(visible ? { delayMs: delay } : {}) }));
      if (visible) this.armRetry(token, delay);
      return;
    }
    const remaining = this.remainingRetryMs();
    const exponential = Math.min(RECONNECT_MAX_DELAY_MS, RECONNECT_BASE_DELAY_MS * (2 ** this.retryAttempt));
    const delay = Math.min(remaining, delayOverride ?? Math.round(exponential * this.jitter()));
    this.sink?.reconnectStatus?.(Object.freeze({ state: "WAITING", attempt: this.retryAttempt + 1, delayMs: delay }));
    this.armRetry(token, delay);
  }

  // Resolves the next reconnect endpoint. The source binding is the fast path;
  // when it is absent (a reopened tab that never held one) or its re-mint fails
  // (an expired binding), the identity provider re-mints from the tab's session
  // identity. A ReconnectRefusal from the identity provider is a genuinely
  // unresolvable session and is rethrown so the caller exhausts terminally.
  private async resolveReconnectEndpoint(attempt: ReconnectAttempt): Promise<AttachmentEndpoint> {
    const source = this.preparedSource;
    if (source && this.freshEndpoint) {
      let fromSource: AttachmentEndpoint | undefined;
      try {
        fromSource = await this.freshEndpoint(attempt.controller.signal, source);
      } catch (error) {
        // A caller-aborted attempt, or a source-mint failure with no identity
        // fallback, stays an ordinary retryable error so the finite retry
        // budget is preserved exactly as before. Only when an identity provider
        // exists do we fall through and let it try.
        if (attempt.controller.signal.aborted || !this.identityEndpoint) throw error;
        fromSource = undefined;
      }
      if (fromSource && fromSource.url.length !== 0 && fromSource.protocols.length !== 0) return fromSource;
      // An unusable endpoint with no identity fallback is an ordinary failure.
      if (!this.identityEndpoint) throw new Error("fresh endpoint unavailable");
      // Otherwise fall through: the identity path can still recover.
    }
    if (!this.identityEndpoint) throw new ReconnectRefusal("source_binding_unavailable");
    const endpoint = await this.identityEndpoint(attempt.controller.signal);
    if (!endpoint || endpoint.url.length === 0 || endpoint.protocols.length === 0) throw new Error("identity endpoint unavailable");
    return endpoint;
  }

  private async attemptReconnect(token: number): Promise<void> {
    if (this.retryDisabled || token !== this.retryToken || (!this.freshEndpoint && !this.identityEndpoint)) return;
    if (!this.offline && this.fastPhaseSpent()) {
      this.scheduleReconnect();
      return;
    }
    const attempt: ReconnectAttempt = {
      token,
      attempt: ++this.retryAttempt,
      controller: new AbortController(),
    };
    this.activeAttempt = attempt;
    this.lastAttemptStartedAt = this.retryRuntime.now();
    attempt.deadlineTimer = this.retryRuntime.setTimeout(() => {
      attempt.deadlineTimer = undefined;
      this.expireAttempt(attempt);
    }, MAX_RECONNECT_ATTEMPT_MS);
    this.sink?.reconnectStatus?.(Object.freeze({ state: "ATTEMPTING", attempt: attempt.attempt }));
    try {
      const endpoint = await this.resolveReconnectEndpoint(attempt);
      if (!this.attemptIsCurrent(attempt)) return;
      this.url = endpoint.url;
      const protocols = withHistoryProtocol(endpoint.protocols, this.desiredHistoryRows);
      this.protocols = [...protocols];
      this.openSocket(endpoint.url, protocols, attempt);
    } catch (error) {
      if (!this.attemptIsCurrent(attempt)) return;
      const socket = attempt.socket;
      const generation = attempt.generation;
      this.finishAttempt(attempt, true);
      if (socket && this.socket === socket) {
        this.cancelLiveness(socket, generation);
        this.socket = undefined;
        if (generation !== undefined) this.notifyClosed(generation, "reconnect_attempt_failed");
        closeSocket(socket, CLOSE_CLIENT_FAULT, "reconnect_attempt_failed");
      }
      // A terminal identity outcome ends the retry with its own code; anything
      // else is ordinary infrastructure loss and stays within the budget.
      if (error instanceof ReconnectRefusal) {
        this.exhaustRetry(error.code);
        return;
      }
      this.scheduleReconnect();
    }
  }

  private attemptIsCurrent(attempt: ReconnectAttempt): boolean {
    return !this.retryDisabled && this.activeAttempt === attempt && attempt.token === this.retryToken && !attempt.controller.signal.aborted;
  }

  private expireAttempt(attempt: ReconnectAttempt): void {
    if (!this.attemptIsCurrent(attempt)) return;
    const socket = attempt.socket;
    const generation = attempt.generation;
    this.finishAttempt(attempt, true);
    if (socket && this.socket === socket) {
      this.cancelLiveness(socket, generation);
      this.socket = undefined;
      if (generation !== undefined) this.notifyClosed(generation, "reconnect_attempt_timeout");
      closeSocket(socket, CLOSE_CLIENT_FAULT, "reconnect_attempt_timeout");
    }
    this.scheduleReconnect();
  }
}
