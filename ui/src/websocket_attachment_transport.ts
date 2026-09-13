import type { AttachmentPagePort, FinalizeCause, FinalizeIntent, PortSendResult } from "./attachment_port";
import { MAX_ATTACHMENT_WIRE_BYTES, decodeServerFrame, encodeBrowserFrame } from "./attachment_wire";
import type { BrowserFrame, HistoryRequest } from "./attachment_protocol";
import { cloneBrowserFrame } from "./attachment_protocol";
import { HISTORY_CHOICES, type HistoryChoice } from "./dashboard";
import {
  TransportLiveness, browserTransportLivenessRuntime, decodeServerLivenessFrame,
  secureTransportLivenessNonce, type TransportLivenessNonceSource, type TransportLivenessRuntime,
} from "./transport_liveness";
import { UNIFIED_TAKEOVER_REASONS } from "./unified_close_policy";
import { decodeServerRefusalFrame } from "./unified_refusal_notice";

export type AttachmentTransportSink = Readonly<{
  openTransport(generation: number): void;
  receiveDecoded(generation: number, value: unknown): "ENQUEUED" | "STALE" | "CLOSED";
  transportClosed(generation: number, reason: string): void;
  reconnectStatus?(status: ReconnectStatus): void;
  // An in-band operational refusal: one request's outcome on a transport
  // that stays open. Nothing about the attachment changes.
  operationalRefusal?(generation: number, code: string): void;
}>;

export type AttachmentEndpoint = Readonly<{ url: string; protocols: readonly string[] }>;
export type EndpointReplacementReason = "takeover_requested" | "session_switch";
export type ReconnectStatus = Readonly<{ state: "DETACHED" | "WAITING" | "ATTEMPTING" | "CONNECTED" | "EXHAUSTED"; attempt: number; delayMs?: number; reason?: string }>;
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
}>;

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

const SOCKET_CONNECTING = 0;
const SOCKET_OPEN = 1;
export const MAX_RECONNECT_ATTEMPTS = 6;
export const MAX_RECONNECT_ELAPSED_MS = 30_000;
export const MAX_RECONNECT_ATTEMPT_MS = 5_000;
const RECONNECT_BASE_DELAY_MS = 250;
const RECONNECT_MAX_DELAY_MS = 4_000;
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
// The takeover policy set is the close policy's exported classification —
// never a private copy, so the transport and the page cannot drift apart.
const takeoverPolicyReasons = UNIFIED_TAKEOVER_REASONS;

export class WebSocketAttachmentTransport implements AttachmentPagePort {
  private sink?: AttachmentTransportSink;
  private socket?: AttachmentWebSocket;
  private generation = 0;
  private closedGeneration = 0;
  private retryTimer?: unknown;
  private retryAttempt = 0;
  private retryStartedAt?: number;
  private retryToken = 0;
  private retryDisabled = false;
  private activeAttempt?: ReconnectAttempt;
  private activeLiveness?: ActiveLiveness;
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
    private readonly retryRuntime: ReconnectRuntime = Object.freeze({
      now: () => Date.now(),
      setTimeout: (callback, delayMs) => globalThis.setTimeout(callback, delayMs),
      clearTimeout: (handle) => globalThis.clearTimeout(handle as ReturnType<typeof setTimeout>),
    }),
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
    const socket = this.socket;
    this.cancelLiveness(socket, intent.generation);
    this.socket = undefined;
    this.closedGeneration = Math.max(this.closedGeneration, intent.generation);
    if (!socket) return;
    const code = protocolFaults.has(intent.cause) ? 1002 : 1011;
    try { socket.close(code, intent.cause.slice(0, 64)); } catch { /* the page already owns the terminal fault */ }
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
    this.retryAttempt = 0;
    this.retryStartedAt = undefined;
    this.sink?.reconnectStatus?.(Object.freeze({ state: "CONNECTED", attempt: 0 }));
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
    if (superseded && this.socket === superseded) {
      this.cancelLiveness(superseded, supersededGeneration);
      this.socket = undefined;
      this.notifyClosed(supersededGeneration, "attach_again_superseded");
      try { superseded.close(1000, "attach_again_superseded"); } catch { /* manual supersession is best effort */ }
    }
    this.retryAttempt = 0;
    this.retryStartedAt = this.retryRuntime.now();
    this.scheduleReconnect(0);
  }

  takeControl(endpoint: AttachmentEndpoint): void {
    this.replaceEndpoint(endpoint, "takeover_requested");
  }

  // One endpoint-replacement primitive. Takeover keeps its landed behaviour;
  // a session switch additionally starts a fresh finite retry budget for the
  // newly committed identity before opening its socket.
  replaceEndpoint(endpoint: AttachmentEndpoint, reason: EndpointReplacementReason): void {
    if (!endpoint || endpoint.url.length === 0 || endpoint.protocols.length === 0) throw new Error("takeover endpoint is unavailable");
    this.retryDisabled = reason === "takeover_requested";
    this.cancelRetryWork();
    if (reason === "session_switch") {
      this.retryAttempt = 0;
      this.retryStartedAt = this.retryRuntime.now();
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
      try { prior.close(1000, reason); } catch { /* the explicit replacement already owns the next generation */ }
    }
    this.url = endpoint.url;
    this.protocols = [...endpoint.protocols];
    this.openSocket(endpoint.url, endpoint.protocols);
    this.protocols = undefined;
  }

  endpointReplacementBusy(): boolean {
    return this.retryTimer !== undefined || this.activeAttempt !== undefined;
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
    this.disconnect(reason);
    this.sink?.reconnectStatus?.(Object.freeze({ state: "DETACHED", attempt: this.retryAttempt, reason }));
  }

  destroy(): void {
    this.retryDisabled = true;
    this.clearPendingHistoryTimer();
    this.cancelRetryWork();
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
    try { socket.close(1000, reason.slice(0, 64)); } catch { /* notification and socket detachment already happened */ }
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
    heartbeat.start();
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
      const result = this.sink?.receiveDecoded(generation, frame);
      if (result === "CLOSED") this.closeCurrent(socket, generation, "page_closed", false);
    } catch {
      this.closeCurrent(socket, generation, "malformed_frame", false);
    }
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
      return;
    }
    this.ensureRetrySession();
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
    }
    try { socket.close(reason === "malformed_frame" || reason === "non_text_frame" || reason === "liveness_protocol" ? 1002 : 1011, reason.slice(0, 64)); } catch { /* notification already happened */ }
    if (retryable) {
      this.ensureRetrySession();
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
    if (this.retryStartedAt !== undefined) return;
    this.retryAttempt = 0;
    this.retryStartedAt = this.retryRuntime.now();
    this.retryToken++;
  }

  private cancelRetryWork(): void {
    this.retryToken++;
    if (this.retryTimer !== undefined) this.retryRuntime.clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
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
    if (this.retryStartedAt === undefined) return MAX_RECONNECT_ELAPSED_MS;
    return Math.max(0, MAX_RECONNECT_ELAPSED_MS - Math.max(0, this.retryRuntime.now() - this.retryStartedAt));
  }

  private exhaustRetry(reason = "retry_budget_exhausted"): void {
    const attempt = this.retryAttempt;
    this.retryDisabled = true;
    this.cancelRetryWork();
    this.sink?.reconnectStatus?.(Object.freeze({ state: "EXHAUSTED", attempt, reason }));
  }

  private scheduleReconnect(delayOverride?: number): void {
    if (this.retryDisabled || !this.freshEndpoint || this.retryTimer !== undefined || this.activeAttempt) return;
    this.ensureRetrySession();
    const remaining = this.remainingRetryMs();
    if (this.retryAttempt >= MAX_RECONNECT_ATTEMPTS || remaining <= 0) {
      this.exhaustRetry();
      return;
    }
    const exponential = Math.min(RECONNECT_MAX_DELAY_MS, RECONNECT_BASE_DELAY_MS * (2 ** this.retryAttempt));
    const jitter = 0.8 + Math.min(1, Math.max(0, this.random())) * 0.4;
    const delay = Math.min(remaining, delayOverride ?? Math.round(exponential * jitter));
    const token = this.retryToken;
    this.sink?.reconnectStatus?.(Object.freeze({ state: "WAITING", attempt: this.retryAttempt + 1, delayMs: delay }));
    this.retryTimer = this.retryRuntime.setTimeout(() => {
      this.retryTimer = undefined;
      void this.attemptReconnect(token);
    }, delay);
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
    const remaining = this.remainingRetryMs();
    if (remaining <= 0 || this.retryAttempt >= MAX_RECONNECT_ATTEMPTS) {
      this.exhaustRetry();
      return;
    }
    const attempt: ReconnectAttempt = {
      token,
      attempt: ++this.retryAttempt,
      controller: new AbortController(),
    };
    this.activeAttempt = attempt;
    const deadline = Math.min(MAX_RECONNECT_ATTEMPT_MS, remaining);
    attempt.deadlineTimer = this.retryRuntime.setTimeout(() => {
      attempt.deadlineTimer = undefined;
      this.expireAttempt(attempt);
    }, deadline);
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
        try { socket.close(1011, "reconnect_attempt_failed"); } catch { /* exact failed attempt is detached */ }
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
      try { socket.close(1011, "reconnect_attempt_timeout"); } catch { /* exact attempt is already detached */ }
    }
    this.scheduleReconnect();
  }
}
