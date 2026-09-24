// M9 W1b — regression test W1B-F13: exhaustive, nonblank per-pane failure policy.
//
// The workspace renders pane state through the SHARED projection type and the
// SHARED close policy (imported, never re-declared). This fence enumerates
// every projection state the dashboard parser admits and every close reason
// the front door, the broker, and the client transport actually emit — the
// same extraction the close-policy fence uses — plus the transport's own
// exhaustion/identity codes, and requires an explicit pane state with visible
// headline, detail, badge, and labelled actions for each. It also pins protocol
// completeness for every broker subscriber-close reason while requiring only
// the explicitly recoverable members to classify through the shared reattach
// set; no workspace source carries a `default => unavailable` escape or a
// private close table.
import { UNIFIED_INTERNAL_REASONS, UNIFIED_INVENTORY_DETAILS, UNIFIED_REATTACH_REASONS, UNIFIED_SUBSCRIBER_CLOSE_REASONS, UNIFIED_TAKEOVER_REASONS, UNIFIED_TERMINAL_REASONS } from "../src/unified_close_policy";
import { PANE_STATE_KINDS, PROJECTION_PANE_STATES, paneStateCopy, paneStateFromClose, paneStateFromProjection, type PaneState } from "../src/workspace_layout";
import { ON_MISSING_VALUES } from "../src/workspace_model";

declare const require: (name: string) => unknown;
const fs = require("node:fs") as { readdirSync(directory: string): string[]; readFileSync(file: string, encoding: "utf8"): string };
const path = require("node:path") as { join(...parts: string[]): string };
const nodeProcess = require("node:process") as { cwd(): string };

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
};

const uiRoot = nodeProcess.cwd();
const readGoPackage = (name: string): string => {
  const directory = path.join(uiRoot, "..", "internal", name);
  const sources = fs.readdirSync(directory).filter((entry: string) => entry.endsWith(".go") && !entry.endsWith("_test.go")).map((entry: string) => fs.readFileSync(path.join(directory, entry), "utf8"));
  assert.ok(sources.length > 0, `no Go sources found under internal/${name}`);
  return sources.join("\n");
};
const CANONICAL = /^[a-z][a-z0-9_]{0,63}$/;
const emitted = new Set<string>();
const collect = (source: string, pattern: RegExp, canonicalize: boolean): void => {
  for (const match of source.matchAll(pattern)) {
    const code = match[1];
    emitted.add(CANONICAL.test(code) ? code : (canonicalize ? "attachment_failed" : code));
  }
};
const goFront = readGoPackage("frontdoor");
const goBroker = readGoPackage("broker");
const goProto = readGoPackage("proto");
const uiTransport = fs.readFileSync(path.join(uiRoot, "src", "websocket_attachment_transport.ts"), "utf8");
const uiLiveness = fs.readFileSync(path.join(uiRoot, "src", "transport_liveness.ts"), "utf8");
collect(goFront, /logTerminalFailure\(\s*"([^"]*)"/g, true);
collect(goFront, /\breason :?= "([a-z0-9_]+)"/g, false);
collect(goBroker, /Type: "error", Code: "([a-z0-9_]+)"/g, false);
collect(goProto, /SubscriberCloseReason = "([a-z0-9_]+)"/g, false);
const brokerDetails = [...goProto.matchAll(/UnifiedSessionDetail[A-Za-z0-9_]+\s*=\s*"([a-z0-9_]+)"/g)].map((match) => match[1]).sort();
assert.equal(JSON.stringify([...UNIFIED_INVENTORY_DETAILS].sort()), JSON.stringify(brokerDetails), "broker inventory-detail vocabulary drifted from the shared UI policy");
// The in-band END reasons (terminal epoch end): the page and the workspace
// route them through the same classifier as a close.
const goTerminal = readGoPackage("terminal");
const endReasonBody = goTerminal.slice(goTerminal.indexOf("func (c terminalCause) endReason() string {"));
collect(endReasonBody.slice(0, endReasonBody.indexOf("\n}\n")), /return "([a-z0-9_]+)"/g, false);
collect(uiTransport, /closeCurrent\([^)"]*"([a-z0-9_]+)"/g, false);
collect(uiTransport, /notifyClosed\([^,)]+,\s*"([a-z0-9_]+)"\)/g, false);
collect(uiTransport, /(?:detach|disconnect)\(reason = "([a-z0-9_]+)"\)/g, false);
collect(uiTransport, /disconnect\("([a-z0-9_]+)"\)/g, false);
collect(uiTransport, /exhaustRetry\("([a-z0-9_]+)"\)/g, false);
collect(uiTransport, /exhaustRetry\(reason = "([a-z0-9_]+)"\)/g, false);
collect(uiTransport, /new ReconnectRefusal\("([a-z0-9_]+)"\)/g, false);
collect(uiTransport, /reason: "([a-z0-9_]+)"/g, false);
collect(uiLiveness, /\.fail\("([a-z0-9_]+)"\)/g, false);
// The controller's identity refusals and the workspace's own attach-time codes.
const uiController = fs.readFileSync(path.join(uiRoot, "src", "unified_pane_controller.ts"), "utf8");
collect(uiController, /new ReconnectRefusal\("([a-z0-9_]+)"\)/g, false);
for (const code of ["rate_limited", "attach_failed", "reconnect_exhausted", "websocket_1006", "websocket_1011"]) emitted.add(code);
for (const set of [UNIFIED_TAKEOVER_REASONS, UNIFIED_TERMINAL_REASONS, UNIFIED_REATTACH_REASONS, UNIFIED_INTERNAL_REASONS, UNIFIED_SUBSCRIBER_CLOSE_REASONS]) for (const reason of set) emitted.add(reason);
for (const sentinel of ["closed", "canceled", "fault", "subscriber_lagged", "lease_held", "control_displaced", "takeover_superseded", "stale_target", "session_gone", "identity_ambiguous", "identity_invalid", "source_binding_unavailable", "retry_budget_exhausted", "input_refused", "page_hidden", "malformed_frame"]) {
  assert.ok(emitted.has(sentinel), `extraction lost a known emitted reason: ${sentinel}`);
}
assert.ok(emitted.size >= 40, `emitted reason extraction is implausibly small: ${emitted.size}`);

const nonblank = (state: PaneState, label: string): void => {
  const copy = paneStateCopy(state);
  assert.ok(copy.badge.trim().length > 0, `${label}: blank badge`);
  assert.ok(copy.headline.trim().length > 0, `${label}: blank headline`);
  assert.ok(copy.detail.trim().length > 0, `${label}: blank detail`);
  for (const affordance of copy.affordances) assert.ok(typeof affordance === "string" && affordance.length > 0, `${label}: unlabelled affordance`);
  if (copy.tone === "failed" || copy.tone === "blocked") assert.ok(copy.affordances.length > 0, `${label}: a ${copy.tone} pane needs a labelled way out`);
};

// Every emitted close reason → an explicit pane state with visible copy.
const closeVerdicts: Record<string, string> = {};
for (const reason of [...emitted].sort()) {
  const state = paneStateFromClose(reason);
  closeVerdicts[reason] = state.kind;
  nonblank(state, `close ${reason}`);
  if (state.kind === "failed" || state.kind === "displaced" || state.kind === "takeover_pending" || state.kind === "reconnecting" || state.kind === "reattaching") {
    assert.ok(paneStateCopy(state).code !== undefined, `close ${reason}: the code must stay visible`);
  }
}
// The shared reattach set renders as a bounded reattach, never a dead end.
for (const reason of UNIFIED_REATTACH_REASONS) assert.equal(paneStateFromClose(reason).kind, "reattaching", `${reason} must render as reattaching`);
assert.equal(paneStateFromClose("subscriber_lagged").kind, "reattaching", "subscriber_lagged must classify as a reattach");
assert.equal(paneStateFromClose("generation_rotated").kind, "reattaching", "generation_rotated must classify as a reattach");
assert.equal(paneStateFromClose("generation_refit").kind, "reattaching", "generation_refit must classify as a reattach");
// Subscriber-close enumeration proves protocol completeness, not that every
// close is recoverable. A failed successor is deliberately terminal.
assert.ok(UNIFIED_SUBSCRIBER_CLOSE_REASONS.has("generation_failed"), "generation_failed must remain in the subscriber-close protocol enumeration");
assert.ok(!UNIFIED_REATTACH_REASONS.has("generation_failed"), "generation_failed must never auto-reattach");
const generationFailedState = paneStateFromClose("generation_failed");
assert.equal(generationFailedState.kind, "failed", "generation_failed must map to a terminal failed workspace state");
const generationFailedCopy = paneStateCopy(generationFailedState);
assert.ok(generationFailedCopy.headline.trim().length > 0, "generation_failed needs a visible headline");
assert.ok(generationFailedCopy.detail.trim().length > 0, "generation_failed needs visible detail");
assert.equal(generationFailedCopy.code, "generation_failed", "generation_failed code must stay visible");
assert.ok(generationFailedCopy.affordances.includes("retry"), "generation_failed needs a labelled retry action");
assert.ok(generationFailedCopy.affordances.includes("dashboard"), "generation_failed needs a labelled dashboard action");
assert.ok(UNIFIED_SUBSCRIBER_CLOSE_REASONS.has("refit_faulted") && !UNIFIED_REATTACH_REASONS.has("refit_faulted"), "refit_faulted must be terminal protocol state");
assert.equal(paneStateFromClose("refit_faulted").kind, "failed", "refit_faulted must map to failed workspace state");
for (const reason of UNIFIED_TAKEOVER_REASONS) assert.ok(["takeover_pending", "displaced"].includes(paneStateFromClose(reason).kind), `${reason} must render as a takeover state`);
assert.equal(paneStateFromClose("lease_held").kind, "takeover_pending");
assert.equal(paneStateFromClose("control_displaced").kind, "displaced");
for (const reason of UNIFIED_INTERNAL_REASONS) assert.equal(paneStateFromClose(reason).kind, "closed", `${reason} is page-inflicted and renders closed`);
// The controller's identity refusals never arrive as a close: they exhaust
// the transport (ReconnectRefusal → reconnectStatus EXHAUSTED) and the page
// renders `{ kind: "failed", reason }`. That path carries each refusal's own
// copy, never the placeholder.
const refusalCodes = [...uiController.matchAll(/new ReconnectRefusal\("([a-z0-9_]+)"\)/g)].map((match) => match[1]);
assert.ok(refusalCodes.includes("session_gone") && refusalCodes.includes("identity_ambiguous") && refusalCodes.includes("identity_invalid"), `controller refusal codes: ${refusalCodes.join(",")}`);
for (const code of refusalCodes) {
  const failed = paneStateCopy(Object.freeze({ kind: "failed" as const, reason: code }));
  nonblank(Object.freeze({ kind: "failed" as const, reason: code }), `refusal ${code}`);
  assert.equal(failed.code, code, `refusal ${code}: the code must stay visible`);
  assert.ok(failed.headline !== paneStateCopy(Object.freeze({ kind: "failed" as const, reason: "no_such_reason_xyz" })).headline, `refusal ${code} renders the placeholder headline`);
}
assert.equal(paneStateCopy(Object.freeze({ kind: "failed" as const, reason: "session_gone" })).headline, "This session has ended");
for (const reason of [...emitted].sort()) nonblank(Object.freeze({ kind: "failed" as const, reason }), `exhausted ${reason}`);
// A hostile reason collapses to the bounded placeholder, never to blank text.
nonblank(paneStateFromClose("<script>x"), "hostile close reason");

// Every projection state → an explicit pane state with visible copy.
const projectionVerdicts: Record<string, string> = {};
for (const state of PROJECTION_PANE_STATES) {
  const pane = paneStateFromProjection({ state } as never);
  projectionVerdicts[state] = pane.kind;
  nonblank(pane, `projection ${state}`);
}
assert.equal(paneStateFromProjection({ state: "open", origin: "birth" }).kind, "connecting");
const deferredRotation = paneStateFromProjection({ state: "open", origin: "birth", detail: "rotation_deferred_alt_screen" });
assert.equal(deferredRotation.kind, "connecting");
assert.ok(/full-screen app.*rotation waits/i.test(paneStateCopy(deferredRotation).detail));
const liveDeferredRotation = Object.freeze({ kind: "live" as const, columns: 80, rows: 24, detail: "rotation_deferred_alt_screen" as const });
assert.equal(paneStateCopy(liveDeferredRotation).badge, "rotation deferred");
assert.ok(/full-screen app.*rotation waits/i.test(paneStateCopy(liveDeferredRotation).detail));
assert.equal(paneStateFromProjection({ state: "adoptable" }).kind, "projection");
assert.equal(paneStateFromProjection({ state: "slots_exhausted" }).kind, "projection");

// Every pane state kind has copy, including each on_missing variant.
for (const kind of PANE_STATE_KINDS) {
  const samples: PaneState[] = kind === "missing" ? ON_MISSING_VALUES.map((onMissing) => Object.freeze({ kind: "missing" as const, onMissing }))
    : kind === "live" ? [Object.freeze({ kind: "live" as const, columns: 80, rows: 24 })]
      : kind === "create_failed" ? [Object.freeze({ kind: "create_failed" as const, code: "name_taken", status: 409 })]
        : kind === "projection" ? PROJECTION_PANE_STATES.map((state) => Object.freeze({ kind: "projection" as const, state }))
          : (kind === "reconnecting" || kind === "reattaching" || kind === "takeover_pending" || kind === "displaced" || kind === "failed") ? [Object.freeze({ kind, reason: "attachment_failed" } as PaneState)]
            : [Object.freeze({ kind } as PaneState)];
  for (const sample of samples) nonblank(sample, `pane kind ${kind}`);
}

// Source fences: no `default => unavailable` escape and no private close table
// in the workspace sources; the runtime routes every close through the shared
// classifier.
for (const file of ["workspace_layout.ts", "workspace_page.ts", "unified_pane_controller.ts"]) {
  const source = fs.readFileSync(path.join(uiRoot, "src", file), "utf8").replace(/\/\/[^\n]*/g, "");
  assert.ok(!/default:\s*(?:return|\{[^}]*return)[^;]*unavailable/.test(source), `${file}: a default => unavailable escape exists`);
  assert.ok(!/new Set\(\[[^\]]*"(?:lease_held|control_displaced|subscriber_lagged|input_refused)"/.test(source), `${file}: a private close table exists`);
}
const pageSource = fs.readFileSync(path.join(uiRoot, "src", "workspace_page.ts"), "utf8");
assert.ok(pageSource.includes("paneStateFromClose(reason)"), "the workspace page must classify transport closes through the shared policy");
assert.ok(pageSource.includes('from "./workspace_layout"') && !pageSource.includes("UNIFIED_TERMINAL_REASONS"), "the page consumes the layout's classification, not the raw sets");

// keep the causal loading-state suite discoverable and transitively
// owned by the canonical Chromium browser gate.
const packageManifest = JSON.parse(fs.readFileSync(path.join(uiRoot, "package.json"), "utf8")) as { scripts?: Record<string, string> };
const loadingStateScript = packageManifest.scripts?.["test:loading-state-browser"] ?? "";
assert.ok(loadingStateScript.includes("loading_state_browser_harness.ts"), "test:loading-state-browser must build the loading state browser harness");
assert.ok(loadingStateScript.includes("run_loading_state_browser.cjs"), "test:loading-state-browser must run the loading state causal suite");
// ci_reachability.cjs enforces this suite's transitive ownership by CI.

process.stdout.write(`workspace failure policy: ${emitted.size} emitted close reasons, ${PROJECTION_PANE_STATES.length} projection states, ${PANE_STATE_KINDS.length} pane kinds — all explicit and nonblank\n`);
declare const process: { stdout: { write(value: string): void } };
