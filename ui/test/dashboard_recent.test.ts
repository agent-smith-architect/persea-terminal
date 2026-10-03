import { parseInventory } from "../src/dashboard";
import { RECENT_SESSION_COUNT_KEY, readRecentSessionCount, saveRecentSessionCount, recentSessionResolution, recentSessions } from "../src/dashboard_recent";
import { readSessionDiscovery, rememberCommittedSession } from "../src/session_discovery";

const assert = Object.assign((value: unknown): void => { if (!value) throw new Error("Expected a truthy value"); }, {
  equal(actual: unknown, expected: unknown): void { if (actual !== expected) throw new Error(`${String(actual)} !== ${String(expected)}`); },
  deepEqual(actual: unknown, expected: unknown): void { if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`); },
});

const saved = new Map<string, string>();
const storage = { getItem: (key: string) => saved.get(key) ?? null, setItem: (key: string, value: string) => { saved.set(key, value); } };
assert.equal(readRecentSessionCount(storage), 3);
for (const count of [0, 3, 5, 8] as const) { assert(saveRecentSessionCount(storage, count)); assert.equal(readRecentSessionCount(storage), count); }
for (const raw of ["-1", "1", "4", "99", "null", " 5", "5.0"]) { storage.setItem(RECENT_SESSION_COUNT_KEY, raw); assert.equal(readRecentSessionCount(storage), 3); }
assert.equal(readRecentSessionCount({ getItem() { throw Error("blocked"); } }), 3);
assert.equal(saveRecentSessionCount({ setItem() { throw Error("blocked"); } }, 5), false);

const source = (id: number, name: string, boot = "first", realm = "r", server = "s") => ({
  realm, server, server_status: "ok", session_id: `$${id}`, name,
  authority: { realm, server, uid: 1000, selector_kind: "socket_name", selector_value: "recent-test", boot_id: boot, server_pid: 42, server_start: 100, session_id: `$${id}`, session_created: 200 + id },
  handles: { alias: "alias", observe: "observe", control: "control" }, width: 80, height: 24, attached: 0, activity: 0, unified: { state: "open", origin: "reconstructed" },
});
const inventory = (...sessions: ReturnType<typeof source>[]) => parseInventory({ aliases: [], realms: [{ name: sessions[0]?.realm ?? "r", servers: [{ label: sessions[0]?.server ?? "s", status: "ok", sessions }] }] });
const initial = inventory(source(1, "alpha"), source(2, "beta"), source(3, "gamma"));
const [alpha, beta, gamma] = initial.realms[0].servers[0].sessions;
const renamed = inventory(source(1, "renamed"));
assert.deepEqual(recentSessionResolution(renamed, alpha.draftScope, "alpha"), { kind: "found", session: renamed.realms[0].servers[0].sessions[0] });
const restarted = inventory(source(7, "alpha", "second"));
const fresh = restarted.realms[0].servers[0].sessions[0];
assert.deepEqual(recentSessionResolution(restarted, alpha.draftScope, "alpha"), { kind: "found", session: fresh });
assert.equal(recentSessionResolution(restarted, alpha.draftScope).kind, "missing");
assert.equal(recentSessionResolution(inventory(source(7, "alpha", "second"), source(8, "alpha", "second")), alpha.draftScope, "alpha").kind, "ambiguous");
assert.equal(recentSessionResolution(inventory(source(7, "alpha", "second", "other")), alpha.draftScope, "alpha").kind, "missing");
assert.equal(recentSessionResolution(inventory(source(7, "alpha", "second", "r", "other")), alpha.draftScope, "alpha").kind, "missing");
assert.equal(recentSessionResolution(restarted, "invalid", "alpha").kind, "missing");
assert.equal(recentSessionResolution(inventory(source(1, "alpha"), source(1, "alpha")), alpha.draftScope, "alpha").kind, "ambiguous");

const history = { pinned: [], recent: [{ scope: alpha.draftScope, at: 10, name: "alpha" }, { scope: gamma.draftScope, at: 30, name: "gamma" }, { scope: beta.draftScope, at: 20, name: "beta" }] };
assert.deepEqual(recentSessions(initial, history).map(item => item.session.name), ["gamma", "beta", "alpha"]);
const last = { draftScope: alpha.draftScope, at: 40, name: "alpha", realm: "r", server: "s" };
assert.deepEqual(recentSessions(initial, history, last).map(item => item.session.name), ["alpha", "gamma", "beta"]);
assert.equal(recentSessions(restarted, { pinned: [], recent: [{ scope: alpha.draftScope, at: 20, name: "alpha" }, { scope: fresh.draftScope, at: 10, name: "alpha" }] }).length, 1);
assert.equal(recentSessions(restarted, { pinned: [], recent: [{ scope: alpha.draftScope, at: 20 }] }, last)[0].session, fresh);
saved.clear(); saveRecentSessionCount(storage, 5);
rememberCommittedSession(storage, alpha.draftScope, 1000, "alpha");
assert.deepEqual(readSessionDiscovery(storage, 1000).recent, [{ scope: alpha.draftScope, at: 1000, name: "alpha" }]);
assert.equal(readRecentSessionCount(storage), 5);
storage.setItem("persea-terminal.session-discovery.v1", JSON.stringify({ recent: [{ scope: alpha.draftScope, at: 1000, name: "bad\nname" }] }));
assert.equal(readSessionDiscovery(storage, 1000).recent[0].name, undefined);
console.log("Recent resolution, device settings, history ordering, and restart ambiguity tests passed");
