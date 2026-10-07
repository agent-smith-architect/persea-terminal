// Input results: what became of the INPUT frames this page sent.
//
// Both ends number the INPUT frames of one socket 1, 2, … in send order. The
// server reports their outcomes in that order, coalesced into runs, as a
// reserved text frame (Go authority: internal/attachmentwire/transport_input.go):
// "PERSEA-INPUT/1 9" — every frame through the ninth was written to the
// terminal in full; "PERSEA-INPUT/1 9 input_paused" — refused with that code.
// A frame with no result when its socket ends is uncertain: it may or may not
// have reached the terminal. Nothing is ever resent.
export const TRANSPORT_INPUT_PREFIX = "PERSEA-INPUT/1 ";

export type InputResultCode = "" | "input_paused" | "input_refused" | "input_dropped" | "input_partial";

const CODES: ReadonlySet<string> = new Set(["input_paused", "input_refused", "input_dropped", "input_partial"]);

export type ServerInputFrame =
  | Readonly<{ type: "ORDINARY" }>
  | Readonly<{ type: "INPUT"; through: number; code: InputResultCode }>
  | Readonly<{ type: "VIOLATION" }>;

export function decodeServerInputFrame(payload: string): ServerInputFrame {
  if (!payload.startsWith(TRANSPORT_INPUT_PREFIX)) return Object.freeze({ type: "ORDINARY" });
  const match = /^([1-9][0-9]{0,15})(?: ([a-z_]+))?$/.exec(payload.slice(TRANSPORT_INPUT_PREFIX.length));
  const through = match ? Number(match[1]) : 0;
  const code = match?.[2] ?? "";
  if (!match || !Number.isSafeInteger(through) || (code !== "" && !CODES.has(code))) return Object.freeze({ type: "VIOLATION" });
  return Object.freeze({ type: "INPUT", through, code: code as InputResultCode });
}

// What the sender of one input is told: it reached the terminal; nothing of
// it did (so sending it again cannot double it); only part of it did; or the
// connection ended before anyone could say.
export type InputDelivery = "written" | "not_written" | "partial" | "uncertain";

export function inputDelivery(code: InputResultCode): InputDelivery {
  if (code === "") return "written";
  return code === "input_partial" ? "partial" : "not_written";
}

export const INPUT_UNCERTAIN_NOTICE = "The connection dropped before the terminal confirmed your last input — check that it arrived";
