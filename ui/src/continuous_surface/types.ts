import type { Terminal } from "@xterm/xterm";

export type LogicalKey =
  | "escape" | "tab" | "backspace" | "delete"
  | "arrow-up" | "arrow-down" | "arrow-left" | "arrow-right"
  | "home" | "end" | "page-up" | "page-down"
  | "f1" | "f2" | "f3" | "f4" | "f5" | "f6"
  | "f7" | "f8" | "f9" | "f10" | "f11" | "f12"
  | { readonly ctrl: string };

export type HistorySource = {
  initialRows(): readonly string[];
};

export type ResolvedStyle = Readonly<{
  foreground: string;
  background: string;
  bold: boolean;
  dim: boolean;
  italic: boolean;
  underline: boolean;
  strike: boolean;
}>;

export type TextRun = Readonly<{
  text: string;
  style: ResolvedStyle;
}>;

export type ParsedRow = Readonly<{
  text: string;
  runs: readonly TextRun[];
  inputUnits: number;
  truncated: boolean;
}>;

export type LiveBridgeCell = Readonly<{
  chars: string;
  code: number;
  width: number;
  fgMode: number;
  fg: number;
  bgMode: number;
  bg: number;
  bold: boolean;
  dim: boolean;
  italic: boolean;
  underline: boolean;
  blink: boolean;
  inverse: boolean;
  invisible: boolean;
  strike: boolean;
  overline: boolean;
}>;

export type LiveBridgeRow = Readonly<{
  cut: bigint;
  writeOrdinal: number;
  evictionOrdinal: number;
  columns: number;
  cells: readonly LiveBridgeCell[];
  isWrapped: boolean;
  comparisonKey: string;
  serializedCellBytes: number;
}>;

export type LiveBridgeSnapshot = Readonly<{
  rows: readonly LiveBridgeRow[];
  confirmationFrozen: boolean;
  canonicalHistoryRows: readonly string[];
  canonicalHistoryKeys: readonly string[];
}>;

export type LiveBridgeFailureReason =
  | "CAPTURE_SLOT_SATURATED"
  | "CAPTURE_STATE_AMBIGUOUS"
  | "CAPTURE_COPY_FAILED"
  | "CAPTURE_DRAIN_FAILED"
  | "BRIDGE_BUDGET_SATURATED"
  | "LIVE_WRITE_FAILED";

export type SurfaceSelectionLayer = "canonical" | "bridge" | "live";
export type SurfaceSelectionState = "dragging" | "settled" | "transfer-in-progress" | "cancelled";

/** Opaque row identity plus a boundary between physical cells. */
export type SurfaceSelectionEndpoint = Readonly<{
  layer: SurfaceSelectionLayer;
  rowToken: string;
  cellBoundary: number;
}>;

export type SurfaceSelectionSpan = Readonly<{
  layer: SurfaceSelectionLayer;
  rowToken: string;
  startCell: number;
  endCell: number;
}>;

export type SurfaceSelection = Readonly<{
  generation: number;
  owner: string;
  anchor: SurfaceSelectionEndpoint;
  focus: SurfaceSelectionEndpoint;
  direction: "forward" | "reverse";
  spans: readonly SurfaceSelectionSpan[];
  state: SurfaceSelectionState;
}>;

export type SurfaceSelectionChange = Readonly<{
  selection: SurfaceSelection | undefined;
  reason: string;
}>;

export type SurfaceDomBoundary = Readonly<{
  node: Node;
  offset: number;
}>;

/**
 * History enters this module as an already-decoded JavaScript string. The caller
 * owns byte-level UTF-8 decoding (including invalid or overlong byte sequences).
 * Within this string boundary U+FFFD is inert printable text and unpaired UTF-16
 * surrogates are discarded by the row parser.
 */
export const CAPTURED_ROW_INPUT_BOUNDARY = "decoded-javascript-string";

export type AnchorEviction = Readonly<{
  text: string;
  removedRows: number;
}>;

export type ReconciliationResult = Readonly<{
  overlapRows: number;
  removedRows: number;
  prependedRows: number;
  appendedRows: number;
  anchorEvicted: boolean;
}>;

export type ReconciliationRevision = Readonly<{
  history: readonly string[];
  replay: Uint8Array;
  kind?: "ONGOING" | "HISTORY";
  cut?: bigint;
}>;

export type ReconciliationDeferReason =
  | "ACTIVE_CONTACT_OR_SCROLL_GESTURE"
  | "INTERSECTING_DOM_SELECTION"
  | "AFFECTED_XTERM_SELECTION"
  | "VISIBLE_ANCHOR_WOULD_BE_EVICTED";

export type ReconciliationFailureReason =
  | "DESTROYED"
  | "DOM_MUTATION_FAILED"
  | "REPLAY_WRITE_FAILED"
  | "INVARIANT_VIOLATION";

export type ReconciliationCompletion =
  | Readonly<{ type: "READY"; reconciliation: ReconciliationResult }>
  | Readonly<{
      type: "FAILED";
      checkpoint: "pre-mutation" | "post-mutation";
      reason: ReconciliationFailureReason;
    }>;

export type ReconciliationAdmission =
  | Readonly<{ type: "DEFERRED"; reason: ReconciliationDeferReason }>
  | Readonly<{
      type: "ACCEPTED";
      completion: Promise<ReconciliationCompletion>;
      authoritativeHistoryHeld?: true;
    }>;

export type ReaderHoldReason =
  | "NOT_FOLLOWING_LIVE"
  | "ACTIVE_CONTACT_OR_SCROLL_GESTURE"
  | "DOM_SELECTION"
  | "XTERM_SELECTION";

export type ReaderPresentationDisposition =
  | Readonly<{ type: "AUTO_PRESENT" }>
  | Readonly<{ type: "HOLD_READER"; reasons: readonly ReaderHoldReason[] }>;

/**
 * Public, read-only proof that the reusable surface has one scroll owner.
 * The xterm viewport is styled through its stable public CSS class rather than
 * a private xterm service.
 */
export type ContinuousSurfaceOneScrollOwnerState = Readonly<{
  terminalScrollback: 1;
  innerViewportBoundary: "stable-xterm-css";
  innerViewportPresent: boolean;
}>;

export type InitialDisplayFit = "ON_MOUNT" | "BEFORE_PRESENTATION";

export type ContinuousSurfaceOptions = Readonly<{
  live: Terminal;
  liveHost: HTMLElement;
  /**
   * One-shot synchronous binding for the canonical live host. The callback must
   * open `live` into exactly this host and return `undefined`.
   */
  initializeLive: (host: HTMLElement) => undefined;
  history: HistorySource;
  /**
   * Optional product policy for whether a wheel event reaches xterm.  Returning
   * false leaves the event to the one outer browser scroller.
   */
  shouldProcessTerminalWheel?: (event: WheelEvent, terminal: Terminal) => boolean;
  onAnchorEvicted?: (event: AnchorEviction) => void;
  synchronousInitialRender?: boolean;
  /** Attachment candidates defer fitting until their replay has populated xterm. */
  initialDisplayFit?: InitialDisplayFit;
}>;
