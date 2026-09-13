export const PREFERENCES_KEY = "persea-terminal.preferences.v1";

const UNREADABLE_STATUS = "Saved settings could not be read; using defaults.";
const UNAVAILABLE_STATUS = "Applied for this page only; browser storage is unavailable.";

const LEGACY_KEYS = Object.freeze({
  mode: "persea-terminal.composer.mode",
  panelSize: "persea-terminal.composer.size",
  compactHeight: "persea-terminal.composer.height.compact",
  expandedHeight: "persea-terminal.composer.height.expanded",
  keyBarCollapsed: "persea-terminal.keybar.collapsed",
});

export type ComposerDensity = "standard" | "compact";
export type ComposerMode = "prose" | "code";
export type ComposerPanelSize = "compact" | "expanded";
export type ComposerHeightTargets = Readonly<Record<ComposerPanelSize, number | null>>;

export type PreferencesV1 = Readonly<{
  version: 1;
  composer: Readonly<{
    density: ComposerDensity;
    mode: ComposerMode;
    panelSize: ComposerPanelSize;
    heightPercent: Readonly<Record<ComposerDensity, ComposerHeightTargets>>;
  }>;
  keyBar: Readonly<{ collapsed: boolean }>;
}>;

export type PreferencesSnapshot = Readonly<{
  preferences: PreferencesV1;
  status: string;
}>;

export type PreferencesStore = Readonly<{
  snapshot(): PreferencesSnapshot;
  update(preferences: PreferencesV1): PreferencesSnapshot;
}>;

type StorageLike = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export const DEFAULT_PREFERENCES: PreferencesV1 = Object.freeze({
  version: 1,
  composer: Object.freeze({
    density: "standard",
    // UX-16 §16.4: code (autocorrect off) is the default in a terminal —
    // a silently autocorrected command is the worse failure. Prose is one
    // tap away in the composer for dictated natural-language instructions.
    mode: "code",
    panelSize: "compact",
    heightPercent: Object.freeze({
      standard: Object.freeze({ compact: null, expanded: null }),
      compact: Object.freeze({ compact: null, expanded: null }),
    }),
  }),
  keyBar: Object.freeze({ collapsed: false }),
});

function exactObject(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return false;
  const actual = Object.keys(value as object).sort();
  const expected = [...keys].sort();
  return actual.length === expected.length && actual.every((key, index) => key === expected[index]);
}

function validHeight(value: unknown, maximum: number): value is number | null {
  return value === null || (Number.isInteger(value) && typeof value === "number" && value >= 10 && value <= maximum);
}

function copyValidated(value: unknown): PreferencesV1 | undefined {
  if (!exactObject(value, ["version", "composer", "keyBar"]) || value.version !== 1) return undefined;
  if (!exactObject(value.composer, ["density", "mode", "panelSize", "heightPercent"])) return undefined;
  const composer = value.composer;
  if ((composer.density !== "standard" && composer.density !== "compact")
    || (composer.mode !== "prose" && composer.mode !== "code")
    || (composer.panelSize !== "compact" && composer.panelSize !== "expanded")
    || !exactObject(composer.heightPercent, ["standard", "compact"])) return undefined;
  const heights = composer.heightPercent;
  if (!exactObject(heights.standard, ["compact", "expanded"])
    || !exactObject(heights.compact, ["compact", "expanded"])
    || !validHeight(heights.standard.compact, 70)
    || !validHeight(heights.standard.expanded, 70)
    || !validHeight(heights.compact.compact, 55)
    || !validHeight(heights.compact.expanded, 55)
    || !exactObject(value.keyBar, ["collapsed"])
    || typeof value.keyBar.collapsed !== "boolean") return undefined;
  return Object.freeze({
    version: 1,
    composer: Object.freeze({
      density: composer.density,
      mode: composer.mode,
      panelSize: composer.panelSize,
      heightPercent: Object.freeze({
        standard: Object.freeze({ compact: heights.standard.compact, expanded: heights.standard.expanded }),
        compact: Object.freeze({ compact: heights.compact.compact, expanded: heights.compact.expanded }),
      }),
    }),
    keyBar: Object.freeze({ collapsed: value.keyBar.collapsed }),
  });
}

function defaults(): PreferencesV1 {
  return copyValidated(DEFAULT_PREFERENCES) as PreferencesV1;
}

export function decodePreferences(raw: string): PreferencesSnapshot {
  try {
    const preferences = copyValidated(JSON.parse(raw));
    return preferences
      ? Object.freeze({ preferences, status: "" })
      : Object.freeze({ preferences: defaults(), status: UNREADABLE_STATUS });
  } catch {
    return Object.freeze({ preferences: defaults(), status: UNREADABLE_STATUS });
  }
}

function legacyHeight(value: string | null): number | null {
  if (value === null || !/^(?:[1-6][0-9]|70)$/.test(value)) return null;
  return Number(value);
}

function readInitial(storage: StorageLike): PreferencesSnapshot {
  let raw: string | null;
  try {
    raw = storage.getItem(PREFERENCES_KEY);
  } catch {
    return Object.freeze({ preferences: defaults(), status: UNAVAILABLE_STATUS });
  }
  if (raw !== null) return decodePreferences(raw);

  let legacy: Readonly<Record<keyof typeof LEGACY_KEYS, string | null>>;
  try {
    legacy = Object.freeze({
      mode: storage.getItem(LEGACY_KEYS.mode),
      panelSize: storage.getItem(LEGACY_KEYS.panelSize),
      compactHeight: storage.getItem(LEGACY_KEYS.compactHeight),
      expandedHeight: storage.getItem(LEGACY_KEYS.expandedHeight),
      keyBarCollapsed: storage.getItem(LEGACY_KEYS.keyBarCollapsed),
    });
  } catch {
    return Object.freeze({ preferences: defaults(), status: UNAVAILABLE_STATUS });
  }
  if (Object.values(legacy).every((value) => value === null)) {
    return Object.freeze({ preferences: defaults(), status: "" });
  }

  const migrated = copyValidated({
    version: 1,
    composer: {
      density: "standard",
      mode: legacy.mode === "code" || legacy.mode === "prose" ? legacy.mode : "code",
      panelSize: legacy.panelSize === "expanded" || legacy.panelSize === "compact" ? legacy.panelSize : "compact",
      heightPercent: {
        standard: { compact: legacyHeight(legacy.compactHeight), expanded: legacyHeight(legacy.expandedHeight) },
        compact: { compact: null, expanded: null },
      },
    },
    keyBar: { collapsed: legacy.keyBarCollapsed === "1" },
  }) as PreferencesV1;
  try {
    storage.setItem(PREFERENCES_KEY, JSON.stringify(migrated));
  } catch {
    return Object.freeze({ preferences: migrated, status: UNAVAILABLE_STATUS });
  }
  try {
    for (const key of Object.values(LEGACY_KEYS)) storage.removeItem(key);
  } catch {
    return Object.freeze({ preferences: migrated, status: UNAVAILABLE_STATUS });
  }
  return Object.freeze({ preferences: migrated, status: "" });
}

export function createPreferencesStore(storage: StorageLike): PreferencesStore {
  let current = readInitial(storage);
  return Object.freeze({
    snapshot: (): PreferencesSnapshot => current,
    update: (candidate: PreferencesV1): PreferencesSnapshot => {
      const preferences = copyValidated(candidate);
      if (!preferences) {
        current = Object.freeze({ preferences: defaults(), status: UNREADABLE_STATUS });
        return current;
      }
      try {
        storage.setItem(PREFERENCES_KEY, JSON.stringify(preferences));
        current = Object.freeze({ preferences, status: "" });
      } catch {
        current = Object.freeze({ preferences, status: UNAVAILABLE_STATUS });
      }
      return current;
    },
  });
}
