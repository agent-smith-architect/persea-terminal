export type CopyFeedbackState = "none" | "ready" | "done" | "failed";

export type CopyFeedbackClock = Readonly<{
  set: (callback: () => void, delay: number) => ReturnType<typeof setTimeout>;
  clear: (timer: ReturnType<typeof setTimeout>) => void;
}>;

const DEFAULT_CLOCK: CopyFeedbackClock = Object.freeze({
  set: (callback, delay) => setTimeout(callback, delay),
  clear: (timer) => clearTimeout(timer),
});

/** Owns one copy control's transient outcome without changing its box. */
export class CopyFeedback {
  private state: CopyFeedbackState = "none";
  private ready = false;
  private timer?: ReturnType<typeof setTimeout>;
  private attempt = 0;

  constructor(
    private readonly render: (state: CopyFeedbackState) => void,
    private readonly clock: CopyFeedbackClock = DEFAULT_CLOCK,
  ) {
    render("none");
  }

  /** Passive render/readiness sync; identical readiness cannot retire an outcome. */
  setReady(ready: boolean): void {
    if (this.ready === ready) return;
    this.invalidate(ready);
  }

  /** Retires an outcome because the value the control would copy changed. */
  selectionChanged(ready: boolean): void {
    this.invalidate(ready);
  }

  private invalidate(ready: boolean): void {
    this.ready = ready;
    this.attempt += 1;
    this.clearTimer();
    this.publish(ready ? "ready" : "none");
  }

  flash(copied: boolean): void {
    const attempt = this.beginAttempt();
    this.settleAttempt(attempt, copied);
  }

  beginAttempt(): number {
    this.clearTimer();
    this.attempt += 1;
    return this.attempt;
  }

  settleAttempt(attempt: number, copied: boolean): void {
    if (attempt !== this.attempt) return;
    this.publish(copied ? "done" : "failed");
    this.timer = this.clock.set(() => {
      this.timer = undefined;
      this.publish(this.ready ? "ready" : "none");
    }, copied ? 1_500 : 2_500);
  }

  dispose(): void {
    this.attempt += 1;
    this.clearTimer();
  }

  current(): CopyFeedbackState {
    return this.state;
  }

  private clearTimer(): void {
    if (this.timer === undefined) return;
    this.clock.clear(this.timer);
    this.timer = undefined;
  }

  private publish(state: CopyFeedbackState): void {
    this.state = state;
    this.render(state);
  }
}
