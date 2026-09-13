// Text keeps its native selection gestures. Only unused space to its right
// (or an empty row) is an input target; terminals have no semantic input field.
function isBlankInputPoint(body: HTMLElement, x: number, y: number): boolean {
  const target = document.elementFromPoint(x, y);
  if (!target || !body.contains(target)) return false;
  const selection = window.getSelection();
  if (selection && !selection.isCollapsed) {
    for (let index = 0; index < selection.rangeCount; index += 1) {
      for (const rect of selection.getRangeAt(index).getClientRects()) {
        // Leave room for the OS selection handles as well as the highlight.
        if (x >= rect.left - 24 && x <= rect.right + 24
          && y >= rect.top - 24 && y <= rect.bottom + 24) return false;
      }
    }
  }
  const row = target.closest<HTMLElement>(".persea-unified-select__row");
  if (!row) return target === body;
  const walker = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
  let lastText: Text | undefined;
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (node.textContent?.trimEnd()) lastText = node as Text;
  }
  if (!lastText) return true;
  const range = document.createRange();
  range.selectNodeContents(row);
  range.setEnd(lastText, lastText.data.trimEnd().length);
  return x > range.getBoundingClientRect().right + 12;
}

/** Resume input only on a completed tap belonging to this frozen snapshot. */
export function bindFrozenSelectionInput(
  body: HTMLElement,
  selectionEpoch: () => number | undefined,
  resumeInput: () => void,
): () => void {
  let tap: { pointer: number; pointerType: string; epoch: number; x: number; y: number; started: number; released: boolean } | undefined;
  const cancel = () => { tap = undefined; };
  const onDown = (event: PointerEvent) => {
    cancel(); // A second finger or a gesture elsewhere cancels the first tap.
    const epoch = selectionEpoch();
    if (!event.isTrusted || !event.isPrimary || event.button !== 0 || epoch === undefined
      || !isBlankInputPoint(body, event.clientX, event.clientY)) return;
    tap = { pointer: event.pointerId, pointerType: event.pointerType, epoch,
      x: event.clientX, y: event.clientY, started: event.timeStamp, released: false };
  };
  const onMove = (event: PointerEvent) => {
    if (tap && event.pointerId === tap.pointer
      && Math.hypot(event.clientX - tap.x, event.clientY - tap.y) > 12) cancel();
  };
  const onUp = (event: PointerEvent) => {
    const pending = tap;
    cancel();
    if (!pending || !event.isTrusted || event.pointerId !== pending.pointer
      || selectionEpoch() !== pending.epoch || event.timeStamp - pending.started > 350
      || Math.hypot(event.clientX - pending.x, event.clientY - pending.y) > 12
      || !isBlankInputPoint(body, event.clientX, event.clientY)) return;
    tap = { ...pending, released: true };
  };
  const complete = (event: MouseEvent | TouchEvent) => {
    const pending = tap;
    cancel();
    if (!pending?.released || !event.isTrusted || selectionEpoch() !== pending.epoch
      || event.timeStamp - pending.started > 500
      || (event.type === "touchend") !== (pending.pointerType === "touch")) return;
    event.preventDefault();
    event.stopPropagation();
    // Finish the native gesture before removing its target. Hiding the overlay
    // on pointerup lets a touch's compatibility click hit the live terminal.
    // Focus stays synchronous in the trusted touchend/click for iOS keyboards.
    resumeInput();
  };
  window.addEventListener("pointerdown", onDown, true);
  window.addEventListener("pointermove", onMove, true);
  window.addEventListener("pointerup", onUp, true);
  window.addEventListener("pointercancel", cancel, true);
  window.addEventListener("blur", cancel);
  document.addEventListener("visibilitychange", cancel);
  body.addEventListener("scroll", cancel, { passive: true });
  body.addEventListener("contextmenu", cancel);
  body.addEventListener("touchend", complete, { passive: false });
  body.addEventListener("click", complete, true);
  return () => {
    cancel();
    window.removeEventListener("pointerdown", onDown, true);
    window.removeEventListener("pointermove", onMove, true);
    window.removeEventListener("pointerup", onUp, true);
    window.removeEventListener("pointercancel", cancel, true);
    window.removeEventListener("blur", cancel);
    document.removeEventListener("visibilitychange", cancel);
    body.removeEventListener("scroll", cancel);
    body.removeEventListener("contextmenu", cancel);
    body.removeEventListener("touchend", complete);
    body.removeEventListener("click", complete, true);
  };
}
