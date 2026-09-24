// Close-reason policy for the unified terminal page.
//
// Every transport loss reaches the page as a canonicalized lowercase reason
// code: server-side refusals travel in the WebSocket close payload
// (frontdoor writeWSCloseReason / the broker error codes it forwards), the
// transport's own client-side faults use its literal reason strings, and a
// bare network drop arrives as `websocket_<code>` because the close frame
// carries no reason. This module classifies that full enumeration:
//
// - TERMINAL: a typed refusal of THIS attachment. Retrying replays the same
//   refusal, so the page stops the reconnect loop and renders the reason with
//   a way out. Control-lease policy reasons (lease_held, control_displaced,
//   takeover_superseded) are also terminal — the transport already refuses to
//   retry them — and are additionally claimable via control takeover.
// - TRANSIENT: network-layer or infrastructure loss where a fresh attempt can
//   genuinely succeed (the broker restarting, a dropped connection, a failed
//   reconnect attempt). The transport keeps its bounded auto-retry; the page
//   renders a visible reconnecting state, and retry exhaustion arrives as a
//   ReconnectStatus EXHAUSTED, which the page renders terminally.
// - REATTACH: a broker close that is NOT a permanent verdict on this
//   session. Retrying with a fresh handle on the SAME session identity
//   genuinely recovers it, so the page keeps the transport's bounded
//   re-attach (source re-mint, then identity re-mint) and renders a
//   reconnecting strip. A burst (the page's own limiter) is the only thing
//   that degrades it to a terminal notice, so a genuinely stuck session cannot
//   loop forever. Two kinds of member:
//   * `input_refused`: the broker emits it for a frame that arrived a beat
//     before the control grant or inside a resize cut, and `continue`s;
//     retrying does NOT replay it. A current front door relays it in-band
//     (unified_refusal_notice) and never closes on it; the member is kept for
//     a front door that still closes, so the page cannot dead-end.
//   * View-ending subscriber close reasons (internal/proto
//     SubscriberCloseReason): `subscriber_lagged` evicts a view that fell
//     behind the committed stream, `generation_rotated` replaces a pressure-
//     rotated generation, and `generation_refit` replaces one after an
//     explicit capture-authoritative width refit. The session and control mode
//     remain intact, so re-attach rebuilds the terminal from snapshot+tail.
//     Those two stay out of UNIFIED_TERMINAL_REASONS. The broader subscriber
//     protocol enumeration also includes `generation_failed` and
//     `refit_faulted`: that exact successor was made ineligible, so each is terminal and stays out of
//     UNIFIED_REATTACH_REASONS, and renders explicit retry/dashboard actions.
// - INTERNAL: closes the page inflicted on itself (navigation away, an
//   explicit socket replacement). Nothing to render.
export type UnifiedCloseClass = "terminal" | "transient" | "reattach" | "internal";

// Inventory details are presentation-only facts. Keep one closed vocabulary
// for the dashboard parser and every mounted terminal consumer so a new broker
// detail cannot silently become an open session with unreviewed UI behavior.
export const UNIFIED_INVENTORY_DETAILS = Object.freeze(["rotation_deferred_alt_screen"] as const);
export type UnifiedInventoryDetail = typeof UNIFIED_INVENTORY_DETAILS[number];

export function unifiedInventoryDetail(value: string): UnifiedInventoryDetail | undefined {
  return UNIFIED_INVENTORY_DETAILS.find((candidate) => candidate === value);
}

// Claimable control-policy reasons; mirrors the transport's takeover policy
// set. Terminal AND eligible for the take-control affordance in control mode.
export const UNIFIED_TAKEOVER_REASONS: ReadonlySet<string> = new Set([
  "lease_held", "control_displaced", "takeover_superseded",
]);

// The broker's typed subscriber close reasons, mirrored from
// internal/proto/attachment_errors.go (SubscriberCloseReason). The
// source-to-policy enumeration test reads that Go set and requires every
// member to be classified here: adding a value there without adding it here
// fails the suite.
export const UNIFIED_SUBSCRIBER_CLOSE_REASONS: ReadonlySet<string> = new Set([
  "generation_rotated",
  "generation_refit",
  "subscriber_lagged",
  "generation_failed",
  "refit_faulted",
]);

// Broker closes that recover by re-attaching on the same session identity.
// The page holds the transport's bounded auto-retry for these and only its own
// burst limiter promotes them to terminal.
export const UNIFIED_REATTACH_REASONS: ReadonlySet<string> = new Set([
  "input_refused",
  "generation_rotated",
  "generation_refit",
  "subscriber_lagged",
]);

// The one exported terminal classification. The transport, the page, and the
// source-to-policy enumeration test all consume THIS set; nothing else may
// keep a private copy of it, because separate copies can omit close reasons.
export const UNIFIED_TERMINAL_REASONS: ReadonlySet<string> = new Set([
  ...UNIFIED_TAKEOVER_REASONS,
  // Broker-typed refusals forwarded verbatim by the front door. Every error
  // control the broker emits on an attachment writer is a deterministic
  // typed refusal: retrying replays it.
  "unified_unavailable", "stale_target", "attach_failed", "bad_mode", "bad_history", "protocol",
  "bad_control", "bad_frame", "history_failed",
  "resize_failed", "resize_rejected", "snapshot_failed",
  "generation_failed",
  "refit_faulted",
  // Front-door-typed refusals of this attachment, including the attachment
  // protocol violations the front closes deterministically: malformed
  // liveness (bad_liveness), a malformed attachment frame (bad_attachment),
  // control traffic on an observe handle (observe_mode), and a non-text
  // WebSocket message (websocket_message_type).
  "lease_unavailable", "lease_lost", "broker_protocol", "browser_liveness", "stale_snapshot",
  "bad_liveness", "bad_attachment", "observe_mode", "websocket_message_type",
  // The client-side liveness engine's own protocol judgement: the server
  // answered liveness with an invalid pong. A deterministic peer protocol
  // violation, not infrastructure loss.
  "liveness_protocol",
  // The transport's judgement of an in-band refusal frame whose code it could
  // not read: the same class of peer protocol violation.
  "refusal_protocol",
  // canonicalFailureCode's fallback for an unrepresentable code.
  "attachment_failed",
]);

export const UNIFIED_INTERNAL_REASONS: ReadonlySet<string> = new Set([
  "page_hidden", "bfcache_restored", "destroyed", "client_disconnect",
  "takeover_requested", "session_switch", "attach_again_superseded", "page_closed", "detached",
]);

export function classifyUnifiedClose(reason: string): UnifiedCloseClass {
  if (UNIFIED_REATTACH_REASONS.has(reason)) return "reattach";
  if (UNIFIED_TERMINAL_REASONS.has(reason)) return "terminal";
  if (UNIFIED_INTERNAL_REASONS.has(reason)) return "internal";
  // Everything else — websocket_<code>, transport_error, transport_send_*,
  // liveness timeouts/unavailability, reconnect_attempt_*,
  // broker_unavailable, broker_deadline, malformed_frame, non_text_frame —
  // is a transient transport-layer loss. This fallback is fenced by the
  // source-to-policy enumeration test: every reason the front, broker, or
  // client transport actually emits must be classified explicitly, so a NEW
  // emitted code fails the suite instead of silently inheriting transient.
  return "transient";
}

const BOUNDED_REASON = /^[a-z][a-z0-9_]{0,63}$/;

// Reason codes are shown next to the notice; anything outside the canonical
// shape collapses to a safe placeholder so a hostile close payload cannot put
// arbitrary text on the page.
export function boundedUnifiedReason(reason: string): string {
  return BOUNDED_REASON.test(reason) ? reason : "attachment_failed";
}

export type UnifiedCloseNotice = Readonly<{ headline: string; detail: string }>;

// An operator needs a sentence and a way out; the code stays visible alongside
// so a report remains diagnosable. Anything unlisted degrades to a generic
// headline with the code intact — never to a blank page.
const NOTICES: Readonly<Record<string, UnifiedCloseNotice>> = Object.freeze({
  lease_held: Object.freeze({ headline: "Already controlled elsewhere", detail: "This session is being controlled in another window or on another device." }),
  control_displaced: Object.freeze({ headline: "Control moved elsewhere", detail: "Another window took control of this session." }),
  takeover_superseded: Object.freeze({ headline: "A newer takeover won", detail: "This request did not become the controller." }),
  unified_unavailable: Object.freeze({ headline: "Unified terminal unavailable", detail: "This session cannot be opened through the unified terminal right now. The dashboard shows what is possible." }),
  stale_target: Object.freeze({ headline: "This session moved on", detail: "The session changed or ended while attaching. Pick it again from the dashboard." }),
  attach_failed: Object.freeze({ headline: "The session could not be attached", detail: "The session host refused this attachment. Try again from the dashboard." }),
  lease_unavailable: Object.freeze({ headline: "Control could not be taken", detail: "This session could not be locked for typing. Try again from the dashboard." }),
  lease_lost: Object.freeze({ headline: "Control moved elsewhere", detail: "Another window took control of this session." }),
  browser_liveness: Object.freeze({ headline: "This page fell behind", detail: "The session host stopped trusting this page's liveness. Reopen it from the dashboard." }),
  bad_liveness: Object.freeze({ headline: "The connection broke protocol", detail: "This page sent a liveness message the session host could not read. Reopen the session from the dashboard." }),
  bad_attachment: Object.freeze({ headline: "The connection broke protocol", detail: "This page sent a frame the session host could not read. Reopen the session from the dashboard." }),
  observe_mode: Object.freeze({ headline: "This view is read-only", detail: "Typing and resizing are not available while observing. Reopen the session in control mode to interact." }),
  websocket_message_type: Object.freeze({ headline: "The connection broke protocol", detail: "A non-text message reached the session host and was refused. Reopen the session from the dashboard." }),
  liveness_protocol: Object.freeze({ headline: "The connection broke protocol", detail: "The session host answered liveness with an invalid message. Reopen the session from the dashboard." }),
  refusal_protocol: Object.freeze({ headline: "The connection broke protocol", detail: "The session host sent a refusal this page could not read. Reopen the session from the dashboard." }),
  reconnect_exhausted: Object.freeze({ headline: "Connection lost", detail: "The connection could not be re-established." }),
  // The transport's retry-exhaustion reasons all mean the same thing to an
  // operator: automatic attempts stopped. A trusted Reconnect starts a new
  // finite cycle with fresh authority; no disconnected input is replayed.
  retry_budget_exhausted: Object.freeze({ headline: "Connection lost", detail: "Automatic reconnect stopped. When your connection returns, select Reconnect to try again." }),
  reconnect_unavailable: Object.freeze({ headline: "Connection lost", detail: "The connection could not be re-established." }),
  source_binding_unavailable: Object.freeze({ headline: "Connection lost", detail: "This session's identity expired while reconnecting. Reopen it from the dashboard." }),
  // Genuinely unresolvable identities, surfaced by the transport's identity
  // re-mint path rather than by a broker close frame.
  session_gone: Object.freeze({ headline: "This session has ended", detail: "The session is no longer running. Open another one from the dashboard." }),
  identity_ambiguous: Object.freeze({ headline: "This session is ambiguous", detail: "More than one session now matches this tab's identity. Pick the one you want from the dashboard." }),
  identity_invalid: Object.freeze({ headline: "This link is incomplete", detail: "This tab is missing the identity needed to reopen its session. Open it again from the dashboard." }),
  // The reattach burst limiter's terminal outcome: input kept being refused.
  input_refused: Object.freeze({ headline: "Input kept being refused", detail: "The session repeatedly refused input and could not be recovered here. Reopen it from the dashboard." }),
  // The reattach burst limiter's terminal outcome for a subscriber close: the
  // page kept falling behind the session's output faster than it could be
  // rebuilt. The session is fine; this page is not keeping up.
  subscriber_lagged: Object.freeze({ headline: "This page kept falling behind", detail: "The session produced output faster than this page could receive it, repeatedly. The session is still running; reopen it from the dashboard." }),
  generation_rotated: Object.freeze({ headline: "Refreshing terminal history", detail: "The terminal journal advanced to a new generation. This page is reconnecting from the authoritative snapshot." }),
  generation_refit: Object.freeze({ headline: "Refitting terminal width", detail: "The terminal width changed and this page is reconnecting from the authoritative post-width snapshot." }),
  generation_failed: Object.freeze({ headline: "Terminal history stopped", detail: "The new terminal journal could not be made durable. Reopen this session from the dashboard to start a fresh unified attachment." }),
  refit_faulted: Object.freeze({ headline: "Terminal refit stopped", detail: "The session width changed, but a durable successor view could not be established. Reopen this session from the dashboard." }),
  // The broker's verdict on the attachment itself — including a Fit whose
  // transaction failed after the session host had already resized. The
  // session is fine; this attachment is not, and reopening re-establishes it
  // at whatever geometry the host now holds.
  attachment_failed: Object.freeze({ headline: "This attachment ended", detail: "The session host ended this attachment. The session is still running; reopen it from the dashboard." }),
});

export function unifiedCloseNotice(reason: string): UnifiedCloseNotice {
  return NOTICES[reason] ?? Object.freeze({ headline: "This terminal is unavailable", detail: "The connection ended and cannot be resumed here. Reopen the session from the dashboard." });
}
