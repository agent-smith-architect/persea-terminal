import type { DashboardInventory, DashboardSession, DraftScopeResolution } from "./dashboard";
import type { SessionDiscovery } from "./session_discovery";
import { sessionScopeIdentity, type LastSessionRecord } from "./session_memory";

export const RECENT_SESSION_COUNT_KEY = "persea-terminal.recent-sessions.v1";
export const RECENT_SESSION_COUNTS = [0, 3, 5, 8] as const;
export type RecentSessionCount = typeof RECENT_SESSION_COUNTS[number];

export function readRecentSessionCount(storage: Pick<Storage, "getItem">): RecentSessionCount {
  try {
    const raw = storage.getItem(RECENT_SESSION_COUNT_KEY);
    return RECENT_SESSION_COUNTS.find(count => raw === String(count)) ?? 3;
  } catch { return 3; }
}

export function saveRecentSessionCount(storage: Pick<Storage, "setItem">, count: RecentSessionCount): boolean {
  try { storage.setItem(RECENT_SESSION_COUNT_KEY, String(count)); return true; } catch { return false; }
}

// A remembered name is only a lookup hint. The resulting row, handle and staged
// identity always come from the inventory that already supplied the main list.
export function recentSessionResolution(inventory: DashboardInventory, scope: string, name?: string): DraftScopeResolution {
  const sessions = inventory.realms.flatMap(realm => realm.servers.flatMap(server => server.sessions));
  const exact = sessions.filter(session => session.draftScope === scope);
  if (exact.length === 1) return { kind: "found", session: exact[0] };
  if (exact.length > 1) return { kind: "ambiguous" };
  const identity = sessionScopeIdentity(scope);
if (!identity || !name) return { kind: "missing" };
  const named = sessions.filter(session => session.realm === identity.realm && session.server === identity.server && session.name === name);
if (named.length === 1) return { kind: "found", session: named[0] };
  return { kind: named.length > 1 ? "ambiguous" : "missing" };
}

export type RecentSession = Readonly<{ session: DashboardSession; at: number }>;

export function recentSessions(inventory: DashboardInventory, discovery: SessionDiscovery, last?: LastSessionRecord): readonly RecentSession[] {
  const remembered = new Map(discovery.recent.map(item => [item.scope, { ...item }]));
  if (last) {
    const previous = remembered.get(last.draftScope);
    remembered.set(last.draftScope, { scope: last.draftScope, at: Math.max(last.at, previous?.at ?? 0), name: previous?.name ?? last.name });
  }
  const ordered = [...remembered.values()].sort((a, b) => b.at - a.at || (a.scope < b.scope ? -1 : a.scope > b.scope ? 1 : 0));
  const seen = new Set<string>();
  const result: RecentSession[] = [];
  for (const item of ordered) {
    const resolved = recentSessionResolution(inventory, item.scope, item.name);
    if (resolved.kind !== "found" || seen.has(resolved.session.draftScope)) continue;
    seen.add(resolved.session.draftScope);
    result.push({ session: resolved.session, at: item.at });
  }
  return result;
}
