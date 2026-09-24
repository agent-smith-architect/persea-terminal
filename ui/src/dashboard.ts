import { installTapFeedback } from "./tap_feedback";
import { hasDraft, preserveFocus, reconcileChildren } from "./dashboard_dom";
import { compareSessionNames, sessionMetadata, SESSION_METADATA_HELP } from "./session_metadata";
import { readSessionDiscovery, type SessionDiscovery } from "./session_discovery";
import { DashboardFavorites } from "./dashboard_favorites";
import { renderTerminalPreview } from "./terminal_preview";
import { SCROLLBACK_CHOICES as HISTORY_CHOICES, ADOPTION_HISTORY_ROWS, SCROLLBACK_STORAGE_KEY, TERMINAL_SCROLLBACK_STORAGE_KEY, type ScrollbackRows as HistoryChoice, createScrollbackControl, readScrollbackRows, readTerminalScrollbackRows, saveScrollbackRows } from "./scrollback_preferences";
import { defaultSessionIsOffered, defaultSessionState, landingMemoryState, parseDefaultSessionPreference, newOperationId, pendingIdentityFromSession, readLastSession, stagePendingSession, type DefaultSessionPreference, type DefaultSessionState, type LastSessionRecord } from "./session_memory";
import { leaf, type SessionSelector } from "./workspace_model";
import { COMPOSER_FONT_SIZE_MAX, COMPOSER_FONT_SIZE_MIN, OperatorPreferencesService, type OperatorPreferenceOutcome, type OperatorPreferencePatch, type OperatorPreferenceSnapshot } from "./operator_preferences";
import { KeyboardSettings } from "./keyboard_settings";
import { ClipboardPanel } from "./clipboard_panel";
import { ClipboardImages } from "./clipboard_images";
import { ClipboardPreferencesService } from "./clipboard_preferences";
import { SnippetService, deviceOrigin } from "./snippet_client";
import { UNIFIED_THEME_IDS, isUnifiedThemeID, unifiedTheme } from "./unified_themes";

// The preferences service validates the terminal font and the composer text
// in the same 9–24 px range; the Appearance card offers exactly that range.
const APPEARANCE_FONT_SIZES: readonly number[] = Object.freeze(
  Array.from({ length: COMPOSER_FONT_SIZE_MAX - COMPOSER_FONT_SIZE_MIN + 1 }, (_unused, index) => COMPOSER_FONT_SIZE_MIN + index),
);
import { WorkspaceAPI, WorkspaceAPIError, workspaceAPIMessage, type WorkspaceRecord } from "./workspace_api";
import { workspaceURL } from "./workspace_url";
import { PHONE_STATE_NOTICE, stablePostureEnvironment, workspacePosture } from "./workspace_posture";
import { unifiedInventoryDetail, type UnifiedInventoryDetail } from "./unified_close_policy";
import { effectiveCollapse, filterSessionList, normalizeSessionFilter, sessionListPresentation, sessionMatchesFilter, type SessionListPresentation, type SessionListRealm, type SessionListServer } from "./session_switcher";
export { effectiveCollapse, filterSessionList, normalizeSessionFilter, sessionListPresentation, sessionMatchesFilter } from "./session_switcher";
export type { SessionListPresentation, SessionListRealm, SessionListServer } from "./session_switcher";

export type AttachmentMode = "observe" | "control";
export { HISTORY_CHOICES };
export type { HistoryChoice };
export const DEFAULT_HISTORY_CHOICE: HistoryChoice = 1_000;
export const HISTORY_CHOICE_ERROR = `history must be one of ${HISTORY_CHOICES.join(", ")}`;
export function parseHistoryChoice(value: string | null): HistoryChoice {
  const normalized = value ?? String(DEFAULT_HISTORY_CHOICE);
  const match = HISTORY_CHOICES.find((choice) => String(choice) === normalized);
  if (match === undefined) throw new Error(HISTORY_CHOICE_ERROR);
  return match;
}
export type DashboardAlias = Readonly<{ aliasId: string; displayAlias: string; revision: number; state: string }>;
export type UnifiedSessionProjection =
  | Readonly<{ state: "open"; origin: "birth" | "reconstructed"; detail?: UnifiedInventoryDetail }>
  | Readonly<{ state: "adoptable" }>
  | Readonly<{ state: "blocked_alt_screen" | "blocked_multi_pane" | "blocked_multi_window" | "blocked_foreign_server" | "slots_exhausted" | "unavailable" }>;
export type DashboardSession = Readonly<{ handles: Readonly<{ alias: string; observe: string; control: string }>; realm: string; uid: number; server: string; serverStatus: string; sessionId: string; name: string; width: number; height: number; attached: number; activity: number; outputActivity?: number; aliases: readonly DashboardAlias[]; draftScope: string; unified?: UnifiedSessionProjection; canStageImages?: true }>;
export type UnifiedDevLaunch = Readonly<{ state: "create"; name: string }>;
export type DashboardServer = Readonly<{ realm: string; label: string; status: string; error?: string; canCreate: boolean; sessions: readonly DashboardSession[]; unifiedDev?: UnifiedDevLaunch }>;
export type DashboardRealm = Readonly<{ name: string; displayName: string; uid?: number; error?: string; servers: readonly DashboardServer[] }>;
export type DashboardInventory = Readonly<{ realms: readonly DashboardRealm[]; detachedAliases: readonly DashboardAlias[] }>;
type ObjectValue = Record<string, unknown>;
type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
export function csrfToken(cookie=document.cookie): string { const values=cookie.split(";").map(v=>v.trim()).filter(v=>v.startsWith("__Host-persea-terminal-csrf=")); if(values.length!==1)throw new Error("CSRF cookie unavailable");const token=values[0].slice(values[0].indexOf("=")+1);if(!/^[A-Za-z0-9_-]{43}$/.test(token))throw new Error("CSRF cookie malformed");return token; }

function object(value: unknown, label: string): ObjectValue { if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`); return value as ObjectValue; }
function array(value: unknown, label: string): unknown[] { if (!Array.isArray(value)) throw new Error(`${label} must be an array`); return value; }
function string(value: unknown, label: string, allowEmpty = false): string { if (typeof value !== "string" || (!allowEmpty && value.length === 0)) throw new Error(`${label} must be a nonempty string`); return value; }
function integer(value: unknown, label: string, minimum = 0): number { if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum) throw new Error(`${label} must be an integer >= ${minimum}`); return value; }
// Absent means false: an older broker that does not report the field simply
// offers no creation affordance, rather than the UI guessing that it may.
function boolean(value: unknown, label: string): boolean { if (value === undefined || value === null) return false; if (typeof value !== "boolean") throw new Error(`${label} must be a boolean`); return value; }
function optionalString(value: unknown, label: string): string | undefined {return value === undefined ? undefined : string(value, label); }

// Broker-internal attachment wrapper sessions (workspace access F3).
//
// While an attachment is live the broker owns one extra tmux session named
// `persea-attach-<32 hex nonce>` (`internal/broker/attachment.go`
// attachmentShadowPrefix; `internal/broker/session_create.go` describes it as
// "an internal wrapper with witness and cleanup invariants", as opposed to an
// ordinary operator session). It is not something an operator created, may
// attach to, or should be counted among their sessions, and it disappears when
// the attachment ends.
//
// There is no typed marker to prefer: the inventory wire record
// (`proto.Session` / `proto.ServerInventory` in internal/proto/control.go)
// carries no "internal" flag, so the exact documented name shape is the only
// witness the browser has. It is matched exactly — the literal prefix plus the
// nonce the broker mints — so an operator session merely *starting* with those
// characters is still shown. The nonce shape is the broker's own recognizer
// (`inspectOrphanShadow`: the prefix, then exactly attachmentNonceHexLength
// characters that hex-decode, which accepts either case; minting is `%x` of 16
// random bytes, so 32 lowercase hex in practice). This is deliberately the
// single place the rule lives: every operator-visible list (dashboard rows,
// dashboard counts, the in-terminal switcher, workspace pickers) is derived
// from parseInventory.
const BROKER_INTERNAL_SESSION = /^persea-attach-[0-9a-fA-F]{32}$/;
export function brokerInternalSessionName(name: string): boolean { return BROKER_INTERNAL_SESSION.test(name); }
type ParsedAlias = DashboardAlias & { incarnationKey: string };

function authorityKey(value: unknown, label: string): { key: string; uid: number } {
  const v = object(value, label);
  const fields = ["realm", "server", "selector_kind", "selector_value", "boot_id", "session_id"] as const;
  const strings = fields.map((field) => string(v[field], `${label}.${field}`));
  const uid = integer(v.uid, `${label}.uid`);
  return { key: JSON.stringify([...strings, uid, integer(v.server_pid, `${label}.server_pid`, 1), integer(v.server_start, `${label}.server_start`, 1), integer(v.session_created, `${label}.session_created`, 1)]), uid };
}

function parseAlias(value: unknown, label: string): ParsedAlias {
  const v = object(value, label); const incarnation = authorityKey(v.session_incarnation, `${label}.session_incarnation`);
  return { aliasId: string(v.alias_id, `${label}.alias_id`), displayAlias: string(v.display_alias, `${label}.display_alias`), revision: integer(v.revision, `${label}.revision`, 1), state: string(v.state, `${label}.state`), incarnationKey: incarnation.key };
}

function parseUnifiedDev(value: unknown, label: string): UnifiedDevLaunch | undefined {
  if (value === undefined) return undefined;
  try {
    const v = object(value, label);
    const state = string(v.state, `${label}.state`);
    const name = string(v.name, `${label}.name`);
    const keys = Object.keys(v).sort().join(",");
    if (state === "create" && keys === "name,state") return { state, name };
  } catch {
    // An invalid optional projection disables only the experimental action.
  }
  return undefined;
}

const UNIFIED_BLOCKED_STATES = Object.freeze(["blocked_alt_screen", "blocked_multi_pane", "blocked_multi_window", "blocked_foreign_server", "slots_exhausted", "unavailable"] as const);

// Strict key-set parsing, same discipline as parseUnifiedDev: an unknown
// state, a misplaced origin/detail, or an extra field hides only this row's unified
// affordance. The row remains visible with its availability explanation.
function parseUnifiedSession(value: unknown, label: string): UnifiedSessionProjection | undefined {
  if (value === undefined) return undefined;
  try {
    const v = object(value, label);
    const state = string(v.state, `${label}.state`);
    const keys = Object.keys(v).sort().join(",");
    if (state === "open" && (keys === "origin,state" || keys === "detail,origin,state")) {
      const origin = string(v.origin, `${label}.origin`);
      const detail = v.detail === undefined ? undefined : unifiedInventoryDetail(string(v.detail, `${label}.detail`));
      if (v.detail !== undefined && detail === undefined) return undefined;
      if (origin === "birth" || origin === "reconstructed") return { state, origin, ...(detail ? { detail } : {}) };
      return undefined;
    }
    if (keys !== "state") return undefined;
    if (state === "adoptable") return { state };
    const blocked = UNIFIED_BLOCKED_STATES.find((candidate) => candidate === state);
    if (blocked) return { state: blocked };
  } catch {
    // An invalid optional projection disables only this row's unified action.
  }
  return undefined;
}

export function parseInventory(value: unknown): DashboardInventory {
  const root = object(value, "inventory");
  // Both processes consent independently: the front door's endpoint gate
  // (top-level) AND the realm broker's own advertisement (per server) must
  // agree before a session offers the image affordance. Either absent means
  // no affordance, exactly like can_create.
  const imageUpload = boolean(root.image_upload, "inventory.image_upload");
  const aliases = array(root.aliases, "inventory.aliases").map((item, i) => parseAlias(item, `inventory.aliases[${i}]`));
  const aliasesByAuthority = new Map<string, ParsedAlias[]>(); const projected = new Set<string>();
  for (const alias of aliases) { const matches = aliasesByAuthority.get(alias.incarnationKey); if (matches) matches.push(alias); else aliasesByAuthority.set(alias.incarnationKey, [alias]); }
  const realms = array(root.realms, "inventory.realms").map((realmValue, ri): DashboardRealm => {
    const realm = object(realmValue, `inventory.realms[${ri}]`); const name = string(realm.name, `inventory.realms[${ri}].name`); const displayName = optionalString(realm.display_name, `inventory.realms[${ri}].display_name`) ?? name; const error = optionalString(realm.error, `inventory.realms[${ri}].error`); let realmUID: number | undefined;
    const servers = array(realm.servers, `inventory.realms[${ri}].servers`).map((serverValue, si): DashboardServer => {
      const sp = `inventory.realms[${ri}].servers[${si}]`; const server = object(serverValue, sp); const label = string(server.label, `${sp}.label`); const status = string(server.status, `${sp}.status`); const serverError = optionalString(server.error, `${sp}.error`);
      const canStageImages = imageUpload && boolean(server.can_stage_images, `${sp}.can_stage_images`);
      const sessions = array(server.sessions, `${sp}.sessions`).map((sessionValue, xi): DashboardSession => {
        const path = `${sp}.sessions[${xi}]`; const session = object(sessionValue, path); const authority = authorityKey(session.authority, `${path}.authority`); const sessionRealm = string(session.realm, `${path}.realm`); const sessionServer = string(session.server, `${path}.server`);
        if (sessionRealm !== name || sessionServer !== label) throw new Error(`${path} identity does not match its group`);
        if (realmUID !== undefined && realmUID !== authority.uid) throw new Error(`realm ${name} contains inconsistent UIDs`); realmUID = authority.uid;
        const sessionName = string(session.name, `${path}.name`);
        // A wrapper is not an operator session, so an alias bound to its
        // incarnation is not "projected" by any row the operator can see: it
        // must keep surfacing as a detached alias rather than disappearing
        // behind a row this parser is about to drop.
        const matchingAliases = brokerInternalSessionName(sessionName) ? [] : aliasesByAuthority.get(authority.key) ?? []; for (const alias of matchingAliases) projected.add(alias.aliasId);
        const handles=object(session.handles,`${path}.handles`); const unified = parseUnifiedSession(session.unified, `${path}.unified`); return { handles:{alias:string(handles.alias,`${path}.handles.alias`),observe:string(handles.observe,`${path}.handles.observe`),control:string(handles.control,`${path}.handles.control`)}, realm: sessionRealm, uid: authority.uid, server: sessionServer, serverStatus: string(session.server_status, `${path}.server_status`), sessionId: string(session.session_id, `${path}.session_id`), name: sessionName, width: integer(session.width, `${path}.width`, 1), height: integer(session.height, `${path}.height`, 1), attached: integer(session.attached, `${path}.attached`), activity: integer(session.activity, `${path}.activity`), ...(session.output_activity === undefined ? {} : { outputActivity: integer(session.output_activity, `${path}.output_activity`) }), aliases: matchingAliases.map(({ incarnationKey: _key, ...alias }) => alias), draftScope: authority.key, ...(unified ? { unified } : {}), ...(canStageImages ? { canStageImages: true as const } : {}) };
      // Validation stays total: every row is parsed (and rejected on shape)
      // before any is dropped, so a malformed wrapper is still an error.
      }).filter((session) => !brokerInternalSessionName(session.name));
      const unifiedDev = parseUnifiedDev(server.unified_dev, `${sp}.unified_dev`);
      return { realm: name, label, status, ...(serverError ? { error: serverError } : {}), canCreate: boolean(server.can_create, `${sp}.can_create`), sessions, ...(unifiedDev ? { unifiedDev } : {}) };
    });
    return { name, displayName, ...(realmUID === undefined ? {} : { uid: realmUID }), ...(error ? { error } : {}), servers };
  });
  return { realms, detachedAliases: aliases.filter((alias) => !projected.has(alias.aliasId)).map(({ incarnationKey: _key, ...alias }) => alias) };
}

// labels are display-only: they name the session on the page and carry no
// authority, which is fixed by the handle.
export function terminalURL(base: string, handle: string, mode: AttachmentMode, history: HistoryChoice = DEFAULT_HISTORY_CHOICE, labels: { name?: string; alias?: string } = {}, draftScope?: string): string { const url = new URL("/terminal", base); url.search = ""; url.hash = new URLSearchParams({ handle, mode, history: String(history), ...(labels.name ? { name: labels.name } : {}), ...(labels.alias ? { alias: labels.alias } : {}), ...(draftScope !== undefined ? { draft_scope: draftScope } : {}) }).toString(); return url.toString(); }
// `openId` names the ONE staged candidate this navigation may claim (operation-owned-candidate).
// It is not an authority and is not derived from one: it grants nothing, it
// names a local storage entry, and the target still needs a valid handle, a
// matching draft scope and a real COMMIT before anything is recorded. Omitted
// by callers that only render a link and stage nothing.
export function unifiedTerminalURL(base: string, handle: string, session: DashboardSession, openId?: string, history: HistoryChoice = readScrollbackRows()): string {
  const labels = { name: session.name, ...(session.aliases.length ? { alias: session.aliases[0].displayAlias } : {}) };
  const url = new URL(terminalURL(base, handle, "control", readTerminalScrollbackRows(session.draftScope, history), labels, session.draftScope));
  url.search = new URLSearchParams({ engine: "unified-dev" }).toString();
  const fragment = new URLSearchParams(url.hash.slice(1));
  fragment.set("engine", "unified-dev");
  if (openId !== undefined) fragment.set("open_id", openId);
  // Present exactly when the inventory advertised image staging for this
  // session's realm; display/UX-only trust — the server re-authorizes every
  // upload by exact realm name.
  if (session.canStageImages) fragment.set("image_realm", session.realm);
  url.hash = fragment.toString();
  return url.toString();
}
// The workspace route constructor is authored once in
// workspace_url.ts beside its parser; the dashboard re-exports it so every
// dashboard entry point composes workspace URLs through the same function.
export { workspaceURL };
export function aliasRequest(alias: DashboardAlias | undefined, handle: string, displayAlias?: string): { url: string; init: RequestInit } {
  if (!alias) return { url: "/api/aliases", init: { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ display_alias: displayAlias ?? "", handle }) } };
  const url = `/api/aliases/${encodeURIComponent(alias.aliasId)}`; const headers = { "Content-Type": "application/json", "If-Match": `"${alias.revision}"` };
  return displayAlias === undefined ? { url, init: { method: "DELETE", headers, body: JSON.stringify({ handle }) } } : { url, init: { method: "PATCH", headers, body: JSON.stringify({ display_alias: displayAlias, handle }) } };
}
/**
 * Operator wording for a creation refusal. The server sends a code from a closed
 * set and never free text, so an unrecognised code falls back to a neutral
 * message rather than being shown raw: a future code must not be able to put
 * unreviewed words on the page.
 */
export function createFailureMessage(code: string, status: number): string {
  switch (code.trim()) {
    case "invalid_name": return "That name is not allowed for this realm.";
    case "not_permitted": return "This realm does not allow creating sessions here.";
    case "name_taken": return "A session with that name already exists.";
    case "at_capacity": return "This realm is already at its session limit.";
    case "server_unavailable": return "The tmux server is unavailable.";
    default: return status === 503 ? "The realm is unreachable." : "The session could not be created.";
  }
}
/**
 * Row wording for a session the unified terminal cannot open right now. Same
 * closed-set rule as creation refusals: states come from the strict parser, so
 * every reachable value has reviewed copy here.
 */
export function unifiedBlockedMessage(state: Exclude<UnifiedSessionProjection, { state: "open" | "adoptable" }>["state"]): string {
  switch (state) {
    case "blocked_alt_screen": return "A full-screen app is active. Retry after it exits.";
    case "blocked_multi_pane": return "This session has multiple panes; the unified terminal opens single-pane sessions.";
    case "blocked_multi_window": return "This session has multiple windows; the unified terminal opens single-window sessions.";
    case "blocked_foreign_server": return "This session lives on a server the unified terminal does not observe.";
    case "slots_exhausted": return "No unified terminal slots remain for this run.";
    case "unavailable": return "The unified terminal is unavailable for this session.";
  }
}
/**
 * Operator wording for an adoption refusal. The server sends a code from a
 * closed set and never free text; an unrecognised code falls back to a neutral
 * message rather than being shown raw.
 */
export function adoptFailureMessage(code: string, status: number): string {
  switch (code.trim()) {
    case "blocked_alt_screen": return "A full-screen app is active in this session. Retry after it exits.";
    case "blocked_multi_pane": return "This session has multiple panes; the unified terminal opens single-pane sessions.";
    case "blocked_multi_window": return "This session has multiple windows; the unified terminal opens single-window sessions.";
    case "session_gone": return "This session just ended.";
    case "slots_exhausted": return "No unified terminal slots remain for this run.";
    case "adoption_in_progress": return "This session is already being opened. Try again in a moment.";
    case "adoption_contended": return "The session would not settle for capture. Try again.";
    case "not_permitted": return "This session cannot be opened through the unified terminal.";
    case "unified_unavailable": return "The unified terminal is unavailable.";
    default: return status === 503 ? "The realm is unreachable." : "The session could not be opened.";
  }
}
export type SessionPreview = Readonly<{ rows: readonly string[]; ansiRows?: readonly string[]; capturedAt: number; width: number; height: number; truncated: boolean }>;
/**
 * Strict parse of one on-demand pane preview, same discipline as
 * parseInventory: a wrong shape throws and renders as an error state, never as
 * partially-trusted content. Plain rows are always available. Optional SGR
 * styling becomes text spans with bounded colors, never executable markup.
 */
export function parsePreview(value: unknown): SessionPreview {
  const v = object(value, "preview");
  const rows = array(v.rows, "preview.rows").map((row, i) => string(row, `preview.rows[${i}]`, true));
  if (rows.length > 41 || rows.join("\n").length > 8192) throw new Error("Preview exceeds its bounds");
  const ansiRows = v.ansi_rows == null ? undefined : array(v.ansi_rows, "preview.ansi_rows").map((row, i) => string(row, `preview.ansi_rows[${i}]`, true));
  if (ansiRows && (ansiRows.length !== rows.length || ansiRows.join("\n").length > 8192)) throw new Error("Preview styling exceeds its bounds");
  return {
    rows,
    ...(ansiRows ? { ansiRows } : {}),
    capturedAt: integer(v.captured_at, "preview.captured_at", 1),
    width: integer(v.width, "preview.width", 1),
    height: integer(v.height, "preview.height", 1),
    truncated: boolean(v.truncated, "preview.truncated"),
  };
}
// The preview is an authenticated GET carrying identity only, exactly like the
// adoption POST body: realm, server label, session ID — never depth or geometry.
export function previewRequestPath(session: Pick<DashboardSession, "realm" | "server" | "sessionId">): string {
  return `/api/session-previews?${new URLSearchParams({ realm: session.realm, server: session.server, session_id: session.sessionId }).toString()}`;
}
/**
 * Operator wording for a preview failure. The server sends a code from a
 * closed set and never free text; an unrecognised code falls back to a neutral
 * message rather than being shown raw.
 */
export function previewFailureMessage(code: string, status: number): string {
  switch (code.trim()) {
    case "session_gone": return "This session just ended.";
    case "server_unavailable": return "The tmux server is unavailable.";
    default:
      if (status === 429) return "Previews are refreshing too fast. Try again in a moment.";
      return status === 503 ? "The realm is unreachable." : "The preview could not be loaded.";
  }
}
export function formatPreviewMeta(preview: Pick<SessionPreview, "capturedAt" | "width" | "height" | "truncated">): string {
  const date = new Date(preview.capturedAt);
  const captured = Number.isNaN(date.valueOf()) ? String(preview.capturedAt) : date.toLocaleTimeString();
  return `${preview.width}×${preview.height} · captured ${captured}${preview.truncated ? " · truncated" : ""}`;
}
export function mutationStatus(status: number): string | undefined {return ({ 409: "Alias changed elsewhere; refreshed current state.", 410: "Session handle expired; refreshed current state.", 412: "Alias revision is stale; refreshed current state.", 503: "Alias service is unavailable; refreshed current state." } as Record<number, string>)[status]; }
export class RefreshGate { private editing = 0; private mutating = false; beginEdit(): void { this.editing++; } endEdit(): void { this.editing = Math.max(0, this.editing - 1); } setMutating(value: boolean): void { this.mutating = value; } permitsBackgroundRefresh(): boolean { return this.editing === 0 && !this.mutating; } }
export const COLLAPSE_STORAGE_KEY = "persea_terminal_dashboard_collapse_v1";
export type CollapseStorage = Pick<Storage, "getItem" | "setItem">;
export function collapseGroupKey(realm: string, server: string): string { return JSON.stringify([realm, server]); }
// Collapse state is presentation only, so a malformed or unreadable record falls
// back to the default (everything expanded) instead of failing the page.
export function readCollapsedGroups(storage: CollapseStorage): Set<string> {
  try {
    const raw = storage.getItem(COLLAPSE_STORAGE_KEY);
    if (raw === null) return new Set();
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed) || !parsed.every((item): item is string => typeof item === "string")) return new Set();
    return new Set(parsed);
  } catch { return new Set(); }
}
export function writeCollapsedGroups(storage: CollapseStorage, groups: ReadonlySet<string>): void {
  try { storage.setItem(COLLAPSE_STORAGE_KEY, JSON.stringify([...groups].sort())); } catch { /* persistence is best effort */ }
}
export function assignText(node: { textContent: string | null }, text: string): void { node.textContent = text; }
function element<K extends keyof HTMLElementTagNameMap>(tag: K, className?: string, text?: string): HTMLElementTagNameMap[K] { const node = document.createElement(tag); if (className) node.className = className; if (text !== undefined) assignText(node, text); return node; }
function updateSessionMetadata(target: HTMLElement, session: DashboardSession): void {
  const text = sessionMetadata(session);
  if (target.textContent === text) return;
  target.replaceChildren(...text.split(" · ").flatMap((part, index) => [
    ...(index ? [document.createTextNode(" · ")] : []), element("span", "session-metadata-part", part),
  ]));
}
function dashboardIcon(kind: "info" | "edit" | "preview"): SVGSVGElement {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("aria-hidden", "true"); svg.classList.add("dashboard-control-icon");
  const path = document.createElementNS(svg.namespaceURI, "path");
  path.setAttribute("d", {
    info: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20M12 11v6M12 7v.1",
    edit: "m15 4 5 5M4 20l5-1L21 7a2 2 0 0 0-5-5L4 14v6Z",
    preview: "M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12Zm13 0a3 3 0 1 0-6 0 3 3 0 0 0 6 0",
  }[kind]); svg.append(path); return svg;
}
function formatActivity(seconds: number): string { const date = new Date(seconds * 1000); return Number.isNaN(date.valueOf()) ? String(seconds) : date.toLocaleString(); }
function sessionStateKey(session: DashboardSession): string { return session.draftScope; }

export type DraftScopeResolution =
  | Readonly<{ kind: "found"; session: DashboardSession }>
  | Readonly<{ kind: "missing" }>
  | Readonly<{ kind: "ambiguous" }>;

export function resolveDraftScope(inventory: DashboardInventory, draftScope: string): DraftScopeResolution {
  const matches = inventory.realms.flatMap((realm) =>
    realm.servers.flatMap((server) => server.sessions),
  ).filter((session) => session.draftScope === draftScope);
  if (matches.length === 0) return Object.freeze({ kind: "missing" });
  if (matches.length !== 1) return Object.freeze({ kind: "ambiguous" });
  return Object.freeze({ kind: "found", session: matches[0] });
}

// --- session memory landing copy. Reviewed operator-facing wording lives here as pure
// functions so the states can be asserted without a DOM. Every message names
// what is true; none of them promises a session the inventory did not confirm.
export function resumeCardLabel(session: Pick<DashboardSession, "name" | "server">): string { return `Resume ${session.name} · ${session.server}`; }
export function resumeCardDetail(session: Pick<DashboardSession, "realm" | "server">, alias?: string): string { return `${session.realm} · ${session.server}${alias ? ` · ${alias}` : ""}`; }
export function landingEndedMessage(record: Pick<LastSessionRecord, "name" | "server">): string { return `Your last session ${record.name}${record.server === "default" ? "" : ` · ${record.server}`} has ended.`; }
export function landingAmbiguousMessage(record: Pick<LastSessionRecord, "name">): string { return `Your last session ${record.name} matches more than one live session; the list below is filtered to that name.`; }
export function landingBlockedMessage(record: Pick<LastSessionRecord, "name" | "server">, blocked: Exclude<UnifiedSessionProjection, { state: "open" | "adoptable" }>["state"]): string {
  return `Your last session ${record.name} · ${record.server}: ${unifiedBlockedMessage(blocked)}`;
}
export function defaultCardLabel(preference: Pick<DefaultSessionPreference, "name" | "server">): string { return `Open default session ${preference.name} · ${preference.server}`; }
export function defaultUnavailableMessage(state: DefaultSessionState): string {
  if (state.kind === "missing") return `The default session ${state.preference.name} · ${state.preference.server} is not running.`;
  if (state.kind === "ambiguous") return `More than one live session is named ${state.preference.name} on ${state.preference.server}.`;
  if (state.kind === "blocked") return `The default session ${state.preference.name} · ${state.preference.server}: ${unifiedBlockedMessage(state.blocked)}`;
  return "";
}

type LandingCard = Readonly<{ el: HTMLElement; action: HTMLElement }>;
type SessionView = Readonly<{ el: HTMLElement; session: Pick<DashboardSession, "name" | "aliases"> }>;
type ServerView = Readonly<{ el: HTMLElement; body: HTMLElement; toggle: HTMLButtonElement; key: string; hasError: boolean; sessions: readonly SessionView[] }>;
type RealmView = Readonly<{ el: HTMLElement; hasError: boolean; servers: readonly ServerView[] }>;

export class Dashboard {
  private readonly cleanup: Array<() => void> = [];
  private destroyed = false;
  private workspaceSerial = 0;
  private lastUpdated?: number;
  private refreshPending = false;
  private refreshError = "";
  private readonly resultStatus = element("p", "dashboard-results");
  private readonly emptyResults = element("div", "dashboard-empty-results");
  private readonly sessionsPanel = element("section", "dashboard-sessions");
  private readonly settingsPanel = element("section", "dashboard-settings");
  private readonly aliasArchive = element("details", "dashboard-settings-panel detached-aliases");
  private discovery: SessionDiscovery = { pinned: [], recent: [] };
  private readonly favorites: DashboardFavorites;
  private readonly favoritesStatus = element("p", "dashboard-favorites-status");
  private listMode: "all" | "pinned" | "recent" = "all";
  private readonly listModes = element("div", "dashboard-list-modes");
  private readonly newSession = element("button", "dashboard-create-shortcut", "+ New session");
  private readonly refreshButton = element("button", "dashboard-refresh", "↻ Refresh");
  private readonly workspaceRefresh = element("button", "dashboard-workspace-refresh", "↻ Refresh");
  private readonly creationPanel = element("section", "dashboard-create-panel");
  private readonly createTarget = element("select");
  private readonly createTargets = new Map<string, DashboardServer>();
  private creationBusy = false;
  private createSelectionExplicit = false;
  private readDeviceHints(): void {
    try { this.lastSession = readLastSession(window.localStorage); this.discovery = readSessionDiscovery(window.localStorage); } catch { /* retain in-page hints if storage is unavailable */ }
  }
  private isFavorite(scope: string): boolean { return this.favorites.snapshot().favorites.includes(scope); }
  private recentAt(scope: string): number {
    const candidate = this.discovery.recent.find(item => item.scope === scope)?.at ?? (this.lastSession?.draftScope === scope ? this.lastSession.at : 0);
    const now = Date.now(); return candidate >= now - 30 * 86_400_000 && candidate <= now + 60_000 ? candidate : 0;
  }
  private readonly realmNodes = new Map<string, HTMLElement>();
  private readonly serverNodes = new Map<string, ServerView>();
  private readonly sessionNodes = new Map<string, { el: HTMLElement; update(session: DashboardSession): void; dispose(): void }>();
  private readonly workspaceNodes = new Map<string, { record: WorkspaceRecord; el: HTMLElement }>();
  private readonly workspaceList = element("ul", "workspace-panel__list");
  private readonly workspaceStatus = element("output", "workspace-panel__status");
  private workspaceCreate?: HTMLElement;
  private readonly onPageShow = (): void => { this.syncScrollbackPreference(); void this.refresh("return"); };
  private readonly onVisibility = (): void => { if (document.visibilityState === "visible") { this.syncScrollbackPreference(); void this.refresh("return"); } };
  private readonly clipboard: ClipboardPanel;
  private readonly gate = new RefreshGate(); private readonly status = element("p", "dashboard-status"); private readonly content = element("div", "dashboard-content"); private requestSerial = 0; private periodic?: number; private navigationReloadPending = false;
  private readonly workspacePanel = element("section", "workspace-panel");
  private readonly workspaceAPI: WorkspaceAPI;
  private workspaceRecords: readonly WorkspaceRecord[] = [];
  private workspaceInventory?: DashboardInventory;
  private readonly searchInput = element("input", "dashboard-search-input"); private filterQuery = ""; private collapsedGroups = new Set<string>(); private readonly expandedSessions = new Set<string>(); private view: readonly RealmView[] = [];
  // Resume appears above the inventory with an explicit Open action. It is
  // presentation only: everything it needs comes from the
  // device memory read at mount, the inventory the dashboard already fetches,
  // and one preferences GET. Nothing here attaches, creates, adopts, or mints
  // before a trusted tap.
  private readonly landing = element("section", "dashboard-landing");
  private lastSession?: LastSessionRecord;
  private defaultPreference?: DefaultSessionPreference;
  private inventory?: DashboardInventory;
  private landingFocusPending = false;
  private landingFilterApplied = false;
  private landingActionPending = false;
  private landingView?: LandingCard & { key: string; update(session: DashboardSession, title: string, detail: string): void };
  private previewDialog?: HTMLDialogElement;
  private previewQueue: Promise<void> = Promise.resolve();
  private scrollbackRows = readScrollbackRows();
  private scrollbackWasSaved = true;
  private nextSessionDOMId = 0;
  private scrollbackSelect?: HTMLSelectElement;
  private syncScrollbackPreference(refreshTerminals = false): void {
    if (!this.scrollbackWasSaved) return;
    const rows = readScrollbackRows();
    if (rows === this.scrollbackRows && !refreshTerminals) return;
    this.scrollbackRows = rows;
    if (this.scrollbackSelect) this.scrollbackSelect.value = String(rows);
    if (this.inventory) preserveFocus(() => this.render(this.inventory!));
  }
  private readonly handleFragmentTransition = (): void => {
    if (this.navigationReloadPending) return;
    const fragment = new URLSearchParams(window.location.hash.slice(1));
    if (!fragment.has("handle")) return;
    this.navigationReloadPending = true;
    window.location.reload();
  };
  // One preferences service per page: the default-session read and the
  // Appearance card share its single load (shared-landing-inventory).
  private readonly preferences: OperatorPreferencesService;
  constructor(
    private readonly root: HTMLElement,
    private readonly fetcher: FetchLike = (input, init) => window.fetch(input, init),
    preferences: OperatorPreferencesService = new OperatorPreferencesService({ fetch: (input, init) => fetcher(input, init) }),
  ) {
    this.workspaceAPI = new WorkspaceAPI(fetcher);
    this.favorites = new DashboardFavorites((input, init) => fetcher(input, init));
    this.preferences = preferences;
    this.clipboard = new ClipboardPanel({
      text: new SnippetService({ fetch: (input, init) => fetcher(input, init), origin: deviceOrigin(window) }),
      images: new ClipboardImages({ fetch: (input, init) => fetcher(input, init), origin: deviceOrigin(window) }),
      preferences: new ClipboardPreferencesService({ fetch: (input, init) => fetcher(input, init) }),
    });
  }
  mount(): void {
    document.body.classList.add("dashboard-mode"); const header = element("header", "dashboard-header"); const titles = element("div"); titles.append(element("p", "dashboard-eyebrow", "PERSEA TERMINAL"), element("h1", undefined, "Sessions"));
    this.refreshButton.type = "button"; this.refreshButton.title = "Refresh the session list"; this.refreshButton.setAttribute("aria-label", "Refresh session list"); this.refreshButton.addEventListener("click", () => void this.refresh("manual"));
    this.workspaceRefresh.type = "button"; this.workspaceRefresh.setAttribute("aria-label", "Refresh workspaces"); this.workspaceRefresh.addEventListener("click", () => void this.refresh("manual"));
    const clipboard = element("button", "dashboard-clipboard", "Clipboard"); clipboard.type = "button"; clipboard.setAttribute("aria-label", "Open shared clipboard"); clipboard.addEventListener("click", () => this.clipboard.open(clipboard));
    const headerActions = element("div", "dashboard-header-actions"); headerActions.append(this.newSession, clipboard); header.append(titles, headerActions);
    try { this.collapsedGroups = readCollapsedGroups(window.localStorage); } catch { /* collapse is only a device hint */ }
    this.readDeviceHints();
    const search = element("div", "dashboard-search");
    this.searchInput.type = "search"; this.searchInput.name = "session-filter"; this.searchInput.placeholder = "Filter sessions"; this.searchInput.autocomplete = "off";
    this.searchInput.setAttribute("aria-label", "Filter sessions by name or alias");
    this.searchInput.addEventListener("input", () => { this.filterQuery = this.searchInput.value; this.applyPresentation(); });
    const clear = element("button", "dashboard-search-clear", "×"); clear.type = "button"; clear.setAttribute("aria-label", "Clear session filter");
    clear.addEventListener("click", () => { this.searchInput.value = ""; this.filterQuery = ""; this.applyPresentation(); this.searchInput.focus(); });
    search.append(this.searchInput, clear);
    this.status.setAttribute("role", "status"); this.status.setAttribute("aria-live", "polite"); this.workspacePanel.setAttribute("aria-label", "Workspaces"); this.root.className = "dashboard"; this.root.setAttribute("aria-label", "Persea Terminal dashboard");
    // Resume is hidden until the current inventory confirms its exact target.
    this.landing.setAttribute("aria-label", "Resume a session");
    this.landing.hidden = true;
    this.resultStatus.setAttribute("role", "status");
    this.emptyResults.hidden = true;
    const reset = element("button", undefined, "Clear filter"); reset.type = "button";
    reset.addEventListener("click", () => { this.listMode = "all"; clear.click(); });
    this.emptyResults.append(element("p", undefined, "No sessions match this filter."), reset);
    this.listModes.setAttribute("aria-label", "Session lists");
    for (const mode of ["all", "pinned", "recent"] as const) {
      const button = element("button", undefined, mode === "all" ? "All" : mode === "pinned" ? "Favorites" : "Recent"); button.type = "button"; button.dataset.mode = mode;
      button.addEventListener("click", () => { this.listMode = mode; if (this.inventory) preserveFocus(() => this.render(this.inventory!)); });
      this.listModes.append(button);
    }
    this.newSession.type = "button"; this.newSession.hidden = true; this.newSession.setAttribute("aria-label", "New session"); this.newSession.title = "Create a session";
    this.creationPanel.id = "dashboard-create"; this.creationPanel.hidden = true;
    this.newSession.setAttribute("aria-controls", this.creationPanel.id); this.newSession.setAttribute("aria-expanded", "false");
    const createHeading = element("div", "dashboard-panel-heading");
    const closeCreate = element("button", "dashboard-icon-button", "×"); closeCreate.type = "button"; closeCreate.setAttribute("aria-label", "Close new session form");
    closeCreate.addEventListener("click", () => { this.creationPanel.hidden = true; this.newSession.setAttribute("aria-expanded", "false"); this.newSession.focus(); });
    createHeading.append(element("h2", undefined, "New session"), closeCreate);
    this.creationPanel.append(createHeading, this.renderCreate());
    this.newSession.addEventListener("click", () => {
      this.creationPanel.hidden = !this.creationPanel.hidden;
      this.newSession.setAttribute("aria-expanded", String(!this.creationPanel.hidden));
      if (!this.creationPanel.hidden) (this.createTarget.value ? this.creationPanel.querySelector<HTMLInputElement>('input[name="name"]') : this.createTarget)?.focus({ preventScroll: true });
    });
    const scrollback = createScrollbackControl(this.scrollbackRows, rows => {
      this.scrollbackRows = rows;
      const saved = saveScrollbackRows(rows);
      this.scrollbackWasSaved = saved;
      this.status.textContent = saved ? "Default saved on this device. Applies when opening terminals without their own setting; open terminals keep their current limit." : "Default selected for this page. This browser could not save the preference.";
      if (this.inventory) preserveFocus(() => this.render(this.inventory!));
    }, { deviceDefault: true });
    scrollback.label.title = "Default for terminals opened on this device. A terminal's own setting takes priority. Already open terminals keep their current limit.";
    this.scrollbackSelect = scrollback.select;
    const onStorage = (event: StorageEvent): void => { if (event.key === SCROLLBACK_STORAGE_KEY || event.key === TERMINAL_SCROLLBACK_STORAGE_KEY || event.key === null) { this.scrollbackWasSaved = true; this.syncScrollbackPreference(true); } };
    window.addEventListener("storage", onStorage); this.cleanup.push(() => window.removeEventListener("storage", onStorage));
    const toolbar = element("div", "dashboard-toolbar"); toolbar.append(search, this.listModes, scrollback.label, this.refreshButton);
    this.favoritesStatus.setAttribute("role", "status");
    this.sessionsPanel.append(toolbar, this.resultStatus, this.favoritesStatus, this.emptyResults, this.content);
    this.settingsPanel.append(this.appearanceCard(), this.keyboardCard(), this.aliasArchive);
    const navigation = element("nav", "dashboard-navigation"); navigation.setAttribute("aria-label", "Dashboard sections");
    const panels = [{ label: "Sessions", panel: this.sessionsPanel }, { label: "Workspaces", panel: this.workspacePanel }, { label: "Settings", panel: this.settingsPanel }];
    for (const { label, panel } of panels) {
      panel.id = `dashboard-${label.toLowerCase()}`;
      const button = element("button", undefined, label); button.type = "button";
      button.setAttribute("aria-controls", panel.id); button.setAttribute("aria-current", label === "Sessions" ? "page" : "false");
      button.addEventListener("click", () => {
        titles.querySelector("h1")!.textContent = label;
        for (const entry of panels) entry.panel.hidden = entry.panel !== panel;
        this.landing.hidden = panel !== this.sessionsPanel || this.landing.childElementCount === 0;
        this.creationPanel.hidden = true; this.newSession.setAttribute("aria-expanded", "false");
        this.newSession.hidden = panel !== this.sessionsPanel || this.createTargets.size === 0;
        if (panel === this.workspacePanel) void this.refreshWorkspaces();
        this.landingFocusPending = false;
        for (const sibling of Array.from(navigation.children)) sibling.setAttribute("aria-current", String(sibling === button ? "page" : "false"));
      });
      navigation.append(button); panel.hidden = label !== "Sessions";
    }
    this.root.replaceChildren(header, navigation, this.creationPanel, this.landing, this.status, this.sessionsPanel, this.workspacePanel, this.settingsPanel);
    this.cleanup.push(this.favorites.subscribe(snapshot => {
      this.favoritesStatus.textContent = snapshot.message;
      if (this.inventory) preserveFocus(() => this.render(this.inventory!));
    }));
    this.cleanup.push(installTapFeedback(document));
    // Device memory and the operator's default are read before any render;
    // neither is authority, and neither can act on its own.
    this.landingFocusPending = new URLSearchParams(window.location.search).get("resume") === "1";
    void this.loadDefaultPreference();
    window.addEventListener("pageshow", this.onPageShow); window.addEventListener("hashchange", this.handleFragmentTransition); document.addEventListener("visibilitychange", this.onVisibility); this.periodic = window.setInterval(() => { if (document.visibilityState === "visible") void this.refresh("background"); }, 60_000); void this.refresh("initial");
  }
  // the appearance preferences (theme, terminal font, composer
  // text) are operator-wide, so they get a dashboard card. It is collapsed by
  // default — the dashboard's job is choosing a session — and it renders from
  // the page's one preferences read. Saves are ordinary preference updates:
  // every open terminal follows through the storage event, and this card
  // follows theirs the same way.
  private keyboardCard(): HTMLElement {
    const card = element("details", "dashboard-settings-panel dashboard-appearance");
    const heading = element("summary", "dashboard-appearance__summary", "Keyboard");
    const body = element("div", "dashboard-settings-body");
    const note = element("p", "dashboard-appearance__note", "Customize the key bar, favorites and prefixes. Shared defaults apply to every terminal, with optional customization for this device.");
    const open = element("button", "dashboard-refresh", "Customize keyboard"); open.type = "button";
    const editor = new KeyboardSettings();
    this.cleanup.push(() => editor.dispose());
    open.addEventListener("click", () => editor.open(open));
    body.append(note, open); card.append(heading, body); return card;
  }
  private appearanceCard(): HTMLElement {
    // the one settings surface for every terminal — theme, terminal
    // font and composer text moved here from the terminal's View popover.
    const card = element("details", "dashboard-settings-panel dashboard-appearance");
    card.open = true;
    const summary = element("summary", "dashboard-appearance__summary", "Appearance");
    const body = element("div", "dashboard-settings-body");
    const grid = element("div", "dashboard-appearance__grid");
    const status = element("p", "dashboard-appearance__status");
    status.setAttribute("role", "status"); status.setAttribute("aria-live", "polite");
    const note = element("p", "dashboard-appearance__note", "Applies to every open terminal at once, no reload needed.");
    const field = (caption: string, select: HTMLSelectElement, label: string): HTMLLabelElement => {
      const wrap = element("label", "dashboard-appearance__field");
      select.setAttribute("aria-label", label);
      wrap.append(element("span", "dashboard-appearance__caption", caption), select);
      return wrap;
    };
    const option = (select: HTMLSelectElement, value: string, text: string): void => {
      const node = element("option", undefined, text); node.value = value; select.append(node);
    };
    const theme = element("select"); for (const id of UNIFIED_THEME_IDS) option(theme, id, unifiedTheme(id).label);
    const font = element("select"); option(font, "auto", "Auto (fit to view)"); for (const size of APPEARANCE_FONT_SIZES) option(font, String(size), `${size} px`);
    // no phone floor — the viewport meta suppresses the iOS focus
    // zoom, so every size 9–24 is the composer's real size on every device.
    const composer = element("select"); for (const size of APPEARANCE_FONT_SIZES) option(composer, String(size), `${size} px`);
    grid.append(field("Theme", theme, "Terminal theme"), field("Terminal font", font, "Terminal font size"), field("Composer text", composer, "Composer text size"));
    body.append(grid, note, status); card.append(summary, body);
    const service = this.preferences;
    let clearStatus: ReturnType<typeof setTimeout> | undefined;
    const say = (text: string, transient: boolean): void => {
      if (clearStatus !== undefined) { clearTimeout(clearStatus); clearStatus = undefined; }
      assignText(status, text);
      if (transient && text !== "") clearStatus = setTimeout(() => { assignText(status, ""); clearStatus = undefined; }, 2_000);
    };
    // While the operator's own saves are in flight the selects show what was
    // chosen, not each intermediate publication (writes are serialised, so a
    // quick second pick would otherwise flick back to the first for a moment);
    // once the newest save settles, the record is the sole authority again.
    let saveSerial = 0;
    let inFlight = 0;
    const render = (snapshot: OperatorPreferenceSnapshot): void => {
      const editable = snapshot.status === "ready" || snapshot.status === "hint";
      theme.disabled = font.disabled = composer.disabled = !editable;
      if (inFlight === 0) {
        theme.value = snapshot.preferences.theme;
        font.value = snapshot.preferences.fontSize === null ? "auto" : String(snapshot.preferences.fontSize);
        composer.value = String(snapshot.preferences.composerFontSize);
      }
      if (snapshot.status === "loading") say("Loading…", false);
      else if (inFlight === 0 && (snapshot.status === "unavailable" || snapshot.status === "conflict")) say(snapshot.message, false);
      else if (snapshot.status === "ready" && inFlight === 0 && status.textContent === "Loading…") say("", false);
      const palette = unifiedTheme(snapshot.preferences.theme);
      this.root.style.setProperty("--dashboard-bg", palette.pageBackground);
      this.root.style.setProperty("--dashboard-card", palette.background);
      this.root.style.setProperty("--dashboard-chrome", palette.chromeBackground);
      this.root.style.setProperty("--dashboard-fg", palette.foreground);
      this.root.style.setProperty("--dashboard-accent", palette.accent);
      this.root.style.colorScheme = snapshot.preferences.theme === "rose-pine-dawn" ? "light" : "dark";
      document.body.style.background = palette.pageBackground;
    };
    const subscription = service.subscribe(render);
    this.cleanup.push(() => { subscription.dispose(); if (clearStatus !== undefined) clearTimeout(clearStatus); });
    // The status line belongs to the newest save: an older operation that
    // settles later must not overwrite it. The service publishes its record
    // synchronously and only then resolves, so a rejection here is a subscriber
    // fault on a write that is already settled — the snapshot is the authority
    // for what to say, and the rejection must not leak out of the void call.
    const save = async (patch: OperatorPreferencePatch): Promise<void> => {
      const serial = ++saveSerial;
      inFlight += 1;
      say("Saving…", false);
      let outcome: OperatorPreferenceOutcome;
      try {
        outcome = await service.update(patch);
      } catch {
        const status = service.snapshot().status;
        outcome = status === "conflict" ? "conflict" : status === "ready" ? "saved" : "unavailable";
      } finally {
        inFlight -= 1;
      }
      if (serial !== saveSerial) return;
      render(service.snapshot());
      if (outcome === "saved") say("Saved — open terminals follow.", true);
      else if (outcome === "conflict") say("Another tab saved first; showing the newer values.", true);
      else say("Could not save: preferences are unavailable.", false);
    };
    theme.addEventListener("change", () => { const id = theme.value; if (isUnifiedThemeID(id)) void save({ theme: id }); });
    font.addEventListener("change", () => void save({ fontSize: font.value === "auto" ? null : Number(font.value) }));
    composer.addEventListener("change", () => void save({ composerFontSize: Number(composer.value) }));
    // The dashboard never re-reads preferences on focus or visibility
    // (shared-landing-inventory); another tab's save reaches the card through the `storage`
    // signal, which triggers one authoritative server read (14.3b) — a
    // storage event never fires in the document that wrote it.
    void service.load().then(() => { if (!this.destroyed) this.cleanup.push(service.watchExternalChanges({ refetchOnForeground: false })); });
    return card;
  }
  // Exactly one preferences read per page, at mount. The default session is an
  // operator preference, not live state: it is never re-fetched on refresh,
  // focus, visibility, or orientation, and a failure simply means no default
  // card.
  private async loadDefaultPreference(): Promise<void> {
    await this.preferences.load();
    if (this.destroyed) return;
    const snapshot = this.preferences.snapshot();
    // An unavailable record offers no default; the list is unaffected.
    this.defaultPreference = snapshot.status === "ready" ? snapshot.preferences.defaultSession ?? undefined : undefined;
    this.renderLanding();
  }
  destroy(): void { this.destroyed = true; this.requestSerial += 1; this.workspaceSerial += 1; window.removeEventListener("pageshow", this.onPageShow); document.removeEventListener("visibilitychange", this.onVisibility); window.removeEventListener("hashchange", this.handleFragmentTransition); if (this.periodic !== undefined) window.clearInterval(this.periodic); for (const dispose of this.cleanup.splice(0)) dispose(); this.clipboard.dispose(); this.favorites.dispose(); }
  async refresh(reason: "initial" | "manual" | "return" | "background" | "mutation"): Promise<void> {
    if (this.destroyed) return;
    if ((reason === "background" || reason === "return") && (this.refreshPending || !this.gate.permitsBackgroundRefresh())) return;
    if (reason === "return") { this.readDeviceHints(); preserveFocus(() => this.renderLanding()); }
    void this.favorites.load(reason === "manual").then(() => {
      try { if (!this.destroyed) void this.favorites.migrateDevicePins(window.localStorage); } catch { /* device migration is optional */ }
    });
    if ((reason === "background" || reason === "return") && this.lastUpdated && Date.now() - this.lastUpdated < 55_000) return;
    const serial = ++this.requestSerial;
    this.refreshPending = true;
    for (const button of [this.refreshButton, this.workspaceRefresh]) { button.disabled = true; button.setAttribute("aria-busy", "true"); }
    if (!this.inventory) this.status.textContent = "Loading sessions…";
    try {
      const response = await this.fetcher("/api/inventory", { cache: "no-store", credentials: "same-origin" });
      if (!response.ok) throw new Error(`Session refresh failed (${response.status}).`);
      const inventory = parseInventory(await response.json());
      if (serial !== this.requestSerial || this.destroyed) return;
      this.workspaceInventory = inventory; this.lastUpdated = Date.now(); this.refreshError = "";
      preserveFocus(() => this.render(inventory));
      if (reason === "initial" || reason === "manual" || reason === "mutation" || !this.workspacePanel.hidden) void this.refreshWorkspaces();
      this.status.textContent = "";
    } catch (error) {
      if (serial !== this.requestSerial || this.destroyed) return;
      this.refreshError = error instanceof Error ? error.message : "Session refresh failed.";
      const age = this.lastUpdated ? ` Showing saved results from ${new Date(this.lastUpdated).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}.` : "";
      this.status.textContent = `${this.refreshError}${age} Use Refresh to retry.`;
    } finally {
      if (serial === this.requestSerial) {
        this.refreshPending = false;
        for (const button of [this.refreshButton, this.workspaceRefresh]) { button.disabled = false; button.removeAttribute("aria-busy"); }
      }
    }
  }

  private async refreshWorkspaces(): Promise<void> {
    const serial = ++this.workspaceSerial;
    try {
      const list = await this.workspaceAPI.list();
      if (serial !== this.workspaceSerial || this.destroyed) return;
      this.workspaceRecords = list.items;
      if (this.workspaceStatus.dataset.refreshFailure === "true") { this.workspaceStatus.replaceChildren(); delete this.workspaceStatus.dataset.refreshFailure; }
      preserveFocus(() => this.renderWorkspaces());
    } catch (error) {
      if (serial !== this.workspaceSerial || this.destroyed) return;
      const notice = workspaceAPIMessage(error);
      const status = element("div", "workspace-panel__failure");
      status.setAttribute("role", "alert");
      status.append(element("strong", undefined, notice.headline), element("span", undefined, notice.detail), element("code", undefined, notice.code));
      const retry = element("button", undefined, "Retry workspaces"); retry.type = "button"; retry.addEventListener("click", () => void this.refreshWorkspaces());
      this.workspaceStatus.replaceChildren(status, retry);
      this.workspaceStatus.dataset.refreshFailure = "true";
      if (!this.workspaceStatus.isConnected) this.workspacePanel.append(this.workspaceStatus);
    }
  }

  private openableWorkspaceSelectors(): readonly SessionSelector[] {
    const inventory = this.workspaceInventory;
    if (!inventory) return [];
    return inventory.realms.flatMap((realm) => realm.servers.flatMap((server) => server.sessions
      .filter((session) => session.unified?.state === "open" || session.unified?.state === "adoptable")
      .map((session) => Object.freeze({ realm: session.realm, server: session.server, name: session.name }))));
  }

  private renderWorkspaces(): void {
    if (!this.workspaceCreate) {
      const heading = element("div", "workspace-panel__heading"); heading.append(element("h2", undefined, "Workspaces"), element("span", "workspace-panel__count"), this.workspaceRefresh);
      this.workspaceStatus.setAttribute("role", "status"); this.workspaceStatus.setAttribute("aria-live", "polite");
      this.workspaceCreate = this.renderWorkspaceCreateAffordance(this.workspaceStatus);
      this.workspacePanel.replaceChildren(heading, element("p", "workspace-panel__description", "Saved arrangements of terminals. On a phone, open each session individually."), this.workspaceList, this.workspaceCreate, this.workspaceStatus);
    }
    this.workspacePanel.querySelector(".workspace-panel__count")!.textContent = String(this.workspaceRecords.length);
    const rows = this.workspaceRecords.map(record => {
      const previous = this.workspaceNodes.get(record.workspaceId);
      if (previous && (previous.record.revision === record.revision || hasDraft(previous.el) || previous.el.contains(document.activeElement))) {
        if (previous.record.revision !== record.revision) {
          previous.el.dataset.changedElsewhere = "true";
          const link = previous.el.querySelector<HTMLAnchorElement>(".workspace-panel__open")!; link.textContent = record.name; link.href = workspaceURL(window.location.href, record.name);
          const notice = previous.el.querySelector<HTMLElement>(".workspace-panel__changed")!; notice.hidden = false;
        }
        return previous.el;
      }
      const el = this.renderWorkspaceRecord(record, this.workspaceStatus);
      this.workspaceNodes.set(record.workspaceId, { record, el }); return el;
    });
    reconcileChildren(this.workspaceList, rows.length ? rows : [element("li", "workspace-panel__empty", "No saved workspaces yet. Create one to keep a group of terminals together.")]);
    for (const key of this.workspaceNodes.keys()) if (!this.workspaceRecords.some(record => record.workspaceId === key)) this.workspaceNodes.delete(key);
    const select = this.workspaceCreate.querySelector<HTMLSelectElement>("select");
    if (select) {
      const selected = select.value;
      const values = this.openableWorkspaceSelectors().map(selector => ({ value: JSON.stringify(selector), label: `${selector.name} · ${selector.realm} · ${selector.server}` }));
      if (JSON.stringify(values.map(item => item.value)) !== JSON.stringify(Array.from(select.options).map(option => option.value))) {
        select.replaceChildren(...values.map(item => { const option = element("option", undefined, item.label); option.value = item.value; return option; }));
        if (values.some(item => item.value === selected)) select.value = selected;
      }
      const submit = this.workspaceCreate.querySelector<HTMLButtonElement>("button[type=submit]"); if (submit) submit.disabled = select.options.length === 0;
    }
  }

  // a phone-class device opens no workspace view (M5), so the
  // dashboard must not offer a create affordance the same device then refuses.
  // The M5 notice takes the form's place — the operator reads the refusal
  // before producing a record for it, rather than after. Existing records stay
  // listed exactly as before: each one keeps its row, and the /workspace
  // landing keeps its "Open as a single terminal" link per leaf. The posture
  // question is asked through the same function the /workspace document uses,
  // from the same stable screen witness, so the two never disagree.
  private renderWorkspaceCreateAffordance(status: HTMLElement): HTMLElement {
    if (workspacePosture(stablePostureEnvironment(window)) !== "phone") return this.renderWorkspaceCreate(status);
    const notice = element("p", "workspace-panel__notice", PHONE_STATE_NOTICE);
    notice.dataset.workspaceCreate = "unavailable";
    return notice;
  }

  private renderWorkspaceRecord(record: WorkspaceRecord, status: HTMLElement): HTMLElement {
    const item = element("li", "workspace-panel__item"); item.dataset.workspaceId = record.workspaceId;
    const open = element("a", "workspace-panel__open", record.name); open.href = workspaceURL(window.location.href, record.name); open.dataset.workspaceAction = "open";
    const manage = element("details", "workspace-panel__manage"); manage.append(element("summary", undefined, "Manage"));
    const revision = element("span", "workspace-panel__revision", `r${record.revision}`); revision.title = `Saved revision ${record.revision}`;
    const rename = element("form", "workspace-panel__rename");
    const input = element("input"); input.name = "name"; input.value = input.defaultValue = record.name; input.setAttribute("aria-label", `Rename ${record.name}`);
    const save = element("button", undefined, "Rename"); save.type = "submit";
    rename.append(input, save);
    rename.addEventListener("submit", (event) => { event.preventDefault(); void this.renameWorkspace(record, input, save, status); });
    const remove = element("button", "workspace-panel__delete", "Delete workspace…"); remove.type = "button";
    const confirmation = element("div", "workspace-panel__confirmation"); confirmation.hidden = true;
    const confirm = element("button", "workspace-panel__delete", `Delete ${record.name}`); confirm.type = "button";
    const cancel = element("button", undefined, "Cancel"); cancel.type = "button";
    confirmation.append(element("p", undefined, `Delete workspace “${record.name}”? This removes its saved layout. Its terminal sessions stay running.`), cancel, confirm);
    remove.addEventListener("click", () => { confirmation.hidden = false; remove.hidden = true; cancel.focus(); });
    cancel.addEventListener("click", () => { confirmation.hidden = true; remove.hidden = false; remove.focus(); });
    confirm.addEventListener("click", () => void this.deleteWorkspace(record, confirm, status));
    const changed = element("p", "workspace-panel__changed", "Updated elsewhere. Your unsaved name is kept."); changed.hidden = true;
    manage.append(revision, changed, rename, remove, confirmation); item.append(open, manage);
    item.addEventListener("focusout", () => queueMicrotask(() => { if (item.dataset.changedElsewhere === "true" && !item.contains(document.activeElement) && !hasDraft(item)) preserveFocus(() => this.renderWorkspaces()); }));
    return item;
  }

  private renderWorkspaceCreate(status: HTMLElement): HTMLElement {
    const form = element("form", "workspace-panel__create");
    const name = element("input"); name.name = "workspace_name"; name.placeholder = "Workspace name"; name.autocomplete = "off"; name.setAttribute("aria-label", "New workspace name");
    const select = element("select"); select.name = "workspace_session"; select.setAttribute("aria-label", "First workspace session");
    for (const selector of this.openableWorkspaceSelectors()) {
      const option = element("option"); option.value = JSON.stringify(selector); option.textContent = `${selector.name} · ${selector.realm} · ${selector.server}`; select.append(option);
    }
    const submit = element("button", undefined, "New workspace"); submit.type = "submit"; submit.disabled = select.options.length === 0;
    const selector = element("span", "workspace-panel__selector"); selector.append(select);
    form.append(name, selector, submit);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      let selector: SessionSelector;
      try { selector = JSON.parse(select.value) as SessionSelector; } catch { return; }
      void this.createWorkspace(name, selector, submit, status);
    });
    return form;
  }

  private async createWorkspace(input: HTMLInputElement, selector: SessionSelector, submit: HTMLButtonElement, status: HTMLElement): Promise<void> {
    if (submit.disabled) return;
    submit.disabled = true; this.gate.setMutating(true); status.textContent = "Saving workspace…";
    try {
      const created = await this.workspaceAPI.create(input.value, leaf(selector));
      this.workspaceSerial += 1; input.value = input.defaultValue = "";
      this.workspaceRecords = Object.freeze([...this.workspaceRecords, created].sort((a, b) => a.normalizedName.localeCompare(b.normalizedName)));
      this.renderWorkspaces();
    } catch (error) {
      status.textContent = workspaceAPIMessage(error, "created").detail;
    } finally { this.gate.setMutating(false); submit.disabled = false; }
  }

  private async renameWorkspace(record: WorkspaceRecord, input: HTMLInputElement, submit: HTMLButtonElement, status: HTMLElement): Promise<void> {
    if (submit.disabled || input.value === record.name) return;
    submit.disabled = true; this.gate.setMutating(true); status.textContent = "Renaming workspace…";
    try {
      const updated = await this.workspaceAPI.update(record, input.value, record.tree);
      this.workspaceSerial += 1;
      this.workspaceNodes.delete(record.workspaceId);
      this.workspaceRecords = Object.freeze(this.workspaceRecords.map((item) => item.workspaceId === updated.workspaceId ? updated : item));
      this.renderWorkspaces();
    } catch (error) {
      if (error instanceof WorkspaceAPIError && error.code === "conflict") this.renderWorkspaceConflict(status, input);
      else status.textContent = workspaceAPIMessage(error, "renamed").detail;
    } finally { this.gate.setMutating(false); submit.disabled = false; }
  }

  private renderWorkspaceConflict(status: HTMLElement, input: HTMLElement): void {
    const reload = element("button", undefined, "Reload saved"); reload.type = "button"; reload.addEventListener("click", () => {
      const item = input.closest<HTMLElement>("[data-workspace-id]");
      if (item?.dataset.workspaceId) this.workspaceNodes.delete(item.dataset.workspaceId);
      void this.refreshWorkspaces();
    });
    const keep = element("button", undefined, "Keep editing"); keep.type = "button"; keep.addEventListener("click", () => input.focus());
    status.replaceChildren(element("span", undefined, "This workspace changed elsewhere. Reload the saved record or keep editing this name."), reload, keep);
  }

  private async deleteWorkspace(record: WorkspaceRecord, button: HTMLButtonElement, status: HTMLElement): Promise<void> {
    if (button.disabled) return;
    button.disabled = true; this.gate.setMutating(true); status.textContent = "Deleting workspace…";
    try {
      await this.workspaceAPI.delete(record);
      this.workspaceSerial += 1;
      this.workspaceRecords = Object.freeze(this.workspaceRecords.filter((item) => item.workspaceId !== record.workspaceId));
      this.renderWorkspaces();
      this.workspaceStatus.textContent = `Workspace “${record.name}” deleted. Terminal sessions remain running.`;
      this.workspaceCreate?.querySelector<HTMLElement>("input, button")?.focus();
    } catch (error) {
      if (error instanceof WorkspaceAPIError && error.code === "conflict") this.renderWorkspaceConflict(status, button);
      else status.textContent = workspaceAPIMessage(error, "deleted").detail;
    } finally { this.gate.setMutating(false); button.disabled = false; }
  }

  private render(inventory: DashboardInventory): void {
    const view: RealmView[] = [];
    const liveScopes = new Set<string>(); const liveServers = new Set<string>();
    for (const realm of inventory.realms) {
      let section = this.realmNodes.get(realm.name);
      if (!section) { section = element("section", "realm-card"); section.dataset.realm = realm.name; section.append(element("div", "realm-title"), element("p", "failure")); this.realmNodes.set(realm.name, section); }
      const title = section.children[0] as HTMLElement;
      const identity = realm.uid === undefined ? `Realm ${realm.name}` : `Realm ${realm.name} · UID ${realm.uid}`;
      if (title.textContent !== realm.displayName) title.replaceChildren(element("h2", undefined, realm.displayName));
      title.title = identity;
      const failure = section.children[1] as HTMLElement; failure.textContent = realm.error ?? ""; failure.hidden = !realm.error;
      const servers = realm.servers.map(server => {
        liveServers.add(collapseGroupKey(server.realm, server.label));
        for (const session of server.sessions) liveScopes.add(session.draftScope);
        const view = this.renderServer(server);
        const heading = view.el.querySelector<HTMLElement>(".server-heading h2, .server-heading h3")!;
        const caption = realm.servers.length === 1 ? realm.displayName : server.label;
        const tag = realm.servers.length === 1 ? "H2" : "H3";
        if (heading.tagName !== tag) heading.replaceWith(element(tag.toLowerCase() as "h2" | "h3", "server-name", caption));
        else heading.textContent = caption;
        return view;
      });
      title.hidden = realm.servers.length === 1;
      reconcileChildren(section, [title, failure, ...servers.map(server => server.el)]);
      view.push({ el: section, hasError: !!realm.error, servers });
    }
    reconcileChildren(this.content, view.map(realm => realm.el));
    for (const key of this.realmNodes.keys()) if (!inventory.realms.some(realm => realm.name === key)) this.realmNodes.delete(key);
    for (const key of this.serverNodes.keys()) if (!liveServers.has(key)) this.serverNodes.delete(key);
    for (const key of this.sessionNodes.keys()) if (!liveScopes.has(key)) { this.sessionNodes.get(key)?.dispose(); this.sessionNodes.delete(key); this.expandedSessions.delete(key); }
    const archiveKey = JSON.stringify(inventory.detachedAliases);
    if (this.aliasArchive.dataset.version !== archiveKey) {
      this.aliasArchive.dataset.version = archiveKey;
      const body = element("div", "dashboard-settings-body");
      body.append(element("p", undefined, "Aliases from ended sessions. Reusing a session name does not reconnect an old alias."));
      for (const alias of inventory.detachedAliases) body.append(element("p", "alias-record", `${alias.displayAlias} · ${alias.state}`));
      this.aliasArchive.replaceChildren(element("summary", undefined, `Alias history · ${inventory.detachedAliases.length}`), body);
    }
    this.aliasArchive.hidden = inventory.detachedAliases.length === 0;
    this.updateCreateTargets(inventory);
    this.newSession.hidden = this.sessionsPanel.hidden || this.createTargets.size === 0;
    this.view = view; this.inventory = inventory; this.renderLanding(); this.applyPresentation();
  }

  // --- session memory landing -----------------------------------------------------------
  //
  // Renders from ONE inventory snapshot — the one the dashboard just parsed —
  // plus the memory read at mount and the single preferences read. It issues no
  // request of its own, so however many cards it draws the request ledger is
  // unchanged.
  private renderLanding(): void {
    const inventory = this.inventory;
    if (inventory === undefined) return;
    // A landing action in flight owns the card. An inventory refresh arrives on
    // a timer AND on every pageshow/visibilitychange — backgrounding the app
    // mid-adoption and returning to it would otherwise replace the disabled
    // button with a fresh enabled one, dropping its status line and letting a
    // second tap spend a second adoption.
    if (this.landingActionPending) return;
    const memory = landingMemoryState(this.lastSession, this.lastSession ? resolveDraftScope(inventory, this.lastSession.draftScope) : undefined);
    const nodes: HTMLElement[] = [];
    let primary: LandingCard | undefined;
    if (memory.kind === "resume") {
      primary = this.renderLandingAction(resumeCardLabel(memory.session), memory.session.name, resumeCardDetail(memory.session, memory.record.alias), "Resume", memory.session, memory.state, "resume");
      nodes.push(primary.el);
    } else if (memory.kind === "ended") {
      nodes.push(element("p", "landing-note", landingEndedMessage(memory.record)));
    } else if (memory.kind === "ambiguous") {
      nodes.push(element("p", "landing-note", landingAmbiguousMessage(memory.record)));
      this.applyAmbiguousFilter(memory.record.name);
    } else if (memory.kind === "blocked") {
      nodes.push(element("p", "landing-note", landingBlockedMessage(memory.record, memory.blocked)));
    }
    // The default is offered only when there is no resumable remembered
    // identity, and it always says "default" — a missing memory never silently
    // becomes a same-name resume.
    if (defaultSessionIsOffered(memory)) {
      const fallback = defaultSessionState(inventory, this.defaultPreference);
      if (fallback.kind === "open") {
        const card = this.renderLandingAction(defaultCardLabel(fallback.preference), fallback.session.name, resumeCardDetail(fallback.session), "Default session", fallback.session, fallback.state, "default");
        nodes.push(card.el);
        primary = primary ?? card;
      } else if (fallback.kind !== "none") {
        nodes.push(element("p", "landing-note", defaultUnavailableMessage(fallback)));
      }
    }
    // The connected card keeps its focus while its action uses current handles.
    // Carry focus explicitly if its availability changes the action variant.
    const hadFocus = this.landing.contains(document.activeElement);
    reconcileChildren(this.landing, nodes);
    this.landing.hidden = this.sessionsPanel.hidden || nodes.length === 0;
    // `?resume=1` (and the PWA start_url) only ORDER AND FOCUS this card. Focus
    // is not activation: a link or button never raises the software keyboard,
    // and nothing is attached until the operator taps.
    if (!this.landing.hidden && this.landingFocusPending && primary) { this.landingFocusPending = false; primary.action.focus(); }
    else if (!this.landing.hidden && hadFocus && primary && !this.landing.contains(document.activeElement)) primary.action.focus();
  }

  // An ambiguous remembered identity shows the list filtered to that name
  // instead of guessing a session. Applied once per page so it never fights the
  // operator's own typing.
  private applyAmbiguousFilter(name: string): void {
    if (this.landingFilterApplied || this.filterQuery !== "") return;
    this.landingFilterApplied = true;
    this.filterQuery = name;
    this.searchInput.value = name;
    // renderLanding also runs from the preferences read, which is not followed
    // by a presentation pass; applying it here keeps the note and the list in
    // agreement whichever path rendered the card.
    this.applyPresentation();
  }

  // Resume and ordinary rows share the same trusted Open action. The card's
  // accessible name also explains whether the choice is Resume or a default.
  // committed-identity. Every trusted action that navigates into the unified terminal
  // stages the identity of the session IT resolved from the authoritative
  // inventory, immediately before the navigation. The terminal page records
  // only what this staged candidate says; it never reads identity out of the
  // URL, which is client-supplied and proves nothing about what attached.
  //
  // Staging is presentation-adjacent bookkeeping, not authority: it stores no
  // handle, URL, token or capability, it is single-use, and it can only ever
  // narrow what the terminal page may record.
  private stageSessionIdentity(session: DashboardSession): string | undefined {
    const identity = pendingIdentityFromSession(session);
    if (identity === undefined) return undefined;
    const operationId = newOperationId();
    if (operationId === undefined) return undefined;
    return stagePendingSession(window.localStorage, operationId, identity) === "staged" ? operationId : undefined;
  }

  private renderLandingAction(accessibleName: string, title: string, detail: string, kicker: string, session: DashboardSession, state: "open" | "adoptable", variant: "resume" | "default"): LandingCard {
    const key = `${variant}:${state}:${session.draftScope}`;
    if (this.landingView?.key === key) { this.landingView.update(session, title, detail); return this.landingView; }
    let current = session;
    const status = element("output", "landing-card-status");
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    const card = element("div", `landing-card landing-card-${variant}`);
    const heading = element("p", "landing-card-kicker", variant === "resume" ? "Resume · Last opened on this device" : kicker);
    const body = element("div", "landing-card-body");
    const name = element("h2", "landing-card-label", title);
    const identity = element("p", "landing-card-detail");
    const metadata = element("p", "session-metadata"); metadata.title = SESSION_METADATA_HELP;
    body.append(name, identity, metadata);
    const actions = element("div", "landing-card-actions");
    const favorite = this.createFavoriteButton(() => current);
    const action = this.createOpenAction(session, state, status, () => current); action.classList.add("landing-action"); action.setAttribute("aria-label", accessibleName);
    actions.append(favorite, action); card.append(heading, body, actions, status);
    const update = (next: DashboardSession, nextTitle: string, _detail: string): void => {
      current = next; name.textContent = next.aliases[0]?.displayAlias || nextTitle;
      const realm = this.inventory?.realms.find(realm => realm.name === next.realm);
      identity.textContent = [realm?.displayName ?? next.realm, ...(realm && realm.servers.length > 1 ? [next.server] : []), ...(next.aliases.length ? [next.name] : [])].join(" · ");
      updateSessionMetadata(metadata, next); this.updateFavoriteButton(favorite, next);
      if (action instanceof HTMLAnchorElement) action.href = unifiedTerminalURL(window.location.href, next.handles.control, next, undefined, this.scrollbackRows);
    };
    update(session, title, detail);
    this.landingView = { key, el: card, action, update }; return this.landingView;
  }

  // Same authority as the row's one-click adopt (`adoptAndOpen`), minus the
  // popup: exactly one adoption POST, then a same-tab navigation with the
  // row's already-minted control handle. A refusal renders as reviewed copy and
  // leaves the page where it is.
  private async adoptAndAssign(session: DashboardSession, button: HTMLButtonElement, status: HTMLElement): Promise<void> {
    if (button.disabled || !button.isConnected || this.landingActionPending) return;
    button.disabled = true;
    this.landingActionPending = true;
    this.gate.setMutating(true);
    assignText(status, "Opening…");
    try {
      const response = await this.fetcher("/api/session-adoptions", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrfToken() },
        body: JSON.stringify({ realm: session.realm, server: session.server, session_id: session.sessionId, history_rows: ADOPTION_HISTORY_ROWS }),
        cache: "no-store",
        credentials: "same-origin",
      });
      if (!response.ok) throw new Error(adoptFailureMessage(await response.text().catch(() => ""), response.status));
      assignText(status, "");
      const openId = this.stageSessionIdentity(session);
      window.location.assign(unifiedTerminalURL(window.location.href, session.handles.control, session, openId, this.scrollbackRows));
    } catch (error) {
      assignText(status, error instanceof Error ? error.message : "The session could not be opened.");
      this.landingActionPending = false;
      this.gate.setMutating(false);
      button.disabled = false;
    }
  }
  // Filtering and collapsing are presentation only: they toggle visibility on the
  // rendered inventory and never change what was fetched or parsed. Applying them
  // to existing nodes (rather than re-rendering) preserves in-progress edits.
  private applyPresentation(): void {
    const presentation = sessionListPresentation(
      this.view.map((realm) => ({ hasError: realm.hasError, servers: realm.servers.map((server) => ({ key: server.key, hasError: server.hasError, sessions: server.sessions.map((item) => item.session) })) })),
      this.filterQuery,
      this.collapsedGroups,
    );
    this.view.forEach((realm, ri) => {
      const realmPresentation = presentation.realms[ri];
      realm.servers.forEach((server, si) => {
        const serverPresentation = realmPresentation.servers[si];
        server.sessions.forEach((item, xi) => {
          const scope = item.el.dataset.sessionScope ?? "";
          const included = this.listMode === "all" || (this.listMode === "pinned" ? this.isFavorite(scope) : this.recentAt(scope) > 0);
          item.el.hidden = !serverPresentation.sessions[xi] || !included;
        });
        server.el.hidden = !server.hasError && (normalizeSessionFilter(this.filterQuery) !== "" || this.listMode !== "all") && server.sessions.every(item => item.el.hidden);
        server.body.hidden = serverPresentation.collapsed;
        server.toggle.setAttribute("aria-expanded", String(!serverPresentation.collapsed));
        assignText(server.toggle, serverPresentation.collapsed ? "▸" : "▾");
      });
      realm.el.hidden = !realm.hasError && realm.servers.every(server => server.el.hidden);
    });
    const total = this.view.reduce((sum, realm) => sum + realm.servers.reduce((n, server) => n + server.sessions.length, 0), 0);
    const matched = this.view.reduce((sum, realm) => sum + realm.servers.reduce((n, server) => n + server.sessions.filter(item => !item.el.hidden).length, 0), 0);
    const label = normalizeSessionFilter(this.filterQuery) || this.listMode !== "all" ? `${matched} of ${total} sessions${this.listMode === "all" ? "" : this.listMode === "pinned" ? " · Favorites" : " · Recent on this device"}` : `${total} live session${total === 1 ? "" : "s"}`;
    if (this.resultStatus.textContent !== label) this.resultStatus.textContent = label;
    this.emptyResults.hidden = (!normalizeSessionFilter(this.filterQuery) && this.listMode === "all") || matched > 0;
    const emptyText = this.emptyResults.querySelector("p");
    if (emptyText) emptyText.textContent = normalizeSessionFilter(this.filterQuery) ? "No sessions match this filter." : this.listMode === "pinned" ? "No favorites are live. Use the star beside Open to save a session across your devices." : "No recent sessions are live on this device yet.";
    for (const button of Array.from(this.listModes.children)) if ((button as HTMLElement).dataset.mode) button.setAttribute("aria-pressed", String((button as HTMLElement).dataset.mode === this.listMode));
  }
  private renderServer(server: DashboardServer): ServerView {
    const key = collapseGroupKey(server.realm, server.label);
    let previous = this.serverNodes.get(key);
    if (!previous) {
      const section = element("section", "server-card"); const header = element("div", "server-heading");
      const toggle = element("button", "server-collapse"); toggle.type = "button"; toggle.setAttribute("aria-label", `Toggle sessions of ${server.realm} · ${server.label}`);
      toggle.addEventListener("click", () => { if (this.collapsedGroups.has(key)) this.collapsedGroups.delete(key); else this.collapsedGroups.add(key); writeCollapsedGroups(window.localStorage, this.collapsedGroups); this.applyPresentation(); });
      header.append(toggle, element("h3", undefined, server.label), element("span", "server-status"));
      const body = element("div", "server-body"); body.append(element("div", "session-grid"));
      section.append(header, element("p", "failure"), body);
      previous = { el: section, body, toggle, key, hasError: false, sessions: [] };
    }
    const status = previous.el.querySelector<HTMLElement>(".server-status")!;
    status.textContent = server.status === "ok" ? String(server.sessions.length) : "Unavailable"; status.className = `server-status status-${server.status}`;
    const failure = previous.el.querySelector<HTMLElement>(".failure")!; failure.textContent = server.error ?? ""; failure.hidden = !server.error;
    const grid = previous.body.querySelector<HTMLElement>(".session-grid")!;
    const sessions = [...server.sessions].sort((a, b) => {
      if (this.listMode === "recent") {
        return this.recentAt(b.draftScope) - this.recentAt(a.draftScope) || compareSessionNames(a, b);
      }
      return Number(this.isFavorite(b.draftScope)) - Number(this.isFavorite(a.draftScope)) || compareSessionNames(a, b);
    }).map(session => ({ el: this.renderSession(session), session }));
    reconcileChildren(grid, sessions.length ? sessions.map(item => item.el) : [element("p", "empty", "No live sessions")]);
    const result = { ...previous, hasError: !!server.error, sessions };
    this.serverNodes.set(key, result); return result;
  }


  /**
   * The realm's broker decides whether creation is offered at all, and decides
   * again on every request. This form is therefore only an affordance: it sends a
   * name, never a command or a directory, and reports back whatever the realm said.
   */
  private updateCreateTargets(inventory: DashboardInventory): void {
    const choices: Array<{ key: string; label: string; server: DashboardServer }> = [];
    for (const realm of inventory.realms) for (const server of realm.servers) {
      if (server.canCreate) choices.push({ key: collapseGroupKey(realm.name, server.label), label: realm.servers.length === 1 ? realm.displayName : `${realm.displayName} · ${server.label}`, server });
    }
    this.createTargets.clear(); for (const choice of choices) this.createTargets.set(choice.key, choice.server);
    const version = JSON.stringify(choices.map(({ key, label }) => [key, label]));
    if (this.createTarget.dataset.version === version) return;
    const selected = this.createTarget.value;
    const placeholder = element("option", undefined, "Choose a user…"); placeholder.value = ""; placeholder.disabled = true;
    const options = choices.map(({ key, label }) => { const option = element("option", undefined, label); option.value = key; return option; });
    this.createTarget.replaceChildren(placeholder, ...options); this.createTarget.dataset.version = version;
    this.createTarget.value = this.createSelectionExplicit && this.createTargets.has(selected) ? selected : choices.length === 1 ? choices[0].key : "";
    this.createTarget.disabled = this.creationBusy || choices.length === 0;
  }

  private renderCreate(): HTMLElement {
    const form = document.createElement("form");
    form.className = "session-create";
    const targetLabel = element("label", "session-create-label", "User");
    this.createTarget.name = "target"; this.createTarget.required = true; this.createTarget.setAttribute("aria-label", "Session user");
    this.createTarget.addEventListener("change", () => { this.createSelectionExplicit = true; });
    targetLabel.append(this.createTarget);
    const label = element("label", "session-create-label", "Session name");
    const input = document.createElement("input");
    input.name = "name";
    input.required = true;
    input.autocomplete = "off";
    input.placeholder = "e.g. research"; input.autocapitalize = "off"; input.spellcheck = false;
    input.setAttribute("aria-label", "New tmux session name");
    const aliasLabel = element("label", "session-create-label", "Alias (optional)");
    const alias = element("input"); alias.name = "display_alias"; alias.maxLength = 128; alias.autocomplete = "off"; alias.placeholder = "A friendly display name"; alias.setAttribute("aria-label", "New session alias (optional)"); aliasLabel.append(alias);
    const submit = document.createElement("button");
    submit.type = "submit";
    submit.textContent = "Create session";
    const status = element("output", "session-create-status");
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    label.append(input);
    form.append(targetLabel, label, aliasLabel, element("p", "session-create-help", "The session name is used in tmux. The alias is its display name on the dashboard."), submit, status);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      const server = this.createTargets.get(this.createTarget.value);
      if (!server || !form.reportValidity()) { this.createTarget.focus(); return; }
      void this.createSession(server, input, alias, submit, status);
    });
    return form;
  }

  private async createSession(server: DashboardServer, input: HTMLInputElement, alias: HTMLInputElement, submit: HTMLButtonElement, status: HTMLElement): Promise<void> {
    const name = input.value.trim();
    const displayAlias = alias.value.trim();
    if (!name) return;
    if (submit.disabled || this.creationBusy || !submit.isConnected) return;
    this.creationBusy = true;
    submit.disabled = input.disabled = alias.disabled = this.createTarget.disabled = true;
    this.gate.setMutating(true);
    assignText(status, "Creating…");
    try {
      const response = await this.fetcher("/api/sessions", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrfToken() },
        body: JSON.stringify({ realm: server.realm, server: server.label, name }),
        cache: "no-store",
        credentials: "same-origin",
      });
      if (response.status === 201) {
        const created: unknown = await response.json().catch(() => undefined);
        const createdID = created && typeof created === "object" && "session_id" in created && typeof created.session_id === "string" ? created.session_id : undefined;
        input.value = "";
        assignText(status, `Created ${name}.`);
        await this.refresh("mutation");
        const sessions = this.inventory?.realms.find(realm => realm.name === server.realm)?.servers.find(item => item.label === server.label)?.sessions ?? [];
        const target = sessions.find(session => session.sessionId === createdID && session.name === name);
        if (displayAlias) {
          if (!target) { assignText(status, `Created ${name}. Its alias could not be saved yet; your alias is kept in the form.`); return; }
          try {
            const request = aliasRequest(undefined, target.handles.alias, displayAlias);
            const saved = await this.fetcher(request.url, { ...request.init, headers: { ...(request.init.headers as Record<string, string>), "X-Persea-CSRF": csrfToken() }, cache: "no-store", credentials: "same-origin" });
            if (!saved.ok) throw new Error("Alias unavailable");
            alias.value = ""; await this.refresh("mutation");
          } catch { this.offerCreatedAliasRetry(target, displayAlias, alias, status); return; }
        }
        this.listMode = "all"; this.searchInput.value = this.filterQuery = "";
        this.collapsedGroups.delete(collapseGroupKey(server.realm, server.label)); this.applyPresentation();
        assignText(status, displayAlias ? `Created ${name} as ${displayAlias}.` : `Created ${name}.`);
        return;
      }
      // The response body carries a closed-set code, never broker text, so the
      // operator-facing wording lives here where it can be reviewed.
      assignText(status, createFailureMessage(await response.text().catch(() => ""), response.status));
    } catch {
      assignText(status, "The request could not be completed.");
    } finally {
      this.gate.setMutating(false);
      this.creationBusy = false;
      submit.disabled = input.disabled = alias.disabled = false; this.createTarget.disabled = this.createTargets.size === 0;
    }
  }
  private offerCreatedAliasRetry(created: DashboardSession, displayAlias: string, input: HTMLInputElement, status: HTMLElement): void {
    const retry = element("button", undefined, "Retry saving alias"); retry.type = "button";
    status.replaceChildren(element("span", undefined, `Created ${created.name}. The alias could not be saved.`), retry);
    retry.addEventListener("click", () => {
      if (this.creationBusy || !retry.isConnected) return;
      this.creationBusy = true; this.gate.setMutating(true);
      const controls = Array.from(this.creationPanel.querySelectorAll<HTMLInputElement | HTMLButtonElement | HTMLSelectElement>("input,button,select"));
      for (const control of controls) control.disabled = true;
      void (async () => {
        try {
          await this.refresh("mutation");
          const target = this.inventory?.realms.flatMap(realm => realm.servers.flatMap(server => server.sessions)).find(session => session.draftScope === created.draftScope);
          if (!target) { status.textContent = `Created ${created.name}, but that session is no longer available. The alias was not applied to another session.`; return; }
          if (!target.aliases.some(alias => alias.displayAlias === displayAlias)) {
            const request = aliasRequest(undefined, target.handles.alias, displayAlias);
            const response = await this.fetcher(request.url, { ...request.init, headers: { ...(request.init.headers as Record<string, string>), "X-Persea-CSRF": csrfToken() }, cache: "no-store", credentials: "same-origin" });
            if (!response.ok) throw new Error("Alias unavailable");
            await this.refresh("mutation");
          }
          input.value = ""; status.textContent = `Created ${created.name} as ${displayAlias}.`;
        } catch { this.offerCreatedAliasRetry(created, displayAlias, input, status); }
        finally {
          this.creationBusy = false; this.gate.setMutating(false);
          for (const control of controls) control.disabled = false;
          this.createTarget.disabled = this.createTargets.size === 0;
        }
      })();
    });
  }
  // Both thumbnails and the modal share one capture. Only an explicit refresh
  // replaces it; wide rows request their first capture when they become visible.
  private renderPreview(session: DashboardSession, currentSession = () => session): { el: HTMLElement; rowButton: HTMLButtonElement; load: () => void; dispose: () => void } {
    const block = element("div", "session-preview");
    const header = element("div", "session-preview-header");
    const refresh = element("button", "session-preview-refresh", "↻");
    refresh.type = "button";
    refresh.title = `Refresh preview of ${session.name}`;
    refresh.setAttribute("aria-label", `Refresh preview of ${session.name}`);
    const meta = element("span", "session-preview-meta");
    header.append(element("span", "session-preview-title", "Output snapshot"), refresh);
    const thumbnail = element("button", "session-preview-thumbnail"); thumbnail.type = "button"; thumbnail.disabled = true;
    thumbnail.setAttribute("aria-label", `Enlarge output preview of ${session.name}`); thumbnail.setAttribute("aria-haspopup", "dialog");
    const screen = element("span", "session-preview-screen", "Loading preview…"); screen.setAttribute("aria-hidden", "true");
    thumbnail.append(screen, element("span", "session-preview-enlarge", "Expand ⤢"));
    const rowButton = element("button", "session-preview-row-button"); rowButton.type = "button";
    rowButton.setAttribute("aria-label", `Preview output of ${session.name}`); rowButton.setAttribute("aria-haspopup", "dialog"); rowButton.title = `Preview output of ${session.name}`;
    const rowScreen = element("span", "session-preview-screen session-preview-row-screen"); rowScreen.setAttribute("aria-hidden", "true");
    const rowLabel = element("span", "session-preview-row-label", "Preview");
    rowButton.append(rowScreen, dashboardIcon("preview"), rowLabel);
    const notice = element("output", "session-preview-notice"); notice.setAttribute("role", "status");
    block.append(header, thumbnail, meta, notice);
    let busy = false;
    let disposed = false;
    let preview: SessionPreview | undefined;
    let controller: AbortController | undefined;
    let dialog: HTMLDialogElement | undefined;
    let modalScreen: HTMLPreElement | undefined;
    let modalMeta: HTMLElement | undefined;
    let modalNotice: HTMLElement | undefined;
    let modalRefresh: HTMLButtonElement | undefined;
    const paint = (target: HTMLElement): void => {
      if (!preview) return;
      let end = preview.rows.length;
      while (end > 1 && preview.rows[end - 1].trim() === "") end--;
      renderTerminalPreview(target, preview.rows.slice(0, end), preview.ansiRows?.slice(0, end), unifiedTheme(this.preferences.snapshot().preferences.theme));
      target.scrollTop = target.scrollHeight;
    };
    const sync = (): void => {
      refresh.disabled = busy;
      if (modalRefresh) modalRefresh.disabled = busy;
      if (modalNotice) modalNotice.textContent = notice.textContent;
      thumbnail.disabled = !preview;
      rowButton.dataset.loaded = String(!!preview);
      rowButton.setAttribute("aria-busy", String(busy));
      refresh.setAttribute("aria-busy", String(busy));
      if (preview) { assignText(meta, formatPreviewMeta(preview)); if (modalMeta) assignText(modalMeta, formatPreviewMeta(preview)); }
    };
    const load = (force = false): void => {
      if (busy || disposed || this.destroyed || !block.isConnected || preview && !force) return;
      busy = true; notice.textContent = ""; sync();
      controller = new AbortController();
      void (async () => {
        try {
          const response = await this.fetcher(previewRequestPath(currentSession()), { cache: "no-store", credentials: "same-origin", signal: controller!.signal });
          if (disposed || !block.isConnected || this.destroyed) return;
          if (!response.ok) {
            const message = previewFailureMessage(await response.text().catch(() => ""), response.status);
            assignText(notice, message); if (!preview) { assignText(screen, "Preview unavailable"); if (modalScreen) assignText(modalScreen, "Preview unavailable"); }
            return;
          }
          const next = parsePreview(await response.json());
          if (disposed || !block.isConnected || this.destroyed) return;
          const changed = JSON.stringify([preview?.rows, preview?.ansiRows]) !== JSON.stringify([next.rows, next.ansiRows]);
          preview = next;
          if (changed) { paint(screen); paint(rowScreen); if (modalScreen) paint(modalScreen); }
        } catch {
          if (!disposed) { assignText(notice, "The preview could not be loaded. Use its Refresh button to retry."); if (!preview) { assignText(screen, "Preview unavailable"); if (modalScreen) assignText(modalScreen, "Preview unavailable"); } }
        } finally {
          busy = false; sync();
        }
      })();
    };
    refresh.addEventListener("click", () => load(true));
    const enlarge = (trigger: HTMLButtonElement): void => {
      if (disposed || !block.isConnected) return;
      this.previewDialog?.close();
      dialog = element("dialog", "session-preview-dialog"); this.previewDialog = dialog;
      const heading = element("div", "dashboard-panel-heading");
      const name = element("h2", undefined, `Output · ${currentSession().aliases[0]?.displayAlias || currentSession().name}`); name.id = `preview-title-${this.sessionNodes.size}-${Date.now()}`;
      const close = element("button", "dashboard-icon-button", "×"); close.type = "button"; close.setAttribute("aria-label", "Close output preview");
      close.addEventListener("click", () => dialog?.close()); heading.append(name, close);
      dialog.setAttribute("aria-labelledby", name.id);
      modalScreen = element("pre", "session-preview-screen"); modalScreen.tabIndex = 0; modalScreen.setAttribute("aria-label", `Output snapshot of ${currentSession().name}`);
      if (!preview) modalScreen.textContent = "Loading preview…";
      modalMeta = element("p", "session-preview-meta");
      modalNotice = element("output", "session-preview-notice"); modalNotice.setAttribute("role", "status");
      modalRefresh = element("button", "session-preview-refresh", "↻ Refresh preview"); modalRefresh.type = "button"; modalRefresh.addEventListener("click", () => load(true));
      const footer = element("div", "session-preview-dialog-footer"); footer.append(modalMeta, modalRefresh);
      const explanation = element("p", "session-preview-explanation", "A snapshot of recent output. It stays still until you refresh it.");
      dialog.append(heading, modalScreen, explanation, footer, modalNotice);
      dialog.addEventListener("click", event => {
        if (event.target !== dialog || !dialog) return;
        const bounds = dialog.getBoundingClientRect();
        if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) dialog.close();
      });
      const opened = dialog;
      opened.addEventListener("close", () => {
        opened.remove(); if (this.previewDialog === opened) this.previewDialog = undefined;
        if (dialog === opened) { dialog = undefined; modalScreen = undefined; modalMeta = undefined; modalNotice = undefined; modalRefresh = undefined; }
        if (trigger.isConnected && !disposed && !this.previewDialog) trigger.focus({ preventScroll: true });
      });
      this.root.append(dialog); dialog.showModal(); paint(modalScreen); sync(); close.focus();
      load();
    };
    thumbnail.addEventListener("click", () => enlarge(thumbnail));
    rowButton.addEventListener("click", () => enlarge(rowButton));
    const observer = new IntersectionObserver(entries => {
      if (!entries.some(entry => entry.isIntersecting) || document.visibilityState !== "visible") return;
      observer.disconnect();
      this.previewQueue = this.previewQueue.then(async () => {
        if (disposed || this.destroyed || preview) return;
        const bounds = rowScreen.getBoundingClientRect();
        if (document.visibilityState !== "visible" || !bounds.width || bounds.bottom < 0 || bounds.top > window.innerHeight) { observer.observe(rowScreen); return; }
        load();
        // Keep passive captures below the server's two-per-second budget.
        await new Promise<void>(resolve => window.setTimeout(resolve, 550));
      });
    });
    observer.observe(rowScreen);
    const resumeObservation = (): void => {
      if (document.visibilityState === "visible" && !disposed && !preview) { observer.unobserve(rowScreen); observer.observe(rowScreen); }
    };
    document.addEventListener("visibilitychange", resumeObservation);
    let theme = this.preferences.snapshot().preferences.theme;
    const paletteSubscription = this.preferences.subscribe(snapshot => {
      if (snapshot.preferences.theme !== theme) { theme = snapshot.preferences.theme; if (preview) { paint(screen); paint(rowScreen); if (modalScreen) paint(modalScreen); } }
    });
    const dispose = (): void => { disposed = true; observer.disconnect(); document.removeEventListener("visibilitychange", resumeObservation); paletteSubscription.dispose(); controller?.abort(); dialog?.close(); dialog?.remove(); };
    return { el: block, rowButton, load, dispose };
  }
  private renderSession(session: DashboardSession): HTMLElement {
    const existing = this.sessionNodes.get(session.draftScope);
    if (existing) { existing.update(session); return existing.el; }
    const article = element("article", "session-card");
    const domId = ++this.nextSessionDOMId;
    article.dataset.sessionScope = session.draftScope;
    const sessionKey = sessionStateKey(session);
    const expanded = this.expandedSessions.has(sessionKey);
    const row = element("div", "session-row");
    const disclosure = element("button", "session-disclosure"); disclosure.append(dashboardIcon("info"));
    disclosure.type = "button";
    disclosure.setAttribute("aria-label", `Session information for ${session.name}`); disclosure.title = "Session information";
    disclosure.setAttribute("aria-expanded", String(expanded));
    const title = element("div", "session-title");
    const name = element("h4", "session-name", session.name); name.title = session.name;
    const alias = element("span", "alias-badge");
    const editAlias = element("button", "session-alias-edit dashboard-icon-button"); editAlias.type = "button"; editAlias.append(dashboardIcon("edit"));
    editAlias.setAttribute("aria-label", `Edit alias for ${session.name}`); editAlias.title = "Edit alias"; editAlias.setAttribute("aria-haspopup", "dialog");
    const names = element("div", "session-names"); names.append(name, editAlias, alias);
    const metadata = element("span", "session-metadata"); metadata.title = SESSION_METADATA_HELP;
    title.append(names);
    const unifiedStatus = element("span", "session-unified-status");
    unifiedStatus.setAttribute("role", "status");
    const open = element("div", "session-actions session-open");
    row.append(title, metadata, unifiedStatus);
    const detail = element("div", "session-detail");
    detail.hidden = !expanded;
    const facts = element("dl", "session-facts");
    const editors = element("div", "alias-editors");
    const aliasDialog = element("dialog", "session-alias-dialog");
    const aliasHeading = element("h2", undefined, `Alias for ${session.name}`); aliasHeading.id = `alias-heading-${domId}`;
    aliasDialog.setAttribute("aria-labelledby", aliasHeading.id);
    const closeAlias = element("button", "dashboard-icon-button", "×"); closeAlias.type = "button"; closeAlias.setAttribute("aria-label", "Close alias editor");
    const aliasHeader = element("div", "dashboard-panel-heading"); aliasHeader.append(aliasHeading, closeAlias);
    aliasDialog.append(aliasHeader, element("p", "session-alias-help", "A display name shared across your devices. The tmux session name stays the same."), editors);
    closeAlias.addEventListener("click", () => aliasDialog.close());
    editAlias.addEventListener("click", () => { aliasDialog.showModal(); editors.querySelector("input")?.focus(); });
    aliasDialog.addEventListener("close", () => { if (editAlias.isConnected) editAlias.focus({ preventScroll: true }); });
    const preview = this.renderPreview(session, () => session);
    const history = element("p", "session-history-origin");
    const pin = this.createFavoriteButton(() => session);
    const secondaryActions = element("div", "session-secondary-actions"); secondaryActions.append(preview.rowButton, pin, disclosure);
    const rowActions = element("div", "session-row-actions"); rowActions.append(secondaryActions, open); row.append(rowActions);
    const info = element("section", "session-information");
    const infoHeading = element("h3", undefined, "Session information"); infoHeading.id = `session-info-${domId}`;
    info.setAttribute("aria-labelledby", infoHeading.id); info.append(infoHeading, facts, history);
    detail.id = `session-detail-${domId}`; disclosure.setAttribute("aria-controls", detail.id);
    detail.append(info, preview.el);
    let aliasVersion = ""; let actionVersion = "";
    const update = (next: DashboardSession): void => {
      session = next;
      name.textContent = session.name; name.title = session.name;
      alias.textContent = session.aliases[0]?.displayAlias ?? ""; alias.title = alias.textContent; alias.hidden = !alias.textContent;
      updateSessionMetadata(metadata, session);
      const pinned = this.isFavorite(session.draftScope);
      article.dataset.pinned = String(pinned);
      this.updateFavoriteButton(pin, session);
      const rows = [["Session", session.name], ["Session id", session.sessionId], ["Identity", `${session.realm} · UID ${session.uid} · ${session.server}`], ["Size", `${session.width} × ${session.height}`], ["Attached clients", String(session.attached)], ["Last output", session.outputActivity ? formatActivity(session.outputActivity) : "Unknown"], ["Last interaction", session.activity ? formatActivity(session.activity) : "Unknown"]];
      facts.replaceChildren(...rows.flatMap(([term, fact]) => [element("dt", undefined, term), element("dd", undefined, fact)]));
      history.hidden = session.unified?.state !== "open" || session.unified.origin !== "reconstructed";
      history.textContent = "Earlier output was imported from tmux when browser access began. The scrollback choice sets the import limit for newly enabled sessions and the number of rows retained in the browser. Previously discarded output cannot be recovered by increasing it. New output is recorded as it arrives.";
      const nextAliases = JSON.stringify(session.aliases);
      const reloadAlias = editors.querySelector('[data-reload="true"]') !== null;
      if ((nextAliases !== aliasVersion || reloadAlias) && (reloadAlias || (!hasDraft(editors) && (!editors.contains(document.activeElement) || editors.querySelector('[data-saved="true"]'))))) {
        aliasVersion = nextAliases;
        editors.replaceChildren(...(session.aliases.length ? session.aliases : [undefined]).map((item, index) => this.renderAliasEditor(session, item, index, () => session)));
      }
      const nextActions = JSON.stringify(session.unified ?? null);
      if (nextActions !== actionVersion) {
        actionVersion = nextActions; open.replaceChildren(); unifiedStatus.textContent = "";
        if (session.unified) this.renderUnifiedPrimary(session, session.unified, open, unifiedStatus, () => session);
        if (!session.unified) unifiedStatus.textContent = "Browser access is unavailable for this session.";
      }
      const primary = open.querySelector<HTMLAnchorElement>("a.action-unified-open");
      if (primary) primary.href = unifiedTerminalURL(window.location.href, session.handles.control, session, undefined, this.scrollbackRows);
    };
    editors.addEventListener("focusout", () => queueMicrotask(() => { if (article.isConnected) update(session); }));
    if (expanded) queueMicrotask(preview.load);
    disclosure.addEventListener("click", () => {
      const next = !this.expandedSessions.has(sessionKey);
      if (next) this.expandedSessions.add(sessionKey); else this.expandedSessions.delete(sessionKey);
      detail.hidden = !next;
      disclosure.setAttribute("aria-expanded", String(next));
      if (next) preview.load();
    });
    article.append(row, detail, aliasDialog);
    update(session);
    const dispose = (): void => { preview.dispose(); aliasDialog.close(); aliasDialog.remove(); };
    this.cleanup.push(dispose);
    this.sessionNodes.set(sessionKey, { el: article, update, dispose });
    return article;
  }
  // The row's ONE primary action when the broker projects a unified state:
  // open navigates same-tab with the row's minted control handle, adoptable
  // adopts then opens in the same tab, and
  // blocked states render their reason as row text — never a dead button.
  private renderUnifiedPrimary(session: DashboardSession, unified: UnifiedSessionProjection, open: HTMLElement, status: HTMLElement, currentSession = () => session): void {
    if (unified.state === "open") {
      open.append(this.createOpenAction(session, "open", status, currentSession));
      if (unified.detail === "rotation_deferred_alt_screen") {
        const reason = "Journal rotation is deferred while a full-screen app is active.";
        assignText(status, reason);
        status.title = reason;
        status.setAttribute("aria-label", reason);
      }
      return;
    }
    if (unified.state === "adoptable") {
      open.append(this.createOpenAction(session, "adoptable", status, currentSession));
      return;
    }
    const reason = unifiedBlockedMessage(unified.state);
    assignText(status, reason);
    status.title = reason;
    status.setAttribute("aria-label", reason);
  }

  private createOpenAction(session: DashboardSession, state: "open" | "adoptable", status: HTMLElement, currentSession = () => session): HTMLAnchorElement | HTMLButtonElement {
    const action = state === "open" ? element("a", "action session-open-action action-unified-open") : element("button", "action session-open-action action-unified-adopt");
    const icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    icon.setAttribute("viewBox", "0 0 20 20"); icon.setAttribute("aria-hidden", "true"); icon.setAttribute("focusable", "false");
    const path = document.createElementNS(icon.namespaceURI, "path");
    path.setAttribute("d", "M3 3.5h14v13H3z M6 7l3 3-3 3 M11 13h3"); icon.append(path);
    action.append(icon, element("span", undefined, "Open"));
    action.title = `Open ${session.name}`; action.setAttribute("aria-label", `Open ${session.name}`);
    if (action instanceof HTMLAnchorElement) {
      action.href = unifiedTerminalURL(window.location.href, session.handles.control, session, undefined, this.scrollbackRows);
      action.addEventListener("click", event => {
        if (!event.isTrusted || !action.isConnected || this.landingActionPending) { event.preventDefault(); return; }
        const current = currentSession();
        action.href = unifiedTerminalURL(window.location.href, current.handles.control, current, this.stageSessionIdentity(current), this.scrollbackRows);
      });
    } else {
      action.type = "button";
      action.addEventListener("click", event => { if (event.isTrusted && action.isConnected) void this.adoptAndAssign(currentSession(), action, status); });
    }
    return action;
  }

  private createFavoriteButton(currentSession: () => DashboardSession): HTMLButtonElement {
    const button = element("button", "session-pin dashboard-icon-button"); button.type = "button";
    button.addEventListener("click", () => {
      if (!button.isConnected || button.disabled || button.getAttribute("aria-disabled") === "true") return;
      const session = currentSession(); void this.favorites.set(session.draftScope, !this.isFavorite(session.draftScope));
    });
    this.updateFavoriteButton(button, currentSession()); return button;
  }

  private updateFavoriteButton(button: HTMLButtonElement, session: DashboardSession): void {
    const state = this.favorites.snapshot(); const selected = state.favorites.includes(session.draftScope);
    button.textContent = selected ? "★" : "☆"; button.setAttribute("aria-pressed", String(selected));
    button.setAttribute("aria-label", `${selected ? "Remove" : "Add"} ${session.name} ${selected ? "from" : "to"} favorites`);
    button.title = state.available ? "Favorites are shared across your devices" : state.loaded ? "Favorites are temporarily unavailable" : "Loading favorites";
    button.disabled = !state.available;
    button.setAttribute("aria-disabled", String(!state.available || state.pending.includes(session.draftScope)));
    button.setAttribute("aria-busy", String(state.pending.includes(session.draftScope)));
  }

  private renderAliasEditor(session: DashboardSession, alias: DashboardAlias | undefined, index: number, currentSession = () => session): HTMLElement {
    const form = element("form", "alias-editor");
    const label = element("label", undefined, "Alias");
    const input = element("input"); input.name = "display_alias"; input.type = "text"; input.maxLength = 128; input.autocomplete = "off";
    input.value = input.defaultValue = alias?.displayAlias ?? ""; input.setAttribute("aria-label", `Alias for ${session.name}${index ? ` ${index + 1}` : ""}`);
    label.append(input);
    const save = element("button", undefined, "Save"); save.type = "submit"; save.setAttribute("aria-label", `Save alias for ${session.name}`);
    form.append(label, save);
    if (alias) { const clear = element("button", "alias-clear dashboard-icon-button", "×"); clear.type = "button"; clear.title = "Clear alias"; clear.setAttribute("aria-label", `Clear alias for ${session.name}`); clear.addEventListener("click", () => void this.mutate(currentSession(), alias, undefined, form)); form.append(clear); }
    form.addEventListener("submit", event => { event.preventDefault(); void this.mutate(currentSession(), alias, input.value, form); });
    input.addEventListener("input", () => { delete form.dataset.reload; });
    const status = element("output", "alias-status"); status.setAttribute("role", "status"); status.setAttribute("aria-live", "polite"); form.append(status);
    return form;
  }
  private async mutate(session: DashboardSession, alias: DashboardAlias | undefined, displayAlias: string | undefined, form: HTMLFormElement): Promise<void> {
    if (form.dataset.busy === "true") return;
    form.dataset.busy = "true"; this.gate.setMutating(true);
    form.querySelector<HTMLElement>(".alias-status")!.textContent = "Saving alias…";
    form.querySelectorAll<HTMLInputElement | HTMLButtonElement>("input, button").forEach(control => { control.disabled = true; });
    try {
      const request = aliasRequest(alias, session.handles.alias, displayAlias);
      const response = await this.fetcher(request.url, { ...request.init, headers: { ...(request.init.headers as Record<string,string>), "X-Persea-CSRF": csrfToken() }, cache: "no-store", credentials: "same-origin" });
      if (!response.ok) {
        const status = form.querySelector<HTMLElement>(".alias-status")!;
        status.textContent = mutationStatus(response.status) ?? `Alias request failed (${response.status}).`;
        if (response.status === 409) {
          const reload = element("button", undefined, "Reload saved alias"); reload.type = "button";
          const keep = element("button", undefined, "Keep editing"); keep.type = "button";
          keep.addEventListener("click", () => { delete form.dataset.reload; form.querySelector("input")?.focus(); });
          reload.addEventListener("click", () => {
            form.dataset.reload = "true";
            void this.refresh("manual").finally(() => { delete form.dataset.reload; this.sessionNodes.get(session.draftScope)?.el.querySelector<HTMLElement>(".alias-editor input")?.focus(); });
          });
          status.replaceChildren(element("span", undefined, "This alias changed elsewhere. Your draft is kept."), reload, keep);
        }
        return;
      }
      for (const input of Array.from(form.querySelectorAll("input"))) input.defaultValue = input.value;
      form.dataset.busy = "false"; form.dataset.saved = "true";
      await this.refresh("mutation");
      this.sessionNodes.get(session.draftScope)?.el.querySelector<HTMLDialogElement>(".session-alias-dialog")?.close();
      this.status.textContent = displayAlias === undefined ? "Alias cleared." : "Alias saved.";
    } catch { form.querySelector<HTMLElement>(".alias-status")!.textContent = "Alias request could not be completed. Your draft is kept."; }
    finally { this.gate.setMutating(false); form.dataset.busy = "false"; form.querySelectorAll<HTMLInputElement | HTMLButtonElement>("input, button").forEach(control => { control.disabled = false; }); }
  }
}
