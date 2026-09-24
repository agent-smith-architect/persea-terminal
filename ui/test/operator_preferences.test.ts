const assert = {
  equal<T>(actual: T, expected: T, message?: string): void {
    if (actual !== expected) throw new Error(`${message ?? "assertion failed"}: ${String(actual)} !== ${String(expected)}`);
  },
  deepEqual(actual: unknown, expected: unknown, message?: string): void {
    if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message ?? "deep equality failed"}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
  },
  doesNotMatch(value: string, pattern: RegExp): void { if (pattern.test(value)) throw new Error(`${JSON.stringify(value)} matched ${pattern}`); },
  async rejects(promise: Promise<unknown>, predicate: (error: unknown) => boolean, message?: string): Promise<void> {
    try { await promise; } catch (error) {
      if (predicate(error)) return;
      throw new Error(message ?? `unexpected rejection: ${String(error)}`);
    }
    throw new Error(message ?? "expected rejection");
  },
};

declare const process: { exitCode: number };

import {
  DEFAULT_COMPOSER_FONT_SIZE,
  DEFAULT_OPERATOR_PREFERENCES,
  OPERATOR_PREFERENCES_HINT_KEY,
  OperatorPreferencesService,
  type OperatorPreferences,
} from "../src/operator_preferences";

class MemoryStorage implements Pick<Storage, "getItem" | "setItem" | "removeItem"> {
  readonly values = new Map<string, string>();
  getItem(key: string): string | null { return this.values.get(key) ?? null; }
  setItem(key: string, value: string): void { this.values.set(key, value); }
  removeItem(key: string): void { this.values.delete(key); }
}

type Stored = { preferences: OperatorPreferences; revision: number; stored: boolean; available: boolean };

function deferred<T>(): Readonly<{
  promise: Promise<T>;
  resolve(value: T): void;
  reject(reason: unknown): void;
}> {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return Object.freeze({ promise, resolve, reject });
}

function storedResponse(value: Stored, status = 200): Response {
  return new Response(JSON.stringify({
    version: value.preferences.version,
    theme: value.preferences.theme,
    font_size: value.preferences.fontSize,
    composer_font_size: value.preferences.composerFontSize,
    default_session: value.preferences.defaultSession,
    revision: value.revision,
    stored: value.stored,
    available: value.available,
  }), { status, headers: { "Content-Type": "application/json", ETag: `"${value.revision}"`, "Cache-Control": "no-store" } });
}

function server(initial: Stored = {
  preferences: DEFAULT_OPERATOR_PREFERENCES,
  revision: 0,
  stored: false,
  available: true,
}) {
  let current = structuredClone(initial);
  const calls: Array<{ input: string; init: RequestInit }> = [];
  const response = (status: number) => new Response(JSON.stringify({
    version: current.preferences.version,
    theme: current.preferences.theme,
    font_size: current.preferences.fontSize,
    composer_font_size: current.preferences.composerFontSize,
    default_session: current.preferences.defaultSession,
    revision: current.revision,
    stored: current.stored,
    available: current.available,
  }), { status, headers: { "Content-Type": "application/json", ETag: `"${current.revision}"`, "Cache-Control": "no-store" } });
  return {
    calls,
    current: () => structuredClone(current),
    set(next: Stored) { current = structuredClone(next); },
    fetch: async (input: string, init: RequestInit): Promise<Response> => {
      calls.push({ input, init });
      if (input !== "/api/preferences") return new Response("wrong route", { status: 404 });
      if ((init.method ?? "GET") === "GET") return response(200);
      const headers = new Headers(init.headers);
      assert.equal(headers.get("X-Persea-CSRF"), "csrf-token");
      assert.equal(headers.get("Content-Type"), "application/json");
      assert.equal(headers.get("If-Match"), `"${current.revision}"`);
      assert.equal(init.cache, "no-store");
      assert.equal(init.credentials, "same-origin");
      const body = JSON.parse(String(init.body)) as Record<string, unknown>;
      current = {
        preferences: {
          version: 1,
          theme: body.theme as OperatorPreferences["theme"],
          fontSize: body.font_size as number | null,
          // The server's own rule: a plain number with a default, so a body
          // that omits it means the default rather than a refusal.
          composerFontSize: (body.composer_font_size as number | undefined) ?? DEFAULT_COMPOSER_FONT_SIZE,
          defaultSession: body.default_session as OperatorPreferences["defaultSession"],
        },
        revision: current.revision + 1,
        stored: true,
        available: true,
      };
      return response(200);
    },
  };
}

async function main(): Promise<void> {
{
  const storage = new MemoryStorage();
  storage.setItem(OPERATOR_PREFERENCES_HINT_KEY, JSON.stringify({ version: 1, theme: "dracula", font_size: 18, default_session: null }));
  const remote = server({
    preferences: { version: 1, theme: "rose-pine", fontSize: 16, composerFontSize: DEFAULT_COMPOSER_FONT_SIZE, defaultSession: { realm: "example-realm", server: "default", name: "alpha" } },
    revision: 7, stored: true, available: true,
  });
  const service = new OperatorPreferencesService({ storage, fetch: remote.fetch, csrf: () => "csrf-token" });
  assert.equal(service.snapshot().preferences.theme, "dracula", "the device hint did not paint synchronously");
  assert.equal(service.snapshot().status, "hint");
  await service.load();
  assert.deepEqual(service.snapshot().preferences, remote.current().preferences, "server authority did not replace the hint");
  assert.equal(service.snapshot().status, "ready");
  assert.equal(service.stats().gets, 1);
  await service.load();
  assert.equal(service.stats().gets, 1, "load was not single-flight/idempotent");
}

{
  const displayName = `release shell ${"x".repeat(80)}`;
  const remote = server({ preferences: {
    version: 1, theme: "default", fontSize: 14, composerFontSize: DEFAULT_COMPOSER_FONT_SIZE,
    defaultSession: { realm: "example-realm", server: "default", name: displayName },
  }, revision: 3, stored: true, available: true });
  const service = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token" });
  await service.load();
  assert.equal(service.snapshot().preferences.defaultSession?.name, displayName, "the client narrowed the server's valid tmux display-name grammar");
}

{
  const remote = server();
  const first = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token" });
  const original = remote.fetch;
  let staleSeen = false;
  const staleFetch = async (input: string, init: RequestInit): Promise<Response> => {
    if ((init.method ?? "GET") === "PUT" && !staleSeen) {
      staleSeen = true;
      remote.calls.push({ input, init });
      const current = remote.current();
      return new Response(JSON.stringify({
        version: 1, theme: current.preferences.theme, font_size: current.preferences.fontSize,
        composer_font_size: current.preferences.composerFontSize,
        default_session: current.preferences.defaultSession, revision: current.revision, stored: true, available: true,
      }), { status: 412, headers: { ETag: `"${current.revision}"`, "Content-Type": "application/json", "Cache-Control": "no-store" } });
    }
    return original(input, init);
  };
  const second = new OperatorPreferencesService({ fetch: staleFetch, csrf: () => "csrf-token" });
  await Promise.all([first.load(), second.load()]);
  assert.equal(await first.update({ theme: "rose-pine", fontSize: 17, defaultSession: { realm: "r", server: "s", name: "n" } }), "saved");
  const firstPut = remote.calls.find((call) => (call.init.method ?? "GET") === "PUT");
  assert.equal(new Headers(firstPut?.init.headers).get("If-Match"), '"0"', "profile A did not send the exact strong revision");

  // Make the second profile's exact revision stale. Its PUT must receive the
  // current record and must not retry behind the operator's back.
  const putsBefore = second.stats().puts;
  assert.equal(await second.update({ theme: "one-dark" }), "conflict");
  assert.equal(second.stats().puts, putsBefore + 1, "a conflict was silently retried");
  assert.equal(second.snapshot().preferences.theme, "rose-pine", "the 412 authority was not applied immediately");
  assert.equal(second.snapshot().status, "conflict");
  assert.equal(await second.update({ theme: "one-dark" }), "saved", "the next deliberate edit at the fresh revision failed");
  assert.equal(second.snapshot().revision, 2);
  const secondPuts = second.stats().puts;
  assert.equal(secondPuts, 2, "profile B did not make exactly the stale edit and one deliberate retry");
  const lastPut = remote.calls.filter((call) => (call.init.method ?? "GET") === "PUT").at(-1);
  assert.equal(new Headers(lastPut?.init.headers).get("If-Match"), '"1"', "profile B's deliberate retry did not use the applied server revision");
  const fresh = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token" });
  await fresh.load();
  assert.equal(fresh.snapshot().revision, 2, "a fresh profile did not converge on N+2");
  assert.equal(fresh.snapshot().preferences.theme, "one-dark");
}

{
  const listeners: string[] = [];
  const remote = server();
  const service = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token" });
  const first = service.subscribe((snapshot) => listeners.push(`${snapshot.generation}:${snapshot.status}:${snapshot.preferences.theme}`));
  await service.load();
  const late: string[] = [];
  const second = service.subscribe((snapshot) => late.push(`${snapshot.generation}:${snapshot.preferences.theme}`));
  assert.equal(late.length, 1, "late subscription did not receive current authority synchronously");
  first.dispose();
  await service.update({ theme: "gruvbox-dark" });
  assert.equal(listeners.length, 2, "a disposed controller received another preference publication");
  assert.equal(late.at(-1)?.endsWith(":gruvbox-dark"), true);
  second.dispose();
}

{
  // Transport and parsing are fallible preparation. The one
  // visible transition is a synchronous publication after the complete strong
  // CAS record exists; no subscriber may observe a half-applied identity.
  const remote = server();
  let releasePut: ((response: Response) => void) | undefined;
  const gatedFetch = async (input: string, init: RequestInit): Promise<Response> => {
    if ((init.method ?? "GET") !== "PUT") return remote.fetch(input, init);
    remote.calls.push({ input, init });
    return new Promise<Response>((resolve) => { releasePut = resolve; });
  };
  const service = new OperatorPreferencesService({ fetch: gatedFetch, csrf: () => "csrf-token" });
  await service.load();
  const publications: Array<{ theme: string; operationResolved: boolean }> = [];
  let operationResolved = false;
  service.subscribe((snapshot) => publications.push({ theme: snapshot.preferences.theme, operationResolved }));
  const operation = service.update({
    theme: "rose-pine-dawn",
    defaultSession: { realm: "example-realm", server: "default", name: "alpha" },
  }).then((outcome) => { operationResolved = true; return outcome; });
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(service.snapshot().preferences.theme, "default", "fallible PUT preparation published before it settled");
  if (releasePut === undefined) throw new Error("the gated preference PUT was not reached");
  releasePut(new Response(JSON.stringify({
    version: 1, theme: "rose-pine-dawn", font_size: 14, composer_font_size: DEFAULT_COMPOSER_FONT_SIZE,
    default_session: { realm: "example-realm", server: "default", name: "alpha" },
    revision: 1, stored: true, available: true,
  }), { status: 200, headers: { ETag: '"1"', "Content-Type": "application/json", "Cache-Control": "no-store" } }));
  assert.equal(await operation, "saved");
  assert.deepEqual(publications.at(-1), { theme: "rose-pine-dawn", operationResolved: false }, "the commit was deferred past operation completion");
  assert.equal(remote.calls.every((call) => call.input === "/api/preferences"), true, "default_session escaped the preference authority endpoint");
}

{
  // The commit primitive includes synchronous observer publication. An
  // await/queue inserted after the validated identity would turn this into an
  // unowned later exception (and let update report success first), rather than
  // the same authoritative commit operation.
  const remote = server();
  const service = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token" });
  await service.load();
  const sentinel = new Error("synchronous preference observer");
  service.subscribe((snapshot) => {
    if (snapshot.preferences.theme === "dracula") throw sentinel;
  });
  await assert.rejects(service.update({ theme: "dracula" }), (error) => error === sentinel);
}

{
  const storage = new MemoryStorage();
  const unreachable = new OperatorPreferencesService({
    storage,
    fetch: async () => { throw new Error("secret /var/lib/persea-terminal/preferences.json"); },
    csrf: () => "csrf-token",
  });
  await unreachable.load();
  assert.equal(unreachable.snapshot().status, "unavailable");
  assert.deepEqual(unreachable.snapshot().preferences, DEFAULT_OPERATOR_PREFERENCES);
  assert.doesNotMatch(unreachable.snapshot().message, /\/var\/|secret/);
  assert.equal(await unreachable.update({ theme: "catppuccin-mocha" }), "unavailable");
  assert.equal(unreachable.snapshot().preferences.theme, "catppuccin-mocha", "degraded page-only preference did not remain usable");
}

{
  const remote = server();
  const service = new OperatorPreferencesService({
    fetch: async (input, init) => (init?.method === "PUT"
      ? new Response("preferences unavailable", { status: 503 })
      : remote.fetch(String(input), init ?? {})),
    csrf: () => "csrf-token",
  });
  await service.load();
  assert.equal(await service.update({ theme: "dracula", fontSize: 15 }), "unavailable");
  assert.equal(service.snapshot().status, "unavailable");
  assert.equal(service.snapshot().preferences.theme, "dracula", "failed write discarded the page-only theme intent");
  assert.equal(service.snapshot().preferences.fontSize, 15, "failed write discarded the page-only font intent");
}

{
  const malformed = new OperatorPreferencesService({
    fetch: async () => new Response(JSON.stringify({ version: 1, theme: "unknown", font_size: 99 }), { status: 200, headers: { ETag: 'W/"0"' } }),
    csrf: () => "csrf-token",
  });
  await malformed.load();
  assert.equal(malformed.snapshot().status, "unavailable");
  assert.deepEqual(malformed.snapshot().preferences, DEFAULT_OPERATOR_PREFERENCES);
}

{
  // A subscriber exception is a subscriber fault, never a transport fault. The
  // PUT below commits at revision 1 and a sibling observer then throws out of
  // the same publication. The rejection must reach the direct caller (the
  // synchronous-commit contract above), and nothing else may change: the client
  // keeps the committed record and its strong revision, no false "unavailable"
  // is published, and the NEXT save still issues a PUT with If-Match: "1".
  let revision = 0;
  let stored = { theme: "default", font_size: 14, composer_font_size: DEFAULT_COMPOSER_FONT_SIZE };
  const ifMatch: Array<string | null> = [];
  const body = () => JSON.stringify({
    version: 1, theme: stored.theme, font_size: stored.font_size, composer_font_size: stored.composer_font_size,
    default_session: null, revision, stored: true, available: true,
  });
  const headers = () => ({ "Content-Type": "application/json", ETag: `"${revision}"`, "Cache-Control": "no-store" });
  const service = new OperatorPreferencesService({
    csrf: () => "csrf-token",
    fetch: async (_input: string, init: RequestInit): Promise<Response> => {
      if ((init.method ?? "GET") === "GET") return new Response(body(), { status: 200, headers: headers() });
      const sent = new Headers(init.headers).get("If-Match");
      ifMatch.push(sent);
      if (sent !== `"${revision}"`) return new Response(body(), { status: 412, headers: headers() });
      const patch = JSON.parse(String(init.body)) as { theme: string; font_size: number; composer_font_size: number };
      stored = { theme: patch.theme, font_size: patch.font_size, composer_font_size: patch.composer_font_size };
      revision += 1;
      return new Response(body(), { status: 200, headers: headers() });
    },
  });
  const published: Array<[string, string, number]> = [];
  service.subscribe((snapshot) => published.push([snapshot.status, snapshot.preferences.theme, snapshot.revision]));
  const sentinel = new Error("sibling preference observer");
  // The sibling fails once, on the publication that commits "dracula", and is
  // healthy afterwards: the second save then shows what the client kept.
  let faulty = true;
  service.subscribe((snapshot) => {
    if (faulty && snapshot.preferences.theme === "dracula") { faulty = false; throw sentinel; }
  });
  await service.load();

  const first = await service.update({ theme: "dracula" }).then(
    (outcome) => `RESOLVED:${outcome}`,
    (error) => (error === sentinel ? "REJECTED:sentinel" : `REJECTED:${String(error)}`),
  );
  assert.equal(first, "REJECTED:sentinel",
    "the subscriber fault did not reach the caller of the operation that committed");
  assert.equal(service.snapshot().status, "ready", "a subscriber fault was reported as a transport failure");
  assert.equal(service.snapshot().revision, revision, "the client CAS revision fell behind the record it committed");
  assert.equal(service.snapshot().preferences.theme, "dracula", "the committed record was replaced by a page-only value");
  assert.equal(service.snapshot().available, true, "a subscriber fault disabled preference persistence");
  assert.equal(published.some(([status]) => status === "unavailable"), false,
    `a false unavailable was published: ${JSON.stringify(published)}`);

  assert.equal(await service.update({ fontSize: 15 }), "saved", "the save after a subscriber fault was a page-only no-op");
  assert.deepEqual(ifMatch, ['"0"', '"1"'], "the save after a subscriber fault did not carry the server revision");
  assert.equal(revision, 2);
  assert.deepEqual(stored, { theme: "dracula", font_size: 15, composer_font_size: DEFAULT_COMPOSER_FONT_SIZE }, "the server record lost the committed write");
}

for (const etag of [null, 'W/"0"', "*", '"0", "1"']) {
  const service = new OperatorPreferencesService({
    fetch: async () => new Response(JSON.stringify({
      version: 1, theme: "default", font_size: 14, composer_font_size: DEFAULT_COMPOSER_FONT_SIZE, default_session: null,
      revision: 0, stored: false, available: true,
    }), { status: 200, headers: etag === null ? {} : { ETag: etag } }),
    csrf: () => "csrf-token",
  });
  await service.load();
  assert.equal(service.snapshot().status, "unavailable", `non-strong ETag ${String(etag)} did not fail closed`);
}

// The font size is a tri-state. `null` is a stored
// state that the service must read, merge and write without collapsing it into
// a number, and without spending a number in 9…24 to represent it.
{
  assert.equal(DEFAULT_OPERATOR_PREFERENCES.fontSize, null, "the shipped default is not auto");
}
{
  // the composer face is a plain number with a default, one step below the
  // 14px the stylesheet used to hardcode for a fine pointer.
  assert.equal(DEFAULT_OPERATOR_PREFERENCES.composerFontSize, 11, "the shipped composer face default moved");
  assert.equal(DEFAULT_COMPOSER_FONT_SIZE, 11, "the exported composer face default moved");
}
{
  // A record the server states carries the face, and it reaches the snapshot.
  const remote = server({
    preferences: { version: 1, theme: "dracula", fontSize: null, composerFontSize: 18, defaultSession: null },
    revision: 4, stored: true, available: true,
  });
  const service = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token", storage: new MemoryStorage() });
  await service.load();
  assert.equal(service.snapshot().status, "ready", "a record carrying composer_font_size did not parse");
  assert.equal(service.snapshot().preferences.composerFontSize, 18, "the stored composer face did not reach the snapshot");
  // And a write states it on the wire, unchanged fields included.
  assert.equal(await service.update({ theme: "one-dark" }), "saved", "an unrelated write failed");
  const put = remote.calls.filter((call) => (call.init.method ?? "GET") === "PUT").at(-1);
  const body = JSON.parse(String(put?.init.body)) as Record<string, unknown>;
  assert.equal(body.composer_font_size, 18, "a write that did not touch the composer face dropped it from the record");
  assert.equal(await service.update({ composerFontSize: 11 }), "saved", "setting the composer face failed");
  assert.equal(service.snapshot().preferences.composerFontSize, 11, "the new composer face did not reach the snapshot");
}
{
  // A response missing the field is not a record this release can read: the
  // wire is exact, and a half-known record would be published as authority.
  const service = new OperatorPreferencesService({
    storage: new MemoryStorage(),
    csrf: () => "csrf-token",
    fetch: async () => new Response(JSON.stringify({
      version: 1, theme: "dracula", font_size: null, default_session: null, revision: 2, stored: true, available: true,
    }), { status: 200, headers: { "Content-Type": "application/json", ETag: '"2"', "Cache-Control": "no-store" } }),
  });
  await service.load();
  assert.equal(service.snapshot().status, "unavailable", "a record without composer_font_size was accepted as authority");
}
{
  // A device hint written before the field existed is still worth its theme:
  // the missing key reads as the default rather than voiding the hint.
  const storage = new MemoryStorage();
  storage.setItem(OPERATOR_PREFERENCES_HINT_KEY, JSON.stringify({ version: 1, theme: "gruvbox-dark", font_size: 18, default_session: null }));
  const service = new OperatorPreferencesService({ storage, fetch: async () => new Response("", { status: 503 }), csrf: () => "csrf-token" });
  assert.equal(service.snapshot().preferences.theme, "gruvbox-dark", "a hint without composer_font_size was discarded whole");
  assert.equal(service.snapshot().preferences.composerFontSize, 11, "a hint without composer_font_size did not read the composer face as the default");
}
{
  // A record whose font is auto parses, and reads back as auto.
  const remote = server({
    preferences: { version: 1, theme: "dracula", fontSize: null, composerFontSize: DEFAULT_COMPOSER_FONT_SIZE, defaultSession: null },
    revision: 4, stored: true, available: true,
  });
  const service = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token", storage: new MemoryStorage() });
  await service.load();
  assert.equal(service.snapshot().status, "ready", "an auto record did not parse");
  assert.equal(service.snapshot().preferences.fontSize, null, "an auto record did not read back as auto");
}
{
  // Clearing an explicit size must reach the wire as JSON null. A merge that
  // used `??` would read the patch's null as "unchanged" and re-send 16.
  const remote = server({
    preferences: { version: 1, theme: "dracula", fontSize: 16, composerFontSize: DEFAULT_COMPOSER_FONT_SIZE, defaultSession: null },
    revision: 2, stored: true, available: true,
  });
  const storage = new MemoryStorage();
  const service = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token", storage });
  await service.load();
  assert.equal(await service.update({ fontSize: null }), "saved", "clearing the font did not save");
  const put = remote.calls.filter((call) => (call.init.method ?? "GET") === "PUT").at(-1);
  assert.equal(JSON.parse(String(put?.init.body)).font_size, null, "the PUT body did not carry auto as null");
  assert.equal(remote.current().preferences.fontSize, null, "the stored record did not become auto");
  assert.equal(service.snapshot().preferences.fontSize, null, "the published record did not become auto");
  // The presentation-only hint carries the same tri-state, so a reload paints
  // auto rather than resurrecting the cleared number.
  assert.equal(JSON.parse(String(storage.getItem(OPERATOR_PREFERENCES_HINT_KEY))).font_size, null, "the hint did not record auto");
  const rehydrated = new OperatorPreferencesService({ fetch: remote.fetch, csrf: () => "csrf-token", storage });
  assert.equal(rehydrated.snapshot().preferences.fontSize, null, "the auto hint did not rehydrate");
  assert.equal(rehydrated.snapshot().status, "hint");
  // An unrelated patch leaves auto alone.
  assert.equal(await service.update({ theme: "one-dark" }), "saved");
  assert.equal(remote.current().preferences.fontSize, null, "a theme write overwrote auto with a number");
}
{
  // A number outside the closed range is still not a record, and neither is a
  // non-integer: `null` is the only non-number the schema admits.
  for (const value of [8, 25, 14.5, "14", true, {}]) {
    const service = new OperatorPreferencesService({
      fetch: async () => new Response(JSON.stringify({
        version: 1, theme: "default", font_size: value, composer_font_size: DEFAULT_COMPOSER_FONT_SIZE, default_session: null,
        revision: 0, stored: false, available: true,
      }), { status: 200, headers: { ETag: '"0"' } }),
      csrf: () => "csrf-token",
      storage: new MemoryStorage(),
    });
    await service.load();
    assert.equal(service.snapshot().status, "unavailable", `font_size ${JSON.stringify(value)} was accepted`);
  }
}

// terminal controls correction R1: the service, not either pane, owns one tokenized
// composer preview. An older queued theme/font PUT is deliberately held,
// allowed to settle after the preview, and observed before the composer PUT
// settles. Both exact subscribers must retain the preview through that older
// publication; only the matching composer operation may remove it, for every
// terminal settlement leg.
for (const leg of ["success", "conflict", "unavailable"] as const) {
  const storage = new MemoryStorage();
  const initial: Stored = { preferences: DEFAULT_OPERATOR_PREFERENCES, revision: 3, stored: true, available: true };
  const priorAuthority: Stored = {
    preferences: { ...DEFAULT_OPERATOR_PREFERENCES, theme: "dracula", fontSize: 15 },
    revision: 4, stored: true, available: true,
  };
  const conflictAuthority: Stored = {
    preferences: { ...priorAuthority.preferences, composerFontSize: 13 },
    revision: 5, stored: true, available: true,
  };
  const firstStarted = deferred<void>();
  const secondStarted = deferred<void>();
  const firstSettlement = deferred<Response>();
  const secondSettlement = deferred<Response>();
  let puts = 0;
  const fetcher = async (_input: string, init: RequestInit): Promise<Response> => {
    if ((init.method ?? "GET") === "GET") return storedResponse(initial);
    puts += 1;
    const headers = new Headers(init.headers);
    if (puts === 1) {
      assert.equal(headers.get("If-Match"), '"3"', `${leg}: prior PUT used the wrong revision`);
      const body = JSON.parse(String(init.body)) as Record<string, unknown>;
      assert.equal(body.theme, "dracula", `${leg}: prior theme was lost`);
      assert.equal(body.font_size, 15, `${leg}: prior font was lost`);
      assert.equal(body.composer_font_size, 11, `${leg}: preview leaked into an already-started PUT`);
      firstStarted.resolve();
      return firstSettlement.promise;
    }
    assert.equal(puts, 2, `${leg}: more than the two deliberate PUTs were made`);
    assert.equal(headers.get("If-Match"), '"4"', `${leg}: composer PUT missed the prior authority`);
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    assert.equal(body.theme, "dracula", `${leg}: composer PUT rolled back the prior theme`);
    assert.equal(body.font_size, 15, `${leg}: composer PUT rolled back the prior font`);
    assert.equal(body.composer_font_size, 17, `${leg}: composer PUT did not carry the preview`);
    secondStarted.resolve();
    return secondSettlement.promise;
  };
  const service = new OperatorPreferencesService({ storage, fetch: fetcher, csrf: () => "csrf-token" });
  await service.load();
  const seenA: string[] = [];
  const seenB: string[] = [];
  service.subscribe((snapshot) => seenA.push(`${snapshot.revision}:${snapshot.preferences.theme}:${snapshot.preferences.fontSize}:${snapshot.preferences.composerFontSize}:${snapshot.status}`));
  service.subscribe((snapshot) => seenB.push(`${snapshot.revision}:${snapshot.preferences.theme}:${snapshot.preferences.fontSize}:${snapshot.preferences.composerFontSize}:${snapshot.status}`));
  const prior = service.update({ theme: "dracula", fontSize: 15 });
  await firstStarted.promise;
  const hintBefore = storage.getItem(OPERATOR_PREFERENCES_HINT_KEY);
  const token = service.preview({ composerFontSize: 17 });
  assert.equal(token !== undefined, true, `${leg}: valid composer preview was refused`);
  assert.equal(service.preview({ composerFontSize: 25 }), undefined, `${leg}: invalid preview was accepted`);
  assert.equal(service.snapshot().preferences.composerFontSize, 17, `${leg}: preview was not synchronous`);
  assert.equal(service.snapshot().status, "loading", `${leg}: preview did not name its pending state`);
  assert.equal(storage.getItem(OPERATOR_PREFERENCES_HINT_KEY), hintBefore, `${leg}: preview became hint authority`);
  const composer = service.update({ composerFontSize: 17 }, token);

  firstSettlement.resolve(storedResponse(priorAuthority));
  await secondStarted.promise;
  assert.equal(await prior, "saved", `${leg}: the prior PUT did not settle first`);
  const between = "4:dracula:15:17:loading";
  assert.equal(seenA.at(-1), between, `${leg}: subscriber A lost the preview to the prior PUT`);
  assert.equal(seenB.at(-1), between, `${leg}: subscriber B lost the preview to the prior PUT`);
  assert.deepEqual(seenA, seenB, `${leg}: the two subscribers observed different projections`);
  const authoritativeHint = JSON.parse(storage.getItem(OPERATOR_PREFERENCES_HINT_KEY) ?? "{}") as Record<string, unknown>;
  assert.equal(authoritativeHint.composer_font_size, 11, `${leg}: projected size contaminated the authoritative hint`);
  assert.equal(authoritativeHint.theme, "dracula", `${leg}: prior authority was not written to the hint`);

  if (leg === "success") {
    secondSettlement.resolve(storedResponse({ ...priorAuthority, preferences: { ...priorAuthority.preferences, composerFontSize: 17 }, revision: 5 }));
    assert.equal(await composer, "saved");
    assert.equal(seenA.at(-1), "5:dracula:15:17:ready", "success: matching settlement did not reveal stored authority");
  } else if (leg === "conflict") {
    secondSettlement.resolve(storedResponse(conflictAuthority, 412));
    assert.equal(await composer, "conflict");
    assert.equal(seenA.at(-1), "5:dracula:15:13:conflict", "conflict: matching settlement did not reveal competing authority");
  } else {
    secondSettlement.resolve(new Response("unavailable", { status: 503 }));
    assert.equal(await composer, "unavailable");
    assert.equal(seenA.at(-1), "0:dracula:15:17:unavailable", "unavailable: attempted page-only size was not retained");
  }
  assert.deepEqual(seenA, seenB, `${leg}: subscribers diverged at matching settlement`);
  assert.equal(puts, 2, `${leg}: operation count was not linear`);
}

// terminal controls Correction 2: A is already in flight, B is queued without a composer
// patch, and only then does C publish/queue its composer preview. B must build
// its full-record wire body from the authority published by A, never from C's
// projected face. Two exact subscribers retain C while B settles, while the
// cross-document hint remains at B's authoritative composer value until C
// owns its own success, conflict, or unavailable settlement.
for (const leg of ["success", "conflict", "unavailable"] as const) {
  const storage = new MemoryStorage();
  const initial: Stored = {
    preferences: DEFAULT_OPERATOR_PREFERENCES,
    revision: 10, stored: true, available: true,
  };
  const authorityA: Stored = {
    preferences: { ...DEFAULT_OPERATOR_PREFERENCES, theme: "dracula" },
    revision: 11, stored: true, available: true,
  };
  const authorityB: Stored = {
    preferences: { ...authorityA.preferences, fontSize: 15 },
    revision: 12, stored: true, available: true,
  };
  const conflictAuthority: Stored = {
    preferences: { ...authorityB.preferences, composerFontSize: 13 },
    revision: 13, stored: true, available: true,
  };
  const started = [deferred<void>(), deferred<void>(), deferred<void>()] as const;
  const settlements = [deferred<Response>(), deferred<Response>(), deferred<Response>()] as const;
  const requests: Array<Readonly<{ ifMatch: string | null; body: Record<string, unknown> }>> = [];
  const fetcher = async (_input: string, init: RequestInit): Promise<Response> => {
    if ((init.method ?? "GET") === "GET") return storedResponse(initial);
    const index = requests.length;
    requests.push(Object.freeze({
      ifMatch: new Headers(init.headers).get("If-Match"),
      body: JSON.parse(String(init.body)) as Record<string, unknown>,
    }));
    assert.equal(index < 3, true, `${leg}: more than the three deliberate PUTs were made`);
    started[index].resolve();
    return settlements[index].promise;
  };
  const service = new OperatorPreferencesService({ storage, fetch: fetcher, csrf: () => "csrf-token" });
  await service.load();
  const seenA: string[] = [];
  const seenB: string[] = [];
  service.subscribe((snapshot) => seenA.push(`${snapshot.revision}:${snapshot.preferences.theme}:${snapshot.preferences.fontSize}:${snapshot.preferences.composerFontSize}:${snapshot.status}`));
  service.subscribe((snapshot) => seenB.push(`${snapshot.revision}:${snapshot.preferences.theme}:${snapshot.preferences.fontSize}:${snapshot.preferences.composerFontSize}:${snapshot.status}`));

  const a = service.update({ theme: "dracula" });
  await started[0].promise;
  const b = service.update({ fontSize: 15 });
  const token = service.preview({ composerFontSize: 17 });
  assert.equal(token !== undefined, true, `${leg}: C preview was refused`);
  const c = service.update({ composerFontSize: 17 }, token);
  assert.equal(requests.length, 1, `${leg}: B or C bypassed the serial queue`);

  settlements[0].resolve(storedResponse(authorityA));
  await started[1].promise;
  assert.equal(await a, "saved", `${leg}: A did not settle before B started`);
  assert.equal(requests[0].ifMatch, '"10"', `${leg}: A used the wrong revision`);
  assert.equal(requests[0].body.composer_font_size, 11, `${leg}: A wire body did not retain the old composer value`);
  assert.equal(requests[1].ifMatch, '"11"', `${leg}: B missed A's authoritative revision`);
  assert.equal(requests[1].body.theme, "dracula", `${leg}: B missed A's authoritative theme`);
  assert.equal(requests[1].body.font_size, 15, `${leg}: B lost its own font patch`);
  assert.equal(requests[1].body.composer_font_size, 11, `${leg}: B wire body leaked C preview`);
  let hint = JSON.parse(storage.getItem(OPERATOR_PREFERENCES_HINT_KEY) ?? "{}") as Record<string, unknown>;
  assert.equal(hint.composer_font_size, 11, `${leg}: A hint leaked C preview before B settlement`);

  settlements[1].resolve(storedResponse(authorityB));
  await started[2].promise;
  assert.equal(await b, "saved", `${leg}: B did not settle before C started`);
  assert.equal(requests[2].ifMatch, '"12"', `${leg}: C missed B's authoritative revision`);
  assert.equal(requests[2].body.theme, "dracula", `${leg}: C rolled back A's theme`);
  assert.equal(requests[2].body.font_size, 15, `${leg}: C rolled back B's font`);
  assert.equal(requests[2].body.composer_font_size, 17, `${leg}: C did not own its explicit composer value`);
  const between = "12:dracula:15:17:loading";
  assert.equal(seenA.at(-1), between, `${leg}: subscriber A lost C preview after B settlement`);
  assert.equal(seenB.at(-1), between, `${leg}: subscriber B lost C preview after B settlement`);
  assert.deepEqual(seenA, seenB, `${leg}: the two subscribers diverged before C settlement`);
  hint = JSON.parse(storage.getItem(OPERATOR_PREFERENCES_HINT_KEY) ?? "{}") as Record<string, unknown>;
  assert.equal(hint.composer_font_size, 11, `${leg}: B hint leaked C preview before C settlement`);
  assert.equal(hint.font_size, 15, `${leg}: B hint did not publish B authority`);

  if (leg === "success") {
    settlements[2].resolve(storedResponse({
      ...authorityB,
      preferences: { ...authorityB.preferences, composerFontSize: 17 },
      revision: 13,
    }));
    assert.equal(await c, "saved");
    assert.equal(seenA.at(-1), "13:dracula:15:17:ready", "success: C did not publish its stored authority");
  } else if (leg === "conflict") {
    settlements[2].resolve(storedResponse(conflictAuthority, 412));
    assert.equal(await c, "conflict");
    assert.equal(seenA.at(-1), "13:dracula:15:13:conflict", "conflict: C did not reveal competing authority");
  } else {
    settlements[2].resolve(new Response("unavailable", { status: 503 }));
    assert.equal(await c, "unavailable");
    assert.equal(seenA.at(-1), "0:dracula:15:17:unavailable", "unavailable: C did not retain its page-only value");
  }
  assert.deepEqual(seenA, seenB, `${leg}: subscribers diverged at C settlement`);
  assert.equal(requests.length, 3, `${leg}: A/B/C operation count was not linear`);
}

// cross-document propagation. A publication
// writes the hint, which is only a SIGNAL to sibling documents; the record is
// always taken from the server by refresh(), which re-reads, publishes nothing
// for an unchanged revision, and never publishes "unavailable" for a transient
// failure. The hint itself carries no authority: a well-formed hint with a
// newer-looking record never moves a loaded record without a server read.
{
  const storage = new MemoryStorage();
  const remote = server({ preferences: { ...DEFAULT_OPERATOR_PREFERENCES, theme: "dracula" }, revision: 4, stored: true, available: true });
  const writer = new OperatorPreferencesService({ storage, fetch: remote.fetch, csrf: () => "csrf-token" });
  const reader = new OperatorPreferencesService({ storage: new MemoryStorage(), fetch: remote.fetch, csrf: () => "csrf-token" });
  await writer.load();
  await reader.load();
  const hintAfterLoad = JSON.parse(storage.getItem(OPERATOR_PREFERENCES_HINT_KEY) ?? "{}") as Record<string, unknown>;
  assert.equal(hintAfterLoad.theme, "dracula", "the hint does not paint the loaded record");
  assert.equal("revision" in hintAfterLoad, false, "the hint claims a revision it has no authority for");
  // subscribe() replays the current snapshot (revision 4) before any event.
  const seen: number[] = [];
  reader.subscribe((snapshot) => seen.push(snapshot.revision));
  assert.deepEqual(seen, [4]);
  assert.equal(await writer.update({ composerFontSize: 20 }), "saved");
  assert.equal(storage.getItem(OPERATOR_PREFERENCES_HINT_KEY) !== null, true, "no hint was written after the update");
  // The signal handler is refresh(): one authoritative read, one publication.
  const getsBefore = reader.stats().gets;
  await reader.refresh();
  assert.equal(reader.stats().gets, getsBefore + 1, "the cross-tab signal did not read the server record");
  assert.equal(reader.snapshot().revision, 5, "refresh did not pick up the newer server record");
  assert.equal(reader.snapshot().preferences.composerFontSize, 20);
  assert.equal(reader.snapshot().status, "ready");
  assert.deepEqual(seen, [4, 5], "refresh did not publish exactly once");
  await reader.refresh();
  assert.deepEqual(seen, [4, 5], "an unchanged refresh published");
  // A PUT after refresh carries the read revision, so it is not stale.
  assert.equal(await reader.update({ theme: "rose-pine" }), "saved", "the refreshed revision was stale for the next write");
  assert.equal(remote.current().revision, 6);
  // Hostile hint: a same-origin hint that names a different
  // profile's record with a higher revision paints before load and is then
  // replaced by the server record; after load it is never consulted at all.
  const hostileHint = JSON.stringify({ version: 1, theme: "dracula", font_size: null, composer_font_size: 24, default_session: null });
  const hostileStorage = new MemoryStorage();
  hostileStorage.setItem(OPERATOR_PREFERENCES_HINT_KEY, hostileHint);
  const hosted = new OperatorPreferencesService({ storage: hostileStorage, fetch: remote.fetch, csrf: () => "csrf-token" });
  assert.equal(hosted.snapshot().preferences.theme, "dracula", "the pre-load paint ignored the device hint");
  assert.equal(hosted.snapshot().status, "hint");
  await hosted.load();
  assert.equal(hosted.snapshot().preferences.theme, "rose-pine", "the loaded record did not replace the hostile hint");
  assert.equal(hosted.snapshot().revision, 6);
  // A hint that claims authority (any revision field) is not even a paint:
  // the schema is closed, so a record-shaped hint is rejected outright.
  const claimingStorage = new MemoryStorage();
  claimingStorage.setItem(OPERATOR_PREFERENCES_HINT_KEY, JSON.stringify({ version: 1, theme: "dracula", font_size: null, composer_font_size: 24, default_session: null, revision: 99 }));
  const claiming = new OperatorPreferencesService({ storage: claimingStorage, fetch: remote.fetch, csrf: () => "csrf-token" });
  assert.equal(claiming.snapshot().preferences.theme, "default", "a hint claiming a revision was painted");
  // After load, the hint is never consulted: a hostile hint present when the
  // signal fires still yields the server record, through a read.
  hostileStorage.setItem(OPERATOR_PREFERENCES_HINT_KEY, hostileHint);
  const hostedGets = hosted.stats().gets;
  await hosted.refresh();
  assert.equal(hosted.stats().gets, hostedGets + 1, "the signal was answered without a server read");
  assert.equal(hosted.snapshot().preferences.theme, "rose-pine", "a higher-revision hint moved a loaded record");
  assert.equal(hosted.snapshot().revision, 6);
  assert.equal("adoptHint" in hosted, false, "a no-read adoption path exists");
  // refresh() before load is a no-op; a failed refresh keeps the record.
  remote.set({ ...remote.current(), available: false });
  const failing = new OperatorPreferencesService({ storage: new MemoryStorage(), fetch: async () => { throw new Error("offline"); }, csrf: () => "csrf-token" });
  assert.equal(await failing.refresh(), undefined, "refresh before load did not resolve");
  assert.equal(failing.stats().gets, 0, "refresh before load made a request");
  remote.set({ ...remote.current(), available: true });
  let offline = false;
  const flaky = new OperatorPreferencesService({ storage: new MemoryStorage(), fetch: async (input, init) => { if (offline) throw new Error("offline"); return remote.fetch(input, init); }, csrf: () => "csrf-token" });
  await flaky.load();
  assert.equal(flaky.snapshot().status, "ready");
  offline = true;
  await flaky.refresh();
  assert.equal(flaky.snapshot().status, "ready", "a failed refresh published unavailable");
  assert.equal(flaky.snapshot().revision, 6);
}
}

// A pending Promise alone does not keep Node alive. Retain one bounded event-
// loop handle so every async contract case reaches its terminal assertion.
const testKeepAlive = setInterval(() => {}, 1_000);
void main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
}).finally(() => clearInterval(testKeepAlive));
