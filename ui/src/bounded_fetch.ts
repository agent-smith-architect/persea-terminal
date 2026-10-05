// Every HTTP exchange the page starts ends within a deadline that covers the
// response headers AND the whole body. On a link that stops passing data
// without closing (a Wi-Fi network with no internet behind it, a phone between
// cells) a plain fetch can stay pending for minutes, and so does everything
// that waits on it: a disabled Refresh button, a terminal waiting for its
// preferences, a shared session-list read that later callers join.
export const REQUEST_DEADLINE_MS = 10_000;

export class RequestTimeoutError extends Error {
  constructor() {
    super("request timed out");
    this.name = "RequestTimeoutError";
  }
}

export type Fetch = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

// A fetch that settles within deadlineMs. The caller's own signal still
// cancels it. The body is read to the end inside the deadline; the returned
// response keeps its copy (Response.clone tees the stream), so callers read it
// as usual and that read can no longer stall.
export function boundedFetchWithin(deadlineMs: number, fetcher: Fetch = (input, init) => globalThis.fetch(input, init)): Fetch {
  return async (input, init = {}) => {
    const controller = new AbortController();
    const parent = init.signal ?? undefined;
    let timedOut = false;
    const timer = setTimeout(() => { timedOut = true; controller.abort(); }, deadlineMs);
    const relay = () => controller.abort(parent?.reason);
    if (parent?.aborted) relay(); else parent?.addEventListener("abort", relay, { once: true });
    try {
      const response = await fetcher(input, { ...init, signal: controller.signal });
      await response.clone().arrayBuffer();
      return response;
    } catch (error) {
      throw timedOut ? new RequestTimeoutError() : error;
    } finally {
      clearTimeout(timer);
      parent?.removeEventListener("abort", relay);
    }
  };
}

export const boundedFetch: Fetch = boundedFetchWithin(REQUEST_DEADLINE_MS);

// A request that carries a file (an image upload or download) moves more
// bytes than a small JSON exchange; on a slow link it needs a larger budget.
export const TRANSFER_DEADLINE_MS = 120_000;
export const boundedTransfer: Fetch = boundedFetchWithin(TRANSFER_DEADLINE_MS);
