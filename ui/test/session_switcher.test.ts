declare const process: { stdout: { write(value: string): void } };

import {
  sessionSwitcherRows,
  switcherInventory,
  type SessionSwitcherInventory,
} from "../src/session_switcher";
import type { DashboardAlias, DashboardInventory, DashboardSession } from "../src/dashboard";
import { outputActivityLabel, compareSessionNames } from "../src/session_metadata";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void {
    if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`);
  },
  deepEqual(actual: unknown, expected: unknown, message = "values differ"): void {
    if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
  },
};

const alias = (displayAlias: string): DashboardAlias => ({ aliasId: `alias-${displayAlias}`, displayAlias, revision: 1, state: "bound" });
const session = (
  name: string,
  draftScope: string,
  unified: DashboardSession["unified"],
  activity: number,
  aliases: readonly DashboardAlias[] = [],
): DashboardSession => ({
  handles: { alias: `alias-${name}`, observe: `observe-${name}`, control: `control-${name}` },
  realm: "local", uid: 1000, server: "private", serverStatus: "ok", sessionId: `$${draftScope}`,
  name, width: 80, height: 24, attached: 0, activity, outputActivity: activity, aliases, draftScope, ...(unified ? { unified } : {}),
});

const alpha = session("alpha", "scope-a", { state: "open", origin: "birth" }, 1_000, [alias("first")]);
const beta = session("beta", "scope-b", { state: "adoptable" }, 2_000, [alias("build lane")]);
const blocked = session("blocked", "scope-c", { state: "blocked_alt_screen" }, 3_000);
const unavailable = session("legacy", "scope-d", undefined, 4_000);
const inventory: SessionSwitcherInventory = Object.freeze({
  sessions: Object.freeze([alpha, beta, blocked, unavailable]),
  groups: Object.freeze([Object.freeze({ key: "local\u0000private", realm: "local", realmLabel: "Local operator", server: "private", sessions: Object.freeze([alpha, beta, blocked, unavailable]) })]),
});

{
  const rows = sessionSwitcherRows(inventory, "scope-a", "", () => "blocked reason", 4_200_000);
  assert.deepEqual(rows.map((row) => [row.session.name, row.selectable, row.current, row.reason]), [
    ["alpha", true, true, ""],
    ["beta", true, false, ""],
    ["blocked", false, false, "blocked reason"],
    ["legacy", false, false, "Unified terminal unavailable"],
  ], "authoritative inventory order and closed eligibility");
  assert.equal(rows[0]?.activityLabel, "Output 53m ago", "output time is honest and compact");
  assert.equal(rows[0]?.statusLabel, "current · Output 53m ago", "current state hid its activity");
  assert.deepEqual(
    rows.map((row) => [row.realmLabel, row.serverLabel, row.primaryAlias, row.geometryLabel, row.attachmentLabel]),
    [
      ["Local operator", "private", "first", "80×24", "0 attached"],
      ["Local operator", "private", "build lane", "80×24", "0 attached"],
      ["Local operator", "private", "", "80×24", "0 attached"],
      ["Local operator", "private", "", "80×24", "0 attached"],
    ],
    "group, alias and compact metadata projection",
  );
}

{
  const rows = sessionSwitcherRows(inventory, "scope-c", "", () => "blocked reason", 4_200_000);
  assert.equal(rows[2]?.statusLabel, "blocked reason", "a blocked current row hid its refusal behind the current label");
}

{
  const filtered = sessionSwitcherRows(inventory, "scope-a", "BUILD", () => "blocked reason", 4_200_000);
  assert.deepEqual(filtered.map((row) => row.session.name), ["beta"], "name/alias search is shared with dashboard semantics");
}

{
  const remote = { ...beta, realm: "remote", server: "build", draftScope: "scope-remote" };
  const projected = switcherInventory({
    realms: [
      { name: "local", displayName: "Alice", servers: [{ realm: "local", label: "private", status: "ok", canCreate: false, sessions: [alpha] }] },
      { name: "remote", displayName: "Build user", servers: [{ realm: "remote", label: "build", status: "ok", canCreate: false, sessions: [remote] }] },
    ],
    detachedAliases: [],
  } satisfies DashboardInventory);
  assert.deepEqual(projected.groups?.map((group) => [group.realmLabel, group.server, group.sessions[0]?.name]), [
    ["Alice", "private", "alpha"],
    ["Build user", "build", "beta"],
  ], "per-user realm hierarchy was flattened");
}

{
  const now = 200_000_000;
  assert.equal(outputActivityLabel(undefined, now), "Output time unknown");
  assert.equal(outputActivityLabel(0, now), "Output time unknown");
  assert.equal(outputActivityLabel(999_999, now), "Output time unknown");
  assert.equal(outputActivityLabel(now / 1000 - 9 * 3600, now), "Output 9h ago", "no eight-hour cap");
  assert.equal(outputActivityLabel(now / 1000 - 2 * 86_400, now), "Output 2d ago");
  const active = { ...alpha, activity: 1, outputActivity: now / 1000 - 10 };
  assert.equal(sessionSwitcherRows({ sessions: [active] }, null, "", () => "", now)[0]?.activityLabel, "Output just now", "session interaction must not mask recent output");
  assert.deepEqual(["qt10", "qt2", "qt1"].map(name => ({ name })).sort(compareSessionNames).map(item => item.name), ["qt1", "qt2", "qt10"]);
}
process.stdout.write("session_switcher.test: PASS\n");
