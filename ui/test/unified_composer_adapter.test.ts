import { DEFAULT_PREFERENCES, createPreferencesStore, type PreferencesV1 } from "../src/preferences";
import { unifiedComposerAvailability, withCompactDensity, withStoredDensity, type UnifiedComposerGate } from "../src/unified_composer_adapter";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
};

function gate(overrides: Partial<UnifiedComposerGate>): UnifiedComposerGate {
  return Object.freeze({
    closed: false,
    capabilityMode: "control" as const,
    prepared: true,
    committed: true,
    controlGranted: true,
    fitPending: false,
    bracketedPasteMode: false,
    ...overrides,
  });
}

// --- availability matrix ----------------------------------------------------

// The go state, in both bracketed-paste modes. The mode is a passthrough of
// xterm's tracked DEC 2004 state: it decides whether the Composer's multiline
// guard trips, so flattening it to a constant would silently disable (or
// permanently arm) the guard.
{
  const plain = unifiedComposerAvailability(gate({}));
  assert.ok(plain.canInject, "committed control page must accept Insert");
  assert.equal(plain.reason, "Ready to insert without running it.");
  assert.equal(plain.bracketedPasteMode, false, "bracketed-paste OFF must pass through");
  const bracketed = unifiedComposerAvailability(gate({ bracketedPasteMode: true }));
  assert.ok(bracketed.canInject, "bracketed-paste mode must not change injectability");
  assert.equal(bracketed.bracketedPasteMode, true, "bracketed-paste ON must pass through");
}

// A destroyed page refuses outright, and reports no paste mode: there is no
// terminal left whose mode could be read.
{
  const closed = unifiedComposerAvailability(gate({ closed: true, bracketedPasteMode: true }));
  assert.equal(closed.canInject, false, "closed page must refuse");
  assert.equal(closed.reason, "This page is closed.");
  assert.equal(closed.bracketedPasteMode, false, "closed page reports no paste mode");
}

// Observe capability refuses regardless of connection state.
{
  const observe = unifiedComposerAvailability(gate({ capabilityMode: "observe", bracketedPasteMode: true }));
  assert.equal(observe.canInject, false, "observe page must refuse");
  assert.equal(observe.reason, "This session is read-only.");
  assert.equal(observe.bracketedPasteMode, true, "observe still reports the real paste mode");
}

// No admission, admission without commit, or a commit whose control grant has
// not landed yet: nothing is attached for typing. The grant case matters — the
// broker refuses input until the MODE(CONTROL) round-trip completes, and an
// Insert in that window would be dropped by sendInput.
{
  for (const state of [
    { prepared: false, committed: false, controlGranted: false },
    { prepared: true, committed: false, controlGranted: false },
    { prepared: true, committed: true, controlGranted: false },
  ]) {
    const detached = unifiedComposerAvailability(gate(state));
    assert.equal(detached.canInject, false, `must refuse while ${JSON.stringify(state)}`);
    assert.equal(detached.reason, "No terminal is attached.");
  }
}

// THE FIT SEAL. While an explicit vertical fit is pending, the page's
// sendInput DROPS every byte (deliberately — a keystroke replayed across a
// cut would land in the wrong screen). Availability must therefore refuse
// with everything else green, or an Insert would be counted into the seal's
// drop diagnostics and silently vanish.
{
  const sealed = unifiedComposerAvailability(gate({ fitPending: true, bracketedPasteMode: true }));
  assert.equal(sealed.canInject, false, "the Fit input seal must refuse Insert");
  assert.equal(sealed.reason, "Terminal typing is disabled locally.");
  assert.equal(sealed.bracketedPasteMode, true, "the seal does not hide the paste mode");
}

// Refusal precedence: closed beats capability beats attachment beats the seal
// — each reason is only ever shown when everything before it is fine.
{
  assert.equal(unifiedComposerAvailability(gate({ closed: true, capabilityMode: "observe", prepared: false, fitPending: true })).reason, "This page is closed.");
  assert.equal(unifiedComposerAvailability(gate({ capabilityMode: "observe", prepared: false, committed: false, fitPending: true })).reason, "This session is read-only.");
  assert.equal(unifiedComposerAvailability(gate({ prepared: false, committed: false, controlGranted: false, fitPending: true })).reason, "No terminal is attached.");
}

// --- compact-density preference adapters -------------------------------------

// Reads force compact density (compact IS the unified design) without
// touching anything else; a stored compact snapshot passes through untouched.
{
  const forced = withCompactDensity(DEFAULT_PREFERENCES);
  assert.equal(forced.composer.density, "compact", "reads must force compact density");
  assert.equal(forced.composer.mode, DEFAULT_PREFERENCES.composer.mode, "mode must survive the density force");
  assert.equal(forced.composer.panelSize, DEFAULT_PREFERENCES.composer.panelSize, "panel size must survive the density force");
  assert.equal(forced.composer.heightPercent, DEFAULT_PREFERENCES.composer.heightPercent, "height branches must survive the density force");
  assert.equal(withCompactDensity(forced), forced, "an already-compact snapshot passes through identically");
}

// Writes restore the stored density: the unified page must never flip the
// legacy page's layout through the shared store.
{
  const written: PreferencesV1 = Object.freeze({
    version: 1,
    composer: Object.freeze({
      density: "compact" as const,
      mode: "code" as const,
      panelSize: "expanded" as const,
      heightPercent: Object.freeze({
        standard: Object.freeze({ compact: null, expanded: null }),
        compact: Object.freeze({ compact: 30, expanded: null }),
      }),
    }),
    keyBar: Object.freeze({ collapsed: false }),
  });
  const restored = withStoredDensity(DEFAULT_PREFERENCES, written);
  assert.equal(restored.composer.density, "standard", "writes must restore the stored density");
  assert.equal(restored.composer.mode, "code", "the written mode must survive the restore");
  assert.equal(restored.composer.panelSize, "expanded", "the written size must survive the restore");
  assert.equal(restored.composer.heightPercent.compact.compact, 30, "the written compact height must survive the restore");
  assert.equal(withStoredDensity(written, written), written, "matching densities pass through identically");
}

// Full round trip through the REAL store and validator: what the unified page
// persists must survive copyValidated with the stored density intact, the
// compact height branch updated, and the standard branch untouched.
{
  const backing = new Map<string, string>();
  const storage = {
    getItem: (key: string) => backing.get(key) ?? null,
    setItem: (key: string, value: string) => { backing.set(key, value); },
    removeItem: (key: string) => { backing.delete(key); },
  };
  const store = createPreferencesStore(storage);
  const read = withCompactDensity(store.snapshot().preferences);
  assert.equal(read.composer.density, "compact", "the unified composer must read compact density");
  // What Composer.persistComposer emits while running as compact density: its
  // own density, and a height write into the compact branch only.
  const persisted: PreferencesV1 = Object.freeze({
    version: 1,
    composer: Object.freeze({
      density: "compact" as const,
      mode: read.composer.mode,
      panelSize: read.composer.panelSize,
      heightPercent: Object.freeze({
        standard: read.composer.heightPercent.standard,
        compact: Object.freeze({ compact: 25, expanded: null }),
      }),
    }),
    keyBar: read.keyBar,
  });
  const result = store.update(withStoredDensity(store.snapshot().preferences, persisted));
  assert.equal(result.status, "", "the adapted write must validate cleanly");
  assert.equal(result.preferences.composer.density, "standard", "the shared store keeps its own density");
  assert.equal(result.preferences.composer.heightPercent.compact.compact, 25, "the compact height write lands");
  assert.equal(result.preferences.composer.heightPercent.standard.compact, null, "the standard branch stays untouched");
}

console.log("unified composer adapter tests PASS");
