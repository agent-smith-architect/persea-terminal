'use strict';
const fs = require('fs'), path = require('path'), crypto = require('crypto');
const { startFixture } = require('./unified_reopen_fixture.cjs');
const { startWorkspaceFixture } = require('./workspace_fixture.cjs');
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve('playwright'));
const UI = process.env.PERSEA_ALIAS_UI || path.resolve(__dirname, '..');
const OUT = process.env.PERSEA_ALIAS_EVIDENCE || path.join(require('os').tmpdir(), 'persea-terminal-tests', 'run_alias_browser');
const engines = process.env.PERSEA_ALIAS_ENGINE ? [process.env.PERSEA_ALIAS_ENGINE] : ['chromium', 'webkit'];
const assert = (value, message) => { if (!value) throw new Error(message); };
const scope = a => JSON.stringify([a.realm, a.server, a.selector_kind, a.selector_value, a.boot_id, a.session_id, a.uid, a.server_pid, a.server_start, a.session_created]);

// Holds the next matching response until released, so a test can order a read
// against a save.
function holdNext() {
  const hold = {};
  hold.fetched = new Promise(resolve => { hold.markFetched = resolve; });
  hold.released = new Promise(resolve => { hold.release = resolve; });
  hold.handled = new Promise(resolve => { hold.markHandled = resolve; });
  return hold;
}
const whenEnabled = async (page, locator) => page.waitForFunction(el => !el.disabled, await locator.elementHandle());

// In a workspace the panes share one inventory read. A read that started before
// a pane saved its alias must not undo the save, and later reads still apply.
async function workspaceCase(browser, engine, evidence) {
  const fixture = await startWorkspaceFixture(UI, { tls: true });
  const api = await playwright.request.newContext({ baseURL: fixture.origin, ignoreHTTPSErrors: true });
  const control = async data => (await api.post('/__fixture/control', { data })).json();
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, ignoreHTTPSErrors: true });
  let phase = 'workspace';
  try {
    const names = ['ws01', 'ws02'];
    const tree = { kind: 'split', direction: 'row', weights: [1, 1], children: names.map(name => ({ kind: 'leaf', session: { realm: 'local', server: 'private', name }, on_missing: 'offer' })) };
    await control({ reset: true, sessions: names, workspace: { name: 'aliases', tree } });
    const target = (await (await api.get('/api/inventory')).json()).realms[0].servers[0].sessions.find(session => session.name === 'ws01');
    let record = { alias_id: 'workspace-alias', display_alias: 'Research', revision: 1, state: 'active', realm: target.realm, server: target.server, session_name: 'ws01', session_incarnation: target.authority };
    let hold;
    const page = await context.newPage();
    page.setDefaultTimeout(8000);
    page.on('console', msg => evidence.console.push({ viewport: 'workspace', phase, type: msg.type(), text: msg.text(), url: msg.location().url }));
    page.on('pageerror', error => evidence.console.push({ viewport: 'workspace', phase, type: 'pageerror', text: String(error) }));
    // The workspace fixture has no alias store; these routes add one record.
    await page.route('**/api/inventory', async route => {
      const response = await route.fetch();
      const inventory = { ...await response.json(), aliases: [{ ...record }] };
      const held = hold; hold = undefined;
      if (held) { held.markFetched(); await held.released; }
      try { await route.fulfill({ response, json: inventory }); } catch { /* the page stopped waiting */ } finally { held?.markHandled(); }
    });
    await page.route('**/api/aliases/*', async route => {
      const request = route.request();
      if (request.method() !== 'PATCH' || request.headers()['if-match'] !== `"${record.revision}"`) return route.fulfill({ status: 409, contentType: 'text/plain', body: 'alias_changed' });
      record = { ...record, display_alias: JSON.parse(request.postData()).display_alias, revision: record.revision + 1 };
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(record) });
    });
    await page.goto(`${fixture.origin}/workspace?engine=unified-dev#name=aliases`);
    await page.getByRole('button', { name: /^Open workspace/ }).click();
    await page.waitForFunction(count => document.querySelectorAll('.ws-cell[data-ws-state="live"] .xterm-rows').length === count, names.length);
    const pane = page.locator('.ws-cell').nth(0), tag = pane.locator('.persea-unified-tag'), alias = pane.locator('.persea-unified-tag__alias');
    const editor = pane.locator('.persea-unified-identity__alias');
    const edit = editor.getByRole('button', { name: 'Edit alias for current session', exact: true });
    const input = editor.getByRole('textbox', { name: 'Alias for current session', exact: true });
    await tag.click();
    await whenEnabled(page, edit);
    assert(await alias.textContent() === 'Research', 'workspace pane does not show its alias');
    const held = hold = holdNext();
    await tag.click(); await tag.click();
    await held.fetched;
    await edit.click(); await input.fill('Deploy');
    await editor.getByRole('button', { name: 'Save', exact: true }).click();
    await editor.getByText('Alias saved.', { exact: true }).waitFor();
    held.release(); await held.handled; await whenEnabled(page, edit);
    assert(await alias.textContent() === 'Deploy', 'a shared read from before the save replaced the saved alias in a workspace pane');
    await edit.click(); await editor.getByRole('button', { name: 'Cancel', exact: true }).click(); await edit.click();
    assert(await input.inputValue() === 'Deploy', 'Cancel in a workspace pane returned to an alias older than the save');
    await editor.getByRole('button', { name: 'Cancel', exact: true }).click();
    // A change made elsewhere after the save reaches the pane on the next open.
    record = { ...record, display_alias: 'Changed elsewhere', revision: record.revision + 1 };
    await tag.click(); await tag.click();
    await page.waitForFunction(el => el.textContent === 'Changed elsewhere', await alias.elementHandle()).catch(() => { throw new Error('a read after the save did not reach the workspace pane'); });
    process.stdout.write(`ALIAS_BROWSER ${engine} workspace passed\n`);
  } finally {
    await context.close(); await api.dispose(); await fixture.close();
  }
}

async function run(engine) {
  const state = { aliases: [], sessions: [], requests: [] };
  const fail = (res, status, code, current) => {
    res.writeHead(status, { 'Content-Type': 'text/plain', ...(current ? { 'X-Persea-Alias-Record': JSON.stringify(current) } : {}) }); res.end(code); return true;
  };
  const fixture = await startFixture(UI, { tls: true, playwrightScreenshotStyle: true, clipboardHTTP: async (req, res, url) => {
    if (url.pathname === '/api/inventory') {
      const end = res.end;
      res.end = function(body, ...rest) {
        const inventory = JSON.parse(body);
        inventory.aliases = state.aliases;
        state.sessions = inventory.realms.flatMap(realm => realm.servers.flatMap(server => server.sessions));
        return end.call(this, JSON.stringify(inventory), ...rest);
      };
      return false;
    }
    if (!url.pathname.startsWith('/api/aliases')) return false;
    let raw = ''; for await (const chunk of req) raw += chunk;
    const body = raw ? JSON.parse(raw) : {};
    state.requests.push({ method: req.method, body, ifMatch: req.headers['if-match'] });
    let record = state.aliases.find(alias => url.pathname === `/api/aliases/${alias.alias_id}`);
    if (req.method === 'POST') {
      const target = state.sessions.find(session => session.handles.alias === body.handle);
      if (!target) return fail(res, 410, 'session_gone');
      const current = state.aliases.find(alias => alias.state === 'active' && scope(alias.session_incarnation) === scope(target.authority));
      if (current) return fail(res, 409, 'alias_exists', current);
      record = { alias_id: crypto.randomBytes(16).toString('hex'), display_alias: '', revision: 0, state: 'active', realm: target.realm, server: target.server, session_name: target.name, session_incarnation: target.authority };
    } else {
      if (!record) return fail(res, 404, 'alias_not_found');
      if (req.headers['if-match'] !== `"${record.revision}"`) return fail(res, 409, 'alias_changed', record);
    }
    if (req.method === 'DELETE') {
      assert(raw === '', 'DELETE sent a session handle'); state.aliases = state.aliases.filter(alias => alias !== record);
      res.writeHead(204); res.end(); return true;
    }
    if (req.method === 'PATCH') assert(body.handle === undefined, 'PATCH sent a rebinding handle');
    const display = body.display_alias?.trim();
    if (!display || [...display].length > 128 || /\p{Cc}/u.test(body.display_alias)) return fail(res, 400, 'alias_invalid');
    if (state.aliases.some(alias => alias !== record && alias.state === 'active' && alias.display_alias.toLowerCase() === display.toLowerCase())) return fail(res, 409, 'alias_in_use');
    state.aliases = state.aliases.filter(alias => alias === record || alias.state !== 'detached' || alias.display_alias.toLowerCase() !== display.toLowerCase());
    record.display_alias = display; record.revision++;
    if (req.method === 'POST') state.aliases.push(record);
    // A test can hold the reply after the store has changed.
    const held = state.holdReply; state.holdReply = undefined;
    if (held) { held.markFetched(); await held.released; }
    res.writeHead(req.method === 'POST' ? 201 : 200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(record)); return true;
  } });
  const browser = await playwright[engine].launch({ headless: true, ...(engine === 'chromium' ? { executablePath: require('./browser_path.cjs')() } : {}) });
  const evidence = { engine, viewports: [], console: [], requests: [] };
  const shapes = process.env.PERSEA_ALIAS_SHORT === '1' ? [['phone', 390, 844]] : [['small', 360, 740], ['phone', 390, 844], ['large', 430, 932], ['landscape', 844, 390], ['desktop', 1280, 800]];
  try {
    for (const [name, width, height] of shapes) {
      let phase = 'dashboard';
      state.aliases = [{ alias_id: 'detached', display_alias: 'Previous project', revision: 1, state: 'detached', realm: 'local', server: 'private', session_name: 'tm9', session_incarnation: {} }];
      const context = await browser.newContext({ viewport: { width, height }, hasTouch: name !== 'desktop', isMobile: name !== 'desktop', ignoreHTTPSErrors: true });
      await context.request.post(`${fixture.origin}/__fixture/control`, { data: { reset: true, switchSessions: true } });
      const page = await context.newPage();
      page.setDefaultTimeout(8000);
      page.on('console', msg => evidence.console.push({ viewport: name, phase, type: msg.type(), text: msg.text(), url: msg.location().url }));
      page.on('pageerror', error => evidence.console.push({ viewport: name, phase, type: 'pageerror', text: String(error) }));
      const shot = async label => {
        if (name !== 'phone' && name !== 'desktop') return;
        phase = 'screenshot'; await page.screenshot({ path: path.join(OUT, `${engine}-${name}-${label}.png`) }); phase = label;
      };
      const row = sessionName => page.locator('.session-card').filter({ has: page.locator('.session-name, .session-tmux-name', { hasText: new RegExp(`^${sessionName}$`) }) });
      await page.goto(`${fixture.origin}/`, { waitUntil: 'load' });
      await row('alpha').locator('.session-alias-edit').click();
      await row('alpha').getByRole('textbox', { name: 'Alias for alpha', exact: true }).fill('Research');
      await row('alpha').getByRole('button', { name: 'Save alias for alpha', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.session-name')?.textContent === 'Research');
      assert(await row('alpha').locator('.session-name').innerText() === 'Research', 'Dashboard save did not update row title');
      assert(await row('alpha').locator('.session-tmux-name').innerText() === 'alpha', 'Dashboard lost tmux name');
      await shot('dashboard');
      await row('beta').locator('.session-alias-edit').click();
      await row('beta').getByRole('textbox', { name: 'Alias for beta', exact: true }).fill('research');
      phase = 'alias-in-use';
      await row('beta').getByRole('button', { name: 'Save alias for beta', exact: true }).click();
      await page.getByText('Another running session already uses this alias.', { exact: true }).waitFor();
      assert(await row('beta').getByRole('textbox', { name: 'Alias for beta', exact: true }).inputValue() === 'research', 'Name conflict lost draft');
      await shot('alias-in-use');
      await page.keyboard.press('Escape');
      await page.getByRole('button', { name: 'Settings', exact: true }).click();
      await shot('settings-disclosures');
      await page.getByText('Aliases of sessions that are not running', { exact: true }).click();
      assert(await page.locator('.alias-history-session').innerText() === 'tm9', 'History omitted session name');
      assert(!(await page.locator('body').innerText()).includes('tombstone'), 'Internal state appeared on page');
      await shot('history');
      await page.getByRole('button', { name: 'Remove alias Previous project', exact: true }).click();
      await page.waitForFunction(() => ![...document.querySelectorAll('summary')].some(el => el.textContent === 'Aliases of sessions that are not running' && el.parentElement.hidden === false));

      phase = 'terminal';
      const raw = await (await context.request.get(`${fixture.origin}/api/inventory`)).json();
      const target = raw.realms[0].servers[0].sessions.find(session => session.name === 'alpha');
      const fragment = new URLSearchParams({ handle: target.handles.control, mode: 'control', history: '1000', name: 'alpha', alias: 'Stale link label', draft_scope: scope(target.authority), engine: 'unified-dev' });
      await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${fragment}`, { waitUntil: 'load' });
      await page.waitForFunction(() => document.querySelector('.xterm-rows')?.textContent.includes('fixture-live'));
      await page.locator('.persea-unified-tag').click();
      await page.waitForFunction(() => document.querySelector('.persea-unified-tag__alias')?.textContent === 'Research').catch(() => { throw new Error('Terminal tag ignored the latest inventory alias'); });
      assert(await page.locator('.persea-unified-tag__alias').isVisible(), 'Phone CSS hides the current alias');
      assert(await page.title() === 'Research · Persea Terminal', 'Title ignored inventory alias');
      const editor = page.locator('.persea-unified-identity__alias');
      const input = editor.getByRole('textbox', { name: 'Alias for current session', exact: true });
      const edit = editor.getByRole('button', { name: 'Edit alias for current session', exact: true });
      assert(!await input.isVisible() && await edit.isVisible(), 'Alias editor must start compact');
      assert(await editor.locator('.persea-unified-identity__alias-value').innerText() === 'Research', 'Compact alias row omitted the current alias');
      const order = await editor.evaluate(el => ({ dashboardY: el.parentElement.querySelector('[aria-label="Open the dashboard"]').getBoundingClientRect().top, aliasY: el.getBoundingClientRect().top, detailsY: el.parentElement.querySelector('.persea-unified-identity__current-details').getBoundingClientRect().top }));
      assert(order.dashboardY < order.aliasY && order.aliasY < order.detailsY, 'Dashboard, alias and current details are out of order');
      const requestsBeforeCancel = state.requests.length;
      await edit.click(); await input.fill('Discard this draft');
      await editor.getByRole('button', { name: 'Cancel', exact: true }).click();
      assert(!await input.isVisible() && state.requests.length === requestsBeforeCancel, 'Cancel saved a draft or left the field expanded');
      await edit.click(); assert(await input.inputValue() === 'Research', 'Cancel kept the discarded draft');
      await editor.getByRole('button', { name: 'Clear', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.persea-unified-tag__alias')?.hidden === true);
      assert(!await input.isVisible(), 'Successful Clear did not collapse the editor');
      await edit.click(); assert(!await editor.getByRole('button', { name: 'Clear', exact: true }).isVisible(), 'Empty alias editor offers Clear'); await input.fill('Deploy');
      await editor.getByRole('button', { name: 'Save', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.persea-unified-tag__alias')?.textContent === 'Deploy');
      assert(await page.title() === 'Deploy · Persea Terminal', 'Terminal save did not update title');
      await editor.getByText('Alias saved.', { exact: true }).waitFor();
      assert(!await input.isVisible(), 'Successful Save did not collapse the editor');
      // Dashboard, the alias, a status line and open details must not squeeze the
      // session list: its first row stays whole, and the whole list is reachable.
      const panel = page.locator('.persea-unified-identity__details');
      const listRoom = () => panel.evaluate(el => {
        const list = el.querySelector('.persea-session-switcher__list'), row = list.querySelector('.persea-session-switcher__row');
        el.scrollTop = el.scrollHeight;
        const p = el.getBoundingClientRect(), l = list.getBoundingClientRect(), r = row.getBoundingClientRect();
        const room = { list: l.height, firstRowBottom: r.bottom - l.top, listBottom: l.bottom, panelBottom: p.bottom, panelTop: p.top, dashboardTop: 0, userScrollable: el.scrollHeight <= el.clientHeight || ['auto', 'scroll'].includes(getComputedStyle(el).overflowY) };
        el.scrollTop = 0; room.dashboardTop = el.querySelector('[aria-label="Open the dashboard"]').getBoundingClientRect().top - el.getBoundingClientRect().top;
        return room;
      });
      for (const open of [false, true]) {
        if (open) await page.locator('.persea-unified-identity__current-details > summary').click();
        const room = await listRoom();
        assert(room.list + 0.5 >= room.firstRowBottom && room.listBottom <= room.panelBottom + 0.5 && room.dashboardTop >= 0 && room.dashboardTop < 16 && room.userScrollable, `${name}: session list squeezed (details ${open ? 'open' : 'closed'}): ${JSON.stringify(room)}`);
        if (open) await page.locator('.persea-unified-identity__current-details > summary').click();
      }
      await shot('terminal-editor');
      const tag = await page.locator('.persea-unified-tag').evaluate(el => {
        const alias = el.querySelector('.persea-unified-tag__alias'), name = el.querySelector('.persea-unified-tag__name');
        return { aliasX: alias.getBoundingClientRect().x, nameX: name.getBoundingClientRect().x, nameVisible: name.getBoundingClientRect().width > 0, separator: getComputedStyle(alias, '::before').content, aliasWeight: getComputedStyle(alias).fontWeight, nameFamily: getComputedStyle(name).fontFamily };
      });
      assert(['none', 'normal', '""'].includes(tag.separator), 'Alias tag has a leading separator');
      assert(Number(tag.aliasWeight) >= 600, 'Alias tag is not bold');
      assert(width < 720 ? !tag.nameVisible : tag.nameVisible && tag.aliasX < tag.nameX, 'Tag does not lead with the alias or hide the tmux name on a phone');
      assert(/mono/i.test(tag.nameFamily), 'Wide tag tmux name lost its code face');
      if (name === 'desktop') {
        // A Save click that the activation fence rejects (the pointer left the
        // button while held) must not fall through to a native form submission.
        // Enter in the field then saves exactly once.
        await edit.click(); await input.fill('Should not save');
        const box = await editor.getByRole('button', { name: 'Save', exact: true }).boundingBox();
        const before = state.requests.length;
        await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2); await page.mouse.down();
        await page.mouse.move(box.x + box.width + 40, box.y + box.height / 2); await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
        await page.mouse.up();
        // A save disables the field synchronously, so the field state shows a fall-through submission at once.
        assert(await input.isVisible() && await input.isEnabled(), 'A Save click rejected by the activation fence submitted the form');
        await input.fill('Deploy');
        const patched = page.waitForResponse(response => response.url().endsWith(`/api/aliases/${state.aliases.find(alias => alias.session_name === 'alpha').alias_id}`) && response.request().method() === 'PATCH');
        await input.press('Enter'); await patched;
        const sent = state.requests.slice(before).map(request => `${request.method} ${request.body.display_alias}`);
        assert(JSON.stringify(sent) === JSON.stringify(['PATCH Deploy']), `Rejected Save click or Enter sent the wrong saves: ${JSON.stringify(sent)}`);
        await editor.getByText('Alias saved.', { exact: true }).waitFor();
        assert(!await input.isVisible(), 'Enter save did not collapse the editor');
      }
      if (name === 'desktop') {
        const editEnabled = () => page.waitForFunction(() => document.querySelector('[aria-label="Edit alias for current session"]')?.disabled === false);
        // A: an inventory request sent before a save must not undo the save.
        // Hold the next inventory response, save while it is pending, then
        // deliver it; the acknowledged alias must stay.
        let release, fetched, handled;
        const held = new Promise(resolve => { release = resolve; });
        const heldFetched = new Promise(resolve => { fetched = resolve; });
        const heldHandled = new Promise(resolve => { handled = resolve; });
        await page.route('**/api/inventory', async route => {
          try { const response = await route.fetch(); fetched(); await held; await route.fulfill({ response }); } catch { /* the page dropped the request */ } finally { handled(); }
        }, { times: 1 });
        await page.locator('.persea-unified-tag').click(); await page.locator('.persea-unified-tag').click();
        await heldFetched;
        await edit.click(); await input.fill('Fresh');
        await editor.getByRole('button', { name: 'Save', exact: true }).click();
        await editor.getByText('Alias saved.', { exact: true }).waitFor();
        release(); await heldHandled; await editEnabled();
        assert(await page.locator('.persea-unified-tag__alias').textContent() === 'Fresh' && await page.title() === 'Fresh · Persea Terminal', 'An inventory sent before the save replaced the saved alias');
        // B: after a Clear whose refresh fails, Cancel must return to the
        // cleared state, and the next save must create a new alias.
        phase = 'inventory-fault';
        await page.route('**/api/inventory', route => route.abort('failed'), { times: 1 });
        await edit.click(); await editor.getByRole('button', { name: 'Clear', exact: true }).click();
        await editor.getByText('Alias cleared.', { exact: true }).waitFor();
        await page.locator('.persea-unified-identity__session-status', { hasText: 'Inventory unavailable' }).waitFor();
        await editEnabled();
        phase = 'terminal';
        await edit.click(); await editor.getByRole('button', { name: 'Cancel', exact: true }).click(); await edit.click();
        assert(await input.inputValue() === '' && !await editor.getByRole('button', { name: 'Clear', exact: true }).isVisible(), 'Cancel restored the cleared alias');
        await input.fill('New');
        const created = page.waitForResponse(response => response.url().endsWith('/api/aliases') && response.request().method() === 'POST');
        await editor.getByRole('button', { name: 'Save', exact: true }).click();
        assert((await created).status() === 201, 'Saving after a cleared alias did not create a new alias');
        await editor.getByText('Alias saved.', { exact: true }).waitFor();
        // C: a Reload that a later save overtakes must leave that save's message.
        phase = 'alias-conflict';
        const changed = state.aliases.find(alias => alias.session_name === 'alpha'); changed.display_alias = 'Changed elsewhere'; changed.revision++;
        await edit.click(); await input.fill('Mine');
        await editor.getByRole('button', { name: 'Save', exact: true }).click();
        const reload = editor.getByRole('button', { name: 'Reload saved alias', exact: true });
        await reload.waitFor();
        const reloadRead = holdNext();
        await page.route('**/api/inventory', async route => {
          try { const response = await route.fetch(); reloadRead.markFetched(); await reloadRead.released; await route.fulfill({ response }); } catch { /* the page stopped waiting */ } finally { reloadRead.markHandled(); }
        }, { times: 1 });
        await reload.click(); await reloadRead.fetched;
        await input.fill('Mine again');
        const refused = page.waitForResponse(response => response.url().includes('/api/aliases/') && response.request().method() === 'PATCH');
        await editor.getByRole('button', { name: 'Save', exact: true }).click();
        assert((await refused).status() === 409, 'The second save on a stale revision was not refused');
        await reload.waitFor();
        // The list status clears when the page has taken the reload's reply; the
        // reload's own continuation runs in the same turn.
        reloadRead.release(); await reloadRead.handled;
        await page.waitForFunction(() => document.querySelector('.persea-unified-identity__session-status')?.textContent === '');
        assert((await editor.locator('output').textContent()).includes('changed on another device') && await reload.isVisible(), 'An overtaken Reload erased the newer conflict message');
        phase = 'terminal';
        await reload.click(); await page.waitForFunction(() => document.querySelector('.persea-unified-identity__alias output')?.textContent === '');
        await editor.getByRole('button', { name: 'Cancel', exact: true }).click();
        // D: an incomplete session list that lacks this session, arriving while
        // a save waits for its reply, must not make the page drop that reply.
        const partial = holdNext(), reply = holdNext();
        await page.route('**/api/inventory', async route => {
          try {
            const response = await route.fetch(), inventory = await response.json();
            inventory.realms[0] = { ...inventory.realms[0], error: 'broker unavailable', servers: [] };
            partial.markFetched(); await partial.released; await route.fulfill({ response, json: inventory });
          } catch { /* the page stopped waiting */ } finally { partial.markHandled(); }
        }, { times: 1 });
        state.holdReply = reply;
        await page.locator('.persea-unified-tag').click(); await page.locator('.persea-unified-tag').click();
        await partial.fetched;
        await edit.click(); await input.fill('After a partial list');
        await editor.getByRole('button', { name: 'Save', exact: true }).click();
        await reply.fetched;
        partial.release(); await partial.handled;
        await page.waitForFunction(() => document.querySelector('.persea-unified-identity__session-status')?.textContent === '');
        reply.release();
        await page.waitForFunction(() => document.querySelector('.persea-unified-identity__alias output')?.textContent !== 'Saving alias…').catch(() => { throw new Error('A save whose reply came after an incomplete session list never finished'); });
        const outcome = await editor.locator('output').textContent();
        assert(outcome === 'Alias saved.', `A save whose reply came after an incomplete session list reported: ${outcome}`);
        await whenEnabled(page, edit);
        assert(await page.locator('.persea-unified-tag__alias').textContent() === 'After a partial list', 'The saved alias is not shown after an incomplete session list');
      }
      await edit.click(); await shot('terminal-editor-expanded');
      const measurements = await editor.evaluate(el => ({ width: el.getBoundingClientRect().width, targets: [...el.querySelectorAll('button,input')].filter(item => item.getBoundingClientRect().height > 0).map(item => ({ tag: item.tagName, height: item.getBoundingClientRect().height, shadow: getComputedStyle(item).boxShadow, border: getComputedStyle(item).borderTopWidth })), overflow: document.documentElement.scrollWidth > innerWidth }));
      assert(measurements.targets.every(item => item.height >= 44), `${name}: alias editor has a small touch target`);
      assert(measurements.targets.filter(item => item.tag === 'BUTTON').every(item => item.shadow === 'none' && item.border === '1px'), `Alias buttons differ from the other popover buttons: ${JSON.stringify(measurements.targets)}`);
      assert(!measurements.overflow, `${name}: alias editor causes horizontal page overflow`);
      await editor.getByRole('button', { name: 'Cancel', exact: true }).click();
      // A later edit on another device must appear on the next popover open.
      await page.locator('.persea-unified-tag').click();
      const current = state.aliases.find(alias => alias.session_name === 'alpha'); current.display_alias = 'Updated elsewhere'; current.revision++;
      await page.locator('.persea-unified-tag').click();
      await page.waitForFunction(() => document.querySelector('.persea-unified-tag__alias')?.textContent === 'Updated elsewhere');
      assert(await page.title() === 'Updated elsewhere · Persea Terminal', 'Popover did not refresh title');
      evidence.viewports.push({ name, width, height, measurements });
      await context.close();
      process.stdout.write(`ALIAS_BROWSER ${engine} ${name} passed\n`);
    }
    await workspaceCase(browser, engine, evidence);
    for (const message of evidence.console) {
      message.triage = message.phase === 'alias-in-use' && message.type === 'error' && message.url?.endsWith('/api/aliases') && /^Failed to load resource:.*409/.test(message.text) ? 'expected HTTP 409 from the deliberate name conflict'
        : message.phase === 'alias-conflict' && message.type === 'error' && /\/api\/aliases\/[^/]+$/.test(message.url ?? '') && /^Failed to load resource:.*409/.test(message.text) ? 'expected HTTP 409 from the deliberate revision conflict'
        : message.phase === 'inventory-fault' && ((message.type === 'error' && message.url?.endsWith('/api/inventory') && /^Failed to load resource/.test(message.text)) || (message.type === 'info' && /^Web Inspector blocked \S+\/api\/inventory from loading$/.test(message.text))) ? 'expected failure of the deliberately failed inventory request'
        : 'unexpected';
    }
    assert(evidence.console.every(message => message.triage !== 'unexpected'), `Unexpected browser messages: ${JSON.stringify(evidence.console)}`);
  } finally {
    evidence.requests = state.requests;
    fs.mkdirSync(OUT, { recursive: true }); fs.writeFileSync(path.join(OUT, `alias-browser-${engine}${process.env.PERSEA_ALIAS_SHORT === '1' ? '-short' : ''}.json`), JSON.stringify(evidence, null, 2));
    await browser.close(); await fixture.close();
  }
}
(async () => { fs.mkdirSync(OUT, { recursive: true }); for (const engine of engines) await run(engine); })().catch(error => { console.error(error.stack || error); process.exitCode = 1; });
