"use strict";

const fs = require("fs"), path = require("path");
const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright"));
const { startClipboardFixture, PNG } = require("./clipboard_fixture.cjs");
const ENGINE = process.env.PERSEA_CLIPBOARD_ENGINE || "chromium";
const ROOT = process.env.PERSEA_CLIPBOARD_EVIDENCE || "/tmp/agent_logs/persea-clipboard-expiry";
const SHAPES = process.env.PERSEA_CLIPBOARD_ONE_SHAPE ? [[390,844]] : [[320,568],[390,844],[430,932],[844,390],[1280,900]];
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
function assert(value, message) { if (!value) throw Error(message); }
async function until(check, message) { for (let i=0;i<100;i++) { if (await check()) return; await pause(40); } throw Error(message); }

async function main() {
  const directory = path.join(ROOT, `expiry-${ENGINE}-${new Date().toISOString().replace(/[:.]/g,"-")}`);
  fs.mkdirSync(directory, { recursive: true });
  const fixture = await startClipboardFixture(path.resolve(__dirname,".."), { tls: true, playwrightScreenshotStyle: true });
  const api = await playwright.request.newContext({ ignoreHTTPSErrors: true });
  const get = async route => (await api.get(fixture.origin+route)).json();
  const control = async body => api.post(fixture.origin+"/__fixture/control", { data: body });
  const imageControl = async body => api.post(fixture.origin+"/__clipboard/control", { data: body });
  const report = { engine: ENGINE, cases: [], console: [], errors: [], pass: false };
  let browser;
  try {
    browser = await playwright[ENGINE].launch({ headless: true, ...(ENGINE==="chromium" ? {executablePath:require("./browser_path.cjs")(),args:["--no-sandbox"]} : {}) });
    for (const [width,height] of SHAPES) {
      await control({reset:true}); await imageControl({reset:true});
      const context = await browser.newContext({viewport:{width,height},hasTouch:width<1000,isMobile:width<1000,ignoreHTTPSErrors:true});
      // Some native pickers consume the pointer release outside the document.
      // Model that event boundary while retaining trusted browser interaction.
      await context.addInitScript(() => {
        const nativePointers = new Set();
        document.addEventListener("pointerdown", event => { if(event.target instanceof HTMLSelectElement) nativePointers.add(event.pointerId); }, true);
        document.addEventListener("pointerup", event => { if(nativePointers.delete(event.pointerId)) event.stopImmediatePropagation(); }, true);
      });
      const page = await context.newPage(); page.setDefaultTimeout(6000);
      if(width===390) await page.route("**/api/**", async route => {
        if(route.request().method()==="PATCH") await pause(400);
        await route.continue();
      });
      page.on("console", m => report.console.push({width,height,type:m.type(),text:m.text()}));
      page.on("pageerror", e => report.errors.push({width,height,text:String(e)}));
      try {
        await page.goto(fixture.origin+"/");
        await page.getByRole("button",{name:"Open shared clipboard",exact:true}).click();
        const dialog = page.locator("dialog.persea-clipboard[open]");
        const button = name => dialog.getByRole("button",{name,exact:true});
        assert((await get("/api/clipboard/preferences")).default_retention_seconds===1800,"Fresh default must be 30 minutes");
        await button("Add text").click(); await page.getByLabel("Text to add to shared clipboard",{exact:true}).fill("Explicit expiry text"); await button("Save").click();
        await dialog.locator('[data-clipboard-item^="text:"]').waitFor();
        const fileChooser = page.waitForEvent("filechooser"); await button("Add image").click();
        await (await fileChooser).setFiles({name:"expiry.png",mimeType:"image/png",buffer:PNG});
        await dialog.locator('[data-clipboard-item^="image:"]').waitFor();
        for (const kind of ["text","image"]) {
          const records = async () => (await get(kind==="text"?"/api/snippets":"/api/clipboard/images")).items;
          const first = (await records())[0], id = first.id;
          const row = dialog.locator(`[data-clipboard-item="${kind}:${id}"]`);
          assert(first.retention_seconds===1800,"New "+kind+" must use 30 minute default");
          const choose = async (seconds,key,clockSkew=0) => {
            const old = (await records()).find(r=>r.id===id);
            const select = row.getByRole("combobox");
            await until(()=>dialog.getAttribute("aria-busy").then(v=>v!=="true"),"Previous selection completed");
            await select.evaluate(node=>{window.__expiryChanges=[];node.addEventListener("change",event=>window.__expiryChanges.push({trusted:event.isTrusted,value:node.value}),{capture:true});});
            const start = Date.now();
            // Open the native picker with a pointer before selecting. A bare
            // key press never exercises the OS popup's pointer ownership.
            await select.click();
            await select.press(key);
            // WebKit can commit and blur on the selection key itself. A
            // second Enter would activate the newly focused item instead.
            if (await page.evaluate(() => window.__expiryChanges.length === 0)) await page.keyboard.press("Enter");
            await until(async()=>{const r=(await records()).find(r=>r.id===id);return r?.revision>old.revision;},"Trusted selection saves "+kind+" "+seconds);
            const updated = (await records()).find(r=>r.id===id);
            assert(updated.revision===old.revision+1,"One selection writes once");
            assert(updated.retention_seconds===seconds,"Explicit "+kind+" choice must replace retention with "+seconds);
            if (seconds===0) assert(updated.expires_at===null,"No expiry clears deadline");
            else {
              const expiry = Date.parse(updated.expires_at);
              assert(expiry>=start+seconds*1000+clockSkew-20&&expiry<=Date.now()+seconds*1000+clockSkew+20,"Selected TTL starts now, without adding the previous remainder");
              assert(expiry-Date.parse(updated.updated_at)===seconds*1000,"Selected duration is exact");
            }
            const changes = await page.evaluate(()=>window.__expiryChanges);
            assert(changes.length===1&&changes[0].trusted&&changes[0].value===String(seconds),"Real browser change carries selected value once");
            await until(()=>row.locator(".persea-clipboard__expiry span").innerText().then(t=>seconds===0?t==="No expiry":seconds===14400?/^4 h/.test(t):/^30 min/.test(t)),"Expiry badge reflects exact saved choice without another click");
            assert(await dialog.isVisible(),"Expiry selection keeps clipboard open");
            assert(await dialog.locator("textarea").count()===0,"Expiry selection does not open editor");
            report.cases.push({width,height,kind,from:old.retention_seconds,to:seconds,pass:true});
          };
          await choose(0,"End");
          const disabled = await row.getByRole("combobox").locator("option").evaluateAll(options=>options.filter(o=>o.value!==""&&o.disabled).map(o=>o.value));
          assert(disabled.length===0,"No expiry must allow every finite option: "+disabled.join(","));
          await choose(1800,"Home");
          await choose(14400,"4");
          await choose(1800,"Home");
          await pause(80); await choose(1800,"Home");
          await choose(0,"End");
          await choose(1800,"Home");
          if (width===390) {
            const select = row.getByRole("combobox"); await select.focus();
            await select.evaluate(node=>{window.__expirySelect=node;});
            await control({snippetWrite:[{kind:"clip",body:"Background publication "+kind}]}); await pause(4400);
            assert(await page.evaluate(()=>window.__expirySelect.isConnected&&document.activeElement===window.__expirySelect),"Polling preserves focused native expiry control");
            await choose(14400,"4"); await choose(1800,"Home");
            if(kind==="image") {
              await imageControl({imageClockSkewMs:1000});
              await choose(14400,"4",1000);
              await imageControl({imageClockSkewMs:0});
            }
          }
        }
        await page.screenshot({path:path.join(directory,`${width}x${height}.png`),fullPage:true});
        const box = await dialog.boundingBox();
        assert(box&&box.x>=-1&&box.y>=-1&&box.x+box.width<=width+1&&box.y+box.height<=height+1,"Expiry controls remain in viewport");
        await button("Close clipboard").click();
      } catch(error) {
        report.failure = {width,height,error:String(error)};
        report.failure.state = await page.evaluate(() => ({ title: document.querySelector("dialog h2")?.textContent, focus: document.activeElement?.tagName, changes: window.__expiryChanges }));
        await page.screenshot({path:path.join(directory,"failure.png"),fullPage:true}).catch(()=>{});
        throw error;
      } finally { await context.close(); }
    }
    assert(report.console.length===0&&report.errors.length===0,"Expiry interactions keep console clean");
    report.pass = true;
  } finally {
    fs.writeFileSync(path.join(directory,"results.json"),JSON.stringify(report,null,2));
    console.log(JSON.stringify({engine:ENGINE,directory,pass:report.pass,cases:report.cases.length,failure:report.failure}));
    if(browser) await browser.close(); await api.dispose(); await fixture.close();
  }
}
main().catch(error=>{console.error(error.stack||String(error));process.exitCode=1;});
