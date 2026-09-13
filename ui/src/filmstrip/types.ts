import type { ITerminalOptions, Terminal } from "@xterm/xterm";

/**
 * A fixed-width physical-row source. The controller is deliberately unable to
 * accept a raw array from callers; this seam is the only ingress for replica
 * history.
 */
export interface ReplicaSource {
  readonly width: number;
  readonly rowCount: number;
  rows(start: number, count: number): readonly string[];
}

export interface CellDimensions {
  readonly width: number;
  readonly height: number;
}

export interface FilmstripOptions {
  /** Element whose complete area is the composite's scrolling viewport. */
  readonly host: HTMLElement;
  /** Already-opened host for the caller-owned live terminal. */
  readonly liveHost: HTMLElement;
  /** Use this in deterministic tests; production derives dimensions publicly. */
  readonly cellDimensions?: CellDimensions;
  /** Defaults to the injected terminal's rows plus the controller's overscan. */
  readonly windowRows?: number;
  /** Defaults to the retained source length, capped at 5,000 rows. */
  readonly historyCapacity?: number;
  /** Test seam for creating the module-owned renderer. */
  readonly createHistoryTerminal?: (options: ITerminalOptions) => Terminal;
}

export interface FilmstripModelState {
  readonly historyRows: number;
  readonly historyCapacity: number;
  readonly droppedRows: number;
  readonly windowRows: number;
  readonly renderedWindowStart: number;
  readonly cellDimensions: CellDimensions;
}

export interface RotationResult {
  readonly appended: number;
  readonly dropped: number;
}
