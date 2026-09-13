import type { ComposerAvailability } from "./composer";
import type { PreferencesV1 } from "./preferences";

/**
 * The unified page's composer gate, reduced to the facts the availability
 * decision reads. The page owns the facts; this module owns the decision, so
 * the ordering of refusals is unit-testable without a DOM or a transport.
 */
export type UnifiedComposerGate = Readonly<{
  closed: boolean;
  capabilityMode: "observe" | "control";
  prepared: boolean;
  committed: boolean;
  // The broker's control grant. Input is refused until the MODE(CONTROL)
  // round-trip completes, so an Insert before the grant would be dropped by
  // sendInput; the composer must refuse it here rather than swallow it.
  controlGranted: boolean;
  fitPending: boolean;
  bracketedPasteMode: boolean;
}>;

/**
 * Mirrors the legacy page's availability semantics on the unified page's own
 * state: canInject exactly when the page could deliver bytes through
 * sendInput, so an Insert the composer permits can never be silently eaten by
 * the input gate. fitPending is the unified page's input seal — sendInput
 * DROPS bytes while a committed geometry is in flight — so it must refuse
 * here rather than let a paste vanish into the seal's drop counter. The
 * reason strings reuse the legacy composer vocabulary.
 */
export function unifiedComposerAvailability(gate: UnifiedComposerGate): ComposerAvailability {
  if (gate.closed) {
    return Object.freeze({ canInject: false, reason: "This page is closed.", bracketedPasteMode: false });
  }
  if (gate.capabilityMode !== "control") {
    return Object.freeze({ canInject: false, reason: "This session is read-only.", bracketedPasteMode: gate.bracketedPasteMode });
  }
  if (!gate.prepared || !gate.committed || !gate.controlGranted) {
    return Object.freeze({ canInject: false, reason: "No terminal is attached.", bracketedPasteMode: gate.bracketedPasteMode });
  }
  if (gate.fitPending) {
    return Object.freeze({ canInject: false, reason: "Terminal typing is disabled locally.", bracketedPasteMode: gate.bracketedPasteMode });
  }
  return Object.freeze({ canInject: true, reason: "Ready to insert without running it.", bracketedPasteMode: gate.bracketedPasteMode });
}

/**
 * The unified composer always runs with compact-density semantics — compact
 * IS its design — while the preferences store stays shared with the legacy
 * page. Reads force the density the Composer sees; writes restore the
 * stored density so the unified page never flips the legacy page's layout.
 * Everything else (mode, panel size, per-density heights) round-trips
 * unchanged: the Composer only writes the height branch of the density it is
 * running as, which here is always the compact branch.
 */
export function withCompactDensity(stored: PreferencesV1): PreferencesV1 {
  if (stored.composer.density === "compact") return stored;
  return Object.freeze({
    ...stored,
    composer: Object.freeze({ ...stored.composer, density: "compact" as const }),
  });
}

export function withStoredDensity(stored: PreferencesV1, next: PreferencesV1): PreferencesV1 {
  if (next.composer.density === stored.composer.density) return next;
  return Object.freeze({
    ...next,
    composer: Object.freeze({ ...next.composer, density: stored.composer.density }),
  });
}
