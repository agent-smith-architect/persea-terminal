import {
  CtrlLatch,
  UNIFIED_FUNCTION_KEYS,
  UNIFIED_KEY_BAR_PRIMARY,
  UNIFIED_SHEET_KEYS,
  latchedCtrlKeyFor,
  unifiedKeyDescriptor,
  type UnifiedKeyDescriptor,
} from "../src/unified_key_bar";
import type { LogicalKey } from "../src/continuous_surface/types";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void {
    if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`);
  },
  ok(value: unknown, message: string): void {
    if (!value) throw new Error(message);
  },
};

// --- key set parity with the legacy attachment page bar ---------------------

assert.equal(
  UNIFIED_KEY_BAR_PRIMARY.map((entry) => `${entry.label}:${String(entry.key)}`).join(","),
  "esc:escape,tab:tab,←:arrow-left,↓:arrow-down,↑:arrow-up,→:arrow-right",
  "primary key row drifted from the legacy bar",
);
// The bar is exactly eight keys (esc tab ctrl ← ↓ ↑ → Fn); the remaining named
// keys live on the quick-actions sheet, reachable with the keyboard closed.
assert.equal(
  UNIFIED_SHEET_KEYS.map((entry) => `${entry.label}:${String(entry.key)}`).join(","),
  "home:home,end:end,pgup:page-up,pgdn:page-down,del:delete,bksp:backspace",
  "sheet key set drifted",
);
assert.equal(UNIFIED_KEY_BAR_PRIMARY.length + 2, 8, "the bar (primary keys + ctrl + ✎) must be exactly eight keys");
assert.equal(
  UNIFIED_FUNCTION_KEYS.map((entry) => `${entry.label}:${String(entry.key)}`).join(","),
  "F1:f1,F2:f2,F3:f3,F4:f4,F5:f5,F6:f6,F7:f7,F8:f8,F9:f9,F10:f10,F11:f11,F12:f12",
  "function-key bank drifted",
);

// --- named descriptor mapping ------------------------------------------------
// The descriptors carry logical keys, never encoded bytes: xterm's evaluator
// owns the DECCKM-dependent arrow encoding. This table pins the identity xterm
// switches on (key + code + legacy keyCode).

const namedExpectations: readonly (readonly [LogicalKey, string, string, number])[] = [
  ["escape", "Escape", "Escape", 27],
  ["tab", "Tab", "Tab", 9],
  ["backspace", "Backspace", "Backspace", 8],
  ["delete", "Delete", "Delete", 46],
  ["arrow-up", "ArrowUp", "ArrowUp", 38],
  ["arrow-down", "ArrowDown", "ArrowDown", 40],
  ["arrow-left", "ArrowLeft", "ArrowLeft", 37],
  ["arrow-right", "ArrowRight", "ArrowRight", 39],
  ["home", "Home", "Home", 36],
  ["end", "End", "End", 35],
  ["page-up", "PageUp", "PageUp", 33],
  ["page-down", "PageDown", "PageDown", 34],
  ...Array.from({ length: 12 }, (_, index) => {
    const number = index + 1;
    return [`f${number}` as LogicalKey, `F${number}`, `F${number}`, 112 + index] as const;
  }),
];
for (const [logical, key, code, keyCode] of namedExpectations) {
  const descriptor = unifiedKeyDescriptor(logical);
  assert.ok(descriptor, `no descriptor for ${String(logical)}`);
  const resolved = descriptor as UnifiedKeyDescriptor;
  assert.equal(resolved.key, key, `key drifted for ${String(logical)}`);
  assert.equal(resolved.code, code, `code drifted for ${String(logical)}`);
  assert.equal(resolved.keyCode, keyCode, `keyCode drifted for ${String(logical)}`);
  assert.equal(resolved.ctrlKey ?? false, false, `named key ${String(logical)} must not carry ctrl`);
  assert.equal(resolved.shiftKey ?? false, false, `named key ${String(logical)} must not carry shift`);
}
assert.equal(unifiedKeyDescriptor("f13" as LogicalKey), undefined, "junk named key accepted");

// --- ctrl chord descriptors ----------------------------------------------------

const ctrlC = unifiedKeyDescriptor({ ctrl: "c" }) as UnifiedKeyDescriptor;
assert.equal(ctrlC.key, "c", "ctrl letter key drifted");
assert.equal(ctrlC.code, "KeyC", "ctrl letter code drifted");
assert.equal(ctrlC.keyCode, 67, "ctrl letter keyCode drifted");
assert.equal(ctrlC.ctrlKey, true, "ctrl letter lost its modifier");
assert.equal(unifiedKeyDescriptor({ ctrl: "C" }), undefined, "uppercase belongs to the latch, not the descriptor");
assert.equal(unifiedKeyDescriptor({ ctrl: "1" }), undefined, "unchordable character accepted");
assert.equal(unifiedKeyDescriptor({ ctrl: "" }), undefined, "empty ctrl accepted");

const ctrlSpecialExpectations: readonly (readonly [string, string, number, boolean])[] = [
  ["[", "BracketLeft", 219, false],
  ["]", "BracketRight", 221, false],
  ["\\", "Backslash", 220, false],
  // The legacy Ctrl+6 keyCode is what xterm recognises as Ctrl+^.
  ["^", "Digit6", 54, false],
  // Shift is load-bearing on "_": xterm maps it to C0.US only through its
  // shifted branch.
  ["_", "Minus", 189, true],
];
for (const [character, code, keyCode, shift] of ctrlSpecialExpectations) {
  const descriptor = unifiedKeyDescriptor({ ctrl: character });
  assert.ok(descriptor, `no descriptor for ctrl+${character}`);
  const resolved = descriptor as UnifiedKeyDescriptor;
  assert.equal(resolved.key, character, `ctrl special key drifted for ${character}`);
  assert.equal(resolved.code, code, `ctrl special code drifted for ${character}`);
  assert.equal(resolved.keyCode, keyCode, `ctrl special keyCode drifted for ${character}`);
  assert.equal(resolved.ctrlKey, true, `ctrl special lost its modifier for ${character}`);
  assert.equal(resolved.shiftKey ?? false, shift, `ctrl special shift drifted for ${character}`);
}

// --- latched character reading -------------------------------------------------

assert.equal(JSON.stringify(latchedCtrlKeyFor("A")), '{"ctrl":"a"}', "latch must lowercase letters");
assert.equal(JSON.stringify(latchedCtrlKeyFor("z")), '{"ctrl":"z"}', "lowercase letter rejected");
assert.equal(JSON.stringify(latchedCtrlKeyFor("[")), '{"ctrl":"["}', "latchable special rejected");
assert.equal(latchedCtrlKeyFor("1"), undefined, "digit consumed by latch");
assert.equal(latchedCtrlKeyFor("Enter"), undefined, "named key consumed by latch");
assert.equal(latchedCtrlKeyFor(""), undefined, "empty character consumed by latch");

// --- latch state machine --------------------------------------------------------

const latch = new CtrlLatch();
assert.equal(latch.latched, false, "latch armed at rest");
assert.equal(latch.consume("a"), undefined, "unarmed latch consumed a character");
assert.equal(latch.latched, false, "consume armed a resting latch");

assert.equal(latch.toggle(), true, "toggle did not arm");
assert.equal(JSON.stringify(latch.consume("A")), '{"ctrl":"a"}', "armed latch refused a letter");
assert.equal(latch.latched, false, "latch survived its one shot");

latch.toggle();
assert.equal(latch.consume("Enter"), undefined, "latch consumed an unchordable key");
assert.equal(latch.latched, true, "unchordable key disarmed the latch — it must pass through armed");
assert.equal(JSON.stringify(latch.consume("c")), '{"ctrl":"c"}', "latch lost after pass-through");
assert.equal(latch.latched, false, "latch stayed armed after a real chord");

latch.toggle();
assert.equal(latch.toggle(), false, "second toggle did not disarm");

latch.toggle();
latch.clear();
assert.equal(latch.latched, false, "clear left the latch armed");

console.log("unified key bar tests PASS");
