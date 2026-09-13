'use strict';
const path = require('path'), crypto = require('crypto');
const { startFixture } = require('./unified_reopen_fixture.cjs');
const token = () => crypto.randomBytes(32).toString('base64url');
const scope = session => { const a = session.authority; return JSON.stringify([a.realm, a.server, a.selector_kind, a.selector_value, a.boot_id, a.session_id, a.uid, a.server_pid, a.server_start, a.session_created]); };
const session = (realm, n, extra = {}) => ({
  realm, server: 'default', server_status: 'ok', name: `qt${n}`, session_id: `$${n}`,
  handles: { alias: token(), observe: token(), control: token() }, width: 80, height: 48, attached: 1,
  activity: Math.floor(Date.now() / 1000) - 900, output_activity: Math.floor(Date.now() / 1000) - 70,
  authority: { realm, server: 'default', uid: realm === 'local' ? 1000 : 1001, selector_kind: 'socket_path', selector_value: `/tmp/cohesion-${realm}.sock`, boot_id: 'cohesion-fixture', server_pid: realm === 'local' ? 42 : 43, server_start: 100, session_id: `$${n}`, session_created: 200 + n },
  unified: { state: 'open', origin: 'reconstructed' }, ...extra,
});

async function startCohesionFixture(ui = path.resolve(__dirname, '..')) {
  const state = {
    requests: [], sessions: [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12].map(n => session('local', n)),
    others: [session('smith', 1, { name: 'automation', unified: { state: 'adoptable' } }), session('smith', 2, { name: 'notes' })],
    aliases: [], favorites: { version: 1, favorites: [], revision: 0, available: true },
    inventoryStatus: 200, favoritesStatus: 200, previewStatus: 200, aliasStatus: 200,
    previewDelay: 0, inventoryDelay: 0, favoriteDelay: 0, previewVersion: 1, nextID: 100, aliasCommitThenFail: false,
    preferences: { version: 1, theme: 'default', font_size: null, composer_font_size: 11, default_session: null, revision: 1, stored: true, available: true },
  };
  const json = (res, status, body, revision) => { res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...(revision === undefined ? {} : { ETag: `"${revision}"` }) }); res.end(JSON.stringify(body)); };
  const read = async req => { let body = ''; for await (const chunk of req) body += chunk; return body ? JSON.parse(body) : {}; };
  const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
  const fixture = await startFixture(ui, { tls: true, playwrightScreenshotStyle: true, clipboardHTTP: async (req, res, url) => {
    state.requests.push({ method: req.method, path: url.pathname, query: url.search, ifMatch: req.headers['if-match'], at: Date.now() });
    const request = state.requests[state.requests.length - 1];
    if (url.pathname === '/api/inventory') {
      if (state.inventoryDelay) await wait(state.inventoryDelay);
      for (const row of [...state.sessions, ...state.others]) row.handles = { alias: token(), observe: token(), control: token() };
      json(res, state.inventoryStatus, state.inventoryStatus === 200 ? { realms: [
        { name: 'local', display_name: 'local_operator', servers: [{ label: 'default', status: 'ok', can_create: true, unified_dev: { state: 'create', name: 'dev_launch' }, sessions: state.sessions }] },
        { name: 'smith', display_name: 'other_operator', servers: [{ label: 'default', status: 'ok', can_create: true, sessions: state.others }] },
      ], aliases: state.aliases } : {}); return true;
    }
    if (url.pathname === '/api/dashboard-preferences') {
      if (state.favoriteDelay) await wait(state.favoriteDelay);
      if (state.favoritesStatus !== 200) { json(res, state.favoritesStatus, {}); return true; }
      if (req.method === 'PUT') {
        const body = await read(req); request.body = body;
        if (req.headers['if-match'] !== `"${state.favorites.revision}"`) { json(res, 412, state.favorites, state.favorites.revision); return true; }
        state.favorites = { ...state.favorites, ...body, revision: state.favorites.revision + 1 };
      }
      json(res, 200, state.favorites, state.favorites.revision); return true;
    }
    if (url.pathname === '/api/preferences') {
      if (req.method === 'PUT') state.preferences = { ...state.preferences, ...await read(req), revision: state.preferences.revision + 1 };
      json(res, 200, state.preferences, state.preferences.revision); return true;
    }
    if (url.pathname === '/api/workspaces') { json(res, 200, { version: 1, items: [] }); return true; }
    if (url.pathname === '/api/session-previews') {
      if (state.previewDelay) await wait(state.previewDelay);
      const rows = ['> persea dashboard', '', '✓ Build passed', '✓ Shared favorites synchronized', `Snapshot ${state.previewVersion}`, '<img src=x onerror=alert(1)>', '', '$'];
      const ansi_rows = ['\x1b[36m> persea dashboard\x1b[0m', '', '\x1b[32m✓ Build passed\x1b[0m', '\x1b[32m✓ Shared favorites synchronized\x1b[0m', `\x1b[38;2;240;180;75mSnapshot ${state.previewVersion}\x1b[0m`, '<img src=x onerror=alert(1)>', '', '$'];
      json(res, state.previewStatus, { rows, ansi_rows, width: 80, height: 48, captured_at: Date.now(), truncated: false }); return true;
    }
    if (url.pathname === '/api/sessions') {
      const body = await read(req); request.body = body;
      const group = body.realm === 'local' ? state.sessions : body.realm === 'smith' ? state.others : undefined;
      if (!group || body.server !== 'default') { json(res, 400, {}); return true; }
      if (group.some(row => row.name === body.name)) { res.writeHead(409); res.end('name_taken'); return true; }
      const created = session(body.realm, state.nextID++, { name: body.name }); group.push(created);
      json(res, 201, { realm: body.realm, server: body.server, name: body.name, session_id: created.session_id }); return true;
    }
    if (url.pathname.startsWith('/api/aliases')) {
      const body = await read(req); request.body = body;
      if (state.aliasStatus !== 200) { json(res, state.aliasStatus, {}); return true; }
      const target = [...state.sessions, ...state.others].find(row => row.handles.alias === body.handle);
      if (!target) { json(res, 410, {}); return true; }
      if (req.method === 'POST') {
        state.aliases.push({ alias_id: crypto.randomBytes(16).toString('hex'), display_alias: body.display_alias, revision: 1, state: 'live', session_incarnation: target.authority });
        if (state.aliasCommitThenFail) { state.aliasCommitThenFail = false; json(res, 503, {}); return true; }
        json(res, 201, state.aliases[state.aliases.length - 1]); return true;
      }
      const record = state.aliases.find(alias => url.pathname.endsWith(alias.alias_id));
      if (!record || req.headers['if-match'] !== `"${record.revision}"`) { json(res, 409, {}); return true; }
      if (req.method === 'DELETE') state.aliases = state.aliases.filter(alias => alias !== record);
      else { record.display_alias = body.display_alias; record.revision++; }
      json(res, 200, record); return true;
    }
    return false;
  } });
  return { ...fixture, state };
}

module.exports = { startCohesionFixture, session, scope };
if (require.main === module) startCohesionFixture(process.env.PERSEA_DASHBOARD_UI).then(fixture => {
  console.log(`Dashboard fixture ready: ${fixture.origin}`);
  for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, () => { void fixture.close().then(() => process.exit(0)); });
}).catch(error => { console.error(error); process.exitCode = 1; });
