import { bindTapActivation } from "../src/tap_activation";

type Receipt = Readonly<{
  type: string;
  trusted: boolean;
  pointerId?: number;
  pointerType?: string;
  generation: number;
}>;

let button: HTMLButtonElement;
let status: HTMLDivElement;
let list: HTMLDivElement;
let detachOnPress = false;
let generation = 0;
let pressed: number | undefined;
let actions: Receipt[] = [];
let dispose: (() => void) | undefined;

function receipt(event: Event): Receipt {
  return {
    type: event.type,
    trusted: event.isTrusted,
    pointerId: event instanceof PointerEvent ? event.pointerId : undefined,
    pointerType: event instanceof PointerEvent ? event.pointerType : undefined,
    generation,
  };
}

// Browser-only harness. Input comes from the browser driver; only leave()
// dispatches an event. An empty status line sits above the control in normal
// flow, as the terminal's connection status does above an open sheet; the
// control sits in a list that can be hidden, as a session row does.
export function mount(): void {
  dispose?.();
  generation = 0;
  actions = [];
  detachOnPress = false;
  status = document.createElement("div");
  status.style.cssText = "font:16px/40px sans-serif";
  list = document.createElement("div");
  button = document.createElement("button");
  button.type = "button";
  button.textContent = "Tap activation";
  button.style.cssText = "display:block;margin:20px;width:150px;height:60px;touch-action:manipulation";
  list.append(button);
  document.body.style.margin = "0";
  document.body.replaceChildren(status, list);
  // Registered before the binding, so it runs first.
  button.addEventListener("pointerdown", (event) => {
    pressed = event.pointerId;
    if (detachOnPress) button.remove();
  });
  dispose = bindTapActivation(button, (event) => actions.push(receipt(event)), () => {}, () => true, () => generation);
}

// The status line appears: everything below it moves down by its height.
export function showStatus(): Promise<void> {
  status.textContent = "Loading history…";
  return new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
}

export function advanceGeneration(): void { generation += 1; }

// The list closes without a change of interaction generation.
export function hideList(): void { list.hidden = true; }

// The list stays in the layout but is not visible.
export function concealList(): void { list.style.visibility = "hidden"; }

export function disable(): void { button.disabled = true; }

// An earlier pointerdown listener removes the control, so the binding's
// pointer capture fails.
export function detachOnNextPress(): void { detachOnPress = true; }

// The pointerleave an engine may send when the layout moves the control from
// under a still pointer, at a moment a test cannot choose (WebKit does).
export function leave(): void {
  button.dispatchEvent(new PointerEvent("pointerleave", { pointerId: pressed, pointerType: "mouse" }));
}

export function box(): Readonly<{ x: number; y: number; width: number; height: number }> {
  const rect = button.getBoundingClientRect();
  return { x: rect.x, y: rect.y, width: rect.width, height: rect.height };
}

export function snapshot(): Readonly<{ actions: readonly Receipt[] }> {
  return { actions: [...actions] };
}
