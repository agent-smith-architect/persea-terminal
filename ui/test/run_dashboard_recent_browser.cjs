'use strict';
const fs = require('fs'), path = require('path');
const { startCohesionFixture, session, scope } = require('./dashboard_cohesion_fixture.cjs');
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve('playwright'));
const ENGINE = process.env.PERSEA_DASHBOARD_ENGINE || 'chromium';
const OUT = process.env.PERSEA_DASHBOARD_EVIDENCE || path.join(require('os').tmpdir(), 'persea-terminal-tests', 'run_dashboard_recent_browser');
const assert = (value, message) => { if (!value) throw Error(message); };

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const fixture = await startCohesionFixture();
  const browser = await playwright[ENGINE].launch({ headless: true, ...(ENGINE === 'chromium' ? { executablePath: require('./browser_path.cjs')(), args: ['--no-sandbox'] } : {}) });
  const evidence = { engine: ENGINE, viewports: [], console: [], pageErrors: [] }; let phase = 'setup';
  try {
    fixture.state.sessions = [1, 2, 3, 4, 5, 6].map(n => session('local', n, { name: `tm${n + 1}`, height: 24 }));
    fixture.state.others = [];
    fixture.state.aliases = fixture.state.sessions.slice(0, 3).map((s, i) => ({ alias_id: `recent-${i}`, display_alias: ['Build', 'Shell', 'Tasks'][i], revision: 1, state: 'active', realm: s.realm, server: s.server, session_name: s.name, session_incarnation: s.authority }));
    for (const shape of [{ name: 'small', width: 320, height: 844 }, { name: 'phone', width: 390, height: 844 }, { name: 'large', width: 430, height: 932 }, { name: 'landscape', width: 844, height: 390 }, { name: 'desktop', width: 1280, height: 800 }]) {
      phase = `${shape.name}:setup`;
      const context = await browser.newContext({ viewport: { width: shape.width, height: shape.height }, hasTouch: shape.name !== 'desktop', isMobile: shape.name !== 'desktop', ignoreHTTPSErrors: true });
      const original = fixture.state.sessions.slice();
      await context.addInitScript(({ sessions }) => {
        const key = s => { const a = s.authority; return JSON.stringify([a.realm, a.server, a.selector_kind, a.selector_value, a.boot_id, a.session_id, a.uid, a.server_pid, a.server_start, a.session_created]); };
        const at = Date.now();
        localStorage.setItem('persea-terminal.session-discovery.v1', JSON.stringify({ pinned: [], recent: sessions.map((s, i) => ({ scope: key(s), name: s.name, at: at - i * 1000 })) }));
        localStorage.setItem('persea-terminal.last-session.v1', JSON.stringify({ draftScope: key(sessions[0]), name: sessions[0].name, realm: sessions[0].realm, server: sessions[0].server, at }));
      }, { sessions: original });
      const page = await context.newPage();
      page.on('console', m => evidence.console.push({ phase, type: m.type(), text: m.text(), location: m.location() }));
      page.on('pageerror', e => evidence.pageErrors.push({ phase, message: String(e) }));
      await page.goto(`${fixture.origin}/?resume=1`);
      const recent = page.locator('.dashboard-recent-rows .session-card');
      await page.waitForFunction(() => document.querySelectorAll('.dashboard-content .session-card').length === 6);
      assert(await recent.count() === 3, `${shape.name}: Recent must show three rows by default`);
      const titles = await recent.locator('.session-name').allTextContents();
      assert(titles.join(',') === 'Build,Shell,Tasks', `${shape.name}: Recent order or alias titles changed: ${titles}`);
      assert(await recent.first().locator('.session-open-action').evaluate(el => document.activeElement === el), `${shape.name}: resume launch did not focus the first Recent Open action`);
      assert(!fixture.state.requests.some(r => r.method === 'POST' && /session-adoptions|sessions$/.test(r.path)), `${shape.name}: passive Recent performed an authority action`);
      for (const mode of ['Favorites', 'Recent', 'Output', 'All']) {
        await page.locator('.dashboard-list-modes').getByRole('button', { name: mode, exact: true }).click();
        assert(await page.locator('.dashboard-toolbar').evaluate(el => el.getBoundingClientRect().right <= innerWidth && document.documentElement.scrollWidth <= innerWidth), `${shape.name}: ${mode} toolbar overflows`);
      }
      phase = `${shape.name}:output`;
      // Output lists every session, latest output first; unknown time last. A
      // refresh re-sorts from the new inventory without any other request.
      const listed = () => page.locator('.dashboard-content .session-card').evaluateAll(cards => cards.map(card => card.querySelector('.session-tmux-name')?.textContent || card.querySelector('.session-name')?.textContent));
      const now = Math.floor(Date.now() / 1000);
      [300, 5, 7200, 60, undefined, 30].forEach((ago, i) => { if (ago === undefined) delete fixture.state.sessions[i].output_activity; else fixture.state.sessions[i].output_activity = now - ago; });
      await page.evaluate(() => document.querySelector('.dashboard-refresh').click());
      await page.waitForFunction(() => document.querySelector('.session-card .session-metadata')?.textContent.includes('Output 5m ago'));
      // A row moved into view still loads its one passive preview, as on scroll.
      const reads = () => fixture.state.requests.filter(r => r.path !== '/api/session-previews').length, beforeSwitch = reads();
      await page.locator('.dashboard-list-modes').getByRole('button', { name: 'Output', exact: true }).click();
      await page.waitForTimeout(300);
      assert(reads() === beforeSwitch, `${shape.name}: Output made a request of its own`);
      assert((await listed()).join(',') === 'tm3,tm7,tm5,tm2,tm4,tm6', `${shape.name}: Output order is not latest first: ${await listed()}`);
      assert(await page.locator('.dashboard-results').textContent() === '6 live sessions', `${shape.name}: Output is not a full list`);
      fixture.state.sessions[2].output_activity = now;
      await page.evaluate(() => document.querySelector('.dashboard-refresh').click());
      await page.waitForFunction(() => document.querySelector('.dashboard-content .session-card .session-tmux-name')?.textContent === 'tm4');
      assert((await listed()).join(',') === 'tm4,tm3,tm7,tm5,tm2,tm6', `${shape.name}: Output did not re-sort after a refresh: ${await listed()}`);
      await page.locator('.dashboard-list-modes').getByRole('button', { name: 'All', exact: true }).click();
      assert((await listed()).join(',') === 'tm2,tm3,tm4,tm5,tm6,tm7', `${shape.name}: All lost name order after Output`);
      fixture.state.sessions.forEach(s => { s.output_activity = now - 70; });
      const favorite = recent.first().locator('.session-pin');
      await favorite.click();
      await page.waitForFunction(() => [...document.querySelectorAll('.session-card')].filter(card => card.querySelector('.session-tmux-name')?.textContent === 'tm2').every(card => card.querySelector('.session-pin')?.getAttribute('aria-pressed') === 'true'));
      await favorite.click();
      await page.waitForFunction(() => [...document.querySelectorAll('.session-card')].filter(card => card.querySelector('.session-tmux-name')?.textContent === 'tm2').every(card => card.querySelector('.session-pin')?.getAttribute('aria-pressed') === 'false'));
      phase = `${shape.name}:previews`;
      const mainRow = page.locator('.dashboard-content .session-card').filter({ has: page.locator('.session-tmux-name', { hasText: /^tm2$/ }) });
      await recent.first().getByRole('button', { name: 'Session information for tm2', exact: true }).click();
      await mainRow.getByRole('button', { name: 'Session information for tm2', exact: true }).click();
      await page.waitForFunction(() => [...document.querySelectorAll('.session-card[data-session-scope]')].filter(el => el.querySelector('.session-tmux-name')?.textContent === 'tm2' && !el.querySelector('.session-detail').hidden && el.querySelector('.session-preview-screen').textContent.includes('Snapshot')).length === 2);
      const modalTrigger = recent.first().locator('.session-preview-row-button');
      await modalTrigger.click();
      await page.getByRole('button', { name: 'Close output preview', exact: true }).click();
      assert(await modalTrigger.evaluate(el => document.activeElement === el), `${shape.name}: Recent preview lost its own focus return`);
      const edit = recent.first().getByRole('button', { name: 'Edit alias for tm2', exact: true });
      await edit.click();
      const aliasDialog = recent.first().locator('.session-alias-dialog');
      await aliasDialog.getByRole('textbox', { name: 'Alias for tm2', exact: true }).fill('Kept draft');
      await aliasDialog.getByRole('textbox', { name: 'Alias for tm2', exact: true }).focus();
      await page.evaluate(() => document.querySelector('.dashboard-refresh').click());
      await page.waitForTimeout(180);
      assert(await aliasDialog.getByRole('textbox', { name: 'Alias for tm2', exact: true }).inputValue() === 'Kept draft', `${shape.name}: refresh discarded the Recent alias draft`);
      assert(await aliasDialog.getByRole('textbox', { name: 'Alias for tm2', exact: true }).evaluate(el => document.activeElement === el), `${shape.name}: refresh stole focus from Recent`);
      await aliasDialog.getByRole('button', { name: 'Close alias editor', exact: true }).click();
      await edit.click();
      await aliasDialog.getByRole('textbox', { name: 'Alias for tm2', exact: true }).fill('Build changed');
      await aliasDialog.getByRole('button', { name: 'Save alias for tm2', exact: true }).click();
      await page.waitForFunction(() => [...document.querySelectorAll('.session-card')].filter(card => card.querySelector('.session-tmux-name')?.textContent === 'tm2').every(card => card.querySelector('.session-name')?.textContent === 'Build changed'));
      assert(!await aliasDialog.isVisible() && await edit.evaluate(el => document.activeElement === el), `${shape.name}: Recent alias save did not close its own dialog and return focus`);
      await edit.click(); await aliasDialog.getByRole('textbox', { name: 'Alias for tm2', exact: true }).fill('Build');
      await aliasDialog.getByRole('button', { name: 'Save alias for tm2', exact: true }).click();
      await page.waitForFunction(() => [...document.querySelectorAll('.session-card')].filter(card => card.querySelector('.session-tmux-name')?.textContent === 'tm2').every(card => card.querySelector('.session-name')?.textContent === 'Build'));
      await recent.first().getByRole('button', { name: 'Session information for tm2', exact: true }).click();
      await mainRow.getByRole('button', { name: 'Session information for tm2', exact: true }).click();
      if (['phone', 'desktop'].includes(shape.name)) { phase = `${shape.name}:screenshot`; await page.evaluate(() => scrollTo(0, 0)); await page.waitForTimeout(400); await page.screenshot({ path: path.join(OUT, `${ENGINE}-${shape.name}-sessions.png`) }); }
      phase = `${shape.name}:settings`;
      await page.getByRole('button', { name: 'Settings', exact: true }).click();
      const count = page.getByRole('combobox', { name: 'Recent sessions on the Sessions tab', exact: true });
      assert(await count.inputValue() === '3', `${shape.name}: default Recent setting is not three`);
      const settingsContained = await page.locator('.dashboard-sessions-settings').evaluate(card => {
        const bounds = card.getBoundingClientRect();
        return [...card.querySelectorAll('select')].every(select => { const box = select.getBoundingClientRect(); return box.left >= bounds.left && box.right <= bounds.right; });
      });
      assert(settingsContained, `${shape.name}: Sessions settings controls overflow their card`);
      assert(await page.locator('.dashboard-toolbar .scrollback-control').count() === 0, `${shape.name}: default scrollback stayed in the session toolbar`);
      if (['phone', 'desktop'].includes(shape.name)) { phase = `${shape.name}:screenshot`; await page.evaluate(() => scrollTo(0, 0)); await page.waitForTimeout(400); await page.screenshot({ path: path.join(OUT, `${ENGINE}-${shape.name}-settings.png`) }); }
      await count.selectOption('0');
      assert(await page.locator('.dashboard-recent').evaluate(el => el.hidden), `${shape.name}: Off did not hide Recent at once`);
      await count.selectOption('5');
      assert(await recent.count() === 5, `${shape.name}: five did not update Recent at once`);
      await count.selectOption('3');
      if (ENGINE === 'chromium' && shape.name === 'desktop') {
        // Rows that Recent drops must be released. Cycle the count, collect
        // garbage, and count the detached session rows the heap still holds.
        phase = `${shape.name}:retention`;
        for (let i = 0; i < 20; i++) { await count.selectOption('0'); await count.selectOption('5'); }
        await count.selectOption('3');
        const cdp = await context.newCDPSession(page);
        await cdp.send('HeapProfiler.collectGarbage');
        const prototype = await cdp.send('Runtime.evaluate', { expression: 'HTMLElement.prototype' });
        const elements = await cdp.send('Runtime.queryObjects', { prototypeObjectId: prototype.result.objectId });
        const held = await cdp.send('Runtime.callFunctionOn', { objectId: elements.objects.objectId, returnByValue: true, functionDeclaration: 'function () { return this.filter(el => { try { return el.classList.contains("session-card") && !el.isConnected; } catch { return false; } }).length; }' });
        await cdp.detach();
        assert(!held.exceptionDetails, `${shape.name}: heap query failed: ${held.exceptionDetails?.text}`);
        assert(held.result.value === 0, `${shape.name}: ${held.result.value} removed session rows are still held after collection`);
      }
      await page.getByRole('button', { name: 'Sessions', exact: true }).click();
      phase = `${shape.name}:restart`;
      fixture.state.sessions = original.map(s => ({ ...s, authority: { ...s.authority, boot_id: 'restarted', server_pid: 99, server_start: 999, session_created: s.authority.session_created + 1000 } }));
      fixture.state.aliases = [];
      await page.getByRole('button', { name: 'Refresh sessions', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.dashboard-recent-rows .session-name')?.textContent === 'tm2');
      assert(await recent.count() === 3, `${shape.name}: restart lost uniquely named Recent sessions`);
      const duplicate = { ...fixture.state.sessions[0], authority: { ...fixture.state.sessions[0].authority, session_id: '$99', session_created: 9090 }, session_id: '$99' };
      fixture.state.sessions.push(duplicate);
      await page.getByRole('button', { name: 'Refresh sessions', exact: true }).click();
      await page.waitForFunction(() => document.querySelectorAll('.dashboard-content .session-card').length === 7);
      assert(!((await recent.locator('.session-name').allTextContents()).includes('tm2')), `${shape.name}: ambiguous same-name Recent guessed a successor`);
      phase = `${shape.name}:tabs`;
      await page.getByRole('button', { name: 'Workspaces', exact: true }).click();
      const layout = await page.locator('.dashboard-navigation').evaluate(nav => [...nav.children].map(el => { const range = document.createRange(); range.selectNodeContents(el); return { text: el.textContent, box: el.getBoundingClientRect().width, textWidth: range.getBoundingClientRect().width, textHeight: range.getBoundingClientRect().height, lineHeight: parseFloat(getComputedStyle(el).lineHeight), wrap: getComputedStyle(el).whiteSpace }; }));
      assert(layout.every(t => t.textWidth <= t.box && t.wrap === 'nowrap' && t.textHeight < t.lineHeight * 1.5), `${shape.name}: dashboard tabs wrap or truncate: ${JSON.stringify(layout)}`);
      if (shape.name === 'small') { phase = `${shape.name}:screenshot`; await page.evaluate(() => scrollTo(0, 0)); await page.waitForTimeout(400); await page.screenshot({ path: path.join(OUT, `${ENGINE}-small-workspaces.png`) }); }
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
      assert(!overflow, `${shape.name}: dashboard overflowed the viewport`);
      evidence.viewports.push({ ...shape, tabs: layout, overflow });
      fixture.state.sessions = original;
      fixture.state.aliases = original.slice(0, 3).map((s, i) => ({ alias_id: `recent-${i}`, display_alias: ['Build', 'Shell', 'Tasks'][i], revision: 1, state: 'active', realm: s.realm, server: s.server, session_name: s.name, session_incarnation: s.authority }));
      await context.close();
    }
    assert(evidence.pageErrors.length === 0, `Page errors: ${JSON.stringify(evidence.pageErrors)}`);
    assert(evidence.console.length === 0, `Console findings: ${JSON.stringify(evidence.console)}`);
    fs.writeFileSync(path.join(OUT, `recent-${ENGINE}.json`), JSON.stringify(evidence, null, 2));
    console.log(`Recent ${ENGINE}: PASS (${evidence.viewports.length} viewports, independent previews/dialogs, device settings, restart resolution)`);
  } finally { await browser.close(); await fixture.close(); }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
