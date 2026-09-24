// every button press paints a brief flash after the click lands.
// On a phone the :active state is gone before the finger lifts, so without
// this a tap on Fit rows, Apply, Select or any other control gives no sign
// that it registered. A disabled button does not flash: nothing registered,
// and its dimmed face already says so.
const TAP_CLASS = "persea-tapped";
const TAP_MS = 340;

export function installTapFeedback(root: HTMLElement | Document): () => void {
  // One timer per button (tap-feedback-timer): a repeat tap inside the dwell restarts its
  // own flash, and only the newest timer may remove the class — the first
  // tap's timer must never cut the second tap's feedback short.
  const timers = new Map<HTMLElement, number>();
  const clear = (button: HTMLElement): void => {
    const timer = timers.get(button);
    if (timer !== undefined) {
      window.clearTimeout(timer);
      timers.delete(button);
    }
    button.classList.remove(TAP_CLASS);
  };
  const onClick = (event: Event): void => {
    const target = event.target;
    if (!(target instanceof Element)) return;
    const button = target.closest("button, [role=button]");
    if (!(button instanceof HTMLElement)) return;
    if (button instanceof HTMLButtonElement && button.disabled) return;
    if (button.getAttribute("aria-disabled") === "true") return;
    clear(button);
    // Reading the layout restarts the animation for a second tap in a row.
    void button.offsetWidth;
    button.classList.add(TAP_CLASS);
    const timer = window.setTimeout(() => {
      if (timers.get(button) !== timer) return;
      timers.delete(button);
      button.classList.remove(TAP_CLASS);
    }, TAP_MS);
    timers.set(button, timer);
  };
  root.addEventListener("click", onClick, true);
  return () => {
    root.removeEventListener("click", onClick, true);
    for (const button of Array.from(timers.keys())) clear(button);
  };
}
