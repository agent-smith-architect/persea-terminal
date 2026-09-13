import { LongPressGesture, type LongPressClock } from "../src/tap_activation";

const equal = (actual: unknown, expected: unknown, message: string): void => {
  if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`);
};

class FakeClock implements LongPressClock {
  private now = 0;
  private serial = 0;
  private readonly timers = new Map<number, { at: number; callback: () => void }>();

  setTimeout(callback: () => void, delay: number): unknown {
    const id = ++this.serial;
    this.timers.set(id, { at: this.now + delay, callback });
    return id;
  }

  clearTimeout(handle: unknown): void { this.timers.delete(handle as number); }

  advance(milliseconds: number): void {
    this.now += milliseconds;
    for (;;) {
      const due = [...this.timers.entries()].filter(([, timer]) => timer.at <= this.now).sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) return;
      this.timers.delete(due[0]);
      due[1].callback();
    }
  }
}

function fixture(): { clock: FakeClock; gesture: LongPressGesture; explanations: () => number } {
  const clock = new FakeClock();
  let count = 0;
  return {
    clock,
    gesture: new LongPressGesture(() => { count += 1; }, clock),
    explanations: () => count,
  };
}

{
  const value = fixture();
  value.gesture.pointerDown(1, 10, 10);
  value.clock.advance(499);
  equal(value.gesture.pointerUp(1), "primary", "a short tap performs exactly the primary action");
  value.clock.advance(1);
  equal(value.explanations(), 0, "a short tap never opens the explainer");
}

{
  const value = fixture();
  value.gesture.pointerDown(2, 10, 10);
  value.clock.advance(500);
  equal(value.explanations(), 1, "500 ms opens the explainer once");
  equal(value.gesture.pointerUp(2), "consumed", "a long press consumes the primary action");
}

for (const cancel of [
  (gesture: LongPressGesture) => gesture.pointerMove(3, 30, 10),
  (gesture: LongPressGesture) => gesture.cancel(),
]) {
  const value = fixture();
  value.gesture.pointerDown(3, 10, 10);
  cancel(value.gesture);
  value.clock.advance(800);
  equal(value.explanations(), 0, "movement/cancellation opens nothing");
  equal(value.gesture.pointerUp(3), "none", "movement/cancellation performs no primary action");
}

console.log("composed tap and long-press activation: PASS");
