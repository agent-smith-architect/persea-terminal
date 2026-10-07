// Operational refusals for the unified terminal page.
//
// A broker `error` whose code is OPERATIONAL is the outcome of ONE request —
// a Fit that did not apply, a Fit outside policy, control traffic on an
// observe handle. The session, its journal and the attachment are all still
// valid, so the front door relays the code in-band as a reserved text frame
// instead of closing the socket, and the page renders it as a passing notice:
// no reconnect, no dead end.
//
// Refused input is reported by its input result instead (input_results.ts),
// with the codes below; it shares these notices. input_paused (typing while
// this page is far behind the session) is also a state on the page: typing
// stays paused until the operator presses Resume typing (see
// refusalPausesTyping).
//
// The operational set here mirrors the Go authority (internal/proto
// attachment_errors.go); the source-to-policy test holds the two equal.
export const TRANSPORT_REFUSAL_PREFIX = "PERSEA-REFUSAL/1 ";

export const UNIFIED_OPERATIONAL_CODES: ReadonlySet<string> = new Set([
  "resize_failed", "resize_rejected", "observe_mode",
]);

// How long a refusal stays on screen. Long enough to read on a phone, short
// enough never to be mistaken for a state.
export const REFUSAL_NOTICE_MS = 4_000;

const codePattern = /^[a-z][a-z0-9_]{0,63}$/;

export type ServerRefusalFrame =
  | Readonly<{ type: "ORDINARY" }>
  | Readonly<{ type: "REFUSAL"; code: string }>
  | Readonly<{ type: "VIOLATION" }>;

export function decodeServerRefusalFrame(payload: string): ServerRefusalFrame {
  if (!payload.startsWith(TRANSPORT_REFUSAL_PREFIX)) return Object.freeze({ type: "ORDINARY" });
  const code = payload.slice(TRANSPORT_REFUSAL_PREFIX.length);
  if (!codePattern.test(code)) return Object.freeze({ type: "VIOLATION" });
  return Object.freeze({ type: "REFUSAL", code });
}

// The Fit seal is held while a RESIZE_REQUEST is outstanding; a refusal of
// that request is the answer, so the seal opens again.
export function refusalReleasesFit(code: string): boolean {
  return code === "resize_failed" || code === "resize_rejected";
}

// A keystroke refused because the page is far behind means the operator's
// command lost its beginning. Waiting is not a command boundary, so the page
// stops sending until the operator marks one by pressing Resume typing; every
// other code stays a passing notice.
export function refusalPausesTyping(code: string): boolean {
  return code === "input_paused";
}

const NOTICES: Readonly<Record<string, string>> = Object.freeze({
  resize_failed: "Fit didn't apply",
  resize_rejected: "Fit was refused",
  input_refused: "Input was refused — try again",
  observe_mode: "This view is read-only",
  input_paused: "Catching up — what you typed was not sent",
  input_dropped: "Input was not sent — control changed",
  input_partial: "Only part of the input reached the terminal",
});

// One sentence with the code beside it, so a report stays diagnosable. A
// code outside the canonical shape is never put on the page.
export function unifiedRefusalNotice(code: string): string {
  const bounded = codePattern.test(code) ? code : "refused";
  return `${NOTICES[bounded] ?? "Request refused"} (${bounded})`;
}
