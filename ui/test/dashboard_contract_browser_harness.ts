// Shared Dashboard coverage retained from the archived legacy surface suite.
import { Dashboard } from "../src/dashboard";
const wait = (ms = 0) => new Promise<void>((resolve) => window.setTimeout(resolve, ms));
async function until(predicate: () => boolean, timeout = 15_000): Promise<void> {
  const deadline = performance.now() + timeout;
  while (!predicate()) {
    if (performance.now() >= deadline) throw new Error("browser harness timeout");
    await wait(16);
  }
}

async function dashboardRefreshPersistence(): Promise<Record<string, unknown>> {
  const host = document.createElement("div");
  document.body.append(host);
  const inventory = {
    realms: [{ name: "local", servers: [{ label: "private", status: "ok", sessions: [{
      handles: { alias: "alias-handle", observe: "observe-handle", control: "control-handle" },
      realm: "local", server: "private", server_status: "ok", session_id: "$1", name: "session",
      width: 164, height: 38, attached: 1, activity: 1_700_000_000,
      unified: { state: "open", origin: "birth" },
      authority: { realm: "local", server: "private", uid: 1000, selector_kind: "socket_path", selector_value: "/tmp/private.sock", boot_id: "boot", server_pid: 42, server_start: 100, session_id: "$1", session_created: 200 },
    }] }] }],
    aliases: [],
  };
  let inventoryReads = 0;
  const dashboard = new Dashboard(host, async (input) => {
    const pathname = new URL(String(input), window.location.href).pathname;
    if (pathname === "/api/inventory") { inventoryReads += 1; return new Response(JSON.stringify(inventory)); }
    if (pathname === "/api/session-previews") return new Response(JSON.stringify({ rows: ["Fixture session preview."], width: 80, height: 24, captured_at: Date.now(), truncated: false }));
    if (pathname === "/api/dashboard-preferences") return new Response(JSON.stringify({ version: 1, favorites: [], revision: 1, available: true }), { headers: { ETag: '"1"' } });
    if (pathname === "/api/workspaces") return new Response(JSON.stringify({ version: 1, items: [] }));
    // This persistence case does not change the active terminal's appearance.
    return new Response(JSON.stringify({ error: "fixture_preferences_unavailable" }), { status: 503 });
  });
  try {
    dashboard.mount();
    await until(() => host.querySelector<HTMLAnchorElement>('.session-row a.session-open-action') !== null);
    host.querySelector<HTMLButtonElement>(".session-disclosure")?.click();
    host.querySelector<HTMLButtonElement>(".session-alias-edit")?.click();
    const initial = host.querySelector<HTMLInputElement>('.alias-editor input[name="display_alias"]');
    const initialLink = host.querySelector<HTMLAnchorElement>('.session-row a.session-open-action');
    if (!initial || !initialLink) throw new Error("dashboard alias editor or Open action absent");
    initial.value = "Unsaved dashboard alias";
    initial.dispatchEvent(new Event("input"));
    initial.focus(); initial.setSelectionRange(2, 8);
    const originalScope = new URLSearchParams(new URL(initialLink.href).hash.slice(1)).get("draft_scope");
    inventory.realms[0].servers[0].sessions[0].handles.control = "refreshed-control-handle";
    await dashboard.refresh("manual");
    const refreshed = host.querySelector<HTMLInputElement>('.alias-editor input[name="display_alias"]');
    if (!refreshed) throw new Error("refreshed dashboard alias editor absent");
    const links = Array.from(host.querySelectorAll<HTMLAnchorElement>('.session-row a.session-open-action'));
    const fragments = links.map(link => new URLSearchParams(new URL(link.href).hash.slice(1)));
    return {
      inventoryReads, aliasDraft: refreshed.value, sameEditor: refreshed === initial,
      focused: document.activeElement === refreshed, selection: [refreshed.selectionStart, refreshed.selectionEnd],
      sameOpenAction: links[0] === initialLink, originalScope,
      linkScopes: fragments.map(fragment => fragment.get("draft_scope")),
      linkHandles: fragments.map(fragment => fragment.get("handle")),
      linkHistories: fragments.map(fragment => fragment.get("history")),
      legacyControls: host.querySelectorAll('select[name="history"], .session-legacy, .session-detail-actions').length,
    };
  } finally {
    dashboard.destroy();
    host.remove();
  }
}
Object.assign(window, { dashboard_contract: { dashboardRefreshPersistence }, dashboard_contractReady: true });
