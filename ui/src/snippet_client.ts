// The document-global snippets / clips service.
//
// ONE service per document. It owns everything that is not pane-local: the
// `/api/snippets` transport, the single-flight poll loop, the authoritative
// snapshot every open sheet renders, and the device-local OSC 52 coalescer
// that publishes through the one distinguished `PUT /api/snippets/osc52`
// record. It owns NO terminal: it never selects a pane, never inserts, never
// focuses, and never touches geometry. Pane-local effects belong to the
// UnifiedTerminalPage that the operator actually tapped, reached through the
// UnifiedPaneController that owns it (clipboard integration law).
//
// Construction is inert: no request is made until a consumer retains the live
// state (retainLive) or performs a mutation, so building a service — or a
// controller that holds one — attaches, adopts, mints and navigates nothing.
//
// Every mutation goes through ONE secured requester (`send` below): same
// origin, credentials included, the CSRF header, the exact JSON content type,
// and `cache: "no-store"` on every method. There is exactly one `fetch` call
// site in this module, and no other module in the UI speaks to /api/snippets.

import { csrfToken } from "./csrf_refresh";
import { isClipboardRetentionSeconds, type ClipboardRetentionSeconds } from "./clipboard_retention";

// --- Wire records ------------------------------------------------------------

export type SnippetKind = "snippet" | "clip";

/** A record exactly as the front door's list/mutation routes return it. */
export type SnippetRecord = Readonly<{
  id: string;
  kind: SnippetKind;
  label: string;
  body: string;
  pinned: boolean;
  origin: string;
  revision: number;
  createdAt: string;
  updatedAt: string;
  expiresAt: string | null;
  retentionSeconds: ClipboardRetentionSeconds;
  /** Clips carry an 80-rune server-derived preview; snippets carry "". */
  preview: string;
}>;

/** The fixed id of the one global, server-owned OSC 52 record (§3a, E2). */
export const OSC_SNIPPET_ID = "osc52";

/** Server limits, mirrored so the UI can state them; the server is authority. */
export const SNIPPET_BODY_MAX_BYTES = 16 << 10;
export const SNIPPET_MAX_SNIPPETS = 256;
export const SNIPPET_CLIP_RING = 20;
export const SNIPPET_LABEL_MAX_RUNES = 64;
export const SNIPPET_ORIGIN_MAX_RUNES = 32;

/** How long a device-local OSC 52 value is coalesced before it is published. */
export const OSC_QUIESCENCE_MS = 750;
/** The poll cadence while a view is retained live in a visible document. */
export const SNIPPET_POLL_MS = 4_000;

// --- Snapshot ----------------------------------------------------------------

export type SnippetStatus = "idle" | "loading" | "ready" | "unavailable";

/**
 * The authoritative view every subscriber renders. `snippets` and `clips`
 * keep the server's order (pinned first, then updated_at descending); the
 * distinguished OSC record is split out so a manual clip and the automatic
 * one are never confused for each other.
 */
/**
 * One outstanding device-clipboard delivery. `body` is the authority: it is
 * the immutable value the clipboard still owes, so an acknowledgement can
 * only be honoured by a copy that actually put THAT string on the clipboard.
  * `revision` is the OSC publication response's revision (0 when an older
  * response omitted it), never the revision of a canonical list item.
 */
export type ClipboardHandoff = Readonly<{
  body: string;
  revision: number;
}>;

export type SnippetSnapshot = Readonly<{
  status: SnippetStatus;
  snippets: readonly SnippetRecord[];
  clips: readonly SnippetRecord[];
  osc: SnippetRecord | undefined;
  /**
   * The last OSC 52 value reached the clips store but not this device's
   * system clipboard (iOS refuses writeText without a gesture). The clip row
   * is then the delivery and its Copy button is the tap.
   *
   * this names the EXACT value still owed to the clipboard, not a
   * bare flag. A bare flag let any successful copy retire it — a manual clip,
   * or a stale OSC row from before a newer failure — so the operator could be
   * told the automatic value had arrived when a different string had.
   */
  clipboardHandoff: ClipboardHandoff | undefined;
  /** Increments on every completed poll; lets a gate pin poll completion. */
  generation: number;
}>;

const EMPTY_SNAPSHOT: SnippetSnapshot = Object.freeze({
  status: "idle" as SnippetStatus,
  snippets: Object.freeze([]) as readonly SnippetRecord[],
  clips: Object.freeze([]) as readonly SnippetRecord[],
  osc: undefined,
  clipboardHandoff: undefined,
  generation: 0,
});

/**
 * What a mutation did, in the operator's terms. Every non-"ok" outcome has
 * one fixed sentence in the page's refusal table and removes the optimistic
 * row that was rendered for it — there is never a phantom item.
 */
export type SnippetOutcome =
  | "ok"
  | "conflict"
  | "full"
  | "too_large"
  | "unavailable"
  | "refused"
  | "unreachable";

// --- Pure helpers (unit-pinned) ---------------------------------------------

/**
 * Whether a body is storable at all: the server's closed grammar is 1…16 KiB
 * of valid UTF-8 whose only control runes are \n and \t. A body outside it
 * can only ever produce a 400, so the client never spends a request on one —
 * and it never rewrites the operator's (or the pane's) bytes to make it fit.
 */
export function isStorableSnippetBody(body: string): boolean {
  const bytes = new TextEncoder().encode(body).length;
  if (bytes === 0 || bytes > SNIPPET_BODY_MAX_BYTES) return false;
  for (const character of body) {
    const code = character.codePointAt(0) ?? 0;
    if (code === 0x0a || code === 0x09) continue;
    if (code < 0x20 || (code >= 0x7f && code <= 0x9f) || code === 0xfffd) return false;
  }
  return true;
}

/** A label the server accepts, derived from a body's first visible line. */
export function snippetLabelFromBody(body: string): string {
  const firstLine = body.split("\n").find((line) => line.trim() !== "") ?? "";
  const collapsed = firstLine.replace(/\t/g, " ").replace(/\s+/g, " ").trim();
  const runes = Array.from(collapsed);
  const label = runes.length > SNIPPET_LABEL_MAX_RUNES
    ? runes.slice(0, SNIPPET_LABEL_MAX_RUNES - 1).join("").trimEnd() + "…"
    : collapsed;
  return label;
}

/** An origin the server accepts: sanitized presentation metadata, never authority. */
export function sanitizeSnippetOrigin(value: string): string {
  // eslint-disable-next-line no-control-regex
  const runes = Array.from(value.replace(/[\u0000-\u001f\u007f-\u009f]/g, "").trim());
  const origin = runes.slice(0, SNIPPET_ORIGIN_MAX_RUNES).join("").trim();
  // The reserved token names the distinguished record and is refused as an
  // origin everywhere; a device that reports it gets no origin at all.
  return origin === OSC_SNIPPET_ID ? "" : origin;
}

export type OSC52Verdict =
  | Readonly<{ kind: "ignore"; reason: "read" | "selection" | "base64" | "utf8" | "oversize" | "grammar" | "malformed" }>
  | Readonly<{ kind: "write"; body: string }>;

/**
 * The longest padded base64 that can decode to SNIPPET_BODY_MAX_BYTES:
 * 4 * ceil(16384 / 3) = 21848 characters. clipboard-decoded-byte-cap: the pinned xterm parser
 * accepts up to 10,000,000 payload characters, every one of them chosen by
 * whatever is running in the pane, so the bound has to be applied to the
 * ENCODED length before atob decodes and allocates. Checking only the decoded
 * length still lets a pane force multi-megabyte work on a phone.
 */
export const OSC_BASE64_MAX_CHARS = 4 * Math.ceil(SNIPPET_BODY_MAX_BYTES / 3);

/**
 * The EXACT decoded size of a grammar-valid base64 string, computed from the
 * string alone: every four characters carry three bytes, less one byte per
 * `=` of padding. Callers must have validated the grammar first — padding is
 * only meaningful on a well-formed encoding.
 */
function decodedBase64Length(value: string): number {
  const padding = value.endsWith("==") ? 2 : value.endsWith("=") ? 1 : 0;
  return (value.length / 4) * 3 - padding;
}

/**
 * `"oversize"` and `"malformed"` are distinguished so the parser can report
 * the honest reason without re-testing the payload.
 */
type Base64Decoded = Uint8Array | "oversize" | "malformed";

function decodeBase64Bytes(value: string): Base64Decoded {
  if (value.length > OSC_BASE64_MAX_CHARS) return "oversize";
  if (value.length === 0 || value.length % 4 !== 0 || !/^[A-Za-z0-9+/]+={0,2}$/.test(value)) return "malformed";
  // 16,384 and 16,385 bytes encode to the SAME 21,848 characters, so
  // the coarse character cap above cannot separate them — only the padding
  // can. Decide the exact decoded size here, so a body one byte over the cap
  // is refused without atob ever allocating for it. This is the single
  // authority on the byte cap: there is deliberately no second check after
  // atob, because a check that can never fire cannot detect a regression anything.
  if (decodedBase64Length(value) > SNIPPET_BODY_MAX_BYTES) return "oversize";
  let binary: string;
  try {
    binary = atob(value);
  } catch {
    return "malformed";
  }
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index) & 0xff;
  return bytes;
}

/**
 * The frozen OSC 52 parser shape (§3d, M3). `data` is everything after
 * `ESC ] 52 ;`. A READ (`?`), an unknown selection, malformed base64, a
 * decoded value over 16 KiB, non-fatal-decodable UTF-8, and a body outside
 * the store's grammar are all *ignored*: the caller swallows them with `true`
 * and emits no reply — zero onData, zero wire bytes, zero persistence. Only
 * selections `c` and `p` write.
 */
export function parseOSC52(data: string): OSC52Verdict {
  const separator = data.indexOf(";");
  if (separator < 0) return Object.freeze({ kind: "ignore" as const, reason: "malformed" as const });
  const selection = data.slice(0, separator);
  const payload = data.slice(separator + 1);
  if (payload === "?") return Object.freeze({ kind: "ignore" as const, reason: "read" as const });
  if (selection !== "c" && selection !== "p") return Object.freeze({ kind: "ignore" as const, reason: "selection" as const });
  // the coarse character cap is applied FIRST, so a multi-megabyte
  // payload is refused before even the grammar regex runs over it. The exact
  // byte cap then falls out of the padding, still before atob.
  if (payload.length > OSC_BASE64_MAX_CHARS) return Object.freeze({ kind: "ignore" as const, reason: "oversize" as const });
  const decoded = decodeBase64Bytes(payload);
  if (decoded === "oversize") return Object.freeze({ kind: "ignore" as const, reason: "oversize" as const });
  if (decoded === "malformed") return Object.freeze({ kind: "ignore" as const, reason: "base64" as const });
  const bytes = decoded;
  let body: string;
  try {
    // Fatal UTF-8, never atob's Latin-1 string: the upstream clipboard addon
    // corrupts non-ASCII exactly by storing that (xterm.js #6000).
    body = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    return Object.freeze({ kind: "ignore" as const, reason: "utf8" as const });
  }
  if (!isStorableSnippetBody(body)) return Object.freeze({ kind: "ignore" as const, reason: "grammar" as const });
  return Object.freeze({ kind: "write" as const, body });
}

// --- Record parsing ----------------------------------------------------------

function asString(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

/**
 * Parses one wire record under the server's closed grammar. A record that
 * does not satisfy it fails the whole response closed: a partially rendered
 * list would tell the operator something the store does not hold.
 */
function parseRecord(value: unknown): SnippetRecord | undefined {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return undefined;
  const wire = value as Record<string, unknown>;
  const id = asString(wire.id);
  const kind = asString(wire.kind);
  const body = asString(wire.body);
  const label = asString(wire.label);
  const origin = asString(wire.origin);
  const createdAt = asString(wire.created_at);
  const updatedAt = asString(wire.updated_at);
  const revision = wire.revision;
  if (id === undefined || body === undefined || label === undefined || origin === undefined) return undefined;
  if (createdAt === undefined || updatedAt === undefined) return undefined;
  if (!Number.isFinite(Date.parse(createdAt)) || !Number.isFinite(Date.parse(updatedAt))) return undefined;
  if (kind !== "snippet" && kind !== "clip") return undefined;
  if (typeof revision !== "number" || !Number.isSafeInteger(revision) || revision < 1) return undefined;
  if (typeof wire.pinned !== "boolean") return undefined;
  if (!(id === OSC_SNIPPET_ID || /^[0-9a-f]{32}$/.test(id))) return undefined;
  const expires = wire.expires_at;
  const expiresAt = expires === null || expires === undefined ? null : asString(expires);
  if (expiresAt === undefined) return undefined;
  if (expiresAt !== null && (!Number.isFinite(Date.parse(expiresAt)) || Date.parse(expiresAt) <= Date.parse(createdAt))) return undefined;
  // Older stores omitted policy metadata: dated records used thirty minutes,
  // and permanent records had no expiry. Kind no longer determines retention.
  const retentionSeconds = wire.retention_seconds === undefined ? (expiresAt === null ? 0 : 1800) : wire.retention_seconds;
  if (!isClipboardRetentionSeconds(retentionSeconds) || (retentionSeconds === 0) !== (expiresAt === null)) return undefined;
  const preview = wire.preview === undefined ? "" : asString(wire.preview);
  if (preview === undefined) return undefined;
  return Object.freeze({
    id, kind, label, body, origin, revision, createdAt, updatedAt, expiresAt, retentionSeconds, preview,
    pinned: wire.pinned,
  });
}

function parseList(value: unknown): readonly SnippetRecord[] | undefined {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return undefined;
  const items = (value as Record<string, unknown>).items;
  if (!Array.isArray(items)) return undefined;
  const out: SnippetRecord[] = [];
  for (const item of items) {
    const record = parseRecord(item);
    if (record === undefined) return undefined;
    out.push(record);
  }
  return Object.freeze(out);
}

// --- The service -------------------------------------------------------------

export type SnippetSubscription = Readonly<{ dispose(): void }>;

/**
 * The surface a pane consumes. A page depends on this, never on the concrete
 * service, so a unit test can drive one without a document.
 */
export type SnippetServicePort = Readonly<{
  subscribe(listener: (snapshot: SnippetSnapshot) => void): SnippetSubscription;
  /** Marks a view as needing live data. Polling pauses in hidden documents. */
  retainLive(): SnippetSubscription;
  refresh(): Promise<void>;
  createSnippet(label: string, body: string): Promise<SnippetOutcome>;
  createClip(body: string, seconds?: ClipboardRetentionSeconds): Promise<SnippetOutcome>;
  updateText(record: SnippetRecord, body: string): Promise<SnippetOutcome>;
  setRetention(record: SnippetRecord, seconds: ClipboardRetentionSeconds): Promise<SnippetOutcome>;
  setPinned(record: SnippetRecord, pinned: boolean): Promise<SnippetOutcome>;
  remove(record: SnippetRecord): Promise<SnippetOutcome>;
  /** Device-local OSC 52 publication: latest value wins, coalesced. */
  publishOSC(body: string): void;
  /**
   * a trusted Copy that reached the system clipboard clears the
   * shared "tap Copy for this device" state — but only if it copied the exact
   * body still outstanding. Only an actual successful writeText may retire
   * it, never a render, and never a copy of some other value.
   */
  acknowledgeClipboard(body: string): void;
  snapshot(): SnippetSnapshot;
  stats(): SnippetStats;
}>;

export type SnippetStats = Readonly<{
  polls: number;
  requests: number;
  oscPuts: number;
  oscCoalesced: number;
  concurrentPolls: number;
  /** reads that started after a mutation response, to reconcile it. */
  reconcilingReads: number;
}>;

type Timer = ReturnType<typeof setTimeout>;

export type SnippetServiceOptions = Readonly<{
  document?: Document;
  fetch?: (input: string, init: RequestInit) => Promise<Response>;
  csrf?: () => string;
  clipboard?: Readonly<{ writeText(text: string): Promise<void> }> | undefined;
  origin?: string;
  pollIntervalMs?: number;
  oscQuiescenceMs?: number;
  setTimer?: (handler: () => void, ms: number) => Timer;
  clearTimer?: (timer: Timer) => void;
}>;

export class SnippetService implements SnippetServicePort {
  private readonly listeners = new Set<(snapshot: SnippetSnapshot) => void>();
  private current: SnippetSnapshot = EMPTY_SNAPSHOT;
  private live = 0;
  private pollTimer: Timer | undefined;
  private inFlight: Promise<void> | undefined;
  private pendingOSC: string | undefined;
  private oscTimer: Timer | undefined;
  private disposed = false;
  private readonly counters = { polls: 0, requests: 0, oscPuts: 0, oscCoalesced: 0, concurrentPolls: 0, reconcilingReads: 0 };
  /** the single owner of OSC publication. At most one PUT is ever live. */
  private oscInFlight = false;
  private pollsInFlight = 0;
  private readonly fetcher: (input: string, init: RequestInit) => Promise<Response>;
  private readonly csrf: () => string;
  private readonly clipboard: Readonly<{ writeText(text: string): Promise<void> }> | undefined;
  private readonly origin: string;
  private readonly pollIntervalMs: number;
  private readonly oscQuiescenceMs: number;
  private readonly setTimer: (handler: () => void, ms: number) => Timer;
  private readonly clearTimer: (timer: Timer) => void;
  private readonly visibilityDocument: Document | undefined;
  private readonly visibilityChanged = (): void => {
    this.cancelPoll();
    if (this.disposed || this.live === 0 || this.visibilityDocument?.visibilityState === "hidden") return;
    void this.refresh().finally(() => this.schedulePoll());
  };

  constructor(options: SnippetServiceOptions = {}) {
    this.fetcher = options.fetch ?? ((input, init) => window.fetch(input, init));
    this.csrf = options.csrf ?? csrfToken;
    this.clipboard = options.clipboard;
    this.origin = sanitizeSnippetOrigin(options.origin ?? "");
    this.pollIntervalMs = options.pollIntervalMs ?? SNIPPET_POLL_MS;
    this.oscQuiescenceMs = options.oscQuiescenceMs ?? OSC_QUIESCENCE_MS;
    this.setTimer = options.setTimer ?? ((handler, ms) => setTimeout(handler, ms));
    this.clearTimer = options.clearTimer ?? ((timer) => clearTimeout(timer));
    this.visibilityDocument = options.document ?? (typeof document === "undefined" ? undefined : document);
    this.visibilityDocument?.addEventListener("visibilitychange", this.visibilityChanged);
  }

  snapshot(): SnippetSnapshot {
    return this.current;
  }

  stats(): SnippetStats {
    return Object.freeze({ ...this.counters });
  }

  /**
   * Subscribes and delivers the CURRENT snapshot synchronously, so a pane
   * created after the service already has state renders that state before its
   * first presentation fit rather than a blank list that fills in later.
   */
  subscribe(listener: (snapshot: SnippetSnapshot) => void): SnippetSubscription {
    this.listeners.add(listener);
    listener(this.current);
    return Object.freeze({
      dispose: () => {
        this.listeners.delete(listener);
      },
    });
  }

  retainLive(): SnippetSubscription {
    this.live += 1;
    if (this.live === 1 && !this.disposed && this.visibilityDocument?.visibilityState !== "hidden") {
      void this.refresh();
      this.schedulePoll();
    }
    let released = false;
    return Object.freeze({
      dispose: () => {
        if (released) return;
        released = true;
        this.live -= 1;
        if (this.live === 0) this.cancelPoll();
      },
    });
  }

  /**
   * One poll, single-flight: a caller that arrives while a poll is running
   * joins it. Six panes with an open list therefore produce ONE request, and
   * a slow response admits no overlapping poll.
   */
  refresh(): Promise<void> {
    if (this.disposed) return Promise.resolve();
    const running = this.inFlight;
    if (running) return running;
    const poll = this.poll().finally(() => {
      this.inFlight = undefined;
    });
    this.inFlight = poll;
    return poll;
  }

  /**
   * a read that is guaranteed to have STARTED after this call, used to
   * reconcile a settled mutation.
   *
   * `refresh()` joins whatever GET is already running, which is right for a
   * poll and WRONG here: a GET that began before the mutation carries
   * pre-mutation authority, so joining it would drop the optimistic row and
   * report "saved" while still rendering the old list. So any in-flight read
   * is awaited and discarded first; only then is a fresh read taken.
   */
  private async reconcileAfterMutation(): Promise<void> {
    if (this.disposed) return;
    const stale = this.inFlight;
    // Its outcome is irrelevant — it is the wrong read by construction.
    if (stale) await stale.catch(() => undefined);
    if (this.disposed) return;
    this.counters.reconcilingReads += 1;
    await this.refresh().catch(() => undefined);
  }

  private async poll(): Promise<void> {
    this.pollsInFlight += 1;
    this.counters.concurrentPolls = Math.max(this.counters.concurrentPolls, this.pollsInFlight);
    if (this.current.status === "idle") this.publish({ status: "loading" });
    try {
      const response = await this.send("/api/snippets", "GET");
      this.counters.polls += 1;
      if (!response.ok) {
        this.publish({ status: "unavailable", snippets: [], clips: [], osc: undefined });
        return;
      }
      const parsed = parseList(await response.json().catch(() => undefined));
      if (parsed === undefined) {
        this.publish({ status: "unavailable", snippets: [], clips: [], osc: undefined });
        return;
      }
      const snippets = parsed.filter((record) => record.kind === "snippet");
      const clips = parsed.filter((record) => record.kind === "clip" && record.id !== OSC_SNIPPET_ID);
      const osc = parsed.find((record) => record.id === OSC_SNIPPET_ID);
      this.publish({
        status: "ready",
        snippets: Object.freeze(snippets),
        clips: Object.freeze(clips),
        osc,
        generation: this.current.generation + 1,
      });
    } catch {
      this.publish({ status: "unavailable", snippets: [], clips: [], osc: undefined });
    } finally {
      this.pollsInFlight -= 1;
    }
  }

  private schedulePoll(): void {
    this.cancelPoll();
    if (this.disposed || this.live === 0 || this.visibilityDocument?.visibilityState === "hidden") return;
    this.pollTimer = this.setTimer(() => {
      this.pollTimer = undefined;
      if (this.disposed || this.live === 0 || this.visibilityDocument?.visibilityState === "hidden") return;
      void this.refresh().finally(() => this.schedulePoll());
    }, this.pollIntervalMs);
  }

  private cancelPoll(): void {
    if (this.pollTimer !== undefined) {
      this.clearTimer(this.pollTimer);
      this.pollTimer = undefined;
    }
  }

  private publish(patch: Partial<SnippetSnapshot>): void {
    this.current = Object.freeze({ ...this.current, ...patch });
    for (const listener of Array.from(this.listeners)) listener(this.current);
  }

  // --- mutations -------------------------------------------------------------

  async createSnippet(label: string, body: string): Promise<SnippetOutcome> {
    return this.mutate("/api/snippets", "POST", { kind: "snippet", label, body, pinned: false, ...(this.origin ? { origin: this.origin } : {}) });
  }

  async createClip(body: string, seconds?: ClipboardRetentionSeconds): Promise<SnippetOutcome> {
    if (seconds !== undefined && !isClipboardRetentionSeconds(seconds)) return "refused";
    return this.mutate("/api/snippets", "POST", { kind: "clip", body, ...(seconds === undefined ? {} : { retention_seconds: seconds }), ...(this.origin ? { origin: this.origin } : {}) });
  }

  async updateText(record: SnippetRecord, body: string): Promise<SnippetOutcome> {
    if (!isStorableSnippetBody(body)) return new TextEncoder().encode(body).length > SNIPPET_BODY_MAX_BYTES ? "too_large" : "refused";
    const label = snippetLabelFromBody(body);
    // Whitespace remains valid content. Preserve a legacy snippet's label
    // when there is no visible line from which to derive a replacement.
    return this.mutate(`/api/snippets/${record.id}`, "PATCH", { body, ...(record.kind === "snippet" && label ? { label } : {}), revision: record.revision });
  }

  async setRetention(record: SnippetRecord, seconds: ClipboardRetentionSeconds): Promise<SnippetOutcome> {
    if (!isClipboardRetentionSeconds(seconds)) return "refused";
    return this.mutate(`/api/snippets/${record.id}`, "PATCH", { retention_seconds: seconds, revision: record.revision });
  }

  async setPinned(record: SnippetRecord, pinned: boolean): Promise<SnippetOutcome> {
    return this.mutate(`/api/snippets/${record.id}`, "PATCH", { pinned, revision: record.revision });
  }

  async remove(record: SnippetRecord): Promise<SnippetOutcome> {
    return this.mutate(`/api/snippets/${record.id}`, "DELETE", { revision: record.revision });
  }

  /**
   * The device-local OSC 52 coalescer. Only the LATEST pending value is kept,
   * and it is published once the bounded window since the first pending value
   * elapses — so a pane (or six, across two profiles) flooding OSC writes
   * spends at most one request per window and the store keeps exactly one
   * record, updated in place.
   */
  publishOSC(body: string): void {
    if (this.disposed || !isStorableSnippetBody(body)) return;
    if (this.pendingOSC !== undefined) this.counters.oscCoalesced += 1;
    this.pendingOSC = body;
    this.armOSC();
  }

  /**
   * publication has ONE owner. The window is armed only when no PUT is
   * live; while one is unresolved every later value collapses into
   * `pendingOSC`, and the newest survivor is published after that PUT settles.
   * Two PUTs are therefore never live together, so an older one can never
   * commit after a newer one and overwrite both the distinguished record and
   * the device clipboard.
   */
  private armOSC(): void {
    if (this.disposed || this.oscTimer !== undefined || this.oscInFlight) return;
    this.oscTimer = this.setTimer(() => {
      this.oscTimer = undefined;
      void this.drainOSC();
    }, this.oscQuiescenceMs);
  }

  private async drainOSC(): Promise<void> {
    if (this.disposed || this.oscInFlight) return;
    const value = this.pendingOSC;
    this.pendingOSC = undefined;
    if (value === undefined) return;
    this.oscInFlight = true;
    try {
      await this.flushOSC(value);
    } finally {
      this.oscInFlight = false;
    }
    // A newer value arrived while that PUT was unresolved: it is the only one
    // that survived, and it starts its own bounded window now.
    if (!this.disposed && this.pendingOSC !== undefined) this.armOSC();
  }

  private async flushOSC(body: string): Promise<void> {
    this.counters.oscPuts += 1;
    // No If-Match: the last committed write wins for the
    // distinguished record, and device-local coalescing is this client's job.
    let publicationRevision = 0;
    const outcome = await this.mutate("/api/snippets/osc52", "PUT", { body, ...(this.origin ? { origin: this.origin } : {}) }, (value) => {
      const record = parseRecord(value);
      if (record?.id === OSC_SNIPPET_ID && record.body === body) publicationRevision = record.revision;
    });
    if (outcome !== "ok") return;
    // The clip record is the delivery. The system clipboard is a bonus that
    // iOS refuses without a gesture: a refusal is a clips-only success, never
    // a terminal failure.
    // whichever way this ends, the outstanding delivery is named by
    // the body that was actually published — so a LATER publication replaces
    // the hand-off rather than adding a second one, and an earlier value can
    // never be mistaken for it.
    const handoff = Object.freeze({ body, revision: publicationRevision });
    if (!this.clipboard) {
      this.publish({ clipboardHandoff: handoff });
      return;
    }
    try {
      await this.clipboard.writeText(body);
      this.publish({ clipboardHandoff: undefined });
    } catch {
      this.publish({ clipboardHandoff: handoff });
    }
  }

  /**
   * retire the "tap Copy for this device" state, and ONLY for the
   * value it is about. Called after a real `writeText` resolved from a
   * trusted gesture, with the body that write actually put on the clipboard.
   * A manual clip, or an older OSC row copied after a newer failure, carries
   * a different body and therefore cannot clear the hand-off. (A copy whose
   * body happens to equal the outstanding one DOES clear it — the clipboard
   * then genuinely holds the owed value, whichever row produced it.)
   */
  acknowledgeClipboard(body: string): void {
    if (this.disposed) return;
    const outstanding = this.current.clipboardHandoff;
    if (outstanding !== undefined && outstanding.body === body) this.publish({ clipboardHandoff: undefined });
  }

  private async mutate(path: string, method: string, body: unknown, accepted?: (value: unknown) => void): Promise<SnippetOutcome> {
    let outcome: SnippetOutcome;
    try {
      const response = await this.send(path, method, body);
      outcome = classifyStatus(response.status);
      if (outcome === "ok" && accepted) accepted(await response.json().catch(() => undefined));
    } catch {
      // A transport failure is an outage like any other: it must fall through
      // to the publication below, not return past it (clipboard-store-recovery).
      outcome = "unreachable";
    }
    // the reconciling read is AWAITED, and it starts after this
    // response. The caller therefore learns the outcome only once the list it
    // will render is post-mutation authority (or the snapshot has gone
    // `unavailable`, which the sheet reports honestly).
    if (outcome === "ok" || outcome === "conflict" || outcome === "full") await this.reconcileAfterMutation();
    // a mutation is also a probe of the store. A 503 or a transport
    // failure means the store is down NOW, and nothing else would say so
    // until the next poll — so later copies would keep firing mutations at a
    // store already known dead, and the first partial success would be
    // reported as a plain refusal instead of "the local half worked".
    //
    // Unlike a failed READ, this does NOT wipe the lists: the last successful
    // poll is still the last authority we had, and its rows carry the LOCAL
    // copy actions that must survive the outage. The status is what changed.
    else if (outcome === "unavailable" || outcome === "unreachable") {
      if (!this.disposed && this.current.status !== "unavailable") this.publish({ status: "unavailable" });
    }
    return outcome;
  }

  /**
   * THE secured requester. Every /api/snippets request — read and write —
   * passes through here: same-origin credentials, `no-store`, the CSRF header
   * and the exact JSON content type on mutations, and nothing else. This is
   * the only fetch call site in the UI that names /api/snippets.
   */
  private send(path: string, method: string, body?: unknown): Promise<Response> {
    this.counters.requests += 1;
    const headers: Record<string, string> = {};
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      headers["X-Persea-CSRF"] = this.csrf();
    }
    return this.fetcher(path, {
      method,
      cache: "no-store",
      credentials: "same-origin",
      headers,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.cancelPoll();
    this.visibilityDocument?.removeEventListener("visibilitychange", this.visibilityChanged);
    if (this.oscTimer !== undefined) this.clearTimer(this.oscTimer);
    this.oscTimer = undefined;
    // A PUT already in flight cannot be recalled, but nothing further is ever
    // published: drainOSC and armOSC both refuse once disposed.
    this.pendingOSC = undefined;
    this.listeners.clear();
  }
}

function classifyStatus(status: number): SnippetOutcome {
  if (status >= 200 && status < 300) return "ok";
  if (status === 409 || status === 412) return "conflict";
  if (status === 507) return "full";
  if (status === 413) return "too_large";
  if (status === 503) return "unavailable";
  return "refused";
}

/**
 * The device label that rides along as sanitized presentation metadata on
 * clips and OSC writes. It names a device class, never an identity, and the
 * server never lets it select a record.
 */
export function deviceOrigin(win: Window): string {
  const coarse = typeof win.matchMedia === "function" && win.matchMedia("(pointer: coarse)").matches;
  return coarse ? "phone" : "desktop";
}
