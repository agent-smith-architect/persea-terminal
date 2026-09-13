import { bindKeyboardActivationFence } from "./tap_activation";

export function bindKeyboardPreservingActivation(
  button: HTMLButtonElement,
  activate: (event: Event) => void,
  restoreFocus: () => void,
  isLive: () => boolean,
  interactionGeneration: () => number,
): void {
  const keyboardFence = bindKeyboardActivationFence(button, interactionGeneration);
  const blockFocus = (event: Event): void => {
    if (!event.isTrusted || !isLive()) return;
    event.preventDefault();
  };
  button.addEventListener("mousedown", blockFocus);
  button.addEventListener("touchstart", blockFocus, { passive: false });
  button.addEventListener("pointerdown", (event) => {
    if (!event.isTrusted || !isLive()) return;
    event.preventDefault();
    keyboardFence.revoke();
    activate(event);
    restoreFocus();
  });
  button.addEventListener("click", (event) => {
    if (!event.isTrusted || event.detail !== 0) return;
    const keyboardAllowed = keyboardFence.acceptClick(event);
    if (!isLive() || !keyboardAllowed) return;
    activate(event);
  });
}
