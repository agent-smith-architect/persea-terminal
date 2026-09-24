'use strict';
const fs = require('fs'), path = require('path'), crypto = require('crypto');
const { startFixture } = require('./unified_reopen_fixture.cjs');
const https = require('https');
const requestJSON = (url, method = 'GET', value) => new Promise((resolve, reject) => {
  const payload = value === undefined ? undefined : JSON.stringify(value);
  const request = https.request(url, { method, rejectUnauthorized: false, headers: payload ? { 'Content-Type': 'application/json' } : {} }, response => {
    let data = ''; response.setEncoding('utf8'); response.on('data', chunk => data += chunk);
    response.on('end', () => { try { if (response.statusCode !== 200) throw Error('Fixture HTTP ' + response.statusCode); resolve(JSON.parse(data)); } catch(error) { reject(error); } });
  }); request.on('error', reject); request.end(payload);
});
const pw = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const UI = path.resolve(__dirname, '..');
const ENGINE = process.env.PERSEA_DASHBOARD_ENGINE || 'chromium';
const OUT = process.env.PERSEA_DASHBOARD_EVIDENCE || '/tmp/agent_logs/session-menu';
const assert = (value, message) => { if (!value) throw Error(message); };
async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const extras = Array.from({ length: 20 }, (_, i) => ({
    handles: { alias: crypto.randomBytes(32).toString('base64url'), observe: crypto.randomBytes(32).toString('base64url'), control: crypto.randomBytes(32).toString('base64url') },
    realm: 'local', server: 'private', server_status: 'ok', session_id: '$' + (i + 100), name: 'qt' + (i + 1), width: 127, height: 30, attached: 1, activity: 1, output_activity: Math.floor(Date.now()/1000) - 70,
    authority: { realm: 'local', server: 'private', uid: 1000, selector_kind: 'socket_path', selector_value: '/tmp/private.sock', boot_id: 'menu-fixture', server_pid: 42, server_start: 100, session_id: '$' + (i + 100), session_created: 200 + i },
    unified: { state: 'open', origin: 'reconstructed' },
  }));
  const fixture = await startFixture(UI, { tls: true, playwrightScreenshotStyle: true, extraInventorySessions: extras });
  const control = input => requestJSON(fixture.origin + '/__fixture/control', 'POST', input);
  const browser = await pw[ENGINE].launch({ headless: true, ...(ENGINE === 'chromium' ? { executablePath: require("./browser_path.cjs")(), args: ['--no-sandbox'] } : {}) });
  const result = { engine: ENGINE, checks: [], console: [], errors: [] }; let phase = 'setup';
  try {
    // The existing toolbar omits its identity chip below 320 content pixels;
    // that compact fallback remains covered by the full toolbar/quick-sheet gate.
    for (const [width, height] of [[360,780],[390,844],[430,932],[844,390],[1440,1000]]) {
      phase = `${width}x${height}`;
      await control({ reset: true, switchSessions: true, terminal_touchSwitcherMetadata: true });
      const context = await browser.newContext({ viewport: { width, height }, screen: { width, height }, isMobile: width < 1000, hasTouch: width < 1000, ignoreHTTPSErrors: true });
      const page = await context.newPage(); page.setDefaultTimeout(7000);
      page.on('console', message => result.console.push({ phase, text: message.text().slice(0,500), type: message.type() })); page.on('pageerror', error => result.errors.push({ phase, text: error.message }));
      const inventory = await requestJSON(fixture.origin + '/api/inventory'); const session = inventory.realms[0].servers[0].sessions[0];
      await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({ handle: session.handles.control, mode: 'control', history: '1000', name: session.name, draft_scope: fixture.draftScope, engine: 'unified-dev' })}`);
      await page.waitForFunction(() => document.querySelector('.xterm-rows')?.textContent.includes('fixture-live'));
      const before = await requestJSON(fixture.origin + '/__fixture/control');
      if (!await page.locator('.persea-unified-tag').isVisible()) {
        result.visibility = await page.evaluate(() => { const nodes = []; for (let node = document.querySelector('.persea-unified-tag'); node; node = node.parentElement) { const rect = node.getBoundingClientRect(); nodes.push({ tag: node.tagName, class: node.className, hidden: node.hidden, display: getComputedStyle(node).display, width: rect.width, height: rect.height }); } return nodes; });
      }
      await page.locator('.persea-unified-tag').click();
      const menu = page.locator('.persea-unified-identity__details'); await menu.locator('.persea-session-switcher__row').last().waitFor();
      assert(await menu.getByText('Current session details', { exact: true }).isVisible(), 'Current identity disclosure missing');
      assert(await menu.locator('.persea-unified-identity__facts').isHidden(), 'Identity facts crowd the switcher before expansion');
      const names = await menu.locator('.persea-session-switcher__session-name').allTextContents();
      assert(names.indexOf('qt2') < names.indexOf('qt10'), 'Menu must sort numbers naturally');
      assert((await menu.locator('.persea-session-switcher__meta').allTextContents()).some(text => text.includes('127×30 · 1 attached · Output 1m ago')), 'Menu lost the shared output metadata');
      const geometry = await menu.evaluate(node => {
        const box = el => { const r = el.getBoundingClientRect(); return { top: r.top, bottom: r.bottom, left: r.left, right: r.right, width: r.width, height: r.height }; };
        const list = node.querySelector('.persea-session-switcher__list'), search = node.querySelector('.persea-session-switcher__search');
        const before = box(search); list.scrollTop = list.scrollHeight; const after = box(search);
        const targets = [...node.querySelectorAll('button,input,summary')].filter(el => el.getClientRects().length && !el.closest('details:not([open]) :not(summary)'));
        return { menu: box(node), list: box(list), searchBefore: before, searchAfter: after, scroll: list.scrollTop, scrollRange: list.scrollHeight - list.clientHeight, viewport: { width: innerWidth, height: innerHeight }, small: targets.filter(el => { const r = box(el); return r.width < 43.9 || r.height < 43.9; }).map(el => el.getAttribute('aria-label') || el.textContent) };
      });
      assert(geometry.menu.left >= 0 && geometry.menu.right <= width + 1 && geometry.menu.bottom <= height + 1, `Menu escaped viewport at ${width}: ${JSON.stringify(geometry)}`);
      assert(geometry.scroll > 0 && geometry.searchBefore.top === geometry.searchAfter.top, `Search scrolled out of reach at ${width}`);
      assert(geometry.small.length === 0, `Menu targets too small at ${width}: ${geometry.small.join(', ')}`);
      const list = menu.locator('.persea-session-switcher__list'); await list.evaluate(node => node.scrollTop = 0);
      const switchButton = menu.getByRole('button', { name: 'Switch to qt1', exact: true });
      await control({ holdInventoryMs: 250 });
      const refreshed = page.waitForResponse(response => response.url().endsWith('/api/inventory'));
      await menu.getByRole('button', { name: 'Refresh sessions', exact: true }).click();
      await switchButton.focus();
      await page.evaluate(() => { window.menuFocusWitness = document.activeElement; });
      await refreshed; await page.waitForTimeout(100); await control({ holdInventoryMs: 0 });
      assert(await page.evaluate(() => document.activeElement === window.menuFocusWitness && window.menuFocusWitness.isConnected), 'Inventory refresh replaced the focused session button');
      await menu.locator('.persea-session-switcher__search').fill('qt20'); assert(await menu.locator('.persea-session-switcher__row').count() === 1, 'Menu search failed');
      await menu.locator('.persea-session-switcher__search').fill('');
      const after = await requestJSON(fixture.origin + '/__fixture/control');
      assert(after.counters.adoptions === before.counters.adoptions && after.counters.refits === before.counters.refits, 'Menu interaction adopted or resized a session');
      assert(after.attachments.flatMap(item => item.inputs).length === before.attachments.flatMap(item => item.inputs).length, 'Menu interaction dispatched terminal input');
      if (width === 390 || width === 1440) { phase = 'screenshot'; await page.screenshot({ path: path.join(OUT, `${ENGINE}-menu-${width}.png`), caret: 'initial', animations: 'allow' }); }
      result.checks.push({ width, height, pass: true, geometry }); await context.close();
    }
    result.unexpectedConsole = result.console.filter(item => !(item.phase === 'screenshot' && /Content Security Policy|CSP|style-src/.test(item.text)));
    assert(result.errors.length === 0 && result.unexpectedConsole.length === 0, 'Menu browser console must be clean'); result.pass = true;
  } catch(error) { result.pass = false; result.failure = String(error); throw error; }
  finally { fs.writeFileSync(path.join(OUT, ENGINE + '-menu.json'), JSON.stringify(result, null, 2) + '\n'); await browser.close(); await fixture.close(); }
  console.log(JSON.stringify({ engine: ENGINE, pass: true, sizes: result.checks.length }));
}
main().catch(error => { console.error(String(error)); process.exitCode = 1; });
