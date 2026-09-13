import { unifiedKeyDescriptor, type UnifiedKeyDescriptor } from "./unified_key_bar";
import type { LogicalKey } from "./continuous_surface/types";

export type KeyModifiers = Readonly<{ ctrl: boolean; alt: boolean; shift: boolean }>;
export const NO_MODIFIERS: KeyModifiers = Object.freeze({ ctrl: false, alt: false, shift: false });
export type KeyChord = Readonly<{ key: string; modifiers: KeyModifiers }>;
export type PrefixProvider = string;
export type LocalTerminalAction = "composer" | "copy" | "select" | "fit-font";
export type TerminalAction =
  | Readonly<{ kind: "key"; chord: KeyChord }>
  | Readonly<{ kind: "sequence"; prefix: PrefixProvider; keys: readonly KeyChord[] }>
  | Readonly<{ kind: "text"; text: string }>
  | Readonly<{ kind: "local"; action: LocalTerminalAction }>;
export type TerminalActionEntry = Readonly<{ id: string; label: string; group: string; action: TerminalAction }>;
export type KeyPlan = Readonly<{ descriptor?: UnifiedKeyDescriptor; character?: string; prefixEscape: boolean }>;

export const KEY_GROUPS: Readonly<Record<string, readonly string[]>> = Object.freeze({
  Navigation: ["escape", "tab", "enter", "backspace", "insert", "delete", "arrow-left", "arrow-down", "arrow-up", "arrow-right", "home", "end", "page-up", "page-down"],
  Function: Array.from({ length: 12 }, (_, i) => `f${i + 1}`),
  Letters: [..."abcdefghijklmnopqrstuvwxyz"],
  Numbers: [..."0123456789"],
  Symbols: [..." `~!@#$%^&*()-_=+[{]}\\|;:'\",<.>/?"],
});
const LABELS: Readonly<Record<string, string>> = Object.freeze({
  escape: "Esc", tab: "Tab", enter: "Enter", backspace: "Bksp", insert: "Ins", delete: "Del",
  "arrow-left": "←", "arrow-down": "↓", "arrow-up": "↑", "arrow-right": "→",
  home: "Home", end: "End", "page-up": "PgUp", "page-down": "PgDn", " ": "Space",
});
const ALL_KEYS = new Set([...Object.values(KEY_GROUPS).flat(), ..."ABCDEFGHIJKLMNOPQRSTUVWXYZ"]);
const SHIFT_BASE = "`1234567890-=[]\\;',./";
const SHIFT_UPPER = "~!@#$%^&*()_+{}|:\"<>?";

export function keyLabel(key: string): string { return LABELS[key] ?? (/^f\d+$/.test(key) ? key.toUpperCase() : key); }
/** Display glyphs never replace the spoken key name or serialized identity. */
export function keyGlyph(key: string): string {
  return ({ tab: "⇥", enter: "↵", backspace: "⌫", delete: "⌦", "page-up": "Pg↑", "page-down": "Pg↓" } as Record<string, string>)[key] ?? keyLabel(key);
}
export function actionGlyph(entry: TerminalActionEntry): string {
  if (entry.action.kind !== "key") return entry.label;
  const { key, modifiers: m } = entry.action.chord;
  return [m.ctrl ? "Ctrl" : "", m.alt ? "Alt" : "", m.shift ? "⇧" : "", keyGlyph(key)].filter(Boolean).join("+");
}
export function keyEncodingNote(chord: KeyChord): string {
  const { key, modifiers: m } = chord;
  if (m.ctrl && m.shift && /^[a-z]$/i.test(key)) return "Uses the same terminal code as Ctrl+" + key.toLowerCase() + ".";
  if (key === "tab" && m.ctrl) return "Ctrl does not add a distinct code to Tab in classic terminal input.";
  if ((key === "enter" || key === "escape") && (m.ctrl || m.shift)) return "Ctrl and Shift do not add a distinct code to this key in classic terminal input.";
  if (key === "backspace" && m.shift) return "Shift does not add a distinct code to Backspace in classic terminal input.";
  return "";
}
export function chordLabel(chord: KeyChord): string {
  return [chord.modifiers.ctrl ? "Ctrl" : "", chord.modifiers.alt ? "Alt" : "", chord.modifiers.shift ? "Shift" : "", keyLabel(chord.key)].filter(Boolean).join("+");
}
export function keyAction(key: string, modifiers: KeyModifiers = NO_MODIFIERS): TerminalActionEntry {
  const chord = { key, modifiers: { ...modifiers } };
  const mask = (modifiers.ctrl ? 1 : 0) | (modifiers.alt ? 2 : 0) | (modifiers.shift ? 4 : 0);
  return { id: `key:${encodeURIComponent(key)}:${mask}`, label: chordLabel(chord), group: "Keys", action: { kind: "key", chord } };
}

/** Only combinations with an honest VT representation are enabled. For
 * printable Alt chords, an explicit Escape prefix avoids the host platform's
 * Option/dead-key policy without changing physical keyboard options. */
export function planKey(chord: KeyChord): KeyPlan | string {
  const { key, modifiers: m } = chord;
  if (!ALL_KEYS.has(key)) return "This key is not supported.";
  if (key.length === 1) {
    let character = key;
    if (m.shift) {
      const index = SHIFT_BASE.indexOf(key);
      character = /^[a-z]$/.test(key) ? key.toUpperCase() : index >= 0 ? SHIFT_UPPER[index] : key;
    }
    let descriptor: UnifiedKeyDescriptor | undefined;
    if (m.ctrl) {
      if (character === " " || character === "@" || character === "2") descriptor = { key: "@", code: "Digit2", keyCode: 50, ctrlKey: true, shiftKey: true };
      else if (/^[3-8]$/.test(character)) descriptor = { key: character, code: `Digit${character}`, keyCode: character.charCodeAt(0), ctrlKey: true };
      else descriptor = unifiedKeyDescriptor({ ctrl: /^[A-Z]$/.test(character) ? character.toLowerCase() : character });
      if (!descriptor) return "This Ctrl combination has no classic terminal code supported by this terminal.";
    } else {
      // xterm deliberately leaves uppercase keydowns to keypress/IME. Use
      // its insertText event path for explicit literal characters as well.
      return { character, prefixEscape: m.alt };
    }
    return { descriptor, prefixEscape: m.alt };
  }
  const named = key === "enter" ? { key: "Enter", code: "Enter", keyCode: 13 }
    : key === "insert" ? { key: "Insert", code: "Insert", keyCode: 45 }
    : unifiedKeyDescriptor(key as LogicalKey);
  if (!named) return "This key is not supported.";
  // Explicit terminal Meta keys use Escape-prefix semantics on every device;
  // a phone's Alt button is not an attempt to invoke the OS's Alt+Tab shortcut.
  if (key === "tab") return { descriptor: { ...named, shiftKey: m.shift }, prefixEscape: m.alt };
  if (key === "enter" || key === "escape") return { descriptor: named, prefixEscape: m.alt };
  if (key === "backspace") return { descriptor: { ...named, ctrlKey: m.ctrl, altKey: m.alt }, prefixEscape: false };
  if (key === "insert" && (m.ctrl || m.alt || m.shift)) return "This terminal's keyboard engine handles modified Insert as a local clipboard command; it cannot send it to the session.";
  if ((key === "page-up" || key === "page-down") && m.shift) return "This terminal's keyboard engine handles Shift+Page keys as local scrolling; it cannot send this chord to the session.";
  if ((key === "page-up" || key === "page-down") && m.alt && !m.ctrl) return { descriptor: named, prefixEscape: true };
  return { descriptor: { ...named, ctrlKey: m.ctrl, altKey: m.alt, shiftKey: m.shift }, prefixEscape: false };
}

const ctrl = (key: string): TerminalActionEntry => keyAction(key, { ...NO_MODIFIERS, ctrl: true });
const seq = (provider: PrefixProvider, key: string, label: string): TerminalActionEntry => ({
  id: `${provider}:${key}`, label, group: "Sequences", action: { kind: "sequence", prefix: provider, keys: [{ key, modifiers: NO_MODIFIERS }] },
});
export const LOCAL_ACTIONS: readonly TerminalActionEntry[] = [
  { id: "local:composer", label: "Composer", group: "Controls", action: { kind: "local", action: "composer" } },
  { id: "local:copy", label: "Copy selection", group: "Controls", action: { kind: "local", action: "copy" } },
  { id: "local:select", label: "Select text", group: "Controls", action: { kind: "local", action: "select" } },
  { id: "local:fit-font", label: "Fit font", group: "Controls", action: { kind: "local", action: "fit-font" } },
];
export const SEQUENCE_ACTIONS: readonly TerminalActionEntry[] = [
  seq("tmux", "n", "tmux: prefix, n"), seq("tmux", "p", "tmux: prefix, p"), seq("tmux", "o", "tmux: prefix, o"),
  seq("screen", "n", "screen: prefix, n"), seq("screen", "p", "screen: prefix, p"),
];
const SPECIAL_ACTIONS = [...LOCAL_ACTIONS, ...SEQUENCE_ACTIONS];
export function prefixSequence(provider: PrefixProvider, chord: KeyChord): TerminalActionEntry {
  return { id: `sequence:${encodeURIComponent(provider)}:${keyAction(chord.key, chord.modifiers).id}`, label: `${provider}: prefix, ${chordLabel(chord)}`, group: "Sequences", action: { kind: "sequence", prefix: provider, keys: [chord] } };
}
export function resolveAction(id: string): TerminalActionEntry | undefined {
  const special = SPECIAL_ACTIONS.find((entry) => entry.id === id);
  if (special) return special;
  const sequence = /^sequence:([^:]+):(key:.*)$/.exec(id);
  if (sequence) {
    try {
      const provider = decodeURIComponent(sequence[1]);
      const key = resolveAction(sequence[2]);
      return validPrefixName(provider) && key?.action.kind === "key" ? prefixSequence(provider, key.action.chord) : undefined;
    } catch { return undefined; }
  }
  const match = /^key:(.*):([0-7])$/.exec(id);
  if (!match) return undefined;
  try {
    const mask = Number(match[2]);
    const entry = keyAction(decodeURIComponent(match[1]), { ctrl: !!(mask & 1), alt: !!(mask & 2), shift: !!(mask & 4) });
    return entry.action.kind === "key" && ALL_KEYS.has(entry.action.chord.key) ? entry : undefined;
  } catch { return undefined; }
}
export const KEY_PRESETS: Readonly<Record<string, readonly string[]>> = Object.freeze({
  Terminal: [ctrl("c").id, ctrl("d").id, keyAction("enter").id, keyAction("home").id, keyAction("end").id, keyAction("page-up").id, keyAction("page-down").id, keyAction("f1").id],
  Shell: [ctrl("a").id, ctrl("e").id, ctrl("u").id, ctrl("w").id, ctrl("r").id, ctrl("c").id, ctrl("d").id, ctrl("l").id],
  Vim: [keyAction("escape").id, ctrl("[").id, ctrl("r").id, ctrl("d").id, ctrl("u").id, keyAction(":").id, keyAction("/").id],
  tmux: [ctrl("b").id, "tmux:n", "tmux:p", "tmux:o", keyAction("escape").id],
  screen: [ctrl("a").id, "screen:n", "screen:p", keyAction("escape").id],
});

export const KEY_PREFERENCES_KEY = "persea-terminal.keys.v1";
export type KeyPreferences = Readonly<{ version: 1; favorites: readonly string[]; pinned: readonly string[]; prefixes: Readonly<Record<PrefixProvider, string>> }>;
export const DEFAULT_KEY_PREFERENCES: KeyPreferences = Object.freeze({ version: 1, favorites: KEY_PRESETS.Terminal, pinned: [], prefixes: { tmux: ctrl("b").id, screen: ctrl("a").id } });
export function validPrefixName(value: string): boolean { return /^[A-Za-z][A-Za-z0-9 _-]{0,31}$/.test(value) && !["constructor", "prototype", "__proto__"].includes(value); }
export function validateKeyPreferences(value: unknown): KeyPreferences | undefined {
  if (!value || typeof value !== "object") return undefined;
  const v = value as Partial<KeyPreferences>;
  const ids = (items: unknown): items is string[] => Array.isArray(items) && items.length <= 100 && items.every((id) => typeof id === "string" && !!resolveAction(id)) && new Set(items).size === items.length;
  if (v.version !== 1 || !ids(v.favorites) || !ids(v.pinned) || !v.pinned.every((id) => v.favorites!.includes(id)) || !v.prefixes) return undefined;
  const prefixes = Object.entries(v.prefixes);
  if (prefixes.length > 20 || !prefixes.every(([name, id]) => validPrefixName(name) && typeof id === "string" && resolveAction(id)?.action.kind === "key")) return undefined;
  return { version: 1, favorites: [...v.favorites], pinned: [...v.pinned], prefixes: Object.fromEntries(prefixes) };
}
/** Kept separate from the strict server and composer schemas: old releases
 * ignore this record, so rolling back does not invalidate either store. */
export function createKeyPreferencesStore(storage?: Pick<Storage, "getItem" | "setItem">): { read(): KeyPreferences; save(value: KeyPreferences): string; status(): string } {
  let current = DEFAULT_KEY_PREFERENCES;
  let status = "";
  try {
    const raw = storage?.getItem(KEY_PREFERENCES_KEY);
    if (raw) { current = validateKeyPreferences(JSON.parse(raw)) ?? DEFAULT_KEY_PREFERENCES; if (current === DEFAULT_KEY_PREFERENCES) status = "Saved keys could not be read; using defaults."; }
  } catch { status = "Saved keys could not be read; using defaults."; }
  return {
    read: () => current, status: () => status,
    save(value) {
      const validated = validateKeyPreferences(value);
      if (!validated) return "Invalid key settings.";
      current = validated;
      try { if (!storage) throw new Error(); storage.setItem(KEY_PREFERENCES_KEY, JSON.stringify(current)); status = "Saved on this browser."; }
      catch { status = "Applied for this page only; browser storage is unavailable."; }
      return status;
    },
  };
}
