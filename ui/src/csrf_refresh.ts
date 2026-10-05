import { boundedFetch } from "./bounded_fetch";

export type CSRFFetch = (
  input: string,
  init: RequestInit,
) => Promise<Readonly<{ ok: boolean }>>;

export function parseCSRFCookie(cookie: string): string {
  const values = cookie.split(";").map((value) => value.trim()).filter((value) => value.startsWith("__Host-persea-terminal-csrf="));
  if (values.length !== 1) throw new Error("CSRF cookie unavailable");
  const token = values[0].slice(values[0].indexOf("=") + 1);
  if (!/^[A-Za-z0-9_-]{43}$/.test(token)) throw new Error("CSRF cookie malformed");
  return token;
}

export function csrfToken(): string {
  return parseCSRFCookie(document.cookie);
}

// The token for a request about to be sent. The cookie is used while the
// browser still holds it (it lives ten minutes); otherwise one small request
// has the server mint a new one. Nothing else is read for it.
export async function refreshCSRFToken(
  signal: AbortSignal,
  fetcher: CSRFFetch = boundedFetch,
  cookieReader: () => string = () => document.cookie,
): Promise<string> {
  try { return parseCSRFCookie(cookieReader()); } catch { /* absent or expired: mint one */ }
  const response = await fetcher("/api/csrf", {
    cache: "no-store",
    credentials: "same-origin",
    signal,
  });
  if (!response.ok) throw new Error("CSRF refresh is unavailable");
  return parseCSRFCookie(cookieReader());
}
