import { CopyFeedback, type CopyFeedbackClock, type CopyFeedbackState } from "../src/copy_feedback";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void {
    if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`);
  },
  deepEqual(actual: unknown, expected: unknown, message = "values differ"): void {
    if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(message);
  },
};

type Task = { callback: () => void; delay: number; due: number; cancelled: boolean };
const tasks: Task[] = [];
let now = 0;
const clock: CopyFeedbackClock = {
  set(callback, delay) {
    const task = { callback, delay, due: now + delay, cancelled: false };
    tasks.push(task);
    return task as unknown as ReturnType<typeof setTimeout>;
  },
  clear(timer) {
    (timer as unknown as Task).cancelled = true;
  },
};
const advance = (milliseconds: number): void => {
  now += milliseconds;
  for (const task of tasks) {
    if (!task.cancelled && task.due <= now) {
      task.cancelled = true;
      task.callback();
    }
  }
};
const states: CopyFeedbackState[] = [];
const feedback = new CopyFeedback((state) => states.push(state), clock);

assert.deepEqual(states, ["none"]);
feedback.setReady(true);
feedback.flash(true);
assert.equal(feedback.current(), "done");
assert.equal(tasks.at(-1)?.delay, 1_500);
const doneTimer = tasks.at(-1)!;
feedback.setReady(true);
advance(999);
feedback.setReady(true);
assert.equal(feedback.current(), "done", "background readiness sync must preserve Copy dwell at 999ms");
advance(500);
feedback.setReady(true);
assert.equal(feedback.current(), "done", "background readiness sync must preserve Copy dwell at 1499ms");
assert.equal(doneTimer.cancelled, false, "idempotent readiness sync must not replace the outcome timer");
advance(1);
assert.equal(feedback.current(), "ready");

feedback.flash(false);
assert.equal(feedback.current(), "failed");
assert.equal(tasks.at(-1)?.delay, 2_500);
advance(2_500);
assert.equal(feedback.current(), "ready");

const stale = feedback.beginAttempt();
const current = feedback.beginAttempt();
feedback.settleAttempt(stale, false);
assert.equal(feedback.current(), "ready", "a stale promise cannot publish");
feedback.settleAttempt(current, true);
assert.equal(feedback.current(), "done");
advance(1_500);

feedback.flash(true);
const firstDone = tasks.at(-1)!;
feedback.flash(true);
assert.equal(firstDone.cancelled, true, "a second copy retires the first timer");
advance(1_500);
assert.equal(feedback.current(), "ready");

feedback.flash(false);
const pending = tasks.at(-1)!;
feedback.setReady(true);
assert.equal(pending.cancelled, false, "same readiness must preserve a failed outcome");
assert.equal(feedback.current(), "failed");
feedback.selectionChanged(true);
assert.equal(pending.cancelled, true, "a real new selection retires the old outcome");
assert.equal(feedback.current(), "ready");

feedback.flash(false);
const readinessChange = tasks.at(-1)!;
feedback.setReady(false);
assert.equal(readinessChange.cancelled, true, "a readiness change retires stale outcomes");
assert.equal(feedback.current(), "none");

feedback.flash(true);
const disposed = tasks.at(-1)!;
feedback.dispose();
assert.equal(disposed.cancelled, true, "dispose cancels the per-control timer");

console.log("copy feedback state machine: PASS");
