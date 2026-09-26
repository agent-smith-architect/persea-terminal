// The /workspace document: the strict route branch, the landing card
// that never auto-attaches, the honest phone-class state,
// and the desktop runtime — one per-pane controller per
// leaf, one single-flight inventory per workspace, pinned identities, a
// designated-pane focus veto composed over terminal's hook, and the
// workspace layout presentation shell.
//
// An ephemeral arrangement lives in this tab
// (sessionStorage, keyed by the workspace name) and is written exactly once,
// at the trusted "Open workspace" tap. Nothing here writes it on layout
// changes (the workspace editor owns store writes and their debounce).
//
// Seam law: this module imports the shared projection/close-policy types
// through the workspace layout module and never re-declares them; it never emits
// RESIZE_REQUEST; it puts no capability, handle, socket, or incarnation key
// into the tree model; it consumes the page's focus hook as a veto only.
import { installTapFeedback } from "./tap_feedback";
import "./workspace_page.css";
import { csrfToken } from "./csrf_refresh";
import { filterSessionList, unifiedBlockedMessage, unifiedTerminalURL, type DashboardInventory, type DashboardSession, type HistoryChoice } from "./dashboard";
import { ADOPTION_HISTORY_ROWS, readScrollbackRows, readTerminalScrollbackRows } from "./scrollback_preferences";
import { stageSessionImage } from "./image_staging_client";
import { SnippetService, deviceOrigin } from "./snippet_client";
import { pendingIdentityFromSession, writeLastSession } from "./session_memory";
import { OperatorPreferencesService } from "./operator_preferences";
import type { CommitFocusContext } from "./unified_focus_claim";
import { classifyUnifiedClose, type UnifiedInventoryDetail } from "./unified_close_policy";
import { UnifiedPaneController, fetchInventory, type InventoryResolver, type InventorySnapshot, type ResolvedPaneIdentity, type UnifiedPaneControllerState } from "./unified_pane_controller";
import {
  PANE_CAP, addLeaf, leaf, leafEntries, parseWorkspace, removeLeaf, sameSession, serializeWorkspace, sessionKey, setLeafAliasHint, split, splitLeaf, unsplit, validateWorkspaceTree, workspaceRefusalMessage,
  type LeafEntry, type NodePath, type SessionSelector, type SplitDirection, type WorkspaceLeaf, type WorkspaceNode, type WorkspaceResult,
} from "./workspace_model";
import {
  WorkspaceLayoutView, paneStateFromClose, paneStateFromProjection, paneStateCopy, renderWorkspacePhoneState, renderWorkspaceUnavailable, workspacePosture, workspaceRefusalNotice,
  type PaneAffordance, type PaneState, type PhoneLeafEntry, type PostureEnvironment, type WorkspaceCell, type WorkspacePosture,
} from "./workspace_layout";
import { WorkspaceAPI, WorkspaceAPIError, workspaceAPIMessage, type WorkspaceRecord } from "./workspace_api";
import { parseWorkspaceLocation, workspaceRouteNotice, workspaceURL } from "./workspace_url";

// --- Posture. Authored in workspace_posture.ts, which
// the dashboard reads too so that its create affordance and this document's
// decision are the same function of the same inputs.
import { stablePostureEnvironment } from "./workspace_posture";
export { stablePostureEnvironment };

// --- The ephemeral arrangement. Written once at the trusted open.
const EPHEMERAL_PREFIX = "persea-workspace-ephemeral-v1:";

export function readEphemeralArrangement(storage: Pick<Storage, "getItem">, name: string): WorkspaceNode | undefined {
  let raw: string | null;
  try { raw = storage.getItem(EPHEMERAL_PREFIX + name); } catch { return undefined; }
  if (raw === null) return undefined;
  let value: unknown;
  try { value = JSON.parse(raw); } catch { return undefined; }
  const parsed = parseWorkspace(value);
  return parsed.ok ? parsed.value : undefined;
}

export function writeEphemeralArrangement(storage: Pick<Storage, "setItem" | "removeItem">, name: string, tree: WorkspaceNode | undefined): void {
  try {
    if (tree === undefined) storage.removeItem(EPHEMERAL_PREFIX + name);
    else storage.setItem(EPHEMERAL_PREFIX + name, JSON.stringify(serializeWorkspace(tree)));
  } catch { /* the arrangement simply does not survive this tab */ }
}

// Arranges N selected sessions (1..PANE_CAP) into a split tree: up to three in
// one row, otherwise two rows. Weights equal.
export function autoArrange(selectors: readonly SessionSelector[]): WorkspaceNode {
  const leaves = selectors.map((selector) => leaf(selector));
  if (leaves.length === 0) throw new Error("a workspace needs at least one pane");
  if (leaves.length === 1) return leaves[0]!;
  if (leaves.length <= 3) return split("row", leaves);
  const top = Math.ceil(leaves.length / 2);
  return split("column", [split("row", leaves.slice(0, top)), split("row", leaves.slice(top))]);
}

// --- Single-flight inventory.
//
// At most one inventory request is in flight per workspace document. A
// caller that already consumed a handle from snapshot generation g asks for
// a snapshot strictly newer than g: six such callers behind the same stale
// generation share ONE fetch. A caller's abort detaches that caller only;
// the shared fetch completes for the others.
export class WorkspaceInventory {
  private generation = 0;
  private current?: InventorySnapshot;
  private inflight?: Promise<InventorySnapshot>;
  private requests = 0;

  constructor(private readonly fetch: (signal: AbortSignal) => Promise<DashboardInventory> = fetchInventory) {}

  requestCount(): number { return this.requests; }
  latest(): InventorySnapshot | undefined { return this.current; }

  snapshot(signal: AbortSignal | undefined, newerThan: number): Promise<InventorySnapshot> {
    if (this.current && this.current.generation > newerThan) return this.detachable(Promise.resolve(this.current), signal);
    if (!this.inflight) {
      this.requests += 1;
      const shared = new AbortController();
      this.inflight = this.fetch(shared.signal).then((inventory) => {
        this.current = Object.freeze({ generation: ++this.generation, inventory });
        return this.current;
      }).finally(() => { this.inflight = undefined; });
    }
    return this.detachable(this.inflight, signal);
  }

  resolver(): InventoryResolver {
    return (signal, newerThan) => this.snapshot(signal, newerThan);
  }

  private detachable<T>(promise: Promise<T>, signal: AbortSignal | undefined): Promise<T> {
    if (!signal) return promise;
    if (signal.aborted) return Promise.reject(new DOMException("aborted", "AbortError"));
    return new Promise<T>((resolve, reject) => {
      const onAbort = () => reject(new DOMException("aborted", "AbortError"));
      signal.addEventListener("abort", onAbort, { once: true });
      promise.then((value) => { signal.removeEventListener("abort", onAbort); resolve(value); }, (error: unknown) => { signal.removeEventListener("abort", onAbort); reject(error); });
    });
  }
}

// --- Resolution of a leaf against one snapshot.
export type LeafResolution =
  | Readonly<{ kind: "resolved"; identity: ResolvedPaneIdentity; session: DashboardSession; snapshotGeneration: number }>
  | Readonly<{ kind: "missing" }>
  | Readonly<{ kind: "ambiguous" }>
  | Readonly<{ kind: "realm_unavailable" }>;

export function resolveLeaf(snapshot: InventorySnapshot, selector: SessionSelector): LeafResolution {
  const realm = snapshot.inventory.realms.find((candidate) => candidate.name === selector.realm);
  if (!realm) return Object.freeze({ kind: "missing" });
  const server = realm.servers.find((candidate) => candidate.label === selector.server);
  if (!server) return realm.error ? Object.freeze({ kind: "realm_unavailable" }) : Object.freeze({ kind: "missing" });
  if (server.error) return Object.freeze({ kind: "realm_unavailable" });
  const matches = server.sessions.filter((session) => session.name === selector.name);
  if (matches.length === 0) return Object.freeze({ kind: "missing" });
  if (matches.length > 1) return Object.freeze({ kind: "ambiguous" });
  const session = matches[0]!;
  return Object.freeze({
    kind: "resolved",
    identity: Object.freeze({ selector: Object.freeze({ ...selector }), incarnationKey: session.draftScope, sessionId: session.sessionId }),
    session,
    snapshotGeneration: snapshot.generation,
  });
}

function resolutionPaneState(resolution: LeafResolution, leafNode: WorkspaceLeaf): PaneState {
  switch (resolution.kind) {
    case "resolved": return resolution.session.unified ? paneStateFromProjection(resolution.session.unified) : Object.freeze({ kind: "projection", state: "unavailable" });
    case "missing": return Object.freeze({ kind: "missing", onMissing: leafNode.onMissing });
    case "ambiguous": return Object.freeze({ kind: "ambiguous" });
    case "realm_unavailable": return Object.freeze({ kind: "realm_unavailable" });
  }
}

// --- DOM helpers (class-based only).
function element<K extends keyof HTMLElementTagNameMap>(tag: K, className?: string, text?: string): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

// --- Boot.

export type WorkspaceBootOptions = Readonly<{
  root: HTMLElement;
  styleNonce: string;
  win?: Window;
  historyRows?: HistoryChoice;
}>;

export async function bootWorkspace(options: WorkspaceBootOptions): Promise<void> {
  const win = options.win ?? window;
  const { root, styleNonce } = options;
  const verdict = parseWorkspaceLocation(win.location);
  if (!verdict.ok) {
    // A typed refusal, rendered in place. Never a redirect and never the
    // terminal self-heal (FW-R cases c/d).
    renderWorkspaceUnavailable(root, workspaceRouteNotice(verdict.refusal));
    return;
  }
  const name = verdict.name;
  const posture = workspacePosture(stablePostureEnvironment(win));
  const inventory = new WorkspaceInventory();
  const workspaceAPI = new WorkspaceAPI((input, init) => win.fetch(input, init));
  let record: WorkspaceRecord;
  try {
    const list = await workspaceAPI.list();
    const match = list.items.filter((candidate) => candidate.name === name);
    if (match.length !== 1) throw new WorkspaceAPIError(match.length === 0 ? "not_found" : "unavailable", match.length === 0 ? 404 : 503);
    record = match[0]!;
  } catch (error) {
    renderWorkspaceUnavailable(root, workspaceAPIMessage(error));
    return;
  }
  // The durable record is the sole workspace authority. A same-name ephemeral
  // arrangement is deliberately not read, merged or deleted here.
  const arrangement = record.tree;
  document.body.classList.add("ws-document");
  let snapshot: InventorySnapshot;
  try {
    snapshot = await inventory.snapshot(undefined, -1);
  } catch {
    renderWorkspaceUnavailable(root, Object.freeze({ headline: "This workspace cannot be opened", detail: "The current sessions could not be read. Retry from the dashboard.", code: "inventory_unavailable" }));
    return;
  }
  if (posture === "phone") {
    renderPhone(root, record.name, arrangement, snapshot, win);
    return;
  }
  const page = new WorkspacePage({ root, styleNonce, name: record.name, win, inventory, historyRows: options.historyRows ?? readScrollbackRows(), record, workspaceAPI });
  page.landing(arrangement, snapshot);
}

// The honest phone-class state: the workspace name, the
// notice, one single-terminal link per resolved leaf with its resolution
// state beside it. Zero transports, zero xterms.
function renderPhone(root: HTMLElement, name: string, arrangement: WorkspaceNode | undefined, snapshot: InventorySnapshot, win: Window): void {
  const entries: PhoneLeafEntry[] = arrangement === undefined ? [] : leafEntries(arrangement).map((entry) => {
    const resolution = resolveLeaf(snapshot, entry.leaf.session);
    const linkable = resolution.kind === "resolved" && resolution.session.unified !== undefined
      && (resolution.session.unified.state === "open" || resolution.session.unified.state === "adoptable");
    // A session that could be attached is "not opened on this device" here —
    // never "connecting": this posture opens no transport. Missing,
    // ambiguous, and blocked projections stay what they are: inventory facts.
    const state: PaneState = linkable ? Object.freeze({ kind: "not_opened" }) : resolutionPaneState(resolution, entry.leaf);
    return Object.freeze({
      leaf: entry.leaf,
      state,
      ...(linkable ? { href: unifiedTerminalURL(win.location.href, resolution.session.handles.control, resolution.session) } : {}),
    });
  });
  renderWorkspacePhoneState(root, name, entries);
}

// --- The desktop workspace page.

type PaneRuntime = {
  readonly cell: WorkspaceCell;
  leaf: WorkspaceLeaf;
  controller?: UnifiedPaneController;
  identity?: ResolvedPaneIdentity;
  columns: number;
  rows: number;
  live: boolean;
  adoptAttempts: number;
  lastClose?: string;
  transportGeneration: number;
  projectionDetail?: UnifiedInventoryDetail;
};

const PRESENTATION_REFRESH_MS = 30_000;

export type WorkspacePageOptions = Readonly<{
  root: HTMLElement;
  styleNonce: string;
  name: string;
  win: Window;
  inventory: WorkspaceInventory;
  historyRows: HistoryChoice;
  record?: WorkspaceRecord;
  workspaceAPI?: WorkspaceAPI;
}>;

const ADOPT_MAX_ATTEMPTS = 6;
const ADOPT_BASE_DELAY_MS = 250;
const ADOPT_MAX_DELAY_MS = 4_000;

export class WorkspacePage {
  private readonly root: HTMLElement;
  private readonly win: Window;
  private readonly inventory: WorkspaceInventory;
  private view?: WorkspaceLayoutView;
  private readonly panes = new Map<string, PaneRuntime>();
  // The designated pane: the only leaf whose COMMIT may claim focus (and only
  // when the page's own pointer rule would have claimed it). Initially the
  // first leaf in reading order; follows the operator's own focus moves.
  private designated?: string;
  private opened = false;
  private torndown = false;
  // ONE snippets/clips service for the whole workspace document (clipboard): six
  // panes subscribe to it and share its single poll loop, and each pane's own
  // controller is what routes a snippet action to that pane's terminal.
  // Construction is inert — nothing is read until a pane opens a list.
  private readonly snippets: SnippetService;
  private record?: WorkspaceRecord;
  private draft?: WorkspaceNode;
  private draftName?: string;
  private editing = false;
  private workspaceTitle?: HTMLElement;
  private editor?: HTMLElement;
  private editorStatus?: HTMLElement;
  private presentationGeneration = 0;
  private presentationTimer?: number;
  private readonly preferences: OperatorPreferencesService;

  constructor(private readonly options: WorkspacePageOptions) {
    this.root = options.root;
    this.win = options.win;
    this.inventory = options.inventory;
    this.snippets = new SnippetService({
      origin: deviceOrigin(options.win),
      ...(typeof navigator !== "undefined" && navigator.clipboard ? { clipboard: navigator.clipboard } : {}),
    });
    this.record = options.record;
    // The service itself probes localStorage defensively. Accessing the getter
    // here would let a browser privacy refusal abort the whole workspace before
    // the server/default fallback can render.
    this.preferences = new OperatorPreferencesService();
    // Preferences saved in another tab (the dashboard's Appearance card)
    // reach every pane of this workspace without a reload.
    this.preferences.watchExternalChanges({ refetchOnForeground: true });
    installTapFeedback(document);
  }

  // The landing card (B3): the arrangement's leaves with their resolution
  // states, a session picker, and one primary action. Nothing attaches,
  // adopts, mints, or upgrades until a TRUSTED tap on that action.
  landing(arrangement: WorkspaceNode | undefined, snapshot: InventorySnapshot): void {
    const { name } = this.options;
    const durable = this.options.record !== undefined;
    const panel = element("section", "ws-landing");
    panel.setAttribute("aria-label", `Workspace ${name}`);
    panel.dataset.wsLanding = arrangement === undefined ? "pick" : "resume";
    panel.append(element("p", "ws-landing__eyebrow", "Workspace"), element("h1", "ws-landing__title", name));
    panel.append(element("p", "ws-landing__notice", arrangement === undefined
      ? `Pick up to ${PANE_CAP} sessions to open side by side. Nothing attaches until you open the workspace.`
      : durable
        ? "This saved workspace is not attached. Open it to attach its panes; edit it only after opening."
        : "This workspace is not attached. Open it to attach its panes; nothing attaches until you do."));
    const selected = new Map<string, SessionSelector>();
    if (arrangement !== undefined) for (const entry of leafEntries(arrangement)) selected.set(entry.key, entry.leaf.session);
    const sessions = snapshot.inventory.realms.flatMap((realm) => realm.servers.flatMap((server) => server.sessions));
    const filter = element("input", "ws-landing__filter");
    filter.type = "search";
    filter.placeholder = "Filter sessions";
    filter.setAttribute("aria-label", "Filter sessions");
    const list = element("ul", "ws-landing__sessions");
    const status = element("p", "ws-landing__status");
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    const open = element("button", "ws-landing__open");
    open.type = "button";
    const renderOpen = () => {
      const count = selected.size;
      open.textContent = count === 0 ? "Open workspace" : `Open workspace (${count} ${count === 1 ? "pane" : "panes"})`;
      open.disabled = count === 0;
    };
    const renderList = () => {
      const query = filter.value;
      list.replaceChildren(...filterSessionList(sessions, query).map((session) => {
        const selector: SessionSelector = Object.freeze({ realm: session.realm, server: session.server, name: session.name });
        const key = sessionKey(selector);
        const item = element("li", "ws-landing__session");
        item.dataset.wsSession = session.name;
        const label = element("label", "ws-landing__session-label");
        const box = element("input", "ws-landing__session-box");
        box.type = "checkbox";
        box.checked = selected.has(key);
        const openable = session.unified !== undefined && (session.unified.state === "open" || session.unified.state === "adoptable");
        const duplicate = [...selected.values()].some((other) => sameSession(other, selector));
        box.disabled = durable || (!openable && !duplicate);
        box.addEventListener("change", () => {
          if (box.checked) {
            if (selected.size >= PANE_CAP) {
              box.checked = false;
              status.textContent = `A workspace opens at most ${PANE_CAP} panes.`;
              return;
            }
            selected.set(key, selector);
          } else {
            selected.delete(key);
          }
          status.textContent = "";
          renderOpen();
        });
        label.append(box, element("span", "ws-landing__session-name", session.name), element("span", "ws-landing__session-scope", `${session.realm} · ${session.server}`));
        const state = session.unified === undefined
          ? "unified terminal unavailable"
          : session.unified.state === "open" || session.unified.state === "adoptable" ? session.unified.state : unifiedBlockedMessage(session.unified.state);
        label.append(element("span", "ws-landing__session-state", state));
        item.append(label);
        return item;
      }));
      // Leaves of the arrangement that the snapshot no longer lists stay
      // visible with their resolution state, so a resume never hides a pane.
      if (arrangement !== undefined) {
        for (const entry of leafEntries(arrangement)) {
          const resolution = resolveLeaf(snapshot, entry.leaf.session);
          if (resolution.kind === "resolved") continue;
          const item = element("li", "ws-landing__session ws-landing__session--unresolved");
          item.dataset.wsSession = entry.leaf.session.name;
          const copy = paneStateCopy(resolutionPaneState(resolution, entry.leaf));
          item.append(element("span", "ws-landing__session-name", entry.leaf.session.name), element("span", "ws-landing__session-scope", `${entry.leaf.session.realm} · ${entry.leaf.session.server}`), element("span", "ws-landing__session-state", copy.badge));
          list.append(item);
        }
      }
    };
    filter.addEventListener("input", renderList);
    open.addEventListener("click", (event) => {
      // Only a trusted in-page tap opens the workspace: a restored or
      // scripted document never attaches by itself (B3).
      if (!event.isTrusted || open.disabled) return;
      const selectors = [...selected.values()];
      if (selectors.length === 0) return;
      const tree = durable && arrangement !== undefined
        ? arrangement
        : arrangement !== undefined && sameLeafSet(arrangement, selectors) ? arrangement : autoArrange(selectors);
      const verdict = validateWorkspaceTree(tree);
      if (!verdict.ok) {
        renderWorkspaceUnavailable(this.root, workspaceRefusalNotice(verdict.refusal));
        return;
      }
      if (!durable) writeEphemeralArrangement(this.win.sessionStorage, name, tree);
      open.disabled = true;
      void this.open(tree);
    });
    const dashboard = element("a", "ws-landing__dashboard", "Dashboard");
    dashboard.href = "/";
    const actions = element("div", "ws-landing__actions");
    actions.append(open, dashboard);
    panel.append(filter, list, status, actions);
    renderList();
    renderOpen();
    this.root.replaceChildren(panel);
  }

  // The trusted open: render every cell in a visible resolving state at
  // once, then attach every leaf independently and concurrently (no
  // staggering).
  async open(tree: WorkspaceNode): Promise<void> {
    if (this.opened || this.torndown) return;
    this.opened = true;
    const entries = leafEntries(tree);
    this.root.replaceChildren();
    this.root.classList.add("ws-document__root");
    const layoutHost = element("main", "ws-layout-host");
    if (this.record && this.options.workspaceAPI) {
      const header = element("header", "ws-workspace-header");
      this.workspaceTitle = element("h1", "ws-workspace-title", this.record.name);
      const edit = element("button", "ws-workspace-edit", "Edit workspace");
      edit.type = "button";
      edit.addEventListener("click", () => this.beginEditing());
      const dashboard = element("a", "ws-workspace-dashboard", "Dashboard");
      dashboard.href = "/";
      header.append(this.workspaceTitle, edit, dashboard);
      this.editor = element("section", "ws-editor");
      this.editor.hidden = true;
      this.root.append(header, this.editor, layoutHost);
    } else {
      this.root.append(layoutHost);
    }
    const view = new WorkspaceLayoutView(layoutHost, tree, {
      onAffordance: (cell, affordance) => { void this.affordance(cell, affordance); },
      onCellRetired: (cell) => this.retire(cell),
      onTreeChange: (next) => {
        if (!this.editing) return;
        this.draft = next;
        if (this.editorStatus) this.editorStatus.textContent = "Local draft changed. Save to make it durable.";
      },
    });
    view.setEditing(false);
    this.view = view;
    this.designated = entries[0]?.key;
    this.root.addEventListener("focusin", (event) => this.onFocusIn(event));
    // A page entering the back/forward cache only suspends its panes, so a
    // restored page can reattach them; a page that is really unloading tears
    // everything down.
    this.win.addEventListener("pagehide", (event) => {
      if (event.persisted) this.suspend();
      else this.teardown("page_hidden");
    });
    this.win.addEventListener("pageshow", (event) => { if (event.persisted) this.resume(); });
    for (const entry of entries) {
      const cell = view.cell(entry.key);
      if (!cell) continue;
      const pane: PaneRuntime = { cell, leaf: entry.leaf, columns: 0, rows: 0, live: false, adoptAttempts: 0, transportGeneration: 0 };
      this.panes.set(entry.key, pane);
      cell.setState(Object.freeze({ kind: "resolving" }));
    }
    this.applyFocusMarks();
    await this.preferences.load();
    const snapshot = this.inventory.latest() ?? await this.inventory.snapshot(undefined, -1);
    this.presentationGeneration = snapshot.generation;
    await Promise.all(entries.map((entry) => this.attachLeaf(entry, snapshot)));
    if (!this.torndown) this.presentationTimer = this.win.setInterval(() => { void this.refreshPresentation(); }, PRESENTATION_REFRESH_MS);
  }

  // One shared inventory refresh updates only presentation detail on the
  // already mounted exact-incarnation controllers. It cannot create, replace,
  // reconnect, mint, take over, send input, resize, or mutate a terminal.
  async refreshPresentation(snapshot?: InventorySnapshot): Promise<void> {
    if (this.torndown) return;
    let current = snapshot;
    if (!current) {
      try {
        current = await this.inventory.snapshot(undefined, this.inventory.latest()?.generation ?? -1);
      } catch {
        return;
      }
    }
    if (current.generation <= this.presentationGeneration) return;
    this.presentationGeneration = current.generation;
    for (const pane of this.panes.values()) {
      const controller = pane.controller;
      const identity = pane.identity;
      if (!controller || !identity) continue;
      // Resolve the pane's CURRENT identity, not its durable leaf. An
      // in-place session switch replaces the running incarnation while the
      // leaf keeps naming the arrangement's session, so resolving the leaf
      // makes the exact-incarnation test permanently false for a switched
      // pane and its rotation-deferred detail never arrives. The exactness
      // test itself is unchanged: a resolution that no longer names this
      // controller's incarnation projects no detail, and the controller
      // refuses any key but its own (workspace rotationC).
      const resolution = resolveLeaf(current, identity.selector);
      const exact = resolution.kind === "resolved" && resolution.identity.incarnationKey === identity.incarnationKey;
      const projection = exact ? resolution.session.unified : undefined;
      const detail = projection?.state === "open" ? projection.detail : undefined;
      controller.updateProjectionDetail(identity.incarnationKey, detail);
    }
  }

  // Resolves one leaf against the given snapshot and, when it names exactly
  // one openable incarnation, pins that identity and opens the pane's own
  // controller. Every outcome renders in the cell; a sibling is never touched.
  private async attachLeaf(entry: LeafEntry, snapshot: InventorySnapshot): Promise<void> {
    const pane = this.panes.get(entry.key);
    if (!pane || this.torndown) return;
    const resolution = resolveLeaf(snapshot, entry.leaf.session);
    pane.cell.setState(resolutionPaneState(resolution, entry.leaf));
    if (resolution.kind !== "resolved") return;
    const { session } = resolution;
    const projection = session.unified;
    if (!projection || (projection.state !== "open" && projection.state !== "adoptable")) return;
    pane.identity = resolution.identity;
    if (projection.state === "adoptable") {
      const adopted = await this.adopt(pane, resolution.identity);
      if (!adopted || this.torndown) return;
    }
    this.mountController(entry.key, pane, resolution);
  }

  // Adoption is the one pre-attach mutation a leaf may perform, and it is per
  // pane: a 429 or a refusal delays or fails THIS pane only.
  private async adopt(pane: PaneRuntime, identity: ResolvedPaneIdentity): Promise<boolean> {
    while (!this.torndown) {
      pane.adoptAttempts += 1;
      let status = 0;
      try {
        const response = await this.win.fetch("/api/session-adoptions", {
          method: "POST", cache: "no-store", credentials: "same-origin",
          headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrfToken() },
          body: JSON.stringify({ realm: identity.selector.realm, server: identity.selector.server, session_id: identity.sessionId, history_rows: ADOPTION_HISTORY_ROWS }),
        });
        status = response.status;
        if (response.ok) return true;
      } catch {
        status = 0;
      }
      if (status === 429 && pane.adoptAttempts < ADOPT_MAX_ATTEMPTS) {
        pane.cell.setState(Object.freeze({ kind: "rate_limited" }));
        const exponential = Math.min(ADOPT_MAX_DELAY_MS, ADOPT_BASE_DELAY_MS * (2 ** (pane.adoptAttempts - 1)));
        await new Promise((resolve) => this.win.setTimeout(resolve, Math.round(exponential * (0.8 + Math.random() * 0.4))));
        continue;
      }
      pane.cell.setState(Object.freeze({ kind: "failed", reason: status === 429 ? "rate_limited" : "attach_failed" }));
      return false;
    }
    return false;
  }

  private mountController(key: string, pane: PaneRuntime, resolution: Extract<LeafResolution, { kind: "resolved" }>): void {
    const { session, identity } = resolution;
    const { styleNonce } = this.options;
    let historyRows = readTerminalScrollbackRows(identity.incarnationKey, this.options.historyRows);
    pane.projectionDetail = session.unified?.state === "open" ? session.unified.detail : undefined;
    const connecting = () => pane.cell.setState(Object.freeze({ kind: "connecting" as const, ...(pane.projectionDetail ? { detail: pane.projectionDetail } : {}) }));
    const live = () => pane.cell.setState(Object.freeze({ kind: "live" as const, columns: pane.columns, rows: pane.rows, ...(pane.projectionDetail ? { detail: pane.projectionDetail } : {}) }));
    connecting();
    const controller = new UnifiedPaneController({
      root: pane.cell.mount, styleNonce, capabilityMode: "control", engine: "unified-dev", historyRows,
      onScrollbackChanged: rows => { historyRows = rows; },
      initialHandle: session.handles.control,
      incarnationKey: identity.incarnationKey,
      initialIdentity: identity,
      resolveInventory: this.inventory.resolver(),
      initialSnapshotGeneration: resolution.snapshotGeneration,
      sessionName: session.name,
      snippets: this.snippets,
      ...(session.aliases[0]?.displayAlias ? { aliasLabel: session.aliases[0].displayAlias } : {}),
      ...(session.canStageImages ? { imageRealm: session.realm } : {}),
      preferences: this.preferences,
      workspaceCell: true,
      ...(session.canStageImages ? { stageImage: (file: File, signal: AbortSignal) => stageSessionImage(session.realm, file, signal) } : {}),
      stageImageForRealm: (realm: string, file: File, signal: AbortSignal) => stageSessionImage(realm, file, signal),
      // No single-terminal reload key: one sessionStorage key cannot carry N
      // handles, and a workspace reload re-resolves at the load boundary.
      onSessionCommitted: (committed) => {
        const remembered = pendingIdentityFromSession(committed);
        if (remembered) writeLastSession(this.win.localStorage, Object.freeze({ ...remembered, at: Date.now() }));
      },
      onIdentityReplaced: (next) => { pane.identity = next; },
      // terminal's hook is a VETO: the page's pointer rule decides whether a
      // COMMIT may claim focus at all; this policy only withholds it from
      // every pane but the designated one.
      claimFocusOnCommit: (context: CommitFocusContext) => context.pointerRuleClaims && key === this.designated,
      observer: {
        openTransport: (generation) => {
          pane.transportGeneration = generation;
          pane.live = false;
          connecting();
        },
        frame: (_generation, frame, verdict) => {
          if (verdict !== "ENQUEUED") return;
          if (frame.type === "PREPARE" && frame.columns !== undefined && frame.rows !== undefined) {
            pane.columns = frame.columns;
            pane.rows = frame.rows;
            if (pane.live) live();
          } else if (frame.type === "COMMIT") {
            pane.live = true;
            live();
          } else if (frame.type === "END") {
            // An in-band END is the attachment's end on an open socket (the
            // session's epoch closed: "closed" | "canceled" | "fault"). The
            // page routes it through transportClosed; the cell classifies
            // the same reason through the shared policy.
            pane.live = false;
            pane.lastClose = frame.reason ?? "attachment_failed";
            pane.cell.setState(paneStateFromClose(frame.reason ?? "attachment_failed"));
          }
        },
        transportClosed: (_generation, reason) => {
          pane.live = false;
          pane.lastClose = reason;
          pane.cell.setState(paneStateFromClose(reason));
        },
        reconnectStatus: (status) => {
          // OFFLINE keeps probing on its own; the pane says so and offers
          // an immediate retry beside it.
          if (status.state === "OFFLINE") pane.cell.setState(Object.freeze({ kind: "failed", reason: "reconnect_offline" }));
          // The page's burst limiter stopped a reattach loop: without this the
          // cell would keep saying "Reattaching" with nothing running.
          if (status.state === "DETACHED" && status.reason !== undefined && classifyUnifiedClose(status.reason) === "reattach") {
            pane.cell.setState(Object.freeze({ kind: "failed", reason: status.reason }));
          }
          if (status.state === "EXHAUSTED") pane.cell.setState(Object.freeze({ kind: "failed", reason: status.reason ?? "reconnect_exhausted" }));
        },
        projectionDetail: (detail) => {
          pane.projectionDetail = detail;
          const state = pane.cell.state();
          if (state.kind === "live") live();
          else if (state.kind === "connecting") connecting();
        },
      },
    });
    pane.controller = controller;
    controller.updateProjectionDetail(identity.incarnationKey, pane.projectionDetail);
    controller.connect();
  }

  private async affordance(cell: WorkspaceCell, affordance: PaneAffordance): Promise<void> {
    const pane = this.panes.get(cell.key);
    if (!pane || this.torndown) return;
    switch (affordance) {
      case "create": {
        await this.createSession(pane);
        return;
      }
      case "skip": {
        pane.cell.setState(Object.freeze({ kind: "missing", onMissing: "skip" }));
        return;
      }
      case "retry": {
        // Explicit operator intent: a pane that never
        // pinned an identity resolves again; a pane that did re-attaches on
        // its pinned identity and never crosses to a same-name replacement.
        if (pane.controller) {
          pane.controller.dispose("retry");
          pane.controller = undefined;
        }
        pane.identity = undefined;
        pane.live = false;
        pane.cell.setState(Object.freeze({ kind: "resolving" }));
        const snapshot = await this.inventory.snapshot(undefined, this.inventory.latest()?.generation ?? -1);
        const entry = leafEntries(this.view?.tree() ?? pane.leaf).find((candidate) => candidate.key === cell.key);
        if (entry) await this.attachLeaf(entry, snapshot);
        return;
      }
      case "take_control": {
        pane.controller?.requestControlTakeover();
        return;
      }
      case "dashboard":
        return;
    }
  }

  private async createSession(pane: PaneRuntime): Promise<void> {
    const { session } = pane.leaf;
    pane.cell.setState(Object.freeze({ kind: "resolving" }));
    let response: Response;
    try {
      response = await this.win.fetch("/api/sessions", {
        method: "POST", cache: "no-store", credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrfToken() },
        body: JSON.stringify({ realm: session.realm, server: session.server, name: session.name }),
      });
    } catch {
      pane.cell.setState(Object.freeze({ kind: "create_failed", code: "unreachable", status: 0 }));
      return;
    }
    if (response.status !== 201) {
      const code = (await response.text().catch(() => "")).trim();
      pane.cell.setState(Object.freeze({ kind: "create_failed", code: /^[a-z][a-z0-9_]{0,63}$/.test(code) ? code : "create_failed", status: response.status }));
      return;
    }
    // One shared snapshot refresh; every other unresolved leaf may reuse it.
    const snapshot = await this.inventory.snapshot(undefined, this.inventory.latest()?.generation ?? -1);
    const entry = leafEntries(this.view?.tree() ?? pane.leaf).find((candidate) => candidate.key === pane.cell.key);
    if (entry) await this.attachLeaf(entry, snapshot);
  }

  private retire(cell: WorkspaceCell): void {
    const pane = this.panes.get(cell.key);
    if (!pane) return;
    pane.controller?.dispose("retired");
    this.panes.delete(cell.key);
    if (this.designated === cell.key) this.designated = this.panes.keys().next().value;
  }

  private onFocusIn(event: FocusEvent): void {
    const target = event.target;
    if (!(target instanceof Element)) return;
    const cellElement = target.closest(".ws-cell");
    if (!(cellElement instanceof HTMLElement)) return;
    for (const pane of this.panes.values()) {
      if (pane.cell.element === cellElement) {
        this.designated = pane.cell.key;
        break;
      }
    }
    this.applyFocusMarks();
  }

  private applyFocusMarks(): void {
    for (const pane of this.panes.values()) {
      if (pane.cell.key === this.designated) pane.cell.element.dataset.wsDesignated = "true";
      else delete pane.cell.element.dataset.wsDesignated;
    }
  }

  private beginEditing(): void {
    if (this.editing || !this.record || !this.options.workspaceAPI || !this.view) return;
    this.editing = true;
    this.draft = this.record.tree;
    this.draftName = this.record.name;
    this.view.setEditing(true);
    this.renderEditor();
  }

  private cancelEditing(): void {
    if (!this.editing || !this.record || !this.view) return;
    this.editing = false;
    this.draft = undefined;
    this.draftName = undefined;
    // Divider weights are the only draft mutation reflected in the live DOM.
    // Restoring the durable tree keeps every keyed cell/controller intact.
    this.view.update(this.record.tree);
    this.view.setEditing(false);
    if (this.editor) this.editor.hidden = true;
  }

  private applyDraft(result: WorkspaceResult<WorkspaceNode>): void {
    if (!result.ok) {
      if (this.editorStatus) this.editorStatus.textContent = workspaceRefusalMessage(result.refusal);
      return;
    }
    this.draft = result.value;
    this.renderEditor();
    if (this.editorStatus) this.editorStatus.textContent = "Local draft changed. Save to make it durable.";
  }

  private availableSessions(tree: WorkspaceNode): readonly SessionSelector[] {
    const selected = leafEntries(tree).map((entry) => entry.leaf.session);
    const snapshot = this.inventory.latest();
    if (!snapshot) return [];
    return snapshot.inventory.realms.flatMap((realm) => realm.servers.flatMap((server) => server.sessions))
      .filter((session) => session.unified !== undefined && (session.unified.state === "open" || session.unified.state === "adoptable"))
      .map((session) => Object.freeze({ realm: session.realm, server: session.server, name: session.name }))
      .filter((selector) => !selected.some((candidate) => sameSession(candidate, selector)));
  }

  private sessionPicker(candidates: readonly SessionSelector[], label: string): HTMLSelectElement {
    const select = element("select", "ws-editor__select");
    select.setAttribute("aria-label", label);
    candidates.forEach((selector, index) => {
      const option = element("option");
      option.value = String(index);
      option.textContent = `${selector.name} — ${selector.realm} · ${selector.server}`;
      select.append(option);
    });
    select.disabled = candidates.length === 0;
    return select;
  }

  private renderEditor(): void {
    const editor = this.editor;
    const draft = this.draft;
    if (!editor || !draft || !this.record) return;
    editor.hidden = false;
    editor.setAttribute("aria-label", "Edit workspace");
    const heading = element("h2", "ws-editor__heading", "Edit workspace");
    const nameLabel = element("label", "ws-editor__field", "Workspace name");
    const name = element("input", "ws-editor__input");
    name.name = "workspace_name";
    name.value = this.draftName ?? this.record.name;
    name.addEventListener("input", () => { this.draftName = name.value; });
    nameLabel.append(name);
    const status = element("output", "ws-editor__status");
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    this.editorStatus = status;
    const save = element("button", "ws-editor__save", "Save");
    save.type = "button";
    save.addEventListener("click", () => { void this.saveDraft(save); });
    const cancel = element("button", "ws-editor__cancel", "Cancel");
    cancel.type = "button";
    cancel.addEventListener("click", () => this.cancelEditing());
    const actions = element("div", "ws-editor__actions");
    actions.append(save, cancel);

    const candidates = this.availableSessions(draft);
    const add = element("fieldset", "ws-editor__add");
    add.append(element("legend", undefined, "Add pane"));
    const addPicker = this.sessionPicker(candidates, "Session to add");
    const addRow = element("button", "ws-editor__add-row", "Add beside");
    const addColumn = element("button", "ws-editor__add-column", "Add below");
    for (const [button, direction] of [[addRow, "row"], [addColumn, "column"]] as const) {
      button.type = "button";
      button.disabled = candidates.length === 0 || leafEntries(draft).length >= PANE_CAP;
      button.addEventListener("click", () => {
        const selector = candidates[Number(addPicker.value)];
        if (selector) this.applyDraft(addLeaf(this.draft ?? draft, leaf(selector), direction));
      });
    }
    add.append(addPicker, addRow, addColumn);

    const panes = element("ol", "ws-editor__panes");
    for (const entry of leafEntries(draft)) {
      const item = element("li", "ws-editor__pane");
      item.dataset.wsSession = entry.leaf.session.name;
      item.append(element("strong", "ws-editor__pane-name", entry.leaf.session.name));
      const aliasLabel = element("label", "ws-editor__alias", "Display label");
      const alias = element("input", "ws-editor__input");
      alias.value = entry.leaf.aliasHint ?? "";
      const setAlias = element("button", "ws-editor__alias-save", "Set label");
      setAlias.type = "button";
      setAlias.addEventListener("click", () => this.applyDraft(setLeafAliasHint(this.draft ?? draft, entry.path, alias.value === "" ? undefined : alias.value)));
      aliasLabel.append(alias, setAlias);
      item.append(aliasLabel);
      const remove = element("button", "ws-editor__remove", "Remove pane");
      remove.type = "button";
      remove.disabled = leafEntries(draft).length === 1;
      remove.addEventListener("click", () => this.applyDraft(removeLeaf(this.draft ?? draft, entry.path)));
      item.append(remove);
      if (candidates.length > 0 && leafEntries(draft).length < PANE_CAP) {
        const splitPicker = this.sessionPicker(candidates, `Session to split beside ${entry.leaf.session.name}`);
        item.append(splitPicker);
        for (const [copy, direction] of [["Split beside", "row"], ["Split below", "column"]] as const) {
          const button = element("button", "ws-editor__split", copy);
          button.type = "button";
          button.addEventListener("click", () => {
            const selector = candidates[Number(splitPicker.value)];
            if (selector) this.applyDraft(splitLeaf(this.draft ?? draft, entry.path, direction, leaf(selector)));
          });
          item.append(button);
        }
      }
      panes.append(item);
    }

    const splits = element("div", "ws-editor__splits");
    const walk = (node: WorkspaceNode, path: NodePath): void => {
      if (node.kind === "leaf") return;
      const group = element("div", "ws-editor__unsplit");
      group.append(element("span", undefined, `Collapse ${node.direction} split`));
      node.children.forEach((_child, index) => {
        const keep = element("button", "ws-editor__keep", `Keep pane group ${index + 1}`);
        keep.type = "button";
        keep.addEventListener("click", () => this.applyDraft(unsplit(this.draft ?? draft, path, index)));
        group.append(keep);
      });
      splits.append(group);
      node.children.forEach((child, index) => walk(child, [...path, index]));
    };
    walk(draft, []);
    editor.replaceChildren(heading, nameLabel, add, panes, splits, status, actions);
  }

  private async saveDraft(button: HTMLButtonElement): Promise<void> {
    const api = this.options.workspaceAPI;
    const record = this.record;
    const draft = this.draft;
    const name = this.draftName ?? record?.name;
    if (!api || !record || !draft || !name || !this.editorStatus) return;
    button.disabled = true;
    this.editorStatus.textContent = "Saving workspace…";
    try {
      const next = await api.update(record, name, draft);
      const renamed = next.name !== record.name;
      this.record = next;
      this.editing = false;
      this.draft = undefined;
      this.draftName = undefined;
      this.updateTree(next.tree);
      this.view?.setEditing(false);
      if (this.workspaceTitle) this.workspaceTitle.textContent = next.name;
      if (this.editor) this.editor.hidden = true;
      if (renamed) this.win.history.replaceState(this.win.history.state, "", workspaceURL(this.win.location.href, next.name));
    } catch (error) {
      const copy = workspaceAPIMessage(error, "saved");
      this.editorStatus.textContent = `${copy.headline}. ${copy.detail} (${copy.code})`;
      if (error instanceof WorkspaceAPIError && error.code === "conflict" && error.current && this.editor) {
        const conflict = element("div", "ws-editor__conflict");
        const reload = element("button", "ws-editor__reload", "Reload saved version");
        const keep = element("button", "ws-editor__keep-editing", "Keep editing");
        reload.type = "button"; keep.type = "button";
        reload.addEventListener("click", () => {
          this.record = error.current;
          this.draft = error.current!.tree;
          this.draftName = error.current!.name;
          this.updateTree(error.current!.tree);
          this.renderEditor();
          if (this.editorStatus) this.editorStatus.textContent = "Reloaded the saved version. Your previous draft was not submitted.";
        });
        keep.addEventListener("click", () => conflict.remove());
        conflict.append(reload, keep);
        this.editor.append(conflict);
      }
    } finally {
      button.disabled = false;
    }
  }

  // Structural layout updates from the workspace editor: the view keeps
  // surviving cell elements, and this page
  // restores focus and selection to the pane that held them.
  updateTree(tree: WorkspaceNode): boolean {
    const view = this.view;
    if (!view) return false;
    const verdict = validateWorkspaceTree(tree);
    if (!verdict.ok) return false;
    const active = this.win.document.activeElement;
    const focusedPane = [...this.panes.values()].find((pane) => active instanceof Node && pane.cell.element.contains(active));
    // Selection is captured as node references and offsets, not as live
    // Range objects: a Range collapses when the view re-parents the cell
    // that holds its endpoints, while the nodes themselves survive the move.
    const selection = this.win.getSelection();
    const ranges: Readonly<{ startContainer: Node; startOffset: number; endContainer: Node; endOffset: number }>[] = [];
    if (selection) {
      for (let i = 0; i < selection.rangeCount; i += 1) {
        const range = selection.getRangeAt(i);
        ranges.push({ startContainer: range.startContainer, startOffset: range.startOffset, endContainer: range.endContainer, endOffset: range.endOffset });
      }
    }
    view.update(tree);
    for (const entry of verdict.value) {
      const existing = this.panes.get(entry.key);
      if (existing) { existing.leaf = entry.leaf; continue; }
      const cell = view.cell(entry.key);
      if (!cell) continue;
      const pane: PaneRuntime = { cell, leaf: entry.leaf, columns: 0, rows: 0, live: false, adoptAttempts: 0, transportGeneration: 0 };
      this.panes.set(entry.key, pane);
      cell.setState(Object.freeze({ kind: "resolving" }));
      const snapshot = this.inventory.latest();
      if (snapshot) void this.attachLeaf(entry, snapshot);
    }
    if (focusedPane && active instanceof HTMLElement && focusedPane.cell.element.contains(active)) {
      active.focus({ preventScroll: true });
      if (selection && ranges.length > 0) {
        selection.removeAllRanges();
        for (const saved of ranges) {
          if (!saved.startContainer.isConnected || !saved.endContainer.isConnected) continue;
          const range = this.win.document.createRange();
          range.setStart(saved.startContainer, saved.startOffset);
          range.setEnd(saved.endContainer, saved.endOffset);
          selection.addRange(range);
        }
      }
    }
    this.applyFocusMarks();
    return true;
  }

  panesSnapshot(): readonly Readonly<{ key: string; state: PaneState; generation: number; identity?: ResolvedPaneIdentity; counters?: UnifiedPaneControllerState }>[] {
    return [...this.panes.values()].map((pane) => Object.freeze({
      key: pane.cell.key, state: pane.cell.state(), generation: pane.transportGeneration,
      ...(pane.identity ? { identity: pane.identity } : {}),
      ...(pane.controller ? { counters: pane.controller.state() } : {}),
    }));
  }

  designatedKey(): string | undefined { return this.designated; }
  inventoryRequestCount(): number { return this.inventory.requestCount(); }
  posture(): WorkspacePosture { return "desktop"; }

  private suspend(): void {
    if (this.torndown) return;
    for (const pane of this.panes.values()) pane.controller?.suspend();
  }

  private resume(): void {
    if (this.torndown) return;
    for (const pane of this.panes.values()) pane.controller?.resume();
  }

  teardown(reason: string): void {
    if (this.torndown) return;
    this.torndown = true;
    if (this.presentationTimer !== undefined) this.win.clearInterval(this.presentationTimer);
    this.presentationTimer = undefined;
    for (const pane of this.panes.values()) pane.controller?.detach(reason);
    this.snippets.dispose();
  }
}

function sameLeafSet(tree: WorkspaceNode, selectors: readonly SessionSelector[]): boolean {
  const leaves = leafEntries(tree);
  if (leaves.length !== selectors.length) return false;
  return leaves.every((entry) => selectors.some((selector) => sameSession(selector, entry.leaf.session)));
}
