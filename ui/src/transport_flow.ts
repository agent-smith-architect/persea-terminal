// Flow acknowledgements pace server-to-browser output. The front door keeps a
// bounded window of unacknowledged attachment frames in flight, so liveness
// and control frames never queue behind more than that window on a slow link.
// The page acknowledges a frame once everything it caused in the terminal has
// been written, never on arrival: arrival only proves the bytes left the
// network, not that the page kept up. The count is cumulative per WebSocket
// and covers attachment frames only; liveness and refusal frames are not
// counted.
export const TRANSPORT_FLOW_PREFIX = "PERSEA-FLOW/1 ";

export function encodeBrowserFlowAck(count: number): string {
  if (!Number.isSafeInteger(count) || count < 1) throw new Error("transport flow count unavailable");
  return `${TRANSPORT_FLOW_PREFIX}ACK ${count}`;
}

// FlowAcknowledger numbers the attachment frames of one WebSocket and
// acknowledges the longest prefix of them that has been consumed. Frames can
// complete out of order (a control frame completes at once, output only after
// the terminal wrote it), so only a contiguous prefix is acknowledged.
// Acknowledgements are coalesced: consumption within one task sends one.
export class FlowAcknowledger {
  private received = 0;
  private consumed = 0;
  private acknowledged = 0;
  private readonly completed = new Set<number>();
  private scheduled = false;
  private retired = false;

  constructor(
    // Sends one acknowledgement; false when the socket can no longer carry it.
    private readonly send: (payload: string) => boolean,
    private readonly schedule: (callback: () => void) => void = (callback) => queueMicrotask(callback),
  ) {}

  // Numbers the next attachment frame.
  receive(): number {
    return ++this.received;
  }

  // Marks one numbered frame consumed.
  consume(sequence: number): void {
    if (this.retired || sequence <= this.consumed || sequence > this.received || this.completed.has(sequence)) return;
    this.completed.add(sequence);
    while (this.completed.delete(this.consumed + 1)) this.consumed++;
    if (this.consumed > this.acknowledged && !this.scheduled) {
      this.scheduled = true;
      this.schedule(() => this.flush());
    }
  }

  retire(): void {
    this.retired = true;
    this.completed.clear();
  }

  private flush(): void {
    this.scheduled = false;
    if (this.retired || this.consumed <= this.acknowledged) return;
    if (this.send(encodeBrowserFlowAck(this.consumed))) this.acknowledged = this.consumed;
  }
}
