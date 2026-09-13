// Reconcile connected, keyed islands without replacing unaffected controls.
// The same node owns its listeners, pending requests, draft and selection.
export function reconcileChildren(parent: HTMLElement, desired: readonly HTMLElement[]): void {
  const keep = new Set(desired);
  for (const child of Array.from(parent.children)) if (!keep.has(child as HTMLElement)) child.remove();
  let cursor = parent.firstElementChild;
  for (const child of desired) {
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
