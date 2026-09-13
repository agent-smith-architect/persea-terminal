import {
  MAX_UINT64,
  cloneBrowserFrame,
  validateServerFrame,
  type BrowserFrame,
  type DecodedAttachmentFrame,
  type ServerFrame,
} from "./attachment_protocol";

export const MAX_ATTACHMENT_WIRE_BYTES = 16_777_216;

const encoder = new TextEncoder();

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
  if (frame.type === "PREPARE") value.replay = encodeBase64(frame.replay);
  if (frame.type === "PREPARE" && frame.kind === "HISTORY") {
    delete value.effectiveHistoryRows;
    value.effective_history_rows = frame.effectiveHistoryRows;
  }
  if (frame.type === "HISTORY_REQUEST") {
    delete value.historyRows;
    value.history_rows = frame.historyRows;
  }
  if (frame.type === "LIVE" || frame.type === "INPUT") value.data = encodeBase64(frame.data);
  return value;
}

function encode(frame: DecodedAttachmentFrame): string {
  const payload = JSON.stringify(toWire(frame));
  if (encoder.encode(payload).byteLength > MAX_ATTACHMENT_WIRE_BYTES) fail("encoded frame exceeds the wire bound");
  return payload;
}

function rejectDuplicateTopLevelFields(payload: string): void {
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
    }
  }
}

function decode(payload: string): Record<string, unknown> {
  if (encoder.encode(payload).byteLength > MAX_ATTACHMENT_WIRE_BYTES) fail("encoded frame exceeds the wire bound");
  let parsed: unknown;
  try { parsed = JSON.parse(payload); } catch { fail("payload is not valid JSON"); }
  rejectDuplicateTopLevelFields(payload);
  const source = record(parsed);
  const value: Record<string, unknown> = { ...source, epoch: uint64(source.epoch, "epoch") };
  if (Object.hasOwn(source, "cut")) value.cut = uint64(source.cut, "cut");
  if (Object.hasOwn(source, "request")) value.request = uint64(source.request, "request");
  if (Object.hasOwn(source, "replay")) value.replay = decodeBase64(source.replay, "replay");
  if (Object.hasOwn(source, "data")) value.data = decodeBase64(source.data, "data");
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

export function encodeServerFrame(frame: ServerFrame): string {
  return encode(validateServerFrame(frame));
}

export function decodeBrowserFrame(payload: string): BrowserFrame {
  return cloneBrowserFrame(decode(payload) as BrowserFrame);
}

export function decodeServerFrame(payload: string): ServerFrame {
  return validateServerFrame(decode(payload));
}
