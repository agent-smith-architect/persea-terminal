'use strict';
const fs = require('fs'), path = require('path'), crypto = require('crypto');
const { startFixture } = require('./unified_reopen_fixture.cjs');
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve('playwright'));
const UI = process.env.PERSEA_ALIAS_UI || path.resolve(__dirname, '..');
const OUT = process.env.PERSEA_ALIAS_EVIDENCE || '/data4/agent/artifacts/evidence/persea-terminal/2026-10-03-alias-switcher/scratch/alias';
const engines = process.env.PERSEA_ALIAS_ENGINE ? [process.env.PERSEA_ALIAS_ENGINE] : ['chromium', 'webkit'];
const assert = (value, message) => { if (!value) throw new Error(message); };
const scope = a => JSON.stringify([a.realm, a.server, a.selector_kind, a.selector_value, a.boot_id, a.session_id, a.uid, a.server_pid, a.server_start, a.session_created]);

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
    res.writeHead(req.method === 'POST' ? 201 : 200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(record)); return true;
  } });
  const browser = await playwright[engine].launch({ headless: true, ...(engine === 'chromium' ? { executablePath: require('./browser_path.cjs')() } : {}) });
  const evidence = { engine, viewports: [], console: [], requests: [] };
  const shapes = process.env.PERSEA_ALIAS_SHORT === '1' ? [['phone', 390, 844]] : [['small', 360, 740], ['phone', 390, 844], ['large', 430, 932], ['landscape', 844, 390], ['desktop', 1280, 800]];
  try {
    for (const [name, width, height] of shapes) {
      let phase = 'dashboard';
      state.aliases = [{ alias_id: 'detached', display_alias: 'Previous project', revision: 1, state: 'detached', realm: 'local', server: 'private', session_name: 'he9', session_incarnation: {} }];
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
      assert(await page.locator('.alias-history-session').innerText() === 'he9', 'History omitted session name');
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
      await shot('terminal-editor');
      const tag = await page.locator('.persea-unified-tag').evaluate(el => {
        const alias = el.querySelector('.persea-unified-tag__alias'), name = el.querySelector('.persea-unified-tag__name');
        return { aliasX: alias.getBoundingClientRect().x, nameX: name.getBoundingClientRect().x, nameVisible: name.getBoundingClientRect().width > 0, separator: getComputedStyle(alias, '::before').content, aliasWeight: getComputedStyle(alias).fontWeight, nameFamily: getComputedStyle(name).fontFamily };
      });
      assert(['none', 'normal', '""'].includes(tag.separator), 'Alias tag has a leading separator');
      assert(Number(tag.aliasWeight) >= 600, 'Alias tag is not bold');
      assert(width < 720 ? !tag.nameVisible : tag.nameVisible && tag.aliasX < tag.nameX, 'Tag does not lead with the alias or hide the tmux name on a phone');
      assert(/mono/i.test(tag.nameFamily), 'Wide tag tmux name lost its code face');
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
    for (const message of evidence.console) {
      message.triage = message.phase === 'alias-in-use' && message.type === 'error' && message.url?.endsWith('/api/aliases') && /^Failed to load resource:.*409/.test(message.text) ? 'expected HTTP 409 from the deliberate name conflict' : 'unexpected';
    }
    assert(evidence.console.every(message => message.triage !== 'unexpected'), `Unexpected browser messages: ${JSON.stringify(evidence.console)}`);
  } finally {
    evidence.requests = state.requests;
    fs.mkdirSync(OUT, { recursive: true }); fs.writeFileSync(path.join(OUT, `alias-browser-${engine}${process.env.PERSEA_ALIAS_SHORT === '1' ? '-short' : ''}.json`), JSON.stringify(evidence, null, 2));
    await browser.close(); await fixture.close();
  }
}
(async () => { fs.mkdirSync(OUT, { recursive: true }); for (const engine of engines) await run(engine); })().catch(error => { console.error(error.stack || error); process.exitCode = 1; });
