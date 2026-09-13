import { csrfToken } from "./csrf_refresh";
import { DEFAULT_KEY_PREFERENCES, KEY_PREFERENCES_KEY, prefixSequence, resolveAction, validPrefixName, validateKeyPreferences } from "./terminal_actions";

export type KeyboardLayout = Readonly<{ bar: readonly string[]; favorites: readonly string[] }>;
export type KeyboardPreferences = Readonly<{ version: 1; layout: KeyboardLayout; prefixes: Readonly<Record<string, string>> }>;
export type DeviceKeyboardPreferences = Readonly<{ version: 1; layout?: KeyboardLayout; prefixes?: Readonly<Record<string, string>> }>;
export type KeyboardPreferenceSnapshot = Readonly<{
  shared: KeyboardPreferences; device: DeviceKeyboardPreferences; effective: KeyboardPreferences;
  revision: number; available: boolean; stored: boolean; loaded: boolean; generation: number; message: string;
}>;
export type KeyboardSaveOutcome = "saved" | "conflict" | "unavailable";
export const DEVICE_KEYBOARD_PREFERENCES_KEY = "persea-terminal.keyboard-device.v1";
export const KEYBOARD_DEFAULTS_UPGRADE_KEY = "persea-terminal.keyboard-defaults-upgrade.v1";
const SHARED_SIGNAL = "persea-terminal.keyboard-shared-change.v1";
export const DEFAULT_KEYBOARD_PREFERENCES: KeyboardPreferences = Object.freeze({
  version: 1,
  layout: Object.freeze({
    bar: Object.freeze(["key:escape:0", "key:tab:0", "modifier:ctrl", "key:arrow-left:0", "key:arrow-down:0", "key:arrow-up:0", "key:arrow-right:0"]),
    favorites: Object.freeze(["modifier:alt", ...DEFAULT_KEY_PREFERENCES.favorites]),
  }),
  prefixes: Object.freeze({ ...DEFAULT_KEY_PREFERENCES.prefixes }),
});
const EMPTY_DEVICE: DeviceKeyboardPreferences = Object.freeze({ version: 1 });
type LocalStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;
type Options = Readonly<{ storage?: LocalStorage | null; fetch?: (url: string, init: RequestInit) => Promise<Response>; csrf?: () => string; timeoutMs?: number }>;
function object(value: unknown): value is Record<string, unknown> { return value !== null && typeof value === "object" && !Array.isArray(value); }
function exact(value: unknown, required: string[], optional: string[] = []): value is Record<string, unknown> {
  return object(value) && required.every((key) => Object.hasOwn(value, key)) && Object.keys(value).every((key) => required.includes(key) || optional.includes(key));
}
export function canonicalKeyboardAction(id: string): string | undefined {
  const entry = resolveAction(id);
  if (entry?.action.kind === "key") return entry.id;
  if (entry?.action.kind === "sequence" && entry.action.keys.length === 1) return prefixSequence(entry.action.prefix, entry.action.keys[0]).id;
  return undefined;
}
function ids(value: unknown, bar: boolean): value is string[] {
  return Array.isArray(value) && value.length <= 100 && new Set(value).size === value.length
    && value.every((id) => typeof id === "string" && ((bar && /^modifier:(ctrl|alt|shift)$/.test(id)) || canonicalKeyboardAction(id) === id));
}
function layout(value: unknown): value is KeyboardLayout { return exact(value, ["bar", "favorites"]) && ids(value.bar, true) && ids(value.favorites, true); }
function prefixes(value: unknown): value is Record<string, string> {
  return object(value) && Object.keys(value).length <= 20 && new Set(Object.keys(value).map((name) => name.toLowerCase())).size === Object.keys(value).length && Object.entries(value).every(([name, id]) => validPrefixName(name) && typeof id === "string" && id.startsWith("key:") && canonicalKeyboardAction(id) === id);
}
export function parseKeyboardPreferences(value: unknown): KeyboardPreferences | undefined {
  if (!exact(value, ["version", "layout", "prefixes"]) || value.version !== 1 || !layout(value.layout) || !prefixes(value.prefixes)) return undefined;
  return freeze(value as KeyboardPreferences);
}
export function parseDeviceKeyboardPreferences(value: unknown): DeviceKeyboardPreferences | undefined {
  if (!exact(value, ["version"], ["layout", "prefixes"]) || value.version !== 1 || (Object.hasOwn(value, "layout") && !layout(value.layout)) || (Object.hasOwn(value, "prefixes") && !prefixes(value.prefixes))) return undefined;
  return Object.freeze({ version: 1, ...(value.layout ? { layout: freezeLayout(value.layout as KeyboardLayout) } : {}), ...(value.prefixes ? { prefixes: Object.freeze({ ...value.prefixes as Record<string, string> }) } : {}) });
}
function freezeLayout(value: KeyboardLayout): KeyboardLayout { return Object.freeze({ bar: Object.freeze([...value.bar]), favorites: Object.freeze([...value.favorites]) }); }
function freeze(value: KeyboardPreferences): KeyboardPreferences { return Object.freeze({ version: 1, layout: freezeLayout(value.layout), prefixes: Object.freeze({ ...value.prefixes }) }); }
export function effectiveKeyboardPreferences(shared: KeyboardPreferences, device: DeviceKeyboardPreferences): KeyboardPreferences {
  return freeze({ version: 1, layout: device.layout ?? shared.layout, prefixes: device.prefixes ?? shared.prefixes });
}
export function migrateLegacyKeyboardPreferences(value: unknown): DeviceKeyboardPreferences | undefined {
  const old = validateKeyPreferences(value);
  if (!old) return undefined;
  if (JSON.stringify(old) === JSON.stringify(DEFAULT_KEY_PREFERENCES)) return EMPTY_DEVICE;
  // Old records allowed names differing only in case. Allocate unique names
  // and rewrite their references together so no sequence changes its meaning.
  const names = new Map<string, string>(), used = new Set<string>();
  for (const name of Object.keys(old.prefixes)) {
    let candidate = name, serial = 2;
    while (used.has(candidate.toLowerCase())) { const suffix = ` ${serial++}`; candidate = name.slice(0, 32 - suffix.length) + suffix; }
    names.set(name, candidate); used.add(candidate.toLowerCase());
  }
  const normalize = (id: string): string | undefined => {
    const entry = resolveAction(id);
    if (entry?.action.kind === "sequence" && entry.action.keys.length === 1) return prefixSequence(names.get(entry.action.prefix) ?? entry.action.prefix, entry.action.keys[0]).id;
    return canonicalKeyboardAction(id);
  };
  const normalized = (items: readonly string[]) => [...new Set(items.map(normalize).filter((id): id is string => !!id))];
  const pinned = normalized(old.pinned), factory = DEFAULT_KEYBOARD_PREFERENCES.layout.bar;
  const combined = [...new Set([...factory, ...pinned])];
  // User pins take priority over automatically inserted factory controls at
  // the new bar's bound. Favorites retain every explicit legacy key as well.
  const bar = combined.length <= 100 ? combined : [...pinned, ...factory.filter((id) => !pinned.includes(id)).slice(0, 100 - pinned.length)];
  return parseDeviceKeyboardPreferences({ version: 1, layout: { bar, favorites: normalized(old.favorites) }, prefixes: Object.fromEntries(Object.entries(old.prefixes).map(([name, id]) => [names.get(name)!, canonicalKeyboardAction(id)])) });
}
function browserStorage(): LocalStorage | undefined { try { return window.localStorage; } catch { return undefined; } }

/** Shared record authority and explicit browser overrides. A document owns one
 * instance; panes subscribe. Edits never upload until an explicit shared save. */
export class KeyboardPreferencesService {
  private readonly storage: LocalStorage | undefined;
  private readonly fetcher: NonNullable<Options["fetch"]>;
  private readonly listeners = new Set<(snapshot: KeyboardPreferenceSnapshot) => void>();
  private current: KeyboardPreferenceSnapshot;
  private loading?: Promise<void>;
  private writing = false;
  private deferredLoad = false;
  private stopWatching?: () => void;
  constructor(private readonly options: Options = {}) {
    this.storage = options.storage === null ? undefined : options.storage ?? browserStorage();
    this.fetcher = options.fetch ?? ((url, init) => window.fetch(url, init));
    const { device, message } = this.readDevice(true);
    this.current = Object.freeze({ shared: DEFAULT_KEYBOARD_PREFERENCES, device, effective: effectiveKeyboardPreferences(DEFAULT_KEYBOARD_PREFERENCES, device), revision: 0, available: false, stored: false, loaded: false, generation: 0, message });
  }
  snapshot(): KeyboardPreferenceSnapshot { return this.current; }
  subscribe(listener: (snapshot: KeyboardPreferenceSnapshot) => void): () => void { this.listeners.add(listener); return () => this.listeners.delete(listener); }
  private publish(patch: Partial<KeyboardPreferenceSnapshot>): void {
    const next = { ...this.current, ...patch, generation: this.current.generation + 1 };
    this.current = Object.freeze({ ...next, effective: effectiveKeyboardPreferences(next.shared, next.device) });
    // Subscriber bugs must never turn a durable successful save into a retry.
    for (const listener of this.listeners) { try { listener(this.current); } catch (error) { console.error("Keyboard settings subscriber failed", error); } }
  }
  private readDevice(migrate: boolean): { device: DeviceKeyboardPreferences; message: string } {
    try {
      const raw = this.storage?.getItem(DEVICE_KEYBOARD_PREFERENCES_KEY);
      if (raw !== null && raw !== undefined) {
        const device = parseDeviceKeyboardPreferences(JSON.parse(raw));
        return device ? this.upgradeDeviceDefaults(device, migrate) : { device: EMPTY_DEVICE, message: "This browser's saved keyboard settings could not be read." };
      }
      const legacy = migrate ? this.storage?.getItem(KEY_PREFERENCES_KEY) : null;
      const device = legacy ? migrateLegacyKeyboardPreferences(JSON.parse(legacy)) : EMPTY_DEVICE;
      if (legacy && device) {
        try { this.storage?.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify(device)); }
        catch { return { device, message: "Previous keyboard settings are retained for this page; browser storage is unavailable." }; }
        return this.upgradeDeviceDefaults(device, migrate);
      }
      return { device: device ?? EMPTY_DEVICE, message: legacy && !device ? "Previous keyboard settings could not be read." : "" };
    } catch { return { device: EMPTY_DEVICE, message: "Browser storage is unavailable; changes can only last for this page." }; }
  }
  private upgradeDeviceDefaults(device: DeviceKeyboardPreferences, migrate: boolean): { device: DeviceKeyboardPreferences; message: string } {
    if (!migrate) return { device, message: "" };
    try { if (this.storage?.getItem(KEYBOARD_DEFAULTS_UPGRADE_KEY) === "1") return { device, message: "" }; }
    catch { return { device, message: "Saved keyboard settings are retained; browser storage is unavailable." }; }
    const favorites = device.layout?.favorites;
    // Existing browser layouts predate the Alt default. Add it once without
    // resetting their bar/prefixes, filling an explicitly empty layout, or
    // evicting a favorite at the limit. Later explicit edits stay authoritative.
    if (device.layout && favorites?.length && favorites.length < 100 && !favorites.includes("modifier:alt")) {
      device = Object.freeze({ ...device, layout: freezeLayout({ ...device.layout, favorites: ["modifier:alt", ...favorites] }) });
    }
    try {
      this.storage?.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify(device));
      this.storage?.setItem(KEYBOARD_DEFAULTS_UPGRADE_KEY, "1");
      return { device, message: "" };
    } catch { return { device, message: "Updated keyboard defaults apply to this page; browser storage is unavailable." }; }
  }
  private async request(method: "GET" | "PUT", preferences?: KeyboardPreferences, revision?: number): Promise<{ response: Response; value: unknown }> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.options.timeoutMs ?? 5000);
    try {
      const response = await this.fetcher("/api/keyboard-preferences", {
        method, credentials: "same-origin", cache: "no-store", signal: controller.signal,
        headers: method === "PUT" ? { "Content-Type": "application/json", "X-Persea-CSRF": (this.options.csrf ?? csrfToken)(), "If-Match": `"${revision}"` } : {},
        ...(preferences ? { body: JSON.stringify(preferences) } : {}),
      });
      return { response, value: await response.json() };
    } finally { clearTimeout(timer); }
  }
  private accept(response: Response, value: unknown): boolean {
    if (!exact(value, ["version", "layout", "prefixes", "revision", "stored", "available"])) return false;
    const shared = parseKeyboardPreferences({ version: value.version, layout: value.layout, prefixes: value.prefixes });
    if (!shared || !Number.isSafeInteger(value.revision) || (value.revision as number) < 0 || typeof value.stored !== "boolean" || typeof value.available !== "boolean" || response.headers.get("ETag") !== `"${value.revision}"`) return false;
    if (this.current.loaded && (value.revision as number) < this.current.revision) return false;
    this.publish({ shared, revision: value.revision as number, available: value.available, stored: value.stored, loaded: true, message: value.available ? "" : "Shared keyboard settings are unavailable. You can customize this device." });
    return true;
  }
  load(): Promise<void> {
    if (this.writing) { this.deferredLoad = true; return Promise.resolve(); }
    if (this.loading) return this.loading;
    this.loading = (async () => {
      try { const { response, value } = await this.request("GET"); if (!response.ok || !this.accept(response, value)) throw new Error("Invalid settings response"); }
      catch { this.publish({ loaded: true, available: false, message: "Shared keyboard settings are unavailable. Your current layout is retained." }); }
      finally { this.loading = undefined; }
    })();
    return this.loading;
  }
  async saveShared(preferences: KeyboardPreferences, expectedRevision: number): Promise<KeyboardSaveOutcome> {
    if (!parseKeyboardPreferences(preferences) || this.writing) return "unavailable";
    if (this.loading) await this.loading;
    if (this.writing) return "unavailable";
    if (this.current.revision !== expectedRevision) return "conflict";
    this.writing = true;
    try {
      const { response, value } = await this.request("PUT", preferences, expectedRevision);
      if (response.status === 412 && this.accept(response, value)) return "conflict";
      if (!response.ok || !this.accept(response, value)) return "unavailable";
      try { this.storage?.setItem(SHARED_SIGNAL, JSON.stringify({ revision: this.current.revision, at: Date.now() })); } catch { /* Polling still refreshes other views. */ }
      return "saved";
    } catch { return "unavailable"; }
    finally { this.writing = false; if (this.deferredLoad) { this.deferredLoad = false; void this.load(); } }
  }
  saveDevice(value: DeviceKeyboardPreferences): string {
    const device = parseDeviceKeyboardPreferences(value);
    if (!device) return "Invalid keyboard settings.";
    let message = "Saved for all terminals in this browser.";
    try {
      if (!this.storage) throw new Error();
      this.storage.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify(device));
      this.storage.setItem(KEYBOARD_DEFAULTS_UPGRADE_KEY, "1");
    }
    catch { message = "Applied for this page only; browser storage is unavailable."; }
    this.publish({ device, message });
    return message;
  }
  /** Keep an empty new-format record so old customization is not re-migrated. */
  useSharedDefaults(): string { return this.saveDevice(EMPTY_DEVICE); }
  watch(): void {
    if (this.stopWatching || typeof window === "undefined") return;
    const foreground = () => { if (!document.hidden) void this.load(); };
    const storage = (event: StorageEvent) => {
      if (event.key === DEVICE_KEYBOARD_PREFERENCES_KEY || event.key === null) this.publish(this.readDevice(false));
      if (event.key === SHARED_SIGNAL || event.key === null) foreground();
    };
    window.addEventListener("storage", storage);
    window.addEventListener("pageshow", foreground);
    window.addEventListener("focus", foreground);
    document.addEventListener("visibilitychange", foreground);
    const timer = window.setInterval(foreground, 30_000);
    this.stopWatching = () => { clearInterval(timer); window.removeEventListener("storage", storage); window.removeEventListener("pageshow", foreground); window.removeEventListener("focus", foreground); document.removeEventListener("visibilitychange", foreground); };
  }
  dispose(): void { this.stopWatching?.(); this.stopWatching = undefined; this.listeners.clear(); }
}
let documentService: KeyboardPreferencesService | undefined;
export function documentKeyboardPreferences(): KeyboardPreferencesService {
  if (!documentService) { documentService = new KeyboardPreferencesService(); documentService.watch(); void documentService.load(); }
  return documentService;
}
