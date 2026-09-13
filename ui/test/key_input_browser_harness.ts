import { Terminal } from "@xterm/xterm";
import { IOSBackspaceRouter, IOS_BACKSPACE_SENTINEL, type IOSBackspaceInstrumentation } from "../src/continuous_surface/ios_backspace_router";
import type { LogicalKey } from "../src/continuous_surface/types";
import { UnifiedTerminalPage } from "../src/unified_terminal_page";
import { unifiedKeyDescriptor, type UnifiedKeyDescriptor } from "../src/unified_key_bar";
import { nativeDictationRestart } from "./fixtures/ios_firefox_dictation_restart_20260905";

type Fixture = Readonly<{
  terminal: Terminal;
  liveHost: HTMLElement;
  router: IOSBackspaceRouter;
  emitted: string[];
  sendLogicalKey(key: LogicalKey): boolean;
  input: {
    setMode(mode: "observe" | "control"): void;
    destroy(): void;
    instrumentation(): { resources: { listeners: { active: number } } };
  };
  dispose(): void;
}>;

// Mount only shared input machinery. Synthetic dispatch uses the current page's
// real methods; the fixture supplies explicit terminal and authority state.
async function fixture(): Promise<Fixture> {
  const liveHost = document.createElement("div");
  liveHost.style.cssText = "width:520px;height:260px;position:relative";
  document.body.append(liveHost);
  const terminal = new Terminal({ cols: 40, rows: 8, scrollback: 0 });
  terminal.open(liveHost);
  const dispatch = Object.assign(Object.create(UnifiedTerminalPage.prototype), {
    terminal, closed: false, syntheticKeyDepth: 0,
  }) as {
    closed: boolean; syntheticKeyDepth: number;
    dispatchSyntheticKey(descriptor: UnifiedKeyDescriptor): boolean;
    dispatchSyntheticInsertText(data: string): boolean;
  };
  let mode = "control";
  const media = matchMedia(COARSE_POINTER_QUERY);
  const sendLogicalKey = (key: LogicalKey): boolean => {
    const descriptor = unifiedKeyDescriptor(key);
    return mode === "control" && descriptor !== undefined && dispatch.dispatchSyntheticKey(descriptor);
  };
  const router = new IOSBackspaceRouter({
    liveHost,
    isFeatureEligible: () => !dispatch.closed && mode === "control" && media.matches && terminal.options.screenReaderMode !== true,
    sendLogicalKey,
    dispatchSyntheticInsert: data => dispatch.dispatchSyntheticInsertText(data),
  });
  terminal.attachCustomKeyEventHandler(event => {
    if (dispatch.closed || mode !== "control") return false;
    if (dispatch.syntheticKeyDepth > 0) return true;
    const disposition = router.handleCustomKey(event);
    return disposition !== "intercept-backspace" && disposition !== "intercept-dictation-prelude";
  });
  const cleanup: Array<() => void> = [];
  const listen = (target: EventTarget, name: string, handler: (event: Event) => void, capture = false) => {
    target.addEventListener(name, handler, capture);
    cleanup.push(() => target.removeEventListener(name, handler, capture));
  };
  listen(liveHost, "beforeinput", event => router.onBeforeInput(event as InputEvent), true);
  listen(liveHost, "input", event => router.onInputCapture(event as InputEvent), true);
  listen(liveHost, "input", event => router.onInput(event as InputEvent));
  listen(liveHost, "compositionstart", event => router.onCompositionStart(event as CompositionEvent), true);
  listen(liveHost, "compositionend", event => router.onCompositionEnd(event as CompositionEvent));
  listen(liveHost, "focusin", event => router.onFocusIn(event as FocusEvent), true);
  listen(liveHost, "blur", event => router.onBlur(event as FocusEvent), true);
  listen(document, "selectionchange", () => router.onSelectionChange());
  listen(media, "change", () => router.onPolicyChange());
  listen(terminal.textarea!, "paste", () => router.onXtermOperationComplete());
  const keySubscription = terminal.onKey(({ key, domEvent }) => router.onXtermKeyHandled(domEvent, key));
  const emitted: string[] = [];
  const subscription = terminal.onData(data => emitted.push(data));
  const input = {
    setMode(value: "observe" | "control") { mode = value; router.onPolicyChange(); },
    destroy() {
      dispatch.closed = true;
      terminal.options.disableStdin = true;
      router.destroy();
      for (const remove of cleanup.splice(0)) remove();
      keySubscription.dispose();
    },
    instrumentation: () => ({ resources: { listeners: { active: cleanup.length } } }),
  };
  return { terminal, liveHost, router, emitted, sendLogicalKey, input, dispose() {
    input.destroy(); subscription.dispose(); terminal.dispose(); liveHost.remove();
  } };
}

type NamedLogicalKey = Exclude<LogicalKey, { readonly ctrl: string }>;
type CtrlSpecialLogicalKey = "[" | "]" | "\\" | "^" | "_";
const COARSE_POINTER_QUERY = "(any-pointer: coarse)";

const EVERY_NAMED_LOGICAL_KEY: Readonly<Record<NamedLogicalKey, string>> = Object.freeze({
  escape: "\x1b",
  tab: "\t",
  backspace: "\x7f",
  delete: "\x1b[3~",
  "arrow-up": "\x1b[A",
  "arrow-down": "\x1b[B",
  "arrow-left": "\x1b[D",
  "arrow-right": "\x1b[C",
  home: "\x1b[H",
  end: "\x1b[F",
  "page-up": "\x1b[5~",
  "page-down": "\x1b[6~",
  f1: "\x1bOP",
  f2: "\x1bOQ",
  f3: "\x1bOR",
  f4: "\x1bOS",
  f5: "\x1b[15~",
  f6: "\x1b[17~",
  f7: "\x1b[18~",
  f8: "\x1b[19~",
  f9: "\x1b[20~",
  f10: "\x1b[21~",
  f11: "\x1b[23~",
  f12: "\x1b[24~",
});

const EVERY_CTRL_SPECIAL_LOGICAL_KEY: Readonly<Record<CtrlSpecialLogicalKey, string>> = Object.freeze({
  "[": "\x1b",
  "]": "\x1d",
  "\\": "\x1c",
  "^": "\x1e",
  "_": "\x1f",
});

function assert(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function show(value: string): string {
  return JSON.stringify(value);
}

function helperTextarea(subject: Fixture): HTMLTextAreaElement {
  const textarea = subject.liveHost.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
  assert(textarea, "xterm helper textarea unavailable");
  return textarea;
}

function keyboardEvent(
  key: string,
  init: Omit<KeyboardEventInit, "key" | "bubbles" | "cancelable" | "composed"> = {},
  keyCodeOverride?: number,
): KeyboardEvent {
  const event = new KeyboardEvent("keydown", {
    key,
    bubbles: true,
    cancelable: true,
    composed: true,
    ...init,
  });
  const keyCode = keyCodeOverride
    ?? (key === "Backspace" ? 8 : key === "Enter" ? 13 : /^[a-z]$/i.test(key) ? key.toUpperCase().charCodeAt(0) : 0);
  if (event.keyCode !== keyCode) Object.defineProperty(event, "keyCode", { configurable: true, get: () => keyCode });
  if (event.which !== keyCode) Object.defineProperty(event, "which", { configurable: true, get: () => keyCode });
  return event;
}

function keyboardRelease(
  key: string,
  init: Omit<KeyboardEventInit, "key" | "bubbles" | "cancelable" | "composed"> = {},
  keyCodeOverride?: number,
): KeyboardEvent {
  const event = keyboardEvent(key, init, keyCodeOverride);
  const release = new KeyboardEvent("keyup", {
    key,
    bubbles: true,
    cancelable: true,
    composed: true,
    ...init,
  });
  if (release.keyCode !== event.keyCode) {
    Object.defineProperty(release, "keyCode", { configurable: true, get: () => event.keyCode });
  }
  if (release.which !== event.which) {
    Object.defineProperty(release, "which", { configurable: true, get: () => event.which });
  }
  return release;
}

function beforeInput(textarea: HTMLTextAreaElement, inputType: string, data: string | null = null): InputEvent {
  const event = new InputEvent("beforeinput", {
    inputType,
    data,
    bubbles: true,
    cancelable: true,
    composed: true,
  });
  textarea.dispatchEvent(event);
  return event;
}

function appliedInput(
  textarea: HTMLTextAreaElement,
  inputType: string,
  data: string | null = null,
  isComposing = false,
): InputEvent {
  const event = new InputEvent("input", {
    inputType,
    data,
    isComposing,
    bubbles: true,
    cancelable: false,
    composed: true,
  });
  textarea.dispatchEvent(event);
  return event;
}

function iosInstrumentation(subject: Fixture): IOSBackspaceInstrumentation {
  const instrumentation = subject.router.instrumentation();
  assert(instrumentation, "iOS Backspace instrumentation unavailable");
  return instrumentation;
}

function emittedText(subject: Fixture): string {
  return subject.emitted.join("");
}

function send(subject: Fixture, key: LogicalKey): Readonly<{ accepted: boolean; delta: string }> {
  const before = emittedText(subject);
  const accepted = subject.sendLogicalKey(key);
  return Object.freeze({ accepted, delta: emittedText(subject).slice(before.length) });
}

function expectSend(subject: Fixture, key: LogicalKey, expected: string, label: string): void {
  const result = send(subject, key);
  assert(result.accepted, `${label} returned false`);
  assert(result.delta === expected, `${label} emitted ${show(result.delta)}, expected ${show(expected)}`);
}

function writeParsed(terminal: Terminal, data: string): Promise<void> {
  return new Promise((resolve) => terminal.write(data, resolve));
}

async function modeEvaluation(): Promise<void> {
  const subject = await fixture();
  try {
    const modeKeys = [
      ["arrow-up", "\x1b[A", "\x1bOA"],
      ["arrow-down", "\x1b[B", "\x1bOB"],
      ["arrow-left", "\x1b[D", "\x1bOD"],
      ["arrow-right", "\x1b[C", "\x1bOC"],
      ["home", "\x1b[H", "\x1bOH"],
      ["end", "\x1b[F", "\x1bOF"],
    ] as const satisfies readonly (readonly [LogicalKey, string, string])[];
    const functionKeys = (Object.keys(EVERY_NAMED_LOGICAL_KEY) as NamedLogicalKey[]).filter((key) => /^f\d+$/.test(key));
    const expectFunctionKeys = (mode: string): void => {
      for (const key of functionKeys) expectSend(subject, key, EVERY_NAMED_LOGICAL_KEY[key], `${mode} ${key}`);
    };
    for (const [key, normal] of modeKeys) expectSend(subject, key, normal, `normal ${key}`);
    expectFunctionKeys("normal-buffer normal-mode");
    await writeParsed(subject.terminal, "\x1b[?1h");
    for (const [key, , application] of modeKeys) {
      const result = send(subject, key);
      assert(result.accepted, `DECCKM application ${key} returned false`);
      assert(
        result.delta === application,
        `DECCKM application ${key} was not evaluated by xterm: expected ${show(application)}, got ${show(result.delta)}`,
      );
    }
    expectFunctionKeys("normal-buffer application-mode");
    await writeParsed(subject.terminal, "\x1b[?1049h");
    expectFunctionKeys("alternate-buffer application-mode");
    await writeParsed(subject.terminal, "\x1b[?1l");
    expectFunctionKeys("alternate-buffer normal-mode");
    for (const [key, normal] of modeKeys) {
      const result = send(subject, key);
      assert(result.accepted, `DECCKM reset ${key} returned false`);
      assert(
        result.delta === normal,
        `DECCKM reset ${key} was not evaluated by xterm: expected ${show(normal)}, got ${show(result.delta)}`,
      );
    }
    await writeParsed(subject.terminal, "\x1b[?1049l");
  } finally {
    subject.dispose();
  }
}

async function logicalCoverage(): Promise<void> {
  const subject = await fixture();
  try {
    for (const key of Object.keys(EVERY_NAMED_LOGICAL_KEY) as NamedLogicalKey[]) {
      expectSend(subject, key, EVERY_NAMED_LOGICAL_KEY[key], `logical ${key}`);
    }
    for (let code = 97; code <= 122; code += 1) {
      const letter = String.fromCharCode(code);
      expectSend(subject, { ctrl: letter }, String.fromCharCode(code - 96), `Ctrl+${letter}`);
    }
    for (const ctrl of Object.keys(EVERY_CTRL_SPECIAL_LOGICAL_KEY) as CtrlSpecialLogicalKey[]) {
      expectSend(subject, { ctrl }, EVERY_CTRL_SPECIAL_LOGICAL_KEY[ctrl], `Ctrl+${ctrl}`);
    }
  } finally {
    subject.dispose();
  }
}

async function emittedTruth(): Promise<void> {
  const subject = await fixture();
  try {
    const success = send(subject, "escape");
    assert(success.accepted === true, "emitted logical key returned false");
    assert(success.delta === "\x1b", `successful logical key delta was ${show(success.delta)}`);

    subject.terminal.attachCustomKeyEventHandler(() => false);
    const before = emittedText(subject);
    const refused = subject.sendLogicalKey("tab");
    assert(refused === false, "custom gate refusal returned true without emission");
    assert(emittedText(subject) === before, "custom gate refusal emitted bytes");
  } finally {
    subject.dispose();
  }
}

async function composedInsertTextAfterLogicalKey(): Promise<void> {
  const subject = await fixture();
  try {
    const textarea = subject.liveHost.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
    assert(textarea, "xterm helper textarea unavailable");
    const dispatchInsertText = (data: string): string => {
      const before = emittedText(subject);
      const event = new InputEvent("input", {
        data,
        inputType: "insertText",
        bubbles: true,
        cancelable: true,
        composed: true,
      });
      assert(event.composed, "insertText fixture is not composed");
      textarea.dispatchEvent(event);
      return emittedText(subject).slice(before.length);
    };

    assert(dispatchInsertText("before") === "before", "composed insertText precondition did not reach xterm");
    expectSend(subject, "arrow-left", "\x1b[D", "logical key before composed insertText");
    const after = dispatchInsertText("after");
    assert(
      after === "after",
      `composed insertText was swallowed after sendLogicalKey: expected ${show("after")}, got ${show(after)}`,
    );
  } finally {
    subject.dispose();
  }
}

async function sustainedBackspaceAndSentinelLifecycle(): Promise<void> {
  assert(matchMedia(COARSE_POINTER_QUERY).matches, "P1 harness requires emulated any-pointer: coarse");
  const subject = await fixture();
  const focusGuard = document.createElement("button");
  focusGuard.textContent = "focus guard";
  document.body.append(focusGuard);
  try {
    const textarea = helperTextarea(subject);
    textarea.focus();
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "eligible focus did not seed exactly one sentinel");
    assert(textarea.selectionStart === 1 && textarea.selectionEnd === 1, "sentinel caret was not placed after the sentinel");

    let before = emittedText(subject);
    const characterKeydown = keyboardEvent("Backspace", { code: "Backspace" });
    textarea.dispatchEvent(characterKeydown);
    assert(!characterKeydown.defaultPrevented, "character Backspace keydown was cancelled");
    assert(emittedText(subject) === before, "character Backspace keydown emitted before applied input");
    const characterBeforeInput = beforeInput(textarea, "deleteContentBackward");
    assert(!characterBeforeInput.defaultPrevented, "character delete beforeinput was cancelled");
    textarea.value = "";
    appliedInput(textarea, "deleteContentBackward");
    assert(emittedText(subject).slice(before.length) === "\x7f", "applied character delete did not emit one xterm-evaluated DEL");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "character delete did not synchronously reseed");
    let records = iosInstrumentation(subject).deleteRecords;
    assert(
      records.at(-1)?.appliedDeleteInputs === 1 && records.at(-1)?.ptyDeleteIntents === 1,
      `character delete instrumentation was not 1:1: ${JSON.stringify(records.at(-1))}`,
    );

    before = emittedText(subject);
    textarea.dispatchEvent(keyboardEvent("Backspace", { code: "Backspace" }));
    beforeInput(textarea, "deleteContentBackward");
    textarea.value = "";
    appliedInput(textarea, "deleteContentBackward");
    assert(
      emittedText(subject).slice(before.length) === "\x7f",
      "a later declared character delete was reclassified by sequence position",
    );

    before = emittedText(subject);
    const wordKeydown = keyboardEvent("Backspace", { code: "Backspace" });
    textarea.dispatchEvent(wordKeydown);
    assert(!wordKeydown.defaultPrevented, "word Backspace keydown was cancelled");
    const wordBeforeInputOne = beforeInput(textarea, "deleteWordBackward");
    const wordBeforeInputTwo = beforeInput(textarea, "deleteWordBackward");
    assert(
      !wordBeforeInputOne.defaultPrevented && !wordBeforeInputTwo.defaultPrevented,
      "paired word-delete beforeinput was cancelled",
    );
    assert(emittedText(subject) === before, "word beforeinput emitted a PTY intent");
    textarea.value = "";
    appliedInput(textarea, "deleteWordBackward");
    assert(emittedText(subject).slice(before.length) === "\x17", "applied word delete did not emit xterm-evaluated Ctrl+W");
    records = iosInstrumentation(subject).deleteRecords;
    assert(
      records.at(-1)?.appliedDeleteInputs === 1 && records.at(-1)?.ptyDeleteIntents === 1,
      `word delete instrumentation was not 1:1: ${JSON.stringify(records.at(-1))}`,
    );

    const afterFirstWordIntent = emittedText(subject);
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    textarea.value = "";
    appliedInput(textarea, "deleteWordBackward");
    assert(
      emittedText(subject) === afterFirstWordIntent,
      "a second applied word delete for one keydown emitted a second PTY intent",
    );
    records = iosInstrumentation(subject).deleteRecords;
    assert(
      records.at(-1)?.appliedDeleteInputs === 2 && records.at(-1)?.ptyDeleteIntents === 1,
      `sequence-bound paired-delete evidence was not 2:1: ${JSON.stringify(records.at(-1))}`,
    );

    before = emittedText(subject);
    const ctrlC = keyboardEvent("c", { code: "KeyC", ctrlKey: true });
    textarea.setSelectionRange(0, IOS_BACKSPACE_SENTINEL.length);
    textarea.dispatchEvent(ctrlC);
    assert(emittedText(subject).slice(before.length) === "\x03", "Ctrl+C bytes changed on the sentinel path");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "Ctrl+C did not synchronously restore the sentinel");
    assert(textarea.selectionStart === 1 && textarea.selectionEnd === 1, "Ctrl+C left the sentinel copy-selectable");

    before = emittedText(subject);
    textarea.dispatchEvent(keyboardEvent("Enter", { code: "Enter" }));
    assert(emittedText(subject).slice(before.length) === "\r", "Enter bytes changed on the sentinel path");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "Enter did not synchronously restore the sentinel");

    textarea.setSelectionRange(0, 0);
    document.dispatchEvent(new Event("selectionchange"));
    assert(textarea.selectionStart === 1 && textarea.selectionEnd === 1, "cursor movement stranded the caret before the sentinel");

    focusGuard.focus();
    assert(textarea.value.length === 0, "blur retained the sentinel");
    textarea.focus();
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "refocus did not restore the sentinel");

    textarea.dispatchEvent(keyboardEvent("Backspace", { code: "Backspace" }));
    assert(iosInstrumentation(subject).pendingKeydownOrdinal !== null, "authority fixture did not establish pending delete state");
    subject.input.setMode("observe");
    assert(textarea.value.length === 0, "authority loss retained the sentinel");
    assert(iosInstrumentation(subject).pendingKeydownOrdinal === null, "authority loss retained pending delete state");
    subject.input.setMode("control");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "authority restoration did not reseed");

    subject.terminal.options.screenReaderMode = true;
    subject.input.setMode("observe");
    subject.input.setMode("control");
    assert(textarea.value.length === 0, "screen-reader mode retained the sentinel");
    before = emittedText(subject);
    textarea.dispatchEvent(keyboardEvent("Backspace", { code: "Backspace" }));
    assert(emittedText(subject).slice(before.length) === "\x7f", "screen-reader mode did not preserve xterm's ordinary Backspace path");
    textarea.dispatchEvent(keyboardRelease("Backspace", { code: "Backspace" }));
    subject.terminal.options.screenReaderMode = false;
    subject.input.setMode("observe");
    subject.input.setMode("control");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "screen-reader deactivation did not restore eligibility");

    before = emittedText(subject);
    const manualBeforeInput = beforeInput(textarea, "insertText", "repeat repeat");
    assert(!manualBeforeInput.defaultPrevented, "manual insert beforeinput was cancelled");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "beforeinput changed the native insertion range");
    textarea.value = IOS_BACKSPACE_SENTINEL + "repeat repeat";
    textarea.setSelectionRange(textarea.value.length, textarea.value.length);
    appliedInput(textarea, "insertText", "repeat repeat");
    assert(String(textarea.value) === "repeat repeat" && Number(textarea.selectionStart) === 13, "applied insertion retained the sentinel or moved the caret");
    const manualDelta = emittedText(subject).slice(before.length);
    assert(
      manualDelta === "repeat repeat",
      `collapsed manual typing was not byte-identical: ${show(manualDelta)}`,
    );

    const clipboard = new DataTransfer();
    clipboard.setData("text/plain", "paste-probe");
    before = emittedText(subject);
    textarea.dispatchEvent(new ClipboardEvent("paste", {
      clipboardData: clipboard,
      bubbles: true,
      cancelable: true,
      composed: true,
    }));
    assert(emittedText(subject).slice(before.length) === "paste-probe", "xterm paste bytes changed");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "paste clear did not synchronously restore the sentinel");
    before = emittedText(subject);
    textarea.dispatchEvent(keyboardEvent("Backspace", { code: "Backspace" }));
    assert(emittedText(subject) === before, "paste-then-hold Backspace bypassed applied-input routing");
    textarea.value = "";
    appliedInput(textarea, "deleteContentBackward");
    assert(emittedText(subject).slice(before.length) === "\x7f", "paste-then-hold was not repeat-eligible");

    const diagnostic = JSON.stringify(iosInstrumentation(subject));
    assert(!diagnostic.includes(IOS_BACKSPACE_SENTINEL), "sentinel leaked into diagnostic output");
    assert(!diagnostic.includes("repeat"), "printable input leaked into diagnostic output");
    assert(!emittedText(subject).includes(IOS_BACKSPACE_SENTINEL), "sentinel leaked into Terminal.onData");
  } finally {
    focusGuard.remove();
    subject.dispose();
  }
}

async function compositionAdversaries(): Promise<void> {
  const subject = await fixture();
  const focusGuard = document.createElement("button");
  document.body.append(focusGuard);
  try {
    const textarea = helperTextarea(subject);
    textarea.focus();
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "composition fixture was not seeded");
    const logicalDispatches = iosInstrumentation(subject).logicalDispatches;

    textarea.dispatchEvent(new CompositionEvent("compositionstart", {
      data: "",
      bubbles: true,
      composed: true,
    }));
    assert(textarea.value.length === 0, "compositionstart did not remove the sentinel before xterm");
    assert(iosInstrumentation(subject).composing, "compositionstart did not activate the exclusion");

    for (const value of ["pinyin", "pin", "拼"]) {
      textarea.value = value;
      textarea.setSelectionRange(value.length, value.length);
      textarea.dispatchEvent(new CompositionEvent("compositionupdate", {
        data: value,
        bubbles: true,
        composed: true,
      }));
      appliedInput(textarea, "insertCompositionText", value, true);
      assert(textarea.value !== IOS_BACKSPACE_SENTINEL, "composition update seeded inside the composed range");
      assert(
        iosInstrumentation(subject).logicalDispatches === logicalDispatches,
        "composition update dispatched a terminal delete intent",
      );
    }

    const beforeEnter = emittedText(subject);
    textarea.dispatchEvent(keyboardEvent("Enter", { code: "Enter" }));
    textarea.dispatchEvent(keyboardRelease("Enter", { code: "Enter" }));
    assert(textarea.value !== IOS_BACKSPACE_SENTINEL, "Enter during composition reseeded");
    assert(
      iosInstrumentation(subject).logicalDispatches === logicalDispatches,
      "Enter during composition dispatched a P1 logical delete",
    );
    assert(!emittedText(subject).slice(beforeEnter.length).includes(IOS_BACKSPACE_SENTINEL), "composition Enter emitted the sentinel");

    focusGuard.focus();
    textarea.focus();
    assert(textarea.value !== IOS_BACKSPACE_SENTINEL, "blur/refocus during composition reseeded");
    textarea.value = "拼";
    textarea.setSelectionRange(1, 1);
    textarea.dispatchEvent(new CompositionEvent("compositionend", {
      data: "拼",
      bubbles: true,
      composed: true,
    }));
    assert(!iosInstrumentation(subject).composing, "compositionend retained composition state");
    assert(textarea.value === "拼", "compositionend rewrote or reseeded the composed field");

    const beforeImmediateTyping = emittedText(subject);
    beforeInput(textarea, "insertText", "x");
    textarea.value = "拼x";
    textarea.setSelectionRange(2, 2);
    appliedInput(textarea, "insertText", "x");
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    assert(
      iosInstrumentation(subject).logicalDispatches === logicalDispatches,
      "ordinary typing immediately after composition dispatched a P1 logical key",
    );
    assert(
      !emittedText(subject).slice(beforeImmediateTyping.length).includes(IOS_BACKSPACE_SENTINEL),
      "ordinary typing immediately after composition emitted the sentinel",
    );

    focusGuard.focus();
    textarea.focus();
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "post-composition refocus did not reseed an empty helper");
    const beforeTyping = emittedText(subject);
    beforeInput(textarea, "insertText", "x");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "post-composition beforeinput changed the native edit range");
    textarea.value = IOS_BACKSPACE_SENTINEL + "x";
    textarea.setSelectionRange(2, 2);
    appliedInput(textarea, "insertText", "x");
    assert(String(textarea.value) === "x" && Number(textarea.selectionStart) === 1, "post-composition input retained the sentinel or moved the caret");
    const postCompositionDelta = emittedText(subject).slice(beforeTyping.length);
    assert(
      postCompositionDelta === "x",
      `ordinary typing after composition was not byte-identical: ${show(postCompositionDelta)}`,
    );
    assert(!emittedText(subject).includes(IOS_BACKSPACE_SENTINEL), "composition path emitted the sentinel");
  } finally {
    focusGuard.remove();
    subject.dispose();
  }
}

// F2/F4 through the direct shared-router host and real xterm: an
// xterm-handled key between establishment and revision ends replacement
// authority (the revision goes raw at the moved cursor, never erased), and a
// composition event disables replacement for the remainder of the focus
// session until blur/refocus.
async function dictationReplacementAuthority(): Promise<void> {
  assert(matchMedia(COARSE_POINTER_QUERY).matches, "F2 harness requires emulated any-pointer: coarse");
  const subject = await fixture();
  const focusGuard = document.createElement("button");
  document.body.append(focusGuard);
  const settle = async (): Promise<void> => {
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
  };
  try {
    const textarea = helperTextarea(subject);
    const reset = (): void => {
      focusGuard.focus();
      textarea.focus();
      assert(textarea.value === IOS_BACKSPACE_SENTINEL, "reset did not return the field to the sole sentinel");
    };
    const establish = (text: string): void => {
      beforeInput(textarea, "insertText", text);
      textarea.value = text;
      textarea.setSelectionRange(text.length, text.length);
      appliedInput(textarea, "insertText", text);
      assert(
        iosInstrumentation(subject).trackedEmissionCount === text.length,
        "establishment did not store the emission count",
      );
    };
    const revise = (text: string): string => {
      const before = emittedText(subject);
      textarea.setSelectionRange(0, textarea.value.length);
      beforeInput(textarea, "insertText", text);
      textarea.value = text;
      textarea.setSelectionRange(text.length, text.length);
      appliedInput(textarea, "insertText", text);
      return emittedText(subject).slice(before.length);
    };

    // Control: the rewrite path works end to end through this host, so the
    // raw-path assertions below cannot pass vacuously.
    textarea.focus();
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "authority fixture was not seeded");
    establish("hello");
    const control = revise("hello world");
    assert(
      control === "\x7f".repeat(5) + "hello world",
      `control rewrite delta was ${show(control)}`,
    );
    assert(iosInstrumentation(subject).replacementRewrites === 1, "control rewrite was not counted");
    assert(iosInstrumentation(subject).trackedEmissionCount === 11, "control rewrite did not re-establish the run");

    // F2 vectors: navigation, a modified chord, and Delete each reach xterm's
    // key evaluator, so replacement authority ends and the next full-field
    // revision is forwarded raw.
    const vectors: ReadonlyArray<readonly [string, number, Partial<Record<"altKey" | "ctrlKey" | "metaKey" | "shiftKey", boolean>>]> = [
      ["ArrowLeft", 37, {}],
      ["Home", 36, {}],
      ["a", 65, { ctrlKey: true }],
      ["Delete", 46, {}],
    ];
    for (const [key, keyCode, modifiers] of vectors) {
      reset();
      establish("hello");
      const beforeKey = emittedText(subject);
      textarea.dispatchEvent(keyboardEvent(key, { ...modifiers }, keyCode));
      textarea.dispatchEvent(keyboardRelease(key, { ...modifiers }, keyCode));
      assert(emittedText(subject).length > beforeKey.length, `${key} did not reach xterm's key evaluator`);
      assert(
        iosInstrumentation(subject).trackedEmissionCount === null,
        `xterm-handled ${key} kept replacement authority`,
      );
      const delta = revise("hello world");
      assert(delta === "hello world", `revision after ${key} was intercepted: ${show(delta)}`);
      assert(
        iosInstrumentation(subject).replacementRewrites === 1,
        `revision after ${key} rewrote instead of falling back raw`,
      );
    }

    // F4: composition in this focus session disables replacement until
    // blur/refocus, even for a later clean dictation-shape run.
    reset();
    textarea.dispatchEvent(new CompositionEvent("compositionstart", { data: "", bubbles: true, composed: true }));
    textarea.value = "拼";
    textarea.setSelectionRange(1, 1);
    textarea.dispatchEvent(new CompositionEvent("compositionupdate", { data: "拼", bubbles: true, composed: true }));
    appliedInput(textarea, "insertCompositionText", "拼", true);
    textarea.dispatchEvent(new CompositionEvent("compositionend", { data: "拼", bubbles: true, composed: true }));
    assert(
      iosInstrumentation(subject).replacementDisabledForFocusSession === true,
      "composition did not arm the focus-session safe-disable",
    );
    await settle();
    // The IME commit consumes the single-use exclusion.
    textarea.value = "";
    beforeInput(textarea, "insertText", "拼");
    textarea.value = "拼";
    textarea.setSelectionRange(1, 1);
    appliedInput(textarea, "insertText", "拼");
    await settle();
    // A clean run establishes, but its full-field revision must go raw.
    textarea.value = "";
    establish("hello");
    const logicalBefore = iosInstrumentation(subject).logicalDispatches;
    const rawDelta = revise("hello world");
    assert(rawDelta === "hello world", `post-composition revision was intercepted: ${show(rawDelta)}`);
    assert(iosInstrumentation(subject).replacementRewrites === 1, "post-composition revision rewrote under the safe-disable");
    assert(
      iosInstrumentation(subject).logicalDispatches === logicalBefore,
      "post-composition revision dispatched erasure keys under the safe-disable",
    );
    // Blur/refocus starts a fresh session: replacement is eligible again.
    reset();
    assert(
      iosInstrumentation(subject).replacementDisabledForFocusSession === false,
      "refocus did not clear the composition safe-disable",
    );
    establish("hello");
    const restored = revise("hello world");
    assert(
      restored === "\x7f".repeat(5) + "hello world",
      `fresh-session rewrite after refocus did not intercept: ${show(restored)}`,
    );
    assert(iosInstrumentation(subject).replacementRewrites === 2, "fresh-session rewrite was not counted");
  } finally {
    focusGuard.remove();
    subject.dispose();
  }
}

async function destroySentinelLifecycle(): Promise<void> {
  const subject = await fixture();
  const textarea = helperTextarea(subject);
  textarea.focus();
  assert(textarea.value === IOS_BACKSPACE_SENTINEL, "destroy fixture was not seeded");
  const before = emittedText(subject);
  subject.input.destroy();
  assert(textarea.value.length === 0, "destroy retained the sentinel");
  const state = subject.input.instrumentation();
  assert(subject.router.instrumentation().destroyed === true, "destroy did not terminate P1 state");
  assert(state.resources.listeners.active === 0, "destroy retained controller listeners");
  textarea.dispatchEvent(keyboardEvent("Backspace", { code: "Backspace" }));
  textarea.value = "";
  appliedInput(textarea, "deleteContentBackward");
  assert(emittedText(subject) === before, "destroyed P1 listeners emitted terminal input");
  subject.dispose();
}

let pointerMediaFixture: Fixture | undefined;

async function preparePointerMediaChange(): Promise<Readonly<Record<string, boolean>>> {
  assert(matchMedia(COARSE_POINTER_QUERY).matches, "pointer-media fixture did not start coarse");
  pointerMediaFixture = await fixture();
  const textarea = helperTextarea(pointerMediaFixture);
  textarea.focus();
  assert(textarea.value === IOS_BACKSPACE_SENTINEL, "coarse pointer did not seed");
  return Object.freeze({ coarse: true, seeded: true });
}

async function assertPointerMediaDisabled(): Promise<Readonly<Record<string, boolean>>> {
  await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
  const subject = pointerMediaFixture;
  assert(subject, "pointer-media fixture unavailable");
  assert(!matchMedia(COARSE_POINTER_QUERY).matches, "pointer media did not switch away from coarse");
  const textarea = helperTextarea(subject);
  assert(textarea.value.length === 0, "pointer-media deactivation retained the sentinel");
  assert(iosInstrumentation(subject).active === false, "pointer-media deactivation retained P1 authority");
  return Object.freeze({ coarse: false, seeded: false });
}

async function assertPointerMediaRestoredAndDestroy(): Promise<Readonly<Record<string, boolean>>> {
  await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
  const subject = pointerMediaFixture;
  assert(subject, "pointer-media fixture unavailable");
  assert(matchMedia(COARSE_POINTER_QUERY).matches, "pointer media did not restore coarse");
  const textarea = helperTextarea(subject);
  assert(textarea.value === IOS_BACKSPACE_SENTINEL, "pointer-media restoration did not reseed");
  subject.input.destroy();
  assert(textarea.value.length === 0, "destroy after pointer-media restoration retained the sentinel");
  assert(subject.input.instrumentation().resources.listeners.active === 0, "pointer-media listener survived destroy");
  subject.dispose();
  pointerMediaFixture = undefined;
  return Object.freeze({ coarse: true, seeded: true, destroyed: true });
}

/**
 * UX-16 §16.7: the two WebKit dictation shapes of the 2026-09-02 iPhone
 * trace, driven through the shared router and xterm. A commit arrives as
 * an input with an empty inputType and null data (xterm forwards nothing);
 * an interim preview arrives as insertText over the tail span it inserted
 * before. Both must leave the PTY equal to the helper field; modeled
 * Backspaces in between must not end the run.
 */
async function dictationTailAndCommitShapes(): Promise<void> {
  assert(matchMedia(COARSE_POINTER_QUERY).matches, "F5 harness requires emulated any-pointer: coarse");
  const subject = await fixture();
  const focusGuard = document.createElement("button");
  document.body.append(focusGuard);
  try {
    const textarea = helperTextarea(subject);
    const insertAt = (start: number, end: number, data: string): string => {
      const before = emittedText(subject);
      textarea.setSelectionRange(start, end);
      beforeInput(textarea, "insertText", data);
      const s = textarea.selectionStart;
      const e = textarea.selectionEnd;
      textarea.value = textarea.value.slice(0, s) + data + textarea.value.slice(e);
      textarea.setSelectionRange(s + data.length, s + data.length);
      appliedInput(textarea, "insertText", data);
      return emittedText(subject).slice(before.length);
    };
    const append = (data: string): string => insertAt(textarea.value.length, textarea.value.length, data);
    const commit = (value: string, inputType = ""): string => {
      const before = emittedText(subject);
      textarea.setSelectionRange(textarea.value.length, textarea.value.length);
      beforeInput(textarea, inputType, null);
      textarea.value = value;
      textarea.setSelectionRange(value.length, value.length);
      appliedInput(textarea, inputType, null);
      return emittedText(subject).slice(before.length);
    };
    // What a line editor holds after the emitted bytes: DEL erases one code
    // point, everything else appends.
    const replay = (bytes: string): string => {
      let held = "";
      for (const unit of bytes) {
        if (unit === "\x7f") held = Array.from(held).slice(0, -1).join("");
        else held += unit;
      }
      return held;
    };
    const backspace = (): string => {
      const before = emittedText(subject);
      textarea.dispatchEvent(keyboardEvent("Backspace", { code: "Backspace" }));
      beforeInput(textarea, "deleteContentBackward");
      textarea.value = Array.from(textarea.value).slice(0, -1).join("");
      textarea.setSelectionRange(textarea.value.length, textarea.value.length);
      appliedInput(textarea, "deleteContentBackward");
      textarea.dispatchEvent(keyboardRelease("Backspace", { code: "Backspace" }));
      return emittedText(subject).slice(before.length);
    };

    textarea.focus();
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "shape fixture was not seeded");
    const runStart = emittedText(subject).length;

    // UX16-R1: a browser-applied newline in an empty-inputType field
    // commit is refused before xterm can see it. The empty run regains its
    // sentinel and remains eligible for the real first commit.
    assert(commit("\n") === "", "empty-run field-diff synthesized a line delimiter");
    assert(textarea.value === IOS_BACKSPACE_SENTINEL, "empty-run refusal did not restore the sentinel");
    assert(iosInstrumentation(subject).trackedEmissionCount === null, "empty-run refusal changed the pre-establishment tracking state");
    assert(iosInstrumentation(subject).lineDelimiterRefusals === 1, "empty-run refusal was not counted exactly once");

    // F6: commits. The first establishes an empty field; the next two extend
    // it; WebKit's trailing NBSP after a typed space must not churn.
    assert(commit("first,") === "first,", "empty-inputType commit was not sent");
    assert(iosInstrumentation(subject).trackedEmissionCount === 6, "commit did not establish the run");
    assert(append(" ") === " ", "typed space after a commit was not forwarded");
    textarea.value = textarea.value.replace(/ $/, " ");
    assert(commit("first, 1") === "1", `commit over a trailing NBSP churned: ${show(emittedText(subject).slice(runStart))}`);
    assert(commit("first, 123") === "23", "extending commit sent more than its suffix");

    // Both field-diff input types and both line delimiters restore the
    // established field/caret/run and emit nothing. The following shapes
    // therefore see precisely the same state they saw before each refusal.
    const trackedBeforeRefusal = iosInstrumentation(subject).trackedEmissionCount;
    for (const inputType of ["", "insertReplacementText"] as const) {
      for (const delimiter of ["\n", "\r"] as const) {
        assert(commit(`first, 123${delimiter}`, inputType) === "", `${show(inputType)} field-diff synthesized ${show(delimiter)}`);
        assert(String(textarea.value) === "first, 123", `${show(inputType)} refusal did not restore the field`);
        assert(textarea.selectionStart === "first, 123".length && textarea.selectionEnd === "first, 123".length,
          `${show(inputType)} refusal did not restore the end caret`);
        assert(iosInstrumentation(subject).trackedEmissionCount === trackedBeforeRefusal, `${show(inputType)} refusal ended the tracked run`);
      }
    }
    assert(iosInstrumentation(subject).lineDelimiterRefusals === 5, "field-diff refusals were not counted exactly once each");
    assert(iosInstrumentation(subject).fieldDiffRewrites === 3, "field-diff rewrites were not counted");
    assert(replay(emittedText(subject).slice(runStart)) === "first, 123", `PTY after commits: ${show(emittedText(subject).slice(runStart))}`);

    // F7: modeled Backspaces keep the run.
    assert(backspace() === "\x7f" && backspace() === "\x7f" && backspace() === "\x7f", "modeled Backspaces did not route one DEL each");
    assert(textarea.value === "first, " && iosInstrumentation(subject).trackedEmissionCount === 7, `Backspaces ended the run: ${JSON.stringify(iosInstrumentation(subject))}`);

    // F5: interim previews over the tail span, then the final commit shape.
    assert(append("o") === "o", "append after Backspaces was rewritten");
    assert(insertAt(7, 8, "on") === "n", "tail preview 'on' was not minimized");
    assert(insertAt(7, 9, "one") === "e", "tail preview 'one' was not minimized");
    assert(insertAt(7, 10, "12") === "\x7f\x7f\x7f12", `diverging preview did not erase its old tail: ${show(emittedText(subject).slice(runStart))}`);
    assert(insertAt(7, 9, "123 first one") === "3 first one", "extending preview was not minimized");
    assert(insertAt(7, 20, "1") === "\x7f".repeat(12), "final-commit preview did not erase the discarded tail");
    for (const token of ["2", "3", " ", "first", " ", "one"]) assert(append(token) === token, `final token ${show(token)} was rewritten`);
    assert(String(textarea.value) === "first, 123 first one", `field after the phrase: ${show(textarea.value)}`);
    assert(replay(emittedText(subject).slice(runStart)) === String(textarea.value), `PTY diverged from the field: ${show(emittedText(subject).slice(runStart))}`);
    assert(iosInstrumentation(subject).tailRevisionRewrites === 5, "tail-replace rewrites were not counted");
    assert(iosInstrumentation(subject).trackedEmissionCount === 20, "the run does not describe the field");

    // Fail-closed: an xterm-handled key ends the run; the next commit and the
    // next tail span both go raw (nothing sent for the commit, the span's
    // data forwarded verbatim), and neither counts as a rewrite.
    textarea.dispatchEvent(keyboardEvent("ArrowLeft", {}, 37));
    textarea.dispatchEvent(keyboardRelease("ArrowLeft", {}, 37));
    assert(iosInstrumentation(subject).trackedEmissionCount === null, "ArrowLeft kept the run");
    assert(commit("first, 123 first one!") === "", "an untracked commit was reconciled by guess");
    assert(insertAt(17, 21, "two") === "two", "an untracked tail span was rewritten");
    assert(iosInstrumentation(subject).fieldDiffRewrites === 3 && iosInstrumentation(subject).tailRevisionRewrites === 5, "raw fallbacks were counted as rewrites");
  } finally {
    focusGuard.remove();
    subject.dispose();
  }
}

async function nativeDictationRestartAfterSpace(): Promise<void> {
  const subject = await fixture();
  try {
    const textarea = helperTextarea(subject);
    textarea.focus();
    const start = emittedText(subject).length;
    let heldKey: string | undefined;
    const replay = (bytes: string): string => {
      const points: string[] = [];
      for (const point of bytes) {
        if (point === "\x7f") points.pop();
        else points.push(point);
      }
      return points.join("");
    };
    for (const entry of nativeDictationRestart) {
      if (entry.kind !== "input") {
        // Pre-edit sentinel timing changed; the represented draft did not.
        assert(textarea.value === entry.field || (textarea.value === IOS_BACKSPACE_SENTINEL && entry.field === ""), `native trace precondition at ${entry.t}: ${show(textarea.value)}`);
      } else {
        // The recorded input state is the browser's applied native edit.
        textarea.value = entry.field;
      }
      textarea.setSelectionRange(...entry.selection);
      if (entry.kind === "keydown") {
        heldKey = entry.key;
        const before = emittedText(subject).length;
        textarea.dispatchEvent(keyboardEvent(entry.key ?? "", { code: "Space" }, 32));
        // The trace logs keydown and the resulting onKey send, not keypress.
        // Chromium emits printable characters from keypress; xterm suppresses
        // it automatically if the engine already handled the keydown.
        textarea.dispatchEvent(new KeyboardEvent("keypress", {
          key: entry.key, code: "Space", keyCode: 32, charCode: 32,
          bubbles: true, cancelable: true, composed: true,
        }));
        assert(emittedText(subject).slice(before) === entry.keyText, "the physical Space did not emit exactly once through xterm");
      } else if (entry.kind === "beforeinput") {
        beforeInput(textarea, entry.inputType ?? "", entry.data ?? null);
      } else if (entry.kind === "input") {
        appliedInput(textarea, entry.inputType ?? "", entry.data ?? null);
        if (heldKey !== undefined) {
          textarea.dispatchEvent(keyboardRelease(heldKey, { code: "Space" }, 32));
          heldKey = undefined;
        }
        assert(replay(emittedText(subject).slice(start)) === entry.field.replace(/\u00a0/g, " "),
          `native restarted dictation diverged at ${entry.t}: ${show(emittedText(subject).slice(start))}`);
      }
    }
    assert(replay(emittedText(subject).slice(start)) === "test 123 best again 123", "native dictation restart final text");
    assert(iosInstrumentation(subject).trackedEmissionCount === 23, "native dictation restart lost tracking");
  } finally {
    subject.dispose();
  }
}

async function run(): Promise<Readonly<Record<string, "passed">>> {
  const cases = { modeEvaluation, logicalCoverage, emittedTruth, composedInsertTextAfterLogicalKey, sustainedBackspaceAndSentinelLifecycle, compositionAdversaries, dictationReplacementAuthority, dictationTailAndCommitShapes, nativeDictationRestartAfterSpace, destroySentinelLifecycle };
  const result: Record<string, "passed"> = {};
  for (const [name, check] of Object.entries(cases)) { await check(); result[name] = "passed"; }
  return Object.freeze(result);
}
Object.assign(window, { keyInputHarness: { run, preparePointerMediaChange, assertPointerMediaDisabled, assertPointerMediaRestoredAndDestroy }, keyInputHarnessReady: true });
