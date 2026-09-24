import { sessionSwitcherRows, switcherInventory } from "../src/session_switcher";
import { COLLAPSE_STORAGE_KEY, HISTORY_CHOICES, adoptFailureMessage, brokerInternalSessionName, defaultCardLabel, defaultUnavailableMessage, landingAmbiguousMessage, landingBlockedMessage, landingEndedMessage, resumeCardDetail, resumeCardLabel, aliasRequest, assignText, collapseGroupKey, createFailureMessage, effectiveCollapse, formatPreviewMeta, mutationStatus, parseHistoryChoice, parseInventory, parsePreview, previewFailureMessage, previewRequestPath, readCollapsedGroups, RefreshGate, sessionMatchesFilter, terminalURL, unifiedBlockedMessage, unifiedTerminalURL, writeCollapsedGroups } from "../src/dashboard";
import { WorkspaceAPI, WorkspaceAPIError, parseWorkspaceList } from "../src/workspace_api";
import { leaf } from "../src/workspace_model";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  deepEqual(actual: unknown, expected: unknown, message = "values differ"): void { if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`); },
  throws(fn: () => unknown): void { try { fn(); } catch { return; } throw new Error("expected function to throw"); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
};

function authority(realm: string, server: string, uid: number, id: string): Record<string, unknown> {
  return { realm, server, uid, selector_kind: "socket_path", selector_value: `/tmp/${server}.sock`, boot_id: `boot-${realm}`, server_pid: 42, server_start: 100, session_id: id, session_created: 200 };
}
function session(realm: string, server: string, uid: number, name: string, id: string, handle: string): Record<string, unknown> {
  return { handles: { alias: handle+"a", observe: handle+"o", control: handle+"c" }, realm, server, server_status: "ok", session_id: id, name, width: 120, height: 40, attached: 2, activity: 1_700_000_000, authority: authority(realm, server, uid, id) };
}

const a = authority("local", "private", 1000, "$1");
const inventory = parseInventory({
  realms: [
    { name: "local", display_name: "fixture-user-k7m2", servers: [{ label: "private", status: "ok", sessions: [{ ...session("local", "private", 1000, "actual-name", "$1", "opaque +&?"), alias: "Friendly", alias_state: "bound" }] }] },
    { name: "remote", servers: [{ label: "other", status: "ok", sessions: [session("remote", "other", 2000, "remote-name", "$8", "h2")] }] },
    { name: "failed", error: "broker unavailable", servers: [] },
  ],
  aliases: [
    { alias_id: "alias/1", display_alias: "Friendly", normalized_alias: "friendly", session_incarnation: a, revision: 7, created_at: "ignored", updated_at: "ignored", state: "bound" },
    { alias_id: "alias/2", display_alias: "Second", normalized_alias: "second", session_incarnation: { ...a }, revision: 11, created_at: "ignored", updated_at: "ignored", state: "active" },
    { alias_id: "old", display_alias: "Same name", normalized_alias: "same name", session_incarnation: authority("local", "private", 1000, "$old"), revision: 3, created_at: "ignored", updated_at: "ignored", state: "tombstone" },
  ],
});
assert.equal(inventory.realms.length, 3);
assert.equal(inventory.realms[0].uid, 1000);
assert.equal(inventory.realms[0].displayName, "fixture-user-k7m2");
assert.equal(inventory.realms[1].displayName, "remote", "missing display_name did not fall back to stable realm ID");
assert.equal(inventory.realms[1].uid, 2000);
assert.equal(inventory.realms[2].error, "broker unavailable");
const projected = inventory.realms[0].servers[0].sessions[0];
assert.deepEqual({ name: projected.name, id: projected.sessionId, uid: projected.uid, aliases: projected.aliases.map((item) => [item.aliasId, item.displayAlias, item.state, item.revision]) }, { name: "actual-name", id: "$1", uid: 1000, aliases: [["alias/1", "Friendly", "bound", 7], ["alias/2", "Second", "active", 11]] });
assert.equal("authority" in projected, false, "authority tuple must not escape parser projection");
assert.equal(projected.draftScope, JSON.stringify(["local", "private", "socket_path", "/tmp/private.sock", "boot-local", "$1", 1000, 42, 100, 200]), "draft scope must be the exact dashboard authority identity");
assert.deepEqual(inventory.detachedAliases.map((item) => [item.displayAlias, item.state]), [["Same name", "tombstone"]]);
const firstLiveRequest = aliasRequest(projected.aliases[0], projected.handles.alias, "Friendly renamed");
const secondLiveRequest = aliasRequest(projected.aliases[1], projected.handles.alias);
assert.equal(firstLiveRequest.url, "/api/aliases/alias%2F1"); assert.equal((firstLiveRequest.init.headers as Record<string, string>)["If-Match"], '"7"');
assert.equal(secondLiveRequest.url, "/api/aliases/alias%2F2"); assert.equal(secondLiveRequest.init.method, "DELETE"); assert.equal((secondLiveRequest.init.headers as Record<string, string>)["If-Match"], '"11"');

for (const malformed of [null, {}, { realms: [], aliases: null }, { realms: [{ name: "x", servers: "bad" }], aliases: [] }, { realms: [{ name: "x", servers: [{ label: "s", status: "ok", sessions: [{ ...session("x", "s", 1, "n", "$1", ""), handles: { alias: "", observe: "o", control: "c" } }] }] }], aliases: [] }]) {
  assert.throws(() => parseInventory(malformed));
}

// the broker's internal attachment wrapper is not an operator
// session. While an attachment is live the broker owns one extra tmux session
// named `persea-attach-<32 hex>`; the smoke of release 1e8f171 saw it listed
// as a normal selectable row in the in-terminal switcher and counted in the
// dashboard list. Every operator-visible list is derived from parseInventory,
// so the rule is pinned there — and the near misses prove it is the exact
// documented shape, not a loose "starts with" match that could hide a real
// operator session.
{
  const wrapperName = "persea-attach-ef231fe23ba62a2c8c1f731746c9667f";
  const wrapperAuthority = authority("local", "private", 1000, "$99");
  const withWrapper = parseInventory({
    realms: [{
      name: "local", display_name: "Local", servers: [{
        label: "private", status: "ok", can_create: true, sessions: [
          { ...session("local", "private", 1000, "operator-one", "$1", "h1"), unified: { state: "open", origin: "birth" } },
          { ...session("local", "private", 1000, wrapperName, "$99", "h99"), unified: { state: "open", origin: "birth" } },
          session("local", "private", 1000, "persea-attach-ef231fe23ba62a2c8c1f731746c9667", "$2", "h2"),
          session("local", "private", 1000, "persea-attach-ef231fe23ba62a2c8c1f731746c9667ff", "$3", "h3"),
          session("local", "private", 1000, "persea-attach-", "$4", "h4"),
          session("local", "private", 1000, "persea-attach-zf231fe23ba62a2c8c1f731746c9667f", "$5", "h5"),
          session("local", "private", 1000, "not-persea-attach-ef231fe23ba62a2c8c1f731746c9667f", "$6", "h6"),
        ],
      }],
    }],
    aliases: [
      { alias_id: "alias/wrapper", display_alias: "Bound to a wrapper", normalized_alias: "bound to a wrapper", session_incarnation: wrapperAuthority, revision: 2, created_at: "ignored", updated_at: "ignored", state: "bound" },
    ],
  });
  const listed = withWrapper.realms[0].servers[0].sessions;
  assert.deepEqual(listed.map((item) => item.name), [
    "operator-one",
    "persea-attach-ef231fe23ba62a2c8c1f731746c9667",
    "persea-attach-ef231fe23ba62a2c8c1f731746c9667ff",
    "persea-attach-",
    "persea-attach-zf231fe23ba62a2c8c1f731746c9667f",
    "not-persea-attach-ef231fe23ba62a2c8c1f731746c9667f",
  ], "only the exact broker wrapper shape may be dropped from the inventory");
  const counted = withWrapper.realms.reduce((sum, realm) => sum + realm.servers.reduce((n, server) => n + server.sessions.length, 0), 0);
  assert.equal(counted, 6, "the dashboard session count must not include the broker wrapper");
  const rows = sessionSwitcherRows(switcherInventory(withWrapper), null, "", () => "blocked");
  assert.equal(rows.some((row) => row.session.name === wrapperName), false, "the switcher must not render the broker wrapper");
  assert.equal(rows.filter((row) => row.selectable).some((row) => row.session.name === wrapperName), false, "the broker wrapper must not be selectable");
  assert.equal(rows.find((row) => row.selectable)?.session.name, "operator-one", "the first explicit choice must never be the broker wrapper");
  assert.deepEqual(withWrapper.detachedAliases.map((item) => item.aliasId), ["alias/wrapper"], "an alias bound to a dropped wrapper must stay visible as detached, not vanish with the row");
  assert.equal(brokerInternalSessionName(wrapperName), true);
  for (const near of ["persea-attach-ef231fe23ba62a2c8c1f731746c9667", "persea-attach-ef231fe23ba62a2c8c1f731746c9667ff", "persea-attach-", "persea-attach-zf231fe23ba62a2c8c1f731746c9667f", "not-persea-attach-ef231fe23ba62a2c8c1f731746c9667f", ""]) {
    assert.equal(brokerInternalSessionName(near), false, `near miss must not be treated as internal: ${near}`);
  }
}

const malicious = "<img src=x onerror=alert(1)>";
const parsedMalicious = parseInventory({ realms: [{ name: malicious, display_name: malicious, error: malicious, servers: [] }], aliases: [] });
assert.equal(parsedMalicious.realms[0].name, malicious);
assert.equal(parsedMalicious.realms[0].displayName, malicious);
assert.equal(parsedMalicious.realms[0].error, malicious);
const fakeNode: { textContent: string | null; innerHTML?: string } = { textContent: null };
assignText(fakeNode, malicious);
assert.equal(fakeNode.textContent, malicious);
assert.equal(fakeNode.innerHTML, undefined, "text rendering must not interpret HTML");

const url = new URL(terminalURL("https://terminal.example/dashboard?old=1", "opaque +&?", "observe"));
assert.equal(url.pathname, "/terminal"); const fragment=new URLSearchParams(url.hash.slice(1)); assert.equal(fragment.get("handle"), "opaque +&?"); assert.equal(fragment.get("mode"), "observe"); assert.equal(url.searchParams.has("realm"), false);
assert.equal(url.search, ""); assert.equal(fragment.get("history"), "1000");
const fragmentKeys: string[] = []; fragment.forEach((_value, key) => fragmentKeys.push(key));
assert.deepEqual(fragmentKeys.sort(), ["handle", "history", "mode"], "browser-supplied target authority escaped into terminal URL");
assert.deepEqual([...HISTORY_CHOICES], [0, 500, 1_000, 2_000, 5_000, 7_500, 10_000]);
for (const choice of HISTORY_CHOICES) assert.equal(parseHistoryChoice(String(choice)), choice);
for (const invalid of ["", "-1", "1", "0500", "500.0", " 500", "+500", "10001"]) assert.throws(() => parseHistoryChoice(invalid));
const tenThousandURL = new URL(terminalURL("https://terminal.example/", "h", "control", 10_000)); assert.equal(new URLSearchParams(tenThousandURL.hash.slice(1)).get("history"), "10000");
const scopedURL = new URL(terminalURL("https://terminal.example/", "h", "control", 1_000, { name: "same" }, projected.draftScope));
const scopedFragment = new URLSearchParams(scopedURL.hash.slice(1));
assert.equal(scopedFragment.get("draft_scope"), projected.draftScope, "exact draft scope did not survive terminalURL");
assert.equal(scopedFragment.get("name"), "same", "display label changed while carrying draft scope");
const post = aliasRequest(undefined, "fresh", "Desk"); assert.equal(post.url, "/api/aliases"); assert.equal(post.init.method, "POST"); assert.deepEqual(JSON.parse(String(post.init.body)), { display_alias: "Desk", handle: "fresh" });
const alias = { aliasId: "alias/1", displayAlias: "Desk", revision: 7, state: "bound" };
const patch = aliasRequest(alias, "unused", "New"); assert.equal(patch.url, "/api/aliases/alias%2F1"); assert.equal(patch.init.method, "PATCH"); assert.equal((patch.init.headers as Record<string, string>)["If-Match"], '"7"'); assert.deepEqual(JSON.parse(String(patch.init.body)), { display_alias: "New", handle: "unused" });
const del = aliasRequest(alias, "unused"); assert.equal(del.init.method, "DELETE"); assert.deepEqual(JSON.parse(String(del.init.body)), { handle: "unused" }); assert.equal((del.init.headers as Record<string, string>)["If-Match"], '"7"');
for (const status of [409, 410, 412, 503]) assert.ok(mutationStatus(status)); assert.equal(mutationStatus(400), undefined);
const gate = new RefreshGate(); assert.equal(gate.permitsBackgroundRefresh(), true); gate.beginEdit(); assert.equal(gate.permitsBackgroundRefresh(), false); gate.setMutating(true); gate.endEdit(); assert.equal(gate.permitsBackgroundRefresh(), false); gate.setMutating(false); assert.equal(gate.permitsBackgroundRefresh(), true);

{
  // Server-level unified_dev now carries only the CREATE affordance.
  const projected = parseInventory({ realms: [{ name: "r", servers: [{ label: "s", status: "ok", sessions: [], unified_dev: { state: "create", name: "unified-target" } }] }], aliases: [] });
  const launch = projected.realms[0].servers[0].unifiedDev;
  assert.equal(launch?.state, "create");
  assert.equal(launch?.name, "unified-target");

  for (const malformed of [
    { state: "open", name: "unified-target", session_id: "$17", control: "mintedc" },
    { state: "blocked_existing_unobserved", name: "unified-target", session_id: "$17" },
    { state: "late_attach", name: "unified-target" },
    { state: "create", name: "unified-target", control: "unexpected" },
  ]) {
    const parsed = parseInventory({ realms: [{ name: "r", servers: [{ label: "s", status: "ok", sessions: [session("r", "s", 1, "unified-target", "$17", "minted")], unified_dev: malformed }] }], aliases: [] });
    assert.equal(parsed.realms[0].servers[0].unifiedDev, undefined, "malformed unified projection did not fail closed");
  }
  const absent = parseInventory({ realms: [{ name: "r", servers: [{ label: "s", status: "ok", sessions: [] }] }], aliases: [] });
  assert.equal(absent.realms[0].servers[0].unifiedDev, undefined);
}

// The per-session unified projection: strict key sets per state, origin only
// on open and only from its closed set, unknown shapes fail closed to an
// absent affordance (the legacy actions remain).
{
  const parseRow = (unified: unknown) => parseInventory({
    realms: [{ name: "r", servers: [{ label: "s", status: "ok", sessions: [{ ...session("r", "s", 1, "n", "$17", "minted"), unified }] }] }],
    aliases: [],
  }).realms[0].servers[0].sessions[0].unified;
  assert.deepEqual(parseRow({ state: "open", origin: "birth" }), { state: "open", origin: "birth" });
  assert.deepEqual(parseRow({ state: "open", origin: "reconstructed" }), { state: "open", origin: "reconstructed" });
  assert.deepEqual(parseRow({ state: "open", origin: "birth", detail: "rotation_deferred_alt_screen" }), { state: "open", origin: "birth", detail: "rotation_deferred_alt_screen" });
  assert.deepEqual(parseRow({ state: "adoptable" }), { state: "adoptable" });
  for (const blocked of ["blocked_alt_screen", "blocked_multi_pane", "blocked_multi_window", "blocked_foreign_server", "slots_exhausted", "unavailable"]) {
    assert.deepEqual(parseRow({ state: blocked }), { state: blocked }, `blocked state ${blocked} did not parse`);
  }
  for (const malformed of [
    { state: "open" },
    { state: "open", origin: "cloned" },
    { state: "open", origin: "birth", control: "minted" },
    { state: "open", origin: "birth", detail: "rotation_deferred_unknown" },
    { state: "adoptable", origin: "birth" },
    { state: "late_attach" },
    { state: "blocked_alt_screen", detail: "why" },
    "adoptable",
    42,
    null,
  ]) {
    assert.equal(parseRow(malformed), undefined, `malformed per-session unified did not fail closed: ${JSON.stringify(malformed)}`);
  }
  assert.equal(parseRow(undefined), undefined, "absent unified field must stay absent");

  // The one-click open URL keeps the CSP invariant: url.search is EXACTLY
  // engine=unified-dev, authority rides the fragment.
  const opened = parseInventory({
    realms: [{ name: "r", servers: [{ label: "s", status: "ok", sessions: [{ ...session("r", "s", 1, "adoptee", "$17", "minted"), unified: { state: "open", origin: "reconstructed" } }] }] }],
    aliases: [],
  }).realms[0].servers[0].sessions[0];
  const unified = new URL(unifiedTerminalURL("https://terminal.example/", opened.handles.control, opened));
  assert.equal(unified.pathname, "/terminal");
  assert.equal(unified.search, "?engine=unified-dev");
  const unifiedFragment = new URLSearchParams(unified.hash.slice(1));
  assert.equal(unifiedFragment.get("engine"), "unified-dev");
  assert.equal(unifiedFragment.get("handle"), "mintedc");
  assert.equal(unifiedFragment.get("mode"), "control");
}

// Blocked-row wording and adoption refusal wording are closed sets; an
// unrecognised adoption code must never reach the page as raw text.
{
  for (const blocked of ["blocked_alt_screen", "blocked_multi_pane", "blocked_multi_window", "blocked_foreign_server", "slots_exhausted", "unavailable"] as const) {
    assert.ok(unifiedBlockedMessage(blocked).length > 0);
  }
  assert.equal(adoptFailureMessage("session_gone", 410), "This session just ended.");
  assert.equal(adoptFailureMessage("  slots_exhausted  ", 503), "No unified terminal slots remain for this run.");
  const injected = adoptFailureMessage("<img src=x onerror=alert(1)>", 400);
  assert.ok(!injected.includes("onerror"));
  assert.equal(injected, "The session could not be opened.");
  assert.equal(adoptFailureMessage("", 503), "The realm is unreachable.");
}

// Creation capability is reported by the realm's broker, which owns the policy.
// An absent field means "no", so an older broker offers no affordance rather than
// the UI guessing that creation might work.
{
  const withFlag = parseInventory({ realms: [{ name: "r", servers: [{ label: "s", status: "ok", can_create: true, sessions: [] }] }], aliases: [] });
  assert.equal(withFlag.realms[0].servers[0].canCreate, true);
  assert.equal(withFlag.realms[0].servers[0].realm, "r", "server must carry its realm for the create request");
  const withoutFlag = parseInventory({ realms: [{ name: "r", servers: [{ label: "s", status: "ok", sessions: [] }] }], aliases: [] });
  assert.equal(withoutFlag.realms[0].servers[0].canCreate, false);
  const falseFlag = parseInventory({ realms: [{ name: "r", servers: [{ label: "s", status: "ok", can_create: false, sessions: [] }] }], aliases: [] });
  assert.equal(falseFlag.realms[0].servers[0].canCreate, false);
  assert.throws(() => parseInventory({ realms: [{ name: "r", servers: [{ label: "s", status: "ok", can_create: "yes", sessions: [] }] }], aliases: [] }));
}

// Image staging needs BOTH consents: the front door's endpoint gate
// (top-level image_upload) AND the realm broker's own can_stage_images. The
// session carries the conjunction, and the unified URL gains the
// display/UX-only image_realm fragment field exactly when it holds.
{
  const stagingRow = (root: Record<string, unknown>, server: Record<string, unknown>) => parseInventory({
    realms: [{ name: "r", servers: [{ label: "s", status: "ok", sessions: [{ ...session("r", "s", 1, "n", "$1", "minted"), unified: { state: "open", origin: "birth" } }], ...server }] }],
    aliases: [],
    ...root,
  }).realms[0].servers[0].sessions[0];
  const advertised = stagingRow({ image_upload: true }, { can_stage_images: true });
  assert.equal(advertised.canStageImages, true, "both consents did not surface on the session");
  assert.equal(stagingRow({ image_upload: true }, {}).canStageImages, undefined, "missing broker advice enabled staging");
  assert.equal(stagingRow({}, { can_stage_images: true }).canStageImages, undefined, "missing front gate enabled staging");
  assert.equal(stagingRow({ image_upload: false }, { can_stage_images: true }).canStageImages, undefined, "disabled front gate enabled staging");
  assert.throws(() => stagingRow({ image_upload: "yes" }, { can_stage_images: true }));
  assert.throws(() => stagingRow({ image_upload: true }, { can_stage_images: "yes" }));
  const withCapability = new URLSearchParams(new URL(unifiedTerminalURL("https://terminal.example/", advertised.handles.control, advertised)).hash.slice(1));
  assert.equal(withCapability.get("image_realm"), "r", "capability did not travel as image_realm");
  const bare = stagingRow({}, {});
  const withoutCapability = new URLSearchParams(new URL(unifiedTerminalURL("https://terminal.example/", bare.handles.control, bare)).hash.slice(1));
  assert.equal(withoutCapability.get("image_realm"), null, "image_realm appeared without the capability");
}

// Refusal wording is chosen here from a closed set of codes. An unrecognised code
// must never be rendered raw, or a future server code could put unreviewed text
// on the operator's page.
{
  assert.equal(createFailureMessage("name_taken", 409), "A session with that name already exists.");
  assert.equal(createFailureMessage("invalid_name", 400), "That name is not allowed for this realm.");
  assert.equal(createFailureMessage("at_capacity", 409), "This realm is already at its session limit.");
  assert.equal(createFailureMessage("not_permitted", 403), "This realm does not allow creating sessions here.");
  assert.equal(createFailureMessage("server_unavailable", 503), "The tmux server is unavailable.");
  assert.equal(createFailureMessage("  name_taken  ", 409), "A session with that name already exists.", "surrounding whitespace must not defeat the mapping");
  const injected = createFailureMessage("<img src=x onerror=alert(1)>", 400);
  assert.ok(!injected.includes("onerror"));
  assert.equal(injected, "The session could not be created.");
  assert.equal(createFailureMessage("", 503), "The realm is unreachable.");
}

// The dashboard filter matches the tmux session name and EVERY display alias,
// case-insensitively, and an empty (or whitespace-only) query matches everything
// so clearing the filter restores the full view.
{
  const aliased = { name: "Build-Server", aliases: [
    { aliasId: "a", displayAlias: "Deploy Lane", revision: 1, state: "bound" },
    { aliasId: "b", displayAlias: "Second Name", revision: 2, state: "active" },
  ] };
  assert.equal(sessionMatchesFilter(aliased, "build"), true, "name substring must match case-insensitively");
  assert.equal(sessionMatchesFilter(aliased, "SERVER"), true, "query case must not matter");
  assert.equal(sessionMatchesFilter(aliased, "deploy"), true, "first alias must participate in matching");
  assert.equal(sessionMatchesFilter(aliased, "second"), true, "every alias must participate in matching");
  assert.equal(sessionMatchesFilter(aliased, "absent"), false);
  assert.equal(sessionMatchesFilter(aliased, ""), true, "empty query must restore the full view");
  assert.equal(sessionMatchesFilter(aliased, "   "), true, "whitespace-only query must count as empty");
  assert.equal(sessionMatchesFilter(aliased, "  lane "), true, "query must be trimmed before matching");
  assert.equal(sessionMatchesFilter({ name: "plain", aliases: [] }, "alias"), false);
}

// Collapse state round-trips through storage; malformed or unreadable records
// fall back to the default (everything expanded) instead of failing the page.
{
  const backing = new Map<string, string>();
  const storage = { getItem: (key: string): string | null => backing.get(key) ?? null, setItem: (key: string, value: string): void => { backing.set(key, value); } };
  assert.deepEqual([...readCollapsedGroups(storage)], []);
  const key = collapseGroupKey("local", "private");
  writeCollapsedGroups(storage, new Set([key]));
  assert.deepEqual([...readCollapsedGroups(storage)], [key], "collapse state did not survive a storage round-trip");
  writeCollapsedGroups(storage, new Set());
  assert.deepEqual([...readCollapsedGroups(storage)], []);
  assert.equal(collapseGroupKey("a", "b·c") === collapseGroupKey("a·b", "c"), false, "group key must not collide across the realm/server boundary");
  for (const malformed of ["not json", "{}", "[3]", "\"x\"", JSON.stringify(["ok", 1])]) {
    backing.set(COLLAPSE_STORAGE_KEY, malformed);
    assert.deepEqual([...readCollapsedGroups(storage)], [], `malformed record did not fall back to expanded: ${malformed}`);
  }
  const denied = { getItem: (): string | null => { throw new Error("denied"); }, setItem: (): void => { throw new Error("denied"); } };
  assert.deepEqual([...readCollapsedGroups(denied)], [], "unreadable storage did not fall back to expanded");
  writeCollapsedGroups(denied, new Set(["x"]));
}

// While a filter is active a persisted collapse is overridden so matches stay
// visible; when the filter clears the persisted collapse applies again.
{
  assert.equal(effectiveCollapse(true, true), false, "active filter must reveal collapsed groups");
  assert.equal(effectiveCollapse(true, false), true, "cleared filter must restore the persisted collapse");
  assert.equal(effectiveCollapse(false, true), false);
  assert.equal(effectiveCollapse(false, false), false);
}

// The on-demand pane preview: strict parsing (a wrong shape throws and renders
// as an error state, never as partially-trusted content), identity-only
// request path, and closed-set failure wording.
{
  const preview = parsePreview({ rows: ["alpha", "", "  spaced tail  ", "<img src=x onerror=alert(1)>"], width: 120, height: 40, captured_at: 1_700_000_000_123, truncated: false });
  assert.deepEqual([...preview.rows], ["alpha", "", "  spaced tail  ", "<img src=x onerror=alert(1)>"], "rows must survive verbatim, empty and HTML-looking rows included");
  assert.equal(preview.width, 120);
  assert.equal(preview.height, 40);
  assert.equal(preview.capturedAt, 1_700_000_000_123);
  assert.equal(preview.truncated, false);
  assert.equal(parsePreview({ rows: [], width: 1, height: 1, captured_at: 1 }).truncated, false, "absent truncated must mean false, like can_create");
  assert.equal(parsePreview({ rows: [], width: 1, height: 1, captured_at: 1, truncated: true }).truncated, true);
  for (const malformed of [
    null,
    {},
    { rows: "text", width: 1, height: 1, captured_at: 1 },
    { rows: [1], width: 1, height: 1, captured_at: 1 },
    { rows: [null], width: 1, height: 1, captured_at: 1 },
    { rows: [], width: 0, height: 1, captured_at: 1 },
    { rows: [], width: "80", height: 1, captured_at: 1 },
    { rows: [], width: 1, captured_at: 1 },
    { rows: [], width: 1, height: 1, captured_at: 0 },
    { rows: [], width: 1, height: 1, captured_at: 1.5 },
    { rows: [], width: 1, height: 1, captured_at: 1, truncated: "no" },
  ]) {
    assert.throws(() => parsePreview(malformed));
  }

  assert.equal(previewRequestPath({ realm: "lo cal", server: "pri&vate", sessionId: "$1" }),
    "/api/session-previews?realm=lo+cal&server=pri%26vate&session_id=%241",
    "preview request must carry exactly the encoded identity triple");

  assert.equal(previewFailureMessage("session_gone", 410), "This session just ended.");
  assert.equal(previewFailureMessage("  server_unavailable  ", 503), "The tmux server is unavailable.");
  assert.equal(previewFailureMessage("", 503), "The realm is unreachable.");
  assert.equal(previewFailureMessage("", 429), "Previews are refreshing too fast. Try again in a moment.");
  const injectedPreview = previewFailureMessage("<img src=x onerror=alert(1)>", 400);
  assert.ok(!injectedPreview.includes("onerror"));
  assert.equal(injectedPreview, "The preview could not be loaded.");

  const meta = formatPreviewMeta({ capturedAt: 1_700_000_000_123, width: 120, height: 40, truncated: false });
  assert.equal(meta.startsWith("120×40 · captured "), true, `meta must lead with pane geometry: ${meta}`);
  assert.ok(!meta.includes("truncated"));
  const truncatedMeta = formatPreviewMeta({ capturedAt: 1_700_000_000_123, width: 80, height: 24, truncated: true });
  assert.equal(truncatedMeta.endsWith(" · truncated"), true, `bounded capture must surface truncation: ${truncatedMeta}`);
}

// --- session memory landing copy. Every state names what is true and nothing else: no
// message promises a session, and the default never speaks as a resume.
{
  assert.equal(resumeCardLabel({ name: "ops", server: "private" }), "Resume ops · private");
  assert.equal(resumeCardDetail({ realm: "local", server: "private" }), "local · private");
  assert.equal(resumeCardDetail({ realm: "local", server: "private" }, "Primary"), "local · private · Primary");
  assert.equal(landingEndedMessage({ name: "ops", server: "private" }), "Your last session ops · private has ended.");
  assert.ok(landingAmbiguousMessage({ name: "ops" }).includes("ops"));
  assert.equal(landingBlockedMessage({ name: "ops", server: "private" }, "blocked_alt_screen"),
    `Your last session ops · private: ${unifiedBlockedMessage("blocked_alt_screen")}`,
    "a blocked resume must reuse the reviewed blocked wording");
  const preference = { realm: "local", server: "private", name: "ops" } as const;
  assert.equal(defaultCardLabel(preference), "Open default session ops · private");
  assert.ok(defaultCardLabel(preference).toLowerCase().includes("default"));
  assert.ok(!defaultCardLabel(preference).toLowerCase().includes("resume"));
  assert.equal(defaultUnavailableMessage({ kind: "missing", preference }), "The default session ops · private is not running.");
  assert.ok(defaultUnavailableMessage({ kind: "ambiguous", preference }).includes("More than one"));
  assert.equal(defaultUnavailableMessage({ kind: "none" }), "");
}

async function workspaceAPIContract(): Promise<void> {
  Object.defineProperty(globalThis, "document", { configurable: true, value: { cookie: `__Host-persea-terminal-csrf=${"a".repeat(43)}` } });
  const wire = { workspace_id: "a".repeat(32), name: "Ops", normalized_name: "ops", revision: 3, created_at: "2026-08-28T00:00:00Z", updated_at: "2026-08-28T00:00:01Z", tree: { kind: "leaf", session: { realm: "r", server: "s", name: "n" }, on_missing: "offer" } };
  assert.deepEqual(parseWorkspaceList({ version: 1, items: [wire] }).items[0].tree, leaf({ realm: "r", server: "s", name: "n" }));
  assert.equal(parseWorkspaceList({ version: 1, items: [{ ...wire, name: "İ", normalized_name: "i" }] }).items[0].normalizedName, "i", "normalized name is server-authoritative");
  for (const malformed of [
    { version: 1, items: [{ ...wire, handle: "authority" }] },
    { version: 2, items: [wire] },
    { version: 1, items: Array.from({ length: 65 }, () => wire) },
  ]) assert.throws(() => parseWorkspaceList(malformed));

  const calls: { input: string; init: RequestInit }[] = [];
  const api = new WorkspaceAPI(async (input, init = {}) => {
    calls.push({ input: String(input), init });
    if (init.method === "DELETE") return new Response(null, { status: 204 });
    return new Response(JSON.stringify({ ...wire, revision: 4 }), { status: 200, headers: { "Content-Type": "application/json" } });
  });
  await api.update({ workspaceId: wire.workspace_id, revision: 3 }, "Ops", leaf({ realm: "r", server: "s", name: "n" }));
  await api.delete({ workspaceId: wire.workspace_id, revision: 4 });
  assert.equal((calls[0].init.headers as Record<string, string>)["If-Match"], '"3"', "update must carry starting revision");
  assert.equal((calls[1].init.headers as Record<string, string>)["If-Match"], '"4"', "delete must carry starting revision");
  assert.ok(String(calls[0].init.body).includes('"tree"'), "update omitted the validated tree");
  assert.ok(!String(calls[0].init.body).includes("handle"), "workspace mutation carried authority");

  const conflict = new WorkspaceAPI(async () => new Response(JSON.stringify(wire), { status: 409, headers: { "Content-Type": "application/json" } }));
  let refusal: unknown;
  try { await conflict.update({ workspaceId: wire.workspace_id, revision: 2 }, "Ops", leaf({ realm: "r", server: "s", name: "n" })); } catch (error) { refusal = error; }
  assert.ok(refusal instanceof WorkspaceAPIError && refusal.code === "conflict" && refusal.current?.revision === 3, "409 did not preserve the server record without retry");
}

void workspaceAPIContract().then(() => console.log("dashboard tests PASS"), (error) => queueMicrotask(() => { throw error; }));
