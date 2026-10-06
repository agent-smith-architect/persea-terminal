import {
  MAX_HISTORY_ROW_UTF8_BYTES, MAX_HISTORY_UTF8_BYTES, MAX_LIVE_BYTES, MAX_REPLAY_BYTES, MAX_UINT64,
  type ServerFrame,
} from "../src/attachment_protocol";
import {
  MAX_ATTACHMENT_HEADER_BYTES, MAX_ATTACHMENT_WIRE_BYTES, decodeServerFrame, encodeServerFrame,
} from "../src/attachment_wire";

const encoder = new TextEncoder();
function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}
function rejects(payload: unknown, name: string): void {
  let failed = false;
  try { decodeServerFrame(payload); } catch { failed = true; }
  assert(failed, `binary decoder accepted ${name}`);
}
export function binaryTestFrame(header: string, bytes = new Uint8Array()): ArrayBuffer {
  const json = encoder.encode(header);
  const result = new ArrayBuffer(4 + json.byteLength + bytes.byteLength);
  new DataView(result).setUint32(0, json.byteLength, false);
  new Uint8Array(result, 4, json.byteLength).set(json);
  new Uint8Array(result, 4 + json.byteLength).set(bytes);
  return result;
}
export function binaryHeader(frame: ArrayBuffer): string {
  return new TextDecoder().decode(new Uint8Array(frame, 4, new DataView(frame).getUint32(0, false)));
}
function sameBytes(actual: Uint8Array, expected: Uint8Array): boolean {
  return actual.byteLength === expected.byteLength && actual.every((byte, index) => byte === expected[index]);
}

for (const type of ["LIVE", "PREPARE"] as const) {
  const maximum = type === "LIVE" ? MAX_LIVE_BYTES : MAX_REPLAY_BYTES;
  for (const size of [1, 2, 3, 256, 1023, maximum]) {
    const bytes = Uint8Array.from({ length: size }, (_, index) => (index * 179 + 255) & 255);
    const base = { version: 1 as const, source: "界".repeat(85), epoch: MAX_UINT64, cut: MAX_UINT64 };
    const frame: ServerFrame = type === "LIVE" ? { ...base, type, data: bytes } : {
      ...base, type, kind: "HISTORY", request: MAX_UINT64, effectiveHistoryRows: 10_000,
      columns: 1000, rows: 1000, history: ["é\x1b[0m", "界"], truncated: true, replay: bytes,
    };
    const encoded = encodeServerFrame(frame);
    const decoded = decodeServerFrame(encoded);
    assert(decoded.type === type && decoded.source === frame.source && decoded.epoch === MAX_UINT64 && "cut" in decoded && decoded.cut === MAX_UINT64, "binary tuple changed");
    assert(decoded.type === "LIVE" ? sameBytes(decoded.data, bytes) : decoded.type === "PREPARE" && sameBytes(decoded.replay, bytes), "arbitrary binary bytes changed");
    assert(sameBytes(new Uint8Array(encoded, 4 + new DataView(encoded).getUint32(0)), bytes), "wire payload was transformed");
    if (decoded.type === "PREPARE") {
      assert(decoded.kind === "HISTORY" && decoded.request === MAX_UINT64 && decoded.effectiveHistoryRows === 10_000 &&
        decoded.columns === 1000 && decoded.rows === 1000 && decoded.truncated &&
        decoded.history.join("|") === "é\x1b[0m|界", "PREPARE metadata changed");
    }
  }
}
for (const bytes of [[255, 192, 0, 128], [27], [91, 51], [49, 109, 226], [130], [172]]) {
  const data = Uint8Array.from(bytes);
  const decoded = decodeServerFrame(encodeServerFrame({ version: 1, type: "LIVE", source: "s", epoch: 1n, cut: 1n, data }));
  assert(decoded.type === "LIVE" && sameBytes(decoded.data, data), "split Unicode or escape sequence changed");
}

const row = "\x01".repeat(MAX_HISTORY_ROW_UTF8_BYTES - 1);
const worst: ServerFrame = {
  version: 1, type: "PREPARE", source: "\x01".repeat(256), epoch: MAX_UINT64, cut: MAX_UINT64,
  kind: "HISTORY", request: MAX_UINT64, effectiveHistoryRows: 10_000, columns: 1000, rows: 1000,
  history: Array.from({ length: Math.floor(MAX_HISTORY_UTF8_BYTES / (row.length + 1)) }, () => row),
  truncated: true, replay: new Uint8Array(MAX_REPLAY_BYTES).fill(255),
};
const worstWire = encodeServerFrame(worst);
const worstDecoded = decodeServerFrame(worstWire);
assert(worstWire.byteLength < MAX_ATTACHMENT_WIRE_BYTES && new DataView(worstWire).getUint32(0) < MAX_ATTACHMENT_HEADER_BYTES, "worst valid PREPARE exceeded bound");
assert(worstDecoded.type === "PREPARE" && worstDecoded.history.join("") === worst.history.join("") && sameBytes(worstDecoded.replay, worst.replay), "maximum PREPARE changed");

// This is the canonical Go encoder's byte layout as well as a browser fixture.
const header = '{"cut":"3","data":2,"epoch":"2","source":"source","type":"LIVE","version":1}';
const valid = binaryTestFrame(header, new Uint8Array([1, 2]));
const golden = decodeServerFrame(valid);
assert(golden.type === "LIVE" && golden.cut === 3n && golden.epoch === 2n && sameBytes(golden.data, new Uint8Array([1, 2])), "Go wire layout disagrees");
const commit = '{"version":1,"type":"COMMIT","source":"source","epoch":"1","cut":"1"}';
const withLength = (length: number) => {
  const result = valid.slice(0);
  new DataView(result).setUint32(0, length, false);
  return result;
};
const malformed: Array<[string, unknown]> = [
  ["text attachment", header], ["Blob", new Blob([valid])], ["typed array", new Uint8Array(valid)],
  ["empty", new ArrayBuffer(0)], ["short prefix", valid.slice(0, 3)],
  ["zero header", withLength(0)], ["truncated header", withLength(valid.byteLength)],
  ["overflow header", withLength(0xffff_ffff)],
  ["oversized header", binaryTestFrame(commit + " ".repeat(MAX_ATTACHMENT_HEADER_BYTES + 1 - commit.length))],
  ["oversized wire", new ArrayBuffer(MAX_ATTACHMENT_WIRE_BYTES + 1)],
  ["trailing JSON", binaryTestFrame(header + "{}", new Uint8Array([1, 2]))],
  ["truncated JSON", binaryTestFrame(header.slice(0, -1), new Uint8Array([1, 2]))],
  ["truncated payload", valid.slice(0, -1)],
  ["trailing payload", binaryTestFrame(header, new Uint8Array([1, 2, 3]))],
  ["body on COMMIT", binaryTestFrame(commit, new Uint8Array([1]))],
  ["wrong direction", binaryTestFrame(commit.replace("COMMIT", "READY"))],
  ["oversized LIVE", binaryTestFrame(header.replace('"data":2', `"data":${MAX_LIVE_BYTES + 1}`), new Uint8Array(MAX_LIVE_BYTES + 1))],
  ["oversized body", binaryTestFrame(header, new Uint8Array(Math.max(MAX_REPLAY_BYTES, MAX_LIVE_BYTES) + 1))],
];
const invalidUTF8 = valid.slice(0);
new Uint8Array(invalidUTF8)[4 + header.indexOf('"source":"source"') + '"source":"'.length] = 255;
malformed.push(["invalid header UTF-8", invalidUTF8]);
for (const [before, after] of [
  ['"version":1', '"version":2'], ['"type":"LIVE"', '"type":"UNKNOWN"'],
  ['"source":"source"', '"source":null'], ['"epoch":"2"', '"epoch":2'],
  ['"epoch":"2"', '"epoch":"02"'], ['"epoch":"2"', '"epoch":"0"'],
  ['"epoch":"2"', '"epoch":"18446744073709551616"'], ['"cut":"3",', ""],
  ['"cut":"3"', '"cut":"3","cut":"4"'], ['"data":2', '"data":"2"'],
  ['"data":2', '"data":null'], ['"data":2', '"data":-1'], ['"data":2', '"data":1.5'],
  ['"data":2', '"data":2.0'], ['"data":2', '"data":2e0'], ['"data":2', '"data":2.00000000000000001'],
  ['"data":2', '"data":02'], ['"version":1', '"version":1.0'],
  ['"data":2', '"data":2,"extra":true'],
]) malformed.push([after, binaryTestFrame(header.replace(before, after), new Uint8Array([1, 2]))]);
for (const [name, frame] of malformed) rejects(frame, name);
// The header bound is inclusive: a complete header of exactly the bound decodes.
assert(decodeServerFrame(binaryTestFrame(commit + " ".repeat(MAX_ATTACHMENT_HEADER_BYTES - commit.length))).type === "COMMIT", "a header of exactly the bound was rejected");
const prepareHeader = '{"version":1,"type":"PREPARE","source":"s","epoch":"1","cut":"1","kind":"HISTORY","columns":80,"rows":24,"history":[],"truncated":false,"replay":0,"request":"1","effective_history_rows":1000}';
assert(decodeServerFrame(binaryTestFrame(prepareHeader)).type === "PREPARE", "empty replay rejected");
for (const [before, after] of [
  ['"columns":80', '"columns":0'], ['"rows":24', '"rows":1001'],
  ['"history":[]', '"history":null'], ['"history":[]', '"history":[null]'],
  ['"truncated":false', '"truncated":null'], ['"replay":0', '"replay":null'],
  ['"replay":0', '"replay":1'], ['"replay":0', '"replay":1e-999'], ['"replay":0', '"replay":-0.0'],
  ['"request":"1"', '"request":"0"'],
  ['"effective_history_rows":1000', '"effective_history_rows":999'],
  ['"effective_history_rows":1000', '"effectiveHistoryRows":1000'],
]) rejects(binaryTestFrame(prepareHeader.replace(before, after)), after);

// Resume fields: a stream position round-trips on PREPARE and LIVE (zero
// included); stream and resumed only on an admission; anything else fails.
{
  const stream = "ABCDEFGHIJKLMNOPQRSTUVWXYZ";
  const base = { version: 1 as const, source: "s", epoch: 1n, cut: 2n };
  const admission: ServerFrame = { ...base, type: "PREPARE", kind: "RECONNECT", columns: 80, rows: 24, history: [], truncated: false, replay: new Uint8Array(), stream, resumed: true, position: { seq: 0, offset: 0 } };
  const live: ServerFrame = { ...base, type: "LIVE", data: Uint8Array.of(65), position: { seq: 9, offset: 2 ** 40 } };
  for (const frame of [admission, live]) {
    const decoded = decodeServerFrame(encodeServerFrame(frame));
    assert(JSON.stringify(decoded.type === "PREPARE" || decoded.type === "LIVE" ? decoded.position : null) === JSON.stringify(frame.type === "PREPARE" || frame.type === "LIVE" ? frame.position : null), `${frame.type} position did not round-trip`);
    if (decoded.type === "PREPARE") assert(decoded.stream === stream && decoded.resumed === true, "admission stream did not round-trip");
  }
  const liveHeader = binaryHeader(encodeServerFrame(live));
  const liveBytes = Uint8Array.of(65);
  for (const [name, header] of [
    ["seq alone", liveHeader.replace(/,"offset":"[0-9]+"/, "")],
    ["padded seq", liveHeader.replace('"seq":"9"', '"seq":"09"')],
    ["numeric offset", liveHeader.replace(/"offset":"([0-9]+)"/, '"offset":$1')],
    ["unsafe offset", liveHeader.replace(/"offset":"[0-9]+"/, '"offset":"9007199254740993"')],
    ["stream on LIVE", liveHeader.replace('"seq"', `"stream":"${stream}","seq"`)],
    ["object position", liveHeader.replace(/"seq":"9","offset":"[0-9]+"/, '"position":{"seq":9,"offset":1}')],
  ] as const) {
    assert(header !== liveHeader, `${name}: edit did not apply`);
    rejects(binaryTestFrame(header, liveBytes), name);
  }
  const { stream: _stream, resumed: _resumed, ...plain } = admission as Extract<ServerFrame, { type: "PREPARE" }>;
  const resize = binaryHeader(encodeServerFrame({ ...plain, kind: "RESIZE" }));
  rejects(binaryTestFrame(resize.replace('"seq"', `"stream":"${stream}","seq"`)), "stream on a resize");
  rejects(binaryTestFrame(binaryHeader(encodeServerFrame({ ...plain, stream })).replace('"seq"', '"resumed":false,"seq"')), "resumed false");
  rejects(binaryTestFrame(binaryHeader(encodeServerFrame(plain)).replace('"seq"', '"resumed":true,"seq"')), "resumed without stream");
}
console.log("PASS binary attachment codec: arbitrary bytes, maximum PREPARE, Go layout, malformed envelopes and fields, resume positions");
