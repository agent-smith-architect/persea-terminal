import { ClipboardImages, ClipboardImageError, clipboardImageErrorMessage } from "../src/clipboard_images";
import { ClipboardPreferencesService } from "../src/clipboard_preferences";
import { RETENTION_SECONDS, clipboardRetentionLabel, isClipboardRetentionSeconds, type ClipboardRetentionSeconds } from "../src/clipboard_retention";
import { SnippetService } from "../src/snippet_client";

function equal(actual: unknown, expected: unknown, message: string): void {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
}
function ok(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
const tick = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
const json = (value: unknown, status = 200, headers: Record<string, string> = {}): Response => new Response(JSON.stringify(value), { status, headers });
const textWire = (patch: Record<string, unknown> = {}): Record<string, unknown> => ({
  id: "a".repeat(32), kind: "clip", body: "exact text", label: "", pinned: false, origin: "phone", revision: 1,
  created_at: "2026-09-07T00:00:00Z", updated_at: "2026-09-07T00:00:00Z", expires_at: "2026-09-07T00:30:00Z", ...patch,
});
const imageWire = (patch: Record<string, unknown> = {}): Record<string, unknown> => ({
  id: "b".repeat(32), media_type: "image/png", byte_size: 3, created_at: "2026-09-07T00:00:00Z",
  expires_at: "2026-09-07T00:30:00Z", origin: "phone", ...patch,
});
const preference = (seconds: number, revision: number): Response => json({ version: 1, default_retention_seconds: seconds, revision }, 200, { ETag: `"${revision}"` });
function deferred(): { promise: Promise<Response>; resolve(response: Response): void } {
  let resolve!: (response: Response) => void;
  return { promise: new Promise<Response>((done) => { resolve = done; }), resolve: (response) => resolve(response) };
}
type Call = { path: string; init: RequestInit };
function transport() {
  const calls: Call[] = [];
  const replies: Array<Response | Promise<Response>> = [];
  return {
    calls, replies,
    fetch: async (path: string, init: RequestInit): Promise<Response> => {
      calls.push({ path, init });
      const next = replies.shift();
      if (!next) throw new Error(`Unexpected ${init.method} ${path}`);
      return next;
    },
  };
}
const body = (call: Call): unknown => JSON.parse(String(call.init.body));
const cases: Array<[string, () => Promise<void>]> = [];

cases.push(["closed retention vocabulary", async () => {
  equal(RETENTION_SECONDS, [1800, 14400, 86400, 604800, 2592000, 0], "duration choices");
  for (const value of [undefined, null, "1800", -1, 1, 1800.5, NaN, Infinity]) equal(isClipboardRetentionSeconds(value), false, "reject unrecognized retention");
  for (const value of RETENTION_SECONDS) ok(isClipboardRetentionSeconds(value), "accept every contract duration");
  equal(clipboardRetentionLabel(0), "No expiry", "permanent label");
  equal(clipboardRetentionLabel(2592000), "30 days", "month means thirty days");
}]);

cases.push(["text metadata normalizes legacy and validates retention independently of kind", async () => {
  const t = transport(); const service = new SnippetService({ fetch: t.fetch });
  t.replies.push(json({ items: [textWire(), textWire({ id: "c".repeat(32), kind: "snippet", expires_at: null })] }));
  await service.refresh();
  equal(service.snapshot().clips[0].retentionSeconds, 1800, "legacy timed clip");
  equal(service.snapshot().snippets[0].retentionSeconds, 0, "legacy permanent snippet");
  t.replies.push(json({ items: [textWire({ retention_seconds: 0, expires_at: null }), textWire({ id: "c".repeat(32), kind: "snippet", retention_seconds: 604800, expires_at: "2026-09-14T00:00:00Z" })] }));
  await service.refresh();
  equal(service.snapshot().status, "ready", "new retention no longer follows legacy kind");
  equal(service.snapshot().clips[0].expiresAt, null, "permanent clip remains null");
  equal(service.snapshot().snippets[0].retentionSeconds, 604800, "timed legacy snippet is accepted");
  for (const patch of [{ retention_seconds: 12 }, { retention_seconds: "1800" }, { retention_seconds: 0 }, { retention_seconds: 1800, expires_at: null }, { expires_at: "invalid" }, { updated_at: "invalid" }]) {
    t.replies.push(json({ items: [textWire(patch)] })); await service.refresh();
    equal(service.snapshot().status, "unavailable", "malformed metadata fails closed");
  }
  service.dispose();
}]);

cases.push(["text edits preserve bytes, revision authority and canonical reconciliation", async () => {
  const t = transport(); const service = new SnippetService({ fetch: t.fetch, csrf: () => "csrf" });
  t.replies.push(json({ items: [textWire({ revision: 3 })] })); await service.refresh();
  const record = service.snapshot().clips[0]; const exact = " \tline\n ";
  t.replies.push(json(textWire({ id: "c".repeat(32), body: exact, revision: 8 })), json({ items: [textWire({ id: "c".repeat(32), body: exact, revision: 8 })] }));
  equal(await service.updateText(record, exact), "ok", "duplicate edit succeeds");
  equal(body(t.calls[1]), { body: exact, revision: 3 }, "edit preserves exact content and CAS");
  equal(service.snapshot().clips.map((item) => item.id), ["c".repeat(32)], "server surviving ID replaces stale item");
  t.replies.push(json({}, 412), json({ items: [textWire({ revision: 9, retention_seconds: 0, expires_at: null })] }));
  equal(await service.setRetention(record, 0), "conflict", "stale retention is not retried");
  equal(body(t.calls[3]), { retention_seconds: 0, revision: 3 }, "retention revision preserved");
  equal(service.snapshot().clips[0].revision, 9, "conflict refresh delivers server authority");
  const headers = t.calls[3].init.headers as Record<string, string>;
  equal(headers["X-Persea-CSRF"], "csrf", "retention mutation CSRF");
  equal(t.calls[3].init.credentials, "same-origin", "retention credentials");
  equal(t.calls[3].init.cache, "no-store", "retention no-store");
  t.replies.push(json({}, 201), json({ items: [] })); await service.createClip(exact, 14400);
  equal(body(t.calls[5]), { kind: "clip", body: exact, retention_seconds: 14400 }, "explicit duration addition");
  t.replies.push(json({}, 200), json({ items: [] })); await service.createClip(exact);
  equal(body(t.calls[7]), { kind: "clip", body: exact }, "omitted duration delegates to shared default");
  const count = t.calls.length;
  equal(await service.updateText(record, "bad\rbody"), "refused", "client does not normalize a control into different content");
  equal(t.calls.length, count, "invalid body does not mutate");
  service.dispose();
}]);

cases.push(["OSC handoff revision comes from publication, not canonical list", async () => {
  const t = transport(); let fire: (() => void) | undefined;
  const service = new SnippetService({ fetch: t.fetch, csrf: () => "csrf", setTimer: (handler) => { fire = handler; return 1 as unknown as ReturnType<typeof setTimeout>; }, clearTimer: () => undefined });
  t.replies.push(json(textWire({ id: "osc52", body: "owed", revision: 17 })), json({ items: [textWire({ body: "owed", revision: 4 })] }));
  service.publishOSC("owed"); fire?.(); await tick(); await tick();
  equal(service.snapshot().osc, undefined, "canonical list does not expose publication record");
  equal(service.snapshot().clipboardHandoff, { body: "owed", revision: 17 }, "handoff carries matching publication authority");
  equal((t.calls[0].init.headers as Record<string, string>)["If-Match"], undefined, "E2 publication remains last-committed-write-wins");
  service.acknowledgeClipboard("other"); ok(service.snapshot().clipboardHandoff, "unrelated copy cannot acknowledge");
  service.acknowledgeClipboard("owed"); equal(service.snapshot().clipboardHandoff, undefined, "matching canonical body acknowledges");
  service.dispose();
}]);

cases.push(["images normalize legacy policy and reject malformed new metadata", async () => {
  const t = transport(); const service = new ClipboardImages({ fetch: t.fetch });
  t.replies.push(json({ items: [imageWire()] })); await service.refresh();
  equal(service.snapshot().items[0].revision, 1, "legacy image revision");
  equal(service.snapshot().items[0].updatedAt, service.snapshot().items[0].createdAt, "legacy image updated date");
  equal(service.snapshot().items[0].retentionSeconds, 1800, "legacy image duration");
  for (const patch of [{ retention_seconds: -1 }, { revision: 0 }, { revision: 1.5 }, { updated_at: "invalid" }, { retention_seconds: 0 }, { retention_seconds: 1800, expires_at: null }]) {
    t.replies.push(json({ items: [imageWire(patch)] })); await service.refresh();
    equal(service.snapshot().status, "unavailable", "invalid image metadata rejected");
  }
  t.replies.push(json({ items: [imageWire({ retention_seconds: 0, expires_at: null, revision: 5 })] })); await service.refresh();
  const record = service.snapshot().items[0];
  t.replies.push(new Response(new Blob(["png"], { type: "image/png" })));
  equal((await service.file(record)).size, 3, "no-expiry image can be downloaded");
  service.dispose();
}]);

cases.push(["image mutations preserve protocol, refresh duplicates and surface conflicts", async () => {
  const t = transport(); const service = new ClipboardImages({ fetch: t.fetch, csrf: () => "csrf", origin: "phone" });
  const permanent = imageWire({ expires_at: null, retention_seconds: 0, revision: 4 });
  t.replies.push(json(permanent, 200), json({ items: [permanent] }));
  await service.add(new Blob(["png"], { type: "image/png" }), 0);
  const headers = t.calls[0].init.headers as Record<string, string>;
  equal(headers["X-Persea-Clipboard-Retention"], "0", "explicit no expiry is not omitted");
  equal(headers["X-Persea-CSRF"], "csrf", "image upload CSRF");
  equal(service.snapshot().items.length, 1, "duplicate 200 response reconciles one server item");
  const record = service.snapshot().items[0];
  t.replies.push(json({}, 412), json({ items: [imageWire({ expires_at: null, retention_seconds: 0, revision: 5 })] }));
  let error: unknown;
  try { await service.setRetention(record, 604800); } catch (caught) { error = caught; }
  ok(error instanceof ClipboardImageError && error.status === 412, "image conflict remains typed");
  ok(clipboardImageErrorMessage(error).includes("changed on another device"), "conflict is intelligible");
  equal(body(t.calls[2]), { retention_seconds: 604800, revision: 4 }, "image retention CAS body");
  equal(service.snapshot().items[0].revision, 5, "conflict is reconciled before completion");
  equal(t.calls.filter((call) => call.init.method === "PATCH").length, 1, "conflict never silently retries");
  t.replies.push(json(permanent, 201), json({ items: [permanent] })); await service.add(new Blob(["png"], { type: "image/png" }));
  equal((t.calls[4].init.headers as Record<string, string>)["X-Persea-Clipboard-Retention"], undefined, "omission uses server default");
  service.dispose();
}]);

cases.push(["image stale read cannot overwrite a completed mutation", async () => {
  const t = transport(); const service = new ClipboardImages({ fetch: t.fetch, csrf: () => "csrf" });
  t.replies.push(json({ items: [imageWire()] })); await service.refresh();
  const stale = deferred(); t.replies.push(stale.promise); const loading = service.refresh();
  const current = imageWire({ revision: 2, updated_at: "2026-09-07T00:01:00Z", retention_seconds: 86400, expires_at: "2026-09-08T00:01:00Z" });
  t.replies.push(json(current), json({ items: [current] }));
  const saving = service.setRetention(service.snapshot().items[0], 86400); await tick();
  const observed: number[] = []; const unsubscribe = service.subscribe((snapshot) => { if (snapshot.status === "ready") observed.push(snapshot.items[0]?.revision ?? 0); });
  stale.resolve(json({ items: [] })); await loading; await saving;
  ok(!observed.includes(0), "stale pre-save list must not publish after mutation");
  equal(service.snapshot().items[0].revision, 2, "reconciled image revision");
  unsubscribe(); service.dispose();
}]);

cases.push(["preferences exact wire grammar and no guessed write authority", async () => {
  const t = transport(); const service = new ClipboardPreferencesService({ fetch: t.fetch });
  equal(await service.setDefault(86400), "unavailable", "fallback default cannot authorize a write");
  equal(t.calls.length, 0, "no request before load");
  for (const [value, etag] of [
    [{ version: 1, default_retention_seconds: 1800, revision: 1, extra: true }, '"1"'],
    [{ version: 2, default_retention_seconds: 1800, revision: 1 }, '"1"'],
    [{ version: 1, default_retention_seconds: 42, revision: 1 }, '"1"'],
    [{ version: 1, default_retention_seconds: 1800, revision: 0 }, '"0"'],
    [{ version: 1, default_retention_seconds: 1800, revision: 1 }, 'W/"1"'],
    [{ version: 1, default_retention_seconds: 1800, revision: 1 }, '"2"'],
  ] as const) {
    t.replies.push(json(value, 200, { ETag: etag })); await service.load();
    equal(service.snapshot().status, "unavailable", "bad preferences/ETag fail closed");
  }
  t.replies.push(preference(0, 2)); await service.load();
  equal(service.snapshot(), { status: "ready", defaultRetentionSeconds: 0, revision: 2 }, "no expiry default parses");
  service.dispose();
}]);

cases.push(["preferences single-flight and stale read fencing across save", async () => {
  const t = transport(); const service = new ClipboardPreferencesService({ fetch: t.fetch, csrf: () => "csrf" });
  const initial = deferred(); t.replies.push(initial.promise);
  const first = service.load(), joined = service.load(); ok(first === joined, "load promises coalesce");
  equal(t.calls.length, 1, "one initial request"); initial.resolve(preference(1800, 1)); await first;
  const stale = deferred(); t.replies.push(stale.promise); const oldRead = service.load();
  t.replies.push(preference(86400, 2), preference(86400, 2));
  const saved = service.setDefault(86400); await tick();
  equal(service.snapshot().defaultRetentionSeconds, 86400, "save response becomes current");
  const values: number[] = []; const off = service.subscribe((snapshot) => { values.push(snapshot.defaultRetentionSeconds); });
  const joinedSave = service.load();
  stale.resolve(preference(1800, 1)); await oldRead; equal(await saved, "ok", "save succeeds"); await joinedSave;
  ok(!values.includes(1800), "stale read cannot undo saved default");
  equal(t.calls.map((call) => call.init.method), ["GET", "GET", "PUT", "GET"], "post-save refresh follows stale read, no overlap");
  equal(body(t.calls[2]), { default_retention_seconds: 86400, revision: 1 }, "preference CAS exact payload");
  equal((t.calls[2].init.headers as Record<string, string>)["X-Persea-CSRF"], "csrf", "preferences CSRF");
  equal(service.snapshot().revision, 2, "final default authority"); off(); service.dispose();
}]);

cases.push(["preferences conflict refreshes without retry and disposed reads are inert", async () => {
  const t = transport(); const service = new ClipboardPreferencesService({ fetch: t.fetch, csrf: () => "csrf" });
  t.replies.push(preference(1800, 1)); await service.load();
  t.replies.push(json({}, 412), preference(2592000, 4));
  equal(await service.setDefault(14400), "conflict", "stale default reports conflict");
  equal(service.snapshot().defaultRetentionSeconds, 2592000, "winner is loaded");
  equal(t.calls.filter((call) => call.init.method === "PUT").length, 1, "no automatic stale retry");
  const pending = deferred(); t.replies.push(pending.promise); const loading = service.load(); service.dispose();
  pending.resolve(preference(0, 5)); await loading;
  equal(service.snapshot().defaultRetentionSeconds, 2592000, "disposed response cannot publish");
}]);

cases.push(["visible preference consumers reload shared authority", async () => {
  const t = transport(); const visibility = Object.assign(new EventTarget(), { visibilityState: "visible" });
  const service = new ClipboardPreferencesService({ fetch: t.fetch, document: visibility as unknown as Document });
  const off = service.subscribe(() => undefined);
  t.replies.push(preference(14400, 3)); visibility.dispatchEvent(new Event("visibilitychange")); await tick();
  equal(service.snapshot().defaultRetentionSeconds, 14400, "foreground reloads shared setting");
  off(); visibility.dispatchEvent(new Event("visibilitychange")); equal(t.calls.length, 1, "no visible consumers means no read");
  service.dispose();
}]);

async function main(): Promise<void> {
  for (const [name, run] of cases) { await run(); console.log(`PASS ${name}`); }
  console.log(`Clipboard clients: ${cases.length} cases passed`);
}
void main().catch((error) => { console.error(error); throw error; });
