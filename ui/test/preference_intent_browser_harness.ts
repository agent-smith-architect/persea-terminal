// The page's two preference call sites must settle the
// MATCHING intent when the operation rejects, not only when it fulfils.
//
// OperatorPreferencesService.update() is deliberately rejection-capable: the
// service publishes its committed record to every subscriber synchronously and
// only then resolves, so a subscriber that throws rejects the same operation
// (pinned at test/operator_preferences.test.ts). A document with more than one
// subscriber is the ordinary case — a workspace runs one service for every
// pane — so that rejection reaches the page whose control started the write.
//
// This harness mounts the REAL UnifiedTerminalPage against the REAL service and
// drives the real Zoom in button, with a sibling subscriber that throws on the
// publication that control commits. It then performs a later authoritative
// publication. If the matching intent were kept, it would mask that publication
// for the rest of the document's life; if the rejection were unhandled, the
// void call would leak it to the window.
//
// the theme picker lives on the dashboard's Appearance card, not
// on the terminal page. The theme cases therefore mount the REAL Dashboard on
// the SAME service as the terminal page (one service, many subscribers — the
// document the product actually runs) and drive the card's real theme select:
// the card's save must settle its status line when the operation rejects, the
// terminal must follow the record live, and a newer pick must win over an
// older operation's failure.
import { Dashboard } from "../src/dashboard";
import { OperatorPreferencesService } from "../src/operator_preferences";
import { UnifiedTerminalPage } from "../src/unified_terminal_page";

const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const SOURCE = "preference-intent-source";
const EPOCH = 1n;
const ROWS = 24;
const encoder = new TextEncoder();

type CaseResult = { name: string; detail: Record<string, unknown> };

const unhandled: string[] = [];
window.addEventListener("unhandledrejection", (event) => {
  event.preventDefault();
  const reason = (event as PromiseRejectionEvent).reason;
  unhandled.push(String(reason && (reason as Error).message ? (reason as Error).message : reason));
});

function settle(): Promise<void> {
  return new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(resolve, 30))));
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

class MemoryStorage {
  private readonly values = new Map<string, string>();
  getItem(key: string): string | null { return this.values.get(key) ?? null; }
  setItem(key: string, value: string): void { this.values.set(key, value); }
  removeItem(key: string): void { this.values.delete(key); }
}

// One in-memory preference record with a strong revision, exactly like the
// front door's: GET reads it, PUT compares If-Match and bumps the revision.
function preferenceServer() {
  let revision = 0;
  let record: { theme: string; font_size: number | null; composer_font_size: number } = { theme: "default", font_size: 14, composer_font_size: 13 };
  const body = () => JSON.stringify({
    version: 1, theme: record.theme, font_size: record.font_size, composer_font_size: record.composer_font_size,
    default_session: null, revision, stored: true, available: true,
  });
  const headers = () => ({ "Content-Type": "application/json", ETag: `"${revision}"`, "Cache-Control": "no-store" });
  return {
    read: () => ({ ...record, revision }),
    fetch: async (_input: string, init: RequestInit): Promise<Response> => {
      if ((init.method ?? "GET") === "GET") return new Response(body(), { status: 200, headers: headers() });
      const sent = new Headers(init.headers).get("If-Match");
      if (sent !== `"${revision}"`) return new Response(body(), { status: 412, headers: headers() });
      const patch = JSON.parse(String(init.body)) as { theme: string; font_size: number | null; composer_font_size: number };
      record = { theme: patch.theme, font_size: patch.font_size, composer_font_size: patch.composer_font_size };
      revision += 1;
      return new Response(body(), { status: 200, headers: headers() });
    },
  };
}

type Mounted = {
  page: UnifiedTerminalPage;
  service: OperatorPreferencesService;
  server: ReturnType<typeof preferenceServer>;
  shell: HTMLElement;
  root: HTMLElement;
};

let generation = 0;
let cut = 9n;

// The boot the product performs: the document-global service is loaded BEFORE
// the page exists (ui/src/app.ts), so the page's own subscription is the first
// one the service holds.
async function mount(): Promise<Mounted> {
  const root = document.createElement("div");
  root.className = "preference-intent-root";
  document.body.append(root);
  const server = preferenceServer();
  const service = new OperatorPreferencesService({
    fetch: server.fetch,
    csrf: () => "csrf-token",
    storage: new MemoryStorage(),
  });
  await service.load();
  const page = new UnifiedTerminalPage({
    root,
    port: { trySend: () => "ACCEPTED", finalize: () => undefined },
    capabilityMode: "control",
    styleNonce: NONCE,
    rememberSource: () => undefined,
    preferences: service,
  });
  generation += 1;
  cut += 1n;
  page.openTransport(generation);
  page.receiveDecoded(generation, {
    version: 1, source: SOURCE, epoch: EPOCH,
    type: "PREPARE", cut, kind: "INITIAL", columns: 80, rows: ROWS,
    history: [], truncated: false, replay: encoder.encode("ready"),
  });
  page.receiveDecoded(generation, { version: 1, source: SOURCE, epoch: EPOCH, type: "COMMIT", cut });
  page.receiveDecoded(generation, { version: 1, source: SOURCE, epoch: EPOCH, type: "MODE", mode: "CONTROL" });
  await settle();
  const shell = root.querySelector(".persea-unified-terminal");
  if (!(shell instanceof HTMLElement)) throw new Error("harness: the unified shell did not render");
  return { page, service, server, shell, root };
}

function dismount(mounted: Mounted): void {
  mounted.page.destroy();
  mounted.root.remove();
}

type MountedDashboard = { root: HTMLElement; select: HTMLSelectElement; status: HTMLElement };

// The product's dashboard, on the terminal's service. Inventory and workspaces
// are refused (503): the card renders from the preferences read alone, and
// both refusals are caught by the dashboard's own status lines.
function mountDashboard(mounted: Mounted): MountedDashboard {
  const root = document.createElement("div");
  root.className = "preference-intent-dashboard";
  document.body.append(root);
  const fetcher = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (url.includes("/api/preferences")) return mounted.server.fetch(url, init ?? {});
    return new Response("{}", { status: 503, headers: { "Content-Type": "application/json" } });
  };
  new Dashboard(root, fetcher, mounted.service).mount();
  const select = root.querySelector('.dashboard-appearance select[aria-label="Terminal theme"]');
  if (!(select instanceof HTMLSelectElement)) throw new Error("harness: the dashboard theme select is absent");
  const status = root.querySelector(".dashboard-appearance__status");
  if (!(status instanceof HTMLElement)) throw new Error("harness: the dashboard appearance status is absent");
  return { root, select, status };
}

function dismountDashboard(dashboard: MountedDashboard): void {
  dashboard.root.remove();
  document.body.classList.remove("dashboard-mode");
}

// The dashboard card's own control, driven the way the operator drives it.
function chooseTheme(dashboard: MountedDashboard, id: string): void {
  const select = dashboard.select;
  select.value = id;
  select.dispatchEvent(new Event("change", { bubbles: true }));
}

function clickZoomIn(mounted: Mounted): void {
  const button = Array.from(mounted.root.querySelectorAll("button")).find((node) => node.textContent === "Zoom in");
  if (!button) throw new Error("harness: the Zoom in control is absent");
  button.click();
}

// A sibling subscriber that fails exactly once, on the publication the page's
// control commits, and is healthy afterwards — so the later authoritative
// publication is a clean one and the case measures only what the page kept.
function faultySibling(service: OperatorPreferencesService, fails: (snapshot: { theme: string; fontSize: number | null }) => boolean): { fired: () => boolean } {
  let armed = true;
  service.subscribe((snapshot) => {
    if (!armed || !fails({ theme: snapshot.preferences.theme, fontSize: snapshot.preferences.fontSize })) return;
    armed = false;
    throw new Error("sibling preference observer");
  });
  return { fired: () => !armed };
}

async function themeRejection(): Promise<CaseResult> {
  const mounted = await mount();
  const dashboard = mountDashboard(mounted);
  await settle();
  const sibling = faultySibling(mounted.service, (snapshot) => snapshot.theme === "dracula");
  const before = unhandled.length;
  chooseTheme(dashboard, "dracula");
  await delay(250);
  const committed = {
    live: mounted.shell.dataset.theme,
    stored: mounted.server.read().theme,
    status: mounted.service.snapshot().status,
    revision: mounted.service.snapshot().revision,
    statusText: dashboard.status.textContent,
    selectValue: dashboard.select.value,
  };
  // A later authoritative publication. A retained intent masks it forever.
  const outcome = await mounted.service.update({ theme: "gruvbox-dark" });
  await delay(150);
  const detail = {
    siblingThrew: sibling.fired(),
    unhandledRejections: unhandled.slice(before),
    committedLiveTheme: committed.live,
    committedStoredTheme: committed.stored,
    committedStatus: committed.status,
    committedRevision: committed.revision,
    committedStatusText: committed.statusText,
    committedSelectValue: committed.selectValue,
    laterOutcome: outcome,
    laterLiveTheme: mounted.shell.dataset.theme,
    laterSelectValue: dashboard.select.value,
  };
  dismountDashboard(dashboard);
  dismount(mounted);
  return { name: "theme_rejection_settles_matching_intent", detail };
}

async function fontRejection(): Promise<CaseResult> {
  const mounted = await mount();
  const sibling = faultySibling(mounted.service, (snapshot) => snapshot.fontSize === 15);
  const before = unhandled.length;
  clickZoomIn(mounted);
  // The debounced save owns the intent for 500 ms before it is submitted.
  await delay(900);
  const committed = {
    live: mounted.shell.dataset.fontBaseline,
    stored: mounted.server.read().font_size,
    status: mounted.service.snapshot().status,
    revision: mounted.service.snapshot().revision,
  };
  const outcome = await mounted.service.update({ fontSize: 20 });
  await delay(150);
  const detail = {
    siblingThrew: sibling.fired(),
    unhandledRejections: unhandled.slice(before),
    committedLiveFont: committed.live,
    committedStoredFont: committed.stored,
    committedStatus: committed.status,
    committedRevision: committed.revision,
    laterOutcome: outcome,
    laterLiveFont: mounted.shell.dataset.fontBaseline,
    // a discarded Zoom intent says so on the page.
    toast: mounted.root.querySelector(".persea-unified-toast")?.textContent ?? "",
  };
  dismount(mounted);
  return { name: "font_rejection_settles_matching_intent", detail };
}

// A newer intent must survive an older operation's failure: the identity fence
// on the rejection path is the same one the fulfilled path uses.
async function newerIntentSurvives(): Promise<CaseResult> {
  const mounted = await mount();
  const dashboard = mountDashboard(mounted);
  await settle();
  const sibling = faultySibling(mounted.service, (snapshot) => snapshot.theme === "dracula");
  const before = unhandled.length;
  chooseTheme(dashboard, "dracula");
  chooseTheme(dashboard, "one-dark");
  await delay(400);
  const detail = {
    siblingThrew: sibling.fired(),
    unhandledRejections: unhandled.slice(before),
    liveTheme: mounted.shell.dataset.theme,
    storedTheme: mounted.server.read().theme,
    status: mounted.service.snapshot().status,
    selectValue: dashboard.select.value,
    statusText: dashboard.status.textContent,
  };
  dismountDashboard(dashboard);
  dismount(mounted);
  return { name: "newer_theme_intent_survives_older_rejection", detail };
}

// control-lifecycle/R3/R4 lifecycle pin. The controller owns these transitions; using
// its internal seam here avoids a fake reimplementation while still mounting
// and destroying the real xterm page in a browser. destroy() must synchronously
// restore the original row and close the page-owned typography disclosure
// before terminal teardown removes its portal.
async function destroyRestoresPresentation(): Promise<CaseResult> {
  const mounted = await mount();
  const internals = mounted.page as unknown as {
    keyBar: HTMLElement;
    keyRow: HTMLElement;
    keysPanel: { open: boolean; element: HTMLElement; showAll(group: string): void };
    composer: { typographyOpen: boolean; typographyPopover?: HTMLElement; setTypographyOpen(open: boolean): void };
    popoverOwner: string;
  };
  const originalNodes = Array.from(internals.keyRow.children);
  const originalAttributes = Array.from(internals.keyRow.attributes, (attribute) => [attribute.name, attribute.value]);
  internals.keyBar.hidden = false;
  internals.keysPanel.showAll("Function");
  internals.composer.setTypographyOpen(true);
  const entered = {
    bank: internals.keysPanel.element.dataset.group,
    typographyOpen: internals.composer.typographyOpen,
    owner: internals.popoverOwner,
  };
  const popover = internals.composer.typographyPopover;
  mounted.page.destroy();
  const restoredNodes = Array.from(internals.keyRow.children);
  const detail = {
    entered,
    labels: restoredNodes.map((node) => node.textContent),
    sameNodes: restoredNodes.every((node, index) => node === originalNodes[index]),
    attributes: Array.from(internals.keyRow.attributes, (attribute) => [attribute.name, attribute.value]),
    originalAttributes,
    keysOpen: internals.keysPanel.open,
    typographyOpen: internals.composer.typographyOpen,
    popoverConnected: popover?.isConnected ?? false,
    owner: internals.popoverOwner,
  };
  mounted.root.remove();
  return { name: "destroy_restores_terminal_controls_presentation", detail };
}

function judge(results: readonly CaseResult[]): string[] {
  const failures: string[] = [];
  const byName = new Map(results.map((result) => [result.name, result.detail]));
  const theme = byName.get("theme_rejection_settles_matching_intent")!;
  const font = byName.get("font_rejection_settles_matching_intent")!;
  const newer = byName.get("newer_theme_intent_survives_older_rejection")!;
  const lifecycle = byName.get("destroy_restores_terminal_controls_presentation")!;

  for (const [name, detail] of [["theme", theme], ["font", font], ["newer", newer]] as const) {
    if (detail.siblingThrew !== true) failures.push(`${name}: the sibling subscriber never threw; the case proves nothing`);
    if ((detail.unhandledRejections as string[]).length > 0) {
      failures.push(`${name}: the page leaked an unhandled rejection ${JSON.stringify(detail.unhandledRejections)}`);
    }
  }

  if (theme.committedStoredTheme !== "dracula") failures.push(`theme: the write did not commit ${JSON.stringify(theme)}`);
  if (theme.committedLiveTheme !== "dracula") failures.push(`theme: the committed record is not live ${JSON.stringify(theme)}`);
  if (theme.committedStatus !== "ready") failures.push(`theme: a subscriber fault was reported as a transport failure ${JSON.stringify(theme)}`);
  if (theme.committedStatusText !== "Saved — open terminals follow." || theme.committedSelectValue !== "dracula") {
    failures.push(`theme: the card did not settle its own save on the rejection path ${JSON.stringify(theme)}`);
  }
  if (theme.laterLiveTheme !== "gruvbox-dark" || theme.laterSelectValue !== "gruvbox-dark") {
    failures.push(`theme: a retained intent masked the later authoritative publication ${JSON.stringify(theme)}`);
  }

  if (font.committedStoredFont !== 15) failures.push(`font: the debounced write did not commit ${JSON.stringify(font)}`);
  if (font.committedLiveFont !== "15") failures.push(`font: the committed baseline is not live ${JSON.stringify(font)}`);
  if (font.committedStatus !== "ready") failures.push(`font: a subscriber fault was reported as a transport failure ${JSON.stringify(font)}`);
  if (font.laterLiveFont !== "20") failures.push(`font: a retained intent masked the later authoritative publication ${JSON.stringify(font)}`);
  if (font.toast !== "Zoom could not be saved") failures.push(`font: the rejected Zoom write was silent on the page ${JSON.stringify(font)}`);

  if (newer.liveTheme !== "one-dark") failures.push(`newer: the newest theme intent did not survive the older rejection ${JSON.stringify(newer)}`);
  if (newer.storedTheme !== "one-dark") failures.push(`newer: the newest theme intent was not the stored authority ${JSON.stringify(newer)}`);
  if (newer.selectValue !== "one-dark" || newer.statusText !== "Saved — open terminals follow.") {
    failures.push(`newer: the card's select or status line did not follow the newest pick ${JSON.stringify(newer)}`);
  }
  const entered = lifecycle.entered as Record<string, unknown>;
  if (entered.bank !== "Function" || entered.typographyOpen !== true || entered.owner !== "typography") {
    failures.push(`lifecycle: the setup never entered both transient states ${JSON.stringify(lifecycle)}`);
  }
  if (lifecycle.labels !== undefined && (lifecycle.labels as string[]).join(" ") !== "Esc ⇥ Ctrl ← ↓ ↑ →") {
    failures.push(`lifecycle: destroy did not restore the standard labels ${JSON.stringify(lifecycle)}`);
  }
  if (lifecycle.sameNodes !== true || lifecycle.keysOpen !== false
    || JSON.stringify(lifecycle.attributes) !== JSON.stringify(lifecycle.originalAttributes)) {
    failures.push(`lifecycle: destroy did not restore the original node identities/attributes ${JSON.stringify(lifecycle)}`);
  }
  if (lifecycle.typographyOpen !== false || lifecycle.popoverConnected !== false || lifecycle.owner !== "none") {
    failures.push(`lifecycle: destroy left a disclosure owner or portal alive ${JSON.stringify(lifecycle)}`);
  }
  return failures;
}

declare global {
  interface Window { __preferenceIntentHarness: { run(): Promise<{ cases: CaseResult[]; failures: string[] }> }; }
}

window.__preferenceIntentHarness = {
  async run(): Promise<{ cases: CaseResult[]; failures: string[] }> {
    const cases: CaseResult[] = [];
    cases.push(await themeRejection());
    cases.push(await fontRejection());
    cases.push(await newerIntentSurvives());
    cases.push(await destroyRestoresPresentation());
    return { cases, failures: judge(cases) };
  },
};
document.body.dataset.preferenceIntentHarnessReady = "true";
