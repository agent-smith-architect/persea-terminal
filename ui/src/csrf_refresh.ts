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

export async function refreshCSRFToken(
  signal: AbortSignal,
  fetcher: CSRFFetch = (input, init) => window.fetch(input, init),
  cookieReader: () => string = () => document.cookie,
): Promise<string> {
  const response = await fetcher("/api/inventory", {
    cache: "no-store",
    credentials: "same-origin",
    signal,
  });
  if (!response.ok) throw new Error("CSRF refresh is unavailable");
  return parseCSRFCookie(cookieReader());
}
