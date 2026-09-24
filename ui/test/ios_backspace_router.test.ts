import {
  IOSBackspaceRouter,
  IOS_BACKSPACE_SENTINEL,
} from "../src/continuous_surface/ios_backspace_router";
import type { LogicalKey } from "../src/continuous_surface/types";
import { nativeDictationRestart } from "./fixtures/ios_firefox_dictation_restart_20260905";

// Pure-logic coverage for the sentinel state machine and beforeinput/input
// classification the unified terminal page arms against xterm's own hidden
// textarea. The event sequences mirror what iOS emits, as pinned by the
// legacy browser harness (test/key_input_browser_harness.ts): one keydown per
// repeat, then a beforeinput/input pair whose inputType is
// deleteContentBackward (character) or deleteWordBackward (accelerated).

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void {
    if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`);
  },
  ok(value: unknown, message: string): void {
    if (!value) throw new Error(message);
  },
};

class FakeTextarea {
  value = "";
  selectionStart = 0;
  selectionEnd = 0;
  setSelectionRange(start: number, end: number): void {
    this.selectionStart = start;
    this.selectionEnd = end;
  }
}

// The router resolves document.activeElement at use time; node has no DOM, so
// the test owns a document stub (same pattern as continuous_surface_selection).
const priorDocument = (globalThis as { document?: unknown }).document;
const documentSelection = { isCollapsed: true };
const documentStub: { activeElement: unknown; getSelection(): typeof documentSelection } = {
  activeElement: null,
  getSelection: () => documentSelection,
};
(globalThis as unknown as { document: unknown }).document = documentStub;

type Fixture = Readonly<{
  router: IOSBackspaceRouter;
  textarea: FakeTextarea;
  dispatched: LogicalKey[];
  inserted: string[];
  wire: string[];
  state: { eligible: boolean; sendResult: boolean; insertResult: boolean };
  focus(): void;
  unfocus(): void;
}>;

function fixture(options?: {
  eligible?: boolean;
  onDispatch?(router: IOSBackspaceRouter, key: LogicalKey): void;
}): Fixture {
  const textarea = new FakeTextarea();
  const state = { eligible: options?.eligible ?? true, sendResult: true, insertResult: true };
  const dispatched: LogicalKey[] = [];
  const inserted: string[] = [];
  const wire: string[] = [];
  const liveHost = {
    querySelector: (selector: string) => (selector === ".xterm-helper-textarea" ? textarea : null),
  } as unknown as HTMLElement;
  const router = new IOSBackspaceRouter({
    liveHost,
    isFeatureEligible: () => state.eligible,
    sendLogicalKey: (key) => {
      dispatched.push(key);
      options?.onDispatch?.(router, key);
      if (state.sendResult && key === "backspace") wire.push("\x7f");
      return state.sendResult;
    },
    dispatchSyntheticInsert: (data) => {
      inserted.push(data);
      if (state.insertResult) wire.push(data);
      return state.insertResult;
    },
  });
  return Object.freeze({
    router,
    textarea,
    dispatched,
    inserted,
    wire,
    state,
    focus: () => {
      documentStub.activeElement = textarea;
    },
    unfocus: () => {
      documentStub.activeElement = null;
    },
  });
}

{
  const subject = fixture();
  subject.focus();
	  subject.textarea.value = IOS_BACKSPACE_SENTINEL;
  subject.textarea.selectionStart = 0;
  subject.textarea.selectionEnd = 0;
  documentSelection.isCollapsed = false;
  subject.router.onSelectionChange();
  assert.equal(subject.textarea.selectionStart, 0, "a sibling frozen selection was collapsed by the focused helper router");
  documentSelection.isCollapsed = true;
  subject.router.onSelectionChange();
  assert.equal(subject.textarea.selectionStart, IOS_BACKSPACE_SENTINEL.length, "the helper caret was not repaired after the external selection ended");
}

function keydown(target: unknown, key: string, modifiers: Partial<Record<"altKey" | "ctrlKey" | "metaKey" | "shiftKey", boolean>> = {}): KeyboardEvent {
  return {
    type: "keydown",
    key,
    altKey: false,
    ctrlKey: false,
    metaKey: false,
    shiftKey: false,
    ...modifiers,
    target,
  } as unknown as KeyboardEvent;
}

function keyup(target: unknown, key: string): KeyboardEvent {
  return {
    type: "keyup",
    key,
    altKey: false,
    ctrlKey: false,
    metaKey: false,
    shiftKey: false,
    target,
  } as unknown as KeyboardEvent;
}

function dictationPrelude(subject: Fixture): void {
  const event = keydown(subject.textarea, "Process") as KeyboardEvent & { keyCode: number; which: number };
  Object.assign(event, { keyCode: 229, which: 229 });
  assert.equal(
    subject.router.handleCustomKey(event),
    "intercept-dictation-prelude",
    "keyCode-229/Process prelude was not kept from xterm's second producer",
  );
}

function inputEvent(target: unknown, inputType: string, isComposing = false, data?: string): InputEvent {
  return {
    inputType,
    isComposing,
    target,
    data: data ?? null,
    propagationStopped: false,
    stopPropagation(): void {
      (this as { propagationStopped: boolean }).propagationStopped = true;
    },
  } as unknown as InputEvent;
}

/**
 * Drive one insertText delivery through the real browser ordering: selection
 * set, beforeinput observed against the pre-event field (handlers may mutate
 * it, e.g. sole-sentinel removal), the default edit applied at the
 * then-current selection, then the applied input through the capture-phase
 * hook — and through the bubble hook only when propagation was not stopped,
 * exactly as stopping capture propagation starves target and bubble
 * listeners in a browser.
 */
function deliverInsert(
  subject: Fixture,
  data: string,
  selection: "caret-end" | "full-field" | { start: number; end: number },
): { stopped: boolean } {
  const ta = subject.textarea;
  if (selection === "full-field") ta.setSelectionRange(0, ta.value.length);
  else if (selection === "caret-end") ta.setSelectionRange(ta.value.length, ta.value.length);
  else ta.setSelectionRange(selection.start, selection.end);
  subject.router.onBeforeInput(inputEvent(ta, "insertText", false, data));
  const start = ta.selectionStart;
  const end = ta.selectionEnd;
  ta.value = ta.value.slice(0, start) + data + ta.value.slice(end);
  ta.setSelectionRange(start + data.length, start + data.length);
  const applied = inputEvent(ta, "insertText", false, data);
  subject.router.onInputCapture(applied);
  const stopped = (applied as unknown as { propagationStopped: boolean }).propagationStopped;
  if (!stopped) {
    subject.wire.push(data);
    subject.router.onInput(applied);
  }
  return { stopped };
}

function deliverTextareaRevision(subject: Fixture, value: string): { stopped: boolean } {
  const ta = subject.textarea;
  ta.setSelectionRange(ta.value.length, ta.value.length);
  subject.router.onBeforeInput(inputEvent(ta, "insertText", false, value));
  ta.value = value;
  ta.setSelectionRange(value.length, value.length);
  const applied = inputEvent(ta, "insertText", false, value);
  subject.router.onInputCapture(applied);
  const stopped = (applied as unknown as { propagationStopped: boolean }).propagationStopped;
  if (!stopped) {
    subject.wire.push(value);
    subject.router.onInput(applied);
  }
  return { stopped };
}

function focusEvent(target: unknown): FocusEvent {
  return { target } as unknown as FocusEvent;
}

function compositionEvent(target: unknown): CompositionEvent {
  return { target } as unknown as CompositionEvent;
}

function seeded(subject: Fixture): void {
  subject.focus();
  subject.router.onFocusIn(focusEvent(subject.textarea));
  assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "fixture did not seed");
}

function replayPTYWire(wire: readonly string[]): string {
  let value = "";
  for (const item of wire) {
    if (item === "\x7f") value = Array.from(value).slice(0, -1).join("");
    else value += item;
  }
  return value;
}

function assertChainedOwnerRevision(
  label: string,
  initial: string,
  revised: string,
  revisionSuffix: string,
  previousSuffixCodePoints: number,
  revisedCodePoints: number,
): void {
  const subject = fixture();
  seeded(subject);
  const initialDelivery = deliverInsert(subject, initial, "caret-end");
  assert.ok(!initialDelivery.stopped, `${label}: initial value did not use the ordinary owner`);
  dictationPrelude(subject);
  const revision = deliverTextareaRevision(subject, revised);
  assert.ok(revision.stopped, `${label}: owner revision escaped its sole producer`);
  assert.equal(subject.dispatched.length, previousSuffixCodePoints, `${label}: owner revision mixed grapheme and code-point erase units`);
  assert.equal(subject.inserted.at(-1), revisionSuffix, `${label}: owner revision emitted the wrong suffix`);
  assert.equal(subject.router.instrumentation().trackedEmissionCount, revisedCodePoints, `${label}: owner revision stored a non-code-point count`);
  const replacement = deliverInsert(subject, "X", "full-field");
  assert.ok(replacement.stopped, `${label}: subsequent ordinary replacement escaped the tracked owner`);
  assert.equal(subject.dispatched.length, previousSuffixCodePoints + revisedCodePoints, `${label}: subsequent replacement erased the wrong code-point count`);
  assert.equal(subject.router.instrumentation().trackedEmissionCount, 1, `${label}: subsequent replacement did not settle the tracked count`);
  assert.equal(replayPTYWire(subject.wire), "X", `${label}: chained owner revision left stale PTY content`);
}

try {
  // --- the sentinel itself ---------------------------------------------------

  assert.equal(IOS_BACKSPACE_SENTINEL, String.fromCharCode(0x200b), "sentinel drifted from the zero-width space");
  assert.equal(IOS_BACKSPACE_SENTINEL.length, 1, "sentinel must be exactly one UTF-16 unit");

  // --- seeding on eligible focus ----------------------------------------------

  {
    const subject = fixture();
    subject.focus();
    subject.router.onFocusIn(focusEvent(subject.textarea));
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "eligible focus did not seed exactly one sentinel");
    assert.ok(
      subject.textarea.selectionStart === 1 && subject.textarea.selectionEnd === 1,
      "sentinel caret was not placed after the sentinel",
    );
    // A focusin for some other element must not seed another field's state.
    const other = new FakeTextarea();
    subject.router.onBlur(focusEvent(other));
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "foreign blur removed the sentinel");
  }

  // --- iOS character delete: keydown, beforeinput, applied input --------------

  {
    const subject = fixture();
    seeded(subject);
    assert.equal(
      subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")),
      "intercept-backspace",
      "eligible Backspace keydown was not intercepted",
    );
    subject.router.onBeforeInput(inputEvent(subject.textarea, "deleteContentBackward"));
    assert.equal(subject.dispatched.length, 0, "beforeinput dispatched before the delete applied");
    subject.textarea.value = "";
    subject.router.onInput(inputEvent(subject.textarea, "deleteContentBackward"));
    assert.equal(JSON.stringify(subject.dispatched), '["backspace"]', "applied character delete did not route one Backspace");
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "character delete did not synchronously reseed");
    const record = subject.router.instrumentation().deleteRecords.at(-1);
    assert.equal(JSON.stringify(record), '{"keydownOrdinal":1,"appliedDeleteInputs":1,"ptyDeleteIntents":1}', "character delete instrumentation was not 1:1");
  }

  // --- iOS word delete: paired beforeinput, 2 applied inputs, 1 intent --------

  {
    const subject = fixture();
    seeded(subject);
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")), "intercept-backspace", "word keydown was not intercepted");
    subject.router.onBeforeInput(inputEvent(subject.textarea, "deleteWordBackward"));
    subject.router.onBeforeInput(inputEvent(subject.textarea, "deleteWordBackward"));
    assert.equal(subject.dispatched.length, 0, "paired word-delete beforeinput dispatched a PTY intent");
    subject.textarea.value = "";
    subject.router.onInput(inputEvent(subject.textarea, "deleteWordBackward"));
    assert.equal(JSON.stringify(subject.dispatched), '[{"ctrl":"w"}]', "applied word delete did not route Ctrl+W");
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "word delete did not synchronously reseed");
    // A second applied delete for the same keydown is evidence, not a second intent.
    subject.textarea.value = "";
    subject.router.onInput(inputEvent(subject.textarea, "deleteWordBackward"));
    assert.equal(subject.dispatched.length, 1, "a second applied delete for one keydown emitted a second PTY intent");
    const record = subject.router.instrumentation().deleteRecords.at(-1);
    assert.equal(JSON.stringify(record), '{"keydownOrdinal":1,"appliedDeleteInputs":2,"ptyDeleteIntents":1}', "sequence-bound paired-delete evidence was not 2:1");
  }

  // --- hold repeat: each repeat keydown routes exactly once --------------------

  {
    const subject = fixture();
    seeded(subject);
    for (let repeat = 0; repeat < 3; repeat += 1) {
      assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")), "intercept-backspace", `repeat ${repeat} was not intercepted`);
      subject.router.onBeforeInput(inputEvent(subject.textarea, "deleteContentBackward"));
      subject.textarea.value = "";
      subject.router.onInput(inputEvent(subject.textarea, "deleteContentBackward"));
    }
    assert.equal(JSON.stringify(subject.dispatched), '["backspace","backspace","backspace"]', "hold repeat did not route 1:1 per keydown");
    assert.equal(subject.router.instrumentation().keydownCount, 3, "hold repeat keydown count drifted");
  }

  // --- keyup ends the pending record; orphan deletes route nothing ------------

  {
    const subject = fixture();
    seeded(subject);
    subject.router.handleCustomKey(keydown(subject.textarea, "Backspace"));
    assert.ok(subject.router.instrumentation().pendingKeydownOrdinal !== null, "keydown did not establish pending state");
    assert.equal(subject.router.handleCustomKey(keyup(subject.textarea, "Backspace")), "pass", "Backspace keyup was not passed through");
    assert.equal(subject.router.instrumentation().pendingKeydownOrdinal, null, "keyup retained the pending record");
    subject.textarea.value = "";
    subject.router.onInput(inputEvent(subject.textarea, "deleteContentBackward"));
    assert.equal(subject.dispatched.length, 0, "an orphan delete without a pending keydown routed a PTY intent");
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "orphan delete did not reseed");
  }

  // --- beforeinput classification ----------------------------------------------

  {
    // Preserve the native edit range; remove a retained sentinel after input.
    const subject = fixture();
    seeded(subject);
    subject.router.handleCustomKey(keydown(subject.textarea, "Backspace"));
    subject.router.onBeforeInput(inputEvent(subject.textarea, "insertText"));
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "beforeinput changed the native edit range");
    assert.equal(subject.router.instrumentation().pendingKeydownOrdinal, null, "insertion retained the pending delete");
    subject.textarea.value = IOS_BACKSPACE_SENTINEL + "x";
    subject.textarea.setSelectionRange(2, 2);
    subject.router.onInputCapture(inputEvent(subject.textarea, "insertText", false, "x"));
    subject.router.onInput(inputEvent(subject.textarea, "insertText"));
    assert.equal(subject.dispatched.length, 0, "insertion routed a PTY intent");
    assert.equal(subject.textarea.value, "x", "insertion was reseeded over");
    assert.equal(subject.textarea.selectionStart, 1, "sentinel removal moved the caret away from the inserted letter");
  }

  {
    // A non-backward delete clears pending but leaves the sentinel alone.
    const subject = fixture();
    seeded(subject);
    subject.router.handleCustomKey(keydown(subject.textarea, "Backspace"));
    subject.router.onBeforeInput(inputEvent(subject.textarea, "deleteContentForward"));
    assert.equal(subject.router.instrumentation().pendingKeydownOrdinal, null, "forward delete retained the pending record");
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "forward delete beforeinput removed the sentinel");
  }

  // --- composition exclusion (CJK IME, dictation) ------------------------------

  {
    const subject = fixture();
    seeded(subject);
    subject.router.onCompositionStart(compositionEvent(subject.textarea));
    assert.equal(subject.textarea.value, "", "compositionstart did not remove the sentinel before xterm records positions");
    assert.ok(subject.router.instrumentation().composing, "compositionstart did not activate the exclusion");
    // Mid-composition updates never route and never reseed.
    subject.textarea.value = "拼";
    subject.router.onInput(inputEvent(subject.textarea, "insertCompositionText", true));
    assert.equal(subject.dispatched.length, 0, "composition update dispatched a terminal intent");
    assert.equal(subject.textarea.value, "拼", "composition update was reseeded over");
    // Backspace inside a composition belongs to the IME, not the terminal.
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")), "pass", "Backspace during composition was intercepted");
    subject.textarea.value = "";
    subject.router.onInput(inputEvent(subject.textarea, "deleteContentBackward", true));
    assert.equal(subject.dispatched.length, 0, "a composing delete routed a PTY intent");
    subject.router.onCompositionEnd(compositionEvent(subject.textarea));
    assert.ok(!subject.router.instrumentation().composing, "compositionend retained composition state");
    assert.equal(subject.textarea.value, "", "compositionend itself reseeded — the next event owns that");
    // The next focus cycle reconciles the now-empty field.
    subject.router.onFocusIn(focusEvent(subject.textarea));
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "post-composition refocus did not reseed");
  }

  // --- eligibility gates ---------------------------------------------------------

  {
    const subject = fixture({ eligible: false });
    subject.focus();
    subject.router.onFocusIn(focusEvent(subject.textarea));
    assert.equal(subject.textarea.value, "", "ineligible focus seeded a sentinel");
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")), "pass", "ineligible Backspace was intercepted");
  }

  {
    const subject = fixture();
    seeded(subject);
    subject.state.eligible = false;
    subject.router.onPolicyChange();
    assert.equal(subject.textarea.value, "", "eligibility loss retained the sentinel");
    subject.state.eligible = true;
    subject.router.onPolicyChange();
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "eligibility restoration did not reseed");
  }

  {
    const subject = fixture();
    seeded(subject);
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace", { ctrlKey: true })), "pass", "modified Backspace was intercepted");
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "a")), "pass", "a non-Backspace key was intercepted");
    subject.textarea.value = "";
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")), "pass", "an empty field intercepted — there is nothing for iOS to delete");
  }

  // --- reentrancy: the router's own dispatch is never an operator keydown ------

  {
    let reentrant: string | undefined;
    const subject = fixture({
      onDispatch: (router) => {
        reentrant = router.handleCustomKey(keydown(documentStub.activeElement, "Backspace"));
      },
    });
    seeded(subject);
    subject.router.handleCustomKey(keydown(subject.textarea, "Backspace"));
    subject.textarea.value = "";
    subject.router.onInput(inputEvent(subject.textarea, "deleteContentBackward"));
    assert.equal(reentrant, "logical-dispatch", "a keydown seen during logical dispatch was not classified as such");
    assert.equal(subject.dispatched.length, 1, "reentrant classification double-routed");
  }

  // --- caret pinning ---------------------------------------------------------------

  {
    const subject = fixture();
    seeded(subject);
    subject.textarea.setSelectionRange(0, 0);
    subject.router.onSelectionChange();
    assert.ok(
      subject.textarea.selectionStart === 1 && subject.textarea.selectionEnd === 1,
      "cursor movement stranded the caret before the sentinel",
    );
    // Keydown repairs the caret before classification, so the native delete
    // always has the sentinel behind it.
    subject.textarea.setSelectionRange(0, 1);
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")), "intercept-backspace", "caret repair broke interception");
    assert.ok(
      subject.textarea.selectionStart === 1 && subject.textarea.selectionEnd === 1,
      "keydown did not repair the sentinel caret",
    );
    // A field holding real text is never caret-managed.
    subject.textarea.value = "real";
    subject.textarea.setSelectionRange(0, 0);
    subject.router.onSelectionChange();
    assert.ok(
      subject.textarea.selectionStart === 0 && subject.textarea.selectionEnd === 0,
      "selection pinning touched a non-sentinel field",
    );
  }

  // --- blur and focus lifecycle -----------------------------------------------------

  {
    const subject = fixture();
    seeded(subject);
    subject.unfocus();
    subject.router.onBlur(focusEvent(subject.textarea));
    assert.equal(subject.textarea.value, "", "blur retained the sentinel");
    subject.focus();
    subject.router.onFocusIn(focusEvent(subject.textarea));
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "refocus did not restore the sentinel");
    // Only the sole sentinel is ever removed; residual real text is xterm's.
    subject.textarea.value = "real";
    subject.router.onBlur(focusEvent(subject.textarea));
    assert.equal(subject.textarea.value, "real", "blur removed non-sentinel textarea content");
  }

  // --- xterm operation reconciliation (Enter / Ctrl+C clear the field) --------------

  {
    const subject = fixture();
    seeded(subject);
    subject.textarea.value = ""; // xterm's CR path cleared the field
    subject.router.onXtermKeyHandled(keydown(subject.textarea, "Enter"));
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "Enter did not synchronously restore the sentinel");
    subject.textarea.value = ""; // xterm's ETX path cleared the field
    subject.router.onXtermKeyHandled(keydown(subject.textarea, "c", { ctrlKey: true }));
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "Ctrl+C did not synchronously restore the sentinel");
    subject.textarea.value = "";
    subject.router.onXtermKeyHandled(keydown(subject.textarea, "a"));
    assert.equal(subject.textarea.value, "", "an ordinary key reconciled the field — only Enter/Ctrl+C clear it");
    subject.router.onXtermOperationComplete(); // the paste path
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "paste reconciliation did not reseed");
  }

  // The physical character is not evidence that its native edit happened.
  // Missing/mismatched field edits and authority changes must never grant an
  // erase budget over text the router cannot account for.
  for (const scenario of ["wrong-data", "wrong-field", "moved-caret", "navigation", "composition", "modified-key", "wrong-key-bytes"] as const) {
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    const key = keydown(subject.textarea, " ", scenario === "modified-key" ? { ctrlKey: true } : {});
    subject.router.handleCustomKey(key);
    subject.router.onXtermKeyHandled(key, scenario === "wrong-key-bytes" ? "\x00" : " ");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, `${scenario}: an unapplied key retained erase authority`);
    if (scenario === "navigation") {
      const arrow = keydown(subject.textarea, "ArrowLeft");
      subject.router.handleCustomKey(arrow);
      subject.router.onXtermKeyHandled(arrow, "\x1b[D");
    }
    if (scenario === "composition") subject.router.onCompositionStart({ target: subject.textarea } as unknown as CompositionEvent);
    if (scenario === "moved-caret") subject.textarea.setSelectionRange(2, 2);
    const data = scenario === "wrong-data" ? "x" : " ";
    subject.router.onBeforeInput(inputEvent(subject.textarea, "insertText", false, data));
    subject.textarea.value = scenario === "wrong-field" ? "other " : `hello${data}`;
    subject.textarea.setSelectionRange(subject.textarea.value.length, subject.textarea.value.length);
    subject.router.onInputCapture(inputEvent(subject.textarea, "insertText", false, data));
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, `${scenario}: unmatched key edit restored erase authority`);
    assert.equal(subject.dispatched.length, 0, `${scenario}: unmatched key edit erased terminal text`);
    assert.equal(subject.inserted.length, 0, `${scenario}: unmatched key edit synthesized text`);
  }

  // --- unmodeled xterm keys end replacement authority ----------------------

  {
    // Establish "hello", deliver ArrowLeft through
    // both public router hooks, then the full-field revision. Five Backspaces
    // at the moved PTY cursor would over-erase; the revision must fall back
    // to the raw path with the erase count invalidated, never guessed.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 5, "regression test run not established");
    subject.router.handleCustomKey(keydown(subject.textarea, "ArrowLeft"));
    subject.router.onXtermKeyHandled(keydown(subject.textarea, "ArrowLeft"));
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "an xterm-handled ArrowLeft kept replacement authority");
    const revision = deliverInsert(subject, "hello world", "full-field");
    assert.ok(!revision.stopped, "replacement after terminal cursor move was intercepted");
    assert.equal(subject.dispatched.length, 0, "replacement after terminal cursor move erased at the moved PTY cursor");
  }

  {
    // The full invalidating class: navigation, Delete, Escape, Tab, function
    // keys, and modified chords all change terminal cursor/editing state
    // outside a modeled beforeinput/input pair.
    const vectors: ReadonlyArray<readonly [string, Partial<Record<"altKey" | "ctrlKey" | "metaKey" | "shiftKey", boolean>>]> = [
      ["Home", {}],
      ["a", { ctrlKey: true }],
      ["Delete", {}],
      ["Escape", {}],
      ["Tab", {}],
      ["F5", {}],
      ["ArrowRight", { altKey: true }],
    ];
    for (const [key, modifiers] of vectors) {
      const subject = fixture();
      seeded(subject);
      deliverInsert(subject, "hello", "caret-end");
      subject.router.handleCustomKey(keydown(subject.textarea, key, modifiers));
      subject.router.onXtermKeyHandled(keydown(subject.textarea, key, modifiers));
      assert.equal(subject.router.instrumentation().trackedEmissionCount, null, `xterm-handled ${key} kept replacement authority`);
      const revision = deliverInsert(subject, "hello world", "full-field");
      assert.ok(!revision.stopped, `revision after xterm-handled ${key} was intercepted`);
      assert.equal(subject.dispatched.length, 0, `revision after xterm-handled ${key} erased`);
    }
  }

  {
    // The router's own erasure Backspaces echo back through xterm's key path
    // mid-rewrite. They are the modeled rewrite itself, not an operator key,
    // and must not invalidate the run the rewrite re-establishes.
    const subject = fixture({
      onDispatch: (router) => {
        router.onXtermKeyHandled(keydown(documentStub.activeElement, "Backspace"));
      },
    });
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    const revision = deliverInsert(subject, "hello world", "full-field");
    assert.ok(revision.stopped, "a rewrite whose erasure echoed through the key path lost interception");
    assert.equal(subject.dispatched.length, 5, "the erasure echo changed the dispatched count");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 11, "the erasure echo invalidated the re-established run");
  }

  {
    // Failed dispatch ends the run for good: the NEXT revision after a failed
    // erasure must not be intercepted either — the erase count is never
    // guessed from a half-applied rewrite.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    subject.state.sendResult = false;
    const failing = deliverInsert(subject, "hello world", "full-field");
    assert.ok(failing.stopped, "the failing rewrite did not intercept the raw event it modeled");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "a failed dispatch kept the tracked run");
    subject.state.sendResult = true;
    const after = deliverInsert(subject, "hello world again", "full-field");
    assert.ok(!after.stopped, "a revision after a failed dispatch was intercepted");
    assert.equal(subject.dispatched.length, 1, "a revision after a failed dispatch erased");
  }

  // --- composition disables replacement for the focus session -----------------

  {
    const subject = fixture();
    seeded(subject);
    subject.router.onCompositionStart(compositionEvent(subject.textarea));
    subject.textarea.value = "拼";
    subject.router.onInput(inputEvent(subject.textarea, "insertCompositionText", true));
    subject.router.onCompositionEnd(compositionEvent(subject.textarea));
    assert.ok(subject.router.instrumentation().replacementDisabledForFocusSession, "composition did not arm the focus-session safe-disable");
    // The IME commit consumes the single-use exclusion...
    subject.textarea.value = "";
    deliverInsert(subject, "拼", "caret-end");
    // ...and a later CLEAN dictation-shape run in the SAME focus session
    // still must not rewrite: the true post-composition boundary is frozen
    // only by the missing real pinyin trace.
    subject.textarea.value = "";
    deliverInsert(subject, "hello", "caret-end");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 5, "a clean run did not establish after composition");
    const revision = deliverInsert(subject, "hello world", "full-field");
    assert.ok(!revision.stopped, "a post-composition revision in the same focus session was intercepted");
    assert.equal(subject.dispatched.length, 0, "a post-composition revision erased");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "a refused post-composition revision kept the run");
    // Blur/refocus starts a fresh session: interception is eligible again.
    subject.unfocus();
    subject.router.onBlur(focusEvent(subject.textarea));
    subject.textarea.value = ""; // xterm clears the field on blur
    subject.focus();
    subject.router.onFocusIn(focusEvent(subject.textarea));
    assert.ok(!subject.router.instrumentation().replacementDisabledForFocusSession, "refocus did not clear the composition safe-disable");
    deliverInsert(subject, "fresh", "caret-end");
    const restored = deliverInsert(subject, "fresh words", "full-field");
    assert.ok(restored.stopped, "a fresh-session revision after refocus was not intercepted");
    assert.equal(subject.dispatched.length, 5, "the fresh-session rewrite did not erase the prior emission");
    assert.equal(JSON.stringify(subject.inserted), '["fresh words"]', "the fresh-session rewrite was not re-dispatched exactly once");
  }

  // --- dictation replacement: session-2 full-field revisions (issue #19) -------

  {
    // Scenario B shape: session 1 establishes and appends; session 2 delivers
    // growing full-field revisions. Each revision must erase exactly the
    // previous emission (stored code-point count, never DOM offsets) and
    // re-dispatch the new transcript once.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello ", "caret-end");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 6, "establishment did not store the code-point count");
    deliverInsert(subject, "world ", "caret-end");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 12, "collapsed append did not advance the count");
    assert.equal(subject.dispatched.length, 0, "session-1 collapsed inserts erased");
    assert.equal(subject.inserted.length, 0, "session-1 collapsed inserts were re-dispatched");

    const first = deliverInsert(subject, "hello world alpha", "full-field");
    assert.ok(first.stopped, "a full-field revision was forwarded raw to xterm");
    assert.equal(subject.dispatched.length, 12, "revision 1 did not erase exactly the previous emission");
    assert.ok(subject.dispatched.every((key) => key === "backspace"), "erasure used a key other than Backspace");
    assert.equal(JSON.stringify(subject.inserted), '["hello world alpha"]', "revision 1 was not re-dispatched exactly once");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 17, "revision 1 did not update the stored count");

    const second = deliverInsert(subject, "hello world alpha beta", "full-field");
    assert.ok(second.stopped, "revision 2 was forwarded raw");
    assert.equal(subject.dispatched.length, 12 + 17, "revision 2 did not erase revision 1's emission");
    assert.equal(subject.inserted.at(-1), "hello world alpha beta", "revision 2 was not re-dispatched");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 22, "revision 2 did not update the stored count");
    assert.equal(subject.router.instrumentation().replacementRewrites, 2, "rewrite instrumentation drifted");
    assert.equal(subject.textarea.value, "hello world alpha beta", "the browser's field replacement did not stand");
  }

  // --- dictation replacement: RUN2 measured ladder ------------------------------

  {
    // The real-iPhone within-session shape: every revision replaces
    // 0..priorLength. The first lands over the sentinel-only field and must
    // establish (prior emission zero), never erase.
    const subject = fixture();
    seeded(subject);
    const revisions = ["one", "one t", "one tw", "one two three", "one two three fo", "one two three four fi"];
    deliverInsert(subject, revisions[0], "full-field");
    assert.equal(subject.dispatched.length, 0, "establishment over the sentinel erased");
    assert.equal(subject.inserted.length, 0, "establishment over the sentinel re-dispatched");
    let expectedBackspaces = 0;
    for (let index = 1; index < revisions.length; index += 1) {
      expectedBackspaces += revisions[index - 1].length;
      const outcome = deliverInsert(subject, revisions[index], "full-field");
      assert.ok(outcome.stopped, `ladder revision ${index} was forwarded raw`);
    }
    assert.equal(subject.dispatched.length, expectedBackspaces, "ladder erasure did not track the prior emission exactly");
    assert.equal(JSON.stringify(subject.inserted), JSON.stringify(revisions.slice(1)), "ladder revisions were not re-dispatched 1:1");
    assert.equal(subject.textarea.value, revisions.at(-1), "ladder final field content drifted");
  }

  // --- repeated-word control: content is never compared -------------------------

  {
    // Scenario E: a legitimately repeated word as collapsed inserts. Any
    // content-based dedup would corrupt this; the router must stand aside.
    const subject = fixture();
    seeded(subject);
    for (let i = 0; i < 3; i += 1) {
      const outcome = deliverInsert(subject, "go ", "caret-end");
      assert.ok(!outcome.stopped, "a collapsed repeated-word insert was intercepted");
    }
    assert.equal(subject.dispatched.length, 0, "repeated words triggered erasure");
    assert.equal(subject.inserted.length, 0, "repeated words were re-dispatched");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 9, "repeated-word appends did not advance the count");
  }

  // --- replacement predicate: composition is a hard exclusion --------------------

  {
    // A full-span insertText mid-composition can never match the predicate.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 5, "control run not established");
    subject.router.onCompositionStart(compositionEvent(subject.textarea));
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "compositionstart did not invalidate the tracked run");
    subject.textarea.setSelectionRange(0, subject.textarea.value.length);
    subject.router.onBeforeInput(inputEvent(subject.textarea, "insertText", true, "hello world"));
    subject.textarea.value = "hello world";
    const composingInput = inputEvent(subject.textarea, "insertText", true, "hello world");
    subject.router.onInputCapture(composingInput);
    assert.ok(!(composingInput as unknown as { propagationStopped: boolean }).propagationStopped, "a composing insert was intercepted");
    assert.equal(subject.dispatched.length, 0, "a composing insert triggered erasure");
    subject.router.onCompositionEnd(compositionEvent(subject.textarea));
    // The first insert after compositionend may be the IME commit: it must
    // not establish or append, whatever shape it takes.
    subject.textarea.value = "";
    const commit = deliverInsert(subject, "中文", "caret-end");
    assert.ok(!commit.stopped, "the post-composition commit was intercepted");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "the post-composition commit established a run");
    // The exclusion is single-use: the next insertion behaves normally again.
    subject.textarea.value = "";
    deliverInsert(subject, "next", "caret-end");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 4, "the commit exclusion outlived its one input");
  }

  // --- fail-closed: uninterpretable shapes fall back, never erase ----------------

  {
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    // Partial span: cannot be modeled; the run ends without interception.
    const partial = deliverInsert(subject, "XY", { start: 2, end: 4 });
    assert.ok(!partial.stopped, "a partial-span insert was intercepted");
    assert.equal(subject.dispatched.length, 0, "a partial-span insert erased");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "a partial-span insert kept the tracked run");
    // A full-field span over untracked content must fall back to today's
    // behavior (raw forward), never guess an erase count.
    const untracked = deliverInsert(subject, "replacement", "full-field");
    assert.ok(!untracked.stopped, "an untracked full-span insert was intercepted");
    assert.equal(subject.dispatched.length, 0, "an untracked full-span insert erased");
  }

  {
    // Mid-field caret insert invalidates; a later full-span falls back.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    const midField = deliverInsert(subject, "X", { start: 2, end: 2 });
    assert.ok(!midField.stopped, "a mid-field caret insert was intercepted");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "a mid-field caret insert kept the tracked run");
  }

  {
    // Authority loss between sessions: the rewrite refuses and the run ends.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    subject.state.eligible = false;
    const revoked = deliverInsert(subject, "hello world", "full-field");
    assert.ok(!revoked.stopped, "an ineligible replacement was intercepted");
    assert.equal(subject.dispatched.length, 0, "an ineligible replacement erased");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "an ineligible replacement kept the tracked run");
  }

  {
    // A keydown between an insertion's beforeinput and its applied input
    // breaks the sequence bound: no interception, run invalidated.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    subject.textarea.setSelectionRange(0, 5);
    subject.router.onBeforeInput(inputEvent(subject.textarea, "insertText", false, "hello there"));
    subject.router.handleCustomKey(keydown(subject.textarea, "a"));
    subject.textarea.value = "hello there";
    const applied = inputEvent(subject.textarea, "insertText", false, "hello there");
    subject.router.onInputCapture(applied);
    assert.ok(!(applied as unknown as { propagationStopped: boolean }).propagationStopped, "a sequence-broken replacement was intercepted");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "an unclassified applied insert kept the tracked run");
  }

  {
    // The applied input must match the pending record's data length.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    subject.textarea.setSelectionRange(0, 5);
    subject.router.onBeforeInput(inputEvent(subject.textarea, "insertText", false, "hello a"));
    subject.textarea.value = "hello ab";
    const mismatched = inputEvent(subject.textarea, "insertText", false, "hello ab");
    subject.router.onInputCapture(mismatched);
    assert.ok(!(mismatched as unknown as { propagationStopped: boolean }).propagationStopped, "a data-length mismatch was intercepted");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "a data-length mismatch kept the tracked run");
  }

  {
    // The browser must have applied exactly the edit the metadata described.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    subject.textarea.setSelectionRange(0, 5);
    subject.router.onBeforeInput(inputEvent(subject.textarea, "insertText", false, "hello a"));
    subject.textarea.value = "helloX a"; // not the described replacement
    const diverged = inputEvent(subject.textarea, "insertText", false, "hello a");
    subject.router.onInputCapture(diverged);
    assert.ok(!(diverged as unknown as { propagationStopped: boolean }).propagationStopped, "a diverged field state was intercepted");
    assert.equal(subject.dispatched.length, 0, "a diverged field state erased");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "a diverged field state kept the tracked run");
  }

  // --- erase-count unit: code points, never UTF-16 offsets -----------------------

  {
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "\u{1F642}\u{1F642}", "caret-end"); // two astral code points, four UTF-16 units
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 2, "astral establishment counted UTF-16 units");
    const replaced = deliverInsert(subject, "\u{1F642}\u{1F642}\u{1F642}", "full-field");
    assert.ok(replaced.stopped, "an astral replacement was forwarded raw");
    assert.equal(subject.dispatched.length, 2, "astral erasure did not use the code-point count");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 3, "astral replacement stored a non-code-point count");
  }

  // --- sentinel bookkeeping: never part of the tracked emission ------------------

  {
    // A full-field span over the sole sentinel is an establishment (the prior
    // emission is zero), not a replacement.
    const subject = fixture();
    seeded(subject);
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "fixture precondition drifted");
    const overSentinel = deliverInsert(subject, "hi", "full-field");
    assert.ok(!overSentinel.stopped, "a span over the sentinel was treated as replacement");
    assert.equal(subject.dispatched.length, 0, "a span over the sentinel erased");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 2, "sentinel-only establishment miscounted");
    assert.equal(subject.textarea.value, "hi", "sentinel survived the establishment");
  }

  // --- fail-closed dispatch outcomes ---------------------------------------------

  {
    // A failed logical Backspace aborts erasure, still delivers the new
    // transcript, and ends the tracked run.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    subject.state.sendResult = false;
    const outcome = deliverInsert(subject, "hello world", "full-field");
    assert.ok(outcome.stopped, "a failing erasure forwarded the raw replacement anyway");
    assert.equal(subject.dispatched.length, 1, "erasure kept dispatching after a failed Backspace");
    assert.equal(JSON.stringify(subject.inserted), '["hello world"]', "the transcript was lost after a failed erasure");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "a failed erasure kept the tracked run");
  }

  {
    // A refused synthetic re-dispatch ends the tracked run.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    subject.state.insertResult = false;
    const outcome = deliverInsert(subject, "hello world", "full-field");
    assert.ok(outcome.stopped, "a refused re-dispatch left the raw path stopped state inconsistent");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "a refused re-dispatch kept the tracked run");
  }

  // --- run lifecycle: blur, xterm clears, destroy --------------------------------

  {
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello", "caret-end");
    subject.router.onXtermKeyHandled(keydown(subject.textarea, "Enter"));
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "Enter's field clear kept the tracked run");
    subject.textarea.value = "";
    subject.router.onFocusIn(focusEvent(subject.textarea));
    deliverInsert(subject, "fresh", "caret-end");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 5, "a fresh run did not establish after Enter");
    subject.unfocus();
    subject.router.onBlur(focusEvent(subject.textarea));
    assert.equal(subject.router.instrumentation().trackedEmissionCount, null, "blur kept the tracked run");
  }

  // --- replacement instrumentation stays text-free -------------------------------

  {
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "secret words", "caret-end");
    deliverInsert(subject, "secret words more", "full-field");
    const diagnostic = JSON.stringify(subject.router.instrumentation());
    assert.ok(!diagnostic.includes("secret"), "transcript text leaked into diagnostic output");
    assert.ok(diagnostic.includes('"replacementRewrites":1'), "instrumentation lost the rewrite counter");
  }

  // --- destroy ------------------------------------------------------------------------

  {
    const subject = fixture();
    seeded(subject);
    subject.router.destroy();
    assert.equal(subject.textarea.value, "", "destroy retained the sentinel");
    assert.ok(subject.router.instrumentation().destroyed, "destroy did not terminate router state");
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")), "pass", "a destroyed router intercepted");
    subject.router.onFocusIn(focusEvent(subject.textarea));
    assert.equal(subject.textarea.value, "", "a destroyed router reseeded");
  }

  // --- instrumentation hygiene ----------------------------------------------------------

  {
    const subject = fixture();
    seeded(subject);
    subject.router.handleCustomKey(keydown(subject.textarea, "Backspace"));
    subject.textarea.value = "";
    subject.router.onInput(inputEvent(subject.textarea, "deleteContentBackward"));
    const diagnostic = JSON.stringify(subject.router.instrumentation());
    assert.ok(!diagnostic.includes(IOS_BACKSPACE_SENTINEL), "sentinel leaked into diagnostic output");
    assert.ok(diagnostic.includes('"seeded":true'), "instrumentation lost the seeded flag");
  }

  // --- input-event-privacy privacy-preserving event-shape ledger -------------------------

  {
    type Shape = Readonly<{
      phase: "paste-complete" | "delete" | "dictation";
      inputType: "operation-complete" | "deleteContentBackward" | "insertText";
      dataUtf16: number;
      fieldUtf16: number;
      selectionStart: number;
      selectionEnd: number;
      compositionGeneration: number;
      trackedNumericCount: number | null;
      disposition: "seeded" | "logical-delete" | "pass" | "rewrite";
    }>;
    const ledger: Shape[] = [];
    const subject = fixture();
    seeded(subject);

    // Programmatic xterm paste clears the helper field. The page's single
    // sink now follows it with this reconciliation; the ledger stores shape
    // and counters only, never the clipboard or dictated text.
    subject.textarea.value = "";
    subject.router.onXtermOperationComplete();
    ledger.push(Object.freeze({
      phase: "paste-complete", inputType: "operation-complete", dataUtf16: 0,
      fieldUtf16: subject.textarea.value.length,
      selectionStart: subject.textarea.selectionStart, selectionEnd: subject.textarea.selectionEnd,
      compositionGeneration: 0, trackedNumericCount: subject.router.instrumentation().trackedEmissionCount,
      disposition: "seeded",
    }));

    for (let index = 0; index < 12; index += 1) {
      assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Backspace")), "intercept-backspace");
      subject.router.onBeforeInput(inputEvent(subject.textarea, "deleteContentBackward"));
      subject.textarea.value = "";
      subject.router.onInput(inputEvent(subject.textarea, "deleteContentBackward"));
      ledger.push(Object.freeze({
        phase: "delete", inputType: "deleteContentBackward", dataUtf16: 0,
        fieldUtf16: subject.textarea.value.length,
        selectionStart: subject.textarea.selectionStart, selectionEnd: subject.textarea.selectionEnd,
        compositionGeneration: 0, trackedNumericCount: subject.router.instrumentation().trackedEmissionCount,
        disposition: "logical-delete",
      }));
    }

    const dictationSession = (lengths: readonly number[]): void => {
      for (const length of lengths) {
        const data = "x".repeat(length);
        const priorLength = subject.textarea.value.length;
        const selectionStart = 0;
        const selectionEnd = priorLength;
        const outcome = deliverInsert(subject, data, "full-field");
        ledger.push(Object.freeze({
          phase: "dictation", inputType: "insertText", dataUtf16: length,
          fieldUtf16: priorLength, selectionStart, selectionEnd,
          compositionGeneration: 0, trackedNumericCount: subject.router.instrumentation().trackedEmissionCount,
          disposition: outcome.stopped ? "rewrite" : "pass",
        }));
      }
    };
    dictationSession([3, 5, 13]);
    subject.unfocus();
    subject.router.onBlur(focusEvent(subject.textarea));
    subject.textarea.value = "";
    subject.focus();
    subject.router.onFocusIn(focusEvent(subject.textarea));
    dictationSession([4, 7, 15]);

    assert.equal(ledger.filter((shape) => shape.phase === "delete").length, 12, "input-event privacy ledger lost held-delete shapes");
    assert.equal(ledger.filter((shape) => shape.phase === "dictation").length, 6, "input-event privacy ledger lost one dictation session");
    const encoded = JSON.stringify(ledger);
    assert.ok(!encoded.includes("xxx"), "input-event privacy ledger retained transcript content");
    assert.ok(encoded.includes('"trackedNumericCount"') && encoded.includes('"selectionStart"'), "input-event privacy ledger lost authority metadata");
    console.log(`INPUT_EVENT_PRIVACY_EVENT_SHAPES=${encoded}`);
  }

  // --- WebKit/xterm #6078 retained-prefix dictation ------------

  {
    // Real iPhone dictation can keep the prior helper value and report the
    // whole revised phrase as insertText at the collapsed end caret. The
    // already-emitted prefix is field history, not new PTY input.
    const subject = fixture();
    seeded(subject);
    const first = deliverInsert(subject, "alpha ", "caret-end");
    assert.ok(!first.stopped, "first dictation batch must use xterm's ordinary path");
    dictationPrelude(subject);
    const second = deliverTextareaRevision(subject, "alpha beta");
    assert.ok(second.stopped, "retained-prefix dictation was forwarded verbatim");
    assert.equal(JSON.stringify(subject.inserted), '["beta"]', "second dictation re-appended the retained prefix");
    assert.equal(subject.textarea.value, "alpha beta", "helper field did not reconcile to the semantic revision");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 10, "semantic revision accounting drifted");
    dictationPrelude(subject);
    const third = deliverTextareaRevision(subject, "alpha beta gamma");
    assert.ok(third.stopped, "third retained-prefix revision escaped the positional path");
    assert.equal(JSON.stringify(subject.inserted), '["beta"," gamma"]', "revision ladder duplicated or lost a suffix");
    assert.equal(subject.textarea.value, "alpha beta gamma", "revision ladder left accumulated helper history");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 16, "revision ladder accounting drifted");
    dictationPrelude(subject);
    const same = deliverTextareaRevision(subject, "alpha beta gamma");
    assert.ok(same.stopped, "same-value retained revision escaped");
    assert.equal(JSON.stringify(subject.inserted), '["beta"," gamma"]', "same-value revision emitted the retained field");
    dictationPrelude(subject);
    const shrink = deliverTextareaRevision(subject, "alpha beta");
    assert.ok(shrink.stopped, "shrinking retained revision escaped");
    assert.equal(subject.dispatched.length, 6, "shrinking revision did not erase exactly the changed suffix");
    assert.equal(subject.textarea.value, "alpha beta", "shrinking revision left helper history");
    dictationPrelude(subject);
    const changed = deliverTextareaRevision(subject, "alpha zeta");
    assert.ok(changed.stopped, "same-length changed revision escaped");
    assert.equal(subject.dispatched.length, 10, "same-length revision erased outside the changed suffix");
    assert.equal(JSON.stringify(subject.inserted), '["beta"," gamma","zeta"]', "same-length revision re-emitted the retained prefix");
    assert.equal(subject.textarea.value, "alpha zeta", "same-length revision left helper history");
    const diagnostic = JSON.stringify(subject.router.instrumentation());
    assert.ok(!diagnostic.includes("alpha") && !diagnostic.includes("beta"), "dictated text leaked into instrumentation");
  }

  {
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "你 ", "caret-end");
    dictationPrelude(subject);
    const revision = deliverTextareaRevision(subject, "你 好");
    assert.ok(revision.stopped, "CJK retained-prefix revision escaped");
    assert.equal(JSON.stringify(subject.inserted), '["好"]', "CJK suffix was split by UTF-16/code-point accounting");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 3, "CJK semantic count drifted");
  }

  {
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "a", "caret-end");
    const ordinary = deliverInsert(subject, "b", "caret-end");
    assert.ok(!ordinary.stopped, "ordinary typing was mistaken for accumulated dictation");
    assert.equal(subject.inserted.length, 0, "ordinary typing was synthetically re-dispatched");
    assert.equal(subject.router.instrumentation().trackedEmissionCount, 2, "ordinary typing accounting regressed");
  }

  {
    // keyCode/which 229 selects xterm's textarea reconciliation owner; it is
    // not evidence that the following insertText is a semantic revision.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "alpha ", "caret-end");
    dictationPrelude(subject);
    const ordinary = deliverInsert(subject, "alpha beta", "caret-end");
    assert.ok(ordinary.stopped, "ambiguous keyCode 229 escaped the single textarea producer");
    assert.equal(subject.textarea.value, "alpha alpha beta", "ordinary insertion was rewritten as dictation");
    assert.equal(subject.dispatched.length, 0, "ambiguous keyCode 229 synthesized erasure keys");
    assert.equal(JSON.stringify(subject.inserted), '["alpha beta"]', "ambiguous keyCode 229 changed the raw insertion");
  }

  {
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "👩‍💻 ", "caret-end");
    dictationPrelude(subject);
    const revision = deliverTextareaRevision(subject, "👩‍💻 ready");
    assert.ok(revision.stopped, "emoji/ZWJ textarea revision escaped its sole producer");
    assert.equal(subject.dispatched.length, 0, "emoji/ZWJ shared grapheme was erased");
    assert.equal(JSON.stringify(subject.inserted), '["ready"]', "emoji/ZWJ revision split or replayed its prefix");
  }

  // The owner path locates its prefix in graphemes, but every erasure and the
  // shared emittedCount contract are Unicode code points. A following ordinary
  // full-field replacement makes a mixed unit observable as stale PTY bytes.
  const chainedFailures: string[] = [];
  for (const args of [
    ["emoji/ZWJ", "👩‍💻 ready", "👩‍💻 set", "set", 5, 7],
    ["combining", "e\u0301 ready", "e\u0301 set", "set", 5, 6],
    ["CJK", "你 ready", "你 set", "set", 5, 5],
  ] as const) {
    const [label, initial, revised, revisionSuffix, previousSuffixCodePoints, revisedCodePoints] = args;
    try {
      assertChainedOwnerRevision(label, initial, revised, revisionSuffix, previousSuffixCodePoints, revisedCodePoints);
    } catch (error) {
      chainedFailures.push(String(error));
    }
  }
  assert.equal(chainedFailures.length, 0, `chained textarea-owner regressions: ${chainedFailures.join(" | ")}`);

  {
    const subject = fixture();
    seeded(subject);
    assert.equal(subject.router.handleCustomKey(keydown(subject.textarea, "Dead")), "pass", "dead key was claimed as textarea-owner input");
    const accent = deliverInsert(subject, "é", "caret-end");
    assert.ok(!accent.stopped && subject.dispatched.length === 0 && subject.inserted.length === 0,
      "press-and-hold/dead-key insertion was rewritten");
  }

  {
    const subject = fixture({ eligible: false });
    subject.focus();
    subject.router.onFocusIn(focusEvent(subject.textarea));
    const prelude = keydown(subject.textarea, "Process") as KeyboardEvent & { keyCode: number; which: number };
    Object.assign(prelude, { keyCode: 229, which: 229 });
    assert.equal(subject.router.handleCustomKey(prelude), "pass", "ineligible/screen-reader posture was claimed by the mobile owner");
    const ordinary = deliverInsert(subject, "spoken", "caret-end");
    assert.ok(!ordinary.stopped && subject.dispatched.length === 0 && subject.inserted.length === 0,
      "ineligible/screen-reader insertion was rewritten");
  }
  // ---------------------------------------------------------------------
  // — the Firefox-iOS (WebKit) dictation trace of 2026-09-02.
  // A dictation commit is a beforeinput/input pair with an empty inputType
  // and null data; xterm forwards neither, so nothing reaches the wire on
  // the raw path. A modeled Backspace is the intercepted keydown and its
  // deleteContentBackward pair.
  // ---------------------------------------------------------------------
  const deliverFieldCommit = (subject: Fixture, value: string, inputType = ""): { stopped: boolean } => {
    const ta = subject.textarea;
    ta.setSelectionRange(ta.value.length, ta.value.length);
    subject.router.onBeforeInput(inputEvent(ta, inputType, false));
    ta.value = value;
    ta.setSelectionRange(value.length, value.length);
    const applied = inputEvent(ta, inputType, false);
    subject.router.onInputCapture(applied);
    const stopped = (applied as unknown as { propagationStopped: boolean }).propagationStopped;
    if (!stopped) subject.router.onInput(applied);
    return { stopped };
  };
  const deliverBackspace = (
    subject: Fixture,
    options: { inputType?: "deleteContentBackward" | "deleteWordBackward"; after?: string } = {},
  ): void => {
    const ta = subject.textarea;
    const inputType = options.inputType ?? "deleteContentBackward";
    assert.equal(subject.router.handleCustomKey(keydown(ta, "Backspace")), "intercept-backspace", "Backspace was not intercepted");
    subject.router.onBeforeInput(inputEvent(ta, inputType, false));
    ta.value = options.after ?? Array.from(ta.value).slice(0, -1).join("");
    ta.setSelectionRange(ta.value.length, ta.value.length);
    subject.router.onInput(inputEvent(ta, inputType, false));
  };
  // WebKit keeps a trailing space as NBSP until text follows it.
  const webkitTrailingSpace = (subject: Fixture): void => {
    subject.textarea.value = subject.textarea.value.replace(/ $/, " ");
  };
  const wireSince = (subject: Fixture, mark: number): string => JSON.stringify(subject.wire.slice(mark));
  const tracked = (subject: Fixture): number | null => subject.router.instrumentation().trackedEmissionCount;

  {
    // Native September 5 capture: the Space key between dictation runs emits
    // through onKey, then changes the field without another xterm emission.
    const subject = fixture();
    seeded(subject);
    let keyEcho = false;
    for (const entry of nativeDictationRestart) {
      subject.textarea.value = entry.field;
      subject.textarea.setSelectionRange(...entry.selection);
      if (entry.kind === "keydown") {
        const event = keydown(subject.textarea, entry.key ?? "");
        subject.router.handleCustomKey(event);
        subject.router.onXtermKeyHandled(event, entry.keyText);
        subject.wire.push(entry.keyText ?? "");
        keyEcho = true;
      } else if (entry.kind === "beforeinput") {
        subject.router.onBeforeInput(inputEvent(subject.textarea, entry.inputType ?? "", entry.composing, entry.data ?? undefined));
      } else if (entry.kind === "input") {
        const event = inputEvent(subject.textarea, entry.inputType ?? "", entry.composing, entry.data ?? undefined);
        subject.router.onInputCapture(event);
        if (!(event as unknown as { propagationStopped: boolean }).propagationStopped) {
          if (!keyEcho && entry.inputType === "insertText") subject.wire.push(entry.data ?? "");
          subject.router.onInput(event);
        }
        keyEcho = false;
        assert.equal(replayPTYWire(subject.wire), entry.field.replace(/\u00a0/g, " "),
          `native restarted dictation diverged at ${entry.t}`);
      }
    }
    assert.equal(replayPTYWire(subject.wire), "test 123 best again 123", "native restarted dictation final text");
    assert.equal(tracked(subject), 23, "native restarted dictation lost tracking");
  }

  {
    // Sequence A of the trace: dictation into an empty field, corrections by
    // Backspace, then interim previews over the tail span and a final commit.
    const subject = fixture();
    seeded(subject);
    const ta = subject.textarea;
    // The first beforeinput ("fir") was cancelled by the browser: no input.
    subject.router.onBeforeInput(inputEvent(ta, "insertText", false, "fir"));
    assert.equal(ta.value, IOS_BACKSPACE_SENTINEL, "cancelled beforeinput changed the native field");
    let mark = subject.wire.length;
    assert.ok(deliverFieldCommit(subject, "first,").stopped, "commit A1 was not reconciled");
    assert.equal(wireSince(subject, mark), '["first,"]', "commit A1 sent the wrong bytes");
    assert.equal(tracked(subject), 6, "commit A1 did not establish the run");
    deliverInsert(subject, " ", "caret-end");
    webkitTrailingSpace(subject);
    assert.equal(ta.value, "first, ", "fixture did not model WebKit's trailing NBSP");
    mark = subject.wire.length;
    assert.ok(deliverFieldCommit(subject, "first, 1").stopped, "commit A2 was not reconciled");
    assert.equal(wireSince(subject, mark), '["1"]', "the NBSP→space transition churned the wire");
    mark = subject.wire.length;
    deliverFieldCommit(subject, "first, 123");
    assert.equal(wireSince(subject, mark), '["23"]', "commit A3 sent the wrong bytes");
    deliverInsert(subject, " ", "caret-end");
    webkitTrailingSpace(subject);
    mark = subject.wire.length;
    deliverFieldCommit(subject, "first, 123 first");
    assert.equal(wireSince(subject, mark), '["first"]', "commit A4 sent the wrong bytes");
    deliverInsert(subject, " ", "caret-end");
    webkitTrailingSpace(subject);
    deliverInsert(subject, "one", "caret-end");
    assert.equal(replayPTYWire(subject.wire), "first, 123 first one", "sequence A commits diverged from the field");
    assert.equal(tracked(subject), 20, "the run does not describe the field after the commits");
    for (let index = 0; index < 9; index += 1) deliverBackspace(subject);
    assert.equal(ta.value, "first, 123 ", "the fixture's Backspaces did not land");
    assert.equal(replayPTYWire(subject.wire), "first, 123 ", "modeled Backspaces diverged from the field");
    assert.equal(tracked(subject), 11, "modeled Backspaces ended the run");
    // Interim previews: each one replaces the tail span it inserted before.
    mark = subject.wire.length;
    assert.ok(!deliverInsert(subject, "o", "caret-end").stopped, "an append was rewritten");
    assert.ok(deliverInsert(subject, "on", { start: 11, end: 12 }).stopped, "tail preview 'on' went raw");
    assert.ok(deliverInsert(subject, "one", { start: 11, end: 13 }).stopped, "tail preview 'one' went raw");
    assert.equal(wireSince(subject, mark), '["o","n","e"]', "tail previews were not minimized to their new suffix");
    mark = subject.wire.length;
    assert.ok(deliverInsert(subject, "12", { start: 11, end: 14 }).stopped, "tail preview '12' went raw");
    assert.equal(wireSince(subject, mark), JSON.stringify(["\x7f", "\x7f", "\x7f", "12"]), "a diverging preview did not erase the old tail");
    mark = subject.wire.length;
    deliverInsert(subject, "123", { start: 11, end: 13 });
    deliverInsert(subject, "123 first one", { start: 11, end: 14 });
    assert.equal(wireSince(subject, mark), '["3"," first one"]', "extending previews were not minimized");
    mark = subject.wire.length;
    assert.ok(deliverInsert(subject, "1", { start: 11, end: 24 }).stopped, "the final-commit preview went raw");
    assert.equal(wireSince(subject, mark), JSON.stringify(Array.from({ length: 12 }, () => "\x7f")), "the final-commit preview did not erase the discarded tail");
    for (const token of ["2", "3", " ", "first", " ", "one"]) {
      assert.ok(!deliverInsert(subject, token, "caret-end").stopped, `final token ${JSON.stringify(token)} was rewritten`);
    }
    assert.equal(ta.value, "first, 123 123 first one", "fixture field after sequence A");
    assert.equal(replayPTYWire(subject.wire), ta.value, "sequence A: the PTY diverged from the field");
    const state = subject.router.instrumentation();
    assert.equal(state.tailRevisionRewrites, 6, "tail-replace rewrite count");
    assert.equal(state.fieldDiffRewrites, 4, "field-diff rewrite count");
    assert.equal(state.trackedEmissionCount, 24, "the run does not describe the field after sequence A");
  }

  {
    // Sequence B of the trace: typed text, a dictation commit appended to it,
    // then interim previews of the next phrase over a tail span that begins
    // with WebKit's NBSP.
    const subject = fixture();
    seeded(subject);
    const ta = subject.textarea;
    subject.router.onBeforeInput(inputEvent(ta, "insertText", false, "t"));
    deliverInsert(subject, "test", "caret-end");
    for (const key of [" ", "1", "2", "3", " "]) deliverInsert(subject, key, "caret-end");
    webkitTrailingSpace(subject);
    assert.equal(tracked(subject), 9, "typed run was not tracked");
    let mark = subject.wire.length;
    assert.ok(deliverFieldCommit(subject, "test 123 first one").stopped, "commit B1 was not reconciled");
    assert.equal(wireSince(subject, mark), '["first one"]', "commit B1 sent the wrong bytes");
    deliverInsert(subject, " ", "caret-end");
    webkitTrailingSpace(subject);
    mark = subject.wire.length;
    assert.ok(deliverInsert(subject, " te", { start: 18, end: 19 }).stopped, "tail preview over NBSP went raw");
    assert.equal(wireSince(subject, mark), '["te"]', "the NBSP tail was churned instead of kept");
    deliverInsert(subject, " test", { start: 18, end: 21 });
    deliverInsert(subject, " test one", { start: 18, end: 23 });
    deliverInsert(subject, " test 12", { start: 18, end: 27 });
    deliverInsert(subject, " test 123", { start: 18, end: 26 });
    mark = subject.wire.length;
    assert.ok(deliverInsert(subject, " ", { start: 18, end: 27 }).stopped, "the final-commit preview went raw");
    assert.equal(wireSince(subject, mark), JSON.stringify(Array.from({ length: 8 }, () => "\x7f")), "the final-commit preview did not erase the discarded tail");
    for (const token of ["test", " ", "1", "2", "3"]) deliverInsert(subject, token, "caret-end");
    assert.equal(ta.value, "test 123 first one test 123", "fixture field after sequence B");
    assert.equal(replayPTYWire(subject.wire), ta.value, "sequence B: the PTY diverged from the field");
    assert.equal(tracked(subject), 27, "the run does not describe the field after sequence B");
  }

  {
    // The 229 prelude followed by an empty-inputType commit.
    const subject = fixture();
    seeded(subject);
    dictationPrelude(subject);
    const mark = subject.wire.length;
    assert.ok(deliverFieldCommit(subject, "hello").stopped, "prelude + commit was not reconciled");
    assert.equal(wireSince(subject, mark), '["hello"]', "prelude + commit bytes");
    assert.equal(subject.router.instrumentation().dictationPreludePending, false, "the prelude flag outlived its commit");
  }

  {
    // iOS autocorrect acceptance: insertReplacementText, which xterm never
    // forwards, reconciles the changed suffix only.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "teh", "caret-end");
    deliverInsert(subject, " ", "caret-end");
    const mark = subject.wire.length;
    assert.ok(deliverFieldCommit(subject, "the ", "insertReplacementText").stopped, "autocorrect replacement went unreconciled");
    assert.equal(wireSince(subject, mark), JSON.stringify(["\x7f", "\x7f", "\x7f", "he "]), "autocorrect replacement bytes");
    assert.equal(replayPTYWire(subject.wire), "the ", "autocorrect replacement diverged from the field");
  }

  {
    // a field mutation must never synthesize Return. Exercise
    // both field-diff input types and both line delimiters over a tracked run;
    // the field, caret and tracking remain exactly where they were.
    for (const inputType of ["", "insertReplacementText"] as const) {
      for (const delimiter of ["\n", "\r"] as const) {
        const subject = fixture();
        seeded(subject);
        deliverInsert(subject, "printf safe", "caret-end");
        const mark = subject.wire.length;
        const trackedBefore = tracked(subject);
        const outcome = deliverFieldCommit(subject, `printf safe${delimiter}`, inputType);
        assert.ok(outcome.stopped, `${JSON.stringify(inputType)} line delimiter went raw`);
        assert.equal(subject.wire.length, mark, `${JSON.stringify(inputType)} line delimiter reached the PTY`);
        assert.equal(subject.textarea.value, "printf safe", `${JSON.stringify(inputType)} refusal did not restore the field`);
        assert.equal(subject.textarea.selectionStart, "printf safe".length, `${JSON.stringify(inputType)} refusal did not restore the start caret`);
        assert.equal(subject.textarea.selectionEnd, "printf safe".length, `${JSON.stringify(inputType)} refusal did not restore the end caret`);
        const state = subject.router.instrumentation();
        assert.equal(state.trackedEmissionCount, trackedBefore, `${JSON.stringify(inputType)} refusal ended the tracked run`);
        assert.equal(state.fieldDiffRewrites, 0, `${JSON.stringify(inputType)} refusal counted as a rewrite`);
        assert.equal(state.lineDelimiterRefusals, 1, `${JSON.stringify(inputType)} refusal was not counted exactly once`);
        const resumed = deliverFieldCommit(subject, "printf safer");
        assert.ok(resumed.stopped, `${JSON.stringify(inputType)} refusal did not preserve reconciliation authority`);
        assert.equal(wireSince(subject, mark), '["r"]', `${JSON.stringify(inputType)} refusal corrupted the next commit`);
      }
    }
  }

  {
    // The same invariant applies to the two insertText replacement paths
    // which synthesize their own PTY edits after the browser mutates the
    // helper field: neither a full-field nor a tail revision may inject Return.
    const full = fixture();
    seeded(full);
    deliverInsert(full, "safe", "caret-end");
    let mark = full.wire.length;
    assert.ok(deliverInsert(full, "unsafe\n", "full-field").stopped, "full-field line delimiter went raw");
    assert.equal(full.wire.length, mark, "full-field line delimiter reached the PTY");
    assert.equal(full.textarea.value, "safe", "full-field refusal did not restore the field");
    assert.equal(tracked(full), 4, "full-field refusal ended the tracked run");
    assert.equal(full.router.instrumentation().lineDelimiterRefusals, 1, "full-field refusal was not counted");

    const tail = fixture();
    seeded(tail);
    deliverInsert(tail, "prefix old", "caret-end");
    mark = tail.wire.length;
    assert.ok(deliverInsert(tail, "new\r", { start: 7, end: 10 }).stopped, "tail line delimiter went raw");
    assert.equal(tail.wire.length, mark, "tail line delimiter reached the PTY");
    assert.equal(tail.textarea.value, "prefix old", "tail refusal did not restore the field");
    assert.equal(tracked(tail), 10, "tail refusal ended the tracked run");
    assert.equal(tail.router.instrumentation().lineDelimiterRefusals, 1, "tail refusal was not counted");
  }

  {
    // The same refusal from the empty run restores the sole sentinel, so a
    // held mobile Backspace and the next dictation commit keep their owner.
    const subject = fixture();
    seeded(subject);
    const mark = subject.wire.length;
    assert.ok(deliverFieldCommit(subject, "\n").stopped, "empty-run line delimiter went raw");
    assert.equal(subject.wire.length, mark, "empty-run line delimiter reached the PTY");
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "empty-run refusal did not restore the sentinel");
    assert.equal(subject.textarea.selectionStart, IOS_BACKSPACE_SENTINEL.length, "empty-run refusal did not restore the sentinel start caret");
    assert.equal(subject.textarea.selectionEnd, IOS_BACKSPACE_SENTINEL.length, "empty-run refusal did not restore the sentinel end caret");
    assert.equal(tracked(subject), null, "empty-run refusal changed the pre-establishment tracking state");
    assert.equal(subject.router.instrumentation().lineDelimiterRefusals, 1, "empty-run refusal was not counted");
    assert.ok(deliverFieldCommit(subject, "safe").stopped, "empty-run refusal did not preserve the next commit");
    assert.equal(wireSince(subject, mark), '["safe"]', "next commit after empty-run refusal was corrupted");
  }

  {
    // A commit whose new suffix ends in WebKit's NBSP sends U+0020.
    const subject = fixture();
    seeded(subject);
    deliverFieldCommit(subject, "a ");
    assert.equal(JSON.stringify(subject.inserted), '["a "]', "NBSP reached the wire");
    assert.equal(tracked(subject), 2, "NBSP commit run");
  }

  {
    // Fail-closed: a commit over an untracked non-empty field is not modeled
    // and nothing is sent (xterm forwards nothing either); the run stays ended.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "abc", "caret-end");
    subject.router.onXtermKeyHandled(keydown(subject.textarea, "ArrowLeft"));
    assert.equal(tracked(subject), null, "xterm-handled key kept the run");
    const mark = subject.wire.length;
    const commit = deliverFieldCommit(subject, "abcd");
    assert.ok(!commit.stopped && subject.wire.length === mark, "an untracked commit was reconciled by guess");
    assert.equal(tracked(subject), null, "an untracked commit re-established the run");
    assert.equal(subject.router.instrumentation().fieldDiffRewrites, 0, "untracked commit counted as a rewrite");
  }

  {
    // Fail-closed: a span not anchored at the field's end goes raw and ends
    // the run; a tail span after the run ended goes raw too.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "hello world", "caret-end");
    const mid = deliverInsert(subject, "XY", { start: 2, end: 4 });
    assert.ok(!mid.stopped, "a mid-field span was rewritten");
    assert.equal(subject.wire.at(-1), "XY", "a mid-field span did not go raw");
    assert.equal(tracked(subject), null, "a mid-field span kept the run");
    const tail = deliverInsert(subject, "ld!", { start: subject.textarea.value.length - 2, end: subject.textarea.value.length });
    assert.ok(!tail.stopped && subject.wire.at(-1) === "ld!", "a tail span on an ended run was rewritten");
    assert.equal(subject.router.instrumentation().tailRevisionRewrites, 0, "raw tail spans counted as rewrites");
  }

  {
    // Modeled Backspaces keep the run down to the empty field, which reseeds
    // and re-establishes; a following commit sends only the new text.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "ab", "caret-end");
    deliverBackspace(subject);
    assert.equal(tracked(subject), 1, "first modeled Backspace ended the run");
    deliverBackspace(subject);
    assert.equal(subject.textarea.value, IOS_BACKSPACE_SENTINEL, "the empty field did not reseed");
    assert.equal(tracked(subject), 0, "the last modeled Backspace ended the run");
    const mark = subject.wire.length;
    assert.ok(deliverFieldCommit(subject, "new").stopped, "commit after Backspaces was not reconciled");
    assert.equal(wireSince(subject, mark), '["new"]', "commit after Backspaces sent the wrong bytes");
    assert.equal(replayPTYWire(subject.wire), "new", "Backspace continuity diverged from the field");
  }

  {
    // A Backspace with the caret mid-field, a word delete, and a Backspace
    // that removes more than one code point each end the run (the Backspace
    // itself is still routed exactly as before).
    const midField = fixture();
    seeded(midField);
    deliverInsert(midField, "abc", "caret-end");
    midField.textarea.setSelectionRange(1, 1);
    assert.equal(midField.router.handleCustomKey(keydown(midField.textarea, "Backspace")), "intercept-backspace", "mid-field Backspace not intercepted");
    midField.router.onBeforeInput(inputEvent(midField.textarea, "deleteContentBackward", false));
    assert.equal(tracked(midField), null, "mid-field Backspace kept the run at beforeinput");
    midField.textarea.value = "bc";
    midField.router.onInput(inputEvent(midField.textarea, "deleteContentBackward", false));
    assert.equal(midField.wire.at(-1), "\x7f", "mid-field Backspace was not routed");
    assert.equal(tracked(midField), null, "mid-field Backspace re-established the run");

    const word = fixture();
    seeded(word);
    deliverInsert(word, "ab cd", "caret-end");
    deliverBackspace(word, { inputType: "deleteWordBackward", after: "ab " });
    assert.equal(JSON.stringify(word.dispatched.at(-1)), '{"ctrl":"w"}', "word delete was not routed as Ctrl+W");
    assert.equal(tracked(word), null, "word delete kept the run");

    const cluster = fixture();
    seeded(cluster);
    deliverInsert(cluster, "ab👩‍💻", "caret-end");
    deliverBackspace(cluster, { after: "ab" });
    assert.equal(cluster.wire.at(-1), "\x7f", "cluster Backspace was not routed");
    assert.equal(tracked(cluster), null, "a multi-code-point Backspace kept the run");
  }

  {
    // Composition in the focus session refuses both new shapes.
    const subject = fixture();
    seeded(subject);
    deliverInsert(subject, "ab", "caret-end");
    subject.router.onCompositionStart(compositionEvent(subject.textarea));
    subject.router.onCompositionEnd(compositionEvent(subject.textarea));
    const commit = deliverFieldCommit(subject, "abc");
    assert.ok(!commit.stopped && subject.inserted.length === 0, "a post-composition commit was reconciled");
    const tail = deliverInsert(subject, "bcd", { start: 1, end: 3 });
    assert.ok(!tail.stopped, "a post-composition tail span was rewritten");
  }
} finally {
  (globalThis as unknown as { document: unknown }).document = priorDocument;
}

console.log("ios backspace router tests PASS");
