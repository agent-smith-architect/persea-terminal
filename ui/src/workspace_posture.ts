// Device posture for workspaces — one rule, one home.
//
// Two documents need this answer and they must never disagree: the /workspace
// document decides whether it opens panes or the honest phone-class state, and
// the dashboard decides whether it may offer a workspace at all (a
// phone must not be given a create affordance the same device then refuses to
// open). The rule and its notice therefore live in a module that imports
// nothing: workspace_layout.ts and workspace_page.ts already depend on
// dashboard.ts, so putting the shared rule in either of them would make the
// dashboard's use of it an import cycle. Both re-export it, so no existing
// importer changes.

// Phone class = coarse pointer AND phone-class width. The short viewport edge
// is the width witness so rotating a phone does not flip posture (and
// mount/destroy cells) mid-session.
export type WorkspacePosture = "desktop" | "phone";
export const PHONE_CLASS_SHORT_EDGE_PX = 600;
export type PostureEnvironment = Readonly<{ coarsePointer: boolean; viewportWidth: number; viewportHeight: number }>;

export function workspacePosture(env: PostureEnvironment): WorkspacePosture {
  const shortEdge = Math.min(env.viewportWidth, env.viewportHeight);
  return env.coarsePointer && shortEdge <= PHONE_CLASS_SHORT_EDGE_PX ? "phone" : "desktop";
}
export function readPostureEnvironment(win: Window): PostureEnvironment {
  return Object.freeze({ coarsePointer: win.matchMedia?.("(pointer: coarse)").matches === true, viewportWidth: win.innerWidth, viewportHeight: win.innerHeight });
}

// Posture. Decided ONCE at boot from the device
// screen's short edge, which is stable under the software keyboard; never
// re-evaluated from innerHeight on resize/orientation events. This is the
// reading both documents use, so the dashboard's prediction and the workspace
// document's decision are the same function of the same inputs.
export function stablePostureEnvironment(win: Window): PostureEnvironment {
  const screenWidth = win.screen?.width ?? 0;
  const screenHeight = win.screen?.height ?? 0;
  const usable = screenWidth > 0 && screenHeight > 0;
  return Object.freeze({
    coarsePointer: win.matchMedia?.("(pointer: coarse)").matches === true,
    viewportWidth: usable ? screenWidth : win.innerWidth,
    viewportHeight: usable ? screenHeight : win.innerHeight,
  });
}

// The one sentence a phone-class device is told about workspaces. The
// /workspace document renders it as its honest state; the dashboard renders
// the same words where the create form would have been, so the refusal the
// operator would meet is stated before they can produce a record for it.
export const PHONE_STATE_NOTICE = "workspace view is not available on this device yet";
