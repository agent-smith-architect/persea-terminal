// Reconcile connected, keyed islands without replacing unaffected controls.
// The same node owns its listeners, pending requests, draft and selection.
// Moving a node detaches it first, and a detached open dialog leaves the top
// layer: it stays open but is no longer modal. A child holding an open dialog
// therefore keeps its place and the others move around it; the order is the same.
export function reconcileChildren(parent: HTMLElement, desired: readonly HTMLElement[]): void {
  const keep = new Set(desired);
  for (const child of Array.from(parent.children)) if (!keep.has(child as HTMLElement)) child.remove();
  const held = desired.findIndex(child => child.parentElement === parent && child.querySelector("dialog[open]") !== null);
  if (held < 0) { placeInOrder(parent, desired, parent.firstElementChild); return; }
  placeInOrder(parent, desired.slice(0, held), parent.firstElementChild);
  placeInOrder(parent, desired.slice(held + 1), desired[held].nextElementSibling);
}

function placeInOrder(parent: HTMLElement, children: readonly HTMLElement[], cursor: Element | null): void {
  for (const child of children) {
    if (child === cursor) cursor = cursor.nextElementSibling;
    else parent.insertBefore(child, cursor);
  }
}

export function preserveFocus(update: () => void): void {
  const active = document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
  const selection = active instanceof HTMLInputElement && /^(text|search)$/.test(active.type)
    ? [active.selectionStart, active.selectionEnd, active.selectionDirection] as const : undefined;
  update();
  if (active?.isConnected && document.activeElement !== active) {
    active.focus({ preventScroll: true });
    if (selection && active instanceof HTMLInputElement) active.setSelectionRange(selection[0], selection[1], selection[2] ?? undefined);
  }
}

export function hasDraft(root: HTMLElement): boolean {
  return Array.from(root.querySelectorAll<HTMLInputElement>("input")).some(input => input.value !== input.defaultValue)
    || root.querySelector('[data-busy="true"]') !== null;
}
