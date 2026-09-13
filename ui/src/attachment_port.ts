import type { BrowserFrame, HistoryRequest, UInt64 } from "./attachment_protocol";
import type { HistoryDepthOutcome } from "./websocket_attachment_transport";

export type PortSendResult = "ACCEPTED" | "SATURATED" | "CLOSED" | "TRANSPORT_LOST";
export type FinalizeCause =
  | "MALFORMED_FRAME" | "OUT_OF_STATE" | "ACTIVE_TUPLE_MISMATCH"
  | "INBOUND_SATURATED" | "OUTBOUND_SATURATED" | "OUTBOUND_CLOSED"
  | "ADMISSION_INVARIANT" | "MOUNT_FAILED" | "REPLAY_FAILED" | "REUSED_READER_COMMIT_FAILED"
  | "LIVE_WRITE_FAILED" | "COMPONENT_PRE_MUTATION_FAILED"
  | "COMPONENT_POST_MUTATION_FAILED" | "UNEXPECTED_COMPONENT_REJECTION"
  | "INPUT_SATURATED" | "RESOURCE_FAILURE";
export type FinalizeIntent = Readonly<{
  generation: number;
  source?: string;
  epoch?: UInt64;
  cause: FinalizeCause;
}>;
export type AttachmentPagePort = Readonly<{
  trySend(generation: number, frame: BrowserFrame): PortSendResult;
  retryHistoryDepth?(generation: number, frame: HistoryRequest): PortSendResult;
  onHistoryDepthOutcome?(listener: (outcome: HistoryDepthOutcome) => void): () => void;
  finalize(intent: FinalizeIntent): void;
  detach?(reason?: string): void;
  attachAgain?(): void;
  takeControl?(endpoint: Readonly<{ url: string; protocols: readonly string[] }>): void;
  connectionCommitted?(generation: number): void;
  destroy?(): void;
}>;
