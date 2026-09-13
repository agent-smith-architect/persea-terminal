// Controller-level pins for an in-place session switch.
//
// The projection observer is presentation only: a throw from it inside the
// switch commit, after the point of no return, must not be read as a commit
// failure and must not render a terminal `attachment_failed` verdict over a
// session that already committed. Settlement failures that are NOT
// presentation-only stay terminal, and a failure before the identity is
// replaced stays an ordinary pre-commit refusal.
//
// The projection-detail guard is keyed by the controller's CURRENT resolved
// incarnation, not the one it was constructed with (M11LF-F3C under E-P4's
// replaceable identity). After a switch A -> B the guard must accept B's key
// and refuse A's; keying by the construction-time `options.incarnationKey`
// inverts both answers.
//
// Why the class is driven this way: `new UnifiedPaneController(...)` builds a
// `UnifiedTerminalPage` (xterm, a live DOM, a WebSocket transport), which a
// node unit bundle has no way to supply, and the browser suites cannot
// separate the two spellings of the guard because the commit already clears
// the outgoing detail (both then behave alike on a live tree). So the real
// prototype methods run against explicit collaborator stubs: every field the
// exercised methods read is set here by name, and nothing is re-implemented.
const assert = {
  equal<T>(actual: T, expected: T, message?: string): void {
    if (actual !== expected) throw new Error(`${message ?? "assertion failed"}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
  },
};

import { UnifiedPaneController, type ResolvedPaneIdentity, type UnifiedPaneObserver } from "../src/unified_pane_controller";
import type { DashboardSession } from "../src/dashboard";
import type { UnifiedInventoryDetail } from "../src/unified_close_policy";

const DETAIL: UnifiedInventoryDetail = "rotation_deferred_alt_screen";

const dashboardSession = (name: string, draftScope: string): DashboardSession => Object.freeze({
  handles: Object.freeze({ alias: `alias-${name}`, observe: `observe-${name}`, control: `control-${name}` }),
  realm: "local", uid: 1000, server: "private", serverStatus: "ok", sessionId: `$${draftScope}`,
  name, width: 80, height: 24, attached: 0, activity: 1_000, aliases: Object.freeze([]), draftScope,
  unified: Object.freeze({ state: "open", origin: "birth" }),
});

const resolvedIdentity = (session: DashboardSession): ResolvedPaneIdentity => Object.freeze({
  selector: Object.freeze({ realm: session.realm, server: session.server, name: session.name }),
  incarnationKey: session.draftScope,
  sessionId: session.sessionId,
});

const runtimeIdentity = (session: DashboardSession) => Object.freeze({
  resolved: resolvedIdentity(session),
  incarnationKey: session.draftScope,
  sessionName: session.name,
  session,
});

type Harness = Readonly<{
  controller: UnifiedPaneController;
  commit(session: DashboardSession): void;
  details: (UnifiedInventoryDetail | undefined)[];
  presentations: string[];
  failures: string[];
  endpoints: string[];
}>;

// Builds a controller whose exercised methods are the real ones. `page` and
// `transport` are the two collaborators the commit primitive calls; both are
// recorded so the pins can read what the commit did.
const harness = (initial: DashboardSession, observer: UnifiedPaneObserver): Harness => {
  const details: (UnifiedInventoryDetail | undefined)[] = [];
  const presentations: string[] = [];
  const failures: string[] = [];
  const endpoints: string[] = [];
  const recordingObserver: UnifiedPaneObserver = Object.freeze({
    ...observer,
    projectionDetail: (detail) => { details.push(detail); observer.projectionDetail?.(detail); },
  });
  const page = {
    replaceSessionPresentation: (value: { sessionName: string }) => { presentations.push(value.sessionName); },
    failSessionSwitch: (reason: string) => { failures.push(reason); },
  };
  const transport = {
    invalidateEndpointWork: () => {},
    replaceEndpoint: (_endpoint: unknown, reason: string) => { endpoints.push(reason); },
  };
  const fields = {
    options: Object.freeze({ incarnationKey: initial.draftScope, observer: recordingObserver }),
    page, transport,
    currentIdentity: runtimeIdentity(initial),
    projectionDetail: undefined as UnifiedInventoryDetail | undefined,
    operationToken: 0,
    counters: { sourceRemints: 0, identityRemints: 0, takeovers: 0, adoptions: 0, switches: 0 },
    committedOnce: true,
    connected: true,
    disposed: false,
    snapshotGeneration: 1,
    lastControlHandle: initial.handles.control as string | undefined,
    lastControlHandleIdentity: initial.draftScope as string | null,
    reloadMintAbort: undefined as AbortController | undefined,
  };
  Object.setPrototypeOf(fields, UnifiedPaneController.prototype);
  const controller = fields as unknown as UnifiedPaneController;
  const commit = (session: DashboardSession): void => {
    const prepared = Object.freeze({
      abort: new AbortController(),
      session,
      identity: runtimeIdentity(session),
      endpoint: { url: `wss://fixture/${session.name}` },
      presentation: Object.freeze({ sessionName: session.name, composerStorageScope: session.draftScope }),
    });
    (controller as unknown as { commitSessionSwitch(value: unknown): void }).commitSessionSwitch(prepared);
  };
  return Object.freeze({ controller, commit, details, presentations, failures, endpoints });
};

const alpha = dashboardSession("alpha", "scope-a");
const beta = dashboardSession("beta", "scope-b");

// --- The re-keyed guard.
{
  const h = harness(alpha, {});

  assert.equal(h.controller.updateProjectionDetail("scope-a", DETAIL), true, "the birth incarnation must accept its own detail");
  assert.equal(h.controller.state().projectionDetail, DETAIL, "the accepted detail was not recorded");
  assert.equal(h.details.length, 1, "the observer did not fire for the accepted detail");

  h.commit(beta);
  assert.equal(h.controller.state().incarnationKey, "scope-b", "the commit did not replace the identity");
  assert.equal(h.presentations.join(","), "beta", "the commit did not replace the presentation");
  assert.equal(h.endpoints.join(","), "session_switch", "the commit did not replace the endpoint");
  assert.equal(h.failures.length, 0, "an ordinary commit rendered a failure");
  // The commit clears A's detail: it named a session this controller no
  // longer holds.
  assert.equal(h.details.length, 2, "the commit did not notify the cleared detail");
  assert.equal(h.details[1], undefined, "the commit cleared the detail with the wrong value");
  assert.equal(h.controller.state().projectionDetail, undefined, "A's detail survived the commit");

  // The property under pin: B's key is the only accepted key.
  const before = h.details.length;
  assert.equal(h.controller.updateProjectionDetail("scope-b", DETAIL), true, "the switched controller refused its own current incarnation key");
  assert.equal(h.controller.state().projectionDetail, DETAIL, "the switched controller did not record B's detail");
  assert.equal(h.details.length, before + 1, "the observer did not fire for B's detail");
  assert.equal(h.details[before], DETAIL, "the observer received the wrong detail for B");
}

{
  const h = harness(alpha, {});
  h.commit(beta);
  const before = h.details.length;
  assert.equal(h.controller.updateProjectionDetail("scope-a", DETAIL), false, "a stale incarnation key was accepted after the switch");
  assert.equal(h.controller.state().projectionDetail, undefined, "a stale key mutated the projection detail");
  assert.equal(h.details.length, before, "a stale key fired the projection observer");
}

// A disposed controller refuses every key, current or stale.
{
  const h = harness(alpha, {});
  (h.controller as unknown as { disposed: boolean }).disposed = true;
  assert.equal(h.controller.updateProjectionDetail("scope-a", DETAIL), false, "a disposed controller accepted a detail");
  assert.equal(h.details.length, 0, "a disposed controller fired the projection observer");
}

// --- The presentation observer cannot terminalize a committed switch.
{
  let thrown = 0;
  const h = harness(alpha, {
    projectionDetail: () => { thrown += 1; throw new Error("presentation observer failed"); },
  });
  // Give the controller a detail to clear, so the commit reaches the throwing
  // observer after the point of no return.
  assert.equal(h.controller.updateProjectionDetail("scope-a", DETAIL), true, "the detail the commit must clear was refused");
  assert.equal(thrown, 1, "the throwing observer was not reached before the commit");

  h.commit(beta);
  assert.equal(thrown, 2, "the commit did not reach the throwing observer");
  assert.equal(h.failures.length, 0, `a presentation observer throw terminalized a committed switch: ${JSON.stringify(h.failures)}`);
  assert.equal(h.controller.state().incarnationKey, "scope-b", "the switch did not commit B");
  assert.equal(h.presentations.join(","), "beta", "the commit stopped before replacing the presentation");
  assert.equal(h.endpoints.join(","), "session_switch", "the commit stopped before replacing the endpoint");
  assert.equal(h.controller.state().counters.switches, 1, "the committed switch was not counted");
  // The refusal path is unchanged for the ordinary caller: the observer still
  // throws inside `updateProjectionDetail`, and the caller still gets true.
  assert.equal(h.controller.updateProjectionDetail("scope-b", DETAIL), true, "a throwing observer changed the guard's answer");
  assert.equal(h.controller.state().projectionDetail, DETAIL, "a throwing observer discarded the recorded detail");
}

// A commit that fails BEFORE the identity is replaced is still a pre-commit
// refusal, not a terminal B verdict: the isolation above must not swallow it.
{
  const h = harness(alpha, {});
  (h.controller as unknown as { transport: { invalidateEndpointWork(): void } }).transport.invalidateEndpointWork = () => {
    throw new Error("endpoint work could not be invalidated");
  };
  let refused = "";
  try { h.commit(beta); } catch (error) { refused = (error as Error).message; }
  assert.equal(refused, "session switch commit failed before identity replacement", "a pre-commit failure was not raised as a refusal");
  assert.equal(h.controller.state().incarnationKey, "scope-a", "a refused commit replaced the identity");
  assert.equal(h.failures.length, 0, "a pre-commit refusal rendered a terminal verdict");
}

// A settlement failure AFTER the point of no return is still terminal.
{
  const h = harness(alpha, {});
  (h.controller as unknown as { page: { replaceSessionPresentation(value: unknown): void } }).page.replaceSessionPresentation = () => {
    throw new Error("presentation replacement failed");
  };
  h.commit(beta);
  assert.equal(h.controller.state().incarnationKey, "scope-b", "the identity was rolled back after the point of no return");
  assert.equal(h.failures.join(","), "attachment_failed", "a post-commit settlement failure was not terminal");
}

console.log("unified pane controller tests PASS");
