"use strict";

const fs=require("fs"),path=require("path"),crypto=require("crypto");
const playwright=require(process.env.PERSEA_PLAYWRIGHT_MODULE||require.resolve("playwright"));
const {startClipboardFixture,installClipboardMock,PNG}=require("./clipboard_fixture.cjs");
const UI=path.resolve(__dirname,"..");
const ROOT=process.env.PERSEA_CLIPBOARD_EVIDENCE||"/tmp/agent_logs/persea-clipboard-browser";
const ENGINE=process.env.PERSEA_CLIPBOARD_ENGINE||"chromium";
const SHAPES=process.env.PERSEA_CLIPBOARD_ONE_SHAPE?[[390,844]]:[[320,568],[390,844],[430,932],[844,390],[1280,900]];
const pause=ms=>new Promise(resolve=>setTimeout(resolve,ms));
function assert(value,message){if(!value)throw Error(message);}
async function until(check,message){for(let i=0;i<150;i++){if(await check())return;await pause(30);}throw Error(message);}

async function main(){
  const directory=path.join(ROOT,`browser-${ENGINE}-${new Date().toISOString().replace(/[:.]/g,"-")}`);
  const bundle=path.join(directory,"ui");fs.mkdirSync(bundle,{recursive:true});
  fs.cpSync(process.env.PERSEA_CLIPBOARD_DIST||path.join(UI,"dist"),path.join(bundle,"dist"),{recursive:true});
  const mutant=process.env.PERSEA_CLIPBOARD_MUTANT;
  if(mutant){
    const file=path.join(bundle,"dist/app.js");let source=fs.readFileSync(file,"utf8");
    if(mutant==="feedback-flow"){
      fs.appendFileSync(path.join(bundle,"dist/app.css"),'\n.persea-clipboard__feedback-anchor { height: auto; } .persea-clipboard__feedback { position: static; display: none; } .persea-clipboard__feedback[data-visible="true"] { display: grid; }\n');
    }else if(mutant==="cancel"){
      const pattern=/const cancel = this\.button\("Cancel", \(\) => this\.showList\("text:" \+ (\w+)\.id\)\);/;
      assert(pattern.test(source),"Cancel mutation anchor");source=source.replace(pattern,'const cancel = this.button("Cancel", (event) => { this.options.terminal?.send(input.value, event); this.showList("text:" + $1.id); });');
    }else if(mutant==="send"){
      const pattern=/const result = this\.options\.terminal\.send\((\w+)\.body, event\);/;
      assert(pattern.test(source),"Send mutation anchor");source=source.replace(pattern,'const result = this.options.terminal.send($1.body, event); this.options.terminal.send($1.body, event);');
    }else if(mutant==="stale"){
      const index=source.indexOf("async attachImage("),from="if (lifetime !== this.lifetime || !this.element.open) return;";assert(index>=0&&source.slice(index).includes(from),"Stale mutation anchor");source=source.slice(0,index)+source.slice(index).replace(from,"/* mutation: stale image completion accepted */");
    }else if(mutant==="receipt-font"){
      const anchor="this.typographyRoot.hidden = receipt;";assert(source.includes(anchor),"Receipt font mutation anchor");source=source.replace(anchor,"this.typographyRoot.hidden = false;");
    }else throw Error("Unknown mutant");
    fs.writeFileSync(file,source);
  }
  const hashes={};for(const name of ["app.js","app.css","index.html"]){hashes[name]=crypto.createHash("sha256").update(fs.readFileSync(path.join(bundle,"dist",name))).digest("hex");}
  const evidence={engine:ENGINE,directory,hashes,mutant,cases:[],pages:[],hardwarePending:["Native iPhone software keyboard, accessory bar and OS clipboard UI"]};
  const tmp=fs.mkdtempSync("/tmp/agent_logs/cb-");process.env.TMPDIR=tmp;
  const fixture=await startClipboardFixture(bundle,{tls:true,playwrightScreenshotStyle:true});
  const api=await playwright.request.newContext({ignoreHTTPSErrors:true});
  const get=async route=>(await api.get(fixture.origin+route)).json();
  const control=async body=>(await api.post(fixture.origin+"/__fixture/control",{data:body})).json();
  const imageControl=async body=>(await api.post(fixture.origin+"/__clipboard/control",{data:body})).json();
  let browser;
  try{browser=await playwright[ENGINE].launch({headless:true,...(ENGINE==="chromium"?{executablePath:require("./browser_path.cjs")(),args:["--no-sandbox"]}:{})});}
  catch(error){await api.dispose();await fixture.close();fs.rmSync(tmp,{recursive:true,force:true});throw error;}
  let activePage,phase="start";
  try{
    for(const [width,height]of SHAPES){
      await control({reset:true,imageStaging:true,switchSessions:true});await imageControl({reset:true});
      const contexts=[];
      const newPage=async label=>{
        const context=await browser.newContext({viewport:{width,height},hasTouch:width<1000,isMobile:width<1000,colorScheme:'dark',ignoreHTTPSErrors:true});contexts.push(context);
        await context.addInitScript(value=>{window.__clipboardPNG=value;},PNG.toString("base64"));
        await context.addInitScript(installClipboardMock);
        const page=await context.newPage();page.setDefaultTimeout(6000);
        const record={label,width,height,console:[],pageerrors:[]};evidence.pages.push(record);
        page.on("console",message=>record.console.push({phase,type:message.type(),text:message.text(),url:message.location().url}));
        page.on("pageerror",error=>record.pageerrors.push({phase,text:String(error)}));
        return page;
      };
      const dashboard=await newPage("dashboard"),terminal=await newPage("terminal");
      const inputs=async()=>(await get("/__fixture/control")).attachments.flatMap(a=>a.inputs);
      const check=async(name,run)=>{phase=name;await run();evidence.cases.push({name,width,height,pass:true});};
      const shot=async(page,name)=>{const file=`${width}x${height}-${name}.png`;await page.screenshot({path:path.join(directory,file),fullPage:true});if(name!=="failure"){const dialog=page.locator("dialog.persea-clipboard[open]");const box=await dialog.count()?await dialog.boundingBox():null;if(box)assert(box.x>=-1&&box.y>=-1&&box.x+box.width<=width+1&&box.y+box.height<=height+1,"Clipboard stays within viewport");}return file;};
      const clipboard=page=>page.getByRole("dialog").filter({has:page.locator(".persea-clipboard__body")});
      const button=(page,name)=>clipboard(page).getByRole("button",{name,exact:true});
      const openTerminal=async()=>{activePage=terminal;if(await clipboard(terminal).isVisible())return;await terminal.locator(".persea-unified-toolbar-paste").click();await clipboard(terminal).waitFor({state:"visible"});};
      const chooseText=async(page,text)=>{await clipboard(page).locator(".persea-clipboard__item").filter({hasText:text}).first().locator(".persea-clipboard__open").click();await page.getByLabel("Clipboard text",{exact:true}).waitFor();};
      try{
        await check("dashboard creates shared text in its own browser context",async()=>{
          activePage=dashboard;await dashboard.goto(fixture.origin+"/");await dashboard.getByRole("button",{name:"Open shared clipboard",exact:true}).click();
          await button(dashboard,"Add text").click();await dashboard.getByLabel("Text to add to shared clipboard",{exact:true}).fill("Shared across devices");
          await button(dashboard,"Save").click();await clipboard(dashboard).locator(".persea-clipboard__item").filter({hasText:"Shared across devices"}).waitFor();
          assert(await clipboard(dashboard).getByText("30 min left",{exact:false}).count()>0,"Recent text exposes its 30-minute expiry");
          await shot(dashboard,"dashboard-clipboard");
        });
        const inventory=await get("/api/inventory"),session=inventory.realms[0].servers[0].sessions[0];
        activePage=terminal;await terminal.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({handle:session.handles.control,mode:"control",history:"1000",name:session.name,draft_scope:fixture.draftScope,engine:"unified-dev",image_realm:"local"})}`);
        await terminal.waitForSelector(".persea-unified-xterm .xterm-helper-textarea");
        await until(async()=>(await get("/__fixture/control")).attachments.some(a=>a.mode==="CONTROL"),"terminal control grant");
        await check("Clipboard opens editor; Cancel discards edits without input",async()=>{
          await openTerminal();await chooseText(terminal,"Shared across devices");await terminal.getByLabel("Clipboard text",{exact:true}).fill("Cancelled draft");
          await shot(terminal,"text-preview");await button(terminal,"Cancel").click();assert((await inputs()).length===0,"Cancel must not send bytes");
          await openTerminal();await chooseText(terminal,"Shared across devices");assert(await terminal.getByLabel("Clipboard text",{exact:true}).inputValue()==="Shared across devices","Preview edits stay local");
          await button(terminal,"Cancel").click();assert((await inputs()).length===0,"Back must not send bytes");
        });
        if(process.env.PERSEA_CLIPBOARD_CASE==="smoke")break;
        await check("Save edits shared text with zero input; row Paste sends once without Enter",async()=>{
          await chooseText(terminal,"Shared across devices");await terminal.getByLabel("Clipboard text",{exact:true}).fill("edited terminal text");
          await button(terminal,"Save").click();assert((await inputs()).length===0,"Save does not send bytes");await clipboard(terminal).locator(".persea-clipboard__item").filter({hasText:"edited terminal text"}).getByRole("button",{name:"Paste text to terminal",exact:true}).click();await until(async()=>(await inputs()).length===1,"single terminal input");
          assert(JSON.stringify(await inputs())==='["edited terminal text"]',"Paste must preserve exact edited bytes without a newline");await openTerminal();await chooseText(terminal,"edited terminal text");await terminal.getByLabel("Clipboard text",{exact:true}).fill("Shared across devices");await button(terminal,"Save").click();await button(terminal,"Close clipboard").click();
        });
        await check("trusted native import returns to shared list without terminal input",async()=>{
          await openTerminal();await terminal.evaluate(()=>{window.__clipboardMock.text="Native import text";window.__clipboardMock.image=false;});
          await button(terminal,"Paste from device").click();await clipboard(terminal).locator(".persea-clipboard__item").filter({hasText:"Native import text"}).waitFor();
          const read=await terminal.evaluate(()=>window.__clipboardMock.reads.at(-1));assert(read.trusted&&read.active,"OS read must run in trusted active click");
          assert((await inputs()).length===1,"Import does not send input");await button(terminal,"Close clipboard").click();
        });
        await check("mixed native clipboard import shares both image and text",async()=>{
          activePage=dashboard;await dashboard.evaluate(()=>{window.__clipboardMock.text="Mixed clipboard text";window.__clipboardMock.image=true;});
          await button(dashboard,"Paste from device").click();await clipboard(dashboard).locator(".persea-clipboard__item").filter({hasText:"Mixed clipboard text"}).waitFor();
          await until(async()=>(await get("/__clipboard/control")).items.length===1,"mixed image import shared");
          const read=await dashboard.evaluate(()=>window.__clipboardMock.reads.at(-1));assert(read.trusted&&read.active,"Mixed OS import activated by a real click");
          await openTerminal();await until(async()=>await clipboard(terminal).locator('[data-clipboard-item^="image:"]').count()===1,"second browser sees shared image");
          await clipboard(terminal).locator('[data-clipboard-item^="image:"]').locator('.persea-clipboard__open').click();await terminal.locator(".persea-clipboard__image").waitFor();
          await until(()=>terminal.locator(".persea-clipboard__image").evaluate(image=>image.complete&&image.naturalWidth>0),"shared image decodes");
          await shot(terminal,"image-preview");assert((await inputs()).length===1,"Image selection does not send input");
        });
        await check("image Copy starts in trusted gesture and resolves promised PNG",async()=>{
          await button(terminal,"Done").click();await imageControl({delayReadMs:150});
          const imageCopy=clipboard(terminal).locator('[data-clipboard-item^="image:"]').getByRole("button",{name:"Copy to device",exact:true});
          await imageCopy.scrollIntoViewIfNeeded();
          await terminal.evaluate(()=>{
            const body=document.querySelector('.persea-clipboard__body'),card=document.querySelector('.persea-clipboard__feedback');
            const geometry=()=>{const r=body.getBoundingClientRect();return [r.top,r.height,body.scrollTop,body.clientHeight,body.scrollHeight];};
            const probe=window.__feedbackProbe={active:true,before:geometry(),samples:[]};
            const sample=()=>{if(!probe.active)return;probe.samples.push({geometry:geometry(),opacity:Number(getComputedStyle(card).opacity),visible:card.dataset.visible==='true'});requestAnimationFrame(sample);};sample();
          });
          await imageCopy.click();await terminal.mouse.move(width-2,height-2);
          const write=await terminal.evaluate(()=>window.__clipboardMock.writes.at(-1));assert(write.kind==="write"&&write.trusted&&write.active&&!write.resolved,"OS write starts before delayed image bytes resolve");
          await until(()=>terminal.evaluate(()=>window.__clipboardMock.writes.at(-1)?.resolved),"promised image copy resolves");
          assert(await terminal.evaluate(()=>window.__clipboardMock.items.at(-1)?.type==="image/png"&&window.__clipboardMock.items.at(-1).size>0),"OS clipboard receives PNG");
          const status=terminal.locator(".persea-clipboard__status");
          await until(async()=>(await status.textContent())==="Image copied to this device.","Copy success message");
          assert(await status.evaluate(node=>{const anchor=node.closest('.persea-clipboard__feedback-anchor');return anchor?.previousElementSibling?.classList.contains('persea-clipboard__header')&&anchor.nextElementSibling?.classList.contains('persea-clipboard__body')&&anchor.getBoundingClientRect().height===0;}),"Feedback floats below the header without occupying list space");
          assert(await status.getAttribute("role")==="status","Feedback is announced without moving focus");
          const feedback=terminal.locator('.persea-clipboard__feedback');
          assert(await feedback.getAttribute('data-kind')==='success','Copy feedback has success styling');
          await pause(220);await shot(terminal,"copy-feedback");
          await pause(5300);assert((await status.textContent())==="","Successful copy feedback expires");
          const motion=await terminal.evaluate(()=>{
            const p=window.__feedbackProbe;p.active=false;
            return {stable:p.samples.every(s=>s.geometry.every((v,i)=>Math.abs(v-p.before[i])<.5)),entered:p.samples.some(s=>s.visible&&s.opacity>0&&s.opacity<1),exited:p.samples.some(s=>!s.visible&&s.opacity>0&&s.opacity<1),samples:p.samples.length,scrollTopBefore:p.before[2]};
          });
          evidence.feedbackMotion??=[];evidence.feedbackMotion.push({width,height,...motion});
          assert(motion.stable,'Feedback appearance and dismissal preserve scroll bounds, content height and position on every frame');
          assert(motion.entered&&motion.exited,'Feedback fades in and out rather than appearing or disappearing abruptly');
          if(width===390){
            const copy=clipboard(terminal).locator('[data-clipboard-item^="text:"]').first().getByRole("button",{name:"Copy to device",exact:true});
            await copy.click();await until(async()=>(await status.textContent())==="Copied to this device.","Text copy success");
            await terminal.evaluate(()=>{window.__savedWriteText=navigator.clipboard.writeText;navigator.clipboard.writeText=async()=>{throw new DOMException("Denied","NotAllowedError");};});
            await copy.click();await until(async()=>(await status.textContent()).startsWith("Copy was refused."),"Copy refusal is visible");
            assert(await feedback.getAttribute('data-kind')==='error','Copy refusal has error styling');
            await pause(220);await shot(terminal,'copy-error-feedback');
            await pause(5200);assert((await status.textContent()).startsWith("Copy was refused."),"An older success timer cannot clear a later error");
            await terminal.evaluate(()=>{navigator.clipboard.writeText=window.__savedWriteText;});
            await button(terminal,'Dismiss clipboard message').focus();await terminal.keyboard.press('Enter');
            await until(async()=>(await status.textContent())==='','Error can be dismissed with the keyboard');
            assert(await button(terminal,'Close clipboard').evaluate(node=>node===document.activeElement),'Keyboard dismissal restores focus inside the dialog');
            await terminal.emulateMedia({reducedMotion:'reduce',colorScheme:'light'});
            await copy.click();await until(async()=>(await status.textContent())==='Copied to this device.','Reduced-motion copy feedback');
            assert(await feedback.evaluate(node=>getComputedStyle(node).transitionDuration==='0s'&&getComputedStyle(node).transform==='none'),'Reduced-motion preference removes animation');
            await shot(terminal,'copy-feedback-light-reduced-motion');
            await button(terminal,'Dismiss clipboard message').focus();await pause(5200);
            assert((await status.textContent())==='Copied to this device.','Focused feedback does not time out while being read');
            await terminal.keyboard.press('Enter');await until(async()=>(await status.textContent())==='','Success can be dismissed immediately');
            await terminal.emulateMedia({reducedMotion:'no-preference',colorScheme:'dark'});
            await copy.click();await button(terminal,'Dismiss clipboard message').click();await copy.click();await terminal.mouse.move(width-2,height-2);await pause(220);
            assert((await status.textContent())==='Copied to this device.','An old fade-out cleanup cannot erase the next message');
            await button(terminal,"Close clipboard").click();await openTerminal();
            assert((await status.textContent())==="","Reopening Clipboard clears previous feedback");
          }
          if(width===1280){
            await imageCopy.click();await until(async()=>(await status.textContent())==='Image copied to this device.','Desktop copy message');
            await feedback.hover();await pause(5200);
            assert((await status.textContent())==='Image copied to this device.','Hovered feedback remains readable');
            await button(terminal,'Dismiss clipboard message').click();await terminal.mouse.move(width-2,height-2);await until(async()=>(await status.textContent())==='','Hovered message can be dismissed');
          }
          await imageControl({delayReadMs:0});
        });
        await check("row Insert image stages without sending",async()=>{
          await clipboard(terminal).locator('[data-clipboard-item^="image:"]').getByRole("button",{name:"Insert image",exact:true}).click();await until(async()=>(await get("/__clipboard/control")).stages.length===1,"explicit image staging");
          assert((await inputs()).length===1,"Staging never sends a terminal path automatically");
          await shot(terminal,"composer-image");
          evidence.composerControls=await terminal.locator("button").evaluateAll(nodes=>nodes.filter(n=>n.getClientRects().length).map(n=>({text:n.textContent,label:n.getAttribute("aria-label")})));
          await terminal.getByRole("button",{name:"Remove image 1 of 1",exact:true}).click();
          assert(await terminal.getByRole("button",{name:/Remove image/}).count()===0,"Remove clears composer image");
          assert((await inputs()).length===1,"Removing a staged image sends no input");
        });
        await check("terminal menu opens the same Clipboard",async()=>{
          await terminal.getByRole("button",{name:"Hide composer — draft stays here",exact:true}).click();
          await terminal.getByRole("button",{name:"Quick actions",exact:true}).click();
          await terminal.getByRole("group",{name:"Terminal menu",exact:true}).getByRole("button",{name:"Open shared clipboard",exact:true}).click();
          await chooseText(terminal,"Shared across devices");await button(terminal,"Cancel").click();assert((await inputs()).length===1,"Menu preview cancellation sends nothing");await button(terminal,"Close clipboard").click();
        });
        if(width===390){
          const nativeCopy=async(field,body,wanted,stale)=>{
            const before=(await get("/api/snippets")).items;await field.fill(body);
            await field.evaluate((node,{wanted,stale})=>{
              window.__nativeCopyEvents=[];document.addEventListener("copy",event=>{const target=event.target;window.__nativeCopyEvents.push({trusted:event.isTrusted,target:target?.tagName,selected:target instanceof HTMLTextAreaElement?target.value.slice(target.selectionStart,target.selectionEnd):"",domSelection:document.getSelection()?.toString(),frozenAnchor:document.querySelector(".persea-unified-select__body")?.contains(document.getSelection()?.anchorNode)});},{capture:true,once:true});
              node.focus();const start=node.value.indexOf(wanted);node.setSelectionRange(start,start+wanted.length);
              if(stale){const frozen=document.querySelector(".persea-unified-select__body");const range=document.createRange();range.selectNodeContents(frozen);document.getSelection().removeAllRanges();document.getSelection().addRange(range);node.focus();node.setSelectionRange(start,start+wanted.length);}
            },{wanted,stale});
            await terminal.keyboard.press(process.platform==="darwin"?"Meta+c":"Control+c");
            const events=await terminal.evaluate(()=>window.__nativeCopyEvents);evidence.lastNativeCopyEvents=events;assert(events.length===1&&events[0].trusted&&events[0].target==="TEXTAREA"&&events[0].selected===wanted,`Native keyboard Copy emits one trusted textarea copy event: ${JSON.stringify(events)}`);
            await until(async()=>(await get("/api/snippets")).items.some(item=>!before.some(old=>old.id===item.id)),"Native Copy creates shared text");await pause(150);
            const after=(await get("/api/snippets")).items,added=after.filter(item=>!before.some(old=>old.id===item.id));
            evidence.nativeCopy??=[];evidence.nativeCopy.push({engine:ENGINE,width,height,stale,events,added});
            assert(added.length===1&&added[0].body===wanted,`Native Copy shares exactly selected text once: ${JSON.stringify(added)}`);assert((await inputs()).length===1,"Native Copy does not send terminal input");
          };
          await check("trusted browser Copy from composer shares only selected substring",async()=>{
            await terminal.getByRole("button",{name:/^Open composer/}).click();await nativeCopy(terminal.locator(".attachment-page__composer-textarea"),"before COMPOSER_NATIVE_SELECTION after","COMPOSER_NATIVE_SELECTION",false);
            await terminal.locator(".attachment-page__composer-textarea").fill("");await terminal.getByRole("button",{name:"Hide composer — draft stays here",exact:true}).click();
          });
          await check("trusted browser Copy from edited preview shares only its substring after frozen selection",async()=>{
            await terminal.getByRole("button",{name:"Select terminal text",exact:true}).click();await terminal.locator(".persea-unified-select__body").waitFor({state:"visible"});
            await terminal.getByRole("button",{name:"Quick actions",exact:true}).click();await terminal.getByRole("group",{name:"Terminal menu",exact:true}).getByRole("button",{name:"Open shared clipboard",exact:true}).click();await chooseText(terminal,"Shared across devices");
            await nativeCopy(terminal.getByLabel("Clipboard text",{exact:true}),"before PREVIEW_NATIVE_SELECTION after","PREVIEW_NATIVE_SELECTION",true);await button(terminal,"Cancel").click();await button(terminal,"Close clipboard").click();
            const leave=terminal.getByRole("button",{name:"Leave selection without copying",exact:true});if(await leave.count())await leave.click();
          });
        }
        await check("cancelled delayed image staging cannot attach or send",async()=>{
          await openTerminal();await imageControl({delayReadMs:350});
          await clipboard(terminal).locator('[data-clipboard-item^="image:"]').getByRole("button",{name:"Insert image",exact:true}).click();await button(terminal,"Close clipboard").click();await pause(500);
          assert((await get("/__clipboard/control")).stages.length===1,"Cancelled download cannot start staging");
          assert(await terminal.getByRole("button",{name:/Remove image/}).count()===0,"Cancelled attachment stays absent");assert((await inputs()).length===1,"Cancelled image sends no input");await imageControl({delayReadMs:0});
        });
        await check("cached text remains locally copyable during server outage",async()=>{
          await openTerminal();await control({snippetUnavailable:true});await imageControl({unavailable:true});
          await clipboard(terminal).locator(".persea-clipboard__item").filter({hasText:"Shared across devices"}).getByRole("button",{name:"Copy to device",exact:true}).click();assert(await terminal.evaluate(()=>window.__clipboardMock.writes.at(-1)?.text)==="Shared across devices","Cached copy works without server");
          await button(terminal,"Close clipboard").click();await control({snippetUnavailable:false});await imageControl({unavailable:false});
        });
        if(width===390)await check("session switch during pending image download never attaches to the new target",async()=>{
          await openTerminal();await imageControl({delayReadMs:1500});const stages=(await get("/__clipboard/control")).stages.length;
          await clipboard(terminal).locator('[data-clipboard-item^="image:"]').getByRole("button",{name:"Insert image",exact:true}).click();await terminal.keyboard.press("Escape");
          await terminal.getByRole("button",{name:"Quick actions",exact:true}).click();await terminal.getByRole("button",{name:"Choose another session",exact:true}).click();await terminal.getByRole("button",{name:"Switch to beta",exact:true}).click();
          await until(async()=>(await get("/__fixture/control")).attachments.some(a=>a.session==="B"&&a.mode==="CONTROL"),"new session control");await pause(1700);
          assert((await get("/__clipboard/control")).stages.length===stages,"Old image completion cannot stage for new target");assert(await terminal.getByRole("button",{name:/Remove image/}).count()===0,"New target composer has no old image");assert((await inputs()).length===1,"Switch sends no stale terminal input");await imageControl({delayReadMs:0});
          await terminal.getByRole("button",{name:"Quick actions",exact:true}).click();await terminal.getByRole("button",{name:"Choose another session",exact:true}).click();await terminal.getByRole("button",{name:"Switch to alpha",exact:true}).click();
          await until(async()=>(await get("/__fixture/control")).attachments.at(-1)?.session==="A"&&(await get("/__fixture/control")).attachments.at(-1)?.mode==="CONTROL","return to alpha");
        });
        await check("retention preferences persist across devices and duplicates keep identity",async()=>{
          await button(dashboard,"Clipboard settings").click();await dashboard.getByLabel("Default clipboard retention",{exact:true}).selectOption("86400");await button(dashboard,"Save").click();
          await until(async()=>(await get("/api/clipboard/preferences")).default_retention_seconds===86400,"Global default saved");
          const settingsDevice=await newPage("preferences-device");await settingsDevice.goto(fixture.origin+"/");await settingsDevice.getByRole("button",{name:"Open shared clipboard",exact:true}).click();await button(settingsDevice,"Clipboard settings").click();
          await until(async()=>await settingsDevice.getByLabel("Default clipboard retention",{exact:true}).inputValue()==="86400","Second device reads global preference");await settingsDevice.close();
          const add=async text=>{await button(dashboard,"Add text").click();await dashboard.getByLabel("Text to add to shared clipboard",{exact:true}).fill(text);await button(dashboard,"Save").click();await clipboard(dashboard).locator(".persea-clipboard__item").filter({hasText:text}).waitFor();};
          await add("Retention duplicate");let initial=(await get("/api/snippets")).items.find(i=>i.body==="Retention duplicate");assert(initial.retention_seconds===86400,"New item uses persisted default");
          await add("Retention duplicate");let repeated=(await get("/api/snippets")).items.filter(i=>i.body==="Retention duplicate");assert(repeated.length===1&&repeated[0].id===initial.id&&repeated[0].revision>initial.revision,"Identical text refreshes same ID once");
          await button(dashboard,"Clipboard settings").click();await dashboard.getByLabel("Default clipboard retention",{exact:true}).selectOption("1800");await button(dashboard,"Save").click();await add("Retention duplicate");
          repeated=(await get("/api/snippets")).items.filter(i=>i.body==="Retention duplicate");assert(repeated.length===1&&repeated[0].retention_seconds===86400&&Date.parse(repeated[0].expires_at)>=Date.parse(initial.expires_at),"Shorter default never reduces duplicate retention");
          await add("Merge source");await chooseText(dashboard,"Merge source");await dashboard.getByLabel("Clipboard text",{exact:true}).fill("Retention duplicate");await button(dashboard,"Save").click();
          await until(async()=>!(await get("/api/snippets")).items.some(i=>i.body==="Merge source"),"Edited duplicate merged");const merged=(await get("/api/snippets")).items.filter(i=>i.body==="Retention duplicate");assert(merged.length===1&&merged[0].id===initial.id&&merged[0].retention_seconds===86400,"Editing into existing content merges without shortening");
          const row=clipboard(dashboard).locator(".persea-clipboard__item").filter({hasText:"Retention duplicate"});await row.getByRole("button",{name:"Delete item",exact:true}).click();await row.waitFor({state:"detached"});assert(!(await get("/api/snippets")).items.some(i=>i.id===initial.id),"Row delete removes exact item");
          const originalImage=(await get("/__clipboard/control")).items[0];await dashboard.evaluate(()=>{window.__clipboardMock.text="";window.__clipboardMock.image=true;});await button(dashboard,"Paste from device").click();await until(async()=>(await get("/__clipboard/control")).items[0]?.revision>originalImage.revision,"Duplicate image renewed");
          const repeatedImages=(await get("/__clipboard/control")).items;assert(repeatedImages.length===1&&repeatedImages[0].id===originalImage.id,"Image dedupe keeps stable ID");
          assert((await inputs()).length===1,"Preferences, duplicates and deletion send zero extra bytes");
        });
        if(width===390)await check("image expiry controls and urgent countdowns remain visible",async()=>{
          activePage=dashboard;const chooserPromise=dashboard.waitForEvent("filechooser");await button(dashboard,"Add image").click();const chooser=await chooserPromise;await chooser.setFiles({name:"expiry.png",mimeType:"image/png",buffer:Buffer.concat([PNG,Buffer.from("expiry")])});
          await until(async()=>(await get("/__clipboard/control")).items.length===2,"File chooser stores image");const extra=(await get("/__clipboard/control")).items.find(i=>i.byte_size>PNG.length);const imageRow=clipboard(dashboard).locator(`[data-clipboard-item="image:${extra.id}"]`);
          await imageRow.getByRole("combobox").press("End");await until(async()=>(await get("/__clipboard/control")).items.some(i=>i.id===extra.id&&i.expires_at===null),"Image no-expiry persisted");
          await until(async()=>await imageRow.getByRole("combobox").isEnabled()&&await imageRow.locator(".persea-clipboard__expiry span").innerText()==="No expiry","Image no-expiry response rendered");
          await imageRow.getByRole("combobox").press("Home");await until(async()=>(await get("/__clipboard/control")).items.some(i=>i.id===extra.id&&i.retention_seconds===1800&&i.expires_at!==null),"Explicit choice replaces permanent image expiry");await imageRow.getByRole("button",{name:"Delete item",exact:true}).click();await imageRow.waitFor({state:"detached"});
          await control({snippetClockSkewMs:-17*60*1000,snippetWrite:[{kind:"clip",body:"Orange countdown"}]});await control({snippetClockSkewMs:-27*60*1000,snippetWrite:[{kind:"clip",body:"Red countdown"}]});await control({snippetClockSkewMs:0});
          await button(dashboard,"Close clipboard").click();await dashboard.getByRole("button",{name:"Open shared clipboard",exact:true}).click();
          for(const [body,urgency]of [["Orange countdown","near"],["Red countdown","soon"]]){
            const row=clipboard(dashboard).locator(".persea-clipboard__item").filter({hasText:body});await row.waitFor();assert(await row.locator(".persea-clipboard__expiry").getAttribute("data-urgency")===urgency,"Countdown urgency: "+body);
          }
          await dashboard.getByLabel("Find a clipboard item",{exact:true}).fill("countdown");await shot(dashboard,"expiry-countdowns");for(const body of ["Orange countdown","Red countdown"])await clipboard(dashboard).locator(".persea-clipboard__item").filter({hasText:body}).getByRole("button",{name:"Delete item",exact:true}).click();await dashboard.getByLabel("Find a clipboard item",{exact:true}).fill("");assert((await inputs()).length===1,"Image retention and delete send no terminal input");
        });
        await check("clipboard mutation requests carry same-origin CSRF",async()=>{
          const stats=await get("/__clipboard/control"),writes=stats.requests.filter(r=>r.method!=="GET");
          assert(writes.length>=4&&writes.every(r=>r.csrf&&r.csrf===r.cookie&&r.origin===fixture.origin&&r.site==="same-origin"),"Shared text, images and staging all carry auth headers");
          const state=await get("/__fixture/control");assert(state.attachments.every(a=>a.resizes===0&&a.resizeRequests.length===0),"Clipboard actions must never resize terminal");
        });
        await check("image insertion receipt hides editing options and restores them for a new draft",async()=>{
          await openTerminal();
          await clipboard(terminal).locator('[data-clipboard-item^="image:"]').first().getByRole("button",{name:"Insert image",exact:true}).click();
          const send=terminal.getByRole("button",{name:"Insert into terminal without running it",exact:true});
          await until(()=>send.isEnabled(),"Staged image ready to insert");
          const before=await inputs();await send.click();
          const receipt=terminal.locator('.attachment-page__composer[data-content="sent"]');await receipt.waitFor({state:"visible"});
          assert(/Inserted.*1 image/.test(await receipt.innerText()),"Image insertion receipt is visible");
          assert(!await receipt.locator('.attachment-page__composer-typography').isVisible(),"Receipt has no font control");
          assert(!await terminal.locator('.attachment-page__composer-typography-popover').isVisible(),"Receipt has no font popover");
          await until(async()=>(await inputs()).length===before.length+1,"Image path sent exactly once");
          assert(!(await inputs()).at(-1).includes("\n"),"Image insertion does not run the path");await shot(terminal,"image-receipt");
          await receipt.locator('.attachment-page__composer-close').click();
          await terminal.getByRole("button",{name:/^Open composer/}).click();
          await terminal.locator('.attachment-page__composer-textarea').fill("next draft");
          assert(await terminal.locator('.attachment-page__composer-typography').isVisible(),"Draft restores font control");
          await terminal.locator('.attachment-page__composer-textarea').fill("");
          await terminal.locator('.attachment-page__composer-close').click();
        });
        await check("disconnect invalidates an already open row Paste action",async()=>{
          const beforeDisconnect=(await inputs()).length;
          await openTerminal();
          const fresh=(await get("/api/inventory")).realms[0].servers[0].sessions[0];const imagePage=await newPage("stale-image");await imagePage.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({handle:fresh.handles.control,mode:"control",history:"1000",name:fresh.name,draft_scope:fixture.draftScope,engine:"unified-dev",image_realm:"local"})}`);await imagePage.waitForSelector(".persea-unified-xterm .xterm-helper-textarea");
          await until(async()=>(await get("/__fixture/control")).attachments.at(-1)?.mode==="CONTROL","image control granted");
          await imagePage.locator(".persea-unified-toolbar-paste").click();
          await imageControl({delayReadMs:400});
          const staged=(await get("/__clipboard/control")).stages.length;await clipboard(imagePage).locator('[data-clipboard-item^="image:"]').getByRole("button",{name:"Insert image",exact:true}).click();
          await control({closeSession:{session:"A",reason:"clipboard stale target test"}});await pause(500);
          const paste=clipboard(terminal).locator(".persea-clipboard__item").filter({hasText:"Shared across devices"}).getByRole("button",{name:"Paste text to terminal",exact:true});if(!await paste.isDisabled())await paste.click();await pause(100);assert((await inputs()).length===beforeDisconnect,"Disconnected preview must not send bytes");
          assert((await get("/__clipboard/control")).stages.length===staged,"Disconnected pending image cannot stage");assert(await imagePage.getByRole("button",{name:/Remove image/}).count()===0,"Disconnected image completion cannot attach");await imageControl({delayReadMs:0});await imagePage.close();
          await button(terminal,"Close clipboard").click();
        });
        if(width===390)await check("polling stops while document hidden and a held pointer preserves its row",async()=>{
          await button(dashboard,"Close clipboard").click();await openTerminal();
          const row=clipboard(terminal).locator(".persea-clipboard__item").filter({hasText:"Shared across devices"});
          await row.scrollIntoViewIfNeeded();await row.evaluate(node=>{window.__heldClipboardRow=node;});const box=await row.locator(".persea-clipboard__open").boundingBox();await terminal.mouse.move(box.x+box.width/2,box.y+box.height/2);await terminal.mouse.down();
          await control({snippetWrite:[{kind:"clip",body:"New while holding"}]});await pause(4400);
          assert(await terminal.evaluate(()=>window.__heldClipboardRow.isConnected),"Polling must preserve held row DOM identity");await terminal.mouse.up();
          await terminal.getByLabel("Clipboard text",{exact:true}).waitFor();assert(await terminal.getByLabel("Clipboard text",{exact:true}).inputValue()==="Shared across devices","Held tap must activate original text");await button(terminal,"Cancel").click();
          await terminal.evaluate(()=>{Object.defineProperty(document,"visibilityState",{configurable:true,get:()=>"hidden"});document.dispatchEvent(new Event("visibilitychange"));});await pause(200);
          const count=(await get("/__clipboard/control")).requests.filter(r=>r.method==="GET").length;await pause(4400);
          const after=(await get("/__clipboard/control")).requests.filter(r=>r.method==="GET");evidence.hiddenPollDelta=after.slice(count);
          assert(after.length===count,`Hidden clipboard must stop polling: ${JSON.stringify(after.slice(count))}`);
          await terminal.evaluate(()=>{delete document.visibilityState;document.dispatchEvent(new Event("visibilitychange"));});await button(terminal,"Close clipboard").click();
        });
        if(width===390)await check("reads preserve expiry; expired recent items disappear while kept text remains",async()=>{
          await openTerminal();await clipboard(terminal).locator(".persea-clipboard__item").filter({hasText:"Shared across devices"}).getByRole("combobox").press("End");await until(async()=>(await get("/api/snippets")).items.some(i=>i.body==="Shared across devices"&&i.expires_at===null),"No expiry persisted");await button(terminal,"Close clipboard").click();
          const first=await get("/api/snippets"),again=await get("/api/snippets");assert(JSON.stringify(first)===JSON.stringify(again),"Repeated list reads never renew expiry");
          const imagesFirst=await get("/api/clipboard/images"),imagesAgain=await get("/api/clipboard/images");assert(JSON.stringify(imagesFirst)===JSON.stringify(imagesAgain),"Image reads never renew expiry");
          await control({snippetClockSkewMs:31*60*1000});await imageControl({imageClockSkewMs:31*60*1000});await openTerminal();
          await until(async()=>await clipboard(terminal).locator('[data-clipboard-item^="image:"]').count()===0,"Expired image removed");
          await until(async()=>await clipboard(terminal).locator('[data-clipboard-item^="text:"]').count()===1,"Expired recent text removed");
          await clipboard(terminal).locator(".persea-clipboard__item").filter({hasText:"Shared across devices"}).waitFor();await button(terminal,"Close clipboard").click();
        });
        if(width===390)await check("replay without a control grant cannot paste retained text",async()=>{
          await control({holdModeGrant:true});const fresh=(await get("/api/inventory")).realms[0].servers[0].sessions[0];const replay=await newPage("replay");activePage=replay;
          await replay.goto(`${fixture.origin}/terminal?engine=unified-dev#${new URLSearchParams({handle:fresh.handles.control,mode:"control",history:"1000",name:fresh.name,draft_scope:fixture.draftScope,engine:"unified-dev",image_realm:"local"})}`);await replay.waitForSelector(".persea-unified-xterm .xterm-helper-textarea");
          await replay.locator(".persea-unified-toolbar-paste").click();assert(await clipboard(replay).locator(".persea-clipboard__item").filter({hasText:"Shared across devices"}).getByRole("button",{name:"Paste text to terminal",exact:true}).isDisabled(),"Replay refuses text before CONTROL grant");assert((await inputs()).length===2,"Replay sends nothing beyond the explicit text and image insertions");await button(replay,"Close clipboard").click();await replay.close();await control({holdModeGrant:false});
        });
        const finalState=await get("/__fixture/control");assert(finalState.attachments.every(a=>a.resizes===0&&a.resizeRequests.length===0),"Final lifecycle actions never resize terminal");
      }catch(error){if(activePage&&!activePage.isClosed()){await shot(activePage,"failure");evidence.visible=await activePage.locator("body").innerText();evidence.selects=await activePage.locator(".persea-clipboard__item").evaluateAll(rows=>rows.map(row=>({id:row.dataset.clipboardItem,options:[...row.querySelectorAll("option")].map(o=>({value:o.value,disabled:o.disabled,selected:o.selected}))})));}throw error;}finally{for(const context of contexts)await context.close();}
    }
    const diagnostics=evidence.pages.flatMap(page=>[...page.console,...page.pageerrors]);
    evidence.expectedDiagnostics=diagnostics.filter(message=>(message.phase==="disconnect invalidates an already open row Paste action"&&/WebSocket.*410/.test(message.text))||(message.phase==="cached text remains locally copyable during server outage"&&/status of 503/.test(message.text)&&/\/api\/(snippets|clipboard\/images(?:\/[0-9a-f]{32})?)$/.test(message.url||"")));
    const unexpected=diagnostics.filter(message=>!evidence.expectedDiagnostics.includes(message));assert(!unexpected.length,`Browser diagnostics: ${JSON.stringify(unexpected)}`);
    evidence.pass=true;
  }catch(error){evidence.pass=false;evidence.failure={phase,error:String(error),stack:error.stack};if(activePage&&!activePage.isClosed()){await activePage.screenshot({path:path.join(directory,"failure.png"),fullPage:true}).catch(()=>{});evidence.failure.visible=await activePage.locator("body").innerText().catch(()=>"");}throw error;}
  finally{fs.writeFileSync(path.join(directory,"results.json"),JSON.stringify(evidence,null,2));console.log(JSON.stringify({engine:ENGINE,directory,pass:evidence.pass,cases:evidence.cases.length,failure:evidence.failure?.error}));await browser.close();await api.dispose();await fixture.close();fs.rmSync(tmp,{recursive:true,force:true});}
}
main().catch(error=>{console.error(error.stack||String(error));process.exitCode=1;});
