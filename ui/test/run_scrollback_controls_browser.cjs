'use strict';
const fs = require('fs'), path = require('path'), https = require('https');
const { startFixture } = require('./unified_reopen_fixture.cjs');
const pw = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const UI = path.resolve(__dirname, '..');
const ENGINE = process.env.PERSEA_DASHBOARD_ENGINE || 'chromium';
const OUT = path.join(process.env.PERSEA_DASHBOARD_EVIDENCE || '/tmp/agent_logs/dashboard-session-controls', `scrollback-${ENGINE}`);
const assert = (condition, message) => { if (!condition) throw Error(message); };
const requestJSON = (url, value) => new Promise((resolve, reject) => {
  const payload = value === undefined ? undefined : JSON.stringify(value);
  const request = https.request(url, { method: payload ? 'POST' : 'GET', rejectUnauthorized: false, headers: payload ? { 'Content-Type': 'application/json' } : {} }, response => {
    let text = ''; response.setEncoding('utf8'); response.on('data', chunk => text += chunk);
    response.on('end', () => { try { if (response.statusCode !== 200) throw Error(`Fixture HTTP ${response.statusCode}`); resolve(JSON.parse(text)); } catch (error) { reject(error); } });
  }); request.on('error', reject); request.end(payload);
});

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const initialReplay = Array.from({ length: 3200 }, (_, i) => `SCROLL-${String(i).padStart(5, '0')}\r\n`).join('');
  const fixture = await startFixture(UI, { tls: true, playwrightScreenshotStyle: true, initialReplay });
  const control = value => requestJSON(fixture.origin + '/__fixture/control', value);
  const browser = await pw[ENGINE].launch({ headless: true, ...(ENGINE === 'chromium' ? { executablePath: require("./browser_path.cjs")(), args: ['--no-sandbox'] } : {}) });
  const result = { engine: ENGINE, checks: [], errors: [], console: [] }; let phase = 'setup';
  try {
    for (const [width, height] of [[320, 568], [390, 844], [430, 932], [844, 390], [1440, 900]]) {
      phase = `scrollback ${width}`; await control({ reset: true });
      const context = await browser.newContext({ viewport: { width, height }, ignoreHTTPSErrors: true });
      const page = await context.newPage(); page.setDefaultTimeout(10000);
      page.on('pageerror', error => result.errors.push({ phase, message: error.message }));
      page.on('console', message => result.console.push({ phase, type: message.type(), message: message.text().slice(0, 500) }));
      try {
        const inventory = await requestJSON(fixture.origin + '/api/inventory'); const session = inventory.realms[0].servers[0].sessions[0];
        await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session.handles.control, mode: 'control', history: '500', name: session.name, draft_scope: fixture.draftScope, engine: 'unified-dev' })}`);
        await page.waitForFunction(() => document.querySelector('.xterm-rows')?.textContent.includes('fixture-live'));
        const top = async () => {
          await page.locator('.persea-unified-scroll').evaluate(node => { node.scrollTop = 0; node.dispatchEvent(new Event('scroll')); });
          await page.waitForTimeout(120);
          return page.locator('.xterm-rows').innerText();
        };
        const initialTop = await top();
        const first = Number(initialTop.match(/SCROLL-(\d+)/)?.[1]);
        assert(Number.isFinite(first) && first >= 2600 && first < 2800, `Initial 500-row selection was ignored: oldest=${first}`);
        const before = await control();
        const menu = page.locator('.persea-unified-sheet--menu');
        const openMenu = async () => { if (!await menu.isVisible()) await page.getByRole('button', { name: 'Quick actions', exact: true }).click(); };
        await openMenu();
        const select = menu.getByRole('combobox', { name: 'Scrollback rows', exact: true });
        assert(await select.inputValue() === '500', 'Terminal did not display its launch depth');
        await select.selectOption('5000');
        assert(new URLSearchParams(new URL(page.url()).hash.slice(1)).get('history') === '5000', 'Live limit was not preserved in the reload URL');
        await menu.getByRole('button', { name: 'Reload available recorded history', exact: true }).click();
        const waitReplay = async count => {
          const until = Date.now() + 10000;
          while (Date.now() < until) { const state = await control(); if (state.counters.replays > count && state.attachments.at(-1)?.frames.includes('READY')) return state; await page.waitForTimeout(50); }
          throw Error('History did not reconnect and replay');
        };
        const replayed = await waitReplay(before.counters.replays);
        await page.waitForFunction(() => document.querySelector('.xterm-rows')?.textContent.includes('SCROLL-'));
        assert((await top()).includes('SCROLL-00000'), 'Increasing and reloading did not recover older recorded rows');
        await openMenu(); await select.selectOption('0');
        await menu.getByRole('button', { name: 'Close terminal menu', exact: true }).click();
        const screenOnly = await top();
        assert(!screenOnly.includes('SCROLL-00000') && Number(screenOnly.match(/SCROLL-(\d+)/)?.[1]) > 3100, 'Screen-only choice did not take effect in the open terminal');
        const after = await control();
        assert(after.counters.replays === replayed.counters.replays && after.counters.adoptions === before.counters.adoptions && after.counters.refits === before.counters.refits, 'Changing retention triggered a replay, adoption, or resize');
        assert(after.attachments.every(item => item.inputs.length === 0 && item.resizeRequests.length === 0 && !item.frames.includes('HISTORY_REQUEST')), 'History controls sent terminal input, geometry, or a legacy history request');
        await openMenu();
        const preferences = await page.evaluate(scope => ({ defaults: localStorage.getItem('persea-terminal.scrollback.v1'), own: JSON.parse(localStorage.getItem('persea-terminal.scrollback.sessions.v1') || '[]').find(entry => entry[0] === scope)?.[1] }), fixture.draftScope);
        assert(await select.inputValue() === '0' && preferences.own === 0 && preferences.defaults === null, 'Live choice was not isolated to this terminal');
        assert(await select.evaluate(node => { const r = node.getBoundingClientRect(); return r.width >= 44 && r.height >= 44 && r.left >= 0 && r.right <= innerWidth; }), 'Scrollback selector is not usable at this width');
        phase = 'screenshot'; await page.screenshot({ path: path.join(OUT, `menu-${width}.png`), animations: 'disabled' }); phase = `scrollback ${width}`;
        phase = `document reload ${width}`;
        await page.reload();
        await page.waitForFunction(() => document.querySelector('.xterm-rows')?.textContent.includes('fixture-live'));
        await openMenu();
        assert(await select.inputValue() === '0' && !(await top()).includes('SCROLL-00000'), 'A document reload lost the live scrollback choice');
        if (width === 1440) {
          phase = 'device default and terminal isolation';
          await control({ switchSessions: true, sessionBState: 'open' });
          const dashboard = await context.newPage();
          dashboard.on('pageerror', error => result.errors.push({ phase, message: error.message }));
          dashboard.on('console', message => result.console.push({ phase, type: message.type(), message: message.text().slice(0, 500) }));
          await dashboard.goto(fixture.origin + '/terminal');
          const defaults = dashboard.getByRole('combobox', { name: 'Scrollback rows', exact: true });
          await dashboard.locator('a.session-open-action').first().waitFor();
          assert(await defaults.inputValue() === '1000', 'Untouched device did not default to 1,000 rows');
          const linkHistory = scope => dashboard.locator('a.session-open-action').evaluateAll((links, wanted) => links.map(link => new URLSearchParams(new URL(link.href).hash.slice(1))).find(query => query.get('draft_scope') === wanted)?.get('history'), scope);
          assert(await linkHistory(fixture.draftScope) === '0' && await linkHistory(fixture.draftScopeB) === '1000', 'Dashboard lost terminal override isolation');
          await defaults.selectOption('2000');
          assert(await linkHistory(fixture.draftScope) === '0' && await linkHistory(fixture.draftScopeB) === '2000', 'Changing device default overwrote a terminal override');
          assert(await select.inputValue() === '0', 'Another tab changed the open terminal limit');
          await select.selectOption('default');
          assert(await page.evaluate(scope => !JSON.parse(localStorage.getItem('persea-terminal.scrollback.sessions.v1') || '[]').some(entry => entry[0] === scope), fixture.draftScope), 'Device default did not clear the terminal override');
          assert(new URLSearchParams(new URL(page.url()).hash.slice(1)).get('history') === '2000', 'Reset did not apply the current device default');
          await select.selectOption('5000');
          const adoptionDepths = [];
          page.on('request', request => { if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/session-adoptions') adoptionDepths.push(request.postDataJSON()); });
          await control({ sessionBState: 'adoptable' });
          const switchTo = async (name, scope) => {
            await openMenu();
            await page.getByRole('button', { name: 'Choose another session', exact: true }).click();
            await page.getByRole('button', { name: `Switch to ${name}`, exact: true }).click();
            await page.waitForFunction(expected => new URLSearchParams(location.hash.slice(1)).get('draft_scope') === expected, scope);
            await page.waitForFunction(() => document.querySelector('.xterm-rows')?.textContent.includes('fixture-live') || document.querySelector('.xterm-rows')?.textContent.includes('beta-live'));
            await openMenu();
          };
          await switchTo('beta', fixture.draftScopeB);
          assert(await select.inputValue() === 'default' && new URLSearchParams(new URL(page.url()).hash.slice(1)).get('history') === '2000', 'Session switch carried A scrollback into B');
          assert(adoptionDepths.some(item => item.session_id === '$8' && item.history_rows === 10000), 'A device viewing limit reduced shared imported history');
          await select.selectOption('500');
          await switchTo(session.name, fixture.draftScope);
          assert(await select.inputValue() === '5000', 'Switching back lost A terminal override');
          const secondDevice = await browser.newContext({ viewport: { width, height }, ignoreHTTPSErrors: true });
          try {
            const fresh = await secondDevice.newPage();
            fresh.on('pageerror', error => result.errors.push({ phase, message: error.message }));
            fresh.on('console', message => result.console.push({ phase, type: message.type(), message: message.text().slice(0, 500) }));
            await fresh.goto(fixture.origin + '/terminal');
            await fresh.locator('a.session-open-action').first().waitFor();
            assert(await fresh.getByRole('combobox', { name: 'Scrollback rows', exact: true }).inputValue() === '1000', 'A separate device inherited another device default');
            assert(await fresh.locator('a.session-open-action').evaluateAll(links => links.every(link => new URLSearchParams(new URL(link.href).hash.slice(1)).get('history') === '1000')), 'A separate device inherited terminal overrides');
          } finally { await secondDevice.close(); }
          const final = await control();
          assert(final.attachments.every(item => item.inputs.length === 0 && item.resizeRequests.length === 0), 'Preference isolation caused terminal input or resizing');
          await dashboard.close();
          result.isolation = { defaultRows: 1000, terminalOverride: true, deviceDefault: true, reset: true, sessionSwitch: true, separateDevice: true };
        }
        result.checks.push({ width, first, pass: true, replays: after.counters.replays });
      } catch (error) {
        result.failedPhase = phase;
        result.lastPage = await page.evaluate(() => ({ pathname: location.pathname, history: new URLSearchParams(location.hash.slice(1)).get('history'), text: document.body.innerText.slice(0, 1200) })).catch(() => null);
        const state = await control(); result.lastCounters = state.counters;
        result.lastAttachments = state.attachments.map(item => ({ frames: item.frames, protocolHistory: item.protocolHistory }));
        throw error;
      } finally { await context.close(); }
    }
    result.unexpectedConsole = result.console.filter(item => !(item.phase === 'screenshot' && /Refused to apply a stylesheet.*style-src/.test(item.message)));
    assert(result.errors.length === 0 && result.unexpectedConsole.length === 0, 'Scrollback controls produced browser errors'); result.pass = true;
  } catch (error) { result.pass = false; result.failure = String(error); throw error; }
  finally { fs.writeFileSync(path.join(OUT, 'result.json'), JSON.stringify(result, null, 2)); await browser.close(); await fixture.close(); }
  console.log(`${ENGINE}: scrollback launch, live retention, and recorded replay passed at ${result.checks.length} sizes`);
}
main().catch(error => { console.error(String(error)); process.exitCode = 1; });
