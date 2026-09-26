// @ts-nocheck
// Real Chromium E2E-1 acceptance. This file deliberately launches the shipped
// front/broker binaries and a private tmux server; no transcript is fabricated.
export {};
declare const require: (name: string) => any;
declare const process: any;

const fs = require("node:fs");
const path = require("node:path");
const net = require("node:net");
const tls = require("node:tls");
const crypto = require("node:crypto");
const { spawn, spawnSync } = require("node:child_process");

const repo = process.env.PERSEA_E2E1_REPO_ROOT;
const artifactRoot = process.env.PERSEA_E2E1_ARTIFACT_ROOT;
const playwrightModule = process.env.PERSEA_PLAYWRIGHT_MODULE || require.resolve("playwright", { paths: [path.join(repo, "ui")] });
const chromePath = process.env.PERSEA_E2E1_CHROME || process.env.CHROME_BIN || require(playwrightModule).chromium.executablePath();
const refitOnly = process.env.PERSEA_E2E1_REFIT_ONLY === "1";
const alternateOnly = process.env.PERSEA_E2E1_ALTERNATE_ONLY === "1";
const slowLinkOnly = process.env.PERSEA_E2E1_SLOW_LINK_ONLY === "1";
const browserEngine = process.env.PERSEA_E2E1_ENGINE || "chromium";
if (!path.isAbsolute(repo ?? "") || !path.isAbsolute(artifactRoot ?? "") || !path.isAbsolute(playwrightModule ?? "")) {
  throw new Error("E2E1 requires absolute repo, artifact, and Playwright module paths");
}
if (!(["chromium", "webkit"] as string[]).includes(browserEngine) || (!refitOnly && browserEngine !== "chromium")) {
  throw new Error(`unsupported E2E1 browser mode ${browserEngine}`);
}

fs.mkdirSync(artifactRoot, { recursive: true, mode: 0o755 });
const transcriptPath = path.join(artifactRoot, "browser-transcript.jsonl");
const transcript = fs.createWriteStream(transcriptPath, { flags: "w", mode: 0o644 });
const children: any[] = [];
let proxy: any;
let browser: any;
let releaseRowGeometryGate: (() => Promise<void>) | undefined;
let proxyRequests = 0;
// The front door charges every request against one operator budget: a burst
// of 40 that refills at 10 a second (operatorBurst and operatorRefill in
// internal/frontdoor/security.go). Every request passes the trusted proxy,
// which follows the same budget, so a scenario can pace its cold page loads
// as the budget requires of a real operator.
const OPERATOR_BURST = 40;
const OPERATOR_REFILL_PER_MS = 10 / 1000;
const operatorBudget = { tokens: OPERATOR_BURST, at: 0 };
function operatorTokens(now = Date.now()): number {
  if (operatorBudget.at === 0) return OPERATOR_BURST;
  return Math.min(OPERATOR_BURST, operatorBudget.tokens + (now - operatorBudget.at) * OPERATOR_REFILL_PER_MS);
}
function chargeOperatorBudget(): void {
  const now = Date.now();
  const tokens = operatorTokens(now);
  // A refused request is not charged.
  operatorBudget.tokens = tokens >= 1 ? tokens - 1 : tokens;
  operatorBudget.at = now;
}
const tmp = fs.mkdtempSync("/tmp/persea-e2e1-browser-");
fs.chmodSync(tmp, 0o700);

function emit(type: string, value: unknown): void {
  transcript.write(`${JSON.stringify({ at: new Date().toISOString(), type, value })}\n`);
}

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

function sha256(data: any): string {
  return crypto.createHash("sha256").update(data).digest("hex");
}

function command(name: string, args: string[], cwd = repo, timeout = 120_000): string {
  const result = spawnSync(name, args, { cwd, encoding: "utf8", timeout, env: { ...process.env, TMPDIR: "/tmp" } });
  if (result.status !== 0) throw new Error(`${name} ${args.join(" ")} failed (${result.status}): ${(result.stderr || result.stdout || "").slice(-2000)}`);
  return result.stdout;
}

function start(name: string, args: string[], logName: string, cwd = repo): any {
  const log = fs.createWriteStream(path.join(artifactRoot, logName), { flags: "w", mode: 0o644 });
  const child = spawn(name, args, { cwd, detached: true, stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, TMPDIR: "/tmp" } });
  child.stdout.pipe(log);
  child.stderr.pipe(log);
  children.push({ child, log });
  return child;
}

async function until<T>(label: string, read: () => Promise<T> | T, timeout = 15_000): Promise<T> {
  const deadline = Date.now() + timeout;
  let last: unknown;
  while (Date.now() < deadline) {
    try {
      const value = await read();
      if (value) return value;
    } catch (error) { last = error; }
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error(`${label} timed out${last ? `: ${String(last)}` : ""}`);
}

function writeJSON(file: string, value: unknown): void {
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
}

// A shaped link stands in for a slow network path, for terminal WebSockets
// only: server-to-browser bytes leave at a fixed rate through a bounded
// bottleneck buffer, and both directions carry a fixed one-way delay. Page
// loads and API requests stay unshaped, so only the terminal stream pays for
// the link. The bottleneck stops reading from the front door once it is full,
// as a congested path stops acknowledging, so the front door's writes block
// instead of vanishing into proxy memory.
const shapedLink = { rate: 0, delayMs: 0, pairs: new Set<{ drop: () => void }>() };
const SHAPED_LINK_BUFFER_BYTES = 64 << 10;
const SHAPED_LINK_TICK_MS = 50;

function shapeLink(browserSocket: any, upstream: any): { push(chunk: any): void; send(chunk: any): void; end(): void } {
  const { rate, delayMs } = shapedLink;
  const queue: any[] = [];
  let queued = 0;
  let inAir = 0;
  let ended = false;
  const finish = () => { if (ended && queued === 0 && inAir === 0) browserSocket.end(); };
  const timer = setInterval(() => {
    let budget = Math.floor(rate * SHAPED_LINK_TICK_MS / 1000);
    while (budget > 0 && queue.length > 0) {
      const head = queue[0];
      const size = Math.min(budget, head.length);
      const slice = head.subarray(0, size);
      if (size === head.length) queue.shift(); else queue[0] = head.subarray(size);
      queued -= size;
      budget -= size;
      inAir += 1;
      setTimeout(() => { inAir -= 1; if (!browserSocket.destroyed) browserSocket.write(slice); finish(); }, delayMs);
    }
    if (queued < SHAPED_LINK_BUFFER_BYTES && upstream.isPaused()) upstream.resume();
  }, SHAPED_LINK_TICK_MS);
  // A drop is a network loss, not a close: both ends are reset at once.
  const pair = { drop: () => { browserSocket.resetAndDestroy(); upstream.destroy(); } };
  const stop = () => { clearInterval(timer); shapedLink.pairs.delete(pair); };
  shapedLink.pairs.add(pair);
  browserSocket.on("close", stop);
  upstream.on("close", stop);
  return {
    push(chunk: any) {
      queue.push(chunk);
      queued += chunk.length;
      if (queued >= SHAPED_LINK_BUFFER_BYTES) upstream.pause();
    },
    send(chunk: any) { setTimeout(() => { if (!upstream.destroyed) upstream.write(chunk); }, delayMs); },
    end() { ended = true; finish(); },
  };
}

function startTrustedProxy(frontSocket: string, canonicalHost: () => string, scheme: "http" | "https", tlsOptions?: Record<string, unknown>): any {
  const handler = (browserSocket: any) => {
    const upstream = net.createConnection(frontSocket);
    let pending = Buffer.alloc(0);
    let forwarded = false;
    let shaper: ReturnType<typeof shapeLink> | undefined;
    browserSocket.on("data", (chunk: any) => {
      if (forwarded) { if (shaper) shaper.send(chunk); else upstream.write(chunk); return; }
      pending = Buffer.concat([pending, chunk]);
      const boundary = pending.indexOf("\r\n\r\n");
      if (boundary < 0) return;
      const lines = pending.subarray(0, boundary).toString("latin1").split("\r\n");
      const request = lines.shift();
      const upgrade = lines.some((line: string) => /^upgrade:\s*websocket$/i.test(line));
      const kept = lines.filter((line: string) => !/^(host|connection|x-forwarded-host|x-forwarded-proto|tailscale-user-login):/i.test(line));
      const header = [request, "Host: localhost", `X-Forwarded-Host: ${canonicalHost()}`, `X-Forwarded-Proto: ${scheme}`, "Tailscale-User-Login: operator@example.test", `Connection: ${upgrade ? "Upgrade" : "close"}`, ...kept, "", ""].join("\r\n");
      chargeOperatorBudget();
      if (proxyRequests++ < 5) emit("trusted-proxy-request", { request, authority: canonicalHost(), headers: header.split("\r\n").slice(0, 10) });
      if (upgrade && shapedLink.rate > 0) shaper = shapeLink(browserSocket, upstream);
      const forwardedRequest = Buffer.concat([Buffer.from(header, "latin1"), pending.subarray(boundary + 4)]);
      if (shaper) shaper.send(forwardedRequest); else upstream.write(forwardedRequest);
      pending = Buffer.alloc(0);
      forwarded = true;
    });
    upstream.on("data", (chunk: any) => { if (shaper) shaper.push(chunk); else browserSocket.write(chunk); });
    upstream.on("end", () => { if (shaper) shaper.end(); else browserSocket.end(); });
    browserSocket.on("end", () => upstream.end());
    browserSocket.on("error", () => upstream.destroy());
    upstream.on("error", () => browserSocket.destroy());
  };
  return tlsOptions ? tls.createServer(tlsOptions, handler) : net.createServer(handler);
}

async function main(): Promise<void> {
  const uid = Number(process.getuid());
  const tmuxSocket = path.join(tmp, "tmux.sock");
  const brokerSocket = path.join(tmp, "broker.sock");
  const frontSocket = path.join(tmp, "front.sock");
  const runtimeDir = path.join(tmp, "journal");
  fs.mkdirSync(runtimeDir, { mode: 0o700 });
  command("tmux", ["-S", tmuxSocket, "-f", "/dev/null", "new-session", "-d", "-s", "unified_anchor", "-c", tmp]);
  const siblingNames = refitOnly ? Array.from({ length: 5 }, (_, index) => `refit_sibling_${index + 1}`) : [];
  for (const sibling of siblingNames) {
    command("tmux", ["-S", tmuxSocket, "-f", "/dev/null", "new-session", "-d", "-s", sibling, "-c", tmp, "sh", "-c", `printf '${sibling}-UNCHANGED\\n'; while :; do sleep 1; done`]);
  }
  const siblingState = (): Record<string, string> => Object.fromEntries(siblingNames.map((sibling) => [sibling,
    command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", `=${sibling}:`, "#{session_id}|#{window_id}|#{pane_id}|#{pane_width}x#{pane_height}|#{pane_pid}|#{history_bytes}"]).trim(),
  ]));
  const siblingsBefore = siblingState();

  let proxyTLS: Record<string, unknown> | undefined;
  if (browserEngine === "webkit") {
    const key = path.join(tmp, "proxy.key");
    const cert = path.join(tmp, "proxy.crt");
    command("/usr/bin/openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost", "-days", "1", "-keyout", key, "-out", cert]);
    proxyTLS = { key: fs.readFileSync(key), cert: fs.readFileSync(cert) };
  }
  const proxyScheme = browserEngine === "webkit" ? "https" : "http";
  proxy = startTrustedProxy(frontSocket, () => canonicalHost, proxyScheme, proxyTLS);
  await new Promise<void>((resolve, reject) => {
    proxy.once("error", reject);
    proxy.listen(0, "127.0.0.1", resolve);
  });
  const address = proxy.address();
  assert(address && typeof address === "object", "proxy did not bind TCP");
  const canonicalHost = `localhost:${address.port}`;
  const base = `${proxyScheme}://${canonicalHost}`;

  const brokerConfig = path.join(tmp, "broker.json");
  writeJSON(brokerConfig, {
    realm: "e2e1", front_uid: uid,
    servers: [{ label: "main", socket_path: tmuxSocket }],
    session_create: { enabled: true, servers: ["main"], name_pattern: "^unified_target$", max_sessions: refitOnly ? 8 : 4, start_directory: tmp, columns: refitOnly ? 120 : 80, rows: refitOnly ? 40 : 24 },
    unified_terminal_dev: { enabled: true, server: "main", session: "unified_target", observer_session: "unified_anchor", runtime_dir: runtimeDir },
  });
  const frontConfig = path.join(tmp, "front.json");
  writeJSON(frontConfig, {
    ingress: { socket_path: frontSocket, peer_uid: uid, canonical_host: canonicalHost, operator_login: "operator@example.test", max_connections: 64, ...(browserEngine === "webkit" ? { hermetic_tls: true } : {}) },
    realms: [{ name: "e2e1", display_name: "E2E1", socket: brokerSocket, broker_uid: uid }],
    aliases: [], alias_store_path: path.join(tmp, "aliases.json"), handle_ttl_seconds: 120, handle_capacity: 256,
    workspace_store_path: path.join(tmp, "workspaces.json"),
  });

  const binary = path.join(artifactRoot, "persea-terminal-e2e1");
  command("go", ["build", "-o", binary, "./cmd/persea-terminal"], repo, 180_000);
  command("npm", ["run", "build"], path.join(repo, "ui"), 120_000);
  start(binary, ["broker", "--socket", brokerSocket, "--config", brokerConfig], "broker.log");
  await until("broker socket", () => fs.existsSync(brokerSocket));
  start(binary, ["front", "--config", frontConfig, "--static-dir", "ui/dist"], "front.log");
  await until("front socket", () => fs.existsSync(frontSocket));

  const playwright = require(playwrightModule);
  browser = await playwright[browserEngine].launch(browserEngine === "chromium"
    ? { headless: true, executablePath: chromePath, args: ["--no-sandbox", "--disable-dev-shm-usage"] }
    : { headless: true });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 2, hasTouch: true, ignoreHTTPSErrors: browserEngine === "webkit" });
  if (browserEngine === "chromium") await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin: base });
  const page = await context.newPage();
  releaseRowGeometryGate = () => page.evaluate(() => (window as any).__releaseRowGeometry?.());
  if (refitOnly) await page.addInitScript(() => {
    // Hold the real committed row geometry, and every later message, so the
    // form's next focus transfer deterministically precedes acknowledgement.
    const NativeWebSocket = window.WebSocket;
    window.WebSocket = class extends NativeWebSocket {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        const held: string[] = [];
        let released = false;
        this.addEventListener("message", (event) => {
          if (released) return;
          let resize = false;
          try { const frame = JSON.parse(String(event.data)); resize = frame.type === "PREPARE" && frame.kind === "RESIZE"; } catch { /* Other wire messages pass through. */ }
          if (!resize && held.length === 0) return;
          event.stopImmediatePropagation();
          held.push(event.data);
          (window as any).__releaseRowGeometry = () => {
            released = true;
            for (const data of held.splice(0)) this.dispatchEvent(new MessageEvent("message", { data }));
            delete (window as any).__releaseRowGeometry;
          };
        });
      }
    };
  });
  const pageErrors: string[] = [];
  const consoleErrors: string[] = [];
  const consoleMessages: Array<{ type: string; text: string }> = [];
  const socketEvents: Array<{ direction: string; bytes: number; payload: string }> = [];
  const httpErrors: Array<Record<string, unknown>> = [];
  const refitResponses: Array<{ status: number; body: string }> = [];
  let topLevelNavigations = 0;
  let terminalWebSockets = 0;
  page.on("framenavigated", (frame: any) => { if (frame === page.mainFrame()) topLevelNavigations += 1; });
  page.on("pageerror", (error: Error) => pageErrors.push(error.message));
  page.on("console", (event: any) => {
    consoleMessages.push({ type: event.type(), text: event.text() });
    if (event.type() === "error") consoleErrors.push(event.text());
  });
  page.on("websocket", (socket: any) => {
    terminalWebSockets += 1;
    socket.on("framesent", (event: any) => {
      const payload = typeof event.payload === "string" ? event.payload : Buffer.from(event.payload).toString("utf8");
      socketEvents.push({ direction: "sent", bytes: Buffer.byteLength(payload), payload: payload.slice(0, 500) });
    });
    socket.on("framereceived", (event: any) => {
      const payload = typeof event.payload === "string" ? event.payload : Buffer.from(event.payload).toString("utf8");
      socketEvents.push({ direction: "received", bytes: Buffer.byteLength(payload), payload: payload.slice(0, 500) });
    });
  });
  const cdp = browserEngine === "chromium" ? await context.newCDPSession(page) : null;
  const requestInitiators = new Map<string, unknown>();
  if (cdp) await cdp.send("Network.enable");
  cdp?.on("Network.requestWillBeSent", (event: any) => {
    const initiator = event.initiator ?? {};
    requestInitiators.set(event.requestId, {
      type: initiator.type ?? "",
      url: initiator.url ?? "",
      lineNumber: initiator.lineNumber ?? -1,
      columnNumber: initiator.columnNumber ?? -1,
      stack: (initiator.stack?.callFrames ?? []).slice(0, 4).map((frame: any) => ({ url: frame.url, lineNumber: frame.lineNumber, columnNumber: frame.columnNumber, functionName: frame.functionName })),
    });
  });
  cdp?.on("Network.responseReceived", (event: any) => {
    if (event.response.status < 400) return;
    const observed = {
      url: event.response.url,
      status: event.response.status,
      mimeType: event.response.mimeType,
      resourceType: event.type,
      initiator: requestInitiators.get(event.requestId) ?? null,
    };
    httpErrors.push(observed);
    emit("http-error", observed);
  });
  if (!cdp) page.on("response", (response: any) => {
    if (response.status() < 400) return;
    const observed = { url: response.url(), status: response.status(), resourceType: response.request().resourceType() };
    httpErrors.push(observed);
    emit("http-error", observed);
  });

  await page.goto(base, { waitUntil: "domcontentloaded" });
  if (alternateOnly) {
    // The dashboard must adopt an already running full-screen program. The
    // command waits without reading input; opening must neither type nor resize.
    command("tmux", ["-S", tmuxSocket, "new-session", "-d", "-s", "unified_target", "-x", "80", "-y", "24",
      "sh", "-c", "printf 'NORMAL-BEFORE\\r\\n\\033[?1049h\\033[2J\\033[HFULLSCREEN-READY'; exec sleep 600"]);
    await until("alternate screen active", () => command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{alternate_on}"]).trim() === "1");
    const witness = () => command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}|#{alternate_on}|#{cursor_x}|#{cursor_y}"]).trim();
    const before = witness();
    for (const viewport of [{ width: 320, height: 568 }, { width: 390, height: 844 }, { width: 430, height: 932 }, { width: 844, height: 390 }]) {
      // Each fresh dashboard/terminal navigation consumes the real shared
      // request budget. Allow its 40-token burst to refill between scenarios.
      await page.waitForTimeout(4100);
      await page.setViewportSize(viewport);
      await page.goto(base, { waitUntil: "domcontentloaded" });
      const row = page.locator(".session-card").filter({ has: page.getByRole("heading", { name: "unified_target", exact: true }) });
      const action = row.locator("button.action-unified-adopt, a.action-unified-open");
      try { await action.waitFor({ state: "visible", timeout: 10000 }); } catch (error) {
        throw new Error(`alternate Open unavailable: ${await page.locator("body").innerText()} inventory=${JSON.stringify(await page.evaluate(async () => (await fetch("/api/inventory")).json()))}: ${error}`);
      }
      assert(await action.isEnabled(), "full-screen session has no enabled Open action");
      await action.click();
      await page.waitForURL(url => url.pathname === "/terminal");
      await page.waitForSelector(".persea-unified-terminal .xterm");
      await until("alternate display replayed", () => page.locator(".xterm-rows").innerText().then(text => text.includes("FULLSCREEN-READY")));
      assert(witness() === before, `opening changed pane state at ${JSON.stringify(viewport)}`);
    }
    assert(socketEvents.filter(event => event.direction === "sent" && /"type":"(?:INPUT|RESIZE_REQUEST)"/.test(event.payload)).length === 0, "opening sent input or a resize request");
    assert(pageErrors.length === 0 && consoleMessages.length === 0 && httpErrors.length === 0, `alternate browser findings: ${JSON.stringify({ pageErrors, consoleMessages, httpErrors })}`);
    emit("pass", { scenario: "dashboard-alternate-screen", viewports: 4, witness: before });
    console.log("Dashboard alternate-screen adoption and reopen passed at four phone viewports");
    return;
  }
  const csrfCookie = (await context.cookies(base)).find((cookie: any) => cookie.name === "__Host-persea-terminal-csrf");
  assert(csrfCookie?.value, "CSRF cookie missing");
  const created = await page.evaluate(async (csrf: string) => {
    const response = await fetch("/api/sessions", { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json", "X-Persea-CSRF": csrf }, body: JSON.stringify({ realm: "e2e1", server: "main", name: "unified_target" }) });
    return { status: response.status, body: await response.text() };
  }, csrfCookie.value);
  assert(created.status === 201, `session creation failed: ${JSON.stringify(created)}`);

  const handle = await until<string>("target inventory", () => page.evaluate(async () => {
    const inventory = await (await fetch("/api/inventory", { cache: "no-store" })).json();
    for (const realm of inventory.realms ?? []) for (const server of realm.servers ?? []) for (const session of server.sessions ?? []) {
      if (session.name === "unified_target") return session.handles?.control ?? "";
    }
    return "";
  }));
  await page.goto(`${base}/terminal#handle=${encodeURIComponent(handle)}&mode=control&history=0&engine=unified-dev`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector(".persea-unified-terminal .xterm");
  await until("initial staged reload capability", () => page.evaluate(() => /^[A-Za-z0-9_-]{43}$/.test(window.sessionStorage.getItem("persea-unified-terminal-reload-handle-v1") ?? "")));
  if (slowLinkOnly) {
    // Slow-link acceptance. A session whose journal holds more than a MiB is
    // opened, used, kept open under steady output, and reconnected through a
    // link as slow as a poor mobile connection. Admission must stay bounded,
    // input must wait until the browser has caught up, no stage may loop, and
    // every liveness round trip must fit the page's window.
    const target = "=unified_target:";
    const LIVENESS_WINDOW_MS = 10_000; // the page's liveness challenge window
    const ATTEMPT_DEADLINE_MS = 5_000; // the page's attempt deadline and the broker's cut timer
    const HISTORY_BYTES = 1 << 20;
    const STEADY_MS = 30_000;
    const LINK_DELAY_MS = 150;
    const filler = "x".repeat(64);
    const xterm = page.locator(".persea-unified-xterm");
    const capture = () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-S", "-200", "-t", target]);
    const steadyIn = (text: string) => Math.max(0, ...[...text.matchAll(/STEADY-(\d{6})/g)].map((match) => Number(match[1])));
    const pageState = () => page.evaluate(() => ({
      phase: document.querySelector<HTMLElement>(".persea-unified-tag__dot")?.dataset.state ?? "",
      notice: document.querySelector<HTMLElement>(".persea-unified-notice")?.hidden === false,
      connection: document.querySelector(".persea-unified-connection")?.textContent ?? "",
      rows: document.querySelector<HTMLElement>(".xterm-rows")?.innerText ?? "",
    }));
    type Attachment = {
      opened: number; closed: number; commitAt: number; modeAt: number;
      bytesBeforeMode: number; framesBeforeMode: number; inputsBeforeMode: number; acks: number;
      pings: Map<string, number>; rtts: number[]; refusals: string[];
    };
    const attachments: Attachment[] = [];
    page.on("websocket", (socket: any) => {
      if (new URL(socket.url()).pathname !== "/ws") return;
      const record: Attachment = { opened: Date.now(), closed: 0, commitAt: 0, modeAt: 0, bytesBeforeMode: 0, framesBeforeMode: 0, inputsBeforeMode: 0, acks: 0, pings: new Map(), rtts: [], refusals: [] };
      attachments.push(record);
      const text = (event: any) => typeof event.payload === "string" ? event.payload : Buffer.from(event.payload).toString("utf8");
      socket.on("close", () => { record.closed = Date.now(); });
      socket.on("framesent", (event: any) => {
        const payload = text(event);
        if (payload.startsWith("PERSEA-LIVENESS/1 PING ")) record.pings.set(payload.slice(23), Date.now());
        else if (payload.startsWith("PERSEA-FLOW/1 ACK ")) record.acks += 1;
        else if (!record.modeAt && payload.includes('"type":"INPUT"')) record.inputsBeforeMode += 1;
      });
      socket.on("framereceived", (event: any) => {
        const payload = text(event);
        const now = Date.now();
        if (payload.startsWith("PERSEA-LIVENESS/1 PONG ")) {
          const nonce = payload.slice(23);
          const sent = record.pings.get(nonce);
          if (sent !== undefined) { record.rtts.push(now - sent); record.pings.delete(nonce); }
          return;
        }
        if (payload.startsWith("PERSEA-")) { record.refusals.push(payload.slice(0, 120)); return; }
        const frame = JSON.parse(payload);
        if (!record.modeAt) {
          const encoded = frame.type === "PREPARE" ? frame.replay : frame.type === "LIVE" ? frame.data : "";
          record.bytesBeforeMode += encoded ? Buffer.from(encoded, "base64").length : 0;
          record.framesBeforeMode += 1;
        }
        if (frame.type === "COMMIT" && !record.commitAt) record.commitAt = now;
        if (frame.type === "MODE" && frame.mode === "CONTROL" && !record.modeAt) record.modeAt = now;
      });
    });
    const liveness = (stage: string, record: Attachment) => {
      const unanswered = [...record.pings.values()].filter((sent) => Date.now() - sent >= LIVENESS_WINDOW_MS);
      assert(record.rtts.length > 0 && unanswered.length === 0 && Math.max(...record.rtts) < LIVENESS_WINDOW_MS,
        `${stage}: liveness round trips ${JSON.stringify(record.rtts)} with ${unanswered.length} unanswered past the ${LIVENESS_WINDOW_MS} ms window`);
    };

    // More than a MiB of history, recorded while the unshaped page is attached,
    // then steady output that keeps running through every later stage.
    await until("initial attachment live", async () => (await pageState()).phase === "live");
    command("tmux", ["-S", tmuxSocket, "send-keys", "-t", target, `seq -w 1 16000 | sed 's/.*/HIST-&-${filler}/'; printf 'HIST-%s\\n' DONE`, "Enter"]);
    await until("history recorded", async () => (await pageState()).rows.includes("HIST-DONE"), 60_000);
    command("tmux", ["-S", tmuxSocket, "send-keys", "-t", target, `(i=0; while :; do for j in 1 2 3 4 5; do i=$((i+1)); printf 'STEADY-%06d-%s\\n' "$i" ${filler}; done; sleep 0.2; done) &`, "Enter"]);
    await until("steady output", () => steadyIn(capture()) > 0);

    // Each stage waits for the attachment the page opens next, measured from
    // the moment its cause (navigation or link loss) happened.
    const attach = async (stage: string, index: number, causedAt: number, rate: number) => {
      const record = await until(`${stage}: terminal socket`, () => attachments[index], 20_000);
      await until(`${stage}: COMMIT`, () => record.commitAt, 20_000);
      // Admission no longer scales with history: it fits the attempt deadline
      // even on this link.
      assert(record.commitAt - record.opened < ATTEMPT_DEADLINE_MS, `${stage}: admission took ${record.commitAt - record.opened} ms`);
      // The backlog is still streaming, so input is sealed: a key typed now
      // must be dropped, never sent, and the page says why.
      if (!record.modeAt) {
        await xterm.focus();
        await page.keyboard.type("z");
        if (!record.modeAt) {
          await until(`${stage}: dropped key explained`, () => page.evaluate(() => document.querySelector(".persea-unified-refusal:not([hidden])")?.textContent === "Input not sent — history is still loading"), 2_000);
        }
      }
      await until(`${stage}: control grant`, () => record.modeAt, Math.ceil(2 * (HISTORY_BYTES * 4 / 3) / rate * 1000) + 60_000);
      assert(record.bytesBeforeMode >= HISTORY_BYTES, `${stage}: only ${record.bytesBeforeMode} bytes of history preceded the control grant`);
      assert(record.inputsBeforeMode === 0, `${stage}: ${record.inputsBeforeMode} input frames left before the control grant`);
      // Flow control must not starve the link: the base64 backlog moves at no
      // less than half the link rate.
      const backlogMs = record.modeAt - record.commitAt;
      const linkMs = record.bytesBeforeMode * 4 / 3 / rate * 1000;
      assert(backlogMs <= 2 * linkMs + 5_000, `${stage}: the backlog took ${backlogMs} ms where the link needs ${Math.round(linkMs)} ms`);
      await until(`${stage}: live`, async () => (await pageState()).phase === "live");
      // Input authority arrived with the browser caught up: a command typed
      // now runs. Control-U clears the probe key if the grant raced it.
      const marker = `${stage.replace(/[^a-z0-9]+/gi, "-")}-${rate}`;
      await xterm.focus();
      await page.keyboard.press("Control+U");
      await page.keyboard.type(`printf 'MARK-%s\\n' ${marker}`);
      await page.keyboard.press("Enter");
      await until(`${stage}: typed command ran`, () => capture().includes(`MARK-${marker}`), 20_000);
      const measured = {
        stage, rate, linkDelayMs: LINK_DELAY_MS,
        toCommitMs: record.commitAt - causedAt, toControlMs: record.modeAt - causedAt, admissionMs: record.commitAt - record.opened, backlogMs,
        bytesBeforeControl: record.bytesBeforeMode, framesBeforeControl: record.framesBeforeMode, acknowledgements: record.acks,
        livenessRoundTripsMs: [...record.rtts],
      };
      emit("slow-link-stage", measured);
      return { record, measured };
    };

    // Steady output for a while: no reconnect, the session stays live, and
    // liveness keeps answering. Output that queued behind the backlog drains
    // meanwhile, so at the end the page must show current output, not merely
    // hold an open socket.
    const steady = async (stage: string, record: Attachment) => {
      const count = attachments.length;
      const startedAt = Date.now();
      while (Date.now() - startedAt < STEADY_MS) {
        const state = await pageState();
        assert(state.phase === "live" && !state.notice && state.connection === "", `${stage}: left live after ${Date.now() - startedAt} ms: ${JSON.stringify({ ...state, rows: undefined })}`);
        assert(attachments.length === count && record.closed === 0, `${stage}: reconnected after ${Date.now() - startedAt} ms`);
        await page.waitForTimeout(250);
      }
      const produced = steadyIn(capture());
      await until(`${stage}: page shows current output`, async () => steadyIn((await pageState()).rows) >= produced, 5_000);
      liveness(stage, record);
      assert(record.refusals.length === 0, `${stage}: refusals ${JSON.stringify(record.refusals)}`);
    };

    const measurements: unknown[] = [];
    for (const rate of [32 << 10, 256 << 10]) {
      // A fresh page load through the link. Cold dashboard and terminal loads
      // take most of the operator budget, and the first ones came just before
      // this, so wait until the budget is whole again.
      await until("operator request budget refilled", () => operatorTokens() === OPERATOR_BURST);
      await page.goto(base, { waitUntil: "domcontentloaded" });
      const handle = await until<string>("fresh control handle", () => page.evaluate(async () => {
        const inventory = await (await fetch("/api/inventory", { cache: "no-store" })).json();
        for (const realm of inventory.realms ?? []) for (const server of realm.servers ?? []) for (const session of server.sessions ?? []) {
          if (session.name === "unified_target") return session.handles?.control ?? "";
        }
        return "";
      }));
      shapedLink.rate = rate;
      shapedLink.delayMs = LINK_DELAY_MS;
      const openIndex = attachments.length;
      const navigatedAt = Date.now();
      await page.goto(`${base}/terminal#handle=${encodeURIComponent(handle)}&mode=control&history=0&engine=unified-dev`, { waitUntil: "domcontentloaded" });
      const opened = await attach("open", openIndex, navigatedAt, rate);
      await steady(`steady after open at ${rate}`, opened.record);
      measurements.push({ ...opened.measured, livenessRoundTripsMs: [...opened.record.rtts] });

      // The link drops; the page reconnects by itself, once, through the same
      // slow link.
      assert(shapedLink.pairs.size === 1, `expected one shaped terminal link, found ${shapedLink.pairs.size}`);
      const reconnectIndex = attachments.length;
      const droppedAt = Date.now();
      for (const pair of [...shapedLink.pairs]) pair.drop();
      const reconnected = await attach("reconnect", reconnectIndex, droppedAt, rate);
      await steady(`steady after reconnect at ${rate}`, reconnected.record);
      assert(attachments.length === reconnectIndex + 1, `reconnect at ${rate} opened ${attachments.length - reconnectIndex} attachments`);
      measurements.push({ ...reconnected.measured, livenessRoundTripsMs: [...reconnected.record.rtts] });
      shapedLink.rate = 0;
    }
    assert(pageErrors.length === 0 && consoleMessages.length === 0 && httpErrors.length === 0, `slow-link browser findings: ${JSON.stringify({ pageErrors, consoleMessages, httpErrors })}`);
    emit("pass", { scenario: "slow-link", measurements });
    console.log(`Slow-link private stack passed: ${JSON.stringify(measurements)}`);
    return;
  }
  await page.waitForTimeout(150);
  const terminal = page.locator(".persea-unified-xterm");
  await terminal.focus();
  const beforeShell = await page.screenshot({ path: path.join(artifactRoot, "00-before-shell.png") });
  await page.keyboard.type("printf 'E2E1-BROWSER-SHELL\\n'");
  await page.keyboard.press("Enter");
  await until("shell echo", () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-t", "=unified_target:"]).includes("E2E1-BROWSER-SHELL"));
  await page.waitForTimeout(150);
  const shellShot = await page.screenshot({ path: path.join(artifactRoot, "01-shell-roundtrip.png") });
  if (!refitOnly) assert(sha256(beforeShell) !== sha256(shellShot), "shell output was not visibly rendered");

  // terminal touch width-refit closure: the handle was minted from the refreshed
  // dashboard inventory after creation and the attachment has committed its
  // broker PREPARE source. Exercise the real front HTTP endpoint twice so a
  // successful generation boundary must re-mint before the second request.
  const refitWidth = async (columns: number): Promise<void> => {
    const before = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
    const rows = Number(before.split("x")[1]);
    const disclosure = page.locator(".persea-unified-view-disclosure");
    if (await disclosure.getAttribute("aria-expanded") !== "true") await disclosure.click();
    const field = page.getByRole("textbox", { name: "Columns", exact: true });
    await field.fill(String(columns));
    if (refitOnly && refitResponses.length === 0) {
      try {
        assert(await page.getByRole("textbox", { name: "Rows", exact: true }).inputValue() === String(rows), "pending row Apply lost its submitted Rows during Columns focus");
        assert(await disclosure.textContent() === "120×40", "row acknowledgement escaped the ordered test gate");
        assert(await page.getByRole("button", { name: "Applying…", exact: true }).getAttribute("aria-disabled") === "true", "geometry input seal released before acknowledgement");
      } finally {
        await page.evaluate(() => (window as any).__releaseRowGeometry?.());
      }
      await until("pre-refit browser row commit", async () => (await disclosure.textContent()) === `120×${rows}`);
    }
    const formBeforeApply = { columns: await field.inputValue(), rows: await page.getByRole("textbox", { name: "Rows", exact: true }).inputValue(), committed: await disclosure.textContent() };
    // one Apply reads both fields; a column change is a width
    // refit (rows unchanged here, so the request carries none).
    const responsePromise = page.waitForResponse((response: any) => response.request().method() === "POST" && new URL(response.url()).pathname === "/api/session-refits");
    await page.getByRole("button", { name: "Apply", exact: true }).click();
    const response = await responsePromise;
    const body = (await response.text()).trim();
    refitResponses.push({ status: response.status(), body });
    const requestGeometry = response.request().postDataJSON();
    emit("width-refit-http", { before, columns, formBeforeApply, requestColumns: requestGeometry.columns, requestRows: requestGeometry.rows ?? null, status: response.status(), body });
    assert(requestGeometry.columns === columns && requestGeometry.rows === undefined, `width-only Apply changed rows: ${JSON.stringify({ columns: requestGeometry.columns, rows: requestGeometry.rows, formBeforeApply })}`);
    assert(response.status() === 200, `fresh adopted width refit status=${response.status()} body=${JSON.stringify(body)} before=${before} requested=${columns}`);
    await until(`width refit ${columns}`, () => {
      const geometry = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
      return geometry === `${columns}x${rows}`;
    });
    await until(`width refit disclosure ${columns}`, async () => (await disclosure.textContent())?.includes(`${columns}×${rows}`) ?? false);
    await terminal.focus();
    await until(`width refit input ${columns}`, () => page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea") ?? false));
  };
  if (refitOnly) {
    const disclosure = page.locator(".persea-unified-view-disclosure");
    await until("initial refit geometry", async () => (await disclosure.textContent())?.includes("120×40") ?? false);
    if (await disclosure.getAttribute("aria-expanded") !== "true") await disclosure.click();
    await page.getByRole("textbox", { name: "Rows", exact: true }).fill("41");
    const applyRows = page.getByRole("button", { name: "Apply", exact: true });
    await until("rows Apply readiness", async () => (await applyRows.getAttribute("aria-disabled")) !== "true");
    const rowDeadline = Date.now() + 15_000;
    for (let attempt = 0; ; attempt += 1) {
      const brokerLog = path.join(artifactRoot, "broker.log");
      const logOffset = fs.readFileSync(brokerLog, "utf8").length;
      await applyRows.click();
      const outcome = await until("held row acknowledgement", async () => {
        const held = await page.evaluate(() => typeof (window as any).__releaseRowGeometry === "function");
        const log = fs.readFileSync(brokerLog, "utf8").slice(logOffset);
        return held || /event=resize_(?:failed|on_terminal_epoch|stale_target) /.test(log) ? { held, log } : undefined;
      }, Math.max(1, rowDeadline - Date.now()));
      if (outcome.held) break;
      // A scheduled recording cut may briefly refuse before issuing geometry.
      // Exercise the visible refusal and a deliberate operator retry only for
      // that exact private-stack verdict; never retry terminal/transaction faults.
      assert(attempt < 2 && /event=resize_failed .* request=120x41 err="terminal frame is not legal in the current state"/.test(outcome.log), `row Apply failed before acknowledgement: ${outcome.log.trim()}`);
      await until("row busy refusal is visible", async () => (await page.locator(".persea-unified-refusal:not([hidden])").textContent()) === "Fit didn't apply (resize_failed)", Math.max(1, rowDeadline - Date.now()));
      assert(await page.getByRole("textbox", { name: "Rows", exact: true }).inputValue() === "41", "busy refusal lost the operator's row draft");
      assert(await applyRows.getAttribute("aria-disabled") !== "true", "busy refusal did not release Apply");
      emit("row-apply-busy-retry", { attempt: attempt + 1, requestedRows: 41 });
    }
    await until("pre-refit row commit", () => command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim() === "120x41");
  }
  const refitNavigationBaseline = topLevelNavigations;
  const firstRefitColumns = refitOnly ? 240 : 96;
  const secondRefitColumns = refitOnly ? 120 : 80;
  await refitWidth(firstRefitColumns);
  await page.keyboard.type(`printf 'E2E1-POST-'; printf 'REFIT-${firstRefitColumns}\\n'`);
  await page.keyboard.press("Enter");
  await until(`post refit ${firstRefitColumns} input`, () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-t", "=unified_target:"]).includes(`E2E1-POST-REFIT-${firstRefitColumns}`));
  await refitWidth(secondRefitColumns);
  await page.keyboard.type(`printf 'E2E1-POST-'; printf 'REFIT-${secondRefitColumns}\\n'`);
  await page.keyboard.press("Enter");
  await until(`post refit ${secondRefitColumns} input`, () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-t", "=unified_target:"]).includes(`E2E1-POST-REFIT-${secondRefitColumns}`));
  if (refitOnly) {
    await until("two refit reattachments", () => terminalWebSockets >= 3);
    const targetTranscript = command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-S", "-", "-t", "=unified_target:"]);
    assert((targetTranscript.match(/E2E1-POST-REFIT-240/g) ?? []).length === 1, "first refit marker was lost or duplicated");
    assert((targetTranscript.match(/E2E1-POST-REFIT-120/g) ?? []).length === 1, "second refit marker was lost or duplicated");
    assert(JSON.stringify(siblingState()) === JSON.stringify(siblingsBefore), `width refit mutated a sibling: before=${JSON.stringify(siblingsBefore)} after=${JSON.stringify(siblingState())}`);
    assert(topLevelNavigations === refitNavigationBaseline, `width refit reloaded the page: before=${refitNavigationBaseline} after=${topLevelNavigations}`);
    assert(terminalWebSockets === 3, `two refits opened ${terminalWebSockets} terminal sockets, want initial plus exactly two reattachments`);
    assert(refitResponses.length === 2 && refitResponses.every((response) => response.status === 200), `refit response ledger=${JSON.stringify(refitResponses)}`);
    const successorSources = refitResponses.map((response) => JSON.parse(response.body).successor_source);
    assert(successorSources.every((source) => /^[A-Za-z0-9_-]{43}$/.test(source)) && new Set(successorSources).size === 2, `successor source ledger=${JSON.stringify(successorSources)}`);
    emit("width-refit-private-stack-green", { browserEngine, firstRefitColumns, secondRefitColumns, geometry: command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim(), siblingsBefore, terminalWebSockets, topLevelNavigations, successorSources, httpErrors });
    return;
  }

  await page.keyboard.type("for i in $(seq 1 40); do printf 'E2E1-RELOAD-%02d\\n' \"$i\"; sleep .03; done");
  await page.keyboard.press("Enter");
  await until("staged reload capability", () => page.evaluate(() => /^[A-Za-z0-9_-]{43}$/.test(window.sessionStorage.getItem("persea-unified-terminal-reload-handle-v1") ?? "")));
  await page.waitForTimeout(120);
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector(".persea-unified-terminal .xterm");
  await page.waitForTimeout(1400);
  const reloadShot = await page.screenshot({ path: path.join(artifactRoot, "02-reload-race.png") });
  await terminal.focus();
  await page.keyboard.type("printf 'E2E1-POST-RELOAD\\n'");
  await page.keyboard.press("Enter");
  await page.waitForTimeout(200);
  await page.screenshot({ path: path.join(artifactRoot, "02b-post-reload-input-attempt.png") });
  emit("post-reload-input-attempt", { active: await page.evaluate(() => ({ tag: document.activeElement?.tagName, className: (document.activeElement as HTMLElement | null)?.className ?? "" })), socketEvents: socketEvents.slice(-12) });
  await until("post reload input", () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-t", "=unified_target:"]).includes("E2E1-POST-RELOAD"));

  await page.keyboard.type("for i in $(seq 1 90); do printf 'E2E1-SCROLL-%03d\\n' \"$i\"; done");
  await page.keyboard.press("Enter");
  await page.waitForTimeout(350);
  await until("scroll transcript completion", () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-S", "-", "-t", "=unified_target:"]).includes("E2E1-SCROLL-090"));
  const outer = page.locator(".persea-unified-scroll");
  const terminalScreen = page.locator(".persea-unified-xterm .xterm-screen");
  await until("native scroll range settlement", () => outer.evaluate((node: HTMLElement) => node.scrollHeight > node.clientHeight + 500));
  await page.waitForTimeout(150);

  const ownership = await page.evaluate(() => {
    const outerNode = document.querySelector<HTMLElement>(".persea-unified-scroll");
    const host = document.querySelector<HTMLElement>(".persea-unified-xterm");
    const screen = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-screen");
    const scrollbars = [...document.querySelectorAll<HTMLElement>(".persea-unified-xterm .xterm-scrollable-element > .scrollbar")];
    if (!outerNode || !host || !screen) return null;
    return {
      outerOverflowY: getComputedStyle(outerNode).overflowY,
      hostPosition: getComputedStyle(host).position,
      productOwners: document.querySelectorAll(".persea-unified-scroll").length,
      pageScrollY: window.scrollY,
      pageScrollHeight: document.documentElement.scrollHeight,
      bodyScrollHeight: document.body.scrollHeight,
      viewportHeight: window.innerHeight,
      visibleXtermScrollbars: scrollbars.filter((node) => {
        const rect = node.getBoundingClientRect();
        const style = getComputedStyle(node);
        return style.display !== "none" && style.visibility !== "hidden" && style.pointerEvents !== "none" && rect.width > 0 && rect.height > 0;
      }).length,
    };
  });
  assert(ownership, "unified scroll ownership geometry unavailable");
  const mechanismFailures: string[] = [];
  if (ownership.productOwners !== 1 || !["auto", "scroll"].includes(ownership.outerOverflowY) || ownership.hostPosition !== "absolute"
    || ownership.visibleXtermScrollbars !== 0 || ownership.pageScrollY !== 0 || ownership.pageScrollHeight > ownership.viewportHeight
    || ownership.bodyScrollHeight > ownership.viewportHeight) {
    mechanismFailures.push(`one-owner structure: ${JSON.stringify(ownership)}`);
  }

  await outer.evaluate((node: HTMLElement) => { node.scrollTop = node.scrollHeight; });
  await page.waitForTimeout(100);
  await page.evaluate(() => {
    (window as any).__perseaWheelDefaults = [];
    document.addEventListener("wheel", (event) => {
      setTimeout(() => (window as any).__perseaWheelDefaults.push({ defaultPrevented: event.defaultPrevented, target: (event.target as HTMLElement | null)?.className ?? "" }), 0);
    }, { capture: true, once: true });
  });
  const wheelBefore = await outer.evaluate((node: HTMLElement) => node.scrollTop);
  await terminalScreen.hover();
  await page.mouse.wheel(0, -420);
  await page.waitForTimeout(120);
  const wheelAfter = await outer.evaluate((node: HTMLElement) => node.scrollTop);
  const wheelDefaults = await page.evaluate(() => (window as any).__perseaWheelDefaults ?? []);
  if (!(wheelAfter < wheelBefore - 1) || wheelDefaults.length !== 1 || wheelDefaults[0].defaultPrevented) {
    mechanismFailures.push(`trusted native wheel: ${JSON.stringify({ wheelBefore, wheelAfter, wheelDefaults })}`);
  }

  const fractionalBefore = await page.evaluate(async () => {
    const outerNode = document.querySelector<HTMLElement>(".persea-unified-scroll")!;
    const host = document.querySelector<HTMLElement>(".persea-unified-xterm")!;
    const screen = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    const maximum = outerNode.scrollHeight - outerNode.clientHeight;
    outerNode.scrollTop = Math.min(maximum - cellHeight * 2, cellHeight * 8 + 0.25);
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    return { top: outerNode.scrollTop, hostTop: host.getBoundingClientRect().top, hostOffset: Number.parseFloat(host.style.top), screenTop: screen.getBoundingClientRect().top, cellHeight };
  });
  const fractionalAfter = await page.evaluate(async () => {
    const outerNode = document.querySelector<HTMLElement>(".persea-unified-scroll")!;
    const host = document.querySelector<HTMLElement>(".persea-unified-xterm")!;
    const screen = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-screen")!;
    outerNode.scrollTop += 0.5;
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    return { top: outerNode.scrollTop, hostTop: host.getBoundingClientRect().top, hostOffset: Number.parseFloat(host.style.top), screenTop: screen.getBoundingClientRect().top };
  });
  const fractionalScrollDelta = fractionalAfter.top - fractionalBefore.top;
  const fractionalPixelDelta = fractionalAfter.screenTop - fractionalBefore.screenTop;
  if (!(fractionalScrollDelta > 0 && fractionalScrollDelta < fractionalBefore.cellHeight / 2)
    || Math.abs(fractionalPixelDelta + fractionalScrollDelta) > 0.08
    || Math.abs(fractionalAfter.hostOffset - fractionalBefore.hostOffset) > 0.08) {
    mechanismFailures.push(`fractional native motion: ${JSON.stringify({ fractionalBefore, fractionalAfter, fractionalScrollDelta, fractionalPixelDelta })}`);
  }
  const rowBoundary = await page.evaluate(async () => {
    const outerNode = document.querySelector<HTMLElement>(".persea-unified-scroll")!;
    const host = document.querySelector<HTMLElement>(".persea-unified-xterm")!;
    const screen = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    const sample = () => ({
      top: outerNode.scrollTop,
      hostTop: host.getBoundingClientRect().top,
      hostOffset: Number.parseFloat(host.style.top),
      screenTop: screen.getBoundingClientRect().top,
      projectedRow: Math.round(Number.parseFloat(host.style.top) / cellHeight),
    });
    outerNode.scrollTop = cellHeight * 9 - 1;
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    const before = sample();
    outerNode.scrollTop += 2;
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    return { before, after: sample(), cellHeight };
  });
  const boundaryScrollDelta = rowBoundary.after.top - rowBoundary.before.top;
  const boundaryHostStep = rowBoundary.after.hostOffset - rowBoundary.before.hostOffset;
  const boundaryScreenStep = rowBoundary.after.screenTop - rowBoundary.before.screenTop;
  if (Math.abs(boundaryScrollDelta - 2) > 0.08
    || rowBoundary.after.projectedRow !== rowBoundary.before.projectedRow + 1
    || Math.abs(boundaryHostStep - rowBoundary.cellHeight) > 0.08
    || Math.abs(boundaryScreenStep - (rowBoundary.cellHeight - boundaryScrollDelta)) > 0.08) {
    mechanismFailures.push(`row-boundary projection: ${JSON.stringify({ rowBoundary, boundaryScrollDelta, boundaryHostStep, boundaryScreenStep })}`);
  }
  emit("native-scroll-mechanisms", { ownership, wheelBefore, wheelAfter, wheelDefaults, fractionalBefore, fractionalAfter, rowBoundary, mechanismFailures });
  assert(mechanismFailures.length === 0, `native scroll mechanism failures: ${mechanismFailures.join(" | ")}`);

  const scrollBeforeLive = await outer.evaluate((node: HTMLElement) => {
    const host = node.querySelector<HTMLElement>(".persea-unified-xterm")!;
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    return { top: node.scrollTop, height: node.scrollHeight, client: node.clientHeight, hostTop: host.getBoundingClientRect().top, screenTop: screen.getBoundingClientRect().top, row: Math.floor(node.scrollTop / cellHeight), remainder: node.scrollTop % cellHeight };
  });
  const visibleClip = await page.evaluate(() => {
    const outerNode = document.querySelector<HTMLElement>(".persea-unified-scroll")!;
    const screen = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-screen")!;
    const outerRect = outerNode.getBoundingClientRect();
    const screenRect = screen.getBoundingClientRect();
    return { x: screenRect.x, y: outerRect.y, width: Math.min(screenRect.width, outerRect.width - 20), height: Math.min(screenRect.bottom, outerRect.bottom) - outerRect.y };
  });
  const pixelsBeforeLive = await page.screenshot({ clip: visibleClip });
  command("tmux", ["-S", tmuxSocket, "send-keys", "-t", "=unified_target:", "printf 'E2E1-SCROLLED-LIVE\\n'", "Enter"]);
  await until("scrolled live append", () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-S", "-", "-t", "=unified_target:"]).includes("E2E1-SCROLLED-LIVE"));
  await page.waitForTimeout(200);
  const scrollAfterLive = await outer.evaluate((node: HTMLElement) => {
    const host = node.querySelector<HTMLElement>(".persea-unified-xterm")!;
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    return { top: node.scrollTop, height: node.scrollHeight, client: node.clientHeight, hostTop: host.getBoundingClientRect().top, screenTop: screen.getBoundingClientRect().top, row: Math.floor(node.scrollTop / cellHeight), remainder: node.scrollTop % cellHeight };
  });
  const pixelsAfterLive = await page.screenshot({ clip: visibleClip });
  assert(Math.abs(scrollAfterLive.top - scrollBeforeLive.top) < 0.08 && scrollAfterLive.row === scrollBeforeLive.row
    && Math.abs(scrollAfterLive.remainder - scrollBeforeLive.remainder) < 0.08 && Math.abs(scrollAfterLive.screenTop - scrollBeforeLive.screenTop) < 0.08
    && sha256(pixelsAfterLive) === sha256(pixelsBeforeLive) && scrollAfterLive.height >= scrollBeforeLive.height,
  `scrolled-up live output moved logical/scalar/pixel anchor: ${JSON.stringify({ scrollBeforeLive, scrollAfterLive, beforePixels: sha256(pixelsBeforeLive), afterPixels: sha256(pixelsAfterLive) })}`);
  const scrolledShot = await page.screenshot({ path: path.join(artifactRoot, "03-scrolled-live.png") });

  await outer.evaluate((node: HTMLElement) => {
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    const maximum = node.scrollHeight - node.clientHeight;
    node.scrollTop = maximum - Math.min(cellHeight / 4, cellHeight / 2 - 2);
  });
  await page.waitForTimeout(100);
  const nearTailBefore = await outer.evaluate((node: HTMLElement) => {
    const host = node.querySelector<HTMLElement>(".persea-unified-xterm")!;
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    const maximum = node.scrollHeight - node.clientHeight;
    return { top: node.scrollTop, maximum, gap: maximum - node.scrollTop, height: node.scrollHeight, hostTop: host.getBoundingClientRect().top, screenTop: screen.getBoundingClientRect().top, cellHeight, row: Math.floor(node.scrollTop / cellHeight), remainder: node.scrollTop % cellHeight };
  });
  assert(nearTailBefore.gap > 1 && nearTailBefore.gap < nearTailBefore.cellHeight / 2,
    `near-tail setup did not land inside the causal tolerance interval: ${JSON.stringify(nearTailBefore)}`);
  const nearTailPixelsBefore = await page.screenshot({ clip: visibleClip });
  command("tmux", ["-S", tmuxSocket, "send-keys", "-t", "=unified_target:", "printf 'E2E1-NEAR-TAIL-ANCHOR\\n'", "Enter"]);
  await until("near-tail append", () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-S", "-", "-t", "=unified_target:"]).includes("E2E1-NEAR-TAIL-ANCHOR"));
  await page.waitForTimeout(200);
  const nearTailAfter = await outer.evaluate((node: HTMLElement) => {
    const host = node.querySelector<HTMLElement>(".persea-unified-xterm")!;
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    const maximum = node.scrollHeight - node.clientHeight;
    return { top: node.scrollTop, maximum, gap: maximum - node.scrollTop, height: node.scrollHeight, hostTop: host.getBoundingClientRect().top, screenTop: screen.getBoundingClientRect().top, cellHeight, row: Math.floor(node.scrollTop / cellHeight), remainder: node.scrollTop % cellHeight };
  });
  const nearTailPixelsAfter = await page.screenshot({ clip: visibleClip });
  assert(Math.abs(nearTailAfter.top - nearTailBefore.top) < 0.08 && nearTailAfter.row === nearTailBefore.row
    && Math.abs(nearTailAfter.remainder - nearTailBefore.remainder) < 0.08 && Math.abs(nearTailAfter.screenTop - nearTailBefore.screenTop) < 0.08
    && sha256(nearTailPixelsAfter) === sha256(nearTailPixelsBefore) && nearTailAfter.height >= nearTailBefore.height,
  `near-tail live output moved scalar/pixel anchor: ${JSON.stringify({ nearTailBefore, nearTailAfter, beforePixels: sha256(nearTailPixelsBefore), afterPixels: sha256(nearTailPixelsAfter) })}`);

  await outer.evaluate((node: HTMLElement) => { node.scrollTop = node.scrollHeight; });
  await page.waitForTimeout(100);
  const tailPixelsBefore = await page.screenshot({ clip: visibleClip });
  command("tmux", ["-S", tmuxSocket, "send-keys", "-t", "=unified_target:", "printf 'E2E1-TAIL-FOLLOW\\n'", "Enter"]);
  await until("tail-follow append", () => command("tmux", ["-S", tmuxSocket, "capture-pane", "-p", "-S", "-", "-t", "=unified_target:"]).includes("E2E1-TAIL-FOLLOW"));
  await page.waitForTimeout(200);
  const tailFollow = await outer.evaluate((node: HTMLElement) => ({ top: node.scrollTop, maximum: node.scrollHeight - node.clientHeight }));
  const tailPixelsAfter = await page.screenshot({ clip: visibleClip });
  assert(Math.abs(tailFollow.maximum - tailFollow.top) <= 1 && sha256(tailPixelsAfter) !== sha256(tailPixelsBefore),
    `return to tail did not resume visible follow: ${JSON.stringify({ tailFollow, beforePixels: sha256(tailPixelsBefore), afterPixels: sha256(tailPixelsAfter) })}`);

  await outer.evaluate((node: HTMLElement) => { node.scrollTop = (node.scrollHeight - node.clientHeight) * 0.45; });
  await page.waitForTimeout(100);
  await page.evaluate(() => {
    (window as any).__perseaTouchDefaults = [];
    for (const type of ["touchstart", "touchmove", "touchend"]) {
      document.addEventListener(type, (event) => {
        setTimeout(() => (window as any).__perseaTouchDefaults.push({ type, defaultPrevented: event.defaultPrevented }), 0);
      }, { capture: true });
    }
  });
  const touchBox = await terminalScreen.boundingBox();
  assert(touchBox, "terminal touch geometry unavailable");
  const touchX = touchBox.x + touchBox.width / 2;
  const touchStartY = touchBox.y + Math.min(touchBox.height * 0.7, 420);
  const touchBefore = await outer.evaluate((node: HTMLElement) => {
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    return { top: node.scrollTop, screenTop: screen.getBoundingClientRect().top };
  });
  const touchPixelsBefore = await page.screenshot({ clip: visibleClip });
  await cdp.send("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 1 });
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: touchX, y: touchStartY, radiusX: 1, radiusY: 1, force: 1 }] });
  for (const delta of [24, 64, 112]) {
    await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: touchX, y: touchStartY - delta, radiusX: 1, radiusY: 1, force: 1 }] });
    await page.waitForTimeout(35);
  }
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await page.waitForTimeout(250);
  const touchAfter = await outer.evaluate((node: HTMLElement) => {
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    return { top: node.scrollTop, screenTop: screen.getBoundingClientRect().top };
  });
  const touchPixelsAfter = await page.screenshot({ clip: visibleClip });
  const touchDefaults = await page.evaluate(() => (window as any).__perseaTouchDefaults ?? []);
  assert(Math.abs(touchAfter.top - touchBefore.top) > 1 && sha256(touchPixelsAfter) !== sha256(touchPixelsBefore)
    && touchDefaults.some((entry: any) => entry.type === "touchmove") && touchDefaults.every((entry: any) => !entry.defaultPrevented),
  `trusted touch did not natively move outer scroll and visible pixels: ${JSON.stringify({ touchBefore, touchAfter, touchDefaults, beforePixels: sha256(touchPixelsBefore), afterPixels: sha256(touchPixelsAfter) })}`);

  await outer.evaluate((node: HTMLElement) => { node.scrollTop = (node.scrollHeight - node.clientHeight) * 0.55; });
  await page.waitForTimeout(100);
  const selectionEdgeBefore = await outer.evaluate((node: HTMLElement) => node.scrollTop);
  const outerBox = await outer.boundingBox();
  const selectionScreenBox = await terminalScreen.boundingBox();
  assert(outerBox && selectionScreenBox, "selection edge geometry unavailable");
  const edgeSelectionX = selectionScreenBox.x + Math.min(180, selectionScreenBox.width * 0.35);
  const edgeSelectionY = outerBox.y + outerBox.height * 0.55;
  await page.mouse.move(edgeSelectionX, edgeSelectionY);
  await page.mouse.down();
  await page.mouse.move(edgeSelectionX + 80, outerBox.y - 24, { steps: 12 });
  await page.waitForTimeout(500);
  const selectionEdgeAfter = await outer.evaluate((node: HTMLElement) => node.scrollTop);
  await page.mouse.up();
  assert(selectionEdgeAfter < selectionEdgeBefore - 1 && await page.getByRole("button", { name: "Copy selection" }).isEnabled(),
    `drag-edge selection did not hand intent to outer scroll: ${JSON.stringify({ selectionEdgeBefore, selectionEdgeAfter })}`);

  await page.mouse.wheel(0, 100000);
  await terminal.focus();
  await page.keyboard.type("printf '\\033[2J\\033[HE2E1-SELECTION-COPY\\n'");
  await page.keyboard.press("Enter");
  await page.waitForTimeout(250);
  const box = await terminal.boundingBox();
  assert(box, "terminal geometry unavailable");
  const cellHeight = box.height / 24;
  const rowY = box.y + 8 + cellHeight / 2;
  const selectionX = box.x + 45;
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: selectionX, y: rowY });
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: selectionX, y: rowY, button: "left", buttons: 1, clickCount: 3 });
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: selectionX, y: rowY, button: "left", buttons: 0, clickCount: 3 });
  await page.getByRole("button", { name: "Copy selection" }).waitFor({ state: "visible" });
  await until("native xterm selection", () => page.getByRole("button", { name: "Copy selection" }).isEnabled(), 1500);
  await page.screenshot({ path: path.join(artifactRoot, "04-selection-visible.png") });
  await page.getByRole("button", { name: "Copy selection" }).click();
  await page.waitForTimeout(100);
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  assert(copied.includes("E2E1-SELECTION-COPY"), `selection copy mismatch: ${JSON.stringify(copied)}`);
  const selectionShot = await page.screenshot({ path: path.join(artifactRoot, "04-selection-copy.png") });

  const geometryBefore = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
  assert(geometryBefore === "80x24", `initial terminal geometry is not exact 80x24: ${geometryBefore}`);
  const largeScreenHeight = await terminalScreen.evaluate((node: HTMLElement) => node.getBoundingClientRect().height);
  await page.getByRole("button", { name: "Zoom in" }).click();
  await page.getByRole("button", { name: "Fit font" }).click();
  await page.waitForTimeout(250);
  const geometryAfterFit = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
  assert(geometryAfterFit === "80x24", `display fit resized tmux: ${geometryBefore} -> ${geometryAfterFit}`);
  await page.setViewportSize({ width: 1280, height: 520 });
  await page.waitForTimeout(300);
  await page.getByRole("button", { name: "Fit font" }).click();
  await page.waitForTimeout(250);
  const geometryAfterSmallViewport = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
  const smallScreenHeight = await terminalScreen.evaluate((node: HTMLElement) => node.getBoundingClientRect().height);
  assert(geometryAfterSmallViewport === "80x24" && smallScreenHeight < largeScreenHeight,
    `smaller presentation viewport changed upstream geometry or did not fit: ${JSON.stringify({ geometryAfterSmallViewport, largeScreenHeight, smallScreenHeight })}`);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.waitForTimeout(300);
  await page.getByRole("button", { name: "Fit font" }).click();
  await page.waitForTimeout(250);

  // Zooming past the fit makes the grid larger than the viewport on both axes.
  // Presentation may letterbox, but it may never strand a column or the final
  // row: whatever the grid renders has to be reachable by scrolling.
  await page.setViewportSize({ width: 900, height: 620 });
  await page.waitForTimeout(300);
  await page.getByRole("button", { name: "Fit font" }).click();
  await page.waitForTimeout(250);
  for (let press = 0; press < 12; press++) {
    await page.getByRole("button", { name: "Zoom in" }).click();
    await page.waitForTimeout(60);
  }
  await page.waitForTimeout(300);
  const zoomedOverflow = await page.evaluate(async () => {
    const outerNode = document.querySelector<HTMLElement>(".persea-unified-scroll")!;
    const screen = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-screen")!;
    outerNode.scrollLeft = outerNode.scrollWidth;
    outerNode.scrollTop = outerNode.scrollHeight;
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    const outerRect = outerNode.getBoundingClientRect();
    const screenRect = screen.getBoundingClientRect();
    return {
      screenWidth: screenRect.width,
      screenHeight: screenRect.height,
      clientWidth: outerNode.clientWidth,
      clientHeight: outerNode.clientHeight,
      horizontalRange: outerNode.scrollWidth - outerNode.clientWidth,
      horizontalOvershoot: screenRect.right - outerRect.right,
      verticalOvershoot: screenRect.bottom - outerRect.bottom,
      overflowX: getComputedStyle(outerNode).overflowX,
    };
  });
  emit("zoomed-overflow-reachability", zoomedOverflow);
  assert(zoomedOverflow.screenWidth > zoomedOverflow.clientWidth && zoomedOverflow.screenHeight > zoomedOverflow.clientHeight,
    `zoom-past-fit setup did not overflow the viewport on both axes: ${JSON.stringify(zoomedOverflow)}`);
  assert(zoomedOverflow.horizontalRange > 0 && zoomedOverflow.horizontalOvershoot <= 1 && zoomedOverflow.verticalOvershoot <= 1,
    `zoomed grid strands columns or the final row outside the reachable range: ${JSON.stringify(zoomedOverflow)}`);
  const geometryAfterZoomOverflow = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
  assert(geometryAfterZoomOverflow === "80x24", `zoom past fit resized tmux: ${geometryAfterZoomOverflow}`);
  await page.screenshot({ path: path.join(artifactRoot, "04-zoomed-overflow-reachability.png") });
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.waitForTimeout(300);
  await page.getByRole("button", { name: "Fit font" }).click();
  await page.waitForTimeout(250);

  const normalAnchorBeforeAlternate = await outer.evaluate(async (node: HTMLElement) => {
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    node.scrollTop = (node.scrollHeight - node.clientHeight) * 0.42 + 1;
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    return { top: node.scrollTop, row: Math.floor(node.scrollTop / cellHeight), remainder: node.scrollTop % cellHeight, cellHeight };
  });
  command("tmux", ["-S", tmuxSocket, "send-keys", "-t", "=unified_target:", "seq 1 80 | TERM=xterm less", "Enter"]);
  await until("alternate screen range collapse", () => outer.evaluate((node: HTMLElement) => node.scrollHeight <= node.clientHeight + 1));
  const alternateState = await outer.evaluate((node: HTMLElement) => ({ top: node.scrollTop, range: node.scrollHeight - node.clientHeight }));
  assert(alternateState.top === 0 && alternateState.range <= 1, `alternate screen retained normal transcript range: ${JSON.stringify(alternateState)}`);
  const tuiShot = await page.screenshot({ path: path.join(artifactRoot, "05-less-alternate-screen.png") });
  command("tmux", ["-S", tmuxSocket, "send-keys", "-t", "=unified_target:", "q"]);
  await until("normal screen range restore", () => outer.evaluate((node: HTMLElement) => node.scrollHeight > node.clientHeight + 500));
  await page.waitForTimeout(200);
  const normalAnchorAfterAlternate = await outer.evaluate((node: HTMLElement) => {
    const screen = node.querySelector<HTMLElement>(".xterm-screen")!;
    const cellHeight = screen.getBoundingClientRect().height / 24;
    return { top: node.scrollTop, row: Math.floor(node.scrollTop / cellHeight), remainder: node.scrollTop % cellHeight, cellHeight };
  });
  assert(normalAnchorAfterAlternate.row === normalAnchorBeforeAlternate.row
    && Math.abs(normalAnchorAfterAlternate.remainder - normalAnchorBeforeAlternate.remainder) < 0.08
    && Math.abs(normalAnchorAfterAlternate.top - normalAnchorBeforeAlternate.top) < 0.08,
  `alternate screen did not restore normal logical/scalar anchor: ${JSON.stringify({ normalAnchorBeforeAlternate, normalAnchorAfterAlternate })}`);
  const geometryAfterTUI = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
  assert(geometryAfterTUI === "80x24", `alternate screen resized tmux: ${geometryBefore} -> ${geometryAfterTUI}`);
  // Everything up to this point — admission, reload, viewport changes, page
  // zoom, font fit, the alternate screen — must have produced no resize request
  // at all. The one control that may is clicked after this line, never before.
  const resizeFrames = socketEvents.filter((event) => event.direction === "sent" && event.payload.includes('"type":"RESIZE_REQUEST"'));
  assert(resizeFrames.length === 0, `browser sent forbidden resize frames: ${JSON.stringify(resizeFrames)}`);

  // Explicit vertical Fit: one trusted click changes the real tmux window
  // height once, changes nothing else, and the change outlives the browser.
  const fitHeightControl = page.getByRole("button", { name: "Fit terminal height" });
  await fitHeightControl.waitFor({ state: "visible" });
  const geometryReadoutBefore = (await page.locator(".persea-unified-geometry").textContent())?.trim();
  assert(geometryReadoutBefore === "80×24", `geometry readout before the fit: ${JSON.stringify(geometryReadoutBefore)}`);
  await page.setViewportSize({ width: 1280, height: 1000 });
  await page.waitForTimeout(300);
  await fitHeightControl.click();
  await until("explicit vertical fit reached tmux", () => {
    const seen = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
    return seen !== "80x24" && seen.startsWith("80x");
  }, 10_000);
  const geometryAfterVerticalFit = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
  const fittedRows = Number.parseInt(geometryAfterVerticalFit.split("x")[1] ?? "0", 10);
  assert(geometryAfterVerticalFit.startsWith("80x") && fittedRows >= 8 && fittedRows <= 120 && fittedRows !== 24,
    `explicit vertical fit did not change rows only: ${geometryAfterVerticalFit}`);
  assert(command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_panes}"]).trim() === "1",
    "explicit vertical fit changed the pane count");
  const verticalFitFrames = socketEvents.filter((event) => event.direction === "sent" && event.payload.includes('"type":"RESIZE_REQUEST"'));
  assert(verticalFitFrames.length === 1, `one click sent ${verticalFitFrames.length} resize requests: ${JSON.stringify(verticalFitFrames)}`);
  assert(verticalFitFrames[0].payload.includes('"columns":80'), `the browser changed columns: ${verticalFitFrames[0].payload}`);
  await until("committed geometry reached the readout", async () => {
    const readout = (await page.locator(".persea-unified-geometry").textContent())?.trim();
    return readout === `80×${fittedRows}`;
  }, 10_000);
  const verticalFitShot = await page.screenshot({ path: path.join(artifactRoot, "05-explicit-vertical-fit.png") });

  // Reopening must replay the committed geometry, not re-request it.
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector(".persea-unified-terminal", { timeout: 10_000 });
  await until("reopened readout replays the committed geometry", async () => {
    const readout = (await page.locator(".persea-unified-geometry").textContent())?.trim();
    return readout === `80×${fittedRows}`;
  }, 15_000);
  const geometryAfterReload = command("tmux", ["-S", tmuxSocket, "display-message", "-p", "-t", "=unified_target:", "#{window_width}x#{window_height}"]).trim();
  assert(geometryAfterReload === geometryAfterVerticalFit,
    `reopening changed the session geometry: ${geometryAfterVerticalFit} -> ${geometryAfterReload}`);
  const reopenedResizeFrames = socketEvents.filter((event) => event.direction === "sent" && event.payload.includes('"type":"RESIZE_REQUEST"'));
  assert(reopenedResizeFrames.length === 1, `reopening sent a resize request: ${JSON.stringify(reopenedResizeFrames)}`);

  const legacyNavigation = page.waitForNavigation({ waitUntil: "domcontentloaded", timeout: 5_000 });
  await page.getByRole("button", { name: "Open legacy" }).click();
  await legacyNavigation;
  await page.waitForURL((url: URL) => !url.hash.includes("engine=unified-dev"));
  await page.waitForSelector(".attachment-page", { timeout: 5_000 });
  assert(await page.locator(".persea-unified-terminal").count() === 0, "legacy navigation retained unified page DOM");
  const legacyShot = await page.screenshot({ path: path.join(artifactRoot, "06-open-legacy.png") });

  emit("http-errors", httpErrors);
  assert(pageErrors.length === 0, `page errors: ${JSON.stringify(pageErrors)}`);
  assert(consoleErrors.length === 0, `console errors: ${JSON.stringify(consoleErrors)}`);
  assert(consoleMessages.length === 0, `console was not clean: ${JSON.stringify(consoleMessages)}`);
  const manifest = {
    verdict: "E2E1_BROWSER_GREEN", base, engine: "chromium", chromium: chromePath,
    screenshots: [beforeShell, shellShot, reloadShot, scrolledShot, selectionShot, tuiShot, legacyShot].map((data: any, index: number) => ({ index, sha256: sha256(data), bytes: data.length })),
    shellVisibleDelta: sha256(beforeShell) !== sha256(shellShot),
    reloadVisible: reloadShot.length > 0,
    scrollBeforeLive, scrollAfterLive, nearTailBefore, nearTailAfter,
    copiedSHA256: sha256(Buffer.from(copied)), copiedBytes: Buffer.byteLength(copied),
    ownership, wheelBefore, wheelAfter, wheelDefaults, fractionalBefore, fractionalAfter, rowBoundary,
    tailFollow, touchBefore, touchAfter, touchDefaults, selectionEdgeBefore, selectionEdgeAfter,
    normalAnchorBeforeAlternate, alternateState, normalAnchorAfterAlternate,
    geometryBefore, geometryAfterFit, geometryAfterSmallViewport, geometryAfterTUI, resizeFrames,
    geometryAfterVerticalFit, geometryAfterReload, verticalFitFrames, reopenedResizeFrames,
    verticalFitShot: { sha256: sha256(verticalFitShot), bytes: verticalFitShot.length },
    realIPhoneHardwarePending: true,
    pageErrors, consoleErrors, consoleMessages, httpErrors,
  };
  fs.writeFileSync(path.join(artifactRoot, "manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o644 });
  emit("pass", manifest);
}

main().catch((error: unknown) => {
  emit("failure", { message: error instanceof Error ? error.message : String(error), stack: error instanceof Error ? error.stack : "" });
  // The transcript lives in the artifact root, which the npm script removes
  // on exit; a failure must also reach the console so a chained run explains
  // its exit code.
  const scenario = slowLinkOnly ? "slow-link" : alternateOnly ? "alternate-screen" : refitOnly ? "width-refit" : "E2E-1";
  console.error(`${scenario} private stack (${browserEngine}) FAILED: ${error instanceof Error ? error.stack ?? error.message : String(error)}`);
  process.exitCode = 1;
}).finally(async () => {
  try { await releaseRowGeometryGate?.(); } catch { /* page already terminal */ }
  try { await browser?.close(); } catch { /* already terminal */ }
  try { if (proxy?.listening) proxy.close(); } catch { /* already terminal */ }
  for (const { child, log } of children.reverse()) {
    try { process.kill(-child.pid, "SIGTERM"); } catch { /* already terminal */ }
    log.end();
  }
  try { command("tmux", ["-S", path.join(tmp, "tmux.sock"), "kill-server"], repo, 10_000); } catch { /* already terminal */ }
  transcript.end();
});
