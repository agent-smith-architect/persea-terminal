"use strict";

// Browser diagnostics the clipboard suite provokes on purpose. Everything else
// a page logs fails the suite.
//
// The fixture marks each deliberate clipboard image outage response. WebKit can
// deliver the matching console message after the test phase has advanced, so
// an image 503 is never excused by its phase: each message must consume one
// marked response for the same URL. Snippet outages are not marked and stay
// limited to the outage phase.
const DISCONNECT_PHASE="disconnect invalidates an already open row Paste action";
const OUTAGE_PHASE="cached text remains locally copyable during server outage";
const STATUS_503=/status of 503/;
const SNIPPETS=/\/api\/snippets$/;

function expectedClipboardDiagnostics(pages){
  const expected=[];
  for(const page of pages){
    const responses=[...page.outageResponses];
    for(const message of [...page.console,...page.pageerrors]){
      if(message.phase===DISCONNECT_PHASE&&/WebSocket.*410/.test(message.text)){expected.push(message);continue;}
      if(!STATUS_503.test(message.text))continue;
      if(message.phase===OUTAGE_PHASE&&SNIPPETS.test(message.url||"")){expected.push(message);continue;}
      const index=responses.findIndex(response=>response.url===message.url);
      if(index<0)continue;
      responses.splice(index,1);
      expected.push(message);
    }
  }
  return expected;
}

module.exports={expectedClipboardDiagnostics,DISCONNECT_PHASE,OUTAGE_PHASE};
