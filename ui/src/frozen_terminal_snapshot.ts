export type FrozenTerminalCellStyle = Readonly<{
  foreground?: string;
  background?: string;
  bold?: true;
  dim?: true;
  italic?: true;
  underline?: true;
  inverse?: true;
  strikethrough?: true;
  invisible?: true;
  blink?: true;
  overline?: true;
}>;
export type FrozenTerminalSegment = Readonly<{ text: string; style: FrozenTerminalCellStyle }>;
export type FrozenTerminalColumnRange = Readonly<{ start: number; end: number }>;
export type FrozenTerminalRow = Readonly<{
  text: string;
  /** Same UTF-16 shape as `text`, with concealed glyphs replaced by spaces. */
  copyText?: string;
  isWrapped: boolean;
  /** Run-length encoded renderer presentation; text concatenates to `text`. */
  segments?: readonly FrozenTerminalSegment[];
  /** UTF-16 text offset at each captured terminal column. */
  columnOffsets?: readonly number[];
  /** Exact captured grapheme range for every terminal column. */
  columnRanges?: readonly FrozenTerminalColumnRange[];
}>;
export type FrozenTerminalSnapshot = Readonly<{
  bufferType: "normal" | "alternate";
  columns: number;
  rows: number;
  baseY: number;
  viewportY: number;
  fontFamily: string;
  fontSizePixels: number;
  rowHeightPixels: number;
  lines: readonly FrozenTerminalRow[];
}>;
export type FrozenTerminalPoint = Readonly<{ row: number; offset: number }>;

export function frozenRangeAtColumn(row: FrozenTerminalRow, column: number): FrozenTerminalColumnRange | undefined {
  if (!Number.isSafeInteger(column) || column < 0) return undefined;
  return row.columnRanges?.[column];
}

const normalized = (value: string): string => value.replace(/\u00a0/g, " ");
const compare = (a: FrozenTerminalPoint, b: FrozenTerminalPoint): number => a.row - b.row || a.offset - b.offset;

export function serializeFrozenRange(
  snapshot: FrozenTerminalSnapshot,
  first: FrozenTerminalPoint,
  second: FrozenTerminalPoint,
): string {
  const [start, end] = compare(first, second) <= 0 ? [first, second] : [second, first];
  if (start.row < 0 || end.row >= snapshot.lines.length) return "";
  let result = "";
  for (let rowIndex = start.row; rowIndex <= end.row; rowIndex += 1) {
    const row = snapshot.lines[rowIndex]!;
    const from = rowIndex === start.row ? Math.max(0, start.offset) : 0;
    const to = rowIndex === end.row ? Math.max(from, end.offset) : row.text.length;
    const source = row.copyText ?? row.text;
    const text = normalized(source.slice(from, to)).replace(/[ \t]+$/u, "");
    if (rowIndex !== start.row && !row.isWrapped) result += "\n";
    result += text;
  }
  return result;
}
export function serializeFrozenScreen(snapshot: FrozenTerminalSnapshot): string {
  if (snapshot.lines.length === 0) return "";
  const start = Math.max(0, Math.min(snapshot.viewportY, snapshot.lines.length - 1));
  const end = Math.max(start, Math.min(snapshot.lines.length, start + snapshot.rows));
  if (end === start) return "";
  return serializeFrozenRange(snapshot, { row: start, offset: 0 }, {
    row: end - 1,
    offset: snapshot.lines[end - 1]!.text.length,
  });
}
