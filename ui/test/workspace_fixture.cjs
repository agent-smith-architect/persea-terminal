"use strict";

// Workspace fixture (M9 W1b): an in-process front door + unified broker double
// hosting N SESSIONS, each with its own one-time handles, control lease,
// takeover offers, source binding, journal output, and broker automaton. The
// reopen fixture (unified_reopen_fixture.cjs) models one session; this one
// reproduces the same front-door semantics per session so the W1b gate can
// drive the REAL bundled /workspace document against six independent
// attachments and inject per-session outcomes: adoptable, missing, blocked
// projections, a foreign lease holder (lease_held → takeover), typed
// subscriber closes, silent transport drops, expired bindings, a killed
// session replaced by a same-name successor, and one-shot 429s per path.
//
// Documents carry the exact CSP shape the front sends: the nonce'd base policy
// on every document, plus `style-src-attr 'unsafe-inline'` only for the exact
// `/terminal?engine=unified-dev` and `/workspace?engine=unified-dev` keys.

const crypto = require("crypto");
const childProcess = require("child_process");
const fs = require("fs");
const http = require("http");
const https = require("https");
const os = require("os");
const path = require("path");
const { Socket, subprotocols, cookieCSRF, createSnippetStore, readJSON, token, WS_GUID, LIVENESS_PREFIX, REFUSAL_PREFIX } = require("./unified_reopen_fixture.cjs");

const STYLE_NONCE = "BBBBBBBBBBBBBBBBBBBBBB";
const CSRF_TOKEN = "wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww";
const BASE_CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'nonce-" + STYLE_NONCE
  + "'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'";
const ATTRIBUTE_CSP = BASE_CSP + "; style-src-attr 'unsafe-inline'";
const REALM = "local";
const SERVER = "private";

function authorityFor(sessionId, created) {
  return Object.freeze({
    realm: REALM, server: SERVER, uid: 1000, selector_kind: "socket_path", selector_value: "/tmp/private.sock",
    boot_id: "boot-workspace", server_pid: 42, server_start: 100, session_id: sessionId, session_created: created,
  });
}

// The client's authorityKey (dashboard.ts) for a fixture authority, so a gate
// can name the incarnation key a pane must pin.
function draftScopeFor(authority) {
  return JSON.stringify([
    authority.realm, authority.server, authority.selector_kind, authority.selector_value, authority.boot_id, authority.session_id,
    authority.uid, authority.server_pid, authority.server_start, authority.session_created,
  ]);
}

function hermeticTLS() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "persea-workspace-tls-"));
  const keyPath = path.join(dir, "key.pem");
  const certPath = path.join(dir, "cert.pem");
  const result = childProcess.spawnSync("openssl", [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", keyPath, "-out", certPath,
    "-days", "1", "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  if (result.status !== 0) throw new Error(`workspace TLS material could not be generated: ${String(result.stderr).slice(-400)}`);
  return { dir, key: fs.readFileSync(keyPath), cert: fs.readFileSync(certPath) };
}

function startWorkspaceFixture(ui, options = {}) {
  const index = fs.readFileSync(path.join(ui, "dist/index.html"), "utf8").replace("__PERSEA_STYLE_NONCE__", STYLE_NONCE);
  const files = {
    "/app.js": { file: path.join(ui, "dist/app.js"), type: "text/javascript" },
    "/app.css": { file: path.join(ui, "dist/app.css"), type: "text/css" },
    "/xterm.css": { file: path.join(ui, "dist/xterm.css"), type: "text/css" },
    // E-P6 installable shell. index.html links these, so a fixture that does not
    // serve them makes the page 404 assets the front door always has
    // (`requiredBundleFiles` refuses to start a release without them). Same paths,
    // same media types (ruling J-EP6-1).
    "/manifest.webmanifest": { file: path.join(ui, "dist/manifest.webmanifest"), type: "application/manifest+json" },
    "/icon-192.png": { file: path.join(ui, "dist/icon-192.png"), type: "image/png" },
    "/icon-512.png": { file: path.join(ui, "dist/icon-512.png"), type: "image/png" },
    "/apple-touch-icon.png": { file: path.join(ui, "dist/apple-touch-icon.png"), type: "image/png" },
  };
  // Optional extra documents (a harness page) served with the document CSP.
  const extraDocuments = options.documents || {};

  let nextSessionSerial = 0;
  let nextAttachmentId = 0;
  const state = {
    sessions: new Map(),        // name -> session record
    workspaces: new Map(),      // normalized name -> durable workspace record
    handles: new Map(),         // handle -> { purpose, consumed, session }
    offers: new Map(),          // losing handle -> { used, session }
    takeoverClaims: [],         // exact offer/source presented to takeover
    attachments: [],            // per WebSocket attachment
    rateLimits: [],             // one-shot 429s: { path, session?, remaining }
    preferences: { version: 1, theme: "default", font_size: null, composer_font_size: 11, default_session: null, revision: 0, stored: false, available: true },
    preferencesCorrupt: false,
    counters: { documents: 0, inventory: 0, workspaceReads: 0, workspaceWrites: 0, handleRequests: 0, takeovers: 0, adoptions: 0, websockets: 0, replays: 0, sessionsCreated: 0, favicon: 0, requests: 0, preferencesGet: 0, preferencesPut: 0 },
    requestsByPath: {},
    holdInventoryMs: 0,
  };

  function makeSession(name, patch = {}) {
    nextSessionSerial += 1;
    const sessionId = `$${nextSessionSerial}`;
    const session = {
      name,
      sessionId,
      authority: authorityFor(sessionId, 200 + nextSessionSerial),
      // open | adoptable | missing | blocked_alt_screen | blocked_multi_pane | blocked_multi_window | blocked_foreign_server | slots_exhausted | unavailable
      projection: "open",
      detail: null,
      adopted: true,
      // The foreign lease holder: another device's live attachment. A newcomer is refused lease_held and takes over.
      foreignHolder: false,
      lease: null,             // { attachment: id } | { foreign: true }
      bindings: new Set(),
      source: token(),
      columns: 80,
      rows: 24,
      output: [],              // journal: every LIVE payload, so a reattach replays snapshot+tail
      holdPrepareMs: 0,
      holdHandleMs: 0,
      holdAdoptionMs: 0,
      holdLeaseRefusalMs: 0,
      replayFocusReporting: false,
      closeOnAttach: "",       // one-shot: close the next attachment right after upgrade with this reason
      displacedForeign: 0,
      created: false,
      ...patch,
    };
    state.sessions.set(name, session);
    return session;
  }

  function reset(names) {
    for (const attachment of state.attachments) if (attachment.socket && !attachment.socket.closed) attachment.socket.close(1000, "fixture_reset");
    state.sessions.clear();
    state.handles.clear();
    state.offers.clear();
    state.workspaces.clear();
    state.takeoverClaims = [];
    state.attachments = [];
    state.rateLimits = [];
    for (const key of Object.keys(state.counters)) state.counters[key] = 0;
    state.requestsByPath = {};
    state.preferences = { version: 1, theme: "default", font_size: null, composer_font_size: 11, default_session: null, revision: 0, stored: false, available: true };
    state.preferencesCorrupt = false;
    snippets.reset();
    state.holdInventoryMs = 0;
    for (const name of names) makeSession(name);
  }
  // The /api/snippets store double (E-P3): ONE store for the whole document,
  // exactly as the front door holds one global store for the operator.
  const snippets = createSnippetStore(CSRF_TOKEN);
  reset(options.sessions || ["ws01", "ws02", "ws03", "ws04", "ws05", "ws06"]);

  function workspaceWire(name, tree, prior) {
    const now = "2026-08-28T00:00:00Z";
    return {
      workspace_id: prior ? prior.workspace_id : crypto.createHash("sha256").update(`workspace:${name}`).digest("hex").slice(0, 32),
      name, normalized_name: name.toLowerCase(), revision: prior ? prior.revision + 1 : 1,
      created_at: prior ? prior.created_at : now, updated_at: now, tree,
    };
  }

  function putWorkspace(name, tree) {
    const normalized = name.toLowerCase();
    const record = workspaceWire(name, tree, state.workspaces.get(normalized));
    state.workspaces.set(normalized, record);
    return record;
  }

  let inventoryResponses = 0;
  const mint = (purpose, session, mintedBy) => {
    const handle = token();
    state.handles.set(handle, { purpose, consumed: false, session: session.name, sessionId: session.sessionId, mintedBy });
    return handle;
  };

  const inventory = () => {
    inventoryResponses += 1;
    const mintedBy = `inventory#${inventoryResponses}`;
    const sessions = [];
    for (const session of state.sessions.values()) {
      if (session.projection === "missing") continue;
      const unified = session.projection === "open" ? { state: "open", origin: "birth", ...(session.detail ? { detail: session.detail } : {}) }
        : session.projection === "adoptable" ? (session.adopted ? { state: "open", origin: "reconstructed", ...(session.detail ? { detail: session.detail } : {}) } : { state: "adoptable" })
          : { state: session.projection };
      sessions.push({
        handles: { alias: mint("alias", session, mintedBy), observe: mint("observe", session, mintedBy), control: mint("control", session, mintedBy) },
        realm: REALM, server: SERVER, server_status: "ok", session_id: session.sessionId, name: session.name,
        width: session.columns, height: session.rows, attached: session.lease ? 1 : 0, activity: 1_700_000_000 + nextSessionSerial,
        authority: session.authority, unified,
      });
    }
    return {
      image_upload: false,
      realms: [{ name: REALM, display_name: "Local realm", servers: [{ label: SERVER, status: "ok", can_create: true, can_stage_images: false, sessions }] }],
      aliases: [],
    };
  };

  const snapshot = () => ({
    counters: { ...state.counters, inventoryResponses },
    snippets: snippets.stats(),
    preferences: { ...state.preferences },
    requestsByPath: { ...state.requestsByPath },
    workspaces: [...state.workspaces.values()].map((record) => ({ ...record })),
    sessions: [...state.sessions.values()].map((session) => ({
      name: session.name, sessionId: session.sessionId, draftScope: draftScopeFor(session.authority), projection: session.projection, detail: session.detail, adopted: session.adopted,
      leaseHeld: session.lease !== null, leaseAttachment: session.lease && session.lease.attachment ? session.lease.attachment : null, foreignHolder: session.foreignHolder,
      displacedForeign: session.displacedForeign, bindings: session.bindings.size, outputLines: session.output.length,
      created: session.created,
      handlesMinted: [...state.handles.values()].filter((entry) => entry.sessionId === session.sessionId).length,
      handlesConsumed: [...state.handles.values()].filter((entry) => entry.sessionId === session.sessionId && entry.consumed).length,
      consumedFrom: [...state.handles.values()].filter((entry) => entry.sessionId === session.sessionId && entry.consumed).map((entry) => entry.mintedBy),
      attachments: state.attachments.filter((attachment) => attachment.sessionId === session.sessionId).map((attachment) => attachment.id),
      liveAttachments: state.attachments.filter((attachment) => attachment.sessionId === session.sessionId && attachment.socket && !attachment.socket.closed).map((attachment) => attachment.id),
    })),
    attachments: state.attachments.map((attachment) => ({
      id: attachment.id, session: attachment.session, sessionId: attachment.sessionId, handlePurpose: attachment.handlePurpose, takeover: attachment.takeover, engine: attachment.engine,
      mode: attachment.mode, mintedBy: attachment.mintedBy, source: attachment.source, frames: attachment.frames.slice(), inputs: attachment.inputs.slice(), closeReason: attachment.closeReason,
      live: attachment.socket ? !attachment.socket.closed : false, resizes: attachment.resizes || 0, refusals: attachment.refusals || 0,
      committed: attachment.committed, replayLines: attachment.replayLines,
      offeredHandle: attachment.offeredHandle,
    })),
    handles: [...state.handles.entries()].map(([handle, entry]) => ({ handle, ...entry })),
    offers: [...state.offers.entries()].map(([handle, entry]) => ({ handle, ...entry })),
    takeoverClaims: state.takeoverClaims.map((entry) => ({ ...entry })),
  });

  const chargeRateLimit = (pathname, sessionName) => {
    const limit = state.rateLimits.find((entry) => entry.path === pathname && entry.remaining > 0 && (!entry.session || entry.session === sessionName));
    if (!limit) return false;
    limit.remaining -= 1;
    return true;
  };

  const writeLive = (session, text) => {
    session.output.push(text);
    for (const attachment of state.attachments) {
      if (attachment.session === session.name && attachment.writeLive) attachment.writeLive(text);
    }
  };

  const handler = async (request, response) => {
    const url = new URL(request.url, "http://localhost");
    response.setHeader("Cache-Control", "no-store");
    if (url.pathname === "/__fixture/control") {
      if (request.method === "POST") {
        const input = (await readJSON(request)) || {};
        if (input.reset) reset(input.sessions || ["ws01", "ws02", "ws03", "ws04", "ws05", "ws06"]);
        if (input.workspace && typeof input.workspace.name === "string" && input.workspace.tree) putWorkspace(input.workspace.name, input.workspace.tree);
        if (typeof input.dropWorkspace === "string") state.workspaces.delete(input.dropWorkspace.toLowerCase());
        if (typeof input.holdInventoryMs === "number") state.holdInventoryMs = input.holdInventoryMs;
        if (input.session) {
          const session = state.sessions.get(input.session);
          if (session) {
            for (const key of ["projection", "detail", "adopted", "foreignHolder", "holdPrepareMs", "holdHandleMs", "holdAdoptionMs", "holdLeaseRefusalMs", "replayFocusReporting", "closeOnAttach", "columns", "rows"]) {
              if (input[key] !== undefined) session[key] = input[key];
            }
            if (session.foreignHolder && !session.lease) session.lease = { foreign: true };
            if (input.expireBindings) session.bindings.clear();
            if (typeof input.writeLive === "string") writeLive(session, input.writeLive);
            if (typeof input.endLive === "string") {
              // The front's in-band END: the epoch ended, the socket stays open.
              for (const attachment of state.attachments) {
                if (attachment.session === session.name && attachment.socket && !attachment.socket.closed && attachment.endLive) attachment.endLive(input.endLive);
              }
            }
            if (typeof input.closeLive === "string") {
              for (const attachment of state.attachments) {
                if (attachment.session === session.name && attachment.socket && !attachment.socket.closed) attachment.close(1011, input.closeLive);
              }
            }
            if (input.kill) {
              for (const attachment of state.attachments) {
                if (attachment.session === session.name && attachment.socket && !attachment.socket.closed) attachment.close(1011, input.killReason || "");
              }
              session.projection = "missing";
              session.bindings.clear();
              session.lease = null;
            }
          }
        }
        if (typeof input.createSession === "string") {
          // A brand-new incarnation (a decoy or a genuine successor): new
          // session_id and creation time, so a different authority key.
          makeSession(input.createSession, { created: true });
        }
        if (typeof input.replaceSession === "string") {
          makeSession(input.replaceSession, {
            projection: input.replacementProjection || "open",
            detail: input.replacementDetail || null,
            created: true,
          });
        }
        if (input.expireAllBindings) for (const session of state.sessions.values()) session.bindings.clear();
        if (typeof input.closeAllLive === "string") {
          for (const attachment of state.attachments) if (attachment.socket && !attachment.socket.closed) attachment.close(1011, input.closeAllLive);
        }
        if (typeof input.writeLiveAll === "string") for (const session of state.sessions.values()) writeLive(session, input.writeLiveAll);
        if (input.rateLimit) state.rateLimits.push({ path: input.rateLimit.path, session: input.rateLimit.session, remaining: input.rateLimit.count || 1 });
        if (input.resetRequests) state.requestsByPath = {};
        if (input.preferences && typeof input.preferences === "object") state.preferences = { ...state.preferences, ...input.preferences };
        if (typeof input.preferencesAvailable === "boolean") state.preferences.available = input.preferencesAvailable;
        if (typeof input.preferencesCorrupt === "boolean") state.preferencesCorrupt = input.preferencesCorrupt;
        snippets.control(input);
      }
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify(snapshot()));
      return;
    }
    state.counters.requests += 1;
    state.requestsByPath[url.pathname] = (state.requestsByPath[url.pathname] || 0) + 1;
    // Shared factory keyboard defaults for the real mounted panes.
    if (request.method === "GET" && url.pathname === "/api/keyboard-preferences") {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("ETag", '"0"');
      response.end(JSON.stringify({
        version: 1,
        layout: {
          bar: ["key:escape:0", "key:tab:0", "modifier:ctrl", "key:arrow-left:0", "key:arrow-down:0", "key:arrow-up:0", "key:arrow-right:0"],
          favorites: ["key:c:1", "key:d:1", "key:enter:0", "key:home:0", "key:end:0", "key:page-up:0", "key:page-down:0", "key:f1:0"],
        },
        prefixes: { tmux: "key:b:1", screen: "key:a:1" },
        revision: 0, stored: false, available: true,
      }));
      return;
    }
    // E-P6 (ruling J-EP6-2). The dashboard document reads the operator's
    // default-session preference exactly once per page, so a fixture that does
    // not answer this makes the page log a 404 the product never produces.
    // Mirrors `getPreferences` / `writePreferences` in
    // `internal/frontdoor/preferences_api.go` for an operator with nothing
    // stored: the `defaultPreferences()` record (version 1, theme "default",
    // font size 14, no default session), revision 0, `stored: false`, plus the
    // `available` flag of the preferencesResponse wrapper, under the same
    // Cache-Control/Content-Type/ETag headers. E-P5 adds the exact strong-CAS
    // mutation path used by all pane controllers through one shared service.
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
      if (!state.preferences.available) { response.writeHead(503); response.end("preferences store unavailable\n"); return; }
      if (request.headers["if-match"] !== `"${state.preferences.revision}"`) {
        response.setHeader("ETag", `"${state.preferences.revision}"`);
        response.writeHead(412);
        response.end(`${JSON.stringify(state.preferences)}\n`);
        return;
      }
      const body = await readJSON(request);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || body.version !== 1) {
        response.writeHead(403); response.end("forbidden\n"); return;
      }
      state.preferences = {
        version: 1, theme: body.theme, font_size: body.font_size,
        // An absent composer face is the default, exactly as the front door
        // reads it: the field is a plain integer with a default, so a client
        // that predates it still states a whole record as far as it knows.
        composer_font_size: typeof body.composer_font_size === "number" ? body.composer_font_size : 11,
        default_session: body.default_session,
        revision: state.preferences.revision + 1, stored: true, available: true,
      };
      response.setHeader("ETag", `"${state.preferences.revision}"`);
      response.end(`${JSON.stringify(state.preferences)}\n`);
      return;
    }
    if (url.pathname === "/api/snippets" || url.pathname.startsWith("/api/snippets/")) {
      await snippets.handle(request, response, url, `${request.socket.encrypted ? "https" : "http"}://${request.headers.host}`);
      return;
    }
    // Workspace clipboard gates exercise shared text and pane authority. Image
    // transfers use the separate clipboard fixture; this collection is empty.
    if (request.method === "GET" && url.pathname === "/api/clipboard/images" && !url.search) {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("X-Content-Type-Options", "nosniff");
      response.end(JSON.stringify({ items: [] }));
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/session-previews") {
      const session = [...state.sessions.values()].find(item => item.projection !== "missing" && item.sessionId === url.searchParams.get("session_id"));
      if (!session || url.searchParams.get("realm") !== REALM || url.searchParams.get("server") !== SERVER) { response.writeHead(404); response.end("unknown preview session"); return; }
      response.setHeader("Cache-Control", "no-store"); response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ realm: REALM, server: SERVER, session_id: session.sessionId,
        rows: ["Fixture workspace session preview."], width: session.columns, height: session.rows, captured_at: Date.now(), truncated: false }));
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/inventory") {
      state.counters.inventory += 1;
      if (chargeRateLimit("/api/inventory")) { response.writeHead(429); response.end("too many requests"); return; }
      if (state.holdInventoryMs > 0) {
        const hold = state.holdInventoryMs;
        state.holdInventoryMs = 0;
        await new Promise((done) => setTimeout(done, hold));
      }
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(JSON.stringify(inventory()));
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/workspaces") {
      state.counters.workspaceReads += 1;
      if (chargeRateLimit("/api/workspaces")) { response.writeHead(429); response.end("too many requests"); return; }
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(JSON.stringify({ version: 1, items: [...state.workspaces.values()] }));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/workspaces") {
      state.counters.workspaceWrites += 1;
      const body = await readJSON(request);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || body.version !== 1 || typeof body.name !== "string" || !body.tree) { response.writeHead(400); response.end("invalid"); return; }
      const normalized = body.name.toLowerCase();
      const current = state.workspaces.get(normalized);
      if (current) { response.setHeader("Content-Type", "application/json"); response.writeHead(409); response.end(JSON.stringify(current)); return; }
      const next = workspaceWire(body.name, body.tree);
      state.workspaces.set(next.normalized_name, next);
      response.setHeader("Content-Type", "application/json");
      response.writeHead(201);
      response.end(JSON.stringify(next));
      return;
    }
    const workspaceMatch = url.pathname.match(/^\/api\/workspaces\/([0-9a-f]{32})$/);
    if (request.method === "PUT" && workspaceMatch) {
      state.counters.workspaceWrites += 1;
      const body = await readJSON(request);
      const current = [...state.workspaces.values()].find((record) => record.workspace_id === workspaceMatch[1]);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || body.version !== 1 || typeof body.name !== "string" || !body.tree) { response.writeHead(400); response.end("invalid"); return; }
      if (!current) { response.writeHead(404); response.end("not found"); return; }
      if (request.headers["if-match"] !== `"${current.revision}"`) { response.setHeader("Content-Type", "application/json"); response.writeHead(409); response.end(JSON.stringify(current)); return; }
      state.workspaces.delete(current.normalized_name);
      const next = workspaceWire(body.name, body.tree, current);
      state.workspaces.set(next.normalized_name, next);
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify(next));
      return;
    }
    if (request.method === "DELETE" && workspaceMatch) {
      state.counters.workspaceWrites += 1;
      const current = [...state.workspaces.values()].find((record) => record.workspace_id === workspaceMatch[1]);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN) { response.writeHead(400); response.end("invalid"); return; }
      if (!current) { response.writeHead(404); response.end("not found"); return; }
      if (request.headers["if-match"] !== `"${current.revision}"`) { response.setHeader("Content-Type", "application/json"); response.writeHead(409); response.end(JSON.stringify(current)); return; }
      state.workspaces.delete(current.normalized_name);
      response.writeHead(204);
      response.end();
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/attachment-handles") {
      state.counters.handleRequests += 1;
      const body = await readJSON(request);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || (body.purpose !== "control" && body.purpose !== "observe")) {
        response.writeHead(400); response.end("invalid attachment-handle request"); return;
      }
      const session = [...state.sessions.values()].find((candidate) => candidate.bindings.has(body.source));
      if (chargeRateLimit("/api/attachment-handles", session && session.name)) { response.writeHead(429); response.end("too many requests"); return; }
      if (!session) { response.writeHead(410); response.end("attachment source is unavailable"); return; }
      if (session.holdHandleMs > 0) {
        const hold = session.holdHandleMs;
        session.holdHandleMs = 0;
        await new Promise((done) => setTimeout(done, hold));
        if (response.destroyed) return;
      }
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ handle: mint(body.purpose, session, "attachment-handles") }));
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
      let session;
      if (body.offer) {
        const offer = state.offers.get(body.offer);
        if (!offer || offer.used) { code(410, "takeover_unavailable"); return; }
        session = state.sessions.get(offer.session);
        if (chargeRateLimit("/api/control-takeovers", session && session.name)) { code(429, "rate_limited"); return; }
        offer.used = true;
      } else {
        session = [...state.sessions.values()].find((candidate) => candidate.bindings.has(body.source));
        if (chargeRateLimit("/api/control-takeovers", session && session.name)) { code(429, "rate_limited"); return; }
        if (!session) { code(410, "takeover_unavailable"); return; }
      }
      response.setHeader("Content-Type", "application/json");
      response.writeHead(201);
      response.end(JSON.stringify({ handle: mint("control-takeover", session, "control-takeovers") }));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/session-adoptions") {
      state.counters.adoptions += 1;
      const body = await readJSON(request);
      const session = body && [...state.sessions.values()].find((candidate) => candidate.sessionId === body.session_id);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !session) { response.writeHead(400); response.end("invalid"); return; }
      if (chargeRateLimit("/api/session-adoptions", session.name)) { response.writeHead(429); response.end("too many requests"); return; }
      if (session.holdAdoptionMs > 0) {
        const hold = session.holdAdoptionMs;
        session.holdAdoptionMs = 0;
        await new Promise((done) => setTimeout(done, hold));
        if (response.destroyed) return;
      }
      session.adopted = true;
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify(body));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/sessions") {
      const body = await readJSON(request);
      if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !body || typeof body.name !== "string") { response.writeHead(400); response.end("invalid_name"); return; }
      const existing = state.sessions.get(body.name);
      if (existing && existing.projection !== "missing") { response.writeHead(409); response.end("name_taken"); return; }
      state.counters.sessionsCreated += 1;
      makeSession(body.name, { created: true });
      response.writeHead(201);
      response.end("{}");
      return;
    }
    if (request.method === "GET" && url.pathname === "/favicon.ico") {
      state.counters.favicon += 1;
      response.writeHead(204); response.end(); return;
    }
    if (request.method === "GET" && (url.pathname === "/" || url.pathname === "/terminal" || url.pathname === "/workspace")) {
      state.counters.documents += 1;
      response.setHeader("Content-Type", "text/html");
      const exact = (url.pathname === "/terminal" || url.pathname === "/workspace") && url.search === "?engine=unified-dev";
      response.setHeader("Content-Security-Policy", exact ? ATTRIBUTE_CSP : BASE_CSP);
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(index);
      return;
    }
    const extra = extraDocuments[url.pathname];
    if (extra && request.method === "GET") {
      response.setHeader("Content-Type", extra.type || "text/html");
      response.setHeader("Content-Security-Policy", ATTRIBUTE_CSP);
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(typeof extra.body === "function" ? extra.body() : fs.readFileSync(extra.file));
      return;
    }
    const file = files[url.pathname];
    if (!file || request.method !== "GET") { response.writeHead(404); response.end("not found"); return; }
    response.setHeader("Content-Type", file.type);
    response.setHeader("Content-Security-Policy", BASE_CSP);
    response.end(fs.readFileSync(file.file));
  };
  const material = options.tls ? hermeticTLS() : null;
  const server = material
    ? https.createServer({ key: material.key, cert: material.cert }, (request, response) => { void handler(request, response); })
    : http.createServer((request, response) => { void handler(request, response); });

  server.on("upgrade", (request, raw) => {
    state.counters.websockets += 1;
    state.counters.requests += 1;
    state.requestsByPath["/ws"] = (state.requestsByPath["/ws"] || 0) + 1;
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
    if (!entry || entry.consumed || (offered.takeover ? entry.purpose !== "control-takeover" : entry.purpose !== offered.mode)) {
      refuse(410, "stale snapshot"); return;
    }
    const session = state.sessions.get(entry.session);
    if (!session) { refuse(410, "stale snapshot"); return; }
    if (chargeRateLimit("/ws", session.name)) { refuse(429, "too many requests"); return; }
    entry.consumed = true;
    const accept = crypto.createHash("sha1").update(request.headers["sec-websocket-key"] + WS_GUID).digest("base64");
    raw.write("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
      + `Sec-WebSocket-Accept: ${accept}\r\nSec-WebSocket-Protocol: persea-terminal.v1\r\n\r\n`);
    nextAttachmentId += 1;
    const attachment = {
      id: nextAttachmentId,
      session: session.name,
      sessionId: session.sessionId,
      handlePurpose: entry.purpose,
      mintedBy: entry.mintedBy,
      takeover: offered.takeover,
      engine: offered.engine || "",
      mode: "OBSERVE",
      frames: [],
      inputs: [],
      closeReason: null,
      socket: null,
      source: session.source,
      holdsLease: false,
      committed: false,
      replayLines: 0,
      offeredHandle: offered.handle,
    };
    state.attachments.push(attachment);
    const socket = new Socket(raw, (text) => onText(text), (reason) => {
      attachment.closeReason = attachment.closeReason || reason;
      if (attachment.holdsLease && session.lease && session.lease.attachment === attachment.id) session.lease = null;
    });
    attachment.socket = socket;
    attachment.close = (code, reason) => { attachment.closeReason = reason; socket.close(code, reason); };
    const closeWith = attachment.close;

    if (offered.mode === "control") {
      if (session.lease && !offered.takeover) {
        const holderLive = session.lease.foreign || (() => {
          const holder = state.attachments.find((entry) => entry.id === session.lease.attachment);
          return holder && holder.socket && !holder.socket.closed;
        })();
        if (holderLive) {
          const refuseLease = () => {
            if (socket.closed) return;
            state.offers.set(offered.handle, { used: false, session: session.name });
            closeWith(1011, "lease_held");
          };
          if (session.holdLeaseRefusalMs > 0) {
            const hold = session.holdLeaseRefusalMs;
            session.holdLeaseRefusalMs = 0;
            setTimeout(refuseLease, hold);
          } else {
            refuseLease();
          }
          return;
        }
        session.lease = null;
      }
      if (offered.takeover && session.lease) {
        if (session.lease.foreign) {
          session.displacedForeign += 1;
          session.foreignHolder = false;
        } else {
          const holder = state.attachments.find((entry) => entry.id === session.lease.attachment);
          if (holder && holder.socket && !holder.socket.closed) {
            holder.closeReason = "control_displaced";
            holder.socket.close(1000, "control_displaced");
          }
        }
      }
      session.lease = { attachment: attachment.id };
      attachment.holdsLease = true;
    }
    if (attachment.engine === "unified-dev" && session.projection === "adoptable" && !session.adopted) { closeWith(1011, "unified_unavailable"); return; }
    if (session.projection === "missing") { closeWith(1011, "stale_target"); return; }
    if (session.projection !== "open" && session.projection !== "adoptable") { closeWith(1011, "unified_unavailable"); return; }
    if (session.closeOnAttach) {
      const reason = session.closeOnAttach;
      session.closeOnAttach = "";
      closeWith(1011, reason);
      return;
    }

    // Broker automaton. Source binding is established at PREPARE, as the front does.
    session.bindings.add(session.source);
    const epoch = "17";
    const cut = "1";
    const frame = (value) => JSON.stringify({ version: 1, source: session.source, epoch, ...value });
    state.counters.replays += 1;
    // Snapshot+tail: the whole journal so far, so a reattach after a typed
    // subscriber close carries the event that triggered it.
    const replayText = (session.replayFocusReporting ? "\x1b[?1004h" : "") + `fixture-replay ${session.name}\r\n` + session.output.map((line) => `${line}\r\n`).join("");
    attachment.replayLines = session.output.length;
    const replay = Buffer.from(replayText, "binary").toString("base64");
    const sendPrepare = () => socket.sendText(frame({ type: "PREPARE", cut, kind: "INITIAL", columns: session.columns, rows: session.rows, history: [], truncated: false, replay }));
    if (session.holdPrepareMs > 0) {
      const hold = session.holdPrepareMs;
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
      attachment.frames.push(value.type === "INPUT" ? `INPUT:${JSON.stringify(Buffer.from(value.data, "base64").toString("binary"))}` : value.type === "RESIZE_REQUEST" ? `RESIZE_REQUEST:${value.columns}x${value.rows}` : value.type);
      if (value.type === "READY") {
        if (live || value.cut !== cut) { closeWith(1011, "attachment_failed"); return; }
        live = true;
        attachment.committed = true;
        socket.sendText(frame({ type: "COMMIT", cut }));
        socket.sendText(frame({ type: "LIVE", cut, data: Buffer.from(`fixture-live ${session.name}\r\n`, "binary").toString("base64") }));
        attachment.writeLive = (data) => {
          if (!socket.closed) socket.sendText(frame({ type: "LIVE", cut, data: Buffer.from(`${data}\r\n`, "binary").toString("base64") }));
        };
        // The front's in-band END: the epoch ended; the socket stays open.
        attachment.endLive = (reason) => { if (!socket.closed) socket.sendText(frame({ type: "END", reason })); };
        return;
      }
      if (!live) { closeWith(1011, "attachment_failed"); return; }
      if (value.type === "MODE_REQUEST") {
        if (offered.mode === "observe" && value.mode === "CONTROL") { closeWith(1011, "observe_mode"); return; }
        attachment.mode = value.mode;
        socket.sendText(frame({ type: "MODE", mode: value.mode }));
        return;
      }
      if (value.type === "INPUT") {
        if (attachment.mode !== "CONTROL") { closeWith(1011, "input_refused"); return; }
        attachment.inputs.push(Buffer.from(value.data, "base64").toString("binary"));
        return;
      }
      if (value.type === "RESIZE_REQUEST") {
        if (attachment.mode !== "CONTROL") { closeWith(1011, "observe_mode"); return; }
        attachment.resizes = (attachment.resizes || 0) + 1;
        // A committed geometry event on the ordered tail: same cut, new rows,
        // columns unchanged (a vertical fit never changes columns).
        session.rows = value.rows;
        socket.sendText(frame({ type: "PREPARE", cut, kind: "RESIZE", columns: session.columns, rows: value.rows, history: [], truncated: false, replay: "" }));
        return;
      }
      if (value.type === "HISTORY_REQUEST" || value.type === "DEFER") return;
      closeWith(1011, "bad_attachment");
    }
  });

  // Every accepted socket, so close() can release the ones an upgrade took
  // out of the HTTP keep-alive pool.
  const rawSockets = new Set();
  server.on("connection", (socket) => { rawSockets.add(socket); socket.on("close", () => rawSockets.delete(socket)); });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const origin = `${material ? "https" : "http"}://127.0.0.1:${server.address().port}`;
      resolve({
        origin,
        styleNonce: STYLE_NONCE,
        draftScopeFor: (name) => { const session = state.sessions.get(name); return session ? draftScopeFor(session.authority) : undefined; },
        close: () => new Promise((done) => {
          reset([]);
          if (server.closeAllConnections) server.closeAllConnections();
          server.close(() => {
            if (material) fs.rmSync(material.dir, { recursive: true, force: true });
            done();
          });
          setTimeout(() => { for (const socket of rawSockets) socket.destroy(); }, 100);
        }),
      });
    });
  });
}

module.exports = { startWorkspaceFixture, STYLE_NONCE, CSRF_TOKEN, BASE_CSP, ATTRIBUTE_CSP, REALM, SERVER };
