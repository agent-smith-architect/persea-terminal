// Unit gate for the document-global snippets/clips service (E-P3).
//
// The OSC 52 parser is pinned here corpus by corpus (EP3-F7's decision table),
// together with the single-flight poll, the device-local coalescer's
// last-value-wins economics (EP3-F8), the CAS/capacity outcome mapping
// (EP3-F5), and the one secured requester every mutation goes through
// (EP3-F6). Nothing here touches a document: the service is a data owner and
// takes its fetch, its clock and its timers by injection.

// Same shape as the other unit files: no node typings in the test bundle.
const assert = {
  equal<T>(actual: T, expected: T, message?: string): void {
    if (actual !== expected) throw new Error(`${message ?? "assertion failed"}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
  },
  ok(value: boolean, message: string): void {
    if (!value) throw new Error(message);
  },
};

import {
  OSC_SNIPPET_ID,
  SnippetService,
  deviceOrigin,
  isStorableSnippetBody,
  OSC_BASE64_MAX_CHARS,
  parseOSC52,
  sanitizeSnippetOrigin,
  snippetLabelFromBody,
  type SnippetRecord,
  type SnippetSnapshot,
} from "../src/snippet_client";

const encoder = new TextEncoder();
const base64 = (value: string): string => {
  const bytes = encoder.encode(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
};
const base64Bytes = (bytes: Uint8Array): string => {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
};

// --- OSC 52 parser: the frozen decision table -------------------------------

const ignored = (data: string, reason: string, message: string): void => {
  const verdict = parseOSC52(data);
  assert.equal(verdict.kind, "ignore", message);
  if (verdict.kind === "ignore") assert.equal(verdict.reason, reason as typeof verdict.reason, `${message} (reason)`);
};
const written = (data: string, body: string, message: string): void => {
  const verdict = parseOSC52(data);
  assert.equal(verdict.kind, "write", message);
  if (verdict.kind === "write") assert.equal(verdict.body, body, `${message} (body)`);
};

// A READ never writes and never replies: it is the whole reason the upstream
// clipboard addon is rejected (it answers a read with 8 bytes into the pane).

// EP3-R3 comes FIRST on purpose: the corpus below also refuses a 16 KiB + 1
// body, and if that ran first it would abort the file before the cheaper,
// more fundamental property — that such a body never reaches atob at all —
// was ever checked.
// --- EP3-R3: the 16 KiB bound is applied BEFORE atob decodes/allocates -------
// The pinned xterm parser accepts up to 10,000,000 payload characters, all of
// them chosen by whatever runs in the pane. Checking only the DECODED length
// still lets a pane force multi-megabyte decode/allocation work on a phone, so
// the encoded length is rejected first and atob is never reached.
{
  const globals = globalThis as unknown as { atob: (value: string) => string };
  const realAtob = globals.atob;
  let atobCalls = 0;
  globals.atob = (value: string): string => { atobCalls += 1; return realAtob(value); };
  const withSpy = (data: string): number => { atobCalls = 0; parseOSC52(data); return atobCalls; };
  try {
    assert.equal(OSC_BASE64_MAX_CHARS, 21848, "the encoded bound is 4 * ceil(16384 / 3)");

    // 16384 and 16385 bytes both encode to exactly 21848 characters
    // (4 * ceil(16385 / 3) === 4 * ceil(16384 / 3)), so the CHARACTER bound
    // cannot separate them — but the padding can, and the padding is part of
    // the encoded string. 16 KiB pads with "==" (16384 % 3 === 1); 16 KiB + 1
    // pads with "=" (16385 % 3 === 2). Deciding from the padding is exact and
    // costs nothing, so one byte over the cap must never reach atob either.
    const overCap = base64("a".repeat(16 * 1024 + 1));
    assert.equal(overCap.length, OSC_BASE64_MAX_CHARS, "16 KiB + 1 encodes to exactly the bound, not past it");
    assert.equal(overCap.endsWith("=") && !overCap.endsWith("=="), true, "16 KiB + 1 carries one padding character");
    assert.equal(withSpy(`c;${overCap}`), 0, "16 KiB + 1 must be rejected from its padding, before atob");
    ignored(`c;${overCap}`, "oversize", "16 KiB + 1 is refused as oversize");

    // 16 KiB + 2 and + 3 close the padding cases: no padding at the bound,
    // and the next byte spills past the character cap entirely.
    const overCapTwo = base64("a".repeat(16 * 1024 + 2));
    assert.equal(overCapTwo.endsWith("="), false, "16 KiB + 2 carries no padding");
    assert.equal(withSpy(`c;${overCapTwo}`), 0, "16 KiB + 2 must be rejected before atob");
    ignored(`c;${overCapTwo}`, "oversize", "16 KiB + 2 is refused as oversize");
    const overCapThree = base64("a".repeat(16 * 1024 + 3));
    assert.equal(overCapThree.length > OSC_BASE64_MAX_CHARS, true, "16 KiB + 3 passes the character bound");
    assert.equal(withSpy(`c;${overCapThree}`), 0, "16 KiB + 3 must be rejected before atob");

    // The boundary itself still decodes: the exact cap is storable.
    const atCap = base64("a".repeat(16 * 1024));
    assert.equal(atCap.endsWith("=="), true, "16 KiB carries two padding characters");
    assert.equal(withSpy(`c;${atCap}`), 1, "exactly 16 KiB is decoded, not refused");
    written(`c;${atCap}`, "a".repeat(16 * 1024), "exactly 16 KiB is a write, not a refusal");

    // One CHARACTER past the encoded bound never reaches atob. This is the
    // check that bounds the pane's worst case.
    const pastBound = "A".repeat(OSC_BASE64_MAX_CHARS + 4);
    assert.equal(withSpy(`c;${pastBound}`), 0, "the 16 KiB limit must be applied before atob allocates/decodes the payload");
    ignored(`c;${pastBound}`, "oversize", "a payload past the encoded bound is refused as oversize");

    // A very large VALID base64 — the pane-controlled worst case.
    const huge = "A".repeat(1_000_000);
    assert.equal(withSpy(`c;${huge}`), 0, "a megabyte of valid base64 must not reach atob");
    ignored(`c;${huge}`, "oversize", "a megabyte of valid base64 is refused as oversize");

    // Malformed AND large: still refused on length, before any validation cost.
    const malformed = "!".repeat(1_000_000);
    assert.equal(withSpy(`c;${malformed}`), 0, "a megabyte of malformed input must not reach atob");
    // ...and it is refused on LENGTH, not on content: the character bound runs
    // before the grammar regex, so a megabyte is never scanned. Without that
    // bound this same payload comes back as "base64" instead.
    ignored(`c;${malformed}`, "oversize", "a megabyte of malformed input is refused on length, before its content is inspected");

    // The controls: exactly 16 KiB of CJK-dominant and of emoji text still
    // decode and store. The BOUNDARY is the point of these two, so each one's
    // byte length is asserted before it is used — `"日".repeat((16*1024)/3)`
    // silently truncated its fractional repeat count to 5461 characters and
    // produced a 16,383-byte control, one byte short of the boundary it
    // claimed to sit on. A control that is not on the boundary tests nothing
    // the shorter bodies above have not already tested.
    const bytesOf = (value: string): number => new TextEncoder().encode(value).length;
    const cjkUnit = "日";                       // 3 bytes
    const cjk = cjkUnit.repeat(Math.floor((16 * 1024) / 3)) + "a";   // 5461 * 3 + 1
    assert.equal(bytesOf(cjk), 16 * 1024, "the CJK boundary control must be exactly 16 KiB");
    const emojiUnit = "🎉";                     // 4 bytes
    const emoji = emojiUnit.repeat((16 * 1024) / 4);
    assert.equal(bytesOf(emoji), 16 * 1024, "the emoji boundary control must be exactly 16 KiB");
    assert.ok(withSpy(`c;${base64(cjk)}`) === 1, "an exact-16 KiB CJK body still reaches atob");
    written(`c;${base64(cjk)}`, cjk, "an exact-16 KiB CJK body still writes");
    assert.ok(withSpy(`c;${base64(emoji)}`) === 1, "an exact-16 KiB emoji body still reaches atob");
    written(`c;${base64(emoji)}`, emoji, "an exact-16 KiB emoji body still writes");
    // ...and one byte past the boundary, in each encoding, is refused without
    // atob — the boundary is only meaningful if its far side is pinned too.
    const cjkOver = cjk + "a";
    assert.equal(bytesOf(cjkOver), 16 * 1024 + 1, "the CJK over-boundary control must be 16 KiB + 1");
    assert.equal(withSpy(`c;${base64(cjkOver)}`), 0, "16 KiB + 1 of CJK-dominant text must not reach atob");
    ignored(`c;${base64(cjkOver)}`, "oversize", "16 KiB + 1 of CJK-dominant text is refused as oversize");
    const emojiOver = emoji + "a";
    assert.equal(bytesOf(emojiOver), 16 * 1024 + 1, "the emoji over-boundary control must be 16 KiB + 1");
    assert.equal(withSpy(`c;${base64(emojiOver)}`), 0, "16 KiB + 1 of emoji text must not reach atob");
    ignored(`c;${base64(emojiOver)}`, "oversize", "16 KiB + 1 of emoji text is refused as oversize");
  } finally {
    globals.atob = realAtob;
  }
}

ignored("c;?", "read", "an OSC 52 read must be ignored");
ignored("p;?", "read", "a primary-selection read must be ignored");
ignored("?", "malformed", "a payload with no selection separator must be ignored");
ignored("", "malformed", "an empty payload must be ignored");
// Only c and p write.
ignored(`s;${base64("nope")}`, "selection", "selection s must not write");
ignored(`cp;${base64("nope")}`, "selection", "a selection set must not write");
ignored(`;${base64("nope")}`, "selection", "an empty selection must not write");
ignored(`C;${base64("nope")}`, "selection", "an uppercase selection must not write");
// Base64 grammar.
ignored("c;not base64!", "base64", "malformed base64 must be ignored");
ignored("c;YQ", "base64", "unpadded base64 must be ignored");
ignored("c;", "base64", "an empty body must be ignored");
ignored("c;====", "base64", "padding-only base64 must be ignored");
// Fatal UTF-8: never atob's Latin-1 string (xterm.js #6000).
ignored(`c;${base64Bytes(new Uint8Array([0xff, 0xfe]))}`, "utf8", "invalid UTF-8 must be ignored");
// Round trips.
written(`c;${base64("echo hello")}`, "echo hello", "ASCII must round-trip");
written(`p;${base64("echo hello")}`, "echo hello", "the primary selection must round-trip");
written(`c;${base64("日本語のテキスト")}`, "日本語のテキスト", "CJK must round-trip");
written(`c;${base64("🎉 done")}`, "🎉 done", "emoji must round-trip");
written(`c;${base64("two\nlines\twith a tab")}`, "two\nlines\twith a tab", "newlines and tabs are storable");
// The 16 KiB decoded cap, exactly.
const exactly16k = "a".repeat(16 * 1024);
written(`c;${base64(exactly16k)}`, exactly16k, "exactly 16 KiB must be accepted");
ignored(`c;${base64(exactly16k + "a")}`, "oversize", "16 KiB + 1 must be ignored");
// The store's grammar: a control byte the file cannot hold is never rewritten
// to fit, and never spends a request that could only 400.
ignored(`c;${base64("bad\u0007bell")}`, "grammar", "a control byte must be ignored");
ignored(`c;${base64("crlf\r\n")}`, "grammar", "a carriage return must be ignored");

assert.equal(isStorableSnippetBody(""), false, "an empty body is not storable");
assert.equal(isStorableSnippetBody("a".repeat(16 * 1024)), true, "16 KiB is storable");
assert.equal(isStorableSnippetBody("a".repeat(16 * 1024 + 1)), false, "16 KiB + 1 is not storable");
assert.equal(isStorableSnippetBody("ok\ttab\nnewline"), true, "tab and newline are storable");
assert.equal(isStorableSnippetBody("\u0000"), false, "NUL is not storable");
assert.equal(isStorableSnippetBody("\u007f"), false, "DEL is not storable");

// --- labels and origins ------------------------------------------------------

assert.equal(snippetLabelFromBody("./deploy.sh --prod"), "./deploy.sh --prod", "a one-line body labels itself");
assert.equal(snippetLabelFromBody("\n\n  first  line \nsecond"), "first line", "the label is the first visible line, collapsed");
assert.equal(Array.from(snippetLabelFromBody("x".repeat(200))).length, 64, "a long label is bounded to 64 runes");
assert.equal(snippetLabelFromBody("   "), "", "a blank body yields no label");
assert.equal(sanitizeSnippetOrigin("phone"), "phone", "an ordinary origin survives");
assert.equal(sanitizeSnippetOrigin("osc52"), "", "the reserved token is never an origin");
assert.equal(sanitizeSnippetOrigin("with\u0007control"), "withcontrol", "control bytes are removed from an origin");
assert.equal(Array.from(sanitizeSnippetOrigin("y".repeat(64))).length, 32, "an origin is bounded to 32 runes");


// --- a scriptable transport --------------------------------------------------

type Call = Readonly<{ path: string; method: string; headers: Record<string, string>; body: string | undefined; credentials: string | undefined; cache: string | undefined }>;

class Harness {
  readonly calls: Call[] = [];
  readonly timers: Array<Readonly<{ id: number; handler: () => void; ms: number }>> = [];
  private nextTimer = 1;
  private queue: Array<() => Promise<Response>> = [];
  private records: unknown[] = [];
  snapshots: SnippetSnapshot[] = [];
  clipboardWrites: string[] = [];
  clipboardRefuses = false;

  service = new SnippetService({
    fetch: (input, init) => this.fetch(input, init),
    csrf: () => "csrf-token",
    origin: "phone",
    clipboard: { writeText: (text) => this.write(text) },
    pollIntervalMs: 1_000,
    oscQuiescenceMs: 750,
    setTimer: ((handler: () => void, ms: number) => {
      const id = this.nextTimer;
      this.nextTimer += 1;
      this.timers.push(Object.freeze({ id, handler, ms }));
      return id as unknown as ReturnType<typeof setTimeout>;
    }) as unknown as (handler: () => void, ms: number) => ReturnType<typeof setTimeout>,
    clearTimer: ((timer: unknown) => {
      const index = this.timers.findIndex((entry) => entry.id === (timer as number));
      if (index >= 0) this.timers.splice(index, 1);
    }) as unknown as (timer: ReturnType<typeof setTimeout>) => void,
  });

  constructor() {
    this.service.subscribe((snapshot) => this.snapshots.push(snapshot));
  }

  private async write(text: string): Promise<void> {
    if (this.clipboardRefuses) throw new Error("refused");
    this.clipboardWrites.push(text);
  }

  setRecords(records: unknown[]): void {
    this.records = records;
  }

  /** Queues one scripted response for the NEXT request, ahead of the default. */
  reply(make: () => Promise<Response>): void {
    this.queue.push(make);
  }

  private fetch(input: string, init: RequestInit): Promise<Response> {
    const headers = (init.headers ?? {}) as Record<string, string>;
    this.calls.push(Object.freeze({
      path: input,
      method: String(init.method),
      headers: { ...headers },
      body: typeof init.body === "string" ? init.body : undefined,
      credentials: init.credentials,
      cache: init.cache,
    }));
    const scripted = this.queue.shift();
    if (scripted) return scripted();
    if (init.method === "GET") return Promise.resolve(json(200, { items: this.records }));
    return Promise.resolve(json(200, {}));
  }

  /** Runs the one timer whose delay matches, as a fake clock tick would. */
  fire(ms: number): void {
    const index = this.timers.findIndex((entry) => entry.ms === ms);
    if (index < 0) throw new Error(`no timer scheduled for ${ms}ms`);
    const entry = this.timers[index];
    this.timers.splice(index, 1);
    entry.handler();
  }
}

const json = (status: number, value: unknown): Response =>
  status === 204 || status === 304
    ? new Response(null, { status })
    : new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });

const record = (patch: Partial<SnippetRecord> & Pick<SnippetRecord, "id" | "kind">): SnippetRecord => Object.freeze({
  label: "", body: "body", pinned: false, origin: "", revision: 1,
  createdAt: "2026-08-27T10:00:00Z", updatedAt: "2026-08-27T10:00:00Z", expiresAt: null, retentionSeconds: 0, preview: "",
  ...patch,
});

// The WIRE shape the front door returns (snake_case), which the client parses.
const wire = (patch: Readonly<Record<string, unknown>> & Readonly<{ id: string; kind: string }>): Record<string, unknown> => ({
  label: "", body: "body", pinned: false, origin: "", revision: 1,
  created_at: "2026-08-27T10:00:00Z", updated_at: "2026-08-27T10:00:00Z", expires_at: null,
  ...patch,
});

const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

// A response the test resolves by hand, so a request can be held "in flight"
// across other work. EP3-R1 and EP3-R2 are both about what may happen while a
// request is unresolved, which no amount of fast fake responses can express.
type Deferred = Readonly<{ promise: Promise<Response>; resolve: (response: Response) => void }>;
const deferred = (): Deferred => {
  let release: (response: Response) => void = () => undefined;
  const promise = new Promise<Response>((resolve) => { release = resolve; });
  return Object.freeze({ promise, resolve: release });
};

// A CJS test bundle has no top-level await: the asynchronous cases are
// collected here and run in order by main().
const blocks: Array<() => Promise<void>> = [];

// --- construction is inert ---------------------------------------------------

{
  const harness = new Harness();
  assert.equal(harness.calls.length, 0, "constructing the service made a request");
  assert.equal(harness.timers.length, 0, "constructing the service armed a timer");
  assert.equal(harness.snapshots.length, 1, "subscribing must deliver the current snapshot synchronously");
  assert.equal(harness.snapshots[0].status, "idle", "the first snapshot must be idle, not loading");
}

// --- single flight: N views, ONE request -------------------------------------

blocks.push(async () => {
  const harness = new Harness();
  let release: (() => void) | undefined;
  harness.reply(() => new Promise<Response>((resolve) => {
    release = () => resolve(json(200, { items: [] }));
  }));
  // Six views retain the live state, exactly as six panes with an open list.
  const retains = [0, 1, 2, 3, 4, 5].map(() => harness.service.retainLive());
  assert.equal(harness.calls.length, 1, "six retained views must produce ONE request");
  // A refresh arriving while the first is in flight joins it.
  const joined = harness.service.refresh();
  assert.equal(harness.calls.length, 1, "a refresh during an in-flight poll must not overlap it");
  release?.();
  await joined;
  assert.equal(harness.calls.length, 1, "the joined refresh must not have issued a second request");
  assert.equal(harness.service.stats().concurrentPolls, 1, "polls overlapped");
  for (const retain of retains) retain.dispose();
  // Releasing every view cancels the loop: a sheet nobody opened costs nothing.
  assert.equal(harness.timers.length, 0, "the poll timer survived the last view's release");
  // A double dispose must not drive the retain count negative and restart a loop.
  retains[0].dispose();
  assert.equal(harness.timers.length, 0, "a repeated dispose restarted the poll");
});

// --- the snapshot splits the distinguished OSC record from manual clips -------

blocks.push(async () => {
  const harness = new Harness();
  harness.setRecords([
    wire({ id: "a".repeat(32), kind: "snippet", label: "deploy", pinned: true }),
    wire({ id: "b".repeat(32), kind: "clip", preview: "clip preview", expires_at: "2026-08-28T10:00:00Z" }),
    wire({ id: OSC_SNIPPET_ID, kind: "clip", body: "from the pane", origin: "phone", expires_at: "2026-08-28T10:00:00Z" }),
  ]);
  await harness.service.refresh();
  const snapshot = harness.service.snapshot();
  assert.equal(snapshot.status, "ready", "a good list must be ready");
  assert.equal(snapshot.snippets.length, 1, "one snippet");
  assert.equal(snapshot.clips.length, 1, "the manual ring must not contain the OSC record");
  assert.equal(snapshot.osc?.body, "from the pane", "the OSC record must be split out");
  assert.equal(snapshot.generation, 1, "a completed poll must advance the generation");

  // A record outside the closed grammar fails the whole response closed: a
  // partially rendered list would claim the store holds something it does not.
  harness.reply(() => Promise.resolve(json(200, { items: [{ id: "nope", kind: "snippet" }] })));
  await harness.service.refresh();
  assert.equal(harness.service.snapshot().status, "unavailable", "a malformed record must fail closed");
  assert.equal(harness.service.snapshot().snippets.length, 0, "a failed read must not keep stale rows");

  harness.reply(() => Promise.resolve(json(503, {})));
  await harness.service.refresh();
  assert.equal(harness.service.snapshot().status, "unavailable", "a 503 must render unavailable");
});

// --- every mutation goes through the ONE secured requester --------------------

blocks.push(async () => {
  const harness = new Harness();
  const target = record({ id: "c".repeat(32), kind: "snippet", label: "deploy", revision: 4 });
  harness.reply(() => Promise.resolve(json(201, {})));
  await harness.service.createSnippet("deploy", "./deploy.sh");
  harness.reply(() => Promise.resolve(json(200, {})));
  await harness.service.createClip("copied text");
  harness.reply(() => Promise.resolve(json(200, {})));
  await harness.service.setPinned(target, true);
  harness.reply(() => Promise.resolve(json(204, {})));
  await harness.service.remove(target);
  harness.service.publishOSC("from the pane");
  harness.reply(() => Promise.resolve(json(200, {})));
  harness.fire(750);
  await settle();

  const mutations = harness.calls.filter((call) => call.method !== "GET");
  assert.equal(mutations.length, 5, "expected exactly five mutations");
  for (const call of mutations) {
    assert.equal(call.headers["Content-Type"], "application/json", `${call.method} ${call.path} lost the exact JSON content type`);
    assert.equal(call.headers["X-Persea-CSRF"], "csrf-token", `${call.method} ${call.path} lost the CSRF header`);
    assert.equal(call.credentials, "same-origin", `${call.method} ${call.path} lost same-origin credentials`);
    assert.equal(call.cache, "no-store", `${call.method} ${call.path} was cacheable`);
    assert.ok(call.path.startsWith("/api/snippets"), `${call.path} is not a snippets route`);
  }
  for (const call of harness.calls.filter((entry) => entry.method === "GET")) {
    assert.equal(call.cache, "no-store", "a read was cacheable");
    assert.equal(call.credentials, "same-origin", "a read lost same-origin credentials");
    assert.equal(call.headers["X-Persea-CSRF"], undefined as unknown as string, "a read carried a CSRF header it does not need");
  }
  // The OSC write is the distinguished upsert: PUT, the fixed id, never a POST
  // and never a per-device id.
  const osc = mutations[4];
  assert.equal(osc.method, "PUT", "the OSC write must be a PUT");
  assert.equal(osc.path, "/api/snippets/osc52", "the OSC write must address the one global record");
  assert.equal(osc.headers["If-Match"], undefined as unknown as string, "the OSC upsert must not carry a precondition");
  assert.equal(JSON.parse(osc.body ?? "{}").origin, "phone", "the OSC write must carry its sanitized origin");
  // The snippet POST carries the closed field set the route accepts.
  const created = JSON.parse(mutations[0].body ?? "{}");
  assert.equal(Object.keys(created).sort().join(","), "body,kind,label,origin,pinned", "the create body is not the closed field set");
  // PATCH/DELETE carry the body revision, never an If-Match.
  assert.equal(JSON.parse(mutations[2].body ?? "{}").revision, 4, "PATCH must carry the record's revision");
  assert.equal(JSON.parse(mutations[3].body ?? "{}").revision, 4, "DELETE must carry the record's revision");
});

// --- outcome mapping: conflict, capacity, unavailable -------------------------

blocks.push(async () => {
  const harness = new Harness();
  const target = record({ id: "d".repeat(32), kind: "snippet", label: "x", revision: 2 });
  harness.reply(() => Promise.resolve(json(409, { id: target.id, revision: 9 })));
  assert.equal(await harness.service.setPinned(target, true), "conflict", "a 409 must be a conflict");
  harness.reply(() => Promise.resolve(json(412, { id: OSC_SNIPPET_ID, revision: 3 })));
  assert.equal(await harness.service.createClip("x"), "conflict", "a 412 must be a conflict");
  harness.reply(() => Promise.resolve(json(507, {})));
  assert.equal(await harness.service.createSnippet("x", "y"), "full", "a 507 must be a full store");
  harness.reply(() => Promise.resolve(json(413, {})));
  assert.equal(await harness.service.createClip("x"), "too_large", "a 413 must be too large");
  harness.reply(() => Promise.resolve(json(503, {})));
  assert.equal(await harness.service.createClip("x"), "unavailable", "a 503 must be unavailable");
  harness.reply(() => Promise.resolve(json(403, {})));
  assert.equal(await harness.service.createClip("x"), "refused", "a 403 must be a refusal");
  harness.reply(() => Promise.reject(new Error("offline")));
  assert.equal(await harness.service.createClip("x"), "unreachable", "a transport failure must be unreachable");
  // A conflict re-reads the store so the sheet shows current authority rather
  // than the state the refused edit assumed — and NEVER silently retries the
  // stale edit: seven attempts produce exactly seven mutations.
  const reads = harness.calls.filter((call) => call.method === "GET").length;
  assert.ok(reads >= 1, `a conflict/capacity refusal must re-read the store (reads=${reads})`);
  assert.equal(harness.calls.filter((call) => call.method !== "GET").length, 7, "a refused mutation was retried behind the operator's back");
});

// --- the device-local coalescer: last value wins, one request per window ------

blocks.push(async () => {
  const harness = new Harness();
  for (let index = 0; index < 30; index += 1) harness.service.publishOSC(`value-${index}`);
  assert.equal(harness.calls.length, 0, "a pending OSC value must not publish before its window");
  assert.equal(harness.service.stats().oscCoalesced, 29, "29 of 30 writes must have coalesced");
  harness.fire(750);
  await settle();
  const puts = harness.calls.filter((call) => call.method === "PUT");
  assert.equal(puts.length, 1, "thirty writes must publish exactly one value");
  assert.equal(JSON.parse(puts[0].body ?? "{}").body, "value-29", "the published value must be the last one");
  assert.equal(harness.clipboardWrites.length, 1, "the published value must reach the system clipboard");
  assert.equal(harness.service.snapshot().clipboardHandoff, undefined, "a successful clipboard write must not ask for a tap");

  // A body outside the store's grammar never becomes a pending value at all.
  harness.service.publishOSC("bad\u0007bell");
  assert.equal(harness.timers.length, 0, "an ungrammatical body armed the coalescer");

  // A refused system-clipboard write is a clips-only success, never a failure.
  harness.clipboardRefuses = true;
  harness.service.publishOSC("second");
  harness.fire(750);
  await settle();
  assert.equal(harness.service.snapshot().clipboardHandoff?.body, "second", "a refused clipboard write must ask for a tap for THAT body");
  assert.equal(harness.calls.filter((call) => call.method === "PUT").length, 2, "the second window must publish once");

  // A refused PUT publishes nothing to the clipboard either.
  harness.clipboardRefuses = false;
  harness.reply(() => Promise.resolve(json(412, { id: OSC_SNIPPET_ID, revision: 4 })));
  harness.service.publishOSC("third");
  harness.fire(750);
  await settle();
  assert.equal(harness.clipboardWrites.length, 1, "a refused OSC upsert must not touch the system clipboard");
});

// --- a disposed service is inert ---------------------------------------------

blocks.push(async () => {
  const harness = new Harness();
  const retain = harness.service.retainLive();
  const before = harness.calls.length;
  harness.service.dispose();
  harness.service.publishOSC("after dispose");
  await harness.service.refresh();
  assert.equal(harness.calls.length, before, "a disposed service still made requests");
  assert.equal(harness.timers.length, 0, "a disposed service left a timer armed");
  retain.dispose();
});

// --- the device label is presentation metadata, never identity ----------------

assert.equal(deviceOrigin({ matchMedia: () => ({ matches: true }) } as unknown as Window), "phone", "a coarse pointer is a phone");
assert.equal(deviceOrigin({ matchMedia: () => ({ matches: false }) } as unknown as Window), "desktop", "a fine pointer is a desktop");
assert.equal(deviceOrigin({} as unknown as Window), "desktop", "a window without matchMedia still yields a label");

declare const process: { exitCode: number };

// --- EP3-R1: a mutation reconciles against a read that STARTED after it ------
// refresh() joins an in-flight GET, which is right for a poll and wrong here:
// a GET that began before the mutation carries pre-mutation authority.

blocks.push(async () => {
  const harness = new Harness();
  const stale = deferred();
  harness.reply(() => stale.promise);
  const polling = harness.service.refresh();      // GET #1, held open
  await settle();
  assert.equal(harness.calls.length, 1, "EP3-R1: the stale GET is in flight");

  const mutation = harness.service.createClip("body");   // POST, resolves at once
  await settle();
  assert.equal(harness.calls.length, 2, "EP3-R1: the POST went out while the stale GET was open");
  let settled = false;
  void mutation.then(() => { settled = true; });
  await settle();
  assert.ok(!settled, "EP3-R1: the mutation must not report an outcome while only the stale GET has answered");

  stale.resolve(json(200, { items: [] }));
  await polling;
  await mutation;
  await settle();
  const gets = harness.calls.filter((call) => call.method === "GET").length;
  assert.equal(gets, 2, "a mutation joined the pre-mutation GET instead of forcing a post-response read");
  assert.equal(harness.calls[2].method, "GET", "EP3-R1: the reconciling read is the request AFTER the mutation");
  assert.equal(harness.service.stats().reconcilingReads, 1, "EP3-R1: exactly one reconciling read was taken");
});

blocks.push(async () => {
  // The same barrier for a refusal that still moves authority: 409/412.
  const harness = new Harness();
  const stale = deferred();
  harness.reply(() => stale.promise);
  const polling = harness.service.refresh();
  await settle();
  harness.reply(() => Promise.resolve(json(409, {})));
  const mutation = harness.service.setPinned(record({ id: "a", kind: "snippet" }), true);
  await settle();
  stale.resolve(json(200, { items: [] }));
  assert.equal(await mutation, "conflict", "EP3-R1: a 409 still classifies as a conflict");
  await polling;
  await settle();
  assert.equal(harness.calls.filter((call) => call.method === "GET").length, 2, "EP3-R1: a conflict also forces a post-response read");
});

blocks.push(async () => {
  // Two ordered mutations: each observes its own qualifying later GET.
  const harness = new Harness();
  await harness.service.createClip("one");
  await harness.service.createClip("two");
  await settle();
  const methods = harness.calls.map((call) => call.method).join(",");
  assert.equal(methods, "POST,GET,POST,GET", "EP3-R1: every mutation is followed by its own read");
  assert.equal(harness.service.stats().reconcilingReads, 2, "EP3-R1: two mutations, two reconciling reads");
});

// --- EP3-R2: OSC publication has ONE serialized owner ------------------------

blocks.push(async () => {
  const harness = new Harness();
  const first = deferred();
  harness.reply(() => first.promise);
  harness.service.publishOSC("old");
  harness.fire(750);
  await settle();
  assert.equal(harness.calls.length, 1, "EP3-R2: the first PUT is in flight");

  // Every later value while that PUT is unresolved collapses to the newest.
  for (let index = 0; index < 30; index += 1) harness.service.publishOSC(`flood-${index}`);
  harness.service.publishOSC("new");
  await settle();
  assert.equal(harness.calls.length, 1, "an older in-flight OSC PUT was allowed a concurrent newer PUT");
  assert.equal(harness.timers.filter((timer) => timer.ms === 750).length, 0, "EP3-R2: no window is armed while a PUT is unresolved");

  first.resolve(json(200, {}));
  await settle();
  await settle();
  harness.fire(750);
  await settle();
  await settle();
  const puts = harness.calls.filter((call) => call.method === "PUT");
  assert.equal(puts.length, 2, "EP3-R2: the survivor publishes once, after settlement");
  assert.equal(JSON.parse(String(puts[1].body)).body, "new", "an older in-flight OSC PUT completed after and overwrote the latest value");
  assert.equal(harness.clipboardWrites.join(","), "old,new", "EP3-R2: the device clipboard is written in publication order");
});

blocks.push(async () => {
  // A refused PUT must not swallow the newer value that arrived during it.
  const harness = new Harness();
  const first = deferred();
  harness.reply(() => first.promise);
  harness.service.publishOSC("old");
  harness.fire(750);
  await settle();
  harness.service.publishOSC("newer");
  first.resolve(json(503, {}));
  await settle();
  await settle();
  harness.fire(750);
  await settle();
  const puts = harness.calls.filter((call) => call.method === "PUT");
  assert.equal(puts.length, 2, "EP3-R2: a refusal does not strand the newest pending value");
  assert.equal(JSON.parse(String(puts[1].body)).body, "newer", "EP3-R2: the value published after a refusal is the newest one");
  assert.equal(harness.clipboardWrites.join(","), "newer", "EP3-R2: a refused PUT writes no clipboard");
});

blocks.push(async () => {
  // Dispose during an unresolved PUT publishes nothing further.
  const harness = new Harness();
  const first = deferred();
  harness.reply(() => first.promise);
  harness.service.publishOSC("old");
  harness.fire(750);
  await settle();
  harness.service.publishOSC("never");
  harness.service.dispose();
  first.resolve(json(200, {}));
  await settle();
  await settle();
  assert.equal(harness.timers.filter((timer) => timer.ms === 750).length, 0, "EP3-R2: dispose arms no further window");
  assert.equal(harness.calls.filter((call) => call.method === "PUT").length, 1, "EP3-R2: nothing is published after dispose");
});

// --- EP3-R6: the acknowledgement is bound to the value it is about -----------
// The outstanding delivery is a VALUE. A bare flag let ANY successful copy
// retire it, so copying an unrelated manual clip told the operator the
// automatic value had reached the clipboard when a different string had.

blocks.push(async () => {
  const raise = async (body: string): Promise<Harness> => {
    const harness = new Harness();
    harness.clipboardRefuses = true;
    harness.service.publishOSC(body);
    harness.fire(750);
    await settle();
    await settle();
    return harness;
  };

  // The hand-off names the body, not a boolean.
  {
    const harness = await raise("value");
    assert.equal(harness.service.snapshot().clipboardHandoff?.body, "value", "EP3-R6: a refused gestureless write names the owed body");
    harness.clipboardRefuses = false;
    harness.service.acknowledgeClipboard("value");
    assert.equal(harness.service.snapshot().clipboardHandoff, undefined, "EP3-R6: copying the owed body retires the hand-off");
  }

  // A MANUAL clip copied successfully cannot retire it.
  {
    const harness = await raise("automatic value");
    harness.service.acknowledgeClipboard("an unrelated manual clip");
    assert.equal(
      harness.service.snapshot().clipboardHandoff?.body,
      "automatic value",
      "EP3-R6: copying an unrelated clip must not clear the automatic hand-off",
    );
  }

  // A STALE OSC row copied after a newer failure cannot retire it either.
  {
    const harness = await raise("older");
    harness.service.publishOSC("newer");
    harness.fire(750);
    await settle();
    await settle();
    assert.equal(harness.service.snapshot().clipboardHandoff?.body, "newer", "EP3-R6: a newer failure replaces the hand-off");
    harness.service.acknowledgeClipboard("older");
    assert.equal(harness.service.snapshot().clipboardHandoff?.body, "newer", "EP3-R6: copying the stale row must not clear the newer hand-off");
    harness.service.acknowledgeClipboard("newer");
    assert.equal(harness.service.snapshot().clipboardHandoff, undefined, "EP3-R6: copying the newest owed body retires it");
  }

  // A newer SUCCESS retires the hand-off on its own: the clipboard now holds
  // the current value, so there is nothing left to tap for.
  {
    const harness = await raise("owed");
    harness.clipboardRefuses = false;
    harness.service.publishOSC("delivered");
    harness.fire(750);
    await settle();
    await settle();
    assert.equal(harness.service.snapshot().clipboardHandoff, undefined, "EP3-R6: a later successful write clears the hand-off");
  }

  // A disposed service acknowledges nothing.
  {
    const harness = await raise("value");
    harness.service.dispose();
    harness.service.acknowledgeClipboard("value");
    assert.equal(harness.service.snapshot().clipboardHandoff?.body, "value", "EP3-R6: a disposed service must not publish");
  }
});

// --- EP3-R5: an operational store failure publishes unavailable --------------
// A mutation is also a probe. Without this, a copy after the store died kept
// firing mutations at a store already known dead, and the sheet only learned
// about the outage at the next poll.

blocks.push(async () => {
  // A 503 latches unavailable...
  {
    const harness = new Harness();
    await harness.service.refresh();
    assert.equal(harness.service.snapshot().status, "ready", "the store starts reachable");
    harness.reply(() => Promise.resolve(json(503, {})));
    assert.equal(await harness.service.createClip("x"), "unavailable", "a 503 is an unavailable outcome");
    assert.equal(harness.service.snapshot().status, "unavailable", "EP3-R5: a 503 mutation must publish unavailable");
  }

  // ...and so does a transport failure.
  {
    const harness = new Harness();
    await harness.service.refresh();
    harness.reply(() => Promise.reject(new Error("offline")));
    assert.equal(await harness.service.createClip("x"), "unreachable", "a transport failure is unreachable");
    assert.equal(harness.service.snapshot().status, "unavailable", "EP3-R5: an unreachable mutation must publish unavailable");
  }

  // The lists are NOT wiped: their rows carry the local copy actions that
  // have to keep working through the outage. A failed READ still wipes them,
  // because a read failure means we hold no authority at all.
  {
    const harness = new Harness();
    harness.setRecords([wire({ id: "c".repeat(32), kind: "clip", preview: "still copyable" })]);
    await harness.service.refresh();
    const before = harness.service.snapshot().clips.length;
    assert.ok(before > 0, "the fixture must seed at least one clip for this to mean anything");
    harness.reply(() => Promise.resolve(json(503, {})));
    await harness.service.createClip("x");
    assert.equal(harness.service.snapshot().status, "unavailable", "the outage is published");
    assert.equal(harness.service.snapshot().clips.length, before, "EP3-R5: a failed mutation must not wipe the last known list");
    harness.reply(() => Promise.resolve(json(503, {})));
    await harness.service.refresh();
    assert.equal(harness.service.snapshot().clips.length, 0, "a failed READ still clears the list");
  }

  // Only ONE publication for a run of failures: the sheet does not thrash.
  {
    const harness = new Harness();
    await harness.service.refresh();
    let publications = 0;
    harness.service.subscribe(() => { publications += 1; });
    publications = 0;
    harness.reply(() => Promise.resolve(json(503, {})));
    harness.reply(() => Promise.resolve(json(503, {})));
    await harness.service.createClip("a");
    await harness.service.createClip("b");
    assert.equal(publications, 1, "EP3-R5: a second failure against a known-dead store must not republish");
  }
});

async function main(): Promise<void> {
  for (const run of blocks) await run();
  console.log("snippet_client.test.ts ok");
}

main().catch((error: unknown) => {
  console.error(error instanceof Error ? (error.stack ?? error.message) : String(error));
  process.exitCode = 1;
});
