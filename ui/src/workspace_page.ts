// The /workspace document: the strict route branch, the landing card
// that never auto-attaches, the honest phone-class state,
// and the desktop runtime — one per-pane controller per
// leaf, one single-flight inventory per workspace, pinned identities, a
// designated-pane focus veto composed over terminal's hook, presentation-only
// maximize, keyboard pane picker, and the visual workspace layout editor.
//
// An ephemeral arrangement lives in this tab
// (sessionStorage, keyed by the workspace name) and is written exactly once,
// at the trusted "Open workspace" tap. Nothing here writes it on layout
// changes (the workspace editor owns store writes).
//
// Seam law: this module imports the shared projection/close-policy types
// through the workspace layout module and never re-declares them; it never emits
// RESIZE_REQUEST; it puts no capability, handle, socket, or incarnation key
// into the tree model; it consumes the page's focus hook as a veto only.
import { installTapFeedback } from "./tap_feedback";
import { boundedFetch, RequestTimeoutError } from "./bounded_fetch";
import "./workspace_page.css";
import { csrfToken } from "./csrf_refresh";
import { filterSessionList, unifiedBlockedMessage, unifiedTerminalURL, type DashboardInventory, type DashboardSession, type HistoryChoice } from "./dashboard";
import { ADOPTION_HISTORY_ROWS, readScrollbackRows, readTerminalScrollbackRows } from "./scrollback_preferences";
import { stageSessionImage } from "./image_staging_client";
import { SnippetService, deviceOrigin } from "./snippet_client";
import { pendingIdentityFromSession, writeLastSession } from "./session_memory";
import { OperatorPreferencesService, type OperatorPreferenceSubscription } from "./operator_preferences";
import type { UnifiedThemeID } from "./unified_themes";
import type { CommitFocusContext } from "./unified_focus_claim";
import { classifyUnifiedClose, type UnifiedInventoryDetail } from "./unified_close_policy";
import { UnifiedPaneController, fetchInventory, type InventoryResolver, type InventorySnapshot, type ResolvedPaneIdentity, type UnifiedPaneControllerState } from "./unified_pane_controller";
import {
  PANE_CAP, addLeaf, applySplitWeightsByMatch, arrangeWithPreset, availablePresetsForCount, leaf, leafEntries, nodeAt, parseWorkspace, pathLabel, presetLabel, removeLeaf, replaceLeafSession, sameSession, serializeWorkspace, sessionKey, setLeafAliasHint, split, splitChildSignatures, splitLeaf, swapLeaves, unsplit, validateWorkspaceTree, workspaceRefusalMessage,
  type LeafEntry, type NodePath, type SessionSelector, type SplitDirection, type WorkspaceLeaf, type WorkspaceNode, type WorkspaceResult,
} from "./workspace_model";
import {
  WorkspaceLayoutView, paneStateFromClose, paneStateFromProjection, paneStateCopy, renderWorkspacePhoneState, renderWorkspaceUnavailable, workspacePosture, workspaceRefusalNotice,
  type PaneAffordance, type PaneState, type PhoneLeafEntry, type WorkspaceCell, type WorkspacePosture, type WorkspaceWeightChange,
} from "./workspace_layout";
import { renderWorkspaceDiagram } from "./workspace_diagram";
import { WorkspaceAPI, WorkspaceAPIError, workspaceAPIMessage, type WorkspaceRecord } from "./workspace_api";
import { parseWorkspaceLocation, workspaceRouteNotice, workspaceURL } from "./workspace_url";
import { nextInventoryReadOrder } from "./inventory_read_order";

// --- Posture. Authored in workspace_posture.ts, which
// the dashboard reads too so that its create affordance and this document's
// decision are the same function of the same inputs.
import { stablePostureEnvironment } from "./workspace_posture";
export { stablePostureEnvironment };

function applyWorkspaceTheme(theme: UnifiedThemeID): void {
  if (typeof document === "undefined" || !document.body) return;
  document.body.dataset.wsTheme = theme;
}

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
  private inflight?: Readonly<{ readOrder: number; promise: Promise<InventorySnapshot> }>;
  private requests = 0;

  constructor(private readonly fetch: (signal: AbortSignal) => Promise<DashboardInventory> = fetchInventory) {}

  requestCount(): number { return this.requests; }
  latest(): InventorySnapshot | undefined { return this.current; }

  // Panes share one read at a time. A caller that needs a read started after
  // readAfter (an alias save) neither reuses an older snapshot nor joins an
  // older read; it starts a new one, and the older read still serves its
  // other callers.
  snapshot(signal: AbortSignal | undefined, newerThan: number, readAfter = 0): Promise<InventorySnapshot> {
    if (this.current && this.current.generation > newerThan && this.current.readOrder > readAfter) return this.detachable(Promise.resolve(this.current), signal);
    if (!this.inflight || this.inflight.readOrder <= readAfter) {
      this.requests += 1;
      const readOrder = nextInventoryReadOrder();
      const shared = new AbortController();
      const promise: Promise<InventorySnapshot> = this.fetch(shared.signal).then((inventory) => {
        // A read that started earlier never replaces a later read's result;
        // its callers receive the later one.
        if (!this.current || readOrder > this.current.readOrder) this.current = Object.freeze({ generation: ++this.generation, readOrder, inventory });
        return this.current;
      }).finally(() => { if (this.inflight?.promise === promise) this.inflight = undefined; });
      this.inflight = Object.freeze({ readOrder, promise });
    }
    return this.detachable(this.inflight.promise, signal);
  }

  resolver(): InventoryResolver {
    return (signal, newerThan, readAfter) => this.snapshot(signal, newerThan, readAfter);
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

function globalAliasForSession(session: DashboardSession | undefined): string | undefined {
  return session?.aliases[0]?.displayAlias || undefined;
}

function globalAliasForSelector(snapshot: InventorySnapshot | undefined, selector: SessionSelector): string | undefined {
  if (!snapshot) return undefined;
  const resolved = resolveLeaf(snapshot, selector);
  return resolved.kind === "resolved" ? globalAliasForSession(resolved.session) : undefined;
}

function formatSelectorChoice(selector: SessionSelector, alias?: string): string {
  return alias ? `${alias} (${selector.name}) — ${selector.realm} · ${selector.server}` : `${selector.name} — ${selector.realm} · ${selector.server}`;
}

function splitGroupSummary(node: WorkspaceNode, snapshot: InventorySnapshot | undefined): string {
  const names = leafEntries(node).map((entry) => entry.leaf.aliasHint ?? globalAliasForSelector(snapshot, entry.leaf.session) ?? entry.leaf.session.name);
  return names.length <= 2 ? names.join(", ") : `${names.slice(0, 2).join(", ")} +${names.length - 2}`;
}

function divergedLiveSplits(liveTree: WorkspaceNode, draftTree: WorkspaceNode): Set<string> {
  const disabled = new Set<string>();
  const walk = (node: WorkspaceNode, path: NodePath): void => {
    if (node.kind !== "split") return;
    const candidate = nodeAt(draftTree, path);
    const sigs = splitChildSignatures(node);
    const matches = candidate !== undefined
      && candidate.kind === "split"
      && candidate.direction === node.direction
      && candidate.children.length === node.children.length
      && splitChildSignatures(candidate).every((sig, idx) => sig === sigs[idx]);
    if (!matches) disabled.add(pathLabel(path));
    node.children.forEach((child, index) => walk(child, [...path, index]));
  };
  walk(liveTree, []);
  return disabled;
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
  document.body.classList.add("ws-document");
  const verdict = parseWorkspaceLocation(win.location);
  if (!verdict.ok) {
    // A typed refusal, rendered in place. Never a redirect and never the
    // terminal self-heal (FW-R cases c/d). Zero preference or inventory reads.
    renderWorkspaceUnavailable(root, workspaceRouteNotice(verdict.refusal));
    return;
  }
  const name = verdict.name;
  const posture = workspacePosture(stablePostureEnvironment(win));
  const inventory = new WorkspaceInventory();
  const workspaceAPI = new WorkspaceAPI();
  // The saved workspaces, the session list and the appearance preferences are
  // independent reads: start them together, so a slow link pays one round trip
  // instead of three. On phone posture, apply the cached theme synchronously
  // without issuing an extra network request.
  const listing = workspaceAPI.list();
  const reading = inventory.snapshot(undefined, -1);
  reading.catch(() => undefined);
  const preferences = new OperatorPreferencesService();
  applyWorkspaceTheme(preferences.snapshot().preferences.theme);
  let bootPrefSub: OperatorPreferenceSubscription | undefined;
  if (posture !== "phone") {
    bootPrefSub = preferences.subscribe((snap) => applyWorkspaceTheme(snap.preferences.theme));
    void preferences.load();
  }
  let record: WorkspaceRecord;
  try {
    const list = await listing;
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
  let snapshot: InventorySnapshot;
  try {
    snapshot = await reading;
  } catch {
    renderWorkspaceUnavailable(root, Object.freeze({ headline: "This workspace cannot be opened", detail: "The current sessions could not be read. Retry from the dashboard.", code: "inventory_unavailable" }));
    return;
  }
  if (posture === "phone") {
    renderPhone(root, record.name, arrangement, snapshot, win);
    return;
  }
  bootPrefSub?.dispose();
  const page = new WorkspacePage({ root, styleNonce, name: record.name, win, inventory, preferences, historyRows: options.historyRows ?? readScrollbackRows(), record, workspaceAPI });
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
  preferences?: OperatorPreferencesService;
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
  private selectedDraftKey?: string;
  private readonly pendingAliases = new Map<string, string>();
  private editing = false;
  private saving = false;
  private workspaceTitle?: HTMLElement;
  private singlePaneLabel?: HTMLElement;
  private panePicker?: HTMLElement;
  private headerNotice?: HTMLElement;
  private saveSwitchButton?: HTMLButtonElement;
  private zoomButton?: HTMLButtonElement;
  private editButton?: HTMLButtonElement;
  private editor?: HTMLElement;
  private editorDiagramHost?: HTMLElement;
  private editorStatus?: HTMLElement;
  private presentationGeneration = 0;
  private presentationTimer?: number;
  private readonly preferences: OperatorPreferencesService;
  private preferenceSub?: OperatorPreferenceSubscription;
  private stopWatchingPreferences?: () => void;
  private shortcutActive = false;
  private readonly onWindowKeyDown = (event: KeyboardEvent): void => this.handleCaptureKeyDown(event);
  private readonly onWindowKeyUp = (event: KeyboardEvent): void => this.handleCaptureKeyUp(event);

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
    this.preferences = options.preferences ?? new OperatorPreferencesService();
    this.preferenceSub = this.preferences.subscribe((snap) => applyWorkspaceTheme(snap.preferences.theme));
    // Preferences saved in another tab (the dashboard's Appearance card)
    // reach every pane of this workspace without a reload.
    this.stopWatchingPreferences = this.preferences.watchExternalChanges({ refetchOnForeground: true });
    installTapFeedback(document);
  }

  // The landing card (B3): the arrangement's leaves with their resolution
  // states and one primary action. Nothing attaches, adopts, mints, or
  // upgrades until a TRUSTED tap on that action.
  landing(arrangement: WorkspaceNode | undefined, snapshot: InventorySnapshot): void {
    const { name } = this.options;
    const durable = this.options.record !== undefined;
    const panel = element("section", "ws-landing");
    panel.setAttribute("aria-label", `Workspace ${name}`);
    panel.dataset.wsLanding = arrangement === undefined ? "pick" : "resume";

    const head = element("div", "ws-landing__head");
    const titles = element("div", "ws-landing__titles");
    titles.append(element("p", "ws-landing__eyebrow", "Workspace"), element("h1", "ws-landing__title", name));
    head.append(titles);

    const selected = new Map<string, SessionSelector>();
    if (arrangement !== undefined) {
      for (const entry of leafEntries(arrangement)) selected.set(entry.key, entry.leaf.session);
      const diagramHost = element("div", "ws-landing__diagram");
      diagramHost.append(renderWorkspaceDiagram(arrangement, {
        showLabels: false,
        availabilityFor: (leafNode) => {
          const res = resolveLeaf(snapshot, leafNode.session);
          if (res.kind !== "resolved") return "missing";
          const st = res.session.unified?.state;
          return st === "open" || st === "adoptable" ? "ready" : "blocked";
        },
      }));
      head.append(diagramHost);
    }
    panel.append(head);

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

    if (durable && arrangement !== undefined) {
      // Focused durable landing card: resolve each saved leaf once in durable
      // tree order. Never dump unrelated host sessions as disabled noise.
      const entries = leafEntries(arrangement);
      let readyCount = 0;
      let missingCount = 0;
      let blockedCount = 0;
      const items: HTMLElement[] = [];
      for (const entry of entries) {
        const resolution = resolveLeaf(snapshot, entry.leaf.session);
        if (resolution.kind === "resolved") {
          const session = resolution.session;
          const openable = session.unified !== undefined && (session.unified.state === "open" || session.unified.state === "adoptable");
          if (openable) readyCount += 1;
          else blockedCount += 1;
          const item = element("li", "ws-landing__session");
          item.dataset.wsSession = session.name;
          const label = element("label", "ws-landing__session-label");
          const box = element("input", "ws-landing__session-box");
          box.type = "checkbox";
          box.checked = true;
          box.disabled = true;
          const alias = entry.leaf.aliasHint ?? globalAliasForSession(session);
          label.append(box, element("span", "ws-landing__session-name", alias ?? session.name));
          if (alias && alias !== session.name) label.append(element("span", "ws-landing__session-tmux", session.name));
          label.append(element("span", "ws-landing__session-scope", `${session.realm} · ${session.server}`));
          const stateText = session.unified === undefined
            ? "unified terminal unavailable"
            : session.unified.state === "open"
              ? "ready to open"
              : session.unified.state === "adoptable"
                ? "can be enabled"
                : unifiedBlockedMessage(session.unified.state);
          const badge = element("span", "ws-landing__session-state", stateText);
          badge.dataset.wsTone = openable ? "ready" : "warn";
          label.append(badge);
          item.append(label);
          items.push(item);
        } else {
          if (resolution.kind === "missing") missingCount += 1;
          else blockedCount += 1;
          const item = element("li", "ws-landing__session ws-landing__session--unresolved");
          item.dataset.wsSession = entry.leaf.session.name;
          const copy = paneStateCopy(resolutionPaneState(resolution, entry.leaf));
          item.append(
            element("span", "ws-landing__session-name", entry.leaf.aliasHint ?? entry.leaf.session.name),
            element("span", "ws-landing__session-scope", `${entry.leaf.session.realm} · ${entry.leaf.session.server}`),
          );
          const badge = element("span", "ws-landing__session-state", copy.badge);
          badge.dataset.wsTone = "warn";
          item.append(badge);
          items.push(item);
        }
      }
      const parts = [`${entries.length} ${entries.length === 1 ? "pane" : "panes"}`, `${readyCount} ready to open`];
      if (missingCount > 0) parts.push(`${missingCount} missing`);
      if (blockedCount > 0) parts.push(`${blockedCount} unavailable`);
      panel.append(element("p", "ws-landing__notice", `${parts.join(" · ")}. Nothing attaches until you open the workspace.`));
      list.replaceChildren(...items);
      const dashboard = element("a", "ws-landing__dashboard", "Dashboard");
      dashboard.href = "/";
      const actions = element("div", "ws-landing__actions");
      actions.append(open, dashboard);
      panel.append(list, status, actions);
    } else {
      panel.append(element("p", "ws-landing__notice", arrangement === undefined
        ? `Pick up to ${PANE_CAP} sessions to open side by side. Nothing attaches until you open the workspace.`
        : "This workspace is not attached. Open it to attach its panes; nothing attaches until you do."));
      const sessions = snapshot.inventory.realms.flatMap((realm) => realm.servers.flatMap((server) => server.sessions));
      const filter = element("input", "ws-landing__filter");
      filter.type = "search";
      filter.placeholder = "Filter sessions";
      filter.setAttribute("aria-label", "Filter sessions");
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
          box.disabled = !openable && !duplicate;
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
          const alias = globalAliasForSession(session);
          label.append(box, element("span", "ws-landing__session-name", alias ?? session.name));
          if (alias && alias !== session.name) label.append(element("span", "ws-landing__session-tmux", session.name));
          label.append(element("span", "ws-landing__session-scope", `${session.realm} · ${session.server}`));
          const stateText = session.unified === undefined
            ? "unified terminal unavailable"
            : session.unified.state === "open"
              ? "ready to open"
              : session.unified.state === "adoptable"
                ? "can be enabled"
                : unifiedBlockedMessage(session.unified.state);
          const badge = element("span", "ws-landing__session-state", stateText);
          badge.dataset.wsTone = openable ? "ready" : "warn";
          label.append(badge);
          item.append(label);
          return item;
        }));
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
      renderList();
      const dashboard = element("a", "ws-landing__dashboard", "Dashboard");
      dashboard.href = "/";
      const actions = element("div", "ws-landing__actions");
      actions.append(open, dashboard);
      panel.append(filter, list, status, actions);
    }

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
    renderOpen();
    this.root.replaceChildren(panel);
    if (durable && !open.disabled) open.focus({ preventScroll: true });
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
      this.singlePaneLabel = element("span", "ws-pane-single-label");
      this.singlePaneLabel.hidden = true;
      const picker = element("div", "ws-pane-picker");
      picker.setAttribute("role", "toolbar");
      picker.setAttribute("aria-label", "Workspace panes");
      picker.addEventListener("keydown", (event) => this.handlePickerKeyDown(event));
      this.panePicker = picker;

      const notice = element("output", "ws-workspace-notice");
      notice.setAttribute("role", "status");
      notice.setAttribute("aria-live", "polite");
      notice.hidden = true;
      this.headerNotice = notice;

      const actions = element("div", "ws-workspace-header__actions");
      const saveSwitch = element("button", "ws-workspace-save-switch", "Save switch");
      saveSwitch.type = "button";
      saveSwitch.hidden = true;
      saveSwitch.addEventListener("click", () => { void this.saveSwitchedPane(); });
      this.saveSwitchButton = saveSwitch;

      const zoom = element("button", "ws-workspace-zoom", "Maximize");
      zoom.type = "button";
      zoom.setAttribute("aria-pressed", "false");
      zoom.title = "Maximize active pane (Alt+Shift+F6, then Z)";
      zoom.addEventListener("click", () => this.toggleZoom(zoom));
      this.zoomButton = zoom;

      const edit = element("button", "ws-workspace-edit", "Edit workspace");
      edit.type = "button";
      edit.setAttribute("aria-expanded", "false");
      edit.addEventListener("click", () => {
        if (this.saving) return;
        if (this.editing) this.cancelEditing();
        else this.beginEditing();
      });
      this.editButton = edit;

      const dashboard = element("a", "ws-workspace-dashboard", "Dashboard");
      dashboard.href = "/";
      actions.append(saveSwitch, zoom, edit, dashboard);
      header.append(this.workspaceTitle, this.singlePaneLabel, picker, notice, actions);
      this.editor = element("section", "ws-editor");
      this.editor.hidden = true;
      this.root.append(header, this.editor, layoutHost);
    } else {
      this.root.append(layoutHost);
    }
    const view = new WorkspaceLayoutView(layoutHost, tree, {
      onAffordance: (cell, affordance) => { void this.affordance(cell, affordance); },
      onCellRetired: (cell) => this.retire(cell),
      onTreeChange: (_next, _cause, weightChange) => {
        if (!this.editing || !this.draft || !weightChange) return;
        const merged = applySplitWeightsByMatch(
          this.draft,
          weightChange.path,
          weightChange.direction,
          weightChange.childSignatures,
          weightChange.weights,
        );
        if (!merged.ok) return;
        this.draft = merged.value;
        this.refreshEditorDiagram();
        if (this.editorStatus) this.editorStatus.textContent = "Local draft changed. Save to make it durable.";
      },
    });
    view.setEditing(false);
    this.view = view;
    this.designated = entries[0]?.key;
    this.root.addEventListener("focusin", (event) => this.onFocusIn(event));
    this.win.addEventListener("keydown", this.onWindowKeyDown, { capture: true });
    this.win.addEventListener("keyup", this.onWindowKeyUp, { capture: true });
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
    this.renderPanePicker();
    await this.preferences.load();
    const snapshot = this.inventory.latest() ?? await this.inventory.snapshot(undefined, -1);
    this.presentationGeneration = snapshot.generation;
    await Promise.all(entries.map((entry) => this.attachLeaf(entry, snapshot)));
    this.renderPanePicker();
    if (!this.torndown) this.presentationTimer = this.win.setInterval(() => { void this.refreshPresentation(); }, PRESENTATION_REFRESH_MS);
  }

  private handleCaptureKeyDown(event: KeyboardEvent): void {
    if (!event.isTrusted || this.torndown || event.isComposing || this.editing) return;
    if (event.code !== "F6" || !event.altKey || !event.shiftKey || event.ctrlKey || event.metaKey) return;
    if (typeof event.getModifierState === "function" && event.getModifierState("AltGraph")) return;
    if (this.panes.size <= 1 && this.view?.zoomedKey() === undefined) return;
    if (this.win.document.querySelector("dialog[open], .persea-unified-view-popover:not([hidden]), .persea-unified-identity__details:not([hidden]), .persea-unified-sheet:not([hidden])")) return;
    const target = event.target;
    if (target instanceof HTMLElement) {
      const isXtermInput = target.classList.contains("xterm-helper-textarea");
      if (!isXtermInput && (target.matches("input, textarea, select") || target.isContentEditable)) return;
      if (target.closest('[data-select-mode="true"]')) return;
    }
    event.preventDefault();
    event.stopImmediatePropagation();
    this.shortcutActive = true;
    if (event.repeat) return;
    const activePill = this.panePicker?.querySelector<HTMLButtonElement>('button[tabindex="0"]')
      ?? this.panePicker?.querySelector<HTMLButtonElement>("button");
    activePill?.focus({ preventScroll: true });
  }

  private handleCaptureKeyUp(event: KeyboardEvent): void {
    if (!this.shortcutActive || event.code !== "F6") return;
    this.shortcutActive = false;
    event.preventDefault();
    event.stopImmediatePropagation();
  }

  private handlePickerKeyDown(event: KeyboardEvent): void {
    const picker = this.panePicker;
    if (!picker) return;
    const pills = Array.from(picker.querySelectorAll<HTMLButtonElement>(".ws-pane-pill"));
    if (pills.length === 0) return;
    const current = pills.findIndex((pill) => pill === this.win.document.activeElement);
    const activeIndex = current >= 0 ? current : 0;
    const moveTo = (index: number) => {
      const bounded = ((index % pills.length) + pills.length) % pills.length;
      pills.forEach((pill, i) => { pill.tabIndex = i === bounded ? 0 : -1; });
      pills[bounded]!.focus({ preventScroll: true });
    };
    if (event.key === "ArrowRight") { event.preventDefault(); moveTo(activeIndex + 1); return; }
    if (event.key === "ArrowLeft") { event.preventDefault(); moveTo(activeIndex - 1); return; }
    if (event.key === "Home") { event.preventDefault(); moveTo(0); return; }
    if (event.key === "End") { event.preventDefault(); moveTo(pills.length - 1); return; }
    if (event.key === "z" || event.key === "Z") {
      event.preventDefault();
      this.toggleZoom(pills[activeIndex]);
      return;
    }
    if (event.key === "Escape") {
      event.preventDefault();
      const targetKey = this.designated;
      if (targetKey) this.focusPane(targetKey, true);
      return;
    }
    if (/^[1-6]$/.test(event.key)) {
      const idx = Number(event.key) - 1;
      if (idx < pills.length) {
        event.preventDefault();
        const key = pills[idx]!.dataset.wsPaneKey;
        if (key) this.activatePaneFromHeader(key);
      }
    }
  }

  private paneDisplayLabel(pane: PaneRuntime): string {
    const snapshot = this.inventory.latest();
    const alias = globalAliasForSelector(snapshot, pane.leaf.session);
    return pane.leaf.aliasHint ?? alias ?? pane.leaf.session.name;
  }

  private paneIsSwitched(pane: PaneRuntime): boolean {
    return pane.identity !== undefined && !sameSession(pane.identity.selector, pane.leaf.session);
  }

  private switchedPaneToSave(): PaneRuntime | undefined {
    const active = this.designated ? this.panes.get(this.designated) : undefined;
    if (active && this.paneIsSwitched(active)) return active;
    const switched = [...this.panes.values()].filter((pane) => this.paneIsSwitched(pane));
    return switched.length === 1 ? switched[0] : undefined;
  }

  private renderPanePicker(): void {
    const entries = this.view ? leafEntries(this.view.tree()) : [];
    const zoomedKey = this.view?.zoomedKey();
    if (this.saveSwitchButton) {
      const activePane = this.switchedPaneToSave();
      const canOffer = !this.editing && this.record !== undefined && this.options.workspaceAPI !== undefined
        && activePane !== undefined;
      if (!canOffer || !activePane?.identity || !this.record) {
        this.saveSwitchButton.hidden = true;
        this.saveSwitchButton.disabled = true;
      } else {
        const target = activePane.identity.selector;
        const entry = leafEntries(this.record.tree).find((candidate) => candidate.key === activePane.cell.key);
        const duplicate = entry !== undefined && !replaceLeafSession(this.record.tree, entry.path, target).ok;
        const label = duplicate
          ? `Cannot save ${target.name}: already another pane in this workspace`
          : `Save ${target.name} in this pane`;
        this.saveSwitchButton.hidden = false;
        this.saveSwitchButton.disabled = this.saving || duplicate || entry === undefined;
        this.saveSwitchButton.setAttribute("aria-label", label);
        this.saveSwitchButton.title = label;
      }
    }
    if (this.zoomButton) {
      this.zoomButton.hidden = entries.length <= 1;
      this.zoomButton.setAttribute("aria-pressed", String(zoomedKey !== undefined));
      this.zoomButton.textContent = zoomedKey !== undefined ? "Restore grid" : "Maximize";
    }
    if (this.singlePaneLabel) {
      const only = entries.length === 1 ? this.panes.get(entries[0]!.key) : undefined;
      const customHint = only?.leaf.aliasHint;
      this.singlePaneLabel.textContent = customHint ?? "";
      this.singlePaneLabel.hidden = !customHint;
    }
    const picker = this.panePicker;
    if (!picker) return;
    if (entries.length <= 1) {
      picker.hidden = true;
      picker.replaceChildren();
      return;
    }
    picker.hidden = false;
    const focusedKey = this.win.document.activeElement instanceof HTMLElement
      ? this.win.document.activeElement.closest<HTMLElement>(".ws-pane-pill")?.dataset.wsPaneKey
      : undefined;
    const pills = entries.map((entry, index) => {
      const pane = this.panes.get(entry.key);
      const pill = element("button", "ws-pane-pill");
      pill.type = "button";
      pill.dataset.wsPaneKey = entry.key;
      pill.dataset.wsSession = entry.leaf.session.name;
      const isDesignated = entry.key === this.designated;
      if (isDesignated) pill.dataset.wsDesignated = "true";
      pill.tabIndex = (focusedKey !== undefined ? entry.key === focusedKey : isDesignated || (this.designated === undefined && index === 0)) ? 0 : -1;
      const state = pane?.cell.state() ?? Object.freeze({ kind: "resolving" as const });
      const copy = paneStateCopy(state);
      pill.dataset.wsTone = copy.tone;
      const dot = element("span", "ws-pane-pill__dot");
      dot.setAttribute("aria-hidden", "true");
      const label = pane ? this.paneDisplayLabel(pane) : (entry.leaf.aliasHint ?? entry.leaf.session.name);
      const text = element("span", "ws-pane-pill__label", label);
      pill.append(dot, text);
      if (pane && this.paneIsSwitched(pane)) {
        pill.append(element("span", "ws-pane-pill__tag", "Switched"));
        pill.title = `${label} (viewing ${pane.identity!.selector.name})`;
      } else {
        pill.title = `${label} — ${entry.leaf.session.realm} · ${entry.leaf.session.server}`;
      }
      pill.setAttribute("aria-pressed", String(isDesignated));
      pill.addEventListener("click", () => this.activatePaneFromHeader(entry.key));
      return pill;
    });
    picker.replaceChildren(...pills);
    if (focusedKey !== undefined) {
      const restore = pills.find((pill) => pill.dataset.wsPaneKey === focusedKey);
      restore?.focus({ preventScroll: true });
    }
  }

  private activatePaneFromHeader(key: string): void {
    if (!this.panes.has(key)) return;
    this.designated = key;
    if (this.view?.zoomedKey() !== undefined) {
      this.view.setZoomed(key);
    }
    this.applyFocusMarks();
    this.renderPanePicker();
    const coarse = this.win.matchMedia?.("(pointer: coarse)").matches === true;
    this.focusPane(key, !coarse);
  }

  private focusPane(key: string, focusTerminal: boolean): void {
    const pane = this.panes.get(key);
    if (!pane) return;
    if (focusTerminal) {
      const textarea = pane.cell.mount.querySelector<HTMLElement>(".xterm-helper-textarea");
      if (textarea) { textarea.focus({ preventScroll: true }); return; }
      const host = pane.cell.mount.querySelector<HTMLElement>(".persea-unified-xterm");
      if (host) { host.focus({ preventScroll: true }); return; }
    }
    pane.cell.element.querySelector<HTMLElement>(".ws-cell__header")?.focus({ preventScroll: true });
  }

  private toggleZoom(invoker?: HTMLElement): void {
    if (!this.view || this.panes.size <= 1 || this.saving) return;
    if (this.editing) this.cancelEditing();
    const currentlyZoomed = this.view.zoomedKey();
    if (currentlyZoomed !== undefined) {
      this.view.setZoomed(undefined);
      this.renderPanePicker();
      if (invoker && invoker.isConnected) invoker.focus({ preventScroll: true });
      else if (this.designated) this.focusPane(this.designated, true);
      return;
    }
    const target = this.designated && this.panes.has(this.designated)
      ? this.designated
      : this.panes.keys().next().value;
    if (!target) return;
    this.designated = target;
    this.view.setZoomed(target);
    this.applyFocusMarks();
    this.renderPanePicker();
    const coarse = this.win.matchMedia?.("(pointer: coarse)").matches === true;
    this.focusPane(target, !coarse);
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
      // pane and its rotation-deferred detail never arrives.
      const resolved = resolveLeaf(current, identity.selector);
      if (resolved.kind !== "resolved" || resolved.identity.incarnationKey !== identity.incarnationKey) continue;
      controller.updateProjectionDetail(identity.incarnationKey, resolved.session.unified?.state === "open" ? resolved.session.unified.detail : undefined);
    }
    this.renderPanePicker();
  }

  // Per-leaf admission: resolve → adopt if adoptable → mount own controller.
  // Every outcome renders in the cell; a sibling is never touched.
  private async attachLeaf(entry: LeafEntry, snapshot: InventorySnapshot): Promise<void> {
    const pane = this.panes.get(entry.key);
    if (!pane || this.torndown) return;
    const resolution = resolveLeaf(snapshot, entry.leaf.session);
    pane.cell.setState(resolutionPaneState(resolution, entry.leaf));
    this.renderPanePicker();
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
        const response = await boundedFetch("/api/session-adoptions", {
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
        this.renderPanePicker();
        const exponential = Math.min(ADOPT_MAX_DELAY_MS, ADOPT_BASE_DELAY_MS * (2 ** (pane.adoptAttempts - 1)));
        await new Promise((resolve) => this.win.setTimeout(resolve, Math.round(exponential * (0.8 + Math.random() * 0.4))));
        continue;
      }
      pane.cell.setState(Object.freeze({ kind: "failed", reason: status === 429 ? "rate_limited" : "attach_failed" }));
      this.renderPanePicker();
      return false;
    }
    return false;
  }

  private mountController(key: string, pane: PaneRuntime, resolution: Extract<LeafResolution, { kind: "resolved" }>): void {
    const { session, identity } = resolution;
    const { styleNonce } = this.options;
    let historyRows = readTerminalScrollbackRows(identity.incarnationKey, this.options.historyRows);
    pane.projectionDetail = session.unified?.state === "open" ? session.unified.detail : undefined;
    const connecting = () => {
      pane.cell.setState(Object.freeze({ kind: "connecting" as const, ...(pane.projectionDetail ? { detail: pane.projectionDetail } : {}) }));
      this.renderPanePicker();
    };
    const live = () => {
      pane.cell.setState(Object.freeze({ kind: "live" as const, columns: pane.columns, rows: pane.rows, ...(pane.projectionDetail ? { detail: pane.projectionDetail } : {}) }));
      this.renderPanePicker();
    };
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
      onIdentityReplaced: (next) => {
        pane.identity = next;
        this.designated = pane.cell.key;
        this.applyFocusMarks();
        this.renderPanePicker();
      },
      // terminal's hook is a VETO: the page's pointer rule decides whether a
      // COMMIT may claim focus at all; this policy only withholds it from
      // every pane but the designated one. Read pane.cell.key dynamically so
      // in-place key reconciliation keeps the veto aligned with designated.
      claimFocusOnCommit: (context: CommitFocusContext) => context.pointerRuleClaims && pane.cell.key === this.designated,
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
            pane.live = false;
            pane.lastClose = frame.reason ?? "attachment_failed";
            pane.cell.setState(paneStateFromClose(frame.reason ?? "attachment_failed"));
            this.renderPanePicker();
          }
        },
        transportClosed: (_generation, reason) => {
          pane.live = false;
          pane.lastClose = reason;
          pane.cell.setState(paneStateFromClose(reason));
          this.renderPanePicker();
        },
        reconnectStatus: (status) => {
          if (status.state === "OFFLINE") pane.cell.setState(Object.freeze({ kind: "failed", reason: "reconnect_offline" }));
          if (status.state === "DETACHED" && status.reason !== undefined && classifyUnifiedClose(status.reason) === "reattach") {
            pane.cell.setState(Object.freeze({ kind: "failed", reason: status.reason }));
          }
          if (status.state === "EXHAUSTED") pane.cell.setState(Object.freeze({ kind: "failed", reason: status.reason ?? "reconnect_exhausted" }));
          this.renderPanePicker();
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
        this.renderPanePicker();
        return;
      }
      case "retry": {
        if (pane.controller) {
          pane.controller.dispose("retry");
          pane.controller = undefined;
        }
        pane.identity = undefined;
        pane.live = false;
        pane.cell.setState(Object.freeze({ kind: "resolving" }));
        this.renderPanePicker();
        const snapshot = await this.paneSnapshot(pane);
        const entry = leafEntries(this.view?.tree() ?? pane.leaf).find((candidate) => candidate.key === cell.key);
        if (snapshot && entry) await this.attachLeaf(entry, snapshot);
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
    this.renderPanePicker();
    let response: Response;
    try {
      response = await boundedFetch("/api/sessions", {
        method: "POST", cache: "no-store", credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrfToken() },
        body: JSON.stringify({ realm: session.realm, server: session.server, name: session.name }),
      });
    } catch (error) {
      pane.cell.setState(error instanceof RequestTimeoutError
        ? Object.freeze({ kind: "failed", reason: "create_outcome_unknown" })
        : Object.freeze({ kind: "create_failed", code: "unreachable", status: 0 }));
      this.renderPanePicker();
      return;
    }
    if (response.status !== 201) {
      const code = (await response.text().catch(() => "")).trim();
      pane.cell.setState(Object.freeze({ kind: "create_failed", code: /^[a-z][a-z0-9_]{0,63}$/.test(code) ? code : "create_failed", status: response.status }));
      this.renderPanePicker();
      return;
    }
    const snapshot = await this.paneSnapshot(pane);
    const entry = leafEntries(this.view?.tree() ?? pane.leaf).find((candidate) => candidate.key === pane.cell.key);
    if (snapshot && entry) await this.attachLeaf(entry, snapshot);
  }

  private async paneSnapshot(pane: PaneRuntime): Promise<InventorySnapshot | undefined> {
    try {
      return await this.inventory.snapshot(undefined, this.inventory.latest()?.generation ?? -1);
    } catch {
      if (!this.torndown && this.panes.get(pane.cell.key) === pane) {
        pane.cell.setState(Object.freeze({ kind: "failed", reason: "session_list_unavailable" }));
        this.renderPanePicker();
      }
      return undefined;
    }
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
    let changed = false;
    for (const pane of this.panes.values()) {
      if (pane.cell.element === cellElement) {
        changed = this.designated !== pane.cell.key;
        this.designated = pane.cell.key;
        break;
      }
    }
    this.applyFocusMarks();
    if (changed) this.renderPanePicker();
  }

  private applyFocusMarks(): void {
    for (const pane of this.panes.values()) {
      if (pane.cell.key === this.designated) pane.cell.element.dataset.wsDesignated = "true";
      else delete pane.cell.element.dataset.wsDesignated;
    }
  }

  private setHeaderNotice(message: string): void {
    if (!this.headerNotice) return;
    this.headerNotice.textContent = message;
    this.headerNotice.hidden = message === "";
  }

  private async saveSwitchedPane(): Promise<void> {
    if (this.saving || this.editing || this.torndown) return;
    const api = this.options.workspaceAPI;
    const record = this.record;
    const pane = this.switchedPaneToSave();
    if (!api || !record || !pane || !pane.identity || !this.paneIsSwitched(pane)) return;
    const entry = leafEntries(record.tree).find((candidate) => candidate.key === pane.cell.key);
    if (!entry) return;
    const replaced = replaceLeafSession(record.tree, entry.path, pane.identity.selector);
    if (!replaced.ok) {
      this.setHeaderNotice(workspaceRefusalMessage(replaced.refusal));
      this.renderPanePicker();
      return;
    }
    this.saving = true;
    this.setHeaderNotice("");
    if (this.editButton) this.editButton.disabled = true;
    if (this.saveSwitchButton) this.saveSwitchButton.disabled = true;
    try {
      const next = await api.update(record, record.name, replaced.value);
      this.record = next;
      this.updateTree(next.tree);
      this.setHeaderNotice("");
    } catch (error) {
      const copy = workspaceAPIMessage(error, "saved");
      this.setHeaderNotice(`${copy.headline}. ${copy.detail} (${copy.code})`);
    } finally {
      this.saving = false;
      if (this.editButton) this.editButton.disabled = false;
      this.renderPanePicker();
    }
  }

  private beginEditing(): void {
    if (this.editing || this.saving || !this.record || !this.options.workspaceAPI || !this.view) return;
    this.editing = true;
    this.draft = this.record.tree;
    this.draftName = this.record.name;
    this.pendingAliases.clear();
    this.setHeaderNotice("");
    this.selectedDraftKey = this.designated ?? leafEntries(this.draft)[0]?.key;
    this.view.setEditing(true);
    this.view.setDisabledSplits(new Set());
    this.editButton?.setAttribute("aria-expanded", "true");
    this.renderPanePicker();
    this.renderEditor();
  }

  private cancelEditing(): void {
    if (!this.editing || this.saving || !this.record || !this.view) return;
    this.editing = false;
    this.draft = undefined;
    this.draftName = undefined;
    this.selectedDraftKey = undefined;
    this.pendingAliases.clear();
    // Divider weights are the only draft mutation reflected in the live DOM.
    // Restoring the durable tree keeps every keyed cell/controller intact.
    this.view.update(this.record.tree);
    this.view.setEditing(false);
    this.editButton?.setAttribute("aria-expanded", "false");
    if (this.editor) this.editor.hidden = true;
    this.renderPanePicker();
  }

  private syncDivergedSeams(): void {
    if (!this.editing || !this.view || !this.draft) return;
    this.view.setDisabledSplits(divergedLiveSplits(this.view.tree(), this.draft));
  }

  // Synchronizes every rendered or pending pane label into this.draft before a
  // structural edit or Save so typed labels are never silently dropped. When
  // strict=true (Save), an invalid label aborts with its exact refusal and key.
  private flushPendingAliases(strict: boolean): Readonly<{ ok: true }> | Readonly<{ ok: false; key: string; message: string }> {
    if (!this.draft) return Object.freeze({ ok: true });
    if (this.editor) {
      for (const paneEl of Array.from(this.editor.querySelectorAll<HTMLElement>(".ws-editor__pane"))) {
        const key = paneEl.dataset.wsPaneKey;
        const input = paneEl.querySelector<HTMLInputElement>(".ws-editor__alias input");
        if (key && input) this.pendingAliases.set(key, input.value);
      }
    }
    for (const [key, rawValue] of Array.from(this.pendingAliases.entries())) {
      const entry = leafEntries(this.draft).find((candidate) => candidate.key === key);
      if (!entry) {
        this.pendingAliases.delete(key);
        continue;
      }
      if (rawValue === (entry.leaf.aliasHint ?? "")) {
        this.pendingAliases.delete(key);
        continue;
      }
      const nextHint = rawValue === "" ? undefined : rawValue;
      const result = setLeafAliasHint(this.draft, entry.path, nextHint);
      if (!result.ok) {
        if (strict) return Object.freeze({ ok: false, key, message: workspaceRefusalMessage(result.refusal) });
        continue;
      }
      this.draft = result.value;
      this.pendingAliases.delete(key);
    }
    return Object.freeze({ ok: true });
  }

  private applyDraft(result: WorkspaceResult<WorkspaceNode>): void {
    if (!result.ok) {
      if (this.editorStatus) this.editorStatus.textContent = workspaceRefusalMessage(result.refusal);
      return;
    }
    this.draft = result.value;
    const keys = new Set(leafEntries(this.draft).map((entry) => entry.key));
    for (const key of Array.from(this.pendingAliases.keys())) {
      if (!keys.has(key)) this.pendingAliases.delete(key);
    }
    if (this.selectedDraftKey && !keys.has(this.selectedDraftKey)) {
      this.selectedDraftKey = leafEntries(this.draft)[0]?.key;
    }
    this.syncDivergedSeams();
    this.renderEditor();
    if (this.editorStatus) this.editorStatus.textContent = "Local draft changed. Save to make it durable.";
  }

  private refreshEditorDiagram(): void {
    const host = this.editorDiagramHost;
    const draft = this.draft;
    const record = this.record;
    if (!host || !draft || !record) return;
    const durableKeys = new Set(leafEntries(record.tree).map((entry) => entry.key));
    const addedKeys = new Set(leafEntries(draft).filter((entry) => !durableKeys.has(entry.key)).map((entry) => entry.key));
    const snapshot = this.inventory.latest();
    host.replaceChildren(renderWorkspaceDiagram(draft, {
      interactive: true,
      showLabels: true,
      selectedKey: this.selectedDraftKey,
      addedKeys,
      labelFor: (leafNode) => leafNode.aliasHint ?? globalAliasForSelector(snapshot, leafNode.session) ?? leafNode.session.name,
      onSelect: (key) => {
        this.selectedDraftKey = key;
        this.refreshEditorDiagram();
        if (this.editor) {
          for (const card of Array.from(this.editor.querySelectorAll<HTMLElement>(".ws-editor__pane"))) {
            if (card.dataset.wsPaneKey === key) {
              card.dataset.wsSelected = "true";
              card.scrollIntoView({ block: "nearest" });
            } else {
              delete card.dataset.wsSelected;
            }
          }
        }
      },
      ariaLabel: "Draft workspace layout",
    }));
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
    const snapshot = this.inventory.latest();
    candidates.forEach((selector, index) => {
      const option = element("option");
      option.value = String(index);
      option.textContent = formatSelectorChoice(selector, globalAliasForSelector(snapshot, selector));
      select.append(option);
    });
    select.disabled = candidates.length === 0;
    return select;
  }

  private renderEditor(): void {
    const editor = this.editor;
    const draft = this.draft;
    const record = this.record;
    if (!editor || !draft || !record) return;
    editor.hidden = false;
    editor.setAttribute("aria-label", "Edit workspace");
    const snapshot = this.inventory.latest();

    const bar = element("div", "ws-editor__bar");
    const heading = element("h2", "ws-editor__heading", "Edit workspace");
    const nameLabel = element("label", "ws-editor__field", "Workspace name");
    const name = element("input", "ws-editor__input");
    name.name = "workspace_name";
    name.value = this.draftName ?? record.name;
    name.addEventListener("input", () => { this.draftName = name.value; });
    nameLabel.append(name);

    const save = element("button", "ws-editor__save", "Save");
    save.type = "button";
    save.disabled = this.saving;
    save.addEventListener("click", () => { void this.saveDraft(save); });
    const cancel = element("button", "ws-editor__cancel", "Cancel");
    cancel.type = "button";
    cancel.disabled = this.saving;
    cancel.addEventListener("click", () => this.cancelEditing());
    const actions = element("div", "ws-editor__actions");
    actions.append(save, cancel);
    bar.append(heading, nameLabel, actions);

    const status = element("output", "ws-editor__status");
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    this.editorStatus = status;

    const draftEntries = leafEntries(draft);
    const durableEntries = leafEntries(record.tree);
    const draftKeys = new Set(draftEntries.map((entry) => entry.key));
    const removedLeaves = durableEntries.filter((entry) => !draftKeys.has(entry.key));

    const overview = element("div", "ws-editor__overview");
    const diagramBox = element("div", "ws-editor__diagram-box");
    const diagramFrame = element("div", "ws-editor__diagram-frame");
    this.editorDiagramHost = diagramFrame;
    diagramBox.append(diagramFrame);
    this.refreshEditorDiagram();

    const presets = availablePresetsForCount(draftEntries.length);
    if (presets.length > 0) {
      const presetBar = element("div", "ws-editor__presets");
      presetBar.append(element("span", "ws-editor__presets-label", "Layout:"));
      for (const preset of presets) {
        const button = element("button", "ws-editor__preset", presetLabel(preset, draftEntries.length));
        button.type = "button";
        button.addEventListener("click", () => {
          this.flushPendingAliases(false);
          const currentDraft = this.draft ?? draft;
          const currentLeaves = leafEntries(currentDraft).map((entry) => entry.leaf);
          this.applyDraft(arrangeWithPreset(currentLeaves, preset));
        });
        presetBar.append(button);
      }
      diagramBox.append(presetBar);
    }

    const removedBar = element("div", "ws-editor__removed");
    removedBar.hidden = removedLeaves.length === 0;
    if (removedLeaves.length > 0) {
      removedBar.append(element("span", undefined, "Pending removal on Save:"));
      for (const removed of removedLeaves) {
        const chip = element("span", "ws-editor__removed-chip");
        const label = removed.leaf.aliasHint ?? globalAliasForSelector(snapshot, removed.leaf.session) ?? removed.leaf.session.name;
        chip.append(element("span", undefined, label));
        if (draftEntries.length < PANE_CAP) {
          const undo = element("button", "ws-editor__undo-remove", "Keep");
          undo.type = "button";
          undo.addEventListener("click", () => this.applyDraft(addLeaf(this.draft ?? draft, removed.leaf, "row")));
          chip.append(undo);
        }
        removedBar.append(chip);
      }
      diagramBox.append(removedBar);
    }

    const candidates = this.availableSessions(draft);
    const add = element("fieldset", "ws-editor__add");
    add.append(element("legend", undefined, `Add pane (${draftEntries.length}/${PANE_CAP})`));
    const addPicker = this.sessionPicker(candidates, "Session to add");
    const addRow = element("button", "ws-editor__add-row", "Add beside");
    const addColumn = element("button", "ws-editor__add-column", "Add below");
    for (const [button, direction] of [[addRow, "row"], [addColumn, "column"]] as const) {
      button.type = "button";
      button.disabled = candidates.length === 0 || draftEntries.length >= PANE_CAP;
      button.addEventListener("click", () => {
        const selector = candidates[Number(addPicker.value)];
        if (selector) this.applyDraft(addLeaf(this.draft ?? draft, leaf(selector), direction));
      });
    }
    add.append(addPicker, addRow, addColumn);
    overview.append(diagramBox, add);

    const panes = element("ol", "ws-editor__panes");
    for (const entry of draftEntries) {
      const item = element("li", "ws-editor__pane");
      item.dataset.wsSession = entry.leaf.session.name;
      item.dataset.wsPaneKey = entry.key;
      if (this.selectedDraftKey === entry.key) item.dataset.wsSelected = "true";

      const head = element("div", "ws-editor__pane-head");
      const globalAlias = globalAliasForSelector(snapshot, entry.leaf.session);
      const title = element("strong", "ws-editor__pane-name", globalAlias ?? entry.leaf.session.name);
      head.append(title);
      if (globalAlias && globalAlias !== entry.leaf.session.name) {
        head.append(element("span", "ws-editor__pane-sub", entry.leaf.session.name));
      }
      const remove = element("button", "ws-editor__remove", "Remove pane");
      remove.type = "button";
      remove.disabled = draftEntries.length === 1;
      remove.addEventListener("click", () => this.applyDraft(removeLeaf(this.draft ?? draft, entry.path)));
      head.append(remove);
      item.append(head);

      const aliasLabel = element("label", "ws-editor__alias", "Display label");
      const alias = element("input", "ws-editor__input");
      alias.value = this.pendingAliases.get(entry.key) ?? entry.leaf.aliasHint ?? "";
      alias.placeholder = "Optional pane label";
      alias.addEventListener("input", () => {
        this.pendingAliases.set(entry.key, alias.value);
        const currentDraft = this.draft ?? draft;
        const currentEntry = leafEntries(currentDraft).find((candidate) => candidate.key === entry.key);
        if (!currentEntry) return;
        const nextHint = alias.value === "" ? undefined : alias.value;
        const updated = setLeafAliasHint(currentDraft, currentEntry.path, nextHint);
        if (updated.ok) {
          this.draft = updated.value;
          this.pendingAliases.delete(entry.key);
          this.refreshEditorDiagram();
          if (this.editorStatus) this.editorStatus.textContent = "Local draft changed. Save to make it durable.";
        }
      });
      const setAlias = element("button", "ws-editor__alias-save", "Set label");
      setAlias.type = "button";
      setAlias.addEventListener("click", () => {
        const currentDraft = this.draft ?? draft;
        const currentEntry = leafEntries(currentDraft).find((candidate) => candidate.key === entry.key);
        if (!currentEntry) return;
        this.pendingAliases.delete(entry.key);
        this.applyDraft(setLeafAliasHint(currentDraft, currentEntry.path, alias.value === "" ? undefined : alias.value));
      });
      aliasLabel.append(alias, setAlias);
      item.append(aliasLabel);

      const livePane = this.panes.get(entry.key);
      if (livePane && this.paneIsSwitched(livePane) && livePane.identity) {
        const switchedSelector = livePane.identity.selector;
        if (replaceLeafSession(draft, entry.path, switchedSelector).ok) {
          const switchedAlias = globalAliasForSelector(snapshot, switchedSelector);
          const switchedLabel = switchedAlias && switchedAlias !== switchedSelector.name
            ? `${switchedAlias} (${switchedSelector.name})`
            : switchedSelector.name;
          const useCurrent = element("button", "ws-editor__use-current", `Use current (${switchedLabel})`);
          useCurrent.type = "button";
          useCurrent.addEventListener("click", () => {
            this.flushPendingAliases(false);
            const currentDraft = this.draft ?? draft;
            const currentEntry = leafEntries(currentDraft).find((candidate) => candidate.key === entry.key);
            if (!currentEntry) return;
            this.applyDraft(replaceLeafSession(currentDraft, currentEntry.path, switchedSelector));
          });
          const switchRow = element("div", "ws-editor__pane-ops");
          switchRow.append(useCurrent);
          item.append(switchRow);
        }
      }

      if (draftEntries.length > 1) {
        const swapRow = element("div", "ws-editor__pane-ops");
        const others = draftEntries.filter((other) => other.key !== entry.key);
        const swapSelect = element("select", "ws-editor__select");
        swapSelect.setAttribute("aria-label", `Swap ${entry.leaf.session.name} with`);
        others.forEach((other, index) => {
          const opt = element("option");
          opt.value = String(index);
          const otherLabel = other.leaf.aliasHint ?? globalAliasForSelector(snapshot, other.leaf.session) ?? other.leaf.session.name;
          opt.textContent = otherLabel;
          swapSelect.append(opt);
        });
        const swapBtn = element("button", "ws-editor__swap", "Swap position");
        swapBtn.type = "button";
        swapBtn.addEventListener("click", () => {
          const target = others[Number(swapSelect.value)];
          if (target) this.applyDraft(swapLeaves(this.draft ?? draft, entry.path, target.path));
        });
        swapRow.append(swapSelect, swapBtn);
        item.append(swapRow);
      }

      if (candidates.length > 0) {
        const opsRow = element("div", "ws-editor__pane-ops");
        const picker = this.sessionPicker(candidates, `Session to split or replace ${entry.leaf.session.name}`);
        opsRow.append(picker);
        const replaceBtn = element("button", "ws-editor__replace", "Replace");
        replaceBtn.type = "button";
        replaceBtn.addEventListener("click", () => {
          const selector = candidates[Number(picker.value)];
          if (selector) this.applyDraft(replaceLeafSession(this.draft ?? draft, entry.path, selector));
        });
        opsRow.append(replaceBtn);
        if (draftEntries.length < PANE_CAP) {
          for (const [copy, direction] of [["Split beside", "row"], ["Split below", "column"]] as const) {
            const button = element("button", "ws-editor__split", copy);
            button.type = "button";
            button.addEventListener("click", () => {
              const selector = candidates[Number(picker.value)];
              if (selector) this.applyDraft(splitLeaf(this.draft ?? draft, entry.path, direction, leaf(selector)));
            });
            opsRow.append(button);
          }
        }
        item.append(opsRow);
      }
      panes.append(item);
    }

    const splits = element("div", "ws-editor__splits");
    let splitCount = 0;
    const walk = (node: WorkspaceNode, path: NodePath): void => {
      if (node.kind === "leaf") return;
      splitCount += 1;
      const group = element("div", "ws-editor__unsplit");
      group.append(element("span", undefined, `Collapse ${node.direction} split:`));
      node.children.forEach((child, index) => {
        const keep = element("button", "ws-editor__keep", `Keep ${splitGroupSummary(child, snapshot)}`);
        keep.type = "button";
        keep.addEventListener("click", () => this.applyDraft(unsplit(this.draft ?? draft, path, index)));
        group.append(keep);
      });
      splits.append(group);
      node.children.forEach((child, index) => walk(child, [...path, index]));
    };
    walk(draft, []);

    const children: HTMLElement[] = [bar, status, overview, panes];
    if (splitCount > 0) {
      const disclosure = element("details", "ws-editor__splits-disclosure");
      disclosure.append(element("summary", undefined, `Split structure (${splitCount} ${splitCount === 1 ? "split" : "splits"})`), splits);
      children.push(disclosure);
    }
    editor.replaceChildren(...children);
  }

  private async saveDraft(button: HTMLButtonElement): Promise<void> {
    if (this.saving) return;
    const api = this.options.workspaceAPI;
    const record = this.record;
    const name = this.draftName ?? record?.name;
    if (!api || !record || !this.draft || !name || !this.editorStatus) return;
    const flushed = this.flushPendingAliases(true);
    if (!flushed.ok) {
      this.editorStatus.textContent = flushed.message;
      const badInput = this.editor?.querySelector<HTMLInputElement>(`.ws-editor__pane[data-ws-pane-key="${CSS.escape(flushed.key)}"] .ws-editor__alias input`);
      badInput?.focus({ preventScroll: true });
      return;
    }
    const draft = this.draft;
    this.saving = true;
    button.disabled = true;
    const cancelBtn = this.editor?.querySelector<HTMLButtonElement>(".ws-editor__cancel");
    if (cancelBtn) cancelBtn.disabled = true;
    if (this.editButton) this.editButton.disabled = true;
    if (this.saveSwitchButton) this.saveSwitchButton.disabled = true;
    this.editorStatus.textContent = "Saving workspace…";
    try {
      const next = await api.update(record, name, draft);
      const renamed = next.name !== record.name;
      this.record = next;
      this.editing = false;
      this.draft = undefined;
      this.draftName = undefined;
      this.selectedDraftKey = undefined;
      this.pendingAliases.clear();
      this.updateTree(next.tree);
      this.view?.setEditing(false);
      this.editButton?.setAttribute("aria-expanded", "false");
      if (this.workspaceTitle) this.workspaceTitle.textContent = next.name;
      if (this.editor) this.editor.hidden = true;
      if (renamed) this.win.history.replaceState(this.win.history.state, "", workspaceURL(this.win.location.href, next.name));
      this.renderPanePicker();
    } catch (error) {
      if (error instanceof WorkspaceAPIError && error.code === "conflict" && error.current) {
        if (error.current.workspaceId !== record.workspaceId) {
          // Normalized-name collision with a DIFFERENT saved workspace. Never
          // retarget this.record or reconcile the other workspace's tree.
          if (this.editorStatus) {
            this.editorStatus.textContent = "That workspace name is already in use. Choose another name.";
          }
          this.editor?.querySelector<HTMLInputElement>('input[name="workspace_name"]')?.focus({ preventScroll: true });
          return;
        }
        const currentRecord = error.current;
        const copy = workspaceAPIMessage(error, "saved");
        if (this.editorStatus) this.editorStatus.textContent = `${copy.headline}. ${copy.detail} (${copy.code})`;
        if (this.editor) {
          this.editor.querySelector(".ws-editor__conflict")?.remove();
          const conflict = element("div", "ws-editor__conflict");
          const reload = element("button", "ws-editor__reload", "Reload saved version");
          const keep = element("button", "ws-editor__keep-editing", "Keep editing");
          reload.type = "button"; keep.type = "button";
          reload.addEventListener("click", () => {
            const renamedElsewhere = currentRecord.name !== this.record?.name;
            this.record = currentRecord;
            this.draft = currentRecord.tree;
            this.draftName = currentRecord.name;
            this.pendingAliases.clear();
            this.updateTree(currentRecord.tree);
            this.view?.setDisabledSplits(new Set());
            if (this.workspaceTitle) this.workspaceTitle.textContent = currentRecord.name;
            if (renamedElsewhere) this.win.history.replaceState(this.win.history.state, "", workspaceURL(this.win.location.href, currentRecord.name));
            this.renderEditor();
            this.renderPanePicker();
            if (this.editorStatus) this.editorStatus.textContent = "Reloaded the saved version. Your previous draft was not submitted.";
          });
          keep.addEventListener("click", () => conflict.remove());
          conflict.append(reload, keep);
          this.editor.append(conflict);
        }
        return;
      }
      const copy = workspaceAPIMessage(error, "saved");
      if (this.editorStatus) this.editorStatus.textContent = `${copy.headline}. ${copy.detail} (${copy.code})`;
    } finally {
      this.saving = false;
      const currentSave = this.editor?.querySelector<HTMLButtonElement>(".ws-editor__save");
      if (currentSave) currentSave.disabled = false;
      const currentCancel = this.editor?.querySelector<HTMLButtonElement>(".ws-editor__cancel");
      if (currentCancel) currentCancel.disabled = false;
      if (cancelBtn) cancelBtn.disabled = false;
      if (this.editButton) this.editButton.disabled = false;
      button.disabled = false;
      this.renderPanePicker();
    }
  }

  // Structural layout updates from the workspace editor: the view keeps
  // surviving cell elements (and re-keys a switched pane whose live session
  // matches its new durable leaf so the active socket never reconnects), and
  // this page restores focus and selection to the pane that held them.
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
    // Key reconciliation for in-place switched panes: if an outgoing durable
    // key currently holds a live controller attached to an incoming durable
    // key not yet owned by another pane, transfer that cell/runtime in place
    // before view.update() so its WebSocket and terminal survive untouched.
    const nextByKey = new Map(verdict.value.map((entry) => [entry.key, entry]));
    const rekeys: { oldKey: string; newKey: string; pane: PaneRuntime; nextLeaf: WorkspaceLeaf }[] = [];
    const claimedNewKeys = new Set<string>();
    for (const [oldKey, pane] of this.panes) {
      if (nextByKey.has(oldKey) || !pane.controller || !pane.identity) continue;
      const liveKey = sessionKey(pane.identity.selector);
      const nextEntry = nextByKey.get(liveKey);
      if (!nextEntry || this.panes.has(liveKey) || claimedNewKeys.has(liveKey)) continue;
      claimedNewKeys.add(liveKey);
      rekeys.push({ oldKey, newKey: liveKey, pane, nextLeaf: nextEntry.leaf });
    }
    for (const { oldKey, newKey, pane, nextLeaf } of rekeys) {
      if (!view.rekeyCell(oldKey, newKey, nextLeaf)) continue;
      this.panes.delete(oldKey);
      pane.leaf = nextLeaf;
      this.panes.set(newKey, pane);
      if (this.designated === oldKey) this.designated = newKey;
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
    this.renderPanePicker();
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
  zoomedKey(): string | undefined { return this.view?.zoomedKey(); }
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
    this.win.removeEventListener("keydown", this.onWindowKeyDown, { capture: true });
    this.win.removeEventListener("keyup", this.onWindowKeyUp, { capture: true });
    if (this.presentationTimer !== undefined) this.win.clearInterval(this.presentationTimer);
    this.presentationTimer = undefined;
    this.preferenceSub?.dispose();
    this.preferenceSub = undefined;
    this.stopWatchingPreferences?.();
    this.stopWatchingPreferences = undefined;
    for (const pane of this.panes.values()) pane.controller?.detach(reason);
    this.snippets.dispose();
  }
}

function sameLeafSet(tree: WorkspaceNode, selectors: readonly SessionSelector[]): boolean {
  const leaves = leafEntries(tree);
  if (leaves.length !== selectors.length) return false;
  return leaves.every((entry) => selectors.some((selector) => sameSession(selector, entry.leaf.session)));
}
