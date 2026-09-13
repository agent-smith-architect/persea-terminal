import { frozenRangeAtColumn, serializeFrozenRange, serializeFrozenScreen, type FrozenTerminalSnapshot } from "../src/frozen_terminal_snapshot";

const equal = (actual: unknown, expected: unknown, message: string): void => {
  if (actual !== expected) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
};
const snapshot: FrozenTerminalSnapshot = Object.freeze({
  bufferType: "normal",
  columns: 8,
  rows: 3,
  baseY: 3,
  viewportY: 2,
  fontFamily: "fixture-mono",
  fontSizePixels: 15,
  rowHeightPixels: 18,
  lines: Object.freeze([
    Object.freeze({ text: "wrapped ", isWrapped: false }),
    Object.freeze({ text: "ASCII  ", isWrapped: true }),
    Object.freeze({
      text: "界e\u0301  ",
      copyText: "界e\u0301  ",
      isWrapped: false,
      columnRanges: Object.freeze([
        Object.freeze({ start: 0, end: 1 }),
        Object.freeze({ start: 0, end: 1 }),
        Object.freeze({ start: 1, end: 3 }),
        Object.freeze({ start: 3, end: 4 }),
        Object.freeze({ start: 4, end: 5 }),
      ]),
    }),
    Object.freeze({ text: "\u00a0   ", isWrapped: false }),
    Object.freeze({ text: "screen  ", isWrapped: false }),
  ]),
});

equal(serializeFrozenRange(snapshot, { row: 0, offset: 0 }, { row: 2, offset: 5 }), "wrappedASCII\n界é", "soft wraps and hard boundaries");
equal(serializeFrozenRange(snapshot, { row: 2, offset: 5 }, { row: 0, offset: 0 }), "wrappedASCII\n界é", "reversed endpoints");
equal(serializeFrozenRange(snapshot, { row: 2, offset: 0 }, { row: 4, offset: 8 }), "界é\n\nscreen", "CJK, combining and blank rows");
equal(serializeFrozenScreen(snapshot), "界é\n\nscreen", "entry viewport screen");

const alternate: FrozenTerminalSnapshot = Object.freeze({ ...snapshot, bufferType: "alternate", baseY: 0, viewportY: 0, lines: snapshot.lines.slice(0, 3) });
equal(serializeFrozenScreen(alternate), "wrappedASCII\n界é", "alternate entry grid only");

const concealed: FrozenTerminalSnapshot = Object.freeze({
  ...snapshot,
  rows: 1,
  viewportY: 0,
  lines: Object.freeze([Object.freeze({ text: "secretZ", copyText: "      Z", isWrapped: false })]),
});
equal(serializeFrozenScreen(concealed), "      Z", "concealed presentation must never enter copied text");

const mapped = snapshot.lines[2]!;
equal(JSON.stringify(frozenRangeAtColumn(mapped, 0)), JSON.stringify({ start: 0, end: 1 }), "wide lead range");
equal(JSON.stringify(frozenRangeAtColumn(mapped, 1)), JSON.stringify({ start: 0, end: 1 }), "wide continuation range");
equal(JSON.stringify(frozenRangeAtColumn(mapped, 2)), JSON.stringify({ start: 1, end: 3 }), "combining grapheme range");

const farRight: FrozenTerminalSnapshot = Object.freeze({
  ...snapshot,
  columns: 240,
  rows: 2,
  viewportY: 0,
  lines: Object.freeze([
    Object.freeze({ text: `${"w".repeat(239)}R`, isWrapped: false }),
    Object.freeze({ text: "FINAL👩‍💻", isWrapped: true }),
  ]),
});
equal(serializeFrozenRange(farRight, { row: 0, offset: 238 }, { row: 1, offset: farRight.lines[1]!.text.length }), "wRFINAL👩‍💻", "far-right wrapped final-row serialization");
equal(serializeFrozenScreen(Object.freeze({ ...farRight, bufferType: "alternate" })), `${"w".repeat(239)}RFINAL👩‍💻`, "alternate final row and wrap boundary");
console.log("frozen terminal snapshot serializer: PASS");
