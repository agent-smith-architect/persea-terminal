import { csrfToken } from "./csrf_refresh";
import { boundedFetch } from "./bounded_fetch";
import { readSessionDiscovery, saveSessionDiscovery } from "./session_discovery";
import { sessionScopeIdentity } from "./session_memory";

export const DASHBOARD_FAVORITES_LIMIT = 128;
type FetchPort = (input: string, init: RequestInit) => Promise<Response>;
type StoragePort = Pick<Storage, "getItem" | "setItem">;
type RecordValue = Readonly<{ favorites: readonly string[]; revision: number; available: boolean }>;
export type DashboardFavoritesSnapshot = RecordValue & Readonly<{ loaded: boolean; pending: readonly string[]; message: string }>;

function validScope(scope: unknown): scope is string {
  return typeof scope === "string" && scope.length <= 2048 && sessionScopeIdentity(scope) !== undefined;
}

export function parseDashboardFavorites(value: unknown, etag: string | null): RecordValue {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Invalid favorites");
  const record = value as Record<string, unknown>;
  if (Object.keys(record).sort().join(",") !== "available,favorites,revision,version"
    || record.version !== 1 || typeof record.available !== "boolean"
    || typeof record.revision !== "number" || !Number.isSafeInteger(record.revision) || record.revision < 0
    || etag !== `"${record.revision}"` || !Array.isArray(record.favorites)
    || record.favorites.length > DASHBOARD_FAVORITES_LIMIT || !record.favorites.every(validScope)
    || new Set(record.favorites).size !== record.favorites.length) throw new Error("Invalid favorites");
  return Object.freeze({ favorites: Object.freeze([...record.favorites]), revision: record.revision, available: record.available });
}

// Changes express one user's intent against the newest record. Retrying a CAS
// conflict never replaces another device's unrelated favorites with an old list.
export class DashboardFavorites {
  private current: RecordValue = { favorites: [], revision: 0, available: false };
  private loaded = false;
  private loadedAt = 0;
  private message = "";
  private readonly pending = new Set<string>();
  private readonly listeners = new Set<(state: DashboardFavoritesSnapshot) => void>();
  private readFlight?: Promise<void>;
  // The newest revision the server is known to hold (an inventory reports
  // it); a read that brings an older one is followed by another read.
  private wanted = 0;
  private queue: Promise<unknown> = Promise.resolve();
  private epoch = 0;
  private disposed = false;

  constructor(private readonly fetcher: FetchPort = boundedFetch, private readonly csrf = csrfToken) {}

  snapshot(): DashboardFavoritesSnapshot {
    return Object.freeze({ ...this.current, loaded: this.loaded, pending: Object.freeze([...this.pending]), message: this.message });
  }

  subscribe(listener: (state: DashboardFavoritesSnapshot) => void): () => void {
    this.listeners.add(listener); listener(this.snapshot());
    return () => { this.listeners.delete(listener); };
  }

  load(force = false, atLeast = 0): Promise<void> {
    this.wanted = Math.max(this.wanted, atLeast);
    if (this.disposed || this.pending.size || !force && this.loadedAt && Date.now() - this.loadedAt < 60_000) return Promise.resolve();
    if (this.readFlight) return this.readFlight;
    const epoch = this.epoch;
    const wanted = this.wanted;
    const job = this.read(epoch).finally(() => {
      if (this.readFlight === job) this.readFlight = undefined;
      // A read that started before a newer revision was announced may have
      // brought an older one.
      if (this.wanted > wanted && this.current.revision < this.wanted) void this.load(true);
    });
    this.readFlight = job; return job;
  }

  private async read(epoch: number): Promise<void> {
    try {
      const response = await this.fetcher("/api/dashboard-preferences", { method: "GET", cache: "no-store", credentials: "same-origin" });
      if (!response.ok) throw new Error("Favorites unavailable");
      const record = parseDashboardFavorites(await response.json(), response.headers.get("ETag"));
      if (epoch !== this.epoch || this.disposed) return;
      this.current = record; this.loadedAt = Date.now();
      this.message = record.available ? "" : "Favorites are temporarily unavailable. Use Refresh to retry.";
    } catch {
      if (epoch !== this.epoch || this.disposed) return;
      this.current = { ...this.current, available: false };
      this.message = "Favorites could not be synchronized. Use Refresh to retry.";
    }
    this.loaded = true; this.publish();
  }

  set(scope: string, favorite: boolean): Promise<boolean> {
    return this.change([scope], favorite);
  }

  private change(scopes: readonly string[], favorite: boolean): Promise<boolean> {
    if (this.disposed || !this.current.available || !scopes.every(validScope) || scopes.some(scope => this.pending.has(scope))) return Promise.resolve(false);
    for (const scope of scopes) this.pending.add(scope);
    this.epoch++; this.message = ""; this.publish();
    const job = this.queue.then(() => this.save(scopes, favorite)).finally(() => {
      for (const scope of scopes) this.pending.delete(scope);
      this.publish();
      // A newer revision announced while this change was pending.
      if (!this.pending.size && this.current.revision < this.wanted) void this.load(true);
    });
    this.queue = job.catch(() => undefined);
    return job;
  }

  private async save(scopes: readonly string[], favorite: boolean): Promise<boolean> {
    let conflicted = false;
    for (let attempt = 0; attempt < 3 && !this.disposed; attempt++) {
      const favorites = favorite ? [...new Set([...this.current.favorites, ...scopes])] : this.current.favorites.filter(scope => !scopes.includes(scope));
      if (favorites.length > DASHBOARD_FAVORITES_LIMIT) {
        this.message = `The dashboard can save ${DASHBOARD_FAVORITES_LIMIT} favorites. Remove one before adding another.`; return false;
      }
      if (JSON.stringify(favorites) === JSON.stringify(this.current.favorites)) {
        // A star removed elsewhere, or moved to its session's new identity
        // while this removal was on its way: the operator must look again.
        if (conflicted && !favorite) { this.message = "Favorites changed while saving. Use Refresh, then try again."; return false; }
        return true;
      }
      const revision = this.current.revision;
      try {
        const response = await this.fetcher("/api/dashboard-preferences", {
          method: "PUT", cache: "no-store", credentials: "same-origin",
          headers: { "Content-Type": "application/json", "X-Persea-CSRF": this.csrf(), "If-Match": `"${revision}"` },
          body: JSON.stringify({ version: 1, favorites }),
        });
        if (!response.ok && response.status !== 412) throw new Error("Favorites unavailable");
        const record = parseDashboardFavorites(await response.json(), response.headers.get("ETag"));
        if (!record.available || record.revision < revision || response.ok && record.revision !== revision + 1) throw new Error("Invalid favorites response");
        this.current = record; this.loaded = true; this.loadedAt = Date.now();
        if (response.status === 412) { conflicted = true; continue; }
        if (!scopes.every(scope => record.favorites.includes(scope) === favorite)) throw new Error("Favorites were not saved");
        this.message = ""; return true;
      } catch {
        // A lost response may still have committed. A new read settles what
        // actually persisted without repeating a mutation after a network error.
        await this.read(this.epoch);
        if (this.current.available && scopes.every(scope => this.current.favorites.includes(scope) === favorite)) { this.message = ""; return true; }
        this.message = "The favorite could not be saved. Use Refresh and try again."; return false;
      }
    }
    this.message = "Favorites changed on another device. Try again."; return false;
  }

  async migrateDevicePins(storage: StoragePort | undefined): Promise<void> {
    if (!storage || !this.current.available) return;
    const legacy = readSessionDiscovery(storage);
    const scopes = legacy.pinned.filter(validScope);
    if (!scopes.length) return;
    if (await this.change(scopes, true)) {
      // Re-read to preserve a terminal visit made while the import was saving.
      const latest = readSessionDiscovery(storage);
      saveSessionDiscovery(storage, { ...latest, pinned: latest.pinned.filter(scope => !scopes.includes(scope)) });
    }
  }

  private publish(): void {
    if (this.disposed) return;
    const state = this.snapshot(); for (const listener of this.listeners) listener(state);
  }

  dispose(): void { this.disposed = true; this.epoch++; this.listeners.clear(); }
}
