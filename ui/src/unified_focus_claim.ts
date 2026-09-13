// The COMMIT focus claim, as a pure function so its shape is unit-pinned.
//
// What the page knows when it decides whether COMMIT may claim focus (and so
// raise a software keyboard). The page's own rule is the pointer rule; the
// optional hook (M9's "which pane" rule) can only VETO a claim, never promote
// one: the result is structurally `pointerRuleClaims && policy(context)`, so
// no consumer can raise a keyboard on a coarse pointer the page would not
// have raised itself (advisor note N3).
export type CommitFocusContext = Readonly<{
  // `(pointer: coarse)` matched at the moment of COMMIT.
  coarsePointer: boolean;
  // The one-shot keyboard restore: the software keyboard was OPEN — the
  // page's viewport classifier, not focus; mobile Safari can leave the
  // textarea focused after a dismissal — AND a text entry of THIS page
  // (xterm's helper textarea or the composer input) held focus when the
  // previous transport generation closed. Captured for coarse pointers at the
  // close, before admission state resets; cleared by the next COMMIT, an
  // explicit Hide, a dismissal the classifier sees, a terminal failure, or
  // destroy. Always false for a page's first admission.
  restoreKeyboardOnCommit: boolean;
  // The page's own verdict: fine pointers always claim (desktop behaviour is
  // byte-identical to before the hook existed); coarse pointers claim only to
  // restore a keyboard that was open at the close.
  pointerRuleClaims: boolean;
}>;

export type CommitFocusPolicy = (context: CommitFocusContext) => boolean;

export function pointerRuleClaims(coarsePointer: boolean, restoreKeyboardOnCommit: boolean): boolean {
  return !coarsePointer || restoreKeyboardOnCommit;
}

export function commitFocusClaim(context: CommitFocusContext, policy: CommitFocusPolicy | undefined): boolean {
  if (!context.pointerRuleClaims) return false;
  return policy === undefined ? true : policy(context) === true;
}
