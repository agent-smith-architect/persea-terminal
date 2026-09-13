// E-P6 — the last-session codec, its landing resolution, and the default
// preference. Falsifiers EP6-F1 (memory content), EP6-F2 (exact incarnation),
// EP6-F5 (default containment), EP6-F6 (hostile local storage) at unit level.
//
// The scope decode is pinned against a draft scope produced by the REAL
// `parseInventory`, so this suite fails if the dashboard's authority key ever
// changes shape rather than silently losing Resume.
declare const process: { stdout: { write(value: string): void }; exit(code: number): void };
import { parseInventory, resolveDraftScope, type DashboardInventory } from "../src/dashboard";
import {
  LAST_SESSION_STORAGE_KEY,
  clearLastSession,
  decodeLastSession,
  defaultSessionIsOffered,
  defaultSessionState,
  encodeLastSession,
  landingMemoryState,
  parseDefaultSessionPreference,
  readLastSession,
  createCommittedSessionRecorder,
  pendingIdentityFromSession,
  stagePendingSession,
  takePendingSession,
  PENDING_SESSION_KEY_PREFIX,
  dropPendingSession,
  isOperationId,
  newOperationId,
  sessionScopeIdentity,
  writeLastSession,
  type LastSessionRecord,
  type MemoryStorage,
} from "../src/session_memory";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  deepEqual(actual: unknown, expected: unknown, message = "values differ"): void { if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
  undefinedValue(value: unknown, message = "expected undefined"): void { if (value !== undefined) throw new Error(`${message}: ${JSON.stringify(value)}`); },
};

const NOW = 1_700_000_500_000;

function authority(realm: string, server: string, id: string, created: number): Record<string, unknown> {
  return { realm, server, uid: 1000, selector_kind: "socket_path", selector_value: `/tmp/${server}.sock`, boot_id: `boot-${realm}`, server_pid: 42, server_start: 100, session_id: id, session_created: created };
}

function session(realm: string, server: string, name: string, id: string, created: number, unified?: Record<string, unknown>): Record<string, unknown> {
  return {
    handles: { alias: `${id}-alias`, observe: `${id}-observe`, control: `${id}-control` },
    realm, server, server_status: "ok", session_id: id, name, width: 120, height: 40, attached: 1, activity: 1_700_000_000,
    authority: authority(realm, server, id, created),
    ...(unified ? { unified } : {}),
  };
}

function inventoryOf(...sessions: Record<string, unknown>[]): DashboardInventory {
  return parseInventory({ realms: [{ name: "local", display_name: "Local", servers: [{ label: "private", status: "ok", can_create: false, sessions }] }], aliases: [] });
}

class FakeStorage implements MemoryStorage {
  readonly entries = new Map<string, string>();
  readFailure?: Error;
  writeFailure?: Error;
  getItem(key: string): string | null { if (this.readFailure) throw this.readFailure; return this.entries.get(key) ?? null; }
  setItem(key: string, value: string): void { if (this.writeFailure) throw this.writeFailure; this.entries.set(key, value); }
  removeItem(key: string): void { this.entries.delete(key); }
  get length(): number { return this.entries.size; }
  key(index: number): string | null { return [...this.entries.keys()][index] ?? null; }
}

// --- The scope decode is the dashboard's own authority key -------------------
{
  const live = inventoryOf(session("local", "private", "ops", "$7", 200, { state: "open", origin: "birth" }));
  const scope = live.realms[0].servers[0].sessions[0].draftScope;
  const identity = sessionScopeIdentity(scope);
  assert.deepEqual(identity, { realm: "local", server: "private", sessionId: "$7" }, "scope decode must agree with the inventory authority key");

  for (const bad of ["", "not json", "{}", "[]", JSON.stringify(["local"]), JSON.stringify(["local", "private", "socket_path", "/tmp/x", "boot", "$1", 1000, 42, 100]), JSON.stringify(["", "private", "socket_path", "/tmp/x", "boot", "$1", 1000, 42, 100, 200]), JSON.stringify(["local", "private", "socket_path", "/tmp/x", "boot", "$1", 1000, 42, 100, -1]), JSON.stringify(["local", "private", "socket_path", "/tmp/x", "boot", "$1", 1000, 42, 100, 1.5]), JSON.stringify(["local", "private", "socket_path", "/tmp/x", "boot", 7, 1000, 42, 100, 200])]) {
    assert.undefinedValue(sessionScopeIdentity(bad), `malformed scope accepted: ${bad}`);
  }
}

// --- Codec: exactly the known fields, nothing else ---------------------------
const liveInventory = inventoryOf(
  session("local", "private", "ops", "$7", 200, { state: "open", origin: "birth" }),
  session("local", "private", "build", "$9", 300, { state: "adoptable" }),
  session("local", "private", "locked", "$11", 400, { state: "blocked_alt_screen" }),
  session("local", "private", "legacy", "$13", 500),
);
const opsSession = liveInventory.realms[0].servers[0].sessions[0];
const buildSession = liveInventory.realms[0].servers[0].sessions[1];
const lockedSession = liveInventory.realms[0].servers[0].sessions[2];
const legacySession = liveInventory.realms[0].servers[0].sessions[3];
const opsScope = opsSession.draftScope;

const goodRecord: LastSessionRecord = Object.freeze({ draftScope: opsScope, name: "ops", alias: "Primary", realm: "local", server: "private", at: NOW - 5_000 });

{
  const encoded = encodeLastSession(goodRecord);
  assert.deepEqual(Object.keys(JSON.parse(encoded)).sort(), ["alias", "at", "draftScope", "name", "realm", "server"], "record must carry exactly the six known fields");
  assert.deepEqual(decodeLastSession(encoded, NOW), goodRecord, "round trip must be lossless");
  assert.ok(!encoded.includes(opsSession.handles.control), "record must never carry an attachment handle");
  assert.ok(!encoded.includes("/terminal") && !encoded.includes("handle="), "record must never carry a URL");

  const withoutAlias = encodeLastSession(Object.freeze({ draftScope: opsScope, name: "ops", realm: "local", server: "private", at: NOW }));
  assert.deepEqual(Object.keys(JSON.parse(withoutAlias)).sort(), ["at", "draftScope", "name", "realm", "server"], "absent alias must not be serialized");
}

// --- EP6-F6: hostile local storage fails closed ------------------------------
{
  const base = JSON.parse(encodeLastSession(goodRecord)) as Record<string, unknown>;
  const hostile: [string, string][] = [
    ["malformed json", "{"],
    ["empty", ""],
    ["array", "[]"],
    ["null", "null"],
    ["string", "\"resume\""],
    ["unknown key", JSON.stringify({ ...base, handle: "cccccccccccccccccccccccccccccccccccccccccc0" })],
    ["prototype key", `{"draftScope":${JSON.stringify(opsScope)},"name":"ops","realm":"local","server":"private","at":${NOW},"__proto__":{"polluted":true}}`],
    ["missing draftScope", JSON.stringify({ name: "ops", realm: "local", server: "private", at: NOW })],
    ["missing name", JSON.stringify({ draftScope: opsScope, realm: "local", server: "private", at: NOW })],
    ["numeric name", JSON.stringify({ ...base, name: 7 })],
    ["empty name", JSON.stringify({ ...base, name: "" })],
    ["control character name", JSON.stringify({ ...base, name: "ops\u0007" })],
    ["overlong name", JSON.stringify({ ...base, name: "x".repeat(129) })],
    ["overlong scope", JSON.stringify({ ...base, draftScope: "x".repeat(5_000) })],
    ["scope is not an authority key", JSON.stringify({ ...base, draftScope: "cccccccccccccccccccccccccccccccccccccccccc0" })],
    ["realm disagrees with scope", JSON.stringify({ ...base, realm: "elsewhere" })],
    ["server disagrees with scope", JSON.stringify({ ...base, server: "elsewhere" })],
    ["zero at", JSON.stringify({ ...base, at: 0 })],
    ["negative at", JSON.stringify({ ...base, at: -1 })],
    ["fractional at", JSON.stringify({ ...base, at: 1.5 })],
    ["nonfinite at", `{"draftScope":${JSON.stringify(opsScope)},"name":"ops","realm":"local","server":"private","at":1e999}`],
    ["string at", JSON.stringify({ ...base, at: String(NOW) })],
    ["far future at", JSON.stringify({ ...base, at: NOW + 48 * 60 * 60 * 1_000 })],
    ["control character alias", JSON.stringify({ ...base, alias: "a\u001bb" })],
    ["null alias", JSON.stringify({ ...base, alias: null })],
  ];
  for (const [label, raw] of hostile) assert.undefinedValue(decodeLastSession(raw, NOW), `hostile record accepted (${label})`);

  // Inherited fields (EP6-F6, named explicitly by the acceptance). A polluted
  // `Object.prototype` must not be able to supply a field the stored record
  // does not itself contain: `in` would accept it, own-property presence does
  // not. The prototype is restored immediately whatever the assertions do.
  const polluted = Object.prototype as unknown as Record<string, unknown>;
  const inherited: [string, string, unknown][] = [
    ["inherited at", JSON.stringify({ draftScope: opsScope, name: "ops", realm: "local", server: "private" }), NOW],
    ["inherited name", JSON.stringify({ draftScope: opsScope, realm: "local", server: "private", at: NOW }), "ops"],
    ["inherited realm", JSON.stringify({ draftScope: opsScope, name: "ops", server: "private", at: NOW }), "local"],
    ["inherited server", JSON.stringify({ draftScope: opsScope, name: "ops", realm: "local", at: NOW }), "private"],
    ["inherited draftScope", JSON.stringify({ name: "ops", realm: "local", server: "private", at: NOW }), opsScope],
  ];
  for (const [label, raw, value] of inherited) {
    const key = label.slice("inherited ".length);
    let outcome: unknown;
    try {
      polluted[key] = value;
      outcome = decodeLastSession(raw, NOW);
    } finally {
      delete polluted[key];
    }
    assert.undefinedValue(outcome, `inherited field accepted (${label})`);
  }
  // An inherited OPTIONAL field must not be read either: the alias would be
  // display copy the record never carried.
  {
    let outcome: ReturnType<typeof decodeLastSession>;
    try {
      polluted.alias = "borrowed";
      outcome = decodeLastSession(encodeLastSession(Object.freeze({ draftScope: opsScope, name: "ops", realm: "local", server: "private", at: NOW })), NOW);
    } finally {
      delete polluted.alias;
    }
    assert.ok(outcome, "a record without an alias must still decode under a polluted prototype");
    assert.undefinedValue(outcome?.alias, "an inherited alias must not be adopted");
  }

  // A capability-shaped LABEL is not a capability: it is refused as a target by
  // resolution, and it is never reflected into a URL because the URL always
  // comes from the resolved inventory session.
  const capabilityShaped = JSON.stringify({ ...base, name: "cccccccccccccccccccccccccccccccccccccccccc0" });
  const decoded = decodeLastSession(capabilityShaped, NOW);
  assert.ok(decoded, "a bounded label must decode even when it looks like a token");
  assert.equal(decoded?.draftScope, opsScope, "the pinned scope, not the label, is what resolves");

  // Storage that throws on read (private mode, blocked site data, a
  // partitioned standalone launch) is "no memory", never an error surface.
  const unreadable = new FakeStorage();
  unreadable.readFailure = new Error("SecurityError");
  assert.undefinedValue(readLastSession(unreadable, NOW), "unreadable storage must read as no memory");

  const readable = new FakeStorage();
  readable.entries.set(LAST_SESSION_STORAGE_KEY, encodeLastSession(goodRecord));
  assert.deepEqual(readLastSession(readable, NOW), goodRecord, "a valid record must round trip through storage");
  clearLastSession(readable);
  assert.equal(readable.entries.has(LAST_SESSION_STORAGE_KEY), false, "clear must remove the record");
}

// --- Writing: validated, bounded, and never throwing -------------------------
{
  const quota = new FakeStorage();
  quota.writeFailure = new Error("QuotaExceededError");
  assert.equal(writeLastSession(quota, goodRecord, NOW), "unavailable", "a quota failure must be reported, not thrown");

  const store = new FakeStorage();
  assert.equal(writeLastSession(store, Object.freeze({ draftScope: "not-an-authority-key", name: "ops", realm: "local", server: "private", at: NOW }), NOW), "invalid", "an unpinnable scope must not be stored");
  assert.equal(store.entries.size, 0, "a refused write must leave storage untouched");

  // EP6-F1 / adjudication EP6-R1. The recorder can only ever write the identity
  // it was CONSTRUCTED with, and that identity comes from the authoritative
  // inventory session a trusted action resolved -- never from the URL.
  const opsSession = liveInventory.realms[0].servers[0].sessions.find((session) => session.name === "ops");
  assert.ok(opsSession);
  const opsIdentity = pendingIdentityFromSession(opsSession!);
  assert.deepEqual(opsIdentity, { draftScope: opsScope, name: "ops", realm: "local", server: "private" });

  const COMMIT = Object.freeze({ type: "COMMIT" });
  type Recorder = ReturnType<typeof createCommittedSessionRecorder>;
  const record = (options: { identity?: typeof opsIdentity; expected?: string | null; beforeArm?: (r: Recorder) => void; drive?: (r: Recorder) => void }) => {
    const store = new FakeStorage();
    const recorder = createCommittedSessionRecorder({
      mode: "navigation",
      storage: store,
      identity: "identity" in options ? options.identity : opsIdentity,
      pageDraftScope: "expected" in options ? options.expected : undefined,
      now: () => NOW,
    });
    options.beforeArm?.(recorder);
    recorder.arm(1);
    (options.drive ?? ((r) => r.frame(1, COMMIT, "ENQUEUED")))(recorder);
    return { store, recorder };
  };

  {
    const { store } = record({ expected: opsScope });
    assert.deepEqual(readLastSession(store, NOW), { draftScope: opsScope, name: "ops", realm: "local", server: "private", at: NOW },
      "a trusted action's identity plus its own COMMIT records that identity");
  }

  // THE EP6-R1 CASE. A valid handle for ops attaches and COMMITs, but the URL
  // carries a DIFFERENT live incarnation's draft scope. The page must not
  // record either one: the candidate says ops, the URL says build, and the
  // honest outcome when identity signals disagree is to record nothing.
  {
    const { store } = record({ expected: buildSession.draftScope });
    assert.undefinedValue(readLastSession(store, NOW),
      "a substituted draft scope must not record any session");
  }

  // A direct or hand-edited terminal URL has no candidate at all. It may
  // attach; it must not rewrite the memory.
  {
    const { store } = record({ identity: undefined, expected: opsScope });
    assert.undefinedValue(readLastSession(store, NOW), "an unstaged attachment records nothing");
  }

  // The candidate is tied to ONE controller operation and is dropped, without
  // replay, by every outcome that is not that operation's enqueued COMMIT.
  const clearing: [string, (r: ReturnType<typeof createCommittedSessionRecorder>) => void][] = [
    ["stale verdict", (r) => { r.frame(1, COMMIT, "STALE"); r.frame(1, COMMIT, "ENQUEUED"); }],
    ["closed verdict", (r) => { r.frame(1, COMMIT, "CLOSED"); r.frame(1, COMMIT, "ENQUEUED"); }],
    ["transport loss", (r) => { r.openTransport(1); r.transportClosed(1, "closed"); r.frame(1, COMMIT, "ENQUEUED"); }],
    ["operational refusal", (r) => { r.openTransport(1); r.operationalRefusal(1, "input_refused"); r.frame(1, COMMIT, "ENQUEUED"); }],
    ["foreign generation", (r) => { r.openTransport(2); r.frame(2, COMMIT, "ENQUEUED"); }],
    ["navigation abandoned", (r) => { r.openTransport(1); r.abandon(); r.frame(1, COMMIT, "ENQUEUED"); }],
    ["no COMMIT at all", (r) => { r.openTransport(1); r.frame(1, Object.freeze({ type: "PREPARE" }), "ENQUEUED"); }],
  ];
  for (const [label, drive] of clearing) {
    const { store, recorder } = record({ expected: opsScope, drive });
    assert.undefinedValue(readLastSession(store, NOW), `${label} must not record`);
    assert.equal(recorder.recorded(), false, `${label} must not report a write`);
  }

  // ---- adjudication EP6-R4: identity resolution is settled per operation ----
  // The callback carries no generation, so the ONLY window in which it can only
  // be this operation's own resolution is before this recorder is armed. There,
  // a same-operation failure still clears -- that is the page's own identity
  // failing, and it must not go on to record.
  for (const outcome of ["session_gone", "identity_ambiguous", "identity_invalid"] as const) {
    const { store, recorder } = record({ expected: opsScope, beforeArm: (r) => r.identityResolve(outcome) });
    assert.undefinedValue(readLastSession(store, NOW), `a same-operation ${outcome} before arming must clear`);
    assert.equal(recorder.recorded(), false);
  }

  // Once armed, failure belongs to the generation-scoped callbacks. An
  // unowned resolution -- an outgoing pane's, a sibling's -- cannot reach in
  // and cancel this operation, and the operation's own transport still can.
  {
    const { store } = record({ expected: opsScope, drive: (r) => { r.openTransport(1); r.identityResolve("session_gone"); r.frame(1, COMMIT, "ENQUEUED"); } });
    assert.ok(readLastSession(store, NOW), "an unowned identity failure must not clear an armed navigation operation");
  }
  {
    const { store } = record({ expected: opsScope, drive: (r) => { r.openTransport(1); r.transportClosed(1, "closed"); r.identityResolve("session_gone"); r.frame(1, COMMIT, "ENQUEUED"); } });
    assert.undefinedValue(readLastSession(store, NOW), "the operation's own transport loss still clears it");
  }

  // A re-mint that still resolves the same pinned incarnation is not a reason
  // to forget: it is the reopen path doing its job.
  {
    const { store } = record({ expected: opsScope, beforeArm: (r) => r.identityResolve("minted"), drive: (r) => { r.openTransport(1); r.frame(1, COMMIT, "ENQUEUED"); } });
    assert.ok(readLastSession(store, NOW), "an identity re-mint that succeeds still records");
  }

  // Exactly one write per operation: a second COMMIT cannot rewrite it.
  {
    const { store } = record({ expected: opsScope, drive: (r) => { r.frame(1, COMMIT, "ENQUEUED"); r.frame(1, COMMIT, "ENQUEUED"); } });
    assert.equal(readLastSession(store, NOW)?.name, "ops");
  }

  // ---- adjudication EP6-R2: a candidate belongs to ONE target operation ----
  const OP_A = "0123456789abcdef0123456789abcdef";
  const OP_B = "fedcba9876543210fedcba9876543210";
  const buildIdentity = pendingIdentityFromSession(liveInventory.realms[0].servers[0].sessions.find((s2) => s2.name === "build")!);
  assert.ok(buildIdentity);

  // Operation ids are fresh, opaque and shaped; a malformed one is never a key.
  {
    const first = newOperationId();
    const second = newOperationId();
    assert.ok(first && isOperationId(first));
    assert.equal(first === second, false, "each operation gets a fresh id");
    assert.equal(isOperationId("../last-session.v1"), false, "a traversal-shaped id is refused");
    assert.equal(isOperationId(""), false);
    assert.equal(isOperationId("XYZ"), false);
    const store = new FakeStorage();
    assert.equal(stagePendingSession(store, "not-an-id", opsIdentity!, NOW), "invalid", "a malformed id stages nothing");
    assert.equal(store.entries.size, 0);
    assert.undefinedValue(takePendingSession(store, "not-an-id", NOW));
  }

  // SHAPE 1: two trusted actions staged from the same dashboard, in both target
  // load orders and both COMMIT orders. Neither consumes nor evicts the other,
  // and the durable record is the LAST successful matching COMMIT.
  for (const order of ["A-then-B", "B-then-A"]) {
    const store = new FakeStorage();
    assert.equal(stagePendingSession(store, OP_A, opsIdentity!, NOW), "staged");
    assert.equal(stagePendingSession(store, OP_B, buildIdentity!, NOW), "staged");

    const takeA = takePendingSession(store, OP_A, NOW);
    const takeB = takePendingSession(store, OP_B, NOW);
    assert.deepEqual(takeA, opsIdentity, `${order}: target A must get A's identity`);
    assert.deepEqual(takeB, buildIdentity, `${order}: target B must get B's identity`);

    const recorderA = createCommittedSessionRecorder({ mode: "navigation", storage: store, identity: takeA, pageDraftScope: opsIdentity!.draftScope, now: () => NOW });
    const recorderB = createCommittedSessionRecorder({ mode: "navigation", storage: store, identity: takeB, pageDraftScope: buildIdentity!.draftScope, now: () => NOW });
    recorderA.arm(1);
    recorderB.arm(1);
    const [first, second] = order === "A-then-B" ? [recorderA, recorderB] : [recorderB, recorderA];
    first.frame(1, COMMIT, "ENQUEUED");
    second.frame(1, COMMIT, "ENQUEUED");
    assert.equal(recorderA.recorded(), true, `${order}: A's COMMIT must record`);
    assert.equal(recorderB.recorded(), true, `${order}: B's COMMIT must record`);
    const expected = order === "A-then-B" ? buildIdentity : opsIdentity;
    assert.equal(readLastSession(store, NOW)?.draftScope, expected!.draftScope,
      `${order}: the record must equal the LAST successful matching COMMIT`);
  }

  // One operation failing clears only itself; its sibling still records.
  {
    const store = new FakeStorage();
    assert.equal(stagePendingSession(store, OP_A, opsIdentity!, NOW), "staged");
    assert.equal(stagePendingSession(store, OP_B, buildIdentity!, NOW), "staged");
    const failed = createCommittedSessionRecorder({ mode: "navigation", storage: store, identity: takePendingSession(store, OP_A, NOW), pageDraftScope: opsIdentity!.draftScope, now: () => NOW });
    const ok = createCommittedSessionRecorder({ mode: "navigation", storage: store, identity: takePendingSession(store, OP_B, NOW), pageDraftScope: buildIdentity!.draftScope, now: () => NOW });
    failed.arm(1); ok.arm(1);
    failed.transportClosed(1, "closed");
    failed.frame(1, COMMIT, "ENQUEUED");
    ok.frame(1, COMMIT, "ENQUEUED");
    assert.equal(failed.recorded(), false, "the failed operation must not record");
    assert.equal(readLastSession(store, NOW)?.draftScope, buildIdentity!.draftScope, "the sibling operation still records");
  }

  // SHAPE 2: a page with no scope, or a disagreeing scope, DROPS the candidate
  // rather than consuming it -- and cannot record from it either.
  {
    for (const scope of [null, undefined, "", buildIdentity!.draftScope]) {
      const store = new FakeStorage();
      const recorder = createCommittedSessionRecorder({ mode: "navigation", storage: store, identity: opsIdentity, pageDraftScope: scope, now: () => NOW });
      recorder.arm(1);
      recorder.frame(1, COMMIT, "ENQUEUED");
      assert.equal(recorder.recorded(), false, `a page scope of ${JSON.stringify(scope)} must not record`);
      assert.undefinedValue(readLastSession(store, NOW));
    }
    // And the navigation-side drop leaves nothing for anyone else to claim.
    const store = new FakeStorage();
    assert.equal(stagePendingSession(store, OP_A, opsIdentity!, NOW), "staged");
    dropPendingSession(store, OP_A);
    assert.undefinedValue(takePendingSession(store, OP_A, NOW), "a dropped candidate cannot be claimed later");
    assert.equal(store.entries.size, 0);
  }

  // ---- adjudication EP6-R3: the direct E-P4 seam must actually work ----
  // E-P4 switches panes in-process. There is no navigation and therefore no URL
  // scope to cross-check; the operation is named by the caller instead. A
  // resolved-mode recorder that demanded a page scope would silently refuse to
  // record for the one caller the seam exists for, which is what it did.
  {
    const store = new FakeStorage();
    const recorder = createCommittedSessionRecorder({ mode: "resolved", storage: store, identity: opsIdentity, generation: 9, now: () => NOW });
    recorder.frame(9, COMMIT, "ENQUEUED");
    assert.equal(recorder.recorded(), true, "E-P4 resolved identity + generation must record without a URL scope");
    assert.deepEqual(readLastSession(store, NOW), { draftScope: opsScope, name: "ops", realm: "local", server: "private", at: NOW });
  }

  // Its generation is named at construction and cannot be renamed afterwards:
  // a stray arm neither binds nor disarms a resolved operation.
  {
    const store = new FakeStorage();
    const recorder = createCommittedSessionRecorder({ mode: "resolved", storage: store, identity: opsIdentity, generation: 9, now: () => NOW });
    recorder.arm(3);
    recorder.frame(3, COMMIT, "ENQUEUED");
    assert.equal(recorder.recorded(), false, "a resolved operation must not answer to a generation it was not given");
    recorder.frame(9, COMMIT, "ENQUEUED");
    assert.equal(recorder.recorded(), true, "a stray arm must not disarm a resolved operation either");
  }

  // A resolved operation is still bounded by its own generation's outcomes.
  {
    const store = new FakeStorage();
    const recorder = createCommittedSessionRecorder({ mode: "resolved", storage: store, identity: opsIdentity, generation: 9, now: () => NOW });
    recorder.operationalRefusal(9, "input_refused");
    recorder.frame(9, COMMIT, "ENQUEUED");
    assert.equal(recorder.recorded(), false, "a resolved operation's own refusal still clears it");
  }

  // ---- adjudication EP6-R4: an unowned identity failure cannot clear it ----
  // The incoming pane is bound to generation 11. The OUTGOING pane's identity
  // resolution fails; that callback names no operation, so a recorder that
  // honoured it would let the pane being replaced cancel the pane replacing it.
  {
    const store = new FakeStorage();
    const incoming = createCommittedSessionRecorder({ mode: "resolved", storage: store, identity: opsIdentity, generation: 11, now: () => NOW });
    incoming.identityResolve("session_gone");
    incoming.frame(11, COMMIT, "ENQUEUED");
    assert.equal(incoming.recorded(), true, "an unowned identity failure must not clear the incoming operation");
    assert.equal(readLastSession(store, NOW)?.draftScope, opsScope);
  }

  // SHAPE 3: the recorder is bound to ONE controller generation. An unarmed
  // recorder ignores everything, and an outgoing controller's events can never
  // commit the incoming identity.
  {
    const store = new FakeStorage();
    const unarmed = createCommittedSessionRecorder({ mode: "navigation", storage: store, identity: opsIdentity, pageDraftScope: opsIdentity!.draftScope, now: () => NOW });
    unarmed.openTransport(4);
    unarmed.frame(4, COMMIT, "ENQUEUED");
    assert.equal(unarmed.recorded(), false, "an unarmed recorder must never latch a generation");

    // The E-P4 seam: the incoming identity is bound to the incoming generation
    // at construction, so the outgoing pane's frames cannot spend it.
    const incoming = createCommittedSessionRecorder({ mode: "resolved", storage: store, identity: opsIdentity, generation: 7, now: () => NOW });
    incoming.frame(6, COMMIT, "ENQUEUED");
    assert.equal(incoming.recorded(), false, "an outgoing generation must not commit the incoming identity");
    incoming.transportClosed(6, "closed");
    incoming.operationalRefusal(6, "input_refused");
    incoming.frame(7, COMMIT, "ENQUEUED");
    assert.equal(incoming.recorded(), true, "a sibling generation's failure must not clear this operation");
    assert.equal(readLastSession(store, NOW)?.draftScope, opsIdentity!.draftScope);

    // Arming twice with a different generation is a different operation.
    const confused = createCommittedSessionRecorder({ mode: "navigation", storage: new FakeStorage(), identity: opsIdentity, pageDraftScope: opsIdentity!.draftScope, now: () => NOW });
    confused.arm(1);
    confused.arm(2);
    confused.frame(1, COMMIT, "ENQUEUED");
    confused.frame(2, COMMIT, "ENQUEUED");
    assert.equal(confused.recorded(), false, "a recorder armed twice for different operations must not record");
  }

  // Staging carries no capability and is single use.
  {
    const staging = new FakeStorage();
    assert.equal(stagePendingSession(staging, OP_A, opsIdentity!, NOW), "staged");
    const raw = staging.entries.get(PENDING_SESSION_KEY_PREFIX + OP_A) ?? "";
    for (const secret of ["handle", "http", "token", "cccccccccccccccccccccccccccccccccccccccccc0"]) {
      assert.equal(raw.includes(secret), false, `a staged candidate must not carry ${secret}`);
    }
    assert.deepEqual(takePendingSession(staging, OP_A, NOW), opsIdentity, "the candidate is readable exactly once");
    assert.undefinedValue(takePendingSession(staging, OP_A, NOW), "a consumed candidate cannot be replayed");
    assert.equal(staging.entries.size, 0, "consuming a candidate removes it");

    assert.equal(stagePendingSession(staging, OP_A, opsIdentity!, NOW), "staged");
    assert.undefinedValue(takePendingSession(staging, OP_A, NOW + 61_000), "a stale candidate is refused");

    const unwritable = new FakeStorage();
    unwritable.writeFailure = new Error("QuotaExceededError");
    assert.equal(stagePendingSession(unwritable, OP_A, opsIdentity!, NOW), "unavailable", "staging never throws");
  }

  // Abandoned candidates are evicted rather than accumulating; staging one
  // operation never disturbs a live sibling.
  {
    const store = new FakeStorage();
    for (let i = 0; i < 12; i += 1) {
      stagePendingSession(store, i.toString(16).padStart(32, "0"), opsIdentity!, NOW + i);
    }
    assert.equal(store.entries.size <= 8, true, `abandoned candidates accumulated: ${store.entries.size}`);
    assert.ok(takePendingSession(store, (11).toString(16).padStart(32, "0"), NOW + 11), "the newest candidate survives eviction");

    const expired = new FakeStorage();
    stagePendingSession(expired, OP_A, opsIdentity!, NOW);
    stagePendingSession(expired, OP_B, buildIdentity!, NOW + 120_000);
    assert.equal(expired.entries.size, 1, "an expired sibling is swept when a new candidate is staged");
  }

  // The candidate is read back through the same strict codec as the record, so
  // a hostile pending entry fails closed rather than becoming an identity.
  {
    const hostile = new FakeStorage();
    hostile.entries.set(PENDING_SESSION_KEY_PREFIX + OP_A, '{"draftScope":"cccccccccccccccccccccccccccccccccccccccccc0","name":"ops","realm":"local","server":"private","at":' + NOW + '}');
    assert.undefinedValue(takePendingSession(hostile, OP_A, NOW), "a hostile candidate is refused");
  }

  // An inventory session whose realm/server disagree with its own pinned scope
  // cannot become a candidate.
  {
    assert.undefinedValue(pendingIdentityFromSession({ ...opsSession!, realm: "elsewhere" }),
      "a session disagreeing with its own scope is not a candidate");
    assert.undefinedValue(pendingIdentityFromSession({ ...opsSession!, draftScope: "not-an-authority-key" }),
      "a session with an unpinnable scope is not a candidate");
  }
}

// --- EP6-F2: exact-incarnation resolution ------------------------------------
{
  const resume = landingMemoryState(goodRecord, resolveDraftScope(liveInventory, goodRecord.draftScope));
  assert.equal(resume.kind, "resume");
  assert.equal(resume.kind === "resume" ? resume.state : "", "open");
  assert.equal(resume.kind === "resume" ? resume.session.sessionId : "", "$7");

  const adoptableRecord: LastSessionRecord = { ...goodRecord, draftScope: buildSession.draftScope, name: "build" };
  const adoptable = landingMemoryState(adoptableRecord, resolveDraftScope(liveInventory, adoptableRecord.draftScope));
  assert.equal(adoptable.kind === "resume" ? adoptable.state : "", "adoptable");

  const blockedRecord: LastSessionRecord = { ...goodRecord, draftScope: lockedSession.draftScope, name: "locked" };
  const blocked = landingMemoryState(blockedRecord, resolveDraftScope(liveInventory, blockedRecord.draftScope));
  assert.equal(blocked.kind, "blocked");
  assert.equal(blocked.kind === "blocked" ? blocked.blocked : "", "blocked_alt_screen");

  const legacyRecord: LastSessionRecord = { ...goodRecord, draftScope: legacySession.draftScope, name: "legacy" };
  const legacy = landingMemoryState(legacyRecord, resolveDraftScope(liveInventory, legacyRecord.draftScope));
  assert.equal(legacy.kind === "blocked" ? legacy.blocked : "", "unavailable", "a session with no unified projection is honestly unavailable");

  // The remembered session ENDED and a same-name successor was created. The
  // pinned incarnation must refuse to match it.
  const successor = inventoryOf(session("local", "private", "ops", "$21", 900, { state: "open", origin: "birth" }));
  const successorSessions = successor.realms[0].servers[0].sessions;
  assert.equal(successorSessions[0].name, "ops", "the fixture must actually contain a same-name successor");
  assert.ok(successorSessions[0].draftScope !== goodRecord.draftScope, "the successor must be a different incarnation");
  const ended = landingMemoryState(goodRecord, resolveDraftScope(successor, goodRecord.draftScope));
  assert.equal(ended.kind, "ended", "a same-name successor must not be resumed");

  const twins = parseInventory({
    realms: [{ name: "local", display_name: "Local", servers: [
      { label: "private", status: "ok", can_create: false, sessions: [session("local", "private", "ops", "$7", 200, { state: "open", origin: "birth" })] },
      { label: "private", status: "ok", can_create: false, sessions: [session("local", "private", "ops", "$7", 200, { state: "open", origin: "birth" })] },
    ] }],
    aliases: [],
  });
  assert.equal(landingMemoryState(goodRecord, resolveDraftScope(twins, goodRecord.draftScope)).kind, "ambiguous", "two identical scopes must be ambiguous, never a guess");
  assert.equal(landingMemoryState(undefined, undefined).kind, "none");
}

// --- EP6-F5: the default is contained and never impersonates a resume --------
{
  assert.undefinedValue(parseDefaultSessionPreference(null));
  assert.undefinedValue(parseDefaultSessionPreference({ version: 1 }));
  assert.undefinedValue(parseDefaultSessionPreference({ default_session: null }));
  assert.undefinedValue(parseDefaultSessionPreference({ default_session: { realm: "local", server: "private" } }));
  assert.undefinedValue(parseDefaultSessionPreference({ default_session: { realm: "local", server: "private", name: "ops", handle: "x" } }), "an unknown member must be refused");
  assert.undefinedValue(parseDefaultSessionPreference({ default_session: { realm: "local", server: "private", name: "" } }));
  assert.undefinedValue(parseDefaultSessionPreference({ default_session: { realm: "local", server: "private", name: "ops\u0001" } }));
  assert.deepEqual(parseDefaultSessionPreference({ version: 1, theme: "dark", default_session: { realm: "local", server: "private", name: "ops" } }), { realm: "local", server: "private", name: "ops" });
  // The preference is parsed with the same own-property discipline as the
  // stored record: an inherited member is not a member the store sent.
  {
    const polluted = Object.prototype as unknown as Record<string, unknown>;
    let outcome: unknown;
    try {
      polluted.name = "ops";
      outcome = parseDefaultSessionPreference({ default_session: { realm: "local", server: "private" } });
    } finally {
      delete polluted.name;
    }
    assert.undefinedValue(outcome, "an inherited preference member must be refused");
  }

  const preference = { realm: "local", server: "private", name: "ops" } as const;
  assert.equal(defaultSessionState(liveInventory, preference).kind, "open");
  assert.equal(defaultSessionState(liveInventory, { realm: "local", server: "private", name: "locked" }).kind, "blocked");
  assert.equal(defaultSessionState(liveInventory, { realm: "local", server: "private", name: "absent" }).kind, "missing");
  assert.equal(defaultSessionState(liveInventory, undefined).kind, "none");
  assert.equal(defaultSessionState(undefined, preference).kind, "none");
  const twoNamed = inventoryOf(
    session("local", "private", "ops", "$7", 200, { state: "open", origin: "birth" }),
    session("local", "private", "ops", "$8", 250, { state: "open", origin: "birth" }),
  );
  assert.equal(defaultSessionState(twoNamed, preference).kind, "ambiguous");

  // Precedence: a resumable memory suppresses the default entirely; every other
  // memory state may offer it, and it is always the DEFAULT card, never a
  // resume of the ended session.
  assert.equal(defaultSessionIsOffered(landingMemoryState(goodRecord, resolveDraftScope(liveInventory, goodRecord.draftScope))), false);
  const successorInventory = inventoryOf(session("local", "private", "ops", "$21", 900, { state: "open", origin: "birth" }));
  assert.equal(defaultSessionIsOffered(landingMemoryState(goodRecord, resolveDraftScope(successorInventory, goodRecord.draftScope))), true);
  assert.equal(defaultSessionIsOffered(landingMemoryState(undefined, undefined)), true);
}

process.stdout.write("session_memory tests PASS\n");
