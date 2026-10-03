export type DashboardAlias = Readonly<{
  aliasId: string; displayAlias: string; revision: number; state: string;
  realm?: string; server?: string; sessionName?: string;
}>;

const messages = {
  alias_invalid: "Use 1 to 128 characters, without control characters.",
  alias_in_use: "Another running session already uses this alias.",
  alias_exists: "This session already has an alias. The current one is shown.",
  alias_changed: "This alias was changed on another device. Reload to see it.",
  session_gone: "This session is no longer running.",
  alias_not_found: "This alias no longer exists.",
  alias_unavailable: "Aliases are temporarily unavailable.",
} as const;
export type AliasErrorCode = keyof typeof messages;
export function aliasErrorCode(code: string): AliasErrorCode {
  const trimmed = code.trim();
  return Object.hasOwn(messages, trimmed) ? trimmed as AliasErrorCode : "alias_unavailable";
}
export function aliasFailureMessage(code: string): string { return messages[aliasErrorCode(code)]; }

export function csrfToken(cookie = document.cookie): string {
  const values = cookie.split(";").map(v => v.trim()).filter(v => v.startsWith("__Host-persea-terminal-csrf="));
  if (values.length !== 1) throw new Error("CSRF cookie unavailable");
  const token = values[0].slice(values[0].indexOf("=") + 1);
  if (!/^[A-Za-z0-9_-]{43}$/.test(token)) throw new Error("CSRF cookie malformed");
  return token;
}

export function parseAliasRecord(value: unknown): DashboardAlias {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Invalid alias record");
  const v = value as Record<string, unknown>;
  const text = (key: string): string => {
    if (typeof v[key] !== "string" || !v[key]) throw new Error(`Invalid alias ${key}`);
    return v[key] as string;
  };
  if (!Number.isSafeInteger(v.revision) || (v.revision as number) < 1 || (v.state !== "active" && v.state !== "detached")) throw new Error("Invalid alias revision or state");
  return { aliasId: text("alias_id"), displayAlias: text("display_alias"), revision: v.revision as number, state: v.state,
    realm: text("realm"), server: text("server"), sessionName: text("session_name") };
}

export function aliasRequest(alias: DashboardAlias | undefined, handle: string, displayAlias?: string): { url: string; init: RequestInit } {
  if (!alias) return { url: "/api/aliases", init: { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ display_alias: displayAlias ?? "", handle }) } };
  const url = `/api/aliases/${encodeURIComponent(alias.aliasId)}`;
  const headers = { "Content-Type": "application/json", "If-Match": `"${alias.revision}"` };
  return displayAlias === undefined ? { url, init: { method: "DELETE", headers } }
    : { url, init: { method: "PATCH", headers, body: JSON.stringify({ display_alias: displayAlias }) } };
}

type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
export type AliasOutcome = Readonly<{ ok: true; alias?: DashboardAlias }>
  | Readonly<{ ok: false; code: AliasErrorCode; message: string; current?: DashboardAlias }>;

export async function saveAlias(alias: DashboardAlias | undefined, handle: string, displayAlias?: string, fetcher: FetchLike = fetch): Promise<AliasOutcome> {
  try {
    const request = aliasRequest(alias, handle, displayAlias);
    const response = await fetcher(request.url, { ...request.init,
      headers: { ...(request.init.headers as Record<string, string>), "X-Persea-CSRF": csrfToken() }, cache: "no-store", credentials: "same-origin" });
    if (response.ok) return response.status === 204 ? { ok: true } : { ok: true, alias: parseAliasRecord(await response.json()) };
    const code = aliasErrorCode(await response.text());
    let current: DashboardAlias | undefined;
    try { const record = response.headers.get("X-Persea-Alias-Record"); if (record) current = parseAliasRecord(JSON.parse(record)); } catch { /* A malformed projection grants no editor state. */ }
    return { ok: false, code, message: aliasFailureMessage(code), ...(current ? { current } : {}) };
  } catch { return { ok: false, code: "alias_unavailable", message: aliasFailureMessage("alias_unavailable") }; }
}
