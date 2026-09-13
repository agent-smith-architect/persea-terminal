import type { LogicalKey } from "./types";

export const IOS_BACKSPACE_SENTINEL = "\u200b";

export type IOSBackspaceDeleteRecord = Readonly<{
  keydownOrdinal: number;
  appliedDeleteInputs: number;
  ptyDeleteIntents: number;
}>;

export type IOSBackspaceInstrumentation = Readonly<{
  active: boolean;
  seeded: boolean;
  composing: boolean;
  pendingKeydownOrdinal: number | null;
  logicalDispatchDepth: number;
  logicalDispatches: number;
  keydownCount: number;
  trackedEmissionCount: number | null;
  pendingInsertKind: PendingInsertKind | null;
  replacementRewrites: number;
  retainedRevisionRewrites: number;
  /** UX-16 §16.7: tail-span dictation previews rewritten in place. */
  tailRevisionRewrites: number;
  /** UX-16 §16.7: field-diff reconciliations of inputs xterm never forwards. */
  fieldDiffRewrites: number;
  /**
   * UX16-R1: rewrites refused because the field text carried a line
   * delimiter (a Return the operator never pressed); field and run restored.
   */
  lineDelimiterRefusals: number;
  dictationPreludePending: boolean;
  /** F4 safe-disable: composition occurred in this focus session, so
   * replacement rewriting is off until blur/refocus. */
  replacementDisabledForFocusSession: boolean;
  deleteRecords: readonly IOSBackspaceDeleteRecord[];
  destroyed: boolean;
}>;

type MutableDeleteRecord = {
  keydownOrdinal: number;
  appliedDeleteInputs: number;
  ptyDeleteIntents: number;
  routed: boolean;
};

type PendingInsertKind =
  | "establish"
  | "append"
  | "replace"
  | "retained-revision"
  | "textarea-owner"
  | "key-echo"
  | "tail-replace"
  | "field-diff";

/**
 * Metadata-only record of one classified beforeinput, matched against the
 * immediately corresponding applied input. Instrumentation sees only the
 * kind and counts; the ephemeral text fields below pair with exactly one
 * input event and are never retained past it.
 */
type PendingInsertRecord = {
  kind: PendingInsertKind;
  dataUtf16: number;
  dataCodePoints: number;
  priorUtf16: number;
  sharedPrefixUtf16?: number;
  sharedPrefixCodePoints?: number;
  /** Ephemeral state paired with one input event; never instrumented. */
  previousValue?: string;
  /** tail-replace: the selected span the browser replaces; ephemeral. */
  replacedText?: string;
  /** key-echo: the character xterm already emitted for the physical key. */
  keyText?: string;
  /** field-diff: the inputType the paired input must carry. */
  inputType?: string;
  compositionGeneration: number;
};

type PendingKeyEcho = Readonly<{
  text: string;
  previousValue: string;
  compositionGeneration: number;
}>;

type IOSBackspaceRouterOptions = Readonly<{
  liveHost: HTMLElement;
  isFeatureEligible(): boolean;
  sendLogicalKey(key: LogicalKey): boolean;
  dispatchSyntheticInsert(data: string): boolean;
}>;

type CustomKeyDisposition = "logical-dispatch" | "intercept-backspace" | "intercept-dictation-prelude" | "pass";

const DELETE_INPUT_TYPES = new Set(["deleteContentBackward", "deleteWordBackward"]);
const DELETE_RECORD_LIMIT = 256;
/**
 * UX-16 §16.7: applied inputs xterm's `_inputEvent` never forwards (it
 * requires `data` and `inputType === "insertText"`). WebKit dictation
 * commits arrive as an empty inputType with null data and replace the field
 * wholesale; iOS autocorrect acceptance arrives as insertReplacementText.
 * Both leave the field as the only truth, so the router reconciles the PTY
 * to the field by diff when the run is tracked.
 */
const FIELD_DIFF_INPUT_TYPES = new Set(["", "insertReplacementText"]);
/**
 * UX16-R1: no rewrite path may synthesize a line delimiter. Only a
 * trusted Enter through xterm's key path sends Return; a field mutation
 * that carries CR/LF (a dictated "new line", a replacement with a break) is
 * refused whole — the raw path is stopped, the field is put back exactly as
 * it was, and the tracked run keeps describing it.
 */
const LINE_DELIMITER = /[\r\n]/;

function isUnmodifiedBackspace(event: KeyboardEvent): boolean {
  return event.type === "keydown"
    && event.key === "Backspace"
    && !event.altKey
    && !event.ctrlKey
    && !event.metaKey
    && !event.shiftKey;
}

function isInsertion(inputType: string): boolean {
  return inputType.startsWith("insert");
}

function codePointLength(data: string): number {
  let length = 0;
  for (const _ of data) length += 1;
  return length;
}

function graphemes(data: string): string[] {
  if (typeof Intl.Segmenter !== "function") return Array.from(data);
  return Array.from(new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(data), (part) => part.segment);
}

/**
 * WebKit keeps a trailing dictated/typed space in the helper field as NBSP
 * (U+00A0) and turns it back into U+0020 once text follows it, while xterm
 * forwarded the event's own U+0020. Field text is compared and re-sent in
 * its U+0020 form so a space never churns and the PTY never receives NBSP
 * from a dictation reconciliation.
 */
function normalizeSpaces(value: string): string {
  return value.replace(/ /g, " ");
}

/**
 * Split `previous` → `current` at their shared grapheme prefix: the code
 * points to erase from the PTY and the suffix to send. Grapheme boundaries
 * keep a ZWJ/combining cluster intact; the erase count is in code points
 * because that is the router's emittedCount and logical Backspace contract.
 */
function fieldDelta(previous: string, current: string): { eraseCodePoints: number; suffix: string } {
  const before = graphemes(normalizeSpaces(previous));
  const after = graphemes(normalizeSpaces(current));
  let shared = 0;
  while (shared < before.length && shared < after.length && before[shared] === after[shared]) shared += 1;
  return {
    eraseCodePoints: codePointLength(before.slice(shared).join("")),
    suffix: after.slice(shared).join(""),
  };
}

/**
 * The re-dispatched dictation revision. `composed: false` takes xterm's
 * `_inputEvent` gate down its unconditional branch: the erasure keyups have
 * already cleared the keydown-seen state, but a concurrently held physical key
 * must not turn the re-dispatch into silent loss.
 */
export function syntheticInsertTextEvent(data: string): InputEvent {
  return new InputEvent("input", {
    data,
    inputType: "insertText",
    isComposing: false,
    bubbles: true,
    cancelable: true,
    composed: false,
  });
}

/**
 * Owns only helper-textarea state and metadata. Event registration and terminal
 * authority remain in TerminalInteractionController's lifecycle ledger.
 */
export class IOSBackspaceRouter {
  private composing = false;
  private destroyed = false;
  private logicalDispatchDepth = 0;
  private logicalDispatches = 0;
  private keydownCount = 0;
  private pending: MutableDeleteRecord | undefined;
  private readonly deleteRecords: MutableDeleteRecord[] = [];
  /**
   * Code points this path previously emitted and which the field still
   * represents. The erase count for a dictation replacement — counted at send
   * time, never derived from DOM offsets or content comparison. `null` means
   * the field's relation to emitted text is unknown: the replacement rewrite
   * refuses (fail-closed, worst case is today's behavior) until an insertion
   * into an empty or sentinel-only field re-establishes the run.
   */
  private emittedCount: number | null = null;
  private pendingInsert: PendingInsertRecord | undefined;
  private pendingKeyEcho: PendingKeyEcho | undefined;
  private compositionGeneration = 0;
  /** The IME commit may follow compositionend as plain insertText; never model it. */
  private commitExclusionPending = false;
  /**
   * Whether any composition event has occurred in the current focus session.
   * The frozen repair contract (the 2026-07-28 decision packet) is
   * IMPLEMENTATION_BLOCKED_ON_REAL_CJK_TRACE: the actual post-composition
   * input boundary can only be frozen from a real device pinyin trace, which
   * does not exist yet. Until that trace reopens the gate, replacement
   * rewriting is disabled outright for the REMAINDER of any focus session
   * that saw composition — the single-commit exclusion below stays as a
   * second fence, but it models exactly one ambiguous input and is not
   * evidence-backed enough to stand alone. Blur or refocus starts a fresh
   * session and restores replacement eligibility.
   */
  private compositionSeenThisFocusSession = false;
  private syntheticInsertDepth = 0;
  private replacementRewrites = 0;
  private retainedRevisionRewrites = 0;
  private tailRevisionRewrites = 0;
  private fieldDiffRewrites = 0;
  private lineDelimiterRefusals = 0;
  private dictationPreludePending = false;
  /**
   * UX-16 §16.7: the run length expected after a modeled Backspace (an
   * intercepted deleteContentBackward with the caret at the end of a field
   * the tracked run fully describes). Set at the delete's beforeinput and
   * consumed by its applied input; any other event in between drops it.
   */
  private expectedRunAfterDelete: number | undefined;
  private insertionSentinel: "start" | "end" | undefined;

  constructor(private readonly options: IOSBackspaceRouterOptions) {}

  onPolicyChange(): void {
    if (!this.featureEligible()) {
      this.pending = undefined;
      this.dictationPreludePending = false;
      this.invalidateEmissionTracking();
      this.removeSoleSentinel();
      return;
    }
    this.seedIfEligible();
  }

  onFocusIn(event: FocusEvent): void {
    if (!this.isTextareaTarget(event.target)) return;
    // A fresh focus session: the composition safe-disable ends here.
    this.dictationPreludePending = false;
    this.compositionSeenThisFocusSession = false;
    this.seedIfEligible();
  }

  onBlur(event: FocusEvent): void {
    if (!this.isTextareaTarget(event.target)) return;
    this.pending = undefined;
    this.dictationPreludePending = false;
    // xterm clears the helper field on blur; the run ends with it, and so
    // does the focus session the composition safe-disable is scoped to.
    this.invalidateEmissionTracking();
    this.compositionSeenThisFocusSession = false;
    this.removeSoleSentinel();
  }

  onSelectionChange(): void {
    const textarea = this.textarea();
    if (!textarea || document.activeElement !== textarea || textarea.value !== IOS_BACKSPACE_SENTINEL) return;
	// A workspace has several xterm helpers but one document selection. A
	// frozen selection in a sibling pane is authoritative operator state; moving
	// the focused helper's caret here would collapse that external range. Text
	// controls expose their own caret through selectionStart/selectionEnd, not a
	// non-collapsed document Range, so this fence preserves the native helper
	// repair without taking ownership of another pane's selection.
	const selection = document.getSelection?.();
	if (selection && !selection.isCollapsed) return;
	textarea.setSelectionRange(IOS_BACKSPACE_SENTINEL.length, IOS_BACKSPACE_SENTINEL.length);
  }

  onCompositionStart(event: CompositionEvent): void {
    if (!this.isTextareaTarget(event.target)) return;
    this.insertionSentinel = undefined;
    this.pending = undefined;
    this.dictationPreludePending = false;
    this.composing = true;
    // Composition is a hard exclusion from replacement rewriting: it
    // invalidates the tracked run outright and bumps the generation so a
    // pending record straddling the edge can never match its applied input.
    this.invalidateEmissionTracking();
    this.compositionGeneration += 1;
    this.compositionSeenThisFocusSession = true;
    this.removeSoleSentinel();
  }

  onCompositionEnd(event: CompositionEvent): void {
    if (!this.isTextareaTarget(event.target)) return;
    this.pending = undefined;
    this.dictationPreludePending = false;
    this.pendingInsert = undefined;
    this.pendingKeyEcho = undefined;
    this.composing = false;
    this.compositionGeneration += 1;
    this.compositionSeenThisFocusSession = true;
    this.commitExclusionPending = true;
  }

  onBeforeInput(event: InputEvent): void {
    if (this.syntheticInsertDepth > 0) return;
    if (!this.isTextareaTarget(event.target)) return;
    this.insertionSentinel = undefined;
    if (this.composing || event.isComposing) {
      this.pending = undefined;
      this.invalidateEmissionTracking();
      return;
    }
    this.expectedRunAfterDelete = undefined;
    const keyEcho = this.pendingKeyEcho;
    this.pendingKeyEcho = undefined;
    if (keyEcho) {
      this.pending = undefined;
      this.pendingInsert = this.classifyKeyEcho(event, keyEcho);
      this.prepareInsertionSentinel();
      return;
    }
    if (FIELD_DIFF_INPUT_TYPES.has(event.inputType)) {
      this.pending = undefined;
      // Classify against the field's pre-event state, before sentinel removal
      // mutates it.
      this.pendingInsert = this.classifyFieldDiff(event);
      this.prepareInsertionSentinel();
      return;
    }
    if (isInsertion(event.inputType)) {
      this.pending = undefined;
      this.pendingInsert = this.classifyInsertion(event);
      this.prepareInsertionSentinel();
      return;
    }
    this.dictationPreludePending = false;
    this.pendingInsert = undefined;
    if (DELETE_INPUT_TYPES.has(event.inputType)) {
      // A Backspace this router intercepted routes exactly one logical
      // Backspace; when the caret sits at the end of a field the tracked run
      // fully describes, the run simply shrinks by the one code point the
      // field loses (verified at the applied input). Word deletes and every
      // other delete shape change the field by an amount event metadata
      // cannot express in emitted code points; those end the run, fail-closed.
      this.expectedRunAfterDelete = this.classifyModeledDelete(event);
      if (this.expectedRunAfterDelete === undefined) this.emittedCount = null;
      return;
    }
    // Every other non-insert mutation: the tracked run ends here.
    this.invalidateEmissionTracking();
    this.pending = undefined;
  }

  /**
   * Capture-phase applied-input entry point, registered ahead of xterm's own
   * textarea listener. This is the only site allowed to stop a raw dictation
   * replacement from reaching xterm's verbatim forward.
   */
  onInputCapture(event: InputEvent): void {
    if (this.syntheticInsertDepth > 0) return;
    if (!this.isTextareaTarget(event.target)) return;
    this.removeAppliedInsertionSentinel();
    this.pendingKeyEcho = undefined;
    const record = this.pendingInsert;
    this.pendingInsert = undefined;
    if (this.composing || event.isComposing) {
      this.emittedCount = null;
      return;
    }
    const data = typeof event.data === "string" ? event.data : "";
    if (!record) {
      // An applied insertion this router never classified (its pending record
      // was dropped by an intervening event) leaves the field diverged from
      // the tracked run; so does a field-diff-shaped input it declined.
      if ((event.inputType === "insertText" && data.length > 0) || FIELD_DIFF_INPUT_TYPES.has(event.inputType)) {
        this.emittedCount = null;
      }
      return;
    }
    const textarea = this.textarea();
    if (!textarea || record.compositionGeneration !== this.compositionGeneration) {
      this.emittedCount = null;
      return;
    }
    if (record.kind === "field-diff") {
      if (event.inputType !== record.inputType) {
        this.emittedCount = null;
        return;
      }
      if (this.reconcileFieldDelta(event, textarea, record.previousValue ?? "")) this.fieldDiffRewrites += 1;
      return;
    }
    if (event.inputType !== "insertText" || data.length !== record.dataUtf16) {
      this.emittedCount = null;
      return;
    }
    if (record.kind === "key-echo") {
      // xterm already emitted this character at keydown/keypress. Reacquire
      // field tracking only after the matching native edit actually lands.
      // WebKit's trailing NBSP is the same space xterm sent as U+0020.
      const matches = data === record.keyText
        && normalizeSpaces(textarea.value) === normalizeSpaces((record.previousValue ?? "") + data)
        && textarea.selectionStart === textarea.value.length
        && textarea.selectionEnd === textarea.value.length
        && this.featureEligible()
        && !this.compositionSeenThisFocusSession;
      this.emittedCount = matches ? codePointLength(textarea.value) : null;
      if (matches) event.stopPropagation();
      return;
    }
    if (record.kind === "textarea-owner") {
      this.reconcileFieldDelta(event, textarea, record.previousValue ?? "");
      return;
    }
    if (record.kind === "tail-replace") {
      // The browser replaced the selected tail with `data`. Keep the part of
      // the tail the revision repeats (its shared grapheme prefix), erase the
      // rest, and send only what is new — the same minimization as a
      // retained revision, so an interim preview costs bytes proportional to
      // the change, not to the phrase.
      const replaced = record.replacedText ?? "";
      if (textarea.value.length !== record.priorUtf16 - replaced.length + record.dataUtf16 || !this.featureEligible()) {
        this.emittedCount = null;
        return;
      }
      if (LINE_DELIMITER.test(data)) {
        this.refuseLineDelimiter(event, textarea, textarea.value.slice(0, record.priorUtf16 - replaced.length) + replaced);
        return;
      }
      const delta = fieldDelta(replaced, data);
      event.stopPropagation();
      this.tailRevisionRewrites += 1;
      let erased = true;
      for (let sent = 0; sent < delta.eraseCodePoints && erased; sent += 1) erased = this.dispatchLogicalKey("backspace");
      const inserted = delta.suffix === "" || this.dispatchGuardedInsert(delta.suffix);
      this.emittedCount = erased && inserted ? codePointLength(textarea.value) : null;
      return;
    }
    const appliedLength = record.kind === "append" || record.kind === "retained-revision"
      ? record.priorUtf16 + record.dataUtf16
      : record.dataUtf16;
    if (textarea.value.length !== appliedLength) {
      // The browser did not apply the edit the metadata described.
      this.emittedCount = null;
      return;
    }
    if (record.kind === "establish") {
      this.emittedCount = record.dataCodePoints;
      return;
    }
    if (record.kind === "append") {
      if (this.emittedCount !== null) this.emittedCount += record.dataCodePoints;
      return;
    }
    const priorEmission = this.emittedCount;
    if (record.kind === "retained-revision") {
      // WebKit dictation may retain the previous helper value, put the caret
      // at its end, and report the *whole* revised phrase as new insertText
      // (xterm.js #6078). At this point the default edit has produced
      // prior+revision. Compare only against the live helper field, keep no
      // transcript in router state. Keep the shared prefix, erase only the
      // changed prior suffix, and route only the changed/new suffix.
      const prior = textarea.value.slice(0, record.priorUtf16);
      const revision = data;
      const sharedUtf16 = record.sharedPrefixUtf16 ?? 0;
      const sharedCodePoints = record.sharedPrefixCodePoints ?? 0;
      if (priorEmission === null || priorEmission <= 0 || sharedUtf16 <= 0
        || revision.slice(0, sharedUtf16) !== prior.slice(0, sharedUtf16)) {
        this.emittedCount = null;
        return;
      }
      if (LINE_DELIMITER.test(revision)) {
        this.refuseLineDelimiter(event, textarea, prior);
        return;
      }
      event.stopPropagation();
      const suffix = revision.slice(sharedUtf16);
      textarea.value = revision;
      textarea.setSelectionRange(revision.length, revision.length);
      this.retainedRevisionRewrites += 1;
      let erased = true;
      for (let sent = sharedCodePoints; sent < priorEmission && erased; sent += 1) erased = this.dispatchLogicalKey("backspace");
      const redispatched = suffix === "" || this.dispatchGuardedInsert(suffix);
      this.emittedCount = erased && redispatched ? record.dataCodePoints : null;
      return;
    }
    if (priorEmission === null || priorEmission <= 0 || !this.featureEligible()) {
      this.emittedCount = null;
      return;
    }
    if (LINE_DELIMITER.test(data)) {
      this.refuseLineDelimiter(event, textarea, record.previousValue ?? "");
      return;
    }
    // A full-field dictation revision: the browser's default replacement
    // stands in the field; xterm must never forward the raw event. Erase what
    // this path previously emitted, then route the new transcript through
    // xterm's normal insertText path.
    event.stopPropagation();
    this.replacementRewrites += 1;
    let erased = true;
    for (let sent = 0; sent < priorEmission && erased; sent += 1) {
      erased = this.dispatchLogicalKey("backspace");
    }
    const redispatched = this.dispatchGuardedInsert(data);
    this.emittedCount = erased && redispatched ? record.dataCodePoints : null;
  }

  onInput(event: InputEvent): void {
    if (this.syntheticInsertDepth > 0) return;
    if (!this.isTextareaTarget(event.target)) return;
    if (this.composing || event.isComposing) {
      this.pending = undefined;
      return;
    }

    const expected = this.expectedRunAfterDelete;
    this.expectedRunAfterDelete = undefined;
    if (DELETE_INPUT_TYPES.has(event.inputType) && this.pending) {
      const record = this.pending;
      record.appliedDeleteInputs += 1;
      let routedNow = false;
      if (!record.routed && this.featureEligible()) {
        record.routed = true;
        const logicalKey: LogicalKey = event.inputType === "deleteWordBackward"
          ? { ctrl: "w" }
          : "backspace";
        if (this.dispatchLogicalKey(logicalKey)) {
          record.ptyDeleteIntents += 1;
          routedNow = true;
        }
      }
      if (expected !== undefined) {
        // The run survives only when this input routed its one Backspace and
        // the field lost exactly the one code point that Backspace erases.
        const textarea = this.textarea();
        this.emittedCount = routedNow && textarea !== null && codePointLength(textarea.value) === expected
          ? expected
          : null;
      }
    } else {
      this.pending = undefined;
      if (expected !== undefined) this.emittedCount = null;
    }
    this.seedIfEligible();
  }

  onXtermKeyHandled(event: KeyboardEvent, keyText?: string): void {
    // The router's own logical dispatches (the erasure Backspaces of a
    // replacement rewrite) come back through xterm's key path; they are the
    // modeled rewrite itself, never an authority-moving operator key.
    if (this.logicalDispatchDepth > 0) return;
    if (!this.isTextareaTarget(event.target) || this.composing) return;
    if (event.keyCode === 229 || event.which === 229 || event.key === "Process") {
      // WebKit's dictation prelude does not move terminal authority. Keep the
      // boolean classifier armed for the immediately following insertText;
      // no dictated content is retained here.
      return;
    }
    const key = event.key;
    if (key === "Enter" || (!event.altKey && (event.ctrlKey || event.metaKey) && key.toLowerCase() === "c")) {
      this.onXtermOperationComplete();
      return;
    }
    const textarea = this.textarea();
    const value = textarea?.value === IOS_BACKSPACE_SENTINEL ? "" : textarea?.value;
    const mirroredCharacter = keyText !== undefined && keyText === key && codePointLength(keyText) === 1
      && !/[\u0000-\u001f\u007f-\u009f]/u.test(keyText)
      && !event.altKey && !event.ctrlKey && !event.metaKey && !event.isComposing
      && !this.compositionSeenThisFocusSession && this.featureEligible()
      && textarea !== null && value !== undefined
      && textarea.selectionStart === textarea.value.length && textarea.selectionEnd === textarea.value.length
      && (value === "" || this.emittedCount === codePointLength(value));
    this.invalidateEmissionTracking();
    if (mirroredCharacter) {
      // The September 5 iPhone trace has a physical Space between dictation
      // runs. Its onKey emission precedes the corresponding native field
      // insertion. Keep that one causal pair, not a guess about a dictation
      // session or a prefix of the next phrase.
      this.pendingKeyEcho = { text: keyText!, previousValue: value!, compositionGeneration: this.compositionGeneration };
    }
    // Every other xterm-handled key sent bytes to the PTY outside a modeled
    // beforeinput/input pair: navigation keys, Delete and Backspace the
    // router does not own, Escape, Tab, function keys, and modified chords
    // all move terminal cursor/editing state while the helper field stays
    // put, so the field's relation to what this path emitted is unknown.
    // The erase count is never guessed: the tracked run ends here and a
    // later full-field revision falls back to the raw path. Plain text
    // insertion can restore tracking only through the verified key-echo pair
    // above; keyCode-229 input remains owned by the existing textarea path.
  }

  /** Reconcile after a synchronous xterm operation that may clear the field. */
  onXtermOperationComplete(): void {
    // Enter, Ctrl+C, and paste clear the field through xterm's own writes; the
    // tracked run ends with the content it modeled.
    this.invalidateEmissionTracking();
    this.seedIfEligible();
  }

  handleCustomKey(event: KeyboardEvent): CustomKeyDisposition {
    if (this.logicalDispatchDepth > 0) return "logical-dispatch";
    if (!this.isTextareaTarget(event.target)) return "pass";
    // A real key event between an insertion's beforeinput and its applied
    // input breaks the pair's sequence bound; likewise for a modeled delete.
    this.pendingInsert = undefined;
    if (event.type !== "keyup") this.pendingKeyEcho = undefined;
    this.expectedRunAfterDelete = undefined;
    this.reconcilePolicy();
    this.repairSoleSentinelCaret();
    const textareaOwner = event.type === "keydown"
      && (event.keyCode === 229 || event.which === 229)
      && !this.composing
      && this.featureEligible()
      && (this.emittedCount !== null || this.textarea()?.value === IOS_BACKSPACE_SENTINEL || this.textarea()?.value === "");
    this.dictationPreludePending = textareaOwner;
    if (textareaOwner) {
      // 229 selects the single textarea producer but says nothing about the
      // insertion's meaning. The paired previous/current textarea delta below
      // decides the bytes and xterm's delayed producer is suppressed here.
      this.pending = undefined;
      return "intercept-dictation-prelude";
    }
    if (!isUnmodifiedBackspace(event) || !this.eventEligible()) {
      this.pending = undefined;
      return "pass";
    }

    this.keydownCount += 1;
    const record: MutableDeleteRecord = {
      keydownOrdinal: this.keydownCount,
      appliedDeleteInputs: 0,
      ptyDeleteIntents: 0,
      routed: false,
    };
    this.deleteRecords.push(record);
    if (this.deleteRecords.length > DELETE_RECORD_LIMIT) this.deleteRecords.shift();
    this.pending = record;
    return "intercept-backspace";
  }

  instrumentation(): IOSBackspaceInstrumentation {
    const textarea = this.textarea();
    return Object.freeze({
      active: this.featureEligible(),
      seeded: textarea?.value === IOS_BACKSPACE_SENTINEL,
      composing: this.composing,
      pendingKeydownOrdinal: this.pending?.keydownOrdinal ?? null,
      logicalDispatchDepth: this.logicalDispatchDepth,
      logicalDispatches: this.logicalDispatches,
      keydownCount: this.keydownCount,
      trackedEmissionCount: this.emittedCount,
      pendingInsertKind: this.pendingInsert?.kind ?? null,
      replacementRewrites: this.replacementRewrites,
      retainedRevisionRewrites: this.retainedRevisionRewrites,
      tailRevisionRewrites: this.tailRevisionRewrites,
      fieldDiffRewrites: this.fieldDiffRewrites,
      lineDelimiterRefusals: this.lineDelimiterRefusals,
      dictationPreludePending: this.dictationPreludePending,
      replacementDisabledForFocusSession: this.compositionSeenThisFocusSession,
      deleteRecords: Object.freeze(this.deleteRecords.map((record) => Object.freeze({
        keydownOrdinal: record.keydownOrdinal,
        appliedDeleteInputs: record.appliedDeleteInputs,
        ptyDeleteIntents: record.ptyDeleteIntents,
      }))),
      destroyed: this.destroyed,
    });
  }

  destroy(): void {
    if (this.destroyed) return;
    this.pending = undefined;
    this.dictationPreludePending = false;
    this.invalidateEmissionTracking();
    this.removeSoleSentinel();
    this.composing = false;
    this.destroyed = true;
  }

  private dispatchLogicalKey(key: LogicalKey): boolean {
    this.logicalDispatchDepth += 1;
    this.logicalDispatches += 1;
    try {
      return this.options.sendLogicalKey(key);
    } finally {
      this.logicalDispatchDepth -= 1;
    }
  }

  private dispatchGuardedInsert(data: string): boolean {
    this.syntheticInsertDepth += 1;
    try {
      return this.options.dispatchSyntheticInsert(data);
    } finally {
      this.syntheticInsertDepth -= 1;
    }
  }

  private invalidateEmissionTracking(): void {
    this.insertionSentinel = undefined;
    this.emittedCount = null;
    this.pendingInsert = undefined;
    this.pendingKeyEcho = undefined;
    this.dictationPreludePending = false;
    this.expectedRunAfterDelete = undefined;
  }

  private classifyKeyEcho(event: InputEvent, echo: PendingKeyEcho): PendingInsertRecord | undefined {
    const textarea = this.textarea();
    if (!textarea || event.inputType !== "insertText" || event.data !== echo.text
      || echo.compositionGeneration !== this.compositionGeneration
      || !this.featureEligible() || this.compositionSeenThisFocusSession) return undefined;
    const value = textarea.value === IOS_BACKSPACE_SENTINEL ? "" : textarea.value;
    if (normalizeSpaces(value) !== normalizeSpaces(echo.previousValue)
      || textarea.selectionStart !== textarea.value.length || textarea.selectionEnd !== textarea.value.length) return undefined;
    return {
      kind: "key-echo",
      dataUtf16: echo.text.length,
      dataCodePoints: codePointLength(echo.text),
      priorUtf16: value.length,
      previousValue: value,
      keyText: echo.text,
      compositionGeneration: this.compositionGeneration,
    };
  }

  /**
   * Make the PTY match the field after an input that replaced field content
   * without a forwardable insertText: erase the changed suffix of what this
   * path previously sent, then route the new suffix through xterm's normal
   * insertText path. The field is the only truth here; no transcript is kept.
   */
  private reconcileFieldDelta(event: InputEvent, textarea: HTMLTextAreaElement, previousValue: string): boolean {
    const currentValue = textarea.value === IOS_BACKSPACE_SENTINEL ? "" : textarea.value;
    if (LINE_DELIMITER.test(currentValue)) {
      this.refuseLineDelimiter(event, textarea, previousValue);
      return false;
    }
    const delta = fieldDelta(previousValue, currentValue);
    event.stopPropagation();
    let erased = true;
    for (let index = 0; index < delta.eraseCodePoints && erased; index += 1) erased = this.dispatchLogicalKey("backspace");
    const inserted = delta.suffix === "" || this.dispatchGuardedInsert(delta.suffix);
    this.emittedCount = erased && inserted ? codePointLength(currentValue) : null;
    return true;
  }

  /**
   * UX16-R1: the browser put a line delimiter into the field. Nothing is
   * sent (the raw path is stopped too), the field is restored to exactly its
   * previous text with the caret at its end (the sole sentinel when that
   * text is empty), and the tracked run — which still describes that text —
   * is left untouched.
   */
  private refuseLineDelimiter(event: InputEvent, textarea: HTMLTextAreaElement, previousValue: string): void {
    event.stopPropagation();
    this.lineDelimiterRefusals += 1;
    const restored = previousValue === IOS_BACKSPACE_SENTINEL ? "" : previousValue;
    textarea.value = restored;
    textarea.setSelectionRange(restored.length, restored.length);
    this.seedIfEligible();
  }

  /**
   * UX-16 §16.7 field-diff: a beforeinput whose applied input xterm will not
   * forward. Modeled only while the field is empty/sentinel or the tracked run
   * equals the field's code-point length, so the previous/current diff is
   * exactly the PTY's change; otherwise the run ends, fail-closed.
   */
  private classifyFieldDiff(event: InputEvent): PendingInsertRecord | undefined {
    this.dictationPreludePending = false;
    if (this.commitExclusionPending) {
      this.commitExclusionPending = false;
      this.emittedCount = null;
      return undefined;
    }
    const textarea = this.textarea();
    if (!textarea) {
      this.emittedCount = null;
      return undefined;
    }
    const value = textarea.value === IOS_BACKSPACE_SENTINEL ? "" : textarea.value;
    const tracked = value === "" ? 0 : this.emittedCount;
    if (tracked === null
      || tracked !== codePointLength(value)
      || this.compositionSeenThisFocusSession
      || !this.featureEligible()) {
      this.emittedCount = null;
      return undefined;
    }
    return {
      kind: "field-diff",
      inputType: event.inputType,
      dataUtf16: 0,
      dataCodePoints: 0,
      priorUtf16: value.length,
      previousValue: value,
      compositionGeneration: this.compositionGeneration,
    };
  }

  /**
   * UX-16 §16.7: the run length after a Backspace this router intercepted,
   * when the delete is the one-code-point shape at the end of a fully tracked
   * field; `undefined` for every other delete.
   */
  private classifyModeledDelete(event: InputEvent): number | undefined {
    if (event.inputType !== "deleteContentBackward" || !this.pending || !this.featureEligible()) return undefined;
    const textarea = this.textarea();
    if (!textarea || this.emittedCount === null || this.compositionSeenThisFocusSession) return undefined;
    const value = textarea.value;
    if (value === "" || value === IOS_BACKSPACE_SENTINEL) return undefined;
    if (textarea.selectionStart !== value.length || textarea.selectionEnd !== value.length) return undefined;
    const length = codePointLength(value);
    if (this.emittedCount !== length) return undefined;
    return length - 1;
  }

  /**
   * Classify one non-composing insertion beforeinput against the field's
   * pre-event state. Returns a metadata-only pending record for the four
   * modeled shapes; every other shape invalidates the tracked run.
   *
   * The replacement discriminator is the frozen contract's P2 predicate:
   * insertText, not composing, selection spanning 0..priorLength with
   * priorLength > 0 — evaluated in DOM offsets purely to prove full-field
   * replacement, never to derive the erase count.
   */
  private classifyInsertion(event: InputEvent): PendingInsertRecord | undefined {
    const textareaOwner = this.dictationPreludePending;
    this.dictationPreludePending = false;
    if (this.commitExclusionPending) {
      // The first field input after compositionend may be the IME commit; it
      // is indistinguishable from dictation by shape alone, so it is excluded.
      this.commitExclusionPending = false;
      this.emittedCount = null;
      return undefined;
    }
    const textarea = this.textarea();
    if (!textarea) {
      this.emittedCount = null;
      return undefined;
    }
    if (event.inputType !== "insertText") {
      this.emittedCount = null;
      return undefined;
    }
    const data = typeof event.data === "string" ? event.data : "";
    const value = textarea.value;
    const selectionStart = textarea.selectionStart;
    const selectionEnd = textarea.selectionEnd;
    if (data.length === 0) {
      // Nothing inserted: harmless over a collapsed caret, an unmodeled
      // deletion over a span.
      if (selectionStart !== selectionEnd) this.emittedCount = null;
      return undefined;
    }
    const record = (kind: PendingInsertKind, priorUtf16: number): PendingInsertRecord => ({
      kind,
      dataUtf16: data.length,
      dataCodePoints: codePointLength(data),
      priorUtf16,
      compositionGeneration: this.compositionGeneration,
    });
    if (textareaOwner) {
      return {
        ...record("textarea-owner", value === IOS_BACKSPACE_SENTINEL ? 0 : value.length),
        previousValue: value === IOS_BACKSPACE_SENTINEL ? "" : value,
      };
    }
    if (value === "" || value === IOS_BACKSPACE_SENTINEL) {
      // The sole sentinel is removed before the browser applies this edit, so
      // the data lands alone in the field whatever the observed selection
      // shape; the prior emission represented in the field is zero, which is
      // also why a span over the sentinel can never satisfy the replacement
      // predicate.
      return record("establish", 0);
    }
    if (selectionStart === value.length && selectionEnd === value.length) {
      if (this.emittedCount === null) return undefined;
      // A normal key inserts only its own data. After WebKit's keyCode-229
      // dictation prelude, a revision may repeat or revise the live field and
      // then add a semantic suffix. This positional test is deliberately
      // local and ephemeral:
      // no dictated bytes enter instrumentation, logs, or retained state.
      let sharedPrefixUtf16 = 0;
      while (sharedPrefixUtf16 < value.length && sharedPrefixUtf16 < data.length
        && value.charCodeAt(sharedPrefixUtf16) === data.charCodeAt(sharedPrefixUtf16)) sharedPrefixUtf16 += 1;
      if (sharedPrefixUtf16 > 0
        && sharedPrefixUtf16 < value.length
        && /[\uD800-\uDBFF]/u.test(value.charAt(sharedPrefixUtf16 - 1))) sharedPrefixUtf16 -= 1;
      const accumulatedRevision = false;
      if (accumulatedRevision) {
        return {
          ...record("retained-revision", value.length),
          sharedPrefixUtf16,
          sharedPrefixCodePoints: codePointLength(value.slice(0, sharedPrefixUtf16)),
        };
      }
      return record("append", value.length);
    }
    if (selectionStart === 0 && selectionEnd === value.length
      && this.emittedCount !== null && this.emittedCount > 0) {
      if (this.compositionSeenThisFocusSession) {
        // The F4 safe-disable: a focus session that saw composition may
        // deliver post-composition insertions whose relation to the field
        // cannot be modeled without the missing real pinyin trace
        // (IMPLEMENTATION_BLOCKED_ON_REAL_CJK_TRACE). The revision goes raw
        // and the run ends; blur/refocus restores eligibility.
        this.emittedCount = null;
        return undefined;
      }
      // The previous text is kept only so a refused revision (R1) can put
      // the field back; it pairs with this one input and is never instrumented.
      return { ...record("replace", value.length), previousValue: value };
    }
    if (selectionEnd === value.length
      && selectionStart > 0
      && selectionStart < selectionEnd
      && this.emittedCount !== null
      && this.emittedCount === codePointLength(value)
      && !this.compositionSeenThisFocusSession) {
      // UX-16 §16.7 tail-replace: WebKit dictation previews each interim
      // result as insertText over the span it previously inserted, anchored
      // at the field's end. With the run equal to the field, the span's
      // code points are exactly the PTY's tail.
      return { ...record("tail-replace", value.length), replacedText: value.slice(selectionStart) };
    }
    // A mid-field caret, a partial span not anchored at the end, or a span
    // over untracked content: the effect on emitted text cannot be
    // established.
    this.emittedCount = null;
    return undefined;
  }

  private eventEligible(): boolean {
    const textarea = this.textarea();
    return this.featureEligible()
      && !this.composing
      && textarea !== null
      && document.activeElement === textarea
      && textarea.value.length > 0;
  }

  private featureEligible(): boolean {
    return !this.destroyed && this.options.isFeatureEligible();
  }

  private reconcilePolicy(): void {
    if (!this.featureEligible()) {
      this.pending = undefined;
      this.removeSoleSentinel();
    }
  }

  private seedIfEligible(): void {
    const textarea = this.textarea();
    if (!textarea || !this.featureEligible() || this.composing || document.activeElement !== textarea) return;
    if (textarea.value === "") textarea.value = IOS_BACKSPACE_SENTINEL;
    this.repairSoleSentinelCaret();
  }

  private repairSoleSentinelCaret(): void {
    const textarea = this.textarea();
    if (!textarea || textarea.value !== IOS_BACKSPACE_SENTINEL) return;
    if (textarea.selectionStart !== IOS_BACKSPACE_SENTINEL.length
      || textarea.selectionEnd !== IOS_BACKSPACE_SENTINEL.length) {
      textarea.setSelectionRange(IOS_BACKSPACE_SENTINEL.length, IOS_BACKSPACE_SENTINEL.length);
    }
  }

  private prepareInsertionSentinel(): void {
    const textarea = this.textarea();
    if (!textarea || textarea.value !== IOS_BACKSPACE_SENTINEL) return;
    // WebKit can abandon the native insertion if beforeinput rewrites value.
    // Let its edit land, then remove only the sentinel that edit retained.
    if (textarea.selectionStart === 1) this.insertionSentinel = "start";
    else if (textarea.selectionEnd === 0) this.insertionSentinel = "end";
  }

  private removeAppliedInsertionSentinel(): void {
    const edge = this.insertionSentinel;
    this.insertionSentinel = undefined;
    const textarea = this.textarea();
    if (!edge || !textarea) return;
    const index = edge === "start" ? 0 : textarea.value.length - 1;
    if (textarea.value[index] !== IOS_BACKSPACE_SENTINEL) return;
    const start = textarea.selectionStart, end = textarea.selectionEnd, direction = textarea.selectionDirection;
    textarea.value = textarea.value.slice(0, index) + textarea.value.slice(index + 1);
    textarea.setSelectionRange(start > index ? start - 1 : start, end > index ? end - 1 : end, direction);
  }

  private removeSoleSentinel(): void {
    const textarea = this.textarea();
    if (!textarea || textarea.value !== IOS_BACKSPACE_SENTINEL) return;
    textarea.value = "";
    textarea.setSelectionRange(0, 0);
  }

  private textarea(): HTMLTextAreaElement | null {
    return this.options.liveHost.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
  }

  private isTextareaTarget(target: EventTarget | null): target is HTMLTextAreaElement {
    const textarea = this.textarea();
    return textarea !== null && target === textarea;
  }
}
