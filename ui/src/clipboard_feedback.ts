import { clipboardIcon } from "./clipboard_icons";

export type ClipboardNoticeKind = "success" | "error" | "info" | "progress";

/** A single, non-modal status message. Its zero-height anchor keeps the
 * clipboard's scroll geometry independent of message length and animation. */
export class ClipboardFeedback {
  readonly element = document.createElement("div");
  private readonly card = document.createElement("div");
  private readonly icon = document.createElement("span");
  private readonly status = document.createElement("p");
  private readonly dismiss = document.createElement("button");
  private expiry?: ReturnType<typeof setTimeout>;
  private clearing?: ReturnType<typeof setTimeout>;
  private transient = false;
  private hovered = false;

  constructor(private readonly returnFocus: () => void) {
    this.element.className = "persea-clipboard__feedback-anchor";
    this.card.className = "persea-clipboard__feedback"; this.card.dataset.visible = "false";
    this.icon.className = "persea-clipboard__feedback-icon"; this.icon.setAttribute("aria-hidden", "true");
    this.status.className = "persea-clipboard__status";
    this.status.setAttribute("role", "status"); this.status.setAttribute("aria-live", "polite"); this.status.setAttribute("aria-atomic", "true");
    this.dismiss.type = "button"; this.dismiss.className = "persea-clipboard__icon persea-clipboard__feedback-dismiss";
    this.dismiss.setAttribute("aria-label", "Dismiss clipboard message"); this.dismiss.title = "Dismiss message";
    this.dismiss.dataset.clipboardDismiss = "true"; this.dismiss.hidden = true;
    this.dismiss.append(clipboardIcon("close"));
    this.dismiss.addEventListener("pointerdown", (event) => {
      if (event.isTrusted && event.button === 0) event.preventDefault();
    });
    this.dismiss.addEventListener("click", (event) => { if (event.isTrusted) this.hide(); });
    this.card.addEventListener("pointerenter", (event) => {
      if (event.pointerType === "mouse") { this.hovered = true; this.pause(); }
    });
    this.card.addEventListener("pointerleave", () => { this.hovered = false; this.schedule(); });
    this.card.addEventListener("focusin", () => this.pause());
    this.card.addEventListener("focusout", () => this.schedule());
    this.card.append(this.icon, this.status, this.dismiss); this.element.append(this.card);
  }

  show(text: string, kind: ClipboardNoticeKind): void {
    this.pause(); clearTimeout(this.clearing); this.clearing = undefined;
    this.transient = kind === "success" || kind === "info";
    this.card.dataset.kind = kind;
    this.icon.replaceChildren(clipboardIcon(kind === "success" ? "check" : kind === "error" ? "warning" : "info"));
    this.status.textContent = text;
    this.card.scrollTop = 0;
    this.dismiss.hidden = false; this.dismiss.disabled = false; this.dismiss.tabIndex = 0;
    this.card.dataset.visible = "true";
    this.schedule();
  }

  reset(): void {
    this.pause(); clearTimeout(this.clearing); this.clearing = undefined;
    this.transient = false; this.hovered = false;
    this.card.dataset.visible = "false"; this.status.textContent = ""; this.dismiss.hidden = true;
  }

  private pause(): void { clearTimeout(this.expiry); this.expiry = undefined; }
  private schedule(): void {
    this.pause();
    if (!this.transient || this.card.dataset.visible !== "true" || this.hovered || this.card.contains(document.activeElement)) return;
    this.expiry = setTimeout(() => this.hide(), 5000);
  }
  private hide(): void {
    this.pause(); this.transient = false;
    if (this.card.contains(document.activeElement)) this.returnFocus();
    this.card.dataset.visible = "false"; this.dismiss.disabled = true; this.dismiss.tabIndex = -1;
    // Keep the visible text during the exit transition. A new message cancels
    // this cleanup so an older timeout cannot erase it.
    const delay = window.matchMedia("(prefers-reduced-motion: reduce)").matches ? 0 : 180;
    clearTimeout(this.clearing);
    this.clearing = setTimeout(() => { this.clearing = undefined; this.status.textContent = ""; this.dismiss.hidden = true; }, delay);
  }
}
