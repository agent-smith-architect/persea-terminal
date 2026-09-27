import {
  MAX_UINT64,
  MAX_HISTORY_UTF8_BYTES,
  MAX_LIVE_BYTES,
  MAX_REPLAY_BYTES,
  cloneBrowserFrame,
  validateServerFrame,
  type BrowserFrame,
  type DecodedAttachmentFrame,
  type ServerFrame,
} from "./attachment_protocol";

export const MAX_ATTACHMENT_WIRE_BYTES = 16_777_216;
export const MAX_ATTACHMENT_HEADER_BYTES = 6 * MAX_HISTORY_UTF8_BYTES + 4096;

const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });

function fail(message: string): never { throw new Error(`attachment wire: ${message}`); }

function record(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) fail("frame must be a JSON object");
  return value as Record<string, unknown>;
}

function uint64(value: unknown, name: string): bigint {
  if (typeof value !== "string" || !/^[1-9][0-9]{0,19}$/.test(value)) fail(`${name} must be a canonical positive decimal uint64 string`);
  const parsed = BigInt(value);
  if (parsed > MAX_UINT64 || parsed.toString(10) !== value) fail(`${name} is outside uint64`);
  return parsed;
}

function encodeBase64(bytes: Uint8Array): string {
  let binary = "";
  for (let offset = 0; offset < bytes.byteLength; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, Math.min(bytes.byteLength, offset + 0x8000)));
  }
  return btoa(binary);
}

function decodeBase64(value: unknown, name: string): Uint8Array {
  if (typeof value !== "string") fail(`${name} must be canonical padded base64`);
  let binary: string;
  try { binary = atob(value); } catch { fail(`${name} must be canonical padded base64`); }
  const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
  if (encodeBase64(bytes) !== value) fail(`${name} must be canonical padded base64`);
  return bytes;
}

function toWire(frame: DecodedAttachmentFrame): Record<string, unknown> {
  const value: Record<string, unknown> = { ...frame, epoch: frame.epoch.toString(10) };
  if ("cut" in frame) value.cut = frame.cut.toString(10);
  if ("request" in frame) value.request = frame.request.toString(10);
  if (frame.type === "PREPARE") value.replay = frame.replay.byteLength;
  if (frame.type === "PREPARE" && frame.kind === "HISTORY") {
    delete value.effectiveHistoryRows;
    value.effective_history_rows = frame.effectiveHistoryRows;
  }
  if (frame.type === "HISTORY_REQUEST") {
    delete value.historyRows;
    value.history_rows = frame.historyRows;
  }
  if (frame.type === "LIVE") value.data = frame.data.byteLength;
  if (frame.type === "INPUT") value.data = encodeBase64(frame.data);
  return value;
}

function encode(frame: DecodedAttachmentFrame): string {
  const payload = JSON.stringify(toWire(frame));
  if (encoder.encode(payload).byteLength > MAX_ATTACHMENT_WIRE_BYTES) fail("encoded frame exceeds the wire bound");
  return payload;
}

// Numbers on the wire are plain integers, as the Go decoder requires. JSON.parse
// would accept 1.0, 1e0 or 1e-999 and turn them into integers, so the spelling
// is checked here, before parsing.
const plainInteger = /^-?(0|[1-9][0-9]*)$/;

function rejectNoncanonicalFields(payload: string): void {
  const keys = new Set<string>();
  let depth = 0;
  let expectingKey = false;
  let inString = false;
  let escaped = false;
  let keyStart = -1;
  let capturingKey = false;
  for (let index = 0; index < payload.length; index += 1) {
    const character = payload[index];
    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (character === "\\") {
        escaped = true;
      } else if (character === '"') {
        inString = false;
        if (capturingKey) {
          const key = JSON.parse(payload.slice(keyStart, index + 1)) as unknown;
          if (typeof key !== "string") fail("object key is not a string");
          if (keys.has(key)) fail(`duplicate field ${key}`);
          keys.add(key);
          if (keys.size > 13) fail("too many wire fields");
          capturingKey = false;
          expectingKey = false;
        }
      }
      continue;
    }
    if (character === '"') {
      inString = true;
      keyStart = index;
      capturingKey = depth === 1 && expectingKey;
    } else if (character === "{" || character === "[") {
      depth += 1;
      if (depth === 1 && character === "{") expectingKey = true;
    } else if (character === "}" || character === "]") {
      depth -= 1;
    } else if (character === "," && depth === 1) {
      expectingKey = true;
    } else if (character === "-" || (character >= "0" && character <= "9")) {
      let end = index + 1;
      while (end < payload.length && "0123456789+-.eE".includes(payload[end])) end += 1;
      if (!plainInteger.test(payload.slice(index, end))) fail("number is not a plain integer");
      index = end - 1;
    }
  }
}

function decode(payload: string, bytes?: Uint8Array): Record<string, unknown> {
  if (bytes === undefined && (payload.length > MAX_ATTACHMENT_WIRE_BYTES || encoder.encode(payload).byteLength > MAX_ATTACHMENT_WIRE_BYTES)) fail("encoded frame exceeds the wire bound");
  rejectNoncanonicalFields(payload);
  let parsed: unknown;
  try { parsed = JSON.parse(payload); } catch { fail("payload is not valid JSON"); }
  const source = record(parsed);
  if (Object.hasOwn(source, "effectiveHistoryRows") || Object.hasOwn(source, "historyRows")) fail("noncanonical history field");
  const value: Record<string, unknown> = { ...source, epoch: uint64(source.epoch, "epoch") };
  if (Object.hasOwn(source, "cut")) value.cut = uint64(source.cut, "cut");
  if (Object.hasOwn(source, "request")) value.request = uint64(source.request, "request");
  if (bytes !== undefined) {
    if (source.type === "LIVE" || source.type === "PREPARE") {
      const field = source.type === "LIVE" ? "data" : "replay";
      const length = source[field];
      const minimum = source.type === "LIVE" ? 1 : 0;
      const maximum = source.type === "LIVE" ? MAX_LIVE_BYTES : MAX_REPLAY_BYTES;
      if (typeof length !== "number" || !Number.isSafeInteger(length) || length < minimum || length > maximum || length !== bytes.byteLength) fail(`invalid ${field} byte length`);
      value[field] = bytes;
    } else if (bytes.byteLength !== 0) fail("unexpected byte payload");
  } else {
    if (Object.hasOwn(source, "data")) value.data = decodeBase64(source.data, "data");
  }
  if (Object.hasOwn(source, "effective_history_rows")) {
    value.effectiveHistoryRows = source.effective_history_rows;
    delete value.effective_history_rows;
  }
  if (Object.hasOwn(source, "history_rows")) {
    value.historyRows = source.history_rows;
    delete value.history_rows;
  }
  return value;
}

export function encodeBrowserFrame(frame: BrowserFrame): string {
  return encode(cloneBrowserFrame(frame));
}

export function encodeServerFrame(frame: ServerFrame): ArrayBuffer {
  const valid = validateServerFrame(frame);
  const header = encoder.encode(JSON.stringify(toWire(valid)));
  const bytes = valid.type === "LIVE" ? valid.data : valid.type === "PREPARE" ? valid.replay : new Uint8Array();
  if (header.byteLength > MAX_ATTACHMENT_HEADER_BYTES || 4 + header.byteLength + bytes.byteLength > MAX_ATTACHMENT_WIRE_BYTES) fail("encoded frame exceeds the wire bound");
  const payload = new ArrayBuffer(4 + header.byteLength + bytes.byteLength);
  new DataView(payload).setUint32(0, header.byteLength, false);
  new Uint8Array(payload, 4, header.byteLength).set(header);
  new Uint8Array(payload, 4 + header.byteLength).set(bytes);
  return payload;
}

export function decodeBrowserFrame(payload: string): BrowserFrame {
  return cloneBrowserFrame(decode(payload) as BrowserFrame);
}

export function decodeServerFrame(payload: unknown): ServerFrame {
  if (!(payload instanceof ArrayBuffer)) fail("server attachment must be binary");
  if (payload.byteLength < 4 || payload.byteLength > MAX_ATTACHMENT_WIRE_BYTES) fail("invalid wire length");
  const headerLength = new DataView(payload).getUint32(0, false);
  if (headerLength === 0 || headerLength > MAX_ATTACHMENT_HEADER_BYTES || headerLength > payload.byteLength - 4) fail("invalid header length");
  const bytes = new Uint8Array(payload, 4 + headerLength);
  if (bytes.byteLength > Math.max(MAX_REPLAY_BYTES, MAX_LIVE_BYTES)) fail("oversized byte payload");
  const header = decoder.decode(new Uint8Array(payload, 4, headerLength));
  return validateServerFrame(decode(header, bytes));
}
