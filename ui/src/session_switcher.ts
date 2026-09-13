import type { DashboardInventory, DashboardSession } from "./dashboard";
import { bindTapActivation } from "./tap_activation";
import { compareSessionNames, outputActivityLabel, SESSION_METADATA_HELP } from "./session_metadata";
import { preserveFocus, reconcileChildren } from "./dashboard_dom";

export function normalizeSessionFilter(query: string): string { return query.trim().toLowerCase(); }

export function sessionMatchesFilter(session: Pick<DashboardSession, "name" | "aliases">, query: string): boolean {
  const normalized = normalizeSessionFilter(query);
  if (normalized === "") return true;
  return [session.name, ...session.aliases.map((alias) => alias.displayAlias)]
    .some((label) => label.toLowerCase().includes(normalized));
}

export function effectiveCollapse(collapsed: boolean, filterActive: boolean): boolean { return collapsed && !filterActive; }

export type SessionListServer = Readonly<{ key: string; hasError: boolean; sessions: readonly Pick<DashboardSession, "name" | "aliases">[] }>;
export type SessionListRealm = Readonly<{ hasError: boolean; servers: readonly SessionListServer[] }>;
export type SessionListPresentation = Readonly<{ filterActive: boolean; realms: readonly Readonly<{ visible: boolean; servers: readonly Readonly<{ visible: boolean; collapsed: boolean; sessions: readonly boolean[] }>[] }>[] }>;

export function sessionListPresentation(realms: readonly SessionListRealm[], query: string, collapsedGroups: ReadonlySet<string>): SessionListPresentation {
  const filterActive = normalizeSessionFilter(query) !== "";
  return {
    filterActive,
    realms: realms.map((realm) => {
      let visibleServers = 0;
      const servers = realm.servers.map((server) => {
        const sessions = server.sessions.map((session) => sessionMatchesFilter(session, query));
        const matches = sessions.filter((visible) => visible).length;
        const visible = !filterActive || matches > 0 || server.hasError;
        if (visible) visibleServers += 1;
        return { visible, collapsed: effectiveCollapse(collapsedGroups.has(server.key), filterActive), sessions };
      });
      return { visible: !(filterActive && visibleServers === 0 && !realm.hasError), servers };
    }),
  };
}

export function filterSessionList<S extends Pick<DashboardSession, "name" | "aliases">>(sessions: readonly S[], query: string): readonly S[] {
  return sessions.filter((session) => sessionMatchesFilter(session, query));
}

export type SessionSwitcherGroup = Readonly<{
  key: string;
  realm: string;
  realmLabel: string;
  server: string;
  sessions: readonly DashboardSession[];
}>;
export type SessionSwitcherInventory = Readonly<{
  sessions: readonly DashboardSession[];
  groups?: readonly SessionSwitcherGroup[];
}>;
export type SessionSwitcherRow = Readonly<{
  session: DashboardSession;
  selectable: boolean;
  current: boolean;
  reason: string;
  activityLabel: string;
  statusLabel: string;
  groupKey: string;
  realmLabel: string;
  serverLabel: string;
  primaryAlias: string;
  geometryLabel: string;
  attachmentLabel: string;
}>;

export function switcherInventory(inventory: DashboardInventory): SessionSwitcherInventory {
  const groups = inventory.realms.flatMap((realm) => realm.servers.map((server) => Object.freeze({
    key: `${realm.name}\u0000${server.label}`,
    realm: realm.name,
    realmLabel: realm.displayName,
    server: server.label,
    sessions: server.sessions,
  })));
  return Object.freeze({
    sessions: Object.freeze(groups.flatMap((group) => group.sessions)),
    groups: Object.freeze(groups),
  });
}

export function sessionSwitcherRows(
  inventory: SessionSwitcherInventory,
  currentDraftScope: string | null,
  query: string,
  blockedMessage: (session: DashboardSession) => string,
  nowMs = Date.now(),
): readonly SessionSwitcherRow[] {
  const groupByScope = new Map<string, SessionSwitcherGroup>();
  for (const group of inventory.groups ?? []) for (const session of group.sessions) groupByScope.set(session.draftScope, group);
  const groupOrder = new Map<string, number>();
  const groupKey = (session: DashboardSession): string => `${session.realm}\u0000${session.server}`;
  for (const session of inventory.sessions) if (!groupOrder.has(groupKey(session))) groupOrder.set(groupKey(session), groupOrder.size);
  return Object.freeze([...filterSessionList(inventory.sessions, query)].sort((a, b) => (groupOrder.get(groupKey(a))! - groupOrder.get(groupKey(b))!) || compareSessionNames(a, b)).map((session) => {
    const selectable = session.unified?.state === "open" || session.unified?.state === "adoptable";
    const reason = selectable ? "" : session.unified ? blockedMessage(session) : "Unified terminal unavailable";
    const group = groupByScope.get(session.draftScope);
    const current = session.draftScope === currentDraftScope;
    const activity = outputActivityLabel(session.outputActivity, nowMs);
    return Object.freeze({
      session,
      selectable,
      current,
      reason,
      activityLabel: activity,
      statusLabel: reason || (current ? `current · ${activity}` : activity),
      groupKey: group?.key ?? `${session.realm}\u0000${session.server}`,
      realmLabel: group?.realmLabel ?? session.realm,
      serverLabel: group?.server ?? session.server,
      primaryAlias: session.aliases[0]?.displayAlias ?? "",
      geometryLabel: `${session.width}\u00d7${session.height}`,
      attachmentLabel: `${session.attached} attached`,
    });
  }));
}

export type SessionSwitcherViewOptions = Readonly<{
  root: HTMLElement;
  currentDraftScope(): string | null;
  blockedMessage(session: DashboardSession): string;
  select(session: DashboardSession): void;
  interactionGeneration(): number;
}>;

// One DOM renderer shared by single-terminal and workspace pane controllers.
// It owns presentation only: the pane-local controller owns inventory and the
// identity transaction, and trusted tap activation prevents a scrolling row
// from becoming a switch.
export class SessionSwitcherView {
  readonly search: HTMLInputElement;
  readonly refresh: HTMLButtonElement;
  private readonly list: HTMLDivElement;
  private inventory: SessionSwitcherInventory = Object.freeze({ sessions: Object.freeze([]) });
  private readonly rowNodes = new Map<string, { button: HTMLButtonElement; update(row: SessionSwitcherRow): void }>();

  constructor(private readonly options: SessionSwitcherViewOptions) {
    this.search = document.createElement("input");
    this.search.type = "search";
    this.search.className = "persea-session-switcher__search";
    this.search.placeholder = "Search sessions";
    this.search.setAttribute("aria-label", "Search sessions");
    this.refresh = document.createElement("button");
    this.refresh.type = "button";
    this.refresh.className = "persea-session-switcher__refresh";
    this.refresh.textContent = "↻";
    this.refresh.title = "Refresh sessions";
    this.refresh.setAttribute("aria-label", "Refresh sessions");
    this.list = document.createElement("div");
    this.list.className = "persea-session-switcher__list";
    this.list.setAttribute("aria-label", "Available sessions");
    const controls = document.createElement("div");
    controls.className = "persea-session-switcher__controls";
    controls.append(this.search, this.refresh);
    options.root.classList.add("persea-session-switcher");
    options.root.append(controls, this.list);
    this.search.addEventListener("input", () => this.render());
  }

  setInventory(inventory: SessionSwitcherInventory): void {
    this.inventory = inventory;
    preserveFocus(() => this.render());
  }

  rows(): readonly SessionSwitcherRow[] {
    return sessionSwitcherRows(this.inventory, this.options.currentDraftScope(), this.search.value, this.options.blockedMessage);
  }

  render(): void {
    const rows = this.rows();
    const realmServers = new Map<string, Set<string>>();
    for (const row of rows) {
      const servers = realmServers.get(row.session.realm) ?? new Set<string>();
      servers.add(row.serverLabel);
      realmServers.set(row.session.realm, servers);
    }
    const realms = new Map<string, HTMLElement>();
    const servers = new Map<string, HTMLElement>();
    const children = new Map<HTMLElement, HTMLElement[]>();
    for (const row of rows) {
      let realm = realms.get(row.session.realm);
      if (!realm) {
        realm = Array.from(this.list.children).find(node => (node as HTMLElement).dataset.realm === row.session.realm) as HTMLElement | undefined ?? document.createElement("section");
        realm.className = "persea-session-switcher__group";
        realm.dataset.realm = row.session.realm;
        const heading = realm.querySelector("h3") ?? document.createElement("h3");
        heading.className = "persea-session-switcher__group-heading";
        heading.textContent = row.realmLabel;
        children.set(realm, [heading]);
        realms.set(row.session.realm, realm);
      }
      let server = servers.get(row.groupKey);
      if (!server) {
        server = Array.from(realm.children).find(node => (node as HTMLElement).dataset.server === row.serverLabel) as HTMLElement | undefined ?? document.createElement("div");
        server.className = "persea-session-switcher__server";
        server.dataset.server = row.serverLabel;
        children.set(server, []);
        if ((realmServers.get(row.session.realm)?.size ?? 0) > 1) {
          const heading = server.querySelector("h4") ?? document.createElement("h4");
          heading.className = "persea-session-switcher__server-heading";
          heading.textContent = row.serverLabel;
          children.get(server)!.push(heading);
        }
        servers.set(row.groupKey, server);
        children.get(realm)!.push(server);
      }
      let entry = this.rowNodes.get(row.session.draftScope);
      if (!entry) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "persea-session-switcher__row";
      button.disabled = !row.selectable || row.current;
      button.dataset.current = row.current ? "true" : "false";
      button.dataset.state = row.session.unified?.state ?? "unavailable";
      button.setAttribute("aria-label", `${row.current ? "Current session " : "Switch to "}${row.session.name}${row.primaryAlias ? ` · ${row.primaryAlias}` : ""}`);
      const name = document.createElement("span");
      name.className = "persea-session-switcher__name";
      const sessionName = document.createElement("span");
      sessionName.className = "persea-session-switcher__session-name";
      sessionName.textContent = row.session.name;
      name.append(sessionName);
        const alias = document.createElement("span");
        alias.className = "persea-session-switcher__alias";
        alias.textContent = row.primaryAlias;
        name.append(alias);
      const meta = document.createElement("span");
      meta.className = "persea-session-switcher__meta";
      meta.textContent = `${row.geometryLabel} · ${row.attachmentLabel} · ${row.statusLabel}`;
      button.append(name, meta);
      let current = row;
      bindTapActivation(button, () => {
        if (!button.disabled) this.options.select(current.session);
      }, () => undefined, () => !button.disabled && button.isConnected, this.options.interactionGeneration);
      entry = { button, update: next => {
        current = next;
        button.disabled = !next.selectable || next.current;
        button.dataset.current = String(next.current);
        button.setAttribute("aria-current", next.current ? "true" : "false");
        button.dataset.state = next.session.unified?.state ?? "unavailable";
        button.setAttribute("aria-label", `${next.current ? "Current session " : "Switch to "}${next.session.name}${next.primaryAlias ? ` · ${next.primaryAlias}` : ""}`);
        sessionName.textContent = next.session.name; sessionName.title = next.session.name;
        alias.textContent = next.primaryAlias; alias.hidden = !next.primaryAlias;
        meta.textContent = `${next.geometryLabel} · ${next.attachmentLabel} · ${next.statusLabel}`;
        meta.title = SESSION_METADATA_HELP;
      } };
      this.rowNodes.set(row.session.draftScope, entry);
      }
      entry.update(row); children.get(server)!.push(entry.button);
    }
    for (const [parent, nodes] of Array.from(children).reverse()) reconcileChildren(parent, nodes);
    reconcileChildren(this.list, Array.from(realms.values()));
    const live = new Set(this.inventory.sessions.map(session => session.draftScope));
    for (const [key, entry] of this.rowNodes) if (!live.has(key)) { entry.button.disabled = true; this.rowNodes.delete(key); }
    if (rows.length === 0) {
      const empty = document.createElement("p");
      empty.className = "persea-session-switcher__empty";
      empty.setAttribute("role", "status");
      empty.textContent = "No matching sessions";
      this.list.append(empty);
    }
  }
}
