import { validateComposerStorageScope } from "../src/composer_storage_scope";
// @ts-expect-error The unit bundle runs on Node; this project intentionally omits Node types from its browser tsconfig.
import { readFileSync } from "node:fs";
import {
  MAX_HISTORY_ROW_UTF8_BYTES, MAX_HISTORY_ROWS, MAX_HISTORY_UTF8_BYTES, MAX_INBOX_FRAMES,
  MAX_GEOMETRY_CELLS, MAX_INPUT_BYTES, MAX_LIVE_BYTES, MAX_REPLAY_BYTES, MAX_SOURCE_UTF8_BYTES, MAX_UINT64,
  cloneBrowserFrame, frameWeight, prepareFingerprint, validateDecodedAttachmentFrame,
  validateServerFrame, type BrowserFrame, type Prepare,
} from "../src/attachment_protocol";
import {
  MAX_ATTACHMENT_WIRE_BYTES, decodeBrowserFrame, decodeServerFrame, encodeBrowserFrame, encodeServerFrame,
} from "../src/attachment_wire";
import {
  CLOSE_CLIENT_FAULT, CLOSE_CLIENT_PROTOCOL_FAULT, CLOSE_ORDERLY,
  HISTORY_DEPTH_PENDING_TIMEOUT_MS, MAX_RECONNECT_ATTEMPTS, MAX_RECONNECT_ATTEMPT_MS, MAX_RECONNECT_ELAPSED_MS,
  OFFLINE_RETRY_MS, OFFLINE_WAKE_SPACING_MS, STABLE_CONNECTION_MS, WebSocketAttachmentTransport,
  type AttachmentWebSocket, type HistoryDepthOutcome, type ReconnectRuntime,
} from "../src/websocket_attachment_transport";
import {
  TRANSPORT_LIVENESS_PREFIX, decodeServerLivenessFrame, encodeBrowserLivenessPing,
  type TransportLivenessRuntime,
} from "../src/transport_liveness";
import { TRANSPORT_FLOW_PREFIX } from "../src/transport_flow";
import { parseCSRFCookie, refreshCSRFToken } from "../src/csrf_refresh";
import { classifyUnifiedClose } from "../src/unified_close_policy";
import { normalizeComposedText, serializeComposerSegments } from "../src/composer";

type Test = Readonly<{ name: string; run: () => void | Promise<void> }>;
const tests: Test[] = [];
const test = (name: string, run: Test["run"]) => tests.push(Object.freeze({ name, run }));
const assert: (condition: unknown, message?: string) => asserts condition = (condition, message = "assertion failed") => { if (!condition) throw new Error(message); };
const equal = (actual: unknown, expected: unknown, message = "values differ") => assert(Object.is(actual, expected), `${message}: ${String(actual)} !== ${String(expected)}`);
const throws = (fn: () => unknown, message = "expected throw") => { let threw = false; try { fn(); } catch { threw = true; } assert(threw, message); };
const readWorkspaceText = (relative: string) => readFileSync(relative, "utf8");
const TRANSPORT_TEST_SOURCE = "S".repeat(43);
// The longest fast-phase wait at jitter 1 (the 4 s cap).
const RECONNECT_TEST_MAX_FAST_DELAY_MS = 4_000;

test("composer_normalization_orders_line_endings_controls_and_trailing_newlines", () => {
  equal(normalizeComposedText("a\r\nb\rc\n\n"), "a\nb\nc");
  equal(normalizeComposedText(`left\tmiddle\u001b[201~right\u0085\n`), "left\tmiddle[201~right");
  equal(normalizeComposedText(" \u2014 \u4f60\u597d  "), " \u2014 \u4f60\u597d  ");
  equal(serializeComposerSegments([
    Object.freeze({ kind: "text" as const, value: "before " }),
    Object.freeze({ kind: "artifact" as const, id: "future", label: "future", resolve: () => "/staged/path" }),
  ]), "before /staged/path");
});

test("composer_storage_scope_validation_fails_closed_without_rewriting_identity", () => {
  equal(validateComposerStorageScope(null), undefined);
  equal(validateComposerStorageScope(""), undefined);
  equal(validateComposerStorageScope("bad\u0000scope"), undefined);
  equal(validateComposerStorageScope("x".repeat(4_097)), undefined);
  const exact = '["realm","server","session",1000]';
  equal(validateComposerStorageScope(exact), exact);
});
const bindTransportSource = (transport: WebSocketAttachmentTransport, generation = 0) => {
  // Retry mechanics are isolated from framing; PREPARE ownership is exercised through real frames below.
  (transport as unknown as { recordPreparedSource(value: number, source: string): void })
    .recordPreparedSource(generation, TRANSPORT_TEST_SOURCE);
};

// Mirrors WebSocket.close() in a browser: page script may pass only 1000 or an
// application code in 3000–4999. Anything else throws InvalidAccessError and
// leaves the socket exactly as it was.
const assertBrowserCloseCode = (code?: number): void => {
  if (code !== undefined && code !== 1000 && (code < 3000 || code > 4999)) {
    throw Object.assign(new Error(`InvalidAccessError: close code ${code}`), { name: "InvalidAccessError" });
  }
};

class TransportFakeSocket {
  bufferedAmount = 0;
  failSend = false;
  readonly sent: string[] = [];
  readonly closes: Array<Readonly<{ code?: number; reason?: string }>> = [];
  private readonly listeners = new Map<string, Array<(event: unknown) => void>>();
  constructor(public readyState = 0) {}
  addEventListener(type: string, listener: (event: unknown) => void): void {
    const values = this.listeners.get(type) ?? [];
    values.push(listener);
    this.listeners.set(type, values);
  }
  send(value: unknown): void {
    if (this.failSend) throw new Error("simulated send loss");
    this.sent.push(String(value));
  }
  close(code?: number, reason?: string): void {
    assertBrowserCloseCode(code);
    this.readyState = 3;
    this.closes.push(Object.freeze({ code, reason }));
  }
  emit(type: string, event: unknown): void { for (const listener of this.listeners.get(type) ?? []) listener(event); }
}

class FakeReconnectClock {
  private clock = 0;
  private next = 1;
  private readonly timers = new Map<number, { due: number; callback: () => void }>();
  private readonly wakeListeners = new Set<() => void>();
  visibleState = true;
  readonly runtime: ReconnectRuntime = Object.freeze({
    now: () => this.clock,
    setTimeout: (callback: () => void, delayMs: number) => {
      const id = this.next++;
      this.timers.set(id, { due: this.clock + delayMs, callback });
      return id;
    },
    clearTimeout: (handle: unknown) => { if (typeof handle === "number") this.timers.delete(handle); },
    subscribeWake: (callback: () => void) => {
      this.wakeListeners.add(callback);
      return () => { this.wakeListeners.delete(callback); };
    },
    visible: () => this.visibleState,
  });
  wake(): void { for (const listener of [...this.wakeListeners]) listener(); }
  // A device sleep: the clock jumps while pending timers pause, each keeping
  // the delay it had left.
  suspend(milliseconds: number): void {
    this.clock += milliseconds;
    for (const timer of this.timers.values()) timer.due += milliseconds;
  }
  wakeSubscribers(): number { return this.wakeListeners.size; }
  now(): number { return this.clock; }
  pending(): number { return this.timers.size; }
  async advance(milliseconds: number): Promise<void> {
    const target = this.clock + milliseconds;
    for (let steps = 0; steps < 100; steps += 1) {
      const next = [...this.timers.entries()].sort((left, right) => left[1].due - right[1].due || left[0] - right[0])[0];
      if (!next || next[1].due > target) break;
      this.clock = next[1].due;
      this.timers.delete(next[0]);
      next[1].callback();
      await this.flush();
    }
    this.clock = target;
    await this.flush();
  }
  async runUntilIdle(): Promise<void> {
    for (let steps = 0; this.timers.size > 0 && steps < 100; steps += 1) {
      const due = Math.min(...[...this.timers.values()].map((timer) => timer.due));
      await this.advance(Math.max(0, due - this.clock));
    }
    assert(this.timers.size === 0, "fake reconnect clock did not converge");
  }
  async flush(): Promise<void> { for (let index = 0; index < 8; index += 1) await Promise.resolve(); }
}

class FakeLivenessClock {
  private clock = 0;
  private wallClock = 0;
  private next = 1;
  private readonly timers = new Map<number, { due: number; callback: () => void }>();
  private readonly retired = new Map<number, () => void>();
  private readonly wakes = new Set<() => void>();
  readonly runtime: TransportLivenessRuntime = Object.freeze({
    now: () => this.clock,
    wallNow: () => this.wallClock,
    setTimeout: (callback: () => void, delayMs: number) => {
      const id = this.next++;
      this.timers.set(id, { due: this.clock + delayMs, callback });
      this.retired.set(id, callback);
      return id;
    },
    clearTimeout: (handle: unknown) => { if (typeof handle === "number") this.timers.delete(handle); },
    subscribeWake: (callback: () => void) => { this.wakes.add(callback); return () => { this.wakes.delete(callback); }; },
  });
  pending(): number { return this.timers.size; }
  nextTimerDelay(): number | undefined {
    if (this.timers.size === 0) return undefined;
    return Math.min(...[...this.timers.values()].map((timer) => timer.due)) - this.clock;
  }
  activeWakeSubscriptions(): number { return this.wakes.size; }
  activeHandles(): number[] { return [...this.timers.keys()]; }
  monotonicNow(): number { return this.clock; }
  wallNow(): number { return this.wallClock; }
  elapseWithoutTimers(milliseconds: number): void { this.clock += milliseconds; this.wallClock += milliseconds; }
  advanceWallOnly(milliseconds: number): void { this.wallClock += milliseconds; }
  rewindWall(milliseconds: number): void { this.wallClock -= milliseconds; }
  wake(): void { for (const callback of [...this.wakes]) callback(); }
  invokeRetired(handle: number): void { this.retired.get(handle)?.(); }
  advance(milliseconds: number): void {
    const target = this.clock + milliseconds;
    for (let steps = 0; steps < 100; steps += 1) {
      const next = [...this.timers.entries()].sort((left, right) => left[1].due - right[1].due || left[0] - right[0])[0];
      if (!next || next[1].due > target) break;
      this.wallClock += next[1].due - this.clock;
      this.clock = next[1].due;
      this.timers.delete(next[0]);
      next[1].callback();
    }
    this.wallClock += target - this.clock;
    this.clock = target;
  }
}

const livenessNonce = "0123456789abcdef0123456789abcdef";
const livenessPong = (nonce = livenessNonce) => `${TRANSPORT_LIVENESS_PREFIX}PONG ${nonce}`;
const inputFrame = (text: string): BrowserFrame => ({ type: "INPUT", version: 1, source: "source", epoch: 1n, data: new TextEncoder().encode(text) });

const prepare = (patch: Partial<Prepare> = {}): Prepare => ({
  type: "PREPARE", version: 1, source: "source", epoch: 1n, cut: 1n, kind: "INITIAL",
  columns: 80, rows: 24, history: ["one", "two"], truncated: false, replay: new Uint8Array([1, 2]),
  ...patch,
} as Prepare);
const ready = (cut = 1n): BrowserFrame => ({ type: "READY", version: 1, source: "source", epoch: 1n, cut });
const commit = (cut = 1n) => ({ type: "COMMIT" as const, version: 1 as const, source: "source", epoch: 1n, cut });
const live = (cut = 1n) => ({ type: "LIVE" as const, version: 1 as const, source: "source", epoch: 1n, cut, data: new Uint8Array([65]) });

test("attachment_wire_uses_decimal_uint64_and_canonical_padded_base64_in_both_directions", () => {
  const initial = prepare({ epoch: MAX_UINT64, cut: MAX_UINT64, replay: new Uint8Array([0, 1, 2, 255]) });
  const encodedInitial = encodeServerFrame(initial);
  assert(encodedInitial.includes(`"epoch":"${MAX_UINT64.toString(10)}"`), "epoch was not encoded as a decimal string");
  assert(encodedInitial.includes('"replay":"AAEC/w=="'), "replay was not encoded as padded standard base64");
  const decodedInitial = decodeServerFrame(encodedInitial);
  equal(decodedInitial.type, "PREPARE");
  assert(decodedInitial.type === "PREPARE");
  equal(decodedInitial.epoch, MAX_UINT64);
  equal(decodedInitial.cut, MAX_UINT64);
  equal(Array.from(decodedInitial.replay).join(","), "0,1,2,255");

  for (const frame of [
    commit(), live(),
    { type: "MODE", version: 1, source: "source", epoch: 1n, mode: "CONTROL", reason: "lease" },
    { type: "END", version: 1, source: "source", epoch: 1n, reason: "done" },
  ] as const) equal(decodeServerFrame(encodeServerFrame(frame)).type, frame.type);

  const input = { type: "INPUT", version: 1, source: "source", epoch: MAX_UINT64, data: new Uint8Array([0, 255]) } as const;
  const decodedInput = decodeBrowserFrame(encodeBrowserFrame(input));
  equal(decodedInput.type, "INPUT");
  assert(decodedInput.type === "INPUT");
  equal(decodedInput.epoch, MAX_UINT64);
  equal(Array.from(decodedInput.data).join(","), "0,255");
  for (const frame of [
    ready(),
    { type: "DEFER", version: 1, source: "source", epoch: 1n, cut: 1n, reason: "ACTIVE_CONTACT_OR_SCROLL_GESTURE" },
    { type: "MODE_REQUEST", version: 1, source: "source", epoch: 1n, mode: "CONTROL" },
    { type: "HISTORY_REQUEST", version: 1, source: "source", epoch: 1n, request: MAX_UINT64, historyRows: 1_000 },
  ] as const) equal(decodeBrowserFrame(encodeBrowserFrame(frame)).type, frame.type);

  const historyPrepare = prepare({ kind: "HISTORY", request: MAX_UINT64, effectiveHistoryRows: 1_000 });
  const encodedHistoryPrepare = encodeServerFrame(historyPrepare);
  assert(encodedHistoryPrepare.includes(`"request":"${MAX_UINT64.toString(10)}"`));
  assert(encodedHistoryPrepare.includes('"effective_history_rows":1000'));
  const decodedHistoryPrepare = decodeServerFrame(encodedHistoryPrepare);
  assert(decodedHistoryPrepare.type === "PREPARE" && decodedHistoryPrepare.kind === "HISTORY");
  equal(decodedHistoryPrepare.request, MAX_UINT64);
  equal(decodedHistoryPrepare.effectiveHistoryRows, 1_000);

  throws(() => decodeServerFrame('{"type":"COMMIT","version":1,"source":"source","epoch":1,"cut":"1"}'), "numeric uint64 was accepted");
  throws(() => decodeServerFrame('{"type":"LIVE","version":1,"source":"source","epoch":"1","cut":"1","data":"AAE"}'), "unpadded base64 was accepted");
  throws(() => decodeServerFrame('{"type":"COMMIT","version":1,"source":"source","epoch":"1","cut":"1","extra":true}'), "unknown field was accepted");
  throws(() => decodeServerFrame('{"type":"LIVE","version":1,"source":"source","epoch":"1","cut":"1","data":"QQ==","data":"Qg=="}'), "duplicate field was accepted");
  throws(() => decodeServerFrame(encodeBrowserFrame(ready())), "browser frame was accepted from the server");
});

test("transport_liveness_codec_is_exact_directional_and_reserved", () => {
  const ping = encodeBrowserLivenessPing(livenessNonce);
  equal(ping, `${TRANSPORT_LIVENESS_PREFIX}PING ${livenessNonce}`);
  equal(new TextEncoder().encode(ping).byteLength, 55);
  const pong = decodeServerLivenessFrame(livenessPong());
  equal(pong.type, "PONG");
  assert(pong.type === "PONG");
  equal(pong.nonce, livenessNonce);
  equal(decodeServerLivenessFrame('{"type":"COMMIT"}').type, "ORDINARY");
  for (const malformed of [
    `${TRANSPORT_LIVENESS_PREFIX}PING ${livenessNonce}`,
    `${TRANSPORT_LIVENESS_PREFIX}PONG ${livenessNonce.toUpperCase()}`,
    `${TRANSPORT_LIVENESS_PREFIX}PONG ${livenessNonce}x`,
    `${TRANSPORT_LIVENESS_PREFIX}PONG ${livenessNonce.slice(1)}`,
    `${TRANSPORT_LIVENESS_PREFIX}NOPE ${livenessNonce}`,
  ]) equal(decodeServerLivenessFrame(malformed).type, "VIOLATION", `reserved frame passed: ${malformed.length}`);
  throws(() => encodeBrowserLivenessPing(livenessNonce.toUpperCase()), "uppercase nonce encoded");
});

test("history_depth_desired_survives_one_class2_reconnect_then_falls_back_after_the_second", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const protocols: Array<readonly string[]> = [];
  const outcomes: HistoryDepthOutcome[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws",
    (_url, values) => {
      protocols.push(Object.freeze([...(values ?? [])]));
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    },
    ["persea-terminal.v2", "persea-history.5000"],
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-terminal.v2", "persea-history.5000"]) }),
    () => 0.5,
    clock.runtime,
  );
  transport.bind({
    openTransport: () => {},
    receiveDecoded: () => "ENQUEUED",
    transportClosed: () => {},
  });
  const unsubscribe = transport.onHistoryDepthOutcome((outcome) => outcomes.push(outcome));
  const generation = transport.connect();
  bindTransportSource(transport, generation);
  equal(transport.trySend(generation, {
    type: "HISTORY_REQUEST", version: 1, source: TRANSPORT_TEST_SOURCE, epoch: 1n, request: 1n, historyRows: 1_000,
  }), "ACCEPTED");
  sockets[0].emit("close", { code: 1011, reason: "cut_failed" });
  await clock.advance(250);
  assert(protocols[1]?.includes("persea-history.1000"), "first class-2 reconnect did not carry desired depth");
  assert(!protocols[1]?.includes("persea-history.5000"), "first class-2 reconnect retained stale effective depth");
  sockets[1].emit("close", { code: 1011, reason: "cut_failed_again" });
  await clock.advance(500);
  assert(protocols[2]?.includes("persea-history.5000"), "second class-2 reconnect did not fall back to committed effective depth");
  equal(outcomes.length, 1);
  equal(outcomes[0]?.status, "failed");
  assert(outcomes[0]?.status === "failed");
  equal(outcomes[0]?.cause, "cut_failure");
  equal(outcomes[0]?.requested, 1_000);
  equal(outcomes[0]?.desired, 5_000);
  equal(outcomes[0]?.effective, 5_000);
  assert(sockets.slice(1).every((socket) => socket.sent.every((payload) => !payload.includes("HISTORY_REQUEST"))), "fallback generated an automatic history cut");
  transport.destroy();
  unsubscribe();
});

test("history_depth_defer_is_pending_not_class2_and_times_out_visibly", async () => {
  const clock = new FakeReconnectClock();
  const socket = new TransportFakeSocket(1);
  const outcomes: HistoryDepthOutcome[] = [];
  const disposedOutcomes: HistoryDepthOutcome[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws",
    () => socket as unknown as AttachmentWebSocket,
    ["persea-terminal.v2", "persea-history.5000"],
    undefined,
    () => 0.5,
    clock.runtime,
  );
  transport.bind({
    openTransport: () => {},
    receiveDecoded: () => "ENQUEUED",
    transportClosed: () => {},
  });
  const unsubscribe = transport.onHistoryDepthOutcome((outcome) => outcomes.push(outcome));
  const unsubscribeDisposed = transport.onHistoryDepthOutcome((outcome) => disposedOutcomes.push(outcome));
  const generation = transport.connect();
  equal(transport.trySend(generation, {
    type: "HISTORY_REQUEST", version: 1, source: "source", epoch: 1n, request: 9n, historyRows: 1_000,
  }), "ACCEPTED");
  socket.emit("message", { data: encodeServerFrame(prepare({
    kind: "HISTORY", cut: 9n, request: 9n, effectiveHistoryRows: 1_000, replay: new Uint8Array(),
  })) });
  equal(transport.trySend(generation, {
    type: "DEFER", version: 1, source: "source", epoch: 1n, cut: 9n, reason: "VISIBLE_ANCHOR_WOULD_BE_EVICTED",
  }), "ACCEPTED");
  equal(outcomes[0]?.status, "pending_reader");
  unsubscribeDisposed();
  await clock.advance(HISTORY_DEPTH_PENDING_TIMEOUT_MS - 1);
  equal(outcomes.length, 1, "pending-reader depth failed before its named timeout");
  await clock.advance(1);
  equal(outcomes[1]?.status, "failed");
  equal(disposedOutcomes.length, 1, "unsubscribed depth listener received a later outcome");
  assert(outcomes[1]?.status === "failed");
  equal(outcomes[1]?.cause, "reader_hold");
  equal(outcomes[1]?.requested, 1_000);
  equal(outcomes[1]?.desired, 5_000);
  equal(outcomes[1]?.effective, 5_000);
  equal(socket.closes.length, 0, "reader DEFER detached the live transport");
  transport.destroy();
  unsubscribe();
});

test("history_depth_defer_does_not_spend_the_ambiguous_cut_retry", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const protocols: Array<readonly string[]> = [];
  const outcomes: HistoryDepthOutcome[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws",
    (_url, values) => {
      protocols.push(Object.freeze([...(values ?? [])]));
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    },
    ["persea-terminal.v2", "persea-history.5000"],
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-terminal.v2", "persea-history.5000"]) }),
    () => 0.5,
    clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  const unsubscribe = transport.onHistoryDepthOutcome((outcome) => outcomes.push(outcome));
  const generation = transport.connect();
  bindTransportSource(transport, generation);
  equal(transport.trySend(generation, {
    type: "HISTORY_REQUEST", version: 1, source: TRANSPORT_TEST_SOURCE, epoch: 1n, request: 1n, historyRows: 1_000,
  }), "ACCEPTED");
  sockets[0].emit("message", { data: encodeServerFrame(prepare({
    source: TRANSPORT_TEST_SOURCE, kind: "HISTORY", cut: 11n, request: 1n, effectiveHistoryRows: 1_000, replay: new Uint8Array(),
  })) });
  equal(transport.trySend(generation, {
    type: "DEFER", version: 1, source: TRANSPORT_TEST_SOURCE, epoch: 1n, cut: 11n, reason: "INTERSECTING_DOM_SELECTION",
  }), "ACCEPTED");
  sockets[0].emit("close", { code: 1011, reason: "cut_failed_after_reader_defer" });
  await clock.advance(250);
  assert(protocols[1]?.includes("persea-history.1000"), "one DEFER consumed the first ambiguous-cut retry");
  assert(outcomes.every((outcome) => outcome.status !== "failed"), "one DEFER plus one ambiguous cut fell back early");
  transport.destroy();
  unsubscribe();
});

test("history_depth_terminal_retirement_clears_the_reader_hold_timer", async () => {
  for (const retirement of ["finalize", "disconnect"] as const) {
    const clock = new FakeReconnectClock();
    const socket = new TransportFakeSocket(1);
    const outcomes: HistoryDepthOutcome[] = [];
    const transport = new WebSocketAttachmentTransport(
      "ws://example.test/ws",
      () => socket as unknown as AttachmentWebSocket,
      ["persea-terminal.v2", "persea-history.5000"],
      undefined,
      () => 0.5,
      clock.runtime,
    );
    transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
    const unsubscribe = transport.onHistoryDepthOutcome((outcome) => outcomes.push(outcome));
    const generation = transport.connect();
    equal(transport.trySend(generation, {
      type: "HISTORY_REQUEST", version: 1, source: "source", epoch: 1n, request: 1n, historyRows: 1_000,
    }), "ACCEPTED");
    socket.emit("message", { data: encodeServerFrame(prepare({
      kind: "HISTORY", cut: 11n, request: 1n, effectiveHistoryRows: 1_000, replay: new Uint8Array(),
    })) });
    equal(transport.trySend(generation, {
      type: "DEFER", version: 1, source: "source", epoch: 1n, cut: 11n, reason: "INTERSECTING_DOM_SELECTION",
    }), "ACCEPTED");
    equal(outcomes.length, 1, `${retirement} fixture did not enter pending-reader state`);
    if (retirement === "finalize") {
      transport.finalize(Object.freeze({ generation, cause: "OUT_OF_STATE" }));
    } else {
      transport.disconnect("test_retirement");
    }
    await clock.advance(HISTORY_DEPTH_PENDING_TIMEOUT_MS);
    equal(outcomes.length, 1, `${retirement} allowed a retired reader-hold timer to emit`);
    unsubscribe();
    transport.destroy();
  }
});

test("same_depth_reader_retry_replaces_the_old_timeout_generation", async () => {
  const clock = new FakeReconnectClock();
  const socket = new TransportFakeSocket(1);
  const outcomes: HistoryDepthOutcome[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws",
    () => socket as unknown as AttachmentWebSocket,
    ["persea-terminal.v2", "persea-history.5000"],
    undefined,
    () => 0.5,
    clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  const unsubscribe = transport.onHistoryDepthOutcome((outcome) => outcomes.push(outcome));
  const generation = transport.connect();
  const request = (id: bigint) => ({
    type: "HISTORY_REQUEST" as const, version: 1 as const, source: "source", epoch: 1n, request: id, historyRows: 1_000 as const,
  });
  equal(transport.trySend(generation, request(1n)), "ACCEPTED");
  socket.emit("message", { data: encodeServerFrame(prepare({
    kind: "HISTORY", cut: 11n, request: 1n, effectiveHistoryRows: 1_000, replay: new Uint8Array(),
  })) });
  equal(transport.trySend(generation, {
    type: "DEFER", version: 1, source: "source", epoch: 1n, cut: 11n, reason: "INTERSECTING_DOM_SELECTION",
  }), "ACCEPTED");
  await clock.advance(HISTORY_DEPTH_PENDING_TIMEOUT_MS - 1);
  equal(transport.retryHistoryDepth(generation, request(2n)), "ACCEPTED");
  await clock.advance(1);
  equal(outcomes.filter((outcome) => outcome.status === "failed").length, 0, "retired same-depth timeout won the retry race");
  socket.emit("message", { data: encodeServerFrame(prepare({
    kind: "HISTORY", cut: 12n, request: 2n, effectiveHistoryRows: 1_000, replay: new Uint8Array(),
  })) });
  equal(transport.trySend(generation, {
    type: "DEFER", version: 1, source: "source", epoch: 1n, cut: 12n, reason: "INTERSECTING_DOM_SELECTION",
  }), "ACCEPTED");
  await clock.advance(HISTORY_DEPTH_PENDING_TIMEOUT_MS);
  equal(outcomes.filter((outcome) => outcome.status === "failed").length, 1, "replacement reader hold did not converge visibly");
  unsubscribe();
  transport.destroy();
});

test("missing_liveness_pong_closes_once_cancels_timers_and_requests_a_fresh_endpoint", async () => {
  const reconnect = new FakeReconnectClock();
  const liveness = new FakeLivenessClock();
  const sockets: TransportFakeSocket[] = [];
  const closed: string[] = [];
  let fresh = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => { const socket = new TransportFakeSocket(1); sockets.push(socket); return socket as unknown as AttachmentWebSocket; },
    ["persea-handle.consumed"], async () => { fresh += 1; return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze([`persea-handle.fresh-${fresh}`]) }); },
    () => 0.5, reconnect.runtime, liveness.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  const generation = transport.connect();
  bindTransportSource(transport, generation);
  sockets[0].emit("open", {});
  equal(liveness.activeWakeSubscriptions(), 1, "active generation did not own exactly one wake subscription");
  equal(sockets[0].sent[0], `${TRANSPORT_LIVENESS_PREFIX}PING ${livenessNonce}`, "open did not send one immediate challenge");
  equal(liveness.pending(), 1);
  liveness.advance(10_000);
  equal(closed.join(","), "liveness_timeout");
  equal(sockets[0].closes.length, 1, "deadline did not close exactly once");
  equal(liveness.pending(), 0, "retired generation retained heartbeat work");
  equal(liveness.activeWakeSubscriptions(), 0, "timeout retained its wake subscription");
  sockets[0].emit("error", {});
  sockets[0].emit("close", { code: 1006, reason: "late" });
  equal(closed.length, 1, "retired socket duplicated liveness loss");
  await reconnect.advance(250);
  equal(fresh, 1, "liveness loss did not request a fresh endpoint");
  equal(sockets.length, 2, "liveness loss did not create one replacement generation");
  sockets[1].emit("open", {});
  equal(liveness.activeWakeSubscriptions(), 1, "replacement did not own exactly one wake subscription");
  transport.detach();
  equal(liveness.activeWakeSubscriptions(), 0, "Detach retained the replacement wake subscription");
});

test("wrong_malformed_and_duplicated_liveness_pongs_fail_closed", () => {
  for (const response of [
    livenessPong("fedcba9876543210fedcba9876543210"),
    `${TRANSPORT_LIVENESS_PREFIX}PONG ${livenessNonce}x`,
    `${TRANSPORT_LIVENESS_PREFIX}PING ${livenessNonce}`,
  ]) {
    const clock = new FakeLivenessClock();
    const socket = new TransportFakeSocket(1);
    const closed: string[] = [];
    const transport = new WebSocketAttachmentTransport(
      "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
      clock.runtime, () => livenessNonce,
    );
    transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
    transport.connect(); socket.emit("open", {});
    equal(clock.activeWakeSubscriptions(), 1, "protocol fixture did not start with one wake subscription");
    socket.emit("message", { data: response });
    equal(closed.join(","), "liveness_protocol", "invalid server liveness was not retryable protocol loss");
    equal(socket.closes[0]?.code, CLOSE_CLIENT_PROTOCOL_FAULT);
    equal(clock.activeWakeSubscriptions(), 0, "protocol failure retained its wake subscription");
  }

  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  transport.connect(); socket.emit("open", {});
  equal(clock.activeWakeSubscriptions(), 1, "duplicate-Pong fixture did not start with one wake subscription");
  socket.emit("message", { data: livenessPong() });
  equal(closed.length, 0);
  equal(clock.activeWakeSubscriptions(), 1, "valid proof removed the active wake subscription");
  socket.emit("message", { data: livenessPong() });
  equal(closed.join(","), "liveness_protocol", "duplicated Pong re-established proof");
  equal(clock.activeWakeSubscriptions(), 0, "duplicated-Pong failure retained its wake subscription");
});

test("ordinary_attachment_traffic_never_satisfies_the_liveness_challenge", () => {
  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const received: unknown[] = [];
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: (_generation, value) => { received.push(value); return "ENQUEUED"; }, transportClosed: (_generation, reason) => { closed.push(reason); } });
  transport.connect(); socket.emit("open", {});
  socket.emit("message", { data: encodeServerFrame(prepare()) });
  equal(received.length, 1);
  clock.advance(10_000);
  equal(closed.join(","), "liveness_timeout");
});

test("non_open_or_throwing_heartbeat_send_is_retryable_and_owned_once", () => {
  for (const failure of ["NON_OPEN", "THROW"] as const) {
    const clock = new FakeLivenessClock();
    const socket = new TransportFakeSocket(failure === "NON_OPEN" ? 0 : 1);
    socket.failSend = failure === "THROW";
    const closed: string[] = [];
    const transport = new WebSocketAttachmentTransport(
      "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
      clock.runtime, () => livenessNonce,
    );
    transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
    transport.connect(); socket.emit("open", {});
    equal(closed.join(","), failure === "NON_OPEN" ? "liveness_send_unavailable" : "liveness_send_failed");
    equal(socket.closes.length, 1, `${failure} heartbeat failure did not close exactly once`);
    equal(clock.pending(), 0, `${failure} heartbeat failure retained timer work`);
    socket.emit("error", {}); socket.emit("close", { code: 1006, reason: "late" });
    equal(closed.length, 1, `${failure} late events duplicated closure`);
  }
});

test("input_after_absolute_liveness_expiry_is_synchronously_sealed_before_encoding_or_send", () => {
  const reconnect = new FakeReconnectClock();
  const liveness = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined,
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }),
    () => 0.5, reconnect.runtime, liveness.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  const generation = transport.connect(); socket.emit("open", {});
  equal(liveness.activeWakeSubscriptions(), 1, "pre-input fixture did not start with one wake subscription");
  socket.emit("message", { data: livenessPong() });
  liveness.elapseWithoutTimers(20_000);
  equal(transport.trySend(generation, inputFrame("MUST-NOT-LEAK")), "TRANSPORT_LOST");
  equal(closed.join(","), "liveness_timeout");
  equal(socket.sent.filter((value) => value.includes('"type":"INPUT"')).length, 0, "expired INPUT bytes reached WebSocket.send");
  equal(liveness.pending(), 0, "pre-input expiry retained obsolete timers");
  equal(liveness.activeWakeSubscriptions(), 0, "pre-input expiry retained its wake subscription");
  equal(reconnect.pending(), 1, "pre-input expiry did not arm fresh reconnect");
  transport.detach();
});

test("wake_after_timer_throttling_evaluates_absolute_expiry", () => {
  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  transport.connect(); socket.emit("open", {}); socket.emit("message", { data: livenessPong() });
  clock.elapseWithoutTimers(20_000);
  clock.wake();
  equal(closed.join(","), "liveness_timeout");
  clock.wake();
  equal(closed.length, 1, "repeated wake duplicated closure");
});

test("system_sleep_wall_expiry_wake_closes_while_monotonic_and_timers_are_paused", () => {
  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  transport.connect(); socket.emit("open", {}); socket.emit("message", { data: livenessPong() });
  equal(clock.monotonicNow(), 0);
  equal(clock.wallNow(), 0);
  equal(clock.pending(), 1, "system-sleep fixture lacked its paused monotonic timer");
  clock.advanceWallOnly(20_000);
  equal(clock.monotonicNow(), 0, "system-sleep fixture advanced monotonic time");
  equal(clock.pending(), 1, "system-sleep fixture delivered a paused timer");
  clock.wake();
  equal(closed.join(","), "liveness_timeout", "wake ignored wall-clock proof expiry");
  equal(socket.sent.filter((value) => value.includes('"type":"INPUT"')).length, 0, "wake expiry sent INPUT");
  equal(clock.pending(), 0, "wake expiry retained its paused timer");
  equal(clock.activeWakeSubscriptions(), 0, "wake expiry retained its wake subscription");
});

test("system_sleep_wall_expiry_pre_input_closes_before_zero_byte_send", () => {
  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  const generation = transport.connect(); socket.emit("open", {}); socket.emit("message", { data: livenessPong() });
  clock.advanceWallOnly(20_000);
  equal(clock.monotonicNow(), 0, "pre-input sleep fixture advanced monotonic time");
  equal(transport.trySend(generation, inputFrame("SLEEP-MUST-NOT-LEAK")), "TRANSPORT_LOST");
  equal(closed.join(","), "liveness_timeout", "pre-input path ignored wall-clock proof expiry");
  equal(socket.sent.filter((value) => value.includes('"type":"INPUT"')).length, 0, "wall-expired INPUT reached WebSocket.send");
  equal(clock.pending(), 0, "wall-expired INPUT retained paused timer work");
  equal(clock.activeWakeSubscriptions(), 0, "wall-expired INPUT retained wake work");
});

test("early_wall_only_wake_reconciles_to_five_second_proof_remainder", () => {
  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  transport.connect(); socket.emit("open", {}); socket.emit("message", { data: livenessPong() });
  const pausedChallengeTimer = clock.activeHandles()[0];
  clock.advanceWallOnly(15_000);
  equal(clock.monotonicNow(), 0, "early-wake fixture advanced paused monotonic time");
  clock.wake();
  equal(closed.length, 0, "early wake closed before paired proof expiry");
  equal(clock.pending(), 1, "early wake lost active one-shot ownership");
  equal(clock.nextTimerDelay(), 5_000, "early wake did not re-arm the exact wall remainder");
  equal(socket.sent.filter((value) => value.startsWith(`${TRANSPORT_LIVENESS_PREFIX}PING `)).length, 2, "wall-due challenge was not sent exactly once");
  clock.invokeRetired(pausedChallengeTimer);
  equal(closed.length, 0, "queued pre-wake timer callback affected reconciled ownership");
  equal(clock.nextTimerDelay(), 5_000, "queued pre-wake timer callback displaced the reconciled timer");
  clock.advance(4_999);
  equal(closed.length, 0, "reconciled timer closed one millisecond early");
  equal(clock.nextTimerDelay(), 1, "reconciled timer lost its final millisecond");
  clock.advance(1);
  equal(closed.join(","), "liveness_timeout", "reconciled timer did not autonomously close at proof age twenty seconds");
  equal(clock.pending(), 0, "autonomous close retained timer work");
  equal(clock.activeWakeSubscriptions(), 0, "autonomous close retained wake work");
});

test("early_wall_only_pre_input_reconciles_without_input_replay", () => {
  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  const generation = transport.connect(); socket.emit("open", {}); socket.emit("message", { data: livenessPong() });
  clock.advanceWallOnly(15_000);
  equal(transport.trySend(generation, inputFrame("EARLY-ONLY-ONCE")), "ACCEPTED", "non-expired early INPUT was sealed");
  equal(closed.length, 0, "early pre-input observation closed before proof expiry");
  equal(clock.pending(), 1, "early pre-input observation lost active one-shot ownership");
  equal(clock.nextTimerDelay(), 5_000, "early pre-input observation did not re-arm the exact wall remainder");
  equal(socket.sent.filter((value) => value.includes('"type":"INPUT"')).length, 1, "early INPUT was dropped or duplicated");
  clock.advance(4_999);
  equal(closed.length, 0, "pre-input reconciled timer closed one millisecond early");
  clock.advance(1);
  equal(closed.join(","), "liveness_timeout", "pre-input reconciled timer did not close at proof age twenty seconds");
  equal(socket.sent.filter((value) => value.includes('"type":"INPUT"')).length, 1, "accepted INPUT replayed during autonomous close");
  equal(transport.trySend(generation, inputFrame("MUST-NOT-REPLAY")), "CLOSED");
  equal(socket.sent.filter((value) => value.includes('"type":"INPUT"')).length, 1, "post-close INPUT reached the socket");
});

test("paired_observation_never_extends_the_existing_challenge_schedule", () => {
  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  transport.connect(); socket.emit("open", {}); socket.emit("message", { data: livenessPong() });
  clock.advance(4_000);
  equal(clock.nextTimerDelay(), 6_000, "proof challenge did not retain its original due time");
  clock.wake();
  equal(clock.nextTimerDelay(), 6_000, "non-expired observation extended or replaced an earlier challenge timer");
  clock.advance(5_999);
  equal(socket.sent.filter((value) => value.startsWith(`${TRANSPORT_LIVENESS_PREFIX}PING `)).length, 1, "challenge ran early");
  clock.advance(1);
  equal(socket.sent.filter((value) => value.startsWith(`${TRANSPORT_LIVENESS_PREFIX}PING `)).length, 2, "challenge schedule was extended");
  equal(closed.length, 0, "on-time challenge closed the generation");
  transport.detach();
});

test("backwards_wall_clock_anomaly_fails_closed_before_input", () => {
  const clock = new FakeLivenessClock();
  const socket = new TransportFakeSocket(1);
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined, undefined, undefined, undefined,
    clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  const generation = transport.connect(); socket.emit("open", {}); socket.emit("message", { data: livenessPong() });
  clock.elapseWithoutTimers(100);
  clock.wake();
  equal(closed.length, 0, "forward clock observation closed a live generation");
  clock.rewindWall(1);
  equal(transport.trySend(generation, inputFrame("CLOCK-MUST-NOT-LEAK")), "TRANSPORT_LOST");
  equal(closed.join(","), "liveness_clock_anomaly");
  equal(socket.sent.filter((value) => value.includes('"type":"INPUT"')).length, 0, "backwards-wall INPUT reached WebSocket.send");
  equal(clock.activeWakeSubscriptions(), 0, "clock anomaly retained wake work");
});

test("retired_generation_timer_and_pong_cannot_close_or_validate_its_replacement", async () => {
  const reconnect = new FakeReconnectClock();
  const liveness = new FakeLivenessClock();
  const sockets: TransportFakeSocket[] = [];
  const closed: string[] = [];
  let nonceIndex = 0;
  const nonces = [livenessNonce, "11111111111111111111111111111111", "22222222222222222222222222222222"];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => { const socket = new TransportFakeSocket(1); sockets.push(socket); return socket as unknown as AttachmentWebSocket; }, undefined,
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }),
    () => 0.5, reconnect.runtime, liveness.runtime, () => nonces[nonceIndex++] ?? nonces.at(-1)!,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  const generation = transport.connect(); bindTransportSource(transport, generation); sockets[0].emit("open", {});
  equal(liveness.activeWakeSubscriptions(), 1, "original generation did not own exactly one wake subscription");
  sockets[0].emit("message", { data: livenessPong(nonces[0]) });
  const retiredTimer = liveness.activeHandles()[0];
  sockets[0].emit("error", {});
  equal(liveness.activeWakeSubscriptions(), 0, "superseded generation retained its wake subscription");
  await reconnect.advance(250);
  equal(sockets.length, 2);
  sockets[1].emit("open", {}); sockets[1].emit("message", { data: livenessPong(nonces[1]) });
  equal(liveness.activeWakeSubscriptions(), 1, "old and replacement wake subscriptions accumulated");
  const closedBeforeStaleWork = closed.length;
  liveness.invokeRetired(retiredTimer);
  sockets[0].emit("message", { data: livenessPong(nonces[0]) });
  equal(closed.length, closedBeforeStaleWork, "retired generation affected replacement");
  equal(liveness.activeWakeSubscriptions(), 1, "retired work changed replacement wake ownership");
  equal(transport.trySend(2, inputFrame("CURRENT")), "ACCEPTED", "replacement lost current proof");
  equal(sockets[1].sent.filter((value) => value.includes('"type":"INPUT"')).length, 1);
  transport.detach();
  equal(liveness.activeWakeSubscriptions(), 0, "final replacement retirement retained wake ownership");
});

test("wake_subscription_ownership_is_exact_across_supersession_detach_and_finalization", () => {
  const clock = new FakeLivenessClock();
  const sockets: TransportFakeSocket[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => { const socket = new TransportFakeSocket(1); sockets.push(socket); return socket as unknown as AttachmentWebSocket; },
    undefined, undefined, undefined, undefined, clock.runtime, () => livenessNonce,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });

  transport.connect(); sockets[0].emit("open", {});
  equal(clock.activeWakeSubscriptions(), 1, "active original generation did not own one wake subscription");
  transport.disconnect("superseded");
  equal(clock.activeWakeSubscriptions(), 0, "explicit supersession retained the old wake subscription");

  const replacement = transport.connect(); sockets[1].emit("open", {});
  equal(clock.activeWakeSubscriptions(), 1, "replacement accumulated beside the superseded wake subscription");
  transport.detach("detached");
  equal(clock.activeWakeSubscriptions(), 0, "Detach retained an active wake subscription");

  const finalGeneration = transport.connect(); sockets[2].emit("open", {});
  equal(clock.activeWakeSubscriptions(), 1, "final generation did not own one wake subscription");
  transport.finalize(Object.freeze({ generation: finalGeneration, cause: "RESOURCE_FAILURE" }));
  equal(clock.activeWakeSubscriptions(), 0, "finalization retained an active wake subscription");
  equal(replacement, 2);
});

test("websocket_attachment_transport_is_generation_bound_bounded_and_never_replays", () => {
  class FakeSocket {
    readyState = 0;
    bufferedAmount = 0;
    readonly sent: string[] = [];
    readonly closes: Array<Readonly<{ code?: number; reason?: string }>> = [];
    private readonly listeners = new Map<string, Array<(event: unknown) => void>>();
    addEventListener(type: string, listener: (event: unknown) => void): void {
      const values = this.listeners.get(type) ?? [];
      values.push(listener);
      this.listeners.set(type, values);
    }
    send(value: unknown): void { assert(typeof value === "string", "transport sent a non-text frame"); this.sent.push(value); }
    close(code?: number, reason?: string): void { assertBrowserCloseCode(code); this.readyState = 3; this.closes.push(Object.freeze({ code, reason })); }
    emit(type: string, event: unknown): void { for (const listener of this.listeners.get(type) ?? []) listener(event); }
  }

  const sockets: FakeSocket[] = [];
  const opened: number[] = [];
  const received: Array<Readonly<{ generation: number; value: unknown }>> = [];
  const closed: Array<Readonly<{ generation: number; reason: string }>> = [];
  const transport = new WebSocketAttachmentTransport("ws://example.test/ws", () => {
    const socket = new FakeSocket();
    sockets.push(socket);
    return socket as unknown as AttachmentWebSocket;
  });
  transport.bind({
    openTransport: (generation) => { opened.push(generation); },
    receiveDecoded: (generation, value) => { received.push(Object.freeze({ generation, value })); return "ENQUEUED"; },
    transportClosed: (generation, reason) => { closed.push(Object.freeze({ generation, reason })); },
  });

  const generation1 = transport.connect();
  equal(generation1, 1);
  equal(opened.join(","), "1");
  const first = sockets[0];
  first.readyState = 1;
  equal(transport.trySend(generation1, ready()), "ACCEPTED");
  equal(first.sent.length, 1);
  equal(decodeBrowserFrame(first.sent[0]).type, "READY");
  first.emit("message", { data: encodeServerFrame(prepare()) });
  equal(received.length, 1);
  equal((received[0].value as { type?: unknown }).type, "PREPARE");
  first.bufferedAmount = MAX_ATTACHMENT_WIRE_BYTES;
  equal(transport.trySend(generation1, ready()), "SATURATED");
  equal(first.sent.length, 1, "saturated frame was queued or replayed");
  first.bufferedAmount = 0;
  first.emit("message", { data: "not-json" });
  equal(closed.length, 1);
  equal(first.closes[0].code, CLOSE_CLIENT_PROTOCOL_FAULT);

  const generation2 = transport.connect();
  equal(generation2, 2);
  equal(opened.join(","), "1,2");
  const second = sockets[1];
  second.readyState = 1;
  equal(transport.trySend(generation1, ready()), "CLOSED", "stale generation was accepted");
  equal(transport.trySend(generation2, ready()), "ACCEPTED");
  equal(second.sent.length, 1, "reconnect replayed an old frame");
  transport.disconnect("page_hidden");
  equal(closed.length, 2);
  equal(closed[1].generation, generation2);
  equal(closed[1].reason, "page_hidden");
  const generation3 = transport.connect();
  equal(generation3, 3);
  const third = sockets[2];
  third.readyState = 1;
  second.emit("close", { code: 1000, reason: "late_old_close" });
  equal(closed.length, 2, "stale close crossed transport generations");
  equal(transport.trySend(generation3, ready()), "ACCEPTED");
  equal(third.sent.length, 1, "explicit reconnect replayed an old frame");
  third.emit("close", { code: 1000, reason: "done" });
  equal(closed.length, 3);
});

test("reconnect_spends_the_fast_phase_then_keeps_probing_offline_until_detached", async () => {
  const clock = new FakeReconnectClock();
  const statuses: string[] = [];
  let attempts = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => { throw new Error("socket must not be constructed when inventory is offline"); }, ["persea-handle.consumed"],
    async () => { attempts += 1; throw new Error("inventory offline"); }, () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    reconnectStatus: (status) => { statuses.push(`${status.state}:${status.attempt}`); },
  });
  bindTransportSource(transport);
  transport.attachAgain();
  // Fast phase at jitter 1: 0, 500, 1500, 3500, 7500, 11500 ms.
  await clock.advance(11_500);
  equal(attempts, MAX_RECONNECT_ATTEMPTS, "fast-phase attempt cap drifted");
  equal(statuses.at(-1), `OFFLINE:${MAX_RECONNECT_ATTEMPTS}`, "a spent fast phase did not go OFFLINE visibly");
  assert(!statuses.some((status) => status.startsWith("EXHAUSTED")), "ordinary loss was reported as unrecoverable");
  equal(clock.wakeSubscribers(), 1, "OFFLINE did not listen for the network or the page coming back");
  await clock.advance(OFFLINE_RETRY_MS - 1);
  equal(attempts, MAX_RECONNECT_ATTEMPTS, "an OFFLINE probe ran before its slow interval");
  await clock.advance(1);
  equal(attempts, MAX_RECONNECT_ATTEMPTS + 1, "OFFLINE stopped probing after the fast phase");
  await clock.advance(OFFLINE_RETRY_MS);
  equal(attempts, MAX_RECONNECT_ATTEMPTS + 2, "OFFLINE probing did not continue");
  transport.detach();
  equal(clock.pending(), 0, "Detach left a retry timer armed");
  equal(clock.wakeSubscribers(), 0, "Detach left the wake subscription");
  await clock.advance(OFFLINE_RETRY_MS * 4);
  equal(attempts, MAX_RECONNECT_ATTEMPTS + 2, "a detached transport kept probing");
});

test("offline_wake_probes_at_once_and_a_hidden_page_waits_for_it", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const statuses: string[] = [];
  let online = false;
  let attempts = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => {
      attempts += 1;
      if (!online) throw new Error("inventory offline");
      return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze([`persea-handle.fresh-${attempts}`]) });
    }, () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    reconnectStatus: (status) => { statuses.push(`${status.state}:${status.delayMs === undefined ? "-" : status.delayMs}`); },
  });
  bindTransportSource(transport);
  transport.attachAgain();
  await clock.advance(11_500);
  equal(statuses.at(-1), `OFFLINE:${OFFLINE_RETRY_MS}`, "fast phase did not settle into visible OFFLINE probing");
  clock.visibleState = false;
  await clock.advance(OFFLINE_RETRY_MS);
  equal(attempts, MAX_RECONNECT_ATTEMPTS + 1, "the armed probe did not run");
  equal(statuses.at(-1), "OFFLINE:-", "a hidden page armed a blind probe");
  equal(clock.pending(), 0, "a hidden page kept a probe timer");
  await clock.advance(OFFLINE_RETRY_MS * 4);
  equal(attempts, MAX_RECONNECT_ATTEMPTS + 1, "a hidden page probed without a wake signal");
  online = true;
  clock.visibleState = true;
  clock.wake();
  clock.wake();
  await clock.advance(0);
  equal(attempts, MAX_RECONNECT_ATTEMPTS + 2, "wake signals did not collapse into exactly one immediate probe");
  equal(sockets.length, 1, "the wake probe did not open one socket");
  transport.connectionCommitted(1);
  equal(statuses.at(-1), "CONNECTED:-");
  equal(clock.wakeSubscribers(), 0, "COMMIT kept the OFFLINE wake subscription");
  equal(clock.pending(), 0, "COMMIT left OFFLINE work armed");
  clock.wake();
  await clock.advance(0);
  equal(sockets.length, 1, "a wake signal on a live connection opened another socket");
  transport.detach();
});

test("offline_wake_bursts_are_spaced_from_the_previous_attempt", async () => {
  const clock = new FakeReconnectClock();
  let attempts = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => { throw new Error("socket must not be constructed while inventory is offline"); }, ["persea-handle.consumed"],
    async () => { attempts += 1; throw new Error("inventory offline"); }, () => 0.5, clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  bindTransportSource(transport);
  transport.attachAgain();
  await clock.advance(11_500);
  equal(attempts, MAX_RECONNECT_ATTEMPTS, "fast phase did not end in OFFLINE");
  // A burst right after the last attempt collapses into one probe, spaced.
  for (let signal = 0; signal < 20; signal += 1) clock.wake();
  await clock.advance(OFFLINE_WAKE_SPACING_MS - 1);
  equal(attempts, MAX_RECONNECT_ATTEMPTS, "a wake burst probed closer than the spacing to the previous attempt");
  await clock.advance(1);
  equal(attempts, MAX_RECONNECT_ATTEMPTS + 1, "a wake burst did not bring the probe forward exactly once");
  // A sustained storm, one signal every 100 ms for 10 s, stays spaced.
  const before = attempts;
  const window = 10_000;
  for (let elapsed = 0; elapsed < window; elapsed += 100) {
    clock.wake();
    await clock.advance(100);
  }
  assert(attempts - before <= Math.ceil(window / OFFLINE_WAKE_SPACING_MS) + 1, `a wake storm produced ${attempts - before} probes in ${window} ms`);
  assert(attempts - before >= Math.floor(window / OFFLINE_WAKE_SPACING_MS), "a wake storm stopped bringing probes forward");
  transport.detach();
  equal(clock.pending(), 0);
});

test("commit_then_immediate_loss_keeps_its_episode_and_settles_into_offline_probing", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const statuses: string[] = [];
  const opened: number[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }),
    () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    reconnectStatus: (status) => { statuses.push(status.state); },
  });
  const first = transport.connect();
  bindTransportSource(transport, first);
  transport.connectionCommitted(first);
  // Each successor commits and is evicted at once, the shape of a view that
  // cannot keep up. COMMIT must not refill the fast phase.
  sockets[0].emit("close", { code: 1000, reason: "subscriber_lagged" });
  for (let round = 0; round < MAX_RECONNECT_ATTEMPTS * 3 && !statuses.includes("OFFLINE"); round += 1) {
    const before = sockets.length;
    await clock.advance(RECONNECT_TEST_MAX_FAST_DELAY_MS);
    if (sockets.length === before) continue;
    opened.push(clock.now());
    const generation = sockets.length;
    transport.connectionCommitted(generation);
    sockets.at(-1)!.emit("close", { code: 1000, reason: "subscriber_lagged" });
  }
  assert(statuses.includes("OFFLINE"), "a commit-then-evict loop never left the fast phase");
  equal(opened.length, MAX_RECONNECT_ATTEMPTS, "the loop ran more fast attempts than one episode allows");
  const socketsAtOffline = sockets.length;
  await clock.advance(OFFLINE_RETRY_MS - 1);
  equal(sockets.length, socketsAtOffline, "OFFLINE loop reattached faster than the slow interval");
  transport.detach();
});

test("a_connection_that_stays_up_starts_a_fresh_episode_on_its_next_loss", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const statuses: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }),
    () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    reconnectStatus: (status) => { statuses.push(`${status.state}:${status.attempt}:${status.delayMs ?? "-"}`); },
  });
  const first = transport.connect();
  bindTransportSource(transport, first);
  transport.connectionCommitted(first);
  for (let loss = 0; loss < MAX_RECONNECT_ATTEMPTS * 2; loss += 1) {
    await clock.advance(STABLE_CONNECTION_MS);
    sockets.at(-1)!.emit("close", { code: 1006, reason: "" });
    equal(statuses.at(-1), "WAITING:1:250", `stable connection ${loss} did not start a fresh episode`);
    await clock.advance(250);
    transport.connectionCommitted(sockets.length);
  }
  transport.detach();
});

test("connected_time_between_momentary_commits_does_not_spend_the_fast_phase", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const statuses: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }),
    () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    reconnectStatus: (status) => { statuses.push(`${status.state}:${status.attempt}`); },
  });
  const first = transport.connect();
  bindTransportSource(transport, first);
  transport.connectionCommitted(first);
  // Every connection stays up just short of stable, so all losses share one
  // episode and together outlast its elapsed budget in wall time. Only the
  // disconnected gaps may spend that budget; the attempts still carry over.
  const up = STABLE_CONNECTION_MS - 1;
  assert((MAX_RECONNECT_ATTEMPTS - 1) * up > MAX_RECONNECT_ELAPSED_MS, "the probe does not outlast the elapsed budget");
  for (let loss = 1; loss <= MAX_RECONNECT_ATTEMPTS; loss += 1) {
    await clock.advance(up);
    const before = sockets.length;
    sockets.at(-1)!.emit("close", { code: 1006, reason: "" });
    equal(statuses.at(-1), `WAITING:${loss}`, `loss ${loss} left the fast phase early`);
    await clock.advance(RECONNECT_TEST_MAX_FAST_DELAY_MS);
    equal(sockets.length, before + 1, `loss ${loss} did not reconnect in the fast phase`);
    transport.connectionCommitted(sockets.length);
  }
  await clock.advance(up);
  sockets.at(-1)!.emit("close", { code: 1006, reason: "" });
  equal(statuses.at(-1), `OFFLINE:${MAX_RECONNECT_ATTEMPTS}`, "momentary commits refilled the fast phase's attempts");
  transport.detach();
});

const recordingTransport = (clock: FakeReconnectClock, sockets: TransportFakeSocket[], statuses: string[], mint: () => Promise<Readonly<{ url: string; protocols: readonly string[] }>>) => {
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"], mint, () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    reconnectStatus: (status) => { statuses.push(`${status.state}:${status.attempt}:${status.delayMs ?? "-"}`); },
  });
  return transport;
};
const freshEndpoint = async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) });
const takeoverEndpoint = Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.takeover"]) });

test("handoff_closes_start_a_fresh_episode", async () => {
  for (const reason of ["generation_refit", "generation_rotated"]) {
    const clock = new FakeReconnectClock();
    const sockets: TransportFakeSocket[] = [];
    const statuses: string[] = [];
    const transport = recordingTransport(clock, sockets, statuses, freshEndpoint);
    const first = transport.connect();
    bindTransportSource(transport, first);
    transport.connectionCommitted(first);
    // A shaky link spends the fast phase with momentary commits.
    for (let loss = 0; loss < MAX_RECONNECT_ATTEMPTS; loss += 1) {
      sockets.at(-1)!.emit("close", { code: 1006, reason: "" });
      await clock.advance(RECONNECT_TEST_MAX_FAST_DELAY_MS);
      transport.connectionCommitted(sockets.length);
    }
    // A deliberate handoff moments later is not another loss.
    await clock.advance(1_000);
    const before = sockets.length;
    sockets.at(-1)!.emit("close", { code: 1000, reason });
    equal(statuses.at(-1), "WAITING:1:250", `${reason} inherited the shaky episode`);
    await clock.advance(250);
    equal(sockets.length, before + 1, `${reason} did not re-attach at the base delay`);
    transport.detach();
  }
});

// A transport whose sink holds each frame's consumption until the test
// releases it, as a page does until its terminal has written the frame.
const flowTransport = (sockets: TransportFakeSocket[], held: Array<() => void> | undefined) => {
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-terminal.v2"], undefined, () => 0.5, new FakeReconnectClock().runtime,
    new FakeLivenessClock().runtime, () => livenessNonce,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    ...(held ? { afterConsumed: (_generation: number, done: () => void) => { held.push(done); } } : {}),
  });
  return transport;
};
const flowAcks = (socket: TransportFakeSocket) => socket.sent.filter((value) => value.startsWith(TRANSPORT_FLOW_PREFIX));
const settleFlow = () => Promise.resolve();

test("flow_acknowledges_a_contiguous_prefix_only_after_consumption", async () => {
  const sockets: TransportFakeSocket[] = [];
  const held: Array<() => void> = [];
  const transport = flowTransport(sockets, held);
  transport.connect();
  const socket = sockets[0]!;
  socket.emit("open", {});
  socket.emit("message", { data: encodeServerFrame(prepare()) });
  socket.emit("message", { data: `PERSEA-REFUSAL/1 resize_failed` });
  socket.emit("message", { data: encodeServerFrame(commit()) });
  socket.emit("message", { data: encodeServerFrame(live()) });
  await settleFlow();
  equal(held.length, 3, "a refusal frame was counted as an attachment frame");
  equal(flowAcks(socket).length, 0, "delivery alone was acknowledged");
  held[1]!();
  await settleFlow();
  equal(flowAcks(socket).length, 0, "an acknowledgement skipped an unconsumed frame");
  held[0]!();
  await settleFlow();
  held[2]!();
  await settleFlow();
  socket.emit("message", { data: encodeServerFrame(live()) });
  socket.emit("message", { data: encodeServerFrame(live()) });
  held[3]!();
  held[4]!();
  await settleFlow();
  equal(flowAcks(socket).join("|"), `${TRANSPORT_FLOW_PREFIX}ACK 2|${TRANSPORT_FLOW_PREFIX}ACK 3|${TRANSPORT_FLOW_PREFIX}ACK 5`,
    "acknowledgements were not cumulative, contiguous and coalesced");
  transport.destroy();
});

test("flow_acknowledgements_stay_on_their_socket", async () => {
  const sockets: TransportFakeSocket[] = [];
  const held: Array<() => void> = [];
  const transport = flowTransport(sockets, held);
  transport.connect();
  sockets[0]!.emit("open", {});
  sockets[0]!.emit("message", { data: encodeServerFrame(prepare()) });
  equal(held.length, 1, "the first socket's frame was not delivered");
  sockets[0]!.emit("close", { code: 1006, reason: "" });
  transport.connect();
  const second = sockets.at(-1)!;
  assert(second !== sockets[0], "no replacement socket");
  second.emit("open", {});
  second.emit("message", { data: encodeServerFrame(prepare()) });
  held[0]!();
  await settleFlow();
  equal(flowAcks(sockets[0]!).length + flowAcks(second).length, 0, "a superseded socket's frame was acknowledged");
  held[1]!();
  await settleFlow();
  equal(flowAcks(second).join("|"), `${TRANSPORT_FLOW_PREFIX}ACK 1`, "the replacement socket did not count from one");
  transport.destroy();
});

test("flow_without_a_consumption_point_acknowledges_on_delivery", async () => {
  const sockets: TransportFakeSocket[] = [];
  const transport = flowTransport(sockets, undefined);
  transport.connect();
  sockets[0]!.emit("open", {});
  sockets[0]!.emit("message", { data: encodeServerFrame(prepare()) });
  sockets[0]!.emit("message", { data: encodeServerFrame(commit()) });
  await settleFlow();
  equal(flowAcks(sockets[0]!).join("|"), `${TRANSPORT_FLOW_PREFIX}ACK 2`);
  transport.destroy();
});

test("an_offline_probe_timer_does_not_block_a_session_switch", async () => {
  const clock = new FakeReconnectClock();
  const transport = recordingTransport(clock, [], [], async () => { throw new Error("inventory offline"); });
  bindTransportSource(transport);
  transport.attachAgain();
  equal(transport.endpointReplacementBusy(), true, "a pending fast-phase retry did not count as busy");
  await clock.advance(11_500);
  equal(clock.pending(), 1, "OFFLINE armed no probe");
  equal(transport.endpointReplacementBusy(), false, "an OFFLINE probe timer blocked a session switch");
  transport.detach();
});

test("waiting_for_the_operator_does_not_spend_the_episode", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const statuses: string[] = [];
  const transport = recordingTransport(clock, sockets, statuses, freshEndpoint);
  const first = transport.connect();
  bindTransportSource(transport, first);
  transport.connectionCommitted(first);
  sockets[0].emit("close", { code: 1006, reason: "" });
  await clock.advance(250);
  // The reconnect finds the lease held and the page waits for the operator.
  sockets.at(-1)!.emit("close", { code: 1011, reason: "lease_held" });
  await clock.advance(10 * 60_000);
  transport.takeControl(takeoverEndpoint);
  transport.connectionCommitted(sockets.length);
  await clock.advance(5_000);
  sockets.at(-1)!.emit("close", { code: 1006, reason: "" });
  equal(statuses.at(-1)?.split(":").slice(0, 2).join(":"), "WAITING:2", "the wait for the operator spent the episode's elapsed budget");
  transport.detach();
});

test("a_takeover_after_an_offline_probe_leaves_offline", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const statuses: string[] = [];
  let online = false;
  let mints = 0;
  const transport = recordingTransport(clock, sockets, statuses, async () => {
    mints += 1;
    if (!online) throw new Error("inventory offline");
    return freshEndpoint();
  });
  bindTransportSource(transport);
  transport.attachAgain();
  await clock.advance(11_500);
  assert(statuses.at(-1)?.startsWith("OFFLINE:"), "the fast phase did not reach OFFLINE");
  online = true;
  await clock.advance(OFFLINE_RETRY_MS);
  equal(sockets.length, 1, "the OFFLINE probe did not open a socket");
  sockets[0].emit("close", { code: 1011, reason: "lease_held" });
  transport.takeControl(takeoverEndpoint);
  const mintsBefore = mints;
  const statusesBefore = statuses.length;
  clock.wake();
  await clock.advance(OFFLINE_WAKE_SPACING_MS);
  equal(mints, mintsBefore, "a wake over a live takeover minted a probe endpoint");
  assert(!statuses.slice(statusesBefore).some((status) => status.startsWith("OFFLINE:")), "a wake showed OFFLINE over a working takeover");
  equal(clock.wakeSubscribers(), 0, "the takeover kept the OFFLINE wake subscription");
  transport.detach();
});

test("waking_after_a_device_sleep_replaces_an_overdue_probe", async () => {
  const clock = new FakeReconnectClock();
  let attempts = 0;
  const transport = recordingTransport(clock, [], [], async () => { attempts += 1; throw new Error("inventory offline"); });
  bindTransportSource(transport);
  transport.attachAgain();
  await clock.advance(11_500);
  equal(attempts, MAX_RECONNECT_ATTEMPTS, "fast-phase attempt cap drifted");
  await clock.advance(5_000);
  // Ten minutes asleep: the clock moves on while the probe timer is paused.
  clock.suspend(10 * 60_000);
  clock.wake();
  await clock.advance(0);
  equal(attempts, MAX_RECONNECT_ATTEMPTS + 1, "waking after a sleep waited for the paused probe");
  transport.detach();
});

test("takeover_replacement_recovers_from_a_later_loss_and_from_loss_before_commit", async () => {
  for (const commitFirst of [true, false]) {
    const clock = new FakeReconnectClock();
    const sockets: TransportFakeSocket[] = [];
    const statuses: string[] = [];
    let mints = 0;
    const transport = new WebSocketAttachmentTransport(
      "ws://example.test/ws", () => {
        const socket = new TransportFakeSocket(1);
        sockets.push(socket);
        return socket as unknown as AttachmentWebSocket;
      }, ["persea-handle.consumed"],
      async () => { mints += 1; return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze([`persea-handle.fresh-${mints}`]) }); },
      () => 0.5, clock.runtime,
    );
    transport.bind({
      openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
      reconnectStatus: (status) => { statuses.push(status.state); },
    });
    const first = transport.connect();
    bindTransportSource(transport, first);
    sockets[0].emit("close", { code: 1000, reason: "lease_held" });
    equal(clock.pending(), 0, "a held lease did not stop automatic retries for the page's claim");
    transport.takeControl(Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.takeover"]) }));
    equal(sockets.length, 2, "takeover did not open its replacement");
    if (commitFirst) transport.connectionCommitted(2);
    sockets[1].emit("close", { code: 1006, reason: "" });
    equal(clock.pending(), 1, `loss ${commitFirst ? "after" : "before"} the takeover COMMIT left no retry armed`);
    await clock.advance(250);
    equal(mints, 1, "takeover loss did not mint a fresh endpoint");
    equal(sockets.length, 3, "takeover loss did not reattach");
    transport.connectionCommitted(3);
    sockets[2].emit("close", { code: 1000, reason: "control_displaced" });
    equal(clock.pending(), 0, "control moving elsewhere still auto-retried");
    transport.detach();
  }
});

test("server_proof_expiry_is_recoverable_loss", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }),
    () => 0.5, clock.runtime,
  );
  // The page's rule for a close: a terminal class stops recovery.
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED",
    transportClosed: (_generation, reason) => { if (classifyUnifiedClose(reason) === "terminal") transport.detach(reason); },
  });
  const first = transport.connect();
  bindTransportSource(transport, first);
  transport.connectionCommitted(first);
  sockets[0].emit("close", { code: 1008, reason: "browser_liveness" });
  await clock.advance(250);
  equal(sockets.length, 2, "front-door proof expiry did not reattach with a fresh endpoint");
  transport.detach();
});

test("page_fault_finalize_tells_the_page_and_leaves_manual_reconnect", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }),
    () => 0.5, clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: (_generation, reason) => { closed.push(reason); } });
  const generation = transport.connect();
  bindTransportSource(transport, generation);
  transport.finalize(Object.freeze({ generation, cause: "ACTIVE_TUPLE_MISMATCH" }));
  equal(closed.join(","), "attachment_fault", "a page fault left the page without a close to render");
  equal(sockets[0].readyState, 3, "a page fault left its socket open");
  transport.finalize(Object.freeze({ generation, cause: "ACTIVE_TUPLE_MISMATCH" }));
  equal(closed.length, 1, "a repeated finalize notified twice");
  await clock.advance(MAX_RECONNECT_ELAPSED_MS);
  equal(sockets.length, 1, "a page fault retried automatically");
  transport.attachAgain();
  await clock.advance(0);
  equal(sockets.length, 2, "Reconnect after a page fault did not reattach");
  transport.detach();
});

test("unexpected_close_reconnects_with_only_the_fresh_inventory_capability", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const endpoints: Array<Readonly<{ url: string; protocols?: readonly string[] }>> = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", (url, protocols) => {
      endpoints.push(Object.freeze({ url, protocols }));
      const socket = new TransportFakeSocket();
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }),
    () => 0.5, clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  const generation = transport.connect();
  bindTransportSource(transport, generation);
  sockets[0].emit("close", { code: 1006, reason: "offline" });
  equal(clock.pending(), 1, "unexpected close did not arm reconnect");
  await clock.advance(250);
  equal(endpoints.length, 2, "fresh endpoint did not create exactly one replacement socket");
  equal(endpoints[0].protocols?.join(","), "persea-handle.consumed");
  equal(endpoints[1].protocols?.join(","), "persea-handle.fresh");
  assert(!endpoints[1].protocols?.includes("persea-handle.consumed"), "consumed capability was reused during reconnect");
  transport.connectionCommitted(2);
  equal(clock.pending(), 0, "successful COMMIT left a reconnect timer armed");
  transport.detach();
});

test("generation_rotated_closes_once_and_remints_one_source_bound_successor", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const acquiredSources: string[] = [];
  const endpoints: string[][] = [];
  const closes: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", (_url, protocols) => {
      endpoints.push([...(protocols ?? [])]);
      const socket = new TransportFakeSocket();
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.predecessor"],
    async (_signal, source) => {
      acquiredSources.push(source);
      return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.successor"]) });
    },
    () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED",
    transportClosed: (_generation, reason) => { closes.push(reason); },
  });
  transport.connect();
  sockets[0].emit("message", { data: encodeServerFrame(prepare({ source: TRANSPORT_TEST_SOURCE })) });
  const close = { code: 1000, reason: "generation_rotated" };
  sockets[0].emit("close", close);
  sockets[0].emit("close", close);
  equal(closes.join(","), "generation_rotated", "one rotation emitted more than one typed close");
  equal(clock.pending(), 1, "rotation close did not arm exactly one reattachment cycle");
  await clock.advance(250);
  equal(acquiredSources.join(","), TRANSPORT_TEST_SOURCE, "rotation remint drifted from the predecessor source binding");
  equal(sockets.length, 2, "one rotation created more than one successor socket");
  equal(endpoints[0].join(","), "persea-handle.predecessor");
  equal(endpoints[1].join(","), "persea-handle.successor");
  transport.connectionCommitted(2);
  equal(clock.pending(), 0, "successor COMMIT left a duplicate reattachment armed");
  transport.detach();
});

test("generation_refit_closes_once_and_remints_one_source_bound_successor", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const acquiredSources: string[] = [];
  const endpoints: string[][] = [];
  const closes: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", (_url, protocols) => {
      endpoints.push([...(protocols ?? [])]);
      const socket = new TransportFakeSocket();
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.predecessor"],
    async (_signal, source) => {
      acquiredSources.push(source);
      return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.refit-successor"]) });
    },
    () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED",
    transportClosed: (_generation, reason) => { closes.push(reason); },
  });
  transport.connect();
  sockets[0].emit("message", { data: encodeServerFrame(prepare({ source: TRANSPORT_TEST_SOURCE })) });
  const close = { code: 1000, reason: "generation_refit" };
  sockets[0].emit("close", close);
  sockets[0].emit("close", close);
  equal(closes.join(","), "generation_refit", "one refit emitted more than one typed close");
  equal(clock.pending(), 1, "refit close did not arm exactly one reattachment cycle");
  await clock.advance(250);
  equal(acquiredSources.join(","), TRANSPORT_TEST_SOURCE, "refit remint drifted from the predecessor source binding");
  equal(sockets.length, 2, "one refit created more than one successor socket");
  equal(endpoints[0].join(","), "persea-handle.predecessor");
  equal(endpoints[1].join(","), "persea-handle.refit-successor");
  transport.connectionCommitted(2);
  equal(clock.pending(), 0, "refit successor COMMIT left a duplicate reattachment armed");
  transport.detach();
});

test("reconnect_source_is_captured_from_prepare_and_is_immutable_within_each_generation", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const acquiredSources: string[] = [];
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket();
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async (_signal, source) => {
      acquiredSources.push(source);
      return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) });
    },
    () => 0.5, clock.runtime,
  );
  let delivered = 0;
  transport.bind({
    openTransport: () => {},
    receiveDecoded: () => { delivered += 1; return "ENQUEUED"; },
    transportClosed: (_generation, reason) => { closed.push(reason); },
  });
  transport.connect();
  sockets[0].emit("message", { data: encodeServerFrame(prepare({ source: TRANSPORT_TEST_SOURCE })) });
  equal(delivered, 1, "valid PREPARE was not delivered after transport source capture");
  sockets[0].emit("close", { code: 1006, reason: "offline" });
  await clock.advance(250);
  equal(acquiredSources.join(","), TRANSPORT_TEST_SOURCE, "fresh endpoint did not receive the exact broker PREPARE source");
  equal(sockets.length, 2, "source-bound reconnect did not create one replacement socket");

  sockets[1].emit("message", { data: encodeServerFrame(prepare({ source: "T".repeat(43), kind: "RECONNECT", epoch: 2n })) });
  equal(delivered, 2, "fresh generation rejected a legitimate source rotation");
  equal(closed.length, 1, "fresh-generation source rotation added a transport closure");

  sockets[1].emit("message", { data: encodeServerFrame(prepare({ source: "U".repeat(43), kind: "RECONNECT", epoch: 2n })) });
  equal(delivered, 2, "same-generation changed source reached the page");
  equal(closed.at(-1), "malformed_frame", "changed reconnect source did not fail closed");
  equal(sockets[1].closes[0]?.code, CLOSE_CLIENT_PROTOCOL_FAULT, "changed reconnect source was not closed as a protocol fault");
  transport.detach();
});

test("reconnect_without_a_broker_prepare_source_never_calls_the_handle_provider", async () => {
  const clock = new FakeReconnectClock();
  const socket = new TransportFakeSocket();
  let acquisitions = 0;
  const statuses: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, ["persea-handle.consumed"],
    async () => {
      acquisitions += 1;
      return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.unreachable"]) });
    },
    () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    reconnectStatus: (value) => { statuses.push(`${value.state}:${value.reason ?? ""}`); },
  });
  transport.connect();
  socket.emit("close", { code: 1006, reason: "offline" });
  await clock.advance(250);
  equal(acquisitions, 0, "fresh handle provider ran without a broker PREPARE source");
  equal(statuses.at(-1), "EXHAUSTED:source_binding_unavailable", "missing PREPARE source was not reported fail-closed");
});

test("csrf_refresh_bootstraps_an_expired_terminal_page_with_one_safe_same_origin_get", async () => {
  const controller = new AbortController();
  const token = "C".repeat(43);
  let cookie = "";
  let calls = 0;
  const refreshed = await refreshCSRFToken(controller.signal, async (input, init) => {
    calls += 1;
    equal(input, "/api/inventory");
    equal(init.cache, "no-store");
    equal(init.credentials, "same-origin");
    assert(init.signal === controller.signal, "CSRF refresh lost cancellation ownership");
    equal(init.method, undefined, "CSRF bootstrap was not a safe GET");
    assert(init.body === undefined && init.headers === undefined, "CSRF bootstrap carried mutation material");
    cookie = `fixture=1; __Host-persea-terminal-csrf=${token}`;
    return Object.freeze({ ok: true });
  }, () => cookie);
  equal(refreshed, token);
  equal(calls, 1, "CSRF bootstrap issued duplicate safe requests");
  equal(parseCSRFCookie(`__Host-persea-terminal-csrf=${token}`), token);

  let rejected = false;
  try {
    await refreshCSRFToken(controller.signal, async () => Object.freeze({ ok: false }), () => cookie);
  } catch {
    rejected = true;
  }
  assert(rejected, "failed CSRF bootstrap continued to a mutation");
});

test("current_socket_send_throw_is_owned_once_and_late_events_cannot_duplicate_retry", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const endpoints: Array<Readonly<{ url: string; protocols?: readonly string[] }>> = [];
  const closed: Array<Readonly<{ generation: number; reason: string }>> = [];
  let freshHandles = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", (url, protocols) => {
      endpoints.push(Object.freeze({ url, protocols }));
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => {
      freshHandles += 1;
      return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze([`persea-handle.fresh-${freshHandles}`]) });
    }, () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED",
    transportClosed: (generation, reason) => { closed.push(Object.freeze({ generation, reason })); },
  });
  const generation = transport.connect();
  bindTransportSource(transport, generation);
  transport.connectionCommitted(generation);
  sockets[0].failSend = true;

  equal(transport.trySend(generation, ready()), "TRANSPORT_LOST", "send throw was mistaken for a local closed-port fault");
  equal(closed.length, 1, "send throw did not notify exactly once");
  equal(closed[0].reason, "transport_send_failed");
  equal(sockets[0].closes.length, 1, "send throw did not retire the exact socket once");
  equal(sockets[0].closes[0].code, CLOSE_CLIENT_FAULT, "send throw was mislabeled orderly");
  equal(clock.pending(), 1, "send throw did not arm bounded reconnect");

  sockets[0].emit("error", {});
  sockets[0].emit("close", { code: 1006, reason: "late_close" });
  equal(closed.length, 1, "late retired-socket event duplicated the page transition");
  equal(clock.pending(), 1, "late retired-socket event duplicated retry work");
  await clock.advance(250);
  equal(freshHandles, 1, "send throw did not request exactly one fresh inventory handle");
  equal(endpoints.length, 2, "send throw created the wrong replacement socket count");
  equal(endpoints[1].protocols?.join(","), "persea-handle.fresh-1");
  assert(!endpoints[1].protocols?.includes("persea-handle.consumed"), "send-loss recovery replayed the original handle");
  transport.connectionCommitted(2);
  transport.detach();
});

test("encoding_and_stale_generation_send_failures_remain_non_retryable", async () => {
  const clock = new FakeReconnectClock();
  const socket = new TransportFakeSocket(1);
  let freshHandles = 0;
  const closed: string[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, undefined,
    async () => { freshHandles += 1; return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }); },
    () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED",
    transportClosed: (_generation, reason) => { closed.push(reason); },
  });
  const generation = transport.connect();
  const invalid = { type: "READY", version: 1, source: "source", epoch: 1n, cut: -1n } as BrowserFrame;
  equal(transport.trySend(generation, invalid), "CLOSED", "encoding failure was mistaken for transport loss");
  equal(transport.trySend(generation - 1, ready()), "CLOSED", "stale generation was mistaken for transport loss");
  equal(closed.length, 0, "non-transport send failure retired the current socket");
  equal(socket.closes.length, 0, "non-transport send failure closed the current socket");
  equal(clock.pending(), 0, "non-transport send failure armed reconnect");
  await clock.advance(MAX_RECONNECT_ELAPSED_MS);
  equal(freshHandles, 0, "non-transport send failure requested a fresh handle");
  transport.detach();
});

test("fault_close_is_abnormal_retry_disabled_and_manual_attach_again_only", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  let endpoints = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => { endpoints += 1; return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze([`persea-handle.fresh-${endpoints}`]) }); },
    () => 0.5, clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  const generation = transport.connect();
  bindTransportSource(transport, generation);
  transport.finalize(Object.freeze({ generation, cause: "OUT_OF_STATE" }));
  equal(sockets[0].closes.length, 1);
  equal(sockets[0].closes[0].code, CLOSE_CLIENT_PROTOCOL_FAULT, "OUT_OF_STATE was mislabeled orderly");
  sockets[0].emit("close", { code: CLOSE_CLIENT_PROTOCOL_FAULT, reason: "OUT_OF_STATE" });
  await clock.advance(MAX_RECONNECT_ELAPSED_MS);
  equal(endpoints, 0, "fault closure retried without explicit Attach Again");
  equal(sockets.length, 1, "fault closure created an automatic replacement socket");
  transport.attachAgain();
  await clock.advance(0);
  equal(endpoints, 1, "manual Attach Again did not request one fresh capability");
  equal(sockets.length, 2, "manual Attach Again did not create one replacement socket");
  transport.connectionCommitted(2);
  equal(clock.pending(), 0);
  transport.detach();

  const internalSocket = new TransportFakeSocket(1);
  const internal = new WebSocketAttachmentTransport("ws://example.test/ws", () => internalSocket as unknown as AttachmentWebSocket);
  internal.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  internal.finalize(Object.freeze({ generation: internal.connect(), cause: "RESOURCE_FAILURE" }));
  equal(internalSocket.closes[0].code, CLOSE_CLIENT_FAULT, "resource fault did not use an abnormal client-fault close");

  const lossClock = new FakeReconnectClock();
  const lossSocket = new TransportFakeSocket(1);
  let lossEndpoints = 0;
  const lossThenFault = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => lossSocket as unknown as AttachmentWebSocket, undefined,
    async () => { lossEndpoints += 1; return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }); },
    () => 0.5, lossClock.runtime,
  );
  lossThenFault.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  const lostGeneration = lossThenFault.connect();
  bindTransportSource(lossThenFault, lostGeneration);
  lossSocket.emit("close", { code: 1006, reason: "lost" });
  equal(lossClock.pending(), 1, "unexpected loss did not initially arm retry");
  lossThenFault.finalize(Object.freeze({ generation: lostGeneration, cause: "OUT_OF_STATE" }));
  equal(lossClock.pending(), 0, "fault without a current socket did not cancel armed retry");
  await lossClock.advance(MAX_RECONNECT_ELAPSED_MS);
  equal(lossEndpoints, 0, "fault without a current socket still retried");
});

test("explicit_detach_is_one_orderly_close_and_never_auto_retries", async () => {
  const clock = new FakeReconnectClock();
  const socket = new TransportFakeSocket(1);
  let endpoints = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => socket as unknown as AttachmentWebSocket, ["persea-handle.consumed"],
    async () => { endpoints += 1; return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }); },
    () => 0.5, clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  transport.connect();
  transport.detach("detached");
  transport.detach("detached");
  equal(socket.closes.length, 1, "explicit Detach did not close exactly once");
  equal(socket.closes[0].code, 1000, "explicit Detach was not orderly");
  socket.emit("close", { code: 1000, reason: "detached" });
  await clock.advance(MAX_RECONNECT_ELAPSED_MS);
  equal(endpoints, 0, "explicit Detach armed automatic retry");
  equal(clock.pending(), 0, "explicit Detach left retry work");
});

test("immediate_attach_again_retries_lease_held_without_protocol_fault", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const statuses: string[] = [];
  let acquisitions = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, ["persea-handle.consumed"],
    async () => {
      acquisitions += 1;
      if (acquisitions === 1) throw new Error("lease_held");
      return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh-after-release"]) });
    },
    () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: (generation) => { statuses.push(`OPEN:${generation}`); },
    receiveDecoded: () => "ENQUEUED",
    transportClosed: (_generation, reason) => { statuses.push(`CLOSED:${reason}`); },
    reconnectStatus: (value) => { statuses.push(`${value.state}:${value.attempt}`); },
  });
  const generation = transport.connect();
  bindTransportSource(transport, generation);
  transport.detach("detached");
  transport.attachAgain();
  await clock.advance(0);
  equal(acquisitions, 1, "immediate Attach Again did not encounter the held lease exactly once");
  equal(sockets.length, 1, "lease_held created a replacement socket before release");
  equal(clock.pending(), 1, "lease_held did not retain one bounded retry timer");
  assert(statuses.includes("WAITING:2"), "lease_held was not converted into the next bounded acquisition attempt");
  assert(!statuses.some((value) => /OUT_OF_STATE|protocol/i.test(value)), "lease_held became a protocol fault");
  await clock.advance(500);
  equal(acquisitions, 2, "orderly release did not permit the bounded retry");
  equal(sockets.length, 2, "post-release retry did not create one fresh socket");
  equal(statuses.at(-1), "OPEN:2", "post-release retry did not open the higher generation");
  transport.connectionCommitted(2);
  equal(clock.pending(), 0, "successful post-release COMMIT retained retry work");
  transport.detach();
});

test("never_resolving_endpoint_is_aborted_and_goes_offline_after_the_fast_phase", async () => {
  const clock = new FakeReconnectClock();
  const signals: AbortSignal[] = [];
  const statuses: string[] = [];
  let sockets = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => { sockets += 1; return new TransportFakeSocket() as unknown as AttachmentWebSocket; }, ["persea-handle.consumed"],
    (signal) => { signals.push(signal); return new Promise(() => {}); }, () => 0.5, clock.runtime,
  );
  transport.bind({
    openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
    reconnectStatus: (status) => { statuses.push(`${status.state}:${status.attempt}`); },
  });
  bindTransportSource(transport);
  transport.attachAgain();
  await clock.advance(MAX_RECONNECT_ELAPSED_MS + MAX_RECONNECT_ATTEMPT_MS);
  assert(signals.length > 0 && signals.length <= MAX_RECONNECT_ATTEMPTS, "never-resolving endpoint attempts were not bounded");
  assert(signals.every((signal) => signal.aborted), "timed-out inventory acquisition was not aborted");
  equal(sockets, 0, "never-resolving inventory created a socket");
  assert(statuses.at(-1)?.startsWith("OFFLINE:"), "never-resolving inventory did not go OFFLINE within the fast phase and one attempt");
  transport.detach();
  equal(clock.pending(), 0);
});

for (const socketState of [0, 1] as const) {
  test(socketState === 0 ? "forever_connecting_socket_is_closed_and_goes_offline" : "open_without_commit_is_closed_and_goes_offline", async () => {
    const clock = new FakeReconnectClock();
    const sockets: TransportFakeSocket[] = [];
    const protocols: string[][] = [];
    const statuses: string[] = [];
    let attempt = 0;
    const transport = new WebSocketAttachmentTransport(
      "ws://example.test/ws", (_url, values) => {
        protocols.push(values ?? []);
        const socket = new TransportFakeSocket(socketState);
        sockets.push(socket);
        return socket as unknown as AttachmentWebSocket;
      }, ["persea-handle.consumed"],
      async () => { attempt += 1; return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze([`persea-handle.fresh-${attempt}`]) }); },
      () => 0.5, clock.runtime,
    );
    transport.bind({
      openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {},
      reconnectStatus: (status) => { statuses.push(status.state); },
    });
    bindTransportSource(transport);
    transport.attachAgain();
    await clock.advance(MAX_RECONNECT_ELAPSED_MS + MAX_RECONNECT_ATTEMPT_MS);
    assert(sockets.length > 0 && sockets.length <= MAX_RECONNECT_ATTEMPTS, "socket-to-COMMIT attempts were not bounded");
    // A browser refuses an invalid close code and leaves the socket open, so
    // the attempt would keep its server attachment and lease: each abandoned
    // socket must actually reach CLOSED.
    assert(sockets.every((socket) => socket.readyState === 3 && socket.closes.length === 1 && socket.closes[0].code === CLOSE_CLIENT_FAULT), "timed-out socket was not closed exactly once with a browser-accepted code");
    assert(protocols.every((values) => values.length === 1 && values[0].startsWith("persea-handle.fresh-") && !values.includes("persea-handle.consumed")), "reconnect reused a consumed capability");
    equal(statuses.at(-1), "OFFLINE", "socket-to-COMMIT hang did not go OFFLINE visibly");
    transport.detach();
  });
}

test("late_endpoint_resolution_after_timeout_is_stale_and_cannot_create_a_socket", async () => {
  const clock = new FakeReconnectClock();
  let resolveEndpoint!: (value: Readonly<{ url: string; protocols: readonly string[] }>) => void;
  let signal: AbortSignal | undefined;
  let sockets = 0;
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => { sockets += 1; return new TransportFakeSocket() as unknown as AttachmentWebSocket; }, ["persea-handle.consumed"],
    (value) => { signal = value; return new Promise((resolve) => { resolveEndpoint = resolve; }); }, () => 0.5, clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  bindTransportSource(transport);
  transport.attachAgain();
  await clock.advance(0);
  await clock.advance(MAX_RECONNECT_ATTEMPT_MS);
  assert(signal?.aborted, "timed-out endpoint acquisition was not canceled");
  resolveEndpoint(Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.stale"]) }));
  await clock.flush();
  equal(sockets, 0, "late stale endpoint resolution created a socket");
  transport.detach();
  equal(clock.pending(), 0);
});

test("detach_cancels_endpoint_and_socket_attempts_without_late_work", async () => {
  const acquisitionClock = new FakeReconnectClock();
  let acquisitionSignal: AbortSignal | undefined;
  let lateResolve!: (value: Readonly<{ url: string; protocols: readonly string[] }>) => void;
  let acquisitionSockets = 0;
  const acquisition = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => { acquisitionSockets += 1; return new TransportFakeSocket() as unknown as AttachmentWebSocket; }, undefined,
    (signal) => { acquisitionSignal = signal; return new Promise((resolve) => { lateResolve = resolve; }); }, () => 0.5, acquisitionClock.runtime,
  );
  acquisition.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  bindTransportSource(acquisition);
  acquisition.attachAgain();
  await acquisitionClock.advance(0);
  acquisition.detach();
  assert(acquisitionSignal?.aborted, "Detach did not abort endpoint acquisition");
  equal(acquisitionClock.pending(), 0, "Detach left the acquisition deadline armed");
  lateResolve(Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.late"]) }));
  await acquisitionClock.flush();
  equal(acquisitionSockets, 0, "detached acquisition created a late socket");

  const connectionClock = new FakeReconnectClock();
  const connectionSockets: TransportFakeSocket[] = [];
  const connection = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(0);
      connectionSockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, undefined,
    async () => Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }), () => 0.5, connectionClock.runtime,
  );
  connection.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  bindTransportSource(connection);
  connection.attachAgain();
  await connectionClock.advance(0);
  equal(connectionSockets.length, 1);
  connection.detach();
  equal(connectionSockets[0].closes[0].code, 1000, "explicit Detach did not orderly-close its owned CONNECTING socket");
  equal(connectionClock.pending(), 0, "Detach left the socket-to-COMMIT deadline armed");
  await connectionClock.advance(MAX_RECONNECT_ELAPSED_MS);
  equal(connectionSockets.length, 1, "Detach created another socket");
});

test("manual_supersession_and_destroy_cancel_exact_attempt_ownership", async () => {
  const clock = new FakeReconnectClock();
  const signals: AbortSignal[] = [];
  let resolveStale!: (value: Readonly<{ url: string; protocols: readonly string[] }>) => void;
  let calls = 0;
  const protocols: string[][] = [];
  const sockets: TransportFakeSocket[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", (_url, values) => {
      protocols.push(values ?? []);
      const socket = new TransportFakeSocket(0);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, undefined,
    (signal) => {
      signals.push(signal);
      calls += 1;
      if (calls === 1) return new Promise((resolve) => { resolveStale = resolve; });
      return Promise.resolve(Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh-second"]) }));
    }, () => 0.5, clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  bindTransportSource(transport);
  transport.attachAgain();
  await clock.advance(0);
  transport.attachAgain();
  assert(signals[0].aborted, "manual retry-token supersession did not abort the prior endpoint attempt");
  await clock.advance(0);
  equal(sockets.length, 1, "manual supersession did not create exactly one current socket");
  equal(protocols[0].join(","), "persea-handle.fresh-second");
  resolveStale(Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.stale-first"]) }));
  await clock.flush();
  equal(sockets.length, 1, "superseded late endpoint resolution created an extra socket");
  transport.destroy();
  assert(signals[1].aborted, "destroy did not abort the current attempt controller");
  equal(sockets[0].closes[0].code, 1000, "destroy did not orderly-close the exact owned socket");
  equal(clock.pending(), 0, "destroy left retry ownership armed");
  await clock.advance(MAX_RECONNECT_ELAPSED_MS);
  equal(sockets.length, 1, "destroy allowed post-cancellation socket creation");
});

test("successful_commit_cancels_attempt_deadline_and_prevents_extra_socket", async () => {
  const clock = new FakeReconnectClock();
  const sockets: TransportFakeSocket[] = [];
  const signals: AbortSignal[] = [];
  const transport = new WebSocketAttachmentTransport(
    "ws://example.test/ws", () => {
      const socket = new TransportFakeSocket(1);
      sockets.push(socket);
      return socket as unknown as AttachmentWebSocket;
    }, undefined,
    async (signal) => { signals.push(signal); return Object.freeze({ url: "ws://example.test/ws", protocols: Object.freeze(["persea-handle.fresh"]) }); }, () => 0.5, clock.runtime,
  );
  transport.bind({ openTransport: () => {}, receiveDecoded: () => "ENQUEUED", transportClosed: () => {} });
  bindTransportSource(transport);
  transport.attachAgain();
  await clock.advance(0);
  equal(sockets.length, 1);
  transport.connectionCommitted(1);
  assert(signals[0].aborted, "COMMIT did not cancel the attempt controller");
  equal(clock.pending(), 0, "COMMIT left the socket-to-COMMIT deadline armed");
  await clock.advance(MAX_RECONNECT_ELAPSED_MS * 2);
  equal(sockets.length, 1, "post-COMMIT timer created an extra socket");
  equal(sockets[0].closes.length, 0, "post-COMMIT deadline closed the live socket");
  transport.detach();
});

test("protocol_accepts_every_canonical_tag_and_direction", () => {
  const frames: unknown[] = [prepare(), prepare({ kind: "HISTORY", request: 1n, effectiveHistoryRows: 1_000 }), ready(), { type: "DEFER", version: 1, source: "source", epoch: 1n, cut: 1n, reason: "INTERSECTING_DOM_SELECTION" }, commit(), live(), { type: "MODE_REQUEST", version: 1, source: "source", epoch: 1n, mode: "CONTROL" }, { type: "MODE", version: 1, source: "source", epoch: 1n, mode: "OBSERVE" }, { type: "INPUT", version: 1, source: "source", epoch: 1n, data: new Uint8Array([1]) }, { type: "RESIZE_REQUEST", version: 1, source: "source", epoch: 1n, columns: 80, rows: 24 }, { type: "HISTORY_REQUEST", version: 1, source: "source", epoch: 1n, request: 1n, historyRows: 1_000 }, { type: "END", version: 1, source: "source", epoch: 1n, reason: "done" }];
  for (const frame of frames) equal(validateDecodedAttachmentFrame(frame).type, (frame as { type: string }).type);
  for (const frame of frames) {
    const tag = (frame as { type: string }).type;
    const direction = ["PREPARE", "COMMIT", "LIVE", "MODE", "END"].includes(tag) ? "server" : "browser";
    validateDecodedAttachmentFrame(frame, direction);
    throws(() => validateDecodedAttachmentFrame(frame, direction === "server" ? "browser" : "server"));
  }
});

test("protocol_rejects_unknown_tags_fields_prototypes_and_field_combinations", () => {
  throws(() => validateServerFrame({ ...prepare(), type: "NOPE" }));
  throws(() => validateServerFrame({ ...prepare(), extra: true }));
  throws(() => validateServerFrame(Object.create(null)));
  const accessor = { ...prepare() } as Record<string, unknown>;
  Object.defineProperty(accessor, "source", { get: () => "source", enumerable: true });
  throws(() => validateServerFrame(accessor));
  throws(() => validateServerFrame({ ...prepare(), kind: "ONGOING", replay: undefined }));
});

test("protocol_enforces_source_reason_uint64_and_geometry_boundaries", () => {
  validateServerFrame(prepare({ source: "x", epoch: MAX_UINT64, cut: MAX_UINT64, columns: MAX_GEOMETRY_CELLS, rows: 1 }));
  throws(() => validateServerFrame(prepare({ source: "" })));
  throws(() => validateServerFrame(prepare({ source: "x".repeat(MAX_SOURCE_UTF8_BYTES + 1) })));
  throws(() => validateServerFrame(prepare({ epoch: 0n })));
  throws(() => validateServerFrame(prepare({ cut: MAX_UINT64 + 1n })));
  throws(() => validateServerFrame(prepare({ columns: 0 })));
  throws(() => validateServerFrame(prepare({ rows: MAX_GEOMETRY_CELLS + 1 })));
  validateDecodedAttachmentFrame({ type: "RESIZE_REQUEST", version: 1, source: "source", epoch: 1n, columns: 1, rows: MAX_GEOMETRY_CELLS }, "browser");
  throws(() => validateDecodedAttachmentFrame({ type: "RESIZE_REQUEST", version: 1, source: "source", epoch: 1n, columns: 0, rows: 24 }, "browser"));
  throws(() => validateServerFrame({ type: "END", version: 1, source: "source", epoch: 1n, reason: "" }));
});

test("protocol_enforces_history_row_total_replay_live_and_input_caps", () => {
  validateServerFrame(prepare({ history: new Array(MAX_HISTORY_ROWS).fill(""), replay: new Uint8Array(MAX_REPLAY_BYTES) }));
  throws(() => validateServerFrame(prepare({ history: new Array(MAX_HISTORY_ROWS + 1).fill("") })));
  throws(() => validateServerFrame(prepare({ history: ["x".repeat(MAX_HISTORY_ROW_UTF8_BYTES + 1)] })));
  throws(() => validateServerFrame(prepare({ history: ["bad\0row"] })));
  throws(() => validateServerFrame(prepare({ history: new Array(Math.floor(MAX_HISTORY_UTF8_BYTES / 8_001) + 1).fill("x".repeat(8_000)) })));
  equal(MAX_HISTORY_UTF8_BYTES, 2_097_152);
  throws(() => validateServerFrame(prepare({ replay: new Uint8Array(MAX_REPLAY_BYTES + 1) })));
  validateServerFrame({ ...live(), data: new Uint8Array(MAX_LIVE_BYTES) });
  throws(() => validateServerFrame({ ...live(), data: new Uint8Array(0) }));
  throws(() => validateDecodedAttachmentFrame({ type: "INPUT", version: 1, source: "source", epoch: 1n, data: new Uint8Array(MAX_INPUT_BYTES + 1) }, "browser"));
});

test("protocol_live_is_cut_bearing_and_no_sequence_exists", () => {
  throws(() => validateServerFrame({ type: "LIVE", version: 1, source: "source", epoch: 1n, data: new Uint8Array([1]) }));
  throws(() => validateServerFrame({ ...live(), sequence: 1 }));
});

test("protocol_clones_decoded_arrays_and_transfers_outbound_ownership_once", () => {
  const replay = new Uint8Array([3]);
  const decoded = validateServerFrame(prepare({ replay, history: ["stable"] })) as Prepare;
  replay[0] = 9;
  equal(decoded.replay[0], 3);
  const data = new Uint8Array([7]);
  const cloned = cloneBrowserFrame({ type: "INPUT", version: 1, source: "source", epoch: 1n, data });
  data[0] = 1;
  equal(cloned.type === "INPUT" ? cloned.data[0] : 0, 7);
});

test("composer_scope_is_display_inert_exact_and_preserved_across_navigation_cleanup", () => {
  const dashboard = readWorkspaceText("src/dashboard.ts");
  const app = readWorkspaceText("src/app.ts");
  const composer = readWorkspaceText("src/composer.ts");
  assert(dashboard.includes("draftScope: authority.key"), "dashboard did not project the exact authority identity");
  assert(dashboard.includes("{ draft_scope: draftScope }"), "terminalURL did not preserve the draft scope fragment");
  assert(app.includes("validateComposerStorageScope(draftScopeFragment)")
    && app.includes("{ composerStorageScope }")
    && app.includes("draft_scope: session.draftScope"),
    "draft scope did not survive validation and first-commit cleanup");
  assert(composer.includes('const DRAFT_STORAGE_PREFIX = "persea-terminal.composer.draft.v1."')
    && composer.includes("this.storageKey = options.storageScope === undefined")
    && !composer.includes("sourceLabel") && !composer.includes("aliasLabel") && !composer.includes("handle"),
  "Composer storage identity has a label or capability fallback");
});

test("composer_mobile_repair_structure_is_shared_bounded_and_operator_vocabulary_only", () => {
  const composer = readWorkspaceText("src/composer.ts");
  const helper = readWorkspaceText("src/keyboard_preserving_button.ts");
  const css = ["attachment_shell", "attachment_composer", "attachment_surface", "unified_terminal_surface", "unified_terminal_toolbar", "unified_terminal_composer", "unified_terminal_overlays", "unified_terminal_disclosures"]
    .map((name) => readWorkspaceText(`src/${name}.css`)).join("\n");
  equal((helper.match(/addEventListener\(/g) ?? []).length, 4, "shared activation helper must own exactly four listeners");
  for (const type of ["mousedown", "touchstart", "pointerdown", "click"]) {
    assert(helper.includes(`addEventListener("${type}"`), `shared activation helper lost ${type}`);
  }
  assert(composer.includes("this.resizePercent > 0 ? cap")
    && composer.includes('this.setResizePercent(this.size === "expanded" ? 45 : 25)')
    && css.includes('[data-sent-reopened="true"]:not([hidden])')
    && css.includes('[data-composer="open"] .attachment-page__keybar-handle { display: none; }')
    && css.includes('[data-sent-reopened="true"] .attachment-page__composer-header {\n  display: flex;\n}'),
  "meaningful size or N3/N4/N11 secondary repair is absent");
  assert(css.includes("html, body, #app")
    && (css.match(/overscroll-behavior: none;/g) ?? []).length >= 2
    && composer.includes('this.button(">_", "Code input", this.onToggleMode')
    && composer.includes('this.modeButton.textContent = prose ? "Aa" : ">_"')
    && !css.includes(".attachment-page__composer-mode-caption")
    && composer.includes("button.title = label")
    && css.includes(".attachment-page__compose-tab { font-size: 16px; }")
    && css.includes(".attachment-page__composer button,\n  .attachment-page__composer output {\n    font-size: 16px;"),
  "overscroll containment or Composer affordance labels are absent");
  for (const visible of [
    "Insert \\u25b8",
    "Insert into terminal without running it",
    "Inserted ${this.lastSentLength.toLocaleString()} ch",
    "Insert anyway",
    "Restore the last cleared or inserted draft",
  ]) assert(composer.includes(visible), `missing Composer vocabulary: ${visible}`);
  // No "not run" qualifier, no idle instruction line, no
  // counter, one autocorrect toggle.
  for (const retired of [
    "\\u00b7 not run",
    "· not run",
    "Ready to insert without running it.",
    "switches autocorrect",
    "composer-counter",
    "Prose autocorrect mode",
    "Code autocorrect mode",
  ]) assert(!composer.includes(retired), `retired Composer chrome is back: ${retired}`);
  assert(!/\b(?:Send|Sent)\b/.test(
    [...composer.matchAll(/(["'`])((?:\\.|(?!\1).)*)\1/g)].map((match) => match[2]).join("\n"),
  ), "user-visible Composer string retained Send/Sent vocabulary");
  assert(composer.includes("onSend") && composer.includes('"SENT"') && composer.includes("sends"),
    "internal send/SENT instrumentation vocabulary changed");
});

test("production_document_preserves_nonce_and_source_bound_csrf_contracts", () => {
  const index = readWorkspaceText("index.html");
  equal(index.split("__PERSEA_STYLE_NONCE__").length - 1, 1, "index must expose exactly one nonce placeholder");
  const app = readWorkspaceText("src/app.ts");
  const endpoints = readWorkspaceText("src/unified_pane_controller.ts");
  const transport = readWorkspaceText("src/websocket_attachment_transport.ts");
  // pagehide always lets the attachment go: a page entering the back/forward
  // cache suspends (detach, remembering whether to reattach), any other detaches.
  assert(app.includes('if (event.persisted) controller.suspend();') && app.includes('else controller.detach("page_hidden");'), "current pagehide lost attachment cleanup");
  assert(endpoints.includes('this.transport.detach("page_hidden");'), "suspend no longer detaches the attachment");
  assert(endpoints.includes('credentials: "same-origin", signal'), "fresh handle acquisition lost cancelable same-origin ownership");
  assert(endpoints.includes("const csrf = await refreshCSRFToken(signal)")
    && endpoints.indexOf("refreshCSRFToken(signal)") < endpoints.indexOf('window.fetch("/api/attachment-handles"'),
  "long-lived terminal mutation does not refresh its bounded CSRF cookie before POST");
  assert(endpoints.includes('"/api/attachment-handles"') && !app.includes("targetFromFragment") && !endpoints.includes("targetFromFragment"), "browser target metadata became a handle-minting authority");
  assert(transport.includes('if (frame.type === "PREPARE") this.recordPreparedSource(generation, frame.source)'), "transport lost server PREPARE source ownership");
});

async function main(): Promise<void> {
  const outcomes: Array<{ name: string; status: "PASS" | "FAIL"; error?: string }> = [];
  for (const entry of tests) {
    try { await entry.run(); outcomes.push({ name: entry.name, status: "PASS" }); }
    catch (error) { outcomes.push({ name: entry.name, status: "FAIL", error: error instanceof Error ? error.stack ?? error.message : String(error) }); }
  }
  const failed = outcomes.filter((outcome) => outcome.status === "FAIL");
  console.log(JSON.stringify({ schema: "persea-terminal-ui-unit-v1", count: outcomes.length, outcomes }, null, 2));
  if (failed.length > 0) throw new Error(`${failed.length} deterministic case(s) failed`);
}

void main();
