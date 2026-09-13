import {
  DEFAULT_KEYBOARD_PREFERENCES,
  DEVICE_KEYBOARD_PREFERENCES_KEY,
  KEYBOARD_DEFAULTS_UPGRADE_KEY,
  KeyboardPreferencesService,
  canonicalKeyboardAction,
  effectiveKeyboardPreferences,
  migrateLegacyKeyboardPreferences,
  parseDeviceKeyboardPreferences,
  parseKeyboardPreferences,
  type KeyboardPreferences,
} from "../src/keyboard_preferences";
import { KEY_PREFERENCES_KEY } from "../src/terminal_actions";

declare const process: { exitCode: number };

function equal(actual: unknown, expected: unknown, message: string): void {
  if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`);
}
function deepEqual(actual: unknown, expected: unknown, message: string): void {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
}
function ok(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}
class MemoryStorage implements Pick<Storage, "getItem" | "setItem" | "removeItem"> {
  readonly values = new Map<string, string>();
  failWrites = false;
  getItem(key: string): string | null { return this.values.get(key) ?? null; }
  setItem(key: string, value: string): void { if (this.failWrites) throw new Error("quota"); this.values.set(key, value); }
  removeItem(key: string): void { this.values.delete(key); }
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const tick = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

// These are backend contract values, independent of the frontend defaults.
const FACTORY: KeyboardPreferences = {
  version: 1,
  layout: {
    bar: ["key:escape:0", "key:tab:0", "modifier:ctrl", "key:arrow-left:0", "key:arrow-down:0", "key:arrow-up:0", "key:arrow-right:0"],
    favorites: ["modifier:alt", "key:c:1", "key:d:1", "key:enter:0", "key:home:0", "key:end:0", "key:page-up:0", "key:page-down:0", "key:f1:0"],
  },
  prefixes: { tmux: "key:b:1", screen: "key:a:1" },
};
const choice = (key: string): KeyboardPreferences => ({ version: 1, layout: { bar: [`key:${key}:0`, "modifier:alt"], favorites: [`key:${key}:7`] }, prefixes: { "My tmux": "key:b:1" } });
function response(preferences: KeyboardPreferences, revision: number, status = 200, available = true): Response {
  return new Response(JSON.stringify({ ...preferences, revision, stored: revision > 0, available }), {
    status, headers: { "Content-Type": "application/json", ETag: `"${revision}"`, "Cache-Control": "no-store" },
  });
}

function server(initial = FACTORY, initialRevision = 0) {
  let current = structuredClone(initial);
  let revision = initialRevision;
  const calls: Array<{ url: string; init: RequestInit }> = [];
  return {
    calls,
    current: () => ({ preferences: structuredClone(current), revision }),
    set(preferences: KeyboardPreferences) { current = structuredClone(preferences); revision += 1; },
    fetch: async (url: string, init: RequestInit): Promise<Response> => {
      calls.push({ url, init });
      if (url !== "/api/keyboard-preferences") return new Response("not found", { status: 404 });
      if (init.method === "GET") return response(current, revision);
      const headers = new Headers(init.headers);
      if (headers.get("X-Persea-CSRF") !== "csrf-token") return new Response("forbidden", { status: 403 });
      if (headers.get("Content-Type") !== "application/json") return new Response("invalid request", { status: 400 });
      if (headers.get("If-Match") !== `"${revision}"`) return response(current, revision, 412);
      current = JSON.parse(String(init.body)) as KeyboardPreferences;
      revision += 1;
      return response(current, revision);
    },
  };
}

const cases: Array<{ name: string; run(): void | Promise<void> }> = [];
const test = (name: string, run: () => void | Promise<void>) => cases.push({ name, run });

test("factory and canonical structural grammar agree with backend", () => {
  deepEqual(DEFAULT_KEYBOARD_PREFERENCES, FACTORY, "factory wire values");
  for (let code = 32; code <= 126; code += 1) {
    for (let mask = 0; mask <= 7; mask += 1) {
      const id = `key:${encodeURIComponent(String.fromCharCode(code))}:${mask}`;
      ok(parseKeyboardPreferences({ ...FACTORY, layout: { bar: [], favorites: [id] } }), `ASCII chord rejected: ${id}`);
    }
  }
  for (const id of ["key:escape:7", "key:insert:7", "key:tab:7", "key:f12:7", "sequence:Missing%20Prefix:key:%3A:7"]) {
    ok(parseKeyboardPreferences({ ...FACTORY, layout: { bar: [], favorites: [id] }, prefixes: {} }), `structural intent rejected: ${id}`);
  }
  for (const id of ["key:%61:0", "key:%2b:0", "key:+:0", "key: :0", "key:a:8", "key:a:01", "key:%C3%A9:0", "key:%00:0", "key:f13:0", "key:Escape:0", "key:ab:0", "tmux:n", "modifier:meta", "local:copy", "sequence:A+Name:key:a:0", "sequence:constructor:key:a:0", "sequence:x:sequence:y:key:a:0"]) {
    equal(parseKeyboardPreferences({ ...FACTORY, layout: { bar: [], favorites: [id] } }), undefined, `noncanonical favorite accepted: ${id}`);
  }
  for (const id of ["modifier:ctrl", "modifier:alt", "modifier:shift"]) {
    ok(parseKeyboardPreferences({ ...FACTORY, layout: { bar: [id], favorites: [] } }), `bar modifier refused: ${id}`);
    ok(parseKeyboardPreferences({ ...FACTORY, layout: { bar: [], favorites: [id] } }), `favorite modifier refused: ${id}`);
  }
  equal(canonicalKeyboardAction("tmux:n"), "sequence:tmux:key:n:0", "legacy canonicalization");
});

test("prefix names and closed schemas agree with backend", () => {
  ok(parseKeyboardPreferences({ ...FACTORY, prefixes: { "My tmux": "key:b:1", T: "key:A:7" } }), "mixed-case/spaced prefix refused");
  for (const prefixes of [
    { tmux: "key:b:1", TMUX: "key:a:1" }, { constructor: "key:a:0" }, { prototype: "key:a:0" },
    JSON.parse('{"__proto__":"key:a:0"}'), { "1bad": "key:a:0" }, { ["a".repeat(33)]: "key:a:0" },
    { a: "modifier:ctrl" }, { a: "sequence:a:key:a:0" }, { a: "key:%61:0" },
  ]) {
    equal(parseKeyboardPreferences({ ...FACTORY, prefixes }), undefined, "invalid prefix map accepted");
    equal(parseDeviceKeyboardPreferences({ version: 1, prefixes }), undefined, "invalid device prefix map accepted");
  }
  for (const value of [
    null, [], { ...FACTORY, version: 2 }, { ...FACTORY, revision: 0 }, { ...FACTORY, LAYOUT: FACTORY.layout },
    { ...FACTORY, layout: { bar: [] } }, { ...FACTORY, layout: { bar: [], favorites: null } },
    { ...FACTORY, layout: { bar: [], favorites: [], extra: true } }, { ...FACTORY, prefixes: null },
    { ...FACTORY, layout: { bar: ["key:a:0", "key:a:0"], favorites: [] } },
    { ...FACTORY, layout: { bar: [], favorites: Array(101).fill("key:a:0") } },
    { ...FACTORY, prefixes: Object.fromEntries(Array.from({ length: 21 }, (_, i) => [`P${i}`, "key:a:0"])) },
  ]) equal(parseKeyboardPreferences(value), undefined, "open/invalid shared schema accepted");
  equal(parseDeviceKeyboardPreferences({ version: 1, layout: null }), undefined, "null means inherit unexpectedly");
});

test("complete sections inherit; explicit empty sections replace", () => {
  const shared = choice("x");
  deepEqual(effectiveKeyboardPreferences(shared, { version: 1 }), shared, "default inheritance");
  const emptyLayout = { bar: [], favorites: [] };
  const layoutOnly = effectiveKeyboardPreferences(shared, { version: 1, layout: emptyLayout });
  deepEqual(layoutOnly.layout, emptyLayout, "empty layout must replace");
  deepEqual(layoutOnly.prefixes, shared.prefixes, "prefixes must inherit independently");
  const prefixOnly = effectiveKeyboardPreferences(shared, { version: 1, prefixes: {} });
  deepEqual(prefixOnly.layout, shared.layout, "layout must inherit independently");
  deepEqual(prefixOnly.prefixes, {}, "empty prefixes must replace");
  const mutable = structuredClone(shared) as { version: 1; layout: { bar: string[]; favorites: string[] }; prefixes: Record<string, string> };
  const parsed = parseKeyboardPreferences(mutable)!;
  mutable.layout.bar.push("key:q:0"); mutable.prefixes.extra = "key:q:0";
  deepEqual(parsed, shared, "parsed authority aliases caller objects");
  ok(Object.isFrozen(parsed.layout.bar) && Object.isFrozen(parsed.prefixes), "nested snapshots are mutable");
});

const LEGACY = { version: 1, favorites: ["tmux:n", "screen:p", "key:c:1"], pinned: ["tmux:n"], prefixes: { tmux: "key:b:1", screen: "key:a:1" } };
test("legacy migration is local, canonical, and preserves rollback bytes", () => {
  const storage = new MemoryStorage();
  const legacyBytes = JSON.stringify(LEGACY);
  storage.setItem(KEY_PREFERENCES_KEY, legacyBytes);
  const remote = server();
  const service = new KeyboardPreferencesService({ storage, fetch: remote.fetch });
  deepEqual(service.snapshot().effective.layout.favorites, ["modifier:alt", "sequence:tmux:key:n:0", "sequence:screen:key:p:0", "key:c:1"], "legacy favorites plus current Alt default");
  ok(service.snapshot().effective.layout.bar.includes("sequence:tmux:key:n:0"), "pinned legacy action lost");
  equal(remote.calls.length, 0, "migration uploaded or fetched");
  equal(storage.getItem(KEY_PREFERENCES_KEY), legacyBytes, "legacy rollback data changed");
  ok(storage.getItem(DEVICE_KEYBOARD_PREFERENCES_KEY), "migration marker missing");
  service.useSharedDefaults();
  const reopened = new KeyboardPreferencesService({ storage, fetch: remote.fetch });
  deepEqual(reopened.snapshot().device, { version: 1 }, "reset remigrated legacy overrides");
  equal(storage.getItem(KEY_PREFERENCES_KEY), legacyBytes, "reset removed rollback data");
  equal(remote.calls.length, 0, "device reset uploaded");
});

test("existing device Favorites gain Alt once without changing other preferences", () => {
  const storage = new MemoryStorage(), remote = server();
  const old = { version: 1 as const, layout: { bar: ["key:tab:0", "modifier:ctrl"], favorites: ["key:f3:0", "key:c:1"] }, prefixes: { Mine: "key:a:1" } };
  storage.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify(old));
  const service = new KeyboardPreferencesService({ storage, fetch: remote.fetch });
  deepEqual(service.snapshot().device, { ...old, layout: { ...old.layout, favorites: ["modifier:alt", ...old.layout.favorites] } }, "upgrade lost saved choices");
  equal(storage.getItem(KEYBOARD_DEFAULTS_UPGRADE_KEY), "1", "upgrade not recorded");
  equal(remote.calls.length, 0, "local upgrade changed shared authority");
  const reloaded = new KeyboardPreferencesService({ storage, fetch: remote.fetch });
  equal(reloaded.snapshot().effective.layout.favorites.filter((id) => id === "modifier:alt").length, 1, "reload duplicates Alt");
  service.saveDevice(old);
  const removed = new KeyboardPreferencesService({ storage, fetch: remote.fetch });
  deepEqual(removed.snapshot().device, old, "explicit later removal was undone");
});

test("the Alt upgrade preserves empty and full layouts and existing Alt ordering", () => {
  for (const favorites of [[], ["key:c:1", "modifier:alt"], Array.from({ length: 100 }, (_, i) => `key:${String.fromCharCode(97 + i % 26)}:${Math.floor(i / 26)}`)]) {
    const storage = new MemoryStorage(), old = { version: 1, layout: { bar: [], favorites } };
    storage.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify(old));
    const service = new KeyboardPreferencesService({ storage, fetch: server().fetch });
    deepEqual(service.snapshot().device, old, "upgrade changed a deliberate boundary layout");
  }
});

test("Alt remains usable for this page if saving its upgrade fails", () => {
  const storage = new MemoryStorage(), old = { version: 1, layout: { bar: ["key:tab:0"], favorites: ["key:f2:0"] } };
  storage.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify(old)); storage.failWrites = true;
  const service = new KeyboardPreferencesService({ storage, fetch: server().fetch });
  deepEqual(service.snapshot().effective.layout.favorites, ["modifier:alt", "key:f2:0"], "failed upgrade write lost the usable default");
  deepEqual(service.snapshot().effective.layout.bar, old.layout.bar, "failed upgrade discarded custom bar");
  equal(storage.getItem(DEVICE_KEYBOARD_PREFERENCES_KEY), JSON.stringify(old), "failed write corrupted saved layout");
  ok(service.snapshot().message.includes("this page"), "page-only migration not explained");
});

test("a failed upgrade-marker read does not discard a valid device layout", () => {
  const storage = new MemoryStorage(), old = { version: 1, layout: { bar: ["key:tab:0"], favorites: ["key:f2:0"] } };
  storage.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify(old));
  const service = new KeyboardPreferencesService({ storage: { ...storage, getItem: (key) => { if (key === KEYBOARD_DEFAULTS_UPGRADE_KEY) throw Error("unavailable"); return storage.getItem(key); }, setItem: (key, value) => storage.setItem(key, value), removeItem: (key) => storage.removeItem(key) }, fetch: server().fetch });
  deepEqual(service.snapshot().device, old, "marker read failure replaced valid settings");
});

test("migration survives unavailable browser writes for this page", () => {
  const storage = new MemoryStorage();
  storage.setItem(KEY_PREFERENCES_KEY, JSON.stringify(LEGACY));
  storage.failWrites = true;
  const service = new KeyboardPreferencesService({ storage, fetch: server().fetch });
  deepEqual(service.snapshot().device, migrateLegacyKeyboardPreferences(LEGACY), "valid legacy customization dropped on failed migration write");
  ok(service.snapshot().message.length > 0, "migration write failure not reported");
  const device = { version: 1 as const, layout: { bar: [], favorites: [] } };
  service.saveDevice(device);
  deepEqual(service.snapshot().effective.layout, device.layout, "page-only override dropped");
  equal(storage.getItem(KEY_PREFERENCES_KEY), JSON.stringify(LEGACY), "rollback bytes changed on write failure");
});

test("a full legacy pin list survives factory-control insertion", () => {
  const named = ["escape", "tab", "enter", "backspace", "insert", "delete", "arrow-left", "arrow-down", "arrow-up", "arrow-right", "home", "end", "page-up", "page-down", ...Array.from({ length: 12 }, (_, i) => `f${i + 1}`)];
  const plain = [..."abcdefghijklmnopqrstuvwxyz0123456789 `~!@#$%^&*()-_=+[{]}\\|;:'\",<.>/?", ...named].map((key) => `key:${encodeURIComponent(key)}:0`);
  const pins = [...new Set([...plain, ...[..."abcde"].map((key) => `key:${key}:2`)])];
  equal(pins.length, 100, "legacy boundary fixture");
  const migrated = migrateLegacyKeyboardPreferences({ version: 1, favorites: pins, pinned: pins, prefixes: { tmux: "key:b:1" } });
  ok(migrated?.layout, "full valid legacy record was discarded");
  deepEqual(migrated.layout.favorites, pins, "full legacy favorites lost");
  ok(migrated.layout.bar.length <= 100, "migration exceeds new bar cap");
  for (const pin of pins) ok(migrated.layout.bar.includes(pin), `explicit legacy pin lost: ${pin}`);
});

test("legacy case-fold prefix collisions retain both sequence meanings", () => {
  const old = {
    version: 1, favorites: ["sequence:TMUX:key:n:0", "tmux:n"], pinned: ["sequence:TMUX:key:n:0"],
    prefixes: { tmux: "key:b:1", TMUX: "key:a:1" },
  };
  const migrated = migrateLegacyKeyboardPreferences(old);
  ok(migrated?.layout && migrated.prefixes, "old valid case-fold-distinct prefix map was discarded");
  equal(Object.keys(migrated.prefixes).length, 2, "one prefix definition was silently dropped");
  equal(new Set(Object.keys(migrated.prefixes).map((name) => name.toLowerCase())).size, 2, "migration still violates new duplicate-name rule");
  equal(migrated.layout.favorites.length, 2, "collision migration lost a favorite");
  for (const [index, expectedPrefixKey] of ["key:a:1", "key:b:1"].entries()) {
    const id = migrated.layout.favorites[index];
    ok(id.startsWith("sequence:"), "legacy sequence was not canonicalized");
    const provider = decodeURIComponent(id.split(":")[1]);
    equal(migrated.prefixes[provider], expectedPrefixKey, "migration silently changed a sequence's prefix bytes");
  }
  ok(migrated.layout.bar.includes(migrated.layout.favorites[0]), "renamed sequence pin lost");
  ok(parseDeviceKeyboardPreferences(migrated), "migration output is invalid new device schema");
});

test("backend request headers and CAS protect two independent clients", async () => {
  const remote = server();
  const one = new KeyboardPreferencesService({ storage: new MemoryStorage(), fetch: remote.fetch, csrf: () => "csrf-token" });
  const two = new KeyboardPreferencesService({ storage: new MemoryStorage(), fetch: remote.fetch, csrf: () => "csrf-token" });
  await Promise.all([one.load(), two.load()]);
  const firstDraft = choice("a"), secondDraft = choice("b");
  const originalSecond = JSON.stringify(secondDraft);
  two.saveDevice({ version: 1, prefixes: {} });
  equal(await one.saveShared(firstDraft, 0), "saved", "correct CSRF header required by backend");
  equal(await two.saveShared(secondDraft, 0), "conflict", "second writer overwrote stale authority");
  deepEqual(remote.current().preferences, firstDraft, "CAS changed winner's values");
  deepEqual(two.snapshot().shared, firstDraft, "412 did not publish latest authority");
  deepEqual(two.snapshot().effective.prefixes, {}, "412 lost browser override");
  equal(JSON.stringify(secondDraft), originalSecond, "conflict mutated caller draft");
  const put = remote.calls.find((call) => call.init.method === "PUT")!;
  equal(put.init.credentials, "same-origin", "request credentials");
  equal(put.init.cache, "no-store", "request cache");
  equal(new Headers(put.init.headers).get("X-Persea-CSRF"), "csrf-token", "CSRF header name");
  deepEqual(JSON.parse(String(put.init.body)), firstDraft, "PUT value envelope changed");
  equal(await two.saveShared(secondDraft, 1), "saved", "explicit retry after reviewing conflict");
});

test("a refresh completing before save invalidates the stale edit revision", async () => {
  const held = deferred<Response>();
  let gets = 0, puts = 0;
  const service = new KeyboardPreferencesService({ storage: null, csrf: () => "csrf-token", fetch: async (_url, init) => {
    if (init.method === "PUT") { puts += 1; return response(choice("b"), 3); }
    gets += 1;
    return gets === 1 ? response(choice("a"), 1) : held.promise;
  } });
  await service.load();
  const refreshing = service.load();
  const saving = service.saveShared(choice("b"), 1);
  held.resolve(response(choice("c"), 2));
  await refreshing;
  equal(await saving, "conflict", "late refresh did not invalidate stale draft revision");
  equal(puts, 0, "stale draft was uploaded");
  equal(service.snapshot().revision, 2, "late refresh authority lost");
});

test("refresh requested during a write waits for its settlement", async () => {
  const held = deferred<Response>();
  let gets = 0;
  const service = new KeyboardPreferencesService({ storage: null, csrf: () => "csrf-token", fetch: async (_url, init) => {
    if (init.method === "PUT") return held.promise;
    gets += 1; return response(gets === 1 ? choice("a") : choice("b"), gets === 1 ? 1 : 2);
  } });
  await service.load();
  const saving = service.saveShared(choice("b"), 1);
  await service.load();
  equal(gets, 1, "refresh overtook pending PUT");
  held.resolve(response(choice("b"), 2));
  equal(await saving, "saved", "write failed");
  await tick();
  equal(gets, 2, "deferred refresh never executed");
  equal(service.snapshot().revision, 2, "deferred read regressed authority");
});

test("two saves waiting behind a read never issue concurrent writes", async () => {
  const reading = deferred<Response>(), writing = deferred<Response>();
  let puts = 0;
  const service = new KeyboardPreferencesService({ storage: null, csrf: () => "csrf-token", fetch: async (_url, init) => {
    if (init.method === "GET") return reading.promise;
    puts += 1; return writing.promise;
  } });
  const loading = service.load();
  const one = service.saveShared(choice("a"), 1);
  const two = service.saveShared(choice("b"), 1);
  reading.resolve(response(FACTORY, 1));
  await loading;
  await tick();
  const issued = puts;
  writing.resolve(response(choice("a"), 2));
  await Promise.all([one, two]);
  equal(issued, 1, "both waiters passed the pre-await writing check");
});

test("failed transports and malformed responses retain authority, overrides and caller draft", async () => {
  for (const failure of ["reject", "503", "invalid json", "wrong etag", "stale revision"]) {
    let failing = false;
    const service = new KeyboardPreferencesService({ storage: new MemoryStorage(), csrf: () => "csrf-token", fetch: async () => {
      if (!failing) return response(choice("a"), 5);
      if (failure === "reject") throw new Error("offline");
      if (failure === "503") return new Response("unavailable", { status: 503 });
      if (failure === "invalid json") return new Response("<html>old release</html>");
      if (failure === "wrong etag") return new Response(JSON.stringify({ ...choice("b"), revision: 6, stored: true, available: true }), { headers: { ETag: '"7"' } });
      return response(choice("b"), 4);
    } });
    await service.load();
    service.saveDevice({ version: 1, prefixes: {} });
    const draft = choice("c"), before = JSON.stringify(draft);
    failing = true;
    equal(await service.saveShared(draft, 5), "unavailable", `${failure}: outcome`);
    equal(service.snapshot().revision, 5, `${failure}: revision lost`);
    deepEqual(service.snapshot().shared, choice("a"), `${failure}: authority lost`);
    deepEqual(service.snapshot().device, { version: 1, prefixes: {} }, `${failure}: override lost`);
    equal(JSON.stringify(draft), before, `${failure}: caller draft changed`);
    await service.load();
    deepEqual(service.snapshot().shared, choice("a"), `${failure}: failed refresh lost authority`);
  }
});

test("unavailable fresh backend is distinct from a configured default record", async () => {
  const service = new KeyboardPreferencesService({ storage: null, fetch: async () => response(FACTORY, 0, 200, false) });
  await service.load();
  equal(service.snapshot().loaded, true, "unavailable record not loaded");
  equal(service.snapshot().available, false, "unavailable record advertised writable");
  equal(service.snapshot().stored, false, "unavailable defaults advertised stored");
  service.saveDevice({ version: 1, layout: { bar: [], favorites: [] } });
  deepEqual(service.snapshot().effective.layout, { bar: [], favorites: [] }, "offline device override unavailable");
});

test("timeout aborts the request and allows a subsequent save", async () => {
  let hang = false;
  const service = new KeyboardPreferencesService({ storage: null, timeoutMs: 5, csrf: () => "csrf-token", fetch: async (_url, init) => {
    if (!hang) return response(FACTORY, init.method === "PUT" ? 1 : 0);
    return new Promise<Response>((_resolve, reject) => init.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true }));
  } });
  await service.load(); hang = true;
  equal(await service.saveShared(choice("a"), 0), "unavailable", "timeout outcome");
  hang = false;
  equal(await service.saveShared(choice("a"), 0), "saved", "timeout left writing flag locked");
});

test("storage signals, foreground and visible polling refresh the document service", async () => {
  const oldWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
  const oldDocument = Object.getOwnPropertyDescriptor(globalThis, "document");
  const browser = new EventTarget();
  const documentEvents = Object.assign(new EventTarget(), { hidden: false });
  const intervals: Array<() => void> = [];
  const handles: ReturnType<typeof setInterval>[] = [];
  Object.assign(browser, { setInterval: (callback: () => void, delay: number) => {
    equal(delay, 30_000, "polling interval"); intervals.push(callback);
    const handle = setInterval(() => {}, 60_000); handles.push(handle); return handle;
  } });
  Object.defineProperty(globalThis, "window", { configurable: true, value: browser });
  Object.defineProperty(globalThis, "document", { configurable: true, value: documentEvents });
  const storage = new MemoryStorage(), remote = server();
  const service = new KeyboardPreferencesService({ storage, fetch: remote.fetch, csrf: () => "csrf-token" });
  const signal = (key: string | null) => browser.dispatchEvent(Object.assign(new Event("storage"), { key }));
  try {
    await service.load(); service.watch(); service.watch();
    equal(intervals.length, 1, "watch installed duplicate polling");
    let notifications = 0; const unsubscribe = service.subscribe(() => { notifications += 1; });
    storage.setItem(DEVICE_KEYBOARD_PREFERENCES_KEY, JSON.stringify({ version: 1, prefixes: {} }));
    signal(DEVICE_KEYBOARD_PREFERENCES_KEY);
    deepEqual(service.snapshot().effective.prefixes, {}, "device storage event not applied");
    equal(remote.calls.length, 1, "device event uploaded or fetched shared settings");
    remote.set(choice("b"));
    signal("persea-terminal.keyboard-shared-change.v1"); await tick();
    equal(service.snapshot().revision, 1, "shared storage signal did not GET authority");
    remote.set(choice("c"));
    browser.dispatchEvent(new Event("focus")); await tick();
    equal(service.snapshot().revision, 2, "visible window focus did not refresh");
    documentEvents.hidden = true;
    const calls = remote.calls.length; remote.set(choice("d"));
    intervals[0](); await tick();
    equal(remote.calls.length, calls, "hidden document polled");
    documentEvents.hidden = false;
    documentEvents.dispatchEvent(new Event("visibilitychange")); await tick();
    equal(service.snapshot().revision, 3, "foreground visibility did not refresh");
    remote.set(choice("e")); intervals[0](); await tick();
    equal(service.snapshot().revision, 4, "visible polling did not refresh remote-device update");
    storage.removeItem(DEVICE_KEYBOARD_PREFERENCES_KEY); signal(null); await tick();
    deepEqual(service.snapshot().device, { version: 1 }, "storage clear did not restore inheritance");
    ok(notifications > 0, "subscribers were not notified");
    unsubscribe(); const count = notifications;
    service.saveDevice({ version: 1, prefixes: {} });
    equal(notifications, count, "unsubscribe failed");
    service.dispose(); const disposedCalls = remote.calls.length;
    browser.dispatchEvent(new Event("focus")); signal("persea-terminal.keyboard-shared-change.v1"); await tick();
    equal(remote.calls.length, disposedCalls, "disposed watcher fetched");
  } finally {
    service.dispose(); for (const handle of handles) clearInterval(handle);
    if (oldWindow) Object.defineProperty(globalThis, "window", oldWindow); else Reflect.deleteProperty(globalThis, "window");
    if (oldDocument) Object.defineProperty(globalThis, "document", oldDocument); else Reflect.deleteProperty(globalThis, "document");
  }
});

async function main(): Promise<void> {
  let failed = 0;
  for (const entry of cases) {
    try { await entry.run(); console.log(`PASS ${entry.name}`); }
    catch (error) { failed += 1; console.error(`FAIL ${entry.name}: ${String(error)}`); }
  }
  console.log(`Keyboard preferences: ${cases.length - failed}/${cases.length} passed`);
  if (failed) process.exitCode = 1;
}
const keepAlive = setInterval(() => {}, 1_000);
void main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(() => clearInterval(keepAlive));
