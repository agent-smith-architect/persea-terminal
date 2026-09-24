"use strict";

// Reopen-flow fixture: an in-process front door + unified broker double.
//
// The reopen flow is decided by four server facts the unit suite cannot
// touch and the dashboard-navigation mock does not model: one-time attachment
// handles (consumed at WebSocket open, a replay answers HTTP 410 before the
// upgrade), the control lease (a held lease closes the newcomer with
// `lease_held` and records a takeover offer for the losing handle), the
// source binding behind /api/attachment-handles, and the broker automaton
// that refuses INPUT before the control grant with `input_refused` — which
// the real front door turns into a WebSocket close. This module reproduces
// exactly those semantics over a hand-rolled RFC 6455 server (Node has no
// built-in WebSocket server and the UI deliberately has no such dependency),
// so the gate drives the REAL bundled page against them. Everything else —
// inventory shape, CSRF cookie, CSP — mirrors the dashboard-navigation gate.

const crypto = require("crypto");
const childProcess = require("child_process");
const fs = require("fs");
const http = require("http");
const https = require("https");
const os = require("os");
const path = require("path");

const WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";
const STYLE_NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const CSRF_TOKEN = "sssssssssssssssssssssssssssssssssssssssssss";
const KEYBOARD_DEFAULTS = {
  version: 1,
  layout: {
    bar: ["key:escape:0", "key:tab:0", "modifier:ctrl", "key:arrow-left:0", "key:arrow-down:0", "key:arrow-up:0", "key:arrow-right:0"],
    favorites: ["modifier:alt", "key:c:1", "key:d:1", "key:enter:0", "key:home:0", "key:end:0", "key:page-up:0", "key:page-down:0", "key:f1:0"],
  },
  prefixes: { tmux: "key:b:1", screen: "key:a:1" },
};
const defaultKeyboardRecord = () => ({ ...structuredClone(KEYBOARD_DEFAULTS), revision: 0, stored: false, available: true });

// A browser-fixture value check, independent of the frontend parser. Real Go
// API tests own raw duplicate-JSON, durable file and trusted-ingress checks.
function validKeyboardValue(value) {
  const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
  const exact = (v, keys) => object(v) && keys.every((key) => Object.hasOwn(v, key)) && Object.keys(v).every((key) => keys.includes(key));
  const prefixName = (name) => /^[A-Za-z][A-Za-z0-9 _-]{0,31}$/.test(name) && !["constructor", "prototype", "__proto__"].includes(name);
  const keyID = (id) => {
    if (typeof id !== "string") return false;
    const match = /^key:([^:]+):[0-7]$/.exec(id);
    if (!match) return false;
    try {
      const key = decodeURIComponent(match[1]);
      return encodeURIComponent(key) === match[1] && (/^[\x20-\x7e]$/.test(key) || /^(escape|tab|enter|backspace|insert|delete|arrow-left|arrow-down|arrow-up|arrow-right|home|end|page-up|page-down|f[1-9]|f1[0-2])$/.test(key));
    } catch { return false; }
  };
  const actionID = (id, bar) => {
    if (typeof id !== "string") return false;
    if (bar && /^modifier:(ctrl|alt|shift)$/.test(id)) return true;
    if (keyID(id)) return true;
    const sequence = /^sequence:([^:]+):(key:.*)$/.exec(id);
    if (!sequence) return false;
    try { const name = decodeURIComponent(sequence[1]); return prefixName(name) && encodeURIComponent(name) === sequence[1] && keyID(sequence[2]); }
    catch { return false; }
  };
  const list = (items, bar) => Array.isArray(items) && items.length <= 100 && new Set(items).size === items.length && items.every((id) => actionID(id, bar));
  if (!exact(value, ["version", "layout", "prefixes"]) || value.version !== 1 || !exact(value.layout, ["bar", "favorites"]) || !list(value.layout.bar, true) || !list(value.layout.favorites, false) || !object(value.prefixes)) return false;
  const names = Object.keys(value.prefixes);
  return names.length <= 20 && new Set(names.map((name) => name.toLowerCase())).size === names.length && names.every((name) => prefixName(name) && keyID(value.prefixes[name]));
}
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'nonce-"
  + STYLE_NONCE
  + "'; connect-src 'self'; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'";
// Playwright WebKit's screenshot synchronizer inserts the exact no-op style
// `body {}`. A focused test may authorize only that hash so the diagnostic
// capture itself does not become the sole CSP violation.
const PLAYWRIGHT_SCREENSHOT_STYLE_HASH = "'sha256-YjaKGiklmzC6wjXA513HAMmzus8VE61XCOT+SmwNZWA='";
const SOURCE = "RRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRR";
const SOURCE_B = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB";
const LIVENESS_PREFIX = "PERSEA-LIVENESS/1 ";
// Operational refusals travel in-band on an open socket (front door
// attachment error policy): one request's outcome, never a close.
const REFUSAL_PREFIX = "PERSEA-REFUSAL/1 ";
const AUTHORITY = Object.freeze({
  realm: "local",
  server: "private",
  uid: 1000,
  selector_kind: "socket_path",
  selector_value: "/tmp/private.sock",
  boot_id: "boot-reopen",
  server_pid: 42,
  server_start: 100,
  session_id: "$7",
  session_created: 207,
});
const DRAFT_SCOPE = JSON.stringify([
  "local", "private", "socket_path", "/tmp/private.sock", "boot-reopen", "$7", 1000, 42, 100, 200 + 7,
]);
const AUTHORITY_B = Object.freeze({ ...AUTHORITY, realm: "remote", session_id: "$8", session_created: 208 });
const DRAFT_SCOPE_B = JSON.stringify([
  "remote", "private", "socket_path", "/tmp/private.sock", "boot-reopen", "$8", 1000, 42, 100, 208,
]);

function token() {
  return crypto.randomBytes(32).toString("base64url");
}

function encodeFrame(opcode, payload) {
  const length = payload.length;
  let header;
  if (length < 126) {
    header = Buffer.from([0x80 | opcode, length]);
  } else if (length < 65_536) {
    header = Buffer.alloc(4);
    header[0] = 0x80 | opcode;
    header[1] = 126;
    header.writeUInt16BE(length, 2);
  } else {
    header = Buffer.alloc(10);
    header[0] = 0x80 | opcode;
    header[1] = 127;
    header.writeBigUInt64BE(BigInt(length), 2);
  }
  return Buffer.concat([header, payload]);
}

// Returns the complete frames at the head of `buffer` plus the unread rest.
function decodeFrames(buffer) {
  const frames = [];
  let offset = 0;
  while (offset + 2 <= buffer.length) {
    const first = buffer[offset];
    const second = buffer[offset + 1];
    const fin = (first & 0x80) !== 0;
    const opcode = first & 0x0f;
    const masked = (second & 0x80) !== 0;
    let length = second & 0x7f;
    let cursor = offset + 2;
    if (length === 126) {
      if (cursor + 2 > buffer.length) break;
      length = buffer.readUInt16BE(cursor);
      cursor += 2;
    } else if (length === 127) {
      if (cursor + 8 > buffer.length) break;
      length = Number(buffer.readBigUInt64BE(cursor));
      cursor += 8;
    }
    let mask;
    if (masked) {
      if (cursor + 4 > buffer.length) break;
      mask = buffer.subarray(cursor, cursor + 4);
      cursor += 4;
    }
    if (cursor + length > buffer.length) break;
    const payload = Buffer.from(buffer.subarray(cursor, cursor + length));
    if (mask) for (let i = 0; i < payload.length; i += 1) payload[i] ^= mask[i % 4];
    frames.push({ fin, opcode, payload });
    offset = cursor + length;
  }
  return { frames, rest: buffer.subarray(offset) };
}

class Socket {
  constructor(raw, onText, onClose) {
    this.raw = raw;
    this.buffer = Buffer.alloc(0);
    this.fragments = [];
    this.closed = false;
    this.onText = onText;
    this.onClose = onClose;
    raw.on("data", (chunk) => this.feed(chunk));
    raw.on("close", () => this.finish("socket_closed"));
    raw.on("error", () => this.finish("socket_error"));
  }

  feed(chunk) {
    this.buffer = Buffer.concat([this.buffer, chunk]);
    const { frames, rest } = decodeFrames(this.buffer);
    this.buffer = rest;
    for (const frame of frames) {
      if (frame.opcode === 0x8) {
        // Answer the peer's close with our own (RFC 6455 §5.5.1) and release
        // the TCP socket: a browser that sent a close waits for the echo, and
        // a fixture that never sends one keeps the server from closing.
        this.write(0x8, frame.payload.subarray(0, 2));
        this.finish(frame.payload.length > 2 ? frame.payload.subarray(2).toString("utf8") : "client_close");
        setTimeout(() => { try { this.raw.destroy(); } catch { /* already gone */ } }, 50);
        return;
      }
      if (frame.opcode === 0x9) {
        this.write(0xa, frame.payload);
        continue;
      }
      if (frame.opcode === 0xa) continue;
      if (frame.opcode === 0x1 || frame.opcode === 0x0) {
        this.fragments.push(frame.payload);
        if (!frame.fin) continue;
        const text = Buffer.concat(this.fragments).toString("utf8");
        this.fragments = [];
        this.onText(text);
        continue;
      }
      this.close(1003, "websocket_message_type");
    }
  }

  write(opcode, payload) {
    if (this.closed || this.raw.destroyed) return;
    try { this.raw.write(encodeFrame(opcode, payload)); } catch { /* the peer is gone */ }
  }

  sendText(text) {
    this.write(0x1, Buffer.from(text, "utf8"));
  }

  // The real front door closes with 1011 + the canonical reason (1000 for
  // control displacement). The browser reads the reason from this frame.
  close(code, reason) {
    if (this.closed) return;
    const body = Buffer.alloc(2 + Buffer.byteLength(reason));
    body.writeUInt16BE(code, 0);
    body.write(reason, 2);
    this.write(0x8, body);
    this.finish(reason);
    setTimeout(() => { try { this.raw.destroy(); } catch { /* already gone */ } }, 50);
  }

  finish(reason) {
    if (this.closed) return;
    this.closed = true;
    this.onClose(reason);
  }
}

function readJSON(request) {
  return new Promise((resolve) => {
    let body = "";
    request.setEncoding("utf8");
    request.on("data", (chunk) => { body += chunk; });
    request.on("end", () => {
      try { resolve(JSON.parse(body)); } catch { resolve(undefined); }
    });
  });
}

function subprotocols(request) {
  const header = request.headers["sec-websocket-protocol"] || "";
  const values = header.split(",").map((value) => value.trim()).filter(Boolean);
  const read = (prefix) => {
    const found = values.filter((value) => value.startsWith(prefix));
    return found.length === 1 ? found[0].slice(prefix.length) : undefined;
  };
  return {
    values,
    handle: read("persea-handle."),
    mode: read("persea-mode."),
    csrf: read("persea-csrf."),
    history: read("persea-history."),
    engine: read("persea-engine."),
    takeover: values.includes("persea-takeover.v1"),
  };
}

function cookieCSRF(request) {
  const cookie = request.headers.cookie || "";
  const match = cookie.split(";").map((value) => value.trim()).find((value) => value.startsWith("__Host-persea-terminal-csrf="));
  return match ? match.slice(match.indexOf("=") + 1) : "";
}


// --- /api/snippets double  -----------------
//
// A faithful stand-in for internal/frontdoor/snippet_store.go + snippet_api.go:
// the closed record grammar, the 256-snippet cap, the 20-entry manual clip
// ring with a 30-minute default and protected extended retention, the internal
// server-owned `osc52` record plus its ordinary canonical clipboard item,
// body-revision CAS on PATCH/DELETE, strong decimal `If-Match` on the OSC
// upsert, and the shared route contract on every method: identity/CSRF on every
// mutation, same-origin only, strict method, exact JSON content type, a
// bounded body read BEFORE the decode, a closed field set, bounded fixed
// error text that never echoes a body, and `Cache-Control: no-store`
// everywhere. Knobs let a gate force the capacity, conflict and unavailable
// paths the UI must render honestly.

const SNIPPET_BODY_MAX = 16 * 1024;
const SNIPPET_REQUEST_MAX = 128 * 1024;
const SNIPPET_MAX_SNIPPETS = 256;
const SNIPPET_CLIP_RING = 20;
const SNIPPET_CLIP_TTL_MS = 30 * 60 * 1000;
const SNIPPET_OSC_ID = "osc52";
const CLIPBOARD_RETENTIONS = [0, 1800, 14400, 86400, 604800, 2592000];
const longerRetention = (a,b) => a === 0 || b === 0 ? 0 : Math.max(a,b);
const CANONICAL_KEY = /^[a-z][a-z0-9_]{0,63}$/;

function snippetPreview(body) {
  const flat = body.replace(/[\n\t]/g, " ");
  const runes = Array.from(flat);
  return runes.length <= 80 ? flat : runes.slice(0, 80).join("");
}

function validText(value, maxRunes) {
  if (typeof value !== "string" || Array.from(value).length > maxRunes) return false;
  for (const character of value) {
    const code = character.codePointAt(0);
    if (code < 0x20 || (code >= 0x7f && code <= 0x9f)) return false;
  }
  return true;
}

function bodyVerdict(body) {
  if (typeof body !== "string") return "invalid";
  if (Buffer.byteLength(body, "utf8") > SNIPPET_BODY_MAX) return "too_large";
  if (body === "") return "invalid";
  for (const character of body) {
    const code = character.codePointAt(0);
    if (code === 0x0a || code === 0x09) continue;
    if (code < 0x20 || (code >= 0x7f && code <= 0x9f) || code === 0xfffd) return "invalid";
  }
  return "ok";
}

function readBoundedBody(request, limit) {
  return new Promise((resolve) => {
    let size = 0;
    const chunks = [];
    let over = false;
    request.on("data", (chunk) => {
      size += chunk.length;
      if (size > limit) { over = true; return; }
      chunks.push(chunk);
    });
    request.on("end", () => resolve(over ? { tooLarge: true } : { text: Buffer.concat(chunks).toString("utf8") }));
    request.on("error", () => resolve({ text: "" }));
  });
}

function createSnippetStore(csrfToken) {
  let clipboardPreferences = { version: 1, default_retention_seconds: 1800, revision: 1 };
  const state = {
    records: new Map(),
    nextId: 0,
    unavailable: false,
    mutationsUnavailable: false,
    mutationsDrop: false,
    // Force the next N creates (POST or OSC PUT) to answer 507 as a full file
    // would: the 1 MiB cap can bind before the item cap, so the UI may never
    // treat a count under the limit as room.
    force507: 0,
    // Force the next N CAS-bearing writes to answer with a stale current
    // record, exactly as a concurrent device would.
    forceConflict: 0,
    // Milliseconds a GET is held before answering: the single-flight probe.
    delayMs: 0,
    // Milliseconds a mutation is held before answering: the optimistic-row probe.
    mutationDelayMs: 0,
    clockSkewMs: 0,
    counters: { get: 0, post: 0, patch: 0, del: 0, oscPut: 0, refused: 0, mutations: 0 },
    getsInFlight: 0,
    getsInFlightPeak: 0,
  };
  const now = () => Date.now() + state.clockSkewMs;
  const mintId = () => {
    state.nextId += 1;
    return state.nextId.toString(16).padStart(32, "0");
  };
  const live = (record) => record.expires_at === null || now() < Date.parse(record.expires_at);
  const liveRecords = () => [...state.records.values()].filter(live);
  const dropExpired = () => {
    for (const [id, record] of [...state.records.entries()]) if (!live(record)) state.records.delete(id);
  };
  const evictClips = (keepID) => {
    dropExpired();
    const oscBody=state.records.get(SNIPPET_OSC_ID)?.body;
    const clips=liveRecords().filter(r=>r.kind==="clip"&&r.id!==SNIPPET_OSC_ID&&r.body!==oscBody).sort((a,b)=>a.updated_at.localeCompare(b.updated_at)||a.id.localeCompare(b.id));
    let count=clips.length;
    for(const item of clips){
      if(count<=SNIPPET_CLIP_RING)break;
      if(item.id===keepID||item.retention_seconds!==1800||item.expires_at===null||Date.parse(item.expires_at)>now()+1800*1000)continue;
      state.records.delete(item.id);count--;
    }
    if(count>SNIPPET_CLIP_RING)throw new Error("snippet store full");
  };
  const list = () => liveRecords().filter(record => record.id !== SNIPPET_OSC_ID)
    .map((record) => (record.kind === "clip" ? { ...record, preview: snippetPreview(record.body) } : { ...record }))
    .sort((left, right) => {
      if (left.pinned !== right.pinned) return left.pinned ? -1 : 1;
      if (left.updated_at !== right.updated_at) return left.updated_at < right.updated_at ? 1 : -1;
      return left.id < right.id ? -1 : 1;
    });

  // A write from "another device" with no browser involved: the cross-device
  // freshness probe posts through this, so the gate never has to pretend a
  // second profile made an HTTP request it did not make.
  const renewed = (record, seconds, origin) => {
    const stamp = Math.max(now(), Date.parse(record.updated_at) + 1);
    const retention = longerRetention(record.retention_seconds, seconds);
    return { ...record, origin: origin ?? record.origin, revision: record.revision + 1,
      updated_at: new Date(stamp).toISOString(), retention_seconds: retention,
      expires_at: retention === 0 ? null : new Date(Math.max(stamp + retention * 1000, Date.parse(record.expires_at) || 0)).toISOString() };
  };
  const writeDirect = (input) => {
    dropExpired();
    const seconds = input.retention_seconds ?? (input.kind === "snippet" ? 0 : clipboardPreferences.default_retention_seconds);
    const stamp = new Date(now()).toISOString();
    if (input.kind === "osc") {
      const current = state.records.get(SNIPPET_OSC_ID);
      const record = current && current.body === input.body ? renewed(current, seconds, input.origin) : {
        id: SNIPPET_OSC_ID, kind: "clip", label: "", body: input.body, pinned: false,
        origin: input.origin || "", revision: (current?.revision ?? 0) + 1,
        created_at: current?.created_at ?? stamp, updated_at: stamp, retention_seconds: seconds,
        expires_at: seconds === 0 ? null : new Date(now() + seconds * 1000).toISOString(),
      };
      state.records.set(SNIPPET_OSC_ID, record);
      writeDirect({ ...input, kind: "clip", retention_seconds: seconds });
      return record;
    }
    const duplicate = liveRecords().filter(record => record.id !== SNIPPET_OSC_ID && record.body === input.body).sort((a,b)=>a.id.localeCompare(b.id))[0];
    if (duplicate) { const previous=new Map(state.records);const record = renewed(duplicate, seconds, input.origin); state.records.set(record.id, record);try{evictClips(record.id);}catch(error){state.records=previous;throw error;}return record; }
    const id = mintId();
    const record = { id, kind: input.kind, label: input.label || "", body: input.body, pinned: Boolean(input.pinned),
      origin: input.origin || "", revision: 1, created_at: stamp, updated_at: stamp, retention_seconds: seconds,
      expires_at: seconds === 0 ? null : new Date(now() + seconds * 1000).toISOString() };
    const previous=new Map(state.records);state.records.set(id, record);
    try{evictClips(id);}catch(error){state.records=previous;throw error;}return record;
  };

  const reset = () => {
    state.records.clear();
    clipboardPreferences = { version: 1, default_retention_seconds: 1800, revision: 1 };
    state.nextId = 0;
    state.unavailable = false;
    state.mutationsUnavailable = false;
    state.mutationsDrop = false;
    state.force507 = 0;
    state.forceConflict = 0;
    state.delayMs = 0;
    state.mutationDelayMs = 0;
    state.clockSkewMs = 0;
    state.getsInFlight = 0;
    state.getsInFlightPeak = 0;
    for (const key of Object.keys(state.counters)) state.counters[key] = 0;
  };

  const control = (input) => {
    if (input.snippetReset) reset();
    if (typeof input.snippetUnavailable === "boolean") state.unavailable = input.snippetUnavailable;
    if (typeof input.snippetMutationsUnavailable === "boolean") state.mutationsUnavailable = input.snippetMutationsUnavailable;
    if (typeof input.snippetMutationsDrop === "boolean") state.mutationsDrop = input.snippetMutationsDrop;
    if (typeof input.snippetForce507 === "number") state.force507 = input.snippetForce507;
    if (typeof input.snippetForceConflict === "number") state.forceConflict = input.snippetForceConflict;
    if (typeof input.snippetDelayMs === "number") state.delayMs = input.snippetDelayMs;
    if (typeof input.snippetMutationDelayMs === "number") state.mutationDelayMs = input.snippetMutationDelayMs;
    if (typeof input.snippetClockSkewMs === "number") state.clockSkewMs = input.snippetClockSkewMs;
    if (Array.isArray(input.snippetWrite)) for (const entry of input.snippetWrite) writeDirect(entry);
    if (input.snippetSeed) {
      const { snippets = 0, clips = 0 } = input.snippetSeed;
      for (let index = 0; index < snippets; index += 1) writeDirect({ kind: "snippet", label: `seed-${index}`, body: `seed body ${index}`, origin: "seed" });
      for (let index = 0; index < clips; index += 1) writeDirect({ kind: "clip", body: `seed clip ${index}`, origin: "seed" });
    }
  };

  const stats = () => ({
    ...state.counters,
    getsInFlightPeak: state.getsInFlightPeak,
    items: liveRecords().length,
    snippets: liveRecords().filter((record) => record.kind === "snippet").length,
    manualClips: liveRecords().filter((record) => record.kind === "clip" && record.id !== SNIPPET_OSC_ID).length,
    osc: liveRecords().filter((record) => record.id === SNIPPET_OSC_ID).length,
    oscRevision: state.records.get(SNIPPET_OSC_ID)?.revision ?? 0,
    oscBody: state.records.get(SNIPPET_OSC_ID)?.body ?? "",
    ids: liveRecords().map((record) => record.id),
  });

  // --- the route -------------------------------------------------------------

  const send = (response, status, payload) => {
    response.setHeader("Cache-Control", "no-store");
    response.setHeader("Content-Type", "application/json");
    response.writeHead(status);
    response.end(JSON.stringify(payload));
  };
  const refuse = (response, status, text) => {
    state.counters.refused += 1;
    response.setHeader("Cache-Control", "no-store");
    response.setHeader("Content-Type", "text/plain");
    response.writeHead(status);
    response.end(text);
  };

  async function handle(request, response, url, origin) {
    response.setHeader("Cache-Control", "no-store");
    const method = request.method;
    const path = url.pathname;
    const isCollection = path === "/api/snippets";
    const id = isCollection ? "" : path.slice("/api/snippets/".length);
    if (!isCollection && (id === "" || id.includes("/"))) { refuse(response, 404, "snippet not found"); return; }
    if (url.search !== "") { refuse(response, 400, "invalid snippet request"); return; }
    const mutation = method !== "GET";
    if (mutation) {
      // Identity + CSRF, exactly as the front door: the header token, the
      // cookie it must equal, and a same-origin request.
      const requestOrigin = request.headers.origin || "";
      const site = request.headers["sec-fetch-site"];
      if (request.headers["x-persea-csrf"] !== csrfToken
        || cookieCSRF(request) !== csrfToken
        || requestOrigin !== origin
        || (site !== undefined && site !== "same-origin")) {
        refuse(response, 403, "forbidden");
        return;
      }
      const contentType = request.headers["content-type"];
      if (contentType !== "application/json") { refuse(response, 400, "invalid snippet request"); return; }
    }
    if (isCollection && method !== "GET" && method !== "POST") { response.setHeader("Allow", "GET, POST"); refuse(response, 405, "method not allowed"); return; }
    if (!isCollection && id === SNIPPET_OSC_ID && method !== "PUT" && method !== "DELETE") { response.setHeader("Allow", "PUT"); refuse(response, 405, "method not allowed"); return; }
    if (!isCollection && id !== SNIPPET_OSC_ID && method !== "PATCH" && method !== "DELETE") { response.setHeader("Allow", "PATCH, DELETE"); refuse(response, 405, "method not allowed"); return; }

    if (method === "GET") {
      state.counters.get += 1;
      state.getsInFlight += 1;
      state.getsInFlightPeak = Math.max(state.getsInFlightPeak, state.getsInFlight);
      if (state.delayMs > 0) await new Promise((done) => setTimeout(done, state.delayMs));
      state.getsInFlight -= 1;
      if (state.unavailable) { refuse(response, 503, "snippet store unavailable"); return; }
      send(response, 200, { items: list() });
      return;
    }

    if (state.mutationDelayMs > 0) await new Promise((done) => setTimeout(done, state.mutationDelayMs));
    // an outage that hits WRITES ONLY, so a gate can watch the client
    // learn about it from a mutation. `snippetUnavailable` fails reads too, so
    // the poll would publish the outage on its own and the mutation's own
    // reporting could not be measured.
    // The counter has to move BEFORE the refusal, or a gate cannot tell "the
    // client did not mutate" from "the store refused the mutation".
    if (state.mutationsUnavailable) { state.counters.mutations += 1; refuse(response, 503, "snippet store unavailable"); return; }
    // Transport loss: no response at all, the socket just goes away.
    if (state.mutationsDrop) { state.counters.mutations += 1; request.destroy(); return; }
    const raw = await readBoundedBody(request, SNIPPET_REQUEST_MAX);
    if (raw.tooLarge) { refuse(response, 413, "snippet too large"); return; }
    let wire;
    try {
      wire = JSON.parse(raw.text);
    } catch {
      refuse(response, 400, "invalid snippet request");
      return;
    }
    if (wire === null || typeof wire !== "object" || Array.isArray(wire)) { refuse(response, 400, "invalid snippet request"); return; }
    for (const key of Object.keys(wire)) if (!CANONICAL_KEY.test(key)) { refuse(response, 400, "invalid snippet request"); return; }

    const allowed = (keys) => Object.keys(wire).every((key) => keys.includes(key));

    if (method === "POST") {
      state.counters.post += 1;
      state.counters.mutations += 1;
      if (!allowed(["kind", "label", "body", "pinned", "origin", "retention_seconds"])) { refuse(response, 400, "invalid snippet request"); return; }
      if (wire.retention_seconds !== undefined && !CLIPBOARD_RETENTIONS.includes(wire.retention_seconds)) { refuse(response, 400, "invalid retention"); return; }
      if (wire.kind !== "snippet" && wire.kind !== "clip") { refuse(response, 400, "invalid snippet request"); return; }
      if (wire.kind === "clip" && (wire.label !== undefined || wire.pinned !== undefined)) { refuse(response, 400, "invalid snippet request"); return; }
      if (typeof wire.origin === "string" && wire.origin.trim() === SNIPPET_OSC_ID) { refuse(response, 400, "invalid snippet request"); return; }
      const verdict = bodyVerdict(wire.body);
      if (verdict === "too_large") { refuse(response, 413, "snippet too large"); return; }
      if (verdict !== "ok") { refuse(response, 400, "invalid snippet request"); return; }
      if (wire.origin !== undefined && !validText(wire.origin, 32)) { refuse(response, 400, "invalid snippet request"); return; }
      if (wire.kind === "snippet") {
        if (typeof wire.label !== "string" || wire.label.trim() === "" || !validText(wire.label, 64)) { refuse(response, 400, "invalid snippet request"); return; }
      }
      if (state.unavailable) { refuse(response, 503, "snippet store unavailable"); return; }
      if (state.force507 > 0) { state.force507 -= 1; refuse(response, 507, "snippet store full"); return; }
      dropExpired();
      if (wire.kind === "snippet" && !liveRecords().some(record=>record.id!==SNIPPET_OSC_ID&&record.body===wire.body) && liveRecords().filter((record) => record.kind === "snippet").length >= SNIPPET_MAX_SNIPPETS) {
        refuse(response, 507, "snippet store full");
        return;
      }
      try { send(response, 201, writeDirect({ kind: wire.kind, label: wire.label, body: wire.body, pinned: wire.pinned, origin: wire.origin, retention_seconds: wire.retention_seconds })); } catch { refuse(response,507,"snippet store full"); }
      return;
    }

    if (method === "PUT") {
      state.counters.oscPut += 1;
      state.counters.mutations += 1;
      if (id !== SNIPPET_OSC_ID) { refuse(response, 405, "method not allowed"); return; }
      if (!allowed(["body", "origin"])) { refuse(response, 400, "invalid snippet request"); return; }
      const verdict = bodyVerdict(wire.body);
      if (verdict === "too_large") { refuse(response, 413, "snippet too large"); return; }
      if (verdict !== "ok") { refuse(response, 400, "invalid snippet request"); return; }
      const header = request.headers["if-match"];
      let expect;
      if (header !== undefined) {
        if (!/^"(0|[1-9][0-9]*)"$/.test(header)) { refuse(response, 400, "invalid snippet request"); return; }
        expect = Number(header.slice(1, -1));
      }
      if (state.unavailable) { refuse(response, 503, "snippet store unavailable"); return; }
      if (state.force507 > 0) { state.force507 -= 1; refuse(response, 507, "snippet store full"); return; }
      dropExpired();
      const current = state.records.get(SNIPPET_OSC_ID);
      const revision = current ? current.revision : 0;
      if (state.forceConflict > 0) {
        state.forceConflict -= 1;
        send(response, 412, current ? { ...current } : { id: SNIPPET_OSC_ID, revision: 0 });
        return;
      }
      if (expect !== undefined && expect !== revision) {
        send(response, 412, current ? { ...current } : { id: SNIPPET_OSC_ID, revision: 0 });
        return;
      }
      const previous=new Map(state.records);try { send(response, 200, writeDirect({ kind: "osc", body: wire.body, origin: wire.origin })); } catch { state.records=previous;refuse(response,507,"snippet store full"); }
      return;
    }

    // PATCH / DELETE: body-revision CAS.
    const target = state.records.get(id);
    if (method === "PATCH") {
      state.counters.patch += 1;
      state.counters.mutations += 1;
      if (!allowed(["label", "body", "pinned", "revision", "retention_seconds"])) { refuse(response, 400, "invalid snippet request"); return; }
    } else {
      state.counters.del += 1;
      state.counters.mutations += 1;
      if (!allowed(["revision"])) { refuse(response, 400, "invalid snippet request"); return; }
    }
    if (!Number.isSafeInteger(wire.revision) || wire.revision < 1) { refuse(response, 400, "invalid snippet request"); return; }
    if (state.unavailable) { refuse(response, 503, "snippet store unavailable"); return; }
    if (!target || !live(target)) { refuse(response, 404, "snippet not found"); return; }
    if (state.forceConflict > 0) { state.forceConflict -= 1; send(response, 409, { ...target }); return; }
    if (target.revision !== wire.revision) { send(response, 409, { ...target }); return; }
    if (method === "DELETE") {
      state.records.delete(id);
      response.setHeader("Cache-Control", "no-store");
      response.writeHead(204);
      response.end();
      return;
    }
    if (id === SNIPPET_OSC_ID) { refuse(response, 400, "invalid snippet request"); return; }
    if (target.kind === "clip" && wire.pinned !== undefined) { refuse(response, 400, "invalid snippet request"); return; }
    if (wire.retention_seconds !== undefined && !CLIPBOARD_RETENTIONS.includes(wire.retention_seconds)) { refuse(response, 400, "invalid retention"); return; }
    let next = renewed(target, wire.retention_seconds ?? target.retention_seconds);
    if (wire.retention_seconds !== undefined) {
      next.retention_seconds = wire.retention_seconds;
      next.expires_at = wire.retention_seconds === 0 ? null : new Date(Date.parse(next.updated_at) + wire.retention_seconds * 1000).toISOString();
    }
    if (wire.label !== undefined) next.label = String(wire.label).trim();
    if (wire.body !== undefined) {
      const verdict = bodyVerdict(wire.body);
      if (verdict === "too_large") { refuse(response, 413, "snippet too large"); return; }
      if (verdict !== "ok") { refuse(response, 400, "invalid snippet request"); return; }
      next.body = wire.body;
    }
    if (wire.pinned !== undefined) next.pinned = Boolean(wire.pinned);
    const duplicate = liveRecords().find(record => record.id !== id && record.id !== SNIPPET_OSC_ID && record.body === next.body);
    if (duplicate) { const previousExpiry=next.expires_at;next = renewed(duplicate, next.retention_seconds, next.origin); if(next.expires_at!==null&&previousExpiry!==null)next.expires_at=new Date(Math.max(Date.parse(next.expires_at),Date.parse(previousExpiry))).toISOString();state.records.delete(id); }
    state.records.set(next.id, next);
    send(response, 200, next);
  }

  async function preferences(request, response, url, origin) {
    response.setHeader("Cache-Control", "no-store"); response.setHeader("X-Content-Type-Options", "nosniff");
    const reply = code => { response.setHeader("ETag", `"${clipboardPreferences.revision}"`); send(response, code, clipboardPreferences); };
    if (url.search) { refuse(response,400,"invalid preferences request"); return; }
    if (request.method === "GET") { reply(200); return; }
    if (request.method !== "PUT") { refuse(response,405,"method not allowed"); return; }
    if(request.headers.origin!==origin || request.headers["sec-fetch-site"]!=="same-origin" || request.headers["x-persea-csrf"]!==csrfToken || cookieCSRF(request)!==csrfToken){refuse(response,403,"forbidden");return;}
    if(request.headers["content-type"]!=="application/json"){refuse(response,400,"invalid preferences request");return;}
    const raw=await readBoundedBody(request,1024);if(raw.tooLarge){refuse(response,413,"preferences too large");return;}let body;try{body=JSON.parse(raw.text);}catch{refuse(response,400,"invalid preferences request");return;}
    if(!body || Object.keys(body).sort().join(",")!=="default_retention_seconds,revision" || !CLIPBOARD_RETENTIONS.includes(body.default_retention_seconds) || !Number.isSafeInteger(body.revision) || body.revision<1){refuse(response,400,"invalid preferences request");return;}
    if(body.revision!==clipboardPreferences.revision){reply(412);return;}
    clipboardPreferences={version:1,default_retention_seconds:body.default_retention_seconds,revision:clipboardPreferences.revision+1};reply(200);
  }
  return { handle, control, reset, stats, writeDirect, preferences, clipboardPreferences: () => ({...clipboardPreferences}) };
}

function hermeticTLS() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "persea-reopen-tls-"));
  const keyPath = path.join(dir, "key.pem");
  const certPath = path.join(dir, "cert.pem");
  const result = childProcess.spawnSync("openssl", [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", keyPath, "-out", certPath,
    "-days", "1", "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  if (result.status !== 0) throw new Error(`hermetic TLS material failed: ${String(result.stderr).slice(-400)}`);
  return { dir, key: fs.readFileSync(keyPath), cert: fs.readFileSync(certPath) };
}

function startFixture(ui, options = {}) {
  const liveEncoding = options.liveEncoding === "utf8" ? "utf8" : "binary";
  const documentCSP = options.playwrightScreenshotStyle
    ? CSP.replace("; connect-src", ` ${PLAYWRIGHT_SCREENSHOT_STYLE_HASH}; connect-src`)
    : CSP;
  const index = fs.readFileSync(path.join(ui, "dist/index.html"), "utf8").replace("__PERSEA_STYLE_NONCE__", STYLE_NONCE);
  const files = {
    "/app.js": { file: path.join(ui, "dist/app.js"), type: "text/javascript" },
    "/app.css": { file: path.join(ui, "dist/app.css"), type: "text/css" },
    "/xterm.css": { file: path.join(ui, "dist/xterm.css"), type: "text/css" },
    // session memory installable shell. index.html links these, so a fixture that does not
    // serve them makes the page 404 assets the front door always has
    // (`requiredBundleFiles` refuses to start a release without them). Same paths,
    // same media types.
    "/manifest.webmanifest": { file: path.join(ui, "dist/manifest.webmanifest"), type: "application/manifest+json" },
    "/icon-192.png": { file: path.join(ui, "dist/icon-192.png"), type: "image/png" },
    "/icon-512.png": { file: path.join(ui, "dist/icon-512.png"), type: "image/png" },
    "/apple-touch-icon.png": { file: path.join(ui, "dist/apple-touch-icon.png"), type: "image/png" },
  };

  // --- front-door state ------------------------------------------------------
  const state = {
    // Closed-set scenario knobs, all operator-driven through /__fixture/control.
    sessionState: "open",          // open | adoptable | missing
    switchSessions: false,
    terminal_touchSwitcherMetadata: false,
    sessionBState: "adoptable",
    adoptedB: false,
    holdAdoptionMs: 0,
    holdHandleMs: 0,
    holdInventoryMs: 0,
    holdInventoryAfter: 0,
    holdTakeoverMs: 0,
    holdLeaseRefusalMs: 0,
    failSessionBConnections: 0,
    replayFocusReporting: false,   // replay carries ESC[?1004h
    refuseInputs: 0,               // broker refuses the next N INPUT frames regardless of mode (legacy close path)
    refuseInputsInBand: 0,         // front relays the next N INPUT refusals in-band; the socket stays open
    refuseResize: "",              // non-empty: RESIZE_REQUEST answered in-band with this code instead of a geometry event
    closeResize: "",               // non-empty: RESIZE_REQUEST answered by closing the socket with this fatal reason (the broker's post-mutation verdict)
    refuseRefit: "",               // non-empty: out-of-band width refit refusal code
	holdRefitMs: 0,                // one-shot: delay the refit response so the input seal can be tested causally
	refitPrepareMismatch: false,   // one-shot: broker result and the next PREPARE name different successor sources
    holdModeGrant: false,          // broker records MODE_REQUEST but withholds the MODE grant
    holdPrepareMs: 0,              // one-shot: the next attachment's PREPARE is delayed this long (a slow reopen)
    holdResizeMs: 0,               // one-shot: the next RESIZE_REQUEST's committed RESIZE is delayed this long (the Fit seal stays up)
    bindingsExpired: false,        // /api/attachment-handles answers 410 (TTL elapsed)
    adopted: false,
    imageStaging: false,           // inventory advertises image staging (the dashboard then links image_realm)
    handles: new Map(),            // handle -> { purpose, consumed }
    offers: new Map(),             // losing handle -> { used }
    bindings: new Set(),           // bound sources
    bindingSessions: new Map(),    // source -> A | B
	  sourceA: SOURCE,
	  sourceB: SOURCE_B,
    handleLedger: [],              // exact source/identity mint authority
    inventoryLedger: [],           // exact inventory capability batches
    takeoverClaims: [],            // exact offer/source presented to takeover
    lease: null,                   // { holder, attachment }
    leaseB: null,
    attachments: [],               // transcript per WebSocket attachment
    preferences: { version: 1, theme: "default", font_size: null, composer_font_size: 11, default_session: null, revision: 0, stored: false, available: true },
    dashboardPreferences: { version: 1, favorites: [], revision: 0, available: true },
    preferenceOperations: [],
    holdPreferencePuts: 0,
    preferencePutReleases: [],
    preferencesCorrupt: false,
    keyboardPreferences: defaultKeyboardRecord(),
    keyboardPreferenceOperations: [],
    holdKeyboardPreferencePuts: 0,
    keyboardPreferencePutReleases: [],
    keyboardPreferencesCorrupt: false,
    geometryA: { columns: 80, rows: 24 },
    geometryB: { columns: 100, rows: 30 },
    refitOperations: [],
    counters: { documents: 0, inventory: 0, handleRequests: 0, takeovers: 0, adoptions: 0, refits: 0, websockets: 0, replays: 0, preferencesGet: 0, preferencesPut: 0, keyboardPreferencesGet: 0, keyboardPreferencesPut: 0 },
  };
  // The /api/snippets store double (clipboard). It is front-door state like the
  // handles and the lease, not a browser concern.
  const snippets = createSnippetStore(CSRF_TOKEN);
  const mint = (purpose, session = "A") => {
    const handle = token();
    state.handles.set(handle, { purpose, consumed: false, session });
    return handle;
  };
  const inventoryHandles = (session) => {
    const handles = { alias: mint("alias", session), observe: mint("observe", session), control: mint("control", session) };
    state.inventoryLedger.push({ session, ...handles });
    return handles;
  };
  const reset = () => {
    for (const release of state.preferencePutReleases.splice(0)) release();
    for (const release of state.keyboardPreferencePutReleases.splice(0)) release();
    state.sessionState = "open";
    state.switchSessions = false;
    state.terminal_touchSwitcherMetadata = false;
    state.sessionBState = "adoptable";
    state.adoptedB = false;
    state.holdAdoptionMs = 0;
    state.holdHandleMs = 0;
    state.holdInventoryMs = 0;
    state.holdInventoryAfter = 0;
    state.holdTakeoverMs = 0;
    state.holdLeaseRefusalMs = 0;
    state.failSessionBConnections = 0;
    state.replayFocusReporting = false;
    state.refuseInputs = 0;
    state.refuseInputsInBand = 0;
    state.refuseResize = "";
    state.closeResize = "";
    state.refuseRefit = "";
    state.holdModeGrant = false;
    state.holdPrepareMs = 0;
    state.holdResizeMs = 0;
	state.holdRefitMs = 0;
	state.refitPrepareMismatch = false;
    state.bindingsExpired = false;
    state.adopted = false;
    state.imageStaging = false;
    state.preferences = { version: 1, theme: "default", font_size: null, composer_font_size: 11, default_session: null, revision: 0, stored: false, available: true };
    state.dashboardPreferences = { version: 1, favorites: [], revision: 0, available: true };
    state.preferenceOperations = [];
    state.holdPreferencePuts = 0;
    state.preferencesCorrupt = false;
    state.keyboardPreferences = defaultKeyboardRecord();
    state.keyboardPreferenceOperations = [];
    state.holdKeyboardPreferencePuts = 0;
    state.keyboardPreferencesCorrupt = false;
    state.handles.clear();
    state.offers.clear();
    state.bindings.clear();
    state.bindingSessions.clear();
	state.sourceA = SOURCE;
	state.sourceB = SOURCE_B;
    state.handleLedger = [];
    state.inventoryLedger = [];
    state.takeoverClaims = [];
    for (const attachment of state.attachments) if (attachment.socket && !attachment.socket.closed) attachment.socket.close(1000, "fixture_reset");
    state.lease = null;
    state.leaseB = null;
    state.attachments = [];
    state.geometryA = { columns: 80, rows: 24 };
    state.geometryB = { columns: 100, rows: 30 };
    state.refitOperations = [];
    for (const key of Object.keys(state.counters)) state.counters[key] = 0;
    snippets.reset();
  };

  const inventory = () => {
    const sessions = [];
    if (state.sessionState !== "missing") {
      sessions.push({
        handles: inventoryHandles("A"),
        realm: "local",
        server: "private",
        server_status: "ok",
        session_id: "$7",
        name: "alpha",
        width: state.geometryA.columns,
        height: state.geometryA.rows,
        attached: state.lease ? 1 : 0,
        activity: 1_700_000_007,
        authority: AUTHORITY,
        unified: state.sessionState === "adoptable" && !state.adopted ? { state: "adoptable" } : { state: "open", origin: "birth" },
      });
    }
    if (state.switchSessions && state.sessionBState !== "missing") {
      sessions.push({
        handles: inventoryHandles("B"),
        realm: "remote", server: "private", server_status: "ok", session_id: "$8", name: "beta",
        width: state.geometryB.columns, height: state.geometryB.rows, attached: state.leaseB ? 1 : 0, activity: 1_700_000_008,
        authority: AUTHORITY_B,
        unified: state.sessionBState === "adoptable" && !state.adoptedB ? { state: "adoptable" } : state.sessionBState === "blocked_alt_screen" ? { state: "blocked_alt_screen" } : { state: "open", origin: "birth" },
      });
    }
    sessions.push(...(options.extraInventorySessions ?? []));
    const localSessions = sessions.filter((session) => session.realm === "local");
    const remoteSessions = sessions.filter((session) => session.realm === "remote");
    const realms = [{ name: "local", display_name: "Local realm", servers: [{ label: "private", status: "ok", can_create: false, can_stage_images: state.imageStaging, sessions: localSessions }] }];
    if (remoteSessions.length > 0) realms.push({ name: "remote", display_name: "Remote realm", servers: [{ label: "private", status: "ok", can_create: false, can_stage_images: state.imageStaging, sessions: remoteSessions }] });
    const aliases = state.terminal_touchSwitcherMetadata ? [
      { alias_id: "alias-alpha", display_alias: "primary shell", revision: 1, state: "active", session_incarnation: AUTHORITY },
      { alias_id: "alias-beta", display_alias: "support shell", revision: 1, state: "active", session_incarnation: AUTHORITY_B },
    ] : [];
    return { image_upload: state.imageStaging, realms, aliases };
  };
  const snapshot = () => ({
    counters: { ...state.counters },
    snippets: snippets.stats(),
    preferences: { ...state.preferences },
    preferenceOperations: state.preferenceOperations.map((operation) => ({ ...operation })),
    pendingPreferencePuts: state.preferencePutReleases.length,
    keyboardPreferences: structuredClone(state.keyboardPreferences),
    keyboardPreferenceOperations: structuredClone(state.keyboardPreferenceOperations),
    pendingKeyboardPreferencePuts: state.keyboardPreferencePutReleases.length,
    leaseHeld: state.lease !== null,
    leaseAttachment: state.lease ? state.lease.attachment : null,
    leaseBAttachment: state.leaseB ? state.leaseB.attachment : null,
    handlesMinted: state.handles.size,
    handlesConsumed: [...state.handles.values()].filter((entry) => entry.consumed).length,
    handles: [...state.handles.entries()].map(([handle, entry]) => ({ handle, purpose: entry.purpose, consumed: entry.consumed, session: entry.session })),
    offers: [...state.offers.entries()].map(([handle, entry]) => ({ handle, used: entry.used, session: entry.session })),
    handleLedger: state.handleLedger.map((entry) => ({ ...entry })),
    inventoryLedger: state.inventoryLedger.map((entry) => ({ ...entry })),
    takeoverClaims: state.takeoverClaims.map((entry) => ({ ...entry })),
    refitOperations: state.refitOperations.map((entry) => ({ ...entry })),
    attachments: state.attachments.map((attachment) => ({
      id: attachment.id,
      handlePurpose: attachment.handlePurpose,
      takeover: attachment.takeover,
      engine: attachment.engine,
      mode: attachment.mode,
      frames: attachment.frames.slice(),
      inputs: attachment.inputs.slice(),
      closeReason: attachment.closeReason,
      live: attachment.socket ? !attachment.socket.closed : false,
      session: attachment.session,
      offeredHandle: attachment.offeredHandle,
      resizes: attachment.resizes || 0,
	  resizeRequests: (attachment.resizeRequests || []).map((request) => ({ ...request })),
      refusals: attachment.refusals || 0,
    })),
  });

  const handler = async (request, response) => {
    const url = new URL(request.url, "http://localhost");
    response.setHeader("Cache-Control", "no-store");
    // Optional clipboard/image double; other fixture consumers keep their
    // original routing and state. It also witnesses shared clipboard headers.
    if (options.clipboardHTTP && await options.clipboardHTTP(request, response, url)) return;
    if (url.pathname === "/__fixture/control") {
      if (request.method === "POST") {
        const input = (await readJSON(request)) || {};
        if (input.reset) reset();
        if (typeof input.sessionState === "string") state.sessionState = input.sessionState;
        if (typeof input.switchSessions === "boolean") state.switchSessions = input.switchSessions;
        if (typeof input.terminal_touchSwitcherMetadata === "boolean") state.terminal_touchSwitcherMetadata = input.terminal_touchSwitcherMetadata;
        if (typeof input.sessionBState === "string") state.sessionBState = input.sessionBState;
        if (typeof input.adoptedB === "boolean") state.adoptedB = input.adoptedB;
        if (typeof input.holdAdoptionMs === "number") state.holdAdoptionMs = input.holdAdoptionMs;
        if (typeof input.holdHandleMs === "number") state.holdHandleMs = input.holdHandleMs;
        if (typeof input.holdInventoryMs === "number") state.holdInventoryMs = input.holdInventoryMs;
        if (typeof input.holdInventoryAfter === "number") state.holdInventoryAfter = input.holdInventoryAfter;
        if (typeof input.holdTakeoverMs === "number") state.holdTakeoverMs = input.holdTakeoverMs;
        if (typeof input.holdLeaseRefusalMs === "number") state.holdLeaseRefusalMs = input.holdLeaseRefusalMs;
        if (typeof input.failSessionBConnections === "number") state.failSessionBConnections = input.failSessionBConnections;
        if (typeof input.adopted === "boolean") state.adopted = input.adopted;
        if (typeof input.imageStaging === "boolean") state.imageStaging = input.imageStaging;
        if (input.preferences && typeof input.preferences === "object") state.preferences = { ...state.preferences, ...input.preferences };
        if (typeof input.preferencesAvailable === "boolean") state.preferences.available = input.preferencesAvailable;
        if (typeof input.preferencesCorrupt === "boolean") state.preferencesCorrupt = input.preferencesCorrupt;
        if (typeof input.holdPreferencePuts === "number") state.holdPreferencePuts = Math.max(0, Math.floor(input.holdPreferencePuts));
        if (input.releasePreferencePut) state.preferencePutReleases.shift()?.();
        if (input.keyboardPreferences && typeof input.keyboardPreferences === "object") state.keyboardPreferences = { ...state.keyboardPreferences, ...structuredClone(input.keyboardPreferences) };
        if (typeof input.keyboardPreferencesAvailable === "boolean") state.keyboardPreferences.available = input.keyboardPreferencesAvailable;
        if (typeof input.keyboardPreferencesCorrupt === "boolean") state.keyboardPreferencesCorrupt = input.keyboardPreferencesCorrupt;
        if (typeof input.holdKeyboardPreferencePuts === "number") state.holdKeyboardPreferencePuts = Math.max(0, Math.floor(input.holdKeyboardPreferencePuts));
        if (input.releaseKeyboardPreferencePut) state.keyboardPreferencePutReleases.shift()?.();
        if (typeof input.replayFocusReporting === "boolean") state.replayFocusReporting = input.replayFocusReporting;
        if (typeof input.refuseInputs === "number") state.refuseInputs = input.refuseInputs;
        if (typeof input.refuseInputsInBand === "number") state.refuseInputsInBand = input.refuseInputsInBand;
        if (typeof input.refuseResize === "string") state.refuseResize = input.refuseResize;
        if (typeof input.closeResize === "string") state.closeResize = input.closeResize;
        if (typeof input.refuseRefit === "string") state.refuseRefit = input.refuseRefit;
		if (typeof input.holdRefitMs === "number") state.holdRefitMs = input.holdRefitMs;
		if (typeof input.refitPrepareMismatch === "boolean") state.refitPrepareMismatch = input.refitPrepareMismatch;
        if (typeof input.holdModeGrant === "boolean") state.holdModeGrant = input.holdModeGrant;
        if (typeof input.holdPrepareMs === "number") state.holdPrepareMs = input.holdPrepareMs;
        if (typeof input.holdResizeMs === "number") state.holdResizeMs = input.holdResizeMs;
        if (typeof input.writeLive === "string") {
          // Live output on every open attachment (e.g. DECSET 1 to flip the
          // cursor-key mode xterm evaluates key-bar arrows against).
          for (const attachment of state.attachments) if (attachment.writeLive) attachment.writeLive(input.writeLive);
        }
        if (typeof input.reprepareSession === "string") {
          for (const attachment of state.attachments) {
            if (attachment.session === input.reprepareSession && attachment.sendPrepare && attachment.socket && !attachment.socket.closed) attachment.sendPrepare();
          }
        }
        if (typeof input.recommitSession === "string") {
          for (const attachment of state.attachments) {
            if (attachment.session === input.recommitSession && attachment.sendCommit && attachment.socket && !attachment.socket.closed) attachment.sendCommit();
          }
        }
        if (input.releaseMode) {
          state.holdModeGrant = false;
          for (const attachment of state.attachments) {
            if (attachment.pendingMode && attachment.socket && !attachment.socket.closed) {
              const grant = attachment.pendingMode;
              attachment.pendingMode = null;
              grant();
            }
          }
        }
        snippets.control(input);
        if (typeof input.bindingsExpired === "boolean") state.bindingsExpired = input.bindingsExpired;
        if (input.releaseLease) state.lease = null;
        if (input.releaseLeaseB) state.leaseB = null;
        if (input.holdLeaseB && !state.leaseB) {
          // A real foreign Control holder, not a boolean shortcut: the first
          // B socket must take the production lease_held -> takeover path and
          // the takeover socket must displace this exact live attachment.
          const foreign = {
            id: state.attachments.length + 1,
            session: "B",
            handlePurpose: "foreign-control",
            takeover: false,
            engine: "unified-dev",
            mode: "CONTROL",
            frames: [], inputs: [], closeReason: null, holdsLease: true,
            socket: null,
          };
          foreign.socket = {
            closed: false,
            close(_code, reason) { this.closed = true; foreign.closeReason = reason; },
          };
          state.attachments.push(foreign);
          state.leaseB = { attachment: foreign.id };
        }
        if (input.closeLive) {
          for (const attachment of state.attachments) {
            if (attachment.socket && !attachment.socket.closed) attachment.socket.close(1011, String(input.closeLive));
          }
        }
        if (input.closeSession && typeof input.closeSession.reason === "string") {
          for (const attachment of state.attachments) {
            if (attachment.session === input.closeSession.session && attachment.socket && !attachment.socket.closed) {
              attachment.socket.close(1011, input.closeSession.reason);
            }
          }
        }
      }
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify(snapshot()));
      return;
    }
    // session memory. The dashboard document reads the operator's
    // default-session preference exactly once per page, so a fixture that does
    // not answer this makes the page log a 404 the product never produces.
    // Mirrors `getPreferences` / `writePreferences` in
    // `internal/frontdoor/preferences_api.go` for an operator with nothing
    // stored: the `defaultPreferences()` record (version 1, theme "default",
    // font size 14, no default session), revision 0, `stored: false`, plus the
    // `available` flag of the preferencesResponse wrapper, under the same
    // Cache-Control/Content-Type/ETag headers. preferences adds the corresponding
    // exact strong-CAS mutation path below.
    if (url.pathname === "/api/dashboard-preferences") {
      const origin = `${tls ? "https" : "http"}://${request.headers.host}`;
      const send = status => {
        response.setHeader("Content-Type", "application/json");
        response.setHeader("ETag", `"${state.dashboardPreferences.revision}"`);
        response.writeHead(status); response.end(JSON.stringify(state.dashboardPreferences));
      };
      if (request.method === "GET") { send(200); return; }
      if (request.method !== "PUT") { response.writeHead(405); response.end(); return; }
      if (request.headers.origin !== origin || request.headers["x-persea-csrf"] !== CSRF_TOKEN || cookieCSRF(request) !== CSRF_TOKEN) { response.writeHead(403); response.end(); return; }
      const raw = await readBoundedBody(request, 320 << 10);
      if (raw.tooLarge) { response.writeHead(413); response.end(); return; }
      let body;
      try { body = JSON.parse(raw.text); } catch { response.writeHead(400); response.end(); return; }
      if (!body || Object.keys(body).sort().join(",") !== "favorites,version" || body.version !== 1 || !Array.isArray(body.favorites) || body.favorites.length > 128 || body.favorites.some(scope => typeof scope !== "string" || scope.length > 2048)) { response.writeHead(400); response.end(); return; }
      if (request.headers["if-match"] !== `"${state.dashboardPreferences.revision}"`) { send(412); return; }
      state.dashboardPreferences = { ...body, revision: state.dashboardPreferences.revision + 1, available: true };
      send(200); return;
    }
    if (request.method === "GET" && url.pathname === "/api/preferences") {
      state.counters.preferencesGet += 1;
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("Content-Type", "application/json");
      response.setHeader("ETag", `"${state.preferences.revision}"`);
      response.end(state.preferencesCorrupt ? '{"version":1,"theme":"unknown"}\n' : `${JSON.stringify(state.preferences)}\n`);
      return;
    }
    if (request.method === "PUT" && url.pathname === "/api/preferences") {
      state.counters.preferencesPut += 1;
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("Content-Type", "application/json");
      const body = await readJSON(request);
      const operation = {
        theme: body?.theme ?? null,
        font_size: body?.font_size ?? null,
        composer_font_size: body?.composer_font_size ?? null,
        if_match: request.headers["if-match"] ?? null,
        outcome: "pending",
      };
      state.preferenceOperations.push(operation);
      if (state.holdPreferencePuts > 0) {
        state.holdPreferencePuts -= 1;
        await new Promise((resolve) => state.preferencePutReleases.push(resolve));
      }
      if (!state.preferences.available) {
        operation.outcome = "unavailable";
        response.writeHead(503); response.end("preferences store unavailable\n"); return;
      }
      if (request.headers["if-match"] !== `"${state.preferences.revision}"`) {
        operation.outcome = "conflict";
        response.setHeader("ETag", `"${state.preferences.revision}"`);
        response.writeHead(412);
        response.end(`${JSON.stringify(state.preferences)}\n`);
        return;
      }
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || body.version !== 1) {
        operation.outcome = "refused";
        response.writeHead(403); response.end("forbidden\n"); return;
      }
      state.preferences = {
        version: 1, theme: body.theme, font_size: body.font_size,
        // The front door's own rule: a plain number with a default, so an
        // absent key is the default rather than a refusal.
        composer_font_size: body.composer_font_size ?? 11,
        default_session: body.default_session,
        revision: state.preferences.revision + 1, stored: true, available: true,
      };
      operation.outcome = "saved";
      response.setHeader("ETag", `"${state.preferences.revision}"`);
      response.end(`${JSON.stringify(state.preferences)}\n`);
      return;
    }
    if (url.pathname === "/api/keyboard-preferences") {
      const origin = `${tls ? "https" : "http"}://${request.headers.host}`;
      const fail = (status, text) => { response.writeHead(status); response.end(`${text}\n`); };
      const send = (status) => {
        response.setHeader("Content-Type", "application/json");
        response.setHeader("ETag", `"${state.keyboardPreferences.revision}"`);
        response.writeHead(status);
        response.end(state.keyboardPreferencesCorrupt ? '{"version":1}\n' : `${JSON.stringify(state.keyboardPreferences)}\n`);
      };
      if (request.headers.origin && request.headers.origin !== origin) { fail(403, "forbidden"); return; }
      if (request.method !== "GET" && request.method !== "PUT") { response.setHeader("Allow", "GET, PUT"); fail(405, "method not allowed"); return; }
      if (url.search) { fail(400, "invalid keyboard preferences request"); return; }
      if (request.method === "GET") { state.counters.keyboardPreferencesGet += 1; send(200); return; }
      state.counters.keyboardPreferencesPut += 1;
      if (request.headers.origin !== origin || request.headers["sec-fetch-site"] !== "same-origin" || request.headers["x-persea-csrf"] !== CSRF_TOKEN || cookieCSRF(request) !== CSRF_TOKEN) { fail(403, "forbidden"); return; }
      if (request.headers["content-type"] !== "application/json") { fail(400, "invalid keyboard preferences request"); return; }
      const ifMatch = request.headers["if-match"];
      if (typeof ifMatch !== "string" || !/^"(0|[1-9][0-9]*)"$/.test(ifMatch)) { send(412); return; }
      const raw = await readBoundedBody(request, 32 << 10);
      if (raw.tooLarge) { fail(413, "keyboard preferences request too large"); return; }
      let body;
      try { body = JSON.parse(raw.text); } catch { fail(400, "invalid keyboard preferences request"); return; }
      if (!validKeyboardValue(body)) { fail(400, "invalid keyboard preferences request"); return; }
      const operation = { preferences: structuredClone(body), if_match: ifMatch, outcome: "pending" };
      state.keyboardPreferenceOperations.push(operation);
      if (state.holdKeyboardPreferencePuts > 0) {
        state.holdKeyboardPreferencePuts -= 1;
        await new Promise((resolve) => state.keyboardPreferencePutReleases.push(resolve));
      }
      if (!state.keyboardPreferences.available) { operation.outcome = "unavailable"; fail(503, "keyboard preferences store unavailable"); return; }
      if (ifMatch !== `"${state.keyboardPreferences.revision}"`) { operation.outcome = "conflict"; send(412); return; }
      state.keyboardPreferences = { ...structuredClone(body), revision: state.keyboardPreferences.revision + 1, stored: true, available: true };
      operation.outcome = "saved";
      send(200); return;
    }
    if (url.pathname === "/api/clipboard/preferences") {
      await snippets.preferences(request,response,url,`${request.socket.encrypted ? "https" : "http"}://${request.headers.host}`);return;
    }
    if (url.pathname === "/api/snippets" || url.pathname.startsWith("/api/snippets/")) {
      await snippets.handle(request, response, url, `${request.socket.encrypted ? "https" : "http"}://${request.headers.host}`);
      return;
    }
    // Text and focus gates open the shared Clipboard. Image transfer has its
    // own fixture; model the empty production collection without a stray 404.
    if (request.method === "GET" && url.pathname === "/api/clipboard/images" && !url.search) {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("X-Content-Type-Options", "nosniff");
      response.end(JSON.stringify({ items: [] }));
      return;
    }
    // Every dashboard load includes the durable-workspace inventory. This
    // reopen fixture owns no saved workspaces, so model the production API's
    // empty successful result instead of leaking a test-only 404 to Chromium.
    if (request.method === "GET" && url.pathname === "/api/workspaces") {
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("Content-Type", "application/json");
      response.end("[]\n");
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/session-previews") {
      // A thumbnail read must not call inventory(), which would mint handles.
      const sessions = [
        ...(state.sessionState !== "missing" ? [{ realm: "local", server: "private", session_id: "$7", width: state.geometryA.columns, height: state.geometryA.rows }] : []),
        ...(state.switchSessions && state.sessionBState !== "missing" ? [{ realm: "remote", server: "private", session_id: "$8", width: state.geometryB.columns, height: state.geometryB.rows }] : []),
        ...(options.extraInventorySessions ?? []),
      ];
      const session = sessions.find(item => item.realm === url.searchParams.get("realm") && item.server === url.searchParams.get("server") && item.session_id === url.searchParams.get("session_id"));
      if (!session) { response.writeHead(404); response.end("unknown preview session"); return; }
      response.setHeader("Cache-Control", "no-store"); response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ realm: session.realm, server: session.server, session_id: session.session_id,
        rows: ["Fixture session preview."], width: session.width, height: session.height, captured_at: Date.now(), truncated: false }));
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/inventory") {
      state.counters.inventory += 1;
      if (state.holdInventoryMs > 0) {
        if (state.holdInventoryAfter > 0) {
          state.holdInventoryAfter -= 1;
        } else {
          const hold = state.holdInventoryMs;
          state.holdInventoryMs = 0;
          await new Promise((done) => setTimeout(done, hold));
          if (response.destroyed) return;
        }
      }
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(JSON.stringify(inventory()));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/attachment-handles") {
      state.counters.handleRequests += 1;
      const body = await readJSON(request);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || (body.purpose !== "control" && body.purpose !== "observe")) {
        response.writeHead(400); response.end("invalid attachment-handle request"); return;
      }
      if (state.bindingsExpired || !state.bindings.has(body.source)) {
        response.writeHead(410); response.end("attachment source is unavailable"); return;
      }
      if (state.holdHandleMs > 0) {
        const hold = state.holdHandleMs;
        state.holdHandleMs = 0;
        await new Promise((done) => setTimeout(done, hold));
        if (response.destroyed) return;
      }
      const session = state.bindingSessions.get(body.source) || "A";
      const handle = mint(body.purpose, session);
      state.handleLedger.push({ source: body.source, purpose: body.purpose, session, handle });
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ handle }));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/control-takeovers") {
      state.counters.takeovers += 1;
      const body = await readJSON(request);
      const code = (status, value) => { response.setHeader("Content-Type", "application/json"); response.writeHead(status); response.end(JSON.stringify({ code: value })); };
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || typeof body.request_id !== "string" || (!body.offer) === (!body.source)) {
        code(400, "invalid_takeover_request"); return;
      }
      state.takeoverClaims.push(body.offer ? { kind: "offer", value: body.offer } : { kind: "source", value: body.source });
      let takeoverSession = "A";
      if (body.offer) {
        const offer = state.offers.get(body.offer);
        if (!offer || offer.used) { code(410, "takeover_unavailable"); return; }
        offer.used = true;
        takeoverSession = offer.session || "A";
      } else if (state.bindingsExpired || !state.bindings.has(body.source)) {
        code(410, "takeover_unavailable"); return;
      } else {
        takeoverSession = state.bindingSessions.get(body.source) || "A";
      }
      const handle = mint("control-takeover", takeoverSession);
      if (state.holdTakeoverMs > 0) {
        const hold = state.holdTakeoverMs;
        state.holdTakeoverMs = 0;
        await new Promise((done) => setTimeout(done, hold));
        if (response.destroyed) return;
      }
      response.setHeader("Content-Type", "application/json");
      response.writeHead(201);
      response.end(JSON.stringify({ handle }));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/session-adoptions") {
      state.counters.adoptions += 1;
      const body = await readJSON(request);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || (body.session_id !== "$7" && body.session_id !== "$8")) {
        response.writeHead(400); response.end("invalid"); return;
      }
      if (state.holdAdoptionMs > 0) {
        const hold = state.holdAdoptionMs;
        state.holdAdoptionMs = 0;
        await new Promise((done) => setTimeout(done, hold));
      }
      if (body.session_id === "$8" && state.sessionBState.startsWith("blocked_")) {
        response.writeHead(409);
        response.end(state.sessionBState);
        return;
      }
      if (body.session_id === "$8") state.adoptedB = true;
      else state.adopted = true;
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify(body));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/session-refits") {
      const body = await readJSON(request);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || !state.bindings.has(body.source) || !Number.isInteger(body.columns) || body.columns < 20 || body.columns > 300 || !/^[A-Za-z0-9_-]{43}$/.test(body.operation || "")) {
        response.writeHead(400); response.end("bad_refit"); return;
      }
      // rows are optional; when present they ride the refit.
      if (body.rows !== undefined && (!Number.isInteger(body.rows) || body.rows < 1 || body.rows > 500)) {
        response.writeHead(400); response.end("bad_refit"); return;
      }
      state.counters.refits += 1;
      const session = state.bindingSessions.get(body.source) || "A";
	  const refitOperation = { source: body.source, columns: body.columns, rows: body.rows ?? null, operation: body.operation, session, successor: null };
	  state.refitOperations.push(refitOperation);
      if (state.holdRefitMs > 0) {
        const hold = state.holdRefitMs;
        state.holdRefitMs = 0;
        await new Promise((done) => setTimeout(done, hold));
      }
      if (state.refuseRefit) {
        const code = state.refuseRefit;
        state.refuseRefit = "";
        response.writeHead(code === "blocked_alt_screen" || code === "refit_in_progress" ? 409 : 503);
        response.end(code);
        return;
      }
      const geometry = session === "B" ? state.geometryB : state.geometryA;
      geometry.columns = body.columns;
      if (body.rows !== undefined) geometry.rows = body.rows;
	  const responseBeforeClose = state.refitPrepareMismatch;
	  const successorSource = token();
	  refitOperation.successor = successorSource;
	  const deliveredSource = state.refitPrepareMismatch ? token() : successorSource;
	  state.refitPrepareMismatch = false;
	  refitOperation.delivered = deliveredSource;
	  if (session === "B") state.sourceB = deliveredSource;
	  else state.sourceA = deliveredSource;
	  state.bindings.add(successorSource);
	  state.bindingSessions.set(successorSource, session);
	  state.bindings.add(deliveredSource);
	  state.bindingSessions.set(deliveredSource, session);
	  if (responseBeforeClose) {
		response.setHeader("Content-Type", "application/json");
		response.end(JSON.stringify({ operation: body.operation, columns: body.columns, rows: geometry.rows, successor_source: successorSource }));
		await new Promise((done) => setTimeout(done, 50));
	  }
      const lease = session === "B" ? state.leaseB : state.lease;
      if (lease) {
        const attachment = state.attachments[lease.attachment - 1];
        if (attachment && attachment.socket && !attachment.socket.closed) {
          attachment.closeReason = "generation_refit";
          attachment.socket.close(1011, "generation_refit");
        }
      }
	  if (!responseBeforeClose) {
		await new Promise((done) => setTimeout(done, 1_500));
		response.setHeader("Content-Type", "application/json");
		response.end(JSON.stringify({ operation: body.operation, columns: body.columns, rows: geometry.rows, successor_source: successorSource }));
	  }
      return;
    }
    if (request.method === "GET" && url.pathname === "/favicon.ico") {
      response.writeHead(204); response.end(); return;
    }
    if (request.method === "GET" && (url.pathname === "/" || url.pathname === "/terminal")) {
      state.counters.documents += 1;
      response.setHeader("Content-Type", "text/html");
      response.setHeader("Content-Security-Policy", url.pathname === "/terminal" && url.searchParams.getAll("engine").length === 1 && url.searchParams.get("engine") === "unified-dev"
        ? `${documentCSP}; style-src-attr 'unsafe-inline'`
        : documentCSP);
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(index);
      return;
    }
    const file = files[url.pathname];
    if (!file || request.method !== "GET") { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", file.type);
    response.setHeader("Content-Security-Policy", CSP);
    response.end(fs.readFileSync(file.file));
  };
  const tls = options.tls ? hermeticTLS() : null;
  const server = tls ? https.createServer({ key: tls.key, cert: tls.cert }, handler) : http.createServer(handler);

  // --- the attachment: front-door capability checks, then the broker automaton
  server.on("upgrade", (request, raw) => {
    state.counters.websockets += 1;
    const url = new URL(request.url, "http://localhost");
    const refuse = (status, text) => {
      raw.write(`HTTP/1.1 ${status} ${text}\r\nContent-Type: text/plain\r\nConnection: close\r\nContent-Length: ${Buffer.byteLength(text)}\r\n\r\n${text}`);
      raw.destroy();
    };
    if (url.pathname !== "/ws" || url.search !== "") { refuse(400, "invalid mode"); return; }
    const offered = subprotocols(request);
    if (!offered.values.includes("persea-terminal.v1") || !offered.handle || (offered.mode !== "control" && offered.mode !== "observe")) {
      refuse(400, "invalid mode"); return;
    }
    if (offered.csrf !== CSRF_TOKEN || offered.csrf !== cookieCSRF(request)) { refuse(403, "csrf"); return; }
    const entry = state.handles.get(offered.handle);
    // The one-time capability: unknown, consumed, or purpose-mismatched handles
    // are a replay and answer 410 BEFORE any upgrade, exactly like the front.
    if (!entry || entry.consumed || (offered.takeover ? entry.purpose !== "control-takeover" : entry.purpose !== offered.mode)) {
      refuse(410, "stale snapshot"); return;
    }
    entry.consumed = true;
    const sessionKey = entry.session || "A";
    if (sessionKey === "B" && state.failSessionBConnections > 0) {
      state.failSessionBConnections -= 1;
      refuse(503, "candidate unavailable");
      return;
    }
    const currentLease = () => sessionKey === "B" ? state.leaseB : state.lease;
    const setLease = (value) => { if (sessionKey === "B") state.leaseB = value; else state.lease = value; };
    const accept = crypto.createHash("sha1").update(request.headers["sec-websocket-key"] + WS_GUID).digest("base64");
    raw.write("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
      + `Sec-WebSocket-Accept: ${accept}\r\nSec-WebSocket-Protocol: persea-terminal.v1\r\n\r\n`);
    const attachment = {
      id: state.attachments.length + 1,
      session: sessionKey,
      handlePurpose: entry.purpose,
      takeover: offered.takeover,
      engine: offered.engine || "",
      mode: "OBSERVE",
      frames: [],
      inputs: [],
	  resizeRequests: [],
      closeReason: null,
      socket: null,
      holdsLease: false,
      pendingMode: null,
      offeredHandle: offered.handle,
    };
    state.attachments.push(attachment);
    const socket = new Socket(raw, (text) => onText(text), (reason) => {
      attachment.closeReason = attachment.closeReason || reason;
      if (attachment.holdsLease && currentLease() && currentLease().attachment === attachment.id) setLease(null);
    });
    attachment.socket = socket;
    const closeWith = (code, reason) => { attachment.closeReason = reason; socket.close(code, reason); };

    if (offered.mode === "control") {
      if (currentLease() && !offered.takeover) {
        const holder = state.attachments[currentLease().attachment - 1];
        if (holder && holder.socket && !holder.socket.closed) {
          // Losing handle becomes a one-shot takeover offer (front door semantics).
          const refuseLease = () => {
            if (socket.closed) return;
            state.offers.set(offered.handle, { used: false, session: sessionKey });
            closeWith(1011, "lease_held");
          };
          if (state.holdLeaseRefusalMs > 0) {
            const hold = state.holdLeaseRefusalMs;
            state.holdLeaseRefusalMs = 0;
            setTimeout(refuseLease, hold);
          } else {
            refuseLease();
          }
          return;
        }
        setLease(null);
      }
      if (offered.takeover && currentLease()) {
        const holder = state.attachments[currentLease().attachment - 1];
        if (holder && holder.socket && !holder.socket.closed) {
          holder.closeReason = "control_displaced";
          holder.socket.close(1000, "control_displaced");
        }
      }
      setLease({ attachment: attachment.id });
      attachment.holdsLease = true;
    }
    const attachmentState = sessionKey === "B" ? state.sessionBState : state.sessionState;
    const attachmentAdopted = sessionKey === "B" ? state.adoptedB : state.adopted;
    if (attachment.engine === "unified-dev" && attachmentState === "adoptable" && !attachmentAdopted) {
      closeWith(1011, "unified_unavailable");
      return;
    }
    if (attachmentState === "missing") {
      closeWith(1011, "stale_target");
      return;
    }

    // Broker automaton. Source binding is established at PREPARE, as the front does.
	const attachmentSource = sessionKey === "B" ? state.sourceB : state.sourceA;
    state.bindings.add(attachmentSource);
    state.bindingSessions.set(attachmentSource, sessionKey);
    const epoch = "17";
    const cut = "1";
    const frame = (value) => JSON.stringify({ version: 1, source: attachmentSource, epoch, ...value });
    state.counters.replays += 1;
    const replay = Buffer.from((state.replayFocusReporting ? "\x1b[?1004h" : "") + (options.initialReplay ?? (sessionKey === "B" ? "beta-replay\r\n" : "fixture-replay\r\n")), "binary").toString("base64");
    const geometry = sessionKey === "B" ? state.geometryB : state.geometryA;
    const sendPrepare = () => socket.sendText(frame({ type: "PREPARE", cut, kind: "INITIAL", columns: geometry.columns, rows: geometry.rows, history: [], truncated: false, replay }));
    attachment.sendPrepare = sendPrepare;
    attachment.sendCommit = () => socket.sendText(frame({ type: "COMMIT", cut }));
    if (state.holdPrepareMs > 0) {
      const hold = state.holdPrepareMs;
      state.holdPrepareMs = 0;
      setTimeout(() => { if (!socket.closed) sendPrepare(); }, hold);
    } else {
      sendPrepare();
    }
    let live = false;
    function onText(text) {
      if (text.startsWith(LIVENESS_PREFIX)) {
        const nonce = text.slice(LIVENESS_PREFIX.length + "PING ".length);
        socket.sendText(`${LIVENESS_PREFIX}PONG ${nonce}`);
        return;
      }
      let value;
      try { value = JSON.parse(text); } catch { closeWith(1011, "bad_attachment"); return; }
      attachment.frames.push(value.type === "INPUT" ? `INPUT:${JSON.stringify(Buffer.from(value.data, "base64").toString("binary"))}` : value.type);
      if (value.type === "READY") {
        if (live || value.cut !== cut) { closeWith(1011, "attachment_failed"); return; }
        live = true;
        socket.sendText(frame({ type: "COMMIT", cut }));
        socket.sendText(frame({ type: "LIVE", cut, data: Buffer.from("fixture-live\r\n", "binary").toString("base64") }));
        attachment.writeLive = (data) => {
          if (!socket.closed) socket.sendText(frame({ type: "LIVE", cut, data: Buffer.from(data, liveEncoding).toString("base64") }));
        };
        return;
      }
      if (!live) { closeWith(1011, "attachment_failed"); return; }
      if (value.type === "MODE_REQUEST") {
        if (offered.mode === "observe" && value.mode === "CONTROL") { closeWith(1011, "observe_mode"); return; }
        const grant = () => {
          if (socket.closed) return;
          attachment.mode = value.mode;
          socket.sendText(frame({ type: "MODE", mode: value.mode }));
        };
        // Holding the grant models the real window between COMMIT and the
        // control grant, during which any INPUT the page emits (a keystroke or
        // xterm's focus-in report) reaches the broker with the automaton still
        // in observe mode.
        if (state.holdModeGrant) { attachment.pendingMode = grant; return; }
        grant();
        return;
      }
      if (value.type === "INPUT") {
        // The real broker answers `error input_refused` for input outside the
        // control grant and means it as a per-frame refusal; the real front
        // door closes the socket with that reason. The browser sees the close.
        if (attachment.mode !== "CONTROL" || state.refuseInputs > 0) {
          if (state.refuseInputs > 0) state.refuseInputs -= 1;
          closeWith(1011, "input_refused");
          return;
        }
        if (state.refuseInputsInBand > 0) {
          // A current front door: the refusal is the frame's outcome, relayed
          // in-band; the frame is dropped and the socket stays open.
          state.refuseInputsInBand -= 1;
          attachment.refusals = (attachment.refusals || 0) + 1;
          socket.sendText(`${REFUSAL_PREFIX}input_refused`);
          return;
        }
        attachment.inputs.push(Buffer.from(value.data, "base64").toString("binary"));
        return;
      }
      if (value.type === "RESIZE_REQUEST") {
        if (attachment.mode !== "CONTROL") { closeWith(1011, "observe_mode"); return; }
        attachment.resizes = (attachment.resizes || 0) + 1;
		attachment.resizeRequests.push({ columns: value.columns, rows: value.rows });
        if (state.refuseResize) {
          attachment.refusals = (attachment.refusals || 0) + 1;
          socket.sendText(`${REFUSAL_PREFIX}${state.refuseResize}`);
          return;
        }
        if (state.closeResize) {
          // A Fit whose transaction failed after tmux was mutated: the broker
          // ends the attachment with a fatal verdict and the front door closes
          // the socket with it. One-shot, so the reopen that follows is live.
          const reason = state.closeResize;
          state.closeResize = "";
          closeWith(1011, reason);
          return;
        }
        // A committed geometry event on the ordered tail: same cut, new rows.
        const committedResize = frame({ type: "PREPARE", cut, kind: "RESIZE", columns: value.columns, rows: value.rows, history: [], truncated: false, replay: "" });
        if (state.holdResizeMs > 0) {
          // A slow tmux mutation: the page's Fit seal stays up until this lands.
          const hold = state.holdResizeMs;
          state.holdResizeMs = 0;
          setTimeout(() => { if (!socket.closed) socket.sendText(committedResize); }, hold);
          return;
        }
        socket.sendText(committedResize);
        return;
      }
      if (value.type === "HISTORY_REQUEST" || value.type === "DEFER") return;
      closeWith(1011, "bad_attachment");
    }
  });

  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const origin = `${tls ? "https" : "http"}://127.0.0.1:${server.address().port}`;
      resolve({
        origin,
        clipboardPreferences: snippets.clipboardPreferences,
        draftScope: DRAFT_SCOPE,
        draftScopeB: DRAFT_SCOPE_B,
		source: state.sourceA,
		sourceB: state.sourceB,
        // Keep-alive HTTP connections from the gate's own control requests
        // would otherwise hold server.close() open forever.
        close: () => new Promise((done) => {
          reset();
          if (server.closeAllConnections) server.closeAllConnections();
          server.close(() => { if (tls) fs.rmSync(tls.dir, { recursive: true, force: true }); done(); });
        }),
      });
    });
  });
}

module.exports = { startFixture, createSnippetStore, defaultKeyboardRecord, STYLE_NONCE, CSRF_TOKEN, DRAFT_SCOPE, DRAFT_SCOPE_B, SOURCE, SOURCE_B, Socket, subprotocols, cookieCSRF, readJSON, token, WS_GUID, LIVENESS_PREFIX, REFUSAL_PREFIX };
