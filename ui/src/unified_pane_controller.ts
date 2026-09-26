// The per-pane unified attachment controller.
//
// This is the one-pane closure that used to live inline in app.ts's unified
// boot branch, extracted so the single terminal and a workspace share ONE
// implementation: the single-terminal page is one consumer, a workspace is N.
// Everything that is per pane and shared with no sibling lives here — the
// takeover offer state (`lastControlHandle`, `committedOnce`), endpoint
// construction, the source-binding re-mint, the identity re-mint through a
// PINNED incarnation key, the transport, the page, and the bind/connect/detach
// lifecycle. Everything page-level (URL parsing, the single-terminal reload
// key, history/location, the workspace's single-flight inventory, designated
// focus policy) stays with the consumer and is supplied as callbacks.
//
// Construction has no network side effect: nothing is fetched or opened until
// `connect()` runs. A controller never name-resolves: its identity path
// resolves the pinned key it was constructed with (`resolveDraftScope`), and a
// pinned key that no longer resolves is `session_gone`, never a same-name
// substitute (B2).
import { csrfToken, refreshCSRFToken } from "./csrf_refresh";
import { ADOPTION_HISTORY_ROWS, readTerminalScrollbackRows } from "./scrollback_preferences";
import { adoptFailureMessage, parseInventory, resolveDraftScope, unifiedBlockedMessage, type AttachmentMode, type DashboardInventory, type DashboardSession, type HistoryChoice } from "./dashboard";
import type { ComposerStagedImage } from "./composer_attachments";
import type { SnippetServicePort } from "./snippet_client";
import { switcherInventory, type SessionSwitcherInventory } from "./session_switcher";
import type { OperatorPreferencePort } from "./operator_preferences";
import type { CommitFocusPolicy } from "./unified_focus_claim";
import type { UnifiedInventoryDetail } from "./unified_close_policy";
import { UnifiedTerminalPage, type WidthRefitAttempt, type WidthRefitResult } from "./unified_terminal_page";
import { ReconnectRefusal, WebSocketAttachmentTransport, type AttachmentEndpoint, type AttachmentTransportSink, type ReconnectStatus } from "./websocket_attachment_transport";

export type UnifiedEngine = "unified-dev";

// Endpoint helpers for the current terminal; they hold no workspace state.

export function attachmentURL(handle: string, mode: "observe" | "control"): string {
  void handle;
  void mode;
  const url = new URL("/ws", window.location.href);
  url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return url.toString();
}

export function attachmentProtocols(handle: string, mode: AttachmentMode, historyRows: number, engine: UnifiedEngine): string[] {
  return ["persea-terminal.v1", `persea-handle.${handle}`, `persea-mode.${mode}`, `persea-csrf.${csrfToken()}`, `persea-history.${historyRows}`, `persea-engine.${engine}`];
}

function takeoverProtocols(handle: string, historyRows: number, engine: UnifiedEngine): string[] {
  return [...attachmentProtocols(handle, "control", historyRows, engine), "persea-takeover.v1"];
}

function randomRequestID(): string {
  const bytes = new Uint8Array(32);
  window.crypto.getRandomValues(bytes);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return window.btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function parseFreshHandle(value: unknown): string {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error("fresh attachment handle is malformed");
  const record = value as Record<string, unknown>;
  if (Object.keys(record).length !== 1 || typeof record.handle !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(record.handle)) {
    throw new Error("fresh attachment handle is malformed");
  }
  return record.handle;
}

export async function freshHandle(source: string, purpose: AttachmentMode, signal: AbortSignal): Promise<string> {
  const csrf = await refreshCSRFToken(signal);
  const response = await window.fetch("/api/attachment-handles", {
    method: "POST", cache: "no-store", credentials: "same-origin", signal,
    headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrf },
    body: JSON.stringify({ source, purpose }),
  });
  if (!response.ok) throw new Error("attachment source is unavailable");
  return parseFreshHandle(await response.json());
}

export async function freshEndpoint(mode: AttachmentMode, historyRows: HistoryChoice, signal: AbortSignal, source: string, engine: UnifiedEngine): Promise<AttachmentEndpoint> {
  const handle = await freshHandle(source, mode, signal);
  return Object.freeze({ url: attachmentURL(handle, mode), protocols: Object.freeze(attachmentProtocols(handle, mode, historyRows, engine)) });
}

export async function takeoverEndpoint(claim: Readonly<{ offer: string } | { source: string }>, historyRows: HistoryChoice, signal: AbortSignal, engine: UnifiedEngine): Promise<AttachmentEndpoint> {
  const requestID = randomRequestID();
  const csrf = await refreshCSRFToken(signal);
  const response = await window.fetch("/api/control-takeovers", {
    method: "POST", cache: "no-store", credentials: "same-origin", signal,
    headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrf },
    body: JSON.stringify({ request_id: requestID, ...claim }),
  });
  if (!response.ok) {
    let code = "takeover_unavailable";
    try {
      const value: unknown = await response.json();
      if (value && typeof value === "object" && !Array.isArray(value)) {
        const fields = value as Record<string, unknown>;
        if (Object.keys(fields).length === 1 && typeof fields.code === "string" && /^[a-z][a-z0-9_]{0,63}$/.test(fields.code)) code = fields.code;
      }
    } catch { /* the UI exposes only its closed fallback code */ }
    throw new Error(code);
  }
  const handle = parseFreshHandle(await response.json());
  return Object.freeze({ url: attachmentURL(handle, "control"), protocols: Object.freeze(takeoverProtocols(handle, historyRows, engine)) });
}

// Fetches one inventory snapshot directly. The single terminal's resolver:
// every identity re-mint reads the current inventory, exactly as before the
// extraction. A workspace supplies its own single-flight resolver instead.
export async function fetchInventory(signal: AbortSignal): Promise<DashboardInventory> {
  const response = await window.fetch("/api/inventory", { cache: "no-store", credentials: "same-origin", signal });
  if (!response.ok) throw new Error("inventory unavailable");
  return parseInventory(await response.json());
}

// --- Pinned identity.

export type SessionSelector = Readonly<{ realm: string; server: string; name: string }>;

// The runtime identity of one pane, resolved once at an explicit load and
// pinned for the controller's lifetime. Never persisted.
export type ResolvedPaneIdentity = Readonly<{
  selector: SessionSelector;
  // session.draftScope === authority.key at resolve time.
  incarnationKey: string;
  // For /api/session-adoptions.
  sessionId: string;
}>;

// A snapshot of the inventory tagged with a monotone generation, so a
// controller that already consumed a handle from generation g can ask for one
// strictly newer than g and a workspace can answer six such requests with ONE
// fetch.
export type InventorySnapshot = Readonly<{ generation: number; inventory: DashboardInventory }>;
export type InventoryResolver = (signal: AbortSignal, newerThan: number) => Promise<InventorySnapshot>;

// Per-pane observability for a consumer that renders pane state outside the
// page (the workspace cell). Every hook is optional and purely observational:
// the page remains the sole renderer and input/geometry owner.
export type UnifiedPaneObserver = Readonly<{
  openTransport?(generation: number): void;
  // Every decoded server frame the transport handed to the page, with the
  // page's verdict.
  frame?(generation: number, frame: Readonly<{ type: string; kind?: string; columns?: number; rows?: number; mode?: string; reason?: string }>, verdict: "ENQUEUED" | "STALE" | "CLOSED"): void;
  transportClosed?(generation: number, reason: string): void;
  reconnectStatus?(status: ReconnectStatus): void;
  operationalRefusal?(generation: number, code: string): void;
  projectionDetail?(detail: UnifiedInventoryDetail | undefined): void;
  // A takeover claim this pane made (automatic or manual), before the request.
  takeoverClaim?(claim: "offer" | "source"): void;
  // The identity re-mint path ran: it read an inventory snapshot and either
  // minted from it or refused.
  identityResolve?(outcome: "minted" | "session_gone" | "identity_ambiguous" | "identity_invalid"): void;
  authorityResult?(event: Readonly<{
    kind: "source_remint" | "identity_remint" | "takeover";
    stage: "resolved" | "discarded" | "recorded";
    operation: number;
    currentOperation: number;
    identity: string | null;
    currentIdentity: string | null;
  }>): void;
  // Mechanism-level switch probe. A regression test may queue a microtask from any
  // phase and call `sample`; because the commit primitive is non-async, every
  // queued sample must observe the completely settled B state.
  sessionSwitchPhase?(phase: SessionSwitchCommitPhase, sample: () => SessionSwitchBoundState): void;
}>;

export type SessionSwitchCommitPhase =
  | "precomputed"
  | "operation_invalidated"
  | "identity_replaced"
  | "presentation_replaced"
  | "location_replaced"
  | "endpoint_replaced"
  | "terminal_failure";

export type SessionSwitchBoundState = Readonly<{
  operationToken: number;
  identity: string | null;
  urlIdentity: string | null;
  transcriptOwner: string | null;
  retryToken: number;
  socketGeneration: number;
  socketState: "none" | "connecting" | "open" | "closing" | "closed";
  draftScope: string | null;
  imageRealm: string | null;
  controlOfferIdentity: string | null;
  admission: Readonly<{ prepared: boolean; committed: boolean; controlGranted: boolean }>;
}>;

export type UnifiedPaneControllerOptions = Readonly<{
  root: HTMLElement;
  styleNonce: string;
  capabilityMode: AttachmentMode;
  engine: UnifiedEngine;
  historyRows: HistoryChoice;
  onScrollbackChanged?(rows: HistoryChoice): void;
  // The one-time handle this pane opens with.
  initialHandle: string;
  // The pinned incarnation key the identity re-mint resolves — the single
  // terminal's fragment `draft_scope`, or a workspace leaf's
  // ResolvedPaneIdentity.incarnationKey. Null: the pane has no identity and an
  // identity re-mint refuses with `identity_invalid`.
  incarnationKey: string | null;
  initialIdentity?: ResolvedPaneIdentity;
  // Inventory access for the identity re-mint. See InventoryResolver.
  resolveInventory: InventoryResolver;
  // The snapshot generation the initial handle came from; the first identity
  // re-mint asks for a strictly newer one. 0 when it did not come from a
  // tagged snapshot.
  initialSnapshotGeneration?: number;
  sessionName?: string;
  aliasLabel?: string;
  imageRealm?: string;
  composerStorageScope?: string;
  stageImage?(file: File, signal: AbortSignal): Promise<ComposerStagedImage>;
  stageImageForRealm?(realm: string, file: File, signal: AbortSignal): Promise<ComposerStagedImage>;
  mintReloadHandle?(source: string, signal: AbortSignal): Promise<string>;
  rememberReloadHandle?(handle: string): void;
  forgetReloadHandle?(): void;
  // Construct all fallible URL data before the switch point of no return.
  // The returned closure performs only the already-prepared history write.
  prepareLocation?(session: DashboardSession): () => void;
  onIdentityReplaced?(identity: ResolvedPaneIdentity, session: DashboardSession): void;
  onSessionCommitted?(session: DashboardSession): void;
  claimFocusOnCommit?: CommitFocusPolicy;
  // The ONE document-global snippets/clips service (clipboard). The controller
  // owns the pane, so it is the only thing that hands the service to a page:
  // a snippet action taken in this pane's sheet reaches THIS pane's terminal
  // and no sibling's, and six controllers still share one poll loop. Passing
  // it is inert — the service fetches nothing until a list is on screen.
  snippets?: SnippetServicePort;
  preferences?: OperatorPreferencePort;
  workspaceCell?: boolean;
  observer?: UnifiedPaneObserver;
}>;

export type UnifiedPaneControllerState = Readonly<{
  connected: boolean;
  disposed: boolean;
  incarnationKey: string | null;
  snapshotGeneration: number;
  projectionDetail?: UnifiedInventoryDetail;
  counters: Readonly<{ sourceRemints: number; identityRemints: number; takeovers: number; adoptions: number; switches: number }>;
}>;

type PreparedSessionSwitch = Readonly<{
  abort: AbortController;
  session: DashboardSession;
  identity: RuntimePaneIdentity;
  endpoint: AttachmentEndpoint;
  presentation: Readonly<{
    sessionName: string;
    aliasLabel?: string;
    composerStorageScope: string;
    stageImage?: (file: File, signal: AbortSignal) => Promise<ComposerStagedImage>;
  }>;
  replaceLocation?: () => void;
}>;

type RuntimePaneIdentity = Readonly<{
  resolved?: ResolvedPaneIdentity;
  incarnationKey: string | null;
  sessionName?: string;
  aliasLabel?: string;
  imageRealm?: string;
  session?: DashboardSession;
}>;

export class UnifiedPaneController {
  private readonly transport: WebSocketAttachmentTransport;
  private readonly page: UnifiedTerminalPage;
  // Same one-shot takeover offer discipline as the legacy branch: the latest
  // minted control handle backs an offer-based claim until the first commit,
  // after which claims are source-based. It is refreshed on every mint so a
  // handle re-minted from identity (a reopened tab) is the one the front door
  // records as the offer when a lease is held by another device.
  private lastControlHandle: string | undefined;
  private lastControlHandleIdentity: string | null;
  private committedOnce = false;
  private connected = false;
  private disposed = false;
  private resumeOnRestore = false;
  private snapshotGeneration: number;
  private projectionDetail?: UnifiedInventoryDetail;
  private readonly counters = { sourceRemints: 0, identityRemints: 0, takeovers: 0, adoptions: 0, switches: 0 };
  private currentIdentity: RuntimePaneIdentity;
  // Exact source from the latest validated PREPARE. The draft-scope
  // incarnation remains the stable session owner; this value is the
  // generation authority an out-of-band refit must present to the broker.
  private currentSource: string | null = null;
  private operationToken = 0;
  private switchPending = false;
  private switchAbort?: AbortController;
  private reloadMintAbort?: AbortController;
  private refitAbort?: AbortController;
  private reconnecting = false;
  private refitPending = false;
	private refitResult?: Promise<WidthRefitResult>;
	private refitResultSource: string | null = null;

  constructor(private readonly options: UnifiedPaneControllerOptions) {
    const { capabilityMode: mode, engine, root, styleNonce } = options;
    const historyRows = readTerminalScrollbackRows(options.incarnationKey, options.historyRows);
    this.lastControlHandle = mode === "control" ? options.initialHandle : undefined;
    this.lastControlHandleIdentity = mode === "control" ? options.incarnationKey : null;
    this.snapshotGeneration = options.initialSnapshotGeneration ?? 0;
    this.currentIdentity = Object.freeze({
      ...(options.initialIdentity ? { resolved: options.initialIdentity } : {}),
      incarnationKey: options.incarnationKey,
      ...(options.sessionName ? { sessionName: options.sessionName } : {}),
      ...(options.aliasLabel ? { aliasLabel: options.aliasLabel } : {}),
      ...(options.imageRealm ? { imageRealm: options.imageRealm } : {}),
    });
    this.transport = new WebSocketAttachmentTransport(
      attachmentURL(options.initialHandle, mode), undefined, attachmentProtocols(options.initialHandle, mode, historyRows, engine),
      (signal, source) => this.mintFromSource(signal, source), undefined, undefined, undefined, undefined, (signal) => this.identityEndpoint(signal),
    );
    this.page = new UnifiedTerminalPage({
      root, port: this.transport, capabilityMode: mode, styleNonce, historyRows,
      ...(options.incarnationKey ? { scrollbackScope: options.incarnationKey } : {}),
      ...(options.onScrollbackChanged ? { onScrollbackChanged: options.onScrollbackChanged } : {}),
      reloadRecordedHistory: () => {
        if (this.disposed || this.currentSource === null || this.refitPending || this.switchPending || this.reconnecting || this.transport.endpointReplacementBusy()) return false;
        this.transport.disconnect("history_reload");
        this.transport.attachAgain();
        return true;
      },
      ...(this.currentIdentity.sessionName ? { sessionName: this.currentIdentity.sessionName } : {}),
      ...(this.currentIdentity.aliasLabel ? { aliasLabel: this.currentIdentity.aliasLabel } : {}),
      ...(options.composerStorageScope ? { composerStorageScope: options.composerStorageScope } : {}),
      ...(options.stageImage ? { stageImage: options.stageImage } : {}),
      rememberSource: (source) => this.rememberSource(source),
      refitWidth: (columns: number, rows?: number) => this.refitWidth(columns, rows),
      ...(mode === "control" ? { sessionSwitch: Object.freeze({
        currentDraftScope: () => this.currentIdentity.incarnationKey,
        inventory: (refresh: boolean, signal: AbortSignal) => this.sessionInventory(refresh, signal),
        blockedMessage: (session: DashboardSession) => this.blockedSessionMessage(session),
        select: (session: DashboardSession) => this.switchSession(session),
      }) } : {}),
      ...(mode === "control" ? { takeControl: async (source: string | undefined, signal: AbortSignal) => {
		if (this.refitPending) throw new Error("refit_in_progress");
        const operation = this.operationToken;
        const identity = this.currentIdentity.incarnationKey;
        const claim = source ? Object.freeze({ source }) : this.lastControlHandle ? Object.freeze({ offer: this.lastControlHandle }) : undefined;
        if (!claim) throw new Error("takeover_unavailable");
        this.counters.takeovers += 1;
        this.options.observer?.takeoverClaim?.(source ? "source" : "offer");
        const endpoint = await takeoverEndpoint(claim, this.page.currentScrollbackRows(), signal, engine);
        this.observeAuthorityResult("takeover", "resolved", operation, identity);
        if (operation !== this.operationToken || this.disposed) {
          this.observeAuthorityResult("takeover", "discarded", operation, identity);
          throw new DOMException("takeover superseded", "AbortError");
        }
        if (!source) {
          this.lastControlHandle = undefined;
          this.lastControlHandleIdentity = null;
        }
        this.observeAuthorityResult("takeover", "recorded", operation, identity);
        return endpoint;
      } } : {}),
      onFirstCommit: () => {
        this.committedOnce = true;
        this.lastControlHandle = undefined;
        this.lastControlHandleIdentity = null;
      },
      onCommit: () => {
        this.committedOnce = true;
        this.lastControlHandle = undefined;
        this.lastControlHandleIdentity = null;
        if (this.currentIdentity.session) options.onSessionCommitted?.(this.currentIdentity.session);
      },
      ...(options.claimFocusOnCommit ? { claimFocusOnCommit: options.claimFocusOnCommit } : {}),
      ...(options.snippets ? { snippets: options.snippets } : {}),
      ...(options.preferences ? { preferences: options.preferences } : {}),
      ...(options.workspaceCell ? { workspaceCell: true } : {}),
    });
    this.transport.bind(options.observer ? this.observingSink(options.observer) : this.page);
  }

  // Non-async commit primitive: the exact operation/source identity and the
  // controller pending bit exist synchronously in the trusted tap task. All
  // fallible CSRF/network work is carried by result, never by a later mutable
  // lookup that could bind to another session.
  private refitWidth(columns: number, rows?: number): WidthRefitAttempt {
    const source = this.currentSource;
    const identity = this.currentIdentity.incarnationKey;
    const operation = randomRequestID();
    if (source === null || this.disposed || this.options.capabilityMode !== "control" || this.refitPending || this.switchPending || this.reconnecting) {
      return Object.freeze({
        operation,
        predecessorSource: source ?? "",
        predecessorIncarnation: identity ?? "",
        result: Promise.resolve(Object.freeze({ ok: false, message: "Width refit is unavailable while this pane is changing", disposition: "refused" as const, successorSource: "" })),
      });
    }
    const predecessorSource: string = source;
    this.refitPending = true;
    const operationToken = ++this.operationToken;
    const abort = new AbortController();
    this.refitAbort = abort;
    const result = this.executeRefitWidth(columns, rows, predecessorSource, identity, operation, operationToken, abort);
	this.refitResult = result;
	this.refitResultSource = predecessorSource;
    return Object.freeze({ operation, predecessorSource, predecessorIncarnation: identity ?? "", result });
  }

  private async executeRefitWidth(columns: number, rows: number | undefined, source: string, identity: string | null, operation: string, operationToken: number, abort: AbortController): Promise<WidthRefitResult> {
    try {
      const csrf = await refreshCSRFToken(abort.signal);
      if (this.disposed || operationToken !== this.operationToken || identity !== this.currentIdentity.incarnationKey || source !== this.currentSource) {
        return Object.freeze({ ok: false, message: "Width refit was superseded", disposition: "refused", successorSource: "" });
      }
      const response = await window.fetch("/api/session-refits", {
        method: "POST", cache: "no-store", credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrf },
        body: JSON.stringify({ source, columns, operation, ...(rows === undefined ? {} : { rows }) }), signal: abort.signal,
      });
      // A successful refit can already have delivered the successor PREPARE,
      // which legitimately changes currentSource. Stable incarnation plus the
      // operation token are the post-request settlement owner.
      if (this.disposed || operationToken !== this.operationToken || identity !== this.currentIdentity.incarnationKey) {
        return Object.freeze({ ok: false, message: "Width refit was superseded", disposition: "refused", successorSource: "" });
      }
      if (!response.ok) {
        const code = (await response.text()).trim();
        const message = code === "blocked_alt_screen" ? "Leave the alternate screen before refitting width"
          : code === "refit_in_progress" ? "Another pane operation is still finishing"
            : code === "slots_exhausted" ? "Width refit needs a free journal generation slot"
              : code === "stale_target" ? "This session changed before width refit began"
                : code === "refit_faulted" ? "Width changed but the new generation could not be made authoritative"
                  : "Width refit was refused";
        return Object.freeze({ ok: false, message, disposition: code === "refit_faulted" ? "terminal" : "refused", successorSource: "" });
      }
      const body: unknown = await response.json();
      if (body === null || typeof body !== "object" || Array.isArray(body)) throw new Error("malformed refit response");
      const fields = body as Record<string, unknown>;
      // The record echoes the rows the successor was built with: the typed rows
      // when they were sent, else the predecessor's — never a third number.
      if (Object.keys(fields).length !== 4 || fields.operation !== operation || fields.columns !== columns
        || !Number.isSafeInteger(fields.rows) || (fields.rows as number) <= 0 || (rows !== undefined && fields.rows !== rows)
		|| typeof fields.successor_source !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(fields.successor_source)) throw new Error("malformed refit response");
      return Object.freeze({ ok: true, message: "", disposition: "success", successorSource: fields.successor_source });
    } catch {
      return Object.freeze({ ok: false, message: "Width refit could not be completed", disposition: "uncertain", successorSource: "" });
    } finally {
      if (this.refitAbort === abort) this.refitAbort = undefined;
      if (operationToken === this.operationToken) this.refitPending = false;
    }
  }

  // Opens the initial attachment. The only network side effect of this
  // object's construction happens here, on the consumer's explicit call.
  connect(): void {
    if (this.disposed || this.connected) return;
    this.connected = true;
    this.transport.connect();
  }

  detach(reason: string): void {
    this.transport.detach(reason);
  }

  // A page entering the back/forward cache detaches. Only a pane that was live
  // or still recovering on its own reattaches when the page is restored: one
  // stopped with a notice (a control refusal, a typed stop, the burst limit)
  // keeps it, since reattaching would replay the refusal or take control back
  // from wherever the operator moved it.
  suspend(): void {
    if (this.disposed) return;
    this.resumeOnRestore = this.transport.recoveryActive();
    this.transport.detach("page_hidden");
  }

  // Reattach with fresh authority, exactly as the notice's Reconnect does.
  resume(): void {
    if (this.disposed || !this.connected || !this.resumeOnRestore) return;
    this.resumeOnRestore = false;
    this.transport.attachAgain();
  }

  // A manual control-takeover claim from the pane's host (the workspace
  // cell's own affordance): the page's one manual claim path, nothing new.
  requestControlTakeover(): void {
    if (this.disposed) return;
    this.page.requestControlTakeover();
  }

  // Ends the pane: detaches its transport and disposes its page (xterm).
  dispose(reason = "destroyed"): void {
    if (this.disposed) return;
    this.disposed = true;
    this.operationToken += 1;
    this.switchAbort?.abort();
    this.switchAbort = undefined;
    this.reloadMintAbort?.abort();
    this.reloadMintAbort = undefined;
    this.refitAbort?.abort();
    this.refitAbort = undefined;
    this.transport.detach(reason);
    this.page.destroy();
  }

  state(): UnifiedPaneControllerState {
    return Object.freeze({
      connected: this.connected, disposed: this.disposed, incarnationKey: this.currentIdentity.incarnationKey, snapshotGeneration: this.snapshotGeneration,
      ...(this.projectionDetail ? { projectionDetail: this.projectionDetail } : {}),
      counters: Object.freeze({ ...this.counters }),
    });
  }

  // Inventory detail is presentation-only and must name this controller's
  // exact resolved incarnation. Because an in-place session switch replaces
  // that incarnation, the guard reads the CURRENT identity: a caller holding a
  // stale key is refused and the detail it carried never reaches the new
  // session. It never reaches transport, capability, input, resize, takeover,
  // or terminal mutation paths.
  updateProjectionDetail(incarnationKey: string, detail: UnifiedInventoryDetail | undefined): boolean {
    if (this.disposed || incarnationKey !== this.currentIdentity.incarnationKey) return false;
    if (this.projectionDetail === detail) return true;
    this.projectionDetail = detail;
    this.notifyProjectionDetail(detail);
    return true;
  }

  // The projection observer renders a badge and nothing else. Like every
  // other observation on this controller it carries no product authority, so
  // its exception must never be read as a failure of the work that ran it:
  // inside the switch commit that would convert an already committed B into
  // a terminal `attachment_failed` verdict over a healthy session, and inside
  // the shared presentation refresh it would strand every later pane.
  private notifyProjectionDetail(detail: UnifiedInventoryDetail | undefined): void {
    try { this.options.observer?.projectionDetail?.(detail); } catch { /* observation has no product authority */ }
  }

  private async sessionInventory(_refresh: boolean, signal: AbortSignal): Promise<SessionSwitcherInventory> {
    const snapshot = await this.options.resolveInventory(signal, this.snapshotGeneration);
    if (signal.aborted || this.disposed) throw new DOMException("session inventory superseded", "AbortError");
    this.snapshotGeneration = snapshot.generation;
    return switcherInventory(snapshot.inventory);
  }

  private blockedSessionMessage(session: DashboardSession): string {
    const state = session.unified?.state;
    if (state === undefined) return "Unified terminal unavailable";
    if (state === "open" || state === "adoptable") return "";
    return unifiedBlockedMessage(state);
  }

  private runtimeIdentity(session: DashboardSession): RuntimePaneIdentity {
    const aliasLabel = session.aliases[0]?.displayAlias;
    const imageRealm = session.canStageImages ? session.realm : undefined;
    return Object.freeze({
      resolved: Object.freeze({
        selector: Object.freeze({ realm: session.realm, server: session.server, name: session.name }),
        incarnationKey: session.draftScope,
        sessionId: session.sessionId,
      }),
      incarnationKey: session.draftScope,
      sessionName: session.name,
      ...(aliasLabel ? { aliasLabel } : {}),
      ...(imageRealm ? { imageRealm } : {}),
      session,
    });
  }

  private prepareSessionSwitch(session: DashboardSession, abort: AbortController): PreparedSessionSwitch {
    const identity = this.runtimeIdentity(session);
    const historyRows = readTerminalScrollbackRows(session.draftScope);
    const endpoint = this.buildEndpoint(session.handles.control, historyRows);
    const presentation = Object.freeze({
      sessionName: session.name,
      // The tag names the session B is now bound to, alias included, so an
      // in-place switch never leaves A's alias standing over B's transcript.
      ...(identity.aliasLabel ? { aliasLabel: identity.aliasLabel } : {}),
      composerStorageScope: session.draftScope,
      historyRows,
      ...(identity.imageRealm && this.options.stageImageForRealm ? {
        stageImage: (file: File, signal: AbortSignal) => this.options.stageImageForRealm!(identity.imageRealm!, file, signal),
      } : {}),
    });
    const replaceLocation = this.options.prepareLocation?.(session);
    return Object.freeze({ abort, session, identity, endpoint, presentation, ...(replaceLocation ? { replaceLocation } : {}) });
  }

  private sessionSwitchState(): SessionSwitchBoundState {
    let urlIdentity: string | null = null;
    try { urlIdentity = new URLSearchParams(window.location.hash.replace(/^#/, "")).get("draft_scope"); } catch { /* diagnostic only */ }
    const page = this.page.sessionSwitchState();
    const transport = this.transport.sessionSwitchState();
    return Object.freeze({
      operationToken: this.operationToken,
      identity: this.currentIdentity.incarnationKey,
      urlIdentity,
      transcriptOwner: page.transcriptOwner,
      retryToken: transport.retryToken,
      socketGeneration: transport.generation,
      socketState: transport.socketState,
      draftScope: page.draftScope,
      imageRealm: this.currentIdentity.imageRealm ?? null,
      controlOfferIdentity: this.lastControlHandleIdentity,
      admission: page.admission,
    });
  }

  private observeSessionSwitch(phase: SessionSwitchCommitPhase): void {
    const observer = this.options.observer?.sessionSwitchPhase;
    if (!observer) return;
    try { observer(phase, () => this.sessionSwitchState()); } catch { /* observation has no product authority */ }
  }

  private observeAuthorityResult(kind: "source_remint" | "identity_remint" | "takeover", stage: "resolved" | "discarded" | "recorded", operation: number, identity: string | null): void {
    try {
      this.options.observer?.authorityResult?.(Object.freeze({
        kind, stage, operation, currentOperation: this.operationToken,
        identity, currentIdentity: this.currentIdentity.incarnationKey,
      }));
    } catch { /* observation has no product authority */ }
  }

  // Deliberately a void-typed, non-async method: an `await` cannot be inserted
  // into this authoritative transition without making the build fail. Every
  // fallible B value is already in `prepared`; any unexpected settlement
  // exception after identity replacement becomes a visible terminal B fault.
  private commitSessionSwitch(prepared: PreparedSessionSwitch): void {
    let crossedPointOfNoReturn = false;
    try {
      this.operationToken += 1;
      this.reloadMintAbort?.abort();
      this.reloadMintAbort = undefined;
      this.forgetReloadHandle();
      prepared.abort.abort();
      this.transport.invalidateEndpointWork();
      this.observeSessionSwitch("operation_invalidated");

      this.currentIdentity = prepared.identity;
      this.currentSource = null;
      crossedPointOfNoReturn = true;
      this.observeSessionSwitch("identity_replaced");
      this.options.onIdentityReplaced?.(prepared.identity.resolved!, prepared.session);
      // A's inventory detail is presentation-only and named A's incarnation, so
      // it does not describe B. Clear it inside the same commit: otherwise a
      // pane that switched away from a rotation-deferred session would keep
      // rendering that detail until the next shared presentation refresh.
      if (this.projectionDetail !== undefined) {
        this.projectionDetail = undefined;
        this.notifyProjectionDetail(undefined);
      }
      this.committedOnce = false;
      this.lastControlHandle = prepared.session.handles.control;
      this.lastControlHandleIdentity = prepared.identity.incarnationKey;
      this.counters.switches += 1;
      this.page.replaceSessionPresentation(prepared.presentation);
      this.observeSessionSwitch("presentation_replaced");
      prepared.replaceLocation?.();
      this.observeSessionSwitch("location_replaced");
      this.transport.replaceEndpoint(prepared.endpoint, "session_switch");
      this.observeSessionSwitch("endpoint_replaced");
    } catch {
      if (!crossedPointOfNoReturn) throw new Error("session switch commit failed before identity replacement");
      // B is authoritative. Never let this escape to the async pre-commit
      // refusal catch, and never resurrect A.
      try { this.page.failSessionSwitch("attachment_failed"); } catch { /* B remains terminal even if rendering failed */ }
      this.observeSessionSwitch("terminal_failure");
    }
  }

  private async switchSession(session: DashboardSession): Promise<Readonly<{ ok: boolean; message: string }>> {
    if (this.disposed || this.options.capabilityMode !== "control") return Object.freeze({ ok: false, message: "Switch unavailable" });
    if (this.refitPending) return Object.freeze({ ok: false, message: "Width refit is still finishing" });
    if (this.switchPending) return Object.freeze({ ok: false, message: "Switching…" });
    if (this.reconnecting || this.transport.endpointReplacementBusy()) return Object.freeze({ ok: false, message: "Reconnecting — try again" });
    if (session.draftScope === this.currentIdentity.incarnationKey) return Object.freeze({ ok: false, message: "Already open" });
    if (session.unified?.state !== "open" && session.unified?.state !== "adoptable") {
      return Object.freeze({ ok: false, message: this.blockedSessionMessage(session) });
    }

    this.switchPending = true;
    // The pending phase does not revoke A: its reconnect/remint work remains
    // valid until the one synchronous commit below makes B current.
    const operation = this.operationToken;
    const switchAbort = new AbortController();
    this.switchAbort = switchAbort;
    try {
      if (session.unified.state === "adoptable") {
        this.counters.adoptions += 1;
        const response = await window.fetch("/api/session-adoptions", {
          method: "POST", cache: "no-store", credentials: "same-origin",
          headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrfToken() },
          body: JSON.stringify({ realm: session.realm, server: session.server, session_id: session.sessionId, history_rows: ADOPTION_HISTORY_ROWS }),
          signal: switchAbort.signal,
        });
        if (operation !== this.operationToken || this.disposed) return Object.freeze({ ok: false, message: "Switch superseded" });
        if (!response.ok) {
          const raw = (await response.text().catch(() => "")).trim();
          const code = /^[a-z][a-z0-9_]{0,63}$/.test(raw) ? raw : "adoption_unavailable";
          return Object.freeze({ ok: false, message: adoptFailureMessage(code, response.status) });
        }
      }
      if (operation !== this.operationToken || this.disposed) return Object.freeze({ ok: false, message: "Switch superseded" });

      const prepared = this.prepareSessionSwitch(session, switchAbort);
      this.observeSessionSwitch("precomputed");
      this.commitSessionSwitch(prepared);
      return Object.freeze({ ok: true, message: "" });
    } catch {
      return Object.freeze({ ok: false, message: switchAbort.signal.aborted ? "Switch superseded" : "Adoption unavailable" });
    } finally {
      if (this.switchAbort === switchAbort) {
        this.switchAbort = undefined;
        this.switchPending = false;
      }
    }
  }

  private recordControlHandle(minted: string, identity: string | null): void {
    if (this.options.capabilityMode === "control" && !this.committedOnce) {
      this.lastControlHandle = minted;
      this.lastControlHandleIdentity = identity;
    }
  }

  private rememberSource(source: string): void {
    this.currentSource = source;
    if (!this.options.mintReloadHandle || !this.options.rememberReloadHandle) return;
    const operation = this.operationToken;
    const identity = this.currentIdentity.incarnationKey;
    this.reloadMintAbort?.abort();
    const abort = new AbortController();
    this.reloadMintAbort = abort;
    void this.options.mintReloadHandle(source, abort.signal).then((handle) => {
      if (abort.signal.aborted || operation !== this.operationToken || this.disposed || identity !== this.currentIdentity.incarnationKey) return;
      this.options.rememberReloadHandle!(handle);
    }).catch(() => {
      if (operation === this.operationToken && identity === this.currentIdentity.incarnationKey) this.forgetReloadHandle();
    }).finally(() => {
      if (this.reloadMintAbort === abort) this.reloadMintAbort = undefined;
    });
  }

  private forgetReloadHandle(): void {
    try { this.options.forgetReloadHandle?.(); } catch { /* reload recovery is best-effort */ }
  }

  private buildEndpoint(minted: string, historyRows = this.page.currentScrollbackRows()): AttachmentEndpoint {
    const { capabilityMode: mode, engine } = this.options;
    return Object.freeze({ url: attachmentURL(minted, mode), protocols: Object.freeze(attachmentProtocols(minted, mode, historyRows, engine)) });
  }

  // Source-binding re-mint (the fast reconnect path), capturing the handle so
  // the takeover offer stays current.
  private async mintFromSource(signal: AbortSignal, source: string): Promise<AttachmentEndpoint> {
    const operation = this.operationToken;
    const identity = this.currentIdentity.incarnationKey;
	if (this.refitResult && source === this.refitResultSource) {
		const result = await this.refitResult;
		if (!result.ok || result.successorSource === "") throw new ReconnectRefusal("refit_faulted");
	}
    this.counters.sourceRemints += 1;
    const minted = await freshHandle(source, this.options.capabilityMode, signal);
    this.observeAuthorityResult("source_remint", "resolved", operation, identity);
    if (operation !== this.operationToken || this.disposed) {
      this.observeAuthorityResult("source_remint", "discarded", operation, identity);
      throw new DOMException("endpoint superseded", "AbortError");
    }
    this.recordControlHandle(minted, identity);
    this.observeAuthorityResult("source_remint", "recorded", operation, identity);
    return this.buildEndpoint(minted);
  }

  // Identity re-mint: used when no source binding is available (a reopened
  // tab whose one-time handle was consumed) or the binding expired. It
  // resolves this pane's session by its PINNED incarnation key, adopts it when
  // it is idle, and mints a fresh handle from the inventory — so reopening the
  // page re-establishes the SAME session with no operator action. Genuinely
  // unresolvable identities reject terminally. It never resolves by name.
  private async identityEndpoint(signal: AbortSignal): Promise<AttachmentEndpoint> {
    const operation = this.operationToken;
    const observer = this.options.observer;
    this.counters.identityRemints += 1;
    const key = this.currentIdentity.incarnationKey;
    if (key === null) {
      observer?.identityResolve?.("identity_invalid");
      throw new ReconnectRefusal("identity_invalid");
    }
    const snapshot = await this.options.resolveInventory(signal, this.snapshotGeneration);
    this.observeAuthorityResult("identity_remint", "resolved", operation, key);
    if (operation !== this.operationToken || this.disposed) {
      this.observeAuthorityResult("identity_remint", "discarded", operation, key);
      throw new DOMException("identity superseded", "AbortError");
    }
    this.snapshotGeneration = snapshot.generation;
    const resolution = resolveDraftScope(snapshot.inventory, key);
    if (resolution.kind === "missing") {
      observer?.identityResolve?.("session_gone");
      throw new ReconnectRefusal("session_gone");
    }
    if (resolution.kind === "ambiguous") {
      observer?.identityResolve?.("identity_ambiguous");
      throw new ReconnectRefusal("identity_ambiguous");
    }
    const session = resolution.session;
    if (session.unified?.state === "adoptable") {
      this.counters.adoptions += 1;
      const adopt = await window.fetch("/api/session-adoptions", {
        method: "POST", cache: "no-store", credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrfToken() },
        body: JSON.stringify({ realm: session.realm, server: session.server, session_id: session.sessionId, history_rows: ADOPTION_HISTORY_ROWS }), signal,
      });
      this.observeAuthorityResult("identity_remint", "resolved", operation, key);
      if (operation !== this.operationToken || this.disposed) {
        this.observeAuthorityResult("identity_remint", "discarded", operation, key);
        throw new DOMException("identity superseded", "AbortError");
      }
      if (!adopt.ok) throw new Error("adoption unavailable");
    }
    const minted = session.handles[this.options.capabilityMode];
    if (operation !== this.operationToken || this.disposed) {
      this.observeAuthorityResult("identity_remint", "discarded", operation, key);
      throw new DOMException("identity superseded", "AbortError");
    }
    this.recordControlHandle(minted, key);
    this.observeAuthorityResult("identity_remint", "recorded", operation, key);
    observer?.identityResolve?.("minted");
    return this.buildEndpoint(minted);
  }

  // A forwarding sink: every transport callback reaches the page unchanged
  // and its verdict is returned unchanged; the observer only watches.
  private observingSink(observer: UnifiedPaneObserver): AttachmentTransportSink {
    const page = this.page;
    return Object.freeze({
      openTransport: (generation: number) => {
        page.openTransport(generation);
        observer.openTransport?.(generation);
      },
      receiveDecoded: (generation: number, value: unknown) => {
        const verdict = page.receiveDecoded(generation, value);
        if (observer.frame && value !== null && typeof value === "object") {
          const record = value as Record<string, unknown>;
          observer.frame(generation, Object.freeze({
            type: String(record.type),
            ...(typeof record.kind === "string" ? { kind: record.kind } : {}),
            ...(typeof record.columns === "number" ? { columns: record.columns } : {}),
            ...(typeof record.rows === "number" ? { rows: record.rows } : {}),
            ...(typeof record.mode === "string" ? { mode: record.mode } : {}),
            ...(typeof record.reason === "string" ? { reason: record.reason } : {}),
          }), verdict);
        }
        return verdict;
      },
      transportClosed: (generation: number, reason: string) => {
        // The observer hears the close first: the page may react to it at
        // once (a burst stop detaches, a lease_held claim reopens), and those
        // reactions must reach the observer after the close that caused them,
        // not be overwritten by it.
        observer.transportClosed?.(generation, reason);
        page.transportClosed(generation, reason);
      },
      reconnectStatus: (status: ReconnectStatus) => {
        this.reconnecting = status.state === "WAITING" || status.state === "ATTEMPTING";
        page.reconnectStatus(status);
        observer.reconnectStatus?.(status);
      },
      operationalRefusal: (generation: number, code: string) => {
        page.operationalRefusal(generation, code);
        observer.operationalRefusal?.(generation, code);
      },
    });
  }
}
