import type { LogicalKey } from "./continuous_surface/types";

/**
 * The unified terminal's accessory key row. A phone keyboard has no arrows,
 * Esc, Tab, Home, End or Ctrl, and a modifierless tap cannot position the
 * cursor, so this row is the actual means of navigating a terminal on a phone
 * rather than a convenience. Key set and order mirror the legacy attachment
 * page bar: escapes first, then the arrow cluster under the thumb. The page
 * inserts the Ctrl latch after tab and the ✎ composer toggle last.
 */
export type KeyBarEntry = Readonly<{ label: string; key: LogicalKey; title: string }>;

export const UNIFIED_KEY_BAR_PRIMARY: readonly KeyBarEntry[] = Object.freeze([
  Object.freeze({ label: "esc", key: "escape" as LogicalKey, title: "Escape" }),
  Object.freeze({ label: "tab", key: "tab" as LogicalKey, title: "Tab" }),
  Object.freeze({ label: "←", key: "arrow-left" as LogicalKey, title: "Left" }),
  Object.freeze({ label: "↓", key: "arrow-down" as LogicalKey, title: "Down" }),
  Object.freeze({ label: "↑", key: "arrow-up" as LogicalKey, title: "Up" }),
  Object.freeze({ label: "→", key: "arrow-right" as LogicalKey, title: "Right" }),
]);

/**
 * The remaining named keys. The bar keeps esc tab ctrl ← ↓ ↑ → in its
 * Standard bank with a fixed Fn switch. These extra keys live on the
 * quick-actions sheet, which is reachable with the keyboard closed — paging
 * through output is a read-only act and must not require raising the keyboard
 * first. F1–F12 use a temporary bank in the same row and therefore are not
 * duplicated here.
 */
export const UNIFIED_SHEET_KEYS: readonly KeyBarEntry[] = Object.freeze([
  Object.freeze({ label: "home", key: "home" as LogicalKey, title: "Home" }),
  Object.freeze({ label: "end", key: "end" as LogicalKey, title: "End" }),
  Object.freeze({ label: "pgup", key: "page-up" as LogicalKey, title: "Page up" }),
  Object.freeze({ label: "pgdn", key: "page-down" as LogicalKey, title: "Page down" }),
  Object.freeze({ label: "del", key: "delete" as LogicalKey, title: "Delete" }),
  Object.freeze({ label: "bksp", key: "backspace" as LogicalKey, title: "Backspace" }),
]);

/** The temporary function-key bank shown in the one existing accessory row. */
export const UNIFIED_FUNCTION_KEYS: readonly KeyBarEntry[] = Object.freeze(
  Array.from({ length: 12 }, (_, index) => {
    const number = index + 1;
    return Object.freeze({
      label: `F${number}`,
      key: `f${number}` as LogicalKey,
      title: `Function key F${number}`,
    });
  }),
);

/**
 * Descriptor for a synthetic keydown aimed at xterm's hidden textarea. The
 * bytes are deliberately NOT encoded here: xterm's keyboard evaluator reads
 * its own tracked DEC private modes, so dispatching the logical key is what
 * keeps arrows mode-correct — ESC O A under application cursor keys, ESC [ A
 * otherwise. Hardcoding escape strings would freeze one mode into both.
 */
export type UnifiedKeyDescriptor = Readonly<{
  key: string;
  code: string;
  keyCode: number;
  ctrlKey?: boolean;
  altKey?: boolean;
  shiftKey?: boolean;
}>;

const NAMED_KEY_DESCRIPTORS: Readonly<Record<Exclude<LogicalKey, { readonly ctrl: string }>, UnifiedKeyDescriptor>> = Object.freeze({
  escape: { key: "Escape", code: "Escape", keyCode: 27 },
  tab: { key: "Tab", code: "Tab", keyCode: 9 },
  backspace: { key: "Backspace", code: "Backspace", keyCode: 8 },
  delete: { key: "Delete", code: "Delete", keyCode: 46 },
  "arrow-up": { key: "ArrowUp", code: "ArrowUp", keyCode: 38 },
  "arrow-down": { key: "ArrowDown", code: "ArrowDown", keyCode: 40 },
  "arrow-left": { key: "ArrowLeft", code: "ArrowLeft", keyCode: 37 },
  "arrow-right": { key: "ArrowRight", code: "ArrowRight", keyCode: 39 },
  home: { key: "Home", code: "Home", keyCode: 36 },
  end: { key: "End", code: "End", keyCode: 35 },
  "page-up": { key: "PageUp", code: "PageUp", keyCode: 33 },
  "page-down": { key: "PageDown", code: "PageDown", keyCode: 34 },
  f1: { key: "F1", code: "F1", keyCode: 112 },
  f2: { key: "F2", code: "F2", keyCode: 113 },
  f3: { key: "F3", code: "F3", keyCode: 114 },
  f4: { key: "F4", code: "F4", keyCode: 115 },
  f5: { key: "F5", code: "F5", keyCode: 116 },
  f6: { key: "F6", code: "F6", keyCode: 117 },
  f7: { key: "F7", code: "F7", keyCode: 118 },
  f8: { key: "F8", code: "F8", keyCode: 119 },
  f9: { key: "F9", code: "F9", keyCode: 120 },
  f10: { key: "F10", code: "F10", keyCode: 121 },
  f11: { key: "F11", code: "F11", keyCode: 122 },
  f12: { key: "F12", code: "F12", keyCode: 123 },
});

const CTRL_SPECIAL_DESCRIPTORS: Readonly<Record<"[" | "]" | "\\" | "^" | "_", UnifiedKeyDescriptor>> = Object.freeze({
  "[": { key: "[", code: "BracketLeft", keyCode: 219, ctrlKey: true },
  "]": { key: "]", code: "BracketRight", keyCode: 221, ctrlKey: true },
  "\\": { key: "\\", code: "Backslash", keyCode: 220, ctrlKey: true },
  // xterm's evaluator recognises the legacy Ctrl+6 keyCode as the terminal Ctrl+^ chord.
  "^": { key: "^", code: "Digit6", keyCode: 54, ctrlKey: true },
  // Shift is load-bearing: xterm falls through its unshifted Ctrl branch and maps
  // the literal "_" key to C0.US.
  "_": { key: "_", code: "Minus", keyCode: 189, ctrlKey: true, shiftKey: true },
});

export function unifiedKeyDescriptor(key: LogicalKey): UnifiedKeyDescriptor | undefined {
  if (typeof key === "string") {
    return Object.prototype.hasOwnProperty.call(NAMED_KEY_DESCRIPTORS, key)
      ? NAMED_KEY_DESCRIPTORS[key]
      : undefined;
  }
  const ctrl = key.ctrl;
  if (/^[a-z]$/.test(ctrl)) {
    const upper = ctrl.toUpperCase();
    return Object.freeze({ key: ctrl, code: `Key${upper}`, keyCode: upper.charCodeAt(0), ctrlKey: true });
  }
  return Object.prototype.hasOwnProperty.call(CTRL_SPECIAL_DESCRIPTORS, ctrl)
    ? CTRL_SPECIAL_DESCRIPTORS[ctrl as keyof typeof CTRL_SPECIAL_DESCRIPTORS]
    : undefined;
}

/** Ctrl latches rather than chords: two-finger chording on glass is not usable. */
const CTRL_LATCHABLE_SPECIALS: ReadonlySet<string> = Object.freeze(new Set(["[", "]", "\\", "^", "_"]));

/**
 * The latched-Ctrl reading of one typed character. Letters chord as their
 * lowercase control — the case a soft keyboard reports is a Shift artifact
 * the control plane ignores. The specials are the remaining C0-mappable
 * punctuation. Anything else is not consumable.
 */
export function latchedCtrlKeyFor(character: string): LogicalKey | undefined {
  if (/^[A-Za-z]$/.test(character)) return Object.freeze({ ctrl: character.toLowerCase() });
  return CTRL_LATCHABLE_SPECIALS.has(character) ? Object.freeze({ ctrl: character }) : undefined;
}

/**
 * The one-shot Ctrl latch. A non-consumable character (Enter, an arrow, an
 * emoji) passes through and leaves the latch armed, mirroring the legacy bar:
 * clearing on a key that could never chord would make the latch feel like it
 * randomly forgot.
 */
export class CtrlLatch {
  private armed = false;

  get latched(): boolean {
    return this.armed;
  }

  toggle(): boolean {
    this.armed = !this.armed;
    return this.armed;
  }

  clear(): void {
    this.armed = false;
  }

  consume(character: string): LogicalKey | undefined {
    if (!this.armed) return undefined;
    const key = latchedCtrlKeyFor(character);
    if (key !== undefined) this.armed = false;
    return key;
  }
}

/**
 * Synthetic keydown for xterm's textarea listener. xterm's evaluator still
 * switches on the legacy keyCode/which fields, which the KeyboardEvent
 * constructor does not accept, so they are shimmed on afterwards.
 */
export function syntheticKeydownEvent(descriptor: UnifiedKeyDescriptor): KeyboardEvent {
  const event = new KeyboardEvent("keydown", {
    key: descriptor.key,
    code: descriptor.code,
    ctrlKey: descriptor.ctrlKey ?? false,
    altKey: descriptor.altKey ?? false,
    shiftKey: descriptor.shiftKey ?? false,
    bubbles: true,
    cancelable: true,
    composed: true,
  });
  if (event.keyCode !== descriptor.keyCode) {
    Object.defineProperty(event, "keyCode", { configurable: true, get: () => descriptor.keyCode });
  }
  if (event.which !== descriptor.keyCode) {
    Object.defineProperty(event, "which", { configurable: true, get: () => descriptor.keyCode });
  }
  return event;
}

/**
 * xterm tracks whether a keydown preceded composed insertText. A neutral
 * Control release clears that public event-path state without moving focus.
 */
export function syntheticCtrlReleaseEvent(): KeyboardEvent {
  const event = new KeyboardEvent("keyup", {
    key: "Control",
    code: "ControlLeft",
    bubbles: true,
    cancelable: true,
    composed: true,
  });
  if (event.keyCode !== 17) {
    Object.defineProperty(event, "keyCode", { configurable: true, get: () => 17 });
  }
  if (event.which !== 17) {
    Object.defineProperty(event, "which", { configurable: true, get: () => 17 });
  }
  return event;
}
