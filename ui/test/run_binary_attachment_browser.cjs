"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const playwright = require("playwright");
const { startFixture, Socket } = require("./unified_reopen_fixture.cjs");

const shapes = [
  { width: 1280, height: 800 },
  { width: 320, height: 568 },
  { width: 390, height: 844 },
  { width: 430, height: 932 },
  { width: 844, height: 390 },
];
const malformed = ["text", "short", "trailing", "utf8", "length"];
let corruption;
const sendAttachment = Socket.prototype.sendAttachment;
Socket.prototype.sendAttachment = function(frame) {
  if (!corruption) return sendAttachment.call(this, frame);
  if (corruption === "text") return this.sendText(JSON.stringify(frame));
  const header = Buffer.from(JSON.stringify({ ...frame, data: frame.data.length }));
  const prefix = Buffer.alloc(4);
  prefix.writeUInt32BE(header.length);
  let bytes = Buffer.concat([prefix, header, frame.data]);
  if (corruption === "short") bytes = bytes.subarray(0, 3);
  if (corruption === "trailing") bytes = Buffer.concat([bytes, Buffer.from([0])]);
  if (corruption === "utf8") bytes[4 + header.indexOf('"source":"') + '"source":"'.length] = 255;
  if (corruption === "length") bytes.writeUInt32BE(0xffff_ffff);
  this.write(0x2, bytes);
};

async function run(engine) {
  const fixture = await startFixture(path.resolve(__dirname, ".."), { tls: true, initialReplay: "\xff\r\nBINARY-REPLAY\r\n" });
  const browser = await playwright[engine].launch({
    headless: true,
    ...(engine === "chromium" ? { executablePath: require("./browser_path.cjs")(), args: ["--no-sandbox"] } : {}),
  });
  const api = await playwright.request.newContext({ baseURL: fixture.origin, ignoreHTTPSErrors: true });
  const control = async data => (await api.post("/__fixture/control", { data })).json();
  try {
    for (const [index, viewport] of shapes.entries()) {
      await control({ reset: true });
      const inventory = await (await api.get("/api/inventory")).json();
      const session = inventory.realms[0].servers[0].sessions[0];
      const context = await browser.newContext({ viewport, hasTouch: viewport.width < 1000, ignoreHTTPSErrors: true });
      const page = await context.newPage();
      const messages = [];
      page.on("console", message => messages.push(`${message.type()}: ${message.text()}`));
      page.on("pageerror", error => messages.push(String(error)));
      const received = [];
      const wsEvent = page.waitForEvent("websocket");
      page.on("websocket", socket => socket.on("framereceived", event => {
        if (typeof event.payload !== "string") received.push(Buffer.from(event.payload));
      }));
      try {
        const hash = new URLSearchParams({ handle: session.handles.control, mode: "control", history: "1000", name: session.name, draft_scope: fixture.draftScope, engine: "unified-dev" });
        await page.goto(`${fixture.origin}/terminal?engine=unified-dev#${hash}`);
        const ws = await wsEvent;
        await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("fixture-live"));
        assert(await page.locator(".xterm-rows").textContent().then(text => text.includes("BINARY-REPLAY")), "raw PREPARE did not render");
        const chunks = [
          Buffer.from([255, 192, 0, 128, 13, 10, 27]),
          Buffer.from("[32mBINARY-"),
          Buffer.from([226]), Buffer.from([130]), Buffer.from([172]),
          Buffer.from("-SPLIT\x1b[0m\r\n"),
        ];
        const before = received.length;
        for (const bytes of chunks) await control({ writeLive: bytes.toString("latin1") });
        await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("BINARY-€-SPLIT"));
        assert.deepEqual(received.slice(before).map(bytes => {
          const length = bytes.readUInt32BE(0);
          const header = JSON.parse(bytes.subarray(4, 4 + length).toString("utf8"));
          assert.equal(header.type, "LIVE");
          assert.equal(header.data, bytes.length - 4 - length);
          return bytes.subarray(4 + length);
        }), chunks, "raw socket changed split terminal bytes");
        corruption = malformed[index];
        const close = ws.waitForEvent("close");
        await control({ writeLive: "must-not-render" });
        await close;
        corruption = undefined;
        await page.waitForFunction(() => document.querySelector(".persea-unified-notice")?.hidden === false);
        const state = await (await api.get("/__fixture/control")).json();
        assert.equal(state.attachments.at(-1).closeReason, "malformed_frame");
        assert.equal(state.attachments.at(-1).peerCloseCode, 4002);
        assert(!(await page.locator(".xterm-rows").textContent()).includes("must-not-render"), "malformed bytes reached terminal");
        assert.deepEqual(messages, [], "browser console must stay clean");
        console.log(`PASS binary attachment ${engine} ${viewport.width}x${viewport.height}: raw replay, split LIVE, ${malformed[index]} rejection, clean console`);
      } finally {
        corruption = undefined;
        await context.close();
      }
    }
  } finally {
    await api.dispose();
    await browser.close();
    await fixture.close();
  }
}

(async () => {
  for (const engine of process.env.PERSEA_BINARY_ENGINE ? [process.env.PERSEA_BINARY_ENGINE] : ["chromium", "webkit"]) await run(engine);
})().catch(error => { console.error(error); process.exitCode = 1; });
