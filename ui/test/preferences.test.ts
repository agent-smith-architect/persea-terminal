import {
  DEFAULT_PREFERENCES,
  PREFERENCES_KEY,
  createPreferencesStore,
  decodePreferences,
  type PreferencesV1,
} from "../src/preferences";

type Test = Readonly<{ name: string; run: () => void | Promise<void> }>;
const tests: Test[] = [];
const test = (name: string, run: Test["run"]) => tests.push(Object.freeze({ name, run }));
const assert: (condition: unknown, message?: string) => asserts condition = (condition, message = "assertion failed") => {
  if (!condition) throw new Error(message);
};
const equal = (actual: unknown, expected: unknown, message = "values differ") =>
  assert(Object.is(actual, expected), `${message}: ${JSON.stringify({ actual, expected })}`);
const deepEqual = (actual: unknown, expected: unknown, message = "values differ") =>
  equal(JSON.stringify(actual), JSON.stringify(expected), message);

const UNREADABLE = "Saved settings could not be read; using defaults.";
const UNAVAILABLE = "Applied for this page only; browser storage is unavailable.";
const LEGACY_KEYS = [
  "persea-terminal.composer.mode",
  "persea-terminal.composer.size",
  "persea-terminal.composer.height.compact",
  "persea-terminal.composer.height.expanded",
  "persea-terminal.keybar.collapsed",
] as const;

type StorageEvent = Readonly<{ action: "get" | "set" | "remove"; key: string; value?: string }>;

class FakeStorage {
  readonly values = new Map<string, string>();
  readonly events: StorageEvent[] = [];
  throwGetKey: string | null = null;
  throwSetKey: string | null = null;
  throwRemoveKey: string | null = null;

  constructor(entries: Readonly<Record<string, string>> = {}) {
    for (const [key, value] of Object.entries(entries)) this.values.set(key, value);
  }

  getItem(key: string): string | null {
    this.events.push({ action: "get", key });
    if (this.throwGetKey === key) throw new Error(`get blocked: ${key}`);
    return this.values.get(key) ?? null;
  }

  setItem(key: string, value: string): void {
    this.events.push({ action: "set", key, value });
    if (this.throwSetKey === key) throw new Error(`set blocked: ${key}`);
    this.values.set(key, value);
  }

  removeItem(key: string): void {
    this.events.push({ action: "remove", key });
    if (this.throwRemoveKey === key) throw new Error(`remove blocked: ${key}`);
    this.values.delete(key);
  }
}

const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;
const valid = (patch: Partial<PreferencesV1["composer"]> = {}): PreferencesV1 => ({
  version: 1,
  composer: {
    density: "standard",
    // UX-16 §16.4: code (autocorrect off) is the default.
    mode: "code",
    panelSize: "compact",
    heightPercent: {
      standard: { compact: null, expanded: null },
      compact: { compact: null, expanded: null },
    },
    ...patch,
  },
  keyBar: { collapsed: false },
});
const decoded = (raw: unknown) => decodePreferences(typeof raw === "string" ? raw : JSON.stringify(raw));
const snapshot = (storage: FakeStorage) => createPreferencesStore(storage).snapshot();
const noStatus = (status: unknown) => status === "" || status === null || status === undefined;

test("defaults_and_round_trip_are_exact_fresh_and_non_mutating", () => {
  equal(PREFERENCES_KEY, "persea-terminal.preferences.v1", "sole durable key");
  deepEqual(DEFAULT_PREFERENCES, valid(), "exact defaults");
  const source = valid({
    density: "compact",
    mode: "prose",
    panelSize: "expanded",
    heightPercent: {
      standard: { compact: 10, expanded: 70 },
      compact: { compact: 10, expanded: 55 },
    },
  });
  const before = clone(source);
  const result = decoded(source);
  deepEqual(result.preferences, before, "valid round trip");
  assert(noStatus(result.status), "valid decode must not report an error");
  deepEqual(source, before, "decode must not mutate input");
  assert(result.preferences !== source, "decode must return a fresh object");
  assert(result.preferences.composer !== source.composer, "nested output must be fresh");
});

test("closed_codec_rejects_primitives_containers_missing_types_enums_and_ranges", () => {
  const base = valid();
  const invalid: Array<readonly [string, unknown]> = [
    ["null root", null], ["boolean root", true], ["number root", 1], ["string root", "x"], ["array root", []],
    ["missing version", (({ version: _drop, ...rest }) => rest)(base)],
    ["version type", { ...base, version: "1" }], ["unknown version", { ...base, version: 2 }], ["fractional version", { ...base, version: 1.1 }],
    ["composer null", { ...base, composer: null }],
    ["density enum", valid({ density: "dense" as PreferencesV1["composer"]["density"] })],
    ["mode enum", valid({ mode: "shell" as PreferencesV1["composer"]["mode"] })],
    ["panel enum", valid({ panelSize: "large" as PreferencesV1["composer"]["panelSize"] })],
    ["heightPercent array", valid({ heightPercent: [] as unknown as PreferencesV1["composer"]["heightPercent"] })],
    ["standard branch missing", valid({ heightPercent: { compact: { compact: null, expanded: null } } as PreferencesV1["composer"]["heightPercent"] })],
    ["standard below", valid({ heightPercent: { standard: { compact: 9, expanded: null }, compact: { compact: null, expanded: null } } })],
    ["standard above", valid({ heightPercent: { standard: { compact: null, expanded: 71 }, compact: { compact: null, expanded: null } } })],
    ["standard fractional", valid({ heightPercent: { standard: { compact: 10.5, expanded: null }, compact: { compact: null, expanded: null } } })],
    ["compact below", valid({ heightPercent: { standard: { compact: null, expanded: null }, compact: { compact: 9, expanded: null } } })],
    ["compact above", valid({ heightPercent: { standard: { compact: null, expanded: null }, compact: { compact: null, expanded: 56 } } })],
    ["compact string", valid({ heightPercent: { standard: { compact: null, expanded: null }, compact: { compact: "10" as unknown as number, expanded: null } } })],
    ["keyBar null", { ...base, keyBar: null }], ["collapsed type", { ...base, keyBar: { collapsed: 0 } }],
  ];
  for (const [name, candidate] of invalid) {
    const result = decoded(candidate);
    deepEqual(result.preferences, DEFAULT_PREFERENCES, `${name} must use complete defaults`);
    equal(result.status, UNREADABLE, `${name} status`);
  }
  for (const [name, raw] of [["malformed", "{"], ["empty", ""], ["primitive JSON", "42"]] as const) {
    const result = decodePreferences(raw);
    deepEqual(result.preferences, DEFAULT_PREFERENCES, `${name} defaults`);
    equal(result.status, UNREADABLE, `${name} status`);
  }
  for (const candidate of [
    valid({ heightPercent: { standard: { compact: 10, expanded: 70 }, compact: { compact: 10, expanded: 55 } } }),
    valid({ heightPercent: { standard: { compact: null, expanded: null }, compact: { compact: null, expanded: null } } }),
  ]) assert(noStatus(decoded(candidate).status), "valid boundaries/null controls must pass");
});

test("recursive_extra_keys_are_rejected_at_every_object_depth", () => {
  const candidates = [
    { ...valid(), extra: true },
    { ...valid(), composer: { ...valid().composer, extra: true } },
    { ...valid(), composer: { ...valid().composer, heightPercent: { ...valid().composer.heightPercent, extra: true } } },
    { ...valid(), composer: { ...valid().composer, heightPercent: { ...valid().composer.heightPercent, standard: { ...valid().composer.heightPercent.standard, extra: true } } } },
    { ...valid(), composer: { ...valid().composer, heightPercent: { ...valid().composer.heightPercent, compact: { ...valid().composer.heightPercent.compact, extra: true } } } },
    { ...valid(), keyBar: { ...valid().keyBar, extra: true } },
  ];
  for (const candidate of candidates) {
    const result = decoded(candidate);
    deepEqual(result.preferences, DEFAULT_PREFERENCES, "recursive extra must reject whole object");
    equal(result.status, UNREADABLE, "recursive extra status");
  }
  assert(noStatus(decoded(valid()).status), "exact adjacent object must pass");
});

test("absent_v1_without_legacy_is_read_only_construction", () => {
  const storage = new FakeStorage();
  const result = snapshot(storage);
  deepEqual(result.preferences, DEFAULT_PREFERENCES, "absent defaults");
  assert(noStatus(result.status), "absent clean storage status");
  equal(storage.events.filter((event) => event.action === "set").length, 0, "construction must not write defaults");
  equal(storage.events.filter((event) => event.action === "remove").length, 0, "construction must not remove absent legacy");
});

test("complete_legacy_migration_writes_v1_then_removes_all_five", () => {
  const storage = new FakeStorage({
    "persea-terminal.composer.mode": "code",
    "persea-terminal.composer.size": "expanded",
    "persea-terminal.composer.height.compact": "25",
    "persea-terminal.composer.height.expanded": "45",
    "persea-terminal.keybar.collapsed": "1",
  });
  const result = snapshot(storage);
  deepEqual(result.preferences, {
    ...valid({ mode: "code", panelSize: "expanded", heightPercent: {
      standard: { compact: 25, expanded: 45 }, compact: { compact: null, expanded: null },
    } }),
    keyBar: { collapsed: true },
  }, "complete migration");
  assert(noStatus(result.status), "successful migration status");
  const writeIndex = storage.events.findIndex((event) => event.action === "set" && event.key === PREFERENCES_KEY);
  assert(writeIndex >= 0, "complete migration must write v1");
  const removals = storage.events.filter((event) => event.action === "remove");
  equal(removals.length, 5, "all legacy keys removed");
  assert(removals.every((event) => storage.events.indexOf(event) > writeIndex), "cleanup must follow successful write");
  for (const key of LEGACY_KEYS) equal(storage.values.has(key), false, `${key} removed`);
  deepEqual(JSON.parse(storage.values.get(PREFERENCES_KEY) ?? "null"), result.preferences, "complete v1 serialization");
});

test("partial_legacy_migration_imports_only_individually_valid_values", () => {
  const storage = new FakeStorage({
    "persea-terminal.composer.mode": "invalid",
    "persea-terminal.composer.size": "expanded",
    "persea-terminal.composer.height.compact": "70",
    "persea-terminal.composer.height.expanded": "70.5",
    "persea-terminal.keybar.collapsed": "0",
  });
  const result = snapshot(storage);
  deepEqual(result.preferences, {
    ...valid({ panelSize: "expanded", heightPercent: {
      standard: { compact: 70, expanded: null }, compact: { compact: null, expanded: null },
    } }),
    keyBar: { collapsed: false },
  }, "partial migration");
  for (const key of LEGACY_KEYS) equal(storage.values.has(key), false, `${key} cleaned after partial migration`);
});

test("failed_migration_write_keeps_derived_memory_status_and_all_legacy_bytes", () => {
  const entries = {
    "persea-terminal.composer.mode": "code",
    "persea-terminal.composer.size": "expanded",
    "persea-terminal.composer.height.compact": "25",
    "persea-terminal.composer.height.expanded": "45",
    "persea-terminal.keybar.collapsed": "1",
  } as const;
  const storage = new FakeStorage(entries);
  storage.throwSetKey = PREFERENCES_KEY;
  const result = snapshot(storage);
  equal(result.preferences.composer.mode, "code", "derived mode remains in memory");
  equal(result.preferences.composer.panelSize, "expanded", "derived size remains in memory");
  equal(result.status, UNAVAILABLE, "failed migration status");
  equal(storage.events.filter((event) => event.action === "set" && event.key === PREFERENCES_KEY).length, 1, "v1 write attempted once");
  equal(storage.events.filter((event) => event.action === "remove").length, 0, "no cleanup after failed write");
  for (const key of LEGACY_KEYS) equal(storage.values.get(key), entries[key], `${key} bytes preserved`);
});

test("invalid_existing_v1_never_reads_or_revives_legacy", () => {
  const storage = new FakeStorage({
    [PREFERENCES_KEY]: JSON.stringify({ ...valid(), composer: { ...valid().composer, density: "broken" } }),
    "persea-terminal.composer.mode": "code",
    "persea-terminal.composer.size": "expanded",
    "persea-terminal.composer.height.compact": "70",
    "persea-terminal.composer.height.expanded": "70",
    "persea-terminal.keybar.collapsed": "1",
  });
  const result = snapshot(storage);
  deepEqual(result.preferences, DEFAULT_PREFERENCES, "invalid v1 complete fallback");
  equal(result.status, UNREADABLE, "invalid v1 status");
  equal(storage.events.filter((event) => LEGACY_KEYS.includes(event.key as typeof LEGACY_KEYS[number])).length, 0, "legacy keys must not be read or removed");
  equal(storage.events.filter((event) => event.action === "set").length, 0, "invalid v1 construction must not write");
});

test("read_write_and_remove_failures_are_honest_and_non_blocking", () => {
  const readFailure = new FakeStorage();
  readFailure.throwGetKey = PREFERENCES_KEY;
  const readResult = snapshot(readFailure);
  deepEqual(readResult.preferences, DEFAULT_PREFERENCES, "read failure defaults");
  equal(readResult.status, UNAVAILABLE, "read failure status");

  const writeFailure = new FakeStorage({ [PREFERENCES_KEY]: JSON.stringify(valid()) });
  writeFailure.throwSetKey = PREFERENCES_KEY;
  const store = createPreferencesStore(writeFailure);
  const compact = valid({ density: "compact" });
  const failed = store.update(compact);
  equal(failed.preferences.composer.density, "compact", "failed write applies in memory");
  equal(failed.status, UNAVAILABLE, "failed write status");
  writeFailure.throwSetKey = null;
  const recovered = store.update(valid({ density: "standard", mode: "code" }));
  equal(recovered.preferences.composer.mode, "code", "later explicit update applies");
  assert(noStatus(recovered.status), "later successful explicit update may clear status");

  const removeFailure = new FakeStorage({
    "persea-terminal.composer.mode": "code",
    "persea-terminal.composer.size": "compact",
    "persea-terminal.composer.height.compact": "25",
    "persea-terminal.composer.height.expanded": "45",
    "persea-terminal.keybar.collapsed": "0",
  });
  removeFailure.throwRemoveKey = LEGACY_KEYS[0];
  const removeResult = snapshot(removeFailure);
  equal(removeResult.preferences.composer.mode, "code", "remove failure keeps migrated memory");
  equal(removeResult.status, UNAVAILABLE, "remove failure status");
  assert(removeFailure.values.has(PREFERENCES_KEY), "v1 write succeeds before cleanup failure");
});

test("updates_write_one_complete_closed_object_and_exclude_sensitive_data", () => {
  const sentinel = "SECRET-draft-handle-realm-server-session-label-terminal-capability-credential-image-path";
  const storage = new FakeStorage({ [PREFERENCES_KEY]: JSON.stringify(valid()) });
  const store = createPreferencesStore(storage);
  const before = valid({ density: "compact", mode: "code", panelSize: "expanded" });
  const next = clone(before);
  const result = store.update(next);
  deepEqual(result.preferences, before, "explicit update snapshot");
  deepEqual(next, before, "update must not mutate caller object");
  const writes = storage.events.filter((event) => event.action === "set" && event.key === PREFERENCES_KEY);
  equal(writes.length, 1, "one explicit full-object write");
  assert(!(writes[0]?.value ?? "").includes(sentinel), "sensitive sentinel excluded");
  deepEqual(Object.keys(JSON.parse(writes[0]?.value ?? "{}")).sort(), ["composer", "keyBar", "version"], "closed root serialization");
  const poisoned = decoded({ ...before, draft: sentinel, imagePath: sentinel });
  deepEqual(poisoned.preferences, DEFAULT_PREFERENCES, "sensitive extra fields reject whole object");
  equal(poisoned.status, UNREADABLE, "sensitive extra status");
});

async function main(): Promise<void> {
  const outcomes: Array<{ name: string; status: "PASS" | "FAIL"; error?: string }> = [];
  for (const entry of tests) {
    try { await entry.run(); outcomes.push({ name: entry.name, status: "PASS" }); }
    catch (error) { outcomes.push({ name: entry.name, status: "FAIL", error: error instanceof Error ? error.stack ?? error.message : String(error) }); }
  }
  const failed = outcomes.filter((outcome) => outcome.status === "FAIL");
  console.log(JSON.stringify({ schema: "persea-terminal-preferences-oracle-v1", count: outcomes.length, outcomes }, null, 2));
  if (failed.length > 0) throw new Error(`${failed.length} deterministic preference case(s) failed`);
}

void main();
