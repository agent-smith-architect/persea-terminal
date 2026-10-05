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
  // The order of the read that produced this list (inventory_read_order.ts).
  readOrder?: number;
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

export function switcherInventory(inventory: DashboardInventory, readOrder: number): SessionSwitcherInventory {
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
    readOrder,
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
      statusLabel: reason || activity,
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
  // The panel or sheet that holds root; revealing a row scrolls nothing
  // outside it.
  surface: HTMLElement;
  currentDraftScope(): string | null;
  blockedMessage(session: DashboardSession): string;
  select(session: DashboardSession): void;
  interactionGeneration(): number;
}>;

// One DOM renderer shared by single-terminal and workspace pane controllers.
// It owns presentation only: the pane-local controller owns inventory and the
// identity transaction, and trusted tap activation prevents a scrolling row
// from becoming a switch.
// Brings the row into view in each box that scrolls it, from the list out to
// the surface (the tag panel, or the quick-actions sheet in which the list
// grows), innermost first. Nothing outside the surface moves, not the page
// and not the terminal; the keyboard focus stays; a box that already shows
// the whole row is left where it is.
function revealRow(row: HTMLElement, list: HTMLElement, surface: HTMLElement): void {
  for (let box: HTMLElement | null = list; box; box = box === surface ? null : box.parentElement) {
    if (box.scrollHeight <= box.clientHeight || !/auto|scroll/.test(getComputedStyle(box).overflowY)) continue;
    const bounds = box.getBoundingClientRect();
    const target = row.getBoundingClientRect();
    if (target.top < bounds.top || target.bottom > bounds.bottom) box.scrollTop += target.top + target.height / 2 - (bounds.top + bounds.height / 2);
  }
}

export class SessionSwitcherView {
  readonly search: HTMLInputElement;
  readonly refresh: HTMLButtonElement;
  private readonly list: HTMLDivElement;
  // Set when the panel opens. The first render that shows the current session
  // in a visible list brings it into view, once, so the operator's own
  // scrolling is kept.
  private revealPending = false;
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

  // Call once the surface is shown. The rows already held are rendered at
  // once, so the reveal does not wait for the network.
  revealCurrent(): void {
    this.revealPending = true;
    if (this.inventory.sessions.length > 0) preserveFocus(() => this.render());
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
      // An alias is the label the operator chose, so it leads; the tmux name
      // follows it in a code face. Without an alias the tmux name leads.
      const name = document.createElement("span");
      name.className = "persea-session-switcher__name";
      const alias = document.createElement("span");
      alias.className = "persea-session-switcher__alias";
      const sessionName = document.createElement("span");
      sessionName.className = "persea-session-switcher__session-name";
      const currentChip = document.createElement("span");
      currentChip.className = "persea-session-switcher__current";
      currentChip.textContent = "Current";
      name.append(alias, sessionName, currentChip);
      const meta = document.createElement("span");
      meta.className = "persea-session-switcher__meta";
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
        button.dataset.alias = String(next.primaryAlias !== "");
        sessionName.textContent = next.session.name; sessionName.title = next.session.name;
        alias.textContent = next.primaryAlias; alias.title = next.primaryAlias; alias.hidden = !next.primaryAlias;
        currentChip.hidden = !next.current;
        meta.textContent = `${next.geometryLabel} · ${next.attachmentLabel} · ${next.statusLabel}`;
        meta.title = SESSION_METADATA_HELP;
      } };
      this.rowNodes.set(row.session.draftScope, entry);
      }
      entry.update(row); children.get(server)!.push(entry.button);
    }
    for (const [parent, nodes] of Array.from(children).reverse()) reconcileChildren(parent, nodes);
    reconcileChildren(this.list, Array.from(realms.values()));
    const current = rows.find(row => row.current);
    const currentButton = current && this.rowNodes.get(current.session.draftScope)?.button;
    if (this.revealPending && currentButton && this.list.clientHeight > 0) {
      this.revealPending = false;
      revealRow(currentButton, this.list, this.options.surface);
    }
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
