import { bindGenerationFencedClickActivation } from "../src/tap_activation";

type Receipt = Readonly<{
  type: string;
  trusted: boolean;
  detail: number;
  key?: string;
  pointerId?: number;
  pointerType?: string;
  generation: number;
  prevented: boolean;
}>;

let button: HTMLButtonElement;
let generation = 0;
let actions: Receipt[] = [];
let events: Receipt[] = [];
let dispose: (() => void) | undefined;

function receipt(event: Event): Receipt {
  return {
    type: event.type,
    trusted: event.isTrusted,
    detail: event instanceof UIEvent ? event.detail : 0,
    key: event instanceof KeyboardEvent ? event.key : undefined,
    pointerId: event instanceof PointerEvent ? event.pointerId : undefined,
    pointerType: event instanceof PointerEvent ? event.pointerType : undefined,
    generation,
    prevented: event.defaultPrevented,
  };
}

// Browser-only harness. Input comes from the browser driver, never dispatchEvent.
export function mount(): void {
  dispose?.();
  generation = 0;
  actions = [];
  events = [];
  button = document.createElement("button");
  button.type = "button";
  button.textContent = "Native activation";
  button.id = "native-activation";
  button.style.cssText = "position:absolute;left:20px;top:20px;width:150px;height:60px";
  document.body.replaceChildren(button);
  dispose = bindGenerationFencedClickActivation(
    button,
    (event) => actions.push(receipt(event)),
    () => true,
    () => generation,
  );
  for (const type of ["pointerdown", "pointerleave", "pointerup", "pointercancel", "click", "keydown", "keyup"]) {
    button.addEventListener(type, (event) => events.push(receipt(event)));
  }
}

export function advanceGeneration(): void { generation += 1; }
export function focus(): void { button.focus(); }
export function programmaticClick(): void { button.click(); }

// This reaches the captureless trusted-click branch; it is not an OS AT test.
// The actual click remains the browser's native Enter default action.
export function withholdNextKeydown(): void {
  button.addEventListener("keydown", (event) => event.stopImmediatePropagation(), { capture: true, once: true });
}

export function snapshot(): Readonly<{ actions: readonly Receipt[]; events: readonly Receipt[] }> {
  return { actions: [...actions], events: [...events] };
}
