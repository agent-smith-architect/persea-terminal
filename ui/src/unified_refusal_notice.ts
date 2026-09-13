// Operational refusals for the unified terminal page.
//
// A broker `error` whose code is OPERATIONAL is the outcome of ONE request —
// a Fit that did not apply, a Fit outside policy, a keystroke that landed a
// beat early, control traffic on an observe handle. The session, its journal
// and the attachment are all still valid, so the front door relays the code
// in-band as a reserved text frame instead of closing the socket, and the
// page renders it as a passing notice: no reconnect, no dead end.
//
// The operational set here mirrors the Go authority (internal/proto
// attachment_errors.go); the source-to-policy test holds the two equal.
export const TRANSPORT_REFUSAL_PREFIX = "PERSEA-REFUSAL/1 ";

export const UNIFIED_OPERATIONAL_CODES: ReadonlySet<string> = new Set([
  "resize_failed", "resize_rejected", "input_refused", "observe_mode",
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

const NOTICES: Readonly<Record<string, string>> = Object.freeze({
  resize_failed: "Fit didn't apply",
  resize_rejected: "Fit was refused",
  input_refused: "Input was refused — try again",
  observe_mode: "This view is read-only",
});

// One sentence with the code beside it, so a report stays diagnosable. A
// code outside the canonical shape is never put on the page.
export function unifiedRefusalNotice(code: string): string {
  const bounded = codePattern.test(code) ? code : "refused";
  return `${NOTICES[bounded] ?? "Request refused"} (${bounded})`;
}
