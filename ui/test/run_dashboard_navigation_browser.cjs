"use strict";

const fs = require("fs");
const http = require("http");
const net = require("net");
const os = require("os");
const path = require("path");
const childProcess = require("child_process");
const { defaultKeyboardRecord } = require("./unified_reopen_fixture.cjs");

const UI = path.resolve(__dirname, "..");
const STYLE_NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const R2C_SCROLL_OMISSION_MUTANT =
  process.env.PERSEA_R2C_SCROLL_OMISSION_MUTANT === "1";
const R2C_SCROLL_POSITIVE_CONTROL =
  process.env.PERSEA_R2C_SCROLL_POSITIVE_CONTROL === "1";
if (R2C_SCROLL_OMISSION_MUTANT && R2C_SCROLL_POSITIVE_CONTROL) {
  throw new Error("R2-C scroll controls are mutually exclusive");
}
const CSRF_TOKEN = "sssssssssssssssssssssssssssssssssssssssssss";
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'nonce-"
  + STYLE_NONCE
  + "'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'";
const HANDLE = Object.freeze({
  alias: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  observe: "ooooooooooooooooooooooooooooooooooooooooooo",
  control: "ccccccccccccccccccccccccccccccccccccccccccc",
});
const AUTHORITY = Object.freeze({
  realm: "local",
  server: "private",
  uid: 1000,
  selector_kind: "socket_path",
  selector_value: "/tmp/private.sock",
  boot_id: "boot-r2a",
  server_pid: 42,
  server_start: 100,
  session_id: "$1",
  session_created: 200,
});
const INVENTORY = Object.freeze({
  realms: [{
    name: "local",
    display_name: "Local realm",
    servers: [{
      label: "private",
      status: "ok",
      can_create: true,
      sessions: [{
        handles: HANDLE,
        realm: "local",
        server: "private",
        server_status: "ok",
        session_id: "$1",
        name: "operator session",
        width: 120,
        height: 40,
        attached: 1,
        activity: 1_700_000_000,
        authority: AUTHORITY,
      }],
    }],
  }],
  aliases: [{
    alias_id: "alias-r2a",
    display_alias: "Primary alias",
    normalized_alias: "primary alias",
    session_incarnation: AUTHORITY,
    revision: 1,
    created_at: "ignored",
    updated_at: "ignored",
    state: "bound",
  }],
});
const UNIFIED_HANDLE = Object.freeze({
  alias: "uuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuu",
  observe: "vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv",
  control: "wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww",
});
const UNIFIED_AUTHORITY = Object.freeze({
  ...AUTHORITY,
  session_id: "$17",
  session_created: 217,
});
const ADOPTEE_HANDLE = Object.freeze({
  alias: "1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  observe: "1ooooooooooooooooooooooooooooooooooooooooooo",
  control: "1ccccccccccccccccccccccccccccccccccccccccccc",
});
const ADOPTEE_AUTHORITY = Object.freeze({
  ...AUTHORITY,
  session_id: "$23",
  session_created: 223,
});
const BLOCKED_HANDLE = Object.freeze({
  alias: "2aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  observe: "2ooooooooooooooooooooooooooooooooooooooooooo",
  control: "2ccccccccccccccccccccccccccccccccccccccccccc",
});
const BLOCKED_AUTHORITY = Object.freeze({
  ...AUTHORITY,
  session_id: "$31",
  session_created: 231,
});

function assert(value, message) {
  if (!value) throw new Error(message);
}

function browserBinary() {
  const candidate = require("./browser_path.cjs")();
  assert(candidate && path.isAbsolute(candidate),
    "set CHROME_BIN to an absolute Chromium-family browser executable");
  fs.accessSync(candidate, fs.constants.X_OK);
  return candidate;
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      server.close(() => resolve(address.port));
    });
  });
}

function requestJSON(url, method = "GET") {
  return new Promise((resolve, reject) => {
    const request = http.request(url, { method }, (response) => {
      let body = "";
      response.setEncoding("utf8");
      response.on("data", (chunk) => { body += chunk; });
      response.on("end", () => {
        try {
          resolve(JSON.parse(body));
        } catch (error) {
          reject(error);
        }
      });
    });
    request.on("error", reject);
    request.end();
  });
}

class CDP {
  constructor(url) {
    this.url = url;
    this.next = 1;
    this.pending = new Map();
    this.listeners = new Map();
  }

  async open() {
    this.ws = new WebSocket(this.url);
    await new Promise((resolve, reject) => {
      this.ws.addEventListener("open", resolve, { once: true });
      this.ws.addEventListener("error", reject, { once: true });
    });
    this.ws.addEventListener("message", (event) => {
      const message = JSON.parse(String(event.data));
      if (message.id) {
        const pending = this.pending.get(message.id);
        if (!pending) return;
        this.pending.delete(message.id);
        if (message.error) pending.reject(new Error(message.error.message));
        else pending.resolve(message.result);
        return;
      }
      for (const listener of this.listeners.get(message.method) ?? []) {
        listener(message.params);
      }
    });
  }

  send(method, params = {}) {
    const id = this.next++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  on(method, listener) {
    const listeners = this.listeners.get(method) ?? [];
    listeners.push(listener);
    this.listeners.set(method, listeners);
  }

  close() {
    this.ws?.close();
  }
}

async function main() {
  const index = fs.readFileSync(path.join(UI, "dist/index.html"), "utf8")
    .replace("__PERSEA_STYLE_NONCE__", STYLE_NONCE);
  const documentRequests = [];
  const createRequests = [];
  const adoptionRequests = [];
  const previewRequests = [];
  const workspaceRequests = [];
  const workspaces = new Map();
  let workspaceRevision = 0;
  let unifiedCreated = false;
  let adopteeAdopted = false;
  const workspaceRecord = (name, tree, prior) => {
    workspaceRevision += 1;
    return {
      workspace_id: prior?.workspace_id ?? "f".repeat(32),
      name,
      normalized_name: name.toLowerCase(),
      revision: workspaceRevision,
      created_at: prior?.created_at ?? "2026-08-28T00:00:00Z",
      updated_at: "2026-08-28T00:00:01Z",
      tree,
    };
  };
  const currentInventory = () => {
    const inventory = JSON.parse(JSON.stringify(INVENTORY));
    const broker = inventory.realms[0].servers[0];
    // Missing Unified state leaves a visible, non-actionable row. Supported
    // adoption and opening paths are exercised below.
    broker.sessions.push({
      handles: ADOPTEE_HANDLE,
      realm: "local",
      server: "private",
      server_status: "ok",
      session_id: "$23",
      name: "adoptee",
      width: 120,
      height: 40,
      attached: 0,
      activity: 1_700_000_023,
      authority: ADOPTEE_AUTHORITY,
      unified: adopteeAdopted ? { state: "open", origin: "reconstructed", detail: "rotation_deferred_alt_screen" } : { state: "adoptable" },
    });
    broker.sessions.push({
      handles: BLOCKED_HANDLE,
      realm: "local",
      server: "private",
      server_status: "ok",
      session_id: "$31",
      name: "fullscreen",
      width: 120,
      height: 40,
      attached: 1,
      activity: 1_700_000_031,
      authority: BLOCKED_AUTHORITY,
      unified: { state: "blocked_alt_screen" },
    });
    if (!unifiedCreated) {
      broker.unified_dev = { state: "create", name: "unified-target" };
      return inventory;
    }
    // Once the target exists, the server-level projection carries no launch
    // affordance at all: the open row itself is the affordance.
    broker.sessions.push({
      handles: UNIFIED_HANDLE,
      realm: "local",
      server: "private",
      server_status: "ok",
      session_id: "$17",
      name: "unified-target",
      width: 120,
      height: 40,
      attached: 1,
      activity: 1_700_000_017,
      authority: UNIFIED_AUTHORITY,
      unified: { state: "open", origin: "birth" },
    });
    return inventory;
  };
  const server = http.createServer((request, response) => {
    const url = new URL(request.url, "http://localhost");
    if (request.method === "GET" && url.pathname === "/api/keyboard-preferences") {
      response.setHeader("Content-Type", "application/json"); response.setHeader("ETag", '"0"');
      response.end(JSON.stringify(defaultKeyboardRecord())); return;
    }
    if (request.method === "GET" && url.pathname === "/api/dashboard-preferences") {
      response.setHeader("Content-Type", "application/json"); response.setHeader("ETag", '"0"');
      response.end(JSON.stringify({ version: 1, favorites: [], revision: 0, available: true })); return;
    }
    if (request.method === "GET" && url.pathname === "/api/preferences") {
      response.setHeader("Content-Type", "application/json"); response.setHeader("ETag", '"0"');
      response.end(JSON.stringify({ version: 1, theme: "default", font_size: null, composer_font_size: 11, default_session: null, revision: 0, stored: false, available: true })); return;
    }
    if (request.method === "GET" && url.pathname === "/api/inventory") {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("Set-Cookie",
        `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(JSON.stringify(currentInventory()));
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/workspaces") {
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("Set-Cookie", `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(JSON.stringify({ version: 1, items: [...workspaces.values()] }));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/workspaces") {
      let body = "";
      request.setEncoding("utf8");
      request.on("data", (chunk) => { body += chunk; });
      request.on("end", () => {
        const parsed = JSON.parse(body);
        workspaceRequests.push({ method: "POST", body: parsed });
        const normalized = typeof parsed.name === "string" ? parsed.name.toLowerCase() : "";
        if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || parsed.version !== 1 || !parsed.tree) { response.writeHead(400); response.end("invalid"); return; }
        if (parsed.name === "Capacity Test") { response.writeHead(507); response.end("workspace store full"); return; }
        const current = workspaces.get(normalized);
        if (current) { response.setHeader("Content-Type", "application/json"); response.writeHead(409); response.end(JSON.stringify(current)); return; }
        const record = workspaceRecord(parsed.name, parsed.tree);
        workspaces.set(record.normalized_name, record);
        response.setHeader("Content-Type", "application/json"); response.writeHead(201); response.end(JSON.stringify(record));
      });
      return;
    }
    const workspaceMatch = url.pathname.match(/^\/api\/workspaces\/([0-9a-f]{32})$/);
    if ((request.method === "PUT" || request.method === "DELETE") && workspaceMatch) {
      let body = "";
      request.setEncoding("utf8");
      request.on("data", (chunk) => { body += chunk; });
      request.on("end", () => {
        const current = [...workspaces.values()].find((record) => record.workspace_id === workspaceMatch[1]);
        workspaceRequests.push({ method: request.method, body: body === "" ? undefined : JSON.parse(body), ifMatch: request.headers["if-match"] });
        if (request.headers["x-persea-csrf"] !== CSRF_TOKEN || !current) { response.writeHead(current ? 400 : 404); response.end("invalid"); return; }
        if (request.headers["if-match"] !== `"${current.revision}"`) { response.setHeader("Content-Type", "application/json"); response.writeHead(409); response.end(JSON.stringify(current)); return; }
        if (request.method === "DELETE") { workspaces.delete(current.normalized_name); response.writeHead(204); response.end(); return; }
        const parsed = JSON.parse(body);
        workspaces.delete(current.normalized_name);
        const record = workspaceRecord(parsed.name, parsed.tree, current);
        workspaces.set(record.normalized_name, record);
        response.setHeader("Content-Type", "application/json"); response.end(JSON.stringify(record));
      });
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/sessions") {
      let body = "";
      request.setEncoding("utf8");
      request.on("data", (chunk) => { body += chunk; });
      request.on("end", () => {
        const parsed = JSON.parse(body);
        createRequests.push(parsed);
        if (request.headers["x-persea-csrf"] !== CSRF_TOKEN
          || JSON.stringify(parsed) !== JSON.stringify({ realm: "local", server: "private", name: "unified-target" })) {
          response.writeHead(400);
          response.end("invalid");
          return;
        }
        unifiedCreated = true;
        response.setHeader("Content-Type", "application/json");
        response.writeHead(201);
        response.end(JSON.stringify(parsed));
      });
      return;
    }
    if (request.method === "GET" && url.pathname === "/api/session-previews") {
      previewRequests.push({
        realm: url.searchParams.get("realm"),
        server: url.searchParams.get("server"),
        session_id: url.searchParams.get("session_id"),
      });
      response.setHeader("Content-Type", "application/json");
      response.setHeader("Cache-Control", "no-store");
      response.end(JSON.stringify({
        realm: "local",
        server: "private",
        session_id: "$1",
        rows: [
          "PREVIEW_MARKER_ONE",
          "<img src=x onerror=window.__previewInjected=1>",
          "",
          "PREVIEW_MARKER_TWO",
        ],
        width: 120,
        height: 40,
        captured_at: 1_700_000_100_000,
        truncated: false,
      }));
      return;
    }
    if (request.method === "POST" && url.pathname === "/api/session-adoptions") {
      let body = "";
      request.setEncoding("utf8");
      request.on("data", (chunk) => { body += chunk; });
      request.on("end", () => {
        const parsed = JSON.parse(body);
        adoptionRequests.push(parsed);
        if (request.headers["x-persea-csrf"] !== CSRF_TOKEN
          || JSON.stringify(parsed) !== JSON.stringify({ realm: "local", server: "private", session_id: "$23", history_rows: 10000 })) {
          response.writeHead(400);
          response.end("invalid");
          return;
        }
        adopteeAdopted = true;
        response.setHeader("Content-Type", "application/json");
        response.writeHead(200);
        response.end(JSON.stringify(parsed));
      });
      return;
    }
    const files = {
      "/app.js": path.join(UI, "dist/app.js"),
      "/app.css": path.join(UI, "dist/app.css"),
      "/xterm.css": path.join(UI, "dist/xterm.css"),
      // E-P6 installable shell. index.html links these, so a fixture that does not
      // serve them makes the page 404 assets the front door always has
      // (`requiredBundleFiles` refuses to start a release without them). Same paths,
      // same media types (ruling J-EP6-1).
      "/manifest.webmanifest": path.join(UI, "dist/manifest.webmanifest"),
      "/icon-192.png": path.join(UI, "dist/icon-192.png"),
      "/icon-512.png": path.join(UI, "dist/icon-512.png"),
      "/apple-touch-icon.png": path.join(UI, "dist/apple-touch-icon.png"),
    };
    if ((url.pathname === "/" || url.pathname === "/terminal") && request.method === "GET") {
      documentRequests.push({ path: url.pathname, query: url.search });
      response.setHeader("Content-Type", "text/html");
      response.setHeader("Content-Security-Policy", CSP);
      response.setHeader("Set-Cookie",
        `__Host-persea-terminal-csrf=${CSRF_TOKEN}; Path=/; Secure; SameSite=Strict`);
      response.end(index);
      return;
    }
    const file = files[url.pathname];
    if (!file || request.method !== "GET") {
      response.writeHead(404);
      response.end("not found");
      return;
    }
    response.setHeader("Content-Type",
      file.endsWith(".css") ? "text/css"
        : file.endsWith(".png") ? "image/png"
          : file.endsWith(".webmanifest") ? "application/manifest+json"
            : "text/javascript");
    response.setHeader("Content-Security-Policy", CSP);
    response.end(fs.readFileSync(file));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const webPort = server.address().port;
  const origin = `http://127.0.0.1:${webPort}`;
  const debugPort = await freePort();
  const profile = fs.mkdtempSync(path.join(os.tmpdir(),
    "persea-terminal-r2a-navigation-"));
  const chrome = childProcess.spawn(browserBinary(), [
    "--headless=new",
    "--no-sandbox",
    "--disable-gpu",
    "--disable-dev-shm-usage",
    `--remote-debugging-port=${debugPort}`,
    `--user-data-dir=${profile}`,
    "--no-first-run",
    "--no-default-browser-check",
    "about:blank",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  const chromeStderr = [];
  let chromeExit = null;
  chrome.stderr.on("data", (chunk) => {
    if (chromeStderr.length < 64) chromeStderr.push(String(chunk));
  });
  chrome.on("exit", (code, signal) => { chromeExit = { code, signal }; });
  let cdp;
  try {
    let target;
    for (let attempt = 0; attempt < 300; attempt += 1) {
      if (chromeExit) break;
      try {
        target = await requestJSON(
          `http://127.0.0.1:${debugPort}/json/new?about:blank`,
          "PUT",
        );
        break;
      } catch {
        await delay(100);
      }
    }
    assert(target?.webSocketDebuggerUrl,
      `Chrome target unavailable: ${JSON.stringify({
        chromeExit,
        stderr: chromeStderr.join("").slice(-2_000),
      })}`);
    cdp = new CDP(target.webSocketDebuggerUrl);
    await cdp.open();
    await cdp.send("Runtime.enable");
    await cdp.send("Page.enable");
    await cdp.send("Network.enable");

    const evaluate = async (expression) => {
      const result = await cdp.send("Runtime.evaluate", {
        expression,
        awaitPromise: true,
        returnByValue: true,
      });
      if (result.exceptionDetails) {
        throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text || "unknown exception");
      }
      return result.result.value;
    };
    const waitFor = async (expression, timeoutMs = 5_000) => {
      const deadline = Date.now() + timeoutMs;
      while (Date.now() < deadline) {
        try {
          if (await evaluate(expression)) return true;
        } catch {
          // A full document navigation temporarily destroys the execution context.
        }
        await delay(25);
      }
      return false;
    };
    const navigate = async (suffix) => {
      const destination = origin + suffix;
      await cdp.send("Page.navigate", { url: destination });
      assert(await waitFor(`location.href === ${JSON.stringify(destination)}
        && document.readyState === "complete"`),
        `document did not complete navigation to ${suffix}`);
    };
    const waitForDashboard = async () => {
      assert(await waitFor("document.body.classList.contains('dashboard-mode')"
        + " && document.querySelector('.session-card') !== null"
        + " && document.querySelector('.workspace-panel__heading, .workspace-panel__failure') !== null"),
      "dashboard actions did not render");
    };
    const navigateTerminalDashboard = async () => {
      await navigate("/");
      await waitForDashboard();
      await navigate("/terminal");
      await waitForDashboard();
    };
    const state = () => evaluate(`({
      href: location.href,
      path: location.pathname,
      query: location.search,
      hash: location.hash,
      dashboard: document.body.classList.contains("dashboard-mode")
        && document.querySelector(".dashboard") !== null,
      terminal: document.querySelector(".attachment-page") !== null,
      lifecycle: document.querySelector("[data-terminal-lifecycle]")
        ?.getAttribute("data-terminal-lifecycle") ?? null,
      heading: document.querySelector("h1,h2,[role=heading]")?.textContent?.trim() ?? null,
      fatal: document.querySelector(".persea-terminal-fatal")?.textContent ?? null,
      text: document.body.textContent ?? ""
    })`);
    const trustedClick = async (targetAction) => {
      await cdp.send("Input.dispatchMouseEvent", {
        type: "mouseMoved",
        x: targetAction.x,
        y: targetAction.y,
      });
      await cdp.send("Input.dispatchMouseEvent", {
        type: "mousePressed",
        x: targetAction.x,
        y: targetAction.y,
        button: "left",
        clickCount: 1,
      });
      await cdp.send("Input.dispatchMouseEvent", {
        type: "mouseReleased",
        x: targetAction.x,
        y: targetAction.y,
        button: "left",
        clickCount: 1,
      });
    };
    await navigate("/");
    await waitForDashboard();
    assert(await waitFor(`document.querySelector(".workspace-panel__create") !== null`),
      "W2 dashboard workspace controls did not render");
    const w2Before = { create: createRequests.length, adoption: adoptionRequests.length };
    await evaluate(`(() => {
      const form = document.querySelector(".workspace-panel__create");
      const input = form?.querySelector('input[name="workspace_name"]');
      if (!(form instanceof HTMLFormElement) || !(input instanceof HTMLInputElement)) throw new Error("workspace create form absent");
      input.value = "Capacity Test";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      form.requestSubmit();
    })()`);
    assert(await waitFor(`/Delete an unused workspace/.test(document.querySelector(".workspace-panel__status")?.textContent ?? "")`),
      "W2 dashboard 507 did not render recoverable capacity copy");
    const capacityState = await evaluate(`({
      status: document.querySelector(".workspace-panel__status")?.textContent ?? "",
      draft: document.querySelector('.workspace-panel__create input[name="workspace_name"]')?.value ?? null,
      items: document.querySelectorAll(".workspace-panel__item").length,
    })`);
    assert(capacityState.draft === "Capacity Test" && capacityState.items === 0,
      `W2 dashboard 507 lost the draft or created a record: ${JSON.stringify(capacityState)}`);
    await evaluate(`(() => {
      const form = document.querySelector(".workspace-panel__create");
      const input = form?.querySelector('input[name="workspace_name"]');
      if (!(form instanceof HTMLFormElement) || !(input instanceof HTMLInputElement)) throw new Error("workspace create form absent");
      input.value = "Ops Workspace";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      form.requestSubmit();
    })()`);
    assert(await waitFor(`document.querySelector(".workspace-panel__item") !== null`),
      "W2 dashboard create did not render its durable record");
    const createdWorkspace = await evaluate(`(() => {
      const item = document.querySelector(".workspace-panel__item");
      const open = item?.querySelector(".workspace-panel__open");
      return { id: item?.dataset.workspaceId ?? null, name: open?.textContent ?? null, href: open?.href ?? null, revision: item?.querySelector(".workspace-panel__revision")?.textContent ?? null, renameHasMaxLength: item?.querySelector('.workspace-panel__rename input')?.hasAttribute("maxlength") ?? null };
    })()`);
    const createdURL = new URL(createdWorkspace.href);
    assert(createdWorkspace.id === "f".repeat(32)
      && createdWorkspace.name === "Ops Workspace"
      && createdWorkspace.revision === "r1"
      && createdWorkspace.renameHasMaxLength === false
      && createdURL.pathname === "/workspace"
      && createdURL.search === "?engine=unified-dev"
      && createdURL.hash === "#name=Ops+Workspace"
      && !createdWorkspace.href.includes("handle="),
    `W2 dashboard create/open route lost its authority-free canonical shape: ${JSON.stringify(createdWorkspace)}`);
    await evaluate(`(() => {
      const form = document.querySelector(".workspace-panel__rename");
      const input = form?.querySelector('input[name="name"]');
      if (!(form instanceof HTMLFormElement) || !(input instanceof HTMLInputElement)) throw new Error("workspace rename form absent");
      input.value = "Ops Renamed";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      form.requestSubmit();
    })()`);
    assert(await waitFor(`document.querySelector(".workspace-panel__open")?.textContent === "Ops Renamed"`),
      "W2 dashboard rename did not publish after its If-Match update");
    const renamedWorkspace = await evaluate(`({
      href: document.querySelector(".workspace-panel__open")?.href ?? null,
      revision: document.querySelector(".workspace-panel__revision")?.textContent ?? null,
    })`);
    assert(renamedWorkspace.revision === "r2" && new URL(renamedWorkspace.href).hash === "#name=Ops+Renamed",
      `W2 dashboard rename did not use the canonical URL: ${JSON.stringify(renamedWorkspace)}`);
    await evaluate(`document.querySelector(".workspace-panel__delete")?.click()`);
    assert(workspaceRequests.length === 3, "W2 first delete click must only ask for confirmation");
    await evaluate(`document.querySelector(".workspace-panel__confirmation .workspace-panel__delete")?.click()`);
    assert(await waitFor(`document.querySelector(".workspace-panel__item") === null`),
      "W2 dashboard delete did not remove its record");
    assert(workspaceRequests.length === 4
      && workspaceRequests.map((request) => request.method).join(",") === "POST,POST,PUT,DELETE"
      && workspaceRequests[2].ifMatch === '"1"'
      && workspaceRequests[3].ifMatch === '"2"'
      && !JSON.stringify(workspaceRequests).includes("handle")
      && createRequests.length === w2Before.create
      && adoptionRequests.length === w2Before.adoption,
    `W2 dashboard mutations changed authority or concurrency: ${JSON.stringify({ workspaceRequests, createRequests, adoptionRequests })}`);
    const workspaceDashboardEvidence = { createdWorkspace, renamedWorkspace, workspaceRequests: [...workspaceRequests] };

    const evidence = { workspaceDashboard: workspaceDashboardEvidence, documentRequests };

    // R2-C dashboard forcing fixture. Duplicate real rendered session cards so
    // content overflow is established independently of production inventory
    // size, then dispatch an actual CDP wheel gesture over dashboard content.
    await navigate("/");
    await waitForDashboard();
    await evaluate(`(() => {
      const grid = document.querySelector(".session-grid");
      const card = grid?.querySelector(".session-card");
      if (!(grid instanceof HTMLElement) || !(card instanceof HTMLElement)) {
        throw new Error("R2-C dashboard overflow fixture absent");
      }
      for (let index = 0; index < 32; index += 1) {
        const clone = card.cloneNode(true);
        if (!(clone instanceof HTMLElement)) throw new Error("R2-C session clone failed");
        clone.dataset.r2cOverflow = String(index);
        clone.querySelectorAll("[id]").forEach((node) => node.removeAttribute("id"));
        clone.querySelectorAll("[for]").forEach((node) => node.removeAttribute("for"));
        grid.append(clone);
      }
      window.__perseaR2CWheelEvents = [];
      window.addEventListener("wheel", (event) => {
        window.__perseaR2CWheelEvents.push({
          deltaY: event.deltaY,
          defaultPrevented: event.defaultPrevented,
          target: event.target instanceof Element
            ? event.target.closest(".dashboard")?.className ?? event.target.className
            : null,
        });
      }, { capture: true });
      if (${JSON.stringify(R2C_SCROLL_OMISSION_MUTANT)}) {
        const style = document.createElement("style");
        style.nonce = ${JSON.stringify(STYLE_NONCE)};
        style.dataset.r2cOmissionMutant = "true";
        style.textContent = [
          "html { height: 100% !important; min-height: 0 !important; overflow: hidden !important; overscroll-behavior: none !important; }",
          "body.dashboard-mode { height: 100% !important; min-height: 100% !important; overflow: auto !important; }",
          "body.dashboard-mode #app { width: auto !important; height: auto !important; min-height: 100% !important; overflow: hidden !important; overscroll-behavior: none !important; }",
        ].join("\\n");
        document.head.append(style);
      }
      if (${JSON.stringify(R2C_SCROLL_POSITIVE_CONTROL)}) {
        const style = document.createElement("style");
        style.nonce = ${JSON.stringify(STYLE_NONCE)};
        style.dataset.r2cPositiveControl = "true";
        style.textContent = [
          "html:has(body.dashboard-mode) { height: auto !important; min-height: 100% !important; overflow: auto !important; overscroll-behavior: auto !important; }",
          "body.dashboard-mode { height: auto !important; overflow: visible !important; }",
          "body.dashboard-mode #app { overflow: visible !important; overscroll-behavior: auto !important; }",
        ].join("\\n");
        document.head.append(style);
      }
      document.scrollingElement.scrollTop = 0;
      document.body.scrollTop = 0;
      document.querySelector("#app").scrollTop = 0;
    })()`);
    await evaluate("new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)))");
    const dashboardScrollBefore = await evaluate(`(() => {
      const root = document.scrollingElement;
      const html = document.documentElement;
      const body = document.body;
      const app = document.querySelector("#app");
      const target = document.querySelector(".session-card");
      if (!(root instanceof HTMLElement)
          || !(app instanceof HTMLElement)
          || !(target instanceof HTMLElement)) {
        throw new Error("R2-C dashboard scroll witness absent");
      }
      const rect = target.getBoundingClientRect();
      const x = Math.max(1, Math.min(innerWidth - 2, rect.left + Math.min(rect.width / 2, 80)));
      const y = Math.max(1, Math.min(innerHeight - 2, rect.top + Math.min(rect.height / 2, 80)));
      return {
        point: { x, y },
        owner: root.tagName,
        rootTop: root.scrollTop,
        rootClientHeight: root.clientHeight,
        rootScrollHeight: root.scrollHeight,
        htmlOverflowY: getComputedStyle(html).overflowY,
        bodyOverflowY: getComputedStyle(body).overflowY,
        bodyClientHeight: body.clientHeight,
        bodyScrollHeight: body.scrollHeight,
        appOverflowY: getComputedStyle(app).overflowY,
        appOverscrollY: getComputedStyle(app).overscrollBehaviorY,
        appClientHeight: app.clientHeight,
        appScrollHeight: app.scrollHeight,
        contentExtent: Math.max(body.scrollHeight, app.scrollHeight),
        viewportHeight: innerHeight,
        scrollbarWidth: innerWidth - html.clientWidth,
        hitInsideDashboard: document.elementFromPoint(x, y)?.closest(".dashboard") !== null,
        omissionMutant: document.querySelector("style[data-r2c-omission-mutant]") !== null,
        positiveControl: document.querySelector("style[data-r2c-positive-control]") !== null,
      };
    })()`);
    assert(dashboardScrollBefore.contentExtent
      > dashboardScrollBefore.viewportHeight + 500
      && dashboardScrollBefore.hitInsideDashboard,
    `R2-C positive control did not overflow real dashboard content: ${JSON.stringify(dashboardScrollBefore)}`);
    if (R2C_SCROLL_OMISSION_MUTANT) {
      assert(dashboardScrollBefore.omissionMutant
        && dashboardScrollBefore.htmlOverflowY === "hidden"
        && dashboardScrollBefore.bodyOverflowY === "auto"
        && dashboardScrollBefore.appOverflowY === "hidden"
        && dashboardScrollBefore.appOverscrollY === "none"
        && dashboardScrollBefore.bodyScrollHeight > dashboardScrollBefore.bodyClientHeight
        && dashboardScrollBefore.appScrollHeight === dashboardScrollBefore.appClientHeight,
      `R2-C omission mutant did not restore deployed ownership: ${JSON.stringify(dashboardScrollBefore)}`);
    }
    if (R2C_SCROLL_POSITIVE_CONTROL) {
      assert(dashboardScrollBefore.positiveControl
        && dashboardScrollBefore.owner === "HTML"
        && dashboardScrollBefore.htmlOverflowY === "auto"
        && dashboardScrollBefore.rootScrollHeight > dashboardScrollBefore.rootClientHeight,
      `R2-C positive control did not give the root overflowing content: ${JSON.stringify(dashboardScrollBefore)}`);
    }
    await cdp.send("Input.dispatchMouseEvent", {
      type: "mouseMoved",
      x: dashboardScrollBefore.point.x,
      y: dashboardScrollBefore.point.y,
    });
    await cdp.send("Input.dispatchMouseEvent", {
      type: "mouseWheel",
      x: dashboardScrollBefore.point.x,
      y: dashboardScrollBefore.point.y,
      deltaX: 0,
      deltaY: 500,
    });
    await delay(250);
    const dashboardScrollAfter = await evaluate(`(() => {
      const root = document.scrollingElement;
      return {
        rootTop: root?.scrollTop ?? null,
        bodyTop: document.body.scrollTop,
        appTop: document.querySelector("#app")?.scrollTop ?? null,
        windowY: window.scrollY,
        wheelEvents: window.__perseaR2CWheelEvents ?? [],
      };
    })()`);
    assert(dashboardScrollBefore.owner === "HTML"
      && dashboardScrollBefore.htmlOverflowY === "auto"
      && dashboardScrollBefore.rootScrollHeight > dashboardScrollBefore.rootClientHeight
      && dashboardScrollAfter.wheelEvents.length > 0
      && dashboardScrollAfter.wheelEvents.some((event) => event.deltaY === 500
        && event.defaultPrevented === false
        && typeof event.target === "string")
      && dashboardScrollAfter.rootTop > dashboardScrollBefore.rootTop
      && dashboardScrollAfter.windowY === dashboardScrollAfter.rootTop,
    "R2-C dashboard root did not consume a real wheel gesture over overflowing content: "
      + JSON.stringify({ before: dashboardScrollBefore, after: dashboardScrollAfter }));
    await evaluate("document.scrollingElement.scrollTop = 0");
    await cdp.send("Input.dispatchKeyEvent", {
      type: "keyDown",
      key: "PageDown",
      code: "PageDown",
      windowsVirtualKeyCode: 34,
      nativeVirtualKeyCode: 34,
    });
    await cdp.send("Input.dispatchKeyEvent", {
      type: "keyUp",
      key: "PageDown",
      code: "PageDown",
      windowsVirtualKeyCode: 34,
      nativeVirtualKeyCode: 34,
    });
    await delay(250);
    const dashboardScrollAffordances = await evaluate(`({
      rootTop: document.scrollingElement?.scrollTop ?? null,
      windowY: window.scrollY,
      scrollbarWidth: innerWidth - document.documentElement.clientWidth,
    })`);
    // PageDown's pixel step depends on the browser's actual viewport. Prove
    // a substantial root scroll and a stable visible scrollbar, not one
    // Chromium build's window-decoration dimensions.
    assert(dashboardScrollAffordances.rootTop >= dashboardScrollBefore.viewportHeight / 2
      && dashboardScrollAffordances.rootTop <= dashboardScrollBefore.viewportHeight
      && dashboardScrollAffordances.windowY === dashboardScrollAffordances.rootTop
      && dashboardScrollAffordances.scrollbarWidth > 0
      && dashboardScrollAffordances.scrollbarWidth === dashboardScrollBefore.scrollbarWidth,
    `R2-C dashboard root keyboard or scrollbar affordance changed: ${JSON.stringify({
      before: dashboardScrollBefore,
      affordances: dashboardScrollAffordances,
    })}`);
    evidence.r2c = {  dashboard: {
      before: dashboardScrollBefore,
      after: dashboardScrollAfter,
      affordances: dashboardScrollAffordances,
    } };

    // A blocked row explains its state and offers neither a dead Open action
    // nor the removed legacy actions.
    await navigate("/");
    await waitForDashboard();
    const blockedRow = await evaluate(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "fullscreen");
      if (!row) return null;
      const status = row.querySelector(".session-unified-status");
      return {
        statusText: status?.textContent ?? "",
        statusTitle: status?.getAttribute("title") ?? "",
        statusVisible: status ? getComputedStyle(status).display !== "none" : false,
        primaryActions: row.querySelectorAll(".session-open a, .session-open button").length,
        demotedControl: row.querySelector(".session-detail-actions a.action-control") !== null,
        demotedObserve: row.querySelector(".session-detail-actions a.action-observe") !== null,
      };
    })()`);
    assert(blockedRow
      && blockedRow.statusText.length > 0
      && blockedRow.statusTitle.length > 0
      && blockedRow.statusVisible === true
      && blockedRow.primaryActions === 0
      && blockedRow.demotedControl === false
      && blockedRow.demotedObserve === false,
    `blocked session did not render a visible reason without dead buttons: ${JSON.stringify(blockedRow)}`);

    // Visible wide-row thumbnails may load independently. Expanding a row
    // reuses its capture or loads it once, and the rows render as
    // TEXT (an HTML-looking row must never become markup), a manual refresh
    // fetches again; inventory refresh and collapsed rows stay quiet.
    const operatorPreviews = () => previewRequests.filter(request => request.session_id === "$1");
    assert(await waitFor(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "operator session");
      const button = row?.querySelector(".session-preview-row-button");
      const bounds = row?.querySelector(".session-preview-row-screen")?.getBoundingClientRect();
      const visible = document.visibilityState === "visible" && bounds && bounds.width > 0 && bounds.bottom >= 0 && bounds.top <= innerHeight;
      return button && button.getAttribute("aria-busy") !== "true" && (button.dataset.loaded === "true" || !visible);
    })()`), "the initial visible thumbnail did not finish loading");
    const operatorPreviewBaseline = operatorPreviews().length;
    const operatorPreviewWasLoaded = await evaluate(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "operator session");
      return row?.querySelector(".session-preview-row-button")?.dataset.loaded === "true";
    })()`);
    const loadedPreviewCount = operatorPreviewBaseline + (operatorPreviewWasLoaded ? 0 : 1);
    const waitForPreviewCount = async (count, timeoutMs = 5_000) => {
      const deadline = Date.now() + timeoutMs;
      while (Date.now() < deadline) {
        if (operatorPreviews().length === count) return true;
        await delay(25);
      }
      return false;
    };
    const operatorDisclosure = `(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "operator session");
      const button = row?.querySelector("button.session-disclosure");
      if (!(button instanceof HTMLButtonElement)) throw new Error("operator session disclosure absent");
      button.click();
      return true;
    })()`;
    await evaluate(operatorDisclosure);
    assert(await waitFor(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "operator session");
      const screen = row?.querySelector(".session-detail .session-preview-screen");
      return Boolean(screen
        && screen.textContent.includes("PREVIEW_MARKER_ONE")
        && screen.textContent.includes("PREVIEW_MARKER_TWO"));
    })()`), "expanding a row did not render its fetched preview rows");
    const previewRendering = await evaluate(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "operator session");
      const screen = row.querySelector(".session-detail .session-preview-screen");
      return {
        tag: screen.tagName,
        injectedNodes: [...screen.querySelectorAll("*")].filter(node => node.tagName !== "SPAN" || [...node.attributes].some(attr => attr.name !== "style")).length,
        injectedFlag: window.__previewInjected ?? null,
        literalMarkup: screen.textContent.includes("<img src=x onerror="),
        meta: row.querySelector(".session-preview-meta")?.textContent ?? "",
        refreshVisible: row.querySelector("button.session-preview-refresh") !== null,
      };
    })()`);
    assert(previewRendering
      && previewRendering.tag === "SPAN"
      && previewRendering.injectedNodes === 0
      && previewRendering.injectedFlag === null
      && previewRendering.literalMarkup === true
      && previewRendering.meta.includes("120×40")
      && previewRendering.refreshVisible === true,
    `preview rows were interpreted instead of rendered as text: ${JSON.stringify(previewRendering)}`);
    assert(operatorPreviews().length === loadedPreviewCount
      && JSON.stringify(operatorPreviews().at(-1)) === JSON.stringify({ realm: "local", server: "private", session_id: "$1" }),
    `preview changed its identity-only request: ${JSON.stringify(previewRequests)}`);
    await evaluate(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "operator session");
      row.querySelector("button.session-preview-refresh").click();
    })()`);
    assert(await waitForPreviewCount(loadedPreviewCount + 1),
      `manual preview refresh did not fetch again: ${JSON.stringify(previewRequests)}`);
    await evaluate(`document.querySelector("button.dashboard-refresh")?.click()`);
    await delay(400);
    assert(operatorPreviews().length === loadedPreviewCount + 1,
      `inventory refresh reloaded the cached preview: ${JSON.stringify(previewRequests)}`);
    assert(await waitFor(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "operator session");
      const screen = row?.querySelector(".session-detail .session-preview-screen");
      return Boolean(screen && screen.textContent.includes("PREVIEW_MARKER_ONE"));
    })()`), "the re-rendered expanded row lost its preview");
    await evaluate(operatorDisclosure);
    await evaluate(`document.querySelector("button.dashboard-refresh")?.click()`);
    await delay(400);
    assert(operatorPreviews().length === loadedPreviewCount + 1,
      `a closed row kept fetching previews: ${JSON.stringify(previewRequests)}`);
    evidence.sessionPreviews = { previewRequests, previewRendering };

    // One trusted click adopts the exact identity and opens this same tab.
    const pagesBeforeAdoption = (await requestJSON(`http://127.0.0.1:${debugPort}/json/list`)).filter(item => item.type === "page").length;
    const adoptAction = await evaluate(`(() => {
      const button = document.querySelector("button.action-unified-adopt");
      if (!(button instanceof HTMLButtonElement) || button.disabled) return null;
      button.scrollIntoView({ block: "center", inline: "center" });
      const rect = button.getBoundingClientRect();
      return { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 };
    })()`);
    assert(adoptAction, "adoptable session did not render its one-click action");
    await trustedClick(adoptAction);
    assert(await waitFor(`location.pathname === "/terminal" && location.search === "?engine=unified-dev"`), "adoption did not navigate this tab");
    const adoptedTarget = { url: (await state()).href };
    assert((await requestJSON(`http://127.0.0.1:${debugPort}/json/list`)).filter(item => item.type === "page").length === pagesBeforeAdoption, "adoption opened a popup");
    const adoptedURL = new URL(adoptedTarget.url);
    const adoptedFragment = new URLSearchParams(adoptedURL.hash.slice(1));
    assert(adoptedURL.pathname === "/terminal"
      && adoptedURL.search === "?engine=unified-dev"
      && adoptedFragment.get("engine") === "unified-dev"
      && adoptedFragment.get("handle") === ADOPTEE_HANDLE.control
      && adoptedFragment.get("mode") === "control"
      && adoptedFragment.get("name") === "adoptee",
    `adoption navigation lost selector or minted authority: ${adoptedTarget.url}`);
    assert(adoptionRequests.length === 1
      && JSON.stringify(adoptionRequests[0]) === JSON.stringify({ realm: "local", server: "private", session_id: "$23", history_rows: 10000 }),
    `adoption changed its identity or selected history: ${JSON.stringify(adoptionRequests)}`);
    // The refreshed row is open now: primary action becomes a plain same-tab
    // anchor and imported-history provenance stays available in Details.
    await navigate("/"); await waitForDashboard();
    assert(await waitFor(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "adoptee");
      return Boolean(row
        && row.querySelector("a.action-unified-open")
        && row.querySelector(".session-history-origin")?.textContent.includes("imported from tmux")
        && /rotation is deferred while a full-screen app is active/i.test(row.querySelector(".session-unified-status")?.textContent ?? ""));
    })()`), "adopted row did not flip to an open anchor with its imported-history explanation");

    // One explicit creation form replaces the development launcher. Creation
    // stays on the dashboard, then the row's ordinary Open action navigates.
    assert(await evaluate(`document.querySelector(".action-unified-dev, .session-legacy, .session-detail-actions") === null`), "a removed development or legacy entry point remains");
    await evaluate(`document.querySelector(".dashboard-create-shortcut").click()`);
    await evaluate(`(() => {
      const form = document.querySelector(".session-create");
      const select = form.querySelector("select"); select.value = JSON.stringify(["local", "private"]);
      const input = form.querySelector('input[name="name"]'); input.value = "unified-target"; input.dispatchEvent(new Event("input", { bubbles: true }));
    })()`);
    const unifiedAction = await evaluate(`(() => {
      const button = document.querySelector(".session-create button[type=submit]");
      if (!(button instanceof HTMLButtonElement) || button.disabled) return null;
      button.scrollIntoView({ block: "center", inline: "center" });
      const rect = button.getBoundingClientRect();
      return { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 };
    })()`);
    assert(unifiedAction, "creation form did not render enabled");
    await trustedClick(unifiedAction);
    assert(await waitFor(`document.querySelector(".session-create-status")?.textContent.includes("Created unified-target.")`), "creation did not finish");
    const dashboardAfterUnified = await state();
    assert(dashboardAfterUnified.dashboard === true && dashboardAfterUnified.path === "/",
      `unified launch replaced the dashboard: ${JSON.stringify(dashboardAfterUnified)}`);
    assert(createRequests.length === 1
      && JSON.stringify(createRequests[0]) === JSON.stringify({ realm: "local", server: "private", name: "unified-target" }),
    `unified launch changed create authority: ${JSON.stringify(createRequests)}`);

    // The new row carries the same one-click Open action as every session.
    await evaluate(`document.querySelector("button.dashboard-refresh")?.click()`);
    assert(await waitFor(`document.querySelector("button.action-unified-dev") === null`),
      "the create panel outlived its target");
    const reopenAnchor = await evaluate(`(() => {
      const row = [...document.querySelectorAll(".session-card")]
        .find((card) => card.querySelector(".session-name")?.textContent === "unified-target");
      const anchor = row?.querySelector("a.action-unified-open");
      if (!(anchor instanceof HTMLAnchorElement)) return null;
      anchor.scrollIntoView({ block: "center", inline: "center" });
      const rect = anchor.getBoundingClientRect();
      return {
        href: anchor.href,
        target: anchor.getAttribute("target"),
        x: rect.left + rect.width / 2,
        y: rect.top + rect.height / 2,
      };
    })()`);
    assert(reopenAnchor && reopenAnchor.target === null,
      `open row lost its same-tab one-click anchor: ${JSON.stringify(reopenAnchor)}`);
    const reopenURL = new URL(reopenAnchor.href);
    const reopenFragment = new URLSearchParams(reopenURL.hash.slice(1));
    assert(reopenURL.pathname === "/terminal"
      && reopenURL.search === "?engine=unified-dev"
      && reopenFragment.get("handle") === UNIFIED_HANDLE.control
      && reopenFragment.get("mode") === "control"
      && reopenFragment.get("engine") === "unified-dev",
    `open row anchor lost the unified URL invariants: ${JSON.stringify(reopenAnchor)}`);
    const reopenBeforeDocuments = documentRequests.length;
    await trustedClick(reopenAnchor);
    assert(await waitFor(`location.pathname === "/terminal" && location.search === "?engine=unified-dev"`),
      "clicking the open row did not navigate this tab to the unified page");
    await delay(250);
    const reopenDocuments = documentRequests
      .slice(reopenBeforeDocuments)
      .filter((item) => item.path === "/terminal" && item.query === "?engine=unified-dev").length;
    assert(reopenDocuments === 1 && createRequests.length === 1 && adoptionRequests.length === 1,
      `open-row navigation reloaded wrongly or re-ran creation/adoption: ${JSON.stringify({ reopenDocuments, createRequests, adoptionRequests })}`);

    evidence.sessionCreationAndOpen = { createRequests, adoptionRequests, dashboardAfterUnified, blockedRow, adopted: adoptedTarget.url, reopen: reopenAnchor };

    process.stdout.write(JSON.stringify({ status: "PASS", ...evidence }) + "\n");
  } finally {
    try {
      cdp?.close();
    } catch {}
    const waitForChromeExit = (timeoutMs) => new Promise((resolve) => {
      let timer = null;
      let settled = false;
      const finish = (exited) => {
        if (settled) return;
        settled = true;
        if (timer !== null) clearTimeout(timer);
        chrome.removeListener("exit", onExit);
        resolve(exited);
      };
      const onExit = () => finish(true);
      chrome.once("exit", onExit);
      if (chromeExit !== null) {
        finish(true);
        return;
      }
      timer = setTimeout(() => finish(false), timeoutMs);
    });
    chrome.kill("SIGTERM");
    if (!(await waitForChromeExit(5_000))) {
      chrome.kill("SIGKILL");
      await waitForChromeExit(5_000);
    }
    server.close();
    fs.rmSync(profile, {
      recursive: true,
      force: true,
      maxRetries: 10,
      retryDelay: 100,
    });
  }
}

main().catch((error) => {
  console.error(error.stack || String(error));
  process.exitCode = 1;
});
