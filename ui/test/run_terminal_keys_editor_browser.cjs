"use strict";
const fs = require("fs");
const path = require("path");
const http = require("http");
const UI = path.resolve(__dirname, "..");
const esbuild = require(path.join(UI, "node_modules/esbuild"));
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const SOURCE = process.env.PERSEA_KEYS_EDITOR_SOURCE || path.join(UI, "src");
const ENGINE = process.env.PERSEA_KEYS_ENGINE || "chromium";
function assert(ok, message) { if (!ok) throw new Error(message); }
async function main() {
  const bundle = esbuild.buildSync({ outdir: path.join(process.env.PERSEA_KEYS_EVIDENCE || "/tmp/agent_logs", "keys-editor-build"), nodePaths: [path.join(UI, "node_modules")], stdin: { contents: `
    import { TerminalKeysPanel } from ${JSON.stringify(path.join(SOURCE, "terminal_keys_panel"))};
    import { KeyboardPreferencesService, DEFAULT_KEYBOARD_PREFERENCES } from ${JSON.stringify(path.join(SOURCE, "keyboard_preferences"))};
    import { planKey } from ${JSON.stringify(path.join(SOURCE, "terminal_actions"))};
    import { syntheticCtrlReleaseEvent, syntheticKeydownEvent, unifiedKeyDescriptor } from ${JSON.stringify(path.join(SOURCE, "unified_key_bar"))};
    import { Terminal } from '@xterm/xterm';
    import ${JSON.stringify(path.join(UI, "test/terminal_keys_catalog_harness.ts"))};
    const terminal = new Terminal(); terminal.open(document.querySelector('#xterm'));
    window.sent = []; window.requests = []; window.remote = structuredClone(DEFAULT_KEYBOARD_PREFERENCES); window.revision = 0;
    window.service = new KeyboardPreferencesService({ csrf: ()=>'fixture-csrf', fetch: async (url, init) => {
      window.requests.push({method:init.method, headers:init.headers, body:init.body});
      if(init.method==='PUT') { window.remote=JSON.parse(init.body); window.revision++; }
      return new Response(JSON.stringify({...window.remote, revision:window.revision, stored:window.revision>0, available:true}), {headers:{ETag:'"'+window.revision+'"'}});
    }});
    let bytes=''; terminal.onData(data=>bytes+=data);
    window.panel = new TerminalKeysPanel({ generation:()=>0, isLive:()=>true, preferences:window.service,
      dispatch:(entry)=>{ bytes=''; const plan=planKey(entry.action.chord);
        if(typeof plan==='string') throw Error(plan);
        if(plan.prefixEscape) {terminal.textarea.dispatchEvent(syntheticKeydownEvent(unifiedKeyDescriptor('escape'))); terminal.textarea.dispatchEvent(syntheticCtrlReleaseEvent());}
        if(plan.character!==undefined) terminal.textarea.dispatchEvent(new InputEvent('input',{data:plan.character,inputType:'insertText',bubbles:true}));
        else terminal.textarea.dispatchEvent(syntheticKeydownEvent(plan.descriptor));
        terminal.textarea.dispatchEvent(syntheticCtrlReleaseEvent()); window.sent.push({id:entry.id,bytes}); return 'sent';
      }, changed:()=>{}, beforeChange:()=>{}, preferencesChanged:()=>{}, keyboardOpen:()=>false,
      settingsReturnFocus:()=>document.querySelector('#settings-return'), hideKeyboard:()=>{} });
    document.body.append(window.panel.element); window.panel.setOpen(true);
    window.ready=window.service.load();
    window.refresh=async()=>{window.revision++; window.remote.layout.bar=window.remote.layout.bar.slice().reverse(); await window.service.load();};
    `, resolveDir: UI, loader: "ts" }, bundle: true, write: false });
  const code = bundle.outputFiles.find(f=>f.path.endsWith('.js')).text;
  const css = bundle.outputFiles.filter(f=>f.path.endsWith('.css')).map(f=>f.text).join("\n");
  const server = http.createServer((req, res) => {
    if(req.url==='/favicon.ico') {res.writeHead(204);res.end();return;}
    res.setHeader("Content-Type", "text/html");
    res.end(`<!doctype html><style>${css}</style><input id="native"><button id="settings-return">Settings return</button><div id="xterm" style="position:absolute;left:-10000px"></div><script>${code}</script>`);
  });
  await new Promise(resolve=>server.listen(0,"127.0.0.1",resolve));
  const browser = await playwright[ENGINE].launch({headless:true,...(ENGINE==='chromium'?{executablePath:require("./browser_path.cjs")(),args:['--no-sandbox']}:{})});
  const evidence=[], errors=[];
  try {
    const page=await browser.newPage(); page.setDefaultTimeout(5000);
    page.on('pageerror',e=>errors.push(String(e))); page.on('console',m=>errors.push(`${m.type()}: ${m.text()}`));
    await page.goto(`http://127.0.0.1:${server.address().port}`); await page.evaluate(()=>window.ready);
    const reset=async()=>{await page.evaluate(()=>localStorage.clear());await page.reload();await page.evaluate(()=>window.ready);};
    const dialog=page.getByRole('dialog');
    const button=name=>dialog.getByRole('button',{name,exact:true});
    const click=id=>page.locator(`[data-key-control="${id}"]`).click();
    const open=async()=>{await click('customize');await dialog.waitFor({state:'visible'});};
    const begin=async()=>{await open();await button('Prefixes').click();await button('Change tmux prefix').click();};
    const empty=async()=>assert(await page.evaluate(()=>window.sent.length===0),'Settings must never dispatch terminal input');
    const favorite=async()=>{await page.evaluate(()=>window.panel.setOpen(true));await click('key:c:1');const sent=await page.evaluate(()=>window.sent);assert(JSON.stringify(sent)===JSON.stringify([{id:'key:c:1',bytes:'\x03'}]),`Favorite dispatch after editor: ${JSON.stringify(sent)}`);};
    const record=async(name,run)=>{await reset();try{await run();evidence.push({case:name,pass:true});}catch(error){evidence.push({case:name,pass:false,error:String(error)});}};
    if(process.env.PERSEA_KEYS_EDITOR_CASE!=='matrix') {
      await record('older device Favorites gain a working Alt key; later removal survives reload',async()=>{
        await page.setViewportSize({width:390,height:844});
        await page.evaluate(()=>localStorage.setItem('persea-terminal.keyboard-device.v1',JSON.stringify({version:1,layout:{bar:['key:tab:0','modifier:ctrl'],favorites:['key:c:1','key:f2:0']},prefixes:{Mine:'key:a:1'}})));
        await page.reload();await page.evaluate(()=>window.ready);
        assert(await page.locator('[data-key-control="modifier:alt"]').isVisible(),'Old saved Favorites show Alt on a phone');
        assert(await page.evaluate(()=>{const p=window.service.snapshot().effective;return JSON.stringify(p.layout.bar)==='["key:tab:0","modifier:ctrl"]'&&p.prefixes.Mine==='key:a:1'&&p.layout.favorites.includes('key:f2:0')&&!window.requests.some(r=>r.method==='PUT');}),'Upgrade preserves device choices without a shared write');
        await click('modifier:alt');await click('key:c:2');
        assert(await page.evaluate(()=>JSON.stringify(window.sent)===JSON.stringify([{id:'key:c:2',bytes:'\x1bc'}])),'Migrated Alt arms a real terminal chord');
        await open();await button('This device').click();await button('Favorites').click();await button('Remove Alt').click();await button('Save for this device').click();await button('Close keyboard settings').click();
        await page.reload();await page.evaluate(()=>window.ready);
        assert(await page.locator('[data-key-control="modifier:alt"]').count()===0,'Removing Alt is durable after upgrade');
        assert(await page.evaluate(()=>window.service.snapshot().effective.layout.favorites.includes('key:f2:0')),'Removing Alt keeps other favorites');
        await open();await button('This device').click();await button('Favorites').click();await dialog.getByLabel('Keyboard preset',{exact:true}).selectOption('Terminal');
        await button('Save for this device').click();await button('Close keyboard settings').click();
        await page.evaluate(()=>window.panel.setOpen(true));
        assert(await page.locator('[data-key-control="modifier:alt"]').isVisible(),'Applying the Terminal Favorites preset restores its Alt default');
        await page.setViewportSize({width:1280,height:720});
      });
      for(const exit of ['close','escape','Key bar','Favorites']) await record(`abandon prefix picker via ${exit}`,async()=>{
        await begin();await empty();
        if(exit==='escape')await page.keyboard.press('Escape');
        else {if(exit!=='close')await button(exit).click();await button('Close keyboard settings').click();}
        await favorite();
      });
      await record('prefix selection is draft-only; close confirmation keeps or discards edits',async()=>{
        await begin();await button('ctrl modifier').click();await button('Add Ctrl+a').click();await empty();
        assert(await page.evaluate(()=>window.service.snapshot().effective.prefixes.tmux==='key:b:1'),'Unsaved prefix does not change effective preferences');
        await button('Close keyboard settings').click();await button('Keep editing').click();
        assert(await dialog.isVisible(),'Keep editing preserves dialog');
        await button('Close keyboard settings').click();await button('Discard changes').click();await favorite();
      });
      await record('device prefix save persists through reload without shared write',async()=>{
        await open();await button('This device').click();await button('Prefixes').click();await button('Change tmux prefix').click();
        await button('ctrl modifier').click();await button('Add Ctrl+a').click();await button('Save for this device').click();await empty();
        assert(await page.evaluate(()=>window.service.snapshot().device.prefixes.tmux==='key:a:1'&&!window.requests.some(r=>r.method==='PUT')),'Device-only prefix save');
        await button('Close keyboard settings').click();await page.reload();await page.evaluate(()=>window.ready);
        assert(await page.evaluate(()=>window.service.snapshot().effective.prefixes.tmux==='key:a:1'),'Device prefix survives reload');await favorite();
      });
      await record('shared prefix save uses revisioned fixture',async()=>{
        await begin();await button('ctrl modifier').click();await button('Add Ctrl+a').click();await button('Save for all devices').click();
        await page.waitForFunction(()=>window.service.snapshot().revision===1);await empty();
        assert(await page.evaluate(()=>{const r=window.requests.find(r=>r.method==='PUT');return r.headers['If-Match']==='"0"'&&r.headers['X-Persea-CSRF']==='fixture-csrf'&&JSON.parse(r.body).prefixes.tmux==='key:a:1';}),'Shared save sends draft and expected revision');
      });
      await record('logical-key keyboard focus after modifier consumption',async()=>{
        await page.evaluate(()=>window.panel.showAll('Letters',true));await page.locator('[data-key-control="key:c:1"]').focus();await page.keyboard.press('Enter');
        assert(await page.evaluate(()=>document.activeElement?.getAttribute('data-key-control')==='key:c:0'),'Consumed Ctrl leaves logical C focused');
        await page.keyboard.press('Enter');assert(await page.evaluate(()=>JSON.stringify(window.sent)===JSON.stringify([{id:'key:c:1',bytes:'\x03'},{id:'key:c:0',bytes:'c'}])),'Next AT/keyboard activation emits unmodified C once');
      });
      await record('native editor draft focus and selection survive terminal key renders',async()=>{
        await page.evaluate(()=>window.panel.showAll('Letters'));
        await page.locator('#native').fill('native draft');await page.locator('#native').evaluate(n=>n.setSelectionRange(2,6));
        await click('modifier:ctrl');await click('key:c:1');
        assert(await page.locator('#native').evaluate(n=>n===document.activeElement&&n.value==='native draft'&&n.selectionStart===2&&n.selectionEnd===6),'Native focus, draft and selection retained');
      });
      await record('prefix name draft selection and exact focused node survive remote refresh',async()=>{
        await open();await button('Prefixes').click();const input=dialog.getByLabel('New prefix name',{exact:true});await input.fill('My new prefix');
        await input.evaluate(n=>{n.setSelectionRange(3,6,'backward');window.originalInput=n;});await page.evaluate(()=>window.refresh());
        assert(await input.evaluate(n=>n===window.originalInput&&n===document.activeElement&&n.value==='My new prefix'&&n.selectionStart===3&&n.selectionEnd===6&&n.selectionDirection==='backward'),'Remote refresh retains input node, focus, draft and selection');
        await page.keyboard.insertText('typed');assert(await input.inputValue()==='My typed prefix','Typing resumes in preserved selection');
      });
      await record('unsaved close confirmation supports keyboard-only continue and discard',async()=>{
        await begin();await button('ctrl modifier').click();await button('Add Ctrl+a').click();
        await page.keyboard.press('Escape');
        assert(await button('Keep editing').evaluate(n=>n===document.activeElement),'Unsaved close focuses Keep editing');
        await page.keyboard.press('Enter');assert(await dialog.isVisible(),'Keyboard continue keeps draft open');
        await page.keyboard.press('Escape');await page.keyboard.press('Tab');
        assert(await button('Discard changes').evaluate(n=>n===document.activeElement),'Discard is next keyboard stop');
        await page.keyboard.press('Enter');assert(!await dialog.isVisible(),'Keyboard discard closes dialog');await empty();await favorite();
      });
      await record('remote refresh defers conflict rendering until prefix name focus leaves',async()=>{
        await open();await button('Prefixes').click();const input=dialog.getByLabel('New prefix name',{exact:true});await input.fill('New draft');
        await page.evaluate(()=>window.refresh());await page.keyboard.press('Tab');
        await dialog.getByRole('region',{name:'Settings changed elsewhere'}).waitFor({state:'visible'});
        assert(await input.inputValue()==='New draft','Deferred conflict keeps unfinished prefix name');
        await button('Keep my edits').click();assert(await input.inputValue()==='New draft','Conflict review keeps unfinished prefix name');await empty();
      });
      await record('held Add prefix click survives deferred remote refresh focusout',async()=>{
        await open();await button('Prefixes').click();const input=dialog.getByLabel('New prefix name',{exact:true});await input.fill('Held draft');
        await input.evaluate(n=>n.setSelectionRange(2,5));await page.evaluate(()=>window.refresh());
        const add=button('Add prefix'), box=await add.boundingBox();assert(box,'Add prefix has a visible target');
        await page.mouse.move(box.x+box.width/2,box.y+box.height/2);await page.mouse.down();
        await page.waitForTimeout(40);await page.mouse.up();
        await dialog.getByRole('region',{name:'Choose a key'}).waitFor({state:'visible'});
        assert(await dialog.getByText('Choose the prefix key for Held draft.',{exact:true}).isVisible(),'Held click opens picker for retained prefix name');await empty();
      });
      for(const control of ['Add prefix','Change tmux prefix','ctrl modifier','alt modifier','shift modifier','Key group','Keyboard preset','Customize prefixes on this device']) await record(`keyboard focus continuity: ${control}`,async()=>{
        await open();let target;
        if(control==='Keyboard preset')target=dialog.getByLabel(control,{exact:true});
        else if(control==='Customize prefixes on this device'){await button('This device').click();target=dialog.getByLabel(control,{exact:true});}
        else {await button('Prefixes').click();if(['ctrl modifier','alt modifier','shift modifier','Key group'].includes(control))await button('Change tmux prefix').click();target=control==='Key group'?dialog.getByLabel(control,{exact:true}):button(control);}
        await target.focus();
        if(control==='Key group')await target.selectOption('Navigation');
        else if(control==='Keyboard preset')await target.selectOption('Terminal');
        else await page.keyboard.press('Space');
        assert(await target.evaluate(n=>document.activeElement===n),`${control} stays focused after its own rerender`);
        await page.keyboard.press('Tab');assert(await dialog.evaluate(n=>n.contains(document.activeElement)),`${control} permits next keyboard navigation`);await empty();
      });
    }
    if(process.env.PERSEA_KEYS_EDITOR_CASE!=='focus') await record('complete xterm key byte matrix',async()=>{
      const count=await page.evaluate(()=>window.runTerminalKeyCatalog());assert(count>500,'Full byte matrix executed');evidence.push({case:'matrix count',engine:ENGINE,count,pass:true});
    });
    assert(errors.length===0,`Browser diagnostics: ${errors.join('; ')}`);
    const failed=evidence.filter(e=>!e.pass);
    if(process.env.PERSEA_KEYS_EVIDENCE){fs.mkdirSync(process.env.PERSEA_KEYS_EVIDENCE,{recursive:true});fs.writeFileSync(path.join(process.env.PERSEA_KEYS_EVIDENCE,`keys-editor-${ENGINE}.json`),JSON.stringify({engine:ENGINE,evidence,errors},null,2));}
    console.log(`Keys editor ${ENGINE}: ${failed.length?'FAIL':'PASS'} (${evidence.filter(e=>e.case!=='matrix count').length} cases)`);
    for(const failure of failed)console.error(`${failure.case}: ${failure.error}`);
    assert(!failed.length,`${failed.length} failing cases`);
  } finally {await browser.close();await new Promise(resolve=>server.close(resolve));}
}
main().catch(error=>{console.error(error.stack||String(error));process.exitCode=1;});
