import "./attachment_page.css";
import "./app.css";
import "./dashboard.css";
import "./scrollback_control.css";
import { readScrollbackRows, readTerminalScrollbackRows } from "./scrollback_preferences";
import { installTapFeedback } from "./tap_feedback";

export type { AttachmentPagePort, FinalizeCause, FinalizeIntent, PortSendResult } from "./attachment_port";
export type { BrowserFrame, DecodedAttachmentFrame, ServerFrame } from "./attachment_protocol";
export { decodeBrowserFrame, decodeServerFrame, encodeBrowserFrame, encodeServerFrame } from "./attachment_wire";
export { WebSocketAttachmentTransport } from "./websocket_attachment_transport";

import { validateComposerStorageScope } from "./composer_storage_scope";
import { UnifiedPaneController, fetchInventory, freshHandle, type ResolvedPaneIdentity, type SessionSwitchBoundState, type SessionSwitchCommitPhase, type UnifiedPaneObserver } from "./unified_pane_controller";
import { Dashboard, parseHistoryChoice } from "./dashboard";
import { stageSessionImage } from "./image_staging_client";
import { createCommittedSessionRecorder, dropPendingSession, pendingIdentityFromSession, sessionScopeIdentity, takePendingSession, writeLastSession } from "./session_memory";
import { SnippetService, deviceOrigin } from "./snippet_client";
import { OperatorPreferencesService } from "./operator_preferences";
import { bootWorkspace } from "./workspace_page";

/**
 * Bounds a fragment-supplied label before it is displayed. Control characters are
 * refused rather than stripped: a value containing them is not a tmux session name
 * and silently rewriting it would show the operator something that does not exist.
 */
function labelFromFragment(value: string | null): string | undefined {
  if (value === null) return undefined;
  const trimmed = value.trim();
  if (trimmed === "" || trimmed.length > 128) return undefined;
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(trimmed)) return undefined;
  return trimmed;
}
/**
 * The realm whose staging endpoint the composer may POST images to, carried
 * by the dashboard exactly when the inventory advertised the capability. Like
 * name/alias it is display/UX-level trust with no authority: the server
 * validates the realm by exact name and re-authorizes every upload, so a
 * crafted value can only produce a 400. A value outside the realm-label shape
 * is dropped rather than rejected — the terminal still opens, without the
 * image affordance.
 */
function imageRealmFromFragment(value: string | null): string | undefined {
  if (value === null || !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(value)) return undefined;
  return value;
}
function renderFatal(root: HTMLElement, error: unknown): void {
  const message = document.createElement("p");
  message.className = "persea-terminal-fatal";
  message.textContent = error instanceof Error ? error.message : "Persea Terminal failed to start";
  root.replaceChildren(message);
}

function documentStyleNonce(): string {
  const nodes = document.querySelectorAll<HTMLMetaElement>('meta[name="persea-style-nonce"]');
  if (nodes.length !== 1) throw new Error("terminal style authority is unavailable");
  const nonce = nodes[0]?.content ?? "";
  if (!/^[A-Za-z0-9+/]{22}$/.test(nonce)) throw new Error("terminal style authority is malformed");
  return nonce;
}

function wasDocumentReload(): boolean {
  const [entry] = window.performance.getEntriesByType("navigation") as PerformanceNavigationTiming[];
  return entry?.type === "reload";
}

const UNIFIED_RELOAD_HANDLE_KEY = "persea-unified-terminal-reload-handle-v1";

function takeUnifiedReloadHandle(): string | undefined {
  const handle = window.sessionStorage.getItem(UNIFIED_RELOAD_HANDLE_KEY) ?? "";
  window.sessionStorage.removeItem(UNIFIED_RELOAD_HANDLE_KEY);
  if (/^[A-Za-z0-9_-]{43}$/.test(handle)) return handle;
  return undefined;
}

async function boot(): Promise<void> {
  const root = document.querySelector<HTMLElement>("#app");
  if (!root) return;
  const styleNonce = documentStyleNonce();
  // The workspace document is decided BEFORE the terminal fragment
  // self-heal below: its fragment contract is {name} only, `engine` in a
  // workspace fragment is a typed refusal, and a workspace is never redirected
  // to /terminal.
  if (window.location.pathname === "/workspace") {
    await bootWorkspace({ root, styleNonce });
    return;
  }
  const fragment = new URLSearchParams(window.location.hash.slice(1));
  const fragmentEngine = fragment.get("engine");
  if (fragmentEngine === "unified-dev" && (window.location.pathname !== "/terminal" || window.location.search !== "?engine=unified-dev")) {
    const capabilityURL = new URL(window.location.href);
    capabilityURL.pathname = "/terminal";
    capabilityURL.search = "?engine=unified-dev";
    window.location.replace(capabilityURL);
    return;
  }
  const search = new URLSearchParams(window.location.search);
  const searchKeys: string[] = [];
  search.forEach((_value, key) => searchKeys.push(key));
  if (searchKeys.length > 1
    || (searchKeys.length === 1
      && !((searchKeys[0] === "engine" && search.get("engine") === "unified-dev")
        // the landing flag of the dashboard and the PWA `start_url`. It
        // carries no authority whatsoever — the dashboard reads it only to
        // decide which card to focus — but it must be an exact known value so
        // the "authority lives in the fragment" refusal keeps its meaning.
        || (searchKeys[0] === "resume" && search.get("resume") === "1")))) {
    throw new Error("terminal authority must be in URL fragment");
  }
  const query = fragment;
  const seen = new Set<string>();
  // `open_id` (session memory, operation-owned-candidate) names the one staged last-session candidate this
  // navigation may claim. It carries no authority: it selects a local storage
  // entry, and the page still needs a valid handle, an agreeing draft scope and
  // a real COMMIT before anything is recorded.
  const allowed = new Set(["handle", "mode", "history", "name", "alias", "draft_scope"]);
  allowed.add("engine");
  allowed.add("image_realm");
  allowed.add("open_id");
  query.forEach((_value, key) => {
    if (!allowed.has(key) || seen.has(key)) throw new Error("terminal fragment fields must be unique and known");
    seen.add(key);
  });
  let handle = query.get("handle");
  const draftScopeFragment = query.get("draft_scope");
  const composerStorageScope = validateComposerStorageScope(draftScopeFragment);
  if (handle === null && query.get("engine") === "unified-dev" && wasDocumentReload()) {
    handle = takeUnifiedReloadHandle() ?? null;
  }
  if (handle === null) {
    new Dashboard(root).mount();
    return;
  }
  if (handle.length === 0) throw new Error("handle must be nonempty");
  const requestedMode = query.get("mode");
  if (requestedMode !== null && requestedMode !== "observe" && requestedMode !== "control") {
    throw new Error("mode must be observe or control");
  }
  const mode = requestedMode ?? "control";
  const engineValue = query.get("engine");
  if (engineValue !== "unified-dev") throw new Error("engine must be unified-dev");
  const engine = engineValue;
  if (wasDocumentReload()) {
    handle = takeUnifiedReloadHandle() ?? handle;
  }
  let historyRows = readTerminalScrollbackRows(draftScopeFragment, query.has("history") ? parseHistoryChoice(query.get("history")) : readScrollbackRows());
  // Display-only labelling. These come from the fragment, so they are
  // client-supplied and carry no authority whatsoever: the session that is
  // actually attached is fixed by the handle. They are rendered as text so a
  // crafted value can misname a pane but never do anything.
  const sourceLabel = labelFromFragment(query.get("name"));
  const aliasLabel = labelFromFragment(query.get("alias"));
  const imageRealm = imageRealmFromFragment(query.get("image_realm"));
  // One unified pane: the single-terminal page is one consumer of the
  // per-pane controller (a workspace is N). Page-level behaviour — the
  // one-shot reload handle in sessionStorage — stays here as callbacks;
  // the controller owns the
  // per-pane claim state, re-mint paths, transport, and page.
  // The device remembers this
  // session only after a real first COMMIT, AND only the identity that the
  // trusted dashboard action staged from the authoritative inventory.
  //
  // Nothing here is read out of the fragment. A COMMIT proves that *a* handle
  // attached; it does not prove that the fragment's `draft_scope` names that
  // attachment, so a URL pairing a valid handle with another live
  // incarnation's scope must not be able to rename "last session". The
  // fragment scope is passed only as a cross-check: it must agree with the
  // candidate, or nothing is recorded. A hand-edited or directly typed
  // terminal URL has no candidate at all — it attaches normally and simply
  // never rewrites the memory.
  //
  // The candidate is consumed here (single use) and tied to this one
  // controller operation; every other outcome drops it without replay.
  // This page may claim ONLY the candidate staged for its own navigation. A
  // page with no scope drops the candidate instead of consuming it, so a
  // hand-edited scope-less URL can neither use nor keep someone else's.
  const openId = query.get("open_id");
  let staged;
  if (typeof draftScopeFragment === "string" && draftScopeFragment !== "") {
    staged = takePendingSession(window.localStorage, openId);
  } else {
    dropPendingSession(window.localStorage, openId);
  }
  const committedSession = createCommittedSessionRecorder({
    mode: "navigation",
    storage: window.localStorage,
    identity: staged,
    pageDraftScope: draftScopeFragment,
  });
  window.addEventListener("pagehide", () => committedSession.abandon());
  // ONE snippets/clips service per document (clipboard). Constructing it makes
  // no request: it reads the store only once a pane's sheet puts a list on
  // screen, and every pane in this document shares that one poll loop.
  const snippets = new SnippetService({
    origin: deviceOrigin(window),
    ...(typeof navigator !== "undefined" && navigator.clipboard ? { clipboard: navigator.clipboard } : {}),
  });
  const decodedScope = draftScopeFragment ? sessionScopeIdentity(draftScopeFragment) : undefined;
  const initialIdentity: ResolvedPaneIdentity | undefined = decodedScope ? Object.freeze({
    selector: Object.freeze({ realm: decodedScope.realm, server: decodedScope.server, name: sourceLabel ?? decodedScope.sessionId }),
    incarnationKey: draftScopeFragment!,
    sessionId: decodedScope.sessionId,
  }) : undefined;
  const switchProbe = (window as unknown as Readonly<{
    __perseaSessionSwitchSwitchProbe?: (phase: SessionSwitchCommitPhase, sample: () => SessionSwitchBoundState) => void;
    __perseaSessionSwitchAuthorityProbe?: NonNullable<UnifiedPaneObserver["authorityResult"]>;
  }>).__perseaSessionSwitchSwitchProbe;
  const authorityProbe = (window as unknown as Readonly<{
    __perseaSessionSwitchAuthorityProbe?: NonNullable<UnifiedPaneObserver["authorityResult"]>;
  }>).__perseaSessionSwitchAuthorityProbe;
  // one server preference read per document, completed before xterm
  // construction so the authoritative font baseline precedes the first fit.
  // Failure resolves to bounded defaults; it never blocks attachment use.
  const preferences = new OperatorPreferencesService();
  await preferences.load();
  // Other tabs (the dashboard's Appearance card) may change the record
  // while this terminal is open; it follows without a reload.
  preferences.watchExternalChanges({ refetchOnForeground: true });
  installTapFeedback(document);
  const controller = new UnifiedPaneController({
    root, styleNonce, capabilityMode: mode, engine, historyRows, initialHandle: handle,
    onScrollbackChanged: rows => {
      historyRows = rows;
      const current = new URL(window.location.href);
      const fragment = new URLSearchParams(current.hash.slice(1));
      fragment.set("history", String(rows));
      current.hash = fragment.toString();
      window.history.replaceState(window.history.state, "", current);
    },
    incarnationKey: draftScopeFragment,
    snippets,
    ...(initialIdentity ? { initialIdentity } : {}),
    preferences,
    // This page owns exactly one controller, so its first transport open is
    // the operation the trusted action started; the recorder is armed from it
    // rather than latching whichever generation happens to call first.
    observer: Object.freeze({
      ...committedSession,
      openTransport: (generation: number): void => { committedSession.arm(generation); },
      ...(typeof switchProbe === "function" ? { sessionSwitchPhase: switchProbe } : {}),
      ...(typeof authorityProbe === "function" ? { authorityResult: authorityProbe } : {}),
    }),
    resolveInventory: async (signal) => Object.freeze({ generation: 0, inventory: await fetchInventory(signal) }),
    ...(sourceLabel ? { sessionName: sourceLabel } : {}),
    ...(aliasLabel ? { aliasLabel } : {}),
    ...(imageRealm ? { imageRealm } : {}),
    ...(composerStorageScope ? { composerStorageScope } : {}),
    ...(imageRealm ? { stageImage: (file: File, signal: AbortSignal) => stageSessionImage(imageRealm, file, signal) } : {}),
    stageImageForRealm: (realm: string, file: File, signal: AbortSignal) => stageSessionImage(realm, file, signal),
    mintReloadHandle: (source: string, signal: AbortSignal) => freshHandle(source, mode, signal),
    rememberReloadHandle: (reloadHandle: string) => window.sessionStorage.setItem(UNIFIED_RELOAD_HANDLE_KEY, reloadHandle),
    forgetReloadHandle: () => window.sessionStorage.removeItem(UNIFIED_RELOAD_HANDLE_KEY),
    prepareLocation: (session) => {
      const clean = new URL("/terminal", window.location.href);
      clean.searchParams.set("engine", "unified-dev");
      clean.hash = new URLSearchParams({
        mode: "control", history: String(readTerminalScrollbackRows(session.draftScope)), engine: "unified-dev",
        name: session.name,
        ...(session.aliases[0]?.displayAlias ? { alias: session.aliases[0].displayAlias } : {}),
        draft_scope: session.draftScope,
        ...(session.canStageImages ? { image_realm: session.realm } : {}),
      }).toString();
      return () => window.history.replaceState(window.history.state, "", clean);
    },
    onSessionCommitted: (session) => {
      const identity = pendingIdentityFromSession(session);
      if (identity) writeLastSession(window.localStorage, Object.freeze({ ...identity, at: Date.now() }));
    },
  });
  controller.connect();
  window.addEventListener("pagehide", () => controller.detach("page_hidden"));
}

void boot().catch((error: unknown) => {
  const root = document.querySelector<HTMLElement>("#app");
  if (root) renderFatal(root, error);
});
