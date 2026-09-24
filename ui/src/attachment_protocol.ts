import type { ReconciliationDeferReason } from "./continuous_surface/types";
import { HISTORY_CHOICES, type HistoryChoice } from "./dashboard";

export type UInt64 = bigint;
export type Mode = "OBSERVE" | "CONTROL";
export type CutKind = "INITIAL" | "RECONNECT" | "ONGOING" | "RESIZE" | "HISTORY";
export type Base = Readonly<{ version: 1; source: string; epoch: UInt64 }>;
type PrepareBase = Base & Readonly<{
  type: "PREPARE";
  cut: UInt64;
  columns: number;
  rows: number;
  history: readonly string[];
  truncated: boolean;
  replay: Uint8Array;
}>;
export type Prepare =
  | PrepareBase & Readonly<{ kind: Exclude<CutKind, "HISTORY"> }>
  | PrepareBase & Readonly<{ kind: "HISTORY"; request: UInt64; effectiveHistoryRows: HistoryChoice }>;
export type Ready = Base & Readonly<{ type: "READY"; cut: UInt64 }>;
export type Defer = Base & Readonly<{ type: "DEFER"; cut: UInt64; reason: ReconciliationDeferReason }>;
export type Commit = Base & Readonly<{ type: "COMMIT"; cut: UInt64 }>;
export type Live = Base & Readonly<{ type: "LIVE"; cut: UInt64; data: Uint8Array }>;
export type ModeRequest = Base & Readonly<{ type: "MODE_REQUEST"; mode: Mode }>;
export type ModeFrame = Base & Readonly<{ type: "MODE"; mode: Mode; reason?: string }>;
export type Input = Base & Readonly<{ type: "INPUT"; data: Uint8Array }>;
export type ResizeRequest = Base & Readonly<{ type: "RESIZE_REQUEST"; columns: number; rows: number }>;
export type HistoryRequest = Base & Readonly<{ type: "HISTORY_REQUEST"; request: UInt64; historyRows: HistoryChoice }>;
export type End = Base & Readonly<{ type: "END"; reason: string }>;
export type DecodedAttachmentFrame = Prepare | Ready | Defer | Commit | Live | ModeRequest | ModeFrame | Input | ResizeRequest | HistoryRequest | End;
export type ServerFrame = Prepare | Commit | Live | ModeFrame | End;
export type BrowserFrame = Ready | Defer | ModeRequest | Input | ResizeRequest | HistoryRequest;
export type FrameDirection = "server" | "browser" | "either";

export const MAX_UINT64 = 0xffff_ffff_ffff_ffffn;
export const MAX_SOURCE_UTF8_BYTES = 256;
export const MAX_HISTORY_ROWS = 10_000;
export const MAX_HISTORY_ROW_UTF8_BYTES = 8_192;
export const MAX_HISTORY_UTF8_BYTES = 2_097_152;
export const MAX_REPLAY_BYTES = 262_144;
export const MAX_LIVE_BYTES = 524_288;
export const MAX_INPUT_BYTES = 262_144;
export const MAX_REASON_UTF8_BYTES = 64;
export const MAX_GEOMETRY_CELLS = 1_000;

// Explicit vertical Fit policy. Narrower than the protocol's generic geometry
// bound on purpose: the protocol bound says what a frame may carry, this says
// what an operator may ask for. The broker re-derives and rechecks all of it
// against the exact pane witness, so this is a courtesy to the operator, never
// the authority.
export const MIN_FIT_ROWS = 8;
export const MAX_FIT_ROWS = 120;
export const MAX_FIT_CELLS = 36_000;

export function validVerticalFit(columns: number, rows: number): boolean {
  return Number.isSafeInteger(columns) && Number.isSafeInteger(rows) &&
    columns >= 1 && columns <= MAX_GEOMETRY_CELLS &&
    rows >= MIN_FIT_ROWS && rows <= MAX_FIT_ROWS &&
    columns * rows <= MAX_FIT_CELLS;
}
export const MAX_INBOX_FRAMES = 64;
export const MAX_INBOX_WEIGHT = MAX_HISTORY_UTF8_BYTES + MAX_REPLAY_BYTES + MAX_SOURCE_UTF8_BYTES + 64;
export const MAX_EGRESS_FRAMES = 64;
export const MAX_EGRESS_BYTES = 524_288;
export const MAX_INPUT_FRAMES = 64;
export const MAX_INPUT_QUEUE_BYTES = 262_144;

const encoder = new TextEncoder();
const SERVER_TAGS = new Set(["PREPARE", "COMMIT", "LIVE", "MODE", "END"]);
const BROWSER_TAGS = new Set(["READY", "DEFER", "MODE_REQUEST", "INPUT", "RESIZE_REQUEST", "HISTORY_REQUEST"]);
const DEFER_REASONS = new Set<ReconciliationDeferReason>([
  "ACTIVE_CONTACT_OR_SCROLL_GESTURE",
  "INTERSECTING_DOM_SELECTION",
  "AFFECTED_XTERM_SELECTION",
  "VISIBLE_ANCHOR_WOULD_BE_EVICTED",
]);

export class FrameValidationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "FrameValidationError";
  }
}

function fail(message: string): never {
  throw new FrameValidationError(message);
}

function utf8(value: string): number {
  return encoder.encode(value).byteLength;
}

function record(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) fail("frame must be an ordinary record");
  if (Object.getPrototypeOf(value) !== Object.prototype) fail("frame prototype is not ordinary");
  const descriptors = Object.getOwnPropertyDescriptors(value);
  for (const [key, descriptor] of Object.entries(descriptors)) {
    if (!("value" in descriptor) || descriptor.get || descriptor.set) fail(`frame field ${key} is not a data property`);
  }
  return value as Record<string, unknown>;
}

function exactKeys(value: Record<string, unknown>, required: readonly string[], optional: readonly string[] = []): void {
  const allowed = new Set([...required, ...optional]);
  const keys = Object.keys(value);
  for (const key of required) if (!Object.hasOwn(value, key) || value[key] === undefined) fail(`missing frame field ${key}`);
  for (const key of keys) if (!allowed.has(key) || value[key] === undefined) fail(`unknown or undefined frame field ${key}`);
}

function stringBound(value: unknown, name: string, minimum: number, maximum: number): string {
  if (typeof value !== "string") fail(`${name} must be a string`);
  const length = utf8(value);
  if (length < minimum || length > maximum) fail(`${name} UTF-8 length is out of bounds`);
  return value;
}

function uint64(value: unknown, name: string): UInt64 {
  if (typeof value !== "bigint" || value < 1n || value > MAX_UINT64) fail(`${name} must be uint64 bigint`);
  return value;
}

function geometry(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0 || value > MAX_GEOMETRY_CELLS) fail(`${
    name
  } must be an integer from 1 through ${MAX_GEOMETRY_CELLS}`);
  return value;
}

function bytes(value: unknown, name: string, minimum: number, maximum: number): Uint8Array {
  if (!(value instanceof Uint8Array)) fail(`${name} must be Uint8Array`);
  if (value.byteLength < minimum || value.byteLength > maximum) fail(`${name} length is out of bounds`);
  return new Uint8Array(value);
}

function base(value: Record<string, unknown>): Base {
  if (value.version !== 1) fail("version must be numeric literal 1");
  return Object.freeze({
    version: 1,
    source: stringBound(value.source, "source", 1, MAX_SOURCE_UTF8_BYTES),
    epoch: uint64(value.epoch, "epoch"),
  });
}

function mode(value: unknown): Mode {
  if (value !== "OBSERVE" && value !== "CONTROL") fail("mode is invalid");
  return value;
}

function cutKind(value: unknown): CutKind {
  if (value !== "INITIAL" && value !== "RECONNECT" && value !== "ONGOING" && value !== "RESIZE" && value !== "HISTORY") fail("cut kind is invalid");
  return value;
}

function historyRows(value: unknown): HistoryChoice {
  if (typeof value !== "number" || !HISTORY_CHOICES.some((choice) => choice === value)) fail("history_rows is not an allowed history depth");
  return value as HistoryChoice;
}

function history(value: unknown): readonly string[] {
  if (!Array.isArray(value) || value.length > MAX_HISTORY_ROWS) fail("history row count is out of bounds");
  let total = 0;
  const copy: string[] = [];
  for (const row of value) {
    if (typeof row !== "string" || row.includes("\0")) fail("history row is invalid");
    const length = utf8(row);
    if (length > MAX_HISTORY_ROW_UTF8_BYTES) fail("history row UTF-8 length is out of bounds");
    total += length + 1;
    if (total > MAX_HISTORY_UTF8_BYTES) fail("history total UTF-8 length is out of bounds");
    copy.push(row);
  }
  return Object.freeze(copy);
}

export function validateDecodedAttachmentFrame(value: unknown, direction: FrameDirection = "either"): DecodedAttachmentFrame {
  const source = record(value);
  if (typeof source.type !== "string") fail("frame type is missing");
  if (![...SERVER_TAGS, ...BROWSER_TAGS].includes(source.type)) fail("frame tag is unknown");
  if (direction === "server" && !SERVER_TAGS.has(source.type)) fail("browser frame received from server");
  if (direction === "browser" && !BROWSER_TAGS.has(source.type)) fail("server frame received from browser");
  const common = ["type", "version", "source", "epoch"];
  const b = base(source);
  switch (source.type) {
    case "PREPARE": {
      const kind = cutKind(source.kind);
      const historyFields = kind === "HISTORY" ? ["request", "effectiveHistoryRows"] : [];
      exactKeys(source, [...common, "cut", "kind", "columns", "rows", "history", "truncated", "replay", ...historyFields]);
      if (typeof source.truncated !== "boolean") fail("truncated must be boolean");
      const commonPrepare = { ...b, type: "PREPARE" as const, cut: uint64(source.cut, "cut"), columns: geometry(source.columns, "columns"), rows: geometry(source.rows, "rows"), history: history(source.history), truncated: source.truncated, replay: bytes(source.replay, "replay", 0, MAX_REPLAY_BYTES) };
      return kind === "HISTORY"
        ? Object.freeze({ ...commonPrepare, kind, request: uint64(source.request, "request"), effectiveHistoryRows: historyRows(source.effectiveHistoryRows) })
        : Object.freeze({ ...commonPrepare, kind });
    }
    case "READY":
      exactKeys(source, [...common, "cut"]);
      return Object.freeze({ ...b, type: "READY", cut: uint64(source.cut, "cut") });
    case "DEFER": {
      exactKeys(source, [...common, "cut", "reason"]);
      const reason = stringBound(source.reason, "reason", 1, MAX_REASON_UTF8_BYTES) as ReconciliationDeferReason;
      if (!DEFER_REASONS.has(reason)) fail("DEFER reason is invalid");
      return Object.freeze({ ...b, type: "DEFER", cut: uint64(source.cut, "cut"), reason });
    }
    case "COMMIT":
      exactKeys(source, [...common, "cut"]);
      return Object.freeze({ ...b, type: "COMMIT", cut: uint64(source.cut, "cut") });
    case "LIVE":
      exactKeys(source, [...common, "cut", "data"]);
      return Object.freeze({ ...b, type: "LIVE", cut: uint64(source.cut, "cut"), data: bytes(source.data, "data", 1, MAX_LIVE_BYTES) });
    case "MODE_REQUEST":
      exactKeys(source, [...common, "mode"]);
      return Object.freeze({ ...b, type: "MODE_REQUEST", mode: mode(source.mode) });
    case "MODE": {
      exactKeys(source, [...common, "mode"], ["reason"]);
      const result: { version: 1; source: string; epoch: UInt64; type: "MODE"; mode: Mode; reason?: string } = { ...b, type: "MODE", mode: mode(source.mode) };
      if (Object.hasOwn(source, "reason")) result.reason = stringBound(source.reason, "reason", 0, MAX_REASON_UTF8_BYTES);
      return Object.freeze(result);
    }
    case "INPUT":
      exactKeys(source, [...common, "data"]);
      return Object.freeze({ ...b, type: "INPUT", data: bytes(source.data, "data", 1, MAX_INPUT_BYTES) });
    case "RESIZE_REQUEST":
      exactKeys(source, [...common, "columns", "rows"]);
      return Object.freeze({ ...b, type: "RESIZE_REQUEST", columns: geometry(source.columns, "columns"), rows: geometry(source.rows, "rows") });
    case "HISTORY_REQUEST":
      exactKeys(source, [...common, "request", "historyRows"]);
      return Object.freeze({ ...b, type: "HISTORY_REQUEST", request: uint64(source.request, "request"), historyRows: historyRows(source.historyRows) });
    case "END":
      exactKeys(source, [...common, "reason"]);
      return Object.freeze({ ...b, type: "END", reason: stringBound(source.reason, "reason", 1, MAX_REASON_UTF8_BYTES) });
    default:
      return fail("unreachable frame tag");
  }
}

export function validateServerFrame(value: unknown): ServerFrame {
  return validateDecodedAttachmentFrame(value, "server") as ServerFrame;
}

export function cloneBrowserFrame(frame: BrowserFrame): BrowserFrame {
  return validateDecodedAttachmentFrame(frame, "browser") as BrowserFrame;
}

export function frameWeight(frame: DecodedAttachmentFrame): number {
  let weight = 64 + utf8(frame.source);
  if (frame.type === "PREPARE") weight += frame.history.reduce((sum, row) => sum + utf8(row) + 1, 0) + frame.replay.byteLength;
  if (frame.type === "LIVE" || frame.type === "INPUT") weight += frame.data.byteLength;
  if (frame.type === "DEFER" || frame.type === "END") weight += utf8(frame.reason);
  if (frame.type === "MODE" && frame.reason !== undefined) weight += utf8(frame.reason);
  return weight;
}

export function outboundByteWeight(frame: BrowserFrame): number {
  return frame.type === "INPUT" ? frame.data.byteLength : 0;
}

export function sameTuple(a: Pick<Base, "source" | "epoch">, b: Pick<Base, "source" | "epoch">): boolean {
  return a.source === b.source && a.epoch === b.epoch;
}

export function sameCutTuple(a: Pick<Base, "source" | "epoch"> & { cut: UInt64 }, b: Pick<Base, "source" | "epoch"> & { cut: UInt64 }): boolean {
  return sameTuple(a, b) && a.cut === b.cut;
}

export function prepareFingerprint(frame: Prepare): string {
  const hex = (bytesValue: Uint8Array) => Array.from(bytesValue, (value) => value.toString(16).padStart(2, "0")).join("");
  return JSON.stringify([
    frame.version, frame.source, frame.epoch.toString(), frame.cut.toString(), frame.kind,
    frame.kind === "HISTORY" ? frame.request.toString() : null,
    frame.kind === "HISTORY" ? frame.effectiveHistoryRows : null,
    frame.columns, frame.rows, frame.history, frame.truncated, hex(frame.replay),
  ]);
}
