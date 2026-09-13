export const TRANSPORT_LIVENESS_PREFIX = "PERSEA-LIVENESS/1 ";
export const TRANSPORT_LIVENESS_FRAME_BYTES = 55;
export const TRANSPORT_LIVENESS_CHALLENGE_MS = 10_000;
export const TRANSPORT_LIVENESS_PROOF_MS = 20_000;

const noncePattern = /^[0-9a-f]{32}$/;

export type TransportLivenessRuntime = Readonly<{
  now(): number;
  wallNow(): number;
  setTimeout(callback: () => void, delayMs: number): unknown;
  clearTimeout(handle: unknown): void;
  subscribeWake(callback: () => void): () => void;
}>;

export type TransportLivenessNonceSource = () => string;
export type ServerLivenessFrame =
  | Readonly<{ type: "ORDINARY" }>
  | Readonly<{ type: "PONG"; nonce: string }>
  | Readonly<{ type: "VIOLATION" }>;

export function decodeServerLivenessFrame(payload: string): ServerLivenessFrame {
  if (!payload.startsWith(TRANSPORT_LIVENESS_PREFIX)) return Object.freeze({ type: "ORDINARY" });
  if (payload.length !== TRANSPORT_LIVENESS_FRAME_BYTES) return Object.freeze({ type: "VIOLATION" });
  const verb = payload.slice(TRANSPORT_LIVENESS_PREFIX.length, TRANSPORT_LIVENESS_PREFIX.length + 4);
  const separator = payload[TRANSPORT_LIVENESS_PREFIX.length + 4];
  const nonce = payload.slice(TRANSPORT_LIVENESS_PREFIX.length + 5);
  if (verb !== "PONG" || separator !== " " || !noncePattern.test(nonce)) return Object.freeze({ type: "VIOLATION" });
  return Object.freeze({ type: "PONG", nonce });
}

export function encodeBrowserLivenessPing(nonce: string): string {
  if (!noncePattern.test(nonce)) throw new Error("transport liveness nonce unavailable");
  return `${TRANSPORT_LIVENESS_PREFIX}PING ${nonce}`;
}

export const secureTransportLivenessNonce: TransportLivenessNonceSource = () => {
  const bytes = new Uint8Array(16);
  globalThis.crypto.getRandomValues(bytes);
  let nonce = "";
  for (const value of bytes) nonce += value.toString(16).padStart(2, "0");
  return nonce;
};

export const browserTransportLivenessRuntime: TransportLivenessRuntime = Object.freeze({
  now: () => globalThis.performance.now(),
  wallNow: () => Date.now(),
  setTimeout: (callback: () => void, delayMs: number) => globalThis.setTimeout(callback, delayMs),
  clearTimeout: (handle: unknown) => globalThis.clearTimeout(handle as ReturnType<typeof setTimeout>),
  subscribeWake: (callback: () => void) => {
    const focus = () => callback();
    const online = () => callback();
    const visible = () => { if (document.visibilityState === "visible") callback(); };
    globalThis.addEventListener("focus", focus);
    globalThis.addEventListener("online", online);
    document.addEventListener("visibilitychange", visible);
    return () => {
      globalThis.removeEventListener("focus", focus);
      globalThis.removeEventListener("online", online);
      document.removeEventListener("visibilitychange", visible);
    };
  },
});

export class TransportLiveness {
  private state: "IDLE" | "ACTIVE" | "RETIRED" = "IDLE";
  private timer?: unknown;
  private timerVersion = 0;
  private timerScheduledMonotonicAt?: number;
  private timerEvent?: "CHALLENGE" | "EXPIRY";
  private timerEventMonotonicAt?: number;
  private timerEventWallAt?: number;
  private unsubscribeWake?: () => void;
  private outstandingNonce?: string;
  private monotonicExpiresAt?: number;
  private wallExpiresAt?: number;
  private lastMonotonicNow?: number;
  private lastWallNow?: number;

  constructor(
    private readonly sendText: (payload: string) => void,
    private readonly isOpen: () => boolean,
    private readonly onFailure: (reason: string) => void,
    private readonly runtime: TransportLivenessRuntime = browserTransportLivenessRuntime,
    private readonly nonceSource: TransportLivenessNonceSource = secureTransportLivenessNonce,
  ) {}

  start(): void {
    if (this.state !== "IDLE") return;
    this.state = "ACTIVE";
    this.unsubscribeWake = this.runtime.subscribeWake(() => this.evaluateExpiry());
    const observed = this.observeClocks();
    if (!observed) return;
    this.monotonicExpiresAt = observed.monotonic + TRANSPORT_LIVENESS_CHALLENGE_MS;
    this.wallExpiresAt = observed.wall + TRANSPORT_LIVENESS_CHALLENGE_MS;
    this.sendChallenge(observed);
  }

  retire(): void {
    if (this.state === "RETIRED") return;
    this.state = "RETIRED";
    this.clearTimer();
    this.unsubscribeWake?.();
    this.unsubscribeWake = undefined;
    this.outstandingNonce = undefined;
    this.monotonicExpiresAt = undefined;
    this.wallExpiresAt = undefined;
    this.lastMonotonicNow = undefined;
    this.lastWallNow = undefined;
  }

  receive(payload: string): "ORDINARY" | "CONSUMED" | "FAILED" {
    const decoded = decodeServerLivenessFrame(payload);
    if (decoded.type === "ORDINARY") return "ORDINARY";
    if (this.state !== "ACTIVE") return "FAILED";
    if (decoded.type !== "PONG" || decoded.nonce !== this.outstandingNonce) {
      this.fail("liveness_protocol");
      return "FAILED";
    }
    const observed = this.observeClocks();
    if (!observed) return "FAILED";
    if (this.expired(observed)) {
      this.fail("liveness_timeout");
      return "FAILED";
    }
    this.outstandingNonce = undefined;
    this.clearTimer();
    this.monotonicExpiresAt = observed.monotonic + TRANSPORT_LIVENESS_PROOF_MS;
    this.wallExpiresAt = observed.wall + TRANSPORT_LIVENESS_PROOF_MS;
    this.scheduleEvent(
      "CHALLENGE",
      observed.monotonic + TRANSPORT_LIVENESS_CHALLENGE_MS,
      observed.wall + TRANSPORT_LIVENESS_CHALLENGE_MS,
      observed,
    );
    return "CONSUMED";
  }

  beforeInput(): boolean {
    if (this.state !== "ACTIVE") return false;
    const observed = this.observeClocks();
    if (!observed) return false;
    if (this.expired(observed)) {
      this.fail("liveness_timeout");
      return false;
    }
    this.reconcileTimer(observed);
    return this.state === "ACTIVE";
  }

  private sendChallenge(observed: Readonly<{ monotonic: number; wall: number }>): void {
    if (this.state !== "ACTIVE") return;
    if (this.expired(observed)) {
      this.fail("liveness_timeout");
      return;
    }
    this.monotonicExpiresAt = Math.min(this.monotonicExpiresAt!, observed.monotonic + TRANSPORT_LIVENESS_CHALLENGE_MS);
    this.wallExpiresAt = Math.min(this.wallExpiresAt!, observed.wall + TRANSPORT_LIVENESS_CHALLENGE_MS);
    if (!this.isOpen()) {
      this.fail("liveness_send_unavailable");
      return;
    }
    let nonce: string;
    let payload: string;
    try {
      nonce = this.nonceSource();
      payload = encodeBrowserLivenessPing(nonce);
      this.sendText(payload);
    } catch {
      this.fail("liveness_send_failed");
      return;
    }
    this.outstandingNonce = nonce;
    this.scheduleEvent("EXPIRY", this.monotonicExpiresAt, this.wallExpiresAt, observed);
  }

  private evaluateExpiry(): void {
    if (this.state !== "ACTIVE") return;
    const observed = this.observeClocks();
    if (!observed) return;
    if (this.expired(observed)) {
      this.fail("liveness_timeout");
      return;
    }
    this.reconcileTimer(observed);
  }

  private observeClocks(): Readonly<{ monotonic: number; wall: number }> | undefined {
    const monotonic = this.runtime.now();
    const wall = this.runtime.wallNow();
    if (!Number.isFinite(monotonic) || !Number.isFinite(wall)
      || (this.lastMonotonicNow !== undefined && monotonic < this.lastMonotonicNow)
      || (this.lastWallNow !== undefined && wall < this.lastWallNow)) {
      this.fail("liveness_clock_anomaly");
      return undefined;
    }
    this.lastMonotonicNow = monotonic;
    this.lastWallNow = wall;
    return Object.freeze({ monotonic, wall });
  }

  private expired(observed: Readonly<{ monotonic: number; wall: number }>): boolean {
    return this.monotonicExpiresAt === undefined || this.wallExpiresAt === undefined
      || observed.monotonic >= this.monotonicExpiresAt || observed.wall >= this.wallExpiresAt;
  }

  private scheduleEvent(
    event: "CHALLENGE" | "EXPIRY",
    monotonicAt: number,
    wallAt: number,
    observed: Readonly<{ monotonic: number; wall: number }>,
  ): void {
    this.clearTimer();
    this.timerEvent = event;
    this.timerEventMonotonicAt = monotonicAt;
    this.timerEventWallAt = wallAt;
    this.reconcileTimer(observed);
  }

  private reconcileTimer(observed: Readonly<{ monotonic: number; wall: number }>): void {
    if (this.state !== "ACTIVE") return;
    const event = this.timerEvent;
    const eventMonotonicAt = this.timerEventMonotonicAt;
    const eventWallAt = this.timerEventWallAt;
    if (event === undefined || eventMonotonicAt === undefined || eventWallAt === undefined
      || this.monotonicExpiresAt === undefined || this.wallExpiresAt === undefined) {
      this.fail("liveness_timer_unavailable");
      return;
    }
    if (observed.monotonic >= eventMonotonicAt || observed.wall >= eventWallAt) {
      if (event === "EXPIRY") {
        this.fail("liveness_timeout");
        return;
      }
      this.clearTimer();
      this.sendChallenge(observed);
      return;
    }
    const effectiveMonotonicAt = Math.min(eventMonotonicAt, this.monotonicExpiresAt);
    const effectiveWallAt = Math.min(eventWallAt, this.wallExpiresAt);
    const delay = Math.max(0, Math.min(
      effectiveMonotonicAt - observed.monotonic,
      effectiveWallAt - observed.wall,
    ));
    const scheduledMonotonicAt = observed.monotonic + delay;
    if (this.timer !== undefined && this.timerScheduledMonotonicAt !== undefined
      && this.timerScheduledMonotonicAt <= scheduledMonotonicAt) return;
    this.cancelScheduledTimer();
    const version = ++this.timerVersion;
    this.timerScheduledMonotonicAt = scheduledMonotonicAt;
    this.timer = this.runtime.setTimeout(() => {
      if (this.state !== "ACTIVE" || this.timerVersion !== version) return;
      this.timer = undefined;
      this.timerScheduledMonotonicAt = undefined;
      this.timerVersion += 1;
      this.evaluateExpiry();
    }, delay);
  }

  private cancelScheduledTimer(): void {
    if (this.timer !== undefined) this.runtime.clearTimeout(this.timer);
    this.timer = undefined;
    this.timerScheduledMonotonicAt = undefined;
    this.timerVersion += 1;
  }

  private clearTimer(): void {
    this.cancelScheduledTimer();
    this.timerEvent = undefined;
    this.timerEventMonotonicAt = undefined;
    this.timerEventWallAt = undefined;
  }

  private fail(reason: string): void {
    if (this.state !== "ACTIVE") return;
    this.retire();
    this.onFailure(reason);
  }
}
