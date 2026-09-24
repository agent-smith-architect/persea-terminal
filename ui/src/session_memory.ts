// Per-device "last session" memory and the landing resolution it feeds.
//
// This module owns ONE localStorage record and its strict codec.
// Everything here is presentation-level truth about a session
// the operator already attached to: the record is written only as the
// consequence of a real unified first COMMIT (or a
// successful switch COMMIT), never at click, connect, or PREPARE. It stores no
// URL, attachment handle, capability, token, draft body, image path, or secret
// — only identity metadata that the authoritative inventory has to confirm
// before anything is offered.
//
// Resolution is EXACT-INCARNATION. The pinned `draftScope` is the dashboard's
// authority key (`dashboard.ts` parseInventory -> authorityKey), which includes
// the boot id, server pid/start and session creation time, so a same-named
// successor of an ended session can never be matched. This module therefore
// consumes the shared identity resolver (`resolveDraftScope`) rather than
// re-implementing one; it imports only TYPES from `dashboard.ts` so there is no
// runtime import cycle.
import type { DashboardInventory, DashboardSession, DraftScopeResolution, UnifiedSessionProjection } from "./dashboard";

import { rememberCommittedSession } from "./session_discovery";
export const LAST_SESSION_STORAGE_KEY = "persea-terminal.last-session.v1";

// Bounds. The draft scope shares the composer storage scope's bound
// (`composer_storage_scope.ts` validateComposerStorageScope); labels share the
// fragment label bound (`app.ts` labelFromFragment). A stored value longer than
// what the product can ever produce is refused rather than truncated.
const MAX_SCOPE_LENGTH = 4_096;
const MAX_LABEL_LENGTH = 128;
// Clock skew tolerance for `at`. A record stamped further in the future than
// this is not a record this device wrote; it fails closed.
const MAX_CLOCK_SKEW_MS = 24 * 60 * 60 * 1_000;

const RECORD_KEYS = Object.freeze(["draftScope", "name", "realm", "server", "at"] as const);
const OPTIONAL_RECORD_KEYS = Object.freeze(["alias"] as const);

/**
 * The whole record. `alias` is present only when the session had one; nothing
 * else is ever stored.
 */
export type LastSessionRecord = Readonly<{
  draftScope: string;
  name: string;
  alias?: string;
  realm: string;
  server: string;
  at: number;
}>;

export type MemoryStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

// eslint-disable-next-line no-control-regex
const CONTROL_CHARACTERS = /[\u0000-\u001f\u007f]/;

/**
 * Own-property presence. `in` walks the prototype chain, so a document that has
 * polluted `Object.prototype` could satisfy a required field without the stored
 * record containing it — an inherited field, which acceptance B2 validate-stored-record names
 * explicitly. Every field this codec reads must be the record's own.
 */
function ownField(value: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key);
}

function boundedLabel(value: unknown, maximum = MAX_LABEL_LENGTH): string | undefined {
  if (typeof value !== "string" || value.length === 0 || value.length > maximum) return undefined;
  if (CONTROL_CHARACTERS.test(value)) return undefined;
  return value;
}

/**
 * The identity a draft scope pins. The scope IS the dashboard's authority key:
 * `JSON.stringify([realm, server, selector_kind, selector_value, boot_id,
 * session_id, uid, server_pid, server_start, session_created])`
 * (`dashboard.ts` authorityKey). Decoding it here lets a record carry the realm
 * and server for honest copy after the session has ended, when the inventory can
 * no longer answer. `session_memory.test.ts` pins this decode against a scope
 * produced by the real `parseInventory`, so a change to the authority key is a
 * RED test rather than a silent loss of Resume.
 */
export function sessionScopeIdentity(draftScope: string): Readonly<{ realm: string; server: string; sessionId: string }> | undefined {
  if (typeof draftScope !== "string" || draftScope.length === 0 || draftScope.length > MAX_SCOPE_LENGTH) return undefined;
  let parsed: unknown;
  try { parsed = JSON.parse(draftScope); } catch { return undefined; }
  if (!Array.isArray(parsed) || parsed.length !== 10) return undefined;
  const strings = parsed.slice(0, 6);
  const numbers = parsed.slice(6);
  for (const value of strings) if (typeof value !== "string" || value.length === 0 || value.length > MAX_SCOPE_LENGTH) return undefined;
  for (const value of numbers) if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) return undefined;
  const [realm, server, , , , sessionId] = strings as string[];
  if (boundedLabel(realm) === undefined || boundedLabel(server) === undefined) return undefined;
  return Object.freeze({ realm, server, sessionId });
}

function validAt(value: unknown, now: number): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0 && value <= now + MAX_CLOCK_SKEW_MS;
}

/**
 * The strict decoder. Every field is rebuilt one at a time from a validated
 * source value, so nothing a hostile document put in storage can survive into
 * the record — an unknown key is a refusal, not something silently carried. A
 * record whose `draftScope` does not decode, or whose realm/server disagree
 * with the scope it pins, is refused: those two fields are display copy for a
 * session the inventory can no longer describe, and they must not be free text.
 */
export function decodeLastSession(raw: string | null, now: number = Date.now()): LastSessionRecord | undefined {
  if (typeof raw !== "string" || raw.length === 0 || raw.length > MAX_SCOPE_LENGTH + 1_024) return undefined;
  let parsed: unknown;
  try { parsed = JSON.parse(raw); } catch { return undefined; }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return undefined;
  const value = parsed as Record<string, unknown>;
  for (const key of Object.keys(value)) {
    if (!(RECORD_KEYS as readonly string[]).includes(key) && !(OPTIONAL_RECORD_KEYS as readonly string[]).includes(key)) return undefined;
  }
  for (const key of RECORD_KEYS) if (!ownField(value, key)) return undefined;
  const draftScope = typeof value.draftScope === "string" && value.draftScope.length > 0 && value.draftScope.length <= MAX_SCOPE_LENGTH
    ? value.draftScope
    : undefined;
  if (draftScope === undefined) return undefined;
  const identity = sessionScopeIdentity(draftScope);
  if (identity === undefined) return undefined;
  const name = boundedLabel(value.name);
  const realm = boundedLabel(value.realm);
  const server = boundedLabel(value.server);
  if (name === undefined || realm === undefined || server === undefined) return undefined;
  if (realm !== identity.realm || server !== identity.server) return undefined;
  if (!validAt(value.at, now)) return undefined;
  const at = value.at;
  let alias: string | undefined;
  if (ownField(value, "alias")) {
    alias = boundedLabel(value.alias);
    if (alias === undefined) return undefined;
  }
  // Canonical field order, identical to encodeLastSession: a record is compared
  // and logged as text in the gates, so its shape must not depend on the path
  // that produced it.
  return Object.freeze({ draftScope, name, ...(alias === undefined ? {} : { alias }), realm, server, at });
}

/**
 * The exact serialization; only the known fields, in a fixed order. The
 * optional field is read as an own property for the same reason the decoder
 * does: an inherited `alias` is display copy the caller never supplied, and it
 * must not be written into storage.
 */
export function encodeLastSession(record: LastSessionRecord): string {
  const alias = ownField(record, "alias") ? record.alias : undefined;
  return JSON.stringify({
    draftScope: record.draftScope,
    name: record.name,
    ...(alias === undefined ? {} : { alias }),
    realm: record.realm,
    server: record.server,
    at: record.at,
  });
}

/**
 * Reading never throws. A browser that refuses storage entirely (private mode,
 * blocked site data, a partitioned standalone launch) is indistinguishable from
 * "no memory yet", which is exactly the honest outcome: the default/list.
 */
export function readLastSession(storage: MemoryStorage, now: number = Date.now()): LastSessionRecord | undefined {
  let raw: string | null;
  try { raw = storage.getItem(LAST_SESSION_STORAGE_KEY); } catch { return undefined; }
  return decodeLastSession(raw, now);
}

export type LastSessionWrite = "written" | "invalid" | "unavailable";

/**
 * Writing never throws either: a quota or security exception means the device
 * simply will not remember, and the product says nothing about it because the
 * landing degrades to the ordinary list.
 */
export function writeLastSession(storage: MemoryStorage, record: LastSessionRecord, now: number = Date.now()): LastSessionWrite {
  const encoded = encodeLastSession(record);
  // Round-tripping through the decoder is the single validation gate: nothing
  // reaches storage that the reader would refuse.
  if (decodeLastSession(encoded, now) === undefined) return "invalid";
  try { storage.setItem(LAST_SESSION_STORAGE_KEY, encoded); } catch { return "unavailable"; }
  return "written";
}

export function clearLastSession(storage: MemoryStorage): void {
  try { storage.removeItem(LAST_SESSION_STORAGE_KEY); } catch { /* nothing to clear if storage refuses */ }
}

/**
 * The identity of a session an operator's TRUSTED action targeted, established
 * from the authoritative inventory projection that owns that action, and
 * carried across the one navigation it causes.
 *
 * This is deliberately NOT taken from the terminal URL. The fragment is
 * client-supplied: a COMMIT proves that *a* handle attached, never that the
 * fragment's `draft_scope` names that attachment, so a URL carrying a valid
 * handle beside a different live incarnation's scope would otherwise record the
 * wrong session as "last session".
 *
 * It is bounded and carries NO capability: no handle, no URL, no source token,
 * no secret — the same fields the durable record holds, minus the timestamp.
 */
export type PendingSessionIdentity = Readonly<{
  draftScope: string;
  name: string;
  alias?: string;
  realm: string;
  server: string;
}>;

// A candidate belongs to ONE target operation. A single origin-global slot
// would be ambient: every dashboard action writes it and every terminal page
// destructively reads it, so two supported popup/tab actions overwrite or
// consume each other's candidate and a page can consume an identity that was
// never staged for it. Each candidate therefore lives
// under its own operation key and is consumed only by the matching target.
export const PENDING_SESSION_KEY_PREFIX = "persea-terminal.pending-session.v1/";

// A candidate is consumed by the navigation it was staged for. This bound only
// limits how long an abandoned one can linger; correctness does not rest on it,
// because a candidate is single-use, operation-keyed, and must also agree with
// the page that offers it.
const MAX_PENDING_AGE_MS = 60_000;
// How many concurrent staged operations one device keeps. Well past the number
// of tabs an operator opens at once; the oldest is evicted beyond it, so an
// abandoned candidate can never accumulate.
const MAX_PENDING_OPERATIONS = 8;
const OPERATION_ID_PATTERN = /^[0-9a-f]{32}$/;

/**
 * Storage that can also be enumerated, so staging can prune expired siblings.
 * Enumeration is optional: without it, staging still works and stale entries
 * simply expire on read.
 */
export type PendingStorage = MemoryStorage & Partial<Pick<Storage, "length" | "key">>;

/**
 * A fresh name for one navigation. NOT an authority and not derived from one:
 * it grants nothing, it names a local storage entry. It exists so the target of
 * a navigation can claim the candidate staged for IT and no other.
 */
export function newOperationId(source: Pick<Crypto, "getRandomValues"> | undefined = globalThis.crypto): string | undefined {
  if (source === undefined || typeof source.getRandomValues !== "function") return undefined;
  const bytes = source.getRandomValues(new Uint8Array(16));
  let id = "";
  for (const byte of bytes) id += byte.toString(16).padStart(2, "0");
  return id;
}

export function isOperationId(value: unknown): value is string {
  return typeof value === "string" && OPERATION_ID_PATTERN.test(value);
}

/**
 * Builds the pending identity from an authoritative inventory session. The
 * caller must pass a session it read from `parseInventory`, never one it
 * assembled from a URL. Labels follow `unifiedTerminalURL`, so the record names
 * the session exactly as the action that opened it did.
 */
export function pendingIdentityFromSession(session: Readonly<{
  draftScope: string; name: string; realm: string; server: string;
  aliases: readonly Readonly<{ displayAlias: string }>[];
}>): PendingSessionIdentity | undefined {
  const identity = sessionScopeIdentity(session.draftScope);
  if (identity === undefined) return undefined;
  if (session.realm !== identity.realm || session.server !== identity.server) return undefined;
  const name = boundedLabel(session.name) ?? boundedLabel(identity.sessionId);
  if (name === undefined) return undefined;
  const alias = session.aliases.length > 0 ? boundedLabel(session.aliases[0].displayAlias) : undefined;
  return Object.freeze({
    draftScope: session.draftScope,
    name,
    ...(alias === undefined ? {} : { alias }),
    realm: identity.realm,
    server: identity.server,
  });
}

function pendingKey(operationId: string): string {
  return PENDING_SESSION_KEY_PREFIX + operationId;
}

function encodePending(identity: PendingSessionIdentity, now: number): string {
  return JSON.stringify({
    draftScope: identity.draftScope,
    name: identity.name,
    ...(ownField(identity, "alias") && identity.alias !== undefined ? { alias: identity.alias } : {}),
    realm: identity.realm,
    server: identity.server,
    at: now,
  });
}

function decodePending(raw: string | null, now: number): Readonly<{ identity: PendingSessionIdentity; at: number }> | undefined {
  // The candidate is read back through the SAME strict codec as the durable
  // record: it is written by this origin, but it is still storage, and storage
  // is never trusted here.
  const record = decodeLastSession(raw, now);
  if (record === undefined) return undefined;
  if (now - record.at > MAX_PENDING_AGE_MS) return undefined;
  const { at, ...identity } = record;
  return Object.freeze({ identity: Object.freeze(identity) as PendingSessionIdentity, at });
}

function pendingKeys(storage: PendingStorage): string[] {
  const keys: string[] = [];
  const length = storage.length;
  if (typeof length !== "number" || typeof storage.key !== "function") return keys;
  for (let index = 0; index < length; index += 1) {
    const key = storage.key(index);
    if (typeof key === "string" && key.startsWith(PENDING_SESSION_KEY_PREFIX)) keys.push(key);
  }
  return keys;
}

// Expired and surplus candidates are evicted when a new one is staged. Only
// OTHER operations are touched, and only ones that can no longer be claimed.
function prunePending(storage: PendingStorage, keep: string, now: number): void {
  const dated: Readonly<{ key: string; at: number }>[] = [];
  for (const key of pendingKeys(storage)) {
    if (key === keep) continue;
    let raw: string | null;
    try { raw = storage.getItem(key); } catch { continue; }
    const entry = decodePending(raw, now);
    if (entry === undefined) { try { storage.removeItem(key); } catch { /* best effort */ } continue; }
    dated.push({ key, at: entry.at });
  }
  dated.sort((left, right) => left.at - right.at);
  for (const entry of dated.slice(0, Math.max(0, dated.length - (MAX_PENDING_OPERATIONS - 1)))) {
    try { storage.removeItem(entry.key); } catch { /* best effort */ }
  }
}

/**
 * Stage the candidate for ONE operation, immediately before the navigation that
 * operation causes. Sibling operations are untouched.
 */
export function stagePendingSession(storage: PendingStorage, operationId: string, identity: PendingSessionIdentity, now: number = Date.now()): "staged" | "invalid" | "unavailable" {
  if (!isOperationId(operationId)) return "invalid";
  const encoded = encodePending(identity, now);
  if (decodePending(encoded, now) === undefined) return "invalid";
  try { storage.setItem(pendingKey(operationId), encoded); } catch { return "unavailable"; }
  prunePending(storage, pendingKey(operationId), now);
  return "staged";
}

/**
 * Claim the candidate staged for THIS operation and remove it in the same
 * breath. Single use, and only the matching target can claim it: a reload, a
 * sibling tab, or a replayed URL finds nothing.
 */
export function takePendingSession(storage: MemoryStorage, operationId: string | null | undefined, now: number = Date.now()): PendingSessionIdentity | undefined {
  if (!isOperationId(operationId)) return undefined;
  let raw: string | null;
  try { raw = storage.getItem(pendingKey(operationId)); } catch { return undefined; }
  try { storage.removeItem(pendingKey(operationId)); } catch { /* consumed in memory regardless */ }
  return decodePending(raw, now)?.identity;
}

/** Drop one operation's candidate without reading it into an identity. */
export function dropPendingSession(storage: MemoryStorage, operationId: string | null | undefined): void {
  if (!isOperationId(operationId)) return;
  try { storage.removeItem(pendingKey(operationId)); } catch { /* nothing to clear if storage refuses */ }
}

export type CommittedSessionRecorder = Readonly<{
  /**
   * Navigation mode only: bind this recorder to the generation of the one
   * controller operation it belongs to. It cannot record until it is armed,
   * and it is armed exactly once — a second arm with a different generation is
   * a different operation and disarms it. In resolved mode the operation is
   * named at construction and this is inert.
   */
  arm(generation: number): void;
  /** The controller's observing sink, verbatim. */
  frame(generation: number, frame: Readonly<{ type: string }>, verdict: "ENQUEUED" | "STALE" | "CLOSED"): void;
  openTransport(generation: number): void;
  transportClosed(generation: number, reason: string): void;
  operationalRefusal(generation: number, code: string): void;
  identityResolve(outcome: "minted" | "session_gone" | "identity_ambiguous" | "identity_invalid"): void;
  /** Navigation abandonment or controller disposal. */
  abandon(): void;
  /** Test/observability only: whether the durable record was written. */
  recorded(): boolean;
}>;

/**
 * How this recorder got its identity. The two shapes are DISCRIMINATED, never
 * inferred from which optional fields happen to be present, because the two
 * have opposite obligations:
 *
 * - `navigation`: the identity crossed a navigation as a staged candidate, so
 *   the page MUST corroborate it against its own URL scope. `pageDraftScope`
 *   is required to be passed — a page with no scope, or one that disagrees,
 *   drops the candidate rather than consuming it. The operation is named later,
 *   by `arm`, from the page's own controller.
 * - `resolved`: the caller already resolved the identity in-process (session switch's
 *   pane switch) and names the operation up front. There is no navigation and
 *   therefore NO URL scope to check — requiring one would make the seam inert.
 *   `generation` is required for exactly that reason: the operation must be
 *   named by something, and here it is named by the caller.
 */
export type CommittedSessionSource =
  | Readonly<{
    mode: "navigation";
    identity: PendingSessionIdentity | undefined;
    /** The page's own incarnation, from its URL. Required, may be absent. */
    pageDraftScope: string | null | undefined;
  }>
  | Readonly<{
    mode: "resolved";
    identity: PendingSessionIdentity | undefined;
    /** The controller generation this identity belongs to. Required. */
    generation: number;
  }>;

/**
 * THE post-COMMIT recorder — the one primitive that may write the durable
 * record. It can only ever write the identity it was CONSTRUCTED with, on the
 * ONE controller operation that identity belongs to.
 *
 * In navigation mode it never latches a generation on its own: an unarmed
 * recorder ignores every callback, so an unrelated controller's event — an
 * outgoing pane during an session switch switch, a sibling reconnect — can never bind it.
 * In resolved mode the generation is given at construction and is never
 * inferred. Either way, events from another generation are ignored, never
 * consumed: a sibling operation's failure is not this operation's failure.
 *
 * Identity resolution is settled per operation. A
 * resolved-mode recorder ignores `identityResolve` outright — its identity is
 * already resolved, and the callback carries no operation, so honouring it
 * would let any pane's failure clear this one. A navigation-mode recorder
 * honours a non-minted result only BEFORE it is armed, which is the window in
 * which the only resolution in flight is its own page's; once armed, failure
 * belongs to its generation-scoped transport and refusal callbacks.
 */
export function createCommittedSessionRecorder(options: Readonly<{
  storage: MemoryStorage;
  now?: () => number;
}> & CommittedSessionSource): CommittedSessionRecorder {
  const now = options.now ?? Date.now;
  const navigation = options.mode === "navigation";
  let candidate: PendingSessionIdentity | undefined = options.identity;
  // Navigation only: a missing or disagreeing page scope drops the candidate
  // outright. Resolved input has no URL to disagree with and is not checked.
  if (candidate !== undefined && options.mode === "navigation"
    && (typeof options.pageDraftScope !== "string" || options.pageDraftScope !== candidate.draftScope)) {
    candidate = undefined;
  }
  let armed: number | undefined = options.mode === "resolved" ? options.generation : undefined;
  let written = false;
  const drop = (): void => { candidate = undefined; };
  const mine = (value: number): boolean => candidate !== undefined && armed !== undefined && armed === value;
  return Object.freeze({
    arm(generation) {
      if (!navigation) return;
      if (armed === undefined) { armed = generation; return; }
      if (armed !== generation) drop();
    },
    frame(value, frame, verdict) {
      if (!mine(value)) return;
      if (verdict !== "ENQUEUED") { drop(); return; }
      if (frame.type !== "COMMIT") return;
      const identity = candidate;
      if (identity === undefined) return;
      drop();
      const at = now();
      written = writeLastSession(options.storage, Object.freeze({
        draftScope: identity.draftScope,
        name: identity.name,
        ...(ownField(identity, "alias") && identity.alias !== undefined ? { alias: identity.alias } : {}),
        realm: identity.realm,
        server: identity.server,
        at,
      }), at) === "written";
      if (written) rememberCommittedSession(options.storage, identity.draftScope, at);
    },
    openTransport() { /* arming is explicit; a transport open never binds */ },
    transportClosed(value) { if (mine(value)) drop(); },
    operationalRefusal(value) { if (mine(value)) drop(); },
    identityResolve(outcome) {
      // Unowned by construction: this callback names no operation. Only the
      // one state in which it can only be this operation's own — a navigation
      // recorder that has not yet been armed — may act on it.
      if (!navigation || armed !== undefined) return;
      if (outcome !== "minted") drop();
    },
    abandon() { drop(); },
    recorded() { return written; },
  });
}

// --- Landing resolution ------------------------------------------------------

export type UnifiedOpenState = "open" | "adoptable";
export type UnifiedBlockedState = Exclude<UnifiedSessionProjection, { state: UnifiedOpenState }>["state"];

/**
 * What the remembered identity resolves to against the CURRENT authoritative
 * inventory. `resolution` is the output of the shared `resolveDraftScope`; this
 * function adds no lookup of its own, so the landing spends no request the
 * dashboard has not already spent.
 */
export type LandingMemoryState =
  | Readonly<{ kind: "none" }>
  | Readonly<{ kind: "resume"; record: LastSessionRecord; session: DashboardSession; state: UnifiedOpenState }>
  | Readonly<{ kind: "blocked"; record: LastSessionRecord; session: DashboardSession; blocked: UnifiedBlockedState }>
  | Readonly<{ kind: "ended"; record: LastSessionRecord }>
  | Readonly<{ kind: "ambiguous"; record: LastSessionRecord }>;

function openState(session: DashboardSession): UnifiedOpenState | undefined {
  const state = session.unified?.state;
  return state === "open" || state === "adoptable" ? state : undefined;
}

function blockedState(session: DashboardSession): UnifiedBlockedState {
  const state = session.unified?.state;
  // A session with no unified projection at all is not a unified target; the
  // honest word for that is the projection's own "unavailable".
  return state === undefined || state === "open" || state === "adoptable" ? "unavailable" : state;
}

export function landingMemoryState(record: LastSessionRecord | undefined, resolution: DraftScopeResolution | undefined): LandingMemoryState {
  if (record === undefined || resolution === undefined) return Object.freeze({ kind: "none" });
  if (resolution.kind === "missing") return Object.freeze({ kind: "ended", record });
  if (resolution.kind === "ambiguous") return Object.freeze({ kind: "ambiguous", record });
  const session = resolution.session;
  const state = openState(session);
  if (state === undefined) return Object.freeze({ kind: "blocked", record, session, blocked: blockedState(session) });
  return Object.freeze({ kind: "resume", record, session, state });
}

// --- Server default session --------------------------------------------------

export type DefaultSessionPreference = Readonly<{ realm: string; server: string; name: string }>;

/**
 * Reads `default_session` out of a `GET /api/preferences` body. The preference
 * is a SUGGESTED authoritative target chosen by the operator, so it is matched
 * by (realm, server, name) — that is what the operator wrote down. It never
 * creates and never attaches by itself, and it never stands in for a remembered
 * incarnation.
 */
export function parseDefaultSessionPreference(value: unknown): DefaultSessionPreference | undefined {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return undefined;
  const body = value as Record<string, unknown>;
  const candidate = body.default_session;
  if (candidate === null || candidate === undefined) return undefined;
  if (typeof candidate !== "object" || Array.isArray(candidate)) return undefined;
  const fields = candidate as Record<string, unknown>;
  // Closed schema: the front door's DefaultSession is exactly this triple
  // (`internal/frontdoor/preferences_store.go`). An unknown member means the
  // page and the store disagree about what a default is, which is a refusal.
  for (const key of Object.keys(fields)) if (key !== "realm" && key !== "server" && key !== "name") return undefined;
  // Own properties only, for the same reason the record codec insists on them.
  for (const key of ["realm", "server", "name"]) if (!ownField(fields, key)) return undefined;
  const realm = boundedLabel(fields.realm);
  const server = boundedLabel(fields.server);
  const name = boundedLabel(fields.name);
  if (realm === undefined || server === undefined || name === undefined) return undefined;
  return Object.freeze({ realm, server, name });
}

export type DefaultSessionState =
  | Readonly<{ kind: "none" }>
  | Readonly<{ kind: "open"; preference: DefaultSessionPreference; session: DashboardSession; state: UnifiedOpenState }>
  | Readonly<{ kind: "blocked"; preference: DefaultSessionPreference; session: DashboardSession; blocked: UnifiedBlockedState }>
  | Readonly<{ kind: "missing"; preference: DefaultSessionPreference }>
  | Readonly<{ kind: "ambiguous"; preference: DefaultSessionPreference }>;

export function defaultSessionState(inventory: DashboardInventory | undefined, preference: DefaultSessionPreference | undefined): DefaultSessionState {
  if (inventory === undefined || preference === undefined) return Object.freeze({ kind: "none" });
  const matches: DashboardSession[] = [];
  for (const realm of inventory.realms) {
    for (const server of realm.servers) {
      for (const session of server.sessions) {
        if (session.realm === preference.realm && session.server === preference.server && session.name === preference.name) matches.push(session);
      }
    }
  }
  if (matches.length === 0) return Object.freeze({ kind: "missing", preference });
  if (matches.length !== 1) return Object.freeze({ kind: "ambiguous", preference });
  const session = matches[0];
  const state = openState(session);
  if (state === undefined) return Object.freeze({ kind: "blocked", preference, session, blocked: blockedState(session) });
  return Object.freeze({ kind: "open", preference, session, state });
}

/**
 * Precedence: a remembered identity
 * that still resolves ALWAYS wins, and the default card is not rendered beside
 * it. Only when there is no valid remembered identity — none stored, ended,
 * ambiguous, or blocked — may the default be offered, and then it is labelled
 * as the default, never as a resume. Nothing here ever falls back from a
 * missing remembered session into a same-name default silently.
 */
export function defaultSessionIsOffered(memory: LandingMemoryState): boolean {
  return memory.kind !== "resume";
}
