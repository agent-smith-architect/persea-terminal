import { UNIFIED_INTERNAL_REASONS, UNIFIED_REATTACH_REASONS, UNIFIED_SUBSCRIBER_CLOSE_REASONS, UNIFIED_TAKEOVER_REASONS, UNIFIED_TERMINAL_REASONS, boundedUnifiedReason, classifyUnifiedClose, unifiedCloseNotice } from "../src/unified_close_policy";

// The enumeration fence reads real sources; the project has no node type
// declarations, so the two touched surfaces are typed locally.
declare const require: (name: string) => unknown;
const fs = require("node:fs") as { readdirSync(directory: string): string[]; readFileSync(file: string, encoding: "utf8"): string };
const path = require("node:path") as { join(...parts: string[]): string };
const nodeProcess = require("node:process") as { cwd(): string };

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
};

// TERMINAL: typed refusals of this attachment — the page must stop the retry
// loop and render the reason. ALL
// broker/front-door-typed refusals are terminal — including the attachment
// protocol violations the front closes deterministically: bad_liveness,
// bad_attachment, observe_mode, websocket_message_type, and the client-side
// liveness protocol judgement liveness_protocol.
for (const reason of [
  "lease_held", "control_displaced", "takeover_superseded",
  "unified_unavailable", "stale_target", "attach_failed", "bad_mode", "bad_history", "protocol",
  "bad_control", "bad_frame", "history_failed",
  "resize_failed", "resize_rejected", "snapshot_failed",
  "lease_unavailable", "lease_lost", "broker_protocol", "browser_liveness", "stale_snapshot",
  "bad_liveness", "bad_attachment", "observe_mode", "websocket_message_type",
  "liveness_protocol", "refusal_protocol",
  "attachment_failed",
]) {
  assert.equal(classifyUnifiedClose(reason), "terminal", `broker-typed refusal must be terminal: ${reason}`);
}

// REATTACH: input_refused is not a permanent verdict — the broker treats it as
// non-fatal and re-attaching on the same identity recovers it. The page holds
// the transport's bounded auto-retry and only its own burst limiter promotes it
// to terminal.
for (const reason of ["input_refused"]) {
  assert.equal(classifyUnifiedClose(reason), "reattach", `re-attachable refusal must classify reattach: ${reason}`);
}
for (const reason of UNIFIED_REATTACH_REASONS) assert.equal(classifyUnifiedClose(reason), "reattach");

// Rotation and lag close only this attachment view and are reconnectable.
for (const reason of ["generation_rotated", "generation_refit", "subscriber_lagged"]) {
  assert.ok(UNIFIED_SUBSCRIBER_CLOSE_REASONS.has(reason), `subscriber close reason must be enumerated: ${reason}`);
  assert.equal(classifyUnifiedClose(reason), "reattach", `subscriber close must be reconnectable: ${reason}`);
  assert.ok(!UNIFIED_TERMINAL_REASONS.has(reason), `subscriber close must never be terminal: ${reason}`);
}
assert.ok(UNIFIED_SUBSCRIBER_CLOSE_REASONS.has("generation_failed"));
assert.equal(classifyUnifiedClose("generation_failed"), "terminal");
assert.ok(!UNIFIED_REATTACH_REASONS.has("generation_failed"));
assert.ok(UNIFIED_SUBSCRIBER_CLOSE_REASONS.has("refit_faulted"));
assert.equal(classifyUnifiedClose("refit_faulted"), "terminal");
assert.ok(!UNIFIED_REATTACH_REASONS.has("refit_faulted"));

// TRANSIENT: network-layer or infrastructure loss — the transport keeps its
// bounded auto-retry and the page renders a visible reconnecting state.
for (const reason of [
  "websocket_1006", "websocket_1001", "transport_error", "transport_send_unavailable",
  "transport_send_failed", "liveness_unavailable", "liveness_timeout",
  "reconnect_attempt_failed", "reconnect_attempt_timeout",
  "broker_unavailable", "broker_deadline", "malformed_frame", "non_text_frame",
]) {
  assert.equal(classifyUnifiedClose(reason), "transient", `network-layer loss must stay retryable: ${reason}`);
}

// INTERNAL: page-inflicted closes render nothing.
for (const reason of [
  "page_hidden", "bfcache_restored", "destroyed", "client_disconnect",
  "takeover_requested", "session_switch", "attach_again_superseded", "page_closed", "detached",
]) {
  assert.equal(classifyUnifiedClose(reason), "internal", `self-inflicted close must render nothing: ${reason}`);
}

assert.ok(UNIFIED_INTERNAL_REASONS.has("session_switch"), "session_switch belongs to the one internal close enumeration");
assert.ok(!UNIFIED_REATTACH_REASONS.has("session_switch"), "session_switch is never a broker reattach reason");
assert.ok(!UNIFIED_TERMINAL_REASONS.has("session_switch"), "session_switch is never a terminal verdict");

// The exported sets ARE the classification the function applies — one shared
// classification, no private copies.
for (const reason of UNIFIED_TERMINAL_REASONS) assert.equal(classifyUnifiedClose(reason), "terminal");
for (const reason of UNIFIED_INTERNAL_REASONS) assert.equal(classifyUnifiedClose(reason), "internal");

// --- source-to-policy enumeration ---------------------------------------
// Every close reason the front door, the broker, or the client transport can
// actually emit must be classified EXPLICITLY below. A NEW emitted code fails
// this test instead of silently inheriting the transient fallback: decide its
// class, add it to unified_close_policy if terminal/internal, and record it
// here.
{
  const uiRoot = nodeProcess.cwd(); // npm test runs from ui/
  const readGoPackage = (name: string): string => {
    const directory = path.join(uiRoot, "..", "internal", name);
    const sources = fs.readdirSync(directory)
      .filter((entry: string) => entry.endsWith(".go") && !entry.endsWith("_test.go"))
      .map((entry: string) => fs.readFileSync(path.join(directory, entry), "utf8"));
    assert.ok(sources.length > 0, `no Go sources found under internal/${name}`);
    return sources.join("\n");
  };
  const goFront = readGoPackage("frontdoor");
  const goBroker = readGoPackage("broker");
  const uiTransport = fs.readFileSync(path.join(uiRoot, "src", "websocket_attachment_transport.ts"), "utf8");
  const uiLiveness = fs.readFileSync(path.join(uiRoot, "src", "transport_liveness.ts"), "utf8");

  const CANONICAL = /^[a-z][a-z0-9_]{0,63}$/;
  const emitted = new Set<string>();
  const collect = (source: string, pattern: RegExp, canonicalize: boolean): number => {
    let count = 0;
    for (const match of source.matchAll(pattern)) {
      const code = match[1];
      // logTerminalFailure codes pass through canonicalFailureCode before
      // they can appear in a close frame; mirror that exactly.
      emitted.add(CANONICAL.test(code) ? code : (canonicalize ? "attachment_failed" : code));
      count += 1;
    }
    return count;
  };

  // Front-door closes: logTerminalFailure feeds writeWSCloseReason.
  assert.ok(collect(goFront, /logTerminalFailure\(\s*"([^"]*)"/g, true) >= 15, "front-door close-reason extraction found too few sites");
  // Control displacement closes carry their literal reason string.
  assert.ok(collect(goFront, /\breason :?= "([a-z0-9_]+)"/g, false) >= 2, "displacement reason extraction found too few sites");
  // Broker typed refusals are forwarded verbatim as the close reason.
  assert.ok(collect(goBroker, /Type: "error", Code: "([a-z0-9_]+)"/g, false) >= 10, "broker refusal-code extraction found too few sites");
  // Subscriber close reasons are written as error codes from the typed
  // constants in internal/proto, not as literals at the broker call site:
  // read the closed set itself, so a value rotation adds there is fenced.
  const goProto = readGoPackage("proto");
  assert.ok(collect(goProto, /SubscriberCloseReason = "([a-z0-9_]+)"/g, false) >= 1, "subscriber close-reason extraction found too few members");
  for (const match of goProto.matchAll(/SubscriberCloseReason = "([a-z0-9_]+)"/g)) {
    assert.ok(UNIFIED_SUBSCRIBER_CLOSE_REASONS.has(match[1]), `broker subscriber close reason "${match[1]}" is not enumerated in UNIFIED_SUBSCRIBER_CLOSE_REASONS`);
  }
  for (const reason of UNIFIED_SUBSCRIBER_CLOSE_REASONS) {
    assert.ok(goProto.includes(`SubscriberCloseReason = "${reason}"`), `UNIFIED_SUBSCRIBER_CLOSE_REASONS names "${reason}", which the broker does not define`);
  }
  // Client transport self-closes.
  assert.ok(collect(uiTransport, /closeCurrent\([^)"]*"([a-z0-9_]+)"/g, false) >= 3, "transport close-reason extraction found too few sites");
  assert.ok(collect(uiTransport, /notifyClosed\([^,)]+,\s*"([a-z0-9_]+)"\)/g, false) >= 1, "transport notify-reason extraction found too few sites");
  const replacementReasons = uiTransport.match(/export type EndpointReplacementReason = ([^;]+);/);
  assert.ok(replacementReasons, "endpoint replacement reason declaration is missing");
  assert.ok(collect(replacementReasons![1], /"([a-z0-9_]+)"/g, false) === 2, "endpoint replacement reason extraction must find takeover and session switch");
  assert.ok(collect(uiTransport, /(?:detach|disconnect)\(reason = "([a-z0-9_]+)"\)/g, false) >= 2, "transport default-reason extraction found too few sites");
  assert.ok(collect(uiTransport, /disconnect\("([a-z0-9_]+)"\)/g, false) >= 1, "transport destroy-reason extraction found too few sites");
  // Client liveness engine faults.
  assert.ok(collect(uiLiveness, /\.fail\("([a-z0-9_]+)"\)/g, false) >= 5, "liveness fault extraction found too few sites");

  // Extraction health: known members of each vocabulary must be present, or
  // a refactor has silently broken the fence.
  for (const sentinel of [
    "bad_liveness", "bad_attachment", "observe_mode", "websocket_message_type",
    "control_displaced", "takeover_superseded",
    "unified_unavailable", "bad_history", "generation_rotated", "generation_refit", "subscriber_lagged", "generation_failed", "refit_faulted",
    "liveness_protocol", "malformed_frame", "takeover_requested", "destroyed",
  ]) {
    assert.ok(emitted.has(sentinel), `extraction lost a known emitted reason: ${sentinel}`);
  }

  const EXPLICIT: Readonly<Record<string, "terminal" | "transient" | "reattach" | "internal">> = Object.freeze({
    // Front-door typed refusals of this attachment.
    bad_liveness: "terminal", bad_attachment: "terminal", observe_mode: "terminal",
    websocket_message_type: "terminal", broker_protocol: "terminal",
    browser_liveness: "terminal", stale_snapshot: "terminal",
    lease_held: "terminal", lease_lost: "terminal", lease_unavailable: "terminal",
    control_displaced: "terminal", takeover_superseded: "terminal",
    attachment_failed: "terminal",
    // Front-door infrastructure loss: a fresh attempt can genuinely succeed.
    broker_read: "transient", broker_write: "transient",
    broker_unavailable: "transient", broker_deadline: "transient",
    source_binding: "transient", upgrade_failed: "transient",
    websocket_read: "transient", websocket_write: "transient",
    websocket_deadline: "transient",
    // Broker typed refusals forwarded verbatim.
    attach_failed: "terminal", bad_control: "terminal", bad_frame: "terminal",
    bad_history: "terminal", bad_mode: "terminal", history_failed: "terminal",
    input_refused: "reattach", protocol: "terminal", resize_failed: "terminal",
    resize_rejected: "terminal", snapshot_failed: "terminal",
    stale_target: "terminal", unified_unavailable: "terminal",
    // Broker typed subscriber closes (internal/proto SubscriberCloseReason):
    // this attachment's view of the journal ended; the session did not.
    generation_rotated: "reattach", generation_refit: "reattach", subscriber_lagged: "reattach", generation_failed: "terminal", refit_faulted: "terminal",
    // Client transport self-closes.
    liveness_protocol: "terminal", refusal_protocol: "terminal",
    malformed_frame: "transient", non_text_frame: "transient",
    transport_error: "transient", transport_send_failed: "transient",
    transport_send_unavailable: "transient",
    reconnect_attempt_failed: "transient", reconnect_attempt_timeout: "transient",
    page_closed: "internal", takeover_requested: "internal", session_switch: "internal",
    attach_again_superseded: "internal",
    detached: "internal", client_disconnect: "internal", destroyed: "internal",
    // Client liveness engine faults: waiting/timer/send loss is retryable.
    liveness_timeout: "transient", liveness_send_unavailable: "transient",
    liveness_send_failed: "transient", liveness_clock_anomaly: "transient",
    liveness_timer_unavailable: "transient", liveness_unavailable: "transient",
  });
  for (const code of [...emitted].sort()) {
    const expected = EXPLICIT[code];
    assert.ok(
      expected !== undefined,
      `NEW emitted close code "${code}" is not explicitly classified — decide terminal/transient/internal in unified_close_policy.ts and record it in this table`,
    );
    assert.equal(classifyUnifiedClose(code), expected, `emitted code ${code} classified wrong`);
  }
}

// A future unknown server code arrives as a transient (the retry budget bounds
// it and exhaustion renders terminally), never as a silent unknown.
assert.equal(classifyUnifiedClose("some_future_code"), "transient");

// The claimable set is exactly the transport's takeover policy set.
assert.equal([...UNIFIED_TAKEOVER_REASONS].sort().join(","), "control_displaced,lease_held,takeover_superseded");
for (const reason of UNIFIED_TAKEOVER_REASONS) {
  assert.equal(classifyUnifiedClose(reason), "terminal", `claimable reason must be terminal: ${reason}`);
}

// Reason codes outside the canonical shape collapse before display.
assert.equal(boundedUnifiedReason("lease_held"), "lease_held");
assert.equal(boundedUnifiedReason("<img src=x onerror=alert(1)>"), "attachment_failed");
assert.equal(boundedUnifiedReason(""), "attachment_failed");
assert.equal(boundedUnifiedReason("UPPER_CASE"), "attachment_failed");

// Every notice has words; an unknown code degrades to a generic headline, and
// the mapped codes carry their reviewed copy.
for (const reason of [
  "lease_held", "unified_unavailable", "stale_target", "attach_failed", "reconnect_exhausted", "never_seen_before",
  "bad_liveness", "bad_attachment", "observe_mode", "websocket_message_type", "liveness_protocol",
]) {
  const notice = unifiedCloseNotice(reason);
  assert.ok(notice.headline.length > 0 && notice.detail.length > 0, `notice for ${reason} must have visible text`);
}
// The protocol violations carry specific copy, not just the generic degradation.
assert.equal(unifiedCloseNotice("observe_mode").headline, "This view is read-only");
assert.equal(unifiedCloseNotice("bad_liveness").headline, "The connection broke protocol");
assert.equal(unifiedCloseNotice("unified_unavailable").headline, "Unified terminal unavailable");
assert.equal(unifiedCloseNotice("never_seen_before").headline, "This terminal is unavailable");

// Reopen-flow terminal outcomes carry reviewed copy: the identity re-mint dead
// ends and the reattach burst limiter's own terminal notice.
// attachment_failed is the broker's verdict on the attachment — including a
// Fit whose transaction failed after the host already resized — and names the
// way back (reopen) rather than degrading to the generic headline.
for (const reason of ["session_gone", "identity_ambiguous", "identity_invalid", "input_refused", "attachment_failed"]) {
  const notice = unifiedCloseNotice(reason);
  assert.ok(notice.headline.length > 0 && notice.detail.length > 0, `notice for ${reason} must have visible text`);
  assert.ok(notice.headline !== "This terminal is unavailable", `${reason} must carry reviewed copy, not the generic degradation`);
}
assert.equal(unifiedCloseNotice("generation_rotated").headline, "Refreshing terminal history");
assert.equal(unifiedCloseNotice("generation_failed").headline, "Terminal history stopped");

console.log("unified close policy tests PASS");
