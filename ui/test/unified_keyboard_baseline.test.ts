import { UnifiedKeyboardBaseline, type KeyboardBaselineSample } from "../src/unified_keyboard_baseline";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void {
    if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`);
  },
  ok(value: unknown, message: string): void {
    if (!value) throw new Error(message);
  },
};

// iPhone-ish portrait: 390x844 layout, keyboard eats ~336px.
const PORTRAIT = { width: 390, rest: 844, keyboard: 508 };
// The same device rotated: 844x390 layout, keyboard eats ~240px.
const LANDSCAPE = { width: 844, rest: 390, keyboard: 150 };
// iPad split view: same height, layout width collapses.
const SPLIT = { width: 508, rest: 844, keyboard: 508 };

function sample(overrides: Partial<KeyboardBaselineSample>): KeyboardBaselineSample {
  return Object.freeze({
    scaledHeight: PORTRAIT.rest,
    layoutWidth: PORTRAIT.width,
    coarsePointer: true,
    textEntryFocused: false,
    ...overrides,
  });
}

// The shared preamble: seed at portrait rest, focus, open the keyboard.
function openedPortrait(): UnifiedKeyboardBaseline {
  const machine = new UnifiedKeyboardBaseline();
  assert.equal(machine.evaluate(sample({})), false, "resting portrait read open");
  assert.equal(
    machine.evaluate(sample({ scaledHeight: PORTRAIT.keyboard, textEntryFocused: true })),
    true,
    "portrait keyboard did not read open",
  );
  return machine;
}

// --- the pre-F5 contract stays ------------------------------------------------

{
  const machine = new UnifiedKeyboardBaseline();
  assert.equal(machine.evaluate(sample({})), false, "a resting viewport read open");
  // The high-water mark rises when a larger sample is seen at rest.
  assert.equal(machine.evaluate(sample({ scaledHeight: 900 })), false, "a growing viewport read open");
  assert.equal(machine.restingViewportHeight(), 900, "the high-water mark did not rise");
  // Chrome-sized shrink (~90px) is not a keyboard.
  assert.equal(machine.evaluate(sample({ scaledHeight: 810 })), false, "browser chrome read as a keyboard");
  // Keyboard-sized shrink is.
  assert.equal(machine.evaluate(sample({ scaledHeight: 560, textEntryFocused: true })), true, "a keyboard-sized shrink did not read open");
  // A fine pointer never reads open.
  assert.equal(machine.evaluate(sample({ scaledHeight: 560, coarsePointer: false })), false, "a fine pointer read open");
}

{
  // A width change at rest (keyboard closed) reseeds immediately — the old
  // mark describes a different device shape.
  const machine = new UnifiedKeyboardBaseline();
  machine.evaluate(sample({}));
  assert.equal(
    machine.evaluate(sample({ scaledHeight: LANDSCAPE.rest, layoutWidth: LANDSCAPE.width })),
    false,
    "a closed-keyboard rotation read open",
  );
  assert.equal(machine.restingViewportHeight(), LANDSCAPE.rest, "a closed-keyboard rotation did not reseed");
  assert.ok(!machine.reseedPending(), "a closed-keyboard rotation left a pending reseed");
}

// --- F5: portrait-open -> landscape-open --------------------------------------

{
  const machine = openedPortrait();
  // Rotation arrives while the keyboard is open and the helper is focused:
  // the sample is keyboard-reduced. The baseline must NOT learn it, and the
  // state holds OPEN (safe inset) instead of zeroing the difference.
  const rotated = machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard, layoutWidth: LANDSCAPE.width, textEntryFocused: true }));
  assert.equal(rotated, true, "rotation while the keyboard was open dropped the OPEN state");
  assert.ok(machine.reseedPending(), "the keyboard-reduced rotation sample was adopted as a baseline");
  // The 1s watchdog repeats the same reading; the state must hold, not decay.
  assert.equal(
    machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard, layoutWidth: LANDSCAPE.width, textEntryFocused: true })),
    true,
    "the watchdog re-read dropped the preserved OPEN state",
  );
  // Chrome-sized growth in the new shape is not the resting mark.
  assert.equal(
    machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard + 60, layoutWidth: LANDSCAPE.width, textEntryFocused: true })),
    true,
    "chrome-sized growth resolved the deferred reseed",
  );
  // The keyboard closing (focus retained, e.g. the dismiss button) grows the
  // viewport by keyboard depth: THAT sample is the new resting mark.
  assert.equal(
    machine.evaluate(sample({ scaledHeight: LANDSCAPE.rest, layoutWidth: LANDSCAPE.width, textEntryFocused: true })),
    false,
    "keyboard dismissal in the new orientation still read open",
  );
  assert.equal(machine.restingViewportHeight(), LANDSCAPE.rest, "dismissal did not establish the landscape resting mark");
  assert.ok(!machine.reseedPending(), "dismissal left the reseed pending");
  // The new orientation now detects its own keyboard.
  assert.equal(
    machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard, layoutWidth: LANDSCAPE.width, textEntryFocused: true })),
    true,
    "the reseeded landscape baseline cannot see its own keyboard",
  );
}

// --- F5: blur/close resolves the deferred reseed --------------------------------

{
  const machine = openedPortrait();
  machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard, layoutWidth: LANDSCAPE.width, textEntryFocused: true }));
  assert.ok(machine.reseedPending(), "rotation under an open keyboard did not defer the reseed");
  // Blur ends the hold: the machine reads closed immediately (safe bias) and
  // adopts the settled viewport through the ordinary high-water raise.
  assert.equal(
    machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard, layoutWidth: LANDSCAPE.width, textEntryFocused: false })),
    false,
    "blur did not release the preserved OPEN state",
  );
  assert.equal(
    machine.evaluate(sample({ scaledHeight: LANDSCAPE.rest, layoutWidth: LANDSCAPE.width, textEntryFocused: false })),
    false,
    "the settled post-blur viewport read open",
  );
  assert.equal(machine.restingViewportHeight(), LANDSCAPE.rest, "the post-blur settle did not become the resting mark");
}

// --- F5: split-view while open ---------------------------------------------------

{
  const machine = openedPortrait();
  const split = machine.evaluate(sample({ scaledHeight: SPLIT.keyboard, layoutWidth: SPLIT.width, textEntryFocused: true }));
  assert.equal(split, true, "entering split view while the keyboard was open dropped the OPEN state");
  assert.ok(machine.reseedPending(), "the split-view keyboard sample was adopted as a baseline");
  assert.equal(
    machine.evaluate(sample({ scaledHeight: SPLIT.rest, layoutWidth: SPLIT.width, textEntryFocused: true })),
    false,
    "keyboard dismissal in split view still read open",
  );
  assert.equal(machine.restingViewportHeight(), SPLIT.rest, "split view did not establish its resting mark");
}

// --- F5: return-to-portrait while still open ------------------------------------

{
  const machine = openedPortrait();
  machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard, layoutWidth: LANDSCAPE.width, textEntryFocused: true }));
  // Rotating back with the keyboard still up: the hold carries across the
  // second width change too.
  assert.equal(
    machine.evaluate(sample({ scaledHeight: PORTRAIT.keyboard, layoutWidth: PORTRAIT.width, textEntryFocused: true })),
    true,
    "returning to portrait while open dropped the OPEN state",
  );
  assert.ok(machine.reseedPending(), "the return-to-portrait keyboard sample was adopted as a baseline");
  // Dismissal back at portrait establishes the portrait resting mark again.
  assert.equal(
    machine.evaluate(sample({ scaledHeight: PORTRAIT.rest, layoutWidth: PORTRAIT.width, textEntryFocused: true })),
    false,
    "portrait dismissal after the round trip still read open",
  );
  assert.equal(machine.restingViewportHeight(), PORTRAIT.rest, "the round trip lost the portrait resting mark");
}

// --- F5: a pointer-class change releases the hold --------------------------------

{
  const machine = openedPortrait();
  machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard, layoutWidth: LANDSCAPE.width, textEntryFocused: true }));
  assert.equal(
    machine.evaluate(sample({ scaledHeight: LANDSCAPE.keyboard, layoutWidth: LANDSCAPE.width, coarsePointer: false, textEntryFocused: true })),
    false,
    "losing the coarse pointer did not release the preserved OPEN state",
  );
}

console.log("unified keyboard baseline tests PASS");
