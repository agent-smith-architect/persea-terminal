import { boundedFetch } from "./bounded_fetch";

export type CSRFFetch = (
  input: string,
  init: RequestInit,
) => Promise<Readonly<{ ok: boolean }>>;

type InFlightCSRF = {
  readonly controller: AbortController;
  readonly promise: Promise<string>;
  waiters: number;
};

const inFlightByFetcher = new WeakMap<CSRFFetch, InFlightCSRF>();

function abortError(signal?: AbortSignal): unknown {
  if (signal?.reason !== undefined) return signal.reason;
  return new DOMException("The operation was aborted.", "AbortError");
}

function defaultCookieReader(): string {
  return document.cookie;
}

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

// Synchronous fast path when the browser already holds a valid cookie; falls
// back to the single-flight /api/csrf refresh only when the cookie is absent
// or expired. Avoids adding an extra microtask on already-fresh mutations.
export function csrfTokenOrRefresh(
  signal?: AbortSignal,
  fetcher: CSRFFetch = boundedFetch,
  cookieReader: () => string = defaultCookieReader,
): string | Promise<string> {
  try { return parseCSRFCookie(cookieReader()); } catch { /* absent or expired */ }
  return refreshCSRFToken(signal, fetcher, cookieReader);
}

// The token for a request about to be sent. The cookie is used while the
// browser still holds it (it lives ten minutes); otherwise one small single-
// flight GET has the server mint a new one. Concurrent callers share that
// flight so waking panes do not race or overwrite each other's cookie; an
// individual caller's abort detaches only that caller unless every waiter
// aborts.
export async function refreshCSRFToken(
  signal?: AbortSignal,
  fetcher: CSRFFetch = boundedFetch,
  cookieReader: () => string = defaultCookieReader,
): Promise<string> {
  try { return parseCSRFCookie(cookieReader()); } catch { /* absent or expired: mint one */ }
  if (signal?.aborted) throw abortError(signal);

  let entry = inFlightByFetcher.get(fetcher);
  if (!entry) {
    const controller = new AbortController();
    const created: InFlightCSRF = {
      controller,
      waiters: 0,
      promise: (async () => {
        const response = await fetcher("/api/csrf", {
          cache: "no-store",
          credentials: "same-origin",
          signal: controller.signal,
        });
        if (!response.ok) throw new Error("CSRF refresh is unavailable");
        return parseCSRFCookie(cookieReader());
      })().finally(() => {
        if (inFlightByFetcher.get(fetcher) === created) inFlightByFetcher.delete(fetcher);
      }),
    };
    // Prevent an unhandled rejection if all waiters abort before settlement.
    void created.promise.catch(() => undefined);
    inFlightByFetcher.set(fetcher, created);
    entry = created;
  }

  entry.waiters += 1;
  if (!signal) {
    try {
      return await entry.promise;
    } finally {
      entry.waiters -= 1;
    }
  }

  const shared = entry;
  return new Promise<string>((resolve, reject) => {
    let settled = false;
    const onAbort = (): void => {
      if (settled) return;
      settled = true;
      shared.waiters -= 1;
      if (shared.waiters <= 0 && !shared.controller.signal.aborted) {
        if (inFlightByFetcher.get(fetcher) === shared) inFlightByFetcher.delete(fetcher);
        shared.controller.abort(signal.reason);
      }
      reject(abortError(signal));
    };
    signal.addEventListener("abort", onAbort, { once: true });
    shared.promise.then(
      (token) => {
        if (settled) return;
        settled = true;
        signal.removeEventListener("abort", onAbort);
        shared.waiters -= 1;
        resolve(token);
      },
      (error: unknown) => {
        if (settled) return;
        settled = true;
        signal.removeEventListener("abort", onAbort);
        shared.waiters -= 1;
        reject(error);
      },
    );
  });
}
