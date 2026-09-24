// The one workspace route constructor and its parser (packet §5, freeze edit
// [F1], regression test FW-R).
//
// Every workspace URL the product emits is built here and nowhere else:
//   /workspace?engine=unified-dev#name=<bounded>
// The query string is the CSP capability key and must remain byte-exact
// (same invariant as unifiedTerminalURL in dashboard.ts and the boot()
// capability check in app.ts). The fragment carries the workspace NAME only —
// no handle, no authority (§3b doctrine: the page resolves everything at
// load), and never `engine`, so the terminal-specific fragment self-heal in
// app.ts (fragment engine=unified-dev → replace to /terminal) can never be
// triggered by a workspace document.
//
// The parser is pure: it takes a location-shaped value and returns a typed
// verdict. It never navigates, never touches `window`, and never redirects —
// a wrong query is a fatal typed refusal rendered by the page, not healed.

import { sessionLabelProblem } from "./workspace_model";

export const WORKSPACE_PATH = "/workspace";
export const WORKSPACE_ENGINE_SEARCH = "?engine=unified-dev";
export const WORKSPACE_FRAGMENT_KEYS: ReadonlySet<string> = new Set(["name"]);
// Bounded exactly like display labels (app.ts labelFromFragment): trimmed,
// nonempty, at most 128 characters, no control characters. Refused, never
// rewritten.
export const WORKSPACE_NAME_MAX_LENGTH = 128;

export function boundedWorkspaceName(value: string | null | undefined): string | undefined {
  if (value === null || value === undefined) return undefined;
  if (sessionLabelProblem(value) !== undefined) return undefined;
  return value;
}

// Builds `/workspace?engine=unified-dev#name=…` against `base`. Throws on an
// unbounded name: a caller holding an invalid name has no workspace to open,
// and emitting a URL that the parser will refuse on arrival is a dead link.
export function workspaceURL(base: string, name: string): string {
  const bounded = boundedWorkspaceName(name);
  if (bounded === undefined || bounded !== name) throw new Error("workspace name must be a bounded display label");
  const url = new URL(WORKSPACE_PATH, base);
  url.search = WORKSPACE_ENGINE_SEARCH;
  url.hash = new URLSearchParams({ name }).toString();
  return url.toString();
}

export type WorkspaceRouteRefusal =
  | Readonly<{ code: "wrong_path"; pathname: string }>
  | Readonly<{ code: "query_missing" }>
  | Readonly<{ code: "query_mismatch"; search: string }>
  | Readonly<{ code: "fragment_engine_rejected" }>
  | Readonly<{ code: "fragment_unknown_key"; key: string }>
  | Readonly<{ code: "fragment_duplicate_key"; key: string }>
  | Readonly<{ code: "name_missing" }>
  | Readonly<{ code: "name_unbounded" }>;

export type WorkspaceRouteVerdict =
  | Readonly<{ ok: true; name: string }>
  | Readonly<{ ok: false; refusal: WorkspaceRouteRefusal }>;

export type LocationLike = Readonly<{ pathname: string; search: string; hash: string }>;

function refuse(refusal: WorkspaceRouteRefusal): WorkspaceRouteVerdict { return Object.freeze({ ok: false, refusal: Object.freeze(refusal) }); }

export function parseWorkspaceLocation(location: LocationLike): WorkspaceRouteVerdict {
  if (location.pathname !== WORKSPACE_PATH) return refuse({ code: "wrong_path", pathname: location.pathname });
  // Byte-exact: a missing, different, extra, or duplicated query field is a
  // refusal, never a redirect (FW-R case c).
  if (location.search === "") return refuse({ code: "query_missing" });
  if (location.search !== WORKSPACE_ENGINE_SEARCH) return refuse({ code: "query_mismatch", search: location.search });
  const fragment = new URLSearchParams(location.hash.startsWith("#") ? location.hash.slice(1) : location.hash);
  const seen = new Set<string>();
  let problem: WorkspaceRouteRefusal | undefined;
  fragment.forEach((_value, key) => {
    if (problem) return;
    // `engine` is not a workspace fragment key. It is refused by name (FW-R
    // case d) so the ordering guard in app.ts has a typed reason to render.
    if (key === "engine") { problem = { code: "fragment_engine_rejected" }; return; }
    if (!WORKSPACE_FRAGMENT_KEYS.has(key)) { problem = { code: "fragment_unknown_key", key }; return; }
    if (seen.has(key)) { problem = { code: "fragment_duplicate_key", key }; return; }
    seen.add(key);
  });
  if (problem) return refuse(problem);
  const raw = fragment.get("name");
  if (raw === null) return refuse({ code: "name_missing" });
  const name = boundedWorkspaceName(raw);
  if (name === undefined) return refuse({ code: "name_unbounded" });
  return Object.freeze({ ok: true, name });
}

export type WorkspaceRouteNotice = Readonly<{ headline: string; detail: string; code: string }>;

// Full-page copy for a typed route refusal (§7 workspace-level failure): a
// sentence, the code, and a way back to the dashboard. Closed set — every
// refusal has reviewed wording, none falls through to a blank page.
export function workspaceRouteNotice(refusal: WorkspaceRouteRefusal): WorkspaceRouteNotice {
  switch (refusal.code) {
    case "wrong_path": return { headline: "This is not a workspace address", detail: "Workspaces open at /workspace. Pick one from the dashboard.", code: refusal.code };
    case "query_missing": return { headline: "This workspace link is incomplete", detail: "The address is missing the unified engine selector. Open the workspace from the dashboard.", code: refusal.code };
    case "query_mismatch": return { headline: "This workspace link is not valid", detail: "The address carries a query this page does not accept. Open the workspace from the dashboard.", code: refusal.code };
    case "fragment_engine_rejected": return { headline: "This workspace link is not valid", detail: "The address carries an engine selector where only the workspace name belongs. Open the workspace from the dashboard.", code: refusal.code };
    case "fragment_unknown_key": return { headline: "This workspace link is not valid", detail: `The address carries an unknown field (${refusal.key}). Open the workspace from the dashboard.`, code: refusal.code };
    case "fragment_duplicate_key": return { headline: "This workspace link is not valid", detail: `The address repeats a field (${refusal.key}). Open the workspace from the dashboard.`, code: refusal.code };
    case "name_missing": return { headline: "This workspace link names no workspace", detail: "The address has no workspace name. Pick a workspace from the dashboard.", code: refusal.code };
    case "name_unbounded": return { headline: "This workspace name is not allowed", detail: "The name is empty, too long, or carries control characters. Pick a workspace from the dashboard.", code: refusal.code };
  }
}
