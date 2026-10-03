import { aliasFailureMessage, aliasRequest, parseAliasRecord, saveAlias } from "../src/alias_client";
import { parseInventory } from "../src/dashboard";
const assert = (value: unknown, message: string): void => { if (!value) throw new Error(message); };
const messages = {
  alias_invalid: "Use 1 to 128 characters, without control characters.",
  alias_in_use: "Another running session already uses this alias.",
  alias_exists: "This session already has an alias. The current one is shown.",
  alias_changed: "This alias was changed on another device. Reload to see it.",
  session_gone: "This session is no longer running.",
  alias_not_found: "This alias no longer exists.",
  alias_unavailable: "Aliases are temporarily unavailable.",
};
for (const [code, message] of Object.entries(messages)) assert(aliasFailureMessage(` ${code}\n`) === message, `Wrong wording for ${code}`);
assert(aliasFailureMessage("<script>server text</script>") === messages.alias_unavailable, "Raw server text leaked");
const a = { realm: "local", server: "private", uid: 1000, selector_kind: "socket_name", selector_value: "fixture", boot_id: "boot", server_pid: 42, server_start: 100, session_id: "$1", session_created: 200 };
const active = { alias_id: "id", display_alias: "Research", revision: 1, state: "active", realm: "local", server: "private", session_name: "he2", session_incarnation: a };
const detached = { ...active, alias_id: "old", display_alias: "Old", state: "detached", session_incarnation: {}, session_name: "he3" };
const session = { realm: "local", server: "private", server_status: "ok", session_id: "$1", name: "he2", authority: a, handles: { alias: "a", observe: "o", control: "c" }, width: 80, height: 24, attached: 0, activity: 0 };
const payload = (aliases: unknown[]) => ({ realms: [{ name: "local", servers: [{ label: "private", status: "ok", sessions: [session] }] }], aliases });
const parsed = parseInventory(payload([active, detached]));
assert(parsed.realms[0].servers[0].sessions[0].aliases[0].displayAlias === "Research", "Active alias did not project");
assert(parsed.detachedAliases[0].sessionName === "he3", "Detached alias did not parse");
const hidden = parseInventory(payload([{ ...detached, session_incarnation: a }]));
assert(hidden.realms[0].servers[0].sessions[0].aliases.length === 0 && hidden.detachedAliases.length === 1, "Detached alias leaked onto a live session");
for (const records of [[active, { ...active, alias_id: "second" }], [{ ...active, state: "tombstone" }], [{ ...active, server: "other" }], [{ ...active, revision: 0 }]]) {
  let failed = false; try { parseInventory(payload(records)); } catch { failed = true; } assert(failed, "Invalid alias inventory accepted");
}
const record = parseAliasRecord(active);
assert(aliasRequest(record, "unused").init.body === undefined, "DELETE still requires a handle");
assert(JSON.parse(String(aliasRequest(record, "unused", "New").init.body)).handle === undefined, "PATCH can rebind");
Object.assign(globalThis, { document: { cookie: `__Host-persea-terminal-csrf=${"A".repeat(43)}` } });
async function main(): Promise<void> {
  for (const code of Object.keys(messages)) {
    const result = await saveAlias(undefined, "handle", "Work", async (_url, init) => {
      assert((init?.headers as Record<string, string>)["X-Persea-CSRF"] === "A".repeat(43), "Missing CSRF header");
      return new Response(`${code}\n`, { status: 409, headers: { "X-Persea-Alias-Record": JSON.stringify(active) } });
    });
    assert(!result.ok && result.message === messages[code as keyof typeof messages] && result.current?.displayAlias === "Research", "Refusal did not parse code and winning record");
  }
  console.log("alias client and v2 inventory tests passed");
}
void main();
