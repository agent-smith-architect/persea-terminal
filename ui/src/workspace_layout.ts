// Workspace layout renderer — presentation only.
//
// Renders a split tree as nested flex containers with draggable dividers
// (pointer math) and +/- nudge buttons, one cell per leaf. A
// cell exposes a mount point for the unified page and a state slot that
// renders every per-pane state as visible text: the workspace never shows
// a blank cell or a dead-looking control.
//
// Layout is presentation-only; no geometry path exists in this code. Dividers
// change CSS weight classes and nothing else. A cell resize reaches the
// mounted page's own ResizeObserver, which refits the font. This module
// imports no transport, no attachment page, and
// no protocol frame type. Styles are class-based (workspace.css): no element
// ever carries a style attribute, so the shell renders under the base nonce'd
// CSP.
import "./workspace.css";
import { createFailureMessage, unifiedBlockedMessage, type UnifiedSessionProjection } from "./dashboard";
import { UNIFIED_TAKEOVER_REASONS, boundedUnifiedReason, classifyUnifiedClose, unifiedCloseNotice, type UnifiedCloseClass, type UnifiedInventoryDetail } from "./unified_close_policy";
import {
  DIVIDER_NUDGE_STEP, dragDivider, leafEntries, normalizeWeights, nudgeDivider, pathLabel, validateWorkspaceTree, workspaceRefusalMessage,
  type NodePath, type OnMissing, type WorkspaceLeaf, type WorkspaceNode, type WorkspaceRefusal, type WorkspaceSplit,
} from "./workspace_model";

// --- Per-pane state. Resolution states are workspace-owned;
// projection states are the shared UnifiedSessionProjection states (imported,
// never re-declared); attachment states are classified through the shared
// close policy. Rendering is exhaustive by construction (`never` checks), so a
// new state or a new emitted close class fails to compile rather than falling
// through to a default.

export type ProjectionPaneState = Exclude<UnifiedSessionProjection["state"], "open">;

export type PaneState =
  | Readonly<{ kind: "resolving" }>
  | Readonly<{ kind: "connecting"; detail?: UnifiedInventoryDetail }>
  | Readonly<{ kind: "live"; columns: number; rows: number; detail?: UnifiedInventoryDetail }>
  | Readonly<{ kind: "missing"; onMissing: OnMissing }>
  | Readonly<{ kind: "ambiguous" }>
  | Readonly<{ kind: "create_failed"; code: string; status: number }>
  | Readonly<{ kind: "duplicate_refused" }>
  | Readonly<{ kind: "realm_unavailable" }>
  | Readonly<{ kind: "projection"; state: ProjectionPaneState }>
  | Readonly<{ kind: "reconnecting"; reason: string }>
  | Readonly<{ kind: "reattaching"; reason: string }>
  | Readonly<{ kind: "takeover_pending"; reason: string }>
  | Readonly<{ kind: "displaced"; reason: string }>
  | Readonly<{ kind: "failed"; reason: string }>
  | Readonly<{ kind: "rate_limited" }>
  | Readonly<{ kind: "closed" }>
  // The phone-class honest state: the session exists and could be
  // attached, but this posture opens no pane — so the card says that, never a
  // transport state that does not exist.
  | Readonly<{ kind: "not_opened" }>;

export type PaneStateKind = PaneState["kind"];
export const PANE_STATE_KINDS: readonly PaneStateKind[] = Object.freeze([
  "resolving", "connecting", "live", "missing", "ambiguous", "create_failed", "duplicate_refused", "realm_unavailable",
  "projection", "reconnecting", "reattaching", "takeover_pending", "displaced", "failed", "rate_limited", "closed", "not_opened",
]);
// Derived from a record the compiler checks against the shared projection
// type: a projection state added in dashboard.ts without an entry here fails
// to compile, so the browser gate's FW-blank sample can never silently omit
// one.
const PROJECTION_PANE_STATE_SET = {
  adoptable: true, blocked_alt_screen: true, blocked_multi_pane: true, blocked_multi_window: true, blocked_foreign_server: true, slots_exhausted: true, unavailable: true,
} satisfies Record<ProjectionPaneState, true>;
export const PROJECTION_PANE_STATES: readonly ProjectionPaneState[] = Object.freeze(Object.keys(PROJECTION_PANE_STATE_SET) as ProjectionPaneState[]);

export type PaneAffordance = "create" | "skip" | "retry" | "take_control" | "dashboard";
export type PaneTone = "neutral" | "waiting" | "live" | "blocked" | "failed";
export type PaneStateCopy = Readonly<{ tone: PaneTone; badge: string; headline: string; detail: string; code?: string; affordances: readonly PaneAffordance[] }>;

function copy(tone: PaneTone, badge: string, headline: string, detail: string, affordances: readonly PaneAffordance[] = [], code?: string): PaneStateCopy {
  return Object.freeze({ tone, badge, headline, detail, affordances: Object.freeze([...affordances]), ...(code !== undefined ? { code } : {}) });
}

// The projection an inventory row carries maps onto a pane state: an open
// session is about to attach; every other projection renders as itself.
export function paneStateFromProjection(projection: UnifiedSessionProjection): PaneState {
  if (projection.state === "open") return Object.freeze({ kind: "connecting", ...(projection.detail ? { detail: projection.detail } : {}) });
  return Object.freeze({ kind: "projection", state: projection.state });
}

// A transport close reason maps onto a pane state through the ONE shared
// close policy: reattach → reattaching, transient (including the reconnectable
// subscriber_lagged / generation_rotated once the policy carries them) →
// reconnecting, internal → closed, terminal → a takeover state when the reason
// is claimable, otherwise failed-with-reason. No workspace-private enum.
export function paneStateFromClose(reason: string): PaneState {
  const bounded = boundedUnifiedReason(reason);
  const cls: UnifiedCloseClass = classifyUnifiedClose(bounded);
  switch (cls) {
    case "reattach": return Object.freeze({ kind: "reattaching", reason: bounded });
    case "transient": return Object.freeze({ kind: "reconnecting", reason: bounded });
    case "internal": return Object.freeze({ kind: "closed" });
    case "terminal":
      if (UNIFIED_TAKEOVER_REASONS.has(bounded)) {
        return bounded === "lease_held"
          ? Object.freeze({ kind: "takeover_pending", reason: bounded })
          : Object.freeze({ kind: "displaced", reason: bounded });
      }
      return Object.freeze({ kind: "failed", reason: bounded });
    default: {
      const exhaustive: never = cls;
      throw new Error(`unclassified close class ${String(exhaustive)}`);
    }
  }
}

function missingAffordances(onMissing: OnMissing): readonly PaneAffordance[] {
  switch (onMissing) {
    case "offer": return ["create", "skip"];
    case "create": return ["retry", "skip"];
    case "skip": return ["create"];
    default: {
      const exhaustive: never = onMissing;
      throw new Error(`unknown on_missing ${String(exhaustive)}`);
    }
  }
}

// Every inventory detail must have reviewed words before it can reach a pane.
// Switching here rather than testing one string means a new member of
// UNIFIED_INVENTORY_DETAILS breaks the build instead of quietly rendering the
// geometry copy — on a live pane that would be a permanent "80×24 / Attached
// at 80×24." strip over a running terminal.
function detailCopy(detail: UnifiedInventoryDetail, headline: string): PaneStateCopy {
  switch (detail) {
    case "rotation_deferred_alt_screen":
      return copy("waiting", "rotation deferred", headline, "A full-screen app is active; this session remains open while journal rotation waits.");
    default: {
      const exhaustive: never = detail;
      throw new Error(`unreviewed inventory detail ${String(exhaustive)}`);
    }
  }
}

export function paneStateCopy(state: PaneState): PaneStateCopy {
  switch (state.kind) {
    case "resolving": return copy("waiting", "resolving", "Finding this session", "Looking the session up in the current inventory.");
    case "connecting": return state.detail === undefined
      ? copy("waiting", "connecting", "Attaching", "Opening this session's own attachment.")
      : detailCopy(state.detail, "Attaching");
    case "live": return state.detail === undefined
      ? copy("live", `${state.columns}×${state.rows}`, "Live", `Attached at ${state.columns}×${state.rows}.`)
      : detailCopy(state.detail, "Live");
    case "missing": {
      const detail = state.onMissing === "create"
        ? "No session has this name. The workspace is set to create it; creation did not complete yet."
        : state.onMissing === "skip"
          ? "No session has this name. The workspace is set to skip it."
          : "No session has this name. Create it now, or skip this pane.";
      return copy("blocked", "missing", "This session does not exist", detail, missingAffordances(state.onMissing));
    }
    case "ambiguous": return copy("blocked", "ambiguous", "More than one session matches", "Several sessions carry this name. Pick the one you want from the dashboard; this pane will not guess.", ["dashboard"]);
    case "create_failed": return copy("failed", "create failed", "The session could not be created", createFailureMessage(state.code, state.status), ["retry", "skip"], boundedUnifiedReason(state.code));
    case "duplicate_refused": return copy("blocked", "duplicate", "Already open in this workspace", "A workspace opens each session once. Remove this pane or point it at another session.", ["dashboard"]);
    case "realm_unavailable": return copy("failed", "realm unavailable", "This realm is unreachable", "The realm that hosts this session did not answer. Retry when it is back.", ["retry"]);
    case "projection": {
      const detail = state.state === "adoptable" ? "This session is being adopted into the unified terminal." : unifiedBlockedMessage(state.state);
      return state.state === "adoptable"
        ? copy("waiting", "adopting", "Adopting", detail)
        : copy("blocked", state.state.replace(/_/g, " "), "Cannot open here", detail, ["retry", "dashboard"], state.state);
    }
    case "reconnecting": return copy("waiting", "reconnecting", "Reconnecting", "The connection dropped; this pane is reconnecting on its own.", [], state.reason);
    case "reattaching": return copy("waiting", "reattaching", "Reattaching", "The session refused a frame while attaching; this pane is reattaching with a fresh handle.", [], state.reason);
    case "takeover_pending": { const notice = unifiedCloseNotice(state.reason); return copy("blocked", "controlled elsewhere", notice.headline, notice.detail, ["take_control", "dashboard"], state.reason); }
    case "displaced": { const notice = unifiedCloseNotice(state.reason); return copy("failed", "displaced", notice.headline, notice.detail, ["take_control", "dashboard"], state.reason); }
    case "failed": { const notice = unifiedCloseNotice(state.reason); return copy("failed", "failed", notice.headline, notice.detail, ["retry", "dashboard"], state.reason); }
    case "rate_limited": return copy("waiting", "rate limited", "Waiting for a request slot", "The server is rate-limiting this page; this pane retries on its own.");
    case "closed": return copy("neutral", "closed", "Closed", "This pane's attachment was closed by this page.", ["retry"]);
    case "not_opened": return copy("neutral", "not opened on this device", "Not opened on this device", "Workspace panes do not open on a phone-class device. Open the session as a single terminal instead.");
    default: {
      const exhaustive: never = state;
      throw new Error(`unrendered pane state ${JSON.stringify(exhaustive)}`);
    }
  }
}

// --- Posture. The rule itself lives in workspace_posture.ts
// so the dashboard can ask the same question without an import cycle; this
// module keeps its name in the surface its callers already import.
export { PHONE_CLASS_SHORT_EDGE_PX, readPostureEnvironment, workspacePosture } from "./workspace_posture";
export type { PostureEnvironment, WorkspacePosture } from "./workspace_posture";

// --- DOM helpers (class-based only).

function element<K extends keyof HTMLElementTagNameMap>(tag: K, className?: string, text?: string): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

const WEIGHT_CLASS = /^ws-w-\d+$/;
function setWeightClass(node: HTMLElement, weight: number): void {
  for (const name of Array.from(node.classList)) if (WEIGHT_CLASS.test(name)) node.classList.remove(name);
  node.classList.add(`ws-w-${weight}`);
}

const AFFORDANCE_LABEL: Readonly<Record<PaneAffordance, string>> = Object.freeze({
  create: "Create session", skip: "Skip this pane", retry: "Retry", take_control: "Take control", dashboard: "Dashboard",
});

// --- Cells.

export type WorkspaceCell = Readonly<{
  key: string;
  leaf: WorkspaceLeaf;
  element: HTMLElement;
  mount: HTMLElement;
  setLeaf(leaf: WorkspaceLeaf): void;
  setState(state: PaneState): void;
  state(): PaneState;
}>;

export type WorkspaceLayoutHandlers = Readonly<{
  onTreeChange?: (tree: WorkspaceNode, cause: "divider_drag" | "divider_nudge" | "divider_key") => void;
  onAffordance?: (cell: WorkspaceCell, affordance: PaneAffordance) => void;
  onCellRetired?: (cell: WorkspaceCell) => void;
}>;

class CellView implements WorkspaceCell {
  readonly element = element("section", "ws-cell");
  readonly mount = element("div", "ws-cell__mount");
  private readonly badge = element("span", "ws-cell__badge");
  private readonly slot = element("div", "ws-cell__state");
  private readonly headline = element("p", "ws-cell__state-headline");
  private readonly detail = element("p", "ws-cell__state-detail");
  private readonly code = element("p", "ws-cell__state-code");
  private readonly actions = element("div", "ws-cell__state-actions");
  private readonly notice = element("aside", "ws-cell__notice");
  private readonly noticeBadge = element("span", "ws-cell__notice-badge");
  private readonly noticeDetail = element("p", "ws-cell__notice-detail");
  private readonly name = element("span", "ws-cell__name");
  private readonly hint = element("span", "ws-cell__hint");
  private readonly scope = element("span", "ws-cell__scope");
  private currentLeaf: WorkspaceLeaf;
  private current: PaneState = Object.freeze({ kind: "resolving" });

  constructor(readonly key: string, leaf: WorkspaceLeaf, private readonly onAffordance: (cell: WorkspaceCell, affordance: PaneAffordance) => void) {
    this.currentLeaf = leaf;
    const { session } = this.currentLeaf;
    this.element.setAttribute("aria-label", `Pane ${session.name}`);
    this.element.dataset.wsSession = session.name;
    const header = element("div", "ws-cell__header");
    header.tabIndex = 0;
    header.setAttribute("aria-label", `Focus pane ${session.name}`);
    header.append(this.name, this.hint, this.scope, this.badge);
    this.slot.setAttribute("role", "status");
    this.slot.setAttribute("aria-live", "polite");
    this.slot.append(this.headline, this.detail, this.code, this.actions);
    // The notice is the live pane's only voice (see setState). It is a status
    // region, never a control, so it takes no focus and no tab stop.
    this.notice.setAttribute("role", "status");
    this.notice.setAttribute("aria-live", "polite");
    this.notice.hidden = true;
    this.notice.append(this.noticeBadge, this.noticeDetail);
    const body = element("div", "ws-cell__body");
    body.append(this.mount, this.slot, this.notice);
    this.element.append(header, body);
    this.setLeaf(this.currentLeaf);
    this.setState(this.current);
  }

  get leaf(): WorkspaceLeaf { return this.currentLeaf; }
  state(): PaneState { return this.current; }

  setLeaf(leaf: WorkspaceLeaf): void {
    this.currentLeaf = leaf;
    const { session } = leaf;
    this.element.setAttribute("aria-label", `Pane ${session.name}`);
    this.element.dataset.wsSession = session.name;
    this.name.textContent = session.name;
    this.hint.textContent = leaf.aliasHint ?? "";
    this.hint.hidden = leaf.aliasHint === undefined;
    this.scope.textContent = `${session.realm} · ${session.server}`;
  }

  setState(state: PaneState): void {
    this.current = state;
    const rendered = paneStateCopy(state);
    this.element.dataset.wsState = state.kind;
    this.element.dataset.wsTone = rendered.tone;
    this.badge.textContent = rendered.badge;
    this.headline.textContent = rendered.headline;
    this.detail.textContent = rendered.detail;
    this.code.textContent = rendered.code ?? "";
    this.code.hidden = rendered.code === undefined;
    this.actions.replaceChildren(...rendered.affordances.map((affordance) => this.action(affordance)));
    this.actions.hidden = rendered.affordances.length === 0;
    // Live panes show the mounted terminal; every other state overlays its
    // copy on the mount so the cell reads as its state, never as empty.
    this.slot.hidden = state.kind === "live";
    // A live pane is the one state whose copy has nowhere to go: the overlay
    // is hidden here and the header badge is hidden in CSS, so the reviewed
    // words for a rotation-deferred live session reached the DOM and reached
    // no reader. An inventory detail is the only thing a live pane still has
    // to say, so it says it in a notice that is positioned OUT OF FLOW. The
    // mount's box never changes, so the terminal never refits and no RESIZE
    // is spent on presentation (workspace rotation).
    const inlineDetail = state.kind === "live" ? state.detail : undefined;
    if (inlineDetail === undefined) delete this.element.dataset.wsDetail;
    else this.element.dataset.wsDetail = inlineDetail;
    this.noticeBadge.textContent = inlineDetail === undefined ? "" : rendered.badge;
    this.noticeDetail.textContent = inlineDetail === undefined ? "" : rendered.detail;
    // The strip is capped to one line in CSS so it cannot cover the terminal
    // it sits over. A cap can hide words, so the reviewed sentence also goes
    // where width cannot truncate it — the title and the accessible name, the
    // same pair the dashboard uses for this very sentence.
    if (inlineDetail === undefined) {
      this.notice.removeAttribute("title");
      this.notice.removeAttribute("aria-label");
    } else {
      const spoken = `${rendered.badge}: ${rendered.detail}`;
      this.notice.title = spoken;
      this.notice.setAttribute("aria-label", spoken);
    }
    this.notice.hidden = inlineDetail === undefined;
  }

  private action(affordance: PaneAffordance): HTMLElement {
    if (affordance === "dashboard") {
      const link = element("a", "ws-cell__action", AFFORDANCE_LABEL.dashboard);
      link.href = "/";
      link.dataset.wsAffordance = affordance;
      return link;
    }
    const button = element("button", "ws-cell__action", AFFORDANCE_LABEL[affordance]);
    button.type = "button";
    button.dataset.wsAffordance = affordance;
    button.addEventListener("click", () => this.onAffordance(this, affordance));
    return button;
  }
}

// --- Dividers.

type DragState = Readonly<{ pointerId: number; path: NodePath; divider: number; before: HTMLElement; after: HTMLElement; size: number }>;

// --- The view.

export class WorkspaceLayoutView {
  private readonly root = element("div", "ws-root");
  private readonly cells = new Map<string, CellView>();
  private readonly splits = new Map<string, HTMLElement>();
  private current: WorkspaceNode;
  private drag: DragState | undefined;
  // The presentation renderer stays interactive by default for its standalone
  // harness/API. WorkspacePage explicitly seals it outside workspace edit mode.
  private editing = true;
  private readonly handlers: WorkspaceLayoutHandlers;

  constructor(private readonly host: HTMLElement, tree: WorkspaceNode, handlers: WorkspaceLayoutHandlers = {}) {
    this.handlers = handlers;
    this.current = tree;
    this.root.setAttribute("role", "group");
    this.root.setAttribute("aria-label", "Workspace panes");
    host.replaceChildren(this.root);
    this.update(tree);
  }

  tree(): WorkspaceNode { return this.current; }
  cell(key: string): WorkspaceCell | undefined { return this.cells.get(key); }
  allCells(): readonly WorkspaceCell[] { return [...this.cells.values()]; }
  setEditing(editing: boolean): void {
    this.editing = editing;
    this.root.dataset.wsEditing = editing ? "true" : "false";
    for (const button of Array.from(this.root.querySelectorAll<HTMLButtonElement>(".ws-divider__nudge"))) button.disabled = !editing;
  }

  // Keyed reconciliation: a cell whose leaf survives keeps its element (and
  // whatever is mounted in it); split containers and dividers are rebuilt.
  update(tree: WorkspaceNode): void {
    const verdict = validateWorkspaceTree(tree);
    if (!verdict.ok) throw new Error(workspaceRefusalMessage(verdict.refusal));
    this.current = tree;
    const live = new Set(verdict.value.map((entry) => entry.key));
    for (const [key, cell] of this.cells) {
      if (live.has(key)) continue;
      this.handlers.onCellRetired?.(cell);
      cell.element.remove();
      this.cells.delete(key);
    }
    this.splits.clear();
    this.root.replaceChildren(this.build(tree, []));
  }

  destroy(): void {
    for (const cell of this.cells.values()) this.handlers.onCellRetired?.(cell);
    this.cells.clear();
    this.splits.clear();
    this.root.remove();
  }

  private build(node: WorkspaceNode, path: NodePath): HTMLElement {
    if (node.kind === "leaf") {
      const key = JSON.stringify([node.session.realm, node.session.server, node.session.name]);
      let cell = this.cells.get(key);
      if (!cell) {
        cell = new CellView(key, node, (target, affordance) => this.handlers.onAffordance?.(target, affordance));
        this.cells.set(key, cell);
      }
      cell.setLeaf(node);
      cell.element.classList.add("ws-track");
      cell.element.dataset.wsPath = pathLabel(path);
      return cell.element;
    }
    const container = element("div", `ws-split ws-split--${node.direction} ws-track`);
    container.dataset.wsSplit = pathLabel(path);
    this.splits.set(pathLabel(path), container);
    node.children.forEach((child, index) => {
      if (index > 0) container.append(this.divider(node, path, index - 1));
      container.append(this.build(child, [...path, index]));
    });
    this.applyWeights(container, node);
    return container;
  }

  private applyWeights(container: HTMLElement, node: WorkspaceSplit): void {
    const tracks = Array.from(container.children).filter((child): child is HTMLElement => child instanceof HTMLElement && child.classList.contains("ws-track"));
    node.weights.forEach((weight, index) => { const track = tracks[index]; if (track) setWeightClass(track, weight); });
    const normalized = normalizeWeights(node.weights);
    const dividers = Array.from(container.children).filter((child): child is HTMLElement => child instanceof HTMLElement && child.classList.contains("ws-divider"));
    dividers.forEach((divider, index) => {
      const pair = normalized[index] + normalized[index + 1];
      divider.setAttribute("aria-valuenow", String(Math.round((normalized[index] / pair) * 100)));
    });
  }

  private divider(node: WorkspaceSplit, path: NodePath, index: number): HTMLElement {
    const divider = element("div", `ws-divider ws-divider--${node.direction}`);
    divider.setAttribute("role", "separator");
    divider.setAttribute("aria-orientation", node.direction === "row" ? "vertical" : "horizontal");
    divider.setAttribute("aria-valuemin", "1");
    divider.setAttribute("aria-valuemax", "99");
    divider.setAttribute("aria-label", `Resize panes ${index + 1} and ${index + 2}`);
    divider.tabIndex = 0;
    divider.dataset.wsSplit = pathLabel(path);
    divider.dataset.wsDivider = String(index);
    const grip = element("div", "ws-divider__grip");
    const shrink = element("button", "ws-divider__nudge", node.direction === "row" ? "◀" : "▲");
    const grow = element("button", "ws-divider__nudge", node.direction === "row" ? "▶" : "▼");
    shrink.type = "button"; grow.type = "button";
    shrink.disabled = !this.editing; grow.disabled = !this.editing;
    shrink.setAttribute("aria-label", `Shrink pane ${index + 1}`);
    grow.setAttribute("aria-label", `Grow pane ${index + 1}`);
    shrink.dataset.wsNudge = "-1"; grow.dataset.wsNudge = "1";
    shrink.addEventListener("click", () => this.nudge(path, index, -DIVIDER_NUDGE_STEP, "divider_nudge"));
    grow.addEventListener("click", () => this.nudge(path, index, DIVIDER_NUDGE_STEP, "divider_nudge"));
    grip.append(shrink, grow);
    divider.append(grip);
    divider.addEventListener("pointerdown", (event) => this.startDrag(event, divider, path, index));
    divider.addEventListener("pointermove", (event) => this.moveDrag(event));
    divider.addEventListener("pointerup", (event) => this.endDrag(event, divider));
    divider.addEventListener("pointercancel", (event) => this.endDrag(event, divider));
    divider.addEventListener("keydown", (event) => {
      const backward = node.direction === "row" ? "ArrowLeft" : "ArrowUp";
      const forward = node.direction === "row" ? "ArrowRight" : "ArrowDown";
      if (event.key !== backward && event.key !== forward) return;
      event.preventDefault();
      this.nudge(path, index, event.key === backward ? -DIVIDER_NUDGE_STEP : DIVIDER_NUDGE_STEP, "divider_key");
    });
    return divider;
  }

  private startDrag(event: PointerEvent, divider: HTMLElement, path: NodePath, index: number): void {
    if (!this.editing || event.button !== 0 || this.drag !== undefined) return;
    if (event.target instanceof HTMLElement && event.target.closest(".ws-divider__nudge")) return;
    const container = this.splits.get(pathLabel(path));
    if (!container) return;
    const tracks = Array.from(container.children).filter((child): child is HTMLElement => child instanceof HTMLElement && child.classList.contains("ws-track"));
    const before = tracks[index]; const after = tracks[index + 1];
    if (!before || !after) return;
    event.preventDefault();
    divider.setPointerCapture(event.pointerId);
    divider.dataset.wsDragging = "true";
    this.drag = Object.freeze({ pointerId: event.pointerId, path, divider: index, before, after, size: 8 });
  }

  private moveDrag(event: PointerEvent): void {
    const drag = this.drag;
    if (!drag || event.pointerId !== drag.pointerId) return;
    const split = this.findSplit(drag.path);
    if (!split) return;
    const horizontal = split.direction === "row";
    const start = horizontal ? drag.before.getBoundingClientRect().left : drag.before.getBoundingClientRect().top;
    const end = horizontal ? drag.after.getBoundingClientRect().right : drag.after.getBoundingClientRect().bottom;
    const position = horizontal ? event.clientX : event.clientY;
    const extent = end - start - drag.size;
    if (extent <= 0) return;
    const fraction = (position - start - drag.size / 2) / extent;
    const next = dragDivider(this.current, drag.path, drag.divider, fraction);
    if (!next.ok) return;
    this.commit(next.value, drag.path, "divider_drag");
  }

  private endDrag(event: PointerEvent, divider: HTMLElement): void {
    const drag = this.drag;
    if (!drag || event.pointerId !== drag.pointerId) return;
    this.drag = undefined;
    delete divider.dataset.wsDragging;
    if (divider.hasPointerCapture(event.pointerId)) divider.releasePointerCapture(event.pointerId);
  }

  private nudge(path: NodePath, index: number, delta: number, cause: "divider_nudge" | "divider_key"): void {
    if (!this.editing) return;
    const next = nudgeDivider(this.current, path, index, delta);
    if (!next.ok) return;
    this.commit(next.value, path, cause);
  }

  private findSplit(path: NodePath): WorkspaceSplit | undefined {
    let node: WorkspaceNode | undefined = this.current;
    for (const step of path) { if (!node || node.kind !== "split") return undefined; node = node.children[step]; }
    return node?.kind === "split" ? node : undefined;
  }

  // Weight changes are applied in place (class swap on the split's tracks); no
  // cell is rebuilt, so a mounted terminal only sees its cell's box change.
  private commit(tree: WorkspaceNode, path: NodePath, cause: "divider_drag" | "divider_nudge" | "divider_key"): void {
    this.current = tree;
    const split = this.findSplit(path);
    const container = this.splits.get(pathLabel(path));
    if (split && container) this.applyWeights(container, split);
    this.handlers.onTreeChange?.(tree, cause);
  }
}

// --- Honest phone-class state: the workspace name, the
// notice, and one link per leaf to open that leaf as a single terminal — the
// href is the same unified terminal URL the dashboard row renders, minted by
// the caller from its inventory snapshot. Zero cells, zero transports.
import { PHONE_STATE_NOTICE } from "./workspace_posture";
export { PHONE_STATE_NOTICE };
export type PhoneLeafEntry = Readonly<{ leaf: WorkspaceLeaf; href?: string; state: PaneState }>;

export function renderWorkspacePhoneState(host: HTMLElement, name: string, leaves: readonly PhoneLeafEntry[]): void {
  const panel = element("section", "ws-phone");
  panel.setAttribute("aria-label", `Workspace ${name}`);
  panel.dataset.wsPosture = "phone";
  panel.append(element("p", "ws-phone__eyebrow", "Workspace"), element("h1", "ws-phone__title", name), element("p", "ws-phone__notice", PHONE_STATE_NOTICE));
  const list = element("ul", "ws-phone__leaves");
  for (const entry of leaves) {
    const item = element("li", "ws-phone__leaf");
    const { session } = entry.leaf;
    item.dataset.wsSession = session.name;
    item.append(element("span", "ws-phone__leaf-name", session.name), element("span", "ws-phone__leaf-scope", `${session.realm} · ${session.server}`));
    const rendered = paneStateCopy(entry.state);
    item.dataset.wsState = entry.state.kind;
    item.append(element("span", "ws-phone__leaf-state", rendered.badge));
    if (entry.href !== undefined) {
      const link = element("a", "ws-phone__leaf-link", "Open as a single terminal");
      link.href = entry.href;
      link.setAttribute("aria-label", `Open ${session.name} as a single terminal`);
      item.append(link);
    }
    list.append(item);
  }
  const dashboard = element("a", "ws-phone__dashboard", "Dashboard");
  dashboard.href = "/";
  panel.append(list, dashboard);
  host.replaceChildren(panel);
}

// --- Workspace-level failure: full surface, a sentence, the code,
// and a way back to the dashboard.
export type WorkspaceUnavailableNotice = Readonly<{ headline: string; detail: string; code?: string }>;

export function renderWorkspaceUnavailable(host: HTMLElement, notice: WorkspaceUnavailableNotice): void {
  const panel = element("section", "ws-unavailable");
  panel.setAttribute("role", "alert");
  panel.dataset.wsUnavailable = notice.code ?? "unavailable";
  panel.append(element("h1", "ws-unavailable__headline", notice.headline), element("p", "ws-unavailable__detail", notice.detail));
  if (notice.code !== undefined) panel.append(element("p", "ws-unavailable__code", notice.code));
  const dashboard = element("a", "ws-unavailable__dashboard", "Dashboard");
  dashboard.href = "/";
  panel.append(dashboard);
  host.replaceChildren(panel);
}

export function workspaceRefusalNotice(refusal: WorkspaceRefusal): WorkspaceUnavailableNotice {
  return Object.freeze({ headline: "This workspace cannot be opened", detail: workspaceRefusalMessage(refusal), code: refusal.code });
}

export { leafEntries };
