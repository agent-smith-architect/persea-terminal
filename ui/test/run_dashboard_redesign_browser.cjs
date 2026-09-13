'use strict';
const fs = require('fs'), path = require('path'), crypto = require('crypto');
const { startFixture } = require('./unified_reopen_fixture.cjs');
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const UI = process.env.PERSEA_DASHBOARD_UI || path.resolve(__dirname, '..');
const ENGINE = process.env.PERSEA_DASHBOARD_ENGINE || 'chromium';
const OUT = process.env.PERSEA_DASHBOARD_EVIDENCE || '/tmp/agent_logs/dashboard-redesign';
const assert = (value, message) => { if (!value) throw Error(message); };
const token = () => crypto.randomBytes(32).toString('base64url');
const authority = n => ({ realm: 'local', server: 'private', uid: 1000, selector_kind: 'socket_path', selector_value: '/tmp/dashboard-fixture.sock', boot_id: 'dashboard-fixture', server_pid: 42, server_start: 100, session_id: '$' + n, session_created: 200 + n });
const session = (n, extra = {}) => ({ handles: { alias: token(), observe: token(), control: token() }, realm: 'local', server: 'private', server_status: 'ok', session_id: '$' + n, name: 'qt' + n, width: 127, height: 30, attached: 1, activity: Math.floor(Date.now()/1000) - 9*3600, output_activity: Math.floor(Date.now()/1000) - 70, authority: authority(n), unified: { state: 'open', origin: 'reconstructed' }, ...extra });
const workspace = () => ({ workspace_id: '1'.repeat(32), name: 'Operations', normalized_name: 'OPERATIONS', revision: 1, created_at: '2026-09-08T10:00:00Z', updated_at: '2026-09-08T10:00:00Z', tree: { kind: 'leaf', session: { realm: 'local', server: 'private', name: 'qt1' }, on_missing: 'offer' } });
async function start() {
  const state = { requests: [], sessions: [10, 2, 1, 3, 4, 5, 6, 7, 8, 9, 11, 12].map(n => session(n)), workspace: workspace(), inventoryStatus: 200, workspaceStatus: 200, delayWorkspace: 0, aliases: [], preferences: { version: 1, theme: 'default', font_size: null, composer_font_size: 11, default_session: null, revision: 1, stored: true, available: true } };
  const json = (res, code, value) => { res.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(value)); };
  const server = await startFixture(UI, { tls: true, playwrightScreenshotStyle: true, clipboardHTTP: async (req, res, url) => {
    state.requests.push({ method: req.method, path: url.pathname, ifMatch: req.headers['if-match'], at: Date.now() });
    if (url.pathname === '/api/inventory') {
      for (const row of state.sessions) row.handles = { alias: token(), observe: token(), control: token() };
      json(res, state.inventoryStatus, state.inventoryStatus === 200 ? { realms: [{ name: 'local', display_name: 'Local sessions', servers: [{ label: 'private', status: 'ok', can_create: true, sessions: state.sessions }] }], aliases: state.aliases } : { error: 'unavailable' }); return true;
    }
    if (url.pathname === '/api/preferences') {
      if (req.method === 'PUT') { let body = ''; for await(const chunk of req) body += chunk; state.preferences = { ...state.preferences, ...JSON.parse(body), revision: state.preferences.revision + 1 }; }
      res.setHeader('ETag', '"' + state.preferences.revision + '"'); json(res, 200, state.preferences); return true;
    }
    if (url.pathname === '/api/workspaces') {
      const saved = { version: 1, items: state.workspace ? [structuredClone(state.workspace)] : [] };
      if (state.delayWorkspace) await new Promise(resolve => setTimeout(resolve, state.delayWorkspace));
      json(res, state.workspaceStatus, state.workspaceStatus === 200 ? saved : { error: 'unavailable' }); return true;
    }
    if (url.pathname.startsWith('/api/workspaces/')) {
      if (req.headers['if-match'] !== '"' + state.workspace?.revision + '"') { json(res, 409, state.workspace); return true; }
      if (req.method === 'DELETE') { state.workspace = null; res.writeHead(204); res.end(); return true; }
      if (req.method === 'PUT') { let body = ''; for await(const chunk of req) body += chunk; const update = JSON.parse(body); state.workspace = { ...state.workspace, ...update, normalized_name: update.name.toUpperCase(), revision: state.workspace.revision + 1 }; json(res, 200, state.workspace); return true; }
    }
    if (url.pathname.startsWith('/api/session-preview')) { json(res, 200, { rows: ['Synthetic terminal preview.'], captured_at: Date.now(), width: 127, height: 30, truncated: false }); return true; }
    if (url.pathname.startsWith('/api/aliases')) { json(res, 409, { error: 'revision_conflict' }); return true; }
    return false;
  } });
  return { ...server, state };
}
async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const fixture = await start();
  const browser = await playwright[ENGINE].launch({ headless: true, ...(ENGINE === 'chromium' ? { executablePath: require("./browser_path.cjs")(), args: ['--no-sandbox'] } : {}) });
  const evidence = { engine: ENGINE, ui: UI, checks: [], console: [], errors: [], viewports: [] }; let phase = 'setup';
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, screen: { width: 1440, height: 1000 }, ignoreHTTPSErrors: true });
  const page = await context.newPage(); page.setDefaultTimeout(6000);
  page.on('console', message => evidence.console.push({ phase, type: message.type(), text: message.text().slice(0, 600) })); page.on('pageerror', error => evidence.errors.push({ phase, text: error.message }));
  const loaded = async () => { await page.goto(fixture.origin); await page.locator('.session-card').first().waitFor(); await page.waitForFunction(() => !document.querySelector('select[aria-label="Terminal theme"]').disabled); };
  const section = async label => { const navigation = page.locator('.dashboard-navigation'); if (await navigation.count()) await navigation.getByRole('button', { name: label, exact: true }).click(); };
  const refresh = async () => { await Promise.all([page.waitForResponse(r => r.url().endsWith('/api/inventory')), page.locator('.dashboard-refresh:visible, .dashboard-workspace-refresh:visible').click()]); await page.waitForTimeout(100); };
  const check = async (name, fn) => { if (process.env.PERSEA_DASHBOARD_CASE && !process.env.PERSEA_DASHBOARD_CASE.split('|').some(filter => name.includes(filter))) return; phase = name; try { await fn(); evidence.checks.push({ name, pass: true }); } catch(error) { evidence.checks.push({ name, pass: false, error: String(error) }); } };
  try {
    await loaded();
    await check('session-first layout, truthful shared metadata, natural sort', async () => {
      assert(await page.locator('.workspace-panel').isHidden(), 'Workspaces must be separate from the initial session list');
      assert(await page.locator('.dashboard-appearance').first().isHidden(), 'Settings must be separate from Sessions');
      const names = await page.locator('.session-name').allTextContents(); assert(names.slice(0, 3).join(',') === 'qt1,qt2,qt3', 'Natural session sorting');
      assert((await page.locator('.session-metadata').first().innerText()).includes('127×30 · 1 attached · Output 1m ago'), 'Recent output must not look quiet for nine hours');
      assert(await page.locator('.session-unified-origin:visible').count() === 0, 'History provenance belongs in Details');
    });
    await check('background refresh retains the actual focused disclosure node', async () => {
      await section('Sessions'); await page.locator('.session-disclosure').first().focus();
      await page.evaluate(() => { window.focusWitness = document.activeElement; });
      const before = fixture.state.requests.filter(r => r.path === '/api/inventory').length;
      await page.waitForTimeout(61_000);
      assert(fixture.state.requests.filter(r => r.path === '/api/inventory').length > before, 'Real periodic refresh must run');
      assert(await page.evaluate(() => document.activeElement === window.focusWitness && window.focusWitness.isConnected), 'Background refresh replaced or blurred the focused node');
    });
    await check('alias draft, selection and disclosure survive return and manual refresh', async () => {
      await page.locator('.session-disclosure').first().click();
      await page.locator('.session-alias-edit').first().click();
      const editor = page.locator('.session-card').first().locator('.alias-editor input'); await editor.fill('My unsaved alias');
      await editor.evaluate(input => { input.setSelectionRange(3, 10); window.aliasWitness = input; });
      const reads = fixture.state.requests.filter(r => r.path === '/api/inventory').length;
      await page.evaluate(() => dispatchEvent(new PageTransitionEvent('pageshow'))); await page.waitForTimeout(150);
      assert(fixture.state.requests.filter(r => r.path === '/api/inventory').length === reads, 'A quick return refetched a recent inventory');
      assert(await page.evaluate(() => document.activeElement === window.aliasWitness && window.aliasWitness.value === 'My unsaved alias' && window.aliasWitness.selectionStart === 3 && window.aliasWitness.selectionEnd === 10), 'Return refresh lost the alias draft or selection');
      await page.keyboard.press('Escape');
      await refresh(); assert(await editor.inputValue() === 'My unsaved alias', 'Manual refresh lost unfocused draft');
      assert(await page.locator('.session-card').first().locator('.session-detail').isVisible(), 'Details closed on refresh');
      assert(await page.locator('.session-card').first().locator('.session-information > summary').count() === 0, 'Session information has a redundant disclosure');
      assert((await page.locator('.session-card').first().locator('.session-history-origin').innerText()).includes('Earlier output was imported from tmux'), 'Plain-language history explanation missing');
      const href = await page.locator('.session-card').first().locator('.action-unified-open').getAttribute('href');
      const current = fixture.state.sessions.find(s => s.name === 'qt1'); assert(href.includes(current.handles.control), 'Stable row kept an expired capability instead of the refreshed one');
    });
    await check('alias conflict preserves a draft and offers an explicit reload of the saved alias', async () => {
      await section('Sessions'); const card = page.locator('.session-card').filter({ has: page.locator('.session-name', { hasText: /^qt1$/ }) });
      if (await card.locator('.session-detail').isHidden()) await card.locator('.session-disclosure').click();
      await card.locator('.session-alias-edit').click();
      const input = card.locator('.alias-editor input'); await input.fill('Keep this alias draft');
      fixture.state.aliases = [{ alias_id: 'fixture-alias', display_alias: 'Saved elsewhere', revision: 2, state: 'active', session_incarnation: authority(1) }];
      await page.keyboard.press('Escape'); await refresh(); await card.locator('.session-alias-edit').click();
      assert(await input.inputValue() === 'Keep this alias draft', 'Remote alias change discarded a draft');
      await card.locator('.alias-editor button[type=submit]').click();
      await card.getByRole('button', { name: 'Keep editing', exact: true }).click(); assert(await input.inputValue() === 'Keep this alias draft', 'Conflict lost the draft');
      await card.getByRole('button', { name: 'Reload saved alias', exact: true }).click(); await page.waitForTimeout(150);
      assert(await card.locator('.alias-editor input').inputValue() === 'Saved elsewhere', 'Explicit reload failed to adopt the latest alias revision');
      await page.keyboard.press('Escape');
    });
    await check('workspace draft survives return, manual refresh and workspace failure', async () => {
      await section('Workspaces'); const draft = page.getByLabel('New workspace name', { exact: true }); await draft.fill('Unsaved workspace');
      await draft.evaluate(input => { window.workspaceWitness = input; input.setSelectionRange(2, 5); });
      await page.evaluate(() => dispatchEvent(new PageTransitionEvent('pageshow'))); await page.waitForTimeout(200);
      assert(await page.evaluate(() => document.activeElement === window.workspaceWitness && window.workspaceWitness.value === 'Unsaved workspace' && window.workspaceWitness.selectionStart === 2), 'Return refresh erased the workspace draft or focus');
      await refresh(); assert(await draft.inputValue() === 'Unsaved workspace', 'Manual refresh erased a workspace draft');
      fixture.state.workspaceStatus = 503; await refresh(); assert(await draft.inputValue() === 'Unsaved workspace', 'Workspace failure destroyed the editor'); fixture.state.workspaceStatus = 200;
      await refresh();
    });
    await check('workspace delete requires a named confirmation and keeps revision guard', async () => {
      await section('Workspaces'); const manage = page.locator('.workspace-panel__manage > summary'); if (await manage.count()) await manage.click();
      const before = fixture.state.requests.filter(r => r.method === 'DELETE').length;
      await page.locator('.workspace-panel__delete').first().click();
      assert(fixture.state.requests.filter(r => r.method === 'DELETE').length === before, 'One click deleted a workspace');
      const confirmation = page.locator('.workspace-panel__confirmation'); assert((await confirmation.innerText()).includes('Operations'), 'Confirmation must name the saved workspace');
      await confirmation.getByRole('button', { name: 'Cancel', exact: true }).click(); assert(fixture.state.workspace !== null, 'Cancel deleted the record');
      await page.locator('.workspace-panel__delete').first().click(); await confirmation.getByRole('button', { name: 'Delete Operations', exact: true }).click();
      await page.waitForFunction(() => !document.querySelector('.workspace-panel__item'));
      const request = fixture.state.requests.find(r => r.method === 'DELETE'); assert(request?.ifMatch === '"1"', 'Delete lost the revision precondition');
      assert(!fixture.state.requests.some(r => /session-(adoptions|creations)|\/api\/input|\/api\/resize/.test(r.path)), 'Dashboard inspection mutated a terminal');
      fixture.state.workspace = workspace(); await refresh();
    });
    await check('empty filter has a count and clear action', async () => {
      await section('Sessions'); await page.getByLabel('Filter sessions by name or alias').fill('no-such-session');
      assert((await page.locator('.dashboard-results').innerText()).startsWith('0 of 12'), 'No-results count missing');
      assert(await page.locator('.dashboard-empty-results').isVisible(), 'No-results explanation missing');
      await page.locator('.dashboard-empty-results button').click(); assert(await page.locator('.session-card:visible').count() === 12, 'Clear did not restore sessions');
    });
    await check('favorites persist per incarnation; a replacement session does not inherit them', async () => {
      await section('Sessions'); const card = page.locator('.session-card').filter({ has: page.locator('.session-name', { hasText: /^qt1$/ }) });
      if (await card.locator('.session-detail').isHidden()) await card.locator('.session-disclosure').click();
      await card.locator('.session-pin').click();
      await page.waitForFunction(() => [...document.querySelectorAll('.session-pin')].every(node => node.getAttribute('aria-busy') === 'false'));
      await page.reload(); await page.locator('.session-card').first().waitFor();
      await page.waitForFunction(() => !document.querySelector('.session-pin').disabled);
      await page.getByRole('button', { name: 'Favorites', exact: true }).click(); assert(await page.locator('.session-card:visible').count() === 1, 'Favorite not retained on reload');
      const row = fixture.state.sessions.find(s => s.name === 'qt1'); row.authority = { ...row.authority, session_created: row.authority.session_created + 1 };
      await refresh(); assert(await page.locator('.session-card:visible').count() === 0, 'Pin rebound to another incarnation with the same name');
      await page.getByRole('button', { name: 'All', exact: true }).click();
    });
    await check('Recent uses live identities and rejects expired or future device records', async () => {
      await section('Sessions');
      await page.evaluate(() => {
        const scope = name => [...document.querySelectorAll('.session-card')].find(card => card.querySelector('.session-name').textContent === name).dataset.sessionScope;
        localStorage.setItem('persea-terminal.session-discovery.v1', JSON.stringify({ pinned: [], recent: [{ scope: scope('qt2'), at: Date.now() - 1000 }, { scope: scope('qt1'), at: Date.now() - 31 * 86400000 }, { scope: scope('qt3'), at: Date.now() + 86400000 }, { scope: 'ended-incarnation', at: Date.now() }] }));
      });
      await page.evaluate(() => dispatchEvent(new PageTransitionEvent('pageshow'))); await page.waitForTimeout(150);
      await page.getByRole('button', { name: 'Recent', exact: true }).click();
      assert(await page.locator('.session-card:visible').count() === 1 && await page.locator('.session-card:visible .session-name').innerText() === 'qt2', 'Recent included an expired, future or missing identity');
      await page.getByRole('button', { name: 'All', exact: true }).click();
    });
    await check('Appearance finishes loading and applies the light palette', async () => {
      await section('Settings');
      const appearance = page.locator('.dashboard-appearance').first();
      if (!await appearance.evaluate(node => node.open)) await appearance.locator(':scope > summary').click();
      assert(!/Loading/.test(await page.locator('.dashboard-appearance__status').first().innerText()), 'Appearance is stuck loading after a successful read');
      await page.getByLabel('Terminal theme', { exact: true }).selectOption('rose-pine-dawn');
      await page.waitForFunction(() => getComputedStyle(document.querySelector('.dashboard')).backgroundColor === 'rgb(242, 233, 225)');
      assert(await page.evaluate(() => getComputedStyle(document.body).backgroundColor === 'rgb(242, 233, 225)'), 'Light palette does not reach outer page chrome');
      await section('Sessions');
      assert(await page.locator('.session-card').first().evaluate(node => getComputedStyle(node).backgroundColor === 'rgb(242, 233, 225)'), 'The visible session list did not receive the light palette');
      const previousPhase = phase; phase = 'screenshot'; await page.screenshot({ path: path.join(OUT, `${ENGINE}-light.png`), animations: 'disabled' }); phase = previousPhase;
      await section('Settings');
      await page.getByLabel('Terminal theme', { exact: true }).selectOption('default');
    });
    await check('responsive sweep, long names, 200% text and desktop width', async () => {
      await section('Sessions');
      for (const disclosure of await page.locator('.session-disclosure[aria-expanded="true"]').all()) await disclosure.click();
      for (const [width, height] of [[320,568],[390,844],[430,932],[844,390],[768,1024],[1440,1000],[1920,1080]]) {
        await page.setViewportSize({ width, height }); await page.evaluate(() => scrollTo(0, 0));
        const metrics = await page.evaluate(() => { const rows = [...document.querySelectorAll('.session-card')].filter(el => el.getClientRects().length); return { width: innerWidth, height: innerHeight, scrollWidth: document.documentElement.scrollWidth, rootWidth: document.querySelector('.dashboard').getBoundingClientRect().width, firstSession: rows[0].getBoundingClientRect().top, completeRows: rows.filter(el => el.getBoundingClientRect().bottom <= innerHeight).length, small: [...document.querySelectorAll('button,a[href],input,select,summary')].filter(el => el.getClientRects().length && (el.getBoundingClientRect().width < 43.9 || el.getBoundingClientRect().height < 43.9)).map(el => ({ label: el.getAttribute('aria-label') || el.textContent.slice(0,45), width: el.getBoundingClientRect().width, height: el.getBoundingClientRect().height })) }; });
        evidence.viewports.push(metrics); assert(metrics.scrollWidth <= width + 1, `Horizontal overflow at ${width}`); assert(metrics.small.length === 0, `Small targets at ${width}: ${JSON.stringify(metrics.small)}`);
        if (width >= 1440) assert(metrics.rootWidth <= 1180, 'Desktop list is unbounded');
        if (height === 568) assert(metrics.completeRows >= 1, 'No complete session visible on small phone');
        if (width === 390 || width === 1440) { const prior = phase; phase = 'screenshot'; await page.screenshot({ path: path.join(OUT, `${ENGINE}-${width}.png`), caret: 'initial', animations: 'allow' }); phase = prior; }
      }
      fixture.state.sessions[0].name = 'long_session_' + 'identity_'.repeat(14); await refresh();
      await page.setViewportSize({ width: 320, height: 700 }); await page.evaluate(() => document.documentElement.style.fontSize = '200%');
      for (const tab of ['Sessions','Workspaces','Settings']) {
        await section(tab);
        const overflow = await page.evaluate(() => ({ width: document.documentElement.scrollWidth, offenders: [...document.querySelectorAll('*')].filter(el => el.getClientRects().length && (el.getBoundingClientRect().right > innerWidth + 1 || el.scrollWidth > el.clientWidth + 2)).map(el => ({ tag: el.tagName, class: el.className, text: el.children.length ? '' : el.textContent.slice(0,60), width: el.getBoundingClientRect().width, scrollWidth: el.scrollWidth, overflow: getComputedStyle(el).overflowX, right: el.getBoundingClientRect().right })).slice(0,30) }));
        (evidence.textEnlargement ||= []).push({ tab, ...overflow });
        if (overflow.width > 321) { const prior = phase; phase = 'screenshot'; await page.screenshot({ path: path.join(OUT, `${ENGINE}-text200-${tab}.png`), caret: 'initial', animations: 'allow' }); phase = prior; }
        assert(overflow.width <= 321, `200% text overflows ${tab}: ${JSON.stringify(overflow.offenders)}`);
      }
      await page.evaluate(() => document.documentElement.style.fontSize = '');
    });
    await check('failed refresh keeps data with a clear retry and no false fresh claim', async () => {
      await section('Sessions'); fixture.state.inventoryStatus = 503; await refresh();
      assert(await page.locator('.session-card').count() === 12, 'Failure removed the last good inventory');
      const text = await page.locator('.dashboard-status').innerText(); assert(/Showing saved results/.test(text) && /retry/.test(text) && !/Updated just now/.test(text), 'Failure did not explain stale results');
      fixture.state.inventoryStatus = 200;
    });
    evidence.unexpectedConsole = evidence.console.filter(item => !(item.text.includes('503') && /failure|failed refresh/.test(item.phase)) && !(item.text.includes('409') && item.phase.startsWith('alias conflict')) && !(item.phase === 'screenshot' && /Content Security Policy|CSP|style-src/.test(item.text)));
    assert(evidence.errors.length === 0 && evidence.unexpectedConsole.length === 0, 'Unexpected browser console or page errors');
  } finally {
    evidence.finished = true; evidence.pass = evidence.checks.length > 0 && evidence.checks.every(check => check.pass) && evidence.errors.length === 0 && (evidence.unexpectedConsole?.length ?? 0) === 0;
    fs.writeFileSync(path.join(OUT, ENGINE + '.json'), JSON.stringify(evidence, null, 2) + '\n');
    await browser.close(); await fixture.close();
  }
  console.log(JSON.stringify({ engine: ENGINE, pass: evidence.pass, checks: evidence.checks.map(check => ({ name: check.name, pass: check.pass, ...(check.pass ? {} : { error: check.error }) })) }));
  if (!evidence.pass) process.exitCode = 1;
}
main().catch(error => { console.error(String(error)); process.exitCode = 1; });
