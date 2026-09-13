// Tap activation for the quick-actions sheet. Kept apart from
// keyboard_preserving_button.ts on purpose: that helper is the single
// keyboard-preserving activation the composer and key bar share, and its
// listener set is pinned by the composer structure test.

type KeyboardActivationFence = Readonly<{
  acceptClick(event: MouseEvent): boolean;
  revoke(): void;
  clear(): void;
  dispose(): void;
}>;

type KeyboardActivationKey = "Enter" | "Space";
type KeyboardActivationState =
  | Readonly<{ kind: "idle" }>
  | Readonly<{ kind: "armed"; key: KeyboardActivationKey; generation: number; released: boolean; pendingClick: boolean }>
  | Readonly<{ kind: "canceled"; key: KeyboardActivationKey; generation: number; released: boolean; pendingClick: boolean }>;

/**
 * A native button click produced by Enter/Space is delivered after the
 * initiating keydown (on keydown for Enter, after keyup for Space). Keep that
 * keyboard authority tied to the disclosure generation in which it began.
 * Idle is deliberately distinct from canceled: a trusted accessibility click
 * has no preceding keydown and stays valid, while the compatibility click for
 * a keyboard press revoked by an overlapping pointer/disclosure transition is
 * consumed. State retires only at an event boundary, never on a timer.
 */
export function bindKeyboardActivationFence(
  button: HTMLButtonElement,
  interactionGeneration: () => number,
): KeyboardActivationFence {
  const idle = Object.freeze({ kind: "idle" } as const);
  let state: KeyboardActivationState = idle;
  const activationKey = (event: KeyboardEvent): KeyboardActivationKey | undefined => {
    if (event.key === "Enter") return "Enter";
    if (event.key === " " || event.key === "Spacebar") return "Space";
    return undefined;
  };
  const clear = (): void => {
    state = idle;
  };
  const retireCanceledSpaceAtRelease = (event: KeyboardEvent): void => {
    // A button's native Space click is the default action of this exact keyup.
    // Once the capture is canceled or stale, cancel that default at its source
    // and retire synchronously. Engines that would emit the compatibility
    // click cannot do so; engines that emit none retain no tombstone capable
    // of poisoning a later, independent accessibility click.
    if (event.cancelable) event.preventDefault();
    clear();
  };
  const revoke = (): void => {
    if (state.kind !== "armed") return;
    state = Object.freeze({ ...state, kind: "canceled" });
  };
  const generationIsCurrent = (capture: Exclude<KeyboardActivationState, Readonly<{ kind: "idle" }>>): boolean =>
    capture.generation === interactionGeneration();
  const onKeyDown = (event: KeyboardEvent): void => {
    if (!event.isTrusted) return;
    const key = activationKey(event);
    if (!key) {
      // An unrelated physical key neither ends nor transfers the authority of
      // a held Enter/Space press. Its matching release must still see the
      // original control and generation.
      return;
    }
    // Repeats belong to the initial physical press. Enter emits a click for
    // every repeat keydown, so retain the original generation after each
    // consumed click and arm only a fresh click token here.
    if (event.repeat) {
      if (state.kind === "idle") {
        state = Object.freeze({ kind: "canceled", key, generation: interactionGeneration(), released: false, pendingClick: true });
      } else if (state.key === key && !state.released) {
        state = Object.freeze({ ...state, pendingClick: true });
      }
      return;
    }
    clear();
    state = Object.freeze({ kind: "armed", key, generation: interactionGeneration(), released: false, pendingClick: true });
  };
  const onKeyUp = (event: KeyboardEvent): void => {
    if (!event.isTrusted || state.kind === "idle") return;
    if (activationKey(event) !== state.key) {
      // Unrelated releases cannot erase a still-physical activation key.
      return;
    }
    if (state.key === "Space"
      && (state.kind === "canceled" || !generationIsCurrent(state))) {
      retireCanceledSpaceAtRelease(event);
      return;
    }
    // Enter clicks on keydown. If its last click token is already consumed,
    // keyup is the deterministic end of the physical press. Space's click is
    // delivered after keyup, so its released state must remain until then.
    if (state.key === "Enter" && !state.pendingClick) {
      clear();
      return;
    }
    state = Object.freeze({ ...state, released: true });
  };
  const onBlur = (): void => clear();
  button.addEventListener("keydown", onKeyDown);
  button.addEventListener("keyup", onKeyUp);
  button.addEventListener("blur", onBlur);
  return Object.freeze({
    acceptClick(event: MouseEvent): boolean {
      if (!event.isTrusted) return false;
      const capture = state;
      // Only genuinely idle detail-zero clicks are captureless accessibility
      // activations. A revoked capture remains a one-click tombstone.
      if (capture.kind === "idle") return true;
      const allowed = capture.kind === "armed" && capture.pendingClick
        && generationIsCurrent(capture);
      if (capture.released) clear();
      else state = Object.freeze({ ...capture, kind: allowed ? capture.kind : "canceled", pendingClick: false });
      return allowed;
    },
    revoke,
    clear,
    dispose(): void {
      clear();
      button.removeEventListener("keydown", onKeyDown);
      button.removeEventListener("keyup", onKeyUp);
      button.removeEventListener("blur", onBlur);
    },
  });
}

/**
 * Native-focus button activation for controls that do not live on a
 * scrollable keyboard-preserving surface. Keyboard, AT, and pointer paths
 * stay distinct: Enter/Space use the shared keyboard fence, a captureless
 * trusted detail-zero click is AT, and pointer clicks retain the pointer's
 * acquisition generation until their native click.
 */
export function bindGenerationFencedClickActivation(
  button: HTMLButtonElement,
  activate: (event: MouseEvent) => void,
  isLive: () => boolean,
  interactionGeneration: () => number,
): () => void {
  const keyboardFence = bindKeyboardActivationFence(button, interactionGeneration);
  const pointerGenerations = new Map<number, Readonly<{ generation: number; released: boolean }>>();
  const onPointerDown = (event: PointerEvent): void => {
    if (!event.isTrusted || !isLive()) return;
    keyboardFence.revoke();
    pointerGenerations.set(event.pointerId, { generation: interactionGeneration(), released: false });
  };
  const cancelPointer = (event: PointerEvent): void => { pointerGenerations.delete(event.pointerId); };
  const onPointerUp = (event: PointerEvent): void => {
    const capture = pointerGenerations.get(event.pointerId);
    if (!event.isTrusted || !capture) return;
    pointerGenerations.set(event.pointerId, { ...capture, released: true });
  };
  const onPointerLeave = (event: PointerEvent): void => {
    // Touch ends with pointerup → pointerleave → click. Only leaving while
    // held cancels acquisition; a completed release still owns its click.
    if (!pointerGenerations.get(event.pointerId)?.released) cancelPointer(event);
  };
  const onClick = (event: MouseEvent): void => {
    if (!isLive()) return;
    // A direct programmatic click has no earlier user gesture whose authority
    // can go stale. Preserve the established HTMLElement.click() contract for
    // automation and application callers, while reserving the provenance
    // state machine below for trusted browser input.
    if (!event.isTrusted) {
      activate(event);
      return;
    }
    if (event.detail === 0) {
      if (!keyboardFence.acceptClick(event)) return;
      activate(event);
      return;
    }
    const pointerId = "pointerId" in event && typeof event.pointerId === "number" ? event.pointerId : undefined;
    let capture = pointerId === undefined ? undefined : pointerGenerations.get(pointerId);
    // Older click events can omit the pointer ID. An explicit ID must never
    // borrow another contact's authority, including after cancellation.
    if (pointerId === undefined && pointerGenerations.size === 1) {
      capture = pointerGenerations.values().next().value;
      pointerGenerations.clear();
    } else if (pointerId !== undefined) {
      pointerGenerations.delete(pointerId);
    }
    if (!capture || capture.generation !== interactionGeneration()) return;
    activate(event);
  };
  button.addEventListener("pointerdown", onPointerDown);
  button.addEventListener("pointerup", onPointerUp);
  button.addEventListener("pointercancel", cancelPointer);
  button.addEventListener("pointerleave", onPointerLeave);
  button.addEventListener("click", onClick);
  return () => {
    pointerGenerations.clear();
    keyboardFence.dispose();
    button.removeEventListener("pointerdown", onPointerDown);
    button.removeEventListener("pointerup", onPointerUp);
    button.removeEventListener("pointercancel", cancelPointer);
    button.removeEventListener("pointerleave", onPointerLeave);
    button.removeEventListener("click", onClick);
  };
}

export type TapRepeat = Readonly<{
  // Begin consumes a one-shot modifier once and returns the same chord for
  // subsequent repeats. Every tick still checks current input authority.
  begin(event: Event): () => void;
  isLive(): boolean;
}>;

/**
 * Tap activation for a control on a SCROLLABLE surface (the quick-actions
 * sheet). Same keyboard-preserving posture as bindKeyboardPreservingActivation
 * (keyboard_preserving_button.ts) — the pointerdown is
 * cancelled, so focus never moves and an open keyboard stays open — but the
 * action fires on pointerup rather than pointerdown, and only when the
 * pointer comes up on the same control without the browser having claimed
 * the gesture for a scroll (a scroll turns the pointer sequence into a
 * pointercancel). Cancelling pointerdown does not cancel scrolling; a
 * cancelled touchstart would, which is why touchstart is left alone here.
 * The returned disposer retires both pointer and keyboard authority.
 */
export function bindTapActivation(
  button: HTMLButtonElement,
  activate: (event: Event) => void,
  restoreFocus: () => void,
  isLive: () => boolean,
  interactionGeneration: () => number,
  repeat?: TapRepeat,
): () => void {
  type Capture = { generation: number; x: number; y: number; focus: Element | null; timer?: number; repeated?: boolean; again?: () => void };
  const armedPointers = new Map<number, Capture>();
  const keyboardFence = bindKeyboardActivationFence(button, interactionGeneration);
  const retirePointer = (pointerId: number): void => {
    const capture = armedPointers.get(pointerId);
    if (!capture) return;
    if (capture.timer !== undefined) window.clearTimeout(capture.timer);
    armedPointers.delete(pointerId); keyboardFence.revoke();
  };
  const cancelAll = (): void => { for (const id of armedPointers.keys()) retirePointer(id); };
  const repeatLive = (capture: Capture): boolean => {
    const rect = button.getBoundingClientRect();
    return !!repeat && isLive() && repeat.isLive() && !document.hidden && button.isConnected && !button.disabled
      && capture.generation === interactionGeneration() && document.activeElement === capture.focus
      && rect.width > 0 && rect.height > 0 && capture.x >= rect.left && capture.x <= rect.right && capture.y >= rect.top && capture.y <= rect.bottom;
  };
  const scheduleRepeat = (pointerId: number, capture: Capture, event: PointerEvent, delay: number): void => {
    capture.timer = window.setTimeout(() => {
      capture.timer = undefined;
      if (armedPointers.get(pointerId) !== capture || !repeatLive(capture)) { retirePointer(pointerId); return; }
      if (!capture.repeated) {
        capture.repeated = true;
        capture.again = repeat!.begin(event);
        // Consuming the latch is this gesture's own state transition. Later
        // changes to the generation retire the hold, including reconnects.
        capture.generation = interactionGeneration();
      } else capture.again?.();
      if (armedPointers.get(pointerId) !== capture || !repeatLive(capture)) { retirePointer(pointerId); return; }
      // One timeout per delivery; background throttling never creates a burst.
      scheduleRepeat(pointerId, capture, event, 60);
    }, delay);
  };
  const onMouseDown = (event: MouseEvent): void => {
    if (!event.isTrusted || !isLive()) return;
    event.preventDefault();
  };
  const onPointerDown = (event: PointerEvent): void => {
    if (!event.isTrusted || !isLive()) return;
    if (repeat && (!event.isPrimary || event.button !== 0 || !repeat.isLive())) return;
    event.preventDefault();
    keyboardFence.revoke();
    const capture: Capture = { generation: interactionGeneration(), x: event.clientX, y: event.clientY, focus: document.activeElement };
    armedPointers.set(event.pointerId, capture);
    if (repeat) scheduleRepeat(event.pointerId, capture, event, 400);
  };
  const cancelPointer = (event: PointerEvent): void => {
    retirePointer(event.pointerId);
  };
  // Implicit touch capture can keep the pointer on the button throughout a
  // drag, even when the engine does not claim the gesture as native scrolling.
  // Use the same movement tolerance as the long-press recognizer below.
  const onPointerMove = (event: PointerEvent): void => {
    const capture = armedPointers.get(event.pointerId);
    if (capture && Math.hypot(event.clientX - capture.x, event.clientY - capture.y) > 8) cancelPointer(event);
  };
  const onPointerUp = (event: PointerEvent): void => {
    onPointerMove(event);
    const capture = armedPointers.get(event.pointerId);
    if (capture === undefined) return;
    const allowed = !repeat || repeatLive(capture);
    retirePointer(event.pointerId);
    if (!event.isTrusted || !isLive() || capture.generation !== interactionGeneration()) return;
    event.preventDefault();
    if (capture.repeated || !allowed) return;
    activate(event);
    restoreFocus();
  };
  // A touch tap's synthesized click hit-tests the DOM as it is AFTER the
  // pointerup, so a control whose action puts something new under the finger
  // would see that click land on whatever now sits there (measured: the sheet
  // opening under the old floating puck delivered the click to a tile).
  // Cancelling touchend suppresses the compatibility mouse events and that
  // click; the activation already happened on pointerup. touchstart is left
  // alone so the sheet still scrolls.
  const onTouchEnd = (event: TouchEvent): void => {
    if (!event.isTrusted || !isLive() || !event.cancelable) return;
    event.preventDefault();
  };
  const onClick = (event: MouseEvent): void => {
    // A pointer click does not own (and must not consume) keyboard provenance.
    if (!event.isTrusted || event.detail !== 0) return;
    const keyboardAllowed = keyboardFence.acceptClick(event);
    if (!isLive() || !keyboardAllowed) return;
    activate(event);
  };
  const onOtherPointer = (event: PointerEvent): void => { if (event.isTrusted) cancelAll(); };
  const onFocus = (): void => { for (const [id, capture] of armedPointers) if (document.activeElement !== capture.focus) retirePointer(id); };
  const onVisibility = (): void => { if (document.hidden) cancelAll(); };
  const onContextMenu = (event: Event): void => { if (armedPointers.size) { event.preventDefault(); } };
  button.addEventListener("mousedown", onMouseDown);
  button.addEventListener("pointerdown", onPointerDown);
  button.addEventListener("pointermove", onPointerMove);
  button.addEventListener("pointercancel", cancelPointer);
  button.addEventListener("lostpointercapture", cancelPointer);
  button.addEventListener("pointerleave", cancelPointer);
  button.addEventListener("pointerup", onPointerUp);
  button.addEventListener("touchend", onTouchEnd, { passive: false });
  button.addEventListener("click", onClick);
  if (repeat) {
    document.addEventListener("pointerdown", onOtherPointer, true);
    document.addEventListener("pointerup", cancelPointer);
    document.addEventListener("pointercancel", cancelPointer);
    document.addEventListener("focusin", onFocus);
    document.addEventListener("visibilitychange", onVisibility);
    window.addEventListener("blur", cancelAll);
    button.addEventListener("contextmenu", onContextMenu);
  }
  return () => {
    cancelAll();
    keyboardFence.dispose();
    button.removeEventListener("mousedown", onMouseDown);
    button.removeEventListener("pointerdown", onPointerDown);
    button.removeEventListener("pointermove", onPointerMove);
    button.removeEventListener("pointercancel", cancelPointer);
    button.removeEventListener("lostpointercapture", cancelPointer);
    button.removeEventListener("pointerleave", cancelPointer);
    button.removeEventListener("pointerup", onPointerUp);
    button.removeEventListener("touchend", onTouchEnd);
    button.removeEventListener("click", onClick);
    if (repeat) {
      document.removeEventListener("pointerdown", onOtherPointer, true);
      document.removeEventListener("pointerup", cancelPointer);
      document.removeEventListener("pointercancel", cancelPointer);
      document.removeEventListener("focusin", onFocus);
      document.removeEventListener("visibilitychange", onVisibility);
      window.removeEventListener("blur", cancelAll);
      button.removeEventListener("contextmenu", onContextMenu);
    }
  };
}

export type LongPressClock = Readonly<{
  setTimeout(callback: () => void, delay: number): unknown;
  clearTimeout(handle: unknown): void;
}>;

export type LongPressRelease = "primary" | "consumed" | "none";

/**
 * One tap/long-press recognizer. The timer and the primary activation share
 * one state, so a consumed long-press can never fall through into the tap it
 * explains. It deliberately does not capture the pointer: native scrolling
 * remains free to cancel the sequence.
 */
export class LongPressGesture {
  private armedPointer: number | undefined;
  private startX = 0;
  private startY = 0;
  private timer: unknown;
  private consumed = false;

  constructor(
    private readonly explain: () => void,
    private readonly clock: LongPressClock,
    private readonly delay = 500,
    private readonly tolerance = 8,
  ) {}

  pointerDown(pointer: number, x: number, y: number): void {
    this.cancel();
    this.armedPointer = pointer;
    this.startX = x;
    this.startY = y;
    this.consumed = false;
    this.timer = this.clock.setTimeout(() => {
      this.timer = undefined;
      if (this.armedPointer === undefined) return;
      this.consumed = true;
      this.explain();
    }, this.delay);
  }

  pointerMove(pointer: number, x: number, y: number): void {
    if (pointer !== this.armedPointer) return;
    if (Math.hypot(x - this.startX, y - this.startY) <= this.tolerance) return;
    this.cancel();
  }

  pointerUp(pointer: number): LongPressRelease {
    if (pointer !== this.armedPointer) return "none";
    const consumed = this.consumed;
    this.clearTimer();
    this.armedPointer = undefined;
    this.consumed = false;
    return consumed ? "consumed" : "primary";
  }

  isConsumed(): boolean { return this.consumed; }

  cancel(): void {
    this.clearTimer();
    this.armedPointer = undefined;
    this.consumed = false;
  }

  private clearTimer(): void {
    if (this.timer === undefined) return;
    this.clock.clearTimeout(this.timer);
    this.timer = undefined;
  }
}

/**
 * A sheet-safe tap activation with a composed long-press explanation. Returns
 * a disposer because visibility and gesture listeners must not outlive a pane.
 */
export function bindExplainedTapActivation(
  button: HTMLButtonElement,
  activate: (event: Event) => void | boolean,
  explain: () => void,
  restoreFocus: () => void,
  isLive: () => boolean,
  interactionGeneration: () => number,
): () => void {
  const clock: LongPressClock = {
    setTimeout: (callback, delay) => window.setTimeout(callback, delay),
    clearTimeout: (handle) => window.clearTimeout(handle as number),
  };
  let suppressCompatibility = false;
  let suppressTimer: number | undefined;
  let armedPointer: Readonly<{ pointerId: number; generation: number }> | undefined;
  const keyboardFence = bindKeyboardActivationFence(button, interactionGeneration);
  const suppress = (): void => {
    suppressCompatibility = true;
    if (suppressTimer !== undefined) window.clearTimeout(suppressTimer);
    suppressTimer = window.setTimeout(() => { suppressCompatibility = false; suppressTimer = undefined; }, 800);
  };
  const gesture = new LongPressGesture(() => {
    if (!isLive() || armedPointer?.generation !== interactionGeneration()) return;
    suppress();
    explain();
  }, clock);
  const onMouseDown = (event: MouseEvent): void => {
    if (!event.isTrusted || !isLive()) return;
    event.preventDefault();
  };
  const onPointerDown = (event: PointerEvent): void => {
    if (!event.isTrusted || !isLive()) return;
    event.preventDefault();
    keyboardFence.revoke();
    armedPointer = Object.freeze({ pointerId: event.pointerId, generation: interactionGeneration() });
    gesture.pointerDown(event.pointerId, event.clientX, event.clientY);
  };
  const onPointerMove = (event: PointerEvent): void => gesture.pointerMove(event.pointerId, event.clientX, event.clientY);
  const onCancel = (event: PointerEvent): void => {
    if (armedPointer?.pointerId !== event.pointerId) return;
    armedPointer = undefined;
    gesture.cancel();
    keyboardFence.revoke();
  };
  const onPointerUp = (event: PointerEvent): void => {
    if (armedPointer?.pointerId !== event.pointerId) return;
    const generation = armedPointer.generation;
    armedPointer = undefined;
    const result = gesture.pointerUp(event.pointerId);
    if (result === "none" || !event.isTrusted || !isLive() || generation !== interactionGeneration()) return;
    event.preventDefault();
    if (result === "consumed") { suppress(); return; }
    if (activate(event) !== false) restoreFocus();
  };
  const onTouchEnd = (event: TouchEvent): void => {
    if (!event.isTrusted || !isLive() || !event.cancelable) return;
    event.preventDefault();
  };
  const onClick = (event: MouseEvent): void => {
    if (!event.isTrusted || !isLive()) return;
    if (suppressCompatibility && event.detail !== 0) {
      event.preventDefault();
      event.stopImmediatePropagation();
      suppressCompatibility = false;
      return;
    }
    if (event.detail !== 0) return;
    const keyboardAllowed = keyboardFence.acceptClick(event);
    if (!keyboardAllowed) return;
    activate(event);
  };
  const onContextMenu = (event: MouseEvent): void => {
    if (!gesture.isConsumed() && !suppressCompatibility) return;
    event.preventDefault();
  };
  const onVisibility = (): void => {
    if (!document.hidden) return;
    armedPointer = undefined;
    gesture.cancel();
    keyboardFence.clear();
  };
  button.addEventListener("mousedown", onMouseDown);
  button.addEventListener("pointerdown", onPointerDown);
  button.addEventListener("pointermove", onPointerMove);
  button.addEventListener("pointercancel", onCancel);
  button.addEventListener("pointerleave", onCancel);
  button.addEventListener("lostpointercapture", onCancel);
  button.addEventListener("pointerup", onPointerUp);
  button.addEventListener("touchend", onTouchEnd, { passive: false });
  button.addEventListener("click", onClick);
  button.addEventListener("contextmenu", onContextMenu);
  document.addEventListener("visibilitychange", onVisibility);
  button.classList.add("persea-explainer-enabled");
  return () => {
    armedPointer = undefined;
    gesture.cancel();
    keyboardFence.dispose();
    if (suppressTimer !== undefined) window.clearTimeout(suppressTimer);
    button.removeEventListener("mousedown", onMouseDown);
    button.removeEventListener("pointerdown", onPointerDown);
    button.removeEventListener("pointermove", onPointerMove);
    button.removeEventListener("pointercancel", onCancel);
    button.removeEventListener("pointerleave", onCancel);
    button.removeEventListener("lostpointercapture", onCancel);
    button.removeEventListener("pointerup", onPointerUp);
    button.removeEventListener("touchend", onTouchEnd);
    button.removeEventListener("click", onClick);
    button.removeEventListener("contextmenu", onContextMenu);
    document.removeEventListener("visibilitychange", onVisibility);
  };
}
