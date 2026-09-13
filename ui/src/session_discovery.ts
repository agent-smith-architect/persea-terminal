// Device-local presentation hints contain exact incarnation identities, never
// capabilities. Every action still resolves through the current inventory.
const KEY = "persea-terminal.session-discovery.v1";
type StoragePort = Pick<Storage, "getItem" | "setItem">;
export type SessionDiscovery = { pinned: string[]; recent: Array<{ scope: string; at: number }> };
export function readSessionDiscovery(storage: StoragePort | undefined, now = Date.now()): SessionDiscovery {
  const empty = (): SessionDiscovery => ({ pinned: [], recent: [] });
  try {
    const value: unknown = JSON.parse(storage?.getItem(KEY) ?? "null");
    if (!value || typeof value !== "object") return empty();
    const record = value as Record<string, unknown>;
    const scope = (value: unknown): value is string => typeof value === "string" && value.length > 0 && value.length <= 4096;
    const pinned = Array.isArray(record.pinned) ? [...new Set(record.pinned.filter(scope))].slice(0, 128) : [];
    const recent: SessionDiscovery["recent"] = [];
    for (const item of Array.isArray(record.recent) ? record.recent.slice(0, 128) : []) {
      if (!item || typeof item !== "object" || !scope(item.scope) || !Number.isSafeInteger(item.at) || item.at > now + 60_000 || item.at < now - 30 * 86_400_000) continue;
      if (!recent.some(entry => entry.scope === item.scope)) recent.push({ scope: item.scope, at: item.at });
    }
    return { pinned, recent: recent.sort((a, b) => b.at - a.at).slice(0, 32) };
  } catch { return empty(); }
}
export function saveSessionDiscovery(storage: StoragePort | undefined, value: SessionDiscovery): boolean {
  try { if (!storage) return false; storage.setItem(KEY, JSON.stringify(value)); return true; } catch { return false; }
}
export function rememberCommittedSession(storage: StoragePort, scope: string, at: number): void {
  const state = readSessionDiscovery(storage, at);
  state.recent = [{ scope, at }, ...state.recent.filter(item => item.scope !== scope)].slice(0, 32);
  saveSessionDiscovery(storage, state);
}
