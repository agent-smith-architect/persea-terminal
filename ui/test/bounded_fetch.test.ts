import { RequestTimeoutError, boundedFetchWithin, type Fetch } from "../src/bounded_fetch";

const assert = (value: unknown, message: string): void => { if (!value) throw new Error(message); };
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// A server whose reply is chosen by the test. Like a real fetch, it fails the
// pending headers or body when its signal aborts.
function server(reply: "no-headers" | "stalled-body" | "complete" | "no-content") {
  const seen: AbortSignal[] = [];
  const fetcher: Fetch = (_input, init) => new Promise<Response>((resolve, reject) => {
    const signal = init!.signal!;
    seen.push(signal);
    const aborted = () => new DOMException("The operation was aborted.", "AbortError");
    if (reply === "no-headers") { signal.addEventListener("abort", () => reject(aborted())); return; }
    if (reply === "no-content") { resolve(new Response(null, { status: 204 })); return; }
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode("{\"value\":"));
        if (reply === "complete") { controller.enqueue(new TextEncoder().encode("1}")); controller.close(); return; }
        signal.addEventListener("abort", () => controller.error(aborted()));
      },
    });
    resolve(new Response(body, { status: 200, headers: { "Content-Type": "application/json" } }));
  });
  return { fetcher, seen };
}

async function outcome(promise: Promise<unknown>): Promise<unknown> {
  try { await promise; return "resolved"; } catch (error) { return error; }
}

async function main(): Promise<void> {
  for (const reply of ["no-headers", "stalled-body"] as const) {
    const { fetcher, seen } = server(reply);
    const started = Date.now();
    const error = await outcome(boundedFetchWithin(50, fetcher)("/api/inventory"));
    assert(error instanceof RequestTimeoutError, `${reply}: did not end with a request timeout`);
    assert(Date.now() - started < 1_000, `${reply}: did not end at its deadline`);
    assert(seen[0]!.aborted, `${reply}: the underlying request was not cancelled`);
  }
  {
    const { fetcher, seen } = server("complete");
    const response = await boundedFetchWithin(50, fetcher)("/api/inventory");
    assert((await response.json() as { value: number }).value === 1, "a complete body was not readable by the caller");
    await sleep(80);
    assert(!seen[0]!.aborted, "a settled request was cancelled after its deadline");
  }
  {
    const { fetcher } = server("no-content");
    const response = await boundedFetchWithin(50, fetcher)("/api/csrf");
    assert(response.status === 204, "a reply without content was not returned");
  }
  {
    const { fetcher, seen } = server("stalled-body");
    const caller = new AbortController();
    const pending = outcome(boundedFetchWithin(5_000, fetcher)("/api/inventory", { signal: caller.signal }));
    await sleep(10);
    caller.abort();
    const error = await pending;
    assert(error instanceof Error && !(error instanceof RequestTimeoutError), "a caller's cancellation was reported as a timeout");
    assert(seen[0]!.aborted, "the caller's cancellation did not reach the request");
  }
  {
    const { fetcher, seen } = server("complete");
    const caller = new AbortController();
    caller.abort();
    await outcome(boundedFetchWithin(5_000, fetcher)("/api/inventory", { signal: caller.signal }));
    assert(seen[0]!.aborted, "an already cancelled caller started a live request");
  }
  {
    // A Request carries its own signal; cancelling it cancels the request.
    const { fetcher, seen } = server("complete");
    const caller = new AbortController();
    caller.abort();
    await outcome(boundedFetchWithin(5_000, fetcher)(new Request("http://localhost/api/inventory", { signal: caller.signal })));
    assert(seen[0]!.aborted, "a cancelled Request started a live request");
  }
  console.log("bounded fetch tests passed");
}

void main();
