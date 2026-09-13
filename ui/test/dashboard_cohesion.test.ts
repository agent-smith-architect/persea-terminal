import { DashboardFavorites, parseDashboardFavorites } from "../src/dashboard_favorites";
import { previewRuns } from "../src/terminal_preview";
import { readSessionDiscovery, saveSessionDiscovery } from "../src/session_discovery";

declare const process: { exitCode: number };
const assert = (condition: unknown, message: string): void => { if (!condition) throw new Error(message); };
const equal = (actual: unknown, expected: unknown, message: string): void => assert(JSON.stringify(actual) === JSON.stringify(expected), `${message}: ${JSON.stringify(actual)}`);
const scope = (id: number): string => JSON.stringify(["local", "private", "socket_path", "/tmp/favorites.sock", "boot", `$${id}`, 1000, 42, 100, 200 + id]);
const cases: Array<{ name: string; run(): void | Promise<void> }> = [];
const test = (name: string, run: () => void | Promise<void>): void => { cases.push({ name, run }); };

function server() {
  let favorites: string[] = [], revision = 0;
  const calls: Array<{ method: string; revision: string | null }> = [];
  let loseReply = false, unavailable = false;
  const response = (status = 200) => new Response(JSON.stringify({ version: 1, favorites, revision, available: true }), { status, headers: { ETag: `"${revision}"` } });
  return {
    calls, current: () => [...favorites], revision: () => revision,
    loseNextReply: () => { loseReply = true; }, unavailable: (value: boolean) => { unavailable = value; },
    fetch: async (_input: string, init: RequestInit): Promise<Response> => {
      const headers = new Headers(init.headers); calls.push({ method: init.method!, revision: headers.get("If-Match") });
      if (unavailable) throw new Error("offline");
      if (init.method === "GET") return response();
      assert(headers.get("X-Persea-CSRF") === "csrf", "mutation omitted CSRF");
      if (headers.get("If-Match") !== `"${revision}"`) return response(412);
      const body = JSON.parse(String(init.body)); favorites = body.favorites; revision++;
      if (loseReply) { loseReply = false; throw new Error("reply lost after commit"); }
      return response();
    },
  };
}

test("favorites synchronize across devices and rebase only the changed intent", async () => {
  const backend = server(); const first = new DashboardFavorites(backend.fetch, () => "csrf"), second = new DashboardFavorites(backend.fetch, () => "csrf");
  await Promise.all([first.load(), second.load()]);
  assert(await first.set(scope(1), true), "first save");
  assert(await second.set(scope(2), true), "second save after stale revision");
  equal(backend.current(), [scope(1), scope(2)], "stale device lost an unrelated favorite");
  assert(await first.set(scope(1), false), "remove across stale revision");
  equal(backend.current(), [scope(2)], "removal lost a different device's favorite");
  await second.load(true); equal(second.snapshot().favorites, [scope(2)], "authoritative read did not synchronize");
  first.dispose(); second.dispose();
});

test("device migration preserves recents and never revives a removed favorite", async () => {
  const backend = server(), service = new DashboardFavorites(backend.fetch, () => "csrf");
  const values = new Map<string, string>();
  const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
  const recent = [{ scope: scope(9), at: Date.now() }]; saveSessionDiscovery(storage, { pinned: [scope(1)], recent });
  await service.load(); await service.migrateDevicePins(storage);
  equal(backend.current(), [scope(1)], "legacy pin not migrated"); equal(readSessionDiscovery(storage).recent, recent, "migration lost recent visits");
  equal(readSessionDiscovery(storage).pinned, [], "migration left a resurrection source");
  await service.set(scope(1), false); await service.migrateDevicePins(storage);
  equal(backend.current(), [], "migration resurrected an intentionally removed favorite"); service.dispose();
});

test("failed migration retains local pins and a lost response is reconciled", async () => {
  const backend = server(), service = new DashboardFavorites(backend.fetch, () => "csrf");
  let stored = JSON.stringify({ pinned: [scope(1)], recent: [] });
  const storage = { getItem: () => stored, setItem: (_key: string, value: string) => { stored = value; } };
  await service.load(); backend.unavailable(true); await service.migrateDevicePins(storage);
  equal(readSessionDiscovery(storage).pinned, [scope(1)], "failed import discarded a device pin");
  backend.unavailable(false); await service.load(true); backend.loseNextReply();
  assert(await service.set(scope(2), true), "committed save was reported lost");
  equal(service.snapshot().favorites, [scope(2)], "lost response left stale state");
  service.dispose();
});

test("favorites deduplicate page reads and reject stale reads after a write", async () => {
  const backend = server(); const service = new DashboardFavorites(backend.fetch, () => "csrf");
  await Promise.all([service.load(), service.load(), service.load()]); await service.load();
  equal(backend.calls.filter(call => call.method === "GET").length, 1, "duplicate foreground reads");
  let release!: (response: Response) => void;
  const slow = new Promise<Response>(resolve => { release = resolve; });
  let delay = false;
  const other = new DashboardFavorites((url, init) => delay && init.method === "GET" ? slow : backend.fetch(url, init), () => "csrf");
  await other.load(); delay = true; const stale = other.load(true);
  assert(await other.set(scope(1), true), "write during read");
  release(new Response(JSON.stringify({ version: 1, favorites: [], revision: 0, available: true }), { headers: { ETag: '"0"' } })); await stale;
  equal(other.snapshot().favorites, [scope(1)], "old read overwrote completed save"); service.dispose(); other.dispose();
});

test("favorite wire records reject capabilities, duplicates and revision mismatch", () => {
  const valid = { version: 1, favorites: [scope(1)], revision: 1, available: true };
  equal(parseDashboardFavorites(valid, '"1"').favorites, [scope(1)], "valid record refused");
  for (const record of [null, [], { ...valid, favorites: ["https://example/#handle=secret"] }, { ...valid, favorites: [scope(1), scope(1)] }, { ...valid, favorites: null }, { ...valid, revision: 0.5 }, { ...valid, extra: true }]) {
    let refused = false; try { parseDashboardFavorites(record, '"1"'); } catch { refused = true; } assert(refused, "invalid record accepted");
  }
  let refused = false; try { parseDashboardFavorites(valid, '"2"'); } catch { refused = true; } assert(refused, "ETag mismatch accepted");
});

test("colored previews retain standard, indexed and RGB colors and resets", () => {
  const runs = previewRuns(['\x1b[1;31mred\x1b[22;39mplain\x1b[38;5;200mindexed', '\x1b[38;2;20;40;60mrgb\x1b[0mend']);
  equal(runs.map(run => run.text).join(""), "redplainindexed\nrgbend", "color parsing changed text");
  equal(runs[0].style, { bold: true, foreground: 1 }, "standard color missing");
  equal(runs[1].style, {}, "style reset missing");
  equal(runs[2].style.foreground, 200, "indexed color missing");
  equal(runs[3].style.foreground, [20, 40, 60], "RGB color missing");
  equal(runs[4].style, {}, "full reset missing");
  equal(previewRuns(['\x1b[38:2::20:40:60mcolon'])[0].style.foreground, [20, 40, 60], "colon color missing");
});

test("preview styling refuses OSC, cursor commands and oversized captures", () => {
  for (const rows of [['\x1b]52;c;c2VjcmV0\a'], ['\x1b[2Jhidden'], ['\x1bPcommand\x1b\\'], ['\u009bsecret'], ['x'.repeat(8193)], Array(42).fill('row')]) {
    let refused = false; try { previewRuns(rows); } catch { refused = true; } assert(refused, "unsafe or unbounded preview accepted");
  }
  equal(previewRuns(['<img src=x onerror=alert(1)>'])[0].text, '<img src=x onerror=alert(1)>', "markup-shaped text must remain text");
});

(async () => {
  for (const test of cases) { try { await test.run(); console.log(`PASS ${test.name}`); } catch (error) { console.error(`FAIL ${test.name}`, error); process.exitCode = 1; } }
})();
