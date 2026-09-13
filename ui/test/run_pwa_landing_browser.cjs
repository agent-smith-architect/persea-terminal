"use strict";

// E-P6 gate — Resume/default landing, PWA manifest, and the memory that feeds
// them (falsifiers EP6-F1 … EP6-F11; EP6-F12 is hardware and is reported NOT
// RUN with its manifest RED control executed here).
//
// The stack is an in-process front-door double: it serves the REAL built
// bundle, answers the real routes, and speaks the real attachment protocol over
// a hand-rolled RFC 6455 socket (the framing and the front-door capability
// checks are reused from unified_reopen_fixture.cjs, exactly as
// workspace_fixture.cjs reuses them). Everything the page could spend —
// documents, inventory, preferences, one-time handles, adoptions, takeovers,
// creations, WebSocket upgrades — is charged to one ledger, so "the landing
// spends nothing before a trusted tap" is a measurement, not a claim.
//
// Engines
//   default            Chromium over CDP on http://127.0.0.1 (the repo's
//                      default browser gate, so `npm run test:browser` runs it).
//   PERSEA_PWA_ENGINE=webkit
//                      WebKit through Playwright over hermetic TLS, because
//                      WebKit refuses a `__Host-` Secure cookie on http.
//                      Requires PERSEA_PLAYWRIGHT_MODULE (absolute) and, in
//                      this environment, PLAYWRIGHT_BROWSERS_PATH.
//
// Optional: PERSEA_EP6_EVIDENCE_DIR — screenshots and the JSON ledger.
// Mutant driving: PERSEA_EP6_ONLY=<item> narrows the run to one falsifier.

const crypto = require("crypto");
const fs = require("fs");
const http = require("http");
const https = require("https");
const os = require("os");
const path = require("path");
const childProcess = require("child_process");

const { Socket, subprotocols, cookieCSRF, readJSON, token, WS_GUID, LIVENESS_PREFIX, STYLE_NONCE, CSRF_TOKEN, defaultKeyboardRecord } = require("./unified_reopen_fixture.cjs");
const { assert, delay, freePort, requestJSON, CDP, launchChrome, stopChrome } = require("./unified_browser_lib.cjs");

const UI = path.resolve(__dirname, "..");
const ENGINE = process.env.PERSEA_PWA_ENGINE || "chromium";
const EVIDENCE = process.env.PERSEA_EP6_EVIDENCE_DIR ? path.resolve(process.env.PERSEA_EP6_EVIDENCE_DIR) : null;
const ONLY = process.env.PERSEA_EP6_ONLY || "";
const PHONE = Object.freeze({ width: 390, height: 844 });
const DESKTOP = Object.freeze({ width: 1280, height: 900 });
// The exact M5 sentence (UX-8 F2), asserted against its single source so a
// reworded product notice can never pass this gate by accident.
const PHONE_STATE_NOTICE = "workspace view is not available on this device yet";
assert(fs.readFileSync(path.join(UI, "src/workspace_posture.ts"), "utf8")
  .includes(`export const PHONE_STATE_NOTICE = ${JSON.stringify(PHONE_STATE_NOTICE)};`),
  "the M5 notice text drifted from src/workspace_posture.ts");
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'nonce-" + STYLE_NONCE
  + "'; connect-src 'self'; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'";
const SOURCE = "PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP";
const MEMORY_KEY = "persea-terminal.last-session.v1";
// One key per operation, not one key per origin (adjudication EP6-R2): the
// candidate a trusted action stages is addressed by the operation id that
// action minted, and only the navigation carrying that id may claim it.
const PENDING_PREFIX = "persea-terminal.pending-session.v1/";
const OP_A = "0123456789abcdef0123456789abcdef";
const OP_B = "fedcba9876543210fedcba9876543210";
// What a trusted dashboard action stages from the authoritative inventory
// immediately before it navigates (adjudication EP6-R1). Identity only: no
// handle, no URL, no token.
function stagedCandidate(session, alias) {
  return JSON.stringify({
    draftScope: session.scope, name: session.name,
    ...(alias ? { alias } : {}), realm: "local", server: "private", at: Date.now(),
  });
}
function stagedEntry(operationId, session, alias) {
  return [PENDING_PREFIX + operationId, stagedCandidate(session, alias)];
}

// Two incarnations of the SAME NAME on the same server. Only the session id and
// the creation time differ — which is exactly what an exact-incarnation resume
// must refuse to confuse (EP6-F2).
function authority(sessionID, created) {
  return { realm: "local", server: "private", uid: 1000, selector_kind: "socket_path", selector_value: "/tmp/private.sock", boot_id: "boot-ep6", server_pid: 42, server_start: 100, session_id: sessionID, session_created: created };
}
function scopeOf(sessionID, created) {
  return JSON.stringify(["local", "private", "socket_path", "/tmp/private.sock", "boot-ep6", sessionID, 1000, 42, 100, created]);
}
const REMEMBERED = Object.freeze({ sessionID: "$7", created: 200, name: "ops", scope: scopeOf("$7", 200) });
const SUCCESSOR = Object.freeze({ sessionID: "$21", created: 900, name: "ops", scope: scopeOf("$21", 900) });
const DEFAULT_SESSION = Object.freeze({ sessionID: "$9", created: 300, name: "build", scope: scopeOf("$9", 300) });

function memoryRecord(scope, name, at) {
  return JSON.stringify({ draftScope: scope, name, realm: "local", server: "private", at });
}

// --- The stack ---------------------------------------------------------------

function hermeticTLS() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "persea-ep6-tls-"));
  const key = path.join(dir, "key.pem");
  const cert = path.join(dir, "cert.pem");
  const result = childProcess.spawnSync("openssl", [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", cert,
    "-days", "1", "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  assert(result.status === 0, `hermetic TLS material could not be generated: ${String(result.stderr).slice(-400)}`);
  return { dir, key: fs.readFileSync(key), cert: fs.readFileSync(cert) };
}

async function startStack({ tls }) {
  const index = fs.readFileSync(path.join(UI, "dist/index.html"), "utf8").replace("__PERSEA_STYLE_NONCE__", STYLE_NONCE);
  const files = {
    "/app.js": { file: "dist/app.js", type: "text/javascript" },
    "/app.css": { file: "dist/app.css", type: "text/css" },
    "/xterm.css": { file: "dist/xterm.css", type: "text/css" },
    "/manifest.webmanifest": { file: "dist/manifest.webmanifest", type: "application/manifest+json" },
    "/icon-192.png": { file: "dist/icon-192.png", type: "image/png" },
    "/icon-512.png": { file: "dist/icon-512.png", type: "image/png" },
    "/apple-touch-icon.png": { file: "dist/apple-touch-icon.png", type: "image/png" },
  };

  const state = {
    // Which sessions the authoritative inventory reports, and in what unified
    // state. `remembered` is the exact incarnation the device memory pins.
    remembered: "open",          // open | adoptable | blocked | absent
    rememberedAttached: 0,       // >0 models "controlled on another device"
    successor: false,            // a SAME-NAME later incarnation is live
    duplicateRemembered: false,  // two identical scopes: the ambiguous case
    defaultSession: "absent",    // absent | open | adoptable | blocked
    preferenceDefault: null,     // the default_session preference body, or null
    preferencesStatus: 200,
    // Attachment behaviour for the EP6-F1 scenarios.
    attachmentFailure: "",       // "" | before_prepare | after_prepare | lease_held
    adoptionDelayMs: 0,          // hold the adoption response, so a refresh can arrive mid-flight
    adopted: false,
    // Saved workspace records the dashboard's Workspaces panel lists (UX-8 F2).
    workspaces: [],
    handles: new Map(),
    attachments: [],
    ledger: [],
  };
  const charge = (method, pathname) => { state.ledger.push(`${method} ${pathname}`); };
  const count = (needle) => state.ledger.filter((entry) => entry === needle).length;
  const mint = (purpose) => { const handle = token(); state.handles.set(handle, { purpose, consumed: false }); return handle; };

  const sessionRecord = (incarnation, name, unified, attached) => ({
    handles: { alias: mint("alias"), observe: mint("observe"), control: mint("control") },
    realm: "local", server: "private", server_status: "ok",
    session_id: incarnation.sessionID, name, width: 80, height: 24, attached, activity: 1_700_000_007,
    authority: authority(incarnation.sessionID, incarnation.created),
    ...(unified ? { unified } : {}),
  });
  const projection = (mode) => {
    if (mode === "open") return { state: "open", origin: "birth" };
    if (mode === "adoptable") return state.adopted ? { state: "open", origin: "birth" } : { state: "adoptable" };
    if (mode === "blocked") return { state: "blocked_alt_screen" };
    return undefined;
  };
  const inventory = () => {
    const sessions = [];
    if (state.remembered !== "absent") {
      sessions.push(sessionRecord(REMEMBERED, REMEMBERED.name, projection(state.remembered), state.rememberedAttached));
      if (state.duplicateRemembered) sessions.push(sessionRecord(REMEMBERED, REMEMBERED.name, projection(state.remembered), 0));
    }
    if (state.successor) sessions.push(sessionRecord(SUCCESSOR, SUCCESSOR.name, projection("open"), 0));
    if (state.defaultSession !== "absent") sessions.push(sessionRecord(DEFAULT_SESSION, DEFAULT_SESSION.name, projection(state.defaultSession), 0));
    return { image_upload: false, realms: [{ name: "local", display_name: "Local realm", servers: [{ label: "private", status: "ok", can_create: false, can_stage_images: false, sessions }] }], aliases: [] };
  };

  const handler = async (request, response) => {
    const url = new URL(request.url, "http://localhost");
    charge(request.method, url.pathname);
    response.setHeader("Cache-Control", "no-store");
    response.setHeader("X-Content-Type-Options", "nosniff");
    if (request.method === "GET" && url.pathname === "/api/inventory") {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(JSON.stringify(inventory()));
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/workspaces") {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(JSON.stringify({ version: 1, items: state.workspaces }));
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/keyboard-preferences") {
      response.setHeader("Content-Type", "application/json"); response.setHeader("ETag", '"0"');
      response.end(JSON.stringify(defaultKeyboardRecord())); return;
    }
    if (request.method === "GET" && url.pathname === "/api/dashboard-preferences") {
      response.setHeader("Content-Type", "application/json"); response.setHeader("ETag", '"0"');
      response.end(JSON.stringify({ version: 1, favorites: [], revision: 0, available: true })); return;
    }
    if (request.method === "GET" && url.pathname === "/api/session-previews") {
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({
        realm: "local", server: "private", session_id: url.searchParams.get("session_id"),
        rows: ["LANDING_PREVIEW_READ_ONLY"], width: 80, height: 24,
        captured_at: Date.now(), truncated: false,
      }));
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/preferences") {
      response.setHeader("Content-Type", "application/json");
      // A real record always carries a valid theme id and its strong ETag: the
      // dashboard now reads it through OperatorPreferencesService, which
      // validates the whole record.
      response.setHeader("ETag", '"1"');
      response.writeHead(state.preferencesStatus);
      response.end(JSON.stringify({ version: 1, theme: "default", font_size: 14, composer_font_size: 13, default_session: state.preferenceDefault, revision: 1, stored: true, available: true }));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/session-adoptions") {
      const body = (await readJSON(request)) || {};
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN) { response.writeHead(403); response.end("csrf"); return; }
      if (state.adoptionDelayMs > 0) await new Promise((resolve) => setTimeout(resolve, state.adoptionDelayMs));
      state.adopted = true;
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify(body));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/attachment-handles") {
      const body = (await readJSON(request)) || {};
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ handle: mint(body.purpose === "observe" ? "observe" : "control") }));
      return;
    }
    if (request.method === "GET" && url.pathname === "/favicon.ico") { response.writeHead(204); response.end(); return; }
    // A same-origin document with no script and no app boot. Seeding device
    // memory needs an origin to write into; loading the dashboard for that
    // would spend inventory the scenario has not asked for yet.
    if (request.method === "GET" && url.pathname === "/__fixture/blank") {
      response.setHeader("Content-Type", "text/html");
      response.setHeader("Content-Security-Policy", CSP);
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end("<!doctype html><title>fixture</title><body></body>");
      return;
    }
    if (request.method === "GET" && (url.pathname === "/" || url.pathname === "/terminal" || url.pathname === "/workspace")) {
      response.setHeader("Content-Type", "text/html");
      response.setHeader("Content-Security-Policy", CSP);
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(index);
      return;
    }
    const file = files[url.pathname];
    if (!file || request.method !== "GET") { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", file.type);
    response.setHeader("Content-Security-Policy", CSP);
    response.end(fs.readFileSync(path.join(UI, file.file)));
  };

  let material = null;
  let server;
  if (tls) {
    material = hermeticTLS();
    server = https.createServer({ key: material.key, cert: material.cert }, (request, response) => { void handler(request, response); });
  } else {
    server = http.createServer((request, response) => { void handler(request, response); });
  }

  // The attachment. Only the EP6-F1 scenarios reach it; every other scenario
  // asserts that it is never reached at all.
  server.on("upgrade", (request, raw) => {
    charge("WS", "/ws");
    const offered = subprotocols(request);
    const refuse = (status, text) => {
      raw.write(`HTTP/1.1 ${status} ${text}\r\nContent-Type: text/plain\r\nConnection: close\r\nContent-Length: ${Buffer.byteLength(text)}\r\n\r\n${text}`);
      raw.destroy();
    };
    if (offered.csrf !== CSRF_TOKEN || offered.csrf !== cookieCSRF(request)) { refuse(403, "csrf"); return; }
    const entry = state.handles.get(offered.handle);
    if (!entry || entry.consumed) { refuse(410, "stale snapshot"); return; }
    entry.consumed = true;
    const accept = crypto.createHash("sha1").update(request.headers["sec-websocket-key"] + WS_GUID).digest("base64");
    raw.write("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
      + `Sec-WebSocket-Accept: ${accept}\r\nSec-WebSocket-Protocol: persea-terminal.v1\r\n\r\n`);
    const attachment = { id: state.attachments.length + 1, frames: [], closeReason: null, socket: null };
    state.attachments.push(attachment);
    const socket = new Socket(raw, (text) => onText(text), (reason) => { attachment.closeReason = attachment.closeReason || reason; });
    attachment.socket = socket;
    const closeWith = (code, reason) => { attachment.closeReason = reason; socket.close(code, reason); };
    // Declared before every early return: the socket's data callback is armed
    // the moment the Socket exists, so a late frame must never meet a dead zone.
    let live = false;

    if (state.attachmentFailure === "before_prepare") { closeWith(1011, "stale_target"); return; }
    if (state.attachmentFailure === "lease_held") { closeWith(1011, "lease_held"); return; }
    const epoch = "17";
    const cut = "1";
    const frame = (value) => JSON.stringify({ version: 1, source: SOURCE, epoch, ...value });
    socket.sendText(frame({ type: "PREPARE", cut, kind: "INITIAL", columns: 80, rows: 24, history: [], truncated: false, replay: Buffer.from("ep6-replay\r\n", "binary").toString("base64") }));
    if (state.attachmentFailure === "after_prepare") setTimeout(() => { if (!socket.closed) closeWith(1011, "stale_target"); }, 500);
    function onText(text) {
      if (text.startsWith(LIVENESS_PREFIX)) {
        socket.sendText(`${LIVENESS_PREFIX}PONG ${text.slice(LIVENESS_PREFIX.length + "PING ".length)}`);
        return;
      }
      let value;
      try { value = JSON.parse(text); } catch { closeWith(1011, "bad_attachment"); return; }
      attachment.frames.push(value.type);
      if (value.type === "READY") {
        // The admission dies between PREPARE and COMMIT: the page has a live
        // transport and a replayed screen, but the session was never committed.
        if (state.attachmentFailure === "after_prepare") { closeWith(1011, "stale_target"); return; }
        if (live || value.cut !== cut) { closeWith(1011, "attachment_failed"); return; }
        live = true;
        socket.sendText(frame({ type: "COMMIT", cut }));
        return;
      }
      if (!live) { closeWith(1011, "attachment_failed"); return; }
      if (value.type === "MODE_REQUEST") { socket.sendText(frame({ type: "MODE", mode: value.mode })); return; }
    }
  });

  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = server.address().port;
  const origin = `${tls ? "https" : "http"}://127.0.0.1:${port}`;
  return {
    origin, state, count,
    ledgerSince: (mark) => state.ledger.slice(mark),
    mark: () => state.ledger.length,
    reset: () => {
      state.remembered = "open"; state.rememberedAttached = 0; state.successor = false; state.duplicateRemembered = false;
      state.defaultSession = "absent"; state.preferenceDefault = null; state.preferencesStatus = 200;
      state.attachmentFailure = ""; state.adoptionDelayMs = 0; state.adopted = false;
      state.workspaces.length = 0;
      state.handles.clear(); state.attachments.length = 0; state.ledger.length = 0;
    },
    stop: async () => {
      for (const attachment of state.attachments) if (attachment.socket && !attachment.socket.closed) attachment.socket.close(1000, "gate_end");
      // A page this gate did not close — a popup a mutant opened, a keep-alive
      // the browser is still holding — would keep server.close() pending for
      // ever, and a teardown that never returns turns a RED run into silence.
      // Connections are dropped, not waited on.
      server.closeAllConnections();
      await new Promise((resolve) => server.close(resolve));
      if (material) fs.rmSync(material.dir, { recursive: true, force: true });
    },
  };
}

// --- Page instrumentation ----------------------------------------------------
//
// Counters only: nothing is blocked, so the page behaves exactly as it ships.
// A landing that opened a popup, constructed a socket, or registered a service
// worker would be visible here even if it left no server-side trace.
const INIT_SCRIPT = `
(() => {
  const record = { opens: 0, sockets: 0, serviceWorkerRegistrations: 0, cspViolations: [], assigns: [] };
  Object.defineProperty(window, "__perseaEP6", { value: record, configurable: false, writable: false });
  const nativeOpen = window.open;
  window.open = function (...args) { record.opens += 1; return nativeOpen.apply(window, args); };
  const NativeWebSocket = window.WebSocket;
  function CountingWebSocket(...args) { record.sockets += 1; return new NativeWebSocket(...args); }
  CountingWebSocket.prototype = NativeWebSocket.prototype;
  for (const key of ["CONNECTING", "OPEN", "CLOSING", "CLOSED"]) CountingWebSocket[key] = NativeWebSocket[key];
  window.WebSocket = CountingWebSocket;
  if (navigator.serviceWorker && typeof navigator.serviceWorker.register === "function") {
    const nativeRegister = navigator.serviceWorker.register.bind(navigator.serviceWorker);
    navigator.serviceWorker.register = function (...args) { record.serviceWorkerRegistrations += 1; return nativeRegister(...args); };
  }
  document.addEventListener("securitypolicyviolation", (event) => {
    record.cspViolations.push({ directive: event.violatedDirective, blocked: String(event.blockedURI).slice(0, 160) });
  });
})();
`;

// What every scenario reads back off the page. Geometry is measured on the
// real rendered card, never on a class name.
const STATE_EXPRESSION = `(() => {
  const record = window.__perseaEP6 || { opens: 0, sockets: 0, serviceWorkerRegistrations: 0, cspViolations: [] };
  const landing = document.querySelector(".dashboard-landing");
  const action = landing ? landing.querySelector(".landing-action") : null;
  const box = action ? action.getBoundingClientRect() : null;
  const interactive = [...document.querySelectorAll("a[href], button, input, select, textarea")].filter((node) => {
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  });
  const first = interactive[0] || null;
  let memory = null;
  try { memory = window.localStorage.getItem(${JSON.stringify(MEMORY_KEY)}); } catch { memory = "<unreadable>"; }
  return {
    href: window.location.href,
    ready: document.readyState,
    landingHidden: landing ? landing.hidden : null,
    landingText: landing ? landing.textContent : "",
    kicker: landing ? [...landing.querySelectorAll(".landing-card-kicker")].map((node) => node.textContent) : [],
    label: landing ? [...landing.querySelectorAll(".landing-card-label")].map((node) => node.textContent) : [],
    notes: landing ? [...landing.querySelectorAll(".landing-note")].map((node) => node.textContent) : [],
    actionTag: action ? action.tagName : null,
    actionHref: action && action.tagName === "A" ? action.getAttribute("href") : null,
    actionAria: action ? action.getAttribute("aria-label") : null,
    actionDisabled: action ? Boolean(action.disabled) : null,
    actionBox: box ? { x: box.x, y: box.y, width: box.width, height: box.height } : null,
    interactiveCount: interactive.length,
    dedicatedLandingAction: Boolean(action && action.textContent.trim() === "Open" && action.closest(".landing-card-actions")),
    firstInteractiveClass: first ? first.className : "",
    activeElementTag: document.activeElement ? document.activeElement.tagName : null,
    activeElementClass: document.activeElement ? String(document.activeElement.className) : "",
    activeElementIsAction: Boolean(action && document.activeElement === action),
    textEntryFocused: Boolean(document.activeElement && ["INPUT", "TEXTAREA"].includes(document.activeElement.tagName)),
    filterValue: (document.querySelector(".dashboard-search-input") || { value: null }).value,
    sessionRows: document.querySelectorAll(".session-card").length,
    visibleSessionRows: [...document.querySelectorAll(".session-card")].filter((node) => node.getBoundingClientRect().height > 0).length,
    xtermScreens: document.querySelectorAll(".xterm-screen").length,
    opens: record.opens,
    sockets: record.sockets,
    serviceWorkerRegistrations: record.serviceWorkerRegistrations,
    cspViolations: record.cspViolations.slice(),
    viewport: { width: window.innerWidth, height: window.innerHeight },
    coarse: window.matchMedia("(pointer: coarse)").matches,
  };
})()`;

// --- Drivers -----------------------------------------------------------------

async function chromiumDriver(origin) {
  const handle = await launchChrome("persea-ep6-landing-");
  const pages = [];
  const open = async ({ viewport, coarse, screen }) => {
    const target = await requestJSON(`http://127.0.0.1:${handle.debugPort}/json/new?about:blank`, "PUT");
    assert(target && target.webSocketDebuggerUrl, "Chrome target unavailable");
    const cdp = new CDP(target.webSocketDebuggerUrl);
    await cdp.open();
    const consoleMessages = [];
    cdp.on("Runtime.consoleAPICalled", (params) => consoleMessages.push({ kind: params.type, text: params.args.map((a) => a.value ?? a.description ?? "").join(" ") }));
    cdp.on("Runtime.exceptionThrown", (params) => consoleMessages.push({ kind: "exception", text: params.exceptionDetails.exception?.description || params.exceptionDetails.text }));
    await cdp.send("Runtime.enable");
    await cdp.send("Page.enable");
    await cdp.send("Log.enable");
    // `screen` is the device witness the product reads for workspace posture
    // (workspace_posture.ts stablePostureEnvironment). It is emulated only when
    // a case asks for it, so a merely narrow desktop window stays a desktop.
    await cdp.send("Emulation.setDeviceMetricsOverride", { width: viewport.width, height: viewport.height, deviceScaleFactor: 1, mobile: Boolean(coarse), ...(screen ? { screenWidth: screen.width, screenHeight: screen.height } : {}) });
    await cdp.send("Emulation.setTouchEmulationEnabled", { enabled: Boolean(coarse), maxTouchPoints: coarse ? 5 : 1 });
    await cdp.send("Emulation.setEmulatedMedia", {
      features: coarse
        ? [{ name: "pointer", value: "coarse" }, { name: "any-pointer", value: "coarse" }, { name: "hover", value: "none" }]
        : [{ name: "pointer", value: "fine" }, { name: "any-pointer", value: "fine" }, { name: "hover", value: "hover" }],
    });
    await cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: INIT_SCRIPT });
    const evaluate = async (expression) => {
      const result = await cdp.send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
      if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text || "evaluate failed");
      return result.result.value;
    };
    const page = {
      consoleMessages,
      evaluate,
      state: () => evaluate(STATE_EXPRESSION),
      goto: async (url) => {
        await cdp.send("Page.navigate", { url });
        await settle(page, url);
      },
      reload: async () => { await cdp.send("Page.reload"); await delay(400); },
      seed: async (entries) => {
        await cdp.send("Page.navigate", { url: `${origin}/__fixture/blank` });
        await delay(160);
        await evaluate(`(() => { try { window.localStorage.clear(); ${entries.map(([key, value]) => `window.localStorage.setItem(${JSON.stringify(key)}, ${JSON.stringify(value)});`).join(" ")} return "ok"; } catch (error) { return String(error); } })()`);
      },
      clickAt: async ({ x, y }) => {
        await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x, y });
        await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", buttons: 1, clickCount: 1 });
        await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", buttons: 0, clickCount: 1 });
      },
      tapAt: async ({ x, y }) => {
        await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
        await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
      },
      touchDrag: async ({ x, y }, dy) => {
        await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
        for (let step = 1; step <= 6; step += 1) {
          await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x, y: y + (dy * step) / 6 }] });
          await delay(12);
        }
        await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
      },
      pressEnter: async () => {
        await cdp.send("Input.dispatchKeyEvent", { type: "rawKeyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13 });
        await cdp.send("Input.dispatchKeyEvent", { type: "char", text: "\r", key: "Enter" });
        await cdp.send("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13 });
      },
      setViewport: async (next) => {
        await cdp.send("Emulation.setDeviceMetricsOverride", { width: next.width, height: next.height, deviceScaleFactor: 1, mobile: Boolean(coarse) });
        await delay(120);
      },
      emitVisible: async () => { await evaluate(`document.dispatchEvent(new Event("visibilitychange"))`); await delay(120); },
      screenshot: async (file) => {
        const shot = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
        fs.writeFileSync(file, Buffer.from(shot.data, "base64"));
      },
      targetCount: async () => (await requestJSON(`http://127.0.0.1:${handle.debugPort}/json/list`)).filter((item) => item.type === "page").length,
      close: async () => { try { cdp.close(); } catch { /* gone */ } await requestJSON(`http://127.0.0.1:${handle.debugPort}/json/close/${target.id}`).catch(() => undefined); },
    };
    pages.push(page);
    return page;
  };
  return { name: "chromium", tls: false, open, close: async () => { for (const page of pages) await page.close().catch(() => undefined); await stopChrome(handle); } };
}

async function webkitDriver(origin) {
  const modulePath = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright");
  assert(modulePath && path.isAbsolute(modulePath), "PERSEA_PLAYWRIGHT_MODULE must be an absolute Playwright module path for the webkit engine");
  const playwright = require(modulePath);
  const browser = await playwright.webkit.launch({ headless: true });
  const contexts = [];
  const open = async ({ viewport, coarse, screen }) => {
    const context = await browser.newContext({ viewport, hasTouch: Boolean(coarse), isMobile: Boolean(coarse), deviceScaleFactor: 1, ignoreHTTPSErrors: true, ...(screen ? { screen } : {}) });
    contexts.push(context);
    await context.addInitScript(INIT_SCRIPT);
    const target = await context.newPage();
    const consoleMessages = [];
    target.on("console", (message) => consoleMessages.push({ kind: message.type(), text: message.text(), url: message.location().url }));
    target.on("response", response => { if (response.status() >= 400) consoleMessages.push({ kind: "http", status: response.status(), url: response.url() }); });
    target.on("pageerror", (error) => consoleMessages.push({ kind: "exception", text: String(error) }));
    const evaluate = (expression) => target.evaluate(expression);
    const page = {
      consoleMessages,
      evaluate,
      state: () => evaluate(STATE_EXPRESSION),
      goto: async (url) => { await target.goto(url, { waitUntil: "load" }).catch(() => undefined); await settle(page, url); },
      reload: async () => { await target.reload({ waitUntil: "load" }).catch(() => undefined); await delay(400); },
      seed: async (entries) => {
        await target.goto(`${origin}/__fixture/blank`, { waitUntil: "load" }).catch(() => undefined);
        await evaluate(`(() => { try { window.localStorage.clear(); ${entries.map(([key, value]) => `window.localStorage.setItem(${JSON.stringify(key)}, ${JSON.stringify(value)});`).join(" ")} return "ok"; } catch (error) { return String(error); } })()`);
      },
      clickAt: ({ x, y }) => target.mouse.click(x, y),
      tapAt: ({ x, y }) => target.touchscreen.tap(x, y),
      // Playwright's WebKit build exposes no trusted low-level touch stream, so
      // the drag here is a SYNTHETIC touch sequence. It is weaker evidence than
      // Chromium's trusted gesture and is labelled as such in the report; the
      // structural half of the same claim — the action binds only `click`, so
      // no pointer or touch phase can activate it — is asserted on both engines.
      touchDrag: async ({ x, y }, dy) => {
        await evaluate(`(() => {
          const node = document.elementFromPoint(${x}, ${y});
          if (!node) return "no-target";
          const touch = (clientY) => (typeof document.createTouch === "function"
            ? document.createTouch(window, node, 1, ${x}, clientY, ${x}, clientY)
            : new Touch({ identifier: 1, target: node, clientX: ${x}, clientY }));
          const list = (...items) => (typeof document.createTouchList === "function" ? document.createTouchList(...items) : items);
          const send = (type, clientY) => {
            const point = touch(clientY);
            const active = type === "touchend" ? list() : list(point);
            node.dispatchEvent(new TouchEvent(type, { bubbles: true, cancelable: true, touches: active, targetTouches: active, changedTouches: list(point) }));
          };
          send("touchstart", ${y});
          for (let step = 1; step <= 6; step += 1) send("touchmove", ${y} + (${dy} * step) / 6);
          send("touchend", ${y + dy});
          return "dispatched";
        })()`);
        await delay(160);
      },
      pressEnter: () => target.keyboard.press("Enter"),
      setViewport: async (next) => { await target.setViewportSize(next); await delay(120); },
      emitVisible: async () => { await evaluate(`document.dispatchEvent(new Event("visibilitychange"))`); await delay(120); },
      screenshot: (file) => target.screenshot({ path: file, fullPage: true }),
      targetCount: async () => context.pages().length,
      close: async () => { await context.close().catch(() => undefined); },
    };
    return page;
  };
  return { name: "webkit", tls: true, open, close: async () => { for (const context of contexts) await context.close().catch(() => undefined); await browser.close(); } };
}

async function settle(page, url) {
  const deadline = Date.now() + 12_000;
  while (Date.now() < deadline) {
    let current = null;
    try { current = await page.state(); } catch { /* navigation in flight */ }
    if (current && current.ready === "complete" && (url === undefined || current.href.startsWith(url.split("#")[0]))) {
      // The landing renders on the first parsed inventory; give that one turn.
      await delay(180);
      return;
    }
    await delay(40);
  }
}

// --- Ledger ------------------------------------------------------------------

const results = [];
let failures = 0;
// The last thing a scenario said it was doing. A stalled item names its step
// instead of leaving a bare timeout, and PERSEA_EP6_TRACE=1 streams every step.
let currentStep = "";
function step(name) {
  currentStep = name;
  if (process.env.PERSEA_EP6_TRACE === "1") process.stderr.write(`  [step] ${name}\n`);
}

// One item may not stall the whole gate. A scenario that never returns — a
// navigation that never lands, a page a mutant wedged — is recorded as that
// item's failure with a named cause instead of becoming silence.
const ITEM_TIMEOUT_MS = Number(process.env.PERSEA_EP6_ITEM_TIMEOUT_MS || 180_000);

async function item(name, description, run) {
  if (ONLY && ONLY !== name) return;
  const started = Date.now();
  currentStep = "";
  let stall;
  try {
    await Promise.race([
      run(),
      new Promise((_resolve, reject) => {
        stall = setTimeout(() => reject(new Error(`${name} did not complete within ${ITEM_TIMEOUT_MS} ms (last step: ${currentStep || "none"})`)), ITEM_TIMEOUT_MS);
        stall.unref?.();
      }),
    ]);
    results.push({ item: name, verdict: "PASS", description, ms: Date.now() - started });
    process.stdout.write(`ok ${name} — ${description}\n`);
  } catch (error) {
    clearTimeout(stall);
    failures += 1;
    results.push({ item: name, verdict: "FAIL", description, ms: Date.now() - started, error: String(error && error.message || error) });
    process.stdout.write(`not ok ${name} — ${description}\n    ${String(error && error.message || error)}\n`);
  } finally {
    clearTimeout(stall);
  }
}

// Visible desktop thumbnails read one snapshot without acquiring attachment or
// control authority. All authority-bearing routes remain forbidden before Open.
const AUTHORITY_ROUTES = ["POST /api/attachment-handles", "POST /api/session-adoptions", "POST /api/control-takeovers", "POST /api/sessions", "WS /ws"];

function assertNoAuthority(entries, label) {
  for (const route of AUTHORITY_ROUTES) {
    const spent = entries.filter((entry) => entry === route).length;
    assert(spent === 0, `${label}: the landing spent ${route} ${spent} time(s) before a trusted tap`);
  }
}

function assertQuiet(state, label) {
  assert(state.sockets === 0, `${label}: ${state.sockets} WebSocket(s) constructed before a trusted tap`);
  assert(state.opens === 0, `${label}: ${state.opens} popup(s) opened`);
  assert(state.serviceWorkerRegistrations === 0, `${label}: a service worker was registered`);
  assert(state.xtermScreens === 0, `${label}: a terminal renderer exists on the landing`);
  assert(state.cspViolations.length === 0, `${label}: CSP violations ${JSON.stringify(state.cspViolations)}`);
}

function consoleErrors(page) {
  return page.consoleMessages.filter((message) => message.kind === "error" || message.kind === "exception");
}

// --- The manifest contract (EP6-F8 schema half and the EP6-F12 RED control) ---

function manifestVerdict(text) {
  let value;
  try { value = JSON.parse(text); } catch { return "manifest is not JSON"; }
  if (value === null || typeof value !== "object" || Array.isArray(value)) return "manifest is not an object";
  if (value.start_url !== "/?resume=1") return `start_url is ${JSON.stringify(value.start_url)}`;
  if (value.display !== "standalone") return `display is ${JSON.stringify(value.display)}`;
  if (value.scope !== "/") return `scope is ${JSON.stringify(value.scope)}`;
  if (value.theme_color !== "#081018") return `theme_color is ${JSON.stringify(value.theme_color)}`;
  if ("serviceworker" in value) return "manifest declares a service worker";
  if (!Array.isArray(value.icons) || value.icons.length < 2) return "manifest does not declare two icons";
  const sizes = value.icons.map((icon) => icon && icon.sizes);
  for (const required of ["192x192", "512x512"]) if (!sizes.includes(required)) return `manifest has no ${required} icon`;
  for (const icon of value.icons) {
    if (typeof icon.src !== "string" || !icon.src.startsWith("/")) return "an icon src is not a same-origin path";
    if (icon.type !== "image/png") return "an icon is not a PNG";
  }
  return "ok";
}

// --- The gate ----------------------------------------------------------------

// A gate that hangs reports nothing, and nothing reads like success. The
// watchdog turns any hang — a stuck navigation, a browser that will not exit, a
// socket teardown that never settles — into a loud non-zero exit.
const WATCHDOG_MS = Number(process.env.PERSEA_EP6_WATCHDOG_MS || 900_000);

function armWatchdog() {
  const timer = setTimeout(() => {
    process.stderr.write(`\nE-P6 landing gate exceeded ${WATCHDOG_MS} ms; the run is incomplete and is reported as failed.\n`);
    process.stderr.write(`items completed: ${JSON.stringify(results.map((entry) => `${entry.item}:${entry.verdict}`))}\n`);
    process.exit(3);
  }, WATCHDOG_MS);
  timer.unref?.();
  return timer;
}

async function main() {
  assert(fs.existsSync(path.join(UI, "dist/app.js")), "run `npm run build` before this gate");
  if (EVIDENCE) fs.mkdirSync(EVIDENCE, { recursive: true });
  const watchdog = armWatchdog();
  const wantsTLS = ENGINE !== "chromium";
  const stack = await startStack({ tls: wantsTLS });
  const driver = ENGINE === "chromium" ? await chromiumDriver(stack.origin) : await webkitDriver(stack.origin);
  const shot = (name) => (EVIDENCE ? path.join(EVIDENCE, `${driver.name}_${name}.png`) : null);
  const at = (box) => ({ x: box.x + box.width / 2, y: box.y + Math.min(box.height / 2, 30) });

  try {
    // ---------------------------------------------------------------- EP6-F1
    await item("EP6-F1", "memory records the trusted action's identity, only after that operation's COMMIT", async () => {
      const page = await driver.open({ viewport: DESKTOP, coarse: false });
      const target = (scopeSource, extra = "", openId = OP_A, withScope = true) => {
        const handle = token();
        stack.state.handles.set(handle, { purpose: "control", consumed: false });
        const scope = withScope ? `&draft_scope=${encodeURIComponent(scopeSource.scope)}` : "";
        const open = openId === null ? "" : `&open_id=${openId}`;
        return { handle, href: `${stack.origin}/terminal?engine=unified-dev#handle=${handle}&mode=control&history=1000&name=${scopeSource.name}${extra}&engine=unified-dev${scope}${open}` };
      };
      const attach = async (scopeSource, seedEntries, extra = "", openId = OP_A, withScope = true) => {
        await page.seed(seedEntries);
        const spot = target(scopeSource, extra, openId, withScope);
        await page.goto(spot.href);
        return spot.handle;
      };
      const settledMemory = async (attempts) => {
        let memory = null;
        for (let i = 0; i < attempts && memory === null; i += 1) {
          await delay(100);
          memory = await page.evaluate(`window.localStorage.getItem(${JSON.stringify(MEMORY_KEY)})`);
        }
        return memory;
      };
      const pendingAt = (operationId) => page.evaluate(`window.localStorage.getItem(${JSON.stringify(PENDING_PREFIX + operationId)})`);
      // A navigation that must NOT reseed still has to be a real document
      // load: /terminal#a -> /terminal#b differs only in the fragment, which
      // the browser serves same-document, so the page would never re-run.
      // Bouncing through the blank fixture preserves storage and forces the
      // load. (Without this the single-use replay receipt below was vacuous.)
      const bounce = async () => { await page.goto(`${stack.origin}/__fixture/blank`); await delay(120); };
      const forget = () => page.evaluate(`window.localStorage.removeItem(${JSON.stringify(MEMORY_KEY)})`);
      try {
        // No attachment outcome short of a real COMMIT may write, even with a
        // perfectly good staged candidate.
        for (const failure of ["before_prepare", "after_prepare", "lease_held"]) {
          stack.reset();
          stack.state.attachmentFailure = failure;
          await attach(REMEMBERED, [stagedEntry(OP_A, REMEMBERED)]);
          await delay(700);
          const state = await page.state();
          assert(state.href.includes("/terminal"), `the terminal document did not load for ${failure}`);
          const memory = await page.evaluate(`window.localStorage.getItem(${JSON.stringify(MEMORY_KEY)})`);
          assert(memory === null, `a failure ${failure} wrote device memory: ${memory}`);
        }

        // The trusted path: the action staged ops, the page attached and
        // committed, the record names ops.
        stack.reset();
        const handle = await attach(REMEMBERED, [stagedEntry(OP_A, REMEMBERED, "Primary")], "&alias=Primary");
        const memory = await settledMemory(60);
        assert(memory, "a committed unified attachment did not record device memory");
        const record = JSON.parse(memory);
        assert.call(null, Object.keys(record).sort().join(",") === "alias,at,draftScope,name,realm,server", `record carries unexpected fields: ${Object.keys(record).join(",")}`);
        assert(record.draftScope === REMEMBERED.scope, "the record does not pin the exact incarnation the page attached to");
        assert(record.name === REMEMBERED.name && record.realm === "local" && record.server === "private", `record identity is wrong: ${memory}`);
        assert(!memory.includes(handle) && !memory.includes("/terminal") && !memory.includes("handle="), "the record leaked a capability or a URL");
        const committed = stack.state.attachments.some((attachment) => attachment.frames.includes("READY"));
        assert(committed, "the fixture never saw the attachment become live");

        // The staged candidate is consumed by the navigation it was staged for.
        assert(await pendingAt(OP_A) === null, "the staged candidate survived its navigation and could be replayed");

        // ---- adjudication EP6-R1, the substituted-draft-scope receipt ----
        // Every operand is the trusted one EXCEPT the URL's draft scope, which
        // names a second live incarnation of the same name. A COMMIT proves a
        // handle attached; it never proves the URL names that attachment, so
        // the page must record NEITHER session rather than rename "last
        // session" to one the operator never opened.
        stack.reset();
        stack.state.successor = true;
        await attach(SUCCESSOR, [stagedEntry(OP_A, REMEMBERED)]);
        const substituted = await settledMemory(25);
        assert(substituted === null, `a substituted draft scope recorded device memory: ${substituted}`);

        // The mirror: the URL is honest but nothing trusted staged it. A
        // directly typed or hand-edited terminal URL attaches and simply does
        // not rewrite the memory — whether or not it names an operation.
        for (const openId of [null, OP_A]) {
          stack.reset();
          await attach(REMEMBERED, [], "", openId);
          const unstaged = await settledMemory(25);
          assert(unstaged === null, `an unstaged attachment recorded device memory: ${unstaged}`);
        }

        // A candidate is single use: replaying the same navigation after the
        // first one consumed it records nothing the second time.
        stack.reset();
        await attach(REMEMBERED, [stagedEntry(OP_A, REMEMBERED)]);
        assert(await settledMemory(60), "the staged navigation did not record");
        await forget();
        await bounce();
        stack.reset();
        const replay = target(REMEMBERED);
        await page.goto(replay.href);
        const replayed = await settledMemory(25);
        assert(replayed === null, `a consumed candidate was replayed: ${replayed}`);

        // ---- adjudication EP6-R2, shape 1: two operations, one dashboard ----
        // Two trusted actions stage two candidates. Whichever target the
        // operator reaches first claims ITS OWN candidate and leaves the
        // sibling's untouched, so both land on the identity their own tap
        // named, in either arrival order. The durable record is the last
        // successful matching COMMIT — never a borrowed one.
        for (const order of [[[OP_A, REMEMBERED], [OP_B, DEFAULT_SESSION]], [[OP_B, DEFAULT_SESSION], [OP_A, REMEMBERED]]]) {
          const [[firstOp, firstSession], [secondOp, secondSession]] = order;
          const label = `${firstOp === OP_A ? "A" : "B"}-then-${secondOp === OP_A ? "A" : "B"}`;
          stack.reset();
          await attach(firstSession, [stagedEntry(OP_A, REMEMBERED), stagedEntry(OP_B, DEFAULT_SESSION)], "", firstOp);
          const first = await settledMemory(60);
          assert(first, `${label}: the first target did not record`);
          assert(JSON.parse(first).draftScope === firstSession.scope, `${label}: the first target recorded a borrowed identity: ${first}`);
          assert(await pendingAt(firstOp) === null, `${label}: the claimed candidate survived its navigation`);
          assert(await pendingAt(secondOp) !== null, `${label}: one target consumed or evicted its sibling's candidate`);

          await forget();
          await bounce();
          stack.reset();
          const second = target(secondSession, "", secondOp);
          await page.goto(second.href);
          const secondMemory = await settledMemory(60);
          assert(secondMemory, `${label}: the second target did not record`);
          assert(JSON.parse(secondMemory).draftScope === secondSession.scope, `${label}: the second target recorded a borrowed identity: ${secondMemory}`);
          assert(await pendingAt(secondOp) === null, `${label}: the second candidate survived its navigation`);
        }

        // A target that names one operation but carries another operation's
        // scope is a mismatched pair: it drops the candidate rather than
        // renaming it, and records nothing.
        stack.reset();
        await attach(REMEMBERED, [stagedEntry(OP_B, DEFAULT_SESSION)], "", OP_B);
        const crossed = await settledMemory(25);
        assert(crossed === null, `a crossed operation/scope pair recorded device memory: ${crossed}`);
        assert(await pendingAt(OP_B) === null, "a crossed pair left its candidate claimable");

        // ---- adjudication EP6-R2, shape 2: a scope-less page cannot claim ----
        // A hand-edited URL with no incarnation of its own must DROP a staged
        // candidate, never consume it into a record it cannot corroborate.
        stack.reset();
        await attach(REMEMBERED, [stagedEntry(OP_A, REMEMBERED)], "", OP_A, false);
        const scopeless = await settledMemory(25);
        assert(scopeless === null, `a scope-less page recorded device memory: ${scopeless}`);
        assert(await pendingAt(OP_A) === null, "a scope-less page left its candidate claimable");

        // Shape 3 — an event from a generation the recorder was not armed for
        // cannot commit the candidate — is pinned in the unit matrix
        // (test/session_memory.test.ts, "SHAPE 3"). It is unreachable from
        // here on purpose: attachment handles are one-time, so this fixture
        // cannot serve a second transport generation to one page.
      } finally { await page.close(); }
    });

    await item("EP6-F2", "resume is exact-incarnation: a same-name successor is never resumed", async () => {
      const page = await driver.open({ viewport: PHONE, coarse: true });
      try {
        stack.reset();
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        await page.goto(`${stack.origin}/`);
        let state = await page.state();
        assert(state.landingHidden === false, "a live remembered session produced no landing card");
        assert(state.kicker.some(text => text.startsWith("Resume")), `expected a Resume card, saw ${JSON.stringify(state.kicker)}`);
        assert(state.actionAria === `Resume ${REMEMBERED.name} · private`, `card accessible name is ${JSON.stringify(state.actionAria)}`);
        assert(state.label.includes(REMEMBERED.name), `card must show the session name: ${JSON.stringify(state.label)}`);
        if (shot("phone_resume_first")) await page.screenshot(shot("phone_resume_first"));

        // The remembered incarnation ends; a same-name successor is created.
        stack.state.remembered = "absent";
        stack.state.successor = true;
        const mark = stack.mark();
        await page.goto(`${stack.origin}/`);
        state = await page.state();
        assert(state.kicker.length === 0 || !state.kicker.some(text => text.startsWith("Resume")), "a same-name successor was offered as a resume");
        assert(state.notes.some((note) => note.includes("has ended")), `expected an honest ended notice, saw ${JSON.stringify(state.notes)}`);
        assert(state.sessionRows === 1, "the successor must still be listed like any other session");
        assertNoAuthority(stack.ledgerSince(mark), "EP6-F2 ended");
        assertQuiet(state, "EP6-F2 ended");
        if (shot("phone_has_ended")) await page.screenshot(shot("phone_has_ended"));

        // Two live sessions carrying the SAME pinned scope: ambiguous, never a guess.
        stack.state.remembered = "open";
        stack.state.successor = false;
        stack.state.duplicateRemembered = true;
        await page.goto(`${stack.origin}/`);
        state = await page.state();
        assert(!state.kicker.some(text => text.startsWith("Resume")), "an ambiguous identity produced a resume card");
        assert(state.notes.some((note) => note.includes("more than one")), `expected an ambiguity notice, saw ${JSON.stringify(state.notes)}`);
        assert(state.filterValue === REMEMBERED.name, `the list was not filtered to the remembered name: ${state.filterValue}`);
      } finally { await page.close(); }
    });

    // ---------------------------------------------------------------- EP6-F3
    await item("EP6-F3", "a landing spends no authority before a trusted tap", async () => {
      const page = await driver.open({ viewport: PHONE, coarse: true });
      try {
        stack.reset();
        // The remembered session is controlled on another device.
        stack.state.rememberedAttached = 1;
        stack.state.preferenceDefault = { realm: "local", server: "private", name: DEFAULT_SESSION.name };
        stack.state.defaultSession = "open";
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        for (const entry of [`${stack.origin}/`, `${stack.origin}/?resume=1`]) {
          const mark = stack.mark();
          await page.goto(entry);
          const state = await page.state();
          assertNoAuthority(stack.ledgerSince(mark), `EP6-F3 ${entry}`);
          assertQuiet(state, `EP6-F3 ${entry}`);
        }
        // Reload, a restore-shaped visibility change, and an orientation change.
        let mark = stack.mark();
        await page.reload();
        await settle(page);
        await page.emitVisible();
        await page.setViewport({ width: PHONE.height, height: PHONE.width });
        await delay(200);
        await page.setViewport(PHONE);
        await delay(200);
        const state = await page.state();
        assertNoAuthority(stack.ledgerSince(mark), "EP6-F3 restore/orientation");
        assertQuiet(state, "EP6-F3 restore/orientation");
        assert(consoleErrors(page).length === 0, `console errors: ${JSON.stringify(page.consoleMessages)}`);
        // The other device is still the controller: nothing here displaced it.
        assert(stack.count("POST /api/control-takeovers") === 0, "a landing attempted a takeover");
      } finally { await page.close(); }
    });

    // ---------------------------------------------------------------- EP6-F4
    await item("EP6-F4", "one trusted tap, one adoption at most, same tab, no popup", async () => {
      const page = await driver.open({ viewport: PHONE, coarse: true });
      try {
        // `open`: the card is the row's own anchor. One same-tab navigation with
        // the already-minted control handle; no adoption, no popup, no new tab.
        step("F4 open: seed and load");
        stack.reset();
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        await page.goto(`${stack.origin}/`);
        let state = await page.state();
        assert(state.actionTag === "A", `an open session's resume action is ${state.actionTag}, not an anchor`);
        const targetsBefore = await page.targetCount();
        step("F4 open: trusted tap");
        await page.tapAt(at(state.actionBox));
        await delay(900);
        state = await page.state();
        assert(state.href.includes("/terminal"), `a trusted tap did not enter the session: ${state.href}`);
        assert(state.href.includes("engine=unified-dev"), `the tap did not land on the unified target: ${state.href}`);
        assert(state.opens === 0 || state.href.includes("/terminal"), "the tap opened a popup");
        assert(stack.count("POST /api/session-adoptions") === 0, "an already-open session was adopted");
        assert((await page.targetCount()) === targetsBefore, "the tap created a second browsing context");

        // `adoptable`: exactly one adoption POST, then a same-tab assign. A
        // second rapid tap cannot spend a second adoption.
        step("F4 adoptable: seed and load");
        stack.reset();
        stack.state.remembered = "adoptable";
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        await page.goto(`${stack.origin}/`);
        state = await page.state();
        assert(state.actionTag === "BUTTON", `an adoptable session's resume action is ${state.actionTag}, not a button`);
        const point = at(state.actionBox);
        const adoptTargetsBefore = await page.targetCount();
        step("F4 adoptable: first trusted tap");
        await page.tapAt(point);
        await delay(600);
        // Judged before the second tap and without touching the page: the
        // browsing-context count comes from the browser, so a popup is visible
        // even while the tab itself is mid-navigation.
        step("F4 adoptable: count browsing contexts");
        assert((await page.targetCount()) === adoptTargetsBefore, "the adopt path created a second browsing context");
        assert(stack.count("POST /api/session-adoptions") === 1, `first tap spent ${stack.count("POST /api/session-adoptions")} adoptions`);
        step("F4 adoptable: second tap cannot spend a second adoption");
        await page.tapAt(point).catch(() => undefined);
        await delay(500);
        step("F4 adoptable: read outcome");
        state = await page.state();
        assert(stack.count("POST /api/session-adoptions") === 1, `adoptions spent: ${stack.count("POST /api/session-adoptions")}`);
        assert(state.href.includes("/terminal"), `the adopted session did not open same-tab: ${state.href}`);

        // An inventory refresh arriving mid-adoption — the shape of
        // backgrounding the app and returning to it — must not replace the
        // in-flight action with a fresh, enabled one.
        step("F4 mid-flight refresh: seed and load");
        stack.reset();
        stack.state.remembered = "adoptable";
        stack.state.adoptionDelayMs = 1_500;
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        await page.goto(`${stack.origin}/`);
        state = await page.state();
        assert(state.actionTag === "BUTTON", "the mid-flight case did not render an adoptable card");
        const slowPoint = at(state.actionBox);
        step("F4 mid-flight refresh: tap, then refresh while the adoption is open");
        await page.tapAt(slowPoint);
        await delay(250);
        await page.emitVisible();
        await delay(250);
        state = await page.state();
        assert(state.actionDisabled === true, "an inventory refresh re-enabled the action while its adoption was still open");
        await page.tapAt(slowPoint);
        await delay(1_800);
        assert(stack.count("POST /api/session-adoptions") === 1, `a refresh mid-adoption allowed ${stack.count("POST /api/session-adoptions")} adoptions`);

        // A programmatic activation is refused: only a trusted event attaches.
        step("F4 untrusted: seed and load");
        stack.reset();
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        stack.state.remembered = "adoptable";
        await page.goto(`${stack.origin}/`);
        const mark = stack.mark();
        await page.evaluate(`document.querySelector(".landing-action").click()`);
        await delay(400);
        state = await page.state();
        assert(!state.href.includes("/terminal"), "an untrusted click attached to a session");
        assertNoAuthority(stack.ledgerSince(mark), "EP6-F4 untrusted click");
      } finally { await page.close(); }
    });

    // ---------------------------------------------------------------- EP6-F5
    await item("EP6-F5", "the default is offered only without a resumable memory, and always says default", async () => {
      const page = await driver.open({ viewport: PHONE, coarse: true });
      try {
        // No memory at all + a resolvable default.
        stack.reset();
        stack.state.remembered = "absent";
        stack.state.defaultSession = "open";
        stack.state.preferenceDefault = { realm: "local", server: "private", name: DEFAULT_SESSION.name };
        await page.seed([]);
        let mark = stack.mark();
        await page.goto(`${stack.origin}/`);
        let state = await page.state();
        assert(state.kicker.includes("Default session"), `expected a default card, saw ${JSON.stringify(state.kicker)}`);
        assert(String(state.actionAria).toLowerCase().includes("default"), `the default action must name itself: ${JSON.stringify(state.actionAria)}`);
        assert(!String(state.actionAria).startsWith("Resume"), "the default impersonated a resume");
        assertNoAuthority(stack.ledgerSince(mark), "EP6-F5 default");
        assertQuiet(state, "EP6-F5 default");
        if (shot("phone_default_card")) await page.screenshot(shot("phone_default_card"));

        // A resumable memory takes precedence and suppresses the default card.
        stack.reset();
        stack.state.defaultSession = "open";
        stack.state.preferenceDefault = { realm: "local", server: "private", name: DEFAULT_SESSION.name };
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        await page.goto(`${stack.origin}/`);
        state = await page.state();
        assert(state.kicker.some(text => text.startsWith("Resume")) && !state.kicker.includes("Default session"), `precedence is wrong: ${JSON.stringify(state.kicker)}`);

        // An ended memory shows the honest notice AND the default, clearly
        // labelled — never the default silently standing in for the resume.
        stack.state.remembered = "absent";
        stack.state.successor = true;
        await page.goto(`${stack.origin}/`);
        state = await page.state();
        assert(state.notes.some((note) => note.includes("has ended")), "the ended notice disappeared once a default existed");
        assert(state.kicker.includes("Default session") && !state.kicker.some(text => text.startsWith("Resume")), `ended + default is wrong: ${JSON.stringify(state.kicker)}`);

        // A remembered session that is live but cannot be opened says exactly
        // why, is never dressed as a resume, and lets the default be offered
        // beneath the reason.
        stack.reset();
        stack.state.remembered = "blocked";
        stack.state.defaultSession = "open";
        stack.state.preferenceDefault = { realm: "local", server: "private", name: DEFAULT_SESSION.name };
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        mark = stack.mark();
        await page.goto(`${stack.origin}/`);
        state = await page.state();
        assert(!state.kicker.some(text => text.startsWith("Resume")), "a blocked session was offered as a resume");
        assert(state.notes.some((note) => note.includes("full-screen app is active")), `expected the reviewed blocked reason, saw ${JSON.stringify(state.notes)}`);
        assert(state.kicker.includes("Default session"), `the default was not offered beside a blocked resume: ${JSON.stringify(state.kicker)}`);
        assertNoAuthority(stack.ledgerSince(mark), "EP6-F5 blocked");
        assertQuiet(state, "EP6-F5 blocked");

        // A default the inventory cannot resolve is a named reason, never a create.
        stack.reset();
        stack.state.remembered = "absent";
        stack.state.defaultSession = "absent";
        stack.state.preferenceDefault = { realm: "local", server: "private", name: "not-running" };
        mark = stack.mark();
        await page.goto(`${stack.origin}/`);
        state = await page.state();
        assert(state.notes.some((note) => note.includes("not running")), `expected a missing-default reason, saw ${JSON.stringify(state.notes)}`);
        assert(stack.count("POST /api/sessions") === 0, "a missing default was created");
        assertNoAuthority(stack.ledgerSince(mark), "EP6-F5 missing default");
      } finally { await page.close(); }
    });

    // ---------------------------------------------------------------- EP6-F6
    await item("EP6-F6", "hostile device memory fails closed with no amplification", async () => {
      const page = await driver.open({ viewport: PHONE, coarse: true });
      try {
        const hostile = [
          ["malformed", "{"],
          ["array", "[]"],
          ["unknown field", JSON.stringify({ draftScope: REMEMBERED.scope, name: "ops", realm: "local", server: "private", at: Date.now(), handle: "cccccccccccccccccccccccccccccccccccccccccc0" })],
          ["capability scope", JSON.stringify({ draftScope: "cccccccccccccccccccccccccccccccccccccccccc0", name: "ops", realm: "local", server: "private", at: Date.now() })],
          ["future stamp", JSON.stringify({ draftScope: REMEMBERED.scope, name: "ops", realm: "local", server: "private", at: Date.now() + 90 * 24 * 3_600_000 })],
          ["overlong", JSON.stringify({ draftScope: REMEMBERED.scope, name: "x".repeat(400), realm: "local", server: "private", at: Date.now() })],
          ["realm disagreement", JSON.stringify({ draftScope: REMEMBERED.scope, name: "ops", realm: "elsewhere", server: "private", at: Date.now() })],
        ];
        let baselineRequests = null;
        for (const [label, raw] of hostile) {
          stack.reset();
          await page.seed([[MEMORY_KEY, raw]]);
          const mark = stack.mark();
          await page.goto(`${stack.origin}/`);
          const state = await page.state();
          assert(!state.kicker.some(text => text.startsWith("Resume")), `hostile memory (${label}) produced a resume card`);
          assert(state.sessionRows === 1, `hostile memory (${label}) broke the ordinary list`);
          assert(!state.landingText.includes("cccccccccccccccccccccccccccccccccccccccccc0"), `hostile memory (${label}) reflected a token into the page`);
          assertNoAuthority(stack.ledgerSince(mark), `EP6-F6 ${label}`);
          assertQuiet(state, `EP6-F6 ${label}`);
          const requests = stack.ledgerSince(mark).length;
          if (baselineRequests === null) baselineRequests = requests;
          assert(Math.abs(requests - baselineRequests) <= 1, `hostile memory (${label}) amplified requests: ${requests} vs ${baselineRequests}`);
        }
        assert(consoleErrors(page).length === 0, `hostile memory produced console errors: ${JSON.stringify(page.consoleMessages)}`);
      } finally { await page.close(); }
    });

    // ---------------------------------------------------------------- EP6-F7
    await item("EP6-F7", "the cards share one inventory snapshot and spend one preferences read", async () => {
      const page = await driver.open({ viewport: PHONE, coarse: true });
      try {
        // A load with NOTHING for the landing to draw, and an otherwise
        // identical load where it draws a notice AND a default card. Equal
        // inventory counts prove the cards consume the snapshot the dashboard
        // already fetched rather than resolving independently.
        stack.reset();
        stack.state.remembered = "absent";
        stack.state.successor = true;
        await page.seed([]);
        let mark = stack.mark();
        await page.goto(`${stack.origin}/`);
        await delay(400);
        const quiet = stack.ledgerSince(mark);
        const quietInventory = quiet.filter((entry) => entry === "GET /api/inventory").length;
        assert(quiet.filter((entry) => entry === "GET /api/preferences").length === 1, `a bare landing read preferences ${quiet.filter((entry) => entry === "GET /api/preferences").length} times`);

        stack.reset();
        stack.state.remembered = "absent";
        stack.state.successor = true;
        stack.state.defaultSession = "open";
        stack.state.preferenceDefault = { realm: "local", server: "private", name: DEFAULT_SESSION.name };
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        mark = stack.mark();
        await page.goto(`${stack.origin}/`);
        await delay(400);
        const busy = stack.ledgerSince(mark);
        const busyInventory = busy.filter((entry) => entry === "GET /api/inventory").length;
        const busyPreferences = busy.filter((entry) => entry === "GET /api/preferences").length;
        const state = await page.state();
        assert(state.notes.length >= 1 && state.kicker.includes("Default session"), `the busy case did not render two landing consumers: ${JSON.stringify({ notes: state.notes, kicker: state.kicker })}`);
        assert(busyInventory === quietInventory, `landing cards multiplied inventory reads: ${busyInventory} vs ${quietInventory}`);
        assert(busyPreferences === 1, `preferences were read ${busyPreferences} times`);

        // A manual refresh is the only extra inventory read, and it re-reads no
        // preference; focus, visibility and orientation add neither.
        mark = stack.mark();
        await page.emitVisible();
        await page.setViewport({ width: PHONE.height, height: PHONE.width });
        await delay(200);
        await page.setViewport(PHONE);
        await delay(200);
        assert(stack.ledgerSince(mark).filter((entry) => entry === "GET /api/preferences").length === 0, "a visibility or orientation change re-read preferences");
        mark = stack.mark();
        const refresh = await page.evaluate(`(() => { const node = document.querySelector(".dashboard-refresh"); const box = node.getBoundingClientRect(); return { x: box.x + box.width / 2, y: box.y + box.height / 2 }; })()`);
        await page.clickAt(refresh);
        await delay(500);
        const after = stack.ledgerSince(mark);
        assert(after.filter((entry) => entry === "GET /api/inventory").length === 1, `a manual refresh spent ${after.filter((entry) => entry === "GET /api/inventory").length} inventory reads`);
        assert(after.filter((entry) => entry === "GET /api/preferences").length === 0, "a manual refresh re-read preferences");
      } finally { await page.close(); }
    });

    // --------------------------------------------------------------- UX14-F4
    await item("UX15-F4", "the dashboard composer picker offers 9–24 on every pointer, no phone floor, no write", async () => {
      // UX14 F4: a phone renders the composer at max(16px, value), so the
      // dashboard card offers 16 ("phone minimum") through 24 there and shows a
      // stored 13 as 16 without a PUT; a fine pointer keeps the full 9–24 list.
      const COMPOSER = `(() => {
        const field = [...document.querySelectorAll(".dashboard-appearance__field")].find((node) => (node.textContent || "").startsWith("Composer text"));
        const select = field ? field.querySelector("select") : null;
        if (!select) return null;
        return { value: select.value, floor: select.dataset.floor || null, options: [...select.options].map((option) => ({ value: option.value, text: option.textContent })) };
      })()`;
      const picker = async (page) => {
        let found = null;
        for (let attempt = 0; attempt < 30 && !(found && found.value); attempt += 1) { found = await page.evaluate(COMPOSER); await delay(100); }
        return found;
      };
      const phone = await driver.open({ viewport: PHONE, coarse: true });
      try {
        stack.reset();
        await phone.seed([]);
        const mark = stack.mark();
        await phone.goto(`${stack.origin}/`);
        const phonePicker = await picker(phone);
        // UX15 §15.5: the viewport meta suppresses the iOS focus zoom, so the
        // phone gets the whole range and the stored value as-is.
        assert(phonePicker && phonePicker.options.length === 16 && phonePicker.options[0].value === "9" && phonePicker.options[0].text === "9 px" && phonePicker.options.at(-1).value === "24", `the phone composer picker offers the wrong range: ${JSON.stringify(phonePicker)}`);
        assert(phonePicker.value === "13" && phonePicker.floor === null, `a stored 13 px was not shown as 13 px on the phone: ${JSON.stringify(phonePicker)}`);
        await delay(300);
        assert(stack.ledgerSince(mark).filter((entry) => entry.startsWith("PUT /api/preferences")).length === 0, "rendering the picker wrote the record");
        // UX15 §15.2 (G2): the dashboard's buttons paint the tap flash too.
        const tap = await phone.evaluate(`(() => {
          const button = document.querySelector(".dashboard-search-clear");
          if (!button) return null;
          button.addEventListener("click", (event) => { event.stopImmediatePropagation(); event.preventDefault(); }, { capture: true, once: true });
          button.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
          return { tapped: button.classList.contains("persea-tapped"), animation: getComputedStyle(button).animationName };
        })()`);
        assert(tap && tap.tapped && tap.animation === "dashboard-tap", `the dashboard button painted no tap feedback: ${JSON.stringify(tap)}`);
      } finally { await phone.close(); }
      const desktop = await driver.open({ viewport: DESKTOP, coarse: false });
      try {
        stack.reset();
        await desktop.seed([]);
        await desktop.goto(`${stack.origin}/`);
        const desktopPicker = await picker(desktop);
        assert(desktopPicker && desktopPicker.options.length === 16 && desktopPicker.options[0].value === "9" && desktopPicker.options[0].text === "9 px" && desktopPicker.value === "13" && desktopPicker.floor === null, `the desktop composer picker changed: ${JSON.stringify(desktopPicker)}`);
      } finally { await desktop.close(); }
    });

    // ---------------------------------------------------------------- EP6-F8
    await item("EP6-F8", "the manifest and icons are ordinary served bundle files with the right types", async () => {
      const page = await driver.open({ viewport: DESKTOP, coarse: false });
      try {
        stack.reset();
        await page.seed([]);
        await page.goto(`${stack.origin}/`);
        const served = await page.evaluate(`(async () => {
          const manifest = await fetch("/manifest.webmanifest", { credentials: "same-origin" });
          const text = await manifest.text();
          const icons = {};
          for (const src of ["/icon-192.png", "/icon-512.png", "/apple-touch-icon.png"]) {
            const response = await fetch(src, { credentials: "same-origin" });
            const blob = await response.blob();
            const bitmap = await createImageBitmap(blob);
            icons[src] = { type: response.headers.get("content-type"), status: response.status, width: bitmap.width, height: bitmap.height };
          }
          const link = document.querySelector('link[rel="manifest"]');
          const apple = document.querySelector('link[rel="apple-touch-icon"]');
          const capable = document.querySelector('meta[name="apple-mobile-web-app-capable"]');
          const theme = document.querySelector('meta[name="theme-color"]');
          return {
            status: manifest.status, type: manifest.headers.get("content-type"), text, icons,
            link: link ? link.getAttribute("href") : null,
            apple: apple ? apple.getAttribute("href") : null,
            capable: capable ? capable.getAttribute("content") : null,
            theme: theme ? theme.getAttribute("content") : null,
          };
        })()`);
        assert(served.status === 200, `manifest status ${served.status}`);
        assert(String(served.type).startsWith("application/manifest+json"), `manifest content type ${served.type}`);
        assert(manifestVerdict(served.text) === "ok", `served manifest rejected: ${manifestVerdict(served.text)}`);
        assert(served.link === "/manifest.webmanifest", `index does not link the manifest: ${served.link}`);
        assert(served.apple === "/apple-touch-icon.png", `index does not link the apple touch icon: ${served.apple}`);
        assert(served.capable === "yes", "index does not declare apple-mobile-web-app-capable");
        assert(served.theme === "#081018", `theme-color meta is ${served.theme}`);
        for (const [src, icon] of Object.entries(served.icons)) {
          assert(icon.status === 200, `${src} status ${icon.status}`);
          assert(String(icon.type).startsWith("image/png"), `${src} content type ${icon.type}`);
          assert(icon.width === icon.height && icon.width > 0, `${src} is ${icon.width}x${icon.height}`);
        }
        assert(served.icons["/icon-192.png"].width === 192 && served.icons["/icon-512.png"].width === 512 && served.icons["/apple-touch-icon.png"].width === 180, "an icon does not have its declared size");
      } finally { await page.close(); }
    });

    // ---------------------------------------------------------------- EP6-F9
    await item("EP6-F9", "there is no service worker anywhere: source, bundle, manifest, or browser", async () => {
      const sources = [];
      const walk = (dir) => {
        for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
          const full = path.join(dir, entry.name);
          if (entry.isDirectory()) { walk(full); continue; }
          if (/\.(ts|css|html|webmanifest)$/.test(entry.name)) sources.push(full);
        }
      };
      walk(path.join(UI, "src"));
      sources.push(path.join(UI, "index.html"), path.join(UI, "manifest.webmanifest"));
      const fence = /serviceWorker|service-worker|serviceworker|workbox|caches\s*\.|CacheStorage/;
      for (const file of sources) {
        const text = fs.readFileSync(file, "utf8");
        assert(!fence.test(text), `${path.relative(UI, file)} references a service worker or cache API`);
      }
      const bundle = fs.readFileSync(path.join(UI, "dist/app.js"), "utf8");
      assert(!/navigator\.serviceWorker|serviceWorker\.register/.test(bundle), "the built bundle references the service worker API");
      assert(manifestVerdict(fs.readFileSync(path.join(UI, "dist/manifest.webmanifest"), "utf8")) === "ok", "the built manifest is not the contract manifest");

      const page = await driver.open({ viewport: DESKTOP, coarse: false });
      try {
        stack.reset();
        await page.seed([]);
        await page.goto(`${stack.origin}/`);
        const browser = await page.evaluate(`(async () => {
          const registrations = navigator.serviceWorker && navigator.serviceWorker.getRegistrations ? (await navigator.serviceWorker.getRegistrations()).length : 0;
          const cacheKeys = typeof caches === "undefined" ? [] : await caches.keys();
          return { registrations, cacheKeys, counted: window.__perseaEP6.serviceWorkerRegistrations };
        })()`);
        assert(browser.registrations === 0, `${browser.registrations} service worker registration(s)`);
        assert(browser.cacheKeys.length === 0, `cache storage is not empty: ${JSON.stringify(browser.cacheKeys)}`);
        assert(browser.counted === 0, "the page called serviceWorker.register");
      } finally { await page.close(); }
    });

    // --------------------------------------------------------------- EP6-F10
    await item("EP6-F10", "Resume has a dedicated visible Open action, 44px+, and raises no keyboard", async () => {
      const page = await driver.open({ viewport: PHONE, coarse: true });
      try {
        stack.reset();
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        await page.goto(`${stack.origin}/?resume=1`);
        let state = await page.state();
        assert(state.coarse === true, "the phone case is not running under a coarse pointer");
        assert(state.dedicatedLandingAction, "Resume does not have its dedicated Open action");
        assert(state.actionBox.width >= 44 && state.actionBox.height >= 44, `the resume action measures ${state.actionBox.width}x${state.actionBox.height}`);
        assert(state.actionBox.y >= 0 && state.actionBox.y + state.actionBox.height <= state.viewport.height, `the card is not fully visible: ${JSON.stringify(state.actionBox)}`);
        // `?resume=1` focuses the card. Focus is not activation, and a link or
        // button never raises the software keyboard.
        assert(state.activeElementIsAction, `?resume=1 focused ${state.activeElementTag}.${state.activeElementClass}`);
        assert(state.textEntryFocused === false, "the landing focused a text entry and would raise the keyboard");
        assert(!state.href.includes("/terminal"), "focusing the card entered the session");

        // Focus is carried across the rebuild an inventory refresh causes; a
        // focused primary action that silently moves to the body 30 seconds
        // later is a keyboard trap of its own.
        await page.emitVisible();
        await delay(250);
        state = await page.state();
        assert(state.activeElementIsAction, `an inventory refresh dropped focus to ${state.activeElementTag}.${state.activeElementClass}`);

        // Structural half, both engines: the action can only be activated by a
        // completed click. No pointer or touch phase is an activation, so a
        // scroll that begins on the card cannot enter a session by construction.
        const source = fs.readFileSync(path.join(UI, "src/dashboard.ts"), "utf8");
        const landingSection = source.slice(source.indexOf("private createOpenAction"), source.indexOf("private createFavoriteButton"));
        assert(landingSection.length > 200, "the landing action could not be located in the source");
        for (const phase of ["pointerdown", "pointerup", "touchstart", "touchend", "mousedown", "mouseup"]) {
          assert(!landingSection.includes(`"${phase}"`), `the landing action binds ${phase}; only a completed click may activate it`);
        }
        assert((landingSection.match(/addEventListener\("click"/g) || []).length === 2, "the landing action must bind exactly one click listener per variant");

        // Behavioural half: a touch scroll that begins on the card.
        const mark = stack.mark();
        await page.touchDrag(at(state.actionBox), 220);
        await delay(400);
        state = await page.state();
        assert(!state.href.includes("/terminal"), "a touch scroll activated the resume card");
        assertNoAuthority(stack.ledgerSince(mark), "EP6-F10 touch scroll");

        // Keyboard activation follows ordinary button semantics.
        stack.reset();
        stack.state.remembered = "adoptable";
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        await page.goto(`${stack.origin}/?resume=1`);
        state = await page.state();
        assert(state.activeElementIsAction, "the adoptable card did not take focus under ?resume=1");
        await page.pressEnter();
        await delay(900);
        state = await page.state();
        assert(state.href.includes("/terminal"), "Enter on the focused card did not enter the session");
      } finally { await page.close(); }
    });

    // --------------------------------------------------------------- EP6-F11
    await item("EP6-F11", "the landing is presentation only; the terminal appears only after the tap", async () => {
      const page = await driver.open({ viewport: DESKTOP, coarse: false });
      try {
        stack.reset();
        await page.seed([[MEMORY_KEY, memoryRecord(REMEMBERED.scope, REMEMBERED.name, Date.now() - 60_000)]]);
        const mark = stack.mark();
        await page.goto(`${stack.origin}/?resume=1`);
        let state = await page.state();
        const previewReads = () => stack.ledgerSince(mark).filter(entry => entry === "GET /api/session-previews").length;
        for (let attempt = 0; attempt < 30 && previewReads() === 0; attempt += 1) await delay(100);
        const previews = previewReads();
        assert(previews > 0 && previews <= state.sessionRows, "visible desktop rows did not read a bounded first snapshot");
        await delay(750);
        assert(previewReads() === previews, "the landing automatically refreshed its captured preview");
        state = await page.state();
        assert(state.sockets === 0 && state.xtermScreens === 0, "the landing constructed a renderer or a socket");
        assertNoAuthority(stack.ledgerSince(mark), "EP6-F11 landing");
        assert(consoleErrors(page).length === 0, `preview console errors: ${JSON.stringify(page.consoleMessages)}`);
        const before = state.interactiveCount;
        await page.clickAt(at(state.actionBox));
        await delay(1_500);
        state = await page.state();
        assert(state.href.includes("/terminal"), `the trusted click did not enter the session: ${state.href}`);
        for (let attempt = 0; attempt < 40 && state.xtermScreens === 0; attempt += 1) { await delay(100); state = await page.state(); }
        assert(state.xtermScreens === 1, `the session page owns ${state.xtermScreens} renderers`);
        assert(state.sockets === 1, `the session page constructed ${state.sockets} sockets`);
        assert(before > 1, "the dashboard list disappeared behind the landing card");
      } finally { await page.close(); }
    });

    // --------------------------------------------------------------- EP6-F12
    await item("EP6-F12-control", "the manifest contract rejects a wrong fixture before any hardware run", async () => {
      const real = fs.readFileSync(path.join(UI, "dist/manifest.webmanifest"), "utf8");
      assert(manifestVerdict(real) === "ok", `the shipped manifest is not acceptable: ${manifestVerdict(real)}`);
      const base = JSON.parse(real);
      const wrong = [
        ["start_url", { ...base, start_url: "/" }],
        ["display", { ...base, display: "browser" }],
        ["icons", { ...base, icons: [base.icons[0]] }],
        ["theme", { ...base, theme_color: "#ffffff" }],
        ["service worker", { ...base, serviceworker: { src: "/sw.js" } }],
      ];
      for (const [label, value] of wrong) {
        const verdict = manifestVerdict(JSON.stringify(value));
        assert(verdict !== "ok", `a manifest with a wrong ${label} was accepted`);
      }
      process.stdout.write("    EP6-F12 (real iPhone home-screen launch) — NOT RUN: no hardware available to this lane.\n");
    });

    // ---------------------------------------------------------------- UX8-F2
    //
    // A phone-class device opens no workspace view (M5), so the dashboard must
    // not offer a create affordance the same device then refuses to open. The
    // live smoke of 1e8f171 created a workspace from a 390x844 iPhone and was
    // then told "workspace view is not available on this device yet". The
    // notice must be shown BEFORE the record exists, in the create form's
    // place, with the saved records still listed and still openable as a single
    // terminal. Posture is read from the emulated device screen — the same
    // stable witness the /workspace document uses — so a merely narrow desktop
    // window is not a phone.
    await item("UX8-F2", "a phone is never offered a workspace it cannot open, and the desktop path is unchanged", async () => {
      const record = {
        workspace_id: "0123456789abcdef0123456789abcdef",
        name: "saved-one",
        normalized_name: "saved-one",
        revision: 3,
        created_at: "2026-08-28T10:00:00Z",
        updated_at: "2026-08-28T10:00:00Z",
        tree: { kind: "leaf", session: { realm: "local", server: "private", name: REMEMBERED.name }, on_missing: "offer" },
      };
      const panel = () => `(() => {
        const panel = document.querySelector(".workspace-panel");
        if (!panel) return null;
        const notice = panel.querySelector("[data-workspace-create='unavailable']");
        return {
          panel: true,
          createForms: panel.querySelectorAll("form.workspace-panel__create").length,
          nameFields: panel.querySelectorAll("[aria-label='New workspace name']").length,
          sessionPickers: panel.querySelectorAll("[aria-label='First workspace session']").length,
          submits: panel.querySelectorAll("form.workspace-panel__create button[type='submit']").length,
          noticeText: notice ? notice.textContent.trim() : null,
          records: Array.from(panel.querySelectorAll("[data-workspace-id]")).map((item) => item.dataset.workspaceId),
          openLinks: Array.from(panel.querySelectorAll("[data-workspace-action='open']")).map((link) => link.getAttribute("href")),
        };
      })()`;
      const settled = async (page) => {
        let seen = null;
        for (let attempt = 0; attempt < 60 && (seen === null || seen.records.length === 0); attempt += 1) {
          await delay(100);
          seen = await page.evaluate(panel());
        }
        return seen;
      };

      const phone = await driver.open({ viewport: PHONE, coarse: true, screen: PHONE });
      try {
        stack.reset();
        stack.state.workspaces.push(record);
        await phone.goto(`${stack.origin}/`);
        const seen = await settled(phone);
        assert(seen && seen.panel, "the phone dashboard rendered no Workspaces panel");
        assert(seen.noticeText === PHONE_STATE_NOTICE, `the phone must show the M5 notice in the create form's place, saw ${JSON.stringify(seen.noticeText)}`);
        assert(seen.createForms === 0 && seen.nameFields === 0 && seen.sessionPickers === 0 && seen.submits === 0,
          `the phone was still offered a workspace create affordance: ${JSON.stringify(seen)}`);
        assert(seen.records.length === 1 && seen.records[0] === record.workspace_id, `the saved record must stay listed on a phone: ${JSON.stringify(seen.records)}`);
        assert(seen.openLinks.length === 1 && seen.openLinks[0].includes("/workspace"), `the saved record must keep its open link: ${JSON.stringify(seen.openLinks)}`);
        if (shot("phone_workspace_create_refused")) await phone.screenshot(shot("phone_workspace_create_refused"));
      } finally { await phone.close(); }

      const desktop = await driver.open({ viewport: DESKTOP, coarse: false });
      try {
        await desktop.goto(`${stack.origin}/`);
        const seen = await settled(desktop);
        assert(seen && seen.panel, "the desktop dashboard rendered no Workspaces panel");
        assert(seen.noticeText === null, `the desktop path must be unchanged, saw the phone notice: ${JSON.stringify(seen.noticeText)}`);
        assert(seen.createForms === 1 && seen.nameFields === 1 && seen.sessionPickers === 1 && seen.submits === 1,
          `the desktop create affordance changed: ${JSON.stringify(seen)}`);
        assert(seen.records.length === 1, `the saved record must stay listed on the desktop: ${JSON.stringify(seen.records)}`);
      } finally { await desktop.close(); }
    });
  } finally {
    await driver.close();
    await stack.stop();
  }

  clearTimeout(watchdog);
  const summary = { engine: driver.name, origin: stack.origin, items: results, failures, hardware: { "EP6-F12": "NOT RUN — no iPhone available to this lane" } };
  if (EVIDENCE) fs.writeFileSync(path.join(EVIDENCE, `ep6_landing_gate_${driver.name}.json`), `${JSON.stringify(summary, null, 2)}\n`);
  process.stdout.write(`\nE-P6 landing gate (${driver.name}): ${results.filter((entry) => entry.verdict === "PASS").length} passed, ${failures} failed\n`);
  // Explicit: a lingering handle must not turn a finished run into a hang.
  process.exit(failures > 0 ? 1 : 0);
}

main().catch((error) => {
  process.stderr.write(`${String(error && error.stack || error)}\n`);
  process.exit(1);
});
