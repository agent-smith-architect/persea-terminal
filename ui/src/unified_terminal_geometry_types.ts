// Terminal geometry availability, width refit, and geometry form types.

export type WidthRefitResult = Readonly<{
  ok: boolean;
  message: string;
  disposition: "success" | "refused" | "terminal" | "uncertain";
	successorSource: string;
}>;

export type WidthRefitAttempt = Readonly<{
  operation: string;
  predecessorSource: string;
  predecessorIncarnation: string;
  result: Promise<WidthRefitResult>;
}>;

export type PendingWidthRefit = {
  operation: string;
  predecessorSource: string;
  predecessorIncarnation: string;
  predecessorGeneration: number;
  predecessorEndpointOperation: number;
	successorSource: string;
  awaitingSuccessor: boolean;
  expected?: Readonly<{ generation: number; source: string; epoch: bigint; cut: bigint }>;
  droppedBytes: number;
};

export type UnifiedGeometryAction = "fit_rows" | "fit_width" | "apply";

export type UnifiedGeometryAvailabilityCode =
  | "available"
  | "closed"
  | "connecting"
  | "observe_mode"
  | "no_control"
  | "replaying"
  | "pending_size"
  | "unknown_geometry"
  | "keyboard"
  | "alternate_buffer"
  | "missing_measurement"
  | "already_fit";

export type UnifiedGeometryAvailability = Readonly<{
  enabled: boolean;
  code: UnifiedGeometryAvailabilityCode;
  message: string;
}>;

export type UnifiedGeometryAvailabilityInput = Readonly<{
  action: UnifiedGeometryAction;
  closed: boolean;
  prepared: boolean;
  committed: boolean;
  controlGranted: boolean;
  replaying: boolean;
  capabilityMode: "observe" | "control";
  fitPending: boolean;
  refitPending: boolean;
  committedColumns: number;
  committedRows: number;
  keyboardGuard: boolean;
  bufferType: "normal" | "alternate";
  measurement?: number;
}>;

export type GeometryFormView = Readonly<{
  root: HTMLElement;
  columns: HTMLInputElement;
  rows: HTMLInputElement;
  apply: HTMLButtonElement;
  fitRows: HTMLButtonElement;
  fitWidth: HTMLButtonElement;
  reason: HTMLOutputElement;
}>;
