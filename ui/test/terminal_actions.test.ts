import { createKeyPreferencesStore, DEFAULT_KEY_PREFERENCES, KEY_GROUPS, KEY_PREFERENCES_KEY, KEY_PRESETS, NO_MODIFIERS, keyAction, planKey, resolveAction, validateKeyPreferences } from "../src/terminal_actions";

function assert(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
for (const keys of Object.values(KEY_GROUPS)) for (const key of keys) {
  const entry = keyAction(key);
  assert(resolveAction(entry.id)?.label === entry.label, `catalog roundtrip ${key}`);
}
for (const preset of Object.values(KEY_PRESETS)) for (const id of preset) assert(resolveAction(id), `preset ${id}`);
const plan = (key: string, ctrl = false, alt = false, shift = false) => planKey({ key, modifiers: { ctrl, alt, shift } });
assert(typeof plan("tab", true) !== "string", "Ctrl+Tab aliases terminal Tab explicitly");
assert(typeof planKey({ key: "page-up", modifiers: { ctrl: false, alt: false, shift: true } }) === "string", "Shift+PageUp is local scrolling, not remote input");
assert(typeof plan("a", true, false, true) !== "string", "Ctrl+Shift+A has the documented Ctrl+A terminal byte");
assert(typeof plan("tab", false, true) !== "string", "Alt+Tab sends an explicit Escape-prefixed Tab");
assert(typeof plan("page-down", false, true) !== "string", "Alt+PageDown sends an explicit Escape-prefixed page key");
assert(typeof plan("enter", true) !== "string" && typeof plan("escape", false, false, true) !== "string", "Ctrl/Shift Enter and Escape aliases are explicit");
assert(typeof plan("backspace", false, false, true) !== "string", "Shift+Backspace aliases Backspace explicitly");
assert(typeof plan("insert", false, false, true) === "string", "modified Insert remains honestly unavailable");
assert(typeof planKey({ key: "f12", modifiers: { ctrl: true, alt: true, shift: true } }) !== "string", "F12 modifier encoding supported");
assert(!resolveAction("key:unknown:1"), "unknown key rejected");
assert(!resolveAction("key:%xx:1"), "malformed URI rejected");
assert(!validateKeyPreferences({ ...DEFAULT_KEY_PREFERENCES, favorites: ["exec:anything"] }), "untyped action refused");
assert(!validateKeyPreferences({ ...DEFAULT_KEY_PREFERENCES, prefixes: { tmux: "local:copy", screen: keyAction("a").id } }), "prefix must be key");
const values = new Map<string, string>();
values.set("persea-terminal.preferences.v1", "old composer preferences");
const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
const store = createKeyPreferencesStore(storage);
const chord = keyAction("x", { ...NO_MODIFIERS, alt: true }).id;
const custom = { ...DEFAULT_KEY_PREFERENCES, favorites: [chord, ...KEY_PRESETS.Shell], pinned: [chord], prefixes: { tmux: keyAction("a", { ...NO_MODIFIERS, ctrl: true }).id, screen: keyAction("`").id } };
assert(store.save(custom) === "Saved on this browser.", "save status");
assert(JSON.stringify(createKeyPreferencesStore(storage).read()) === JSON.stringify(custom), "reload preserves order, pins, overrides and independent prefixes");
assert(values.get("persea-terminal.preferences.v1") === "old composer preferences", "existing strict record untouched");
values.set(KEY_PREFERENCES_KEY, "invalid");
assert(createKeyPreferencesStore(storage).read() === DEFAULT_KEY_PREFERENCES, "corrupt settings fall back");
const unavailable = createKeyPreferencesStore();
assert(unavailable.save(custom).includes("page only"), "unavailable storage explained");
assert(unavailable.read().favorites[0] === chord, "storage failure keeps page customization");
console.log("terminal actions: PASS (catalog, refused chords, preferences and rollback isolation)");
