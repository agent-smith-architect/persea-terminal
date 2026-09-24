// M9 W1a — FW-R route constructor regression test.
// (a) dashboard open and (b) saved-workspace reload both produce exactly
// /workspace?engine=unified-dev#name=<bounded>; (c) a missing, different,
// extra, or duplicated query field is a typed refusal with no navigation;
// (d) a fragment `engine` key is rejected as unknown and never navigates;
// (e) the /terminal self-heal ordering guard lives in app.ts — W1b.
declare const process: { stdout: { write(value: string): void } };
import {
  WORKSPACE_ENGINE_SEARCH, WORKSPACE_NAME_MAX_LENGTH, WORKSPACE_PATH, boundedWorkspaceName, parseWorkspaceLocation, workspaceRouteNotice, workspaceURL,
  type WorkspaceRouteRefusal,
} from "../src/workspace_url";
import { workspaceURL as dashboardWorkspaceURL } from "../src/dashboard";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
  throws(fn: () => unknown, message = "expected function to throw"): void { try { fn(); } catch { return; } throw new Error(message); },
};

function refusalOf(location: { pathname: string; search: string; hash: string }, message: string): WorkspaceRouteRefusal {
  const verdict = parseWorkspaceLocation(location);
  if (verdict.ok) throw new Error(`${message}: accepted ${verdict.name}`);
  return verdict.refusal;
}

// A location stub with navigation spies: the parser must never call them.
// (It cannot even name them — the type it accepts has no replace/assign — but
// the spy pins the runtime fact for FW-R (c) and (d).)
function spiedLocation(pathname: string, search: string, hash: string): { pathname: string; search: string; hash: string; replace: (url: string) => void; assign: (url: string) => void; navigations: string[] } {
  const navigations: string[] = [];
  return { pathname, search, hash, navigations, replace: (url: string) => { navigations.push(`replace:${url}`); }, assign: (url: string) => { navigations.push(`assign:${url}`); } };
}

// (a) dashboard "Workspaces" open: exact selector-complete URL.
{
  const url = workspaceURL("https://terminal.example/", "ops wall");
  assert.equal(url, "https://terminal.example/workspace?engine=unified-dev#name=ops+wall", "FW-R (a) exact workspace URL");
  const parsed = new URL(url);
  assert.equal(parsed.pathname, WORKSPACE_PATH);
  assert.equal(parsed.search, WORKSPACE_ENGINE_SEARCH, "the query is the byte-exact CSP capability key");
  assert.equal(parsed.hash, "#name=ops+wall");
  // The base's own path, query, and fragment never leak into the workspace URL.
  assert.equal(workspaceURL("https://terminal.example/terminal?engine=unified-dev#handle=abc&mode=control", "x"), "https://terminal.example/workspace?engine=unified-dev#name=x", "base authority is discarded");
  assert.equal(workspaceURL("http://127.0.0.1:8080/", "a&b=c"), "http://127.0.0.1:8080/workspace?engine=unified-dev#name=a%26b%3Dc", "name is fragment-encoded");
  assert.equal(dashboardWorkspaceURL, workspaceURL, "dashboard.ts re-exports the one constructor");
  // Unbounded names have no workspace to open: constructing a dead link throws.
  for (const bad of ["", " ", " padded", "padded ", "a\u0000b", "x".repeat(WORKSPACE_NAME_MAX_LENGTH + 1)]) assert.throws(() => workspaceURL("https://terminal.example/", bad), `unbounded name ${JSON.stringify(bad)} must throw`);
  assert.equal(workspaceURL("https://terminal.example/", "x".repeat(WORKSPACE_NAME_MAX_LENGTH)).length > 0, true, "128 characters is the bound");
}

// (b) saved-workspace reload: the constructed URL parses back to the same name,
// including a name with unicode and reserved characters.
{
  for (const name of ["ops wall", "wall #2 — ops", "a&b=c", "名前", "x".repeat(WORKSPACE_NAME_MAX_LENGTH)]) {
    const url = new URL(workspaceURL("https://terminal.example/", name));
    const location = spiedLocation(url.pathname, url.search, url.hash);
    const verdict = parseWorkspaceLocation(location);
    assert.ok(verdict.ok && verdict.name === name, `FW-R (b) reload round trip for ${JSON.stringify(name)}`);
    assert.equal(location.navigations.length, 0, "parsing never navigates");
  }
  // Both `+` and `%20` spell a space in the fragment; the parser accepts what URLSearchParams accepts.
  const legacy = parseWorkspaceLocation({ pathname: "/workspace", search: "?engine=unified-dev", hash: "#name=ops%20wall" });
  assert.ok(legacy.ok && legacy.name === "ops wall", "percent-encoded space");
  const bare = parseWorkspaceLocation({ pathname: "/workspace", search: "?engine=unified-dev", hash: "name=x" });
  assert.ok(bare.ok && bare.name === "x", "hash without the leading # is accepted");
}

// (c) missing, different, extra, or duplicated query field → typed refusal, no navigation.
{
  const cases: Array<[string, WorkspaceRouteRefusal["code"]]> = [
    ["", "query_missing"],
    ["?engine=other", "query_mismatch"],
    ["?engine=unified-dev&x=1", "query_mismatch"],
    ["?x=1&engine=unified-dev", "query_mismatch"],
    ["?engine=unified-dev&engine=unified-dev", "query_mismatch"],
    ["?Engine=unified-dev", "query_mismatch"],
    ["?engine=unified-dev ", "query_mismatch"],
    ["?engine=unified-dev#", "query_mismatch"],
  ];
  for (const [search, code] of cases) {
    const location = spiedLocation("/workspace", search, "#name=x");
    const refusal = refusalOf(location, `FW-R (c) ${JSON.stringify(search)}`);
    assert.equal(refusal.code, code, `FW-R (c) ${JSON.stringify(search)} refusal code`);
    assert.equal(location.navigations.length, 0, `FW-R (c) ${JSON.stringify(search)} must not navigate`);
  }
  const wrong = refusalOf(spiedLocation("/terminal", "?engine=unified-dev", "#name=x"), "wrong path");
  assert.equal(wrong.code, "wrong_path");
}

// (d) fragment `engine` is rejected as unknown; no /terminal self-heal is reachable from here.
{
  for (const hash of ["#name=x&engine=unified-dev", "#engine=unified-dev&name=x", "#engine=unified-dev", "#engine=other&name=x"]) {
    const location = spiedLocation("/workspace", "?engine=unified-dev", hash);
    const refusal = refusalOf(location, `FW-R (d) ${hash}`);
    assert.equal(refusal.code, "fragment_engine_rejected", `FW-R (d) ${hash} refusal code`);
    assert.equal(location.navigations.length, 0, `FW-R (d) ${hash} must not navigate`);
    assert.equal(location.pathname, "/workspace", "location untouched");
  }
  const unknown = refusalOf(spiedLocation("/workspace", "?engine=unified-dev", "#name=x&handle=abc"), "unknown key");
  assert.ok(unknown.code === "fragment_unknown_key" && unknown.key === "handle", "a handle in the fragment is unknown: no authority rides a workspace URL");
  const duplicate = refusalOf(spiedLocation("/workspace", "?engine=unified-dev", "#name=a&name=b"), "duplicate name");
  assert.ok(duplicate.code === "fragment_duplicate_key" && duplicate.key === "name");
  assert.equal(refusalOf(spiedLocation("/workspace", "?engine=unified-dev", "#"), "empty fragment").code, "name_missing");
  assert.equal(refusalOf(spiedLocation("/workspace", "?engine=unified-dev", ""), "no fragment").code, "name_missing");
  for (const hash of ["#name=", "#name=%20", "#name=a%00b", `#name=${"x".repeat(WORKSPACE_NAME_MAX_LENGTH + 1)}`]) assert.equal(refusalOf(spiedLocation("/workspace", "?engine=unified-dev", hash), hash).code, "name_unbounded", `unbounded ${hash}`);
  const padded = refusalOf({ pathname: "/workspace", search: "?engine=unified-dev", hash: "#name=%20ops%20" }, "padded fragment");
  assert.equal(padded.code, "name_unbounded", "a padded fragment name is refused, never rewritten");
  assert.equal(boundedWorkspaceName(null), undefined);
}

// (e) TODO W1b — FW-R (e): a /terminal document with fragment `engine` still
// self-heals exactly as today, and a /workspace document never reaches that
// self-heal (window.location unchanged, no replace()). Both live in app.ts's
// boot() ordering, which W1b owns; this stub names the case so the browser
// gate that W1b adds has its slot. Nothing here asserts on app.ts.
{
  process.stdout.write("workspace_url.test: TODO(W1b) FW-R (e) /terminal self-heal ordering guard — app.ts boot() branch order, not testable from workspace_url.ts\n");
}

// Every refusal code has reviewed full-page copy.
{
  const refusals: WorkspaceRouteRefusal[] = [
    { code: "wrong_path", pathname: "/x" }, { code: "query_missing" }, { code: "query_mismatch", search: "?x" }, { code: "fragment_engine_rejected" },
    { code: "fragment_unknown_key", key: "k" }, { code: "fragment_duplicate_key", key: "name" }, { code: "name_missing" }, { code: "name_unbounded" },
  ];
  for (const refusal of refusals) {
    const notice = workspaceRouteNotice(refusal);
    assert.ok(notice.headline.length > 0 && notice.detail.length > 0 && notice.code === refusal.code, `notice for ${refusal.code}`);
  }
}

process.stdout.write("workspace_url.test: PASS\n");
