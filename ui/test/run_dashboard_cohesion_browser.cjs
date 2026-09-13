'use strict';
const fs = require('fs'), path = require('path');
const { startCohesionFixture, session, scope } = require('./dashboard_cohesion_fixture.cjs');
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const ENGINE = process.env.PERSEA_DASHBOARD_ENGINE || 'chromium';
const UI = process.env.PERSEA_DASHBOARD_UI || path.resolve(__dirname, '..');
const OUT = path.join(process.env.PERSEA_DASHBOARD_EVIDENCE || '/tmp/agent_logs/dashboard-cohesion', `cohesion-${ENGINE}`);
const assert = (value, message) => { if (!value) throw Error(message); };

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const fixture = await startCohesionFixture(UI);
  const browser = await playwright[ENGINE].launch({ headless: true, ...(ENGINE === 'chromium' ? { executablePath: require("./browser_path.cjs")(), args: ['--no-sandbox'] } : {}) });
  const evidence = { engine: ENGINE, checks: [], console: [], errors: [], viewports: [] }; let phase = 'setup';
  const context = await browser.newContext({ viewport: { width: 1280, height: 960 }, ignoreHTTPSErrors: true });
  const capture = page => {
    page.setDefaultTimeout(7000);
    page.on('console', message => evidence.console.push({ phase, type: message.type(), text: message.text().slice(0, 500), url: message.location().url }));
    page.on('pageerror', error => evidence.errors.push({ phase, text: error.message }));
  };
  const page = await context.newPage(); capture(page);
  const row = (name, realm = 'local', target = page) => target.locator(`.realm-card[data-realm="${realm}"] .session-card`).filter({ has: target.getByRole('heading', { name, exact: true }) });
  const count = route => fixture.state.requests.filter(request => request.path === route).length;
  const previewsFor = id => fixture.state.requests.filter(request => request.path === '/api/session-previews' && new URLSearchParams(request.query).get('session_id') === id).length;
  const ready = async target => { await target.locator('.session-card').first().waitFor(); await target.waitForFunction(() => !document.querySelector('.session-card .session-pin').disabled); };
  const refresh = async () => {
    const response = page.waitForResponse(response => response.url().endsWith('/api/inventory'));
    await page.getByRole('button', { name: 'Refresh session list', exact: true }).click(); await response;
    await page.waitForFunction(() => !document.querySelector('.dashboard-refresh').disabled);
  };
  const section = async name => { await page.locator('.dashboard-navigation').getByRole('button', { name, exact: true }).click(); };
  const screenshot = async name => { const previous = phase; phase = 'screenshot'; await page.screenshot({ path: path.join(OUT, `${name}.png`), animations: 'disabled' }); phase = previous; };
  const check = async (name, run) => {
    if (process.env.PERSEA_DASHBOARD_CASE && !name.includes(process.env.PERSEA_DASHBOARD_CASE)) return;
    phase = name;
    try { await run(); evidence.checks.push({ name, pass: true }); }
    catch (error) { evidence.checks.push({ name, pass: false, error: String(error) }); await screenshot(`failure-${evidence.checks.length}`).catch(() => undefined); }
  };
  try {
    const remembered = fixture.state.sessions[0];
    await context.addInitScript(record => localStorage.setItem('persea-terminal.last-session.v1', JSON.stringify(record)), { draftScope: scope(remembered), name: remembered.name, realm: remembered.realm, server: remembered.server, at: Date.now() });
    await page.clock.install({ time: new Date() }); await page.goto(fixture.origin); await ready(page);
    await check('consistent rows and Resume have explicit matching actions and live metadata', async () => {
      assert(await page.locator('.unified-dev-launch, .session-legacy, .session-detail-actions').count() === 0, 'Development or legacy entry point remains');
      assert(await page.locator('.session-create').count() === 1, 'Creation has duplicate entry forms');
      assert((await page.locator('.server-heading h2').allTextContents()).join(',') === 'local_operator,other_operator', 'Infrastructure labels leaked into single-server user headings');
      const actions = page.locator('.session-open-action');
      for (const action of await actions.all()) { assert(await action.locator('svg').count() === 1 && await action.innerText() === 'Open', 'Open controls differ'); }
      assert((await page.locator('.landing-card-kicker').innerText()).includes('Last opened on this device'), 'Resume selection is unexplained');
      assert((await page.locator('.landing-card .session-metadata').innerText()) === (await row('qt1').locator('.session-metadata').innerText()), 'Resume omitted live metadata');
      const before = page.url(), requests = fixture.state.requests.filter(request => request.method !== 'GET').length;
      await page.locator('.landing-card-body').click();
      assert(page.url() === before && fixture.state.requests.filter(request => request.method !== 'GET').length === requests, 'Passive Resume body performed an action');
      assert(await row('qt1').locator('.session-pin').isVisible() && await row('qt1').locator('.session-detail').isHidden(), 'Favorite requires Details');
      const gap = await page.evaluate(() => document.querySelector('.dashboard-results').getBoundingClientRect().top - document.querySelector('.dashboard-toolbar').getBoundingClientRect().bottom);
      assert(gap >= 10, 'Session count touches the filter controls');
      assert((await page.locator('.dashboard-status').textContent()) === '', 'Unexplained refresh timestamp remains');
    });

    await check('snapshot lifetime is independent of inventory refresh and window return', async () => {
      const sessionRow = row('qt1'); await sessionRow.locator('.session-disclosure').click();
      await sessionRow.locator('.session-preview-thumbnail:not(:disabled)').waitFor();
      const previews = previewsFor('$1'), inventory = count('/api/inventory');
      await page.evaluate(() => { window.previewWitness = document.querySelector('.session-card .session-preview-screen'); document.dispatchEvent(new Event('visibilitychange')); window.dispatchEvent(new Event('pageshow')); });
      assert(count('/api/inventory') === inventory, 'A quick window return refreshed a recent inventory');
      await refresh();
      const automatic = page.waitForResponse(response => response.url().endsWith('/api/inventory')); await page.clock.fastForward(61_000); await automatic;
      assert(previewsFor('$1') === previews, 'Inventory or foreground refresh reloaded a snapshot');
      assert(await page.evaluate(() => window.previewWitness === document.querySelector('.session-card .session-preview-screen')), 'Snapshot DOM was replaced');
      await sessionRow.locator('.session-disclosure').click(); await sessionRow.locator('.session-disclosure').click();
      assert(previewsFor('$1') === previews, 'Reopening Details refetched a cached snapshot');
      const green = await sessionRow.locator('.session-preview .session-preview-screen span').filter({ hasText: 'Build passed' }).evaluate(node => getComputedStyle(node).color);
      assert(green !== await sessionRow.locator('.session-preview .session-preview-screen').evaluate(node => getComputedStyle(node).color), 'Thumbnail lost terminal colors');
      assert(await sessionRow.locator('.session-preview img').count() === 0, 'Preview text became markup');
      await sessionRow.locator('.session-preview-thumbnail').click(); await page.getByRole('dialog').waitFor();
      assert(previewsFor('$1') === previews, 'Enlarging a snapshot triggered another capture');
      fixture.state.previewDelay = 350; fixture.state.previewVersion++;
      const reply = page.waitForResponse(response => response.url().includes('/api/session-previews'));
      await page.getByRole('dialog').getByRole('button', { name: 'Refresh preview' }).click();
      assert((await page.getByRole('dialog').locator('.session-preview-screen').innerText()).includes('Snapshot 1'), 'Refresh blanked the displayed snapshot');
      await reply; await page.waitForFunction(() => document.querySelector('dialog .session-preview-screen').textContent.includes('Snapshot 2'));
      fixture.state.previewDelay = 0;
      await screenshot('preview-desktop');
      await page.keyboard.press('Escape');
      assert(await sessionRow.locator('.session-preview-thumbnail').evaluate(node => node === document.activeElement), 'Dialog did not restore focus');
    });

    await check('alias editing stays beside the collapsed name and closes after save', async () => {
      const card = row('qt1');
      if (await card.locator('.session-detail').isVisible()) await card.locator('.session-disclosure').click();
      assert(await card.locator('.session-alias-edit').isVisible() && await card.locator('.alias-editor').isHidden(), 'Alias editor crowds the collapsed row');
      await card.getByRole('button', { name: 'Edit alias for qt1', exact: true }).click();
      const editor = card.locator('.session-alias-dialog');
      await editor.getByRole('textbox', { name: 'Alias for qt1', exact: true }).fill('Build worker');
      await editor.getByRole('button', { name: 'Save alias for qt1', exact: true }).click();
      await page.waitForFunction(() => !document.querySelector('.session-alias-dialog').open);
      assert(await card.locator('.alias-badge').innerText() === 'Build worker', 'Saved alias did not appear beside the session');
      assert(await card.locator('.session-name').innerText() === 'qt1' && await card.locator('.session-detail').isHidden(), 'Editing an alias renamed tmux or expanded the row');
      assert(await card.locator('.session-alias-edit').evaluate(node => document.activeElement === node), 'Alias save lost return focus');
      await card.locator('.session-alias-edit').click();
      await editor.getByRole('button', { name: 'Clear alias for qt1', exact: true }).click();
      await page.waitForFunction(() => !document.querySelector('.session-alias-dialog').open);
      assert(await card.locator('.alias-badge').isHidden(), 'Clearing an alias did not update the row');
    });

    await check('row previews and the single information panel reflow beside balanced Resume actions', async () => {
      const phoneContext = await browser.newContext({ viewport: { width: 390, height: 844 }, ignoreHTTPSErrors: true });
      const phone = await phoneContext.newPage(); capture(phone);
      try {
        await phone.goto(fixture.origin); await ready(phone);
        const card = row('qt10', 'local', phone);
        const initial = previewsFor('$10');
        assert(await card.locator('.session-detail').isHidden() && await card.locator('.session-preview-row-button').isVisible(), 'Preview requires row expansion');
        assert(await card.locator('.session-preview-row-screen').isHidden(), 'Narrow row did not use an eye icon');
        await card.locator('.session-preview-row-button').click();
        await phone.waitForFunction(() => document.querySelector('.session-preview-dialog .session-preview-screen')?.textContent.includes('Build passed'));
        assert(previewsFor('$10') === initial + 1, 'The row preview did not make one capture');
        await phone.keyboard.press('Escape');
        assert(await card.locator('.session-preview-row-button').evaluate(node => node === document.activeElement), 'Preview did not return focus to its row button');
        await card.locator('.session-disclosure').click();
        assert(await card.locator('.session-information > summary, .session-detail .alias-editor').count() === 0, 'Expanded row still contains nested disclosure or alias editing');
        assert(previewsFor('$10') === initial + 1, 'Information expansion duplicated the row capture');
        assert(await card.evaluate(node => node.querySelector('.session-preview').getBoundingClientRect().bottom <= node.querySelector('.session-information').getBoundingClientRect().top), 'Phone information does not put the preview first');
        await phone.setViewportSize({ width: 1440, height: 1000 });
        assert(await card.evaluate(node => node.querySelector('.session-preview').getBoundingClientRect().left >= node.querySelector('.session-information').getBoundingClientRect().right), 'Desktop preview is not beside session information');
      } finally { await phoneContext.close(); }
      for (const width of [320, 390, 1440]) {
        await page.setViewportSize({ width, height: 900 });
        assert(await page.locator('.landing-card').evaluate(node => { const body = node.querySelector('.landing-card-body').getBoundingClientRect(), actions = node.querySelector('.landing-card-actions').getBoundingClientRect(); return actions.left >= body.right && Math.abs((actions.top + actions.bottom - body.top - body.bottom) / 2) < 2; }), `Resume actions fell below the identity at ${width}`);
      }
      await page.setViewportSize({ width: 1280, height: 960 });
    });

    await check('scrollback choice is preserved in launch links while shared history keeps its full import', async () => {
      const openingContext = await browser.newContext({ viewport: { width: 390, height: 844 }, ignoreHTTPSErrors: true });
      const opening = await openingContext.newPage(); capture(opening);
      try {
        await opening.goto(fixture.origin); await ready(opening);
        const choice = opening.getByRole('combobox', { name: 'Scrollback rows', exact: true });
        await choice.selectOption('5000');
        for (const action of await opening.locator('a.session-open-action').all()) assert(new URLSearchParams(new URL(await action.getAttribute('href'), fixture.origin).hash.slice(1)).get('history') === '5000', 'An Open action ignored the selected scrollback');
        await opening.reload(); await ready(opening); assert(await choice.inputValue() === '5000', 'Scrollback choice did not persist');
        let adoption;
        await opening.route('**/api/session-adoptions', async route => { adoption = route.request().postDataJSON(); await route.fulfill({ status: 200, contentType: 'application/json', body: '{}' }); });
        await opening.route('**/terminal?**', route => route.fulfill({ status: 200, contentType: 'text/html', body: '<!doctype html><title>Opened</title>' }));
        await row('automation', 'smith', opening).locator('.session-open-action').click();
        await opening.waitForURL('**/terminal?**');
        assert(adoption?.history_rows === 10000 && adoption.realm === 'smith' && adoption.session_id === '$1', 'The device viewing limit reduced the exact session shared history import');
        assert(new URLSearchParams(new URL(opening.url()).hash.slice(1)).get('history') === '5000', 'Adopt-and-open lost the selected scrollback');
      } finally { await openingContext.close(); }
    });

    await check('shared favorites survive separate browser storage and concurrent changes', async () => {
      const otherContext = await browser.newContext({ viewport: { width: 390, height: 844 }, ignoreHTTPSErrors: true });
      const other = await otherContext.newPage(); capture(other); await other.goto(fixture.origin); await ready(other);
      try {
        await row('qt1').locator('.session-pin').click(); await page.waitForFunction(() => document.querySelector('.session-card .session-pin').getAttribute('aria-busy') === 'false');
        await row('qt2', 'local', other).locator('.session-pin').click();
        await other.waitForFunction(() => [...document.querySelectorAll('.session-pin')].every(node => node.getAttribute('aria-busy') === 'false'));
        assert(fixture.state.favorites.favorites.includes(scope(fixture.state.sessions[0])) && fixture.state.favorites.favorites.includes(scope(fixture.state.sessions[1])), 'Concurrent device save lost a favorite');
        await refresh();
        assert(await row('qt2').locator('.session-pin').getAttribute('aria-pressed') === 'true', 'Dashboard did not synchronize the other device');
        await page.reload(); await ready(page);
        assert(await row('qt1').locator('.session-pin').getAttribute('aria-pressed') === 'true', 'Favorite did not survive reload');
      } finally { await otherContext.close(); }
      const oldScope = scope(fixture.state.sessions[0]);
      fixture.state.sessions[0] = session('local', 1, { authority: { ...fixture.state.sessions[0].authority, session_created: 99999 } });
      await refresh();
      assert(await row('qt1').locator('.session-pin').getAttribute('aria-pressed') === 'false' && fixture.state.favorites.favorites.includes(oldScope), 'A replacement session inherited an old favorite');
    });

    await check('one creation form explicitly selects a user and saves name plus optional alias', async () => {
      await page.getByRole('button', { name: 'New session', exact: true }).click();
      const form = page.locator('.session-create');
      assert(await form.locator('select').inputValue() === '', 'Creation silently selected the first user');
      await form.getByRole('textbox', { name: 'New tmux session name', exact: true }).fill('cohesion_created');
      await form.getByRole('textbox', { name: 'New session alias (optional)', exact: true }).fill('Research notes');
      const creates = count('/api/sessions'); await form.getByRole('button', { name: 'Create session', exact: true }).click();
      assert(count('/api/sessions') === creates, 'Creation proceeded without a user choice');
      await form.locator('select').selectOption(JSON.stringify(['smith', 'default']));
      await refresh();
      assert(await form.locator('input[name="name"]').inputValue() === 'cohesion_created' && await form.locator('select').inputValue() === JSON.stringify(['smith', 'default']), 'Refresh lost the creation draft');
      await form.getByRole('button', { name: 'Create session', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.session-create-status').textContent.includes('as Research notes'));
      assert(count('/api/sessions') === creates + 1, 'Creation was duplicated');
      const request = fixture.state.requests.filter(request => request.path === '/api/sessions').at(-1);
      assert(JSON.stringify(request.body) === JSON.stringify({ realm: 'smith', server: 'default', name: 'cohesion_created' }), 'Creation did not use the selected user or sent extra execution fields');
      assert((await row('cohesion_created', 'smith').locator('.alias-badge').innerText()) === 'Research notes', 'Optional alias was not applied');
      await page.getByRole('button', { name: 'Close new session form', exact: true }).click();
    });

    await check('alias recovery never repeats creation or binds a replacement session', async () => {
      const form = page.locator('.session-create');
      for (const recovery of ['retry', 'lost-reply', 'replacement']) {
        await page.getByRole('button', { name: 'New session', exact: true }).click();
        await form.locator('select').selectOption(JSON.stringify(['smith', 'default']));
        const name = `alias_${recovery.replace('-', '_')}`;
        await form.locator('input[name="name"]').fill(name);
        await form.locator('input[name="display_alias"]').fill('Recovery alias');
        fixture.state.aliasStatus = recovery === 'lost-reply' ? 200 : 503;
        fixture.state.aliasCommitThenFail = recovery === 'lost-reply';
        const creates = count('/api/sessions');
        await form.getByRole('button', { name: 'Create session', exact: true }).click();
        await form.getByRole('button', { name: 'Retry saving alias', exact: true }).waitFor();
        assert(count('/api/sessions') === creates + 1 && await form.locator('input[name="display_alias"]').inputValue() === 'Recovery alias', 'Alias failure lost the draft or repeated creation');
        const aliasRequests = count('/api/aliases');
        fixture.state.aliasStatus = 200;
        if (recovery === 'replacement') {
          const created = fixture.state.others.find(row => row.name === name);
          created.authority = { ...created.authority, session_created: created.authority.session_created + 1 };
        }
        await form.getByRole('button', { name: 'Retry saving alias', exact: true }).click();
        await page.waitForFunction(replacement => {
          const text = document.querySelector('.session-create-status').textContent;
          return text.includes(replacement ? 'no longer available' : 'as Recovery alias');
        }, recovery === 'replacement');
        assert(count('/api/sessions') === creates + 1, 'Alias retry created another session');
        assert(count('/api/aliases') === aliasRequests + (recovery === 'retry' ? 1 : 0), 'Alias retry duplicated a committed save or wrote to a replacement');
        await page.getByRole('button', { name: 'Close new session form', exact: true }).click();
      }
    });

    await check('preview failure retains output and removing its session closes the modal', async () => {
      const previewRow = row('qt3'); await previewRow.locator('.session-disclosure').click();
      await previewRow.locator('.session-preview-thumbnail:not(:disabled)').waitFor();
      await previewRow.locator('.session-preview-thumbnail').click();
      const previous = await page.getByRole('dialog').locator('.session-preview-screen').innerText();
      fixture.state.previewStatus = 503;
      await page.getByRole('dialog').getByRole('button', { name: 'Refresh preview' }).click();
      await page.waitForFunction(() => !!document.querySelector('dialog .session-preview-notice')?.textContent);
      assert(await page.getByRole('dialog').locator('.session-preview-screen').innerText() === previous, 'Failed refresh discarded the last snapshot');
      fixture.state.previewStatus = 200;
      fixture.state.sessions = fixture.state.sessions.filter(row => row.name !== 'qt3');
      // A modal makes the background inert; the regular inventory timer must
      // still retire an ended session and its pending dialog safely.
      const response = page.waitForResponse(reply => reply.url().endsWith('/api/inventory'));
      await page.clock.fastForward(61_000); await response;
      await page.waitForFunction(() => document.querySelector('dialog.session-preview-dialog') === null);
      assert(await previewRow.count() === 0, 'An ended session retained its preview row');
    });

    await check('settings use uniform collapsible panels with inset controls', async () => {
      await section('Settings');
      const panels = page.locator('.dashboard-settings > details:not([hidden])'); assert(await panels.count() >= 2, 'Settings do not use consistent panels');
      for (const panel of await panels.all()) {
        if (!await panel.evaluate(node => node.open)) await panel.locator(':scope > summary').click();
        const inset = await panel.evaluate(node => { const bounds = node.getBoundingClientRect(); return [...node.querySelectorAll('button,input,select')].every(control => { const box = control.getBoundingClientRect(); return box.left >= bounds.left + 12 && box.right <= bounds.right - 12 && box.bottom <= bounds.bottom - 12; }); });
        assert(inset, 'A settings control touches the panel edge');
      }
      await screenshot('settings-desktop');
      await page.getByRole('button', { name: 'Customize keyboard', exact: true }).click(); await page.getByRole('dialog').waitFor();
      await page.keyboard.press('Escape'); await section('Sessions');
    });

    await check('responsive rows forms settings and colored modal stay contained', async () => {
      for (const [width, height] of [[320, 568], [390, 844], [430, 932], [844, 390], [1024, 768], [1440, 1000], [1920, 1080]]) {
        await page.setViewportSize({ width, height }); await section('Sessions');
        const metrics = await page.evaluate(() => ({ width: innerWidth, scroll: document.documentElement.scrollWidth, small: [...document.querySelectorAll('button,a[href],input,select,summary')].filter(node => node.getClientRects().length && (node.getBoundingClientRect().width < 43.9 || node.getBoundingClientRect().height < 43.9)).map(node => node.getAttribute('aria-label') || node.textContent.slice(0, 40)), metadata: [...document.querySelectorAll('.session-metadata')].filter(node => node.getClientRects().length).every(node => parseFloat(getComputedStyle(node).fontSize) >= 12) }));
        evidence.viewports.push(metrics); assert(metrics.scroll <= width + 1 && metrics.small.length === 0 && metrics.metadata, `Rows overflow or targets shrink at ${width}: ${JSON.stringify(metrics)}`);
        if ([390, 1440].includes(width)) await screenshot(`sessions-${width}`);
        await page.getByRole('button', { name: 'New session', exact: true }).click();
        assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `Creation overflows at ${width}`);
        if ([390, 1440].includes(width)) await screenshot(`creation-${width}`);
        await page.getByRole('button', { name: 'Close new session form', exact: true }).click();
        await section('Settings'); assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `Settings overflow at ${width}`);
        if (width === 390) await screenshot('settings-phone');
      }
      await section('Sessions'); await page.setViewportSize({ width: 390, height: 844 });
      await row('qt1').locator('.session-disclosure').click(); await row('qt1').locator('.session-preview-thumbnail:not(:disabled)').waitFor();
      await row('qt1').locator('.session-preview-thumbnail').click();
      assert(await page.getByRole('dialog').evaluate(node => { const box = node.getBoundingClientRect(); return box.left >= 0 && box.right <= innerWidth && box.top >= 0 && box.bottom <= innerHeight; }), 'Preview dialog exceeds the phone viewport');
      await screenshot('preview-phone'); await page.keyboard.press('Escape');
      await row('qt1').locator('.session-disclosure').click();
      await page.evaluate(() => { document.documentElement.style.fontSize = '200%'; });
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), '200% text overflows'); await screenshot('text-200');
      await page.evaluate(() => { document.documentElement.style.fontSize = ''; });
    });
  } finally {
    evidence.unexpectedConsole = evidence.console.filter(item => !(item.phase.startsWith('shared favorites') && /\b412\b/.test(item.text)) && !(/^(alias recovery|preview failure)/.test(item.phase) && /\b503\b/.test(item.text)) && !(item.phase === 'screenshot' && /Refused to apply a stylesheet.*style-src/.test(item.text)));
    evidence.finished = true; evidence.pass = evidence.checks.length > 0 && evidence.checks.every(check => check.pass) && evidence.errors.length === 0 && evidence.unexpectedConsole.length === 0;
    evidence.requestCounts = Object.fromEntries([...new Set(fixture.state.requests.map(request => request.path))].map(route => [route, count(route)]));
    fs.writeFileSync(path.join(OUT, 'result.json'), JSON.stringify(evidence, null, 2));
    await context.close(); await browser.close(); await fixture.close();
    console.log(`${ENGINE}: ${evidence.checks.filter(check => check.pass).length}/${evidence.checks.length} checks passed; ${evidence.errors.length} page errors; ${evidence.unexpectedConsole.length} unexpected console messages. ${path.join(OUT, 'result.json')}`);
    if (!evidence.pass) process.exitCode = 1;
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
