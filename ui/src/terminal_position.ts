// Where a terminal grid sits when it is smaller than the space the page gives
// it. On an axis where the grid is as large as its space or larger there is no
// free space to distribute, so every choice starts the grid at that axis's
// start edge and its scroll range reaches every cell.
export const TERMINAL_POSITIONS = Object.freeze(["top-center", "top-left", "center"] as const);

export type TerminalPosition = typeof TERMINAL_POSITIONS[number];
export const DEFAULT_TERMINAL_POSITION: TerminalPosition = "top-center";

export const TERMINAL_POSITION_LABELS: Readonly<Record<TerminalPosition, string>> = Object.freeze({
  "top-center": "Top center",
  "top-left": "Top left",
  center: "Center",
});

export function isTerminalPosition(value: unknown): value is TerminalPosition {
  return typeof value === "string" && (TERMINAL_POSITIONS as readonly string[]).includes(value);
}

// Only Center moves a short grid down. The horizontal axis is the stylesheet's,
// keyed by the same value on the terminal shell.
export function centersVertically(position: TerminalPosition): boolean {
  return position === "center";
}
