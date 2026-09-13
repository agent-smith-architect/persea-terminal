// Same shape as the other unit files: no node typings in the test bundle.
const assert = {
  equal<T>(actual: T, expected: T, message?: string): void {
    if (actual !== expected) throw new Error(`${message ?? "assertion failed"}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
  },
};

import { commitFocusClaim, pointerRuleClaims, type CommitFocusContext } from "../src/unified_focus_claim";

// The pointer rule: fine pointers always claim; coarse pointers only to
// consume the one-shot keyboard restore.
assert.equal(pointerRuleClaims(false, false), true, "a fine pointer must always claim");
assert.equal(pointerRuleClaims(false, true), true, "a fine pointer must always claim");
assert.equal(pointerRuleClaims(true, false), false, "a coarse pointer must not claim without the restore");
assert.equal(pointerRuleClaims(true, true), true, "a coarse pointer must claim to consume the restore");

const context = (coarsePointer: boolean, restoreKeyboardOnCommit: boolean): CommitFocusContext => Object.freeze({
  coarsePointer, restoreKeyboardOnCommit, pointerRuleClaims: pointerRuleClaims(coarsePointer, restoreKeyboardOnCommit),
});

// No hook: the pointer rule is the verdict.
assert.equal(commitFocusClaim(context(false, false), undefined), true);
assert.equal(commitFocusClaim(context(true, false), undefined), false);
assert.equal(commitFocusClaim(context(true, true), undefined), true);

// N3: the hook is a veto, never a promotion. A consumer that answers true
// unconditionally cannot raise a keyboard the page would not have raised.
const promote = () => true;
assert.equal(commitFocusClaim(context(true, false), promote), false, "the hook promoted focus on a coarse pointer");
assert.equal(commitFocusClaim(context(true, true), promote), true);
assert.equal(commitFocusClaim(context(false, false), promote), true);

// A veto holds on every pointer.
const veto = () => false;
assert.equal(commitFocusClaim(context(false, false), veto), false, "the hook could not veto a fine-pointer claim");
assert.equal(commitFocusClaim(context(true, true), veto), false, "the hook could not veto the restore");

// The hook is consulted only when the page would claim, and sees the context.
let seen: CommitFocusContext | undefined;
assert.equal(commitFocusClaim(context(true, false), (ctx) => { seen = ctx; return true; }), false);
assert.equal(seen, undefined, "the hook was consulted although the pointer rule already said no");
assert.equal(commitFocusClaim(context(false, false), (ctx) => { seen = ctx; return ctx.pointerRuleClaims; }), true);
assert.equal(seen?.pointerRuleClaims, true);

// Only a literal true claims: a truthy non-boolean from a loosely typed
// consumer does not.
assert.equal(commitFocusClaim(context(false, false), (() => 1) as unknown as () => boolean), false);

console.log("unified focus claim tests PASS");
