import { csrfToken } from "./csrf_refresh";
import { isClipboardRetentionSeconds, type ClipboardRetentionSeconds } from "./clipboard_retention";

export type ClipboardPreferencesSnapshot = Readonly<{
  status: "idle" | "loading" | "ready" | "unavailable";
  defaultRetentionSeconds: ClipboardRetentionSeconds;
  revision: number;
}>;
export type ClipboardPreferencesOutcome = "ok" | "conflict" | "unavailable";
export type ClipboardPreferencesOptions = Readonly<{
  fetch?: (input: string, init: RequestInit) => Promise<Response>;
  csrf?: () => string;
  document?: Document;
}>;

function parsePreferences(value: unknown, etag: string | null): ClipboardPreferencesSnapshot {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Invalid clipboard preferences");
  const wire = value as Record<string, unknown>;
  if (Object.keys(wire).sort().join(",") !== "default_retention_seconds,revision,version"
    || wire.version !== 1 || !isClipboardRetentionSeconds(wire.default_retention_seconds)
    || typeof wire.revision !== "number" || !Number.isSafeInteger(wire.revision) || wire.revision < 1
    || etag !== `"${wire.revision}"`) throw new Error("Invalid clipboard preferences");
  return Object.freeze({ status: "ready", defaultRetentionSeconds: wire.default_retention_seconds, revision: wire.revision });
}

/** Server-owned defaults shared by all visible consumers in one document. */
export class ClipboardPreferencesService {
  private current: ClipboardPreferencesSnapshot = Object.freeze({ status: "idle", defaultRetentionSeconds: 1800, revision: 0 });
  private readonly listeners = new Set<(snapshot: ClipboardPreferencesSnapshot) => void>();
  private readonly fetcher: NonNullable<ClipboardPreferencesOptions["fetch"]>;
  private readonly csrf: () => string;
  private readonly visibilityDocument?: Document;
  private inFlight?: Promise<void>;
  private saveInFlight?: Promise<ClipboardPreferencesOutcome>;
  private generation = 0;
  private disposed = false;
  private readonly foreground = (): void => {
    if (this.visibilityDocument?.visibilityState === "visible" && this.listeners.size) void this.load();
  };

  constructor(options: ClipboardPreferencesOptions = {}) {
    this.fetcher = options.fetch ?? ((input, init) => fetch(input, init));
    this.csrf = options.csrf ?? csrfToken;
    this.visibilityDocument = options.document ?? (typeof document === "undefined" ? undefined : document);
    this.visibilityDocument?.addEventListener("visibilitychange", this.foreground);
  }

  snapshot(): ClipboardPreferencesSnapshot { return this.current; }

  subscribe(listener: (snapshot: ClipboardPreferencesSnapshot) => void): () => void {
    if (this.disposed) return () => undefined;
    this.listeners.add(listener); listener(this.current);
    return () => { this.listeners.delete(listener); };
  }

  load(): Promise<void> {
    if (this.disposed) return Promise.resolve();
    // The save's own reconciliation supplies current authority. A visibility
    // event must not start a read in the middle of that transaction.
    if (this.saveInFlight) return this.saveInFlight.then(() => undefined);
    return this.refresh();
  }

  private refresh(): Promise<void> {
    if (this.disposed) return Promise.resolve();
    if (this.inFlight) return this.inFlight;
    const generation = this.generation;
    if (this.current.status === "idle") this.publish({ ...this.current, status: "loading" });
    const pending = this.read(generation).finally(() => { if (this.inFlight === pending) this.inFlight = undefined; });
    this.inFlight = pending;
    return pending;
  }

  private async read(generation: number): Promise<void> {
    try {
      const response = await this.request("GET");
      if (!response.ok) throw new Error("Clipboard preferences unavailable");
      const snapshot = parsePreferences(await response.json(), response.headers.get("ETag"));
      if (generation === this.generation) this.publish(snapshot);
    } catch {
      if (generation === this.generation) this.publish({ ...this.current, status: "unavailable" });
    }
  }

  setDefault(seconds: ClipboardRetentionSeconds): Promise<ClipboardPreferencesOutcome> {
    if (this.disposed || this.saveInFlight || this.current.status !== "ready" || !isClipboardRetentionSeconds(seconds)) return Promise.resolve("unavailable");
    const revision = this.current.revision;
    this.generation++;
    const pending = Promise.resolve().then(() => this.save(seconds, revision)).finally(() => {
      if (this.saveInFlight === pending) this.saveInFlight = undefined;
    });
    this.saveInFlight = pending;
    this.publish({ ...this.current, status: "loading" });
    return pending;
  }

  private async save(seconds: ClipboardRetentionSeconds, revision: number): Promise<ClipboardPreferencesOutcome> {
    if (this.disposed) return "unavailable";
    let outcome: ClipboardPreferencesOutcome = "unavailable";
    try {
      const response = await this.request("PUT", { default_retention_seconds: seconds, revision });
      if (response.status === 412) outcome = "conflict";
      else if (response.ok) {
        const snapshot = parsePreferences(await response.json(), response.headers.get("ETag"));
        if (snapshot.revision < revision) throw new Error("Stale clipboard preferences");
        this.publish(snapshot); outcome = "ok";
      }
    } catch { /* Reconcile even a lost response; the server may have saved it. */ }
    if (this.disposed) return "unavailable";
    this.generation++;
    // A read begun before the save is never authority for its completion.
    const stale = this.inFlight;
    if (stale) await stale;
    if (this.disposed) return "unavailable";
    await this.refresh();
    return outcome;
  }

  private request(method: "GET" | "PUT", body?: unknown): Promise<Response> {
    return this.fetcher("/api/clipboard/preferences", {
      method, cache: "no-store", credentials: "same-origin",
      headers: body === undefined ? {} : { "Content-Type": "application/json", "X-Persea-CSRF": this.csrf() },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
  }

  private publish(snapshot: ClipboardPreferencesSnapshot): void {
    if (this.disposed) return;
    this.current = Object.freeze(snapshot);
    for (const listener of this.listeners) listener(this.current);
  }

  dispose(): void {
    this.disposed = true; this.generation++; this.listeners.clear();
    this.visibilityDocument?.removeEventListener("visibilitychange", this.foreground);
  }
}

const documentServices = new WeakMap<Document, ClipboardPreferencesService>();
export function documentClipboardPreferences(): ClipboardPreferencesService {
  let service = documentServices.get(document);
  if (!service) {
    service = new ClipboardPreferencesService({ document }); documentServices.set(document, service);
    window.addEventListener("pagehide", (event) => { if (!event.persisted) service?.dispose(); });
  }
  return service;
}
