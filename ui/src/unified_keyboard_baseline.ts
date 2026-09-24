// The unified page's software-keyboard presence judgement.
//
// Whether the software keyboard is open is measured against the page's own
// history, never against window.innerHeight: iOS Safari moves innerHeight
// when the browser chrome collapses and expands, so a predicate shaped
// "vv.height < innerHeight" can stay true forever after the keyboard has
// gone — the stuck half-screen pin this machine replaces. The reference is
// a high-water mark of the scale-normalized visual viewport height
// (height x scale is invariant under pinch zoom) observed at rest. The
// keyboard reads open when the live reading sits far enough below that mark
// to be a keyboard rather than browser chrome: chrome show/hide moves an
// iPhone viewport by roughly 90px, software keyboards by 260px and more, so
// the threshold is the larger of 120px and a quarter of the resting height.
// The mark rises whenever a larger viewport is seen while no reseed is
// pending, and reseeds on a layout width change (rotation, split view) — the
// one case where the old mark describes a different device shape.
//
// a width-change sample taken while the state is OPEN and a text-entry
// element of the page still holds focus is keyboard-reduced, not at rest.
// Learning it as the resting mark would zero the difference, hide the bar,
// and remove the inset while the keyboard is still up — putting the cursor
// behind the keyboard. Such a sample NEVER becomes the baseline: the machine
// preserves OPEN (the safe inset) and defers the reseed until blur/close or
// until a decisively larger scale-normalized sample — keyboard-sized growth,
// not chrome-sized — establishes the new orientation's true resting
// high-water mark. Misreads still bias toward CLOSED everywhere else, whose
// outputs — no pin, no bar — are always safe.

export type KeyboardBaselineSample = Readonly<{
  /** viewport.height * viewport.scale — invariant under pinch zoom. */
  scaledHeight: number;
  /** window.innerWidth — the device-shape discriminator. */
  layoutWidth: number;
  coarsePointer: boolean;
  /** A text-entry element of this page holds focus (helper or composer). */
  textEntryFocused: boolean;
}>;

// Growth and depth discriminator between browser chrome (~90px) and a
// software keyboard (260px and more).
function keyboardThreshold(reference: number): number {
  return Math.max(120, reference * 0.25);
}

export class UnifiedKeyboardBaseline {
  private restingHeight = 0;
  private restingWidth = 0;
  private open = false;
  // A width change arrived while OPEN and focused: the resting mark is stale
  // but the current samples are keyboard-reduced, so no new mark can be
  // learned yet. While pending, the state holds OPEN and floor tracks the
  // smallest sample seen in the new shape.
  private pendingReseed = false;
  private pendingFloor = 0;

  evaluate(sample: KeyboardBaselineSample): boolean {
    const scaled = sample.scaledHeight;
    const width = sample.layoutWidth;
    const coarse = sample.coarsePointer;
    const focused = sample.textEntryFocused;
    if (this.restingWidth === 0) {
      this.restingWidth = width;
      this.restingHeight = scaled;
    } else if (Math.abs(width - this.restingWidth) > 64) {
      this.restingWidth = width;
      if (this.open && coarse && focused) {
        // never learn a resting baseline from a width-change sample
        // while the prior state is OPEN and the page keeps text-entry focus.
        this.pendingReseed = true;
        this.pendingFloor = scaled;
      } else {
        this.restingHeight = scaled;
        this.pendingReseed = false;
      }
    } else if (!this.pendingReseed && scaled > this.restingHeight) {
      this.restingHeight = scaled;
    }
    if (this.pendingReseed) {
      this.pendingFloor = Math.min(this.pendingFloor, scaled);
      const grownEnough = scaled - this.pendingFloor > keyboardThreshold(scaled);
      if (coarse && focused && !grownEnough) {
        // Preserve OPEN: the safe inset stays until blur/close or a
        // keyboard-sized growth reveals the new shape's resting height.
        this.open = true;
        return true;
      }
      // Blur/close, a pointer-class change, or keyboard-sized growth: this
      // sample is at rest in the new shape and becomes the baseline. If the
      // keyboard is in fact still closing, the ordinary high-water raise
      // adopts the taller settled sample on the next evaluation.
      this.restingHeight = scaled;
      this.pendingReseed = false;
    }
    this.open = coarse && this.restingHeight - scaled > keyboardThreshold(this.restingHeight);
    return this.open;
  }

  isOpen(): boolean {
    return this.open;
  }

  /** Diagnostic/test accessor: the current resting high-water mark. */
  restingViewportHeight(): number {
    return this.restingHeight;
  }

  /** Diagnostic/test accessor: a deferred post-rotation reseed is pending. */
  reseedPending(): boolean {
    return this.pendingReseed;
  }
}
