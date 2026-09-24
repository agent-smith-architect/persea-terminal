// The dashboard's session list + filter as a reusable list model.
// The extraction must be behaviour-identical: this suite replays the exact
// loop the dashboard used to run inline (its previous applyPresentation body)
// as a reference oracle and requires the same visibility and collapse verdict
// for every session, server, and realm across queries and collapse sets.
declare const process: { stdout: { write(value: string): void } };
import { effectiveCollapse, filterSessionList, normalizeSessionFilter, sessionListPresentation, sessionMatchesFilter, type DashboardAlias, type SessionListRealm } from "../src/dashboard";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  deepEqual(actual: unknown, expected: unknown, message = "values differ"): void { if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
};

function alias(displayAlias: string): DashboardAlias { return { aliasId: `id-${displayAlias}`, displayAlias, revision: 1, state: "bound" }; }
function session(name: string, ...aliases: string[]): { name: string; aliases: DashboardAlias[] } { return { name, aliases: aliases.map(alias) }; }

// The previous inline algorithm, verbatim in structure, as the oracle.
function reference(realms: readonly SessionListRealm[], query: string, collapsedGroups: ReadonlySet<string>): unknown {
  const filterActive = normalizeSessionFilter(query) !== "";
  const out: Array<{ visible: boolean; servers: Array<{ visible: boolean; collapsed: boolean; sessions: boolean[] }> }> = [];
  for (const realm of realms) {
    let visibleServers = 0;
    const servers: Array<{ visible: boolean; collapsed: boolean; sessions: boolean[] }> = [];
    for (const server of realm.servers) {
      let matches = 0;
      const sessions: boolean[] = [];
      for (const item of server.sessions) { const visible = sessionMatchesFilter(item, query); sessions.push(visible); if (visible) matches += 1; }
      const serverVisible = !filterActive || matches > 0 || server.hasError;
      if (serverVisible) visibleServers += 1;
      const collapsed = effectiveCollapse(collapsedGroups.has(server.key), filterActive);
      servers.push({ visible: serverVisible, collapsed, sessions });
    }
    out.push({ visible: !(filterActive && visibleServers === 0 && !realm.hasError), servers });
  }
  return { filterActive, realms: out };
}

const realms: SessionListRealm[] = [
  { hasError: false, servers: [
    { key: "local\u0000private", hasError: false, sessions: [session("build-server", "Deploy lane", "Second"), session("plain"), session("Scratch")] },
    { key: "local\u0000shared", hasError: true, sessions: [] },
  ] },
  { hasError: true, servers: [{ key: "remote\u0000other", hasError: false, sessions: [session("remote-name")] }] },
  { hasError: false, servers: [{ key: "empty\u0000srv", hasError: false, sessions: [] }] },
];

const queries = ["", "   ", "build", "SERVER", "deploy", "second", "absent", "  lane ", "plain", "scratch", "remote", "e"];
const collapseSets = [new Set<string>(), new Set(["local\u0000private"]), new Set(["local\u0000private", "remote\u0000other", "empty\u0000srv"]), new Set(["nowhere"])];
let compared = 0;
for (const query of queries) {
  for (const collapsed of collapseSets) {
    assert.deepEqual(sessionListPresentation(realms, query, collapsed), reference(realms, query, collapsed), `presentation for ${JSON.stringify(query)} with ${JSON.stringify([...collapsed])}`);
    compared += 1;
  }
}
assert.ok(compared === queries.length * collapseSets.length, "every combination was compared");

// Pinned semantics the oracle encodes.
{
  const active = sessionListPresentation(realms, "build", new Set(["local\u0000private"]));
  assert.equal(active.filterActive, true);
  assert.deepEqual(active.realms[0].servers[0].sessions, [true, false, false], "only the match is visible");
  assert.equal(active.realms[0].servers[0].collapsed, false, "an active filter reveals a collapsed group");
  assert.equal(active.realms[0].servers[1].visible, true, "an erroring server stays visible without matches");
  assert.equal(active.realms[1].visible, true, "an erroring realm stays visible without matches");
  assert.equal(active.realms[2].visible, false, "a realm with no matches and no error hides");
  const idle = sessionListPresentation(realms, "", new Set(["local\u0000private"]));
  assert.equal(idle.filterActive, false);
  assert.equal(idle.realms[0].servers[0].collapsed, true, "a cleared filter restores the persisted collapse");
  assert.deepEqual(idle.realms.map((realm) => realm.visible), [true, true, true]);
}

// The flat picker helper keeps order and applies the same matcher.
{
  const list = realms[0].servers[0].sessions;
  assert.deepEqual(filterSessionList(list, "SECOND").map((item) => item.name), ["build-server"]);
  assert.deepEqual(filterSessionList(list, " ").map((item) => item.name), ["build-server", "plain", "Scratch"]);
  assert.deepEqual(filterSessionList(list, "zzz"), []);
}

process.stdout.write("session_list_model.test: PASS\n");
