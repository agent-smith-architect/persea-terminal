"use strict";

const crypto = require("crypto");
const { startFixture, CSRF_TOKEN, cookieCSRF, readJSON } = require("./unified_reopen_fixture.cjs");
const IMAGE_TTL = 30 * 60 * 1000;
// A real, decodable 1x1 PNG, used for OS clipboard and file chooser fixtures.
const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j1ioAAAAASUVORK5CYII=", "base64");

async function startClipboardFixture(ui, options = {}) {
  let fixture;
  const policies=[0,1800,14400,86400,604800,2592000];
  const state = { images: new Map(), requests: [], stages: [], delayReadMs: 0, delayStageMs: 0, unavailable: false, imageClockSkewMs: 0, stageRefused: false };
  const now = () => Date.now() + state.imageClockSkewMs;
  const list = () => [...state.images.values()].filter(item => (item.expires_at===null||Date.parse(item.expires_at)>now())).map(({ body, ...record }) => record).sort((a,b) => b.updated_at.localeCompare(a.updated_at) || a.id.localeCompare(b.id));
  const reset = () => { state.images.clear(); state.requests.length = state.stages.length = 0; state.delayReadMs = state.delayStageMs = state.imageClockSkewMs = 0; state.unavailable = state.stageRefused = false; };
  const json = (response, status, value) => { response.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" }); response.end(JSON.stringify(value)); };
  const error = (response, status) => { response.writeHead(status, { "Content-Type": "text/plain", "Cache-Control": "no-store" }); response.end(`fixture refusal ${status}`); };
  const readBody = async request => { const chunks=[];for await(const chunk of request)chunks.push(chunk);return Buffer.concat(chunks); };
  const origin = request => `${request.socket.encrypted ? "https" : "http"}://${request.headers.host}`;
  const authorized = request => request.headers["x-persea-csrf"] === CSRF_TOKEN && cookieCSRF(request) === CSRF_TOKEN && request.headers.origin === origin(request) && request.headers["sec-fetch-site"] === "same-origin";
  const clipboardHTTP = async (request, response, url) => {
    if (url.pathname === "/__clipboard/control") {
      if (request.method === "POST") {
        const input = await readJSON(request) || {};
        if(input.reset)reset();
        for(const name of ["delayReadMs","delayStageMs","imageClockSkewMs"])if(typeof input[name]==="number")state[name]=input[name];
        for(const name of ["unavailable","stageRefused"])if(typeof input[name]==="boolean")state[name]=input[name];
      }
      json(response,200,{ requests:state.requests, stages:state.stages, items:list() });return true;
    }
    if (url.pathname === "/api/clipboard/preferences" || url.pathname === "/api/snippets" || url.pathname.startsWith("/api/snippets/")) {
      state.requests.push({path:url.pathname,method:request.method,csrf:request.headers["x-persea-csrf"]||"",origin:request.headers.origin||"",site:request.headers["sec-fetch-site"]||"",cookie:cookieCSRF(request)});
      return false;
    }
    const collection = url.pathname === "/api/clipboard/images";
    const item = /^\/api\/clipboard\/images\/([0-9a-f]{32})$/.exec(url.pathname);
    const stage = url.pathname === "/api/session-images";
    if (!collection && !item && !stage) return false;
    const operation = {path:url.pathname,method:request.method,csrf:request.headers["x-persea-csrf"]||"",origin:request.headers.origin||"",site:request.headers["sec-fetch-site"]||"",cookie:cookieCSRF(request)};
    state.requests.push(operation);
    if(request.method!=="GET"&&!authorized(request)){error(response,403);return true;}
    if(!stage&&url.search){error(response,400);return true;}
    if(state.unavailable&&!stage){response.setHeader("X-Persea-Test-Outage","1");error(response,503);return true;}
    if(stage){
      if(request.method!=="POST"||url.searchParams.get("realm")!=="local"){error(response,400);return true;}
      const body=await readBody(request),id=crypto.randomBytes(16).toString("hex"),media=request.headers["content-type"];
      const record={id,path:`/var/lib/persea-terminal-staging/local/img-${id}.png`,bytes:body.length,media_type:media,expires_at:new Date(now()+IMAGE_TTL).toISOString()};
      state.stages.push({...record,body:body.toString("base64"),at:Date.now()});
      if(state.delayStageMs)await new Promise(resolve=>setTimeout(resolve,state.delayStageMs));
      if(state.stageRefused)error(response,503);else json(response,201,record);
      return true;
    }
    if(collection&&request.method==="GET"){json(response,200,{items:list()});return true;}
    if(collection&&request.method==="POST"){
      const body=await readBody(request),media=request.headers["content-type"];
      if(body.length>10*1024*1024){error(response,413);return true;}
      if(!["image/png","image/jpeg","image/gif","image/webp"].includes(media)||!body.length){error(response,415);return true;}
      const requested=request.headers["x-persea-clipboard-retention"];
      const seconds=requested===undefined?fixture.clipboardPreferences().default_retention_seconds:Number(requested);
      if(!policies.includes(seconds)){error(response,400);return true;}
      const duplicate=[...state.images.values()].find(r=>(r.expires_at===null||Date.parse(r.expires_at)>now())&&r.media_type===media&&r.body.equals(body));
      if(!duplicate&&list().length>=20){error(response,507);return true;}
      const id=duplicate?.id||crypto.randomBytes(16).toString("hex"),stamp=Math.max(now(),duplicate?Date.parse(duplicate.updated_at)+1:0);
      const retention=duplicate?(duplicate.retention_seconds===0||seconds===0?0:Math.max(duplicate.retention_seconds,seconds)):seconds;
      const record={id,media_type:media,byte_size:body.length,created_at:duplicate?.created_at||new Date(stamp).toISOString(),updated_at:new Date(stamp).toISOString(),revision:(duplicate?.revision||0)+1,retention_seconds:retention,expires_at:retention===0?null:new Date(Math.max(stamp+retention*1000,Date.parse(duplicate?.expires_at)||0)).toISOString(),origin:request.headers["x-persea-clipboard-origin"]||""};
      state.images.set(id,{...record,body});json(response,201,record);return true;
    }
    const record=item&&state.images.get(item[1]);
    if(!record||(record.expires_at!==null&&Date.parse(record.expires_at)<=now())){error(response,404);return true;}
    if(request.method==="GET"){
      if(state.delayReadMs)await new Promise(resolve=>setTimeout(resolve,state.delayReadMs));
      if(response.destroyed)return true;
      response.writeHead(200,{"Content-Type":record.media_type,"Cache-Control":"no-store","X-Content-Type-Options":"nosniff"});response.end(record.body);return true;
    }
    if(request.method==="PATCH"){
      const wire=await readJSON(request);
      if(request.headers["content-type"]!=="application/json"||!wire||Object.keys(wire).some(k=>!["revision","retention_seconds"].includes(k))||!policies.includes(wire.retention_seconds)||(!Number.isSafeInteger(wire.revision)||wire.revision<1)){error(response,400);return true;}
      if(wire.revision!==record.revision){const {body,...metadata}=record;json(response,412,metadata);return true;}
      const retention=wire.retention_seconds,stamp=Math.max(now(),Date.parse(record.updated_at)+1);
      Object.assign(record,{revision:record.revision+1,updated_at:new Date(stamp).toISOString(),retention_seconds:retention,expires_at:retention===0?null:new Date(stamp+retention*1000).toISOString()});
      const {body,...metadata}=record;json(response,200,metadata);return true;
    }
    if(request.method==="DELETE"){
      const body=await readBody(request);
      let wire;try{wire=JSON.parse(body.toString());}catch{}
      if(request.headers["content-type"]!=="application/json"||!wire||Object.keys(wire).some(k=>k!=="revision")){error(response,400);return true;}
      if(wire.revision!==undefined&&(!Number.isSafeInteger(wire.revision)||wire.revision<1)){error(response,400);return true;}
      if(wire.revision!==undefined&&wire.revision!==record.revision){error(response,412);return true;}
      state.images.delete(record.id);response.writeHead(204);response.end();return true;
    }
    error(response,405);return true;
  };
  fixture = await startFixture(ui,{...options,clipboardHTTP});
  return {...fixture};
}

function installClipboardMock() {
  const state={text:"Native clipboard text",image:false,reads:[],writes:[],items:[],hold:false,pending:[],lastClick:{trusted:false,active:false}};
  window.__clipboardMock=state;
  document.addEventListener("click",event=>{state.lastClick={trusted:event.isTrusted,active:!!navigator.userActivation?.isActive};},true);
  const snapshot=kind=>({kind,trusted:state.lastClick.trusted,active:!!navigator.userActivation?.isActive,at:performance.now()});
  // The native constructor still receives promised image representations when
  // supported. This mock records OS boundary activation without claiming iOS UI.
  if(typeof ClipboardItem==="undefined")window.ClipboardItem=class{
    constructor(data){this.data=data;this.types=Object.keys(data);}async getType(type){return this.data[type];}
  };
  const imageBlob=()=>new Blob([Uint8Array.from(atob(window.__clipboardPNG),c=>c.charCodeAt(0))],{type:"image/png"});
  Object.defineProperty(navigator,"clipboard",{configurable:true,value:{
    read(){state.reads.push(snapshot("read"));const result=()=>[{types:[...(state.text?["text/plain"]:[]),...(state.image?["image/png"]:[])],getType:async type=>type==="image/png"?imageBlob():new Blob([state.text],{type:"text/plain"})}];return state.hold?new Promise(resolve=>state.pending.push(()=>resolve(result()))):Promise.resolve(result());},
    readText(){state.reads.push(snapshot("readText"));return Promise.resolve(state.text);},
    writeText(text){state.writes.push({...snapshot("writeText"),text});return Promise.resolve();},
    write(items){const entry={...snapshot("write"),types:items.flatMap(item=>item.types),resolved:false};state.writes.push(entry);return Promise.all(items.map(async item=>{for(const type of item.types){const blob=await item.getType(type);state.items.push({type,size:blob.size});}})).then(()=>{entry.resolved=true;});}
  }});
}

module.exports={startClipboardFixture,installClipboardMock,PNG};
