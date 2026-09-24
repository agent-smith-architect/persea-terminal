// One document-global client for the operator preference record (preferences).
//
// The service owns the sole GET/PUT transport and the strong revision. Pane
// controllers subscribe to it; they never fetch independently, so a six-pane
// workspace still performs one read and one deliberate write. Construction is
// inert except for reading a presentation-only local hint. Server authority
// always replaces that hint before a pane is constructed by the production
// boot paths.
import { csrfToken } from "./csrf_refresh";
import { DEFAULT_UNIFIED_THEME, isUnifiedThemeID, type UnifiedThemeID } from "./unified_themes";

export const OPERATOR_PREFERENCES_HINT_KEY = "persea-terminal.operator-preferences-hint.v1";

// The composer's face, in px (C1). A plain number with a default, never a
// tri-state: the composer has no auto-fit to hand the decision back to, so
// there is nothing for "auto" to mean. It shares the terminal font's range
// because the same eyes read both.
export const COMPOSER_FONT_SIZE_MIN = 9;
export const COMPOSER_FONT_SIZE_MAX = 24;
export const DEFAULT_COMPOSER_FONT_SIZE = 11;

export type DefaultSessionPreference = Readonly<{ realm: string; server: string; name: string }>;
// fontSize is the font-size tri-state: a number is the operator's explicit size,
// which overrides auto-fit on every load and reattach, and `null` is "auto" —
// the page fits the font to the viewport. `null` is a stored state, not the
// absence of one: the wire carries it as JSON null, and the record's own
// revision covers it like any other field. No number in 9…24 is spent as a
// sentinel, so every size in the range stays a choice an operator can make.
export type OperatorPreferences = Readonly<{
  version: 1;
  theme: UnifiedThemeID;
  fontSize: number | null;
  composerFontSize: number;
  defaultSession: DefaultSessionPreference | null;
}>;

export const DEFAULT_OPERATOR_PREFERENCES: OperatorPreferences = Object.freeze({
  version: 1 as const,
  theme: DEFAULT_UNIFIED_THEME,
  fontSize: null,
  composerFontSize: DEFAULT_COMPOSER_FONT_SIZE,
  defaultSession: null,
});

export type OperatorPreferenceStatus = "hint" | "loading" | "ready" | "conflict" | "unavailable";
export type OperatorPreferenceSnapshot = Readonly<{
  preferences: OperatorPreferences;
  revision: number;
  stored: boolean;
  available: boolean;
  status: OperatorPreferenceStatus;
  message: string;
  generation: number;
}>;
export type OperatorPreferenceOutcome = "saved" | "conflict" | "unavailable" | "refused";
export type OperatorPreferenceSubscription = Readonly<{ dispose(): void }>;
// A preview is an in-document presentation lease, not record authority. The
// opaque token lets the write that owns it remove exactly its own overlay;
// an older queued mutation can publish in between without clearing it, and a
// stale completion can never clear a newer preview.
export type OperatorPreferencePreviewToken = Readonly<{ id: number; kind: "composer-font-size" }>;
// An absent patch key means "leave this field alone"; an explicit `null`
// fontSize means "store auto". The two are distinct, so merge() must test for
// `undefined` rather than use `??`, which would swallow the auto state.
export type OperatorPreferencePatch = Readonly<{
  theme?: UnifiedThemeID;
  fontSize?: number | null;
  composerFontSize?: number;
  defaultSession?: DefaultSessionPreference | null;
}>;

export type OperatorPreferencePort = Readonly<{
  snapshot(): OperatorPreferenceSnapshot;
  subscribe(listener: (snapshot: OperatorPreferenceSnapshot) => void): OperatorPreferenceSubscription;
  load(): Promise<void>;
  preview?(patch: OperatorPreferencePatch): OperatorPreferencePreviewToken | undefined;
  update(patch: OperatorPreferencePatch, preview?: OperatorPreferencePreviewToken): Promise<OperatorPreferenceOutcome>;
}>;

export type OperatorPreferenceStats = Readonly<{ gets: number; puts: number; conflicts: number; publications: number }>;
export type OperatorPreferencesServiceOptions = Readonly<{
  fetch?: (input: string, init: RequestInit) => Promise<Response>;
  csrf?: () => string;
  storage?: Pick<Storage, "getItem" | "setItem" | "removeItem">;
  requestTimeoutMs?: number;
}>;

const UNAVAILABLE = "Applied for this page only; preferences unavailable.";
const CONFLICT = "Preferences changed elsewhere. Review and try again.";
const LABEL = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;

function exactObject(value: unknown, keys: readonly string[]): Record<string, unknown> | undefined {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return undefined;
  const object = value as Record<string, unknown>;
  const actual = Object.keys(object).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) return undefined;
  return object;
}

// Like exactObject, but a named key may be absent. Used ONLY for the local
// presentation hint: a hint written by a release before composer_font_size
// existed is worth honouring for the theme it carries, and the missing field
// reads as its default. The wire record stays exact — it comes from the server
// this release is deployed with.
function objectWithOptional(value: unknown, required: readonly string[], optional: readonly string[]): Record<string, unknown> | undefined {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return undefined;
  const object = value as Record<string, unknown>;
  const keys = Object.keys(object);
  if (required.some((key) => !keys.includes(key))) return undefined;
  if (keys.some((key) => !required.includes(key) && !optional.includes(key))) return undefined;
  return object;
}

function validName(value: string): boolean {
  if (value.length === 0 || new TextEncoder().encode(value).length > 128) return false;
  // eslint-disable-next-line no-control-regex
  return !/[\u0000-\u001f\u007f-\u009f]/.test(value);
}

function parseDefaultSession(value: unknown): DefaultSessionPreference | null | undefined {
  if (value === null) return null;
  const object = exactObject(value, ["realm", "server", "name"]);
  if (object === undefined || typeof object.realm !== "string" || typeof object.server !== "string" || typeof object.name !== "string") return undefined;
  if (!LABEL.test(object.realm) || !LABEL.test(object.server) || !validName(object.name)) return undefined;
  return Object.freeze({ realm: object.realm, server: object.server, name: object.name });
}

function validFontSize(value: unknown): value is number | null {
  return value === null || (Number.isSafeInteger(value) && (value as number) >= 9 && (value as number) <= 24);
}

export function validComposerFontSize(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= COMPOSER_FONT_SIZE_MIN && (value as number) <= COMPOSER_FONT_SIZE_MAX;
}

function validPreferences(preferences: OperatorPreferences): boolean {
  return preferences.version === 1
    && isUnifiedThemeID(preferences.theme)
    && validFontSize(preferences.fontSize)
    && validComposerFontSize(preferences.composerFontSize)
    && parseDefaultSession(preferences.defaultSession) !== undefined;
}

function freezePreferences(preferences: OperatorPreferences): OperatorPreferences {
  return Object.freeze({
    version: 1 as const,
    theme: preferences.theme,
    fontSize: preferences.fontSize,
    composerFontSize: preferences.composerFontSize,
    defaultSession: preferences.defaultSession === null ? null : Object.freeze({ ...preferences.defaultSession }),
  });
}

type ParsedRecord = Readonly<{ preferences: OperatorPreferences; revision: number; stored: boolean; available: boolean }>;

// What one PUT attempt established before anything was published. Transport and
// parsing are fallible preparation; publication is the commit. Keeping them in
// separate steps is what stops a subscriber exception from being read as a
// transport failure (see write()).
type WriteSettlement =
  | Readonly<{ kind: "record"; record: ParsedRecord }>
  | Readonly<{ kind: "conflict"; record: ParsedRecord }>
  | Readonly<{ kind: "degraded"; preferences: OperatorPreferences }>;

function parseRecord(value: unknown, etag: string | null): ParsedRecord | undefined {
  const object = exactObject(value, ["version", "theme", "font_size", "composer_font_size", "default_session", "revision", "stored", "available"]);
  if (object === undefined || object.version !== 1 || !isUnifiedThemeID(object.theme)) return undefined;
  if (!validFontSize(object.font_size)) return undefined;
  if (!validComposerFontSize(object.composer_font_size)) return undefined;
  if (!Number.isSafeInteger(object.revision) || (object.revision as number) < 0) return undefined;
  if (typeof object.stored !== "boolean" || typeof object.available !== "boolean") return undefined;
  const revision = object.revision as number;
  if (etag !== `"${revision}"`) return undefined;
  const defaultSession = parseDefaultSession(object.default_session);
  if (defaultSession === undefined) return undefined;
  return Object.freeze({
    preferences: freezePreferences({ version: 1, theme: object.theme, fontSize: object.font_size, composerFontSize: object.composer_font_size, defaultSession }),
    revision,
    stored: object.stored,
    available: object.available,
  });
}

// The hint is a device-local paint before the server answers and a cross-tab
// signal after it; it is never a source of record truth (terminal layout F5), so it
// carries no revision and nothing in it is adopted after load.
function readHint(storage: OperatorPreferencesServiceOptions["storage"]): OperatorPreferences | undefined {
  if (storage === undefined) return undefined;
  try {
    const raw = storage.getItem(OPERATOR_PREFERENCES_HINT_KEY);
    if (raw === null) return undefined;
    const object = objectWithOptional(JSON.parse(raw), ["version", "theme", "font_size", "default_session"], ["composer_font_size"]);
    if (object === undefined || object.version !== 1 || !isUnifiedThemeID(object.theme)) return undefined;
    const defaultSession = parseDefaultSession(object.default_session);
    const composerFontSize = object.composer_font_size === undefined ? DEFAULT_COMPOSER_FONT_SIZE : object.composer_font_size;
    const preferences = { version: 1 as const, theme: object.theme, fontSize: object.font_size as number | null, composerFontSize, defaultSession };
    if (defaultSession === undefined || !validPreferences(preferences as OperatorPreferences)) return undefined;
    return freezePreferences(preferences as OperatorPreferences);
  } catch {
    return undefined;
  }
}

function writeHint(storage: OperatorPreferencesServiceOptions["storage"], preferences: OperatorPreferences): void {
  if (storage === undefined) return;
  try {
    storage.setItem(OPERATOR_PREFERENCES_HINT_KEY, JSON.stringify({
      version: 1,
      theme: preferences.theme,
      font_size: preferences.fontSize,
      composer_font_size: preferences.composerFontSize,
      default_session: preferences.defaultSession,
    }));
  } catch { /* the server record remains authoritative */ }
}

function browserStorage(): OperatorPreferencesServiceOptions["storage"] {
  if (typeof window === "undefined") return undefined;
  try { return window.localStorage; } catch { return undefined; }
}

export class OperatorPreferencesService implements OperatorPreferencePort {
  private readonly listeners = new Set<(snapshot: OperatorPreferenceSnapshot) => void>();
  private current: OperatorPreferenceSnapshot;
  private authoritative: Readonly<{
    record: ParsedRecord;
    status: OperatorPreferenceStatus;
    message: string;
  }>;
  private previewSerial = 0;
  private composerPreview?: Readonly<{ token: OperatorPreferencePreviewToken; value: number }>;
  private loadPromise: Promise<void> | undefined;
  private mutation: Promise<OperatorPreferenceOutcome> = Promise.resolve("saved");
  private readonly fetcher: (input: string, init: RequestInit) => Promise<Response>;
  private readonly csrf: () => string;
  private readonly storage: OperatorPreferencesServiceOptions["storage"];
  private readonly requestTimeoutMs: number;
  private readonly counters = { gets: 0, puts: 0, conflicts: 0, publications: 0 };

  constructor(options: OperatorPreferencesServiceOptions = {}) {
    this.fetcher = options.fetch ?? ((input, init) => window.fetch(input, init));
    this.csrf = options.csrf ?? csrfToken;
    this.storage = options.storage ?? browserStorage();
    this.requestTimeoutMs = options.requestTimeoutMs ?? 5_000;
    const hint = readHint(this.storage);
    const record = Object.freeze({
      preferences: hint ?? DEFAULT_OPERATOR_PREFERENCES,
      revision: 0,
      stored: false,
      available: false,
    });
    const status = hint ? "hint" as const : "loading" as const;
    this.authoritative = Object.freeze({ record, status, message: "" });
    this.current = Object.freeze({
      ...record,
      status,
      message: "",
      generation: 0,
    });
  }

  snapshot(): OperatorPreferenceSnapshot { return this.current; }
  stats(): OperatorPreferenceStats { return Object.freeze({ ...this.counters }); }

  subscribe(listener: (snapshot: OperatorPreferenceSnapshot) => void): OperatorPreferenceSubscription {
    this.listeners.add(listener);
    listener(this.current);
    let disposed = false;
    return Object.freeze({ dispose: () => {
      if (disposed) return;
      disposed = true;
      this.listeners.delete(listener);
    } });
  }

  load(): Promise<void> {
    if (this.loadPromise !== undefined) return this.loadPromise;
    this.loadPromise = this.read(true);
    return this.loadPromise;
  }

  // (as amended after F5): a preference saved in another tab —
  // the dashboard's Appearance card, another terminal — reaches this document
  // without a reload. Every publication writes the localStorage hint, so the
  // `storage` event is the cross-tab SIGNAL; the record itself is always taken
  // from the server by an authoritative read, never from the hint, because the
  // hint carries no operator identity and a same-origin leftover from another
  // profile must not move a loaded record. With refetchOnForeground, a tab
  // coming back to the foreground re-reads too, because a backgrounded phone
  // tab may have been suspended while the event fired. A failed re-read keeps
  // the current record. Only a loaded service reacts (refresh() is a no-op
  // before load), and an unchanged revision publishes nothing.
  watchExternalChanges(options: Readonly<{ refetchOnForeground?: boolean }> = {}): () => void {
    if (typeof window === "undefined" || typeof document === "undefined") return () => undefined;
    const refetch = options.refetchOnForeground === true;
    const onStorage = (event: StorageEvent): void => {
      if (event.key !== null && event.key !== OPERATOR_PREFERENCES_HINT_KEY) return;
      void this.refresh();
    };
    const onForeground = (): void => {
      if (document.visibilityState === "visible") void this.refresh();
    };
    window.addEventListener("storage", onStorage);
    if (refetch) {
      document.addEventListener("visibilitychange", onForeground);
      window.addEventListener("focus", onForeground);
    }
    return () => {
      window.removeEventListener("storage", onStorage);
      document.removeEventListener("visibilitychange", onForeground);
      window.removeEventListener("focus", onForeground);
    };
  }

  // Re-reads the server record. Serialized behind writes so a stale GET can
  // never overtake a PUT's settlement; a no-op before the first load.
  refresh(): Promise<void> {
    if (this.loadPromise === undefined) return Promise.resolve();
    const operation = this.mutation.then(() => this.read(false), () => this.read(false));
    this.mutation = operation.then(() => "saved" as const, () => "saved" as const);
    return operation;
  }

  update(patch: OperatorPreferencePatch, preview?: OperatorPreferencePreviewToken): Promise<OperatorPreferenceOutcome> {
    const operation = this.mutation.then(() => this.write(patch), () => this.write(patch));
    const settled = operation.then(
      (outcome) => {
        this.settlePreview(preview);
        return outcome;
      },
      (error: unknown) => {
        this.settlePreview(preview);
        throw error;
      },
    );
    this.mutation = settled;
    return settled;
  }

  // Publish an in-document presentation intent without writing the local hint.
  // Every pane subscribed to this one service sees the face immediately; the
  // later stored publication writes the hint and therefore remains the sole
  // cross-tab signal.
  preview(patch: OperatorPreferencePatch): OperatorPreferencePreviewToken | undefined {
    if (Object.keys(patch).length !== 1 || patch.composerFontSize === undefined) return undefined;
    const preferences = this.merge(patch);
    if (preferences === undefined || !this.authoritative.record.available) return undefined;
    const token = Object.freeze({ id: ++this.previewSerial, kind: "composer-font-size" as const });
    this.composerPreview = Object.freeze({ token, value: preferences.composerFontSize });
    this.publishProjection();
    return token;
  }

  private publish(record: ParsedRecord, status: OperatorPreferenceStatus, message: string): void {
    this.authoritative = Object.freeze({ record, status, message });
    writeHint(this.storage, record.preferences);
    this.publishProjection();
  }

  private publishProjection(): void {
    const authority = this.authoritative;
    const preview = this.composerPreview;
    const preferences = preview === undefined
      ? authority.record.preferences
      : freezePreferences({ ...authority.record.preferences, composerFontSize: preview.value });
    this.current = Object.freeze({
      ...authority.record,
      preferences,
      status: preview === undefined ? authority.status : "loading",
      message: preview === undefined ? authority.message : "Saving preferences…",
      generation: this.current.generation + 1,
    });
    this.counters.publications += 1;
    for (const listener of [...this.listeners]) listener(this.current);
  }

  private settlePreview(token: OperatorPreferencePreviewToken | undefined): void {
    if (token === undefined || this.composerPreview?.token !== token) return;
    this.composerPreview = undefined;
    this.publishProjection();
  }

  private unavailable(preferences: OperatorPreferences = DEFAULT_OPERATOR_PREFERENCES): void {
    this.publish({ preferences: freezePreferences(preferences), revision: 0, stored: false, available: false }, "unavailable", UNAVAILABLE);
  }

  private async read(publishFailure: boolean): Promise<void> {
    this.counters.gets += 1;
    try {
      const response = await this.request({ method: "GET", cache: "no-store", credentials: "same-origin" });
      if (!response.ok) { if (publishFailure) this.unavailable(); return; }
      const record = parseRecord(await response.json(), response.headers.get("ETag"));
      if (record === undefined) { if (publishFailure) this.unavailable(); return; }
      // A refresh that finds the record unchanged publishes nothing: every
      // subscriber already holds it, and a publication is a real event to them.
      if (!publishFailure && record.revision === this.authoritative.record.revision && this.authoritative.status === "ready") return;
      this.publish(record, record.available ? "ready" : "unavailable", record.available ? "" : UNAVAILABLE);
    } catch {
      if (publishFailure) this.unavailable();
    }
  }

  private merge(
    patch: OperatorPreferencePatch,
    base: OperatorPreferences = this.authoritative.record.preferences,
  ): OperatorPreferences | undefined {
    const preferences = freezePreferences({
      version: 1,
      theme: patch.theme ?? base.theme,
      fontSize: patch.fontSize === undefined ? base.fontSize : patch.fontSize,
      composerFontSize: patch.composerFontSize ?? base.composerFontSize,
      defaultSession: patch.defaultSession === undefined ? base.defaultSession : patch.defaultSession,
    });
    return validPreferences(preferences) ? preferences : undefined;
  }

  private async write(patch: OperatorPreferencePatch): Promise<OperatorPreferenceOutcome> {
    await this.load();
    // A queued full-record write is rebased only on stored/server authority.
    // `current` may carry a newer composer preview whose owning write is still
    // behind this operation in the serial queue; persisting that projection
    // here would let the older operation steal the newer intent.
    const authority = this.authoritative.record;
    const next = this.merge(patch, authority.preferences);
    if (next === undefined) return "refused";
    if (!authority.available) {
      this.unavailable(next);
      return "unavailable";
    }
    const revision = authority.revision;
    this.counters.puts += 1;
    // The attempt owns every fallible transport step; publication happens after
    // it, outside that catch. A subscriber that throws during the commit is a
    // subscriber fault, not a transport fault: catching it here would republish
    // "unavailable" at revision 0 over a record the server had already stored,
    // and the short-circuit above would then make every later save a page-only
    // no-op that never issues another PUT. The exception still leaves through
    // this same operation, so the direct caller keeps its synchronous-commit
    // rejection.
    const settlement = await this.attempt(next, revision);
    if (settlement.kind === "conflict") {
      this.counters.conflicts += 1;
      this.publish(settlement.record, "conflict", CONFLICT);
      return "conflict";
    }
    if (settlement.kind === "record") {
      const { record } = settlement;
      this.publish(record, record.available ? "ready" : "unavailable", record.available ? "" : UNAVAILABLE);
      return record.available ? "saved" : "unavailable";
    }
    this.unavailable(settlement.preferences);
    return "unavailable";
  }

  private async attempt(next: OperatorPreferences, revision: number): Promise<WriteSettlement> {
    try {
      const response = await this.request({
        method: "PUT",
        cache: "no-store",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "X-Persea-CSRF": this.csrf(),
          "If-Match": `"${revision}"`,
        },
        body: JSON.stringify({ version: 1, theme: next.theme, font_size: next.fontSize, composer_font_size: next.composerFontSize, default_session: next.defaultSession }),
      });
      if (response.status === 412) {
        const record = parseRecord(await response.json(), response.headers.get("ETag"));
        // An unreadable conflict body carries no authority at all, so the page
        // falls back to the default record rather than the attempted value.
        return record === undefined
          ? { kind: "degraded", preferences: DEFAULT_OPERATOR_PREFERENCES }
          : { kind: "conflict", record };
      }
      if (!response.ok) return { kind: "degraded", preferences: next };
      const record = parseRecord(await response.json(), response.headers.get("ETag"));
      return record === undefined ? { kind: "degraded", preferences: next } : { kind: "record", record };
    } catch {
      return { kind: "degraded", preferences: next };
    }
  }

  private async request(init: RequestInit): Promise<Response> {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeout = new Promise<Response>((_resolve, reject) => {
      timer = setTimeout(() => {
        controller.abort();
        reject(new Error("preferences request timed out"));
      }, this.requestTimeoutMs);
    });
    try {
      return await Promise.race([
        this.fetcher("/api/preferences", { ...init, signal: controller.signal }),
        timeout,
      ]);
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }
}
