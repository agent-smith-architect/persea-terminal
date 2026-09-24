// Browser harness for the workspace layout shell (FW3 layout storm,
// FW-blank per-pane states, coarse-pointer touch targets, and the honest
// phone-class posture). No attachment, no transport, no terminal: each cell
// hosts a dummy pane that owns a stub send port. The stub is the FW3 sentinel:
// the storm must leave it untouched, and only an explicit fit call — never
// layout — may reach it (positive control).
import {
  PANE_STATE_KINDS, PHONE_STATE_NOTICE, PROJECTION_PANE_STATES, WorkspaceLayoutView, paneStateCopy, paneStateFromClose, paneStateFromProjection, readPostureEnvironment,
  renderWorkspacePhoneState, renderWorkspaceUnavailable, workspacePosture, type PaneState, type WorkspaceCell, type WorkspacePosture,
} from "../src/workspace_layout";
import { leaf, leafEntries, serializeWorkspaceTree, split, type WorkspaceLeaf, type WorkspaceNode } from "../src/workspace_model";
import { unifiedTerminalURL, type DashboardSession } from "../src/dashboard";
import { workspaceRouteNotice } from "../src/workspace_url";

type Sentinel = { resizeRequests: number; portCalls: string[]; webSockets: number; cspViolations: string[]; resizeObserverFires: number; pointerLog: string[] };
declare global {
  interface Window { __wsSentinel: Sentinel; __wsHarness: Harness; }
}

const sentinel: Sentinel = { resizeRequests: 0, portCalls: [], webSockets: 0, cspViolations: [], resizeObserverFires: 0, pointerLog: [] };
window.__wsSentinel = sentinel;
// Diagnostic trail for the gate: which pointer events reached which element.
for (const type of ["pointerdown", "pointermove", "pointerup", "pointercancel"] as const) {
  document.addEventListener(type, (event) => {
    const target = event.target instanceof Element ? event.target.className : String(event.target);
    if (sentinel.pointerLog.length < 400) sentinel.pointerLog.push(`${type}:${event.pointerId}:${event.button}:${event.buttons}:${Math.round(event.clientX)},${Math.round(event.clientY)}:${target}`);
  }, { capture: true });
}
document.addEventListener("securitypolicyviolation", (event) => {
  sentinel.cspViolations.push(`${event.violatedDirective}:${event.blockedURI}:${event.sourceFile ?? ""}:${event.lineNumber}`);
});
// Transport construction witness: no transport may be constructed on any shell path.
const NativeWebSocket = window.WebSocket;
(window as unknown as { WebSocket: unknown }).WebSocket = class CountingSocket {
  constructor(..._args: unknown[]) { sentinel.webSockets += 1; throw new Error("the workspace shell must not open a transport"); }
  static readonly CONNECTING = NativeWebSocket.CONNECTING;
};

// A dummy pane: what the unified page occupies at runtime, reduced to the two
// things FW3 cares about — a ResizeObserver that refits presentation only, and
// a send port that layout must never reach.
class DummyPane {
  readonly port = { trySend: (frame: { type: string }): "ACCEPTED" => { sentinel.portCalls.push(frame.type); if (frame.type === "RESIZE_REQUEST") sentinel.resizeRequests += 1; return "ACCEPTED"; } };
  readonly element = document.createElement("div");
  private readonly observer: ResizeObserver;
  constructor(readonly cell: WorkspaceCell) {
    this.element.className = "harness-pane";
    const label = document.createElement("p");
    label.className = "harness-pane__label";
    label.textContent = `${cell.leaf.session.name} · dummy 80×24`;
    const grid = document.createElement("pre");
    grid.className = "harness-pane__grid";
    grid.textContent = Array.from({ length: 24 }, (_row, index) => `${String(index + 1).padStart(2, "0")} ${"·".repeat(77)}`).join("\n");
    this.element.append(label, grid);
    cell.mount.append(this.element);
    this.observer = new ResizeObserver((entries) => {
      sentinel.resizeObserverFires += entries.length;
      // Presentation-only "fitFont": pick a size class from the box width.
      const width = entries[entries.length - 1]?.contentRect.width ?? 0;
      const fit = width < 240 ? "xs" : width < 420 ? "s" : width < 700 ? "m" : "l";
      for (const name of Array.from(grid.classList)) if (name.startsWith("harness-fit-")) grid.classList.remove(name);
      grid.classList.add(`harness-fit-${fit}`);
    });
    this.observer.observe(cell.mount);
  }
  // The ONLY path to the port: an explicit, deliberate fit (the ↕ analogue).
  explicitFit(): void { this.port.trySend({ type: "RESIZE_REQUEST" }); }
  destroy(): void { this.observer.disconnect(); this.element.remove(); }
}

function session(name: string): DashboardSession {
  return { handles: { alias: `a${name}`.padEnd(43, "a"), observe: `o${name}`.padEnd(43, "o"), control: `c${name}`.padEnd(43, "c") }, realm: "main", uid: 1000, server: "default", serverStatus: "ok", sessionId: `$${name}`, name, width: 80, height: 24, attached: 1, activity: 1_700_000_000, aliases: [], draftScope: JSON.stringify(["main", "default", name]), unified: { state: "open", origin: "birth" } };
}

const TREE: WorkspaceNode = split("row", [
  leaf({ realm: "main", server: "default", name: "qt20" }, "offer", "primary-terminal"),
  split("column", [leaf({ realm: "main", server: "default", name: "build" }, "create"), leaf({ realm: "main", server: "default", name: "scratch" }, "skip")]),
  split("column", [leaf({ realm: "main", server: "default", name: "logs" }), split("row", [leaf({ realm: "main", server: "default", name: "deploy" }), leaf({ realm: "main", server: "default", name: "watch" })])]),
], [2, 1, 1]);

// Every §7 state reachable through the typed constructors: each PaneState
// kind, every projection state, and close reasons of every policy class.
function sampleStates(): readonly PaneState[] {
  const states: PaneState[] = [];
  for (const kind of PANE_STATE_KINDS) {
    switch (kind) {
      case "resolving": case "connecting": case "ambiguous": case "duplicate_refused": case "realm_unavailable": case "rate_limited": case "closed": case "not_opened": states.push({ kind }); break;
      case "live": states.push({ kind, columns: 80, rows: 24 }); break;
      case "missing": states.push({ kind, onMissing: "offer" }, { kind, onMissing: "create" }, { kind, onMissing: "skip" }); break;
      case "create_failed": states.push({ kind, code: "name_taken", status: 409 }, { kind, code: "weird", status: 503 }); break;
      case "projection": for (const state of PROJECTION_PANE_STATES) states.push({ kind, state }); break;
      case "reconnecting": states.push(paneStateFromClose("websocket_1006")); break;
      case "reattaching": states.push(paneStateFromClose("input_refused")); break;
      case "takeover_pending": states.push(paneStateFromClose("lease_held")); break;
      case "displaced": states.push(paneStateFromClose("control_displaced"), paneStateFromClose("takeover_superseded")); break;
      case "failed": states.push(paneStateFromClose("attach_failed"), paneStateFromClose("stale_target"), paneStateFromClose("<hostile>")); break;
      default: { const exhaustive: never = kind; throw new Error(`unsampled ${String(exhaustive)}`); }
    }
  }
  // Appended, never spliced: run_workspace_layout_browser.cjs addresses
  // samples by index, so a new state goes on the end.
  // A LIVE pane carrying an inventory detail is its own case: the overlay is
  // hidden and the header badge is display:none, so this is the one state
  // whose copy has only the notice to reach a reader through.
  states.push(paneStateFromProjection({ state: "open", origin: "birth" }), paneStateFromProjection({ state: "open", origin: "birth", detail: "rotation_deferred_alt_screen" }), paneStateFromProjection({ state: "slots_exhausted" }), paneStateFromClose("page_hidden"),
    { kind: "live", columns: 80, rows: 24, detail: "rotation_deferred_alt_screen" });
  return states;
}

type Rect = { x: number; y: number; width: number; height: number };
type NoticeCover = Readonly<{ name: string | undefined; detail: string; noticeHeight: number | null; mountHeight: number | null; share: number | null }>;
function rect(node: Element): Rect { const r = node.getBoundingClientRect(); return { x: r.left, y: r.top, width: r.width, height: r.height }; }
function blankReason(cell: Element, applied?: PaneState): string | undefined {
  const header = cell.querySelector(".ws-cell__header");
  const state = cell.querySelector<HTMLElement>(".ws-cell__state");
  const mount = cell.querySelector<HTMLElement>(".ws-cell__mount");
  const box = rect(cell);
  if (box.width <= 0 || box.height <= 0) return "zero-size cell";
  if (!header || (header.textContent ?? "").trim() === "") return "empty header";
  if (rect(header).height <= 0) return "header not laid out";
  const kind = (cell as HTMLElement).dataset.wsState;
  if (kind === "live") {
    if (!mount || (mount.textContent ?? "").trim() === "") return "live cell with an empty mount";
    if (!state?.hidden) return "live cell still shows its state overlay";
    // A live cell hides its overlay AND its header badge, so a projection
    // detail has only the notice left to speak through. The expectation comes
    // from the state that was APPLIED — a product that renders the detail
    // nowhere also writes no marker, and must not be excused by its own
    // silence.
    const detail = applied !== undefined && applied.kind === "live" ? applied.detail : undefined;
    if (detail === undefined) return undefined;
    const notice = cell.querySelector<HTMLElement>(".ws-cell__notice");
    if (!notice || notice.hidden) return `live+${detail}: no notice`;
    if (rect(notice).height <= 0 || rect(notice).width <= 0) return `live+${detail}: notice not laid out`;
    const text = (notice.textContent ?? "").trim();
    if (text === "") return `live+${detail}: empty notice`;
    const copy = paneStateCopy(applied!);
    if (!text.includes(copy.badge) || !text.includes(copy.detail)) return `live+${detail}: notice does not carry the reviewed copy`;
    // The strip is capped, so the sentence must survive where the cap cannot
    // reach it. Without this a bounded notice could silently lose the words.
    const spoken = `${copy.badge}: ${copy.detail}`;
    if (notice.title !== spoken) return `live+${detail}: notice title does not carry the reviewed sentence`;
    if (notice.getAttribute("aria-label") !== spoken) return `live+${detail}: notice has no accessible name carrying the reviewed sentence`;
    return undefined;
  }
  if (!state || state.hidden) return `${kind}: state slot hidden`;
  if ((state.querySelector(".ws-cell__state-headline")?.textContent ?? "").trim() === "") return `${kind}: empty headline`;
  if ((state.querySelector(".ws-cell__state-detail")?.textContent ?? "").trim() === "") return `${kind}: empty detail`;
  if (rect(state).height <= 0) return `${kind}: state slot not laid out`;
  const buttons = Array.from(state.querySelectorAll("button, a"));
  if (buttons.some((button) => (button.textContent ?? "").trim() === "")) return `${kind}: unlabeled control`;
  return undefined;
}

class Harness {
  readonly host = document.querySelector<HTMLElement>("#ws-host")!;
  private view: WorkspaceLayoutView | undefined;
  private panes = new Map<string, DummyPane>();
  private treeChanges: string[] = [];
  private affordances: string[] = [];
  posture: WorkspacePosture = "desktop";
  readonly states = sampleStates();

  applyPosture(): WorkspacePosture {
    const env = readPostureEnvironment(window);
    const posture = workspacePosture(env);
    this.posture = posture;
    if (posture === "phone") this.mountPhone(); else this.mountDesktop();
    return posture;
  }

  mountDesktop(): void {
    this.teardown();
    this.view = new WorkspaceLayoutView(this.host, TREE, {
      onTreeChange: (tree, cause) => { this.treeChanges.push(`${cause}:${JSON.stringify(serializeWorkspaceTree(tree))}`); },
      onAffordance: (cell, affordance) => { this.affordances.push(`${cell.leaf.session.name}:${affordance}`); },
      onCellRetired: (cell) => { this.panes.get(cell.key)?.destroy(); this.panes.delete(cell.key); },
    });
    for (const cell of this.view.allCells()) this.panes.set(cell.key, new DummyPane(cell));
    this.setStates(0);
  }

  // The honest phone-class state: links minted from a (synthetic) inventory
  // snapshot exactly as the dashboard row mints them; one leaf deliberately
  // unresolved so its state renders beside a missing link.
  mountPhone(): void {
    this.teardown();
    const entries = leafEntries(TREE).map((entry, index) => {
      const missing = entry.leaf.session.name === "scratch";
      return { leaf: entry.leaf, ...(missing ? {} : { href: unifiedTerminalURL(window.location.href, session(entry.leaf.session.name).handles.control, session(entry.leaf.session.name)) }), state: missing ? ({ kind: "missing", onMissing: "skip" } as const) : index === 0 ? ({ kind: "projection", state: "slots_exhausted" } as const) : ({ kind: "resolving" } as const) };
    });
    renderWorkspacePhoneState(this.host, "ops wall", entries);
  }

  mountUnavailable(): void {
    this.teardown();
    renderWorkspaceUnavailable(this.host, workspaceRouteNotice({ code: "fragment_engine_rejected" }));
  }

  private teardown(): void {
    for (const pane of this.panes.values()) pane.destroy();
    this.panes.clear();
    this.view?.destroy();
    this.view = undefined;
    this.host.replaceChildren();
  }

  // Round-robin the sample states over the six cells, offset by `shift`, so
  // every state lands in every cell across the storm.
  setStates(shift: number): string[] {
    if (!this.view) return [];
    const cells = this.view.allCells();
    const applied: string[] = [];
    cells.forEach((cell, index) => { const state = this.states[(index + shift) % this.states.length]; cell.setState(state); applied.push(`${cell.leaf.session.name}=${state.kind}`); });
    return applied;
  }
  setAllStates(kindIndex: number): void {
    if (!this.view) return;
    for (const cell of this.view.allCells()) cell.setState(this.states[kindIndex % this.states.length]);
  }
  showcase(): void {
    if (!this.view) return;
    const picks: PaneState[] = [{ kind: "live", columns: 80, rows: 24 }, { kind: "live", columns: 200, rows: 50 }, { kind: "missing", onMissing: "offer" }, paneStateFromClose("lease_held"), { kind: "projection", state: "blocked_alt_screen" }, { kind: "reconnecting", reason: "websocket_1006" }];
    this.view.allCells().forEach((cell, index) => cell.setState(picks[index % picks.length]));
  }
  resizeHost(preset: "full" | "narrow" | "short" | "tiny"): void {
    for (const name of Array.from(this.host.classList)) if (name.startsWith("harness-host--")) this.host.classList.remove(name);
    this.host.classList.add(`harness-host--${preset}`);
  }
  explicitFit(name: string): void {
    for (const pane of this.panes.values()) if (pane.cell.leaf.session.name === name) pane.explicitFit();
  }
  resetSentinel(): void { sentinel.resizeRequests = 0; sentinel.portCalls = []; }

  audit(): unknown {
    const cells = Array.from(this.host.querySelectorAll<HTMLElement>(".ws-cell"));
    const appliedByElement = new Map<Element, PaneState>();
    for (const cell of this.view?.allCells() ?? []) appliedByElement.set(cell.element, cell.state());
    const dividers = Array.from(this.host.querySelectorAll<HTMLElement>(".ws-divider"));
    const weights = Array.from(this.host.querySelectorAll<HTMLElement>("[data-ws-split]:not(.ws-divider)")).map((container) => ({
      split: container.dataset.wsSplit,
      weights: Array.from(container.children).filter((child) => child.classList.contains("ws-track")).map((child) => Number(Array.from(child.classList).find((name) => name.startsWith("ws-w-"))?.slice(5) ?? "0")),
    }));
    const phone = this.host.querySelector<HTMLElement>(".ws-phone");
    return {
      posture: this.posture,
      coarse: window.matchMedia("(pointer: coarse)").matches,
      viewport: { width: window.innerWidth, height: window.innerHeight },
      host: rect(this.host),
      cellCount: cells.length,
      blanks: cells.map((cell) => ({ name: cell.dataset.wsSession, reason: blankReason(cell, appliedByElement.get(cell)) })).filter((item) => item.reason !== undefined),
      // A live pane's notice sits OVER the terminal, so how much of it the
      // strip hides is a product fact the gate must measure, not a style
      // detail. Reported for every live cell that carries a detail; the gate
      // holds it to a quarter of the mount and to 48 CSS pixels.
      noticeCover: cells.flatMap((cell): NoticeCover[] => {
        const applied = appliedByElement.get(cell);
        if (applied === undefined || applied.kind !== "live" || applied.detail === undefined) return [];
        const notice = cell.querySelector<HTMLElement>(".ws-cell__notice");
        const mount = cell.querySelector<HTMLElement>(".ws-cell__mount");
        if (!notice || !mount) return [{ name: cell.dataset.wsSession, detail: applied.detail, noticeHeight: null, mountHeight: null, share: null }];
        const noticeHeight = Math.round(rect(notice).height);
        const mountHeight = Math.round(rect(mount).height);
        return [{ name: cell.dataset.wsSession, detail: applied.detail, noticeHeight, mountHeight, share: mountHeight > 0 ? noticeHeight / mountHeight : null }];
      }),
      states: cells.map((cell) => `${cell.dataset.wsSession}=${cell.dataset.wsState}`),
      dividers: dividers.map((divider) => ({ split: divider.dataset.wsSplit, index: Number(divider.dataset.wsDivider), orientation: divider.getAttribute("aria-orientation"), valuenow: Number(divider.getAttribute("aria-valuenow")), rect: rect(divider),
        nudges: Array.from(divider.querySelectorAll<HTMLElement>(".ws-divider__nudge")).map((button) => ({ direction: button.dataset.wsNudge, rect: rect(button) })) })),
      actions: Array.from(this.host.querySelectorAll<HTMLElement>(".ws-cell__action")).map((action) => ({ affordance: action.dataset.wsAffordance, rect: rect(action), text: action.textContent })),
      weights,
      treeChanges: this.treeChanges.length,
      lastTreeChange: this.treeChanges[this.treeChanges.length - 1] ?? null,
      affordances: this.affordances,
      styleAttributes: this.host.querySelectorAll("[style]").length,
      phone: phone ? {
        title: phone.querySelector(".ws-phone__title")?.textContent ?? "",
        notice: phone.querySelector(".ws-phone__notice")?.textContent ?? "",
        expectedNotice: PHONE_STATE_NOTICE,
        leaves: Array.from(phone.querySelectorAll<HTMLElement>(".ws-phone__leaf")).map((item) => ({ name: item.dataset.wsSession, state: item.dataset.wsState, stateText: item.querySelector(".ws-phone__leaf-state")?.textContent ?? "", href: item.querySelector<HTMLAnchorElement>(".ws-phone__leaf-link")?.href ?? null, linkRect: item.querySelector(".ws-phone__leaf-link") ? rect(item.querySelector(".ws-phone__leaf-link")!) : null })),
        dashboardRect: phone.querySelector(".ws-phone__dashboard") ? rect(phone.querySelector(".ws-phone__dashboard")!) : null,
      } : null,
      unavailable: this.host.querySelector<HTMLElement>(".ws-unavailable")?.textContent ?? null,
      unavailableDashboardRect: this.host.querySelector(".ws-unavailable__dashboard") ? rect(this.host.querySelector(".ws-unavailable__dashboard")!) : null,
      sentinel: { ...sentinel, cspViolations: [...sentinel.cspViolations], portCalls: [...sentinel.portCalls] },
      sampleStateCount: this.states.length,
    };
  }
}

window.__wsHarness = new Harness();
window.__wsHarness.applyPosture();
document.body.dataset.wsHarnessReady = "true";
